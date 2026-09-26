// Package webui implements HDTVheadend's admin dashboard: session-cookie
// login, an IP allow-list, and a JSON API plus static HTML/JS for managing
// streams.
package webui

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// sessionInfo describes one logged-in session for display on the Sessions
// page. ID is an opaque display identifier, unrelated to the cookie value,
// so the API can list and revoke sessions without ever exposing a live
// cookie token.
type sessionInfo struct {
	id        string
	ip        string
	createdAt time.Time
	expiresAt time.Time
}

// sessions is a tiny in-memory session store (cookie token -> info). Nova
// Core is a single-process headend; sessions don't need to survive a
// restart.
type sessions struct {
	mu   sync.Mutex
	toks map[string]*sessionInfo
}

func newSessions() *sessions { return &sessions{toks: make(map[string]*sessionInfo)} }

func (s *sessions) create(ip string) string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	tok := hex.EncodeToString(b)

	idB := make([]byte, 8)
	_, _ = rand.Read(idB)

	now := time.Now()
	s.mu.Lock()
	s.toks[tok] = &sessionInfo{
		id:        hex.EncodeToString(idB),
		ip:        ip,
		createdAt: now,
		expiresAt: now.Add(24 * time.Hour),
	}
	s.mu.Unlock()
	return tok
}

func (s *sessions) valid(tok string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	info, ok := s.toks[tok]
	if !ok || time.Now().After(info.expiresAt) {
		delete(s.toks, tok)
		return false
	}
	return true
}

func (s *sessions) revoke(tok string) {
	s.mu.Lock()
	delete(s.toks, tok)
	s.mu.Unlock()
}

// list returns all live sessions, newest first.
func (s *sessions) list() []sessionInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	out := make([]sessionInfo, 0, len(s.toks))
	for tok, info := range s.toks {
		if now.After(info.expiresAt) {
			delete(s.toks, tok)
			continue
		}
		out = append(out, *info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].createdAt.After(out[j].createdAt) })
	return out
}

// revokeByID revokes the session with the given display id, returning
// whether one was found.
func (s *sessions) revokeByID(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for tok, info := range s.toks {
		if info.id == id {
			delete(s.toks, tok)
			return true
		}
	}
	return false
}

const cookieName = "nova_session"

// HashPassword bcrypt-hashes a plaintext admin password for storage in
// config.Admin.PasswordHash.
func HashPassword(plain string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(plain), bcrypt.DefaultCost)
	return string(b), err
}

func checkPassword(hash, plain string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(plain)) == nil
}

// ipAllowed reports whether remoteAddr's IP is in allowlist. An empty
// allowlist means "allow all" (used for first-run setups; operators should
// configure this promptly).
func ipAllowed(remoteAddr string, allowlist []string) bool {
	if len(allowlist) == 0 {
		return true
	}
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	for _, entry := range allowlist {
		if strings.Contains(entry, "/") {
			_, cidr, err := net.ParseCIDR(entry)
			if err == nil && cidr.Contains(ip) {
				return true
			}
			continue
		}
		if net.ParseIP(entry).Equal(ip) {
			return true
		}
	}
	return false
}

// loginRateLimiter blocks brute-force admin login attempts with a simple
// per-IP exponential backoff.
type loginRateLimiter struct {
	mu    sync.Mutex
	next  map[string]time.Time
	fails map[string]int
}

func newLoginRateLimiter() *loginRateLimiter {
	return &loginRateLimiter{next: make(map[string]time.Time), fails: make(map[string]int)}
}

func (l *loginRateLimiter) allowed(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return time.Now().After(l.next[ip])
}

func (l *loginRateLimiter) recordFailure(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.fails[ip]++
	backoff := time.Duration(l.fails[ip]) * 2 * time.Second
	if backoff > 5*time.Minute {
		backoff = 5 * time.Minute
	}
	l.next[ip] = time.Now().Add(backoff)
}

func (l *loginRateLimiter) recordSuccess(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.fails, ip)
	delete(l.next, ip)
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func constantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
