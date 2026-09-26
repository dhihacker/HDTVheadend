// Package srt implements an SRT (Secure Reliable Transport) transport-stream
// input, in either Caller or Listener mode, using the pure-Go datarhei/gosrt
// implementation. Rendezvous mode is not offered: gosrt doesn't implement
// it, and the only alternative (linking the official C libsrt via cgo)
// would break the single-static-binary, no-external-dependency design.
package srt

import (
	"context"
	"fmt"
	"time"

	gosrt "github.com/datarhei/gosrt"

	"hdtvheadend/internal/config"
	"hdtvheadend/internal/streambus"
	"hdtvheadend/internal/tsutil"
)

func buildConfig(passphrase, streamID string, latencyMS int) gosrt.Config {
	c := gosrt.DefaultConfig()
	c.StreamId = streamID
	if passphrase != "" {
		c.Passphrase = passphrase
		c.EnforcedEncryption = true
	} else {
		c.EnforcedEncryption = false
	}
	if latencyMS > 0 {
		c.Latency = time.Duration(latencyMS) * time.Millisecond
	}
	return c
}

// Run pulls an MPEG-TS stream over SRT and republishes aligned TS packets
// to bus until ctx is canceled. In Caller mode it dials out to in.Addr; in
// Listener mode it binds in.Addr and accepts exactly one publisher,
// validating StreamId/passphrase if configured.
func Run(ctx context.Context, in config.Input, bus *streambus.Bus) error {
	cfg := buildConfig(in.SRTPassphrase, in.SRTStreamID, in.SRTLatencyMS)

	conn, closeAll, err := connect(ctx, in, cfg)
	if err != nil {
		return err
	}
	defer closeAll()

	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			closeAll()
		case <-done:
		}
	}()

	var carry []byte
	buf := make([]byte, 64*1024)
	for {
		n, err := conn.Read(buf)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("srt: read %s: %w", in.Addr, err)
		}
		if n == 0 {
			continue
		}
		data := append(carry, buf[:n]...)
		aligned := (len(data) / tsutil.PacketSize) * tsutil.PacketSize
		if aligned > 0 {
			chunk := make([]byte, aligned)
			copy(chunk, data[:aligned])
			bus.Publish(chunk)
		}
		carry = append([]byte(nil), data[aligned:]...)
	}
}

// connect establishes the SRT connection and returns it along with a
// closeAll func that tears down both the connection and (in Listener mode)
// its backing listener. The listener is deliberately kept open for the
// life of the stream rather than closed right after Accept2: gosrt's
// Listener.Close() closes every connection it currently has established,
// so closing it immediately after accepting would kill the very
// connection just accepted.
func connect(ctx context.Context, in config.Input, cfg gosrt.Config) (gosrt.Conn, func(), error) {
	switch in.SRTMode {
	case config.SRTListener:
		ln, err := gosrt.Listen("srt", in.Addr, cfg)
		if err != nil {
			return nil, nil, fmt.Errorf("srt: listen %s: %w", in.Addr, err)
		}

		acceptDone := make(chan struct{})
		go func() {
			select {
			case <-ctx.Done():
				ln.Close()
			case <-acceptDone:
			}
		}()

		req, err := ln.Accept2()
		close(acceptDone)
		if err != nil {
			ln.Close()
			return nil, nil, fmt.Errorf("srt: accept on %s: %w", in.Addr, err)
		}

		if in.SRTStreamID != "" && req.StreamId() != in.SRTStreamID {
			req.Reject(gosrt.REJ_PEER)
			ln.Close()
			return nil, nil, fmt.Errorf("srt: rejected connection from %s: unexpected stream id %q", req.RemoteAddr(), req.StreamId())
		}
		if req.IsEncrypted() {
			if err := req.SetPassphrase(in.SRTPassphrase); err != nil {
				req.Reject(gosrt.REJ_BADSECRET)
				ln.Close()
				return nil, nil, fmt.Errorf("srt: passphrase rejected from %s: %w", req.RemoteAddr(), err)
			}
		}
		conn, err := req.Accept()
		if err != nil {
			ln.Close()
			return nil, nil, fmt.Errorf("srt: accept connection from %s: %w", req.RemoteAddr(), err)
		}
		return conn, func() { conn.Close(); ln.Close() }, nil

	case config.SRTCaller, "":
		conn, err := gosrt.Dial("srt", in.Addr, cfg)
		if err != nil {
			return nil, nil, fmt.Errorf("srt: dial %s: %w", in.Addr, err)
		}
		return conn, func() { conn.Close() }, nil

	default:
		return nil, nil, fmt.Errorf("srt: unsupported mode %q (supported: caller, listener)", in.SRTMode)
	}
}
