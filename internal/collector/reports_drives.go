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

// reportsDrivesTimeLayout parses the `time` field of a GET /v1/reports/drives
// entry. Same layout as every other timestamp this library reports (see
// driveLastCleanedLayout, the first collector to meet it): the zone offset
// carries no colon, so it is not time.RFC3339 and parsing it as such fails.
//
// Named separately rather than used through driveLastCleanedLayout so this
// collector reads on its own, per the convention the journal records: one
// const per collector, all currently the same value.
const reportsDrivesTimeLayout = driveLastCleanedLayout

// reportsDrivesMBToBytes converts the endpoint's three data-volume fields to
// the base unit Prometheus takes. R1.11.2 documents all three as "The number
// of MB of data ..." and does not say whether the MB is decimal or binary. The
// decimal reading (1 MB = 1e6) is taken here for one reason only: it is the
// reading dataCartridgeUsageMBToBytes and reportsLibraryMBToBytes already
// took, and three collectors converting the same manufacturer's "MB" three
// different ways would make the families incomparable. If IBM means MiB, all
// three are 4.9% low together, which is a single correction rather than a
// discrepancy to hunt.
//
// Deliberately not an alias of reportsLibraryMBToBytes: these are different
// fields on different endpoints that happen to share an ambiguity, and
// coupling them would make it look as though the manual states somewhere that
// they are the same unit. It does not.
const reportsDrivesMBToBytes = 1e6

// reportsDriveWindow is the parsed shape of one entry from GET
// /v1/reports/drives. The endpoint returns one entry per drive per completed
// hour, covering the last week unless the `after`/`before` query parameters
// narrow it: 40 drives x 168 hours in the reference capture's fleet.
//
// The eight activity fields are plain float64 rather than pointers, and the
// six environmental ones are pointers, and that split is the same one
// reportsLibraryWindow makes for the same reason: a count of things that
// happened during an hour has a meaningful zero (a drive mounts nothing
// overnight and that is a reading, not a gap), while a temperature or humidity
// has none. A drive that could not take the reading leaves them absent, and a
// 0 °C / 0% RH emitted in their place would be a freezing, bone dry drive that
// the operating-envelope alerts would act on.
//
// `sn` is deliberately not parsed. The drive's serial already ships on
// tapelibrary_drive_info, keyed by the same location, and the journal's rule
// is that an identity attribute lives on exactly one _info series: re-emitting
// it here would give operators two places to read the same string and two
// places for it to disagree. location is the join key between the two
// families.
type reportsDriveWindow struct {
	Location                string   `json:"location"`
	Time                    string   `json:"time"`
	Duration                float64  `json:"duration"`
	Mounts                  float64  `json:"mounts"`
	Cleans                  float64  `json:"cleans"`
	DataReadByHosts         float64  `json:"dataReadByHosts"`
	DataWrittenByHosts      float64  `json:"dataWrittenByHosts"`
	DataWrittenToCartridges float64  `json:"dataWrittenToCartridges"`
	ErrorsCorrectedRead     float64  `json:"errorsCorrectedRead"`
	ErrorsCorrectedWrite    float64  `json:"errorsCorrectedWrite"`
	ErrorsUncorrected       float64  `json:"errorsUncorrected"`
	TemperatureAverage      *float64 `json:"temperatureAverage"`
	TemperatureMin          *float64 `json:"temperatureMin"`
	TemperatureMax          *float64 `json:"temperatureMax"`
	HumidityAverage         *float64 `json:"humidityAverage"`
	HumidityMin             *float64 `json:"humidityMin"`
	HumidityMax             *float64 `json:"humidityMax"`
}

// reportsDriveStats is the one window this collector exposes for a single
// drive, together with its own parsed timestamp. The timestamp is carried out
// of the parser rather than re-parsed at emission time because selecting the
// window already required parsing it, and because a window whose time could
// not be parsed never becomes a selection (see parseReportsDrives).
type reportsDriveStats struct {
	window reportsDriveWindow
	at     time.Time
}

// reportsDrivesData is this collector's only I/O: it fetches the raw response
// body from the configured library. Kept separate from parsing
// (parseReportsDrives, below) so parsing stays pure and unit-testable without
// a live library.
//
// The request carries no `after`/`before` parameter, matching
// ReportsLibraryCollector, and unlike GET /v1/events, which must be bounded
// because a bare request returns every event the library ever recorded. The
// cost is real and was weighed rather than overlooked: the default week is
// ~168 windows x 40 drives, roughly 3.3 MB at the capture's ~500 bytes per
// entry, against reports/library's ~67 KB for the same week. Two things pay
// for it. An `after` parameter would add a clock-skew failure mode, where a
// library running ahead of this host answers an `after` in its own future with
// an empty array — the exact response parseReportsDrives must reject, so skew
// would present as a collector that silently stops advancing. And the
// concurrency ceiling of 1 means the transfer blocks its sibling collectors on
// the same library, which is why the interval is an hour rather than
// reports/library's 15m: matching the endpoint's own publication cadence
// instead of quartering it cuts the transfer fourfold for data that cannot
// change in between.
func (c *ReportsDrivesCollector) reportsDrivesData(ctx context.Context) ([]byte, error) {
	return c.client.Fetch(ctx, "/reports/drives")
}

// parseReportsDrives decodes reportsDrivesData's response body and selects,
// for each drive, the single window this collector exposes: that drive's
// newest, by its own `time` field. Pure: no I/O, no logging, no side effects.
//
// Selection is per drive rather than library-wide. All 40 drives share the
// newest window in the reference capture, but a drive that was removed, went
// offline, or was installed part-way through the week has its own last
// reported hour, and collapsing them onto a single library-wide window would
// either drop that drive's series entirely or serve another drive's timestamp
// against its values.
//
// Selection is by timestamp rather than by position, for the reason
// parseReportsLibrary already gives: R1.11.2 documents no ordering for this
// endpoint, and a collector that silently depended on the capture's happening
// to arrive newest-first would report week-old activity as current the first
// time a firmware release changed it.
//
// Two inputs are rejected outright rather than passed through — refresh keeps
// the previous cache on error, which is the right outcome for a response this
// collector cannot interpret:
//
//   - An empty array. A library with no drive cannot serve any host, so an
//     empty list is a response that lost its content rather than a real
//     report. It is also what a clock-skewed `after` would produce if one were
//     ever added.
//   - A response in which no entry carries both a location and a parseable
//     timestamp, which leaves nothing selectable.
//
// An individual entry missing either is skipped rather than failing the whole
// response: unlike parseDrives, where a nameless entry means the inventory
// itself is malformed, one unusable hour out of a week must not discard the
// other 167 for every drive. Unlike parseDrives there is also no duplicate
// check to make. A repeated location is the normal shape here (one entry per
// hour), and the selection map below is keyed by location, so a duplicate
// series — the failure that takes down Registry.Gather for the whole scrape —
// is impossible by construction rather than by validation.
//
// The result is sorted by location so the cached slice is deterministic for a
// given response. Registry.Gather sorts the exposition independently, so this
// is for the cache and its tests, not for the wire format.
func parseReportsDrives(b []byte) ([]reportsDriveStats, error) {
	var entries []reportsDriveWindow
	if err := json.Unmarshal(b, &entries); err != nil {
		return nil, fmt.Errorf("parse reports/drives response: %w", err)
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("parse reports/drives response: empty array, want at least one window")
	}

	// Indexed rather than `for _, e := range entries`: the struct is wide and
	// a week of hourly windows across 40 drives is ~6 720 of them, so ranging
	// by value would copy every entry on every refresh to read two fields from
	// most of them. The copy that does happen is the assignment below, which
	// runs only when a window actually becomes the newest so far for its
	// drive.
	newest := make(map[string]reportsDriveStats, len(entries))
	for i := range entries {
		e := &entries[i]
		if e.Location == "" {
			continue
		}
		at, err := time.Parse(reportsDrivesTimeLayout, e.Time)
		if err != nil {
			continue
		}
		if cur, ok := newest[e.Location]; ok && !at.After(cur.at) {
			continue
		}
		newest[e.Location] = reportsDriveStats{window: entries[i], at: at}
	}
	if len(newest) == 0 {
		return nil, fmt.Errorf("parse reports/drives response: %d window(s), none carrying both a location and a parseable %q timestamp", len(entries), reportsDrivesTimeLayout)
	}

	// Sorting the keys rather than the selected windows: reportsDriveStats is
	// 176 bytes, so ranging the map by value or sorting a slice of it copies
	// far more than sorting the location strings that already order it.
	locations := make([]string, 0, len(newest))
	for loc := range newest {
		locations = append(locations, loc)
	}
	sort.Strings(locations)

	out := make([]reportsDriveStats, 0, len(locations))
	for _, loc := range locations {
		out = append(out, newest[loc])
	}
	return out, nil
}

// reportsDrivesGetMetrics is the glue between the I/O step
// (reportsDrivesData) and the pure parsing step (parseReportsDrives): the
// shape every collector in this exporter follows, regardless of flavor.
// refresh, below, calls this on its own background schedule; nothing else in
// this file calls the library directly.
func (c *ReportsDrivesCollector) reportsDrivesGetMetrics(ctx context.Context) ([]reportsDriveStats, error) {
	data, err := c.reportsDrivesData(ctx)
	if err != nil {
		return nil, err
	}
	return parseReportsDrives(data)
}

// ReportsDrivesCollector reads GET /v1/reports/drives: per-drive hourly
// activity, error and environmental figures. R1.11.2 records one entry per
// drive per completed hour and keeps a year of them, of which a week is
// retrievable at a time; this collector exposes each drive's newest entry
// only.
//
// Its metrics are prefixed `..._drive_report_`, not `..._reports_drives_`.
// The subsystem names the resource being reported on (the drive) with `report`
// as the qualifier, which is the shape ReportsLibraryCollector established as
// `..._library_report_` and which matches how every other per-drive family in
// this exporter is already spelled (`..._drive_state`, `..._drive_info`). The
// flag namespace still follows the endpoint: `--collector.reports_drives.*`.
//
// **These are per-window quantities, not cumulative counters, so every one of
// them is a Gauge including the three error figures.** `errorsCorrectedRead`
// is the number of corrected read errors during one specific hour; the next
// window restarts the count from zero. rate() and increase() are meaningless
// here and _total would be a lie. The genuinely monotonic error counters this
// library exposes live on /v1/dataCartridges/lifetimeMetrics instead, per
// cartridge rather than per drive.
//
// **The three error figures are three metric names rather than one carrying a
// `direction` label**, which is where this collector deliberately parts
// company with DataCartridgesLifetimeCollector's `{direction, correction}`
// pair. Prometheus's own exporter-writing guidance names read/write as its
// canonical example of related-but-distinct concepts that are easier to use as
// separate metrics than as one metric with a label. That collector's cross
// product is complete (corrected and uncorrected, each read and write), which
// is what justifies the labelled histogram there; this endpoint reports
// `errorsUncorrected` with no direction at all, so a `direction` label here
// would need a synthetic value the library never sends.
//
// **The value is a snapshot of a window that has already closed, exposed at
// scrape time rather than at the window's own timestamp.** Prometheus's
// honor_timestamps is deliberately not used (see docs/exporter-journal.md,
// "Open questions"), so a drive that stopped publishing new windows would
// otherwise serve its last one forever and look perfectly healthy.
// tapelibrary_drive_report_window_timestamp_seconds is the guard against
// exactly that, and is per drive rather than library-wide for the reason
// parseReportsDrives gives: a single drive dropping out of the report is the
// failure this gauge exists to surface, and a library-wide timestamp would
// hide it behind its 39 healthy siblings.
//
// It is the background-refresh variant, which on this target model is not a
// choice: a scrape serves N libraries through one /metrics and must never
// block on a machine that has gone away.
type ReportsDrivesCollector struct {
	client   *Client
	interval time.Duration
	log      *logger.Logger

	mounts               *prometheus.Desc
	cleans               *prometheus.Desc
	readByHosts          *prometheus.Desc
	writtenByHosts       *prometheus.Desc
	writtenToCartridges  *prometheus.Desc
	errorsCorrectedRead  *prometheus.Desc
	errorsCorrectedWrite *prometheus.Desc
	errorsUncorrected    *prometheus.Desc
	temperatureAverage   *prometheus.Desc
	temperatureMin       *prometheus.Desc
	temperatureMax       *prometheus.Desc
	humidityAverage      *prometheus.Desc
	humidityMin          *prometheus.Desc
	humidityMax          *prometheus.Desc
	windowTimestamp      *prometheus.Desc
	windowDuration       *prometheus.Desc
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

// NewReportsDrivesCollector builds the collector and its Descs. It is pure:
// it starts no goroutine and performs no I/O, which is what makes it
// constructible in tests with no background refresh running. Call Start once,
// after construction, to begin refreshing.
//
// Temperature and humidity are six descriptors rather than one carrying a
// `stat="average|min|max"` label, on the same reasoning
// NewReportsLibraryCollector records: Prometheus's own exporter guidance is
// that a metric should make sense when summed or averaged across its labels,
// and summing an average with a minimum and a maximum is meaningless.
func NewReportsDrivesCollector(log *logger.Logger, client *Client, interval time.Duration) *ReportsDrivesCollector {
	return &ReportsDrivesCollector{
		client:   client,
		interval: interval,
		log:      log,
		mounts: prometheus.NewDesc(
			"tapelibrary_drive_report_mounts",
			"Number of cartridges mounted into this drive during the reporting window. A per-window figure, not a cumulative counter: the next window restarts from zero.",
			[]string{"location"}, nil,
		),
		cleans: prometheus.NewDesc(
			"tapelibrary_drive_report_cleans",
			"Number of times this drive was cleaned during the reporting window. A per-window figure, not a cumulative counter. Zero in every window of the reference capture: a drive requests cleaning rarely, so a window recording one is the event worth looking at.",
			[]string{"location"}, nil,
		),
		readByHosts: prometheus.NewDesc(
			"tapelibrary_drive_report_read_by_hosts_bytes",
			"Bytes read from cartridges by this drive during the reporting window. Converted from the API's megabytes, read decimally (1 MB = 1e6 bytes) to match how this exporter already converts the library report and the cartridge lifetime counters.",
			[]string{"location"}, nil,
		),
		writtenByHosts: prometheus.NewDesc(
			"tapelibrary_drive_report_written_by_hosts_bytes",
			"Bytes written to cartridges by this drive during the reporting window, measured before compression. Divide by tapelibrary_drive_report_written_to_cartridges_bytes for this drive's average compression ratio over the window. Converted from the API's decimal megabytes.",
			[]string{"location"}, nil,
		),
		writtenToCartridges: prometheus.NewDesc(
			"tapelibrary_drive_report_written_to_cartridges_bytes",
			"Bytes this drive actually wrote onto the media during the reporting window, after compression. Converted from the API's decimal megabytes.",
			[]string{"location"}, nil,
		),
		errorsCorrectedRead: prometheus.NewDesc(
			"tapelibrary_drive_report_errors_corrected_read",
			"Read errors this drive corrected during the reporting window. A corrected error cost throughput but lost no data; a drive whose corrected count runs far above its peers is the classic early signature of a failing head or a dirty tape path. Compare against tapelibrary_drive_report_read_by_hosts_bytes before reading a high count as a fault, since a busy drive corrects more.",
			[]string{"location"}, nil,
		),
		errorsCorrectedWrite: prometheus.NewDesc(
			"tapelibrary_drive_report_errors_corrected_write",
			"Write errors this drive corrected during the reporting window, typically by rewriting the affected block further along the tape. Compare against tapelibrary_drive_report_written_by_hosts_bytes before reading a high count as a fault.",
			[]string{"location"}, nil,
		),
		errorsUncorrected: prometheus.NewDesc(
			"tapelibrary_drive_report_errors_uncorrected",
			"Errors this drive could not correct during the reporting window, read and write together: R1.11.2 reports no direction breakdown for these, unlike the corrected pair. Any non-zero value is data the drive failed to move, and is what DriveReportUncorrectedErrors reads.",
			[]string{"location"}, nil,
		),
		temperatureAverage: prometheus.NewDesc(
			"tapelibrary_drive_report_temperature_average_celsius",
			"Average temperature in Celsius this drive measured over the reporting window. Measured inside the library at the drive, so it reads above the ambient figure R1.11.2's operating envelope is written against. Absent, never zero, when the drive reported no reading.",
			[]string{"location"}, nil,
		),
		temperatureMin: prometheus.NewDesc(
			"tapelibrary_drive_report_temperature_min_celsius",
			"Lowest temperature in Celsius this drive measured over the reporting window. Absent, never zero, when the drive reported no reading.",
			[]string{"location"}, nil,
		),
		temperatureMax: prometheus.NewDesc(
			"tapelibrary_drive_report_temperature_max_celsius",
			"Highest temperature in Celsius this drive measured over the reporting window. Absent, never zero, when the drive reported no reading.",
			[]string{"location"}, nil,
		),
		humidityAverage: prometheus.NewDesc(
			"tapelibrary_drive_report_humidity_average_ratio",
			"Average relative humidity this drive measured over the reporting window, as a ratio from 0 to 1 (the API reports a 0-100 percentage). Absent, never zero, when the drive reported no reading.",
			[]string{"location"}, nil,
		),
		humidityMin: prometheus.NewDesc(
			"tapelibrary_drive_report_humidity_min_ratio",
			"Lowest relative humidity this drive measured over the reporting window, as a ratio from 0 to 1. Absent, never zero, when the drive reported no reading.",
			[]string{"location"}, nil,
		),
		humidityMax: prometheus.NewDesc(
			"tapelibrary_drive_report_humidity_max_ratio",
			"Highest relative humidity this drive measured over the reporting window, as a ratio from 0 to 1. Absent, never zero, when the drive reported no reading.",
			[]string{"location"}, nil,
		),
		windowTimestamp: prometheus.NewDesc(
			"tapelibrary_drive_report_window_timestamp_seconds",
			"Unix time the library stamped on the reporting window these metrics describe, for this drive. Per drive rather than library-wide so that a single drive dropping out of the report is visible: the library publishes one window per completed hour, so alert if time() - this exceeds a few hours. Every other metric in this family would otherwise keep serving a stale window's values indefinitely, looking healthy.",
			[]string{"location"}, nil,
		),
		windowDuration: prometheus.NewDesc(
			"tapelibrary_drive_report_window_duration_seconds",
			"Number of seconds this drive's reporting window covers, as the library reports it. 3600 on every window in the reference capture. Exposed so that a window covering less than a full hour is visible rather than assumed away: its activity figures would be proportionally low through no fault of the drive.",
			[]string{"location"}, nil,
		),
		lastRefreshDesc: prometheus.NewDesc(
			"tapelibrary_drive_report_last_refresh_timestamp_seconds",
			"Unix time of the last successful reports/drives refresh. Alert if time() - this > 2 x the collector's configured interval. Named for this collector's metric subsystem rather than its registered name, so a subsystem sweep finds the freshness of the data it is reading. Distinct from tapelibrary_drive_report_window_timestamp_seconds, which ages even while this one stays current.",
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
func (c *ReportsDrivesCollector) Start(ctx context.Context) {
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
func (c *ReportsDrivesCollector) Done() <-chan struct{} {
	return c.done
}

// refresh performs the one I/O call (reportsDrivesGetMetrics, via the injected
// *Client) and, on success, atomically replaces the cache. On error it logs
// and returns, leaving the previous cache and lastRefresh untouched,
// fail-open: a transient failure serves the last-known-good data instead of
// dropping the series, and the freshness gauge is the signal that a refresh
// is stale, not a dropped scrape.
func (c *ReportsDrivesCollector) refresh(ctx context.Context) {
	drives, err := c.reportsDrivesGetMetrics(ctx)
	if err != nil {
		c.log.Error("Failed to refresh reports/drives metrics: keeping previous cache", "err", err)
		return
	}

	// Ten always-emitted series per drive: the eight per-window activity and
	// error figures, plus that drive's own window timestamp and duration. None
	// of them can be absent on a parsed window, which is what guarantees this
	// collector never emits a scrape carrying the freshness gauge alone after
	// a successful refresh.
	metrics := make([]prometheus.Metric, 0, len(drives)*16)
	for i := range drives {
		w := &drives[i].window
		loc := w.Location
		metrics = append(metrics,
			prometheus.MustNewConstMetric(c.mounts, prometheus.GaugeValue, w.Mounts, loc),
			prometheus.MustNewConstMetric(c.cleans, prometheus.GaugeValue, w.Cleans, loc),
			prometheus.MustNewConstMetric(c.readByHosts, prometheus.GaugeValue, w.DataReadByHosts*reportsDrivesMBToBytes, loc),
			prometheus.MustNewConstMetric(c.writtenByHosts, prometheus.GaugeValue, w.DataWrittenByHosts*reportsDrivesMBToBytes, loc),
			prometheus.MustNewConstMetric(c.writtenToCartridges, prometheus.GaugeValue, w.DataWrittenToCartridges*reportsDrivesMBToBytes, loc),
			prometheus.MustNewConstMetric(c.errorsCorrectedRead, prometheus.GaugeValue, w.ErrorsCorrectedRead, loc),
			prometheus.MustNewConstMetric(c.errorsCorrectedWrite, prometheus.GaugeValue, w.ErrorsCorrectedWrite, loc),
			prometheus.MustNewConstMetric(c.errorsUncorrected, prometheus.GaugeValue, w.ErrorsUncorrected, loc),
			prometheus.MustNewConstMetric(c.windowTimestamp, prometheus.GaugeValue, float64(drives[i].at.Unix()), loc),
			prometheus.MustNewConstMetric(c.windowDuration, prometheus.GaugeValue, w.Duration, loc),
		)

		// The six environmental readings, each emitted only if this drive
		// actually reported it. A null here means the drive could not take the
		// reading, not that the reading was zero, and the operating-envelope
		// alerting rules read these directly: a 0 °C / 0% RH standing in for
		// "unknown" would page on a drive that is merely quiet about its
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
			metrics = append(metrics, prometheus.MustNewConstMetric(m.desc, prometheus.GaugeValue, *m.value*m.scale, loc))
		}
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
func (c *ReportsDrivesCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.mounts
	ch <- c.cleans
	ch <- c.readByHosts
	ch <- c.writtenByHosts
	ch <- c.writtenToCartridges
	ch <- c.errorsCorrectedRead
	ch <- c.errorsCorrectedWrite
	ch <- c.errorsUncorrected
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
func (c *ReportsDrivesCollector) Collect(ch chan<- prometheus.Metric) {
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
