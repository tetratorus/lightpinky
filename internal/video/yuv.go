package video

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"runtime"
	"unsafe"
)

// YUV is a picture in Y′CbCr 4:2:0, as decoders give their pictures, for a
// UI that converts it as it draws it, as MyGo's Bitmap.UpdateYUV does on
// the GPU: NV12, as the hardware decoders leave pictures, or I420, as the
// software decoder does.
type YUV struct {
	// NV12 tells that the planes are the luma and the chroma, Cb and Cr
	// interleaved; else they are I420's luma, Cb and Cr.
	NV12          bool
	Width, Height int
	// Planes hold the samples in rows of Strides bytes: the luma a byte a
	// pixel, the chroma half as wide and high, rounded up.
	Planes  [3][]byte
	Strides [3]int
	// Matrix converts the samples to RGB, and FullRange tells that they
	// take all of 0 to 255 rather than video's 16 to 235 (240 for chroma).
	Matrix    Matrix
	FullRange bool
}

// Matrix is the standard whose coefficients turn Y′CbCr into RGB.
type Matrix uint8

const (
	BT601  Matrix = iota // standard definition video's
	BT709                // HD video's
	BT2020               // UHD video's
)

// ConvertYUV copies the last picture Receive returned into dst, at its own
// size and reusing dst's planes: in NV12 or I420 as the decoder gave it,
// and converted to I420 when it came in another layout, as 10-bit pictures
// do. Its matrix is the one Convert reads it with.
func (d *Decoder) ConvertYUV(dst *YUV) error {
	if !d.have {
		return errors.New("video: no picture to convert")
	}
	l := d.l
	src, err := d.onCPU()
	if err != nil {
		return err
	}
	if src == d.sw {
		defer l.av_frame_unref(d.sw)
	}
	format := getInt32(src, frameFormat)
	nv12 := format == l.pixNV12
	dst.layout(nv12, int(getInt32(src, frameWidth)), int(getInt32(src, frameHeight)))
	dst.Matrix = matrixOf(getInt32(src, frameColorspace))
	dst.FullRange = getInt32(src, frameColorRange) == avColorRangeJPEG || format == l.pixYUVJ420P
	if nv12 || format == l.pixYUV420P || format == l.pixYUVJ420P {
		return dst.copyFrom(src)
	}
	return d.toI420(dst, src)
}

// matrixOf is the matrix swscale reads a picture's samples with in Convert,
// from its AVColorSpace: BT.601 for a stream that names none, and BT.709
// for one whose matrix swscale cannot convert, as Convert falls back to.
func matrixOf(colorspace int32) Matrix {
	switch colorspace {
	case 1, 7: // AVCOL_SPC_BT709, AVCOL_SPC_SMPTE240M
		return BT709
	case 9, 10: // AVCOL_SPC_BT2020_NCL, AVCOL_SPC_BT2020_CL
		return BT2020
	case 0, 2, 3, 4, 5, 6: // RGB, unspecified, reserved, FCC, BT470BG, SMPTE170M
		return BT601
	}
	// YCgCo, as the Mi 10 Pro's encoder tags its stream, and the others.
	return BT709
}

// layout sizes the planes for a w×h picture, in rows as long as their
// samples, reusing their memory.
func (y *YUV) layout(nv12 bool, w, h int) {
	cw := (w + 1) / 2
	y.NV12, y.Width, y.Height = nv12, w, h
	y.Strides = [3]int{w, cw, cw}
	if nv12 {
		y.Strides = [3]int{w, 2 * cw, 0}
	}
	for i := range y.Planes {
		n := y.Strides[i] * y.rows(i)
		if cap(y.Planes[i]) < n {
			y.Planes[i] = make([]byte, n)
		}
		y.Planes[i] = y.Planes[i][:n]
	}
}

// rows is the number of rows of plane i.
func (y *YUV) rows(i int) int {
	if i == 0 {
		return y.Height
	}
	return (y.Height + 1) / 2
}

// copyFrom copies the planes of a frame in the picture's layout, NV12 or
// I420: the frame goes back to the decoder.
func (y *YUV) copyFrom(frame unsafe.Pointer) error {
	n := 3
	if y.NV12 {
		n = 2
	}
	for i := range n {
		rows, row := y.rows(i), y.Strides[i]
		p := ptrAt(frame, frameData+uintptr(8*i))
		stride := int(getInt32(frame, frameLinesize+uintptr(4*i)))
		if p == nil || stride < row {
			return fmt.Errorf("video: plane %d of a %dx%d picture in rows of %d bytes", i, y.Width, y.Height, stride)
		}
		src := unsafe.Slice((*byte)(p), stride*(rows-1)+row)
		if stride == row {
			copy(y.Planes[i], src)
			continue
		}
		for r := range rows {
			copy(y.Planes[i][r*row:][:row], src[r*stride:][:row])
		}
	}
	return nil
}

// toI420 converts a picture in another layout to dst's I420, at its size
// and in its colors.
func (d *Decoder) toI420(dst *YUV, src unsafe.Pointer) error {
	l := d.l
	out := d.dst
	colors := [...]uintptr{frameColorRange, frameColorPrim, frameColorTRC, frameColorspace, frameChromaLoc}
	var kept [len(colors)]int32
	for i, off := range colors {
		kept[i] = getInt32(out, off)
	}
	if d.asBT709 {
		tagBT709(src)
	}
	for i := range 3 {
		putPtr(out, frameData+uintptr(8*i), unsafe.Pointer(&dst.Planes[i][0]))
		putInt32(out, frameLinesize+uintptr(4*i), int32(dst.Strides[i]))
	}
	putInt32(out, frameWidth, int32(dst.Width))
	putInt32(out, frameHeight, int32(dst.Height))
	putInt32(out, frameFormat, l.pixYUV420P)
	for _, off := range []uintptr{frameColorPrim, frameColorTRC, frameColorspace, frameChromaLoc} {
		putInt32(out, off, getInt32(src, off))
	}
	rng := int32(avColorRangeMPEG)
	if dst.FullRange {
		rng = avColorRangeJPEG
	}
	putInt32(out, frameColorRange, rng)
	r := l.sws_scale_frame(d.sws, out, src)
	if r < 0 && !d.asBT709 {
		// A matrix swscale cannot convert, as in Convert: read the
		// pictures as BT.709 from now on, which only relabels them here.
		d.asBT709 = true
		tagBT709(src)
		tagBT709(out)
		r = l.sws_scale_frame(d.sws, out, src)
	}
	// out describes dst only during the call. Its colors go back to what
	// they were, FFmpeg's "unspecified" that Convert's RGBA pictures take:
	// zero would be reserved primaries and transfer, which swscale refuses.
	for i := range 3 {
		putPtr(out, frameData+uintptr(8*i), nil)
		putInt32(out, frameLinesize+uintptr(4*i), 0)
	}
	for i, off := range colors {
		putInt32(out, off, kept[i])
	}
	runtime.KeepAlive(dst)
	if r < 0 {
		return l.averror("convert picture to I420", r)
	}
	return nil
}

// Clone copies the picture, as for a screenshot: the player writes its
// planes again.
func (y *YUV) Clone() *YUV {
	c := *y
	for i, p := range y.Planes {
		c.Planes[i] = append([]byte(nil), p...)
	}
	return &c
}

// Equal reports whether two pictures are the same sample for sample, at
// the same size and in the same layout and colors. Phones go on sending a
// still screen for a while, as pictures that decode to the same samples.
// It costs a memory comparison of the planes, a fraction of a millisecond
// for a phone's screen, and stops at the first difference.
func (y *YUV) Equal(o *YUV) bool {
	if y == nil || o == nil {
		return y == o
	}
	if y.NV12 != o.NV12 || y.Width != o.Width || y.Height != o.Height || y.Strides != o.Strides ||
		y.Matrix != o.Matrix || y.FullRange != o.FullRange {
		return false
	}
	for i := range y.Planes {
		if !bytes.Equal(y.Planes[i], o.Planes[i]) {
			return false
		}
	}
	return true
}

// RGBA converts the picture to RGBA as MyGo shows its planes, for a
// screenshot: each chroma sample sits in the middle of the 2×2 pixels it
// covers, and pixels between two take some of each.
func (y *YUV) RGBA() *image.RGBA {
	w, h := y.Width, y.Height
	img := image.NewRGBA(image.Rect(0, 0, max(w, 0), max(h, 0)))
	if w <= 0 || h <= 0 {
		return img
	}
	k := y.coefficients()
	cw, ch := (w+1)/2, (h+1)/2
	cb, cr := y.Planes[1], y.Planes[2]
	cbAt, crAt, step := 0, 0, 1
	cStride := y.Strides[1]
	if y.NV12 {
		cr, crAt, step = cb, 1, 2
	}
	crStride := y.Strides[2]
	if y.NV12 {
		crStride = cStride
	}
	// The chroma of a row, between the two chroma rows nearest it: 4×.
	vcb, vcr := make([]int32, cw), make([]int32, cw)
	for row := range h {
		// Rows 2i and 2i+1 of the picture sit a quarter of a chroma row
		// above and below chroma row i.
		r0, w0, w1 := (row-1)>>1, int32(1), int32(3)
		if row%2 == 1 {
			w0, w1 = 3, 1
		}
		a, b := clampTo(r0, ch), clampTo(r0+1, ch)
		for j := range cw {
			vcb[j] = w0*int32(cb[a*cStride+j*step+cbAt]) + w1*int32(cb[b*cStride+j*step+cbAt])
			vcr[j] = w0*int32(cr[a*crStride+j*step+crAt]) + w1*int32(cr[b*crStride+j*step+crAt])
		}
		luma := y.Planes[0][row*y.Strides[0]:][:w]
		out := img.Pix[row*img.Stride:][:4*w]
		for x := range w {
			c0, u0, u1 := (x-1)>>1, int32(1), int32(3)
			if x%2 == 1 {
				u0, u1 = 3, 1
			}
			p, q := clampTo(c0, cw), clampTo(c0+1, cw)
			// 16× the chroma, less 16×128.
			u := u0*vcb[p] + u1*vcb[q] - 16*128
			v := u0*vcr[p] + u1*vcr[q] - 16*128
			l := k.y * (int32(luma[x]) - k.yOff)
			o := out[4*x:][:4]
			o[0] = to8(l + (k.rv*v)>>4)
			o[1] = to8(l + (k.gu*u+k.gv*v)>>4)
			o[2] = to8(l + (k.bu*u)>>4)
			o[3] = 255
		}
	}
	return img
}

// yuvCoefficients turn samples into RGB, in 1/65536ths.
type yuvCoefficients struct {
	y, yOff        int32
	rv, gu, gv, bu int32
}

func (y *YUV) coefficients() yuvCoefficients {
	kr, kb := 0.299, 0.114
	switch y.Matrix {
	case BT709:
		kr, kb = 0.2126, 0.0722
	case BT2020:
		kr, kb = 0.2627, 0.0593
	}
	kg := 1 - kr - kb
	ys, yOff, cs := 255.0/219, int32(16), 255.0/224
	if y.FullRange {
		ys, yOff, cs = 1, 0, 1
	}
	fix := func(v float64) int32 { return int32(v*65536 + 0.5*sign(v)) }
	return yuvCoefficients{
		y: fix(ys), yOff: yOff,
		rv: fix(2 * (1 - kr) * cs),
		gu: fix(-2 * kb * (1 - kb) / kg * cs),
		gv: fix(-2 * kr * (1 - kr) / kg * cs),
		bu: fix(2 * (1 - kb) * cs),
	}
}

func sign(v float64) float64 {
	if v < 0 {
		return -1
	}
	return 1
}

func clampTo(i, n int) int {
	return min(max(i, 0), n-1)
}

// to8 rounds a value in 1/65536ths to a byte.
func to8(v int32) byte {
	v = (v + 1<<15) >> 16
	return byte(min(max(v, 0), 255))
}
