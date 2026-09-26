package webui

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"hdtvheadend/internal/config"
	"hdtvheadend/internal/logbuf"
)

//go:embed static/*
var staticFS embed.FS

// StreamStat is a running (or stopped) stream's live counters, as reported
// by the Store. Clients/InputBps/OutputBps are zero for a stream that isn't
// currently running.
type StreamStat struct {
	ID           string  `json:"id"`
	Running      bool    `json:"running"`      // handle exists (outputs are registered/listening)
	OnDemand     bool    `json:"on_demand"`    // stream is configured for on-demand input start/stop
	InputActive  bool    `json:"input_active"` // the input is currently pulling/receiving data right now
	Clients      int     `json:"clients"`
	InputBps     float64 `json:"input_bps"`              // rate arriving from the source
	OutputBps    float64 `json:"output_bps"`             // aggregate rate served across all current output connections
	ActiveSource int     `json:"active_source"`          // index into [input]+backup_inputs
	SourceCount  int     `json:"source_count"`           // 1 if no backups configured
	SourcePinned bool    `json:"source_pinned"`          // true if manually switched, not automatic
	SourceError  string  `json:"source_error,omitempty"` // last error from a failed source, if any
}

// Store is the interface the web UI needs from whatever holds live config
// and stream state; main.go's Server implements it.
type Store interface {
	Config() *config.Config
	SaveConfig() error
	ApplyStreams() error // restart running streams to match the current config
	StreamStats() []StreamStat
	SwitchSource(streamID string, index int) error // index < 0 returns to automatic failover
	LogLines(since int64) []logbuf.Line            // 0 = everything currently buffered
	LastLogSeq() int64
}

// Server holds the web UI's own state (sessions, rate limiter) plus a
// reference to the app Store it manages.
type Server struct {
	store     Store
	sess      *sessions
	limiter   *loginRateLimiter
	startedAt time.Time
}

func New(store Store) *Server {
	return &Server{store: store, sess: newSessions(), limiter: newLoginRateLimiter(), startedAt: time.Now()}
}

// Register attaches the web UI and JSON API routes to mux.
func (s *Server) Register(mux *http.ServeMux) {
	static, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(err) // embed.FS is compiled in; this can't fail at runtime
	}
	mux.Handle("/", s.ipGateHandler(http.FileServer(http.FS(static))))

	mux.HandleFunc("/api/login", s.ipGate(s.handleLogin))
	mux.HandleFunc("/api/logout", s.ipGate(s.requireAuth(s.handleLogout)))
	mux.HandleFunc("/api/streams", s.ipGate(s.requireAuth(s.handleStreams)))
	mux.HandleFunc("/api/streams/", s.ipGate(s.requireAuth(s.handleStreamByID)))
	mux.HandleFunc("/api/settings", s.ipGate(s.requireAuth(s.handleSettings)))
	mux.HandleFunc("/api/status", s.ipGate(s.requireAuth(s.handleStatus)))
	mux.HandleFunc("/api/events", s.ipGate(s.requireAuth(s.handleEvents)))
	mux.HandleFunc("/api/logs", s.ipGate(s.requireAuth(s.handleLogs)))
	mux.HandleFunc("/api/logs/events", s.ipGate(s.requireAuth(s.handleLogEvents)))
	mux.HandleFunc("/api/network-interfaces", s.ipGate(s.requireAuth(s.handleNetworkInterfaces)))
}

func (s *Server) ipGate(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !ipAllowed(r.RemoteAddr, s.store.Config().AllowedIPs) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

func (s *Server) ipGateHandler(next http.Handler) http.HandlerFunc {
	return s.ipGate(next.ServeHTTP)
}

func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(cookieName)
		if err != nil || !s.sess.valid(c.Value) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ip := clientIP(r)
	if !s.limiter.allowed(ip) {
		http.Error(w, "too many attempts, slow down", http.StatusTooManyRequests)
		return
	}

	var req struct{ Username, Password string }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	admin := s.store.Config().Admin
	ok := constantTimeEqual(req.Username, admin.Username) && checkPassword(admin.PasswordHash, req.Password)
	if !ok {
		s.limiter.recordFailure(ip)
		http.Error(w, "invalid credentials", http.StatusUnauthorized)
		return
	}
	s.limiter.recordSuccess(ip)

	tok := s.sess.create(ip)
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    tok,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int((24 * 60 * 60)),
	})
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(cookieName); err == nil {
		s.sess.revoke(c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1})
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleStreams(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, s.store.Config().Streams)
	case http.MethodPost:
		var st config.Stream
		if err := json.NewDecoder(r.Body).Decode(&st); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		c := s.store.Config()
		c.Streams = append(c.Streams, st)
		if err := s.saveAndApply(w); err != nil {
			return
		}
		writeJSON(w, st)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleStreamByID(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Path[len("/api/streams/"):]

	if strings.HasSuffix(id, "/switch-source") {
		s.handleSwitchSource(w, r, strings.TrimSuffix(id, "/switch-source"))
		return
	}

	c := s.store.Config()

	switch r.Method {
	case http.MethodDelete:
		out := c.Streams[:0]
		for _, st := range c.Streams {
			if st.ID != id {
				out = append(out, st)
			}
		}
		c.Streams = out
		if err := s.saveAndApply(w); err != nil {
			return
		}
		w.WriteHeader(http.StatusOK)
	case http.MethodPut:
		var updated config.Stream
		if err := json.NewDecoder(r.Body).Decode(&updated); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		for i, st := range c.Streams {
			if st.ID == id {
				c.Streams[i] = updated
			}
		}
		if err := s.saveAndApply(w); err != nil {
			return
		}
		writeJSON(w, updated)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

type switchSourceRequest struct {
	Index int `json:"index"` // 0 = primary, 1 = first backup, ...; negative = back to automatic
}

func (s *Server) handleSwitchSource(w http.ResponseWriter, r *http.Request, streamID string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req switchSourceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := s.store.SwitchSource(streamID, req.Index); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusOK)
}

type settingsView struct {
	AdminUsername      string   `json:"admin_username"`
	ListenAddr         string   `json:"listen_addr"`
	AllowedIPs         []string `json:"allowed_ips"`
	EPGSources         []string `json:"epg_sources"`
	VictoriaMetricsURL string   `json:"victoria_metrics_url"`
	VictoriaLogsURL    string   `json:"victoria_logs_url"`
}

type settingsUpdate struct {
	AllowedIPs         []string `json:"allowed_ips"`
	EPGSources         []string `json:"epg_sources"`
	NewPassword        string   `json:"new_password,omitempty"`
	VictoriaMetricsURL string   `json:"victoria_metrics_url"`
	VictoriaLogsURL    string   `json:"victoria_logs_url"`
}

func settingsViewOf(c *config.Config) settingsView {
	return settingsView{
		AdminUsername:      c.Admin.Username,
		ListenAddr:         c.ListenAddr,
		AllowedIPs:         c.AllowedIPs,
		EPGSources:         c.EPGSources,
		VictoriaMetricsURL: c.VictoriaMetricsURL,
		VictoriaLogsURL:    c.VictoriaLogsURL,
	}
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	c := s.store.Config()
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, settingsViewOf(c))
	case http.MethodPut:
		var req settingsUpdate
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		c.AllowedIPs = req.AllowedIPs
		c.EPGSources = req.EPGSources
		// Take effect on next restart: the metrics/log shippers are
		// started once at boot from these values.
		c.VictoriaMetricsURL = req.VictoriaMetricsURL
		c.VictoriaLogsURL = req.VictoriaLogsURL
		if req.NewPassword != "" {
			hash, err := HashPassword(req.NewPassword)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			c.Admin.PasswordHash = hash
		}
		if err := s.store.SaveConfig(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, settingsViewOf(c))
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

type statusView struct {
	StreamCount  int          `json:"stream_count"`
	EnabledCount int          `json:"enabled_count"`
	RunningCount int          `json:"running_count"`
	TunerCount   int          `json:"tuner_count"` // configured DVB-input streams
	ClientCount  int          `json:"client_count"`
	InputBps     float64      `json:"input_bps"`
	OutputBps    float64      `json:"output_bps"`
	UptimeSecs   int64        `json:"uptime_seconds"`
	Streams      []StreamStat `json:"streams"`
}

func (s *Server) buildStatusView() statusView {
	c := s.store.Config()
	stats := s.store.StreamStats()

	statByID := make(map[string]StreamStat, len(stats))
	for _, st := range stats {
		statByID[st.ID] = st
	}

	view := statusView{StreamCount: len(c.Streams), Streams: stats}
	for _, st := range c.Streams {
		if st.Enabled {
			view.EnabledCount++
		}
		if st.Input.Type == config.InputDVB {
			view.TunerCount++
		}
		if live, ok := statByID[st.ID]; ok {
			if live.Running {
				view.RunningCount++
			}
			view.ClientCount += live.Clients
			view.InputBps += live.InputBps
			view.OutputBps += live.OutputBps
		}
	}
	view.UptimeSecs = int64(time.Since(s.startedAt).Seconds())
	return view
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.buildStatusView())
}

// handleEvents pushes the same payload as GET /api/status as a
// Server-Sent Events stream, once immediately and then every 2 seconds, so
// the dashboard/streams/tuners views get live updates without polling.
// Plain HTTP, no upgrade handshake — EventSource also reconnects on its
// own if the connection drops, so there's no client-side retry logic to
// write either.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	send := func() bool {
		data, err := json.Marshal(s.buildStatusView())
		if err != nil {
			return false
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}
	if !send() {
		return
	}

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			if !send() {
				return
			}
		}
	}
}

// logLineView is a logbuf.Line reshaped for JSON, with a Unix-millisecond
// timestamp instead of a Go time.Time (simpler for the frontend to sort/
// format without a date-parsing dependency).
type logLineView struct {
	Seq  int64  `json:"seq"`
	TsMS int64  `json:"ts_ms"`
	Text string `json:"text"`
}

func toLogLineViews(lines []logbuf.Line) []logLineView {
	out := make([]logLineView, len(lines))
	for i, l := range lines {
		out[i] = logLineView{Seq: l.Seq, TsMS: l.Time.UnixMilli(), Text: l.Text}
	}
	return out
}

// handleLogs returns buffered log lines as JSON. ?since=<seq> returns only
// lines after that sequence number (for polling/pagination); omitted or 0
// returns everything currently buffered.
func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	writeJSON(w, struct {
		Lines []logLineView `json:"lines"`
		Last  int64         `json:"last_seq"`
	}{toLogLineViews(s.store.LogLines(since)), s.store.LastLogSeq()})
}

// handleLogEvents streams new log lines as Server-Sent Events as they're
// written, starting from "now" (the caller isn't expected to have seen
// anything before it connected) unless ?since=<seq> asks to resume from a
// specific point (e.g. after a brief disconnect).
func (s *Server) handleLogEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	since := s.store.LastLogSeq()
	if v, err := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64); err == nil && v > 0 {
		since = v
	}

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			lines := s.store.LogLines(since)
			if len(lines) == 0 {
				continue
			}
			since = lines[len(lines)-1].Seq
			data, err := json.Marshal(toLogLineViews(lines))
			if err != nil {
				continue
			}
			if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// handleNetworkInterfaces lists this machine's own local IPv4 addresses
// (the equivalent of `ip addr`/`ifconfig`), for populating a "Listen
// address" dropdown when configuring a server-mode input (RTMP/RTMPS,
// etc.) — picking one of these, rather than typing an IP by hand, avoids
// configuring an address this machine doesn't actually own (which fails
// to bind at all) or a remote device's address by mistake (which was
// never bindable here in the first place).
func (s *Server) handleNetworkInterfaces(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, struct {
		Addrs []string `json:"addrs"`
	}{listLocalIPv4s()})
}

func listLocalIPv4s() []string {
	out := []string{"0.0.0.0"} // always first: bind all interfaces
	ifaces, err := net.Interfaces()
	if err != nil {
		return out
	}
	for _, iface := range ifaces {
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipnet.IP.To4()
			if ip4 == nil {
				continue
			}
			out = append(out, ip4.String())
		}
	}
	return out
}

func (s *Server) saveAndApply(w http.ResponseWriter) error {
	if err := s.store.SaveConfig(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return err
	}
	if err := s.store.ApplyStreams(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return err
	}
	return nil
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
