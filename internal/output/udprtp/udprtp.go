// Package udprtp implements a UDP / RTP output: it sends a stream's data
// to a unicast or multicast UDP destination, optionally wrapped in RTP.
package udprtp

import (
	"context"
	"fmt"
	"net"
	"syscall"

	"hdtvheadend/internal/streambus"
	"hdtvheadend/internal/tsutil"
)

const rtpPayloadTypeMP2T = 33

// Run sends bus's published chunks to addr until ctx is canceled. If rtp is
// true, each 7-TS-packet group (the standard RTP/MP2T packing, 7*188=1316
// bytes) is wrapped in a minimal RTP header.
func Run(ctx context.Context, addr string, ttl int, rtp bool, bus *streambus.Bus) error {
	udpAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return fmt.Errorf("udprtp output: resolve %s: %w", addr, err)
	}
	conn, err := net.DialUDP("udp", nil, udpAddr)
	if err != nil {
		return fmt.Errorf("udprtp output: dial %s: %w", addr, err)
	}
	defer conn.Close()

	if udpAddr.IP != nil && udpAddr.IP.IsMulticast() && ttl > 0 {
		_ = setMulticastTTL(conn, ttl)
	}

	ch, unsub := bus.Subscribe(1024)
	defer unsub()

	var seq uint16
	var ts uint32
	const ssrc = 0x4e4f5641 // "NOVA"

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case chunk, ok := <-ch:
			if !ok {
				return nil
			}
			if !rtp {
				_, _ = conn.Write(chunk)
				continue
			}
			for off := 0; off < len(chunk); off += 7 * tsutil.PacketSize {
				end := off + 7*tsutil.PacketSize
				if end > len(chunk) {
					end = len(chunk)
				}
				pkt := buildRTP(seq, ts, ssrc, chunk[off:end])
				_, _ = conn.Write(pkt)
				seq++
				ts += 90000 / 25 // approximate 25fps-equivalent 90kHz clock tick
			}
		}
	}
}

func setMulticastTTL(conn *net.UDPConn, ttl int) error {
	f, err := conn.File()
	if err != nil {
		return err
	}
	defer f.Close()
	return syscall.SetsockoptInt(int(f.Fd()), syscall.IPPROTO_IP, syscall.IP_MULTICAST_TTL, ttl)
}

func buildRTP(seq uint16, ts uint32, ssrc uint32, payload []byte) []byte {
	pkt := make([]byte, 12+len(payload))
	pkt[0] = 0x80 // version 2
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
