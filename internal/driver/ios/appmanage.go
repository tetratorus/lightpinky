package ios

// driver.AppManager on iPhones: installing development builds from the
// computer through the streaming zip conduit, uninstalling through the
// installation proxy, stopping through CoreDevice or WebDriverAgent. See
// docs/notes/apps.md.

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/tetratorus/lightpinky/internal/apppkg"
	"github.com/tetratorus/lightpinky/internal/driver"
	goios "github.com/danielpaulus/go-ios/ios"
	"github.com/danielpaulus/go-ios/ios/installationproxy"
	"github.com/danielpaulus/go-ios/ios/zipconduit"
)

var (
	_ driver.AppManager = (*Device)(nil)
	_ driver.AppManager = (*WDADevice)(nil)
)

// readIOSPackage reads the app in paths and checks that the phone with
// udid can take it: one .ipa or .app folder, built for a phone, with a
// provisioning profile that lists the phone and has not run out. Each
// refusal says in plain words what to do.
func readIOSPackage(paths []string, udid string, now time.Time) (apppkg.IOSApp, driver.Package, error) {
	if len(paths) != 1 {
		return apppkg.IOSApp{}, driver.Package{}, driver.PackageError("An iPhone app is one .ipa file or one .app folder; %d files were given.", len(paths))
	}
	file := paths[0]
	base := filepath.Base(file)
	if !apppkg.IsIOSApp(file) {
		if apppkg.IsAPK(file) {
			return apppkg.IOSApp{}, driver.Package{}, driver.PackageError("“%s” is an Android app; an iPhone installs an .ipa file or a .app folder.", base)
		}
		return apppkg.IOSApp{}, driver.Package{}, driver.PackageError("“%s” is not an iPhone app: pass an .ipa file or a .app folder.", base)
	}
	a, err := apppkg.ReadIOSApp(file)
	if err != nil {
		return apppkg.IOSApp{}, driver.Package{}, driver.PackageError("%s", strings.TrimSuffix(err.Error(), "."))
	}
	name := a.Name
	if a.ForSimulator() {
		return a, driver.Package{}, driver.PackageError("“%s” is built for the iOS Simulator, which an iPhone cannot run. Build it for a device: in Xcode, choose the phone or Any iOS Device as the destination.", name)
	}
	if a.Profile == nil {
		return a, driver.Package{}, driver.PackageError("“%s” has no provisioning profile, so the iPhone would refuse it. Sign it with a development profile that lists this phone.", name)
	}
	pr, err := parseProfile(a.Profile)
	if err != nil {
		return a, driver.Package{}, driver.PackageError("The provisioning profile of “%s” cannot be read (%v). Sign it again with a development profile that lists this phone.", name, err)
	}
	if now.After(pr.Expires) {
		return a, driver.Package{}, driver.PackageError("The provisioning profile of “%s” ran out on %s. Sign it again with a current profile.", name, pr.Expires.Local().Format("January 2, 2006"))
	}
	if !pr.runsOn(udid) {
		if len(pr.Devices) == 0 {
			return a, driver.Package{}, driver.PackageError("“%s” is signed for the App Store, which installs it only from the App Store or TestFlight. Sign it with a development profile that lists this phone.", name)
		}
		return a, driver.Package{}, driver.PackageError("The provisioning profile of “%s” does not list this iPhone (UDID %s). Add the phone to the profile in your Apple Developer account, or let Xcode register it, then build again.", name, udid)
	}
	p := driver.Package{ID: a.BundleID, Name: name, Version: a.VersionText(), SignerKey: pr.TeamID}
	switch {
	case pr.TeamName != "" && pr.TeamID != "":
		p.Signer = fmt.Sprintf("%s (%s)", pr.TeamName, pr.TeamID)
	case pr.TeamName != "":
		p.Signer = pr.TeamName
	default:
		p.Signer = pr.TeamID
	}
	if p.SignerKey == "" {
		// A profile without a team id: the application identifier's prefix
		// names the team.
		p.SignerKey, _, _ = strings.Cut(pr.AppID, ".")
	}
	return a, p, nil
}

// untilDone runs fn and returns what it returns, or ctx's error when ctx
// ends first; stop then ends what fn waits on (it closes the connection).
func untilDone(ctx context.Context, fn func() error, stop func()) error {
	ch := make(chan error, 1)
	go func() { ch <- fn() }()
	select {
	case err := <-ch:
		return err
	case <-ctx.Done():
		stop()
		return ctx.Err()
	}
}

// installedVersion is the version of the app with bundle id id on the
// phone, "" when it is not installed. Only apps people installed are
// looked at: the gateway refuses installs over the phone's own apps.
func installedVersion(ctx context.Context, e goios.DeviceEntry, id string) (string, error) {
	conn, err := installationproxy.New(e)
	if err != nil {
		return "", fmt.Errorf("installation proxy: %w", err)
	}
	defer conn.Close()
	var apps []installationproxy.AppInfo
	err = untilDone(ctx, func() error {
		var err error
		apps, err = conn.BrowseUserApps()
		return err
	}, conn.Close)
	if err != nil {
		return "", fmt.Errorf("list apps: %w", err)
	}
	for _, a := range apps {
		if a.CFBundleIdentifier() == id {
			build, _ := a["CFBundleVersion"].(string)
			return apppkg.VersionText(a.CFBundleShortVersionString(), build), nil
		}
	}
	return "", nil
}

// installIPA installs the app at file, an .ipa or a .app folder, through
// the streaming zip conduit over usbmuxd (the cable or Wi-Fi).
func installIPA(ctx context.Context, e goios.DeviceEntry, file, name string) error {
	if fi, err := os.Stat(file); err == nil && fi.IsDir() {
		// go-ios sends folders with the computer's paths in them; an .ipa
		// goes as it is.
		tmp, err := os.MkdirTemp("", "pinky-ipa-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(tmp)
		ipa := filepath.Join(tmp, strings.TrimSuffix(filepath.Base(file), filepath.Ext(file))+".ipa")
		if err := apppkg.PackApp(file, ipa); err != nil {
			return driver.PackageError("%s", strings.TrimSuffix(err.Error(), "."))
		}
		file = ipa
	}
	conn, err := zipconduit.New(e)
	if err != nil {
		return fmt.Errorf("connect to the iPhone's installer: %w", err)
	}
	defer conn.Close()
	err = untilDone(ctx, func() error { return conn.SendFile(file) }, func() { conn.Close() })
	if err != nil {
		return installErrorIOS(name, err)
	}
	return nil
}

// goiosInstallErr matches go-ios's words for an install the phone
// refused: the phone's error code and its description.
var goiosInstallErr = regexp.MustCompile(`(?s)(?:original error|failed installing): '([A-Za-z]+)' errorDescription:'(.*)'`)

// installErrorsIOS put the phone's install errors in plain words.
var installErrorsIOS = map[string]string{
	"ApplicationVerificationFailed":              "The iPhone refused “%s”: its signature or provisioning profile does not let it run on this phone. Sign it with a development profile that lists the phone, then build again",
	"DeviceOSVersionTooLow":                      "“%s” needs a newer iOS than this iPhone has",
	"MismatchedApplicationIdentifierEntitlement": "“%s” is on the iPhone already, signed by another team. Uninstall it first, or sign this build with the same team",
	"IncorrectArchitecture":                      "“%s” is not built for this iPhone's processor",
	"PackageInspectionFailed":                    "The iPhone could not read “%s”. Build the .ipa again",
	"PackageExtractionFailed":                    "The iPhone could not unpack “%s”. Build the .ipa again",
	"DeviceFamilyNotSupported":                   "“%s” is not made for this kind of device (an iPad-only app on an iPhone, say)",
	"InstallProhibited":                          "This iPhone does not allow installing apps (Screen Time or a device management profile)",
}

// installErrorIOS turns the phone's refusal into a PackageError; other
// errors (a dropped connection) stay as they are.
func installErrorIOS(name string, err error) error {
	m := goiosInstallErr.FindStringSubmatch(err.Error())
	if m == nil {
		return fmt.Errorf("install “%s”: %w", name, err)
	}
	code, desc := m[1], strings.TrimSpace(m[2])
	if msg, ok := installErrorsIOS[code]; ok {
		if strings.Contains(msg, "%s") {
			msg = fmt.Sprintf(msg, name)
		}
		if desc != "" {
			return driver.PackageError("%s (%s: %s).", msg, code, strings.TrimSuffix(desc, "."))
		}
		return driver.PackageError("%s (%s).", msg, code)
	}
	if desc != "" {
		return driver.PackageError("The iPhone did not install “%s”: %s (%s).", name, strings.TrimSuffix(desc, "."), code)
	}
	return driver.PackageError("The iPhone did not install “%s” (%s).", name, code)
}

// uninstallBundle removes the app with bundle id id through the
// installation proxy.
func uninstallBundle(ctx context.Context, e goios.DeviceEntry, id string) error {
	conn, err := installationproxy.New(e)
	if err != nil {
		return fmt.Errorf("installation proxy: %w", err)
	}
	defer conn.Close()
	err = untilDone(ctx, func() error { return conn.Uninstall(id) }, conn.Close)
	if err != nil {
		if s, ok := strings.CutPrefix(err.Error(), "received uninstall error: "); ok {
			return driver.PackageError("The iPhone did not uninstall %s: %s.", id, strings.TrimSuffix(s, "."))
		}
		return fmt.Errorf("uninstall %s: %w", id, err)
	}
	return nil
}

// errNoInstaller is an iPhone Pinky reaches without usbmuxd: a Simulator.
var errNoInstaller = fmt.Errorf("installing apps on a Simulator: %w", driver.ErrUnsupported)

// InspectPackage implements driver.AppManager.
func (d *Device) InspectPackage(ctx context.Context, paths []string) (driver.Package, error) {
	_, p, err := readIOSPackage(paths, d.info.ID, time.Now())
	if err != nil {
		return p, err
	}
	p.Installed, err = installedVersion(ctx, d.l.base, p.ID)
	return p, d.gone(err)
}

// InstallPackage implements driver.AppManager.
func (d *Device) InstallPackage(ctx context.Context, paths []string) error {
	a, p, err := readIOSPackage(paths, d.info.ID, time.Now())
	if err != nil {
		return err
	}
	start := time.Now()
	err = installIPA(ctx, d.l.base, a.File, p.Name)
	d.appsChanged()
	if err != nil {
		return d.gone(err)
	}
	slog.Info("installed an app", "device", d.info.ID, "app", p.ID, "version", p.Version, "took", time.Since(start).Round(time.Millisecond))
	return nil
}

// UninstallApp implements driver.AppManager.
func (d *Device) UninstallApp(ctx context.Context, id string) error {
	err := uninstallBundle(ctx, d.l.base, id)
	d.appsChanged()
	return d.gone(err)
}

// StopApp implements driver.AppManager: SIGKILL to the app's own process,
// found by its executable in the app's bundle, as swiping it away ends it.
// Its extensions (widgets, keyboards) run on.
func (d *Device) StopApp(ctx context.Context, id string) error {
	return d.gone(d.stopApp(ctx, id))
}

func (d *Device) stopApp(ctx context.Context, id string) error {
	c, err := d.catalogFresh(ctx)
	if err != nil {
		return err
	}
	var bundles []string
	c.mu.Lock()
	for path, app := range c.byPath {
		if app.ID == id {
			bundles = append(bundles, path)
		}
	}
	c.mu.Unlock()
	if len(bundles) == 0 {
		return fmt.Errorf("%s is not installed: %w", id, driver.ErrNotFound)
	}
	procs, err := d.processPaths(ctx)
	if err != nil {
		return err
	}
	pids := appProcesses(bundles, procs)
	if len(pids) == 0 {
		return nil // not running
	}
	svc, err := dialCoreDevice(d.l.entry, appService)
	if err != nil {
		return err
	}
	defer svc.Close()
	for _, pid := range pids {
		sc, cancel := context.WithTimeout(ctx, 5*time.Second)
		_, err := svc.invoke(sc, "com.apple.coredevice.feature.sendsignaltoprocess", "", map[string]any{
			"process": map[string]any{"processIdentifier": int64(pid)},
			"signal":  int64(9),
		})
		cancel()
		if err != nil {
			return fmt.Errorf("stop %s: %w", id, err)
		}
	}
	d.svcMu.Lock()
	for _, pid := range pids {
		delete(d.pidApps, pid)
	}
	d.svcMu.Unlock()
	return nil
}

// appProcesses picks the processes whose executable sits directly in one
// of the bundles: the app itself, not its extensions in PlugIns. The paths
// are the iPhone's, with forward slashes on every computer.
func appProcesses(bundles []string, procs map[uint64]string) []uint64 {
	var pids []uint64
	for pid, exe := range procs {
		dir := path.Dir(exe)
		for _, b := range bundles {
			b = strings.TrimSuffix(b, "/")
			if dir == b || dir == "/private"+b || "/private"+dir == b {
				pids = append(pids, pid)
				break
			}
		}
	}
	return pids
}

// appsChanged has the app list read again after an install or uninstall.
func (d *Device) appsChanged() {
	d.svcMu.Lock()
	c := d.apps
	d.svcMu.Unlock()
	if c != nil {
		c.mu.Lock()
		c.at = time.Time{}
		c.mu.Unlock()
	}
}

// InspectPackage implements driver.AppManager on the WebDriverAgent path.
func (d *WDADevice) InspectPackage(ctx context.Context, paths []string) (driver.Package, error) {
	if d.entry.DeviceID == 0 {
		return driver.Package{}, errNoInstaller
	}
	_, p, err := readIOSPackage(paths, d.info.ID, time.Now())
	if err != nil {
		return p, err
	}
	p.Installed, err = installedVersion(ctx, d.entry, p.ID)
	return p, err
}

// InstallPackage implements driver.AppManager on the WebDriverAgent path.
func (d *WDADevice) InstallPackage(ctx context.Context, paths []string) error {
	if d.entry.DeviceID == 0 {
		return errNoInstaller
	}
	a, p, err := readIOSPackage(paths, d.info.ID, time.Now())
	if err != nil {
		return err
	}
	err = installIPA(ctx, d.entry, a.File, p.Name)
	d.appsChanged()
	if err == nil {
		slog.Info("installed an app", "device", d.info.ID, "app", p.ID, "version", p.Version)
	}
	return err
}

// UninstallApp implements driver.AppManager on the WebDriverAgent path.
func (d *WDADevice) UninstallApp(ctx context.Context, id string) error {
	if d.entry.DeviceID == 0 {
		return errNoInstaller
	}
	err := uninstallBundle(ctx, d.entry, id)
	d.appsChanged()
	return err
}

// StopApp implements driver.AppManager through WebDriverAgent, which
// terminates the app as XCTest does.
func (d *WDADevice) StopApp(ctx context.Context, id string) error {
	if err := d.c.terminate(ctx, id); err != nil {
		return fmt.Errorf("stop %s: %w", id, err)
	}
	return nil
}

func (d *WDADevice) appsChanged() {
	d.appMu.Lock()
	c := d.apps
	d.appMu.Unlock()
	if c != nil {
		c.mu.Lock()
		c.at = time.Time{}
		c.mu.Unlock()
	}
}
