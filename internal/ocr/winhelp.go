package ocr

import (
	"image"
	"strings"
	"unicode"
)

// Helpers of the Windows recognizer, kept apart from its WinRT calls so
// that their tests run everywhere.

// joinWords joins a line's words with spaces, except between two Chinese
// or Japanese characters: Windows splits Chinese text into words of a
// character or a few, which are not separated on screen.
func joinWords(words []string) string {
	var b strings.Builder
	var prev rune
	for i, w := range words {
		w = strings.TrimSpace(w)
		if w == "" {
			continue
		}
		first := []rune(w)[0]
		if i > 0 && b.Len() > 0 && !(closeUp(prev) && closeUp(first)) {
			b.WriteByte(' ')
		}
		b.WriteString(w)
		r := []rune(w)
		prev = r[len(r)-1]
	}
	return b.String()
}

// closeUp reports whether r is written without spaces around it: Han,
// kana and their punctuation.
func closeUp(r rune) bool {
	return unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana) ||
		(r >= 0x3000 && r <= 0x303f) || (r >= 0xff00 && r <= 0xffef)
}

// bgra returns img's pixels as opaque BGRA, scaled down by a whole box
// filter when a side is longer than maxSide (0: no limit), and the size.
func bgra(img image.Image, maxSide int) ([]byte, int, int) {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	f := 1
	for maxSide > 0 && (w/f > maxSide || h/f > maxSide) {
		f++
	}
	ow, oh := w/f, h/f
	out := make([]byte, ow*oh*4)
	rgba, isRGBA := img.(*image.RGBA)
	for y := 0; y < oh; y++ {
		for x := 0; x < ow; x++ {
			var sr, sg, sb uint32
			for dy := 0; dy < f; dy++ {
				for dx := 0; dx < f; dx++ {
					px, py := b.Min.X+x*f+dx, b.Min.Y+y*f+dy
					if isRGBA {
						i := rgba.PixOffset(px, py)
						sr += uint32(rgba.Pix[i])
						sg += uint32(rgba.Pix[i+1])
						sb += uint32(rgba.Pix[i+2])
						continue
					}
					r, g, bl, _ := img.At(px, py).RGBA()
					sr += r >> 8
					sg += g >> 8
					sb += bl >> 8
				}
			}
			n := uint32(f * f)
			i := (y*ow + x) * 4
			out[i], out[i+1], out[i+2], out[i+3] = byte(sb/n), byte(sg/n), byte(sr/n), 0xff
		}
	}
	return out, ow, oh
}
