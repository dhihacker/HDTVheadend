// Package epg fetches XMLTV guide sources and merges them into one master
// guide, keeping only <channel> and <programme> elements — the minimum
// Jellyfin/Plex/TiviMate need for tvg-id-matched EPG binding.
package epg

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"sync"
)

type tv struct {
	XMLName    xml.Name    `xml:"tv"`
	Channels   []channel   `xml:"channel"`
	Programmes []programme `xml:"programme"`
}

type channel struct {
	ID          string   `xml:"id,attr"`
	DisplayName []string `xml:"display-name"`
	Icon        *icon    `xml:"icon"`
}

type icon struct {
	Src string `xml:"src,attr"`
}

type programme struct {
	Channel string    `xml:"channel,attr"`
	Start   string    `xml:"start,attr"`
	Stop    string    `xml:"stop,attr"`
	Title   []rawText `xml:"title"`
	Desc    []rawText `xml:"desc"`
}

type rawText struct {
	Lang  string `xml:"lang,attr,omitempty"`
	Value string `xml:",chardata"`
}

// Store holds the last-fetched merged guide and serves it over HTTP.
type Store struct {
	mu  sync.RWMutex
	xml []byte
}

func NewStore() *Store {
	return &Store{xml: []byte(`<?xml version="1.0" encoding="UTF-8"?><tv></tv>`)}
}

// Handler serves the current merged guide as application/xml.
func (s *Store) Handler(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	w.Header().Set("Content-Type", "application/xml")
	_, _ = w.Write(s.xml)
}

// Refresh fetches every source and merges channels/programmes into one
// guide, deduplicating channels by ID.
func (s *Store) Refresh(ctx context.Context, sources []string) error {
	merged := tv{}
	seenChan := make(map[string]bool)

	for _, url := range sources {
		t, err := fetch(ctx, url)
		if err != nil {
			continue // best-effort: one bad source shouldn't blank the guide
		}
		for _, c := range t.Channels {
			if !seenChan[c.ID] {
				seenChan[c.ID] = true
				merged.Channels = append(merged.Channels, c)
			}
		}
		merged.Programmes = append(merged.Programmes, t.Programmes...)
	}

	out, err := xml.MarshalIndent(merged, "", "  ")
	if err != nil {
		return err
	}
	full := append([]byte(`<?xml version="1.0" encoding="UTF-8"?>`+"\n"), out...)

	s.mu.Lock()
	s.xml = full
	s.mu.Unlock()
	return nil
}

func fetch(ctx context.Context, url string) (*tv, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("epg: GET %s: %s", url, resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 256<<20))
	if err != nil {
		return nil, err
	}
	var t tv
	if err := xml.Unmarshal(data, &t); err != nil {
		return nil, err
	}
	return &t, nil
}
