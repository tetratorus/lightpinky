package ios

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tetratorus/lightpinky/internal/driver"
	"github.com/tetratorus/lightpinky/internal/screen"
)

// Waking and unlocking. The screen's state comes from CoreDevice's display
// info (the backlight state SpringBoard's BacklightServices reports), the
// lock state from lockdown (see Locked). Pinky wakes a phone with the Home
// button only: the side button would lock a phone whose screen is on.

// Backlight states, from BacklightServices.
const (
	backlightOn       = "activeOn"
	backlightDimmed   = "activeDimmed" // about to turn off
	backlightAlwaysOn = "inactiveOn"   // the always-on display: asleep, shown dimmed
	backlightOff      = "off"
)

// screenAwake reports whether a backlight state means the screen is on and
// takes touches. The always-on display counts as asleep.
func screenAwake(state string) (bool, error) {
	switch state {
	case backlightOn, backlightDimmed:
		return true, nil
	case backlightOff, backlightAlwaysOn:
		return false, nil
	}
	return false, fmt.Errorf("unknown backlight state %q", state)
}

// ScreenOn implements driver.Waker.
func (d *Device) ScreenOn(ctx context.Context) (bool, error) {
	d.svcMu.Lock()
	svc := d.info2
	d.svcMu.Unlock()
	if svc == nil {
		return false, fmt.Errorf("display info: %w", driver.ErrOffline)
	}
	si, err := readScreenInfo(ctx, svc)
	if err != nil && isClosedErr(err) && ctx.Err() == nil {
		// The phone closed the connection (it locked, or a long session
		// went quiet); the failed request dropped it, so this dials again.
		si, err = readScreenInfo(ctx, svc)
	}
	if err != nil {
		return false, err
	}
	return screenAwake(si.Backlight)
}

// Wake implements driver.Waker: it presses Home when the screen is off and
// waits for it to come on. A phone with a passcode wakes to its lock screen.
func (d *Device) Wake(ctx context.Context) error {
	return wake(ctx, devicePower{d})
}

// Unlock implements driver.Unlocker. It wakes the screen, swipes up from
// the bottom edge for the passcode pad, checks that the pad is showing,
// types the passcode on the virtual keyboard, checking before each key that
// the phone is still locked, presses Return unless the passcode is four or
// six digits (those submit themselves), and returns nil once lockdown says
// the phone is unlocked, or ErrLocked. It never types the passcode twice. A
// phone that is already unlocked is only woken.
func (d *Device) Unlock(ctx context.Context, pin string) error {
	return unlock(ctx, devicePower{d}, pin)
}

// powerPhone is what waking and unlocking need from a phone; tests fake it.
type powerPhone interface {
	// freshAX drops the accessibility session and any failure kept from
	// it, so the pad is looked for in a session made on the lock screen:
	// one made before the phone locked may no longer see it.
	freshAX()
	screenOn(ctx context.Context) (bool, error)
	locked(ctx context.Context) (bool, error)
	pressHome(ctx context.Context) error
	swipeUp(ctx context.Context) error
	keypadShown(ctx context.Context) (bool, error)
	pressKey(ctx context.Context, k keyStroke) error
	sleep(ctx context.Context, d time.Duration) error
}

// Timing for waking and unlocking.
const (
	wakeWait    = 2 * time.Second        // for the screen to come on after Home
	keypadWait  = 3 * time.Second        // for the passcode pad after each swipe
	unlockWait  = 4 * time.Second        // for lockdown to report the unlock
	powerPoll   = 150 * time.Millisecond // between checks
	pinKeyGap   = 120 * time.Millisecond // between passcode keys
	afterWake   = 500 * time.Millisecond // before the first swipe on a screen that just lit up
	afterSwipe  = 600 * time.Millisecond // before looking for the pad
	afterKeypad = 300 * time.Millisecond // before the first passcode key
)

// unlockSwipes is how many times Unlock swipes up for the passcode pad. A
// lock screen with notifications can take the first swipe for its
// notification list (seen once in the field, where a second try worked), and
// one that has just lit up can miss it. A swipe on the lock screen types
// nothing, so trying again uses no attempt.
const unlockSwipes = 3

func wake(ctx context.Context, p powerPhone) error {
	_, err := wakeUp(ctx, p)
	return err
}

// wakeUp is wake that reports whether the screen was dark, so that it had
// to press Home.
func wakeUp(ctx context.Context, p powerPhone) (bool, error) {
	on, err := p.screenOn(ctx)
	if err != nil {
		return false, err
	}
	if on {
		return false, nil
	}
	if err := p.pressHome(ctx); err != nil {
		return true, err
	}
	return true, waitFor(ctx, p, wakeWait, p.screenOn, "the screen did not turn on")
}

// errWaitedOut marks waitFor's failure when the wait passed.
var errWaitedOut = errors.New("waited out")

// waitFor polls cond until it holds or wait passes. Then the error says
// failure and wraps errWaitedOut.
func waitFor(ctx context.Context, p powerPhone, wait time.Duration, cond func(context.Context) (bool, error), failure string) error {
	for waited := time.Duration(0); ; waited += powerPoll {
		ok, err := cond(ctx)
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		if waited >= wait {
			return &waitError{failure}
		}
		if err := p.sleep(ctx, powerPoll); err != nil {
			return err
		}
	}
}

// waitError is waitFor's error for a wait that passed.
type waitError struct{ failure string }

func (e *waitError) Error() string        { return e.failure }
func (e *waitError) Is(target error) bool { return target == errWaitedOut }

// pinStrokes turns a passcode into key strokes; passcodes are printable
// ASCII (digits, or letters and symbols for an alphanumeric passcode).
func pinStrokes(pin string) ([]keyStroke, error) {
	if pin == "" {
		return nil, errors.New("unlock: empty passcode")
	}
	strokes := make([]keyStroke, 0, len(pin))
	for _, r := range pin {
		u, shift, ok := asciiUsage(r)
		if !ok || r == ' ' || r == '\n' || r == '\t' {
			return nil, fmt.Errorf("unlock: a passcode character cannot be typed on the virtual keyboard: %w", driver.ErrUnsupported)
		}
		ks := keyStroke{usage: u}
		if shift {
			ks.mod = keyLeftShift
		}
		strokes = append(strokes, ks)
	}
	return strokes, nil
}

// submitsItself reports whether iOS submits a passcode once its last digit
// is typed: the four- and six-digit numeric passcodes do; custom numeric and
// alphanumeric ones wait for Return.
func submitsItself(pin string) bool {
	if len(pin) != 4 && len(pin) != 6 {
		return false
	}
	for _, r := range pin {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func unlock(ctx context.Context, p powerPhone, pin string) error {
	strokes, err := pinStrokes(pin)
	if err != nil {
		return err
	}
	unlocked, err := reachPad(ctx, p)
	if err != nil || unlocked {
		return err
	}
	if err := p.sleep(ctx, afterKeypad); err != nil {
		return err
	}
	// From the first key on, this is the one attempt: finish it whatever
	// ctx says, and never type the passcode again. Before each key, check
	// that the phone is still locked: once it is not, the keys would go to
	// whatever app is in front.
	actx := context.WithoutCancel(ctx)
	if !submitsItself(pin) {
		strokes = append(strokes, keyStroke{usage: keyEnter})
	}
	for i, k := range strokes {
		l, err := p.locked(actx)
		if err != nil {
			return fmt.Errorf("unlock: %w", err)
		}
		if !l {
			return nil
		}
		if err := p.pressKey(actx, k); err != nil {
			return fmt.Errorf("unlock: typing the passcode: %w", err)
		}
		if i < len(strokes)-1 {
			p.sleep(actx, pinKeyGap)
		}
	}
	isUnlocked := func(c context.Context) (bool, error) {
		l, err := p.locked(c)
		return !l, err
	}
	if err := waitFor(actx, p, unlockWait, isUnlocked, "still locked"); err != nil {
		return fmt.Errorf("unlock: %v: %w", err, driver.ErrLocked)
	}
	return nil
}

// reachPad wakes the phone and brings up its passcode pad, typing nothing.
// It reports unlocked when the phone needs no passcode after all: it was
// not locked, or Face ID opened it on the way. An error wraps
// ErrPINNotEntered once the phone was found locked.
func reachPad(ctx context.Context, p powerPhone) (unlocked bool, err error) {
	woke, err := wakeUp(ctx, p)
	if err != nil {
		return false, fmt.Errorf("unlock: %w", err)
	}
	locked, err := p.locked(ctx)
	if err != nil {
		return false, fmt.Errorf("unlock: %w", err)
	}
	if !locked {
		return true, nil
	}
	p.freshAX()
	if woke {
		// The lock screen takes a moment to come up behind the backlight.
		if err := p.sleep(ctx, afterWake); err != nil {
			return false, err
		}
	}
	// Face ID can unlock the phone on the way, if its owner is there. A
	// failed look for the pad is tried again in a new session.
	var lookErr error
	padOrOpen := func(c context.Context) (bool, error) {
		l, err := p.locked(c)
		if err != nil || !l {
			unlocked = err == nil
			return unlocked, err
		}
		shown, err := p.keypadShown(c)
		if err != nil {
			lookErr = err
			p.freshAX()
			return false, nil
		}
		return shown, nil
	}
	for swipe := 1; ; swipe++ {
		if swipe > 1 {
			// The screen may have gone dark while the pad was looked for.
			if _, err := wakeUp(ctx, p); err != nil {
				return false, fmt.Errorf("unlock: %w: %w", err, driver.ErrPINNotEntered)
			}
		}
		if err := p.swipeUp(ctx); err != nil {
			return false, fmt.Errorf("unlock: %w: %w", err, driver.ErrPINNotEntered)
		}
		if err := p.sleep(ctx, afterSwipe); err != nil {
			return false, err
		}
		err := waitFor(ctx, p, keypadWait, padOrOpen, "the passcode pad did not appear")
		if err == nil {
			return unlocked, nil
		}
		if !errors.Is(err, errWaitedOut) || swipe == unlockSwipes {
			if lookErr != nil {
				err = fmt.Errorf("%v (last error: %v)", err, lookErr)
			}
			if errors.Is(err, errWaitedOut) {
				err = fmt.Errorf("%v after %d swipes", err, swipe)
			}
			// Nothing was typed, so no attempt was used.
			return false, fmt.Errorf("unlock: %v: %w", err, driver.ErrPINNotEntered)
		}
	}
}

// devicePower is a Device as a powerPhone.
type devicePower struct{ d *Device }

func (p devicePower) screenOn(ctx context.Context) (bool, error) { return p.d.ScreenOn(ctx) }

func (p devicePower) freshAX() {
	d := p.d
	d.svcMu.Lock()
	defer d.svcMu.Unlock()
	if d.rax != nil {
		d.rax.Close()
		d.rax = nil
	}
	d.raxErr = nil
}
func (p devicePower) locked(ctx context.Context) (bool, error) { return p.d.Locked(ctx) }

func (p devicePower) pressHome(ctx context.Context) error {
	p.d.inputMu.Lock()
	defer p.d.inputMu.Unlock()
	h, err := p.d.waitInput(ctx)
	if err != nil {
		return err
	}
	return h.button(ctx, consumerPage, usageHome, 60*time.Millisecond)
}

// swipeUp swipes from the bottom edge to the middle of the screen, which on
// the lock screen brings up the passcode pad.
func (p devicePower) swipeUp(ctx context.Context) error {
	s := p.d.freshDisplay(ctx).upright()
	x := s.Width / 2
	return p.d.Swipe(ctx, screen.Point{X: x, Y: s.Height - 1}, screen.Point{X: x, Y: s.Height / 2}, 300*time.Millisecond)
}

// keypadShown looks for the passcode pad: buttons for all ten digits (their
// labels start with the digit, "2, ABC", whatever the language), or a
// secure text field for an alphanumeric passcode.
func (p devicePower) keypadShown(ctx context.Context) (bool, error) {
	rax, err := p.d.raxReady(ctx)
	if err != nil {
		return false, err
	}
	lim := p.d.walkLimits()
	c, cancel := context.WithTimeout(context.WithoutCancel(ctx), lim.Budget+raxRequestTimeout)
	defer cancel()
	els, _, err := rax.walk(c, lim)
	if err != nil {
		return false, err
	}
	return passcodePad(els), nil
}

// passcodePad reports whether elements include a passcode pad.
func passcodePad(els []raxElement) bool {
	var digits [10]bool
	for _, e := range els {
		if e.Type == xcSecureTextField {
			return true
		}
		l := strings.TrimSpace(e.Label)
		if l == "" || l[0] < '0' || l[0] > '9' || (len(l) > 1 && l[1] != ',' && l[1] != ' ') {
			continue
		}
		digits[l[0]-'0'] = true
	}
	for _, ok := range digits {
		if !ok {
			return false
		}
	}
	return true
}

func (p devicePower) pressKey(ctx context.Context, k keyStroke) error {
	p.d.inputMu.Lock()
	defer p.d.inputMu.Unlock()
	return p.d.pressKeys(ctx, []keyStroke{k})
}

func (p devicePower) sleep(ctx context.Context, d time.Duration) error { return sleepCtx(ctx, d) }

var (
	_ driver.Waker    = (*Device)(nil)
	_ driver.Unlocker = (*Device)(nil)
)
