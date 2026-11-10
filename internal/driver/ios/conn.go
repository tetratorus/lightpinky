package ios

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	goios "github.com/danielpaulus/go-ios/ios"
)

// link is a tunnel plus the device's service list: everything needed to open
// service connections to one device.
type link struct {
	udid   string
	base   goios.DeviceEntry // the usbmuxd entry
	tunnel *Tunnel
	rsd    goios.RsdHandshakeResponse
	entry  goios.DeviceEntry // routes go-ios connections through the tunnel

	rsdConn  io.Closer // the RSD connection the host was announced on, open for rsdLinger
	rsdClose sync.Once
}

// dialLink opens a tunnel to the device and reads its service list. The
// first RSD handshake on a tunnel also announces the host: without it the
// device treats us as a legacy peer and its services crash when they send
// large replies such as screenshots. iOS 27.2 accepts that announcement only
// once per tunnel, so later handshakes must not repeat it.
func dialLink(ctx context.Context, udid string, wifi bool) (*link, error) {
	base, err := findEntry(udid, wifi)
	if err != nil {
		return nil, err
	}
	return dialLinkEntry(ctx, base)
}

// dialLinkEntry opens a tunnel through one usbmuxd entry.
func dialLinkEntry(ctx context.Context, base goios.DeviceEntry) (*link, error) {
	return dialLinkEntryMTU(ctx, base, requestedMTU)
}

// dialLinkEntryMTU is dialLinkEntry asking the device for an MTU of mtu.
func dialLinkEntryMTU(ctx context.Context, base goios.DeviceEntry, mtu int) (*link, error) {
	udid := base.Properties.SerialNumber
	t, err := openTunnel(ctx, base, mtu)
	if err != nil {
		return nil, err
	}
	l := &link{udid: udid, base: base, tunnel: t}
	l.entry = t.Entry(base, nil)
	rsd, conn, err := l.handshake(ctx, true)
	if err != nil {
		t.Close()
		return nil, err
	}
	l.rsd = rsd
	l.entry = t.Entry(base, rsd)
	l.rsdConn = conn
	time.AfterFunc(rsdLinger, l.closeRSD)
	return l, nil
}

// rsdLinger is how long the RSD connection the host was announced on stays
// open. The phone takes the announcement only once it has also settled TLS
// on that connection, which can take longer than the host takes to send
// it: closed at once, the connection sometimes ended first, and the phone
// logged "Device must renegotiate TLS" and kept the tunnel as a legacy
// peer's ("Invalid or missing remote device connection version flags").
// On such a tunnel cryptexd never read the developer disk image Pinky sent
// (iOS 27.0.1, 2026-11-08). Once taken, the announcement outlasts the
// connection.
var rsdLinger = 2 * time.Second

// closeRSD closes the RSD connection the host was announced on, once.
func (l *link) closeRSD() {
	l.rsdClose.Do(func() {
		if l.rsdConn != nil {
			l.rsdConn.Close()
		}
	})
}

// findEntry returns the usbmuxd entry for a device. A phone that is on USB
// and Wi-Fi at once is listed twice; USB wins unless wifi asks for the
// network entry, which then must exist.
func findEntry(udid string, wifi bool) (goios.DeviceEntry, error) {
	list, err := goios.ListDevices()
	if err != nil {
		return goios.DeviceEntry{}, fmt.Errorf("usbmuxd: %w", err)
	}
	return pickEntry(list, udid, wifi)
}

// entryFor is findEntry(id, false) through the provider's usbmuxd list,
// which tests replace.
func (p *Provider) entryFor(id string) (goios.DeviceEntry, error) {
	if p.listFn == nil {
		return findEntry(id, false)
	}
	list, err := p.listFn()
	if err != nil {
		return goios.DeviceEntry{}, fmt.Errorf("usbmuxd: %w", err)
	}
	return pickEntry(list, id, false)
}

// pickEntry is findEntry's choice in a list usbmuxd gave.
func pickEntry(list goios.DeviceList, udid string, wifi bool) (goios.DeviceEntry, error) {
	var found *goios.DeviceEntry
	for i, e := range list.DeviceList {
		if e.Properties.SerialNumber != udid {
			continue
		}
		network := e.Properties.ConnectionType == "Network"
		if wifi && !network {
			continue
		}
		if found == nil || !network {
			found = &list.DeviceList[i]
		}
	}
	if found == nil {
		if wifi {
			return goios.DeviceEntry{}, fmt.Errorf("iPhone %s is not reachable over Wi-Fi", udid)
		}
		return goios.DeviceEntry{}, fmt.Errorf("iPhone %s is not connected", udid)
	}
	return *found, nil
}

// handshake reads the device's service list on a new RSD connection, as
// the host when asHost. It returns that connection open: the caller closes
// it (see rsdLinger).
func (l *link) handshake(ctx context.Context, asHost bool) (goios.RsdHandshakeResponse, io.Closer, error) {
	type result struct {
		h    goios.RsdHandshakeResponse
		conn io.Closer
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		s, err := goios.NewWithAddrPortDevice(l.entry.Address, l.tunnel.RSDPort(), l.entry)
		if err != nil {
			ch <- result{err: err}
			return
		}
		var h goios.RsdHandshakeResponse
		if asHost {
			h, err = s.HandshakeAsHost()
		} else {
			h, err = s.Handshake()
		}
		if err != nil {
			s.Close()
			ch <- result{err: err}
			return
		}
		ch <- result{h, s, nil}
	}()
	// A handshake given up on closes its connection when it ends.
	abandon := func() {
		go func() {
			if r := <-ch; r.conn != nil {
				r.conn.Close()
			}
		}()
	}
	select {
	case r := <-ch:
		if r.err != nil {
			return r.h, nil, fmt.Errorf("RSD handshake: %w", r.err)
		}
		return r.h, r.conn, nil
	case <-ctx.Done():
		abandon()
		return goios.RsdHandshakeResponse{}, nil, fmt.Errorf("RSD handshake: %w", ctx.Err())
	case <-time.After(15 * time.Second):
		abandon()
		return goios.RsdHandshakeResponse{}, nil, fmt.Errorf("RSD handshake: timed out")
	}
}

// has reports whether the device offers a service.
func (l *link) has(service string) bool { return l.rsd.GetPort(service) != 0 }

func (l *link) Close() error {
	l.closeRSD()
	return l.tunnel.Close()
}
