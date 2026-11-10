package apppkg

import (
	"encoding/binary"
	"errors"
)

// resources.arsc, read only as far as finding a string resource: an app's
// label is usually @string/app_name.

// Type chunk flags and entry flags.
const (
	typeFlagSparse   = 0x01
	typeFlagOffset16 = 0x02
	entryComplex     = 0x0001
	entryCompact     = 0x0008
)

// table is a resources.arsc read for string lookups.
type table struct {
	strings  *stringPool
	packages map[uint8][]chunk // a package's type chunks, by package id
}

func readTable(b []byte) (*table, error) {
	top, err := chunks(b)
	if err != nil && len(top) == 0 {
		return nil, err
	}
	if len(top) == 0 || top[0].typ != chunkTable {
		return nil, errors.New("not a resource table")
	}
	body, _ := chunks(top[0].b[top[0].header:])
	t := &table{packages: map[uint8][]chunk{}}
	for _, c := range body {
		switch c.typ {
		case chunkStringPool:
			if t.strings == nil {
				if t.strings, err = readStringPool(c); err != nil {
					return nil, err
				}
			}
		case chunkTablePackage:
			if len(c.b) < 12 {
				continue
			}
			id := uint8(binary.LittleEndian.Uint32(c.b[8:]))
			inner, _ := chunks(c.b[c.header:])
			for _, x := range inner {
				if x.typ == chunkTableType {
					t.packages[id] = append(t.packages[id], x)
				}
			}
		}
	}
	return t, nil
}

// lookupString resolves a string resource, following references, in the
// default configuration when it has one.
func (t *table) lookupString(id uint32) string {
	for range 8 {
		typ, data, ok := t.value(id)
		switch {
		case !ok:
			return ""
		case typ == typeString:
			return t.strings.get(data)
		case typ == typeReference && data != 0:
			id = data
		default:
			return ""
		}
	}
	return ""
}

// value returns a resource's value in the best configuration: the
// default one, else English, else the first.
func (t *table) value(id uint32) (typ uint8, data uint32, ok bool) {
	pkg, typeID, entry := uint8(id>>24), uint8(id>>16), int(id&0xffff)
	best := -1
	for _, c := range t.packages[pkg] {
		b := c.b
		if len(b) < 20 || b[8] != typeID {
			continue
		}
		vt, vd, found := typeEntry(c, entry)
		if !found {
			continue
		}
		if s := configScore(b[20:c.header]); s > best {
			best, typ, data, ok = s, vt, vd, true
		}
	}
	return typ, data, ok
}

// configScore ranks a ResTable_config: 2 for the default configuration,
// 1 for English, 0 for any other.
func configScore(cfg []byte) int {
	if len(cfg) < 12 {
		return 0
	}
	if cfg[8] == 'e' && cfg[9] == 'n' {
		return 1
	}
	for _, x := range cfg[4:] {
		if x != 0 {
			return 0
		}
	}
	return 2
}

// typeEntry reads entry i of a type chunk, when it is a simple value.
func typeEntry(c chunk, i int) (typ uint8, data uint32, ok bool) {
	b := c.b
	flags := b[9]
	count := int(binary.LittleEndian.Uint32(b[12:]))
	start := int(binary.LittleEndian.Uint32(b[16:]))
	off := -1
	switch {
	case flags&typeFlagSparse != 0:
		for k := 0; k < count; k++ {
			at := c.header + 4*k
			if at+4 > len(b) {
				return 0, 0, false
			}
			if int(binary.LittleEndian.Uint16(b[at:])) == i {
				off = 4 * int(binary.LittleEndian.Uint16(b[at+2:]))
				break
			}
		}
	case i >= count:
		return 0, 0, false
	case flags&typeFlagOffset16 != 0:
		at := c.header + 2*i
		if at+2 > len(b) {
			return 0, 0, false
		}
		if v := binary.LittleEndian.Uint16(b[at:]); v != 0xffff {
			off = 4 * int(v)
		}
	default:
		at := c.header + 4*i
		if at+4 > len(b) {
			return 0, 0, false
		}
		if v := binary.LittleEndian.Uint32(b[at:]); v != 0xffffffff {
			off = int(v)
		}
	}
	if off < 0 {
		return 0, 0, false
	}
	at := start + off
	if at < 0 || at+8 > len(b) {
		return 0, 0, false
	}
	e := b[at:]
	eflags := binary.LittleEndian.Uint16(e[2:])
	switch {
	case eflags&entryCompact != 0:
		return uint8(eflags >> 8), binary.LittleEndian.Uint32(e[4:]), true
	case eflags&entryComplex != 0:
		return 0, 0, false
	}
	size := int(binary.LittleEndian.Uint16(e))
	if size+8 > len(e) {
		return 0, 0, false
	}
	v := e[size:]
	return v[3], binary.LittleEndian.Uint32(v[4:]), true
}
