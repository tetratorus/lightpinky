package main

import (
	"image"
	"image/color"
	"testing"
)

func TestRotateFramesUprightBeforeOCR(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 2, 3))
	for y := 0; y < 3; y++ {
		for x := 0; x < 2; x++ {
			src.SetRGBA(x, y, color.RGBA{R: uint8(y*2 + x + 1), A: 255})
		}
	}
	for _, tc := range []struct {
		turn, width, height int
		pixels              []uint8
	}{
		{0, 2, 3, []uint8{1, 2, 3, 4, 5, 6}},
		{1, 3, 2, []uint8{5, 3, 1, 6, 4, 2}},
		{2, 2, 3, []uint8{6, 5, 4, 3, 2, 1}},
		{3, 3, 2, []uint8{2, 4, 6, 1, 3, 5}},
	} {
		got := rotateRGBA(src, tc.turn)
		if got.Bounds().Dx() != tc.width || got.Bounds().Dy() != tc.height {
			t.Fatalf("turn %d: wrong dimensions %v", tc.turn, got.Bounds())
		}
		for i, want := range tc.pixels {
			if pixel := got.RGBAAt(i%tc.width, i/tc.width).R; pixel != want {
				t.Errorf("turn %d, pixel %d: got %d, want %d", tc.turn, i, pixel, want)
			}
		}
	}
}
