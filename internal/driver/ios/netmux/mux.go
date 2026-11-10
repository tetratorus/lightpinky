// Package netmux is a usbmuxd of Pinky's own for iPhones and iPads over
// Wi-Fi, for systems whose usbmuxd does not list them there by itself:
// Linux, where libimobiledevice's usbmuxd has no network support, and
// Windows, where Apple's lists a phone on Wi-Fi only while it is on a
// cable too.
//
// A Mux serves the usbmuxd protocol on a socket of its own. It hands
// everything about phones on a cable to the system's usbmuxd (the
// upstream) and adds the phones a Finder found on the network: ListDevices
// lists them as ConnectionType "Network", and Connect to one of them dials
// the phone's address on the port asked for, as macOS's usbmuxd does.
// go-ios, and so all of Pinky, uses it once USBMUXD_SOCKET_ADDRESS points
// at it.
package netmux

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/bits"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"howett.net/plist"
)

// Phone is a phone found on the network: its UDID and its address.
type Phone struct {
	UDID string
	Addr netip.Addr
}

// Mux is the usbmuxd. Its zero value is not ready; use New.
type Mux struct {
	upstream string // "unix:///var/run/usbmuxd"
	log      *slog.Logger

	mu     sync.Mutex
	phones map[string]netPhone // by UDID
	nextID int
	upDown bool            // the upstream did not answer the last ListDevices
	upNet  map[string]bool // UDIDs the upstream listed on the network last

	// DialTimeout bounds connecting to a phone on the network.
	DialTimeout time.Duration

	// Heartbeat keeps the heartbeat of every listed phone going
	// (heartbeat.go), through the usbmuxd go-ios uses, so set it once
	// USBMUXD_SOCKET_ADDRESS points at this Mux. HeartbeatFn connects to a
	// phone's heartbeat service instead; for tests.
	Heartbeat   bool
	HeartbeatFn func(p Phone, id int) (heartbeatConn, error)

	// PairDir is where the system's usbmuxd keeps pair records
	// (LockdownDir). The Finder reads them there while that usbmuxd is not
	// running: libimobiledevice's exits once no phone is on a cable
	// (usbmuxd --systemd), and udev starts it again when one is plugged in.
	PairDir string
	// ServePairDir answers clients' ReadPairRecord from PairDir too while
	// the system's usbmuxd is not running. Not on Windows (New), where the
	// Mux listens on a loopback port any local user can reach and the pair
	// records, the computer's private key among them, are readable by
	// administrators only: a Pinky run as one would hand them out. Apple's
	// usbmuxd keeps running there anyway.
	ServePairDir bool
}

// netPhone is a listed phone, the DeviceID it was given, and what ends
// its heartbeat.
type netPhone struct {
	Phone
	id   int
	stop context.CancelFunc
}

// firstNetworkID is the first DeviceID a phone on the network gets, far
// above the ones usbmuxd gives phones on a cable (1, 2, ...), as netmuxd
// does.
const firstNetworkID = 1 << 28

// New returns a Mux that hands phones on a cable to the usbmuxd at
// upstream, an address as go-ios writes them ("unix:///var/run/usbmuxd",
// "tcp://127.0.0.1:27015").
func New(upstream string, log *slog.Logger) *Mux {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Mux{upstream: upstream, log: log, phones: map[string]netPhone{}, nextID: firstNetworkID, DialTimeout: 5 * time.Second,
		PairDir: LockdownDir, ServePairDir: runtime.GOOS != "windows"}
}

// SetPhones sets the phones on the network. A phone keeps its DeviceID
// while it stays at its address; one that comes back, or moves, gets a new
// one, as with usbmuxd, so the phone's users see a new attach. The Mux
// keeps each listed phone's heartbeat going (heartbeat.go).
func (m *Mux) SetPhones(list []Phone) {
	m.mu.Lock()
	defer m.mu.Unlock()
	next := map[string]netPhone{}
	for _, p := range list {
		if old, ok := m.phones[p.UDID]; ok && old.Addr == p.Addr {
			next[p.UDID] = old
			continue
		}
		np := netPhone{Phone: p, id: m.nextID}
		m.nextID++
		if m.Heartbeat {
			ctx, cancel := context.WithCancel(context.Background())
			np.stop = cancel
			go m.heartbeat(ctx, p, np.id)
		}
		next[p.UDID] = np
	}
	for id, old := range m.phones {
		if np, ok := next[id]; (!ok || np.id != old.id) && old.stop != nil {
			old.stop()
		}
	}
	m.phones = next
}

// Phones returns the phones on the network, by UDID.
func (m *Mux) Phones() []Phone {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Phone
	for _, p := range m.phones {
		out = append(out, p.Phone)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UDID < out[j].UDID })
	return out
}

func (m *Mux) byID(id int) (netPhone, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, p := range m.phones {
		if p.id == id {
			return p, true
		}
	}
	return netPhone{}, false
}

// Serve answers the clients that connect to l until l is closed.
func (m *Mux) Serve(l net.Listener) error {
	for {
		c, err := l.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		go m.serve(c)
	}
}

// header is a usbmuxd message's header; the payload, a plist, follows.
type header struct {
	Length  uint32
	Version uint32
	Request uint32
	Tag     uint32
}

// maxMessage bounds a message's size, as go-ios does.
const maxMessage = 1 << 20

func readMsg(r io.Reader) (header, []byte, error) {
	var h header
	if err := binary.Read(r, binary.LittleEndian, &h); err != nil {
		return h, nil, err
	}
	if h.Length < 16 || h.Length > maxMessage {
		return h, nil, fmt.Errorf("netmux: message of %d bytes", h.Length)
	}
	body := make([]byte, h.Length-16)
	if _, err := io.ReadFull(r, body); err != nil {
		return h, nil, err
	}
	return h, body, nil
}

func writeMsg(w io.Writer, h header, body []byte) error {
	h.Length = 16 + uint32(len(body))
	var buf bytes.Buffer
	_ = binary.Write(&buf, binary.LittleEndian, h)
	buf.Write(body)
	_, err := w.Write(buf.Bytes())
	return err
}

func writePlist(w io.Writer, tag uint32, v any) error {
	body, err := plist.Marshal(v, plist.XMLFormat)
	if err != nil {
		return err
	}
	return writeMsg(w, header{Version: 1, Request: 8, Tag: tag}, body)
}

// result is usbmuxd's answer to a request: 0 is success.
type result struct {
	MessageType string
	Number      int
}

// usbmuxd's result numbers.
const (
	resultOK        = 0
	resultBadDevice = 2 // no such device or pair record
	resultRefused   = 3 // the phone refused the connection
)

// request is what Mux reads of a client's request.
type request struct {
	MessageType  string
	DeviceID     int
	PortNumber   uint16 // in network byte order, as usbmuxd takes it
	PairRecordID string // ReadPairRecord
}

// serve answers one client: requests until one turns the connection into
// a connection to a phone (Connect) or a stream of events (Listen).
func (m *Mux) serve(c net.Conn) {
	defer c.Close()
	var up net.Conn // the upstream connection for this client, once needed
	defer func() {
		if up != nil {
			up.Close()
		}
	}()
	upstream := func() (net.Conn, error) {
		if up == nil {
			conn, err := m.dialUpstream()
			if err != nil {
				return nil, err
			}
			up = conn
		}
		return up, nil
	}
	// relay sends the request to the upstream and its answer back.
	relay := func(h header, body []byte) (header, []byte, error) {
		u, err := upstream()
		if err != nil {
			return header{}, nil, err
		}
		if err := writeMsg(u, h, body); err != nil {
			return header{}, nil, err
		}
		rh, rbody, err := readMsg(u)
		if err != nil {
			return header{}, nil, err
		}
		return rh, rbody, writeMsg(c, rh, rbody)
	}
	for {
		h, body, err := readMsg(c)
		if err != nil {
			return
		}
		var req request
		if _, err := plist.Unmarshal(body, &req); err != nil || h.Request != 8 {
			return // only plist messages; usbmuxd's old binary protocol is not used
		}
		switch req.MessageType {
		case "ListDevices":
			if err := m.listDevices(c, h, body); err != nil {
				return // the client sees an error, as without a usbmuxd
			}
		case "Connect":
			if p, ok := m.byID(req.DeviceID); ok {
				m.connectNetwork(c, h.Tag, p, bits.ReverseBytes16(req.PortNumber))
				return
			}
			_, rbody, err := relay(h, body)
			if err != nil {
				return
			}
			var r result
			if _, err := plist.Unmarshal(rbody, &r); err == nil && r.MessageType == "Result" && r.Number == resultOK {
				pipe(c, up)
				return
			}
		case "Listen":
			// Phones on the network are not announced: Pinky asks for the
			// list (ListDevices) every few seconds.
			u, err := upstream()
			if err != nil {
				return
			}
			if err := writeMsg(u, h, body); err != nil {
				return
			}
			pipe(c, u)
			return
		case "ReadPairRecord":
			// The upstream keeps the pair records, also of phones on the
			// network; while it is not running they are read from PairDir
			// (ServePairDir).
			if _, _, err := relay(h, body); err != nil {
				if up != nil {
					up.Close()
					up = nil
				}
				if err := m.answerFromPairDir(c, h.Tag, req); err != nil {
					m.log.Debug("netmux: "+req.MessageType, "err", err)
					return
				}
			}
		default:
			// SavePairRecord, DeletePairRecord and the like: the upstream's.
			if _, _, err := relay(h, body); err != nil {
				return
			}
		}
	}
}

// listDevices answers ListDevices with the upstream's list and the phones
// on the network. While the upstream is not running (no phone on a cable),
// the list has the phones on the network only; with none, the client sees
// the error, as without a usbmuxd.
func (m *Mux) listDevices(c net.Conn, h header, body []byte) error {
	var list struct {
		DeviceList []map[string]any
	}
	rh, err := m.upstreamList(h, body, &list)
	m.mu.Lock()
	changed := m.upDown != (err != nil)
	m.upDown = err != nil
	m.mu.Unlock()
	if changed {
		// Pinky asks every 2 s: a line when it changes, not each time.
		m.log.Debug("netmux: the system's usbmuxd answers", "ok", err == nil, "err", err)
	}
	if err != nil {
		if len(m.Phones()) == 0 {
			return err
		}
		rh, list.DeviceList = header{Version: 1, Request: 8, Tag: h.Tag}, nil
	}
	// A usbmuxd that lists phones on the network itself (usbmuxd2,
	// netmuxd in its place) keeps its own entries.
	upNet := map[string]bool{}
	for _, d := range list.DeviceList {
		if props, ok := d["Properties"].(map[string]any); ok && props["ConnectionType"] == "Network" {
			if id, ok := props["SerialNumber"].(string); ok {
				upNet[id] = true
			}
		}
	}
	m.mu.Lock()
	m.upNet = upNet
	var phones []netPhone
	for _, p := range m.phones {
		if !upNet[p.UDID] {
			phones = append(phones, p)
		}
	}
	m.mu.Unlock()
	sort.Slice(phones, func(i, j int) bool { return phones[i].id < phones[j].id })
	for _, p := range phones {
		list.DeviceList = append(list.DeviceList, map[string]any{
			"DeviceID":    p.id,
			"MessageType": "Attached",
			"Properties": map[string]any{
				"ConnectionType": "Network",
				"DeviceID":       p.id,
				"SerialNumber":   p.UDID,
				"NetworkAddress": sockaddr(p.Addr),
			},
		})
	}
	if list.DeviceList == nil {
		list.DeviceList = []map[string]any{}
	}
	out, err := plist.Marshal(list, plist.XMLFormat)
	if err != nil {
		return err
	}
	return writeMsg(c, rh, out)
}

// upstreamListsOnNetwork reports whether the upstream listed the phone on
// the network itself, as of the last ListDevices.
func (m *Mux) upstreamListsOnNetwork(udid string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.upNet[udid]
}

// upstreamList asks the upstream for its list.
func (m *Mux) upstreamList(h header, body []byte, list any) (header, error) {
	u, err := m.dialUpstream()
	if err != nil {
		return header{}, err
	}
	defer u.Close()
	if err := writeMsg(u, h, body); err != nil {
		return header{}, err
	}
	rh, rbody, err := readMsg(u)
	if err != nil {
		return header{}, err
	}
	_, err = plist.Unmarshal(rbody, list)
	return rh, err
}

// answerFromPairDir answers ReadPairRecord from the pair records in
// PairDir, as the upstream would.
func (m *Mux) answerFromPairDir(c net.Conn, tag uint32, req request) error {
	if !m.ServePairDir {
		return writePlist(c, tag, result{MessageType: "Result", Number: resultBadDevice})
	}
	data, err := m.pairFile(req.PairRecordID)
	if err != nil {
		return writePlist(c, tag, result{MessageType: "Result", Number: resultBadDevice})
	}
	return writePlist(c, tag, map[string]any{"PairRecordData": data})
}

// pairFile reads a phone's pair record, by UDID, from PairDir.
func (m *Mux) pairFile(name string) ([]byte, error) {
	if name == "" || strings.ContainsAny(name, `/\`) || strings.HasPrefix(name, ".") {
		return nil, fmt.Errorf("netmux: bad pair record name %q", name)
	}
	return os.ReadFile(filepath.Join(m.PairDir, name+".plist"))
}

// sockaddr is the phone's address as macOS's usbmuxd gives it, a struct
// sockaddr_in (BSD layout: length, family, port, address).
func sockaddr(a netip.Addr) []byte {
	b := make([]byte, 16)
	b[0], b[1] = 16, 2
	ip := a.As4()
	copy(b[4:8], ip[:])
	return b
}

// connectNetwork connects the client to port on the phone, and then
// passes the bytes both ways.
func (m *Mux) connectNetwork(c net.Conn, tag uint32, p netPhone, port uint16) {
	addr := net.JoinHostPort(p.Addr.String(), strconv.Itoa(int(port)))
	conn, err := net.DialTimeout("tcp", addr, m.DialTimeout)
	if err != nil {
		m.log.Debug("netmux: cannot connect to the phone", "device", p.UDID, "addr", addr, "err", err)
		_ = writePlist(c, tag, result{MessageType: "Result", Number: resultRefused})
		return
	}
	defer conn.Close()
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true)
		_ = tc.SetKeepAlive(true)
		_ = tc.SetKeepAlivePeriod(15 * time.Second)
	}
	if err := writePlist(c, tag, result{MessageType: "Result", Number: resultOK}); err != nil {
		return
	}
	pipe(c, conn)
}

// pipe passes bytes both ways until either side ends.
func pipe(a, b net.Conn) {
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		done <- struct{}{}
	}
	go cp(a, b)
	go cp(b, a)
	<-done
	// One side ended: end the other, so its copy returns too.
	a.Close()
	b.Close()
	<-done
}

func (m *Mux) dialUpstream() (net.Conn, error) {
	network, addr := "unix", m.upstream
	switch {
	case strings.HasPrefix(addr, "unix://"):
		addr = strings.TrimPrefix(addr, "unix://")
	case strings.HasPrefix(addr, "tcp://"):
		network, addr = "tcp", strings.TrimPrefix(addr, "tcp://")
	}
	return net.DialTimeout(network, addr, 5*time.Second)
}
