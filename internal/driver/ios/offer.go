package ios

// The media stream offer. Its layout follows go-ios's ios/display/offer.go
// (MIT License, Copyright (c) 2019 danielpaulus), which pins it against a
// captured Xcode offer; the device rejects any deviation with an opaque
// "Invalid Parameter". Two values differ from go-ios on purpose, see
// offerOptions.

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"howett.net/plist"
)

const (
	negotiatorModeVideo = 5
	decoderName         = "Viceroy 1.7.0"
	codecCapID          = 50115

	// The two codec banks the offer advertises, by RTP payload type. The
	// device picks one and streams with its payload type; on iOS 27 it picks
	// 100, and the stream in it is HEVC (see rtp.go, which checks the NAL
	// header rather than trusting the number).
	payloadType123 = 123
	payloadType100 = 100

	// capturedTimestamp is the host clock value in the captured offer; the
	// device does not check it.
	capturedTimestamp = uint64(17137042128614416384)
)

// offerOptions are the knobs of the offer that matter.
type offerOptions struct {
	// AllowRTCPFB lets the device act on RTCP feedback such as FIR keyframe
	// requests.
	AllowRTCPFB bool
	// Features is the feature list of the payload type 100 bank. Xcode sends
	// "FLS;VRAE:0;SW:1;", but VRAE:0 forbids resolution adaptation, so under
	// its bitrate cap the encoder drops frames under motion and the picture
	// smears (measured by pymobiledevice3 on iOS 27). "FLS;SW:1;" keeps every
	// frame at a slightly higher QP instead.
	Features string
	// LTRP turns on long-term reference frames. Off by default: under loss
	// they leave the picture torn. Xcode's captured offer has them on.
	LTRP bool
	// NoFEC turns off forward error correction, which is on by default.
	NoFEC bool
}

func defaultOfferOptions() offerOptions {
	return offerOptions{Features: "FLS;SW:1;"}
}

type pbuf struct{ b []byte }

func (p *pbuf) tag(field, wire uint64) { p.b = binary.AppendUvarint(p.b, field<<3|wire) }

func (p *pbuf) varint(field, v uint64) {
	p.tag(field, 0)
	p.b = binary.AppendUvarint(p.b, v)
}

func (p *pbuf) boolean(field uint64, v bool) {
	if v {
		p.varint(field, 1)
	} else {
		p.varint(field, 0)
	}
}

func (p *pbuf) bytes(field uint64, v []byte) {
	p.tag(field, 2)
	p.b = binary.AppendUvarint(p.b, uint64(len(v)))
	p.b = append(p.b, v...)
}

func (p *pbuf) str(field uint64, v string) { p.bytes(field, []byte(v)) }

// varintPadded writes v as a varint of exactly width bytes, padded with
// continuation bytes, as the captured offer does for the session id.
func (p *pbuf) varintPadded(field, v uint64, width int) {
	p.tag(field, 0)
	enc := binary.AppendUvarint(nil, v)
	for len(enc) < width {
		enc[len(enc)-1] |= 0x80
		enc = append(enc, 0)
	}
	p.b = append(p.b, enc...)
}

func codecBank(payloadType uint64, features string, trailer uint64, pairs int) []byte {
	var b pbuf
	b.varint(1, payloadType)
	for i := 0; i < pairs; i++ {
		var e pbuf
		e.varint(1, 1)
		e.varint(2, uint64(1+i%2))
		e.varint(3, codecCapID)
		e.varint(4, 0)
		b.bytes(2, e.b)
	}
	b.str(3, features)
	b.varint(4, trailer)
	return b.b
}

type bitrateTier struct {
	kind, bps, buf uint64
	hasBuf         bool
}

var videoTiers = []bitrateTier{
	{4074, 0, 16384, true},
	{0, 75_000_000, 524288, true},
	{0, 40_000_000, 12288, true},
	{16, 4100, 0, false},
	{0, 20_000_000, 98304, true},
	{4, 6500, 0, false},
	{0, 6_000_000, 131072, true},
	{0, 100_000_000, 1048576, true},
	{0, 60_000_000, 262144, true},
	{1, 299, 0, false},
}

func mediaBlob(sessionID uint32, o offerOptions) []byte {
	var vs pbuf
	vs.varintPadded(1, uint64(sessionID), 5)
	vs.boolean(2, o.AllowRTCPFB)
	vs.bytes(3, codecBank(payloadType123, "FLS;SW:1;", 1, 4))
	vs.bytes(3, codecBank(payloadType100, o.Features, 14, 2))
	vs.boolean(7, o.LTRP)
	vs.varint(8, 63)
	if !o.NoFEC {
		vs.varint(10, 1)
	}
	vs.varint(12, 1)

	var b pbuf
	b.varint(1, 1)
	b.varint(2, 1)
	b.bytes(5, vs.b)
	b.str(6, decoderName)
	b.varint(8, 0)
	for _, t := range videoTiers {
		var e pbuf
		e.varint(1, t.kind)
		e.varint(2, t.bps)
		if t.hasBuf {
			e.varint(3, t.buf)
		}
		b.bytes(9, e.b)
	}
	b.varint(13, capturedTimestamp)
	b.varint(14, 2)
	b.varint(16, 0)
	b.varint(18, 1)
	return b.b
}

// endpointInfo describes the host. These are Xcode's values; the host is often
// not a Mac, and the device may pick encoder parameters from them.
func endpointInfo() []byte {
	var b pbuf
	b.varint(1, 0)
	b.varint(2, 1)
	b.str(3, "Mac15,9")
	b.str(4, "2205.3.1")
	b.str(5, "25F80")
	return b.b
}

// buildOffer encodes the negotiator offer plist.
func buildOffer(callID uuid.UUID, sessionID uint32, o offerOptions) ([]byte, error) {
	var z bytes.Buffer
	w, err := zlib.NewWriterLevel(&z, zlib.BestCompression) // the device rejects other levels
	if err != nil {
		return nil, err
	}
	w.Write(mediaBlob(sessionID, o))
	if err := w.Close(); err != nil {
		return nil, err
	}
	offer := struct {
		RemoteEndpointInfo []byte `plist:"avcMediaStreamOptionRemoteEndpointInfo"`
		NegotiatorMode     int    `plist:"avcMediaStreamNegotiatorMode"`
		MediaBlob          []byte `plist:"avcMediaStreamNegotiatorMediaBlob"`
		CallID             string `plist:"avcMediaStreamOptionCallID"`
	}{endpointInfo(), negotiatorModeVideo, z.Bytes(), strings.ToUpper(callID.String())}
	data, err := plist.Marshal(offer, plist.BinaryFormat)
	if err != nil {
		return nil, fmt.Errorf("encode offer: %w", err)
	}
	return data, nil
}
