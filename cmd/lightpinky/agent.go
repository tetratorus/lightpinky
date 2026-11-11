package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tetratorus/lightpinky/internal/driver"
	"github.com/tetratorus/lightpinky/internal/driver/ios"
	"github.com/tetratorus/lightpinky/internal/screen"
)

// view is what the agent last saw of one phone: refs stay the same between
// reads, and the last screen is what actions diff against and what
// coordinate taps are checked against.
type view struct {
	refs *screen.Refs
	mu   sync.Mutex
	seen *screen.Screen
}

func (v *view) last() *screen.Screen {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.seen
}

func (v *view) set(s *screen.Screen) {
	v.mu.Lock()
	v.seen = s
	v.mu.Unlock()
}

func (ph *phones) view(d driver.Device) *view {
	ph.viewMu.Lock()
	defer ph.viewMu.Unlock()
	if ph.views == nil {
		ph.views = map[string]*view{}
	}
	id := d.Info().ID
	v := ph.views[id]
	if v == nil {
		v = &view{refs: screen.NewRefs()}
		ph.views[id] = v
	}
	return v
}

// read observes the screen and numbers its elements. It reads text from
// the screen image when ocr is set or the tree has next to nothing in it.
func (ph *phones) read(ctx context.Context, d driver.Device, ocr bool) (*screen.Screen, error) {
	s, err := d.Observe(ctx, driver.ObserveOptions{OCR: ocr})
	if err == nil && !ocr && thin(s) {
		s, err = d.Observe(ctx, driver.ObserveOptions{OCR: true})
	}
	if err != nil {
		return nil, err
	}
	ph.view(d).refs.Assign(s)
	return s, nil
}

// thin reports a screen whose tree names fewer than 3 things: an app that
// draws its own text, which only the image shows.
func thin(s *screen.Screen) bool {
	n := 0
	for _, e := range s.Elements {
		if e.Label != "" {
			n++
		}
	}
	return n < 3 && !s.Locked
}

// step is an action on the phone. prev is the screen the agent last saw
// (nil when none), now the screen just before the action when the action
// asked to look first (else nil). It may return a note for the reply.
type step func(d driver.Device, prev, now *screen.Screen) (string, error)

// act runs an action, waits for the screen to settle and replies with what
// changed since the agent's last read (or the whole screen). look reads the
// screen before the action, for refs and the changed-screen check. check,
// when set, looks at the screens before and after the action.
func (ph *phones) act(ctx context.Context, in actIn, look bool, f step, check func(before, after *screen.Screen) (string, error)) *sdk.CallToolResult {
	var out string
	err := ph.with(ctx, in.Device, func(d driver.Device) error {
		v := ph.view(d)
		prev := v.last()
		var now *screen.Screen
		if look {
			var err error
			if now, err = ph.read(ctx, d, false); err != nil {
				return err
			}
		}
		note, err := f(d, prev, now)
		if err != nil {
			return err
		}
		msg := "ok"
		if in.NoSettle {
			out = withNote(msg, note)
			return nil
		}
		if s, ok := d.(driver.Settler); ok {
			msg = settleNote(s.WaitSettled(ctx, settleLimit))
		}
		if in.quiet && check == nil {
			out = withNote(msg, note)
			return nil
		}
		after, err := ph.read(ctx, d, in.OCR)
		if err != nil {
			out = withNote(msg, note) + "\n(could not read the screen after: " + err.Error() + ")"
			return nil
		}
		if check != nil {
			before := now
			if before == nil {
				before = prev
			}
			n, err := check(before, after)
			if err != nil {
				v.set(after)
				return err
			}
			if n != "" {
				note = strings.TrimSpace(note + " " + n)
			}
		}
		out = withNote(msg, note)
		if in.quiet {
			return nil
		}
		base := prev
		if base == nil || in.full {
			base = now
		}
		if in.full {
			base = nil
		}
		out += "\n" + diffScreens(base, after)
		v.set(after)
		return nil
	})
	if err != nil {
		return fail(err)
	}
	return text(out)
}

func withNote(msg, note string) string {
	if note == "" {
		return msg
	}
	return msg + " · " + note
}

// diffScreens is the new screen as text, or only the lines that changed
// since old when both show the same app the same way. Lines start with +
// (new), - (gone) or ~ (changed, as it reads now).
func diffScreens(old, s *screen.Screen) string {
	if old == nil || old.App.ID != s.App.ID || old.Width != s.Width || old.Height != s.Height ||
		old.Orientation != s.Orientation || old.Locked != s.Locked {
		return renderScreen(s)
	}
	type line struct {
		key, text string
	}
	// Text read from the image is compared only when both reads have it.
	ocr := hasOCR(old) && hasOCR(s)
	lines := func(s *screen.Screen) []line {
		var ls []line
		seen := map[string]bool{}
		n := map[string]int{}
		for _, e := range s.Elements {
			if e.Source == screen.SourceOCR && !ocr {
				continue
			}
			t := elementLine(e)
			if seen[plainLine(e)] {
				continue
			}
			seen[plainLine(e)] = true
			k := fmt.Sprintf("r%d", e.Ref)
			if e.Ref == 0 {
				// Text read from the image moves by a pixel or two
				// between reads: it changes only when the text does.
				k = fmt.Sprintf("o%s\x00%d", e.Label, n[e.Label])
				n[e.Label]++
			}
			ls = append(ls, line{k, t})
		}
		return ls
	}
	was := map[string]string{}
	for _, l := range lines(old) {
		was[l.key] = l.text
	}
	var b strings.Builder
	b.WriteString(s.Header())
	for _, ev := range s.Events {
		b.WriteString("\n" + ev.Line())
	}
	changed := 0
	is := map[string]bool{}
	nl := lines(s)
	for _, l := range nl {
		is[l.key] = true
		t, ok := was[l.key]
		switch {
		case !ok:
			b.WriteString("\n+ " + l.text)
		case t != l.text && l.key[0] == 'r':
			b.WriteString("\n~ " + l.text)
		default:
			continue
		}
		changed++
	}
	for _, l := range lines(old) {
		if !is[l.key] {
			b.WriteString("\n- " + l.text)
			changed++
		}
	}
	if changed == 0 {
		b.WriteString("\n(no change since your last read)")
	} else if changed > len(nl)*2/3 && len(nl) > 6 {
		// Most of it changed: the whole screen reads more easily.
		return renderScreen(s)
	}
	return b.String()
}

func hasOCR(s *screen.Screen) bool {
	for _, e := range s.Elements {
		if e.Source == screen.SourceOCR {
			return true
		}
	}
	return false
}

// targetIn picks where an action lands: an element by ref, else a point.
type targetIn struct {
	Ref   int  `json:"ref,omitempty" jsonschema:"an element's [number] from elements; used instead of coordinates"`
	Force bool `json:"force,omitempty" jsonschema:"act on the coordinates even when that part of the screen changed since your last read (e.g. a tab bar that stays put while the page changes)"`
}

// find is the element with this ref on the screen now.
func find(now *screen.Screen, ref int) (screen.Element, error) {
	e, ok := now.Find(ref)
	if !ok || e.Bounds.Empty() {
		return e, fmt.Errorf("[%d] is not on the screen now; read the screen again", ref)
	}
	return e, nil
}

// under is the smallest numbered element at p, leaving out ones that fill
// most of the screen (windows, scroll views).
func under(s *screen.Screen, p screen.Point) (screen.Element, bool) {
	var best screen.Element
	found := false
	for _, e := range s.Elements {
		r := e.Bounds
		if e.Ref == 0 || r.Empty() || !r.Contains(p) || r.W*r.H*2 > s.Width*s.Height {
			continue
		}
		if !found || r.W*r.H < best.Bounds.W*best.Bounds.H {
			best, found = e, true
		}
	}
	return best, found
}

// stale says why a point the agent picked from its last read (prev) may not
// land on the same thing now, or "" when it will.
func stale(prev, now *screen.Screen, p screen.Point) string {
	if prev == nil || now == nil {
		return ""
	}
	if prev.App.ID != now.App.ID {
		return fmt.Sprintf("the app in front changed from %s to %s", appName(prev.App), appName(now.App))
	}
	if prev.Width != now.Width || prev.Height != now.Height {
		return "the screen turned"
	}
	was, ok := under(prev, p)
	if !ok {
		return ""
	}
	is, ok := under(now, p)
	if ok && is.Ref == was.Ref && !moved(was.Bounds, is.Bounds) {
		return ""
	}
	if e, ok := now.Find(was.Ref); ok {
		if moved(was.Bounds, e.Bounds) {
			c := e.Bounds.Center()
			return fmt.Sprintf("%s moved to @%d,%d", short(was), c.X, c.Y)
		}
		if ok && is.Ref != was.Ref {
			return fmt.Sprintf("%s is now under that point instead of %s", short(is), short(was))
		}
		return ""
	}
	return fmt.Sprintf("%s is no longer on the screen", short(was))
}

// short names an element for a message: its ref, role and label.
func short(e screen.Element) string {
	s := fmt.Sprintf("[%d] %s", e.Ref, e.Role)
	if e.Label != "" {
		s += fmt.Sprintf(" %q", e.Label)
	}
	return s
}

func moved(a, b screen.Rect) bool {
	d := func(x, y int) bool { return x-y > 4 || y-x > 4 }
	return d(a.X, b.X) || d(a.Y, b.Y) || d(a.W, b.W) || d(a.H, b.H)
}

func appName(a screen.App) string {
	if a.Name != "" {
		return a.Name
	}
	if a.ID != "" {
		return a.ID
	}
	return "nothing"
}

// aim is where a targeted action lands: the ref's center, else (x, y)
// after checking it still points at what the agent saw there.
func aim(t targetIn, x, y int, prev, now *screen.Screen) (screen.Point, error) {
	if t.Ref != 0 {
		e, err := find(now, t.Ref)
		return e.Bounds.Center(), err
	}
	p := screen.Point{X: x, Y: y}
	if !t.Force {
		if why := stale(prev, now, p); why != "" {
			return p, fmt.Errorf("not done: since your last read, %s. Read the screen again, or pass force: true", why)
		}
	}
	return p, inside(now, p)
}

func inside(s *screen.Screen, p screen.Point) error {
	if s == nil || s.Width == 0 {
		return nil
	}
	if p.X < 0 || p.Y < 0 || p.X >= s.Width || p.Y >= s.Height {
		return fmt.Errorf("%d,%d is off the %dx%d screen", p.X, p.Y, s.Width, s.Height)
	}
	return nil
}

// region is the ref's bounds, or the whole screen.
func region(ref int, now *screen.Screen) (screen.Rect, error) {
	if ref != 0 {
		e, err := find(now, ref)
		return e.Bounds, err
	}
	return screen.Rect{W: now.Width, H: now.Height}, nil
}

// scrollPath is a swipe across most of r with the finger moving in dir.
func scrollPath(r screen.Rect, dir string) (from, to screen.Point, err error) {
	c := r.Center()
	lo := func(o, n int) int { return o + n/5 }
	hi := func(o, n int) int { return o + n*4/5 }
	switch dir {
	case "up":
		return screen.Point{X: c.X, Y: hi(r.Y, r.H)}, screen.Point{X: c.X, Y: lo(r.Y, r.H)}, nil
	case "down":
		return screen.Point{X: c.X, Y: lo(r.Y, r.H)}, screen.Point{X: c.X, Y: hi(r.Y, r.H)}, nil
	case "left":
		return screen.Point{X: hi(r.X, r.W), Y: c.Y}, screen.Point{X: lo(r.X, r.W), Y: c.Y}, nil
	case "right":
		return screen.Point{X: lo(r.X, r.W), Y: c.Y}, screen.Point{X: hi(r.X, r.W), Y: c.Y}, nil
	}
	return from, to, fmt.Errorf("direction %q: use up, down, left or right", dir)
}

func ms(n, def int) time.Duration {
	if n <= 0 {
		n = def
	}
	return time.Duration(n) * time.Millisecond
}

func (ph *phones) tap(ctx context.Context, in tapIn) *sdk.CallToolResult {
	if in.Ref == 0 && in.X == 0 && in.Y == 0 {
		return fail(errors.New("tap: give ref, or x and y"))
	}
	look := in.Ref != 0 || !in.Force
	return ph.act(ctx, in.actIn, look, func(d driver.Device, prev, now *screen.Screen) (string, error) {
		p, err := aim(in.targetIn, in.X, in.Y, prev, now)
		if err != nil {
			return "", err
		}
		return "", d.Tap(ctx, p, driver.TapOptions{LongPress: in.LongPress})
	}, nil)
}

func (ph *phones) swipe(ctx context.Context, in swipeIn) *sdk.CallToolResult {
	if in.Direction == "" && in.Ref != 0 {
		return fail(errors.New("swipe: ref needs a direction"))
	}
	if in.Direction == "" && in.X1 == in.X2 && in.Y1 == in.Y2 {
		return fail(errors.New("swipe: give direction, or x1,y1 and x2,y2 that differ"))
	}
	return ph.act(ctx, in.actIn, in.Direction != "", func(d driver.Device, _, now *screen.Screen) (string, error) {
		from, to := screen.Point{X: in.X1, Y: in.Y1}, screen.Point{X: in.X2, Y: in.Y2}
		if in.Direction != "" {
			r, err := region(in.Ref, now)
			if err != nil {
				return "", err
			}
			if from, to, err = scrollPath(r, in.Direction); err != nil {
				return "", err
			}
		}
		return "", d.Swipe(ctx, from, to, ms(in.DurationMS, 300))
	}, nil)
}

func (ph *phones) drag(ctx context.Context, in dragIn) *sdk.CallToolResult {
	if in.Ref == 0 && in.From == nil {
		return fail(errors.New("drag: give ref or from"))
	}
	if len(in.Via) > 8 {
		return fail(errors.New("drag: at most 8 via points"))
	}
	return ph.act(ctx, in.actIn, true, func(d driver.Device, prev, now *screen.Screen) (string, error) {
		dr, ok := d.(driver.Dragger)
		if !ok {
			return "", unsupported(d)
		}
		var x, y int
		if in.From != nil {
			x, y = in.From.X, in.From.Y
		}
		from, err := aim(in.targetIn, x, y, prev, now)
		if err != nil {
			return "", err
		}
		g := driver.Drag{From: from, Hold: ms(in.HoldMS, 0)}
		move := ms(in.DurationMS, 500)
		for _, p := range append(append([]dragPoint{}, in.Via...), in.To) {
			q := screen.Point{X: p.X, Y: p.Y}
			if err := inside(now, q); err != nil {
				return "", err
			}
			g.Stops = append(g.Stops, driver.DragStop{Point: q, Move: move, Pause: time.Duration(p.PauseMS) * time.Millisecond})
		}
		return "", dr.Drag(ctx, g)
	}, nil)
}

func (ph *phones) pinch(ctx context.Context, in pinchIn) *sdk.CallToolResult {
	if in.Scale < 0.2 || in.Scale > 5 || (in.Scale > 0.95 && in.Scale < 1.05) {
		return fail(errors.New("pinch: scale must be between 0.2 and 5, and not 1 (below 1 zooms out, above 1 zooms in)"))
	}
	return ph.act(ctx, in.actIn, true, func(d driver.Device, _, now *screen.Screen) (string, error) {
		pi, ok := d.(driver.Pincher)
		if !ok {
			return "", unsupported(d)
		}
		r, err := region(in.Ref, now)
		if err != nil {
			return "", err
		}
		c := r.Center()
		if in.Ref == 0 && (in.X != 0 || in.Y != 0) {
			c = screen.Point{X: in.X, Y: in.Y}
			if err := inside(now, c); err != nil {
				return "", err
			}
		}
		room := min(r.W, r.H) * 4 / 5
		g := driver.Pinch{Center: c, Rotate: in.Rotation, Duration: ms(in.DurationMS, 500)}
		if r.H > r.W {
			g.Angle = 90
		}
		if in.Scale > 1 {
			g.From, g.To = int(float64(room)/in.Scale), room
		} else {
			g.From, g.To = room, int(float64(room)*in.Scale)
		}
		if min(g.From, g.To) < 40 {
			return "", fmt.Errorf("pinch: %dx%d is too small for scale %g", r.W, r.H, in.Scale)
		}
		return "", pi.Pinch(ctx, g)
	}, nil)
}

func (ph *phones) typeText(ctx context.Context, in typeIn) *sdk.CallToolResult {
	var check func(before, after *screen.Screen) (string, error)
	if in.Text != "" {
		check = func(before, after *screen.Screen) (string, error) { return typeShown(in.Text, before, after) }
	}
	return ph.act(ctx, in.actIn, in.Text != "", func(d driver.Device, _, _ *screen.Screen) (string, error) {
		if _, ok := d.(*ios.Device); ok {
			check = nil // the phone checks the field itself
		}
		return "", d.Type(ctx, in.Text, driver.TypeOptions{Append: in.Append, Submit: in.Submit})
	}, func(before, after *screen.Screen) (string, error) {
		if check == nil {
			return "", nil
		}
		return check(before, after)
	})
}

// typeShown checks that typed text shows on the screen after typing. It
// never repeats the text, which may be secret.
func typeShown(text string, before, after *screen.Screen) (string, error) {
	t := strings.TrimSpace(text)
	if strings.Contains(t, "\n") {
		t = strings.TrimSpace(strings.SplitN(t, "\n", 2)[0])
	}
	if t == "" || after == nil {
		return "", nil
	}
	for _, e := range after.Elements {
		if e.Has(screen.StatePassword) && e.Has(screen.StateFocused) {
			return "the field hides its text, so it could not be checked", nil
		}
		if strings.Contains(strings.ToLower(e.Label), strings.ToLower(t)) {
			return "", nil
		}
	}
	if before != nil && renderScreen(before) == renderScreen(after) {
		return "", errors.New("type: the screen reads as it did before typing, so the text most likely did not go in: tap the field first, and look for a prompt such as Allow Paste")
	}
	return "the typed text does not show on the screen; look before going on", nil
}

// waitOCREvery is how often wait_for reads text from the screen image.
const waitOCREvery = 1500 * time.Millisecond

func (ph *phones) waitFor(ctx context.Context, in waitIn) *sdk.CallToolResult {
	texts := in.Any
	if in.Text != "" {
		texts = append([]string{in.Text}, texts...)
	}
	if len(texts) == 0 {
		return fail(errors.New("wait_for: give text or any"))
	}
	timeout := ms(in.TimeoutMS, 10000)
	if timeout > 60*time.Second {
		timeout = 60 * time.Second
	}
	start := time.Now()
	var res *sdk.CallToolResult
	err := ph.with(ctx, in.Device, func(d driver.Device) error {
		var s *screen.Screen
		var shown string
		var lastOCR time.Time
		for {
			var err error
			if s, err = ph.read(ctx, d, false); err != nil {
				return err
			}
			shown = screenShows(s, texts)
			// The image is slower to read: look there now and then for
			// text the tree leaves out.
			if shown == "" && !in.NoOCR && time.Since(lastOCR) >= waitOCREvery {
				lastOCR = time.Now()
				if o, err := ph.read(ctx, d, true); err == nil {
					s, shown = o, screenShows(o, texts)
				}
			}
			if (shown != "") != in.Gone || time.Since(start) >= timeout {
				break
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(250 * time.Millisecond):
			}
		}
		ph.view(d).set(s)
		took := time.Since(start).Round(100 * time.Millisecond)
		var msg string
		switch {
		case !in.Gone && shown != "":
			msg = fmt.Sprintf("%q shows (after %s)", shown, took)
		case in.Gone && shown == "":
			msg = fmt.Sprintf("gone (after %s)", took)
		case in.Gone:
			msg = fmt.Sprintf("timed out: %q still shows after %s", shown, took)
		default:
			msg = fmt.Sprintf("timed out: none of it shows after %s", took)
		}
		res = text(msg + "\n" + renderScreen(s))
		res.IsError = strings.HasPrefix(msg, "timed out")
		return nil
	})
	if err != nil {
		return fail(err)
	}
	return res
}

// screenShows is the first of texts that an element or text read from the
// image contains, or "".
func screenShows(s *screen.Screen, texts []string) string {
	for _, t := range texts {
		for _, e := range s.Elements {
			if containsText(e.Label, t) {
				return t
			}
		}
	}
	return ""
}

// containsText reports whether s contains t, ignoring case, as whole words
// where t starts or ends with a letter or digit: "Stop" is not in
// "Stopwatch". Scripts written without spaces (Chinese, Japanese, Thai...)
// match anywhere.
func containsText(s, t string) bool {
	s, t = strings.ToLower(s), strings.ToLower(strings.TrimSpace(t))
	if t == "" {
		return false
	}
	word := func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }
	spaced := func(r rune) bool {
		return word(r) && !unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Thai, unicode.Lao, unicode.Khmer, unicode.Myanmar)
	}
	tr := []rune(t)
	first, last := tr[0], tr[len(tr)-1]
	for i := 0; ; {
		j := strings.Index(s[i:], t)
		if j < 0 {
			return false
		}
		j += i
		end := j + len(t)
		before, after := lastRune(s[:j]), firstRune(s[end:])
		okL := !spaced(first) || before == 0 || !word(before)
		okR := !spaced(last) || after == 0 || !word(after)
		if okL && okR {
			return true
		}
		i = j + len(string(first))
	}
}

func firstRune(s string) rune {
	for _, r := range s {
		return r
	}
	return 0
}

func lastRune(s string) rune {
	r := []rune(s)
	if len(r) == 0 {
		return 0
	}
	return r[len(r)-1]
}

type batchStep struct {
	Tool string         `json:"tool" jsonschema:"tap, swipe, drag, pinch, type, key, launch, open_link, stop_app or wait_for"`
	Args map[string]any `json:"args,omitempty" jsonschema:"that tool's arguments"`
}

type batchIn struct {
	devIn
	Steps []batchStep `json:"steps" jsonschema:"at most 50, run in order"`
}

// batchable is a tool input that a batch can point at one device and
// keep quiet until the last step.
type batchable interface {
	batch(device string, last bool)
}

func (in *devIn) batch(device string, _ bool) {
	if in.Device == "" {
		in.Device = device
	}
}

func (in *actIn) batch(device string, last bool) {
	in.devIn.batch(device, last)
	in.quiet, in.full = !last, last
}

func runAs[T any](f func(context.Context, T) *sdk.CallToolResult) func(context.Context, map[string]any, string, bool) *sdk.CallToolResult {
	return func(ctx context.Context, args map[string]any, device string, last bool) *sdk.CallToolResult {
		var in T
		b, _ := json.Marshal(args)
		if err := json.Unmarshal(b, &in); err != nil {
			return fail(fmt.Errorf("arguments: %w", err))
		}
		if bt, ok := any(&in).(batchable); ok {
			bt.batch(device, last)
		}
		return f(ctx, in)
	}
}

func (ph *phones) batch(ctx context.Context, in batchIn) *sdk.CallToolResult {
	tools := map[string]func(context.Context, map[string]any, string, bool) *sdk.CallToolResult{
		"tap": runAs(ph.tap), "swipe": runAs(ph.swipe), "drag": runAs(ph.drag), "pinch": runAs(ph.pinch),
		"type": runAs(ph.typeText), "key": runAs(ph.key), "launch": runAs(ph.launch),
		"open_link": runAs(ph.openLink), "stop_app": runAs(ph.stopApp), "wait_for": runAs(ph.waitFor),
	}
	if len(in.Steps) == 0 || len(in.Steps) > 50 {
		return fail(errors.New("batch: give 1 to 50 steps"))
	}
	for i, st := range in.Steps {
		if tools[st.Tool] == nil {
			return fail(fmt.Errorf("batch: step %d: %q cannot be batched; nothing was run", i+1, st.Tool))
		}
	}
	var b strings.Builder
	for i, st := range in.Steps {
		last := i == len(in.Steps)-1
		r := tools[st.Tool](ctx, st.Args, in.Device, last)
		t := resultText(r)
		if r.IsError {
			fmt.Fprintf(&b, "step %d of %d (%s) failed, so the rest did not run: %s", i+1, len(in.Steps), st.Tool, t)
			res := text(b.String())
			res.IsError = true
			return res
		}
		if last {
			fmt.Fprintf(&b, "%d %s: %s", i+1, st.Tool, t)
		} else {
			fmt.Fprintf(&b, "%d %s: %s\n", i+1, st.Tool, firstLine(t))
		}
	}
	return text(b.String())
}

func resultText(r *sdk.CallToolResult) string {
	var parts []string
	for _, c := range r.Content {
		if t, ok := c.(*sdk.TextContent); ok {
			parts = append(parts, t.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
