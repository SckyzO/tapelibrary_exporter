package instance

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	promconfig "github.com/prometheus/common/config"

	"github.com/sckyzo/tapelibrary_exporter/internal/config"
	"github.com/sckyzo/tapelibrary_exporter/internal/logger"
)

// fakeBG stands in for a background collector. It counts Start calls, which is
// how every reconciler test below distinguishes "kept running" from "rebuilt":
// a handle that was rebuilt has a NEW fakeBG at 1 start, a handle that survived
// has the SAME fakeBG still at 1.
type fakeBG struct {
	starts int32
	desc   *prometheus.Desc
	done   chan struct{}
}

func newFakeBG(name string) *fakeBG {
	return &fakeBG{
		desc: prometheus.NewDesc("demo_"+name, "fixture", nil, nil),
		done: make(chan struct{}),
	}
}

func (f *fakeBG) Describe(ch chan<- *prometheus.Desc) { ch <- f.desc }

// Always emits exactly one metric, so StatusTracker reports it healthy: a
// collector emitting none is counted as a failed scrape.
func (f *fakeBG) Collect(ch chan<- prometheus.Metric) {
	ch <- prometheus.MustNewConstMetric(f.desc, prometheus.GaugeValue, 1)
}

func (f *fakeBG) Start(ctx context.Context) {
	atomic.AddInt32(&f.starts, 1)
	go func() { <-ctx.Done(); close(f.done) }()
}

func (f *fakeBG) Done() <-chan struct{} { return f.done }

// Compile-time proof the fake satisfies the seam.
var _ BackgroundCollector = (*fakeBG)(nil)

// TestFactoryBuildsAndStarts proves New receives the exact Handle passed to
// it, not a copy or a reconstruction from its fields: a factory that only
// read Handle.Address back out would not notice a later SetTransport or
// ClientFor call landing on a DIFFERENT Handle value.
func TestFactoryBuildsAndStarts(t *testing.T) {
	enabled := true
	var got *Handle
	f := Factory{
		Name:    "fake",
		Enabled: &enabled,
		New: func(h *Handle) (BackgroundCollector, error) {
			got = h
			return newFakeBG("fake"), nil
		},
	}

	h := NewHandle("machine-a", "https://machine-a", &http.Client{}, 0, nil)
	bg, err := f.New(h)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got != h {
		t.Error("New received a different Handle than the one passed to it")
	}
	if !*f.Enabled {
		t.Error("Enabled = false, want true")
	}

	ctx, cancel := context.WithCancel(context.Background())
	bg.Start(ctx)
	cancel()
	select {
	case <-bg.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("Done() did not close within 2s after cancel")
	}
}

// TestClientForRejectsANonPositiveTimeout pins the reason ClientFor returns an
// error at all: a Handle's transport carries no http.Client.Timeout, so a
// collector reaching it with no deadline would hang its poller forever. That is
// a configuration fault and it must fail the boot, not surface at 3am.
func TestClientForRejectsANonPositiveTimeout(t *testing.T) {
	h := NewHandle("lib1", "https://a.example", &http.Client{}, 0, nil)
	if _, err := h.ClientFor(0); err == nil {
		t.Fatal("ClientFor accepted a zero timeout")
	}
	if _, err := h.ClientFor(-time.Second); err == nil {
		t.Fatal("ClientFor accepted a negative timeout")
	}
	if _, err := h.ClientFor(5 * time.Second); err != nil {
		t.Fatalf("ClientFor rejected a valid timeout: %v", err)
	}
}

// TestSetTransportReachesEveryClient proves one swap reaches all of an
// instance's collectors, which is what lets a credential rotation leave every
// poller running and every cache intact.
func TestSetTransportReachesEveryClient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	h := NewHandle("lib1", srv.URL, &http.Client{}, 0, nil)
	a, err := h.ClientFor(time.Second)
	if err != nil {
		t.Fatalf("ClientFor: %v", err)
	}
	b, err := h.ClientFor(2 * time.Second)
	if err != nil {
		t.Fatalf("ClientFor: %v", err)
	}

	if _, err := a.Fetch(context.Background(), "/"); err != nil {
		t.Fatalf("first client before the swap: %v", err)
	}

	old := h.SetTransport(&http.Client{Transport: refusingRoundTripper{}})
	if old == nil {
		t.Fatal("SetTransport did not return the client it replaced; the caller cannot close its idle connections")
	}
	if _, err := a.Fetch(context.Background(), "/"); err == nil {
		t.Fatal("the first client did not follow the swap")
	}
	if _, err := b.Fetch(context.Background(), "/"); err == nil {
		t.Fatal("the second client did not follow the swap")
	}
}

// TestHandleSharesOneLimiter proves an instance's collectors contend for one
// ceiling, which is the whole point of hanging it on the Handle.
func TestHandleSharesOneLimiter(t *testing.T) {
	h := NewHandle("lib1", "https://a.example", &http.Client{}, 1, nil)
	a, _ := h.ClientFor(time.Second)
	b, _ := h.ClientFor(time.Second)
	if a.Limiter() == nil {
		t.Fatal("a Handle built with a ceiling handed out a client with no limiter")
	}
	if a.Limiter() != b.Limiter() {
		t.Fatal("two collectors of one instance got different limiters")
	}

	unlimited := NewHandle("lib2", "https://b.example", &http.Client{}, 0, nil)
	c, _ := unlimited.ClientFor(time.Second)
	if c.Limiter() != nil {
		t.Fatal("a Handle built with ceiling 0 handed out a limiter")
	}
}

// refusingRoundTripper fails every request, so a test can prove which
// transport a Client actually used. Redeclared here rather than shared with
// internal/collector's own copy (see code/http/collector_test.go.tmpl and
// code/http/variants/collector_shared_test.go.tmpl): this package's test
// binary is compiled independently of that one, and a one-line unexported
// type is not worth a shared test-helper package.
type refusingRoundTripper struct{}

func (refusingRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("refused by test transport")
}

// testRegistry builds a Registry whose single factory hands out a fakeBG and
// records every one it made, keyed by the address the Handle carried.
func testRegistry(t *testing.T, root *prometheus.Registry) (*Registry, *[]*fakeBG) {
	t.Helper()
	made := &[]*fakeBG{}
	enabled := true
	factories := []Factory{{
		Name:    "example",
		Enabled: &enabled,
		New: func(h *Handle) (BackgroundCollector, error) {
			bg := newFakeBG("example")
			*made = append(*made, bg)
			return bg, nil
		},
	}}
	return NewRegistry(logger.NewTextLogger("error"), root, "target", factories, 0), made
}

// inst builds the resolved instance list a test reconciles against.
func inst(name, addr string, labels map[string]string, cfg *promconfig.HTTPClientConfig) config.ResolvedInstance {
	return config.ResolvedInstance{Name: name, Address: addr, Labels: labels, ClientConfig: cfg}
}

// seriesFor counts how many series the root registry gathers carrying the given
// value for the identifying label.
func seriesFor(t *testing.T, root *prometheus.Registry, target string) int {
	t.Helper()
	mfs, err := root.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	n := 0
	for _, mf := range mfs {
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "target" && l.GetValue() == target {
					n++
				}
			}
		}
	}
	return n
}

// applyOrFail runs one full reconcile cycle, which is what boot and reload both
// do.
func applyOrFail(t *testing.T, r *Registry, ctx context.Context, instances []config.ResolvedInstance) {
	t.Helper()
	p, err := r.Prepare(instances)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	r.Commit(ctx, p)
}

// TestReconcileKeepsAnUnchangedInstanceUntouched: reconciling the same file
// twice must be a no-op, not a rebuild.
func TestReconcileKeepsAnUnchangedInstanceUntouched(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	root := prometheus.NewRegistry()
	r, made := testRegistry(t, root)

	list := []config.ResolvedInstance{inst("lib1", "https://a.example", nil, nil)}
	applyOrFail(t, r, ctx, list)
	applyOrFail(t, r, ctx, list)

	if len(*made) != 1 {
		t.Fatalf("%d collectors were built; reconciling an unchanged file must build none", len(*made))
	}
	if got := atomic.LoadInt32(&(*made)[0].starts); got != 1 {
		t.Fatalf("the poller was started %d times; an unchanged instance must not be restarted", got)
	}
	if seriesFor(t, root, "lib1") == 0 {
		t.Fatal("the instance's series disappeared after a no-op reconcile")
	}
}

// TestReconcileSwapsTransportWhenOnlyCredentialsChanged: the case that pays for
// this whole design. Rotating a secret written inline must not cost a restart,
// because a restart costs up to one refresh interval of data per instance.
func TestReconcileSwapsTransportWhenOnlyCredentialsChanged(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	root := prometheus.NewRegistry()
	r, made := testRegistry(t, root)

	before := &promconfig.HTTPClientConfig{BasicAuth: &promconfig.BasicAuth{Username: "u", Password: "old"}}
	after := &promconfig.HTTPClientConfig{BasicAuth: &promconfig.BasicAuth{Username: "u", Password: "new"}}

	applyOrFail(t, r, ctx, []config.ResolvedInstance{inst("lib1", "https://a.example", nil, before)})
	first := (*made)[0]

	applyOrFail(t, r, ctx, []config.ResolvedInstance{inst("lib1", "https://a.example", nil, after)})

	if len(*made) != 1 {
		t.Fatalf("%d collectors were built; a credentials-only change must build none", len(*made))
	}
	if got := atomic.LoadInt32(&first.starts); got != 1 {
		t.Fatalf("the poller was started %d times; a credentials-only change must not restart it", got)
	}
	if seriesFor(t, root, "lib1") == 0 {
		t.Fatal("the instance's series disappeared during a credential rotation")
	}

	// The regression this whole test exists to pin: Commit must store the new
	// clientConfig onto the Handle after swapping the transport, or this THIRD
	// Prepare, reconciling the exact content it just swapped to, would compare
	// against the STALE pre-swap config and produce another retransport, and
	// then another on every future reload forever. A third Prepare with
	// unchanged content must therefore see nothing left to do at all.
	p, err := r.Prepare([]config.ResolvedInstance{inst("lib1", "https://a.example", nil, after)})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if len(p.retransport) != 0 || len(p.add) != 0 || len(p.remove) != 0 || len(p.relabel) != 0 {
		t.Fatalf("a third Prepare with unchanged content produced work: %+v; Commit must not have stored the post-swap clientConfig", p)
	}
}

// TestReconcileDoesNotSwapWhenOnlyTheModuleNameChanged: credentials are
// compared by resolved CONTENT, so renaming a module without changing what it
// holds must be a no-op. Without this, every rename would swap a transport for
// nothing.
func TestReconcileDoesNotSwapWhenOnlyTheModuleNameChanged(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	root := prometheus.NewRegistry()
	r, made := testRegistry(t, root)

	cfgA := &promconfig.HTTPClientConfig{BasicAuth: &promconfig.BasicAuth{Username: "u", Password: "p"}}
	cfgB := &promconfig.HTTPClientConfig{BasicAuth: &promconfig.BasicAuth{Username: "u", Password: "p"}}

	applyOrFail(t, r, ctx, []config.ResolvedInstance{inst("lib1", "https://a.example", nil, cfgA)})
	p, err := r.Prepare([]config.ResolvedInstance{inst("lib1", "https://a.example", nil, cfgB)})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if len(p.retransport) != 0 {
		t.Fatal("two identical resolved configs produced a transport swap; comparison must be by content, not by pointer or module name")
	}
	if len(p.add) != 0 || len(p.remove) != 0 || len(p.relabel) != 0 {
		t.Fatalf("an identical configuration produced work: %+v", p)
	}
	_ = made
}

// TestReconcileReregistersWhenOnlyLabelsChanged: labels live in the registerer
// wrapper, applied at Collect time, so the collector's cache knows nothing
// about them and a label change costs a re-registration, not a restart.
func TestReconcileReregistersWhenOnlyLabelsChanged(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	root := prometheus.NewRegistry()
	r, made := testRegistry(t, root)

	applyOrFail(t, r, ctx, []config.ResolvedInstance{
		inst("lib1", "https://a.example", map[string]string{"site": "paris"}, nil)})
	first := (*made)[0]

	applyOrFail(t, r, ctx, []config.ResolvedInstance{
		inst("lib1", "https://a.example", map[string]string{"site": "lyon"}, nil)})

	if len(*made) != 1 {
		t.Fatalf("%d collectors were built; a label-only change must build none", len(*made))
	}
	if got := atomic.LoadInt32(&first.starts); got != 1 {
		t.Fatalf("the poller was started %d times; a label-only change must not restart it", got)
	}

	mfs, err := root.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	sites := map[string]bool{}
	for _, mf := range mfs {
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "site" {
					sites[l.GetValue()] = true
				}
			}
		}
	}
	if sites["paris"] {
		t.Fatal("the old label value is still being served; the previous registration was not removed")
	}
	if !sites["lyon"] {
		t.Fatal("the new label value is not being served")
	}
}

// TestPrepareRejectsALabelKeySetChange is the fix for a confirmed panic:
// client_golang never releases a metric family's label-NAME dimension once
// registered (see Registry.refuseLabelKeyChange's own doc comment), so
// changing which label KEYS an instance's series carry, even through a
// careful unregister-then-register sequence, would panic rather than merely
// error. Prepare must refuse before Commit ever runs, mutate nothing, and
// name the keys that moved.
func TestPrepareRejectsALabelKeySetChange(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	root := prometheus.NewRegistry()
	r, made := testRegistry(t, root)

	applyOrFail(t, r, ctx, []config.ResolvedInstance{
		inst("lib1", "https://a.example", map[string]string{"site": "paris"}, nil)})

	_, err := r.Prepare([]config.ResolvedInstance{
		inst("lib1", "https://a.example", map[string]string{"region": "eu"}, nil)})
	if err == nil {
		t.Fatal("Prepare accepted a reload that changed the instance label KEY SET")
	}
	if !strings.Contains(err.Error(), "site") || !strings.Contains(err.Error(), "region") {
		t.Fatalf("error does not name the keys that moved (want both %q and %q): %v", "site", "region", err)
	}

	if len(*made) != 1 {
		t.Fatalf("%d collectors were built; a refused Prepare must build none", len(*made))
	}
	found := false
	mfs, gatherErr := root.Gather()
	if gatherErr != nil {
		t.Fatalf("gather: %v", gatherErr)
	}
	for _, mf := range mfs {
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "site" && l.GetValue() == "paris" {
					found = true
				}
			}
		}
	}
	if !found {
		t.Fatal("a refused Prepare disturbed the instance that was already running; it must mutate nothing")
	}
}

// TestPrepareAcceptsALabelValueChangeUnderTheSameKeySet pins the other half
// of the fix directly on Prepare's own Plan, right next to the refusal
// above, so the two cases cannot silently collapse into the same refusal: a
// VALUE change under an UNCHANGED key set is not a dimension change at all
// and must keep producing a plain relabel, with no restart required.
// TestReconcileReregistersWhenOnlyLabelsChanged above already proves this
// end to end (including that the poller is not restarted); this test adds
// the narrower, adjacent assertion on Prepare itself.
func TestPrepareAcceptsALabelValueChangeUnderTheSameKeySet(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	root := prometheus.NewRegistry()
	r, _ := testRegistry(t, root)

	applyOrFail(t, r, ctx, []config.ResolvedInstance{
		inst("lib1", "https://a.example", map[string]string{"site": "paris"}, nil)})

	p, err := r.Prepare([]config.ResolvedInstance{
		inst("lib1", "https://a.example", map[string]string{"site": "lyon"}, nil)})
	if err != nil {
		t.Fatalf("Prepare refused a label VALUE change under an unchanged key set: %v", err)
	}
	if len(p.relabel) != 1 {
		t.Fatalf("Plan.relabel has %d entries, want 1", len(p.relabel))
	}
	if len(p.add) != 0 || len(p.remove) != 0 || len(p.retransport) != 0 {
		t.Fatalf("a value-only label change produced more than a relabel: %+v", p)
	}
}

// TestReconcileRebuildsWhenTheAddressChanged: a different address is a
// different machine, so the cache describes something else now and must go.
func TestReconcileRebuildsWhenTheAddressChanged(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	root := prometheus.NewRegistry()
	r, made := testRegistry(t, root)

	applyOrFail(t, r, ctx, []config.ResolvedInstance{inst("lib1", "https://a.example", nil, nil)})
	applyOrFail(t, r, ctx, []config.ResolvedInstance{inst("lib1", "https://b.example", nil, nil)})

	if len(*made) != 2 {
		t.Fatalf("%d collectors were built; a changed address must rebuild the instance", len(*made))
	}
	if got := atomic.LoadInt32(&(*made)[1].starts); got != 1 {
		t.Fatalf("the replacement poller was started %d times, want 1", got)
	}
	if seriesFor(t, root, "lib1") == 0 {
		t.Fatal("the rebuilt instance is not registered")
	}
}

// TestReconcileStartsAndDrainsOnAddAndRemove: the added machine appears, the
// removed one is unregistered immediately (not after its drain) and its poller
// is cancelled.
func TestReconcileStartsAndDrainsOnAddAndRemove(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	root := prometheus.NewRegistry()
	r, made := testRegistry(t, root)

	applyOrFail(t, r, ctx, []config.ResolvedInstance{inst("lib1", "https://a.example", nil, nil)})
	applyOrFail(t, r, ctx, []config.ResolvedInstance{
		inst("lib1", "https://a.example", nil, nil),
		inst("lib2", "https://b.example", nil, nil),
	})
	if seriesFor(t, root, "lib2") == 0 {
		t.Fatal("the added instance is not registered")
	}

	applyOrFail(t, r, ctx, []config.ResolvedInstance{inst("lib1", "https://a.example", nil, nil)})
	if n := seriesFor(t, root, "lib2"); n != 0 {
		t.Fatalf("the removed instance still has %d series; unregistration must be synchronous, not deferred to the drain", n)
	}
	if seriesFor(t, root, "lib1") == 0 {
		t.Fatal("removing one instance unregistered another")
	}

	// The removed machine's poller must have been cancelled.
	removed := (*made)[1]
	select {
	case <-removed.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("the removed instance's poller was never cancelled")
	}
}

// TestReloadOrderRejectsAnInvalidInstanceListBeforeReconciling pins the shape
// of main's reload closure. ResolveInstances is validation and it must run
// first: a list with a duplicate name or an unresolvable module has to be
// refused while the running set is still untouched, not discovered halfway
// through a reconcile.
//
// The closure itself lives in package main and cannot be imported, so this test
// asserts the contract the closure depends on: that a Registry which never saw
// an invalid list is exactly the Registry it was before.
func TestReloadOrderRejectsAnInvalidInstanceListBeforeReconciling(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	root := prometheus.NewRegistry()
	r, made := testRegistry(t, root)

	applyOrFail(t, r, ctx, []config.ResolvedInstance{inst("lib1", "https://a.example", nil, nil)})
	before := seriesFor(t, root, "lib1")

	// What main's closure does on a reload, in order. The validation step is
	// the one that must reject this file.
	cfg := &config.Config{Instances: []config.Instance{
		{Name: "lib1", Address: "https://a.example"},
		{Name: "lib1", Address: "https://b.example"}, // duplicate name
	}}
	if _, err := cfg.ResolveInstances("target", "collector"); err == nil {
		t.Fatal("ResolveInstances accepted a duplicate instance name")
	}

	// Because validation failed, Prepare was never reached and the live set is
	// untouched.
	if seriesFor(t, root, "lib1") != before {
		t.Fatal("the running instance was disturbed by a configuration that never validated")
	}
	if len(*made) != 1 {
		t.Fatalf("%d collectors exist; a rejected configuration must build none", len(*made))
	}
}

// TestPrepareMutatesNothingOnFailure is the atomicity assertion at unit level:
// a factory failing on the third instance must leave the first two untouched.
func TestPrepareMutatesNothingOnFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	root := prometheus.NewRegistry()

	enabled := true
	fail := false
	made := 0
	r := NewRegistry(logger.NewTextLogger("error"), root, "target", []Factory{{
		Name:    "example",
		Enabled: &enabled,
		New: func(h *Handle) (BackgroundCollector, error) {
			if fail && h.Name == "lib3" {
				return nil, errors.New("unreadable CA")
			}
			made++
			return newFakeBG("example"), nil
		},
	}}, 0)

	applyOrFail(t, r, ctx, []config.ResolvedInstance{inst("lib1", "https://a.example", nil, nil)})
	before := seriesFor(t, root, "lib1")

	fail = true
	if _, err := r.Prepare([]config.ResolvedInstance{
		inst("lib1", "https://a.example", nil, nil),
		inst("lib2", "https://b.example", nil, nil),
		inst("lib3", "https://c.example", nil, nil),
	}); err == nil {
		t.Fatal("Prepare succeeded despite a factory that failed")
	}

	if seriesFor(t, root, "lib1") != before {
		t.Fatal("a failed Prepare disturbed the instance that was already running")
	}
	if seriesFor(t, root, "lib2") != 0 {
		t.Fatal("a failed Prepare registered an instance; it must mutate nothing")
	}
}

// timedBG closes its Done channel after a fixed duration measured from
// Start, regardless of context cancellation, standing in for a poller whose
// in-flight request finishes on its own schedule rather than one that is
// cancelled. Used by TestWaitSharesOneBudgetAcrossCollectors and
// TestWaitSharesOneBudgetAcrossInstances: a permanently-stuck collector
// (fakeBG, which exits only on ctx.Done) cannot tell a shared budget apart
// from a per-collector or per-instance one, because Wait returns on the very
// FIRST timeout it hits either way, so a scenario of nothing but stuck
// collectors costs roughly one budget under the correct implementation and
// every broken one alike. What actually tells them apart is a collector (or
// instance) that EVENTUALLY succeeds, late: under a shared clock that
// lateness eats into the SAME budget whatever comes next must also finish
// within; under a clock re-armed per collector or per instance, the late
// success buys whatever comes next a full fresh budget it should never have
// had.
type timedBG struct {
	desc  *prometheus.Desc
	done  chan struct{}
	after time.Duration
}

func newTimedBG(name string, after time.Duration) *timedBG {
	return &timedBG{desc: prometheus.NewDesc("demo_"+name, "fixture", nil, nil), done: make(chan struct{}), after: after}
}

func (f *timedBG) Describe(ch chan<- *prometheus.Desc) { ch <- f.desc }
func (f *timedBG) Collect(ch chan<- prometheus.Metric) {
	ch <- prometheus.MustNewConstMetric(f.desc, prometheus.GaugeValue, 1)
}
func (f *timedBG) Start(context.Context) { go func() { time.Sleep(f.after); close(f.done) }() }
func (f *timedBG) Done() <-chan struct{} { return f.done }

// TestWaitSharesOneBudgetAcrossCollectors proves Wait's shared-budget claim
// for the INNER loop: one instance with two collectors, so h.bgs (a slice,
// built in factory-declaration order) is deterministic. This pins only the
// per-collector half of the N*M*budget claim; see
// TestWaitSharesOneBudgetAcrossInstances below for the per-instance half,
// which this fixture (one instance) cannot exercise: with only one h.bgs
// slice ranged, moving deadline := time.After(budget) into Wait's OUTER loop
// would still create exactly one deadline here, indistinguishable from the
// correct placement before both loops.
//
// The first collector succeeds late, close to the budget; the second never
// exits at all. Under the shared clock (correct), the first collector's
// lateness eats into the one deadline the second collector must also race
// against, so the whole call still finishes in roughly one budget. Under a
// clock re-armed per collector (the mutation this test exists to catch, see
// timedBG's own doc comment for why "everything permanently stuck" cannot
// tell the two apart), the second collector gets a full fresh budget on top
// of however long the first one took, and the call runs measurably longer.
func TestWaitSharesOneBudgetAcrossCollectors(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	root := prometheus.NewRegistry()

	const (
		budget    = 600 * time.Millisecond
		lateAfter = 450 * time.Millisecond // succeeds, but only well into the budget
	)
	enabled := true
	factories := []Factory{
		{Name: "late", Enabled: &enabled, New: func(h *Handle) (BackgroundCollector, error) { return newTimedBG("late", lateAfter), nil }},
		{Name: "stuck", Enabled: &enabled, New: func(h *Handle) (BackgroundCollector, error) { return newFakeBG("stuck"), nil }},
	}
	r := NewRegistry(logger.NewTextLogger("error"), root, "target", factories, 0)
	applyOrFail(t, r, ctx, []config.ResolvedInstance{inst("lib1", "https://a.example", nil, nil)})

	start := time.Now()
	r.Wait(budget)
	elapsed := time.Since(start)

	// Correct: ~1 budget (~600ms: the "late" collector's own 450ms is well
	// inside the one shared deadline, which then fires for "stuck" at the
	// 600ms mark regardless). Broken (budget re-armed per collector): ~1050ms
	// (450ms for "late" to succeed, THEN a full fresh 600ms budget for
	// "stuck"). The threshold sits at the midpoint, 225ms clear on either
	// side: wide enough to absorb scheduling jitter on a loaded CI runner,
	// unlike an earlier version of this test (80ms of headroom against a
	// 200ms budget), which was tight enough to flake on a busy machine.
	const threshold = budget + lateAfter/2
	if elapsed > threshold {
		t.Fatalf("Wait took %v with a %v budget (one collector succeeding at %v, one never exiting); want at most ~%v (one shared budget), not ~%v (a fresh budget per collector, %v + %v)",
			elapsed, budget, lateAfter, threshold, lateAfter+budget, lateAfter, budget)
	}
}

// TestWaitSharesOneBudgetAcrossInstances proves the other half of Wait's
// shared-budget claim: the OUTER loop, over r.handles, must not re-arm the
// deadline per instance either. Two instances, one collector each, so this
// exercises the outer loop the same way
// TestWaitSharesOneBudgetAcrossCollectors exercises the inner one.
//
// r.handles is a map: Wait sorts instance names before ranging it
// specifically so this scenario is deterministic (see Wait's own doc
// comment). The instances are named "inst-a-late" and "inst-b-stuck" so
// sorted order visits the late one first, every run, regardless of Go's
// randomized map iteration: without that, this test would only catch a
// broken (re-armed-per-instance) budget on whichever run happened to visit
// the late instance first, and silently pass the other half of the time,
// exactly the flaw the sorted iteration exists to close.
//
// "inst-a-late" carries one collector that succeeds partway through the
// budget; "inst-b-stuck" carries one that never exits. Under the shared
// clock (correct), finishing "inst-a-late" costs nothing extra: the one
// deadline armed when Wait started is still what "inst-b-stuck" races
// against, so the whole call finishes in roughly one budget. Under a clock
// re-armed per instance (the mutation this test exists to catch), moving
// on to "inst-b-stuck" re-arms a full fresh budget on top of however long
// "inst-a-late" took to finish, and the call runs measurably longer.
func TestWaitSharesOneBudgetAcrossInstances(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	root := prometheus.NewRegistry()

	const (
		budget    = 600 * time.Millisecond
		lateAfter = 400 * time.Millisecond // succeeds well inside the budget
	)
	enabled := true
	factories := []Factory{
		{Name: "mixed", Enabled: &enabled, New: func(h *Handle) (BackgroundCollector, error) {
			if h.Name == "inst-a-late" {
				return newTimedBG("late", lateAfter), nil
			}
			return newFakeBG("stuck"), nil
		}},
	}
	r := NewRegistry(logger.NewTextLogger("error"), root, "target", factories, 0)
	applyOrFail(t, r, ctx, []config.ResolvedInstance{
		inst("inst-a-late", "https://a.example", nil, nil),
		inst("inst-b-stuck", "https://b.example", nil, nil),
	})

	start := time.Now()
	r.Wait(budget)
	elapsed := time.Since(start)

	// Correct: ~1 budget (~600ms: "inst-a-late" finishes at 400ms, well
	// inside the one shared deadline, which then fires for "inst-b-stuck" at
	// the 600ms mark regardless). Broken (budget re-armed per instance):
	// ~1000ms (400ms for "inst-a-late" to finish, THEN a full fresh 600ms
	// budget for "inst-b-stuck"). 200ms of clearance on either side of the
	// midpoint threshold, the same generous margin as
	// TestWaitSharesOneBudgetAcrossCollectors above.
	const threshold = budget + lateAfter/2
	if elapsed > threshold {
		t.Fatalf("Wait took %v with a %v budget (one instance finishing at %v, one never exiting); want at most ~%v (one shared budget), not ~%v (a fresh budget per instance, %v + %v)",
			elapsed, budget, lateAfter, threshold, lateAfter+budget, lateAfter, budget)
	}
}
