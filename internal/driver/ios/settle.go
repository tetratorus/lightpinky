package ios

import (
	"context"
	"time"

	"github.com/tetratorus/lightpinky/internal/driver"
)

// settleQuiet is how long the screen must stay still to count as settled,
// and settleMin how long WaitSettled waits at least: a tap on a list row
// starts moving the picture up to about 330 ms after touch-down, so a short
// quiet stretch right after the call proves nothing.
const (
	settleQuiet = 250 * time.Millisecond
	settleMin   = 500 * time.Millisecond
)

// WaitSettled implements driver.Settler. The display stream keeps sending
// frames while nothing moves, all of one size; motion makes the sizes vary.
// The screen counts as settled once settleMin passed since the call and the
// frames of the last settleQuiet kept one size, with no accessibility screen
// or layout notification in that time.
func (d *Device) WaitSettled(ctx context.Context, limit time.Duration) (bool, error) {
	start := time.Now()
	deadline := start.Add(limit)
	for {
		m, _, err := d.live(ctx)
		if err != nil {
			return false, err
		}
		now := time.Now()
		if now.Sub(start) >= settleMin && d.quietSince(m, now.Add(-settleQuiet)) {
			return true, nil
		}
		if now.After(deadline) {
			return false, nil
		}
		if err := sleepCtx(ctx, 30*time.Millisecond); err != nil {
			return false, err
		}
	}
}

// quietSince reports whether nothing moved on screen since t: either no
// frame came while the stream is alive (the phone stops sending frames
// about 7 s after the last change), or every frame since t has the same
// size, give or take 2% or 64 bytes. The size of a still frame depends on
// the encoder's bitrate at the time, about 1–2 KB when idle and a padded
// 10 KB for seconds after motion, so a constant size is the sign, not a
// small one.
func (d *Device) quietSince(m *media, t time.Time) bool {
	d.svcMu.Lock()
	ax := d.ax
	d.svcMu.Unlock()
	if ax != nil && ax.lastChange().After(t) {
		return false
	}
	fs := m.framesSince(t)
	if len(fs) == 0 {
		return m.alive()
	}
	if len(fs) < 3 {
		return false
	}
	lo, hi := fs[0].size, fs[0].size
	for _, f := range fs[1:] {
		lo, hi = min(lo, f.size), max(hi, f.size)
	}
	return hi-lo <= max(64, lo/50)
}

var _ driver.Settler = (*Device)(nil)
