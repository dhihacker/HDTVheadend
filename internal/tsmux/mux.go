package tsmux

import (
	"time"

	"hdtvheadend/internal/tsutil"
)

// psiRepeatInterval is how often PAT/PMT are resent even without a
// keyframe forcing one, matching typical broadcast practice (DVB/ATSC
// commonly target 100ms; ffmpeg's own mpegts muxer defaults similarly).
// Without this, a client joining mid-GOP on a long-keyframe-interval
// encode could wait many seconds before ever seeing a PMT.
const psiRepeatInterval = 200 * time.Millisecond

// Fixed, conventional PIDs (matching what ffmpeg/VLC's own TS muxers use),
// so output from this muxer looks unsurprising to any TS analyzer.
const (
	patPID   uint16 = 0x0000
	pmtPID   uint16 = 0x1000
	VideoPID uint16 = 0x0100
	AudioPID uint16 = 0x0101

	streamTypeH264 = 0x1b
	streamTypeAAC  = 0x0f
)

// Muxer builds MPEG-TS packets from H.264 (Annex-B) video access units and
// AAC (ADTS) audio frames. Every emitted []byte is a whole number of
// 188-byte packets, ready to publish to a streambus.
type Muxer struct {
	emit func([]byte)

	patCC, pmtCC, videoCC, audioCC byte
	lastPSI                        time.Time
	psiSent                        bool
}

// NewMuxer returns a Muxer that calls emit with each batch of TS packets it
// produces. emit must not retain the slice past the call.
func NewMuxer(emit func([]byte)) *Muxer {
	return &Muxer{emit: emit}
}

// WriteVideo muxes one H.264 access unit (a slice of Annex-B NALUs, as
// returned by h264.SplitAnnexB — start codes are added here) with its
// presentation/decode timestamps (90kHz). A PAT/PMT is (re)sent on every
// keyframe and at least every psiRepeatInterval regardless, so a player
// tuning in mid-stream finds the PMT quickly even on a long GOP.
func (m *Muxer) WriteVideo(pts, dts int64, nalus [][]byte, keyframe bool) {
	if keyframe || time.Since(m.lastPSI) >= psiRepeatInterval {
		m.writePAT()
		m.writePMT()
		m.lastPSI = time.Now()
		m.psiSent = true
	}

	payload := annexBJoin(nalus)
	pesPrefix := byte(0x03) // PTS + DTS
	tsBytes := encodeTimestamp(0x03, pts)
	tsBytes = append(tsBytes, encodeTimestamp(0x01, dts)...)
	if pts == dts {
		pesPrefix = 0x02 // PTS only
		tsBytes = encodeTimestamp(0x02, pts)
	}
	pes := buildPES(0xE0, pesPrefix, tsBytes, payload, true)
	m.packetize(VideoPID, &m.videoCC, pes, true, keyframe)
}

// WriteAudio muxes one ADTS-framed AAC frame with its presentation
// timestamp (90kHz). If audio arrives before the first video frame (and so
// before WriteVideo has had a chance to send one), a PAT/PMT is sent here
// too, so an audio-only or audio-first stream isn't dropped by a demuxer
// that hasn't learned the PIDs yet.
func (m *Muxer) WriteAudio(pts int64, adtsFrame []byte) {
	if !m.psiSent {
		m.writePAT()
		m.writePMT()
		m.psiSent = true
	}
	tsBytes := encodeTimestamp(0x02, pts)
	pes := buildPES(0xC0, 0x02, tsBytes, adtsFrame, true)
	m.packetize(AudioPID, &m.audioCC, pes, false, false)
}

func annexBJoin(nalus [][]byte) []byte {
	size := 0
	for _, n := range nalus {
		size += 4 + len(n)
	}
	out := make([]byte, 0, size)
	sc := []byte{0, 0, 0, 1}
	for _, n := range nalus {
		out = append(out, sc...)
		out = append(out, n...)
	}
	return out
}

// buildPES builds a PES packet: start code + stream id + length + flags +
// timestamps + payload. dataAligned sets data_alignment_indicator, which
// should always be true here: every PES packet this muxer builds carries
// exactly one complete access unit (one ADTS frame for audio, one full set
// of NALUs for video) starting right at its boundary. Without it, ffmpeg's
// own H.264 parser (used even in -c copy / stream-probing paths, separate
// from the actual decoder) gets confused about where NALUs begin relative
// to PES boundaries and fails to activate the PPS for every single slice.
func buildPES(streamID byte, ptsDtsFlags byte, timestamps, payload []byte, dataAligned bool) []byte {
	headerDataLen := len(timestamps)
	flagsByte1 := byte(0x80) // '10' + scrambling(00) + priority(0) + copyright(0) + original(0)
	if dataAligned {
		flagsByte1 |= 0x04 // data_alignment_indicator
	}
	flagsByte2 := ptsDtsFlags << 6

	// PES_packet_length counts everything after that field: the two flags
	// bytes, header_data_length, the timestamps, and the payload. Video
	// always declares 0 (spec-legal "length unbounded"), since we don't
	// know the encoded frame size in advance when streaming live.
	pesLen := 3 + headerDataLen + len(payload)
	if streamID == 0xE0 {
		pesLen = 0
	}

	out := make([]byte, 0, 9+headerDataLen+len(payload))
	out = append(out, 0x00, 0x00, 0x01, streamID)
	out = append(out, byte(pesLen>>8), byte(pesLen))
	out = append(out, flagsByte1, flagsByte2, byte(headerDataLen))
	out = append(out, timestamps...)
	out = append(out, payload...)
	return out
}

// packetize splits a PES packet across 188-byte TS packets, inserting a
// PCR on the first packet when withPCR is true (used for the video PID,
// which is declared as the PCR_PID in the PMT). keyframe marks the first
// packet's adaptation field with random_access_indicator, the standard
// signal that a decoder/parser can (re)synchronize here.
func (m *Muxer) packetize(pid uint16, cc *byte, pes []byte, withPCR, keyframe bool) {
	first := true
	for len(pes) > 0 {
		var pcrContent []byte
		if first && withPCR {
			pcrContent = buildPCRField(extractPCRHint(pes))
		}
		if first && keyframe {
			if pcrContent == nil {
				pcrContent = []byte{0}
			}
			pcrContent[0] |= 0x40 // random_access_indicator
		}

		// headerAvail is the 184 bytes available after the 4-byte TS
		// header for adaptation-field + payload combined. minAF is the
		// adaptation field's minimum size (length byte + PCR content)
		// when one is required at all.
		const headerAvail = tsutil.PacketSize - 4
		minAF := 0
		if len(pcrContent) > 0 {
			minAF = 1 + len(pcrContent)
		}

		n := len(pes)
		if maxPayload := headerAvail - minAF; n > maxPayload {
			n = maxPayload
		}
		afTotalLen := headerAvail - n // 0 means "payload fills the packet exactly, no AF needed"

		pkt := make([]byte, tsutil.PacketSize)
		pkt[0] = tsutil.SyncByte
		pusi := byte(0)
		if first {
			pusi = 0x40
		}
		pkt[1] = pusi | byte((pid>>8)&0x1f)
		pkt[2] = byte(pid)

		offset := 4
		if afTotalLen > 0 {
			pkt[3] = 0x30 | (*cc & 0x0f) // adaptation field + payload
			af := assembleAdaptationField(pcrContent, afTotalLen)
			copy(pkt[4:], af)
			offset = 4 + afTotalLen
		} else {
			pkt[3] = 0x10 | (*cc & 0x0f) // payload only
		}
		*cc = (*cc + 1) & 0x0f

		copy(pkt[offset:], pes[:n])
		pes = pes[n:]

		m.emit(pkt)
		first = false
	}
}

// extractPCRHint recovers the PTS just encoded into pes, to reuse as a PCR
// value (base=PTS, extension=0). pes always starts with a PES header we
// just built, so the timestamp is at a fixed offset.
func extractPCRHint(pes []byte) int64 {
	// pes[7] = PES_header_data_length; timestamps start at pes[9].
	if len(pes) < 14 {
		return 0
	}
	return decodeTimestamp(pes[9:14])
}

// buildPCRField returns the 7 content bytes of an adaptation field
// carrying only a PCR: a flags byte (PCR_flag set) followed by the 6-byte
// PCR value (33-bit 90kHz base + 6 reserved bits + 9-bit 27MHz extension,
// extension always 0 here).
func buildPCRField(pcr int64) []byte {
	b := make([]byte, 7)
	b[0] = 0x10 // PCR_flag
	base := pcr & 0x1ffffffff
	b[1] = byte(base >> 25)
	b[2] = byte(base >> 17)
	b[3] = byte(base >> 9)
	b[4] = byte(base >> 1)
	b[5] = byte(base<<7) | 0x7e
	b[6] = 0x00
	return b
}

// assembleAdaptationField builds a complete adaptation field of exactly
// totalLen bytes: a length byte (totalLen-1, per spec not counting
// itself), then content, then 0xFF stuffing to fill the rest.
func assembleAdaptationField(content []byte, totalLen int) []byte {
	af := make([]byte, totalLen)
	af[0] = byte(totalLen - 1)
	copy(af[1:], content)
	for i := 1 + len(content); i < totalLen; i++ {
		af[i] = 0xff
	}
	return af
}
