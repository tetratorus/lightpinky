package mdns

import (
	"errors"
	"syscall"
)

// refusedByPrivacy: macOS 15 and later fail a send to the local network
// with "no route to host" while the app may not use it (System Settings ›
// Privacy & Security › Local Network), also while its prompt is open.
func refusedByPrivacy(err error) bool {
	return errors.Is(err, syscall.EHOSTUNREACH)
}
