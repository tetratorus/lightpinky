package ios

import (
	"fmt"
	"strconv"
	"strings"
)

// osVersion is an iOS version, such as 18.6.2.
type osVersion struct{ Major, Minor, Patch int }

func parseVersion(s string) osVersion {
	var v osVersion
	parts := strings.SplitN(strings.TrimSpace(s), ".", 3)
	nums := []*int{&v.Major, &v.Minor, &v.Patch}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			break
		}
		*nums[i] = n
	}
	return v
}

func (v osVersion) atLeast(major, minor int) bool {
	return v.Major > major || (v.Major == major && v.Minor >= minor)
}

func (v osVersion) String() string {
	if v.Patch != 0 {
		return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
	}
	return fmt.Sprintf("%d.%d", v.Major, v.Minor)
}

// Which way Pinky drives a phone, by its iOS version.
//
//   - iOS 27 and later: CoreDevice's screen stream and HID input, the
//     protocol behind Xcode's Device Hub ("coredevice").
//   - iOS 17.4 to 26: the same tunnel and developer services, but the phone
//     refuses the screen stream and HID input (CoreDeviceError 9021; Xcode
//     27 shares only iOS 27 screens too). Pinky drives these through the
//     WebDriverAgent helper when it is installed ("wda"), and otherwise
//     through the accessibility inspector, with screenshots for the picture
//     ("accessibility").
//   - iOS 17.0 to 17.3: the tunnel needs remote pairing over the phone's USB
//     network interface, which needs administrator rights on the computer.
//   - iOS 16 and earlier: the developer services come from a developer disk
//     image for that exact version, which current Xcode no longer includes.
type pathKind int

const (
	pathUnsupported pathKind = iota
	pathCoreDevice
	pathLegacy
)

// Backend names in driver.Info.
const (
	backendCoreDevice    = "coredevice"
	backendWDA           = "wda"
	backendAccessibility = "accessibility"
)

// minLegacy is the first iOS with the lockdown tunnel (CoreDeviceProxy).
var minLegacy = osVersion{Major: 17, Minor: 4}

// pathFor says how Pinky can drive a phone on iOS v, and when it cannot,
// why, in a sentence for the person.
func pathFor(v osVersion) (pathKind, string) {
	switch {
	case v.Major >= minIOS:
		return pathCoreDevice, ""
	case v.atLeast(minLegacy.Major, minLegacy.Minor):
		return pathLegacy, ""
	case v.Major == 17:
		return pathUnsupported, fmt.Sprintf("Pinky needs iOS 17.4 or later; this iPhone runs iOS %s. Update it in Settings > General > Software Update.", v)
	case v.Major == 0:
		return pathUnsupported, "Pinky could not read this iPhone's iOS version."
	default:
		return pathUnsupported, fmt.Sprintf("Pinky needs iOS 17.4 or later; this iPhone runs iOS %s. Earlier versions need a developer disk image that current Xcode no longer includes.", v)
	}
}
