// Package hlsdash segments a live MPEG-TS stream into a rolling window of
// in-memory segments and serves them as either an HLS (.m3u8) or DASH
// (.mpd) live stream. Segments are cut on a fixed wall-clock interval
// rather than aligned to keyframes/PAT-PMT boundaries — simple and
// reliable, at the cost of an occasional decoder stutter right at a
// segment boundary. Each segment is a self-contained slice of the raw TS
// (which repeats PAT/PMT periodically), so both HLS and DASH players can
// start decoding from any segment without a separate init segment.
package hlsdash

import (
	"context"
	"fmt"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"hdtvheadend/internal/streambus"
)

type segment struct {
	seq      int
	data     []byte
	duration float64
}

// Segmenter cuts one stream's data into a rolling window of segments and
// serves HLS/DASH manifests plus the segments themselves.
type Segmenter struct {
	mu        sync.Mutex
	segments  []segment
	nextSeq   int
	startTime time.Time // wall-clock time segment 0 started; DASH clients
	// compute the expected live segment number from (now-startTime)/duration,
	// so this must be real, not a fixed epoch.
}

func NewSegmenter() *Segmenter { return &Segmenter{} }

// Run subscribes to bus and cuts a new segment every segDur until ctx is
// canceled, keeping the most recent count segments.
func (s *Segmenter) Run(ctx context.Context, bus *streambus.Bus, segDur time.Duration, count int) error {
	if count <= 0 {
		count = 6
	}
	if segDur <= 0 {
		segDur = 4 * time.Second
	}

	ch, unsub := bus.Subscribe(1024)
	defer unsub()

	var buf []byte
	start := time.Now()
	s.mu.Lock()
	s.startTime = start
	s.mu.Unlock()
	ticker := time.NewTicker(segDur)
	defer ticker.Stop()

	cut := func() {
		if len(buf) == 0 {
			return
		}
		s.mu.Lock()
		s.segments = append(s.segments, segment{seq: s.nextSeq, data: buf, duration: time.Since(start).Seconds()})
		s.nextSeq++
		if len(s.segments) > count {
			s.segments = s.segments[len(s.segments)-count:]
		}
		s.mu.Unlock()
		buf = nil
		start = time.Now()
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			cut()
		case chunk, ok := <-ch:
			if !ok {
				return nil
			}
			buf = append(buf, chunk...)
		}
	}
}

// Handler serves, under one path prefix: playlist.m3u8 (HLS media
// playlist), manifest.mpd (DASH manifest), and seg-<N>.ts (the segments
// referenced by either manifest).
func (s *Segmenter) Handler(targetDurSeconds int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch base := path.Base(r.URL.Path); {
		case base == "playlist.m3u8":
			s.servePlaylist(w, targetDurSeconds)
		case base == "manifest.mpd":
			s.serveMPD(w, targetDurSeconds)
		case strings.HasPrefix(base, "seg-") && strings.HasSuffix(base, ".ts"):
			s.serveSegment(w, base)
		default:
			http.NotFound(w, r)
		}
	}
}

func (s *Segmenter) servePlaylist(w http.ResponseWriter, targetDur int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var b strings.Builder
	b.WriteString("#EXTM3U\n#EXT-X-VERSION:3\n")
	fmt.Fprintf(&b, "#EXT-X-TARGETDURATION:%d\n", targetDur)
	if len(s.segments) > 0 {
		fmt.Fprintf(&b, "#EXT-X-MEDIA-SEQUENCE:%d\n", s.segments[0].seq)
	}
	for _, seg := range s.segments {
		fmt.Fprintf(&b, "#EXTINF:%.3f,\nseg-%d.ts\n", seg.duration, seg.seq)
	}

	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write([]byte(b.String()))
}

func (s *Segmenter) serveMPD(w http.ResponseWriter, segDurSeconds int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Rather than a bare SegmentTemplate (which makes the client compute
	// "the current segment number" itself from availabilityStartTime and
	// wall-clock time — fragile in practice, since a client's clock math
	// can disagree with the server by orders of magnitude if it assumes a
	// different implicit timescale), list an explicit SegmentTimeline: the
	// client is told exactly which segments exist right now, no clock math
	// involved.
	startNum := 0
	if len(s.segments) > 0 {
		startNum = s.segments[0].seq
	}
	bufferDepth := segDurSeconds * len(s.segments)
	if bufferDepth <= 0 {
		bufferDepth = segDurSeconds
	}
	ast := s.startTime.UTC().Format("2006-01-02T15:04:05.000Z")

	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	fmt.Fprintf(&b, `<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" profiles="urn:mpeg:dash:profile:isoff-live:2011" type="dynamic" minimumUpdatePeriod="PT%dS" availabilityStartTime="%s" timeShiftBufferDepth="PT%dS">`+"\n", segDurSeconds, ast, bufferDepth)
	b.WriteString("  <Period start=\"PT0S\">\n")
	b.WriteString("    <AdaptationSet mimeType=\"video/mp2t\" segmentAlignment=\"true\">\n")
	fmt.Fprintf(&b, `      <SegmentTemplate media="seg-$Number$.ts" startNumber="%d" timescale="1">`+"\n", startNum)
	b.WriteString("        <SegmentTimeline>\n")
	if len(s.segments) > 0 {
		fmt.Fprintf(&b, `          <S t="0" d="%d" r="%d"/>`+"\n", segDurSeconds, len(s.segments)-1)
	}
	b.WriteString("        </SegmentTimeline>\n")
	b.WriteString("      </SegmentTemplate>\n")
	b.WriteString("      <Representation id=\"0\" bandwidth=\"5000000\"/>\n")
	b.WriteString("    </AdaptationSet>\n  </Period>\n</MPD>\n")

	w.Header().Set("Content-Type", "application/dash+xml")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write([]byte(b.String()))
}

func (s *Segmenter) serveSegment(w http.ResponseWriter, name string) {
	numStr := strings.TrimSuffix(strings.TrimPrefix(name, "seg-"), ".ts")
	n, err := strconv.Atoi(numStr)
	if err != nil {
		http.NotFound(w, nil)
		return
	}

	s.mu.Lock()
	var data []byte
	for _, seg := range s.segments {
		if seg.seq == n {
			data = seg.data
			break
		}
	}
	s.mu.Unlock()

	if data == nil {
		http.Error(w, "segment expired or not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "video/mp2t")
	_, _ = w.Write(data)
}
