package ios

import (
	"context"
	"fmt"
	"image"
	"sync"
	"time"

	"github.com/tetratorus/lightpinky/internal/driver"
	"github.com/tetratorus/lightpinky/internal/driver/ios/cryptex"
	goios "github.com/danielpaulus/go-ios/ios"
	"github.com/danielpaulus/go-ios/ios/imagemounter"
)

// An iPhone's checkup (driver.Checker): Developer Mode, the developer disk
// image and whether this computer has one to mount again should it go
// missing, the
// WebDriverAgent helper where Pinky drives the phone through it, a
// screenshot, and the input service. It only reads: no taps, no keys, no
// settings, nothing mounted.

// imageDirSetter is an opened iPhone that is told where Pinky keeps
// developer disk images (Provider.Open).
type imageDirSetter interface{ setImageDir(string) }

func (d *Device) setImageDir(dir string)    { d.imageDir = dir }
func (d *WDADevice) setImageDir(dir string) { d.imageDir = dir }
func (a *AXDevice) setImageDir(dir string)  { a.d.imageDir = dir }

// Checkup implements driver.Checker on CoreDevice (iOS 27 and later).
func (d *Device) Checkup(ctx context.Context) (driver.Checkup, error) {
	return runChecks(ctx, d.info,
		func(context.Context) driver.Check { return checkDevMode(d.l.base) },
		func(ctx context.Context) driver.Check { return checkImage(ctx, d.l, d.imageDir, true) },
		d.checkScreen,
		d.checkInput,
	)
}

// checkScreen takes a screenshot, and fails when the last start of the
// live picture got none: Pinky shows the phone and takes its input only
// with that picture.
func (d *Device) checkScreen(ctx context.Context) driver.Check {
	d.mu.Lock()
	nv := d.noVideo
	d.mu.Unlock()
	if nv != nil {
		return noVideoCheck(nv.Detail())
	}
	return checkScreenshot(ctx, d.Screenshot)
}

// Checkup implements driver.Checker through the WebDriverAgent helper.
func (d *WDADevice) Checkup(ctx context.Context) (driver.Checkup, error) {
	checks := []func(context.Context) driver.Check{d.checkHelper,
		func(ctx context.Context) driver.Check { return checkScreenshot(ctx, d.Screenshot) }}
	if d.entry.Properties.SerialNumber != "" { // not a simulator
		checks = append(checks,
			func(context.Context) driver.Check { return checkDevMode(d.entry) },
			func(ctx context.Context) driver.Check { return checkImage(ctx, d.l, d.imageDir, false) })
	}
	return runChecks(ctx, d.info, checks...)
}

// Checkup implements driver.Checker without the helper (iOS 17.4 to 26):
// the phone can be read but takes no input.
func (a *AXDevice) Checkup(ctx context.Context) (driver.Checkup, error) {
	d := a.d
	return runChecks(ctx, d.info,
		func(context.Context) driver.Check { return checkDevMode(d.l.base) },
		func(ctx context.Context) driver.Check { return checkImage(ctx, d.l, d.imageDir, false) },
		func(ctx context.Context) driver.Check { return checkScreenshot(ctx, a.Screenshot) },
		func(context.Context) driver.Check {
			return driver.Check{ID: driver.CheckHelper, Status: driver.CheckFail, Fix: driver.FixInstallHelper,
				Detail: "below iOS 27 an iPhone takes taps and typing only through the WebDriverAgent helper, which is not installed"}
		},
		func(context.Context) driver.Check {
			return driver.Check{ID: driver.CheckInput, Status: driver.CheckFail, Fix: driver.FixInstallHelper,
				Detail: "without the WebDriverAgent helper this iPhone takes no taps, swipes or typing"}
		},
	)
}

// runChecks runs the checks at once and keeps their order.
func runChecks(ctx context.Context, info driver.Info, checks ...func(context.Context) driver.Check) (driver.Checkup, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cu := driver.Checkup{Checks: make([]driver.Check, len(checks))}
	if info.OSVersion != "" {
		cu.System = "iOS " + info.OSVersion
	}
	var wg sync.WaitGroup
	for i, check := range checks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cu.Checks[i] = check(ctx)
		}()
	}
	wg.Wait()
	return cu, nil
}

// checkDevMode reads Developer Mode from the image mounter service.
func checkDevMode(e goios.DeviceEntry) driver.Check {
	c := driver.Check{ID: driver.CheckDeveloperMode}
	on, err := imagemounter.IsDevModeEnabled(e)
	switch {
	case err != nil:
		c.Status, c.Detail = driver.CheckSkip, err.Error()
	case on:
		c.Status, c.Detail = driver.CheckPass, "Developer Mode is on"
	default:
		c.Status, c.Fix, c.Detail = driver.CheckFail, driver.SetupDeveloperMode, devModeDetail
	}
	return c
}

// checkImage says which developer disk image the phone has, whether the
// services Pinky uses are there (the screen and HID with coreDevice, else
// legacyReady's), and whether this computer has an image to mount again
// should it go missing: iPhones below iOS 27 drop it when they restart.
func checkImage(ctx context.Context, l *link, ownDir string, coreDevice bool) driver.Check {
	c := driver.Check{ID: driver.CheckDiskImage}
	if l == nil {
		c.Status, c.Detail = driver.CheckSkip, "no tunnel to the iPhone"
		return c
	}
	if coreDevice && !(l.has(displayService) && l.has(universalHIDService)) || !coreDevice && !legacyReady(l) {
		c.Status, c.Fix, c.Detail = driver.CheckFail, driver.SetupDiskImage, errServicesMissing.Error()
		return c
	}
	mounted := mountedImage(ctx, l)
	// The image the preparer would take, passing over those Apple refused.
	refused := readRefusals(ownDir)
	img, err := findDiskImageAvoiding(ownDir, func(img diskImage) bool {
		_, ok := refused[refusalKey(img)]
		return ok
	})
	if err != nil {
		c.Status, c.Fix = driver.CheckWarn, driver.SetupNoDiskImage
		c.Detail = mounted + "; this computer has no developer disk image to mount again should it go missing, as it does below iOS 27 when the iPhone restarts (" + err.Error() + ")"
		return c
	}
	c.Status = driver.CheckPass
	c.Detail = fmt.Sprintf("%s; should it go missing, Pinky mounts %s again (%s)", mounted, imageName(img), img.Dir)
	return c
}

// mountedImage says what the phone has mounted, read only: the cryptex
// cryptexd lists, else whether the image mounter has a personalized image.
func mountedImage(ctx context.Context, l *link) string {
	type result struct{ s string }
	ch := make(chan result, 1)
	go func() {
		if l.has(cryptexService) {
			if m, err := cryptex.NewCryptexDeveloperDiskImageMounter(l.entry); err == nil {
				if list, err := m.ListCryptexes(); err == nil {
					for _, cx := range list {
						if cx.Identifier == ddiCryptex {
							ch <- result{"the developer disk image " + cx.Version + " is mounted as a cryptex"}
							return
						}
					}
				}
			}
		}
		if v, err := goios.GetProductVersion(l.base); err == nil {
			if m, err := imagemounter.NewPersonalizedDeveloperDiskImageMounter(l.base, v); err == nil {
				defer m.Close()
				if imgs, err := m.ListImages(); err == nil && len(imgs) > 0 {
					ch <- result{"a personalized developer disk image is mounted"}
					return
				}
			}
		}
		ch <- result{"the developer services are there"}
	}()
	select {
	case r := <-ch:
		return r.s
	case <-ctx.Done():
		return "the developer services are there"
	}
}

// imageName names a developer disk image for people: where it came from
// and its build.
func imageName(img diskImage) string {
	from := map[string]string{sourceXcode: "Xcode's image", sourcePinky: "the image in Pinky's folder",
		sourceEnv: "the image PINKY_DDI names", sourceMirror: "the image downloaded from " + mirrorName}[img.Source]
	if from == "" {
		from = "the image"
	}
	if img.Build != "" {
		from += " " + img.Build
	}
	return from
}

// checkScreenshot takes a screenshot.
func checkScreenshot(ctx context.Context, shot func(context.Context) (image.Image, error)) driver.Check {
	c := driver.Check{ID: driver.CheckScreen}
	img, err := shot(ctx)
	if err != nil {
		c.Status, c.Fix, c.Detail = driver.CheckFail, driver.FixScreenshot, err.Error()
		return c
	}
	b := img.Bounds()
	c.Status, c.Detail = driver.CheckPass, fmt.Sprintf("a %d×%d screenshot", b.Dx(), b.Dy())
	return c
}

// checkInput sees that the phone's HID service, which takes Pinky's taps
// and keys, accepts a connection. It sends no event: any would reach the
// phone.
func (d *Device) checkInput(ctx context.Context) driver.Check {
	c := driver.Check{ID: driver.CheckInput}
	if !d.l.has(universalHIDService) {
		c.Status, c.Detail = driver.CheckFail, "the iPhone does not offer "+universalHIDService
		return c
	}
	type result struct{ err error }
	ch := make(chan result, 1)
	go func() {
		x, err := dialXPC(d.l.entry, universalHIDService)
		if err == nil {
			x.Close()
		}
		ch <- result{err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			c.Status, c.Detail = driver.CheckFail, "the HID service does not answer: "+r.err.Error()
			return c
		}
	case <-ctx.Done():
		c.Status, c.Detail = driver.CheckFail, "the HID service does not answer: "+ctx.Err().Error()
		return c
	}
	c.Status, c.Detail = driver.CheckPass, "the HID service answers (no event was sent)"
	return c
}

// checkHelper asks the WebDriverAgent helper for its status, and reads when
// its provisioning profile runs out: it warns driver.HelperReminder before,
// and once it has run out, as iOS then no longer starts the helper.
func (d *WDADevice) checkHelper(ctx context.Context) driver.Check {
	c := driver.Check{ID: driver.CheckHelper}
	exp, expErr := d.helperExpires(ctx)
	now := time.Now()
	st, err := d.c.status(ctx)
	if err != nil {
		c.Status, c.Fix, c.Detail = driver.CheckFail, driver.FixInstallHelper, "WebDriverAgent does not answer: "+err.Error()
		if !exp.IsZero() && !now.Before(exp) {
			c.Detail = helperExpired(exp) + " (" + err.Error() + ")"
		}
		return c
	}
	c.Status, c.Detail = driver.CheckPass, "WebDriverAgent answers"
	if st.Build.Version != "" {
		c.Detail += " (version " + st.Build.Version + ")"
	}
	switch {
	case expErr != nil:
		c.Detail += "; when its provisioning profile runs out is unknown: " + expErr.Error()
	case exp.IsZero():
	case !now.Before(exp):
		// It runs until it stops; iOS will not start it again.
		c.Status, c.Fix = driver.CheckWarn, driver.FixInstallHelper
		c.Detail += "; but its provisioning profile ran out on " + helperDate(exp) + ", so iOS will not start it again: install it again from Xcode"
	case exp.Sub(now) <= driver.HelperReminder:
		c.Status, c.Fix = driver.CheckWarn, driver.FixInstallHelper
		c.Detail += "; its provisioning profile runs out on " + helperDate(exp) + ": install it again from Xcode before then"
	default:
		c.Detail += "; its provisioning profile runs out on " + helperDate(exp)
	}
	return c
}

var (
	_ driver.Checker = (*Device)(nil)
	_ driver.Checker = (*WDADevice)(nil)
	_ driver.Checker = (*AXDevice)(nil)
)
