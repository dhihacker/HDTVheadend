// Package srt implements an SRT output: it republishes a stream's data to
// one SRT destination (Caller mode, dialing out to a remote receiver) or
// serves it to SRT subscribers (Listener mode, accepting incoming pull
// connections). Rendezvous mode is not offered; see the input/srt package
// doc comment for why.
package srt

import (
	"context"
	"fmt"
	"log"
	"time"

	gosrt "github.com/datarhei/gosrt"

	"hdtvheadend/internal/streambus"
	"hdtvheadend/internal/tsutil"
)

func buildConfig(passphrase, streamID string, latencyMS int) gosrt.Config {
	c := gosrt.DefaultConfig()
	c.StreamId = streamID
	if passphrase != "" {
		c.Passphrase = passphrase
		c.EnforcedEncryption = true
	}
	if latencyMS > 0 {
		c.Latency = time.Duration(latencyMS) * time.Millisecond
	}
	return c
}

// Run sends bus's data out over SRT until ctx is canceled.
//
// In Caller mode it dials addr once and pushes to it; on disconnect it
// retries with backoff.
//
// In Listener mode it binds addr and serves every accepted subscriber
// concurrently from the same bus, so multiple SRT players can pull the
// same stream.
func Run(ctx context.Context, addr string, mode, passphrase, streamID string, latencyMS int, bus *streambus.Bus) error {
	cfg := buildConfig(passphrase, streamID, latencyMS)

	switch mode {
	case "listener", "":
		return runListener(ctx, addr, cfg, bus)
	case "caller":
		return runCaller(ctx, addr, cfg, bus)
	default:
		return fmt.Errorf("srt output: unsupported mode %q (supported: caller, listener)", mode)
	}
}

func runCaller(ctx context.Context, addr string, cfg gosrt.Config, bus *streambus.Bus) error {
	backoff := time.Second
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		conn, err := gosrt.Dial("srt", addr, cfg)
		if err != nil {
			log.Printf("srt output: dial %s: %v", addr, err)
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
		serve(ctx, conn, bus)
	}
}

func runListener(ctx context.Context, addr string, cfg gosrt.Config, bus *streambus.Bus) error {
	ln, err := gosrt.Listen("srt", addr, cfg)
	if err != nil {
		return fmt.Errorf("srt output: listen %s: %w", addr, err)
	}
	defer ln.Close()

	go func() {
		<-ctx.Done()
		ln.Close()
	}()

	for {
		req, err := ln.Accept2()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("srt output: accept on %s: %w", addr, err)
		}
		if cfg.StreamId != "" && req.StreamId() != cfg.StreamId {
			req.Reject(gosrt.REJ_PEER)
			continue
		}
		if req.IsEncrypted() {
			if err := req.SetPassphrase(cfg.Passphrase); err != nil {
				req.Reject(gosrt.REJ_BADSECRET)
				continue
			}
		}
		conn, err := req.Accept()
		if err != nil {
			log.Printf("srt output: accept connection from %s: %v", req.RemoteAddr(), err)
			continue
		}
		go serve(ctx, conn, bus)
	}
}

// serve streams bus's chunks to conn, split into standard 7-TS-packet
// (1316 byte) groups, until either the connection fails or ctx is done.
func serve(ctx context.Context, conn gosrt.Conn, bus *streambus.Bus) {
	defer conn.Close()

	ch, unsub := bus.Subscribe(1024)
	defer unsub()

	for {
		select {
		case <-ctx.Done():
			return
		case chunk, ok := <-ch:
			if !ok {
				return
			}
			for off := 0; off < len(chunk); off += 7 * tsutil.PacketSize {
				end := off + 7*tsutil.PacketSize
				if end > len(chunk) {
					end = len(chunk)
				}
				if _, err := conn.Write(chunk[off:end]); err != nil {
					return
				}
			}
		}
	}
}
