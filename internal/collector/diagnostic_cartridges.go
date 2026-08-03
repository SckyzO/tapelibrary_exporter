package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/sckyzo/tapelibrary_exporter/internal/logger"
)

// diagnosticCartridgeUsageLayout parses the `mostRecentUsage` field. Same
// layout as every other timestamp this library reports (see
// driveLastCleanedLayout, the first collector to meet it): the zone offset
// carries no colon, so it is not time.RFC3339 and parsing it as such fails.
const diagnosticCartridgeUsageLayout = driveLastCleanedLayout

// diagnosticCartridgeStates is R1.11.2's documented `state` table for this
// endpoint, five values. Emitted in full as a stateset on every scrape, plus
// any value actually observed that is not on this list: the manual is
// demonstrably incomplete elsewhere in this exporter, and an untabulated
// state must surface as a new series rather than silently leave every series
// at 0.
//
// A full stateset is affordable here for a reason that does not generalise
// to the other cartridge endpoints: the population is five. The journal's
// cardinality rule reserves the full form for objects numbering in the tens
// and requires the active-state-only form where they number in the
// thousands, which is why DataCartridgesCollector carries state as a label
// on its _info instead.
var diagnosticCartridgeStates = []string{
	"unknown",
	"atEndOfLife",
	"exportQueued",
	"importing",
	"normal",
}

// diagnosticCartridgeAccessValues is the `accessible` ternary shared with
// drives and every other cartridge type. Read from THIS endpoint's own
// documentation rather than inherited: the journal records that the same
// label key carries a different documented value set on accessors, so a
// stateset's known list is never copied between collectors.
var diagnosticCartridgeAccessValues = []string{"normal", "limited", "no"}

// diagnosticCartridgeUsable is the state a cartridge must be in for the
// robot to be able to select it for a service action. See usable's own
// descriptor for why the accessible check rides alongside it.
const diagnosticCartridgeUsable = "normal"

// diagnosticCartridge is one entry of GET /v1/diagnosticCartridges.
//
// The cartridge-memory fields are pointers because the library reports them
// as null whenever it has not read the cartridge's memory, which on this
// endpoint is the MAJORITY case rather than an edge case: three of the five
// cartridges in the reference capture return null for `type`, `vendor`,
// `sn`, `worm`, `format` and `lifetimeRemaining` all at once, while still
// reporting volser, state, accessible, location and mediaType. That is the
// same 45%-of-cartridges pattern DataCartridgesCollector already documents,
// and it is handled the same way here: a nullable LABEL field takes the
// literal token "unknown" (see orUnknown, shared with that collector), and a
// nullable MEASUREMENT emits no series at all.
//
// The "unknown" token is safe on each field it is applied to here, and that
// was checked rather than assumed: cartridge types are two-character barcode
// codes (JK, JL) and `worm` is "true"/"false", so neither can legitimately
// BE the string "unknown". It is deliberately NOT applied to `state`, whose
// documented value set already contains `unknown` — using the token there
// would merge a real library state with a missing reading.
//
// `internalAddress` is deliberately not parsed. R1.11.2 nominates it as the
// tie-breaker for duplicate volsers and it is genuinely unique, but the
// manual also documents it as changing whenever a cartridge is assigned,
// unassigned or moved: it would churn a fresh series out of every move while
// identifying nothing an operator can act on. `nativeCapacity`,
// `typeDescription`, `density`, `densityCode`, `manufactureDate` and
// `vendor` are not parsed either, matching DataCartridgesCollector, which
// reads the same cartridge-memory block and emits none of them.
type diagnosticCartridge struct {
	Volser     string `json:"volser"`
	State      string `json:"state"`
	Accessible string `json:"accessible"`
	Location   string `json:"location"`
	MediaType  string `json:"mediaType"`

	// Type is the two-character generation code (JK, JL...), null when the
	// library has not read the cartridge's memory.
	Type *string `json:"type"`

	// Worm is "true" or "false" as a JSON string, not a bool, and null on the
	// same terms as Type.
	Worm *string `json:"worm"`

	// MostRecentUsage is the wire-format timestamp of the last mount. Empty
	// or unparseable emits NO series rather than a 0: zero here is not a
	// missing reading, it is an assertion that the cartridge was last used at
	// the Unix epoch, which silently corrupts every "within the last N days"
	// query rather than merely being absent from it.
	MostRecentUsage string `json:"mostRecentUsage"`

	// LifetimeRemaining is the estimated percentage of media life left,
	// 0..100, null when the cartridge memory was not read.
	LifetimeRemaining *int `json:"lifetimeRemaining"`
}

// cartridgeType returns the generation code for labelling, or the "unknown"
// token when the library did not read the cartridge's memory.
func (d *diagnosticCartridge) cartridgeType() string { return orUnknown(d.Type) }

// diagnosticCartridgeKey is what makes a cartridge unique on this endpoint.
//
// **volser alone is NOT a key on any cartridge endpoint of this library.**
// Established by CleaningCartridgesCollector against its own capture, which
// holds 70 cartridges under 63 distinct volsers, and stated plainly by
// R1.11.2: "if there are duplicate VOLSERs, this value [internalAddress] is
// used to identify the cartridge." The 2026-07-28 diagnostic capture happens
// to carry five distinct volsers, but that is a property of five cartridges
// rather than a guarantee, and the cost of being wrong is not local: two
// metrics sharing a descriptor and a label set fail Registry.Gather for the
// WHOLE scrape, taking out all nineteen collectors rather than this one.
type diagnosticCartridgeKey struct {
	volser   string
	location string
}

// diagnosticCartridgesData is this collector's only I/O: it fetches the raw
// response body from the configured library. Kept separate from parsing
// (parseDiagnosticCartridges, below) so parsing stays pure and unit-testable
// without a live library.
func (c *DiagnosticCartridgesCollector) diagnosticCartridgesData(ctx context.Context) ([]byte, error) {
	return c.client.Fetch(ctx, "/diagnosticCartridges")
}

// parseDiagnosticCartridges decodes the response body. Pure: no I/O, no
// logging, no side effects.
//
// Two inputs are rejected outright rather than passed through — refresh
// keeps the previous cache on error, which is the right outcome for a
// response this collector cannot interpret:
//
//   - An entry with no volser or no location. Either would produce a series
//     that traces back to no cartridge, and neither can be defaulted.
//   - A duplicate of the (volser, location) PAIR, for the reason
//     diagnosticCartridgeKey documents.
//
// An EMPTY array is deliberately NOT an error, and this is the one place
// this collector parts company with its cartridge siblings. A library with
// no data cartridge cannot serve a host and a library with no cleaning
// cartridge cannot clean a drive, so both of those treat an empty list as a
// response that lost its content. A library with no diagnostic cartridge is
// merely a library nobody has loaded one into: it runs normally until a
// service action needs one. Rejecting that would replace a true reading of
// zero with a stale cache, and zero is exactly what
// DiagnosticCartridgesExhausted has to be able to see.
//
// The result is sorted by volser then location so the cached slice is
// deterministic for a given response. Registry.Gather sorts the exposition
// independently, so this is for the cache and its tests, not for the wire.
func parseDiagnosticCartridges(b []byte) ([]diagnosticCartridge, error) {
	var entries []diagnosticCartridge
	if err := json.Unmarshal(b, &entries); err != nil {
		return nil, fmt.Errorf("parse diagnostic cartridges response: %w", err)
	}

	seen := make(map[diagnosticCartridgeKey]struct{}, len(entries))
	for i := range entries {
		e := &entries[i]
		if e.Volser == "" {
			return nil, fmt.Errorf("parse diagnostic cartridges response: entry %d has no volser", i)
		}
		if e.Location == "" {
			return nil, fmt.Errorf("parse diagnostic cartridges response: cartridge %q has no location", e.Volser)
		}
		key := diagnosticCartridgeKey{e.Volser, e.Location}
		if _, dup := seen[key]; dup {
			return nil, fmt.Errorf("parse diagnostic cartridges response: duplicate cartridge %q at location %q", e.Volser, e.Location)
		}
		seen[key] = struct{}{}
	}

	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Volser != entries[j].Volser {
			return entries[i].Volser < entries[j].Volser
		}
		return entries[i].Location < entries[j].Location
	})
	return entries, nil
}

// diagnosticCartridgesGetMetrics is the glue between the I/O step and the
// pure parsing step: the shape every collector in this exporter follows,
// regardless of flavor. refresh, below, calls this on its own background
// schedule; nothing else in this file calls the library directly.
func (c *DiagnosticCartridgesCollector) diagnosticCartridgesGetMetrics(ctx context.Context) ([]diagnosticCartridge, error) {
	data, err := c.diagnosticCartridgesData(ctx)
	if err != nil {
		return nil, err
	}
	return parseDiagnosticCartridges(data)
}

// DiagnosticCartridgesCollector reads GET /v1/diagnosticCartridges: the
// cartridges the library uses for its own service actions rather than for
// host data.
//
// **What it exists to answer is "can the library still diagnose itself".**
// A diagnostic cartridge is not read or written by any host, so none of the
// throughput or error questions the data-cartridge collectors ask apply. The
// one that does is availability, and it is invisible until the day a service
// action needs a cartridge and finds none usable — at which point the fix is
// ordering media, not something an engineer on site can do.
// tapelibrary_diagnostic_cartridges_usable is that signal, and
// DiagnosticCartridgesExhausted is the rule that reads it.
//
// **The per-cartridge detail is ON by default**, which makes this the second
// collector in this exporter to emit `volser` without being asked, after
// CleaningCartridgesCollector. It rests on the same justification and not on
// a different one: the population is bounded by service policy rather than
// by library capacity, and at five cartridges against that collector's
// seventy the argument is stronger here than where it was first made. The
// flag exists anyway, and the library-wide aggregates every alert reads are
// emitted regardless of it, so turning it off costs the ability to name
// WHICH cartridge to pull and nothing else.
//
// It is the background-refresh variant, which on this target model is not a
// choice: a scrape serves N libraries through one /metrics and must never
// block on a machine that has gone away.
type DiagnosticCartridgesCollector struct {
	client    *Client
	interval  time.Duration
	log       *logger.Logger
	perVolser bool

	stateCount      *prometheus.Desc
	accessCount     *prometheus.Desc
	usable          *prometheus.Desc
	lifetimeUnknown *prometheus.Desc
	info            *prometheus.Desc
	lastUsage       *prometheus.Desc
	lifetimeRatio   *prometheus.Desc
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

// NewDiagnosticCartridgesCollector builds the collector and its Descs. It is
// pure: it starts no goroutine and performs no I/O, which is what makes it
// constructible in tests with no background refresh running. Call Start
// once, after construction, to begin refreshing.
func NewDiagnosticCartridgesCollector(log *logger.Logger, client *Client, interval time.Duration, perVolser bool) *DiagnosticCartridgesCollector {
	return &DiagnosticCartridgesCollector{
		client:    client,
		interval:  interval,
		log:       log,
		perVolser: perVolser,
		stateCount: prometheus.NewDesc(
			"tapelibrary_diagnostic_cartridges",
			"Number of diagnostic cartridges in each state, as a stateset over R1.11.2's five documented values plus any value actually observed. Always emitted, independently of --collector.diagnostic_cartridges.per-volser. Summing across state gives the library's whole diagnostic population.",
			[]string{"state"}, nil,
		),
		accessCount: prometheus.NewDesc(
			"tapelibrary_diagnostic_cartridges_access",
			"Number of diagnostic cartridges the accessor can reach, by access level. A cartridge behind a blocking position reads limited or no while its state stays normal, so this is a different question from the state count above and both are needed to explain a low usable figure.",
			[]string{"access"}, nil,
		),
		usable: prometheus.NewDesc(
			"tapelibrary_diagnostic_cartridges_usable",
			"Number of diagnostic cartridges the library could actually select for a service action right now: state normal AND reachable by the accessor. Deliberately narrower than the normal state count, because a cartridge the robot cannot reach is one it cannot use. Reaching 0 means the next service action requiring media will be blocked, and is what DiagnosticCartridgesExhausted reads. Always emitted, independently of the per-volser flag.",
			nil, nil,
		),
		lifetimeUnknown: prometheus.NewDesc(
			"tapelibrary_diagnostic_cartridges_lifetime_unknown",
			"Number of diagnostic cartridges reporting no remaining-life reading at all, because the library has not read their cartridge memory. Three of the five in the reference capture, so a high value here is the ordinary state of this endpoint rather than a fault: it is published so that the remaining-life series below are read as covering a subset, never as covering the whole population.",
			nil, nil,
		),
		info: prometheus.NewDesc(
			"tapelibrary_diagnostic_cartridge_info",
			"Always 1. Carries this diagnostic cartridge's current state and identity as labels, joinable to the per-cartridge measurements on volser and location. Labels the library did not read carry the literal token unknown rather than an empty string. Emitted only when --collector.diagnostic_cartridges.per-volser is set.",
			[]string{"volser", "location", "state", "media_type", "cartridge_type", "access", "worm"}, nil,
		),
		lastUsage: prometheus.NewDesc(
			"tapelibrary_diagnostic_cartridge_last_usage_timestamp_seconds",
			"Unix time this diagnostic cartridge was last mounted. Absent, never zero, when the library reports no usage timestamp or one that cannot be parsed: a 0 here would place the mount at the Unix epoch and quietly corrupt every query asking what has been used recently. Emitted only when --collector.diagnostic_cartridges.per-volser is set.",
			[]string{"volser", "location"}, nil,
		),
		lifetimeRatio: prometheus.NewDesc(
			"tapelibrary_diagnostic_cartridge_lifetime_remaining_ratio",
			"Estimated media life left on this diagnostic cartridge, as a ratio from 0 to 1 (the API reports a 0-100 percentage). Absent, never zero, for a cartridge whose memory the library has not read, which is the majority case on this endpoint: a 0 would read as a cartridge at end of life. Count the absent ones with tapelibrary_diagnostic_cartridges_lifetime_unknown. Emitted only when --collector.diagnostic_cartridges.per-volser is set.",
			[]string{"volser", "location"}, nil,
		),
		lastRefreshDesc: prometheus.NewDesc(
			"tapelibrary_diagnostic_cartridges_last_refresh_timestamp_seconds",
			"Unix time of the last successful diagnostic cartridges refresh. Alert if time() - this > 2 x the collector's configured interval; CollectorNeverRefreshed and CollectorRefreshStale already do.",
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
func (c *DiagnosticCartridgesCollector) Start(ctx context.Context) {
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
func (c *DiagnosticCartridgesCollector) Done() <-chan struct{} {
	return c.done
}

// refresh performs the one I/O call and, on success, atomically replaces the
// cache. On error it logs and returns, leaving the previous cache and
// lastRefresh untouched, fail-open: a transient failure serves the
// last-known-good data instead of dropping the series, and the freshness
// gauge is the signal that a refresh is stale, not a dropped scrape.
func (c *DiagnosticCartridgesCollector) refresh(ctx context.Context) {
	cartridges, err := c.diagnosticCartridgesGetMetrics(ctx)
	if err != nil {
		c.log.Error("Failed to refresh diagnostic cartridges metrics: keeping previous cache", "err", err)
		return
	}

	states := make(map[string]int, len(diagnosticCartridgeStates))
	access := make(map[string]int, len(diagnosticCartridgeAccessValues))
	usable, lifetimeUnknown := 0, 0

	metrics := make([]prometheus.Metric, 0, len(cartridges)*3+len(diagnosticCartridgeStates)+len(diagnosticCartridgeAccessValues)+2)

	for i := range cartridges {
		d := &cartridges[i]
		states[d.State]++
		access[d.Accessible]++
		// Narrower than the normal state count on purpose: a cartridge the
		// accessor cannot reach cannot be selected, whatever its state says.
		if d.State == diagnosticCartridgeUsable && d.Accessible != "no" {
			usable++
		}
		if d.LifetimeRemaining == nil {
			lifetimeUnknown++
		}

		if !c.perVolser {
			continue
		}
		metrics = append(metrics, prometheus.MustNewConstMetric(
			c.info, prometheus.GaugeValue, 1,
			d.Volser, d.Location, d.State, d.MediaType, d.cartridgeType(), d.Accessible, orUnknown(d.Worm),
		))
		// Absent rather than zero: see lastUsage's own descriptor.
		if at, err := time.Parse(diagnosticCartridgeUsageLayout, d.MostRecentUsage); err == nil {
			metrics = append(metrics, prometheus.MustNewConstMetric(
				c.lastUsage, prometheus.GaugeValue, float64(at.Unix()), d.Volser, d.Location))
		} else if d.MostRecentUsage != "" {
			c.log.Warn("Unparseable diagnostic cartridge usage timestamp: emitting no series for it",
				"volser", d.Volser, "location", d.Location, "value", d.MostRecentUsage)
		}
		if d.LifetimeRemaining != nil {
			metrics = append(metrics, prometheus.MustNewConstMetric(
				c.lifetimeRatio, prometheus.GaugeValue, float64(*d.LifetimeRemaining)/100, d.Volser, d.Location))
		}
	}

	// The documented sets in full, plus anything observed that is not on
	// them. Emitted unconditionally so a library with no diagnostic cartridge
	// at all still publishes a complete, all-zero stateset rather than
	// vanishing from every query written against it.
	metrics = c.valueCounts(metrics, c.stateCount, "state", diagnosticCartridgeStates, states)
	metrics = c.valueCounts(metrics, c.accessCount, "accessible", diagnosticCartridgeAccessValues, access)
	metrics = append(metrics,
		prometheus.MustNewConstMetric(c.usable, prometheus.GaugeValue, float64(usable)),
		prometheus.MustNewConstMetric(c.lifetimeUnknown, prometheus.GaugeValue, float64(lifetimeUnknown)),
	)

	c.mu.Lock()
	c.cached = metrics
	c.lastRefresh = time.Now()
	c.mu.Unlock()
}

// valueCounts emits one series per documented value, then one per observed
// value the documented list does not contain.
//
// Shared by the state and access families, which differ only in their
// descriptor and their value set. Written once rather than twice so the
// emit-observed-anyway branch — the thing that catches the manual being
// wrong, and R1.11.2 has been wrong about a state table more than once in
// this exporter — cannot be present on one family and forgotten on the
// other. Mutates observed via delete, which is safe because every caller
// passes a map local to one refresh.
func (c *DiagnosticCartridgesCollector) valueCounts(
	metrics []prometheus.Metric,
	desc *prometheus.Desc,
	field string,
	known []string,
	observed map[string]int,
) []prometheus.Metric {
	for _, v := range known {
		metrics = append(metrics, prometheus.MustNewConstMetric(
			desc, prometheus.GaugeValue, float64(observed[v]), v))
		delete(observed, v)
	}
	for _, v := range slices.Sorted(maps.Keys(observed)) {
		c.log.Warn("Diagnostic cartridge reported a value absent from the documented set: emitting it anyway",
			"field", field, "value", v, "count", observed[v])
		metrics = append(metrics, prometheus.MustNewConstMetric(
			desc, prometheus.GaugeValue, float64(observed[v]), v))
	}
	return metrics
}

// Describe sends every one of this collector's descriptors, including the
// three per-cartridge ones a --no-per-volser run never emits. Constant
// regardless of scrape or refresh outcome, which is what makes
// prometheus.DescribeByCollect unnecessary here.
func (c *DiagnosticCartridgesCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.stateCount
	ch <- c.accessCount
	ch <- c.usable
	ch <- c.lifetimeUnknown
	ch <- c.info
	ch <- c.lastUsage
	ch <- c.lifetimeRatio
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
// staleness, and CollectorNeverRefreshed is the rule that reads it.
func (c *DiagnosticCartridgesCollector) Collect(ch chan<- prometheus.Metric) {
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
