// Command lightpinky is an MCP server that exposes an iPhone's raw
// developer services (iOS 27+, CoreDevice) to an agent: screenshots, the
// accessibility element tree, touch, keys, typing and apps.
package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"image/jpeg"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tetratorus/lightpinky/internal/driver"
	"github.com/tetratorus/lightpinky/internal/driver/ios"
	"github.com/tetratorus/lightpinky/internal/screen"
)

func main() {
	addr := flag.String("http", "", "serve MCP over HTTP on this address (needs LIGHTPINKY_TOKEN); default is stdio")
	wifi := flag.Bool("wifi", true, "keep using phones over Wi-Fi after they were seen on the cable")
	dir := flag.String("ddi", defaultDDIDir(), "folder for the developer disk image")
	sock := flag.String("socket", "", "serve MCP over HTTP on this Unix socket (stateless, JSON responses), for clients that come and go such as scripts/phone")
	idle := flag.Duration("idle", 0, "with -socket, exit after this long without a request (0: never)")
	sims := flag.Bool("simulators", runtime.GOOS == "darwin", "also drive booted iOS Simulators (through WebDriverAgent, built once with Xcode)")
	flag.Parse()

	opts := &slog.HandlerOptions{}
	if os.Getenv("LIGHTPINKY_DEBUG") != "" {
		opts.Level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, opts))
	p := ios.NewProvider()
	p.Log = log
	p.DiskImageDir = *dir
	p.WiFiMux = *wifi
	p.PhonesFile = phonesFile()
	p.UseSavedPhones()
	p.SetWiFi(*wifi)
	all := &allPhones{phone: p}
	if *sims {
		sp := ios.NewSimProvider(simulatorDir())
		sp.Log = log
		all.sims = sp
	}
	ph := &phones{p: all, wifi: *wifi, open: map[string]driver.Device{}}
	defer ph.closeAll()

	srv := sdk.NewServer(&sdk.Implementation{Name: "lightpinky", Version: "0.1.0"}, nil)
	ph.register(srv)

	if *sock != "" {
		if err := serveSocket(srv, *sock, *idle, log, ph.closeAll); err != nil {
			log.Error("socket", "err", err)
			ph.closeAll()
			os.Exit(1)
		}
		return
	}
	if *addr == "" {
		if err := srv.Run(context.Background(), &sdk.StdioTransport{}); err != nil {
			log.Error("stdio", "err", err)
			os.Exit(1)
		}
		return
	}
	token := os.Getenv("LIGHTPINKY_TOKEN")
	if token == "" {
		fmt.Fprintln(os.Stderr, "lightpinky: -http needs LIGHTPINKY_TOKEN")
		os.Exit(2)
	}
	h := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return srv }, nil)
	log.Info("serving MCP over HTTP", "addr", *addr, "path", "/mcp")
	mux := http.NewServeMux()
	mux.Handle("/mcp", bearer(token, h))
	if err := http.ListenAndServe(*addr, mux); err != nil {
		log.Error("http", "err", err)
		os.Exit(1)
	}
}

func defaultDDIDir() string {
	d, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(d, "lightpinky", "ddi")
}

func phonesFile() string {
	d, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(d, "lightpinky", "phones.json")
}

func bearer(token string, next http.Handler) http.Handler {
	want := []byte("Bearer " + token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), want) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

type phones struct {
	p    phoneProvider
	wifi bool

	mu         sync.Mutex
	open       map[string]driver.Device
	unlockMu   sync.Mutex
	openLocked func(context.Context, driver.Info) (driver.Device, error)
}

func (ph *phones) list(ctx context.Context) ([]driver.Info, error) {
	return ph.discover(ctx)
}

// device returns the open phone with this id, or the only online phone
// when id is empty.
func (ph *phones) device(ctx context.Context, id string) (driver.Device, error) {
	return ph.openDevice(ctx, id, false)
}

// drop closes a phone whose call failed, so the next call opens it again.
func (ph *phones) drop(d driver.Device) {
	ph.mu.Lock()
	defer ph.mu.Unlock()
	for id, o := range ph.open {
		if o == d {
			delete(ph.open, id)
		}
	}
	d.Close()
}

func (ph *phones) closeAll() {
	ph.mu.Lock()
	defer ph.mu.Unlock()
	for id, d := range ph.open {
		d.Close()
		delete(ph.open, id)
	}
}

// with runs f on the phone, reopening it once if the phone's link went away.
func (ph *phones) with(ctx context.Context, id string, f func(driver.Device) error) error {
	for try := 0; ; try++ {
		d, err := ph.device(ctx, id)
		if err != nil {
			return err
		}
		err = f(d)
		if err == nil || !errors.Is(err, driver.ErrOffline) || try > 0 {
			return err
		}
		ph.drop(d)
	}
}

type devIn struct {
	Device string `json:"device,omitempty" jsonschema:"UDID; may be left out when one iPhone is online"`
}

// actIn is a device tool that changes the screen. It waits for the screen
// to settle by default, so the next read sees where the action led.
type actIn struct {
	devIn
	NoSettle bool `json:"no_settle,omitempty" jsonschema:"return at once instead of waiting (up to 2 s) for the screen to stop changing"`
}

type shotIn struct {
	devIn
	MaxSize int `json:"max_size,omitempty" jsonschema:"shrink the image by a whole factor so its longer side is at most this many pixels; the reply gives the factor to multiply image coordinates by for tap and swipe"`
}

type elementsIn struct {
	devIn
	JSON bool `json:"json,omitempty" jsonschema:"the full observation as JSON instead of one line per element"`
}

type tapIn struct {
	actIn
	X         int  `json:"x" jsonschema:"x in screenshot pixels"`
	Y         int  `json:"y" jsonschema:"y in screenshot pixels"`
	LongPress bool `json:"long_press,omitempty"`
}

type swipeIn struct {
	actIn
	X1         int `json:"x1"`
	Y1         int `json:"y1"`
	X2         int `json:"x2"`
	Y2         int `json:"y2"`
	DurationMS int `json:"duration_ms,omitempty" jsonschema:"default 300"`
}

type typeIn struct {
	actIn
	Text   string `json:"text"`
	Append bool   `json:"append,omitempty" jsonschema:"keep the field's current text"`
	Submit bool   `json:"submit,omitempty" jsonschema:"press return afterwards"`
}

type keyIn struct {
	actIn
	Key string `json:"key" jsonschema:"home; back (the screen's back button, else an edge swipe); enter, tab, delete, forward_delete, escape; dpad_up, dpad_down, dpad_left, dpad_right, page_up, page_down (iOS 27 phones)"`
}

type linkIn struct {
	actIn
	URL string `json:"url" jsonschema:"web link or app URL scheme"`
}

type launchIn struct {
	actIn
	App string `json:"app" jsonschema:"bundle id or app name"`
}

func unsupported(d driver.Device) error {
	return fmt.Errorf("%s is driven through %s: %w", d.Info().Name, d.Info().Backend, driver.ErrUnsupported)
}

func text(v any) *sdk.CallToolResult {
	var s string
	if str, ok := v.(string); ok {
		s = str
	} else {
		b, err := json.MarshalIndent(v, "", " ")
		if err != nil {
			return fail(err)
		}
		s = string(b)
	}
	return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: s}}}
}

func fail(err error) *sdk.CallToolResult {
	return &sdk.CallToolResult{IsError: true, Content: []sdk.Content{&sdk.TextContent{Text: err.Error()}}}
}

func done(err error) (*sdk.CallToolResult, any, error) {
	if err != nil {
		return fail(err), nil, nil
	}
	return text("ok"), nil, nil
}

func (ph *phones) register(srv *sdk.Server) {
	sdk.AddTool(srv, &sdk.Tool{Name: "unlock", Description: "Wake and unlock the iPhone, including over Wi-Fi, using IPHONE_UNLOCK_CODE from the server environment. No passcode argument. Makes one passcode attempt; never retries input."},
		func(ctx context.Context, _ *sdk.CallToolRequest, in devIn) (*sdk.CallToolResult, any, error) {
			return done(ph.unlock(ctx, in.Device))
		})

	sdk.AddTool(srv, &sdk.Tool{Name: "devices", Description: "List iPhones this computer sees, with their state and, when not online, what they wait for."},
		func(ctx context.Context, _ *sdk.CallToolRequest, _ struct{}) (*sdk.CallToolResult, any, error) {
			infos, err := ph.list(ctx)
			if err != nil {
				return fail(err), nil, nil
			}
			return text(infos), nil, nil
		})

	sdk.AddTool(srv, &sdk.Tool{Name: "download_disk_image", Description: "Download Apple's developer disk image (pinned, checksummed mirror) for phones whose state says they need one. Xcode installs the same image."},
		func(ctx context.Context, _ *sdk.CallToolRequest, _ struct{}) (*sdk.CallToolResult, any, error) {
			return done(ph.p.DownloadDiskImage(ctx, nil))
		})

	sdk.AddTool(srv, &sdk.Tool{Name: "screenshot", Description: "Screenshot the phone as JPEG. Coordinates for tap and swipe are in this image's pixels (times the factor the reply gives with max_size)."},
		func(ctx context.Context, _ *sdk.CallToolRequest, in shotIn) (*sdk.CallToolResult, any, error) {
			var buf bytes.Buffer
			var note string
			err := ph.with(ctx, in.Device, func(d driver.Device) error {
				img, err := d.Screenshot(ctx)
				if err != nil {
					return err
				}
				b := img.Bounds()
				if k := shrinkFactor(b.Dx(), b.Dy(), in.MaxSize); k > 1 {
					img = shrink(img, k)
					s := img.Bounds()
					note = fmt.Sprintf("image %dx%d of a %dx%d screen: multiply image coordinates by %d for tap and swipe", s.Dx(), s.Dy(), b.Dx(), b.Dy(), k)
				}
				buf.Reset()
				return jpeg.Encode(&buf, img, &jpeg.Options{Quality: 80})
			})
			if err != nil {
				return fail(err), nil, nil
			}
			res := &sdk.CallToolResult{Content: []sdk.Content{&sdk.ImageContent{Data: buf.Bytes(), MIMEType: "image/jpeg"}}}
			if note != "" {
				res.Content = append(res.Content, &sdk.TextContent{Text: note})
			}
			return res, nil, nil
		})

	sdk.AddTool(srv, &sdk.Tool{Name: "elements", Description: "Read the screen through the accessibility tree. The first line is the screen's pixel size, the app in front, orientation, keyboard and lock state; then one line per element: role, \"label\", id=\"identifier\" when it has one, @x,y its center and [x,y wxh] its bounds in screenshot pixels, then states. json: true gives the full observation."},
		func(ctx context.Context, _ *sdk.CallToolRequest, in elementsIn) (*sdk.CallToolResult, any, error) {
			var s *screen.Screen
			err := ph.with(ctx, in.Device, func(d driver.Device) (err error) {
				s, err = d.Observe(ctx, driver.ObserveOptions{})
				return err
			})
			if err != nil {
				return fail(err), nil, nil
			}
			if in.JSON {
				return text(s), nil, nil
			}
			return text(renderScreen(s)), nil, nil
		})

	sdk.AddTool(srv, &sdk.Tool{Name: "tap", Description: "Tap, or long-press, a point in screenshot pixels."},
		func(ctx context.Context, _ *sdk.CallToolRequest, in tapIn) (*sdk.CallToolResult, any, error) {
			return ph.act(ctx, in.actIn, func(d driver.Device) error {
				return d.Tap(ctx, screen.Point{X: in.X, Y: in.Y}, driver.TapOptions{LongPress: in.LongPress})
			})
		})

	sdk.AddTool(srv, &sdk.Tool{Name: "swipe", Description: "Swipe from one point to another in screenshot pixels. To scroll a list down, swipe up over it."},
		func(ctx context.Context, _ *sdk.CallToolRequest, in swipeIn) (*sdk.CallToolResult, any, error) {
			dur := time.Duration(in.DurationMS) * time.Millisecond
			if dur <= 0 {
				dur = 300 * time.Millisecond
			}
			return ph.act(ctx, in.actIn, func(d driver.Device) error {
				return d.Swipe(ctx, screen.Point{X: in.X1, Y: in.Y1}, screen.Point{X: in.X2, Y: in.Y2}, dur)
			})
		})

	sdk.AddTool(srv, &sdk.Tool{Name: "type", Description: "Type text into the focused field (through the pasteboard, so any language works). Replaces the field's text unless append is set."},
		func(ctx context.Context, _ *sdk.CallToolRequest, in typeIn) (*sdk.CallToolResult, any, error) {
			return ph.act(ctx, in.actIn, func(d driver.Device) error {
				return d.Type(ctx, in.Text, driver.TypeOptions{Append: in.Append, Submit: in.Submit})
			})
		})

	sdk.AddTool(srv, &sdk.Tool{Name: "key", Description: "Press a hardware or keyboard key."},
		func(ctx context.Context, _ *sdk.CallToolRequest, in keyIn) (*sdk.CallToolResult, any, error) {
			return ph.act(ctx, in.actIn, func(d driver.Device) error {
				return d.Key(ctx, driver.Key(in.Key))
			})
		})

	sdk.AddTool(srv, &sdk.Tool{Name: "launch", Description: "Open an app by bundle id or name, bringing it to the front."},
		func(ctx context.Context, _ *sdk.CallToolRequest, in launchIn) (*sdk.CallToolResult, any, error) {
			return ph.act(ctx, in.actIn, func(d driver.Device) error {
				return d.Launch(ctx, in.App)
			})
		})

	sdk.AddTool(srv, &sdk.Tool{Name: "apps", Description: "List installed apps with bundle ids."},
		func(ctx context.Context, _ *sdk.CallToolRequest, in devIn) (*sdk.CallToolResult, any, error) {
			var apps []driver.App
			err := ph.with(ctx, in.Device, func(d driver.Device) (err error) {
				apps, err = d.Apps(ctx)
				return err
			})
			if err != nil {
				return fail(err), nil, nil
			}
			return text(apps), nil, nil
		})

	sdk.AddTool(srv, &sdk.Tool{Name: "stop_app", Description: "Quit an app by bundle id."},
		func(ctx context.Context, _ *sdk.CallToolRequest, in launchIn) (*sdk.CallToolResult, any, error) {
			return ph.act(ctx, in.actIn, func(d driver.Device) error {
				st, ok := d.(interface {
					StopApp(context.Context, string) error
				})
				if !ok {
					return unsupported(d)
				}
				return st.StopApp(ctx, in.App)
			})
		})

	sdk.AddTool(srv, &sdk.Tool{Name: "open_link", Description: "Open a URL in the app iOS would open it in."},
		func(ctx context.Context, _ *sdk.CallToolRequest, in linkIn) (*sdk.CallToolResult, any, error) {
			return ph.act(ctx, in.actIn, func(d driver.Device) error {
				lo, ok := d.(driver.LinkOpener)
				if !ok {
					return unsupported(d)
				}
				app, err := lo.LinkApp(ctx, in.URL)
				if err != nil {
					return err
				}
				return lo.OpenLink(ctx, in.URL, app)
			})
		})

	sdk.AddTool(srv, &sdk.Tool{Name: "power", Description: "Report whether the phone is locked and its screen on; wake: true lights the screen first."},
		func(ctx context.Context, _ *sdk.CallToolRequest, in struct {
			devIn
			Wake bool `json:"wake,omitempty"`
		}) (*sdk.CallToolResult, any, error) {
			var st struct{ Locked, ScreenOn bool }
			d, err := ph.openDevice(ctx, in.Device, in.Wake)
			if err != nil {
				return fail(err), nil, nil
			}
			err = func() (err error) {
				dev, isPhone := d.(*ios.Device)
				if in.Wake {
					if !isPhone {
						return unsupported(d)
					}
					if err = dev.Wake(ctx); err != nil {
						return err
					}
				}
				lr, ok := d.(driver.LockReporter)
				if !ok {
					return unsupported(d)
				}
				if st.Locked, err = lr.Locked(ctx); err != nil {
					return err
				}
				if !isPhone {
					st.ScreenOn = true // a Simulator's screen is always on
					return nil
				}
				st.ScreenOn, err = dev.ScreenOn(ctx)
				return err
			}()
			if err != nil {
				return fail(err), nil, nil
			}
			return text(st), nil, nil
		})
}
