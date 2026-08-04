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
	dto "github.com/prometheus/client_model/go"

	"github.com/sckyzo/tapelibrary_exporter/internal/logger"
)

// diagnosticCartridgesFixtureServer serves testdata/diagnostic_cartridges.json
// on every request and returns a collector already pointed at it, with the
// per-volser detail on (the shipped default). Nothing is started: the caller
// decides whether to drive refresh directly (deterministic) or via Start.
func diagnosticCartridgesFixtureServer(t *testing.T) (*httptest.Server, *DiagnosticCartridgesCollector) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "testdata/diagnostic_cartridges.json")
	}))
	t.Cleanup(srv.Close)

	log := logger.NewTextLogger("error")
	return srv, NewDiagnosticCartridgesCollector(log, NewClient(srv.URL, time.Second), 5*time.Minute, true)
}

// TestParseDiagnosticCartridges exercises parseDiagnosticCartridges (piece 2,
// the pure parser) with a static byte fixture: no HTTP, no collector, no
// logger, no goroutine involved.
func TestParseDiagnosticCartridges(t *testing.T) {
	data, err := os.ReadFile("testdata/diagnostic_cartridges.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	cartridges, err := parseDiagnosticCartridges(data)
	if err != nil {
		t.Fatalf("parseDiagnosticCartridges: %v", err)
	}
	if got, want := len(cartridges), 5; got != want {
		t.Fatalf("len(cartridges) = %d, want %d", got, want)
	}

	// Sorted by volser then location, which is what the parser promises so
	// the cached slice is deterministic for a given response.
	if got, want := cartridges[0].Volser, "TST011JL"; got != want {
		t.Errorf("cartridges[0].Volser = %q, want %q: the result must be sorted", got, want)
	}
	if got, want := cartridges[4].Volser, "TST082JK"; got != want {
		t.Errorf("cartridges[4].Volser = %q, want %q: the result must be sorted", got, want)
	}

	byVolser := make(map[string]diagnosticCartridge, len(cartridges))
	for _, d := range cartridges {
		byVolser[d.Volser] = d
	}

	// The cartridge whose memory the library HAS read.
	full := byVolser["TST080JL"]
	if full.Type == nil || *full.Type != "JL" {
		t.Errorf("TST080JL: Type = %v, want JL", full.Type)
	}
	if full.Worm == nil || *full.Worm != "false" {
		t.Errorf("TST080JL: Worm = %v, want \"false\" (a JSON string, not a bool)", full.Worm)
	}
	if full.LifetimeRemaining == nil || *full.LifetimeRemaining != 96 {
		t.Errorf("TST080JL: LifetimeRemaining = %v, want 96", full.LifetimeRemaining)
	}

	// Three of the five report null for the whole cartridge-memory block,
	// which on this endpoint is the ordinary case rather than an edge case.
	// They must decode to nil, never to a zero value: a 0 lifetime would read
	// as a cartridge at end of life.
	bare := byVolser["TST079JK"]
	if bare.Type != nil {
		t.Errorf("TST079JK: Type = %v, want nil", *bare.Type)
	}
	if bare.LifetimeRemaining != nil {
		t.Errorf("TST079JK: LifetimeRemaining = %v, want nil, never 0", *bare.LifetimeRemaining)
	}
	// The fields the library reports regardless survive that.
	if bare.State != "normal" || bare.Accessible != "normal" || bare.MediaType != "3592" {
		t.Errorf("TST079JK: state/accessible/mediaType = %q/%q/%q, want normal/normal/3592", bare.State, bare.Accessible, bare.MediaType)
	}

	// One cartridge sits in a drive rather than a slot. location carries the
	// library's own native string verbatim either way.
	if got, want := byVolser["TST011JL"].Location, "drive_F11C2R3"; got != want {
		t.Errorf("TST011JL: Location = %q, want %q", got, want)
	}

	t.Run("an empty array is a real reading of zero, not an error", func(t *testing.T) {
		// The one place this collector parts company with its cartridge
		// siblings: a library with no diagnostic cartridge is a library
		// nobody has loaded one into, and zero is exactly what
		// DiagnosticCartridgesExhausted has to be able to see.
		got, err := parseDiagnosticCartridges([]byte(`[]`))
		if err != nil {
			t.Fatalf("parseDiagnosticCartridges([]) = %v, want nil: an empty list is a valid reading here", err)
		}
		if len(got) != 0 {
			t.Errorf("len = %d, want 0", len(got))
		}
	})

	t.Run("a duplicate volser at a different location is accepted", func(t *testing.T) {
		// volser alone is NOT a key on any cartridge endpoint of this
		// library: R1.11.2 says so and the cleaning capture proves it.
		got, err := parseDiagnosticCartridges([]byte(`[
			{"volser":"TST079JK","state":"normal","accessible":"normal","location":"slot_F1C1R1T0"},
			{"volser":"TST079JK","state":"normal","accessible":"normal","location":"slot_F2C1R1T0"}
		]`))
		if err != nil {
			t.Fatalf("parseDiagnosticCartridges: %v", err)
		}
		if len(got) != 2 {
			t.Errorf("len = %d, want 2: the same barcode at two locations is two cartridges", len(got))
		}
	})

	t.Run("a duplicate of the volser+location PAIR is rejected", func(t *testing.T) {
		// Two metrics sharing a descriptor and a label set fail
		// Registry.Gather for the WHOLE scrape, so this must never reach
		// Collect.
		_, err := parseDiagnosticCartridges([]byte(`[
			{"volser":"TST079JK","state":"normal","accessible":"normal","location":"slot_F1C1R1T0"},
			{"volser":"TST079JK","state":"normal","accessible":"normal","location":"slot_F1C1R1T0"}
		]`))
		if err == nil {
			t.Error("a duplicate volser+location pair returned a nil error, want non-nil")
		}
	})

	t.Run("an entry with no volser is an error", func(t *testing.T) {
		if _, err := parseDiagnosticCartridges([]byte(`[{"volser":"","location":"slot_F1C1R1T0"}]`)); err == nil {
			t.Error("an entry with no volser returned a nil error, want non-nil")
		}
	})

	t.Run("an entry with no location is an error", func(t *testing.T) {
		if _, err := parseDiagnosticCartridges([]byte(`[{"volser":"TST079JK","location":""}]`)); err == nil {
			t.Error("an entry with no location returned a nil error, want non-nil")
		}
	})

	t.Run("malformed input yields an error, not a panic", func(t *testing.T) {
		if _, err := parseDiagnosticCartridges([]byte("not json")); err == nil {
			t.Error("parseDiagnosticCartridges(malformed) returned a nil error, want non-nil")
		}
	})

	t.Run("an object rather than the documented array is an error", func(t *testing.T) {
		if _, err := parseDiagnosticCartridges([]byte(`{"volser":"TST079JK"}`)); err == nil {
			t.Error("parseDiagnosticCartridges(object) returned a nil error, want non-nil")
		}
	})

	t.Run("empty input yields an error, not a panic", func(t *testing.T) {
		if _, err := parseDiagnosticCartridges(nil); err == nil {
			t.Error("parseDiagnosticCartridges(nil) returned a nil error, want non-nil")
		}
	})
}

// TestDiagnosticCartridgesCollector_Describe locks the descriptor count at
// exactly 8 (four library-wide, three per-cartridge, and the freshness gauge)
// so a future edit that silently adds or drops a metric is caught here rather
// than downstream in docs-check or a dashboard. The count does NOT vary with
// the per-volser flag: Describe is constant by contract.
func TestDiagnosticCartridgesCollector_Describe(t *testing.T) {
	log := logger.NewTextLogger("error")
	for name, perVolser := range map[string]bool{"per-volser on": true, "per-volser off": false} {
		t.Run(name, func(t *testing.T) {
			c := NewDiagnosticCartridgesCollector(log, NewClient("http://example.invalid", time.Second), 5*time.Minute, perVolser)
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
		})
	}
}

// TestDiagnosticCartridgesCollector_Collect pins the exact exposition text
// against the fixture. refresh is driven directly rather than through Start,
// so the assertion is deterministic and carries no sleep.
//
// Registry.Gather sorts metric families by name and, within a family, by
// label value: that ordering is deterministic, not incidental.
func TestDiagnosticCartridgesCollector_Collect(t *testing.T) {
	_, c := diagnosticCartridgesFixtureServer(t)
	c.refresh(context.Background())

	// The library-wide families, which every alert reads and which are
	// emitted regardless of the per-volser flag. All five cartridges are
	// normal and reachable in the reference capture, so usable is 5; three
	// report no cartridge memory, so lifetime_unknown is 3.
	expected := `
# HELP tapelibrary_diagnostic_cartridges Number of diagnostic cartridges in each state, as a stateset over R1.11.2's five documented values plus any value actually observed. Always emitted, independently of --collector.diagnostic_cartridges.per-volser. Summing across state gives the library's whole diagnostic population.
# TYPE tapelibrary_diagnostic_cartridges gauge
tapelibrary_diagnostic_cartridges{state="atEndOfLife"} 0
tapelibrary_diagnostic_cartridges{state="exportQueued"} 0
tapelibrary_diagnostic_cartridges{state="importing"} 0
tapelibrary_diagnostic_cartridges{state="normal"} 5
tapelibrary_diagnostic_cartridges{state="unknown"} 0
# HELP tapelibrary_diagnostic_cartridges_access Number of diagnostic cartridges the accessor can reach, by access level. A cartridge behind a blocking position reads limited or no while its state stays normal, so this is a different question from the state count above and both are needed to explain a low usable figure.
# TYPE tapelibrary_diagnostic_cartridges_access gauge
tapelibrary_diagnostic_cartridges_access{access="limited"} 0
tapelibrary_diagnostic_cartridges_access{access="no"} 0
tapelibrary_diagnostic_cartridges_access{access="normal"} 5
# HELP tapelibrary_diagnostic_cartridges_lifetime_unknown Number of diagnostic cartridges reporting no remaining-life reading at all, because the library has not read their cartridge memory. Three of the five in the reference capture, so a high value here is the ordinary state of this endpoint rather than a fault: it is published so that the remaining-life series below are read as covering a subset, never as covering the whole population.
# TYPE tapelibrary_diagnostic_cartridges_lifetime_unknown gauge
tapelibrary_diagnostic_cartridges_lifetime_unknown 3
# HELP tapelibrary_diagnostic_cartridges_usable Number of diagnostic cartridges the library could actually select for a service action right now: state normal AND reachable by the accessor. Deliberately narrower than the normal state count, because a cartridge the robot cannot reach is one it cannot use. Reaching 0 means the next service action requiring media will be blocked, and is what DiagnosticCartridgesExhausted reads. Always emitted, independently of the per-volser flag.
# TYPE tapelibrary_diagnostic_cartridges_usable gauge
tapelibrary_diagnostic_cartridges_usable 5
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected),
		"tapelibrary_diagnostic_cartridges",
		"tapelibrary_diagnostic_cartridges_access",
		"tapelibrary_diagnostic_cartridges_lifetime_unknown",
		"tapelibrary_diagnostic_cartridges_usable",
	); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}

	// The per-cartridge families. The three cartridges whose memory was never
	// read carry cartridge_type="unknown" and worm="unknown" rather than an
	// empty label, and emit no lifetime series at all.
	expectedPerVolser := `
# HELP tapelibrary_diagnostic_cartridge_lifetime_remaining_ratio Estimated media life left on this diagnostic cartridge, as a ratio from 0 to 1 (the API reports a 0-100 percentage). Absent, never zero, for a cartridge whose memory the library has not read, which is the majority case on this endpoint: a 0 would read as a cartridge at end of life. Count the absent ones with tapelibrary_diagnostic_cartridges_lifetime_unknown. Emitted only when --collector.diagnostic_cartridges.per-volser is set.
# TYPE tapelibrary_diagnostic_cartridge_lifetime_remaining_ratio gauge
tapelibrary_diagnostic_cartridge_lifetime_remaining_ratio{location="drive_F11C2R3",volser="TST011JL"} 0.95
tapelibrary_diagnostic_cartridge_lifetime_remaining_ratio{location="slot_F11C8R19T0",volser="TST080JL"} 0.96
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expectedPerVolser),
		"tapelibrary_diagnostic_cartridge_lifetime_remaining_ratio",
	); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}

	// 5 state + 3 access + usable + lifetime_unknown + 5 info + 5 last_usage
	// + 2 lifetime_ratio + freshness.
	reg := prometheus.NewRegistry()
	if err := reg.Register(c); err != nil {
		t.Fatalf("Register: %v", err)
	}
	count, err := testutil.GatherAndCount(reg)
	if err != nil {
		t.Fatalf("GatherAndCount: %v", err)
	}
	if count != 23 {
		t.Fatalf("GatherAndCount = %d, want 23", count)
	}
}

// TestDiagnosticCartridgesCollector_UnreadMemoryLabelsAsUnknown pins the
// journal's rule that a nullable LABEL field takes the literal token
// "unknown" while a nullable MEASUREMENT emits nothing. Three of the five
// fixture cartridges take that branch, so this is the endpoint's normal path.
func TestDiagnosticCartridgesCollector_UnreadMemoryLabelsAsUnknown(t *testing.T) {
	_, c := diagnosticCartridgesFixtureServer(t)
	c.refresh(context.Background())

	got := testutil.CollectAndCount(c, "tapelibrary_diagnostic_cartridge_info")
	if got != 5 {
		t.Fatalf("info series = %d, want 5: one per cartridge regardless of what its memory reports", got)
	}

	// An empty label is what cleaning_cartridges already refuses for state,
	// and it is unreadable in a dashboard; the token is what keeps the counts
	// complete instead.
	expected := `
# HELP tapelibrary_diagnostic_cartridge_info Always 1. Carries this diagnostic cartridge's current state and identity as labels, joinable to the per-cartridge measurements on volser and location. Labels the library did not read carry the literal token unknown rather than an empty string. Emitted only when --collector.diagnostic_cartridges.per-volser is set.
# TYPE tapelibrary_diagnostic_cartridge_info gauge
tapelibrary_diagnostic_cartridge_info{access="normal",cartridge_type="JL",location="drive_F11C2R3",media_type="3592",state="normal",volser="TST011JL",worm="false"} 1
tapelibrary_diagnostic_cartridge_info{access="normal",cartridge_type="unknown",location="slot_F12C6R1T0",media_type="3592",state="normal",volser="TST079JK",worm="unknown"} 1
tapelibrary_diagnostic_cartridge_info{access="normal",cartridge_type="JL",location="slot_F11C8R19T0",media_type="3592",state="normal",volser="TST080JL",worm="false"} 1
tapelibrary_diagnostic_cartridge_info{access="normal",cartridge_type="unknown",location="slot_F11C10R19T0",media_type="3592",state="normal",volser="TST081JK",worm="unknown"} 1
tapelibrary_diagnostic_cartridge_info{access="normal",cartridge_type="unknown",location="slot_F1C4R1T0",media_type="3592",state="normal",volser="TST082JK",worm="unknown"} 1
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected), "tapelibrary_diagnostic_cartridge_info"); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}

	// The measurement side of the same rule: no zero stands in for a missing
	// reading.
	if got := testutil.CollectAndCount(c, "tapelibrary_diagnostic_cartridge_lifetime_remaining_ratio"); got != 2 {
		t.Errorf("lifetime ratio series = %d, want 2: a cartridge with no memory reading emits none, never a 0", got)
	}
}

// TestDiagnosticCartridgesCollector_UsableIsNarrowerThanNormal pins the one
// piece of judgement in this collector: a cartridge the accessor cannot reach
// is one the library cannot select, whatever its state says. Getting this
// wrong would make DiagnosticCartridgesExhausted silent in exactly the case
// it exists for.
func TestDiagnosticCartridgesCollector_UsableIsNarrowerThanNormal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[
			{"volser":"TST001JK","state":"normal","accessible":"normal","location":"slot_F1C1R1T0","mediaType":"3592"},
			{"volser":"TST002JK","state":"normal","accessible":"no","location":"slot_F1C2R1T0","mediaType":"3592"},
			{"volser":"TST003JK","state":"atEndOfLife","accessible":"normal","location":"slot_F1C3R1T0","mediaType":"3592"},
			{"volser":"TST004JK","state":"exportQueued","accessible":"normal","location":"slot_F1C4R1T0","mediaType":"3592"}
		]`))
	}))
	defer srv.Close()

	log := logger.NewTextLogger("error")
	c := NewDiagnosticCartridgesCollector(log, NewClient(srv.URL, time.Second), 5*time.Minute, true)
	c.refresh(context.Background())

	// Four cartridges, three of them "normal" by state, but only one the
	// robot could actually pick up.
	expected := `
# HELP tapelibrary_diagnostic_cartridges_usable Number of diagnostic cartridges the library could actually select for a service action right now: state normal AND reachable by the accessor. Deliberately narrower than the normal state count, because a cartridge the robot cannot reach is one it cannot use. Reaching 0 means the next service action requiring media will be blocked, and is what DiagnosticCartridgesExhausted reads. Always emitted, independently of the per-volser flag.
# TYPE tapelibrary_diagnostic_cartridges_usable gauge
tapelibrary_diagnostic_cartridges_usable 1
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected), "tapelibrary_diagnostic_cartridges_usable"); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}
}

// TestDiagnosticCartridgesCollector_UndocumentedStateIsStillEmitted covers
// the emit-observed-anyway branch. R1.11.2's state tables have been
// incomplete more than once in this exporter, and a state nobody tabulated
// must surface as a new series rather than leave every documented one at 0.
func TestDiagnosticCartridgesCollector_UndocumentedStateIsStillEmitted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[
			{"volser":"TST001JK","state":"someFutureState","accessible":"normal","location":"slot_F1C1R1T0","mediaType":"3592"}
		]`))
	}))
	defer srv.Close()

	log := logger.NewTextLogger("error")
	c := NewDiagnosticCartridgesCollector(log, NewClient(srv.URL, time.Second), 5*time.Minute, true)
	c.refresh(context.Background())

	// The five documented values plus the observed one.
	if got := testutil.CollectAndCount(c, "tapelibrary_diagnostic_cartridges"); got != 6 {
		t.Fatalf("state series = %d, want 6 (5 documented + 1 observed)", got)
	}
	// And it must not count as usable: only the documented `normal` does.
	expected := `
# HELP tapelibrary_diagnostic_cartridges_usable Number of diagnostic cartridges the library could actually select for a service action right now: state normal AND reachable by the accessor. Deliberately narrower than the normal state count, because a cartridge the robot cannot reach is one it cannot use. Reaching 0 means the next service action requiring media will be blocked, and is what DiagnosticCartridgesExhausted reads. Always emitted, independently of the per-volser flag.
# TYPE tapelibrary_diagnostic_cartridges_usable gauge
tapelibrary_diagnostic_cartridges_usable 0
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected), "tapelibrary_diagnostic_cartridges_usable"); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}
}

// TestDiagnosticCartridgesCollector_EmptyLibraryStillEmitsTheAggregates is
// the collector-authoring rule applied to the case this endpoint can
// genuinely reach: a library nobody has loaded a diagnostic cartridge into
// must still publish a complete, all-zero stateset, so every query written
// against it keeps working and usable reads a true 0.
func TestDiagnosticCartridgesCollector_EmptyLibraryStillEmitsTheAggregates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	log := logger.NewTextLogger("error")
	c := NewDiagnosticCartridgesCollector(log, NewClient(srv.URL, time.Second), 5*time.Minute, true)
	c.refresh(context.Background())

	reg := prometheus.NewRegistry()
	if err := reg.Register(c); err != nil {
		t.Fatalf("Register: %v", err)
	}
	count, err := testutil.GatherAndCount(reg)
	if err != nil {
		t.Fatalf("GatherAndCount: %v", err)
	}
	// 5 state + 3 access + usable + lifetime_unknown + freshness. No
	// per-cartridge series, because there are no cartridges.
	if count != 11 {
		t.Fatalf("GatherAndCount = %d, want 11 on an empty library", count)
	}
	if got := testutil.ToFloat64(usableOf(t, c)); got != 0 {
		t.Errorf("usable = %v, want 0", got)
	}
}

// TestDiagnosticCartridgesCollector_PerVolserOffDropsOnlyTheDetail pins the
// flag's contract: turning it off costs the ability to name WHICH cartridge
// to pull, and never the library-wide aggregates the alerts read.
func TestDiagnosticCartridgesCollector_PerVolserOffDropsOnlyTheDetail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "testdata/diagnostic_cartridges.json")
	}))
	defer srv.Close()

	log := logger.NewTextLogger("error")
	c := NewDiagnosticCartridgesCollector(log, NewClient(srv.URL, time.Second), 5*time.Minute, false)
	c.refresh(context.Background())

	for _, name := range []string{
		"tapelibrary_diagnostic_cartridge_info",
		"tapelibrary_diagnostic_cartridge_last_usage_timestamp_seconds",
		"tapelibrary_diagnostic_cartridge_lifetime_remaining_ratio",
	} {
		if got := testutil.CollectAndCount(c, name); got != 0 {
			t.Errorf("%s produced %d series with per-volser off, want 0", name, got)
		}
	}

	// The aggregates are untouched: 5 state + 3 access + usable +
	// lifetime_unknown + freshness.
	reg := prometheus.NewRegistry()
	if err := reg.Register(c); err != nil {
		t.Fatalf("Register: %v", err)
	}
	count, err := testutil.GatherAndCount(reg)
	if err != nil {
		t.Fatalf("GatherAndCount: %v", err)
	}
	if count != 11 {
		t.Fatalf("GatherAndCount = %d, want 11: the alerts' aggregates must survive the flag", count)
	}
	if got := testutil.ToFloat64(usableOf(t, c)); got != 5 {
		t.Errorf("usable = %v, want 5: the flag must not change what the alerts read", got)
	}
}

// TestDiagnosticCartridgesCollector_DoneClosesOnCancel verifies the Done()
// channel closes when the context passed to Start is cancelled. This is the
// mechanism main.go's shutdown seam relies on.
func TestDiagnosticCartridgesCollector_DoneClosesOnCancel(t *testing.T) {
	log := logger.NewTextLogger("error")
	c := NewDiagnosticCartridgesCollector(log, NewClient("http://example.invalid", time.Second), 5*time.Minute, true)
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

// TestDiagnosticCartridgesCollector_CollectServesCacheWithoutIO proves
// Collect never calls the library: after Start's own immediate refresh, a
// long interval guarantees the ticker cannot fire again during this test, so
// any further request could only come from Collect calling out.
func TestDiagnosticCartridgesCollector_CollectServesCacheWithoutIO(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.ServeFile(w, r, "testdata/diagnostic_cartridges.json")
	}))
	defer srv.Close()

	log := logger.NewTextLogger("error")
	c := NewDiagnosticCartridgesCollector(log, NewClient(srv.URL, time.Second), time.Hour, true)
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

// TestDiagnosticCartridgesCollector_ErrorHandling drives a refresh against a
// library that only ever fails. No cache was ever filled, so the scrape must
// carry exactly the freshness gauge.
func TestDiagnosticCartridgesCollector_ErrorHandling(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	log := logger.NewTextLogger("error")
	c := NewDiagnosticCartridgesCollector(log, NewClient(srv.URL, time.Second), 5*time.Minute, true)
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

// TestDiagnosticCartridgesCollector_ErrorKeepsPreviousCache scripts the
// backend to succeed once then fail, and drives a further refresh via a short
// interval. The cache from the successful first refresh must survive.
func TestDiagnosticCartridgesCollector_ErrorKeepsPreviousCache(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			http.ServeFile(w, r, "testdata/diagnostic_cartridges.json")
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	log := logger.NewTextLogger("error")
	c := NewDiagnosticCartridgesCollector(log, NewClient(srv.URL, time.Second), 20*time.Millisecond, true)
	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		cancel()
		<-c.Done()
	}()

	c.Start(ctx)

	// Waiting for call 3 rather than sleeping a fixed duration: refresh runs
	// synchronously on Start's single goroutine, so the ticker cannot
	// dispatch call 3 until call 2's whole error path has returned.
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
	if count != 23 {
		t.Fatalf("GatherAndCount = %d, want 23: the previous cache must survive a later refresh error", count)
	}
}

// TestDiagnosticCartridgesCollector_StatusTrackerSuccessOnFirstScrape covers
// the startup window before Start's first refresh has completed. Collect must
// still emit exactly the freshness gauge, valued 0, and StatusTracker must
// still report this collector as successful: "Collect ran and returned data"
// and "the data is fresh" are different questions, which is precisely why
// CollectorNeverRefreshed reads the gauge rather than the success metric.
func TestDiagnosticCartridgesCollector_StatusTrackerSuccessOnFirstScrape(t *testing.T) {
	log := logger.NewTextLogger("error")
	c := NewDiagnosticCartridgesCollector(log, NewClient("http://example.invalid", time.Second), 5*time.Minute, true)
	// Do NOT call Start(): cache is empty, no refresh has ever run.

	expected := `
# HELP tapelibrary_diagnostic_cartridges_last_refresh_timestamp_seconds Unix time of the last successful diagnostic cartridges refresh. Alert if time() - this > 2 x the collector's configured interval; CollectorNeverRefreshed and CollectorRefreshStale already do.
# TYPE tapelibrary_diagnostic_cartridges_last_refresh_timestamp_seconds gauge
tapelibrary_diagnostic_cartridges_last_refresh_timestamp_seconds 0
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected)); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}

	tracker := NewStatusTracker(log)
	tracker.Add("diagnostic_cartridges", c)

	expectedSuccess := `
# HELP ` + statusTrackerSuccessMetric + ` Whether the last scrape of the collector succeeded (1=success, 0=failure)
# TYPE ` + statusTrackerSuccessMetric + ` gauge
` + statusTrackerSuccessMetric + `{collector="diagnostic_cartridges"} 1
`
	if err := testutil.CollectAndCompare(tracker, strings.NewReader(expectedSuccess), statusTrackerSuccessMetric); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}
}

// usableOf exposes the usable gauge as a single-metric collector so
// testutil.ToFloat64 can read its value: that helper refuses a collector
// emitting more than one metric, and this one emits a whole family.
func usableOf(t *testing.T, c *DiagnosticCartridgesCollector) prometheus.Collector {
	t.Helper()
	g := prometheus.NewGauge(prometheus.GaugeOpts{Name: "usable_probe"})
	ch := make(chan prometheus.Metric, 64)
	c.Collect(ch)
	close(ch)
	for m := range ch {
		if !strings.Contains(m.Desc().String(), "tapelibrary_diagnostic_cartridges_usable") {
			continue
		}
		var pb dto.Metric
		if err := m.Write(&pb); err != nil {
			t.Fatalf("write metric: %v", err)
		}
		g.Set(pb.GetGauge().GetValue())
		return g
	}
	t.Fatal("usable gauge not found in Collect output")
	return nil
}
