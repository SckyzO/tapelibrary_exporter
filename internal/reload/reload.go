// Package reload implements configuration reload for the target models driven
// by a configuration file: SIGHUP always, and POST /-/reload behind
// --web.enable-lifecycle.
//
// It owns the MECHANISM and nothing else. What a new configuration means, and
// what has to change in the running process to adopt it, is the caller's
// business: main.go passes an apply function, which is where the per-target-model
// policy lives. That split is what keeps this package identical between the
// multi and multi-instance scaffolds.
//
// Three properties, each testable on its own:
//
//   - Serialization. One goroutine consumes every request, so a SIGHUP arriving
//     during a POST waits its turn and the HTTP handler receives the error of
//     its OWN reload rather than someone else's.
//   - Prepare then commit. Everything that can fail (re-read, parse, compare
//     flags:, validate, build transports) runs before anything is mutated. A
//     caller's apply must respect the same split, or a bad CA on the third
//     instance leaves the process half reconfigured.
//   - Fail closed. Any error leaves the running configuration untouched, drives
//     the successful gauge to 0, and logs at Error. The success timestamp only
//     ever advances on a completed commit.
package reload

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/sckyzo/tapelibrary_exporter/internal/config"
	"github.com/sckyzo/tapelibrary_exporter/internal/logger"
)

// Reloader re-reads one configuration file on demand and hands the result to
// apply. Build it with New and start its consumer with Run.
type Reloader struct {
	log   *logger.Logger
	path  string
	boot  *config.Config
	apply func(*config.Config) error

	// requests carries one channel per caller, which is how a caller gets the
	// error of the reload IT asked for. Prometheus itself uses this shape for
	// the same problem.
	requests chan chan error

	// inline is the one mutual-exclusion primitive that guards every call to
	// reloadOnce, whichever of the three sites triggers it: Run's SIGHUP case,
	// Run's channel-request case, and Reload's own direct fallback. Run's
	// single consuming goroutine is still what gives a channel-delivered
	// request its place in line and lets its caller learn the outcome of the
	// SPECIFIC reload it asked for, and it is uncontended there in the common
	// case; but Run is not sitting at its top-level select for the whole
	// reloadOnce call it is currently running, so a second reload arriving
	// during that window (or before Run's very first loop iteration) finds no
	// receiver and takes Reload's fallback. Without this mutex ALSO covering
	// Run's own two call sites, that fallback would run concurrently with the
	// reloadOnce Run is already in the middle of: the one thing this whole
	// package exists to prevent. Taken in unit tests that never start Run too,
	// which is where the name comes from.
	inline sync.Mutex

	successful  prometheus.Gauge
	successTime prometheus.Gauge
}

// New builds a Reloader for path. boot is the configuration the process
// started with, kept so a later reload can tell whether the "flags:" section
// moved. apply adopts a new configuration and must itself be split into a
// phase that can fail and mutates nothing, and a phase that mutates and cannot.
// apply must never call Reload itself, directly or through anything it calls:
// reloadOnce already holds inline for the whole call, and inline is not
// reentrant, so a reload triggering another reload from inside apply would
// deadlock against itself.
func New(log *logger.Logger, path string, boot *config.Config, apply func(*config.Config) error) *Reloader {
	return &Reloader{
		log:      log,
		path:     path,
		boot:     boot,
		apply:    apply,
		requests: make(chan chan error),
		successful: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "tapelibrary_exporter_config_last_reload_successful",
			Help: "Whether the last configuration reload attempt succeeded (1) or failed (0).",
		}),
		successTime: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "tapelibrary_exporter_config_last_reload_success_timestamp_seconds",
			Help: "Unix time of the last SUCCESSFUL configuration reload.",
		}),
	}
}

// Collectors returns the metrics this package owns, for main.go to register on
// the exporter's own registry. Returned rather than self-registered: main.go
// builds a custom registry precisely to avoid package-level global state.
func (r *Reloader) Collectors() []prometheus.Collector {
	return []prometheus.Collector{r.successful, r.successTime}
}

// Run consumes reload requests, one at a time, until ctx is done. It also
// installs the SIGHUP handler.
//
// SIGHUP gets its OWN signal.Notify channel and never touches the
// signal.NotifyContext main.go uses for SIGTERM and SIGINT: routing it there
// would cancel the process context and turn a reload into a shutdown.
//
// Run returning is not the same moment as the process actually exiting: main
// still drains in-flight HTTP requests and background collectors for up to
// several more seconds afterward. A SIGHUP landing in that window must be a
// silent no-op, not fatal, which is why cleanup below ends with
// signal.Ignore rather than signal.Stop: Stop only detaches this package's
// own channel and leaves SIGHUP's default disposition in charge again, which
// on most platforms terminates the process outright, aborting that drain
// (confirmed: exit code 129, mid-shutdown). Ignore instead leaves SIGHUP
// permanently discarded from here on, which is exactly the right end state
// for a signal whose only purpose was reloading a configuration that no
// longer has a consumer.
func (r *Reloader) Run(ctx context.Context) {
	// Mark the process as "configuration currently good" before serving, so
	// the gauge is meaningful from the first scrape rather than absent until
	// somebody reloads.
	r.successful.Set(1)

	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Ignore(syscall.SIGHUP)

	for {
		select {
		case <-ctx.Done():
			return
		case <-hup:
			r.log.Info("SIGHUP received, reloading configuration", "path", r.path)
			if err := r.reloadOnceLocked(); err != nil {
				r.log.Error("Configuration reload failed, keeping the running configuration", "err", err)
			} else {
				r.log.Info("Configuration reloaded")
			}
		case reply := <-r.requests:
			err := r.reloadOnceLocked()
			if err != nil {
				r.log.Error("Configuration reload failed, keeping the running configuration", "err", err)
			} else {
				r.log.Info("Configuration reloaded")
			}
			reply <- err
		}
	}
}

// Reload performs one reload and returns its outcome. It is what Handler calls
// and what a test drives directly.
//
// When Run is consuming, the work happens on Run's goroutine, which is what
// keeps it serialized with SIGHUP and lets each caller receive the error of the
// reload IT asked for. When Run is not consuming, whether because it was never
// started (a unit test) or because it is already busy running someone else's
// reload, the send finds no receiver and Reload runs reloadOnce itself,
// through the same reloadOnceLocked wrapper Run's own two cases use, so this
// fallback can never overlap with a reload Run is concurrently running.
func (r *Reloader) Reload() error {
	reply := make(chan error, 1)
	select {
	case r.requests <- reply:
		return <-reply
	default:
		return r.reloadOnceLocked()
	}
}

// reloadOnceLocked serializes every call to reloadOnce behind inline: Run's
// SIGHUP case, Run's channel-request case, and Reload's own fallback all go
// through here, which is what makes inline the one mutual-exclusion domain
// reloadOnce ever runs under, regardless of which of those three call sites
// triggered it.
func (r *Reloader) reloadOnceLocked() error {
	r.inline.Lock()
	defer r.inline.Unlock()
	return r.reloadOnce()
}

// reloadOnce is the prepare-then-commit body. Everything above the apply call
// can fail and mutates nothing.
func (r *Reloader) reloadOnce() error {
	if r.path == "" {
		// No --config.file is the documented allow-any starting posture on
		// this target model (see docs/configuration.md), not a broken state:
		// an exporter running that way has nothing on disk to re-read, so a
		// SIGHUP or POST /-/reload arriving here is a NO-OP SUCCESS, not a
		// failure. Calling fail() here would drive the successful gauge to
		// 0 and, since nothing ever runs a second reload to clear it, pin
		// ConfigReloadFailed on a perfectly healthy exporter forever. Both
		// triggers get the same answer on purpose: an operator who sends an
		// explicit POST for a build that was never given a file deserves
		// "there was nothing to reload", not a 500 that reads as this
		// package being broken.
		r.log.Info("no configuration file to reload: this exporter was started without --config.file")
		return nil
	}

	next, err := config.Load(r.path) // re-read, UnmarshalStrict, resolve relative paths
	if err != nil {
		return r.fail(err)
	}

	// "flags:" is applied once, at startup, by rendering it into arguments for
	// kingpin. A running process cannot adopt a new value for one, so a changed
	// section refuses the WHOLE reload rather than applying the rest and leaving
	// the process describing neither file.
	if changed := config.DiffFlags(r.boot, next); len(changed) > 0 {
		return r.fail(fmt.Errorf(
			"flags section changed (%s); these are applied once at startup, restart to apply them",
			strings.Join(changed, ", ")))
	}

	if err := r.apply(next); err != nil {
		return r.fail(err)
	}

	r.successful.Set(1)
	r.successTime.Set(float64(time.Now().Unix()))
	return nil
}

// fail drives the gauge and returns the error, so every failure path is one
// line and none can forget the metric.
func (r *Reloader) fail(err error) error {
	r.successful.Set(0)
	return fmt.Errorf("reload refused: %w", err)
}

// Handler serves POST /-/reload. Gate it behind --web.enable-lifecycle: a
// mutating endpoint on an exporter that is unauthenticated by default is the
// reason for that flag. --web.config.file covers this route once it is set,
// but it is not set by default, and SIGHUP already requires being on the
// machine.
//
// main.go must register this handler UNCONDITIONALLY and refuse inside it
// when the flag is unset (http.NotFound, then return, before calling this
// Handler), rather than only calling http.Handle when the flag is set.
// Exporter main.go files register a catch-all "/" landing-page handler, and
// Go's http.ServeMux treats a bare "/" pattern as a subtree match for any
// path with no more specific registration: leaving /-/reload entirely
// unregistered does not make it 404, it makes it fall through to that
// catch-all and answer 200 with the landing page. Checking the flag inside
// the handler is what actually gives an exporter that did not opt in a real
// 404 on this path.
func (r *Reloader) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "use POST to reload the configuration", http.StatusMethodNotAllowed)
			return
		}
		if err := r.Reload(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		if r.path == "" {
			// Same 200, same no-restart contract as a real reload (see
			// reloadOnce's own comment on why this is a no-op SUCCESS, not a
			// failure), but a distinct body: an operator who edited a file
			// and forgot to pass --config.file must not read "reload
			// succeeded" and believe their edit is live.
			_, _ = w.Write([]byte("no configuration file to reload: this exporter was started without --config.file\n"))
			return
		}
		_, _ = w.Write([]byte("reload succeeded\n"))
	})
}
