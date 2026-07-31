package collector

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// TestNilLimiterIsPassThrough pins the default: no ceiling configured,
// NewLimiter returns nil for any non-positive limit, and a nil *Limiter must
// cost nothing and never block.
func TestNilLimiterIsPassThrough(t *testing.T) {
	var l *Limiter
	if NewLimiter(0) != nil {
		t.Fatal("NewLimiter(0) returned a limiter; 0 must mean unlimited")
	}
	if NewLimiter(-1) != nil {
		t.Fatal("NewLimiter(-1) returned a limiter; a non-positive ceiling must mean unlimited")
	}
	release, err := l.Acquire(context.Background())
	if err != nil {
		t.Fatalf("nil limiter refused an acquisition: %v", err)
	}
	release()
}

// TestLimiterEnforcesItsCeiling proves the third caller waits while two hold
// slots, and proceeds as soon as one is released.
func TestLimiterEnforcesItsCeiling(t *testing.T) {
	l := NewLimiter(2)

	r1, err := l.Acquire(context.Background())
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	r2, err := l.Acquire(context.Background())
	if err != nil {
		t.Fatalf("second acquire: %v", err)
	}

	third := make(chan struct{})
	go func() {
		r3, err := l.Acquire(context.Background())
		if err == nil {
			r3()
		}
		close(third)
	}()

	select {
	case <-third:
		t.Fatal("the third acquire proceeded while both slots were held")
	case <-time.After(50 * time.Millisecond):
	}

	r1()
	select {
	case <-third:
	case <-time.After(2 * time.Second):
		t.Fatal("the third acquire did not proceed after a slot was released")
	}
	r2()
}

// TestLimiterAcquireHonoursContext is the property that keeps a ceiling from
// degrading silently: a starved caller fails on its own deadline instead of
// queueing forever while its ticker drops ticks.
func TestLimiterAcquireHonoursContext(t *testing.T) {
	l := NewLimiter(1)
	release, err := l.Acquire(context.Background())
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := l.Acquire(ctx); err == nil {
		t.Fatal("Acquire returned success on an expired context while the only slot was held")
	}
}

// TestLimiterReleaseIsSafeUnderConcurrency runs the ceiling under -race with
// more callers than slots, and asserts the ceiling was never exceeded.
func TestLimiterReleaseIsSafeUnderConcurrency(t *testing.T) {
	const ceiling = 3
	l := NewLimiter(ceiling)

	var mu sync.Mutex
	inFlight, peak := 0, 0

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, err := l.Acquire(context.Background())
			if err != nil {
				t.Errorf("acquire: %v", err)
				return
			}
			mu.Lock()
			inFlight++
			if inFlight > peak {
				peak = inFlight
			}
			mu.Unlock()

			time.Sleep(time.Millisecond)

			mu.Lock()
			inFlight--
			mu.Unlock()
			release()
		}()
	}
	wg.Wait()

	if peak > ceiling {
		t.Fatalf("peak concurrency was %d, ceiling is %d", peak, ceiling)
	}
	if peak == 0 {
		t.Fatal("no goroutine was ever observed in flight; the test proved nothing")
	}
}

// requestWaitSampleCount reads RequestWait's current sample count for one
// outcome label value, via a real Gather so it reflects exactly what a
// scrape would see. Deliberately avoids importing client_model directly to
// do this: only values Gather itself returns are inspected, their concrete
// type never named here, the same way instance_test.go.tmpl's seriesFor
// stays import-free elsewhere in this scaffold.
func requestWaitSampleCount(t *testing.T, outcome string) uint64 {
	t.Helper()
	reg := prometheus.NewRegistry()
	if err := reg.Register(RequestWait); err != nil {
		t.Fatalf("register RequestWait: %v", err)
	}
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != "tapelibrary_exporter_request_wait_seconds" {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "outcome" && l.GetValue() == outcome {
					return m.GetHistogram().GetSampleCount()
				}
			}
		}
	}
	t.Fatalf(`no outcome=%q series found for RequestWait; init() must pre-populate both outcome values`, outcome)
	return 0
}

// TestAcquireLabelsRequestWaitByOutcome proves the outcome label actually
// discriminates between Acquire's two paths, not just that RequestWait gets
// observed at all: a granted acquire must bump "success" and leave "error"
// untouched, and a ctx-expired acquire must do the opposite. Guards against
// the Help text being truthful ("...by outcome.") while the series itself
// still conflated both outcomes into one bucket.
func TestAcquireLabelsRequestWaitByOutcome(t *testing.T) {
	beforeSuccess := requestWaitSampleCount(t, "success")
	beforeError := requestWaitSampleCount(t, "error")

	l := NewLimiter(1)
	release, err := l.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire a free slot: %v", err)
	}
	defer release()

	if got, want := requestWaitSampleCount(t, "success"), beforeSuccess+1; got != want {
		t.Fatalf(`RequestWait outcome="success" sample count = %d, want %d after a granted acquire`, got, want)
	}
	if got := requestWaitSampleCount(t, "error"); got != beforeError {
		t.Fatalf(`RequestWait outcome="error" sample count changed to %d (want unchanged at %d) on a granted acquire`, got, beforeError)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := l.Acquire(ctx); err == nil {
		t.Fatal("Acquire on an expired context, with the only slot already held, returned no error")
	}

	if got, want := requestWaitSampleCount(t, "error"), beforeError+1; got != want {
		t.Fatalf(`RequestWait outcome="error" sample count = %d, want %d after a ctx-expired acquire`, got, want)
	}
	if got, want := requestWaitSampleCount(t, "success"), beforeSuccess+1; got != want {
		t.Fatalf(`RequestWait outcome="success" sample count changed to %d (want unchanged at %d) on a ctx-expired acquire`, got, want)
	}
}

// TestLimiterSetGroupsByTarget proves two collectors on the same address share
// one ceiling and two collectors on different addresses do not.
func TestLimiterSetGroupsByTarget(t *testing.T) {
	s := NewLimiterSet(4)
	// Two separate calls, each stored before comparing: calling For twice
	// inline on the same line (s.For(x) != s.For(x)) reads to staticcheck's
	// SA4000 as comparing an expression to itself, a false positive here
	// since For is stateful, but named variables sidestep the false alarm
	// the same way TestNewClientForSharesOneTransport's a.tr.Get() !=
	// b.tr.Get() already does elsewhere in this package.
	first := s.For("http://a.example")
	second := s.For("http://a.example")
	if first != second {
		t.Fatal("LimiterSet handed out two limiters for one target")
	}
	if s.For("http://a.example") == s.For("http://b.example") {
		t.Fatal("LimiterSet shared one limiter across two targets")
	}
	if NewLimiterSet(0).For("http://a.example") != nil {
		t.Fatal("a LimiterSet built with ceiling 0 handed out a non-nil limiter")
	}
}
