package h264

import (
	"encoding/hex"
	"testing"
)

func TestParseSPSDimensions(t *testing.T) {
	// Real SPS NALUs extracted from actual libx264 output (via ffmpeg) at
	// three different profiles and resolutions, each independently
	// confirmed correct against the source testsrc size.
	cases := []struct {
		name    string
		hexNALU string
		wantW   int
		wantH   int
	}{
		{"baseline 640x480", "6742c01ed900a03db011000003000100000300320f162e48", 640, 480},
		{"main 1280x720", "674d401feca02802dd80880000030008000003019078c18cb0", 1280, 720},
		{"high 1920x1080", "67640028acd940780227e5c044000003000400000300c83c60c658", 1920, 1080},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			nalu, err := hex.DecodeString(c.hexNALU)
			if err != nil {
				t.Fatalf("bad test fixture: %v", err)
			}
			got, err := ParseSPSDimensions(nalu)
			if err != nil {
				t.Fatalf("ParseSPSDimensions: %v", err)
			}
			if got.Width != c.wantW || got.Height != c.wantH {
				t.Errorf("got %dx%d, want %dx%d", got.Width, got.Height, c.wantW, c.wantH)
			}
		})
	}
}

func TestParseSPSDimensionsRejectsNonSPS(t *testing.T) {
	pps := []byte{0x68, 0xcb, 0x83, 0xcb, 0x20}
	if _, err := ParseSPSDimensions(pps); err == nil {
		t.Fatal("expected an error parsing a PPS NALU as an SPS")
	}
}
