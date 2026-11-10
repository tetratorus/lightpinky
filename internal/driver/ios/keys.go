package ios

import (
	"context"
	"fmt"
	"time"

	"github.com/tetratorus/lightpinky/internal/driver"
	"github.com/tetratorus/lightpinky/internal/screen"
)

// HID usages on the consumer page (0x0C) that iOS maps to hardware buttons.
const (
	consumerPage = 0x0C
	usageHome    = 0x40 // "Menu", the home button
)

// HID keyboard usages (page 7).
const (
	keyA         = 0x04
	keyV         = 0x19
	key1         = 0x1E
	key0         = 0x27
	keyEnter     = 0x28
	keyEscape    = 0x29
	keyBackspace = 0x2A
	keyTab       = 0x2B
	keySpace     = 0x2C
	keyDeleteFwd = 0x4C
	keyRight     = 0x4F
	keyLeft      = 0x50
	keyDown      = 0x51
	keyUp        = 0x52
	keyPageUp    = 0x4B
	keyPageDown  = 0x4E
	keyLeftCtrl  = 0xE0
	keyLeftShift = 0xE1
	keyLeftGUI   = 0xE3 // Command
)

var keyUsages = map[driver.Key]uint8{
	driver.KeyEnter:         keyEnter,
	driver.KeyTab:           keyTab,
	driver.KeyDelete:        keyBackspace,
	driver.KeyForwardDelete: keyDeleteFwd,
	driver.KeyEscape:        keyEscape,
	driver.KeyDpadUp:        keyUp,
	driver.KeyDpadDown:      keyDown,
	driver.KeyDpadLeft:      keyLeft,
	driver.KeyDpadRight:     keyRight,
	driver.KeyPageUp:        keyPageUp,
	driver.KeyPageDown:      keyPageDown,
}

// asciiUsage maps a printable ASCII character to its usage on a US layout
// and whether Shift is needed.
func asciiUsage(r rune) (uint8, bool, bool) {
	switch {
	case r >= 'a' && r <= 'z':
		return keyA + uint8(r-'a'), false, true
	case r >= 'A' && r <= 'Z':
		return keyA + uint8(r-'A'), true, true
	case r >= '1' && r <= '9':
		return key1 + uint8(r-'1'), false, true
	case r == '0':
		return key0, false, true
	}
	if u, ok := asciiPunct[r]; ok {
		return u.usage, u.shift, true
	}
	return 0, false, false
}

var asciiPunct = map[rune]struct {
	usage uint8
	shift bool
}{
	' ': {keySpace, false}, '\n': {keyEnter, false}, '\t': {keyTab, false},
	'!': {key1, true}, '@': {key1 + 1, true}, '#': {key1 + 2, true}, '$': {key1 + 3, true},
	'%': {key1 + 4, true}, '^': {key1 + 5, true}, '&': {key1 + 6, true}, '*': {key1 + 7, true},
	'(': {key1 + 8, true}, ')': {key0, true},
	'-': {0x2D, false}, '_': {0x2D, true}, '=': {0x2E, false}, '+': {0x2E, true},
	'[': {0x2F, false}, '{': {0x2F, true}, ']': {0x30, false}, '}': {0x30, true},
	'\\': {0x31, false}, '|': {0x31, true}, ';': {0x33, false}, ':': {0x33, true},
	'\'': {0x34, false}, '"': {0x34, true}, '`': {0x35, false}, '~': {0x35, true},
	',': {0x36, false}, '<': {0x36, true}, '.': {0x37, false}, '>': {0x37, true},
	'/': {0x38, false}, '?': {0x38, true},
}

// Key implements driver.Device. Home is the hardware button, Back the
// system's left-edge swipe, and editing and arrow keys go through a virtual
// keyboard. iPhone has no Recents key.
func (d *Device) Key(ctx context.Context, k driver.Key) error {
	return d.gone(d.key(ctx, k))
}

// key is Key without marking a dropped connection (gone).
func (d *Device) key(ctx context.Context, k driver.Key) error {
	switch k {
	case driver.KeyHome:
		d.inputMu.Lock()
		defer d.inputMu.Unlock()
		h, err := d.waitInput(ctx)
		if err != nil {
			return err
		}
		return h.button(ctx, consumerPage, usageHome, 60*time.Millisecond)
	case driver.KeyBack:
		s := d.freshDisplay(ctx).upright()
		y := s.Height / 2
		return d.Swipe(ctx, screen.Point{X: 0, Y: y}, screen.Point{X: s.Width * 7 / 10, Y: y}, 250*time.Millisecond)
	case driver.KeyRecents:
		return fmt.Errorf("recents on iPhone: %w", driver.ErrUnsupported)
	}
	u, ok := keyUsages[k]
	if !ok {
		return fmt.Errorf("key %q: %w", k, driver.ErrUnsupported)
	}
	d.inputMu.Lock()
	defer d.inputMu.Unlock()
	return d.pressKeys(ctx, []keyStroke{{usage: u}})
}

type keyStroke struct {
	usage uint8
	mod   uint8 // a modifier held around the key, 0 for none
}

// pressKeys types strokes on the virtual keyboard, one press and release
// each, paced so the device sees every key. Callers hold inputMu.
func (d *Device) pressKeys(ctx context.Context, strokes []keyStroke) error {
	h, err := d.waitInput(ctx)
	if err != nil {
		return err
	}
	id, err := h.ensureKeyboard(ctx)
	if err != nil {
		return err
	}
	// Whatever happens below, nothing stays pressed: a modifier left down
	// would turn the person's next keys into shortcuts.
	defer h.keys(id, nil)
	const gap = 12 * time.Millisecond
	for _, s := range strokes {
		held := []uint8{s.usage}
		if s.mod != 0 {
			held = []uint8{s.mod, s.usage}
			if err := h.keys(id, []uint8{s.mod}); err != nil {
				return err
			}
			if err := sleepCtx(ctx, gap); err != nil {
				return err
			}
		}
		if err := h.keys(id, held); err != nil {
			return err
		}
		sleepCtx(context.Background(), gap) // a key that went down must come up
		if err := h.keys(id, nil); err != nil {
			return err
		}
		if err := sleepCtx(ctx, gap); err != nil {
			return err
		}
	}
	return nil
}
