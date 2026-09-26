package dvbcsa

// This file implements the DVB Common Scrambling Algorithm (CSA1) block and
// stream ciphers and the CSA key schedule, built on the public constant
// tables in tables.go. The algorithm structure follows the published
// reference behavior of libdvbcsa (dvbcsa_block.c / dvbcsa_key.c /
// dvbcsa_stream.c / dvbcsa_algo.c), reimplemented in Go.
//
// CSA1 is the cipher underlying BISS (Basic Interoperable Scrambling
// System), the standard professional broadcast contribution-feed
// encryption used for satellite/cable interoperability testing and
// point-to-point feeds — not a subscriber conditional-access system.

// Key holds a derived CSA key: the control word, its nibble-swapped form
// used by the stream cipher, and the expanded 56-byte block cipher
// schedule.
type Key struct {
	cw  [8]byte
	cws [8]byte
	sch [56]byte
}

// NewKey derives a CSA Key from an 8-byte control word.
func NewKey(cw [8]byte) *Key {
	a := loadLE64(cw[:])
	var cws [8]byte
	storeLE64(cws[:], ((a&0xf0f0f0f0f0f0f0f0)>>4)|((a&0x0f0f0f0f0f0f0f0f)<<4))
	return &Key{cw: cw, cws: cws, sch: scheduleBlock(cw)}
}

// Decrypt descrambles data in place, following the CSA chained-block +
// stream construction. Any trailing bytes beyond the last full 8-byte
// block are left untouched, matching the reference behavior for TS
// packet payloads whose length is not a multiple of 8.
func (k *Key) Decrypt(data []byte) {
	n := len(data)
	if n < 8 {
		return
	}
	alen := n &^ 7

	var iv [8]byte
	copy(iv[:], data[0:8])
	streamXor(k.cws, iv, data[8:n])

	blockDecrypt(k.sch[:], data[0:8], data[0:8])
	for i := 8; i < alen; i += 8 {
		xor64(data[i-8:i], data[i:i+8])
		blockDecrypt(k.sch[:], data[i:i+8], data[i:i+8])
	}
}

// Encrypt scrambles data in place; the inverse of Decrypt.
func (k *Key) Encrypt(data []byte) {
	n := len(data)
	if n < 8 {
		return
	}
	alen := n &^ 7

	blockEncrypt(k.sch[:], data[alen-8:alen], data[alen-8:alen])
	for i := alen - 16; i >= 0; i -= 8 {
		xor64(data[i:i+8], data[i+8:i+16])
		blockEncrypt(k.sch[:], data[i:i+8], data[i:i+8])
	}

	var iv [8]byte
	copy(iv[:], data[0:8])
	streamXor(k.cws, iv, data[8:n])
}

func xor64(dst, src []byte) {
	for i := 0; i < 8; i++ {
		dst[i] ^= src[i]
	}
}

func loadLE32(p []byte) uint32 {
	return uint32(p[0]) | uint32(p[1])<<8 | uint32(p[2])<<16 | uint32(p[3])<<24
}

func loadLE64(p []byte) uint64 {
	return uint64(p[0]) | uint64(p[1])<<8 | uint64(p[2])<<16 | uint64(p[3])<<24 |
		uint64(p[4])<<32 | uint64(p[5])<<40 | uint64(p[6])<<48 | uint64(p[7])<<56
}

func storeLE64(p []byte, w uint64) {
	p[0] = byte(w)
	p[1] = byte(w >> 8)
	p[2] = byte(w >> 16)
	p[3] = byte(w >> 24)
	p[4] = byte(w >> 32)
	p[5] = byte(w >> 40)
	p[6] = byte(w >> 48)
	p[7] = byte(w >> 56)
}

// --- block cipher ---

func blockDecrypt(key []byte, in, out []byte) {
	var W [8]byte
	copy(W[:], in)

	for i := 55; i >= 0; i-- {
		S := csaBlockSbox[key[i]^W[6]]
		L := W[7] ^ S

		W[7] = W[6]
		W[6] = W[5] ^ csaBlockPerm[S]
		W[5] = W[4]
		W[4] = W[3] ^ L
		W[3] = W[2] ^ L
		W[2] = W[1] ^ L
		W[1] = W[0]
		W[0] = L
	}

	copy(out, W[:])
}

func blockEncrypt(key []byte, in, out []byte) {
	var W [8]byte
	copy(W[:], in)

	for i := 0; i < 56; i++ {
		S := csaBlockSbox[key[i]^W[7]]
		L := W[1]

		W[1] = W[2] ^ W[0]
		W[2] = W[3] ^ W[0]
		W[3] = W[4] ^ W[0]
		W[4] = W[5]
		W[5] = W[6] ^ csaBlockPerm[S]
		W[6] = W[7]
		W[7] = W[0] ^ S
		W[0] = L
	}

	copy(out, W[:])
}

// --- key schedule ---

func scheduleBlock(cw [8]byte) [56]byte {
	var k [7]uint64
	k[6] = loadLE64(cw[:])
	for i := 6; i > 0; i-- {
		k[i-1] = permuteBlock(k[i])
	}

	var kk [56]byte
	for i := 0; i < 7; i++ {
		for j := 0; j < 8; j++ {
			kk[i*8+j] = byte(k[i]>>(uint(j)*8)) ^ byte(i)
		}
	}
	return kk
}

func permuteBlock(k uint64) uint64 {
	var n uint64
	for i := 0; i < 8; i++ {
		n |= csaKeyPerm[i][k&0xff]
		k >>= 8
	}
	return n
}

// --- stream cipher ---

var csaStreamOut = [16]byte{
	0x00, 0x55, 0x55, 0x00, 0xaa, 0xff, 0xff, 0xaa,
	0xaa, 0xff, 0xff, 0xaa, 0x00, 0x55, 0x55, 0x00,
}

func swapNbl(b byte) byte { return (b >> 4) | (b << 4) }

func streamRotate(pqzyx uint32, x uint32) uint32 {
	if pqzyx&0x1000 != 0 {
		return ((x << 1) | ((x >> 3) & 1)) & 0xf
	}
	return x
}

func streamSboxes(A uint64) uint32 {
	var res uint32
	var t uint64

	t = A & 0x2018004200
	res = uint32(csaStreamSbox[1][((t>>37)^(t>>27)^(t>>25)^(t>>11)^(t>>5))&0x1f])

	t = A & 0x4201480000
	res |= uint32(csaStreamSbox[4][((t>>38)^(t>>32)^(t>>22)^(t>>16)^(t>>18))&0x1f])

	t = A & 0x8040122000
	res |= uint32(csaStreamSbox[5][((t>>39)^(t>>29)^(t>>18)^(t>>14)^(t>>9))&0x1f])

	t = A & 0x1082010040
	res |= uint32(csaStreamSbox[0][((t>>36)^(t>>30)^(t>>23)^(t>>3)^(t>>12))&0x1f])

	t = A & 0x0004a00180
	res |= uint32(csaStreamSbox[2][((t>>26)^(t>>22)^(t>>19)^(t>>5)^(t>>3))&0x1f])

	t = A & 0x0100048820
	res |= uint32(csaStreamSbox[3][((t>>32)^(t>>17)^(t>>9)^(t>>2)^(t>>11))&0x1f])

	t = A & 0x0c20001400
	res |= uint32(csaStreamSbox[6][((t>>35)^(t>>33)^(t>>27)^(t>>9)^(t>>6))&0x1f])

	return res
}

func streamBSel(B uint64) uint32 {
	t := uint32(B >> 9)
	return (((t) ^ (t >> 27)) & 0x8) ^
		((t >> 18) & 0x9) ^
		(((t >> 22) ^ (t >> 7)) & 0x4) ^
		((t >> 4) & 0x5) ^
		(((t >> 24) ^ (t >> 6) ^ (t >> 11)) & 0x2) ^
		(((t >> 29) ^ (t >> 23)) & 0x1) ^
		((t >> 13) & 0xe)
}

func streamCfed(pqzyx uint32, cfed uint32) uint32 {
	return ((cfed & 0x0f00) >> 4) | uint32(csaStreamCdef[((cfed&0x10ff)|(pqzyx&0x2f00))>>4])
}

func streamInitRound(iv byte, A, B *uint64, pqzyx, cfed *uint32) {
	*A <<= 4
	*A |= (((*A) >> 40) ^ uint64(*pqzyx) ^ uint64(*cfed) ^ uint64(iv>>4)) & 0x0f

	tmp := uint32((((*B) >> 24) ^ ((*B) >> 36) ^ uint64(*pqzyx>>4) ^ uint64(iv)) & 0x0f)
	tmp = streamRotate(*pqzyx, tmp)

	*B <<= 4
	*B |= uint64(tmp)

	*cfed = streamCfed(*pqzyx, *cfed) ^ streamBSel(*B)
	*pqzyx = streamSboxes(*A)
}

func streamRound(A, B *uint64, pqzyx, cfed *uint32) {
	*A <<= 4
	*A |= (((*A) >> 40) ^ uint64(*pqzyx)) & 0xf

	tmp := uint32((((*B) >> 24) ^ ((*B) >> 36) ^ uint64(*pqzyx>>4)) & 0xf)

	*B <<= 4
	*B |= uint64(streamRotate(*pqzyx, tmp))

	*cfed = streamCfed(*pqzyx, *cfed) ^ streamBSel(*B)
	*pqzyx = streamSboxes(*A)
}

func streamXor(cw [8]byte, iv [8]byte, data []byte) {
	A := uint64(loadLE32(cw[0:4]))
	B := uint64(loadLE32(cw[4:8]))
	var pqzyx, cfed uint32

	for i := 0; i < 8; i++ {
		streamInitRound(iv[i], &A, &B, &pqzyx, &cfed)
		streamInitRound(swapNbl(iv[i]), &A, &B, &pqzyx, &cfed)
		streamInitRound(iv[i], &A, &B, &pqzyx, &cfed)
		streamInitRound(swapNbl(iv[i]), &A, &B, &pqzyx, &cfed)
	}

	for i := 0; i < len(data); i++ {
		streamRound(&A, &B, &pqzyx, &cfed)
		data[i] ^= csaStreamOut[cfed&0xf] & 0xc0

		streamRound(&A, &B, &pqzyx, &cfed)
		data[i] ^= csaStreamOut[cfed&0xf] & 0x30

		streamRound(&A, &B, &pqzyx, &cfed)
		data[i] ^= csaStreamOut[cfed&0xf] & 0x0c

		streamRound(&A, &B, &pqzyx, &cfed)
		data[i] ^= csaStreamOut[cfed&0xf] & 0x03
	}
}
