package ios

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tetratorus/lightpinky/internal/driver"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
)

// rtpClock is the display stream's RTP clock rate. The answer does not
// state it; it was measured on iOS 27 (timestamps advance 400 per frame at
// 60 fps).
const rtpClock = 24000

// videoSilence is how long the stream may go without any packet before it
// counts as stopped. Video frames stop about 7 s after the screen last
// changed, but the phone's RTCP sender reports keep coming every second.
const videoSilence = 5 * time.Second

// media is the long-lived display stream of one device. It runs for the
// whole session, because HID input only works while a stream runs, and fans
// its packets out to Stream subscribers.
type media struct {
	l   *link
	svc *cdService
	udp *gonet.UDPConn
	ans streamAnswer
	dst *net.UDPAddr // where RTCP goes

	started  time.Time
	lastRTP  atomic.Int64 // unix nanos of the last RTP packet
	lastRTCP atomic.Int64 // unix nanos of the last RTCP packet from the phone
	rtcpIn   atomic.Int64 // RTCP packets received
	done     chan struct{}
	stopped  atomic.Bool
	errMu    sync.Mutex
	err      error

	// rxMu guards the receive statistics that receiver reports carry.
	rxMu      sync.Mutex
	lastSeq   uint16
	cycles    uint32
	baseSeq   uint32 // extended sequence number of the first packet
	received  uint32 // packets taken
	prior     lossPrior
	jitter    float64
	lastSR    uint32    // middle 32 bits of the last SR's NTP time
	lastSRAt  time.Time // when it arrived
	rxNeedKey bool      // a copy of needKey for the feedback loop

	// Receiver state, owned by the receive loop.
	dep         depacketizer
	au          [][]byte
	auTS        uint32
	auOpen      bool
	auBad       bool
	haveSeq     bool
	extTS       int64
	prevTS      uint32
	haveTS      bool
	needKey     bool
	codecNoted  bool   // wire.Codec is set
	transit     uint32 // arrival minus RTP time of the last packet, in clock units
	haveTransit bool
	srNTP       time.Time // wall clock of the last SR
	srTS        uint32    // RTP time of the last SR

	mu       sync.Mutex
	subs     map[*subscriber]struct{}
	codec    driver.Codec
	width    int
	height   int
	config   []byte // the latest parameter sets, Annex B
	ready    chan struct{}
	isReady  bool
	frameLog []frameMark // recent frames, for settle detection and stats
	exts     [][]byte    // RTP header extensions seen (noteExt)

	turn    atomic.Int32 // the interface's turn from the display's frame, for packets (setTurn)
	stats   mediaStats
	wire    wireStats // what came over the wire, for telling why no picture came
	lastFIR time.Time // the last keyframe request

	display int // the display id the stream was asked for
}

type frameMark struct {
	at   time.Time
	size int
	key  bool
}

// mediaStats are counters since the stream started.
type mediaStats struct {
	Packets    int
	Bytes      int64
	Frames     int
	KeyFrames  int
	Lost       int // RTP packets missing
	Dropped    int // access units dropped as incomplete
	FIRs       int
	SRDelay    time.Duration // one-way delay estimated from RTCP SR, if any
	SRDelaySet bool
}

// wireStats are what came over the wire since the stream started, beyond
// mediaStats: when no picture comes, they tell a phone that sent nothing
// from one that sent something Pinky could not read.
type wireStats struct {
	NotRTP       int           // packets that did not parse as RTP
	PayloadTypes map[uint8]int // RTP packets by payload type
	Codec        string        // what the payloads read as: hevc, h264, or "" before any did
	Unreadable   int           // payloads that read as neither codec
	Units        int           // access units completed
	NALTypes     map[int]int   // NAL units of completed access units, by type
	WaitedKey    int           // frames passed over while waiting for a key frame
	KeyNoConfig  int           // key frames that came before any parameter sets
	BadConfig    int           // parameter sets whose picture size did not parse
}

type subscriber struct {
	ch      chan driver.Packet
	needKey bool
	closed  bool
}

// startMedia starts a stream of one display (its CoreDevice id).
func startMedia(ctx context.Context, l *link, displayID int) (*media, error) {
	udp, err := l.tunnel.ListenUDP(8 << 20)
	if err != nil {
		return nil, err
	}
	svc, err := dialCoreDevice(l.entry, displayService)
	if err != nil {
		udp.Close()
		return nil, err
	}
	m := &media{
		l: l, svc: svc, udp: udp, done: make(chan struct{}), ready: make(chan struct{}),
		subs: map[*subscriber]struct{}{}, needKey: true, rxNeedKey: true, display: displayID,
	}
	port := udp.LocalAddr().(*net.UDPAddr).Port
	opts := defaultOfferOptions()
	opts.AllowRTCPFB = true // lets keyframe requests through
	sctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	m.started = time.Now()
	ans, err := startVideoStream(sctx, svc, l.tunnel.HostAddr().String(), port, l.tunnel.DeviceAddr().String(), displayID, opts)
	cancel()
	if err != nil {
		udp.Close()
		svc.Close()
		// The device may have started it anyway; stop on a fresh connection.
		c, cc := context.WithTimeout(context.Background(), 5*time.Second)
		stopMediaStreamsFresh(c, l.entry)
		cc()
		return nil, fmt.Errorf("start display stream (display %d): %w", displayID, err)
	}
	m.ans = ans
	if ans.SourcePort != 0 {
		m.dst = udpAddr(l.tunnel.DeviceAddr().String(), ans.SourcePort)
	}
	go m.receive()
	go m.feedback()
	return m, nil
}

// lastPacket is when the last RTP or RTCP packet arrived, or the start.
func (m *media) lastPacket() time.Time {
	t := max(m.lastRTP.Load(), m.lastRTCP.Load())
	if t == 0 {
		return m.started
	}
	return time.Unix(0, t)
}

// alive reports whether the phone is still sending: RTCP comes every second
// even when no frames do.
func (m *media) alive() bool {
	return !m.stopped.Load() && time.Since(m.lastPacket()) < 2500*time.Millisecond
}

// waitReady waits for the first keyframe, after which Size and the codec are
// known. When none comes in time it returns a *noVideoError, which tells
// what came instead.
func (m *media) waitReady(ctx context.Context) error {
	select {
	case <-m.ready:
		return nil
	case <-m.done:
		// The phone ended the stream before its first picture.
		return &noVideoError{cause: m.Err(), tries: []noVideoTry{m.noVideoTry()}}
	case <-ctx.Done():
		return &noVideoError{cause: ctx.Err(), tries: []noVideoTry{m.noVideoTry()}}
	}
}

func (m *media) fail(err error) {
	m.errMu.Lock()
	if m.err == nil {
		m.err = err
	}
	m.errMu.Unlock()
	m.close()
}

// Err says why the stream ended.
func (m *media) Err() error {
	m.errMu.Lock()
	defer m.errMu.Unlock()
	if m.err == nil && m.stopped.Load() {
		return errors.New("stream stopped")
	}
	return m.err
}

func (m *media) close() {
	if m.stopped.Swap(true) {
		return
	}
	close(m.done)
	m.udp.Close()
	m.mu.Lock()
	for s := range m.subs {
		if !s.closed {
			s.closed = true
			close(s.ch)
		}
	}
	m.subs = map[*subscriber]struct{}{}
	m.mu.Unlock()
}

// stop ends the stream on the device and closes everything.
func (m *media) stop(ctx context.Context) error {
	err := stopMediaStreams(ctx, m.svc)
	if err != nil {
		// The start connection is gone; try a fresh one so the device does
		// not stay "observed remotely".
		err = stopMediaStreamsFresh(ctx, m.l.entry)
	}
	m.close()
	m.svc.Close()
	return err
}

func (m *media) receive() {
	buf := make([]byte, 65536)
	for {
		n, _, err := m.udp.ReadFrom(buf)
		if err != nil {
			if !m.stopped.Load() {
				m.fail(fmt.Errorf("video receive: %w", err))
			}
			return
		}
		pkt := buf[:n]
		if isRTCP(pkt) {
			m.onRTCP(pkt)
			continue
		}
		p, err := parseRTP(pkt)
		if err != nil {
			m.mu.Lock()
			m.wire.NotRTP++
			m.mu.Unlock()
			continue
		}
		if p.Ext != nil {
			m.noteExt(p.Ext)
		}
		m.onRTP(p, n)
	}
}

func (m *media) onRTCP(b []byte) {
	m.lastRTCP.Store(time.Now().UnixNano())
	m.rtcpIn.Add(1)
	// Walk the compound packet for a sender report.
	for len(b) >= 4 {
		l := 4 * (int(binary.BigEndian.Uint16(b[2:4])) + 1)
		if l > len(b) {
			return
		}
		if b[1] == 203 {
			m.fail(errors.New("the phone ended the video stream (RTCP BYE)"))
			return
		}
		if b[1] == 200 && l >= 28 {
			sec := binary.BigEndian.Uint32(b[8:12])
			frac := binary.BigEndian.Uint32(b[12:16])
			m.rxMu.Lock()
			m.lastSR = sec<<16 | frac>>16
			m.lastSRAt = time.Now()
			m.rxMu.Unlock()
			m.srNTP = ntpTime(sec, frac)
			m.srTS = binary.BigEndian.Uint32(b[16:20])
		}
		b = b[l:]
	}
}

// ntpTime converts an NTP timestamp to wall clock time.
func ntpTime(sec, frac uint32) time.Time {
	const ntpUnixOffset = 2208988800
	ns := int64(frac) * 1e9 >> 32
	return time.Unix(int64(sec)-ntpUnixOffset, ns)
}

func (m *media) onRTP(p rtpPacket, size int) {
	now := time.Now()
	m.lastRTP.Store(now.UnixNano())
	m.mu.Lock()
	m.stats.Packets++
	m.stats.Bytes += int64(size)
	if m.wire.PayloadTypes == nil {
		m.wire.PayloadTypes = map[uint8]int{}
	}
	m.wire.PayloadTypes[p.PT]++
	m.mu.Unlock()
	ok, gap := m.track(p, now)
	if !ok {
		return
	}
	if m.auOpen && p.TS != m.auTS {
		// The marker went missing; with a gap, so may the frame's tail.
		if gap {
			m.auBad = true
		}
		m.finishAU()
	}
	if !m.auOpen {
		m.auOpen, m.auTS = true, p.TS
	}
	if gap {
		// The lost packets may also have been this frame's head.
		m.auBad = true
		m.dep.reset()
	}
	m.au = m.dep.push(p.Payload, m.au)
	if codec := m.dep.codec; codec == "" || !m.codecNoted {
		m.mu.Lock()
		if codec == "" {
			m.wire.Unreadable++
		} else {
			m.wire.Codec, m.codecNoted = codec, true
		}
		m.mu.Unlock()
	}
	if p.Marker {
		m.finishAU()
		m.sendReceipt(p.TS)
	}
}

// track updates sequence and jitter bookkeeping. It returns ok=false for a
// late or duplicate packet, which is dropped, and gap=true when packets went
// missing before this one.
//
// The tunnel rides a reliable link, so loss only happens when a buffer
// overflows under a burst. A frame that lost packets is dropped and the
// stream skips to the next keyframe, which it asks for.
func (m *media) track(p rtpPacket, now time.Time) (ok, gap bool) {
	m.rxMu.Lock()
	defer m.rxMu.Unlock()
	if m.haveSeq {
		d := p.Seq - (m.lastSeq + 1)
		if d >= 0x8000 {
			return false, false
		}
		if d != 0 {
			gap = true
			m.mu.Lock()
			m.stats.Lost += int(d)
			m.mu.Unlock()
		}
		if p.Seq < m.lastSeq {
			m.cycles += 1 << 16
		}
	}
	if !m.haveSeq {
		m.baseSeq = uint32(p.Seq)
	}
	m.lastSeq, m.haveSeq = p.Seq, true
	m.received++

	// Interarrival jitter (RFC 3550 A.8), in clock units, with 32-bit
	// arithmetic so that the RTP clock's wrap does not show up as jitter.
	ticks := uint64(float64(now.Sub(m.started).Nanoseconds()) * rtpClock / 1e9)
	transit := uint32(ticks) - p.TS
	if m.haveTransit {
		dt := int32(transit - m.transit)
		if dt < 0 {
			dt = -dt
		}
		m.jitter += (float64(dt) - m.jitter) / 16
	}
	m.transit, m.haveTransit = transit, true
	return true, gap
}

// finishAU turns the collected NAL units into packets.
func (m *media) finishAU() {
	nals, ts, bad := m.au, m.auTS, m.auBad
	m.au, m.auOpen, m.auBad = nil, false, false
	if len(nals) == 0 {
		return
	}
	codec := m.dep.codec
	if !m.haveTS {
		m.prevTS, m.haveTS = ts, true
	}
	m.extTS += int64(int32(ts - m.prevTS))
	m.prevTS = ts
	// Split before scaling: extTS*1e9 would overflow after about 107 hours.
	pts := time.Duration(m.extTS/rtpClock)*time.Second + time.Duration(m.extTS%rtpClock)*time.Second/rtpClock

	m.noteUnit(codec, nals)
	var cfg, frame [][]byte
	key := false
	for _, n := range nals {
		c, k := nalInfo(codec, n)
		switch {
		case c:
			cfg = append(cfg, n)
		default:
			if k {
				key = true
			}
			frame = append(frame, n)
		}
	}
	if bad {
		m.mu.Lock()
		m.stats.Dropped++
		m.mu.Unlock()
		m.setNeedKey(true)
		m.requestKeyFrame()
		return
	}
	if len(cfg) > 0 {
		m.setConfig(codec, cfg)
	}
	if m.needKey && !key {
		m.mu.Lock()
		m.wire.WaitedKey++
		m.mu.Unlock()
		m.requestKeyFrame()
		return
	}
	if key {
		m.setNeedKey(false)
	}
	if len(frame) == 0 {
		return
	}
	data := annexB(frame)
	m.mu.Lock()
	m.stats.Frames++
	if key {
		m.stats.KeyFrames++
	}
	if !m.srNTP.IsZero() {
		// The device's wall clock for this frame, from the last SR mapping,
		// against our arrival time. Only meaningful if both clocks are
		// synchronized (both use NTP), so treat it as an estimate.
		sent := m.srNTP.Add(time.Duration(int64(int32(ts-m.srTS)) * int64(time.Second) / rtpClock))
		m.stats.SRDelay = time.Since(sent)
		m.stats.SRDelaySet = true
	}
	m.frameLog = append(m.frameLog, frameMark{at: time.Now(), size: len(data), key: key})
	if len(m.frameLog) > 600 {
		m.frameLog = append(m.frameLog[:0], m.frameLog[len(m.frameLog)-300:]...)
	}
	if key && !m.isReady && m.config == nil {
		m.wire.KeyNoConfig++
	}
	if key && !m.isReady && m.config != nil {
		m.isReady = true
		close(m.ready)
	}
	cfgData := m.config
	turn := int(m.turn.Load())
	pkt := driver.Packet{Data: data, PTS: pts, KeyFrame: key, Turn: turn}
	for s := range m.subs {
		if s.closed {
			continue
		}
		if s.needKey {
			if !key {
				continue
			}
			if !m.offer(s, driver.Packet{Data: cfgData, PTS: pts, Config: true, Turn: turn}) {
				continue
			}
			s.needKey = false
		}
		if !m.offer(s, pkt) {
			s.needKey = true
		}
	}
	m.mu.Unlock()
}

func (m *media) setNeedKey(v bool) {
	m.needKey = v
	m.rxMu.Lock()
	m.rxNeedKey = v
	m.rxMu.Unlock()
}

// offer hands a packet to a subscriber without blocking the receiver. A
// subscriber that falls behind skips to the next keyframe.
func (m *media) offer(s *subscriber, p driver.Packet) bool {
	select {
	case s.ch <- p:
		return true
	default:
		go m.requestKeyFrame()
		return false
	}
}

func (m *media) setConfig(codec string, nals [][]byte) {
	w, h := 0, 0
	sps := false
	for _, n := range nals {
		var err error
		if codec == "hevc" && hevcType(n) == hevcSPS {
			sps = true
			w, h, err = hevcSPSSize(n)
		} else if codec == "h264" && n[0]&0x1f == h264SPS {
			sps = true
			w, h, err = h264SPSSize(n)
		}
		if err != nil {
			w, h = 0, 0
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if sps && w == 0 {
		m.wire.BadConfig++
	}
	m.config = annexB(nals)
	if codec == "hevc" {
		m.codec = driver.HEVC
	} else {
		m.codec = driver.H264
	}
	if w > 0 {
		m.width, m.height = w, h
	}
}

// requestKeyFrame sends an RTCP picture loss indication (RFC 4585), which
// makes the device send an IDR frame within about 70 ms. The device ignores
// a request that comes right after a keyframe, so feedback repeats it while
// someone still waits. At most one request per 500 ms.
func (m *media) requestKeyFrame() {
	m.mu.Lock()
	if time.Since(m.lastFIR) < 500*time.Millisecond || m.dst == nil {
		m.mu.Unlock()
		return
	}
	m.lastFIR = time.Now()
	m.stats.FIRs++
	m.mu.Unlock()
	b := make([]byte, 0, 12)
	b = append(b, 0x81, 206, 0, 2)
	b = binary.BigEndian.AppendUint32(b, m.ans.HostSSRC)
	b = binary.BigEndian.AppendUint32(b, m.ans.DeviceSSRC)
	m.udp.WriteTo(b, m.dst)
}

// waitingForKey reports whether a subscriber, or the receiver itself after
// loss, waits for a keyframe.
func (m *media) waitingForKey() bool {
	m.rxMu.Lock()
	need := m.rxNeedKey
	m.rxMu.Unlock()
	if need {
		return true
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for s := range m.subs {
		if s.needKey && !s.closed {
			return true
		}
	}
	return false
}

// sendReceipt acknowledges one frame, as Xcode does: an RTCP APP packet
// (subtype 0, name 0x00000005) carrying the frame's RTP time. The device
// paces its encoder by these.
func (m *media) sendReceipt(ts uint32) {
	if m.dst == nil {
		return
	}
	b := make([]byte, 0, 16)
	b = append(b, 0x80, 204, 0, 3)
	b = binary.BigEndian.AppendUint32(b, m.ans.HostSSRC)
	b = binary.BigEndian.AppendUint32(b, 5)
	b = binary.BigEndian.AppendUint32(b, ts)
	m.udp.WriteTo(b, m.dst)
}

// feedback sends receiver reports every second (the device sets an RTCP
// timeout, 10 s on iOS 27, and stops the stream without them) and repeats
// keyframe requests while someone waits for one.
func (m *media) feedback() {
	t := time.NewTicker(200 * time.Millisecond)
	defer t.Stop()
	var lastRR time.Time
	for {
		select {
		case <-m.done:
			return
		case <-t.C:
		}
		// Silence on both RTP and RTCP means the phone stopped the stream:
		// after a lost tunnel, a restarted media daemon, or receiver reports
		// that did not arrive for 10 s. Fail, so the device restarts it.
		if since := time.Since(m.lastPacket()); since > videoSilence {
			m.fail(fmt.Errorf("nothing from the phone's video stream for %v", since.Round(time.Second)))
			return
		}
		if m.dst == nil {
			continue
		}
		if time.Since(lastRR) >= time.Second {
			lastRR = time.Now()
			m.udp.WriteTo(m.receiverReport(), m.dst)
		}
		if m.waitingForKey() {
			m.mu.Lock()
			due := time.Since(m.lastFIR) >= 600*time.Millisecond
			m.mu.Unlock()
			if due {
				m.requestKeyFrame()
			}
		}
	}
}

func (m *media) receiverReport() []byte {
	m.rxMu.Lock()
	defer m.rxMu.Unlock()
	var dlsr uint32
	if !m.lastSRAt.IsZero() {
		dlsr = uint32(time.Since(m.lastSRAt).Seconds() * 65536)
	}
	b := make([]byte, 0, 44)
	b = append(b, 0x81, 201, 0, 7)
	b = binary.BigEndian.AppendUint32(b, m.ans.HostSSRC)
	b = binary.BigEndian.AppendUint32(b, m.ans.DeviceSSRC)
	var fraction uint8
	var lost int32
	if m.haveSeq {
		fraction, lost = m.prior.update(m.cycles|uint32(m.lastSeq), m.baseSeq, m.received)
	}
	b = append(b, fraction, byte(lost>>16), byte(lost>>8), byte(lost))
	b = binary.BigEndian.AppendUint32(b, m.cycles|uint32(m.lastSeq))
	b = binary.BigEndian.AppendUint32(b, uint32(m.jitter))
	b = binary.BigEndian.AppendUint32(b, m.lastSR)
	b = binary.BigEndian.AppendUint32(b, dlsr)
	// SDES with an empty CNAME, as Xcode sends.
	b = append(b, 0x81, 202, 0, 2)
	b = binary.BigEndian.AppendUint32(b, m.ans.HostSSRC)
	b = append(b, 1, 0, 0, 0)
	return b
}

// lossPrior is what the last receiver report counted, for the fraction
// lost since then.
type lossPrior struct{ expected, received uint32 }

// update returns RFC 3550's loss fields (A.3) and remembers the counts: the
// fraction of packets lost since the last report, in 256ths, and the
// packets lost since the stream started, clamped to 24 bits. The phone's
// encoder adapts its bitrate to them; reporting no loss at all, as Pinky
// did, kept it sending at its cap on links that could not carry it (Wi-Fi,
// where the tunnel backs up and the phone drops packets).
func (p *lossPrior) update(extMax, base, received uint32) (uint8, int32) {
	expected := extMax - base + 1
	lost := int64(expected) - int64(received)
	lost = max(min(lost, 0x7fffff), -0x800000)
	expInt := expected - p.expected
	recInt := received - p.received
	p.expected, p.received = expected, received
	lostInt := int64(expInt) - int64(recInt)
	var fraction uint8
	if expInt != 0 && lostInt > 0 {
		fraction = uint8(min(lostInt<<8/int64(expInt), 255))
	}
	return fraction, int32(lost)
}

// subscribe adds a Stream consumer. It starts at the next keyframe, which
// it asks the device for.
func (m *media) subscribe() *subscriber {
	s := &subscriber{ch: make(chan driver.Packet, 256), needKey: true}
	m.mu.Lock()
	if m.stopped.Load() {
		s.closed = true
		close(s.ch)
	} else {
		m.subs[s] = struct{}{}
	}
	m.mu.Unlock()
	m.requestKeyFrame()
	return s
}

func (m *media) unsubscribe(s *subscriber) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.subs[s]; ok {
		delete(m.subs, s)
		if !s.closed {
			s.closed = true
			close(s.ch)
		}
	}
}

func (m *media) size() (int, int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.width, m.height
}

// setTurn sets the turn the packets carry from now on: how far the
// interface is turned from the display's frame, which the picture is in.
func (m *media) setTurn(q quarter) { m.turn.Store(int32(q & 3)) }

// noteExt keeps the RTP header extensions the stream carried, the first
// few different ones, for diagnostics (iosctl orient).
func (m *media) noteExt(ext []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.exts {
		if bytes.Equal(e, ext) {
			return
		}
	}
	if len(m.exts) < 16 {
		m.exts = append(m.exts, bytes.Clone(ext))
	}
}

// headerExts returns the different RTP header extensions seen so far.
func (m *media) headerExts() [][]byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([][]byte(nil), m.exts...)
}

func (m *media) snapshot() mediaStats {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stats
}

// frames returns the frames that arrived since t.
func (m *media) framesSince(t time.Time) []frameMark {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []frameMark
	for i := len(m.frameLog) - 1; i >= 0 && m.frameLog[i].at.After(t); i-- {
		out = append(out, m.frameLog[i])
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// mediaStream is the driver.Stream a subscriber sees.
type mediaStream struct {
	m    *media
	s    *subscriber
	once sync.Once
}

func (st *mediaStream) Codec() driver.Codec {
	st.m.mu.Lock()
	defer st.m.mu.Unlock()
	return st.m.codec
}

func (st *mediaStream) Size() (int, int)              { return st.m.size() }
func (st *mediaStream) Packets() <-chan driver.Packet { return st.s.ch }
func (st *mediaStream) Err() error                    { return st.m.Err() }

func (st *mediaStream) Close() error {
	st.once.Do(func() { st.m.unsubscribe(st.s) })
	return nil
}

// RequestKeyFrame asks the phone for a key frame now (an RTCP FIR, at most
// one every 500 ms). The gateway shares one stream among the mirror and
// the recorder, so a member that needs a key frame (a viewer that fell
// behind, a recording after a pause) asks through it: the stream itself
// never sees that member fall behind.
func (st *mediaStream) RequestKeyFrame() { st.m.requestKeyFrame() }
