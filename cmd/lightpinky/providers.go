package main

import (
	"context"
	"os"
	"path/filepath"

	"github.com/tetratorus/lightpinky/internal/driver"
	"github.com/tetratorus/lightpinky/internal/driver/ios"
)

// lister is a source of phones: iPhones through CoreDevice, or the Mac's
// booted iOS Simulators.
type lister interface {
	List(context.Context) ([]driver.Info, error)
	Open(context.Context, string) (driver.Device, error)
}

// allPhones lists iPhones and booted Simulators together and opens each
// through the provider that listed it.
type allPhones struct {
	phone *ios.Provider
	sims  lister // nil off macOS or with -simulators=false
}

func (a *allPhones) List(ctx context.Context) ([]driver.Info, error) {
	infos, err := a.phone.List(ctx)
	if err != nil || a.sims == nil {
		return infos, err
	}
	sims, err := a.sims.List(ctx)
	if err != nil {
		return infos, nil
	}
	return append(infos, sims...), nil
}

func (a *allPhones) Open(ctx context.Context, id string) (driver.Device, error) {
	if a.sims != nil {
		sims, err := a.sims.List(ctx)
		if err == nil {
			for _, in := range sims {
				if in.ID == id {
					return a.sims.Open(ctx, id)
				}
			}
		}
	}
	return a.phone.Open(ctx, id)
}

func (a *allPhones) UseOverWiFi(id string) { a.phone.UseOverWiFi(id) }

func (a *allPhones) DownloadDiskImage(ctx context.Context, progress func(int64, int64)) error {
	return a.phone.DownloadDiskImage(ctx, progress)
}

// simulatorDir is where the WebDriverAgent build for Simulators and its
// runs are kept.
func simulatorDir() string {
	d, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(d, "lightpinky", "WebDriverAgent")
}
