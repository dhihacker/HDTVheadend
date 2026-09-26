// Package udprtp implements UDP and RTP transport-stream inputs, unicast
// or multicast.
package udprtp

import (
	"context"
	"fmt"
	"net"

	"hdtvheadend/internal/streambus"
)

// Run listens on addr (host:port; a multicast group address joins that
// group, optionally on a named interface) and republishes received TS
// payloads to bus. RTP framing (a 12-byte header starting with version 2,
// i.e. top two bits of the first byte == 0b10) is detected and stripped
// automatically per-packet, so this works for both plain UDP-TS and
// RTP/UDP-TS sources without separate configuration.
func Run(ctx context.Context, addr, iface string, bus *streambus.Bus) error {
	udpAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return fmt.Errorf("udprtp: resolve %s: %w", addr, err)
	}

	var conn *net.UDPConn
	if udpAddr.IP != nil && udpAddr.IP.IsMulticast() {
		var ifi *net.Interface
		if iface != "" {
			ifi, err = net.InterfaceByName(iface)
			if err != nil {
				return fmt.Errorf("udprtp: interface %s: %w", iface, err)
			}
		}
		conn, err = net.ListenMulticastUDP("udp", ifi, udpAddr)
	} else {
		conn, err = net.ListenUDP("udp", udpAddr)
	}
	if err != nil {
		return fmt.Errorf("udprtp: listen %s: %w", addr, err)
	}
	defer conn.Close()

	go func() {
		<-ctx.Done()
		conn.Close()
	}()

	buf := make([]byte, 65535)
	for {
		n, err := conn.Read(buf)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("udprtp: read %s: %w", addr, err)
		}
		if n == 0 {
			continue
		}
		payload := buf[:n]
		if isRTP(payload) {
			payload = payload[12:]
		}
		if len(payload) == 0 {
			continue
		}
		chunk := make([]byte, len(payload))
		copy(chunk, payload)
		bus.Publish(chunk)
	}
}

func isRTP(p []byte) bool {
	return len(p) > 12 && p[0]&0xc0 == 0x80 && p[1]&0x7f == 33 // payload type 33 = MP2T
}
