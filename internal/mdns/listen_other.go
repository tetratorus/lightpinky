//go:build !darwin && !linux

package mdns

import (
	"errors"
	"net"
)

// listenGroup is not done here: binding to a multicast address is not
// possible everywhere, and binding the wildcard could take the system
// responder's unicast answers. Answers sent straight back still arrive.
func listenGroup() (net.PacketConn, error) {
	return nil, errors.New("mdns: no group socket on this system")
}
