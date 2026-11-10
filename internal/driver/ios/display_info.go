package ios

import (
	"context"
	"fmt"
	"math"
	"strings"
)

const (
	deviceInfoService     = "com.apple.coredevice.deviceinfo"
	featureGetDisplayInfo = "com.apple.coredevice.feature.getdisplayinfo"
)

// screenInfo describes the main display, as CoreDevice reads it.
//
// Orientation, measured on 2026-11-08 on an iPhone 13 mini and an iPad Pro
// 11-inch (3rd generation), iOS 27.0.1, in every orientation each allows:
//
//   - Width and Height are the display's own frame, the way its panel is
//     mounted, whatever way the device is turned: 1125x2436 on the iPhone,
//     2388x1668 on the iPad, whose panel is landscape.
//   - Orientation (currentOrientation) is how far the interface is turned
//     from that frame, clockwise: the video is in the display's frame and
//     turns upright by it.
//   - Native (nativeOrientation) is how far the display's frame is turned
//     from the device held the usual way up (rot0 on the iPhone, rot270 on
//     the iPad). The touch panel's 0..65535 range is in that upright-held
//     frame, so a touch turns by both.
//   - Screenshots and remoteAXService's frames are the interface's, the
//     right way up, and so are Pinky's coordinates (screen).
type screenInfo struct {
	// Width and Height are the size UIKit renders at (points times scale),
	// in the display's own frame. The panel itself may be slightly smaller
	// (1080x2340 on an iPhone 13 mini, against 1125x2436 rendered).
	Width, Height int
	Scale         float64 // pixels per point
	Orientation   string  // rot0, rot90, rot180, rot270: the interface's turn from the display's frame
	Native        string  // rot0, ...: the display's turn from the device held the usual way up
	Backlight     string  // activeOn, off, ...
	// DisplayID is the display's CoreDevice id (displayId), which a stream
	// asks for; 0 when the reply names none.
	DisplayID int
	UniqueID  string // its uniqueId, which a screenshot may ask for; "" when the reply names none
	// Key tells the display from the phone's others across reads
	// (displayEntry.key): when it changes, the phone moved to another one.
	Key string
	Lit int // as displayEntry's
}

// rotQuarter reads rot0, rot90, rot180 and rot270 as quarter turns
// clockwise; anything else is none.
func rotQuarter(rot string) quarter {
	switch rot {
	case "rot90":
		return 1
	case "rot180":
		return 2
	case "rot270":
		return 3
	}
	return 0
}

// turn is how far the interface is turned from the display's frame,
// clockwise: what puts the video upright.
func (s screenInfo) turn() quarter { return rotQuarter(s.Orientation) }

// touchTurn is how far the interface is turned from the touch panel's
// frame, the device held the usual way up.
func (s screenInfo) touchTurn() quarter {
	return (rotQuarter(s.Native) + s.turn()) & 3
}

// upright returns the screen the right way up: the size swapped when the
// interface is turned a quarter from the display's frame.
func (s screenInfo) upright() screenInfo {
	s.Width, s.Height = turnSize(s.Width, s.Height, s.turn())
	return s
}

// orientationOf names the orientation of a screen the right way up.
func orientationOf(w, h int) string {
	if w > h {
		return "landscape"
	}
	return "portrait"
}

// Several displays. A foldable iPhone (iPhone Duo, iOS 27) has a display
// inside and one outside, and getdisplayinfo lists both. CoreDevice's own
// model of a display (CoreDevice.Display in Xcode 27.2's
// CoreDeviceUtilities, whose Codable keys are its property names) carries
// displayId, uniqueId, bounds, frame, pointScale, currentOrientation,
// nativeOrientation, primary, active, backlightState (off, activeOn,
// activeDimmed, inactiveOn), external, type and the display's modes. Pinky
// shows the display the person looks at: a lit one (active, or a lit
// backlight when active is not said), the primary one among several lit
// ones, and the primary one when none is lit. A display of another type
// than integrated (external, wireless, virtual: the phone makes a wireless
// one for a video stream) is never the phone's own screen, and only the
// phone's own displays make it a phone with several. No foldable has been
// tried: which display a folded Duo calls primary, and whether active
// follows the fold, are not known yet.
//
// The replies seen from phones with one display (iOS 27.0 to 27.2) name no
// displayId, so a foldable's may not either, nor list both its displays.
// What is known (GitHub issue #32): a Duo folded, its outer display on,
// streams display 1; unfolded, display 1 brings no picture. So when the
// display in use has no id, or a phone with two screens (twoScreens) lists
// one, the stream tries ids 1, 2 and 3 and remembers which brought the
// display's picture (Device.good), and a fold shows as another display
// (key: its uniqueId, else its id and size) being the one in use.

// displayEntry is one display of a getdisplayinfo reply.
type displayEntry struct {
	ID            int    // displayId; 0 when the reply names none
	UniqueID      string // uniqueId; "" when the reply names none
	Width, Height int    // its bounds, in the display's own frame
	Primary       bool
	External      bool
	Kind          string // its type, lower case: integrated, external, wireless, virtual, dfr, unknown; "" when not said
	// Lit is 1 for a display that is on, -1 for one that is off and 0 when
	// the reply does not say.
	Lit       int
	Backlight string
	m         map[string]any
}

// displayIDKeys are the keys a display's id may come under: CoreDevice's
// own (displayId), then spellings other versions may use.
var displayIDKeys = []string{"displayId", "displayID", "id"}

// parseDisplays reads every display of a getdisplayinfo reply, in its order.
func parseDisplays(out map[string]any) []displayEntry {
	list, _ := out["displays"].([]any)
	var ds []displayEntry
	for _, d := range list {
		m, ok := d.(map[string]any)
		if !ok {
			continue
		}
		e := displayEntry{m: m}
		for _, k := range displayIDKeys {
			if v, ok := m[k]; ok {
				e.ID = int(toInt64(v))
				break
			}
		}
		for _, k := range []string{"uniqueId", "uniqueID"} {
			if v, ok := m[k].(string); ok && v != "" {
				e.UniqueID = v
				break
			}
		}
		e.Width, e.Height = boundsSize(m)
		e.Primary, _ = m["primary"].(bool)
		e.External, _ = m["external"].(bool)
		e.Kind = displayKind(m["type"])
		e.Backlight, _ = m["backlightState"].(string)
		if a, ok := m["active"].(bool); ok {
			e.Lit = -1
			if a {
				e.Lit = 1
			}
		} else if e.Backlight != "" {
			e.Lit = -1
			if e.Backlight == backlightOn || e.Backlight == backlightDimmed {
				e.Lit = 1
			}
		}
		ds = append(ds, e)
	}
	return ds
}

// displayKind reads a display's type: a string, or an enum case encoded as
// a map with the case's name as its only key.
func displayKind(v any) string {
	switch t := v.(type) {
	case string:
		return strings.ToLower(t)
	case map[string]any:
		if len(t) == 1 {
			for k := range t {
				return strings.ToLower(k)
			}
		}
	}
	return ""
}

// own reports whether the display is one of the phone's own screens.
func (e displayEntry) own() bool {
	return !e.External && (e.Kind == "" || e.Kind == "integrated" || e.Kind == "unknown")
}

// several reports whether the phone has more than one screen of its own.
func several(ds []displayEntry) bool {
	n := 0
	for _, e := range ds {
		if e.own() {
			n++
		}
	}
	return n > 1
}

// idsKnown reports whether the reply lists several displays of the
// phone's own, each with an id of its own: then an id names one display.
func idsKnown(ds []displayEntry) bool {
	seen := map[int]bool{}
	for _, e := range ds {
		if !e.own() {
			continue
		}
		if e.ID <= 0 || seen[e.ID] {
			return false
		}
		seen[e.ID] = true
	}
	return len(seen) > 1
}

// key tells the display from the phone's others across reads: its
// uniqueId, else its id and its size, the shorter side first (a display's
// bounds are in its own frame, but turning the phone must never look like
// another display).
func (e displayEntry) key() string {
	if e.UniqueID != "" {
		return "u " + e.UniqueID
	}
	w, h := e.Width, e.Height
	if w > h {
		w, h = h, w
	}
	return fmt.Sprintf("%d %dx%d", e.ID, w, h)
}

// boundsSize reads a display's bounds, [[x, y], [w, h]].
func boundsSize(m map[string]any) (int, int) {
	if b, ok := m["bounds"].([]any); ok && len(b) == 2 {
		if wh, ok := b[1].([]any); ok && len(wh) == 2 {
			return int(toFloat(wh[0])), int(toFloat(wh[1]))
		}
	}
	return 0, 0
}

// rankDisplays orders the displays Pinky would show, best first: the
// phone's own displays before the others (own), lit before unknown before off, then the
// primary one, then the reply's order.
func rankDisplays(ds []displayEntry) []displayEntry {
	out := append([]displayEntry(nil), ds...)
	score := func(e displayEntry) int {
		s := 0
		if e.own() {
			s += 8
		}
		s += 2 * (e.Lit + 1)
		if e.Primary {
			s++
		}
		return s
	}
	// Insertion sort keeps the reply's order among equals.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && score(out[j]) > score(out[j-1]); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// chooseDisplay returns the display Pinky shows (see rankDisplays). Without
// a usable display it returns false.
func chooseDisplay(ds []displayEntry) (displayEntry, bool) {
	for _, e := range rankDisplays(ds) {
		if e.Width > 0 && e.Height > 0 {
			return e, true
		}
	}
	return displayEntry{}, false
}

// anyLit reports whether a display of the phone's own says it is on.
func anyLit(ds []displayEntry) bool {
	for _, e := range ds {
		if e.Lit > 0 && e.own() {
			return true
		}
	}
	return false
}

// defaultDisplayID is the display a stream asks for when the phone has one
// display, as Xcode and go-ios do: the main display.
const defaultDisplayID = 1

// streamDisplays returns the display ids to ask a stream of, best first.
// A phone with one display of its own gets the default id first, as
// before Pinky knew of several, then that display's id when it is another.
// A phone with several, or with two screens (two) whatever the reply lists,
// gets shown first (the id of the display Pinky shows, 0 for none), then
// its other displays in rankDisplays' order, then the default id when no
// display has it; and when the ids do not tell its screens apart (a
// display of its own without one, or one listed of two), ids 2 and 3 too.
// Other displays (an external screen, the stream's own wireless display)
// are never asked for.
func streamDisplays(ds []displayEntry, shown int, two bool) []int {
	var ids []int
	add := func(id int) {
		if id <= 0 {
			return
		}
		for _, x := range ids {
			if x == id {
				return
			}
		}
		ids = append(ids, id)
	}
	if !several(ds) && !two {
		add(defaultDisplayID)
		for _, e := range ds {
			if e.own() {
				add(e.ID)
			}
		}
		return ids
	}
	add(shown)
	unsure := !several(ds)
	for _, e := range rankDisplays(ds) {
		if e.own() {
			add(e.ID)
			unsure = unsure || e.ID <= 0
		}
	}
	add(defaultDisplayID)
	if unsure {
		add(2)
		add(3)
	}
	return ids
}

// describeDisplays sums up a reply's displays for logs and the checkup:
// "2 displays: id 1 2200x2480 primary off, id 2 1080x2400 lit".
func describeDisplays(ds []displayEntry) string {
	if len(ds) == 0 {
		return "no displays listed"
	}
	var b strings.Builder
	if len(ds) == 1 {
		b.WriteString("1 display: ")
	} else {
		fmt.Fprintf(&b, "%d displays: ", len(ds))
	}
	for i, e := range ds {
		if i > 0 {
			b.WriteString(", ")
		}
		if e.ID > 0 {
			fmt.Fprintf(&b, "id %d ", e.ID)
		} else {
			b.WriteString("no id ")
		}
		fmt.Fprintf(&b, "%dx%d", e.Width, e.Height)
		if e.Primary {
			b.WriteString(" primary")
		}
		if e.External {
			b.WriteString(" external")
		}
		if e.Kind != "" && e.Kind != "integrated" && !(e.External && e.Kind == "external") {
			b.WriteString(" " + e.Kind)
		}
		switch e.Lit {
		case 1:
			b.WriteString(" lit")
		case -1:
			b.WriteString(" off")
		}
		if e.Backlight != "" {
			b.WriteString(" (" + e.Backlight + ")")
		}
	}
	return b.String()
}

// readDisplay returns the entry of CoreDevice's display info for the
// display Pinky shows (chooseDisplay), as the phone sends it.
func readDisplay(ctx context.Context, svc *cdService) (map[string]any, error) {
	out, err := svc.invoke(ctx, featureGetDisplayInfo, "", nil)
	if err != nil {
		return nil, err
	}
	e, ok := chooseDisplay(parseDisplays(out))
	if !ok {
		return nil, fmt.Errorf("display info: no main display")
	}
	return e.m, nil
}

// displayInfo is what a getdisplayinfo reply says: the display Pinky
// shows, and every display the phone listed.
type displayInfo struct {
	Screen   screenInfo
	Displays []displayEntry
	out      map[string]any // the reply
}

// screenOfKey describes the listed display with key k.
func (di displayInfo) screenOfKey(k string) (screenInfo, bool) {
	for _, e := range di.Displays {
		if k != "" && e.key() == k && e.Width > 0 && e.Height > 0 {
			return screenOf(e, di.out), true
		}
	}
	return screenInfo{}, false
}

// pictureOf returns the display of the phone's own that a stream's picture
// of w x h shows, by its shape: the ratio of its sides within 3% of the
// picture's, and no other display's so. The id a stream asks for may name
// no listed display, or another than the reply's id would say; the
// picture tells. A foldable's two displays differ in shape far more.
func pictureOf(ds []displayEntry, w, h int) (displayEntry, bool) {
	if w <= 0 || h <= 0 {
		return displayEntry{}, false
	}
	ratio := func(a, b int) float64 {
		if a > b {
			a, b = b, a
		}
		return float64(a) / float64(b)
	}
	r := ratio(w, h)
	var found displayEntry
	n := 0
	for _, e := range ds {
		if e.own() && e.Width > 0 && e.Height > 0 && math.Abs(ratio(e.Width, e.Height)-r) <= 0.03*r {
			found = e
			n++
		}
	}
	return found, n == 1
}

// screenFor describes the listed display with id id.
func (di displayInfo) screenFor(id int) (screenInfo, bool) {
	for _, e := range di.Displays {
		if e.ID == id && id > 0 && e.Width > 0 && e.Height > 0 {
			return screenOf(e, di.out), true
		}
	}
	return screenInfo{}, false
}

func readScreenInfo(ctx context.Context, svc *cdService) (screenInfo, error) {
	di, err := readDisplayInfo(ctx, svc, "")
	return di.Screen, err
}

// readDisplayInfo reads the phone's displays; keep is as parseDisplayInfo
// takes it.
func readDisplayInfo(ctx context.Context, svc *cdService, keep string) (displayInfo, error) {
	out, err := svc.invoke(ctx, featureGetDisplayInfo, "", nil)
	if err != nil {
		return displayInfo{}, err
	}
	return parseDisplayInfo(out, keep)
}

// parseScreenInfo reads the display Pinky shows from a getdisplayinfo
// reply.
func parseScreenInfo(out map[string]any) (screenInfo, error) {
	di, err := parseDisplayInfo(out, "")
	return di.Screen, err
}

// parseDisplayInfo reads a getdisplayinfo reply: the display Pinky shows
// (chooseDisplay) and the list. keep is the key of the display Pinky
// showed so far ("": none): while no display of several is lit (the phone
// asleep or locked), Pinky stays on it rather than move to the primary
// one, and moves when one lights up.
func parseDisplayInfo(out map[string]any, keep string) (displayInfo, error) {
	ds := parseDisplays(out)
	di := displayInfo{Displays: ds, out: out}
	e, ok := chooseDisplay(ds)
	if keep != "" && several(ds) && !anyLit(ds) {
		for _, k := range ds {
			if k.key() == keep && k.Width > 0 && k.Height > 0 {
				e, ok = k, true
				break
			}
		}
	}
	if !ok {
		if len(ds) > 0 {
			return di, fmt.Errorf("display info: no size for the main display")
		}
		return di, fmt.Errorf("display info: no main display")
	}
	di.Screen = screenOf(e, out)
	return di, nil
}

// screenOf describes one display of a reply.
func screenOf(e displayEntry, out map[string]any) screenInfo {
	m := e.m
	si := screenInfo{Width: e.Width, Height: e.Height, Scale: 1, DisplayID: e.ID, UniqueID: e.UniqueID, Key: e.key(), Lit: e.Lit}
	if s := toFloat(m["pointScale"]); s > 0 {
		si.Scale = s
	}
	si.Orientation, _ = m["currentOrientation"].(string)
	si.Native, _ = m["nativeOrientation"].(string)
	si.Backlight = e.Backlight
	if si.Backlight == "" {
		// iOS 27.2 moved it next to "displays".
		si.Backlight, _ = out["backlightState"].(string)
	}
	return si
}

func toFloat(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case float32:
		return float64(n)
	default:
		return float64(toInt64(v))
	}
}
