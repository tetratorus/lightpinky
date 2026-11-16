package ios

import (
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/tetratorus/lightpinky/internal/driver"
	"github.com/tetratorus/lightpinky/internal/screen"
)

const nativeModernFixture = `{
	"identifier":"coredevice-id",
	"properties":{
		"connection":{"pairingState":"paired","transportType":"localNetwork","state":"connected"},
		"hardware":{"udid":"phone-udid","platform":"iOS","reality":"physical","marketingName":"iPhone 16 Pro"},
		"software":{"osVersionNumber":{"stringValue":"27.2"}},
		"state":{"name":"My iPhone"}
	},
	"capabilities":[{"featureIdentifier":"com.apple.coredevice.feature.capturescreenshot"}]
}`

const nativeLegacyFixture = `{
	"identifier":"coredevice-id",
	"connectionProperties":{"pairingState":"paired","transportType":"localNetwork","tunnelState":"connected"},
	"hardwareProperties":{"udid":"phone-udid","platform":"iOS","reality":"physical","marketingName":"iPhone 16 Pro"},
	"deviceProperties":{"name":"My iPhone","osVersionNumber":"27.2"},
	"capabilities":[{"featureIdentifier":"com.apple.coredevice.feature.capturescreenshot"}]
}`

func nativeEnvelope(result string) []byte {
	return []byte(`{"info":{"outcome":"success","jsonVersion":5},"result":` + result + `}`)
}

func TestNativeListSchemas(t *testing.T) {
	for _, fixture := range []string{nativeModernFixture, nativeLegacyFixture} {
		p := NewNativeProvider()
		p.runFn = func(ctx context.Context, args ...string) ([]byte, error) {
			if got := strings.Join(args, " "); got != "list devices --json-output - --quiet --timeout 15" {
				t.Fatalf("command = %q", got)
			}
			if _, ok := ctx.Deadline(); !ok {
				t.Fatal("missing command deadline")
			}
			return nativeEnvelope(`{"devices":[` + fixture + `]}`), nil
		}
		infos, err := p.List(context.Background())
		if err != nil || len(infos) != 1 {
			t.Fatalf("List = %v, %v", infos, err)
		}
		info := infos[0]
		if info.ID != "phone-udid" || info.Name != "My iPhone" || info.Model != "iPhone 16 Pro" ||
			info.OSVersion != "27.2" || info.Connection != "wifi" || info.Backend != "coredevice-native" ||
			info.Platform != driver.IOS || info.State != driver.StateOnline || info.Virtual {
			t.Fatalf("info = %+v", info)
		}
	}
}

func TestNativeFiltersAndStates(t *testing.T) {
	for _, tc := range []struct {
		name, old, replacement string
		want                   bool
		state                  driver.State
	}{
		{"physical", "physical", "simulated", false, ""},
		{"platform", `"platform":"iOS"`, `"platform":"tvOS"`, false, ""},
		{"pairing", `"paired"`, `"unpaired"`, false, ""},
		{"usb", "localNetwork", "wired", false, ""},
		{"identity", `"udid":"phone-udid"`, `"udid":""`, false, ""},
		{"disconnected", `"state":"connected"`, `"state":"disconnected"`, true, driver.StateOffline},
		{"unsupported", "capturescreenshot", "connectdevice", true, driver.StateNeedsSetup},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var e nativeEntry
			if err := json.Unmarshal([]byte(strings.ReplaceAll(nativeModernFixture, tc.old, tc.replacement)), &e); err != nil {
				t.Fatal(err)
			}
			info, ok := e.info()
			if ok != tc.want || (ok && info.State != tc.state) {
				t.Fatalf("info = %+v, included = %v", info, ok)
			}
		})
	}
}

func TestNativeOpenVerifiesDetails(t *testing.T) {
	for _, tc := range []struct {
		name, details string
		want          error
	}{
		{"connected", nativeModernFixture, nil},
		{"offline", strings.ReplaceAll(nativeModernFixture, `"state":"connected"`, `"state":"disconnected"`), driver.ErrOffline},
		{"unsupported", strings.ReplaceAll(nativeModernFixture, "capturescreenshot", "connectdevice"), driver.ErrUnsupported},
		{"wrong device", strings.ReplaceAll(nativeModernFixture, "phone-udid", "another-phone"), driver.ErrNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := NewNativeProvider()
			calls := 0
			p.runFn = func(ctx context.Context, args ...string) ([]byte, error) {
				calls++
				if args[0] == "list" {
					return nativeEnvelope(`{"devices":[` + strings.ReplaceAll(nativeModernFixture, `"state":"connected"`, `"state":"disconnected"`) + `]}`), nil
				}
				if got := strings.Join(args, " "); got != "device info details --device phone-udid --json-output - --quiet --timeout 15" {
					t.Fatalf("command = %q", got)
				}
				return nativeEnvelope(tc.details), nil
			}
			d, err := p.Open(context.Background(), "phone-udid")
			if !errors.Is(err, tc.want) || calls != 2 {
				t.Fatalf("Open = %v, %v; calls = %d", d, err, calls)
			}
			if err == nil && d.Info().State != driver.StateOnline {
				t.Fatalf("info = %+v", d.Info())
			}
		})
	}
}

func TestNativeCommandRejectsInvalidResults(t *testing.T) {
	for _, output := range []string{
		`not JSON`, `{}`, `{"info":{"outcome":"success"}}`,
		`{"info":{"outcome":"failed"},"result":{},"error":{"message":"failure"}}`,
		`{"info":{"outcome":"success"},"result":null}`,
	} {
		p := &NativeProvider{runFn: func(context.Context, ...string) ([]byte, error) {
			return []byte(output), nil
		}}
		if _, err := p.List(context.Background()); err == nil {
			t.Fatalf("accepted %s", output)
		}
	}
}

func nativeTestPhone(run func(context.Context, ...string) ([]byte, error)) *nativePhone {
	return &nativePhone{
		provider: &NativeProvider{runFn: run},
		info:     driver.Info{ID: "phone-udid", Platform: driver.IOS, State: driver.StateOnline},
	}
}

func TestNativeScreenshotTemporaryFiles(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	d := nativeTestPhone(func(ctx context.Context, args ...string) ([]byte, error) {
		if strings.Join(args[:4], " ") != "device capture screenshot --destination" {
			t.Errorf("command = %v", args)
		}
		path := args[4]
		if filepath.Ext(path) != ".png" {
			t.Errorf("destination = %s", path)
		}
		mu.Lock()
		paths = append(paths, path)
		mu.Unlock()
		img := image.NewRGBA(image.Rect(0, 0, 3, 7))
		img.Set(1, 2, color.RGBA{255, 0, 0, 255})
		f, err := os.Create(path)
		if err != nil {
			return nil, err
		}
		err = png.Encode(f, img)
		f.Close()
		return nativeEnvelope(`{}`), err
	})
	var wg sync.WaitGroup
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			img, err := d.Screenshot(context.Background())
			if err != nil {
				t.Error(err)
				return
			}
			if img.Bounds().Dx() != 3 || img.Bounds().Dy() != 7 {
				t.Errorf("bounds = %v", img.Bounds())
			}
		}()
	}
	wg.Wait()
	seen := map[string]bool{}
	for _, path := range paths {
		if seen[path] {
			t.Errorf("destination reused: %s", path)
		}
		seen[path] = true
		if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
			t.Errorf("temporary screenshot directory survived: %s (%v)", path, err)
		}
	}
}

func TestNativeScreenshotFailuresCleanUp(t *testing.T) {
	for _, mode := range []string{"command", "invalid png", "missing file"} {
		t.Run(mode, func(t *testing.T) {
			path := ""
			d := nativeTestPhone(func(ctx context.Context, args ...string) ([]byte, error) {
				path = args[4]
				switch mode {
				case "command":
					return nil, errors.New("capture failed")
				case "invalid png":
					if err := os.WriteFile(path, []byte("not a PNG"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				return nativeEnvelope(`{}`), nil
			})
			if _, err := d.Screenshot(context.Background()); err == nil {
				t.Fatal("screenshot failure was ignored")
			}
			if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
				t.Fatalf("temporary directory survived: %v", err)
			}
		})
	}
}

const nativeAppsFixture = `{"apps":[
	{"bundleIdentifier":"com.example.notes","name":"Notes","removable":true,"defaultApp":true},
	{"bundleIdentifier":"com.apple.Preferences","name":"Settings","removable":false,"defaultApp":true},
	{"bundleIdentifier":"com.example.hidden","name":"Hidden","hidden":true},
	{"bundleIdentifier":"com.example.internal","name":"Internal","internalApp":true},
	{"bundleIdentifier":"com.example.clip","name":"Clip","appClip":true},
	{"bundleIdentifier":"","name":"No bundle"}
]}`

func TestNativeAppsAndLaunch(t *testing.T) {
	var launches []string
	d := nativeTestPhone(func(ctx context.Context, args ...string) ([]byte, error) {
		switch strings.Join(args[:3], " ") {
		case "device info apps":
			if args[3] != "--include-all-apps" {
				t.Fatal("only developer-installed apps would be listed")
			}
			return nativeEnvelope(nativeAppsFixture), nil
		case "device process launch":
			launches = append(launches, args[3])
			return nativeEnvelope(`{}`), nil
		}
		t.Fatalf("unexpected command: %v", args)
		return nil, nil
	})
	apps, err := d.Apps(context.Background())
	if err != nil || len(apps) != 2 || apps[0].ID != "com.example.notes" || apps[0].System || !apps[1].System {
		t.Fatalf("Apps = %+v, %v", apps, err)
	}
	for _, app := range []string{" settings ", "com.example.notes"} {
		if err := d.Launch(context.Background(), app); err != nil {
			t.Fatal(err)
		}
	}
	if got := strings.Join(launches, ","); got != "com.apple.Preferences,com.example.notes" {
		t.Fatalf("launched = %s", got)
	}
	for _, app := range []string{"", "not installed"} {
		if err := d.Launch(context.Background(), app); !errors.Is(err, driver.ErrNotFound) {
			t.Fatalf("Launch(%q) = %v", app, err)
		}
	}
}

func TestNativeObserveDoesNotCaptureOrInventForeground(t *testing.T) {
	d := nativeTestPhone(func(ctx context.Context, args ...string) ([]byte, error) {
		switch strings.Join(args[:3], " ") {
		case "device info lockState":
			return nativeEnvelope(`{"passcodeRequired":true,"unlockedSinceBoot":true}`), nil
		case "device info displays":
			return nativeEnvelope(`{"backlightState":"off","displays":[{"primary":true,"bounds":[[0,0],[1206,2622]]}]}`), nil
		}
		t.Fatalf("observation must not capture or launch: %v", args)
		return nil, nil
	})
	s, err := d.Observe(context.Background(), driver.ObserveOptions{
		OCR:        true,
		MayCapture: func(*screen.Screen) bool { return false },
		Frame: func(context.Context) (image.Image, error) {
			t.Fatal("observation must not read a frame")
			return nil, nil
		},
	})
	if err != nil || !s.Locked || s.Width != 1206 || s.Height != 2622 || s.Orientation != "portrait" ||
		s.App.ID != "" || s.App.Name != "" || len(s.Elements) != 0 {
		t.Fatalf("Observe = %+v, %v", s, err)
	}
	on, err := d.ScreenOn(context.Background())
	if err != nil || on {
		t.Fatalf("ScreenOn = %v, %v", on, err)
	}
}

func TestNativeMissingLockAndDisplayFailClosed(t *testing.T) {
	d := nativeTestPhone(func(context.Context, ...string) ([]byte, error) {
		return nativeEnvelope(`{}`), nil
	})
	if _, err := d.Locked(context.Background()); !errors.Is(err, driver.ErrUnsupported) {
		t.Fatalf("Locked = %v", err)
	}
	if _, err := d.ScreenOn(context.Background()); !errors.Is(err, driver.ErrUnsupported) {
		t.Fatalf("ScreenOn = %v", err)
	}
	if _, err := d.Observe(context.Background(), driver.ObserveOptions{}); err == nil {
		t.Fatal("missing state was reported as an unlocked screen")
	}
}

func TestNativeUnsupportedInputAndClose(t *testing.T) {
	d := nativeTestPhone(func(context.Context, ...string) ([]byte, error) {
		t.Fatal("unsupported or closed operation ran devicectl")
		return nil, nil
	})
	for _, err := range []error{
		d.Tap(context.Background(), screen.Point{}, driver.TapOptions{}),
		d.Swipe(context.Background(), screen.Point{}, screen.Point{}, 0),
		d.Type(context.Background(), "text", driver.TypeOptions{}),
		d.Key(context.Background(), driver.KeyHome),
		d.Wake(context.Background()),
	} {
		if !errors.Is(err, driver.ErrUnsupported) {
			t.Fatalf("input = %v", err)
		}
	}
	if _, ok := any(d).(driver.Unlocker); ok {
		t.Fatal("native provider must not advertise passcode input")
	}
	if _, err := d.Stream(context.Background(), driver.StreamOptions{}); !errors.Is(err, driver.ErrUnsupported) {
		t.Fatalf("Stream = %v", err)
	}
	d.Close()
	if _, err := d.Apps(context.Background()); !errors.Is(err, driver.ErrOffline) {
		t.Fatalf("Apps after Close = %v", err)
	}
}

func TestNativeCommandCancellationAndErrors(t *testing.T) {
	failure := errors.New("transport failed")
	p := &NativeProvider{runFn: func(context.Context, ...string) ([]byte, error) {
		return nil, failure
	}}
	if _, err := p.List(context.Background()); !errors.Is(err, failure) {
		t.Fatalf("List error lost: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.List(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("List cancellation lost: %v", err)
	}
}
