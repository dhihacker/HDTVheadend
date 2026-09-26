//go:build ignore

// Throwaway generator: muxes a tiny synthetic H.264+AAC TS file so it can
// be validated against ffprobe (an independent implementation) rather than
// just our own round-trip test. Not part of the package build.
package main

import (
	"fmt"
	"os"

	"hdtvheadend/internal/tsmux"
)

func main() {
	f, err := os.Create(os.Args[1])
	if err != nil {
		panic(err)
	}
	defer f.Close()

	mux := tsmux.NewMuxer(func(pkt []byte) { f.Write(pkt) })

	sps := []byte{0x67, 0x64, 0x00, 0x1f, 0xac, 0xd9, 0x40, 0x50, 0x05, 0xbb, 0x01, 0x10, 0x00, 0x00, 0x03, 0x00, 0x10, 0x00, 0x00, 0x03, 0x03, 0xc0, 0xf1, 0x83, 0x19, 0x60}
	pps := []byte{0x68, 0xeb, 0xe3, 0xcb, 0x22, 0xc0}

	// A real IDR isn't needed for ffprobe to report stream metadata (it
	// reads SPS/PPS from the sequence params, not by fully decoding), so a
	// synthetic slice payload is enough to prove the container is valid.
	fakeSlicePayload := make([]byte, 2000)
	for i := range fakeSlicePayload {
		fakeSlicePayload[i] = byte(i)
	}

	pts := int64(0)
	for frame := 0; frame < 50; frame++ {
		keyframe := frame%25 == 0
		nalus := [][]byte{}
		if keyframe {
			nalus = append(nalus, sps, pps)
		}
		idr := append([]byte{0x65}, fakeSlicePayload...)
		if !keyframe {
			idr[0] = 0x41
		}
		nalus = append(nalus, idr)
		mux.WriteVideo(pts, pts, nalus, keyframe)

		// 2 AAC frames per video frame (~23ms each) to roughly match a
		// 44.1kHz/1024-sample cadence against 25fps video.
		for a := 0; a < 2; a++ {
			adts := buildFakeADTS()
			mux.WriteAudio(pts, adts)
		}

		pts += 3600 // 40ms @ 90kHz
	}

	fmt.Println("wrote", os.Args[1])
}

func buildFakeADTS() []byte {
	payload := make([]byte, 200)
	for i := range payload {
		payload[i] = byte(i * 7)
	}
	frameLen := 7 + len(payload)
	h := make([]byte, 7)
	h[0] = 0xff
	h[1] = 0xf1
	h[2] = (1 << 6) | (4 << 2) // AAC-LC, 44100Hz
	h[3] = byte(frameLen >> 11)
	h[4] = byte(frameLen >> 3)
	h[5] = byte(frameLen<<5) | 0x1f
	h[6] = 0xfc
	return append(h, payload...)
}
