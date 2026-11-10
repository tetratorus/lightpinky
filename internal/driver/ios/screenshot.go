package ios

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/png"
	"time"

	goios "github.com/danielpaulus/go-ios/ios"
	dtx "github.com/danielpaulus/go-ios/ios/dtx_codec"
)

const (
	screenCaptureService     = "com.apple.coredevice.screencaptureservice"
	featureCaptureScreenshot = "com.apple.coredevice.feature.capturescreenshot"
	actionCaptureScreenshot  = "com.apple.coredevice.action.capturescreenshot"
)

// Two services take screenshots, both as 1125x2436 PNGs on an iPhone 13 mini:
// the instruments one (DTServiceHub, what Xcode's Instruments uses) and
// CoreDevice's screen capture service, which encodes 16 bits per channel.
// Over USB a busy home screen takes about 0.6 s (5.4 MB) and 0.75 s
// (6.9 MB); a plain Settings page 0.35 s. Either service can get stuck on the
// device and stop answering until it restarts, so each call falls back to
// the other.

// ScreenshotPNG returns a PNG of the main display at the size UIKit renders
// (1125x2436 on an iPhone 13 mini), which is the driver's coordinate space.
func (d *Device) ScreenshotPNG(ctx context.Context) ([]byte, error) {
	d.mu.Lock()
	uid := ""
	if d.multiLocked() {
		uid = d.scr.UniqueID
	}
	d.mu.Unlock()
	d.svcMu.Lock()
	defer d.svcMu.Unlock()
	if uid != "" && !d.shotUIDOff {
		// A phone with several displays: the instruments service takes the
		// main display, which may be the one folded away; the capture
		// service takes the display the stream shows, by its uniqueId.
		b, err := d.captureShotOf(ctx, uid)
		if err == nil {
			return b, nil
		}
		if ctx.Err() != nil {
			return nil, fmt.Errorf("screenshot: %w", err)
		}
		d.shotUIDOff = true
		d.log.Warn("iphone: no screenshot of the display in use; taking the main display's from now on", "device", d.info.ID, "display", uid, "err", err)
	}
	first, second := d.instrumentsShot, d.captureShot
	if d.preferCapture {
		first, second = second, first
	}
	b, err1 := first(ctx)
	if err1 == nil {
		return b, nil
	}
	if ctx.Err() != nil {
		return nil, fmt.Errorf("screenshot: %w", err1)
	}
	b, err2 := second(ctx)
	if err2 == nil {
		d.preferCapture = !d.preferCapture
		return b, nil
	}
	return nil, fmt.Errorf("screenshot: %w", errors.Join(err1, err2))
}

// shotTimeouts bound one screenshot from each service. Over USB a PNG of a
// busy screen (7 MB) arrives within a second, so a service that has not
// answered in a few seconds is stuck and the other one is asked. Over Wi-Fi
// the PNG arrives at about 1 MB/s: a busy iPad screen (5 to 15 MB) took 5
// to 26 s on 2026-11-08, and go-ios's own 5 s for the instruments channel
// failed every one of them, after which the capture service sent the same
// screen again, 16 bits a channel.
func shotTimeouts(conn string) (instruments, capture time.Duration) {
	if conn == "wifi" {
		return 30 * time.Second, 30 * time.Second
	}
	return 5 * time.Second, 10 * time.Second
}

// The instruments screenshot channel, opened as go-ios's ScreenshotService
// opens it, and asked with Pinky's deadline instead of its 5 s.
const (
	instrumentsScreenshot = "com.apple.instruments.server.services.screenshot"
	instrumentsRemote     = "com.apple.instruments.remoteserver"
	instrumentsRemote14   = "com.apple.instruments.remoteserver.DVTSecureSocketProxy"
)

type instShot struct {
	conn *dtx.Connection
	ch   *dtx.Channel
}

// ackDispatcher acknowledges what the phone sends on the connection unasked.
type ackDispatcher struct{ conn *dtx.Connection }

func (a ackDispatcher) Dispatch(m dtx.Message) { dtx.SendAckIfNeeded(a.conn, m) }

func openInstShot(e goios.DeviceEntry, timeout time.Duration) (*instShot, error) {
	var conn *dtx.Connection
	var err error
	if e.SupportsRsd() {
		conn, err = dtx.NewTunnelConnection(e, instrumentsService)
	} else if conn, err = dtx.NewUsbmuxdConnection(e, instrumentsRemote); err != nil {
		conn, err = dtx.NewUsbmuxdConnection(e, instrumentsRemote14)
	}
	if err != nil {
		return nil, err
	}
	ch := conn.RequestChannelIdentifier(instrumentsScreenshot, ackDispatcher{conn}, dtx.WithTimeout(uint32(timeout/time.Second)))
	return &instShot{conn: conn, ch: ch}, nil
}

func (s *instShot) take(ctx context.Context) ([]byte, error) {
	msg, err := s.ch.MethodCallWithContext(ctx, "takeScreenshot")
	if err != nil {
		return nil, fmt.Errorf("TakeScreenshot: %w", err)
	}
	if len(msg.Payload) == 0 {
		return nil, errors.New("TakeScreenshot: empty response payload")
	}
	b, ok := msg.Payload[0].([]byte)
	if !ok {
		return nil, fmt.Errorf("TakeScreenshot: unexpected payload type %T", msg.Payload[0])
	}
	return b, nil
}

func (s *instShot) Close() { s.conn.Close() }

// instrumentsShot uses the instruments screenshot channel. Callers hold
// svcMu.
func (d *Device) instrumentsShot(ctx context.Context) ([]byte, error) {
	limit, _ := shotTimeouts(d.info.Connection)
	if d.inst == nil {
		type result struct {
			s   *instShot
			err error
		}
		ch := make(chan result, 1)
		go func() {
			s, err := openInstShot(d.l.entry, limit)
			ch <- result{s, err}
		}()
		select {
		case r := <-ch:
			if r.err != nil {
				return nil, fmt.Errorf("instruments: %w", r.err)
			}
			d.inst = r.s
		case <-time.After(dialTimeout):
			go func() {
				if r := <-ch; r.s != nil {
					r.s.Close()
				}
			}()
			return nil, fmt.Errorf("instruments: no answer in %v", dialTimeout)
		case <-ctx.Done():
			go func() {
				if r := <-ch; r.s != nil {
					r.s.Close()
				}
			}()
			return nil, ctx.Err()
		}
	}
	s := d.inst
	tctx, cancel := context.WithTimeout(ctx, limit)
	b, err := s.take(tctx)
	cancel()
	if err != nil || len(b) == 0 {
		// The connection's state is unknown now; start over next time.
		s.Close()
		d.inst = nil
		if err == nil {
			err = errors.New("empty image")
		}
		return nil, fmt.Errorf("instruments: %w", err)
	}
	return b, nil
}

// captureShot uses CoreDevice's screen capture service. Callers hold svcMu.
func (d *Device) captureShot(ctx context.Context) ([]byte, error) {
	return d.captureShotOf(ctx, "")
}

// captureShotOf takes a screenshot of the display with the uniqueId uid
// ("": the main display) with CoreDevice's screen capture service, whose
// key for it is displayUniqueID (go-ios's ScreenshotOptions). Callers hold
// svcMu.
func (d *Device) captureShotOf(ctx context.Context, uid string) ([]byte, error) {
	in := map[string]any{"requestedFormat": "png"}
	if uid != "" {
		in["displayUniqueID"] = uid
	}
	for attempt := 0; ; attempt++ {
		if d.shot == nil {
			svc, err := dialCoreDevice(d.l.entry, screenCaptureService)
			if err != nil {
				return nil, err
			}
			d.shot = svc
		}
		_, limit := shotTimeouts(d.info.Connection)
		c, cancel := context.WithTimeout(ctx, limit)
		out, err := d.shot.invoke(c, featureCaptureScreenshot, actionCaptureScreenshot, in)
		cancel()
		if err != nil {
			d.shot.Close()
			d.shot = nil
			if isClosedErr(err) && attempt == 0 && ctx.Err() == nil {
				continue // the service closes idle connections; reconnect once
			}
			return nil, err
		}
		img, _ := out["image"].([]byte)
		if len(img) == 0 {
			return nil, fmt.Errorf("%s: empty image", screenCaptureService)
		}
		return img, nil
	}
}

// Screenshot implements driver.Device.
func (d *Device) Screenshot(ctx context.Context) (image.Image, error) {
	b, err := d.ScreenshotPNG(ctx)
	if err != nil {
		return nil, d.gone(err)
	}
	img, err := png.Decode(bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("screenshot: decode: %w", err)
	}
	return img, nil
}
