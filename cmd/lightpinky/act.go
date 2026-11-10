package main

import (
	"context"
	"fmt"
	"image"
	"image/color"
	"strings"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tetratorus/lightpinky/internal/driver"
	"github.com/tetratorus/lightpinky/internal/screen"
)

// settleLimit is how long an action waits for the screen to stop changing.
const settleLimit = 2 * time.Second

// act runs an action on the phone, then waits for the screen to settle
// unless in.NoSettle, so a read right after sees where it led.
func (ph *phones) act(ctx context.Context, in actIn, f func(driver.Device) error) (*sdk.CallToolResult, any, error) {
	msg := "ok"
	err := ph.with(ctx, in.Device, func(d driver.Device) error {
		if err := f(d); err != nil {
			return err
		}
		if s, ok := d.(driver.Settler); ok && !in.NoSettle {
			msg = settleNote(s.WaitSettled(ctx, settleLimit))
		}
		return nil
	})
	if err != nil {
		return fail(err), nil, nil
	}
	return text(msg), nil, nil
}

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
		var line strings.Builder
		writeElement(&line, e)
		// Trees can list one element twice (the keyboard's Dictate button).
		if seen[line.String()] {
			continue
		}
		seen[line.String()] = true
		b.WriteByte('\n')
		b.WriteString(line.String())
	}
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
