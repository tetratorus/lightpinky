//go:build windows

package ocr

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg" // Recognize's JPEGs
	_ "image/png"  // and PNGs
	"runtime"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/tetratorus/lightpinky/internal/driver"
)

// OCR with Windows.Media.Ocr, the recognizer Windows 10 and 11 carry,
// called through its WinRT interfaces with syscall, as Pinky has no cgo
// (the same way internal/secret reaches Windows Hello). It reads the
// languages whose OCR packs are installed, which come with the languages
// in Windows' settings: Pinky takes the first of Options.Languages that
// is installed (Chinese reads English too), else the person's own.
//
// The engine reads a SoftwareBitmap made from the picture's pixels; a
// picture larger than OcrEngine.MaxImageDimension is scaled down first.
// It gives no confidence, so every line has 1.

var (
	modcombase                    = windows.NewLazySystemDLL("combase.dll")
	procRoInitialize              = modcombase.NewProc("RoInitialize")
	procRoUninitialize            = modcombase.NewProc("RoUninitialize")
	procRoGetActivationFactory    = modcombase.NewProc("RoGetActivationFactory")
	procWindowsCreateString       = modcombase.NewProc("WindowsCreateString")
	procWindowsDeleteString       = modcombase.NewProc("WindowsDeleteString")
	procWindowsGetStringRawBuffer = modcombase.NewProc("WindowsGetStringRawBuffer")
)

// Interface ids, from Windows' metadata (as microsoft/windows-rs has them).
var (
	iidOcrEngineStatics           = windows.GUID{Data1: 0x5bffa85a, Data2: 0x3384, Data3: 0x3540, Data4: [8]byte{0x99, 0x40, 0x69, 0x91, 0x20, 0xd4, 0x28, 0xa8}}
	iidLanguageFactory            = windows.GUID{Data1: 0x9b0252ac, Data2: 0x0c27, Data3: 0x44f8, Data4: [8]byte{0xb7, 0x92, 0x97, 0x93, 0xfb, 0x66, 0xc6, 0x3e}}
	iidSoftwareBitmapStatics      = windows.GUID{Data1: 0xdf0385db, Data2: 0x672f, Data3: 0x4a9d, Data4: [8]byte{0x80, 0x6e, 0xc2, 0x44, 0x2f, 0x34, 0x3e, 0x86}}
	iidCryptographicBufferStatics = windows.GUID{Data1: 0x320b7e22, Data2: 0x3cb0, Data3: 0x4cdf, Data4: [8]byte{0x86, 0x63, 0x1d, 0x28, 0x91, 0x00, 0x65, 0xeb}}
	iidAsyncInfo                  = windows.GUID{Data1: 0x00000036, Data4: [8]byte{0xc0, 0, 0, 0, 0, 0, 0, 0x46}}
	classOcrEngine                = "Windows.Media.Ocr.OcrEngine"
	classLanguage                 = "Windows.Globalization.Language"
	classSoftwareBitmap           = "Windows.Graphics.Imaging.SoftwareBitmap"
	classCryptographicBuffer      = "Windows.Security.Cryptography.CryptographicBuffer"
	errNoEngine                   = errors.New("ocr: Windows has no OCR language installed (add a language with its optical character recognition in Settings › Time & language)")
	errEngineGone                 = errors.New("ocr: Windows.Media.Ocr is not available")
	recognizeTimeout              = 20 * time.Second
)

// bitmapPixelFormatBgra8 is BitmapPixelFormat.Bgra8.
const bitmapPixelFormatBgra8 = 87

// Vtable slots: IUnknown's three, then IInspectable's three, then each
// interface's methods in order.
const (
	slotQueryInterface = 0
	slotRelease        = 2

	slotMaxImageDimension     = 6  // IOcrEngineStatics
	slotIsLanguageSupported   = 8  // IOcrEngineStatics
	slotTryCreateFromLanguage = 9  // IOcrEngineStatics
	slotTryCreateFromProfile  = 10 // IOcrEngineStatics.TryCreateFromUserProfileLanguages
	slotCreateLanguage        = 6  // ILanguageFactory
	slotCreateFromByteArray   = 9  // ICryptographicBufferStatics
	slotCreateCopyFromBuffer  = 9  // ISoftwareBitmapStatics
	slotRecognizeAsync        = 6  // IOcrEngine
	slotResultLines           = 6  // IOcrResult
	slotLineWords             = 6  // IOcrLine
	slotWordBoundingRect      = 6  // IOcrWord
	slotWordText              = 7  // IOcrWord
	slotVectorGetAt           = 6  // IVectorView<T>
	slotVectorSize            = 7  // IVectorView<T>
	slotGetResults            = 8  // IAsyncOperation<T>
	slotAsyncStatus           = 7  // IAsyncInfo
	slotAsyncErrorCode        = 8  // IAsyncInfo
	slotAsyncCancel           = 9  // IAsyncInfo

	asyncStarted   = 0
	asyncCompleted = 1
	asyncCanceled  = 2

	roInitMultithreaded = 1          // RO_INIT_MULTITHREADED
	rpcEChangedMode     = 0x80010106 // RPC_E_CHANGED_MODE
)

// comObject is a COM interface pointer.
type comObject struct{ p unsafe.Pointer }

// call calls the method in a vtable slot. Pointers the arguments carry
// stay alive and in place for the call, as for syscall.SyscallN.
//
//go:uintptrescapes
func (o comObject) call(slot int, args ...uintptr) uint32 {
	vtbl := *(*unsafe.Pointer)(o.p)
	fn := *(*uintptr)(unsafe.Add(vtbl, uintptr(slot)*unsafe.Sizeof(uintptr(0))))
	r, _, _ := syscall.SyscallN(fn, append([]uintptr{uintptr(o.p)}, args...)...)
	return uint32(r)
}

func (o comObject) release() {
	if o.p != nil {
		o.call(slotRelease)
	}
}

// get calls a method whose last parameter receives an interface; args
// come before it. Each call converts the out pointer to a uintptr right in
// call's arguments, which keeps it on the heap (go:uintptrescapes): a
// stack variable could move before Windows writes to it.
func (o comObject) get(slot int, args ...uintptr) (comObject, error) {
	var out unsafe.Pointer
	var hr uint32
	switch len(args) {
	case 0:
		hr = o.call(slot, uintptr(unsafe.Pointer(&out)))
	case 1:
		hr = o.call(slot, args[0], uintptr(unsafe.Pointer(&out)))
	case 2:
		hr = o.call(slot, args[0], args[1], uintptr(unsafe.Pointer(&out)))
	case 4:
		hr = o.call(slot, args[0], args[1], args[2], args[3], uintptr(unsafe.Pointer(&out)))
	default:
		panic(fmt.Sprintf("ocr: get with %d arguments", len(args)))
	}
	if failed(hr) {
		return comObject{}, hresult(hr)
	}
	if out == nil {
		return comObject{}, errNoObject
	}
	return comObject{out}, nil
}

var errNoObject = errors.New("ocr: a WinRT call returned nothing")

func (o comObject) queryInterface(iid *windows.GUID) (comObject, error) {
	return o.get(slotQueryInterface, uintptr(unsafe.Pointer(iid)))
}

func failed(hr uint32) bool { return int32(hr) < 0 }

func hresult(hr uint32) error { return syscall.Errno(hr) }

// hstring is a WinRT string, deleted with free.
type hstring uintptr

func newHString(s string) (hstring, error) {
	u, err := windows.UTF16FromString(s)
	if err != nil {
		return 0, err
	}
	h := new(hstring)
	if r, _, _ := procWindowsCreateString.Call(uintptr(unsafe.Pointer(&u[0])), uintptr(len(u)-1), uintptr(unsafe.Pointer(h))); failed(uint32(r)) {
		return 0, hresult(uint32(r))
	}
	return *h, nil
}

func (h hstring) free() {
	if h != 0 {
		procWindowsDeleteString.Call(uintptr(h))
	}
}

// String copies the string out.
func (h hstring) String() string {
	if h == 0 {
		return ""
	}
	var n uint32
	p, _, _ := procWindowsGetStringRawBuffer.Call(uintptr(h), uintptr(unsafe.Pointer(&n)))
	if p == 0 || n == 0 {
		return ""
	}
	// Reinterpret rather than convert, so vet sees no uintptr arithmetic.
	ptr := *(*unsafe.Pointer)(unsafe.Pointer(&p))
	return windows.UTF16ToString(unsafe.Slice((*uint16)(ptr), n))
}

// getString calls a method whose last parameter receives an HSTRING.
func (o comObject) getString(slot int) (string, error) {
	h := new(hstring)
	if hr := o.call(slot, uintptr(unsafe.Pointer(h))); failed(hr) {
		return "", hresult(hr)
	}
	defer h.free()
	return h.String(), nil
}

// winrt runs fn on a thread of its own in WinRT's multithreaded apartment.
func winrt(fn func() error) error {
	if err := modcombase.Load(); err != nil {
		return fmt.Errorf("%w: %v", errEngineGone, err)
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	r, _, _ := procRoInitialize.Call(roInitMultithreaded)
	switch hr := uint32(r); {
	case !failed(hr): // S_OK, or S_FALSE when it already was
		defer procRoUninitialize.Call()
	case hr == rpcEChangedMode: // already a single-threaded apartment: use it
	default:
		return fmt.Errorf("%w: RoInitialize: %v", errEngineGone, hresult(hr))
	}
	return fn()
}

func activationFactory(class string, iid *windows.GUID) (comObject, error) {
	h, err := newHString(class)
	if err != nil {
		return comObject{}, err
	}
	defer h.free()
	out := new(unsafe.Pointer)
	if r, _, _ := procRoGetActivationFactory.Call(uintptr(h), uintptr(unsafe.Pointer(iid)), uintptr(unsafe.Pointer(out))); failed(uint32(r)) {
		return comObject{}, fmt.Errorf("%s: %w", class, hresult(uint32(r)))
	}
	return comObject{*out}, nil
}

// engineSetup is what the first use finds: which language to read and the
// largest picture the engine takes.
var engineSetup struct {
	once     sync.Once
	err      error
	language string // "" for the person's own languages
	maxSide  int
}

// setup finds the engine's language, once.
func setup() error {
	engineSetup.once.Do(func() {
		engineSetup.err = winrt(func() error {
			statics, err := activationFactory(classOcrEngine, &iidOcrEngineStatics)
			if err != nil {
				return fmt.Errorf("%w: %v", errEngineGone, err)
			}
			defer statics.release()
			max := new(uint32)
			if hr := statics.call(slotMaxImageDimension, uintptr(unsafe.Pointer(max))); !failed(hr) {
				engineSetup.maxSide = int(*max)
			}
			for _, tag := range Languages {
				if ok, _ := languageSupported(statics, tag); ok {
					engineSetup.language = tag
					return nil
				}
			}
			e, err := statics.get(slotTryCreateFromProfile)
			if err != nil {
				return errNoEngine
			}
			e.release()
			return nil
		})
	})
	return engineSetup.err
}

func languageSupported(statics comObject, tag string) (bool, error) {
	lang, err := newLanguage(tag)
	if err != nil {
		return false, err
	}
	defer lang.release()
	ok := new(byte)
	if hr := statics.call(slotIsLanguageSupported, uintptr(lang.p), uintptr(unsafe.Pointer(ok))); failed(hr) {
		return false, hresult(hr)
	}
	return *ok != 0, nil
}

func newLanguage(tag string) (comObject, error) {
	f, err := activationFactory(classLanguage, &iidLanguageFactory)
	if err != nil {
		return comObject{}, err
	}
	defer f.release()
	h, err := newHString(tag)
	if err != nil {
		return comObject{}, err
	}
	defer h.free()
	return f.get(slotCreateLanguage, uintptr(h))
}

// newEngine makes an engine for the language setup found.
func newEngine(statics comObject) (comObject, error) {
	if engineSetup.language == "" {
		e, err := statics.get(slotTryCreateFromProfile)
		if err != nil {
			return comObject{}, errNoEngine
		}
		return e, nil
	}
	lang, err := newLanguage(engineSetup.language)
	if err != nil {
		return comObject{}, err
	}
	defer lang.release()
	e, err := statics.get(slotTryCreateFromLanguage, uintptr(lang.p))
	if err != nil {
		return comObject{}, errNoEngine
	}
	return e, nil
}

func supported() error {
	if err := setup(); err != nil {
		if errors.Is(err, driver.ErrUnsupported) {
			return err
		}
		return fmt.Errorf("%w: %w", err, driver.ErrUnsupported)
	}
	return nil
}

func recognize(img []byte, opt Options) ([]Line, error) {
	if len(img) == 0 {
		return nil, fmt.Errorf("ocr: empty image")
	}
	m, _, err := image.Decode(bytes.NewReader(img))
	if err != nil {
		return nil, fmt.Errorf("ocr: %w", err)
	}
	return recognizeImage(m, opt)
}

func recognizeImage(img image.Image, _ Options) ([]Line, error) {
	if err := supported(); err != nil {
		return nil, err
	}
	if img == nil || img.Bounds().Empty() {
		return nil, fmt.Errorf("ocr: empty image")
	}
	pix, w, h := bgra(img, engineSetup.maxSide)
	var lines []Line
	err := winrt(func() error {
		var err error
		lines, err = recognizeBGRA(pix, w, h)
		return err
	})
	return lines, err
}

func recognizeBGRA(pix []byte, w, h int) ([]Line, error) {
	statics, err := activationFactory(classOcrEngine, &iidOcrEngineStatics)
	if err != nil {
		return nil, err
	}
	defer statics.release()
	engine, err := newEngine(statics)
	if err != nil {
		return nil, err
	}
	defer engine.release()

	cb, err := activationFactory(classCryptographicBuffer, &iidCryptographicBufferStatics)
	if err != nil {
		return nil, err
	}
	defer cb.release()
	// pix is on the heap, where nothing moves, and alive until the copy is
	// made.
	buf, err := cb.get(slotCreateFromByteArray, uintptr(len(pix)), uintptr(unsafe.Pointer(&pix[0])))
	runtime.KeepAlive(pix)
	if err != nil {
		return nil, fmt.Errorf("ocr: copy the picture: %w", err)
	}
	defer buf.release()
	sb, err := activationFactory(classSoftwareBitmap, &iidSoftwareBitmapStatics)
	if err != nil {
		return nil, err
	}
	defer sb.release()
	bitmap, err := sb.get(slotCreateCopyFromBuffer, uintptr(buf.p), bitmapPixelFormatBgra8, uintptr(w), uintptr(h))
	if err != nil {
		return nil, fmt.Errorf("ocr: make the bitmap: %w", err)
	}
	defer bitmap.release()

	op, err := engine.get(slotRecognizeAsync, uintptr(bitmap.p))
	if err != nil {
		return nil, fmt.Errorf("ocr: recognize: %w", err)
	}
	result, err := await(op)
	if err != nil {
		return nil, fmt.Errorf("ocr: recognize: %w", err)
	}
	defer result.release()
	return readLines(result, w, h)
}

// await waits for an IAsyncOperation whose result is an interface, and
// returns the result, releasing the operation.
func await(op comObject) (comObject, error) {
	defer op.release()
	info, err := op.queryInterface(&iidAsyncInfo)
	if err != nil {
		return comObject{}, err
	}
	defer info.release()
	status := new(int32)
	deadline := time.Now().Add(recognizeTimeout)
	for {
		if hr := info.call(slotAsyncStatus, uintptr(unsafe.Pointer(status))); failed(hr) {
			return comObject{}, hresult(hr)
		}
		switch *status {
		case asyncStarted:
			if time.Now().After(deadline) {
				info.call(slotAsyncCancel)
				return comObject{}, errors.New("timed out")
			}
			time.Sleep(5 * time.Millisecond)
		case asyncCompleted:
			return op.get(slotGetResults)
		case asyncCanceled:
			return comObject{}, errors.New("cancelled")
		default: // asyncError
			code := new(int32)
			info.call(slotAsyncErrorCode, uintptr(unsafe.Pointer(code)))
			return comObject{}, hresult(uint32(*code))
		}
	}
}

// rect is Windows.Foundation.Rect.
type rect struct{ X, Y, W, H float32 }

// readLines reads an OcrResult's lines: each line's words, their union as
// its box, normalized to the w x h picture.
func readLines(result comObject, w, h int) ([]Line, error) {
	lines, err := result.get(slotResultLines)
	if err != nil {
		return nil, err
	}
	defer lines.release()
	var out []Line
	err = each(lines, func(line comObject) error {
		words, err := line.get(slotLineWords)
		if err != nil {
			return err
		}
		defer words.release()
		var texts []string
		var box rect
		first := true
		err = each(words, func(word comObject) error {
			r := new(rect)
			if hr := word.call(slotWordBoundingRect, uintptr(unsafe.Pointer(r))); failed(hr) {
				return hresult(hr)
			}
			t, err := word.getString(slotWordText)
			if err != nil {
				return err
			}
			texts = append(texts, t)
			if first {
				box, first = *r, false
			} else {
				box = union(box, *r)
			}
			return nil
		})
		if err != nil || first {
			return err
		}
		out = append(out, Line{
			Text:       joinWords(texts),
			Confidence: 1,
			X:          float64(box.X) / float64(w), Y: float64(box.Y) / float64(h),
			W: float64(box.W) / float64(w), H: float64(box.H) / float64(h),
		})
		return nil
	})
	return out, err
}

// each calls fn with every item of an IVectorView, releasing it after.
func each(v comObject, fn func(comObject) error) error {
	n := new(uint32)
	if hr := v.call(slotVectorSize, uintptr(unsafe.Pointer(n))); failed(hr) {
		return hresult(hr)
	}
	for i := uint32(0); i < *n; i++ {
		item, err := v.get(slotVectorGetAt, uintptr(i))
		if err != nil {
			return err
		}
		err = fn(item)
		item.release()
		if err != nil {
			return err
		}
	}
	return nil
}

func union(a, b rect) rect {
	x0, y0 := min(a.X, b.X), min(a.Y, b.Y)
	x1, y1 := max(a.X+a.W, b.X+b.W), max(a.Y+a.H, b.Y+b.H)
	return rect{X: x0, Y: y0, W: x1 - x0, H: y1 - y0}
}
