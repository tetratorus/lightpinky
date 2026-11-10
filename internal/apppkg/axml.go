package apppkg

import (
	"encoding/binary"
	"errors"
	"fmt"
	"unicode/utf16"
)

// Android's binary XML (AndroidManifest.xml inside an APK) and the string
// pools it shares with resources.arsc: frameworks/base/libs/androidfw,
// ResourceTypes.h.

// Chunk types.
const (
	chunkStringPool   = 0x0001
	chunkTable        = 0x0002
	chunkXML          = 0x0003
	chunkXMLStartElem = 0x0102
	chunkXMLResMap    = 0x0180
	chunkTablePackage = 0x0200
	chunkTableType    = 0x0201
)

// Res_value data types.
const (
	typeReference = 0x01
	typeString    = 0x03
	typeIntDec    = 0x10
	typeIntHex    = 0x11
)

// Attribute resource ids, for manifests whose attribute names were
// stripped.
const (
	attrLabel            = 0x01010001
	attrVersionCode      = 0x0101021b
	attrVersionName      = 0x0101021c
	attrVersionCodeMajor = 0x01010576
)

const androidNS = "http://schemas.android.com/apk/res/android"

var errShort = errors.New("truncated")

// chunk is one chunk: its type, header size and whole bytes.
type chunk struct {
	typ    uint16
	header int
	b      []byte
}

// chunks splits b into the chunks it holds, one after another.
func chunks(b []byte) ([]chunk, error) {
	var out []chunk
	for len(b) > 0 {
		if len(b) < 8 {
			return out, errShort
		}
		typ := binary.LittleEndian.Uint16(b)
		hdr := int(binary.LittleEndian.Uint16(b[2:]))
		size := int(binary.LittleEndian.Uint32(b[4:]))
		if size < 8 || hdr < 8 || hdr > size || size > len(b) {
			return out, fmt.Errorf("chunk 0x%04x of %d bytes: %w", typ, size, errShort)
		}
		out = append(out, chunk{typ: typ, header: hdr, b: b[:size]})
		b = b[size:]
	}
	return out, nil
}

// stringPool is a ResStringPool.
type stringPool struct {
	b       []byte
	offsets []uint32
	start   int
	utf8    bool
}

func readStringPool(c chunk) (*stringPool, error) {
	b := c.b
	if c.header < 28 || len(b) < 28 {
		return nil, errShort
	}
	n := int(binary.LittleEndian.Uint32(b[8:]))
	flags := binary.LittleEndian.Uint32(b[16:])
	start := int(binary.LittleEndian.Uint32(b[20:]))
	if n < 0 || c.header+4*n > len(b) || start > len(b) {
		return nil, errShort
	}
	p := &stringPool{b: b, start: start, utf8: flags&0x100 != 0, offsets: make([]uint32, n)}
	for i := range p.offsets {
		p.offsets[i] = binary.LittleEndian.Uint32(b[c.header+4*i:])
	}
	return p, nil
}

// get returns string i, or "" for one that is not there.
func (p *stringPool) get(i uint32) string {
	if p == nil || int64(i) >= int64(len(p.offsets)) {
		return ""
	}
	at := p.start + int(p.offsets[i])
	if at < 0 || at >= len(p.b) {
		return ""
	}
	b := p.b[at:]
	if p.utf8 {
		_, n := utf8Len(b) // UTF-16 length, unused
		if n == 0 {
			return ""
		}
		b = b[n:]
		size, n := utf8Len(b)
		if n == 0 || n+size > len(b) {
			return ""
		}
		return string(b[n : n+size])
	}
	if len(b) < 2 {
		return ""
	}
	size := int(binary.LittleEndian.Uint16(b))
	b = b[2:]
	if size&0x8000 != 0 {
		if len(b) < 2 {
			return ""
		}
		size = (size&0x7fff)<<16 | int(binary.LittleEndian.Uint16(b))
		b = b[2:]
	}
	if 2*size > len(b) {
		return ""
	}
	u := make([]uint16, size)
	for i := range u {
		u[i] = binary.LittleEndian.Uint16(b[2*i:])
	}
	return string(utf16.Decode(u))
}

// utf8Len reads a UTF-8 pool length of one or two bytes.
func utf8Len(b []byte) (size, n int) {
	if len(b) < 1 {
		return 0, 0
	}
	if b[0]&0x80 == 0 {
		return int(b[0]), 1
	}
	if len(b) < 2 {
		return 0, 0
	}
	return int(b[0]&0x7f)<<8 | int(b[1]), 2
}

// xmlAttr is an attribute of an element in binary XML.
type xmlAttr struct {
	ns, name string
	resID    uint32 // the attribute's resource id, 0 when unknown
	raw      string // the raw string value, if any
	typ      uint8
	data     uint32
}

// xmlElem is a start element in binary XML.
type xmlElem struct {
	name  string
	depth int
	attrs []xmlAttr
}

// readXML returns the start elements of a binary XML document, in order.
func readXML(b []byte) ([]xmlElem, error) {
	top, err := chunks(b)
	if err != nil {
		return nil, err
	}
	if len(top) != 1 || top[0].typ != chunkXML {
		return nil, errors.New("not binary XML")
	}
	body, err := chunks(top[0].b[top[0].header:])
	if err != nil && len(body) == 0 {
		return nil, err
	}
	var pool *stringPool
	var resMap []uint32
	var out []xmlElem
	depth := 0
	for _, c := range body {
		switch c.typ {
		case chunkStringPool:
			if pool, err = readStringPool(c); err != nil {
				return nil, fmt.Errorf("string pool: %w", err)
			}
		case chunkXMLResMap:
			for i := c.header; i+4 <= len(c.b); i += 4 {
				resMap = append(resMap, binary.LittleEndian.Uint32(c.b[i:]))
			}
		case chunkXMLStartElem:
			e, err := readElem(c, pool, resMap)
			if err != nil {
				return nil, err
			}
			e.depth = depth
			depth++
			out = append(out, e)
		case chunkXMLStartElem + 1: // end element
			depth--
		}
	}
	return out, nil
}

func readElem(c chunk, pool *stringPool, resMap []uint32) (xmlElem, error) {
	b := c.b[c.header:]
	if len(b) < 20 {
		return xmlElem{}, errShort
	}
	e := xmlElem{name: pool.get(binary.LittleEndian.Uint32(b[4:]))}
	start := int(binary.LittleEndian.Uint16(b[8:]))
	size := int(binary.LittleEndian.Uint16(b[10:]))
	count := int(binary.LittleEndian.Uint16(b[12:]))
	if size < 20 {
		return e, errShort
	}
	for i := 0; i < count; i++ {
		at := start + i*size
		if at+20 > len(b) {
			return e, errShort
		}
		a := b[at:]
		name := binary.LittleEndian.Uint32(a[4:])
		x := xmlAttr{
			ns:   pool.get(binary.LittleEndian.Uint32(a)),
			name: pool.get(name),
			typ:  a[15],
			data: binary.LittleEndian.Uint32(a[16:]),
		}
		if raw := binary.LittleEndian.Uint32(a[8:]); raw != 0xffffffff {
			x.raw = pool.get(raw)
		} else if x.typ == typeString {
			x.raw = pool.get(x.data)
		}
		if int64(name) < int64(len(resMap)) {
			x.resID = resMap[name]
		}
		e.attrs = append(e.attrs, x)
	}
	return e, nil
}

// attr finds an attribute by its android: name or resource id, or by a
// plain name when id is 0.
func (e xmlElem) attr(name string, id uint32) (xmlAttr, bool) {
	for _, a := range e.attrs {
		if id != 0 && a.resID == id {
			return a, true
		}
	}
	for _, a := range e.attrs {
		if a.name == name && (id == 0) == (a.ns == "") {
			return a, true
		}
		if a.name == name && id != 0 && a.ns == androidNS {
			return a, true
		}
	}
	return xmlAttr{}, false
}

// text is an attribute's value as a string.
func (a xmlAttr) text() string {
	switch {
	case a.raw != "":
		return a.raw
	case a.typ == typeIntDec:
		return fmt.Sprint(int32(a.data))
	case a.typ == typeIntHex:
		return fmt.Sprintf("0x%x", a.data)
	}
	return ""
}
