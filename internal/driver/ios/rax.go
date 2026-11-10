package ios

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	dtx "github.com/danielpaulus/go-ios/ios/dtx_codec"
	"github.com/danielpaulus/go-ios/ios/nskeyedarchiver"

	goios "github.com/danielpaulus/go-ios/ios"
)

// raxService is the accessibility daemon's service that Xcode 27's Device
// Hub uses to make a mirrored iPhone readable by VoiceOver on the Mac. It
// speaks AccessibilityPlatformTranslation: the host sends archived
// AXPTranslatorRequests with processDataFromHost: and the phone answers
// with archived AXPTranslatorResponses through processDataFromRemoteDevice:,
// element frames included. See docs/notes/ios.md.
const raxService = "com.apple.accessibility.axAuditDaemon.remoteAXService"

const (
	// raxHostAPIVersion is what Xcode 27's AXAuditRemoteDevice answers when
	// the phone asks for hostAPIVersion.
	raxHostAPIVersion = 7
	// raxClientType is the cached tree client type Xcode uses.
	raxClientType = 2
	// raxRequestTimeout bounds one request; answers take 5–60 ms over USB.
	raxRequestTimeout = 2 * time.Second
)

// AXP request types, as iOS 27 answers them. The numbers are the cases of
// -[AXPTranslator processTranslatorRequest:] (macOS 27 ships the iOS
// translator too): 1 processApplicationObject:, 2 processAttributeRequest:,
// 3 processCanSetAttribute:, 4 processFrontMostApp:, 5
// processMultipleAttributeRequest:, 6 processHitTest: (parameters point,
// displayId, contextId), 7 processActionRequest:, 8 processSetAttribute:, 9
// processSupportedActions:, 10 processSupportsAttributes:, 11
// processAXTreeElements:.
const (
	axpReqAppObject    = 1 // an application's element by pid, parameters {"pid": n}
	axpReqAttribute    = 2 // one attribute of an element
	axpReqFrontmostApp = 4 // the frontmost application's element
	axpReqAttributes   = 5 // several attributes, parameters {"attributes": [...]}
	axpReqTreeDump     = 11
)

// AXP attribute types, named as AXPTranslatorRequest's description names
// them on macOS 27.
const (
	axpAttrActivationPoint  = 1
	axpAttrAttributedLabel  = 3
	axpAttrChildren         = 8
	axpAttrNavigationOrder  = 9  // children in VoiceOver's order
	axpAttrFocusedElement   = 19 // of an application: the element with keyboard focus
	axpAttrFrame            = 21 // screen coordinates, in points
	axpAttrHelp             = 23
	axpAttrIdentifier       = 25
	axpAttrIsElement        = 26
	axpAttrIsEnabled        = 27
	axpAttrIsFocused        = 28
	axpAttrIsSelected       = 30
	axpAttrIsVisible        = 32
	axpAttrLabel            = 33
	axpAttrPlaceholder      = 42
	axpAttrRole             = 45
	axpAttrValue            = 53
	axpAttrAutomationType   = 61 // XCUIElementType
	axpAttrValueDescription = 64
	axpAttrIsRemoteElement  = 66
	axpAttrRemoteElementPID = 69 // the process a remote element shows
	axpAttrTraits           = 77 // UIAccessibilityTraits, private bits included

	axpAttrApplicationOrientation = 129 // of an application: its interface orientation
)

// raxWalkAttrs are the attributes a walk asks for, for every element.
var raxWalkAttrs = []int{
	axpAttrChildren, axpAttrNavigationOrder, axpAttrFrame, axpAttrLabel, axpAttrValue, axpAttrIsElement,
	axpAttrIdentifier, axpAttrAutomationType, axpAttrTraits, axpAttrIsEnabled,
	axpAttrIsSelected, axpAttrIsFocused, axpAttrPlaceholder, axpAttrValueDescription,
	axpAttrIsVisible, axpAttrIsRemoteElement, axpAttrRemoteElementPID, axpAttrRole,
}

var errRAXTimeout = errors.New("remote accessibility: no answer")

// raxSession is one connection to raxService. Answers carry no request ID,
// so it sends one request at a time.
type raxSession struct {
	conn *dtx.Connection
	ch   *dtx.Channel
	done chan struct{}
	once sync.Once

	reqMu  sync.Mutex
	mu     sync.Mutex
	waiter chan *axpResponse
	broken bool // a request timed out; a late answer would be misread

	tree   *raxTree      // the last tree the phone pushed, under mu
	treeCh chan struct{} // closed when a new tree arrives, under mu

	// meta is what walks and second pushes said of each element, by
	// process and ID, under mu: a first push lacks it (see fill).
	meta map[[2]uint64]objMeta
}

// objMeta is what a first push leaves out of an element.
type objMeta struct {
	Type  int
	Ident string
}

// maxMeta bounds the elements a session remembers.
const maxMeta = 20000

// learn remembers the type and identifier of elements read whole: from a
// walk or a second push.
func (s *raxSession) learn(els []raxElement) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.meta == nil || len(s.meta)+len(els) > maxMeta {
		s.meta = make(map[[2]uint64]objMeta, len(els))
	}
	for _, e := range els {
		s.meta[[2]uint64{uint64(e.Object.PID), e.Object.ID}] = objMeta{Type: e.Type, Ident: e.Ident}
	}
}

// fill returns a copy of a first push's elements with the types and
// identifiers walks and second pushes gave them, and whether it knew every
// one. Elements seen before then read the same whichever source a read
// came from: their role, identifier and ancestry, and so their refs, do not
// change between a read from a first push and the next (field test item
// 13: refs flipped between two numbers, roles between button and text).
func (s *raxSession) fill(els []raxElement) ([]raxElement, bool) {
	out := append([]raxElement(nil), els...)
	s.mu.Lock()
	defer s.mu.Unlock()
	known := true
	for i := range out {
		e := &out[i]
		m, ok := s.meta[[2]uint64{uint64(e.Object.PID), e.Object.ID}]
		if !ok {
			known = false
			continue
		}
		if e.Type == 0 {
			e.Type = m.Type
		}
		if e.Ident == "" {
			e.Ident = m.Ident
		}
	}
	return out, known
}

// raxTree is a tree the phone pushed, in walk order.
type raxTree struct {
	els []raxElement
	at  time.Time
	// start is when the first push of this screen arrived: the phone
	// pushes a first tree after a change, then the same with more
	// attributes (AutomationType, Identifier), sometimes a second later.
	start  time.Time
	first  bool // the first push, which lacks those attributes
	appPID int
}

// unnamed reports whether some element on screen has nothing to name it
// by, which a first push may lack and its second push has.
func (t *raxTree) unnamed() bool {
	for _, e := range t.els[min(1, len(t.els)):] {
		if e.IsElement && e.HasFrame && e.Label == "" && e.Value == "" && e.Ident == "" && e.ValueText == "" && e.Placeholder == "" {
			return true
		}
	}
	return false
}

func dialRAX(ctx context.Context, entry goios.DeviceEntry) (*raxSession, error) {
	type result struct {
		c   *dtx.Connection
		err error
	}
	ch := make(chan result, 1)
	go func() {
		c, err := dtx.NewTunnelConnection(entry, raxService)
		ch <- result{c, err}
	}()
	var conn *dtx.Connection
	select {
	case r := <-ch:
		if r.err != nil {
			return nil, fmt.Errorf("connect %s: %w", raxService, r.err)
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
	s := &raxSession{conn: conn, ch: conn.GlobalChannel(), done: make(chan struct{}), treeCh: make(chan struct{})}
	conn.MessageDispatcher = s
	s.ch.RegisterMethodForRemote("hostAPIVersion")
	go s.answerVersion()
	s.ch.MethodCallAsync("_notifyOfPublishedCapabilities:", map[string]any{
		"com.apple.private.DTXBlockCompression": uint64(2),
		"com.apple.private.DTXConnection":       uint64(1),
	})
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := s.ch.MethodCallWithContext(cctx, "deviceAPIVersion"); err != nil {
		s.Close()
		return nil, fmt.Errorf("remote accessibility: %w", err)
	}
	// The phone's translator answers only for a client that needs
	// accessibility (and drops the connection when told it no longer does);
	// it then also pushes the screen's tree after every change.
	if err := s.ch.MethodCallAsync("clientNeedsAccessibility:", true); err != nil {
		s.Close()
		return nil, fmt.Errorf("remote accessibility: %w", err)
	}
	return s, nil
}

// answerVersion answers the phone's hostAPIVersion question as Xcode does.
func (s *raxSession) answerVersion() {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		select {
		case <-s.done:
		case <-s.conn.Closed():
		}
		cancel()
	}()
	for {
		msg, err := s.ch.ReceiveMethodCallWithTimeout(ctx, "hostAPIVersion")
		if err != nil {
			return
		}
		payload, err := nskeyedarchiver.ArchiveBin(uint64(raxHostAPIVersion))
		if err != nil {
			continue
		}
		b, err := dtx.Encode(msg.Identifier, msg.ConversationIndex+1, msg.ChannelCode, false, dtx.ResponseWithReturnValueInPayload, payload, dtx.NewPrimitiveDictionary())
		if err == nil {
			s.conn.Send(b)
		}
	}
}

// Dispatch implements dtx.Dispatcher for what the phone sends on its own:
// processDataFromRemoteDevice: with an archived AXPTranslatorResponse.
// go-ios acknowledges the call before handing it over.
func (s *raxSession) Dispatch(msg dtx.Message) {
	if len(msg.Payload) == 0 {
		return
	}
	if sel, _ := msg.Payload[0].(string); sel != "processDataFromRemoteDevice:" {
		return
	}
	resp := decodeRAXData(msg.Auxiliary.GetArguments())
	if resp == nil {
		return
	}
	if d, ok := resp.Result.(axpDict); ok && d["treeDump"] != nil {
		list, _ := d["treeDump"].([]any)
		if els := treeFromDump(list); len(els) > 0 {
			kind, _ := d["treeDumpType"].(string)
			now := time.Now()
			t := &raxTree{els: els, at: now, start: now, first: kind == "AXPTreeDumpTypeInitialDump", appPID: els[0].Object.PID}
			if !t.first {
				s.learn(els)
			}
			s.mu.Lock()
			if prev := s.tree; !t.first && prev != nil && prev.first && prev.appPID == t.appPID {
				t.start = prev.start
			}
			s.tree = t
			close(s.treeCh)
			s.treeCh = make(chan struct{})
			s.mu.Unlock()
		}
		return
	}
	if resp.Notification != 0 || resp.RequestType == axpReqTreeDump {
		return // a notification; the tree that follows a change is what counts
	}
	s.mu.Lock()
	w := s.waiter
	s.waiter = nil
	s.mu.Unlock()
	if w != nil {
		w <- resp // buffered
	}
}

func decodeRAXData(args []any) *axpResponse {
	if len(args) == 0 {
		return nil
	}
	b, ok := args[0].([]byte)
	if !ok {
		return nil
	}
	v, err := nskeyedarchiver.Unarchive(b)
	if err != nil || len(v) != 1 {
		return nil
	}
	inner, ok := v[0].([]byte)
	if !ok {
		return nil
	}
	x, err := unarchiveAXP(inner)
	if err != nil {
		return nil
	}
	r, _ := x.(*axpResponse)
	return r
}

// pushed returns the last tree the phone pushed, and a channel closed when
// the next one arrives.
func (s *raxSession) pushed() (*raxTree, <-chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tree, s.treeCh
}

// treeFromDump rebuilds a walk from a pushed tree: the phone pushes, after
// screen changes, the frontmost app's element (request type 4) and every
// element on screen with its attributes (type 5), which is what a walk
// reads. An "additional data" push repeats them with more attributes.
func treeFromDump(list []any) []raxElement {
	var app *axpObject
	attrs := map[[2]uint64]axpDict{}
	for _, x := range list {
		r, _ := x.(*axpResponse)
		if r == nil {
			continue
		}
		switch r.RequestType {
		case axpReqFrontmostApp:
			app, _ = r.Result.(*axpObject)
		case axpReqAttributes:
			if ad, ok := r.Result.(axpDict); ok && r.Object != nil {
				attrs[[2]uint64{uint64(r.Object.PID), r.Object.ID}] = ad
			}
		}
	}
	if app == nil {
		return nil
	}
	type item struct {
		o             *axpObject
		parent, depth int
	}
	stack := []item{{app, -1, 0}}
	seen := map[[2]uint64]bool{}
	var out []raxElement
	for len(stack) > 0 {
		it := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		key := [2]uint64{uint64(it.o.PID), it.o.ID}
		ad, ok := attrs[key]
		if seen[key] || !ok || it.depth > 40 {
			continue
		}
		seen[key] = true
		e, kids := raxElementFrom(it.o, ad, it.parent, it.depth)
		out = append(out, e)
		idx := len(out) - 1
		for i := len(kids) - 1; i >= 0; i-- {
			stack = append(stack, item{kids[i], idx, it.depth + 1})
		}
	}
	return out
}

func (s *raxSession) alive() bool {
	s.mu.Lock()
	broken := s.broken
	s.mu.Unlock()
	if broken {
		return false
	}
	select {
	case <-s.conn.Closed():
		return false
	case <-s.done:
		return false
	default:
		return true
	}
}

// request sends one request and waits for its answer.
func (s *raxSession) request(ctx context.Context, r axpRequest) (*axpResponse, error) {
	b, err := archiveAXPRequest(r)
	if err != nil {
		return nil, err
	}
	s.reqMu.Lock()
	defer s.reqMu.Unlock()
	w := make(chan *axpResponse, 1)
	s.mu.Lock()
	if s.broken {
		s.mu.Unlock()
		return nil, errRAXTimeout
	}
	s.waiter = w
	s.mu.Unlock()
	fail := func(err error) (*axpResponse, error) {
		s.mu.Lock()
		s.waiter = nil
		s.broken = true
		s.mu.Unlock()
		return nil, err
	}
	if err := s.ch.MethodCallAsync("processDataFromHost:", b); err != nil {
		return fail(err)
	}
	t := time.NewTimer(raxRequestTimeout)
	defer t.Stop()
	select {
	case resp := <-w:
		if resp.RequestType != r.Type {
			return fail(fmt.Errorf("remote accessibility: answer to request type %d, want %d", resp.RequestType, r.Type))
		}
		return resp, nil
	case <-t.C:
		return fail(errRAXTimeout)
	case <-ctx.Done():
		return fail(ctx.Err())
	case <-s.conn.Closed():
		return fail(fmt.Errorf("remote accessibility: %w", errClosed(s.conn)))
	}
}

func errClosed(c *dtx.Connection) error {
	if err := c.Err(); err != nil {
		return err
	}
	return errors.New("connection closed")
}

// frontmostApp returns the frontmost application's element.
func (s *raxSession) frontmostApp(ctx context.Context) (*axpObject, error) {
	r, err := s.request(ctx, axpRequest{Type: axpReqFrontmostApp, Client: raxClientType})
	if err != nil {
		return nil, err
	}
	app, _ := r.Result.(*axpObject)
	if app == nil {
		return nil, errors.New("remote accessibility: no frontmost app")
	}
	return app, nil
}

// raxElement is one element of a walk.
type raxElement struct {
	Object      *axpObject
	Parent      int // index of the parent element, -1 for the app
	Depth       int
	Label       string
	Value       string
	Placeholder string
	ValueText   string // ValueDescription
	Ident       string
	IdentValue  bool // Value came from the identifier (a display, like Calculator's)
	Frame       axpRect
	HasFrame    bool
	Type        int // XCUIElementType; 0 in a first pushed tree
	AXRole      int // AccessibilityPlatformTranslation's role
	Traits      uint64
	IsElement   bool
	Enabled     bool
	Selected    bool
	Focused     bool
	Visible     bool
	Web         bool // an element of WebKit's web content process (see markWebContent)
	Remote      bool // a view showing another process's elements
	RemotePID   int  // that process
	Children    int
}

func raxElementFrom(o *axpObject, ad axpDict, parent, depth int) (raxElement, []*axpObject) {
	e := raxElement{Object: o, Parent: parent, Depth: depth, Enabled: true}
	e.Label, _ = ad[axpAttrLabel].(string)
	e.Value = axpString(ad[axpAttrValue])
	e.Placeholder, _ = ad[axpAttrPlaceholder].(string)
	e.ValueText, _ = ad[axpAttrValueDescription].(string)
	e.Ident, _ = ad[axpAttrIdentifier].(string)
	// Some apps carry the value in the identifier (Calculator's display is
	// "StandardInputView;value:144", with no label or value): keep the
	// name as the identifier, which stays the same, and the rest as the
	// value.
	if name, v, ok := strings.Cut(e.Ident, ";value:"); ok && name != "" {
		e.Ident = name
		if e.Value == "" {
			e.Value, e.IdentValue = v, true
		}
	}
	e.Frame, e.HasFrame = ad[axpAttrFrame].(axpRect)
	e.Type = toInt(ad[axpAttrAutomationType])
	e.AXRole = toInt(ad[axpAttrRole])
	if t, ok := ad[axpAttrTraits].(uint64); ok {
		e.Traits = t
	}
	e.IsElement, _ = ad[axpAttrIsElement].(bool)
	if v, ok := ad[axpAttrIsEnabled].(bool); ok {
		e.Enabled = v
	}
	e.Selected, _ = ad[axpAttrIsSelected].(bool)
	e.Focused, _ = ad[axpAttrIsFocused].(bool)
	e.Visible = true
	if v, ok := ad[axpAttrIsVisible].(bool); ok {
		e.Visible = v
	}
	e.Remote, _ = ad[axpAttrIsRemoteElement].(bool)
	e.RemotePID = toInt(ad[axpAttrRemoteElementPID])
	var kids []*axpObject
	list, ok := ad[axpAttrNavigationOrder].([]any)
	if !ok {
		list, _ = ad[axpAttrChildren].([]any)
	}
	for _, k := range list {
		if ko, ok := k.(*axpObject); ok {
			kids = append(kids, ko)
		}
	}
	e.Children = len(kids)
	return e, kids
}

// axpString renders a value attribute: text as is, numbers in decimal.
func axpString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case uint64, int64, float64, float32:
		return fmt.Sprint(x)
	case bool:
		if x {
			return "1"
		}
		return "0"
	}
	return ""
}

// walk reads the frontmost app's elements, depth first in the order the
// app lists them (its reading order), with raxWalkAttrs each. The first
// element is the app's own. The phone lists what is on screen, so a walk
// does not reach rows scrolled out of view.
func (s *raxSession) walk(ctx context.Context, lim walkLimits) ([]raxElement, bool, error) {
	return s.walkUntil(ctx, lim, nil, nil)
}

// walkUntil is walk that gives up between two requests once stop is set,
// so no answer is left in flight, and tells onApp the frontmost app first.
func (s *raxSession) walkUntil(ctx context.Context, lim walkLimits, stop *atomic.Bool, onApp func(*axpObject)) ([]raxElement, bool, error) {
	app, err := s.frontmostApp(ctx)
	if err != nil {
		return nil, false, err
	}
	if onApp != nil {
		onApp(app)
	}
	return s.walkFrom(ctx, app, lim, stop)
}

// appObject returns the application element of process pid, in front or
// not.
func (s *raxSession) appObject(ctx context.Context, pid int) (*axpObject, error) {
	r, err := s.request(ctx, axpRequest{Type: axpReqAppObject, Client: raxClientType, Params: map[string]any{"pid": pid}})
	if err != nil {
		return nil, err
	}
	app, _ := r.Result.(*axpObject)
	if app == nil {
		return nil, fmt.Errorf("remote accessibility: no application element for pid %d", pid)
	}
	return app, nil
}

// walkFrom walks the elements below root, root first, as walkUntil does.
func (s *raxSession) walkFrom(ctx context.Context, app *axpObject, lim walkLimits, stop *atomic.Bool) ([]raxElement, bool, error) {
	type item struct {
		o             *axpObject
		parent, depth int
	}
	stack := []item{{app, -1, 0}}
	deadline := time.Now().Add(lim.Budget)
	seen := map[[2]uint64]bool{}
	var out []raxElement
	for len(stack) > 0 {
		if len(out) >= lim.MaxElements || time.Now().After(deadline) || (stop != nil && stop.Load()) {
			return out, false, nil
		}
		it := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		key := [2]uint64{uint64(it.o.PID), it.o.ID}
		if seen[key] || it.depth > 40 {
			continue
		}
		seen[key] = true
		r, err := s.request(ctx, axpRequest{
			Type: axpReqAttributes, Client: raxClientType, Object: it.o,
			Params: map[string]any{"attributes": raxWalkAttrs},
		})
		if err != nil {
			return out, false, err
		}
		ad, _ := r.Result.(axpDict)
		e, kids := raxElementFrom(it.o, ad, it.parent, it.depth)
		out = append(out, e)
		s.learn(out[len(out)-1:])
		idx := len(out) - 1
		for i := len(kids) - 1; i >= 0; i-- {
			stack = append(stack, item{kids[i], idx, it.depth + 1})
		}
	}
	return out, true, nil
}

// focusedElement returns the frontmost app's focused element, or nil when
// nothing has keyboard focus.
func (s *raxSession) focusedElement(ctx context.Context) (*axpObject, error) {
	app, err := s.frontmostApp(ctx)
	if err != nil {
		return nil, err
	}
	r, err := s.request(ctx, axpRequest{Type: axpReqAttribute, Attribute: axpAttrFocusedElement, Object: app, Client: raxClientType})
	if err != nil {
		return nil, err
	}
	el, _ := r.Result.(*axpObject)
	return el, nil
}

// stringAttribute reads one attribute of an element as text.
func (s *raxSession) stringAttribute(ctx context.Context, o *axpObject, attr int) (string, error) {
	r, err := s.request(ctx, axpRequest{Type: axpReqAttribute, Attribute: attr, Object: o, Client: raxClientType})
	if err != nil {
		return "", err
	}
	return axpString(r.Result), nil
}

// Close tells the phone this client is gone and closes the connection.
func (s *raxSession) Close() {
	s.once.Do(func() {
		close(s.done)
		s.ch.MethodCallAsync("clientNeedsAccessibility:", false)
		s.conn.Close()
	})
}
