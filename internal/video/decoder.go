package video

import (
	"errors"
	"fmt"
	"image"
	"os"
	"runtime"
	"time"
	"unsafe"

	"github.com/tetratorus/lightpinky/internal/driver"
)

// DecoderOptions tune a Decoder.
type DecoderOptions struct {
	// Software turns hardware decoding off. PINKY_VIDEO_SOFTWARE=1 does too.
	Software bool
	// Threads is the number of software decoding threads; 0 picks one per
	// core, up to 8. Slice threads only, which add no latency.
	Threads int
	// Smooth scales with bilinear filtering instead of the faster, slightly
	// blockier fast-bilinear when the picture is shown smaller.
	Smooth bool
	// ConvertThreads is the number of threads that convert pictures to
	// RGBA; 0 is 2, which halves the time a picture takes for no more CPU
	// in all (measured on an M4 Pro). Many threads cost more CPU in all.
	ConvertThreads int
}

// Decoder decodes one H.264 or HEVC stream of Annex B packets and converts
// its pictures to RGBA (Convert) or copies their Y′CbCr planes for a UI
// that converts them as it draws (ConvertYUV). It is not safe for
// concurrent use: a mirror runs it on its own goroutine.
type Decoder struct {
	l        *lib
	codec    driver.Codec
	ctx      unsafe.Pointer // AVCodecContext*
	pkt      unsafe.Pointer // AVPacket*
	frame    unsafe.Pointer // AVFrame*: the decoded picture, maybe in GPU memory
	sw       unsafe.Pointer // AVFrame*: the picture mapped or copied to the CPU
	dst      unsafe.Pointer // AVFrame*: describes the caller's RGBA image
	sws      unsafe.Pointer // SwsContext*
	hwDevice unsafe.Pointer // AVBufferRef*: the hardware device, or nil
	hwPixFmt int32          // the hardware pixel format, -1 without one
	hwName   string         // "videotoolbox", "d3d11va", "vaapi"

	asBT709  bool   // the stream's own color description cannot be converted
	config   []byte // a configuration packet waiting for the next frame
	have     bool   // frame holds a picture
	hwFrames int    // pictures that came from the hardware decoder
	swFrames int    // pictures decoded in software
	mapFails int    // pictures that could not be mapped and were copied
	closed   bool
}

// hwDeviceName is the FFmpeg hardware device type for the platform's
// decoder API.
func hwDeviceName() string {
	switch runtime.GOOS {
	case "darwin":
		return "videotoolbox"
	case "windows":
		return "d3d11va"
	case "linux", "freebsd":
		return "vaapi"
	}
	return ""
}

// NewDecoder returns a decoder for the codec, with hardware decoding when
// the platform's decoder (VideoToolbox, D3D11VA, VA-API) takes the stream,
// and FFmpeg's software decoder otherwise. FFmpeg falls back to software by
// itself when the hardware refuses a stream after it started.
func NewDecoder(codec driver.Codec, opt DecoderOptions) (*Decoder, error) {
	if err := Load(); err != nil {
		return nil, err
	}
	l := loaded
	name := ""
	switch codec {
	case driver.H264:
		name = "h264"
	case driver.HEVC:
		name = "hevc"
	default:
		return nil, fmt.Errorf("video: unsupported codec %q", codec)
	}
	c := l.avcodec_find_decoder_by_name(name)
	if c == nil {
		return nil, fmt.Errorf("video: this FFmpeg has no %s decoder", name)
	}
	d := &Decoder{l: l, codec: codec, hwPixFmt: -1}
	d.ctx = l.avcodec_alloc_context3(c)
	if d.ctx == nil {
		return nil, errors.New("video: avcodec_alloc_context3 failed")
	}
	threads := opt.Threads
	if threads <= 0 {
		threads = min(runtime.NumCPU(), 8)
	}
	l.av_opt_set_int(d.ctx, "threads", int64(threads), 0)
	l.av_opt_set(d.ctx, "thread_type", "slice", 0)
	l.av_opt_set(d.ctx, "flags", "+low_delay", 0)
	l.av_opt_set(d.ctx, "flags2", "+fast", 0)

	if !opt.Software && os.Getenv("PINKY_VIDEO_SOFTWARE") == "" {
		d.initHardware(c) // falls back to software on any error
	}
	if r := l.avcodec_open2(d.ctx, c, nil); r < 0 {
		err := l.averror("open "+name+" decoder", r)
		d.Close()
		return nil, err
	}
	d.pkt = l.av_packet_alloc()
	d.frame = l.av_frame_alloc()
	d.sw = l.av_frame_alloc()
	d.dst = l.av_frame_alloc()
	d.sws = l.sws_alloc_context()
	if d.pkt == nil || d.frame == nil || d.sw == nil || d.dst == nil || d.sws == nil {
		d.Close()
		return nil, errors.New("video: out of memory")
	}
	flags := int64(swsFastBilinear)
	if opt.Smooth {
		flags = swsBilinear
	}
	l.av_opt_set_int(d.sws, "sws_flags", flags, 0)
	convThreads := opt.ConvertThreads
	if convThreads <= 0 {
		convThreads = 2
	}
	l.av_opt_set_int(d.sws, "threads", int64(convThreads), 0)
	return d, nil
}

func (d *Decoder) initHardware(codec unsafe.Pointer) {
	l := d.l
	name := hwDeviceName()
	if name == "" || !hwLibrariesLoad(name) {
		return
	}
	typ := l.av_hwdevice_find_type_by_name(name)
	if typ <= 0 {
		return
	}
	pixFmt := int32(-1)
	for i := int32(0); ; i++ {
		cfg := l.avcodec_get_hw_config(codec, i)
		if cfg == nil {
			break
		}
		if getInt32(cfg, hwConfigMethods)&avHWConfigDeviceCtx != 0 && getInt32(cfg, hwConfigDeviceType) == typ {
			pixFmt = getInt32(cfg, hwConfigPixFmt)
			break
		}
	}
	if pixFmt < 0 {
		return
	}
	var dev unsafe.Pointer
	if r := l.av_hwdevice_ctx_create(&dev, typ, nil, nil, 0); r < 0 || dev == nil {
		return
	}
	ref := l.av_buffer_ref(dev)
	if ref == nil {
		l.av_buffer_unref(&dev)
		return
	}
	// FFmpeg's default get_format picks the device's pixel format when
	// hw_device_ctx is set, and software when the hardware refuses.
	putPtr(d.ctx, ctxHWDeviceCtx, ref)
	d.hwDevice = dev
	d.hwPixFmt = pixFmt
	d.hwName = name
}

// Codec returns the stream's codec.
func (d *Decoder) Codec() driver.Codec { return d.codec }

// Hardware reports whether the last picture came from the hardware
// decoder, and its API ("videotoolbox"), or "" for software.
func (d *Decoder) Hardware() string {
	if d.have && d.hwPixFmt >= 0 && getInt32(d.frame, frameFormat) == d.hwPixFmt {
		return d.hwName
	}
	if d.hwFrames > 0 && d.swFrames == 0 {
		return d.hwName
	}
	return ""
}

// Send passes one packet of the stream to the decoder. A configuration
// packet (SPS, PPS, VPS) is kept and sent together with the next one, as
// scrcpy's client does. Call Receive after each Send until it returns
// false: the decoder holds at most a few pictures.
func (d *Decoder) Send(p driver.Packet) error {
	if d.closed {
		return errors.New("video: decoder closed")
	}
	if p.Config {
		d.config = append(d.config[:0], p.Data...)
		return nil
	}
	n := len(d.config) + len(p.Data)
	if n == 0 {
		return nil
	}
	if r := d.l.av_new_packet(d.pkt, int32(n)); r < 0 {
		return d.l.averror("allocate packet", r)
	}
	buf := unsafe.Slice((*byte)(ptrAt(d.pkt, packetData)), n)
	copy(buf, d.config)
	copy(buf[len(d.config):], p.Data)
	d.config = d.config[:0]
	pts := int64(p.PTS / time.Microsecond)
	putInt64(d.pkt, packetPTS, pts)
	putInt64(d.pkt, packetDTS, pts)
	if p.KeyFrame {
		putInt32(d.pkt, packetFlags, avPktFlagKey)
	}
	r := d.l.avcodec_send_packet(d.ctx, d.pkt)
	d.l.av_packet_unref(d.pkt)
	if r == avErrorEAGAIN {
		// The decoder is full: the caller must Receive first. Pictures
		// are received after every Send, so this means a broken stream;
		// drop the packet rather than block.
		return nil
	}
	if r < 0 {
		return d.l.averror("decode", r)
	}
	return nil
}

// Picture describes a decoded picture.
type Picture struct {
	Width, Height int           // in the stream's pixels
	PTS           time.Duration // the packet's PTS
	Hardware      bool          // decoded by the platform's hardware decoder
}

// Receive returns the next decoded picture, which stays in the decoder
// until the next Receive or Convert. ok is false when the decoder needs
// another packet first.
func (d *Decoder) Receive() (pic Picture, ok bool, err error) {
	if d.closed {
		return Picture{}, false, errors.New("video: decoder closed")
	}
	d.l.av_frame_unref(d.frame)
	d.have = false
	r := d.l.avcodec_receive_frame(d.ctx, d.frame)
	if r == avErrorEAGAIN || r == avErrorEOF {
		return Picture{}, false, nil
	}
	if r < 0 {
		return Picture{}, false, d.l.averror("receive picture", r)
	}
	w, h := int(getInt32(d.frame, frameWidth)), int(getInt32(d.frame, frameHeight))
	if w <= 0 || h <= 0 || w > maxPlausibleDimension || h > maxPlausibleDimension {
		d.l.av_frame_unref(d.frame)
		return Picture{}, false, fmt.Errorf("video: picture of %dx%d", w, h)
	}
	d.have = true
	hw := d.hwPixFmt >= 0 && getInt32(d.frame, frameFormat) == d.hwPixFmt
	if hw {
		d.hwFrames++
	} else {
		d.swFrames++
	}
	pts := getInt64(d.frame, framePTS)
	var dur time.Duration
	if pts != avNoPTS {
		dur = time.Duration(pts) * time.Microsecond
	}
	return Picture{Width: w, Height: h, PTS: dur, Hardware: hw}, true, nil
}

// Convert writes the last picture Receive returned into dst, scaled to
// fill dst's bounds (callers keep the aspect ratio). dst must start at the
// origin with rows 4×width bytes long, as image.NewRGBA makes it.
func (d *Decoder) Convert(dst *image.RGBA) error {
	if !d.have {
		return errors.New("video: no picture to convert")
	}
	b := dst.Bounds()
	if b.Min != (image.Point{}) || dst.Stride != 4*b.Dx() || b.Dx() <= 0 || b.Dy() <= 0 || len(dst.Pix) < dst.Stride*b.Dy() {
		return errors.New("video: Convert needs an image.NewRGBA picture")
	}
	l := d.l
	src, err := d.onCPU()
	if err != nil {
		return err
	}
	if src == d.sw {
		defer l.av_frame_unref(d.sw)
	}

	// dst describes the caller's pixels, which FFmpeg only writes during
	// the call and never keeps: buf stays nil.
	putPtr(d.dst, frameData, unsafe.Pointer(&dst.Pix[0]))
	putInt32(d.dst, frameLinesize, int32(dst.Stride))
	putInt32(d.dst, frameWidth, int32(b.Dx()))
	putInt32(d.dst, frameHeight, int32(b.Dy()))
	putInt32(d.dst, frameFormat, l.pixRGBA)
	if d.asBT709 {
		tagBT709(src)
	}
	r := l.sws_scale_frame(d.sws, d.dst, src)
	if r < 0 && !d.asBT709 {
		// Some encoders tag their stream with a color description swscale
		// cannot convert, such as the YCgCo matrix of the Mi 10 Pro's
		// encoder. Read the pictures as BT.709 from now on, as players
		// do, rather than show nothing.
		d.asBT709 = true
		tagBT709(src)
		r = l.sws_scale_frame(d.sws, d.dst, src)
	}
	putPtr(d.dst, frameData, nil)
	putInt32(d.dst, frameLinesize, 0)
	runtime.KeepAlive(dst)
	if r < 0 {
		return l.averror("convert picture", r)
	}
	return nil
}

// onCPU returns the last picture where the CPU can read it: the decoder's
// own frame, or for a picture of the hardware decoder, its GPU surface
// mapped for reading where the platform can (no copy on VideoToolbox),
// else copied to the CPU. The caller unrefs d.sw when it returns that.
func (d *Decoder) onCPU() (unsafe.Pointer, error) {
	l := d.l
	src := d.frame
	if d.hwPixFmt < 0 || getInt32(src, frameFormat) != d.hwPixFmt {
		return src, nil
	}
	l.av_frame_unref(d.sw)
	if r := d.mapFrame(src); r < 0 {
		d.mapFails++
		l.av_frame_unref(d.sw)
		if r := l.av_hwframe_transfer_data(d.sw, src, 0); r < 0 {
			return nil, l.averror("copy picture from GPU", r)
		}
	}
	// The decoder's color description holds for the mapped copy.
	for _, off := range []uintptr{frameColorRange, frameColorPrim, frameColorTRC, frameColorspace, frameChromaLoc} {
		putInt32(d.sw, off, getInt32(src, off))
	}
	return d.sw, nil
}

// tagBT709 relabels a picture's matrix, primaries and transfer as BT.709,
// keeping its range and pixels.
func tagBT709(frame unsafe.Pointer) {
	const bt709 = 1 // AVCOL_SPC_BT709, AVCOL_PRI_BT709 and AVCOL_TRC_BT709
	putInt32(frame, frameColorspace, bt709)
	putInt32(frame, frameColorPrim, bt709)
	putInt32(frame, frameColorTRC, bt709)
}

// noMap forces copying hardware pictures, to compare with mapping them.
var noMap = os.Getenv("PINKY_VIDEO_NOMAP") != ""

func (d *Decoder) mapFrame(src unsafe.Pointer) int32 {
	if noMap {
		return -1
	}
	return d.l.av_hwframe_map(d.sw, src, avHWFrameMapRead)
}

// Flush drops the pictures the decoder holds, as after a gap in the
// stream; it decodes again from the next key frame.
func (d *Decoder) Flush() {
	if !d.closed {
		d.l.avcodec_flush_buffers(d.ctx)
		d.config = d.config[:0]
		d.have = false
	}
}

// Stats returns how many pictures came from the hardware and software
// decoders, and how many hardware pictures had to be copied rather than
// mapped.
func (d *Decoder) Stats() (hardware, software, copied int) {
	return d.hwFrames, d.swFrames, d.mapFails
}

// Close frees the decoder.
func (d *Decoder) Close() {
	if d.closed {
		return
	}
	d.closed = true
	l := d.l
	if d.frame != nil {
		l.av_frame_free(&d.frame)
	}
	if d.sw != nil {
		l.av_frame_free(&d.sw)
	}
	if d.dst != nil {
		l.av_frame_free(&d.dst)
	}
	if d.pkt != nil {
		l.av_packet_free(&d.pkt)
	}
	if d.sws != nil {
		l.sws_free_context(&d.sws)
	}
	if d.ctx != nil {
		l.avcodec_free_context(&d.ctx) // unrefs hw_device_ctx
	}
	if d.hwDevice != nil {
		l.av_buffer_unref(&d.hwDevice)
	}
}
