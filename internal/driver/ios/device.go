package ios

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tetratorus/lightpinky/internal/driver"
	"github.com/tetratorus/lightpinky/internal/screen"
)

// inputSettle is how long the device needs after a stream starts before it
// routes HID input from this host to the screen; input sent earlier lands on
// a surface that is not authenticated yet and is dropped. pymobiledevice3
// measured 0.1 s to 0.4 s depending on the model.
const inputSettle = 500 * time.Millisecond

// Device is an open session with one iPhone. It keeps one tunnel for its
// whole life, and one video stream with an HID connection on it while
// anyone watches or Pinky sends input (see idle.go).
type Device struct {
	info driver.Info
	l    *link
	log  *slog.Logger

	mu      sync.Mutex // guards the fields below
	m       *media
	hid     *hidSession
	scr     screenInfo // as the phone last described it (screen gives it upright)
	scrAt   time.Time  // when scr was read
	inputAt time.Time  // when input starts to work

	// A phone with several displays (a foldable) shows one at a time; see
	// display_info.go. two says the model has two screens, whatever its
	// replies list. dinfo is the last read of them all, and want the key of
	// the display in use there (chooseDisplay), wantID its id (0: none
	// named) and wantLit whether it is lit (as displayEntry.Lit); streamFor
	// is what want was when the running stream started, and streamOn the
	// id the stream asked for, another than wantID when that one gave no
	// picture or the reply names none, and streamShows the key of the
	// listed display its picture shows ("": not known). good remembers, by
	// key, the id whose stream last brought a display's picture, for the
	// next start. scr describes the display the stream shows.
	two         bool
	dinfo       displayInfo
	want        string
	wantID      int
	wantLit     int
	streamFor   string
	streamOn    int
	streamShows string
	good        map[string]int
	noVideo     *noVideoError // why the last stream start got no picture; nil once one did

	lastInput atomic.Int64 // unix nanos of the last input or launch Pinky sent

	lastFront   screen.App // the home screen app the last read found in front (appid.go)
	lastFrontAt time.Time
	closed      bool
	done        chan struct{} // closed by Close
	watchers    int           // stream views open (see idle.go)
	watchedAt   time.Time     // when one last opened or closed

	inputMu   sync.Mutex // serializes gestures and raw touches
	restartMu sync.Mutex // one stream restart at a time; guards the two below

	restartFailedAt time.Time
	restartErr      error

	svcMu       sync.Mutex
	info2       *cdService // deviceinfo
	shot        *cdService // screencapture
	inst        *instShot
	ax          *axSession
	rax         *raxSession
	raxErr      error // why rax could not connect, until raxFailedAt+raxRecheckFailures
	raxFailedAt time.Time
	apps        *appCatalog
	pidApps     map[uint64]driver.App
	pidMiss     map[uint64]time.Time          // processes that are no app, and when that was found
	web         webCache                      // which processes of a walk are WebKit's web content
	foreground  func(context.Context) []axApp // for tests: the app states the inspector reported
	sb          springBoardWatch
	capture     captureMarks // what screenshots did not show (capture.go)
	pb          *pbConn

	logs appLogs // the apps whose logs a task reads (logs.go)

	typeStats   TypeStats // guarded by mu
	inputNote   string    // for the agent, about the last Type (TakeInputNote); guarded by mu
	keysOnly    bool      // Type skips the pasteboard (TypeByKeys); guarded by mu
	framesAfter time.Time // no frames of the video for pictures until then (capture.go); guarded by mu

	// preferCapture is set when the screen capture service answered and the
	// instruments one did not.
	preferCapture bool
	// shotUIDOff is set when the capture service failed a screenshot of
	// one display by its uniqueId: later ones take the main display.
	shotUIDOff bool

	imageDir string // Pinky's folder for developer disk images (checkup.go)
}

// Open connects to a device and starts its video stream and HID session.
// It goes over USB when it can, and over Wi-Fi when info.Connection is
// "wifi".
func Open(ctx context.Context, info driver.Info) (*Device, error) {
	return open(ctx, info, nil)
}

// open is Open with a logger for what the stream does (nil: none).
func open(ctx context.Context, info driver.Info, log *slog.Logger) (*Device, error) {
	l, err := dialLink(ctx, info.ID, info.Connection == "wifi")
	if err != nil {
		return nil, err
	}
	d := &Device{info: info, l: l, log: logTo(log), done: make(chan struct{}), two: twoScreens(info.Model)}
	if err := d.start(ctx); err != nil {
		l.Close()
		return nil, err
	}
	go d.idleLoop()
	go d.orientLoop()
	// Connect the accessibility service now (the phone answers its first
	// requests slowly while it builds the first tree it pushes), and the
	// pasteboard service, whose handshake would otherwise slow the first
	// Type by half a second.
	go func() {
		c, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		d.raxReady(c)
		if pb := d.pasteboard(); pb != nil {
			pb.connect()
		}
		// The first read names its app through the catalog, and the
		// inspector session follows apps coming to the foreground from now
		// on (appid.go).
		d.catalog(c)
		d.axReady(c)
	}()
	return d, nil
}

func (d *Device) start(ctx context.Context) error {
	svc, err := dialCoreDevice(d.l.entry, deviceInfoService)
	if err != nil {
		return err
	}
	di, err := readDisplayInfo(ctx, svc, "")
	if err != nil {
		svc.Close()
		return err
	}
	d.info2 = svc
	d.noteDisplays(di)
	if err := d.restartMedia(ctx); err != nil {
		return err
	}
	return nil
}

// restartTimeout bounds a stream restart. A restart runs detached from the
// call that noticed the dead stream: one cancelled tap must not abort a
// restart that the next call would only start again.
const restartTimeout = 20 * time.Second

// restartBackoff spaces out failed restarts: restarting streams often can
// wedge the phone's media daemon.
const restartBackoff = 5 * time.Second

// readyWait is how long a stream start waits for its first key frame.
const readyWait = 8 * time.Second

// nextDisplayWait is what each further display a restart tries adds to
// restartTimeout: a start and readyWait.
const nextDisplayWait = 12 * time.Second

// restartMedia (re)starts the video stream and the HID session on it. It
// asks for the display Pinky shows; on a phone with several displays, one
// that brings no picture (no key frame within readyWait) or that the phone
// refuses to stream is followed by the others (streamDisplays), the one
// that last brought this display's picture first.
func (d *Device) restartMedia(ctx context.Context) error {
	err := d.startStream(ctx)
	var nv *noVideoError
	if err != nil && !errors.As(err, &nv) {
		return err
	}
	d.mu.Lock()
	d.noVideo = nv // for the checkup
	d.mu.Unlock()
	return err
}

// startStream stops the running stream, if any, and starts one, trying the
// displays restartMedia says.
func (d *Device) startStream(ctx context.Context) error {
	d.mu.Lock()
	want, ids := d.streamPlanLocked()
	ds, multi := d.dinfo.Displays, d.multiLocked()
	d.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), restartTimeout+time.Duration(len(ids)-1)*nextDisplayWait)
	defer cancel()
	stop := func(m *media) {
		c, cc := context.WithTimeout(context.Background(), 5*time.Second)
		m.stop(c)
		cc()
	}
	d.mu.Lock()
	old, oldHID := d.m, d.hid
	d.m, d.hid = nil, nil
	d.mu.Unlock()
	if oldHID != nil {
		oldHID.Close()
	}
	if old != nil {
		stop(old)
	}
	sd := streamDisplay{
		start:  func(ctx context.Context, id int) (*media, error) { return startMedia(ctx, d.l, id) },
		stop:   stop,
		gone:   d.linkGone,
		wait:   readyWait,
		log:    d.log,
		device: d.info.ID,
	}
	m, _, err := sd.try(ctx, ids, ds)
	if err != nil {
		return err
	}
	h, err := newHIDSession(d.l.entry)
	if err != nil {
		stop(m)
		return err
	}
	pw, ph := m.size()
	d.mu.Lock()
	if d.closed {
		// Close ran while this restart was under way; it waits on restartMu,
		// so it is this restart's job to stop what it started.
		d.mu.Unlock()
		h.Close()
		stop(m)
		return fmt.Errorf("device closed: %w", driver.ErrOffline)
	}
	d.m, d.hid = m, h
	d.inputAt = m.started.Add(inputSettle)
	d.streamedLocked(want, m.display, pw, ph)
	m.setTurn(d.scr.turn())
	d.mu.Unlock()
	if multi {
		d.log.Info("iphone: streaming display", "device", d.info.ID, "display", m.display, "tried", ids,
			"wanted", want, "picture", fmt.Sprintf("%dx%d", pw, ph), "displays", describeDisplays(ds))
	}
	return nil
}

// streamPlanLocked returns what a stream start is for (the key of the
// display in use) and the display ids it tries, best first: the one whose
// stream last brought that display's picture, then streamDisplays', three
// at most. Callers hold mu.
func (d *Device) streamPlanLocked() (string, []int) {
	want := d.want
	ids := streamDisplays(d.dinfo.Displays, d.wantID, d.two)
	if id := d.good[want]; d.multiLocked() && want != "" && id > 0 {
		ids = preferDisplay(ids, id)
	}
	if len(ids) > 3 {
		ids = ids[:3]
	}
	return want, ids
}

// streamedLocked notes that a stream started for the display with key want
// streams display id, with a picture of pw x ph (0: not known), and
// remembers that pair for the next start. Callers hold mu.
func (d *Device) streamedLocked(want string, id, pw, ph int) {
	d.streamFor, d.streamOn, d.streamShows = want, id, ""
	if !d.multiLocked() {
		return
	}
	if d.good == nil {
		d.good = map[string]int{}
	}
	if want != "" {
		d.good[want] = id
	}
	// The listed display the picture shows: the one of its shape, else,
	// when each display has an id of its own, the one with the id asked
	// for. Touches and the picture's size go by it, also when it is
	// another than the one Pinky meant.
	ds := d.dinfo.Displays
	if e, ok := pictureOf(ds, pw, ph); ok {
		d.streamShows = e.key()
	} else if idsKnown(ds) {
		for _, e := range ds {
			if e.own() && e.ID == id {
				d.streamShows = e.key()
			}
		}
	}
	if si, ok := d.dinfo.screenOfKey(d.streamShows); ok {
		d.scr = si
		if d.streamShows != want {
			d.good[d.streamShows] = id // the id shows that display, too
		}
	}
}

// streamDisplay starts streams display by display (try); tests give it
// fake streams.
type streamDisplay struct {
	start  func(ctx context.Context, id int) (*media, error)
	stop   func(m *media)
	gone   func() bool   // the link went down: no other display can do better
	wait   time.Duration // how long a display has for its first picture
	log    *slog.Logger
	device string
}

// try starts a stream for each display of ids in turn until one brings a
// picture, and returns that stream and what the displays before it got
// (nil when the first one worked). It moves on to the next display when
// the phone refused one (otherDisplayMayStart) or streamed it without a
// picture; any other failure, the last display's or a lost link ends it.
// The error is then a *noVideoError when any display got that far, else
// the start's own error.
func (s streamDisplay) try(ctx context.Context, ids []int, ds []displayEntry) (*media, *noVideoError, error) {
	var nv *noVideoError
	for i, id := range ids {
		last := i == len(ids)-1
		m, err := s.start(ctx, id)
		if err != nil {
			if last || !otherDisplayMayStart(err) || s.gone() {
				return nil, nil, nv.add(err, id, ds)
			}
			s.log.Warn("iphone: the phone did not stream a display", "device", s.device, "display", id, "displays", describeDisplays(ds), "err", err)
			nv = nv.with(noVideoTry{Display: id, Err: err}, err, ds)
			continue
		}
		rctx, rcancel := context.WithTimeout(ctx, s.wait)
		err = m.waitReady(rctx)
		rcancel()
		if err == nil {
			return m, nv, nil
		}
		s.stop(m)
		var e *noVideoError
		if !errors.As(err, &e) {
			return nil, nil, nv.add(err, id, ds)
		}
		for _, t := range e.tries {
			s.log.Warn("iphone: no video from the phone", "device", s.device, "display", t.Display,
				"displays", describeDisplays(ds), "answer", t.Answer, "config", t.Config, "wire", t.Wire, "err", e.cause)
			nv = nv.with(t, e.cause, ds)
		}
		if last || s.gone() {
			return nil, nil, nv
		}
	}
	return nil, nil, errors.New("no display to stream")
}

// preferDisplay moves id to the front of ids.
func preferDisplay(ids []int, id int) []int {
	out := []int{id}
	for _, x := range ids {
		if x != id {
			out = append(out, x)
		}
	}
	return out
}

// otherDisplayMayStart reports whether a stream start that failed with err
// may work for another display: the phone refused the display, not the
// stream (an app holding the camera, which no display changes) or the
// connection.
func otherDisplayMayStart(err error) bool {
	var ce *CoreDeviceError
	return errors.As(err, &ce) && ce.Code != errMediaInUse
}

// linkGone reports whether the tunnel the device was opened over stopped.
func (d *Device) linkGone() bool {
	if d.l == nil || d.l.tunnel == nil {
		return false
	}
	select {
	case <-d.l.tunnel.Done():
		return true
	default:
		return false
	}
}

// multiLocked reports whether the phone has several displays of its own:
// its reply lists them, or its model has two screens. Callers hold mu.
func (d *Device) multiLocked() bool {
	return d.two || several(d.dinfo.Displays)
}

// displayMovedLocked reports whether a phone with several displays now
// shows another one than the running stream was started for: it was folded
// or unfolded, and the stream must start again on the display now in use.
// A display that says it is off is not moved to: the phone is asleep, and
// its stream starts again when one lights up. Callers hold mu.
func (d *Device) displayMovedLocked() bool {
	return d.m != nil && d.multiLocked() && d.want != "" && d.streamFor != "" && d.want != d.streamFor && d.wantLit >= 0
}

// displayMoved is displayMovedLocked for callers that do not hold mu.
func (d *Device) displayMoved() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.displayMovedLocked()
}

// live returns the running stream and HID session. It restarts the stream
// if the phone stopped it and reconnects HID if that connection dropped.
func (d *Device) live(ctx context.Context) (*media, *hidSession, error) {
	d.mu.Lock()
	closed, m, h, moved := d.closed, d.m, d.hid, d.displayMovedLocked()
	d.mu.Unlock()
	if closed {
		return nil, nil, fmt.Errorf("device closed: %w", driver.ErrOffline)
	}
	select {
	case <-d.l.tunnel.Done():
		return nil, nil, fmt.Errorf("%v: %w", d.l.tunnel.Err(), driver.ErrOffline)
	default:
	}
	if m != nil && !m.stopped.Load() && h != nil && h.alive() && !moved {
		return m, h, nil
	}
	d.restartMu.Lock()
	defer d.restartMu.Unlock()
	d.mu.Lock()
	closed, m, h, moved = d.closed, d.m, d.hid, d.displayMovedLocked()
	from, to, toID := d.streamOn, d.want, d.wantID
	d.mu.Unlock()
	if closed {
		return nil, nil, fmt.Errorf("device closed: %w", driver.ErrOffline)
	}
	if moved {
		d.log.Info("iphone: the phone switched displays", "device", d.info.ID, "from", from, "to", to, "toID", toID)
	} else if m != nil && !m.stopped.Load() {
		if h != nil && h.alive() {
			return m, h, nil // another caller fixed it
		}
		// The stream runs but the HID connection dropped: reconnect it.
		nh, err := newHIDSession(d.l.entry)
		if err != nil {
			return nil, nil, err
		}
		d.mu.Lock()
		d.hid = nh
		d.mu.Unlock()
		if h != nil {
			h.Close()
		}
		return m, nh, nil
	}
	if time.Since(d.restartFailedAt) < restartBackoff {
		return nil, nil, fmt.Errorf("video stream: %w", d.restartErr)
	}
	if err := d.restartMedia(ctx); err != nil {
		d.restartFailedAt, d.restartErr = time.Now(), err
		return nil, nil, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.m, d.hid, nil
}

// Info implements driver.Device.
func (d *Device) Info() driver.Info { return d.info }

// gone wraps err as driver.ErrOffline once the connection the device was
// opened over is gone: its tunnel stopped, as it does when the cable is
// pulled or the phone leaves the network. Calls then fail in many ways (a
// lockdown connection refused, a service that does not answer); the
// gateway reads driver.ErrOffline as the phone having dropped, and opens it
// again over the connection it is on now.
func (d *Device) gone(err error) error {
	if err == nil || errors.Is(err, driver.ErrOffline) || d.l == nil || d.l.tunnel == nil {
		return err
	}
	select {
	case <-d.l.tunnel.Done():
		return fmt.Errorf("%v: %w", err, driver.ErrOffline)
	default:
	}
	return err
}

// Stream implements driver.Device. Each call returns a new view of the
// device's stream that starts at the next keyframe. Closing the view does
// not stop the device's stream, which HID input needs; it stops once no
// view is open and Pinky sends no input for a while (see idle.go).
func (d *Device) Stream(ctx context.Context, _ driver.StreamOptions) (driver.Stream, error) {
	d.watch() // before the stream is looked at: an idle stop must not race this view
	m, _, err := d.live(ctx)
	if err != nil {
		d.unwatch()
		return nil, err
	}
	return &watchedStream{mediaStream: &mediaStream{m: m, s: m.subscribe()}, d: d}, nil
}

// StreamStats reports the stream's counters, for diagnostics.
func (d *Device) StreamStats() (MediaStats, bool) {
	d.mu.Lock()
	m := d.m
	d.mu.Unlock()
	if m == nil {
		return MediaStats{}, false
	}
	s := m.snapshot()
	w, h := m.size()
	return MediaStats{
		Width: w, Height: h, Packets: s.Packets, Bytes: s.Bytes, Frames: s.Frames,
		KeyFrames: s.KeyFrames, Lost: s.Lost, Dropped: s.Dropped, KeyRequests: s.FIRs,
		Since: m.started, Delay: s.SRDelay, DelayKnown: s.SRDelaySet,
	}, true
}

// MediaStats are a stream's counters since it started.
type MediaStats struct {
	Width, Height int
	Packets       int
	Bytes         int64
	Frames        int
	KeyFrames     int
	Lost          int
	Dropped       int
	KeyRequests   int
	Since         time.Time
	// Delay is the one-way delay estimated from RTCP sender reports. It
	// assumes the phone's and the computer's clocks agree.
	Delay      time.Duration
	DelayKnown bool
}

// screen returns the screen the right way up, as the phone last described
// it: Pinky's coordinates (see screenInfo).
func (d *Device) screen() screenInfo {
	return d.display().upright()
}

// display returns the main display as the phone last described it, in the
// display's own frame.
func (d *Device) display() screenInfo {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.scr
}

// hasDisplayInfo reports whether the phone describes its display
// (CoreDevice's display info, iOS 27); below iOS 27 Pinky knows the screen
// from screenshots alone.
func (d *Device) hasDisplayInfo() bool {
	d.svcMu.Lock()
	defer d.svcMu.Unlock()
	return d.info2 != nil
}

// orientFresh is how long a read of the display's orientation is trusted:
// the person may turn the device at any time, and a read costs one
// request (about 12 ms over USB).
const orientFresh = time.Second

// readDisplayNow reads the main display again and keeps what it says: its
// orientation above all, which the video's packets carry from then on, and
// on a foldable which display is in use.
func (d *Device) readDisplayNow(ctx context.Context) (screenInfo, error) {
	d.svcMu.Lock()
	svc := d.info2
	d.svcMu.Unlock()
	if svc == nil {
		return d.display(), fmt.Errorf("display info: %w", driver.ErrOffline)
	}
	d.mu.Lock()
	keep := d.want
	d.mu.Unlock()
	di, err := readDisplayInfo(ctx, svc, keep)
	if err != nil && isClosedErr(err) && ctx.Err() == nil {
		// The phone closed the connection (it locked, or a long session
		// went quiet); the failed request dropped it, so this dials again.
		di, err = readDisplayInfo(ctx, svc, keep)
	}
	if err != nil {
		return d.display(), err
	}
	return d.noteDisplays(di), nil
}

// noteDisplays keeps a fresh description of the phone's displays and
// returns the one Pinky shows. While the stream shows another listed
// display than the one chosen, because the chosen one gave no picture,
// that is the one Pinky shows; when the phone moves to another display (it
// was folded or unfolded), the next live restarts the stream on it.
func (d *Device) noteDisplays(di displayInfo) screenInfo {
	d.mu.Lock()
	si := di.Screen
	d.want, d.wantID, d.wantLit = si.Key, si.DisplayID, si.Lit
	if si.Lit == 0 && (si.Backlight == backlightOff || si.Backlight == backlightAlwaysOn) {
		d.wantLit = -1 // the phone's backlight (iOS 27.2 says it next to the displays)
	}
	if d.m != nil && d.streamShows != "" {
		switch d.want {
		case d.streamFor:
			if s, ok := di.screenOfKey(d.streamShows); ok {
				si = s
			}
		case d.streamShows:
			d.streamFor = d.want // moved to the display the stream already shows
		}
	}
	d.dinfo = di
	d.scr, d.scrAt = si, time.Now()
	m := d.m
	d.mu.Unlock()
	if m != nil {
		m.setTurn(si.turn())
	}
	return si
}

// freshDisplay returns the main display, read again when the last read is
// older than orientFresh. A failed read keeps the last one.
func (d *Device) freshDisplay(ctx context.Context) screenInfo {
	d.mu.Lock()
	si, at := d.scr, d.scrAt
	d.mu.Unlock()
	if time.Since(at) < orientFresh {
		return si
	}
	si, _ = d.readDisplayNow(ctx)
	return si
}

// panel maps a screen pixel, the right way up, to the touch panel's
// 0..65535 range, which is in the frame of the device held the usual way
// up (see screenInfo).
func (d *Device) panel(p screen.Point) (uint16, uint16) {
	return panelPoint(p, d.display())
}

// panelPoint maps a point of the upright screen si describes to the touch
// panel.
func panelPoint(p screen.Point, si screenInfo) (uint16, uint16) {
	u := si.upright()
	if u.Width <= 0 || u.Height <= 0 {
		return 0, 0
	}
	p.X = max(0, min(p.X, u.Width-1))
	p.Y = max(0, min(p.Y, u.Height-1))
	back := si.touchTurn().inverse()
	t := turnPoint(p, u.Width, u.Height, back)
	tw, th := turnSize(u.Width, u.Height, back)
	return scaleAxis(t.X, tw), scaleAxis(t.Y, th)
}

func scaleAxis(v, size int) uint16 {
	if size <= 0 {
		return 0
	}
	if v < 0 {
		v = 0
	}
	if v >= size {
		v = size - 1
	}
	return uint16((2*v + 1) * 65536 / (2 * size))
}

// waitInput blocks until the stream has been up long enough for input.
func (d *Device) waitInput(ctx context.Context) (*hidSession, error) {
	_, h, err := d.live(ctx)
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	at := d.inputAt
	d.mu.Unlock()
	if wait := time.Until(at); wait > 0 {
		t := time.NewTimer(wait)
		defer t.Stop()
		select {
		case <-t.C:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	d.markInput()
	return h, nil
}

// markInput records that Pinky is about to change the screen.
func (d *Device) markInput() { d.lastInput.Store(time.Now().UnixNano()) }

// lastInputAt is when Pinky last sent input or launched an app.
func (d *Device) lastInputAt() time.Time {
	if n := d.lastInput.Load(); n != 0 {
		return time.Unix(0, n)
	}
	return time.Time{}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Tap implements driver.Device.
func (d *Device) Tap(ctx context.Context, p screen.Point, opt driver.TapOptions) error {
	hold := 60 * time.Millisecond
	if opt.LongPress {
		hold = 800 * time.Millisecond
	}
	return d.gesture(ctx, []screen.Point{p}, hold)
}

// Swipe implements driver.Device.
func (d *Device) Swipe(ctx context.Context, from, to screen.Point, dur time.Duration) error {
	return d.gesture(ctx, swipePath(from, to, dur), 10*time.Millisecond)
}

// swipePath is a straight line from from to to, one point per 10 ms.
func swipePath(from, to screen.Point, dur time.Duration) []screen.Point {
	if dur <= 0 {
		dur = 300 * time.Millisecond
	}
	n := int(dur / (10 * time.Millisecond))
	if n < 2 {
		n = 2
	}
	path := make([]screen.Point, 0, n+1)
	for i := 0; i <= n; i++ {
		path = append(path, screen.Point{
			X: from.X + (to.X-from.X)*i/n,
			Y: from.Y + (to.Y-from.Y)*i/n,
		})
	}
	return path
}

// gesture puts a finger down at path[0], moves it along the path with gap
// between reports, and lifts it at the end. A one-point path is a tap held
// for gap. The finger always comes up, even when ctx ends mid-gesture.
func (d *Device) gesture(ctx context.Context, path []screen.Point, gap time.Duration) error {
	return d.gestureHook(ctx, path, gap, nil)
}

// gestureHook is gesture with a callback right after the finger goes down.
func (d *Device) gestureHook(ctx context.Context, path []screen.Point, gap time.Duration, onDown func()) error {
	steps := make([]driver.DragStep, 0, len(path)+1)
	steps = append(steps, driver.DragStep{P: path[0]})
	if len(path) == 1 {
		steps = append(steps, driver.DragStep{Wait: gap, P: path[0]})
	}
	for _, p := range path[1:] {
		steps = append(steps, driver.DragStep{Wait: gap, P: p})
	}
	return d.gestureSteps(ctx, steps, onDown)
}

// Drag implements driver.Dragger: the finger moves in reports 10 ms apart
// and rests without reporting.
func (d *Device) Drag(ctx context.Context, g driver.Drag) error {
	return d.gestureSteps(ctx, g.Steps(10*time.Millisecond), nil)
}

// gestureSteps puts a finger down at steps[0], then waits each step's Wait
// and moves the finger to its point (a step that stays put only waits),
// and lifts it at the last. The finger always comes up, even when ctx
// ends mid-gesture.
func (d *Device) gestureSteps(ctx context.Context, steps []driver.DragStep, onDown func()) error {
	d.inputMu.Lock()
	defer d.inputMu.Unlock()
	// The whole gesture maps through one description of the screen, read
	// fresh: the person may have turned the device since the last one, or
	// folded it, which waitInput then follows to the display in use.
	d.freshDisplay(ctx)
	h, err := d.waitInput(ctx)
	if err != nil {
		return err
	}
	// A finger the person left down on the mirror (its touch-up never came)
	// would turn this gesture into a drag from there.
	if err := h.liftHeld(); err != nil {
		return err
	}
	si := d.freshDisplay(ctx)
	x, y := panelPoint(steps[0].P, si)
	if err := h.touchAt(true, x, y); err != nil {
		return err
	}
	if onDown != nil {
		onDown()
	}
	var gerr error
	for _, st := range steps[1:] {
		if gerr = sleepCtx(ctx, st.Wait); gerr != nil {
			break
		}
		nx, ny := panelPoint(st.P, si)
		if nx == x && ny == y {
			continue
		}
		x, y = nx, ny
		if gerr = h.touchAt(true, x, y); gerr != nil {
			break
		}
	}
	if err := h.touchAt(false, x, y); err != nil {
		return errors.Join(gerr, err)
	}
	return gerr
}

// videoSize is the size of the stream's picture, or 0, 0 before its first
// parameter sets.
func (d *Device) videoSize() (int, int) {
	d.mu.Lock()
	m := d.m
	d.mu.Unlock()
	if m == nil {
		return 0, 0
	}
	return m.size()
}

// fromVideo converts a point in the stream's picture to the display's
// pixels, in the display's own frame (the video is in it, whatever way the
// device is turned; see screenInfo). The phone puts its screen at 1:1 in
// the picture's top-left corner and pads the rest with black: 1125x2436
// inside 1136x2464 on the 13 mini, 2388x1668 inside 2400x1680 on an iPad
// Pro, measured by matching decoded frames against screenshots. So a point
// keeps its coordinates, and one in the padding ends up on the screen's edge
// (panel clamps it). A picture smaller than the screen would mean the phone scaled
// its screen down to fit, which no tested phone did; the point is then
// scaled back up, assuming a uniform fit in the top-left corner.
func fromVideo(p screen.Point, vw, vh, sw, sh int) screen.Point {
	if vw <= 0 || vh <= 0 || sw <= 0 || sh <= 0 || (vw >= sw && vh >= sh) {
		return p
	}
	c := min(float64(vw)/float64(sw), float64(vh)/float64(sh))
	return screen.Point{X: int(math.Round(float64(p.X) / c)), Y: int(math.Round(float64(p.Y) / c))}
}

// Touch implements driver.Toucher: raw touches for the person's drags on
// the mirror, at points in the stream's picture as the phone sends it, in
// the display's own frame (see fromVideo); the mirror turns the picture
// upright for the person (driver.Packet.Turn) and its points back. A move
// without a down is ignored.
func (d *Device) Touch(ctx context.Context, a driver.TouchAction, p screen.Point) error {
	d.inputMu.Lock()
	defer d.inputMu.Unlock()
	h, err := d.waitInput(ctx)
	if err != nil {
		return err
	}
	s := d.display()
	vw, vh := d.videoSize()
	native := fromVideo(p, vw, vh, s.Width, s.Height)
	native.X = max(0, min(native.X, s.Width-1))
	native.Y = max(0, min(native.Y, s.Height-1))
	// The display's frame turned by the interface's turn is the upright
	// screen panelPoint takes.
	x, y := panelPoint(turnPoint(native, s.Width, s.Height, s.turn()), s)
	switch a {
	case driver.TouchDown:
		return h.touchAt(true, x, y)
	case driver.TouchMove:
		h.mu.Lock()
		down := h.down
		h.mu.Unlock()
		if !down {
			return nil
		}
		return h.touchAt(true, x, y)
	case driver.TouchUp:
		return h.touchAt(false, x, y)
	}
	return fmt.Errorf("touch action %q: %w", a, driver.ErrUnsupported)
}

// Close implements driver.Device. It lifts any held finger, stops the
// stream on the device and closes the tunnel.
func (d *Device) Close() error {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return nil
	}
	d.closed = true
	if d.done != nil {
		close(d.done)
	}
	d.mu.Unlock()
	// A restart in flight sees closed and stops its own stream; wait for it.
	d.restartMu.Lock()
	d.mu.Lock()
	m, h := d.m, d.hid
	d.m, d.hid = nil, nil
	d.mu.Unlock()
	d.restartMu.Unlock()
	var errs []error
	if h != nil {
		errs = append(errs, h.Close())
	}
	if m != nil {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		errs = append(errs, m.stop(c))
		cancel()
	}
	d.svcMu.Lock()
	for _, s := range []*cdService{d.info2, d.shot} {
		if s != nil {
			s.Close()
		}
	}
	if d.inst != nil {
		d.inst.Close()
	}
	if d.ax != nil {
		d.ax.Close()
	}
	if d.rax != nil {
		d.rax.Close()
	}
	if d.pb != nil {
		d.pb.Close()
	}
	d.svcMu.Unlock()
	d.logs.stopAll()
	errs = append(errs, d.l.Close())
	return errors.Join(errs...)
}

var (
	_ driver.Toucher = (*Device)(nil)
)

var (
	_ driver.Device       = (*Device)(nil)
	_ driver.LockReporter = (*Device)(nil)
)
