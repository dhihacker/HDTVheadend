// Package webrtc implements a WHEP (WebRTC-HTTP Egress Protocol) output: a
// browser POSTs an SDP offer to an HTTP endpoint on this server to start
// watching, ICE/DTLS/SRTP then carries the media directly. Only H.264 video
// is sent — WebRTC's mandatory audio codec is Opus, and this binary's
// pipeline is AAC-only with no audio transcoding, so a stream's audio (if
// any) is simply not sent to WHEP viewers.
//
// Unlike the WHIP input, WHEP has no "one at a time" limit: any number of
// viewers may POST an offer concurrently, each getting their own
// PeerConnection fed from the same streambus subscription pattern used by
// every other output.
//
// Verified end-to-end with a real Pion-based WHEP viewer test client (real
// ICE/DTLS/SRTP, not a mock): received H.264 access units decode cleanly
// via ffmpeg once past the viewer's first fully-delivered keyframe. As with
// any live stream, a viewer who joins mid-GOP won't render anything until
// the next keyframe arrives — ordinary behavior, not specific to WebRTC or
// this package.
package webrtc

import (
	"context"
	"io"
	"net/http"
	"time"

	pion "github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"

	"hdtvheadend/internal/streambus"
	"hdtvheadend/internal/tsmux"
)

// DemandHook lets a caller (see internal/manager) track when a viewer's
// session starts and ends, to drive an on-demand stream's input
// start/stop. Acquire is called once a viewer's PeerConnection is fully
// negotiated (about to start receiving media); the caller must call the
// returned release func exactly once, when that viewer disconnects.
type DemandHook interface {
	Acquire() (release func())
}

// Handler returns an http.HandlerFunc serving a WHEP endpoint: each POST is
// one viewer's SDP offer/answer exchange. hook may be nil (no demand
// tracking; the usual case for a statically-running stream). ctx bounds
// every viewer's session lifetime (canceling it disconnects all current
// viewers), independent of any per-viewer disconnect.
func Handler(ctx context.Context, bus *streambus.Bus, hook DemandHook) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		handleWHEP(ctx, w, r, bus, hook)
	}
}

func handleWHEP(ctx context.Context, w http.ResponseWriter, r *http.Request, bus *streambus.Bus, hook DemandHook) {
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

	pc, err := pion.NewPeerConnection(pion.Configuration{})
	if err != nil {
		http.Error(w, "create peer connection: "+err.Error(), http.StatusInternalServerError)
		return
	}

	videoTrack, err := pion.NewTrackLocalStaticSample(
		pion.RTPCodecCapability{MimeType: pion.MimeTypeH264, ClockRate: 90000},
		"video", "hdtvheadend",
	)
	if err != nil {
		pc.Close()
		http.Error(w, "create video track: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if _, err := pc.AddTrack(videoTrack); err != nil {
		pc.Close()
		http.Error(w, "add video track: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// A receive-only audio transceiver isn't needed (we never send audio),
	// but some browsers (Firefox, Chrome) have been observed not rendering
	// a video-only remote stream unless the peer connection has at least
	// one audio m-line too. Declaring a sendonly one that never carries any
	// samples satisfies that without adding real audio support.
	if _, err := pc.AddTransceiverFromKind(pion.RTPCodecTypeAudio, pion.RTPTransceiverInit{
		Direction: pion.RTPTransceiverDirectionSendonly,
	}); err != nil {
		pc.Close()
		http.Error(w, "add audio transceiver: "+err.Error(), http.StatusInternalServerError)
		return
	}

	viewerCtx, cancel := context.WithCancel(ctx)
	pc.OnConnectionStateChange(func(state pion.PeerConnectionState) {
		switch state {
		case pion.PeerConnectionStateFailed, pion.PeerConnectionStateClosed, pion.PeerConnectionStateDisconnected:
			cancel()
		}
	})

	if err := pc.SetRemoteDescription(pion.SessionDescription{Type: pion.SDPTypeOffer, SDP: string(offerSDP)}); err != nil {
		cancel()
		pc.Close()
		http.Error(w, "set remote description: "+err.Error(), http.StatusBadRequest)
		return
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		cancel()
		pc.Close()
		http.Error(w, "create answer: "+err.Error(), http.StatusInternalServerError)
		return
	}
	gatherComplete := pion.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(answer); err != nil {
		cancel()
		pc.Close()
		http.Error(w, "set local description: "+err.Error(), http.StatusInternalServerError)
		return
	}
	select {
	case <-gatherComplete:
	case <-time.After(10 * time.Second):
		cancel()
		pc.Close()
		http.Error(w, "ICE gathering timed out", http.StatusGatewayTimeout)
		return
	}

	var release func()
	if hook != nil {
		release = hook.Acquire()
	}
	go func() {
		if release != nil {
			defer release()
		}
		feedViewer(viewerCtx, bus, videoTrack, pc)
	}()

	final := pc.LocalDescription()
	w.Header().Set("Content-Type", "application/sdp")
	w.Header().Set("Location", r.URL.Path) // simplified: no per-session DELETE resource
	w.WriteHeader(http.StatusCreated)
	_, _ = w.Write([]byte(final.SDP))
}

// feedViewer demuxes bus's MPEG-TS content and writes each video access
// unit to track as a WebRTC sample; Pion handles RTP packetization
// (including FU-A fragmentation) internally. Runs until ctx is canceled
// (viewer disconnected) or the stream ends.
func feedViewer(ctx context.Context, bus *streambus.Bus, track *pion.TrackLocalStaticSample, pc *pion.PeerConnection) {
	defer pc.Close()

	ch, unsub := bus.Subscribe(1024)
	defer unsub()

	demux := tsmux.NewDemuxer()
	lastPTS := int64(-1)
	const defaultFrameDur = 40 * time.Millisecond // ~25fps, used only for the first frame

	for {
		select {
		case <-ctx.Done():
			return
		case chunk, ok := <-ch:
			if !ok {
				return
			}
			var writeErr error
			demux.Feed(chunk, func(au tsmux.AccessUnit) {
				if !au.Video || writeErr != nil {
					return
				}
				dur := defaultFrameDur
				if lastPTS >= 0 && au.PTS > lastPTS {
					dur = time.Duration(au.PTS-lastPTS) * time.Second / 90000
				}
				lastPTS = au.PTS
				// au.Data is already Annex-B framed (start codes included),
				// which is exactly what Pion's H264 sample packetizer expects.
				writeErr = track.WriteSample(media.Sample{Data: au.Data, Duration: dur})
			})
			if writeErr != nil {
				return
			}
		}
	}
}
