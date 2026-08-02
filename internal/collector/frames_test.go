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

// framesFixtureServer serves testdata/frames.json on every request and
// returns a collector already pointed at it. Nothing is started: the caller
// decides whether to drive refresh directly (deterministic) or via Start
// (which is what the lifecycle tests below exercise).
func framesFixtureServer(t *testing.T) (*httptest.Server, *FramesCollector) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "testdata/frames.json")
	}))
	t.Cleanup(srv.Close)

	log := logger.NewTextLogger("error")
	return srv, NewFramesCollector(log, NewClient(srv.URL, time.Second), time.Hour)
}

// framesServing returns a collector fed by a one-shot server that answers
// every request with body. Used by the branch tests below, which need input
// shapes the real capture does not contain and which must therefore not be
// invented inside testdata/frames.json (that fixture stays faithful to the
// 2026-07-28 capture).
func framesServing(t *testing.T, body string) *FramesCollector {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	log := logger.NewTextLogger("error")
	return NewFramesCollector(log, NewClient(srv.URL, time.Second), time.Hour)
}

// TestParseFrames exercises parseFrames (piece 2, the pure parser) with a
// static byte fixture: no HTTP, no collector, no logger, no goroutine
// involved.
func TestParseFrames(t *testing.T) {
	data, err := os.ReadFile("testdata/frames.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	frames, err := parseFrames(data)
	if err != nil {
		t.Fatalf("parseFrames: %v", err)
	}

	if len(frames) != 3 {
		t.Fatalf("len(frames) = %d, want 3", len(frames))
	}

	// The base frame, which is the only one in the fixture carrying I/O
	// stations, and which pins every field the collector reads.
	base := frames[1]
	if base.Location != "frame_F2" {
		t.Errorf("Location = %q, want %q", base.Location, "frame_F2")
	}
	if base.State != "normal" {
		t.Errorf("State = %q, want %q", base.State, "normal")
	}
	if base.Type != "base" {
		t.Errorf("Type = %q, want %q", base.Type, "base")
	}
	if base.MTM != "3584-L25" {
		t.Errorf("MTM = %q, want %q", base.MTM, "3584-L25")
	}
	if base.SN != "SN00000002" {
		t.Errorf("SN = %q, want %q", base.SN, "SN00000002")
	}
	if base.MediaType != "3592" {
		t.Errorf("MediaType = %q, want %q", base.MediaType, "3592")
	}
	if base.FrontDoor != "closed" {
		t.Errorf("FrontDoor = %q, want %q", base.FrontDoor, "closed")
	}
	if base.SideDoor != "closed" {
		t.Errorf("SideDoor = %q, want %q", base.SideDoor, "closed")
	}
	if base.Slots != 660 {
		t.Errorf("Slots = %v, want 660", base.Slots)
	}
	if base.Cartridges != 661 {
		t.Errorf("Cartridges = %v, want 661", base.Cartridges)
	}
	if base.Drives != 16 {
		t.Errorf("Drives = %v, want 16", base.Drives)
	}
	if base.IOStations != 2 {
		t.Errorf("IOStations = %v, want 2", base.IOStations)
	}

	// A null door must decode to the empty string rather than to "closed":
	// the whole capture reports rearDoor null on every frame, and the
	// collector relies on this distinction to emit no series at all for a
	// door position the hardware does not have.
	t.Run("a null door decodes to the empty string, not to closed", func(t *testing.T) {
		if base.RearDoor != "" {
			t.Errorf("RearDoor = %q, want %q (null must not be mistaken for a real position)", base.RearDoor, "")
		}
	})

	t.Run("malformed input yields an error, not a panic", func(t *testing.T) {
		if _, err := parseFrames([]byte("not json")); err == nil {
			t.Error("parseFrames(malformed) returned a nil error, want non-nil")
		}
	})

	t.Run("an object rather than the documented array is an error", func(t *testing.T) {
		if _, err := parseFrames([]byte(`{"location":"frame_F1"}`)); err == nil {
			t.Error("parseFrames(object) returned a nil error, want non-nil")
		}
	})

	// Every TS4500 has at least a base frame, so an empty list is a response
	// that lost its content rather than a library with no frames. Accepting
	// it would replace a good cache with no frames at all.
	t.Run("empty array is an error, not a library with no frames", func(t *testing.T) {
		if _, err := parseFrames([]byte(`[]`)); err == nil {
			t.Error("parseFrames(empty array) returned a nil error, want non-nil")
		}
	})

	t.Run("empty input yields an error, not a panic", func(t *testing.T) {
		if _, err := parseFrames(nil); err == nil {
			t.Error("parseFrames(nil) returned a nil error, want non-nil")
		}
	})

	t.Run("an entry with no location is an error", func(t *testing.T) {
		if _, err := parseFrames([]byte(`[{"state":"normal"}]`)); err == nil {
			t.Error("parseFrames(no location) returned a nil error, want non-nil")
		}
	})

	// Fail closed, in the pure step: two entries at one location would send
	// two metrics sharing a descriptor AND a label set, and Registry.Gather
	// rejects the whole scrape when that happens, taking out every other
	// collector's metrics with it (CONTRIBUTING.md, "Common Pitfalls").
	t.Run("duplicate locations are rejected rather than reaching Collect", func(t *testing.T) {
		dup := `[{"location":"frame_F1","state":"normal"},{"location":"frame_F1","state":"normal"}]`
		if _, err := parseFrames([]byte(dup)); err == nil {
			t.Error("parseFrames(duplicate location) returned a nil error, want non-nil")
		}
	})
}

// TestFramesCollector_Describe locks the descriptor count at exactly 8 (the
// stateset, the door gauge, four per-frame counts, the identity info metric,
// and the freshness gauge) so a future edit that silently adds or drops a
// metric is caught here rather than downstream in docs-check or a dashboard.
func TestFramesCollector_Describe(t *testing.T) {
	log := logger.NewTextLogger("error")
	c := NewFramesCollector(log, NewClient("http://example.invalid", time.Second), time.Hour)

	ch := make(chan *prometheus.Desc, 20)
	c.Describe(ch)
	close(ch)

	count := 0
	for range ch {
		count++
	}
	if count != 8 {
		t.Fatalf("Describe sent %d descriptors, want 8", count)
	}
}

// TestFramesCollector_Collect pins the exact exposition text of every
// business metric against the fixture. refresh is driven directly rather
// than through Start, so the assertion is deterministic and carries no
// sleep: Start's own scheduling is what the lifecycle tests below cover.
//
// Two halves matter beyond the raw numbers. The stateset emits all six
// documented states per frame, exactly one carrying 1, which is what keeps
// the severity classification in the alerting rules rather than in the
// value. And tapelibrary_frame_door_open carries front and side only: every
// frame in the capture reports rearDoor null, and a frame with no rear door
// must produce no series rather than a 0 asserting that a door which does
// not exist is closed.
func TestFramesCollector_Collect(t *testing.T) {
	_, c := framesFixtureServer(t)
	c.refresh(context.Background())

	expected := `
# HELP tapelibrary_frame_cartridges_present Number of cartridges currently present in this frame.
# TYPE tapelibrary_frame_cartridges_present gauge
tapelibrary_frame_cartridges_present{location="frame_F1"} 141
tapelibrary_frame_cartridges_present{location="frame_F2"} 661
tapelibrary_frame_cartridges_present{location="frame_F3"} 991
# HELP tapelibrary_frame_door_open Whether the frame's door is open (1) or closed (0). A door position the frame does not physically have reports no series at all, rather than a 0 claiming a closed door that does not exist.
# TYPE tapelibrary_frame_door_open gauge
tapelibrary_frame_door_open{door="front",location="frame_F1"} 0
tapelibrary_frame_door_open{door="front",location="frame_F2"} 0
tapelibrary_frame_door_open{door="front",location="frame_F3"} 0
tapelibrary_frame_door_open{door="side",location="frame_F1"} 0
tapelibrary_frame_door_open{door="side",location="frame_F2"} 0
tapelibrary_frame_door_open{door="side",location="frame_F3"} 0
# HELP tapelibrary_frame_drives_installed Number of tape drives installed in this frame.
# TYPE tapelibrary_frame_drives_installed gauge
tapelibrary_frame_drives_installed{location="frame_F1"} 4
tapelibrary_frame_drives_installed{location="frame_F2"} 16
tapelibrary_frame_drives_installed{location="frame_F3"} 0
# HELP tapelibrary_frame_info Frame identity, always 1. Identity strings live here rather than on a measurement series, so replacing a frame changes this series alone instead of breaking the continuity of every other.
# TYPE tapelibrary_frame_info gauge
tapelibrary_frame_info{frame_type="base",location="frame_F2",media_type="3592",mtm="3584-L25",serial="SN00000002"} 1
tapelibrary_frame_info{frame_type="expansion",location="frame_F1",media_type="3592",mtm="3584-D25",serial="SN00000001"} 1
tapelibrary_frame_info{frame_type="storageOnlyExpansion",location="frame_F3",media_type="3592",mtm="3584-S25",serial="SN00000003"} 1
# HELP tapelibrary_frame_io_stations_installed Number of I/O stations installed in this frame.
# TYPE tapelibrary_frame_io_stations_installed gauge
tapelibrary_frame_io_stations_installed{location="frame_F1"} 0
tapelibrary_frame_io_stations_installed{location="frame_F2"} 2
tapelibrary_frame_io_stations_installed{location="frame_F3"} 0
# HELP tapelibrary_frame_slots_capacity Number of cartridge slots this frame physically holds.
# TYPE tapelibrary_frame_slots_capacity gauge
tapelibrary_frame_slots_capacity{location="frame_F1"} 590
tapelibrary_frame_slots_capacity{location="frame_F2"} 660
tapelibrary_frame_slots_capacity{location="frame_F3"} 1000
# HELP tapelibrary_frame_state Operational state of the frame, as a stateset: 1 on the active state and 0 on every other known state.
# TYPE tapelibrary_frame_state gauge
tapelibrary_frame_state{location="frame_F1",state="acUnreachable"} 0
tapelibrary_frame_state{location="frame_F1",state="calibrationRequired"} 0
tapelibrary_frame_state{location="frame_F1",state="frontDoorOpenWhileNotAllowed"} 0
tapelibrary_frame_state{location="frame_F1",state="inventoryPending"} 0
tapelibrary_frame_state{location="frame_F1",state="normal"} 1
tapelibrary_frame_state{location="frame_F1",state="unknown"} 0
tapelibrary_frame_state{location="frame_F2",state="acUnreachable"} 0
tapelibrary_frame_state{location="frame_F2",state="calibrationRequired"} 0
tapelibrary_frame_state{location="frame_F2",state="frontDoorOpenWhileNotAllowed"} 0
tapelibrary_frame_state{location="frame_F2",state="inventoryPending"} 0
tapelibrary_frame_state{location="frame_F2",state="normal"} 1
tapelibrary_frame_state{location="frame_F2",state="unknown"} 0
tapelibrary_frame_state{location="frame_F3",state="acUnreachable"} 0
tapelibrary_frame_state{location="frame_F3",state="calibrationRequired"} 0
tapelibrary_frame_state{location="frame_F3",state="frontDoorOpenWhileNotAllowed"} 0
tapelibrary_frame_state{location="frame_F3",state="inventoryPending"} 0
tapelibrary_frame_state{location="frame_F3",state="normal"} 1
tapelibrary_frame_state{location="frame_F3",state="unknown"} 0
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected),
		"tapelibrary_frame_cartridges_present",
		"tapelibrary_frame_door_open",
		"tapelibrary_frame_drives_installed",
		"tapelibrary_frame_info",
		"tapelibrary_frame_io_stations_installed",
		"tapelibrary_frame_slots_capacity",
		"tapelibrary_frame_state",
	); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}

	// 39 cached metrics (3 frames x (6 stateset + 2 doors + 4 counts + 1
	// info)) plus the freshness gauge Collect always appends. The real
	// library carries 12 frames rather than the fixture's 3, which is the
	// 157-series figure recorded against this collector's cardinality budget
	// in docs/exporter-journal.md.
	reg := prometheus.NewRegistry()
	if err := reg.Register(c); err != nil {
		t.Fatalf("Register: %v", err)
	}
	count, err := testutil.GatherAndCount(reg)
	if err != nil {
		t.Fatalf("GatherAndCount: %v", err)
	}
	if count != 40 {
		t.Fatalf("GatherAndCount = %d, want 40 (3 frames x 13 + freshness)", count)
	}
}

// TestFramesCollector_UndocumentedStateIsStillEmitted covers the manual's
// own incompleteness: it names states in prose that appear in none of its
// tables. A state outside the documented set must surface as its own series
// at 1, rather than leaving all six documented series at 0 and making the
// frame look stateless.
func TestFramesCollector_UndocumentedStateIsStillEmitted(t *testing.T) {
	c := framesServing(t, `[{"location":"frame_F1","state":"someUndocumentedState","sn":"SN00000001","mtm":"3584-D25","type":"expansion","mediaType":"3592","frontDoor":"closed","rearDoor":null,"sideDoor":"closed","slots":590,"cartridges":141,"drives":4,"ioStations":0}]`)
	c.refresh(context.Background())

	expected := `
# HELP tapelibrary_frame_state Operational state of the frame, as a stateset: 1 on the active state and 0 on every other known state.
# TYPE tapelibrary_frame_state gauge
tapelibrary_frame_state{location="frame_F1",state="acUnreachable"} 0
tapelibrary_frame_state{location="frame_F1",state="calibrationRequired"} 0
tapelibrary_frame_state{location="frame_F1",state="frontDoorOpenWhileNotAllowed"} 0
tapelibrary_frame_state{location="frame_F1",state="inventoryPending"} 0
tapelibrary_frame_state{location="frame_F1",state="normal"} 0
tapelibrary_frame_state{location="frame_F1",state="someUndocumentedState"} 1
tapelibrary_frame_state{location="frame_F1",state="unknown"} 0
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected), "tapelibrary_frame_state"); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}
}

// TestFramesCollector_DoorPositions pins the three-way behaviour of the door
// gauge on a single frame: an open door reports 1, a door the frame does not
// have (null on the wire) reports no series at all, and a position this
// collector cannot map to open-or-closed also reports no series rather than
// a 0 quietly asserting "closed".
//
// The last case is the one that matters operationally: a silent 0 there is
// exactly how a door alert stops firing without anyone noticing, which is
// the failure mode the legacy RoS rules have been living with (they match on
// frame states that R1.11.2 does not define at all).
func TestFramesCollector_DoorPositions(t *testing.T) {
	c := framesServing(t, `[{"location":"frame_F1","state":"normal","sn":"SN00000001","mtm":"3584-D25","type":"expansion","mediaType":"3592","frontDoor":"open","rearDoor":null,"sideDoor":"ajar","slots":590,"cartridges":141,"drives":4,"ioStations":0}]`)
	c.refresh(context.Background())

	expected := `
# HELP tapelibrary_frame_door_open Whether the frame's door is open (1) or closed (0). A door position the frame does not physically have reports no series at all, rather than a 0 claiming a closed door that does not exist.
# TYPE tapelibrary_frame_door_open gauge
tapelibrary_frame_door_open{door="front",location="frame_F1"} 1
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected), "tapelibrary_frame_door_open"); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}

	// 6 stateset + 1 door + 4 counts + 1 info + freshness. Pinned so that a
	// future change emitting a 0 for the null or the unrecognized door is
	// caught here even if it somehow satisfied the comparison above.
	reg := prometheus.NewRegistry()
	if err := reg.Register(c); err != nil {
		t.Fatalf("Register: %v", err)
	}
	count, err := testutil.GatherAndCount(reg)
	if err != nil {
		t.Fatalf("GatherAndCount: %v", err)
	}
	if count != 13 {
		t.Fatalf("GatherAndCount = %d, want 13 (only the front door produces a series)", count)
	}
}

// TestFramesCollector_DoneClosesOnCancel verifies the Done() channel closes
// when the context passed to Start is cancelled. This is the mechanism
// main.go's shutdown seam relies on (see registry.Wait after
// web.ListenAndServe).
func TestFramesCollector_DoneClosesOnCancel(t *testing.T) {
	log := logger.NewTextLogger("error")
	c := NewFramesCollector(log, NewClient("http://example.invalid", time.Second), time.Hour)
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

// TestFramesCollector_CollectServesCacheWithoutIO proves Collect never calls
// the library: after Start's own immediate refresh completes, a long interval
// (1 hour) guarantees the ticker cannot fire again during this test, so any
// further request the server receives could only come from Collect itself
// calling out, which the design forbids.
func TestFramesCollector_CollectServesCacheWithoutIO(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.ServeFile(w, r, "testdata/frames.json")
	}))
	defer srv.Close()

	log := logger.NewTextLogger("error")
	c := NewFramesCollector(log, NewClient(srv.URL, time.Second), time.Hour)
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

// TestFramesCollector_ErrorHandling drives a refresh against a library that
// only ever fails. No cache was ever filled, so the scrape must carry exactly
// the freshness gauge, and must neither panic nor emit a partial stateset.
func TestFramesCollector_ErrorHandling(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	log := logger.NewTextLogger("error")
	c := NewFramesCollector(log, NewClient(srv.URL, time.Second), time.Hour)
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

// TestFramesCollector_ErrorKeepsPreviousCache scripts the backend to succeed
// once, then fail on every later call, and drives at least one more refresh
// via a short interval. The cache from the successful first refresh must
// survive the later failure (fail-open, per refresh's doc comment) rather
// than being cleared or replaced with nothing.
func TestFramesCollector_ErrorKeepsPreviousCache(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			http.ServeFile(w, r, "testdata/frames.json")
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	log := logger.NewTextLogger("error")
	c := NewFramesCollector(log, NewClient(srv.URL, time.Second), 20*time.Millisecond)
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
	// A surviving cache emits 40 metrics: the 39 cached from the successful
	// first refresh plus the freshness gauge. A wrongly-cleared cache would
	// emit only 1 (the gauge alone), which "> 0" cannot tell apart.
	if count != 40 {
		t.Fatalf("GatherAndCount = %d, want 40: the previous cache must survive a later refresh error", count)
	}
}

// TestFramesCollector_StatusTrackerSuccessOnFirstScrape covers the startup
// window before Start's first refresh has completed (Start is deliberately
// never called here). Collect must still emit exactly the freshness gauge,
// valued 0 (not a zero time.Time's large-negative Unix()), and StatusTracker
// must still report this collector as successful: "Collect ran and returned
// data" and "the data is fresh" are different questions.
func TestFramesCollector_StatusTrackerSuccessOnFirstScrape(t *testing.T) {
	log := logger.NewTextLogger("error")
	c := NewFramesCollector(log, NewClient("http://example.invalid", time.Second), time.Hour)
	// Do NOT call Start(): cache is empty, no refresh has ever run.

	expected := `
# HELP tapelibrary_frames_last_refresh_timestamp_seconds Unix time of the last successful frames refresh. Alert if time() - this > 2 x the collector's configured interval.
# TYPE tapelibrary_frames_last_refresh_timestamp_seconds gauge
tapelibrary_frames_last_refresh_timestamp_seconds 0
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected)); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}

	tracker := NewStatusTracker(log)
	tracker.Add("frames", c)

	expectedSuccess := `
# HELP ` + statusTrackerSuccessMetric + ` Whether the last scrape of the collector succeeded (1=success, 0=failure)
# TYPE ` + statusTrackerSuccessMetric + ` gauge
` + statusTrackerSuccessMetric + `{collector="frames"} 1
`
	if err := testutil.CollectAndCompare(tracker, strings.NewReader(expectedSuccess), statusTrackerSuccessMetric); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}
}
