// Package dynhttp provides an HTTP router whose routes can be registered
// and removed at any time while the server is running.
//
// net/http's own ServeMux can't do this: registering the same pattern
// twice panics the connection that triggered it, and there is no API to
// unregister a pattern at all once added. That's fine for a fixed set of
// routes set up once at startup, but stream inputs/outputs register their
// own paths dynamically — added, edited, or removed for as long as the
// process runs — and a path can legitimately be reused (editing a stream
// back to an earlier path, or recreating one that happens to pick the
// same default path again). Mux exists specifically for that dynamic set
// of routes; anything registered once at startup and never touched again
// can and should still just use a plain http.ServeMux.
package dynhttp

import (
	"net/http"
	"strings"
	"sync"
)

// Mux is an http.Handler whose exact-match and trailing-slash-prefix
// routes (mirroring net/http.ServeMux's own two pattern styles) can be
// added and removed freely. Requests matching no registered route fall
// through to fallback, if set.
type Mux struct {
	mu       sync.RWMutex
	exact    map[string]http.Handler
	prefixes map[string]http.Handler
	fallback http.Handler
}

// New returns a Mux that serves fallback for any request matching no
// dynamically-registered route. fallback may be nil (unmatched requests
// then get a plain 404).
func New(fallback http.Handler) *Mux {
	return &Mux{
		exact:    make(map[string]http.Handler),
		prefixes: make(map[string]http.Handler),
		fallback: fallback,
	}
}

// Handle registers handler for pattern. A pattern ending in "/" matches
// by longest-prefix, like a ServeMux subtree pattern; any other pattern
// matches only that exact path. Registering a pattern that's already
// registered replaces it (no panic, unlike http.ServeMux).
func (m *Mux) Handle(pattern string, handler http.Handler) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if strings.HasSuffix(pattern, "/") {
		m.prefixes[pattern] = handler
	} else {
		m.exact[pattern] = handler
	}
}

// HandleFunc is Handle for a plain handler function.
func (m *Mux) HandleFunc(pattern string, handler http.HandlerFunc) {
	m.Handle(pattern, handler)
}

// Remove unregisters pattern. Safe to call for a pattern that was never
// registered, or already removed.
func (m *Mux) Remove(pattern string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.exact, pattern)
	delete(m.prefixes, pattern)
}

func (m *Mux) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.mu.RLock()
	h := m.lookupLocked(r.URL.Path)
	fallback := m.fallback
	m.mu.RUnlock()

	if h != nil {
		h.ServeHTTP(w, r)
		return
	}
	if fallback != nil {
		fallback.ServeHTTP(w, r)
		return
	}
	http.NotFound(w, r)
}

func (m *Mux) lookupLocked(path string) http.Handler {
	if h, ok := m.exact[path]; ok {
		return h
	}
	var bestLen int
	var best http.Handler
	for prefix, h := range m.prefixes {
		if len(prefix) > bestLen && strings.HasPrefix(path, prefix) {
			bestLen, best = len(prefix), h
		}
	}
	return best
}
