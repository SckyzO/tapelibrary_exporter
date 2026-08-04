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

// reportsLibraryTimeLayout parses the `time` field of a GET
// /v1/reports/library entry. Same layout as every other timestamp this
// library reports (see driveLastCleanedLayout, the first collector to meet
// it): the zone offset carries no colon, so it is not time.RFC3339 and
// parsing it as such fails.
//
// Named separately rather than used through driveLastCleanedLayout so this
// collector reads on its own, per the convention the journal records: one
// const per collector, all currently the same value.
const reportsLibraryTimeLayout = driveLastCleanedLayout

// reportsLibraryMBToBytes converts the endpoint's three data-volume fields to
// the base unit Prometheus takes. R1.11.2 documents all three as "The number
// of MB of data ..." and, as with dataWrittenToCartridge on
// /v1/dataCartridges/lifetimeMetrics, does not say whether the MB is decimal
// or binary. The decimal reading (1 MB = 1e6) is taken here for one reason
// only: it is the reading dataCartridgeUsageMBToBytes already took, and two
// collectors converting the same manufacturer's "MB" two different ways would
// make the two families incomparable. If IBM means MiB, both are 4.9% low
// together, which is a single correction rather than a discrepancy to hunt.
//
// Deliberately not an alias of dataCartridgeUsageMBToBytes: these are
// different fields on different endpoints that happen to share an ambiguity,
// and coupling them would make it look as though the manual states somewhere
// that they are the same unit. It does not.
const reportsLibraryMBToBytes = 1e6

// reportsLibraryWindow is the parsed shape of one entry from GET
// /v1/reports/library. The endpoint returns one entry per completed hour,
// newest first in the 2026-07-28 capture, covering the last week unless the
// `after`/`before` query parameters narrow it.
//
// The seven activity fields are plain float64 rather than pointers, and the
// six environmental ones are pointers, and that split is deliberate: a count
// of things that happened during an hour has a meaningful zero (a library
// mounts nothing overnight and that is a reading, not a gap), while a
// temperature or humidity has none. R1.11.2 sources the environmental figures
// from the drives, so a library with no drive able to report leaves them
// absent, and a 0 °C / 0% RH emitted in their place would be a freezing, bone
// dry library that the operating-envelope alerts would act on. Absent emits no
// series at all, per the journal's rule for a reading the hardware cannot take.
type reportsLibraryWindow struct {
	Time                    string   `json:"time"`
	Duration                float64  `json:"duration"`
	Mounts                  float64  `json:"mounts"`
	Imports                 float64  `json:"imports"`
	Exports                 float64  `json:"exports"`
	Moves                   float64  `json:"moves"`
	DataReadByHosts         float64  `json:"dataReadByHosts"`
	DataWrittenByHosts      float64  `json:"dataWrittenByHosts"`
	DataWrittenToCartridges float64  `json:"dataWrittenToCartridges"`
	TemperatureAverage      *float64 `json:"temperatureAverage"`
	TemperatureMin          *float64 `json:"temperatureMin"`
	TemperatureMax          *float64 `json:"temperatureMax"`
	HumidityAverage         *float64 `json:"humidityAverage"`
	HumidityMin             *float64 `json:"humidityMin"`
	HumidityMax             *float64 `json:"humidityMax"`
}

// reportsLibraryStats is the one window this collector exposes, together with
// its own parsed timestamp. The timestamp is carried out of the parser rather
// than re-parsed at emission time because selecting the window already
// required parsing it, and because a window whose time could not be parsed
// never becomes the selection (see parseReportsLibrary).
type reportsLibraryStats struct {
	window reportsLibraryWindow
	at     time.Time
}

// reportsLibraryData is this collector's only I/O: it fetches the raw response
// body from the configured library. Kept separate from parsing
// (parseReportsLibrary, below) so parsing stays pure and unit-testable without
// a live library.
//
// The request carries no `after`/`before` parameter. Unlike GET /v1/events,
// which returns every event the library ever recorded and therefore must be
// bounded by a lookback, this endpoint already defaults to the last week:
// roughly 168 hourly entries, ~67 KB at the capture's ~400 bytes per entry.
// That is small enough that narrowing it would buy nothing and would add a
// clock-skew failure mode, where a library running ahead of this host answers
// an `after` in its own future with an empty array.
func (c *ReportsLibraryCollector) reportsLibraryData(ctx context.Context) ([]byte, error) {
	return c.client.Fetch(ctx, "/reports/library")
}

// parseReportsLibrary decodes reportsLibraryData's response body and selects
// the single window this collector exposes: the newest one, by its own `time`
// field. Pure: no I/O, no logging, no side effects.
//
// Selection is by timestamp rather than by position. The 2026-07-28 capture
// happens to be ordered newest first, but R1.11.2 documents no ordering for
// this endpoint, and a collector that silently depended on one would report
// week-old activity as current the first time a firmware release changed it.
//
// An entry whose `time` is missing, empty or unparseable cannot be ordered
// against the others, so it is skipped for selection rather than being
// compared as a zero time (which would sort it last and could never win) or
// emitted with a fabricated timestamp. If that leaves nothing selectable, the
// response is rejected, exactly as an empty array is: refresh's fail-open
// behaviour then keeps the previous good cache, which is the right outcome
// for a response this collector cannot interpret. It is also what lets Collect
// emit the window timestamp unconditionally, since a successful parse always
// carries a real one.
func parseReportsLibrary(b []byte) (reportsLibraryStats, error) {
	var entries []reportsLibraryWindow
	if err := json.Unmarshal(b, &entries); err != nil {
		return reportsLibraryStats{}, fmt.Errorf("parse reports/library response: %w", err)
	}
	if len(entries) == 0 {
		return reportsLibraryStats{}, fmt.Errorf("parse reports/library response: empty array, want at least one window")
	}

	var (
		newest reportsLibraryStats
		found  bool
	)
	// Indexed rather than `for _, e := range entries`: the struct is 128
	// bytes and a week of hourly windows is ~168 of them, so ranging by
	// value would copy every entry on every refresh to read one field from
	// most of them. The copy that does happen is the assignment below, which
	// runs only when a window actually becomes the newest so far.
	for i := range entries {
		at, err := time.Parse(reportsLibraryTimeLayout, entries[i].Time)
		if err != nil {
			continue
		}
		if !found || at.After(newest.at) {
			newest = reportsLibraryStats{window: entries[i], at: at}
			found = true
		}
	}
	if !found {
		return reportsLibraryStats{}, fmt.Errorf("parse reports/library response: %d window(s), none carrying a parseable %q timestamp", len(entries), reportsLibraryTimeLayout)
	}
	return newest, nil
}

// reportsLibraryGetMetrics is the glue between the I/O step
// (reportsLibraryData) and the pure parsing step (parseReportsLibrary): the
// shape every collector in this exporter follows, regardless of flavor.
// refresh, below, calls this on its own background schedule; nothing else in
// this file calls the library directly.
func (c *ReportsLibraryCollector) reportsLibraryGetMetrics(ctx context.Context) (reportsLibraryStats, error) {
	data, err := c.reportsLibraryData(ctx)
	if err != nil {
		return reportsLibraryStats{}, err
	}
	return parseReportsLibrary(data)
}

// ReportsLibraryCollector reads GET /v1/reports/library: the library's own
// hourly activity and environmental report. R1.11.2 records one entry per
// completed hour and keeps a year of them, of which a week is retrievable at
// a time; this collector exposes the newest entry only.
//
// Its metrics are prefixed `..._library_report_`, not `..._reports_library_`.
// The subsystem names the resource being reported on (the library) with
// `report` as the qualifier, which is the shape the sibling collectors will
// take as `..._drive_report_` and `..._accessor_report_`, matching how every
// other per-resource family in this exporter is already spelled
// (`..._drive_state`, `..._accessor_humidity_ratio`). The flag namespace
// still follows the endpoint: `--collector.reports_library.*`.
//
// **These are per-window quantities, not cumulative counters, so every one of
// them is a Gauge including the seven activity figures.** `mounts` is the
// number of mounts during one specific hour; the next window restarts the
// count from zero. rate() and increase() are meaningless here and _total
// would be a lie. The genuinely monotonic device counters this library
// exposes live on /v1/accessors, /v1/slots and
// /v1/dataCartridges/lifetimeMetrics instead.
//
// **The value is a snapshot of a window that has already closed, exposed at
// scrape time rather than at the window's own timestamp.** Prometheus's
// honor_timestamps is deliberately not used (see docs/exporter-journal.md,
// "Open questions"), so a library that stopped publishing new windows would
// otherwise serve its last one forever and look perfectly healthy.
// tapelibrary_library_report_window_timestamp_seconds is the guard against
// exactly that, and is why it is emitted unconditionally.
//
// It is the background-refresh variant, which on this target model is not a
// choice: a scrape serves N libraries through one /metrics and must never
// block on a machine that has gone away.
type ReportsLibraryCollector struct {
	client   *Client
	interval time.Duration
	log      *logger.Logger

	mounts              *prometheus.Desc
	imports             *prometheus.Desc
	exports             *prometheus.Desc
	moves               *prometheus.Desc
	readByHosts         *prometheus.Desc
	writtenByHosts      *prometheus.Desc
	writtenToCartridges *prometheus.Desc
	temperatureAverage  *prometheus.Desc
	temperatureMin      *prometheus.Desc
	temperatureMax      *prometheus.Desc
	humidityAverage     *prometheus.Desc
	humidityMin         *prometheus.Desc
	humidityMax         *prometheus.Desc
	windowTimestamp     *prometheus.Desc
	windowDuration      *prometheus.Desc
	lastRefreshDesc     *prometheus.Desc

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

// NewReportsLibraryCollector builds the collector and its Descs. It is pure:
// it starts no goroutine and performs no I/O, which is what makes it
// constructible in tests with no background refresh running. Call Start once,
// after construction, to begin refreshing.
//
// Temperature and humidity are six descriptors rather than one carrying a
// `stat="average|min|max"` label, deliberately. Prometheus's own exporter
// guidance is that a metric should make sense when summed or averaged across
// its labels, and summing an average with a minimum and a maximum is
// meaningless: the same reason its naming guidance prefers separate families
// over a `{result="success"|"failure"}` label. Six names also let an
// operating-envelope rule name the statistic it means (the hottest drive, or
// the whole population's average) in the metric rather than in a matcher.
func NewReportsLibraryCollector(log *logger.Logger, client *Client, interval time.Duration) *ReportsLibraryCollector {
	return &ReportsLibraryCollector{
		client:   client,
		interval: interval,
		log:      log,
		mounts: prometheus.NewDesc(
			"tapelibrary_library_report_mounts",
			"Number of cartridges mounted into a drive during the reporting window. A per-window figure, not a cumulative counter: the next window restarts from zero.",
			nil, nil,
		),
		imports: prometheus.NewDesc(
			"tapelibrary_library_report_imports",
			"Number of cartridges added to the library during the reporting window. R1.11.2 counts an import only once the host has issued the SCSI move media command or the cartridge was manually assigned to a logical library.",
			nil, nil,
		),
		exports: prometheus.NewDesc(
			"tapelibrary_library_report_exports",
			"Number of cartridges removed from the library during the reporting window, counted once the cartridge has physically reached the I/O station.",
			nil, nil,
		),
		moves: prometheus.NewDesc(
			"tapelibrary_library_report_moves",
			"Number of times a cartridge was moved from one location to another during the reporting window, host-initiated and library-initiated alike. Each move is one get plus one put by the gripper, and the figure includes mounts, demounts, imports and exports. Cartridges shuffled aside to reach a deeper tier are not counted.",
			nil, nil,
		),
		readByHosts: prometheus.NewDesc(
			"tapelibrary_library_report_read_by_hosts_bytes",
			"Bytes read from cartridges by all drives during the reporting window. Converted from the API's megabytes, read decimally (1 MB = 1e6 bytes) to match how this exporter already converts the cartridge lifetime counters.",
			nil, nil,
		),
		writtenByHosts: prometheus.NewDesc(
			"tapelibrary_library_report_written_by_hosts_bytes",
			"Bytes written to cartridges by all drives during the reporting window, measured before compression. Divide by tapelibrary_library_report_written_to_cartridges_bytes for the window's average compression ratio. Converted from the API's decimal megabytes.",
			nil, nil,
		),
		writtenToCartridges: prometheus.NewDesc(
			"tapelibrary_library_report_written_to_cartridges_bytes",
			"Bytes actually written onto the media by all drives during the reporting window, after compression. Converted from the API's decimal megabytes.",
			nil, nil,
		),
		temperatureAverage: prometheus.NewDesc(
			"tapelibrary_library_report_temperature_average_celsius",
			"Average temperature in Celsius across all drives over the reporting window. Measured inside the library at the drives, so it reads above the ambient figure R1.11.2's operating envelope is written against. Absent, never zero, when no drive reported a reading.",
			nil, nil,
		),
		temperatureMin: prometheus.NewDesc(
			"tapelibrary_library_report_temperature_min_celsius",
			"Lowest temperature in Celsius reported by any drive over the reporting window. Absent, never zero, when no drive reported a reading.",
			nil, nil,
		),
		temperatureMax: prometheus.NewDesc(
			"tapelibrary_library_report_temperature_max_celsius",
			"Highest temperature in Celsius reported by any drive over the reporting window. Absent, never zero, when no drive reported a reading.",
			nil, nil,
		),
		humidityAverage: prometheus.NewDesc(
			"tapelibrary_library_report_humidity_average_ratio",
			"Average relative humidity across all drives over the reporting window, as a ratio from 0 to 1 (the API reports a 0-100 percentage). Absent, never zero, when no drive reported a reading.",
			nil, nil,
		),
		humidityMin: prometheus.NewDesc(
			"tapelibrary_library_report_humidity_min_ratio",
			"Lowest relative humidity reported by any drive over the reporting window, as a ratio from 0 to 1. Absent, never zero, when no drive reported a reading.",
			nil, nil,
		),
		humidityMax: prometheus.NewDesc(
			"tapelibrary_library_report_humidity_max_ratio",
			"Highest relative humidity reported by any drive over the reporting window, as a ratio from 0 to 1. Absent, never zero, when no drive reported a reading.",
			nil, nil,
		),
		windowTimestamp: prometheus.NewDesc(
			"tapelibrary_library_report_window_timestamp_seconds",
			"Unix time the library stamped on the reporting window these metrics describe. The library publishes one window per completed hour, so alert if time() - this exceeds a few hours: every other metric in this family would otherwise keep serving a stale window's values indefinitely, looking healthy.",
			nil, nil,
		),
		windowDuration: prometheus.NewDesc(
			"tapelibrary_library_report_window_duration_seconds",
			"Number of seconds the reporting window covers, as the library reports it. 3600 on every window in the reference capture. Exposed so that a window covering less than a full hour is visible rather than assumed away: its activity figures would be proportionally low through no fault of the library.",
			nil, nil,
		),
		lastRefreshDesc: prometheus.NewDesc(
			"tapelibrary_library_report_last_refresh_timestamp_seconds",
			"Unix time of the last successful reports/library refresh. Alert if time() - this > 2 x the collector's configured interval. Named for this collector's metric subsystem rather than its registered name, so a subsystem sweep finds the freshness of the data it is reading. Distinct from tapelibrary_library_report_window_timestamp_seconds, which ages even while this one stays current.",
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
func (c *ReportsLibraryCollector) Start(ctx context.Context) {
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
func (c *ReportsLibraryCollector) Done() <-chan struct{} {
	return c.done
}

// refresh performs the one I/O call (reportsLibraryGetMetrics, via the
// injected *Client) and, on success, atomically replaces the cache. On error
// it logs and returns, leaving the previous cache and lastRefresh untouched,
// fail-open: a transient failure serves the last-known-good data instead of
// dropping the series, and the freshness gauge is the signal that a refresh
// is stale, not a dropped scrape.
func (c *ReportsLibraryCollector) refresh(ctx context.Context) {
	stats, err := c.reportsLibraryGetMetrics(ctx)
	if err != nil {
		c.log.Error("Failed to refresh reports/library metrics: keeping previous cache", "err", err)
		return
	}
	w := stats.window

	// Nine always-emitted series: the seven per-window activity figures, plus
	// the window's own timestamp and duration. None of them can be absent on
	// a parsed window, which is what guarantees this collector never emits a
	// scrape carrying the freshness gauge alone after a successful refresh.
	metrics := make([]prometheus.Metric, 0, 15)
	metrics = append(metrics,
		prometheus.MustNewConstMetric(c.mounts, prometheus.GaugeValue, w.Mounts),
		prometheus.MustNewConstMetric(c.imports, prometheus.GaugeValue, w.Imports),
		prometheus.MustNewConstMetric(c.exports, prometheus.GaugeValue, w.Exports),
		prometheus.MustNewConstMetric(c.moves, prometheus.GaugeValue, w.Moves),
		prometheus.MustNewConstMetric(c.readByHosts, prometheus.GaugeValue, w.DataReadByHosts*reportsLibraryMBToBytes),
		prometheus.MustNewConstMetric(c.writtenByHosts, prometheus.GaugeValue, w.DataWrittenByHosts*reportsLibraryMBToBytes),
		prometheus.MustNewConstMetric(c.writtenToCartridges, prometheus.GaugeValue, w.DataWrittenToCartridges*reportsLibraryMBToBytes),
		prometheus.MustNewConstMetric(c.windowTimestamp, prometheus.GaugeValue, float64(stats.at.Unix())),
		prometheus.MustNewConstMetric(c.windowDuration, prometheus.GaugeValue, w.Duration),
	)

	// The six environmental readings, each emitted only if the library
	// actually reported it. A null here means no drive could take the
	// reading, not that the reading was zero, and the operating-envelope
	// alerting rules read these directly: a 0 °C / 0% RH standing in for
	// "unknown" would page on a library that is merely quiet about its
	// sensors. Humidity arrives as a 0-100 percentage and is emitted as a
	// ratio, per the `_ratio` unit suffix and matching
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
		metrics = append(metrics, prometheus.MustNewConstMetric(m.desc, prometheus.GaugeValue, *m.value*m.scale))
	}

	c.mu.Lock()
	c.cached = metrics
	c.lastRefresh = time.Now()
	c.mu.Unlock()
}

// Describe sends every one of this collector's descriptors, including the six
// environmental ones that a given refresh may not emit and the freshness
// gauge. Constant regardless of scrape or refresh outcome, which is what
// makes prometheus.DescribeByCollect unnecessary here.
func (c *ReportsLibraryCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.mounts
	ch <- c.imports
	ch <- c.exports
	ch <- c.moves
	ch <- c.readByHosts
	ch <- c.writtenByHosts
	ch <- c.writtenToCartridges
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
func (c *ReportsLibraryCollector) Collect(ch chan<- prometheus.Metric) {
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
