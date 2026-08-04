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

// accessorStates is the full set of values GET /v1/accessors documents for its
// "state" field (TS4500 R1.11.2, "URL endpoints and resources"). Every one of
// them is emitted per accessor on every refresh as its own series, exactly one
// carrying 1 and the rest 0: the stateset encoding this exporter uses for
// every enumeration (see docs/exporter-journal.md, "Enum encoding").
//
// failedToInitialize closes the list without being in the manual's own table.
// The manual names it in the description of the library status
// accessorsUnavailable ("noMovementAllowed, failedToInitialize, and
// bothGrippersFailed") and tabulates it nowhere, which is the same
// tables-are-a-floor gap the LibraryCollector and FramesCollector already
// carry a branch for.
//
// reorienting is deliberately NOT here. The legacy RoS scripts in
// samples/legacy/ expect it; R1.11.2 documents the same phase as calibrating,
// so a rule matching reorienting can never have fired on this firmware. This
// is the second of the four legacy-vs-manual contradictions recorded in
// docs/exporter-journal.md, resolved the same way the frame door
// pseudo-states were: the manual wins, and the emit-observed-anyway branch in
// refresh below is what surfaces the truth if a live library disagrees.
//
// Order is the manual's own priority order, not alphabetical;
// Registry.Gather sorts the exposition output by label value regardless, so
// this slice's order is invisible to a scrape and kept as-is to stay diffable
// against the manual.
var accessorStates = []string{
	"inServiceMode",
	"noMovementAllowed",
	"bothGrippersFailed",
	"gripper1Failed",
	"gripper2Failed",
	"scannerFailed",
	"noMotorPower",
	"calibrating",
	"onlineStandby",
	"onlineActive",
	"failedToInitialize",
}

// accessorAccessValues is the set of values the driveAccess and
// cartridgeAccess fields take. The manual states these two as prose rather
// than as a table ("this attribute is normal when the accessor is in a state
// that allows it ... Otherwise, limited is reported"), so the set is two
// values, not the three of the `accessible` ternary that drives and cartridges
// carry.
//
// Both fields exist only to express one thing the state stateset cannot: on a
// dual-accessor library, whether the OTHER accessor is physically parked
// somewhere that blocks this one's path. An accessor can be onlineActive and
// still be unable to reach half the library.
var accessorAccessValues = []string{
	"normal",
	"limited",
}

// accessorStats is the parsed shape of one GET /v1/accessors entry. The
// endpoint returns one element per robotic accessor, keyed by the library's
// own native location string (accessor_Aa, accessor_Ab), which is what the
// `location` label carries verbatim so a value in a dashboard can be pasted
// straight into the library's GUI.
//
// Only the fields this collector emits are declared. The response also carries
// stateReferenceEvent, the ID of the event that caused the current state; it
// is left out because an event ID is neither a measurement nor a bounded
// label value, and events get their own collector.
//
// Four fields are pointers, and the reason is the same for all four: the API
// documents them as nullable, and a plain float64 would silently decode null
// to 0. A pivot count of 0 and an accessor that does not pivot are different
// facts, and only one of them should produce a series.
//
//   - Temperature and Humidity are null on ALL TS4500 hardware ("For TS4500,
//     null is returned as there is no sensor"), which the 2026-07-28 capture
//     confirms on both accessors. They are declared anyway so that hardware
//     carrying the sensor reports it without a code change.
//   - Pivots and VelocityScalingPivot are documented null on accessors that do
//     not pivot. A 0 there would read as a robot that has never pivoted rather
//     than one that cannot.
type accessorStats struct {
	Location             string   `json:"location"`
	State                string   `json:"state"`
	DriveAccess          string   `json:"driveAccess"`
	CartridgeAccess      string   `json:"cartridgeAccess"`
	Pivots               *float64 `json:"pivots"`
	BarCodeScans         float64  `json:"barCodeScans"`
	VelocityScalingXY    float64  `json:"velocityScalingXY"`
	VelocityScalingPivot *float64 `json:"velocityScalingPivot"`
	TravelX              float64  `json:"travelX"`
	TravelY              float64  `json:"travelY"`
	GetsGripper1         float64  `json:"getsGripper1"`
	PutsGripper1         float64  `json:"putsGripper1"`
	GetsGripper2         float64  `json:"getsGripper2"`
	PutsGripper2         float64  `json:"putsGripper2"`
	Temperature          *float64 `json:"temperature"`
	Humidity             *float64 `json:"humidity"`
}

// accessorsData is this collector's only I/O: it fetches the raw response body
// from the configured library. Kept separate from parsing (parseAccessors,
// below) so parsing stays pure and unit-testable without a live library.
func (c *AccessorsCollector) accessorsData(ctx context.Context) ([]byte, error) {
	return c.client.Fetch(ctx, "/accessors")
}

// parseAccessors decodes accessorsData's response body into one accessorStats
// per robotic accessor. Pure: no I/O, no logging, no side effects, so every
// input maps deterministically to an output. That is what makes it
// unit-testable with plain byte fixtures (see the test file's
// TestParseAccessors).
//
// Three inputs are rejected rather than passed through, all for the same
// reason — refresh keeps the previous cache on error, which is the right
// outcome for a response this collector cannot interpret:
//
//   - An empty array. A library with no accessor cannot move a cartridge and
//     would not be answering this request, so an empty list is a response that
//     lost its content rather than a real inventory.
//   - An entry with no location. It would emit a series labelled location=""
//     that no operator can trace back to any hardware.
//   - Two entries sharing one location. Every metric below is keyed by
//     location alone, so a duplicate would send two metrics with the same
//     descriptor and the same label set, and Registry.Gather rejects the
//     WHOLE scrape when that happens, not just the offending series. Failing
//     closed here keeps a malformed response from taking out every other
//     collector's metrics too (see CONTRIBUTING.md, "Common Pitfalls").
func parseAccessors(b []byte) ([]accessorStats, error) {
	var entries []accessorStats
	if err := json.Unmarshal(b, &entries); err != nil {
		return nil, fmt.Errorf("parse accessors response: %w", err)
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("parse accessors response: empty array, want at least one accessor")
	}

	// Indexed rather than ranged by value: accessorStats is wide enough that
	// copying one per iteration is what gocritic's rangeValCopy flags.
	seen := make(map[string]struct{}, len(entries))
	for i := range entries {
		e := &entries[i]
		if e.Location == "" {
			return nil, fmt.Errorf("parse accessors response: entry with an empty location")
		}
		if _, dup := seen[e.Location]; dup {
			return nil, fmt.Errorf("parse accessors response: duplicate location %q", e.Location)
		}
		seen[e.Location] = struct{}{}
	}
	return entries, nil
}

// accessorsGetMetrics is the glue between the I/O step (accessorsData) and the
// pure parsing step (parseAccessors): the shape every collector in this
// exporter follows, regardless of flavor. refresh, below, calls this on its
// own background schedule; nothing else in this file calls the library
// directly.
func (c *AccessorsCollector) accessorsGetMetrics(ctx context.Context) ([]accessorStats, error) {
	data, err := c.accessorsData(ctx)
	if err != nil {
		return nil, err
	}
	return parseAccessors(data)
}

// AccessorsCollector reads GET /v1/accessors: the operational state of every
// robotic accessor, whether it can currently reach the library's drives and
// cartridges, and the lifetime usage counters the library keeps for each one
// (pivots, bar-code scans, distance travelled, and per-gripper gets and puts).
// It is the background-refresh variant, which on this target model is not a
// choice: a scrape serves N libraries through one /metrics and must never
// block on a machine that has gone away. A background goroutine (started by
// Start, below) refreshes a cached metric slice on a fixed interval, and
// Collect only ever reads that cache under mu.
type AccessorsCollector struct {
	client   *Client
	interval time.Duration
	log      *logger.Logger

	state                *prometheus.Desc
	driveAccess          *prometheus.Desc
	cartridgeAccess      *prometheus.Desc
	pivots               *prometheus.Desc
	barCodeScans         *prometheus.Desc
	travelMeters         *prometheus.Desc
	gets                 *prometheus.Desc
	puts                 *prometheus.Desc
	velocityScalingXY    *prometheus.Desc
	velocityScalingPivot *prometheus.Desc
	temperature          *prometheus.Desc
	humidity             *prometheus.Desc
	lastRefreshDesc      *prometheus.Desc

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

// NewAccessorsCollector builds the collector and its Descs. It is pure: it
// starts no goroutine and performs no I/O, which is what makes it
// constructible in tests with no background refresh running. Call Start
// once, after construction, to begin refreshing.
//
// The five lifetime counters are CounterValue with a _total suffix: the
// library reports them as monotonic device totals that survive a restart of
// this exporter, so rate() over them is meaningful and a reset is a real
// event worth seeing. Everything else the device reports as a snapshot is a
// Gauge (see docs/exporter-journal.md, "Metric name shape").
func NewAccessorsCollector(log *logger.Logger, client *Client, interval time.Duration) *AccessorsCollector {
	return &AccessorsCollector{
		client:   client,
		interval: interval,
		log:      log,
		state: prometheus.NewDesc(
			"tapelibrary_accessor_state",
			"Operational state of the robotic accessor, as a stateset: 1 on the active state and 0 on every other known state.",
			[]string{"location", "state"}, nil,
		),
		driveAccess: prometheus.NewDesc(
			"tapelibrary_accessor_drive_access",
			"Whether the accessor can reach the library's drives, as a stateset: 1 on the active value and 0 on every other known value. On a dual-accessor library this also reflects the other accessor's position, so an accessor can be online and still report limited.",
			[]string{"location", "access"}, nil,
		),
		cartridgeAccess: prometheus.NewDesc(
			"tapelibrary_accessor_cartridge_access",
			"Whether the accessor can reach the library's cartridges, as a stateset: 1 on the active value and 0 on every other known value. On a dual-accessor library this also reflects the other accessor's position, so an accessor can be online and still report limited.",
			[]string{"location", "access"}, nil,
		),
		pivots: prometheus.NewDesc(
			"tapelibrary_accessor_pivots_total",
			"Number of pivots this accessor has performed in its lifetime. An accessor that does not pivot reports no series at all, rather than a 0 claiming it has never pivoted.",
			[]string{"location"}, nil,
		),
		barCodeScans: prometheus.NewDesc(
			"tapelibrary_accessor_bar_code_scans_total",
			"Number of bar code scans this accessor has performed in its lifetime.",
			[]string{"location"}, nil,
		),
		travelMeters: prometheus.NewDesc(
			"tapelibrary_accessor_travel_meters_total",
			"Distance in meters this accessor has travelled in its lifetime, per axis: x is horizontal, y is vertical.",
			[]string{"location", "axis"}, nil,
		),
		gets: prometheus.NewDesc(
			"tapelibrary_accessor_gets_total",
			"Number of times the gripper has engaged to retrieve a cartridge into this accessor, in its lifetime.",
			[]string{"location", "gripper"}, nil,
		),
		puts: prometheus.NewDesc(
			"tapelibrary_accessor_puts_total",
			"Number of times the gripper has engaged to place a cartridge out of this accessor, in its lifetime.",
			[]string{"location", "gripper"}, nil,
		),
		velocityScalingXY: prometheus.NewDesc(
			"tapelibrary_accessor_velocity_scaling_xy_ratio",
			"Scaling applied to the accessor's maximum velocity in the X and Y directions, as a ratio from 0 to 1 (the API reports a 0-100 percentage). Anything below 1 means the accessor has been deliberately slowed.",
			[]string{"location"}, nil,
		),
		velocityScalingPivot: prometheus.NewDesc(
			"tapelibrary_accessor_velocity_scaling_pivot_ratio",
			"Scaling applied to the accessor's maximum pivot velocity, as a ratio from 0 to 1 (the API reports a 0-100 percentage). An accessor that does not pivot reports no series at all.",
			[]string{"location"}, nil,
		),
		temperature: prometheus.NewDesc(
			"tapelibrary_accessor_temperature_celsius",
			"Temperature in Celsius measured inside the library by a sensor on this accessor, at its current position. Accessors carrying no such sensor report no series at all, rather than a 0 that would read as a freezing library.",
			[]string{"location"}, nil,
		),
		humidity: prometheus.NewDesc(
			"tapelibrary_accessor_humidity_ratio",
			"Relative humidity measured inside the library by a sensor on this accessor, as a ratio from 0 to 1 (the API reports a 0-100 percentage). Accessors carrying no such sensor report no series at all.",
			[]string{"location"}, nil,
		),
		lastRefreshDesc: prometheus.NewDesc(
			"tapelibrary_accessors_last_refresh_timestamp_seconds",
			"Unix time of the last successful accessors refresh. Alert if time() - this > 2 x the collector's configured interval.",
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
func (c *AccessorsCollector) Start(ctx context.Context) {
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
func (c *AccessorsCollector) Done() <-chan struct{} {
	return c.done
}

// stateset emits one series per known value of an enumerated field, exactly
// one carrying 1, plus the observed value itself when the manual does not
// document it. The manual's tables are demonstrably a floor rather than a
// ceiling (it names states in prose that appear in no table), so an unlisted
// value must surface as its own series rather than silently leave every
// series at 0 and make the accessor look stateless.
//
// The `known` flag is what keeps that extra series from duplicating a label
// set already emitted in the loop, which would fail Gather for this
// collector's whole scrape.
//
// Shared by the three enumerated fields on this endpoint (state, driveAccess,
// cartridgeAccess) rather than written out three times: they differ only in
// their descriptor and their value list, and the emit-observed-anyway branch
// is subtle enough that one tested copy beats three.
func (c *AccessorsCollector) stateset(
	metrics []prometheus.Metric,
	desc *prometheus.Desc,
	location, observed, field string,
	known []string,
) []prometheus.Metric {
	documented := false
	for _, v := range known {
		value := 0.0
		if v == observed {
			value = 1.0
			documented = true
		}
		metrics = append(metrics, prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, value, location, v))
	}
	if !documented && observed != "" {
		c.log.Warn("Accessor reported a value absent from the documented set: emitting it anyway",
			"location", location, "field", field, "value", observed)
		metrics = append(metrics, prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, 1, location, observed))
	}
	return metrics
}

// refresh performs the one I/O call (accessorsGetMetrics, via the injected
// *Client) and, on success, atomically replaces the cache. On error it logs
// and returns, leaving the previous cache and lastRefresh untouched,
// fail-open: a transient failure serves the last-known-good data instead of
// dropping the series, and the freshness gauge is the signal that a refresh
// is stale, not a dropped scrape.
func (c *AccessorsCollector) refresh(ctx context.Context) {
	accessors, err := c.accessorsGetMetrics(ctx)
	if err != nil {
		c.log.Error("Failed to refresh accessors metrics: keeping previous cache", "err", err)
		return
	}

	// Per accessor: three statesets, two axes of travel, two grippers x
	// (gets + puts), and up to five single-valued gauges and counters.
	perAccessor := len(accessorStates) + 2*len(accessorAccessValues) + 11
	metrics := make([]prometheus.Metric, 0, len(accessors)*perAccessor)

	// Indexed rather than ranged by value, same reason as parseAccessors above.
	for i := range accessors {
		a := &accessors[i]

		metrics = c.stateset(metrics, c.state, a.Location, a.State, "state", accessorStates)
		metrics = c.stateset(metrics, c.driveAccess, a.Location, a.DriveAccess, "driveAccess", accessorAccessValues)
		metrics = c.stateset(metrics, c.cartridgeAccess, a.Location, a.CartridgeAccess, "cartridgeAccess", accessorAccessValues)

		metrics = append(metrics,
			prometheus.MustNewConstMetric(c.barCodeScans, prometheus.CounterValue, a.BarCodeScans, a.Location),
			prometheus.MustNewConstMetric(c.travelMeters, prometheus.CounterValue, a.TravelX, a.Location, "x"),
			prometheus.MustNewConstMetric(c.travelMeters, prometheus.CounterValue, a.TravelY, a.Location, "y"),
			prometheus.MustNewConstMetric(c.gets, prometheus.CounterValue, a.GetsGripper1, a.Location, "1"),
			prometheus.MustNewConstMetric(c.gets, prometheus.CounterValue, a.GetsGripper2, a.Location, "2"),
			prometheus.MustNewConstMetric(c.puts, prometheus.CounterValue, a.PutsGripper1, a.Location, "1"),
			prometheus.MustNewConstMetric(c.puts, prometheus.CounterValue, a.PutsGripper2, a.Location, "2"),
			// The API reports 0-100; Prometheus convention is a 0-1 ratio.
			prometheus.MustNewConstMetric(c.velocityScalingXY, prometheus.GaugeValue, a.VelocityScalingXY/100, a.Location),
		)

		// The four nullable fields. null on the wire means "this hardware
		// cannot report this", which is not the same fact as a zero, so it
		// produces no series at all rather than a 0 that a dashboard would
		// average, alert on, or plot as a real reading. Every accessor in the
		// 2026-07-28 capture reports temperature and humidity null, so on this
		// fleet those two are the common case rather than an edge case.
		if a.Pivots != nil {
			metrics = append(metrics, prometheus.MustNewConstMetric(c.pivots, prometheus.CounterValue, *a.Pivots, a.Location))
		}
		if a.VelocityScalingPivot != nil {
			metrics = append(metrics, prometheus.MustNewConstMetric(c.velocityScalingPivot, prometheus.GaugeValue, *a.VelocityScalingPivot/100, a.Location))
		}
		if a.Temperature != nil {
			metrics = append(metrics, prometheus.MustNewConstMetric(c.temperature, prometheus.GaugeValue, *a.Temperature, a.Location))
		}
		if a.Humidity != nil {
			metrics = append(metrics, prometheus.MustNewConstMetric(c.humidity, prometheus.GaugeValue, *a.Humidity/100, a.Location))
		}
	}

	c.mu.Lock()
	c.cached = metrics
	c.lastRefresh = time.Now()
	c.mu.Unlock()
}

// Describe sends every one of this collector's descriptors, including the
// freshness gauge. Constant regardless of scrape or refresh outcome, which
// is what makes prometheus.DescribeByCollect unnecessary here. The four
// nullable metrics are described even on hardware that never populates them:
// a descriptor is what this collector CAN emit, not what it did last time.
func (c *AccessorsCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.state
	ch <- c.driveAccess
	ch <- c.cartridgeAccess
	ch <- c.pivots
	ch <- c.barCodeScans
	ch <- c.travelMeters
	ch <- c.gets
	ch <- c.puts
	ch <- c.velocityScalingXY
	ch <- c.velocityScalingPivot
	ch <- c.temperature
	ch <- c.humidity
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
func (c *AccessorsCollector) Collect(ch chan<- prometheus.Metric) {
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
