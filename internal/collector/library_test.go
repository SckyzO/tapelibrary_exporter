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

// libraryFixtureServer serves testdata/library.json on every request and
// returns a collector already pointed at it. Nothing is started: the caller
// decides whether to drive refresh directly (deterministic) or via Start
// (which is what the lifecycle tests below exercise).
func libraryFixtureServer(t *testing.T) (*httptest.Server, *LibraryCollector) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "testdata/library.json")
	}))
	t.Cleanup(srv.Close)

	log := logger.NewTextLogger("error")
	return srv, NewLibraryCollector(log, NewClient(srv.URL, time.Second), time.Hour)
}

// TestParseLibrary exercises parseLibrary (piece 2, the pure parser) with a
// static byte fixture: no HTTP, no collector, no logger, no goroutine
// involved.
func TestParseLibrary(t *testing.T) {
	data, err := os.ReadFile("testdata/library.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	stats, err := parseLibrary(data)
	if err != nil {
		t.Fatalf("parseLibrary: %v", err)
	}

	if stats.Name != "library1" {
		t.Errorf("Name = %q, want %q", stats.Name, "library1")
	}
	if stats.Status != "driveDegraded" {
		t.Errorf("Status = %q, want %q", stats.Status, "driveDegraded")
	}
	if stats.TotalCapacity != 10732 {
		t.Errorf("TotalCapacity = %v, want 10732", stats.TotalCapacity)
	}
	if stats.LicensedCapacity != 9844 {
		t.Errorf("LicensedCapacity = %v, want 9844", stats.LicensedCapacity)
	}
	if stats.TotalCartridges != 9749 {
		t.Errorf("TotalCartridges = %v, want 9749", stats.TotalCartridges)
	}
	if stats.AssignedCartridges != 9672 {
		t.Errorf("AssignedCartridges = %v, want 9672", stats.AssignedCartridges)
	}
	if stats.Firmware != "1.11.0.2-C00" {
		t.Errorf("Firmware = %q, want %q", stats.Firmware, "1.11.0.2-C00")
	}
	if stats.SN != "SN00000001" {
		t.Errorf("SN = %q, want %q", stats.SN, "SN00000001")
	}
	if stats.CapacityUtilThresh != 99 {
		t.Errorf("CapacityUtilThresh = %v, want 99", stats.CapacityUtilThresh)
	}
	if stats.DualAccessorUtilThresh != 98 {
		t.Errorf("DualAccessorUtilThresh = %v, want 98", stats.DualAccessorUtilThresh)
	}

	t.Run("malformed input yields an error, not a panic", func(t *testing.T) {
		if _, err := parseLibrary([]byte("not json")); err == nil {
			t.Error("parseLibrary(malformed) returned a nil error, want non-nil")
		}
	})

	t.Run("an object rather than the documented array is an error", func(t *testing.T) {
		if _, err := parseLibrary([]byte(`{"name":"library1"}`)); err == nil {
			t.Error("parseLibrary(object) returned a nil error, want non-nil")
		}
	})

	// An empty array must not decode to a zero-valued struct: a library
	// reporting 0 capacity, 0 cartridges and an empty status would be
	// indistinguishable from a real library at zero, and would silently
	// overwrite a good cache with nonsense.
	t.Run("empty array is an error, not a zero-valued library", func(t *testing.T) {
		if _, err := parseLibrary([]byte(`[]`)); err == nil {
			t.Error("parseLibrary(empty array) returned a nil error, want non-nil")
		}
	})

	t.Run("empty input yields an error, not a panic", func(t *testing.T) {
		if _, err := parseLibrary(nil); err == nil {
			t.Error("parseLibrary(nil) returned a nil error, want non-nil")
		}
	})
}

// TestLibraryCollector_Describe locks the descriptor count at exactly 9 (the
// stateset, six capacity/threshold gauges, the identity info metric, and the
// freshness gauge) so a future edit that silently adds or drops a metric is
// caught here rather than downstream in docs-check or a dashboard.
func TestLibraryCollector_Describe(t *testing.T) {
	log := logger.NewTextLogger("error")
	c := NewLibraryCollector(log, NewClient("http://example.invalid", time.Second), time.Hour)

	ch := make(chan *prometheus.Desc, 20)
	c.Describe(ch)
	close(ch)

	count := 0
	for range ch {
		count++
	}
	if count != 9 {
		t.Fatalf("Describe sent %d descriptors, want 9", count)
	}
}

// TestLibraryCollector_Collect pins the exact exposition text of every
// business metric against the fixture. refresh is driven directly rather
// than through Start, so the assertion is deterministic and carries no
// sleep: Start's own scheduling is what the lifecycle tests below cover.
//
// The stateset is the important half: exactly one of the 17 documented
// statuses carries 1 and every other carries 0, which is what keeps the
// severity classification in the alerting rules rather than in the value.
func TestLibraryCollector_Collect(t *testing.T) {
	_, c := libraryFixtureServer(t)
	c.refresh(context.Background())

	expected := `
# HELP tapelibrary_library_capacity_util_threshold_ratio Capacity utilization threshold configured on the library, as a ratio from 0 to 1.
# TYPE tapelibrary_library_capacity_util_threshold_ratio gauge
tapelibrary_library_capacity_util_threshold_ratio 0.99
# HELP tapelibrary_library_cartridges_assigned Number of cartridges currently assigned to a logical library.
# TYPE tapelibrary_library_cartridges_assigned gauge
tapelibrary_library_cartridges_assigned 9672
# HELP tapelibrary_library_cartridges_present Number of cartridges currently present in the library.
# TYPE tapelibrary_library_cartridges_present gauge
tapelibrary_library_cartridges_present 9749
# HELP tapelibrary_library_dual_accessor_util_threshold_ratio Dual-accessor utilization threshold configured on the library, as a ratio from 0 to 1.
# TYPE tapelibrary_library_dual_accessor_util_threshold_ratio gauge
tapelibrary_library_dual_accessor_util_threshold_ratio 0.98
# HELP tapelibrary_library_info Library identity, always 1. Identity strings live here rather than on a measurement series, so a firmware upgrade changes this series alone instead of breaking the continuity of every other.
# TYPE tapelibrary_library_info gauge
tapelibrary_library_info{firmware="1.11.0.2-C00",name="library1",serial="SN00000001"} 1
# HELP tapelibrary_library_slots_capacity Total number of cartridge slots the library physically holds.
# TYPE tapelibrary_library_slots_capacity gauge
tapelibrary_library_slots_capacity 10732
# HELP tapelibrary_library_slots_licensed Number of cartridge slots the library is currently licensed to use.
# TYPE tapelibrary_library_slots_licensed gauge
tapelibrary_library_slots_licensed 9844
# HELP tapelibrary_library_state Operational status of the library, as a stateset: 1 on the active status and 0 on every other known status.
# TYPE tapelibrary_library_state gauge
tapelibrary_library_state{state="accessorDegraded"} 0
tapelibrary_library_state{state="accessorsUnavailable"} 0
tapelibrary_library_state{state="calibrationRequired"} 0
tapelibrary_library_state{state="cartridgeDegraded"} 0
tapelibrary_library_state{state="doorOpen"} 0
tapelibrary_library_state{state="doorOpenWhileNotAllowed"} 0
tapelibrary_library_state{state="driveDegraded"} 1
tapelibrary_library_state{state="inServiceMode"} 0
tapelibrary_library_state{state="initializing"} 0
tapelibrary_library_state{state="nodeCardDegraded"} 0
tapelibrary_library_state{state="notConfigured"} 0
tapelibrary_library_state{state="online"} 0
tapelibrary_library_state{state="paused"} 0
tapelibrary_library_state{state="pausing"} 0
tapelibrary_library_state{state="scanningInventory"} 0
tapelibrary_library_state{state="unknown"} 0
tapelibrary_library_state{state="updating"} 0
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected),
		"tapelibrary_library_capacity_util_threshold_ratio",
		"tapelibrary_library_cartridges_assigned",
		"tapelibrary_library_cartridges_present",
		"tapelibrary_library_dual_accessor_util_threshold_ratio",
		"tapelibrary_library_info",
		"tapelibrary_library_slots_capacity",
		"tapelibrary_library_slots_licensed",
		"tapelibrary_library_state",
	); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}

	// 24 cached metrics (17 stateset + 6 gauges + 1 info) plus the freshness
	// gauge Collect always appends. This is the observed cardinality recorded
	// in docs/exporter-journal.md's budget for this collector.
	reg := prometheus.NewRegistry()
	if err := reg.Register(c); err != nil {
		t.Fatalf("Register: %v", err)
	}
	count, err := testutil.GatherAndCount(reg)
	if err != nil {
		t.Fatalf("GatherAndCount: %v", err)
	}
	if count != 25 {
		t.Fatalf("GatherAndCount = %d, want 25 (17 stateset + 6 gauges + 1 info + freshness)", count)
	}
}

// TestLibraryCollector_UndocumentedStateIsStillEmitted covers the manual's
// own incompleteness: it names states in prose that appear in none of its
// tables. A status outside the documented set must surface as its own series
// at 1, rather than leaving all 17 documented series at 0 and making the
// library look stateless. The extra series pushes the scrape to 26.
func TestLibraryCollector_UndocumentedStateIsStillEmitted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"name":"library1","status":"someUndocumentedState","sn":"SN00000001","firmware":"1.11.0.2-C00"}]`))
	}))
	defer srv.Close()

	log := logger.NewTextLogger("error")
	c := NewLibraryCollector(log, NewClient(srv.URL, time.Second), time.Hour)
	c.refresh(context.Background())

	expected := `
# HELP tapelibrary_library_state Operational status of the library, as a stateset: 1 on the active status and 0 on every other known status.
# TYPE tapelibrary_library_state gauge
tapelibrary_library_state{state="accessorDegraded"} 0
tapelibrary_library_state{state="accessorsUnavailable"} 0
tapelibrary_library_state{state="calibrationRequired"} 0
tapelibrary_library_state{state="cartridgeDegraded"} 0
tapelibrary_library_state{state="doorOpen"} 0
tapelibrary_library_state{state="doorOpenWhileNotAllowed"} 0
tapelibrary_library_state{state="driveDegraded"} 0
tapelibrary_library_state{state="inServiceMode"} 0
tapelibrary_library_state{state="initializing"} 0
tapelibrary_library_state{state="nodeCardDegraded"} 0
tapelibrary_library_state{state="notConfigured"} 0
tapelibrary_library_state{state="online"} 0
tapelibrary_library_state{state="paused"} 0
tapelibrary_library_state{state="pausing"} 0
tapelibrary_library_state{state="scanningInventory"} 0
tapelibrary_library_state{state="someUndocumentedState"} 1
tapelibrary_library_state{state="unknown"} 0
tapelibrary_library_state{state="updating"} 0
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected), "tapelibrary_library_state"); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}
}

// TestLibraryCollector_DoneClosesOnCancel verifies the Done() channel closes
// when the context passed to Start is cancelled. This is the mechanism
// main.go's shutdown seam relies on (see registry.Wait after
// web.ListenAndServe).
func TestLibraryCollector_DoneClosesOnCancel(t *testing.T) {
	log := logger.NewTextLogger("error")
	c := NewLibraryCollector(log, NewClient("http://example.invalid", time.Second), time.Hour)
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

// TestLibraryCollector_CollectServesCacheWithoutIO proves Collect never calls
// the library: after Start's own immediate refresh completes, a long interval
// (1 hour) guarantees the ticker cannot fire again during this test, so any
// further request the server receives could only come from Collect itself
// calling out, which the design forbids.
func TestLibraryCollector_CollectServesCacheWithoutIO(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.ServeFile(w, r, "testdata/library.json")
	}))
	defer srv.Close()

	log := logger.NewTextLogger("error")
	c := NewLibraryCollector(log, NewClient(srv.URL, time.Second), time.Hour)
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

// TestLibraryCollector_ErrorHandling drives a refresh against a library that
// only ever fails. No cache was ever filled, so the scrape must carry exactly
// the freshness gauge, and must neither panic nor emit a partial stateset.
func TestLibraryCollector_ErrorHandling(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	log := logger.NewTextLogger("error")
	c := NewLibraryCollector(log, NewClient(srv.URL, time.Second), time.Hour)
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

// TestLibraryCollector_ErrorKeepsPreviousCache scripts the backend to succeed
// once, then fail on every later call, and drives at least one more refresh
// via a short interval. The cache from the successful first refresh must
// survive the later failure (fail-open, per refresh's doc comment) rather
// than being cleared or replaced with nothing.
func TestLibraryCollector_ErrorKeepsPreviousCache(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			http.ServeFile(w, r, "testdata/library.json")
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	log := logger.NewTextLogger("error")
	c := NewLibraryCollector(log, NewClient(srv.URL, time.Second), 20*time.Millisecond)
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

// TestLibraryCollector_StatusTrackerSuccessOnFirstScrape covers the startup
// window before Start's first refresh has completed (Start is deliberately
// never called here). Collect must still emit exactly the freshness gauge,
// valued 0 (not a zero time.Time's large-negative Unix()), and StatusTracker
// must still report this collector as successful: "Collect ran and returned
// data" and "the data is fresh" are different questions.
func TestLibraryCollector_StatusTrackerSuccessOnFirstScrape(t *testing.T) {
	log := logger.NewTextLogger("error")
	c := NewLibraryCollector(log, NewClient("http://example.invalid", time.Second), time.Hour)
	// Do NOT call Start(): cache is empty, no refresh has ever run.

	expected := `
# HELP tapelibrary_library_last_refresh_timestamp_seconds Unix time of the last successful library refresh. Alert if time() - this > 2 x the collector's configured interval.
# TYPE tapelibrary_library_last_refresh_timestamp_seconds gauge
tapelibrary_library_last_refresh_timestamp_seconds 0
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected)); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}

	tracker := NewStatusTracker(log)
	tracker.Add("library", c)

	expectedSuccess := `
# HELP ` + statusTrackerSuccessMetric + ` Whether the last scrape of the collector succeeded (1=success, 0=failure)
# TYPE ` + statusTrackerSuccessMetric + ` gauge
` + statusTrackerSuccessMetric + `{collector="library"} 1
`
	if err := testutil.CollectAndCompare(tracker, strings.NewReader(expectedSuccess), statusTrackerSuccessMetric); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}
}
