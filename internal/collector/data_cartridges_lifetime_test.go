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
	"github.com/prometheus/common/expfmt"

	"github.com/sckyzo/tapelibrary_exporter/internal/logger"
)

// dataCartridgesLifetimeFixtureServer serves
// testdata/data_cartridges_lifetime.json on every request and returns a
// collector already pointed at it, with per-volser detail OFF — the shipped
// default. Nothing is started: the caller decides whether to drive refresh
// directly (deterministic) or via Start (which is what the lifecycle tests below
// exercise).
func dataCartridgesLifetimeFixtureServer(t *testing.T) (*httptest.Server, *DataCartridgesLifetimeCollector) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "testdata/data_cartridges_lifetime.json")
	}))
	t.Cleanup(srv.Close)

	log := logger.NewTextLogger("error")
	return srv, NewDataCartridgesLifetimeCollector(log, NewClient(srv.URL, time.Second), time.Hour, false)
}

// dataCartridgesLifetimeServing returns a collector fed by a server that answers
// every request with body. Used by the branch tests below, which need input
// shapes the real capture does not contain and which must therefore not be
// invented inside testdata/data_cartridges_lifetime.json (that fixture stays
// faithful to the 2026-07-28 capture's own shapes).
func dataCartridgesLifetimeServing(t *testing.T, body string, perVolser bool) *DataCartridgesLifetimeCollector {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	log := logger.NewTextLogger("error")
	return NewDataCartridgesLifetimeCollector(log, NewClient(srv.URL, time.Second), time.Hour, perVolser)
}

// TestParseDataCartridgesLifetime exercises parseDataCartridgesLifetime (piece
// 2, the pure parser) with static byte fixtures: no HTTP, no collector, no
// logger, no goroutine involved.
func TestParseDataCartridgesLifetime(t *testing.T) {
	data, err := os.ReadFile("testdata/data_cartridges_lifetime.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	carts, err := parseDataCartridgesLifetime(data)
	if err != nil {
		t.Fatalf("parseDataCartridgesLifetime(fixture): %v", err)
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
	if first.InternalAddress != "0112C6" {
		t.Errorf("InternalAddress = %q, want 0112C6", first.InternalAddress)
	}
	for name, got := range map[string]struct {
		p    *int64
		want int64
	}{
		"MotionMeters":           {first.MotionMeters, 3170420},
		"Mounts":                 {first.Mounts, 702},
		"DataWrittenToCartridge": {first.DataWrittenToCartridge, 36023891},
		"ErrorsCorrectedRead":    {first.ErrorsCorrectedRead, 133},
		"ErrorsCorrectedWrite":   {first.ErrorsCorrectedWrite, 424},
		"ErrorsUncorrectedRead":  {first.ErrorsUncorrectedRead, 0},
		"ErrorsUncorrectedWrite": {first.ErrorsUncorrectedWrite, 3},
	} {
		if got.p == nil {
			t.Errorf("%s = nil, want %d", name, got.want)
			continue
		}
		if *got.p != got.want {
			t.Errorf("%s = %d, want %d", name, *got.p, got.want)
		}
	}

	// The cartridge-memory-absent cohort: all seven counters null together, and
	// unread() rather than valid() is what must classify it. A pointer that
	// decoded to 0 here would assert the cartridge had never moved.
	unreadEntry := carts[1]
	if unreadEntry.Volser != "TST084JD" {
		t.Fatalf("fixture entry 1 = %q, want the unread cartridge TST084JD", unreadEntry.Volser)
	}
	if !unreadEntry.unread() {
		t.Error("unread() = false on the all-null cartridge, want true")
	}
	if unreadEntry.valid() {
		t.Error("valid() = true on the all-null cartridge, want false")
	}

	// The corrupt reading the capture actually contains: a negative
	// motionMeters beside 50 mounts and 0 bytes written. It is NOT unread (it
	// reported something), and it is NOT valid (what it reported is impossible).
	invalidEntry := carts[2]
	if invalidEntry.Volser != "TST090JD" {
		t.Fatalf("fixture entry 2 = %q, want the corrupt cartridge TST090JD", invalidEntry.Volser)
	}
	if invalidEntry.unread() {
		t.Error("unread() = true on the negative-counter cartridge, want false: it did report values")
	}
	if invalidEntry.valid() {
		t.Error("valid() = true on the negative-counter cartridge, want false")
	}

	t.Run("rejects malformed JSON", func(t *testing.T) {
		if _, err := parseDataCartridgesLifetime([]byte(`{`)); err == nil {
			t.Fatal("parseDataCartridgesLifetime(`{`) = nil error, want a parse error")
		}
	})

	t.Run("rejects an empty array", func(t *testing.T) {
		// A library reporting zero cartridges has lost its inventory or
		// truncated the body: keeping the previous cache is the safer reading.
		if _, err := parseDataCartridgesLifetime([]byte(`[]`)); err == nil {
			t.Fatal("parseDataCartridgesLifetime(`[]`) = nil error, want an error")
		}
	})

	t.Run("rejects an empty volser", func(t *testing.T) {
		body := `[{"volser":"","internalAddress":"0112C6","motionMeters":1,"mounts":1,"dataWrittenToCartridge":1,"errorsCorrectedRead":0,"errorsCorrectedWrite":0,"errorsUncorrectedRead":0,"errorsUncorrectedWrite":0}]`
		if _, err := parseDataCartridgesLifetime([]byte(body)); err == nil {
			t.Fatal("empty volser accepted, want an error")
		}
	})

	t.Run("rejects an empty internal address", func(t *testing.T) {
		body := `[{"volser":"TST001JD","internalAddress":"","motionMeters":1,"mounts":1,"dataWrittenToCartridge":1,"errorsCorrectedRead":0,"errorsCorrectedWrite":0,"errorsUncorrectedRead":0,"errorsUncorrectedWrite":0}]`
		if _, err := parseDataCartridgesLifetime([]byte(body)); err == nil {
			t.Fatal("empty internal address accepted, want an error")
		}
	})

	// The one place this collector is stricter than its siblings, and the
	// reason is Registry.Gather rather than tidiness: this endpoint reports no
	// location, so two entries under one volser would give two metrics the same
	// descriptor and label set and fail the scrape for every collector and every
	// library at once. The error must name both internal addresses, or the
	// offending pair cannot be found.
	t.Run("rejects a duplicate volser and names both addresses", func(t *testing.T) {
		body := `[
			{"volser":"TST001JD","internalAddress":"0112C6","motionMeters":1,"mounts":1,"dataWrittenToCartridge":1,"errorsCorrectedRead":0,"errorsCorrectedWrite":0,"errorsUncorrectedRead":0,"errorsUncorrectedWrite":0},
			{"volser":"TST001JD","internalAddress":"030403","motionMeters":2,"mounts":2,"dataWrittenToCartridge":2,"errorsCorrectedRead":0,"errorsCorrectedWrite":0,"errorsUncorrectedRead":0,"errorsUncorrectedWrite":0}
		]`
		_, err := parseDataCartridgesLifetime([]byte(body))
		if err == nil {
			t.Fatal("duplicate volser accepted, want an error")
		}
		for _, want := range []string{"TST001JD", "0112C6", "030403"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not name %q", err, want)
			}
		}
	})

	// A volser repeated across DIFFERENT responses is not a duplicate: the
	// rejection is per response, so a stable inventory parses on every refresh.
	t.Run("accepts the same volser on a later response", func(t *testing.T) {
		body := `[{"volser":"TST001JD","internalAddress":"0112C6","motionMeters":1,"mounts":1,"dataWrittenToCartridge":1,"errorsCorrectedRead":0,"errorsCorrectedWrite":0,"errorsUncorrectedRead":0,"errorsUncorrectedWrite":0}]`
		for i := 0; i < 2; i++ {
			if _, err := parseDataCartridgesLifetime([]byte(body)); err != nil {
				t.Fatalf("parse %d: %v", i, err)
			}
		}
	})
}

// TestDataCartridgesLifetimeCollector_ValidRejectsAPartialRecord covers the
// shape the 2026-07-28 capture does not contain: some counters present, others
// null. It is handled anyway because the cost is one branch and the failure mode
// of not handling it is a silent 0 in whichever family lost its field.
func TestDataCartridgesLifetimeCollector_ValidRejectsAPartialRecord(t *testing.T) {
	body := `[{"volser":"TST001JD","internalAddress":"0112C6","motionMeters":100,"mounts":null,"dataWrittenToCartridge":1,"errorsCorrectedRead":0,"errorsCorrectedWrite":0,"errorsUncorrectedRead":0,"errorsUncorrectedWrite":0}]`
	carts, err := parseDataCartridgesLifetime([]byte(body))
	if err != nil {
		t.Fatalf("parseDataCartridgesLifetime: %v", err)
	}
	if carts[0].unread() {
		t.Error("unread() = true on a partial record, want false: six of seven counters are present")
	}
	if carts[0].valid() {
		t.Error("valid() = true on a partial record, want false")
	}

	// And it must land under reason="invalid", not silently vanish from both
	// gauges and leave the arithmetic short.
	c := dataCartridgesLifetimeServing(t, body, false)
	c.refresh(context.Background())

	expected := `
# HELP tapelibrary_data_cartridges_usage_unknown Number of data cartridges excluded from the usage histograms, by reason. Always emitted for both reasons. reason="unread" is a cartridge whose memory the library has not read, which returns all seven counters as null at once and is the endpoint's ordinary shape rather than a fault (39% of the reference capture). reason="invalid" is a cartridge that reported a negative counter or a partial record, which is a cartridge-memory fault worth investigating. Each histogram's _count plus both of these is the library's full cartridge parc.
# TYPE tapelibrary_data_cartridges_usage_unknown gauge
tapelibrary_data_cartridges_usage_unknown{reason="invalid"} 1
tapelibrary_data_cartridges_usage_unknown{reason="unread"} 0
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected), "tapelibrary_data_cartridges_usage_unknown"); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}
}

func TestDataCartridgesLifetimeCollector_Describe(t *testing.T) {
	log := logger.NewTextLogger("error")
	c := NewDataCartridgesLifetimeCollector(log, NewClient("http://example.invalid", time.Second), time.Hour, true)

	ch := make(chan *prometheus.Desc, 30)
	c.Describe(ch)
	close(ch)

	count := 0
	for range ch {
		count++
	}
	if count != 10 {
		t.Fatalf("Describe sent %d descriptors, want 10", count)
	}
}

// TestDataCartridgesLifetimeCollector_DescribeIsConstantWithoutPerVolser pins
// the rule that a descriptor is what this collector CAN emit, not what it did
// last time: turning per-volser on changes the metrics, never the descriptors,
// and docs/metrics.md documents them on that basis.
func TestDataCartridgesLifetimeCollector_DescribeIsConstantWithoutPerVolser(t *testing.T) {
	log := logger.NewTextLogger("error")
	c := NewDataCartridgesLifetimeCollector(log, NewClient("http://example.invalid", time.Second), time.Hour, false)

	ch := make(chan *prometheus.Desc, 30)
	c.Describe(ch)
	close(ch)

	count := 0
	for range ch {
		count++
	}
	if count != 10 {
		t.Fatalf("Describe sent %d descriptors with per-volser off, want 10", count)
	}
}

// TestDataCartridgesLifetimeCollector_Collect pins the exact exposition text of
// every always-emitted aggregate against the fixture, at the shipped default
// flag settings. refresh is driven directly rather than through Start, so the
// assertion is deterministic and carries no sleep: Start's own scheduling is
// what the lifecycle tests below cover.
func TestDataCartridgesLifetimeCollector_Collect(t *testing.T) {
	_, c := dataCartridgesLifetimeFixtureServer(t)
	c.refresh(context.Background())

	// Six of the fixture's eight cartridges have a usable record. The other two
	// are the endpoint's two documented ways of having none, and they are
	// counted apart rather than together: TST084JD reported nothing at all
	// (unread, benign, 39% of the real capture), TST090JD reported a negative
	// motionMeters (invalid, a cartridge-memory fault). Every histogram below
	// has _count 6, and 6 + 1 + 1 = 8 closes the arithmetic against the parc.
	expectedDistributions := `
# HELP tapelibrary_data_cartridges_usage_motion_meters Distribution of metres of tape drawn across the drive head over each data cartridge's lifetime. Always emitted. Cartridges the library reports no usable reading for are counted in tapelibrary_data_cartridges_usage_unknown instead of being observed here, so this histogram's _count plus that gauge is the library's full cartridge parc.
# TYPE tapelibrary_data_cartridges_usage_motion_meters histogram
tapelibrary_data_cartridges_usage_motion_meters_bucket{le="500000"} 2
tapelibrary_data_cartridges_usage_motion_meters_bucket{le="1e+06"} 2
tapelibrary_data_cartridges_usage_motion_meters_bucket{le="2.5e+06"} 2
tapelibrary_data_cartridges_usage_motion_meters_bucket{le="5e+06"} 4
tapelibrary_data_cartridges_usage_motion_meters_bucket{le="1e+07"} 5
tapelibrary_data_cartridges_usage_motion_meters_bucket{le="2e+07"} 6
tapelibrary_data_cartridges_usage_motion_meters_bucket{le="+Inf"} 6
tapelibrary_data_cartridges_usage_motion_meters_sum 2.8462251e+07
tapelibrary_data_cartridges_usage_motion_meters_count 6
# HELP tapelibrary_data_cartridges_usage_mounts Distribution of the number of times each data cartridge has been loaded into a drive over its lifetime. Always emitted. Cartridges the library reports no usable reading for are counted in tapelibrary_data_cartridges_usage_unknown instead of being observed here.
# TYPE tapelibrary_data_cartridges_usage_mounts histogram
tapelibrary_data_cartridges_usage_mounts_bucket{le="50"} 2
tapelibrary_data_cartridges_usage_mounts_bucket{le="250"} 3
tapelibrary_data_cartridges_usage_mounts_bucket{le="500"} 4
tapelibrary_data_cartridges_usage_mounts_bucket{le="1000"} 5
tapelibrary_data_cartridges_usage_mounts_bucket{le="2500"} 6
tapelibrary_data_cartridges_usage_mounts_bucket{le="5000"} 6
tapelibrary_data_cartridges_usage_mounts_bucket{le="+Inf"} 6
tapelibrary_data_cartridges_usage_mounts_sum 2244
tapelibrary_data_cartridges_usage_mounts_count 6
# HELP tapelibrary_data_cartridges_usage_written_bytes Distribution of bytes written to each data cartridge over its lifetime. Converted from the API's megabytes, read decimally (1 MB = 1e6 bytes) to match how this media's capacity is advertised. Always emitted. Cartridges the library reports no usable reading for are counted in tapelibrary_data_cartridges_usage_unknown instead of being observed here.
# TYPE tapelibrary_data_cartridges_usage_written_bytes histogram
tapelibrary_data_cartridges_usage_written_bytes_bucket{le="1e+12"} 0
tapelibrary_data_cartridges_usage_written_bytes_bucket{le="5e+12"} 0
tapelibrary_data_cartridges_usage_written_bytes_bucket{le="1e+13"} 0
tapelibrary_data_cartridges_usage_written_bytes_bucket{le="2.5e+13"} 3
tapelibrary_data_cartridges_usage_written_bytes_bucket{le="5e+13"} 5
tapelibrary_data_cartridges_usage_written_bytes_bucket{le="1e+14"} 6
tapelibrary_data_cartridges_usage_written_bytes_bucket{le="+Inf"} 6
tapelibrary_data_cartridges_usage_written_bytes_sum 1.78519996e+14
tapelibrary_data_cartridges_usage_written_bytes_count 6
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expectedDistributions),
		"tapelibrary_data_cartridges_usage_motion_meters",
		"tapelibrary_data_cartridges_usage_mounts",
		"tapelibrary_data_cartridges_usage_written_bytes",
	); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}

	// All four (direction, correction) pairs are always emitted, including the
	// ones no cartridge in the fixture has a non-zero value for: a rule matching
	// correction="uncorrected" must have a series to read on a healthy library,
	// and an absent series satisfies no matcher.
	//
	// le="0" is the load-bearing bound. _count minus that bucket is the number
	// of cartridges carrying at least one error of that kind — 3 for uncorrected
	// reads, 2 for uncorrected writes — which is the quantity
	// DataCartridgeUncorrectedErrorsRising alerts on. The two cartridges past
	// le="10000" on the read side are TST091JD (4 537) and TST143JD (16 161),
	// and landing in +Inf is exactly what should make them findable.
	expectedErrors := `
# HELP tapelibrary_data_cartridges_usage_errors Distribution of lifetime error counts across the library's data cartridges, by transfer direction and by whether the library was able to correct them. Always emitted for all four combinations. The number of cartridges carrying at least one error of a given kind is this histogram's _count minus its le="0" bucket; a rising uncorrected count is media loss in progress and is what DataCartridgeUncorrectedErrorsRising reads.
# TYPE tapelibrary_data_cartridges_usage_errors histogram
tapelibrary_data_cartridges_usage_errors_bucket{correction="corrected",direction="read",le="0"} 2
tapelibrary_data_cartridges_usage_errors_bucket{correction="corrected",direction="read",le="1"} 2
tapelibrary_data_cartridges_usage_errors_bucket{correction="corrected",direction="read",le="10"} 2
tapelibrary_data_cartridges_usage_errors_bucket{correction="corrected",direction="read",le="100"} 2
tapelibrary_data_cartridges_usage_errors_bucket{correction="corrected",direction="read",le="1000"} 5
tapelibrary_data_cartridges_usage_errors_bucket{correction="corrected",direction="read",le="10000"} 5
tapelibrary_data_cartridges_usage_errors_bucket{correction="corrected",direction="read",le="+Inf"} 6
tapelibrary_data_cartridges_usage_errors_sum{correction="corrected",direction="read"} 12444
tapelibrary_data_cartridges_usage_errors_count{correction="corrected",direction="read"} 6
tapelibrary_data_cartridges_usage_errors_bucket{correction="corrected",direction="write",le="0"} 0
tapelibrary_data_cartridges_usage_errors_bucket{correction="corrected",direction="write",le="1"} 0
tapelibrary_data_cartridges_usage_errors_bucket{correction="corrected",direction="write",le="10"} 0
tapelibrary_data_cartridges_usage_errors_bucket{correction="corrected",direction="write",le="100"} 0
tapelibrary_data_cartridges_usage_errors_bucket{correction="corrected",direction="write",le="1000"} 6
tapelibrary_data_cartridges_usage_errors_bucket{correction="corrected",direction="write",le="10000"} 6
tapelibrary_data_cartridges_usage_errors_bucket{correction="corrected",direction="write",le="+Inf"} 6
tapelibrary_data_cartridges_usage_errors_sum{correction="corrected",direction="write"} 2437
tapelibrary_data_cartridges_usage_errors_count{correction="corrected",direction="write"} 6
tapelibrary_data_cartridges_usage_errors_bucket{correction="uncorrected",direction="read",le="0"} 3
tapelibrary_data_cartridges_usage_errors_bucket{correction="uncorrected",direction="read",le="1"} 3
tapelibrary_data_cartridges_usage_errors_bucket{correction="uncorrected",direction="read",le="10"} 3
tapelibrary_data_cartridges_usage_errors_bucket{correction="uncorrected",direction="read",le="100"} 3
tapelibrary_data_cartridges_usage_errors_bucket{correction="uncorrected",direction="read",le="1000"} 4
tapelibrary_data_cartridges_usage_errors_bucket{correction="uncorrected",direction="read",le="10000"} 5
tapelibrary_data_cartridges_usage_errors_bucket{correction="uncorrected",direction="read",le="+Inf"} 6
tapelibrary_data_cartridges_usage_errors_sum{correction="uncorrected",direction="read"} 21113
tapelibrary_data_cartridges_usage_errors_count{correction="uncorrected",direction="read"} 6
tapelibrary_data_cartridges_usage_errors_bucket{correction="uncorrected",direction="write",le="0"} 4
tapelibrary_data_cartridges_usage_errors_bucket{correction="uncorrected",direction="write",le="1"} 5
tapelibrary_data_cartridges_usage_errors_bucket{correction="uncorrected",direction="write",le="10"} 6
tapelibrary_data_cartridges_usage_errors_bucket{correction="uncorrected",direction="write",le="100"} 6
tapelibrary_data_cartridges_usage_errors_bucket{correction="uncorrected",direction="write",le="1000"} 6
tapelibrary_data_cartridges_usage_errors_bucket{correction="uncorrected",direction="write",le="10000"} 6
tapelibrary_data_cartridges_usage_errors_bucket{correction="uncorrected",direction="write",le="+Inf"} 6
tapelibrary_data_cartridges_usage_errors_sum{correction="uncorrected",direction="write"} 4
tapelibrary_data_cartridges_usage_errors_count{correction="uncorrected",direction="write"} 6
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expectedErrors),
		"tapelibrary_data_cartridges_usage_errors",
	); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}

	// Both reasons always emitted, including a zero. Splitting them is the point:
	// "unread" at 39% of a real library is normal and nothing should page on it,
	// while a single "invalid" is a cartridge whose memory contradicts itself.
	expectedUnknown := `
# HELP tapelibrary_data_cartridges_usage_unknown Number of data cartridges excluded from the usage histograms, by reason. Always emitted for both reasons. reason="unread" is a cartridge whose memory the library has not read, which returns all seven counters as null at once and is the endpoint's ordinary shape rather than a fault (39% of the reference capture). reason="invalid" is a cartridge that reported a negative counter or a partial record, which is a cartridge-memory fault worth investigating. Each histogram's _count plus both of these is the library's full cartridge parc.
# TYPE tapelibrary_data_cartridges_usage_unknown gauge
tapelibrary_data_cartridges_usage_unknown{reason="invalid"} 1
tapelibrary_data_cartridges_usage_unknown{reason="unread"} 1
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expectedUnknown),
		"tapelibrary_data_cartridges_usage_unknown",
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
	// 3 single-family histograms + 4 error combinations + 2 unknown reasons +
	// the freshness gauge. No per-cartridge series: the flag is off.
	//
	// **A histogram is ONE metric but ELEVEN series here**, so this figure and
	// the cardinality budget in docs/exporter-journal.md legitimately differ:
	// 10 metrics expand on the wire to 7 x (6 buckets + Inf + sum + count) = 63,
	// plus 2 + 1, for 66 series.
	if count != 10 {
		t.Fatalf("GatherAndCount = %d, want 10", count)
	}
}

// TestDataCartridgesLifetimeCollector_PerVolserAddsPerCartridgeSeries is the
// guarantee that makes --collector.data_cartridges_lifetime.per-volser a pure
// cardinality lever. Turning it on adds the four per-cartridge families and
// changes nothing else.
func TestDataCartridgesLifetimeCollector_PerVolserAddsPerCartridgeSeries(t *testing.T) {
	data, err := os.ReadFile("testdata/data_cartridges_lifetime.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	c := dataCartridgesLifetimeServing(t, string(data), true)
	c.refresh(context.Background())

	// Only the six cartridges with a usable record get per-cartridge series.
	// The unread and the corrupt one produce NO series at all rather than a 0,
	// which is the same rule DataCartridgesCollector applies to a null
	// lifetimeRemaining: absent is a missing reading, 0 is an assertion.
	expected := `
# HELP tapelibrary_data_cartridge_usage_mounts_total Number of times this individual data cartridge has been loaded into a drive over its lifetime, as the library's own cumulative counter. Emitted only when --collector.data_cartridges_lifetime.per-volser is set. A cartridge with no usable reading produces no series at all.
# TYPE tapelibrary_data_cartridge_usage_mounts_total counter
tapelibrary_data_cartridge_usage_mounts_total{volser="TST083JD"} 702
tapelibrary_data_cartridge_usage_mounts_total{volser="TST091JD"} 145
tapelibrary_data_cartridge_usage_mounts_total{volser="TST095JD"} 17
tapelibrary_data_cartridge_usage_mounts_total{volser="TST099JD"} 16
tapelibrary_data_cartridge_usage_mounts_total{volser="TST143JD"} 347
tapelibrary_data_cartridge_usage_mounts_total{volser="TST147JD"} 1017
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected),
		"tapelibrary_data_cartridge_usage_mounts_total",
	); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}

	// The four error counters must not be transposed, which is why the whole
	// family is stated rather than a sample of it. TST083JD is the fixture's
	// only cartridge with a non-zero uncorrected WRITE count (3) beside a zero
	// uncorrected READ count, and TST143JD is its mirror image (16 161 read, 0
	// write), so a read/write swap fails here rather than shipping a plausible
	// dashboard that names the wrong direction.
	expectedErrors := `
# HELP tapelibrary_data_cartridge_usage_errors_total Lifetime error count for this individual data cartridge, by transfer direction and by whether the library was able to correct them, as the library's own cumulative counters. Emitted only when --collector.data_cartridges_lifetime.per-volser is set, and then for all four combinations. A cartridge with no usable reading produces no series at all.
# TYPE tapelibrary_data_cartridge_usage_errors_total counter
tapelibrary_data_cartridge_usage_errors_total{correction="corrected",direction="read",volser="TST083JD"} 133
tapelibrary_data_cartridge_usage_errors_total{correction="corrected",direction="read",volser="TST091JD"} 11518
tapelibrary_data_cartridge_usage_errors_total{correction="corrected",direction="read",volser="TST095JD"} 0
tapelibrary_data_cartridge_usage_errors_total{correction="corrected",direction="read",volser="TST099JD"} 0
tapelibrary_data_cartridge_usage_errors_total{correction="corrected",direction="read",volser="TST143JD"} 188
tapelibrary_data_cartridge_usage_errors_total{correction="corrected",direction="read",volser="TST147JD"} 605
tapelibrary_data_cartridge_usage_errors_total{correction="corrected",direction="write",volser="TST083JD"} 424
tapelibrary_data_cartridge_usage_errors_total{correction="corrected",direction="write",volser="TST091JD"} 549
tapelibrary_data_cartridge_usage_errors_total{correction="corrected",direction="write",volser="TST095JD"} 179
tapelibrary_data_cartridge_usage_errors_total{correction="corrected",direction="write",volser="TST099JD"} 160
tapelibrary_data_cartridge_usage_errors_total{correction="corrected",direction="write",volser="TST143JD"} 447
tapelibrary_data_cartridge_usage_errors_total{correction="corrected",direction="write",volser="TST147JD"} 678
tapelibrary_data_cartridge_usage_errors_total{correction="uncorrected",direction="read",volser="TST083JD"} 0
tapelibrary_data_cartridge_usage_errors_total{correction="uncorrected",direction="read",volser="TST091JD"} 4537
tapelibrary_data_cartridge_usage_errors_total{correction="uncorrected",direction="read",volser="TST095JD"} 0
tapelibrary_data_cartridge_usage_errors_total{correction="uncorrected",direction="read",volser="TST099JD"} 0
tapelibrary_data_cartridge_usage_errors_total{correction="uncorrected",direction="read",volser="TST143JD"} 16161
tapelibrary_data_cartridge_usage_errors_total{correction="uncorrected",direction="read",volser="TST147JD"} 415
tapelibrary_data_cartridge_usage_errors_total{correction="uncorrected",direction="write",volser="TST083JD"} 3
tapelibrary_data_cartridge_usage_errors_total{correction="uncorrected",direction="write",volser="TST091JD"} 0
tapelibrary_data_cartridge_usage_errors_total{correction="uncorrected",direction="write",volser="TST095JD"} 0
tapelibrary_data_cartridge_usage_errors_total{correction="uncorrected",direction="write",volser="TST099JD"} 0
tapelibrary_data_cartridge_usage_errors_total{correction="uncorrected",direction="write",volser="TST143JD"} 0
tapelibrary_data_cartridge_usage_errors_total{correction="uncorrected",direction="write",volser="TST147JD"} 1
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expectedErrors),
		"tapelibrary_data_cartridge_usage_errors_total",
	); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}

	// Bytes, not megabytes: 36 023 891 MB x 1e6.
	expectedWritten := `
# HELP tapelibrary_data_cartridge_usage_written_bytes_total Bytes written to this individual data cartridge over its lifetime, as the library's own cumulative counter converted from the API's decimal megabytes. Emitted only when --collector.data_cartridges_lifetime.per-volser is set. A cartridge with no usable reading produces no series at all.
# TYPE tapelibrary_data_cartridge_usage_written_bytes_total counter
tapelibrary_data_cartridge_usage_written_bytes_total{volser="TST083JD"} 3.6023891e+13
tapelibrary_data_cartridge_usage_written_bytes_total{volser="TST091JD"} 1.7772314e+13
tapelibrary_data_cartridge_usage_written_bytes_total{volser="TST095JD"} 1.509634e+13
tapelibrary_data_cartridge_usage_written_bytes_total{volser="TST099JD"} 1.314158e+13
tapelibrary_data_cartridge_usage_written_bytes_total{volser="TST143JD"} 2.527836e+13
tapelibrary_data_cartridge_usage_written_bytes_total{volser="TST147JD"} 7.1207511e+13
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expectedWritten),
		"tapelibrary_data_cartridge_usage_written_bytes_total",
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
	// The 10 aggregate metrics plus 6 cartridges x 7 per-cartridge metrics.
	if count != 52 {
		t.Fatalf("GatherAndCount = %d, want 52", count)
	}
}

// TestDataCartridgesLifetimeCollector_PerVolserKeepsAggregatesIdentical is what
// makes the flag safe to leave off: every aggregate an alert reads is
// byte-identical either way, so flipping it is a pure sizing decision and no
// rule depends on it.
func TestDataCartridgesLifetimeCollector_PerVolserKeepsAggregatesIdentical(t *testing.T) {
	data, err := os.ReadFile("testdata/data_cartridges_lifetime.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	aggregates := []string{
		"tapelibrary_data_cartridges_usage_motion_meters",
		"tapelibrary_data_cartridges_usage_mounts",
		"tapelibrary_data_cartridges_usage_written_bytes",
		"tapelibrary_data_cartridges_usage_errors",
		"tapelibrary_data_cartridges_usage_unknown",
	}

	off := dataCartridgesLifetimeServing(t, string(data), false)
	off.refresh(context.Background())
	on := dataCartridgesLifetimeServing(t, string(data), true)
	on.refresh(context.Background())

	renderedOff, err := testutil.CollectAndFormat(off, expfmt.TypeTextPlain, aggregates...)
	if err != nil {
		t.Fatalf("CollectAndFormat (per-volser off): %v", err)
	}
	renderedOn, err := testutil.CollectAndFormat(on, expfmt.TypeTextPlain, aggregates...)
	if err != nil {
		t.Fatalf("CollectAndFormat (per-volser on): %v", err)
	}

	if string(renderedOff) != string(renderedOn) {
		t.Fatalf("aggregates differ with per-volser on:\n--- off ---\n%s\n--- on ---\n%s", renderedOff, renderedOn)
	}
	// Guard against the assertion passing because both sides rendered nothing.
	if !strings.Contains(string(renderedOff), "tapelibrary_data_cartridges_usage_motion_meters_count 6") {
		t.Fatalf("rendered aggregates look empty, so the comparison above proved nothing:\n%s", renderedOff)
	}
}

// TestDataCartridgesLifetimeCollector_NegativeCounterIsNotZero is the collector's
// defining decision, pinned. The capture's TST090JD reports motionMeters
// -285 211 648 beside 50 mounts and 0 bytes written. Clamping it to 0 would
// assert a mounted cartridge has never moved; passing it through would drag the
// histogram's _sum negative and make rate() on it meaningless. It is excluded
// from every family and counted under reason="invalid" instead.
func TestDataCartridgesLifetimeCollector_NegativeCounterIsNotZero(t *testing.T) {
	_, c := dataCartridgesLifetimeFixtureServer(t)
	c.refresh(context.Background())

	// A _sum of 28 462 251 is the six valid cartridges only. Including
	// TST090JD's -285 211 648 would give a NEGATIVE sum; clamping it to 0 would
	// leave the sum unchanged but push _count to 7 and le="500000" to 3.
	expected := `
# HELP tapelibrary_data_cartridges_usage_motion_meters Distribution of metres of tape drawn across the drive head over each data cartridge's lifetime. Always emitted. Cartridges the library reports no usable reading for are counted in tapelibrary_data_cartridges_usage_unknown instead of being observed here, so this histogram's _count plus that gauge is the library's full cartridge parc.
# TYPE tapelibrary_data_cartridges_usage_motion_meters histogram
tapelibrary_data_cartridges_usage_motion_meters_bucket{le="500000"} 2
tapelibrary_data_cartridges_usage_motion_meters_bucket{le="1e+06"} 2
tapelibrary_data_cartridges_usage_motion_meters_bucket{le="2.5e+06"} 2
tapelibrary_data_cartridges_usage_motion_meters_bucket{le="5e+06"} 4
tapelibrary_data_cartridges_usage_motion_meters_bucket{le="1e+07"} 5
tapelibrary_data_cartridges_usage_motion_meters_bucket{le="2e+07"} 6
tapelibrary_data_cartridges_usage_motion_meters_bucket{le="+Inf"} 6
tapelibrary_data_cartridges_usage_motion_meters_sum 2.8462251e+07
tapelibrary_data_cartridges_usage_motion_meters_count 6
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected),
		"tapelibrary_data_cartridges_usage_motion_meters",
	); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}

	// And no per-cartridge series for it either, even with the flag on.
	data, err := os.ReadFile("testdata/data_cartridges_lifetime.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	withDetail := dataCartridgesLifetimeServing(t, string(data), true)
	withDetail.refresh(context.Background())

	if n := testutil.CollectAndCount(withDetail, "tapelibrary_data_cartridge_usage_motion_meters_total"); n != 6 {
		t.Fatalf("per-cartridge motion series = %d, want 6: the corrupt and unread cartridges must emit none", n)
	}
}

// TestDataCartridgesLifetimeCollector_UnreadCohortEmitsNoPerCartridgeSeries
// covers the 39% of a real library that reports no cartridge memory at all.
// Absent, never zero: a 0 on a cumulative counter asserts the cartridge has
// never been mounted, which is a fabricated reading rather than a missing one.
func TestDataCartridgesLifetimeCollector_UnreadCohortEmitsNoPerCartridgeSeries(t *testing.T) {
	body := `[{"volser":"TST001JD","internalAddress":"0112C6","motionMeters":null,"mounts":null,"dataWrittenToCartridge":null,"errorsCorrectedRead":null,"errorsCorrectedWrite":null,"errorsUncorrectedRead":null,"errorsUncorrectedWrite":null}]`
	c := dataCartridgesLifetimeServing(t, body, true)
	c.refresh(context.Background())

	for _, name := range []string{
		"tapelibrary_data_cartridge_usage_motion_meters_total",
		"tapelibrary_data_cartridge_usage_mounts_total",
		"tapelibrary_data_cartridge_usage_written_bytes_total",
		"tapelibrary_data_cartridge_usage_errors_total",
	} {
		if n := testutil.CollectAndCount(c, name); n != 0 {
			t.Errorf("%s = %d series, want 0 for an unread cartridge", name, n)
		}
	}

	// The histograms still ship, empty, and the cartridge is accounted for.
	expected := `
# HELP tapelibrary_data_cartridges_usage_unknown Number of data cartridges excluded from the usage histograms, by reason. Always emitted for both reasons. reason="unread" is a cartridge whose memory the library has not read, which returns all seven counters as null at once and is the endpoint's ordinary shape rather than a fault (39% of the reference capture). reason="invalid" is a cartridge that reported a negative counter or a partial record, which is a cartridge-memory fault worth investigating. Each histogram's _count plus both of these is the library's full cartridge parc.
# TYPE tapelibrary_data_cartridges_usage_unknown gauge
tapelibrary_data_cartridges_usage_unknown{reason="invalid"} 0
tapelibrary_data_cartridges_usage_unknown{reason="unread"} 1
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected),
		"tapelibrary_data_cartridges_usage_unknown",
	); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}
}

func TestDataCartridgesLifetimeCollector_DoneClosesOnCancel(t *testing.T) {
	log := logger.NewTextLogger("error")
	c := NewDataCartridgesLifetimeCollector(log, NewClient("http://example.invalid", time.Second), time.Hour, false)
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

// TestDataCartridgesLifetimeCollector_CollectServesCacheWithoutIO proves Collect
// never calls the library: after Start's own immediate refresh completes, a long
// interval (1 hour) guarantees the ticker cannot fire again during this test, so
// any further request the server receives could only come from Collect itself
// calling out, which the design forbids.
func TestDataCartridgesLifetimeCollector_CollectServesCacheWithoutIO(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.ServeFile(w, r, "testdata/data_cartridges_lifetime.json")
	}))
	defer srv.Close()

	log := logger.NewTextLogger("error")
	c := NewDataCartridgesLifetimeCollector(log, NewClient(srv.URL, time.Second), time.Hour, false)
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

// TestDataCartridgesLifetimeCollector_ErrorHandling drives a refresh against a
// library that only ever fails. No cache was ever filled, so the scrape must
// carry exactly the freshness gauge, and must neither panic nor emit a partial
// set of aggregates.
func TestDataCartridgesLifetimeCollector_ErrorHandling(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	log := logger.NewTextLogger("error")
	c := NewDataCartridgesLifetimeCollector(log, NewClient(srv.URL, time.Second), time.Hour, false)
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

// TestDataCartridgesLifetimeCollector_ErrorKeepsPreviousCache scripts the
// backend to succeed once, then fail on every later call. The cache from the
// successful first refresh must survive the later failure (fail-open, per
// refresh's doc comment) rather than being cleared.
func TestDataCartridgesLifetimeCollector_ErrorKeepsPreviousCache(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			http.ServeFile(w, r, "testdata/data_cartridges_lifetime.json")
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	log := logger.NewTextLogger("error")
	c := NewDataCartridgesLifetimeCollector(log, NewClient(srv.URL, time.Second), 20*time.Millisecond, false)
	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		cancel()
		<-c.Done()
	}()

	c.Start(ctx)

	// Wait for backend call 3 to start, rather than sleeping a fixed duration:
	// refresh runs synchronously on Start's single goroutine, so the ticker
	// cannot dispatch call 3 until call 2 (the first failing, 500 refresh) has
	// fully returned, its error branch included.
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
	// A surviving cache emits 10: the 9 cached from the successful first refresh
	// plus the freshness gauge. A wrongly-cleared cache would emit only 1.
	if count != 10 {
		t.Fatalf("GatherAndCount = %d, want 10: the previous cache must survive a later refresh error", count)
	}
}

// TestDataCartridgesLifetimeCollector_StatusTrackerSuccessOnFirstScrape covers
// the startup window before Start's first refresh has completed (Start is
// deliberately never called here). Collect must still emit exactly the freshness
// gauge, valued 0 (not a zero time.Time's large-negative Unix()), and
// StatusTracker must still report this collector as successful: "Collect ran and
// returned data" and "the data is fresh" are different questions.
func TestDataCartridgesLifetimeCollector_StatusTrackerSuccessOnFirstScrape(t *testing.T) {
	log := logger.NewTextLogger("error")
	c := NewDataCartridgesLifetimeCollector(log, NewClient("http://example.invalid", time.Second), time.Hour, false)
	// Do NOT call Start(): cache is empty, no refresh has ever run.

	expected := `
# HELP tapelibrary_data_cartridges_usage_last_refresh_timestamp_seconds Unix time of the last successful data cartridges lifetime metrics refresh. Alert if time() - this > 2 x the collector's configured interval.
# TYPE tapelibrary_data_cartridges_usage_last_refresh_timestamp_seconds gauge
tapelibrary_data_cartridges_usage_last_refresh_timestamp_seconds 0
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected)); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}

	tracker := NewStatusTracker(log)
	tracker.Add("data_cartridges_lifetime", c)

	expectedSuccess := `
# HELP ` + statusTrackerSuccessMetric + ` Whether the last scrape of the collector succeeded (1=success, 0=failure)
# TYPE ` + statusTrackerSuccessMetric + ` gauge
` + statusTrackerSuccessMetric + `{collector="data_cartridges_lifetime"} 1
`
	if err := testutil.CollectAndCompare(tracker, strings.NewReader(expectedSuccess), statusTrackerSuccessMetric); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}
}

// TestDataCartridgesLifetimeCollector_StatusTrackerFailure pins the other half
// of the tracker contract: a collector whose refresh has never succeeded still
// emits its freshness gauge, so it reports as alive. That is deliberate. The
// freshness gauge's VALUE, not the success gauge, is what says the data is
// stale.
func TestDataCartridgesLifetimeCollector_StatusTrackerFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	log := logger.NewTextLogger("error")
	c := NewDataCartridgesLifetimeCollector(log, NewClient(srv.URL, time.Second), time.Hour, false)
	c.refresh(context.Background())

	tracker := NewStatusTracker(log)
	tracker.Add("data_cartridges_lifetime", c)

	expectedSuccess := `
# HELP ` + statusTrackerSuccessMetric + ` Whether the last scrape of the collector succeeded (1=success, 0=failure)
# TYPE ` + statusTrackerSuccessMetric + ` gauge
` + statusTrackerSuccessMetric + `{collector="data_cartridges_lifetime"} 1
`
	if err := testutil.CollectAndCompare(tracker, strings.NewReader(expectedSuccess), statusTrackerSuccessMetric); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}
}
