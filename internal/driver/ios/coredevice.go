package ios

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	goios "github.com/danielpaulus/go-ios/ios"
	"github.com/danielpaulus/go-ios/ios/xpc"
	"github.com/google/uuid"
)

// cdService is a connection to one CoreDevice RemoteXPC service. Requests run
// one at a time: nothing in the framing pairs a reply with its request. A
// request that fails drops the connection, and the next one dials again:
// the phone closes these connections at times (when it locks, in a long
// session), and a service that stayed dropped made waking and unlocking
// from the computer fail until the phone was opened again.
type cdService struct {
	name  string
	devID string
	entry goios.DeviceEntry

	mu     sync.Mutex
	conn   *xpc.Connection
	closed bool                                                     // Close ran: never dial again
	dial   func(goios.DeviceEntry, string) (*xpc.Connection, error) // for tests
}

func dialCoreDevice(entry goios.DeviceEntry, name string) (*cdService, error) {
	conn, err := dialXPCConn(entry, name)
	if err != nil {
		return nil, err
	}
	return &cdService{name: name, devID: uuid.New().String(), entry: entry, conn: conn}, nil
}

// dialTimeout bounds connecting to a service. A service that is stuck on the
// device can accept the connection and never finish the RemoteXPC
// handshake, and nothing else would end the wait.
const dialTimeout = 10 * time.Second

func dialXPCConn(entry goios.DeviceEntry, name string) (*xpc.Connection, error) {
	type result struct {
		c   *xpc.Connection
		err error
	}
	ch := make(chan result, 1)
	go func() {
		c, err := goios.ConnectToXpcServiceTunnelIface(entry, name)
		ch <- result{c, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			return nil, fmt.Errorf("connect %s: %w", name, r.err)
		}
		return r.c, nil
	case <-time.After(dialTimeout):
		go func() {
			if r := <-ch; r.c != nil {
				r.c.Close()
			}
		}()
		return nil, fmt.Errorf("connect %s: no answer in %v", name, dialTimeout)
	}
}

// CoreDeviceError is an error the device reported for a request.
type CoreDeviceError struct {
	Code   int64
	Domain string
	Desc   string
}

func (e *CoreDeviceError) Error() string {
	if e.Desc != "" {
		return fmt.Sprintf("%s (%s %d)", e.Desc, e.Domain, e.Code)
	}
	return fmt.Sprintf("CoreDevice error %s %d", e.Domain, e.Code)
}

// invoke sends one request and waits for its reply. A cancelled or failed
// exchange closes the connection, because its stream position is unknown.
func (s *cdService) invoke(ctx context.Context, feature, action string, input map[string]any) (map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn == nil {
		if s.closed {
			return nil, fmt.Errorf("%s: connection closed", s.name)
		}
		dial := s.dial
		if dial == nil {
			dial = dialXPCConn
		}
		c, err := dial(s.entry, s.name)
		if err != nil {
			return nil, err
		}
		s.conn = c
	}
	conn := s.conn
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer func() {
		if !stop() {
			s.drop() // ctx closed the connection; the next request reconnects
		}
	}()

	req := coreDeviceRequest(s.devID, feature, action, input)
	if err := conn.Send(req, xpc.HeartbeatRequestFlag); err != nil {
		s.drop()
		return nil, fmt.Errorf("%s: send %s: %w", s.name, feature, ctxErr(ctx, err))
	}
	resp, err := conn.ReceiveOnServerClientStream()
	if err != nil {
		s.drop()
		return nil, fmt.Errorf("%s: %s: %w", s.name, feature, ctxErr(ctx, err))
	}
	if out, ok := resp["CoreDevice.output"].(map[string]any); ok {
		return out, nil
	}
	if e, ok := resp["CoreDevice.error"].(map[string]any); ok {
		return nil, decodeCoreDeviceError(e)
	}
	return nil, fmt.Errorf("%s: unexpected reply to %s: %v", s.name, feature, resp)
}

// coreDeviceRequest builds a CoreDevice invocation. It always announces the
// newer protocol versions: some features (the lock state, the display
// service) refuse the old ones.
func coreDeviceRequest(devID, feature, action string, input map[string]any) map[string]any {
	if input == nil {
		input = map[string]any{}
	}
	req := map[string]any{
		"CoreDevice.CoreDeviceDDIProtocolVersion": int64(2),
		"CoreDevice.action":                       map[string]any{},
		"CoreDevice.coreDeviceVersion": map[string]any{
			"components":              []any{uint64(629), uint64(3)},
			"originalComponentsCount": int64(2),
			"stringValue":             "629.3",
		},
		"CoreDevice.deviceIdentifier":     devID,
		"CoreDevice.featureIdentifier":    feature,
		"CoreDevice.input":                input,
		"CoreDevice.invocationIdentifier": uuid.New().String(),
	}
	if action != "" {
		req["CoreDevice.actionIdentifier"] = action
	}
	return req
}

func decodeCoreDeviceError(e map[string]any) error {
	ce := &CoreDeviceError{}
	if v, ok := e["code"].(int64); ok {
		ce.Code = v
	} else if v, ok := e["code"].(uint64); ok {
		ce.Code = int64(v)
	}
	ce.Domain, _ = e["domain"].(string)
	if ui, ok := e["userInfo"].(map[string]any); ok {
		for _, k := range []string{"NSLocalizedDescription", "NSLocalizedFailureReason", "NSDebugDescription"} {
			if d, ok := ui[k].(map[string]any); ok {
				if s, ok := d["string"].(string); ok && s != "" {
					ce.Desc = s
					break
				}
			}
			if s, ok := ui[k].(string); ok && s != "" {
				ce.Desc = s
				break
			}
		}
	}
	if ce.Desc == "" && ce.Domain == "" {
		return fmt.Errorf("CoreDevice error: %v", e)
	}
	return ce
}

func ctxErr(ctx context.Context, err error) error {
	if c := ctx.Err(); c != nil {
		return errors.Join(c, err)
	}
	return err
}

func (s *cdService) drop() {
	if s.conn != nil {
		s.conn.Close()
		s.conn = nil
	}
}

func (s *cdService) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	s.drop()
	return nil
}

// isClosedErr reports whether err looks like the device closing the channel.
func isClosedErr(err error) bool {
	if err == nil {
		return false
	}
	m := err.Error()
	// go-ios's HTTP/2 layer reports a peer that shuts the connection down
	// as "received GOAWAY".
	for _, s := range []string{"EOF", "closed", "reset", "GOAWAY", "broken pipe"} {
		if strings.Contains(m, s) {
			return true
		}
	}
	return false
}
