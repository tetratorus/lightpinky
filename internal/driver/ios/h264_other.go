//go:build !darwin

package ios

import (
	"fmt"
	"time"

	"github.com/tetratorus/lightpinky/internal/driver"
)

// The paths below iOS 27 encode their picture with macOS's VideoToolbox;
// other systems have no encoder Pinky can call without cgo yet.

type h264Encoder struct{ w, h int }

type encodedFrame struct {
	data     []byte
	config   []byte
	keyFrame bool
}

func newH264Encoder(w, h, fps, bitRate int) (*h264Encoder, error) {
	return nil, h264Available()
}

func (e *h264Encoder) encode(y, uv []byte, pts time.Duration, forceKey bool) ([]encodedFrame, error) {
	return nil, h264Available()
}

func (e *h264Encoder) Close() {}

func h264Available() error {
	return fmt.Errorf("the live picture of iPhones below iOS 27 needs macOS: %w", driver.ErrUnsupported)
}
