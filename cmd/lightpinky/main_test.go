package main

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/tetratorus/lightpinky/internal/driver"
)

type testProvider struct {
	infos     []driver.Info
	device    driver.Device
	lists     int
	opens     int
	emptyOnce bool
}

func (p *testProvider) List(context.Context) ([]driver.Info, error) {
	p.lists++
	if p.emptyOnce && p.lists == 1 {
		return nil, nil
	}
	return p.infos, nil
}

func (p *testProvider) Open(context.Context, string) (driver.Device, error) {
	p.opens++
	return p.device, nil
}

func (p *testProvider) UseOverWiFi(string) {}

func (p *testProvider) DownloadDiskImage(context.Context, func(int64, int64)) error {
	return nil
}

type testPhone struct {
	driver.Device
	unlocks int
	pin     string
	err     error
}

func (d *testPhone) Unlock(_ context.Context, pin string) error {
	d.unlocks++
	d.pin = pin
	return d.err
}

func (d *testPhone) Close() error { return nil }

func TestLockedWiFiUnlock(t *testing.T) {
	t.Setenv("IPHONE_UNLOCK_CODE", "test-passcode")
	d := &testPhone{}
	p := &testProvider{infos: []driver.Info{{
		ID: "phone", Connection: "wifi", State: driver.StateNeedsSetup, Setup: driver.SetupUnlock,
	}}}
	ph := &phones{p: p, open: map[string]driver.Device{}, openLocked: func(context.Context, driver.Info) (driver.Device, error) {
		return d, nil
	}}
	if err := ph.unlock(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if d.unlocks != 1 || d.pin != "test-passcode" || p.opens != 0 {
		t.Fatal("unlock must use the environment and the explicit locked-Wi-Fi connection")
	}
	if ph.open["phone"] != d {
		t.Fatal("subsequent tools must reuse the unlocked connection")
	}
}

func TestUnlockMissingEnvironmentDoesNotConnect(t *testing.T) {
	t.Setenv("IPHONE_UNLOCK_CODE", "")
	p := &testProvider{}
	ph := &phones{p: p}
	if err := ph.unlock(context.Background(), "phone"); err == nil || !strings.Contains(err.Error(), "IPHONE_UNLOCK_CODE") {
		t.Fatalf("expected environment setup error, got %v", err)
	}
	if p.lists != 0 || p.opens != 0 {
		t.Fatal("must not connect without the configured code")
	}
}

func TestUnlockNeverRetriesInput(t *testing.T) {
	t.Setenv("IPHONE_UNLOCK_CODE", "test-passcode")
	for _, failure := range []error{driver.ErrOffline, driver.ErrLocked} {
		d := &testPhone{err: failure}
		ph := &phones{open: map[string]driver.Device{"phone": d}}
		if err := ph.unlock(context.Background(), "phone"); !errors.Is(err, failure) {
			t.Fatalf("expected %v, got %v", failure, err)
		}
		if d.unlocks != 1 {
			t.Fatal("must never retry passcode input")
		}
	}
}

func TestExplicitWakeDoesNotSkipOtherSetup(t *testing.T) {
	for _, setup := range []string{driver.SetupTrust, driver.SetupDeveloperMode} {
		p := &testProvider{infos: []driver.Info{{
			ID: "phone", Connection: "wifi", State: driver.StateNeedsSetup, Setup: setup,
		}}}
		ph := &phones{p: p, open: map[string]driver.Device{}, openLocked: func(context.Context, driver.Info) (driver.Device, error) {
			t.Fatal("must not open a phone needing other setup")
			return nil, nil
		}}
		if _, err := ph.interactiveDevice(context.Background(), "phone"); err == nil {
			t.Fatal("expected setup error")
		}
	}
}

func TestDiscoveryWaitsForFirstPhone(t *testing.T) {
	p := &testProvider{emptyOnce: true, infos: []driver.Info{{ID: "phone", State: driver.StateOnline}}}
	ph := &phones{p: p}
	infos, err := ph.list(context.Background())
	if err != nil || len(infos) != 1 || p.lists < 2 {
		t.Fatalf("discovery returned before the first phone: %v %v", infos, err)
	}
}

func TestMultiplePhonesRequireSelection(t *testing.T) {
	p := &testProvider{infos: []driver.Info{
		{ID: "one", State: driver.StateOnline},
		{ID: "two", Connection: "wifi", State: driver.StateNeedsSetup, Setup: driver.SetupUnlock},
	}}
	ph := &phones{p: p}
	if _, err := ph.interactiveDevice(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "pass device") {
		t.Fatalf("expected selection error, got %v", err)
	}
}

func TestConnectRetriesBeforeInputOnly(t *testing.T) {
	calls := 0
	d := &testPhone{}
	got, err := connect(context.Background(), func() (driver.Device, error) {
		calls++
		if calls == 1 {
			return nil, io.EOF
		}
		return d, nil
	})
	if err != nil || got != d || calls != 2 {
		t.Fatalf("connection retry failed: %v, calls %d", err, calls)
	}
}

func TestCanceledDiscoveryReturnsPromptly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p := &testProvider{}
	ph := &phones{p: p}
	if _, err := ph.list(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation, got %v", err)
	}
	if p.lists != 0 {
		t.Fatal("must not start discovery after cancellation")
	}
}
