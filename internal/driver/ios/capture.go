package ios

import (
	"bytes"
	"context"
	"image"
	"image/draw"
	"image/jpeg"
	"image/png"
	"math"
	"sync"
	"time"

	"github.com/tetratorus/lightpinky/internal/driver"
	"github.com/tetratorus/lightpinky/internal/screen"
)

// Some apps draw part of their screen in a layer that iOS leaves out of
// screenshots, recordings and mirroring: Trust's passcode keypad, in the
// field test (item 21), was in the agent's tree, keys and Face ID button,
// while the person watching in Pinky saw blank space there. Pinky cannot
// show what iOS does not capture, but it can tell the agent: elements that
// the screenshot shows nothing of, while the tree has them, carry the state
// below. A read that takes a screenshot (one with OCR) finds them; reads
// without one keep what the last such read of the same app found, so that
// an element's states do not change with the kind of read (an approval
// compares them before it acts).

// stateNotInScreenshot marks an element that the screenshot, and so the
// person watching in Pinky, does not show.
const stateNotInScreenshot = "not_in_screenshot"

// Detection limits.
const (
	uncapturedMin   = 3 // elements, at least, before any is marked: one blank element is noise
	uncapturedRange = 6 // brightness range, out of 255, that counts as blank
	uncapturedKeep  = time.Minute
)

// uncaptured returns the elements, by index, that the screenshot shows
// nothing of: their inside is one flat color and OCR read no text there.
// It returns none unless at least uncapturedMin are.
func uncaptured(els []screen.Element, img image.Image, lines []ocrLine, w, h int) []int {
	if img == nil || w <= 0 || h <= 0 {
		return nil
	}
	b := img.Bounds()
	sx, sy := float64(b.Dx())/float64(w), float64(b.Dy())/float64(h)
	var out []int
	for i, e := range els {
		if e.Source != screen.SourceTree || e.Label == "" || !visibleRole(e.Role) {
			continue
		}
		r := e.Bounds
		if r.W < 30 || r.H < 30 || r.W*r.H > w*h/2 {
			continue
		}
		if ocrInside(lines, r, w, h) {
			continue
		}
		if flat(img, r, sx, sy) {
			out = append(out, i)
		}
	}
	if len(out) < uncapturedMin {
		return nil
	}
	return out
}

// visibleRole reports whether elements of a role draw something: text or a
// control.
func visibleRole(role string) bool {
	switch role {
	case screen.RoleButton, screen.RoleText, screen.RoleInput, screen.RoleLink, screen.RoleTab, screen.RoleSwitch, screen.RoleCheckbox:
		return true
	}
	return false
}

// ocrInside reports whether an OCR line's middle lies in r.
func ocrInside(lines []ocrLine, r screen.Rect, w, h int) bool {
	for _, l := range lines {
		if r.Contains(ocrRect(l, w, h).Center()) {
			return true
		}
	}
	return false
}

// flat reports whether all of r is one color in img: no sample, taken
// every couple of pixels, is brighter or darker than another by more than
// uncapturedRange. Thin strokes count: Calculator's minus sign is a few
// pixels high in an orange circle.
func flat(img image.Image, r screen.Rect, sx, sy float64) bool {
	step := max(2, int(math.Ceil(math.Sqrt(float64(r.W*r.H)/uncapturedSamples))))
	b := img.Bounds()
	lo, hi := 255, 0
	for y := r.Y; y < r.Y+r.H; y += step {
		for x := r.X; x < r.X+r.W; x += step {
			px, py := b.Min.X+int(float64(x)*sx), b.Min.Y+int(float64(y)*sy)
			if px >= b.Max.X || py >= b.Max.Y {
				continue
			}
			cr, cg, cb, _ := img.At(px, py).RGBA()
			l := int((299*cr + 587*cg + 114*cb) / 1000 >> 8)
			lo, hi = min(lo, l), max(hi, l)
			if hi-lo > uncapturedRange {
				return false
			}
		}
	}
	return true
}

// uncapturedSamples bounds the samples flat takes of one element.
const uncapturedSamples = 40000

// decodeShot decodes a screenshot for uncaptured, nil when it cannot.
func decodeShot(b []byte) image.Image {
	img, err := png.Decode(bytes.NewReader(b))
	if err != nil {
		return nil
	}
	return img
}

// captureMarks remembers, per app, which elements the screenshot did not
// show, for reads without one.
type captureMarks struct {
	mu   sync.Mutex
	app  string
	keys map[string]time.Time
}

func elementKey(e screen.Element) string {
	return e.Role + "\x00" + e.Label + "\x00" + e.ID + "\x00" + e.Path
}

// mark records what a read with a screenshot found and marks those
// elements; found are indexes into s.Elements.
func (c *captureMarks) mark(s *screen.Screen, found []int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.app != s.App.ID || c.keys == nil {
		c.app, c.keys = s.App.ID, map[string]time.Time{}
	}
	now := time.Now()
	in := map[int]bool{}
	for _, i := range found {
		in[i] = true
	}
	for i := range s.Elements {
		k := elementKey(s.Elements[i])
		if in[i] {
			c.keys[k] = now
			s.Elements[i].States = appendState(s.Elements[i].States, stateNotInScreenshot)
		} else {
			delete(c.keys, k) // the screenshot shows it
		}
	}
}

// apply marks, on a read without a screenshot, the elements that the last
// read with one did not show.
func (c *captureMarks) apply(s *screen.Screen) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.app != s.App.ID || len(c.keys) == 0 {
		return
	}
	for i := range s.Elements {
		if t, ok := c.keys[elementKey(s.Elements[i])]; ok && time.Since(t) < uncapturedKeep {
			s.Elements[i].States = appendState(s.Elements[i].States, stateNotInScreenshot)
		}
	}
}

// ocrPicture is the picture OCR reads, encoded for Vision, and decoded for
// the capture marks: over Wi-Fi a frame of the live video, when the gateway
// offers one (driver.ObserveOptions.Frame), since a screenshot costs
// seconds there against well under a second for a frame; else, or when the
// frame fails, a screenshot. from says which.
func (d *Device) ocrPicture(ctx context.Context, opt driver.ObserveOptions) (data []byte, img image.Image, from string, err error) {
	if s, ok := d.videoFrame(ctx, opt.Frame); ok {
		var buf bytes.Buffer
		if jerr := jpeg.Encode(&buf, s, &jpeg.Options{Quality: 95}); jerr == nil {
			return buf.Bytes(), s, "video", nil
		}
	}
	if ctx.Err() != nil {
		return nil, nil, "", ctx.Err()
	}
	png, err := d.ScreenshotPNG(ctx)
	if err != nil {
		return nil, nil, "", err
	}
	return png, nil, "screenshot", nil
}

// Pictures from the live video over Wi-Fi. A phone sends a frame within a
// second of being asked (0.1 to 0.8 s on an iPhone 13 mini over Wi-Fi, also
// when its stream had just started), but one that loses most of its video's
// packets, or ignores the request for a key frame while its screen stays
// still, sends none for seconds (an iPad Pro 11-inch on 2026-11-08: four of
// six frames missed 5 s). So a frame is waited for frameWait at most, and
// after one failed the phone's screenshots are used for frameRetry: each
// read would else wait for a frame before its screenshot. While the mirror
// or a task's recording watches the phone, the gateway has the latest
// picture at hand and the frame comes at once.
var (
	frameWait  = 2500 * time.Millisecond
	frameRetry = time.Minute
)

// videoFrame returns a picture of the screen from frame, a frame of the live
// video (driver.ObserveOptions.Frame), cropped to the screen, over Wi-Fi
// only; ok is false when there is none to take, or it did not come in
// time.
func (d *Device) videoFrame(ctx context.Context, frame func(context.Context) (image.Image, error)) (image.Image, bool) {
	if frame == nil || d.info.Connection != "wifi" {
		return nil, false
	}
	d.mu.Lock()
	skip := time.Now().Before(d.framesAfter)
	d.mu.Unlock()
	if skip {
		return nil, false
	}
	fctx, cancel := context.WithTimeout(ctx, frameWait)
	f, err := frame(fctx)
	cancel()
	if err == nil {
		// The video is in the display's own frame: cut the screen out of
		// it there, then turn it upright as screenshots are.
		si := d.freshDisplay(ctx)
		if s, ok := screenOfFrame(f, si); ok {
			return turnImage(s, si.turn()), true
		}
	}
	if ctx.Err() != nil {
		return nil, false // the read gave up: not the frame's fault
	}
	d.mu.Lock()
	d.framesAfter = time.Now().Add(frameRetry)
	d.mu.Unlock()
	return nil, false
}

// ScreenshotFromFrame implements driver.FrameShooter: over Wi-Fi, where a
// screenshot takes seconds (a PNG of 1 to 15 MB at about 1 MB/s), the
// screen from a frame of the live video, as OCR reads it.
func (d *Device) ScreenshotFromFrame(ctx context.Context, frame func(context.Context) (image.Image, error)) (image.Image, bool) {
	return d.videoFrame(ctx, frame)
}

// screenOfFrame is the screen in a frame of the live video, in the
// display's own frame (si's Width and Height): the phone puts its screen at
// 1:1 in the picture's top-left corner and pads the rest (fromVideo). ok is false for a picture
// smaller than the screen, which no tested phone sends, or without a known
// screen size.
func screenOfFrame(f image.Image, si screenInfo) (image.Image, bool) {
	if f == nil || si.Width <= 0 || si.Height <= 0 {
		return nil, false
	}
	b := f.Bounds()
	if b.Dx() < si.Width || b.Dy() < si.Height {
		return nil, false
	}
	r := image.Rect(b.Min.X, b.Min.Y, b.Min.X+si.Width, b.Min.Y+si.Height)
	if sub, ok := f.(interface {
		SubImage(image.Rectangle) image.Image
	}); ok {
		return sub.SubImage(r), true
	}
	out := image.NewRGBA(image.Rect(0, 0, si.Width, si.Height))
	draw.Draw(out, out.Bounds(), f, r.Min, draw.Src)
	return out, true
}
