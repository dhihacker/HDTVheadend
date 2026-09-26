// Package dash implements a basic MPEG-DASH (MPD) input. It follows a
// manifest's SegmentTemplate to the highest-bitrate representation and
// republishes the concatenated fMP4 (init + media segments) to a
// streambus, byte for byte, with no remuxing.
//
// fMP4/CMAF is not MPEG-TS: this input is meant to feed an HTTP output
// that serves the stream as progressive fMP4, not the UDP/HTTP-TS
// outputs, which expect 188-byte-aligned TS packets.
package dash

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"hdtvheadend/internal/streambus"
)

type mpd struct {
	Type          string   `xml:"type,attr"`
	MinimumUpdate string   `xml:"minimumUpdatePeriod,attr"`
	Periods       []period `xml:"Period"`
	baseURL       string   // resolved manifest URL, for relative refs
}

type period struct {
	AdaptationSets []adaptationSet `xml:"AdaptationSet"`
}

type adaptationSet struct {
	SegmentTemplate *segmentTemplate `xml:"SegmentTemplate"`
	Representations []representation `xml:"Representation"`
}

type representation struct {
	ID              string           `xml:"id,attr"`
	Bandwidth       int              `xml:"bandwidth,attr"`
	SegmentTemplate *segmentTemplate `xml:"SegmentTemplate"`
}

type segmentTemplate struct {
	Media          string           `xml:"media,attr"`
	Initialization string           `xml:"initialization,attr"`
	StartNumber    int64            `xml:"startNumber,attr"`
	Duration       int64            `xml:"duration,attr"`
	Timescale      int64            `xml:"timescale,attr"`
	Timeline       *segmentTimeline `xml:"SegmentTimeline"`
}

type segmentTimeline struct {
	S []segTimelineEntry `xml:"S"`
}

type segTimelineEntry struct {
	T int64 `xml:"t,attr"`
	D int64 `xml:"d,attr"`
	R int64 `xml:"r,attr"`
}

func Run(ctx context.Context, mpdURL string, bus *streambus.Bus) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		m, err := fetchMPD(ctx, mpdURL)
		if err != nil {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(3 * time.Second):
			}
			continue
		}

		rep, tmpl, err := pickRepresentation(m)
		if err != nil {
			return fmt.Errorf("dash: %w", err)
		}

		if err := playRepresentation(ctx, m, rep, tmpl, bus); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
		}

		if m.Type != "dynamic" {
			return nil // static (VOD) manifest fully played
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(refreshInterval(m)):
		}
	}
}

func refreshInterval(m *mpd) time.Duration {
	d, err := parseISODuration(m.MinimumUpdate)
	if err != nil || d <= 0 {
		return 5 * time.Second
	}
	return d
}

func pickRepresentation(m *mpd) (representation, *segmentTemplate, error) {
	var best representation
	var bestTmpl *segmentTemplate
	found := false

	for _, p := range m.Periods {
		for _, as := range p.AdaptationSets {
			for _, r := range as.Representations {
				if !found || r.Bandwidth > best.Bandwidth {
					best = r
					found = true
					if r.SegmentTemplate != nil {
						bestTmpl = r.SegmentTemplate
					} else {
						bestTmpl = as.SegmentTemplate
					}
				}
			}
		}
	}
	if !found {
		return representation{}, nil, fmt.Errorf("no Representation with SegmentTemplate found")
	}
	if bestTmpl == nil {
		return representation{}, nil, fmt.Errorf("representation %s has no SegmentTemplate (not supported)", best.ID)
	}
	return best, bestTmpl, nil
}

func playRepresentation(ctx context.Context, m *mpd, rep representation, tmpl *segmentTemplate, bus *streambus.Bus) error {
	if tmpl.Initialization != "" {
		initURL := resolve(m.baseURL, expand(tmpl.Initialization, rep, 0, 0))
		if err := fetchAndPublish(ctx, initURL, bus); err != nil {
			return err
		}
	}

	if tmpl.Timeline != nil {
		return playTimeline(ctx, m, rep, tmpl, bus)
	}
	return playNumbered(ctx, m, rep, tmpl, bus)
}

func playNumbered(ctx context.Context, m *mpd, rep representation, tmpl *segmentTemplate, bus *streambus.Bus) error {
	start := tmpl.StartNumber
	if start == 0 {
		start = 1
	}
	n := start
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		segURL := resolve(m.baseURL, expand(tmpl.Media, rep, n, 0))
		if err := fetchAndPublish(ctx, segURL, bus); err != nil {
			return err // segment 404 → representation exhausted (VOD) or not yet available (live)
		}
		n++
		if m.Type != "dynamic" {
			continue // VOD: keep going until fetch fails
		}
		return nil // live: one segment per manifest refresh is enough for this basic client
	}
}

func playTimeline(ctx context.Context, m *mpd, rep representation, tmpl *segmentTemplate, bus *streambus.Bus) error {
	t := int64(0)
	for _, s := range tmpl.Timeline.S {
		if s.T != 0 {
			t = s.T
		}
		repeats := s.R
		if repeats < 0 {
			repeats = 0
		}
		for i := int64(0); i <= repeats; i++ {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}
			segURL := resolve(m.baseURL, expand(tmpl.Media, rep, 0, t))
			if err := fetchAndPublish(ctx, segURL, bus); err != nil {
				return err
			}
			t += s.D
		}
	}
	return nil
}

func expand(template string, rep representation, number, timeVal int64) string {
	out := template
	out = strings.ReplaceAll(out, "$RepresentationID$", rep.ID)
	out = strings.ReplaceAll(out, "$Bandwidth$", strconv.Itoa(rep.Bandwidth))
	if number != 0 {
		out = strings.ReplaceAll(out, "$Number$", strconv.FormatInt(number, 10))
	}
	if timeVal != 0 {
		out = strings.ReplaceAll(out, "$Time$", strconv.FormatInt(timeVal, 10))
	}
	return out
}

func fetchMPD(ctx context.Context, mpdURL string) (*mpd, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, mpdURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", mpdURL, resp.Status)
	}
	var m mpd
	if err := xml.NewDecoder(resp.Body).Decode(&m); err != nil {
		return nil, err
	}
	m.baseURL = mpdURL
	return &m, nil
}

func fetchAndPublish(ctx context.Context, segURL string, bus *streambus.Bus) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, segURL, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", segURL, resp.Status)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	bus.Publish(data)
	return nil
}

func resolve(base, ref string) string {
	b, err := url.Parse(base)
	if err != nil {
		return ref
	}
	r, err := url.Parse(ref)
	if err != nil {
		return ref
	}
	return b.ResolveReference(r).String()
}

// parseISODuration parses a small subset of ISO-8601 durations (PnYnMnDTnHnMnS)
// sufficient for MPD minimumUpdatePeriod values like "PT5S" or "PT2M".
func parseISODuration(s string) (time.Duration, error) {
	if !strings.HasPrefix(s, "P") {
		return 0, fmt.Errorf("not an ISO-8601 duration: %q", s)
	}
	s = s[1:]
	var total time.Duration
	inTime := false
	num := ""
	for _, c := range s {
		switch {
		case c == 'T':
			inTime = true
		case c >= '0' && c <= '9' || c == '.':
			num += string(c)
		default:
			v, _ := strconv.ParseFloat(num, 64)
			num = ""
			switch c {
			case 'H':
				total += time.Duration(v * float64(time.Hour))
			case 'M':
				if inTime {
					total += time.Duration(v * float64(time.Minute))
				}
			case 'S':
				total += time.Duration(v * float64(time.Second))
			}
		}
	}
	return total, nil
}
