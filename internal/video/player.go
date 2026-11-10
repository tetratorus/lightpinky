package video

import (
	"bytes"
	"image"
	"io"
	"sync"
	"time"

	"github.com/tetratorus/lightpinky/internal/driver"
)

// Frame is a decoded picture, converted to RGBA or as its planes.
type Frame struct {
	// Image holds the picture in RGBA, scaled to fit the size the player
	// was asked for; nil with PlayerOptions.YUV. It belongs to the player:
	// it stays as it is until the next Take, after which the player may
	// write into it again (two conversions later at the earliest).
	Image *image.RGBA
	// YUV holds the picture's planes at the stream's size with
	// PlayerOptions.YUV, else nil. It belongs to the player as Image does.
	YUV *YUV
	// Width and Height are the stream's picture size, in the device's
	// pixels, whatever size Image has.
	Width, Height int
	Seq           uint64        // counts the pictures decoded and handed over
	PTS           time.Duration // the packet's PTS
	// Turn is how many quarter turns clockwise put the picture the right
	// way up (driver.Packet.Turn), from the packet it was decoded from.
	Turn     int
	Received time.Time // when its packet arrived
	Ready    time.Time // when it was converted
}

// PlayerOptions tune a Player.
type PlayerOptions struct {
	Decoder DecoderOptions
	// OnFrame is called on the player's goroutine after each new picture,
	// and once more when the stream ends. A UI wakes its window from it
	// and then calls Take on its own thread.
	OnFrame func()
	// MaxBacklog is the number of queued packets beyond which the player
	// skips to the next key frame to catch up; default 30.
	MaxBacklog int
	// YUV hands pictures over as their Y′CbCr planes (Frame.YUV) at the
	// stream's size, copied as the decoder gives them, for a UI that
	// converts and scales them as it draws them (MyGo's Bitmap.UpdateYUV):
	// the least CPU a picture can cost. SetSize then changes nothing.
	YUV bool
	// SkipRepeats hands over no picture that is the same, sample for
	// sample, as the one before, and calls no OnFrame for it. Phones go on
	// sending pictures for a while after their screen stops changing (an
	// iPhone 20 to 30 a second for 5 to 7 s); a mirror that woke for each
	// would paint as if the screen moved. A recording's player keeps them,
	// as their PTS move its playhead.
	SkipRepeats bool
}

// Stats describe a player's work so far.
type Stats struct {
	Hardware   string        // "videotoolbox", "d3d11va", "vaapi", or "" for software
	Decoded    uint64        // pictures decoded and converted, repeats aside
	Repeats    uint64        // pictures the same as the one before, not handed over (SkipRepeats)
	Shown      uint64        // pictures handed to Take
	Dropped    uint64        // pictures replaced before Take saw them
	Skipped    uint64        // packets skipped to catch up
	Idle       uint64        // pictures decoded but not converted, as nobody took any
	Errors     uint64        // packets the decoder rejected
	Decode     time.Duration // mean time from a packet's arrival to its picture out of the decoder
	Convert    time.Duration // mean time to convert a picture to RGBA, or to copy its planes
	Latency    time.Duration // mean time from a packet's arrival to its picture being ready
	FPS        float64       // pictures ready per second, over the last second or so
	OutW, OutH int           // the size pictures are converted to
}

// Player decodes a live stream on a goroutine of its own and keeps the
// newest picture for the UI, so the mirror always shows the latest one and
// never queues behind. It triple-buffers: the picture the UI shows, the
// newest one waiting for it, and the one being converted.
type Player struct {
	stream driver.Stream
	opt    PlayerOptions

	mu       sync.Mutex
	wantW    int // the size to fit pictures into, in pixels; 0 = the stream's
	wantH    int
	front    *Frame // returned by the last Take
	pending  *Frame // the newest picture, not yet taken
	spare    *Frame // free for the decoder
	ready    bool   // pending holds a picture Take has not returned
	err      error
	stats    Stats
	sumDec   time.Duration
	sumConv  time.Duration
	sumLat   time.Duration
	fpsCount int
	fpsStart time.Time
	last     *Frame // the picture handed over last; the player's goroutine's only

	done      chan struct{}
	closeOnce sync.Once
}

// NewPlayer starts decoding stream. The player owns the stream and closes
// it with Close.
func NewPlayer(stream driver.Stream, opt PlayerOptions) (*Player, error) {
	dec, err := NewDecoder(stream.Codec(), opt.Decoder)
	if err != nil {
		return nil, err
	}
	if opt.MaxBacklog <= 0 {
		opt.MaxBacklog = 30
	}
	p := &Player{stream: stream, opt: opt, done: make(chan struct{}), fpsStart: time.Now()}
	go p.run(dec)
	return p, nil
}

// SetSize sets the size, in pixels, to fit pictures into, keeping their
// aspect ratio; pictures are never scaled up. Zero means the stream's own
// size. A mirror sets the pixel size it shows the phone at, so the GPU
// uploads no more than it draws.
func (p *Player) SetSize(w, h int) {
	p.mu.Lock()
	p.wantW, p.wantH = max(w, 0), max(h, 0)
	p.mu.Unlock()
}

// Take returns the newest picture decoded since the last Take, or nil.
// The picture it returned before may be written again from now on.
func (p *Player) Take() *Frame {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.ready {
		return nil
	}
	p.front, p.pending = p.pending, p.front
	p.ready = false
	p.stats.Shown++
	return p.front
}

// Pending reports whether a picture waits for Take.
func (p *Player) Pending() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.ready
}

// Done is closed when the stream ended or failed.
func (p *Player) Done() <-chan struct{} { return p.done }

// Err returns why the stream ended: nil while it plays, io.EOF at its end.
func (p *Player) Err() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}

// Stats returns the player's counters.
func (p *Player) Stats() Stats {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.stats
	if n := time.Duration(s.Decoded); n > 0 {
		s.Decode = p.sumDec / n
		s.Convert = p.sumConv / n
		s.Latency = p.sumLat / n
	}
	return s
}

// Close stops decoding, closes the stream and waits for the decoder.
func (p *Player) Close() {
	p.closeOnce.Do(func() {
		p.stream.Close()
		<-p.done
	})
}

func (p *Player) run(dec *Decoder) {
	defer close(p.done)
	defer dec.Close()
	notify := func() {
		if p.opt.OnFrame != nil {
			p.opt.OnFrame()
		}
	}
	packets := p.stream.Packets()
	skipping := false
	for pkt := range packets {
		received := time.Now()
		turn := pkt.Turn
		// Behind by more than the backlog: drop everything up to the next
		// key frame, which the decoder can start from.
		if !skipping && len(packets) > p.opt.MaxBacklog {
			skipping = true
		}
		if skipping {
			if !pkt.KeyFrame && !pkt.Config {
				p.mu.Lock()
				p.stats.Skipped++
				p.mu.Unlock()
				continue
			}
			if pkt.KeyFrame {
				skipping = false
				dec.Flush()
			}
		}
		if err := dec.Send(pkt); err != nil {
			p.mu.Lock()
			p.stats.Errors++
			p.mu.Unlock()
			continue
		}
		for {
			pic, ok, err := dec.Receive()
			if err != nil {
				p.mu.Lock()
				p.stats.Errors++
				p.mu.Unlock()
				break
			}
			if !ok {
				break
			}
			decoded := time.Now()
			if p.convert(dec, pic, turn, received, decoded) {
				notify()
			}
		}
	}
	err := p.stream.Err()
	if err == nil {
		err = io.EOF
	}
	p.mu.Lock()
	p.err = err
	p.mu.Unlock()
	notify()
}

// convert writes the picture into the spare buffer and publishes it.
// While nobody takes pictures (a window out of sight paints nothing), it
// keeps the one waiting and converts nothing: decoding goes on, as later
// pictures depend on earlier ones. Nobody takes them when the picture
// waiting has waited for over a second, not when Take has not been called
// for a while: a mirror of a still screen calls it only when OnFrame woke it.
func (p *Player) convert(dec *Decoder, pic Picture, turn int, received, decoded time.Time) bool {
	p.mu.Lock()
	if p.ready && time.Since(p.pending.Ready) > time.Second {
		p.stats.Idle++
		p.mu.Unlock()
		return false
	}
	w, h := fitSize(pic.Width, pic.Height, p.wantW, p.wantH)
	f := p.spare
	p.spare = nil
	p.mu.Unlock()
	var err error
	if p.opt.YUV {
		if f == nil || f.YUV == nil {
			f = &Frame{YUV: &YUV{}}
		}
		err = dec.ConvertYUV(f.YUV)
		w, h = f.YUV.Width, f.YUV.Height
	} else {
		if f == nil || f.Image == nil || f.Image.Rect.Dx() != w || f.Image.Rect.Dy() != h {
			f = &Frame{Image: image.NewRGBA(image.Rect(0, 0, w, h))}
		}
		err = dec.Convert(f.Image)
	}
	if err != nil {
		p.mu.Lock()
		p.stats.Errors++
		p.spare = f
		p.mu.Unlock()
		return false
	}
	now := time.Now()
	f.Width, f.Height = pic.Width, pic.Height
	f.Turn = turn & 3
	f.PTS, f.Received, f.Ready = pic.PTS, received, now
	// The last picture stays as it is until this one is handed over: the
	// UI only reads it, and the player writes only the spare.
	if p.opt.SkipRepeats && samePicture(f, p.last) {
		p.mu.Lock()
		p.stats.Repeats++
		p.spare = f
		p.mu.Unlock()
		return false
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	p.last = f
	p.stats.Decoded++
	f.Seq = p.stats.Decoded
	p.stats.Hardware = dec.Hardware()
	p.stats.OutW, p.stats.OutH = w, h
	p.sumDec += decoded.Sub(received)
	p.sumConv += now.Sub(decoded)
	p.sumLat += now.Sub(received)
	p.fpsCount++
	if el := now.Sub(p.fpsStart); el >= time.Second {
		p.stats.FPS = float64(p.fpsCount) / el.Seconds()
		p.fpsCount, p.fpsStart = 0, now
	}
	if p.ready {
		p.stats.Dropped++ // the UI had not taken the last one
	}
	// The old pending picture, taken or not, becomes the spare.
	p.spare, p.pending = p.pending, f
	p.ready = true
	return true
}

// samePicture reports whether a and b show the same picture the same way.
func samePicture(a, b *Frame) bool {
	if a == nil || b == nil || a.Width != b.Width || a.Height != b.Height || a.Turn != b.Turn {
		return false
	}
	if a.YUV != nil || b.YUV != nil {
		return a.YUV.Equal(b.YUV)
	}
	return a.Image != nil && b.Image != nil && a.Image.Rect == b.Image.Rect &&
		a.Image.Stride == b.Image.Stride && bytes.Equal(a.Image.Pix, b.Image.Pix)
}

// fitSize returns the size to convert a w×h picture to so it fits inside
// maxW×maxH without growing; even, as chroma subsampling prefers.
func fitSize(w, h, maxW, maxH int) (int, int) {
	if maxW <= 0 || maxH <= 0 || (w <= maxW && h <= maxH) {
		return w, h
	}
	sw := float64(maxW) / float64(w)
	sh := float64(maxH) / float64(h)
	s := min(sw, sh)
	ow := max(2, int(float64(w)*s+0.5)&^1)
	oh := max(2, int(float64(h)*s+0.5)&^1)
	return ow, oh
}
