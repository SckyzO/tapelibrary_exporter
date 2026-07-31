package reload

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/sckyzo/tapelibrary_exporter/internal/config"
	"github.com/sckyzo/tapelibrary_exporter/internal/logger"
)

func writeConfig(t *testing.T, dir, body string) string {
	t.Helper()
	p := filepath.Join(dir, "config.yml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return p
}

// startRun starts r.Run on its own goroutine and returns its ctx alongside a
// stop function that cancels it and blocks until Run has actually returned.
// Every test in this file that starts Run must stop it this way, never with
// a bare `defer cancel()`: Run's own cleanup unconditionally calls
// signal.Ignore(syscall.SIGHUP) on exit (see Run's own doc comment for why),
// a process-wide side effect that undoes every OTHER goroutine's SIGHUP
// registrations too, not only this Reloader's own. A `defer cancel()` with
// no wait leaves that goroutine free to keep running, and to call Ignore,
// at any point after its own test function has already returned; under
// `go test -count=N` that landmine can fire while a LATER test (in
// particular TestSIGHUPTriggersAReloadWithoutTerminatingTheProcess) is in
// the middle of its own signal.Notify setup, wiping it out. Reproduced
// empirically while hardening that test: TestReloadIsSerialized and
// TestConcurrentReloadsMixBothPathsAndStaySerialized both start Run and
// both run in the same file, so both needed this too, not just the one
// test that sends real signals.
func startRun(r *Reloader) (ctx context.Context, stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.Run(ctx)
	}()
	return ctx, func() {
		cancel()
		<-done
	}
}

// TestReloadAppliesAValidFile is the happy path, and the one assertion that
// proves a reload CHANGED something rather than merely answering.
func TestReloadAppliesAValidFile(t *testing.T) {
	dir := t.TempDir()
	p := writeConfig(t, dir, "modules:\n  prod: {}\n")
	boot, err := config.Load(p)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	var applied *config.Config
	r := New(logger.NewTextLogger("error"), p, boot, func(c *config.Config) error {
		applied = c
		return nil
	})

	writeConfig(t, dir, "modules:\n  prod: {}\n  staging: {}\n")
	if err := r.Reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if applied == nil {
		t.Fatal("apply was never called")
	}
	if _, ok := applied.Modules["staging"]; !ok {
		t.Fatal("apply received a config without the module the new file added")
	}
	if v := testGaugeValue(t, r.successful); v != 1 {
		t.Fatalf("config_last_reload_successful is %v after a successful reload, want 1", v)
	}
	if testGaugeValue(t, r.successTime) == 0 {
		t.Fatal("the success timestamp was never advanced")
	}
}

// TestReloadKeepsTheOldConfigWhenTheNewOneIsInvalid is the assertion that
// proves atomicity: a broken file must not be applied, and must not disturb
// what is running.
func TestReloadKeepsTheOldConfigWhenTheNewOneIsInvalid(t *testing.T) {
	dir := t.TempDir()
	p := writeConfig(t, dir, "modules:\n  prod: {}\n")
	boot, _ := config.Load(p)

	calls := 0
	r := New(logger.NewTextLogger("error"), p, boot, func(*config.Config) error {
		calls++
		return nil
	})

	writeConfig(t, dir, "modules:\n  prod: {\n") // not YAML
	if err := r.Reload(); err == nil {
		t.Fatal("reload accepted a file that does not parse")
	}
	if calls != 0 {
		t.Fatalf("apply ran %d times on an unparseable file; it must never be reached", calls)
	}
	if v := testGaugeValue(t, r.successful); v != 0 {
		t.Fatalf("config_last_reload_successful is %v after a failed reload, want 0", v)
	}
}

// TestReloadWithNoConfigFileIsANoOpSuccess pins the fix for a multi build
// that never received --config.file (path == ""): a SIGHUP or POST
// /-/reload arriving there must not drive config_last_reload_successful to
// 0, because there is nothing to reload, not because anything failed. Run
// sets the gauge to 1 before serving; this proves reloadOnce leaves it
// there rather than calling fail().
func TestReloadWithNoConfigFileIsANoOpSuccess(t *testing.T) {
	calls := 0
	r := New(logger.NewTextLogger("error"), "", nil, func(*config.Config) error {
		calls++
		return nil
	})
	r.successful.Set(1) // mirrors what Run does before serving

	if err := r.Reload(); err != nil {
		t.Fatalf("reload with no configuration file returned an error, want a no-op success: %v", err)
	}
	if calls != 0 {
		t.Fatalf("apply ran %d times with no configuration file to read; it must never be reached", calls)
	}
	if v := testGaugeValue(t, r.successful); v != 1 {
		t.Fatalf("config_last_reload_successful is %v after a no-op reload, want 1 (nothing failed)", v)
	}
}

// TestReloadRefusesAChangedFlagsSection pins the whole-file refusal and the
// message naming the offending keys.
func TestReloadRefusesAChangedFlagsSection(t *testing.T) {
	dir := t.TempDir()
	p := writeConfig(t, dir, "flags:\n  log.level: info\nmodules:\n  prod: {}\n")
	boot, _ := config.Load(p)

	calls := 0
	r := New(logger.NewTextLogger("error"), p, boot, func(*config.Config) error {
		calls++
		return nil
	})

	writeConfig(t, dir, "flags:\n  log.level: debug\nmodules:\n  prod: {}\n")
	err := r.Reload()
	if err == nil {
		t.Fatal("reload accepted a changed flags: section")
	}
	if !strings.Contains(err.Error(), "log.level") {
		t.Fatalf("the refusal does not name the offending key: %v", err)
	}
	if calls != 0 {
		t.Fatalf("apply ran %d times despite the refusal", calls)
	}
}

// TestReloadFailsClosedWhenApplyErrors pins the one fail-closed branch none of
// the tests above reach: every prior failure test (unparseable file, changed
// flags: section) is refused BEFORE apply ever runs. This is the only test
// that lets apply itself be the thing that fails, which is what the package
// doc's "Fail closed" bullet actually promises for a caller's own refusal
// (a bad CA, a validation error inside apply), not just this package's own
// prepare-phase checks.
func TestReloadFailsClosedWhenApplyErrors(t *testing.T) {
	dir := t.TempDir()
	p := writeConfig(t, dir, "modules:\n  prod: {}\n")
	boot, _ := config.Load(p)

	calls := 0
	applyErr := errors.New("apply refused the new configuration")
	r := New(logger.NewTextLogger("error"), p, boot, func(*config.Config) error {
		calls++
		return applyErr
	})

	writeConfig(t, dir, "modules:\n  prod: {}\n  staging: {}\n")
	err := r.Reload()
	if err == nil {
		t.Fatal("reload succeeded despite apply returning an error")
	}
	if !errors.Is(err, applyErr) {
		t.Fatalf("reload's error does not wrap apply's own error: %v", err)
	}
	if calls != 1 {
		t.Fatalf("apply ran %d times, want exactly 1", calls)
	}
	if v := testGaugeValue(t, r.successful); v != 0 {
		t.Fatalf("config_last_reload_successful is %v after apply failed, want 0", v)
	}
	if v := testGaugeValue(t, r.successTime); v != 0 {
		t.Fatalf("config_last_reload_success_timestamp_seconds is %v after apply failed, want 0 (never advanced)", v)
	}
}

// TestReloadIsSerialized proves two concurrent reloads never overlap, which is
// what lets the HTTP handler report the status of ITS OWN reload.
func TestReloadIsSerialized(t *testing.T) {
	dir := t.TempDir()
	p := writeConfig(t, dir, "modules:\n  prod: {}\n")
	boot, _ := config.Load(p)

	var mu sync.Mutex
	inFlight, peak := 0, 0
	r := New(logger.NewTextLogger("error"), p, boot, func(*config.Config) error {
		mu.Lock()
		inFlight++
		if inFlight > peak {
			peak = inFlight
		}
		mu.Unlock()
		time.Sleep(5 * time.Millisecond)
		mu.Lock()
		inFlight--
		mu.Unlock()
		return nil
	})

	_, stop := startRun(r)
	defer stop()

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = r.Reload() }()
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if peak > 1 {
		t.Fatalf("%d reloads ran at once; they must be serialized", peak)
	}
}

// TestSIGHUPTriggersAReloadWithoutTerminatingTheProcess pins the package
// doc's hard SIGHUP invariant: "SIGHUP gets its OWN signal.Notify channel...
// routing it [through the shared NotifyContext] would cancel the process
// context and turn a reload into a shutdown." No test that only calls
// Reload() directly, however many times, can ever exercise Run's "case
// <-hup" branch: that requires a real, kernel-delivered signal. Sends actual
// SIGHUPs to this test process and waits for apply to observe one, then
// confirms the Reloader's own context is still alive.
//
// Waits on a channel apply signals, not on a wall-clock poll of a counter:
// an earlier version looped on atomic.LoadInt64 with a 1ms time.Sleep and a
// 5s deadline, which flaked in CI on a loaded runner (goroutine scheduling
// alone can eat well past 1ms under contention, and resending a real kill
// syscall every 1ms adds to exactly the load it is losing to). The event
// this test actually needs, "Run's hup case ran apply at least once", is
// available for free through the apply callback every New() caller already
// supplies; no change to reload.go itself, and no test-only export, was
// needed to observe it.
//
// Also waits for Run's own goroutine to have fully RETURNED before this
// test itself returns (see the <-done receive below), not just for ctx to
// be cancelled: Run's cleanup ends with signal.Ignore(syscall.SIGHUP) (see
// Run's own doc comment for why), which is process-wide and, per
// signal.Ignore's own contract, undoes every OTHER signal.Notify
// registration for that signal too, not only this Reloader's. Under
// `go test -count=N`, this test's own guard/hup registrations from the
// NEXT iteration would otherwise be able to start (and lose their
// registration to) a STILL-RUNNING previous iteration's deferred Ignore,
// a real cross-iteration flake reproduced empirically while hardening this
// test (running -count=20 without this wait failed roughly 1 run in 12).
// Waiting here keeps every iteration's signal-handler-table mutations fully
// serialized against the next one's.
func TestSIGHUPTriggersAReloadWithoutTerminatingTheProcess(t *testing.T) {
	dir := t.TempDir()
	p := writeConfig(t, dir, "modules:\n  prod: {}\n")
	boot, _ := config.Load(p)

	// Buffered so apply, running on Run's own goroutine, never blocks on
	// this test's own bookkeeping: a slow or absent receiver here must
	// never be able to stall the reload path itself.
	applied := make(chan struct{}, 1)
	r := New(logger.NewTextLogger("error"), p, boot, func(*config.Config) error {
		select {
		case applied <- struct{}{}:
		default:
		}
		return nil
	})

	// A guard registration for SIGHUP, taken out before Run's own and before
	// any signal is sent. Go disables a signal's default disposition (Term,
	// for SIGHUP) process-wide as soon as ANY channel has been registered for
	// it via signal.Notify; registering one here closes the brief window
	// between "go r.Run(ctx)" returning and Run's own signal.Notify call,
	// during which an unhandled SIGHUP would kill this test binary outright
	// (SIGHUP's default action) instead of failing this one test.
	guard := make(chan os.Signal, 1)
	signal.Notify(guard, syscall.SIGHUP)
	defer signal.Stop(guard)

	// Declared AFTER signal.Stop(guard) above, so it runs BEFORE it (defers
	// are LIFO): stop cancels and blocks until Run has actually returned,
	// so its own signal.Ignore has already run by the time this defer
	// completes. signal.Stop(guard) above then fires last, once Ignore is
	// no longer a risk to race, rather than possibly running (and losing
	// guard's own registration to Ignore) while Run's goroutine is still
	// shutting down. See startRun's own doc comment for why every Run
	// caller in this file, not just this test, needs this.
	ctx, stop := startRun(r)
	defer stop()

	// The first send can still race Run's own signal.Notify call (guard
	// above only prevents that race from killing the process, it says
	// nothing about whether hup itself was registered in time to receive
	// THIS particular signal), so it may need resending. 25ms, not 1ms: a
	// cadence that gives Run's goroutine room to actually get scheduled on
	// a busy machine instead of competing with it for CPU. The 20s outer
	// bound exists only to fail this one test instead of hanging the suite
	// forever if Run's hup case never fires at all; the common case exits
	// as soon as the event arrives; it does not wait out the bound.
	if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatalf("send SIGHUP: %v", err)
	}
	resend := time.NewTicker(25 * time.Millisecond)
	defer resend.Stop()
	timeout := time.NewTimer(20 * time.Second)
	defer timeout.Stop()
wait:
	for {
		select {
		case <-applied:
			break wait
		case <-resend.C:
			if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
				t.Fatalf("send SIGHUP: %v", err)
			}
		case <-timeout.C:
			t.Fatal("sent SIGHUP repeatedly for 20s and apply was never called; Run's hup case never fired")
		}
	}

	if ctx.Err() != nil {
		t.Fatal("the reloader's own context was cancelled by SIGHUP; a reload must never turn into a shutdown")
	}
}

// TestConcurrentReloadsMixBothPathsAndStaySerialized pins the property
// TestReloadIsSerialized leaves to chance: with Run genuinely consuming, a
// flood of concurrent Reload() callers must mix the channel path Run's own
// goroutine services with the inline fallback, not resolve entirely through
// one or the other, and stay serialized regardless of which path each one
// took. That mix is exactly the scenario the reloadOnceLocked fix above
// closes: a channel-delivered reload racing an inline one. Immediately
// after starting Run, TestReloadIsSerialized's own burst of concurrent
// Reload() calls races Run's startup (signal.Notify, then reaching its
// first select) against 10 goroutines being scheduled, a race the burst
// alone can lose badly enough that every call resolves inline before Run is
// ever scheduled, leaving Run's channel-consumption case unexercised. The
// blocking send on r.requests below, which bypasses Reload()'s own
// non-blocking select, first confirms Run has reached its consuming loop,
// closing that startup race before the burst begins.
func TestConcurrentReloadsMixBothPathsAndStaySerialized(t *testing.T) {
	dir := t.TempDir()
	p := writeConfig(t, dir, "modules:\n  prod: {}\n")
	boot, _ := config.Load(p)

	var mu sync.Mutex
	inFlight, peak, applies := 0, 0, 0
	r := New(logger.NewTextLogger("error"), p, boot, func(*config.Config) error {
		mu.Lock()
		inFlight++
		if inFlight > peak {
			peak = inFlight
		}
		applies++
		mu.Unlock()
		time.Sleep(2 * time.Millisecond)
		mu.Lock()
		inFlight--
		mu.Unlock()
		return nil
	})

	_, stop := startRun(r)
	defer stop()

	// A raw send on the unexported requests channel, not a call to Reload():
	// this line only returns once Run's "case reply := <-r.requests" has
	// actually received it, which is what confirms Run is at its consuming
	// loop before the burst below starts.
	primer := make(chan error, 1)
	r.requests <- primer
	if err := <-primer; err != nil {
		t.Fatalf("primer reload: %v", err)
	}

	const reloaders = 120
	var wg sync.WaitGroup
	for i := 0; i < reloaders; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := r.Reload(); err != nil {
				t.Errorf("reload: %v", err)
			}
		}()
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if peak > 1 {
		t.Fatalf("%d reloads ran at once across %d concurrent Reload() callers; they must be serialized", peak, reloaders)
	}
	if applies != reloaders+1 { // +1 for the primer above
		t.Fatalf("apply ran %d times, want exactly %d (%d Reload() calls plus the primer); some reload was lost", applies, reloaders+1, reloaders)
	}
}

// TestHandlerRejectsGET and TestHandlerReportsItsOwnFailure pin the HTTP
// surface.
func TestHandlerRejectsGET(t *testing.T) {
	dir := t.TempDir()
	p := writeConfig(t, dir, "modules:\n  prod: {}\n")
	boot, _ := config.Load(p)
	r := New(logger.NewTextLogger("error"), p, boot, func(*config.Config) error { return nil })

	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/-/reload", nil)
	r.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /-/reload returned %d, want 405", rec.Code)
	}
}

func TestHandlerReportsItsOwnFailure(t *testing.T) {
	dir := t.TempDir()
	p := writeConfig(t, dir, "modules:\n  prod: {}\n")
	boot, _ := config.Load(p)
	r := New(logger.NewTextLogger("error"), p, boot, func(*config.Config) error { return nil })

	writeConfig(t, dir, "modules:\n  prod: {\n")
	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/-/reload", nil)
	r.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("POST /-/reload on a broken file returned %d, want 500", rec.Code)
	}
}

// TestCollectorsRegistersBothGauges pins the contract main.go relies on to
// wire this package's metrics onto its own registry: exactly the successful
// and success-timestamp gauges, both registrable.
func TestCollectorsRegistersBothGauges(t *testing.T) {
	dir := t.TempDir()
	p := writeConfig(t, dir, "modules:\n  prod: {}\n")
	boot, _ := config.Load(p)
	r := New(logger.NewTextLogger("error"), p, boot, func(*config.Config) error { return nil })

	reg := prometheus.NewRegistry()
	for _, c := range r.Collectors() {
		if err := reg.Register(c); err != nil {
			t.Fatalf("register: %v", err)
		}
	}
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	if len(mfs) != 2 {
		t.Fatalf("Collectors() registered %d metric families, want 2 (successful, successTime)", len(mfs))
	}
}

// testGaugeValue reads a gauge without going through a registry.
func testGaugeValue(t *testing.T, g prometheus.Gauge) float64 {
	t.Helper()
	var m dto.Metric
	if err := g.Write(&m); err != nil {
		t.Fatalf("read gauge: %v", err)
	}
	return m.GetGauge().GetValue()
}
