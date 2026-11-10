package driver

import (
	"context"
	"math"
	"time"

	"github.com/tetratorus/lightpinky/internal/screen"
)

// Pinch is a two-finger gesture: both fingers touch down on either side of
// Center, move apart or together (and turn around Center, with Rotate) for
// Duration, and lift together. Pincher performs it.
type Pinch struct {
	// Center is the point halfway between the fingers, in screen pixels.
	Center screen.Point
	// From and To are the distance between the fingers when they touch
	// down and when they lift, in screen pixels: To > From zooms in.
	From, To int
	// Angle is the direction from the first finger to the second when they
	// touch down, in degrees clockwise from the screen's x axis (y points
	// down). Rotate turns both fingers around Center by that many degrees
	// as they move, clockwise on the screen when positive.
	Angle, Rotate float64
	Duration      time.Duration
}

// At returns where the two fingers are at progress t, from 0 (touch down)
// to 1 (lift). The fingers ease in and out, so they start and stop gently
// and the content keeps the scale it reached instead of flinging on.
func (g Pinch) At(t float64) (a, b screen.Point) {
	t = math.Max(0, math.Min(1, t))
	e := t * t * (3 - 2*t)
	span := float64(g.From) + float64(g.To-g.From)*e
	rad := (g.Angle + g.Rotate*e) * math.Pi / 180
	dx, dy := math.Cos(rad)*span/2, math.Sin(rad)*span/2
	cx, cy := float64(g.Center.X), float64(g.Center.Y)
	a = screen.Point{X: int(math.Round(cx - dx)), Y: int(math.Round(cy - dy))}
	b = screen.Point{X: int(math.Round(cx + dx)), Y: int(math.Round(cy + dy))}
	return a, b
}

// Steps returns how many moves a driver that sends one every step makes,
// at least one.
func (g Pinch) Steps(step time.Duration) int {
	if step <= 0 {
		return 1
	}
	return max(1, int(g.Duration/step))
}

// Pincher is a Device that can pinch with two fingers: zoom a map or a
// photo, or turn it. A Device without it cannot, and the gateway tells the
// agent so.
type Pincher interface {
	Pinch(ctx context.Context, g Pinch) error
}

// MultiToucher injects raw touches of two fingers at once, for the
// person's pinch on the mirror: TouchDown puts both fingers down at p and
// q, TouchMove moves them there, and TouchUp lifts both. The points are in
// the pixels of the picture the device's Stream sends, as for Toucher.
type MultiToucher interface {
	Touch2(ctx context.Context, a TouchAction, p, q screen.Point) error
}
