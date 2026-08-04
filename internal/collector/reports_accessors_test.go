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

// reportsAccessorsFixtureServer serves testdata/reports_accessors.json on
// every request and returns a collector already pointed at it. Nothing is
// started: the caller decides whether to drive refresh directly
// (deterministic) or via Start (which is what the lifecycle tests below
// exercise).
func reportsAccessorsFixtureServer(t *testing.T) (*httptest.Server, *ReportsAccessorsCollector) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "testdata/reports_accessors.json")
	}))
	t.Cleanup(srv.Close)

	log := logger.NewTextLogger("error")
	return srv, NewReportsAccessorsCollector(log, NewClient(srv.URL, time.Second), 15*time.Minute)
}

// TestParseReportsAccessors exercises parseReportsAccessors (piece 2, the
// pure parser) with a static byte fixture: no HTTP, no collector, no logger,
// no goroutine involved.
//
// The fixture carries both accessors x four hourly windows, and the
// assertions below pin each accessor's newest one (09:05). Getting
// accessor_Aa's 08:05 figures (56 pivots, 38 gets on gripper 1) would mean
// the selection picked a window that is an hour stale.
func TestParseReportsAccessors(t *testing.T) {
	data, err := os.ReadFile("testdata/reports_accessors.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	accessors, err := parseReportsAccessors(data)
	if err != nil {
		t.Fatalf("parseReportsAccessors: %v", err)
	}

	if got, want := len(accessors), 2; got != want {
		t.Fatalf("len(accessors) = %d, want %d: one selected window per accessor", got, want)
	}

	// Sorted by location, which is what parseReportsAccessors promises so the
	// cached slice is deterministic for a given response.
	if got, want := accessors[0].window.Location, "accessor_Aa"; got != want {
		t.Errorf("accessors[0].Location = %q, want %q: the result must be sorted by location", got, want)
	}
	if got, want := accessors[1].window.Location, "accessor_Ab"; got != want {
		t.Errorf("accessors[1].Location = %q, want %q: the result must be sorted by location", got, want)
	}

	byLocation := make(map[string]reportsAccessorStats, len(accessors))
	for _, a := range accessors {
		byLocation[a.window.Location] = a
	}

	// Both accessors' newest window is the same 09:05 hour in this capture,
	// but that is a property of the data, not something the parser may
	// assume: the per-accessor selection is what the mixed-timestamp sub-test
	// below pins.
	for loc, a := range byLocation {
		if got, want := a.at.Unix(), int64(1785229500); got != want {
			t.Errorf("%s: at.Unix() = %d, want %d (the 09:05 window)", loc, got, want)
		}
		if a.window.Duration != 3600 {
			t.Errorf("%s: Duration = %v, want 3600", loc, a.window.Duration)
		}
		// Zero in every window of this capture, and a genuine reading rather
		// than a gap: this is exactly why the activity fields are plain
		// float64 and not pointers.
		if a.window.BarCodeScans != 0 {
			t.Errorf("%s: BarCodeScans = %v, want 0", loc, a.window.BarCodeScans)
		}
		// Every accessor on this fleet reports null for all six
		// environmental fields: the hardware carries no such sensor, which
		// /v1/accessors reports the same way.
		if a.window.TemperatureAverage != nil {
			t.Errorf("%s: TemperatureAverage = %v, want nil", loc, *a.window.TemperatureAverage)
		}
		if a.window.HumidityAverage != nil {
			t.Errorf("%s: HumidityAverage = %v, want nil", loc, *a.window.HumidityAverage)
		}
	}

	aa := byLocation["accessor_Aa"].window
	if aa.Pivots != 62 {
		t.Errorf("accessor_Aa: Pivots = %v, want 62 (the 09:05 window, not 08:05's 56)", aa.Pivots)
	}
	if aa.TravelX != 184 {
		t.Errorf("accessor_Aa: TravelX = %v, want 184", aa.TravelX)
	}
	if aa.TravelY != 54 {
		t.Errorf("accessor_Aa: TravelY = %v, want 54", aa.TravelY)
	}
	if aa.GetsGripper1 != 32 {
		t.Errorf("accessor_Aa: GetsGripper1 = %v, want 32 (the 09:05 window, not 08:05's 38)", aa.GetsGripper1)
	}
	if aa.PutsGripper2 != 33 {
		t.Errorf("accessor_Aa: PutsGripper2 = %v, want 33", aa.PutsGripper2)
	}

	ab := byLocation["accessor_Ab"].window
	if ab.Pivots != 42 {
		t.Errorf("accessor_Ab: Pivots = %v, want 42", ab.Pivots)
	}
	if ab.GetsGripper2 != 16 {
		t.Errorf("accessor_Ab: GetsGripper2 = %v, want 16", ab.GetsGripper2)
	}

	// R1.11.2 documents no ordering for this endpoint. A parser that took the
	// first entry per accessor would pass every assertion above while being
	// one firmware release away from reporting week-old activity as current.
	t.Run("the newest window wins per accessor regardless of array order", func(t *testing.T) {
		shuffled := []byte(`[
			{"location":"accessor_Aa","time":"2026-07-28T06:05:00+0000","pivots":35},
			{"location":"accessor_Aa","time":"2026-07-28T09:05:00+0000","pivots":62},
			{"location":"accessor_Aa","time":"2026-07-28T07:05:00+0000","pivots":38}
		]`)
		accessors, err := parseReportsAccessors(shuffled)
		if err != nil {
			t.Fatalf("parseReportsAccessors: %v", err)
		}
		if len(accessors) != 1 {
			t.Fatalf("len(accessors) = %d, want 1", len(accessors))
		}
		if accessors[0].window.Pivots != 62 {
			t.Errorf("Pivots = %v, want 62: selection must be by timestamp, not by position", accessors[0].window.Pivots)
		}
	})

	// The selection is per accessor, not library-wide. On a two-accessor
	// library this matters more than it does on a forty-drive one: an
	// accessor that stopped reporting is half the robotics, and collapsing
	// both onto one library-wide window would serve its sibling's timestamp
	// against its own stale values.
	t.Run("each accessor keeps its own newest window", func(t *testing.T) {
		mixed := []byte(`[
			{"location":"accessor_Aa","time":"2026-07-28T09:05:00+0000","pivots":62},
			{"location":"accessor_Aa","time":"2026-07-28T08:05:00+0000","pivots":56},
			{"location":"accessor_Ab","time":"2026-07-28T06:05:00+0000","pivots":38}
		]`)
		accessors, err := parseReportsAccessors(mixed)
		if err != nil {
			t.Fatalf("parseReportsAccessors: %v", err)
		}
		if len(accessors) != 2 {
			t.Fatalf("len(accessors) = %d, want 2", len(accessors))
		}
		byLoc := map[string]reportsAccessorStats{}
		for _, a := range accessors {
			byLoc[a.window.Location] = a
		}
		if got := byLoc["accessor_Aa"].window.Pivots; got != 62 {
			t.Errorf("accessor_Aa: Pivots = %v, want 62", got)
		}
		if got := byLoc["accessor_Ab"].window.Pivots; got != 38 {
			t.Errorf("accessor_Ab: Pivots = %v, want 38: an accessor whose newest window is older must keep it", got)
		}
		if got, want := byLoc["accessor_Ab"].at.Unix(), int64(1785218700); got != want {
			t.Errorf("accessor_Ab: at.Unix() = %d, want %d (its own 06:05 window)", got, want)
		}
	})

	// The wire format's zone offset carries no colon, so a parser reaching
	// for time.RFC3339 rejects every window the library ever sends.
	t.Run("the offset carries no colon and must still parse", func(t *testing.T) {
		accessors, err := parseReportsAccessors([]byte(`[{"location":"accessor_Aa","time":"2026-07-28T09:05:00+0000","pivots":62}]`))
		if err != nil {
			t.Fatalf("parseReportsAccessors: %v", err)
		}
		if got, want := accessors[0].at.Unix(), int64(1785229500); got != want {
			t.Errorf("at.Unix() = %d, want %d", got, want)
		}
	})

	// One unusable hour out of a week must not discard the other 167 for
	// both accessors, so an entry missing a timestamp or a location is
	// skipped rather than failing the whole response. Here the only parseable
	// window for the accessor is also its oldest.
	t.Run("an entry with an unparseable timestamp is skipped, not ranked", func(t *testing.T) {
		mixed := []byte(`[
			{"location":"accessor_Aa","time":"not a timestamp","pivots":99},
			{"location":"accessor_Aa","time":"2026-07-28T09:05:00","pivots":98},
			{"location":"accessor_Aa","time":"2026-07-28T06:05:00+0000","pivots":35}
		]`)
		accessors, err := parseReportsAccessors(mixed)
		if err != nil {
			t.Fatalf("parseReportsAccessors: %v", err)
		}
		if len(accessors) != 1 {
			t.Fatalf("len(accessors) = %d, want 1", len(accessors))
		}
		if accessors[0].window.Pivots != 35 {
			t.Errorf("Pivots = %v, want 35: only a parseable window may be selected", accessors[0].window.Pivots)
		}
	})

	// A series labelled location="" traces back to no hardware, so the entry
	// is skipped. Its sibling still ships.
	t.Run("an entry with an empty location is skipped, not emitted", func(t *testing.T) {
		mixed := []byte(`[
			{"location":"","time":"2026-07-28T09:05:00+0000","pivots":99},
			{"location":"accessor_Aa","time":"2026-07-28T09:05:00+0000","pivots":62}
		]`)
		accessors, err := parseReportsAccessors(mixed)
		if err != nil {
			t.Fatalf("parseReportsAccessors: %v", err)
		}
		if len(accessors) != 1 {
			t.Fatalf("len(accessors) = %d, want 1: an entry with no location must not become a series", len(accessors))
		}
		if accessors[0].window.Location != "accessor_Aa" {
			t.Errorf("Location = %q, want accessor_Aa", accessors[0].window.Location)
		}
	})

	// Rejected rather than served with a fabricated timestamp: refresh then
	// keeps the previous good cache, and the per-accessor window timestamps
	// stay honest.
	t.Run("nothing selectable anywhere is an error", func(t *testing.T) {
		if _, err := parseReportsAccessors([]byte(`[{"location":"","time":""},{"location":"accessor_Aa","time":"nope"}]`)); err == nil {
			t.Error("parseReportsAccessors(nothing selectable) returned a nil error, want non-nil")
		}
	})

	t.Run("malformed input yields an error, not a panic", func(t *testing.T) {
		if _, err := parseReportsAccessors([]byte("not json")); err == nil {
			t.Error("parseReportsAccessors(malformed) returned a nil error, want non-nil")
		}
	})

	t.Run("an object rather than the documented array is an error", func(t *testing.T) {
		if _, err := parseReportsAccessors([]byte(`{"pivots":62}`)); err == nil {
			t.Error("parseReportsAccessors(object) returned a nil error, want non-nil")
		}
	})

	// An empty array must not decode to no accessors at all: a library with
	// no accessor cannot move a cartridge, and accepting it would silently
	// replace a good cache with nothing.
	t.Run("empty array is an error, not an empty fleet", func(t *testing.T) {
		if _, err := parseReportsAccessors([]byte(`[]`)); err == nil {
			t.Error("parseReportsAccessors(empty array) returned a nil error, want non-nil")
		}
	})

	t.Run("empty input yields an error, not a panic", func(t *testing.T) {
		if _, err := parseReportsAccessors(nil); err == nil {
			t.Error("parseReportsAccessors(nil) returned a nil error, want non-nil")
		}
	})

	// The mirror of the fixture's all-null case: an accessor that DOES carry
	// sensors must decode them, so the six descriptors are not dead weight on
	// hardware other than this fleet's.
	t.Run("present environmental readings parse as values, not nil", func(t *testing.T) {
		accessors, err := parseReportsAccessors([]byte(`[{"location":"accessor_Aa","time":"2026-07-28T09:05:00+0000","pivots":62,"temperatureAverage":24.5,"humidityMax":41.0}]`))
		if err != nil {
			t.Fatalf("parseReportsAccessors: %v", err)
		}
		w := accessors[0].window
		if w.TemperatureAverage == nil || *w.TemperatureAverage != 24.5 {
			t.Errorf("TemperatureAverage = %v, want 24.5", w.TemperatureAverage)
		}
		if w.HumidityMax == nil || *w.HumidityMax != 41.0 {
			t.Errorf("HumidityMax = %v, want 41", w.HumidityMax)
		}
	})
}

// TestReportsAccessorsCollector_Describe locks the descriptor count at
// exactly 14 (five activity gauges, six environmental, the per-accessor
// window timestamp and duration, and the freshness gauge) so a future edit
// that silently adds or drops a metric is caught here rather than downstream
// in docs-check or a dashboard.
func TestReportsAccessorsCollector_Describe(t *testing.T) {
	log := logger.NewTextLogger("error")
	c := NewReportsAccessorsCollector(log, NewClient("http://example.invalid", time.Second), 15*time.Minute)

	ch := make(chan *prometheus.Desc, 30)
	c.Describe(ch)
	close(ch)

	count := 0
	for range ch {
		count++
	}
	if count != 14 {
		t.Fatalf("Describe sent %d descriptors, want 14", count)
	}
}

// TestReportsAccessorsCollector_Collect pins the exact exposition text of
// every business metric against the fixture. refresh is driven directly
// rather than through Start, so the assertion is deterministic and carries
// no sleep: Start's own scheduling is what the lifecycle tests below cover.
//
// Every value here belongs to each accessor's NEWEST window (09:05).
// Registry.Gather sorts metric families by name and, within a family, by
// label value, which is why the gripper and axis families group by the
// LABEL first and the location second: that ordering is deterministic, not
// incidental.
//
// None of the six environmental families appears, because every entry of the
// reference capture reports null for all of them. That absence is asserted
// positively in TestReportsAccessorsCollector_AbsentSensorsEmitNoSeries
// rather than being left to this test's silence.
func TestReportsAccessorsCollector_Collect(t *testing.T) {
	_, c := reportsAccessorsFixtureServer(t)
	c.refresh(context.Background())

	expected := `
# HELP tapelibrary_accessor_report_bar_code_scans Number of bar code scans this accessor performed during the reporting window. A per-window figure, not a cumulative counter. Zero in every window of the reference capture: this fleet scans on inventory rather than on every move, so a window recording scans is an inventory pass. Its lifetime counterpart is tapelibrary_accessor_bar_code_scans_total.
# TYPE tapelibrary_accessor_report_bar_code_scans gauge
tapelibrary_accessor_report_bar_code_scans{location="accessor_Aa"} 0
tapelibrary_accessor_report_bar_code_scans{location="accessor_Ab"} 0
# HELP tapelibrary_accessor_report_gets Number of times this accessor's gripper engaged to retrieve a cartridge during the reporting window. A per-window figure, not a cumulative counter. Compare the two accessors' shares of the library total: a lifetime counter cannot show one of them stopping, which is what AccessorReportShareCollapsed reads. Its lifetime counterpart is tapelibrary_accessor_gets_total.
# TYPE tapelibrary_accessor_report_gets gauge
tapelibrary_accessor_report_gets{gripper="1",location="accessor_Aa"} 32
tapelibrary_accessor_report_gets{gripper="1",location="accessor_Ab"} 24
tapelibrary_accessor_report_gets{gripper="2",location="accessor_Aa"} 32
tapelibrary_accessor_report_gets{gripper="2",location="accessor_Ab"} 16
# HELP tapelibrary_accessor_report_pivots Number of pivots this accessor performed during the reporting window. A per-window figure, not a cumulative counter: the next window restarts from zero. Its lifetime counterpart is tapelibrary_accessor_pivots_total.
# TYPE tapelibrary_accessor_report_pivots gauge
tapelibrary_accessor_report_pivots{location="accessor_Aa"} 62
tapelibrary_accessor_report_pivots{location="accessor_Ab"} 42
# HELP tapelibrary_accessor_report_puts Number of times this accessor's gripper engaged to place a cartridge during the reporting window. A per-window figure, not a cumulative counter. Normally tracks gets closely, since a cartridge retrieved is a cartridge put somewhere. Its lifetime counterpart is tapelibrary_accessor_puts_total.
# TYPE tapelibrary_accessor_report_puts gauge
tapelibrary_accessor_report_puts{gripper="1",location="accessor_Aa"} 32
tapelibrary_accessor_report_puts{gripper="1",location="accessor_Ab"} 24
tapelibrary_accessor_report_puts{gripper="2",location="accessor_Aa"} 33
tapelibrary_accessor_report_puts{gripper="2",location="accessor_Ab"} 16
# HELP tapelibrary_accessor_report_travel_meters Distance in meters this accessor travelled during the reporting window, per axis: x is horizontal, y is vertical. A per-window figure, not a cumulative counter. Its lifetime counterpart is tapelibrary_accessor_travel_meters_total.
# TYPE tapelibrary_accessor_report_travel_meters gauge
tapelibrary_accessor_report_travel_meters{axis="x",location="accessor_Aa"} 184
tapelibrary_accessor_report_travel_meters{axis="x",location="accessor_Ab"} 141
tapelibrary_accessor_report_travel_meters{axis="y",location="accessor_Aa"} 54
tapelibrary_accessor_report_travel_meters{axis="y",location="accessor_Ab"} 24
# HELP tapelibrary_accessor_report_window_duration_seconds Number of seconds this accessor's reporting window covers, as the library reports it. 3600 on every window in the reference capture. Exposed so that a window covering less than a full hour is visible rather than assumed away: its activity figures would be proportionally low through no fault of the accessor.
# TYPE tapelibrary_accessor_report_window_duration_seconds gauge
tapelibrary_accessor_report_window_duration_seconds{location="accessor_Aa"} 3600
tapelibrary_accessor_report_window_duration_seconds{location="accessor_Ab"} 3600
# HELP tapelibrary_accessor_report_window_timestamp_seconds Unix time the library stamped on the reporting window these metrics describe, for this accessor. Per accessor rather than library-wide so that one of the two dropping out of the report is visible: the library publishes one window per completed hour, so alert if time() - this exceeds a few hours. Every other metric in this family would otherwise keep serving a stale window's values indefinitely, looking healthy.
# TYPE tapelibrary_accessor_report_window_timestamp_seconds gauge
tapelibrary_accessor_report_window_timestamp_seconds{location="accessor_Aa"} 1.7852295e+09
tapelibrary_accessor_report_window_timestamp_seconds{location="accessor_Ab"} 1.7852295e+09
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected),
		"tapelibrary_accessor_report_bar_code_scans",
		"tapelibrary_accessor_report_gets",
		"tapelibrary_accessor_report_pivots",
		"tapelibrary_accessor_report_puts",
		"tapelibrary_accessor_report_travel_meters",
		"tapelibrary_accessor_report_window_duration_seconds",
		"tapelibrary_accessor_report_window_timestamp_seconds",
	); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}

	// 20 cached metrics (2 accessors x 10 always-emitted series) plus the
	// freshness gauge Collect always appends. This is the 21 recorded in
	// docs/exporter-journal.md's budget for this collector on sensorless
	// hardware; a fleet whose accessors carry sensors would reach 33.
	reg := prometheus.NewRegistry()
	if err := reg.Register(c); err != nil {
		t.Fatalf("Register: %v", err)
	}
	count, err := testutil.GatherAndCount(reg)
	if err != nil {
		t.Fatalf("GatherAndCount: %v", err)
	}
	if count != 21 {
		t.Fatalf("GatherAndCount = %d, want 21 (2 accessors x 10 series + freshness)", count)
	}
}

// TestReportsAccessorsCollector_AbsentSensorsEmitNoSeries covers the null
// branch, which on this endpoint is the NORMAL path rather than the corner
// case it is on reports_drives: every entry of the reference capture reports
// null for all six environmental fields, because the accessors on this fleet
// carry no such sensor.
//
// Two things are pinned. The fixture emits none of the six, and never a zero
// in their place: a 0 °C / 0% RH would sit outside R1.11.2's operating
// envelope in both directions. And an accessor that DOES report them emits
// them, beside a sensorless sibling, so the six descriptors are live code on
// other hardware rather than permanently dead weight here.
func TestReportsAccessorsCollector_AbsentSensorsEmitNoSeries(t *testing.T) {
	environmental := []string{
		"tapelibrary_accessor_report_temperature_average_celsius",
		"tapelibrary_accessor_report_temperature_min_celsius",
		"tapelibrary_accessor_report_temperature_max_celsius",
		"tapelibrary_accessor_report_humidity_average_ratio",
		"tapelibrary_accessor_report_humidity_min_ratio",
		"tapelibrary_accessor_report_humidity_max_ratio",
	}

	t.Run("the reference capture emits no environmental series at all", func(t *testing.T) {
		_, c := reportsAccessorsFixtureServer(t)
		c.refresh(context.Background())

		for _, name := range environmental {
			if got := testutil.CollectAndCount(c, name); got != 0 {
				t.Errorf("%s produced %d series, want 0: this fleet's accessors carry no sensor, and a zero would page", name, got)
			}
		}
	})

	t.Run("an accessor that reports sensors emits them beside one that does not", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`[
				{"location":"accessor_Aa","time":"2026-07-28T09:05:00+0000","duration":3600,"pivots":62,
				 "temperatureAverage":null,"temperatureMin":null,"temperatureMax":null,
				 "humidityAverage":null,"humidityMin":null,"humidityMax":null},
				{"location":"accessor_Ab","time":"2026-07-28T09:05:00+0000","duration":3600,"pivots":42,
				 "temperatureAverage":24.5,"temperatureMin":24.0,"temperatureMax":25.0,
				 "humidityAverage":33.0,"humidityMin":32.0,"humidityMax":41.0}
			]`))
		}))
		defer srv.Close()

		log := logger.NewTextLogger("error")
		c := NewReportsAccessorsCollector(log, NewClient(srv.URL, time.Second), 15*time.Minute)
		c.refresh(context.Background())

		// One series each, from the sensor-carrying accessor only: never two,
		// and never a zero standing in for the sensorless one.
		for _, name := range environmental {
			if got := testutil.CollectAndCount(c, name); got != 1 {
				t.Errorf("%s produced %d series, want 1: an absent reading must emit nothing, never a zero", name, got)
			}
		}

		// The activity figures and each window's own identity are unaffected:
		// 10 always-present series per accessor, 6 environmental from the
		// sensor-carrying one only, plus the freshness gauge.
		reg := prometheus.NewRegistry()
		if err := reg.Register(c); err != nil {
			t.Fatalf("Register: %v", err)
		}
		count, err := testutil.GatherAndCount(reg)
		if err != nil {
			t.Fatalf("GatherAndCount: %v", err)
		}
		if count != 27 {
			t.Fatalf("GatherAndCount = %d, want 27 (2 accessors x 10 + 6 environmental + freshness)", count)
		}
	})
}

// TestReportsAccessorsCollector_DoneClosesOnCancel verifies the Done()
// channel closes when the context passed to Start is cancelled. This is the
// mechanism main.go's shutdown seam relies on (see registry.Wait after
// web.ListenAndServe).
func TestReportsAccessorsCollector_DoneClosesOnCancel(t *testing.T) {
	log := logger.NewTextLogger("error")
	c := NewReportsAccessorsCollector(log, NewClient("http://example.invalid", time.Second), 15*time.Minute)
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

// TestReportsAccessorsCollector_CollectServesCacheWithoutIO proves Collect
// never calls the library: after Start's own immediate refresh completes, a
// long interval (15 minutes) guarantees the ticker cannot fire again during
// this test, so any further request the server receives could only come from
// Collect itself calling out, which the design forbids. It matters here
// because the concurrency ceiling is 1: a Collect that called out would
// block every sibling collector on the same library, on every scrape.
func TestReportsAccessorsCollector_CollectServesCacheWithoutIO(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.ServeFile(w, r, "testdata/reports_accessors.json")
	}))
	defer srv.Close()

	log := logger.NewTextLogger("error")
	c := NewReportsAccessorsCollector(log, NewClient(srv.URL, time.Second), 15*time.Minute)
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

// TestReportsAccessorsCollector_ErrorHandling drives a refresh against a
// library that only ever fails. No cache was ever filled, so the scrape must
// carry exactly the freshness gauge, and must neither panic nor emit a
// partial window.
func TestReportsAccessorsCollector_ErrorHandling(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	log := logger.NewTextLogger("error")
	c := NewReportsAccessorsCollector(log, NewClient(srv.URL, time.Second), 15*time.Minute)
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

// TestReportsAccessorsCollector_ErrorKeepsPreviousCache scripts the backend
// to succeed once, then fail on every later call, and drives at least one
// more refresh via a short interval. The cache from the successful first
// refresh must survive the later failure (fail-open, per refresh's doc
// comment) rather than being cleared or replaced with nothing.
func TestReportsAccessorsCollector_ErrorKeepsPreviousCache(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			http.ServeFile(w, r, "testdata/reports_accessors.json")
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	log := logger.NewTextLogger("error")
	c := NewReportsAccessorsCollector(log, NewClient(srv.URL, time.Second), 20*time.Millisecond)
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
	// A surviving cache emits 21 metrics: the 20 cached from the successful
	// first refresh plus the freshness gauge. A wrongly-cleared cache would
	// emit only 1 (the gauge alone), which "> 0" cannot tell apart.
	if count != 21 {
		t.Fatalf("GatherAndCount = %d, want 21: the previous cache must survive a later refresh error", count)
	}
}

// TestReportsAccessorsCollector_StatusTrackerSuccessOnFirstScrape covers the
// startup window before Start's first refresh has completed (Start is
// deliberately never called here). Collect must still emit exactly the
// freshness gauge, valued 0 (not a zero time.Time's large-negative Unix()),
// and StatusTracker must still report this collector as successful: "Collect
// ran and returned data" and "the data is fresh" are different questions.
func TestReportsAccessorsCollector_StatusTrackerSuccessOnFirstScrape(t *testing.T) {
	log := logger.NewTextLogger("error")
	c := NewReportsAccessorsCollector(log, NewClient("http://example.invalid", time.Second), 15*time.Minute)
	// Do NOT call Start(): cache is empty, no refresh has ever run.

	expected := `
# HELP tapelibrary_accessor_report_last_refresh_timestamp_seconds Unix time of the last successful reports/accessors refresh. Alert if time() - this > 2 x the collector's configured interval. Named for this collector's metric subsystem rather than its registered name, so a subsystem sweep finds the freshness of the data it is reading. Distinct from tapelibrary_accessor_report_window_timestamp_seconds, which ages even while this one stays current.
# TYPE tapelibrary_accessor_report_last_refresh_timestamp_seconds gauge
tapelibrary_accessor_report_last_refresh_timestamp_seconds 0
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected)); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}

	tracker := NewStatusTracker(log)
	tracker.Add("reports_accessors", c)

	expectedSuccess := `
# HELP ` + statusTrackerSuccessMetric + ` Whether the last scrape of the collector succeeded (1=success, 0=failure)
# TYPE ` + statusTrackerSuccessMetric + ` gauge
` + statusTrackerSuccessMetric + `{collector="reports_accessors"} 1
`
	if err := testutil.CollectAndCompare(tracker, strings.NewReader(expectedSuccess), statusTrackerSuccessMetric); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}
}
