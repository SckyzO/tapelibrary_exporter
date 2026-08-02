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

// dataCartridgesFixtureServer serves testdata/data_cartridges.json on every
// request and returns a collector already pointed at it, with per-volser detail
// OFF — the shipped default, and the inverse of the cleaning-cartridge
// collector's. Nothing is started: the caller decides whether to drive refresh
// directly (deterministic) or via Start (which is what the lifecycle tests below
// exercise).
func dataCartridgesFixtureServer(t *testing.T) (*httptest.Server, *DataCartridgesCollector) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "testdata/data_cartridges.json")
	}))
	t.Cleanup(srv.Close)

	log := logger.NewTextLogger("error")
	return srv, NewDataCartridgesCollector(log, NewClient(srv.URL, time.Second), time.Hour, false)
}

// dataCartridgesServing returns a collector fed by a server that answers every
// request with body. Used by the branch tests below, which need input shapes the
// real capture does not contain and which must therefore not be invented inside
// testdata/data_cartridges.json (that fixture stays faithful to the 2026-07-28
// capture's own shapes).
func dataCartridgesServing(t *testing.T, body string, perVolser bool) *DataCartridgesCollector {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	log := logger.NewTextLogger("error")
	return NewDataCartridgesCollector(log, NewClient(srv.URL, time.Second), time.Hour, perVolser)
}

// TestParseDataCartridges exercises parseDataCartridges (piece 2, the pure
// parser) with static byte fixtures: no HTTP, no collector, no logger, no
// goroutine involved.
func TestParseDataCartridges(t *testing.T) {
	data, err := os.ReadFile("testdata/data_cartridges.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	carts, err := parseDataCartridges(data)
	if err != nil {
		t.Fatalf("parseDataCartridges(fixture): %v", err)
	}
	if len(carts) != 8 {
		t.Fatalf("parsed %d cartridges, want 8", len(carts))
	}

	// Field-for-field on the first entry, so a silently renamed JSON tag fails
	// here rather than as an all-zero series much later.
	first := carts[0]
	if first.Volser != "TST083JD" {
		t.Errorf("Volser = %q, want TST083JD", first.Volser)
	}
	if first.State != "normal" {
		t.Errorf("State = %q, want normal", first.State)
	}
	if first.Accessible != "normal" {
		t.Errorf("Accessible = %q, want normal", first.Accessible)
	}
	if first.Location != "slot_F7C3R15T1" {
		t.Errorf("Location = %q, want slot_F7C3R15T1", first.Location)
	}
	if first.MediaType != "3592" {
		t.Errorf("MediaType = %q, want 3592", first.MediaType)
	}
	if first.Type == nil || *first.Type != "JD" {
		t.Errorf("Type = %v, want JD", first.Type)
	}
	if first.Worm == nil || *first.Worm != "false" {
		t.Errorf("Worm = %v, want \"false\"", first.Worm)
	}
	if first.Encrypted == nil || *first.Encrypted != "no" {
		t.Errorf("Encrypted = %v, want no", first.Encrypted)
	}
	if first.LogicalLibrary == nil || *first.LogicalLibrary != "Library-5" {
		t.Errorf("LogicalLibrary = %v, want Library-5", first.LogicalLibrary)
	}
	if first.LifetimeRemaining == nil || *first.LifetimeRemaining != 80 {
		t.Errorf("LifetimeRemaining = %v, want 80", first.LifetimeRemaining)
	}
	if first.MostRecentUsage == nil || *first.MostRecentUsage != "2026-06-26T07:12:33+0000" {
		t.Errorf("MostRecentUsage = %v, want 2026-06-26T07:12:33+0000", first.MostRecentUsage)
	}

	t.Run("the cartridge-memory-absent cohort decodes as null, never as zero", func(t *testing.T) {
		// The defining property of this endpoint: 27 of the 60 cartridges in
		// the 2026-07-28 capture report null for every cartridge-memory field
		// at once, and the fixture keeps three of them. A LifetimeRemaining
		// decoded to a plain int would read 0 here, which R1.11.2 defines as
		// the cartridge being at risk of data loss — the single worst thing
		// this collector could get wrong.
		absent := 0
		for i := range carts {
			if carts[i].LifetimeRemaining == nil {
				absent++
				if carts[i].Type != nil {
					t.Errorf("%s: Type = %v with no lifetime reading, want nil", carts[i].Volser, carts[i].Type)
				}
				if carts[i].Worm != nil {
					t.Errorf("%s: Worm = %v with no lifetime reading, want nil", carts[i].Volser, carts[i].Worm)
				}
			}
		}
		if absent != 3 {
			t.Fatalf("cartridges with no lifetime reading = %d, want 3", absent)
		}
	})

	t.Run("a lifetimeRemaining of 0 is a real reading", func(t *testing.T) {
		// The counterpart of the case above, and the reason the field is a
		// pointer rather than being sentinel-encoded: 0 and "no reading" are
		// different facts and both occur in this fixture.
		var found bool
		for i := range carts {
			if carts[i].Volser == "TST091JD" {
				found = carts[i].LifetimeRemaining != nil && *carts[i].LifetimeRemaining == 0
			}
		}
		if !found {
			t.Fatal("TST091JD did not decode a lifetimeRemaining of 0 as a real reading")
		}
	})

	t.Run("a null logicalLibrary becomes the unassigned token", func(t *testing.T) {
		var got string
		for i := range carts {
			if carts[i].Volser == "TST105JD" {
				got = carts[i].logicalLibrary()
			}
		}
		if got != dataCartridgeUnassigned {
			t.Fatalf("logicalLibrary() = %q for the unassigned cartridge, want %q", got, dataCartridgeUnassigned)
		}
	})

	t.Run("a null type becomes the unknown token", func(t *testing.T) {
		var got string
		for i := range carts {
			if carts[i].Volser == "TST084JD" {
				got = carts[i].cartridgeType()
			}
		}
		if got != dataCartridgeUnknown {
			t.Fatalf("cartridgeType() = %q for a cartridge with no cartridge memory read, want %q", got, dataCartridgeUnknown)
		}
	})

	t.Run("an empty array is rejected", func(t *testing.T) {
		// The opposite of CleaningCartridgesCollector, which accepts [] and is
		// the only collector in this exporter that does. A library reporting
		// zero DATA cartridges has lost its inventory or truncated the body;
		// keeping the previous cache and letting the freshness gauge go stale
		// is the safer reading.
		if _, err := parseDataCartridges([]byte(`[]`)); err == nil {
			t.Fatal("parseDataCartridges([]) = nil error, want a rejection: an empty data-cartridge inventory is content loss, not a reading")
		}
	})

	t.Run("malformed JSON is rejected", func(t *testing.T) {
		if _, err := parseDataCartridges([]byte(`{"not":"an array"}`)); err == nil {
			t.Fatal("parseDataCartridges(object) = nil error, want a parse failure")
		}
	})

	t.Run("truncated JSON is rejected", func(t *testing.T) {
		if _, err := parseDataCartridges([]byte(`[{"volser":"TST001JD"`)); err == nil {
			t.Fatal("parseDataCartridges(truncated) = nil error, want a parse failure")
		}
	})

	t.Run("an empty volser is rejected", func(t *testing.T) {
		body := `[{"volser":"","state":"normal","location":"slot_F1C1R1T1"}]`
		if _, err := parseDataCartridges([]byte(body)); err == nil {
			t.Fatal("parseDataCartridges(empty volser) = nil error, want a rejection")
		}
	})

	t.Run("an empty location is rejected", func(t *testing.T) {
		body := `[{"volser":"TST001JD","state":"normal","location":""}]`
		if _, err := parseDataCartridges([]byte(body)); err == nil {
			t.Fatal("parseDataCartridges(empty location) = nil error, want a rejection")
		}
	})

	t.Run("a duplicate volser AND location pair is rejected", func(t *testing.T) {
		// Two metrics sharing a descriptor AND a label set fail Registry.Gather
		// for the whole scrape. On the largest endpoint in the exporter that
		// would take out all eighteen collectors at once, so this fails closed.
		body := `[
			{"volser":"TST001JD","state":"normal","location":"slot_F1C1R1T1"},
			{"volser":"TST001JD","state":"normal","location":"slot_F1C1R1T1"}
		]`
		if _, err := parseDataCartridges([]byte(body)); err == nil {
			t.Fatal("parseDataCartridges(duplicate pair) = nil error, want a rejection")
		}
	})

	t.Run("a duplicate volser at different locations is accepted", func(t *testing.T) {
		// R1.11.2 states that internalAddress is the tie-breaker "if there are
		// duplicate VOLSERs", and the sibling cleaningCartridges capture proves
		// the case is real. Rejecting it would drop half a real population.
		body := `[
			{"volser":"TST001JD","state":"normal","location":"slot_F1C1R1T1"},
			{"volser":"TST001JD","state":"normal","location":"slot_F2C1R1T1"}
		]`
		carts, err := parseDataCartridges([]byte(body))
		if err != nil {
			t.Fatalf("parseDataCartridges(duplicate volser, distinct locations) = %v, want nil", err)
		}
		if len(carts) != 2 {
			t.Fatalf("parsed %d cartridges, want 2", len(carts))
		}
	})
}

func TestDataCartridgesCollector_Describe(t *testing.T) {
	log := logger.NewTextLogger("error")
	c := NewDataCartridgesCollector(log, NewClient("http://example.invalid", time.Second), time.Hour, true)

	ch := make(chan *prometheus.Desc, 30)
	c.Describe(ch)
	close(ch)

	count := 0
	for range ch {
		count++
	}
	if count != 11 {
		t.Fatalf("Describe sent %d descriptors, want 11", count)
	}
}

// TestDataCartridgesCollector_DescribeIsConstantWithoutPerVolser pins the rule
// that a descriptor is what this collector CAN emit, not what it did last time:
// turning per-volser on changes the metrics, never the descriptors, and
// docs/metrics.md documents them on that basis.
func TestDataCartridgesCollector_DescribeIsConstantWithoutPerVolser(t *testing.T) {
	log := logger.NewTextLogger("error")
	c := NewDataCartridgesCollector(log, NewClient("http://example.invalid", time.Second), time.Hour, false)

	ch := make(chan *prometheus.Desc, 30)
	c.Describe(ch)
	close(ch)

	count := 0
	for range ch {
		count++
	}
	if count != 11 {
		t.Fatalf("Describe sent %d descriptors with per-volser off, want 11", count)
	}
}

// TestDataCartridgesCollector_Collect pins the exact exposition text of every
// always-emitted aggregate against the fixture, at the shipped default flag
// settings. refresh is driven directly rather than through Start, so the
// assertion is deterministic and carries no sleep: Start's own scheduling is
// what the lifecycle tests below cover.
func TestDataCartridgesCollector_Collect(t *testing.T) {
	_, c := dataCartridgesFixtureServer(t)
	c.refresh(context.Background())

	// Every documented state is emitted for every partition the response
	// mentions, including the zeroes: a rule matching state="atEndOfLife" must
	// have a series to read when the last spent cartridge is migrated out, and
	// an absent series satisfies no matcher. The unassigned partition is the
	// fixture's single cartridge with a null logicalLibrary.
	expectedStates := `
# HELP tapelibrary_data_cartridges Number of data cartridges the library holds in each documented state, per logical library. Every documented state is emitted for every partition seen, whether or not any cartridge is in it, so a rule matching a state still has a series to read when its count reaches zero. A cartridge assigned to no partition is counted under logical_library="unassigned".
# TYPE tapelibrary_data_cartridges gauge
tapelibrary_data_cartridges{logical_library="Library-5",state="assignmentRequired"} 0
tapelibrary_data_cartridges{logical_library="Library-5",state="atEndOfLife"} 1
tapelibrary_data_cartridges{logical_library="Library-5",state="exportQueued"} 0
tapelibrary_data_cartridges{logical_library="Library-5",state="failedVerification"} 1
tapelibrary_data_cartridges{logical_library="Library-5",state="importing"} 0
tapelibrary_data_cartridges{logical_library="Library-5",state="normal"} 3
tapelibrary_data_cartridges{logical_library="Library-5",state="uncertainBarcode"} 0
tapelibrary_data_cartridges{logical_library="Library-5",state="unknown"} 0
tapelibrary_data_cartridges{logical_library="Library-5",state="verifying"} 0
tapelibrary_data_cartridges{logical_library="Library-6",state="assignmentRequired"} 0
tapelibrary_data_cartridges{logical_library="Library-6",state="atEndOfLife"} 0
tapelibrary_data_cartridges{logical_library="Library-6",state="exportQueued"} 0
tapelibrary_data_cartridges{logical_library="Library-6",state="failedVerification"} 0
tapelibrary_data_cartridges{logical_library="Library-6",state="importing"} 1
tapelibrary_data_cartridges{logical_library="Library-6",state="normal"} 1
tapelibrary_data_cartridges{logical_library="Library-6",state="uncertainBarcode"} 0
tapelibrary_data_cartridges{logical_library="Library-6",state="unknown"} 0
tapelibrary_data_cartridges{logical_library="Library-6",state="verifying"} 0
tapelibrary_data_cartridges{logical_library="unassigned",state="assignmentRequired"} 0
tapelibrary_data_cartridges{logical_library="unassigned",state="atEndOfLife"} 0
tapelibrary_data_cartridges{logical_library="unassigned",state="exportQueued"} 0
tapelibrary_data_cartridges{logical_library="unassigned",state="failedVerification"} 0
tapelibrary_data_cartridges{logical_library="unassigned",state="importing"} 0
tapelibrary_data_cartridges{logical_library="unassigned",state="normal"} 1
tapelibrary_data_cartridges{logical_library="unassigned",state="uncertainBarcode"} 0
tapelibrary_data_cartridges{logical_library="unassigned",state="unknown"} 0
tapelibrary_data_cartridges{logical_library="unassigned",state="verifying"} 0
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expectedStates), "tapelibrary_data_cartridges"); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}

	// The three unknown tokens are what keep every typed breakdown summing to
	// the library's real cartridge count: 5 JD + 3 unknown = 8, 6 no + 1
	// unknown + 1 yes = 8, 4 false + 1 true + 3 unknown = 8. That property is
	// the whole reason the tokens exist, so it is asserted rather than assumed.
	expectedBreakdowns := `
# HELP tapelibrary_data_cartridges_access Number of data cartridges by accessor reach. Always emitted for all three documented values. On a dual-accessor library a non-zero access="limited" or access="no" count is the only inventory-scale signal that a robotics fault has put part of the cartridge parc out of reach.
# TYPE tapelibrary_data_cartridges_access gauge
tapelibrary_data_cartridges_access{access="limited"} 0
tapelibrary_data_cartridges_access{access="no"} 0
tapelibrary_data_cartridges_access{access="normal"} 8
# HELP tapelibrary_data_cartridges_encryption Number of data cartridges by encryption state. Always emitted for all three values. encrypted="unknown" is the manual's null, which it defines as a cartridge that has not been mounted, and is a distinct fact from "no": nothing is known about the cartridge's contents either way.
# TYPE tapelibrary_data_cartridges_encryption gauge
tapelibrary_data_cartridges_encryption{encrypted="no"} 6
tapelibrary_data_cartridges_encryption{encrypted="unknown"} 1
tapelibrary_data_cartridges_encryption{encrypted="yes"} 1
# HELP tapelibrary_data_cartridges_media Number of data cartridges of each media and cartridge type. Only combinations the library actually reports are emitted: the manual documents 28 cartridge type codes and a site runs one or two. A cartridge whose type the library has not read from cartridge memory is counted under cartridge_type="unknown" rather than dropped, so these counts always sum to the library's real cartridge count.
# TYPE tapelibrary_data_cartridges_media gauge
tapelibrary_data_cartridges_media{cartridge_type="JD",media_type="3592"} 5
tapelibrary_data_cartridges_media{cartridge_type="unknown",media_type="3592"} 3
# HELP tapelibrary_data_cartridges_worm Number of data cartridges by write-once (WORM) status. Always emitted for all three values. worm="unknown" is a cartridge whose cartridge memory the library has not read.
# TYPE tapelibrary_data_cartridges_worm gauge
tapelibrary_data_cartridges_worm{worm="false"} 4
tapelibrary_data_cartridges_worm{worm="true"} 1
tapelibrary_data_cartridges_worm{worm="unknown"} 3
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expectedBreakdowns),
		"tapelibrary_data_cartridges_access",
		"tapelibrary_data_cartridges_encryption",
		"tapelibrary_data_cartridges_media",
		"tapelibrary_data_cartridges_worm",
	); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}

	// The histogram covers only the five cartridges with a reading (0.80, 0.00,
	// 0.97, 0.63, 0.10), and _count + lifetime_unknown = 8, the full fixture.
	// le="0" is 1 because a lifetimeRemaining of 0 IS a real reading; the three
	// cartridges with no reading are NOT in that bucket, which is exactly the
	// distinction this collector exists to preserve.
	expectedLifetime := `
# HELP tapelibrary_data_cartridges_lifetime_remaining_ratio Distribution of remaining media life across the library's data cartridges, as a ratio from 0 (at end of life, at risk of data loss and out of warranty) to 1 (unused). Converted from the API's 0-100 percentage. Cartridges the library reports no reading for are counted in tapelibrary_data_cartridges_lifetime_unknown instead of being observed here as 0, which would falsely report them as spent.
# TYPE tapelibrary_data_cartridges_lifetime_remaining_ratio histogram
tapelibrary_data_cartridges_lifetime_remaining_ratio_bucket{le="0"} 1
tapelibrary_data_cartridges_lifetime_remaining_ratio_bucket{le="0.1"} 2
tapelibrary_data_cartridges_lifetime_remaining_ratio_bucket{le="0.2"} 2
tapelibrary_data_cartridges_lifetime_remaining_ratio_bucket{le="0.3"} 2
tapelibrary_data_cartridges_lifetime_remaining_ratio_bucket{le="0.5"} 2
tapelibrary_data_cartridges_lifetime_remaining_ratio_bucket{le="0.75"} 3
tapelibrary_data_cartridges_lifetime_remaining_ratio_bucket{le="0.9"} 4
tapelibrary_data_cartridges_lifetime_remaining_ratio_bucket{le="1"} 5
tapelibrary_data_cartridges_lifetime_remaining_ratio_bucket{le="+Inf"} 5
tapelibrary_data_cartridges_lifetime_remaining_ratio_sum 2.5
tapelibrary_data_cartridges_lifetime_remaining_ratio_count 5
# HELP tapelibrary_data_cartridges_lifetime_unknown Number of data cartridges for which the library reports no remaining-life reading. Always emitted. These are excluded from tapelibrary_data_cartridges_lifetime_remaining_ratio entirely, so this value plus that histogram's _count is the library's full cartridge parc; without it, the histogram would read as covering every cartridge when it can cover only those whose cartridge memory has been read.
# TYPE tapelibrary_data_cartridges_lifetime_unknown gauge
tapelibrary_data_cartridges_lifetime_unknown 3
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expectedLifetime),
		"tapelibrary_data_cartridges_lifetime_remaining_ratio",
		"tapelibrary_data_cartridges_lifetime_unknown",
	); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}

	reg := prometheus.NewRegistry()
	if err := reg.Register(c); err != nil {
		t.Fatalf("Register: %v", err)
	}
	count, err := testutil.GatherAndCount(reg)
	if err != nil {
		// A duplicate label set fails Gather for the WHOLE scrape, which on
		// this endpoint would take out every other collector too.
		t.Fatalf("GatherAndCount: %v", err)
	}
	// 27 state counts (9 documented states x 3 partitions) + 2 media pairs +
	// 3 encryption + 3 WORM + 3 access + the unknown-lifetime count + the
	// histogram + the freshness gauge. No per-cartridge series: the flag is off.
	if count != 41 {
		t.Fatalf("GatherAndCount = %d, want 41", count)
	}
}

// TestDataCartridgesCollector_PerVolserAddsPerCartridgeSeries is the guarantee
// that makes --collector.data_cartridges.per-volser a pure cardinality lever.
// Turning it on adds the three per-cartridge families and changes nothing else,
// so every aggregate an alert reads is byte-identical either way.
func TestDataCartridgesCollector_PerVolserAddsPerCartridgeSeries(t *testing.T) {
	data, err := os.ReadFile("testdata/data_cartridges.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	c := dataCartridgesServing(t, string(data), true)
	c.refresh(context.Background())

	// Five ratios, not eight: the three cartridges with no reading emit no
	// series at all rather than a 0 that would falsely mark them spent.
	// TST091JD's 0 is the real reading, and it IS emitted.
	expected := `
# HELP tapelibrary_data_cartridge_last_usage_timestamp_seconds Unix time at which this data cartridge was last mounted into a drive. A cartridge the library reports no usage for produces no series at all, rather than a 0 that would place its last mount in 1970. Emitted only when --collector.data_cartridges.per-volser is set.
# TYPE tapelibrary_data_cartridge_last_usage_timestamp_seconds gauge
tapelibrary_data_cartridge_last_usage_timestamp_seconds{location="ioSlot_F2IOu1",volser="TST112JD"} 1.785169869e+09
tapelibrary_data_cartridge_last_usage_timestamp_seconds{location="slot_F3C7R11T3",volser="TST105JD"} 1.778471297e+09
tapelibrary_data_cartridge_last_usage_timestamp_seconds{location="slot_F4C2R08T2",volser="TST091JD"} 1.784498691e+09
tapelibrary_data_cartridge_last_usage_timestamp_seconds{location="slot_F6C1R21T0",volser="TST097JD"} 1.78298374e+09
tapelibrary_data_cartridge_last_usage_timestamp_seconds{location="slot_F7C3R15T1",volser="TST083JD"} 1.782457953e+09
tapelibrary_data_cartridge_last_usage_timestamp_seconds{location="slot_F8C4R29T1",volser="TST077JD"} 1.784980624e+09
tapelibrary_data_cartridge_last_usage_timestamp_seconds{location="slot_F9C9R2T4",volser="TST084JD"} 1.653396742e+09
# HELP tapelibrary_data_cartridge_lifetime_remaining_ratio Remaining media life of this individual data cartridge, as a ratio from 0 to 1. Emitted only when --collector.data_cartridges.per-volser is set, which it is not by default. A cartridge the library reports no reading for produces no series at all, rather than a 0 that would falsely mark it spent. Keyed by volser AND location: a barcode is not unique in a tape library.
# TYPE tapelibrary_data_cartridge_lifetime_remaining_ratio gauge
tapelibrary_data_cartridge_lifetime_remaining_ratio{location="slot_F3C7R11T3",volser="TST105JD"} 0.63
tapelibrary_data_cartridge_lifetime_remaining_ratio{location="slot_F4C2R08T2",volser="TST091JD"} 0
tapelibrary_data_cartridge_lifetime_remaining_ratio{location="slot_F6C1R21T0",volser="TST097JD"} 0.97
tapelibrary_data_cartridge_lifetime_remaining_ratio{location="slot_F7C3R15T1",volser="TST083JD"} 0.8
tapelibrary_data_cartridge_lifetime_remaining_ratio{location="slot_F8C4R29T1",volser="TST077JD"} 0.1
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected),
		"tapelibrary_data_cartridge_last_usage_timestamp_seconds",
		"tapelibrary_data_cartridge_lifetime_remaining_ratio",
	); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}

	reg := prometheus.NewRegistry()
	if err := reg.Register(c); err != nil {
		t.Fatalf("Register: %v", err)
	}
	count, err := testutil.GatherAndCount(reg)
	if err != nil {
		t.Fatalf("GatherAndCount: %v", err)
	}
	// The 41 aggregates above, plus 8 _info, 5 lifetime ratios and 7 usage
	// timestamps. At the 9 749 cartridges this library really holds, that
	// per-cartridge term is ~29 250 series, which is why the flag is off by
	// default.
	if count != 61 {
		t.Fatalf("GatherAndCount = %d, want 61 with per-volser on", count)
	}
}

// TestDataCartridgesCollector_PerVolserKeepsAggregatesIdentical proves the flag
// is a pure ADDITION. The aggregates every alert reads must not shift by a
// single series when an operator flips it, or the flag would be a monitoring
// change dressed up as a cardinality one.
func TestDataCartridgesCollector_PerVolserKeepsAggregatesIdentical(t *testing.T) {
	data, err := os.ReadFile("testdata/data_cartridges.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	aggregates := []string{
		"tapelibrary_data_cartridges",
		"tapelibrary_data_cartridges_access",
		"tapelibrary_data_cartridges_encryption",
		"tapelibrary_data_cartridges_media",
		"tapelibrary_data_cartridges_worm",
		"tapelibrary_data_cartridges_lifetime_remaining_ratio",
		"tapelibrary_data_cartridges_lifetime_unknown",
	}

	off := dataCartridgesServing(t, string(data), false)
	off.refresh(context.Background())
	on := dataCartridgesServing(t, string(data), true)
	on.refresh(context.Background())

	for _, name := range aggregates {
		offCount := testutil.CollectAndCount(off, name)
		onCount := testutil.CollectAndCount(on, name)
		if offCount != onCount {
			t.Errorf("%s: %d series with per-volser off, %d with it on; the flag must add per-cartridge detail and change no aggregate", name, offCount, onCount)
		}
	}
}

// TestDataCartridgesCollector_NullLifetimeIsNotZero is the regression test for
// the single most damaging thing this collector could do. 45% of the cartridges
// in the 2026-07-28 capture report a null lifetimeRemaining, and R1.11.2 defines
// 0% as "at risk of data loss, may not be covered by warranty". Decoding that
// null to 0 would report nearly half a library's parc as spent, and
// DataCartridgesWearingOut would page on it.
func TestDataCartridgesCollector_NullLifetimeIsNotZero(t *testing.T) {
	body := `[{"volser":"TST001JD","state":"normal","accessible":"normal","location":"slot_F1C1R1T1","mediaType":"3592","type":null,"worm":null,"encrypted":"no","logicalLibrary":"Library-5","lifetimeRemaining":null,"mostRecentUsage":null}]`
	c := dataCartridgesServing(t, body, true)
	c.refresh(context.Background())

	expected := `
# HELP tapelibrary_data_cartridges_lifetime_remaining_ratio Distribution of remaining media life across the library's data cartridges, as a ratio from 0 (at end of life, at risk of data loss and out of warranty) to 1 (unused). Converted from the API's 0-100 percentage. Cartridges the library reports no reading for are counted in tapelibrary_data_cartridges_lifetime_unknown instead of being observed here as 0, which would falsely report them as spent.
# TYPE tapelibrary_data_cartridges_lifetime_remaining_ratio histogram
tapelibrary_data_cartridges_lifetime_remaining_ratio_bucket{le="0"} 0
tapelibrary_data_cartridges_lifetime_remaining_ratio_bucket{le="0.1"} 0
tapelibrary_data_cartridges_lifetime_remaining_ratio_bucket{le="0.2"} 0
tapelibrary_data_cartridges_lifetime_remaining_ratio_bucket{le="0.3"} 0
tapelibrary_data_cartridges_lifetime_remaining_ratio_bucket{le="0.5"} 0
tapelibrary_data_cartridges_lifetime_remaining_ratio_bucket{le="0.75"} 0
tapelibrary_data_cartridges_lifetime_remaining_ratio_bucket{le="0.9"} 0
tapelibrary_data_cartridges_lifetime_remaining_ratio_bucket{le="1"} 0
tapelibrary_data_cartridges_lifetime_remaining_ratio_bucket{le="+Inf"} 0
tapelibrary_data_cartridges_lifetime_remaining_ratio_sum 0
tapelibrary_data_cartridges_lifetime_remaining_ratio_count 0
# HELP tapelibrary_data_cartridges_lifetime_unknown Number of data cartridges for which the library reports no remaining-life reading. Always emitted. These are excluded from tapelibrary_data_cartridges_lifetime_remaining_ratio entirely, so this value plus that histogram's _count is the library's full cartridge parc; without it, the histogram would read as covering every cartridge when it can cover only those whose cartridge memory has been read.
# TYPE tapelibrary_data_cartridges_lifetime_unknown gauge
tapelibrary_data_cartridges_lifetime_unknown 1
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected),
		"tapelibrary_data_cartridges_lifetime_remaining_ratio",
		"tapelibrary_data_cartridges_lifetime_unknown",
	); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}

	// And no per-cartridge ratio at all, even with the flag on.
	count := testutil.CollectAndCount(c, "tapelibrary_data_cartridge_lifetime_remaining_ratio")
	if count != 0 {
		t.Fatalf("per-cartridge lifetime series = %d, want 0: a cartridge with no reading must emit none", count)
	}
}

// TestDataCartridgesCollector_UndocumentedStateIsStillCounted covers the
// emit-observed-anyway branch. R1.11.2's own prose names cartridgeFailedMove
// and errorThresholdExceeded under library.cartridgeDegraded while tabulating
// neither, so the manual's table is demonstrably a floor rather than a ceiling.
// An unlisted state must surface as its own series rather than vanish from the
// counts.
func TestDataCartridgesCollector_UndocumentedStateIsStillCounted(t *testing.T) {
	body := `[{"volser":"TST001JD","state":"errorThresholdExceeded","accessible":"normal","location":"slot_F1C1R1T1","mediaType":"3592","logicalLibrary":"Library-5"}]`
	c := dataCartridgesServing(t, body, false)
	c.refresh(context.Background())

	count := testutil.CollectAndCount(c, "tapelibrary_data_cartridges")
	// The 9 documented states for Library-5, plus the undocumented one.
	if count != 10 {
		t.Fatalf("state series = %d, want 10 (9 documented + 1 observed-but-undocumented)", count)
	}

	expected := `
# HELP tapelibrary_data_cartridges Number of data cartridges the library holds in each documented state, per logical library. Every documented state is emitted for every partition seen, whether or not any cartridge is in it, so a rule matching a state still has a series to read when its count reaches zero. A cartridge assigned to no partition is counted under logical_library="unassigned".
# TYPE tapelibrary_data_cartridges gauge
tapelibrary_data_cartridges{logical_library="Library-5",state="assignmentRequired"} 0
tapelibrary_data_cartridges{logical_library="Library-5",state="atEndOfLife"} 0
tapelibrary_data_cartridges{logical_library="Library-5",state="errorThresholdExceeded"} 1
tapelibrary_data_cartridges{logical_library="Library-5",state="exportQueued"} 0
tapelibrary_data_cartridges{logical_library="Library-5",state="failedVerification"} 0
tapelibrary_data_cartridges{logical_library="Library-5",state="importing"} 0
tapelibrary_data_cartridges{logical_library="Library-5",state="normal"} 0
tapelibrary_data_cartridges{logical_library="Library-5",state="uncertainBarcode"} 0
tapelibrary_data_cartridges{logical_library="Library-5",state="unknown"} 0
tapelibrary_data_cartridges{logical_library="Library-5",state="verifying"} 0
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected), "tapelibrary_data_cartridges"); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}
}

// TestDataCartridgesCollector_EmptyStateIsCountedNowhere pins the other half of
// stateCounts' undocumented branch: an entry with no state at all is warned
// about and counted in no series, rather than emitted under state="" which no
// operator could act on.
func TestDataCartridgesCollector_EmptyStateIsCountedNowhere(t *testing.T) {
	body := `[{"volser":"TST001JD","state":"","accessible":"normal","location":"slot_F1C1R1T1","mediaType":"3592","logicalLibrary":"Library-5"}]`
	c := dataCartridgesServing(t, body, false)
	c.refresh(context.Background())

	count := testutil.CollectAndCount(c, "tapelibrary_data_cartridges")
	if count != 9 {
		t.Fatalf("state series = %d, want 9 (the documented states only; an empty state emits none)", count)
	}
}

// TestDataCartridgesCollector_UndocumentedValueIsStillCounted covers the same
// emit-observed-anyway branch on the shared valueCounts helper, which the
// encryption, WORM and access families all route through. Written once in the
// collector precisely so this property cannot hold for one family and be
// forgotten on another.
func TestDataCartridgesCollector_UndocumentedValueIsStillCounted(t *testing.T) {
	body := `[{"volser":"TST001JD","state":"normal","accessible":"degraded","location":"slot_F1C1R1T1","mediaType":"3592","logicalLibrary":"Library-5","encrypted":"pending"}]`
	c := dataCartridgesServing(t, body, false)
	c.refresh(context.Background())

	expected := `
# HELP tapelibrary_data_cartridges_access Number of data cartridges by accessor reach. Always emitted for all three documented values. On a dual-accessor library a non-zero access="limited" or access="no" count is the only inventory-scale signal that a robotics fault has put part of the cartridge parc out of reach.
# TYPE tapelibrary_data_cartridges_access gauge
tapelibrary_data_cartridges_access{access="degraded"} 1
tapelibrary_data_cartridges_access{access="limited"} 0
tapelibrary_data_cartridges_access{access="no"} 0
tapelibrary_data_cartridges_access{access="normal"} 0
# HELP tapelibrary_data_cartridges_encryption Number of data cartridges by encryption state. Always emitted for all three values. encrypted="unknown" is the manual's null, which it defines as a cartridge that has not been mounted, and is a distinct fact from "no": nothing is known about the cartridge's contents either way.
# TYPE tapelibrary_data_cartridges_encryption gauge
tapelibrary_data_cartridges_encryption{encrypted="no"} 0
tapelibrary_data_cartridges_encryption{encrypted="pending"} 1
tapelibrary_data_cartridges_encryption{encrypted="unknown"} 0
tapelibrary_data_cartridges_encryption{encrypted="yes"} 0
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected),
		"tapelibrary_data_cartridges_access",
		"tapelibrary_data_cartridges_encryption",
	); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}
}

// TestDataCartridgesCollector_NullUsageEmitsNoSeries pins the timestamp rule
// this exporter established on DrivesCollector: a null instant produces no
// series at all, never a 0 that would place the event at the Unix epoch and
// quietly corrupt every "unused since" query.
func TestDataCartridgesCollector_NullUsageEmitsNoSeries(t *testing.T) {
	body := `[{"volser":"TST001JD","state":"normal","accessible":"normal","location":"slot_F1C1R1T1","mediaType":"3592","logicalLibrary":"Library-5","mostRecentUsage":null}]`
	c := dataCartridgesServing(t, body, true)
	c.refresh(context.Background())

	count := testutil.CollectAndCount(c, "tapelibrary_data_cartridge_last_usage_timestamp_seconds")
	if count != 0 {
		t.Fatalf("usage series = %d, want 0: a null mostRecentUsage must emit none", count)
	}
}

// TestDataCartridgesCollector_UnparseableUsageEmitsNoSeries covers the other
// branch of parseMostRecentUsage. The wire format carries no colon in its zone
// offset, so an RFC3339 value is exactly the kind of drift this must survive:
// it is logged and skipped, never guessed at.
func TestDataCartridgesCollector_UnparseableUsageEmitsNoSeries(t *testing.T) {
	body := `[{"volser":"TST001JD","state":"normal","accessible":"normal","location":"slot_F1C1R1T1","mediaType":"3592","logicalLibrary":"Library-5","mostRecentUsage":"2026-06-26T07:12:33+00:00"}]`
	c := dataCartridgesServing(t, body, true)
	c.refresh(context.Background())

	count := testutil.CollectAndCount(c, "tapelibrary_data_cartridge_last_usage_timestamp_seconds")
	if count != 0 {
		t.Fatalf("usage series = %d, want 0: an unparseable mostRecentUsage must emit none", count)
	}
}

func TestDataCartridgesCollector_DoneClosesOnCancel(t *testing.T) {
	log := logger.NewTextLogger("error")
	c := NewDataCartridgesCollector(log, NewClient("http://example.invalid", time.Second), time.Hour, false)
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

// TestDataCartridgesCollector_CollectServesCacheWithoutIO proves Collect never
// calls the library: after Start's own immediate refresh completes, a long
// interval (1 hour) guarantees the ticker cannot fire again during this test, so
// any further request the server receives could only come from Collect itself
// calling out, which the design forbids. On a 9 749-entry endpoint reached over
// a slow SCSI/LCC path, a Collect that called out would time out the scrape for
// every other collector sharing that instance's ceiling of one.
func TestDataCartridgesCollector_CollectServesCacheWithoutIO(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.ServeFile(w, r, "testdata/data_cartridges.json")
	}))
	defer srv.Close()

	log := logger.NewTextLogger("error")
	c := NewDataCartridgesCollector(log, NewClient(srv.URL, time.Second), time.Hour, false)
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

// TestDataCartridgesCollector_ErrorHandling drives a refresh against a library
// that only ever fails. No cache was ever filled, so the scrape must carry
// exactly the freshness gauge, and must neither panic nor emit a partial set of
// aggregates.
func TestDataCartridgesCollector_ErrorHandling(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	log := logger.NewTextLogger("error")
	c := NewDataCartridgesCollector(log, NewClient(srv.URL, time.Second), time.Hour, false)
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

// TestDataCartridgesCollector_ErrorKeepsPreviousCache scripts the backend to
// succeed once, then fail on every later call, and drives at least one more
// refresh via a short interval. The cache from the successful first refresh must
// survive the later failure (fail-open, per refresh's doc comment) rather than
// being cleared or replaced with nothing.
//
// A cleared cache would drop every aggregate, and a rule matching
// state="atEndOfLife" does not match an absent series, so a library that became
// unreachable would silently stop being watched rather than alerting.
func TestDataCartridgesCollector_ErrorKeepsPreviousCache(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			http.ServeFile(w, r, "testdata/data_cartridges.json")
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	log := logger.NewTextLogger("error")
	c := NewDataCartridgesCollector(log, NewClient(srv.URL, time.Second), 20*time.Millisecond, false)
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
	// A surviving cache emits 41: the 40 cached from the successful first
	// refresh plus the freshness gauge. A wrongly-cleared cache would emit only
	// 1 (the gauge alone), which "> 0" cannot tell apart.
	if count != 41 {
		t.Fatalf("GatherAndCount = %d, want 41: the previous cache must survive a later refresh error", count)
	}
}

// TestDataCartridgesCollector_StatusTrackerSuccessOnFirstScrape covers the
// startup window before Start's first refresh has completed (Start is
// deliberately never called here). On this endpoint that window is the longest
// in the exporter — 9 749 entries over a slow path — so the guarantee matters
// more here than anywhere else. Collect must still emit exactly the freshness
// gauge, valued 0 (not a zero time.Time's large-negative Unix()), and
// StatusTracker must still report this collector as successful: "Collect ran and
// returned data" and "the data is fresh" are different questions.
func TestDataCartridgesCollector_StatusTrackerSuccessOnFirstScrape(t *testing.T) {
	log := logger.NewTextLogger("error")
	c := NewDataCartridgesCollector(log, NewClient("http://example.invalid", time.Second), time.Hour, false)
	// Do NOT call Start(): cache is empty, no refresh has ever run.

	expected := `
# HELP tapelibrary_data_cartridges_last_refresh_timestamp_seconds Unix time of the last successful data cartridges refresh. Alert if time() - this > 2 x the collector's configured interval.
# TYPE tapelibrary_data_cartridges_last_refresh_timestamp_seconds gauge
tapelibrary_data_cartridges_last_refresh_timestamp_seconds 0
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected)); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}

	tracker := NewStatusTracker(log)
	tracker.Add("data_cartridges", c)

	expectedSuccess := `
# HELP ` + statusTrackerSuccessMetric + ` Whether the last scrape of the collector succeeded (1=success, 0=failure)
# TYPE ` + statusTrackerSuccessMetric + ` gauge
` + statusTrackerSuccessMetric + `{collector="data_cartridges"} 1
`
	if err := testutil.CollectAndCompare(tracker, strings.NewReader(expectedSuccess), statusTrackerSuccessMetric); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}
}

// TestDataCartridgesCollector_StatusTrackerFailure pins the other half of the
// tracker contract: a collector whose refresh has never succeeded still emits
// its freshness gauge, so it reports as alive. That is deliberate. The freshness
// gauge's VALUE, not the success gauge, is what says the data is stale, and the
// rule for reading it is stated in the descriptor's own help text rather than
// shipped in monitoring/prometheus/alerts.yml — no collector in this exporter
// ships a staleness rule, since the right interval to alert on is
// per-deployment.
func TestDataCartridgesCollector_StatusTrackerFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	log := logger.NewTextLogger("error")
	c := NewDataCartridgesCollector(log, NewClient(srv.URL, time.Second), time.Hour, false)
	c.refresh(context.Background())

	tracker := NewStatusTracker(log)
	tracker.Add("data_cartridges", c)

	expectedSuccess := `
# HELP ` + statusTrackerSuccessMetric + ` Whether the last scrape of the collector succeeded (1=success, 0=failure)
# TYPE ` + statusTrackerSuccessMetric + ` gauge
` + statusTrackerSuccessMetric + `{collector="data_cartridges"} 1
`
	if err := testutil.CollectAndCompare(tracker, strings.NewReader(expectedSuccess), statusTrackerSuccessMetric); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}
}
