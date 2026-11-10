package driver

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// The switches a developer flips while testing an app, which agents set
// with phone_device (docs/notes/device-switches.md). Each changes a setting
// of the phone; the gateway asks the person where it should, never lets a
// read-only agent change one, and puts every switch back when the task
// ends.
const (
	// SwitchRotation is the screen's rotation: RotationPortrait and the
	// others, or RotationAuto when it follows how the phone is held.
	SwitchRotation = "rotation"
	// SwitchAppearance is light or dark: AppearanceLight, AppearanceDark,
	// or AppearanceAuto when it follows the time of day.
	SwitchAppearance = "appearance"
	// SwitchLocation is a simulated location every app on the phone sees,
	// as FormatLocation writes it, or "" when none is simulated.
	SwitchLocation = "location"
)

// Switches lists the switch names in the order they are set.
var Switches = []string{SwitchRotation, SwitchAppearance, SwitchLocation}

// Rotation values. Landscape is the phone turned a quarter counterclockwise,
// its top to the left (Android's ROTATION_90, iOS's LandscapeLeft).
const (
	RotationPortrait         = "portrait"
	RotationLandscape        = "landscape"
	RotationUpsideDown       = "upside_down"
	RotationReverseLandscape = "reverse_landscape"
	RotationAuto             = "auto"
)

// Appearance values. AppearanceAuto is only read (Android's automatic
// night mode); agents set light or dark.
const (
	AppearanceLight = "light"
	AppearanceDark  = "dark"
	AppearanceAuto  = "auto"
)

// Switcher is a Device whose switches agents may set. A switch the phone
// lacks returns an error wrapping ErrUnsupported that says why in a
// sentence, and what would work instead.
type Switcher interface {
	// Switch reads a switch's value.
	Switch(ctx context.Context, name string) (string, error)
	// SetSwitch sets a switch: to one of the values above, or to a value
	// Switch returned, which puts it back as it was. A location of ""
	// stops simulating one.
	SetSwitch(ctx context.Context, name, value string) error
}

// Locale is the language and region an app opens in.
type Locale struct {
	// Language is a BCP 47 language tag without a region, such as "ja",
	// "zh-Hans" or "pt"; "" keeps the phone's language.
	Language string
	// Region is a two-letter region code such as "JP"; "" keeps the
	// region the phone has.
	Region string
}

// Tag returns the locale as one BCP 47 tag, such as "ja-JP"; a part that
// is "" is left out.
func (l Locale) Tag() string {
	switch {
	case l.Language == "":
		return l.Region
	case l.Region == "":
		return l.Language
	}
	return l.Language + "-" + l.Region
}

// LocaleLauncher is a Device that opens an app in a language and region of
// its own, leaving the rest of the phone as it is (phone_launch with
// language or region).
type LocaleLauncher interface {
	// LaunchLocale opens an app in loc, starting it again if it runs:
	// apps read their language when they start. When the language stays
	// set after the app quits (Android's per-app languages), it returns
	// how to put the app back in the phone's language; the gateway calls
	// that when the task ends. It returns nil when nothing stays.
	LaunchLocale(ctx context.Context, app string, loc Locale) (restore func(context.Context) error, err error)
}

// ErrSwitchChanged is what putting back returns when the switch no longer
// has the value Pinky set: the person changed it, and it stays as they
// set it.
var ErrSwitchChanged = errors.New("changed since Pinky set it")

// FormatLocation writes a location as SwitchLocation's value:
// "35.6812,139.7671", latitude first.
func FormatLocation(lat, lon float64) string {
	return strconv.FormatFloat(lat, 'f', -1, 64) + "," + strconv.FormatFloat(lon, 'f', -1, 64)
}

// ParseLocation reads a SwitchLocation value.
func ParseLocation(s string) (lat, lon float64, err error) {
	a, b, ok := strings.Cut(s, ",")
	if !ok {
		return 0, 0, fmt.Errorf("location %q is not latitude,longitude", s)
	}
	if lat, err = strconv.ParseFloat(strings.TrimSpace(a), 64); err == nil {
		lon, err = strconv.ParseFloat(strings.TrimSpace(b), 64)
	}
	if err != nil || lat < -90 || lat > 90 || lon < -180 || lon > 180 {
		return 0, 0, fmt.Errorf("location %q is not latitude,longitude", s)
	}
	return lat, lon, nil
}
