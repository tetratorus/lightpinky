package ios

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"unicode"

	"github.com/tetratorus/lightpinky/internal/driver"
)

// Links: phone_launch opens web links and apps' own links ("myapp://…").
// The gateway judges the app a link opens in before anything opens, so
// LinkApp names that app, and OpenLink opens the link in it by bundle id
// and nowhere else.
//
// iOS opens a custom scheme in the app that declares it (CFBundleURLTypes,
// which the installation proxy lists). When several do, Apple's own app
// wins over the others; among the others, which one iOS picks is not
// defined, so Pinky refuses rather than guess. Web links open in Safari,
// by name, whatever browser the person made their default: that is the
// app the rules judged.

// safariID is the app an http or https link opens in.
const safariID = "com.apple.mobilesafari"

var safari = driver.App{ID: safariID, Name: "Safari", System: true}

// linkScheme returns a link's scheme in lowercase, refusing what no link
// holds: no scheme, spaces or control characters. A web link needs a host.
func linkScheme(link string) (string, error) {
	link = strings.TrimSpace(link)
	bad := func() (string, error) {
		return "", fmt.Errorf("open link %q: not a link: %w", link, driver.ErrUnsupported)
	}
	if link == "" || strings.ContainsFunc(link, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
		return bad()
	}
	u, err := url.Parse(link)
	if err != nil || u.Scheme == "" {
		return bad()
	}
	scheme := strings.ToLower(u.Scheme)
	if (scheme == "http" || scheme == "https") && u.Host == "" {
		return bad()
	}
	return scheme, nil
}

// webScheme reports whether Safari opens a scheme's links.
func webScheme(scheme string) bool { return scheme == "http" || scheme == "https" }

// pickSchemeApp returns the app iOS opens a link in, from the apps that
// declare its scheme.
func pickSchemeApp(link string, apps []driver.App) (driver.App, error) {
	var apple []driver.App
	for _, a := range apps {
		if a.System {
			apple = append(apple, a)
		}
	}
	if len(apple) > 0 {
		apps = apple // Apple's own app opens the schemes it declares
	}
	switch len(apps) {
	case 0:
		return driver.App{}, fmt.Errorf("no app opens %s: %w", link, driver.ErrNotFound)
	case 1:
		return apps[0], nil
	}
	return driver.App{}, &driver.LinkChoiceError{Link: link, Apps: apps}
}

// LinkApp implements driver.LinkOpener: Safari for a web link, else the app
// that declares the link's scheme.
func (d *Device) LinkApp(ctx context.Context, link string) (driver.App, error) {
	app, err := d.linkApp(ctx, link)
	return app, d.gone(err)
}

func (d *Device) linkApp(ctx context.Context, link string) (driver.App, error) {
	scheme, err := linkScheme(link)
	if err != nil || webScheme(scheme) {
		return safari, err
	}
	c, err := d.catalog(ctx)
	if err != nil {
		return driver.App{}, err
	}
	app, err := pickSchemeApp(link, c.schemeApps(scheme))
	if errors.Is(err, driver.ErrNotFound) {
		// An app installed since the list was read.
		if c, err = d.catalogFresh(ctx); err != nil {
			return driver.App{}, err
		}
		app, err = pickSchemeApp(link, c.schemeApps(scheme))
	}
	return app, err
}

// OpenLink implements driver.LinkOpener: the app comes up with the link,
// through payloadURL in its launch request (as devicectl's --payload-url).
// A running app comes to the front without restarting.
func (d *Device) OpenLink(ctx context.Context, link string, app driver.App) error {
	return d.gone(d.openLink(ctx, link, app))
}

func (d *Device) openLink(ctx context.Context, link string, app driver.App) error {
	want, err := d.linkApp(ctx, link)
	if err != nil {
		return err
	}
	if want.ID != app.ID {
		return fmt.Errorf("open %s in %s: the app does not open this link: %w", link, app.ID, driver.ErrNotFound)
	}
	return d.startApp(ctx, app.ID, strings.TrimSpace(link))
}

// wdaOpenURL is the first WebDriverAgent whose /url opens a link in the app
// it names (bundleId, appium/WebDriverAgent 4.11.0, Xcode 14.3 and later).
// Earlier ones ignore the app and open the link wherever iOS would, which
// the rules did not judge, so Pinky does not open links through them.
var wdaOpenURL = osVersion{Major: 4, Minor: 11}

// LinkApp implements driver.LinkOpener, as on the CoreDevice path.
func (d *WDADevice) LinkApp(ctx context.Context, link string) (driver.App, error) {
	scheme, err := linkScheme(link)
	if err != nil || webScheme(scheme) {
		return safari, err
	}
	apps, err := d.schemeApps(ctx, scheme)
	if err != nil {
		return driver.App{}, err
	}
	return pickSchemeApp(link, apps)
}

// schemeApps returns the apps that declare a URL scheme, from the app list
// or, on a simulator, from SetLinkSchemes.
func (d *WDADevice) schemeApps(ctx context.Context, scheme string) ([]driver.App, error) {
	d.appMu.Lock()
	fn := d.schemesFn
	d.appMu.Unlock()
	if fn != nil {
		m, err := fn(ctx)
		if err != nil {
			return nil, err
		}
		return m[scheme], nil
	}
	if d.entry.DeviceID == 0 {
		return nil, fmt.Errorf("finding the app for a %s: link: no app list: %w", scheme, driver.ErrUnsupported)
	}
	if _, err := d.Apps(ctx); err != nil {
		return nil, err
	}
	d.appMu.Lock()
	c := d.apps
	d.appMu.Unlock()
	return c.schemeApps(scheme), nil
}

// SetLinkSchemes gives a simulator, which has no installation proxy, the
// apps that declare each URL scheme (lowercase): LinkApp and OpenLink find
// the app a link opens in there. Without it a simulator opens web links
// only.
func (d *WDADevice) SetLinkSchemes(fn func(context.Context) (map[string][]driver.App, error)) {
	d.appMu.Lock()
	d.schemesFn = fn
	d.appMu.Unlock()
}

// OpenLink implements driver.LinkOpener: WebDriverAgent opens the link in
// the app by bundle id (XCTest's openURL:usingApplication:).
func (d *WDADevice) OpenLink(ctx context.Context, link string, app driver.App) error {
	want, err := d.LinkApp(ctx, link)
	if err != nil {
		return err
	}
	if want.ID != app.ID {
		return fmt.Errorf("open %s in %s: the app does not open this link: %w", link, app.ID, driver.ErrNotFound)
	}
	st, err := d.c.status(ctx)
	if err != nil {
		return err
	}
	if v := parseVersion(st.Build.Version); st.Build.Version == "" || !v.atLeast(wdaOpenURL.Major, wdaOpenURL.Minor) {
		return fmt.Errorf("WebDriverAgent %q cannot open a link in a given app (needs %s or later; install it again): %w",
			st.Build.Version, wdaOpenURL, driver.ErrUnsupported)
	}
	if err := d.c.do(ctx, d.c.long, http.MethodPost, "/url", map[string]any{"url": strings.TrimSpace(link), "bundleId": app.ID}, nil); err != nil {
		return fmt.Errorf("open %s in %s: %w", link, app.ID, err)
	}
	return nil
}

var (
	_ driver.LinkOpener = (*Device)(nil)
	_ driver.LinkOpener = (*AXDevice)(nil)
	_ driver.LinkOpener = (*WDADevice)(nil)
)
