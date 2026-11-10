//go:build darwin || linux

package mdns

import (
	"fmt"
	"net"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// listenGroup opens a socket bound to the mDNS group's address and port,
// which it shares with the system's responder (SO_REUSEPORT). Bound to the
// group rather than to every address, it gets the multicast messages and
// none of the unicast answers meant for the responder. The net package
// cannot bind to a multicast address: it binds the wildcard instead.
func listenGroup() (net.PacketConn, error) {
	syscall.ForkLock.RLock()
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, unix.IPPROTO_UDP)
	if err == nil {
		unix.CloseOnExec(fd)
	}
	syscall.ForkLock.RUnlock()
	if err != nil {
		return nil, fmt.Errorf("mdns: socket: %w", err)
	}
	fail := func(what string, err error) (net.PacketConn, error) {
		unix.Close(fd)
		return nil, fmt.Errorf("mdns: %s: %w", what, err)
	}
	if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_REUSEADDR, 1); err != nil {
		return fail("SO_REUSEADDR", err)
	}
	if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_REUSEPORT, 1); err != nil {
		return fail("SO_REUSEPORT", err)
	}
	sa := &unix.SockaddrInet4{Port: int(Group.Port()), Addr: Group.Addr().As4()}
	if err := unix.Bind(fd, sa); err != nil {
		return fail("bind "+Group.String(), err)
	}
	f := os.NewFile(uintptr(fd), "mdns-group")
	c, err := net.FilePacketConn(f) // dups the descriptor
	f.Close()
	if err != nil {
		return nil, fmt.Errorf("mdns: %w", err)
	}
	return c, nil
}
