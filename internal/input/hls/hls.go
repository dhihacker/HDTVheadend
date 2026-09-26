// Package hls implements an HLS (m3u8) input for MPEG-TS-segmented
// playlists: it follows a master playlist to a media playlist (or accepts
// a media playlist directly), downloads new segments as they appear, and
// republishes their TS payload to a streambus.
//
// Segments that are not MPEG-TS (e.g. fMP4/CMAF, used by many modern
// HLS/DASH-shared streams) are not remuxed — they are not TS-aligned, so
// they are skipped for TS-only outputs. Use the DASH input's raw fMP4
// passthrough for those, served over HTTP progressively.
package hls

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"hdtvheadend/internal/streambus"
	"hdtvheadend/internal/tsutil"
)

func Run(ctx context.Context, playlistURL string, bus *streambus.Bus) error {
	mediaURL, err := resolveMediaPlaylist(ctx, playlistURL)
	if err != nil {
		return fmt.Errorf("hls: %w", err)
	}

	seen := make(map[string]bool)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		pl, err := fetchPlaylist(ctx, mediaURL)
		if err != nil {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(3 * time.Second):
			}
			continue
		}

		for _, seg := range pl.segments {
			if seen[seg] {
				continue
			}
			seen[seg] = true
			if err := fetchSegment(ctx, seg, bus); err != nil && ctx.Err() != nil {
				return ctx.Err()
			}
		}

		if pl.endlist {
			return nil
		}

		wait := time.Duration(pl.targetDuration) * time.Second
		if wait <= 0 {
			wait = 4 * time.Second
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
}

type playlist struct {
	segments       []string
	targetDuration int
	endlist        bool
}

func resolveMediaPlaylist(ctx context.Context, playlistURL string) (string, error) {
	body, err := get(ctx, playlistURL)
	if err != nil {
		return "", err
	}
	defer body.Close()

	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	var variant string
	expectVariant := false
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "#EXT-X-STREAM-INF") {
			expectVariant = true
			continue
		}
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if expectVariant {
			variant = line // the URI line that always immediately follows EXT-X-STREAM-INF
		}
		// Whether or not expectVariant was set, the first non-comment line
		// settles it: either it's the variant URI (master playlist) or it's
		// a segment URI (already a media playlist, nothing to follow).
		break
	}
	if variant == "" {
		return playlistURL, nil // already a media playlist
	}
	return resolveURL(playlistURL, variant)
}

func fetchPlaylist(ctx context.Context, playlistURL string) (*playlist, error) {
	body, err := get(ctx, playlistURL)
	if err != nil {
		return nil, err
	}
	defer body.Close()

	pl := &playlist{}
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case strings.HasPrefix(line, "#EXT-X-TARGETDURATION:"):
			d, _ := strconv.Atoi(strings.TrimPrefix(line, "#EXT-X-TARGETDURATION:"))
			pl.targetDuration = d
		case line == "#EXT-X-ENDLIST":
			pl.endlist = true
		case line != "" && !strings.HasPrefix(line, "#"):
			segURL, err := resolveURL(playlistURL, line)
			if err == nil {
				pl.segments = append(pl.segments, segURL)
			}
		}
	}
	return pl, sc.Err()
}

func fetchSegment(ctx context.Context, segURL string, bus *streambus.Bus) error {
	body, err := get(ctx, segURL)
	if err != nil {
		return err
	}
	defer body.Close()

	data, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	if len(data) < tsutil.PacketSize || data[0] != tsutil.SyncByte {
		return nil // not MPEG-TS; skip (see package doc)
	}
	aligned := (len(data) / tsutil.PacketSize) * tsutil.PacketSize
	bus.Publish(data[:aligned])
	return nil
}

func get(ctx context.Context, u string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("GET %s: %s", u, resp.Status)
	}
	return resp.Body, nil
}

func resolveURL(base, ref string) (string, error) {
	b, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	r, err := url.Parse(ref)
	if err != nil {
		return "", err
	}
	return b.ResolveReference(r).String(), nil
}
