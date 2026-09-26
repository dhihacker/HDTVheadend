// Package manager wires each configured stream's input through optional
// conditional access to its streambus, and starts/stops the outputs that
// read from it.
package manager

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"hdtvheadend/internal/ca/biss"
	"hdtvheadend/internal/config"
	"hdtvheadend/internal/dvb"
	"hdtvheadend/internal/dynhttp"
	"hdtvheadend/internal/failover"
	"hdtvheadend/internal/input/dash"
	"hdtvheadend/internal/input/hls"
	httpin "hdtvheadend/internal/input/httpts"
	rtmpin "hdtvheadend/internal/input/rtmp"
	rtspin "hdtvheadend/internal/input/rtsp"
	srtin "hdtvheadend/internal/input/srt"
	udpin "hdtvheadend/internal/input/udprtp"
	webrtcin "hdtvheadend/internal/input/webrtc"
	"hdtvheadend/internal/output/hlsdash"
	httpout "hdtvheadend/internal/output/httpts"
	rtmpout "hdtvheadend/internal/output/rtmp"
	rtspout "hdtvheadend/internal/output/rtsp"
	srtout "hdtvheadend/internal/output/srt"
	udpout "hdtvheadend/internal/output/udprtp"
	webrtcout "hdtvheadend/internal/output/webrtc"
	"hdtvheadend/internal/streambus"
)

// Descrambler applies conditional access to a raw TS chunk in place.
type Descrambler interface {
	DescrambleStream(buf []byte)
}

// Handle is a running stream: its bus (for the HTTP mux to attach output
// handlers to), its failover supervisor (for source status/manual
// switching), and a cancel func to stop it.
type Handle struct {
	Stream config.Stream
	Bus    *streambus.Bus // outBus: what outputs read from (post-CA)
	RawBus *streambus.Bus // rawBus: raw input rate, pre-CA

	mu       sync.Mutex
	failover *failover.Supervisor // nil while an on-demand input is idle

	demand *demandTracker // nil for a static-mode stream

	cancel  context.CancelFunc
	wg      sync.WaitGroup // output goroutines
	inputWG sync.WaitGroup // the current input goroutine, if any

	mux             *dynhttp.Mux
	registeredPaths []string // this stream's own paths on mux, unregistered on Stop
}

func (h *Handle) Stop() {
	h.cancel()
	h.wg.Wait()
	h.inputWG.Wait()
	h.Bus.Close()
	for _, p := range h.registeredPaths {
		h.mux.Remove(p)
	}
}

// FailoverStatus reports the current input's source status. If the input
// is idle (on-demand, no demand right now), it reports no error and a
// source count of 1 with no way to know how many backups are configured;
// callers needing that should consult Stream.BackupInputs instead.
func (h *Handle) FailoverStatus() (active int, pinned bool, total int, lastErr error) {
	h.mu.Lock()
	f := h.failover
	h.mu.Unlock()
	if f == nil {
		return 0, false, 1 + len(h.Stream.BackupInputs), nil
	}
	return f.Status()
}

// SwitchSource pins the running input to a specific source, or returns an
// error if the input is currently idle (on-demand, nothing to switch).
func (h *Handle) SwitchSource(index int) error {
	h.mu.Lock()
	f := h.failover
	h.mu.Unlock()
	if f == nil {
		return fmt.Errorf("input is idle (on-demand, no active viewers right now)")
	}
	return f.SwitchTo(index)
}

// InputActive reports whether the input is currently running: always true
// for a static stream once its Handle exists, or reflects current demand
// for an on-demand one.
func (h *Handle) InputActive() bool {
	if h.demand == nil {
		return true
	}
	return h.demand.IsRunning()
}

// OnDemand reports whether this stream's input starts/stops based on
// viewer demand rather than running continuously.
func (h *Handle) OnDemand() bool {
	return h.demand != nil
}

// demandAcquirePermanent marks the input as permanently demanded because a
// push-style output (UDP/SRT/RTSP/RTMP) with no per-viewer connect/
// disconnect signal is configured — for an on-demand stream, such an
// output falls back to keeping the input running continuously for as long
// as it's configured, same as static mode.
func (h *Handle) demandAcquirePermanent() {
	if h.demand != nil {
		h.demand.Acquire()
	}
}

// Start launches a stream's input, wraps it with CA if configured, and
// starts its outputs. It registers HTTP-TS output paths on mux.
func Start(ctx context.Context, s config.Stream, mux *dynhttp.Mux) (*Handle, error) {
	ctx, cancel := context.WithCancel(ctx)
	rawBus := streambus.New()
	outBus := rawBus

	descrambler, err := buildCA(s.CA)
	if err != nil {
		cancel()
		return nil, err
	}
	if descrambler != nil {
		outBus = streambus.New()
		ch, unsub := rawBus.Subscribe(512)
		go func() {
			defer unsub()
			for chunk := range ch {
				buf := append([]byte(nil), chunk...)
				descrambler.DescrambleStream(buf)
				outBus.Publish(buf)
			}
		}()
	}

	sources := append([]config.Input{s.Input}, s.BackupInputs...)
	run := func(ctx context.Context, in config.Input, bus *streambus.Bus) error {
		return runInput(ctx, in, bus, mux)
	}

	h := &Handle{Stream: s, Bus: outBus, RawBus: rawBus, cancel: cancel, mux: mux}
	registerPath := func(path string, handler http.Handler) {
		mux.Handle(path, handler)
		h.registeredPaths = append(h.registeredPaths, path)
	}

	var inputCancel context.CancelFunc
	startInput := func() {
		var inputCtx context.Context
		inputCtx, inputCancel = context.WithCancel(ctx)
		sup := failover.NewSupervisor(sources, run, rawBus)
		h.mu.Lock()
		h.failover = sup
		h.mu.Unlock()
		h.inputWG.Add(1)
		go func() {
			defer h.inputWG.Done()
			onEvent := func(idx int, err error) {
				if err == nil {
					if idx == 0 {
						log.Printf("stream %s: input active (primary)", s.ID)
					} else {
						log.Printf("stream %s: input active (backup #%d)", s.ID, idx)
					}
					return
				}
				log.Printf("stream %s: input source #%d failed: %v", s.ID, idx, err)
			}
			if err := sup.Run(inputCtx, onEvent); err != nil && inputCtx.Err() == nil {
				log.Printf("stream %s: input stopped: %v", s.ID, err)
			}
		}()
	}
	stopInput := func() {
		h.mu.Lock()
		h.failover = nil
		h.mu.Unlock()
		if inputCancel != nil {
			inputCancel()
		}
		h.inputWG.Wait()
	}

	// A WHIP input registers itself on the shared mux once and is a
	// passive listener (waiting for a publisher), not something with a
	// per-viewer demand signal to gate on the way outputs have — so it
	// always runs statically, regardless of Mode. Its path is still
	// tracked for removal on Stop, same as every other registered path.
	usesSharedMuxInput := s.Input.Type == config.InputWebRTC
	if usesSharedMuxInput {
		h.registeredPaths = append(h.registeredPaths, s.Input.Path)
	}
	for _, b := range s.BackupInputs {
		if b.Type == config.InputWebRTC {
			usesSharedMuxInput = true
			h.registeredPaths = append(h.registeredPaths, b.Path)
		}
	}

	if s.Mode == config.StreamModeOnDemand && !usesSharedMuxInput {
		h.demand = newDemandTracker(ctx, startInput, stopInput, 20*time.Second)
	} else {
		startInput()
	}

	for _, o := range s.Outputs {
		o := o
		switch o.Type {
		case config.OutputHTTPTS:
			registerPath(o.Path, wrapConnDemand(h.demand, httpout.Handler(outBus)))
		case config.OutputUDP:
			h.demandAcquirePermanent()
			h.wg.Add(1)
			go func() {
				defer h.wg.Done()
				if err := udpout.Run(ctx, o.Addr, o.TTL, o.RTP, outBus); err != nil && ctx.Err() == nil {
					log.Printf("stream %s: udp output %s stopped: %v", s.ID, o.Addr, err)
				}
			}()
		case config.OutputSRT:
			h.demandAcquirePermanent()
			h.wg.Add(1)
			go func() {
				defer h.wg.Done()
				mode := string(o.SRTMode)
				if err := srtout.Run(ctx, o.Addr, mode, o.SRTPassphrase, o.SRTStreamID, o.SRTLatencyMS, outBus); err != nil && ctx.Err() == nil {
					log.Printf("stream %s: srt output %s stopped: %v", s.ID, o.Addr, err)
				}
			}()
		case config.OutputRTSP:
			h.demandAcquirePermanent()
			h.wg.Add(1)
			go func() {
				defer h.wg.Done()
				if err := rtspout.Run(ctx, o.Addr, outBus); err != nil && ctx.Err() == nil {
					log.Printf("stream %s: rtsp output %s stopped: %v", s.ID, o.Addr, err)
				}
			}()
		case config.OutputHLS, config.OutputDASH:
			segDur := time.Duration(orDefault(o.SegmentSeconds, 4)) * time.Second
			count := orDefault(o.SegmentCount, 6)
			seg := hlsdash.NewSegmenter()
			h.wg.Add(1)
			go func() {
				defer h.wg.Done()
				if err := seg.Run(ctx, outBus, segDur, count); err != nil && ctx.Err() == nil {
					log.Printf("stream %s: %s segmenter stopped: %v", s.ID, o.Type, err)
				}
			}()
			registerPath(ensureTrailingSlash(o.Path), wrapHeartbeatDemand(h.demand, seg.Handler(int(segDur.Seconds()))))
		case config.OutputRTMP:
			h.demandAcquirePermanent()
			h.wg.Add(1)
			go func() {
				defer h.wg.Done()
				if err := rtmpout.Run(ctx, o.URL, outBus); err != nil && ctx.Err() == nil {
					log.Printf("stream %s: rtmp output stopped: %v", s.ID, err)
				}
			}()
		case config.OutputWebRTC:
			registerPath(o.Path, webrtcout.Handler(ctx, outBus, demandHookOf(h.demand)))
		}
	}

	return h, nil
}

// wrapConnDemand wraps a long-lived-connection handler (HTTP-TS: one
// request per client, held open for the whole session) so entering the
// handler counts as demand and returning from it (client disconnected)
// releases it.
func wrapConnDemand(d *demandTracker, handler http.HandlerFunc) http.HandlerFunc {
	if d == nil {
		return handler
	}
	return func(w http.ResponseWriter, r *http.Request) {
		release := d.Acquire()
		defer release()
		handler(w, r)
	}
}

// wrapHeartbeatDemand wraps a request/response-style polling handler
// (HLS/DASH playlist and segment requests) so each request marks demand,
// idling out on its own after a period of no requests rather than needing
// a paired release.
func wrapHeartbeatDemand(d *demandTracker, handler http.HandlerFunc) http.HandlerFunc {
	if d == nil {
		return handler
	}
	return func(w http.ResponseWriter, r *http.Request) {
		d.Heartbeat()
		handler(w, r)
	}
}

// demandHookOf adapts *demandTracker to webrtcout.DemandHook, taking care
// to return a truly nil interface (not a non-nil interface wrapping a nil
// pointer) when there's no tracker, so the callee's own "hook != nil"
// check behaves correctly.
func demandHookOf(d *demandTracker) webrtcout.DemandHook {
	if d == nil {
		return nil
	}
	return d
}

func orDefault(v, def int) int {
	if v <= 0 {
		return def
	}
	return v
}

func ensureTrailingSlash(p string) string {
	if strings.HasSuffix(p, "/") {
		return p
	}
	return p + "/"
}

func runInput(ctx context.Context, in config.Input, bus *streambus.Bus, mux *dynhttp.Mux) error {
	switch in.Type {
	case config.InputHTTPTS:
		return httpin.Run(ctx, in.URL, bus)
	case config.InputUDP, config.InputRTP:
		return udpin.Run(ctx, in.Addr, in.Iface, bus)
	case config.InputHLS, config.InputYouTube:
		return hls.Run(ctx, in.URL, bus)
	case config.InputDASH:
		return dash.Run(ctx, in.URL, bus)
	case config.InputDVB:
		return runDVBInput(ctx, in, bus)
	case config.InputSRT:
		return srtin.Run(ctx, in, bus)
	case config.InputRTSP:
		return rtspin.Run(ctx, in.URL, bus)
	case config.InputRTMP:
		return rtmpin.Run(ctx, in.Addr, in.RTMPStreamKey, bus)
	case config.InputRTMPS:
		return rtmpin.RunTLS(ctx, in.Addr, in.RTMPStreamKey, in.TLSCertFile, in.TLSKeyFile, bus)
	case config.InputRTMPPull:
		return rtmpin.PullFrom(ctx, in.URL, bus)
	case config.InputWebRTC:
		return webrtcin.Run(ctx, mux, in.Path, bus)
	default:
		return fmt.Errorf("unknown input type %q", in.Type)
	}
}

func runDVBInput(ctx context.Context, in config.Input, bus *streambus.Bus) error {
	fe, err := dvb.OpenFrontend(in.DVBAdapter, in.DVBFrontend)
	if err != nil {
		return err
	}
	defer fe.Close()

	err = fe.Tune(dvb.TuneParams{
		System:       string(in.DVBSystem),
		FrequencyKHz: in.FrequencyKHz,
		SymbolRateKS: in.SymbolRateKS,
		Polarization: in.Polarization,
		BandwidthHz:  in.Bandwidth,
		Modulation:   in.Modulation,
	}, 10*time.Second)
	if err != nil {
		return err
	}

	dvr, err := dvb.OpenFullTSCapture(in.DVBAdapter, in.DVBFrontend, in.DVBFrontend)
	if err != nil {
		return err
	}
	return dvb.RunDVR(ctx, dvr, bus)
}

func buildCA(ca config.CA) (Descrambler, error) {
	switch ca.Type {
	case config.CANone:
		return nil, nil
	case config.CABISSSessionWord:
		return biss.NewSessionWord(ca.Key)
	case config.CABISSControlWord:
		return biss.NewControlWord(ca.Key)
	case config.CACIPlus:
		return nil, fmt.Errorf("ci_plus CA is applied by hardware CI CAM, not in software (see internal/ci)")
	default:
		return nil, fmt.Errorf("unknown CA type %q", ca.Type)
	}
}
