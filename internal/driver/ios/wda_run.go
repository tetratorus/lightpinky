package ios

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	goios "github.com/danielpaulus/go-ios/ios"
	"github.com/danielpaulus/go-ios/ios/installationproxy"
	"github.com/danielpaulus/go-ios/ios/testmanagerd"
)

// The WebDriverAgent runner is a UI test bundle inside an app whose bundle
// id ends in ".xctrunner" (Xcode names it after the test target:
// <prefix>.WebDriverAgentRunner.xctrunner). The person builds and installs
// it once with their own signing team (see docs/notes/ios.md); Pinky finds
// it on the phone and starts its test through testmanagerd, the way Xcode's
// "Test" does, every time it opens the phone.

// wdaTestBundle is the runner's test bundle.
const wdaTestBundle = "WebDriverAgentRunner.xctest"

// wdaEnv is the runner's environment, binding both of WDA's servers to
// bind. WDA listens on every interface of the phone by default, Wi-Fi
// included, where anyone on the network could drive the phone around
// Pinky's approvals.
func wdaEnv(bind string) map[string]any {
	return map[string]any{
		"USE_IP":            bind,
		"USE_PORT":          fmt.Sprint(wdaPort),
		"MJPEG_SERVER_PORT": fmt.Sprint(mjpegPort),
	}
}

// wdaBind is the address WDA listens on, which only Pinky can reach. On a
// cable usbmuxd connects on the phone's loopback. On Wi-Fi it connects to
// the phone's network address, which anyone on the network can reach, so
// Pinky goes through its tunnel instead, whose address on the phone only
// this computer reaches.
func wdaBind(base goios.DeviceEntry, t *Tunnel) string {
	if base.Properties.ConnectionType == "Network" && t != nil {
		return t.DeviceAddr().String()
	}
	return "127.0.0.1"
}

// findWDARunner returns the bundle id of the WebDriverAgent runner
// installed on the phone, or "" when there is none.
func findWDARunner(e goios.DeviceEntry) (string, error) {
	r, err := findRunnerApp(e)
	return r.BundleID, err
}

// wdaRunner is the WebDriverAgent runner installed on a phone.
type wdaRunner struct {
	BundleID string
	// AppID is its application identifier, "<team>.<bundle id>", from
	// the entitlements it was signed with; "" when unknown.
	AppID string
}

// findRunnerApp finds the WebDriverAgent runner installed on the phone;
// its BundleID is "" when there is none.
func findRunnerApp(e goios.DeviceEntry) (wdaRunner, error) {
	conn, err := installationproxy.New(e)
	if err != nil {
		return wdaRunner{}, fmt.Errorf("installation proxy: %w", err)
	}
	defer conn.Close()
	apps, err := conn.BrowseUserApps()
	if err != nil {
		return wdaRunner{}, fmt.Errorf("list apps: %w", err)
	}
	return pickRunnerApp(apps), nil
}

// pickWDARunner chooses the runner among the installed apps: a test runner
// named after WebDriverAgent. Pinky's own build wins over others.
func pickWDARunner(apps []installationproxy.AppInfo) string { return pickRunnerApp(apps).BundleID }

func pickRunnerApp(apps []installationproxy.AppInfo) wdaRunner {
	var best wdaRunner
	for _, a := range apps {
		id, _ := a["CFBundleIdentifier"].(string)
		if !strings.HasSuffix(id, ".xctrunner") {
			continue
		}
		name, _ := a["CFBundleName"].(string)
		exe, _ := a["CFBundleExecutable"].(string)
		if !strings.Contains(strings.ToLower(id+" "+name+" "+exe), "webdriveragentrunner") {
			continue
		}
		if best.BundleID == "" || strings.Contains(strings.ToLower(id), "pinky") {
			best = wdaRunner{BundleID: id}
			if ent, ok := a["Entitlements"].(map[string]any); ok {
				best.AppID, _ = ent["application-identifier"].(string)
			}
		}
	}
	return best
}

// wdaLog receives the runner's test log; tests point it at stderr.
var wdaLog io.Writer = io.Discard

// wdaRun is the test run hosting WebDriverAgent on a phone.
type wdaRun struct {
	cancel context.CancelFunc
	done   chan struct{}
	log    *runnerLog

	mu  sync.Mutex
	err error
}

// startWDA starts the runner's test, WDA listening on bind. entry must
// reach the phone's testmanagerd: the plain usbmuxd entry below iOS 17, the
// tunnel's entry from 17.4. The run lasts until stop, or until the test
// ends on the phone (the person closed the runner, the phone locked for
// long, a reboot).
func startWDA(entry goios.DeviceEntry, bundleID, bind string) *wdaRun {
	ctx, cancel := context.WithCancel(context.Background())
	r := &wdaRun{cancel: cancel, done: make(chan struct{}), log: &runnerLog{}}
	go func() {
		defer close(r.done)
		dir, err := os.MkdirTemp("", "pinky-wda-")
		if err == nil {
			defer os.RemoveAll(dir)
		}
		_, err = testmanagerd.RunTestWithConfig(ctx, testmanagerd.TestConfig{
			BundleId:           bundleID,
			TestRunnerBundleId: bundleID,
			XctestConfigName:   wdaTestBundle,
			Env:                wdaEnv(bind),
			Device:             entry,
			Listener:           testmanagerd.NewTestListener(r.log, r.log, dir),
		})
		if err == nil && ctx.Err() == nil {
			err = errors.New("the WebDriverAgent test ended")
		}
		r.mu.Lock()
		r.err = err
		r.mu.Unlock()
	}()
	return r
}

// alive reports whether the run is still going, and if not, why it ended.
func (r *wdaRun) alive() (bool, error) {
	select {
	case <-r.done:
		r.mu.Lock()
		defer r.mu.Unlock()
		return false, r.err
	default:
		return true, nil
	}
}

// stop ends the run and waits a little for the phone to let go of it.
func (r *wdaRun) stop() {
	r.cancel()
	select {
	case <-r.done:
	case <-time.After(5 * time.Second):
	}
}

// asking reports whether the phone waits for its passcode to allow
// automation (see runnerLog).
func (r *wdaRun) asking() bool { return r.log != nil && r.log.asking(time.Now()) }

// runnerLog reads the runner's log as it arrives, passing it on to wdaLog,
// for one wait: now and then iOS asks on the phone for its passcode before
// a UI test runs (after a restart, and again later: on iOS 27 half an hour
// later, after the phone had locked), and XCTest waits up to a minute for it.
// The runner logs "<XCTWaiter: 0x...> entering wait loop for 60.00s with
// expectations: `enabling automation mode`" when it starts waiting, and
// "Returning result ... for synchronous waiter <XCTWaiter: 0x...>" when
// the wait ends, either way. A wait that ends at once (automation already
// allowed) is not asking.
type runnerLog struct {
	mu     sync.Mutex
	line   []byte    // the line being read
	waiter string    // the XCTWaiter waiting for automation mode, "" when none
	since  time.Time // when it started waiting
}

// automationGrace is how long the automation wait must last to be a
// question to the person. Without one it ends within a second.
const automationGrace = 5 * time.Second

// maxLogLine bounds a log line runnerLog keeps; the lines it looks for are
// short, and longer lines are cut.
const maxLogLine = 4096

func (l *runnerLog) Write(b []byte) (int, error) {
	n := len(b)
	wdaLog.Write(b)
	l.mu.Lock()
	defer l.mu.Unlock()
	for len(b) > 0 {
		i := bytes.IndexByte(b, '\n')
		part := b
		if i >= 0 {
			part = b[:i]
		}
		if room := maxLogLine - len(l.line); room > 0 {
			l.line = append(l.line, part[:min(len(part), room)]...)
		}
		if i < 0 {
			break
		}
		l.read(string(l.line), time.Now())
		l.line = l.line[:0]
		b = b[i+1:]
	}
	return n, nil
}

// read takes one line of the log.
func (l *runnerLog) read(line string, now time.Time) {
	switch {
	case strings.Contains(line, "entering wait loop") && strings.Contains(line, "enabling automation mode"):
		l.waiter, l.since = waiterOf(line), now
	case l.waiter != "" && strings.Contains(line, "for synchronous waiter "+l.waiter):
		l.waiter = ""
	}
}

func (l *runnerLog) asking(now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.waiter != "" && now.Sub(l.since) >= automationGrace
}

// waiterOf finds the waiter a log line names, "<XCTWaiter: 0x...>".
func waiterOf(line string) string {
	i := strings.Index(line, "<XCTWaiter: ")
	if i < 0 {
		return "<XCTWaiter>"
	}
	j := strings.IndexByte(line[i:], '>')
	if j < 0 {
		return "<XCTWaiter>"
	}
	return line[i : i+j+1]
}
