// Package httpts implements an HTTP-TS input: it pulls a continuous MPEG-TS
// stream from an upstream HTTP(S) URL and republishes each aligned packet
// group to a streambus.
package httpts

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"hdtvheadend/internal/streambus"
	"hdtvheadend/internal/tsutil"
)

// Run connects to url and streams TS data into bus until ctx is canceled.
// On a connection error it retries with backoff, so callers can just run
// this in a goroutine for the lifetime of the stream.
func Run(ctx context.Context, url string, bus *streambus.Bus) error {
	backoff := time.Second
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if err := pull(ctx, url, bus); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
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

func pull(ctx context.Context, url string, bus *streambus.Bus) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("httpts: upstream %s returned %s", url, resp.Status)
	}

	var carry []byte
	buf := make([]byte, 64*tsutil.PacketSize)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			data := append(carry, buf[:n]...)
			aligned := (len(data) / tsutil.PacketSize) * tsutil.PacketSize
			if aligned > 0 {
				chunk := make([]byte, aligned)
				copy(chunk, data[:aligned])
				bus.Publish(chunk)
			}
			carry = append([]byte(nil), data[aligned:]...)
		}
		if err != nil {
			if err == io.EOF {
				return fmt.Errorf("httpts: upstream %s closed connection", url)
			}
			return err
		}
	}
}
