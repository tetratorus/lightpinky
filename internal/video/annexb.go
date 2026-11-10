package video

import (
	"bytes"

	"github.com/tetratorus/lightpinky/internal/driver"
)

// NAL unit types the stream code looks at.
const (
	h264Slice = 1
	h264IDR   = 5
	h264SPS   = 7
	h264PPS   = 8
	h264AUD   = 9
	hevcIRAP0 = 16 // BLA_W_LP; IRAP pictures are 16 to 23
	hevcIRAP1 = 23
	hevcVPS   = 32
	hevcSPS   = 33
	hevcPPS   = 34
	hevcAUD   = 35
)

// NALType returns the type of the NAL unit whose header starts at b[0].
func NALType(codec driver.Codec, b []byte) int {
	if len(b) == 0 {
		return -1
	}
	if codec == driver.HEVC {
		return int(b[0]>>1) & 0x3f
	}
	return int(b[0]) & 0x1f
}

// isConfig reports whether a NAL unit is a parameter set.
func isConfig(codec driver.Codec, t int) bool {
	if codec == driver.HEVC {
		return t == hevcVPS || t == hevcSPS || t == hevcPPS
	}
	return t == h264SPS || t == h264PPS
}

// isKey reports whether a NAL unit starts a picture decodable on its own.
func isKey(codec driver.Codec, t int) bool {
	if codec == driver.HEVC {
		return t >= hevcIRAP0 && t <= hevcIRAP1
	}
	return t == h264IDR
}

func isAUD(codec driver.Codec, t int) bool {
	if codec == driver.HEVC {
		return t == hevcAUD
	}
	return t == h264AUD
}

// SplitNALs returns the NAL units of an Annex B buffer, each without its
// start code.
func SplitNALs(b []byte) [][]byte {
	var out [][]byte
	start := -1
	for i := 0; i+3 <= len(b); {
		if b[i] == 0 && b[i+1] == 0 && b[i+2] == 1 {
			if start >= 0 {
				end := i
				if end > start && b[end-1] == 0 {
					end-- // the zero byte of a four-byte start code
				}
				out = append(out, b[start:end])
			}
			i += 3
			start = i
			continue
		}
		i++
	}
	if start >= 0 && start < len(b) {
		out = append(out, b[start:])
	}
	return out
}

var startCode = []byte{0, 0, 0, 1}

// joinNALs writes NAL units back into Annex B with four-byte start codes.
func joinNALs(nals [][]byte) []byte {
	n := 0
	for _, u := range nals {
		n += len(startCode) + len(u)
	}
	out := make([]byte, 0, n)
	for _, u := range nals {
		out = append(out, startCode...)
		out = append(out, u...)
	}
	return out
}

// auSplitter cuts an Annex B byte stream into access units at access unit
// delimiters, which the test stream's encoder inserts.
type auSplitter struct {
	codec driver.Codec
	buf   []byte
}

// push adds bytes and returns the access units completed by them.
func (s *auSplitter) push(data []byte) [][]byte {
	s.buf = append(s.buf, data...)
	var out [][]byte
	// The unit at the front starts with its own delimiter, so the search
	// starts past its first byte; a delimiter found further on ends it.
	pos := 1
	for pos < len(s.buf) {
		i := bytes.Index(s.buf[pos:], []byte{0, 0, 1})
		if i < 0 {
			break
		}
		i += pos
		if i+3 >= len(s.buf) {
			break // the header byte has not arrived yet
		}
		if isAUD(s.codec, NALType(s.codec, s.buf[i+3:])) {
			cut := i
			if s.buf[cut-1] == 0 {
				cut-- // the zero byte of a four-byte start code
			}
			if cut > 0 {
				out = append(out, append([]byte(nil), s.buf[:cut]...))
				s.buf = append(s.buf[:0], s.buf[cut:]...)
				pos = 1
				continue
			}
		}
		pos = i + 3
	}
	return out
}

// packetizer turns access units into driver packets the way scrcpy sends
// them: parameter sets in a configuration packet of their own whenever they
// change, then each picture without parameter sets or delimiters.
type packetizer struct {
	codec  driver.Codec
	config []byte
}

func (p *packetizer) packets(au []byte) []driver.Packet {
	var cfg, pic [][]byte
	key := false
	for _, u := range SplitNALs(au) {
		t := NALType(p.codec, u)
		switch {
		case isAUD(p.codec, t):
		case isConfig(p.codec, t):
			cfg = append(cfg, u)
		default:
			key = key || isKey(p.codec, t)
			pic = append(pic, u)
		}
	}
	var out []driver.Packet
	if len(cfg) > 0 {
		if c := joinNALs(cfg); !bytes.Equal(c, p.config) {
			p.config = c
			out = append(out, driver.Packet{Data: c, Config: true})
		}
	}
	if len(pic) > 0 {
		out = append(out, driver.Packet{Data: joinNALs(pic), KeyFrame: key})
	}
	return out
}
