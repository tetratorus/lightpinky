// Package apppkg reads app packages on the computer, for the approval to
// install them: an Android APK's package name, version, label and signer,
// and an iPhone app's bundle id, version, name and provisioning profile.
// It checks nothing the phone checks when it installs; it only says what
// the files hold, in words the person can judge.
package apppkg

import (
	"archive/zip"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// APK is an Android package file, as its manifest and signature say.
type APK struct {
	File        string // the path it was read from
	Package     string
	VersionCode int64
	VersionName string
	Label       string // "" when the manifest names none
	Split       string // the split's name; "" for a base APK
	Signer      Cert
}

// Version is the version as people see it with the version code:
// "1.3 (14)".
func (a APK) Version() string {
	return VersionText(a.VersionName, strconv.FormatInt(a.VersionCode, 10))
}

// Name is the app's label, else its package name.
func (a APK) Name() string {
	if a.Label != "" {
		return a.Label
	}
	return a.Package
}

// VersionText puts a version and a build number together, "1.3 (14)",
// or gives whichever there is.
func VersionText(version, build string) string {
	switch {
	case version == "":
		return build
	case build == "" || build == version:
		return version
	}
	return version + " (" + build + ")"
}

// maxManifest and maxTable bound what is read from an APK into memory.
const (
	maxManifest = 8 << 20
	maxTable    = 64 << 20
)

// ReadAPK reads one APK.
func ReadAPK(file string) (APK, error) {
	base := filepath.Base(file)
	f, err := os.Open(file)
	if err != nil {
		return APK{}, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return APK{}, err
	}
	z, err := zip.NewReader(f, fi.Size())
	if err != nil {
		return APK{}, fmt.Errorf("“%s” is not an APK: it is not a ZIP file", base)
	}
	var manifest, arsc *zip.File
	for _, e := range z.File {
		switch e.Name {
		case "AndroidManifest.xml":
			manifest = e
		case "resources.arsc":
			arsc = e
		}
	}
	if manifest == nil {
		return APK{}, fmt.Errorf("“%s” is not an APK: it has no AndroidManifest.xml", base)
	}
	b, err := readEntry(manifest, maxManifest)
	if err != nil {
		return APK{}, fmt.Errorf("“%s” cannot be read: %v", base, err)
	}
	elems, err := readXML(b)
	if err != nil || len(elems) == 0 || elems[0].name != "manifest" {
		return APK{}, fmt.Errorf("“%s” is not an APK: its manifest cannot be read", base)
	}
	m := elems[0]
	a := APK{File: file}
	if x, ok := m.attr("package", 0); ok {
		a.Package = x.text()
	}
	if x, ok := m.attr("split", 0); ok {
		a.Split = x.text()
	}
	if x, ok := m.attr("versionName", attrVersionName); ok {
		a.VersionName = x.text()
	}
	if x, ok := m.attr("versionCode", attrVersionCode); ok {
		a.VersionCode = int64(x.data)
		if x.raw != "" {
			a.VersionCode, _ = strconv.ParseInt(x.raw, 0, 64)
		}
	}
	if x, ok := m.attr("versionCodeMajor", attrVersionCodeMajor); ok && x.data != 0 {
		a.VersionCode = int64(x.data)<<32 | a.VersionCode&0xffffffff
	}
	if a.Package == "" {
		return APK{}, fmt.Errorf("“%s” is not an APK: its manifest names no package", base)
	}
	for _, e := range elems[1:] {
		if e.name != "application" || e.depth != 1 {
			continue
		}
		if x, ok := e.attr("label", attrLabel); ok {
			switch {
			case x.typ == typeReference && arsc != nil:
				if tb, err := readEntry(arsc, maxTable); err == nil {
					if t, err := readTable(tb); err == nil {
						a.Label = t.lookupString(x.data)
					}
				}
			default:
				a.Label = x.text()
			}
		}
		break
	}
	a.Label = strings.TrimSpace(a.Label)

	a.Signer, err = blockSigner(f, fi.Size())
	if errors.Is(err, errNoSigningBlock) {
		a.Signer, err = jarSigner(z)
	}
	if err != nil {
		return APK{}, fmt.Errorf("“%s” has no signature Pinky can read (%v); Android installs only signed APKs", base, err)
	}
	return a, nil
}

// ReadAPKs reads the APKs of one app, one or a base and its splits, and
// returns the base. They must hold one package, one version and one signer.
func ReadAPKs(files []string) (APK, error) {
	var base *APK
	var all []APK
	splits := map[string]string{}
	for _, file := range files {
		a, err := ReadAPK(file)
		if err != nil {
			return APK{}, err
		}
		if len(all) > 0 {
			first := all[0]
			switch {
			case a.Package != first.Package:
				return APK{}, fmt.Errorf("“%s” is %s and “%s” is %s: install the APKs of one app together", filepath.Base(first.File), first.Package, filepath.Base(file), a.Package)
			case a.VersionCode != first.VersionCode:
				return APK{}, fmt.Errorf("“%s” is version %d and “%s” version %d: install the APKs of one build together", filepath.Base(first.File), first.VersionCode, filepath.Base(file), a.VersionCode)
			case a.Signer.SHA256 != first.Signer.SHA256:
				return APK{}, fmt.Errorf("“%s” and “%s” are signed by different keys", filepath.Base(first.File), filepath.Base(file))
			}
		}
		if a.Split == "" {
			if base != nil {
				return APK{}, fmt.Errorf("“%s” and “%s” are both a base APK: pass one base and its splits", filepath.Base(base.File), filepath.Base(file))
			}
			base = &a
		} else if other, ok := splits[a.Split]; ok {
			return APK{}, fmt.Errorf("“%s” and “%s” are the same split (%s)", other, filepath.Base(file), a.Split)
		} else {
			splits[a.Split] = filepath.Base(file)
		}
		all = append(all, a)
	}
	switch {
	case len(all) == 0:
		return APK{}, errors.New("no APK given")
	case base == nil:
		return APK{}, errors.New("these are split APKs without their base APK: pass the base too")
	}
	return *base, nil
}
