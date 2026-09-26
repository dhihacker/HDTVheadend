package tsmux

import "hdtvheadend/internal/tsutil"

// AccessUnit is one demuxed elementary-stream access unit: a full H.264
// Annex-B video frame (start codes included) or one ADTS-framed AAC audio
// frame, with its 90kHz timestamps.
type AccessUnit struct {
	Video bool
	PTS   int64
	DTS   int64 // equals PTS for audio
	Data  []byte
}

type pesAssembly struct {
	buf     []byte
	started bool
}

// Demuxer extracts H.264 video and AAC audio access units from a raw TS
// byte stream. It follows PAT -> PMT to find the video/audio PIDs
// automatically; other stream types in the PMT are ignored.
type Demuxer struct {
	pmtPID             uint16
	videoPID, audioPID uint16
	assemblies         map[uint16]*pesAssembly
}

func NewDemuxer() *Demuxer {
	return &Demuxer{assemblies: make(map[uint16]*pesAssembly)}
}

// Feed processes a run of 188-byte-aligned TS packets, calling onAU for
// each completed access unit found. data need not align to any particular
// PES boundary; partial PES data is buffered across calls.
func (d *Demuxer) Feed(data []byte, onAU func(AccessUnit)) {
	for i := 0; i+tsutil.PacketSize <= len(data); i += tsutil.PacketSize {
		pkt := data[i : i+tsutil.PacketSize]
		if pkt[0] != tsutil.SyncByte {
			continue
		}
		pid := tsutil.PID(pkt)
		pusi := pkt[1]&0x40 != 0
		off := tsutil.PayloadOffset(pkt)
		if off >= len(pkt) {
			continue
		}
		payload := pkt[off:]

		switch {
		case pid == 0x0000:
			d.parsePAT(payload, pusi)
		case d.pmtPID != 0 && pid == d.pmtPID:
			d.parsePMT(payload, pusi)
		case pid == d.videoPID || pid == d.audioPID:
			d.feedPES(pid, payload, pusi, onAU)
		}
	}
}

func (d *Demuxer) parsePAT(payload []byte, pusi bool) {
	if !pusi || len(payload) < 1 {
		return
	}
	ptr := int(payload[0])
	if 1+ptr >= len(payload) {
		return
	}
	section := payload[1+ptr:]
	if len(section) < 8 {
		return
	}
	sectionLength := int(section[1]&0x0f)<<8 | int(section[2])
	if len(section) < 3+sectionLength || sectionLength < 9 {
		return
	}
	end := 3 + sectionLength - 4 // exclude CRC
	for i := 8; i+4 <= end; i += 4 {
		programNumber := int(section[i])<<8 | int(section[i+1])
		pid := uint16(section[i+2]&0x1f)<<8 | uint16(section[i+3])
		if programNumber != 0 {
			d.pmtPID = pid
			return
		}
	}
}

func (d *Demuxer) parsePMT(payload []byte, pusi bool) {
	if !pusi || len(payload) < 1 {
		return
	}
	ptr := int(payload[0])
	if 1+ptr >= len(payload) {
		return
	}
	section := payload[1+ptr:]
	if len(section) < 12 {
		return
	}
	sectionLength := int(section[1]&0x0f)<<8 | int(section[2])
	if len(section) < 3+sectionLength {
		return
	}
	programInfoLength := int(section[10]&0x0f)<<8 | int(section[11])
	end := 3 + sectionLength - 4
	for i := 12 + programInfoLength; i+5 <= end; {
		streamType := section[i]
		pid := uint16(section[i+1]&0x1f)<<8 | uint16(section[i+2])
		esInfoLength := int(section[i+3]&0x0f)<<8 | int(section[i+4])
		switch streamType {
		case streamTypeH264:
			d.videoPID = pid
		case streamTypeAAC:
			d.audioPID = pid
		}
		i += 5 + esInfoLength
	}
}

func (d *Demuxer) feedPES(pid uint16, payload []byte, pusi bool, onAU func(AccessUnit)) {
	asm := d.assemblies[pid]
	if asm == nil {
		asm = &pesAssembly{}
		d.assemblies[pid] = asm
	}
	if pusi {
		if asm.started {
			d.emitAU(pid, asm, onAU)
		}
		asm.buf = append([]byte(nil), payload...)
		asm.started = true
		return
	}
	if !asm.started {
		return // haven't seen this PID's first PES start yet; drop stray continuation
	}
	asm.buf = append(asm.buf, payload...)
}

func (d *Demuxer) emitAU(pid uint16, asm *pesAssembly, onAU func(AccessUnit)) {
	buf := asm.buf
	asm.buf = nil
	asm.started = false

	if len(buf) < 9 || buf[0] != 0 || buf[1] != 0 || buf[2] != 1 {
		return
	}
	ptsDTSFlags := buf[7] >> 6
	headerDataLen := int(buf[8])
	if 9+headerDataLen > len(buf) {
		return
	}
	optional := buf[9 : 9+headerDataLen]
	payload := buf[9+headerDataLen:]

	var pts, dts int64 = -1, -1
	switch ptsDTSFlags {
	case 0x2:
		if len(optional) >= 5 {
			pts = decodeTimestamp(optional[0:5])
			dts = pts
		}
	case 0x3:
		if len(optional) >= 10 {
			pts = decodeTimestamp(optional[0:5])
			dts = decodeTimestamp(optional[5:10])
		}
	}
	if pts < 0 || len(payload) == 0 {
		return
	}
	onAU(AccessUnit{Video: pid == d.videoPID, PTS: pts, DTS: dts, Data: payload})
}
