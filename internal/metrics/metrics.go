// Package metrics exposes HDTVheadend's live stream/tuner/client counters
// as Prometheus metrics, refreshed on each scrape from a StatsSource (the
// same data the web UI's /api/status uses).
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"net/http"
)

// StreamStat mirrors webui.StreamStat; duplicated here (rather than
// importing internal/webui) to keep this package dependency-free of the
// HTTP/session machinery — it only needs the numbers.
type StreamStat struct {
	ID           string
	Running      bool
	Clients      int
	InputBps     float64
	OutputBps    float64
	ActiveSource int
	SourcePinned bool
}

// StatsSource is the live data this package renders as Prometheus metrics.
type StatsSource interface {
	StreamStats() []StreamStat
}

type collector struct {
	source StatsSource

	running      *prometheus.Desc
	clients      *prometheus.Desc
	inputBps     *prometheus.Desc
	outputBps    *prometheus.Desc
	activeSource *prometheus.Desc
	sourcePinned *prometheus.Desc
}

// NewHandler returns an http.Handler serving Prometheus text-format
// metrics for /metrics, sourced live from source on every scrape.
func NewHandler(source StatsSource) http.Handler {
	c := &collector{
		source: source,
		running: prometheus.NewDesc("hdtvheadend_stream_running", "1 if the stream's input is currently running, 0 otherwise.",
			[]string{"stream_id"}, nil),
		clients: prometheus.NewDesc("hdtvheadend_stream_clients", "Number of clients currently connected to this stream's outputs.",
			[]string{"stream_id"}, nil),
		inputBps: prometheus.NewDesc("hdtvheadend_stream_input_bytes_per_second", "Live input (source) rate for this stream, in bytes/sec.",
			[]string{"stream_id"}, nil),
		outputBps: prometheus.NewDesc("hdtvheadend_stream_output_bytes_per_second", "Live aggregate output rate for this stream across all current client/destination connections, in bytes/sec.",
			[]string{"stream_id"}, nil),
		activeSource: prometheus.NewDesc("hdtvheadend_stream_active_source", "Index of the currently active input source (0=primary, 1+=backups).",
			[]string{"stream_id"}, nil),
		sourcePinned: prometheus.NewDesc("hdtvheadend_stream_source_pinned", "1 if the active source was manually pinned rather than chosen automatically.",
			[]string{"stream_id"}, nil),
	}
	reg := prometheus.NewRegistry()
	reg.MustRegister(c)
	return promhttp.HandlerFor(reg, promhttp.HandlerOpts{})
}

func (c *collector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.running
	ch <- c.clients
	ch <- c.inputBps
	ch <- c.outputBps
	ch <- c.activeSource
	ch <- c.sourcePinned
}

func (c *collector) Collect(ch chan<- prometheus.Metric) {
	for _, s := range c.source.StreamStats() {
		running := 0.0
		if s.Running {
			running = 1.0
		}
		pinned := 0.0
		if s.SourcePinned {
			pinned = 1.0
		}
		ch <- prometheus.MustNewConstMetric(c.running, prometheus.GaugeValue, running, s.ID)
		ch <- prometheus.MustNewConstMetric(c.clients, prometheus.GaugeValue, float64(s.Clients), s.ID)
		ch <- prometheus.MustNewConstMetric(c.inputBps, prometheus.GaugeValue, s.InputBps, s.ID)
		ch <- prometheus.MustNewConstMetric(c.outputBps, prometheus.GaugeValue, s.OutputBps, s.ID)
		if s.Running {
			ch <- prometheus.MustNewConstMetric(c.activeSource, prometheus.GaugeValue, float64(s.ActiveSource), s.ID)
			ch <- prometheus.MustNewConstMetric(c.sourcePinned, prometheus.GaugeValue, pinned, s.ID)
		}
	}
}
