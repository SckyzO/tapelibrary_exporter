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

// cleaningCartridgesFixtureServer serves testdata/cleaning_cartridges.json on
// every request and returns a collector already pointed at it, with per-volser
// detail on (the shipped default). Nothing is started: the caller decides
// whether to drive refresh directly (deterministic) or via Start (which is what
// the lifecycle tests below exercise).
func cleaningCartridgesFixtureServer(t *testing.T) (*httptest.Server, *CleaningCartridgesCollector) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "testdata/cleaning_cartridges.json")
	}))
	t.Cleanup(srv.Close)

	log := logger.NewTextLogger("error")
	return srv, NewCleaningCartridgesCollector(log, NewClient(srv.URL, time.Second), time.Hour, true)
}

// cleaningCartridgesServing returns a collector fed by a server that answers
// every request with body. Used by the branch tests below, which need input
// shapes the real capture does not contain and which must therefore not be
// invented inside testdata/cleaning_cartridges.json (that fixture stays
// faithful to the 2026-07-28 capture, in which every cartridge is `normal` and
// every one reports a mostRecentUsage).
func cleaningCartridgesServing(t *testing.T, body string, perVolser bool) *CleaningCartridgesCollector {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	log := logger.NewTextLogger("error")
	return NewCleaningCartridgesCollector(log, NewClient(srv.URL, time.Second), time.Hour, perVolser)
}

// TestParseCleaningCartridges exercises parseCleaningCartridges (piece 2, the
// pure parser) with static byte fixtures: no HTTP, no collector, no logger, no
// goroutine involved.
func TestParseCleaningCartridges(t *testing.T) {
	data, err := os.ReadFile("testdata/cleaning_cartridges.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	carts, err := parseCleaningCartridges(data)
	if err != nil {
		t.Fatalf("parseCleaningCartridges(fixture): %v", err)
	}
	if len(carts) != 6 {
		t.Fatalf("parsed %d cartridges, want 6", len(carts))
	}

	// Field-for-field on the first entry, so a silently renamed JSON tag
	// fails here rather than as an all-zero series much later.
	first := carts[0]
	if first.Volser != "TST016JA" {
		t.Errorf("Volser = %q, want TST016JA", first.Volser)
	}
	if first.State != "normal" {
		t.Errorf("State = %q, want normal", first.State)
	}
	if first.Accessible != "normal" {
		t.Errorf("Accessible = %q, want normal", first.Accessible)
	}
	if first.CleansRemaining != 50 {
		t.Errorf("CleansRemaining = %d, want 50", first.CleansRemaining)
	}
	if first.Location != "slot_F7C3R27T4" {
		t.Errorf("Location = %q, want slot_F7C3R27T4", first.Location)
	}
	if first.MediaType != "3592" {
		t.Errorf("MediaType = %q, want 3592", first.MediaType)
	}
	if first.MostRecentUsage == nil || *first.MostRecentUsage != "2025-12-08T14:46:13+0000" {
		t.Errorf("MostRecentUsage = %v, want 2025-12-08T14:46:13+0000", first.MostRecentUsage)
	}

	// A cleansRemaining of 0 is a real reading, not a missing one: the
	// fixture carries two such cartridges, both parked in an I/O station.
	exhausted := 0
	for i := range carts {
		if carts[i].CleansRemaining == 0 {
			exhausted++
		}
	}
	if exhausted != 2 {
		t.Errorf("cartridges with 0 cleans left = %d, want 2", exhausted)
	}

	t.Run("duplicate volsers at different locations are accepted", func(t *testing.T) {
		// The defining property of this endpoint, and the reason every
		// series is keyed on the pair. R1.11.2's own example shows two
		// CLNI01L1 entries; the capture this fixture is trimmed from holds
		// seven such pairs. Rejecting them would drop half a real cartridge
		// population.
		seen := map[string]int{}
		for i := range carts {
			seen[carts[i].Volser]++
		}
		if seen["TST021JA"] != 2 || seen["TST048JA"] != 2 {
			t.Fatalf("fixture lost its duplicate volsers: TST021JA=%d TST048JA=%d, want 2 and 2",
				seen["TST021JA"], seen["TST048JA"])
		}
	})

	t.Run("empty array is accepted", func(t *testing.T) {
		// The one collector in this exporter where [] is a real reading. A
		// library CAN run out of cleaning cartridges, and that is exactly
		// what CleaningCartridgesExhausted pages on: rejecting [] here would
		// keep serving a stale list of cartridges that no longer exist at
		// the moment the alert most needed to fire.
		carts, err := parseCleaningCartridges([]byte(`[]`))
		if err != nil {
			t.Fatalf("parseCleaningCartridges([]) = %v, want nil: an empty library is a real reading", err)
		}
		if len(carts) != 0 {
			t.Fatalf("parsed %d cartridges from [], want 0", len(carts))
		}
	})

	t.Run("malformed JSON is rejected", func(t *testing.T) {
		if _, err := parseCleaningCartridges([]byte(`{"not":"an array"}`)); err == nil {
			t.Fatal("parseCleaningCartridges(object) = nil error, want a parse failure")
		}
	})

	t.Run("truncated JSON is rejected", func(t *testing.T) {
		if _, err := parseCleaningCartridges([]byte(`[{"volser":"TST001JA"`)); err == nil {
			t.Fatal("parseCleaningCartridges(truncated) = nil error, want a parse failure")
		}
	})

	t.Run("an empty volser is rejected", func(t *testing.T) {
		body := `[{"volser":"","state":"normal","location":"slot_F1C1R1T1"}]`
		if _, err := parseCleaningCartridges([]byte(body)); err == nil {
			t.Fatal("parseCleaningCartridges(empty volser) = nil error, want a rejection")
		}
	})

	t.Run("an empty location is rejected", func(t *testing.T) {
		body := `[{"volser":"TST001JA","state":"normal","location":""}]`
		if _, err := parseCleaningCartridges([]byte(body)); err == nil {
			t.Fatal("parseCleaningCartridges(empty location) = nil error, want a rejection")
		}
	})

	t.Run("a duplicate volser AND location pair is rejected", func(t *testing.T) {
		// Two metrics sharing a descriptor AND a label set fail
		// Registry.Gather for the whole scrape, so this one fails closed.
		body := `[
			{"volser":"TST001JA","state":"normal","location":"slot_F1C1R1T1"},
			{"volser":"TST001JA","state":"normal","location":"slot_F1C1R1T1"}
		]`
		if _, err := parseCleaningCartridges([]byte(body)); err == nil {
			t.Fatal("parseCleaningCartridges(duplicate pair) = nil error, want a rejection")
		}
	})
}

func TestCleaningCartridgesCollector_Describe(t *testing.T) {
	log := logger.NewTextLogger("error")
	c := NewCleaningCartridgesCollector(log, NewClient("http://example.invalid", time.Second), time.Hour, true)

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

// TestCleaningCartridgesCollector_DescribeIsConstantWithoutPerVolser pins the
// rule that a descriptor is what this collector CAN emit, not what it did last
// time: turning per-volser off changes the metrics, never the descriptors, and
// docs/metrics.md documents them on that basis.
func TestCleaningCartridgesCollector_DescribeIsConstantWithoutPerVolser(t *testing.T) {
	log := logger.NewTextLogger("error")
	c := NewCleaningCartridgesCollector(log, NewClient("http://example.invalid", time.Second), time.Hour, false)

	ch := make(chan *prometheus.Desc, 20)
	c.Describe(ch)
	close(ch)

	count := 0
	for range ch {
		count++
	}
	if count != 6 {
		t.Fatalf("Describe sent %d descriptors with per-volser off, want 6", count)
	}
}

// TestCleaningCartridgesCollector_Collect pins the exact exposition text
// against the fixture. refresh is driven directly rather than through Start, so
// the assertion is deterministic and carries no sleep: Start's own scheduling is
// what the lifecycle tests below cover.
func TestCleaningCartridgesCollector_Collect(t *testing.T) {
	_, c := cleaningCartridgesFixtureServer(t)
	c.refresh(context.Background())

	// Registry.Gather sorts families by name and, within a family, by label
	// value — so the two ioSlot cartridges lead, then the four slot ones by
	// location. TST021JA and TST048JA each appear twice, which is the whole
	// point: this block is the regression test for the volser-is-not-a-key
	// finding, and it would fail to gather at all if the pair were not the key.
	expected := `
# HELP tapelibrary_cleaning_cartridge_cleans_remaining Number of clean operations left on this cleaning cartridge. Emitted only when --collector.cleaning_cartridges.per-volser is set, which it is by default. Keyed by volser AND location: a barcode is not unique in a tape library, and this endpoint legitimately reports two cartridges under one volser.
# TYPE tapelibrary_cleaning_cartridge_cleans_remaining gauge
tapelibrary_cleaning_cartridge_cleans_remaining{access="normal",location="ioSlot_F2IOuR2",media_type="3592",state="normal",volser="TST017JA"} 0
tapelibrary_cleaning_cartridge_cleans_remaining{access="normal",location="ioSlot_F2IOuR4",media_type="3592",state="normal",volser="TST048JA"} 0
tapelibrary_cleaning_cartridge_cleans_remaining{access="normal",location="slot_F4C3R32T3",media_type="3592",state="normal",volser="TST021JA"} 50
tapelibrary_cleaning_cartridge_cleans_remaining{access="normal",location="slot_F6C6R19T0",media_type="3592",state="normal",volser="TST021JA"} 49
tapelibrary_cleaning_cartridge_cleans_remaining{access="normal",location="slot_F7C3R27T4",media_type="3592",state="normal",volser="TST016JA"} 50
tapelibrary_cleaning_cartridge_cleans_remaining{access="normal",location="slot_F8C1R19T2",media_type="3592",state="normal",volser="TST048JA"} 50
# HELP tapelibrary_cleaning_cartridge_last_usage_timestamp_seconds Unix time at which this cleaning cartridge was last mounted into a drive. A cartridge the library reports no usage for produces no series at all, rather than a 0 that would place its last clean in 1970. Emitted only when --collector.cleaning_cartridges.per-volser is set.
# TYPE tapelibrary_cleaning_cartridge_last_usage_timestamp_seconds gauge
tapelibrary_cleaning_cartridge_last_usage_timestamp_seconds{location="ioSlot_F2IOuR2",volser="TST017JA"} 1.780350378e+09
tapelibrary_cleaning_cartridge_last_usage_timestamp_seconds{location="ioSlot_F2IOuR4",volser="TST048JA"} 1.784128116e+09
tapelibrary_cleaning_cartridge_last_usage_timestamp_seconds{location="slot_F4C3R32T3",volser="TST021JA"} 1.772632621e+09
tapelibrary_cleaning_cartridge_last_usage_timestamp_seconds{location="slot_F6C6R19T0",volser="TST021JA"} 1.784886009e+09
tapelibrary_cleaning_cartridge_last_usage_timestamp_seconds{location="slot_F7C3R27T4",volser="TST016JA"} 1.765205173e+09
tapelibrary_cleaning_cartridge_last_usage_timestamp_seconds{location="slot_F8C1R19T2",volser="TST048JA"} 1.772632802e+09
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected),
		"tapelibrary_cleaning_cartridge_cleans_remaining",
		"tapelibrary_cleaning_cartridge_last_usage_timestamp_seconds",
	); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}

	// The library-wide aggregates. Every documented state is emitted whether
	// or not a cartridge is in it, so a rule matching state="normal" still has
	// a series to read when the count falls to zero. usable is 4, not 6: two
	// of the fixture's cartridges have no cleans left.
	expectedAggregates := `
# HELP tapelibrary_cleaning_cartridges Number of cleaning cartridges the library holds in each documented state. Always emitted, one series per known state, so a library holding none at all reports 0 rather than dropping the series.
# TYPE tapelibrary_cleaning_cartridges gauge
tapelibrary_cleaning_cartridges{state="exportQueued"} 0
tapelibrary_cleaning_cartridges{state="importing"} 0
tapelibrary_cleaning_cartridges{state="normal"} 6
# HELP tapelibrary_cleaning_cartridges_cleans_remaining Total clean operations left across every cleaning cartridge in the library. Always emitted, independently of --collector.cleaning_cartridges.per-volser, so turning that flag off costs per-cartridge detail but never the alert this value drives.
# TYPE tapelibrary_cleaning_cartridges_cleans_remaining gauge
tapelibrary_cleaning_cartridges_cleans_remaining 199
# HELP tapelibrary_cleaning_cartridges_usable Number of cleaning cartridges that are both in the normal state and have at least one clean left. Reaching 0 means the library can no longer clean a drive, whatever the raw cartridge count says.
# TYPE tapelibrary_cleaning_cartridges_usable gauge
tapelibrary_cleaning_cartridges_usable 4
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expectedAggregates),
		"tapelibrary_cleaning_cartridges",
		"tapelibrary_cleaning_cartridges_cleans_remaining",
		"tapelibrary_cleaning_cartridges_usable",
	); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}

	reg := prometheus.NewRegistry()
	if err := reg.Register(c); err != nil {
		t.Fatalf("Register: %v", err)
	}
	count, err := testutil.GatherAndCount(reg)
	if err != nil {
		// A duplicate label set fails Gather for the WHOLE scrape, so this
		// error is what a volser-only key would produce against a fixture
		// carrying two cartridges under one barcode.
		t.Fatalf("GatherAndCount: %v", err)
	}
	// 6 per-cartridge gauges + 6 usage timestamps + 3 state counts + the
	// summed cleans + the usable count + the freshness gauge.
	if count != 18 {
		t.Fatalf("GatherAndCount = %d, want 18", count)
	}
}

// TestCleaningCartridgesCollector_PerVolserOffKeepsAggregates is the guarantee
// that makes --collector.cleaning_cartridges.per-volser a pure cardinality
// lever. The flag drops the two per-cartridge families and nothing else: a site
// that turns it off loses the ability to name the exhausted cartridge, but
// CleaningCartridgesLow, CleaningCartridgesCritical and
// CleaningCartridgesExhausted all keep firing.
func TestCleaningCartridgesCollector_PerVolserOffKeepsAggregates(t *testing.T) {
	data, err := os.ReadFile("testdata/cleaning_cartridges.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	c := cleaningCartridgesServing(t, string(data), false)
	c.refresh(context.Background())

	expected := `
# HELP tapelibrary_cleaning_cartridges Number of cleaning cartridges the library holds in each documented state. Always emitted, one series per known state, so a library holding none at all reports 0 rather than dropping the series.
# TYPE tapelibrary_cleaning_cartridges gauge
tapelibrary_cleaning_cartridges{state="exportQueued"} 0
tapelibrary_cleaning_cartridges{state="importing"} 0
tapelibrary_cleaning_cartridges{state="normal"} 6
# HELP tapelibrary_cleaning_cartridges_cleans_remaining Total clean operations left across every cleaning cartridge in the library. Always emitted, independently of --collector.cleaning_cartridges.per-volser, so turning that flag off costs per-cartridge detail but never the alert this value drives.
# TYPE tapelibrary_cleaning_cartridges_cleans_remaining gauge
tapelibrary_cleaning_cartridges_cleans_remaining 199
# HELP tapelibrary_cleaning_cartridges_usable Number of cleaning cartridges that are both in the normal state and have at least one clean left. Reaching 0 means the library can no longer clean a drive, whatever the raw cartridge count says.
# TYPE tapelibrary_cleaning_cartridges_usable gauge
tapelibrary_cleaning_cartridges_usable 4
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected),
		"tapelibrary_cleaning_cartridges",
		"tapelibrary_cleaning_cartridges_cleans_remaining",
		"tapelibrary_cleaning_cartridges_usable",
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
	// 3 state counts + the summed cleans + the usable count + the freshness
	// gauge. The 12 per-cartridge series are gone, and only those.
	if count != 6 {
		t.Fatalf("GatherAndCount = %d, want 6 with per-volser off", count)
	}
}

// TestCleaningCartridgesCollector_EmptyLibraryReportsZero covers the reading
// this collector accepts and every other one in the exporter rejects. A library
// with no cleaning cartridges at all must report zeroes, not keep serving a
// comfortable stale cache, because that state is exactly what
// CleaningCartridgesExhausted pages on.
func TestCleaningCartridgesCollector_EmptyLibraryReportsZero(t *testing.T) {
	c := cleaningCartridgesServing(t, `[]`, true)
	c.refresh(context.Background())

	expected := `
# HELP tapelibrary_cleaning_cartridges Number of cleaning cartridges the library holds in each documented state. Always emitted, one series per known state, so a library holding none at all reports 0 rather than dropping the series.
# TYPE tapelibrary_cleaning_cartridges gauge
tapelibrary_cleaning_cartridges{state="exportQueued"} 0
tapelibrary_cleaning_cartridges{state="importing"} 0
tapelibrary_cleaning_cartridges{state="normal"} 0
# HELP tapelibrary_cleaning_cartridges_cleans_remaining Total clean operations left across every cleaning cartridge in the library. Always emitted, independently of --collector.cleaning_cartridges.per-volser, so turning that flag off costs per-cartridge detail but never the alert this value drives.
# TYPE tapelibrary_cleaning_cartridges_cleans_remaining gauge
tapelibrary_cleaning_cartridges_cleans_remaining 0
# HELP tapelibrary_cleaning_cartridges_usable Number of cleaning cartridges that are both in the normal state and have at least one clean left. Reaching 0 means the library can no longer clean a drive, whatever the raw cartridge count says.
# TYPE tapelibrary_cleaning_cartridges_usable gauge
tapelibrary_cleaning_cartridges_usable 0
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected),
		"tapelibrary_cleaning_cartridges",
		"tapelibrary_cleaning_cartridges_cleans_remaining",
		"tapelibrary_cleaning_cartridges_usable",
	); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}

	// StatusTracker counts emitted metrics per scrape, so an empty library
	// must still look like a live collector rather than a failed one.
	log := logger.NewTextLogger("error")
	tracker := NewStatusTracker(log)
	tracker.Add("cleaning_cartridges", c)

	expectedSuccess := `
# HELP ` + statusTrackerSuccessMetric + ` Whether the last scrape of the collector succeeded (1=success, 0=failure)
# TYPE ` + statusTrackerSuccessMetric + ` gauge
` + statusTrackerSuccessMetric + `{collector="cleaning_cartridges"} 1
`
	if err := testutil.CollectAndCompare(tracker, strings.NewReader(expectedSuccess), statusTrackerSuccessMetric); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}
}

// TestCleaningCartridgesCollector_UsableExcludesUnusableStates pins the reason
// `usable` is not simply the state family's state="normal" series: a cartridge
// queued for export or mid-import is reported by the library and counted in its
// state, but the robot will not select it to clean a drive.
func TestCleaningCartridgesCollector_UsableExcludesUnusableStates(t *testing.T) {
	body := `[
		{"volser":"TST001JA","state":"normal","accessible":"normal","cleansRemaining":40,"location":"slot_F1C1R1T1","mediaType":"3592","mostRecentUsage":null},
		{"volser":"TST002JA","state":"exportQueued","accessible":"normal","cleansRemaining":30,"location":"slot_F1C1R1T2","mediaType":"3592","mostRecentUsage":null},
		{"volser":"TST003JA","state":"importing","accessible":"normal","cleansRemaining":50,"location":"slot_F1C1R1T3","mediaType":"3592","mostRecentUsage":null},
		{"volser":"TST004JA","state":"normal","accessible":"normal","cleansRemaining":0,"location":"slot_F1C1R1T4","mediaType":"3592","mostRecentUsage":null}
	]`
	c := cleaningCartridgesServing(t, body, true)
	c.refresh(context.Background())

	// Four cartridges, 120 cleans between them, but only ONE the library can
	// actually use: one is queued for export, one is importing, one is spent.
	expected := `
# HELP tapelibrary_cleaning_cartridges Number of cleaning cartridges the library holds in each documented state. Always emitted, one series per known state, so a library holding none at all reports 0 rather than dropping the series.
# TYPE tapelibrary_cleaning_cartridges gauge
tapelibrary_cleaning_cartridges{state="exportQueued"} 1
tapelibrary_cleaning_cartridges{state="importing"} 1
tapelibrary_cleaning_cartridges{state="normal"} 2
# HELP tapelibrary_cleaning_cartridges_cleans_remaining Total clean operations left across every cleaning cartridge in the library. Always emitted, independently of --collector.cleaning_cartridges.per-volser, so turning that flag off costs per-cartridge detail but never the alert this value drives.
# TYPE tapelibrary_cleaning_cartridges_cleans_remaining gauge
tapelibrary_cleaning_cartridges_cleans_remaining 120
# HELP tapelibrary_cleaning_cartridges_usable Number of cleaning cartridges that are both in the normal state and have at least one clean left. Reaching 0 means the library can no longer clean a drive, whatever the raw cartridge count says.
# TYPE tapelibrary_cleaning_cartridges_usable gauge
tapelibrary_cleaning_cartridges_usable 1
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected),
		"tapelibrary_cleaning_cartridges",
		"tapelibrary_cleaning_cartridges_cleans_remaining",
		"tapelibrary_cleaning_cartridges_usable",
	); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}
}

// TestCleaningCartridgesCollector_UndocumentedStateIsStillCounted matters more
// on this endpoint than on any other: R1.11.2 documents `unknown` for every
// sibling cartridge type and omits it here alone, so this branch is what
// settles whether that omission is real if a library ever reports one.
func TestCleaningCartridgesCollector_UndocumentedStateIsStillCounted(t *testing.T) {
	body := `[
		{"volser":"TST001JA","state":"normal","accessible":"normal","cleansRemaining":40,"location":"slot_F1C1R1T1","mediaType":"3592","mostRecentUsage":null},
		{"volser":"TST002JA","state":"unknown","accessible":"normal","cleansRemaining":10,"location":"slot_F1C1R1T2","mediaType":"3592","mostRecentUsage":null}
	]`
	c := cleaningCartridgesServing(t, body, true)
	c.refresh(context.Background())

	expected := `
# HELP tapelibrary_cleaning_cartridges Number of cleaning cartridges the library holds in each documented state. Always emitted, one series per known state, so a library holding none at all reports 0 rather than dropping the series.
# TYPE tapelibrary_cleaning_cartridges gauge
tapelibrary_cleaning_cartridges{state="exportQueued"} 0
tapelibrary_cleaning_cartridges{state="importing"} 0
tapelibrary_cleaning_cartridges{state="normal"} 1
tapelibrary_cleaning_cartridges{state="unknown"} 1
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected), "tapelibrary_cleaning_cartridges"); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}

	// An undocumented state is not a usable one: `usable` reads state
	// equality against "normal", so the unknown cartridge's 10 remaining
	// cleans count toward the total but not toward what the library can use.
	expectedUsable := `
# HELP tapelibrary_cleaning_cartridges_usable Number of cleaning cartridges that are both in the normal state and have at least one clean left. Reaching 0 means the library can no longer clean a drive, whatever the raw cartridge count says.
# TYPE tapelibrary_cleaning_cartridges_usable gauge
tapelibrary_cleaning_cartridges_usable 1
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expectedUsable), "tapelibrary_cleaning_cartridges_usable"); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}
}

// TestCleaningCartridgesCollector_NullUsageEmitsNoSeries covers the documented
// "never mounted, or the library lost the record" case. A 0 there would place
// the last clean in 1970 and make every "unused since" query silently wrong.
func TestCleaningCartridgesCollector_NullUsageEmitsNoSeries(t *testing.T) {
	body := `[
		{"volser":"TST001JA","state":"normal","accessible":"normal","cleansRemaining":50,"location":"slot_F1C1R1T1","mediaType":"3592","mostRecentUsage":null}
	]`
	c := cleaningCartridgesServing(t, body, true)
	c.refresh(context.Background())

	reg := prometheus.NewRegistry()
	if err := reg.Register(c); err != nil {
		t.Fatalf("Register: %v", err)
	}
	count, err := testutil.GatherAndCount(reg, "tapelibrary_cleaning_cartridge_last_usage_timestamp_seconds")
	if err != nil {
		t.Fatalf("GatherAndCount: %v", err)
	}
	if count != 0 {
		t.Fatalf("last-usage series = %d, want 0: a null mostRecentUsage must emit nothing", count)
	}

	// The cartridge itself is still fully reported: only the timestamp is
	// withheld.
	cleans, err := testutil.GatherAndCount(reg, "tapelibrary_cleaning_cartridge_cleans_remaining")
	if err != nil {
		t.Fatalf("GatherAndCount: %v", err)
	}
	if cleans != 1 {
		t.Fatalf("cleans-remaining series = %d, want 1", cleans)
	}
}

// TestCleaningCartridgesCollector_UnparseableUsageEmitsNoSeries covers a
// timestamp this collector does not understand — here RFC3339, whose zone
// offset carries a colon the library's own format does not. Logged and skipped,
// never guessed at.
func TestCleaningCartridgesCollector_UnparseableUsageEmitsNoSeries(t *testing.T) {
	body := `[
		{"volser":"TST001JA","state":"normal","accessible":"normal","cleansRemaining":50,"location":"slot_F1C1R1T1","mediaType":"3592","mostRecentUsage":"2026-07-24T09:40:09+00:00"}
	]`
	c := cleaningCartridgesServing(t, body, true)
	c.refresh(context.Background())

	reg := prometheus.NewRegistry()
	if err := reg.Register(c); err != nil {
		t.Fatalf("Register: %v", err)
	}
	count, err := testutil.GatherAndCount(reg, "tapelibrary_cleaning_cartridge_last_usage_timestamp_seconds")
	if err != nil {
		t.Fatalf("GatherAndCount: %v", err)
	}
	if count != 0 {
		t.Fatalf("last-usage series = %d, want 0: an unparseable timestamp must emit nothing", count)
	}
}

func TestCleaningCartridgesCollector_DoneClosesOnCancel(t *testing.T) {
	log := logger.NewTextLogger("error")
	c := NewCleaningCartridgesCollector(log, NewClient("http://example.invalid", time.Second), time.Hour, true)
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

// TestCleaningCartridgesCollector_CollectServesCacheWithoutIO proves Collect
// never calls the library: after Start's own immediate refresh completes, a long
// interval (1 hour) guarantees the ticker cannot fire again during this test, so
// any further request the server receives could only come from Collect itself
// calling out, which the design forbids.
func TestCleaningCartridgesCollector_CollectServesCacheWithoutIO(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.ServeFile(w, r, "testdata/cleaning_cartridges.json")
	}))
	defer srv.Close()

	log := logger.NewTextLogger("error")
	c := NewCleaningCartridgesCollector(log, NewClient(srv.URL, time.Second), time.Hour, true)
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

// TestCleaningCartridgesCollector_ErrorHandling drives a refresh against a
// library that only ever fails. No cache was ever filled, so the scrape must
// carry exactly the freshness gauge, and must neither panic nor emit a partial
// set of aggregates.
func TestCleaningCartridgesCollector_ErrorHandling(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	log := logger.NewTextLogger("error")
	c := NewCleaningCartridgesCollector(log, NewClient(srv.URL, time.Second), time.Hour, true)
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

// TestCleaningCartridgesCollector_ErrorKeepsPreviousCache scripts the backend to
// succeed once, then fail on every later call, and drives at least one more
// refresh via a short interval. The cache from the successful first refresh must
// survive the later failure (fail-open, per refresh's doc comment) rather than
// being cleared or replaced with nothing.
//
// A cleared cache would drop tapelibrary_cleaning_cartridges_cleans_remaining
// entirely, and `< 100` does not match an absent series, so a library that
// became unreachable would silently stop being watched rather than alerting —
// the exact failure mode the ported legacy thresholds exist to prevent.
func TestCleaningCartridgesCollector_ErrorKeepsPreviousCache(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			http.ServeFile(w, r, "testdata/cleaning_cartridges.json")
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	log := logger.NewTextLogger("error")
	c := NewCleaningCartridgesCollector(log, NewClient(srv.URL, time.Second), 20*time.Millisecond, true)
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
	// A surviving cache emits 18 metrics: the 17 cached from the successful
	// first refresh plus the freshness gauge. A wrongly-cleared cache would
	// emit only 1 (the gauge alone), which "> 0" cannot tell apart.
	if count != 18 {
		t.Fatalf("GatherAndCount = %d, want 18: the previous cache must survive a later refresh error", count)
	}
}

// TestCleaningCartridgesCollector_StatusTrackerSuccessOnFirstScrape covers the
// startup window before Start's first refresh has completed (Start is
// deliberately never called here). Collect must still emit exactly the freshness
// gauge, valued 0 (not a zero time.Time's large-negative Unix()), and
// StatusTracker must still report this collector as successful: "Collect ran and
// returned data" and "the data is fresh" are different questions.
func TestCleaningCartridgesCollector_StatusTrackerSuccessOnFirstScrape(t *testing.T) {
	log := logger.NewTextLogger("error")
	c := NewCleaningCartridgesCollector(log, NewClient("http://example.invalid", time.Second), time.Hour, true)
	// Do NOT call Start(): cache is empty, no refresh has ever run.

	expected := `
# HELP tapelibrary_cleaning_cartridges_last_refresh_timestamp_seconds Unix time of the last successful cleaning cartridges refresh. Alert if time() - this > 2 x the collector's configured interval.
# TYPE tapelibrary_cleaning_cartridges_last_refresh_timestamp_seconds gauge
tapelibrary_cleaning_cartridges_last_refresh_timestamp_seconds 0
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected)); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}

	tracker := NewStatusTracker(log)
	tracker.Add("cleaning_cartridges", c)

	expectedSuccess := `
# HELP ` + statusTrackerSuccessMetric + ` Whether the last scrape of the collector succeeded (1=success, 0=failure)
# TYPE ` + statusTrackerSuccessMetric + ` gauge
` + statusTrackerSuccessMetric + `{collector="cleaning_cartridges"} 1
`
	if err := testutil.CollectAndCompare(tracker, strings.NewReader(expectedSuccess), statusTrackerSuccessMetric); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}
}

// TestCleaningCartridgesCollector_StatusTrackerFailure pins the other half of
// the tracker contract: a collector whose refresh has never succeeded still
// emits its freshness gauge, so it reports as alive. That is deliberate. The
// freshness gauge's VALUE, not the success gauge, is what says the data is
// stale, and the rule for reading it is stated in the descriptor's own help
// text rather than shipped in monitoring/prometheus/alerts.yml — no collector
// in this exporter ships a staleness rule, since the right interval to alert
// on is per-deployment.
func TestCleaningCartridgesCollector_StatusTrackerFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	log := logger.NewTextLogger("error")
	c := NewCleaningCartridgesCollector(log, NewClient(srv.URL, time.Second), time.Hour, true)
	c.refresh(context.Background())

	tracker := NewStatusTracker(log)
	tracker.Add("cleaning_cartridges", c)

	expectedSuccess := `
# HELP ` + statusTrackerSuccessMetric + ` Whether the last scrape of the collector succeeded (1=success, 0=failure)
# TYPE ` + statusTrackerSuccessMetric + ` gauge
` + statusTrackerSuccessMetric + `{collector="cleaning_cartridges"} 1
`
	if err := testutil.CollectAndCompare(tracker, strings.NewReader(expectedSuccess), statusTrackerSuccessMetric); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}
}
