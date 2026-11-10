package netmux

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"time"

	goios "github.com/danielpaulus/go-ios/ios"
	"howett.net/plist"
)

// The lockdown heartbeat. A usbmuxd that lists a phone on the network keeps
// a connection to the phone's com.apple.mobile.heartbeat service, and
// answers each "Marco" the phone sends with "Polo"; macOS's usbmuxd, Apple's
// on Windows and netmuxd all do. Without it the phone still answers
// lockdown and some services (syslog_relay), but closes AMFI, the image
// mounter and CoreDeviceProxy right after the TLS handshake, so Pinky can
// neither check Developer Mode nor start the tunnel (iPhone 18 Pro Max, iOS
// 27.2, on Windows, 2026-11-09: with the heartbeat answered, the same
// requests worked at once). So the Mux keeps one for every phone it lists,
// for as long as it lists it at that address.

// heartbeatRetry is the longest wait before a heartbeat that ended or could
// not start is started again; it doubles from a second while it keeps
// failing.
const heartbeatRetry = 30 * time.Second

// heartbeatService is the phone's heartbeat service.
const heartbeatService = "com.apple.mobile.heartbeat"

const (
	// heartbeatConnect bounds starting the heartbeat service on a phone,
	// which go-ios does without a context or deadline.
	heartbeatConnect = 20 * time.Second
	// heartbeatQuiet is how long a heartbeat may stay silent before the
	// phone's first Marco says how often, in seconds, it sends one; from
	// then on, three of its intervals.
	heartbeatQuiet = time.Minute
)

// heartbeatConn is a connection to a phone's heartbeat service.
type heartbeatConn interface {
	Reader() io.Reader
	Writer() io.Writer
	Close() error
}

// connectHeartbeat starts the heartbeat service on the phone listed with
// DeviceID id, through the usbmuxd go-ios uses (this Mux, once
// USBMUXD_SOCKET_ADDRESS points at it).
func connectHeartbeat(p Phone, id int) (heartbeatConn, error) {
	e := goios.DeviceEntry{DeviceID: id, Properties: goios.DeviceProperties{SerialNumber: p.UDID, ConnectionType: "Network"}}
	c, err := goios.ConnectToService(e, heartbeatService)
	if err != nil {
		return nil, err
	}
	return c, nil
}

// heartbeat keeps the heartbeat of the phone listed with DeviceID id going
// until ctx ends.
func (m *Mux) heartbeat(ctx context.Context, p Phone, id int) {
	connect := m.HeartbeatFn
	if connect == nil {
		connect = connectHeartbeat
	}
	wait := time.Second
	for ctx.Err() == nil {
		if m.upstreamListsOnNetwork(p.UDID) {
			// The system's usbmuxd lists it on the network itself (Apple's
			// on Windows while it is on a cable too) and keeps its
			// heartbeat: a second one is not needed.
			select {
			case <-ctx.Done():
				return
			case <-time.After(heartbeatRetry):
			}
			continue
		}
		c, err := connectWithin(ctx, heartbeatConnect, func() (heartbeatConn, error) { return connect(p, id) })
		if err == nil {
			start := time.Now()
			stop := context.AfterFunc(ctx, func() { c.Close() })
			err = answerHeartbeat(c, heartbeatQuiet, time.Second)
			stop()
			c.Close()
			if time.Since(start) > heartbeatRetry {
				wait = time.Second // it ran for a while: start again soon
			}
		}
		if ctx.Err() != nil {
			return
		}
		m.log.Debug("netmux: the phone's heartbeat ended; starting it again", "device", p.UDID, "err", err, "in", wait)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = min(2*wait, heartbeatRetry)
	}
}

// connectWithin runs connect, which takes no context, and gives up on it
// when ctx ends or after limit; a connection that comes later is closed.
func connectWithin(ctx context.Context, limit time.Duration, connect func() (heartbeatConn, error)) (heartbeatConn, error) {
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	type result struct {
		c   heartbeatConn
		err error
	}
	ch := make(chan result, 1)
	go func() {
		c, err := connect()
		ch <- result{c, err}
	}()
	select {
	case r := <-ch:
		return r.c, r.err
	case <-ctx.Done():
		go func() {
			if r := <-ch; r.c != nil {
				r.c.Close()
			}
		}()
		return nil, fmt.Errorf("starting the heartbeat: %w", ctx.Err())
	}
}

// deadliner is a heartbeat connection whose reads can time out, as
// go-ios's connections are.
type deadliner interface{ Conn() net.Conn }

// answerHeartbeat answers the phone's "Marco"s with "Polo" until the
// connection ends, or stays silent for longer than quiet, then three of
// the intervals the phone gives (in units of unit: a second).
func answerHeartbeat(c heartbeatConn, quiet, unit time.Duration) error {
	polo, err := plist.Marshal(map[string]any{"Command": "Polo"}, plist.XMLFormat)
	if err != nil {
		return err
	}
	msg := binary.BigEndian.AppendUint32(nil, uint32(len(polo)))
	msg = append(msg, polo...)
	for {
		if d, ok := c.(deadliner); ok {
			if nc := d.Conn(); nc != nil {
				_ = nc.SetReadDeadline(time.Now().Add(quiet))
			}
		}
		var n uint32
		if err := binary.Read(c.Reader(), binary.BigEndian, &n); err != nil {
			return err
		}
		if n > 1<<20 {
			return fmt.Errorf("heartbeat message of %d bytes", n)
		}
		body := make([]byte, n)
		if _, err := io.ReadFull(c.Reader(), body); err != nil {
			return err
		}
		var m struct {
			Command  string
			Interval any // seconds
		}
		if _, err := plist.Unmarshal(body, &m); err != nil {
			return err
		}
		if s := seconds(m.Interval); s > 0 {
			quiet = 3 * time.Duration(s*float64(unit))
		}
		if m.Command != "Marco" {
			continue // "SleepyTime" and the like: nothing to answer
		}
		if _, err := c.Writer().Write(msg); err != nil {
			return err
		}
	}
}

// seconds reads a plist number, 0 for anything else.
func seconds(v any) float64 {
	switch n := v.(type) {
	case uint64:
		return float64(n)
	case int64:
		return float64(n)
	case float64:
		return n
	}
	return 0
}
