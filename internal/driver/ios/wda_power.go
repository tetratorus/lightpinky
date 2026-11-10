package ios

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/tetratorus/lightpinky/internal/driver"
	"github.com/tetratorus/lightpinky/internal/screen"
	goios "github.com/danielpaulus/go-ios/ios"
)

// Unlock implements driver.Unlocker through WebDriverAgent, with the steps
// and checks of the CoreDevice path (see unlock): Home, a swipe up from the
// bottom edge, the passcode pad found in the tree, the passcode tapped on
// the pad's keys (typed into the field of an alphanumeric passcode), the
// lock checked before every key, one attempt only.
func (d *WDADevice) Unlock(ctx context.Context, pin string) error {
	return unlock(ctx, &wdaPower{d: d}, pin)
}

// lockState reads whether the phone needs its passcode: from lockdown, as
// the CoreDevice path does, or from WDA on a simulator, which has no
// lockdown, or when lockdown does not answer (over Wi-Fi it can fail, and
// a failure must not read as unlocked: GitHub issue #7).
func (d *WDADevice) lockState(ctx context.Context) (bool, error) {
	if d.entry.DeviceID == 0 {
		return d.c.locked(ctx)
	}
	locked, err := d.lockdownLocked(ctx)
	if err != nil && ctx.Err() == nil {
		if l, werr := d.c.locked(ctx); werr == nil {
			return l, nil
		}
	}
	return locked, err
}

// lockdownLocked reads lockdown's PasswordProtected.
func (d *WDADevice) lockdownLocked(ctx context.Context) (bool, error) {
	if d.lockdownFn != nil {
		return d.lockdownFn(ctx)
	}
	type result struct {
		v   bool
		err error
	}
	ch := make(chan result, 1)
	go func() {
		conn, err := goios.ConnectLockdownWithSession(d.entry)
		if err != nil {
			ch <- result{err: err}
			return
		}
		defer conn.Close()
		v, err := conn.GetValue("PasswordProtected")
		if err != nil {
			ch <- result{err: err}
			return
		}
		b, ok := v.(bool)
		if !ok {
			ch <- result{err: fmt.Errorf("PasswordProtected is %T", v)}
			return
		}
		ch <- result{v: b}
	}()
	select {
	case r := <-ch:
		return r.v, r.err
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

// wdaPower is a WDADevice as a powerPhone.
type wdaPower struct {
	d *WDADevice

	mu     sync.Mutex
	woke   bool                  // Home was pressed on the locked phone
	keys   map[rune]screen.Point // the pad's digit keys, from the last look
	secure bool                  // the pad is a field: an alphanumeric passcode
}

func (p *wdaPower) freshAX() {}

// screenOn is the screen's state as far as WDA can tell, which is not the
// backlight: an unlocked phone's screen counts as on, so Home is never
// pressed on it (that would leave the app); a locked one's as off until
// Home woke it.
func (p *wdaPower) screenOn(ctx context.Context) (bool, error) {
	locked, err := p.d.lockState(ctx)
	if err != nil {
		return false, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return !locked || p.woke, nil
}

func (p *wdaPower) locked(ctx context.Context) (bool, error) { return p.d.lockState(ctx) }

func (p *wdaPower) pressHome(ctx context.Context) error {
	p.d.inputMu.Lock()
	err := p.d.c.pressHome(ctx)
	p.d.inputMu.Unlock()
	if err == nil {
		p.mu.Lock()
		p.woke = true
		p.mu.Unlock()
	}
	return err
}

// swipeUp swipes from the bottom edge to the middle of the screen, which on
// the lock screen brings up the passcode pad.
func (p *wdaPower) swipeUp(ctx context.Context) error {
	w, h := p.d.sizeNow(ctx)
	x := w / 2
	return p.d.Swipe(ctx, screen.Point{X: x, Y: h - 1}, screen.Point{X: x, Y: h / 2}, 300*time.Millisecond)
}

// keypadShown looks for the passcode pad in the tree WDA reads (SpringBoard
// on the lock screen) and remembers where its digit keys are.
func (p *wdaPower) keypadShown(ctx context.Context) (bool, error) {
	root, err := p.d.source(ctx)
	if err != nil {
		return false, err
	}
	els := flattenWDA(root)
	if !passcodePad(els) {
		return false, nil
	}
	keys, secure := padKeys(els, p.d.scale)
	p.mu.Lock()
	p.keys, p.secure = keys, secure
	p.mu.Unlock()
	return true, nil
}

// flattenWDA lists every visible node of WDA's tree as the remoteAX path
// describes elements, for passcodePad.
func flattenWDA(root *wdaNode) []raxElement {
	var out []raxElement
	var walk func(n *wdaNode, depth int)
	walk = func(n *wdaNode, depth int) {
		if depth > 80 {
			return
		}
		if e := raxFromWDA(n); e.Visible && e.HasFrame {
			out = append(out, e)
		}
		for i := range n.Children {
			walk(&n.Children[i], depth+1)
		}
	}
	walk(root, 0)
	return out
}

// padKeys finds the centers of the pad's digit keys, in screenshot pixels,
// and whether the pad is a secure field instead (an alphanumeric passcode,
// typed on the keyboard).
func padKeys(els []raxElement, scale float64) (map[rune]screen.Point, bool) {
	keys := map[rune]screen.Point{}
	for _, e := range els {
		if e.Type == xcSecureTextField {
			return nil, true
		}
		l := []rune(e.Label)
		if len(l) == 0 || l[0] < '0' || l[0] > '9' || (len(l) > 1 && l[1] != ',' && l[1] != ' ') {
			continue
		}
		if _, ok := keys[l[0]]; ok {
			continue
		}
		keys[l[0]] = screen.Point{
			X: int(math.Round((e.Frame.X + e.Frame.W/2) * scale)),
			Y: int(math.Round((e.Frame.Y + e.Frame.H/2) * scale)),
		}
	}
	return keys, false
}

// pressKey taps a digit's key on the pad, or types the character (and
// Return) into an alphanumeric passcode's field.
func (p *wdaPower) pressKey(ctx context.Context, k keyStroke) error {
	r, ok := strokeRune(k)
	if !ok {
		return fmt.Errorf("a key Pinky cannot type through WebDriverAgent: %w", driver.ErrUnsupported)
	}
	p.mu.Lock()
	pt, isKey := p.keys[r]
	secure := p.secure
	p.mu.Unlock()
	if !secure && isKey {
		return p.d.Tap(ctx, pt, driver.TapOptions{})
	}
	if !secure && r != '\n' {
		return fmt.Errorf("the passcode pad has no key for a character of the passcode: %w", driver.ErrUnsupported)
	}
	p.d.inputMu.Lock()
	defer p.d.inputMu.Unlock()
	return p.d.c.keys(ctx, string(r))
}

func (p *wdaPower) sleep(ctx context.Context, d time.Duration) error { return sleepCtx(ctx, d) }

// strokeRune turns a key stroke from pinStrokes back into its character.
func strokeRune(k keyStroke) (rune, bool) {
	if k.usage == keyEnter && k.mod == 0 {
		return '\n', true
	}
	for r := rune(0x21); r < 0x7f; r++ {
		u, shift, ok := asciiUsage(r)
		if !ok || u != k.usage {
			continue
		}
		if shift == (k.mod == keyLeftShift) {
			return r, true
		}
	}
	return 0, false
}

var _ driver.Unlocker = (*WDADevice)(nil)
