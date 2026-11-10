package ios

import (
	"bytes"
	"encoding/binary"
	"errors"
)

// rtpPacket is one parsed RTP packet. Payload aliases the input buffer.
type rtpPacket struct {
	Marker  bool
	PT      uint8
	Seq     uint16
	TS      uint32
	SSRC    uint32
	Payload []byte
	Ext     []byte // the header extension: its profile, length and data
}

var errShortRTP = errors.New("short RTP packet")

// parseRTP parses an RTP header (RFC 3550): CSRCs and padding are
// skipped, a header extension is kept as it is.
func parseRTP(b []byte) (rtpPacket, error) {
	if len(b) < 12 || b[0]>>6 != 2 {
		return rtpPacket{}, errShortRTP
	}
	p := rtpPacket{
		Marker: b[1]&0x80 != 0,
		PT:     b[1] & 0x7f,
		Seq:    binary.BigEndian.Uint16(b[2:4]),
		TS:     binary.BigEndian.Uint32(b[4:8]),
		SSRC:   binary.BigEndian.Uint32(b[8:12]),
	}
	n := 12 + 4*int(b[0]&0x0f)
	if len(b) < n {
		return rtpPacket{}, errShortRTP
	}
	if b[0]&0x10 != 0 {
		if len(b) < n+4 {
			return rtpPacket{}, errShortRTP
		}
		start := n
		n += 4 + 4*int(binary.BigEndian.Uint16(b[n+2:n+4]))
		if len(b) < n {
			return rtpPacket{}, errShortRTP
		}
		p.Ext = b[start:n]
	}
	end := len(b)
	if b[0]&0x20 != 0 {
		pad := int(b[end-1])
		if pad == 0 || end-pad < n {
			return rtpPacket{}, errShortRTP
		}
		end -= pad
	}
	p.Payload = b[n:end]
	return p, nil
}

// isRTCP reports whether a packet on a multiplexed port is RTCP (RFC 5761):
// its second byte is a payload type from 192 to 223.
func isRTCP(b []byte) bool { return len(b) >= 2 && b[1] >= 192 && b[1] <= 223 }

// HEVC NAL unit types (H.265 table 7-1).
const (
	hevcBLAWLP    = 16
	hevcCRANUT    = 21
	hevcVPS       = 32
	hevcSPS       = 33
	hevcPPS       = 34
	hevcAUD       = 35
	hevcAP        = 48 // RFC 7798 aggregation packet
	hevcFU        = 49 // RFC 7798 fragmentation unit
	hevcPACI      = 50 // RFC 7798 payload content information
	hevcMaxNALLen = 4 << 20
)

// H.264 NAL unit types (H.264 table 7-1 and RFC 6184).
const (
	h264IDR   = 5
	h264SPS   = 7
	h264PPS   = 8
	h264STAPA = 24
	h264FUA   = 28
)

// displayTrailer is a constant 14-byte footer the display service appends to
// the last fragment of each coded slice. Apple's own receiver strips it;
// decoders ignore it, but it is not part of the bitstream.
var displayTrailer = []byte{0x04, 0xf0, 0x0a, 0xc0, 0x00, 0x00, 0x03, 0x00, 0x00, 0x04, 0xec, 0x0a, 0xb0, 0x03}

func hevcType(nal []byte) int { return int(nal[0]>>1) & 0x3f }

// depacketizer turns RTP payloads into complete NAL units: RFC 7798 for HEVC
// and RFC 6184 for H.264. Which one applies is decided from the first payload,
// since the payload type number says nothing reliable about the codec.
type depacketizer struct {
	codec  string // "hevc", "h264", or "" until known
	fu     []byte
	inFU   bool
	broken bool // a fragment went missing; drop until the next start
}

// reset drops a partial fragment, after packet loss.
func (d *depacketizer) reset() {
	d.fu = d.fu[:0]
	d.inFU = false
	d.broken = true
}

// sniff guesses the codec from a payload's NAL header.
func sniff(p []byte) string {
	if len(p) < 2 || p[0]&0x80 != 0 {
		return ""
	}
	// An HEVC NAL header is two bytes with a layer id of 0 and a temporal id
	// plus one of at least 1, so its second byte is 1..7. H.264 has a one-byte
	// header and the second byte is payload.
	t := hevcType(p)
	if p[1] >= 1 && p[1] <= 7 && p[0]&0x01 == 0 {
		switch {
		case t <= hevcCRANUT, t >= hevcVPS && t <= 40, t == hevcAP, t == hevcFU:
			return "hevc"
		}
	}
	switch p[0] & 0x1f {
	case 1, h264IDR, 6, h264SPS, h264PPS, 9, h264STAPA, h264FUA:
		return "h264"
	}
	return ""
}

// push adds one RTP payload and appends the NAL units it completes to out.
func (d *depacketizer) push(p []byte, out [][]byte) [][]byte {
	if len(p) == 0 {
		return out
	}
	if d.codec == "" {
		d.codec = sniff(p)
		if d.codec == "" {
			return out
		}
	}
	if d.codec == "hevc" {
		return d.pushHEVC(p, out)
	}
	return d.pushH264(p, out)
}

func (d *depacketizer) pushHEVC(p []byte, out [][]byte) [][]byte {
	if len(p) < 3 {
		return out
	}
	switch hevcType(p) {
	case hevcAP:
		// Two-byte payload header, then 16-bit size + NAL unit, repeated.
		// The sprop-max-don-diff DONL fields are absent: the device never
		// signals interleaving.
		for i := 2; i+2 <= len(p); {
			n := int(binary.BigEndian.Uint16(p[i:]))
			i += 2
			if n == 0 || i+n > len(p) {
				break
			}
			out = append(out, clone(p[i:i+n]))
			i += n
		}
	case hevcFU:
		h := p[2]
		start, end := h&0x80 != 0, h&0x40 != 0
		if start {
			typ := h & 0x3f
			d.fu = append(d.fu[:0], (p[0]&0x81)|typ<<1, p[1])
			d.inFU = true
			d.broken = false
		} else if !d.inFU || d.broken {
			return out
		}
		d.fu = append(d.fu, p[3:]...)
		if len(d.fu) > hevcMaxNALLen {
			d.reset()
			return out
		}
		if end {
			out = append(out, stripTrailer(clone(d.fu)))
			d.fu = d.fu[:0]
			d.inFU = false
		}
	case hevcPACI:
		// Payload content information wraps another payload; not used by the
		// device. Skip it rather than feed it to a decoder.
	default:
		out = append(out, stripTrailer(clone(p)))
	}
	return out
}

func (d *depacketizer) pushH264(p []byte, out [][]byte) [][]byte {
	switch p[0] & 0x1f {
	case h264STAPA:
		for i := 1; i+2 <= len(p); {
			n := int(binary.BigEndian.Uint16(p[i:]))
			i += 2
			if n == 0 || i+n > len(p) {
				break
			}
			out = append(out, clone(p[i:i+n]))
			i += n
		}
	case h264FUA:
		if len(p) < 2 {
			return out
		}
		h := p[1]
		start, end := h&0x80 != 0, h&0x40 != 0
		if start {
			d.fu = append(d.fu[:0], (p[0]&0xe0)|(h&0x1f))
			d.inFU = true
			d.broken = false
		} else if !d.inFU || d.broken {
			return out
		}
		d.fu = append(d.fu, p[2:]...)
		if len(d.fu) > hevcMaxNALLen {
			d.reset()
			return out
		}
		if end {
			out = append(out, stripTrailer(clone(d.fu)))
			d.fu = d.fu[:0]
			d.inFU = false
		}
	default:
		out = append(out, stripTrailer(clone(p)))
	}
	return out
}

func stripTrailer(nal []byte) []byte {
	if len(nal) > len(displayTrailer) && bytes.HasSuffix(nal, displayTrailer) {
		return nal[:len(nal)-len(displayTrailer)]
	}
	return nal
}

func clone(b []byte) []byte { return append([]byte(nil), b...) }

// nalInfo classifies a NAL unit of the given codec.
func nalInfo(codec string, nal []byte) (config, key bool) {
	if len(nal) == 0 {
		return false, false
	}
	if codec == "hevc" {
		t := hevcType(nal)
		return t >= hevcVPS && t <= hevcPPS, t >= hevcBLAWLP && t <= hevcCRANUT
	}
	t := int(nal[0] & 0x1f)
	return t == h264SPS || t == h264PPS, t == h264IDR
}

// annexB joins NAL units with four-byte start codes.
func annexB(nals [][]byte) []byte {
	n := 0
	for _, x := range nals {
		n += 4 + len(x)
	}
	out := make([]byte, 0, n)
	for _, x := range nals {
		out = append(out, 0, 0, 0, 1)
		out = append(out, x...)
	}
	return out
}
