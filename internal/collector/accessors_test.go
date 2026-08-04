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

// accessorsFixtureServer serves testdata/accessors.json on every request and
// returns a collector already pointed at it. Nothing is started: the caller
// decides whether to drive refresh directly (deterministic) or via Start
// (which is what the lifecycle tests below exercise).
func accessorsFixtureServer(t *testing.T) (*httptest.Server, *AccessorsCollector) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "testdata/accessors.json")
	}))
	t.Cleanup(srv.Close)

	log := logger.NewTextLogger("error")
	return srv, NewAccessorsCollector(log, NewClient(srv.URL, time.Second), time.Hour)
}

// accessorsServing returns a collector fed by a one-shot server that answers
// every request with body. Used by the branch tests below, which need input
// shapes the real capture does not contain — a degraded accessor, a populated
// environmental sensor — and which must therefore not be invented inside
// testdata/accessors.json (that fixture stays faithful to the 2026-07-28
// capture, where both accessors are healthy and both sensors are null).
func accessorsServing(t *testing.T, body string) *AccessorsCollector {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	log := logger.NewTextLogger("error")
	return NewAccessorsCollector(log, NewClient(srv.URL, time.Second), time.Hour)
}

// TestParseAccessors exercises parseAccessors (piece 2, the pure parser) with
// a static byte fixture: no HTTP, no collector, no logger, no goroutine
// involved.
func TestParseAccessors(t *testing.T) {
	data, err := os.ReadFile("testdata/accessors.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	accessors, err := parseAccessors(data)
	if err != nil {
		t.Fatalf("parseAccessors: %v", err)
	}

	if len(accessors) != 2 {
		t.Fatalf("len(accessors) = %d, want 2", len(accessors))
	}

	// The A accessor, which pins every non-nullable field the collector reads.
	a := accessors[0]
	if a.Location != "accessor_Aa" {
		t.Errorf("Location = %q, want %q", a.Location, "accessor_Aa")
	}
	if a.State != "onlineActive" {
		t.Errorf("State = %q, want %q", a.State, "onlineActive")
	}
	if a.DriveAccess != "normal" {
		t.Errorf("DriveAccess = %q, want %q", a.DriveAccess, "normal")
	}
	if a.CartridgeAccess != "normal" {
		t.Errorf("CartridgeAccess = %q, want %q", a.CartridgeAccess, "normal")
	}
	if a.BarCodeScans != 258892 {
		t.Errorf("BarCodeScans = %v, want 258892", a.BarCodeScans)
	}
	if a.VelocityScalingXY != 100 {
		t.Errorf("VelocityScalingXY = %v, want 100", a.VelocityScalingXY)
	}
	if a.TravelX != 329026 {
		t.Errorf("TravelX = %v, want 329026", a.TravelX)
	}
	if a.TravelY != 50453 {
		t.Errorf("TravelY = %v, want 50453", a.TravelY)
	}
	if a.GetsGripper1 != 2727298 {
		t.Errorf("GetsGripper1 = %v, want 2727298", a.GetsGripper1)
	}
	if a.PutsGripper1 != 2727272 {
		t.Errorf("PutsGripper1 = %v, want 2727272", a.PutsGripper1)
	}
	if a.GetsGripper2 != 2727304 {
		t.Errorf("GetsGripper2 = %v, want 2727304", a.GetsGripper2)
	}
	if a.PutsGripper2 != 2727265 {
		t.Errorf("PutsGripper2 = %v, want 2727265", a.PutsGripper2)
	}

	// The nullable four. A pointer is the whole point: the TS4500 carries no
	// environmental sensor on its accessors, so temperature and humidity are
	// null on every entry of the capture, and a plain float64 would decode
	// that to a 0 indistinguishable from a real freezing-cold reading.
	t.Run("a present nullable field decodes to a non-nil pointer", func(t *testing.T) {
		if a.Pivots == nil {
			t.Fatal("Pivots = nil, want a value: the capture reports it")
		}
		if *a.Pivots != 6079578 {
			t.Errorf("*Pivots = %v, want 6079578", *a.Pivots)
		}
		if a.VelocityScalingPivot == nil {
			t.Fatal("VelocityScalingPivot = nil, want a value: the capture reports it")
		}
		if *a.VelocityScalingPivot != 100 {
			t.Errorf("*VelocityScalingPivot = %v, want 100", *a.VelocityScalingPivot)
		}
	})

	t.Run("a null sensor decodes to nil, not to zero", func(t *testing.T) {
		if a.Temperature != nil {
			t.Errorf("Temperature = %v, want nil (null must not be mistaken for a real reading)", *a.Temperature)
		}
		if a.Humidity != nil {
			t.Errorf("Humidity = %v, want nil (null must not be mistaken for a real reading)", *a.Humidity)
		}
	})

	t.Run("malformed input yields an error, not a panic", func(t *testing.T) {
		if _, err := parseAccessors([]byte("not json")); err == nil {
			t.Error("parseAccessors(malformed) returned a nil error, want non-nil")
		}
	})

	t.Run("an object rather than the documented array is an error", func(t *testing.T) {
		if _, err := parseAccessors([]byte(`{"location":"accessor_Aa"}`)); err == nil {
			t.Error("parseAccessors(object) returned a nil error, want non-nil")
		}
	})

	// A library with no accessor cannot move a cartridge and would not be
	// answering this request, so an empty list is a response that lost its
	// content rather than a real inventory. Accepting it would replace a good
	// cache with no accessors at all.
	t.Run("empty array is an error, not a library with no accessors", func(t *testing.T) {
		if _, err := parseAccessors([]byte(`[]`)); err == nil {
			t.Error("parseAccessors(empty array) returned a nil error, want non-nil")
		}
	})

	t.Run("empty input yields an error, not a panic", func(t *testing.T) {
		if _, err := parseAccessors(nil); err == nil {
			t.Error("parseAccessors(nil) returned a nil error, want non-nil")
		}
	})

	t.Run("an entry with no location is an error", func(t *testing.T) {
		if _, err := parseAccessors([]byte(`[{"state":"onlineActive"}]`)); err == nil {
			t.Error("parseAccessors(no location) returned a nil error, want non-nil")
		}
	})

	// Fail closed, in the pure step: two entries at one location would send
	// two metrics sharing a descriptor AND a label set, and Registry.Gather
	// rejects the whole scrape when that happens, taking out every other
	// collector's metrics with it (CONTRIBUTING.md, "Common Pitfalls").
	t.Run("duplicate locations are rejected rather than reaching Collect", func(t *testing.T) {
		dup := `[{"location":"accessor_Aa","state":"onlineActive"},{"location":"accessor_Aa","state":"onlineActive"}]`
		if _, err := parseAccessors([]byte(dup)); err == nil {
			t.Error("parseAccessors(duplicate location) returned a nil error, want non-nil")
		}
	})
}

// TestAccessorsCollector_Describe locks the descriptor count at exactly 13
// (three statesets, five lifetime counters' descriptors, two velocity-scaling
// gauges, two environmental gauges, and the freshness gauge) so a future edit
// that silently adds or drops a metric is caught here rather than downstream
// in docs-check or a dashboard.
//
// Describing all 13 on hardware that populates only 10 of them is deliberate:
// a descriptor states what this collector CAN emit, not what its last refresh
// happened to contain.
func TestAccessorsCollector_Describe(t *testing.T) {
	log := logger.NewTextLogger("error")
	c := NewAccessorsCollector(log, NewClient("http://example.invalid", time.Second), time.Hour)

	ch := make(chan *prometheus.Desc, 30)
	c.Describe(ch)
	close(ch)

	count := 0
	for range ch {
		count++
	}
	if count != 13 {
		t.Fatalf("Describe sent %d descriptors, want 13", count)
	}
}

// TestAccessorsCollector_Collect pins the exact exposition text of every
// business metric against the fixture. refresh is driven directly rather than
// through Start, so the assertion is deterministic and carries no sleep:
// Start's own scheduling is what the lifecycle tests below cover.
//
// Three things matter beyond the raw numbers. The state stateset emits all 11
// known states per accessor, exactly one carrying 1, which keeps the severity
// classification in the alerting rules rather than in the value. drive_access
// and cartridge_access are separate statesets rather than labels on the state
// one: on a dual-accessor library they answer "can this robot still reach the
// drives", which the state of the robot itself cannot express. And neither
// temperature nor humidity appears at all — no TS4500 accessor carries those
// sensors, and a 0 would read as a freezing, bone-dry library.
func TestAccessorsCollector_Collect(t *testing.T) {
	_, c := accessorsFixtureServer(t)
	c.refresh(context.Background())

	expected := `
# HELP tapelibrary_accessor_bar_code_scans_total Number of bar code scans this accessor has performed in its lifetime.
# TYPE tapelibrary_accessor_bar_code_scans_total counter
tapelibrary_accessor_bar_code_scans_total{location="accessor_Aa"} 258892
tapelibrary_accessor_bar_code_scans_total{location="accessor_Ab"} 151821
# HELP tapelibrary_accessor_cartridge_access Whether the accessor can reach the library's cartridges, as a stateset: 1 on the active value and 0 on every other known value. On a dual-accessor library this also reflects the other accessor's position, so an accessor can be online and still report limited.
# TYPE tapelibrary_accessor_cartridge_access gauge
tapelibrary_accessor_cartridge_access{access="limited",location="accessor_Aa"} 0
tapelibrary_accessor_cartridge_access{access="limited",location="accessor_Ab"} 0
tapelibrary_accessor_cartridge_access{access="normal",location="accessor_Aa"} 1
tapelibrary_accessor_cartridge_access{access="normal",location="accessor_Ab"} 1
# HELP tapelibrary_accessor_drive_access Whether the accessor can reach the library's drives, as a stateset: 1 on the active value and 0 on every other known value. On a dual-accessor library this also reflects the other accessor's position, so an accessor can be online and still report limited.
# TYPE tapelibrary_accessor_drive_access gauge
tapelibrary_accessor_drive_access{access="limited",location="accessor_Aa"} 0
tapelibrary_accessor_drive_access{access="limited",location="accessor_Ab"} 0
tapelibrary_accessor_drive_access{access="normal",location="accessor_Aa"} 1
tapelibrary_accessor_drive_access{access="normal",location="accessor_Ab"} 1
# HELP tapelibrary_accessor_gets_total Number of times the gripper has engaged to retrieve a cartridge into this accessor, in its lifetime.
# TYPE tapelibrary_accessor_gets_total counter
tapelibrary_accessor_gets_total{gripper="1",location="accessor_Aa"} 2727298
tapelibrary_accessor_gets_total{gripper="1",location="accessor_Ab"} 2461157
tapelibrary_accessor_gets_total{gripper="2",location="accessor_Aa"} 2727304
tapelibrary_accessor_gets_total{gripper="2",location="accessor_Ab"} 2461154
# HELP tapelibrary_accessor_pivots_total Number of pivots this accessor has performed in its lifetime. An accessor that does not pivot reports no series at all, rather than a 0 claiming it has never pivoted.
# TYPE tapelibrary_accessor_pivots_total counter
tapelibrary_accessor_pivots_total{location="accessor_Aa"} 6079578
tapelibrary_accessor_pivots_total{location="accessor_Ab"} 5516728
# HELP tapelibrary_accessor_puts_total Number of times the gripper has engaged to place a cartridge out of this accessor, in its lifetime.
# TYPE tapelibrary_accessor_puts_total counter
tapelibrary_accessor_puts_total{gripper="1",location="accessor_Aa"} 2727272
tapelibrary_accessor_puts_total{gripper="1",location="accessor_Ab"} 2461125
tapelibrary_accessor_puts_total{gripper="2",location="accessor_Aa"} 2727265
tapelibrary_accessor_puts_total{gripper="2",location="accessor_Ab"} 2461126
# HELP tapelibrary_accessor_state Operational state of the robotic accessor, as a stateset: 1 on the active state and 0 on every other known state.
# TYPE tapelibrary_accessor_state gauge
tapelibrary_accessor_state{location="accessor_Aa",state="bothGrippersFailed"} 0
tapelibrary_accessor_state{location="accessor_Aa",state="calibrating"} 0
tapelibrary_accessor_state{location="accessor_Aa",state="failedToInitialize"} 0
tapelibrary_accessor_state{location="accessor_Aa",state="gripper1Failed"} 0
tapelibrary_accessor_state{location="accessor_Aa",state="gripper2Failed"} 0
tapelibrary_accessor_state{location="accessor_Aa",state="inServiceMode"} 0
tapelibrary_accessor_state{location="accessor_Aa",state="noMotorPower"} 0
tapelibrary_accessor_state{location="accessor_Aa",state="noMovementAllowed"} 0
tapelibrary_accessor_state{location="accessor_Aa",state="onlineActive"} 1
tapelibrary_accessor_state{location="accessor_Aa",state="onlineStandby"} 0
tapelibrary_accessor_state{location="accessor_Aa",state="scannerFailed"} 0
tapelibrary_accessor_state{location="accessor_Ab",state="bothGrippersFailed"} 0
tapelibrary_accessor_state{location="accessor_Ab",state="calibrating"} 0
tapelibrary_accessor_state{location="accessor_Ab",state="failedToInitialize"} 0
tapelibrary_accessor_state{location="accessor_Ab",state="gripper1Failed"} 0
tapelibrary_accessor_state{location="accessor_Ab",state="gripper2Failed"} 0
tapelibrary_accessor_state{location="accessor_Ab",state="inServiceMode"} 0
tapelibrary_accessor_state{location="accessor_Ab",state="noMotorPower"} 0
tapelibrary_accessor_state{location="accessor_Ab",state="noMovementAllowed"} 0
tapelibrary_accessor_state{location="accessor_Ab",state="onlineActive"} 1
tapelibrary_accessor_state{location="accessor_Ab",state="onlineStandby"} 0
tapelibrary_accessor_state{location="accessor_Ab",state="scannerFailed"} 0
# HELP tapelibrary_accessor_travel_meters_total Distance in meters this accessor has travelled in its lifetime, per axis: x is horizontal, y is vertical.
# TYPE tapelibrary_accessor_travel_meters_total counter
tapelibrary_accessor_travel_meters_total{axis="x",location="accessor_Aa"} 329026
tapelibrary_accessor_travel_meters_total{axis="x",location="accessor_Ab"} 328348
tapelibrary_accessor_travel_meters_total{axis="y",location="accessor_Aa"} 50453
tapelibrary_accessor_travel_meters_total{axis="y",location="accessor_Ab"} 291176
# HELP tapelibrary_accessor_velocity_scaling_pivot_ratio Scaling applied to the accessor's maximum pivot velocity, as a ratio from 0 to 1 (the API reports a 0-100 percentage). An accessor that does not pivot reports no series at all.
# TYPE tapelibrary_accessor_velocity_scaling_pivot_ratio gauge
tapelibrary_accessor_velocity_scaling_pivot_ratio{location="accessor_Aa"} 1
tapelibrary_accessor_velocity_scaling_pivot_ratio{location="accessor_Ab"} 1
# HELP tapelibrary_accessor_velocity_scaling_xy_ratio Scaling applied to the accessor's maximum velocity in the X and Y directions, as a ratio from 0 to 1 (the API reports a 0-100 percentage). Anything below 1 means the accessor has been deliberately slowed.
# TYPE tapelibrary_accessor_velocity_scaling_xy_ratio gauge
tapelibrary_accessor_velocity_scaling_xy_ratio{location="accessor_Aa"} 1
tapelibrary_accessor_velocity_scaling_xy_ratio{location="accessor_Ab"} 1
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected),
		"tapelibrary_accessor_bar_code_scans_total",
		"tapelibrary_accessor_cartridge_access",
		"tapelibrary_accessor_drive_access",
		"tapelibrary_accessor_gets_total",
		"tapelibrary_accessor_humidity_ratio",
		"tapelibrary_accessor_pivots_total",
		"tapelibrary_accessor_puts_total",
		"tapelibrary_accessor_state",
		"tapelibrary_accessor_temperature_celsius",
		"tapelibrary_accessor_travel_meters_total",
		"tapelibrary_accessor_velocity_scaling_pivot_ratio",
		"tapelibrary_accessor_velocity_scaling_xy_ratio",
	); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}

	// 50 cached metrics (2 accessors x (11 states + 2 drive access + 2
	// cartridge access + 1 scans + 2 travel axes + 2 gets + 2 puts + 1 pivots
	// + 2 velocity scalings)) plus the freshness gauge Collect always appends.
	// Temperature and humidity contribute nothing: this hardware has no
	// sensor. That is the 51-observed figure recorded against this collector's
	// cardinality budget in docs/exporter-journal.md.
	reg := prometheus.NewRegistry()
	if err := reg.Register(c); err != nil {
		t.Fatalf("Register: %v", err)
	}
	count, err := testutil.GatherAndCount(reg)
	if err != nil {
		t.Fatalf("GatherAndCount: %v", err)
	}
	if count != 51 {
		t.Fatalf("GatherAndCount = %d, want 51 (2 accessors x 25 + freshness)", count)
	}
}

// TestAccessorsCollector_UndocumentedValueIsStillEmitted covers the manual's
// own incompleteness on both kinds of enumerated field this collector emits.
// The state tables are demonstrably a floor rather than a ceiling — the manual
// names failedToInitialize in prose and tabulates it nowhere — so a value
// outside the documented set must surface as its own series at 1, rather than
// leaving every documented series at 0 and making the accessor look stateless.
//
// Both families are checked because refresh drives all three statesets through
// one shared helper: a regression there would hit driveAccess and
// cartridgeAccess just as readily as state.
func TestAccessorsCollector_UndocumentedValueIsStillEmitted(t *testing.T) {
	c := accessorsServing(t, `[{"location":"accessor_Aa","state":"someUndocumentedState","driveAccess":"blocked","cartridgeAccess":"normal","pivots":1,"barCodeScans":2,"velocityScalingXY":100,"velocityScalingPivot":100,"travelX":3,"travelY":4,"getsGripper1":5,"putsGripper1":6,"getsGripper2":7,"putsGripper2":8,"temperature":null,"humidity":null}]`)
	c.refresh(context.Background())

	expected := `
# HELP tapelibrary_accessor_drive_access Whether the accessor can reach the library's drives, as a stateset: 1 on the active value and 0 on every other known value. On a dual-accessor library this also reflects the other accessor's position, so an accessor can be online and still report limited.
# TYPE tapelibrary_accessor_drive_access gauge
tapelibrary_accessor_drive_access{access="blocked",location="accessor_Aa"} 1
tapelibrary_accessor_drive_access{access="limited",location="accessor_Aa"} 0
tapelibrary_accessor_drive_access{access="normal",location="accessor_Aa"} 0
# HELP tapelibrary_accessor_state Operational state of the robotic accessor, as a stateset: 1 on the active state and 0 on every other known state.
# TYPE tapelibrary_accessor_state gauge
tapelibrary_accessor_state{location="accessor_Aa",state="bothGrippersFailed"} 0
tapelibrary_accessor_state{location="accessor_Aa",state="calibrating"} 0
tapelibrary_accessor_state{location="accessor_Aa",state="failedToInitialize"} 0
tapelibrary_accessor_state{location="accessor_Aa",state="gripper1Failed"} 0
tapelibrary_accessor_state{location="accessor_Aa",state="gripper2Failed"} 0
tapelibrary_accessor_state{location="accessor_Aa",state="inServiceMode"} 0
tapelibrary_accessor_state{location="accessor_Aa",state="noMotorPower"} 0
tapelibrary_accessor_state{location="accessor_Aa",state="noMovementAllowed"} 0
tapelibrary_accessor_state{location="accessor_Aa",state="onlineActive"} 0
tapelibrary_accessor_state{location="accessor_Aa",state="onlineStandby"} 0
tapelibrary_accessor_state{location="accessor_Aa",state="scannerFailed"} 0
tapelibrary_accessor_state{location="accessor_Aa",state="someUndocumentedState"} 1
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected),
		"tapelibrary_accessor_drive_access",
		"tapelibrary_accessor_state",
	); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}
}

// TestAccessorsCollector_NullableFieldsEmitNoSeries pins the behaviour of the
// four nullable fields from the other side of the fixture: here the
// environmental sensors DO report and the pivot fields do not, the exact
// mirror of what every TS4500 in the capture returns.
//
// The point is that null never becomes 0. A pivot count of 0 and an accessor
// that cannot pivot are different facts; so are 0 degrees Celsius and no
// thermometer. Emitting a series for the second of each pair is how a
// dashboard ends up averaging a temperature nobody measured.
func TestAccessorsCollector_NullableFieldsEmitNoSeries(t *testing.T) {
	c := accessorsServing(t, `[{"location":"accessor_Aa","state":"onlineActive","driveAccess":"normal","cartridgeAccess":"normal","pivots":null,"barCodeScans":2,"velocityScalingXY":80,"velocityScalingPivot":null,"travelX":3,"travelY":4,"getsGripper1":5,"putsGripper1":6,"getsGripper2":7,"putsGripper2":8,"temperature":32.1,"humidity":28.9}]`)
	c.refresh(context.Background())

	// Both percentages become ratios; both nullable pivot metrics are absent
	// entirely rather than present at 0.
	expected := `
# HELP tapelibrary_accessor_humidity_ratio Relative humidity measured inside the library by a sensor on this accessor, as a ratio from 0 to 1 (the API reports a 0-100 percentage). Accessors carrying no such sensor report no series at all.
# TYPE tapelibrary_accessor_humidity_ratio gauge
tapelibrary_accessor_humidity_ratio{location="accessor_Aa"} 0.289
# HELP tapelibrary_accessor_temperature_celsius Temperature in Celsius measured inside the library by a sensor on this accessor, at its current position. Accessors carrying no such sensor report no series at all, rather than a 0 that would read as a freezing library.
# TYPE tapelibrary_accessor_temperature_celsius gauge
tapelibrary_accessor_temperature_celsius{location="accessor_Aa"} 32.1
# HELP tapelibrary_accessor_velocity_scaling_xy_ratio Scaling applied to the accessor's maximum velocity in the X and Y directions, as a ratio from 0 to 1 (the API reports a 0-100 percentage). Anything below 1 means the accessor has been deliberately slowed.
# TYPE tapelibrary_accessor_velocity_scaling_xy_ratio gauge
tapelibrary_accessor_velocity_scaling_xy_ratio{location="accessor_Aa"} 0.8
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected),
		"tapelibrary_accessor_humidity_ratio",
		"tapelibrary_accessor_pivots_total",
		"tapelibrary_accessor_temperature_celsius",
		"tapelibrary_accessor_velocity_scaling_pivot_ratio",
		"tapelibrary_accessor_velocity_scaling_xy_ratio",
	); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}

	// 11 states + 2 drive access + 2 cartridge access + 1 scans + 2 travel
	// axes + 2 gets + 2 puts + 1 velocity XY + temperature + humidity +
	// freshness. Pinned so that a future change emitting a 0 for either null
	// pivot field is caught here even if it somehow satisfied the comparison
	// above.
	reg := prometheus.NewRegistry()
	if err := reg.Register(c); err != nil {
		t.Fatalf("Register: %v", err)
	}
	count, err := testutil.GatherAndCount(reg)
	if err != nil {
		t.Fatalf("GatherAndCount: %v", err)
	}
	if count != 26 {
		t.Fatalf("GatherAndCount = %d, want 26 (neither null pivot field produces a series)", count)
	}
}

// TestAccessorsCollector_DoneClosesOnCancel verifies the Done() channel closes
// when the context passed to Start is cancelled. This is the mechanism
// main.go's shutdown seam relies on (see registry.Wait after
// web.ListenAndServe).
func TestAccessorsCollector_DoneClosesOnCancel(t *testing.T) {
	log := logger.NewTextLogger("error")
	c := NewAccessorsCollector(log, NewClient("http://example.invalid", time.Second), time.Hour)
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

// TestAccessorsCollector_CollectServesCacheWithoutIO proves Collect never
// calls the library: after Start's own immediate refresh completes, a long
// interval (1 hour) guarantees the ticker cannot fire again during this test,
// so any further request the server receives could only come from Collect
// itself calling out, which the design forbids.
func TestAccessorsCollector_CollectServesCacheWithoutIO(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.ServeFile(w, r, "testdata/accessors.json")
	}))
	defer srv.Close()

	log := logger.NewTextLogger("error")
	c := NewAccessorsCollector(log, NewClient(srv.URL, time.Second), time.Hour)
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

// TestAccessorsCollector_ErrorHandling drives a refresh against a library that
// only ever fails. No cache was ever filled, so the scrape must carry exactly
// the freshness gauge, and must neither panic nor emit a partial stateset.
func TestAccessorsCollector_ErrorHandling(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	log := logger.NewTextLogger("error")
	c := NewAccessorsCollector(log, NewClient(srv.URL, time.Second), time.Hour)
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

// TestAccessorsCollector_ErrorKeepsPreviousCache scripts the backend to
// succeed once, then fail on every later call, and drives at least one more
// refresh via a short interval. The cache from the successful first refresh
// must survive the later failure (fail-open, per refresh's doc comment) rather
// than being cleared or replaced with nothing.
func TestAccessorsCollector_ErrorKeepsPreviousCache(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			http.ServeFile(w, r, "testdata/accessors.json")
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	log := logger.NewTextLogger("error")
	c := NewAccessorsCollector(log, NewClient(srv.URL, time.Second), 20*time.Millisecond)
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
	// A surviving cache emits 51 metrics: the 50 cached from the successful
	// first refresh plus the freshness gauge. A wrongly-cleared cache would
	// emit only 1 (the gauge alone), which "> 0" cannot tell apart.
	if count != 51 {
		t.Fatalf("GatherAndCount = %d, want 51: the previous cache must survive a later refresh error", count)
	}
}

// TestAccessorsCollector_StatusTrackerSuccessOnFirstScrape covers the startup
// window before Start's first refresh has completed (Start is deliberately
// never called here). Collect must still emit exactly the freshness gauge,
// valued 0 (not a zero time.Time's large-negative Unix()), and StatusTracker
// must still report this collector as successful: "Collect ran and returned
// data" and "the data is fresh" are different questions.
func TestAccessorsCollector_StatusTrackerSuccessOnFirstScrape(t *testing.T) {
	log := logger.NewTextLogger("error")
	c := NewAccessorsCollector(log, NewClient("http://example.invalid", time.Second), time.Hour)
	// Do NOT call Start(): cache is empty, no refresh has ever run.

	expected := `
# HELP tapelibrary_accessors_last_refresh_timestamp_seconds Unix time of the last successful accessors refresh. Alert if time() - this > 2 x the collector's configured interval.
# TYPE tapelibrary_accessors_last_refresh_timestamp_seconds gauge
tapelibrary_accessors_last_refresh_timestamp_seconds 0
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected)); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}

	tracker := NewStatusTracker(log)
	tracker.Add("accessors", c)

	expectedSuccess := `
# HELP ` + statusTrackerSuccessMetric + ` Whether the last scrape of the collector succeeded (1=success, 0=failure)
# TYPE ` + statusTrackerSuccessMetric + ` gauge
` + statusTrackerSuccessMetric + `{collector="accessors"} 1
`
	if err := testutil.CollectAndCompare(tracker, strings.NewReader(expectedSuccess), statusTrackerSuccessMetric); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}
}
