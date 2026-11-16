package ios

import (
	"context"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/tetratorus/lightpinky/internal/driver"
	"github.com/tetratorus/lightpinky/internal/screen"
)

func (d *nativePhone) Screenshot(ctx context.Context) (image.Image, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "lightpinky-coredevice-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "screenshot.png")
	if err := d.command(ctx, nil, "device", "capture", "screenshot", "--destination", path); err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening CoreDevice screenshot: %w", err)
	}
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		return nil, fmt.Errorf("decoding CoreDevice screenshot: %w", err)
	}
	return img, nil
}

func (d *nativePhone) Apps(ctx context.Context) ([]driver.App, error) {
	var result struct {
		Apps []struct {
			BundleIdentifier string `json:"bundleIdentifier"`
			Name             string `json:"name"`
			Hidden           bool   `json:"hidden"`
			InternalApp      bool   `json:"internalApp"`
			AppClip          bool   `json:"appClip"`
			Removable        *bool  `json:"removable"`
		} `json:"apps"`
	}
	if err := d.command(ctx, &result, "device", "info", "apps", "--include-all-apps"); err != nil {
		return nil, err
	}
	apps := make([]driver.App, 0, len(result.Apps))
	for _, a := range result.Apps {
		if a.BundleIdentifier == "" || a.Hidden || a.InternalApp || a.AppClip {
			continue
		}
		name := a.Name
		if name == "" {
			name = a.BundleIdentifier
		}
		apps = append(apps, driver.App{
			ID: a.BundleIdentifier, Name: name,
			System: a.Removable != nil && !*a.Removable,
		})
	}
	sort.Slice(apps, func(i, j int) bool {
		if apps[i].Name == apps[j].Name {
			return apps[i].ID < apps[j].ID
		}
		return apps[i].Name < apps[j].Name
	})
	return apps, nil
}

func (d *nativePhone) Launch(ctx context.Context, app string) error {
	app = strings.TrimSpace(app)
	if app == "" {
		return fmt.Errorf("no app given: %w", driver.ErrNotFound)
	}
	apps, err := d.Apps(ctx)
	if err != nil {
		return err
	}
	id := ""
	for _, a := range apps {
		if a.ID == app {
			id = a.ID
			break
		}
	}
	if id == "" {
		for _, a := range apps {
			if strings.EqualFold(a.ID, app) || strings.EqualFold(a.Name, app) {
				if id != "" {
					return fmt.Errorf("app name %q is ambiguous; use a bundle identifier", app)
				}
				id = a.ID
			}
		}
	}
	if id == "" {
		return fmt.Errorf("app %q: %w", app, driver.ErrNotFound)
	}
	return d.command(ctx, nil, "device", "process", "launch", id)
}

type nativeDisplay struct {
	BacklightState string    `json:"backlightState"`
	Bounds         [2][2]int `json:"bounds"`
	Primary        bool      `json:"primary"`
}

func (d *nativePhone) display(ctx context.Context) (nativeDisplay, error) {
	var result struct {
		BacklightState string          `json:"backlightState"`
		Displays       []nativeDisplay `json:"displays"`
	}
	if err := d.command(ctx, &result, "device", "info", "displays"); err != nil {
		return nativeDisplay{}, err
	}
	for _, display := range result.Displays {
		if display.Primary {
			if display.BacklightState == "" {
				display.BacklightState = result.BacklightState
			}
			return display, nil
		}
	}
	return nativeDisplay{}, fmt.Errorf("CoreDevice returned no primary display: %w", driver.ErrUnsupported)
}

func (d *nativePhone) ScreenOn(ctx context.Context) (bool, error) {
	display, err := d.display(ctx)
	if err != nil {
		return false, err
	}
	return screenAwake(display.BacklightState)
}

func (*nativePhone) Wake(context.Context) error { return driver.ErrUnsupported }

func (d *nativePhone) Locked(ctx context.Context) (bool, error) {
	var result struct {
		PasscodeRequired *bool `json:"passcodeRequired"`
	}
	if err := d.command(ctx, &result, "device", "info", "lockState"); err != nil {
		return false, err
	}
	if result.PasscodeRequired == nil {
		return false, fmt.Errorf("CoreDevice returned no lock state: %w", driver.ErrUnsupported)
	}
	return *result.PasscodeRequired, nil
}

func (d *nativePhone) Observe(ctx context.Context, opt driver.ObserveOptions) (*screen.Screen, error) {
	locked, err := d.Locked(ctx)
	if err != nil {
		return nil, err
	}
	display, err := d.display(ctx)
	if err != nil {
		return nil, err
	}
	width, height := display.Bounds[1][0], display.Bounds[1][1]
	if width <= 0 || height <= 0 {
		return nil, fmt.Errorf("CoreDevice returned no display dimensions: %w", driver.ErrUnsupported)
	}
	orientation := "portrait"
	if width > height {
		orientation = "landscape"
	}
	return &screen.Screen{
		Width: width, Height: height, Orientation: orientation,
		Locked: locked, CapturedAt: time.Now(),
	}, nil
}

var _ driver.Waker = (*nativePhone)(nil)
