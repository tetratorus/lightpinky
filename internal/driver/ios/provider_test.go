package ios

import (
	"context"
	"errors"
	"testing"

	goios "github.com/danielpaulus/go-ios/ios"
	"github.com/tetratorus/lightpinky/internal/driver"
)

func TestProviderTransportSelection(t *testing.T) {
	usb := goios.DeviceEntry{DeviceID: 1, Properties: goios.DeviceProperties{
		SerialNumber: "phone", ConnectionType: "USB",
	}}
	wifi := goios.DeviceEntry{DeviceID: 2, Properties: goios.DeviceProperties{
		SerialNumber: "phone", ConnectionType: "Network",
	}}
	for _, tc := range []struct {
		name     string
		wifiOnly bool
		entries  []goios.DeviceEntry
		want     int
	}{
		{"default prefers USB", false, []goios.DeviceEntry{usb, wifi}, 1},
		{"default reverse order", false, []goios.DeviceEntry{wifi, usb}, 1},
		{"Wi-Fi only with USB", true, []goios.DeviceEntry{usb, wifi}, 2},
		{"Wi-Fi only reverse order", true, []goios.DeviceEntry{wifi, usb}, 2},
		{"USB removed", true, []goios.DeviceEntry{wifi}, 2},
		{"Wi-Fi absent", true, []goios.DeviceEntry{usb}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := NewProvider()
			p.WiFiOnly = tc.wifiOnly
			p.SetWiFi(true)
			p.UseOverWiFi("phone")
			p.listFn = func() (goios.DeviceList, error) {
				return goios.DeviceList{DeviceList: tc.entries}, nil
			}
			listed := p.entries(tc.entries)
			entry, err := p.entryFor("phone")
			if tc.want == 0 {
				if len(listed) != 0 || err == nil {
					t.Fatalf("USB fallback: listed=%v, entry=%v, err=%v", listed, entry, err)
				}
				// Open must reject USB before trying developer services.
				if _, err := p.Open(context.Background(), "phone"); !errors.Is(err, driver.ErrNotFound) {
					t.Fatalf("Open without Wi-Fi = %v", err)
				}
				return
			}
			if len(listed) != 1 || listed[0].DeviceID != tc.want {
				t.Fatalf("listed=%v, want device %d", listed, tc.want)
			}
			if err != nil || entry.DeviceID != tc.want {
				t.Fatalf("entry=%v, err=%v, want device %d", entry, err, tc.want)
			}
		})
	}
}
