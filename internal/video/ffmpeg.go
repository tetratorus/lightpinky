// Package video decodes the phones' H.264 and HEVC streams for the desktop
// client. It calls FFmpeg's libavcodec, libavutil and libswscale through
// purego, so Pinky builds without cgo and loads FFmpeg only when a mirror
// shows.
//
// The struct offsets below are those of FFmpeg 9 (libavcodec 63, libavutil
// 61, libswscale 10). Load refuses other major versions, whose layouts
// differ, and checks the layout at run time before the first decode.
package video

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
)

// The FFmpeg major versions this package is built for (FFmpeg 9).
const (
	avcodecMajor = 63
	avutilMajor  = 61
	swscaleMajor = 10
)

// Offsets of the fields Pinky reads and writes, for FFmpeg 9 on 64-bit
// platforms. They were printed with offsetof() against FFmpeg 9.0's
// headers; checkLayout verifies them at run time.
const (
	frameData          = 0   // AVFrame.data[8]
	frameLinesize      = 64  // AVFrame.linesize[8]
	frameWidth         = 104 // AVFrame.width
	frameHeight        = 108 // AVFrame.height
	frameFormat        = 116 // AVFrame.format
	framePTS           = 136 // AVFrame.pts
	frameBuf           = 184 // AVFrame.buf[8]
	frameColorRange    = 280 // AVFrame.color_range
	frameColorPrim     = 284 // AVFrame.color_primaries
	frameColorTRC      = 288 // AVFrame.color_trc
	frameColorspace    = 292 // AVFrame.colorspace
	frameChromaLoc     = 296 // AVFrame.chroma_location
	packetBuf          = 0   // AVPacket.buf
	packetPTS          = 8   // AVPacket.pts
	packetDTS          = 16  // AVPacket.dts
	packetData         = 24  // AVPacket.data
	packetSize         = 32  // AVPacket.size
	packetFlags        = 40  // AVPacket.flags
	ctxCodecID         = 24  // AVCodecContext.codec_id
	ctxThreadCount     = 656 // AVCodecContext.thread_count
	ctxHWFramesCtx     = 552 // AVCodecContext.hw_frames_ctx
	ctxHWDeviceCtx     = 560 // AVCodecContext.hw_device_ctx
	codecID            = 20  // AVCodec.id
	hwConfigPixFmt     = 0   // AVCodecHWConfig.pix_fmt
	hwConfigMethods    = 4   // AVCodecHWConfig.methods
	hwConfigDeviceType = 8   // AVCodecHWConfig.device_type
)

// Constants of FFmpeg's API.
const (
	avNoPTS               = -0x8000000000000000 // AV_NOPTS_VALUE
	avPktFlagKey          = 1                   // AV_PKT_FLAG_KEY
	avHWConfigDeviceCtx   = 1                   // AV_CODEC_HW_CONFIG_METHOD_HW_DEVICE_CTX
	avHWFrameMapRead      = 1                   // AV_HWFRAME_MAP_READ
	avLogError            = 16                  // AV_LOG_ERROR
	avLogQuiet            = -8                  // AV_LOG_QUIET
	avInputBufferPadding  = 64                  // AV_INPUT_BUFFER_PADDING_SIZE
	avErrorEOF            = -541478725          // AVERROR_EOF
	swsFastBilinear       = 1                   // SWS_FAST_BILINEAR
	swsBilinear           = 2                   // SWS_BILINEAR
	avOptSearchChildren   = 1                   // AV_OPT_SEARCH_CHILDREN
	avColorRangeMPEG      = 1                   // AVCOL_RANGE_MPEG, video's 16 to 235
	avColorRangeJPEG      = 2                   // AVCOL_RANGE_JPEG, all of 0 to 255
	maxPlausibleDimension = 16384
)

// avErrorEAGAIN is AVERROR(EAGAIN), which follows the platform's errno.
var avErrorEAGAIN = func() int32 {
	switch runtime.GOOS {
	case "darwin", "ios", "freebsd", "netbsd", "openbsd":
		return -35
	default: // Linux, and Windows builds of FFmpeg (MinGW and MSVC)
		return -11
	}
}()

// ErrNotFound reports that FFmpeg's libraries are not installed where
// Pinky looks for them. Its message says where it looked and how to fix it.
var ErrNotFound = errors.New("FFmpeg 9 not found")

// lib holds FFmpeg's functions once loaded.
type lib struct {
	dirs []string // where the libraries were found, for messages

	// libavutil
	avutil_version                func() uint32
	av_frame_alloc                func() unsafe.Pointer
	av_frame_free                 func(*unsafe.Pointer)
	av_frame_unref                func(unsafe.Pointer)
	av_frame_get_buffer           func(unsafe.Pointer, int32) int32
	av_hwdevice_find_type_by_name func(string) int32
	av_hwdevice_ctx_create        func(*unsafe.Pointer, int32, unsafe.Pointer, unsafe.Pointer, int32) int32
	av_hwframe_transfer_data      func(unsafe.Pointer, unsafe.Pointer, int32) int32
	av_hwframe_map                func(unsafe.Pointer, unsafe.Pointer, int32) int32
	av_buffer_ref                 func(unsafe.Pointer) unsafe.Pointer
	av_buffer_unref               func(*unsafe.Pointer)
	av_get_pix_fmt                func(string) int32
	av_get_pix_fmt_name           func(int32) string
	av_strerror                   func(int32, *byte, uintptr) int32
	av_log_set_level              func(int32)
	av_opt_set                    func(unsafe.Pointer, string, string, int32) int32
	av_opt_set_int                func(unsafe.Pointer, string, int64, int32) int32
	av_opt_get_int                func(unsafe.Pointer, string, int32, *int64) int32

	// libavcodec
	avcodec_version              func() uint32
	avcodec_find_decoder_by_name func(string) unsafe.Pointer
	avcodec_get_hw_config        func(unsafe.Pointer, int32) unsafe.Pointer
	avcodec_alloc_context3       func(unsafe.Pointer) unsafe.Pointer
	avcodec_free_context         func(*unsafe.Pointer)
	avcodec_open2                func(unsafe.Pointer, unsafe.Pointer, unsafe.Pointer) int32
	avcodec_send_packet          func(unsafe.Pointer, unsafe.Pointer) int32
	avcodec_receive_frame        func(unsafe.Pointer, unsafe.Pointer) int32
	avcodec_flush_buffers        func(unsafe.Pointer)
	av_packet_alloc              func() unsafe.Pointer
	av_packet_free               func(*unsafe.Pointer)
	av_new_packet                func(unsafe.Pointer, int32) int32
	av_packet_unref              func(unsafe.Pointer)

	// libswscale
	swscale_version   func() uint32
	sws_alloc_context func() unsafe.Pointer
	sws_free_context  func(*unsafe.Pointer)
	sws_scale_frame   func(unsafe.Pointer, unsafe.Pointer, unsafe.Pointer) int32

	pixRGBA     int32 // AV_PIX_FMT_RGBA
	pixNV12     int32 // AV_PIX_FMT_NV12, as hardware decoders leave pictures
	pixYUV420P  int32 // AV_PIX_FMT_YUV420P (I420), as the software decoder does
	pixYUVJ420P int32 // AV_PIX_FMT_YUVJ420P, the same in full range
}

var (
	loadOnce sync.Once
	loaded   *lib
	loadErr  error
)

// Load finds and loads FFmpeg. It is called by NewDecoder, and the client
// calls it at start so a missing FFmpeg is reported at once. It is safe to
// call more than once; later calls return the first result.
//
// Pinky looks in PINKY_FFMPEG_DIR, in the app bundle's Frameworks folder
// on macOS (Pinky.app carries its own FFmpeg there), next to the
// executable, and then where Homebrew, the system's package manager or the
// dynamic loader put libraries.
func Load() error {
	loadOnce.Do(func() { loaded, loadErr = load() })
	return loadErr
}

// Version describes the loaded FFmpeg, as "libavcodec 63.1.102 (/opt/homebrew/lib)".
func Version() string {
	if Load() != nil {
		return ""
	}
	v := loaded.avcodec_version()
	return fmt.Sprintf("libavcodec %d.%d.%d (%s)", v>>16, v>>8&0xff, v&0xff, strings.Join(loaded.dirs, ", "))
}

func libNames() (avutil, avcodec, swscale string) {
	switch runtime.GOOS {
	case "darwin":
		return fmt.Sprintf("libavutil.%d.dylib", avutilMajor),
			fmt.Sprintf("libavcodec.%d.dylib", avcodecMajor),
			fmt.Sprintf("libswscale.%d.dylib", swscaleMajor)
	case "windows":
		return fmt.Sprintf("avutil-%d.dll", avutilMajor),
			fmt.Sprintf("avcodec-%d.dll", avcodecMajor),
			fmt.Sprintf("swscale-%d.dll", swscaleMajor)
	default:
		return fmt.Sprintf("libavutil.so.%d", avutilMajor),
			fmt.Sprintf("libavcodec.so.%d", avcodecMajor),
			fmt.Sprintf("libswscale.so.%d", swscaleMajor)
	}
}

// swresampleMajor is libswresample's major version in FFmpeg 9. Pinky does
// not call it, but some builds of libavcodec (BtbN's, for one) link it.
const swresampleMajor = 7

// preloadSwresample loads libswresample from dir, the folder libavutil came
// from, when it is there. A library's dependencies are looked for on the
// dynamic loader's own path, not in the library's folder, so a libavcodec
// that links libswresample fails to load from a folder of its own (such as
// PINKY_FFMPEG_DIR) unless libswresample is loaded first.
func preloadSwresample(dir string) {
	if !filepath.IsAbs(dir) {
		return
	}
	var name string
	switch runtime.GOOS {
	case "darwin":
		name = fmt.Sprintf("libswresample.%d.dylib", swresampleMajor)
	case "windows":
		name = fmt.Sprintf("swresample-%d.dll", swresampleMajor)
	default:
		name = fmt.Sprintf("libswresample.so.%d", swresampleMajor)
	}
	path := filepath.Join(dir, name)
	if _, err := os.Stat(path); err == nil {
		openLibrary(path) // best effort: libavcodec reports what is missing
	}
}

// hwLibrariesLoad reports whether the system libraries a hardware decoder
// needs load. Some FFmpeg builds load them lazily and abort the process
// without them: BtbN's Linux builds load libva on first use and fail an
// assertion when it is missing. So Pinky asks for VA-API only where libva
// and its DRM backend load.
func hwLibrariesLoad(device string) bool {
	if device != "vaapi" {
		return true
	}
	for _, name := range []string{"libva.so.2", "libva-drm.so.2"} {
		if h, err := openLibrary(name); err != nil || h == 0 {
			return false
		}
	}
	return true
}

// LibraryDirs returns the folders Load looks in for FFmpeg's libraries, in
// order, for code that loads more of FFmpeg (such as libavformat) from the
// same place. An empty entry means the dynamic loader's own search path.
func LibraryDirs() []string { return searchDirs() }

// searchDirs lists the folders to try, in order: PINKY_FFMPEG_DIR, the
// app bundle's Frameworks folder (where make app puts Pinky's own build),
// the executable's folder, then where Homebrew or the system's package
// manager put FFmpeg. An empty entry means the dynamic loader's own search
// path.
func searchDirs() []string {
	var dirs []string
	if d := os.Getenv("PINKY_FFMPEG_DIR"); d != "" {
		dirs = append(dirs, d)
	}
	if exe, err := os.Executable(); err == nil {
		if real, err := filepath.EvalSymlinks(exe); err == nil {
			exe = real
		}
		dir := filepath.Dir(exe)
		if runtime.GOOS == "darwin" {
			// Pinky.app/Contents/MacOS/Pinky -> Pinky.app/Contents/Frameworks
			dirs = append(dirs, filepath.Join(filepath.Dir(dir), "Frameworks"))
		}
		dirs = append(dirs, dir)
	}
	switch runtime.GOOS {
	case "darwin":
		dirs = append(dirs, "/opt/homebrew/opt/ffmpeg/lib", "/opt/homebrew/lib", "/usr/local/opt/ffmpeg/lib", "/usr/local/lib")
	case "linux", "freebsd", "netbsd":
		dirs = append(dirs, "", "/usr/lib/x86_64-linux-gnu", "/usr/lib/aarch64-linux-gnu", "/usr/lib64", "/usr/lib", "/usr/local/lib")
	case "windows":
		dirs = append(dirs, "")
	}
	return dirs
}

func load() (*lib, error) {
	avutilName, avcodecName, swscaleName := libNames()
	open := func(name string) (uintptr, string, error) {
		var errs []string
		for _, dir := range searchDirs() {
			path := name
			if dir != "" {
				path = filepath.Join(dir, name)
				if _, err := os.Stat(path); err != nil {
					continue
				}
			}
			h, err := openLibrary(path)
			if err == nil {
				if dir == "" {
					dir = "system search path"
				}
				return h, dir, nil
			}
			errs = append(errs, err.Error())
		}
		where := strings.Join(nonEmpty(searchDirs()), ", ")
		msg := fmt.Sprintf("%v: no %s in %s or the system's library path", ErrNotFound, name, where)
		if len(errs) > 0 {
			msg += " (" + strings.Join(errs, "; ") + ")"
		}
		return 0, "", fmt.Errorf("%s. %s: %w", msg, installHint(), ErrNotFound)
	}

	l := &lib{}
	hAvutil, d1, err := open(avutilName)
	if err != nil {
		return nil, err
	}
	preloadSwresample(d1)
	hAvcodec, d2, err := open(avcodecName)
	if err != nil {
		return nil, err
	}
	hSwscale, d3, err := open(swscaleName)
	if err != nil {
		return nil, err
	}
	l.dirs = uniq(d1, d2, d3)

	var missing []string
	reg := func(fptr any, h uintptr, name string) {
		sym, err := lookupSymbol(h, name)
		if err != nil || sym == 0 {
			missing = append(missing, name)
			return
		}
		purego.RegisterFunc(fptr, sym)
	}
	reg(&l.avutil_version, hAvutil, "avutil_version")
	reg(&l.av_frame_alloc, hAvutil, "av_frame_alloc")
	reg(&l.av_frame_free, hAvutil, "av_frame_free")
	reg(&l.av_frame_unref, hAvutil, "av_frame_unref")
	reg(&l.av_frame_get_buffer, hAvutil, "av_frame_get_buffer")
	reg(&l.av_hwdevice_find_type_by_name, hAvutil, "av_hwdevice_find_type_by_name")
	reg(&l.av_hwdevice_ctx_create, hAvutil, "av_hwdevice_ctx_create")
	reg(&l.av_hwframe_transfer_data, hAvutil, "av_hwframe_transfer_data")
	reg(&l.av_hwframe_map, hAvutil, "av_hwframe_map")
	reg(&l.av_buffer_ref, hAvutil, "av_buffer_ref")
	reg(&l.av_buffer_unref, hAvutil, "av_buffer_unref")
	reg(&l.av_get_pix_fmt, hAvutil, "av_get_pix_fmt")
	reg(&l.av_get_pix_fmt_name, hAvutil, "av_get_pix_fmt_name")
	reg(&l.av_strerror, hAvutil, "av_strerror")
	reg(&l.av_log_set_level, hAvutil, "av_log_set_level")
	reg(&l.av_opt_set, hAvutil, "av_opt_set")
	reg(&l.av_opt_set_int, hAvutil, "av_opt_set_int")
	reg(&l.av_opt_get_int, hAvutil, "av_opt_get_int")

	reg(&l.avcodec_version, hAvcodec, "avcodec_version")
	reg(&l.avcodec_find_decoder_by_name, hAvcodec, "avcodec_find_decoder_by_name")
	reg(&l.avcodec_get_hw_config, hAvcodec, "avcodec_get_hw_config")
	reg(&l.avcodec_alloc_context3, hAvcodec, "avcodec_alloc_context3")
	reg(&l.avcodec_free_context, hAvcodec, "avcodec_free_context")
	reg(&l.avcodec_open2, hAvcodec, "avcodec_open2")
	reg(&l.avcodec_send_packet, hAvcodec, "avcodec_send_packet")
	reg(&l.avcodec_receive_frame, hAvcodec, "avcodec_receive_frame")
	reg(&l.avcodec_flush_buffers, hAvcodec, "avcodec_flush_buffers")
	reg(&l.av_packet_alloc, hAvcodec, "av_packet_alloc")
	reg(&l.av_packet_free, hAvcodec, "av_packet_free")
	reg(&l.av_new_packet, hAvcodec, "av_new_packet")
	reg(&l.av_packet_unref, hAvcodec, "av_packet_unref")

	reg(&l.swscale_version, hSwscale, "swscale_version")
	reg(&l.sws_alloc_context, hSwscale, "sws_alloc_context")
	reg(&l.sws_free_context, hSwscale, "sws_free_context")
	reg(&l.sws_scale_frame, hSwscale, "sws_scale_frame")
	if len(missing) > 0 {
		return nil, fmt.Errorf("FFmpeg in %s lacks %s; Pinky needs FFmpeg 9: %w",
			strings.Join(l.dirs, ", "), strings.Join(missing, ", "), ErrNotFound)
	}

	for _, v := range []struct {
		name string
		got  uint32
		want uint32
	}{
		{"libavcodec", l.avcodec_version() >> 16, avcodecMajor},
		{"libavutil", l.avutil_version() >> 16, avutilMajor},
		{"libswscale", l.swscale_version() >> 16, swscaleMajor},
	} {
		if v.got != v.want {
			return nil, fmt.Errorf("%s %d in %s; Pinky needs FFmpeg 9 (%s %d). %s: %w",
				v.name, v.got, strings.Join(l.dirs, ", "), v.name, v.want, installHint(), ErrNotFound)
		}
	}

	if os.Getenv("PINKY_FFMPEG_LOG") == "" {
		l.av_log_set_level(avLogQuiet)
	} else {
		l.av_log_set_level(avLogError)
	}
	l.pixRGBA = l.av_get_pix_fmt("rgba")
	l.pixNV12 = l.av_get_pix_fmt("nv12")
	l.pixYUV420P = l.av_get_pix_fmt("yuv420p")
	l.pixYUVJ420P = l.av_get_pix_fmt("yuvj420p")
	if l.pixRGBA < 0 || l.pixNV12 < 0 || l.pixYUV420P < 0 {
		return nil, errors.New("video: FFmpeg does not know the rgba, nv12 or yuv420p pixel format")
	}
	if err := checkLayout(l); err != nil {
		return nil, err
	}
	return l, nil
}

// checkLayout verifies the struct offsets against what FFmpeg itself
// writes, so a build of FFmpeg 9 with another layout fails here with a
// clear error rather than corrupting memory later.
func checkLayout(l *lib) error {
	bad := func(what string) error {
		return fmt.Errorf("video: FFmpeg in %s has an unexpected %s layout; Pinky supports FFmpeg 9.0 on 64-bit systems",
			strings.Join(l.dirs, ", "), what)
	}
	if unsafe.Sizeof(uintptr(0)) != 8 {
		return errors.New("video: Pinky's FFmpeg bindings need a 64-bit system")
	}

	f := l.av_frame_alloc()
	if f == nil {
		return errors.New("video: av_frame_alloc failed")
	}
	defer l.av_frame_free(&f)
	putInt32(f, frameWidth, 16)
	putInt32(f, frameHeight, 8)
	putInt32(f, frameFormat, l.pixRGBA)
	if r := l.av_frame_get_buffer(f, 0); r < 0 {
		return bad("AVFrame")
	}
	if ptrAt(f, frameData) == nil || getInt32(f, frameLinesize) < 64 || ptrAt(f, frameBuf) == nil {
		return bad("AVFrame")
	}

	p := l.av_packet_alloc()
	if p == nil {
		return errors.New("video: av_packet_alloc failed")
	}
	defer l.av_packet_free(&p)
	if r := l.av_new_packet(p, 100); r < 0 {
		return bad("AVPacket")
	}
	if getInt32(p, packetSize) != 100 || ptrAt(p, packetData) == nil || ptrAt(p, packetBuf) == nil {
		return bad("AVPacket")
	}
	l.av_packet_unref(p)

	codec := l.avcodec_find_decoder_by_name("h264")
	if codec == nil {
		return errors.New("video: this FFmpeg has no H.264 decoder")
	}
	ctx := l.avcodec_alloc_context3(codec)
	if ctx == nil {
		return errors.New("video: avcodec_alloc_context3 failed")
	}
	defer l.avcodec_free_context(&ctx)
	if getInt32(ctx, ctxCodecID) != getInt32(codec, codecID) {
		return bad("AVCodecContext")
	}
	if r := l.av_opt_set_int(ctx, "threads", 3, 0); r < 0 {
		return bad("AVCodecContext")
	}
	if getInt32(ctx, ctxThreadCount) != 3 || ptrAt(ctx, ctxHWDeviceCtx) != nil || ptrAt(ctx, ctxHWFramesCtx) != nil {
		return bad("AVCodecContext")
	}
	return nil
}

func installHint() string {
	switch runtime.GOOS {
	case "darwin":
		return "Install it with `brew install ffmpeg`, or set PINKY_FFMPEG_DIR to a folder holding FFmpeg 9's dylibs"
	case "windows":
		return "Put FFmpeg 9's shared DLLs (avcodec-63.dll, avutil-61.dll, swscale-10.dll) next to pinky.exe, or set PINKY_FFMPEG_DIR"
	default:
		return "Install FFmpeg 9's shared libraries (libavcodec63 or ffmpeg-libs), or set PINKY_FFMPEG_DIR"
	}
}

// averror turns an FFmpeg error code into a Go error.
func (l *lib) averror(op string, code int32) error {
	var buf [128]byte
	msg := fmt.Sprintf("error %d", code)
	if l.av_strerror(code, &buf[0], uintptr(len(buf))) == 0 {
		n := 0
		for n < len(buf) && buf[n] != 0 {
			n++
		}
		msg = string(buf[:n])
	}
	return fmt.Errorf("video: %s: %s", op, msg)
}

func ptrAt(base unsafe.Pointer, off uintptr) unsafe.Pointer {
	return *(*unsafe.Pointer)(unsafe.Add(base, off))
}

func putPtr(base unsafe.Pointer, off uintptr, v unsafe.Pointer) {
	*(*unsafe.Pointer)(unsafe.Add(base, off)) = v
}

func getInt32(base unsafe.Pointer, off uintptr) int32 {
	return *(*int32)(unsafe.Add(base, off))
}

func putInt32(base unsafe.Pointer, off uintptr, v int32) {
	*(*int32)(unsafe.Add(base, off)) = v
}

func getInt64(base unsafe.Pointer, off uintptr) int64 {
	return *(*int64)(unsafe.Add(base, off))
}

func putInt64(base unsafe.Pointer, off uintptr, v int64) {
	*(*int64)(unsafe.Add(base, off)) = v
}

func nonEmpty(s []string) []string {
	var out []string
	for _, v := range s {
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

func uniq(s ...string) []string {
	var out []string
	for _, v := range s {
		dup := false
		for _, o := range out {
			dup = dup || o == v
		}
		if !dup {
			out = append(out, v)
		}
	}
	return out
}
