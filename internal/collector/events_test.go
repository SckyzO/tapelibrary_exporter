package collector

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/sckyzo/tapelibrary_exporter/internal/logger"
)

// eventsFixtureServer serves testdata/events.json on every request and returns
// a collector already pointed at it, with no error-code allow-list. Nothing is
// started: the caller decides whether to drive refresh directly
// (deterministic) or via Start (which is what the lifecycle tests below
// exercise).
func eventsFixtureServer(t *testing.T) (*httptest.Server, *EventsCollector) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "testdata/events.json")
	}))
	t.Cleanup(srv.Close)

	log := logger.NewTextLogger("error")
	return srv, NewEventsCollector(log, NewClient(srv.URL, time.Second), time.Hour, time.Hour, "")
}

// eventsServing returns a collector fed by a server that answers every request
// with body. Used by the branch tests below, which need input shapes the real
// capture does not contain and which must therefore not be invented inside
// testdata/events.json (that fixture stays faithful to the 2026-07-28 capture,
// in which every event is `information`).
func eventsServing(t *testing.T, body string) *EventsCollector {
	t.Helper()
	return eventsServingWithCodes(t, body, "")
}

// eventsServingWithCodes is eventsServing with an explicit
// --collector.events.error-codes value, for the allow-list tests.
func eventsServingWithCodes(t *testing.T, body, codes string) *EventsCollector {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	log := logger.NewTextLogger("error")
	return NewEventsCollector(log, NewClient(srv.URL, time.Second), time.Hour, time.Hour, codes)
}

// TestParseEvents exercises parseEvents (piece 2, the pure parser) with a
// static byte fixture: no HTTP, no collector, no logger, no goroutine
// involved.
func TestParseEvents(t *testing.T) {
	data, err := os.ReadFile("testdata/events.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	events, err := parseEvents(data)
	if err != nil {
		t.Fatalf("parseEvents(fixture): %v", err)
	}
	if len(events) != 10 {
		t.Fatalf("parseEvents(fixture) returned %d events, want 10", len(events))
	}

	first := events[0]
	if first.Severity == nil || *first.Severity != "information" {
		t.Errorf("events[0].Severity = %v, want \"information\"", first.Severity)
	}
	if first.Time == nil || *first.Time != "2026-07-28T06:00:05+0000" {
		t.Errorf("events[0].Time = %v, want \"2026-07-28T06:00:05+0000\"", first.Time)
	}
	if first.ErrorCode == nil || *first.ErrorCode != "0832" {
		t.Errorf("events[0].ErrorCode = %v, want \"0832\"", first.ErrorCode)
	}

	t.Run("empty array is a healthy quiet library, not an error", func(t *testing.T) {
		// The single most important line in this file. Every other collector
		// in this exporter rejects an empty array as a response that lost its
		// content; this one reads a LOG over a bounded window, where "nothing
		// happened in the last hour" is the best answer a library can give.
		// Rejecting it would keep the previous cache alive, so a library that
		// went quiet would go on reporting its last event forever.
		events, err := parseEvents([]byte(`[]`))
		if err != nil {
			t.Fatalf("parseEvents(empty array): %v, want nil", err)
		}
		if len(events) != 0 {
			t.Fatalf("parseEvents(empty array) returned %d events, want 0", len(events))
		}
	})

	t.Run("null fields decode to nil rather than empty strings", func(t *testing.T) {
		events, err := parseEvents([]byte(`[{"ID":1,"severity":null,"time":null,"errorCode":null}]`))
		if err != nil {
			t.Fatalf("parseEvents: %v", err)
		}
		if events[0].Severity != nil || events[0].Time != nil || events[0].ErrorCode != nil {
			t.Fatalf("null fields decoded to non-nil: %+v", events[0])
		}
	})

	t.Run("malformed JSON is rejected", func(t *testing.T) {
		if _, err := parseEvents([]byte(`{"not":"an array"}`)); err == nil {
			t.Fatal("parseEvents(object) returned nil error, want a parse failure")
		}
	})

	t.Run("empty body is rejected", func(t *testing.T) {
		if _, err := parseEvents(nil); err == nil {
			t.Fatal("parseEvents(nil) returned nil error, want a parse failure")
		}
	})
}

// TestParseEventCodeAllowList covers the flag parsing, which decides whether
// the tapelibrary_events_by_code family is emitted at all.
func TestParseEventCodeAllowList(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want []string // nil means "expect a nil set"
	}{
		{"empty flag suppresses the family", "", nil},
		{"whitespace only suppresses the family", "  ,  , ", nil},
		{"single code", "B792", []string{"B792"}},
		{"several codes", "0834,B792", []string{"0834", "B792"}},
		{"spaces and empties are trimmed", " 0834 , ,B792, ", []string{"0834", "B792"}},
		{"matching is case-insensitive", "b792", []string{"B792"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseEventCodeAllowList(tt.raw)
			if tt.want == nil {
				if got != nil {
					t.Fatalf("parseEventCodeAllowList(%q) = %v, want nil", tt.raw, got)
				}
				return
			}
			if len(got) != len(tt.want) {
				t.Fatalf("parseEventCodeAllowList(%q) has %d entries, want %d", tt.raw, len(got), len(tt.want))
			}
			for _, code := range tt.want {
				if _, ok := got[code]; !ok {
					t.Errorf("parseEventCodeAllowList(%q) is missing %q", tt.raw, code)
				}
			}
		})
	}
}

// TestEventsCollector_Describe pins the descriptor count. countByCode is
// described even with no allow-list configured, because a descriptor states
// what this collector CAN emit and docs/metrics.md documents it on that basis.
func TestEventsCollector_Describe(t *testing.T) {
	log := logger.NewTextLogger("error")
	c := NewEventsCollector(log, NewClient("http://example.invalid", time.Second), time.Hour, time.Hour, "")

	ch := make(chan *prometheus.Desc, 20)
	c.Describe(ch)
	close(ch)

	count := 0
	for range ch {
		count++
	}
	if count != 4 {
		t.Fatalf("Describe sent %d descriptors, want 4", count)
	}
}

// TestEventsCollector_Collect pins the exact exposition text against the
// fixture. refresh is driven directly rather than through Start, so the
// assertion is deterministic and carries no sleep: Start's own scheduling is
// what the lifecycle tests below cover.
//
// All five documented severities are emitted, four of them at 0, which is what
// lets the shipped alert rules read `> 0` from the first scrape rather than
// only after the library's first error.
func TestEventsCollector_Collect(t *testing.T) {
	_, c := eventsFixtureServer(t)
	c.refresh(context.Background())

	expected := `
# HELP tapelibrary_events Number of events of this severity the library raised within the collector's lookback window. Every documented severity is emitted, 0 included. inactiveError and inactiveWarning mean RESOLVED, not a lesser error: do not match them as active conditions.
# TYPE tapelibrary_events gauge
tapelibrary_events{severity="error"} 0
tapelibrary_events{severity="inactiveError"} 0
tapelibrary_events{severity="inactiveWarning"} 0
tapelibrary_events{severity="information"} 10
tapelibrary_events{severity="warning"} 0
# HELP tapelibrary_events_last_seen_timestamp_seconds Unix time of the most recent event of this severity within the collector's lookback window. Absent, never 0, when that severity raised nothing in the window: 0 would assert an event at the Unix epoch.
# TYPE tapelibrary_events_last_seen_timestamp_seconds gauge
tapelibrary_events_last_seen_timestamp_seconds{severity="information"} 1.785230151e+09
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected),
		"tapelibrary_events", "tapelibrary_events_last_seen_timestamp_seconds"); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}

	// 5 severity counts + 1 last-seen (only `information` occurred in the
	// fixture) + the freshness gauge Collect always appends. No
	// tapelibrary_events_by_code: the fixture collector carries no allow-list.
	reg := prometheus.NewRegistry()
	if err := reg.Register(c); err != nil {
		t.Fatalf("Register: %v", err)
	}
	count, err := testutil.GatherAndCount(reg)
	if err != nil {
		t.Fatalf("GatherAndCount: %v", err)
	}
	if count != 7 {
		t.Fatalf("GatherAndCount = %d, want 7 (5 severities + 1 last-seen + freshness)", count)
	}
}

// TestEventsCollector_QuietLibrary covers the empty window: the healthiest
// answer this endpoint can give, and the one every other collector in this
// exporter treats as a broken response. Every severity must still be emitted
// at 0, and no last-seen series at all.
//
// This is what stops a library going quiet from looking like a library going
// away: if the empty array were rejected, refresh would keep the previous
// cache and the last error ever raised would stay pinned at 1 forever.
func TestEventsCollector_QuietLibrary(t *testing.T) {
	c := eventsServing(t, `[]`)
	c.refresh(context.Background())

	expected := `
# HELP tapelibrary_events Number of events of this severity the library raised within the collector's lookback window. Every documented severity is emitted, 0 included. inactiveError and inactiveWarning mean RESOLVED, not a lesser error: do not match them as active conditions.
# TYPE tapelibrary_events gauge
tapelibrary_events{severity="error"} 0
tapelibrary_events{severity="inactiveError"} 0
tapelibrary_events{severity="inactiveWarning"} 0
tapelibrary_events{severity="information"} 0
tapelibrary_events{severity="warning"} 0
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected),
		"tapelibrary_events", "tapelibrary_events_last_seen_timestamp_seconds"); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}
}

// TestEventsCollector_ErrorAndWarningSeverities covers the two severities the
// shipped alert rules read. The 2026-07-28 capture holds nothing but
// `information` events, so the fixture cannot exercise this path and a
// synthetic body must: without it, the whole reason this collector exists
// would ship untested.
//
// It also pins the resolved/active distinction. inactiveError and
// inactiveWarning mean an error that has been RESOLVED, so they count on their
// own series and must never be folded into the active ones.
func TestEventsCollector_ErrorAndWarningSeverities(t *testing.T) {
	c := eventsServing(t, `[
		{"ID":1,"severity":"error","time":"2026-07-28T09:00:00+0000","errorCode":"B792"},
		{"ID":2,"severity":"error","time":"2026-07-28T09:05:00+0000","errorCode":"B792"},
		{"ID":3,"severity":"warning","time":"2026-07-28T09:10:00+0000","errorCode":"0217"},
		{"ID":4,"severity":"inactiveError","time":"2026-07-28T09:15:00+0000","errorCode":"B792"},
		{"ID":5,"severity":"inactiveWarning","time":"2026-07-28T09:20:00+0000","errorCode":"0217"}
	]`)
	c.refresh(context.Background())

	expected := `
# HELP tapelibrary_events Number of events of this severity the library raised within the collector's lookback window. Every documented severity is emitted, 0 included. inactiveError and inactiveWarning mean RESOLVED, not a lesser error: do not match them as active conditions.
# TYPE tapelibrary_events gauge
tapelibrary_events{severity="error"} 2
tapelibrary_events{severity="inactiveError"} 1
tapelibrary_events{severity="inactiveWarning"} 1
tapelibrary_events{severity="information"} 0
tapelibrary_events{severity="warning"} 1
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected), "tapelibrary_events"); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}

	// Last-seen is per severity and takes the NEWEST of each, not the newest
	// overall: event 2 for `error`, not event 5.
	expectedSeen := `
# HELP tapelibrary_events_last_seen_timestamp_seconds Unix time of the most recent event of this severity within the collector's lookback window. Absent, never 0, when that severity raised nothing in the window: 0 would assert an event at the Unix epoch.
# TYPE tapelibrary_events_last_seen_timestamp_seconds gauge
tapelibrary_events_last_seen_timestamp_seconds{severity="error"} 1.7852295e+09
tapelibrary_events_last_seen_timestamp_seconds{severity="inactiveError"} 1.7852301e+09
tapelibrary_events_last_seen_timestamp_seconds{severity="inactiveWarning"} 1.7852304e+09
tapelibrary_events_last_seen_timestamp_seconds{severity="warning"} 1.7852298e+09
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expectedSeen), "tapelibrary_events_last_seen_timestamp_seconds"); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}
}

// TestEventsCollector_UndocumentedSeverityIsStillEmitted covers the manual's
// own incompleteness: it names states in prose that appear in none of its
// tables. A severity outside the documented set must surface as its own series
// rather than being folded into one of the five or dropped, which would make
// the counts fail to sum to the number of events returned with nothing on the
// wire explaining the gap.
func TestEventsCollector_UndocumentedSeverityIsStillEmitted(t *testing.T) {
	c := eventsServing(t, `[{"ID":1,"severity":"catastrophic","time":"2026-07-28T09:00:00+0000","errorCode":"B792"}]`)
	c.refresh(context.Background())

	expected := `
# HELP tapelibrary_events Number of events of this severity the library raised within the collector's lookback window. Every documented severity is emitted, 0 included. inactiveError and inactiveWarning mean RESOLVED, not a lesser error: do not match them as active conditions.
# TYPE tapelibrary_events gauge
tapelibrary_events{severity="catastrophic"} 1
tapelibrary_events{severity="error"} 0
tapelibrary_events{severity="inactiveError"} 0
tapelibrary_events{severity="inactiveWarning"} 0
tapelibrary_events{severity="information"} 0
tapelibrary_events{severity="warning"} 0
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected), "tapelibrary_events"); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}
}

// TestEventsCollector_NullSeverityCountsAsUnknown pins the nullable-label
// branch. A null severity is a malformed event, but dropping it would leave
// the per-severity counts failing to sum to the number of events the endpoint
// returned, with nothing on the wire to explain the gap — so it takes the
// literal `unknown` token, which collides with no documented severity.
func TestEventsCollector_NullSeverityCountsAsUnknown(t *testing.T) {
	c := eventsServing(t, `[
		{"ID":1,"severity":null,"time":"2026-07-28T09:00:00+0000","errorCode":"B792"},
		{"ID":2,"severity":"","time":"2026-07-28T09:01:00+0000","errorCode":"B792"}
	]`)
	c.refresh(context.Background())

	expected := `
# HELP tapelibrary_events Number of events of this severity the library raised within the collector's lookback window. Every documented severity is emitted, 0 included. inactiveError and inactiveWarning mean RESOLVED, not a lesser error: do not match them as active conditions.
# TYPE tapelibrary_events gauge
tapelibrary_events{severity="error"} 0
tapelibrary_events{severity="inactiveError"} 0
tapelibrary_events{severity="inactiveWarning"} 0
tapelibrary_events{severity="information"} 0
tapelibrary_events{severity="unknown"} 2
tapelibrary_events{severity="warning"} 0
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected), "tapelibrary_events"); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}
}

// TestEventsCollector_UnparseableTimeStillCounts pins the timestamp
// convention. The wire format carries no colon in its zone offset, so an
// RFC3339 value is exactly the kind of thing that fails to parse. The event
// still counts — it happened, whatever its clock said — but contributes no
// last-seen reading, and 0 is never emitted in its place: 0 would assert an
// event at the Unix epoch.
func TestEventsCollector_UnparseableTimeStillCounts(t *testing.T) {
	c := eventsServing(t, `[
		{"ID":1,"severity":"error","time":"2026-07-28T09:00:00Z","errorCode":"B792"},
		{"ID":2,"severity":"error","time":null,"errorCode":"B792"}
	]`)
	c.refresh(context.Background())

	expected := `
# HELP tapelibrary_events Number of events of this severity the library raised within the collector's lookback window. Every documented severity is emitted, 0 included. inactiveError and inactiveWarning mean RESOLVED, not a lesser error: do not match them as active conditions.
# TYPE tapelibrary_events gauge
tapelibrary_events{severity="error"} 2
tapelibrary_events{severity="inactiveError"} 0
tapelibrary_events{severity="inactiveWarning"} 0
tapelibrary_events{severity="information"} 0
tapelibrary_events{severity="warning"} 0
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected),
		"tapelibrary_events", "tapelibrary_events_last_seen_timestamp_seconds"); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}
}

// TestEventsCollector_ErrorCodeAllowList covers the opt-in family. Only listed
// codes are emitted, matching is case-insensitive, and the LABEL carries the
// library's own spelling rather than the folded key.
func TestEventsCollector_ErrorCodeAllowList(t *testing.T) {
	c := eventsServingWithCodes(t, `[
		{"ID":1,"severity":"error","time":"2026-07-28T09:00:00+0000","errorCode":"B792"},
		{"ID":2,"severity":"error","time":"2026-07-28T09:05:00+0000","errorCode":"B792"},
		{"ID":3,"severity":"warning","time":"2026-07-28T09:10:00+0000","errorCode":"B792"},
		{"ID":4,"severity":"error","time":"2026-07-28T09:15:00+0000","errorCode":"0217"}
	]`, "b792")
	c.refresh(context.Background())

	// 0217 is not listed, so it contributes to tapelibrary_events but to no
	// by-code series. B792 splits across two severities, which is why severity
	// is a label on this family rather than being dropped.
	expected := `
# HELP tapelibrary_events_by_code Number of events carrying this library error code within the collector's lookback window. Emitted only for codes named in --collector.events.error-codes, and only for codes actually observed: absent means the code did not occur in the window.
# TYPE tapelibrary_events_by_code gauge
tapelibrary_events_by_code{error_code="B792",severity="error"} 2
tapelibrary_events_by_code{error_code="B792",severity="warning"} 1
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected), "tapelibrary_events_by_code"); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}
}

// TestEventsCollector_NoAllowListEmitsNoByCodeSeries pins the default. An
// empty --collector.events.error-codes must suppress the family entirely
// rather than emitting one series per code the library happened to raise,
// which is the unbounded-cardinality outcome the flag exists to prevent.
func TestEventsCollector_NoAllowListEmitsNoByCodeSeries(t *testing.T) {
	c := eventsServing(t, `[
		{"ID":1,"severity":"error","time":"2026-07-28T09:00:00+0000","errorCode":"B792"},
		{"ID":2,"severity":"error","time":"2026-07-28T09:05:00+0000","errorCode":"0217"}
	]`)
	c.refresh(context.Background())

	if err := testutil.CollectAndCompare(c, strings.NewReader(""), "tapelibrary_events_by_code"); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}
}

// TestEventsCollector_FetchesABoundedWindow is the regression test for the
// single most expensive mistake this collector could make. R1.11.2 is explicit
// that a bare GET /v1/events "retrieves a list of all events", and the capture
// carries IDs past 19 400 — so a request without `after` would pull the
// library's entire event history over the slow SCSI/LCC path on every refresh,
// holding the library's only request slot against seventeen siblings.
//
// It also pins the encoding. The offset in eventsQueryLayout's output starts
// with `+` east of Greenwich, and a literal `+` in a query value decodes to a
// SPACE, so the parameter has to survive a round trip through a real URL
// parser rather than merely appearing in the raw query string.
func TestEventsCollector_FetchesABoundedWindow(t *testing.T) {
	var gotQuery atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery.Store(r.URL.Query().Get("after"))
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	log := logger.NewTextLogger("error")
	c := NewEventsCollector(log, NewClient(srv.URL, time.Second), 5*time.Minute, 90*time.Minute, "")

	before := time.Now()
	c.refresh(context.Background())

	raw, _ := gotQuery.Load().(string)
	if raw == "" {
		t.Fatal("request carried no `after` parameter: a bare GET /v1/events returns the library's entire event history")
	}

	// The server decoded it, so `+02:00` cannot have arrived as ` 02:00`.
	after, err := time.Parse(eventsQueryLayout, raw)
	if err != nil {
		t.Fatalf("after=%q does not parse as the manual's query format: %v", raw, err)
	}

	// The window must be the configured lookback back from roughly now,
	// bounded generously so a slow test machine cannot flake it.
	want := before.Add(-90 * time.Minute)
	if delta := after.Sub(want); delta < -time.Minute || delta > time.Minute {
		t.Fatalf("after=%v is %v away from now-lookback (%v), want within a minute", after, delta, want)
	}
}

// TestEventsCollector_DoneClosesOnCancel verifies the Done() channel closes
// when the context passed to Start is cancelled. This is the mechanism
// main.go's shutdown seam relies on (see registry.Wait after
// web.ListenAndServe).
func TestEventsCollector_DoneClosesOnCancel(t *testing.T) {
	log := logger.NewTextLogger("error")
	c := NewEventsCollector(log, NewClient("http://example.invalid", time.Second), time.Hour, time.Hour, "")
	ctx, cancel := context.WithCancel(context.Background())

	c.Start(ctx)
	cancel()

	select {
	case <-c.Done():
		// success
	case <-time.After(2 * time.Second):
		t.Fatal("Done() did not close within 2s after cancel: graceful shutdown broken")
	}
}

// TestEventsCollector_CollectServesCacheWithoutIO proves Collect never calls
// the library: after Start's own immediate refresh completes, a long interval
// (1 hour) guarantees the ticker cannot fire again during this test, so any
// further request the server receives could only come from Collect itself
// calling out, which the design forbids.
func TestEventsCollector_CollectServesCacheWithoutIO(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.ServeFile(w, r, "testdata/events.json")
	}))
	defer srv.Close()

	log := logger.NewTextLogger("error")
	c := NewEventsCollector(log, NewClient(srv.URL, time.Second), time.Hour, time.Hour, "")
	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		cancel()
		<-c.Done()
	}()

	c.Start(ctx)
	time.Sleep(100 * time.Millisecond) // let Start's immediate refresh land

	afterStart := calls.Load()
	if afterStart == 0 {
		t.Fatal("calls = 0 after Start, want >= 1: Start must run an immediate refresh")
	}

	reg := prometheus.NewRegistry()
	if err := reg.Register(c); err != nil {
		t.Fatalf("Register: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := testutil.GatherAndCount(reg); err != nil {
			t.Fatalf("GatherAndCount (scrape %d): %v", i, err)
		}
	}

	if got := calls.Load(); got != afterStart {
		t.Fatalf("calls after 3 scrapes = %d, want %d unchanged: Collect must never call the library", got, afterStart)
	}
}

// TestEventsCollector_ErrorHandling drives a refresh against a library that
// only ever fails. No cache was ever filled, so the scrape must carry exactly
// the freshness gauge, and must neither panic nor emit a partial stateset.
func TestEventsCollector_ErrorHandling(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	log := logger.NewTextLogger("error")
	c := NewEventsCollector(log, NewClient(srv.URL, time.Second), time.Hour, time.Hour, "")
	c.refresh(context.Background())

	reg := prometheus.NewRegistry()
	if err := reg.Register(c); err != nil {
		t.Fatalf("Register: %v", err)
	}
	count, err := testutil.GatherAndCount(reg)
	if err != nil {
		t.Fatalf("GatherAndCount: %v", err)
	}
	if count != 1 {
		t.Fatalf("GatherAndCount = %d, want 1 (freshness gauge only): a failed first refresh must cache nothing", count)
	}
}

// TestEventsCollector_ErrorKeepsPreviousCache scripts the backend to succeed
// once, then fail on every later call, and drives at least one more refresh
// via a short interval. The cache from the successful first refresh must
// survive the later failure (fail-open, per refresh's doc comment) rather than
// being cleared or replaced with nothing.
//
// A cleared cache would drop tapelibrary_events{severity="error"} entirely,
// and an absent series does not satisfy the shipped rules' `> 0`, so a library
// that became unreachable would silently stop being watched for new errors
// rather than alerting.
func TestEventsCollector_ErrorKeepsPreviousCache(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			http.ServeFile(w, r, "testdata/events.json")
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	log := logger.NewTextLogger("error")
	c := NewEventsCollector(log, NewClient(srv.URL, time.Second), 20*time.Millisecond, time.Hour, "")
	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		cancel()
		<-c.Done()
	}()

	c.Start(ctx)

	// Wait for backend call 3 to start, rather than sleeping a fixed
	// duration: refresh runs synchronously on Start's single goroutine, so
	// the ticker cannot dispatch call 3 until call 2 (the first failing, 500
	// refresh) has fully returned, its error branch included. Seeing calls
	// reach 3 is therefore a deterministic proof that call 2's whole error
	// path, not merely the server's response, has already completed.
	deadline := time.Now().Add(2 * time.Second)
	for calls.Load() < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("calls = %d after 2s, want >= 3: a failing refresh's error path never completed", calls.Load())
		}
		time.Sleep(5 * time.Millisecond)
	}

	reg := prometheus.NewRegistry()
	if err := reg.Register(c); err != nil {
		t.Fatalf("Register: %v", err)
	}
	count, err := testutil.GatherAndCount(reg)
	if err != nil {
		t.Fatalf("GatherAndCount: %v", err)
	}
	// A surviving cache emits 7 metrics: the 6 cached from the successful
	// first refresh plus the freshness gauge. A wrongly-cleared cache would
	// emit only 1 (the gauge alone), which "> 0" cannot tell apart.
	if count != 7 {
		t.Fatalf("GatherAndCount = %d, want 7: the previous cache must survive a later refresh error", count)
	}
}

// TestEventsCollector_StatusTrackerSuccessOnFirstScrape covers the startup
// window before Start's first refresh has completed (Start is deliberately
// never called here). Collect must still emit exactly the freshness gauge,
// valued 0 (not a zero time.Time's large-negative Unix()), and StatusTracker
// must still report this collector as successful: "Collect ran and returned
// data" and "the data is fresh" are different questions.
func TestEventsCollector_StatusTrackerSuccessOnFirstScrape(t *testing.T) {
	log := logger.NewTextLogger("error")
	c := NewEventsCollector(log, NewClient("http://example.invalid", time.Second), time.Hour, time.Hour, "")
	// Do NOT call Start(): cache is empty, no refresh has ever run.

	expected := `
# HELP tapelibrary_events_last_refresh_timestamp_seconds Unix time of the last successful events refresh. Alert if time() - this > 2 x the collector's configured interval.
# TYPE tapelibrary_events_last_refresh_timestamp_seconds gauge
tapelibrary_events_last_refresh_timestamp_seconds 0
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected)); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}

	tracker := NewStatusTracker(log)
	tracker.Add("events", c)

	expectedSuccess := `
# HELP ` + statusTrackerSuccessMetric + ` Whether the last scrape of the collector succeeded (1=success, 0=failure)
# TYPE ` + statusTrackerSuccessMetric + ` gauge
` + statusTrackerSuccessMetric + `{collector="events"} 1
`
	if err := testutil.CollectAndCompare(tracker, strings.NewReader(expectedSuccess), statusTrackerSuccessMetric); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}
}
