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

// logicalLibrariesMetricNames is every family this collector emits except the
// freshness gauge, whose value is a wall-clock timestamp and therefore cannot be
// pinned in an exposition block. Listed in the order Registry.Gather sorts them
// (by family name), which is the order the expected blocks below use.
var logicalLibrariesMetricNames = []string{
	"tapelibrary_logical_library_cartridges",
	"tapelibrary_logical_library_drives",
	"tapelibrary_logical_library_info",
	"tapelibrary_logical_library_virtual_io_slots",
	"tapelibrary_logical_library_virtual_slots",
}

// logicalLibrariesFixtureServer serves testdata/logical_libraries.json on every
// request and returns a collector already pointed at it. Nothing is started: the
// caller decides whether to drive refresh directly (deterministic) or via Start
// (which is what the lifecycle tests below exercise).
func logicalLibrariesFixtureServer(t *testing.T) (*httptest.Server, *LogicalLibrariesCollector) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "testdata/logical_libraries.json")
	}))
	t.Cleanup(srv.Close)

	log := logger.NewTextLogger("error")
	return srv, NewLogicalLibrariesCollector(log, NewClient(srv.URL, time.Second), time.Hour)
}

// logicalLibrariesServing returns a collector fed by a server that answers every
// request with body. Used by the branch tests below, which need input shapes the
// real capture does not contain and which must therefore not be invented inside
// testdata/logical_libraries.json (that fixture stays faithful to the 2026-07-28
// capture, in which both partitions are 3592 media with encryption disabled).
func logicalLibrariesServing(t *testing.T, body string) *LogicalLibrariesCollector {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	log := logger.NewTextLogger("error")
	return NewLogicalLibrariesCollector(log, NewClient(srv.URL, time.Second), time.Hour)
}

// TestParseLogicalLibraries exercises parseLogicalLibraries (piece 2, the pure
// parser) with a static byte fixture: no HTTP, no collector, no logger, no
// goroutine involved.
func TestParseLogicalLibraries(t *testing.T) {
	data, err := os.ReadFile("testdata/logical_libraries.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	libraries, err := parseLogicalLibraries(data)
	if err != nil {
		t.Fatalf("parseLogicalLibraries: %v", err)
	}

	// Two partitions, which is the library's FULL complement rather than a
	// trim: unlike fc_ports and drives, the 2026-07-28 capture of this
	// endpoint is complete at 373 bytes.
	if len(libraries) != 2 {
		t.Fatalf("len(libraries) = %d, want 2", len(libraries))
	}

	first := libraries[0]
	if first.Name != "Library-5" {
		t.Errorf("Name = %q, want %q", first.Name, "Library-5")
	}
	if first.MediaType != "3592" {
		t.Errorf("MediaType = %q, want %q", first.MediaType, "3592")
	}
	if first.Drives != 20 {
		t.Errorf("Drives = %d, want 20", first.Drives)
	}
	if first.VirtualSlots != 5000 {
		t.Errorf("VirtualSlots = %d, want 5000", first.VirtualSlots)
	}
	if first.VirtualIOSlots != 255 {
		t.Errorf("VirtualIOSlots = %d, want 255", first.VirtualIOSlots)
	}
	if first.Cartridges != 4819 {
		t.Errorf("Cartridges = %d, want 4819", first.Cartridges)
	}
	if first.EncryptionMethod != "none" {
		t.Errorf("EncryptionMethod = %q, want %q", first.EncryptionMethod, "none")
	}

	// The join key to the drives collector, which reads the same string from
	// /v1/drives into the same logical_library label. If a partition name
	// ever stops being decoded, every query pairing a drive with its
	// partition's slot count silently matches nothing.
	t.Run("every partition carries the name the drives collector joins on", func(t *testing.T) {
		for _, l := range libraries {
			if l.Name == "" {
				t.Error("a partition has an empty Name: the drives join key is missing")
			}
		}
	})

	// R1.11.2 documents the name as unique per library, and the capture
	// agrees. Verified here rather than inherited from another collector, per
	// the key rule in docs/exporter-journal.md — the same check node_cards
	// failed (an accessor carries two cards at one location) and fc_ports
	// passed.
	t.Run("the two partitions carry distinct names", func(t *testing.T) {
		if libraries[0].Name == libraries[1].Name {
			t.Fatalf("both partitions are named %q: name does not key this endpoint", libraries[0].Name)
		}
	})

	// The capture is 3592-only with encryption off, so the other documented
	// media type and a real encryption mode are exercised here rather than
	// invented inside the fixture.
	t.Run("the LTO media type and a real encryption mode decode", func(t *testing.T) {
		body := `[{"name":"myLib1","mediaType":"LTO","drives":2,"virtualSlots":730,` +
			`"virtualIOSlots":255,"cartridges":84,"encryptionMethod":"libraryManagedBarcode"}]`
		got, err := parseLogicalLibraries([]byte(body))
		if err != nil {
			t.Fatalf("parseLogicalLibraries(LTO): %v", err)
		}
		if got[0].MediaType != "LTO" {
			t.Errorf("MediaType = %q, want %q", got[0].MediaType, "LTO")
		}
		if got[0].EncryptionMethod != "libraryManagedBarcode" {
			t.Errorf("EncryptionMethod = %q, want %q", got[0].EncryptionMethod, "libraryManagedBarcode")
		}
	})

	t.Run("malformed input yields an error, not a panic", func(t *testing.T) {
		if _, err := parseLogicalLibraries([]byte("not json")); err == nil {
			t.Error("parseLogicalLibraries(malformed) returned a nil error, want non-nil")
		}
	})

	t.Run("an object rather than the documented array is an error", func(t *testing.T) {
		if _, err := parseLogicalLibraries([]byte(`{"name":"Library-5"}`)); err == nil {
			t.Error("parseLogicalLibraries(object) returned a nil error, want non-nil")
		}
	})

	// A TS4500 with no partitions serves no host application at all, and
	// reports that as library.status = notConfigured on its own endpoint,
	// which the library collector already emits. So [] here is treated as a
	// response that lost its content rather than as a de-partitioned library:
	// accepting it would replace a good cache with nothing.
	t.Run("empty array is an error, not a library with no partitions", func(t *testing.T) {
		if _, err := parseLogicalLibraries([]byte(`[]`)); err == nil {
			t.Error("parseLogicalLibraries(empty array) returned a nil error, want non-nil")
		}
	})

	t.Run("empty input yields an error, not a panic", func(t *testing.T) {
		if _, err := parseLogicalLibraries(nil); err == nil {
			t.Error("parseLogicalLibraries(nil) returned a nil error, want non-nil")
		}
	})

	t.Run("an entry with no name is an error", func(t *testing.T) {
		if _, err := parseLogicalLibraries([]byte(`[{"mediaType":"3592"}]`)); err == nil {
			t.Error("parseLogicalLibraries(no name) returned a nil error, want non-nil")
		}
	})

	// Fail closed, in the pure step: two entries with one name would send two
	// metrics sharing a descriptor AND a label set, and Registry.Gather
	// rejects the whole scrape when that happens, taking out every other
	// collector's metrics with it (CONTRIBUTING.md, "Common Pitfalls").
	t.Run("duplicate names are rejected rather than reaching Collect", func(t *testing.T) {
		dup := `[{"name":"Library-5","mediaType":"3592"},{"name":"Library-5","mediaType":"3592"}]`
		if _, err := parseLogicalLibraries([]byte(dup)); err == nil {
			t.Error("parseLogicalLibraries(duplicate name) returned a nil error, want non-nil")
		}
	})
}

// TestLogicalLibrariesCollector_Describe locks the descriptor count at exactly 6
// (four capacity gauges, the info series and the freshness gauge) so a future
// edit that silently adds or drops a metric is caught here rather than
// downstream in docs-check or a dashboard.
func TestLogicalLibrariesCollector_Describe(t *testing.T) {
	log := logger.NewTextLogger("error")
	c := NewLogicalLibrariesCollector(log, NewClient("http://example.invalid", time.Second), time.Hour)

	ch := make(chan *prometheus.Desc, 20)
	c.Describe(ch)
	close(ch)

	count := 0
	for range ch {
		count++
	}
	if count != 6 {
		t.Fatalf("Describe sent %d descriptors, want 6", count)
	}
}

// TestLogicalLibrariesCollector_Collect pins the exact exposition text of every
// family against the fixture. refresh is driven directly rather than through
// Start, so the assertion is deterministic and carries no sleep: Start's own
// scheduling is what the lifecycle tests below cover.
//
// There is no stateset here — this is the first collector in the exporter whose
// endpoint reports no state at all — so every family is a plain per-partition
// gauge and the count is exactly 5 per partition with no conditional series.
func TestLogicalLibrariesCollector_Collect(t *testing.T) {
	_, c := logicalLibrariesFixtureServer(t)
	c.refresh(context.Background())

	expected := `
# HELP tapelibrary_logical_library_cartridges Number of cartridges currently assigned to the logical library. Divide by tapelibrary_logical_library_virtual_slots for the partition's saturation: at 1 it can accept no further imports, however much free space the physical library still reports.
# TYPE tapelibrary_logical_library_cartridges gauge
tapelibrary_logical_library_cartridges{logical_library="Library-5"} 4819
tapelibrary_logical_library_cartridges{logical_library="Library-6"} 4853
# HELP tapelibrary_logical_library_drives Number of drives assigned to the logical library and reported to its host application as data transfer element addresses.
# TYPE tapelibrary_logical_library_drives gauge
tapelibrary_logical_library_drives{logical_library="Library-5"} 20
tapelibrary_logical_library_drives{logical_library="Library-6"} 20
# HELP tapelibrary_logical_library_info Logical library identity and configuration, always 1. Identity strings live here rather than on a measurement series, so reconfiguring a partition's encryption changes this series alone instead of breaking the continuity of its capacity counts.
# TYPE tapelibrary_logical_library_info gauge
tapelibrary_logical_library_info{encryption_method="none",logical_library="Library-5",media_type="3592"} 1
tapelibrary_logical_library_info{encryption_method="none",logical_library="Library-6",media_type="3592"} 1
# HELP tapelibrary_logical_library_virtual_io_slots Number of virtual I/O slots the logical library reports to its host application as import/export element addresses. Capped at 255 by the library, and never fewer than the physical I/O slots behind it.
# TYPE tapelibrary_logical_library_virtual_io_slots gauge
tapelibrary_logical_library_virtual_io_slots{logical_library="Library-5"} 255
tapelibrary_logical_library_virtual_io_slots{logical_library="Library-6"} 255
# HELP tapelibrary_logical_library_virtual_slots Number of virtual storage slots the logical library reports to its host application as storage element addresses. The library will not let this fall below the assigned cartridge count, so it is the ceiling that count saturates against.
# TYPE tapelibrary_logical_library_virtual_slots gauge
tapelibrary_logical_library_virtual_slots{logical_library="Library-5"} 5000
tapelibrary_logical_library_virtual_slots{logical_library="Library-6"} 5000
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected), logicalLibrariesMetricNames...); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}

	// 10 cached metrics (2 partitions x 5) plus the freshness gauge Collect
	// always appends. Like power_supplies, node_cards and io_stations — and
	// unlike fc_ports and drives — this fixture is the library's FULL
	// complement rather than a trim, so 11 is also the real per-library figure
	// with no projection step.
	reg := prometheus.NewRegistry()
	if err := reg.Register(c); err != nil {
		t.Fatalf("Register: %v", err)
	}
	count, err := testutil.GatherAndCount(reg)
	if err != nil {
		t.Fatalf("GatherAndCount: %v", err)
	}
	if count != 11 {
		t.Fatalf("GatherAndCount = %d, want 11 (2 partitions x 5 + freshness)", count)
	}
}

// TestLogicalLibrariesCollector_SaturationIsComputable is the one test here that
// exists for an alerting rule rather than for the collector's own shape.
// LogicalLibraryNearlyFull divides the cartridge count by the virtual slot
// count, and PromQL can only do that if both series carry an IDENTICAL label
// set — a single extra label on either side makes the division match nothing and
// the rule silently never fires.
//
// The 2026-07-28 capture already sits at 0.964 and 0.971, so this ratio is not a
// hypothetical: it is the number that decided the rule's thresholds (see
// docs/exporter-journal.md).
func TestLogicalLibrariesCollector_SaturationIsComputable(t *testing.T) {
	_, c := logicalLibrariesFixtureServer(t)
	c.refresh(context.Background())

	reg := prometheus.NewRegistry()
	if err := reg.Register(c); err != nil {
		t.Fatalf("Register: %v", err)
	}
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}

	labelsOf := func(name string) map[string][]string {
		out := make(map[string][]string)
		for _, fam := range families {
			if fam.GetName() != name {
				continue
			}
			for _, m := range fam.GetMetric() {
				var partition string
				var keys []string
				for _, lp := range m.GetLabel() {
					keys = append(keys, lp.GetName())
					if lp.GetName() == "logical_library" {
						partition = lp.GetValue()
					}
				}
				out[partition] = keys
			}
		}
		return out
	}

	num := labelsOf("tapelibrary_logical_library_cartridges")
	den := labelsOf("tapelibrary_logical_library_virtual_slots")

	if len(num) == 0 || len(den) == 0 {
		t.Fatalf("cartridges=%d series, virtual_slots=%d series: both are needed for the ratio", len(num), len(den))
	}
	for partition, numKeys := range num {
		denKeys, ok := den[partition]
		if !ok {
			t.Fatalf("partition %q has a cartridge count but no virtual slot count: the ratio matches nothing", partition)
		}
		if strings.Join(numKeys, ",") != strings.Join(denKeys, ",") {
			t.Fatalf("partition %q: cartridges labelled %v but virtual_slots labelled %v; PromQL division needs an identical label set",
				partition, numKeys, denKeys)
		}
	}
}

// TestLogicalLibrariesCollector_ZeroCountsAreEmitted covers the one place this
// collector deliberately departs from the exporter's "absent, not zero" habit.
//
// Elsewhere a null means the hardware cannot report (accessors.temperature), or
// that a thing does not exist (a frame's absent rear door), and emitting a 0
// would assert a reading nobody took. Here R1.11.2 types all four fields as
// plain numbers: a partition with zero cartridges is empty and a partition with
// zero drives assigned cannot mount anything, and both are real, alarming
// readings that must reach a dashboard rather than vanish from it.
func TestLogicalLibrariesCollector_ZeroCountsAreEmitted(t *testing.T) {
	c := logicalLibrariesServing(t, `[{"name":"Library-9","mediaType":"3592","drives":0,`+
		`"virtualSlots":0,"virtualIOSlots":0,"cartridges":0,"encryptionMethod":"none"}]`)
	c.refresh(context.Background())

	expected := `
# HELP tapelibrary_logical_library_cartridges Number of cartridges currently assigned to the logical library. Divide by tapelibrary_logical_library_virtual_slots for the partition's saturation: at 1 it can accept no further imports, however much free space the physical library still reports.
# TYPE tapelibrary_logical_library_cartridges gauge
tapelibrary_logical_library_cartridges{logical_library="Library-9"} 0
# HELP tapelibrary_logical_library_drives Number of drives assigned to the logical library and reported to its host application as data transfer element addresses.
# TYPE tapelibrary_logical_library_drives gauge
tapelibrary_logical_library_drives{logical_library="Library-9"} 0
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected),
		"tapelibrary_logical_library_cartridges", "tapelibrary_logical_library_drives"); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}
}

// TestLogicalLibrariesCollector_DoneClosesOnCancel verifies the Done() channel
// closes when the context passed to Start is cancelled. This is the mechanism
// main.go's shutdown seam relies on (see registry.Wait after
// web.ListenAndServe).
func TestLogicalLibrariesCollector_DoneClosesOnCancel(t *testing.T) {
	log := logger.NewTextLogger("error")
	c := NewLogicalLibrariesCollector(log, NewClient("http://example.invalid", time.Second), time.Hour)
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

// TestLogicalLibrariesCollector_CollectServesCacheWithoutIO proves Collect never
// calls the library: after Start's own immediate refresh completes, a long
// interval (1 hour) guarantees the ticker cannot fire again during this test, so
// any further request the server receives could only come from Collect itself
// calling out, which the design forbids.
func TestLogicalLibrariesCollector_CollectServesCacheWithoutIO(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.ServeFile(w, r, "testdata/logical_libraries.json")
	}))
	defer srv.Close()

	log := logger.NewTextLogger("error")
	c := NewLogicalLibrariesCollector(log, NewClient(srv.URL, time.Second), time.Hour)
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

// TestLogicalLibrariesCollector_ErrorHandling drives a refresh against a library
// that only ever fails. No cache was ever filled, so the scrape must carry
// exactly the freshness gauge, and must neither panic nor emit a partial set of
// capacity gauges.
func TestLogicalLibrariesCollector_ErrorHandling(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	log := logger.NewTextLogger("error")
	c := NewLogicalLibrariesCollector(log, NewClient(srv.URL, time.Second), time.Hour)
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

// TestLogicalLibrariesCollector_ErrorKeepsPreviousCache scripts the backend to
// succeed once, then fail on every later call, and drives at least one more
// refresh via a short interval. The cache from the successful first refresh must
// survive the later failure (fail-open, per refresh's doc comment) rather than
// being cleared or replaced with nothing.
//
// A cleared cache here would drop both sides of LogicalLibraryNearlyFull's
// division at once, and an absent series cannot exceed a threshold, so a library
// that became unreachable would silently stop being watched for a partition
// filling up rather than alerting.
func TestLogicalLibrariesCollector_ErrorKeepsPreviousCache(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			http.ServeFile(w, r, "testdata/logical_libraries.json")
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	log := logger.NewTextLogger("error")
	c := NewLogicalLibrariesCollector(log, NewClient(srv.URL, time.Second), 20*time.Millisecond)
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
	// A surviving cache emits 11 metrics: the 10 cached from the successful
	// first refresh plus the freshness gauge. A wrongly-cleared cache would
	// emit only 1 (the gauge alone), which "> 0" cannot tell apart.
	if count != 11 {
		t.Fatalf("GatherAndCount = %d, want 11: the previous cache must survive a later refresh error", count)
	}
}

// TestLogicalLibrariesCollector_StatusTrackerSuccessOnFirstScrape covers the
// startup window before Start's first refresh has completed (Start is
// deliberately never called here). Collect must still emit exactly the freshness
// gauge, valued 0 (not a zero time.Time's large-negative Unix()), and
// StatusTracker must still report this collector as successful: "Collect ran and
// returned data" and "the data is fresh" are different questions.
func TestLogicalLibrariesCollector_StatusTrackerSuccessOnFirstScrape(t *testing.T) {
	log := logger.NewTextLogger("error")
	c := NewLogicalLibrariesCollector(log, NewClient("http://example.invalid", time.Second), time.Hour)
	// Do NOT call Start(): cache is empty, no refresh has ever run.

	expected := `
# HELP tapelibrary_logical_libraries_last_refresh_timestamp_seconds Unix time of the last successful logical libraries refresh. Alert if time() - this > 2 x the collector's configured interval.
# TYPE tapelibrary_logical_libraries_last_refresh_timestamp_seconds gauge
tapelibrary_logical_libraries_last_refresh_timestamp_seconds 0
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected)); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}

	tracker := NewStatusTracker(log)
	tracker.Add("logical_libraries", c)

	expectedSuccess := `
# HELP ` + statusTrackerSuccessMetric + ` Whether the last scrape of the collector succeeded (1=success, 0=failure)
# TYPE ` + statusTrackerSuccessMetric + ` gauge
` + statusTrackerSuccessMetric + `{collector="logical_libraries"} 1
`
	if err := testutil.CollectAndCompare(tracker, strings.NewReader(expectedSuccess), statusTrackerSuccessMetric); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}
}
