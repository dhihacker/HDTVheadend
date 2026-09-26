// Package httpts implements an HTTP-TS output: it serves a stream's live
// data to any number of concurrent HTTP clients as a raw
// "video/mp2t" byte stream.
package httpts

import (
	"net/http"

	"hdtvheadend/internal/streambus"
)

// Handler returns an http.HandlerFunc that streams bus's published chunks
// to each connecting client until the client disconnects.
func Handler(bus *streambus.Bus) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "video/mp2t")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "close")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()

		ch, unsub := bus.Subscribe(512)
		defer unsub()

		ctx := r.Context()
		for {
			select {
			case <-ctx.Done():
				return
			case chunk, ok := <-ch:
				if !ok {
					return
				}
				if _, err := w.Write(chunk); err != nil {
					return
				}
				flusher.Flush()
			}
		}
	}
}
