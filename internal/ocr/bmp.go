package ocr

import (
	"encoding/binary"
	"image"
)

// encodeBMP encodes img as an uncompressed 24-bit BMP. A frame of a
// phone's screen is about 2.5 million pixels: as a BMP it takes a few
// milliseconds to write and to read back, against tens of milliseconds
// for a JPEG and hundreds for a PNG.
func encodeBMP(img image.Image) []byte {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	stride := (w*3 + 3) &^ 3
	const header = 14 + 40
	out := make([]byte, header+stride*h)
	le := binary.LittleEndian
	// BITMAPFILEHEADER
	out[0], out[1] = 'B', 'M'
	le.PutUint32(out[2:], uint32(len(out)))
	le.PutUint32(out[10:], header)
	// BITMAPINFOHEADER: bottom-up rows, no compression.
	le.PutUint32(out[14:], 40)
	le.PutUint32(out[18:], uint32(w))
	le.PutUint32(out[22:], uint32(h))
	le.PutUint16(out[26:], 1)
	le.PutUint16(out[28:], 24)
	le.PutUint32(out[34:], uint32(stride*h))
	le.PutUint32(out[38:], 2835) // 72 dpi
	le.PutUint32(out[42:], 2835)

	row := func(y int) []byte { // y counts from the image's top
		off := header + (h-1-y)*stride
		return out[off : off+w*3]
	}
	switch m := img.(type) {
	case *image.RGBA:
		for y := 0; y < h; y++ {
			src := m.Pix[m.PixOffset(b.Min.X, b.Min.Y+y):]
			dst := row(y)
			for x := 0; x < w; x++ {
				dst[3*x], dst[3*x+1], dst[3*x+2] = src[4*x+2], src[4*x+1], src[4*x]
			}
		}
	case *image.NRGBA:
		for y := 0; y < h; y++ {
			src := m.Pix[m.PixOffset(b.Min.X, b.Min.Y+y):]
			dst := row(y)
			for x := 0; x < w; x++ {
				dst[3*x], dst[3*x+1], dst[3*x+2] = src[4*x+2], src[4*x+1], src[4*x]
			}
		}
	default:
		for y := 0; y < h; y++ {
			dst := row(y)
			for x := 0; x < w; x++ {
				r, g, bl, _ := img.At(b.Min.X+x, b.Min.Y+y).RGBA()
				dst[3*x], dst[3*x+1], dst[3*x+2] = byte(bl>>8), byte(g>>8), byte(r>>8)
			}
		}
	}
	return out
}
