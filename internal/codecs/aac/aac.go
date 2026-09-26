// Package aac provides the minimal AAC bitstream handling needed to bridge
// between MPEG-TS's ADTS-framed AAC (each frame self-describing) and
// RTMP/FLV's raw-AAC framing (a single AudioSpecificConfig "sequence
// header" sent once, then bare frames with no per-frame header).
package aac

import "fmt"

// Frame is one ADTS-framed AAC access unit.
type Frame struct {
	Profile         byte // ADTS 2-bit profile (0=Main,1=LC,2=SSR,3=LTP)
	SampleRateIndex byte
	ChannelConfig   byte
	Payload         []byte // raw AAC data, ADTS header stripped
}

// SplitADTS parses one or more back-to-back ADTS frames out of data (a PES
// payload commonly contains several).
func SplitADTS(data []byte) ([]Frame, error) {
	var frames []Frame
	for i := 0; i+7 <= len(data); {
		if data[i] != 0xff || data[i+1]&0xf0 != 0xf0 {
			return frames, fmt.Errorf("aac: lost ADTS sync at offset %d", i)
		}
		protectionAbsent := data[i+1] & 0x01
		profile := (data[i+2] >> 6) & 0x03
		sampleRateIdx := (data[i+2] >> 2) & 0x0f
		channelConfig := ((data[i+2] & 0x01) << 2) | (data[i+3] >> 6)
		frameLen := (int(data[i+3]&0x03) << 11) | (int(data[i+4]) << 3) | (int(data[i+5]) >> 5)

		headerLen := 7
		if protectionAbsent == 0 {
			headerLen = 9
		}
		if frameLen < headerLen || i+frameLen > len(data) {
			return frames, fmt.Errorf("aac: ADTS frame length %d invalid at offset %d", frameLen, i)
		}

		frames = append(frames, Frame{
			Profile:         profile,
			SampleRateIndex: sampleRateIdx,
			ChannelConfig:   channelConfig,
			Payload:         data[i+headerLen : i+frameLen],
		})
		i += frameLen
	}
	return frames, nil
}

// BuildADTSHeader returns a 7-byte ADTS header (no CRC) for a frame whose
// total length (header + payload) is frameLen.
func BuildADTSHeader(profile, sampleRateIndex, channelConfig byte, frameLen int) []byte {
	h := make([]byte, 7)
	h[0] = 0xff
	h[1] = 0xf1 // MPEG-4, layer 0, protection_absent=1
	h[2] = (profile << 6) | (sampleRateIndex << 2) | (channelConfig >> 2)
	h[3] = (channelConfig&0x03)<<6 | byte(frameLen>>11)
	h[4] = byte(frameLen >> 3)
	h[5] = byte(frameLen<<5) | 0x1f
	h[6] = 0xfc
	return h
}

// BuildAudioSpecificConfig returns the 2-byte AudioSpecificConfig RTMP/FLV
// expects as the AAC "sequence header", built from ADTS-style fields.
func BuildAudioSpecificConfig(profile, sampleRateIndex, channelConfig byte) []byte {
	audioObjectType := profile + 1 // ADTS profile -> MPEG-4 audio object type
	asc := make([]byte, 2)
	asc[0] = (audioObjectType << 3) | (sampleRateIndex >> 1)
	asc[1] = (sampleRateIndex&0x01)<<7 | (channelConfig << 3)
	return asc
}

// ParseAudioSpecificConfig extracts the ADTS-equivalent profile/sample
// rate index/channel config from a 2-byte AudioSpecificConfig.
func ParseAudioSpecificConfig(asc []byte) (profile, sampleRateIndex, channelConfig byte, err error) {
	if len(asc) < 2 {
		return 0, 0, 0, fmt.Errorf("aac: AudioSpecificConfig too short")
	}
	audioObjectType := asc[0] >> 3
	if audioObjectType == 0 {
		return 0, 0, 0, fmt.Errorf("aac: invalid audio object type 0")
	}
	profile = audioObjectType - 1
	sampleRateIndex = ((asc[0] & 0x07) << 1) | (asc[1] >> 7)
	channelConfig = (asc[1] >> 3) & 0x0f
	return profile, sampleRateIndex, channelConfig, nil
}
