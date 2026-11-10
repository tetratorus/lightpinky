package main

import (
	"image"
	"image/color"
	"strings"
	"testing"

	"github.com/tetratorus/lightpinky/internal/screen"
)

func TestRenderScreenOneLinePerElement(t *testing.T) {
	dictate := screen.Element{Role: "button", Label: "Dictate", ID: "dictation", Bounds: screen.Rect{X: 975, Y: 2415, W: 207, H: 207}}
	s := &screen.Screen{Width: 1206, Height: 2622, App: screen.App{Name: "Settings", ID: "com.apple.Preferences"}, Elements: []screen.Element{
		{Role: "button", Label: "General", ID: "com.apple.settings.general", Bounds: screen.Rect{X: 48, Y: 1140, W: 1110, H: 159}, States: []string{"selected"}},
		dictate, dictate,
	}}
	got := renderScreen(s)
	want := []string{
		`button "General" id="com.apple.settings.general" @603,1219 [48,1140 1110x159] selected`,
		`button "Dictate" id="dictation" @1078,2518 [975,2415 207x207]`,
	}
	lines := strings.Split(got, "\n")
	if !strings.HasPrefix(lines[0], "screen 1206x2622 · app: Settings (com.apple.Preferences)") {
		t.Errorf("header %q", lines[0])
	}
	if strings.Join(lines[1:], "\n") != strings.Join(want, "\n") {
		t.Errorf("got\n%s\nwant\n%s", strings.Join(lines[1:], "\n"), strings.Join(want, "\n"))
	}
}

func TestShrink(t *testing.T) {
	for _, c := range []struct{ w, h, max, k int }{
		{1206, 2622, 0, 1}, {1206, 2622, 3000, 1}, {1206, 2622, 900, 3}, {1206, 2622, 1311, 2}, {1206, 2622, 1310, 3},
	} {
		if k := shrinkFactor(c.w, c.h, c.max); k != c.k {
			t.Errorf("shrinkFactor(%d, %d, %d) = %d, want %d", c.w, c.h, c.max, k, c.k)
		}
	}
	img := image.NewRGBA(image.Rect(0, 0, 4, 2))
	img.Set(0, 0, color.White)
	img.Set(1, 1, color.White)
	out := shrink(img, 2)
	if b := out.Bounds(); b.Dx() != 2 || b.Dy() != 1 {
		t.Fatalf("size %v", b)
	}
	if r, _, _, _ := out.At(0, 0).RGBA(); r>>8 != 127 {
		t.Errorf("average %d, want 127", r>>8)
	}
}
