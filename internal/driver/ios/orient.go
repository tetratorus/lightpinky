package ios

import (
	"encoding/binary"
	"image"
	"image/draw"

	"github.com/tetratorus/lightpinky/internal/screen"
)

// Orientation. Pinky's coordinates on an iPhone or iPad are the screen as
// the person sees it, the right way up: element bounds, taps, swipes,
// screenshots and the mirror's picture all agree, in every orientation, as
// on Android. Some of what the phone gives is in its panel's own, fixed
// orientation instead (the picture WebDriverAgent streams, for one), and is
// turned upright on the way in.

// quarter is a turn in quarter turns clockwise, 0 to 3.
type quarter int

// swaps reports whether the turn swaps width and height.
func (q quarter) swaps() bool { return q&1 == 1 }

// inverse is the turn that undoes q.
func (q quarter) inverse() quarter { return (4 - q&3) & 3 }

// turnSize returns the size of a w×h picture after turning it q.
func turnSize(w, h int, q quarter) (int, int) {
	if q.swaps() {
		return h, w
	}
	return w, h
}

// turnPoint returns where the pixel at p of a w×h picture lands when the
// picture turns q clockwise.
func turnPoint(p screen.Point, w, h int, q quarter) screen.Point {
	switch q & 3 {
	case 1:
		return screen.Point{X: h - 1 - p.Y, Y: p.X}
	case 2:
		return screen.Point{X: w - 1 - p.X, Y: h - 1 - p.Y}
	case 3:
		return screen.Point{X: p.Y, Y: w - 1 - p.X}
	}
	return p
}

// turnNV12 turns NV12 planes (a w×h Y plane, then Cb and Cr interleaved at
// half size; w and h even) q clockwise.
func turnNV12(y, uv []byte, w, h int, q quarter) ([]byte, []byte, int, int) {
	q &= 3
	if q == 0 {
		return y, uv, w, h
	}
	tw, th := turnSize(w, h, q)
	ty := make([]byte, len(y))
	for row := 0; row < h; row++ {
		src := y[row*w : (row+1)*w]
		for col, v := range src {
			p := turnPoint(screen.Point{X: col, Y: row}, w, h, q)
			ty[p.Y*tw+p.X] = v
		}
	}
	tuv := make([]byte, len(uv))
	cw, ch := w/2, h/2
	tcw := tw / 2
	for row := 0; row < ch; row++ {
		for col := 0; col < cw; col++ {
			p := turnPoint(screen.Point{X: col, Y: row}, cw, ch, q)
			i, j := 2*(row*cw+col), 2*(p.Y*tcw+p.X)
			tuv[j], tuv[j+1] = uv[i], uv[i+1]
		}
	}
	return ty, tuv, tw, th
}

// turnImage turns a picture q clockwise.
func turnImage(img image.Image, q quarter) image.Image {
	q &= 3
	if q == 0 {
		return img
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	src, ok := img.(*image.RGBA)
	if !ok || b.Min != (image.Point{}) {
		src = image.NewRGBA(image.Rect(0, 0, w, h))
		draw.Draw(src, src.Rect, img, b.Min, draw.Src)
	}
	tw, th := turnSize(w, h, q)
	dst := image.NewRGBA(image.Rect(0, 0, tw, th))
	for row := 0; row < h; row++ {
		line := src.Pix[row*src.Stride : row*src.Stride+4*w]
		for col := 0; col < w; col++ {
			p := turnPoint(screen.Point{X: col, Y: row}, w, h, q)
			i := p.Y*dst.Stride + 4*p.X
			copy(dst.Pix[i:i+4], line[4*col:4*col+4])
		}
	}
	return dst
}

// exifTurn reads a JPEG's EXIF orientation and returns the turn that puts
// the picture the right way up: WebDriverAgent's MJPEG frames are the panel
// as it is, with the interface's orientation only in this tag. Mirrored
// orientations (2, 4, 5, 7), which a screen never has, and pictures without
// the tag need no turn.
func exifTurn(b []byte) quarter {
	if len(b) < 4 || b[0] != 0xFF || b[1] != 0xD8 {
		return 0
	}
	for i := 2; i+4 <= len(b); {
		if b[i] != 0xFF {
			return 0
		}
		marker := b[i+1]
		if marker == 0xD8 || (marker >= 0xD0 && marker <= 0xD7) || marker == 0x01 {
			i += 2
			continue
		}
		if marker == 0xDA || marker == 0xD9 {
			return 0 // the picture's data starts: no EXIF before it
		}
		n := int(binary.BigEndian.Uint16(b[i+2:]))
		if n < 2 || i+2+n > len(b) {
			return 0
		}
		seg := b[i+4 : i+2+n]
		if marker == 0xE1 && len(seg) > 14 && string(seg[:6]) == "Exif\x00\x00" {
			return tiffTurn(seg[6:])
		}
		i += 2 + n
	}
	return 0
}

// tiffTurn reads the orientation tag (0x0112) of an EXIF block's first IFD.
func tiffTurn(t []byte) quarter {
	if len(t) < 8 {
		return 0
	}
	var bo binary.ByteOrder
	switch string(t[:2]) {
	case "II":
		bo = binary.LittleEndian
	case "MM":
		bo = binary.BigEndian
	default:
		return 0
	}
	off := int(bo.Uint32(t[4:]))
	if off < 8 || off+2 > len(t) {
		return 0
	}
	count := int(bo.Uint16(t[off:]))
	for k := 0; k < count; k++ {
		e := off + 2 + 12*k
		if e+12 > len(t) {
			return 0
		}
		if bo.Uint16(t[e:]) != 0x0112 {
			continue
		}
		switch bo.Uint16(t[e+8:]) {
		case 3:
			return 2
		case 6:
			return 1
		case 8:
			return 3
		}
		return 0
	}
	return 0
}
