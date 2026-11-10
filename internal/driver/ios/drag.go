package ios

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/tetratorus/lightpinky/internal/driver"
)

// wdaDragStep is how far apart the moves of a drag through WebDriverAgent
// are. XCTest draws a straight line between two moves, so they need not
// be as close as the reports of the HID path; they only shape the easing.
const wdaDragStep = 40 * time.Millisecond

// Drag implements driver.Dragger.
func (d *WDADevice) Drag(ctx context.Context, g driver.Drag) error {
	steps := g.Steps(wdaDragStep)
	pts := make([]wdaPoint, len(steps))
	for i, s := range steps {
		pts[i] = d.toPoints(s.P)
	}
	d.inputMu.Lock()
	defer d.inputMu.Unlock()
	return d.c.drag(ctx, pts, steps)
}

// drag performs a drag as W3C pointer actions: down at pts[0], then for
// each next step a move to its point taking its Wait, or a pause when the
// finger stays put, and up at the last.
func (c *wdaClient) drag(ctx context.Context, pts []wdaPoint, steps []driver.DragStep) error {
	if len(pts) == 0 || len(steps) != len(pts) {
		return errors.New("empty drag")
	}
	acts := []map[string]any{
		{"type": "pointerMove", "duration": 0, "x": pts[0].X, "y": pts[0].Y},
		{"type": "pointerDown", "button": 0},
	}
	for i := 1; i < len(pts); i++ {
		ms := steps[i].Wait.Milliseconds()
		if pts[i] == pts[i-1] {
			if ms > 0 {
				acts = append(acts, map[string]any{"type": "pause", "duration": ms})
			}
			continue
		}
		acts = append(acts, map[string]any{"type": "pointerMove", "duration": max(ms, 1), "x": pts[i].X, "y": pts[i].Y})
	}
	acts = append(acts, map[string]any{"type": "pointerUp", "button": 0})
	body := map[string]any{"actions": []any{map[string]any{
		"type": "pointer", "id": "finger1",
		"parameters": map[string]any{"pointerType": "touch"},
		"actions":    acts,
	}}}
	return c.do(ctx, c.long, http.MethodPost, "/actions", body, nil)
}

var (
	_ driver.Dragger = (*Device)(nil)
	_ driver.Dragger = (*WDADevice)(nil)
)
