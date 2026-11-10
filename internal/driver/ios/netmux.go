package ios

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"

	"github.com/tetratorus/lightpinky/internal/driver/ios/netmux"
	goios "github.com/danielpaulus/go-ios/ios"
)

// Phones over Wi-Fi on Linux and Windows. libimobiledevice's usbmuxd, the
// one Linux systems have, lists phones on a cable only, and Apple's on
// Windows lists a phone on Wi-Fi only while it is on a cable too, so a
// phone that Pinky used went offline when it was unplugged instead of
// moving to Wi-Fi. With
// Provider.WiFiMux, while Pinky uses phones over Wi-Fi, the provider runs a
// usbmuxd of its own (the package netmux) in front of the system's: it
// hands it everything about phones on a cable, and adds the phones it finds
// on the network itself, as macOS's usbmuxd lists them, matched to this
// computer's pair records. go-ios, and so everything Pinky does with a
// phone, goes through it from then on (USBMUXD_SOCKET_ADDRESS, set for
// this process). Turning Wi-Fi off stops the search, and the phones on
// Wi-Fi leave the list; phones on a cable go on through it.
//
// When USBMUXD_SOCKET_ADDRESS is set already (netmuxd, usbmuxd2 or
// another usbmuxd the person runs), Pinky uses that one and leaves the
// network to it.
//
// The variable is set for the whole process, so every process Pinky starts
// inherits it: the update helper and the Pinky it starts after an update,
// a restart, an agent and what the agent runs. A Pinky that inherits the
// address of another Pinky's usbmuxd, gone with it, must not take it for
// one the person set, or no iPhone connects (Windows after an update). So
// Pinky sets ownMuxVar to the same address, and a Pinky that starts with
// both equal forgets them (forgetInheritedMux), before go-ios reads the
// address.

// ownMuxVar holds the address Pinky set USBMUXD_SOCKET_ADDRESS to for its
// own usbmuxd.
const ownMuxVar = "PINKY_OWN_USBMUXD"

func init() { forgetInheritedMux() }

// forgetInheritedMux unsets USBMUXD_SOCKET_ADDRESS when it is the address
// another Pinky set for its own usbmuxd, and ownMuxVar always.
func forgetInheritedMux() {
	own := os.Getenv(ownMuxVar)
	if own != "" && os.Getenv("USBMUXD_SOCKET_ADDRESS") == own {
		os.Unsetenv("USBMUXD_SOCKET_ADDRESS")
	}
	os.Unsetenv(ownMuxVar)
}

// muxState is the provider's usbmuxd of its own. Its zero value is ready.
type muxState struct {
	mu      sync.Mutex
	mux     *netmux.Mux
	stop    context.CancelFunc // ends the search while it runs
	skipped bool               // USBMUXD_SOCKET_ADDRESS was set, or the socket failed

	// For tests.
	anyOS    bool
	browseFn func() (netmux.Source, error)
	pairFn   func(udid string) (netmux.PairInfo, error)
	probeFn  func(ctx context.Context, addr netip.Addr) bool
	dir      string // where the socket goes instead of a new folder
}

// wifiMux starts the usbmuxd of its own and the search when Pinky uses
// phones over Wi-Fi, and stops the search when it no longer does.
func (p *Provider) wifiMux(on bool) {
	if !p.WiFiMux || (!ownMuxOS() && !p.nmux.anyOS) {
		return
	}
	m := &p.nmux
	m.mu.Lock()
	defer m.mu.Unlock()
	if !on {
		if m.stop != nil {
			m.stop()
			m.stop = nil
		}
		return
	}
	if m.mux == nil && !m.skipped {
		p.startMuxLocked()
	}
	if m.mux == nil || m.stop != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.stop = cancel
	f := &netmux.Finder{Mux: m.mux, UDIDs: p.pairedUDIDs, Log: p.logger(), BrowseFn: m.browseFn}
	if m.probeFn != nil {
		f.ProbeFn = m.probeFn
	}
	if m.pairFn != nil {
		f.PairFn = m.pairFn
	}
	go f.Run(ctx)
}

// startMuxLocked starts the usbmuxd of its own and points go-ios at it.
// The caller holds p.nmux.mu.
func (p *Provider) startMuxLocked() {
	m := &p.nmux
	if v := os.Getenv("USBMUXD_SOCKET_ADDRESS"); v != "" {
		m.skipped = true
		p.logger().Info("iphone: using the usbmuxd in USBMUXD_SOCKET_ADDRESS, which lists phones on Wi-Fi itself or not", "address", v)
		return
	}
	upstream := goios.GetUsbmuxdSocket()
	if runtime.GOOS == "windows" && m.dir == "" {
		// Apple's usbmuxd listens on a TCP port on Windows, and go-ios
		// dials one as well; a unix socket's file there can outlive a
		// Pinky that was killed and then refuse the next one.
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			m.skipped = true
			p.logger().Warn("iphone: cannot look for phones on Wi-Fi", "err", err)
			return
		}
		p.serveMuxLocked(upstream, l, "tcp://"+l.Addr().String())
		return
	}
	dir := m.dir
	if dir == "" {
		base := os.Getenv("XDG_RUNTIME_DIR")
		if base == "" {
			base = os.TempDir()
		}
		d, err := os.MkdirTemp(base, "pinky-usbmuxd-") // 0700: this user's only
		if err != nil {
			m.skipped = true
			p.logger().Warn("iphone: cannot look for phones on Wi-Fi", "err", err)
			return
		}
		dir = d
	}
	path := filepath.Join(dir, "usbmuxd")
	l, err := net.Listen("unix", path)
	if err != nil {
		m.skipped = true
		p.logger().Warn("iphone: cannot look for phones on Wi-Fi", "err", err)
		return
	}
	p.serveMuxLocked(upstream, l, "unix://"+path)
}

// serveMuxLocked runs the usbmuxd of its own on l, in front of upstream,
// and points go-ios at it (addr). The caller holds p.nmux.mu.
func (p *Provider) serveMuxLocked(upstream string, l net.Listener, addr string) {
	m := &p.nmux
	m.mux = netmux.New(upstream, p.logger())
	go m.mux.Serve(l)
	os.Setenv("USBMUXD_SOCKET_ADDRESS", addr)
	os.Setenv(ownMuxVar, addr) // so that the processes it starts forget it
	// Phones on the network close developer services without it
	// (netmux/heartbeat.go). Not in tests, which list made-up phones.
	m.mux.Heartbeat = m.dir == ""
	p.logger().Info("iphone: looking for iPhones and iPads on Wi-Fi itself; this computer's usbmuxd does not list them there by itself", "usbmuxd", upstream)
}

// ownMuxOS reports whether Pinky looks for phones on Wi-Fi itself on this
// system: on Linux, whose usbmuxd lists phones on a cable only, and on
// Windows, whose Apple usbmuxd lists an iPhone on Wi-Fi only while it is
// on a cable too (an iPhone 18 Pro Max on iOS 27.2, 2026-11-09), and on
// macOS, whose usbmuxd did not list an iPhone 16 Pro on iOS 27.2 on Wi-Fi
// at all, on the Mac's subnet or another one: its attach failed the
// preflight with 0xe8000084 ("This device is no longer connected"), while
// Pinky's own mux used it (macOS 27.2.1, 2026-11-10). A phone macOS's
// usbmuxd does list on the network goes through it as before.
func ownMuxOS() bool {
	return runtime.GOOS == "linux" || runtime.GOOS == "windows" || runtime.GOOS == "darwin"
}

// pairedUDIDs returns the phones this computer paired with: those with a
// pair record where the system's usbmuxd keeps them, and those Pinky has
// seen, but not those the person removed. Under Only, only those phones.
func (p *Provider) pairedUDIDs() []string {
	ids := map[string]bool{}
	for _, id := range netmux.PairedUDIDs(netmux.LockdownDir) {
		ids[id] = true
	}
	for _, id := range readPhones(p.PhonesFile) {
		ids[id] = true
	}
	p.mu.Lock()
	for id := range p.known {
		ids[id] = true
	}
	for id := range p.wifi.use {
		ids[id] = true
	}
	// A phone the person removed is not looked for, nor asked, until it is
	// on a cable again (Forget).
	for id := range p.wifi.forgot {
		delete(ids, id)
	}
	p.mu.Unlock()
	var out []string
	for id := range ids {
		if p.Only == nil || p.Only[id] {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// otherNetworkDetail says, for a phone found on Wi-Fi that does not answer,
// when its address is on none of this computer's networks: the phone then
// accepts lockdown's TCP connection but closes it before any answer (an
// iPhone 16 Pro on iOS 27.2 at 10.250.0.123, the Mac at 10.68.8.121,
// 2026-11-10), and the person should put both on the same Wi-Fi.
func (p *Provider) otherNetworkDetail(id string) string {
	p.nmux.mu.Lock()
	m := p.nmux.mux
	p.nmux.mu.Unlock()
	if m == nil {
		return ""
	}
	for _, ph := range m.Phones() {
		if ph.UDID == id {
			return otherNetwork(ph.Addr, localPrefixes())
		}
	}
	return ""
}

func localPrefixes() []netip.Prefix {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	var out []netip.Prefix
	for _, a := range addrs {
		n, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip, ok := netip.AddrFromSlice(n.IP)
		if !ok {
			continue
		}
		ones, _ := n.Mask.Size()
		out = append(out, netip.PrefixFrom(ip.Unmap(), ones))
	}
	return out
}

func otherNetwork(phone netip.Addr, local []netip.Prefix) string {
	phone = phone.Unmap()
	if !phone.IsValid() || phone.IsLinkLocalUnicast() || phone.IsLoopback() {
		return ""
	}
	var mine []string
	for _, l := range local {
		a := l.Addr()
		if a.Is4() != phone.Is4() || a.IsLoopback() || a.IsLinkLocalUnicast() {
			continue
		}
		if l.Masked().Contains(phone) {
			return ""
		}
		mine = append(mine, a.String())
	}
	if len(mine) == 0 {
		return ""
	}
	return fmt.Sprintf("The iPhone is at %s, on another network than this computer (%s); put both on the same Wi-Fi network. ", phone, strings.Join(mine, ", "))
}
