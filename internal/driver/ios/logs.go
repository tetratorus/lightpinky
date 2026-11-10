package ios

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tetratorus/lightpinky/internal/driver"
	goios "github.com/danielpaulus/go-ios/ios"
	"github.com/danielpaulus/go-ios/ios/afc"
	"github.com/danielpaulus/go-ios/ios/instruments"
	"github.com/danielpaulus/go-ios/ios/ostrace"
)

// App logs and crash reports (driver.AppLogger, docs/notes/app-logs.md).
//
// An iPhone keeps no log history a computer can read back, short of an
// archive of the whole system, so Pinky listens: the first AppLogs call
// for an app starts a follower that streams the log of each of the app's
// processes from os_trace_relay, asked for that process alone, until the
// task ends (StopAppLogs). Other processes' lines never leave the phone.
// Crash reports come from crashreportcopymobile: the .ips files named
// after the app's executable whose header names the app's bundle id, cut
// down to their main parts.

const (
	installationProxyService = "com.apple.mobile.installation_proxy"
	crashMoverService        = "com.apple.crashreportmover"
	crashCopyService         = "com.apple.crashreportcopymobile"
)

// Limits of listening and reading.
const (
	followPoll     = time.Second            // how often a follower looks for the app's processes
	followFast     = 150 * time.Millisecond // ... for a while after Pinky launched an app
	followFastFor  = 3 * time.Second
	followGone     = 3 * time.Second // a process's stream stays open this long once it is not listed, for its last lines
	followRelook   = 5 * time.Second // at most this often the app's bundle is looked up again (a new install moves it)
	followMaxLines = 20_000          // lines a follower keeps; older ones go
	logTextMax     = 16 << 10        // bytes of one line's text kept
	openTimeout    = 10 * time.Second
	crashFileMax   = 4 << 20 // bytes of one crash report read
	crashReads     = 50      // crash reports read at most per call
	crashFrames    = 40      // frames of a backtrace shown
)

// appBundle is what the installation proxy says about one app.
type appBundle struct {
	ID, Path, Executable string
	Dev                  bool // signed with get-task-allow: a development build
}

// lookupBundle asks the installation proxy about one app. Pinky's own
// helper (the WebDriverAgent runner) is never read.
func lookupBundle(ctx context.Context, entry goios.DeviceEntry, id string) (appBundle, error) {
	if id == "" || strings.HasSuffix(id, ".xctrunner") {
		return appBundle{}, fmt.Errorf("%w: Pinky does not read its own helper's logs", driver.ErrNotFound)
	}
	var b appBundle
	err := within(ctx, func() error {
		conn, err := goios.ConnectToService(entry, installationProxyService)
		if err != nil {
			return fmt.Errorf("installation proxy: %w", err)
		}
		defer conn.Close()
		stop := context.AfterFunc(ctx, func() { conn.Close() })
		defer stop()
		codec := goios.NewPlistCodec()
		req, err := codec.Encode(map[string]any{"Command": "Lookup", "ClientOptions": map[string]any{
			"BundleIDs":        []string{id},
			"ReturnAttributes": []string{"CFBundleIdentifier", "CFBundleExecutable", "Path", "Entitlements"},
		}})
		if err != nil {
			return err
		}
		if err := conn.Send(req); err != nil {
			return fmt.Errorf("look up %s: %w", id, err)
		}
		for range 10 {
			msg, err := codec.Decode(conn.Reader())
			if err != nil {
				return fmt.Errorf("look up %s: %w", id, err)
			}
			m, err := goios.ParsePlist(msg)
			if err != nil {
				return fmt.Errorf("look up %s: %w", id, err)
			}
			if a, ok := lookupResult(m, id); ok {
				b = a
				return nil
			}
			if e, _ := m["Error"].(string); e != "" {
				return fmt.Errorf("look up %s: %s", id, e)
			}
			if s, _ := m["Status"].(string); s == "Complete" || m["LookupResult"] != nil {
				break
			}
		}
		return fmt.Errorf("%w: %s is not installed", driver.ErrNotFound, id)
	})
	return b, err
}

// lookupResult reads one app out of the installation proxy's answer to
// Lookup.
func lookupResult(m map[string]any, id string) (appBundle, bool) {
	res, _ := m["LookupResult"].(map[string]any)
	a, _ := res[id].(map[string]any)
	if a == nil {
		return appBundle{}, false
	}
	b := appBundle{ID: id}
	b.Path, _ = a["Path"].(string)
	b.Path = strings.TrimSuffix(b.Path, "/")
	b.Executable, _ = a["CFBundleExecutable"].(string)
	if ent, ok := a["Entitlements"].(map[string]any); ok {
		b.Dev, _ = ent["get-task-allow"].(bool)
	}
	return b, b.Path != ""
}

// within runs fn, which may block on the phone, and gives up when ctx
// ends (fn is left to finish on its own; it should close what it opened
// when ctx ends).
func within(ctx context.Context, fn func() error) error {
	ch := make(chan error, 1)
	go func() { ch <- fn() }()
	select {
	case err := <-ch:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// bundlePath is a path without the /private prefix, which some services
// put before /var and others leave out.
func bundlePath(p string) string { return strings.TrimPrefix(p, "/private") }

// inBundle reports whether an executable lies inside an app's bundle: the
// app itself or one of its extensions.
func inBundle(exe, dir string) bool {
	return dir != "" && exe != "" && strings.HasPrefix(bundlePath(exe), bundlePath(dir)+"/")
}

// entryLine turns an os_trace entry into a log line. Its tag is the
// entry's subsystem and category, or the name of the code that logged it.
func entryLine(e ostrace.LogEntry) driver.LogLine {
	l := driver.LogLine{Time: e.Timestamp, PID: int(e.PID), Level: entryLevel(e.Level), Text: e.Message}
	if e.Label != nil && e.Label.Subsystem != "" {
		l.Tag = e.Label.Subsystem
		if e.Label.Category != "" {
			l.Tag += ":" + e.Label.Category
		}
	} else if e.ImageName != "" {
		l.Tag = path.Base(e.ImageName)
	}
	if len(l.Text) > logTextMax {
		l.Text = l.Text[:logTextMax] + "…"
	}
	return l
}

// entryLevel maps os_log's types to log levels: debug, info (default and
// info), error, and fatal for a fault.
func entryLevel(l ostrace.LogLevel) driver.LogLevel {
	switch l {
	case ostrace.LogLevelDebug:
		return driver.LevelDebug
	case ostrace.LogLevelError:
		return driver.LevelError
	case ostrace.LogLevelFault:
		return driver.LevelFatal
	}
	return driver.LevelInfo
}

// logStream is one process's os_trace stream (*ostrace.Connection).
type logStream interface {
	ReadEntry() (ostrace.LogEntry, error)
	Close() error
}

// logSource is what a follower needs of the phone.
type logSource struct {
	open     func(pid int) (logStream, error)                  // a stream of one process's lines
	procs    func(ctx context.Context) (map[int]string, error) // the phone's processes: pid -> executable path
	relocate func(ctx context.Context) (string, error)         // the app's bundle directory, looked up again
}

// appFollower collects one app's log lines: it finds the app's processes
// and streams each one's lines while it runs.
type appFollower struct {
	src   logSource
	start time.Time

	// For tests: how often to look for processes, and how long a process's
	// stream outlives its listing.
	poll, gone time.Duration

	ctx    context.Context // ends with stop
	cancel context.CancelFunc
	wake   chan struct{}
	wg     sync.WaitGroup

	mu       sync.Mutex
	dir      string // the app's bundle directory
	lookedAt time.Time
	lines    []driver.LogLine // in the order they came
	trimmed  time.Time        // the newest line dropped for room
	streams  map[int]logStream
	missing  map[int]time.Time // followed processes no longer listed, since when
	stopped  bool
}

func newFollower(dir string, src logSource) *appFollower {
	ctx, cancel := context.WithCancel(context.Background())
	return &appFollower{src: src, start: time.Now(), poll: followPoll, gone: followGone,
		ctx: ctx, cancel: cancel, wake: make(chan struct{}, 1),
		dir: dir, lookedAt: time.Now(), streams: map[int]logStream{}, missing: map[int]time.Time{}}
}

// run looks for the app's processes until the follower stops.
func (f *appFollower) run() {
	defer f.wg.Done()
	var fastUntil time.Time
	for {
		f.scan()
		wait := f.poll
		if time.Now().Before(fastUntil) {
			wait = min(wait, followFast)
		}
		t := time.NewTimer(wait)
		select {
		case <-f.ctx.Done():
			t.Stop()
			return
		case <-f.wake:
			t.Stop()
			fastUntil = time.Now().Add(followFastFor)
		case <-t.C:
		}
	}
}

// poke makes the follower look for processes now, and often for a while:
// Pinky just launched an app.
func (f *appFollower) poke() {
	select {
	case f.wake <- struct{}{}:
	default:
	}
}

// scan lists the phone's processes once, follows the app's new ones and
// lets go of those that ended.
func (f *appFollower) scan() {
	ctx, cancel := context.WithTimeout(f.ctx, 5*time.Second)
	defer cancel()
	procs, err := f.src.procs(ctx)
	if err != nil {
		return
	}
	f.mu.Lock()
	dir, lookedAt := f.dir, f.lookedAt
	f.mu.Unlock()
	relooked := false
	for pid, exe := range procs {
		if !inBundle(exe, dir) {
			// A process of a bundle with the app's name elsewhere: the app
			// may have been installed again, which moves its bundle.
			if relooked || f.src.relocate == nil || time.Since(lookedAt) < followRelook ||
				!strings.Contains(bundlePath(exe), "/"+path.Base(dir)+"/") {
				continue
			}
			relooked = true
			d, err := f.src.relocate(ctx)
			f.mu.Lock()
			f.lookedAt = time.Now()
			if err == nil && d != "" {
				f.dir, dir = d, d
			}
			f.mu.Unlock()
			if !inBundle(exe, dir) {
				continue
			}
		}
		f.follow(pid)
	}
	now := time.Now()
	f.mu.Lock()
	defer f.mu.Unlock()
	for pid, s := range f.streams {
		if exe, ok := procs[pid]; ok && inBundle(exe, f.dir) {
			delete(f.missing, pid)
			continue
		}
		since, ok := f.missing[pid]
		if !ok {
			f.missing[pid] = now
			continue
		}
		if now.Sub(since) >= f.gone {
			s.Close()
			delete(f.streams, pid)
			delete(f.missing, pid)
		}
	}
}

// follow streams a process's lines, unless it already does.
func (f *appFollower) follow(pid int) {
	f.mu.Lock()
	_, have := f.streams[pid]
	stopped := f.stopped
	f.mu.Unlock()
	if have || stopped {
		return
	}
	s, err := openWithin(openTimeout, func() (logStream, error) { return f.src.open(pid) })
	if err != nil {
		return // the next scan tries again
	}
	f.mu.Lock()
	if f.stopped || f.streams[pid] != nil {
		f.mu.Unlock()
		s.Close()
		return
	}
	f.streams[pid] = s
	delete(f.missing, pid)
	f.wg.Add(1)
	f.mu.Unlock()
	go f.read(pid, s)
}

// openWithin opens a stream, giving up after d; a stream that opens later
// is closed.
func openWithin(d time.Duration, open func() (logStream, error)) (logStream, error) {
	type result struct {
		s   logStream
		err error
	}
	ch := make(chan result, 1)
	go func() {
		s, err := open()
		ch <- result{s, err}
	}()
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case r := <-ch:
		return r.s, r.err
	case <-t.C:
		go func() {
			if r := <-ch; r.err == nil {
				r.s.Close()
			}
		}()
		return nil, errors.New("os_trace: the phone did not answer")
	}
}

// read collects a process's lines until its stream ends. Every line is
// checked to be the process's own.
func (f *appFollower) read(pid int, s logStream) {
	defer f.wg.Done()
	for {
		e, err := s.ReadEntry()
		if err != nil {
			break
		}
		if int(e.PID) != pid {
			continue
		}
		f.mu.Lock()
		if e.Filename != "" && !inBundle(e.Filename, f.dir) {
			f.mu.Unlock()
			continue
		}
		f.addLocked(entryLine(e))
		f.mu.Unlock()
	}
	s.Close()
	f.mu.Lock()
	if f.streams[pid] == s {
		delete(f.streams, pid)
		delete(f.missing, pid)
	}
	f.mu.Unlock()
}

// addLocked keeps a line, dropping the oldest quarter when full.
func (f *appFollower) addLocked(l driver.LogLine) {
	if f.stopped {
		return
	}
	f.lines = append(f.lines, l)
	if len(f.lines) <= followMaxLines {
		return
	}
	n := len(f.lines) - followMaxLines*3/4
	for _, old := range f.lines[:n] {
		if old.Time.After(f.trimmed) {
			f.trimmed = old.Time
		}
	}
	f.lines = append([]driver.LogLine(nil), f.lines[n:]...)
}

// query returns the lines the follower has that pass q, oldest first.
func (f *appFollower) query(q driver.LogQuery) driver.LogResult {
	f.mu.Lock()
	var kept []driver.LogLine
	for _, l := range f.lines {
		if (q.Since.IsZero() || !l.Time.Before(q.Since)) && q.Keep(l) {
			kept = append(kept, l)
		}
	}
	from := f.start
	if f.trimmed.After(from) {
		from = f.trimmed
	}
	f.mu.Unlock()
	sort.SliceStable(kept, func(i, j int) bool { return kept[i].Time.Before(kept[j].Time) })
	res := driver.LogResult{Lines: kept, From: from}
	if q.Max > 0 && len(kept) > q.Max {
		res.Dropped = len(kept) - q.Max
		res.Lines = kept[res.Dropped:]
	}
	return res
}

// stop ends the follower's streams and forgets its lines.
func (f *appFollower) stop() {
	f.mu.Lock()
	if f.stopped {
		f.mu.Unlock()
		return
	}
	f.stopped = true
	f.cancel()
	for _, s := range f.streams {
		s.Close()
	}
	f.streams, f.lines = map[int]logStream{}, nil
	f.mu.Unlock()
	f.wg.Wait()
}

// appLogs holds a phone's followers, one per app read in the task.
type appLogs struct {
	mu        sync.Mutex
	followers map[string]*appFollower // by bundle id
}

// follower returns the app's follower, starting one in dir when there is
// none.
func (a *appLogs) follower(id, dir string, src logSource) *appFollower {
	a.mu.Lock()
	defer a.mu.Unlock()
	if f := a.followers[id]; f != nil {
		return f
	}
	if a.followers == nil {
		a.followers = map[string]*appFollower{}
	}
	f := newFollower(dir, src)
	a.followers[id] = f
	f.wg.Add(1)
	go f.run()
	return f
}

// launched tells the app's follower, if any, that Pinky launched the app:
// pid is its process when the phone said (0 when not).
func (a *appLogs) launched(id string, pid int) {
	a.mu.Lock()
	f := a.followers[id]
	a.mu.Unlock()
	if f == nil {
		return
	}
	if pid > 0 {
		go f.follow(pid)
	}
	f.poke()
}

// stopAll stops every follower (driver.LogListener).
func (a *appLogs) stopAll() {
	a.mu.Lock()
	fs := a.followers
	a.followers = nil
	a.mu.Unlock()
	var wg sync.WaitGroup
	for _, f := range fs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f.stop()
		}()
	}
	wg.Wait()
}

// logPhone is what reading logs needs of either kind of iPhone connection.
type logPhone struct {
	entry goios.DeviceEntry // the usbmuxd entry, for lockdown services
	rsd   goios.DeviceEntry // the tunnel's entry when there is one: os_trace's shim service
	procs func(ctx context.Context) (map[int]string, error)
	logs  *appLogs
}

func (p logPhone) appLogs(ctx context.Context, q driver.LogQuery) (driver.LogResult, error) {
	b, err := lookupBundle(ctx, p.entry, q.App.ID)
	if err != nil {
		return driver.LogResult{}, err
	}
	src := logSource{open: p.openStream, procs: p.procs, relocate: func(ctx context.Context) (string, error) {
		b, err := lookupBundle(ctx, p.entry, b.ID)
		return b.Path, err
	}}
	f := p.logs.follower(b.ID, b.Path, src)
	f.mu.Lock()
	f.dir, f.lookedAt = b.Path, time.Now() // a new install moves the bundle
	f.mu.Unlock()
	return f.query(q), nil
}

// openStream asks os_trace_relay for one process's lines, every level.
func (p logPhone) openStream(pid int) (logStream, error) {
	c, err := ostrace.New(p.entry, pid, ostrace.MessageFilterLogMessage, ostrace.StreamFlagsAll)
	if err != nil && p.rsd.SupportsRsd() {
		c, err = ostrace.New(p.rsd, pid, ostrace.MessageFilterLogMessage, ostrace.StreamFlagsAll)
	}
	if err != nil {
		return nil, err
	}
	return c, nil
}

func (p logPhone) appCrashes(ctx context.Context, q driver.LogQuery) ([]driver.CrashReport, error) {
	b, err := lookupBundle(ctx, p.entry, q.App.ID)
	if err != nil {
		return nil, err
	}
	var out []driver.CrashReport
	err = within(ctx, func() error {
		// The mover brings new reports into the folder the copy service
		// shows; without it they can be missing for a while.
		moveCrashes(ctx, p.entry)
		conn, err := goios.ConnectToService(p.entry, crashCopyService)
		if err != nil {
			return fmt.Errorf("crash reports: %w", err)
		}
		defer conn.Close()
		stop := context.AfterFunc(ctx, func() { conn.Close() })
		defer stop()
		out, err = readCrashes(afcFiles{afc.NewFromConn(conn)}, b, q)
		return err
	})
	return out, err
}

// moveCrashes asks the phone to move its new crash reports where the copy
// service reads them; it answers "ping" when done.
func moveCrashes(ctx context.Context, entry goios.DeviceEntry) {
	conn, err := goios.ConnectToService(entry, crashMoverService)
	if err != nil {
		return
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	if c := conn.Conn(); c != nil {
		c.SetReadDeadline(time.Now().Add(10 * time.Second))
	}
	ping := make([]byte, 4)
	io.ReadFull(conn.Reader(), ping)
}

func (p logPhone) devBuild(ctx context.Context, app driver.App) (bool, error) {
	b, err := lookupBundle(ctx, p.entry, app.ID)
	return b.Dev, err
}

// AppLogs implements driver.AppLogger: the app's lines since the first
// call for it in the task.
func (d *Device) AppLogs(ctx context.Context, q driver.LogQuery) (driver.LogResult, error) {
	res, err := d.logPhone().appLogs(ctx, q)
	return res, d.gone(err)
}

// AppCrashes implements driver.AppLogger.
func (d *Device) AppCrashes(ctx context.Context, q driver.LogQuery) ([]driver.CrashReport, error) {
	res, err := d.logPhone().appCrashes(ctx, q)
	return res, d.gone(err)
}

// DevBuild implements driver.AppLogger: whether the app is signed with
// get-task-allow, as apps Xcode installs are.
func (d *Device) DevBuild(ctx context.Context, app driver.App) (bool, error) {
	dev, err := d.logPhone().devBuild(ctx, app)
	return dev, d.gone(err)
}

// StopAppLogs implements driver.LogListener.
func (d *Device) StopAppLogs() { d.logs.stopAll() }

func (d *Device) logPhone() logPhone {
	return logPhone{entry: d.l.base, rsd: d.l.entry, logs: &d.logs, procs: func(ctx context.Context) (map[int]string, error) {
		m, err := d.processPaths(ctx)
		if err != nil {
			return nil, err
		}
		out := make(map[int]string, len(m))
		for pid, exe := range m {
			out[int(pid)] = exe
		}
		return out, nil
	}}
}

// AppLogs implements driver.AppLogger. A Simulator's logs are not read.
func (d *WDADevice) AppLogs(ctx context.Context, q driver.LogQuery) (driver.LogResult, error) {
	p, err := d.logPhone()
	if err != nil {
		return driver.LogResult{}, err
	}
	return p.appLogs(ctx, q)
}

// AppCrashes implements driver.AppLogger.
func (d *WDADevice) AppCrashes(ctx context.Context, q driver.LogQuery) ([]driver.CrashReport, error) {
	p, err := d.logPhone()
	if err != nil {
		return nil, err
	}
	return p.appCrashes(ctx, q)
}

// DevBuild implements driver.AppLogger.
func (d *WDADevice) DevBuild(ctx context.Context, app driver.App) (bool, error) {
	p, err := d.logPhone()
	if err != nil {
		return false, err
	}
	return p.devBuild(ctx, app)
}

// StopAppLogs implements driver.LogListener.
func (d *WDADevice) StopAppLogs() {
	d.logs.stopAll()
	d.procMu.Lock()
	if d.procSvc != nil {
		d.procSvc.Close()
		d.procSvc = nil
	}
	d.procMu.Unlock()
}

func (d *WDADevice) logPhone() (logPhone, error) {
	if d.entry.DeviceID == 0 {
		return logPhone{}, fmt.Errorf("app logs on a Simulator: %w", driver.ErrUnsupported)
	}
	p := logPhone{entry: d.entry, logs: &d.logs, procs: d.processPaths}
	if d.l != nil {
		p.rsd = d.l.entry
	}
	return p, nil
}

// processPaths lists the phone's processes as pid -> executable path,
// through the instruments service the helper's test run needs anyway.
func (d *WDADevice) processPaths(ctx context.Context) (map[int]string, error) {
	d.procMu.Lock()
	defer d.procMu.Unlock()
	if d.procSvc == nil {
		e := d.entry
		if d.l != nil {
			e = d.l.entry
		}
		var svc *instruments.DeviceInfoService
		err := within(ctx, func() (err error) {
			svc, err = instruments.NewDeviceInfoService(e)
			return err
		})
		if err != nil {
			return nil, err
		}
		d.procSvc = svc
	}
	svc := d.procSvc
	var list []instruments.ProcessInfo
	err := within(ctx, func() (err error) {
		list, err = svc.ProcessList()
		return err
	})
	if err != nil {
		svc.Close()
		d.procSvc = nil
		return nil, err
	}
	m := make(map[int]string, len(list))
	for _, p := range list {
		if p.RealAppName != "" {
			m[int(p.Pid)] = p.RealAppName
		}
	}
	return m, nil
}

var (
	_ driver.AppLogger   = (*Device)(nil)
	_ driver.LogListener = (*Device)(nil)
	_ driver.AppLogger   = (*WDADevice)(nil)
	_ driver.LogListener = (*WDADevice)(nil)
)
