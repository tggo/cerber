// Package metrics exposes cerber's metrics in Prometheus format. Two sources are
// merged on one private registry (no global state):
//
//   - Collector — a snapshot bridge over the in-memory usage.Tracker (cumulative
//     per-credential and per-model counters), read on each scrape so the tracker
//     stays the single source of truth for usage.
//   - Metrics — live instruments updated as requests flow: HTTP latency/status
//     histograms+counters and per-provider concurrency gauges (in-flight count,
//     queue depth, queue-wait time) for providers with a concurrency cap.
package metrics

import (
	"net/http"
	"strconv"

	"github.com/tggo/cerber/internal/usage"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Collector turns a usage.Tracker snapshot into Prometheus metrics.
type Collector struct {
	tr      *usage.Tracker
	version string

	requests      *prometheus.Desc
	errors        *prometheus.Desc
	inTokens      *prometheus.Desc
	outTokens     *prometheus.Desc
	reqByModel    *prometheus.Desc
	costByModel   *prometheus.Desc
	inTokByModel  *prometheus.Desc
	outTokByModel *prometheus.Desc
	buildInfo     *prometheus.Desc
}

// NewCollector builds a Collector over the given tracker. version labels the
// build_info metric (e.g. version.String()).
func NewCollector(tr *usage.Tracker, version string) *Collector {
	return &Collector{
		tr:            tr,
		version:       version,
		requests:      prometheus.NewDesc("cerber_requests_total", "Total requests per credential.", []string{"credential"}, nil),
		errors:        prometheus.NewDesc("cerber_errors_total", "Total errored requests per credential.", []string{"credential"}, nil),
		inTokens:      prometheus.NewDesc("cerber_input_tokens_total", "Total input tokens per credential.", []string{"credential"}, nil),
		outTokens:     prometheus.NewDesc("cerber_output_tokens_total", "Total output tokens per credential.", []string{"credential"}, nil),
		reqByModel:    prometheus.NewDesc("cerber_requests_by_model_total", "Total requests per model.", []string{"model"}, nil),
		costByModel:   prometheus.NewDesc("cerber_cost_usd_total", "Cumulative cost (USD) per model from configured pricing.", []string{"model"}, nil),
		inTokByModel:  prometheus.NewDesc("cerber_input_tokens_by_model_total", "Total input tokens per model.", []string{"model"}, nil),
		outTokByModel: prometheus.NewDesc("cerber_output_tokens_by_model_total", "Total output tokens per model.", []string{"model"}, nil),
		buildInfo:     prometheus.NewDesc("cerber_build_info", "Build info; constant 1 with the version label.", []string{"version"}, nil),
	}
}

// Describe implements prometheus.Collector.
func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.requests
	ch <- c.errors
	ch <- c.inTokens
	ch <- c.outTokens
	ch <- c.reqByModel
	ch <- c.costByModel
	ch <- c.inTokByModel
	ch <- c.outTokByModel
	ch <- c.buildInfo
}

// Collect implements prometheus.Collector.
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	rep := c.tr.Snapshot()
	for _, e := range rep.ByCredential {
		ch <- prometheus.MustNewConstMetric(c.requests, prometheus.CounterValue, float64(e.Requests), e.Name)
		ch <- prometheus.MustNewConstMetric(c.errors, prometheus.CounterValue, float64(e.Errors), e.Name)
		ch <- prometheus.MustNewConstMetric(c.inTokens, prometheus.CounterValue, float64(e.InputTokens), e.Name)
		ch <- prometheus.MustNewConstMetric(c.outTokens, prometheus.CounterValue, float64(e.OutputTokens), e.Name)
	}
	for _, e := range rep.ByModel {
		ch <- prometheus.MustNewConstMetric(c.reqByModel, prometheus.CounterValue, float64(e.Requests), e.Name)
		ch <- prometheus.MustNewConstMetric(c.inTokByModel, prometheus.CounterValue, float64(e.InputTokens), e.Name)
		ch <- prometheus.MustNewConstMetric(c.outTokByModel, prometheus.CounterValue, float64(e.OutputTokens), e.Name)
		if e.Cost > 0 {
			ch <- prometheus.MustNewConstMetric(c.costByModel, prometheus.CounterValue, e.Cost, e.Name)
		}
	}
	ch <- prometheus.MustNewConstMetric(c.buildInfo, prometheus.GaugeValue, 1, c.version)
}

// httpBuckets covers fast admin calls through slow multi-second LLM streams.
var httpBuckets = []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 20, 30, 60, 120}

// queueBuckets covers the wait a request spends queued behind a provider's
// concurrency cap (can be tens of seconds when the cap is 1).
var queueBuckets = []float64{0.01, 0.1, 0.5, 1, 2, 5, 10, 30, 60, 120, 300}

// Metrics holds the live instruments updated as requests flow. The zero value is
// not usable; call NewMetrics.
type Metrics struct {
	httpDuration *prometheus.HistogramVec
	httpRequests *prometheus.CounterVec
	inflight     *prometheus.GaugeVec
	queueDepth   *prometheus.GaugeVec
	queueWait    *prometheus.HistogramVec
}

// NewMetrics constructs the live instruments. They are registered (and thus
// exposed) only when passed to Handler.
func NewMetrics() *Metrics {
	return &Metrics{
		httpDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "cerber_http_request_duration_seconds",
			Help:    "HTTP request latency by path, resolved provider, and status code.",
			Buckets: httpBuckets,
		}, []string{"path", "provider", "status"}),
		httpRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "cerber_http_requests_total",
			Help: "HTTP requests by path, resolved provider, and status code.",
		}, []string{"path", "provider", "status"}),
		inflight: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "cerber_provider_inflight_requests",
			Help: "In-flight upstream requests per provider (held until the response body is closed).",
		}, []string{"provider"}),
		queueDepth: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "cerber_provider_queue_depth",
			Help: "Requests currently waiting for a concurrency slot per provider.",
		}, []string{"provider"}),
		queueWait: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "cerber_provider_queue_wait_seconds",
			Help:    "Time a request waited for a concurrency slot per provider.",
			Buckets: queueBuckets,
		}, []string{"provider"}),
	}
}

// MustRegister adds the live instruments to reg.
func (m *Metrics) MustRegister(reg *prometheus.Registry) {
	reg.MustRegister(m.httpDuration, m.httpRequests, m.inflight, m.queueDepth, m.queueWait)
}

// ObserveHTTP records one finished HTTP request: its latency and status. An
// empty provider (request didn't resolve to one) is labelled "none".
func (m *Metrics) ObserveHTTP(path, provider string, status int, seconds float64) {
	if m == nil {
		return
	}
	if provider == "" {
		provider = "none"
	}
	sc := strconv.Itoa(status)
	m.httpDuration.WithLabelValues(path, provider, sc).Observe(seconds)
	m.httpRequests.WithLabelValues(path, provider, sc).Inc()
}

// QueueWait records how long a request waited for a provider's concurrency slot.
func (m *Metrics) QueueWait(provider string, seconds float64) {
	if m == nil {
		return
	}
	m.queueWait.WithLabelValues(provider).Observe(seconds)
}

// InflightInc / InflightDec track in-flight upstream requests per provider.
func (m *Metrics) InflightInc(provider string) {
	if m == nil {
		return
	}
	m.inflight.WithLabelValues(provider).Inc()
}

func (m *Metrics) InflightDec(provider string) {
	if m == nil {
		return
	}
	m.inflight.WithLabelValues(provider).Dec()
}

// QueueDepthInc / QueueDepthDec track requests waiting for a slot per provider.
func (m *Metrics) QueueDepthInc(provider string) {
	if m == nil {
		return
	}
	m.queueDepth.WithLabelValues(provider).Inc()
}

func (m *Metrics) QueueDepthDec(provider string) {
	if m == nil {
		return
	}
	m.queueDepth.WithLabelValues(provider).Dec()
}

// Handler returns an HTTP handler serving the usage snapshot plus (when non-nil)
// the live instruments on a private registry (no global state). version labels
// build_info.
func Handler(tr *usage.Tracker, version string, m *Metrics) http.Handler {
	reg := prometheus.NewRegistry()
	reg.MustRegister(NewCollector(tr, version))
	if m != nil {
		m.MustRegister(reg)
	}
	return promhttp.HandlerFor(reg, promhttp.HandlerOpts{})
}
