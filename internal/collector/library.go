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

// libraryStates is the full set of values GET /v1/library documents for its
// "status" field (TS4500 R1.11.2, "URL endpoints and resources"). Every one of
// them is emitted on every refresh as its own series, exactly one carrying 1
// and the rest 0: the stateset encoding this exporter uses for every
// enumeration (see docs/exporter-journal.md, "Enum encoding").
//
// Emitting the whole set rather than only the active value is what keeps the
// severity classification in the alerting rules instead of in the metric
// value, and what makes a transition to a state that has never yet occurred
// visible as a series changing from 0 to 1, rather than as a series appearing
// from nowhere.
//
// Order is the manual's own, not alphabetical; Registry.Gather sorts the
// exposition output by label value regardless, so this slice's order is
// invisible to a scrape and kept as-is to stay diffable against the manual.
var libraryStates = []string{
	"unknown",
	"doorOpenWhileNotAllowed",
	"notConfigured",
	"doorOpen",
	"initializing",
	"inServiceMode",
	"accessorsUnavailable",
	"calibrationRequired",
	"accessorDegraded",
	"nodeCardDegraded",
	"driveDegraded",
	"cartridgeDegraded",
	"updating",
	"pausing",
	"paused",
	"scanningInventory",
	"online",
}

// libraryStats is the parsed shape of one GET /v1/library entry. The endpoint
// returns a single-element array, not an object (see parseLibrary).
//
// Only the fields this collector actually emits are declared. The response
// carries considerably more — site address, contact telephone, NTP server
// addresses, GUI toggles — which is either configuration surface or contact
// detail belonging to no time series, and in several cases personal data that
// has no business in /metrics at all.
//
// Note that the API's own "state" field is the postal state of the library's
// physical address, NOT its operational state; the operational state is
// "status", and that is what this collector's `state` label carries.
type libraryStats struct {
	Name               string  `json:"name"`
	Status             string  `json:"status"`
	TotalCapacity      float64 `json:"totalCapacity"`
	LicensedCapacity   float64 `json:"licensedCapacity"`
	TotalCartridges    float64 `json:"totalCartridges"`
	AssignedCartridges float64 `json:"assignedCartridges"`
	Firmware           string  `json:"firmware"`
	SN                 string  `json:"sn"`
	// Both threshold fields are percentages (0-100) on the wire and are
	// converted to 0-1 ratios on emission, per the metric-name shape's
	// `_ratio` unit. See refresh below.
	CapacityUtilThresh     float64 `json:"capacityUtilThresh"`
	DualAccessorUtilThresh float64 `json:"dualAccessorUtilThresh"`
}

// libraryData is this collector's only I/O: it fetches the raw response body
// from the configured library. Kept separate from parsing (parseLibrary,
// below) so parsing stays pure and unit-testable without a live library.
func (c *LibraryCollector) libraryData(ctx context.Context) ([]byte, error) {
	return c.client.Fetch(ctx, "/library")
}

// parseLibrary decodes libraryData's response body into libraryStats. Pure:
// no I/O, no logging, no side effects, so every input maps deterministically
// to an output. That is what makes it unit-testable with plain byte fixtures
// (see the test file's TestParseLibrary).
//
// The endpoint wraps its single object in an array. An empty array is
// rejected rather than mapped to a zero-valued struct: a library that
// reported no capacity, no cartridges and an empty status would be
// indistinguishable from a real library at zero, and refresh's fail-open
// behaviour (keep the previous cache on error) is the right outcome for a
// response this collector cannot interpret.
func parseLibrary(b []byte) (libraryStats, error) {
	var entries []libraryStats
	if err := json.Unmarshal(b, &entries); err != nil {
		return libraryStats{}, fmt.Errorf("parse library response: %w", err)
	}
	if len(entries) == 0 {
		return libraryStats{}, fmt.Errorf("parse library response: empty array, want exactly one entry")
	}
	return entries[0], nil
}

// libraryGetMetrics is the glue between the I/O step (libraryData) and the
// pure parsing step (parseLibrary): the shape every collector in this
// exporter follows, regardless of flavor. refresh, below, calls this on its
// own background schedule; nothing else in this file calls the library
// directly.
func (c *LibraryCollector) libraryGetMetrics(ctx context.Context) (libraryStats, error) {
	data, err := c.libraryData(ctx)
	if err != nil {
		return libraryStats{}, err
	}
	return parseLibrary(data)
}

// LibraryCollector reads GET /v1/library: the library's own operational
// status, its capacity and cartridge counters, and its identity. It is the
// background-refresh variant, which on this target model is not a choice: a
// scrape serves N libraries through one /metrics and must never block on a
// machine that has gone away. A background goroutine (started by Start,
// below) refreshes a cached metric slice on a fixed interval, and Collect
// only ever reads that cache under mu.
type LibraryCollector struct {
	client   *Client
	interval time.Duration
	log      *logger.Logger

	state                 *prometheus.Desc
	slotsCapacity         *prometheus.Desc
	slotsLicensed         *prometheus.Desc
	cartridgesPresent     *prometheus.Desc
	cartridgesAssigned    *prometheus.Desc
	capacityUtilThreshold *prometheus.Desc
	dualAccessorThreshold *prometheus.Desc
	info                  *prometheus.Desc
	lastRefreshDesc       *prometheus.Desc

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

// NewLibraryCollector builds the collector and its Descs. It is pure: it
// starts no goroutine and performs no I/O, which is what makes it
// constructible in tests with no background refresh running. Call Start
// once, after construction, to begin refreshing.
func NewLibraryCollector(log *logger.Logger, client *Client, interval time.Duration) *LibraryCollector {
	return &LibraryCollector{
		client:   client,
		interval: interval,
		log:      log,
		state: prometheus.NewDesc(
			"tapelibrary_library_state",
			"Operational status of the library, as a stateset: 1 on the active status and 0 on every other known status.",
			[]string{"state"}, nil,
		),
		slotsCapacity: prometheus.NewDesc(
			"tapelibrary_library_slots_capacity",
			"Total number of cartridge slots the library physically holds.",
			nil, nil,
		),
		slotsLicensed: prometheus.NewDesc(
			"tapelibrary_library_slots_licensed",
			"Number of cartridge slots the library is currently licensed to use.",
			nil, nil,
		),
		cartridgesPresent: prometheus.NewDesc(
			"tapelibrary_library_cartridges_present",
			"Number of cartridges currently present in the library.",
			nil, nil,
		),
		cartridgesAssigned: prometheus.NewDesc(
			"tapelibrary_library_cartridges_assigned",
			"Number of cartridges currently assigned to a logical library.",
			nil, nil,
		),
		capacityUtilThreshold: prometheus.NewDesc(
			"tapelibrary_library_capacity_util_threshold_ratio",
			"Capacity utilization threshold configured on the library, as a ratio from 0 to 1.",
			nil, nil,
		),
		dualAccessorThreshold: prometheus.NewDesc(
			"tapelibrary_library_dual_accessor_util_threshold_ratio",
			"Dual-accessor utilization threshold configured on the library, as a ratio from 0 to 1.",
			nil, nil,
		),
		info: prometheus.NewDesc(
			"tapelibrary_library_info",
			"Library identity, always 1. Identity strings live here rather than on a measurement series, so a firmware upgrade changes this series alone instead of breaking the continuity of every other.",
			[]string{"name", "serial", "firmware"}, nil,
		),
		lastRefreshDesc: prometheus.NewDesc(
			"tapelibrary_library_last_refresh_timestamp_seconds",
			"Unix time of the last successful library refresh. Alert if time() - this > 2 x the collector's configured interval.",
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
func (c *LibraryCollector) Start(ctx context.Context) {
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
func (c *LibraryCollector) Done() <-chan struct{} {
	return c.done
}

// refresh performs the one I/O call (libraryGetMetrics, via the injected
// *Client) and, on success, atomically replaces the cache. On error it logs
// and returns, leaving the previous cache and lastRefresh untouched,
// fail-open: a transient failure serves the last-known-good data instead of
// dropping the series, and the freshness gauge is the signal that a refresh
// is stale, not a dropped scrape.
func (c *LibraryCollector) refresh(ctx context.Context) {
	stats, err := c.libraryGetMetrics(ctx)
	if err != nil {
		c.log.Error("Failed to refresh library metrics: keeping previous cache", "err", err)
		return
	}

	metrics := make([]prometheus.Metric, 0, len(libraryStates)+7)

	// The stateset: one series per documented status, plus the observed one
	// if the manual does not document it. The manual's tables are
	// demonstrably a floor rather than a ceiling (it names states in prose
	// that appear in no table), so an unlisted status must surface as its
	// own series rather than silently leave every series at 0 and make the
	// library look stateless. `known` is what keeps that extra series from
	// duplicating a label set already emitted above, which would fail
	// Gather for this collector's whole scrape.
	known := false
	for _, s := range libraryStates {
		value := 0.0
		if s == stats.Status {
			value = 1.0
			known = true
		}
		metrics = append(metrics, prometheus.MustNewConstMetric(c.state, prometheus.GaugeValue, value, s))
	}
	if !known && stats.Status != "" {
		c.log.Warn("Library reported a status absent from the documented set: emitting it anyway",
			"status", stats.Status)
		metrics = append(metrics, prometheus.MustNewConstMetric(c.state, prometheus.GaugeValue, 1, stats.Status))
	}

	metrics = append(metrics,
		prometheus.MustNewConstMetric(c.slotsCapacity, prometheus.GaugeValue, stats.TotalCapacity),
		prometheus.MustNewConstMetric(c.slotsLicensed, prometheus.GaugeValue, stats.LicensedCapacity),
		prometheus.MustNewConstMetric(c.cartridgesPresent, prometheus.GaugeValue, stats.TotalCartridges),
		prometheus.MustNewConstMetric(c.cartridgesAssigned, prometheus.GaugeValue, stats.AssignedCartridges),
		// Both thresholds arrive as percentages and are emitted as ratios:
		// base units, per the metric-name shape's `_ratio` suffix.
		prometheus.MustNewConstMetric(c.capacityUtilThreshold, prometheus.GaugeValue, stats.CapacityUtilThresh/100),
		prometheus.MustNewConstMetric(c.dualAccessorThreshold, prometheus.GaugeValue, stats.DualAccessorUtilThresh/100),
		prometheus.MustNewConstMetric(c.info, prometheus.GaugeValue, 1, stats.Name, stats.SN, stats.Firmware),
	)

	c.mu.Lock()
	c.cached = metrics
	c.lastRefresh = time.Now()
	c.mu.Unlock()
}

// Describe sends every one of this collector's descriptors, including the
// freshness gauge. Constant regardless of scrape or refresh outcome, which
// is what makes prometheus.DescribeByCollect unnecessary here.
func (c *LibraryCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.state
	ch <- c.slotsCapacity
	ch <- c.slotsLicensed
	ch <- c.cartridgesPresent
	ch <- c.cartridgesAssigned
	ch <- c.capacityUtilThreshold
	ch <- c.dualAccessorThreshold
	ch <- c.info
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
func (c *LibraryCollector) Collect(ch chan<- prometheus.Metric) {
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
