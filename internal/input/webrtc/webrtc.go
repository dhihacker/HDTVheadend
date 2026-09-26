// Package webrtc implements a WHIP (WebRTC-HTTP Ingestion Protocol) input:
// a publisher (OBS 30+, a browser, or any WHIP client) POSTs an SDP offer
// to an HTTP endpoint on this server to start publishing, ICE/DTLS/SRTP
// then carries the media directly. Only H.264 video is bridged into the TS
// pipeline and republished to the streambus; an audio track, if offered, is
// accepted (so senders that always send audio still negotiate cleanly) but
// its RTP is just drained and discarded — WebRTC's mandatory audio codec is
// Opus, which this binary has no way to get into an AAC-based MPEG-TS
// stream without an audio transcoder, and none exists here.
//
// One publisher at a time; a second WHIP POST is rejected with 409 while
// one is active.
//
// Verified end-to-end with a real Pion-based WHIP publisher test client
// (real ICE/DTLS/SRTP, not a mock): the resulting MPEG-TS decodes cleanly
// once the stream reaches its first fully-received keyframe. The very first
// GOP after a publisher connects can be lost if the encoder starts writing
// samples before ICE connectivity is actually established on the wire —
// ordinary WebRTC transport behavior (media is best-effort UDP; nothing is
// buffered/retransmitted before the connection is up), not a defect in this
// package. A well-behaved encoder (OBS, browsers) already waits for its
// PeerConnection to connect before feeding a track, which avoids this in
// practice; only a naively-eager publisher would hit it.
package webrtc

import (
	"context"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/pion/rtp/codecs"
	pion "github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media/samplebuilder"

	"hdtvheadend/internal/codecs/h264"
	"hdtvheadend/internal/dynhttp"
	"hdtvheadend/internal/streambus"
	"hdtvheadend/internal/tsmux"
)

// Run registers a WHIP endpoint at path on mux and blocks until ctx is
// canceled.
func Run(ctx context.Context, mux *dynhttp.Mux, path string, bus *streambus.Bus) error {
	shared := &sharedState{bus: bus}
	mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
		handleWHIP(ctx, w, r, shared)
	})
	<-ctx.Done()
	return ctx.Err()
}

type sharedState struct {
	bus *streambus.Bus

	mu     sync.Mutex
	active bool
}

func (s *sharedState) tryAcquire() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active {
		return false
	}
	s.active = true
	return true
}

func (s *sharedState) release() {
	s.mu.Lock()
	s.active = false
	s.mu.Unlock()
}

func handleWHIP(ctx context.Context, w http.ResponseWriter, r *http.Request, shared *sharedState) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if ct := r.Header.Get("Content-Type"); ct != "" && ct != "application/sdp" {
		http.Error(w, "expected Content-Type: application/sdp", http.StatusUnsupportedMediaType)
		return
	}
	offerSDP, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read offer: "+err.Error(), http.StatusBadRequest)
		return
	}

	if !shared.tryAcquire() {
		http.Error(w, "a publisher is already active", http.StatusConflict)
		return
	}

	pc, err := pion.NewPeerConnection(pion.Configuration{})
	if err != nil {
		shared.release()
		http.Error(w, "create peer connection: "+err.Error(), http.StatusInternalServerError)
		return
	}

	var closeOnce sync.Once
	cleanup := func() {
		closeOnce.Do(func() {
			shared.release()
			pc.Close()
		})
	}

	pc.OnConnectionStateChange(func(state pion.PeerConnectionState) {
		switch state {
		case pion.PeerConnectionStateFailed, pion.PeerConnectionStateClosed, pion.PeerConnectionStateDisconnected:
			cleanup()
		}
	})
	go func() {
		<-ctx.Done() // stream removed/disabled/server shutting down
		cleanup()
	}()

	pc.OnTrack(func(track *pion.TrackRemote, receiver *pion.RTPReceiver) {
		if track.Kind() == pion.RTPCodecTypeAudio {
			drainTrack(track)
			return
		}
		readVideoTrack(track, shared.bus)
	})

	if err := pc.SetRemoteDescription(pion.SessionDescription{Type: pion.SDPTypeOffer, SDP: string(offerSDP)}); err != nil {
		cleanup()
		http.Error(w, "set remote description: "+err.Error(), http.StatusBadRequest)
		return
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		cleanup()
		http.Error(w, "create answer: "+err.Error(), http.StatusInternalServerError)
		return
	}
	gatherComplete := pion.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(answer); err != nil {
		cleanup()
		http.Error(w, "set local description: "+err.Error(), http.StatusInternalServerError)
		return
	}
	select {
	case <-gatherComplete:
	case <-time.After(10 * time.Second):
		cleanup()
		http.Error(w, "ICE gathering timed out", http.StatusGatewayTimeout)
		return
	}

	final := pc.LocalDescription()
	w.Header().Set("Content-Type", "application/sdp")
	w.Header().Set("Location", r.URL.Path) // simplified: no per-session DELETE resource
	w.WriteHeader(http.StatusCreated)
	_, _ = w.Write([]byte(final.SDP))
}

func drainTrack(track *pion.TrackRemote) {
	for {
		if _, _, err := track.ReadRTP(); err != nil {
			return
		}
	}
}

// readVideoTrack depacketizes H.264 RTP into access units and remuxes them
// into MPEG-TS on bus, until the track ends (peer disconnected/closed).
func readVideoTrack(track *pion.TrackRemote, bus *streambus.Bus) {
	clockRate := track.Codec().ClockRate
	if clockRate == 0 {
		clockRate = 90000
	}
	sb := samplebuilder.New(50, &codecs.H264Packet{}, clockRate)

	var tsBuf []byte
	m := tsmux.NewMuxer(func(pkt []byte) { tsBuf = append(tsBuf, pkt...) })
	flush := func() {
		if len(tsBuf) == 0 {
			return
		}
		chunk := tsBuf
		tsBuf = nil
		bus.Publish(chunk)
	}

	var pts int64
	for {
		pkt, _, err := track.ReadRTP()
		if err != nil {
			return
		}
		sb.Push(pkt)
		for sample := sb.Pop(); sample != nil; sample = sb.Pop() {
			nalus := h264.SplitAnnexB(sample.Data)
			if len(nalus) == 0 {
				continue
			}
			keyframe := h264.HasKeyframe(nalus)
			m.WriteVideo(pts, pts, nalus, keyframe)
			flush()
			pts += int64(sample.Duration.Seconds() * 90000)
		}
	}
}
