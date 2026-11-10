// Package mdns finds DNS-SD services on the local network with multicast
// DNS (RFC 6762 and RFC 6763). Pinky uses it to find the wireless
// debugging services of Android phones itself, because the adb server's
// own discovery stops working after the computer's network changes (an
// interface comes or goes) and stays broken until the server restarts.
//
// A Browser only asks; it never answers or announces anything. It sends
// its questions from a port of its own with the unicast-response bit set,
// so responders answer it directly, and it listens on the mDNS group for
// the answers responders multicast and for the announcements of services
// that start. It binds that socket to the group's address, not to every
// address, so it does not take the unicast answers meant for the system's
// own responder (mDNSResponder on macOS, Avahi on Linux) that shares the
// port.
package mdns

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/dns/dnsmessage"
	"golang.org/x/net/ipv4"
)

// Group is the IPv4 mDNS group and port.
var Group = netip.MustParseAddrPort("224.0.0.251:5353")

// Errors Err reports.
var (
	// ErrNotAllowed means the system does not let this process use the
	// local network. On macOS that is the app's switch under System
	// Settings › Privacy & Security › Local Network.
	ErrNotAllowed = errors.New("mdns: not allowed to use the local network")
	// ErrNoNetwork means no network interface can send multicast.
	ErrNoNetwork = errors.New("mdns: no network interface to search")
)

// Service is a DNS-SD service instance found on the network.
type Service struct {
	Instance string       // "adb-3e8be48e-kIY2HE"
	Type     string       // "_adb-tls-connect._tcp"
	Host     string       // the SRV target, "Android_X32DXQ4S.local."
	Port     int          // 0 until the SRV record came
	Addrs    []netip.Addr // the host's addresses, IPv4 first
	TXT      []string     // the instance's TXT strings ("key=value"), nil until its TXT record came
}

// Usable reports whether an address in a record can be a device's on the
// network: not unspecified, loopback or multicast, not in 0.0.0.0/8 or the
// IPv4 broadcast address, and not an IPv4 address written as IPv6 (a
// device sends those in an A record). A record that names any other
// points searchers at this computer or at nobody: by mistake, or by
// someone who replays a phone's record with another address. The cache
// skips such records, so they are neither dialed nor flush the host's
// addresses.
func Usable(a netip.Addr) bool {
	switch {
	case !a.IsValid(), a.IsUnspecified(), a.IsLoopback(), a.IsMulticast(), a.Is4In6():
		return false
	case a.Is4():
		b := a.As4()
		return b[0] != 0 && b != [4]byte{255, 255, 255, 255}
	}
	return true
}

// Addr is where the service listens, "192.168.5.137:38161", or "" while
// its port or address is unknown. It prefers IPv4 and leaves out
// link-local IPv6 addresses, which need an interface to dial.
func (s Service) Addr() string {
	if s.Port == 0 {
		return ""
	}
	for _, a := range s.Addrs {
		if a.Is4() || !a.IsLinkLocalUnicast() {
			return netip.AddrPortFrom(a, uint16(s.Port)).String()
		}
	}
	return ""
}

// Options adjust a Browser. The zero value searches every interface that
// can multicast.
type Options struct {
	// Dest is where the questions go instead of the mDNS group on each
	// interface, for tests.
	Dest netip.AddrPort
	// Interfaces lists the interfaces to search; nil means each one that
	// is up, can multicast and has an IPv4 address. It is read again
	// before every round of questions, so the search follows the network.
	Interfaces func() ([]net.Interface, error)
	// NoGroup skips listening on the mDNS group, for tests.
	NoGroup bool
	// MulticastAnswers asks from port 5353 (the group socket) and without
	// the unicast-response bit, so that responders answer to the mDNS
	// group: a question from another port gets its answer by unicast to
	// that port (RFC 6762 §6.7), which a firewall that lets mDNS in but
	// nothing else (ufw's default rules) drops. Linux only: there a socket
	// bound to the group's address sends from the interface's own; other
	// systems ignore it.
	MulticastAnswers bool
	// MaxEvery, when set, spaces the rounds of questions as RFC 6762 §5.2
	// asks of a search that runs for long: 1 s after the first, then twice
	// as long each time, up to MaxEvery, so that the responders on the
	// network, which all answer each round, are not woken every few
	// seconds. The rounds start over when this computer's network
	// addresses change. Zero keeps the quick schedule below, for searches
	// that last seconds.
	MaxEvery time.Duration
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

// The questions go out at once, then after these delays, then every
// steadyEvery: a phone whose Wi-Fi dozes misses the first ones. With
// Options.MaxEvery they back off instead (schedule).
var (
	queryDelays = []time.Duration{time.Second, time.Second, 2 * time.Second, 2 * time.Second, 4 * time.Second}
	steadyEvery = 5 * time.Second
	// netWatch is how often a search with Options.MaxEvery looks whether
	// the computer's network addresses changed, to start its rounds over.
	netWatch = 5 * time.Second
	// followUp is how soon a service that showed up without its port or
	// address is asked for them, at most once per followEvery.
	followUp    = 300 * time.Millisecond
	followEvery = time.Second
)

// Browser searches for services of some types until it is closed.
type Browser struct {
	types []string // "_adb-tls-pairing._tcp", lower case
	opt   Options

	uc  net.PacketConn // questions, and the answers sent straight back
	upc *ipv4.PacketConn
	gc  net.PacketConn // the mDNS group: multicast answers and announcements
	gpc *ipv4.PacketConn

	mu     sync.Mutex
	cache  cache
	err    error
	joined map[int]bool // interfaces gc tried to join the group on
	member bool         // gc joined the group on at least one

	// What this browser sent to the group comes back to gc (multicast
	// loopback). When it does not, the system drops this process's
	// traffic on the local network: macOS's Local Network privacy does
	// that without an error while the app is not allowed, or while the
	// system asks the person.
	echoes  [][]byte  // questions sent since the last one came back
	since   time.Time // when the first of them went out
	blocked bool      // none came back in loopWait

	kick   chan struct{}
	closed chan struct{}
	once   sync.Once
	wg     sync.WaitGroup
}

// Browse starts searching for services of the given types, such as
// "_adb-tls-connect._tcp". It fails only when it cannot open a socket;
// Err says what keeps a search from working once it runs.
func Browse(types []string, opt Options) (*Browser, error) {
	if len(types) == 0 {
		return nil, errors.New("mdns: no service types to browse")
	}
	if runtime.GOOS != "linux" {
		opt.MulticastAnswers = false
	}
	b := &Browser{opt: opt, joined: map[int]bool{}, kick: make(chan struct{}, 1), closed: make(chan struct{})}
	for _, t := range types {
		t = strings.ToLower(strings.TrimSuffix(strings.TrimSuffix(t, "."), ".local"))
		if !slices.Contains(b.types, t) {
			b.types = append(b.types, t)
		}
	}
	b.cache.init()
	uc, err := net.ListenPacket("udp4", "0.0.0.0:0")
	if err != nil {
		return nil, fmt.Errorf("mdns: %w", err)
	}
	b.uc, b.upc = uc, ipv4.NewPacketConn(uc)
	_ = b.upc.SetMulticastTTL(255) // RFC 6762 §11
	_ = b.upc.SetMulticastLoopback(true)
	if !opt.NoGroup && !opt.Dest.IsValid() {
		// Without the group, only answers sent straight back arrive; most
		// responders send them for questions with the unicast-response bit.
		if gc, err := listenGroup(); err == nil {
			b.gc, b.gpc = gc, ipv4.NewPacketConn(gc)
		}
	}
	b.wg.Add(1)
	go b.read(b.uc, false)
	if b.gc != nil {
		b.wg.Add(1)
		go b.read(b.gc, true)
	}
	b.wg.Add(1)
	go b.run()
	return b, nil
}

// Services returns the services found so far, sorted by type and name,
// without the ones whose records have expired or said goodbye. A service
// whose port or address has not come yet is listed with Port 0 or no
// Addrs.
func (b *Browser) Services() []Service {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.cache.services(b.now(), b.types)
}

// Err says why the search cannot work, or nil when its last round of
// questions went out on at least one interface (and, where the browser
// listens on the group, came back to it). It wraps ErrNotAllowed or
// ErrNoNetwork when those are the reasons.
func (b *Browser) Err() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.err == nil && b.blocked {
		return fmt.Errorf("%w: the questions sent to the mDNS group never came back to this computer", ErrNotAllowed)
	}
	return b.err
}

// Close ends the search.
func (b *Browser) Close() error {
	b.once.Do(func() {
		close(b.closed)
		b.uc.Close()
		if b.gc != nil {
			b.gc.Close()
		}
	})
	b.wg.Wait()
	return nil
}

func (b *Browser) now() time.Time {
	if b.opt.Now != nil {
		return b.opt.Now()
	}
	return time.Now()
}

// run sends the questions on schedule, and a follow-up soon after a
// service shows up without its port or address.
func (b *Browser) run() {
	defer b.wg.Done()
	sched := schedule{max: b.opt.MaxEvery}
	next := time.NewTimer(0)
	defer next.Stop()
	var follow <-chan time.Time
	var followed time.Time
	var watch <-chan time.Time
	var addrs string
	if b.opt.MaxEvery > 0 {
		t := time.NewTicker(netWatch)
		defer t.Stop()
		watch = t.C
		addrs = b.netKey()
	}
	for {
		select {
		case <-b.closed:
			return
		case <-next.C:
			b.ask(true)
			next.Reset(sched.next())
		case <-watch:
			// Another network: the services there are asked for at once.
			if k := b.netKey(); k != addrs {
				addrs = k
				sched.restart()
				next.Reset(0)
			}
		case <-b.kick:
			// A responder that never sends the missing records is asked
			// again by the rounds above, not in a loop.
			if follow == nil && time.Since(followed) >= followEvery {
				follow = time.After(followUp)
			}
		case <-follow:
			follow, followed = nil, time.Now()
			b.ask(false)
		}
	}
}

// schedule is how long to wait after each round of questions.
type schedule struct {
	max   time.Duration // Options.MaxEvery; zero: queryDelays, then steadyEvery
	step  int
	every time.Duration // with max: the last wait
}

func (s *schedule) next() time.Duration {
	if s.max > 0 {
		s.every = min(max(2*s.every, time.Second), max(s.max, time.Second))
		return s.every
	}
	if s.step < len(queryDelays) {
		s.step++
		return queryDelays[s.step-1]
	}
	return steadyEvery
}

// restart starts the waits over, as for a search just started.
func (s *schedule) restart() { s.step, s.every = 0, 0 }

// interfaceAddrs lists an interface's addresses; tests replace it.
var interfaceAddrs = func(ifi *net.Interface) ([]net.Addr, error) { return ifi.Addrs() }

// netKey sums up the interfaces the search uses and their addresses, which
// change when the computer joins another network.
func (b *Browser) netKey() string {
	ifs, err := b.interfaces()
	if err != nil {
		return ""
	}
	var sb strings.Builder
	for i := range ifs {
		sb.WriteString(ifs[i].Name)
		addrs, _ := interfaceAddrs(&ifs[i])
		for _, a := range addrs {
			sb.WriteString(" " + a.String())
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

// ask sends one round of questions: the types (when all is set) and the
// names whose records are missing.
func (b *Browser) ask(all bool) {
	b.mu.Lock()
	srv, hosts := b.cache.missing(b.now(), b.types)
	b.mu.Unlock()
	if !all && len(srv) == 0 && len(hosts) == 0 {
		return
	}
	var types []string
	if all {
		types = b.types
	}
	msg, err := query(types, srv, hosts, !b.opt.MulticastAnswers)
	if err != nil {
		b.setErr(err)
		return
	}
	err = b.send(msg)
	b.setErr(err)
	b.mu.Lock()
	member := b.member
	b.mu.Unlock()
	if err == nil && member && !b.opt.Dest.IsValid() {
		b.expectEcho(msg)
	}
}

// loopWait is how long a question sent to the group may take to come back
// before the browser counts the local network as closed to it.
const loopWait = 1500 * time.Millisecond

// expectEcho notes a question sent to the group, and whether the earlier
// ones came back in time.
func (b *Browser) expectEcho(msg []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	if len(b.echoes) > 0 && now.Sub(b.since) > loopWait {
		b.blocked = true
	}
	if len(b.echoes) == 0 {
		b.since = now
	}
	if len(b.echoes) == 4 {
		b.echoes = b.echoes[1:]
	}
	b.echoes = append(b.echoes, msg)
}

// echoed reports whether a message on the group socket is one this
// browser sent, and if so clears the wait for it.
func (b *Browser) echoed(msg []byte) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, e := range b.echoes {
		if string(e) == string(msg) {
			b.echoes, b.blocked = nil, false
			return true
		}
	}
	return false
}

func (b *Browser) setErr(err error) {
	b.mu.Lock()
	b.err = err
	b.mu.Unlock()
}

// send sends a message to the mDNS group on every interface, or to
// Options.Dest. It returns nil when it went out at least once.
func (b *Browser) send(msg []byte) error {
	if b.opt.Dest.IsValid() {
		_, err := b.uc.WriteTo(msg, net.UDPAddrFromAddrPort(b.opt.Dest))
		return sendErr(err)
	}
	ifs, err := b.interfaces()
	if err != nil {
		return fmt.Errorf("%w: %v", ErrNoNetwork, err)
	}
	if len(ifs) == 0 {
		return ErrNoNetwork
	}
	group := net.UDPAddrFromAddrPort(Group)
	pc := b.upc
	if b.opt.MulticastAnswers && b.gpc != nil {
		pc = b.gpc
	}
	var first error
	sent := false
	for i := range ifs {
		ifi := &ifs[i]
		b.join(ifi)
		err := pc.SetMulticastInterface(ifi)
		if err == nil {
			_, err = pc.WriteTo(msg, nil, group)
		}
		if err == nil {
			sent = true
		} else if first == nil {
			first = fmt.Errorf("%s: %w", ifi.Name, err)
		}
	}
	if sent {
		return nil
	}
	return sendErr(first)
}

// join adds the group socket to the group on an interface the first time
// the search uses it.
func (b *Browser) join(ifi *net.Interface) {
	if b.gpc == nil {
		return
	}
	b.mu.Lock()
	done := b.joined[ifi.Index]
	b.joined[ifi.Index] = true
	b.mu.Unlock()
	if done {
		return
	}
	if err := b.gpc.JoinGroup(ifi, net.UDPAddrFromAddrPort(Group)); err == nil {
		b.mu.Lock()
		b.member = true
		b.mu.Unlock()
	}
}

func (b *Browser) interfaces() ([]net.Interface, error) {
	if b.opt.Interfaces != nil {
		return b.opt.Interfaces()
	}
	return MulticastInterfaces()
}

// MulticastInterfaces lists the interfaces a search uses by default: up,
// able to multicast, not loopback or point-to-point (VPN tunnels), with an
// IPv4 address.
func MulticastInterfaces() ([]net.Interface, error) {
	all, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	var out []net.Interface
	for _, ifi := range all {
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagMulticast == 0 ||
			ifi.Flags&(net.FlagLoopback|net.FlagPointToPoint) != 0 {
			continue
		}
		addrs, err := ifi.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil {
				out = append(out, ifi)
				break
			}
		}
	}
	return out, nil
}

// read reads one socket's messages until the browser closes.
func (b *Browser) read(conn net.PacketConn, group bool) {
	defer b.wg.Done()
	buf := make([]byte, 9000) // RFC 6762 §17
	for {
		n, _, err := conn.ReadFrom(buf)
		if err != nil {
			select {
			case <-b.closed:
				return
			default:
			}
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			// A socket that fails for good: stop reading it, the other
			// one may still work.
			return
		}
		if group && b.echoed(buf[:n]) {
			continue
		}
		b.handle(buf[:n])
	}
}

// handle reads a message's records into the cache.
func (b *Browser) handle(msg []byte) {
	recs, ok := parse(msg)
	if !ok {
		return
	}
	b.mu.Lock()
	unresolved := b.cache.add(b.now(), b.types, recs)
	b.mu.Unlock()
	if unresolved {
		select {
		case b.kick <- struct{}{}:
		default:
		}
	}
}

// query builds a message with questions for the types' instances (PTR),
// the instances' SRV and TXT records and the hosts' addresses, each with
// the unicast-response bit set when unicast (RFC 6762 §5.4).
func query(types, instances, hosts []string, unicast bool) ([]byte, error) {
	b := dnsmessage.NewBuilder(make([]byte, 0, 512), dnsmessage.Header{})
	b.EnableCompression()
	if err := b.StartQuestions(); err != nil {
		return nil, err
	}
	qu := dnsmessage.ClassINET
	if unicast {
		qu |= unicastBit
	}
	q := func(name string, t dnsmessage.Type) error {
		n, err := dnsmessage.NewName(name)
		if err != nil {
			return nil // a name too long for DNS cannot be asked for; skip it
		}
		return b.Question(dnsmessage.Question{Name: n, Type: t, Class: qu})
	}
	for _, t := range types {
		if err := q(t+".local.", dnsmessage.TypePTR); err != nil {
			return nil, err
		}
	}
	for _, fqdn := range instances {
		if err := q(fqdn, dnsmessage.TypeSRV); err != nil {
			return nil, err
		}
	}
	for _, h := range hosts {
		if err := q(h, dnsmessage.TypeA); err != nil {
			return nil, err
		}
		if err := q(h, dnsmessage.TypeAAAA); err != nil {
			return nil, err
		}
	}
	return b.Finish()
}

// unicastBit is the top bit of a question's class (the unicast-response
// bit) and of a record's class (cache flush).
const unicastBit = 0x8000

// record is one resource record of a response that the cache keeps.
type record struct {
	name   string // lower case, with the final dot
	typ    dnsmessage.Type
	ttl    uint32
	flush  bool
	target string // PTR and SRV: as sent
	port   uint16 // SRV
	addr   netip.Addr
	txt    []string // TXT
}

// parse reads the PTR, SRV, TXT, A and AAAA records of an mDNS response,
// in every section; it skips the other types (NSEC, OPT) and questions.
func parse(msg []byte) ([]record, bool) {
	var p dnsmessage.Parser
	h, err := p.Start(msg)
	if err != nil || !h.Response || h.RCode != dnsmessage.RCodeSuccess {
		return nil, false
	}
	if err := p.SkipAllQuestions(); err != nil {
		return nil, false
	}
	var out []record
	sections := []struct {
		header func() (dnsmessage.ResourceHeader, error)
		skip   func() error
	}{
		{p.AnswerHeader, p.SkipAnswer},
		{p.AuthorityHeader, p.SkipAuthority},
		{p.AdditionalHeader, p.SkipAdditional},
	}
	for _, s := range sections {
		for {
			rh, err := s.header()
			if errors.Is(err, dnsmessage.ErrSectionDone) {
				break
			}
			if err != nil {
				return out, len(out) > 0
			}
			r := record{name: strings.ToLower(rh.Name.String()), typ: rh.Type, ttl: rh.TTL, flush: rh.Class&unicastBit != 0}
			if rh.Class&^unicastBit != dnsmessage.ClassINET {
				if err := s.skip(); err != nil {
					return out, len(out) > 0
				}
				continue
			}
			switch rh.Type {
			case dnsmessage.TypePTR:
				rr, err := p.PTRResource()
				if err != nil {
					return out, len(out) > 0
				}
				r.target = rr.PTR.String()
			case dnsmessage.TypeSRV:
				rr, err := p.SRVResource()
				if err != nil {
					return out, len(out) > 0
				}
				r.target, r.port = rr.Target.String(), rr.Port
			case dnsmessage.TypeTXT:
				// A TXT record that does not parse is skipped, not the
				// rest of the message: the services' PTR, SRV and address
				// records often come after it, and none of them needs it.
				rr, err := p.TXTResource()
				if err != nil {
					if err := s.skip(); err != nil {
						return out, len(out) > 0
					}
					continue
				}
				r.txt = rr.TXT
			case dnsmessage.TypeA:
				rr, err := p.AResource()
				if err != nil {
					return out, len(out) > 0
				}
				r.addr = netip.AddrFrom4(rr.A)
			case dnsmessage.TypeAAAA:
				rr, err := p.AAAAResource()
				if err != nil {
					return out, len(out) > 0
				}
				r.addr = netip.AddrFrom16(rr.AAAA)
			default:
				if err := s.skip(); err != nil {
					return out, len(out) > 0
				}
				continue
			}
			out = append(out, r)
		}
	}
	return out, true
}

// cache holds the records of the services browsed for, until their TTLs
// run out (RFC 6762 §10).
type cache struct {
	ptrs  map[ptrKey]time.Time  // a type's instance, until it expires
	srvs  map[string]srvRecord  // by the instance's full name, lower case
	txts  map[string]txtRecord  // by the instance's full name, lower case
	addrs map[string][]addrItem // by host name, lower case
}

type ptrKey struct {
	typ      string // "_adb-tls-pairing._tcp"
	instance string // as sent
}

type srvRecord struct {
	target  string
	port    uint16
	expires time.Time
}

type txtRecord struct {
	txt     []string
	expires time.Time
}

type addrItem struct {
	addr     netip.Addr
	expires  time.Time
	received time.Time
}

// maxHosts bounds the hosts the cache keeps addresses of.
const maxHosts = 256

func (c *cache) init() {
	c.ptrs = map[ptrKey]time.Time{}
	c.srvs = map[string]srvRecord{}
	c.txts = map[string]txtRecord{}
	c.addrs = map[string][]addrItem{}
}

// typeOf returns which browsed type a name such as
// "_adb-tls-pairing._tcp.local." is, or "".
func typeOf(name string, types []string) string {
	for _, t := range types {
		if name == t+".local." {
			return t
		}
	}
	return ""
}

// instanceOf returns the instance name in an instance's full name, such as
// "pinky-x._adb-tls-pairing._tcp.local.", and its type, when the type is
// browsed.
func instanceOf(fqdn string, types []string) (instance, typ string) {
	low := strings.ToLower(fqdn)
	for _, t := range types {
		if suffix := "." + t + ".local."; strings.HasSuffix(low, suffix) && len(low) > len(suffix) {
			return fqdn[:len(fqdn)-len(suffix)], t
		}
	}
	return "", ""
}

// add stores a response's records of the browsed services, and reports
// whether a service now lacks its port or address.
func (c *cache) add(now time.Time, types []string, recs []record) bool {
	expiry := func(ttl uint32) time.Time { return now.Add(time.Duration(ttl) * time.Second) }
	// The hosts whose addresses matter: the targets of the services'
	// SRV records, in this response or already known.
	hosts := map[string]bool{}
	for _, s := range c.srvs {
		hosts[strings.ToLower(s.target)] = true
	}
	for _, r := range recs {
		switch r.typ {
		case dnsmessage.TypePTR:
			typ := typeOf(r.name, types)
			if typ == "" {
				continue
			}
			inst, ityp := instanceOf(r.target, types)
			if inst == "" || ityp != typ {
				continue
			}
			k := ptrKey{typ, inst}
			if r.ttl == 0 {
				delete(c.ptrs, k) // goodbye
				continue
			}
			c.ptrs[k] = expiry(r.ttl)
		case dnsmessage.TypeSRV:
			if inst, _ := instanceOf(r.name, types); inst == "" {
				continue
			}
			if r.ttl == 0 {
				delete(c.srvs, r.name)
				continue
			}
			c.srvs[r.name] = srvRecord{target: r.target, port: r.port, expires: expiry(r.ttl)}
			hosts[strings.ToLower(r.target)] = true
		case dnsmessage.TypeTXT:
			if inst, _ := instanceOf(r.name, types); inst == "" {
				continue
			}
			if r.ttl == 0 {
				delete(c.txts, r.name)
				continue
			}
			c.txts[r.name] = txtRecord{txt: r.txt, expires: expiry(r.ttl)}
		}
	}
	flushed := map[string]bool{}
	for _, r := range recs {
		if (r.typ != dnsmessage.TypeA && r.typ != dnsmessage.TypeAAAA) || !hosts[r.name] || !Usable(r.addr) {
			continue
		}
		items := c.addrs[r.name]
		if r.flush && !flushed[r.name] {
			// Cache flush: this response has the host's current addresses;
			// forget those that came more than a second ago (§10.2).
			flushed[r.name] = true
			items = slices.DeleteFunc(items, func(it addrItem) bool {
				return it.addr.Is4() == r.addr.Is4() && now.Sub(it.received) > time.Second
			})
		}
		items = slices.DeleteFunc(items, func(it addrItem) bool { return it.addr == r.addr })
		if r.ttl > 0 {
			items = append(items, addrItem{addr: r.addr, expires: expiry(r.ttl), received: now})
		}
		if len(items) == 0 {
			delete(c.addrs, r.name)
		} else {
			c.addrs[r.name] = items
		}
	}
	c.prune(now)
	srv, missingHosts := c.missing(now, types)
	return len(srv) > 0 || len(missingHosts) > 0
}

// prune drops what has expired, and the addresses of hosts no service
// points at once there are many.
func (c *cache) prune(now time.Time) {
	for k, exp := range c.ptrs {
		if !now.Before(exp) {
			delete(c.ptrs, k)
		}
	}
	for k, s := range c.srvs {
		if !now.Before(s.expires) {
			delete(c.srvs, k)
		}
	}
	for k, t := range c.txts {
		if !now.Before(t.expires) {
			delete(c.txts, k)
		}
	}
	for h, items := range c.addrs {
		items = slices.DeleteFunc(items, func(it addrItem) bool { return !now.Before(it.expires) })
		if len(items) == 0 {
			delete(c.addrs, h)
		} else {
			c.addrs[h] = items
		}
	}
	if len(c.addrs) > maxHosts {
		used := map[string]bool{}
		for _, s := range c.srvs {
			used[strings.ToLower(s.target)] = true
		}
		for h := range c.addrs {
			if !used[h] {
				delete(c.addrs, h)
			}
		}
	}
}

// missing returns the full names of the instances whose SRV record has not
// come, and the hosts whose addresses have not.
func (c *cache) missing(now time.Time, types []string) (instances, hosts []string) {
	for k, exp := range c.ptrs {
		if !now.Before(exp) {
			continue
		}
		fqdn := k.instance + "." + k.typ + ".local."
		s, ok := c.srvs[strings.ToLower(fqdn)]
		if !ok || !now.Before(s.expires) {
			instances = append(instances, fqdn)
			continue
		}
		if !c.hasAddr(now, s.target) && !slices.Contains(hosts, s.target) {
			hosts = append(hosts, s.target)
		}
	}
	slices.Sort(instances)
	slices.Sort(hosts)
	return instances, hosts
}

func (c *cache) hasAddr(now time.Time, host string) bool {
	for _, it := range c.addrs[strings.ToLower(host)] {
		if now.Before(it.expires) {
			return true
		}
	}
	return false
}

func (c *cache) services(now time.Time, types []string) []Service {
	var out []Service
	for k, exp := range c.ptrs {
		if !now.Before(exp) {
			continue
		}
		svc := Service{Instance: k.instance, Type: k.typ}
		fqdn := strings.ToLower(k.instance + "." + k.typ + ".local.")
		if t, ok := c.txts[fqdn]; ok && now.Before(t.expires) {
			svc.TXT = append([]string(nil), t.txt...)
		}
		if s, ok := c.srvs[fqdn]; ok && now.Before(s.expires) {
			svc.Host, svc.Port = s.target, int(s.port)
			for _, it := range c.addrs[strings.ToLower(s.target)] {
				if now.Before(it.expires) {
					svc.Addrs = append(svc.Addrs, it.addr)
				}
			}
			slices.SortStableFunc(svc.Addrs, func(a, b netip.Addr) int {
				if a.Is4() != b.Is4() {
					if a.Is4() {
						return -1
					}
					return 1
				}
				return 0
			})
		}
		out = append(out, svc)
	}
	slices.SortFunc(out, func(a, b Service) int {
		if c := strings.Compare(a.Type, b.Type); c != 0 {
			return c
		}
		return strings.Compare(a.Instance, b.Instance)
	})
	return out
}

// String is for logs: "adb-x (_adb-tls-connect._tcp) at 192.168.1.5:40000".
func (s Service) String() string {
	at := s.Addr()
	if at == "" {
		at = "?"
		if s.Port != 0 {
			at = s.Host + ":" + strconv.Itoa(s.Port)
		}
	}
	return s.Instance + " (" + s.Type + ") at " + at
}
