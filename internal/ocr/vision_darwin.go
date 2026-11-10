//go:build darwin

package ocr

import (
	"fmt"
	"image"
	"runtime"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
	"github.com/ebitengine/purego/objc"
)

// OCR with Apple's Vision framework, called through purego so the build
// stays free of cgo.

type cgPoint struct{ X, Y float64 }
type cgSize struct{ W, H float64 }
type cgRect struct {
	Origin cgPoint
	Size   cgSize
}

var vision struct {
	once sync.Once
	err  error

	poolPush func() uintptr
	poolPop  func(uintptr)

	nsData, nsDict, nsArray, nsString, handler, request objc.Class

	alloc, init, release, dataWithBytes, dictionary, array, addObject,
	stringWithUTF8, initWithData, setLevel, setCorrection, setLanguages,
	setAutoLanguage, perform, results, count, objectAtIndex, topCandidates,
	str, utf8, confidence, boundingBox, respondsTo objc.SEL
}

func loadVision() error {
	vision.once.Do(func() {
		for _, lib := range []string{
			"/System/Library/Frameworks/Foundation.framework/Foundation",
			"/System/Library/Frameworks/Vision.framework/Vision",
		} {
			if _, err := purego.Dlopen(lib, purego.RTLD_NOW|purego.RTLD_GLOBAL); err != nil {
				vision.err = fmt.Errorf("load %s: %w", lib, err)
				return
			}
		}
		objcLib, err := purego.Dlopen("/usr/lib/libobjc.A.dylib", purego.RTLD_NOW|purego.RTLD_GLOBAL)
		if err != nil {
			vision.err = err
			return
		}
		purego.RegisterLibFunc(&vision.poolPush, objcLib, "objc_autoreleasePoolPush")
		purego.RegisterLibFunc(&vision.poolPop, objcLib, "objc_autoreleasePoolPop")
		v := &vision
		v.nsData = objc.GetClass("NSData")
		v.nsDict = objc.GetClass("NSDictionary")
		v.nsArray = objc.GetClass("NSMutableArray")
		v.nsString = objc.GetClass("NSString")
		v.handler = objc.GetClass("VNImageRequestHandler")
		v.request = objc.GetClass("VNRecognizeTextRequest")
		if v.handler == 0 || v.request == 0 {
			vision.err = fmt.Errorf("Vision text recognition is not available")
			return
		}
		v.alloc = objc.RegisterName("alloc")
		v.init = objc.RegisterName("init")
		v.release = objc.RegisterName("release")
		v.dataWithBytes = objc.RegisterName("dataWithBytes:length:")
		v.dictionary = objc.RegisterName("dictionary")
		v.array = objc.RegisterName("array")
		v.addObject = objc.RegisterName("addObject:")
		v.stringWithUTF8 = objc.RegisterName("stringWithUTF8String:")
		v.initWithData = objc.RegisterName("initWithData:options:")
		v.setLevel = objc.RegisterName("setRecognitionLevel:")
		v.setCorrection = objc.RegisterName("setUsesLanguageCorrection:")
		v.setLanguages = objc.RegisterName("setRecognitionLanguages:")
		v.setAutoLanguage = objc.RegisterName("setAutomaticallyDetectsLanguage:")
		v.perform = objc.RegisterName("performRequests:error:")
		v.results = objc.RegisterName("results")
		v.count = objc.RegisterName("count")
		v.objectAtIndex = objc.RegisterName("objectAtIndex:")
		v.topCandidates = objc.RegisterName("topCandidates:")
		v.str = objc.RegisterName("string")
		v.utf8 = objc.RegisterName("UTF8String")
		v.confidence = objc.RegisterName("confidence")
		v.boundingBox = objc.RegisterName("boundingBox")
		v.respondsTo = objc.RegisterName("respondsToSelector:")
	})
	return vision.err
}

// goString copies a NUL-terminated C string.
func goString(p uintptr) string {
	if p == 0 {
		return ""
	}
	// Reinterpret rather than convert, so vet sees no uintptr arithmetic.
	ptr := *(*unsafe.Pointer)(unsafe.Pointer(&p))
	var n int
	for *(*byte)(unsafe.Add(ptr, n)) != 0 {
		n++
	}
	return string(unsafe.Slice((*byte)(ptr), n))
}

func supported() error { return loadVision() }

// recognizeImage hands Vision the picture as an uncompressed BMP, which
// costs far less to make than a PNG or JPEG and to read back.
func recognizeImage(img image.Image, opt Options) ([]Line, error) {
	if img == nil || img.Bounds().Empty() {
		return nil, fmt.Errorf("ocr: empty image")
	}
	return recognize(encodeBMP(img), opt)
}

// recognize runs Vision's text recognizer on an encoded image (PNG, JPEG
// or BMP) and returns the lines it read, with boxes normalized to 0..1
// from the top-left corner.
func recognize(img []byte, opt Options) ([]Line, error) {
	if err := loadVision(); err != nil {
		return nil, err
	}
	if len(img) == 0 {
		return nil, fmt.Errorf("ocr: empty image")
	}
	// Autorelease pools belong to a thread.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	v := &vision
	pool := v.poolPush()
	defer v.poolPop(pool)

	data := objc.ID(v.nsData).Send(v.dataWithBytes, unsafe.Pointer(&img[0]), uint64(len(img)))
	runtime.KeepAlive(img)
	opts := objc.ID(v.nsDict).Send(v.dictionary)
	h := objc.ID(v.handler).Send(v.alloc).Send(v.initWithData, data, opts)
	if h == 0 {
		return nil, fmt.Errorf("ocr: cannot read the image")
	}
	defer h.Send(v.release)
	req := objc.ID(v.request).Send(v.alloc).Send(v.init)
	defer req.Send(v.release)
	level := uint64(0) // accurate
	if opt.Fast {
		level = 1
	}
	req.Send(v.setLevel, level)
	req.Send(v.setCorrection, !opt.Fast)
	if len(opt.Languages) > 0 {
		langs := objc.ID(v.nsArray).Send(v.array)
		for _, l := range opt.Languages {
			langs.Send(v.addObject, objc.ID(v.nsString).Send(v.stringWithUTF8, l))
		}
		req.Send(v.setLanguages, langs)
	}
	if opt.AutoLanguage && objc.Send[bool](req, v.respondsTo, v.setAutoLanguage) {
		req.Send(v.setAutoLanguage, true)
	}
	reqs := objc.ID(v.nsArray).Send(v.array)
	reqs.Send(v.addObject, req)
	if ok := objc.Send[bool](h, v.perform, reqs, uintptr(0)); !ok {
		return nil, fmt.Errorf("ocr: Vision failed to recognize text")
	}
	res := req.Send(v.results)
	if res == 0 {
		return nil, nil
	}
	n := objc.Send[uint64](res, v.count)
	out := make([]Line, 0, n)
	for i := uint64(0); i < n; i++ {
		obs := res.Send(v.objectAtIndex, i)
		cands := obs.Send(v.topCandidates, uint64(1))
		if cands == 0 || objc.Send[uint64](cands, v.count) == 0 {
			continue
		}
		top := cands.Send(v.objectAtIndex, uint64(0))
		text := goString(objc.Send[uintptr](top.Send(v.str), v.utf8))
		conf := objc.Send[float32](top, v.confidence)
		bb := objc.Send[cgRect](obs, v.boundingBox)
		out = append(out, Line{
			Text:       text,
			Confidence: conf,
			// Vision's origin is the bottom-left corner.
			X: bb.Origin.X, Y: 1 - bb.Origin.Y - bb.Size.H, W: bb.Size.W, H: bb.Size.H,
		})
	}
	return out, nil
}
