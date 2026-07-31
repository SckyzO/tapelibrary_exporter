package collector

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	promconfig "github.com/prometheus/common/config"
)

// statusTrackerSuccessMetric is the collector_success metric family name,
// referenced by the background collector's own StatusTracker test. On a
// multi-instance scaffold the background collector is the starter, so this
// shared declaration lives here rather than in the synchronous collector test
// (which such a scaffold does not ship).
const statusTrackerSuccessMetric = "tapelibrary_exporter_collector_success"

// TestRequestDuration_CustomRegistryReachable locks that RequestDuration is a
// plain, exported *prometheus.HistogramVec this exporter's own custom registry
// can register directly, not a promauto value reachable only from
// prometheus.DefaultRegisterer.
func TestRequestDuration_CustomRegistryReachable(t *testing.T) {
	RequestDuration.WithLabelValues("success").Observe(0.01)

	reg := prometheus.NewRegistry()
	if err := reg.Register(RequestDuration); err != nil {
		t.Fatalf("Register(RequestDuration) on a fresh custom registry: %v", err)
	}
	count, err := testutil.GatherAndCount(reg, "tapelibrary_exporter_request_duration_seconds")
	if err != nil {
		t.Fatalf("GatherAndCount(custom registry): %v", err)
	}
	if count == 0 {
		t.Fatal("GatherAndCount(custom registry) = 0, want >= 1: RequestDuration did not reach a registry it was explicitly registered on")
	}

	dcount, err := testutil.GatherAndCount(prometheus.DefaultGatherer, "tapelibrary_exporter_request_duration_seconds")
	if err != nil {
		t.Fatalf("GatherAndCount(DefaultGatherer): %v", err)
	}
	if dcount != 0 {
		t.Fatalf("GatherAndCount(DefaultGatherer) = %d, want 0: RequestDuration must not self-register into the process-wide default registerer", dcount)
	}
}

func TestNewClientWithConfigAppliesBasicAuth(t *testing.T) {
	var gotUser, gotPass string
	var gotOK bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUser, gotPass, gotOK = r.BasicAuth()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{}"))
	}))
	defer srv.Close()

	cfg := promconfig.HTTPClientConfig{
		BasicAuth: &promconfig.BasicAuth{Username: "monitor", Password: "hunter2"},
	}
	c, err := NewClientWithConfig(srv.URL, time.Second, cfg)
	if err != nil {
		t.Fatalf("NewClientWithConfig: %v", err)
	}
	if _, err := c.Fetch(context.Background(), "/"); err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	if !gotOK {
		t.Fatal("the request carried no basic auth header")
	}
	if gotUser != "monitor" || gotPass != "hunter2" {
		t.Errorf("credentials = %q/%q, want monitor/hunter2", gotUser, gotPass)
	}
}

func TestNewClientWithConfigRejectsAnUnreadableCA(t *testing.T) {
	cfg := promconfig.HTTPClientConfig{
		TLSConfig: promconfig.TLSConfig{CAFile: "/nonexistent/ca.pem"},
	}
	if _, err := NewClientWithConfig("https://example.invalid", time.Second, cfg); err == nil {
		t.Fatal("NewClientWithConfig accepted a CA file that cannot be read")
	}
}

// TestNewClientForSharesOneTransport proves NewClientFor binds two targets to
// the SAME *http.Client instead of building a private one per target.
func TestNewClientForSharesOneTransport(t *testing.T) {
	hc, err := NewHTTPClient(promconfig.HTTPClientConfig{}, 5*time.Second)
	if err != nil {
		t.Fatalf("NewHTTPClient: %v", err)
	}

	a := NewClientFor("http://a.example", hc)
	b := NewClientFor("http://b.example", hc)

	if a.tr.Get() != b.tr.Get() {
		t.Error("two targets built their own http.Client; the transport must be shared")
	}
	if a.baseURL == b.baseURL {
		t.Errorf("both clients bound to %s; each target must keep its own base URL", a.baseURL)
	}
}

// TestTransportSetIsVisibleToExistingClients proves the reload seam: a Client
// built on a Transport picks up a replacement *http.Client without being
// rebuilt. This is what lets a multi-instance reload rotate credentials
// without stopping the poller and dropping its cache. Duplicated from
// code/http/collector_test.go.tmpl rather than shared: this file exists
// specifically because the background collector's own test file replaces
// collector_test.go.tmpl wholesale for --target-model multi-instance, so
// NewClientOn and Transport need a caller here too, in the model this
// reload work is actually for, or `deadcode -test ./...` (see
// Makefile.tmpl's make deadcode target) finds them unreachable.
func TestTransportSetIsVisibleToExistingClients(t *testing.T) {
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("first"))
	}))
	defer first.Close()

	tr := NewTransport(&http.Client{})
	c := NewClientOn(tr, first.URL, time.Second)

	got, err := c.Fetch(context.Background(), "/")
	if err != nil {
		t.Fatalf("first fetch: %v", err)
	}
	if string(got) != "first" {
		t.Fatalf("first fetch returned %q, want %q", got, "first")
	}

	// atomic.Pointer exists precisely so a reload's Set can land while a
	// poller's Fetch is already reading the pointer: hammer both concurrently
	// with no ordering between them and run under -race to prove that race is
	// safe, not just that a swap made between two sequential Fetch calls is
	// visible (the weaker property the block below also shows).
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_, _ = c.Fetch(context.Background(), "/")
			}
		}
	}()
	for i := 0; i < 200; i++ {
		if i%2 == 0 {
			tr.Set(&http.Client{})
		} else {
			tr.Set(&http.Client{Transport: refusingRoundTripper{}})
		}
	}
	close(stop)
	wg.Wait()

	// Swap the transport for one that refuses every request, and prove the
	// SAME Client now uses it.
	tr.Set(&http.Client{Transport: refusingRoundTripper{}})
	if _, err := c.Fetch(context.Background(), "/"); err == nil {
		t.Fatal("fetch succeeded after Transport.Set installed a refusing client")
	}
}

// refusingRoundTripper fails every request, so a test can prove which
// transport a Client actually used.
type refusingRoundTripper struct{}

func (refusingRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("refused by test transport")
}

// TestNewClientOnAppliesItsOwnTimeout proves two collectors can share one
// transport and still carry different per-collector deadlines, which
// http.Client.Timeout cannot express because it lives on the shared client.
func TestNewClientOnAppliesItsOwnTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		_, _ = w.Write([]byte("slow"))
	}))
	defer srv.Close()

	tr := NewTransport(&http.Client{})
	quick := NewClientOn(tr, srv.URL, 20*time.Millisecond)
	patient := NewClientOn(tr, srv.URL, 5*time.Second)

	if _, err := quick.Fetch(context.Background(), "/"); err == nil {
		t.Fatal("the 20ms client did not time out against a 200ms handler")
	}
	if _, err := patient.Fetch(context.Background(), "/"); err != nil {
		t.Fatalf("the 5s client failed against a 200ms handler: %v", err)
	}
}

// TestNewHTTPClientLeavesZeroTimeoutForASharedTransport pins the other half
// of the timeout contract: a *http.Client built to back a Transport shared by
// more than one NewClientOn call must carry no Timeout of its own, or that
// single shared value silently overrides every collector's own per-Client
// deadline the moment it is shorter (see NewHTTPClient's and NewClientOn's
// own doc comments for why). NewHTTPClient must return timeout exactly as
// passed, never substituting a default when it is 0.
func TestNewHTTPClientLeavesZeroTimeoutForASharedTransport(t *testing.T) {
	hc, err := NewHTTPClient(promconfig.HTTPClientConfig{}, 0)
	if err != nil {
		t.Fatalf("NewHTTPClient: %v", err)
	}
	if hc.Timeout != 0 {
		t.Fatalf("NewHTTPClient(cfg, 0).Timeout = %v, want 0: a *http.Client meant to back a shared Transport must carry no deadline of its own", hc.Timeout)
	}
}

// TestClientWithLimiterBlocksASecondConcurrentFetch proves WithLimiter
// actually reaches Fetch, not merely that the field gets set: with the
// ceiling at 1, a second Fetch blocks while the first holds the only slot,
// and proceeds the moment the first releases it. Duplicated from
// code/http/collector_test.go.tmpl rather than shared, for the same reason
// TestTransportSetIsVisibleToExistingClients above is: this file replaces
// collector_test.go.tmpl wholesale for --target-model multi-instance, so
// WithLimiter needs a caller here too, or deadcode -test ./... finds it
// unreachable.
func TestClientWithLimiterBlocksASecondConcurrentFetch(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(started) })
		<-release
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	tr := NewTransport(&http.Client{})
	c := NewClientOn(tr, srv.URL, 5*time.Second).WithLimiter(NewLimiter(1))

	first := make(chan struct{})
	go func() {
		_, _ = c.Fetch(context.Background(), "/")
		close(first)
	}()
	<-started // the first Fetch has acquired the only slot and reached the handler

	second := make(chan struct{})
	go func() {
		_, _ = c.Fetch(context.Background(), "/")
		close(second)
	}()

	select {
	case <-second:
		t.Fatal("the second Fetch proceeded while the first held the only limiter slot")
	case <-time.After(50 * time.Millisecond):
	}

	close(release)

	select {
	case <-second:
	case <-time.After(2 * time.Second):
		t.Fatal("the second Fetch did not proceed after the first released its slot")
	}
	<-first
}

// TestClientWithLimiterBoundsTheAcquireWaitOnNewClient proves the fix for a
// Critical review finding: on NewClient, the only deadline mechanism is
// http.Client.Timeout, which does not start counting until Do begins, so it
// gives no cover at all for time spent blocked in Acquire before Do ever
// runs. Duplicated from code/http/collector_test.go.tmpl for the same reason
// TestTransportSetIsVisibleToExistingClients above is: this file replaces
// collector_test.go.tmpl wholesale for --target-model multi-instance.
//
// Proven directly rather than by timing an HTTP round trip: the only slot is
// acquired here and never released, so success would mean an actually
// unbounded wait, and the assertion is on a measured duration, not a comment.
func TestClientWithLimiterBoundsTheAcquireWaitOnNewClient(t *testing.T) {
	const timeout = 50 * time.Millisecond
	lim := NewLimiter(1)
	release, err := lim.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire the only slot: %v", err)
	}
	defer release()

	c := NewClient("http://example.invalid", timeout).WithLimiter(lim)

	done := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		_, _ = c.Fetch(context.Background(), "/")
		done <- time.Since(start)
	}()

	select {
	case elapsed := <-done:
		if elapsed > 3*timeout {
			t.Fatalf("Fetch took %v to fail with the only slot permanently held; want at most %v: the acquire wait must be bounded by the Client's own timeout even though the ctx passed in (context.Background()) carries no deadline of its own", elapsed, 3*timeout)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Fetch did not return within 2s: the acquire wait is unbounded on a Client built by NewClient with a limiter attached, exactly the Critical finding this test guards against")
	}
}

// TestClientWithLimiterIsTotalEvenWithoutABoundedAcquireWait proves
// WithLimiter never panics, including on a Client with no bound at all on its
// acquire wait. WithLimiter is reachable from a live probe request goroutine
// (see its own doc comment), where a panic would crash that request instead
// of failing loudly at boot, so validating this combination belongs at
// flag-parse time, in code/http/wiring/client_build.frag (the boot-time
// check ahead of WithLimiter), not here. Duplicated from
// code/http/collector_test.go.tmpl for the same reason the other Client
// tests in this file are.
func TestClientWithLimiterIsTotalEvenWithoutABoundedAcquireWait(t *testing.T) {
	c := NewClientFor("http://example.invalid", &http.Client{}) // Timeout left at zero
	if got := c.WithLimiter(NewLimiter(1)); got != c {
		t.Fatal("WithLimiter did not return the same Client")
	}
}
