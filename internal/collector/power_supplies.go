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

// powerSupplyStates is the full set of values GET /v1/powerSupplies documents
// for its "state" field (TS4500 R1.11.2, "Power supplies"). Every one of them
// is emitted per supply on every refresh as its own series, exactly one
// carrying 1 and the rest 0: the stateset encoding this exporter uses for
// every enumeration (see docs/exporter-journal.md, "Enum encoding").
//
// Order is the manual's own — it tabulates these "in order of priority", not
// alphabetically. Registry.Gather sorts the exposition output by label value
// regardless, so this slice's order is invisible to a scrape and is kept as-is
// to stay diffable against the manual.
//
// Three states is the whole enumeration, which is what makes this the
// narrowest collector in the exporter: the endpoint reports a location and a
// health state and nothing else, so there is no identity to carry on an _info
// series and no measurement to carry beside the stateset.
var powerSupplyStates = []string{
	"unknown",
	"failed",
	"online",
}

// powerSupplyStats is the parsed shape of one GET /v1/powerSupplies entry. The
// endpoint returns one element per physical supply, keyed by the library's own
// native location string (powerSupply_F1PSa, powerSupply_F1PSb, ...), which is
// what the `location` label carries verbatim so a value in a dashboard can be
// pasted straight into the library's GUI.
//
// Both documented attributes are declared, because both are emitted: R1.11.2
// gives this endpoint exactly `location` and `state`. That is not a trimmed
// subset the way frameStats is — there is nothing else on the wire to leave
// out.
type powerSupplyStats struct {
	Location string `json:"location"`
	State    string `json:"state"`
}

// powerSuppliesData is this collector's only I/O: it fetches the raw response
// body from the configured library. Kept separate from parsing
// (parsePowerSupplies, below) so parsing stays pure and unit-testable without
// a live library.
func (c *PowerSuppliesCollector) powerSuppliesData(ctx context.Context) ([]byte, error) {
	return c.client.Fetch(ctx, "/powerSupplies")
}

// parsePowerSupplies decodes powerSuppliesData's response body into one
// powerSupplyStats per physical supply. Pure: no I/O, no logging, no side
// effects, so every input maps deterministically to an output. That is what
// makes it unit-testable with plain byte fixtures (see the test file's
// TestParsePowerSupplies).
//
// Three inputs are rejected rather than passed through, all for the same
// reason — refresh keeps the previous cache on error, which is the right
// outcome for a response this collector cannot interpret:
//
//   - An empty array. R1.11.2 states the library is powered by two AC-to-DC
//     supplies in its L25 or L55 frame, so every TS4500 has at least two: an
//     empty list is a response that lost its content, not a library running on
//     no power supplies.
//   - An entry with no location. It would emit a series labelled location=""
//     that no operator can trace back to any hardware.
//   - Two entries sharing one location. Every metric below is keyed by
//     location alone, so a duplicate would send two metrics with the same
//     descriptor and the same label set, and Registry.Gather rejects the
//     WHOLE scrape when that happens, not just the offending series. Failing
//     closed here keeps a malformed response from taking out every other
//     collector's metrics too (see CONTRIBUTING.md, "Common Pitfalls").
func parsePowerSupplies(b []byte) ([]powerSupplyStats, error) {
	var entries []powerSupplyStats
	if err := json.Unmarshal(b, &entries); err != nil {
		return nil, fmt.Errorf("parse power supplies response: %w", err)
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("parse power supplies response: empty array, want at least one power supply")
	}

	seen := make(map[string]struct{}, len(entries))
	for _, e := range entries {
		if e.Location == "" {
			return nil, fmt.Errorf("parse power supplies response: entry with an empty location")
		}
		if _, dup := seen[e.Location]; dup {
			return nil, fmt.Errorf("parse power supplies response: duplicate location %q", e.Location)
		}
		seen[e.Location] = struct{}{}
	}
	return entries, nil
}

// powerSuppliesGetMetrics is the glue between the I/O step
// (powerSuppliesData) and the pure parsing step (parsePowerSupplies): the
// shape every collector in this exporter follows, regardless of flavor.
// refresh, below, calls this on its own background schedule; nothing else in
// this file calls the library directly.
func (c *PowerSuppliesCollector) powerSuppliesGetMetrics(ctx context.Context) ([]powerSupplyStats, error) {
	data, err := c.powerSuppliesData(ctx)
	if err != nil {
		return nil, err
	}
	return parsePowerSupplies(data)
}

// PowerSuppliesCollector reads GET /v1/powerSupplies: the health state of
// every AC-to-DC power supply in the library. Supplies are installed in pairs
// per frame and R1.11.2 is explicit that a single one is adequate to power its
// frame, so a supply leaving `online` costs redundancy rather than service —
// which is exactly why it needs monitoring: nothing else in the library
// reports it, and the loss is invisible until the surviving supply also goes.
//
// It is the background-refresh variant, which on this target model is not a
// choice: a scrape serves N libraries through one /metrics and must never
// block on a machine that has gone away. A background goroutine (started by
// Start, below) refreshes a cached metric slice on a fixed interval, and
// Collect only ever reads that cache under mu.
type PowerSuppliesCollector struct {
	client   *Client
	interval time.Duration
	log      *logger.Logger

	state           *prometheus.Desc
	lastRefreshDesc *prometheus.Desc

	// mu guards cached and lastRefresh: refresh (below) writes them from the
	// background goroutine started by Start, Collect reads them from
	// whichever goroutine calls it (a Prometheus scrape). RWMutex, not a
	// plain Mutex, because Collect only ever reads.
	mu          sync.RWMutex
	cached      []prometheus.Metric
	lastRefresh time.Time

	// done is closed when the background goroutine launched by Start exits.
	// main.go waits on Done() (via instance.BackgroundCollector) after the
	// HTTP server has shut down, so the process doesn't exit mid-refresh.
	done chan struct{}
}

// NewPowerSuppliesCollector builds the collector and its Descs. It is pure: it
// starts no goroutine and performs no I/O, which is what makes it
// constructible in tests with no background refresh running. Call Start once,
// after construction, to begin refreshing.
func NewPowerSuppliesCollector(log *logger.Logger, client *Client, interval time.Duration) *PowerSuppliesCollector {
	return &PowerSuppliesCollector{
		client:   client,
		interval: interval,
		log:      log,
		state: prometheus.NewDesc(
			"tapelibrary_power_supply_state",
			"Operational state of the power supply, as a stateset: 1 on the active state and 0 on every other known state.",
			[]string{"location", "state"}, nil,
		),
		lastRefreshDesc: prometheus.NewDesc(
			"tapelibrary_power_supplies_last_refresh_timestamp_seconds",
			"Unix time of the last successful power supplies refresh. Alert if time() - this > 2 x the collector's configured interval.",
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
func (c *PowerSuppliesCollector) Start(ctx context.Context) {
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
func (c *PowerSuppliesCollector) Done() <-chan struct{} {
	return c.done
}

// refresh performs the one I/O call (powerSuppliesGetMetrics, via the injected
// *Client) and, on success, atomically replaces the cache. On error it logs
// and returns, leaving the previous cache and lastRefresh untouched,
// fail-open: a transient failure serves the last-known-good data instead of
// dropping the series, and the freshness gauge is the signal that a refresh
// is stale, not a dropped scrape.
func (c *PowerSuppliesCollector) refresh(ctx context.Context) {
	supplies, err := c.powerSuppliesGetMetrics(ctx)
	if err != nil {
		c.log.Error("Failed to refresh power supplies metrics: keeping previous cache", "err", err)
		return
	}

	// One stateset per supply, and nothing else on this endpoint.
	metrics := make([]prometheus.Metric, 0, len(supplies)*len(powerSupplyStates))

	for _, ps := range supplies {
		// The stateset: one series per documented state, plus the observed
		// one if the manual does not document it. The manual's tables are
		// demonstrably a floor rather than a ceiling (it names states in
		// prose that appear in no table), so an unlisted state must surface
		// as its own series rather than silently leave every series at 0 and
		// make the supply look stateless. `known` is what keeps that extra
		// series from duplicating a label set already emitted above, which
		// would fail Gather for this collector's whole scrape.
		known := false
		for _, s := range powerSupplyStates {
			value := 0.0
			if s == ps.State {
				value = 1.0
				known = true
			}
			metrics = append(metrics, prometheus.MustNewConstMetric(c.state, prometheus.GaugeValue, value, ps.Location, s))
		}
		if !known && ps.State != "" {
			c.log.Warn("Power supply reported a state absent from the documented set: emitting it anyway",
				"location", ps.Location, "state", ps.State)
			metrics = append(metrics, prometheus.MustNewConstMetric(c.state, prometheus.GaugeValue, 1, ps.Location, ps.State))
		}
	}

	c.mu.Lock()
	c.cached = metrics
	c.lastRefresh = time.Now()
	c.mu.Unlock()
}

// Describe sends every one of this collector's descriptors, including the
// freshness gauge. Constant regardless of scrape or refresh outcome, which
// is what makes prometheus.DescribeByCollect unnecessary here.
func (c *PowerSuppliesCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.state
	ch <- c.lastRefreshDesc
}

// Collect replays the cached metrics from the last successful refresh:
// O(cached size), never touches the library, never blocks on it. It then
// ALWAYS sends the freshness gauge, even before any refresh has ever
// completed (value 0 in that case, since a zero time.Time's Unix() is a
// large negative, not a meaningful "ancient" epoch marker). A collector must
// never emit zero metrics on what it considers a healthy outcome:
// StatusTracker counts emitted metrics per scrape, and an empty cache before
// the first refresh completes is a normal startup window, not a failure. The
// freshness gauge alone guarantees at least one metric every scrape, so
// StatusTracker reports this collector as alive; the gauge's own value (0
// until the first refresh lands) is the separate, correct signal for
// staleness.
func (c *PowerSuppliesCollector) Collect(ch chan<- prometheus.Metric) {
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
