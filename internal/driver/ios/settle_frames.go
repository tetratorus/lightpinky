package ios

import (
	"context"
	"time"

	"github.com/tetratorus/lightpinky/internal/driver"
)

// Settling below iOS 27 watches the picture: a frame that differs from the
// one before it (other bytes; the phone encodes a still screen the same way
// every time) means the screen is still moving. As on iOS 27, the screen
// counts as settled once settleMin passed since the call and nothing
// changed for settleQuiet.

// frameSettle waits until frames from next stop changing, or until limit.
// next returns each frame's key as it arrives.
func frameSettle(ctx context.Context, limit time.Duration, next func(context.Context) (uint64, error)) (bool, error) {
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	var last uint64
	changed := start
	seen := false
	for {
		key, err := next(ctx)
		now := time.Now()
		if err != nil {
			if ctx.Err() != nil && now.Sub(start) >= limit {
				return false, nil // limit reached while waiting for a frame
			}
			return false, err
		}
		if !seen || key != last {
			last, seen, changed = key, true, now
		}
		if now.Sub(start) >= settleMin && now.Sub(changed) >= settleQuiet {
			return true, nil
		}
	}
}

// WaitSettled implements driver.Settler with WebDriverAgent's MJPEG
// picture, 15 frames a second.
func (d *WDADevice) WaitSettled(ctx context.Context, limit time.Duration) (bool, error) {
	if _, err := d.c.session(ctx); err != nil {
		return false, err
	}
	sctx, cancel := context.WithTimeout(ctx, limit+time.Second)
	defer cancel()
	src, err := openMJPEG(sctx, d.c.long, d.c.mjpeg)
	if err != nil {
		return false, err
	}
	defer src.Close()
	return frameSettle(ctx, limit, func(ctx context.Context) (uint64, error) {
		f, err := src.next(ctx)
		return f.key, err
	})
}

// WaitSettled implements driver.Settler with screenshots: the screen is
// settled when two in a row are the same and settleMin has passed.
func (a *AXDevice) WaitSettled(ctx context.Context, limit time.Duration) (bool, error) {
	start := time.Now()
	deadline := start.Add(limit)
	var last uint64
	seen := false
	for time.Now().Before(deadline) {
		c, cancel := context.WithDeadline(ctx, deadline)
		b, err := a.d.ScreenshotPNG(c)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return false, ctx.Err()
			}
			if time.Now().After(deadline) {
				return false, nil
			}
			return false, err
		}
		key := frameKey(b)
		if seen && key == last && time.Since(start) >= settleMin {
			return true, nil
		}
		last, seen = key, true
	}
	return false, nil
}

var (
	_ driver.Settler = (*WDADevice)(nil)
	_ driver.Settler = (*AXDevice)(nil)
)
