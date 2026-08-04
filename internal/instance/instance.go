// Package instance implements the multi-instance target model: one process
// watches a fixed list of machines declared in --config.file, each refreshed by
// its own background poller, all served through a single /metrics that
// Prometheus scrapes as one target.
//
// It mirrors internal/probe's NamedFactory pattern but carries a different
// lifecycle: a /probe factory builds a synchronous collector per request, while
// an instance factory builds a BACKGROUND collector (poller plus cache) once at
// boot, which must be Start()-ed and waited on via Done() at shutdown. Those
// lifecycles do not fit one signature, so the two seams stay separate. See the
// design doc's "Architecture" section.
package instance

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"sort"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	promconfig "github.com/prometheus/common/config"

	"github.com/sckyzo/tapelibrary_exporter/internal/collector"
	"github.com/sckyzo/tapelibrary_exporter/internal/config"
	"github.com/sckyzo/tapelibrary_exporter/internal/logger"
)

// BackgroundCollector is the lifecycle a multi-instance collector must satisfy:
// a prometheus.Collector a background goroutine refreshes. Start launches that
// goroutine bound to ctx; Done closes when it has fully exited, so main can wait
// for it during shutdown. The background collector variant (internal/collector's
// ExampleCollector, code/http/variants/) already satisfies this structurally.
type BackgroundCollector interface {
	prometheus.Collector
	Start(ctx context.Context)
	Done() <-chan struct{}
}

// Handle is everything the process keeps about ONE watched machine: the
// transport its collectors share, the concurrency ceiling they contend for, the
// labels its series carry, and the cancellation that stops its pollers.
//
// It exists because a configuration reload has to be able to change a machine's
// credentials WITHOUT stopping its pollers: restarting them would drop their
// caches, and under this target model a cache is worth up to a full refresh
// interval of data. Sharing one transport per machine also collapses what used
// to be one http.Transport per instance per collector.
//
// Address is immutable on purpose. A changed address is a different machine, so
// the cached data is no longer about the thing it claims to describe: that case
// rebuilds the Handle rather than mutating it.
type Handle struct {
	Name    string
	Address string

	tr          *collector.Transport
	limiter     *collector.Limiter
	queueBudget time.Duration

	// labels are the identifying and extra labels this machine's series
	// carry. Applied by prometheus.WrapRegistererWith at REGISTRATION, with
	// the wrapper adding them at Collect time, so the collectors and their
	// caches know nothing about them: that is what makes a label change
	// cost a re-registration (see Registry.Commit's relabel handling) and
	// not a poller restart.
	labels prometheus.Labels

	// clientConfig is the RESOLVED module content this machine's transport was
	// built from, remembered so a reload can tell a real credential change from
	// a module merely renamed. Compared with reflect.DeepEqual, never by module
	// name: renaming a module without changing what it holds must restart
	// nothing, and editing one under the same name must swap the transport.
	clientConfig *promconfig.HTTPClientConfig

	// tracker is what is registered on the labelled wrapper of the exporter's
	// registry. Kept so a label change can unregister through a wrapper built
	// with the PREVIOUS labels and re-register the same tracker with the new
	// ones, without the collectors ever noticing.
	tracker *collector.StatusTracker

	// session is the RoE login/logout handshake this machine's transport
	// carries, or nil when no credentials are configured. Kept so shutdown
	// and removal can END the session: the library's session table is
	// finite and R1.11.2 documents no idle eviction, so an exporter that
	// exits without logging out leaks one session per instance per restart
	// until the machine refuses to open another. Replaced, and the old one
	// ended, whenever a reload swaps the transport.
	session *collector.SessionTransport

	cancel context.CancelFunc
	bgs    []BackgroundCollector
}

// NewHandle builds a handle for one machine. hc is the client its collectors
// share, built once from the instance's resolved module. limit is
// --exporter.max-requests-per-target; 0 means unlimited.
func NewHandle(name, address string, hc *http.Client, limit int, labels prometheus.Labels, queueBudget time.Duration) *Handle {
	return &Handle{
		Name:        name,
		Address:     address,
		tr:          collector.NewTransport(hc),
		limiter:     collector.NewLimiter(limit),
		labels:      labels,
		queueBudget: queueBudget,
	}
}

// ClientFor returns a Client bound to this machine, carrying one collector's
// own timeout and sharing this machine's transport and ceiling.
//
// A non-positive timeout is refused rather than treated as "no deadline": the
// shared transport carries no http.Client.Timeout of its own, so a collector
// reaching it without one would block its poller forever on a hung connection.
// The error travels out through Factory.New and fails the boot, or fails a
// reload's prepare phase before anything is swapped.
func (h *Handle) ClientFor(timeout time.Duration) (*collector.Client, error) {
	if timeout <= 0 {
		return nil, fmt.Errorf("instance %q: a collector timeout must be positive, got %v (the shared transport carries no timeout of its own)", h.Name, timeout)
	}
	return collector.NewClientOn(h.tr, h.Address, timeout).WithLimiter(h.limiter).WithQueueBudget(h.queueBudget), nil
}

// SetTransport installs a new shared client and returns the one it replaced, so
// the caller can release its idle connections. Every collector of this machine
// sees the change on its next request, without being stopped and without losing
// its cache.
func (h *Handle) SetTransport(hc *http.Client) *http.Client {
	old := h.tr.Get()
	h.tr.Set(hc)
	return old
}

// Factory builds one instance-bound background collector.
//
// It takes the Handle rather than an address and a client config because the
// transport is now built once per machine, in the reconciler, and shared: a
// factory that built its own would give every collector of one machine its own
// connection pool and would put the transport out of a reload's reach.
//
// New may fail: ClientFor refuses a non-positive timeout, and a collector may
// have its own reasons. That failure fails the boot, or fails a reload's
// prepare phase before anything is swapped.
type Factory struct {
	Name    string
	Enabled *bool
	New     func(h *Handle) (BackgroundCollector, error)
}

// Registry owns the live set of watched machines. Boot and reload both go
// through Prepare then Commit, so the reload path is exercised by every start
// and by every golden cell, not only when somebody sends a SIGHUP.
//
// Not safe for concurrent use: every call comes from the reload goroutine,
// which internal/reload serializes, or from main before that goroutine starts.
type Registry struct {
	log           *logger.Logger
	root          prometheus.Registerer
	instanceLabel string
	factories     []Factory // already filtered to the globally-enabled ones
	limit         int       // --exporter.max-requests-per-target

	// queueBudget is --exporter.max-queue-wait: how long a collector may wait
	// for this machine's single request slot before giving up the round. It
	// is separate from any collector's own request timeout, because with a
	// ceiling of 1 a queue is the normal state and a collector's position in
	// it says nothing about how long its own request needs.
	queueBudget time.Duration

	handles map[string]*Handle // live, by instance name

	// labelKeys is the sorted set of extra instance label KEYS this registry
	// has ever registered a series under, and labelKeysKnown reports whether
	// that has happened at least once. Set by Commit, never by Prepare (which
	// must mutate nothing), and STICKY: it survives every instance later being
	// removed, because client_golang's own registry never forgets a metric
	// family's label-name dimension once registered, even after every series
	// of that family is unregistered (see Prepare's refuseLabelKeyChange for
	// the exact mechanism this protects against). "Currently nothing is live"
	// therefore does not mean "any key set is safe again".
	labelKeys      []string
	labelKeysKnown bool
}

// NewRegistry builds an empty registry. root is the exporter's own registry;
// each instance's collectors are registered on a wrapper of it carrying that
// instance's labels.
func NewRegistry(log *logger.Logger, root prometheus.Registerer, instanceLabel string, factories []Factory, limit int, queueBudget time.Duration) *Registry {
	return &Registry{
		log:           log,
		root:          root,
		instanceLabel: instanceLabel,
		factories:     factories,
		limit:         limit,
		queueBudget:   queueBudget,
		handles:       make(map[string]*Handle),
	}
}

// Plan is what Prepare produced and Commit will apply.
//
// Everything in it is already BUILT: the transports exist, so the step that
// fails on an unreadable CA or secret file has already run. That is what makes
// Commit unable to fail, and it is what makes a reload atomic: a bad CA on the
// third instance is refused before the first one has been touched.
type Plan struct {
	add         []*addOp
	remove      []*Handle
	relabel     []*relabelOp
	retransport []*retransportOp
}

// addOp is a machine to start: a handle whose collectors are built but not yet
// started or registered.
type addOp struct {
	handle     *Handle
	tracker    *collector.StatusTracker
	collectors []BackgroundCollector
}

// relabelOp is the same machine under new labels. The poller is not touched:
// WrapRegistererWith applies its ConstLabels inside the wrapper at Collect
// time, so the collector's cached []prometheus.Metric carries bare Descs and
// knows nothing about them. Only the registration moves.
type relabelOp struct {
	handle    *Handle
	newLabels prometheus.Labels
}

// retransportOp is the same machine with new credentials. The poller is not
// touched either: its Clients share the Handle's Transport, so replacing the
// *http.Client underneath them is enough, and their caches survive.
//
// clientConfig travels with the new client so Commit can record what the
// transport was built from. Without it the next reload compares against the
// stale config and swaps the transport on every reload forever.
type retransportOp struct {
	handle       *Handle
	client       *http.Client
	session      *collector.SessionTransport
	clientConfig *promconfig.HTTPClientConfig
}

// labelsFor builds the label set an instance's series carry: the identifying
// label fixed at scaffold time, plus the instance's own extra labels. Built
// here rather than in main so boot and reload cannot disagree about it.
func (r *Registry) labelsFor(inst config.ResolvedInstance) prometheus.Labels {
	labels := prometheus.Labels{r.instanceLabel: inst.Name}
	for k, v := range inst.Labels {
		labels[k] = v
	}
	return labels
}

// Prepare classifies every instance in the new configuration against the live
// set and builds everything the resulting Plan will need. This is the phase
// that can fail, and it mutates nothing: on error the caller keeps running
// exactly what it was running.
//
// Boot is Prepare against an empty live set, which is why this code path is
// exercised by every start rather than only by a reload.
//
// Instances are processed in the order the file declares them, so a file with
// two broken instances always fails on the same one.
//
// Before any of that, refuseLabelKeyChange gates the whole reload: a changed
// instance label KEY SET is refused outright, for every instance at once, not
// classified per instance. See that method's own doc comment for why.
func (r *Registry) Prepare(instances []config.ResolvedInstance) (*Plan, error) {
	if err := r.refuseLabelKeyChange(instances); err != nil {
		return nil, err
	}

	p := &Plan{}
	seen := make(map[string]bool, len(instances))

	for _, inst := range instances {
		seen[inst.Name] = true
		labels := r.labelsFor(inst)
		cur, live := r.handles[inst.Name]

		// Not watched yet: build it whole. Every collector is constructed here,
		// so a factory that fails (a non-positive timeout, or a reason of its
		// own) fails the whole reload before anything has been started.
		if !live || cur.Address != inst.Address {
			hc, session, err := clientFor(inst.ClientConfig, inst.Address)
			if err != nil {
				return nil, fmt.Errorf("instance %q: %w", inst.Name, err)
			}
			h := NewHandle(inst.Name, inst.Address, hc, r.limit, labels, r.queueBudget)
			h.clientConfig = inst.ClientConfig
			h.session = session

			tracker := collector.NewStatusTracker(r.log)
			var bgs []BackgroundCollector
			for _, f := range r.factories {
				bg, err := f.New(h)
				if err != nil {
					return nil, fmt.Errorf("instance %q, collector %q: %w", inst.Name, f.Name, err)
				}
				tracker.Add(f.Name, bg)
				bgs = append(bgs, bg)
			}
			p.add = append(p.add, &addOp{handle: h, tracker: tracker, collectors: bgs})

			// A changed ADDRESS is a different machine: the cache describes
			// something else now, so the old handle is removed alongside.
			if live {
				p.remove = append(p.remove, cur)
			}
			continue
		}

		// Same machine, still at the same address. Two things may have moved,
		// and each is cheap on its own: neither stops a poller or drops a cache.
		if !reflect.DeepEqual(cur.labels, labels) {
			p.relabel = append(p.relabel, &relabelOp{handle: cur, newLabels: labels})
		}
		if !reflect.DeepEqual(cur.clientConfig, inst.ClientConfig) {
			hc, session, err := clientFor(inst.ClientConfig, inst.Address)
			if err != nil {
				return nil, fmt.Errorf("instance %q: %w", inst.Name, err)
			}
			p.retransport = append(p.retransport, &retransportOp{
				handle:       cur,
				client:       hc,
				session:      session,
				clientConfig: inst.ClientConfig,
			})
		}
	}

	// Anything the file no longer declares. Sorted so a reload removing several
	// instances logs them in the same order every time.
	var gone []string
	for name := range r.handles {
		if !seen[name] {
			gone = append(gone, name)
		}
	}
	sort.Strings(gone)
	for _, name := range gone {
		p.remove = append(p.remove, r.handles[name])
	}

	return p, nil
}

// refuseLabelKeyChange is the fix for a confirmed panic: client_golang's
// Unregister deliberately leaves a metric family's label-NAME dimension
// recorded once it has been registered, "as those must be consistent
// throughout the lifetime of a program" (see
// prometheus/registry.go's own comment on dimHashesByName). So once this
// exporter has registered an instance's StatusTracker under a given set of
// label KEYS, re-registering the same metric family under a DIFFERENT set of
// label keys panics, even through Commit's careful unregister-then-register
// sequence. A label VALUE change is unaffected: the key set is unchanged, so
// the dimension is unchanged, and that case keeps reloading with no restart.
//
// Five ways the key set can move between two otherwise-valid files were
// confirmed: adding a label, removing one, renaming a key across every
// instance, replacing a labelled instance with an unlabelled one, and an
// address-change rebuild landing on a new key set too. ResolveInstances
// already guarantees every instance WITHIN one file declares the same key
// set (see its own doc comment), so any single instance represents the whole
// file; it has no way to see what was live under a PREVIOUS file, which is
// what this check adds.
//
// The ruling is to refuse rather than redesign: a per-instance child
// registry was considered and declined for this release. Refusing here, in
// Prepare, is what keeps the promise that this phase is the one allowed to
// fail and that a refusal mutates nothing: no handle is touched, no plan is
// built, the caller keeps running exactly what it was running, and the
// operator is told in plain language that this particular edit needs a
// restart, not a signal.
func (r *Registry) refuseLabelKeyChange(instances []config.ResolvedInstance) error {
	if !r.labelKeysKnown || len(instances) == 0 {
		return nil
	}
	newKeys := sortedExtraKeys(instances[0].Labels, "")
	if reflect.DeepEqual(newKeys, r.labelKeys) {
		return nil
	}
	added, removed := diffKeys(r.labelKeys, newKeys)
	return fmt.Errorf(
		"instance label keys changed from %v to %v (added: %v, removed: %v): this requires a restart of the process, not a reload; a Prometheus registry never releases a metric family's label-name dimension once registered, so re-registering it under a different set of label keys would panic",
		r.labelKeys, newKeys, added, removed,
	)
}

// sortedExtraKeys returns the sorted set of keys in labels, excluding
// exclude. Shared by both sides of refuseLabelKeyChange's comparison:
// instances[0].Labels (config.ResolvedInstance's extra labels only, so
// exclude is "") and a live Handle's full labels (which also carries the
// identifying label, so exclude is the instance label name).
func sortedExtraKeys(labels map[string]string, exclude string) []string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		if k == exclude {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// diffKeys reports which keys were added and removed going from live to new.
// Both inputs are already sorted, so the outputs are too. Used only to build
// a clear error message naming what moved; a renamed key surfaces as one
// added key and one removed key, which is an accurate description of what a
// Prometheus registry actually sees.
func diffKeys(live, newKeys []string) (added, removed []string) {
	liveSet := make(map[string]bool, len(live))
	for _, k := range live {
		liveSet[k] = true
	}
	newSet := make(map[string]bool, len(newKeys))
	for _, k := range newKeys {
		newSet[k] = true
	}
	for _, k := range newKeys {
		if !liveSet[k] {
			added = append(added, k)
		}
	}
	for _, k := range live {
		if !newSet[k] {
			removed = append(removed, k)
		}
	}
	return added, removed
}

// clientFor builds the shared *http.Client for one instance. A nil resolved
// config means the default transport, exactly as it did before this package
// owned the construction.
// It also installs the RoE session handshake, which is what makes this
// exporter able to authenticate against real hardware: the TS4500 accepts no
// HTTP authentication scheme at all, so basic_auth's credentials are POSTed
// to /v1/login and the resulting cookie rides every request. See
// collector.SessionTransport, which documents why that is a transport
// concern rather than a configuration one.
//
// BasicAuth is deliberately STRIPPED from the copy handed to NewHTTPClient.
// The credentials are consumed by the login handshake instead, and leaving
// the block in place would additionally send an Authorization header to a
// server that ignores it — credentials on the wire for no purpose. The copy
// is shallow, which is enough: only the BasicAuth pointer is cleared, and
// the caller's config is never mutated.
//
// With no basic_auth configured there is no session and no wrapper: the
// transport is exactly what it was before this existed. That keeps every
// test in this package, and any deployment against a target that needs no
// authentication, on the path they already ran.
func clientFor(hcfg *promconfig.HTTPClientConfig, address string) (*http.Client, *collector.SessionTransport, error) {
	if hcfg == nil {
		return &http.Client{}, nil, nil
	}

	user, password, err := collector.SessionCredentials(hcfg)
	if err != nil {
		return nil, nil, err
	}

	cfg := *hcfg
	if user != "" {
		cfg.BasicAuth = nil
	}

	// The per-request deadline lives on each collector's Client, not here: this
	// transport is shared by collectors whose timeouts differ.
	hc, err := collector.NewHTTPClient(cfg, 0)
	if err != nil {
		return nil, nil, err
	}
	if user == "" {
		return hc, nil, nil
	}

	session := collector.NewSessionTransport(hc.Transport, address, user, password)
	hc.Transport = session
	return hc, session, nil
}

// Commit applies a prepared plan. It cannot fail: every construction that
// could has already happened in Prepare, the names it registers were proved
// unique by ResolveInstances, Prepare has already refused any plan whose
// instance label KEY SET differs from what is already live (see
// refuseLabelKeyChange), and starting a goroutine does not fail.
//
// What reloads with no restart, as a direct result: an instance's address,
// its credentials, which instances exist, and the VALUES of its labels. What
// does not, and is refused earlier, in Prepare, rather than discovered here
// as a panic: the SET of label KEYS an instance's series carry. A Prometheus
// registry never forgets a metric family's label-name dimension once it has
// registered a series under it, even after every series of that family is
// unregistered, so MustRegister below can never hit that refusal: by the
// time a Plan reaches Commit, every registration it will perform is already
// known to keep every metric family's dimension exactly as it was.
//
// Order matters. Removals unregister FIRST, so a removed instance's series are
// gone from the very next scrape rather than lingering for the length of a
// drain. Draining is then handed to a background goroutine, so a reload never
// blocks behind a poller stuck in a long request.
func (r *Registry) Commit(ctx context.Context, p *Plan) {
	for _, h := range p.remove {
		prometheus.WrapRegistererWith(h.labels, r.root).Unregister(h.tracker)
		delete(r.handles, h.Name)
		h.cancel()
		r.log.Info("Stopped watching instance", "instance", h.Name, "address", h.Address)
		go h.drain(5*time.Second, r.log)
	}

	for _, op := range p.relabel {
		// Unregister through a wrapper built with the PREVIOUS labels: that is
		// how the registry identifies what to remove, and it is why the handle
		// remembers them. The tracker and every collector behind it are
		// untouched, so no cache is lost.
		prometheus.WrapRegistererWith(op.handle.labels, r.root).Unregister(op.handle.tracker)
		op.handle.labels = op.newLabels
		prometheus.WrapRegistererWith(op.handle.labels, r.root).MustRegister(op.handle.tracker)
		r.log.Info("Relabelled instance", "instance", op.handle.Name)
	}

	for _, op := range p.retransport {
		old := op.handle.SetTransport(op.client)
		oldSession := op.handle.session
		op.handle.session = op.session
		// Store what the new transport was built FROM, or the next reload will
		// compare against the stale config and swap the transport again on
		// every single reload.
		op.handle.clientConfig = op.clientConfig
		// End the session the REPLACED credentials opened, on its own
		// goroutine so a reload never blocks on a machine that has gone
		// away. Skipping this would leak one session per credential
		// rotation, and rotating credentials is exactly when the old account
		// may already have been disabled on the library.
		if oldSession != nil {
			// context.Background, deliberately, NOT Commit's ctx: that one is
			// scoped to the reload and is cancelled as soon as Commit
			// returns, which would abort the logout it was started to
			// perform. endSession applies its own bounded budget instead, so
			// this is a goroutine with a deadline, not one without.
			go endSession(oldSession, op.handle, r.log) //nolint:gosec // G118: the logout must outlive the reload that triggered it; endSession bounds it
		}
		if old != nil {
			// Only IDLE connections close, so a request in flight on the old
			// client finishes undisturbed. Without this the old transport holds
			// its sockets until IdleConnTimeout.
			old.CloseIdleConnections()
		}
		r.log.Info("Rotated credentials for instance", "instance", op.handle.Name)
	}

	for _, op := range p.add {
		instCtx, cancel := context.WithCancel(ctx)
		op.handle.cancel = cancel
		op.handle.bgs = op.collectors
		op.handle.tracker = op.tracker
		for _, bg := range op.collectors {
			bg.Start(instCtx)
		}
		prometheus.WrapRegistererWith(op.handle.labels, r.root).MustRegister(op.tracker)
		r.handles[op.handle.Name] = op.handle
		r.log.Info("Watching instance", "instance", op.handle.Name, "address", op.handle.Address)
	}

	// Record the key set now live, for refuseLabelKeyChange's next comparison.
	// Deliberately sticky: only written when there is a live handle to read it
	// from, never cleared, so a reload that removes every instance leaves the
	// last known key set in place rather than forgetting it. That mirrors the
	// underlying registry, which does the same forever, not just until the
	// last series of a family happens to be unregistered.
	for _, h := range r.handles {
		r.labelKeys = sortedExtraKeys(h.labels, r.instanceLabel)
		r.labelKeysKnown = true
		break
	}
}

// drain waits for one removed instance's pollers to exit, under a bounded
// budget, and warns if they do not. Called on its own goroutine so a reload
// returns immediately: the instance is already unregistered, so a lingering
// poller is unreferenced and harmless.
func (h *Handle) drain(budget time.Duration, log *logger.Logger) {
	deadline := time.After(budget)
	for _, bg := range h.bgs {
		select {
		case <-bg.Done():
		case <-deadline:
			log.Warn("a removed instance's background collectors did not all stop within the drain budget", "instance", h.Name, "address", h.Address)
			// Log out anyway. A poller that overran its budget is
			// unreferenced, not holding the session, and leaving the session
			// open is the more expensive of the two failures.
			endSession(h.session, h, log)
			return
		}
	}
	// Only once the pollers have stopped: logging out from under a refresh
	// that is still in flight would fail that refresh for no reason.
	endSession(h.session, h, log)
}

// endSession ends one machine's RoE session, bounded so a machine that has
// gone away cannot hold up a shutdown or a reload. A nil session (no
// credentials configured) is a no-op, as is a session that never logged in.
//
// Failure is logged, never returned: by the time this runs the caller is
// already shutting the instance down and has nothing left to do about it.
// The log line matters anyway — a session this exporter failed to end is one
// the library holds until it decides otherwise, and a run of these is what
// explains a later "too many sessions" refusal.
func endSession(s *collector.SessionTransport, h *Handle, log *logger.Logger) {
	if s == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), sessionLogoutBudget)
	defer cancel()
	if err := s.Logout(ctx); err != nil {
		log.Warn("could not end the session for an instance; the library will hold it until it expires on its own",
			"instance", h.Name, "address", h.Address, "err", err)
	}
}

// sessionLogoutBudget bounds each logout. Short on purpose: it runs during
// shutdown, after the HTTP server has already stopped, so every second here
// is a second the process takes to exit. One round trip to a machine that is
// answering takes milliseconds; a machine that is not answering is exactly
// the case this must not wait for.
const sessionLogoutBudget = 3 * time.Second

// Wait blocks until every live instance's pollers have exited, under ONE shared
// budget rather than one per instance or one per collector: with N instances by
// M collectors, a per-collector (or per-instance) wait would worst-case at
// N*M*budget (or N*budget). deadline is created exactly once, before either
// loop below, and reused by every select in both of them, which is what makes
// that budget shared rather than reset partway through. Called by main at
// shutdown, after the HTTP server has stopped.
//
// Iterates r.handles in sorted-by-name order rather than raw map order. Wait's
// own correctness never depended on visitation order (draining one instance
// before another is not itself observable), but a stable order is what lets
// TestWaitSharesOneBudgetAcrossInstances pin the outer loop deterministically,
// by instance name, instead of at the mercy of Go's randomized map iteration,
// which would make a real regression here (the deadline re-armed per instance)
// pass or fail depending on which instance the runtime happened to visit
// first.
func (r *Registry) Wait(budget time.Duration) {
	deadline := time.After(budget)
	names := make([]string, 0, len(r.handles))
	for name := range r.handles {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		for _, bg := range r.handles[name].bgs {
			select {
			case <-bg.Done():
			case <-deadline:
				r.log.Warn("background collectors did not all stop within the shutdown budget; exiting anyway")
				// Still end every session before returning. The pollers are
				// unreferenced at this point and a leaked session outlives
				// the process, so this is the one piece of shutdown work
				// worth doing even after the budget is spent.
				r.endSessions()
				return
			}
		}
	}
	r.endSessions()
}

// endSessions logs every live instance out, concurrently and under one
// bounded budget each, so five libraries cost one logout's latency rather
// than five. Called by Wait once the pollers have stopped, which is the last
// thing this process does with the network.
//
// Concurrent rather than sequential specifically because of the failure
// case: a machine that has gone away burns the full sessionLogoutBudget, and
// doing that five times in series would add fifteen seconds to a shutdown
// for sessions that are already lost.
func (r *Registry) endSessions() {
	var wg sync.WaitGroup
	for _, h := range r.handles {
		if h.session == nil {
			continue
		}
		wg.Add(1)
		go func(h *Handle) {
			defer wg.Done()
			endSession(h.session, h, r.log)
		}(h)
	}
	wg.Wait()
}
