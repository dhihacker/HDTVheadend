// Package rtsp implements a minimal RTSP 1.0 server for one stream: it
// answers OPTIONS/DESCRIBE/SETUP/PLAY, serves MPEG-TS wrapped in RTP
// (static payload type 33, RFC 2250) over the TCP-interleaved transport
// (no separate UDP port negotiation needed), and supports any number of
// concurrent players.
package rtsp

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"

	"hdtvheadend/internal/streambus"
	"hdtvheadend/internal/tsutil"
)

const rtpPayloadTypeMP2T = 33

// Run listens on addr and serves bus's data to any number of concurrent
// RTSP/TCP-interleaved players until ctx is canceled.
func Run(ctx context.Context, addr string, bus *streambus.Bus) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("rtsp output: listen %s: %w", addr, err)
	}
	defer ln.Close()

	go func() {
		<-ctx.Done()
		ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("rtsp output: accept on %s: %w", addr, err)
		}
		go serveConn(ctx, conn, bus)
	}
}

// syncConn serializes writes to a net.Conn: once PLAY starts, the RTP
// streaming goroutine and the request-handling loop both write to the same
// TCP connection (RTSP interleaves control responses and media data on one
// socket), and net.Conn does not guarantee atomicity across concurrent
// Write calls.
type syncConn struct {
	net.Conn
	mu sync.Mutex
}

func (c *syncConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.Conn.Write(p)
}

func serveConn(ctx context.Context, raw net.Conn, bus *streambus.Bus) {
	conn := &syncConn{Conn: raw}
	defer conn.Close()
	go func() { <-ctx.Done(); conn.Close() }()

	r := bufio.NewReader(conn)
	session := randomSessionID()

	for {
		req, err := readRequest(r)
		if err != nil {
			return
		}

		switch req.method {
		case "OPTIONS":
			writeResponse(conn, req.cseq, 200, "OK", map[string]string{
				"Public": "OPTIONS, DESCRIBE, SETUP, PLAY, TEARDOWN",
			}, nil)
		case "DESCRIBE":
			sdp := buildSDP(req.uri)
			writeResponse(conn, req.cseq, 200, "OK", map[string]string{
				"Content-Base": req.uri + "/",
				"Content-Type": "application/sdp",
			}, sdp)
		case "SETUP":
			writeResponse(conn, req.cseq, 200, "OK", map[string]string{
				"Transport": "RTP/AVP/TCP;unicast;interleaved=0-1",
				"Session":   session,
			}, nil)
		case "PLAY":
			writeResponse(conn, req.cseq, 200, "OK", map[string]string{
				"Session": session,
				"Range":   "npt=0.000-",
			}, nil)
			go stream(ctx, conn, bus)
		case "TEARDOWN":
			writeResponse(conn, req.cseq, 200, "OK", map[string]string{"Session": session}, nil)
			return
		default:
			writeResponse(conn, req.cseq, 501, "Not Implemented", nil, nil)
		}
	}
}

func stream(ctx context.Context, conn net.Conn, bus *streambus.Bus) {
	ch, unsub := bus.Subscribe(1024)
	defer unsub()

	var seq uint16
	var ts uint32
	const ssrc = 0x48445456 // "HDTV"

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
				pkt := buildRTP(seq, ts, ssrc, chunk[off:end])
				frame := make([]byte, 4+len(pkt))
				frame[0] = '$'
				frame[1] = 0 // channel 0 = RTP data
				frame[2] = byte(len(pkt) >> 8)
				frame[3] = byte(len(pkt))
				copy(frame[4:], pkt)
				if _, err := conn.Write(frame); err != nil {
					return
				}
				seq++
				ts += 90000 / 25
			}
		}
	}
}

func buildRTP(seq uint16, ts uint32, ssrc uint32, payload []byte) []byte {
	pkt := make([]byte, 12+len(payload))
	pkt[0] = 0x80
	pkt[1] = rtpPayloadTypeMP2T
	pkt[2] = byte(seq >> 8)
	pkt[3] = byte(seq)
	pkt[4] = byte(ts >> 24)
	pkt[5] = byte(ts >> 16)
	pkt[6] = byte(ts >> 8)
	pkt[7] = byte(ts)
	pkt[8] = byte(ssrc >> 24)
	pkt[9] = byte(ssrc >> 16)
	pkt[10] = byte(ssrc >> 8)
	pkt[11] = byte(ssrc)
	copy(pkt[12:], payload)
	return pkt
}

func buildSDP(uri string) []byte {
	sdp := "v=0\r\n" +
		"o=- 0 0 IN IP4 0.0.0.0\r\n" +
		"s=HDTVheadend\r\n" +
		"c=IN IP4 0.0.0.0\r\n" +
		"t=0 0\r\n" +
		"a=control:*\r\n" +
		"m=video 0 RTP/AVP " + strconv.Itoa(rtpPayloadTypeMP2T) + "\r\n" +
		"a=control:trackID=0\r\n"
	return []byte(sdp)
}

type request struct {
	method  string
	uri     string
	cseq    string
	headers map[string]string
}

func readRequest(r *bufio.Reader) (*request, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return nil, fmt.Errorf("malformed request line %q", line)
	}
	req := &request{method: fields[0], uri: fields[1], headers: make(map[string]string)}

	for {
		hl, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		hl = strings.TrimRight(hl, "\r\n")
		if hl == "" {
			break
		}
		kv := strings.SplitN(hl, ":", 2)
		if len(kv) != 2 {
			continue
		}
		key := strings.TrimSpace(kv[0])
		val := strings.TrimSpace(kv[1])
		req.headers[key] = val
		if strings.EqualFold(key, "CSeq") {
			req.cseq = val
		}
	}
	return req, nil
}

func writeResponse(conn net.Conn, cseq string, code int, reason string, headers map[string]string, body []byte) {
	var b strings.Builder
	fmt.Fprintf(&b, "RTSP/1.0 %d %s\r\n", code, reason)
	fmt.Fprintf(&b, "CSeq: %s\r\n", cseq)
	for k, v := range headers {
		if v == "" {
			continue
		}
		fmt.Fprintf(&b, "%s: %s\r\n", k, v)
	}
	if len(body) > 0 {
		fmt.Fprintf(&b, "Content-Length: %d\r\n", len(body))
	}
	b.WriteString("\r\n")
	_, _ = conn.Write([]byte(b.String()))
	if len(body) > 0 {
		_, _ = conn.Write(body)
	}
}

func randomSessionID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
