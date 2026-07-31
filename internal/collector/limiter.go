package collector

import (
	"context"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// RequestWait records how long a request waited for a concurrency slot,
// labeled by outcome ("success" if a slot was granted, "error" if ctx was
// done first, see Acquire below), the same "outcome" convention
// RequestDuration next to it already uses, and the same values: Acquire's own
// two returns are exactly success (nil error) and error (ctx.Err()), so this
// label is not a second, independent vocabulary. No per-instance label: the
// exporter's own registry carries none, and the per-collector freshness gauge
// the background variant already emits is what identifies WHICH collector is
// starving. This one answers whether anything is queueing at all, and
// whether the queueing paid off.
//
// Registered alongside --exporter.max-requests-per-target, the flag that
// turns a ceiling on: registry.frag registers it for single (the same way it
// registers RequestDuration), and mains/multi/main.go.tmpl and
// mains/multi-instance/main.go.tmpl each hardcode their own
// reg.MustRegister(collector.RequestWait) call beside their own
// RequestDuration one. It reaches every http-flavor target model's /metrics,
// including multi, which never attaches a ceiling and so always reports zero
// here: a permanent zero is the truthful reading of "no ceiling configured",
// not a sign the metric is broken. init() below is what keeps that permanent
// zero VISIBLE rather than the series going missing outright: a HistogramVec
// with a label reports nothing at all for a label value that has never been
// observed, unlike the plain, label-less Histogram this var used to be, which
// always self-reported zero once registered, whether or not Observe ever ran.
//
// Observed whenever a ceiling is actually configured, even when a slot is
// immediately free (the wait is then near zero). On the nil fast path (no
// ceiling configured, the default) Acquire returns before touching this
// histogram at all, matching the "costs one nil check and no allocation"
// contract below: there is nothing to report when there is no ceiling to
// wait on.
var RequestWait = prometheus.NewHistogramVec(prometheus.HistogramOpts{
	Name: "tapelibrary_exporter_request_wait_seconds",
	Help: "Duration in seconds a request waited for a per-target concurrency slot, by outcome.",
}, []string{"outcome"})

func init() {
	// Touch both outcome values once, at package init, so RequestWait always
	// exposes both series (each zero-valued until observed) instead of being
	// entirely absent from /metrics whenever no ceiling is configured, which
	// is every build's default and multi's permanent state (see the var's own
	// doc comment above). A HistogramVec that has never seen WithLabelValues
	// for a given label combination reports nothing at all for it; this is
	// what restores the plain Histogram's old "always present, permanent
	// zero" behaviour under the new label.
	RequestWait.WithLabelValues("success")
	RequestWait.WithLabelValues("error")
}

// Limiter bounds how many requests this exporter has in flight against one
// target at a time. It exists because every collector under the multi-instance
// target model is a background poller with its own goroutine and its own
// ticker, entirely outside the sequential collection StatusTracker performs, so
// nothing else caps how many of them hit one machine at once.
//
// A nil *Limiter is a valid, unlimited limiter, which is the default: it
// takes a positive --exporter.max-requests-per-target to construct a real
// one (see Limiters below and instance.NewHandle for the multi-instance
// model's own construction site). The common path, nil, costs one nil check
// and no allocation regardless.
type Limiter struct {
	slots chan struct{}
}

// NewLimiter returns a limiter admitting at most limit concurrent requests, or
// nil when limit is not positive, meaning unlimited.
func NewLimiter(limit int) *Limiter {
	if limit <= 0 {
		return nil
	}
	return &Limiter{slots: make(chan struct{}, limit)}
}

// Acquire blocks until a slot is free or ctx is done, and returns the function
// that gives the slot back. The returned release is always safe to call, and
// is a no-op when err is non-nil, so callers can defer it unconditionally
// after checking the error.
//
// Honouring ctx is what makes a ceiling safe to turn on: the caller's own
// deadline (a collector's --collector.<name>.timeout) bounds the wait, so a
// starved poller fails its refresh, logs, and keeps its previous cache. Without
// it the poller would sit in the queue while time.Ticker silently dropped its
// ticks and the effective refresh interval drifted with nothing reporting it.
func (l *Limiter) Acquire(ctx context.Context) (func(), error) {
	if l == nil {
		return func() {}, nil
	}
	start := time.Now()
	select {
	case l.slots <- struct{}{}:
		RequestWait.WithLabelValues("success").Observe(time.Since(start).Seconds())
		var once sync.Once
		return func() { once.Do(func() { <-l.slots }) }, nil
	case <-ctx.Done():
		RequestWait.WithLabelValues("error").Observe(time.Since(start).Seconds())
		return func() {}, ctx.Err()
	}
}

// LimiterSet indexes one Limiter per target address. It exists for the
// single-target model, where each collector carries its own
// --collector.<name>.target and there is no shared per-machine object to hang a
// ceiling on: collectors naming the same address must share one ceiling, and
// collectors naming different addresses must not.
//
// The multi-instance model does NOT use this: there, instance.Handle owns one
// Limiter per watched machine by construction, so an instance added by a reload
// gets its own without anything having to be pre-populated. Keeping that model
// off this index is deliberate, because a pre-populated map would silently fail
// to cover an instance added later.
//
// For is called at startup only, over the finite and immutable set of target
// flags, so the key space is never caller-controlled.
type LimiterSet struct {
	limit int

	mu       sync.Mutex
	byTarget map[string]*Limiter
}

// NewLimiterSet returns a set handing out limiters with the given ceiling. A
// non-positive ceiling makes every For return nil, meaning unlimited.
func NewLimiterSet(limit int) *LimiterSet {
	return &LimiterSet{limit: limit, byTarget: make(map[string]*Limiter)}
}

// For returns the limiter shared by every collector pointed at target.
func (s *LimiterSet) For(target string) *Limiter {
	if s == nil || s.limit <= 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if l, ok := s.byTarget[target]; ok {
		return l
	}
	l := NewLimiter(s.limit)
	s.byTarget[target] = l
	return l
}

// Limiters is the single, shared LimiterSet every single-target-model
// collector's client_build wiring consults. It is a package-level var, not a
// local one, specifically so it can be built exactly once no matter how many
// collectors' client_build.frag runs in the same main(): the wiring that
// constructs it (see code/http/wiring/client_build.frag) does so only when
// this is still nil, so the first collector's frag builds the real
// *LimiterSet and every later one, including anything /add-collector
// appends, reuses that same set instead of silently replacing it. Reusing
// the same set is what makes For's own promise hold across collectors, not
// just within one: two collectors naming the same --collector.<name>.target
// share one Limiter only if they consult the same LimiterSet.
var Limiters *LimiterSet
