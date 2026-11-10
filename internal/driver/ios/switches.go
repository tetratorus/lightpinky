package ios

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	goios "github.com/danielpaulus/go-ios/ios"

	"github.com/tetratorus/lightpinky/internal/driver"
)

// The switches agents set with phone_device (docs/notes/device-switches.md).
// WebDriverAgent has all three, on Simulators and on iPhones with the
// helper; CoreDevice has none, and opens apps in another language only.

var (
	_ driver.Switcher       = (*WDADevice)(nil)
	_ driver.LocaleLauncher = (*WDADevice)(nil)
	_ driver.LocaleLauncher = (*Device)(nil)
	_ driver.LocaleLauncher = (*AXDevice)(nil)
)

// wdaOrientations maps WDA's device orientations to rotation values, and
// wdaSetOrientations the rotation values to what POST /orientation takes.
var (
	wdaOrientations = map[string]string{
		"UIDeviceOrientationPortrait":           driver.RotationPortrait,
		"UIDeviceOrientationLandscapeLeft":      driver.RotationLandscape,
		"UIDeviceOrientationLandscapeRight":     driver.RotationReverseLandscape,
		"UIDeviceOrientationPortraitUpsideDown": driver.RotationUpsideDown,
	}
	wdaSetOrientations = map[string]string{
		driver.RotationPortrait:         "PORTRAIT",
		driver.RotationLandscape:        "LANDSCAPE",
		driver.RotationReverseLandscape: "UIA_DEVICE_ORIENTATION_LANDSCAPERIGHT",
		driver.RotationUpsideDown:       "UIA_DEVICE_ORIENTATION_PORTRAIT_UPSIDEDOWN",
	}
)

// Switch implements driver.Switcher.
func (d *WDADevice) Switch(ctx context.Context, name string) (string, error) {
	switch name {
	case driver.SwitchRotation:
		return d.orientation(ctx)
	case driver.SwitchAppearance:
		info, err := d.c.deviceInfo(ctx)
		if err != nil {
			return "", err
		}
		switch info.Style {
		case "light", "dark":
			return info.Style, nil
		}
		return "", fmt.Errorf("WebDriverAgent did not say whether the phone is light or dark (%q): %w", info.Style, driver.ErrUnsupported)
	case driver.SwitchLocation:
		var loc wdaLocation
		if err := d.c.call(ctx, d.c.hc, http.MethodGet, "/wda/simulatedLocation", nil, &loc); err != nil {
			return "", err
		}
		if loc.Latitude == nil || loc.Longitude == nil {
			return "", nil
		}
		return driver.FormatLocation(*loc.Latitude, *loc.Longitude), nil
	}
	return "", fmt.Errorf("no switch %q: %w", name, driver.ErrUnsupported)
}

// SetSwitch implements driver.Switcher.
func (d *WDADevice) SetSwitch(ctx context.Context, name, value string) error {
	switch name {
	case driver.SwitchRotation:
		return d.setOrientation(ctx, value)
	case driver.SwitchAppearance:
		if value != driver.AppearanceLight && value != driver.AppearanceDark {
			return fmt.Errorf("an iPhone's appearance is light or dark, not %q: %w", value, driver.ErrUnsupported)
		}
		if err := d.c.call(ctx, d.c.hc, http.MethodPost, "/wda/device/appearance", map[string]any{"name": value}, nil); err != nil {
			return err
		}
		if got, err := d.Switch(ctx, name); err == nil && got != value {
			return fmt.Errorf("the phone stayed %s", got)
		}
		return nil
	case driver.SwitchLocation:
		if value == "" {
			return d.c.call(ctx, d.c.hc, http.MethodDelete, "/wda/simulatedLocation", nil, nil)
		}
		lat, lon, err := driver.ParseLocation(value)
		if err != nil {
			return err
		}
		return d.c.call(ctx, d.c.hc, http.MethodPost, "/wda/simulatedLocation", map[string]any{"latitude": lat, "longitude": lon}, nil)
	}
	return fmt.Errorf("no switch %q: %w", name, driver.ErrUnsupported)
}

// orientation reads the device's orientation. Lying flat (face up or down)
// the device has none of its own, and the interface's tells which way the
// screen shows.
func (d *WDADevice) orientation(ctx context.Context) (string, error) {
	var o string
	if err := d.c.call(ctx, d.c.hc, http.MethodGet, "/wda/deviceOrientation", nil, &o); err != nil {
		return "", err
	}
	if r, ok := wdaOrientations[o]; ok {
		return r, nil
	}
	switch q := d.interfaceTurn(ctx); q {
	case 1:
		return driver.RotationReverseLandscape, nil
	case 2:
		return driver.RotationUpsideDown, nil
	case 3:
		return driver.RotationLandscape, nil
	}
	return driver.RotationPortrait, nil
}

// setOrientation turns the device. An app that only runs upright answers
// "Unable To Rotate Device", though the device has turned: what counts is
// the device's orientation afterwards. The app in front turns with it when
// it can.
func (d *WDADevice) setOrientation(ctx context.Context, value string) error {
	o, ok := wdaSetOrientations[value]
	if !ok {
		if value == driver.RotationAuto {
			return fmt.Errorf("an iPhone turns with how it is held unless its rotation lock is on, which only the person can change in Control Center; Pinky turns it to portrait, landscape, upside_down or reverse_landscape: %w", driver.ErrUnsupported)
		}
		return fmt.Errorf("no rotation %q: %w", value, driver.ErrUnsupported)
	}
	err := d.c.call(ctx, d.c.long, http.MethodPost, "/orientation", map[string]any{"orientation": o}, nil)
	d.uiAt.Store(0) // read the interface's turn again
	if got, rerr := d.orientation(ctx); rerr == nil {
		if got == value {
			return nil
		}
		if err == nil {
			return fmt.Errorf("the phone stayed %s", got)
		}
	}
	return err
}

type wdaLocation struct {
	Latitude  *float64 `json:"latitude"`
	Longitude *float64 `json:"longitude"`
}

// wdaDeviceInfo is part of GET /wda/device/info.
type wdaDeviceInfo struct {
	Style  string `json:"userInterfaceStyle"` // light, dark or unsupported
	Locale string `json:"currentLocale"`      // en_US@rg=sgzzzz
}

func (c *wdaClient) deviceInfo(ctx context.Context) (wdaDeviceInfo, error) {
	var i wdaDeviceInfo
	err := c.call(ctx, c.hc, http.MethodGet, "/wda/device/info", nil, &i)
	return i, err
}

// LaunchLocale implements driver.LocaleLauncher: the app starts again with
// the launch arguments -AppleLanguages and -AppleLocale, which outrank the
// phone's settings for that run alone. A running app ignores them, so it
// quits first. Nothing stays set: the app is in the phone's language again
// once it quits, which is how the returned func puts it back.
func (d *WDADevice) LaunchLocale(ctx context.Context, app string, loc driver.Locale) (func(context.Context) error, error) {
	id, err := d.resolveApp(ctx, app)
	if err != nil {
		return nil, err
	}
	phone := ""
	if loc.Language == "" {
		info, err := d.c.deviceInfo(ctx)
		if err != nil {
			return nil, err
		}
		phone = info.Locale
	}
	args, err := localeArguments(loc, phone)
	if err != nil {
		return nil, err
	}
	if err := d.c.do(ctx, d.c.long, http.MethodPost, "/wda/apps/terminate", map[string]any{"bundleId": id}, nil); err != nil {
		return nil, fmt.Errorf("quit %s: %w", id, err)
	}
	body := map[string]any{"bundleId": id, "arguments": args, "environment": map[string]any{}}
	if err := d.c.do(ctx, d.c.long, http.MethodPost, "/wda/apps/launch", body, nil); err != nil {
		return nil, fmt.Errorf("launch %s: %w", id, err)
	}
	return func(ctx context.Context) error {
		// 1 is "not running" (XCUIApplicationState): it opens in the
		// phone's language again by itself.
		var state int
		if err := d.c.do(ctx, d.c.hc, http.MethodPost, "/wda/apps/state", map[string]any{"bundleId": id}, &state); err != nil {
			return err
		}
		if state <= 1 {
			return nil
		}
		return d.c.do(ctx, d.c.long, http.MethodPost, "/wda/apps/terminate", map[string]any{"bundleId": id}, nil)
	}, nil
}

// localeArguments are the launch arguments that open an app in loc.
// phone is the phone's own locale, such as "en_US@rg=sgzzzz", whose
// language a region alone goes with.
func localeArguments(loc driver.Locale, phone string) ([]any, error) {
	var args []any
	lang := loc.Language
	if lang != "" {
		// AppleLanguages lists languages with their region, as Settings
		// writes them ("en-GB"): an app with a localization for the
		// region (pt-BR, en-GB) picks it.
		args = append(args, "-AppleLanguages", `("`+loc.Tag()+`")`)
	}
	if loc.Region != "" {
		if lang == "" {
			lang = phoneLanguage(phone)
			if lang == "" {
				return nil, fmt.Errorf("the phone did not say its language, which a region alone goes with; give a language too: %w", driver.ErrUnsupported)
			}
		}
		args = append(args, "-AppleLocale", strings.ReplaceAll(lang, "-", "_")+"_"+loc.Region)
	}
	return args, nil
}

// phoneLanguage is the language of a locale identifier such as
// "en_US@rg=sgzzzz" or "zh-Hans_CN": "en", "zh-Hans".
func phoneLanguage(locale string) string {
	locale, _, _ = strings.Cut(locale, "@")
	lang, _, _ := strings.Cut(locale, "_")
	return lang
}

// LaunchLocale implements driver.LocaleLauncher through CoreDevice's app
// service, whose launch takes arguments and quits a running app first
// (terminateExisting).
func (d *Device) LaunchLocale(ctx context.Context, app string, loc driver.Locale) (func(context.Context) error, error) {
	return nil, d.gone(d.launchLocale(ctx, app, loc))
}

func (d *Device) launchLocale(ctx context.Context, app string, loc driver.Locale) error {
	if strings.Contains(app, "://") {
		return fmt.Errorf("a link opens in its app, which keeps its language; open an app: %w", driver.ErrUnsupported)
	}
	id, err := d.resolveApp(ctx, app)
	if err != nil {
		return err
	}
	phone := ""
	if loc.Language == "" {
		if phone, err = lockdownLocale(ctx, d.l.base); err != nil {
			return fmt.Errorf("read the phone's language: %w", err)
		}
	}
	args, err := localeArguments(loc, phone)
	if err != nil {
		return err
	}
	d.markInput()
	input, err := launchInput(id, "")
	if err != nil {
		return err
	}
	opts := input["options"].(map[string]any)
	opts["arguments"] = args
	opts["terminateExisting"] = true
	svc, err := dialCoreDevice(d.l.entry, appService)
	if err != nil {
		return err
	}
	defer svc.Close()
	c, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if _, err := svc.invoke(c, featureLaunchApplication, "", input); err != nil {
		return fmt.Errorf("launch %s: %w", id, err)
	}
	return nil
}

// lockdownLocale reads the phone's locale ("en_US") from lockdown.
func lockdownLocale(ctx context.Context, e goios.DeviceEntry) (string, error) {
	return bounded(ctx, lockdownTimeout, func() (string, error) {
		conn, err := goios.ConnectLockdownWithSession(e)
		if err != nil {
			return "", err
		}
		defer conn.Close()
		v, err := conn.GetValueForDomain("Locale", "com.apple.international")
		if err != nil {
			return "", err
		}
		s, _ := v.(string)
		if s == "" {
			return "", errors.New("lockdown has no locale")
		}
		return s, nil
	})
}

// LaunchLocale implements driver.LocaleLauncher.
func (a *AXDevice) LaunchLocale(ctx context.Context, app string, loc driver.Locale) (func(context.Context) error, error) {
	return a.d.LaunchLocale(ctx, app, loc)
}
