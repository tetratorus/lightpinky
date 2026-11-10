package ios

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	goios "github.com/danielpaulus/go-ios/ios"
	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv6"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"
)

// coreDeviceProxy is the lockdown service that carries the CoreDevice tunnel:
// a byte stream of bare IPv6 packets between the host and the device.
const coreDeviceProxy = "com.apple.internal.devicecompute.CoreDeviceProxy"

// requestedMTU is what the host asks for. The device grants its own value,
// which is used. Larger packets mean fewer IP headers and fewer copies for the
// video stream; the lockdown link is a TCP stream, so there is no path MTU.
const requestedMTU = 16000

// uploadMTU is what a tunnel that sends the device much data asks for: the
// developer disk image's 16 MB. With 16000, the iPhone 13 mini (iOS 27.0.1)
// took the first full-size packets of the upload and then sent nothing more
// on the tunnel, not even the ACKs for them, until its TCP gave up 18 s
// later (2026-11-08); the small packets of everything else Pinky sends had
// never shown it. go-ios's own tunnels ask for 1280.
const uploadMTU = 1280

// Tunnel is a userspace CoreDevice tunnel to one device. It runs a gVisor
// network stack in this process, so it needs no root and no utun interface,
// and unlike go-ios's own userspace tunnel it carries UDP as well as TCP: the
// display service streams RTP over UDP.
//
// go-ios dials services through a local forwarder that speaks go-ios's
// userspace-tunnel preamble (16-byte IPv6 address, 4-byte little-endian port).
// The forwarder listens on 127.0.0.1 only and forwards only to the device's
// tunnel address.
type Tunnel struct {
	udid    string
	stack   *stack.Stack
	link    *channel.Endpoint
	nic     tcpip.NICID
	host    netip.Addr // our address inside the tunnel
	device  netip.Addr // the device's address inside the tunnel
	rsdPort int
	mtu     int

	conn     io.ReadWriteCloser // the CoreDeviceProxy stream
	listener net.Listener       // the forwarder go-ios dials

	inPackets  atomic.Uint64
	inBytes    atomic.Uint64
	outPackets atomic.Uint64

	closeOnce sync.Once
	done      chan struct{}
	errMu     sync.Mutex
	err       error
}

// tunnelParams is the CDTunnel handshake answer.
type tunnelParams struct {
	ServerAddress    string `json:"serverAddress"`
	ServerRSDPort    uint64 `json:"serverRSDPort"`
	ClientParameters struct {
		Address string `json:"address"`
		Netmask string `json:"netmask"`
		MTU     uint64 `json:"mtu"`
	} `json:"clientParameters"`
}

// OpenTunnel starts a userspace tunnel to the device over usbmuxd. It works
// next to macOS's own CoreDevice tunnel (Xcode, devicectl): the device gives
// each tunnel its own addresses.
func OpenTunnel(ctx context.Context, entry goios.DeviceEntry) (*Tunnel, error) {
	return openTunnel(ctx, entry, requestedMTU)
}

// openTunnel is OpenTunnel asking the device for an MTU of mtu.
func openTunnel(ctx context.Context, entry goios.DeviceEntry, mtu int) (*Tunnel, error) {
	conn, err := goios.ConnectToService(entry, coreDeviceProxy)
	if err != nil {
		return nil, fmt.Errorf("start %s: %w", coreDeviceProxy, err)
	}
	// The handshake has no deadline of its own; closing the stream ends it.
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	params, err := tunnelHandshake(conn, mtu)
	if !stop() && err == nil {
		err = ctx.Err() // ctx ended as the handshake finished: conn is closed
	}
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("tunnel handshake: %w", err)
	}
	t, err := newTunnel(entry.Properties.SerialNumber, conn, params)
	if err != nil {
		conn.Close()
		return nil, err
	}
	return t, nil
}

// tunnelHandshake runs the CDTunnel exchange: "CDTunnel", a big-endian 16-bit
// length and a JSON body, in each direction.
func tunnelHandshake(rw io.ReadWriter, mtu int) (tunnelParams, error) {
	body, err := json.Marshal(map[string]any{"type": "clientHandshakeRequest", "mtu": mtu})
	if err != nil {
		return tunnelParams{}, err
	}
	var req bytes.Buffer
	req.WriteString("CDTunnel")
	binary.Write(&req, binary.BigEndian, uint16(len(body)))
	req.Write(body)
	if _, err := rw.Write(req.Bytes()); err != nil {
		return tunnelParams{}, err
	}
	hdr := make([]byte, 10)
	if _, err := io.ReadFull(rw, hdr); err != nil {
		return tunnelParams{}, fmt.Errorf("read header: %w", err)
	}
	if string(hdr[:8]) != "CDTunnel" {
		return tunnelParams{}, fmt.Errorf("unexpected header %q", hdr[:8])
	}
	resp := make([]byte, binary.BigEndian.Uint16(hdr[8:]))
	if _, err := io.ReadFull(rw, resp); err != nil {
		return tunnelParams{}, fmt.Errorf("read body: %w", err)
	}
	var p tunnelParams
	if err := json.Unmarshal(resp, &p); err != nil {
		return tunnelParams{}, fmt.Errorf("decode %q: %w", resp, err)
	}
	if p.ServerAddress == "" || p.ClientParameters.Address == "" || p.ServerRSDPort == 0 {
		return tunnelParams{}, fmt.Errorf("incomplete parameters %q", resp)
	}
	return p, nil
}

func newTunnel(udid string, conn io.ReadWriteCloser, p tunnelParams) (*Tunnel, error) {
	host, err := netip.ParseAddr(p.ClientParameters.Address)
	if err != nil {
		return nil, fmt.Errorf("tunnel client address: %w", err)
	}
	dev, err := netip.ParseAddr(p.ServerAddress)
	if err != nil {
		return nil, fmt.Errorf("tunnel server address: %w", err)
	}
	mtu := int(p.ClientParameters.MTU)
	if mtu <= 0 {
		mtu = 1280
	}

	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv6.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol},
	})
	// Room for video bursts and screenshot transfers.
	sack := tcpip.TCPSACKEnabled(true)
	s.SetTransportProtocolOption(tcp.ProtocolNumber, &sack)
	rcv := tcpip.TCPReceiveBufferSizeRangeOption{Min: 4 << 10, Default: 1 << 20, Max: 8 << 20}
	s.SetTransportProtocolOption(tcp.ProtocolNumber, &rcv)
	snd := tcpip.TCPSendBufferSizeRangeOption{Min: 4 << 10, Default: 1 << 20, Max: 8 << 20}
	s.SetTransportProtocolOption(tcp.ProtocolNumber, &snd)
	s.SetOption(tcpip.ReceiveBufferSizeOption{Min: 4 << 10, Default: 1 << 20, Max: 16 << 20})
	moderate := tcpip.TCPModerateReceiveBufferOption(true)
	s.SetTransportProtocolOption(tcp.ProtocolNumber, &moderate)

	link := channel.New(1024, uint32(mtu), "")
	nic := tcpip.NICID(1)
	if err := s.CreateNIC(nic, link); err != nil {
		return nil, fmt.Errorf("create NIC: %s", err)
	}
	addr := tcpip.ProtocolAddress{
		Protocol:          ipv6.ProtocolNumber,
		AddressWithPrefix: tcpip.AddressWithPrefix{Address: tcpip.AddrFrom16(host.As16()), PrefixLen: 64},
	}
	if err := s.AddProtocolAddress(nic, addr, stack.AddressProperties{}); err != nil {
		return nil, fmt.Errorf("add address: %s", err)
	}
	s.SetRouteTable([]tcpip.Route{{Destination: header.IPv6EmptySubnet, NIC: nic}})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		s.Close()
		return nil, fmt.Errorf("listen for go-ios: %w", err)
	}
	t := &Tunnel{
		udid: udid, stack: s, link: link, nic: nic, host: host, device: dev,
		rsdPort: int(p.ServerRSDPort), mtu: mtu, conn: conn, listener: ln,
		done: make(chan struct{}),
	}
	go t.inbound()
	go t.outbound()
	go t.serve()
	return t, nil
}

// inbound moves packets from the device into the stack. The lockdown stream
// has no packet boundaries, so each packet is cut by its IPv6 length field.
func (t *Tunnel) inbound() {
	br := bufio.NewReaderSize(t.conn, 256<<10)
	hdr := make([]byte, header.IPv6MinimumSize)
	for {
		if _, err := io.ReadFull(br, hdr); err != nil {
			t.fail(fmt.Errorf("tunnel read: %w", err))
			return
		}
		if hdr[0]>>4 != 6 {
			t.fail(fmt.Errorf("tunnel read: not an IPv6 packet (version %d)", hdr[0]>>4))
			return
		}
		n := int(binary.BigEndian.Uint16(hdr[4:6]))
		pkt := make([]byte, header.IPv6MinimumSize+n)
		copy(pkt, hdr)
		if _, err := io.ReadFull(br, pkt[header.IPv6MinimumSize:]); err != nil {
			t.fail(fmt.Errorf("tunnel read: %w", err))
			return
		}
		t.inPackets.Add(1)
		t.inBytes.Add(uint64(len(pkt)))
		pb := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(pkt)})
		t.link.InjectInbound(ipv6.ProtocolNumber, pb)
		pb.DecRef()
	}
}

// outbound moves packets from the stack to the device, one Write each.
func (t *Tunnel) outbound() {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-t.done
		cancel()
	}()
	for {
		pkt := t.link.ReadContext(ctx)
		if pkt == nil {
			return
		}
		buf := pkt.ToBuffer()
		data := buf.Flatten()
		buf.Release()
		pkt.DecRef()
		if _, err := t.conn.Write(data); err != nil {
			t.fail(fmt.Errorf("tunnel write: %w", err))
			return
		}
		t.outPackets.Add(1)
	}
}

// serve accepts go-ios's connections on the local forwarder.
func (t *Tunnel) serve() {
	for {
		c, err := t.listener.Accept()
		if err != nil {
			return
		}
		go t.forward(c)
	}
}

func (t *Tunnel) forward(c net.Conn) {
	defer c.Close()
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	var pre [20]byte
	if _, err := io.ReadFull(c, pre[:]); err != nil {
		return
	}
	c.SetReadDeadline(time.Time{})
	if netip.AddrFrom16([16]byte(pre[:16])) != t.device {
		return // only the device is reachable through the forwarder
	}
	port := int(binary.LittleEndian.Uint32(pre[16:]))
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	remote, err := t.DialTCP(ctx, port)
	cancel()
	if err != nil {
		return
	}
	defer remote.Close()
	errc := make(chan error, 2)
	go func() { _, err := io.Copy(remote, c); remote.CloseWrite(); errc <- err }()
	go func() { _, err := io.Copy(c, remote); errc <- err }()
	select {
	case <-errc:
	case <-t.done:
	}
}

func (t *Tunnel) fail(err error) {
	t.errMu.Lock()
	if t.err == nil {
		t.err = err
	}
	t.errMu.Unlock()
	t.Close()
}

// Err reports why the tunnel stopped, or nil while it runs.
func (t *Tunnel) Err() error {
	t.errMu.Lock()
	defer t.errMu.Unlock()
	return t.err
}

// Done is closed when the tunnel stops.
func (t *Tunnel) Done() <-chan struct{} { return t.done }

// HostAddr is this host's address inside the tunnel.
func (t *Tunnel) HostAddr() netip.Addr { return t.host }

// DeviceAddr is the device's address inside the tunnel.
func (t *Tunnel) DeviceAddr() netip.Addr { return t.device }

// RSDPort is the device's RemoteServiceDiscovery port.
func (t *Tunnel) RSDPort() int { return t.rsdPort }

// MTU is the MTU the device granted.
func (t *Tunnel) MTU() int { return t.mtu }

// DialTCP connects to a port on the device through the tunnel.
func (t *Tunnel) DialTCP(ctx context.Context, port int) (*gonet.TCPConn, error) {
	select {
	case <-t.done:
		return nil, errTunnelClosed(t.Err())
	default:
	}
	addr := tcpip.FullAddress{NIC: t.nic, Addr: tcpip.AddrFrom16(t.device.As16()), Port: uint16(port)}
	return gonet.DialContextTCP(ctx, t.stack, addr, ipv6.ProtocolNumber)
}

// ListenUDP binds a UDP socket on the host's tunnel address, for the device
// to send media to. bufSize is the receive buffer in bytes.
func (t *Tunnel) ListenUDP(bufSize int) (*gonet.UDPConn, error) {
	var wq waiter.Queue
	ep, terr := t.stack.NewEndpoint(udp.ProtocolNumber, ipv6.ProtocolNumber, &wq)
	if terr != nil {
		return nil, fmt.Errorf("udp endpoint: %s", terr)
	}
	ep.SocketOptions().SetReceiveBufferSize(int64(bufSize), true)
	if terr := ep.Bind(tcpip.FullAddress{NIC: t.nic, Addr: tcpip.AddrFrom16(t.host.As16())}); terr != nil {
		ep.Close()
		return nil, fmt.Errorf("udp bind: %s", terr)
	}
	return gonet.NewUDPConn(&wq, ep), nil
}

// Entry returns a go-ios device entry whose service connections go through
// this tunnel. rsd is the service list from the RSD handshake.
func (t *Tunnel) Entry(base goios.DeviceEntry, rsd goios.RsdPortProvider) goios.DeviceEntry {
	e := base
	e.Address = t.device.String()
	e.Rsd = rsd
	e.UserspaceTUN = true
	e.UserspaceTUNHost = "127.0.0.1"
	e.UserspaceTUNPort = t.listener.Addr().(*net.TCPAddr).Port
	return e
}

// TunnelStats are the tunnel's counters.
type TunnelStats struct {
	InPackets, InBytes, OutPackets uint64
	UDPReceived, UDPBufferDrops    uint64
	TCPRetransmits                 uint64
}

// Stats returns the tunnel's counters.
func (t *Tunnel) Stats() TunnelStats {
	st := t.stack.Stats()
	return TunnelStats{
		InPackets:      t.inPackets.Load(),
		InBytes:        t.inBytes.Load(),
		OutPackets:     t.outPackets.Load(),
		UDPReceived:    st.UDP.PacketsReceived.Value(),
		UDPBufferDrops: st.UDP.ReceiveBufferErrors.Value(),
		TCPRetransmits: st.TCP.Retransmits.Value(),
	}
}

// Close tears the tunnel down. It is safe to call more than once.
func (t *Tunnel) Close() error {
	t.closeOnce.Do(func() {
		close(t.done)
		t.listener.Close()
		t.conn.Close()
		t.link.Close()
		t.stack.Close()
	})
	return nil
}

func errTunnelClosed(cause error) error {
	if cause != nil {
		return fmt.Errorf("tunnel closed: %w", cause)
	}
	return errors.New("tunnel closed")
}
