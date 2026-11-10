package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/tetratorus/lightpinky/internal/driver"
	"github.com/tetratorus/lightpinky/internal/driver/ios"
)

type phoneProvider interface {
	List(context.Context) ([]driver.Info, error)
	Open(context.Context, string) (driver.Device, error)
	UseOverWiFi(string)
	DownloadDiskImage(context.Context, func(int64, int64)) error
}

func (ph *phones) discover(ctx context.Context) ([]driver.Info, error) {
	until := time.Now().Add(15 * time.Second)
	for {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		infos, err := ph.p.List(ctx)
		if err != nil {
			return nil, err
		}
		refresh := false
		if ph.wifi {
			for _, in := range infos {
				ph.p.UseOverWiFi(in.ID)
				refresh = refresh || in.Setup == driver.SetupConfirmWiFi
			}
		}
		if refresh && time.Now().Before(until) {
			continue
		}
		if len(infos) > 0 {
			for i := range infos {
				if lockedWiFi(infos[i]) {
					infos[i].Detail = "Locked on Wi-Fi. Call unlock to use IPHONE_UNLOCK_CODE, or power with wake:true to wake without unlocking."
				}
			}
			return infos, nil
		}
		if time.Now().After(until) {
			return []driver.Info{}, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// waitPrepared lists the phones, waiting while the wanted phone (or, with no
// id, every phone) is still being prepared by lightpinky itself, as a
// Simulator is while its WebDriverAgent is built the first time.
func (ph *phones) waitPrepared(ctx context.Context, id string) ([]driver.Info, error) {
	until := time.Now().Add(prepareTimeout)
	for {
		infos, err := ph.list(ctx)
		if err != nil {
			return nil, err
		}
		preparing, ready := false, false
		for _, in := range infos {
			if id != "" && in.ID != id {
				continue
			}
			if in.Setup == driver.SetupPreparing {
				preparing = true
			} else if in.State == driver.StateOnline {
				ready = true
			}
		}
		if !preparing || ready && id == "" || time.Now().After(until) {
			return infos, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// prepareTimeout bounds waitPrepared; the first WebDriverAgent build after
// an Xcode update can take minutes.
var prepareTimeout = 15 * time.Minute

func lockedWiFi(info driver.Info) bool {
	return info.Connection == "wifi" && info.State == driver.StateNeedsSetup && info.Setup == driver.SetupUnlock
}

func (ph *phones) interactiveDevice(ctx context.Context, id string) (driver.Device, error) {
	return ph.openDevice(ctx, id, true)
}

func (ph *phones) openDevice(ctx context.Context, id string, interactive bool) (driver.Device, error) {
	ph.mu.Lock()
	defer ph.mu.Unlock()
	if d := ph.open[id]; id != "" && d != nil {
		return d, nil
	}
	infos, err := ph.waitPrepared(ctx, id)
	if err != nil {
		return nil, err
	}
	var candidates []driver.Info
	for _, in := range infos {
		if id != "" {
			if in.ID == id {
				candidates = append(candidates, in)
			}
		} else if in.State == driver.StateOnline || ph.open[in.ID] != nil || interactive && lockedWiFi(in) {
			candidates = append(candidates, in)
		}
	}
	if len(candidates) == 0 {
		return nil, errors.New("no usable iPhone found; call devices for setup details (locked Wi-Fi phones can use unlock or power with wake:true)")
	}
	if len(candidates) > 1 {
		ids := make([]string, len(candidates))
		for i, in := range candidates {
			ids[i] = in.ID
		}
		return nil, fmt.Errorf("several iPhones are available (%s); pass device", strings.Join(ids, ", "))
	}
	info := candidates[0]
	if d := ph.open[info.ID]; d != nil {
		return d, nil
	}
	var d driver.Device
	if interactive && lockedWiFi(info) {
		open := ph.openLocked
		if open == nil {
			open = func(ctx context.Context, in driver.Info) (driver.Device, error) {
				return ios.Open(ctx, in)
			}
		}
		d, err = connect(ctx, func() (driver.Device, error) { return open(ctx, info) })
	} else if info.State == driver.StateOnline {
		d, err = connect(ctx, func() (driver.Device, error) { return ph.p.Open(ctx, info.ID) })
	} else {
		return nil, fmt.Errorf("%s needs %s: %s", info.ID, info.Setup, info.Detail)
	}
	if err != nil {
		return nil, err
	}
	if ph.open == nil {
		ph.open = map[string]driver.Device{}
	}
	ph.open[info.ID] = d
	return d, nil
}

func connect(ctx context.Context, open func() (driver.Device, error)) (driver.Device, error) {
	d, err := open()
	if err == nil || !(errors.Is(err, io.EOF) || strings.Contains(err.Error(), "EOF") || strings.Contains(err.Error(), "connection reset")) {
		return d, err
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(500 * time.Millisecond):
		return open()
	}
}

func (ph *phones) unlock(ctx context.Context, id string) error {
	ph.unlockMu.Lock()
	defer ph.unlockMu.Unlock()
	pin := os.Getenv("IPHONE_UNLOCK_CODE")
	if pin == "" {
		return errors.New("IPHONE_UNLOCK_CODE is not set in the server environment; use scripts/phone unlock to load it from ~/.zshrc, or unlock on the phone")
	}
	d, err := ph.interactiveDevice(ctx, id)
	if err != nil {
		return err
	}
	u, ok := d.(driver.Unlocker)
	if !ok {
		return driver.ErrUnsupported
	}
	if err := u.Unlock(ctx, pin); err != nil {
		if errors.Is(err, driver.ErrOffline) {
			ph.drop(d)
		}
		return fmt.Errorf("unlock did not complete; input was not retried: %w", err)
	}
	return nil
}
