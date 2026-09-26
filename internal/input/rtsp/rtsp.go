// Package rtsp implements a minimal RTSP 1.0 client for pulling an
// MPEG-TS-over-RTP source (RFC 2250, RTP static payload type 33): it
// performs OPTIONS/DESCRIBE/SETUP/PLAY, requests TCP-interleaved delivery
// (no separate UDP port negotiation, so it works through NAT without extra
// firewall config), and republishes the de-RTP'd TS payload to a
// streambus.
package rtsp

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	"hdtvheadend/internal/streambus"
	"hdtvheadend/internal/tsutil"
)

// Run connects to rtspURL and republishes its MPEG-TS-over-RTP payload to
// bus until ctx is canceled.
func Run(ctx context.Context, rtspURL string, bus *streambus.Bus) error {
	u, err := url.Parse(rtspURL)
	if err != nil {
		return fmt.Errorf("rtsp: parse url: %w", err)
	}
	if u.Scheme != "rtsp" {
		return fmt.Errorf("rtsp: unsupported scheme %q", u.Scheme)
	}
	addr := u.Host
	if !strings.Contains(addr, ":") {
		addr += ":554"
	}

	dialer := net.Dialer{}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("rtsp: dial %s: %w", addr, err)
	}
	defer conn.Close()

	go func() {
		<-ctx.Done()
		conn.Close()
	}()

	c := &client{conn: conn, r: bufio.NewReader(conn), base: rtspURL}

	if _, err := c.request("OPTIONS", rtspURL, nil); err != nil {
		return fmt.Errorf("rtsp: OPTIONS: %w", err)
	}

	descResp, err := c.request("DESCRIBE", rtspURL, map[string]string{"Accept": "application/sdp"})
	if err != nil {
		return fmt.Errorf("rtsp: DESCRIBE: %w", err)
	}
	track, payloadType, err := parseSDPFirstMedia(descResp.body, rtspURL)
	if err != nil {
		return fmt.Errorf("rtsp: %w", err)
	}
	_ = payloadType // only MP2T (33) is meaningful here; we don't need to branch on it.

	setupResp, err := c.request("SETUP", track, map[string]string{
		"Transport": "RTP/AVP/TCP;unicast;interleaved=0-1",
	})
	if err != nil {
		return fmt.Errorf("rtsp: SETUP: %w", err)
	}
	session := firstField(setupResp.headers["Session"], ';')
	if session == "" {
		return fmt.Errorf("rtsp: SETUP response missing Session header")
	}
	c.session = session

	if _, err := c.request("PLAY", rtspURL, map[string]string{"Range": "npt=0.000-"}); err != nil {
		return fmt.Errorf("rtsp: PLAY: %w", err)
	}

	var carry []byte
	for {
		channel, payload, err := c.readInterleavedFrame()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("rtsp: read stream: %w", err)
		}
		if channel != 0 { // 0 = RTP data, 1 = RTCP; we only want RTP
			continue
		}
		rtpPayload := stripRTPHeader(payload)
		if len(rtpPayload) == 0 {
			continue
		}
		data := append(carry, rtpPayload...)
		aligned := (len(data) / tsutil.PacketSize) * tsutil.PacketSize
		if aligned > 0 {
			chunk := make([]byte, aligned)
			copy(chunk, data[:aligned])
			bus.Publish(chunk)
		}
		carry = append([]byte(nil), data[aligned:]...)
	}
}

type response struct {
	status  int
	headers map[string]string
	body    []byte
}

type client struct {
	conn    net.Conn
	r       *bufio.Reader
	base    string
	cseq    int
	session string
}

func (c *client) request(method, uri string, extraHeaders map[string]string) (*response, error) {
	c.cseq++
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s RTSP/1.0\r\n", method, uri)
	fmt.Fprintf(&b, "CSeq: %d\r\n", c.cseq)
	if c.session != "" {
		fmt.Fprintf(&b, "Session: %s\r\n", c.session)
	}
	for k, v := range extraHeaders {
		fmt.Fprintf(&b, "%s: %s\r\n", k, v)
	}
	b.WriteString("\r\n")

	if _, err := c.conn.Write([]byte(b.String())); err != nil {
		return nil, err
	}
	return c.readResponse()
}

func (c *client) readResponse() (*response, error) {
	statusLine, err := c.r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	parts := strings.SplitN(strings.TrimSpace(statusLine), " ", 3)
	if len(parts) < 2 {
		return nil, fmt.Errorf("malformed status line %q", statusLine)
	}
	status, _ := strconv.Atoi(parts[1])

	headers := make(map[string]string)
	contentLen := 0
	for {
		line, err := c.r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		kv := strings.SplitN(line, ":", 2)
		if len(kv) != 2 {
			continue
		}
		key := strings.TrimSpace(kv[0])
		val := strings.TrimSpace(kv[1])
		headers[key] = val
		if strings.EqualFold(key, "Content-Length") {
			contentLen, _ = strconv.Atoi(val)
		}
	}

	var body []byte
	if contentLen > 0 {
		body = make([]byte, contentLen)
		if _, err := readFull(c.r, body); err != nil {
			return nil, err
		}
	}

	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("status %d", status)
	}
	return &response{status: status, headers: headers, body: body}, nil
}

// readInterleavedFrame reads one RTSP TCP-interleaved binary frame:
// '$' <channel byte> <2-byte big-endian length> <payload>.
func (c *client) readInterleavedFrame() (channel byte, payload []byte, err error) {
	for {
		magic, err := c.r.ReadByte()
		if err != nil {
			return 0, nil, err
		}
		if magic != '$' {
			continue // skip stray non-interleaved bytes (e.g. trailing response text)
		}
		hdr := make([]byte, 3)
		if _, err := readFull(c.r, hdr); err != nil {
			return 0, nil, err
		}
		length := int(hdr[1])<<8 | int(hdr[2])
		payload := make([]byte, length)
		if _, err := readFull(c.r, payload); err != nil {
			return 0, nil, err
		}
		return hdr[0], payload, nil
	}
}

func readFull(r *bufio.Reader, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := r.Read(buf[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

// stripRTPHeader removes the RTP header (RFC 3550), accounting for CSRC
// identifiers and an optional extension header.
func stripRTPHeader(pkt []byte) []byte {
	if len(pkt) < 12 {
		return nil
	}
	cc := int(pkt[0] & 0x0f)
	off := 12 + cc*4
	if len(pkt) < off {
		return nil
	}
	if pkt[0]&0x10 != 0 { // extension bit
		if len(pkt) < off+4 {
			return nil
		}
		extLen := int(pkt[off+2])<<8 | int(pkt[off+3])
		off += 4 + extLen*4
	}
	if off > len(pkt) {
		return nil
	}
	return pkt[off:]
}

// parseSDPFirstMedia finds the first media section in an SDP body, returns
// its absolute control URL and RTP payload type.
func parseSDPFirstMedia(sdp []byte, baseURL string) (controlURL string, payloadType int, err error) {
	lines := strings.Split(string(sdp), "\n")
	inMedia := false
	control := ""
	pt := -1
	sessionControl := ""

	for _, raw := range lines {
		line := strings.TrimRight(raw, "\r")
		switch {
		case strings.HasPrefix(line, "a=control:") && !inMedia:
			sessionControl = strings.TrimPrefix(line, "a=control:")
		case strings.HasPrefix(line, "m="):
			if inMedia {
				break
			}
			inMedia = true
			fields := strings.Fields(line)
			if len(fields) >= 4 {
				pt, _ = strconv.Atoi(fields[len(fields)-1])
			}
		case inMedia && strings.HasPrefix(line, "a=control:"):
			control = strings.TrimPrefix(line, "a=control:")
		}
	}

	if control == "" {
		control = sessionControl
	}
	if control == "" {
		return "", 0, fmt.Errorf("SDP has no media control attribute")
	}
	if strings.HasPrefix(control, "rtsp://") {
		return control, pt, nil
	}
	sep := "/"
	if strings.HasSuffix(baseURL, "/") {
		sep = ""
	}
	return baseURL + sep + control, pt, nil
}

func firstField(s string, sep byte) string {
	if i := strings.IndexByte(s, sep); i >= 0 {
		return s[:i]
	}
	return s
}
