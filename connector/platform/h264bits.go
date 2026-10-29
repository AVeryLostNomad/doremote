package platform

import (
	"bytes"
	"encoding/binary"
	"errors"
	"log"
	"math/bits"
)

// H.264 NAL unit types used here.
const (
	h264NALSlice = 1
	h264NALIDR   = 5
	h264NALSPS   = 7
	h264NALPPS   = 8
)

var h264StartCode = []byte{0, 0, 0, 1}

// h264NextNAL returns the first NAL unit (without start code or trailing zero bytes) of the
// Annex B stream b and the rest of the stream after it. nal is nil when b holds no more NALs.
func h264NextNAL(b []byte) (nal, rest []byte) {
	i := bytes.Index(b, h264StartCode[1:])
	if i < 0 {
		return nil, nil
	}
	b = b[i+3:]
	end := bytes.Index(b, h264StartCode[1:])
	if end < 0 {
		end = len(b)
	}
	nal, rest = b[:end], b[end:]
	for len(nal) > 0 && nal[len(nal)-1] == 0 {
		nal = nal[:len(nal)-1]
	}
	return nal, rest
}

// h264IsAnnexB reports whether b starts with an Annex B start code.
func h264IsAnnexB(b []byte) bool {
	return bytes.HasPrefix(b, h264StartCode[1:]) || bytes.HasPrefix(b, h264StartCode)
}

// h264PictureMask is the h264NALMask bits of coded-picture (VCL) NAL units.
const h264PictureMask = 1<<h264NALSlice | 1<<h264NALIDR

// h264NALMask returns the set (bit 1<<type) of NAL unit types in the Annex B stream b.
func h264NALMask(b []byte) uint32 {
	var m uint32
	for nal, rest := h264NextNAL(b); nal != nil; nal, rest = h264NextNAL(rest) {
		m |= 1 << (nal[0] & 0x1f)
	}
	return m
}

// h264ToAnnexB makes one encoder output sample Annex B in place. Samples whose 4-byte
// big-endian length prefixes (the MP4/AVCC layout) tile b exactly are rewritten to 4-byte
// start codes; tiling disambiguates them from Annex B, whose start code can read as a length.
func h264ToAnnexB(b []byte) error {
	i := 0
	for i+4 < len(b) {
		n := int(binary.BigEndian.Uint32(b[i:]))
		if n == 0 || n > len(b)-i-4 || b[i+4]&0x80 != 0 {
			break
		}
		i += 4 + n
	}
	if i == len(b) {
		for i = 0; i < len(b); {
			n := int(binary.BigEndian.Uint32(b[i:]))
			copy(b[i:], h264StartCode)
			i += 4 + n
		}
		return nil
	}
	if !h264IsAnnexB(b) {
		return errors.New("h264: encoder output is neither Annex B nor length-prefixed")
	}
	return nil
}

// h264AnnexBFromAVCC converts parameter sets to Annex B: an AVCDecoderConfigurationRecord
// (avcC) is unpacked, anything already in Annex B is returned unchanged.
func h264AnnexBFromAVCC(rec []byte) ([]byte, error) {
	if h264IsAnnexB(rec) {
		return rec, nil
	}
	if len(rec) < 7 || rec[0] != 1 {
		return nil, errors.New("h264: sequence header is neither Annex B nor avcC")
	}
	var out []byte
	p := rec[5:]
	for set := range 2 {
		if len(p) < 1 {
			return nil, errors.New("h264: truncated avcC")
		}
		n := int(p[0])
		if set == 0 {
			n &= 0x1f
		}
		p = p[1:]
		for range n {
			if len(p) < 2 || len(p) < 2+int(binary.BigEndian.Uint16(p)) {
				return nil, errors.New("h264: truncated avcC parameter set")
			}
			l := int(binary.BigEndian.Uint16(p))
			out = append(append(out, h264StartCode...), p[2:2+l]...)
			p = p[2+l:]
		}
	}
	return out, nil
}

// h264AccessUnit normalises encoder output into the access units Session.NextFrame returns.
type h264AccessUnit struct {
	buf       []byte
	spsIn     []byte // last SPS seen from the encoder and its colour-patched form
	spsOut    []byte
	spsWarned bool
}

// assemble appends the NAL units of the Annex B access unit au to a fresh access unit with
// 4-byte start codes. Every SPS gets BT.709 limited-range colour signalling in its VUI (some
// encoders ignore the media type's colour attributes), and an IDR without in-band SPS gets
// paramSets (Annex B) prepended. The result is valid until the next call; key reports an IDR,
// vcl whether au held a coded picture at all.
func (a *h264AccessUnit) assemble(au, paramSets []byte) (out []byte, key, vcl bool) {
	hasSPS := false
	for nal, rest := h264NextNAL(au); nal != nil; nal, rest = h264NextNAL(rest) {
		switch nal[0] & 0x1f {
		case h264NALIDR:
			key, vcl = true, true
		case h264NALSlice:
			vcl = true
		case h264NALSPS:
			hasSPS = true
		}
	}
	a.buf = a.buf[:0]
	if key && !hasSPS {
		a.appendNALs(paramSets)
	}
	a.appendNALs(au)
	return a.buf, key, vcl
}

func (a *h264AccessUnit) appendNALs(b []byte) {
	for nal, rest := h264NextNAL(b); nal != nil; nal, rest = h264NextNAL(rest) {
		if nal[0]&0x1f == h264NALSPS {
			nal = a.sps(nal)
		}
		a.buf = append(append(a.buf, h264StartCode...), nal...)
	}
}

func (a *h264AccessUnit) sps(nal []byte) []byte {
	if bytes.Equal(nal, a.spsIn) {
		return a.spsOut
	}
	out, _, err := h264SPSWithBT709VUI(nal)
	if err != nil {
		if !a.spsWarned {
			a.spsWarned = true
			log.Printf("h264: leaving encoder SPS colour signalling unchanged: %v", err)
		}
		out = nal
	}
	a.spsIn = append(a.spsIn[:0], nal...)
	a.spsOut = append(a.spsOut[:0], out...)
	return a.spsOut
}

// h264SPSWithBT709VUI returns the SPS NAL unit nal with VUI colour description BT.709
// primaries/transfer/matrix and video_full_range_flag=0, so browsers convert the limited-range
// BT.709 pictures correctly. changed is false (and nal returned) when it already says so. An
// SPS without VUI also gains bitstream_restriction with max_num_reorder_frames=0, telling
// decoders they may output every picture immediately.
func h264SPSWithBT709VUI(nal []byte) (out []byte, changed bool, err error) {
	if len(nal) < 4 || nal[0]&0x1f != h264NALSPS {
		return nil, false, errors.New("h264: not an SPS NAL unit")
	}
	rbsp := h264Unescape(nal[1:])
	r, maxRefFrames := h264ReadSPSHead(rbsp)
	vuiPos := r.pos
	hasVUI := r.u(1) == 1

	var w h264BitWriter
	w.copyBits(rbsp, 0, vuiPos)
	w.put(1, 1) // vui_parameters_present_flag
	videoFormat := uint32(5)
	if hasVUI {
		ar := r.u(1) // aspect_ratio_info_present_flag
		w.put(ar, 1)
		if ar == 1 {
			idc := r.u(8)
			w.put(idc, 8)
			if idc == 255 { // Extended_SAR
				w.put(r.u(32), 32)
			}
		}
		ov := r.u(1) // overscan_info_present_flag
		w.put(ov, 1)
		if ov == 1 {
			w.put(r.u(1), 1)
		}
		if r.u(1) == 1 { // video_signal_type_present_flag
			videoFormat = r.u(3)
			full := r.u(1)
			if r.u(1) == 1 { // colour_description_present_flag
				cp, tc, mc := r.u(8), r.u(8), r.u(8)
				if full == 0 && cp == 1 && tc == 1 && mc == 1 && r.err == nil {
					return nal, false, nil
				}
			}
		}
	} else {
		w.put(0, 1) // aspect_ratio_info_present_flag
		w.put(0, 1) // overscan_info_present_flag
	}
	if r.err != nil {
		return nil, false, r.err
	}
	w.put(1, 1)           // video_signal_type_present_flag
	w.put(videoFormat, 3) // video_format
	w.put(0, 1)           // video_full_range_flag
	w.put(1, 1)           // colour_description_present_flag
	w.put(1, 8)           // colour_primaries: BT.709
	w.put(1, 8)           // transfer_characteristics: BT.709
	w.put(1, 8)           // matrix_coefficients: BT.709
	if hasVUI {
		// The rest of the VUI (chroma location, timing, HRD, restrictions) is unchanged:
		// copy it up to the rbsp_stop_one_bit.
		stop := h264StopBit(rbsp)
		if stop < r.pos {
			return nil, false, errors.New("h264: SPS has no rbsp_stop_one_bit")
		}
		w.copyBits(rbsp, r.pos, stop)
	} else {
		w.put(0, 1) // chroma_loc_info_present_flag
		w.put(0, 1) // timing_info_present_flag
		w.put(0, 1) // nal_hrd_parameters_present_flag
		w.put(0, 1) // vcl_hrd_parameters_present_flag
		w.put(0, 1) // pic_struct_present_flag
		w.put(1, 1) // bitstream_restriction_flag
		w.put(1, 1) // motion_vectors_over_pic_boundaries_flag
		w.ue(2)     // max_bytes_per_pic_denom (default)
		w.ue(1)     // max_bits_per_mb_denom (default)
		w.ue(15)    // log2_max_mv_length_horizontal (default)
		w.ue(15)    // log2_max_mv_length_vertical (default)
		w.ue(0)     // max_num_reorder_frames
		w.ue(max(maxRefFrames, 1))
	}
	w.put(1, 1) // rbsp_stop_one_bit
	for w.n%8 != 0 {
		w.put(0, 1)
	}
	return h264Escape(append(make([]byte, 0, len(nal)+8), nal[0]), w.b), true, nil
}

// h264ReadSPSHead parses an SPS RBSP up to vui_parameters_present_flag and returns a reader
// positioned at that flag plus max_num_ref_frames. Parse errors are left in r.err.
func h264ReadSPSHead(rbsp []byte) (r h264BitReader, maxRefFrames uint32) {
	r.b = rbsp
	profile := r.u(8)
	r.u(16) // constraint flags, level_idc
	r.ue()  // seq_parameter_set_id
	switch profile {
	case 100, 110, 122, 244, 44, 83, 86, 118, 128, 138, 139, 134, 135:
		chroma := r.ue()
		if chroma == 3 {
			r.u(1) // separate_colour_plane_flag
		}
		r.ue()           // bit_depth_luma_minus8
		r.ue()           // bit_depth_chroma_minus8
		r.u(1)           // qpprime_y_zero_transform_bypass_flag
		if r.u(1) == 1 { // seq_scaling_matrix_present_flag
			lists := 8
			if chroma == 3 {
				lists = 12
			}
			for i := range lists {
				if r.u(1) == 0 {
					continue
				}
				size := 16
				if i >= 6 {
					size = 64
				}
				last, next := int32(8), int32(8)
				for range size {
					if next != 0 {
						next = (last + r.se() + 256) % 256
					}
					if next != 0 {
						last = next
					}
				}
			}
		}
	}
	r.ue()          // log2_max_frame_num_minus4
	switch r.ue() { // pic_order_cnt_type
	case 0:
		r.ue() // log2_max_pic_order_cnt_lsb_minus4
	case 1:
		r.u(1) // delta_pic_order_always_zero_flag
		r.se() // offset_for_non_ref_pic
		r.se() // offset_for_top_to_bottom_field
		for n := r.ue(); n > 0 && r.err == nil; n-- {
			r.se()
		}
	}
	maxRefFrames = r.ue()
	r.u(1)           // gaps_in_frame_num_value_allowed_flag
	r.ue()           // pic_width_in_mbs_minus1
	r.ue()           // pic_height_in_map_units_minus1
	if r.u(1) == 0 { // frame_mbs_only_flag
		r.u(1) // mb_adaptive_frame_field_flag
	}
	r.u(1)           // direct_8x8_inference_flag
	if r.u(1) == 1 { // frame_cropping_flag
		r.ue()
		r.ue()
		r.ue()
		r.ue()
	}
	return r, maxRefFrames
}

// h264Unescape removes emulation_prevention_three_bytes.
func h264Unescape(b []byte) []byte {
	out := make([]byte, 0, len(b))
	zeros := 0
	for _, c := range b {
		if zeros >= 2 && c == 3 {
			zeros = 0
			continue
		}
		out = append(out, c)
		if c == 0 {
			zeros++
		} else {
			zeros = 0
		}
	}
	return out
}

// h264Escape appends rbsp to dst with emulation_prevention_three_bytes inserted.
func h264Escape(dst, rbsp []byte) []byte {
	zeros := 0
	for _, c := range rbsp {
		if zeros >= 2 && c <= 3 {
			dst = append(dst, 3)
			zeros = 0
		}
		dst = append(dst, c)
		if c == 0 {
			zeros++
		} else {
			zeros = 0
		}
	}
	return dst
}

// h264StopBit returns the bit position of the rbsp_stop_one_bit (the last set bit), or -1.
func h264StopBit(rbsp []byte) int {
	for i := len(rbsp) - 1; i >= 0; i-- {
		if rbsp[i] != 0 {
			return i*8 + 7 - bits.TrailingZeros8(rbsp[i])
		}
	}
	return -1
}

type h264BitReader struct {
	b   []byte
	pos int // in bits
	err error
}

func (r *h264BitReader) u(n int) uint32 {
	var v uint32
	for range n {
		if r.pos >= len(r.b)*8 {
			r.err = errors.New("h264: SPS truncated")
			return 0
		}
		v = v<<1 | uint32(r.b[r.pos/8]>>(7-r.pos%8)&1)
		r.pos++
	}
	return v
}

func (r *h264BitReader) ue() uint32 {
	zeros := 0
	for r.u(1) == 0 {
		if zeros++; zeros > 31 || r.err != nil {
			r.err = errors.New("h264: bad Exp-Golomb code in SPS")
			return 0
		}
	}
	return 1<<zeros - 1 + r.u(zeros)
}

func (r *h264BitReader) se() int32 {
	v := r.ue()
	if v&1 == 1 {
		return int32(v/2 + 1)
	}
	return -int32(v / 2)
}

type h264BitWriter struct {
	b []byte
	n int // bits written
}

func (w *h264BitWriter) put(v uint32, n int) {
	for i := n - 1; i >= 0; i-- {
		if w.n%8 == 0 {
			w.b = append(w.b, 0)
		}
		w.b[len(w.b)-1] |= byte(v>>i&1) << (7 - w.n%8)
		w.n++
	}
}

func (w *h264BitWriter) ue(v uint32) {
	n := bits.Len32(v + 1)
	w.put(0, n-1)
	w.put(v+1, n)
}

// copyBits appends bits [from, to) of b.
func (w *h264BitWriter) copyBits(b []byte, from, to int) {
	for p := from; p < to; p++ {
		w.put(uint32(b[p/8]>>(7-p%8)&1), 1)
	}
}
