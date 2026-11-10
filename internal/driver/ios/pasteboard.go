package ios

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	goios "github.com/danielpaulus/go-ios/ios"
	"github.com/danielpaulus/go-ios/ios/xpc"
	"github.com/google/uuid"
)

// The phone's pasteboard, through CoreDevice's pasteboard service (the DDI's
// dtpasteboardd). It speaks bare XPC dictionaries whose "command" field
// says what they are: PULL is answered by PULL_REPLY, SET by SET_REPLY.
// pymobiledevice3's pasteboard_service.py documents the format, reversed
// from Apple's CoreDeviceUtilities.
const pasteboardService = "com.apple.coredevice.pasteboardservice"

const generalPasteboard = "general"

// Data inclusion policies for PULL. allResolved carries every
// representation of every item; promiseSecondary only each item's first
// (primary) one and lists the others as promises; allPromised carries none.
const (
	pbAllResolved      = "allResolved"
	pbPromiseSecondary = "promiseSecondary"
	pbAllPromised      = "allPromised"
)

// pbRestrictedCode is the CoreDeviceError the service answers a PULL with
// when an item carries a marker type (Universal Clipboard's
// com.apple.is-remote-clipboard, org.nspasteboard.ConcealedType for
// passwords, the transient and auto-generated types): it never reads such
// items, so they cannot be saved.
const pbRestrictedCode = 26006

// errPBRestricted reports a pasteboard the service refuses to read.
var errPBRestricted = errors.New("the pasteboard holds an item from another device, a password or other transient content, which the phone does not let Pinky read")

// pbData is one representation of a pasteboard item.
type pbData struct {
	Data     []byte
	Promised bool // listed without its data
}

// pbItem is one pasteboard item: its types (UTIs) in order of preference,
// and the data of each.
type pbItem struct {
	Types []string
	Data  map[string]pbData
}

// pbSnapshot is the pasteboard's state.
type pbSnapshot struct {
	Items       []pbItem
	ChangeCount int64
	HasCount    bool
}

// complete reports whether every representation came with its data.
func (s *pbSnapshot) complete() bool {
	for _, it := range s.Items {
		for _, t := range it.Types {
			if d, ok := it.Data[t]; !ok || d.Promised {
				return false
			}
		}
	}
	return true
}

// wireItems encodes the items for SET, leaving out representations that
// came without data.
func (s *pbSnapshot) wireItems() []any {
	out := make([]any, 0, len(s.Items))
	for _, it := range s.Items {
		types := make([]any, 0, len(it.Types))
		data := map[string]any{}
		for _, t := range it.Types {
			d, ok := it.Data[t]
			if !ok || d.Promised {
				continue
			}
			types = append(types, t)
			data[t] = map[string]any{"data": d.Data}
		}
		if len(types) == 0 {
			continue
		}
		out = append(out, map[string]any{"types": types, "data": data})
	}
	return out
}

// textItems is a pasteboard holding text, the way UIKit's string setter
// writes it.
func textItems(text string) []any {
	const uti = "public.utf8-plain-text"
	return []any{map[string]any{
		"types": []any{uti},
		"data":  map[string]any{uti: map[string]any{"data": []byte(text)}},
	}}
}

// parsePBReply reads a PULL_REPLY or SET_REPLY. Errors the device reports
// come back as *CoreDeviceError, a refused read as errPBRestricted.
func parsePBReply(r map[string]any, want string) (*pbSnapshot, error) {
	if e, ok := r["error"].(map[string]any); ok {
		err := decodeCoreDeviceError(e)
		var ce *CoreDeviceError
		if errors.As(err, &ce) && ce.Code == pbRestrictedCode {
			return nil, errPBRestricted
		}
		return nil, fmt.Errorf("pasteboard: %w", err)
	}
	if c, _ := r["command"].(string); c != want {
		return nil, fmt.Errorf("pasteboard: got %q, want %s", c, want)
	}
	pb, _ := r["pasteboard"].(map[string]any)
	s := &pbSnapshot{}
	if md, ok := pb["metadata"].(map[string]any); ok {
		switch n := md["changeCount"].(type) {
		case int64:
			s.ChangeCount, s.HasCount = n, true
		case uint64:
			s.ChangeCount, s.HasCount = int64(n), true
		}
	}
	items, _ := pb["items"].([]any)
	for _, raw := range items {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		it := pbItem{Data: map[string]pbData{}}
		types, _ := m["types"].([]any)
		for _, t := range types {
			if s, ok := t.(string); ok {
				it.Types = append(it.Types, s)
			}
		}
		data, _ := m["data"].(map[string]any)
		for uti, v := range data {
			dm, _ := v.(map[string]any)
			if b, ok := dm["data"].([]byte); ok {
				it.Data[uti] = pbData{Data: b}
				continue
			}
			it.Data[uti] = pbData{Promised: true}
		}
		s.Items = append(s.Items, it)
	}
	return s, nil
}

// pbConn is a connection to the pasteboard service. Requests run one at a
// time; a request that fails or is cut short drops the connection, because
// its reply could still come and be read as the next one's.
type pbConn struct {
	entry goios.DeviceEntry
	nonce string // names Pinky's writes in their sourceMetadata
	seq   atomic.Int64

	mu   sync.Mutex
	conn *xpc.Connection
}

func newPBConn(entry goios.DeviceEntry) *pbConn {
	return &pbConn{entry: entry, nonce: "pinky-" + uuid.New().String()}
}

// connect dials the service if the connection is not up, so the first Type
// does not wait for it (the RemoteXPC handshake takes about half a second).
func (p *pbConn) connect() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.conn != nil {
		return nil
	}
	c, err := dialXPCConn(p.entry, pasteboardService)
	if err != nil {
		return err
	}
	p.conn = c
	return nil
}

// request sends msg and waits for the reply, connecting first when needed.
func (p *pbConn) request(ctx context.Context, msg map[string]any) (map[string]any, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.conn == nil {
		c, err := dialXPCConn(p.entry, pasteboardService)
		if err != nil {
			return nil, err
		}
		p.conn = c
	}
	conn := p.conn
	type result struct {
		m   map[string]any
		err error
	}
	ch := make(chan result, 1)
	go func() {
		if err := conn.Send(msg, xpc.HeartbeatRequestFlag); err != nil {
			ch <- result{err: err}
			return
		}
		m, err := conn.ReceiveOnServerClientStream()
		ch <- result{m, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			p.drop()
			return nil, fmt.Errorf("pasteboard: %w", r.err)
		}
		return r.m, nil
	case <-ctx.Done():
		p.drop()
		return nil, fmt.Errorf("pasteboard: %w", ctx.Err())
	}
}

func (p *pbConn) drop() {
	if p.conn != nil {
		p.conn.Close()
		p.conn = nil
	}
}

// Close closes the connection.
func (p *pbConn) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.drop()
	return nil
}

// pull reads the general pasteboard.
func (p *pbConn) pull(ctx context.Context, policy string) (*pbSnapshot, error) {
	r, err := p.request(ctx, map[string]any{
		"command":        "PULL",
		"pasteboardName": generalPasteboard,
		"dataPolicy":     map[string]any{policy: map[string]any{}},
	})
	if err != nil {
		return nil, err
	}
	return parsePBReply(r, "PULL_REPLY")
}

// set replaces the general pasteboard's items and returns its new state.
//
// It always sends sourceMetadata. The daemon writes with UIPasteboard's
// setItemProviders:localOnly:expirationDate: and passes localOnly only when
// the snapshot it writes has source metadata (as Xcode's host-to-device
// sync does): without it, the write would be offered to the owner's other
// devices through Universal Clipboard and replace their clipboards too.
func (p *pbConn) set(ctx context.Context, items []any) (*pbSnapshot, error) {
	r, err := p.request(ctx, map[string]any{
		"command":        "SET",
		"pasteboardName": generalPasteboard,
		"items":          items,
		"sourceMetadata": map[string]any{
			"nonce":          p.nonce,
			"pasteboardName": "pinky",
			"changeCount":    p.seq.Add(1),
		},
	})
	if err != nil {
		return nil, err
	}
	return parsePBReply(r, "SET_REPLY")
}
