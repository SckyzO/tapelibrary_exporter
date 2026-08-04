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

// powerSuppliesFixtureServer serves testdata/power_supplies.json on every
// request and returns a collector already pointed at it. Nothing is started:
// the caller decides whether to drive refresh directly (deterministic) or via
// Start (which is what the lifecycle tests below exercise).
func powerSuppliesFixtureServer(t *testing.T) (*httptest.Server, *PowerSuppliesCollector) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "testdata/power_supplies.json")
	}))
	t.Cleanup(srv.Close)

	log := logger.NewTextLogger("error")
	return srv, NewPowerSuppliesCollector(log, NewClient(srv.URL, time.Second), time.Hour)
}

// powerSuppliesServing returns a collector fed by a server that answers every
// request with body. Used by the branch tests below, which need input shapes
// the real capture does not contain and which must therefore not be invented
// inside testdata/power_supplies.json (that fixture stays faithful to the
// 2026-07-28 capture, in which every supply is online).
func powerSuppliesServing(t *testing.T, body string) *PowerSuppliesCollector {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	log := logger.NewTextLogger("error")
	return NewPowerSuppliesCollector(log, NewClient(srv.URL, time.Second), time.Hour)
}

// TestParsePowerSupplies exercises parsePowerSupplies (piece 2, the pure
// parser) with a static byte fixture: no HTTP, no collector, no logger, no
// goroutine involved.
func TestParsePowerSupplies(t *testing.T) {
	data, err := os.ReadFile("testdata/power_supplies.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	supplies, err := parsePowerSupplies(data)
	if err != nil {
		t.Fatalf("parsePowerSupplies: %v", err)
	}

	// Eight supplies across four frames: the capture's full set, not a trim.
	// Only L25/L55 and D25/D55 frames carry supplies, which is why a
	// twelve-frame library reports eight rather than twenty-four.
	if len(supplies) != 8 {
		t.Fatalf("len(supplies) = %d, want 8", len(supplies))
	}

	first := supplies[0]
	if first.Location != "powerSupply_F1PSa" {
		t.Errorf("Location = %q, want %q", first.Location, "powerSupply_F1PSa")
	}
	if first.State != "online" {
		t.Errorf("State = %q, want %q", first.State, "online")
	}

	// The a/b pairing is the entire point of this resource: R1.11.2 states a
	// single supply is adequate to power its frame and the second exists for
	// redundancy, so a fixture that lost one half of a pair would stop
	// exercising the signal this collector was built for.
	t.Run("supplies come in redundant a/b pairs per frame", func(t *testing.T) {
		perFrame := map[string]int{}
		for _, ps := range supplies {
			frame, _, found := strings.Cut(strings.TrimPrefix(ps.Location, "powerSupply_"), "PS")
			if !found {
				t.Errorf("Location %q does not match the documented powerSupply_F<f>PS<a|b> grammar", ps.Location)
				continue
			}
			perFrame[frame]++
		}
		if len(perFrame) != 4 {
			t.Errorf("supplies span %d frames, want 4", len(perFrame))
		}
		for frame, n := range perFrame {
			if n != 2 {
				t.Errorf("frame %s carries %d supplies, want 2 (a redundant pair)", frame, n)
			}
		}
	})

	t.Run("malformed input yields an error, not a panic", func(t *testing.T) {
		if _, err := parsePowerSupplies([]byte("not json")); err == nil {
			t.Error("parsePowerSupplies(malformed) returned a nil error, want non-nil")
		}
	})

	t.Run("an object rather than the documented array is an error", func(t *testing.T) {
		if _, err := parsePowerSupplies([]byte(`{"location":"powerSupply_F1PSa"}`)); err == nil {
			t.Error("parsePowerSupplies(object) returned a nil error, want non-nil")
		}
	})

	// Every TS4500 is powered by two supplies in its L25/L55 frame, so an
	// empty list is a response that lost its content rather than a library
	// running on none. Accepting it would replace a good cache with nothing.
	t.Run("empty array is an error, not a library with no power supplies", func(t *testing.T) {
		if _, err := parsePowerSupplies([]byte(`[]`)); err == nil {
			t.Error("parsePowerSupplies(empty array) returned a nil error, want non-nil")
		}
	})

	t.Run("empty input yields an error, not a panic", func(t *testing.T) {
		if _, err := parsePowerSupplies(nil); err == nil {
			t.Error("parsePowerSupplies(nil) returned a nil error, want non-nil")
		}
	})

	t.Run("an entry with no location is an error", func(t *testing.T) {
		if _, err := parsePowerSupplies([]byte(`[{"state":"online"}]`)); err == nil {
			t.Error("parsePowerSupplies(no location) returned a nil error, want non-nil")
		}
	})

	// Fail closed, in the pure step: two entries at one location would send
	// two metrics sharing a descriptor AND a label set, and Registry.Gather
	// rejects the whole scrape when that happens, taking out every other
	// collector's metrics with it (CONTRIBUTING.md, "Common Pitfalls").
	t.Run("duplicate locations are rejected rather than reaching Collect", func(t *testing.T) {
		dup := `[{"location":"powerSupply_F1PSa","state":"online"},{"location":"powerSupply_F1PSa","state":"online"}]`
		if _, err := parsePowerSupplies([]byte(dup)); err == nil {
			t.Error("parsePowerSupplies(duplicate location) returned a nil error, want non-nil")
		}
	})
}

// TestPowerSuppliesCollector_Describe locks the descriptor count at exactly 2
// (the stateset and the freshness gauge) so a future edit that silently adds
// or drops a metric is caught here rather than downstream in docs-check or a
// dashboard. This endpoint reports a location and a health state and nothing
// else, so 2 is the whole surface, not a subset someone forgot to extend.
func TestPowerSuppliesCollector_Describe(t *testing.T) {
	log := logger.NewTextLogger("error")
	c := NewPowerSuppliesCollector(log, NewClient("http://example.invalid", time.Second), time.Hour)

	ch := make(chan *prometheus.Desc, 20)
	c.Describe(ch)
	close(ch)

	count := 0
	for range ch {
		count++
	}
	if count != 2 {
		t.Fatalf("Describe sent %d descriptors, want 2", count)
	}
}

// TestPowerSuppliesCollector_Collect pins the exact exposition text of the
// stateset against the fixture. refresh is driven directly rather than through
// Start, so the assertion is deterministic and carries no sleep: Start's own
// scheduling is what the lifecycle tests below cover.
//
// All three documented states are emitted per supply, exactly one carrying 1,
// which is what keeps the severity classification in the alerting rules rather
// than in the value.
func TestPowerSuppliesCollector_Collect(t *testing.T) {
	_, c := powerSuppliesFixtureServer(t)
	c.refresh(context.Background())

	expected := `
# HELP tapelibrary_power_supply_state Operational state of the power supply, as a stateset: 1 on the active state and 0 on every other known state.
# TYPE tapelibrary_power_supply_state gauge
tapelibrary_power_supply_state{location="powerSupply_F11PSa",state="failed"} 0
tapelibrary_power_supply_state{location="powerSupply_F11PSa",state="online"} 1
tapelibrary_power_supply_state{location="powerSupply_F11PSa",state="unknown"} 0
tapelibrary_power_supply_state{location="powerSupply_F11PSb",state="failed"} 0
tapelibrary_power_supply_state{location="powerSupply_F11PSb",state="online"} 1
tapelibrary_power_supply_state{location="powerSupply_F11PSb",state="unknown"} 0
tapelibrary_power_supply_state{location="powerSupply_F12PSa",state="failed"} 0
tapelibrary_power_supply_state{location="powerSupply_F12PSa",state="online"} 1
tapelibrary_power_supply_state{location="powerSupply_F12PSa",state="unknown"} 0
tapelibrary_power_supply_state{location="powerSupply_F12PSb",state="failed"} 0
tapelibrary_power_supply_state{location="powerSupply_F12PSb",state="online"} 1
tapelibrary_power_supply_state{location="powerSupply_F12PSb",state="unknown"} 0
tapelibrary_power_supply_state{location="powerSupply_F1PSa",state="failed"} 0
tapelibrary_power_supply_state{location="powerSupply_F1PSa",state="online"} 1
tapelibrary_power_supply_state{location="powerSupply_F1PSa",state="unknown"} 0
tapelibrary_power_supply_state{location="powerSupply_F1PSb",state="failed"} 0
tapelibrary_power_supply_state{location="powerSupply_F1PSb",state="online"} 1
tapelibrary_power_supply_state{location="powerSupply_F1PSb",state="unknown"} 0
tapelibrary_power_supply_state{location="powerSupply_F2PSa",state="failed"} 0
tapelibrary_power_supply_state{location="powerSupply_F2PSa",state="online"} 1
tapelibrary_power_supply_state{location="powerSupply_F2PSa",state="unknown"} 0
tapelibrary_power_supply_state{location="powerSupply_F2PSb",state="failed"} 0
tapelibrary_power_supply_state{location="powerSupply_F2PSb",state="online"} 1
tapelibrary_power_supply_state{location="powerSupply_F2PSb",state="unknown"} 0
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected), "tapelibrary_power_supply_state"); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}

	// 24 cached metrics (8 supplies x 3 documented states) plus the freshness
	// gauge Collect always appends. Unlike the frames and drives fixtures,
	// this one is the library's full complement rather than a trim, so 25 is
	// also the real per-library figure recorded against this collector's
	// cardinality budget in docs/exporter-journal.md.
	reg := prometheus.NewRegistry()
	if err := reg.Register(c); err != nil {
		t.Fatalf("Register: %v", err)
	}
	count, err := testutil.GatherAndCount(reg)
	if err != nil {
		t.Fatalf("GatherAndCount: %v", err)
	}
	if count != 25 {
		t.Fatalf("GatherAndCount = %d, want 25 (8 supplies x 3 states + freshness)", count)
	}
}

// TestPowerSuppliesCollector_FailedSupply covers the state the critical alert
// reads. The 2026-07-28 capture shows a wholly healthy library, so the fixture
// cannot exercise this path and a synthetic body must: without it, the one
// transition this collector exists to detect would be untested.
func TestPowerSuppliesCollector_FailedSupply(t *testing.T) {
	c := powerSuppliesServing(t, `[{"location":"powerSupply_F1PSa","state":"failed"},{"location":"powerSupply_F1PSb","state":"online"}]`)
	c.refresh(context.Background())

	expected := `
# HELP tapelibrary_power_supply_state Operational state of the power supply, as a stateset: 1 on the active state and 0 on every other known state.
# TYPE tapelibrary_power_supply_state gauge
tapelibrary_power_supply_state{location="powerSupply_F1PSa",state="failed"} 1
tapelibrary_power_supply_state{location="powerSupply_F1PSa",state="online"} 0
tapelibrary_power_supply_state{location="powerSupply_F1PSa",state="unknown"} 0
tapelibrary_power_supply_state{location="powerSupply_F1PSb",state="failed"} 0
tapelibrary_power_supply_state{location="powerSupply_F1PSb",state="online"} 1
tapelibrary_power_supply_state{location="powerSupply_F1PSb",state="unknown"} 0
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected), "tapelibrary_power_supply_state"); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}
}

// TestPowerSuppliesCollector_UndocumentedStateIsStillEmitted covers the
// manual's own incompleteness: it names states in prose that appear in none of
// its tables. A state outside the documented set must surface as its own
// series at 1, rather than leaving all three documented series at 0 and making
// the supply look stateless — which would also silently satisfy the
// PowerSupplyNotOnline rule's `== 0` without anyone being able to say why.
func TestPowerSuppliesCollector_UndocumentedStateIsStillEmitted(t *testing.T) {
	c := powerSuppliesServing(t, `[{"location":"powerSupply_F1PSa","state":"someUndocumentedState"}]`)
	c.refresh(context.Background())

	expected := `
# HELP tapelibrary_power_supply_state Operational state of the power supply, as a stateset: 1 on the active state and 0 on every other known state.
# TYPE tapelibrary_power_supply_state gauge
tapelibrary_power_supply_state{location="powerSupply_F1PSa",state="failed"} 0
tapelibrary_power_supply_state{location="powerSupply_F1PSa",state="online"} 0
tapelibrary_power_supply_state{location="powerSupply_F1PSa",state="someUndocumentedState"} 1
tapelibrary_power_supply_state{location="powerSupply_F1PSa",state="unknown"} 0
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected), "tapelibrary_power_supply_state"); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}
}

// TestPowerSuppliesCollector_DoneClosesOnCancel verifies the Done() channel
// closes when the context passed to Start is cancelled. This is the mechanism
// main.go's shutdown seam relies on (see registry.Wait after
// web.ListenAndServe).
func TestPowerSuppliesCollector_DoneClosesOnCancel(t *testing.T) {
	log := logger.NewTextLogger("error")
	c := NewPowerSuppliesCollector(log, NewClient("http://example.invalid", time.Second), time.Hour)
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

// TestPowerSuppliesCollector_CollectServesCacheWithoutIO proves Collect never
// calls the library: after Start's own immediate refresh completes, a long
// interval (1 hour) guarantees the ticker cannot fire again during this test,
// so any further request the server receives could only come from Collect
// itself calling out, which the design forbids.
func TestPowerSuppliesCollector_CollectServesCacheWithoutIO(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.ServeFile(w, r, "testdata/power_supplies.json")
	}))
	defer srv.Close()

	log := logger.NewTextLogger("error")
	c := NewPowerSuppliesCollector(log, NewClient(srv.URL, time.Second), time.Hour)
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

// TestPowerSuppliesCollector_ErrorHandling drives a refresh against a library
// that only ever fails. No cache was ever filled, so the scrape must carry
// exactly the freshness gauge, and must neither panic nor emit a partial
// stateset.
func TestPowerSuppliesCollector_ErrorHandling(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	log := logger.NewTextLogger("error")
	c := NewPowerSuppliesCollector(log, NewClient(srv.URL, time.Second), time.Hour)
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

// TestPowerSuppliesCollector_ErrorKeepsPreviousCache scripts the backend to
// succeed once, then fail on every later call, and drives at least one more
// refresh via a short interval. The cache from the successful first refresh
// must survive the later failure (fail-open, per refresh's doc comment) rather
// than being cleared or replaced with nothing.
//
// The stakes are higher here than on most collectors: a cleared cache would
// drop tapelibrary_power_supply_state{state="online"} entirely, and an
// absent series does not satisfy PowerSupplyNotOnline's `== 0`, so a library
// that became unreachable would silently stop being watched for lost
// redundancy rather than alerting.
func TestPowerSuppliesCollector_ErrorKeepsPreviousCache(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			http.ServeFile(w, r, "testdata/power_supplies.json")
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	log := logger.NewTextLogger("error")
	c := NewPowerSuppliesCollector(log, NewClient(srv.URL, time.Second), 20*time.Millisecond)
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
	// A surviving cache emits 25 metrics: the 24 cached from the successful
	// first refresh plus the freshness gauge. A wrongly-cleared cache would
	// emit only 1 (the gauge alone), which "> 0" cannot tell apart.
	if count != 25 {
		t.Fatalf("GatherAndCount = %d, want 25: the previous cache must survive a later refresh error", count)
	}
}

// TestPowerSuppliesCollector_StatusTrackerSuccessOnFirstScrape covers the
// startup window before Start's first refresh has completed (Start is
// deliberately never called here). Collect must still emit exactly the
// freshness gauge, valued 0 (not a zero time.Time's large-negative Unix()),
// and StatusTracker must still report this collector as successful: "Collect
// ran and returned data" and "the data is fresh" are different questions.
func TestPowerSuppliesCollector_StatusTrackerSuccessOnFirstScrape(t *testing.T) {
	log := logger.NewTextLogger("error")
	c := NewPowerSuppliesCollector(log, NewClient("http://example.invalid", time.Second), time.Hour)
	// Do NOT call Start(): cache is empty, no refresh has ever run.

	expected := `
# HELP tapelibrary_power_supplies_last_refresh_timestamp_seconds Unix time of the last successful power supplies refresh. Alert if time() - this > 2 x the collector's configured interval.
# TYPE tapelibrary_power_supplies_last_refresh_timestamp_seconds gauge
tapelibrary_power_supplies_last_refresh_timestamp_seconds 0
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected)); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}

	tracker := NewStatusTracker(log)
	tracker.Add("power_supplies", c)

	expectedSuccess := `
# HELP ` + statusTrackerSuccessMetric + ` Whether the last scrape of the collector succeeded (1=success, 0=failure)
# TYPE ` + statusTrackerSuccessMetric + ` gauge
` + statusTrackerSuccessMetric + `{collector="power_supplies"} 1
`
	if err := testutil.CollectAndCompare(tracker, strings.NewReader(expectedSuccess), statusTrackerSuccessMetric); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}
}
