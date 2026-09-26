// Package biss implements BISS (Basic Interoperable Scrambling System)
// descrambling for MPEG-TS. BISS is the standard professional
// broadcast-contribution conditional access used for satellite/cable
// interoperability testing and point-to-point feeds: a distributor of a
// feed you're authorized to receive gives you a session word or control
// word, and you apply it here — this is not a subscriber pay-TV system.
package biss

import (
	"encoding/hex"
	"fmt"

	"hdtvheadend/internal/dvbcsa"
	"hdtvheadend/internal/tsutil"
)

// Descrambler applies a fixed BISS control word to MPEG-TS packets.
type Descrambler struct {
	key *dvbcsa.Key
}

// NewSessionWord derives a BISS-1 descrambler from a 12-hex-digit (6-byte)
// session word, using the standard BISS-1 checksum-derived control word:
// CW = SW[0:6] + (SW0^SW1^SW2) + (SW3^SW4^SW5).
func NewSessionWord(hexSW string) (*Descrambler, error) {
	sw, err := decodeHex(hexSW, 6)
	if err != nil {
		return nil, fmt.Errorf("biss: session word: %w", err)
	}
	var cw [8]byte
	copy(cw[:6], sw)
	cw[6] = sw[0] ^ sw[1] ^ sw[2]
	cw[7] = sw[3] ^ sw[4] ^ sw[5]
	return &Descrambler{key: dvbcsa.NewKey(cw)}, nil
}

// NewControlWord derives a descrambler from a 16-hex-digit (8-byte) BISS-E
// / BISS-CA control word, used directly with no checksum derivation.
func NewControlWord(hexCW string) (*Descrambler, error) {
	raw, err := decodeHex(hexCW, 8)
	if err != nil {
		return nil, fmt.Errorf("biss: control word: %w", err)
	}
	var cw [8]byte
	copy(cw[:], raw)
	return &Descrambler{key: dvbcsa.NewKey(cw)}, nil
}

func decodeHex(s string, n int) ([]byte, error) {
	b, err := hex.DecodeString(s)
	if err != nil {
		return nil, err
	}
	if len(b) != n {
		return nil, fmt.Errorf("expected %d bytes (%d hex chars), got %d", n, n*2, len(b))
	}
	return b, nil
}

// DescramblePacket descrambles a single 188-byte TS packet in place, if it
// is marked as scrambled. Packets with no payload, or already clear, are
// left untouched.
func (d *Descrambler) DescramblePacket(pkt []byte) {
	if tsutil.ScramblingControl(pkt) == tsutil.ScramblingNone {
		return
	}
	off := tsutil.PayloadOffset(pkt)
	if off >= len(pkt) {
		return
	}
	d.key.Decrypt(pkt[off:])
	tsutil.SetScramblingControl(pkt, tsutil.ScramblingNone)
}

// DescrambleStream descrambles every packet in a buffer of concatenated
// 188-byte TS packets in place.
func (d *Descrambler) DescrambleStream(buf []byte) {
	for _, pkt := range tsutil.Split(buf) {
		d.DescramblePacket(pkt)
	}
}
