package ios

import (
	"github.com/tetratorus/lightpinky/internal/ocr"
	"github.com/tetratorus/lightpinky/internal/screen"
)

// OCR is the shared engine in internal/ocr: Vision on macOS,
// Windows.Media.Ocr on Windows. These names keep the driver's code as it
// was before the engine moved there.

type ocrLine = ocr.Line

type ocrOptions = ocr.Options

// ocrLanguages are the languages OCR reads, in priority order.
var ocrLanguages = ocr.Languages

func recognizeText(img []byte, opt ocrOptions) ([]ocrLine, error) { return ocr.Recognize(img, opt) }

func ocrRect(l ocrLine, w, h int) screen.Rect { return ocr.Rect(l, w, h) }

// keyboardFromOCR finds an on-screen keyboard in OCR lines (ocr.KeyboardTop).
func keyboardFromOCR(lines []ocrLine) (float64, bool) { return ocr.KeyboardTop(lines) }

// ocrExtras returns the OCR lines no element already shows, as OCR
// elements (ocr.Extras).
func ocrExtras(els []screen.Element, lines []ocrLine, w, h int) []screen.Element {
	return ocr.Extras(els, lines, w, h)
}
