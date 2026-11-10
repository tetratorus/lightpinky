package mdns

import (
	"errors"
	"fmt"
	"syscall"
)

// sendErr marks the errors that mean the system refused to send as
// ErrNotAllowed: a firewall rule (EPERM, EACCES) or, on macOS, Local
// Network privacy, which makes sends fail with EHOSTUNREACH.
func sendErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) || refusedByPrivacy(err) {
		return fmt.Errorf("%w: %v", ErrNotAllowed, err)
	}
	return err
}
