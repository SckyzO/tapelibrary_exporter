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

// nodeCardStates is the full set of values GET /v1/nodeCards documents for its
// "state" field (TS4500 R1.11.2, "Node cards"). Every one of them is emitted
// per card on every refresh as its own series, exactly one carrying 1 and the
// rest 0: the stateset encoding this exporter uses for every enumeration (see
// docs/exporter-journal.md, "Enum encoding").
//
// Order is the manual's own priority order, not alphabetical; Registry.Gather
// sorts the exposition output by label value regardless, so this slice's order
// is invisible to a scrape and kept as-is to stay diffable against the manual.
//
// "online" is the healthy value, NOT "normal". The legacy check scripts under
// samples/legacy/ expect "normal" and would classify a perfectly healthy card
// as unmapped; R1.11.2 documents "online" and the manual wins, exactly as it
// did for the frame door pseudo-states and accessors.reorienting (see
// docs/exporter-journal.md, "Open questions / assumptions"). The
// emit-observed-anyway branch in stateset, below, is what settles it for real
// if any library ever reports "normal".
var nodeCardStates = []string{
	"unknown",
	"restarting",
	"inServiceMode",
	"unreachable",
	"noEthernet",
	"noCAN",
	"online",
}

// nodeCardLastRestartLayout is the timestamp format GET /v1/nodeCards returns
// in its "lastRestart" field: 2026-02-06T13:52:16+0000. It is the same grammar
// every timestamp this library reports uses — note the zone offset carries no
// colon, so this is NOT time.RFC3339 and parsing it as such fails — so it
// reuses the constant DrivesCollector established rather than re-deriving a
// second copy free to drift from it.
const nodeCardLastRestartLayout = driveLastCleanedLayout

// nodeCardStats is the parsed shape of one GET /v1/nodeCards entry. The
// endpoint returns one element per controller card in the library: the LCC
// (Library Control Card) in each frame, and the MDA and ACC cards riding on
// each accessor.
//
// **This is the first endpoint in this exporter where `location` is not a
// unique key.** The 2026-07-28 capture carries an MDA *and* an ACC card both
// at accessor_Aa, and both again at accessor_Ab: a card's location names the
// hardware it is installed in, and an accessor holds two different cards. Every
// metric below is therefore keyed by location AND type, and parseNodeCards
// rejects a duplicate of that pair rather than that of location alone —
// two metrics sharing a descriptor and a label set fail Registry.Gather for
// the WHOLE scrape (see CONTRIBUTING.md, "Common Pitfalls").
//
// The type field is carried on the `card_type` label rather than `type`:
// docs/exporter-journal.md's shared label vocabulary rules `type` out as the
// canonical too-generic label name, which is the same reason the cartridge
// generation is `cartridge_type` and the frame class is `frame_type`.
//
// Only the fields this collector emits are declared. The response also carries
// ID, barcode, ec, cfBarcode, cfPartNum and cfVendor; the compact-flash trio
// describes a subcomponent no operator alerts on, ID is an opaque handle that
// cannot be pasted into the GUI, and none of them is a measurement, so all are
// left on the wire rather than turned into label churn on the info series.
//
// Three fields are pointers, because the API documents them as nullable and a
// plain string would silently decode null to "":
//
//   - ReportingLCC and PrimaryLCC are null on every card that is not an LCC.
//     That is "this question does not apply to this card", not "no", so they
//     produce no series at all rather than a 0 asserting the MDA card lost an
//     election it never stood in.
//   - LastRestart is null on a card that has not restarted since the library
//     last lost its record of it. A zero timestamp there would place the
//     restart in 1970 and make every "restarted within N days" query silently
//     wrong, so it too produces no series.
type nodeCardStats struct {
	Type         string  `json:"type"`
	Location     string  `json:"location"`
	State        string  `json:"state"`
	PartNumber   string  `json:"partNum"`
	SerialNumber string  `json:"sn"`
	Firmware     string  `json:"firmware"`
	ReportingLCC *string `json:"reportingLCC"`
	PrimaryLCC   *string `json:"primaryLCC"`
	LastRestart  *string `json:"lastRestart"`
}

// nodeCardsData is this collector's only I/O: it fetches the raw response body
// from the configured library. Kept separate from parsing (parseNodeCards,
// below) so parsing stays pure and unit-testable without a live library.
func (c *NodeCardsCollector) nodeCardsData(ctx context.Context) ([]byte, error) {
	return c.client.Fetch(ctx, "/nodeCards")
}

// parseNodeCards decodes nodeCardsData's response body into one nodeCardStats
// per controller card. Pure: no I/O, no logging, no side effects, so every
// input maps deterministically to an output. That is what makes it
// unit-testable with plain byte fixtures (see the test file's
// TestParseNodeCards).
//
// Four inputs are rejected rather than passed through, all for the same
// reason — refresh keeps the previous cache on error, which is the right
// outcome for a response this collector cannot interpret:
//
//   - An empty array. Every TS4500 runs on at least one LCC, and an
//     unreachable LCC is precisely what the library reports as an `unknown`
//     card rather than by omitting it, so an empty list is a response that
//     lost its content rather than a library running on no controller cards.
//     Accepting it would replace a good cache with nothing at the exact
//     moment the hardware most needs watching.
//   - An entry with no location. It would emit a series labelled location=""
//     that no operator can trace back to any hardware.
//   - An entry with no type. Unlike every earlier collector, type is half of
//     this endpoint's key (see nodeCardStats): an empty one would silently
//     collide with any other typeless card at the same location.
//   - Two entries sharing one location AND type. Keyed on the pair, not on
//     location alone, which on this endpoint is legitimately shared by an MDA
//     and an ACC card. Failing closed here keeps a malformed response from
//     taking out every other collector's metrics too.
func parseNodeCards(b []byte) ([]nodeCardStats, error) {
	var entries []nodeCardStats
	if err := json.Unmarshal(b, &entries); err != nil {
		return nil, fmt.Errorf("parse node cards response: %w", err)
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("parse node cards response: empty array, want at least one node card")
	}

	// Indexed rather than ranged by value: nodeCardStats is wide enough that
	// copying one per iteration is what gocritic's rangeValCopy flags.
	type nodeCardKey struct{ location, cardType string }
	seen := make(map[nodeCardKey]struct{}, len(entries))
	for i := range entries {
		e := &entries[i]
		if e.Location == "" {
			return nil, fmt.Errorf("parse node cards response: entry with an empty location")
		}
		if e.Type == "" {
			return nil, fmt.Errorf("parse node cards response: entry at %q with an empty type", e.Location)
		}
		key := nodeCardKey{e.Location, e.Type}
		if _, dup := seen[key]; dup {
			return nil, fmt.Errorf("parse node cards response: duplicate %s card at location %q", e.Type, e.Location)
		}
		seen[key] = struct{}{}
	}
	return entries, nil
}

// nodeCardsGetMetrics is the glue between the I/O step (nodeCardsData) and the
// pure parsing step (parseNodeCards): the shape every collector in this
// exporter follows, regardless of flavor. refresh, below, calls this on its own
// background schedule; nothing else in this file calls the library directly.
func (c *NodeCardsCollector) nodeCardsGetMetrics(ctx context.Context) ([]nodeCardStats, error) {
	data, err := c.nodeCardsData(ctx)
	if err != nil {
		return nil, err
	}
	return parseNodeCards(data)
}

// NodeCardsCollector reads GET /v1/nodeCards: the health of every controller
// card in the library — the LCC in each frame, and the MDA and ACC cards on
// each accessor — plus which LCC currently holds the primary and reporting
// roles, and when each card last restarted.
//
// These cards are the library's own nervous system, which is what makes them
// worth their own collector rather than a footnote on the frame: R1.11.2
// attributes a power supply reporting `unknown` to an unreachable LCC rather
// than to the supply, so a degraded node card shows up as noise on several
// other collectors before anything names it. This one names it.
//
// It is the background-refresh variant, which on this target model is not a
// choice: a scrape serves N libraries through one /metrics and must never
// block on a machine that has gone away. A background goroutine (started by
// Start, below) refreshes a cached metric slice on a fixed interval, and
// Collect only ever reads that cache under mu.
type NodeCardsCollector struct {
	client   *Client
	interval time.Duration
	log      *logger.Logger

	state           *prometheus.Desc
	info            *prometheus.Desc
	lastRestart     *prometheus.Desc
	primaryLCC      *prometheus.Desc
	reportingLCC    *prometheus.Desc
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

// NewNodeCardsCollector builds the collector and its Descs. It is pure: it
// starts no goroutine and performs no I/O, which is what makes it
// constructible in tests with no background refresh running. Call Start once,
// after construction, to begin refreshing.
//
// Every metric here is a Gauge. The endpoint reports no monotonic counter —
// lastRestart is an instant, not a restart tally — so nothing on it takes the
// CounterValue treatment the accessors' robotics counters do (see
// docs/exporter-journal.md, "Metric name shape").
func NewNodeCardsCollector(log *logger.Logger, client *Client, interval time.Duration) *NodeCardsCollector {
	return &NodeCardsCollector{
		client:   client,
		interval: interval,
		log:      log,
		state: prometheus.NewDesc(
			"tapelibrary_node_card_state",
			"Operational state of the node card, as a stateset: 1 on the active state and 0 on every other known state.",
			[]string{"location", "card_type", "state"}, nil,
		),
		info: prometheus.NewDesc(
			"tapelibrary_node_card_info",
			"Node card identity, always 1. Identity strings live here rather than on a measurement series, so a firmware upgrade or a card swap changes this series alone instead of breaking the continuity of every other.",
			[]string{"location", "card_type", "serial", "part_number", "firmware"}, nil,
		),
		lastRestart: prometheus.NewDesc(
			"tapelibrary_node_card_last_restart_timestamp_seconds",
			"Unix time at which this node card last restarted. A card the library reports no restart for produces no series at all, rather than a 0 that would place its last restart in 1970.",
			[]string{"location", "card_type"}, nil,
		),
		primaryLCC: prometheus.NewDesc(
			"tapelibrary_node_card_primary_lcc",
			"Whether this card is the library's primary LCC (1) or not (0). Emitted only for cards that report the role at all, which means the LCC cards: an accessor's MDA or ACC card produces no series rather than a 0 asserting it lost an election it never stood in.",
			[]string{"location", "card_type"}, nil,
		),
		reportingLCC: prometheus.NewDesc(
			"tapelibrary_node_card_reporting_lcc",
			"Whether this card is the LCC currently answering for the library (1) or not (0). Orthogonal to the primary role: a failover moves this one first. Emitted only for cards that report the role at all.",
			[]string{"location", "card_type"}, nil,
		),
		lastRefreshDesc: prometheus.NewDesc(
			"tapelibrary_node_cards_last_refresh_timestamp_seconds",
			"Unix time of the last successful node cards refresh. Alert if time() - this > 2 x the collector's configured interval.",
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
func (c *NodeCardsCollector) Start(ctx context.Context) {
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
func (c *NodeCardsCollector) Done() <-chan struct{} {
	return c.done
}

// stateset emits one series per known state, exactly one carrying 1, plus the
// observed state itself when the manual does not document it. The manual's
// tables are demonstrably a floor rather than a ceiling (it names states in
// prose that appear in no table, which is how AccessorsCollector ended up with
// failedToInitialize), so an unlisted state must surface as its own series
// rather than silently leave every series at 0 and make the card look
// stateless.
//
// The stakes are specific here: the legacy scripts expect a "normal" state
// this firmware does not document, so if any library does report it, this
// branch is what turns a silent all-zero stateset into a visible series and a
// logged warning naming the value.
//
// The `documented` flag is what keeps that extra series from duplicating a
// label set already emitted in the loop, which would fail Gather for this
// collector's whole scrape.
func (c *NodeCardsCollector) stateset(metrics []prometheus.Metric, n *nodeCardStats) []prometheus.Metric {
	documented := false
	for _, s := range nodeCardStates {
		value := 0.0
		if s == n.State {
			value = 1.0
			documented = true
		}
		metrics = append(metrics, prometheus.MustNewConstMetric(c.state, prometheus.GaugeValue, value, n.Location, n.Type, s))
	}
	if !documented && n.State != "" {
		c.log.Warn("Node card reported a state absent from the documented set: emitting it anyway",
			"location", n.Location, "card_type", n.Type, "state", n.State)
		metrics = append(metrics, prometheus.MustNewConstMetric(c.state, prometheus.GaugeValue, 1, n.Location, n.Type, n.State))
	}
	return metrics
}

// lccRole renders one of the API's nullable yes/no role fields as a gauge
// value, or reports that no series should be emitted.
//
// Null and empty are the documented "this card is not an LCC" case and pass
// silently: absence of the role is not the same fact as holding it and losing,
// and a 0 would put every MDA and ACC card into the denominator of any
// "exactly one primary LCC" query. Anything else that is neither yes nor no is
// a response this collector does not understand, and is logged rather than
// guessed at in either direction.
func (c *NodeCardsCollector) lccRole(n *nodeCardStats, field string, v *string) (float64, bool) {
	if v == nil || *v == "" {
		return 0, false
	}
	switch *v {
	case "yes":
		return 1, true
	case "no":
		return 0, true
	}
	c.log.Warn("Node card reported an unparseable LCC role: emitting no series for it",
		"location", n.Location, "card_type", n.Type, "field", field, "value", *v)
	return 0, false
}

// parseLastRestart turns the API's lastRestart string into an instant, or
// reports that no series should be emitted. Null and empty are the documented
// "no recorded restart" case and pass silently; anything else that fails to
// parse is a response this collector does not understand and is logged once
// per refresh, per card.
func (c *NodeCardsCollector) parseLastRestart(n *nodeCardStats) (time.Time, bool) {
	if n.LastRestart == nil || *n.LastRestart == "" {
		return time.Time{}, false
	}
	ts, err := time.Parse(nodeCardLastRestartLayout, *n.LastRestart)
	if err != nil {
		c.log.Warn("Node card reported an unparseable lastRestart timestamp: emitting no series for it",
			"location", n.Location, "card_type", n.Type, "value", *n.LastRestart, "err", err)
		return time.Time{}, false
	}
	return ts, true
}

// refresh performs the one I/O call (nodeCardsGetMetrics, via the injected
// *Client) and, on success, atomically replaces the cache. On error it logs
// and returns, leaving the previous cache and lastRefresh untouched,
// fail-open: a transient failure serves the last-known-good data instead of
// dropping the series, and the freshness gauge is the signal that a refresh
// is stale, not a dropped scrape.
func (c *NodeCardsCollector) refresh(ctx context.Context) {
	cards, err := c.nodeCardsGetMetrics(ctx)
	if err != nil {
		c.log.Error("Failed to refresh node cards metrics: keeping previous cache", "err", err)
		return
	}

	// Per card: one stateset, one info series, and up to one last-restart
	// timestamp and two LCC role series.
	perCard := len(nodeCardStates) + 4
	metrics := make([]prometheus.Metric, 0, len(cards)*perCard)

	// Indexed rather than ranged by value, same reason as parseNodeCards above.
	for i := range cards {
		n := &cards[i]

		metrics = c.stateset(metrics, n)

		metrics = append(metrics, prometheus.MustNewConstMetric(
			c.info, prometheus.GaugeValue, 1,
			n.Location, n.Type, n.SerialNumber, n.PartNumber, n.Firmware,
		))

		if ts, ok := c.parseLastRestart(n); ok {
			metrics = append(metrics, prometheus.MustNewConstMetric(
				c.lastRestart, prometheus.GaugeValue, float64(ts.Unix()), n.Location, n.Type))
		}

		if v, ok := c.lccRole(n, "primaryLCC", n.PrimaryLCC); ok {
			metrics = append(metrics, prometheus.MustNewConstMetric(
				c.primaryLCC, prometheus.GaugeValue, v, n.Location, n.Type))
		}
		if v, ok := c.lccRole(n, "reportingLCC", n.ReportingLCC); ok {
			metrics = append(metrics, prometheus.MustNewConstMetric(
				c.reportingLCC, prometheus.GaugeValue, v, n.Location, n.Type))
		}
	}

	c.mu.Lock()
	c.cached = metrics
	c.lastRefresh = time.Now()
	c.mu.Unlock()
}

// Describe sends every one of this collector's descriptors, including the
// freshness gauge. Constant regardless of scrape or refresh outcome, which is
// what makes prometheus.DescribeByCollect unnecessary here.
//
// The two LCC role descriptors are described even on a library whose cards
// never report the fields: a descriptor is what this collector CAN emit, not
// what it did last time, and docs/metrics.md documents them on that basis.
func (c *NodeCardsCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.state
	ch <- c.info
	ch <- c.lastRestart
	ch <- c.primaryLCC
	ch <- c.reportingLCC
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
func (c *NodeCardsCollector) Collect(ch chan<- prometheus.Metric) {
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
