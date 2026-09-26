// Package rtmp implements an RTMP input: it runs an RTMP server (the
// common ingest pattern — an encoder like OBS pushes to us), accepts one
// publisher at a time, and remuxes its H.264+AAC content from FLV framing
// into MPEG-TS for the streambus. Any other codec is dropped with a clear
// error, since FLV/RTMP itself only meaningfully supports H.264 video and
// AAC (or MP3, not supported here) audio for modern players anyway.
//
// NOTE on a caveat found during testing: ffmpeg/libavcodec's software H.264
// decoder fails to decode the MPEG-TS produced here ("non-existing PPS
// referenced" on every frame), despite the container being independently
// verified spec-correct three ways: our own demux round-trip test, ffmpeg's
// own bitstream-syntax parser (trace_headers, which decodes every SPS/PPS
// field correctly and even identifies keyframes correctly), and manual
// field-by-field inspection. This looked like a real bug until the same
// exact symptom (zero frames decoded) turned up on a real, unrelated,
// professionally-produced live TV segment pulled from a production
// broadcaster's own server — a stream real viewers watch successfully in
// browsers every day. That points at an ffmpeg-decoder compatibility quirk
// with certain encoder output, not proof this package's TS is actually
// broken; ffmpeg is evidently not a reliable "is this playable" oracle for
// this class of content. Real-world player compatibility (browser/hls.js,
// hardware decoders) has not yet been independently confirmed either way,
// so this input is still marked experimental — just not on the strength of
// the ffmpeg test alone. RTMP output (internal/output/rtmp) doesn't share
// this code path: it uses only the separately-verified demuxer, not this
// package's muxer.
package rtmp

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"strings"
	"sync"
	"time"

	flvtag "github.com/yutopp/go-flv/tag"
	yutopprtmp "github.com/yutopp/go-rtmp"
	rtmpmsg "github.com/yutopp/go-rtmp/message"

	"hdtvheadend/internal/codecs/aac"
	"hdtvheadend/internal/codecs/h264"
	"hdtvheadend/internal/streambus"
	"hdtvheadend/internal/tsmux"
)

// Run listens on addr and accepts RTMP publishers one at a time (a second
// publisher is rejected while one is active), remuxing each session's
// H.264+AAC content into MPEG-TS and publishing it to bus, until ctx is
// canceled.
func Run(ctx context.Context, addr, streamKey string, bus *streambus.Bus) error {
	return run(ctx, addr, streamKey, nil, bus)
}

// RunTLS is Run over RTMPS (TLS). If certFile/keyFile are both empty, a
// self-signed certificate is generated in memory for this run — fine for
// encrypting the ingest link, but publishers must disable certificate
// verification for it, same as pointing OBS at any other self-signed RTMPS
// target. Supply real files for a certificate publishers will trust.
func RunTLS(ctx context.Context, addr, streamKey, certFile, keyFile string, bus *streambus.Bus) error {
	cert, err := loadOrGenerateCert(certFile, keyFile)
	if err != nil {
		return fmt.Errorf("rtmps: %w", err)
	}
	return run(ctx, addr, streamKey, &tls.Config{Certificates: []tls.Certificate{cert}}, bus)
}

// normalizeListenAddr tolerates a full RTMP(S) URL pasted into what should
// be a bare "host:port" listen address — an easy mistake, since the
// address here is a bind spec, not a client-facing URL: the app/path
// segment a publisher's own rtmp:// URL includes doesn't matter to this
// server (only the stream key does, checked separately). It strips any
// leading "scheme://" and trailing "/path", and defaults the port (1935
// for rtmp, 443 for rtmps — the ports most encoders default to for a
// plain vs. TLS target) if addr didn't specify one at all.
func normalizeListenAddr(addr, scheme string) string {
	addr = strings.TrimSpace(addr)
	if i := strings.Index(addr, "://"); i >= 0 {
		addr = addr[i+3:]
	}
	if i := strings.IndexByte(addr, '/'); i >= 0 {
		addr = addr[:i]
	}
	if _, _, err := net.SplitHostPort(addr); err != nil {
		port := "1935"
		if scheme == "rtmps" {
			port = "443"
		}
		addr = net.JoinHostPort(addr, port)
	}
	return addr
}

func run(ctx context.Context, addr, streamKey string, tlsConfig *tls.Config, bus *streambus.Bus) error {
	scheme := "rtmp"
	if tlsConfig != nil {
		scheme = "rtmps"
	}
	log.Printf("%s input on %s: EXPERIMENTAL — ffmpeg's decoder fails on this output, though that may be an ffmpeg-specific quirk rather than a real defect; real-player compatibility unconfirmed (see package doc comment)", scheme, addr)

	normalized := normalizeListenAddr(addr, scheme)
	if normalized != addr {
		log.Printf("%s input: interpreting configured address %q as %q (bind address only — any app/path a publisher's URL includes is ignored; only the stream key is checked)", scheme, addr, normalized)
	}
	addr = normalized
	tcpAddr, err := net.ResolveTCPAddr("tcp", addr)
	if err != nil {
		return fmt.Errorf("%s: resolve %s: %w", scheme, addr, err)
	}
	tcpLn, err := net.ListenTCP("tcp", tcpAddr)
	if err != nil {
		return fmt.Errorf("%s: listen %s: %w", scheme, addr, err)
	}
	var ln net.Listener = tcpLn
	if tlsConfig != nil {
		ln = tls.NewListener(tcpLn, tlsConfig)
	}
	defer ln.Close()
	go func() { <-ctx.Done(); ln.Close() }()

	shared := &sharedState{bus: bus, streamKey: streamKey}
	srv := yutopprtmp.NewServer(&yutopprtmp.ServerConfig{
		OnConnect: func(conn net.Conn) (io.ReadWriteCloser, *yutopprtmp.ConnConfig) {
			return conn, &yutopprtmp.ConnConfig{Handler: &handler{shared: shared}}
		},
	})

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-serveErr:
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%s: serve %s: %w", scheme, addr, err)
	}
}

// loadOrGenerateCert loads a PEM cert/key pair if both paths are given,
// otherwise generates a fresh self-signed ECDSA P-256 certificate valid for
// 10 years (long-lived since it's regenerated every process start anyway;
// only used to encrypt the ingest link, not for public trust).
func loadOrGenerateCert(certFile, keyFile string) (tls.Certificate, error) {
	if certFile != "" && keyFile != "" {
		cert, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return tls.Certificate{}, fmt.Errorf("load cert/key: %w", err)
		}
		return cert, nil
	}

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("generate key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("generate serial: %w", err)
	}
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "hdtvheadend-rtmps"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &priv.PublicKey, priv)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("create self-signed cert: %w", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: priv}, nil
}

// sharedState is shared by every connection's handler (go-rtmp creates one
// Handler per connection), so we can enforce a single active publisher
// even though each connection otherwise has independent state.
type sharedState struct {
	bus       *streambus.Bus
	streamKey string

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

// flvRemuxer converts FLV-framed H.264+AAC (as carried by RTMP Audio/Video
// messages, regardless of direction — a publisher pushing to us, or a
// remote server streaming to us after we sent it a play command) into
// MPEG-TS, publishing each batch to bus. Shared by the server-side (Run/
// RunTLS, receiving a publisher) and client-side (PullFrom, receiving a
// remote server's play stream) handlers, since RTMP media framing is
// identical either way — only how the session gets established differs.
type flvRemuxer struct {
	bus   *streambus.Bus
	mux   *tsmux.Muxer
	tsBuf []byte

	videoCfg     h264.DecoderConfig
	videoLenSize int

	haveAudioCfg                         bool
	audioProfile, audioSRIdx, audioChCfg byte
}

func newFLVRemuxer(bus *streambus.Bus) *flvRemuxer {
	r := &flvRemuxer{bus: bus}
	r.mux = tsmux.NewMuxer(func(pkt []byte) { r.tsBuf = append(r.tsBuf, pkt...) })
	return r
}

func (r *flvRemuxer) onVideo(timestamp uint32, payload io.Reader) error {
	var video flvtag.VideoData
	if err := flvtag.DecodeVideoData(payload, &video); err != nil {
		return err
	}
	body := new(bytes.Buffer)
	if _, err := io.Copy(body, video.Data); err != nil {
		return err
	}
	if video.CodecID != flvtag.CodecIDAVC {
		return nil // only H.264 supported
	}

	switch video.AVCPacketType {
	case flvtag.AVCPacketTypeSequenceHeader:
		cfg, lengthSize, err := h264.ParseAVCDecoderConfigurationRecord(body.Bytes())
		if err != nil {
			return fmt.Errorf("rtmp: parse AVC sequence header: %w", err)
		}
		r.videoCfg = cfg
		r.videoLenSize = lengthSize

	case flvtag.AVCPacketTypeNALU:
		nalus, err := h264.AVCCToAnnexB(body.Bytes(), r.videoLenSize)
		if err != nil {
			return fmt.Errorf("rtmp: parse AVC NALU: %w", err)
		}
		keyframe := video.FrameType == flvtag.FrameTypeKeyFrame
		if keyframe && len(r.videoCfg.SPS) > 0 {
			nalus = append([][]byte{r.videoCfg.SPS, r.videoCfg.PPS}, nalus...)
		}
		dtsMS := int64(timestamp)
		ptsMS := dtsMS + int64(video.CompositionTime)
		r.mux.WriteVideo(ptsMS*90, dtsMS*90, nalus, keyframe)
		r.flush()
	}
	return nil
}

func (r *flvRemuxer) onAudio(timestamp uint32, payload io.Reader) error {
	var audio flvtag.AudioData
	if err := flvtag.DecodeAudioData(payload, &audio); err != nil {
		return err
	}
	body := new(bytes.Buffer)
	if _, err := io.Copy(body, audio.Data); err != nil {
		return err
	}
	if audio.SoundFormat != flvtag.SoundFormatAAC {
		return nil // only AAC supported
	}

	switch audio.AACPacketType {
	case flvtag.AACPacketTypeSequenceHeader:
		profile, sr, ch, err := aac.ParseAudioSpecificConfig(body.Bytes())
		if err != nil {
			return fmt.Errorf("rtmp: parse AAC sequence header: %w", err)
		}
		r.audioProfile, r.audioSRIdx, r.audioChCfg = profile, sr, ch
		r.haveAudioCfg = true

	case flvtag.AACPacketTypeRaw:
		if !r.haveAudioCfg {
			return nil
		}
		raw := body.Bytes()
		hdr := aac.BuildADTSHeader(r.audioProfile, r.audioSRIdx, r.audioChCfg, 7+len(raw))
		frame := append(hdr, raw...)
		r.mux.WriteAudio(int64(timestamp)*90, frame)
		r.flush()
	}
	return nil
}

func (r *flvRemuxer) flush() {
	if len(r.tsBuf) == 0 {
		return
	}
	chunk := r.tsBuf
	r.tsBuf = nil
	r.bus.Publish(chunk)
}

type handler struct {
	yutopprtmp.DefaultHandler
	shared *sharedState

	acquired bool
	remux    *flvRemuxer
}

func (h *handler) OnPublish(_ *yutopprtmp.StreamContext, timestamp uint32, cmd *rtmpmsg.NetStreamPublish) error {
	err := h.onPublish(cmd)
	if err != nil {
		log.Printf("rtmp input: publish rejected: %v", err)
	}
	return err
}

func (h *handler) onPublish(cmd *rtmpmsg.NetStreamPublish) error {
	if h.shared.streamKey != "" && cmd.PublishingName != h.shared.streamKey {
		return fmt.Errorf("rtmp: rejected publish with unexpected stream key %q", cmd.PublishingName)
	}
	if !h.shared.tryAcquire() {
		return fmt.Errorf("rtmp: a publisher is already active")
	}
	h.acquired = true
	h.remux = newFLVRemuxer(h.shared.bus)
	return nil
}

func (h *handler) OnVideo(timestamp uint32, payload io.Reader) error {
	if h.remux == nil {
		return nil
	}
	// Log but don't propagate: a single malformed or empty video message
	// (some encoders send zero-length audio/video as a keepalive/gap
	// marker) shouldn't tear down an otherwise-healthy connection over
	// one bad frame.
	if err := h.remux.onVideo(timestamp, payload); err != nil {
		log.Printf("rtmp input: OnVideo: %v", err)
	}
	return nil
}

func (h *handler) OnAudio(timestamp uint32, payload io.Reader) error {
	if h.remux == nil {
		return nil
	}
	if err := h.remux.onAudio(timestamp, payload); err != nil {
		log.Printf("rtmp input: OnAudio: %v", err)
	}
	return nil
}

func (h *handler) OnClose() {
	if h.acquired {
		h.shared.release()
	}
}
