package ios

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tetratorus/lightpinky/internal/driver"
)

type unlockPhone struct {
	on, isLocked  bool
	swipes, keys  int
	unlockAt      int
	unlockOnSwipe bool
	swipeErr      error
	keyErr        error
}

func (p *unlockPhone) screenOn(context.Context) (bool, error) { return p.on, nil }
func (p *unlockPhone) locked(context.Context) (bool, error)   { return p.isLocked, nil }
func (p *unlockPhone) pressHome(context.Context) error        { p.on = true; return nil }
func (p *unlockPhone) swipeUp(context.Context) error {
	p.swipes++
	if p.unlockOnSwipe {
		p.isLocked = false
	}
	return p.swipeErr
}
func (p *unlockPhone) pressKey(context.Context, keyStroke) error {
	p.keys++
	if p.keys == p.unlockAt {
		p.isLocked = false
	}
	return p.keyErr
}
func (p *unlockPhone) sleep(ctx context.Context, _ time.Duration) error { return ctx.Err() }

func TestUnlockWithoutAccessibility(t *testing.T) {
	for _, tc := range []struct {
		name         string
		phone        unlockPhone
		keys, swipes int
		err          error
	}{
		{"locked asleep", unlockPhone{isLocked: true, unlockAt: 6}, 6, 1, nil},
		{"already unlocked", unlockPhone{on: true}, 0, 0, nil},
		{"face ID during swipe", unlockPhone{on: true, isLocked: true, unlockOnSwipe: true}, 0, 1, nil},
		{"unlocked during input", unlockPhone{on: true, isLocked: true, unlockAt: 2}, 2, 1, nil},
		{"swipe failed", unlockPhone{isLocked: true, swipeErr: driver.ErrOffline}, 0, 1, driver.ErrPINNotEntered},
		{"input failed", unlockPhone{isLocked: true, keyErr: driver.ErrOffline}, 1, 1, driver.ErrOffline},
		{"code rejected once", unlockPhone{isLocked: true}, 6, 1, driver.ErrLocked},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := unlock(context.Background(), &tc.phone, "123456")
			if !errors.Is(err, tc.err) || tc.phone.keys != tc.keys || tc.phone.swipes != tc.swipes {
				t.Fatalf("err=%v keys=%d swipes=%d; want err=%v keys=%d swipes=%d",
					err, tc.phone.keys, tc.phone.swipes, tc.err, tc.keys, tc.swipes)
			}
		})
	}
}
