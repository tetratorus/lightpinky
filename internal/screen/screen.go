// Package screen defines the screen model that every driver produces and
// every agent reads: one element per line, with refs that stay the same
// while an element keeps its identity.
package screen

import (
	"fmt"
	"strings"
	"time"
)

// Point is a position in screen pixels.
type Point struct{ X, Y int }

// Rect is an axis-aligned rectangle in screen pixels.
type Rect struct{ X, Y, W, H int }

// Center returns the rectangle's center point.
func (r Rect) Center() Point { return Point{X: r.X + r.W/2, Y: r.Y + r.H/2} }

// Empty reports whether the rectangle has no area.
func (r Rect) Empty() bool { return r.W <= 0 || r.H <= 0 }

// Contains reports whether p lies inside the rectangle.
func (r Rect) Contains(p Point) bool {
	return p.X >= r.X && p.X < r.X+r.W && p.Y >= r.Y && p.Y < r.Y+r.H
}

// Source says where an element came from.
type Source string

// Element sources.
const (
	SourceTree Source = "tree"
	SourceOCR  Source = "ocr"
)

// Element states that drivers report. Drivers may add others.
const (
	StateFocused    = "focused"
	StateSelected   = "selected"
	StateChecked    = "checked"
	StateDisabled   = "disabled"
	StateScrollable = "scrollable"
	StateEditable   = "editable"
	StatePassword   = "password"
)

// Normalized roles. Drivers map platform classes onto these.
const (
	RoleButton   = "button"
	RoleInput    = "input"
	RoleText     = "text"
	RoleImage    = "image"
	RoleList     = "list"
	RoleSwitch   = "switch"
	RoleCheckbox = "checkbox"
	RoleTab      = "tab"
	RoleLink     = "link"
	RoleOther    = "other"
)

// Element is one item on the screen.
type Element struct {
	// Ref is assigned by Refs; 0 means unassigned. OCR elements have none.
	Ref    int
	Role   string
	Label  string
	Bounds Rect
	States []string
	Source Source
	// Path is the element's ancestry signature, without the element itself.
	// Drivers should prefer class names and resource ids over sibling
	// indices, so that the signature survives unrelated layout changes.
	// It identifies elements and is never shown to agents.
	Path string
	// ID is the platform's resource id or accessibility identifier, if any.
	ID string
	// Masked is set when Pinky masked a one-time code in Label (policy);
	// pictures of the screen cover Bounds. It is never shown to agents.
	Masked bool
	// Hidden is set on the element that stands in for what Pinky hid from
	// agents on another app's screen: a blocked app's notification in the
	// shade, or its card in recent apps. Label says so; pictures of the
	// screen cover Bounds, and are not taken when Bounds is empty.
	Hidden bool
}

// notificationID starts the ID of an element that stands for a
// notification shown over the app, which has no resource id of its own:
// an iPhone's banner, which SpringBoard draws.
const notificationID = "pinky:notification"

// NotificationID returns the ID of an element that stands for a
// notification shown over the app, posted by app (a bundle id or package;
// "" when the driver cannot tell). Its label starts with the app's name,
// and the gateway hides the notifications of blocked apps.
func NotificationID(app string) string {
	if app == "" {
		return notificationID
	}
	return notificationID + ":" + app
}

// NotificationApp reports whether an element's ID is a NotificationID,
// and returns the app that posted the notification, if known.
func NotificationApp(id string) (app string, ok bool) {
	if id == notificationID {
		return "", true
	}
	app, ok = strings.CutPrefix(id, notificationID+":")
	return app, ok
}

// Has reports whether the element is in the given state.
func (e Element) Has(state string) bool {
	for _, s := range e.States {
		if s == state {
			return true
		}
	}
	return false
}

// Line renders the element the way agents read it:
// `[ref] role "label" @x,y state...`, or `ocr "text" @x,y`.
func (e Element) Line() string {
	c := e.Bounds.Center()
	var b strings.Builder
	if e.Source == SourceOCR {
		fmt.Fprintf(&b, "ocr %q", e.Label)
		if !e.Bounds.Empty() {
			fmt.Fprintf(&b, " @%d,%d", c.X, c.Y)
		}
		return b.String()
	}
	fmt.Fprintf(&b, "[%d] %s", e.Ref, e.Role)
	if e.Label != "" {
		fmt.Fprintf(&b, " %q", e.Label)
	}
	if !e.Bounds.Empty() {
		fmt.Fprintf(&b, " @%d,%d", c.X, c.Y)
	}
	for _, st := range e.States {
		b.WriteByte(' ')
		b.WriteString(st)
	}
	return b.String()
}

// EventKind is the kind of a transient event shown above the elements.
type EventKind string

// Event kinds.
const (
	EventToast        EventKind = "toast"
	EventNotification EventKind = "notification"
	EventDialog       EventKind = "dialog"
)

// Event is something that appeared since the last observation.
type Event struct {
	Kind EventKind
	App  string // the posting app, for notifications
	Text string
}

// Line renders the event, e.g. `toast: "Added to cart"`.
func (ev Event) Line() string {
	if ev.App != "" {
		return fmt.Sprintf("%s: %s · %q", ev.Kind, ev.App, ev.Text)
	}
	return fmt.Sprintf("%s: %q", ev.Kind, ev.Text)
}

// App identifies the foreground app.
type App struct {
	Name string // "Dinely"
	ID   string // package name or bundle id
}

// Screen is one observation of a device.
type Screen struct {
	// Width and Height are the pixel size of the coordinate space that
	// element bounds and agent taps use: the screenshot the agent would get.
	Width, Height int
	App           App
	Orientation   string // "portrait" or "landscape"
	Keyboard      bool
	Locked        bool
	Elements      []Element
	Events        []Event
	CapturedAt    time.Time
}

// Find returns the element with the given ref.
func (s *Screen) Find(ref int) (Element, bool) {
	for _, e := range s.Elements {
		if e.Ref == ref && ref != 0 {
			return e, true
		}
	}
	return Element{}, false
}

// Focused returns the focused element, if any.
func (s *Screen) Focused() (Element, bool) {
	for _, e := range s.Elements {
		if e.Has(StateFocused) {
			return e, true
		}
	}
	return Element{}, false
}

// Header renders the first line of the screen.
func (s *Screen) Header() string {
	parts := []string{fmt.Sprintf("screen %dx%d", s.Width, s.Height)}
	if s.Locked {
		parts = append(parts, "locked")
	}
	if s.App.Name != "" || s.App.ID != "" {
		app := s.App.Name
		switch {
		case app == "":
			app = s.App.ID
		case s.App.ID != "":
			app += " (" + s.App.ID + ")"
		}
		parts = append(parts, "app: "+app)
	}
	if s.Orientation != "" {
		parts = append(parts, s.Orientation)
	}
	if s.Keyboard {
		parts = append(parts, "keyboard shown")
	} else {
		parts = append(parts, "keyboard hidden")
	}
	if f, ok := s.Focused(); ok && f.Ref != 0 {
		parts = append(parts, fmt.Sprintf("focus [%d]", f.Ref))
	}
	return strings.Join(parts, " · ")
}

// Render returns the full text form agents read.
func (s *Screen) Render() string {
	var b strings.Builder
	b.WriteString(s.Header())
	for _, ev := range s.Events {
		b.WriteByte('\n')
		b.WriteString(ev.Line())
	}
	for _, e := range s.Elements {
		b.WriteByte('\n')
		b.WriteString(e.Line())
	}
	return b.String()
}
