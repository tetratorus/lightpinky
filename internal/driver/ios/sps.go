package ios

import "errors"

// bitReader reads an RBSP (emulation prevention bytes already removed).
type bitReader struct {
	b   []byte
	pos int // in bits
}

var errSPS = errors.New("truncated SPS")

func (r *bitReader) u(n int) (uint32, error) {
	var v uint32
	for i := 0; i < n; i++ {
		if r.pos >= len(r.b)*8 {
			return 0, errSPS
		}
		bit := (r.b[r.pos/8] >> (7 - r.pos%8)) & 1
		v = v<<1 | uint32(bit)
		r.pos++
	}
	return v, nil
}

func (r *bitReader) skip(n int) error {
	if r.pos+n > len(r.b)*8 {
		return errSPS
	}
	r.pos += n
	return nil
}

// ue reads an unsigned Exp-Golomb code.
func (r *bitReader) ue() (uint32, error) {
	zeros := 0
	for {
		b, err := r.u(1)
		if err != nil {
			return 0, err
		}
		if b == 1 {
			break
		}
		zeros++
		if zeros > 31 {
			return 0, errSPS
		}
	}
	rest, err := r.u(zeros)
	if err != nil {
		return 0, err
	}
	return (1<<zeros - 1) + rest, nil
}

func (r *bitReader) se() (int32, error) {
	v, err := r.ue()
	if err != nil {
		return 0, err
	}
	if v&1 == 1 {
		return int32(v+1) / 2, nil
	}
	return -int32(v / 2), nil
}

// rbsp removes emulation prevention bytes (00 00 03 -> 00 00).
func rbsp(nal []byte) []byte {
	out := make([]byte, 0, len(nal))
	zeros := 0
	for _, b := range nal {
		if zeros >= 2 && b == 3 {
			zeros = 0
			continue
		}
		if b == 0 {
			zeros++
		} else {
			zeros = 0
		}
		out = append(out, b)
	}
	return out
}

// chromaSub returns SubWidthC and SubHeightC for a chroma_format_idc.
func chromaSub(chroma uint32) (int, int) {
	switch chroma {
	case 1:
		return 2, 2
	case 2:
		return 2, 1
	default:
		return 1, 1
	}
}

// hevcSPSSize returns the displayed width and height an HEVC SPS declares,
// after the conformance window.
func hevcSPSSize(nal []byte) (int, int, error) {
	if len(nal) < 3 {
		return 0, 0, errSPS
	}
	r := &bitReader{b: rbsp(nal[2:])}
	if err := r.skip(4); err != nil { // sps_video_parameter_set_id
		return 0, 0, err
	}
	maxSub, err := r.u(3)
	if err != nil {
		return 0, 0, err
	}
	if err := r.skip(1); err != nil { // temporal_id_nesting
		return 0, 0, err
	}
	// profile_tier_level: 2+1+5 + 32 + 4 + 43 + 1 bits, then level_idc.
	if err := r.skip(88 + 8); err != nil {
		return 0, 0, err
	}
	profilePresent := make([]bool, maxSub)
	levelPresent := make([]bool, maxSub)
	for i := 0; i < int(maxSub); i++ {
		p, err := r.u(1)
		if err != nil {
			return 0, 0, err
		}
		l, err := r.u(1)
		if err != nil {
			return 0, 0, err
		}
		profilePresent[i], levelPresent[i] = p == 1, l == 1
	}
	if maxSub > 0 {
		if err := r.skip(2 * (8 - int(maxSub))); err != nil {
			return 0, 0, err
		}
	}
	for i := 0; i < int(maxSub); i++ {
		if profilePresent[i] {
			if err := r.skip(88); err != nil {
				return 0, 0, err
			}
		}
		if levelPresent[i] {
			if err := r.skip(8); err != nil {
				return 0, 0, err
			}
		}
	}
	if _, err := r.ue(); err != nil { // sps_seq_parameter_set_id
		return 0, 0, err
	}
	chroma, err := r.ue()
	if err != nil {
		return 0, 0, err
	}
	if chroma == 3 {
		if err := r.skip(1); err != nil {
			return 0, 0, err
		}
	}
	w, err := r.ue()
	if err != nil {
		return 0, 0, err
	}
	h, err := r.ue()
	if err != nil {
		return 0, 0, err
	}
	width, height := int(w), int(h)
	conf, err := r.u(1)
	if err != nil {
		return 0, 0, err
	}
	if conf == 1 {
		var off [4]uint32
		for i := range off {
			if off[i], err = r.ue(); err != nil {
				return 0, 0, err
			}
		}
		sw, sh := chromaSub(chroma)
		width -= sw * int(off[0]+off[1])
		height -= sh * int(off[2]+off[3])
	}
	if width <= 0 || height <= 0 || width > 16384 || height > 16384 {
		return 0, 0, errSPS
	}
	return width, height, nil
}

// h264SPSSize returns the displayed width and height an H.264 SPS declares,
// after frame cropping.
func h264SPSSize(nal []byte) (int, int, error) {
	if len(nal) < 4 {
		return 0, 0, errSPS
	}
	r := &bitReader{b: rbsp(nal[1:])}
	profile, err := r.u(8)
	if err != nil {
		return 0, 0, err
	}
	if err := r.skip(16); err != nil { // constraint flags, level_idc
		return 0, 0, err
	}
	if _, err := r.ue(); err != nil { // seq_parameter_set_id
		return 0, 0, err
	}
	chroma := uint32(1)
	switch profile {
	case 100, 110, 122, 244, 44, 83, 86, 118, 128, 138, 139, 134, 135:
		if chroma, err = r.ue(); err != nil {
			return 0, 0, err
		}
		if chroma == 3 {
			if err := r.skip(1); err != nil {
				return 0, 0, err
			}
		}
		for i := 0; i < 2; i++ { // bit depths
			if _, err := r.ue(); err != nil {
				return 0, 0, err
			}
		}
		if err := r.skip(1); err != nil { // qpprime_y_zero_transform_bypass
			return 0, 0, err
		}
		scaling, err := r.u(1)
		if err != nil {
			return 0, 0, err
		}
		if scaling == 1 {
			lists := 8
			if chroma == 3 {
				lists = 12
			}
			for i := 0; i < lists; i++ {
				present, err := r.u(1)
				if err != nil {
					return 0, 0, err
				}
				if present == 0 {
					continue
				}
				size := 16
				if i >= 6 {
					size = 64
				}
				last, next := int32(8), int32(8)
				for j := 0; j < size; j++ {
					if next != 0 {
						d, err := r.se()
						if err != nil {
							return 0, 0, err
						}
						next = (last + d + 256) % 256
					}
					if next != 0 {
						last = next
					}
				}
			}
		}
	}
	if _, err := r.ue(); err != nil { // log2_max_frame_num_minus4
		return 0, 0, err
	}
	poc, err := r.ue()
	if err != nil {
		return 0, 0, err
	}
	switch poc {
	case 0:
		if _, err := r.ue(); err != nil {
			return 0, 0, err
		}
	case 1:
		if err := r.skip(1); err != nil {
			return 0, 0, err
		}
		for i := 0; i < 2; i++ {
			if _, err := r.se(); err != nil {
				return 0, 0, err
			}
		}
		n, err := r.ue()
		if err != nil {
			return 0, 0, err
		}
		for i := 0; i < int(n); i++ {
			if _, err := r.se(); err != nil {
				return 0, 0, err
			}
		}
	}
	if _, err := r.ue(); err != nil { // max_num_ref_frames
		return 0, 0, err
	}
	if err := r.skip(1); err != nil { // gaps_in_frame_num_value_allowed
		return 0, 0, err
	}
	wmb, err := r.ue()
	if err != nil {
		return 0, 0, err
	}
	hmu, err := r.ue()
	if err != nil {
		return 0, 0, err
	}
	frameOnly, err := r.u(1)
	if err != nil {
		return 0, 0, err
	}
	if frameOnly == 0 {
		if err := r.skip(1); err != nil {
			return 0, 0, err
		}
	}
	if err := r.skip(1); err != nil { // direct_8x8_inference
		return 0, 0, err
	}
	width := int(wmb+1) * 16
	height := int(hmu+1) * 16 * int(2-frameOnly)
	crop, err := r.u(1)
	if err != nil {
		return 0, 0, err
	}
	if crop == 1 {
		var off [4]uint32
		for i := range off {
			if off[i], err = r.ue(); err != nil {
				return 0, 0, err
			}
		}
		cx, cy := 1, 2-int(frameOnly)
		if chroma != 0 {
			sw, sh := chromaSub(chroma)
			cx, cy = sw, sh*(2-int(frameOnly))
		}
		width -= cx * int(off[0]+off[1])
		height -= cy * int(off[2]+off[3])
	}
	if width <= 0 || height <= 0 || width > 16384 || height > 16384 {
		return 0, 0, errSPS
	}
	return width, height, nil
}
