package ios

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tetratorus/lightpinky/internal/driver"
	goios "github.com/danielpaulus/go-ios/ios"
	"github.com/danielpaulus/go-ios/ios/imagemounter"
)

// minIOS is the first iOS whose CoreDevice services stream the screen and
// take HID input. Older phones go other ways; see pathFor.
const minIOS = 27

// Provider discovers iPhones through usbmuxd (Apple Devices on Windows,
// usbmuxd on Linux, built in on macOS): on a cable, and over Wi-Fi once
// SetWiFi turns that on (wifi.go).
type Provider struct {
	// Only, when set, limits the provider to these phones, by UDID: it does
	// not look at the others usbmuxd lists at all, not even their names
	// (PINKY_DEVICES, tests).
	Only map[string]bool

	// DiskImageDir is Pinky's own folder for Apple's developer disk image
	// (an iOS_DDI folder in it), looked in before Xcode's; "" for none.
	DiskImageDir string

	// WiFiMux lets the provider run a usbmuxd of its own on Linux and
	// Windows, whose usbmuxd lists phones on Wi-Fi not at all or only while
	// they are on a cable too, to find phones on Wi-Fi (netmux.go). Only the
	// app's provider sets it: it points go-ios, for the whole process, at
	// that usbmuxd, which then carries every connection to an iPhone.
	WiFiMux bool

	// Log gets a line for each step Pinky takes to prepare a phone:
	// mounting the developer disk image, showing the Developer Mode
	// switch, asking for trust, downloading the image. nil for none.
	Log *slog.Logger

	mu          sync.Mutex
	ddi         map[string]ddiCheck // by UDID
	known       map[string]known    // by UDID, what List last found
	setup       func(ctx context.Context, e goios.DeviceEntry) (bool, error)
	legacySetup func(ctx context.Context, e goios.DeviceEntry) (bool, error)
	findRunner  func(e goios.DeviceEntry) (string, error)
	ways        map[string]string    // by UDID: the connection the person chose (see SetConnection)
	wdaWaits    map[string]*wdaStart // by UDID: a runner waiting for the passcode (see wdaState)
	wdaHolds    map[string]wdaHold   // by UDID: no new runner until then

	// The helper's provisioning profile (wda_profile.go): by UDID, the
	// helper whose profile List reads again, and for tests how it is read.
	helpers  map[string]helperRead
	helperFn func(ctx context.Context, e goios.DeviceEntry, r wdaRunner) (time.Time, error)

	// For tests: how a phone is inspected, and the clock.
	inspectFn func(ctx context.Context, e goios.DeviceEntry) driver.Info
	now       func() time.Time

	wifi       wifiState                                               // phones over Wi-Fi (wifi.go)
	lost       lostState                                               // phones usbmuxd lost on Wi-Fi (lost.go)
	nmux       muxState                                                // a usbmuxd of its own on Linux (netmux.go)
	restartFn  func(ctx context.Context, command, reason string) error // for tests: runAsAdmin
	prep       preparer                                                // Developer Mode and the developer disk image (prepare.go)
	muxErr     error                                                   // usbmuxd's answer to the last List
	muxChecked bool
	muxFails   int                              // Lists in a row that usbmuxd did not answer
	muxPhones  int                              // phones the last answered List found
	listFn     func() (goios.DeviceList, error) // for tests: usbmuxd's list
	openFails  map[string]string                // by UDID: why the last open got no picture (noVideoError.Detail)
}

// recheckOffline is how often List asks a phone that is not online again,
// and one driven without the WebDriverAgent helper, so a helper installed
// meanwhile is noticed. Other online phones are asked again when their
// usbmuxd entry changes, which it does when the phone reconnects or
// restarts, and when their developer services go away (recheckOnline).
const recheckOffline = 30 * time.Second

// recheckOnline is how often List looks at an online phone's developer
// services again: another tool can take the developer disk image off while
// the phone stays attached, and Pinky then mounts it again.
const recheckOnline = time.Minute

// wdaHold keeps Pinky from starting the helper on a phone again for a
// while, after the phone asked for its passcode to allow automation and
// nobody entered it while the runner waited. The request stays on the
// phone, and a passcode entered there later counts for the next start.
// Each miss in a row doubles the wait.
type wdaHold struct {
	key    entryKey
	until  time.Time
	misses int
}

// How long Pinky waits after a missed passcode before starting the helper
// again, at first and at most.
const (
	wdaHoldFirst = 2 * time.Minute
	wdaHoldMax   = 30 * time.Minute
)

// known is an inspected phone and the usbmuxd entry it was seen through.
type known struct {
	key  entryKey
	info driver.Info
	at   time.Time
}

// entryKey identifies one usbmuxd connection to a phone. usbmuxd gives a
// phone a new DeviceID each time it attaches.
type entryKey struct {
	deviceID   int
	connection string
}

func keyOf(e goios.DeviceEntry) entryKey {
	return entryKey{deviceID: e.DeviceID, connection: e.Properties.ConnectionType}
}

type ddiCheck struct {
	ok  bool
	at  time.Time
	err string
	key entryKey // the attach it was seen through: a phone that reconnects, or restarts, is looked at again
}

// NewProvider returns an iOS provider.
func NewProvider() *Provider {
	return &Provider{ddi: map[string]ddiCheck{}, known: map[string]known{}, setup: probeServices, legacySetup: probeLegacyServices}
}

// Platform implements driver.Provider.
func (p *Provider) Platform() driver.Platform { return driver.IOS }

// List implements driver.Provider. It checks what Pinky needs and says in
// Detail how to fix what is missing. A gateway calls it every few seconds,
// so it asks each phone again only when needed (see recheckOffline).
func (p *Provider) List(ctx context.Context) ([]driver.Info, error) {
	list, ok, err := p.listUsbmuxd()
	if !ok {
		return nil, err // nil without usbmuxd: CheckHost says what to install
	}
	entries := p.entries(list)
	p.noteWiFiListing(entries)
	out := make([]driver.Info, len(entries))
	listed := make([]bool, len(entries))
	var wg sync.WaitGroup
	for i, e := range entries {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if p.foundOnly(e) {
				// Over Wi-Fi only, and not the person's yet: its name and
				// nothing else (wifi.go).
				out[i], listed[i] = p.foundInfo(ctx, e)
				return
			}
			if e.Properties.ConnectionType == "Network" && p.dozing(e.Properties.SerialNumber) {
				// Back on Wi-Fi a moment after it dropped off, with no
				// passcode to tell: most likely asleep (wifi.go).
				return
			}
			out[i], listed[i] = p.info(ctx, e), true
		}()
	}
	wg.Wait()
	p.allowWiFi(entries, out, listed)
	p.forgetExcept(entries)
	kept := out[:0]
	ids := map[string]bool{}
	for i, info := range out {
		if listed[i] {
			kept = append(kept, info)
			ids[info.ID] = true
		}
	}
	// Phones on Wi-Fi that dropped off usbmuxd's list a moment ago, as one
	// that sleeps does (wifi.go).
	kept = append(kept, p.heldFound(ids)...)
	kept = append(kept, p.heldAsleep(ids)...)
	p.remember(kept)
	// Phones usbmuxd lost on Wi-Fi though they are on the network (lost.go).
	onMux := map[string]bool{}
	onNet := map[string]bool{}
	onUSB := map[string]bool{}
	for _, e := range list {
		onMux[e.Properties.SerialNumber] = true
		if e.Properties.ConnectionType == "Network" {
			onNet[e.Properties.SerialNumber] = true
		} else {
			onUSB[e.Properties.SerialNumber] = true
		}
	}
	for _, info := range p.lostPhones(onMux, onUSB, onNet) {
		if !ids[info.ID] && (p.Only == nil || p.Only[info.ID]) {
			kept = append(kept, info)
		}
	}
	// What each one's Wi-Fi connection is (wifiallow.go).
	for i := range kept {
		kept[i].WiFi = p.wifiConnection(kept[i].ID, onNet[kept[i].ID])
	}
	return kept, nil
}

// foundOnly reports whether Pinky reaches the phone behind e only over
// Wi-Fi and may not prepare or open it yet (wifi.go).
func (p *Provider) foundOnly(e goios.DeviceEntry) bool {
	return e.Properties.ConnectionType == "Network" && !p.usesOverWiFi(e.Properties.SerialNumber)
}

// entries picks one usbmuxd entry per phone, sorted by UDID. A phone on USB
// and Wi-Fi at once is listed twice; USB wins. Entries over the network
// count only while Pinky uses phones over Wi-Fi, and not for phones the
// person removed or ignored (wifi.go).
func (p *Provider) entries(list []goios.DeviceEntry) []goios.DeviceEntry {
	byID := map[string]goios.DeviceEntry{}
	for _, e := range list {
		if p.Only != nil && !p.Only[e.Properties.SerialNumber] {
			continue
		}
		if e.Properties.ConnectionType == "Network" && !p.listedOverWiFi(e.Properties.SerialNumber) {
			continue
		}
		id := e.Properties.SerialNumber
		if prev, ok := byID[id]; ok && prev.Properties.ConnectionType == "USB" {
			continue
		}
		byID[id] = e
	}
	out := make([]goios.DeviceEntry, 0, len(byID))
	for _, e := range byID {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Properties.SerialNumber < out[j].Properties.SerialNumber })
	return out
}

// info returns what is known about the phone behind e. It inspects the
// phone again only when its entry changed, every recheckOffline while it is
// not online, or when its developer services went away (recheckOnline), so
// a phone that stays attached costs little more than a look at its services
// a minute.
func (p *Provider) info(ctx context.Context, e goios.DeviceEntry) driver.Info {
	id, key := e.Properties.SerialNumber, keyOf(e)
	p.mu.Lock()
	k, ok := p.known[id]
	p.mu.Unlock()
	if ok && k.key == key && k.info.State == driver.StateOnline && k.info.Backend == backendWDA {
		if info, waiting := p.wdaState(ctx, key, k.info); waiting {
			return info
		}
		var lasts bool
		k.info, lasts = p.helperStill(ctx, e, k)
		ok = ok && lasts // its provisioning profile ran out: inspect it again
	}
	if ok && k.key == key && k.info.State == driver.StateOnline && k.info.Backend != backendAccessibility {
		if p.clock().Sub(k.at) < recheckOnline || p.servicesStill(ctx, e, k) {
			return k.info
		}
	} else if ok && k.key == key && p.clock().Sub(k.at) < recheckFor(k.info) {
		return k.info
	}
	inspect := p.inspectFn
	if inspect == nil {
		inspect = p.inspect
	}
	info := inspect(ctx, e)
	if ctx.Err() != nil {
		return info // cut short; do not remember it
	}
	p.mu.Lock()
	if p.known == nil {
		p.known = map[string]known{}
	}
	p.known[id] = known{key: key, info: info, at: p.clock()}
	p.mu.Unlock()
	return info
}

// forgetExcept drops what List knew about phones that are gone, and stops
// the helper's runners that waited on them.
func (p *Provider) forgetExcept(entries []goios.DeviceEntry) {
	listed := make(map[string]bool, len(entries))
	for _, e := range entries {
		listed[e.Properties.SerialNumber] = true
	}
	var stop []*wdaStart
	p.mu.Lock()
	for id := range p.known {
		if !listed[id] {
			delete(p.known, id)
		}
	}
	for id, f := range p.wifi.found {
		if !listed[id] && p.clock().Sub(f.at) >= foundReread && p.clock().Sub(f.seen) >= foundHold {
			delete(p.wifi.found, id)
		}
	}
	for id, s := range p.wdaWaits {
		if !listed[id] {
			stop = append(stop, s)
			delete(p.wdaWaits, id)
		}
	}
	for id := range p.openFails {
		if !listed[id] {
			delete(p.openFails, id)
		}
	}
	for id := range p.wdaHolds {
		if !listed[id] {
			delete(p.wdaHolds, id)
		}
	}
	p.forgetHelpers(listed)
	p.mu.Unlock()
	for _, s := range stop {
		go s.stop()
	}
}

// wdaState is how a phone driven through the helper is while its runner
// waits for the passcode (see errAutomation), or for a while after nobody
// entered it: needing setup, so the gateway does not open it, with what to
// do. waiting is false for any other phone, and once the helper answers
// (the next Open takes the waiting runner over).
func (p *Provider) wdaState(ctx context.Context, key entryKey, info driver.Info) (driver.Info, bool) {
	id := info.ID
	p.mu.Lock()
	s := p.wdaWaits[id]
	if s != nil && keyOf(s.base) != key {
		// The phone reconnected or restarted: that runner is gone with
		// the old connection, and nobody missed anything.
		delete(p.wdaWaits, id)
		go s.stop()
		s = nil
	}
	p.mu.Unlock()
	if s != nil {
		if s.ready(ctx) {
			return info, false
		}
		if s.running() {
			info.State, info.Setup, info.Detail = driver.StateNeedsSetup, driver.SetupPasscode, automationAsk
			return info, true
		}
		// The runner gave up waiting: nobody entered the passcode.
		p.mu.Lock()
		if p.wdaWaits[id] == s {
			delete(p.wdaWaits, id)
			h := p.wdaHolds[id]
			if h.key != key {
				h = wdaHold{key: key}
			}
			h.misses++
			wait := wdaHoldFirst << (h.misses - 1)
			if wait > wdaHoldMax || wait <= 0 {
				wait = wdaHoldMax
			}
			h.until = p.clock().Add(wait)
			if p.wdaHolds == nil {
				p.wdaHolds = map[string]wdaHold{}
			}
			p.wdaHolds[id] = h
		}
		p.mu.Unlock()
		go s.stop()
	}
	p.mu.Lock()
	h, held := p.wdaHolds[id]
	p.mu.Unlock()
	if held && h.key == key && p.clock().Before(h.until) {
		info.State, info.Setup, info.Detail = driver.StateNeedsSetup, driver.SetupPasscode, automationMissed
		return info, true
	}
	return info, false
}

// openWDA opens a phone through the helper, taking over the runner that
// waited for the passcode. When the phone asks for it, the runner is kept
// for List to watch (see wdaState) instead of started again every pass.
func (p *Provider) openWDA(ctx context.Context, info driver.Info) (driver.Device, error) {
	p.mu.Lock()
	s := p.wdaWaits[info.ID]
	delete(p.wdaWaits, info.ID)
	p.mu.Unlock()
	if s == nil {
		var err error
		if s, err = startWDAFor(ctx, info); err != nil {
			return nil, err
		}
	}
	d, err := s.open(ctx, info)
	switch {
	case err == nil:
		p.mu.Lock()
		delete(p.wdaHolds, info.ID)
		p.mu.Unlock()
		d.log = p.logger()
		return d, nil
	case errors.Is(err, errAutomation):
		p.mu.Lock()
		if p.wdaWaits == nil {
			p.wdaWaits = map[string]*wdaStart{}
		}
		p.wdaWaits[info.ID] = s
		p.mu.Unlock()
	default:
		s.stop()
	}
	return nil, err
}

func (p *Provider) logger() *slog.Logger { return logTo(p.Log) }

func (p *Provider) clock() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

func (p *Provider) inspect(ctx context.Context, e goios.DeviceEntry) driver.Info {
	info := driver.Info{
		ID:         e.Properties.SerialNumber,
		Platform:   driver.IOS,
		Connection: "usb",
		Backend:    "coredevice",
		State:      driver.StateOnline,
	}
	if e.Properties.ConnectionType == "Network" {
		info.Connection = "wifi"
	}
	values, err := getValues(ctx, e)
	if err != nil {
		if held, ok := p.stillAsleep(info.ID); ok && info.Connection == "wifi" {
			// Locked a moment ago, and asleep: it answers now and then.
			return held
		}
		if !isPairingError(err) {
			info.State = driver.StateOffline
			info.Detail = "The iPhone does not answer: " + err.Error()
			return info
		}
		if info.Connection == "wifi" {
			// The phone asks to trust a computer only on a cable.
			info = trustState(info, err)
			info.Detail = trustWiFiDetail
			return info
		}
		return trustState(info, p.prep.askTrust(e, err, p.logger()))
	}
	info.Name = values.Value.DeviceName
	info.Model = modelName(values.Value.ProductType)
	info.OSVersion = values.Value.ProductVersion
	if values.Value.PasswordProtected {
		p.noteLocked(info.ID) // it has a passcode (wifi.go, dozing)
		if info.Connection == "wifi" {
			// Locked on Wi-Fi, most likely asleep: Pinky waits until it
			// is unlocked (wifi.go).
			return p.lockedOnWiFi(info)
		}
	}
	p.awake(info.ID)
	kind, why := pathFor(parseVersion(info.OSVersion))
	if kind == pathUnsupported {
		info.State = driver.StateNeedsSetup
		info.Detail = why
		if parseVersion(info.OSVersion).Major != 0 {
			info.Setup = driver.SetupUpdate
		}
		return info
	}
	dev, err := bounded(ctx, lockdownTimeout, func() (bool, error) { return imagemounter.IsDevModeEnabled(e) })
	if err == nil && !dev {
		return p.devModeOff(e, info)
	}
	switch p.way(info.ID) {
	case backendWDA:
		return p.inspectWDA(ctx, e, info)
	case backendCoreDevice:
		if kind != pathCoreDevice {
			return coreDeviceTooOldState(info)
		}
	default:
		if kind == pathLegacy {
			return p.inspectLegacy(ctx, e, info)
		}
	}
	ok, err := p.services(ctx, e)
	switch {
	case err != nil:
		info.State = driver.StateOffline
		info.Detail = "Could not reach the iPhone's developer services: " + err.Error()
	case !ok:
		return p.diskImageMissing(e, info, p.setup)
	}
	return info
}

// services reports whether the screen and HID services are published, which
// they are once the developer disk image is mounted. It needs a tunnel, so
// results are cached: a mounted image stays mounted until the phone restarts.
func (p *Provider) services(ctx context.Context, e goios.DeviceEntry) (bool, error) {
	return p.servicesWith(ctx, e, "display", p.setup)
}

// servicesWith is services with the check to run, cached by the phone and
// the check's name.
func (p *Provider) servicesWith(ctx context.Context, e goios.DeviceEntry, check string, setup func(context.Context, goios.DeviceEntry) (bool, error)) (bool, error) {
	id, key := e.Properties.SerialNumber+"|"+check, keyOf(e)
	p.mu.Lock()
	c, ok := p.ddi[id]
	p.mu.Unlock()
	if ok && c.key == key && ((c.ok && time.Since(c.at) < 5*time.Minute) || (!c.ok && time.Since(c.at) < 10*time.Second)) {
		if c.err != "" {
			return false, errors.New(c.err)
		}
		return c.ok, nil
	}
	ok, err := setup(ctx, e)
	c = ddiCheck{ok: ok, at: time.Now(), key: key}
	if err != nil {
		c.err = err.Error()
	}
	p.mu.Lock()
	p.ddi[id] = c
	p.mu.Unlock()
	return ok, err
}

// servicesStill looks again at the developer services of a phone List knows
// as online, and reports whether they are still there. A failed look counts
// as there; only services seen missing make List inspect the phone again,
// which mounts the image.
func (p *Provider) servicesStill(ctx context.Context, e goios.DeviceEntry, k known) bool {
	check, setup := "display", p.setup
	if k.info.Backend == backendWDA {
		check, setup = "developer", p.legacySetup
	}
	ok, err := setup(ctx, e)
	id := e.Properties.SerialNumber
	p.mu.Lock()
	defer p.mu.Unlock()
	if err == nil && !ok {
		if p.ddi == nil {
			p.ddi = map[string]ddiCheck{}
		}
		p.ddi[id+"|"+check] = ddiCheck{at: time.Now(), key: k.key}
		p.logger().Info("iphone: the developer services went away; looking at the phone again", "device", id)
		return false
	}
	if now, still := p.known[id]; still && now.key == k.key {
		now.at = p.clock()
		p.known[id] = now
	}
	return true
}

// inspectLegacy finishes inspecting a phone on iOS 17.4 to 26: the
// developer services must be there (the developer disk image mounted), and
// the WebDriverAgent helper decides the backend.
func (p *Provider) inspectLegacy(ctx context.Context, e goios.DeviceEntry, info driver.Info) driver.Info {
	ok, err := p.servicesWith(ctx, e, "developer", p.legacySetup)
	switch {
	case err != nil:
		info.State = driver.StateOffline
		info.Detail = "Could not reach the iPhone's developer services: " + err.Error()
		return info
	case !ok:
		return p.diskImageMissing(e, info, p.legacySetup)
	}
	info.Backend = backendAccessibility
	if r, err := p.runnerApp(e); err == nil && r.BundleID != "" {
		info.Backend = backendWDA
		return p.withHelper(ctx, e, info, r)
	}
	return info
}

// inspectWDA finishes inspecting a phone the person chose to drive through
// the WebDriverAgent helper, on any iOS Pinky supports: the developer
// services must be there (testmanagerd runs the helper), and the helper
// must be installed.
func (p *Provider) inspectWDA(ctx context.Context, e goios.DeviceEntry, info driver.Info) driver.Info {
	ok, err := p.servicesWith(ctx, e, "developer", p.legacySetup)
	switch {
	case err != nil:
		info.State = driver.StateOffline
		info.Detail = "Could not reach the iPhone's developer services: " + err.Error()
		return info
	case !ok:
		return p.diskImageMissing(e, info, p.legacySetup)
	}
	r, err := p.runnerApp(e)
	if err != nil || r.BundleID == "" {
		info.State, info.Setup = driver.StateNeedsSetup, driver.SetupInstallHelper
		info.Detail = wdaMissing
		return info
	}
	info.Backend = backendWDA
	return p.withHelper(ctx, e, info, r)
}

func (p *Provider) runner(e goios.DeviceEntry) (string, error) {
	find := p.findRunner
	if find == nil {
		find = findWDARunner
	}
	return find(e)
}

// Sentences for a chosen connection the phone cannot use, and for the
// passcode the phone asks for before the helper can control it.
const (
	coreDeviceTooOld = "Connecting without the helper needs iOS 27 or later; this iPhone runs iOS %s. Choose Automatic or WebDriverAgent in Settings > Phones."
	wdaMissing       = "The WebDriverAgent helper is not installed on this iPhone. Install it once from Xcode (docs/notes/ios.md, \"Installing the WebDriverAgent helper\"), or choose another connection in Settings > Phones."
	automationAsk    = "Enter the passcode on the iPhone: iOS asks for it now and then before the WebDriverAgent helper can control the phone."
	automationMissed = "The iPhone asked for its passcode to let the WebDriverAgent helper control it. If it still asks, enter the passcode there. Pinky tries again in a few minutes, or at once when you reconnect the iPhone or choose its connection again in Settings > Phones."
)

// coreDeviceTooOldState fills in a phone the person chose to connect
// without the helper (CoreDevice) that runs an iOS below 27: it needs
// another connection, which the person chooses in Settings › Phones.
func coreDeviceTooOldState(info driver.Info) driver.Info {
	info.State, info.Setup = driver.StateNeedsSetup, driver.SetupConnection
	info.Detail = fmt.Sprintf(coreDeviceTooOld, info.OSVersion)
	return info
}

// way is the connection the person chose for a phone.
func (p *Provider) way(id string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.ways[id]
}

// SetConnection implements driver.ConnectionChooser: "coredevice",
// "wda" or driver.ConnectionAuto. The phone is inspected again at the next
// List, and its next Open goes the chosen way.
func (p *Provider) SetConnection(id, way string) error {
	switch way {
	case driver.ConnectionAuto, backendCoreDevice, backendWDA:
	default:
		return fmt.Errorf("connection %q: %w", way, driver.ErrUnsupported)
	}
	p.mu.Lock()
	if p.ways == nil {
		p.ways = map[string]string{}
	}
	if way == driver.ConnectionAuto {
		delete(p.ways, id)
	} else {
		p.ways[id] = way
	}
	delete(p.known, id)
	s := p.wdaWaits[id]
	delete(p.wdaWaits, id)
	delete(p.wdaHolds, id)
	p.mu.Unlock()
	if s != nil {
		go s.stop()
	}
	return nil
}

// Connections implements driver.ConnectionChooser: Automatic, CoreDevice
// (iOS 27 and later) and WebDriverAgent (the helper installed), with what
// each needs that the phone lacks. It asks lockdown for the version and the
// installation proxy for the helper, both quick.
func (p *Provider) Connections(ctx context.Context, id string) driver.Connections {
	out := driver.Connections{Chosen: p.way(id)}
	p.mu.Lock()
	k, seen := p.known[id]
	p.mu.Unlock()
	if seen && k.info.State == driver.StateOnline {
		out.Current = k.info.Backend
	}
	auto := driver.Connection{ID: driver.ConnectionAuto, Available: true}
	cd := driver.Connection{ID: backendCoreDevice}
	wda := driver.Connection{ID: backendWDA}
	offline := func(c *driver.Connection, err error) {
		c.Reason, c.Detail = "offline", "The iPhone does not answer: "+err.Error()
	}
	e, err := p.entryFor(id)
	if err == nil && e.Properties.ConnectionType == "Network" && (!p.listedOverWiFi(id) || !p.usesOverWiFi(id)) {
		// Not the person's yet, or Wi-Fi is off: nothing is asked of it.
		err = errors.New("the iPhone is not connected by cable, and Pinky does not use it over Wi-Fi")
	}
	if err != nil {
		offline(&cd, err)
		offline(&wda, err)
		out.Ways = []driver.Connection{auto, cd, wda}
		return out
	}
	version := k.info.OSVersion
	if !seen || version == "" {
		if values, err := getValues(ctx, e); err == nil {
			version = values.Value.ProductVersion
		} else {
			offline(&cd, err)
			offline(&wda, err)
			out.Ways = []driver.Connection{auto, cd, wda}
			return out
		}
	}
	kind, why := pathFor(parseVersion(version))
	switch kind {
	case pathCoreDevice:
		cd.Available = true
	case pathUnsupported:
		cd.Reason, cd.Detail = "os_version", why
	default:
		cd.Reason, cd.Detail = "os_version", fmt.Sprintf(coreDeviceTooOld, version)
	}
	switch {
	case kind == pathUnsupported:
		wda.Reason, wda.Detail = "os_version", why
	default:
		if rid, err := p.runner(e); err != nil {
			offline(&wda, err)
		} else if rid == "" {
			wda.Reason, wda.Detail = "not_installed", wdaMissing
		} else {
			wda.Available = true
		}
	}
	if kind == pathUnsupported {
		auto.Available, auto.Reason, auto.Detail = false, "os_version", why
	}
	out.Ways = []driver.Connection{auto, cd, wda}
	return out
}

// probeLegacyServices reports whether a phone below iOS 27 lists what Pinky
// uses there (see legacyReady).
func probeLegacyServices(ctx context.Context, e goios.DeviceEntry) (bool, error) {
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	l, err := dialLinkEntry(cctx, e)
	if err != nil {
		return false, err
	}
	defer l.Close()
	return legacyReady(l), nil
}

// legacyReady reports whether a phone below iOS 27 lists instruments, for
// screenshots, and a service only the developer disk image publishes, for
// XCTest (WebDriverAgent runs on the image's frameworks). Instruments and
// the accessibility daemons come with iOS itself: iOS 27 lists them with
// no image mounted, so they say nothing about the image.
func legacyReady(l *link) bool {
	if !l.has(instrumentsService) {
		return false
	}
	for _, s := range diskImageServices {
		if l.has(s) {
			return true
		}
	}
	return false
}

// diskImageServices are what the developer disk image publishes, through
// its launch daemons (Xcode 27.2's image, cryptex and personalized alike).
// They are listed whenever the image is mounted, also where the phone
// refuses them below iOS 27.
var diskImageServices = []string{displayService, universalHIDService, screenCaptureService, pasteboardService, deviceInfoService}

// instrumentsService is DTServiceHub, instruments over RemoteXPC (iOS 17+).
const instrumentsService = "com.apple.instruments.dtservicehub"

func probeServices(ctx context.Context, e goios.DeviceEntry) (bool, error) {
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	l, err := dialLinkEntry(cctx, e)
	if err != nil {
		return false, err
	}
	defer l.Close()
	return l.has(displayService) && l.has(universalHIDService), nil
}

// Open implements driver.Provider. It checks only the phone it opens, and
// reuses what List found when the phone's entry has not changed.
func (p *Provider) Open(ctx context.Context, id string) (driver.Device, error) {
	if p.Only != nil && !p.Only[id] {
		return nil, fmt.Errorf("%s is not among the phones this provider may use: %w", id, driver.ErrNotFound)
	}
	entry, err := p.entryFor(id)
	if err != nil {
		return nil, fmt.Errorf("%v: %w", err, driver.ErrNotFound)
	}
	if entry.Properties.ConnectionType == "Network" {
		switch {
		case !p.listedOverWiFi(id):
			return nil, fmt.Errorf("%s is reachable only over Wi-Fi, which Pinky does not use for it: %w", id, driver.ErrNotFound)
		case !p.usesOverWiFi(id):
			return nil, fmt.Errorf("%s was found on Wi-Fi, and the person has not chosen to use it", id)
		}
	}
	info := p.info(ctx, entry)
	if info.State != driver.StateOnline {
		return nil, fmt.Errorf("%s is %s: %s", id, info.State, info.Detail)
	}
	dev, err := p.openBackend(ctx, info)
	p.noteOpen(id, err)
	if err != nil {
		if info.Backend == backendCoreDevice {
			// Most often the developer services went away since List
			// looked: the next List looks again, and mounts the image.
			p.forgetPhone(id)
		}
		return nil, err
	}
	if s, ok := dev.(imageDirSetter); ok {
		s.setImageDir(p.DiskImageDir) // for the checkup (checkup.go)
	}
	return dev, nil
}

// openBackend opens the phone the way its Info.Backend says.
func (p *Provider) openBackend(ctx context.Context, info driver.Info) (driver.Device, error) {
	switch info.Backend {
	case backendWDA:
		return p.openWDA(ctx, info)
	case backendAccessibility:
		return OpenAccessibility(ctx, info)
	}
	return open(ctx, info, p.logger())
}

// noteOpen remembers an open that failed for want of a picture, for the
// phone's checkup (OpenChecks), and forgets it once the phone opens or
// fails otherwise.
func (p *Provider) noteOpen(id string, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var nv *noVideoError
	if !errors.As(err, &nv) {
		delete(p.openFails, id)
		return
	}
	if p.openFails == nil {
		p.openFails = map[string]string{}
	}
	p.openFails[id] = nv.Detail()
}

// OpenChecks implements driver.OpenChecker: a phone whose last open got no
// picture from it fails the checkup's screen item, with what came instead.
func (p *Provider) OpenChecks(id string) []driver.Check {
	p.mu.Lock()
	detail, ok := p.openFails[id]
	p.mu.Unlock()
	if !ok {
		return nil
	}
	return []driver.Check{noVideoCheck(detail)}
}

// noVideoCheck is the checkup's screen item of a phone that sends no
// picture: restarting it is what helps first.
func noVideoCheck(detail string) driver.Check {
	return driver.Check{ID: driver.CheckScreen, Status: driver.CheckFail, Fix: driver.FixRestart,
		Detail: "no live picture from the phone. " + detail}
}

func isPairingError(err error) bool {
	m := strings.ToLower(err.Error())
	for _, s := range []string{"pair", "invalidhostid", "passwordprotected", "trust", "sslhandshake"} {
		if strings.Contains(m, s) {
			return true
		}
	}
	return false
}

func major(version string) int {
	v, _, _ := strings.Cut(version, ".")
	n, _ := strconv.Atoi(v)
	return n
}

// modelName turns a product type into a marketing name, for the models
// Pinky can drive (iOS 17.4 runs on iPhone XS, XR and later, and iPadOS 17
// on iPads from 2018 on).
// twoScreenModels are the phones with two screens of their own, by
// product type: the iPhone Duo folds, with a display inside and one
// outside (display_info.go).
var twoScreenModels = []string{"iPhone19,4"}

// twoScreens reports whether model, as driver.Info.Model names it
// (modelName), has two screens.
func twoScreens(model string) bool {
	for _, pt := range twoScreenModels {
		if model == pt || model == modelName(pt) {
			return true
		}
	}
	return false
}

func modelName(productType string) string {
	if n, ok := models[productType]; ok {
		return n
	}
	return productType
}

var models = map[string]string{
	"iPhone11,2": "iPhone XS", "iPhone11,4": "iPhone XS Max", "iPhone11,6": "iPhone XS Max", "iPhone11,8": "iPhone XR",
	"iPhone12,1": "iPhone 11", "iPhone12,3": "iPhone 11 Pro", "iPhone12,5": "iPhone 11 Pro Max", "iPhone12,8": "iPhone SE (2nd generation)",
	"iPhone13,1": "iPhone 12 mini", "iPhone13,2": "iPhone 12", "iPhone13,3": "iPhone 12 Pro", "iPhone13,4": "iPhone 12 Pro Max",
	"iPhone14,4": "iPhone 13 mini", "iPhone14,5": "iPhone 13", "iPhone14,2": "iPhone 13 Pro", "iPhone14,3": "iPhone 13 Pro Max",
	"iPhone14,6": "iPhone SE (3rd generation)", "iPhone14,7": "iPhone 14", "iPhone14,8": "iPhone 14 Plus",
	"iPhone15,2": "iPhone 14 Pro", "iPhone15,3": "iPhone 14 Pro Max", "iPhone15,4": "iPhone 15", "iPhone15,5": "iPhone 15 Plus",
	"iPhone16,1": "iPhone 15 Pro", "iPhone16,2": "iPhone 15 Pro Max",
	"iPhone17,1": "iPhone 16 Pro", "iPhone17,2": "iPhone 16 Pro Max", "iPhone17,3": "iPhone 16", "iPhone17,4": "iPhone 16 Plus", "iPhone17,5": "iPhone 16e",
	"iPhone18,1": "iPhone 17 Pro", "iPhone18,2": "iPhone 17 Pro Max", "iPhone18,3": "iPhone 17", "iPhone18,4": "iPhone Air", "iPhone18,5": "iPhone 17e",
	"iPhone19,2": "iPhone 18 Pro", "iPhone19,3": "iPhone 18 Pro Max", "iPhone19,7": "iPhone 18 Pro Max", "iPhone19,4": "iPhone Duo",

	"iPad7,11": "iPad (7th generation)", "iPad7,12": "iPad (7th generation)",
	"iPad11,6": "iPad (8th generation)", "iPad11,7": "iPad (8th generation)",
	"iPad12,1": "iPad (9th generation)", "iPad12,2": "iPad (9th generation)",
	"iPad13,18": "iPad (10th generation)", "iPad13,19": "iPad (10th generation)",
	"iPad15,7": "iPad (A16)", "iPad15,8": "iPad (A16)",
	"iPad11,1": "iPad mini (5th generation)", "iPad11,2": "iPad mini (5th generation)",
	"iPad14,1": "iPad mini (6th generation)", "iPad14,2": "iPad mini (6th generation)",
	"iPad16,1": "iPad mini (A17 Pro)", "iPad16,2": "iPad mini (A17 Pro)",
	"iPad19,1": "iPad mini (A20 Pro)", "iPad19,2": "iPad mini (A20 Pro)",
	"iPad11,3": "iPad Air (3rd generation)", "iPad11,4": "iPad Air (3rd generation)",
	"iPad13,1": "iPad Air (4th generation)", "iPad13,2": "iPad Air (4th generation)",
	"iPad13,16": "iPad Air (5th generation)", "iPad13,17": "iPad Air (5th generation)",
	"iPad14,8": "iPad Air 11-inch (M2)", "iPad14,9": "iPad Air 11-inch (M2)",
	"iPad14,10": "iPad Air 13-inch (M2)", "iPad14,11": "iPad Air 13-inch (M2)",
	"iPad15,3": "iPad Air 11-inch (M3)", "iPad15,4": "iPad Air 11-inch (M3)",
	"iPad15,5": "iPad Air 13-inch (M3)", "iPad15,6": "iPad Air 13-inch (M3)",
	"iPad16,8": "iPad Air 11-inch (M4)", "iPad16,9": "iPad Air 11-inch (M4)",
	"iPad16,10": "iPad Air 13-inch (M4)", "iPad16,11": "iPad Air 13-inch (M4)",
	"iPad8,1": "iPad Pro 11-inch", "iPad8,2": "iPad Pro 11-inch", "iPad8,3": "iPad Pro 11-inch", "iPad8,4": "iPad Pro 11-inch",
	"iPad8,9": "iPad Pro 11-inch (2nd generation)", "iPad8,10": "iPad Pro 11-inch (2nd generation)",
	"iPad13,4": "iPad Pro 11-inch (3rd generation)", "iPad13,5": "iPad Pro 11-inch (3rd generation)",
	"iPad13,6": "iPad Pro 11-inch (3rd generation)", "iPad13,7": "iPad Pro 11-inch (3rd generation)",
	"iPad14,3": "iPad Pro 11-inch (4th generation)", "iPad14,4": "iPad Pro 11-inch (4th generation)",
	"iPad16,3": "iPad Pro 11-inch (M4)", "iPad16,4": "iPad Pro 11-inch (M4)",
	"iPad17,1": "iPad Pro 11-inch (M5)", "iPad17,2": "iPad Pro 11-inch (M5)",
	"iPad8,5": "iPad Pro 12.9-inch (3rd generation)", "iPad8,6": "iPad Pro 12.9-inch (3rd generation)",
	"iPad8,7": "iPad Pro 12.9-inch (3rd generation)", "iPad8,8": "iPad Pro 12.9-inch (3rd generation)",
	"iPad8,11": "iPad Pro 12.9-inch (4th generation)", "iPad8,12": "iPad Pro 12.9-inch (4th generation)",
	"iPad13,8": "iPad Pro 12.9-inch (5th generation)", "iPad13,9": "iPad Pro 12.9-inch (5th generation)",
	"iPad13,10": "iPad Pro 12.9-inch (5th generation)", "iPad13,11": "iPad Pro 12.9-inch (5th generation)",
	"iPad14,5": "iPad Pro 12.9-inch (6th generation)", "iPad14,6": "iPad Pro 12.9-inch (6th generation)",
	"iPad16,5": "iPad Pro 13-inch (M4)", "iPad16,6": "iPad Pro 13-inch (M4)",
	"iPad17,3": "iPad Pro 13-inch (M5)", "iPad17,4": "iPad Pro 13-inch (M5)",
}

var (
	_ driver.Provider          = (*Provider)(nil)
	_ driver.ConnectionChooser = (*Provider)(nil)
	_ driver.OpenChecker       = (*Provider)(nil)
)
