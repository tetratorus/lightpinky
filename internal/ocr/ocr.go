// Package ocr reads text from pictures of a phone's screen, for screens
// whose element tree misses it: web views and canvases on Android, and
// iPhones read without frames. It uses the computer's own recognizer,
// called without cgo: Vision on macOS and Windows.Media.Ocr on Windows.
// Other systems have none, which Supported reports.
package ocr

import (
	_ "embed"
	"image"
	"strings"
	"sync"
	"unicode"

	"github.com/tetratorus/lightpinky/internal/screen"
)

// Line is one line of text OCR found, with a box normalized to 0..1 from
// the image's top-left corner.
type Line struct {
	Text       string
	Confidence float32
	X, Y, W, H float64
}

// Options tune a recognition.
type Options struct {
	Fast      bool     // the fast recognizer: lower accuracy, no language correction
	Languages []string // in priority order, e.g. "zh-Hans", "en-US"
	// AutoLanguage lets Vision detect the language. It can load extra
	// models on the fly, which has taken over ten seconds.
	AutoLanguage bool
}

// Languages are the languages Pinky reads, in priority order.
var Languages = []string{"zh-Hans", "zh-Hant", "en-US"}

// MinConfidence is the confidence below which a line is left out of the
// screen.
const MinConfidence = 0.3

// Supported reports whether this computer can read text from pictures:
// nil, or why not, wrapping driver.ErrUnsupported. It is cheap: the
// recognizer's models load at the first recognition.
func Supported() error { return supported() }

// Recognize reads the text in an encoded image (PNG or JPEG).
func Recognize(img []byte, opt Options) ([]Line, error) { return recognize(img, opt) }

// RecognizeImage reads the text in a decoded image, such as a frame of the
// live video.
func RecognizeImage(img image.Image, opt Options) ([]Line, error) {
	return recognizeImage(img, opt)
}

// warmPicture is a small picture of "Pinky 拼读 42": English, Chinese and
// digits, so that a recognition of it loads every model a screen needs.
//
//go:embed warm.png
var warmPicture []byte

var warmOnce sync.Once

// Warm loads the recognizer's models in the background, once per process,
// so that the first screen read with OCR need not wait for them. Vision
// takes 0.1 to 0.2 s for that while macOS keeps the models it compiled,
// and 13 to 15 s when it has dropped them (seen after builds and on a Mac
// that had not run OCR for a while).
func Warm() {
	if Supported() != nil {
		return
	}
	warmOnce.Do(func() {
		go func() { _, _ = Recognize(warmPicture, Options{Languages: Languages}) }()
	})
}

// Rect is a line's box in a w x h screen.
func Rect(l Line, w, h int) screen.Rect {
	return screen.Rect{
		X: int(l.X * float64(w)), Y: int(l.Y * float64(h)),
		W: int(l.W * float64(w)), H: int(l.H * float64(h)),
	}
}

// Extras returns the lines no element already shows, as OCR elements of a
// w x h screen: a line counts as shown when its center lies in an element
// whose label contains its text. Empty lines and lines below
// MinConfidence are left out.
func Extras(els []screen.Element, lines []Line, w, h int) []screen.Element {
	var out []screen.Element
	for _, l := range lines {
		text := strings.TrimSpace(l.Text)
		if text == "" || l.Confidence < MinConfidence {
			continue
		}
		r := Rect(l, w, h)
		c := r.Center()
		nt := norm(text)
		shown := false
		for _, e := range els {
			if e.Bounds.Contains(c) && strings.Contains(norm(e.Label), nt) {
				shown = true
				break
			}
		}
		if !shown {
			out = append(out, screen.Element{Role: screen.RoleText, Label: text, Source: screen.SourceOCR, Bounds: r})
		}
	}
	return out
}

// KeyboardTop finds an on-screen keyboard in OCR lines: at least five
// single letters in the lower half of the screen, spread across most of
// its width (a list's section index is a column of single letters too).
// It returns the keyboard's top edge, normalized.
func KeyboardTop(lines []Line) (float64, bool) {
	top, n := 1.0, 0
	left, right := 1.0, 0.0
	for _, l := range lines {
		t := []rune(strings.TrimSpace(l.Text))
		if l.Y < 0.5 || len(t) != 1 || !unicode.IsLetter(t[0]) {
			continue
		}
		n++
		top = min(top, l.Y)
		left = min(left, l.X)
		right = max(right, l.X+l.W)
	}
	if n < 5 || right-left < 0.5 {
		return 0, false
	}
	return top - 0.02, true
}

// norm folds a string for matching: lower case, letters and digits only.
func norm(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}
