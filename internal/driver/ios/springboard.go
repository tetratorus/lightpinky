package ios

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/tetratorus/lightpinky/internal/screen"
)

// What SpringBoard shows over the app in front: notification banners, the
// alert of a timer going off, status banners. None of it is in the app's own
// tree, which reads walk from the frontmost application, so the agent never
// learned that the phone wanted attention (field test item 7: a timer that
// went off during a task showed only as OCR fragments), and the gateway's
// masking of one-time codes and hiding of blocked apps' notifications,
// which work on notification events, did not apply.
//
// SpringBoard's application element answers whichever app is in front (a
// request of type 1 with its pid). While an app is in front, its children
// are the status bar's items, which all carry traitStatusBar, and whatever
// SpringBoard draws over the app. Each read asks for them in the
// background (see springBoardWatch), and what is not the status bar
// becomes an event, once per appearance: an alert a dialog event, whose
// buttons are also listed so that an agent can press them; anything else
// with a button a notification event (a banner opens its app when tapped);
// the rest a toast event. A notification is also listed as one element
// while it shows, with its place and the app that posted it in its ID
// (screen.NotificationID), so that the gateway can hide a blocked app's
// banner and cover it in screenshots, and mask a code in it there too. The
// app's elements under an overlay are left out, as under the tab bar.

// traitStatusBar marks the status bar's items in SpringBoard's tree (the
// time, signal, Wi-Fi, battery, location, the breadcrumb back to an app).
// Observed on iOS 27; not public.
const traitStatusBar = 1 << 23

// XCUIElementTypeAlert and XCUIElementTypeDialog.
const (
	xcAlert  = 7
	xcDialog = 8
)

// Limits for reading SpringBoard's overlays.
var sbLimits = walkLimits{MaxElements: 60, Budget: 1500 * time.Millisecond}

// sbRecheck is how long a SpringBoard whose element could not be had is
// left alone.
const sbRecheck = 30 * time.Second

// sbOverlay is one thing SpringBoard shows over the app: the subtree of one
// of its application element's children.
type sbOverlay struct {
	key [2]uint64 // the child's process and ID
	els []raxElement
}

// sbProbe is what one look at SpringBoard found.
type sbProbe struct {
	overlays []sbOverlay
	front    bool // SpringBoard was the frontmost application: nothing applies
	at       time.Time
}

// springBoardWatch keeps track of SpringBoard's overlays for a Device.
type springBoardWatch struct {
	mu       sync.Mutex
	pid      int
	app      *axpObject
	status   map[uint64]bool // children found to be status bar items
	failedAt time.Time
	running  chan struct{} // closed when the probe in flight ends
	last     *sbProbe
	shown    map[[2]uint64]bool // overlays already reported
	// posted is the bundle id of the app that posted each notification
	// overlay still shown ("" when not found), found when it came.
	posted map[[2]uint64]string
}

// kickSpringBoard starts a look at SpringBoard's overlays, unless one is
// under way, and returns a channel closed when the look in flight ends.
// The look runs on its own deadline: a request cut short would spoil the
// session.
func (d *Device) kickSpringBoard(rax *raxSession) <-chan struct{} {
	w := &d.sb
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.running != nil {
		return w.running
	}
	if !w.failedAt.IsZero() && time.Since(w.failedAt) < sbRecheck {
		done := make(chan struct{})
		close(done)
		return done
	}
	done := make(chan struct{})
	w.running = done
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*sbLimits.Budget+2*raxRequestTimeout)
		defer cancel()
		p, err := d.probeSpringBoard(ctx, rax)
		w.mu.Lock()
		if err != nil {
			w.failedAt = time.Now()
		} else {
			w.last = p
		}
		w.running = nil
		w.mu.Unlock()
		close(done)
	}()
	return done
}

// springBoardOverlays returns what SpringBoard shows over the app in
// front: the result of the look started for this read when it has ended,
// else of the last one if it is recent.
func (d *Device) springBoardOverlays(done <-chan struct{}) ([]sbOverlay, bool) {
	select {
	case <-done:
	default:
	}
	w := &d.sb
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.last == nil || w.last.front || time.Since(w.last.at) > 5*time.Second {
		return nil, false
	}
	return w.last.overlays, true
}

// probeSpringBoard reads SpringBoard's children that are not the status
// bar's, each with what lies below it.
func (d *Device) probeSpringBoard(ctx context.Context, rax *raxSession) (*sbProbe, error) {
	app, err := d.springBoardApp(ctx, rax)
	if err != nil {
		return nil, err
	}
	front, err := rax.frontmostApp(ctx)
	if err != nil {
		return nil, err
	}
	p := &sbProbe{at: time.Now()}
	if front.PID == app.PID {
		p.front = true
		return p, nil
	}
	r, err := rax.request(ctx, axpRequest{Type: axpReqAttributes, Client: raxClientType, Object: app,
		Params: map[string]any{"attributes": []int{axpAttrChildren, axpAttrNavigationOrder}}})
	if err != nil {
		return nil, err
	}
	ad, _ := r.Result.(axpDict)
	_, kids := raxElementFrom(app, ad, -1, 0)
	w := &d.sb
	for _, k := range kids {
		w.mu.Lock()
		status := w.status[k.ID]
		w.mu.Unlock()
		if status {
			continue
		}
		els, _, err := rax.walkFrom(ctx, k, sbLimits, nil)
		if err != nil {
			return nil, err
		}
		if len(els) == 0 {
			continue
		}
		if els[0].Traits&traitStatusBar != 0 {
			w.mu.Lock()
			if w.status == nil {
				w.status = map[uint64]bool{}
			}
			w.status[k.ID] = true
			w.mu.Unlock()
			continue
		}
		p.overlays = append(p.overlays, sbOverlay{key: [2]uint64{uint64(k.PID), k.ID}, els: els})
	}
	return p, nil
}

// springBoardApp returns SpringBoard's application element, found through
// its pid in the process list.
func (d *Device) springBoardApp(ctx context.Context, rax *raxSession) (*axpObject, error) {
	w := &d.sb
	w.mu.Lock()
	app := w.app
	w.mu.Unlock()
	if app != nil {
		return app, nil
	}
	procs, err := d.processPaths(ctx)
	if err != nil {
		return nil, err
	}
	pid := 0
	for p, exe := range procs {
		if strings.HasPrefix(exe, "/System/Library/CoreServices/SpringBoard.app/") {
			pid = int(p)
		}
	}
	if pid == 0 {
		return nil, errNoSpringBoard
	}
	app, err = rax.appObject(ctx, pid)
	if err != nil {
		return nil, err
	}
	w.mu.Lock()
	w.pid, w.app = pid, app
	w.mu.Unlock()
	return app, nil
}

// noteFront tells the watch which application a read found in front: when
// it is SpringBoard, its element is that one.
func (d *Device) noteFront(front *axpObject, isSpringBoard bool) {
	if !isSpringBoard {
		return
	}
	w := &d.sb
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.app == nil || w.app.PID != front.PID || w.app.ID != front.ID {
		w.app, w.pid, w.status = front, front.PID, nil
		w.failedAt = time.Time{}
	}
}

var errNoSpringBoard = errors.New("SpringBoard is not running")

// springBoardElement returns SpringBoard's application element, if known.
func (d *Device) springBoardElement() *axpObject {
	d.sb.mu.Lock()
	defer d.sb.mu.Unlock()
	return d.sb.app
}

// overlayKind says what an overlay is, as an event kind.
func overlayKind(els []raxElement) screen.EventKind {
	button := false
	for _, e := range els {
		switch {
		case e.Type == xcAlert || e.Type == xcDialog:
			return screen.EventDialog
		case e.Type == xcButton || e.Traits&traitButton != 0 || e.AXRole == axpRoleButton:
			button = true
		}
	}
	if button {
		return screen.EventNotification
	}
	return screen.EventToast
}

// overlayText is what an overlay says: its elements' labels and values as
// agents read them, in order, each once.
func overlayText(els []raxElement) string {
	var parts []string
	seen := map[string]bool{}
	for _, e := range els {
		if !e.IsElement || !e.Visible {
			continue
		}
		role, _ := raxRole(e, false)
		l := strings.TrimSpace(raxLabel(e, role))
		if l == "" || seen[l] {
			continue
		}
		seen[l] = true
		parts = append(parts, l)
	}
	text := strings.Join(parts, ", ")
	if r := []rune(text); len(r) > maxEventText {
		text = string(r[:maxEventText]) + "…"
	}
	return text
}

// maxEventText bounds an event's text, in characters.
const maxEventText = 300

// overlayRect is the part of the screen an overlay covers, in pixels, from
// the frames of SpringBoard's own elements (another process's, such as a
// live activity's, have frames of their own only).
func overlayRect(o sbOverlay, scale float64, w, h int) screen.Rect {
	var r screen.Rect
	for _, e := range o.els {
		if e.HasFrame && e.Object.PID == int(o.key[0]) {
			r = union(r, pixelRect(e.Frame, scale, w, h))
		}
	}
	return r
}

// overlayEvents turns what SpringBoard shows into events, each overlay
// once while it stays, and returns the overlays' places, to leave out the
// app's elements under them, the dialogs' elements, to list, and an
// element for each notification shown (notificationElement).
func (d *Device) overlayEvents(ctx context.Context, over []sbOverlay, scale float64, w, h int) (evs []screen.Event, covered []screen.Rect, dialog []raxElement, notes []screen.Element) {
	sw := &d.sb
	sw.mu.Lock()
	now := map[[2]uint64]bool{}
	var fresh []sbOverlay
	for _, o := range over {
		now[o.key] = true
		if !sw.shown[o.key] {
			fresh = append(fresh, o)
		}
	}
	sw.shown = now
	posted := map[[2]uint64]string{}
	for k, app := range sw.posted {
		if now[k] {
			posted[k] = app
		}
	}
	sw.mu.Unlock()
	for _, o := range fresh {
		kind := overlayKind(o.els)
		text := overlayText(o.els)
		if text == "" {
			continue
		}
		ev := screen.Event{Kind: kind, Text: text}
		if kind == screen.EventNotification {
			ev.App, ev.Text = d.postingApp(ctx, text)
			posted[o.key] = ev.App
		}
		evs = append(evs, ev)
	}
	sw.mu.Lock()
	sw.posted = posted
	sw.mu.Unlock()
	for _, o := range over {
		kind := overlayKind(o.els)
		if kind == screen.EventToast {
			continue // a status banner, gone in a moment
		}
		r := overlayRect(o, scale, w, h)
		if !r.Empty() {
			covered = append(covered, r)
		}
		switch kind {
		case screen.EventDialog:
			dialog = append(dialog, o.els...)
		case screen.EventNotification:
			if e, ok := notificationElement(o, r, posted[o.key]); ok {
				notes = append(notes, e)
			}
		}
	}
	return evs, covered, dialog, notes
}

// notificationElement is the element that stands for a notification
// SpringBoard shows over the app: its whole text, its app's name first, as
// a button (a tap opens the app), at its place r; app is the bundle id of
// the app that posted it, if known. A banner whose frames are another
// process's has no place: the gateway then withholds a screenshot of a
// blocked app's.
func notificationElement(o sbOverlay, r screen.Rect, app string) (screen.Element, bool) {
	text := overlayText(o.els)
	if text == "" {
		return screen.Element{}, false
	}
	return screen.Element{Role: screen.RoleButton, Label: text, Bounds: r, Source: screen.SourceTree,
		Path: springBoard, ID: screen.NotificationID(app)}, true
}

// dialogElements turns the elements of SpringBoard's alerts into screen
// elements, as children of SpringBoard's application element.
func dialogElements(app *axpObject, els []raxElement, scale float64, w, h int) []screen.Element {
	if app == nil || len(els) == 0 {
		return nil
	}
	tree := make([]raxElement, 0, len(els)+1)
	tree = append(tree, raxElement{Object: app, Parent: -1})
	base := 1
	for _, e := range els {
		if e.Parent < 0 {
			base = len(tree) // a new overlay's top element
			e.Parent = 0
		} else {
			e.Parent += base
		}
		tree = append(tree, e)
	}
	out, _ := raxScreenElements(tree, scale, w, h, springBoard)
	return out
}

// postingApp finds the app a banner names first ("Messages, Anna, Lunch?"),
// and returns its bundle id, which blocks and filters go by, and the rest
// of the text. Without one the text stays whole.
func (d *Device) postingApp(ctx context.Context, text string) (string, string) {
	name, rest, ok := strings.Cut(text, ", ")
	if !ok {
		return "", text
	}
	if app, found := d.appByName(ctx, name); found {
		return app.ID, rest
	}
	return "", text
}
