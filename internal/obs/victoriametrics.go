// Package obs ships HDTVheadend's own metrics and logs to an
// operator-provided VictoriaMetrics/VictoriaLogs instance. Neither service
// is bundled with HDTVheadend: both are external servers the operator runs
// and points this binary at via config; shipping is a no-op when the
// corresponding URL is unset.
package obs

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// ShipMetrics periodically scrapes localMetricsURL (this process's own
// /metrics endpoint) and pushes the result to a VictoriaMetrics instance's
// Prometheus text import endpoint, until ctx is canceled. It never
// returns an error: a failed push just logs (via onErr) and retries on the
// next tick, so a transient VictoriaMetrics outage doesn't affect the
// stream pipeline.
func ShipMetrics(ctx context.Context, vmBaseURL, localMetricsURL string, interval time.Duration, onErr func(error)) {
	if vmBaseURL == "" {
		return
	}
	if interval <= 0 {
		interval = 10 * time.Second
	}
	target := strings.TrimSuffix(vmBaseURL, "/") + "/api/v1/import/prometheus"

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := pushOnce(ctx, localMetricsURL, target); err != nil && onErr != nil {
				onErr(err)
			}
		}
	}
}

func pushOnce(ctx context.Context, localMetricsURL, target string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, localMetricsURL, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("scrape local metrics: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("scrape local metrics: status %s", resp.Status)
	}

	pushReq, err := http.NewRequestWithContext(ctx, http.MethodPost, target, resp.Body)
	if err != nil {
		return err
	}
	pushResp, err := http.DefaultClient.Do(pushReq)
	if err != nil {
		return fmt.Errorf("push to victoriametrics: %w", err)
	}
	defer pushResp.Body.Close()
	if pushResp.StatusCode/100 != 2 {
		return fmt.Errorf("push to victoriametrics: status %s", pushResp.Status)
	}
	return nil
}
