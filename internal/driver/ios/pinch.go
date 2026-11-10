package ios

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/tetratorus/lightpinky/internal/driver"
	"github.com/tetratorus/lightpinky/internal/screen"
)

// Pinches go through WebDriverAgent as W3C actions with two touch
// pointers, which XCTest performs as one multi-touch event. Device Hub's
// virtual touchscreen takes one contact as Pinky drives it (hid.go), so
// *Device does not pinch: its reports have five contact slots, but a
// second contact has not been tried on a phone.

// wdaPinchStep is how often a pinch's path is sampled: W3C moves are
// straight lines, so a turning pinch needs a point every few degrees.
const (
	wdaPinchStep     = 40 * time.Millisecond
	wdaPinchMaxMoves = 60
)

// gesture2 performs two fingers' paths at once: both touch down at their
// first points, move to each next point together, steps[i] taking the move
// to a[i] and b[i] (steps[0] is ignored), and lift at their last.
func (c *wdaClient) gesture2(ctx context.Context, a, b []wdaPoint, steps []time.Duration) error {
	if len(a) == 0 || len(a) != len(b) || len(steps) != len(a) {
		return errors.New("empty or uneven two-finger gesture")
	}
	finger := func(id string, path []wdaPoint) map[string]any {
		acts := []map[string]any{
			{"type": "pointerMove", "duration": 0, "x": path[0].X, "y": path[0].Y},
			{"type": "pointerDown", "button": 0},
		}
		for i, p := range path[1:] {
			acts = append(acts, map[string]any{"type": "pointerMove", "duration": max(steps[i+1].Milliseconds(), 1), "x": p.X, "y": p.Y})
		}
		acts = append(acts, map[string]any{"type": "pointerUp", "button": 0})
		return map[string]any{
			"type": "pointer", "id": id,
			"parameters": map[string]any{"pointerType": "touch"},
			"actions":    acts,
		}
	}
	body := map[string]any{"actions": []any{finger("finger1", a), finger("finger2", b)}}
	return c.do(ctx, c.long, http.MethodPost, "/actions", body, nil)
}

// Pinch implements driver.Pincher.
func (d *WDADevice) Pinch(ctx context.Context, g driver.Pinch) error {
	n := min(g.Steps(wdaPinchStep), wdaPinchMaxMoves)
	a, b := make([]wdaPoint, n+1), make([]wdaPoint, n+1)
	steps := make([]time.Duration, n+1)
	for i := 0; i <= n; i++ {
		pa, pb := g.At(float64(i) / float64(n))
		a[i], b[i] = d.toPoints(pa), d.toPoints(pb)
		if i > 0 {
			steps[i] = g.Duration / time.Duration(n)
		}
	}
	d.inputMu.Lock()
	defer d.inputMu.Unlock()
	return d.c.gesture2(ctx, a, b, steps)
}

// Touch2 implements driver.MultiToucher for the person's pinch on the
// mirror: like Touch, it records both fingers while they are down and
// plays the pinch back when they lift.
func (d *WDADevice) Touch2(ctx context.Context, act driver.TouchAction, p, q screen.Point) error {
	vw, vh := d.videoSize()
	if vw > 0 && vh > 0 {
		d.seeOrientation(float64(vw), float64(vh))
	}
	w, h := d.size()
	p, q = fromVideo(p, vw, vh, w, h), fromVideo(q, vw, vh, w, h)
	a, b, steps, err := d.rec2.touch(act, p, q, time.Now())
	if err != nil || a == nil {
		return err
	}
	pa, pb := make([]wdaPoint, len(a)), make([]wdaPoint, len(b))
	for i := range a {
		pa[i], pb[i] = d.toPoints(a[i]), d.toPoints(b[i])
	}
	d.inputMu.Lock()
	defer d.inputMu.Unlock()
	return d.c.gesture2(ctx, pa, pb, steps)
}

// pinchRecorder is touchRecorder for two fingers: it records their paths
// while they are down and returns them when they lift.
type pinchRecorder struct {
	mu   sync.Mutex
	down bool
	a, b []screen.Point
	at   []time.Time
}

// touch records one two-finger touch; on TouchUp it returns both paths and
// how long each move took (steps[0] is zero).
func (r *pinchRecorder) touch(act driver.TouchAction, p, q screen.Point, now time.Time) (a, b []screen.Point, steps []time.Duration, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch act {
	case driver.TouchDown:
		r.down = true
		r.a, r.b, r.at = []screen.Point{p}, []screen.Point{q}, []time.Time{now}
		return nil, nil, nil, nil
	case driver.TouchMove:
		n := len(r.a)
		if !r.down || max(dist(r.a[n-1], p), dist(r.b[n-1], q)) < recMinMove || now.Sub(r.at[n-1]) < recMinGap {
			return nil, nil, nil, nil
		}
		if n >= recMaxPoints {
			// Keep the shape: drop every other point but the first.
			ka, kb, kt := r.a[:1], r.b[:1], r.at[:1]
			for i := 2; i < n; i += 2 {
				ka, kb, kt = append(ka, r.a[i]), append(kb, r.b[i]), append(kt, r.at[i])
			}
			r.a, r.b, r.at = ka, kb, kt
		}
		r.a, r.b, r.at = append(r.a, p), append(r.b, q), append(r.at, now)
		return nil, nil, nil, nil
	case driver.TouchUp:
		if !r.down {
			return nil, nil, nil, nil
		}
		r.down = false
		a, b = append(r.a, p), append(r.b, q)
		at := append(r.at, now)
		steps = make([]time.Duration, len(a))
		for i := 1; i < len(a); i++ {
			steps[i] = at[i].Sub(at[i-1])
		}
		return a, b, steps, nil
	}
	return nil, nil, nil, fmt.Errorf("touch action %q: %w", act, driver.ErrUnsupported)
}

var (
	_ driver.Pincher      = (*WDADevice)(nil)
	_ driver.MultiToucher = (*WDADevice)(nil)
)
