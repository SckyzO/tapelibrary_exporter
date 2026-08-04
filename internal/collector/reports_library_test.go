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

// reportsLibraryFixtureServer serves testdata/reports_library.json on every
// request and returns a collector already pointed at it. Nothing is started:
// the caller decides whether to drive refresh directly (deterministic) or via
// Start (which is what the lifecycle tests below exercise).
func reportsLibraryFixtureServer(t *testing.T) (*httptest.Server, *ReportsLibraryCollector) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "testdata/reports_library.json")
	}))
	t.Cleanup(srv.Close)

	log := logger.NewTextLogger("error")
	return srv, NewReportsLibraryCollector(log, NewClient(srv.URL, time.Second), time.Hour)
}

// TestParseReportsLibrary exercises parseReportsLibrary (piece 2, the pure
// parser) with a static byte fixture: no HTTP, no collector, no logger, no
// goroutine involved.
//
// The fixture carries four hourly windows, and the assertions below pin the
// newest one (09:05:38). Getting 08:05:45's 39 mounts here would mean the
// selection picked a window that is an hour stale.
func TestParseReportsLibrary(t *testing.T) {
	data, err := os.ReadFile("testdata/reports_library.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	stats, err := parseReportsLibrary(data)
	if err != nil {
		t.Fatalf("parseReportsLibrary: %v", err)
	}

	if got, want := stats.at.Unix(), int64(1785229538); got != want {
		t.Errorf("at.Unix() = %d, want %d (the 09:05:38 window)", got, want)
	}
	if stats.window.Duration != 3600 {
		t.Errorf("Duration = %v, want 3600", stats.window.Duration)
	}
	if stats.window.Mounts != 36 {
		t.Errorf("Mounts = %v, want 36", stats.window.Mounts)
	}
	if stats.window.Imports != 0 {
		t.Errorf("Imports = %v, want 0", stats.window.Imports)
	}
	if stats.window.Exports != 0 {
		t.Errorf("Exports = %v, want 0", stats.window.Exports)
	}
	if stats.window.Moves != 72 {
		t.Errorf("Moves = %v, want 72", stats.window.Moves)
	}
	if stats.window.DataReadByHosts != 130566 {
		t.Errorf("DataReadByHosts = %v, want 130566", stats.window.DataReadByHosts)
	}
	if stats.window.DataWrittenByHosts != 0 {
		t.Errorf("DataWrittenByHosts = %v, want 0", stats.window.DataWrittenByHosts)
	}
	if stats.window.DataWrittenToCartridges != 61754 {
		t.Errorf("DataWrittenToCartridges = %v, want 61754", stats.window.DataWrittenToCartridges)
	}
	if stats.window.TemperatureAverage == nil || *stats.window.TemperatureAverage != 24.4 {
		t.Errorf("TemperatureAverage = %v, want 24.4", stats.window.TemperatureAverage)
	}
	if stats.window.TemperatureMin == nil || *stats.window.TemperatureMin != 21.0 {
		t.Errorf("TemperatureMin = %v, want 21", stats.window.TemperatureMin)
	}
	if stats.window.TemperatureMax == nil || *stats.window.TemperatureMax != 28.0 {
		t.Errorf("TemperatureMax = %v, want 28", stats.window.TemperatureMax)
	}
	if stats.window.HumidityAverage == nil || *stats.window.HumidityAverage != 33.0 {
		t.Errorf("HumidityAverage = %v, want 33", stats.window.HumidityAverage)
	}
	if stats.window.HumidityMin == nil || *stats.window.HumidityMin != 27.0 {
		t.Errorf("HumidityMin = %v, want 27", stats.window.HumidityMin)
	}
	if stats.window.HumidityMax == nil || *stats.window.HumidityMax != 40.4 {
		t.Errorf("HumidityMax = %v, want 40.4", stats.window.HumidityMax)
	}

	// R1.11.2 documents no ordering for this endpoint. The capture happens to
	// arrive newest first, so a parser that took entries[0] would pass every
	// assertion above while being one firmware release away from reporting
	// week-old activity as current.
	t.Run("the newest window wins regardless of array order", func(t *testing.T) {
		shuffled := []byte(`[
			{"time":"2026-07-28T06:05:37+0000","duration":3600,"mounts":29},
			{"time":"2026-07-28T09:05:38+0000","duration":3600,"mounts":36},
			{"time":"2026-07-28T07:05:26+0000","duration":3600,"mounts":34}
		]`)
		stats, err := parseReportsLibrary(shuffled)
		if err != nil {
			t.Fatalf("parseReportsLibrary: %v", err)
		}
		if stats.window.Mounts != 36 {
			t.Errorf("Mounts = %v, want 36: selection must be by timestamp, not by position", stats.window.Mounts)
		}
	})

	// The wire format's zone offset carries no colon, so a parser reaching
	// for time.RFC3339 rejects every window the library ever sends.
	t.Run("the offset carries no colon and must still parse", func(t *testing.T) {
		stats, err := parseReportsLibrary([]byte(`[{"time":"2026-07-28T09:05:38+0000","mounts":36}]`))
		if err != nil {
			t.Fatalf("parseReportsLibrary: %v", err)
		}
		if got, want := stats.at.Unix(), int64(1785229538); got != want {
			t.Errorf("at.Unix() = %d, want %d", got, want)
		}
	})

	// An unorderable window is skipped rather than compared as a zero time,
	// which would sort it last and let a good window keep winning by
	// accident. Here the only parseable window is also the oldest, so a
	// parser that did not skip would return the 09:05 entry.
	t.Run("a window with an unparseable timestamp is skipped, not ranked", func(t *testing.T) {
		mixed := []byte(`[
			{"time":"not a timestamp","mounts":99},
			{"time":"2026-07-28T09:05:38","mounts":98},
			{"time":"2026-07-28T06:05:37+0000","mounts":29}
		]`)
		stats, err := parseReportsLibrary(mixed)
		if err != nil {
			t.Fatalf("parseReportsLibrary: %v", err)
		}
		if stats.window.Mounts != 29 {
			t.Errorf("Mounts = %v, want 29: only the parseable window may be selected", stats.window.Mounts)
		}
	})

	// Rejected rather than served with a fabricated timestamp: refresh then
	// keeps the previous good cache, and Collect's unconditional window
	// timestamp stays honest.
	t.Run("no parseable timestamp anywhere is an error", func(t *testing.T) {
		if _, err := parseReportsLibrary([]byte(`[{"time":"","mounts":36},{"mounts":39}]`)); err == nil {
			t.Error("parseReportsLibrary(no parseable timestamp) returned a nil error, want non-nil")
		}
	})

	t.Run("malformed input yields an error, not a panic", func(t *testing.T) {
		if _, err := parseReportsLibrary([]byte("not json")); err == nil {
			t.Error("parseReportsLibrary(malformed) returned a nil error, want non-nil")
		}
	})

	t.Run("an object rather than the documented array is an error", func(t *testing.T) {
		if _, err := parseReportsLibrary([]byte(`{"mounts":36}`)); err == nil {
			t.Error("parseReportsLibrary(object) returned a nil error, want non-nil")
		}
	})

	// An empty array must not decode to a zero-valued window: a library
	// reporting 0 mounts, 0 moves and 0 °C would be indistinguishable from a
	// real idle library, and would silently overwrite a good cache.
	t.Run("empty array is an error, not a zero-valued window", func(t *testing.T) {
		if _, err := parseReportsLibrary([]byte(`[]`)); err == nil {
			t.Error("parseReportsLibrary(empty array) returned a nil error, want non-nil")
		}
	})

	t.Run("empty input yields an error, not a panic", func(t *testing.T) {
		if _, err := parseReportsLibrary(nil); err == nil {
			t.Error("parseReportsLibrary(nil) returned a nil error, want non-nil")
		}
	})

	// Absent environmental readings must survive parsing as nil rather than
	// decoding to 0: refresh emits no series for them, and the operating
	// envelope rules would otherwise page on a freezing, bone dry library.
	t.Run("null environmental readings parse as nil, not zero", func(t *testing.T) {
		stats, err := parseReportsLibrary([]byte(`[{"time":"2026-07-28T09:05:38+0000","mounts":36,"temperatureAverage":null,"humidityMax":null}]`))
		if err != nil {
			t.Fatalf("parseReportsLibrary: %v", err)
		}
		if stats.window.TemperatureAverage != nil {
			t.Errorf("TemperatureAverage = %v, want nil", *stats.window.TemperatureAverage)
		}
		if stats.window.HumidityMax != nil {
			t.Errorf("HumidityMax = %v, want nil", *stats.window.HumidityMax)
		}
		if stats.window.Mounts != 36 {
			t.Errorf("Mounts = %v, want 36: a null sensor must not discard the activity figures", stats.window.Mounts)
		}
	})
}

// TestReportsLibraryCollector_Describe locks the descriptor count at exactly
// 16 (seven activity gauges, six environmental, the window timestamp and
// duration, and the freshness gauge) so a future edit that silently adds or
// drops a metric is caught here rather than downstream in docs-check or a
// dashboard.
func TestReportsLibraryCollector_Describe(t *testing.T) {
	log := logger.NewTextLogger("error")
	c := NewReportsLibraryCollector(log, NewClient("http://example.invalid", time.Second), time.Hour)

	ch := make(chan *prometheus.Desc, 30)
	c.Describe(ch)
	close(ch)

	count := 0
	for range ch {
		count++
	}
	if count != 16 {
		t.Fatalf("Describe sent %d descriptors, want 16", count)
	}
}

// TestReportsLibraryCollector_Collect pins the exact exposition text of every
// business metric against the fixture. refresh is driven directly rather than
// through Start, so the assertion is deterministic and carries no sleep:
// Start's own scheduling is what the lifecycle tests below cover.
//
// Every value here belongs to the fixture's NEWEST window (09:05:38): 36
// mounts rather than 08:05's 39, and 130566 MB read rather than 346607.
func TestReportsLibraryCollector_Collect(t *testing.T) {
	_, c := reportsLibraryFixtureServer(t)
	c.refresh(context.Background())

	expected := `
# HELP tapelibrary_library_report_exports Number of cartridges removed from the library during the reporting window, counted once the cartridge has physically reached the I/O station.
# TYPE tapelibrary_library_report_exports gauge
tapelibrary_library_report_exports 0
# HELP tapelibrary_library_report_humidity_average_ratio Average relative humidity across all drives over the reporting window, as a ratio from 0 to 1 (the API reports a 0-100 percentage). Absent, never zero, when no drive reported a reading.
# TYPE tapelibrary_library_report_humidity_average_ratio gauge
tapelibrary_library_report_humidity_average_ratio 0.33
# HELP tapelibrary_library_report_humidity_max_ratio Highest relative humidity reported by any drive over the reporting window, as a ratio from 0 to 1. Absent, never zero, when no drive reported a reading.
# TYPE tapelibrary_library_report_humidity_max_ratio gauge
tapelibrary_library_report_humidity_max_ratio 0.40399999999999997
# HELP tapelibrary_library_report_humidity_min_ratio Lowest relative humidity reported by any drive over the reporting window, as a ratio from 0 to 1. Absent, never zero, when no drive reported a reading.
# TYPE tapelibrary_library_report_humidity_min_ratio gauge
tapelibrary_library_report_humidity_min_ratio 0.27
# HELP tapelibrary_library_report_imports Number of cartridges added to the library during the reporting window. R1.11.2 counts an import only once the host has issued the SCSI move media command or the cartridge was manually assigned to a logical library.
# TYPE tapelibrary_library_report_imports gauge
tapelibrary_library_report_imports 0
# HELP tapelibrary_library_report_mounts Number of cartridges mounted into a drive during the reporting window. A per-window figure, not a cumulative counter: the next window restarts from zero.
# TYPE tapelibrary_library_report_mounts gauge
tapelibrary_library_report_mounts 36
# HELP tapelibrary_library_report_moves Number of times a cartridge was moved from one location to another during the reporting window, host-initiated and library-initiated alike. Each move is one get plus one put by the gripper, and the figure includes mounts, demounts, imports and exports. Cartridges shuffled aside to reach a deeper tier are not counted.
# TYPE tapelibrary_library_report_moves gauge
tapelibrary_library_report_moves 72
# HELP tapelibrary_library_report_read_by_hosts_bytes Bytes read from cartridges by all drives during the reporting window. Converted from the API's megabytes, read decimally (1 MB = 1e6 bytes) to match how this exporter already converts the cartridge lifetime counters.
# TYPE tapelibrary_library_report_read_by_hosts_bytes gauge
tapelibrary_library_report_read_by_hosts_bytes 1.30566e+11
# HELP tapelibrary_library_report_temperature_average_celsius Average temperature in Celsius across all drives over the reporting window. Measured inside the library at the drives, so it reads above the ambient figure R1.11.2's operating envelope is written against. Absent, never zero, when no drive reported a reading.
# TYPE tapelibrary_library_report_temperature_average_celsius gauge
tapelibrary_library_report_temperature_average_celsius 24.4
# HELP tapelibrary_library_report_temperature_max_celsius Highest temperature in Celsius reported by any drive over the reporting window. Absent, never zero, when no drive reported a reading.
# TYPE tapelibrary_library_report_temperature_max_celsius gauge
tapelibrary_library_report_temperature_max_celsius 28
# HELP tapelibrary_library_report_temperature_min_celsius Lowest temperature in Celsius reported by any drive over the reporting window. Absent, never zero, when no drive reported a reading.
# TYPE tapelibrary_library_report_temperature_min_celsius gauge
tapelibrary_library_report_temperature_min_celsius 21
# HELP tapelibrary_library_report_window_duration_seconds Number of seconds the reporting window covers, as the library reports it. 3600 on every window in the reference capture. Exposed so that a window covering less than a full hour is visible rather than assumed away: its activity figures would be proportionally low through no fault of the library.
# TYPE tapelibrary_library_report_window_duration_seconds gauge
tapelibrary_library_report_window_duration_seconds 3600
# HELP tapelibrary_library_report_window_timestamp_seconds Unix time the library stamped on the reporting window these metrics describe. The library publishes one window per completed hour, so alert if time() - this exceeds a few hours: every other metric in this family would otherwise keep serving a stale window's values indefinitely, looking healthy.
# TYPE tapelibrary_library_report_window_timestamp_seconds gauge
tapelibrary_library_report_window_timestamp_seconds 1.785229538e+09
# HELP tapelibrary_library_report_written_by_hosts_bytes Bytes written to cartridges by all drives during the reporting window, measured before compression. Divide by tapelibrary_library_report_written_to_cartridges_bytes for the window's average compression ratio. Converted from the API's decimal megabytes.
# TYPE tapelibrary_library_report_written_by_hosts_bytes gauge
tapelibrary_library_report_written_by_hosts_bytes 0
# HELP tapelibrary_library_report_written_to_cartridges_bytes Bytes actually written onto the media by all drives during the reporting window, after compression. Converted from the API's decimal megabytes.
# TYPE tapelibrary_library_report_written_to_cartridges_bytes gauge
tapelibrary_library_report_written_to_cartridges_bytes 6.1754e+10
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected),
		"tapelibrary_library_report_exports",
		"tapelibrary_library_report_humidity_average_ratio",
		"tapelibrary_library_report_humidity_max_ratio",
		"tapelibrary_library_report_humidity_min_ratio",
		"tapelibrary_library_report_imports",
		"tapelibrary_library_report_mounts",
		"tapelibrary_library_report_moves",
		"tapelibrary_library_report_read_by_hosts_bytes",
		"tapelibrary_library_report_temperature_average_celsius",
		"tapelibrary_library_report_temperature_max_celsius",
		"tapelibrary_library_report_temperature_min_celsius",
		"tapelibrary_library_report_window_duration_seconds",
		"tapelibrary_library_report_window_timestamp_seconds",
		"tapelibrary_library_report_written_by_hosts_bytes",
		"tapelibrary_library_report_written_to_cartridges_bytes",
	); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}

	// 15 cached metrics (7 activity + 6 environmental + window timestamp +
	// duration) plus the freshness gauge Collect always appends. This is the
	// observed cardinality recorded in docs/exporter-journal.md's budget for
	// this collector.
	reg := prometheus.NewRegistry()
	if err := reg.Register(c); err != nil {
		t.Fatalf("Register: %v", err)
	}
	count, err := testutil.GatherAndCount(reg)
	if err != nil {
		t.Fatalf("GatherAndCount: %v", err)
	}
	if count != 16 {
		t.Fatalf("GatherAndCount = %d, want 16 (7 activity + 6 environmental + window timestamp + duration + freshness)", count)
	}
}

// TestReportsLibraryCollector_AbsentSensorsEmitNoSeries covers the null
// branch: a library whose drives reported no temperature or humidity must
// emit the nine always-present series and nothing in their place. A 0 °C /
// 0% RH would sit outside R1.11.2's operating envelope in both directions and
// page on a library that is merely quiet about its sensors.
func TestReportsLibraryCollector_AbsentSensorsEmitNoSeries(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"time":"2026-07-28T09:05:38+0000","duration":3600,"mounts":36,"moves":72,
			"temperatureAverage":null,"temperatureMin":null,"temperatureMax":null,
			"humidityAverage":null,"humidityMin":null,"humidityMax":null}]`))
	}))
	defer srv.Close()

	log := logger.NewTextLogger("error")
	c := NewReportsLibraryCollector(log, NewClient(srv.URL, time.Second), time.Hour)
	c.refresh(context.Background())

	for _, name := range []string{
		"tapelibrary_library_report_temperature_average_celsius",
		"tapelibrary_library_report_temperature_min_celsius",
		"tapelibrary_library_report_temperature_max_celsius",
		"tapelibrary_library_report_humidity_average_ratio",
		"tapelibrary_library_report_humidity_min_ratio",
		"tapelibrary_library_report_humidity_max_ratio",
	} {
		if got := testutil.CollectAndCount(c, name); got != 0 {
			t.Errorf("%s produced %d series, want 0: an absent reading must emit nothing, never a zero", name, got)
		}
	}

	// The activity figures and the window's own identity are unaffected, so
	// the scrape is still nine cached series plus the freshness gauge.
	reg := prometheus.NewRegistry()
	if err := reg.Register(c); err != nil {
		t.Fatalf("Register: %v", err)
	}
	count, err := testutil.GatherAndCount(reg)
	if err != nil {
		t.Fatalf("GatherAndCount: %v", err)
	}
	if count != 10 {
		t.Fatalf("GatherAndCount = %d, want 10 (7 activity + window timestamp + duration + freshness)", count)
	}
}

// TestReportsLibraryCollector_DoneClosesOnCancel verifies the Done() channel
// closes when the context passed to Start is cancelled. This is the mechanism
// main.go's shutdown seam relies on (see registry.Wait after
// web.ListenAndServe).
func TestReportsLibraryCollector_DoneClosesOnCancel(t *testing.T) {
	log := logger.NewTextLogger("error")
	c := NewReportsLibraryCollector(log, NewClient("http://example.invalid", time.Second), time.Hour)
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

// TestReportsLibraryCollector_CollectServesCacheWithoutIO proves Collect never
// calls the library: after Start's own immediate refresh completes, a long
// interval (1 hour) guarantees the ticker cannot fire again during this test,
// so any further request the server receives could only come from Collect
// itself calling out, which the design forbids.
func TestReportsLibraryCollector_CollectServesCacheWithoutIO(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.ServeFile(w, r, "testdata/reports_library.json")
	}))
	defer srv.Close()

	log := logger.NewTextLogger("error")
	c := NewReportsLibraryCollector(log, NewClient(srv.URL, time.Second), time.Hour)
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

// TestReportsLibraryCollector_ErrorHandling drives a refresh against a library
// that only ever fails. No cache was ever filled, so the scrape must carry
// exactly the freshness gauge, and must neither panic nor emit a partial
// window.
func TestReportsLibraryCollector_ErrorHandling(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	log := logger.NewTextLogger("error")
	c := NewReportsLibraryCollector(log, NewClient(srv.URL, time.Second), time.Hour)
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

// TestReportsLibraryCollector_ErrorKeepsPreviousCache scripts the backend to
// succeed once, then fail on every later call, and drives at least one more
// refresh via a short interval. The cache from the successful first refresh
// must survive the later failure (fail-open, per refresh's doc comment)
// rather than being cleared or replaced with nothing.
func TestReportsLibraryCollector_ErrorKeepsPreviousCache(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			http.ServeFile(w, r, "testdata/reports_library.json")
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	log := logger.NewTextLogger("error")
	c := NewReportsLibraryCollector(log, NewClient(srv.URL, time.Second), 20*time.Millisecond)
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
	// A surviving cache emits 16 metrics: the 15 cached from the successful
	// first refresh plus the freshness gauge. A wrongly-cleared cache would
	// emit only 1 (the gauge alone), which "> 0" cannot tell apart.
	if count != 16 {
		t.Fatalf("GatherAndCount = %d, want 16: the previous cache must survive a later refresh error", count)
	}
}

// TestReportsLibraryCollector_StatusTrackerSuccessOnFirstScrape covers the
// startup window before Start's first refresh has completed (Start is
// deliberately never called here). Collect must still emit exactly the
// freshness gauge, valued 0 (not a zero time.Time's large-negative Unix()),
// and StatusTracker must still report this collector as successful: "Collect
// ran and returned data" and "the data is fresh" are different questions.
func TestReportsLibraryCollector_StatusTrackerSuccessOnFirstScrape(t *testing.T) {
	log := logger.NewTextLogger("error")
	c := NewReportsLibraryCollector(log, NewClient("http://example.invalid", time.Second), time.Hour)
	// Do NOT call Start(): cache is empty, no refresh has ever run.

	expected := `
# HELP tapelibrary_library_report_last_refresh_timestamp_seconds Unix time of the last successful reports/library refresh. Alert if time() - this > 2 x the collector's configured interval. Named for this collector's metric subsystem rather than its registered name, so a subsystem sweep finds the freshness of the data it is reading. Distinct from tapelibrary_library_report_window_timestamp_seconds, which ages even while this one stays current.
# TYPE tapelibrary_library_report_last_refresh_timestamp_seconds gauge
tapelibrary_library_report_last_refresh_timestamp_seconds 0
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected)); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}

	tracker := NewStatusTracker(log)
	tracker.Add("reports_library", c)

	expectedSuccess := `
# HELP ` + statusTrackerSuccessMetric + ` Whether the last scrape of the collector succeeded (1=success, 0=failure)
# TYPE ` + statusTrackerSuccessMetric + ` gauge
` + statusTrackerSuccessMetric + `{collector="reports_library"} 1
`
	if err := testutil.CollectAndCompare(tracker, strings.NewReader(expectedSuccess), statusTrackerSuccessMetric); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}
}
