package tsmux

import "hdtvheadend/internal/tsutil"

// writePAT emits a single-program PAT: program 1 -> pmtPID.
func (m *Muxer) writePAT() {
	section := make([]byte, 0, 13)
	section = append(section, 0x00) // table_id
	// section_length filled in below
	section = append(section, 0x00, 0x00) // placeholder for length + transport_stream_id start
	section = append(section,
		0x00, 0x01, // transport_stream_id = 1
		0xc1,       // reserved(2)=11, version=0, current_next=1
		0x00,       // section_number
		0x00,       // last_section_number
		0x00, 0x01, // program_number = 1
		byte(0xe0|byte(pmtPID>>8)), byte(pmtPID&0xff), // reserved(3)=111 + PMT PID
	)
	// section_length = bytes from after the length field through CRC.
	sectionLen := len(section) - 3 + 4 // -3 removes table_id+2 placeholder bytes, +4 for CRC
	section[1] = 0xb0 | byte(sectionLen>>8&0x0f)
	section[2] = byte(sectionLen)

	crc := crc32MPEG(section)
	section = append(section, byte(crc>>24), byte(crc>>16), byte(crc>>8), byte(crc))

	m.writePSI(patPID, &m.patCC, section)
}

// writePMT emits a PMT declaring both our fixed video(H.264) and
// audio(AAC) elementary streams unconditionally, regardless of whether a
// frame of each has actually been muxed yet. This must not depend on
// haveVideo/haveAudio: a real encoder's PMT declares its full track set
// upfront, and if we only added a track once we'd already seen a frame of
// it, a demuxer reading our output could see PMT v1 (video only) before
// audio's first frame arrives, permanently ignore the audio PID it doesn't
// know about yet, and never recover even after a later PMT declares it —
// simplest fix is to just always declare both from the very first PMT.
func (m *Muxer) writePMT() {
	section := make([]byte, 0, 32)
	section = append(section, 0x02)       // table_id
	section = append(section, 0x00, 0x00) // placeholder
	section = append(section,
		0x00, 0x01, // program_number = 1
		0xc1,                                              // reserved+version+current_next
		0x00,                                              // section_number
		0x00,                                              // last_section_number
		byte(0xe0|byte(VideoPID>>8)), byte(VideoPID&0xff), // reserved(3) + PCR_PID (video)
		0xf0, 0x00, // reserved(4) + program_info_length = 0
		streamTypeH264, byte(0xe0|byte(VideoPID>>8)), byte(VideoPID&0xff), 0xf0, 0x00,
		streamTypeAAC, byte(0xe0|byte(AudioPID>>8)), byte(AudioPID&0xff), 0xf0, 0x00,
	)

	sectionLen := len(section) - 3 + 4
	section[1] = 0xb0 | byte(sectionLen>>8&0x0f)
	section[2] = byte(sectionLen)

	crc := crc32MPEG(section)
	section = append(section, byte(crc>>24), byte(crc>>16), byte(crc>>8), byte(crc))

	m.writePSI(pmtPID, &m.pmtCC, section)
}

// writePSI wraps a PSI section (PAT or PMT) in a single TS packet: sync
// byte, PUSI set, a pointer_field of 0, the section, then 0xFF padding.
// PAT/PMT sections here are always small enough to fit in one packet.
func (m *Muxer) writePSI(pid uint16, cc *byte, section []byte) {
	pkt := make([]byte, tsutil.PacketSize)
	pkt[0] = tsutil.SyncByte
	pkt[1] = 0x40 | byte((pid>>8)&0x1f) // PUSI=1
	pkt[2] = byte(pid)
	pkt[3] = 0x10 | (*cc & 0x0f) // payload only
	*cc = (*cc + 1) & 0x0f
	pkt[4] = 0x00 // pointer_field
	copy(pkt[5:], section)
	for i := 5 + len(section); i < tsutil.PacketSize; i++ {
		pkt[i] = 0xff
	}
	m.emit(pkt)
}
