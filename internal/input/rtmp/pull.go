package rtmp

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net"
	"net/url"
	"path"
	"strings"
	"time"

	yutopprtmp "github.com/yutopp/go-rtmp"
	rtmpmsg "github.com/yutopp/go-rtmp/message"

	"hdtvheadend/internal/streambus"
)

// PullFrom connects to a remote RTMP server as a client and plays
// rtmpURL's stream — the "consume a stream that's already being served
// somewhere else" counterpart to Run/RunTLS, which instead run a server
// and accept an incoming push. rtmpURL's last path segment is the stream
// name sent in the play command (e.g. "rtmp://host[:port]/app/name" —
// same app/name convention as this package's own output side; the app
// segment is passed through to the remote server's connect handshake but
// otherwise doesn't matter to us). Remuxes the received H.264+AAC content
// into MPEG-TS and publishes it to bus until ctx is canceled; on
// disconnect it retries with backoff.
func PullFrom(ctx context.Context, rtmpURL string, bus *streambus.Bus) error {
	u, err := url.Parse(rtmpURL)
	if err != nil {
		return fmt.Errorf("rtmp input: parse url: %w", err)
	}
	useTLS := false
	switch u.Scheme {
	case "rtmp":
	case "rtmps":
		useTLS = true
	default:
		return fmt.Errorf("rtmp input: unsupported scheme %q", u.Scheme)
	}
	dir, streamName := path.Split(strings.TrimPrefix(u.Path, "/"))
	app := strings.TrimSuffix(dir, "/")
	if streamName == "" {
		return fmt.Errorf("rtmp input: url must end with a stream name, e.g. rtmp://host/app/streamname")
	}
	hostport := withDefaultRTMPPort(u.Host, useTLS)

	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := pullOnce(ctx, hostport, useTLS, app, streamName, rtmpURL, bus); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			log.Printf("rtmp input (pull %s): %v", rtmpURL, err)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second
	}
}

// withDefaultRTMPPort appends the scheme's conventional default port (1935
// for RTMP, 443 for RTMPS) if hostport didn't specify one.
func withDefaultRTMPPort(hostport string, useTLS bool) string {
	if _, _, err := net.SplitHostPort(hostport); err == nil {
		return hostport
	}
	if useTLS {
		return net.JoinHostPort(hostport, "443")
	}
	return net.JoinHostPort(hostport, "1935")
}

// connectTimeout bounds how long the dial+connect+createStream+play
// handshake sequence is allowed to take. It matters more than a typical
// dial timeout would: the underlying library's Connect/CreateStream/Play
// calls block on a transaction-completion channel with no deadline of
// their own (context.TODO() internally) and, critically, closing the
// connection out from under a blocked call does NOT unblock it — the
// read loop that would normally resolve that channel just exits on a
// close, and nothing else ever signals it. Without this timeout, a
// remote that accepts the TCP connection but then stalls partway through
// the handshake would hang this goroutine forever, which in turn would
// hang Handle.Stop's wait for it, which runs holding the app's config
// lock — freezing the entire dashboard, not just this one stream.
const connectTimeout = 15 * time.Second

func pullOnce(ctx context.Context, hostport string, useTLS bool, app, streamName, tcURL string, bus *streambus.Bus) error {
	remux := newFLVRemuxer(bus)
	cfg := &yutopprtmp.ConnConfig{Handler: &pullHandler{remux: remux}}

	type connectResult struct {
		client *yutopprtmp.ClientConn
		stream *yutopprtmp.Stream
		err    error
	}
	resultCh := make(chan connectResult, 1)

	// Run the whole handshake in its own goroutine so this function can
	// give up on it (see connectTimeout above) without waiting for calls
	// that may never return. If it does eventually resolve after we've
	// already given up, the resulting connection (or the dial's own
	// error) is simply discarded — a bounded, harmless leak that ends
	// once the remote errors out on its own or the process exits, far
	// better than freezing the whole app waiting on it.
	go func() {
		var client *yutopprtmp.ClientConn
		var err error
		if useTLS {
			client, err = yutopprtmp.TLSDial("rtmps", hostport, cfg, &tls.Config{})
		} else {
			client, err = yutopprtmp.Dial("rtmp", hostport, cfg)
		}
		if err != nil {
			resultCh <- connectResult{err: fmt.Errorf("dial %s: %w", hostport, err)}
			return
		}

		if err := client.Connect(&rtmpmsg.NetConnectionConnect{Command: rtmpmsg.NetConnectionConnectCommand{
			App: app, TCURL: tcURL, FlashVer: "FMLE/3.0 (compatible; HDTVheadend)",
		}}); err != nil {
			client.Close()
			resultCh <- connectResult{err: fmt.Errorf("connect: %w", err)}
			return
		}

		stream, err := client.CreateStream(nil, 4096)
		if err != nil {
			client.Close()
			resultCh <- connectResult{err: fmt.Errorf("create stream: %w", err)}
			return
		}

		// Must happen before Play, not after: the connection's message-
		// read loop runs concurrently from Dial onward, so a fast server
		// can start sending Audio/Video the instant it processes our
		// play command — before we'd get back here to prepare the
		// handler, causing the stream's still-nil message handler to
		// panic. Only reproduces against a real, low-latency server; a
		// slow test double can mask it entirely.
		stream.PrepareForClientPlay()
		if err := stream.Play(&rtmpmsg.NetStreamPlay{StreamName: streamName}); err != nil {
			client.Close()
			resultCh <- connectResult{err: fmt.Errorf("play: %w", err)}
			return
		}

		resultCh <- connectResult{client: client, stream: stream}
	}()

	var res connectResult
	select {
	case res = <-resultCh:
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(connectTimeout):
		return fmt.Errorf("connect/play handshake timed out after %s (remote accepted the connection but never completed it)", connectTimeout)
	}
	if res.err != nil {
		return res.err
	}
	client := res.client
	defer client.Close()

	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			client.Close()
		case <-done:
		}
	}()

	// The client library has no "connection closed" signal for a purely
	// receiving session (nothing here writes continuously the way the
	// output side does, so a write error can't be used to detect
	// disconnection) — poll ClientConn.LastError, which its internal read
	// loop sets once the connection drops for any reason.
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := client.LastError(); err != nil {
				return fmt.Errorf("connection lost: %w", err)
			}
		}
	}
}

// pullHandler receives Audio/Video messages from a remote server we've
// asked to play a stream to us. Unlike the server-side handler, there's no
// publish handshake to gate on and no shared single-publisher state to
// enforce — remux is ready to receive from the moment we send play.
type pullHandler struct {
	yutopprtmp.DefaultHandler
	remux *flvRemuxer
}

func (h *pullHandler) OnVideo(timestamp uint32, payload io.Reader) error {
	// Log but don't propagate: a single malformed or empty video/audio
	// message (some encoders send zero-length audio as a keepalive/gap
	// marker) shouldn't tear down an otherwise-healthy pulled connection
	// over one bad frame.
	if err := h.remux.onVideo(timestamp, payload); err != nil {
		log.Printf("rtmp input (pull): OnVideo: %v", err)
	}
	return nil
}

func (h *pullHandler) OnAudio(timestamp uint32, payload io.Reader) error {
	if err := h.remux.onAudio(timestamp, payload); err != nil {
		log.Printf("rtmp input (pull): OnAudio: %v", err)
	}
	return nil
}
