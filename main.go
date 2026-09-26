// Command hdtvheadend is a single-binary DVB/IP streaming headend: it tunes
// DVB-S/S2/T/T2/C hardware and/or pulls HTTP-TS, UDP/RTP, HLS and DASH
// sources, optionally descrambles them (BISS, or a real hardware CI/CI+
// CAM), and republishes them as HTTP-TS, UDP/multicast and an M3U
// playlist, behind a small web UI.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"time"

	"hdtvheadend/internal/config"
	"hdtvheadend/internal/dynhttp"
	"hdtvheadend/internal/epg"
	"hdtvheadend/internal/install"
	"hdtvheadend/internal/logbuf"
	"hdtvheadend/internal/manager"
	"hdtvheadend/internal/metrics"
	"hdtvheadend/internal/obs"
	"hdtvheadend/internal/output/playlist"
	"hdtvheadend/internal/webui"
)

func main() {
	installFlag := flag.Bool("install", false, "install HDTVheadend as a systemd service")
	uninstallFlag := flag.Bool("uninstall", false, "uninstall HDTVheadend")
	listenFlag := flag.String("listen", "", "listen address for -install (default :8088)")
	configFlag := flag.String("config", install.ConfigPath, "path to config.json")
	flag.Parse()

	switch {
	case *installFlag:
		if err := install.Run(*listenFlag); err != nil {
			log.Fatal(err)
		}
		return
	case *uninstallFlag:
		if err := install.RunUninstall(); err != nil {
			log.Fatal(err)
		}
		return
	}

	if err := runServer(*configFlag); err != nil {
		log.Fatal(err)
	}
}

// app wires config, running streams, and the web UI/EPG together, and
// implements webui.Store so the dashboard can edit streams live.
type app struct {
	mu       sync.Mutex
	cfgPath  string
	cfg      *config.Config
	mux      *dynhttp.Mux
	handles  map[string]*manager.Handle
	epgStore *epg.Store
	baseCtx  context.Context
	logs     *logbuf.Ring
}

func runServer(cfgPath string) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return fmt.Errorf("load config %s: %w (run with -install first)", cfgPath, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// VictoriaLogs is optional and off unless configured; NewLogShipper
	// returns nil in that case, and (*obs.LogShipper).Write on a nil
	// receiver is a safe no-op, so this tee is always fine to install.
	// logs (in-memory, always on) backs the dashboard's own Logs page, so
	// there's always somewhere to see what happened without needing
	// VictoriaLogs or shell access to the box.
	logShipper := obs.NewLogShipper(cfg.VictoriaLogsURL)
	logs := logbuf.New(2000)
	log.SetOutput(io.MultiWriter(os.Stderr, logShipper, logs))
	if logShipper != nil {
		go logShipper.Run(ctx, 5*time.Second, func(err error) { log.Printf("victorialogs: %v", err) })
	}

	// fixedMux holds routes registered exactly once at startup and never
	// touched again (the dashboard itself, its API, playlist/EPG/
	// metrics) — a plain http.ServeMux is fine for these. Stream inputs/
	// outputs instead register on a.mux (dynhttp.Mux), which — unlike
	// ServeMux — supports being added to, changed, and removed from for
	// as long as the process runs, needed because streams get added,
	// edited and removed live; a plain ServeMux panics if a path is ever
	// registered a second time (which editing a stream back to an
	// earlier output path, or recreating one with the same default path,
	// would otherwise trigger) and has no way to unregister a path at
	// all. Requests matching no dynamic route fall through to fixedMux.
	fixedMux := http.NewServeMux()
	a := &app{
		cfgPath:  cfgPath,
		cfg:      cfg,
		mux:      dynhttp.New(fixedMux),
		handles:  make(map[string]*manager.Handle),
		epgStore: epg.NewStore(),
		baseCtx:  ctx,
		logs:     logs,
	}

	if err := a.ApplyStreams(); err != nil {
		return err
	}

	fixedMux.HandleFunc("/playlist.m3u8", playlist.Handler(a.Config))
	fixedMux.HandleFunc("/epg.xml", a.epgStore.Handler)
	fixedMux.Handle("/metrics", metrics.NewHandler(metricsAdapter{a}))
	webui.New(a).Register(fixedMux)

	go a.refreshEPGLoop(ctx)
	if cfg.VictoriaMetricsURL != "" {
		localAddr := cfg.ListenAddr
		if strings.HasPrefix(localAddr, ":") {
			localAddr = "127.0.0.1" + localAddr
		}
		go obs.ShipMetrics(ctx, cfg.VictoriaMetricsURL, "http://"+localAddr+"/metrics", 10*time.Second,
			func(err error) { log.Printf("victoriametrics: %v", err) })
	}

	srv := &http.Server{Addr: cfg.ListenAddr, Handler: a.mux}
	go func() {
		log.Printf("HDTVheadend listening on %s", cfg.ListenAddr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig

	log.Println("shutting down...")
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	_ = srv.Shutdown(shutdownCtx)
	cancel()
	a.stopAll()
	return nil
}

// metricsAdapter converts app's webui-shaped StreamStats into the plain
// struct internal/metrics expects, keeping that package free of the
// webui/session/HTTP dependency tree.
type metricsAdapter struct{ a *app }

func (m metricsAdapter) StreamStats() []metrics.StreamStat {
	src := m.a.StreamStats()
	out := make([]metrics.StreamStat, len(src))
	for i, s := range src {
		out[i] = metrics.StreamStat{
			ID: s.ID, Running: s.Running, Clients: s.Clients, InputBps: s.InputBps, OutputBps: s.OutputBps,
			ActiveSource: s.ActiveSource, SourcePinned: s.SourcePinned,
		}
	}
	return out
}

func (a *app) Config() *config.Config {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cfg
}

func (a *app) SaveConfig() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cfg.Save(a.cfgPath)
}

// ApplyStreams reconciles running stream handles against the current
// config: stopping removed/disabled streams and starting new/enabled ones.
// A changed stream is restarted (stop + start) rather than diffed in place.
func (a *app) ApplyStreams() error {
	a.mu.Lock()
	defer a.mu.Unlock()

	wanted := make(map[string]config.Stream)
	for _, s := range a.cfg.Streams {
		if s.Enabled {
			wanted[s.ID] = s
		}
	}

	for id, h := range a.handles {
		if _, ok := wanted[id]; !ok {
			h.Stop()
			delete(a.handles, id)
		}
	}

	for id, s := range wanted {
		if h, running := a.handles[id]; running {
			if reflect.DeepEqual(h.Stream, s) {
				continue // unchanged; leave it running as-is
			}
			// Changed: stop the old handle so its outputs' path
			// registrations stop serving live data, then start a fresh
			// one with the new config. Note this can't reclaim the old
			// paths themselves — net/http's ServeMux has no way to
			// unregister a handler once registered — so an old output
			// URL a client is still holding won't 404, but its bus is
			// closed and it'll stop receiving any new data. Any new or
			// changed output paths register cleanly since they're new to
			// the mux.
			h.Stop()
			delete(a.handles, id)
		}
		h, err := manager.Start(a.baseCtx, s, a.mux)
		if err != nil {
			log.Printf("stream %s: failed to start: %v", id, err)
			continue
		}
		a.handles[id] = h
	}
	return nil
}

// LogLines returns buffered log lines with Seq > since (0 for everything
// currently buffered), for the dashboard's Logs page.
func (a *app) LogLines(since int64) []logbuf.Line {
	return a.logs.LinesSince(since)
}

// LastLogSeq returns the current log sequence number, so the Logs page's
// live tail can start from "now" without first fetching the whole buffer.
func (a *app) LastLogSeq() int64 {
	return a.logs.LastSeq()
}

// StreamStats reports live client/throughput counters for every configured
// stream, for the dashboard. A stream with no running handle (disabled, or
// failed to start) reports zero values.
func (a *app) StreamStats() []webui.StreamStat {
	a.mu.Lock()
	defer a.mu.Unlock()

	out := make([]webui.StreamStat, 0, len(a.cfg.Streams))
	for _, s := range a.cfg.Streams {
		stat := webui.StreamStat{ID: s.ID, SourceCount: 1 + len(s.BackupInputs), OnDemand: s.Mode == config.StreamModeOnDemand}
		if h, ok := a.handles[s.ID]; ok {
			stat.Running = true
			stat.InputActive = h.InputActive()
			clients := h.Bus.Subscribers()
			stat.Clients = clients
			stat.InputBps = h.RawBus.RateBps()
			stat.OutputBps = h.Bus.RateBps() * float64(clients)
			active, pinned, total, lastErr := h.FailoverStatus()
			stat.ActiveSource = active
			stat.SourcePinned = pinned
			stat.SourceCount = total
			if lastErr != nil {
				stat.SourceError = lastErr.Error()
			}
		}
		out = append(out, stat)
	}
	return out
}

// SwitchSource pins a running stream's input to a specific source index
// (0 = primary, 1+ = backups in order), or returns it to automatic
// failover if index is negative.
func (a *app) SwitchSource(streamID string, index int) error {
	a.mu.Lock()
	h, ok := a.handles[streamID]
	a.mu.Unlock()
	if !ok {
		return fmt.Errorf("stream %q is not running", streamID)
	}
	return h.SwitchSource(index)
}

func (a *app) stopAll() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, h := range a.handles {
		h.Stop()
	}
}

func (a *app) refreshEPGLoop(ctx context.Context) {
	refresh := func() {
		sources := a.Config().EPGSources
		if len(sources) == 0 {
			return
		}
		if err := a.epgStore.Refresh(ctx, sources); err != nil {
			log.Printf("epg: refresh failed: %v", err)
		}
	}
	refresh()
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			refresh()
		}
	}
}
