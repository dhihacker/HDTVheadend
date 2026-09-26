package tsmux

import (
	"bytes"
	"testing"
)

func TestMuxDemuxRoundTrip(t *testing.T) {
	sps := []byte{0x67, 0x42, 0x00, 0x1e, 0xaa, 0xbb, 0xcc}
	pps := []byte{0x68, 0xce, 0x3c, 0x80}
	idr := append([]byte{0x65}, bytes.Repeat([]byte{0x11, 0x22, 0x33, 0x44}, 100)...)
	p := append([]byte{0x41}, bytes.Repeat([]byte{0x55, 0x66}, 50)...)
	adts := bytes.Repeat([]byte{0xAB, 0xCD, 0xEF, 0x01}, 60) // fake raw AAC payload

	var tsOut bytes.Buffer
	mux := NewMuxer(func(pkt []byte) { tsOut.Write(pkt) })

	// Frame 0: keyframe with SPS+PPS+IDR, PTS=DTS=0.
	mux.WriteVideo(0, 0, [][]byte{sps, pps, idr}, true)
	// Frame 1: P-frame, PTS=3600 (40ms @ 90kHz), DTS same (no B-frames here).
	mux.WriteVideo(3600, 3600, [][]byte{p}, false)

	// One audio frame with a real ADTS header wrapping our fake payload.
	adtsFrame := append(buildTestADTSHeader(len(adts)+7), adts...)
	mux.WriteAudio(1500, adtsFrame)

	// A PES is only known complete once the next one on the same PID
	// starts (streaming demux), so write one more frame of each to flush
	// the ones we actually want to inspect.
	mux.WriteVideo(7200, 7200, [][]byte{p}, false)
	mux.WriteAudio(3000, adtsFrame)

	if tsOut.Len()%188 != 0 {
		t.Fatalf("muxer output not 188-byte aligned: %d bytes", tsOut.Len())
	}

	demux := NewDemuxer()
	var got []AccessUnit
	demux.Feed(tsOut.Bytes(), func(au AccessUnit) { got = append(got, au) })

	var videoAUs, audioAUs []AccessUnit
	for _, au := range got {
		if au.Video {
			videoAUs = append(videoAUs, au)
		} else {
			audioAUs = append(audioAUs, au)
		}
	}

	if len(videoAUs) < 1 {
		t.Fatalf("expected at least 1 video access unit, got %d", len(videoAUs))
	}
	wantFrame0 := annexBJoin([][]byte{sps, pps, idr})
	if !bytes.Equal(videoAUs[0].Data, wantFrame0) {
		t.Fatalf("video frame 0 payload mismatch:\n got  %x\n want %x", videoAUs[0].Data, wantFrame0)
	}
	if videoAUs[0].PTS != 0 || videoAUs[0].DTS != 0 {
		t.Fatalf("video frame 0 timestamps: got pts=%d dts=%d, want 0,0", videoAUs[0].PTS, videoAUs[0].DTS)
	}

	if len(audioAUs) < 1 {
		t.Fatalf("expected at least 1 audio access unit, got %d", len(audioAUs))
	}
	if !bytes.Equal(audioAUs[0].Data, adtsFrame) {
		t.Fatalf("audio frame payload mismatch:\n got  %x\n want %x", audioAUs[0].Data, adtsFrame)
	}
	if audioAUs[0].PTS != 1500 {
		t.Fatalf("audio frame PTS: got %d, want 1500", audioAUs[0].PTS)
	}
}

func buildTestADTSHeader(frameLen int) []byte {
	h := make([]byte, 7)
	h[0] = 0xff
	h[1] = 0xf1
	h[2] = (1 << 6) | (4 << 2) // profile=LC(1), sampleRateIdx=4(44100)
	h[3] = byte(frameLen >> 11)
	h[4] = byte(frameLen >> 3)
	h[5] = byte(frameLen<<5) | 0x1f
	h[6] = 0xfc
	return h
}
