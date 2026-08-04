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

// driveStates is the full set of values GET /v1/drives documents for its
// "state" field (TS4500 R1.11.2, "URL endpoints and resources"). Every one of
// them is emitted per drive on every refresh as its own series, exactly one
// carrying 1 and the rest 0: the stateset encoding this exporter uses for
// every enumeration (see docs/exporter-journal.md, "Enum encoding").
//
// Order is the manual's own priority order, not alphabetical; Registry.Gather
// sorts the exposition output by label value regardless, so this slice's order
// is invisible to a scrape and kept as-is to stay diffable against the manual.
var driveStates = []string{
	"unknown",
	"inServiceMode",
	"restarting",
	"initializing",
	"unreachable",
	"resetRequired",
	"updating",
	"cleaning",
	"online",
}

// driveOperations is the set of values GET /v1/drives documents for its
// "operation" field: what the drive is doing right now, which is orthogonal to
// whether the drive is healthy. A drive can be online and idle, or online and
// mid-unload, and only this field tells the two apart.
//
// "none" is this exporter's rendering of the API's own null, which the manual
// describes as "no operation is in progress". A JSON null is a documented
// value here rather than a missing field, so unlike every other nullable field
// in this exporter it does become a series — see refresh's driveOperation
// helper for why that is not a contradiction.
var driveOperations = []string{
	"none",
	"empty",
	"loading",
	"ready",
	"unloading",
	"unloaded",
}

// driveAccessValues is the set of values the "accessible" field takes: whether
// the accessor can currently reach this drive at all. Three values here, not
// the two of AccessorsCollector's driveAccess/cartridgeAccess.
//
// That difference is deliberate and is the convention the accessors collector
// established (docs/exporter-journal.md, "Shared label vocabulary"): the
// `access` label's value set is per-resource, read from the endpoint being
// collected, never inherited from another one. Same label key, same meaning
// ("what can this thing still reach"), two documented sets.
var driveAccessValues = []string{
	"normal",
	"limited",
	"no",
}

// driveLastCleanedLayout is the timestamp format GET /v1/drives returns in its
// "lastCleaned" field: 2026-07-23T20:43:36+0000. Note the zone offset carries
// no colon, so this is NOT time.RFC3339 and parsing it as such fails.
//
// DrivesCollector is the first collector in this exporter to emit a timestamp
// the library itself reports, so this layout is the convention every later one
// follows rather than re-deriving.
const driveLastCleanedLayout = "2006-01-02T15:04:05-0700"

// driveStats is the parsed shape of one GET /v1/drives entry. The endpoint
// returns one element per tape drive, keyed by the library's own native
// location string (drive_F1C4R1), which is what the `location` label carries
// verbatim so a value in a dashboard can be pasted straight into the library's
// GUI.
//
// Only the fields this collector emits are declared. The response also carries
// barcode, interfaceMode, elementAddress and beacon; none is a measurement,
// and none answers a question an operator brings to a dashboard, so they are
// left on the wire rather than turned into label churn on the info series.
//
// Two fields are pointers, for the same reason in both cases: the API
// documents them as nullable, and a plain string would silently decode null to
// "".
//
//   - Operation is null when the drive has no operation in progress. That is a
//     documented value rather than missing data, so it maps to the "none"
//     member of driveOperations rather than to no series (see driveOperation).
//   - LastCleaned is null on a drive that has never been cleaned. A zero
//     timestamp there would place the last cleaning in 1970 and make every
//     "cleaned within N days" query silently wrong, so it produces no series.
type driveStats struct {
	Location       string  `json:"location"`
	SerialNumber   string  `json:"sn"`
	MediaType      string  `json:"mediaType"`
	State          string  `json:"state"`
	Operation      *string `json:"operation"`
	Accessible     string  `json:"accessible"`
	MTM            string  `json:"mtm"`
	Interface      string  `json:"interface"`
	LogicalLibrary string  `json:"logicalLibrary"`
	Use            string  `json:"use"`
	Firmware       string  `json:"firmware"`
	Encryption     string  `json:"encryption"`
	WWNN           string  `json:"wwnn"`
	Volser         *string `json:"volser"`
	LastCleaned    *string `json:"lastCleaned"`
}

// drivesData is this collector's only I/O: it fetches the raw response body
// from the configured library. Kept separate from parsing (parseDrives, below)
// so parsing stays pure and unit-testable without a live library.
func (c *DrivesCollector) drivesData(ctx context.Context) ([]byte, error) {
	return c.client.Fetch(ctx, "/drives")
}

// parseDrives decodes drivesData's response body into one driveStats per tape
// drive. Pure: no I/O, no logging, no side effects, so every input maps
// deterministically to an output. That is what makes it unit-testable with
// plain byte fixtures (see the test file's TestParseDrives).
//
// Three inputs are rejected rather than passed through, all for the same
// reason — refresh keeps the previous cache on error, which is the right
// outcome for a response this collector cannot interpret:
//
//   - An empty array. A library with no drive cannot serve any host, so an
//     empty list is a response that lost its content rather than a real
//     inventory. Accepting it would replace a good cache with no drives at
//     all, and the freshness gauge going stale is the visible signal that
//     something is wrong, which a silently emptied cache is not.
//   - An entry with no location. It would emit a series labelled location=""
//     that no operator can trace back to any hardware.
//   - Two entries sharing one location. Every metric below is keyed by
//     location, so a duplicate would send two metrics with the same descriptor
//     and the same label set, and Registry.Gather rejects the WHOLE scrape when
//     that happens, not just the offending series. Failing closed here keeps a
//     malformed response from taking out every other collector's metrics too
//     (see CONTRIBUTING.md, "Common Pitfalls").
func parseDrives(b []byte) ([]driveStats, error) {
	var entries []driveStats
	if err := json.Unmarshal(b, &entries); err != nil {
		return nil, fmt.Errorf("parse drives response: %w", err)
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("parse drives response: empty array, want at least one drive")
	}

	// Indexed rather than ranged by value: driveStats is wide enough that
	// copying one per iteration is what gocritic's rangeValCopy flags.
	seen := make(map[string]struct{}, len(entries))
	for i := range entries {
		e := &entries[i]
		if e.Location == "" {
			return nil, fmt.Errorf("parse drives response: entry with an empty location")
		}
		if _, dup := seen[e.Location]; dup {
			return nil, fmt.Errorf("parse drives response: duplicate location %q", e.Location)
		}
		seen[e.Location] = struct{}{}
	}
	return entries, nil
}

// drivesGetMetrics is the glue between the I/O step (drivesData) and the pure
// parsing step (parseDrives): the shape every collector in this exporter
// follows, regardless of flavor. refresh, below, calls this on its own
// background schedule; nothing else in this file calls the library directly.
func (c *DrivesCollector) drivesGetMetrics(ctx context.Context) ([]driveStats, error) {
	data, err := c.drivesData(ctx)
	if err != nil {
		return nil, err
	}
	return parseDrives(data)
}

// DrivesCollector reads GET /v1/drives: the operational state of every tape
// drive in the library, what each one is currently doing, whether the accessor
// can still reach it, when it was last cleaned, and its identity. It is the
// background-refresh variant, which on this target model is not a choice: a
// scrape serves N libraries through one /metrics and must never block on a
// machine that has gone away. A background goroutine (started by Start, below)
// refreshes a cached metric slice on a fixed interval, and Collect only ever
// reads that cache under mu.
//
// This is the largest per-object collector on the fleet: 40 drives x 20 series
// is ~800 of the ~2 425 series a library emits at default settings. The one
// field held back from that default is volser — see perVolser below.
type DrivesCollector struct {
	client   *Client
	interval time.Duration
	log      *logger.Logger

	// perVolser gates the loadedCartridge series, and defaults to false.
	// Only 40 volsers are ever loaded at once, so the ACTIVE series count is
	// trivial — but the drive that holds a given tape changes constantly, so
	// location x volser accumulates a fresh index entry for every pairing
	// that has ever existed (up to 9 749 cartridges x 40 drives here). The
	// cost lands in Prometheus's index and in query fan-out over time, not in
	// this exporter's own memory, which is exactly the kind of cost that is
	// invisible until it is expensive. Operators who want "which tape is in
	// drive X" opt in with --collector.drives.per-volser.
	perVolser bool

	state           *prometheus.Desc
	operation       *prometheus.Desc
	access          *prometheus.Desc
	lastCleaned     *prometheus.Desc
	info            *prometheus.Desc
	loadedCartridge *prometheus.Desc
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

// NewDrivesCollector builds the collector and its Descs. It is pure: it starts
// no goroutine and performs no I/O, which is what makes it constructible in
// tests with no background refresh running. Call Start once, after
// construction, to begin refreshing.
//
// Every metric here is a Gauge. The library reports no monotonic lifetime
// counter on this endpoint — the drive-level totals live under
// /v1/reports/drives, which gets its own collector — so nothing on it takes
// the CounterValue treatment the accessors' robotics counters do (see
// docs/exporter-journal.md, "Metric name shape").
//
// logical_library rides on the three statesets as well as on the info series.
// It costs nothing in cardinality (it is constant per drive) and it is what
// lets the "too few online drives in this logical library" rule be a plain
// count by (job, library, logical_library) rather than a group_left join
// against the info series. The accepted cost is that reassigning a drive
// between logical libraries breaks the continuity of its measurement series —
// which is a real configuration change, and arguably one worth seeing as a
// discontinuity.
func NewDrivesCollector(log *logger.Logger, client *Client, interval time.Duration, perVolser bool) *DrivesCollector {
	return &DrivesCollector{
		client:    client,
		interval:  interval,
		log:       log,
		perVolser: perVolser,
		state: prometheus.NewDesc(
			"tapelibrary_drive_state",
			"Operational state of the tape drive, as a stateset: 1 on the active state and 0 on every other known state.",
			[]string{"location", "logical_library", "state"}, nil,
		),
		operation: prometheus.NewDesc(
			"tapelibrary_drive_operation",
			"Operation the tape drive is currently performing, as a stateset: 1 on the active operation and 0 on every other known operation. Orthogonal to the drive's state, which says whether the drive is healthy rather than what it is doing. The API's null is reported here as none.",
			[]string{"location", "logical_library", "operation"}, nil,
		),
		access: prometheus.NewDesc(
			"tapelibrary_drive_access",
			"Whether the accessor can reach this drive, as a stateset: 1 on the active value and 0 on every other known value. A drive can be online and still be unreachable, in which case no cartridge can be mounted in it.",
			[]string{"location", "logical_library", "access"}, nil,
		),
		lastCleaned: prometheus.NewDesc(
			"tapelibrary_drive_last_cleaned_timestamp_seconds",
			"Unix time at which this drive was last cleaned. A drive that has never been cleaned reports no series at all, rather than a 0 that would place its last cleaning in 1970.",
			[]string{"location", "logical_library"}, nil,
		),
		info: prometheus.NewDesc(
			"tapelibrary_drive_info",
			"Drive identity, always 1. Identity strings live here rather than on a measurement series, so a firmware upgrade or a drive swap changes this series alone instead of breaking the continuity of every other. use=controlPath marks the drives the host talks to the library through.",
			[]string{"location", "logical_library", "serial", "firmware", "mtm", "media_type", "use", "encryption", "interface", "wwnn"}, nil,
		),
		loadedCartridge: prometheus.NewDesc(
			"tapelibrary_drive_loaded_cartridge_info",
			"The cartridge currently loaded in this drive, always 1. Emitted only when --collector.drives.per-volser is set, and only for drives that actually hold a cartridge: the volser churns, so this pairing accumulates index entries in Prometheus far beyond the 40 that are ever active at once.",
			[]string{"location", "logical_library", "volser"}, nil,
		),
		lastRefreshDesc: prometheus.NewDesc(
			"tapelibrary_drives_last_refresh_timestamp_seconds",
			"Unix time of the last successful drives refresh. Alert if time() - this > 2 x the collector's configured interval.",
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
func (c *DrivesCollector) Start(ctx context.Context) {
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
func (c *DrivesCollector) Done() <-chan struct{} {
	return c.done
}

// stateset emits one series per known value of an enumerated field, exactly
// one carrying 1, plus the observed value itself when the manual does not
// document it. The manual's tables are demonstrably a floor rather than a
// ceiling (it names states in prose that appear in no table, which is how
// AccessorsCollector ended up with failedToInitialize), so an unlisted value
// must surface as its own series rather than silently leave every series at 0
// and make the drive look stateless.
//
// The `known` flag is what keeps that extra series from duplicating a label
// set already emitted in the loop, which would fail Gather for this
// collector's whole scrape.
//
// Shared by the three enumerated fields on this endpoint (state, operation,
// accessible) rather than written out three times: they differ only in their
// descriptor and their value list, and the emit-observed-anyway branch is
// subtle enough that one tested copy beats three.
func (c *DrivesCollector) stateset(
	metrics []prometheus.Metric,
	desc *prometheus.Desc,
	location, logicalLibrary, observed, field string,
	known []string,
) []prometheus.Metric {
	documented := false
	for _, v := range known {
		value := 0.0
		if v == observed {
			value = 1.0
			documented = true
		}
		metrics = append(metrics, prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, value, location, logicalLibrary, v))
	}
	if !documented && observed != "" {
		c.log.Warn("Drive reported a value absent from the documented set: emitting it anyway",
			"location", location, "field", field, "value", observed)
		metrics = append(metrics, prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, 1, location, logicalLibrary, observed))
	}
	return metrics
}

// driveOperation renders the nullable operation field as the stateset member
// it means. This is the one place in this exporter where a JSON null becomes a
// series rather than suppressing one, and the distinction is that here null is
// a documented VALUE ("no operation is in progress") rather than the absence
// of a reading. An idle drive and a drive whose operation the hardware cannot
// report are the same fact on this endpoint; an accessor with no thermometer
// and an accessor at 0 degrees are not.
//
// An empty string is folded into the same branch: the manual documents no such
// value, and a stateset with every member at 0 says less than one that names
// the drive idle.
func driveOperation(d *driveStats) string {
	if d.Operation == nil || *d.Operation == "" {
		return "none"
	}
	return *d.Operation
}

// refresh performs the one I/O call (drivesGetMetrics, via the injected
// *Client) and, on success, atomically replaces the cache. On error it logs
// and returns, leaving the previous cache and lastRefresh untouched,
// fail-open: a transient failure serves the last-known-good data instead of
// dropping the series, and the freshness gauge is the signal that a refresh
// is stale, not a dropped scrape.
func (c *DrivesCollector) refresh(ctx context.Context) {
	drives, err := c.drivesGetMetrics(ctx)
	if err != nil {
		c.log.Error("Failed to refresh drives metrics: keeping previous cache", "err", err)
		return
	}

	// Per drive: three statesets, one info series, and up to one last-cleaned
	// timestamp and one loaded-cartridge series.
	perDrive := len(driveStates) + len(driveOperations) + len(driveAccessValues) + 3
	metrics := make([]prometheus.Metric, 0, len(drives)*perDrive)

	// Indexed rather than ranged by value, same reason as parseDrives above.
	for i := range drives {
		d := &drives[i]

		metrics = c.stateset(metrics, c.state, d.Location, d.LogicalLibrary, d.State, "state", driveStates)
		metrics = c.stateset(metrics, c.operation, d.Location, d.LogicalLibrary, driveOperation(d), "operation", driveOperations)
		metrics = c.stateset(metrics, c.access, d.Location, d.LogicalLibrary, d.Accessible, "accessible", driveAccessValues)

		metrics = append(metrics, prometheus.MustNewConstMetric(
			c.info, prometheus.GaugeValue, 1,
			d.Location, d.LogicalLibrary, d.SerialNumber, d.Firmware, d.MTM,
			d.MediaType, d.Use, d.Encryption, d.Interface, d.WWNN,
		))

		// A drive that has never been cleaned reports null, which is not the
		// same fact as a cleaning at the epoch, so it produces no series at
		// all. An unparseable timestamp is logged rather than guessed at: a
		// wrong instant here would make every "cleaned within N days" query
		// quietly answer about the wrong drives.
		if ts, ok := c.parseLastCleaned(d); ok {
			metrics = append(metrics, prometheus.MustNewConstMetric(
				c.lastCleaned, prometheus.GaugeValue, float64(ts.Unix()), d.Location, d.LogicalLibrary))
		}

		// Gated, and skipped for an empty drive: a volser label of "" would
		// claim a cartridge is loaded whose barcode nobody could read.
		if c.perVolser && d.Volser != nil && *d.Volser != "" {
			metrics = append(metrics, prometheus.MustNewConstMetric(
				c.loadedCartridge, prometheus.GaugeValue, 1, d.Location, d.LogicalLibrary, *d.Volser))
		}
	}

	c.mu.Lock()
	c.cached = metrics
	c.lastRefresh = time.Now()
	c.mu.Unlock()
}

// parseLastCleaned turns the API's lastCleaned string into an instant, or
// reports that no series should be emitted. Null and empty are the documented
// "never cleaned" case and pass silently; anything else that fails to parse is
// a response this collector does not understand and is logged once per
// refresh, per drive.
func (c *DrivesCollector) parseLastCleaned(d *driveStats) (time.Time, bool) {
	if d.LastCleaned == nil || *d.LastCleaned == "" {
		return time.Time{}, false
	}
	ts, err := time.Parse(driveLastCleanedLayout, *d.LastCleaned)
	if err != nil {
		c.log.Warn("Drive reported an unparseable lastCleaned timestamp: emitting no series for it",
			"location", d.Location, "value", *d.LastCleaned, "err", err)
		return time.Time{}, false
	}
	return ts, true
}

// Describe sends every one of this collector's descriptors, including the
// freshness gauge. Constant regardless of scrape or refresh outcome, which is
// what makes prometheus.DescribeByCollect unnecessary here.
//
// loadedCartridge is described even when --collector.drives.per-volser is off
// and it will emit nothing: a descriptor is what this collector CAN emit, not
// what it did last time, and docs/metrics.md documents it on that basis.
func (c *DrivesCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.state
	ch <- c.operation
	ch <- c.access
	ch <- c.lastCleaned
	ch <- c.info
	ch <- c.loadedCartridge
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
func (c *DrivesCollector) Collect(ch chan<- prometheus.Metric) {
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
