package apppkg

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"howett.net/plist"
)

// IOSApp is an iPhone app bundle, a .app folder or an .ipa holding one, as
// its Info.plist says. Profile is its embedded.mobileprovision, which the
// iPhone driver reads (it lists the phones a development build installs
// on).
type IOSApp struct {
	File         string // the path it was read from
	BundleID     string
	Name         string // CFBundleDisplayName, else CFBundleName, else the executable
	Version      string // CFBundleShortVersionString
	Build        string // CFBundleVersion
	Platforms    []string
	MinimumOS    string
	Profile      []byte // nil when it has none
	BundleFolder string // "Dinely.app"
}

// VersionText is the version with the build number, "1.3 (14)".
func (a IOSApp) VersionText() string { return VersionText(a.Version, a.Build) }

// ForSimulator reports whether the app was built for the iOS Simulator,
// which a phone cannot run.
func (a IOSApp) ForSimulator() bool {
	for _, p := range a.Platforms {
		if strings.EqualFold(p, "iPhoneSimulator") {
			return true
		}
	}
	return false
}

// maxPlist bounds an Info.plist or profile read into memory.
const maxPlist = 4 << 20

// ReadIOSApp reads a .app folder or an .ipa.
func ReadIOSApp(file string) (IOSApp, error) {
	base := filepath.Base(file)
	fi, err := os.Stat(file)
	if err != nil {
		return IOSApp{}, err
	}
	if fi.IsDir() {
		if !strings.EqualFold(filepath.Ext(file), ".app") {
			return IOSApp{}, fmt.Errorf("“%s” is a folder but not an app: pass a .app folder or an .ipa", base)
		}
		info, err := readLimited(filepath.Join(file, "Info.plist"))
		if err != nil {
			return IOSApp{}, fmt.Errorf("“%s” has no Info.plist Pinky can read: %v", base, err)
		}
		a, err := parseInfo(info, file, base)
		if err != nil {
			return IOSApp{}, err
		}
		if p, err := readLimited(filepath.Join(file, "embedded.mobileprovision")); err == nil {
			a.Profile = p
		}
		return a, nil
	}
	z, err := zip.OpenReader(file)
	if err != nil {
		return IOSApp{}, fmt.Errorf("“%s” is not an .ipa: it is not a ZIP file", base)
	}
	defer z.Close()
	var info, profile *zip.File
	folder := ""
	for _, f := range z.File {
		parts := strings.Split(f.Name, "/")
		if len(parts) != 3 || parts[0] != "Payload" || !strings.HasSuffix(parts[1], ".app") {
			continue
		}
		if folder != "" && parts[1] != folder {
			return IOSApp{}, fmt.Errorf("“%s” holds more than one app", base)
		}
		folder = parts[1]
		switch parts[2] {
		case "Info.plist":
			info = f
		case "embedded.mobileprovision":
			profile = f
		}
	}
	if info == nil {
		return IOSApp{}, fmt.Errorf("“%s” is not an .ipa: it has no Payload/<name>.app/Info.plist", base)
	}
	b, err := readEntry(info, maxPlist)
	if err != nil {
		return IOSApp{}, fmt.Errorf("“%s” cannot be read: %v", base, err)
	}
	a, err := parseInfo(b, file, folder)
	if err != nil {
		return IOSApp{}, err
	}
	if profile != nil {
		if p, err := readEntry(profile, maxPlist); err == nil {
			a.Profile = p
		}
	}
	return a, nil
}

func readLimited(file string) ([]byte, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxPlist+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxPlist {
		return nil, errors.New("it is too large")
	}
	return b, nil
}

func parseInfo(b []byte, file, folder string) (IOSApp, error) {
	var raw struct {
		ID          string   `plist:"CFBundleIdentifier"`
		DisplayName string   `plist:"CFBundleDisplayName"`
		Name        string   `plist:"CFBundleName"`
		Executable  string   `plist:"CFBundleExecutable"`
		Version     string   `plist:"CFBundleShortVersionString"`
		Build       string   `plist:"CFBundleVersion"`
		Platforms   []string `plist:"CFBundleSupportedPlatforms"`
		MinimumOS   string   `plist:"MinimumOSVersion"`
	}
	if _, err := plist.Unmarshal(b, &raw); err != nil {
		return IOSApp{}, fmt.Errorf("the Info.plist of “%s” cannot be read: %v", filepath.Base(file), err)
	}
	if raw.ID == "" {
		return IOSApp{}, fmt.Errorf("the Info.plist of “%s” names no bundle id", filepath.Base(file))
	}
	name := raw.DisplayName
	if name == "" {
		name = raw.Name
	}
	if name == "" {
		name = raw.Executable
	}
	if name == "" {
		name = raw.ID
	}
	return IOSApp{File: file, BundleID: raw.ID, Name: strings.TrimSpace(name), Version: raw.Version, Build: raw.Build,
		Platforms: raw.Platforms, MinimumOS: raw.MinimumOS, BundleFolder: folder}, nil
}

// PackApp writes a .app folder as an .ipa to dst: Payload/<name>.app/…,
// stored without compression. Symbolic links are refused: an app bundle
// for a phone has none, and one could point anywhere on the computer.
func PackApp(app, dst string) error {
	base := filepath.Base(app)
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	w := zip.NewWriter(out)
	werr := filepath.WalkDir(app, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(app, p)
		if err != nil {
			return err
		}
		name := path.Join("Payload", base, filepath.ToSlash(rel))
		if d.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("“%s” holds a symbolic link, %s, which Pinky does not send to a phone", base, filepath.ToSlash(rel))
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		h := &zip.FileHeader{Name: name, Method: zip.Store, Modified: time.Unix(0, 0)}
		h.SetMode(fi.Mode())
		if d.IsDir() {
			h.Name += "/"
			_, err := w.CreateHeader(h)
			return err
		}
		if !fi.Mode().IsRegular() {
			return fmt.Errorf("“%s” holds %s, which is not a file", base, filepath.ToSlash(rel))
		}
		hw, err := w.CreateHeader(h)
		if err != nil {
			return err
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(hw, f)
		return err
	})
	cerr := w.Close()
	if ferr := out.Close(); cerr == nil {
		cerr = ferr
	}
	if werr != nil {
		os.Remove(dst)
		return werr
	}
	if cerr != nil {
		os.Remove(dst)
	}
	return cerr
}

// IsAPK and IsIOSApp tell the kinds of files apart by name.
func IsAPK(file string) bool { return strings.EqualFold(filepath.Ext(file), ".apk") }

// IsIOSApp reports whether file is named as an .ipa or a .app folder.
func IsIOSApp(file string) bool {
	ext := strings.ToLower(filepath.Ext(strings.TrimRight(file, `/\`)))
	return ext == ".ipa" || ext == ".app"
}
