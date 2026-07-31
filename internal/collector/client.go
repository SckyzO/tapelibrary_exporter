package collector

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	promconfig "github.com/prometheus/common/config"
)

// RequestDuration times every Client.Fetch call, labeled by outcome
// ("success" or "error"). It is declared once at package scope, not per
// Client, because Prometheus metrics are meant to live for the process
// lifetime, not the lifetime of whichever struct happens to touch them.
// Exported (capitalized) so main.go's // @@COLLECTOR_REGISTRY@@ wiring (see
// registry.frag) can register it directly on this exporter's own custom
// registry.
//
// Deliberately built with plain prometheus.NewHistogramVec, not
// promauto.NewHistogramVec: promauto's factories auto-register into
// prometheus.DefaultRegisterer, the process-wide global registry. But
// main.go builds its own prometheus.NewRegistry() instead of using that
// global one (see main.go's own comment on that line), and only collectors
// reachable via register()/tracker.Add(...) ever reach /metrics. A
// *prometheus.HistogramVec already implements prometheus.Collector, so
// registry.frag registers this var the exact same way it registers every
// other collector: no promauto, no second registry to thread through
// NewClient.
//
// Single process-wide histogram, labeled only by outcome: if
// /add-collector later wires a second HTTP-flavor collector against a
// different target, both collectors' request timings share these same
// "success"/"error" buckets and are not distinguishable from each other by
// target. Fine for this scaffold's one-collector v0.1 starting point; a
// per-target breakdown would need an additional label threaded through
// Client.
var RequestDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
	Name: "tapelibrary_exporter_request_duration_seconds",
	Help: "Duration in seconds of HTTP requests issued by this exporter's collectors, by outcome.",
}, []string{"outcome"})

// Transport holds the *http.Client every Client bound to one machine shares.
// The pointer is atomic so a configuration reload can replace the transport
// underneath collectors that are already running: their next request uses the
// new credentials, their goroutine is never stopped, and their cache survives.
//
// A nil-safe zero value is deliberately NOT offered: a Transport with no
// client would fail every request with a confusing nil dereference, and every
// construction path here supplies one.
type Transport struct {
	hc atomic.Pointer[http.Client]
}

// NewTransport wraps an already-built *http.Client. Build the client with
// NewHTTPClient so its authentication and TLS come from the configuration.
func NewTransport(hc *http.Client) *Transport {
	t := &Transport{}
	t.hc.Store(hc)
	return t
}

// Set replaces the shared client. Callers should call CloseIdleConnections on
// the one they replaced: it only closes IDLE connections, so a request already
// in flight on the old client finishes undisturbed.
func (t *Transport) Set(hc *http.Client) { t.hc.Store(hc) }

// Get returns the client in force right now. Fetch calls it once per request,
// so a request that starts before a reload completes runs entirely on the
// transport it started with.
func (t *Transport) Get() *http.Client { return t.hc.Load() }

// Client is the HTTP boundary every collector in this flavor talks to.
// Building it around a base URL, instead of reaching for http.DefaultClient
// directly from a collector, is what makes collectors testable: point
// NewClient at an httptest.Server instead of a real target.
//
// There are two deadline mechanisms here and exactly one is active per
// construction path, which is what keeps the pre-v0.7 path unchanged:
//
//   - NewClient owns a PRIVATE *http.Client and puts the deadline on its
//     Timeout field, leaving c.timeout at zero. This is what every shipped
//     collector test and every repository scaffolded before v0.7 uses, and its
//     connection behaviour is identical to what it always was.
//   - NewClientOn SHARES a Transport with the other collectors of one machine,
//     so the deadline cannot live on http.Client.Timeout (it would be the same
//     for all of them). It goes in c.timeout and Fetch applies it as a context
//     deadline instead, which is what lets one collector wait fifteen minutes
//     while its sibling waits five seconds.
type Client struct {
	tr      *Transport
	baseURL string
	timeout time.Duration // 0 means "the deadline is on the shared-nothing http.Client"

	// acquireTimeout bounds ONLY the wait for a limiter slot, never the
	// request that follows it. It exists because http.Client.Timeout (the
	// mechanism that bounds the request itself on every path except
	// NewClientOn) does not start counting until Do begins: it gives no
	// cover at all for time already spent blocked in Acquire before Do ever
	// runs. Every constructor below sets it to the same per-request deadline
	// it already knows, so a Client's worst case with a limiter attached is
	// at most acquireTimeout (the wait) plus whatever already bounds the
	// request, never an unbounded queue on top of an already-bounded
	// request. See acquire and WithLimiter.
	acquireTimeout time.Duration
	limiter        *Limiter // nil means unlimited, see limiter.go
}

// NewClient builds a Client bound to target (a base URL, for example
// "http://localhost:9100"), applying timeout to every request. Unchanged
// since v0.1: its transport is private to this Client and its deadline is
// http.Client.Timeout.
//
// acquireTimeout is set to the same timeout so that IF a limiter is later
// attached with WithLimiter, the wait for a slot is bounded too. With no
// limiter attached (every deployment before v0.7, and every deployment today
// that never calls WithLimiter) acquireTimeout is stored but never read, so
// this line changes nothing observable: see acquire.
func NewClient(target string, timeout time.Duration) *Client {
	return &Client{
		tr:             NewTransport(&http.Client{Timeout: timeout}),
		baseURL:        target,
		acquireTimeout: timeout,
	}
}

// NewClientOn binds a Client to a SHARED Transport, with its own per-collector
// deadline. This is the multi-instance construction path: one Transport per
// watched machine, one Client per collector polling it. A concurrency ceiling
// on that same machine sits beside this Client, not inside it: see Limiter.
//
// A non-positive timeout is a programming error rather than "no deadline": the
// shared http.Client carries no Timeout of its own, so a collector reaching
// here without one would hang its poller forever. Callers that take the
// timeout from a flag should reject a non-positive value at boot; see
// instance.Handle.ClientFor, which returns an error for exactly this.
//
// "The shared http.Client carries no Timeout of its own" is a precondition
// NewClientOn trusts, not one it can check: tr may be reused by later Set
// calls with a client built anywhere. It holds only because whoever built
// that *http.Client did so through NewHTTPClient with timeout 0, exactly as
// NewHTTPClient's own doc comment requires for a client meant to back a
// Transport shared by more than one NewClientOn. Build it any other way and
// this Client's own timeout stops being the only deadline in play, silently.
func NewClientOn(tr *Transport, target string, timeout time.Duration) *Client {
	return &Client{tr: tr, baseURL: target, timeout: timeout, acquireTimeout: timeout}
}

// WithLimiter returns the same Client bound to lim, which bounds how many
// requests this exporter has in flight against one machine at a time. A nil
// limiter means unlimited, which is the default.
//
// Deliberately total: attach and return, no validation, no panic. This method
// is reachable from a live scrape, not only from boot-time wiring:
// wiring/probe_factory.frag calls NewClientFor once per incoming probe
// request, from inside the HTTP handler (see probe.go's own factory-call
// site), so anything this method does runs on a request goroutine that
// net/http will recover a panic on rather than crash the process, turning a
// misconfiguration into silently broken probes and stack-trace spam instead
// of a clean, loud boot failure. A Client with no bound on its acquire wait
// (acquireTimeout and timeout both zero) attached to a real limiter here will
// therefore wait unboundedly inside Fetch's acquire (see its own comment):
// that is a real gap, left open on purpose rather than guarded badly here.
//
// The check lives at flag-parse time instead: the single target model's
// code/http/wiring/client_build.frag, spliced into main.go at
// // @@CLIENT_BUILD@@, rejects at boot any collector whose own
// --collector.<name>.timeout is non-positive while a ceiling is configured,
// the same way instance.Handle.ClientFor rejects a non-positive NewClientOn
// timeout (see NewClientOn's own doc comment) by returning an error instead
// of proceeding, right before the exampleClient this method is called on
// gets built. Skipping that check would let --collector.<name>.timeout=0s, a
// flag combination that works today with no ceiling configured, combine
// with a configured ceiling to produce exactly the unbounded wait described
// above, and this method has no way to catch that at the point it runs. The
// multi-instance model needs no equivalent of its own: instance.Handle.
// ClientFor above is that model's own flag-parse-time (well,
// reload-prepare-time) check, and it runs before this method ever does.
func (c *Client) WithLimiter(lim *Limiter) *Client {
	c.limiter = lim
	return c
}

// Limiter returns the ceiling this Client contends for, or nil when
// unlimited. Exported for tests that assert two collectors of one instance
// share one ceiling.
func (c *Client) Limiter() *Limiter { return c.limiter }

// NewHTTPClient builds the *http.Client carrying the authentication and TLS
// declared in --config.file's http_client_config section.
//
// Build it ONCE and share it across targets. NewClientFromConfig mints a
// fresh http.Transport on every call and caches nothing, so building one per
// request would give each request a private connection pool, re-read the CA
// and credential files from disk, and leave the discarded transport holding
// idle connections until IdleConnTimeout. An *http.Client and the transport
// inside it are safe for concurrent use, which is what makes sharing correct.
//
// Pass 0 for timeout when the *http.Client built here is going to back a
// Transport shared by more than one NewClientOn call: that is not a missing
// deadline, it is the ONLY way to keep a single deadline from applying to
// every collector on the Transport. A Client built with NewClientOn carries
// its own deadline in Client.timeout, applied per request by Fetch as a
// context, precisely so collectors sharing one Transport can disagree on how
// long to wait. Pass anything but 0 in that case and hc.Timeout still wins
// underneath every one of them: http.Client.Timeout fires regardless of the
// context deadline Fetch also set, whichever is shorter, so the failure is
// silent rather than a build error. There is no check here that catches a
// caller getting this wrong, because by the time NewClientOn runs, the
// *http.Client already exists; this comment, and
// TestNewHTTPClientLeavesZeroTimeoutForASharedTransport in this package's own
// test file, are the contract. Reserve a non-zero timeout for a *http.Client
// this function's caller keeps entirely to itself and never hands to
// NewTransport more than once, such as NewClientWithConfig below.
//
// internal/instance's Registry (the multi-instance reconciler) is the caller
// this paragraph is for: its clientFor helper always passes 0 here, on
// purpose, when it builds or rotates a machine's shared transport, because
// that transport backs every one of that machine's collectors at once and
// each carries its own deadline through Handle.ClientFor instead.
//
// NewClientFromConfig's own transport already sizes its idle connection pool
// for many callers sharing one host, not one: it sets MaxIdleConnsPerHost to
// 1000 (see prometheus/common/config, which cites
// https://github.com/golang/go/issues/13801 for why net/http's own default of
// 2 is too small to share), far above what even a few dozen collectors on one
// machine would hold open at once. That is deliberate on the dependency's
// part and is why this function does not also set idle connection limits of
// its own: doing so would either duplicate a value already generous enough or
// require reaching through whatever http.RoundTripper this config wraps the
// base *http.Transport in (authentication, TLS reload, HTTP/2), which is not
// guaranteed to be a *http.Transport at the top level.
func NewHTTPClient(httpCfg promconfig.HTTPClientConfig, timeout time.Duration) (*http.Client, error) {
	hc, err := promconfig.NewClientFromConfig(httpCfg, "tapelibrary")
	if err != nil {
		return nil, fmt.Errorf("build HTTP client from http_client_config: %w", err)
	}
	// NewClientFromConfig sets no overall deadline, so apply the same
	// per-request timeout NewClient applies. 0 is left as 0: see this
	// function's own doc comment on why that matters for a shared Transport.
	hc.Timeout = timeout
	return hc, nil
}

// NewClientFor binds an already-built *http.Client to one base URL. Many
// targets share one *http.Client, each paying only this struct.
//
// acquireTimeout is read off hc.Timeout, not passed separately: for every
// caller of this function that timeout IS the deadline that already governs
// the request (NewClientWithConfig below builds hc with exactly the timeout
// it was given), so reading it back here is the same value, not a guess. The
// one construction NewHTTPClient's own doc comment reserves a 0 Timeout for
// (backing a Transport shared by more than one NewClientOn) never reaches
// this function under today's callers, so a 0 read here means genuinely no
// bound is known; a Client built this way and then handed a real limiter via
// WithLimiter would wait unboundedly, a gap that method's own doc comment
// names and points at the flag-parse-time check that must prevent it.
func NewClientFor(target string, hc *http.Client) *Client {
	return &Client{tr: NewTransport(hc), baseURL: target, acquireTimeout: hc.Timeout}
}

// NewClientWithConfig builds a Client whose transport carries the
// authentication and TLS declared in --config.file's http_client_config
// section. It sits beside NewClient rather than replacing it: NewClient's
// signature is depended on by every collector test this scaffold ships and by
// any repo scaffolded before the configuration layer existed.
//
// Use it only when the operator actually supplied that section. With no
// section, keep calling NewClient: NewClientFromConfig returns a client with
// its own transport settings, so routing the no-authentication case through
// here would quietly change connection behaviour (keep-alives, HTTP/2) for
// every existing deployment.
func NewClientWithConfig(target string, timeout time.Duration, httpCfg promconfig.HTTPClientConfig) (*Client, error) {
	hc, err := NewHTTPClient(httpCfg, timeout)
	if err != nil {
		return nil, err
	}
	return NewClientFor(target, hc), nil
}

// Fetch issues a GET request for baseURL+path and returns the response
// body. A non-2xx status is reported as an error so callers never have to
// separately inspect the status code. Every call, success or failure, is
// timed and recorded on RequestDuration.
func (c *Client) Fetch(ctx context.Context, path string) (data []byte, err error) {
	start := time.Now()
	defer func() {
		outcome := "success"
		if err != nil {
			outcome = "error"
		}
		RequestDuration.WithLabelValues(outcome).Observe(time.Since(start).Seconds())
	}()

	// The per-collector deadline, for Clients sharing a Transport (see
	// NewClientOn). A Client built with NewClient never reaches this: its
	// deadline lives on its own private http.Client.Timeout, so its c.timeout
	// stays zero and this block is a no-op for it.
	//
	// Applied before the limiter below on purpose: for a Client on this path,
	// ctx now carries the ONLY deadline that governs the rest of this call,
	// wait and request together, so the wait is charged against this
	// collector's own budget, never added on top of it. A Client whose
	// deadline lives on http.Client.Timeout instead reaches acquire below
	// with no deadline on ctx at all; that path bounds the wait itself, via
	// acquireTimeout, for exactly this reason. See acquire's own comment.
	if c.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}

	release, err := c.acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("wait for a request slot for %s: %w", path, err)
	}
	defer release()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, fmt.Errorf("build request for %s: %w", path, err)
	}

	resp, err := c.tr.Get().Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("fetch %s: unexpected status %s", path, resp.Status)
	}

	data, err = io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response body for %s: %w", path, err)
	}
	return data, nil
}

// acquire waits for a limiter slot, bounding that wait to acquireTimeout so
// it can never exceed the same deadline that already governs the request
// about to follow it. The bound is applied to a context local to this call
// only: it never replaces or shortens the ctx Fetch goes on to build the
// request with, so a Client whose request deadline lives on
// http.Client.Timeout (every constructor except NewClientOn) keeps that
// deadline exactly as it was before, limiter or not.
//
// Total worst case once a limiter is attached: acquireTimeout (the wait)
// plus whatever already bounds the request, at most twice the configured
// timeout, and if the wait alone exceeds that budget Fetch fails fast with
// an error instead of eventually succeeding late (see Fetch's own error
// message for this). For NewClientOn, ctx already carries c.timeout's
// deadline by the time this runs (see Fetch above), so the wait is already
// covered by that single deadline and acquireTimeout adds nothing further:
// worst case there stays exactly c.timeout, unchanged from before WithLimiter
// existed.
func (c *Client) acquire(ctx context.Context) (func(), error) {
	if c.limiter == nil {
		// Not a nil dereference: Acquire's own nil check (see its doc
		// comment) is what decides "no limiter" means unlimited, and calling
		// through the nil *Limiter here, rather than duplicating that
		// no-op release here, keeps this the ONE place that decision is
		// made.
		return c.limiter.Acquire(ctx)
	}
	if c.acquireTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.acquireTimeout)
		defer cancel()
	}
	return c.limiter.Acquire(ctx)
}
