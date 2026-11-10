package ios

import (
	"context"
	"encoding/binary"
	"fmt"
	"hash/fnv"
	"image"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tetratorus/lightpinky/internal/driver"
)

// The paths below iOS 27 get the phone's picture as separate frames (JPEG
// from WebDriverAgent's MJPEG server, or screenshots) and encode them to
// H.264 for the mirror and the recorder, which take only H.264 and HEVC.

// frame is one picture from a frameSource. key identifies its encoded
// bytes, so a picture that did not change is not decoded or encoded again.
// turn puts the picture the right way up.
type frame struct {
	key    uint64
	decode func() (image.Image, error)
	turn   quarter
}

// frameSource yields the phone's picture, one frame at a time.
type frameSource interface {
	next(ctx context.Context) (frame, error)
	Close() error
}

// Stream settings for the paths below iOS 27.
const (
	frameFPS     = 15
	frameBitRate = 2_500_000
	// frameRefresh is how often an unchanged picture is sent again as a key
	// frame, so a viewer that joins late gets a picture without waiting
	// for the screen to change. The phone's own stream on iOS 27 sends
	// nothing while still too.
	frameRefresh = 10 * time.Second
)

// frameStream is a driver.Stream of H.264 encoded from a frameSource.
type frameStream struct {
	src    frameSource
	cancel context.CancelFunc
	pk     chan driver.Packet
	done   chan struct{}
	w, h   int
	turn   atomic.Int32 // the last frame's turn, plus one; 0 before the first

	mu  sync.Mutex
	err error
}

// startFrameStream opens a source and starts encoding. It returns once the
// first frame is encoded, so Size is known.
func startFrameStream(ctx context.Context, open func(context.Context) (frameSource, error), screenW, screenH int) (driver.Stream, error) {
	if err := h264Available(); err != nil {
		return nil, err
	}
	sctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	src, err := open(sctx)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("video: %w", err)
	}
	s := &frameStream{src: src, cancel: cancel, pk: make(chan driver.Packet, 64), done: make(chan struct{})}
	first := make(chan error, 1)
	go s.run(sctx, first)
	select {
	case err := <-first:
		if err != nil {
			s.Close()
			return nil, fmt.Errorf("video: %w", err)
		}
		return s, nil
	case <-ctx.Done():
		s.Close()
		return nil, ctx.Err()
	}
}

func (s *frameStream) run(ctx context.Context, first chan<- error) {
	defer close(s.done)
	defer close(s.pk)
	var (
		enc     *h264Encoder
		lastKey uint64
		lastAt  time.Time
		start   = time.Now()
		started bool
	)
	defer func() {
		if enc != nil {
			enc.Close()
		}
	}()
	fail := func(err error) {
		s.mu.Lock()
		if s.err == nil {
			s.err = err
		}
		s.mu.Unlock()
		if !started {
			first <- err
		}
	}
	for {
		f, err := s.src.next(ctx)
		if err != nil {
			if ctx.Err() != nil {
				err = nil
			}
			fail(err)
			return
		}
		now := time.Now()
		refresh := started && now.Sub(lastAt) >= frameRefresh
		if started && f.key == lastKey && !refresh {
			continue
		}
		img, err := f.decode()
		if err != nil {
			continue // a damaged frame; the next one will do
		}
		y, uv, w, h := toNV12(img)
		if w == 0 {
			continue
		}
		y, uv, w, h = turnNV12(y, uv, w, h, f.turn)
		s.turn.Store(int32(f.turn) + 1)
		if enc != nil && (enc.w != w || enc.h != h) {
			enc.Close() // the picture changed size (rotation)
			enc = nil
		}
		forceKey := refresh
		if enc == nil {
			if enc, err = newH264Encoder(w, h, frameFPS, frameBitRate); err != nil {
				fail(err)
				return
			}
			forceKey = true
			s.mu.Lock()
			s.w, s.h = w, h
			s.mu.Unlock()
		}
		out, err := enc.encode(y, uv, now.Sub(start), forceKey)
		if err != nil {
			fail(err)
			return
		}
		lastKey, lastAt = f.key, now
		for _, e := range out {
			pts := now.Sub(start)
			if e.keyFrame && len(e.config) > 0 {
				if !s.send(ctx, driver.Packet{Data: e.config, PTS: pts, Config: true}) {
					return
				}
			}
			if !s.send(ctx, driver.Packet{Data: e.data, PTS: pts, KeyFrame: e.keyFrame}) {
				return
			}
			if !started && e.keyFrame {
				started = true
				first <- nil
			}
		}
	}
}

func (s *frameStream) send(ctx context.Context, p driver.Packet) bool {
	select {
	case s.pk <- p:
		return true
	case <-ctx.Done():
		return false
	}
}

func (s *frameStream) Codec() driver.Codec { return driver.H264 }

// lastTurn is the turn that put the last frame upright; ok is false before
// the first frame.
func (s *frameStream) lastTurn() (q quarter, ok bool) {
	n := s.turn.Load()
	return quarter(n - 1), n > 0
}

func (s *frameStream) Size() (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w, s.h
}

func (s *frameStream) Packets() <-chan driver.Packet { return s.pk }

func (s *frameStream) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

func (s *frameStream) Close() error {
	s.cancel()
	err := s.src.Close()
	<-s.done
	return err
}

// toNV12 converts a picture to NV12 planes, cropped to even sizes: the Y
// plane, then Cb and Cr interleaved at half size. JPEG's 4:2:0 pictures
// convert without arithmetic; others go through RGB.
func toNV12(img image.Image) (y, uv []byte, w, h int) {
	b := img.Bounds()
	w, h = b.Dx()&^1, b.Dy()&^1
	if w <= 0 || h <= 0 {
		return nil, nil, 0, 0
	}
	y = make([]byte, w*h)
	uv = make([]byte, w*h/2)
	if yc, ok := img.(*image.YCbCr); ok && yc.SubsampleRatio == image.YCbCrSubsampleRatio420 {
		for row := 0; row < h; row++ {
			copy(y[row*w:(row+1)*w], yc.Y[yc.YOffset(b.Min.X, b.Min.Y+row):])
		}
		for row := 0; row < h/2; row++ {
			ci := yc.COffset(b.Min.X, b.Min.Y+2*row)
			dst := uv[row*w : (row+1)*w]
			for col := 0; col < w/2; col++ {
				dst[2*col] = yc.Cb[ci+col]
				dst[2*col+1] = yc.Cr[ci+col]
			}
		}
		return y, uv, w, h
	}
	for row := 0; row < h; row += 2 {
		for col := 0; col < w; col += 2 {
			var cb, cr int
			for dy := 0; dy < 2; dy++ {
				for dx := 0; dx < 2; dx++ {
					r, g, bl, _ := img.At(b.Min.X+col+dx, b.Min.Y+row+dy).RGBA()
					r8, g8, b8 := int(r>>8), int(g>>8), int(bl>>8)
					// BT.601 video range, as '420v' expects.
					y[(row+dy)*w+col+dx] = clamp8((66*r8+129*g8+25*b8+128)>>8 + 16)
					cb += (-38*r8 - 74*g8 + 112*b8 + 128) >> 8
					cr += (112*r8 - 94*g8 - 18*b8 + 128) >> 8
				}
			}
			uv[(row/2)*w+col] = clamp8(cb/4 + 128)
			uv[(row/2)*w+col+1] = clamp8(cr/4 + 128)
		}
	}
	return y, uv, w, h
}

func clamp8(v int) byte {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return byte(v)
}

// frameKey hashes a frame's encoded bytes.
func frameKey(b []byte) uint64 {
	h := fnv.New64a()
	h.Write(b)
	return h.Sum64()
}

// avccToAnnexB turns NAL units prefixed by their length (lenSize bytes, big
// endian) into Annex B units with start codes.
func avccToAnnexB(b []byte, lenSize int) []byte {
	out := make([]byte, 0, len(b)+16)
	for len(b) >= lenSize {
		var n int
		switch lenSize {
		case 4:
			n = int(binary.BigEndian.Uint32(b))
		case 2:
			n = int(binary.BigEndian.Uint16(b))
		default:
			return out
		}
		b = b[lenSize:]
		if n <= 0 || n > len(b) {
			break
		}
		out = append(out, 0, 0, 0, 1)
		out = append(out, b[:n]...)
		b = b[n:]
	}
	return out
}

// hasIDR reports whether Annex B data holds an IDR slice (NAL type 5).
func hasIDR(b []byte) bool {
	for i := 0; i+3 < len(b); i++ {
		if b[i] == 0 && b[i+1] == 0 && b[i+2] == 1 {
			if b[i+3]&0x1f == 5 {
				return true
			}
			i += 2
		}
	}
	return false
}
