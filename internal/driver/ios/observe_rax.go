package ios

import (
	"context"
	"fmt"
	"image"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/tetratorus/lightpinky/internal/driver"
	"github.com/tetratorus/lightpinky/internal/screen"
)

// XCUIElementType values (XCTest), as the AutomationType attribute gives them.
const (
	xcOther           = 1
	xcButton          = 9
	xcRadioButton     = 10
	xcCheckBox        = 12
	xcPopUpButton     = 14
	xcMenuButton      = 16
	xcToolbarButton   = 17
	xcKeyboard        = 19
	xcKey             = 20
	xcTabBar          = 22
	xcTable           = 26
	xcCollectionView  = 32
	xcSwitch          = 40
	xcToggle          = 41
	xcLink            = 42
	xcImage           = 43
	xcIcon            = 44
	xcSearchField     = 45
	xcScrollView      = 46
	xcStaticText      = 48
	xcTextField       = 49
	xcSecureTextField = 50
	xcTextView        = 52
	xcMenuItem        = 54
	xcIncrementArrow  = 59
	xcDecrementArrow  = 60
	xcCell            = 75
	xcTab             = 80
)

// raxRecheckFailures is how long a phone whose remoteAXService failed is
// left on the inspector path before Observe tries the service again.
const raxRecheckFailures = 30 * time.Second

// raxLimits bound one walk over remoteAXService.
var raxLimits = walkLimits{MaxElements: 400, Budget: 4 * time.Second}

// raxLimitsWiFi bound one walk over Wi-Fi, where each of its requests (one
// per node, one after the other) takes tens of milliseconds instead of a
// few, and more in a session that just started, while the app builds its
// accessibility tree: on an iPad Pro 11-inch the first read of a new
// session over Wi-Fi gave 5 to 17 of the 19 elements Settings showed, cut
// at 4 s, and later reads of the same session all 19 in about 2 s
// (2026-11-08).
var raxLimitsWiFi = walkLimits{MaxElements: 400, Budget: 8 * time.Second}

// walkLimits returns the bounds of a walk over remoteAXService on this
// phone's connection.
func (d *Device) walkLimits() walkLimits {
	if d.info.Connection == "wifi" {
		return raxLimitsWiFi
	}
	return raxLimits
}

// raxReady returns the remote accessibility session, connecting on first
// use. A phone that refused it is asked again only after a while, so every
// Observe on it does not pay for a failed connection.
func (d *Device) raxReady(ctx context.Context) (*raxSession, error) {
	d.svcMu.Lock()
	defer d.svcMu.Unlock()
	if d.rax != nil {
		if d.rax.alive() {
			return d.rax, nil
		}
		d.rax.Close()
		d.rax = nil
	}
	if d.raxErr != nil && time.Since(d.raxFailedAt) < raxRecheckFailures {
		return nil, d.raxErr
	}
	d.mu.Lock()
	closed := d.closed
	d.mu.Unlock()
	if closed {
		return nil, fmt.Errorf("device closed: %w", driver.ErrOffline)
	}
	s, err := dialRAX(ctx, d.l.entry)
	if err != nil {
		if ctx.Err() == nil {
			d.raxErr, d.raxFailedAt = err, time.Now()
		}
		return nil, err
	}
	d.rax, d.raxErr = s, nil
	return s, nil
}

// observeRAX reads the screen through remoteAXService: every element with
// its frame, from one walk. OCR runs only when asked for, to add text the
// tree does not have.
func (d *Device) observeRAX(ctx context.Context, opt driver.ObserveOptions, st *ObserveStats) (*screen.Screen, error) {
	start := time.Now()
	rax, err := d.raxReady(ctx)
	if err != nil {
		return nil, err
	}
	// What SpringBoard shows over the app, looked at in the background
	// while the app's tree comes (see springboard.go).
	sbDone := d.kickSpringBoard(rax)
	// The display's orientation, read while the tree comes: the person
	// may have turned the device since the last read.
	shown := make(chan struct{})
	go func() {
		defer close(shown)
		d.readDisplayNow(ctx)
	}()
	var locked bool
	lockKnown := make(chan struct{})
	go func() {
		defer close(lockKnown)
		locked, _ = d.Locked(ctx)
	}()
	front := make(chan *axpObject, 1)
	var els []raxElement
	var complete bool
	var walkErr error
	walked := make(chan struct{})
	go func() {
		defer close(walked)
		t := time.Now()
		var pushed bool
		els, complete, pushed, walkErr = d.raxTree(ctx, rax, &st.Trace, func(app *axpObject) {
			select {
			case front <- app:
			default:
			}
		})
		st.Walk = time.Since(t)
		if pushed {
			st.Source = "remoteax-push"
		}
	}()
	var lines []ocrLine
	var ocrErr error
	var shot image.Image // the screenshot, for what it does not show (capture.go)
	var pictured sync.WaitGroup
	var gate *captureGate
	if opt.OCR {
		// The picture waits until the read may take it (capturegate.go).
		gate = newCaptureGate(opt.MayCapture, func() {
			pictured.Add(1)
			go func() {
				defer pictured.Done()
				t := time.Now()
				png, pic, from, err := d.ocrPicture(ctx, opt)
				st.Shot, st.ShotFrom = time.Since(t), from
				if err != nil {
					ocrErr = err
					return
				}
				decoded := make(chan image.Image, 1)
				go func() {
					if pic != nil {
						decoded <- pic
						return
					}
					decoded <- decodeShot(png)
				}()
				t = time.Now()
				lines, ocrErr = recognizeText(png, ocrOptions{Languages: ocrLanguages})
				st.OCR = time.Since(t)
				shot = <-decoded
			}()
		})
	}
	asked := make(chan struct{})
	go func() {
		// Ask as soon as the frontmost app and the lock state are known.
		defer close(asked)
		if gate == nil || opt.MayCapture == nil {
			return
		}
		var app *axpObject
		select {
		case app = <-front:
		case <-walked:
			return
		}
		<-lockKnown
		apps, ok := d.appCandidates(ctx, app.PID, locked)
		if !ok {
			return
		}
		<-shown
		si := d.screen()
		var screens []*screen.Screen
		for _, a := range apps {
			screens = append(screens, &screen.Screen{Width: si.Width, Height: si.Height,
				Orientation: orientationOf(si.Width, si.Height), App: a, Locked: locked})
		}
		gate.early(screens)
	}()
	<-walked
	<-lockKnown
	<-asked
	<-shown
	si := d.screen()
	if len(els) == 0 {
		pictured.Wait() // a picture begun after the app is not left running
		if walkErr == nil {
			walkErr = errRAXTimeout
		}
		return nil, walkErr
	}
	st.WalkError = walkErr
	st.Complete = complete && walkErr == nil
	if st.Source == "" {
		st.Source = "remoteax"
	}

	els = d.markWebContent(ctx, els)
	app := els[0]
	s := &screen.Screen{
		Width: si.Width, Height: si.Height,
		Orientation: orientationOf(si.Width, si.Height),
		Locked:      locked,
		CapturedAt:  time.Now(),
	}
	s.App = d.screenApp(ctx, els, locked, func(format string, args ...any) {
		st.Trace = append(st.Trace, fmt.Sprintf(format, args...))
	})
	if gate != nil {
		use := gate.late(s)
		pictured.Wait()
		st.Capture, st.CaptureWait = gate.stats()
		if !use {
			lines, shot, ocrErr = nil, nil, nil
		}
		st.OCRError = ocrErr
	}
	scale := si.Scale
	if app.HasFrame && app.Frame.W > 0 {
		scale = float64(si.Width) / app.Frame.W
	}
	frontApp, _ := d.appForPID(ctx, uint64(app.Object.PID))
	frontSB := frontApp.ID == springBoard
	d.noteFront(app.Object, frontSB)
	var covered []screen.Rect
	var dialog, notes []screen.Element
	if !frontSB {
		if over, ok := d.springBoardOverlays(sbDone); ok && len(over) > 0 {
			var dlg []raxElement
			s.Events, covered, dlg, notes = d.overlayEvents(ctx, over, scale, si.Width, si.Height)
			dialog = dialogElements(d.springBoardElement(), dlg, scale, si.Width, si.Height)
			st.Trace = append(st.Trace, fmt.Sprintf("SpringBoard shows %d overlays over the app", len(over)))
		} else if ok {
			d.overlayEvents(ctx, nil, scale, si.Width, si.Height) // forget the ones gone
		}
	}
	s.Elements, s.Keyboard = raxScreenElements(els, scale, si.Width, si.Height, s.App.ID, covered...)
	// Notifications over the app come first, as they are on top.
	s.Elements = append(append(notes, s.Elements...), dialog...)
	if asleepRead(ctx, s, d.ScreenOn) {
		s.Locked = true
		lines, shot = nil, nil // a dark screen
		st.Trace = append(st.Trace, "the screen is off: read as locked")
	}
	if opt.OCR && len(lines) > 0 {
		if top, kb := keyboardFromOCR(lines); kb {
			s.Keyboard = true
			kept := lines[:0:0]
			for _, l := range lines {
				if l.Y < top {
					kept = append(kept, l)
				}
			}
			lines = kept
		}
		s.Elements = append(s.Elements, ocrExtras(s.Elements, lines, si.Width, si.Height)...)
	}
	if shot != nil {
		d.capture.mark(s, uncaptured(s.Elements, shot, lines, si.Width, si.Height))
	} else {
		d.capture.apply(s)
	}
	for _, e := range s.Elements {
		if e.Source == screen.SourceTree && !e.Bounds.Empty() {
			st.Placed++
		}
	}
	st.Elements = len(els) - 1
	st.OCRLines = len(lines)
	st.Total = time.Since(start)
	return s, nil
}

// asleepRead reports whether a read that found next to nothing, of a phone
// that does not read as locked, is of a phone asleep: its screen is off.
// The tree of a dark screen has the app's element and one more at most,
// and a phone without a passcode, or one that locks a while after its
// screen went off, reads as unlocked (seen on an iPad Pro 11-inch over
// Wi-Fi for 28 s, until it left the network: 2026-11-08). Such a read
// counts as locked, as the lock screen it shows once woken: an agent is
// told the phone is locked instead of getting an empty screen.
func asleepRead(ctx context.Context, s *screen.Screen, screenOn func(context.Context) (bool, error)) bool {
	if s.Locked || len(s.Elements) > 1 {
		return false
	}
	on, err := screenOn(ctx)
	return err == nil && !on
}

// AccessibilityPlatformTranslation roles seen on iOS 27 (AXPAttributeRole).
// Unlike AutomationType, every source has them, a first push included.
const (
	axpRoleButton     = 2
	axpRoleSwitch     = 3
	axpRoleOther      = 4 // a container or a status bar item
	axpRoleHeader     = 6 // a heading
	axpRoleImage      = 7
	axpRoleTab        = 9  // an item of iOS 27's floating tab bar, value 1 when selected
	axpRoleAdjustable = 13 // a picker wheel, a page indicator
	axpRoleText       = 14
	axpRoleTextField  = 15 // text and search fields
)

// traitAdjustable is UIAccessibilityTraitAdjustable: picker wheels,
// sliders and steppers change with a swipe up or down.
const traitAdjustable = 1 << 12

// stateAdjustable marks an element that a vertical swipe on it changes: a
// picker wheel, a slider, a stepper.
const stateAdjustable = "adjustable"

// xcPickerWheel is XCUIElementTypePickerWheel.
const xcPickerWheel = 39

// traitRemoteHost marks a view that shows another process's elements (the
// keyboard, for one). Observed on iOS 27; not public.
const traitRemoteHost = 1 << 34

// traitSecureText is UIKit's private UIAccessibilityTraitSecureTextField,
// which every secure text field carries, native or on a web page. Without
// an AutomationType (WebKit's text fields, elements a first push lists) it
// alone tells a password field from a plain one. Seen on Safari's password
// fields on iOS 27; the value is UIAccessibility.framework's (Mac Catalyst,
// macOS 27.2), whose bit 23 is the status bar element trait the status
// bar's items carry.
const traitSecureText = 1 << 24

const (
	// raxPushAfter: a push whose first tree arrives sooner than this after
	// an input may describe the screen before it. The phone pushes 1.1–1.5 s
	// after a change.
	raxPushAfter = time.Second
	// raxPushTrust is how long after Pinky's last input a pushed tree is
	// trusted. Later the screen may have changed on its own in ways the
	// phone does not push (a timer's text), so Observe walks; by then the
	// phone is not building a tree, so the walk is quick.
	raxPushTrust = 6 * time.Second
	// raxSecondPush is how long Observe waits for a first push's second
	// one when the first leaves an element unnamed.
	raxSecondPush = 1200 * time.Millisecond
	// raxEmptyWait: an app that has just launched can show its screen
	// before it lists any element; a walk that finds none waits this long
	// for the phone's push, then walks again.
	raxEmptyWait = 1500 * time.Millisecond
)

// raxTree returns the elements on screen: the tree the phone pushed after
// Pinky's last input, while that is recent, else a walk. A walk right after
// a change waits behind the tree the phone is building then (1–4 s on
// screens with many images), so after an input it walks and waits for that
// push at once, and takes whichever comes first. A push counts only when
// its first tree came at least raxPushAfter after the input and its app is
// the frontmost app the walk found first: a push can arrive late, after
// the next input, and then describes the screen before it. onApp, when
// set, learns the frontmost app as soon as the walk finds it, before the
// tree.
func (d *Device) raxTree(ctx context.Context, rax *raxSession, trace *[]string, onApp func(*axpObject)) ([]raxElement, bool, bool, error) {
	input := d.lastInputAt()
	trusted := time.Since(input) < raxPushTrust
	t0 := time.Now()
	note := func(format string, args ...any) {
		*trace = append(*trace, fmt.Sprintf("%v ", time.Since(t0).Round(time.Millisecond))+fmt.Sprintf(format, args...))
	}
	note("input %v ago, trusted %v", t0.Sub(input).Round(time.Millisecond), trusted)
	var stop atomic.Bool
	apps := make(chan *axpObject, 1)
	walked := walkDetached(ctx, rax, d.walkLimits(), &stop, func(app *axpObject) {
		if onApp != nil {
			onApp(app)
		}
		apps <- app
	})
	if !trusted {
		select {
		case r := <-walked:
			return r.els, r.complete, false, r.err
		case <-ctx.Done():
			stop.Store(true)
			return nil, false, false, ctx.Err()
		}
	}
	var app *axpObject
	t, next := rax.pushed()
	var first *raxTree // a matching first push still waiting for its second
	var second <-chan time.Time
	// consider returns the elements of a push to use now, or remembers a
	// first push. A first push lacks every element's type and identifier:
	// it is used only when walks or second pushes told them for all its
	// elements, which fill puts in; else Observe waits for the second push,
	// as it does for one that leaves an element unnamed.
	consider := func(t *raxTree) []raxElement {
		if t == nil || app == nil || t.appPID != app.PID || !t.start.After(input.Add(raxPushAfter)) || time.Since(input) >= raxPushTrust {
			return nil
		}
		if !t.first {
			note("push from %v after the input", t.start.Sub(input).Round(time.Millisecond))
			return t.els
		}
		els, known := rax.fill(t.els)
		if known && !t.unnamed() {
			note("first push of known elements, %v after the input", t.start.Sub(input).Round(time.Millisecond))
			return els
		}
		if first == nil || first.start != t.start {
			note("first push with new or unnamed elements, %v after the input", t.start.Sub(input).Round(time.Millisecond))
			first = t
			second = time.After(raxSecondPush)
		}
		return nil
	}
	var empty <-chan time.Time
	rewalked := false
	for {
		select {
		case r := <-walked:
			note("walk: %d elements, err %v", len(r.els), r.err)
			if r.err != nil || len(r.els) > 1 || rewalked {
				return r.els, r.complete, false, r.err
			}
			empty = time.After(raxEmptyWait)
			walked = nil
		case <-empty:
			note("walk again")
			walked = walkDetached(ctx, rax, d.walkLimits(), &stop, nil)
			empty = nil
			rewalked = true
		case app = <-apps:
			note("frontmost app pid %d", app.PID)
			if els := consider(t); els != nil {
				stop.Store(true)
				return els, true, true, nil
			}
		case <-next:
			t, next = rax.pushed()
			if els := consider(t); els != nil {
				stop.Store(true)
				return els, true, true, nil
			}
		case <-second:
			note("no second push; using the first")
			stop.Store(true)
			els, _ := rax.fill(first.els)
			return els, true, true, nil
		case <-ctx.Done():
			stop.Store(true)
			return nil, false, false, ctx.Err()
		}
	}
}

type walkResult struct {
	els      []raxElement
	complete bool
	err      error
}

// walkDetached walks with a context the caller's cancellation does not
// reach: a request cut short would leave its answer in flight and spoil
// the session. A caller that gives up sets stop, and the walk ends between
// two requests.
func walkDetached(ctx context.Context, rax *raxSession, lim walkLimits, stop *atomic.Bool, onApp func(*axpObject)) <-chan walkResult {
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), lim.Budget+raxRequestTimeout)
	ch := make(chan walkResult, 1)
	go func() {
		defer cancel()
		els, complete, err := rax.walkUntil(wctx, lim, stop, onApp)
		ch <- walkResult{els, complete, err}
	}()
	return ch
}

// raxScreenElements turns a walk into screen elements: the accessibility
// elements (containers only lead to them), with frames in screen pixels.
// Elements entirely off screen are left out, and so are those under what
// covers them: the keyboard, the tab bar, and covered, in pixels.
//
// Elements of other processes come with frames relative to the view that
// hosts them. A host is marked by traitRemoteHost and comes right before
// them in the walk; elements of a process with no such host (home screen
// widgets) cannot be placed and are left out, except those of web content
// (Web), which a browser lists without a host and with frames in screen
// points. A process whose elements are mostly single characters is the
// on-screen keyboard: it only marks the keyboard as shown.
func raxScreenElements(els []raxElement, scale float64, w, h int, appID string, covered ...screen.Rect) ([]screen.Element, bool) {
	if len(els) == 0 {
		return nil, false
	}
	appPID := els[0].Object.PID
	keyboard := false
	keyboardPID := map[int]bool{}
	chars := map[int]int{}
	for _, e := range els {
		if e.Object.PID != appPID && !e.Web && isKeyChar(e.Label) {
			chars[e.Object.PID]++ // a page's own one-letter links are no keys
		}
	}
	for pid, n := range chars {
		if n >= 8 {
			keyboardPID[pid] = true
			keyboard = true
		}
	}
	offset := map[int]axpRect{} // by process: its host's frame
	var host *raxElement        // the last host, until an element of the app follows it
	inKeyboard := make([]bool, len(els))
	inTabBar := make([]bool, len(els))
	paths := make([]string, len(els))
	var out []screen.Element
	var kb screen.Rect // the keyboard's keys, in pixels
	for i := range els {
		e := els[i]
		switch p := e.Parent; {
		case p > 0 && p < i:
			inKeyboard[i] = inKeyboard[p]
			inTabBar[i] = inTabBar[p]
			paths[i] = paths[p] + "/" + xcName(els[p])
		default: // the app, or right below it
			paths[i] = appID
		}
		switch e.Type {
		case xcKeyboard:
			inKeyboard[i] = true
			keyboard = true
		case xcTabBar:
			inTabBar[i] = true
		}
		switch {
		case e.Traits&traitRemoteHost != 0 && e.HasFrame:
			host = &els[i]
		case e.Object.PID == appPID:
			host = nil
		}
		placed := true // the frame is in screen points
		if pid := e.Object.PID; pid != appPID && e.HasFrame {
			if _, ok := offset[pid]; !ok && host != nil && fitsIn(e.Frame, host.Frame) {
				offset[pid] = host.Frame
			}
			hf, ok := offset[pid]
			if !ok && !keyboardPID[pid] && !e.Web {
				continue // no host to place it by
			}
			placed = ok || e.Web
			e.Frame.X += hf.X
			e.Frame.Y += hf.Y
		}
		if i == 0 || !e.IsElement || !e.Visible {
			continue
		}
		if inKeyboard[i] || keyboardPID[e.Object.PID] {
			keyboard = true
			if e.HasFrame && placed {
				kb = union(kb, pixelRect(e.Frame, scale, w, h))
			}
			continue
		}
		if !e.HasFrame || e.Traits&traitRemoteHost != 0 {
			continue
		}
		r := pixelRect(e.Frame, scale, w, h)
		if r.Empty() {
			continue
		}
		role, states := raxRole(e, inTabBar[i])
		label := raxLabel(e, role)
		if label == "" && role == screen.RoleOther {
			continue
		}
		out = append(out, screen.Element{
			Role: role, Label: label, Bounds: r, States: states,
			Source: screen.SourceTree, Path: paths[i], ID: e.Ident,
		})
	}
	var over []occluder
	if !kb.Empty() {
		over = append(over, occluder{r: kb})
	}
	for _, r := range covered {
		over = append(over, occluder{r: r})
	}
	if tb, ok := tabBar(out, scale, w, h); ok {
		over = append(over, tb)
	}
	return unoccluded(out, over, int(math.Round(minVisiblePoints*scale))), keyboard
}

// An occluder is something drawn over the app's own elements: the keyboard,
// or the tab bar that iOS 27 floats over the content. member says which
// elements belong to it, and so are not under it.
type occluder struct {
	r      screen.Rect
	member func(screen.Element) bool
}

// minVisiblePoints is the least height and width, in points, of the part
// of a covered element that is left to tap.
const minVisiblePoints = 8

// tabBar finds a tab bar at the bottom of the screen from its tabs: iOS 27
// lists the items of its floating tab bar without the bar, and rows scroll
// on behind it. The bar is the tabs' box with the capsule's margin, down to
// the bottom edge: below the capsule lies the home indicator, where a
// swipe goes home.
func tabBar(els []screen.Element, scale float64, w, h int) (occluder, bool) {
	var r screen.Rect
	for _, e := range els {
		if e.Role == screen.RoleTab && e.Bounds.Center().Y > h*7/10 {
			r = union(r, e.Bounds)
		}
	}
	if r.Empty() {
		return occluder{}, false
	}
	m := int(math.Round(tabBarMargin * scale))
	r = clipRect(screen.Rect{X: r.X - m, Y: r.Y - m, W: r.W + 2*m, H: h - (r.Y - m)}, w, h)
	in := r
	return occluder{r: r, member: func(e screen.Element) bool {
		return e.Role == screen.RoleTab && in.Contains(e.Bounds.Center())
	}}, true
}

// tabBarMargin is how far, in points, iOS 27's tab bar capsule reaches
// beyond its items.
const tabBarMargin = 6

// unoccluded drops the elements whose middle lies under an occluder,
// unless a good part of them stays uncovered (a quarter of it, at least min
// pixels each way), which then becomes their bounds: a tap by ref goes to
// the middle of the bounds, which must not land on the tab bar or a key.
func unoccluded(els []screen.Element, over []occluder, min int) []screen.Element {
	if len(over) == 0 {
		return els
	}
	out := make([]screen.Element, 0, len(els))
	for _, e := range els {
		b, keep := e.Bounds, true
		for _, o := range over {
			if (o.member != nil && o.member(e)) || !o.r.Contains(b.Center()) {
				continue
			}
			b = largestOutside(b, o.r)
			if b.W < min || b.H < min || 4*b.W*b.H < e.Bounds.W*e.Bounds.H {
				keep = false
				break
			}
		}
		if keep {
			e.Bounds = b
			out = append(out, e)
		}
	}
	return out
}

// largestOutside is the largest part of b that o leaves uncovered: the
// strip above, below, left or right of o.
func largestOutside(b, o screen.Rect) screen.Rect {
	cands := []screen.Rect{
		{X: b.X, Y: b.Y, W: b.W, H: o.Y - b.Y},                     // above
		{X: b.X, Y: o.Y + o.H, W: b.W, H: b.Y + b.H - (o.Y + o.H)}, // below
		{X: b.X, Y: b.Y, W: o.X - b.X, H: b.H},                     // left
		{X: o.X + o.W, Y: b.Y, W: b.X + b.W - (o.X + o.W), H: b.H}, // right
	}
	var best screen.Rect
	for _, c := range cands {
		if c.W > 0 && c.H > 0 && c.W*c.H > best.W*best.H {
			best = c
		}
	}
	return best
}

// union is the smallest rectangle holding a and b; an empty one is ignored.
func union(a, b screen.Rect) screen.Rect {
	switch {
	case a.Empty():
		return b
	case b.Empty():
		return a
	}
	x0, y0 := min(a.X, b.X), min(a.Y, b.Y)
	x1, y1 := max(a.X+a.W, b.X+b.W), max(a.Y+a.H, b.Y+b.H)
	return screen.Rect{X: x0, Y: y0, W: x1 - x0, H: y1 - y0}
}

// clipRect clips r to a w x h screen.
func clipRect(r screen.Rect, w, h int) screen.Rect {
	x0, y0 := max(r.X, 0), max(r.Y, 0)
	x1, y1 := min(r.X+r.W, w), min(r.Y+r.H, h)
	if x1 <= x0 || y1 <= y0 {
		return screen.Rect{}
	}
	return screen.Rect{X: x0, Y: y0, W: x1 - x0, H: y1 - y0}
}

// isKeyChar reports whether a label is one letter or digit, as keyboard keys
// are labelled.
func isKeyChar(label string) bool {
	rs := []rune(strings.TrimSpace(label))
	return len(rs) == 1 && (unicode.IsLetter(rs[0]) || unicode.IsDigit(rs[0]))
}

// fitsIn reports whether a frame relative to a host lies within the host's
// size, give or take a point.
func fitsIn(f, host axpRect) bool {
	return f.X >= -1 && f.Y >= -1 && f.X+f.W <= host.W+1 && f.Y+f.H <= host.H+1
}

// xcName names an ancestor in an element's path: its identifier when it has
// one, else its type.
func xcName(e raxElement) string {
	if e.Ident != "" {
		return e.Ident
	}
	return "t" + itoa(e.Type)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// pixelRect converts a frame in points to screen pixels, clipped to the
// screen.
func pixelRect(f axpRect, scale float64, w, h int) screen.Rect {
	x0 := int(math.Round(f.X * scale))
	y0 := int(math.Round(f.Y * scale))
	x1 := int(math.Round((f.X + f.W) * scale))
	y1 := int(math.Round((f.Y + f.H) * scale))
	x0, y0 = max(x0, 0), max(y0, 0)
	x1, y1 = min(x1, w), min(y1, h)
	if x1 <= x0 || y1 <= y0 {
		return screen.Rect{}
	}
	return screen.Rect{X: x0, Y: y0, W: x1 - x0, H: y1 - y0}
}

// raxRole maps an element's XCUIElementType, or its traits when the type
// says nothing, to a role and states.
func raxRole(e raxElement, inTabBar bool) (string, []string) {
	var role string
	var states []string
	checked := func() {
		if v := strings.ToLower(e.Value); v == "1" || v == "on" || v == "开" {
			states = append(states, screen.StateChecked)
		}
	}
	switch e.Type {
	case xcButton, xcPopUpButton, xcMenuButton, xcToolbarButton, xcMenuItem, xcKey, xcIcon, xcCell, xcIncrementArrow, xcDecrementArrow:
		role = screen.RoleButton
	case xcRadioButton, xcCheckBox:
		role = screen.RoleCheckbox
		checked()
	case xcSwitch, xcToggle:
		role = screen.RoleSwitch
		checked()
	case xcLink:
		role = screen.RoleLink
	case xcImage:
		role = screen.RoleImage
	case xcSearchField, xcTextField, xcTextView:
		role = screen.RoleInput
		states = append(states, screen.StateEditable)
	case xcSecureTextField:
		role = screen.RoleInput
		states = append(states, screen.StateEditable, screen.StatePassword)
	case xcStaticText:
		role = screen.RoleText
	case xcTab:
		role = screen.RoleTab
	case xcTable, xcCollectionView, xcScrollView:
		role = screen.RoleList
	default:
		role, states = axRole(e.Traits, nil, e.Value)
		// A first pushed tree has no AutomationType; the translator's own
		// role tells what traits do not (seen on iOS 27).
		switch e.AXRole {
		case axpRoleSwitch:
			role, states = screen.RoleSwitch, nil
			checked()
		case axpRoleTextField:
			role = screen.RoleInput
			states = appendState(states, screen.StateEditable)
			if e.Traits&traitSecureText != 0 {
				states = appendState(states, screen.StatePassword)
			}
		case axpRoleImage:
			if role == screen.RoleOther {
				role = screen.RoleImage
			}
		case axpRoleButton:
			if role == screen.RoleOther {
				role = screen.RoleButton
			}
		case axpRoleText, axpRoleHeader:
			if role == screen.RoleOther {
				role = screen.RoleText
			}
		}
	}
	// iOS 27's floating tab bar lists its items as buttons, outside any tab
	// bar, with the translator's tab role.
	if e.AXRole == axpRoleTab && (role == screen.RoleButton || role == screen.RoleOther) {
		role = screen.RoleTab
	}
	if e.Traits&traitAdjustable != 0 || e.Type == xcPickerWheel || e.AXRole == axpRoleAdjustable {
		states = appendState(states, stateAdjustable)
	}
	if role == screen.RoleOther && e.IdentValue {
		role = screen.RoleText // shows a value and nothing else
	}
	if inTabBar && role == screen.RoleButton {
		role = screen.RoleTab
	}
	if role == screen.RoleInput && e.Focused {
		states = append(states, screen.StateFocused)
	}
	if e.Selected || e.Traits&traitSelected != 0 {
		states = appendState(states, screen.StateSelected)
	}
	if !e.Enabled || e.Traits&traitNotEnabled != 0 {
		states = appendState(states, screen.StateDisabled)
	}
	return role, states
}

func appendState(states []string, s string) []string {
	for _, x := range states {
		if x == s {
			return states
		}
	}
	return append(states, s)
}

// raxLabel is what an agent reads for the element: its label, then its
// value as VoiceOver reads it, except for switches and check boxes whose
// value is a state, and tabs whose value says whether they are selected.
// The value VoiceOver reads is the value description when there is one:
// a picker wheel's value is the index of its row in a long looping list
// ("5003"), its description the row ("12 o’clock").
func raxLabel(e raxElement, role string) string {
	label := strings.TrimSpace(e.Label)
	value := strings.TrimSpace(e.ValueText)
	if value == "" {
		value = strings.TrimSpace(e.Value)
	}
	if role == screen.RoleSwitch || role == screen.RoleCheckbox || (role == screen.RoleTab && (value == "0" || value == "1")) {
		value = ""
	}
	if label == "" {
		label, value = value, ""
	}
	if label == "" {
		label = strings.TrimSpace(e.Placeholder)
	}
	if label == "" {
		label = e.Ident
	}
	if value != "" && value != label {
		label += ", " + value
	}
	return label
}
