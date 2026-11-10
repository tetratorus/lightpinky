package ios

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/png"
	"time"

	"github.com/tetratorus/lightpinky/internal/driver"
	"github.com/tetratorus/lightpinky/internal/screen"
)

// AXDevice drives an iPhone on iOS 17.4 to 26 without anything installed on
// it. The phone refuses CoreDevice's screen stream and HID input below iOS
// 27, so this path has only the developer services that remain:
//
//   - the picture: screenshots from instruments, a few a second, encoded to
//     H.264 for the mirror;
//   - the elements: the accessibility inspector's walk, placed on the
//     screen by OCR (the inspector gives no frames);
//   - no input at all. The inspector offers an Activate action (what
//     VoiceOver's double tap does), but on iOS 27.0.1 it returned at once
//     and pressed nothing in Calculator, also with the inspector's focus
//     on the key; values are not settable. So taps, swipes, keys and typing
//     return unsupported and name the WebDriverAgent helper;
//   - apps: CoreDevice's app service launches them.
//
// It shares Device's services code through an inner Device that never
// starts the stream or HID.
type AXDevice struct {
	d *Device
}

// OpenAccessibility opens a phone on iOS 17.4 to 26 through its developer
// services alone.
func OpenAccessibility(ctx context.Context, info driver.Info) (*AXDevice, error) {
	l, err := dialLink(ctx, info.ID, info.Connection == "wifi")
	if err != nil {
		return nil, err
	}
	d := &Device{info: info, l: l}
	d.info.Backend = backendAccessibility
	png, err := d.ScreenshotPNG(ctx)
	if err != nil {
		l.Close()
		return nil, err
	}
	cfg, err := pngSize(png)
	if err != nil {
		l.Close()
		return nil, fmt.Errorf("screenshot: %w", err)
	}
	// Screenshots are the screen the right way up, so they are its size;
	// each read takes it again from its screenshot (observeInspector).
	d.scr = screenInfo{Width: cfg.Width, Height: cfg.Height, Scale: 1}
	// Connect the accessibility daemon now; its first walk is slow.
	go func() {
		c, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		d.axReady(c)
	}()
	return &AXDevice{d: d}, nil
}

func pngSize(b []byte) (image.Config, error) {
	return png.DecodeConfig(bytes.NewReader(b))
}

// Info implements driver.Device.
func (a *AXDevice) Info() driver.Info { return a.d.info }

// Observe implements driver.Device: the inspector's walk, placed by OCR.
func (a *AXDevice) Observe(ctx context.Context, opt driver.ObserveOptions) (*screen.Screen, error) {
	s, _, err := a.d.observeInspector(ctx, opt, ObserveStats{})
	return s, err
}

// Screenshot implements driver.Device.
func (a *AXDevice) Screenshot(ctx context.Context) (image.Image, error) {
	return a.d.Screenshot(ctx)
}

// errNeedsHelper explains what this path cannot do.
func errNeedsHelper(what string) error {
	return fmt.Errorf("%s needs the WebDriverAgent helper on iPhones below iOS 27 (docs/notes/ios.md): %w", what, driver.ErrUnsupported)
}

// Tap implements driver.Device.
func (a *AXDevice) Tap(ctx context.Context, p screen.Point, opt driver.TapOptions) error {
	return errNeedsHelper("tapping")
}

// Swipe implements driver.Device.
func (a *AXDevice) Swipe(ctx context.Context, from, to screen.Point, dur time.Duration) error {
	return errNeedsHelper("swiping")
}

// Type implements driver.Device.
func (a *AXDevice) Type(ctx context.Context, text string, opt driver.TypeOptions) error {
	return errNeedsHelper("typing")
}

// Key implements driver.Device.
func (a *AXDevice) Key(ctx context.Context, k driver.Key) error {
	if k == driver.KeyRecents {
		return fmt.Errorf("recents on iPhone: %w", driver.ErrUnsupported)
	}
	return errNeedsHelper(fmt.Sprintf("the %s key", k))
}

// Launch implements driver.Device, through CoreDevice's app service.
func (a *AXDevice) Launch(ctx context.Context, app string) error { return a.d.Launch(ctx, app) }

// LinkApp implements driver.LinkOpener.
func (a *AXDevice) LinkApp(ctx context.Context, link string) (driver.App, error) {
	return a.d.LinkApp(ctx, link)
}

// OpenLink implements driver.LinkOpener, through CoreDevice's app service.
func (a *AXDevice) OpenLink(ctx context.Context, link string, app driver.App) error {
	return a.d.OpenLink(ctx, link, app)
}

// Apps implements driver.Device.
func (a *AXDevice) Apps(ctx context.Context) ([]driver.App, error) { return a.d.Apps(ctx) }

// Locked implements driver.LockReporter.
func (a *AXDevice) Locked(ctx context.Context) (bool, error) { return a.d.Locked(ctx) }

// Stream implements driver.Device: screenshots, encoded to H.264.
func (a *AXDevice) Stream(ctx context.Context, opt driver.StreamOptions) (driver.Stream, error) {
	return startFrameStream(ctx, func(context.Context) (frameSource, error) {
		return &shotSource{d: a.d}, nil
	}, a.d.scr.Width, a.d.scr.Height)
}

// Close implements driver.Device.
func (a *AXDevice) Close() error { return a.d.Close() }

// shotSource yields screenshots as frames, as often as the phone takes
// them (0.35 to 0.75 s each over USB on an iPhone 13 mini), and less often
// while the screen stays the same.
type shotSource struct {
	d       *Device
	same    int
	lastKey uint64
}

const (
	shotGap     = 100 * time.Millisecond // between screenshots while the screen changes
	shotIdleGap = time.Second            // once it has not changed for a while
)

func (s *shotSource) next(ctx context.Context) (frame, error) {
	gap := shotGap
	if s.same >= 5 {
		gap = shotIdleGap
	}
	if err := sleepCtx(ctx, gap); err != nil {
		return frame{}, err
	}
	b, err := s.d.ScreenshotPNG(ctx)
	if err != nil {
		return frame{}, err
	}
	key := frameKey(b)
	if key == s.lastKey {
		s.same++
	} else {
		s.same, s.lastKey = 0, key
	}
	return frame{key: key, decode: func() (image.Image, error) { return png.Decode(bytes.NewReader(b)) }}, nil
}

func (s *shotSource) Close() error { return nil }

var (
	_ driver.Device       = (*AXDevice)(nil)
	_ driver.LockReporter = (*AXDevice)(nil)
)
