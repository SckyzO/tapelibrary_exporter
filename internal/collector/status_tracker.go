// Package collector implements the Prometheus collectors for the
// tapelibrary_exporter exporter.
package collector

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/sckyzo/tapelibrary_exporter/internal/logger"
)

// StatusTracker is a single Prometheus collector that runs a set of inner
// collectors and emits per-collector health metrics. Using one shared collector
// avoids duplicate descriptor panics that occur when each inner collector
// independently emits the same status metric descriptor.
type StatusTracker struct {
	entries  []statusEntry
	success  *prometheus.Desc
	duration *prometheus.Desc
	logger   *logger.Logger
}

type statusEntry struct {
	name      string
	collector prometheus.Collector
}

// NewStatusTracker creates a StatusTracker. Register it once with the Prometheus
// registry; add inner collectors via Add().
func NewStatusTracker(log *logger.Logger) *StatusTracker {
	return &StatusTracker{
		logger: log,
		success: prometheus.NewDesc(
			"tapelibrary_exporter_collector_success",
			"Whether the last scrape of the collector succeeded (1=success, 0=failure)",
			[]string{"collector"}, nil,
		),
		duration: prometheus.NewDesc(
			"tapelibrary_exporter_collector_duration_seconds",
			"Duration of the last scrape for the collector in seconds",
			[]string{"collector"}, nil,
		),
	}
}

// Add registers an inner collector under the given name.
func (st *StatusTracker) Add(name string, c prometheus.Collector) {
	st.entries = append(st.entries, statusEntry{name: name, collector: c})
}

// Describe sends the inner collectors' descriptors plus the two status descriptors.
func (st *StatusTracker) Describe(ch chan<- *prometheus.Desc) {
	for _, e := range st.entries {
		e.collector.Describe(ch)
	}
	ch <- st.success
	ch <- st.duration
}

// Collect runs each inner collector, measures its duration, and emits status
// metrics. Success is count-based, not just panic-based: each inner
// collector's output is buffered on a private per-entry channel (instead of
// writing directly into ch) so this method can count how many metrics it
// produced before forwarding them. A collector that returns normally but
// emits zero metrics, the documented "log the error, then bare return"
// shape every collector in this exporter follows (see collector.go's Collect
// doc comment), is exactly as much a failed scrape as one that panics, so
// both are reported as collector_success=0 here. Panics are still caught via
// defer/recover in the same goroutine that calls Collect; deferring the
// channel close ahead of the recover (LIFO: recover runs first) guarantees
// the buffering goroutine below always sees its channel closed and returns,
// even when the wrapped Collect panics partway through.
func (st *StatusTracker) Collect(ch chan<- prometheus.Metric) {
	for _, e := range st.entries {
		start := time.Now()
		succeeded := 1.0

		sub := make(chan prometheus.Metric)
		var collected []prometheus.Metric
		done := make(chan struct{})
		go func() {
			for m := range sub {
				collected = append(collected, m)
			}
			close(done)
		}()

		func() {
			defer close(sub)
			defer func() {
				if r := recover(); r != nil {
					st.logger.Error("Collector panicked", "collector", e.name, "panic", r)
					succeeded = 0
				}
			}()
			e.collector.Collect(sub)
		}()
		<-done

		elapsed := time.Since(start).Seconds()
		if len(collected) == 0 {
			succeeded = 0
		}
		for _, m := range collected {
			ch <- m
		}

		ch <- prometheus.MustNewConstMetric(st.success, prometheus.GaugeValue, succeeded, e.name)
		ch <- prometheus.MustNewConstMetric(st.duration, prometheus.GaugeValue, elapsed, e.name)
	}
}
