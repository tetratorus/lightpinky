package ios

import (
	"fmt"
	"os"
	"runtime"

	"github.com/tetratorus/lightpinky/internal/driver"
	goios "github.com/danielpaulus/go-ios/ios"
)

// What the computer needs for iPhones: usbmuxd, which every connection to a
// phone goes through. macOS has it built in. On Windows it comes with
// Apple's iPhone driver, from the Apple Devices app (Microsoft Store) or
// iTunes; on Linux from the usbmuxd package, whose daemon udev starts while
// an iPhone is plugged in.

// listUsbmuxd asks usbmuxd for its phones. ok is false when it does not
// answer: then there are no phones (err nil), unless it answered with
// phones a moment ago, when the first two failures in a row are errors, so
// that a hiccup does not drop the phones from the gateway.
func (p *Provider) listUsbmuxd() (list []goios.DeviceEntry, ok bool, err error) {
	listDevices := p.listFn
	if listDevices == nil {
		listDevices = goios.ListDevices
	}
	l, err := listDevices()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.muxErr, p.muxChecked = err, true
	if err == nil {
		p.muxFails, p.muxPhones = 0, len(l.DeviceList)
		return l.DeviceList, true, nil
	}
	p.muxFails++
	if p.muxPhones > 0 && p.muxFails < 3 {
		return nil, false, fmt.Errorf("usbmuxd: %w", err)
	}
	return nil, false, nil
}

// CheckHost implements driver.HostChecker, from what the last List found
// (List runs every few seconds in the gateway).
func (p *Provider) CheckHost() driver.HostCheck {
	p.mu.Lock()
	err, checked := p.muxErr, p.muxChecked
	p.mu.Unlock()
	if !checked {
		_, err = goios.ListDevices()
	}
	if err == nil {
		return driver.HostCheck{}
	}
	return hostCheck(runtime.GOOS, usbmuxdInstalled(), err)
}

// hostCheck says what is missing on goos when usbmuxd does not answer.
func hostCheck(goos string, installed bool, err error) driver.HostCheck {
	switch goos {
	case "windows":
		return driver.HostCheck{Missing: "apple_devices", Detail: "iPhones need Apple's iPhone driver: install Apple Devices from the Microsoft Store (or iTunes from apple.com), then plug the iPhone in again."}
	case "linux":
		if installed {
			return driver.HostCheck{Missing: "usbmuxd_stopped", Detail: "usbmuxd is installed but not running. It starts when an iPhone is plugged in; if it does not, run sudo systemctl start usbmuxd."}
		}
		return driver.HostCheck{Missing: "usbmuxd", Detail: "iPhones need usbmuxd: install it with your package manager (for example sudo apt install usbmuxd), then plug the iPhone in again."}
	}
	return driver.HostCheck{Missing: "usbmuxd", Detail: "usbmuxd does not answer: " + err.Error()}
}

// usbmuxdInstalled reports whether a usbmuxd program is on this computer
// (Linux), in the places packages put it.
func usbmuxdInstalled() bool {
	for _, p := range []string{"/usr/sbin/usbmuxd", "/usr/bin/usbmuxd", "/usr/local/sbin/usbmuxd", "/usr/local/bin/usbmuxd", "/sbin/usbmuxd"} {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return true
		}
	}
	return false
}

var _ driver.HostChecker = (*Provider)(nil)
