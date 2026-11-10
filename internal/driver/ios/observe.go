package ios

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/png"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/tetratorus/lightpinky/internal/driver"
	"github.com/tetratorus/lightpinky/internal/screen"
)

// UIAccessibilityTraits bits (public API).
const (
	traitButton      = 1 << 0
	traitLink        = 1 << 1
	traitImage       = 1 << 2
	traitSelected    = 1 << 3
	traitKeyboardKey = 1 << 5
	traitStaticText  = 1 << 6
	traitNotEnabled  = 1 << 8
	traitSearchField = 1 << 10
	traitTabBar      = 1 << 15
	traitHeader      = 1 << 16
)

// axRole maps traits to a normalized role and states. names are normalized
// trait names (see traitWords); bits are UIAccessibilityTraits when known.
func axRole(bits uint64, names []string, value string) (role string, states []string) {
	switch {
	case has(names, "secure text field"):
		role = screen.RoleInput
		states = append(states, screen.StateEditable, screen.StatePassword)
	case bits&traitSearchField != 0 || has(names, "search field") || has(names, "text field"):
		role = screen.RoleInput
		states = append(states, screen.StateEditable)
	case has(names, "toggle"):
		role = screen.RoleSwitch
		if v := strings.ToLower(value); v == "1" || v == "on" || v == "开" {
			states = append(states, screen.StateChecked)
		}
	case has(names, "tab") || bits&traitTabBar != 0:
		role = screen.RoleTab
	case bits&(traitButton|traitKeyboardKey) != 0 || has(names, "button") || has(names, "back button") || has(names, "menu item") || has(names, "keyboard key"):
		role = screen.RoleButton
	case bits&traitLink != 0 || has(names, "link"):
		role = screen.RoleLink
	case bits&traitImage != 0 || has(names, "image"):
		role = screen.RoleImage
	case bits&(traitStaticText|traitHeader) != 0 || has(names, "header"):
		role = screen.RoleText
	default:
		role = screen.RoleOther
	}
	if bits&traitSelected != 0 || has(names, "selected") {
		states = append(states, screen.StateSelected)
	}
	if bits&traitNotEnabled != 0 || has(names, "dimmed") {
		states = append(states, screen.StateDisabled)
	}
	return role, states
}

// observeLimits bound the accessibility walk of one Observe.
var observeLimits = walkLimits{MaxElements: 150, Budget: 5 * time.Second, Batch: 8}

// axReady returns the accessibility session, connecting on first use.
func (d *Device) axReady(ctx context.Context) (*axSession, error) {
	d.svcMu.Lock()
	defer d.svcMu.Unlock()
	if d.ax != nil {
		select {
		case <-d.ax.conn.Closed():
			d.ax = nil
		default:
			return d.ax, nil
		}
	}
	a, err := dialAX(ctx, d.l.base)
	if err != nil {
		return nil, err
	}
	d.ax = a
	return a, nil
}

// ObserveStats describe the last Observe, for diagnostics.
type ObserveStats struct {
	Source   string // "remoteax" (frames from the phone) or "inspector" (OCR places elements)
	Elements int
	Placed   int // elements with a position: from the phone, or by OCR on the inspector path
	OCRLines int
	Walk     time.Duration
	Shot     time.Duration
	ShotFrom string // what OCR read: "screenshot", or "video" (a frame of the live video, over Wi-Fi)
	// Capture says when the picture for OCR started (captureGate): "at
	// once" (no MayCapture), "after the app", "after the read", or "" when
	// the screen could not be captured; CaptureWait is how long into the
	// read.
	Capture     string
	CaptureWait time.Duration
	OCR         time.Duration
	Total       time.Duration
	Complete    bool // the walk came back to its start
	WalkError   error
	OCRError    error
	RAXError    error    // why the remoteax path was not used
	Trace       []string // what the remoteax path waited for, with times
}

// Observe implements driver.Device. It reads the screen through the
// accessibility daemon's remoteAXService, which gives every element with
// its frame (see observeRAX). When that service fails, it falls back to the
// inspector walk, which gives labels, roles and identifiers but no frames,
// and places the elements whose label is on screen by OCR. Text the tree did
// not report comes back as ocr lines when opt.OCR is set.
func (d *Device) Observe(ctx context.Context, opt driver.ObserveOptions) (*screen.Screen, error) {
	scr, _, err := d.ObserveWithStats(ctx, opt)
	return scr, err
}

// ObserveWithStats is Observe with timings.
func (d *Device) ObserveWithStats(ctx context.Context, opt driver.ObserveOptions) (*screen.Screen, ObserveStats, error) {
	s, st, err := d.observeWithStats(ctx, opt)
	return s, st, d.gone(err)
}

func (d *Device) observeWithStats(ctx context.Context, opt driver.ObserveOptions) (*screen.Screen, ObserveStats, error) {
	var st ObserveStats
	s, err := d.observeRAX(ctx, opt, &st)
	if err == nil {
		return s, st, nil
	}
	if ctx.Err() != nil {
		return nil, st, ctx.Err()
	}
	st = ObserveStats{RAXError: err}
	// A locked phone can stop answering accessibility requests (seen with
	// its screen off after a long session), and the inspector walk then
	// times out too. What matters on it is that it is locked: say so
	// rather than fail the read, so the agent hands the phone to the
	// person instead of retrying.
	if locked, lerr := d.Locked(ctx); lerr == nil && locked {
		st.Source = "lockdown"
		return d.lockedScreen(), st, nil
	}
	return d.observeInspector(ctx, opt, st)
}

// lockedScreen is a locked phone whose screen could not be read: no
// elements, since its lock screen can show any app's notifications.
func (d *Device) lockedScreen() *screen.Screen {
	si := d.screen()
	return &screen.Screen{
		Width: si.Width, Height: si.Height,
		App:         screen.App{Name: "Home Screen", ID: springBoard},
		Orientation: orientationOf(si.Width, si.Height),
		Locked:      true,
		CapturedAt:  time.Now(),
	}
}

// observeInspector is Observe through the inspector walk and OCR.
// errEmptyRead is a read in which neither the accessibility tree nor OCR
// found anything.
var errEmptyRead = errors.New("the iPhone answered with an empty screen")

func (d *Device) observeInspector(ctx context.Context, opt driver.ObserveOptions, st ObserveStats) (*screen.Screen, ObserveStats, error) {
	start := time.Now()
	st.Source = "inspector"
	ax, err := d.axReady(ctx)
	if err != nil {
		return nil, st, err
	}

	var wg sync.WaitGroup
	var nodes []axNode
	var complete bool
	var walkErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.Now()
		nodes, complete, walkErr = ax.walk(ctx, observeLimits)
		st.Walk = time.Since(t)
	}()
	var lines []ocrLine
	var ocrErr error
	var shotW, shotH int // the picture OCR read, the right way up
	var pictured sync.WaitGroup
	// The picture places the elements and adds OCR's text; it waits until
	// the read may take it (capturegate.go), here once the walk named the
	// app: on this path, a fallback, the walk is the read.
	gate := newCaptureGate(opt.MayCapture, func() {
		pictured.Add(1)
		go func() {
			defer pictured.Done()
			t := time.Now()
			png, pic, from, err := d.ocrPicture(ctx, opt)
			st.Shot, st.ShotFrom = time.Since(t), from
			if err != nil {
				ocrErr = err
				return
			}
			shotW, shotH = pictureSize(png, pic)
			t = time.Now()
			lines, ocrErr = recognizeText(png, ocrOptions{Languages: ocrLanguages})
			st.OCR = time.Since(t)
		}()
	})
	var locked bool
	wg.Add(1)
	go func() {
		defer wg.Done()
		locked, _ = d.Locked(ctx)
	}()
	if d.hasDisplayInfo() {
		// The display's orientation, read while the walk runs.
		wg.Add(1)
		go func() {
			defer wg.Done()
			d.readDisplayNow(ctx)
		}()
	}
	wg.Wait()
	si := d.screen()
	appName, appID := ax.foreground()
	if app, ok := d.appForPID(ctx, ownerPID(nodes)); ok {
		appName, appID = app.Name, app.ID
	}
	s := &screen.Screen{
		Width: si.Width, Height: si.Height,
		App:         screen.App{Name: appName, ID: appID},
		Orientation: orientationOf(si.Width, si.Height),
		Locked:      locked,
		CapturedAt:  time.Now(),
	}
	use := gate.late(s)
	pictured.Wait()
	st.Capture, st.CaptureWait = gate.stats()
	if !use {
		lines, ocrErr = nil, nil
	}
	if !d.hasDisplayInfo() && shotW > 0 && shotH > 0 {
		// Without display info (below iOS 27) the screenshot, which is
		// always upright, gives the screen's size as it is now.
		d.mu.Lock()
		d.scr.Width, d.scr.Height = shotW, shotH
		d.mu.Unlock()
		si = d.screen()
		s.Width, s.Height, s.Orientation = si.Width, si.Height, orientationOf(si.Width, si.Height)
	}
	if len(nodes) == 0 {
		if ocrErr != nil || len(lines) == 0 {
			// Nothing to show is not a screen: the phone's services did not
			// answer (seen over Wi-Fi after a session dropped), and an
			// empty read would tell the agent the screen is blank.
			if walkErr == nil {
				walkErr = errEmptyRead
			}
			return nil, st, walkErr
		}
		// Without the tree the screen is still worth reading: fall back to
		// OCR text alone.
		opt.OCR = true
	}
	st.WalkError = walkErr
	st.OCRError = ocrErr
	st.Complete = complete

	parts := make([]captionParts, len(nodes))
	firsts := make([]string, len(nodes))
	for i, n := range nodes {
		parts[i] = parseCaption(n.caption)
		firsts[i] = parts[i].first
	}
	keys := keyboardKeys(firsts)
	var match []string
	for i, n := range nodes {
		if keys[i] || n.traits&traitKeyboardKey != 0 {
			s.Keyboard = true
			continue
		}
		cp := parts[i]
		role, states := axRole(n.traits, cp.traits, cp.value)
		label := cp.label
		if label == "" {
			label = n.ident
		}
		s.Elements = append(s.Elements, screen.Element{
			Role: role, Label: label, States: states, Source: screen.SourceTree,
			Path: appID, ID: n.ident,
		})
		match = append(match, cp.first)
	}
	// The on-screen keyboard is drawn by another process (InputUI), so the
	// walk does not see it; its keys show up in OCR as single letters.
	kbTop, kb := keyboardFromOCR(lines)
	if kb {
		s.Keyboard = true
		kept := lines[:0:0]
		for _, l := range lines {
			if l.Y < kbTop {
				kept = append(kept, l)
			}
		}
		lines = kept
	}
	used := placeByOCR(s.Elements, match, lines, si.Width, si.Height)
	for _, e := range s.Elements {
		if !e.Bounds.Empty() {
			st.Placed++
		}
	}
	if opt.OCR {
		for i, l := range lines {
			if used[i] || strings.TrimSpace(l.Text) == "" || l.Confidence < 0.3 {
				continue
			}
			s.Elements = append(s.Elements, screen.Element{
				Role: screen.RoleText, Label: l.Text, Source: screen.SourceOCR,
				Bounds: ocrRect(l, si.Width, si.Height),
			})
		}
	}
	if ocrErr != nil && len(s.Elements) == 0 {
		return nil, st, fmt.Errorf("observe: %w", ocrErr)
	}
	st.Elements = len(nodes)
	st.OCRLines = len(lines)
	st.Total = time.Since(start)
	return s, st, nil
}

// appForPID resolves the app a process belongs to, through the process list
// and the app catalog. Results are cached for the session: a pid does not
// change apps. A process the catalog does not list (a system service, or an
// app installed since the catalog was read, which reads it again) is not
// looked up again for pidRecheck.
func (d *Device) appForPID(ctx context.Context, pid uint64) (driver.App, bool) {
	if pid == 0 {
		return driver.App{}, false
	}
	d.svcMu.Lock()
	app, ok := d.pidApps[pid]
	missed, wasMissed := d.pidMiss[pid]
	d.svcMu.Unlock()
	if ok {
		return app, true
	}
	if wasMissed && time.Since(missed) < pidRecheck {
		return driver.App{}, false
	}
	procs, err := d.processPaths(ctx)
	if err != nil {
		return driver.App{}, false
	}
	exe, ok := procs[pid]
	if !ok {
		return driver.App{}, false
	}
	c, err := d.catalog(ctx)
	if err != nil {
		return driver.App{}, false
	}
	app, ok = c.appForExecutable(exe)
	if !ok {
		if c, err = d.catalogFresh(ctx); err == nil {
			app, ok = c.appForExecutable(exe)
		}
	}
	d.svcMu.Lock()
	defer d.svcMu.Unlock()
	if !ok {
		if d.pidMiss == nil {
			d.pidMiss = map[uint64]time.Time{}
		}
		d.pidMiss[pid] = time.Now()
		return driver.App{}, false
	}
	if d.pidApps == nil {
		d.pidApps = map[uint64]driver.App{}
	}
	d.pidApps[pid] = app
	return app, true
}

// pidRecheck is how long a process that is no app is not looked up again.
const pidRecheck = 30 * time.Second

// ownerPID is the process that owns most of the walked elements: the app
// on screen.
func ownerPID(nodes []axNode) uint64 {
	count := map[uint64]int{}
	var best uint64
	for _, n := range nodes {
		p := handlePID(n.handle)
		if p == 0 {
			continue
		}
		count[p]++
		if count[p] > count[best] {
			best = p
		}
	}
	return best
}

// norm folds a string for matching: lower case, letters and digits only.
func norm(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// truncated reports whether OCR text ends in an ellipsis, as truncated
// labels do on screen, and returns the text before it.
func truncated(s string) (string, bool) {
	t := strings.TrimSpace(s)
	for _, suf := range []string{"…", "...", "…."} {
		if strings.HasSuffix(t, suf) {
			return strings.TrimSuffix(t, suf), true
		}
	}
	return t, false
}

// placeByOCR gives tree elements the box of the OCR line that shows their
// label, in rounds so that a loose match never takes a line another element
// matches exactly: equal text first, then text truncated with an ellipsis,
// then a line that holds the label as whole words (OCR merges neighbouring
// labels into one line; the element then gets its share of the box), and
// last a line that is part of the label. Lines are taken in reading order, so repeated labels map onto
// repeated text from top to bottom. It returns which lines were used.
func placeByOCR(els []screen.Element, match []string, lines []ocrLine, w, h int) []bool {
	order := make([]int, len(lines))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		la, lb := lines[order[a]], lines[order[b]]
		if dy := la.Y - lb.Y; dy > 0.01 || dy < -0.01 {
			return la.Y < lb.Y
		}
		return la.X < lb.X
	})
	normed := make([]string, len(lines))
	for i, l := range lines {
		normed[i] = norm(l.Text)
	}
	want := make([]string, len(els))
	for i, e := range els {
		if e.Source != screen.SourceTree {
			continue
		}
		if i < len(match) {
			want[i] = match[i]
		}
		if norm(want[i]) == "" {
			want[i] = e.Label
		}
	}
	used := make([]bool, len(lines))
	placed := make([]bool, len(els))
	// Inputs go first: a field shows its placeholder, and the same word
	// often labels a row elsewhere (Settings has a "Search" field and a
	// "Search" row that may be off screen).
	idx := make([]int, 0, len(els))
	for i, e := range els {
		if e.Role == screen.RoleInput {
			idx = append(idx, i)
		}
	}
	for i, e := range els {
		if e.Role != screen.RoleInput {
			idx = append(idx, i)
		}
	}
	rounds := []func(label string, j int) bool{
		func(label string, j int) bool { return normed[j] == norm(label) },
		func(label string, j int) bool {
			pre, ok := truncated(lines[j].Text)
			p := norm(pre)
			return ok && len([]rune(p)) >= 3 && strings.HasPrefix(norm(label), p)
		},
		func(label string, j int) bool {
			return len([]rune(norm(label))) >= 3 && containsWords(lines[j].Text, label)
		},
		// Last, a line that is a whole-word part of the label: a home
		// screen folder reads "Wallets folder" and shows "Wallets".
		func(label string, j int) bool {
			return len([]rune(normed[j])) >= 4 && containsWords(label, lines[j].Text)
		},
	}
	for round, ok := range rounds {
		for _, i := range idx {
			if placed[i] || want[i] == "" || norm(want[i]) == "" {
				continue
			}
			for _, j := range order {
				if used[j] || normed[j] == "" || !ok(want[i], j) {
					continue
				}
				r := ocrRect(lines[j], w, h)
				if round == 2 {
					r = shareOf(r, lines[j].Text, want[i])
				}
				els[i].Bounds = r
				placed[i] = true
				used[j] = true
				break
			}
		}
	}
	return used
}

// containsWords reports whether text holds label as whole words, ignoring
// case.
func containsWords(text, label string) bool {
	t, l := strings.ToLower(text), strings.ToLower(strings.TrimSpace(label))
	for start := 0; ; {
		k := strings.Index(t[start:], l)
		if k < 0 {
			return false
		}
		k += start
		before := k == 0 || !isWordRune(lastRune(t[:k]))
		after := k+len(l) == len(t) || !isWordRune(firstRune(t[k+len(l):]))
		if before && after {
			return true
		}
		start = k + 1
	}
}

func isWordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }

func lastRune(s string) rune {
	r := []rune(s)
	return r[len(r)-1]
}

func firstRune(s string) rune {
	for _, r := range s {
		return r
	}
	return 0
}

// shareOf narrows a line's box to the part that shows label, by character
// position.
func shareOf(r screen.Rect, text, label string) screen.Rect {
	t := []rune(strings.ToLower(text))
	l := []rune(strings.ToLower(strings.TrimSpace(label)))
	k := strings.Index(string(t), string(l))
	if k < 0 || len(t) == 0 {
		return r
	}
	off := len([]rune(string(t)[:k]))
	r.X += r.W * off / len(t)
	r.W = r.W * len(l) / len(t)
	return r
}

// pictureSize is the size of the picture OCR read: the decoded one, else
// the PNG's header.
func pictureSize(b []byte, img image.Image) (int, int) {
	if img != nil {
		return img.Bounds().Dx(), img.Bounds().Dy()
	}
	if cfg, err := png.DecodeConfig(bytes.NewReader(b)); err == nil {
		return cfg.Width, cfg.Height
	}
	return 0, 0
}
