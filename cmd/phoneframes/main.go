// Command phoneframes streams an iPhone's screen as raw frames on stdout
// for ascii-screen's --frames mode: a "W H" line, then W*H*4 bytes of
// RGBA, repeated at most every -interval and only while the phone is
// unlocked (however it was unlocked). The stream is the same live video
// the mirror uses, so an unchanged screen costs almost nothing.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"image"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/tetratorus/lightpinky/internal/driver"
	"github.com/tetratorus/lightpinky/internal/driver/ios"
	"github.com/tetratorus/lightpinky/internal/video"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "phoneframes:", err)
		os.Exit(1)
	}
}

func phonesFile() string {
	d, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(d, "lightpinky", "phones.json")
}

func run() error {
	device := flag.String("device", "", "phone UDID; default: the only online iPhone")
	interval := flag.Duration("interval", 500*time.Millisecond, "minimum time between emitted frames")
	wifiOnly := flag.Bool("wifi-only", false, "use Wi-Fi even when USB is connected; never fall back to USB")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	p := ios.NewProvider()
	p.WiFiOnly = *wifiOnly
	p.PhonesFile = phonesFile()
	p.UseSavedPhones()
	p.WiFiMux = true
	p.SetWiFi(true)

	// Discovery over Wi-Fi takes a few seconds; wait for an online phone.
	var info *driver.Info
	deadline := time.Now().Add(60 * time.Second)
	for info == nil {
		infos, err := p.List(ctx)
		if err != nil {
			return err
		}
		for i := range infos {
			in := &infos[i]
			if *device != "" {
				if in.ID == *device {
					info = in
				}
			} else if in.State == driver.StateOnline {
				if info != nil {
					return fmt.Errorf("several iPhones are online; pass -device")
				}
				info = in
			}
		}
		if info == nil {
			if time.Now().After(deadline) {
				return fmt.Errorf("no online iPhone found")
			}
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(2 * time.Second):
			}
		}
	}
	if info.State != driver.StateOnline {
		return fmt.Errorf("%s is %s; unlock the phone first", info.ID, info.State)
	}
	d, err := p.Open(ctx, info.ID)
	if err != nil {
		return err
	}
	defer d.Close()

	stream, player, err := openStream(ctx, d)
	if err != nil {
		return err
	}

	out := bufio.NewWriterSize(os.Stdout, 1<<20)
	defer out.Flush()

	var lastEmit, lastLockCheck time.Time
	locked := false
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
	emit:
		for {
			select {
			case <-ctx.Done():
				return nil
			case <-player.Done():
				break emit
			case <-tick.C:
			}
			if !player.Pending() || time.Since(lastEmit) < *interval {
				continue
			}
			// The lock state costs a lockdown round trip; check it at most
			// twice a second, and always before the first frame.
			if time.Since(lastLockCheck) >= 500*time.Millisecond || lastEmit.IsZero() {
				lastLockCheck = time.Now()
				if lr, ok := d.(driver.LockReporter); ok {
					if v, err := lr.Locked(ctx); err == nil {
						locked = v
					}
				}
			}
			fr := player.Take()
			if fr == nil || fr.Image == nil || locked {
				continue
			}
			img := rotateRGBA(fr.Image, fr.Turn)
			b := img.Bounds()
			if len(img.Pix) != b.Dx()*b.Dy()*4 {
				return fmt.Errorf("frame has unexpected pixel layout")
			}
			if _, err := fmt.Fprintf(out, "%d %d\n", b.Dx(), b.Dy()); err != nil {
				return err
			}
			if _, err := out.Write(bgra(img.Pix)); err != nil {
				return err
			}
			if err := out.Flush(); err != nil {
				return err
			}
			lastEmit = time.Now()
		}
		// The stream ended (the phone locked, the tunnel dropped): poll the
		// lock state cheaply once a second and only pay for a stream reopen
		// once the phone is actually unlocked. A failed reopen backs off to 5s.
		if stream != nil {
			stream.Close()
			stream = nil
		}
		backoff := time.Second
		for {
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(backoff):
			}
			if lr, ok := d.(driver.LockReporter); ok {
				if v, err := lr.Locked(ctx); err == nil && v {
					backoff = time.Second
					continue
				}
			}
			ns, np, oerr := openStream(ctx, d)
			if oerr != nil {
				backoff *= 2
				if backoff > 5*time.Second {
					backoff = 5 * time.Second
				}
				continue
			}
			stream, player = ns, np
			break
		}
	}
}

// openStream starts the device's live video and its player; the device
// stays open across reconnects.
func openStream(ctx context.Context, d driver.Device) (driver.Stream, *video.Player, error) {
	stream, err := d.Stream(ctx, driver.StreamOptions{})
	if err != nil {
		return nil, nil, err
	}
	player, err := video.NewPlayer(stream, video.PlayerOptions{SkipRepeats: true})
	if err != nil {
		stream.Close()
		return nil, nil, err
	}
	return stream, player, nil
}

// bgra swaps RGBA pixels to BGRA, the layout CVPixelBuffer wants.
func bgra(pix []byte) []byte {
	out := make([]byte, len(pix))
	for i := 0; i+3 < len(pix); i += 4 {
		out[i], out[i+1], out[i+2], out[i+3] = pix[i+2], pix[i+1], pix[i], 255
	}
	return out
}

// rotateRGBA turns src clockwise by turns quarter turns (0-3).
func rotateRGBA(src *image.RGBA, turns int) *image.RGBA {
	switch turns & 3 {
	case 0:
		return src
	case 1:
		return turnRGBA(src, true)
	case 2:
		return turnRGBA(turnRGBA(src, true), true)
	default:
		return turnRGBA(turnRGBA(turnRGBA(src, true), true), true)
	}
}

func turnRGBA(src *image.RGBA, cw bool) *image.RGBA {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	dst := image.NewRGBA(image.Rect(0, 0, h, w))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			dx, dy := y, w-1-x
			if cw {
				dx, dy = h-1-y, x
			}
			dst.Set(dx, dy, src.At(x, y))
		}
	}
	return dst
}
