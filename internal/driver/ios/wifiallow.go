package ios

import (
	"context"
	"errors"
	"time"

	"github.com/tetratorus/lightpinky/internal/driver"
	goios "github.com/danielpaulus/go-ios/ios"
)

// usbmuxd lists a phone over the network only once Wi-Fi connections are on
// for it: Finder's "Show this iPhone when on Wi-Fi", Xcode's "Connect via
// network", the phone's lockdown value EnableWifiConnections in the domain
// com.apple.mobile.wireless_lockdown. Nothing turns it on by itself, so a
// person who never ticked the box in Finder found Pinky's Wi-Fi switch did
// nothing. While Pinky uses phones over Wi-Fi, it turns the value on for a
// phone on a cable that trusts this computer, as Finder does when the box
// is ticked, and reads it first: a phone that has it on is left as it is.
// It never turns it off: turning Pinky's switch off, or removing the phone,
// leaves the phone's value alone. (Unticking Finder's box does not turn it
// off either: on 2026-11-09 the iPhone 13 mini read it as on afterwards,
// and usbmuxd listed the phone over Wi-Fi.) Once per attach;
// after a failure (a phone that did not answer in time) again after
// wifiAllowRetry.

const (
	wirelessDomain = "com.apple.mobile.wireless_lockdown"
	wirelessKey    = "EnableWifiConnections"
)

// wifiAllowRetry is how soon Pinky tries again to turn on Wi-Fi connections
// for a phone, on the same attach, after it failed.
const wifiAllowRetry = time.Minute

// wifiAllowed is what Pinky knows of one phone's Wi-Fi connections: what
// it found or did on the phone's last attach on a cable (its
// Info.WiFi.Connections), and whether it tries now. Each change is logged
// once, so that a person's log says what happened.
type wifiAllowed struct {
	key      entryKey // the attach it is about
	result   string   // a driver.WiFiConnections value
	detail   string
	running  bool
	failedAt time.Time // the last failed try
	logged   string    // result and detail as last logged
	seen     bool      // listed over the network at the last List
}

// allowWiFi turns on Wi-Fi connections, in the background, for the phones
// List found on a cable that trust this computer (their lockdown values
// read: infos[i] has a name), while Pinky uses phones over Wi-Fi, and
// notes why it does not for the others.
func (p *Provider) allowWiFi(entries []goios.DeviceEntry, infos []driver.Info, listed []bool) {
	for i, e := range entries {
		info := infos[i]
		if !listed[i] || e.Properties.ConnectionType != "USB" {
			continue
		}
		id, key := e.Properties.SerialNumber, keyOf(e)
		p.mu.Lock()
		a := p.wifi.allowed[id]
		if a.key != key {
			a = wifiAllowed{key: key, result: driver.WiFiConnectionsUnknown, logged: a.logged, seen: a.seen}
		}
		try := false
		switch {
		case !p.wifi.on:
			a.result, a.detail = driver.WiFiConnectionsOff, "Use iPhones and iPads over Wi-Fi is off"
		case p.wifi.forgot[id]:
			a.result, a.detail = driver.WiFiConnectionsUnknown, "the person removed the phone from Pinky"
		case info.State == driver.StateUnauthorized:
			a.result, a.detail = driver.WiFiConnectionsUnknown, "the phone does not trust this computer yet"
		case info.Name == "":
			a.result, a.detail = driver.WiFiConnectionsUnknown, "lockdown could not be read: "+nonEmptyDetail(info.Detail)
		case a.result == driver.WiFiConnectionsOn || a.result == driver.WiFiConnectionsTurnedOn || a.running:
		case a.result == driver.WiFiConnectionsFailed && p.clock().Sub(a.failedAt) < wifiAllowRetry:
		default:
			try, a.running = true, true
			p.wifi.allowing.Add(1)
		}
		p.noteAllowedLocked(id, info.Name, a)
		allow := p.wifi.allowFn
		p.mu.Unlock()
		if !try {
			continue
		}
		if allow == nil {
			allow = allowWiFiConnections
		}
		go func() {
			defer p.wifi.allowing.Done()
			turned, err := allow(context.Background(), e)
			p.mu.Lock()
			defer p.mu.Unlock()
			a := p.wifi.allowed[id]
			if a.key != key {
				return // a new attach meanwhile
			}
			a.running = false
			switch {
			case err != nil:
				a.result, a.detail, a.failedAt = driver.WiFiConnectionsFailed, err.Error(), p.clock()
			case turned:
				a.result, a.detail = driver.WiFiConnectionsTurnedOn, ""
			default:
				a.result, a.detail = driver.WiFiConnectionsOn, ""
			}
			p.noteAllowedLocked(id, info.Name, a)
		}()
	}
}

// noteAllowedLocked keeps a and logs what changed. The caller holds p.mu.
func (p *Provider) noteAllowedLocked(id, name string, a wifiAllowed) {
	if p.wifi.allowed == nil {
		p.wifi.allowed = map[string]wifiAllowed{}
	}
	if now := a.result + "|" + a.detail; !a.running && now != a.logged {
		a.logged = now
		log := p.logger()
		switch a.result {
		case driver.WiFiConnectionsOn:
			log.Info("iphone: Wi-Fi connections are on already", "device", id)
		case driver.WiFiConnectionsTurnedOn:
			log.Info("iphone: turned on Wi-Fi connections, as Finder's \"Show this iPhone when on Wi-Fi\" does", "device", id, "name", name)
		case driver.WiFiConnectionsFailed:
			log.Warn("iphone: could not turn on Wi-Fi connections", "device", id, "err", a.detail)
		default:
			log.Info("iphone: Wi-Fi connections not looked at", "device", id, "why", a.detail)
		}
	}
	p.wifi.allowed[id] = a
}

// wifiConnection is what List says of a phone's Wi-Fi connection
// (Info.WiFi): seen is whether usbmuxd lists it over the network now.
// A change of seen is logged.
func (p *Provider) wifiConnection(id string, seen bool) driver.WiFiConnection {
	p.mu.Lock()
	defer p.mu.Unlock()
	a, known := p.wifi.allowed[id]
	if known && a.seen != seen {
		if seen {
			p.logger().Info("iphone: listed over Wi-Fi", "device", id)
		} else {
			p.logger().Info("iphone: no longer listed over Wi-Fi", "device", id)
		}
	}
	if p.wifi.allowed == nil {
		p.wifi.allowed = map[string]wifiAllowed{}
	}
	if !known {
		a = wifiAllowed{result: driver.WiFiConnectionsUnknown, detail: "not plugged in since Pinky started"}
		if seen {
			p.logger().Info("iphone: listed over Wi-Fi", "device", id)
		}
	}
	a.seen = seen
	p.wifi.allowed[id] = a
	w := driver.WiFiConnection{Connections: a.result, Detail: a.detail, Seen: seen}
	if seen && w.Connections != driver.WiFiConnectionsTurnedOn {
		// Listed over the network: they are on, whoever turned them on.
		w.Connections, w.Detail = driver.WiFiConnectionsOn, ""
	}
	return w
}

// nonEmptyDetail is a phone's Detail, or "no answer".
func nonEmptyDetail(s string) string {
	if s == "" {
		return "no answer"
	}
	return s
}

// errWiFiNotAllowed means the phone took the value but reads it as off.
var errWiFiNotAllowed = errors.New("the phone still reads Wi-Fi connections as off")

// allowWiFiConnections turns on Wi-Fi connections for the phone behind e
// unless they are on, in one lockdown session, and reports whether it
// turned them on.
func allowWiFiConnections(ctx context.Context, e goios.DeviceEntry) (bool, error) {
	return bounded(ctx, lockdownTimeout, func() (bool, error) {
		conn, err := goios.ConnectLockdownWithSession(e)
		if err != nil {
			return false, err
		}
		defer conn.Close()
		v, err := conn.GetValueForDomain(wirelessKey, wirelessDomain)
		if err != nil {
			return false, err
		}
		if on, _ := v.(bool); on {
			return false, nil
		}
		if err := conn.SetValueForDomain(wirelessKey, wirelessDomain, true); err != nil {
			return false, err
		}
		v, err = conn.GetValueForDomain(wirelessKey, wirelessDomain)
		if err != nil {
			return false, err
		}
		if on, _ := v.(bool); !on {
			return false, errWiFiNotAllowed
		}
		return true, nil
	})
}
