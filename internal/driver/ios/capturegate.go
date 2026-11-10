package ios

import (
	"context"
	"sync"
	"time"

	"github.com/tetratorus/lightpinky/internal/screen"
)

// A read takes a picture of the screen (a screenshot, or a frame of the
// live video) to read its text by OCR, and on the inspector path to place
// the tree's elements. driver.ObserveOptions.MayCapture says whether it may:
// not of a locked phone, nor of a blocked app the person did not allow for
// the task. The gateway answers from the screen as read, so the picture
// waits until the read knows the app and the lock state.
//
// To keep the wait short, the remoteAXService and WebDriverAgent paths ask
// as soon as the frontmost process and the lock state are known:
// remoteAXService names the process in the walk's first request (18 to 19
// ms over USB), WebDriverAgent the app in its own request, both long before
// the tree, while lockdown tells the lock state meanwhile. On
// remoteAXService the read names an app from the process alone, except for
// SpringBoard, which is the home screen when its icons are in the tree and
// otherwise shows the app beneath it (Face ID's prompt over a bank's app
// shows the bank): appCandidates lists every app the read may name, and the
// picture starts early only when each may be captured. A process Pinky
// cannot name waits for the whole read, which finds the app by the name in
// the tree. The read asks again with the screen it returns, and drops the
// text of a picture it may not use: the app changed while it read.

// captureGate starts a read's picture once MayCapture allows it.
type captureGate struct {
	may   func(*screen.Screen) bool // nil: any screen
	start func()
	begun time.Time // when the read began, for the stats

	mu      sync.Mutex
	started bool
	at      time.Duration // when the picture started, after begun
	how     string        // "at once", "after the app", "after the read"
}

// newCaptureGate returns a gate that calls start once a screen may be
// captured; with no MayCapture, at once.
func newCaptureGate(may func(*screen.Screen) bool, start func()) *captureGate {
	g := &captureGate{may: may, start: start, begun: time.Now()}
	if may == nil {
		g.begin("at once")
	}
	return g
}

// begin starts the picture. Callers hold g.mu, or own g alone.
func (g *captureGate) begin(how string) {
	g.started, g.at, g.how = true, time.Since(g.begun), how
	g.start()
}

// early asks with the screens the read may return, each with the app it
// may name and the lock state, before the rest of the read is in: the
// picture starts only when every one may be captured.
func (g *captureGate) early(screens []*screen.Screen) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.started || len(screens) == 0 {
		return
	}
	for _, s := range screens {
		if !g.may(s) {
			return
		}
	}
	g.begin("after the app")
}

// late asks with the screen the read returns: it starts the picture if
// nothing started it and the screen may be captured, and reports whether
// the picture's text may be used. Callers wait for the picture after it.
func (g *captureGate) late(s *screen.Screen) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.may == nil {
		return true
	}
	if !g.started {
		if !g.may(s) {
			return false
		}
		g.begin("after the read")
		return true
	}
	return g.may(s)
}

// stats says when the picture started, and how: "" when it did not.
func (g *captureGate) stats() (string, time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.started {
		return "", 0
	}
	return g.how, g.at
}

// appCandidates lists the apps screenApp may name for a read whose
// frontmost process is pid, from what it knows before the tree: one app
// when the process settles it, SpringBoard and the app beneath it while the
// phone is unlocked (the tree's icons tell them apart). ok is false for a
// process the process list does not name, which screenApp looks up by the
// name in the tree.
func (d *Device) appCandidates(ctx context.Context, pid int, locked bool) ([]screen.App, bool) {
	app, ok := d.appForPID(ctx, uint64(pid))
	if !ok {
		return nil, false
	}
	named := screen.App{Name: app.Name, ID: app.ID}
	switch {
	case app.ID != springBoard && d.homeScreenApp(ctx, app.ID):
		return []screen.App{named}, true
	case locked:
		return []screen.App{named}, true
	case app.ID == springBoard:
		if fg, found := d.foregroundApp(ctx); found {
			return []screen.App{named, fg}, true
		}
		return []screen.App{named}, true
	default: // system UI over an app
		if fg, found := d.foregroundApp(ctx); found {
			return []screen.App{fg}, true
		}
		return []screen.App{named}, true
	}
}
