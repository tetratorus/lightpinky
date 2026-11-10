//go:build !darwin && !windows

package ocr

import (
	"fmt"
	"image"

	"github.com/tetratorus/lightpinky/internal/driver"
)

// errNone says why there is no OCR here: Linux has no recognizer built
// in, and Pinky carries none (PaddleOCR on ONNX Runtime is planned in the
// design).
var errNone = fmt.Errorf("ocr: this computer has no text recognition (Pinky reads text from pictures on macOS and Windows): %w", driver.ErrUnsupported)

func supported() error { return errNone }

func recognize([]byte, Options) ([]Line, error) { return nil, errNone }

func recognizeImage(image.Image, Options) ([]Line, error) { return nil, errNone }
