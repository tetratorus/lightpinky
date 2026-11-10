package ios

import (
	"context"
	"sync"
	"time"
)

// While the display stream runs, iOS tells apps that the screen is being
// shared (UIScreen.isCaptured), shows the screen-sharing indicator and
// keeps the camera and microphone from apps, and some banking apps refuse
// to run at all (DBS digibank, China Merchants Bank; seen in the field).
// HID input needs the stream, so it cannot stay off while Pinky types or
// taps, but nothing needs it while nobody watches the phone in Pinky,
// nothing records it and Pinky sends no input: then it stops, and the next
// view or input starts it again (about a second: see restartMedia).

// streamIdle is how long the stream runs on with no view open and no
// input from Pinky. Agents usually act within seconds of each other, and a
// task's recording keeps a view open for the whole task, so the stream
// rarely stops in the middle of one.
const streamIdle = 30 * time.Second

// idleCheck is how often the idle stream is looked for.
const idleCheck = 5 * time.Second

// watchedStream is a view of the stream that the device counts while it
// is open.
type watchedStream struct {
	*mediaStream
	d    *Device
	once sync.Once
}

func (w *watchedStream) Close() error {
	w.once.Do(func() { w.d.unwatch() })
	return w.mediaStream.Close()
}

// watch counts a view that is about to open. Callers that fail to open it
// call unwatch.
func (d *Device) watch() {
	d.mu.Lock()
	d.watchers++
	d.watchedAt = time.Now()
	d.mu.Unlock()
}

func (d *Device) unwatch() {
	d.mu.Lock()
	d.watchers--
	d.watchedAt = time.Now()
	d.mu.Unlock()
}

// idleLoop stops the stream once it is idle, until the device closes.
func (d *Device) idleLoop() {
	t := time.NewTicker(idleCheck)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			d.stopIfIdle(time.Now())
		case <-d.done:
			return
		}
	}
}

// orientPoll is how often the display's orientation is read while anyone
// watches the video: the packets carry it, so the mirror turns with the
// device within about this long.
const orientPoll = time.Second

// orientLoop reads the display's orientation every orientPoll while a view
// of the video is open. Nothing else tells Pinky the device turned: the
// picture keeps the display's own frame and size. Nor that a foldable was
// folded or unfolded: the stream then goes on showing the display put
// away, and starts again on the one in use (live), which ends the views
// open on it; the mirror opens a new one.
func (d *Device) orientLoop() {
	t := time.NewTicker(orientPoll)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			d.mu.Lock()
			watched := d.watchers > 0 && d.m != nil
			d.mu.Unlock()
			if watched {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				d.readDisplayNow(ctx)
				cancel()
				if d.displayMoved() {
					d.live(context.Background())
				}
			}
		case <-d.done:
			return
		}
	}
}

// stopIfIdle stops the stream and the HID session on it when no view has
// been open and no input sent for streamIdle. It reports whether it did.
func (d *Device) stopIfIdle(now time.Time) bool {
	d.restartMu.Lock()
	defer d.restartMu.Unlock()
	m, h := d.takeIdle(now)
	if m == nil {
		return false
	}
	if h != nil {
		h.Close()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	m.stop(ctx)
	cancel()
	return true
}

// takeIdle detaches the stream and its HID session when they are idle, for
// the caller to stop; the next view or input starts new ones (see live).
// Callers hold restartMu.
func (d *Device) takeIdle(now time.Time) (*media, *hidSession) {
	// Input in progress holds inputMu; it is not idle then.
	if !d.inputMu.TryLock() {
		return nil, nil
	}
	defer d.inputMu.Unlock()
	d.mu.Lock()
	defer d.mu.Unlock()
	m, h := d.m, d.hid
	if d.closed || m == nil || m.stopped.Load() || d.watchers > 0 || !idleSince(now, streamIdle, d.watchedAt, d.lastInputAt(), m.started) {
		return nil, nil
	}
	d.m, d.hid = nil, nil
	return m, h
}

// idleSince reports whether at least idle passed since the latest of ts.
func idleSince(now time.Time, idle time.Duration, ts ...time.Time) bool {
	var last time.Time
	for _, t := range ts {
		if t.After(last) {
			last = t
		}
	}
	return now.Sub(last) >= idle
}
