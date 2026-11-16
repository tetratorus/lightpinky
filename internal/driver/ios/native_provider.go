package ios

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/tetratorus/lightpinky/internal/driver"
	"github.com/tetratorus/lightpinky/internal/screen"
)

type NativeProvider struct {
	runFn func(context.Context, ...string) ([]byte, error)
}

func NewNativeProvider() *NativeProvider { return &NativeProvider{} }

func (*NativeProvider) Platform() driver.Platform { return driver.IOS }

func (p *NativeProvider) command(ctx context.Context, result any, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	args = append(args, "--json-output", "-", "--quiet", "--timeout", "15")
	var data []byte
	var err error
	if p.runFn != nil {
		data, err = p.runFn(ctx, args...)
	} else {
		x, xerr := findXcode()
		if xerr != nil {
			return fmt.Errorf("CoreDevice requires Xcode on macOS: %w", driver.ErrUnsupported)
		}
		cmd := exec.CommandContext(ctx, "/usr/bin/xcrun", append([]string{"devicectl"}, args...)...)
		cmd.Env = x.env()
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		data, err = cmd.Output()
		if err != nil {
			err = fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return fmt.Errorf("devicectl %s: %w", strings.Join(args[:2], " "), err)
	}
	var envelope struct {
		Info struct {
			Outcome string `json:"outcome"`
		} `json:"info"`
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return fmt.Errorf("reading devicectl JSON: %w", err)
	}
	if envelope.Info.Outcome != "success" {
		return fmt.Errorf("devicectl outcome %q: %s", envelope.Info.Outcome, envelope.Error)
	}
	if len(envelope.Result) == 0 || string(envelope.Result) == "null" {
		return fmt.Errorf("devicectl returned no result")
	}
	if result != nil {
		if err := json.Unmarshal(envelope.Result, result); err != nil {
			return fmt.Errorf("reading devicectl result: %w", err)
		}
	}
	return nil
}

type nativeConnection struct {
	PairingState  string `json:"pairingState"`
	TransportType string `json:"transportType"`
	State         string `json:"state"`
	TunnelState   string `json:"tunnelState"`
}

type nativeHardware struct {
	UDID          string `json:"udid"`
	Platform      string `json:"platform"`
	Reality       string `json:"reality"`
	MarketingName string `json:"marketingName"`
	ProductType   string `json:"productType"`
}

type nativeEntry struct {
	Identifier string `json:"identifier"`
	Properties struct {
		Connection *nativeConnection `json:"connection"`
		Hardware   *nativeHardware   `json:"hardware"`
		State      *struct {
			Name string `json:"name"`
		} `json:"state"`
		Software *struct {
			OSVersionNumber struct {
				StringValue string `json:"stringValue"`
			} `json:"osVersionNumber"`
		} `json:"software"`
	} `json:"properties"`
	ConnectionProperties nativeConnection `json:"connectionProperties"`
	HardwareProperties   nativeHardware   `json:"hardwareProperties"`
	DeviceProperties     struct {
		Name            string `json:"name"`
		OSVersionNumber string `json:"osVersionNumber"`
	} `json:"deviceProperties"`
	Capabilities []struct {
		FeatureIdentifier string `json:"featureIdentifier"`
	} `json:"capabilities"`
}

func (e nativeEntry) hasFeature(name string) bool {
	for _, c := range e.Capabilities {
		if c.FeatureIdentifier == "com.apple.coredevice.feature."+name {
			return true
		}
	}
	return false
}

func (e nativeEntry) info() (driver.Info, bool) {
	c, h := e.ConnectionProperties, e.HardwareProperties
	name, version := e.DeviceProperties.Name, e.DeviceProperties.OSVersionNumber
	if e.Properties.Connection != nil {
		c = *e.Properties.Connection
	}
	if e.Properties.Hardware != nil {
		h = *e.Properties.Hardware
	}
	if e.Properties.State != nil {
		name = e.Properties.State.Name
	}
	if e.Properties.Software != nil {
		version = e.Properties.Software.OSVersionNumber.StringValue
	}
	if h.UDID == "" || h.Platform != "iOS" || h.Reality != "physical" ||
		c.PairingState != "paired" || c.TransportType != "localNetwork" {
		return driver.Info{}, false
	}
	info := driver.Info{
		ID: h.UDID, Platform: driver.IOS, Name: name, Model: h.MarketingName,
		OSVersion: version, Connection: "wifi", Backend: "coredevice-native",
		State:  driver.StateOffline,
		Detail: "CoreDevice tunnel is not connected; opening the phone will try to connect.",
	}
	if info.Model == "" {
		info.Model = h.ProductType
	}
	if c.State == "connected" || (c.State == "" && c.TunnelState == "connected") {
		info.State, info.Detail = driver.StateOnline, ""
		info.WiFi.Seen = true
		if !e.hasFeature("capturescreenshot") {
			info.State = driver.StateNeedsSetup
			info.Detail = "CoreDevice does not offer native screenshots on this phone."
		}
	}
	return info, true
}

func (p *NativeProvider) List(ctx context.Context) ([]driver.Info, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if p.runFn == nil {
		x, err := findXcode()
		if err != nil {
			return nil, nil
		}
		if st, err := os.Stat(x.tool("devicectl")); err != nil || st.IsDir() {
			return nil, nil
		}
	}
	var result struct {
		Devices []nativeEntry `json:"devices"`
	}
	if err := p.command(ctx, &result, "list", "devices"); err != nil {
		return nil, err
	}
	var infos []driver.Info
	for _, e := range result.Devices {
		if info, ok := e.info(); ok {
			infos = append(infos, info)
		}
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].ID < infos[j].ID })
	return infos, nil
}

func (p *NativeProvider) Open(ctx context.Context, id string) (driver.Device, error) {
	if p.runFn == nil {
		if _, err := findXcode(); err != nil {
			return nil, fmt.Errorf("CoreDevice requires Xcode on macOS: %w", driver.ErrUnsupported)
		}
	}
	infos, err := p.List(ctx)
	if err != nil {
		return nil, err
	}
	found := false
	for _, info := range infos {
		if info.ID == id {
			found = true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("CoreDevice phone %q: %w", id, driver.ErrNotFound)
	}
	var e nativeEntry
	if err := p.command(ctx, &e, "device", "info", "details", "--device", id); err != nil {
		return nil, err
	}
	info, ok := e.info()
	if !ok || info.ID != id {
		return nil, fmt.Errorf("CoreDevice details do not match paired physical Wi-Fi phone %q: %w", id, driver.ErrNotFound)
	}
	if info.State == driver.StateOffline {
		return nil, fmt.Errorf("CoreDevice tunnel did not connect: %w", driver.ErrOffline)
	}
	if !e.hasFeature("capturescreenshot") {
		return nil, fmt.Errorf("CoreDevice native screenshots are unavailable: %w", driver.ErrUnsupported)
	}
	return &nativePhone{provider: p, info: info}, nil
}

type nativePhone struct {
	provider *NativeProvider
	info     driver.Info
	closed   atomic.Bool
}

func (d *nativePhone) Info() driver.Info { return d.info }

func (d *nativePhone) command(ctx context.Context, result any, args ...string) error {
	if d.closed.Load() {
		return driver.ErrOffline
	}
	args = append(args, "--device", d.info.ID)
	return d.provider.command(ctx, result, args...)
}

func (d *nativePhone) Close() error {
	d.closed.Store(true)
	return nil
}

func (*nativePhone) Tap(context.Context, screen.Point, driver.TapOptions) error {
	return driver.ErrUnsupported
}

func (*nativePhone) Swipe(context.Context, screen.Point, screen.Point, time.Duration) error {
	return driver.ErrUnsupported
}

func (*nativePhone) Type(context.Context, string, driver.TypeOptions) error {
	return driver.ErrUnsupported
}

func (*nativePhone) Key(context.Context, driver.Key) error { return driver.ErrUnsupported }

func (*nativePhone) Stream(context.Context, driver.StreamOptions) (driver.Stream, error) {
	return nil, driver.ErrUnsupported
}

var _ driver.Provider = (*NativeProvider)(nil)
var _ driver.Device = (*nativePhone)(nil)
