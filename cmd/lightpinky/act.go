package main

import (
	"fmt"
	"image"
	"image/color"
	"strings"
	"time"

	"github.com/tetratorus/lightpinky/internal/screen"
)

// settleLimit is how long an action waits for the screen to stop changing.
const settleLimit = 2 * time.Second

func settleNote(settled bool, err error) string {
	switch {
	case err != nil:
		return "ok (did not wait for the screen to settle: " + err.Error() + ")"
	case !settled:
		return fmt.Sprintf("ok (the screen was still changing after %s)", settleLimit)
	}
	return "ok"
}

// renderScreen is the observation as text: a header, then one line per
// element with its center and bounds in screenshot pixels.
func renderScreen(s *screen.Screen) string {
	var b strings.Builder
	b.WriteString(s.Header())
	for _, ev := range s.Events {
		b.WriteByte('\n')
		b.WriteString(ev.Line())
	}
	seen := map[string]bool{}
	for _, e := range s.Elements {
		line := elementLine(e)
		// Trees can list one element twice (the keyboard's Dictate button).
		if seen[plainLine(e)] {
			continue
		}
		seen[plainLine(e)] = true
		b.WriteByte('\n')
		b.WriteString(line)
	}
	return b.String()
}

// elementLine is one element as text, its ref first.
func elementLine(e screen.Element) string {
	var b strings.Builder
	if e.Ref != 0 {
		fmt.Fprintf(&b, "[%d] ", e.Ref)
	}
	writeElement(&b, e)
	return b.String()
}

// plainLine is the element without its ref, to tell repeats apart.
func plainLine(e screen.Element) string {
	var b strings.Builder
	writeElement(&b, e)
	return b.String()
}

func writeElement(b *strings.Builder, e screen.Element) {
	if e.Source == screen.SourceOCR {
		b.WriteString("ocr")
	} else {
		b.WriteString(e.Role)
	}
	if e.Label != "" {
		fmt.Fprintf(b, " %q", e.Label)
	}
	if e.ID != "" {
		fmt.Fprintf(b, " id=%q", e.ID)
	}
	if r := e.Bounds; !r.Empty() {
		c := r.Center()
		fmt.Fprintf(b, " @%d,%d [%d,%d %dx%d]", c.X, c.Y, r.X, r.Y, r.W, r.H)
	}
	for _, st := range e.States {
		b.WriteByte(' ')
		b.WriteString(st)
	}
}

// shrinkFactor is the least whole factor that brings a w×h image's longer
// side to at most max; 1 when max is 0 or already met.
func shrinkFactor(w, h, max int) int {
	long := w
	if h > long {
		long = h
	}
	if max <= 0 || long <= max {
		return 1
	}
	return (long + max - 1) / max
}

// shrink averages each k×k block of img into one pixel.
func shrink(img image.Image, k int) image.Image {
	b := img.Bounds()
	out := image.NewRGBA(image.Rect(0, 0, b.Dx()/k, b.Dy()/k))
	for y := 0; y < out.Rect.Dy(); y++ {
		for x := 0; x < out.Rect.Dx(); x++ {
			var r, g, bl, n uint32
			for yy := 0; yy < k; yy++ {
				for xx := 0; xx < k; xx++ {
					cr, cg, cb, _ := img.At(b.Min.X+x*k+xx, b.Min.Y+y*k+yy).RGBA()
					r, g, bl, n = r+cr, g+cg, bl+cb, n+1
				}
			}
			out.SetRGBA(x, y, color.RGBA{uint8(r / n >> 8), uint8(g / n >> 8), uint8(bl / n >> 8), 255})
		}
	}
	return out
}
