package ios

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	goios "github.com/danielpaulus/go-ios/ios"
	dtx "github.com/danielpaulus/go-ios/ios/dtx_codec"
	"github.com/danielpaulus/go-ios/ios/nskeyedarchiver"
)

// The accessibility audit daemon is what Xcode's Accessibility Inspector
// talks to. It reads elements by moving the inspector's focus, VoiceOver
// style, one element per round trip; each focus change carries the
// element's spoken caption and an opaque handle, and attributes such as the
// label, value and traits can then be read for that handle. On iOS 27 it
// gives no element frames (see docs/notes/ios.md).
const axService = "com.apple.accessibility.axAuditDaemon.remoteserver"

const (
	selElementChanged = "hostInspectorCurrentElementChanged:"
	selAppState       = "hostAppStateChanged:"
	selNotification   = "hostInspectorNotificationReceived:"
)

// Host selectors the daemon calls. Registering them keeps the DTX reader
// from logging every one it would otherwise drop.
var axHostSelectors = []string{
	selElementChanged, selAppState, selNotification,
	"hostInspectorMonitoredEventTypeChanged:",
	"hostInspectorCurrentElementPropertiesChanged:",
	"hostDeviceFrontmostAppPidDidChange:",
	"hostDeviceFrontmostApplicationsDidChange",
	"hostDeviceRunningApplicationsDidChange",
	"hostAccessibilitySettingDidChange:",
	"hostAppendAuditLog:",
	"hostFoundAuditIssue:",
	"hostBailWithMessage:",
}

// Inspector move directions.
const (
	axNext  = int32(4)
	axFirst = int32(5)
)

// axNode is one element the walk visited.
type axNode struct {
	handle     []byte // PlatformElementValue_v1
	caption    string // label, value, traits and hint, as VoiceOver reads it
	ident      string // accessibility identifier
	label      string
	value      string
	traits     uint64
	traitNames []string
}

type axApp struct {
	name, id string
	pid      uint64
	state    uint64
	seq      int
	at       time.Time // when its state last changed
}

// axSession is a long-lived connection to the audit daemon.
type axSession struct {
	conn *dtx.Connection
	ch   *dtx.Channel

	walkMu sync.Mutex // one walk at a time: the inspector focus is global

	appMu   sync.Mutex
	apps    map[string]*axApp
	appSeq  int
	appsIn  chan struct{} // closed when the first app state arrived
	appsGot bool          // appsIn is closed
	changed atomic.Int64  // unix nanos of the last screen or layout change

	done chan struct{}
	once sync.Once
}

func dialAX(ctx context.Context, entry goios.DeviceEntry) (*axSession, error) {
	type result struct {
		c   *dtx.Connection
		err error
	}
	ch := make(chan result, 1)
	go func() {
		c, err := dtx.NewUsbmuxdConnection(entry, axService)
		ch <- result{c, err}
	}()
	var conn *dtx.Connection
	select {
	case r := <-ch:
		if r.err != nil {
			return nil, fmt.Errorf("connect %s: %w", axService, r.err)
		}
		conn = r.c
	case <-ctx.Done():
		go func() {
			if r := <-ch; r.c != nil {
				r.c.Close()
			}
		}()
		return nil, ctx.Err()
	}
	a := &axSession{conn: conn, ch: conn.GlobalChannel(), apps: map[string]*axApp{}, appsIn: make(chan struct{}), done: make(chan struct{})}
	conn.MessageDispatcher = a
	for _, sel := range axHostSelectors {
		a.ch.RegisterMethodForRemote(sel)
	}
	go a.pump()
	a.ch.MethodCallAsync("_notifyOfPublishedCapabilities:", map[string]any{
		"com.apple.private.DTXBlockCompression": uint64(2),
		"com.apple.private.DTXConnection":       uint64(1),
	})
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := a.ch.MethodCallWithContext(cctx, "deviceCapabilities"); err != nil {
		conn.Close()
		return nil, fmt.Errorf("accessibility daemon: %w", err)
	}
	// No on-screen highlight, no selection mode (on iOS 27 it swallows
	// touches), and app state events on.
	a.ch.MethodCallAsync("deviceInspectorShowVisuals:", false)
	a.ch.MethodCallAsync("deviceInspectorShowIgnoredElements:", false)
	a.ch.MethodCallAsync("deviceInspectorSetMonitoredEventType:", uint64(0))
	// The daemon lists the running apps when app monitoring turns on. A
	// session that ended without turning it off leaves it on, so turn it off
	// first to get the list again.
	a.ch.MethodCallAsync("deviceSetAppMonitoringEnabled:", false)
	a.ch.MethodCallAsync("deviceSetAppMonitoringEnabled:", true)
	a.ch.MethodCallAsync("deviceSetAuditTargetPid:", uint64(0))
	return a, nil
}

// pump consumes the daemon's events. Element changes are left for walks.
func (a *axSession) pump() {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		select {
		case <-a.done:
		case <-a.conn.Closed():
		}
		cancel()
	}()
	for _, sel := range axHostSelectors {
		if sel == selElementChanged {
			continue
		}
		go func(sel string) {
			for {
				msg, err := a.ch.ReceiveMethodCallWithTimeout(ctx, sel)
				if err != nil {
					return
				}
				switch sel {
				case selAppState:
					a.onAppState(msg)
				case selNotification:
					a.changed.Store(time.Now().UnixNano())
				}
			}
		}(sel)
	}
}

func unarchiveArg(msg dtx.Message) any {
	args := msg.Auxiliary.GetArguments()
	if len(args) == 0 {
		return nil
	}
	b, ok := args[0].([]byte)
	if !ok {
		return args[0]
	}
	v, err := nskeyedarchiver.Unarchive(b)
	if err != nil || len(v) == 0 {
		return nil
	}
	return v[0]
}

// Foreground state in SBApplicationNotificationStateChanged events.
const appForeground = 8

// UI services that report themselves in the foreground next to the app the
// person sees.
var axBackgroundUI = map[string]bool{
	"com.apple.InputUI":                       true,
	"com.apple.chrono.WidgetRenderer-Default": true,
	"com.apple.AccessibilityUIServer":         true,
	"com.apple.SleepLockScreen":               true,
}

func (a *axSession) onAppState(msg dtx.Message) {
	m, ok := unarchiveArg(msg).(map[string]any)
	if !ok {
		return
	}
	id, _ := m["displayID"].(string)
	if id == "" {
		return
	}
	name, _ := m["appName"].(string)
	state := uint64(toInt64(m["state"]))
	pid := uint64(toInt64(m["pid"]))
	a.appMu.Lock()
	defer a.appMu.Unlock()
	a.appSeq++
	app := a.apps[id]
	if app == nil {
		app = &axApp{id: id}
		a.apps[id] = app
	}
	if name != "" && name != "Unknown" {
		app.name = name
	}
	app.state = state
	app.seq = a.appSeq
	if pid != 0 {
		app.pid = pid
	}
	app.at = time.Now()
	if !a.appsGot {
		a.appsGot = true
		close(a.appsIn)
	}
}

// waitApps waits, up to wait, for the daemon's first app state. On iOS 27
// it sends none when app monitoring turns on, only changes after that (an
// app coming to the foreground or leaving it), so a session knows only the
// apps that changed since it connected.
func (a *axSession) waitApps(ctx context.Context, wait time.Duration) bool {
	t := time.NewTimer(wait)
	defer t.Stop()
	a.appMu.Lock()
	in := a.appsIn
	a.appMu.Unlock()
	select {
	case <-in:
		return true
	case <-t.C:
	case <-ctx.Done():
	}
	return false
}

// allApps returns what the daemon last said of each app it reported.
func (a *axSession) allApps() []axApp {
	a.appMu.Lock()
	defer a.appMu.Unlock()
	out := make([]axApp, 0, len(a.apps))
	for _, app := range a.apps {
		out = append(out, *app)
	}
	return out
}

// Dispatch implements dtx.Dispatcher for the daemon's calls that no walk
// or pump waits for, which go-ios would otherwise log as errors.
func (a *axSession) Dispatch(dtx.Message) {}

// foregroundApps returns the apps the system has in the foreground, the one
// that came there last first. An app stays there while something of the
// system's covers it: Face ID's prompt, an alert, a banner.
func (a *axSession) foregroundApps() []axApp {
	a.appMu.Lock()
	defer a.appMu.Unlock()
	var out []axApp
	for _, app := range a.apps {
		if app.state == appForeground {
			out = append(out, *app)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].seq > out[j].seq })
	return out
}

// handlePID is the process that owns an element: the first four bytes of
// its handle, little endian (SpringBoard's elements start 25 00 00 00, and
// its pid is 37).
func handlePID(h []byte) uint64 {
	if len(h) < 4 {
		return 0
	}
	return uint64(h[0]) | uint64(h[1])<<8 | uint64(h[2])<<16 | uint64(h[3])<<24
}

// foreground returns the app the person sees: the one that most recently
// came to the foreground and is still there, else SpringBoard.
func (a *axSession) foreground() (name, id string) {
	a.appMu.Lock()
	defer a.appMu.Unlock()
	var best *axApp
	for _, app := range a.apps {
		if app.state != appForeground || axBackgroundUI[app.id] || app.id == "com.apple.springboard" {
			continue
		}
		if strings.Contains(app.id, ".") && strings.Count(app.id, ".") > 3 {
			continue // extensions and helpers, e.g. com.apple.WebKit.WebContent
		}
		if best == nil || app.seq > best.seq {
			best = app
		}
	}
	if best == nil {
		return "Home Screen", "com.apple.springboard"
	}
	return best.name, best.id
}

// lastChange is when the daemon last reported a screen or layout change.
func (a *axSession) lastChange() time.Time {
	n := a.changed.Load()
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n)
}

func moveOptions(dir int32) any {
	pass := func(v any) any {
		return nskeyedarchiver.NewNSMutableDictionary(map[string]any{"ObjectType": "passthrough", "Value": v})
	}
	return nskeyedarchiver.NewNSMutableDictionary(map[string]any{
		"ObjectType": "passthrough",
		"Value": nskeyedarchiver.NewNSMutableDictionary(map[string]any{
			"allowNonAX":        pass(false),
			"direction":         pass(dir),
			"includeContainers": pass(false),
		}),
	})
}

func elementArg(handle []byte) any {
	return nskeyedarchiver.NewNSMutableDictionary(map[string]any{
		"ObjectType": "AXAuditElement_v1",
		"Value": nskeyedarchiver.NewNSMutableDictionary(map[string]any{
			"ObjectType": "passthrough",
			"Value": nskeyedarchiver.NewNSMutableDictionary(map[string]any{
				"PlatformElementValue_v1": nskeyedarchiver.NewNSMutableDictionary(map[string]any{
					"ObjectType": "passthrough", "Value": handle,
				}),
			}),
		}),
	})
}

func attributeArg(name string) any {
	return nskeyedarchiver.NewNSMutableDictionary(map[string]any{
		"ObjectType": "AXAuditElementAttribute_v1",
		"Value": nskeyedarchiver.NewNSMutableDictionary(map[string]any{
			"ObjectType": "passthrough",
			"Value": nskeyedarchiver.NewNSMutableDictionary(map[string]any{
				"AttributeNameValue_v1": nskeyedarchiver.NewNSMutableDictionary(map[string]any{
					"ObjectType": "passthrough", "Value": name,
				}),
			}),
		}),
	})
}

// passthrough unwraps {"ObjectType": ..., "Value": v} layers.
func passthrough(v any) any {
	for {
		m, ok := v.(map[string]any)
		if !ok {
			return v
		}
		inner, ok := m["Value"]
		if !ok {
			return v
		}
		v = inner
	}
}

// field reads key from an inspector object, unwrapping passthrough layers.
func field(v any, key string) any {
	for i := 0; i < 4; i++ {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		if x, ok := m[key]; ok {
			return passthrough(x)
		}
		v = m["Value"]
	}
	return nil
}

func findHandle(v any) []byte {
	switch t := v.(type) {
	case map[string]any:
		if pe, ok := t["PlatformElementValue_v1"]; ok {
			if b, ok := passthrough(pe).([]byte); ok {
				return b
			}
		}
		for _, x := range t {
			if b := findHandle(x); b != nil {
				return b
			}
		}
	case []any:
		for _, x := range t {
			if b := findHandle(x); b != nil {
				return b
			}
		}
	}
	return nil
}

func parseFocus(msg dtx.Message) (axNode, bool) {
	v := unarchiveArg(msg)
	n := axNode{handle: findHandle(v)}
	if n.handle == nil {
		return n, false
	}
	n.caption, _ = field(v, "CaptionTextValue_v1").(string)
	if n.caption == "" {
		n.caption, _ = field(v, "SpokenDescriptionValue_v1").(string)
	}
	if el := field(v, "ElementValue_v1"); el != nil {
		n.ident, _ = field(el, "AccessibilityIdentifier_v1").(string)
	}
	return n, true
}

// recvFocus waits for the next focus change.
func (a *axSession) recvFocus(ctx context.Context, wait time.Duration) (dtx.Message, error) {
	c, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	return a.ch.ReceiveMethodCallWithTimeout(c, selElementChanged)
}

// drain discards focus changes left over from earlier moves.
func (a *axSession) drain(ctx context.Context) {
	for {
		if _, err := a.recvFocus(ctx, 30*time.Millisecond); err != nil {
			return
		}
	}
}

// walkLimits bound a walk.
type walkLimits struct {
	MaxElements int
	Budget      time.Duration
	Batch       int
}

var errAXWalk = errors.New("accessibility walk")

// walk visits every element on the screen in reading order, starting from
// the first, until the focus comes back to an element it saw. Attributes
// are read for each element as soon as it arrives, while the daemon still
// has it cached.
func (a *axSession) walk(ctx context.Context, lim walkLimits) ([]axNode, bool, error) {
	a.walkMu.Lock()
	defer a.walkMu.Unlock()
	if lim.Batch <= 0 {
		lim.Batch = 8
	}
	deadline := time.Now().Add(lim.Budget)
	a.drain(ctx)
	// The focused element shows a focus ring on the phone (and in the
	// video) until the preview is cleared, however the walk ends.
	defer func() {
		a.ch.MethodCallAsync("deviceInspectorPreviewOnElement:", nskeyedarchiver.NewNSNull())
		a.ch.MethodCallAsync("deviceInspectorShowVisuals:", false)
	}()

	a.ch.MethodCallAsync("deviceInspectorFocusOnElement:", nskeyedarchiver.NewNSNull())
	if _, err := a.recvFocus(ctx, 3*time.Second); err != nil {
		return nil, false, fmt.Errorf("%w: focus: %v", errAXWalk, err)
	}
	a.ch.MethodCallAsync("deviceInspectorMoveWithOptions:", moveOptions(axFirst))
	msg, err := a.recvFocus(ctx, 3*time.Second)
	if err != nil {
		return nil, false, fmt.Errorf("%w: first element: %v", errAXWalk, err)
	}
	first, ok := parseFocus(msg)
	if !ok {
		return nil, false, fmt.Errorf("%w: first element has no handle", errAXWalk)
	}

	// The daemon answers attribute queries only for the element holding the
	// focus, so reading them would cost a round trip per element on top of
	// the move. The walk keeps the moves pipelined and takes everything from
	// the captions instead.
	nodes := []*axNode{&first}
	key := func(n axNode) string { return string(n.handle) + "\x00" + n.caption }
	seen := map[string]bool{key(first): true}

	pending := 0
	complete := false
	for !complete {
		if len(nodes) >= lim.MaxElements || time.Now().After(deadline) {
			break
		}
		for i := 0; i < lim.Batch; i++ {
			a.ch.MethodCallAsync("deviceInspectorMoveWithOptions:", moveOptions(axNext))
			pending++
		}
		for pending > 0 {
			msg, err := a.recvFocus(ctx, 2*time.Second)
			if err != nil {
				// The focus did not move: a single-element screen, or the
				// end of what the inspector can reach.
				pending = 0
				complete = true
				break
			}
			pending--
			n, ok := parseFocus(msg)
			if !ok || complete {
				continue
			}
			if seen[key(n)] {
				complete = true
				continue
			}
			seen[key(n)] = true
			np := &n
			nodes = append(nodes, np)
		}
	}
	// Let the extra moves of the last batch land, so the next walk starts
	// clean.
	for ; pending > 0; pending-- {
		if _, err := a.recvFocus(ctx, 300*time.Millisecond); err != nil {
			break
		}
	}
	out := make([]axNode, len(nodes))
	for i, n := range nodes {
		out[i] = *n
	}
	return out, complete, ctx.Err()
}

// readAttributes reads the label, value and traits of an element. The daemon
// answers only while the element holds the inspector focus.
func (a *axSession) readAttributes(ctx context.Context, n *axNode) {
	get := func(attr string) any {
		c, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		resp, err := a.ch.MethodCallWithContext(c, "deviceElement:valueForAttribute:", elementArg(n.handle), attributeArg(attr))
		if err != nil || len(resp.Payload) == 0 {
			return nil
		}
		return passthrough(resp.Payload[0])
	}
	var wg sync.WaitGroup
	var label, value, traits, names any
	wg.Add(4)
	go func() { defer wg.Done(); label = get("Label") }()
	go func() { defer wg.Done(); value = get("Value") }()
	go func() { defer wg.Done(); traits = get("Traits") }()
	go func() { defer wg.Done(); names = get("TraitsHumanReadable") }()
	wg.Wait()
	n.label, _ = label.(string)
	n.value, _ = value.(string)
	n.traits = uint64(toInt64(traits))
	if list, ok := names.([]any); ok {
		for _, x := range list {
			if s, ok := passthrough(x).(string); ok && s != "" {
				n.traitNames = append(n.traitNames, s)
			}
		}
	}
}

func (a *axSession) Close() error {
	a.once.Do(func() {
		a.ch.MethodCallAsync("deviceInspectorSetMonitoredEventType:", uint64(0))
		a.ch.MethodCallAsync("deviceSetAppMonitoringEnabled:", false)
		a.ch.MethodCallAsync("devicePerformFinalCleanup")
		time.Sleep(100 * time.Millisecond)
		close(a.done)
		a.conn.Close()
	})
	return nil
}
