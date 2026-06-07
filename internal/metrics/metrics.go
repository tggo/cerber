// Package metrics exposes cerber's usage aggregates as Prometheus metrics. It
// bridges the in-memory usage.Tracker to Prometheus via a custom collector that
// reads a snapshot on each scrape, so there is a single source of truth.
package metrics

import (
	"net/http"

	"cerber/internal/usage"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Collector turns a usage.Tracker snapshot into Prometheus metrics.
type Collector struct {
	tr *usage.Tracker

	requests   *prometheus.Desc
	errors     *prometheus.Desc
	inTokens   *prometheus.Desc
	outTokens  *prometheus.Desc
	reqByModel *prometheus.Desc
}

// NewCollector builds a Collector over the given tracker.
func NewCollector(tr *usage.Tracker) *Collector {
	return &Collector{
		tr:         tr,
		requests:   prometheus.NewDesc("cerber_requests_total", "Total requests per credential.", []string{"credential"}, nil),
		errors:     prometheus.NewDesc("cerber_errors_total", "Total errored requests per credential.", []string{"credential"}, nil),
		inTokens:   prometheus.NewDesc("cerber_input_tokens_total", "Total input tokens per credential.", []string{"credential"}, nil),
		outTokens:  prometheus.NewDesc("cerber_output_tokens_total", "Total output tokens per credential.", []string{"credential"}, nil),
		reqByModel: prometheus.NewDesc("cerber_requests_by_model_total", "Total requests per model.", []string{"model"}, nil),
	}
}

// Describe implements prometheus.Collector.
func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.requests
	ch <- c.errors
	ch <- c.inTokens
	ch <- c.outTokens
	ch <- c.reqByModel
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
	}
}

// Handler returns an HTTP handler serving the metrics for the given tracker on a
// private registry (no global state).
func Handler(tr *usage.Tracker) http.Handler {
	reg := prometheus.NewRegistry()
	reg.MustRegister(NewCollector(tr))
	return promhttp.HandlerFor(reg, promhttp.HandlerOpts{})
}
