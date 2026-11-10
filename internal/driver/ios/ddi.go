package ios

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/tetratorus/lightpinky/internal/driver"
	"github.com/tetratorus/lightpinky/internal/driver/ios/cryptex"
	"howett.net/plist"
)

// Developer disk images on this computer.
//
// iPhones on iOS 17 and later get the developer services Pinky uses (the
// screen stream, touch and keys, the accessibility services, instruments)
// from Apple's developer disk image: one image for every iOS from 17 on,
// which Xcode installs in /Library/Developer/DeveloperDiskImages/iOS_DDI.
// Pinky does not ship Apple's files. It uses an image already on the
// computer and mounts it on the phone itself (mount.go), so Xcode never has
// to be opened.

// diskImage is a developer disk image on this computer.
type diskImage struct {
	Dir     string // the Restore folder, which holds BuildManifest.plist
	Build   string // the Xcode build it came with, such as "27B52" (version.plist)
	Version string // the version the phone reports once it is mounted as a cryptex, such as "27.2.52.0"; "" for images older than Xcode 16
	Source  string // where it was found, one of the source constants
}

// Where a disk image was found.
const (
	sourceEnv    = driver.DiskImageFromEnv    // PINKY_DDI
	sourcePinky  = driver.DiskImageFromFolder // Pinky's own folder
	sourceXcode  = driver.DiskImageFromXcode  // installed by Xcode
	sourceMirror = driver.DiskImageFromMirror // downloaded from the third-party mirror (mirror.go)
)

// ddiEnv names a developer disk image folder to use instead of the others:
// an iOS_DDI folder (as Xcode installs it) or the Restore folder inside one.
const ddiEnv = "PINKY_DDI"

// xcodeDDI is where Xcode 16 and later install the iOS developer disk image,
// on first launch. Tests change it.
var xcodeDDI = "/Library/Developer/DeveloperDiskImages/iOS_DDI"

// errNoDiskImage means no developer disk image was found on this computer.
var errNoDiskImage = errors.New("no developer disk image on this computer")

// diskImageDirs are the folders findDiskImage looks in, in order. ownDir is
// Pinky's own folder for images ("" for none).
func diskImageDirs(ownDir string) []struct{ dir, source string } {
	var out []struct{ dir, source string }
	if v := strings.TrimSpace(os.Getenv(ddiEnv)); v != "" {
		out = append(out, struct{ dir, source string }{v, sourceEnv})
	}
	if ownDir != "" {
		out = append(out, struct{ dir, source string }{filepath.Join(ownDir, "iOS_DDI"), sourcePinky})
	}
	if runtime.GOOS == "darwin" {
		out = append(out, struct{ dir, source string }{xcodeDDI, sourceXcode})
	}
	return out
}

// findDiskImage returns the newest usable developer disk image in the
// folders diskImageDirs lists, the official copies. PINKY_DDI, when set, is
// the only folder looked in, so a wrong path is reported rather than passed
// over. Only when there is no official copy does it take the one the person
// downloaded from the third-party mirror.
func findDiskImage(ownDir string) (diskImage, error) {
	return findDiskImageAvoiding(ownDir, nil)
}

// findDiskImageAvoiding is findDiskImage passing over the images refused
// reports (nil: none), those Apple's signing server refused to sign: an
// official copy Apple refused gives way to another one, then to the
// mirror's download. When Apple refused every image here, it returns the
// mirror's download if there is one, else the newest official copy, for
// the mount to report; PINKY_DDI's image is returned either way.
func findDiskImageAvoiding(ownDir string, refused func(diskImage) bool) (diskImage, error) {
	if refused == nil {
		refused = func(diskImage) bool { return false }
	}
	var best, bestRefused diskImage
	var errs []string
	for _, c := range diskImageDirs(ownDir) {
		img, err := readDiskImage(c.dir)
		if err != nil {
			if c.source == sourceEnv {
				return diskImage{}, fmt.Errorf("%s=%s: %w", ddiEnv, c.dir, err)
			}
			if !errors.Is(err, os.ErrNotExist) {
				errs = append(errs, err.Error())
			}
			continue
		}
		img.Source = c.source
		if c.source == sourceEnv {
			return img, nil
		}
		if refused(img) {
			if bestRefused.Dir == "" || newerImage(img, bestRefused) {
				bestRefused = img
			}
			continue
		}
		if best.Dir == "" || newerImage(img, best) {
			best = img
		}
	}
	if best.Dir != "" {
		return best, nil
	}
	if ownDir != "" {
		if img, err := readDiskImage(filepath.Join(ownDir, mirrorFolder)); err == nil {
			img.Source = sourceMirror
			return img, nil
		}
	}
	if bestRefused.Dir != "" {
		return bestRefused, nil
	}
	if len(errs) > 0 {
		return diskImage{}, fmt.Errorf("%w (%s)", errNoDiskImage, strings.Join(errs, "; "))
	}
	return diskImage{}, errNoDiskImage
}

// readDiskImage reads the developer disk image in dir, an iOS_DDI folder
// or the Restore folder in one.
func readDiskImage(dir string) (diskImage, error) {
	restore := dir
	if _, err := os.Stat(filepath.Join(dir, "Restore", "BuildManifest.plist")); err == nil {
		restore = filepath.Join(dir, "Restore")
	}
	manifest := filepath.Join(restore, "BuildManifest.plist")
	if _, err := os.Stat(manifest); err != nil {
		return diskImage{}, err
	}
	img := diskImage{Dir: restore}
	var build struct {
		ProductBuildVersion string
	}
	// version.plist sits next to Restore in Xcode's layout; the manifest
	// itself names the build too.
	for _, p := range []string{filepath.Join(filepath.Dir(restore), "version.plist"), manifest} {
		if b, err := os.ReadFile(p); err == nil {
			if _, err := plist.Unmarshal(b, &build); err == nil && build.ProductBuildVersion != "" {
				img.Build = build.ProductBuildVersion
				break
			}
		}
	}
	if v, err := cryptex.CryptexImageVersion(restore); err == nil {
		img.Version = v.Version
	}
	if err := checkManifestFiles(restore); err != nil {
		return diskImage{}, err
	}
	return img, nil
}

// checkManifestFiles checks that the files the manifest's identities name
// for mounting are there, so a half-copied folder is not used.
func checkManifestFiles(restore string) error {
	b, err := os.ReadFile(filepath.Join(restore, "BuildManifest.plist"))
	if err != nil {
		return err
	}
	var m struct {
		BuildIdentities []struct {
			Manifest map[string]struct {
				Info struct {
					Path        string
					Personalize bool
				}
			}
		}
	}
	if _, err := plist.Unmarshal(b, &m); err != nil {
		return fmt.Errorf("%s: %w", filepath.Join(restore, "BuildManifest.plist"), err)
	}
	if len(m.BuildIdentities) == 0 {
		return fmt.Errorf("%s: no build identities", filepath.Join(restore, "BuildManifest.plist"))
	}
	seen := map[string]bool{}
	for _, id := range m.BuildIdentities {
		for _, e := range id.Manifest {
			p := e.Info.Path
			if p == "" || !e.Info.Personalize || seen[p] {
				continue
			}
			seen[p] = true
			if !filepath.IsLocal(p) {
				return fmt.Errorf("%s: the manifest names %q outside the image", restore, p)
			}
			if _, err := os.Stat(filepath.Join(restore, p)); err != nil {
				return fmt.Errorf("%s is incomplete: %w", restore, err)
			}
		}
	}
	return nil
}

// newerImage reports whether a is newer than b: by cryptex version, then by
// Xcode build.
func newerImage(a, b diskImage) bool {
	if c := compareVersions(a.Version, b.Version); c != 0 {
		return c > 0
	}
	return compareBuilds(a.Build, b.Build) > 0
}

// compareVersions compares dotted numbers such as "27.2.52.0"; "" is the
// oldest.
func compareVersions(a, b string) int {
	if a == b {
		return 0
	}
	if a == "" {
		return -1
	}
	if b == "" {
		return 1
	}
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) || i < len(bs); i++ {
		var x, y int
		if i < len(as) {
			x, _ = strconv.Atoi(as[i])
		}
		if i < len(bs) {
			y, _ = strconv.Atoi(bs[i])
		}
		if x != y {
			if x > y {
				return 1
			}
			return -1
		}
	}
	return 0
}

// compareBuilds compares Apple build numbers such as "27B52" and "17E5179g":
// the major number, the train letter, then the build number.
func compareBuilds(a, b string) int {
	pa, pb := splitBuild(a), splitBuild(b)
	for i := range pa {
		if pa[i] != pb[i] {
			if pa[i] > pb[i] {
				return 1
			}
			return -1
		}
	}
	return 0
}

func splitBuild(s string) [3]int {
	var out [3]int
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		out[0] = out[0]*10 + int(s[i]-'0')
		i++
	}
	if i < len(s) && s[i] >= 'A' && s[i] <= 'Z' {
		out[1] = int(s[i])
		i++
	}
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		out[2] = out[2]*10 + int(s[i]-'0')
		i++
	}
	return out
}
