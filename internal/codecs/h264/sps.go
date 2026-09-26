package h264

import "fmt"

// bitReader reads individual bits (MSB-first, matching H.264's RBSP bit
// order) and Exp-Golomb codes out of a byte slice.
type bitReader struct {
	data []byte
	pos  int // bit position
}

func (r *bitReader) bit() (uint32, error) {
	if r.pos >= len(r.data)*8 {
		return 0, fmt.Errorf("h264: sps: read past end of data")
	}
	b := (r.data[r.pos/8] >> (7 - r.pos%8)) & 1
	r.pos++
	return uint32(b), nil
}

func (r *bitReader) bits(n int) (uint32, error) {
	var v uint32
	for i := 0; i < n; i++ {
		b, err := r.bit()
		if err != nil {
			return 0, err
		}
		v = v<<1 | b
	}
	return v, nil
}

// ue reads an Exp-Golomb-coded unsigned value (the "ue(v)" descriptor used
// throughout the SPS/PPS syntax).
func (r *bitReader) ue() (uint32, error) {
	zeros := 0
	for {
		b, err := r.bit()
		if err != nil {
			return 0, err
		}
		if b != 0 {
			break
		}
		zeros++
		if zeros > 32 {
			return 0, fmt.Errorf("h264: sps: exp-golomb code too long")
		}
	}
	if zeros == 0 {
		return 0, nil
	}
	rest, err := r.bits(zeros)
	if err != nil {
		return 0, err
	}
	return (1 << uint(zeros)) - 1 + rest, nil
}

// SPSDimensions is the coded picture size decoded from an SPS, in pixels,
// after cropping is applied (i.e. the actual displayed resolution).
type SPSDimensions struct {
	Width, Height int
}

// hasChromaFormatIDC lists the profile_idc values whose SPS syntax
// includes the seq_scaling_matrix / chroma_format_idc fields (Rec.
// ITU-T H.264, section 7.3.2.1.1) — the "high" profile family. Baseline
// (66), Main (77) and Extended (88), the overwhelming majority of what
// real-world encoders (including this project's own muxer) produce,
// don't have this block at all.
func hasChromaFormatIDC(profileIDC uint32) bool {
	switch profileIDC {
	case 100, 110, 122, 244, 44, 83, 86, 118, 128, 138, 139, 134, 135:
		return true
	default:
		return false
	}
}

// ParseSPSDimensions decodes the coded width/height (after cropping) from
// a raw SPS NALU (the 1-byte NAL header included, as returned by
// SplitAnnexB — i.e. sps[0] has nal_unit_type 7). Used only to populate
// an informational onMetaData width/height for RTMP output — actual
// decoding relies on the SPS/PPS bytes themselves, sent through
// unmodified, so a failure here never affects playability, only that one
// metadata field.
func ParseSPSDimensions(sps []byte) (SPSDimensions, error) {
	if len(sps) < 2 || Type(sps) != NALTypeSPS {
		return SPSDimensions{}, fmt.Errorf("h264: not an SPS NALU")
	}
	r := &bitReader{data: sps[1:]} // skip the NAL header byte

	profileIDC, err := r.bits(8)
	if err != nil {
		return SPSDimensions{}, err
	}
	if _, err := r.bits(8); err != nil { // constraint_set flags + reserved
		return SPSDimensions{}, err
	}
	if _, err := r.bits(8); err != nil { // level_idc
		return SPSDimensions{}, err
	}
	if _, err := r.ue(); err != nil { // seq_parameter_set_id
		return SPSDimensions{}, err
	}

	chromaFormatIDC := uint32(1) // default 4:2:0 when this profile family doesn't signal it
	if hasChromaFormatIDC(profileIDC) {
		chromaFormatIDC, err = r.ue()
		if err != nil {
			return SPSDimensions{}, err
		}
		if chromaFormatIDC == 3 {
			if _, err := r.bit(); err != nil { // separate_colour_plane_flag
				return SPSDimensions{}, err
			}
		}
		if _, err := r.ue(); err != nil { // bit_depth_luma_minus8
			return SPSDimensions{}, err
		}
		if _, err := r.ue(); err != nil { // bit_depth_chroma_minus8
			return SPSDimensions{}, err
		}
		if _, err := r.bit(); err != nil { // qpprime_y_zero_transform_bypass_flag
			return SPSDimensions{}, err
		}
		seqScalingMatrixPresent, err := r.bit()
		if err != nil {
			return SPSDimensions{}, err
		}
		if seqScalingMatrixPresent != 0 {
			// Scaling lists are involved enough (8 or 12 lists, each
			// itself variable-length) that decoding them just to skip
			// past is more code than the field is worth here; give up
			// rather than risk misparsing the rest of the SPS.
			return SPSDimensions{}, fmt.Errorf("h264: sps: seq_scaling_matrix_present unsupported")
		}
	}

	if _, err := r.ue(); err != nil { // log2_max_frame_num_minus4
		return SPSDimensions{}, err
	}
	picOrderCntType, err := r.ue()
	if err != nil {
		return SPSDimensions{}, err
	}
	switch picOrderCntType {
	case 0:
		if _, err := r.ue(); err != nil { // log2_max_pic_order_cnt_lsb_minus4
			return SPSDimensions{}, err
		}
	case 1:
		if _, err := r.bit(); err != nil { // delta_pic_order_always_zero_flag
			return SPSDimensions{}, err
		}
		if _, err := r.se(); err != nil { // offset_for_non_ref_pic
			return SPSDimensions{}, err
		}
		if _, err := r.se(); err != nil { // offset_for_top_to_bottom_field
			return SPSDimensions{}, err
		}
		numRefFrames, err := r.ue()
		if err != nil {
			return SPSDimensions{}, err
		}
		for i := uint32(0); i < numRefFrames; i++ {
			if _, err := r.se(); err != nil { // offset_for_ref_frame[i]
				return SPSDimensions{}, err
			}
		}
	}

	if _, err := r.ue(); err != nil { // max_num_ref_frames
		return SPSDimensions{}, err
	}
	if _, err := r.bit(); err != nil { // gaps_in_frame_num_value_allowed_flag
		return SPSDimensions{}, err
	}
	picWidthInMbsMinus1, err := r.ue()
	if err != nil {
		return SPSDimensions{}, err
	}
	picHeightInMapUnitsMinus1, err := r.ue()
	if err != nil {
		return SPSDimensions{}, err
	}
	frameMbsOnlyFlag, err := r.bit()
	if err != nil {
		return SPSDimensions{}, err
	}
	if frameMbsOnlyFlag == 0 {
		if _, err := r.bit(); err != nil { // mb_adaptive_frame_field_flag
			return SPSDimensions{}, err
		}
	}
	if _, err := r.bit(); err != nil { // direct_8x8_inference_flag
		return SPSDimensions{}, err
	}

	var cropLeft, cropRight, cropTop, cropBottom uint32
	frameCroppingFlag, err := r.bit()
	if err != nil {
		return SPSDimensions{}, err
	}
	if frameCroppingFlag != 0 {
		if cropLeft, err = r.ue(); err != nil {
			return SPSDimensions{}, err
		}
		if cropRight, err = r.ue(); err != nil {
			return SPSDimensions{}, err
		}
		if cropTop, err = r.ue(); err != nil {
			return SPSDimensions{}, err
		}
		if cropBottom, err = r.ue(); err != nil {
			return SPSDimensions{}, err
		}
	}

	// Crop units per Rec. ITU-T H.264 §7.4.2.1.1: for monochrome
	// (chroma_format_idc 0), CropUnitX=1, CropUnitY=2-frame_mbs_only_flag;
	// otherwise CropUnitX=SubWidthC, CropUnitY=SubHeightC*(2-frame_mbs_only_flag).
	subWidthC, subHeightC := 2, 2 // chroma_format_idc 1 (4:2:0), the common case
	switch chromaFormatIDC {
	case 2:
		subWidthC, subHeightC = 2, 1
	case 3:
		subWidthC, subHeightC = 1, 1
	}
	cropUnitX, cropUnitY := subWidthC, subHeightC*(2-int(frameMbsOnlyFlag))
	if chromaFormatIDC == 0 {
		cropUnitX, cropUnitY = 1, 2-int(frameMbsOnlyFlag)
	}

	width := int(picWidthInMbsMinus1+1)*16 - int(cropLeft+cropRight)*cropUnitX
	height := (2-int(frameMbsOnlyFlag))*int(picHeightInMapUnitsMinus1+1)*16 - int(cropTop+cropBottom)*cropUnitY

	if width <= 0 || height <= 0 {
		return SPSDimensions{}, fmt.Errorf("h264: sps: decoded non-positive dimensions (%dx%d)", width, height)
	}
	return SPSDimensions{Width: width, Height: height}, nil
}

// se reads an Exp-Golomb-coded signed value (the "se(v)" descriptor).
func (r *bitReader) se() (int32, error) {
	v, err := r.ue()
	if err != nil {
		return 0, err
	}
	if v%2 == 0 {
		return -int32(v / 2), nil
	}
	return int32((v + 1) / 2), nil
}
