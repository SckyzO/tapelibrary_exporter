package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/sckyzo/tapelibrary_exporter/internal/logger"
)

// cleaningCartridgeStates is the full set of values GET /v1/cleaningCartridges
// documents for its "state" field (TS4500 R1.11.2, "Get cleaning cartridges").
// Order is the manual's own priority order, not alphabetical; Registry.Gather
// sorts the exposition output by label value regardless, so this slice's order
// is invisible to a scrape and kept as-is to stay diffable against the manual.
//
// Unlike every other cartridge type, this table carries NO "unknown" value —
// dataCartridges and diagnosticCartridges both document one and this endpoint
// does not. That asymmetry is the manual's, not a transcription slip, which is
// why the emit-observed-anyway branch in stateCounts below matters more here
// than elsewhere: if a library ever does report `unknown` for a cleaning
// cartridge, that branch is what turns a silent miscount into a visible series
// and a logged warning naming the value.
var cleaningCartridgeStates = []string{
	"exportQueued",
	"importing",
	"normal",
}

// cleaningCartridgeUsageLayout is the timestamp format GET
// /v1/cleaningCartridges returns in its "mostRecentUsage" field:
// 2026-07-24T09:40:09+0000. It is the same grammar every timestamp this
// library reports uses — note the zone offset carries no colon, so this is NOT
// time.RFC3339 and parsing it as such fails — so it reuses the constant
// DrivesCollector established rather than re-deriving a third copy free to
// drift from it.
const cleaningCartridgeUsageLayout = driveLastCleanedLayout

// cleaningCartridgeStats is the parsed shape of one GET /v1/cleaningCartridges
// entry. The endpoint returns one element per cleaning cartridge the library
// holds — 70 on the 2026-07-28 capture, a population bounded by the site's
// cleaning policy rather than by library capacity, which is what makes this the
// one collector in this exporter emitting `volser` by default (see
// docs/exporter-journal.md, "Cardinality budget").
//
// **`volser` is NOT a unique key on this endpoint, and must never be used as
// one.** The 2026-07-28 capture carries 70 cartridges under only 63 distinct
// volsers: TST021JA, TST022JA, TST032JA, TST033JA, TST036JA, TST048JA and
// TST051JA each appear twice, at different locations, with different
// cleansRemaining. This is not a capture artifact — R1.11.2's own worked
// example for this endpoint shows two CLNI01L1 entries side by side, and the
// manual states plainly that "if there are duplicate VOLSERs, this value
// [internalAddress] is used to identify the cartridge". Duplicate barcodes are
// an ordinary fact of a tape library's life.
//
// Every metric below is therefore keyed by volser AND location, and
// parseCleaningCartridges rejects a duplicate of that pair rather than that of
// volser alone — two metrics sharing a descriptor and a label set fail
// Registry.Gather for the WHOLE scrape, every other collector's metrics
// included (see CONTRIBUTING.md, "Common Pitfalls"). The legacy scripts under
// samples/legacy/ did emit the duplicates, which is visible in their own
// captured .prom output; a textfile scrape tolerated what client_golang will
// not. This is the same rule NodeCardsCollector established for its own
// location+card_type pair: a key is whatever makes an object unique on ITS OWN
// endpoint, verified against the capture, never inherited from the collector
// written before it.
//
// internalAddress is deliberately NOT parsed, even though it is the manual's
// nominated tie-breaker and genuinely unique. R1.11.2 documents it as changing
// "if the cartridge is assigned or unassigned from a logical library or if the
// cartridge is moved by the host or library", so as a label it would churn a
// fresh series out of every move while identifying nothing an operator can act
// on — the same argument that keeps drives.volser behind an opt-in flag.
// `location` is unique across all 70 entries of the capture, is stable for as
// long as the cartridge sits still, and can be pasted straight into the GUI.
type cleaningCartridgeStats struct {
	// Volser is the cartridge's barcode. Half of this endpoint's key, and NOT
	// unique on its own: see the type comment above.
	Volser string `json:"volser"`

	// State is the cartridge's health status, one of cleaningCartridgeStates.
	// Carried as a label on the cleansRemaining measurement rather than as a
	// per-cartridge stateset: at 70 cartridges a full stateset would cost 210
	// series to answer a question the library-wide count already answers, and
	// the budget in docs/exporter-journal.md allots one series per cartridge.
	State string `json:"state"`

	// Accessible is the ternary reach field (normal, limited, no) shared with
	// drives and every other cartridge type, and carried on the `access`
	// label. Its value set is per-resource, not global (the rule
	// AccessorsCollector established): on cartridges it is this documented
	// ternary.
	Accessible string `json:"accessible"`

	// CleansRemaining is the number of clean operations this cartridge has
	// left. The single most operationally useful field on the endpoint: it is
	// what the ported legacy alert sums per library, and what identifies the
	// exhausted cartridge once that sum starts falling. A 0 is a real reading
	// (the capture holds two such cartridges, both parked in an I/O station
	// awaiting export), not a missing one, and is emitted as such.
	CleansRemaining int `json:"cleansRemaining"`

	// Location is the cartridge's current position — a slot, an I/O slot, a
	// drive or a gripper. The other half of this endpoint's key, and the only
	// field on it that is unique across the whole capture.
	Location string `json:"location"`

	// MediaType is "3592" or "LTO" per R1.11.2. Identity rather than a
	// grouping key, but it rides on the measurement series here rather than on
	// an _info series of its own: this collector emits no _info at all, since
	// the budget's one-series-per-cartridge allowance is spent on
	// cleansRemaining itself.
	MediaType string `json:"mediaType"`

	// MostRecentUsage is the last time this cartridge was mounted into a
	// drive. A pointer because R1.11.2 documents it as null "if this is
	// unknown or the cartridge has not been mounted", and a plain string would
	// silently decode that null to "". A zero timestamp would place the last
	// clean in 1970 and make every "unused since" query quietly wrong, so a
	// null produces no series at all rather than a 0.
	MostRecentUsage *string `json:"mostRecentUsage"`
}

// cleaningCartridgesData is this collector's only I/O: it fetches the raw
// response body from the configured library. Kept separate from parsing
// (parseCleaningCartridges, below) so parsing stays pure and unit-testable
// without a live library.
func (c *CleaningCartridgesCollector) cleaningCartridgesData(ctx context.Context) ([]byte, error) {
	return c.client.Fetch(ctx, "/cleaningCartridges")
}

// parseCleaningCartridges decodes cleaningCartridgesData's response body into
// one cleaningCartridgeStats per cleaning cartridge. Pure: no I/O, no logging,
// no side effects, so every input maps deterministically to an output. That is
// what makes it unit-testable with plain byte fixtures (see the test file's
// TestParseCleaningCartridges).
//
// Three inputs are rejected rather than passed through, all for the same
// reason — refresh keeps the previous cache on error, which is the right
// outcome for a response this collector cannot interpret:
//
//   - An entry with no volser, or none with no location. Either would emit a
//     series labelled with an empty half of the key, which no operator can
//     trace back to a physical cartridge, and which would silently collide
//     with any other entry missing the same half.
//   - Two entries sharing one volser AND location. Keyed on the pair, not on
//     volser alone, which on this endpoint is legitimately shared by two
//     physically distinct cartridges (see cleaningCartridgeStats). Failing
//     closed here keeps a malformed response from taking out every other
//     collector's metrics too.
//
// **An empty array is accepted, and this is the one collector in the exporter
// where that is true.** Every other one treats [] as a response that lost its
// content, because a TS4500 always has at least one frame, one accessor, one
// LCC and one partition. It does NOT always have a cleaning cartridge: they are
// consumable, they are exported once exhausted, and a library running out of
// them is precisely the condition CleaningCartridgesExhausted exists to page
// on. Rejecting [] would keep serving the previous cache — a comfortable list
// of cartridges that no longer exist — at the exact moment the alert needed to
// fire. So [] refreshes normally, and every aggregate below simply reports
// zero, which the always-emitted aggregate family makes expressible.
func parseCleaningCartridges(b []byte) ([]cleaningCartridgeStats, error) {
	var entries []cleaningCartridgeStats
	if err := json.Unmarshal(b, &entries); err != nil {
		return nil, fmt.Errorf("parse cleaning cartridges response: %w", err)
	}

	type cleaningCartridgeKey struct{ volser, location string }
	seen := make(map[cleaningCartridgeKey]struct{}, len(entries))
	// Indexed rather than ranged by value: cleaningCartridgeStats carries five
	// strings, an int and a pointer, so copying one per iteration is what
	// gocritic's rangeValCopy flags. refresh below takes the address for the
	// same reason.
	for i := range entries {
		e := &entries[i]
		if e.Volser == "" {
			return nil, fmt.Errorf("parse cleaning cartridges response: entry at location %q with an empty volser", e.Location)
		}
		if e.Location == "" {
			return nil, fmt.Errorf("parse cleaning cartridges response: entry with volser %q and an empty location", e.Volser)
		}
		key := cleaningCartridgeKey{e.Volser, e.Location}
		if _, dup := seen[key]; dup {
			return nil, fmt.Errorf("parse cleaning cartridges response: duplicate cartridge %q at location %q", e.Volser, e.Location)
		}
		seen[key] = struct{}{}
	}
	return entries, nil
}

// cleaningCartridgesGetMetrics is the glue between the I/O step
// (cleaningCartridgesData) and the pure parsing step
// (parseCleaningCartridges): the shape every collector in this exporter
// follows, regardless of flavor. refresh, below, calls this on its own
// background schedule; nothing else in this file calls the library directly.
func (c *CleaningCartridgesCollector) cleaningCartridgesGetMetrics(ctx context.Context) ([]cleaningCartridgeStats, error) {
	data, err := c.cleaningCartridgesData(ctx)
	if err != nil {
		return nil, err
	}
	return parseCleaningCartridges(data)
}

// CleaningCartridgesCollector reads GET /v1/cleaningCartridges: how much
// cleaning capacity the library has left, and which cartridge is running out of
// it.
//
// A drive that cannot be cleaned degrades and eventually fails, and the library
// gives no other warning: /v1/drives reports a lastCleaned timestamp with no
// threshold attached to it, which is exactly why DrivesCollector ships no rule
// on that field (see docs/exporter-journal.md). The cleaning supply is the
// grounded signal instead — the legacy scripts under samples/legacy/ alerted on
// its summed cleansRemaining at 100 and 30 for years, and those two thresholds
// are carried across verbatim, the only measured thresholds in this exporter
// that did not have to be re-derived.
//
// It is the background-refresh variant, which on this target model is not a
// choice: a scrape serves N libraries through one /metrics and must never block
// on a machine that has gone away. A background goroutine (started by Start,
// below) refreshes a cached metric slice on a fixed interval, and Collect only
// ever reads that cache under mu.
type CleaningCartridgesCollector struct {
	client   *Client
	interval time.Duration
	log      *logger.Logger

	// perVolser gates the two per-cartridge series, and defaults to TRUE —
	// the inverse of DrivesCollector's flag of the same name, and the only
	// per-item detail in this exporter that ships on.
	//
	// The asymmetry is justified by population size, not by the resource's
	// nature. A cleaning cartridge's volser does not churn the way a drive's
	// loaded volser does: the population is bounded by the site's cleaning
	// policy (70 here) rather than by library capacity (9 749 data
	// cartridges), and a cartridge sits in one slot until it is used or
	// exported. Per-cartridge detail is also the whole point of the
	// collector — a summed cleansRemaining tells an operator the library is
	// running low, and only the per-cartridge series says which cartridge to
	// pull.
	//
	// A site running cleaning cartridges in the thousands should flip it off
	// with --collector.cleaning_cartridges.per-volser=false, which is why
	// every aggregate below is computed from the full parsed list and emitted
	// unconditionally: turning this off must cost detail, never the alert.
	perVolser bool

	cleansRemaining      *prometheus.Desc
	lastUsage            *prometheus.Desc
	stateCount           *prometheus.Desc
	totalCleansRemaining *prometheus.Desc
	usable               *prometheus.Desc
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

// NewCleaningCartridgesCollector builds the collector and its Descs. It is
// pure: it starts no goroutine and performs no I/O, which is what makes it
// constructible in tests with no background refresh running. Call Start once,
// after construction, to begin refreshing.
//
// Every metric here is a Gauge. cleansRemaining counts DOWN as the cartridge is
// used and resets when the cartridge is replaced, so it is not the monotonic
// counter its name might suggest and takes no _total suffix (see
// docs/exporter-journal.md, "Metric name shape").
//
// The plural/singular split in the metric names is load-bearing rather than
// cosmetic, and follows the convention already set by
// tapelibrary_logical_library_* against
// tapelibrary_logical_libraries_last_refresh_timestamp_seconds: the SINGULAR
// subsystem (tapelibrary_cleaning_cartridge_*) is per-cartridge and gated by
// perVolser, the PLURAL one (tapelibrary_cleaning_cartridges*) is
// library-wide and always emitted.
func NewCleaningCartridgesCollector(log *logger.Logger, client *Client, interval time.Duration, perVolser bool) *CleaningCartridgesCollector {
	return &CleaningCartridgesCollector{
		client:    client,
		interval:  interval,
		log:       log,
		perVolser: perVolser,
		cleansRemaining: prometheus.NewDesc(
			"tapelibrary_cleaning_cartridge_cleans_remaining",
			"Number of clean operations left on this cleaning cartridge. Emitted only when --collector.cleaning_cartridges.per-volser is set, which it is by default. Keyed by volser AND location: a barcode is not unique in a tape library, and this endpoint legitimately reports two cartridges under one volser.",
			[]string{"volser", "location", "state", "access", "media_type"}, nil,
		),
		lastUsage: prometheus.NewDesc(
			"tapelibrary_cleaning_cartridge_last_usage_timestamp_seconds",
			"Unix time at which this cleaning cartridge was last mounted into a drive. A cartridge the library reports no usage for produces no series at all, rather than a 0 that would place its last clean in 1970. Emitted only when --collector.cleaning_cartridges.per-volser is set.",
			[]string{"volser", "location"}, nil,
		),
		stateCount: prometheus.NewDesc(
			"tapelibrary_cleaning_cartridges",
			"Number of cleaning cartridges the library holds in each documented state. Always emitted, one series per known state, so a library holding none at all reports 0 rather than dropping the series.",
			[]string{"state"}, nil,
		),
		totalCleansRemaining: prometheus.NewDesc(
			"tapelibrary_cleaning_cartridges_cleans_remaining",
			"Total clean operations left across every cleaning cartridge in the library. Always emitted, independently of --collector.cleaning_cartridges.per-volser, so turning that flag off costs per-cartridge detail but never the alert this value drives.",
			nil, nil,
		),
		usable: prometheus.NewDesc(
			"tapelibrary_cleaning_cartridges_usable",
			"Number of cleaning cartridges that are both in the normal state and have at least one clean left. Reaching 0 means the library can no longer clean a drive, whatever the raw cartridge count says.",
			nil, nil,
		),
		lastRefreshDesc: prometheus.NewDesc(
			"tapelibrary_cleaning_cartridges_last_refresh_timestamp_seconds",
			"Unix time of the last successful cleaning cartridges refresh. Alert if time() - this > 2 x the collector's configured interval.",
			nil, nil,
		),
		done: make(chan struct{}),
	}
}

// Start launches the background refresh goroutine. Call once, after
// construction. The first refresh runs immediately (so the cache starts filling
// as soon as the process starts) without Start itself waiting for it: a slow
// first fetch never blocks process startup. The goroutine exits when ctx is
// cancelled; Done() can then be used to wait for it to finish.
func (c *CleaningCartridgesCollector) Start(ctx context.Context) {
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

// Done returns a channel that is closed when the background goroutine started
// by Start has fully exited. main.go's shutdown seam waits on this (bounded, so
// a stuck refresh can't hang process exit forever) after the HTTP server itself
// has stopped.
func (c *CleaningCartridgesCollector) Done() <-chan struct{} {
	return c.done
}

// stateCounts emits one series per documented state carrying the number of
// cartridges in it, plus one per observed state the manual does not document.
//
// This is the aggregate counterpart of the per-object stateset every other
// collector in this exporter emits, and it is what the budget buys instead of
// one: at 70 cartridges a full per-cartridge stateset costs 210 series to say
// what these three say, and the active state is on the cleansRemaining series
// anyway for anyone who needs it per cartridge.
//
// Every documented state is emitted whether or not any cartridge is in it, so
// a rule matching on state="normal" still has a series to read when the count
// falls to zero — an absent series satisfies no matcher, so a library that had
// just lost its last usable cartridge would otherwise go quiet rather than
// alert. That also makes this family the collector's always-emitted floor: it
// alone guarantees Collect never sends zero metrics on a healthy-but-empty
// scrape, which StatusTracker would otherwise count as a failed collector.
//
// The manual's tables are demonstrably a floor rather than a ceiling (it names
// states in prose that appear in no table, which is how AccessorsCollector
// ended up with failedToInitialize), and this endpoint's table is the one that
// omits `unknown` while every sibling cartridge endpoint documents it. So an
// unlisted state surfaces as its own series and a logged warning rather than
// silently vanishing from the counts.
func (c *CleaningCartridgesCollector) stateCounts(metrics []prometheus.Metric, carts []cleaningCartridgeStats) []prometheus.Metric {
	counts := make(map[string]int, len(cleaningCartridgeStates))
	for i := range carts {
		counts[carts[i].State]++
	}

	for _, s := range cleaningCartridgeStates {
		metrics = append(metrics, prometheus.MustNewConstMetric(
			c.stateCount, prometheus.GaugeValue, float64(counts[s]), s))
		// Deleting as we go leaves exactly the undocumented states behind,
		// which is also what keeps the loop below from duplicating a label
		// set already emitted here and failing Gather for the whole scrape.
		delete(counts, s)
	}

	// Sorted rather than ranged over the map directly: map order is random,
	// and a warning that names its states in a different order every refresh
	// is needlessly hard to read in a log.
	for _, s := range slices.Sorted(maps.Keys(counts)) {
		if s == "" {
			// An entry with no state at all. It is counted nowhere rather
			// than emitted under state="", which would be a series no
			// operator can act on.
			c.log.Warn("Cleaning cartridges reported with an empty state: counted in no state series", "count", counts[s])
			continue
		}
		c.log.Warn("Cleaning cartridge reported a state absent from the documented set: emitting it anyway",
			"state", s, "count", counts[s])
		metrics = append(metrics, prometheus.MustNewConstMetric(
			c.stateCount, prometheus.GaugeValue, float64(counts[s]), s))
	}
	return metrics
}

// parseMostRecentUsage turns the API's mostRecentUsage string into an instant,
// or reports that no series should be emitted. Null and empty are the
// documented "never mounted, or the library has lost the record" case and pass
// silently; anything else that fails to parse is a response this collector does
// not understand and is logged once per refresh, per cartridge.
func (c *CleaningCartridgesCollector) parseMostRecentUsage(cc *cleaningCartridgeStats) (time.Time, bool) {
	if cc.MostRecentUsage == nil || *cc.MostRecentUsage == "" {
		return time.Time{}, false
	}
	ts, err := time.Parse(cleaningCartridgeUsageLayout, *cc.MostRecentUsage)
	if err != nil {
		c.log.Warn("Cleaning cartridge reported an unparseable mostRecentUsage timestamp: emitting no series for it",
			"volser", cc.Volser, "location", cc.Location, "value", *cc.MostRecentUsage, "err", err)
		return time.Time{}, false
	}
	return ts, true
}

// refresh performs the one I/O call (cleaningCartridgesGetMetrics, via the
// injected *Client) and, on success, atomically replaces the cache. On error it
// logs and returns, leaving the previous cache and lastRefresh untouched,
// fail-open: a transient failure serves the last-known-good data instead of
// dropping the series, and the freshness gauge is the signal that a refresh is
// stale, not a dropped scrape.
//
// The three library-wide aggregates are computed from every parsed cartridge
// and emitted whatever perVolser says. Only the two per-cartridge families
// depend on it, which is what makes the flag a pure cardinality lever: flipping
// it off cannot silence CleaningCartridgesLow, CleaningCartridgesCritical or
// CleaningCartridgesExhausted.
func (c *CleaningCartridgesCollector) refresh(ctx context.Context) {
	carts, err := c.cleaningCartridgesGetMetrics(ctx)
	if err != nil {
		c.log.Error("Failed to refresh cleaning cartridges metrics: keeping previous cache", "err", err)
		return
	}

	// Per cartridge, when perVolser is on: the cleansRemaining gauge and up to
	// one last-usage timestamp. Plus the library-wide aggregates.
	const perCartridge = 2
	aggregates := len(cleaningCartridgeStates) + 2
	metrics := make([]prometheus.Metric, 0, len(carts)*perCartridge+aggregates)

	totalCleans := 0
	usableCount := 0

	// Indexed rather than ranged by value, same reason as
	// parseCleaningCartridges above.
	for i := range carts {
		cc := &carts[i]

		totalCleans += cc.CleansRemaining
		// "Usable" is deliberately narrower than "present": a cartridge with
		// no cleans left is still reported by the library, still occupies a
		// slot, and still counts in the state family, but it cannot clean a
		// drive. So is one queued for export or mid-import — the robot will
		// not select either. This is the count CleaningCartridgesExhausted
		// reads, and the reason it is not simply the state family's
		// state="normal" series.
		if cc.State == "normal" && cc.CleansRemaining > 0 {
			usableCount++
		}

		if !c.perVolser {
			continue
		}

		metrics = append(metrics, prometheus.MustNewConstMetric(
			c.cleansRemaining, prometheus.GaugeValue, float64(cc.CleansRemaining),
			cc.Volser, cc.Location, cc.State, cc.Accessible, cc.MediaType,
		))

		if ts, ok := c.parseMostRecentUsage(cc); ok {
			metrics = append(metrics, prometheus.MustNewConstMetric(
				c.lastUsage, prometheus.GaugeValue, float64(ts.Unix()), cc.Volser, cc.Location))
		}
	}

	metrics = c.stateCounts(metrics, carts)
	metrics = append(metrics,
		prometheus.MustNewConstMetric(c.totalCleansRemaining, prometheus.GaugeValue, float64(totalCleans)),
		prometheus.MustNewConstMetric(c.usable, prometheus.GaugeValue, float64(usableCount)),
	)

	c.mu.Lock()
	c.cached = metrics
	c.lastRefresh = time.Now()
	c.mu.Unlock()
}

// Describe sends every one of this collector's descriptors, including the
// freshness gauge. Constant regardless of scrape or refresh outcome, which is
// what makes prometheus.DescribeByCollect unnecessary here.
//
// The two per-cartridge descriptors are described even when
// --collector.cleaning_cartridges.per-volser is off: a descriptor is what this
// collector CAN emit, not what it did last time, and docs/metrics.md documents
// them on that basis.
func (c *CleaningCartridgesCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.cleansRemaining
	ch <- c.lastUsage
	ch <- c.stateCount
	ch <- c.totalCleansRemaining
	ch <- c.usable
	ch <- c.lastRefreshDesc
}

// Collect replays the cached metrics from the last successful refresh:
// O(cached size), never touches the library, never blocks on it. It then ALWAYS
// sends the freshness gauge, even before any refresh has ever completed (value
// 0 in that case, since a zero time.Time's Unix() is a large negative, not a
// meaningful "ancient" epoch marker). A collector must never emit zero metrics
// on what it considers a healthy outcome: StatusTracker counts emitted metrics
// per scrape, and an empty cache before the first refresh completes is a normal
// startup window, not a failure. The freshness gauge alone guarantees at least
// one metric every scrape, so StatusTracker reports this collector as alive;
// the gauge's own value (0 until the first refresh lands) is the separate,
// correct signal for staleness.
func (c *CleaningCartridgesCollector) Collect(ch chan<- prometheus.Metric) {
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
