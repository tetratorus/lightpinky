package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/tetratorus/lightpinky/internal/driver"
	"github.com/tetratorus/lightpinky/internal/screen"
)

func settings() *screen.Screen {
	return &screen.Screen{
		Width: 1206, Height: 2622, App: screen.App{Name: "Settings", ID: "com.apple.Preferences"}, Orientation: "portrait",
		Elements: []screen.Element{
			{Role: screen.RoleText, Label: "Settings", Bounds: screen.Rect{X: 48, Y: 357, W: 399, H: 126}, Path: "nav"},
			{Role: screen.RoleButton, Label: "General", Bounds: screen.Rect{X: 48, Y: 1140, W: 1110, H: 159}, Path: "list"},
			{Role: screen.RoleButton, Label: "Camera", Bounds: screen.Rect{X: 48, Y: 1300, W: 1110, H: 159}, Path: "list"},
			{Role: screen.RoleButton, Label: "Dictate", Bounds: screen.Rect{X: 1023, Y: 2433, W: 54, H: 66}, Path: "bar"},
			{Role: screen.RoleButton, Label: "Dictate", Bounds: screen.Rect{X: 1023, Y: 2433, W: 54, H: 66}, Path: "bar"},
		},
	}
}

func numbered(r *screen.Refs, s *screen.Screen) *screen.Screen {
	r.Assign(s)
	return s
}

func TestRefsStayAndRepeatsShowOnce(t *testing.T) {
	r := screen.NewRefs()
	a := numbered(r, settings())
	b := settings()
	b.Elements = b.Elements[1:] // the title scrolled away
	numbered(r, b)
	if a.Elements[1].Ref != b.Elements[0].Ref {
		t.Fatalf("General's ref changed: %d then %d", a.Elements[1].Ref, b.Elements[0].Ref)
	}
	out := renderScreen(a)
	if !strings.Contains(out, `[2] button "General" @603,1219`) {
		t.Fatalf("no ref in:\n%s", out)
	}
	if strings.Count(out, "Dictate") != 1 {
		t.Fatalf("a repeated element shows twice:\n%s", out)
	}
}

func TestDiffScreens(t *testing.T) {
	r := screen.NewRefs()
	old := numbered(r, settings())
	s := settings()
	s.Elements[2].States = []string{screen.StateSelected}
	s.Elements = append(s.Elements[1:], screen.Element{Role: screen.RoleButton, Label: "Siri", Bounds: screen.Rect{X: 48, Y: 1460, W: 1110, H: 159}, Path: "list"})
	numbered(r, s)
	got := diffScreens(old, s)
	for _, want := range []string{"\n~ [3] button \"Camera\"", "\n+ [6] button \"Siri\"", "\n- [1] text \"Settings\""} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "General") {
		t.Fatalf("unchanged line shown:\n%s", got)
	}
	if got := diffScreens(old, numbered(r, settings())); !strings.HasSuffix(got, "(no change since your last read)") {
		t.Fatalf("same screen:\n%s", got)
	}
	other := numbered(r, settings())
	other.App = screen.App{Name: "Safari", ID: "com.apple.mobilesafari"}
	if got := diffScreens(old, other); got != renderScreen(other) {
		t.Fatalf("another app should show whole:\n%s", got)
	}
	// Text read from the image in one read only is not a change.
	ocr := numbered(r, settings())
	ocr.Elements = append(ocr.Elements, screen.Element{Label: "11:14", Bounds: screen.Rect{X: 151, Y: 71, W: 137, H: 58}, Source: screen.SourceOCR})
	if got := diffScreens(ocr, numbered(r, settings())); !strings.HasSuffix(got, "(no change since your last read)") {
		t.Fatalf("OCR in one read only:\n%s", got)
	}
}

func TestStale(t *testing.T) {
	r := screen.NewRefs()
	prev := numbered(r, settings())
	general := screen.Point{X: 603, Y: 1219}
	if why := stale(prev, numbered(r, settings()), general); why != "" {
		t.Fatalf("same screen refused: %s", why)
	}
	scrolled := settings()
	for i := range scrolled.Elements[1:3] {
		scrolled.Elements[1+i].Bounds.Y -= 400
	}
	numbered(r, scrolled)
	if why := stale(prev, scrolled, general); !strings.Contains(why, `"General" moved to @603,819`) {
		t.Fatalf("moved: %q", why)
	}
	gone := settings()
	gone.Elements = append(gone.Elements[:1], gone.Elements[2:]...)
	if why := stale(prev, numbered(r, gone), general); !strings.Contains(why, "no longer on the screen") {
		t.Fatalf("gone: %q", why)
	}
	safari := numbered(r, settings())
	safari.App = screen.App{Name: "Safari", ID: "com.apple.mobilesafari"}
	if why := stale(prev, safari, general); !strings.Contains(why, "Settings to Safari") {
		t.Fatalf("app: %q", why)
	}
	if why := stale(prev, scrolled, screen.Point{X: 5, Y: 5}); why != "" {
		t.Fatalf("nothing was there: %q", why)
	}
	if _, err := aim(targetIn{Force: true}, 603, 1219, prev, scrolled); err != nil {
		t.Fatalf("force: %v", err)
	}
}

func TestContainsText(t *testing.T) {
	for _, c := range []struct {
		s, t string
		want bool
	}{
		{"Stopwatch", "Stop", false},
		{"Stop timer", "stop", true},
		{"No Results for “Wi-Fi”", "wi-fi", true},
		{"打开设置页面", "设置", true},
		{"Search, Wi-Fi", "Search", true},
		{"Research", "search", false},
		{"Total: $12", "$12", true},
	} {
		if got := containsText(c.s, c.t); got != c.want {
			t.Errorf("containsText(%q, %q) = %v", c.s, c.t, got)
		}
	}
}

func TestTypeShown(t *testing.T) {
	before := settings()
	after := settings()
	after.Elements = append(after.Elements, screen.Element{Role: screen.RoleInput, Label: "Search, hunter2", States: []string{screen.StateFocused}})
	if note, err := typeShown("hunter2", before, after); note != "" || err != nil {
		t.Fatalf("shown: %q %v", note, err)
	}
	_, err := typeShown("hunter2", before, settings())
	if err == nil || strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("unchanged screen must fail without repeating the text: %v", err)
	}
	changed := settings()
	changed.Elements = changed.Elements[1:]
	if note, err := typeShown("hunter2", before, changed); err != nil || note == "" || strings.Contains(note, "hunter2") {
		t.Fatalf("not shown: %q %v", note, err)
	}
	secret := settings()
	secret.Elements = append(secret.Elements, screen.Element{Role: screen.RoleInput, Label: "Password", States: []string{screen.StatePassword, screen.StateFocused}})
	if note, err := typeShown("hunter2", before, secret); err != nil || !strings.Contains(note, "hides") {
		t.Fatalf("password: %q %v", note, err)
	}
}

func TestScrollPath(t *testing.T) {
	from, to, err := scrollPath(screen.Rect{X: 0, Y: 100, W: 1000, H: 1000}, "up")
	if err != nil || from != (screen.Point{X: 500, Y: 900}) || to != (screen.Point{X: 500, Y: 300}) {
		t.Fatalf("up: %v %v %v", from, to, err)
	}
	if _, _, err := scrollPath(screen.Rect{W: 10, H: 10}, "sideways"); err == nil {
		t.Fatal("bad direction accepted")
	}
}

// screenPhone shows one screen and records taps and keys.
type screenPhone struct {
	driver.Device
	s     *screen.Screen
	taps  []screen.Point
	keys  []driver.Key
	reads int
}

func (d *screenPhone) Info() driver.Info { return driver.Info{ID: "sim", Name: "iPhone"} }
func (d *screenPhone) Close() error      { return nil }
func (d *screenPhone) Observe(context.Context, driver.ObserveOptions) (*screen.Screen, error) {
	d.reads++
	c := *d.s
	c.Elements = append([]screen.Element(nil), d.s.Elements...)
	return &c, nil
}
func (d *screenPhone) Tap(_ context.Context, p screen.Point, _ driver.TapOptions) error {
	d.taps = append(d.taps, p)
	return nil
}
func (d *screenPhone) Key(_ context.Context, k driver.Key) error {
	if k == "bad" {
		return errors.New("no such key")
	}
	d.keys = append(d.keys, k)
	return nil
}

func TestTapByRefAndBatchStops(t *testing.T) {
	d := &screenPhone{s: settings()}
	ph := &phones{open: map[string]driver.Device{"sim": d}}
	ctx := context.Background()
	if r := ph.tap(ctx, tapIn{actIn: actIn{devIn: devIn{Device: "sim"}}, targetIn: targetIn{Ref: 2}}); r.IsError {
		t.Fatal(resultText(r))
	}
	if len(d.taps) != 1 || d.taps[0] != (screen.Point{X: 603, Y: 1219}) {
		t.Fatalf("taps: %v", d.taps)
	}
	r := ph.batch(ctx, batchIn{devIn: devIn{Device: "sim"}, Steps: []batchStep{
		{Tool: "key", Args: map[string]any{"key": "home"}},
		{Tool: "key", Args: map[string]any{"key": "bad"}},
		{Tool: "tap", Args: map[string]any{"ref": 2}},
	}})
	if !r.IsError || !strings.Contains(resultText(r), "step 2 of 3 (key) failed") {
		t.Fatalf("batch: %s", resultText(r))
	}
	if len(d.keys) != 1 || len(d.taps) != 1 {
		t.Fatalf("ran past the failure: keys %v taps %v", d.keys, d.taps)
	}
	if r := ph.batch(ctx, batchIn{Steps: []batchStep{{Tool: "screenshot"}}}); !r.IsError {
		t.Fatal("unbatchable tool accepted")
	}
}
