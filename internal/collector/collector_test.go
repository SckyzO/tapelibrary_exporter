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

// TestParseExample exercises parseExample (piece 2, the pure parser) with a
// static byte fixture: no HTTP, no collector, no logger, no goroutine
// involved. Identical in spirit to the synchronous flavor's own test of the
// same name: the parser is pure regardless of how its caller schedules I/O.
func TestParseExample(t *testing.T) {
	data, err := os.ReadFile("testdata/example.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	stats, err := parseExample(data)
	if err != nil {
		t.Fatalf("parseExample: %v", err)
	}
	if stats.Items != 42 {
		t.Errorf("Items = %d, want 42", stats.Items)
	}
	if !stats.Healthy {
		t.Error("Healthy = false, want true")
	}

	t.Run("malformed input yields an error, not a panic", func(t *testing.T) {
		if _, err := parseExample([]byte("not json")); err == nil {
			t.Error("parseExample(malformed) returned a nil error, want non-nil")
		}
	})
}

// TestExampleCollector_Describe locks the descriptor count at exactly 3
// (items, healthy, and the freshness gauge lastRefreshDesc, one more than
// the synchronous flavor, which has no freshness gauge) so a future edit
// that silently adds or drops a metric is caught here rather than
// downstream.
func TestExampleCollector_Describe(t *testing.T) {
	log := logger.NewTextLogger("error")
	c := NewExampleCollector(log, NewClient("http://example.invalid", time.Second), time.Hour)

	ch := make(chan *prometheus.Desc, 10)
	c.Describe(ch)
	close(ch)

	count := 0
	for range ch {
		count++
	}
	if count != 3 {
		t.Fatalf("Describe sent %d descriptors, want 3", count)
	}
}

// TestExampleCollector_DoneClosesOnCancel verifies the Done() channel closes
// when the context passed to Start is cancelled. This is the mechanism
// main.go's backgroundCollector shutdown seam relies on (see
// cmd/tapelibrary_exporter/main.go's wait loop after web.ListenAndServe).
func TestExampleCollector_DoneClosesOnCancel(t *testing.T) {
	log := logger.NewTextLogger("error")
	c := NewExampleCollector(log, NewClient("http://example.invalid", time.Second), time.Hour)
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

// TestExampleCollector_CollectServesCacheWithoutIO proves Collect never
// calls the target: after Start's own immediate refresh completes, a long
// interval (1 hour) guarantees the ticker cannot fire again during this
// test, so any further request the server receives could only come from
// Collect itself calling out, which the design forbids. Scraping (via
// Gather) three times back to back must not move the request counter past
// whatever Start's own first refresh already set it to.
func TestExampleCollector_CollectServesCacheWithoutIO(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.ServeFile(w, r, "testdata/example.json")
	}))
	defer srv.Close()

	log := logger.NewTextLogger("error")
	c := NewExampleCollector(log, NewClient(srv.URL, time.Second), time.Hour)
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
		t.Fatalf("calls after 3 scrapes = %d, want %d unchanged: Collect must never call the target", got, afterStart)
	}
}

// TestExampleCollector_ErrorKeepsPreviousCache scripts the backend to
// succeed once, then fail on every later call, and drives at least one more
// refresh via a short interval. The cache from the successful first refresh
// must survive the later failure (fail-open, per this collector's Collect
// doc comment) rather than being cleared or replaced with nothing.
func TestExampleCollector_ErrorKeepsPreviousCache(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			http.ServeFile(w, r, "testdata/example.json")
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	log := logger.NewTextLogger("error")
	c := NewExampleCollector(log, NewClient(srv.URL, time.Second), 20*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		cancel()
		<-c.Done()
	}()

	c.Start(ctx)

	// Wait for backend call 3 to start, rather than sleeping a fixed
	// duration: refresh runs synchronously on Start's single goroutine, so
	// the ticker cannot dispatch call 3 until call 2 (the first failing,
	// 500 refresh) has fully returned, its error branch included. Seeing
	// calls reach 3 is therefore a deterministic proof that call 2's whole
	// error path, not merely the server's response, has already completed,
	// which a fixed sleep can only ever approximate.
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
	// A surviving cache emits exactly 3 metrics on a scrape: the 2 cached
	// business metrics (items, healthy) from the successful first refresh,
	// plus the freshness gauge Collect always emits regardless of cache
	// state. A wrongly-cleared cache would emit only 1 (the gauge alone).
	// Count > 0 cannot tell those two outcomes apart, since the gauge alone
	// already satisfies "> 0". Assert the exact count instead.
	if count != 3 {
		t.Fatalf("GatherAndCount = %d, want 3 (2 cached metrics + freshness gauge): the previous cache must survive a later refresh error", count)
	}
}

// TestExampleCollector_FirstScrapeEmitsFreshnessGaugeZero covers the startup
// window before Start's first refresh has completed (Start is deliberately
// never called here). Collect must still emit exactly the freshness gauge,
// valued 0 (not a zero time.Time's large-negative Unix()), and StatusTracker
// must still report this collector as successful: because "Collect ran
// and returned data" and "the data is fresh" are different questions (see
// this collector's Collect doc comment); an empty cache before the first
// refresh is a normal startup state, not a failed scrape.
func TestExampleCollector_FirstScrapeEmitsFreshnessGaugeZero(t *testing.T) {
	log := logger.NewTextLogger("error")
	c := NewExampleCollector(log, NewClient("http://example.invalid", time.Second), time.Hour)
	// Do NOT call Start(): cache is empty, no refresh has ever run.

	expected := `
# HELP tapelibrary_example_last_refresh_timestamp_seconds Unix time of the last successful example refresh. Alert if time() - this > 2 x the collector's configured interval.
# TYPE tapelibrary_example_last_refresh_timestamp_seconds gauge
tapelibrary_example_last_refresh_timestamp_seconds 0
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected)); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}

	tracker := NewStatusTracker(log)
	tracker.Add("example", c)

	expectedSuccess := `
# HELP ` + statusTrackerSuccessMetric + ` Whether the last scrape of the collector succeeded (1=success, 0=failure)
# TYPE ` + statusTrackerSuccessMetric + ` gauge
` + statusTrackerSuccessMetric + `{collector="example"} 1
`
	if err := testutil.CollectAndCompare(tracker, strings.NewReader(expectedSuccess), statusTrackerSuccessMetric); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}
}
