package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/sckyzo/tapelibrary_exporter/internal/logger"
)

// reportsAccessorsTimeLayout parses the `time` field of a GET
// /v1/reports/accessors entry. Same layout as every other timestamp this
// library reports (see driveLastCleanedLayout, the first collector to meet
// it): the zone offset carries no colon, so it is not time.RFC3339 and
// parsing it as such fails.
//
// Named separately rather than used through driveLastCleanedLayout so this
// collector reads on its own, per the convention the journal records: one
// const per collector, all currently the same value.
const reportsAccessorsTimeLayout = driveLastCleanedLayout

// reportsAccessorWindow is the parsed shape of one entry from GET
// /v1/reports/accessors. The endpoint returns one entry per accessor per
// completed hour, covering the last week: 2 accessors x 168 hours on the
// reference capture's fleet, roughly 148 KB.
//
// The eight activity fields are plain float64 rather than pointers, and the
// six environmental ones are pointers, and that split is the same one
// reportsDriveWindow and reportsLibraryWindow make for the same reason: a
// count of things that happened during an hour has a meaningful zero (an
// accessor moves nothing overnight and that is a reading, not a gap), while
// a temperature or humidity has none. `barCodeScans` is 0 in every window of
// the reference capture and is exactly the case that split protects: a
// genuine zero that must stay on the wire.
//
// All six environmental fields are null on every entry of the reference
// capture, and that is the hardware rather than the capture: the accessors
// on this fleet carry no temperature or humidity sensor, which
// /v1/accessors reports the same way (see AccessorsCollector). They are
// parsed and emitted anyway, absent-not-zero, so a site whose accessors do
// carry sensors gets them for free.
type reportsAccessorWindow struct {
	Location           string   `json:"location"`
	Time               string   `json:"time"`
	Duration           float64  `json:"duration"`
	Pivots             float64  `json:"pivots"`
	BarCodeScans       float64  `json:"barCodeScans"`
	TravelX            float64  `json:"travelX"`
	TravelY            float64  `json:"travelY"`
	GetsGripper1       float64  `json:"getsGripper1"`
	PutsGripper1       float64  `json:"putsGripper1"`
	GetsGripper2       float64  `json:"getsGripper2"`
	PutsGripper2       float64  `json:"putsGripper2"`
	TemperatureAverage *float64 `json:"temperatureAverage"`
	TemperatureMin     *float64 `json:"temperatureMin"`
	TemperatureMax     *float64 `json:"temperatureMax"`
	HumidityAverage    *float64 `json:"humidityAverage"`
	HumidityMin        *float64 `json:"humidityMin"`
	HumidityMax        *float64 `json:"humidityMax"`
}

// reportsAccessorStats is the one window this collector exposes for a single
// accessor, together with its own parsed timestamp. The timestamp is carried
// out of the parser rather than re-parsed at emission time because selecting
// the window already required parsing it, and because a window whose time
// could not be parsed never becomes a selection (see parseReportsAccessors).
type reportsAccessorStats struct {
	window reportsAccessorWindow
	at     time.Time
}

// reportsAccessorsData is this collector's only I/O: it fetches the raw
// response body from the configured library. Kept separate from parsing
// (parseReportsAccessors, below) so parsing stays pure and unit-testable
// without a live library.
//
// The request carries no `after`/`before` parameter, matching
// ReportsLibraryCollector and ReportsDrivesCollector, and unlike GET
// /v1/events, which must be bounded because a bare request returns every
// event the library ever recorded. The cost is small here and was checked
// rather than assumed: a library has two accessors, so the default week is
// ~168 windows x 2, roughly 148 KB at the capture's ~440 bytes per entry.
// That sits between reports/library's ~67 KB and nowhere near
// reports/drives' ~3.3 MB, which is why this collector polls at
// reports/library's 15m rather than adopting reports/drives' hourly cadence.
// An `after` parameter would add a clock-skew failure mode for no saving
// worth having: a library running ahead of this host would answer an `after`
// in its own future with an empty array, the exact response
// parseReportsAccessors must reject, so skew would present as a collector
// that silently stops advancing.
func (c *ReportsAccessorsCollector) reportsAccessorsData(ctx context.Context) ([]byte, error) {
	return c.client.Fetch(ctx, "/reports/accessors")
}

// parseReportsAccessors decodes reportsAccessorsData's response body and
// selects, for each accessor, the single window this collector exposes: that
// accessor's newest, by its own `time` field. Pure: no I/O, no logging, no
// side effects.
//
// Selection is per accessor rather than library-wide. Both accessors share
// the newest window in the reference capture, but an accessor taken into
// service mode or removed part-way through the week has its own last
// reported hour, and collapsing them onto a single library-wide window would
// either drop that accessor's series entirely or serve its sibling's
// timestamp against its values. On a two-accessor library that matters more
// than it does on a forty-drive one: the sibling is not one of forty, it is
// the only other one, and AccessorReportShareCollapsed reads exactly the
// case where one of the two has stopped working.
//
// Selection is by timestamp rather than by position, for the reason
// parseReportsLibrary already gives: R1.11.2 documents no ordering for this
// endpoint, and a collector that silently depended on the capture's
// happening to arrive newest-first would report week-old activity as current
// the first time a firmware release changed it.
//
// Two inputs are rejected outright rather than passed through — refresh
// keeps the previous cache on error, which is the right outcome for a
// response this collector cannot interpret:
//
//   - An empty array. A library with no accessor cannot move a cartridge at
//     all, so an empty list is a response that lost its content rather than
//     a real report. It is also what a clock-skewed `after` would produce if
//     one were ever added.
//   - A response in which no entry carries both a location and a parseable
//     timestamp, which leaves nothing selectable.
//
// An individual entry missing either is skipped rather than failing the
// whole response: one unusable hour out of a week must not discard the other
// 167 for both accessors. Unlike parseAccessors there is also no duplicate
// check to make. A repeated location is the normal shape here (one entry per
// hour), and the selection map below is keyed by location, so a duplicate
// series — the failure that takes down Registry.Gather for the whole scrape
// — is impossible by construction rather than by validation.
//
// The result is sorted by location so the cached slice is deterministic for
// a given response. Registry.Gather sorts the exposition independently, so
// this is for the cache and its tests, not for the wire format.
func parseReportsAccessors(b []byte) ([]reportsAccessorStats, error) {
	var entries []reportsAccessorWindow
	if err := json.Unmarshal(b, &entries); err != nil {
		return nil, fmt.Errorf("parse reports/accessors response: %w", err)
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("parse reports/accessors response: empty array, want at least one window")
	}

	// Indexed rather than `for _, e := range entries`: the struct is wide and
	// a week of hourly windows across both accessors is ~336 of them, so
	// ranging by value would copy every entry on every refresh to read two
	// fields from most of them. The copy that does happen is the assignment
	// below, which runs only when a window actually becomes the newest so far
	// for its accessor.
	newest := make(map[string]reportsAccessorStats, len(entries))
	for i := range entries {
		e := &entries[i]
		if e.Location == "" {
			continue
		}
		at, err := time.Parse(reportsAccessorsTimeLayout, e.Time)
		if err != nil {
			continue
		}
		if cur, ok := newest[e.Location]; ok && !at.After(cur.at) {
			continue
		}
		newest[e.Location] = reportsAccessorStats{window: entries[i], at: at}
	}
	if len(newest) == 0 {
		return nil, fmt.Errorf("parse reports/accessors response: %d window(s), none carrying both a location and a parseable %q timestamp", len(entries), reportsAccessorsTimeLayout)
	}

	// Sorting the keys rather than the selected windows: reportsAccessorStats
	// is wide, so ranging the map by value or sorting a slice of it copies far
	// more than sorting the location strings that already order it.
	locations := make([]string, 0, len(newest))
	for loc := range newest {
		locations = append(locations, loc)
	}
	sort.Strings(locations)

	out := make([]reportsAccessorStats, 0, len(locations))
	for _, loc := range locations {
		out = append(out, newest[loc])
	}
	return out, nil
}

// reportsAccessorsGetMetrics is the glue between the I/O step
// (reportsAccessorsData) and the pure parsing step (parseReportsAccessors):
// the shape every collector in this exporter follows, regardless of flavor.
// refresh, below, calls this on its own background schedule; nothing else in
// this file calls the library directly.
func (c *ReportsAccessorsCollector) reportsAccessorsGetMetrics(ctx context.Context) ([]reportsAccessorStats, error) {
	data, err := c.reportsAccessorsData(ctx)
	if err != nil {
		return nil, err
	}
	return parseReportsAccessors(data)
}

// ReportsAccessorsCollector reads GET /v1/reports/accessors: per-accessor
// hourly robotics activity and environmental figures. R1.11.2 records one
// entry per accessor per completed hour and keeps a year of them, of which a
// week is retrievable at a time; this collector exposes each accessor's
// newest entry only.
//
// Its metrics are prefixed `..._accessor_report_`, not
// `..._reports_accessors_`. The subsystem names the resource being reported
// on (the accessor) with `report` as the qualifier, which is the shape
// ReportsLibraryCollector established as `..._library_report_` and
// ReportsDrivesCollector followed as `..._drive_report_`, and which matches
// how every other per-accessor family in this exporter is already spelled
// (`..._accessor_state`, `..._accessor_pivots_total`). The flag namespace
// still follows the endpoint: `--collector.reports_accessors.*`.
//
// **These are per-window quantities, not cumulative counters, so every one
// of them is a Gauge, and none carries a `_total` suffix.** `pivots` here is
// the number of pivots during one specific hour; the next window restarts
// from zero. The genuinely monotonic lifetime counterparts of these same
// five quantities live on AccessorsCollector, as
// `tapelibrary_accessor_pivots_total`, `..._bar_code_scans_total`,
// `..._travel_meters_total`, `..._gets_total` and `..._puts_total`. That
// pairing is the whole point of this collector: a lifetime counter that has
// accumulated millions of gets moves imperceptibly when an accessor stops
// working, while the hourly window it stops contributing to drops to zero
// immediately.
//
// **gets and puts are two metric names rather than one carrying an
// `operation` label**, and `gripper` IS a label on both, which is not a
// contradiction. Prometheus's own exporter-writing guidance names
// read/write and send/receive as the canonical example of related-but-
// distinct concepts that are easier to use as separate metrics; get/put is
// the same shape. `gripper` passes the opposite test: R1.11.2 reports the
// complete cross product (getsGripper1, getsGripper2, putsGripper1,
// putsGripper2), so summing across it is meaningful and no synthetic value
// has to be invented to square the table. The same reasoning splits travel
// into an `axis` label, which the library also reports completely (travelX,
// travelY).
//
// **The value is a snapshot of a window that has already closed, exposed at
// scrape time rather than at the window's own timestamp.** Prometheus's
// honor_timestamps is deliberately not used (see docs/exporter-journal.md,
// "Open questions"), so an accessor that stopped publishing new windows
// would otherwise serve its last one forever and look perfectly healthy.
// tapelibrary_accessor_report_window_timestamp_seconds is the guard against
// exactly that, and is per accessor rather than library-wide for the reason
// parseReportsAccessors gives.
//
// It is the background-refresh variant, which on this target model is not a
// choice: a scrape serves N libraries through one /metrics and must never
// block on a machine that has gone away.
type ReportsAccessorsCollector struct {
	client   *Client
	interval time.Duration
	log      *logger.Logger

	pivots             *prometheus.Desc
	barCodeScans       *prometheus.Desc
	travelMeters       *prometheus.Desc
	gets               *prometheus.Desc
	puts               *prometheus.Desc
	temperatureAverage *prometheus.Desc
	temperatureMin     *prometheus.Desc
	temperatureMax     *prometheus.Desc
	humidityAverage    *prometheus.Desc
	humidityMin        *prometheus.Desc
	humidityMax        *prometheus.Desc
	windowTimestamp    *prometheus.Desc
	windowDuration     *prometheus.Desc
	lastRefreshDesc    *prometheus.Desc

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

// NewReportsAccessorsCollector builds the collector and its Descs. It is
// pure: it starts no goroutine and performs no I/O, which is what makes it
// constructible in tests with no background refresh running. Call Start
// once, after construction, to begin refreshing.
//
// Temperature and humidity are six descriptors rather than one carrying a
// `stat="average|min|max"` label, on the same reasoning
// NewReportsLibraryCollector records: Prometheus's own exporter guidance is
// that a metric should make sense when summed or averaged across its labels,
// and summing an average with a minimum and a maximum is meaningless.
func NewReportsAccessorsCollector(log *logger.Logger, client *Client, interval time.Duration) *ReportsAccessorsCollector {
	return &ReportsAccessorsCollector{
		client:   client,
		interval: interval,
		log:      log,
		pivots: prometheus.NewDesc(
			"tapelibrary_accessor_report_pivots",
			"Number of pivots this accessor performed during the reporting window. A per-window figure, not a cumulative counter: the next window restarts from zero. Its lifetime counterpart is tapelibrary_accessor_pivots_total.",
			[]string{"location"}, nil,
		),
		barCodeScans: prometheus.NewDesc(
			"tapelibrary_accessor_report_bar_code_scans",
			"Number of bar code scans this accessor performed during the reporting window. A per-window figure, not a cumulative counter. Zero in every window of the reference capture: this fleet scans on inventory rather than on every move, so a window recording scans is an inventory pass. Its lifetime counterpart is tapelibrary_accessor_bar_code_scans_total.",
			[]string{"location"}, nil,
		),
		travelMeters: prometheus.NewDesc(
			"tapelibrary_accessor_report_travel_meters",
			"Distance in meters this accessor travelled during the reporting window, per axis: x is horizontal, y is vertical. A per-window figure, not a cumulative counter. Its lifetime counterpart is tapelibrary_accessor_travel_meters_total.",
			[]string{"location", "axis"}, nil,
		),
		gets: prometheus.NewDesc(
			"tapelibrary_accessor_report_gets",
			"Number of times this accessor's gripper engaged to retrieve a cartridge during the reporting window. A per-window figure, not a cumulative counter. Compare the two accessors' shares of the library total: a lifetime counter cannot show one of them stopping, which is what AccessorReportShareCollapsed reads. Its lifetime counterpart is tapelibrary_accessor_gets_total.",
			[]string{"location", "gripper"}, nil,
		),
		puts: prometheus.NewDesc(
			"tapelibrary_accessor_report_puts",
			"Number of times this accessor's gripper engaged to place a cartridge during the reporting window. A per-window figure, not a cumulative counter. Normally tracks gets closely, since a cartridge retrieved is a cartridge put somewhere. Its lifetime counterpart is tapelibrary_accessor_puts_total.",
			[]string{"location", "gripper"}, nil,
		),
		temperatureAverage: prometheus.NewDesc(
			"tapelibrary_accessor_report_temperature_average_celsius",
			"Average temperature in Celsius this accessor measured over the reporting window. Absent, never zero, when the accessor reported no reading: the accessors on the reference fleet carry no such sensor and report null in every window, exactly as tapelibrary_accessor_temperature_celsius does.",
			[]string{"location"}, nil,
		),
		temperatureMin: prometheus.NewDesc(
			"tapelibrary_accessor_report_temperature_min_celsius",
			"Lowest temperature in Celsius this accessor measured over the reporting window. Absent, never zero, when the accessor reported no reading.",
			[]string{"location"}, nil,
		),
		temperatureMax: prometheus.NewDesc(
			"tapelibrary_accessor_report_temperature_max_celsius",
			"Highest temperature in Celsius this accessor measured over the reporting window. Absent, never zero, when the accessor reported no reading.",
			[]string{"location"}, nil,
		),
		humidityAverage: prometheus.NewDesc(
			"tapelibrary_accessor_report_humidity_average_ratio",
			"Average relative humidity this accessor measured over the reporting window, as a ratio from 0 to 1 (the API reports a 0-100 percentage). Absent, never zero, when the accessor reported no reading.",
			[]string{"location"}, nil,
		),
		humidityMin: prometheus.NewDesc(
			"tapelibrary_accessor_report_humidity_min_ratio",
			"Lowest relative humidity this accessor measured over the reporting window, as a ratio from 0 to 1. Absent, never zero, when the accessor reported no reading.",
			[]string{"location"}, nil,
		),
		humidityMax: prometheus.NewDesc(
			"tapelibrary_accessor_report_humidity_max_ratio",
			"Highest relative humidity this accessor measured over the reporting window, as a ratio from 0 to 1. Absent, never zero, when the accessor reported no reading.",
			[]string{"location"}, nil,
		),
		windowTimestamp: prometheus.NewDesc(
			"tapelibrary_accessor_report_window_timestamp_seconds",
			"Unix time the library stamped on the reporting window these metrics describe, for this accessor. Per accessor rather than library-wide so that one of the two dropping out of the report is visible: the library publishes one window per completed hour, so alert if time() - this exceeds a few hours. Every other metric in this family would otherwise keep serving a stale window's values indefinitely, looking healthy.",
			[]string{"location"}, nil,
		),
		windowDuration: prometheus.NewDesc(
			"tapelibrary_accessor_report_window_duration_seconds",
			"Number of seconds this accessor's reporting window covers, as the library reports it. 3600 on every window in the reference capture. Exposed so that a window covering less than a full hour is visible rather than assumed away: its activity figures would be proportionally low through no fault of the accessor.",
			[]string{"location"}, nil,
		),
		lastRefreshDesc: prometheus.NewDesc(
			"tapelibrary_accessor_report_last_refresh_timestamp_seconds",
			"Unix time of the last successful reports/accessors refresh. Alert if time() - this > 2 x the collector's configured interval. Named for this collector's metric subsystem rather than its registered name, so a subsystem sweep finds the freshness of the data it is reading. Distinct from tapelibrary_accessor_report_window_timestamp_seconds, which ages even while this one stays current.",
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
func (c *ReportsAccessorsCollector) Start(ctx context.Context) {
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
func (c *ReportsAccessorsCollector) Done() <-chan struct{} {
	return c.done
}

// refresh performs the one I/O call (reportsAccessorsGetMetrics, via the
// injected *Client) and, on success, atomically replaces the cache. On error
// it logs and returns, leaving the previous cache and lastRefresh untouched,
// fail-open: a transient failure serves the last-known-good data instead of
// dropping the series, and the freshness gauge is the signal that a refresh
// is stale, not a dropped scrape.
func (c *ReportsAccessorsCollector) refresh(ctx context.Context) {
	accessors, err := c.reportsAccessorsGetMetrics(ctx)
	if err != nil {
		c.log.Error("Failed to refresh reports/accessors metrics: keeping previous cache", "err", err)
		return
	}

	// Ten always-emitted series per accessor: pivots, bar code scans, travel
	// on two axes, gets and puts on two grippers each, plus that accessor's
	// own window timestamp and duration. None of them can be absent on a
	// parsed window, which is what guarantees this collector never emits a
	// scrape carrying the freshness gauge alone after a successful refresh.
	metrics := make([]prometheus.Metric, 0, len(accessors)*16)
	for i := range accessors {
		w := &accessors[i].window
		loc := w.Location
		metrics = append(metrics,
			prometheus.MustNewConstMetric(c.pivots, prometheus.GaugeValue, w.Pivots, loc),
			prometheus.MustNewConstMetric(c.barCodeScans, prometheus.GaugeValue, w.BarCodeScans, loc),
			prometheus.MustNewConstMetric(c.travelMeters, prometheus.GaugeValue, w.TravelX, loc, "x"),
			prometheus.MustNewConstMetric(c.travelMeters, prometheus.GaugeValue, w.TravelY, loc, "y"),
			prometheus.MustNewConstMetric(c.gets, prometheus.GaugeValue, w.GetsGripper1, loc, "1"),
			prometheus.MustNewConstMetric(c.gets, prometheus.GaugeValue, w.GetsGripper2, loc, "2"),
			prometheus.MustNewConstMetric(c.puts, prometheus.GaugeValue, w.PutsGripper1, loc, "1"),
			prometheus.MustNewConstMetric(c.puts, prometheus.GaugeValue, w.PutsGripper2, loc, "2"),
			prometheus.MustNewConstMetric(c.windowTimestamp, prometheus.GaugeValue, float64(accessors[i].at.Unix()), loc),
			prometheus.MustNewConstMetric(c.windowDuration, prometheus.GaugeValue, w.Duration, loc),
		)

		// The six environmental readings, each emitted only if this accessor
		// actually reported it. A null here means the accessor could not take
		// the reading, not that the reading was zero: a 0 °C / 0% RH would be
		// a freezing, bone dry library, outside R1.11.2's operating envelope
		// in both directions. Every entry of the reference capture takes this
		// branch, since the accessors on this fleet carry no such sensor.
		// Humidity arrives as a 0-100 percentage and is emitted as a ratio,
		// per the `_ratio` unit suffix and matching
		// tapelibrary_accessor_humidity_ratio.
		for _, m := range []struct {
			desc  *prometheus.Desc
			value *float64
			scale float64
		}{
			{c.temperatureAverage, w.TemperatureAverage, 1},
			{c.temperatureMin, w.TemperatureMin, 1},
			{c.temperatureMax, w.TemperatureMax, 1},
			{c.humidityAverage, w.HumidityAverage, 0.01},
			{c.humidityMin, w.HumidityMin, 0.01},
			{c.humidityMax, w.HumidityMax, 0.01},
		} {
			if m.value == nil {
				continue
			}
			metrics = append(metrics, prometheus.MustNewConstMetric(m.desc, prometheus.GaugeValue, *m.value*m.scale, loc))
		}
	}

	c.mu.Lock()
	c.cached = metrics
	c.lastRefresh = time.Now()
	c.mu.Unlock()
}

// Describe sends every one of this collector's descriptors, including the
// six environmental ones that no refresh on the reference fleet will ever
// emit and the freshness gauge. Constant regardless of scrape or refresh
// outcome, which is what makes prometheus.DescribeByCollect unnecessary
// here.
func (c *ReportsAccessorsCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.pivots
	ch <- c.barCodeScans
	ch <- c.travelMeters
	ch <- c.gets
	ch <- c.puts
	ch <- c.temperatureAverage
	ch <- c.temperatureMin
	ch <- c.temperatureMax
	ch <- c.humidityAverage
	ch <- c.humidityMin
	ch <- c.humidityMax
	ch <- c.windowTimestamp
	ch <- c.windowDuration
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
func (c *ReportsAccessorsCollector) Collect(ch chan<- prometheus.Metric) {
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
