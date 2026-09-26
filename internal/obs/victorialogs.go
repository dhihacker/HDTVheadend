package obs

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// LogShipper is an io.Writer suitable for log.SetOutput (typically combined
// with os.Stderr via io.MultiWriter, so nothing is lost if VictoriaLogs is
// unreachable): it batches lines and ships them to a VictoriaLogs
// instance's JSON line ingestion endpoint on a timer.
type LogShipper struct {
	url string

	mu  sync.Mutex
	buf []byte
}

// NewLogShipper returns a shipper for baseURL (e.g. "http://vl:9428"), or
// nil if baseURL is empty — callers can pass a nil *LogShipper to
// io.MultiWriter-style setups; Write on a nil receiver is a safe no-op.
func NewLogShipper(baseURL string) *LogShipper {
	if baseURL == "" {
		return nil
	}
	return &LogShipper{url: strings.TrimSuffix(baseURL, "/") + "/insert/jsonline"}
}

func (s *LogShipper) Write(p []byte) (int, error) {
	if s == nil {
		return len(p), nil
	}
	text := strings.TrimRight(string(p), "\n")
	if text == "" {
		return len(p), nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	for _, line := range strings.Split(text, "\n") {
		doc, err := json.Marshal(map[string]string{
			"_time":   time.Now().UTC().Format(time.RFC3339Nano),
			"_msg":    line,
			"service": "hdtvheadend",
		})
		if err != nil {
			continue
		}
		s.buf = append(s.buf, doc...)
		s.buf = append(s.buf, '\n')
	}
	return len(p), nil
}

// Run periodically flushes buffered log lines to VictoriaLogs until ctx is
// canceled, then makes one best-effort final flush.
func (s *LogShipper) Run(ctx context.Context, interval time.Duration, onErr func(error)) {
	if s == nil {
		return
	}
	if interval <= 0 {
		interval = 5 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			s.flush(context.Background(), onErr)
			return
		case <-ticker.C:
			s.flush(ctx, onErr)
		}
	}
}

func (s *LogShipper) flush(ctx context.Context, onErr func(error)) {
	s.mu.Lock()
	if len(s.buf) == 0 {
		s.mu.Unlock()
		return
	}
	batch := s.buf
	s.buf = nil
	s.mu.Unlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url, bytes.NewReader(batch))
	if err != nil {
		if onErr != nil {
			onErr(err)
		}
		return
	}
	req.Header.Set("Content-Type", "application/stream+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		if onErr != nil {
			onErr(fmt.Errorf("push to victorialogs: %w", err))
		}
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		if onErr != nil {
			onErr(fmt.Errorf("push to victorialogs: status %s", resp.Status))
		}
	}
}
