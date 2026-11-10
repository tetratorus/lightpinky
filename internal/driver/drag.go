package driver

import (
	"context"
	"math"
	"time"

	"github.com/tetratorus/lightpinky/internal/screen"
)

// Drag is a one-finger drag that moves something: the finger touches down
// at From and rests there for Hold (long enough to pick up an app icon or
// a list row that moves on a long press), then moves to each stop in turn,
// resting at each for its Pause, and lifts at the last. The finger stops
// before it lifts, so nothing flings on as after a swipe, and a rest near
// the screen's edge lets a home screen turn to its next page. Dragger
// performs it. Points are in screen pixels.
type Drag struct {
	From  screen.Point
	Hold  time.Duration
	Stops []DragStop
}

// DragStop is one leg of a drag: the finger moves to Point over Move,
// easing in and out, then rests there for Pause.
type DragStop struct {
	Point       screen.Point
	Move, Pause time.Duration
}

// DragStep is one sample of a drag, for drivers that move the finger in
// steps: wait Wait, then put the finger at P. The first step puts the
// finger down; the finger lifts at the last. A step at the same point as
// the one before is a rest.
type DragStep struct {
	Wait time.Duration
	P    screen.Point
}

// To is where the finger lifts: the last stop, or From when there is none.
func (g Drag) To() screen.Point {
	if len(g.Stops) == 0 {
		return g.From
	}
	return g.Stops[len(g.Stops)-1].Point
}

// Points lists where the finger touches down, every stop and where it
// lifts, in order.
func (g Drag) Points() []screen.Point {
	out := make([]screen.Point, 0, len(g.Stops)+1)
	out = append(out, g.From)
	for _, s := range g.Stops {
		out = append(out, s.Point)
	}
	return out
}

// Duration is how long the finger stays down.
func (g Drag) Duration() time.Duration {
	d := g.Hold
	for _, s := range g.Stops {
		d += s.Move + s.Pause
	}
	return d
}

// Steps returns the drag as samples about step apart while the finger
// moves; a rest is one step of its whole length at the same point. Moves
// ease in and out, so the finger starts and stops gently.
func (g Drag) Steps(step time.Duration) []DragStep {
	out := []DragStep{{P: g.From}}
	if g.Hold > 0 {
		out = append(out, DragStep{Wait: g.Hold, P: g.From})
	}
	at := g.From
	for _, s := range g.Stops {
		n := 1
		if step > 0 {
			n = max(1, int(s.Move/step))
		}
		var waited time.Duration
		for i := 1; i <= n; i++ {
			t := float64(i) / float64(n)
			e := t * t * (3 - 2*t)
			p := screen.Point{
				X: at.X + int(math.Round(float64(s.Point.X-at.X)*e)),
				Y: at.Y + int(math.Round(float64(s.Point.Y-at.Y)*e)),
			}
			w := s.Move * time.Duration(i) / time.Duration(n)
			out = append(out, DragStep{Wait: w - waited, P: p})
			waited = w
		}
		if s.Pause > 0 {
			out = append(out, DragStep{Wait: s.Pause, P: s.Point})
		}
		at = s.Point
	}
	return out
}

// Dragger is a Device that can drag: press, hold to pick something up,
// move with rests on the way, and let go without a fling. A Device without
// it cannot, and the gateway tells the agent so.
type Dragger interface {
	Drag(ctx context.Context, g Drag) error
}

// DragReplanner is a Dragger that can change the rest of a drag while the
// finger is down. The gateway uses it on a home screen, which shrinks its
// page once an app is picked up, to let go where the target moved to.
type DragReplanner interface {
	Dragger
	// DragReplan performs g as Drag does, but once the finger has reached
	// g.Stops[at] and rested there for its Pause, it calls replan with the
	// finger still down and still. Stops replan returns replace
	// g.Stops[at+1:], from g.Stops[at].Point on; nil keeps them. The finger
	// rests while replan runs, so replan should be quick, and it must not
	// send input to the phone. The finger always lifts, also when ctx ends.
	DragReplan(ctx context.Context, g Drag, at int, replan func(ctx context.Context) []DragStop) error
}
