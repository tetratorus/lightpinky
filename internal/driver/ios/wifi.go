package ios

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/tetratorus/lightpinky/internal/driver"
	goios "github.com/danielpaulus/go-ios/ios"
)

// iPhones and iPads over Wi-Fi (driver.WiFiFinder). usbmuxd lists a phone
// over the network while it is on the same network as this computer and
// was paired with it with Wi-Fi allowed (Finder's "Show this iPhone when
// on Wi-Fi", Xcode's "Connect via network", or Pinky itself once the phone
// trusted it on a cable: wifiallow.go). Everything Pinky uses runs
// through that connection: lockdown, the tunnel, the screen stream, HID,
// the accessibility services and the developer disk image.
//
// usbmuxd lists every phone ever paired that way, though, a family
// member's iPad among them. So Pinky leaves a phone it reaches only over
// Wi-Fi alone until the gateway says it is the person's (UseOverWiFi):
// Pinky used it before, it was on a cable, or the person chose Use. Until
// then the phone is listed with driver.SetupConfirmWiFi, and Pinky reads
// its name, model and iOS version from lockdown, once per attach, and does
// nothing else: no Developer Mode switch, no disk image, no tunnel. A phone
// on a cable is the person's doing and is prepared as before. A phone the
// person removed or ignored (Forget) is not listed over Wi-Fi at all.

// wifiState is the provider's Wi-Fi side, guarded by Provider.mu.
type wifiState struct {
	on     bool
	use    map[string]bool       // phones the person uses (UseOverWiFi), by UDID
	forgot map[string]bool       // phones the person removed or ignored (Forget)
	found  map[string]foundPhone // what was read of phones found over Wi-Fi only
	asleep map[string]heldPhone  // phones the person uses, found locked on Wi-Fi

	// For phones without a passcode, which never read as locked (dozing):
	// the phones seen locked, when usbmuxd last stopped listing a phone
	// over Wi-Fi, and since when it lists it there this time.
	seenLocked map[string]bool
	dropped    map[string]time.Time
	since      map[string]time.Time

	// Wi-Fi connections Pinky turned on, or found on, for phones on a
	// cable (wifiallow.go), and the turning on that runs.
	allowed  map[string]wifiAllowed
	allowing sync.WaitGroup

	namesFn func(ctx context.Context, e goios.DeviceEntry) (phoneNames, error) // for tests: readNames
	allowFn func(ctx context.Context, e goios.DeviceEntry) (bool, error)       // for tests: allowWiFiConnections
}

// foundPhone is what was read of a phone found over Wi-Fi only: the attach
// it was read through, when, and when usbmuxd last listed it.
type foundPhone struct {
	key  entryKey
	info driver.Info
	at   time.Time
	seen time.Time
}

// heldPhone is a phone List keeps listing for a while after usbmuxd
// stopped listing it, and when usbmuxd last listed it.
type heldPhone struct {
	info driver.Info
	seen time.Time
}

// lockdownTimeout bounds one lockdown read. Over Wi-Fi usbmuxd keeps listing
// a phone for a while after it went to sleep or left the network, and go-ios
// waits for an answer without a deadline: without this, one such phone would
// hold up every List.
const lockdownTimeout = 8 * time.Second

// SetWiFi implements driver.WiFiFinder. A phone that was open over Wi-Fi is
// no longer listed once it is off, so the gateway closes it. On Linux and
// Windows it also starts or stops the search for phones on Wi-Fi
// (netmux.go).
func (p *Provider) SetWiFi(on bool) {
	p.mu.Lock()
	if p.wifi.on != on {
		p.logger().Info("iphone: using phones over Wi-Fi", "on", on)
	}
	p.wifi.on = on
	p.mu.Unlock()
	p.wifiMux(on)
}

// wifiOn reports whether Pinky uses phones over Wi-Fi.
func (p *Provider) wifiOn() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.wifi.on
}

// UseOverWiFi implements driver.WiFiFinder: the next List inspects the
// phone, which prepares it, also over Wi-Fi.
func (p *Provider) UseOverWiFi(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.wifi.use == nil {
		p.wifi.use = map[string]bool{}
	}
	if !p.wifi.use[id] {
		p.wifi.use[id] = true
		if _, found := p.wifi.found[id]; found {
			p.logger().Info("iphone: the person uses the phone found on Wi-Fi", "device", id)
		}
	}
	delete(p.wifi.found, id)
}

// usesOverWiFi reports whether Pinky may prepare and open the phone over
// Wi-Fi.
func (p *Provider) usesOverWiFi(id string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.wifi.use[id]
}

// Forget implements driver.Forgetter: the person removed the phone from
// Pinky, or ignored it on Wi-Fi. Pinky then leaves it alone over Wi-Fi; on
// a cable it is the person's again (the gateway calls Remember, then
// UseOverWiFi).
func (p *Provider) Forget(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.wifi.forgot == nil {
		p.wifi.forgot = map[string]bool{}
	}
	p.wifi.forgot[id] = true
	delete(p.wifi.use, id)
	delete(p.wifi.found, id)
	delete(p.known, id)
}

// Remember implements driver.Forgetter.
func (p *Provider) Remember(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.wifi.forgot, id)
}

// Disconnect implements driver.Forgetter. Pinky's own connections to a
// phone end when the gateway closes it, and usbmuxd's listing over the
// network is the system's, so there is nothing more to drop.
func (p *Provider) Disconnect(context.Context, string) error { return nil }

// listedOverWiFi reports whether List lists a phone usbmuxd has over the
// network: Pinky uses phones over Wi-Fi and the person did not remove it.
func (p *Provider) listedOverWiFi(id string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.wifi.on && !p.wifi.forgot[id]
}

// A phone the person uses that is locked on Wi-Fi is most likely asleep: an
// iPad on a desk then shows up on usbmuxd's list for 10 to 30 s about once a
// minute, as a new attach each time. Opening it there starts its screen
// stream, which lights its screen, every time (iPad Pro, iPadOS 27.0.1,
// 2026-11-08), and the session ends a few seconds later with the listing.
// So Pinky reads only lockdown (whether it is locked) while it is locked on
// Wi-Fi, lists it as waiting for the person to unlock it (needs_setup with
// driver.SetupUnlock), and opens it once it is unlocked; agents ask the
// person to unlock it. On a cable nothing changes.

// sleepHold is how long List keeps listing a phone found locked on Wi-Fi
// after usbmuxd stopped listing it, so one that sleeps reads as locked all
// along rather than coming and going.
const sleepHold = 3 * time.Minute

// lockedOnWiFi is a phone the person uses that is locked on Wi-Fi.
func (p *Provider) lockedOnWiFi(info driver.Info) driver.Info {
	info.State, info.Setup, info.Detail = driver.StateNeedsSetup, driver.SetupUnlock, lockedWiFiDetail
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.wifi.asleep == nil {
		p.wifi.asleep = map[string]heldPhone{}
	}
	if _, was := p.wifi.asleep[info.ID]; !was {
		p.logger().Info("iphone: locked on Wi-Fi; waiting until it is unlocked", "device", info.ID)
	}
	if p.wifi.seenLocked == nil {
		p.wifi.seenLocked = map[string]bool{}
	}
	p.wifi.seenLocked[info.ID] = true
	p.wifi.asleep[info.ID] = heldPhone{info: info, seen: p.clock()}
	return info
}

// stillAsleep returns what was listed of a phone found locked on Wi-Fi
// less than sleepHold ago, for a look at it that got no answer: one that
// sleeps answers lockdown only now and then.
func (p *Provider) stillAsleep(id string) (driver.Info, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	h, ok := p.wifi.asleep[id]
	if !ok || p.clock().Sub(h.seen) >= sleepHold {
		return driver.Info{}, false
	}
	return h.info, true
}

// A phone without a passcode never reads as locked, also while it sleeps,
// so the lock rule above does not hold it back, and an iPad without one
// would light its screen each time it shows up while it sleeps. Pinky
// knows a phone sleeps when usbmuxd dropped it from its Wi-Fi list a
// moment ago; it then opens it only once usbmuxd has listed it for
// awakeAfter in a row, which an awake phone is and a sleeping one rarely
// (10 to 30 s at a time, now and then 44 s). Until then it is not listed,
// so it stays offline. A phone seen locked has a passcode, and the lock
// rule covers it without the wait.
const (
	dozeWindow = 3 * time.Minute
	awakeAfter = 30 * time.Second
)

// noteLocked notes that a phone has a passcode: it was seen locked.
func (p *Provider) noteLocked(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.wifi.seenLocked == nil {
		p.wifi.seenLocked = map[string]bool{}
	}
	p.wifi.seenLocked[id] = true
}

// noteWiFiListing notes, after a List, since when usbmuxd lists each phone
// over Wi-Fi only, and when it stopped listing one there that was.
func (p *Provider) noteWiFiListing(entries []goios.DeviceEntry) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.wifi.since == nil {
		p.wifi.since = map[string]time.Time{}
		p.wifi.dropped = map[string]time.Time{}
	}
	now := p.clock()
	onWiFi := map[string]bool{}
	for _, e := range entries {
		if e.Properties.ConnectionType == "Network" {
			id := e.Properties.SerialNumber
			onWiFi[id] = true
			if _, ok := p.wifi.since[id]; !ok {
				p.wifi.since[id] = now
			}
		}
	}
	listed := map[string]bool{}
	for _, e := range entries {
		listed[e.Properties.SerialNumber] = true
	}
	for id := range p.wifi.since {
		if onWiFi[id] {
			continue
		}
		delete(p.wifi.since, id)
		if !listed[id] && p.wifi.on {
			p.wifi.dropped[id] = now // gone from the list, not onto a cable
		}
	}
	for id, t := range p.wifi.dropped {
		if now.Sub(t) >= dozeWindow {
			delete(p.wifi.dropped, id)
		}
	}
}

// dozing reports whether a phone the person uses, listed over Wi-Fi, most
// likely sleeps: it has no passcode that Pinky knows of, usbmuxd dropped
// it a moment ago, and lists it again for less than awakeAfter.
func (p *Provider) dozing(id string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.wifi.seenLocked[id] {
		return false
	}
	dropped, ok := p.wifi.dropped[id]
	if !ok || p.clock().Sub(dropped) >= dozeWindow {
		return false
	}
	since, ok := p.wifi.since[id]
	return ok && p.clock().Sub(since) < awakeAfter
}

// awake notes that a phone is not locked on Wi-Fi (any more).
func (p *Provider) awake(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.wifi.asleep, id)
}

// heldAsleep returns the phones found locked on Wi-Fi that usbmuxd stopped
// listing less than sleepHold ago and that are not among listed.
func (p *Provider) heldAsleep(listed map[string]bool) []driver.Info {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []driver.Info
	for id, h := range p.wifi.asleep {
		switch {
		case listed[id]:
		case !p.wifi.on || p.wifi.forgot[id] || !p.wifi.use[id] || p.clock().Sub(h.seen) >= sleepHold:
			delete(p.wifi.asleep, id)
		default:
			out = append(out, h.info)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Sentences for a phone over Wi-Fi.
const (
	lockedWiFiDetail = "Unlock the iPhone: it is on Wi-Fi, where Pinky connects to it only once it is unlocked, so as not to wake its screen."
	foundDetail      = "Found on Wi-Fi: this iPhone or iPad was paired with this computer before, in Finder or Xcode, and Pinky has not used it. Pinky leaves it alone until you choose Use in Pinky."
	trustWiFiDetail  = "This computer cannot read the iPhone over Wi-Fi: it no longer trusts this computer. Plug it in with a cable, unlock it and tap Trust; it asks only on a cable."
)

// foundReread is how long what was read of a phone found on Wi-Fi is kept.
// An iPad that sleeps drops off usbmuxd's list and comes back every minute
// or two, each time as a new attach; its name stays.
const foundReread = 10 * time.Minute

// foundHold is how long List keeps listing a phone found on Wi-Fi after
// usbmuxd stopped listing it, so that one that sleeps (an iPad on a desk
// shows up for 15 to 30 s every half minute to a minute and a half) does
// not come and go in Pinky's list.
const foundHold = 3 * time.Minute

// foundInfo is what List shows of a phone that usbmuxd lists only over
// Wi-Fi and that the person has not said is theirs: its name, model and iOS
// version, read from lockdown, and nothing else. ok is false when the phone
// does not answer: it is not listed then, since there is nothing to show,
// and is asked again after recheckOffline.
func (p *Provider) foundInfo(ctx context.Context, e goios.DeviceEntry) (driver.Info, bool) {
	id, key := e.Properties.SerialNumber, keyOf(e)
	p.mu.Lock()
	f, ok := p.wifi.found[id]
	p.mu.Unlock()
	if ok {
		age := p.clock().Sub(f.at)
		switch {
		case f.info.Name != "" && age < foundReread:
			p.sawFound(id)
			return f.info, true
		case f.info.Name == "" && f.key == key && age < recheckOffline:
			return driver.Info{}, false
		}
	}
	info := driver.Info{
		ID: id, Platform: driver.IOS, Connection: "wifi", Backend: backendCoreDevice,
		State: driver.StateNeedsSetup, Setup: driver.SetupConfirmWiFi, Detail: foundDetail,
	}
	read := p.wifi.namesFn
	if read == nil {
		read = readNames
	}
	names, err := read(ctx, e)
	if ctx.Err() != nil {
		return driver.Info{}, false // cut short; do not remember it
	}
	if err == nil {
		info.Name, info.Model, info.OSVersion = names.name, modelName(names.productType), names.version
		if info.Name == "" {
			info.Name = info.Model
		}
	}
	switch {
	case info.Name == "":
		// Nothing to show; asked again after recheckOffline.
		p.logger().Debug("iphone: a phone found on Wi-Fi does not answer", "device", id, "err", err)
	case !ok || f.info.Name == "":
		p.logger().Info("iphone: found a phone on Wi-Fi that Pinky has not used; waiting for the person", "device", id, "name", info.Name, "model", info.Model)
	}
	p.mu.Lock()
	if p.wifi.found == nil {
		p.wifi.found = map[string]foundPhone{}
	}
	if !p.wifi.use[id] {
		now := p.clock()
		p.wifi.found[id] = foundPhone{key: key, info: info, at: now, seen: now}
	}
	p.mu.Unlock()
	return info, info.Name != ""
}

// sawFound notes that usbmuxd lists a found phone now.
func (p *Provider) sawFound(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if f, ok := p.wifi.found[id]; ok {
		f.seen = p.clock()
		p.wifi.found[id] = f
	}
}

// heldFound returns the phones found on Wi-Fi that usbmuxd stopped
// listing less than foundHold ago and that are not among listed: List
// keeps them (foundHold).
func (p *Provider) heldFound(listed map[string]bool) []driver.Info {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []driver.Info
	if !p.wifi.on {
		return nil
	}
	for id, f := range p.wifi.found {
		if listed[id] || f.info.Name == "" || p.wifi.use[id] || p.wifi.forgot[id] || p.clock().Sub(f.seen) >= foundHold {
			continue
		}
		out = append(out, f.info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// phoneNames is what Pinky reads of a phone found on Wi-Fi.
type phoneNames struct{ name, productType, version string }

// readNames reads a phone's name, product type and iOS version from
// lockdown, in one session, and nothing else.
func readNames(ctx context.Context, e goios.DeviceEntry) (phoneNames, error) {
	return bounded(ctx, lockdownTimeout, func() (phoneNames, error) {
		conn, err := goios.ConnectLockdownWithSession(e)
		if err != nil {
			return phoneNames{}, err
		}
		defer conn.Close()
		var n phoneNames
		for _, v := range []struct {
			key string
			to  *string
		}{{"DeviceName", &n.name}, {"ProductType", &n.productType}, {"ProductVersion", &n.version}} {
			got, err := conn.GetValue(v.key)
			if err != nil {
				return phoneNames{}, err
			}
			*v.to, _ = got.(string)
		}
		return n, nil
	})
}

// getValues is goios.GetValues within lockdownTimeout.
func getValues(ctx context.Context, e goios.DeviceEntry) (goios.GetAllValuesResponse, error) {
	return bounded(ctx, lockdownTimeout, func() (goios.GetAllValuesResponse, error) { return goios.GetValues(e) })
}

// bounded runs f and returns its result, or an error once ctx ends or d
// passed. f keeps running then, and its result is dropped: go-ios's lockdown
// calls take no context.
func bounded[T any](ctx context.Context, d time.Duration, f func() (T, error)) (T, error) {
	type result struct {
		v   T
		err error
	}
	ch := make(chan result, 1)
	go func() {
		v, err := f()
		ch <- result{v, err}
	}()
	t := time.NewTimer(d)
	defer t.Stop()
	var zero T
	select {
	case r := <-ch:
		return r.v, r.err
	case <-ctx.Done():
		return zero, ctx.Err()
	case <-t.C:
		return zero, fmt.Errorf("no answer within %v", d)
	}
}

var _ driver.WiFiFinder = (*Provider)(nil)
var _ driver.Forgetter = (*Provider)(nil)
