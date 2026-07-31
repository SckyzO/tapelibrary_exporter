package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/sckyzo/tapelibrary_exporter/internal/logger"
)

// exampleStats is the parsed shape of the example target's response. This is
// a placeholder shape, {"items": <int>, "healthy": <bool>}, documenting the
// pattern rather than a real API. Replace it, and parseExample below, with
// your actual target's real response shape when adapting this collector.
type exampleStats struct {
	Items   int  `json:"items"`
	Healthy bool `json:"healthy"`
}

// exampleData is this collector's only I/O: it fetches the raw response body
// from the configured target. Kept separate from parsing (parseExample,
// below) so parsing stays pure and unit-testable without a live server.
func (c *ExampleCollector) exampleData(ctx context.Context) ([]byte, error) {
	return c.client.Fetch(ctx, "/library")
}

// parseExample decodes exampleData's response body into exampleStats. Pure:
// no I/O, no logging, no side effects, so every input maps deterministically
// to an output. That is what makes it unit-testable with plain byte
// fixtures (see the test file's TestParseExample).
func parseExample(b []byte) (exampleStats, error) {
	var stats exampleStats
	if err := json.Unmarshal(b, &stats); err != nil {
		return exampleStats{}, fmt.Errorf("parse example response: %w", err)
	}
	return stats, nil
}

// exampleGetMetrics is the glue between the I/O step (exampleData) and the
// pure parsing step (parseExample): the shape every collector in this
// exporter follows, regardless of flavor. refresh, below, calls this on its
// own background schedule; nothing else in this file calls the target
// directly.
func (c *ExampleCollector) exampleGetMetrics(ctx context.Context) (exampleStats, error) {
	data, err := c.exampleData(ctx)
	if err != nil {
		return exampleStats{}, err
	}
	return parseExample(data)
}

// ExampleCollector is the background-refresh variant materialized by
// /add-collector --variant background. Unlike the synchronous collector
// (collector.go.tmpl), Collect never calls the target: a background
// goroutine (started by Start, below) refreshes a cached metric slice on a
// fixed interval, and Collect only ever reads that cache under mu. This
// decouples scrape cadence from fetch cadence entirely, so a scrape never
// blocks on a slow or expensive target, no matter how slow it is.
type ExampleCollector struct {
	client   *Client
	interval time.Duration
	log      *logger.Logger

	items           *prometheus.Desc
	healthy         *prometheus.Desc
	lastRefreshDesc *prometheus.Desc

	// mu guards cached and lastRefresh: refresh (below) writes them from the
	// background goroutine started by Start, Collect reads them from
	// whichever goroutine calls it (a Prometheus scrape). RWMutex, not a
	// plain Mutex, because Collect only ever reads.
	mu          sync.RWMutex
	cached      []prometheus.Metric
	lastRefresh time.Time

	// done is closed when the background goroutine launched by Start exits.
	// main.go waits on Done() (via the backgroundCollector seam) after the
	// HTTP server has shut down, so the process doesn't exit mid-refresh.
	done chan struct{}
}

// NewExampleCollector builds the collector and its Descs. It is pure: it
// starts no goroutine and performs no I/O, which is what makes it
// constructible in tests with no background refresh running. Call Start
// once, after construction, to begin refreshing.
func NewExampleCollector(log *logger.Logger, client *Client, interval time.Duration) *ExampleCollector {
	return &ExampleCollector{
		client:   client,
		interval: interval,
		log:      log,
		items: prometheus.NewDesc(
			"tapelibrary_items",
			"Number of items reported by the example target.",
			nil, nil,
		),
		healthy: prometheus.NewDesc(
			"tapelibrary_healthy",
			"Whether the example target reports itself healthy (1) or not (0).",
			nil, nil,
		),
		lastRefreshDesc: prometheus.NewDesc(
			"tapelibrary_example_last_refresh_timestamp_seconds",
			"Unix time of the last successful example refresh. Alert if time() - this > 2 x the collector's configured interval.",
			nil, nil,
		),
		done: make(chan struct{}),
	}
}

// Start launches the background refresh goroutine. Call once, after
// construction. The first refresh runs immediately (so the cache starts
// filling as soon as the process starts) without Start itself waiting for
// it: a slow first fetch never blocks process startup. The goroutine exits
// when ctx is cancelled; Done() can then be used to wait for it to finish.
func (c *ExampleCollector) Start(ctx context.Context) {
	go func() {
		defer close(c.done)
		c.refresh(ctx)
		ticker := time.NewTicker(c.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				c.refresh(ctx)
			case <-ctx.Done():
				return
			}
		}
	}()
}

// Done returns a channel that is closed when the background goroutine
// started by Start has fully exited. main.go's shutdown seam waits on this
// (bounded, so a stuck refresh can't hang process exit forever) after the
// HTTP server itself has stopped.
func (c *ExampleCollector) Done() <-chan struct{} {
	return c.done
}

// refresh performs the one flavor-specific I/O call (exampleGetMetrics, via
// the injected *Client) and, on success, atomically replaces the cache.
// On error it logs and returns, leaving the previous cache and lastRefresh
// untouched, fail-open: a transient failure serves the last-known-good
// data instead of dropping the series, and the freshness gauge (below) is
// the signal that a refresh is stale, not a dropped scrape.
func (c *ExampleCollector) refresh(ctx context.Context) {
	stats, err := c.exampleGetMetrics(ctx)
	if err != nil {
		c.log.Error("Failed to refresh example metrics: keeping previous cache", "err", err)
		return
	}

	healthy := 0.0
	if stats.Healthy {
		healthy = 1.0
	}
	metrics := []prometheus.Metric{
		prometheus.MustNewConstMetric(c.items, prometheus.GaugeValue, float64(stats.Items)),
		prometheus.MustNewConstMetric(c.healthy, prometheus.GaugeValue, healthy),
	}

	c.mu.Lock()
	c.cached = metrics
	c.lastRefresh = time.Now()
	c.mu.Unlock()
}

// Describe sends every one of this collector's descriptors, including the
// freshness gauge. Constant regardless of scrape or refresh outcome, which
// is what makes prometheus.DescribeByCollect unnecessary here.
func (c *ExampleCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.items
	ch <- c.healthy
	ch <- c.lastRefreshDesc
}

// Collect replays the cached metrics from the last successful refresh:
// O(cached size), never touches the target, never blocks on it. It then
// ALWAYS sends the freshness gauge, even before any refresh has ever
// completed (value 0 in that case, since a zero time.Time's Unix() is a
// large negative, not a meaningful "ancient" epoch marker). See the
// collector-authoring rule in collector.go.tmpl's Collect for why a
// collector must never emit zero metrics on what it considers a healthy
// outcome: StatusTracker counts emitted metrics per scrape, and an empty
// cache before the first refresh completes is a normal startup window, not
// a failure. The freshness gauge alone guarantees at least one metric every
// scrape, so StatusTracker reports this collector as alive; the gauge's own
// value (0 until the first refresh lands) is the separate, correct signal
// for staleness: see NewExampleCollector's lastRefreshDesc help text.
func (c *ExampleCollector) Collect(ch chan<- prometheus.Metric) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	for _, m := range c.cached {
		ch <- m
	}

	refreshUnix := 0.0
	if !c.lastRefresh.IsZero() {
		refreshUnix = float64(c.lastRefresh.Unix())
	}
	ch <- prometheus.MustNewConstMetric(c.lastRefreshDesc, prometheus.GaugeValue, refreshUnix)
}
