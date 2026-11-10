package video

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/draw"
	"io"
	"os/exec"
	"runtime"
	"strconv"
	"sync"
	"time"

	"github.com/tetratorus/lightpinky/internal/driver"
)

// TestStreamOptions describe a synthetic stream made by the ffmpeg command
// line tool from one of its test sources. Demos and tests use it in place
// of a phone, so no real screen is ever recorded.
type TestStreamOptions struct {
	Codec         driver.Codec // H264 (default) or HEVC
	Width, Height int          // default 1080×2400
	FPS           int          // default 60
	// Source is an ffmpeg lavfi source without its size and rate, such as
	// "testsrc2" (default), "testsrc", "smptehdbars" or "rgbtestsrc".
	Source string
	// Bitrate in bits per second; default 8 Mbit/s, scrcpy's default.
	Bitrate int
	// FFmpeg is the ffmpeg binary; default "ffmpeg" from PATH.
	FFmpeg string
	// GOP is the key frame interval in frames; default one second.
	GOP int
	// Matrix tags the stream's VUI with these matrix coefficients (ITU-T
	// H.273) without converting the pixels; 8 (YCgCo) reproduces the Mi 10
	// Pro's encoder. Zero leaves the tag unset.
	Matrix int
}

// TestStream is a driver.Stream of an ffmpeg test source, encoded live and
// paced in real time. It sends packets as scrcpy does: a configuration
// packet, then one packet per picture.
type TestStream struct {
	opt     TestStreamOptions
	cmd     *exec.Cmd
	cancel  context.CancelFunc
	packets chan driver.Packet
	stderr  bytes.Buffer

	mu     sync.Mutex
	err    error
	closed bool
	done   chan struct{}
}

// StartTestStream starts ffmpeg and returns its stream. It fails at once
// when ffmpeg is missing.
func StartTestStream(ctx context.Context, opt TestStreamOptions) (*TestStream, error) {
	if opt.Codec == "" {
		opt.Codec = driver.H264
	}
	if opt.Width <= 0 || opt.Height <= 0 {
		opt.Width, opt.Height = 1080, 2400
	}
	opt.Width, opt.Height = opt.Width&^1, opt.Height&^1
	if opt.FPS <= 0 {
		opt.FPS = 60
	}
	if opt.Source == "" {
		opt.Source = "testsrc2"
	}
	if opt.Bitrate <= 0 {
		opt.Bitrate = 8_000_000
	}
	if opt.GOP <= 0 {
		opt.GOP = opt.FPS
	}
	bin := opt.FFmpeg
	if bin == "" {
		bin = "ffmpeg"
	}
	path, err := exec.LookPath(bin)
	if err != nil {
		return nil, fmt.Errorf("video: the test stream needs the ffmpeg command: %w", err)
	}

	src := fmt.Sprintf("%s=size=%dx%d:rate=%d", opt.Source, opt.Width, opt.Height, opt.FPS)
	args := []string{"-hide_banner", "-loglevel", "error", "-nostdin",
		"-re", "-f", "lavfi", "-i", src, "-pix_fmt", "yuv420p", "-g", strconv.Itoa(opt.GOP), "-bf", "0",
		"-b:v", strconv.Itoa(opt.Bitrate), "-maxrate", strconv.Itoa(opt.Bitrate), "-bufsize", strconv.Itoa(opt.Bitrate / 2)}
	tag := ""
	if opt.Matrix > 0 {
		tag = ":matrix_coefficients=" + strconv.Itoa(opt.Matrix)
	}
	switch opt.Codec {
	case driver.H264:
		args = append(args, "-c:v", "libx264", "-preset", "ultrafast", "-tune", "zerolatency",
			"-bsf:v", "h264_metadata=aud=insert"+tag, "-f", "h264", "-")
	case driver.HEVC:
		if runtime.GOOS == "darwin" {
			args = append(args, "-c:v", "hevc_videotoolbox", "-realtime", "1", "-prio_speed", "1")
		} else {
			args = append(args, "-c:v", "libx265", "-preset", "ultrafast", "-tune", "zerolatency", "-x265-params", "log-level=error")
		}
		args = append(args, "-bsf:v", "hevc_metadata=aud=insert"+tag, "-f", "hevc", "-")
	default:
		return nil, fmt.Errorf("video: unsupported codec %q", opt.Codec)
	}

	return startFFmpeg(ctx, path, args, opt, nil)
}

// startFFmpeg runs ffmpeg with args, which write an Annex B stream with
// access unit delimiters to stdout, and turns it into packets. feed, when
// set, writes ffmpeg's input to its stdin until it fails or ctx ends.
func startFFmpeg(ctx context.Context, path string, args []string, opt TestStreamOptions, feed func(ctx context.Context, w io.Writer) error) (*TestStream, error) {
	cctx, cancel := context.WithCancel(ctx)
	s := &TestStream{opt: opt, cancel: cancel, packets: make(chan driver.Packet, 120), done: make(chan struct{})}
	s.cmd = exec.CommandContext(cctx, path, args...)
	s.cmd.Stderr = &s.stderr
	var in io.WriteCloser
	if feed != nil {
		var err error
		if in, err = s.cmd.StdinPipe(); err != nil {
			cancel()
			return nil, err
		}
	}
	out, err := s.cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	if err := s.cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("video: start ffmpeg: %w", err)
	}
	if feed != nil {
		go func() {
			defer in.Close()
			_ = feed(cctx, in)
		}()
	}
	go s.read(out)
	return s, nil
}

// FrameStream encodes the pictures frame returns, fps times a second, into
// a live H.264 stream through the ffmpeg command, at w×h pixels. Phones
// that draw their screen rather than stream it (internal/demo) use it, so
// the mirror shows them and its touches land in their pixels. It fails at
// once when ffmpeg is missing.
func FrameStream(ctx context.Context, w, h, fps int, frame func() image.Image) (*TestStream, error) {
	if w <= 0 || h <= 0 || w%2 != 0 || h%2 != 0 {
		return nil, fmt.Errorf("video: frame stream size %dx%d must be positive and even", w, h)
	}
	if fps <= 0 {
		fps = 15
	}
	path, err := exec.LookPath("ffmpeg")
	if err != nil {
		return nil, fmt.Errorf("video: the frame stream needs the ffmpeg command: %w", err)
	}
	opt := TestStreamOptions{Codec: driver.H264, Width: w, Height: h, FPS: fps}
	args := []string{"-hide_banner", "-loglevel", "error", "-nostdin",
		"-f", "rawvideo", "-pix_fmt", "rgba", "-s", fmt.Sprintf("%dx%d", w, h), "-r", strconv.Itoa(fps), "-i", "-",
		"-pix_fmt", "yuv420p", "-g", strconv.Itoa(fps * 2), "-bf", "0",
		"-c:v", "libx264", "-preset", "ultrafast", "-tune", "zerolatency",
		"-bsf:v", "h264_metadata=aud=insert", "-f", "h264", "-"}
	// -nostdin only stops ffmpeg reading commands from the terminal; the
	// pictures still come on stdin as the input "-".
	feed := func(ctx context.Context, out io.Writer) error {
		buf := image.NewRGBA(image.Rect(0, 0, w, h))
		tick := time.NewTicker(time.Second / time.Duration(fps))
		defer tick.Stop()
		for {
			if img := frame(); img != nil {
				pix := buf.Pix
				if rgba, ok := img.(*image.RGBA); ok && rgba.Rect == buf.Rect && rgba.Stride == 4*w {
					pix = rgba.Pix
				} else {
					draw.Draw(buf, buf.Rect, img, img.Bounds().Min, draw.Src)
				}
				if _, err := out.Write(pix); err != nil {
					return err
				}
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-tick.C:
			}
		}
	}
	return startFFmpeg(ctx, path, args, opt, feed)
}

func (s *TestStream) read(out io.Reader) {
	defer close(s.done)
	defer close(s.packets)
	split := auSplitter{codec: s.opt.Codec}
	pk := packetizer{codec: s.opt.Codec}
	start := time.Now()
	buf := make([]byte, 64<<10)
	for {
		n, err := out.Read(buf)
		if n > 0 {
			for _, au := range split.push(buf[:n]) {
				for _, p := range pk.packets(au) {
					p.PTS = time.Since(start)
					select {
					case s.packets <- p:
					default:
						// A reader that stopped reading falls behind: drop
						// the packet, as a network would. Pictures until
						// the next key frame may show artifacts.
					}
				}
			}
		}
		if err != nil {
			werr := s.cmd.Wait()
			s.mu.Lock()
			if !s.closed {
				switch {
				case werr != nil && s.stderr.Len() > 0:
					s.err = fmt.Errorf("video: ffmpeg: %v: %s", werr, bytes.TrimSpace(s.stderr.Bytes()))
				case werr != nil:
					s.err = fmt.Errorf("video: ffmpeg: %w", werr)
				case !errors.Is(err, io.EOF):
					s.err = err
				default:
					s.err = io.EOF
				}
			}
			s.mu.Unlock()
			return
		}
	}
}

// Codec implements driver.Stream.
func (s *TestStream) Codec() driver.Codec { return s.opt.Codec }

// Size implements driver.Stream.
func (s *TestStream) Size() (int, int) { return s.opt.Width, s.opt.Height }

// Packets implements driver.Stream.
func (s *TestStream) Packets() <-chan driver.Packet { return s.packets }

// Err implements driver.Stream.
func (s *TestStream) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// Close implements driver.Stream: it stops ffmpeg and waits for it.
func (s *TestStream) Close() error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.cancel()
	<-s.done
	return nil
}

var _ driver.Stream = (*TestStream)(nil)
