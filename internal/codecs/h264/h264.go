// Package h264 provides the minimal H.264 bitstream handling needed to
// bridge between MPEG-TS's Annex-B NALU framing (start codes) and RTMP/FLV's
// AVCC framing (length-prefixed NALUs plus a separate AVCDecoderConfiguration
// "sequence header" carrying SPS/PPS). It does not parse SPS/PPS beyond the
// three fixed bytes needed for the AVCDecoderConfigurationRecord header.
package h264

import (
	"encoding/binary"
	"fmt"
)

// NAL unit types (ITU-T H.264 Table 7-1), the ones this package cares about.
const (
	NALTypeNonIDR NALUType = 1
	NALTypeIDR    NALUType = 5
	NALTypeSEI    NALUType = 6
	NALTypeSPS    NALUType = 7
	NALTypePPS    NALUType = 8
	NALTypeAUD    NALUType = 9
)

type NALUType byte

// Type returns a NALU's type (the low 5 bits of its first byte). nalu must
// not include a start code.
func Type(nalu []byte) NALUType {
	if len(nalu) == 0 {
		return 0
	}
	return NALUType(nalu[0] & 0x1f)
}

// SplitAnnexB splits a byte stream framed with Annex-B start codes (00 00 01
// or 00 00 00 01) into individual NALUs, none of which include the start
// code. Trailing zero-padding bytes commonly left before the next start
// code are trimmed.
func SplitAnnexB(data []byte) [][]byte {
	var nalus [][]byte
	starts := findStartCodes(data)
	for i, s := range starts {
		end := len(data)
		if i+1 < len(starts) {
			end = starts[i+1].pos
		}
		naluStart := s.pos + s.len
		if naluStart >= end {
			continue
		}
		nalu := data[naluStart:end]
		// Trim trailing zero bytes that just pad up to the next start code.
		for len(nalu) > 0 && nalu[len(nalu)-1] == 0x00 {
			nalu = nalu[:len(nalu)-1]
		}
		if len(nalu) > 0 {
			nalus = append(nalus, nalu)
		}
	}
	return nalus
}

type startCode struct {
	pos int
	len int
}

func findStartCodes(data []byte) []startCode {
	var out []startCode
	for i := 0; i+2 < len(data); {
		if data[i] == 0 && data[i+1] == 0 && data[i+2] == 1 {
			if i > 0 && data[i-1] == 0 {
				out = append(out, startCode{pos: i - 1, len: 4})
			} else {
				out = append(out, startCode{pos: i, len: 3})
			}
			i += 3
			continue
		}
		i++
	}
	return out
}

// HasKeyframe reports whether nalus (as returned by SplitAnnexB) contains an
// IDR slice.
func HasKeyframe(nalus [][]byte) bool {
	for _, n := range nalus {
		if Type(n) == NALTypeIDR {
			return true
		}
	}
	return false
}

// AnnexBToAVCC concatenates nalus (SPS/PPS/AUD excluded by the caller if
// desired) into AVCC framing: each NALU prefixed with its 4-byte
// big-endian length, no start codes.
func AnnexBToAVCC(nalus [][]byte) []byte {
	size := 0
	for _, n := range nalus {
		size += 4 + len(n)
	}
	out := make([]byte, 0, size)
	var lenBuf [4]byte
	for _, n := range nalus {
		binary.BigEndian.PutUint32(lenBuf[:], uint32(len(n)))
		out = append(out, lenBuf[:]...)
		out = append(out, n...)
	}
	return out
}

// AVCCToAnnexB parses length-prefixed NALUs (lengthSize bytes per prefix, as
// declared by the stream's AVCDecoderConfigurationRecord) and returns them
// as individual NALUs (without start codes; use SplitAnnexB's output shape
// directly, or call AnnexBFrame to add start codes back for TS/PES output).
func AVCCToAnnexB(data []byte, lengthSize int) ([][]byte, error) {
	if lengthSize <= 0 || lengthSize > 4 {
		lengthSize = 4
	}
	var nalus [][]byte
	for i := 0; i+lengthSize <= len(data); {
		length := 0
		for b := 0; b < lengthSize; b++ {
			length = length<<8 | int(data[i+b])
		}
		i += lengthSize
		if i+length > len(data) {
			return nalus, fmt.Errorf("h264: AVCC NALU length %d exceeds remaining data", length)
		}
		nalus = append(nalus, data[i:i+length])
		i += length
	}
	return nalus, nil
}

// AnnexBFrame joins nalus with 4-byte Annex-B start codes (00 00 00 01),
// suitable for embedding directly in a PES payload.
func AnnexBFrame(nalus [][]byte) []byte {
	size := 0
	for _, n := range nalus {
		size += 4 + len(n)
	}
	out := make([]byte, 0, size)
	startCode4 := []byte{0, 0, 0, 1}
	for _, n := range nalus {
		out = append(out, startCode4...)
		out = append(out, n...)
	}
	return out
}

// DecoderConfig holds one SPS and one PPS — the common case for live
// broadcast/streaming H.264. Multiple SPS/PPS (rare outside adaptive
// resolution switching) are not supported.
type DecoderConfig struct {
	SPS []byte
	PPS []byte
}

// BuildAVCDecoderConfigurationRecord builds the "sequence header" payload
// FLV/RTMP expects once, before any AVCPacketType=NALU video data (ISO
// 14496-15 5.2.4.1). Profile/compatibility/level are read directly from the
// SPS's first three RBSP bytes.
func BuildAVCDecoderConfigurationRecord(c DecoderConfig) ([]byte, error) {
	if len(c.SPS) < 4 {
		return nil, fmt.Errorf("h264: SPS too short (%d bytes)", len(c.SPS))
	}
	buf := make([]byte, 0, 11+len(c.SPS)+len(c.PPS))
	buf = append(buf,
		1,        // configurationVersion
		c.SPS[1], // AVCProfileIndication
		c.SPS[2], // profile_compatibility
		c.SPS[3], // AVCLevelIndication
		0xff,     // reserved(6)=111111, lengthSizeMinusOne=3 (4-byte lengths)
		0xe1,     // reserved(3)=111, numOfSequenceParameterSets=1
	)
	buf = append(buf, byte(len(c.SPS)>>8), byte(len(c.SPS)))
	buf = append(buf, c.SPS...)
	buf = append(buf, 1) // numOfPictureParameterSets
	buf = append(buf, byte(len(c.PPS)>>8), byte(len(c.PPS)))
	buf = append(buf, c.PPS...)
	return buf, nil
}

// ParseAVCDecoderConfigurationRecord extracts the (first) SPS/PPS and the
// AVCC length-prefix size from a sequence header payload.
func ParseAVCDecoderConfigurationRecord(data []byte) (cfg DecoderConfig, lengthSize int, err error) {
	if len(data) < 6 {
		return cfg, 0, fmt.Errorf("h264: AVCDecoderConfigurationRecord too short")
	}
	lengthSize = int(data[4]&0x03) + 1
	numSPS := int(data[5] & 0x1f)
	i := 6
	for s := 0; s < numSPS; s++ {
		if i+2 > len(data) {
			return cfg, 0, fmt.Errorf("h264: truncated SPS length")
		}
		l := int(data[i])<<8 | int(data[i+1])
		i += 2
		if i+l > len(data) {
			return cfg, 0, fmt.Errorf("h264: truncated SPS data")
		}
		if s == 0 {
			cfg.SPS = data[i : i+l]
		}
		i += l
	}
	if i >= len(data) {
		return cfg, 0, fmt.Errorf("h264: missing PPS count")
	}
	numPPS := int(data[i])
	i++
	for p := 0; p < numPPS; p++ {
		if i+2 > len(data) {
			return cfg, 0, fmt.Errorf("h264: truncated PPS length")
		}
		l := int(data[i])<<8 | int(data[i+1])
		i += 2
		if i+l > len(data) {
			return cfg, 0, fmt.Errorf("h264: truncated PPS data")
		}
		if p == 0 {
			cfg.PPS = data[i : i+l]
		}
		i += l
	}
	return cfg, lengthSize, nil
}
