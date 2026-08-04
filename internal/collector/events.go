package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/sckyzo/tapelibrary_exporter/internal/logger"
)

// eventSeverities is the full set of values GET /v1/events documents for its
// "severity" field (TS4500 R1.11.2, "Get events"). Every one is emitted on
// every refresh, carrying the number of events of that severity the lookback
// window returned — 0 included. That is the stateset encoding this exporter
// uses for every enumeration (see docs/exporter-journal.md, "Enum encoding"),
// applied to a count rather than to a single active member: a severity nobody
// raised is a 0, not an absent series, so `> 0` alert rules mean what they say
// from the first scrape rather than only after the first event.
//
// inactiveError and inactiveWarning mean RESOLVED, not "a lesser error".
// Alerting must never match them as active conditions; the two rules shipped
// in monitoring/prometheus/alerts.yml match the bare `error` and `warning`
// members exactly for that reason, never a `severity=~".*rror"` pattern.
//
// Order is the manual's own. Registry.Gather sorts the exposition output by
// label value regardless, so this slice's order is invisible to a scrape and
// is kept as-is to stay diffable against the manual.
var eventSeverities = []string{
	"error",
	"warning",
	"inactiveError",
	"inactiveWarning",
	"information",
}

// eventSeverityUnknown is the label value carried by an event whose severity
// the library reported as null or empty. R1.11.2 does not document severity as
// nullable, so this is a malformed event rather than a documented state — but
// dropping it would leave the per-severity counts failing to sum to the number
// of events the endpoint actually returned, with nothing on the wire to
// explain the gap. It takes the literal `unknown` token instead, following the
// nullable-label rule data_cartridges established (see
// docs/exporter-journal.md, "Null is not always the absence of a reading").
//
// The collision check that rule requires passes here: eventSeverities above
// documents no `unknown` member, so the token cannot merge a real severity
// with a missing reading. It is emitted only when actually observed, through
// the same branch that surfaces an undocumented severity, so it costs no
// series on a healthy library.
const eventSeverityUnknown = "unknown"

// eventTimeLayout parses the `time` field of a GET /v1/events entry. The wire
// format is 2026-07-28T09:15:51+0000, whose zone offset carries no colon, so
// it is NOT time.RFC3339 and parsing it as such fails (see
// docs/exporter-journal.md, "Timestamps the library reports").
//
// Do not confuse it with eventsQueryLayout below. The two differ by one colon,
// in opposite directions, and the manual is explicit about both: responses
// carry ±hhmm and the after/before query parameters take ±hh:mm.
const eventTimeLayout = "2006-01-02T15:04:05-0700"

// eventsQueryLayout formats the `after` query parameter. R1.11.2 specifies
// YYYY-MM-DDThh:mm:ss±hh:mm for it — with the colon the response format omits.
//
// The offset is always sent explicitly rather than omitted. The manual allows
// omitting it ("the system's current time zone is used"), but that resolves the
// instant against the LIBRARY's timezone while the value was computed from the
// exporter host's clock, so a monitoring host in UTC watching a library in
// UTC+2 would silently shift its window by two hours. An explicit offset makes
// the parameter an absolute instant, leaving only real clock skew to worry
// about — which the lookback default is sized to absorb.
const eventsQueryLayout = "2006-01-02T15:04:05-07:00"

// eventStats is the parsed shape of one GET /v1/events entry, trimmed to the
// three fields this collector emits. The endpoint returns eight; the five left
// out are left out deliberately, and each for its own reason:
//
//   - `state` is interpolated free text, not an enumeration. R1.11.2 documents
//     values like "Assigned PMR <PMR number>. Service action required", so a
//     label would carry a PMR number straight into the cardinality.
//   - `description` is a prose sentence, for the same reason and more so.
//   - `user` and `location` are unbounded in principle: any account name, any
//     hardware location in the library. Neither is worth a series against a
//     count that already answers "how bad, and when".
//   - `ID` is the library's own sequence number. It identifies an event for
//     GET /v1/events/<ID>, which is where an operator goes after an alert
//     fires; it is not a quantity to graph.
//
// All three fields are pointers because all three must be distinguishable from
// their zero values: a missing severity takes eventSeverityUnknown, a missing
// time emits no last-seen series, and a missing errorCode matches no
// allow-list entry. A plain string would silently render each as "".
type eventStats struct {
	Severity  *string `json:"severity"`
	Time      *string `json:"time"`
	ErrorCode *string `json:"errorCode"`
}

// parseEventCodeAllowList turns the raw --collector.events.error-codes flag
// value into the set eventsGetMetrics matches against. Pure, so the flag's
// parsing is unit-testable without a collector or a library.
//
// Entries are trimmed and empties dropped, so "0834, ,B792," yields exactly
// two codes rather than four. Matching is case-insensitive — R1.11.2 describes
// errorCode as "a 4-digit hex error code" and the library reports the letters
// uppercase (B792), but an operator typing b792 into a flag has not made a
// mistake worth a silent no-match. The key is therefore folded to upper case
// here and at the comparison; the LABEL still carries the library's own
// spelling verbatim, so nothing in the exposition is rewritten by this
// convenience.
//
// Returns nil for an empty flag, which is the default: a nil set means the
// tapelibrary_events_by_code family is never emitted at all.
func parseEventCodeAllowList(raw string) map[string]struct{} {
	codes := make(map[string]struct{})
	for _, c := range strings.Split(raw, ",") {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		codes[strings.ToUpper(c)] = struct{}{}
	}
	if len(codes) == 0 {
		return nil
	}
	return codes
}

// eventsData is this collector's only I/O: it fetches the raw response body
// from the configured library. Kept separate from parsing (parseEvents, below)
// so parsing stays pure and unit-testable without a live library.
//
// The `after` parameter is not optional in practice, and this is the one place
// in the exporter where omitting a query parameter would be a bug rather than
// a simplification. R1.11.2 is explicit that a bare GET /v1/events "retrieves
// a list of all events": the 2026-07-28 capture carries IDs past 19 400, so a
// bare fetch would pull the library's entire event history over the slow
// SCSI/LCC path on every refresh, holding the library's single request slot
// (the concurrency ceiling is 1) against seventeen sibling collectors to
// re-download years of log lines for a metric that only reports on the last
// hour.
//
// Only `after` is sent, never `before`. That is deliberate beyond wanting an
// open-ended upper bound: the manual notes that REST over Ethernet requires
// `&` be encoded as `%26` when both parameters are used, and a single
// parameter has no `&` to get wrong.
//
// url.Values.Encode() builds the query rather than string concatenation,
// because the timezone offset in eventsQueryLayout's output starts with `+` on
// any library east of Greenwich, and a literal `+` in a query value decodes to
// a SPACE. Encode() writes it as %2B.
func (c *EventsCollector) eventsData(ctx context.Context) ([]byte, error) {
	after := time.Now().Add(-c.lookback).Format(eventsQueryLayout)
	q := url.Values{"after": []string{after}}
	return c.client.Fetch(ctx, "/events?"+q.Encode())
}

// parseEvents decodes eventsData's response body into one eventStats per
// event. Pure: no I/O, no logging, no side effects, so every input maps
// deterministically to an output. That is what makes it unit-testable with
// plain byte fixtures (see the test file's TestParseEvents).
//
// It is the most permissive parser in this exporter, and the difference is the
// endpoint's rather than an oversight. Every other collector here reads an
// inventory of hardware that certainly exists, so parsePowerSupplies rejects
// an empty array as a response that lost its content. This one reads a LOG
// over a bounded window: an empty array is the healthiest answer a library can
// give — nothing happened in the last hour — and rejecting it would keep the
// previous cache alive, so a library that had genuinely gone quiet would go on
// reporting the last event it ever raised until the exporter restarted.
//
// There is no duplicate-label-set check either, for the same structural
// reason: nothing below is keyed per entry. Events are folded into per-severity
// counts, so two identical entries are two events, not a collision that would
// fail Gather for the whole scrape (see CONTRIBUTING.md, "Common Pitfalls").
//
// What is left to reject is therefore only a body this collector cannot read
// at all: anything that is not a JSON array of objects.
func parseEvents(b []byte) ([]eventStats, error) {
	var entries []eventStats
	if err := json.Unmarshal(b, &entries); err != nil {
		return nil, fmt.Errorf("parse events response: %w", err)
	}
	return entries, nil
}

// eventsGetMetrics is the glue between the I/O step (eventsData) and the pure
// parsing step (parseEvents): the shape every collector in this exporter
// follows, regardless of flavor. refresh, below, calls this on its own
// background schedule; nothing else in this file calls the library directly.
func (c *EventsCollector) eventsGetMetrics(ctx context.Context) ([]eventStats, error) {
	data, err := c.eventsData(ctx)
	if err != nil {
		return nil, err
	}
	return parseEvents(data)
}

// EventsCollector reads GET /v1/events over a trailing window: how many events
// the library raised in the last --collector.events.lookback, broken down by
// severity, and when the most recent one of each severity happened.
//
// It is the only collector in this exporter that reads a log rather than a
// hardware inventory, and that shapes everything about it. The counts are
// Gauges over a sliding window, not counters: the library reports no cumulative
// event total, and reconstructing one client-side would reset on every
// exporter restart. `tapelibrary_events{severity="error"} > 0` therefore reads
// as "an error was raised in the last window", which is exactly the condition
// the shipped alert rules match.
//
// It is the background-refresh variant, which on this target model is not a
// choice: a scrape serves N libraries through one /metrics and must never
// block on a machine that has gone away. A background goroutine (started by
// Start, below) refreshes a cached metric slice on a fixed interval, and
// Collect only ever reads that cache under mu.
type EventsCollector struct {
	client   *Client
	interval time.Duration
	// lookback bounds the window sent as the `after` query parameter. It
	// should be >= interval: the window is a trailing one recomputed at every
	// refresh, so a lookback shorter than the gap between two refreshes leaves
	// intervals of time no refresh ever asks about, and an event landing in
	// one of those gaps is never counted. NewEventsCollector warns rather than
	// clamping — an operator who wants a short window on a fast interval is
	// making a real choice, and silently rewriting it would be worse.
	lookback time.Duration
	// errorCodes is the --collector.events.error-codes allow-list, upper-cased
	// by parseEventCodeAllowList. nil (the default) suppresses the
	// tapelibrary_events_by_code family entirely.
	errorCodes map[string]struct{}
	log        *logger.Logger

	count           *prometheus.Desc
	lastSeen        *prometheus.Desc
	countByCode     *prometheus.Desc
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

// NewEventsCollector builds the collector and its Descs. It is pure in the
// sense that matters: it starts no goroutine and performs no I/O, which is
// what makes it constructible in tests with no background refresh running.
// Call Start once, after construction, to begin refreshing.
//
// rawErrorCodes is the --collector.events.error-codes flag value verbatim, so
// main.go stays a plain pass-through of parsed flags like every other factory
// and the parsing lives here, where it is tested.
func NewEventsCollector(log *logger.Logger, client *Client, interval, lookback time.Duration, rawErrorCodes string) *EventsCollector {
	if lookback < interval {
		log.Warn("Events lookback is shorter than the refresh interval: events raised between two refreshes will never be counted",
			"lookback", lookback, "interval", interval)
	}

	return &EventsCollector{
		client:     client,
		interval:   interval,
		lookback:   lookback,
		errorCodes: parseEventCodeAllowList(rawErrorCodes),
		log:        log,
		count: prometheus.NewDesc(
			"tapelibrary_events",
			"Number of events of this severity the library raised within the collector's lookback window. Every documented severity is emitted, 0 included. inactiveError and inactiveWarning mean RESOLVED, not a lesser error: do not match them as active conditions.",
			[]string{"severity"}, nil,
		),
		lastSeen: prometheus.NewDesc(
			"tapelibrary_events_last_seen_timestamp_seconds",
			"Unix time of the most recent event of this severity within the collector's lookback window. Absent, never 0, when that severity raised nothing in the window: 0 would assert an event at the Unix epoch.",
			[]string{"severity"}, nil,
		),
		countByCode: prometheus.NewDesc(
			"tapelibrary_events_by_code",
			"Number of events carrying this library error code within the collector's lookback window. Emitted only for codes named in --collector.events.error-codes, and only for codes actually observed: absent means the code did not occur in the window.",
			[]string{"severity", "error_code"}, nil,
		),
		lastRefreshDesc: prometheus.NewDesc(
			"tapelibrary_events_last_refresh_timestamp_seconds",
			"Unix time of the last successful events refresh. Alert if time() - this > 2 x the collector's configured interval.",
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
func (c *EventsCollector) Start(ctx context.Context) {
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
func (c *EventsCollector) Done() <-chan struct{} {
	return c.done
}

// eventCodeKey is the label pair tapelibrary_events_by_code is keyed by. A
// struct map key rather than a joined string, so no separator can collide with
// a value: severity is a documented word and error_code a hex code today, but a
// key built by concatenation would be a latent bug the day either changes.
type eventCodeKey struct {
	severity  string
	errorCode string
}

// severityOf resolves one entry's severity label. A null or empty severity
// takes the eventSeverityUnknown token rather than being dropped, so the
// per-severity counts still sum to the number of events the endpoint returned
// (see eventSeverityUnknown's own comment for why that token is safe here).
func (c *EventsCollector) severityOf(e *eventStats) string {
	if e.Severity == nil || *e.Severity == "" {
		c.log.Warn("Event reported no severity: counting it as unknown")
		return eventSeverityUnknown
	}
	return *e.Severity
}

// eventTime parses one entry's `time` field, or reports that it contributes no
// last-seen reading. R1.11.2 does not document `time` as nullable, so unlike
// drives' lastCleaned there is no documented-absence case here: both a missing
// value and an unparseable one are responses this collector does not
// understand, and both log. The event still counts — it happened, whatever its
// clock said — which is why this returns a bool rather than dropping the entry.
func (c *EventsCollector) eventTime(e *eventStats) (time.Time, bool) {
	if e.Time == nil || *e.Time == "" {
		c.log.Warn("Event reported no time: counting it, but emitting no last-seen reading for it")
		return time.Time{}, false
	}
	ts, err := time.Parse(eventTimeLayout, *e.Time)
	if err != nil {
		c.log.Warn("Event reported an unparseable time: counting it, but emitting no last-seen reading for it",
			"value", *e.Time, "err", err)
		return time.Time{}, false
	}
	return ts, true
}

// refresh performs the one I/O call (eventsGetMetrics, via the injected
// *Client) and, on success, atomically replaces the cache. On error it logs
// and returns, leaving the previous cache and lastRefresh untouched,
// fail-open: a transient failure serves the last-known-good data instead of
// dropping the series, and the freshness gauge is the signal that a refresh
// is stale, not a dropped scrape.
func (c *EventsCollector) refresh(ctx context.Context) {
	events, err := c.eventsGetMetrics(ctx)
	if err != nil {
		c.log.Error("Failed to refresh events metrics: keeping previous cache", "err", err)
		return
	}

	counts := make(map[string]int, len(eventSeverities))
	newest := make(map[string]time.Time, len(eventSeverities))
	byCode := make(map[eventCodeKey]int)
	// codeLabels keeps the library's own spelling of each matched code, since
	// the allow-list is folded to upper case for matching but the label must
	// carry what came off the wire.
	codeLabels := make(map[string]string)

	for i := range events {
		e := &events[i]
		severity := c.severityOf(e)
		counts[severity]++

		if ts, ok := c.eventTime(e); ok {
			if prev, seen := newest[severity]; !seen || ts.After(prev) {
				newest[severity] = ts
			}
		}

		// Only allow-listed codes are keyed, so this map is bounded by the
		// operator's own flag rather than by whatever the library raised: a
		// 4-digit hex code has 65 536 possible values and R1.11.2 enumerates
		// none of them (see docs/exporter-journal.md, "Open questions").
		if c.errorCodes != nil && e.ErrorCode != nil && *e.ErrorCode != "" {
			folded := strings.ToUpper(*e.ErrorCode)
			if _, listed := c.errorCodes[folded]; listed {
				byCode[eventCodeKey{severity: severity, errorCode: folded}]++
				codeLabels[folded] = *e.ErrorCode
			}
		}
	}

	metrics := make([]prometheus.Metric, 0, len(eventSeverities)+len(newest)+len(byCode))

	// The full stateset: one count per documented severity, 0 included, so a
	// quiet library still emits every series an alert rule reads. `documented`
	// is what keeps the undocumented-severity branch below from re-emitting a
	// label set already sent here, which would fail Gather for this
	// collector's whole scrape.
	documented := make(map[string]struct{}, len(eventSeverities))
	for _, s := range eventSeverities {
		documented[s] = struct{}{}
		metrics = append(metrics, prometheus.MustNewConstMetric(c.count, prometheus.GaugeValue, float64(counts[s]), s))
	}

	// A severity outside the documented set surfaces as its own series rather
	// than being folded into one of the five or dropped. The manual's tables
	// are demonstrably a floor rather than a ceiling — it names states in prose
	// that appear in no table — and a severity nobody documented is exactly the
	// thing an operator needs to see, not the thing to hide. This is also where
	// eventSeverityUnknown reaches the wire.
	//
	// Sorted, so the cached slice is deterministic across refreshes with
	// identical input: Gather sorts the exposition anyway, but a stable cache
	// makes this collector's own tests and any future diffing of it honest.
	extras := make([]string, 0)
	for s := range counts {
		if _, ok := documented[s]; !ok {
			extras = append(extras, s)
		}
	}
	sort.Strings(extras)
	for _, s := range extras {
		if s != eventSeverityUnknown {
			c.log.Warn("Event reported a severity absent from the documented set: emitting it anyway", "severity", s)
		}
		metrics = append(metrics, prometheus.MustNewConstMetric(c.count, prometheus.GaugeValue, float64(counts[s]), s))
	}

	// Last-seen, only where there is something to report. A severity that
	// raised nothing in the window emits NO series here rather than a 0: zero
	// is not a missing reading, it is an assertion that an event happened at
	// the Unix epoch, which silently corrupts every "within the last N" query
	// rather than merely being absent from it (see docs/exporter-journal.md,
	// "Timestamps the library reports").
	severities := make([]string, 0, len(newest))
	for s := range newest {
		severities = append(severities, s)
	}
	sort.Strings(severities)
	for _, s := range severities {
		metrics = append(metrics, prometheus.MustNewConstMetric(c.lastSeen, prometheus.GaugeValue, float64(newest[s].Unix()), s))
	}

	keys := make([]eventCodeKey, 0, len(byCode))
	for k := range byCode {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].severity != keys[j].severity {
			return keys[i].severity < keys[j].severity
		}
		return keys[i].errorCode < keys[j].errorCode
	})
	for _, k := range keys {
		metrics = append(metrics, prometheus.MustNewConstMetric(c.countByCode, prometheus.GaugeValue, float64(byCode[k]), k.severity, codeLabels[k.errorCode]))
	}

	c.mu.Lock()
	c.cached = metrics
	c.lastRefresh = time.Now()
	c.mu.Unlock()
}

// Describe sends every one of this collector's descriptors, including the
// freshness gauge. Constant regardless of scrape or refresh outcome, which
// is what makes prometheus.DescribeByCollect unnecessary here.
//
// countByCode is described even when --collector.events.error-codes is empty
// and it will emit nothing: a descriptor is what this collector CAN emit, not
// what it did last time, and docs/metrics.md documents it on that basis. This
// matches how drives describes loadedCartridge with --per-volser off.
func (c *EventsCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.count
	ch <- c.lastSeen
	ch <- c.countByCode
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
func (c *EventsCollector) Collect(ch chan<- prometheus.Metric) {
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
