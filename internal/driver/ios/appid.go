package ios

import (
	"context"
	"net/url"
	"strings"
	"time"

	"github.com/tetratorus/lightpinky/internal/driver"
	"github.com/tetratorus/lightpinky/internal/screen"
)

// Naming the app on screen. A read starts from remoteAXService's frontmost
// application element, whose process is looked up in the process list and
// the app catalog. Two things went wrong with that in the field:
//
//   - The first read of a session named the app without its bundle id
//     ("Plasma One", item 27): the lookup failed once, and the element's
//     label was all there was. Blocking by bundle id cannot work on such a
//     read, so a lookup that fails now falls back to the catalog by name,
//     and a process the catalog does not list refreshes it once.
//   - While Face ID's prompt or its fade-out was in front of an app, the
//     frontmost application was SpringBoard ("Home Screen") or a process
//     with no name, and the screen still showed the app beneath (OCR read
//     its balances; item 14). A blocked app would leak. When the frontmost
//     process is not an app from the home screen, the app the system has in
//     the foreground beneath it is named instead, from the accessibility
//     daemon's app states: the screen is still that app's, with something
//     of the system's in front.

// appListWait bounds the wait for the daemon's first app state, in tests.
const appListWait = time.Second

// screenApp names the app on screen from a read's elements, the first of
// which is the frontmost application's. note records what it decided, for
// ObserveStats.Trace. On a locked phone the lock screen is SpringBoard's,
// and nothing is beneath it.
func (d *Device) screenApp(ctx context.Context, els []raxElement, locked bool, note func(string, ...any)) screen.App {
	front := els[0]
	app, ok := d.appForPID(ctx, uint64(front.Object.PID))
	if !ok && strings.TrimSpace(front.Label) != "" {
		if app, ok = d.appByName(ctx, front.Label); ok {
			note("app %q found by its name", app.ID)
		}
	}
	if ok && app.ID != springBoard && d.homeScreenApp(ctx, app.ID) {
		d.sawFront(screen.App{Name: app.Name, ID: app.ID})
		return screen.App{Name: app.Name, ID: app.ID}
	}
	// The home screen itself: its icons are on screen. The app that was in
	// front may still be listed in the foreground for a moment after Home.
	if ok && app.ID == springBoard && (locked || homeIcons(els)) {
		d.sawFront(screen.App{})
		return screen.App{Name: app.Name, ID: app.ID}
	}
	if !locked {
		if fg, found := d.foregroundApp(ctx); found {
			note("pid %d (%q) is in front of %s", front.Object.PID, app.ID, fg.ID)
			return fg
		}
	}
	if ok {
		return screen.App{Name: app.Name, ID: app.ID}
	}
	return screen.App{Name: front.Label}
}

// sawFront records the home screen app a read found in front, or none.
func (d *Device) sawFront(app screen.App) {
	d.mu.Lock()
	d.lastFront, d.lastFrontAt = app, time.Now()
	d.mu.Unlock()
}

// lastFrontKeep is how long the app a read found in front is taken to be
// beneath what came in front of it, when the inspector cannot say.
const lastFrontKeep = 30 * time.Second

// homeIcons reports whether a read shows the home screen's app icons.
func homeIcons(els []raxElement) bool {
	for _, e := range els[min(1, len(els)):] {
		if e.Type == xcIcon && e.Visible && e.HasFrame && e.Object.PID == els[0].Object.PID {
			return true
		}
	}
	return false
}

// foregroundApp returns the home screen app the system has in the
// foreground beneath what is in front, if it can tell: an app the
// inspector session saw come to the foreground since the last read that
// named the app in front, else that app, unless the daemon said since that
// it left. The daemon reports only changes after the session connects (Open
// connects it), so an app that was in front all along is known only from
// reads. And it reports apps that iOS wakes in the background (a bank's
// app, for a push; widgets) in the foreground too: those it reported before
// a read found another app in front were not on screen.
func (d *Device) foregroundApp(ctx context.Context) (screen.App, bool) {
	states, tracking := d.appStatesList(ctx)
	d.mu.Lock()
	last, lastAt := d.lastFront, d.lastFrontAt
	d.mu.Unlock()
	var best screen.App
	var bestAt time.Time
	for _, a := range states {
		if a.state != appForeground || a.id == springBoard || axBackgroundUI[a.id] || !a.at.After(lastAt) || !a.at.After(bestAt) {
			continue
		}
		if app, ok := d.listedApp(ctx, a.id); ok {
			best, bestAt = screen.App{Name: app.Name, ID: app.ID}, a.at
		}
	}
	if best.ID != "" {
		return best, true
	}
	if last.ID == "" || !(tracking || time.Since(lastAt) < lastFrontKeep) {
		return screen.App{}, false
	}
	for _, a := range states {
		if a.id == last.ID && a.state != appForeground && a.at.After(lastAt) {
			return screen.App{}, false // it left
		}
	}
	return last, true
}

// appStatesList returns what the inspector session knows of each app, and
// whether it is connected to tell changes.
func (d *Device) appStatesList(ctx context.Context) ([]axApp, bool) {
	if d.foreground != nil {
		return d.foreground(ctx), true
	}
	ax, err := d.axReady(ctx)
	if err != nil {
		return nil, false
	}
	return ax.allApps(), true
}

// homeScreenApp reports whether id is an app a person opens from the home
// screen, rather than SpringBoard, a system UI service (Face ID's and the
// passcode's prompts, share sheets) or a helper.
func (d *Device) homeScreenApp(ctx context.Context, id string) bool {
	_, ok := d.listedApp(ctx, id)
	return ok
}

// listedApp returns the home screen app with bundle id id.
func (d *Device) listedApp(ctx context.Context, id string) (driver.App, bool) {
	c, err := d.catalog(ctx)
	if err != nil {
		return driver.App{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, a := range c.apps {
		if a.ID == id {
			return a, true
		}
	}
	return driver.App{}, false
}

// appByName returns the one home screen app named name, as the frontmost
// application's element labels it.
func (d *Device) appByName(ctx context.Context, name string) (driver.App, bool) {
	c, err := d.catalog(ctx)
	if err != nil {
		return driver.App{}, false
	}
	return c.byName(name)
}

// byName returns the one listed app with this display name; none when two
// share it.
func (c *appCatalog) byName(name string) (driver.App, bool) {
	name = strings.TrimSpace(name)
	c.mu.Lock()
	defer c.mu.Unlock()
	var found driver.App
	n := 0
	for _, a := range c.apps {
		if a.Name == name {
			found = a
			n++
		}
	}
	return found, n == 1
}

// executablePath turns a process's executable URL into a path: CoreDevice
// lists processes by file URL, whose path is percent-encoded ("Plasma%20One").
func executablePath(rel string) string {
	if u, err := url.Parse(rel); err == nil && u.Scheme == "file" && u.Path != "" {
		return u.Path
	}
	return strings.TrimPrefix(rel, "file://")
}
