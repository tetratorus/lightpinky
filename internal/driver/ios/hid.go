package ios

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"time"

	goios "github.com/danielpaulus/go-ios/ios"
	"github.com/danielpaulus/go-ios/ios/xpc"
)

// HID over CoreDevice. Wire formats were worked out by go-ios (ios/hid, MIT)
// and pymobiledevice3 (as a protocol reference only; no code is taken from
// it). The device applies input only while a media stream from this host is
// running: without one it accepts every report and drops it without an error.
// It never acknowledges a report either, so a nil error means "sent".

const (
	universalHIDService = "com.apple.coredevice.hid.universalhidservice"
	indigoHIDService    = "com.apple.coredevice.hid.indigo"

	featureUniversalHID = "com.apple.coredevice.feature.remote.universalhidservice"
	featureHIDButton    = "com.apple.coredevice.feature.remote.hid.button"

	// surfaceTouchscreen is the service id of the built-in touchscreen.
	surfaceTouchscreen = uint64(257)
	// surfaceKeyboard is the id we ask for when registering a virtual
	// keyboard. The high bit marks a host-provided, session-specific surface.
	surfaceKeyboard = uint64(0x100002001)

	touchReportLen    = 58
	keyboardReportLen = 39

	// touchIdentity is the contact identity Xcode uses for one finger.
	touchIdentity = 2
	touchContact  = 0xC0 | touchIdentity // touching and in range
	touchRelease  = touchIdentity

	buttonDown = 1
	buttonUp   = 2
)

var hidEpoch = time.Now()

// hidTimestamp stamps a report in the phone's mach time units: 24 MHz ticks
// on every iPhone that runs iOS 27. Nanoseconds would make every interval
// look 41.7 times longer: a 12 ms key press reads as half a second and opens
// the press-and-hold accent menu. Only deltas matter, so the origin is the
// first report. The field is 48 bits wide.
func hidTimestamp() uint64 {
	ns := uint64(time.Since(hidEpoch).Nanoseconds())
	return (ns / 125 * 3) & (1<<48 - 1)
}

func put48(b []byte, v uint64) {
	var full [8]byte
	binary.LittleEndian.PutUint64(full[:], v)
	copy(b, full[:6])
}

// touchReport is a 58-byte main-touchscreen report with one contact.
// Layout: report id 0x09, contact count, max contacts (5), then five 5-byte
// contact slots [state, x u16le, y u16le], 12 reserved bytes, the constant
// 02 00 00 00, a 48-bit timestamp and 8 reserved bytes. Positions are
// 0..65535 on each axis of the panel, independent of resolution.
func touchReport(state byte, x, y uint16, ts uint64) []byte {
	r := make([]byte, touchReportLen)
	r[0] = 0x09
	r[1] = 1
	r[2] = 5
	r[3] = state
	binary.LittleEndian.PutUint16(r[4:6], x)
	binary.LittleEndian.PutUint16(r[6:8], y)
	r[40] = 0x02
	put48(r[44:50], ts)
	return r
}

// keyboardReport is a 39-byte virtual keyboard report: report id 0x01, a
// 240-bit bitmap of the HID keyboard usages (page 7) held down, a 48-bit
// timestamp and two reserved bytes. Every report carries the full set of
// pressed keys.
func keyboardReport(usages []uint8, ts uint64) []byte {
	r := make([]byte, keyboardReportLen)
	r[0] = 0x01
	for _, u := range usages {
		if u < 240 {
			r[1+u/8] |= 1 << (u % 8)
		}
	}
	put48(r[31:37], ts)
	return r
}

// keyboardDescriptor declares a boot keyboard. The reports we send do not
// follow its layout; dtuhidd accepts that, as it does for macOS Universal
// Control, and the descriptor only makes the surface count as a keyboard.
var keyboardDescriptor = []byte{
	0x05, 0x01, 0x09, 0x06, 0xA1, 0x01, // Generic Desktop, Keyboard, Application
	0x05, 0x07, 0x19, 0xE0, 0x29, 0xE7, 0x15, 0x00, 0x25, 0x01, 0x95, 0x08, 0x75, 0x01, 0x81, 0x02, // modifiers
	0x95, 0x01, 0x75, 0x08, 0x81, 0x01, // reserved byte
	0x05, 0x07, 0x19, 0x00, 0x29, 0xFF, 0x15, 0x00, 0x26, 0xFF, 0x00, 0x95, 0x06, 0x75, 0x08, 0x81, 0x00, // 6 keys
	0x05, 0x08, 0x19, 0x01, 0x29, 0x05, 0x15, 0x00, 0x25, 0x01, 0x95, 0x05, 0x75, 0x01, 0x91, 0x02, // LEDs
	0x95, 0x01, 0x75, 0x03, 0x91, 0x01, 0xC0,
}

// xpcLink is one RemoteXPC connection whose replies are drained in the
// background, so that unread messages never stall the HTTP/2 stream.
type xpcLink struct {
	name    string
	mu      sync.Mutex
	conn    *xpc.Connection
	replies chan map[string]any
	done    chan struct{}
	errMu   sync.Mutex
	err     error
}

func dialXPC(entry goios.DeviceEntry, name string) (*xpcLink, error) {
	conn, err := dialXPCConn(entry, name)
	if err != nil {
		return nil, err
	}
	x := &xpcLink{name: name, conn: conn, replies: make(chan map[string]any, 16), done: make(chan struct{})}
	go x.read()
	return x, nil
}

func (x *xpcLink) read() {
	defer close(x.done)
	for {
		m, err := x.conn.ReceiveOnServerClientStream()
		if err != nil {
			x.errMu.Lock()
			x.err = err
			x.errMu.Unlock()
			return
		}
		select {
		case x.replies <- m:
		default: // nobody waits for it
		}
	}
}

func (x *xpcLink) send(m map[string]any, wantReply bool) error {
	x.mu.Lock()
	defer x.mu.Unlock()
	select {
	case <-x.done:
		x.errMu.Lock()
		defer x.errMu.Unlock()
		return fmt.Errorf("%s: connection closed: %v", x.name, x.err)
	default:
	}
	var err error
	if wantReply {
		err = x.conn.Send(m, xpc.HeartbeatRequestFlag) // the "wants reply" flag
	} else {
		err = x.conn.Send(m)
	}
	if err != nil {
		return fmt.Errorf("%s: %w", x.name, err)
	}
	return nil
}

// request sends m and waits for the next reply.
func (x *xpcLink) request(ctx context.Context, m map[string]any) (map[string]any, error) {
	for {
		select {
		case <-x.replies: // a stale reply from an earlier message
			continue
		default:
		}
		break
	}
	if err := x.send(m, true); err != nil {
		return nil, err
	}
	select {
	case r := <-x.replies:
		return r, nil
	case <-x.done:
		return nil, fmt.Errorf("%s: connection closed", x.name)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (x *xpcLink) Close() error { return x.conn.Close() }

// alive reports whether the connection still reads.
func (x *xpcLink) alive() bool {
	select {
	case <-x.done:
		return false
	default:
		return true
	}
}

// hidSession owns the HID connections of one device.
type hidSession struct {
	entry goios.DeviceEntry

	mu       sync.Mutex
	touch    *xpcLink
	buttons  *xpcLink
	keyboard uint64 // registered keyboard service id, 0 if none
	down     bool
	last     [2]uint16
	held     *[2]uint64 // a button held down: usage page and usage
}

// alive reports whether the touch connection still works.
func (h *hidSession) alive() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.touch != nil && h.touch.alive()
}

// liftHeld lifts a contact that is still down.
func (h *hidSession) liftHeld() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.down {
		return nil
	}
	return h.touchLocked(false, h.last[0], h.last[1])
}

func newHIDSession(entry goios.DeviceEntry) (*hidSession, error) {
	t, err := dialXPC(entry, universalHIDService)
	if err != nil {
		return nil, err
	}
	return &hidSession{entry: entry, touch: t}, nil
}

func universalReport(service uint64, report []byte) map[string]any {
	return map[string]any{
		"featureIdentifier": featureUniversalHID,
		"messageType":       "Request",
		"payload": map[string]any{
			"send": map[string]any{"_0": report, "_1": service},
		},
	}
}

// touchAt reports one contact (down) or its release (up) at panel position x, y.
func (h *hidSession) touchAt(down bool, x, y uint16) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.touchLocked(down, x, y)
}

func (h *hidSession) touchLocked(down bool, x, y uint16) error {
	state := byte(touchContact)
	if !down {
		if !h.down {
			return nil
		}
		state = touchRelease
	}
	if err := h.touch.send(universalReport(surfaceTouchscreen, touchReport(state, x, y, hidTimestamp())), false); err != nil {
		return err
	}
	h.down = down
	h.last = [2]uint16{x, y}
	return nil
}

// button presses a hardware button by HID usage and holds it for hold.
func (h *hidSession) button(ctx context.Context, page, usage uint64, hold time.Duration) error {
	h.mu.Lock()
	if h.buttons != nil && !h.buttons.alive() {
		h.buttons.Close()
		h.buttons = nil
	}
	if h.buttons == nil {
		b, err := dialXPC(h.entry, indigoHIDService)
		if err != nil {
			h.mu.Unlock()
			return err
		}
		h.buttons = b
	}
	b := h.buttons
	if err := b.send(buttonEvent(page, usage, buttonDown), false); err != nil {
		h.mu.Unlock()
		return err
	}
	h.held = &[2]uint64{page, usage}
	h.mu.Unlock()
	t := time.NewTimer(hold)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
	// The release goes out even when ctx ended: a held button must come up.
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.held == nil {
		return nil // Close released it
	}
	h.held = nil
	return b.send(buttonEvent(page, usage, buttonUp), false)
}

func buttonEvent(page, usage, state uint64) map[string]any {
	return map[string]any{
		"messageType":       "IndigoButtonEvent",
		"featureIdentifier": featureHIDButton,
		"payload": map[string]any{
			"state":     state,
			"usagePage": page,
			"usageCode": usage,
		},
	}
}

// ensureKeyboard registers a virtual keyboard with the device, once per
// session. While it is attached iOS treats the phone as having a hardware
// keyboard and hides the on-screen one.
func (h *hidSession) ensureKeyboard(ctx context.Context) (uint64, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.keyboard != 0 {
		return h.keyboard, nil
	}
	usagePage, usage := int64(1), int64(6)
	storage := map[string]any{
		"Manufacturer":     map[string]any{"string": "Pinky"},
		"Product":          map[string]any{"string": "Pinky virtual keyboard"},
		"ProductID":        map[string]any{"int": int64(0x0250)},
		"VendorID":         map[string]any{"int": int64(0x05AC)},
		"PrimaryUsage":     map[string]any{"int": usage},
		"PrimaryUsagePage": map[string]any{"int": usagePage},
		"DeviceUsagePairs": map[string]any{"array": []any{
			map[string]any{"dictionary": map[string]any{
				"DeviceUsage":     map[string]any{"int": usage},
				"DeviceUsagePage": map[string]any{"int": usagePage},
			}},
		}},
		"Transport":                      map[string]any{"string": "USB"},
		"ReportDescriptor":               map[string]any{"data": keyboardDescriptor},
		"UniversalControlVirtualService": map[string]any{"bool": true},
		"_ServiceID":                     map[string]any{"uint": surfaceKeyboard},
	}
	req := map[string]any{
		"featureIdentifier": featureUniversalHID,
		"messageType":       "Request",
		"payload": map[string]any{
			"createService": map[string]any{
				"_0": map[string]any{
					"DeviceUsagePairs":                   []any{map[string]any{"DeviceUsage": usage, "DeviceUsagePage": usagePage}},
					"PrimaryUsage":                       uint64(usage),
					"PrimaryUsagePage":                   uint64(usagePage),
					"Product":                            "Pinky virtual keyboard",
					"ProductID":                          int64(0x0250),
					"VendorID":                           int64(0x05AC),
					"_CoreDevice_codablePropertyStorage": storage,
					"_ServiceID":                         surfaceKeyboard,
				},
			},
		},
	}
	rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	resp, err := h.touch.request(rctx, req)
	cancel()
	if err != nil {
		return 0, fmt.Errorf("register keyboard: %w", err)
	}
	id := surfaceKeyboard
	if v := toInt64(resp["serviceID"]); v != 0 {
		id = uint64(v)
	}
	if e, ok := resp["error"]; ok {
		return 0, fmt.Errorf("register keyboard: %v", e)
	}
	// iOS attaches the keyboard asynchronously and nothing reports when it
	// is done; a Shift sent too early stays latched.
	time.Sleep(300 * time.Millisecond)
	h.keyboard = id
	return id, nil
}

// keys sends one keyboard report with the given usages held.
func (h *hidSession) keys(id uint64, usages []uint8) error {
	return h.touch.send(universalReport(id, keyboardReport(usages, hidTimestamp())), false)
}

// Close lifts a held contact and closes the connections.
func (h *hidSession) Close() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	var errs []error
	if h.down {
		errs = append(errs, h.touchLocked(false, h.last[0], h.last[1]))
	}
	if h.keyboard != 0 {
		errs = append(errs, h.touch.send(universalReport(h.keyboard, keyboardReport(nil, hidTimestamp())), false))
	}
	if h.held != nil && h.buttons != nil {
		errs = append(errs, h.buttons.send(buttonEvent(h.held[0], h.held[1], buttonUp), false))
		h.held = nil
	}
	errs = append(errs, h.touch.Close())
	if h.buttons != nil {
		errs = append(errs, h.buttons.Close())
	}
	return errors.Join(errs...)
}
