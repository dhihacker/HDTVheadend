// Package config defines HDTVheadend's on-disk configuration: the admin
// account, listen address, and the set of configured streams (an input,
// optional conditional access, and one or more outputs).
package config

import (
	"encoding/json"
	"os"
)

type InputType string

const (
	InputHTTPTS InputType = "httpts"
	InputUDP    InputType = "udp"
	InputRTP    InputType = "rtp"
	InputHLS    InputType = "hls"
	InputDASH   InputType = "dash"
	InputDVB    InputType = "dvb"
	InputSRT    InputType = "srt"
	InputRTSP   InputType = "rtsp"
	InputRTMP   InputType = "rtmp"
	InputRTMPS  InputType = "rtmps" // RTMP ingest server over TLS

	// InputRTMPPull is the opposite of InputRTMP/InputRTMPS: instead of
	// running a server and waiting for an encoder to push to us, it
	// connects OUT as a client to an existing RTMP stream already being
	// served elsewhere (e.g. a VPS restream target) and plays it. Reuses
	// URL, not Addr — e.g. "rtmp://host[:port]/app/streamname" or
	// "rtmps://..." (TLS is selected by the URL's own scheme, not a
	// separate input type).
	InputRTMPPull InputType = "rtmp_pull"

	// InputYouTube is functionally identical to InputHLS (reuses URL) —
	// HDTVheadend does not scrape YouTube or work around its bot
	// detection. The operator extracts a live stream's .m3u8 URL
	// themselves (e.g. from their browser's network tab, for a stream
	// they own or are authorized to redistribute) and pastes it in; this
	// is just a clearly-labeled, discoverable alias so that path doesn't
	// require knowing HLS's own input type exists.
	InputYouTube InputType = "youtube"

	// InputWebRTC accepts a WHIP (WebRTC-HTTP Ingestion Protocol) publisher
	// — an encoder like OBS 30+, or a browser, POSTs an SDP offer to Path
	// on this server's own HTTP port. Video (H.264) only: WebRTC's
	// mandatory audio codec is Opus, which HDTVheadend's AAC-based TS
	// pipeline can't carry without transcoding (not implemented — no audio
	// codec support exists in this binary at all). Any audio track is
	// accepted in the SDP but its data is discarded.
	InputWebRTC InputType = "webrtc"
)

// StreamMode selects when a stream's input actually runs.
type StreamMode string

const (
	// StreamModeStatic (the default, including the empty/unset value for
	// backward compatibility with existing config files) starts the input
	// as soon as the stream is enabled and keeps it running continuously.
	StreamModeStatic StreamMode = "static"

	// StreamModeOnDemand starts the input only once something actually
	// wants the stream's output — a viewer connects (HTTP-TS, WHEP), or a
	// playlist/segment/manifest is requested (HLS/DASH) — and stops it
	// again after a period of no such demand. This saves the bandwidth/
	// connection cost of continuously pulling a source (HLS, RTSP, SRT
	// caller, UDP, DVB tuner) nobody is currently watching.
	//
	// Push-style outputs (UDP, SRT, RTSP, RTMP) have no natural per-viewer
	// "demand" signal to hook, so configuring one alongside on-demand mode
	// simply keeps the input running continuously for as long as that
	// output is configured — the same as static mode, just for that
	// stream. Similarly, a WHIP (WebRTC) input is a listener, not
	// something this binary pulls from, so it always runs regardless of
	// Mode; on-demand only affects pull-style inputs.
	StreamModeOnDemand StreamMode = "on_demand"
)

type OutputType string

const (
	OutputHTTPTS OutputType = "httpts"
	OutputUDP    OutputType = "udp"
	OutputSRT    OutputType = "srt"
	OutputRTSP   OutputType = "rtsp"
	OutputHLS    OutputType = "hls"
	OutputDASH   OutputType = "dash"
	OutputRTMP   OutputType = "rtmp"

	// OutputWebRTC serves WHEP (WebRTC-HTTP Egress Protocol): a browser
	// POSTs an SDP offer to Path to start watching. Video (H.264) only,
	// same reason as InputWebRTC.
	OutputWebRTC OutputType = "webrtc"
)

// SRTMode selects which side initiates the SRT connection, matching the
// Haivision SRT terminology. Rendezvous is intentionally not offered: the
// only pure-Go SRT implementation available (datarhei/gosrt) doesn't
// implement it, and the alternative (linking the official C libsrt via
// cgo) would break the single-static-binary, no-external-deps design.
type SRTMode string

const (
	SRTCaller   SRTMode = "caller"
	SRTListener SRTMode = "listener"
)

type CAType string

const (
	CANone            CAType = ""
	CABISSSessionWord CAType = "biss_sw" // 12 hex digit BISS-1 session word
	CABISSControlWord CAType = "biss_cw" // 16 hex digit BISS-E/CA control word
	CACIPlus          CAType = "ci_plus" // hardware CI/CI+ CAM, real licensed card
)

// DVBDeliverySystem selects the tuner frontend mode.
type DVBDeliverySystem string

const (
	DVBS  DVBDeliverySystem = "DVBS"
	DVBS2 DVBDeliverySystem = "DVBS2"
	DVBT  DVBDeliverySystem = "DVBT"
	DVBT2 DVBDeliverySystem = "DVBT2"
	DVBC  DVBDeliverySystem = "DVBC"
)

// Input describes where a stream's transport-stream data comes from.
type Input struct {
	Type InputType `json:"type"`

	// httpts / hls / dash
	URL string `json:"url,omitempty"`

	// udp / rtp
	Addr  string `json:"addr,omitempty"` // "0.0.0.0:5000" unicast, "239.1.1.1:5000" multicast
	Iface string `json:"iface,omitempty"`

	// dvb
	DVBAdapter   int               `json:"dvb_adapter,omitempty"`
	DVBFrontend  int               `json:"dvb_frontend,omitempty"`
	DVBSystem    DVBDeliverySystem `json:"dvb_system,omitempty"`
	FrequencyKHz uint32            `json:"frequency_khz,omitempty"`
	SymbolRateKS uint32            `json:"symbol_rate_ks,omitempty"` // DVB-S/S2/C
	Polarization string            `json:"polarization,omitempty"`   // DVB-S/S2: H, V, L, R
	Modulation   string            `json:"modulation,omitempty"`
	Bandwidth    uint32            `json:"bandwidth_hz,omitempty"` // DVB-T/T2/C
	ServiceID    int               `json:"service_id,omitempty"`   // 0 = whole mux

	// srt: Addr is the remote host:port to dial (caller) or local host:port
	// to bind (listener).
	SRTMode       SRTMode `json:"srt_mode,omitempty"`
	SRTPassphrase string  `json:"srt_passphrase,omitempty"` // 10-80 chars, enables AES encryption
	SRTStreamID   string  `json:"srt_stream_id,omitempty"`
	SRTLatencyMS  int     `json:"srt_latency_ms,omitempty"` // 0 = library default (120ms)

	// rtsp: reuses URL, e.g. "rtsp://host:port/path"

	// rtmp: reuses Addr as the local "host:port" to listen on (RTMP input
	// is always an ingest server — an encoder pushes to us).
	//
	// EXPERIMENTAL: ffmpeg's decoder fails on this input's output TS despite
	// passing every structural validation we have, but the same symptom
	// shows up on unrelated real broadcast content too — likely an
	// ffmpeg-specific decoder quirk, not proof of a real defect here. Real
	// player compatibility is unconfirmed either way (see
	// internal/input/rtmp's package doc comment). RTMP output is unaffected.
	RTMPStreamKey string `json:"rtmp_stream_key,omitempty"` // optional; empty = accept any publishing name

	// RTMPApp is purely cosmetic: it only affects the example ingest URL
	// shown in the dashboard's Status view (e.g. "live" displays as
	// rtmp://host/live/key). The server doesn't check a publisher's app
	// name at all — only RTMPStreamKey — so this can be renamed freely
	// without needing to match whatever a publisher actually sends.
	RTMPApp string `json:"rtmp_app,omitempty"`

	// rtmps: same as rtmp, plus TLS. TLSCertFile/TLSKeyFile are optional —
	// if either is empty, a self-signed certificate is generated in memory
	// at startup (fine for encrypting the ingest link; publishers must
	// disable certificate verification, as OBS and most encoders let you
	// do for a custom RTMPS target). Supply real files for a certificate
	// publishers will actually trust.
	TLSCertFile string `json:"tls_cert_file,omitempty"`
	TLSKeyFile  string `json:"tls_key_file,omitempty"`

	// webrtc: the HTTP path (on this server's normal listen address) a
	// WHIP publisher POSTs its SDP offer to, e.g. "/whip/mystream".
	Path string `json:"path,omitempty"`
}

// CA describes the conditional-access method applied to a stream's input,
// before it is fanned out to outputs.
type CA struct {
	Type CAType `json:"type,omitempty"`
	Key  string `json:"key,omitempty"` // hex session/control word for BISS
}

// Output describes one destination a stream is republished to.
type Output struct {
	Type OutputType `json:"type"`
	Path string     `json:"path,omitempty"` // httpts: served at http://host:port/<path>; hls/dash: served under http://host:port/<path>/
	Addr string     `json:"addr,omitempty"` // udp/srt/rtsp: destination or listen "ip:port"
	TTL  int        `json:"ttl,omitempty"`  // udp multicast TTL
	RTP  bool       `json:"rtp,omitempty"`  // udp: wrap payload in RTP

	// srt
	SRTMode       SRTMode `json:"srt_mode,omitempty"`
	SRTPassphrase string  `json:"srt_passphrase,omitempty"`
	SRTStreamID   string  `json:"srt_stream_id,omitempty"`
	SRTLatencyMS  int     `json:"srt_latency_ms,omitempty"`

	// hls / dash
	SegmentSeconds int `json:"segment_seconds,omitempty"` // default 4
	SegmentCount   int `json:"segment_count,omitempty"`   // playlist window, default 6

	// rtmp: reuses URL for the full target, e.g.
	// "rtmp://a.rtmp.youtube.com/live2/xxxx-xxxx-xxxx-xxxx" (stream key is
	// the last path segment). Output pushes out as an RTMP client; there's
	// no "server, accept a player" RTMP output mode.
	URL string `json:"url,omitempty"`
}

// Stream is one channel: a primary input (with optional ordered backups for
// failover), optional CA, one or more outputs.
type Stream struct {
	ID           string     `json:"id"`
	Name         string     `json:"name"`
	Group        string     `json:"group,omitempty"`
	LogoURL      string     `json:"logo_url,omitempty"`
	EPGID        string     `json:"epg_id,omitempty"`
	Input        Input      `json:"input"`
	BackupInputs []Input    `json:"backup_inputs,omitempty"` // tried in order if the primary (or current) source fails
	CA           CA         `json:"ca,omitempty"`
	Outputs      []Output   `json:"outputs"`
	Mode         StreamMode `json:"mode,omitempty"` // "" / "static" (default) or "on_demand"
	Enabled      bool       `json:"enabled"`
}

type Admin struct {
	Username     string `json:"username"`
	PasswordHash string `json:"password_hash"`
}

type Config struct {
	ListenAddr string   `json:"listen_addr"`
	AllowedIPs []string `json:"allowed_ips,omitempty"`
	Admin      Admin    `json:"admin"`
	EPGSources []string `json:"epg_sources,omitempty"`
	Streams    []Stream `json:"streams"`

	// Observability: both are optional and off (empty) by default. These
	// are separate servers the operator runs and points HDTVheadend at;
	// nothing about VictoriaMetrics/VictoriaLogs is bundled in this binary.
	VictoriaMetricsURL string `json:"victoria_metrics_url,omitempty"` // base URL, e.g. http://vm:8428
	VictoriaLogsURL    string `json:"victoria_logs_url,omitempty"`    // base URL, e.g. http://vl:9428
}

func Default() *Config {
	return &Config{
		ListenAddr: ":8088",
		Streams:    []Stream{},
	}
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) Save(path string) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}
