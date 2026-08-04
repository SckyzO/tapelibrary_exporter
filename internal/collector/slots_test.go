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

// slotsFixtureServer serves testdata/slots.json on every request and returns a
// collector already pointed at it. Nothing is started: the caller decides
// whether to drive refresh directly (deterministic) or via Start (which is what
// the lifecycle tests below exercise).
func slotsFixtureServer(t *testing.T, perSlot bool) (*httptest.Server, *SlotsCollector) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "testdata/slots.json")
	}))
	t.Cleanup(srv.Close)

	log := logger.NewTextLogger("error")
	return srv, NewSlotsCollector(log, NewClient(srv.URL, time.Second), time.Hour, perSlot)
}

// slotsServing returns a collector fed by a server that answers every request
// with body. Used by the branch tests below, which need input shapes the real
// capture does not contain and which must therefore not be invented inside
// testdata/slots.json (that fixture stays faithful to the 2026-07-28 capture,
// in which every 4-tier slot is either wholly full or wholly empty and no
// barcode is unreadable).
func slotsServing(t *testing.T, body string, perSlot bool) *SlotsCollector {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	log := logger.NewTextLogger("error")
	return NewSlotsCollector(log, NewClient(srv.URL, time.Second), time.Hour, perSlot)
}

// slotsScalar gathers c and returns the value of the single, unlabelled series
// in the named metric family.
//
// testutil.ToFloat64 cannot be used here: it requires the collector to emit
// exactly one metric in total, and this one emits at least thirteen. The
// alternative is a full CollectAndCompare block per assertion, which buries a
// one-number check under a paragraph of help text and makes an unrelated
// wording edit look like an arithmetic failure. Fails the test rather than
// returning a zero value, so a typo'd metric name cannot pass as a legitimate 0.
func slotsScalar(t *testing.T, c prometheus.Collector, name string) float64 {
	t.Helper()

	reg := prometheus.NewRegistry()
	if err := reg.Register(c); err != nil {
		t.Fatalf("Register: %v", err)
	}
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for _, mf := range families {
		if mf.GetName() != name {
			continue
		}
		if n := len(mf.GetMetric()); n != 1 {
			t.Fatalf("%s carries %d series, want exactly 1 (slotsScalar reads unlabelled gauges only)", name, n)
		}
		return mf.GetMetric()[0].GetGauge().GetValue()
	}
	t.Fatalf("%s is absent from the gathered output", name)
	return 0
}

// TestParseSlots exercises parseSlots (piece 2, the pure parser) with a static
// byte fixture: no HTTP, no collector, no logger, no goroutine involved.
func TestParseSlots(t *testing.T) {
	data, err := os.ReadFile("testdata/slots.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	slots, err := parseSlots(data)
	if err != nil {
		t.Fatalf("parseSlots: %v", err)
	}

	// Eight slots, an eight-entry trim of the 79-slot capture chosen to carry
	// both documented states at both tier depths this fleet runs, with occupied
	// and empty examples of each.
	if len(slots) != 8 {
		t.Fatalf("len(slots) = %d, want 8", len(slots))
	}

	first := slots[0]
	if first.Location != "slot_F1C9R2" {
		t.Errorf("Location = %q, want %q", first.Location, "slot_F1C9R2")
	}
	if first.State != "normal" {
		t.Errorf("State = %q, want %q", first.State, "normal")
	}
	if first.Tiers != 4 {
		t.Errorf("Tiers = %d, want 4", first.Tiers)
	}
	if first.Puts != 1163 || first.PutRetries != 0 || first.GetRetries != 1 {
		t.Errorf("counters = (%v, %v, %v), want (1163, 0, 1)", first.Puts, first.PutRetries, first.GetRetries)
	}

	// The single most important property of this endpoint, and the one that
	// separates it from every other collector here: a slot entry is a COLUMN.
	// Its location carries no tier suffix, and the cartridges it holds are in
	// the contents array rather than at locations of their own. A future edit
	// that started keying this collector on a tiered location, or joining it
	// naively to DataCartridgesCollector, breaks here first.
	t.Run("a slot location is un-tiered and describes tiers stacked positions", func(t *testing.T) {
		positions := 0
		for i := range slots {
			s := &slots[i]
			if strings.Contains(strings.TrimPrefix(s.Location, "slot_"), "T") {
				t.Errorf("Location %q carries a tier suffix: /v1/slots reports columns, only /v1/dataCartridges reports tiered positions", s.Location)
			}
			if got := s.positions(); got != s.Tiers {
				t.Errorf("%s: positions() = %d, want %d (contents must hold one entry per tier)", s.Location, got, s.Tiers)
			}
			positions += s.positions()
		}
		// Eight slots, twenty positions: four 4-tier slots and four 1-tier
		// ones. Counting entries instead of positions would report a library
		// 60% smaller than it is.
		if positions != 20 {
			t.Errorf("total positions = %d, want 20 across 8 slots", positions)
		}
	})

	t.Run("null contents entries are empty tiers, not cartridges", func(t *testing.T) {
		// slot_F1C3R10 is a 4-tier slot with every tier null.
		empty := slots[1]
		if empty.Location != "slot_F1C3R10" {
			t.Fatalf("fixture order changed: slots[1] is %q", empty.Location)
		}
		occupied, unreadable := empty.occupancy()
		if occupied != 0 || unreadable != 0 {
			t.Errorf("occupancy() = (%d, %d), want (0, 0)", occupied, unreadable)
		}
		if empty.positions() != 4 {
			t.Errorf("positions() = %d, want 4: an empty tier still is a position", empty.positions())
		}
	})

	t.Run("malformed input yields an error, not a panic", func(t *testing.T) {
		if _, err := parseSlots([]byte("not json")); err == nil {
			t.Error("parseSlots(malformed) returned a nil error, want non-nil")
		}
	})

	t.Run("an object rather than the documented array is an error", func(t *testing.T) {
		if _, err := parseSlots([]byte(`{"location":"slot_F1C1R1"}`)); err == nil {
			t.Error("parseSlots(object) returned a nil error, want non-nil")
		}
	})

	// Every TS4500 has storage slots — 10 730 across this library's twelve
	// frames — so an empty list is a response that lost its content over the
	// slow SCSI/LCC path rather than a library running without any. Accepting
	// it would replace a good cache with nothing and drive
	// tapelibrary_slots_positions_available to 0, which SlotsNearlyFull reads.
	t.Run("empty array is an error, not a library with no slots", func(t *testing.T) {
		if _, err := parseSlots([]byte(`[]`)); err == nil {
			t.Error("parseSlots(empty array) returned a nil error, want non-nil")
		}
	})

	t.Run("empty input yields an error, not a panic", func(t *testing.T) {
		if _, err := parseSlots(nil); err == nil {
			t.Error("parseSlots(nil) returned a nil error, want non-nil")
		}
	})

	t.Run("an entry with no location is an error", func(t *testing.T) {
		if _, err := parseSlots([]byte(`[{"state":"normal","contents":[null],"tiers":1}]`)); err == nil {
			t.Error("parseSlots(no location) returned a nil error, want non-nil")
		}
	})

	// Fail closed, in the pure step: two entries at one location would send two
	// metrics sharing a descriptor AND a label set once --collector.slots.per-slot
	// is on, and Registry.Gather rejects the whole scrape when that happens,
	// taking out every other collector's metrics with it (CONTRIBUTING.md,
	// "Common Pitfalls").
	t.Run("duplicate locations are rejected rather than reaching Collect", func(t *testing.T) {
		dup := `[{"location":"slot_F1C1R1","state":"normal","contents":[null],"tiers":1},{"location":"slot_F1C1R1","state":"normal","contents":[null],"tiers":1}]`
		if _, err := parseSlots([]byte(dup)); err == nil {
			t.Error("parseSlots(duplicate location) returned a nil error, want non-nil")
		}
	})
}

// TestSlotsCollector_Describe locks the descriptor count at exactly 15 (nine
// library-wide families, five per-slot ones and the freshness gauge) so a
// future edit that silently adds or drops a metric is caught here rather than
// downstream in docs-check or a dashboard.
func TestSlotsCollector_Describe(t *testing.T) {
	log := logger.NewTextLogger("error")
	c := NewSlotsCollector(log, NewClient("http://example.invalid", time.Second), time.Hour, false)

	ch := make(chan *prometheus.Desc, 40)
	c.Describe(ch)
	close(ch)

	count := 0
	for range ch {
		count++
	}
	if count != 15 {
		t.Fatalf("Describe sent %d descriptors, want 15", count)
	}
}

// TestSlotsCollector_DescribeIsConstantWithoutPerSlot pins that a descriptor is
// what this collector CAN emit rather than what it did last time: the five
// per-slot descriptors are described with the flag off, which is what lets
// docs/metrics.md document them unconditionally and what keeps docs-check from
// flagging them as undocumented.
func TestSlotsCollector_DescribeIsConstantWithoutPerSlot(t *testing.T) {
	log := logger.NewTextLogger("error")
	count := func(perSlot bool) int {
		c := NewSlotsCollector(log, NewClient("http://example.invalid", time.Second), time.Hour, perSlot)
		ch := make(chan *prometheus.Desc, 40)
		c.Describe(ch)
		close(ch)
		n := 0
		for range ch {
			n++
		}
		return n
	}
	if off, on := count(false), count(true); off != on {
		t.Fatalf("Describe sent %d descriptors with per-slot off and %d with it on; the set must not depend on the flag", off, on)
	}
}

// TestSlotsCollector_Collect pins the exact exposition text of every
// library-wide family against the fixture. refresh is driven directly rather
// than through Start, so the assertion is deterministic and carries no sleep:
// Start's own scheduling is what the lifecycle tests below cover.
//
// The freshness gauge is excluded from the comparison (its value is the wall
// clock) but is counted in the GatherAndCount assertion at the end.
func TestSlotsCollector_Collect(t *testing.T) {
	_, c := slotsFixtureServer(t, false)
	c.refresh(context.Background())

	expected := `
# HELP tapelibrary_slots Number of storage slots in each documented state. A slot is a column holding one or more stacked tiers, not a single cartridge position: use tapelibrary_slots_positions for capacity. Both documented states are always emitted, so a rule matching one still has a series to read when its count reaches zero.
# TYPE tapelibrary_slots gauge
tapelibrary_slots{state="inServiceMode"} 4
tapelibrary_slots{state="normal"} 4
# HELP tapelibrary_slots_depth Number of storage slots of each tier depth: 1 for a single-deep slot, up to 5 for a high-density one. Only depths the library actually reports are emitted, since nothing alerts on a particular depth. This is the library's physical geometry, so it changes only when a frame is added or removed.
# TYPE tapelibrary_slots_depth gauge
tapelibrary_slots_depth{tiers="1"} 4
tapelibrary_slots_depth{tiers="4"} 4
# HELP tapelibrary_slots_get_retries_total Total number of retries required while getting a cartridge from a storage slot, over the lifetime of the library's slots, summed across every slot. This endpoint reports no matching gets counter, so unlike the put side there is no denominator available and no ratio can be formed. Summed, so removing a frame reads as a counter reset.
# TYPE tapelibrary_slots_get_retries_total counter
tapelibrary_slots_get_retries_total 33
# HELP tapelibrary_slots_positions Number of cartridge positions across the library's storage slots, by the state of the slot holding them. One high-density slot contributes up to five positions. Both documented states are always emitted.
# TYPE tapelibrary_slots_positions gauge
tapelibrary_slots_positions{state="inServiceMode"} 10
tapelibrary_slots_positions{state="normal"} 10
# HELP tapelibrary_slots_positions_available Number of empty cartridge positions the library can actually put a cartridge into: free positions in slots whose state is normal. Positions in a slot placed in service mode are excluded, because R1.11.2 defines that state as one the robot may not select as a destination even though cartridges can still be moved out of it. This is the figure SlotsNearlyFull reads, and it is not derivable from LibraryCollector's capacity gauges, which know nothing about slot state.
# TYPE tapelibrary_slots_positions_available gauge
tapelibrary_slots_positions_available 5
# HELP tapelibrary_slots_positions_occupied Number of cartridge positions holding a cartridge, by the state of the slot holding them. Cartridges whose barcode the library could not read are counted here too, and again in tapelibrary_slots_positions_unreadable. Both documented states are always emitted.
# TYPE tapelibrary_slots_positions_occupied gauge
tapelibrary_slots_positions_occupied{state="inServiceMode"} 9
tapelibrary_slots_positions_occupied{state="normal"} 5
# HELP tapelibrary_slots_positions_unreadable Number of occupied cartridge positions whose cartridge barcode the library could not read (reported as an unknown VOLSER). A subset of tapelibrary_slots_positions_occupied. Such a cartridge occupies its position and carries no volser, so it appears in no other collector in this exporter. R1.11.2 documents the unknown token on the I/O station endpoint and not on this one, so on firmware that never reports it this gauge stays at 0.
# TYPE tapelibrary_slots_positions_unreadable gauge
tapelibrary_slots_positions_unreadable 0
# HELP tapelibrary_slots_put_retries_total Total number of retries required while placing a cartridge into a storage slot, over the lifetime of the library's slots, summed across every slot. Zero on all 79 slots of the 2026-07-28 capture against 25 023 lifetime puts, so any sustained non-zero rate against tapelibrary_slots_puts_total is a robotics degradation signal rather than normal wear. Summed, so removing a frame reads as a counter reset.
# TYPE tapelibrary_slots_put_retries_total counter
tapelibrary_slots_put_retries_total 0
# HELP tapelibrary_slots_puts_total Total number of times a cartridge has been placed into a storage slot over the lifetime of the library's slots, summed across every slot. Summed rather than emitted per slot to keep the default build affordable, so removing a frame drops its slots out of the sum and reads as a counter reset. This is the denominator of the put-retry ratio SlotPutRetryRateHigh reads.
# TYPE tapelibrary_slots_puts_total counter
tapelibrary_slots_puts_total 6384
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected),
		"tapelibrary_slots",
		"tapelibrary_slots_depth",
		"tapelibrary_slots_get_retries_total",
		"tapelibrary_slots_positions",
		"tapelibrary_slots_positions_available",
		"tapelibrary_slots_positions_occupied",
		"tapelibrary_slots_positions_unreadable",
		"tapelibrary_slots_put_retries_total",
		"tapelibrary_slots_puts_total",
	); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}

	// 13 cached aggregates plus the freshness gauge Collect always appends: 2
	// states x 3 families, 2 tier depths, available, unreadable and the three
	// counters. This is the whole default cost of the collector, independent of
	// how many slots the library holds, which is the point of aggregating.
	reg := prometheus.NewRegistry()
	if err := reg.Register(c); err != nil {
		t.Fatalf("Register: %v", err)
	}
	count, err := testutil.GatherAndCount(reg)
	if err != nil {
		t.Fatalf("GatherAndCount: %v", err)
	}
	if count != 14 {
		t.Fatalf("GatherAndCount = %d, want 14 (13 aggregates + freshness)", count)
	}
}

// TestSlotsCollector_AvailableExcludesServiceModePositions is the regression
// test for the one number this collector exists to produce. The fixture's
// inServiceMode slots hold 10 positions of which 9 are occupied, so a naive
// "every free position" count would report 6 available. R1.11.2 defines
// inServiceMode as a state in which the slot "cannot be selected as a cartridge
// destination", so the correct answer is 5 — and the difference is exactly what
// makes this metric worth having next to LibraryCollector's capacity gauges,
// which know nothing about slot state.
func TestSlotsCollector_AvailableExcludesServiceModePositions(t *testing.T) {
	_, c := slotsFixtureServer(t, false)
	c.refresh(context.Background())

	if got := slotsScalar(t, c, "tapelibrary_slots_positions_available"); got != 5 {
		t.Fatalf("tapelibrary_slots_positions_available = %v, want 5", got)
	}
}

// TestSlotsCollector_PartiallyOccupiedSlot covers a high-density slot with some
// tiers full and some empty. Every 4-tier slot in the 2026-07-28 capture is
// either wholly full or wholly empty, so the fixture cannot exercise this path
// and a synthetic body must: without it, an occupancy count that treated a slot
// as all-or-nothing would pass every other test here.
func TestSlotsCollector_PartiallyOccupiedSlot(t *testing.T) {
	body := `[{"location":"slot_F1C1R1","state":"normal","contents":["TST001JD",null,"TST002JD",null],"puts":10,"putRetries":0,"getRetries":0,"tiers":4}]`
	c := slotsServing(t, body, false)
	c.refresh(context.Background())

	expected := `
# HELP tapelibrary_slots_positions_occupied Number of cartridge positions holding a cartridge, by the state of the slot holding them. Cartridges whose barcode the library could not read are counted here too, and again in tapelibrary_slots_positions_unreadable. Both documented states are always emitted.
# TYPE tapelibrary_slots_positions_occupied gauge
tapelibrary_slots_positions_occupied{state="inServiceMode"} 0
tapelibrary_slots_positions_occupied{state="normal"} 2
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected), "tapelibrary_slots_positions_occupied"); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}
	if got := slotsScalar(t, c, "tapelibrary_slots_positions_available"); got != 2 {
		t.Fatalf("tapelibrary_slots_positions_available = %v, want 2", got)
	}
}

// TestSlotsCollector_UnreadableBarcodeIsOccupied covers the unknown VOLSER
// token. Such a cartridge is present, so it must count as occupied and reduce
// the available capacity — counting it as an empty tier would advertise a
// position the robot cannot use. It is additionally counted in its own gauge,
// because it appears in no other collector in this exporter: it has no volser,
// so DataCartridgesCollector cannot see it.
func TestSlotsCollector_UnreadableBarcodeIsOccupied(t *testing.T) {
	body := `[{"location":"slot_F1C1R1","state":"normal","contents":["unknown",null],"puts":0,"putRetries":0,"getRetries":0,"tiers":2}]`
	c := slotsServing(t, body, false)
	c.refresh(context.Background())

	if got := slotsScalar(t, c, "tapelibrary_slots_positions_unreadable"); got != 1 {
		t.Fatalf("tapelibrary_slots_positions_unreadable = %v, want 1", got)
	}
	if got := slotsScalar(t, c, "tapelibrary_slots_positions_available"); got != 1 {
		t.Fatalf("tapelibrary_slots_positions_available = %v, want 1: an unreadable cartridge occupies its position", got)
	}
}

// TestSlotsCollector_EmptyStringContentsIsAnEmptyTier pins the defensive fold
// IOStationsCollector already applies: "" is not a documented value, and
// counting an unnamed tier as occupied would understate the free capacity
// SlotsNearlyFull reads.
func TestSlotsCollector_EmptyStringContentsIsAnEmptyTier(t *testing.T) {
	body := `[{"location":"slot_F1C1R1","state":"normal","contents":["",null],"puts":0,"putRetries":0,"getRetries":0,"tiers":2}]`
	c := slotsServing(t, body, false)
	c.refresh(context.Background())

	if got := slotsScalar(t, c, "tapelibrary_slots_positions_available"); got != 2 {
		t.Fatalf("tapelibrary_slots_positions_available = %v, want 2", got)
	}
	if got := slotsScalar(t, c, "tapelibrary_slots_positions_unreadable"); got != 0 {
		t.Fatalf("tapelibrary_slots_positions_unreadable = %v, want 0", got)
	}
}

// TestSlotsCollector_UndocumentedStateIsStillCounted covers the manual's own
// incompleteness. /v1/slots has the shortest state table in the API — two
// values — which makes an untabulated third more likely rather than less, and a
// state outside the documented set must surface as its own series rather than
// leaving both documented series at 0 and making the library look slotless.
func TestSlotsCollector_UndocumentedStateIsStillCounted(t *testing.T) {
	body := `[{"location":"slot_F1C1R1","state":"someUndocumentedState","contents":["TST001JD"],"puts":0,"putRetries":0,"getRetries":0,"tiers":1}]`
	c := slotsServing(t, body, false)
	c.refresh(context.Background())

	expected := `
# HELP tapelibrary_slots Number of storage slots in each documented state. A slot is a column holding one or more stacked tiers, not a single cartridge position: use tapelibrary_slots_positions for capacity. Both documented states are always emitted, so a rule matching one still has a series to read when its count reaches zero.
# TYPE tapelibrary_slots gauge
tapelibrary_slots{state="inServiceMode"} 0
tapelibrary_slots{state="normal"} 0
tapelibrary_slots{state="someUndocumentedState"} 1
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected), "tapelibrary_slots"); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}

	// An undocumented state is not normal, so its free positions are NOT
	// advertised as available: only the state the manual defines as usable
	// counts towards what the robot may target.
	if got := slotsScalar(t, c, "tapelibrary_slots_positions_available"); got != 0 {
		t.Fatalf("tapelibrary_slots_positions_available = %v, want 0", got)
	}
}

// TestSlotsCollector_EmptyStateIsCountedNowhere pins that a slot reporting no
// state at all is counted in no state series rather than emitted under
// state="", a label no operator can act on.
func TestSlotsCollector_EmptyStateIsCountedNowhere(t *testing.T) {
	body := `[{"location":"slot_F1C1R1","state":"","contents":["TST001JD"],"puts":0,"putRetries":0,"getRetries":0,"tiers":1}]`
	c := slotsServing(t, body, false)
	c.refresh(context.Background())

	// Exactly the two documented states, both at zero: no third series under
	// an empty label value.
	if got := testutil.CollectAndCount(c, "tapelibrary_slots"); got != 2 {
		t.Fatalf("tapelibrary_slots series = %d, want 2 (the documented states only)", got)
	}
}

// TestSlotsCollector_TierMismatchCountsFromContents pins which of the two
// fields wins when they disagree. Capacity is counted over contents, so the
// available-position subtraction is over one array on both sides and cannot go
// negative; the declared tiers field still drives the depth distribution, which
// is what makes the disagreement visible on the wire as well as in the log.
func TestSlotsCollector_TierMismatchCountsFromContents(t *testing.T) {
	body := `[{"location":"slot_F1C1R1","state":"normal","contents":[null,null],"puts":0,"putRetries":0,"getRetries":0,"tiers":4}]`
	c := slotsServing(t, body, false)
	c.refresh(context.Background())

	expected := `
# HELP tapelibrary_slots_depth Number of storage slots of each tier depth: 1 for a single-deep slot, up to 5 for a high-density one. Only depths the library actually reports are emitted, since nothing alerts on a particular depth. This is the library's physical geometry, so it changes only when a frame is added or removed.
# TYPE tapelibrary_slots_depth gauge
tapelibrary_slots_depth{tiers="4"} 1
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected), "tapelibrary_slots_depth"); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}
	if got := slotsScalar(t, c, "tapelibrary_slots_positions_available"); got != 2 {
		t.Fatalf("tapelibrary_slots_positions_available = %v, want 2 (counted from contents, not from tiers)", got)
	}
}

// TestSlotsCollector_PerSlotAddsPerSlotSeries drives the collector with
// --collector.slots.per-slot on and pins two of the five per-slot families
// exactly, plus the total series count.
func TestSlotsCollector_PerSlotAddsPerSlotSeries(t *testing.T) {
	_, c := slotsFixtureServer(t, true)
	c.refresh(context.Background())

	expected := `
# HELP tapelibrary_slot_get_retries_total Number of retries required while getting a cartridge from this individual storage slot, over its lifetime. Emitted only when --collector.slots.per-slot is set.
# TYPE tapelibrary_slot_get_retries_total counter
tapelibrary_slot_get_retries_total{location="slot_F11C2R2"} 8
tapelibrary_slot_get_retries_total{location="slot_F1C3R10"} 0
tapelibrary_slot_get_retries_total{location="slot_F1C4R1"} 8
tapelibrary_slot_get_retries_total{location="slot_F1C4R3"} 10
tapelibrary_slot_get_retries_total{location="slot_F1C9R2"} 1
tapelibrary_slot_get_retries_total{location="slot_F2C1R1"} 5
tapelibrary_slot_get_retries_total{location="slot_F5C7R14"} 0
tapelibrary_slot_get_retries_total{location="slot_F6C2R4"} 1
# HELP tapelibrary_slot_positions_occupied Number of tiers in this individual storage slot holding a cartridge. Emitted only when --collector.slots.per-slot is set, which it is not by default. The slot's total depth is on tapelibrary_slot_info's tiers label.
# TYPE tapelibrary_slot_positions_occupied gauge
tapelibrary_slot_positions_occupied{location="slot_F11C2R2"} 0
tapelibrary_slot_positions_occupied{location="slot_F1C3R10"} 0
tapelibrary_slot_positions_occupied{location="slot_F1C4R1"} 1
tapelibrary_slot_positions_occupied{location="slot_F1C4R3"} 0
tapelibrary_slot_positions_occupied{location="slot_F1C9R2"} 4
tapelibrary_slot_positions_occupied{location="slot_F2C1R1"} 4
tapelibrary_slot_positions_occupied{location="slot_F5C7R14"} 4
tapelibrary_slot_positions_occupied{location="slot_F6C2R4"} 1
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected),
		"tapelibrary_slot_get_retries_total",
		"tapelibrary_slot_positions_occupied",
	); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}

	// The 13 aggregates and the freshness gauge, plus 5 series for each of the
	// 8 slots. At the roughly 4 300 slots this library really holds, that
	// per-slot term is about 21 500 series, which is why the flag is off by
	// default.
	reg := prometheus.NewRegistry()
	if err := reg.Register(c); err != nil {
		t.Fatalf("Register: %v", err)
	}
	count, err := testutil.GatherAndCount(reg)
	if err != nil {
		t.Fatalf("GatherAndCount: %v", err)
	}
	if count != 54 {
		t.Fatalf("GatherAndCount = %d, want 54 with per-slot on", count)
	}
}

// TestSlotsCollector_PerSlotInfoCarriesStateAndDepth pins that the active state
// and the physical depth ride on the _info series rather than on a per-slot
// stateset, which the cardinality budget rules out at library scale.
func TestSlotsCollector_PerSlotInfoCarriesStateAndDepth(t *testing.T) {
	_, c := slotsFixtureServer(t, true)
	c.refresh(context.Background())

	expected := `
# HELP tapelibrary_slot_info Always 1. Carries this storage slot's current state and its physical tier depth as labels, joinable to the per-slot measurements on location. The active state rides here rather than on a per-slot stateset: at library scale a full stateset over every slot is what the cardinality budget rules out. Emitted only when --collector.slots.per-slot is set. Note that this location has no tier suffix, so joining it to a cartridge location from DataCartridgesCollector needs one appended.
# TYPE tapelibrary_slot_info gauge
tapelibrary_slot_info{location="slot_F11C2R2",state="inServiceMode",tiers="1"} 1
tapelibrary_slot_info{location="slot_F1C3R10",state="normal",tiers="4"} 1
tapelibrary_slot_info{location="slot_F1C4R1",state="normal",tiers="1"} 1
tapelibrary_slot_info{location="slot_F1C4R3",state="normal",tiers="1"} 1
tapelibrary_slot_info{location="slot_F1C9R2",state="normal",tiers="4"} 1
tapelibrary_slot_info{location="slot_F2C1R1",state="inServiceMode",tiers="4"} 1
tapelibrary_slot_info{location="slot_F5C7R14",state="inServiceMode",tiers="4"} 1
tapelibrary_slot_info{location="slot_F6C2R4",state="inServiceMode",tiers="1"} 1
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected), "tapelibrary_slot_info"); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}
}

// TestSlotsCollector_PerSlotKeepsAggregatesIdentical proves the flag is a pure
// ADDITION. The aggregates every alert reads must not shift by a single series
// when an operator flips it, or the flag would be a monitoring change dressed
// up as a cardinality one.
func TestSlotsCollector_PerSlotKeepsAggregatesIdentical(t *testing.T) {
	data, err := os.ReadFile("testdata/slots.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	aggregates := []string{
		"tapelibrary_slots",
		"tapelibrary_slots_depth",
		"tapelibrary_slots_positions",
		"tapelibrary_slots_positions_available",
		"tapelibrary_slots_positions_occupied",
		"tapelibrary_slots_positions_unreadable",
		"tapelibrary_slots_puts_total",
		"tapelibrary_slots_put_retries_total",
		"tapelibrary_slots_get_retries_total",
	}

	off := slotsServing(t, string(data), false)
	off.refresh(context.Background())
	on := slotsServing(t, string(data), true)
	on.refresh(context.Background())

	for _, name := range aggregates {
		offCount := testutil.CollectAndCount(off, name)
		onCount := testutil.CollectAndCount(on, name)
		if offCount != onCount {
			t.Errorf("%s: %d series with per-slot off, %d with it on; the flag must add per-slot detail and change no aggregate", name, offCount, onCount)
		}
	}
}

// TestSlotsCollector_DoneClosesOnCancel verifies the Done() channel closes when
// the context passed to Start is cancelled. This is the mechanism main.go's
// shutdown seam relies on (see registry.Wait after web.ListenAndServe).
func TestSlotsCollector_DoneClosesOnCancel(t *testing.T) {
	log := logger.NewTextLogger("error")
	c := NewSlotsCollector(log, NewClient("http://example.invalid", time.Second), time.Hour, false)
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

// TestSlotsCollector_CollectServesCacheWithoutIO proves Collect never calls the
// library: after Start's own immediate refresh completes, a long interval (1
// hour) guarantees the ticker cannot fire again during this test, so any
// further request the server receives could only come from Collect itself
// calling out, which the design forbids. On an endpoint this heavy that
// guarantee is the difference between a scrape and a timeout.
func TestSlotsCollector_CollectServesCacheWithoutIO(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.ServeFile(w, r, "testdata/slots.json")
	}))
	defer srv.Close()

	log := logger.NewTextLogger("error")
	c := NewSlotsCollector(log, NewClient(srv.URL, time.Second), time.Hour, false)
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

// TestSlotsCollector_ErrorHandling drives a refresh against a library that only
// ever fails. No cache was ever filled, so the scrape must carry exactly the
// freshness gauge, and must neither panic nor emit a partial aggregate.
func TestSlotsCollector_ErrorHandling(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	log := logger.NewTextLogger("error")
	c := NewSlotsCollector(log, NewClient(srv.URL, time.Second), time.Hour, false)
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

// TestSlotsCollector_ErrorKeepsPreviousCache scripts the backend to succeed
// once, then fail on every later call, and drives at least one more refresh via
// a short interval. The cache from the successful first refresh must survive
// the later failure (fail-open, per refresh's doc comment) rather than being
// cleared or replaced with nothing.
//
// The stakes are specific here: a cleared cache would drop
// tapelibrary_slots_positions_available entirely, and an absent series
// satisfies no comparison, so SlotsNearlyFull would silently stop watching a
// library that had become unreachable rather than firing.
func TestSlotsCollector_ErrorKeepsPreviousCache(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			http.ServeFile(w, r, "testdata/slots.json")
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	log := logger.NewTextLogger("error")
	c := NewSlotsCollector(log, NewClient(srv.URL, time.Second), 20*time.Millisecond, false)
	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		cancel()
		<-c.Done()
	}()

	c.Start(ctx)

	// Wait for backend call 3 to start, rather than sleeping a fixed duration:
	// refresh runs synchronously on Start's single goroutine, so the ticker
	// cannot dispatch call 3 until call 2 (the first failing, 500 refresh) has
	// fully returned, its error branch included. Seeing calls reach 3 is
	// therefore a deterministic proof that call 2's whole error path, not
	// merely the server's response, has already completed.
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
	// A surviving cache emits 14 metrics: the 13 cached from the successful
	// first refresh plus the freshness gauge. A wrongly-cleared cache would
	// emit only 1 (the gauge alone), which "> 0" cannot tell apart.
	if count != 14 {
		t.Fatalf("GatherAndCount = %d, want 14: the previous cache must survive a later refresh error", count)
	}
}

// TestSlotsCollector_StatusTrackerSuccessOnFirstScrape covers the startup
// window before Start's first refresh has completed (Start is deliberately
// never called here). Collect must still emit exactly the freshness gauge,
// valued 0 (not a zero time.Time's large-negative Unix()), and StatusTracker
// must still report this collector as successful: "Collect ran and returned
// data" and "the data is fresh" are different questions.
func TestSlotsCollector_StatusTrackerSuccessOnFirstScrape(t *testing.T) {
	log := logger.NewTextLogger("error")
	c := NewSlotsCollector(log, NewClient("http://example.invalid", time.Second), time.Hour, false)
	// Do NOT call Start(): cache is empty, no refresh has ever run.

	expected := `
# HELP tapelibrary_slots_last_refresh_timestamp_seconds Unix time of the last successful slots refresh. Alert if time() - this > 2 x the collector's configured interval.
# TYPE tapelibrary_slots_last_refresh_timestamp_seconds gauge
tapelibrary_slots_last_refresh_timestamp_seconds 0
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected)); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}

	tracker := NewStatusTracker(log)
	tracker.Add("slots", c)

	expectedSuccess := `
# HELP ` + statusTrackerSuccessMetric + ` Whether the last scrape of the collector succeeded (1=success, 0=failure)
# TYPE ` + statusTrackerSuccessMetric + ` gauge
` + statusTrackerSuccessMetric + `{collector="slots"} 1
`
	if err := testutil.CollectAndCompare(tracker, strings.NewReader(expectedSuccess), statusTrackerSuccessMetric); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}
}

// TestSlotsCollector_StatusTrackerFailure pins that a collector whose refresh
// has only ever failed is still reported as a SUCCESSFUL scrape: Collect
// emitted the freshness gauge, so StatusTracker saw a metric. Staleness is the
// freshness gauge's job to report, not the health metric's, and conflating the
// two would make a slow library indistinguishable from a broken collector.
func TestSlotsCollector_StatusTrackerFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	log := logger.NewTextLogger("error")
	c := NewSlotsCollector(log, NewClient(srv.URL, time.Second), time.Hour, false)
	c.refresh(context.Background())

	tracker := NewStatusTracker(log)
	tracker.Add("slots", c)

	expectedSuccess := `
# HELP ` + statusTrackerSuccessMetric + ` Whether the last scrape of the collector succeeded (1=success, 0=failure)
# TYPE ` + statusTrackerSuccessMetric + ` gauge
` + statusTrackerSuccessMetric + `{collector="slots"} 1
`
	if err := testutil.CollectAndCompare(tracker, strings.NewReader(expectedSuccess), statusTrackerSuccessMetric); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}
}
