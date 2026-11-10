//go:build darwin

package ios

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/ebitengine/purego"
)

// H.264 encoding with macOS's VideoToolbox, called through purego so the
// build stays free of cgo. The paths below iOS 27 get their picture as
// JPEG frames or screenshots; the mirror and the recorder take H.264.

// cmTime is CoreMedia's CMTime.
type cmTime struct {
	Value     int64
	Timescale int32
	Flags     uint32
	Epoch     int64
}

const (
	cmTimeValid = 1

	codecH264          = 0x61766331 // 'avc1'
	pixelFormatNV12    = 0x34323076 // '420v', bi-planar 4:2:0, video range
	cfNumberSInt32Type = 3
	vtTimescale        = 90000
)

var vt struct {
	once sync.Once
	err  error

	createSession   func(alloc uintptr, w, h int32, codec uint32, spec, attrs, dataAlloc, cb, refcon uintptr, out *uintptr) int32
	setProperty     func(session, key, value uintptr) int32
	prepare         func(session uintptr) int32
	encodeFrame     func(session, image uintptr, pts, dur cmTime, props, frameRefcon uintptr, flags *uint32) int32
	completeFrames  func(session uintptr, until cmTime) int32
	invalidate      func(session uintptr)
	cfRelease       func(cf uintptr)
	cfNumberCreate  func(alloc uintptr, typ int, value unsafe.Pointer) uintptr
	cfDictCreate    func(alloc uintptr, keys, values unsafe.Pointer, n int, keyCB, valueCB uintptr) uintptr
	pbCreate        func(alloc uintptr, w, h uintptr, format uint32, attrs uintptr, out *uintptr) int32
	pbLock          func(pb uintptr, flags uint64) int32
	pbUnlock        func(pb uintptr, flags uint64) int32
	pbPlaneBase     func(pb uintptr, plane uintptr) uintptr
	pbPlaneStride   func(pb uintptr, plane uintptr) uintptr
	sbDataBuffer    func(sb uintptr) uintptr
	sbFormat        func(sb uintptr) uintptr
	bbLength        func(bb uintptr) uintptr
	bbCopy          func(bb, offset, length uintptr, dest unsafe.Pointer) int32
	paramSetAtIndex func(fmt uintptr, index uintptr, ptr *uintptr, size *uintptr, count *uintptr, nalLen *int32) int32

	keyRealTime, keyProfile, keyReorder, keyBitRate, keyMaxKeyInterval, keyFrameRate, keyForceKey uintptr
	profileMain, cfTrue, cfFalse, dictKeyCB, dictValueCB                                          uintptr

	callback uintptr
}

// vtEncoders routes VideoToolbox's callback to its encoder by refcon.
var (
	vtEncoders sync.Map // uintptr -> *h264Encoder
	vtNextID   atomic.Uintptr
)

func loadVT() error {
	vt.once.Do(func() {
		libs := map[string]uintptr{}
		for _, name := range []string{"CoreFoundation", "CoreVideo", "CoreMedia", "VideoToolbox"} {
			h, err := purego.Dlopen("/System/Library/Frameworks/"+name+".framework/"+name, purego.RTLD_NOW|purego.RTLD_GLOBAL)
			if err != nil {
				vt.err = fmt.Errorf("load %s: %w", name, err)
				return
			}
			libs[name] = h
		}
		cf, cv, cm, vtb := libs["CoreFoundation"], libs["CoreVideo"], libs["CoreMedia"], libs["VideoToolbox"]
		purego.RegisterLibFunc(&vt.createSession, vtb, "VTCompressionSessionCreate")
		purego.RegisterLibFunc(&vt.setProperty, vtb, "VTSessionSetProperty")
		purego.RegisterLibFunc(&vt.prepare, vtb, "VTCompressionSessionPrepareToEncodeFrames")
		purego.RegisterLibFunc(&vt.encodeFrame, vtb, "VTCompressionSessionEncodeFrame")
		purego.RegisterLibFunc(&vt.completeFrames, vtb, "VTCompressionSessionCompleteFrames")
		purego.RegisterLibFunc(&vt.invalidate, vtb, "VTCompressionSessionInvalidate")
		purego.RegisterLibFunc(&vt.cfRelease, cf, "CFRelease")
		purego.RegisterLibFunc(&vt.cfNumberCreate, cf, "CFNumberCreate")
		purego.RegisterLibFunc(&vt.cfDictCreate, cf, "CFDictionaryCreate")
		purego.RegisterLibFunc(&vt.pbCreate, cv, "CVPixelBufferCreate")
		purego.RegisterLibFunc(&vt.pbLock, cv, "CVPixelBufferLockBaseAddress")
		purego.RegisterLibFunc(&vt.pbUnlock, cv, "CVPixelBufferUnlockBaseAddress")
		purego.RegisterLibFunc(&vt.pbPlaneBase, cv, "CVPixelBufferGetBaseAddressOfPlane")
		purego.RegisterLibFunc(&vt.pbPlaneStride, cv, "CVPixelBufferGetBytesPerRowOfPlane")
		purego.RegisterLibFunc(&vt.sbDataBuffer, cm, "CMSampleBufferGetDataBuffer")
		purego.RegisterLibFunc(&vt.sbFormat, cm, "CMSampleBufferGetFormatDescription")
		purego.RegisterLibFunc(&vt.bbLength, cm, "CMBlockBufferGetDataLength")
		purego.RegisterLibFunc(&vt.bbCopy, cm, "CMBlockBufferCopyDataBytes")
		purego.RegisterLibFunc(&vt.paramSetAtIndex, cm, "CMVideoFormatDescriptionGetH264ParameterSetAtIndex")

		// Global CFStringRef and CFBooleanRef constants: Dlsym gives the
		// variable's address, which holds the object.
		deref := func(lib uintptr, name string) uintptr {
			p, err := purego.Dlsym(lib, name)
			if err != nil || p == 0 {
				if vt.err == nil {
					vt.err = fmt.Errorf("VideoToolbox: no symbol %s", name)
				}
				return 0
			}
			return *(*uintptr)(cPtr(p))
		}
		addr := func(lib uintptr, name string) uintptr {
			p, err := purego.Dlsym(lib, name)
			if err != nil && vt.err == nil {
				vt.err = fmt.Errorf("VideoToolbox: no symbol %s", name)
			}
			return p
		}
		vt.keyRealTime = deref(vtb, "kVTCompressionPropertyKey_RealTime")
		vt.keyProfile = deref(vtb, "kVTCompressionPropertyKey_ProfileLevel")
		vt.keyReorder = deref(vtb, "kVTCompressionPropertyKey_AllowFrameReordering")
		vt.keyBitRate = deref(vtb, "kVTCompressionPropertyKey_AverageBitRate")
		vt.keyMaxKeyInterval = deref(vtb, "kVTCompressionPropertyKey_MaxKeyFrameInterval")
		vt.keyFrameRate = deref(vtb, "kVTCompressionPropertyKey_ExpectedFrameRate")
		vt.keyForceKey = deref(vtb, "kVTEncodeFrameOptionKey_ForceKeyFrame")
		vt.profileMain = deref(vtb, "kVTProfileLevel_H264_Main_AutoLevel")
		vt.cfTrue = deref(cf, "kCFBooleanTrue")
		vt.cfFalse = deref(cf, "kCFBooleanFalse")
		vt.dictKeyCB = addr(cf, "kCFTypeDictionaryKeyCallBacks")
		vt.dictValueCB = addr(cf, "kCFTypeDictionaryValueCallBacks")
		if vt.err != nil {
			return
		}
		vt.callback = purego.NewCallback(vtOutput)
	})
	return vt.err
}

// h264Encoder encodes NV12 pictures of one size to Annex B H.264.
type h264Encoder struct {
	id      uintptr
	session uintptr
	w, h    int

	mu  sync.Mutex
	out []encodedFrame // what the callback produced for the frame in flight
	err error
}

type encodedFrame struct {
	data     []byte // Annex B
	config   []byte // SPS and PPS, Annex B; only with key frames
	keyFrame bool
}

// newH264Encoder starts a real-time H.264 encoder (Main profile, no
// B-frames) for pictures of w by h, which must be even.
func newH264Encoder(w, h, fps, bitRate int) (*h264Encoder, error) {
	if err := loadVT(); err != nil {
		return nil, err
	}
	if w <= 0 || h <= 0 || w%2 != 0 || h%2 != 0 {
		return nil, fmt.Errorf("H.264: bad picture size %dx%d", w, h)
	}
	e := &h264Encoder{id: vtNextID.Add(1), w: w, h: h}
	vtEncoders.Store(e.id, e)
	var s uintptr
	if st := vt.createSession(0, int32(w), int32(h), codecH264, 0, 0, 0, vt.callback, e.id, &s); st != 0 || s == 0 {
		vtEncoders.Delete(e.id)
		return nil, fmt.Errorf("VideoToolbox: create session: OSStatus %d", st)
	}
	e.session = s
	num := func(v int32) uintptr { return vt.cfNumberCreate(0, cfNumberSInt32Type, unsafe.Pointer(&v)) }
	props := []struct {
		key, value uintptr
		release    bool
	}{
		{vt.keyRealTime, vt.cfTrue, false},
		{vt.keyProfile, vt.profileMain, false},
		{vt.keyReorder, vt.cfFalse, false},
		{vt.keyBitRate, num(int32(bitRate)), true},
		{vt.keyMaxKeyInterval, num(int32(fps * 20)), true},
		{vt.keyFrameRate, num(int32(fps)), true},
	}
	for _, p := range props {
		vt.setProperty(s, p.key, p.value) // best effort: an encoder may lack one
		if p.release && p.value != 0 {
			vt.cfRelease(p.value)
		}
	}
	vt.prepare(s)
	return e, nil
}

// vtOutput is VideoToolbox's output callback. It runs on one of
// VideoToolbox's threads while EncodeFrame or CompleteFrames waits.
func vtOutput(refcon, frameRefcon uintptr, status int32, flags uint32, sample uintptr) {
	v, ok := vtEncoders.Load(refcon)
	if !ok {
		return
	}
	e := v.(*h264Encoder)
	if status != 0 || sample == 0 {
		e.mu.Lock()
		if status != 0 {
			e.err = fmt.Errorf("VideoToolbox: encode: OSStatus %d", status)
		}
		e.mu.Unlock()
		return // a dropped frame
	}
	bb := vt.sbDataBuffer(sample)
	if bb == 0 {
		return
	}
	n := vt.bbLength(bb)
	avcc := make([]byte, n)
	if n > 0 && vt.bbCopy(bb, 0, n, unsafe.Pointer(&avcc[0])) != 0 {
		return
	}
	f := encodedFrame{data: avccToAnnexB(avcc, 4)}
	f.keyFrame = hasIDR(f.data)
	if f.keyFrame {
		f.config = parameterSets(vt.sbFormat(sample))
	}
	e.mu.Lock()
	e.out = append(e.out, f)
	e.mu.Unlock()
}

// parameterSets reads the SPS and PPS of a format description, Annex B.
func parameterSets(fd uintptr) []byte {
	if fd == 0 {
		return nil
	}
	var out []byte
	var count uintptr
	var nalLen int32
	for i := uintptr(0); ; i++ {
		var p, size uintptr
		if vt.paramSetAtIndex(fd, i, &p, &size, &count, &nalLen) != 0 || p == 0 {
			break
		}
		out = append(out, 0, 0, 0, 1)
		out = append(out, unsafe.Slice((*byte)(cPtr(p)), size)...)
		if i+1 >= count {
			break
		}
	}
	return out
}

// encode encodes one picture given as NV12 planes: y is w*h bytes, uv
// w*h/2 bytes of interleaved Cb and Cr. A forced key frame starts a
// self-contained picture, for a viewer that joins. It returns what the
// encoder produced, which is one frame, or none when it dropped one.
func (e *h264Encoder) encode(y, uv []byte, pts time.Duration, forceKey bool) ([]encodedFrame, error) {
	if len(y) < e.w*e.h || len(uv) < e.w*e.h/2 {
		return nil, errors.New("H.264: short picture")
	}
	var pb uintptr
	if st := vt.pbCreate(0, uintptr(e.w), uintptr(e.h), pixelFormatNV12, 0, &pb); st != 0 || pb == 0 {
		return nil, fmt.Errorf("CoreVideo: create pixel buffer: %d", st)
	}
	defer vt.cfRelease(pb)
	vt.pbLock(pb, 0)
	copyPlane(vt.pbPlaneBase(pb, 0), vt.pbPlaneStride(pb, 0), y, e.w, e.h)
	copyPlane(vt.pbPlaneBase(pb, 1), vt.pbPlaneStride(pb, 1), uv, e.w, e.h/2)
	vt.pbUnlock(pb, 0)

	var props uintptr
	if forceKey {
		keys := [1]uintptr{vt.keyForceKey}
		vals := [1]uintptr{vt.cfTrue}
		props = vt.cfDictCreate(0, unsafe.Pointer(&keys[0]), unsafe.Pointer(&vals[0]), 1, vt.dictKeyCB, vt.dictValueCB)
		defer vt.cfRelease(props)
	}
	t := cmTime{Value: int64(pts / (time.Second / vtTimescale)), Timescale: vtTimescale, Flags: cmTimeValid}
	invalid := cmTime{}
	var flags uint32
	if st := vt.encodeFrame(e.session, pb, t, invalid, props, 0, &flags); st != 0 {
		return nil, fmt.Errorf("VideoToolbox: encode: OSStatus %d", st)
	}
	// Wait for the frame: Pinky streams with no delay.
	vt.completeFrames(e.session, t)
	runtime.KeepAlive(y)
	runtime.KeepAlive(uv)
	e.mu.Lock()
	out, err := e.out, e.err
	e.out, e.err = nil, nil
	e.mu.Unlock()
	return out, err
}

func copyPlane(dst, stride uintptr, src []byte, w, h int) {
	if dst == 0 {
		return
	}
	for row := 0; row < h; row++ {
		d := unsafe.Slice((*byte)(unsafe.Add(cPtr(dst), uintptr(row)*stride)), w)
		copy(d, src[row*w:(row+1)*w])
	}
}

func (e *h264Encoder) Close() {
	if e.session != 0 {
		vt.completeFrames(e.session, cmTime{})
		vt.invalidate(e.session)
		vt.cfRelease(e.session)
		e.session = 0
	}
	vtEncoders.Delete(e.id)
}

// cPtr turns an address C handed out into a pointer. The memory is C's, so
// Go's garbage collector never moves it.
func cPtr(p uintptr) unsafe.Pointer { return *(*unsafe.Pointer)(unsafe.Pointer(&p)) }

// h264Available reports whether this computer can encode H.264.
func h264Available() error { return loadVT() }
