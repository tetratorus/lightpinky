package ios

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	goios "github.com/danielpaulus/go-ios/ios"
	"github.com/tetratorus/lightpinky/internal/driver"
	"howett.net/plist"
)

// iOS Simulators as phones, on macOS.
//
// SimProvider lists every iOS Simulator booted on the Mac as an iPhone or
// iPad (Info.Virtual) and drives it through WebDriverAgent, the way iPhones
// below iOS 27 are driven: WDA speaks the same HTTP on a Simulator. It
// builds WDA itself with the Mac's Xcode (simwda.go); until that build is
// there, a booted Simulator is listed as needing setup. iOS 27 Simulators
// offer nothing better: simctl has no input or accessibility commands, and
// the services Pinky uses on iOS 27 phones are not there (docs/notes/ios.md).
//
// It finds Xcode the way xcrun does, without running xcrun, which on a Mac
// without the command line tools asks to install them: DEVELOPER_DIR, then
// the folder xcode-select chose, then the Xcode apps in /Applications
// (when xcode-select chose the command line tools, which have no
// Simulator). It runs that Xcode's simctl and xcodebuild with DEVELOPER_DIR
// set. Without Xcode it lists nothing, and VirtualPhones says so.
//
// It also starts and stops Simulators for the person (driver.VirtualLauncher):
// simctl boot, with Simulator.app opened on it so it shows, and simctl
// shutdown. It does not implement driver.HostChecker: the iOS provider's
// check is the platform's, and a Mac without Xcode can still use iPhones.
type SimProvider struct {
	// Dir is Pinky's folder for WebDriverAgent: its source, its build, and
	// the test runs (simwda.go).
	Dir string

	// Log gets a line when Pinky builds WebDriverAgent, starts it on a
	// Simulator, or fails to. nil for none.
	Log *slog.Logger

	mu       sync.Mutex
	x        xcode
	xErr     error
	xAt      time.Time
	types    map[string]string // device type identifier → name ("iPhone 17")
	typesAt  time.Time
	devs     []simEntry // the last simctl list
	devsAt   time.Time
	devStamp string               // the device set's stamp when devs was read
	build    simBuild             // the WebDriverAgent build for the current Xcode
	starting map[string]time.Time // by UDID: booted by StartVirtual, until listed as booted

	// For tests.
	findXcodeFn func() (xcode, error)
	simctlFn    func(ctx context.Context, args ...string) ([]byte, error)
	buildFn     func(ctx context.Context, x xcode) (string, error)
	setDir      string // the Simulators' device set; "" for the default
	now         func() time.Time
}

// NewSimProvider returns an iOS Simulator provider keeping WebDriverAgent
// in dir.
func NewSimProvider(dir string) *SimProvider {
	return &SimProvider{Dir: dir}
}

// Platform implements driver.Provider.
func (p *SimProvider) Platform() driver.Platform { return driver.IOS }

func (p *SimProvider) clock() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

func (p *SimProvider) logger() *slog.Logger { return logTo(p.Log) }

// xcode is an Xcode Pinky can run Simulators with.
type xcode struct {
	Developer string // its Contents/Developer folder
	App       string // the Xcode app
	Version   string // "27.2"
	Build     string // "27B52"
}

func (x xcode) tool(name string) string { return filepath.Join(x.Developer, "usr", "bin", name) }

// env is the environment for Xcode's tools: Pinky's, with DEVELOPER_DIR.
func (x xcode) env() []string { return append(os.Environ(), "DEVELOPER_DIR="+x.Developer) }

// errNoXcode is a Mac without Xcode, or another system.
var errNoXcode = errors.New("iOS Simulators need Xcode, which this Mac does not have")

// xcodeSelectLink is where xcode-select keeps the folder it chose.
const xcodeSelectLink = "/var/db/xcode_select_link"

// findXcode finds the Xcode to use, as described on SimProvider.
func findXcode() (xcode, error) {
	if runtime.GOOS != "darwin" {
		return xcode{}, errNoXcode
	}
	var dirs []string
	if d := os.Getenv("DEVELOPER_DIR"); d != "" {
		dirs = append(dirs, d)
	}
	if d, err := os.Readlink(xcodeSelectLink); err == nil {
		dirs = append(dirs, d)
	}
	apps, _ := filepath.Glob("/Applications/Xcode*.app")
	sort.Slice(apps, func(i, j int) bool {
		// Xcode.app first, then the others (Xcode-beta.app) by name.
		if a, b := filepath.Base(apps[i]) == "Xcode.app", filepath.Base(apps[j]) == "Xcode.app"; a != b {
			return a
		}
		return apps[i] < apps[j]
	})
	dirs = append(dirs, apps...)
	for _, d := range dirs {
		if x, ok := xcodeAt(d); ok {
			return x, nil
		}
	}
	return xcode{}, errNoXcode
}

// xcodeAt reads the Xcode at a developer folder or an Xcode app, as
// DEVELOPER_DIR may name either.
func xcodeAt(dir string) (xcode, bool) {
	if strings.HasSuffix(dir, ".app") || strings.HasSuffix(dir, ".app/") {
		dir = filepath.Join(dir, "Contents", "Developer")
	}
	x := xcode{Developer: filepath.Clean(dir)}
	for _, t := range []string{"simctl", "xcodebuild"} {
		if st, err := os.Stat(x.tool(t)); err != nil || st.IsDir() {
			return xcode{}, false
		}
	}
	contents := filepath.Dir(x.Developer)
	x.App = filepath.Dir(contents)
	b, err := os.ReadFile(filepath.Join(contents, "version.plist"))
	if err != nil {
		return xcode{}, false
	}
	var v struct {
		Version string `plist:"CFBundleShortVersionString"`
		Build   string `plist:"ProductBuildVersion"`
	}
	if _, err := plist.Unmarshal(b, &v); err != nil || v.Build == "" {
		return xcode{}, false
	}
	x.Version, x.Build = v.Version, v.Build
	return x, true
}

// xcodeNow returns the Xcode to use, looked for again every half minute,
// so an Xcode installed or updated meanwhile is noticed.
func (p *SimProvider) xcodeNow() (xcode, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.xAt.IsZero() && p.clock().Sub(p.xAt) < 30*time.Second {
		return p.x, p.xErr
	}
	find := findXcode
	if p.findXcodeFn != nil {
		find = p.findXcodeFn
	}
	x, err := find()
	if x.Build != p.x.Build {
		p.types = nil
	}
	p.x, p.xErr, p.xAt = x, err, p.clock()
	return x, err
}

// simctl runs Xcode's simctl. Its error says what simctl wrote, without the
// line that only names the error's domain.
func (p *SimProvider) simctl(ctx context.Context, x xcode, args ...string) ([]byte, error) {
	if p.simctlFn != nil {
		return p.simctlFn(ctx, args...)
	}
	cmd := exec.CommandContext(ctx, x.tool("simctl"), args...)
	cmd.Env = x.env()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if msg := simctlMessage(stderr.String()); msg != "" {
			return out, fmt.Errorf("simctl %s: %s", args[0], msg)
		}
		return out, fmt.Errorf("simctl %s: %w", args[0], err)
	}
	return out, nil
}

// simctlMessage makes one sentence of simctl's error output.
func simctlMessage(s string) string {
	var parts []string
	for _, l := range strings.Split(s, "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "An error was encountered processing the command") ||
			strings.HasPrefix(l, "Underlying error") {
			continue
		}
		parts = append(parts, l)
	}
	msg := strings.Join(parts, " ")
	if len(msg) > 400 {
		msg = msg[:400] + "…"
	}
	return msg
}

// simEntry is one Simulator device, as simctl lists it.
type simEntry struct {
	UDID              string `json:"udid"`
	Name              string `json:"name"`
	State             string `json:"state"` // "Booted", "Booting", "Shutdown", "Shutting Down"
	IsAvailable       bool   `json:"isAvailable"`
	AvailabilityError string `json:"availabilityError"`
	DeviceType        string `json:"deviceTypeIdentifier"`
	LastUsedAt        string `json:"lastUsedAt"`

	OSVersion string `json:"-"` // from its runtime: "26.5"
}

func (e simEntry) booted() bool { return e.State == "Booted" }

// parseSimDevices reads `simctl list -j devices`, keeping the iOS
// Simulators (iPhones and iPads; not watches, TVs or Vision Pro).
func parseSimDevices(b []byte) ([]simEntry, error) {
	var v struct {
		Devices map[string][]simEntry `json:"devices"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, fmt.Errorf("reading simctl's list: %w", err)
	}
	var out []simEntry
	for rt, ds := range v.Devices {
		ver, ok := iosRuntimeVersion(rt)
		if !ok {
			continue
		}
		for _, d := range ds {
			d.OSVersion = ver
			out = append(out, d)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UDID < out[j].UDID })
	return out, nil
}

// iosRuntimeVersion reads the iOS version from a runtime identifier:
// "com.apple.CoreSimulator.SimRuntime.iOS-26-5" is "26.5".
func iosRuntimeVersion(id string) (string, bool) {
	const prefix = "com.apple.CoreSimulator.SimRuntime.iOS-"
	if !strings.HasPrefix(id, prefix) {
		return "", false
	}
	return strings.ReplaceAll(strings.TrimPrefix(id, prefix), "-", "."), true
}

// parseDeviceTypes reads `simctl list -j devicetypes`: names by identifier.
func parseDeviceTypes(b []byte) (map[string]string, error) {
	var v struct {
		DeviceTypes []struct {
			Identifier string `json:"identifier"`
			Name       string `json:"name"`
		} `json:"devicetypes"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, fmt.Errorf("reading simctl's device types: %w", err)
	}
	m := make(map[string]string, len(v.DeviceTypes))
	for _, t := range v.DeviceTypes {
		m[t.Identifier] = t.Name
	}
	return m, nil
}

// typeName is a device type's name, or one made from its identifier
// ("com.apple.CoreSimulator.SimDeviceType.iPhone-17e" is "iPhone 17e").
func typeName(types map[string]string, id string) string {
	if n := types[id]; n != "" {
		return n
	}
	_, last, _ := strings.Cut(id, "SimDeviceType.")
	return strings.ReplaceAll(last, "-", " ")
}

// deviceSet is the folder of the person's Simulators.
func (p *SimProvider) deviceSet() string {
	if p.setDir != "" {
		return p.setDir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, "Library", "Developer", "CoreSimulator", "Devices")
}

// setStamp changes when a Simulator is added, removed, renamed, booted or
// shut down: CoreSimulator writes each one's state to its device.plist.
// "" when the folder cannot be read.
func setStamp(dir string) string {
	if dir == "" {
		return ""
	}
	es, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	var b strings.Builder
	for _, e := range es {
		if !e.IsDir() {
			continue
		}
		st, err := os.Stat(filepath.Join(dir, e.Name(), "device.plist"))
		if err != nil {
			continue
		}
		fmt.Fprintf(&b, "%s %d %d\n", e.Name(), st.ModTime().UnixNano(), st.Size())
	}
	return b.String()
}

// How long a simctl list is used: it costs a fifth of a second, and
// discovery asks every two. A change to the device set reads it again at
// once; without a stamp, after listFresh.
const (
	listFresh  = 5 * time.Second
	listMaxAge = time.Minute
)

// devices returns the iOS Simulators, read again when the device set
// changed.
func (p *SimProvider) devices(ctx context.Context, x xcode, force bool) ([]simEntry, error) {
	stamp := setStamp(p.deviceSet())
	p.mu.Lock()
	age := p.clock().Sub(p.devsAt)
	fresh := !p.devsAt.IsZero() && !force && age < listMaxAge &&
		(stamp != "" && stamp == p.devStamp || stamp == "" && age < listFresh)
	if fresh {
		devs := p.devs
		p.mu.Unlock()
		return devs, nil
	}
	p.mu.Unlock()
	b, err := p.simctl(ctx, x, "list", "-j", "devices")
	if err != nil {
		return nil, err
	}
	devs, err := parseSimDevices(b)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	p.devs, p.devsAt, p.devStamp = devs, p.clock(), stamp
	p.mu.Unlock()
	return devs, nil
}

// typeNames returns the device types' names, read once per Xcode and again
// when a Simulator has a type not among them.
func (p *SimProvider) typeNames(ctx context.Context, x xcode, devs []simEntry) map[string]string {
	p.mu.Lock()
	types, at := p.types, p.typesAt
	p.mu.Unlock()
	missing := types == nil
	for _, d := range devs {
		if _, ok := types[d.DeviceType]; !ok {
			missing = true
		}
	}
	if !missing || types != nil && p.clock().Sub(at) < time.Minute {
		return types
	}
	b, err := p.simctl(ctx, x, "list", "-j", "devicetypes")
	if err != nil {
		return types
	}
	m, err := parseDeviceTypes(b)
	if err != nil {
		return types
	}
	p.mu.Lock()
	p.types, p.typesAt = m, p.clock()
	p.mu.Unlock()
	return m
}

// List implements driver.Provider: the booted iOS Simulators. One is
// online once WebDriverAgent is built for the Mac's Xcode; until then it
// needs setup, preparing while Pinky builds it.
func (p *SimProvider) List(ctx context.Context) ([]driver.Info, error) {
	x, err := p.xcodeNow()
	if err != nil {
		return nil, nil
	}
	devs, err := p.devices(ctx, x, false)
	if err != nil {
		return nil, err
	}
	types := p.typeNames(ctx, x, devs)
	var out []driver.Info
	for _, d := range devs {
		if !d.booted() || !d.IsAvailable {
			continue
		}
		out = append(out, driver.Info{
			ID:         d.UDID,
			Platform:   driver.IOS,
			Name:       d.Name,
			Model:      typeName(types, d.DeviceType),
			OSVersion:  d.OSVersion,
			Connection: "usb",
			Backend:    backendWDA,
			Virtual:    true,
			VirtualID:  d.UDID,
		})
	}
	p.mu.Lock()
	for _, d := range devs {
		if d.booted() || d.State == "Shutdown" && p.clock().Sub(p.starting[d.UDID]) > 2*time.Minute {
			delete(p.starting, d.UDID)
		}
	}
	p.mu.Unlock()
	if len(out) == 0 {
		return out, nil
	}
	state, setup, detail := p.helperState(x)
	for i := range out {
		out[i].State, out[i].Setup, out[i].Detail = state, setup, detail
	}
	return out, nil
}

// helperState says whether a booted Simulator can be used yet, and starts
// building WebDriverAgent when it is not there for this Xcode.
func (p *SimProvider) helperState(x xcode) (driver.State, string, string) {
	key := buildKey(x, simArch())
	p.mu.Lock()
	defer p.mu.Unlock()
	b := &p.build
	if b.key != key {
		*b = simBuild{key: key, products: builtProducts(p.Dir, key)}
	}
	switch {
	case b.products != "":
		return driver.StateOnline, "", ""
	case b.running:
	case b.err != nil && p.clock().Sub(b.failedAt) < wdaBuildRetry:
		return driver.StateNeedsSetup, driver.SetupSimulatorHelper, b.err.Error()
	default:
		p.startBuildLocked(x)
	}
	return driver.StateNeedsSetup, driver.SetupPreparing,
		"lightpinky is building the WebDriverAgent helper for Simulators with Xcode " + x.Version + ", once per Xcode version; it takes a minute or two."
}

// startBuildLocked builds WebDriverAgent in the background. Under p.mu.
func (p *SimProvider) startBuildLocked(x xcode) {
	b := &p.build
	b.running, b.err = true, nil
	key := b.key
	build := p.buildFn
	if build == nil {
		dir := p.Dir
		build = func(ctx context.Context, x xcode) (string, error) {
			if dir == "" {
				return "", errors.New("lightpinky has no folder to build WebDriverAgent in")
			}
			return buildWDA(ctx, dir, x, simArch(), &http.Client{Timeout: 2 * time.Minute})
		}
	}
	log := p.logger()
	log.Info("ios simulator: building WebDriverAgent", "xcode", x.Version, "build", x.Build, "wda", wdaVersion)
	go func() {
		start := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), wdaBuildTimeout)
		products, err := build(ctx, x)
		cancel()
		if err != nil {
			log.Warn("ios simulator: building WebDriverAgent failed", "err", err)
		} else {
			log.Info("ios simulator: built WebDriverAgent", "took", time.Since(start).Round(time.Second), "to", products)
		}
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.build.key != key {
			return
		}
		p.build.running = false
		p.build.products, p.build.err = products, err
		if err != nil {
			p.build.failedAt = p.clock()
		}
	}()
}

// retryBuild lets the next List build WebDriverAgent again at once, after
// a failure.
func (p *SimProvider) retryBuild() {
	p.mu.Lock()
	if !p.build.running && p.build.err != nil {
		p.build.err, p.build.failedAt = nil, time.Time{}
	}
	p.mu.Unlock()
}

// Open implements driver.Provider: it starts WebDriverAgent's test run on
// the Simulator and connects to it.
func (p *SimProvider) Open(ctx context.Context, id string) (driver.Device, error) {
	infos, err := p.List(ctx)
	if err != nil {
		return nil, err
	}
	var info driver.Info
	for _, i := range infos {
		if i.ID == id {
			info = i
		}
	}
	if info.ID == "" {
		return nil, fmt.Errorf("simulator %s is not booted: %w", id, driver.ErrNotFound)
	}
	if info.State != driver.StateOnline {
		return nil, fmt.Errorf("simulator %s: %s: %w", id, info.Detail, driver.ErrOffline)
	}
	x, err := p.xcodeNow()
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	products := p.build.products
	p.mu.Unlock()
	run, err := startRun(x, products, id, filepath.Join(p.Dir, "runs"))
	if err != nil {
		if errors.Is(err, errBuildGone) {
			p.forgetBuild()
		}
		return nil, err
	}
	start := time.Now()
	octx, cancel := context.WithTimeout(ctx, wdaStartTimeout)
	defer cancel()
	go func() {
		select {
		case <-run.done:
			cancel()
		case <-octx.Done():
		}
	}()
	d, err := openWDA(octx, info, newWDAURL(run.url(), run.mjpegURL()), goios.DeviceEntry{}, nil)
	if err != nil {
		if ended, why := run.exited(); ended {
			err = why
		}
		run.stop()
		p.logger().Warn("ios simulator: WebDriverAgent did not start", "udid", id, "err", err)
		return nil, err
	}
	d.log = p.logger()
	apps := &simApps{p: p, x: x, udid: id}
	d.appsFn = apps.list
	p.logger().Info("ios simulator: WebDriverAgent started", "udid", id, "took", time.Since(start).Round(time.Millisecond))
	return &simPhone{WDADevice: d, run: run}, nil
}

// errBuildGone is a build that went from its folder.
var errBuildGone = errors.New("the WebDriverAgent build is gone")

// forgetBuild makes the next List look for the build again.
func (p *SimProvider) forgetBuild() {
	p.mu.Lock()
	p.build = simBuild{}
	p.mu.Unlock()
}

// simPhone is a Simulator driven through WebDriverAgent: the WDA device,
// whose Close also ends the test run Pinky started for it.
type simPhone struct {
	*WDADevice
	run *simRun
}

// Close implements driver.Device.
func (d *simPhone) Close() error {
	err := d.WDADevice.Close()
	d.run.stop()
	return err
}

// simApps lists a Simulator's apps through simctl, kept a few seconds.
type simApps struct {
	p    *SimProvider
	x    xcode
	udid string

	mu   sync.Mutex
	apps []driver.App
	at   time.Time
}

func (a *simApps) list(ctx context.Context) ([]driver.App, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.apps != nil && time.Since(a.at) < appCacheTTL {
		return append([]driver.App(nil), a.apps...), nil
	}
	b, err := a.p.simctl(ctx, a.x, "listapps", a.udid)
	if err != nil {
		return nil, err
	}
	apps, err := parseSimApps(b)
	if err != nil {
		return nil, err
	}
	a.apps, a.at = apps, time.Now()
	return append([]driver.App(nil), apps...), nil
}

// parseSimApps reads `simctl listapps`, a property list of apps by bundle
// id. WebDriverAgent's runner is left out, as Pinky's helper, and so are
// hidden apps.
func parseSimApps(b []byte) ([]driver.App, error) {
	var m map[string]struct {
		DisplayName     string `plist:"CFBundleDisplayName"`
		Name            string `plist:"CFBundleName"`
		ApplicationType string `plist:"ApplicationType"`
		IsHidden        any    `plist:"IsHidden"` // "1" in simctl's text, else a number or boolean
	}
	if _, err := plist.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("reading simctl's app list: %w", err)
	}
	apps := make([]driver.App, 0, len(m))
	for id, a := range m {
		if strings.HasSuffix(id, ".xctrunner") {
			continue
		}
		if h := fmt.Sprint(a.IsHidden); h == "1" || h == "true" {
			continue // not on the home screen, as CarPlay
		}
		name := a.DisplayName
		if name == "" {
			name = a.Name
		}
		apps = append(apps, driver.App{ID: id, Name: name, System: a.ApplicationType == "System"})
	}
	sort.Slice(apps, func(i, j int) bool { return apps[i].ID < apps[j].ID })
	return apps, nil
}

// VirtualPhones implements driver.VirtualLauncher: the Mac's iOS
// Simulators, the running ones first, then by when they were last used.
func (p *SimProvider) VirtualPhones(ctx context.Context) (driver.VirtualPhones, error) {
	vp := driver.VirtualPhones{Platform: driver.IOS}
	x, err := p.xcodeNow()
	if err != nil {
		vp.Missing, vp.Detail = "xcode", "iOS Simulators need Xcode, from the Mac App Store; this Mac has none, or only its command line tools."
		return vp, nil
	}
	vp.Where = x.App
	devs, err := p.devices(ctx, x, true)
	if err != nil {
		return vp, err
	}
	types := p.typeNames(ctx, x, devs)
	p.mu.Lock()
	starting := map[string]bool{}
	for id := range p.starting {
		starting[id] = true
	}
	p.mu.Unlock()
	sort.SliceStable(devs, func(i, j int) bool {
		a, b := devs[i], devs[j]
		if a.booted() != b.booted() {
			return a.booted()
		}
		if a.LastUsedAt != b.LastUsedAt {
			return a.LastUsedAt > b.LastUsedAt // RFC 3339 in UTC sorts as text
		}
		return a.Name < b.Name
	})
	for _, d := range devs {
		v := driver.VirtualPhone{
			ID: d.UDID, Name: d.Name, Model: typeName(types, d.DeviceType), OSVersion: d.OSVersion,
			Running:  d.booted(),
			Starting: d.State == "Booting" || starting[d.UDID] && !d.booted(),
		}
		if v.Running {
			v.DeviceID = d.UDID
		}
		if !d.IsAvailable {
			v.Unavailable = d.AvailabilityError
			if v.Unavailable == "" {
				v.Unavailable = "its iOS runtime is not installed"
			}
		}
		vp.Phones = append(vp.Phones, v)
	}
	return vp, nil
}

// StartVirtual implements driver.VirtualLauncher: it boots the Simulator
// and opens Simulator.app on it. A Simulator already booted is left as it
// is, and a failed WebDriverAgent build is tried again at once.
func (p *SimProvider) StartVirtual(ctx context.Context, id string) error {
	x, err := p.xcodeNow()
	if err != nil {
		return err
	}
	devs, err := p.devices(ctx, x, true)
	if err != nil {
		return err
	}
	var dev *simEntry
	for i := range devs {
		if devs[i].UDID == id {
			dev = &devs[i]
		}
	}
	if dev == nil {
		return fmt.Errorf("simulator %s: %w", id, driver.ErrNotFound)
	}
	p.retryBuild()
	if dev.State == "Shutdown" {
		if _, err := p.simctl(ctx, x, "boot", id); err != nil {
			// It may have booted meanwhile.
			if devs, lerr := p.devices(ctx, x, true); lerr != nil || !simBooted(devs, id) {
				return err
			}
		}
		p.mu.Lock()
		if p.starting == nil {
			p.starting = map[string]time.Time{}
		}
		p.starting[id] = p.clock()
		p.mu.Unlock()
	}
	p.showSimulator(x, id)
	p.mu.Lock()
	p.devsAt = time.Time{} // list it as booted at the next pass
	p.mu.Unlock()
	return nil
}

func simBooted(devs []simEntry, id string) bool {
	for _, d := range devs {
		if d.UDID == id {
			return d.booted() || d.State == "Booting"
		}
	}
	return false
}

// showSimulator opens Simulator.app on a Simulator, so the person sees it.
// Pinky drives it either way. Xcode 27 has no Simulator.app: its Device
// Hub opens on the device it showed last, often a real iPhone, and takes
// no device to show, so the person sees the Simulator in Pinky's mirror.
func (p *SimProvider) showSimulator(x xcode, id string) {
	if p.simctlFn != nil {
		return // tests
	}
	app := filepath.Join(x.Developer, "Applications", "Simulator.app")
	if _, err := os.Stat(app); err != nil {
		p.logger().Debug("ios simulator: no Simulator.app to show it in", "xcode", x.Developer)
		return
	}
	cmd := exec.Command("/usr/bin/open", "-a", app, "--args", "-CurrentDeviceUDID", id)
	cmd.Env = x.env()
	if err := cmd.Run(); err != nil {
		p.logger().Warn("ios simulator: opening Simulator.app", "err", err)
	}
}

// StopVirtual implements driver.VirtualLauncher: simctl shutdown.
func (p *SimProvider) StopVirtual(ctx context.Context, id string) error {
	x, err := p.xcodeNow()
	if err != nil {
		return err
	}
	if _, err := p.simctl(ctx, x, "shutdown", id); err != nil {
		if devs, lerr := p.devices(ctx, x, true); lerr == nil && !simBooted(devs, id) {
			return nil // already shut down
		}
		return err
	}
	p.mu.Lock()
	delete(p.starting, id)
	p.devsAt = time.Time{}
	p.mu.Unlock()
	return nil
}

var (
	_ driver.Provider        = (*SimProvider)(nil)
	_ driver.VirtualLauncher = (*SimProvider)(nil)
	_ driver.Device          = (*simPhone)(nil)
	_ driver.Checker         = (*simPhone)(nil)
	_ driver.LockReporter    = (*simPhone)(nil)
)
