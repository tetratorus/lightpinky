package ios

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tetratorus/lightpinky/internal/driver"
	"github.com/tetratorus/lightpinky/internal/mdns"
	goios "github.com/danielpaulus/go-ios/ios"
)

// Phones macOS's usbmuxd lost on Wi-Fi (driver.SetupDeviceService).
//
// usbmuxd lists a phone over the network when mDNSResponder reports the
// phone's _apple-mobdev2._tcp record as new. When its connection to a phone
// on Wi-Fi breaks once (its Wi-Fi turned off and on, a restart), it asks
// mDNSResponder to reconfirm the record, which fails on macOS 27.2 (-65541),
// and lets the phone go; it takes the phone back only when the record is new
// again, which for a phone that keeps it fresh is when it expires, up to about
// 75 minutes later (iphone-wifi-why, 2026-11-08). Meanwhile Finder and Xcode
// do not see the phone either, and `sudo killall usbmuxd` (launchd starts it
// again) brings it back at once.
//
// Pinky cannot get around it on its own: lockdown on the phone did not take a
// connection by the phone's address (port 62078 timed out, the record's port
// refused) while usbmuxd had lost it, and a second way to every service
// besides usbmuxd's would be a large and brittle part of its own. So it
// notices the state and lets the person restart the service: while it uses
// phones over Wi-Fi and one the person uses is missing from usbmuxd's list, it
// looks for _apple-mobdev2._tcp on the network itself (the package mdns, which
// asks and never answers), matches the instances' Wi-Fi addresses (the name is
// "<Wi-Fi MAC>@<link-local address>-supportsRP-26") with those in this
// computer's pair records, and lists a phone seen there for lostAfter in a row
// with SetupDeviceService. RestartDeviceService restarts usbmuxd through
// macOS's request for an administrator's password, only when the person asks.

const (
	// lostAfter is how long a missing phone must show up on the network
	// before Pinky says usbmuxd lost it: one that sleeps and wakes comes
	// back by itself within seconds.
	lostAfter = time.Minute
	// lostIdle is how long the search goes on once no phone is missing.
	lostIdle = 5 * time.Minute
	// pairRetry is how soon a pair record that could not be read is read
	// again.
	pairRetry = 10 * time.Minute
	// A search that has not seen any missing phone on the network for
	// lostQuiet pauses for lostPause, so a phone that is away all day
	// (taken out of the house) does not keep Pinky asking the network
	// every few seconds.
	lostQuiet = 10 * time.Minute
	lostPause = 5 * time.Minute

	// A phone whose cable was just pulled is not one that sleeps: usbmuxd
	// lists it over Wi-Fi about 2 s after the pull, when its Wi-Fi
	// connection is on. So for unplugRecent after a pull, one the network
	// shows that usbmuxd still does not list unplugGrace after it is lost
	// (an owner waited 75 s for Lost on Wi-Fi after unplugging, 2026-11-09),
	// and one that neither shows notSeenAfter after it is not seen on Wi-Fi
	// at all: another network, one that keeps devices apart, a VPN, or
	// asleep. Only for phones whose Wi-Fi connection Pinky knows is on.
	unplugGrace  = 8 * time.Second
	notSeenAfter = 15 * time.Second
	unplugRecent = 10 * time.Minute
	// notSeenSearch is how long the search must have run to say a phone
	// is not on the network: answers take a moment to come.
	notSeenSearch = 5 * time.Second
)

// mobdevService is the service type a phone advertises for Wi-Fi sync.
const mobdevService = "_apple-mobdev2._tcp"

// lostState is the provider's search for phones usbmuxd lost. Its zero value
// is ready.
type lostState struct {
	mu      sync.Mutex
	src     bonjourSource
	srcErr  error
	started time.Time            // when the search started
	onNet   time.Time            // when it last saw a missing phone on the network
	paused  time.Time            // no search until then
	needed  time.Time            // when a phone was last missing
	macs    map[string]pairMAC   // by UDID
	seen    map[string]time.Time // by UDID: on the network since, while missing from usbmuxd's list
	names   map[string]driver.Info
	reports map[string]bool      // logged as lost
	cabled  map[string]time.Time // by UDID: when usbmuxd last listed it on a cable
	netted  map[string]time.Time // by UDID: when usbmuxd last listed it over the network
	notSeen map[string]bool      // logged as not seen on Wi-Fi

	// For tests.
	browseFn func() (bonjourSource, error)
	pairFn   func(udid string) (string, error)
	anyOS    bool
}

// pairMAC is a phone's Wi-Fi address from its pair record, or when reading
// it failed.
type pairMAC struct {
	mac    string // lower case, "08:87:c7:2a:8a:db"
	failed time.Time
}

// bonjourSource is a search for DNS-SD services: an *mdns.Browser.
type bonjourSource interface {
	Services() []mdns.Service
	Err() error
	Close() error
}

// remember notes what a listed phone is called, for when usbmuxd loses it.
func (p *Provider) remember(infos []driver.Info) {
	p.lost.mu.Lock()
	defer p.lost.mu.Unlock()
	if p.lost.names == nil {
		p.lost.names = map[string]driver.Info{}
	}
	for _, info := range infos {
		if info.Name != "" {
			p.lost.names[info.ID] = driver.Info{Name: info.Name, Model: info.Model, OSVersion: info.OSVersion}
		}
	}
}

// lostPhones returns the phones the person uses that usbmuxd does not list
// though they have shown up on the network for lostAfter (unplugGrace after
// a pull), and those not seen on Wi-Fi at all a moment after a pull: listed
// are the UDIDs usbmuxd lists now, cabled those it lists on a cable, netted
// those it lists over the network. A phone usbmuxd listed over the network
// after its pull came through it, and may be asleep now: the slow rule. It
// starts and stops the search as needed.
func (p *Provider) lostPhones(listed, cabled, netted map[string]bool) []driver.Info {
	w := &p.lost
	if runtime.GOOS != "darwin" && !w.anyOS {
		return nil // the macOS usbmuxd's bug; others not seen
	}
	p.mu.Lock()
	on := p.wifi.on
	var missing []string
	connOn := map[string]bool{} // Pinky knows its Wi-Fi connection is on (wifiallow.go)
	for id := range p.wifi.use {
		if !listed[id] && !p.wifi.forgot[id] {
			missing = append(missing, id)
			r := p.wifi.allowed[id].result
			connOn[id] = r == driver.WiFiConnectionsOn || r == driver.WiFiConnectionsTurnedOn
		}
	}
	now := p.clock()
	p.mu.Unlock()
	w.mu.Lock()
	if w.cabled == nil {
		w.cabled, w.netted = map[string]time.Time{}, map[string]time.Time{}
	}
	for id := range cabled {
		w.cabled[id] = now
	}
	for id := range netted {
		w.netted[id] = now
	}
	w.mu.Unlock()
	if !on {
		w.mu.Lock()
		w.stopLocked()
		w.seen = map[string]time.Time{}
		w.mu.Unlock()
		return nil
	}
	sort.Strings(missing)

	// The Wi-Fi addresses of the missing phones, from this computer's pair
	// records (read through usbmuxd; Android phones have none).
	w.mu.Lock()
	if w.macs == nil {
		w.macs = map[string]pairMAC{}
		w.seen = map[string]time.Time{}
		w.reports = map[string]bool{}
	}
	if w.notSeen == nil {
		w.notSeen = map[string]bool{}
	}
	var toRead []string
	for _, id := range missing {
		m, ok := w.macs[id]
		if !ok || (m.mac == "" && now.Sub(m.failed) >= pairRetry) {
			toRead = append(toRead, id)
		}
	}
	read := w.pairFn
	w.mu.Unlock()
	if read == nil {
		read = pairWiFiMAC
	}
	for _, id := range toRead {
		mac, err := read(id)
		w.mu.Lock()
		if err != nil || mac == "" {
			w.macs[id] = pairMAC{failed: now}
		} else {
			w.macs[id] = pairMAC{mac: strings.ToLower(mac)}
		}
		w.mu.Unlock()
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	var wanted []string
	for _, id := range missing {
		if w.macs[id].mac != "" {
			wanted = append(wanted, id)
		}
	}
	if on && len(wanted) > 0 {
		w.needed = now
	}
	if !on || now.Sub(w.needed) >= lostIdle || now.Before(w.paused) {
		w.stopLocked()
		w.seen = map[string]time.Time{}
		return nil
	}
	if w.src != nil && now.Sub(w.started) >= lostQuiet && now.Sub(w.onNet) >= lostQuiet {
		// None of them came by for a while: away, not lost.
		w.stopLocked()
		w.seen = map[string]time.Time{}
		w.paused = now.Add(lostPause)
		return nil
	}
	if w.src == nil {
		browse := w.browseFn
		if browse == nil {
			browse = func() (bonjourSource, error) { return mdns.Browse([]string{mobdevService}, mdns.Options{}) }
		}
		w.src, w.srcErr = browse()
		w.started = now
		if w.srcErr != nil {
			p.logger().Warn("iphone: cannot look for phones on the network", "err", w.srcErr)
			return nil
		}
		p.logger().Info("iphone: looking on the network for phones usbmuxd does not list", "phones", len(wanted))
	}
	onNet := map[string]bool{}
	for _, s := range w.src.Services() {
		if mac, _, ok := strings.Cut(s.Instance, "@"); ok {
			onNet[strings.ToLower(mac)] = true
		}
	}
	var out []driver.Info
	for _, id := range missing {
		mac := w.macs[id].mac
		pulled := w.cabled[id]
		recent := connOn[id] && !pulled.IsZero() && now.Sub(pulled) < unplugRecent && !w.netted[id].After(pulled)
		if mac == "" || !onNet[mac] {
			delete(w.seen, id)
			delete(w.reports, id)
			if mac != "" && recent && now.Sub(pulled) >= notSeenAfter && now.Sub(w.started) >= notSeenSearch {
				if !w.notSeen[id] {
					w.notSeen[id] = true
					p.logger().Warn("iphone: not seen on Wi-Fi since its cable was pulled: usbmuxd does not list it, and the network does not show it", "device", id, "wifi_mac", mac, "pulled", pulled)
				}
				info := w.names[id]
				info.ID, info.Platform, info.Connection, info.Backend = id, driver.IOS, "wifi", backendCoreDevice
				info.State, info.Setup, info.Detail = driver.StateOffline, driver.SetupNotOnWiFi, notSeenDetail
				out = append(out, info)
			}
			continue
		}
		delete(w.notSeen, id)
		w.onNet = now
		since, ok := w.seen[id]
		if !ok {
			w.seen[id], since = now, now
		}
		wait := lostAfter
		if recent {
			// Just unplugged: usbmuxd lists a phone over Wi-Fi within
			// seconds, or has lost it.
			if now.Sub(pulled) < unplugGrace {
				continue
			}
			wait = 0
		}
		if now.Sub(since) < wait || (!recent && !ok) {
			continue
		}
		if !w.reports[id] {
			w.reports[id] = true
			p.logger().Warn("iphone: usbmuxd lost the phone on Wi-Fi; it is on the network", "device", id, "wifi_mac", mac, "since", since)
		}
		info := w.names[id]
		info.ID, info.Platform, info.Connection, info.Backend = id, driver.IOS, "wifi", backendCoreDevice
		info.State, info.Setup, info.Detail = driver.StateNeedsSetup, driver.SetupDeviceService, lostDetail
		out = append(out, info)
	}
	for id := range w.seen {
		if listed[id] {
			delete(w.seen, id)
			delete(w.reports, id)
		}
	}
	for id := range w.notSeen {
		if listed[id] {
			delete(w.notSeen, id)
		}
	}
	return out
}

// stopLocked ends the search. The caller holds w.mu.
func (w *lostState) stopLocked() {
	if w.src != nil {
		w.src.Close()
		w.src = nil
	}
}

// notSeenDetail is the Detail of a phone not seen on Wi-Fi since its cable
// was pulled, which the gateway's status line shows.
const notSeenDetail = "Not seen on Wi-Fi"

// lostDetail says what is wrong with a phone usbmuxd lost.
const lostDetail = "This Mac's device service (usbmuxd) lost the iPhone on Wi-Fi: the iPhone is on the network, but the service no longer lists it, and may not for up to an hour. Restarting the service brings it back."

// pairWiFiMAC reads the phone's Wi-Fi address from this computer's pair
// record, through usbmuxd. It does not contact the phone.
func pairWiFiMAC(udid string) (string, error) {
	r, err := goios.ReadPairRecord(udid)
	if err != nil {
		return "", err
	}
	return r.WiFiMACAddress, nil
}

// RestartDeviceService implements driver.DeviceServiceRestarter on macOS:
// it quits usbmuxd, which launchd starts again, through macOS's request for
// an administrator's password (osascript's "with administrator
// privileges"), with reason in it. Every iPhone and iPad drops for a second
// or two, on a cable too.
func (p *Provider) RestartDeviceService(ctx context.Context, reason string) error {
	if runtime.GOOS != "darwin" && p.restartFn == nil {
		return fmt.Errorf("restarting the device service: %w", driver.ErrUnsupported)
	}
	run := p.restartFn
	if run == nil {
		run = runAsAdmin
	}
	p.logger().Info("iphone: restarting usbmuxd at the person's request")
	if err := run(ctx, "/usr/bin/killall usbmuxd", reason); err != nil {
		return err
	}
	p.mu.Lock()
	for id := range p.known {
		delete(p.known, id) // every phone reattaches
	}
	p.mu.Unlock()
	p.lost.mu.Lock()
	p.lost.seen = map[string]time.Time{}
	p.lost.mu.Unlock()
	return nil
}

// runAsAdmin runs a shell command as root through macOS's request for an
// administrator's password, showing reason in it.
func runAsAdmin(ctx context.Context, command, reason string) error {
	script := fmt.Sprintf("do shell script %s with administrator privileges with prompt %s", appleScriptString(command), appleScriptString(reason))
	out, err := exec.CommandContext(ctx, "/usr/bin/osascript", "-e", script).CombinedOutput()
	if err == nil {
		return nil
	}
	if strings.Contains(string(out), "-128") || strings.Contains(strings.ToLower(string(out)), "user canceled") {
		return fmt.Errorf("restarting the device service: %w", driver.ErrCanceled)
	}
	return fmt.Errorf("restarting the device service: %v: %s", err, strings.TrimSpace(string(out)))
}

// appleScriptString quotes s as an AppleScript string literal.
func appleScriptString(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

var _ driver.DeviceServiceRestarter = (*Provider)(nil)
