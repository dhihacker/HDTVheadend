// Package tsutil provides low-level MPEG-TS packet helpers shared by
// inputs, outputs and the conditional-access layer.
package tsutil

import "errors"

const (
	PacketSize = 188
	SyncByte   = 0x47

	// Scrambling control field values (byte 3, bits 6-7).
	ScramblingNone = 0x0
	ScramblingEven = 0x2
	ScramblingOdd  = 0x3
)

var ErrShortPacket = errors.New("tsutil: packet shorter than 188 bytes")

// PID returns the 13-bit packet identifier of a TS packet.
func PID(pkt []byte) uint16 {
	return uint16(pkt[1]&0x1f)<<8 | uint16(pkt[2])
}

// ScramblingControl returns the transport_scrambling_control field.
func ScramblingControl(pkt []byte) byte {
	return (pkt[3] >> 6) & 0x3
}

// SetScramblingControl overwrites the transport_scrambling_control field.
func SetScramblingControl(pkt []byte, v byte) {
	pkt[3] = pkt[3]&0x3f | (v&0x3)<<6
}

// HasAdaptationField reports whether the adaptation_field_control bits
// indicate an adaptation field is present.
func HasAdaptationField(pkt []byte) bool {
	return pkt[3]&0x20 != 0
}

// HasPayload reports whether the adaptation_field_control bits indicate a
// payload is present.
func HasPayload(pkt []byte) bool {
	return pkt[3]&0x10 != 0
}

// PayloadOffset returns the byte offset of the payload within the packet,
// accounting for an optional adaptation field.
func PayloadOffset(pkt []byte) int {
	if !HasAdaptationField(pkt) {
		return 4
	}
	if len(pkt) < 5 {
		return len(pkt)
	}
	adaptLen := int(pkt[4])
	off := 5 + adaptLen
	if off > len(pkt) {
		off = len(pkt)
	}
	return off
}

// Validate checks that pkt looks like a single, well-formed TS packet.
func Validate(pkt []byte) error {
	if len(pkt) < PacketSize {
		return ErrShortPacket
	}
	return nil
}

// Split slices a contiguous byte buffer into individual 188-byte TS
// packets, resyncing on the sync byte if the buffer doesn't start aligned.
func Split(buf []byte) [][]byte {
	var pkts [][]byte
	n := len(buf)
	start := 0
	// Resync: find first sync byte that also has a sync byte 188 bytes later
	// (or is within the last packet of the buffer).
	for start < n && buf[start] != SyncByte {
		start++
	}
	for i := start; i+PacketSize <= n; i += PacketSize {
		if buf[i] != SyncByte {
			// Lost sync; try to resync from here.
			j := i
			for j < n && buf[j] != SyncByte {
				j++
			}
			i = j - PacketSize
			continue
		}
		pkts = append(pkts, buf[i:i+PacketSize])
	}
	return pkts
}
