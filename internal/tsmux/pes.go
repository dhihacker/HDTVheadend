// Package tsmux implements the minimal MPEG-TS muxing/demuxing needed to
// bridge TS to elementary H.264+AAC access units and back, for the RTMP
// input/output bridge. It intentionally supports exactly one video PID
// (H.264, stream_type 0x1b) and one audio PID (AAC ADTS, stream_type
// 0x0f) per program — everything a live H.264/AAC RTMP restream needs, and
// no more.
package tsmux

// decodeTimestamp decodes a 33-bit 90kHz PTS/DTS from its 5-byte PES
// encoding (ITU-T H.222.0 2.4.3.7).
func decodeTimestamp(b []byte) int64 {
	return (int64(b[0]&0x0e) << 29) | (int64(b[1]) << 22) | (int64(b[2]&0xfe) << 14) | (int64(b[3]) << 7) | (int64(b[4]) >> 1)
}

// encodeTimestamp encodes a 33-bit 90kHz PTS/DTS into its 5-byte PES
// encoding, with the given 4-bit prefix (0x2 = PTS only, 0x3 = PTS when
// DTS follows, 0x1 = DTS).
func encodeTimestamp(prefix byte, ts int64) []byte {
	b := make([]byte, 5)
	b[0] = (prefix << 4) | byte((ts>>29)&0x0e) | 0x01
	b[1] = byte(ts >> 22)
	b[2] = byte((ts>>14)&0xfe) | 0x01
	b[3] = byte(ts >> 7)
	b[4] = byte((ts<<1)&0xfe) | 0x01
	return b
}

// crc32MPEG computes the CRC-32/MPEG-2 checksum (poly 0x04C11DB7, init
// 0xFFFFFFFF, no reflection, no final XOR) PAT/PMT sections use.
func crc32MPEG(data []byte) uint32 {
	crc := uint32(0xFFFFFFFF)
	for _, b := range data {
		crc ^= uint32(b) << 24
		for i := 0; i < 8; i++ {
			if crc&0x80000000 != 0 {
				crc = (crc << 1) ^ 0x04C11DB7
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}
