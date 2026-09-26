package dynhttp

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func handlerReturning(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(body))
	}
}

func get(t *testing.T, m *Mux, path string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	m.ServeHTTP(rec, req)
	return rec.Body.String()
}

func TestExactMatch(t *testing.T) {
	m := New(nil)
	m.Handle("/stream/a.ts", handlerReturning("a"))
	if got := get(t, m, "/stream/a.ts"); got != "a" {
		t.Fatalf("got %q, want %q", got, "a")
	}
}

func TestPrefixMatch(t *testing.T) {
	m := New(nil)
	m.Handle("/hls/chan/", handlerReturning("hls"))
	if got := get(t, m, "/hls/chan/playlist.m3u8"); got != "hls" {
		t.Fatalf("got %q, want %q", got, "hls")
	}
}

func TestLongestPrefixWins(t *testing.T) {
	m := New(nil)
	m.Handle("/hls/", handlerReturning("outer"))
	m.Handle("/hls/chan/", handlerReturning("inner"))
	if got := get(t, m, "/hls/chan/playlist.m3u8"); got != "inner" {
		t.Fatalf("got %q, want %q (longest prefix should win)", got, "inner")
	}
	if got := get(t, m, "/hls/other/x"); got != "outer" {
		t.Fatalf("got %q, want %q", got, "outer")
	}
}

func TestFallback(t *testing.T) {
	m := New(handlerReturning("fallback"))
	m.Handle("/stream/a.ts", handlerReturning("a"))
	if got := get(t, m, "/nope"); got != "fallback" {
		t.Fatalf("got %q, want %q", got, "fallback")
	}
}

func TestNoFallbackIs404(t *testing.T) {
	m := New(nil)
	req := httptest.NewRequest(http.MethodGet, "/nope", nil)
	rec := httptest.NewRecorder()
	m.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestReRegisteringDoesNotPanic(t *testing.T) {
	m := New(nil)
	m.Handle("/stream/a.ts", handlerReturning("first"))
	m.Handle("/stream/a.ts", handlerReturning("second")) // must not panic, unlike http.ServeMux
	if got := get(t, m, "/stream/a.ts"); got != "second" {
		t.Fatalf("got %q, want %q (re-registering should replace)", got, "second")
	}
}

func TestRemoveThenReRegister(t *testing.T) {
	m := New(handlerReturning("fallback"))
	m.Handle("/stream/a.ts", handlerReturning("first"))
	m.Remove("/stream/a.ts")
	if got := get(t, m, "/stream/a.ts"); got != "fallback" {
		t.Fatalf("got %q, want fallback after Remove", got)
	}
	// Reusing a removed path (the exact scenario that crashes a plain
	// http.ServeMux) must work cleanly.
	m.Handle("/stream/a.ts", handlerReturning("reused"))
	if got := get(t, m, "/stream/a.ts"); got != "reused" {
		t.Fatalf("got %q, want %q", got, "reused")
	}
}

func TestRemoveUnknownPatternIsSafe(t *testing.T) {
	m := New(nil)
	m.Remove("/never/registered") // must not panic
}

func TestPrefixRemove(t *testing.T) {
	m := New(handlerReturning("fallback"))
	m.Handle("/hls/chan/", handlerReturning("hls"))
	m.Remove("/hls/chan/")
	if got := get(t, m, "/hls/chan/playlist.m3u8"); got != "fallback" {
		t.Fatalf("got %q, want fallback after removing prefix route", got)
	}
}
