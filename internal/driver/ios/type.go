package ios

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tetratorus/lightpinky/internal/driver"
)

// Typing pastes: the person's pasteboard is saved, set to the text,
// Command-V goes through the virtual keyboard, and once the field shows the
// text the pasteboard gets its old contents back. Pasting enters any text
// whatever the active input method (a Pinyin keyboard turns typed letters
// into a composition), and is as fast for a page as for a word. What is
// saved stays in memory, for the length of one Type call, and is never
// logged or shown.
//
// Without the pasteboard service, or when it does not answer, Type falls
// back to keys: printable ASCII only, through a Latin keyboard with the U.S.
// layout (see typeKeys). Either way it reads the field back afterwards and
// returns an error when it reads otherwise than the text (checkTyped).

// TypeStats says how the last Type call went, for tests and iosctl. It
// holds neither the text nor the pasteboard.
type TypeStats struct {
	Method    string // "paste" or "keys"
	Saved     string // what was saved: "full", "primary" (each item's first representation), "empty", "unreadable"
	Restore   string // "restored", "cleared", "left" (the pasteboard changed meanwhile), or why it failed
	Confirmed bool   // the field's value changed before the pasteboard was put back
	Spaces    string // spaces iOS's smart insert added next to the paste: "" (none), "removed", "kept" (not taken off: Pinky could not tell where they were), "unconfirmed" (taken off, but the field did not read as expected)
	Save      time.Duration
	Paste     time.Duration // SET to the field showing the text
	Restored  time.Duration
	Total     time.Duration
	Trace     []string // the keys path: what the keyboard probes found, with times
	Checked   string   // the field read back: "same", "different", "hidden", "unchanged", "unfocused", "unreadable"; or, when it could not be read, the app: "shown" (it shows the text), "unchanged"
}

// LastTypeStats returns how the last Type call went.
func (d *Device) LastTypeStats() TypeStats {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.typeStats
}

// TakeInputNote implements driver.InputNoter: what the agent should know
// about the last Type, such as a field Pinky could not read back to check.
func (d *Device) TakeInputNote() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := d.inputNote
	d.inputNote = ""
	return n
}

// TypeByKeys has Type skip the pasteboard and type by keys, for iosctl and
// tests of the keys path.
func (d *Device) TypeByKeys(on bool) {
	d.mu.Lock()
	d.keysOnly = on
	d.mu.Unlock()
}

func (d *Device) byKeys() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.keysOnly
}

// Timeouts for typing. The pasteboard daemon answers in milliseconds, but
// reading every representation of an item can stall it: it loads them one
// at a time, and an app can list a type it fails to produce (Notes and
// public.rtf: about 64 s, pymobiledevice3 found). A SET sent meanwhile
// waits for that.
const (
	pbQuick      = 3 * time.Second  // a request that should be quick
	pbFullPull   = 3 * time.Second  // reading every representation
	pbStalled    = 75 * time.Second // a SET queued behind a stalled read
	pasteConfirm = 1500 * time.Millisecond
	pasteBlind   = 500 * time.Millisecond // the wait when the field cannot be read
)

// errPasteUnavailable means the pasteboard service cannot be used; typing
// falls back to keys.
var errPasteUnavailable = errors.New("pasteboard service unavailable")

// Type implements driver.Device: it enters text into the focused field.
// Without Append it first clears the field with Command-A and Delete, as a
// person with a keyboard would. Submit presses Return afterwards.
func (d *Device) Type(ctx context.Context, text string, opt driver.TypeOptions) error {
	return d.gone(d.typeText(ctx, text, opt))
}

// typeText is Type without marking a dropped connection (gone).
func (d *Device) typeText(ctx context.Context, text string, opt driver.TypeOptions) error {
	start := time.Now()
	st := TypeStats{}
	var note string
	defer func() {
		st.Total = time.Since(start)
		d.mu.Lock()
		d.typeStats = st
		d.inputNote = note
		d.mu.Unlock()
	}()
	d.inputMu.Lock()
	defer d.inputMu.Unlock()
	// Choose how to type before touching the field, so text that cannot be
	// typed leaves it as it was.
	var (
		pb     *pbConn
		saved  *pbSnapshot
		keys   []keyStroke
		method = "keys"
		pbErr  error
	)
	if text != "" {
		if pb = d.pasteboard(); pb != nil && !d.byKeys() {
			t0 := time.Now()
			saved, st.Saved, pbErr = savePasteboard(ctx, pb)
			st.Save = time.Since(t0)
			switch {
			case pbErr == nil:
				method = "paste"
			case !errors.Is(pbErr, errPasteUnavailable):
				return pbErr
			}
		}
		if method == "keys" {
			var err error
			if keys, err = keyStrokesFor(text, pbErr); err != nil {
				return err
			}
		}
		st.Method = method
	}
	clear := func() error {
		if opt.Append {
			return nil
		}
		return d.pressKeys(ctx, []keyStroke{{usage: keyA, mod: keyLeftGUI}, {usage: keyBackspace}})
	}
	if text == "" {
		if err := clear(); err != nil {
			return err
		}
	} else {
		if method == "keys" {
			// The keys path checks the keyboard through remoteAXService:
			// wait for it rather than type blind, as right after Open.
			rctx, cancel := context.WithTimeout(ctx, raxDialWait)
			d.raxReady(rctx)
			cancel()
		}
		pfield, value, perr := d.focusedField(ctx)
		var prior *string // nil when it could not be read
		if pfield != nil {
			prior = &value
		}
		// A focused field that cannot be read is checked by what the app
		// shows instead (checkUnread), so remember that now.
		var shown *shownText
		look := func() {
			if perr != nil {
				if s, ok := d.readShown(ctx); ok {
					shown = &s
				}
			}
		}
		var err error
		if method == "paste" {
			if err = clear(); err == nil {
				look()
				err = d.paste(ctx, pb, saved, text, &st)
			}
		} else {
			// Keys go only where the keyboard turns them into this text,
			// which typeKeys checks first, clearing the field after that.
			look()
			err = d.typeKeys(ctx, keys, clear, pbErr, &st)
		}
		if err != nil {
			return err
		}
		if note, err = d.checkTyped(ctx, text, opt.Append, prior, shown, method, pbErr, &st); err != nil {
			if opt.Submit {
				err = fmt.Errorf("%w; Return was not pressed", err)
			}
			return err
		}
	}
	if opt.Submit {
		return d.pressKeys(ctx, []keyStroke{{usage: keyEnter}})
	}
	return nil
}

// raxDialWait bounds the wait for remoteAXService before typing by keys.
const raxDialWait = 8 * time.Second

// checkTyped reads the focused field back after typing text and returns an
// error when it reads otherwise, so that wrong text never counts as typed:
// keys arrive as other letters on a hardware keyboard layout other than
// U.S., iOS corrects and capitalizes words typed as keys, and a field may
// format or cut what is pasted. It is an error too when the text most
// likely went nowhere: no element of the app in front has focus, or the
// field reads as before or empty (GitHub issue #65: a search field that
// never took focus, and Type said nothing), or, for a field that cannot be
// read, the app shows exactly what it did (issue #76). Only when the field
// cannot be read and the app changed otherwise, or the field hides its
// text, does it return a note for the agent instead.
// When the field cannot be read, what the app shows tells instead
// (checkUnread; shown is what it showed before typing, nil when unknown).
// prior is the field's value before, for appended text, nil when it could
// not be read. Callers hold inputMu.
func (d *Device) checkTyped(ctx context.Context, text string, appended bool, prior *string, shown *shownText, method string, pbErr error, st *TypeStats) (string, error) {
	unread := func() (string, error) { return d.checkUnread(ctx, text, shown, method, st) }
	field, v, ferr := d.focusedField(ctx)
	rax, err := d.raxNow()
	if ferr != nil || err != nil {
		return unread()
	}
	// The app in front may read as the on-screen keyboard's process (a
	// keyboard extension, seen with Doubao Input) for a moment while it
	// goes away, with nothing focused: read again before concluding.
	for deadline := time.Now().Add(pasteConfirm); field == nil && ferr == nil && time.Now().Before(deadline); {
		if err := sleepCtx(ctx, 100*time.Millisecond); err != nil {
			return "", err
		}
		field, v, ferr = d.focusedField(ctx)
	}
	if ferr != nil {
		return unread()
	}
	if field == nil {
		st.Checked = "unfocused"
		return "", errUnfocused
	}
	ok := func(v string) bool { return typedAs(v, text, appended, prior, st.Spaces) }
	// Apps update a field's value a moment after the keys.
	for deadline := time.Now().Add(pasteConfirm); !ok(v) && time.Now().Before(deadline); {
		if err := sleepCtx(ctx, 40*time.Millisecond); err != nil {
			return "", err
		}
		if v, err = rax.stringAttribute(context.WithoutCancel(ctx), field, axpAttrValue); err != nil {
			return unread()
		}
	}
	checked, note, err := judgeTyped(v, text, appended, prior, method, pbErr, st.Spaces)
	st.Checked = checked
	return note, err
}

// errUnfocused is checkTyped's error when nothing in the app in front has
// focus after typing.
var errUnfocused = errors.New("type: no field has focus in the app in front, so the text most likely went nowhere: look at the screen, and if the text is not there, tap the field until it shows a cursor or the keyboard, then type again")

// judgeTyped tells what a field that reads v after typing text says: how
// it checked (TypeStats.Checked), a note for the agent when the text could
// not be checked, or an error when the field reads otherwise than the text
// or as if nothing went in.
func judgeTyped(v, text string, appended bool, prior *string, method string, pbErr error, spaces string) (checked, note string, err error) {
	switch {
	case typedAs(v, text, appended, prior, spaces):
		return "same", "", nil
	case hiddenText(v):
		return "hidden", "The field hides its text, so Pinky could not check what was typed.", nil
	case v == "" || (appended && prior != nil && v == *prior):
		// The text did not go in: the field never took focus or the keys
		// went nowhere (GitHub issue #65). Or the value does not follow
		// what is typed (a view of the app's own), which the agent can
		// tell from the screen. A replaced value that reads as something
		// else before is not this: typing the same wrong text again (an
		// agent trying once more) must not pass.
		return "unchanged", "", errors.New("type: the field reads as it did before typing, or empty, so the text most likely did not go in (the field may not have taken focus): look at the screen, and if the text is not there, tap the field until it shows a cursor or the keyboard, then type again")
	}
	got, want := len([]rune(v)), len([]rune(text))
	if appended && prior != nil {
		want += len([]rune(*prior))
	}
	if method == "paste" {
		return "different", "", fmt.Errorf("type: after pasting, the field reads otherwise than the text (%d characters where %d were expected): the field may format, cut or change what it takes; read it before going on", got, want)
	}
	return "different", "", fmt.Errorf("type: after typing by keys, because %s, the field reads otherwise than the text (%d characters where %d were expected): iOS may have corrected or capitalized words as they were typed, or the phone's hardware keyboard layout is not U.S.; read the field and fix it before going on", keysWhy(pbErr), got, want)
}

// typedAs reports whether a field that reads v holds text as typed: the
// whole of it, or appended to prior (where the cursor was, or after a
// placeholder that went away; nil when the old value could not be read,
// which leaves only the text to look for). Spaces that smart insert added
// and Pinky could not take off are allowed around a paste.
func typedAs(v, text string, appended bool, prior *string, spaces string) bool {
	loose := spaces == "kept" || spaces == "unconfirmed"
	if !appended {
		return v == text || (loose && strings.TrimSpace(v) == strings.TrimSpace(text))
	}
	if !strings.Contains(v, text) {
		return false
	}
	if prior == nil {
		return true
	}
	slack := 0
	if loose {
		slack = 2
	}
	return len([]rune(v)) <= len([]rune(*prior))+len([]rune(text))+slack
}

// hiddenText reports whether a field's value is a secure field's bullets.
func hiddenText(v string) bool {
	if v == "" {
		return false
	}
	for _, r := range v {
		if r != '•' && r != '●' {
			return false
		}
	}
	return true
}

// keysWhy says why Type typed by keys: pbErr is why the pasteboard did not
// do, nil when the phone has no pasteboard service (or iosctl asked).
func keysWhy(pbErr error) string {
	if pbErr != nil {
		return "the phone's pasteboard did not answer"
	}
	return "Pinky could not use the phone's pasteboard"
}

// keyStrokesFor is how Type types text without the pasteboard: as keys,
// which only printable ASCII can be. pbErr is why the pasteboard did not
// do: nil when the phone has no pasteboard service, so text that keys
// cannot type is unsupported; else the service is there and did not answer
// this time, which the error says, so that the caller tries again instead
// of concluding that the text cannot be typed.
func keyStrokesFor(text string, pbErr error) ([]keyStroke, error) {
	keys, err := asciiStrokes(text)
	if err != nil && pbErr != nil {
		return nil, fmt.Errorf("type: the phone's pasteboard did not answer, so nothing was typed; try again: %v", pbErr)
	}
	return keys, err
}

// pasteboard returns the pasteboard connection, or nil when the phone has
// no pasteboard service.
func (d *Device) pasteboard() *pbConn {
	if !d.l.has(pasteboardService) {
		return nil
	}
	d.svcMu.Lock()
	defer d.svcMu.Unlock()
	if d.pb == nil {
		d.pb = newPBConn(d.l.entry)
	}
	return d.pb
}

// paste types text through the pasteboard, whose old contents savePasteboard
// read into saved, and puts them back. Callers hold inputMu.
func (d *Device) paste(ctx context.Context, pb *pbConn, saved *pbSnapshot, text string, st *TypeStats) error {
	field, before := d.focusedValue(ctx)

	t1 := time.Now()
	sctx, cancel := context.WithTimeout(ctx, pbStalled)
	mine, err := pb.set(sctx, textItems(text))
	cancel()
	if err != nil {
		var ce *CoreDeviceError
		if errors.As(err, &ce) {
			return fmt.Errorf("type: the phone refused the text: %w", err) // nothing was written
		}
		// The SET may still land; put the pasteboard back after it.
		st.Restore = restorePasteboard(pb, saved, nil)
		return fmt.Errorf("type: %w", err)
	}
	// From here on the pasteboard is put back whatever happens.
	defer func() {
		t2 := time.Now()
		st.Restore = restorePasteboard(pb, saved, mine)
		st.Restored = time.Since(t2)
	}()
	if err := d.pressKeys(ctx, []keyStroke{{usage: keyV, mod: keyLeftGUI}}); err != nil {
		return err
	}
	// Put the pasteboard back only once the app has read it: restoring
	// first would paste the person's own clipboard into the field.
	var after string
	st.Confirmed, after = d.waitValueChange(ctx, field, before)
	st.Paste = time.Since(t1)
	if st.Confirmed {
		st.Spaces = d.undoSmartSpaces(ctx, field, before, after, text)
	}
	return nil
}

// savePasteboard reads the pasteboard so it can be put back. A pasteboard
// the phone does not let anyone read comes back nil with kind "unreadable";
// it is cleared after pasting rather than left holding the text. Other
// failures mean the service did not answer.
func savePasteboard(ctx context.Context, pb pbPuller) (*pbSnapshot, string, error) {
	pull := func() (*pbSnapshot, error) {
		c, cancel := context.WithTimeout(ctx, pbQuick)
		defer cancel()
		return pb.pull(c, pbPromiseSecondary)
	}
	snap, err := pull()
	if err != nil && !errors.Is(err, errPBRestricted) && !errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
		// The phone closes the service's connection after a while idle, and
		// the first request after that fails at once; the failure dropped
		// it, so this one dials again. A read changes nothing, so it can
		// repeat. One that timed out is not repeated: the daemon is busy,
		// and a second wait would only double the time before typing by
		// keys.
		snap, err = pull()
	}
	switch {
	case errors.Is(err, errPBRestricted):
		return nil, "unreadable", nil
	case err != nil:
		if ctx.Err() != nil {
			return nil, "", ctx.Err()
		}
		return nil, "", fmt.Errorf("%w: %v", errPasteUnavailable, err)
	case len(snap.Items) == 0:
		return snap, "empty", nil
	case snap.complete():
		return snap, "full", nil
	}
	// Other representations are only promised: read them all, but not for
	// long. If the daemon is stuck on one an app cannot produce, keep the
	// first representation of each item, which it did give.
	c, cancel := context.WithTimeout(ctx, pbFullPull)
	full, err := pb.pull(c, pbAllResolved)
	cancel()
	if err == nil {
		return full, "full", nil
	}
	if ctx.Err() != nil {
		return nil, "", ctx.Err()
	}
	return snap, "primary", nil
}

// pbPuller reads the pasteboard; pbConn is one, tests fake it.
type pbPuller interface {
	pull(ctx context.Context, policy string) (*pbSnapshot, error)
}

// restorePasteboard puts saved back (an empty pasteboard when saved is nil)
// unless the pasteboard changed since Pinky's own SET, whose result is
// mine (nil when unknown): then someone copied something newer, which
// wins. It uses its own deadline, so a cancelled Type still restores.
func restorePasteboard(pb *pbConn, saved, mine *pbSnapshot) string {
	ctx, cancel := context.WithTimeout(context.Background(), pbStalled)
	defer cancel()
	if mine != nil && mine.HasCount {
		c, cancel := context.WithTimeout(ctx, pbQuick)
		cur, err := pb.pull(c, pbAllPromised)
		cancel()
		switch {
		case errors.Is(err, errPBRestricted):
			return "left" // an item from another device arrived
		case err == nil && cur.HasCount && cur.ChangeCount != mine.ChangeCount:
			return "left"
		}
	}
	items := []any{}
	what := "cleared"
	if saved != nil && len(saved.Items) > 0 {
		items, what = saved.wireItems(), "restored"
	}
	if _, err := pb.set(ctx, items); err != nil {
		return "failed: " + err.Error()
	}
	return what
}

// focusedValue returns the frontmost app's focused element and its value,
// or nil when remoteAXService cannot tell or nothing has focus.
func (d *Device) focusedValue(ctx context.Context) (*axpObject, string) {
	el, v, _ := d.focusedField(ctx)
	return el, v
}

// focusedField is focusedValue that tells the two nils apart: an error
// when remoteAXService cannot be asked or the value cannot be read, a nil
// element without one when the app in front has nothing focused.
func (d *Device) focusedField(ctx context.Context) (*axpObject, string, error) {
	rax, err := d.raxNow()
	if err != nil {
		return nil, "", err
	}
	// No deadline of its own: a request cut short marks the session broken,
	// and each request already gives up after raxRequestTimeout.
	c := context.WithoutCancel(ctx)
	el, err := rax.focusedElement(c)
	if err != nil || el == nil {
		return nil, "", err
	}
	v, err := rax.stringAttribute(c, el, axpAttrValue)
	if err != nil {
		return nil, "", err
	}
	return el, v, nil
}

// waitValueChange waits until field's value differs from before, and returns
// the new value. Without a field it waits a fixed time, which a responsive
// app needs far less of.
func (d *Device) waitValueChange(ctx context.Context, field *axpObject, before string) (bool, string) {
	return d.waitValue(ctx, field, func(v string) bool { return v != before })
}

// waitValue waits until field's value satisfies done, and returns it.
func (d *Device) waitValue(ctx context.Context, field *axpObject, done func(string) bool) (bool, string) {
	rax, err := d.raxNow()
	if field == nil || err != nil {
		sleepCtx(ctx, pasteBlind)
		return false, ""
	}
	deadline := time.Now().Add(pasteConfirm)
	for time.Now().Before(deadline) {
		if sleepCtx(ctx, 40*time.Millisecond) != nil {
			return false, ""
		}
		v, err := rax.stringAttribute(context.WithoutCancel(ctx), field, axpAttrValue)
		if err != nil {
			sleepCtx(ctx, pasteBlind)
			return false, ""
		}
		if done(v) {
			return true, v
		}
	}
	return false, ""
}

// raxNow returns the remoteAXService session if it is connected, without
// connecting: typing must not wait for a dial.
func (d *Device) raxNow() (*raxSession, error) {
	d.svcMu.Lock()
	defer d.svcMu.Unlock()
	if d.rax != nil && d.rax.alive() {
		return d.rax, nil
	}
	return nil, errRAXTimeout
}

// asciiStrokes turns printable ASCII into key strokes on a US layout.
func asciiStrokes(text string) ([]keyStroke, error) {
	strokes := make([]keyStroke, 0, len(text))
	for _, r := range text {
		u, shift, ok := asciiUsage(r)
		if !ok {
			return nil, fmt.Errorf("character %q: without the pasteboard service only printable ASCII can be typed on iPhone: %w", r, driver.ErrUnsupported)
		}
		ks := keyStroke{usage: u}
		if shift {
			ks.mod = keyLeftShift
		}
		strokes = append(strokes, ks)
	}
	return strokes, nil
}

// typeKeys types strokes on the virtual keyboard, the fallback when the
// pasteboard does not do. Keyboard reports carry key codes, not text, and
// the phone turns them into its own text: an input method composes letters
// (Pinyin into Chinese), and a hardware keyboard layout other than U.S.
// puts other letters on the keys (Colemak, Dvorak, AZERTY). So it first
// types probes and deletes them (probeKeyboard): a non-Latin keyboard is
// switched to the other one with Control-Space for the duration, and
// switched back after, and a layout other than U.S. stops it with an
// error, unless the other keyboard has the U.S. layout. Then clear empties
// the field, unless the text is appended, and the strokes go. pbErr is why
// the pasteboard did not do, for the error. Callers hold inputMu.
func (d *Device) typeKeys(ctx context.Context, strokes []keyStroke, clear func() error, pbErr error, st *TypeStats) error {
	rax, err := d.raxNow()
	if err != nil {
		// The keyboard cannot be checked.
		if err := clear(); err != nil {
			return err
		}
		return d.pressKeys(ctx, strokes)
	}
	t0 := time.Now()
	note := func(format string, args ...any) {
		st.Trace = append(st.Trace, fmt.Sprintf("%v ", time.Since(t0).Round(time.Millisecond))+fmt.Sprintf(format, args...))
	}
	first, err := d.probeKeyboard(ctx, rax, note)
	if err != nil {
		return err
	}
	if first.kind == kbLatin {
		if err := clear(); err != nil {
			return err
		}
		return d.pressKeys(ctx, strokes)
	}
	toggle := []keyStroke{{usage: keySpace, mod: keyLeftCtrl}}
	if err := d.pressKeys(ctx, toggle); err != nil {
		return err
	}
	note("switched")
	if err := sleepCtx(ctx, keyboardSwitch); err != nil {
		return err
	}
	kb, err := d.probeKeyboard(ctx, rax, note)
	switch {
	case err != nil:
	case kb.kind == kbLatin:
		if err = d.spendSwitchSpace(ctx, note); err == nil {
			if err = clear(); err == nil {
				err = d.pressKeys(ctx, strokes)
			}
		}
		note("typed")
	case kb.kind == kbLayout:
		err = layoutError(kb, pbErr)
	case first.kind == kbLayout:
		err = layoutError(first, pbErr)
	default:
		err = fmt.Errorf("the active keyboard composes text and Control-Space did not switch to a Latin one: %w", driver.ErrUnsupported)
	}
	// Switch back whatever happened: the keyboard is the person's. Let the
	// Latin keyboard finish with the last word first.
	bctx := context.WithoutCancel(ctx)
	sleepCtx(bctx, keyboardSettle)
	if _, v := d.focusedValue(bctx); v != "" {
		note("before switching back: %d characters", len([]rune(v)))
	}
	if berr := d.pressKeys(bctx, toggle); err == nil {
		err = berr
	}
	note("switched back")
	sleepCtx(bctx, keyboardSwitch)
	if _, v := d.focusedValue(bctx); v != "" {
		note("after switching back: %d characters", len([]rune(v)))
	}
	return err
}

// spendSwitchSpace makes sure a Space goes into the field before the text
// does: after Control-Space changed the keyboard, iOS swallows a Space,
// which dropped the first space of the text typed next ("one two" became
// "onetwo", GitHub issue #29); without a switch nothing is lost. It types
// "x", Space and "y" and reads the field: "x y" means the Space went in,
// "xy" that it was swallowed, and either is deleted again; it tries up to
// three times. A field that reads neither, or cannot be read, is left
// alone. The trailing Space alone would not tell, as some fields drop a
// trailing space from their value. Callers hold inputMu, on a Latin
// keyboard.
func (d *Device) spendSwitchSpace(ctx context.Context, note func(string, ...any)) error {
	rax, err := d.raxNow()
	if err != nil {
		return nil
	}
	x, _, _ := asciiUsage('x')
	y, _, _ := asciiUsage('y')
	for try := 1; try <= 3; try++ {
		field, before := d.focusedValue(ctx)
		if field == nil {
			return nil
		}
		if err := d.pressKeys(ctx, []keyStroke{{usage: x}, {usage: keySpace}, {usage: y}}); err != nil {
			return err
		}
		got, after := "", before
		for deadline := time.Now().Add(switchSpaceWait); time.Now().Before(deadline); {
			if err := sleepCtx(ctx, 50*time.Millisecond); err != nil {
				return err
			}
			v, err := rax.stringAttribute(context.WithoutCancel(ctx), field, axpAttrValue)
			if err != nil {
				return nil
			}
			after = v
			if got = spaceProbe(before, v); got != "" {
				break
			}
		}
		back := func(n int) error {
			strokes := make([]keyStroke, n)
			for i := range strokes {
				strokes[i] = keyStroke{usage: keyBackspace}
			}
			return d.pressKeys(ctx, strokes)
		}
		switch got {
		case "x y":
			note("space after the switch: went in on try %d", try)
			return back(3)
		case "xy":
			note("space after the switch: swallowed on try %d", try)
			if err := back(2); err != nil {
				return err
			}
		default:
			// Delete what arrived, if that can be told, and leave the
			// rest to the read-back.
			note("space after the switch: the field read otherwise")
			if n := len([]rune(after)) - len([]rune(before)); n > 0 && n <= 3 {
				return back(n)
			}
			return nil
		}
	}
	return nil
}

// spaceProbe tells what spendSwitchSpace's "x", Space and "y" put into a
// field whose value was before and is after now: "x y", "xy" (the Space
// swallowed), or "" when the field reads otherwise. The letters may come
// capitalized, and an empty field's value was its placeholder.
func spaceProbe(before, after string) string {
	got := arrived3(before, after)
	if got == "" {
		got = after // a placeholder that went away
	}
	for _, want := range []string{"x y", "xy"} {
		if strings.EqualFold(got, want) {
			return want
		}
	}
	return ""
}

// arrived3 is what typing put into a field whose value was before and is
// after now, as arrived, for more than one character.
func arrived3(before, after string) string {
	b, a := []rune(before), []rune(after)
	if len(a) <= len(b) {
		return ""
	}
	i := 0
	for i < len(b) && a[i] == b[i] {
		i++
	}
	j := 0
	for j < len(b)-i && a[len(a)-1-j] == b[len(b)-1-j] {
		j++
	}
	if i+j != len(b) {
		return ""
	}
	return string(a[i : len(a)-j])
}

// switchSpaceWait is how long spendSwitchSpace waits for its keys to show
// in the field: they do within a few hundred milliseconds.
const switchSpaceWait = 800 * time.Millisecond

// keyboardSwitch is how long iOS takes to change keyboards; keyboardSettle
// how long a keyboard takes with the last keys typed.
const (
	keyboardSwitch = 700 * time.Millisecond
	keyboardSettle = 300 * time.Millisecond
	candidateWait  = time.Second // for an input method's candidate bar
)

// keyboardKind is what the probes found of the active keyboard.
type keyboardKind int

const (
	kbLatin     keyboardKind = iota // keys arrive as on a U.S. layout, or Pinky cannot tell
	kbComposing                     // an input method composes them (Pinyin, Kana, Hangul)
	kbLayout                        // a Latin keyboard whose hardware layout is not U.S.
)

// keyboardProbe is what probeKeyboard found: for kbLayout, the key typed
// (as a U.S. layout has it) and what arrived instead.
type keyboardProbe struct {
	kind     keyboardKind
	key, got string
}

// layoutProbe is the key that tells a U.S. hardware layout from the others:
// semicolon's key is ";" on a U.S. layout and a letter on Colemak, Dvorak,
// AZERTY, QWERTZ and most others ("o", "s", "m", "ö"). The first probe, "a",
// tells input methods from Latin keyboards, but sits where it is on most
// layouts.
const layoutProbe = ';'

// probeKeyboard types probes into the focused field at the cursor and
// deletes them again: "a", which an input method composes (a candidate bar
// in the app's own tree, sometimes only after a few hundred milliseconds,
// or a non-Latin letter in the field), and then, on a Latin keyboard whose
// field can be read, layoutProbe. Callers hold inputMu.
func (d *Device) probeKeyboard(ctx context.Context, rax *raxSession, note func(string, ...any)) (keyboardProbe, error) {
	kb, err := d.latinKeyboard(ctx, rax, note)
	if err != nil || kb.kind != kbLatin {
		return kb, err
	}
	field, before := d.focusedValue(ctx)
	if field == nil {
		return kb, nil // the layout cannot be checked
	}
	u, _, _ := asciiUsage(layoutProbe)
	if err := d.pressKeys(ctx, []keyStroke{{usage: u}}); err != nil {
		return kb, err
	}
	defer d.pressKeys(context.WithoutCancel(ctx), []keyStroke{{usage: keyBackspace}})
	changed, after := d.waitValueChange(ctx, field, before)
	if !changed {
		note("layout probe: no change read")
		return kb, nil
	}
	got := arrived(before, after)
	note("layout probe: as on a U.S. layout %v", got == string(layoutProbe))
	if got != string(layoutProbe) {
		return keyboardProbe{kind: kbLayout, key: string(layoutProbe), got: got}, nil
	}
	return kb, nil
}

// latinKeyboard types the probe letter "a" and reports whether it arrived
// as itself (a Latin keyboard), as an input method's composition, or as
// another Latin letter (a Latin keyboard whose layout moves "a", such as
// AZERTY). The probe is deleted again either way. Callers hold inputMu.
func (d *Device) latinKeyboard(ctx context.Context, rax *raxSession, note func(string, ...any)) (keyboardProbe, error) {
	_, before := d.focusedValue(ctx)
	if err := d.pressKeys(ctx, []keyStroke{{usage: keyA}}); err != nil {
		return keyboardProbe{}, err
	}
	defer d.pressKeys(context.WithoutCancel(ctx), []keyStroke{{usage: keyBackspace}})
	start := time.Now()
	for {
		if err := sleepCtx(ctx, 150*time.Millisecond); err != nil {
			return keyboardProbe{}, err
		}
		field, value := d.focusedValue(ctx)
		lim := d.walkLimits()
		wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), lim.Budget+raxRequestTimeout)
		els, _, err := rax.walk(wctx, lim)
		cancel()
		if err != nil {
			return keyboardProbe{}, err
		}
		cands := hasCandidates(els)
		got := ""
		if value != before {
			got = arrived(before, value)
		}
		note("probe: field %v, arrived %v, candidates %v", field != nil, got == "a" || got == "A", cands)
		switch {
		case cands:
			return keyboardProbe{kind: kbComposing}, nil
		case got == "a" || got == "A":
		case otherLatin(got):
			return keyboardProbe{kind: kbLayout, key: "a", got: got}, nil
		case got != "":
			return keyboardProbe{kind: kbComposing}, nil // a letter of another script
		}
		if time.Since(start) >= candidateWait {
			if field != nil && got == "" && !probeArrived(value) {
				return keyboardProbe{kind: kbComposing}, nil // nothing arrived that can be read
			}
			return keyboardProbe{kind: kbLatin}, nil
		}
	}
}

// arrived returns what typing one key put into a field whose value was
// before and is after now: the text added where the cursor was, or, when
// after is not before with text added (a placeholder that went away), its
// last character.
func arrived(before, after string) string {
	b, a := []rune(before), []rune(after)
	if len(a) > len(b) {
		i := 0
		for i < len(b) && a[i] == b[i] {
			i++
		}
		j := 0
		for j < len(b)-i && a[len(a)-1-j] == b[len(b)-1-j] {
			j++
		}
		if i+j == len(b) {
			return string(a[i : len(a)-j])
		}
	}
	if len(a) == 0 {
		return ""
	}
	return string(a[len(a)-1])
}

// otherLatin reports whether s is a single printable ASCII character other
// than the probe's "a": a Latin keyboard put another character on its key.
func otherLatin(s string) bool {
	return len(s) == 1 && s[0] > ' ' && s[0] < 0x7f && s != "a" && s != "A"
}

// layoutError says why Type typed nothing: keys would enter other text.
func layoutError(kb keyboardProbe, pbErr error) error {
	return fmt.Errorf("type: %s, so Pinky would type by keys, but the phone's hardware keyboard layout is not U.S. (the key that is %q on a U.S. layout typed %q), so the keys would enter other text. Nothing was typed. Try again, as the pasteboard may answer then, or set the hardware keyboard layout to U.S. in the phone's Settings › General › Keyboard › Hardware Keyboard: %w",
		keysWhy(pbErr), kb.key, kb.got, driver.ErrUnsupported)
}

// probeArrived reports whether the field's value ends with the probe letter
// as typed, or capitalized by the keyboard's auto-capitalization.
func probeArrived(value string) bool {
	return strings.HasSuffix(value, "a") || strings.HasSuffix(value, "A")
}

// hasCandidates reports whether an input method's candidate bar is on
// screen: cells or buttons identified by a short non-Latin candidate that
// starts their label ("啊, 叹词的啊", identifier "啊").
func hasCandidates(els []raxElement) bool {
	for _, e := range els {
		id := []rune(e.Ident)
		if len(id) == 0 || len(id) > 8 || id[0] < 0x80 {
			continue
		}
		if strings.HasPrefix(e.Label, e.Ident) {
			return true
		}
	}
	return false
}
