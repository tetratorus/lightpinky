package ios

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/tetratorus/lightpinky/internal/driver"
	"howett.net/plist"
)

// Downloading Apple's developer disk image from a third-party mirror, for
// a computer without an official copy (no Xcode) or whose official copy
// (Xcode's, or one in Pinky's folder) Apple's signing server refused:
// offered only then, and done only when the person asks; a download is
// then used instead of a refused image. Pinky pins
// every file by size and SHA-256 at one commit of the mirror, so a changed
// file is refused, and the phone mounts only what Apple's signing server
// signs for it. Pinky itself hosts and ships none of Apple's files.

// The mirror is the one pymobiledevice3 downloads from, kept by its author.
// 27A5228h (Xcode 27.0 beta 4) is the newest iOS image it has. It stores
// the image's two halves apart under new names: the cryptex, which iOS 27
// installs, and the personalized image older iOS mounts. Pinky puts them
// back together under the names the cryptex half's manifest gives them.
const (
	mirrorName   = "github.com/doronz88/DeveloperDiskImage"
	mirrorCommit = "e2aaeac89ca8fa56c2fa1f144d182994e07a525b"
	mirrorBuild  = "27A5228h"
	mirrorXcode  = "Xcode 27.0 beta 4"
	mirrorFolder = "iOS_DDI-mirror" // in Pinky's folder
)

// mirrorBase is where the files are fetched from: the pinned commit.
var mirrorBase = "https://raw.githubusercontent.com/doronz88/DeveloperDiskImage/" + mirrorCommit + "/PersonalizedImages/"

// mirrorFile is one file of the mirror's image.
type mirrorFile struct {
	from   string // under mirrorBase
	to     string // in the Restore folder Pinky assembles
	size   int64
	sha256 string
}

var mirrorFiles = []mirrorFile{
	{"Xcode_iOS_DDI_Cryptex/BuildManifest.plist", "BuildManifest.plist", 804946, "27385d7582b03b36bb3104e22b520aee0c47d72fecb4e8ecfe12ef5d966c7012"},
	{"Xcode_iOS_DDI_Cryptex/Image.dmg", "Image.dmg", 15895040, "873097f695a8b9734e2abc54f795a8874d40ff6fd11208ecb01ef29534c7c176"},
	{"Xcode_iOS_DDI_Cryptex/Image.dmg.cryptex_info", "Image.dmg.cryptex_info", 430, "edf49aef55aacc063d4d7be05b713bb545ce2993b3f62bcc15eccd75e610ee6c"},
	{"Xcode_iOS_DDI_Cryptex/Image.dmg.root_hash", "Image.dmg.root_hash", 229, "3543fad2805b88119695c417e12679380b3b5a2742994bbcc839c8e2de5d7302"},
	{"Xcode_iOS_DDI_Cryptex/Image.dmg.trustcache", "Image.dmg.trustcache", 1895, "f7f21986074eee03a215aca16ecfc78d6bf183600d8a0d2fb691f9896782e6f0"},
	// The personalized half, under the names the cryptex half's manifest
	// gives it (Apple's own, 022-22070-072).
	{"Xcode_iOS_DDI_Personalized/Image.dmg", "022-22070-072.dmg", 15733248, "05fd807da5e19f030fa4941f24800c965c6c77982ab572dd5d1ef778fb69f9ca"},
	{"Xcode_iOS_DDI_Personalized/Image.dmg.trustcache", "Firmware/022-22070-072.dmg.trustcache", 1895, "36af60889ff5a737874a26daeb8e1a0139ebfebec6ec2e4d8f6a3c1bf1dce35c"},
}

func mirrorSize(files []mirrorFile) int64 {
	var n int64
	for _, f := range files {
		n += f.size
	}
	return n
}

// DiskImageMirror implements driver.DiskImageDownloader.
func (p *Provider) DiskImageMirror() driver.DiskImageMirror {
	return driver.DiskImageMirror{Name: mirrorName, Image: mirrorXcode + " (" + mirrorBuild + ")", Size: mirrorSize(mirrorFiles)}
}

// DownloadDiskImage implements driver.DiskImageDownloader: the mirror's
// image into Pinky's folder, each file checked. Phones that wait for an
// image are looked at again at once.
func (p *Provider) DownloadDiskImage(ctx context.Context, progress func(done, total int64)) error {
	if p.DiskImageDir == "" {
		return errors.New("Pinky has no folder to keep the developer disk image in")
	}
	client := &http.Client{Timeout: 10 * time.Minute}
	log, start := p.logger(), time.Now()
	log.Info("iphone: downloading the developer disk image", "from", mirrorName, "commit", mirrorCommit, "build", mirrorBuild)
	if err := downloadMirror(ctx, client, mirrorBase, mirrorFiles, p.DiskImageDir, progress); err != nil {
		log.Warn("iphone: could not download the developer disk image", "from", mirrorName, "err", err)
		return err
	}
	log.Info("iphone: downloaded the developer disk image", "to", filepath.Join(p.DiskImageDir, mirrorFolder), "took", time.Since(start).Round(time.Millisecond))
	p.retryNow()
	return nil
}

// retryNow forgets the failed mounts and the phones that are not online,
// so the next List mounts the image a new download brought at once.
func (p *Provider) retryNow() {
	p.prep.mu.Lock()
	for id, j := range p.prep.jobs {
		if !j.running && j.err != nil {
			delete(p.prep.jobs, id)
		}
	}
	p.prep.mu.Unlock()
	p.mu.Lock()
	defer p.mu.Unlock()
	for id, k := range p.known {
		if k.info.State != driver.StateOnline {
			delete(p.known, id)
		}
	}
	p.ddi = map[string]ddiCheck{}
}

// downloadMirror fetches files from base into dir/iOS_DDI-mirror, checking
// each one's size and SHA-256, and replaces an earlier download only once
// the new one is complete.
func downloadMirror(ctx context.Context, client *http.Client, base string, files []mirrorFile, dir string, progress func(done, total int64)) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(dir, ".download-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp) // gone after the rename
	total, done := mirrorSize(files), int64(0)
	for _, f := range files {
		err := fetchMirrorFile(ctx, client, base+f.from, f, filepath.Join(tmp, "Restore", filepath.FromSlash(f.to)), func(n int64) {
			done += n
			if progress != nil {
				progress(done, total)
			}
		})
		if err != nil {
			return err
		}
	}
	// Next to Restore, as Xcode's version.plist, with where it came from.
	b, err := plist.MarshalIndent(map[string]any{
		"ProductBuildVersion": mirrorBuild,
		"Source":              "https://" + mirrorName,
		"SourceCommit":        mirrorCommit,
	}, plist.XMLFormat, "\t")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(tmp, "version.plist"), b, 0o644); err != nil {
		return err
	}
	if _, err := readDiskImage(tmp); err != nil {
		return fmt.Errorf("the downloaded developer disk image is incomplete: %w", err)
	}
	final := filepath.Join(dir, mirrorFolder)
	if err := os.RemoveAll(final); err != nil {
		return err
	}
	return os.Rename(tmp, final)
}

// fetchMirrorFile downloads url to path, checking its size and SHA-256
// against f; added gets the bytes as they come.
func fetchMirrorFile(ctx context.Context, client *http.Client, url string, f mirrorFile, path string, added func(int64)) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "Pinky")
	res, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("downloading %s: %w", f.from, err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("downloading %s: the mirror answered %s", f.from, res.Status)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	out, err := os.Create(path)
	if err != nil {
		return err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h, countWriter(added)), io.LimitReader(res.Body, f.size+1))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("downloading %s: %w", f.from, err)
	}
	if n != f.size || hex.EncodeToString(h.Sum(nil)) != f.sha256 {
		return fmt.Errorf("%s: %w", f.from, driver.ErrDiskImageMismatch)
	}
	return nil
}

// countWriter reports how many bytes pass through it.
type countWriter func(int64)

func (c countWriter) Write(b []byte) (int, error) {
	c(int64(len(b)))
	return len(b), nil
}

var _ driver.DiskImageDownloader = (*Provider)(nil)
