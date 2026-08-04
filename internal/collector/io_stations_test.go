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

// ioStationsMetricNames is every family this collector emits except the
// freshness gauge, whose value is a wall-clock timestamp and therefore cannot
// be pinned in an exposition block. Listed in the order Registry.Gather sorts
// them (by family name), which is the order the expected blocks below use.
var ioStationsMetricNames = []string{
	"tapelibrary_io_station_door_open",
	"tapelibrary_io_station_info",
	"tapelibrary_io_station_magazine_present",
	"tapelibrary_io_station_slots",
	"tapelibrary_io_station_slots_occupied",
	"tapelibrary_io_station_slots_unreadable",
	"tapelibrary_io_station_state",
}

// ioStationsFixtureServer serves testdata/io_stations.json on every request and
// returns a collector already pointed at it. Nothing is started: the caller
// decides whether to drive refresh directly (deterministic) or via Start (which
// is what the lifecycle tests below exercise).
func ioStationsFixtureServer(t *testing.T) (*httptest.Server, *IOStationsCollector) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "testdata/io_stations.json")
	}))
	t.Cleanup(srv.Close)

	log := logger.NewTextLogger("error")
	return srv, NewIOStationsCollector(log, NewClient(srv.URL, time.Second), time.Hour)
}

// ioStationsServing returns a collector fed by a server that answers every
// request with body. Used by the branch tests below, which need input shapes
// the real capture does not contain and which must therefore not be invented
// inside testdata/io_stations.json (that fixture stays faithful to the
// 2026-07-28 capture, in which both stations are normal, closed, and hold a
// 3592 magazine).
func ioStationsServing(t *testing.T, body string) *IOStationsCollector {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	log := logger.NewTextLogger("error")
	return NewIOStationsCollector(log, NewClient(srv.URL, time.Second), time.Hour)
}

// TestParseIOStations exercises parseIOStations (piece 2, the pure parser) with
// a static byte fixture: no HTTP, no collector, no logger, no goroutine
// involved.
func TestParseIOStations(t *testing.T) {
	data, err := os.ReadFile("testdata/io_stations.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	stations, err := parseIOStations(data)
	if err != nil {
		t.Fatalf("parseIOStations: %v", err)
	}

	// Two stations, which is the capture's full set rather than a trim: a
	// TS4500 base frame ships exactly two I/O stations and this library has
	// no expansion-frame pair.
	if len(stations) != 2 {
		t.Fatalf("len(stations) = %d, want 2", len(stations))
	}

	first := stations[0]
	if first.Location != "ioStation_F2IOu" {
		t.Errorf("Location = %q, want %q", first.Location, "ioStation_F2IOu")
	}
	if first.State != "normal" {
		t.Errorf("State = %q, want %q", first.State, "normal")
	}
	if first.Door == nil {
		t.Fatal("Door = nil, want a door object: a TS4500 I/O station has one")
	}
	if first.Door.Opened != "no" {
		t.Errorf("Door.Opened = %q, want %q", first.Door.Opened, "no")
	}
	if first.Magazine == nil {
		t.Fatal("Magazine = nil, want a magazine object")
	}
	if first.Magazine.MediaType != "3592" {
		t.Errorf("Magazine.MediaType = %q, want %q", first.Magazine.MediaType, "3592")
	}
	if first.Magazine.IOSlots != 16 {
		t.Errorf("Magazine.IOSlots = %d, want 16 (the 3592 magazine's capacity)", first.Magazine.IOSlots)
	}

	// The null-vs-string distinction in contentsVolser is the whole reason
	// the field is []*string. If it ever decays to []string, every empty slot
	// becomes an indistinguishable "" and the occupancy counts silently go to
	// the magazine's full capacity.
	t.Run("empty slots stay distinguishable from occupied ones", func(t *testing.T) {
		occupied, unreadable := magazineCounts(first.Magazine)
		if occupied != 4 {
			t.Errorf("occupied = %d, want 4", occupied)
		}
		if unreadable != 0 {
			t.Errorf("unreadable = %d, want 0: the capture holds no unreadable barcode", unreadable)
		}
		if len(first.Magazine.ContentsVolser) != 16 {
			t.Errorf("len(ContentsVolser) = %d, want 16: one entry per slot, occupied or not",
				len(first.Magazine.ContentsVolser))
		}
	})

	t.Run("the second station is empty, not absent", func(t *testing.T) {
		second := stations[1]
		if second.Magazine == nil {
			t.Fatal("Magazine = nil, want an inserted-but-empty magazine")
		}
		occupied, _ := magazineCounts(second.Magazine)
		if occupied != 0 {
			t.Errorf("occupied = %d, want 0", occupied)
		}
	})

	t.Run("malformed input yields an error, not a panic", func(t *testing.T) {
		if _, err := parseIOStations([]byte("not json")); err == nil {
			t.Error("parseIOStations(malformed) returned a nil error, want non-nil")
		}
	})

	t.Run("an object rather than the documented array is an error", func(t *testing.T) {
		if _, err := parseIOStations([]byte(`{"location":"ioStation_F2IOu"}`)); err == nil {
			t.Error("parseIOStations(object) returned a nil error, want non-nil")
		}
	})

	// R1.11.2: the L25 and L55 base frames "come with two I/O stations", so
	// an empty list is a response that lost its content rather than a library
	// with none. Accepting it would replace a good cache with nothing.
	t.Run("empty array is an error, not a library with no I/O stations", func(t *testing.T) {
		if _, err := parseIOStations([]byte(`[]`)); err == nil {
			t.Error("parseIOStations(empty array) returned a nil error, want non-nil")
		}
	})

	t.Run("empty input yields an error, not a panic", func(t *testing.T) {
		if _, err := parseIOStations(nil); err == nil {
			t.Error("parseIOStations(nil) returned a nil error, want non-nil")
		}
	})

	t.Run("an entry with no location is an error", func(t *testing.T) {
		if _, err := parseIOStations([]byte(`[{"state":"normal"}]`)); err == nil {
			t.Error("parseIOStations(no location) returned a nil error, want non-nil")
		}
	})

	// Fail closed, in the pure step: two entries at one location would send
	// two metrics sharing a descriptor AND a label set, and Registry.Gather
	// rejects the whole scrape when that happens, taking out every other
	// collector's metrics with it (CONTRIBUTING.md, "Common Pitfalls").
	//
	// Unlike node_cards, location alone IS the key here: a station is its
	// position, and nothing shares one.
	t.Run("duplicate locations are rejected rather than reaching Collect", func(t *testing.T) {
		dup := `[{"location":"ioStation_F2IOu","state":"normal"},{"location":"ioStation_F2IOu","state":"normal"}]`
		if _, err := parseIOStations([]byte(dup)); err == nil {
			t.Error("parseIOStations(duplicate location) returned a nil error, want non-nil")
		}
	})

	// A null door and a null magazine are both documented shapes, not
	// malformed input: the first is every Diamondback station, the second is
	// any station whose door is open or whose magazine has been removed.
	// Neither may be rejected.
	t.Run("null door and null magazine parse rather than erroring", func(t *testing.T) {
		body := `[{"location":"ioStation_C3","state":"normal","door":null,"magazine":null}]`
		got, err := parseIOStations([]byte(body))
		if err != nil {
			t.Fatalf("parseIOStations(null door and magazine): %v", err)
		}
		if got[0].Door != nil {
			t.Error("Door != nil, want nil")
		}
		if got[0].Magazine != nil {
			t.Error("Magazine != nil, want nil")
		}
	})
}

// TestIOStationsCollector_Describe locks the descriptor count at exactly 8 (the
// stateset, the door gauge, the magazine-present gauge, three magazine counts,
// the info series and the freshness gauge) so a future edit that silently adds
// or drops a metric is caught here rather than downstream in docs-check or a
// dashboard.
func TestIOStationsCollector_Describe(t *testing.T) {
	log := logger.NewTextLogger("error")
	c := NewIOStationsCollector(log, NewClient("http://example.invalid", time.Second), time.Hour)

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

// TestIOStationsCollector_Collect pins the exact exposition text of every
// family against the fixture. refresh is driven directly rather than through
// Start, so the assertion is deterministic and carries no sleep: Start's own
// scheduling is what the lifecycle tests below cover.
//
// All five documented states are emitted per station, exactly one carrying 1,
// which is what keeps the severity classification in the alerting rules rather
// than in the value.
func TestIOStationsCollector_Collect(t *testing.T) {
	_, c := ioStationsFixtureServer(t)
	c.refresh(context.Background())

	expected := `
# HELP tapelibrary_io_station_door_open Whether the I/O station's door is open (1) or closed (0). A station that reports no door at all, or a position this exporter cannot map to open-or-closed, produces no series rather than a 0 asserting a closed door.
# TYPE tapelibrary_io_station_door_open gauge
tapelibrary_io_station_door_open{location="ioStation_F2IOl"} 0
tapelibrary_io_station_door_open{location="ioStation_F2IOu"} 0
# HELP tapelibrary_io_station_info Identity of the magazine currently inserted in this I/O station, always 1. Identity strings live here rather than on a measurement series, so swapping a magazine changes this series alone instead of breaking the continuity of the occupancy counts.
# TYPE tapelibrary_io_station_info gauge
tapelibrary_io_station_info{location="ioStation_F2IOl",media_type="3592"} 1
tapelibrary_io_station_info{location="ioStation_F2IOu",media_type="3592"} 1
# HELP tapelibrary_io_station_magazine_present Whether the I/O station currently reports an inserted magazine (1) or not (0). R1.11.2 returns no magazine both when none is inserted and when the door is open, so a 0 means one of those two, not specifically an empty station.
# TYPE tapelibrary_io_station_magazine_present gauge
tapelibrary_io_station_magazine_present{location="ioStation_F2IOl"} 1
tapelibrary_io_station_magazine_present{location="ioStation_F2IOu"} 1
# HELP tapelibrary_io_station_slots Number of cartridge slots in the magazine currently inserted in this I/O station (18 for LTO, 16 for 3592). Emitted only while a magazine is reported, since a station with none has no capacity rather than a capacity of zero.
# TYPE tapelibrary_io_station_slots gauge
tapelibrary_io_station_slots{location="ioStation_F2IOl"} 16
tapelibrary_io_station_slots{location="ioStation_F2IOu"} 16
# HELP tapelibrary_io_station_slots_occupied Number of slots holding a cartridge in the magazine currently inserted in this I/O station, cartridges with an unreadable barcode included. Emitted only while a magazine is reported: a 0 from a station with no magazine would read as an empty magazine ready to accept an import.
# TYPE tapelibrary_io_station_slots_occupied gauge
tapelibrary_io_station_slots_occupied{location="ioStation_F2IOl"} 0
tapelibrary_io_station_slots_occupied{location="ioStation_F2IOu"} 4
# HELP tapelibrary_io_station_slots_unreadable Number of occupied slots whose cartridge barcode the library could not read (reported as an unknown VOLSER). A subset of tapelibrary_io_station_slots_occupied: these cartridges are present but unidentifiable, and cannot be imported until someone reseats or replaces the label.
# TYPE tapelibrary_io_station_slots_unreadable gauge
tapelibrary_io_station_slots_unreadable{location="ioStation_F2IOl"} 0
tapelibrary_io_station_slots_unreadable{location="ioStation_F2IOu"} 0
# HELP tapelibrary_io_station_state Operational state of the I/O station, as a stateset: 1 on the active state and 0 on every other known state.
# TYPE tapelibrary_io_station_state gauge
tapelibrary_io_station_state{location="ioStation_F2IOl",state="closedNoMagazine"} 0
tapelibrary_io_station_state{location="ioStation_F2IOl",state="doorOpenTooLong"} 0
tapelibrary_io_station_state{location="ioStation_F2IOl",state="failedToClose"} 0
tapelibrary_io_station_state{location="ioStation_F2IOl",state="normal"} 1
tapelibrary_io_station_state{location="ioStation_F2IOl",state="unknown"} 0
tapelibrary_io_station_state{location="ioStation_F2IOu",state="closedNoMagazine"} 0
tapelibrary_io_station_state{location="ioStation_F2IOu",state="doorOpenTooLong"} 0
tapelibrary_io_station_state{location="ioStation_F2IOu",state="failedToClose"} 0
tapelibrary_io_station_state{location="ioStation_F2IOu",state="normal"} 1
tapelibrary_io_station_state{location="ioStation_F2IOu",state="unknown"} 0
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected), ioStationsMetricNames...); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}

	// 22 cached metrics (2 stations x (5 documented states + door + magazine
	// present + slots + occupied + unreadable + info)) plus the freshness
	// gauge Collect always appends. Like power_supplies and node_cards, this
	// fixture is the library's full complement rather than a trim, so 23 is
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
	if count != 23 {
		t.Fatalf("GatherAndCount = %d, want 23 (2 stations x 11 + freshness)", count)
	}
}

// TestIOStationsCollector_DoorOpen covers the state the IOStationDoorOpen rule
// reads. Both stations in the 2026-07-28 capture are closed, so the fixture
// cannot exercise this path and a synthetic body must: without it, one of the
// two signals this collector exists to provide would ship untested.
//
// The magazine is null here because that is what the library actually sends
// with the door open (R1.11.2), which makes this also the proof that an open
// door does not silently report an empty magazine as a full one's absence.
func TestIOStationsCollector_DoorOpen(t *testing.T) {
	c := ioStationsServing(t, `[{"location":"ioStation_F1IOu","state":"doorOpenTooLong","door":{"opened":"yes"},"magazine":null}]`)
	c.refresh(context.Background())

	expected := `
# HELP tapelibrary_io_station_door_open Whether the I/O station's door is open (1) or closed (0). A station that reports no door at all, or a position this exporter cannot map to open-or-closed, produces no series rather than a 0 asserting a closed door.
# TYPE tapelibrary_io_station_door_open gauge
tapelibrary_io_station_door_open{location="ioStation_F1IOu"} 1
# HELP tapelibrary_io_station_magazine_present Whether the I/O station currently reports an inserted magazine (1) or not (0). R1.11.2 returns no magazine both when none is inserted and when the door is open, so a 0 means one of those two, not specifically an empty station.
# TYPE tapelibrary_io_station_magazine_present gauge
tapelibrary_io_station_magazine_present{location="ioStation_F1IOu"} 0
# HELP tapelibrary_io_station_state Operational state of the I/O station, as a stateset: 1 on the active state and 0 on every other known state.
# TYPE tapelibrary_io_station_state gauge
tapelibrary_io_station_state{location="ioStation_F1IOu",state="closedNoMagazine"} 0
tapelibrary_io_station_state{location="ioStation_F1IOu",state="doorOpenTooLong"} 1
tapelibrary_io_station_state{location="ioStation_F1IOu",state="failedToClose"} 0
tapelibrary_io_station_state{location="ioStation_F1IOu",state="normal"} 0
tapelibrary_io_station_state{location="ioStation_F1IOu",state="unknown"} 0
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected), ioStationsMetricNames...); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}
}

// TestIOStationsCollector_NoMagazineEmitsNoSlotSeries pins the decision that a
// station with no magazine reports magazine_present=0 and NOTHING else about
// its slots.
//
// A 0 occupancy would be the dangerous alternative: it reads as "an empty
// magazine, ready to accept an import", which is the exact opposite of the
// truth. The capacity, occupancy, unreadable and info series must all be absent
// rather than zeroed.
func TestIOStationsCollector_NoMagazineEmitsNoSlotSeries(t *testing.T) {
	c := ioStationsServing(t, `[{"location":"ioStation_F1IOl","state":"closedNoMagazine","door":{"opened":"no"},"magazine":null}]`)
	c.refresh(context.Background())

	reg := prometheus.NewRegistry()
	if err := reg.Register(c); err != nil {
		t.Fatalf("Register: %v", err)
	}

	for _, name := range []string{
		"tapelibrary_io_station_slots",
		"tapelibrary_io_station_slots_occupied",
		"tapelibrary_io_station_slots_unreadable",
		"tapelibrary_io_station_info",
	} {
		count, err := testutil.GatherAndCount(reg, name)
		if err != nil {
			t.Fatalf("GatherAndCount(%s): %v", name, err)
		}
		if count != 0 {
			t.Errorf("GatherAndCount(%s) = %d, want 0: a station with no magazine has no slots, not zero slots", name, count)
		}
	}

	expected := `
# HELP tapelibrary_io_station_magazine_present Whether the I/O station currently reports an inserted magazine (1) or not (0). R1.11.2 returns no magazine both when none is inserted and when the door is open, so a 0 means one of those two, not specifically an empty station.
# TYPE tapelibrary_io_station_magazine_present gauge
tapelibrary_io_station_magazine_present{location="ioStation_F1IOl"} 0
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected), "tapelibrary_io_station_magazine_present"); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}
}

// TestIOStationsCollector_UnreadableBarcodeIsCounted covers the one signal on
// this endpoint that comes from R1.11.2's contract rather than from the
// capture: contentsVolser carries the literal string "unknown" for a cartridge
// whose barcode the library could not read.
//
// Such a cartridge is PRESENT (so it occupies a slot and counts toward
// occupancy) but unidentifiable (so it cannot be imported). Counting it as an
// empty slot would make a jammed I/O station look like it had room.
func TestIOStationsCollector_UnreadableBarcodeIsCounted(t *testing.T) {
	body := `[{"location":"ioStation_F1IOu","state":"normal","door":{"opened":"no"},` +
		`"magazine":{"mediaType":"LTO","ioSlots":3,"contentsVolser":["TST001L9","unknown",null]}}]`
	c := ioStationsServing(t, body)
	c.refresh(context.Background())

	expected := `
# HELP tapelibrary_io_station_slots Number of cartridge slots in the magazine currently inserted in this I/O station (18 for LTO, 16 for 3592). Emitted only while a magazine is reported, since a station with none has no capacity rather than a capacity of zero.
# TYPE tapelibrary_io_station_slots gauge
tapelibrary_io_station_slots{location="ioStation_F1IOu"} 3
# HELP tapelibrary_io_station_slots_occupied Number of slots holding a cartridge in the magazine currently inserted in this I/O station, cartridges with an unreadable barcode included. Emitted only while a magazine is reported: a 0 from a station with no magazine would read as an empty magazine ready to accept an import.
# TYPE tapelibrary_io_station_slots_occupied gauge
tapelibrary_io_station_slots_occupied{location="ioStation_F1IOu"} 2
# HELP tapelibrary_io_station_slots_unreadable Number of occupied slots whose cartridge barcode the library could not read (reported as an unknown VOLSER). A subset of tapelibrary_io_station_slots_occupied: these cartridges are present but unidentifiable, and cannot be imported until someone reseats or replaces the label.
# TYPE tapelibrary_io_station_slots_unreadable gauge
tapelibrary_io_station_slots_unreadable{location="ioStation_F1IOu"} 1
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected),
		"tapelibrary_io_station_slots",
		"tapelibrary_io_station_slots_occupied",
		"tapelibrary_io_station_slots_unreadable",
	); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}
}

// TestIOStationsCollector_AbsentDoorEmitsNoSeries covers the two inputs for
// which a door gauge must not exist at all, rather than reporting 0.
//
// A 0 asserts "this door is present and closed". For a Diamondback station
// (which R1.11.2 says has no door of its own) that is false, and for a position
// nobody has interpreted it is an unexamined guess. Either way it is exactly
// how a door alert stops firing without anyone noticing — the same failure the
// frames collector's legacy door pseudo-states turned out to have.
func TestIOStationsCollector_AbsentDoorEmitsNoSeries(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{
			name: "a station reporting no door at all",
			body: `[{"location":"ioStation_C3","state":"normal","door":null,"magazine":null}]`,
		},
		{
			name: "a door position outside the documented yes/no pair",
			body: `[{"location":"ioStation_F1IOu","state":"normal","door":{"opened":"ajar"},"magazine":null}]`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := ioStationsServing(t, tc.body)
			c.refresh(context.Background())

			reg := prometheus.NewRegistry()
			if err := reg.Register(c); err != nil {
				t.Fatalf("Register: %v", err)
			}
			count, err := testutil.GatherAndCount(reg, "tapelibrary_io_station_door_open")
			if err != nil {
				t.Fatalf("GatherAndCount: %v", err)
			}
			if count != 0 {
				t.Fatalf("GatherAndCount(door_open) = %d, want 0: a 0 would assert a closed door that was never observed", count)
			}

			// The station itself is still reported: only the door series is
			// withheld, never the whole entry.
			stateCount, err := testutil.GatherAndCount(reg, "tapelibrary_io_station_state")
			if err != nil {
				t.Fatalf("GatherAndCount: %v", err)
			}
			if stateCount != len(ioStationStates) {
				t.Fatalf("GatherAndCount(state) = %d, want %d: withholding the door must not drop the station",
					stateCount, len(ioStationStates))
			}
		})
	}
}

// TestIOStationsCollector_UndocumentedStateIsStillEmitted covers the manual's
// own incompleteness: it names states in prose that appear in none of its
// tables. A state outside the documented set must surface as its own series at
// 1, rather than leaving all five documented series at 0 and making the station
// look stateless — which would also silently satisfy nothing at all, so the
// signal would be lost rather than merely mislabelled.
func TestIOStationsCollector_UndocumentedStateIsStillEmitted(t *testing.T) {
	c := ioStationsServing(t, `[{"location":"ioStation_F1IOu","state":"someUndocumentedState","door":{"opened":"no"},"magazine":null}]`)
	c.refresh(context.Background())

	expected := `
# HELP tapelibrary_io_station_state Operational state of the I/O station, as a stateset: 1 on the active state and 0 on every other known state.
# TYPE tapelibrary_io_station_state gauge
tapelibrary_io_station_state{location="ioStation_F1IOu",state="closedNoMagazine"} 0
tapelibrary_io_station_state{location="ioStation_F1IOu",state="doorOpenTooLong"} 0
tapelibrary_io_station_state{location="ioStation_F1IOu",state="failedToClose"} 0
tapelibrary_io_station_state{location="ioStation_F1IOu",state="normal"} 0
tapelibrary_io_station_state{location="ioStation_F1IOu",state="someUndocumentedState"} 1
tapelibrary_io_station_state{location="ioStation_F1IOu",state="unknown"} 0
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected), "tapelibrary_io_station_state"); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}
}

// TestIOStationsCollector_DoneClosesOnCancel verifies the Done() channel closes
// when the context passed to Start is cancelled. This is the mechanism main.go's
// shutdown seam relies on (see registry.Wait after web.ListenAndServe).
func TestIOStationsCollector_DoneClosesOnCancel(t *testing.T) {
	log := logger.NewTextLogger("error")
	c := NewIOStationsCollector(log, NewClient("http://example.invalid", time.Second), time.Hour)
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

// TestIOStationsCollector_CollectServesCacheWithoutIO proves Collect never calls
// the library: after Start's own immediate refresh completes, a long interval (1
// hour) guarantees the ticker cannot fire again during this test, so any further
// request the server receives could only come from Collect itself calling out,
// which the design forbids.
func TestIOStationsCollector_CollectServesCacheWithoutIO(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.ServeFile(w, r, "testdata/io_stations.json")
	}))
	defer srv.Close()

	log := logger.NewTextLogger("error")
	c := NewIOStationsCollector(log, NewClient(srv.URL, time.Second), time.Hour)
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

// TestIOStationsCollector_ErrorHandling drives a refresh against a library that
// only ever fails. No cache was ever filled, so the scrape must carry exactly
// the freshness gauge, and must neither panic nor emit a partial stateset.
func TestIOStationsCollector_ErrorHandling(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	log := logger.NewTextLogger("error")
	c := NewIOStationsCollector(log, NewClient(srv.URL, time.Second), time.Hour)
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

// TestIOStationsCollector_ErrorKeepsPreviousCache scripts the backend to succeed
// once, then fail on every later call, and drives at least one more refresh via
// a short interval. The cache from the successful first refresh must survive the
// later failure (fail-open, per refresh's doc comment) rather than being cleared
// or replaced with nothing.
//
// A cleared cache here would drop tapelibrary_io_station_door_open entirely, and
// an absent series cannot satisfy IOStationDoorOpen's `== 1`, so a library that
// became unreachable would silently stop being watched for a door left open
// rather than alerting.
func TestIOStationsCollector_ErrorKeepsPreviousCache(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			http.ServeFile(w, r, "testdata/io_stations.json")
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	log := logger.NewTextLogger("error")
	c := NewIOStationsCollector(log, NewClient(srv.URL, time.Second), 20*time.Millisecond)
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
	// A surviving cache emits 23 metrics: the 22 cached from the successful
	// first refresh plus the freshness gauge. A wrongly-cleared cache would
	// emit only 1 (the gauge alone), which "> 0" cannot tell apart.
	if count != 23 {
		t.Fatalf("GatherAndCount = %d, want 23: the previous cache must survive a later refresh error", count)
	}
}

// TestIOStationsCollector_StatusTrackerSuccessOnFirstScrape covers the startup
// window before Start's first refresh has completed (Start is deliberately never
// called here). Collect must still emit exactly the freshness gauge, valued 0
// (not a zero time.Time's large-negative Unix()), and StatusTracker must still
// report this collector as successful: "Collect ran and returned data" and "the
// data is fresh" are different questions.
func TestIOStationsCollector_StatusTrackerSuccessOnFirstScrape(t *testing.T) {
	log := logger.NewTextLogger("error")
	c := NewIOStationsCollector(log, NewClient("http://example.invalid", time.Second), time.Hour)
	// Do NOT call Start(): cache is empty, no refresh has ever run.

	expected := `
# HELP tapelibrary_io_stations_last_refresh_timestamp_seconds Unix time of the last successful io stations refresh. Alert if time() - this > 2 x the collector's configured interval.
# TYPE tapelibrary_io_stations_last_refresh_timestamp_seconds gauge
tapelibrary_io_stations_last_refresh_timestamp_seconds 0
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected)); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}

	tracker := NewStatusTracker(log)
	tracker.Add("io_stations", c)

	expectedSuccess := `
# HELP ` + statusTrackerSuccessMetric + ` Whether the last scrape of the collector succeeded (1=success, 0=failure)
# TYPE ` + statusTrackerSuccessMetric + ` gauge
` + statusTrackerSuccessMetric + `{collector="io_stations"} 1
`
	if err := testutil.CollectAndCompare(tracker, strings.NewReader(expectedSuccess), statusTrackerSuccessMetric); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}
}
