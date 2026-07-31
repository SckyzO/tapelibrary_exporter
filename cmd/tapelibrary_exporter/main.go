package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/alecthomas/kingpin/v2"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/common/version"
	"github.com/prometheus/exporter-toolkit/web"
	webflag "github.com/prometheus/exporter-toolkit/web/kingpinflag"

	"github.com/sckyzo/tapelibrary_exporter/internal/collector"
	"github.com/sckyzo/tapelibrary_exporter/internal/config"
	"github.com/sckyzo/tapelibrary_exporter/internal/instance"
	"github.com/sckyzo/tapelibrary_exporter/internal/logger"
	"github.com/sckyzo/tapelibrary_exporter/internal/reload"
)

// namespace is this exporter's Prometheus metric prefix (used by the landing
// page and startup log). See internal/collector for how tapelibrary is
// substituted into each collector's own metric names.
const namespace = "tapelibrary"

// instanceLabel is the identifying label this exporter applies to every series
// of every watched machine (see WrapRegistererWith below). It is fixed at
// scaffold time by scaffold.sh --instance-label (default "target"), never a
// runtime knob: a config edit that renamed every series at once would make
// docs/metrics.md unverifiable by make docs-check.
const instanceLabel = "library"

var (
	logLevel     = kingpin.Flag("log.level", "Only log messages with the given severity or above. One of: [debug, info, warn, error]").Default("info").Enum("debug", "info", "warn", "error")
	logFormat    = kingpin.Flag("log.format", "Log format. One of: [json, text]").Default("text").Enum("json", "text")
	toolkitFlags = webflag.AddFlags(kingpin.CommandLine, ":9170")

	// disableExporterMetrics removes Go runtime and process metrics from /metrics.
	disableExporterMetrics = kingpin.Flag(
		"web.disable-exporter-metrics",
		"Exclude Go runtime and process metrics from the /metrics endpoint.",
	).Default("false").Bool()

	// enableLifecycle exposes POST /-/reload. Default false, which is
	// Prometheus's own posture for a mutating endpoint: a scaffolded
	// multi-instance exporter is unauthenticated by default, so shipping every
	// generated exporter an unauthenticated MUTATING endpoint would be the one
	// change here that degrades the default posture of an operator who
	// configured nothing. --web.config.file covers the route once it is set,
	// but it is not set by default. SIGHUP is always on: it already requires
	// being on the machine.
	enableLifecycle = kingpin.Flag(
		"web.enable-lifecycle",
		"Expose POST /-/reload, which reloads --config.file. SIGHUP always works and needs no flag.",
	).Default("false").Bool()

	// maxRequestsPerTarget bounds how many requests this exporter has in
	// flight per WATCHED INSTANCE, not per physical address, despite its flag
	// name (kept identical to the single target model's own
	// --exporter.max-requests-per-target, which really is per-address there;
	// see mains/single/main.go.tmpl). On this target model the enforcement
	// boundary is instance.Handle, one per --config.file entry: instance.NewHandle
	// gives each Handle its own Limiter, so two instances that happen to
	// share one physical address are bounded INDEPENDENTLY, not together,
	// and this exporter's aggregate concurrency against that one machine can
	// exceed the configured value. This is a deliberate Task 8 design choice
	// (a per-address index would need to be pre-populated to cover an
	// instance a reload adds later, or rebuilt on every reload; per-Handle
	// does neither), not an oversight, but it means this flag is not a hard
	// cap on a physical machine watched under two different instance names.
	// The help text below says so; see also the flag doc comment in
	// mains/single/main.go.tmpl, where the same name means the stricter,
	// per-address thing.
	//
	// 0, the default, means unlimited, which is the same posture
	// --probe.target-allowlist takes with its empty value: this is opt-in
	// hardening, not a default that moves under an operator who configured
	// nothing.
	//
	// A ceiling turns a slow collector into a source of starvation for its
	// siblings, so it is a deliberate choice with a visible cost. The wait is
	// charged against each collector's own timeout, and
	// tapelibrary_exporter_request_wait_seconds is what makes queueing
	// visible rather than silent.
	maxRequestsPerTarget = kingpin.Flag(
		"exporter.max-requests-per-target",
		"Maximum concurrent requests this exporter issues per watched instance. 0 (default) means unlimited. Two instances sharing one physical address are bounded independently, not together.",
	).Default("0").Int()

	// configFile is REQUIRED for this target model: without it there are no
	// instances to watch. Its value is read straight from os.Args below, before
	// parsing, because it decides which arguments the parser is given.
	configFile = kingpin.Flag(
		"config.file",
		"Path to the YAML configuration file listing the instances to watch (required). Unrelated to --web.config.file, which configures the TLS server this exporter exposes.",
	).Default("").String()
)

// indexHTML is the landing page served at /.
var indexHTML = fmt.Sprintf(`<html>
	<head><title>%s Exporter</title></head>
	<body>
		<h1>%s Exporter (multi-instance)</h1>
		<p>This exporter watches a fixed list of instances from its configuration file and serves them all through <a href='/metrics'>/metrics</a>.</p>
	</body>
</html>`, namespace, namespace)

func main() {
	var log *logger.Logger

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	// factories holds one entry per collector, in declaration order. scaffold.sh
	// injects the starter at the marker below, and /add-collector appends every
	// collector added later. Leave the marker in place.
	var factories []instance.Factory

	// @@INSTANCE_FACTORIES@@
	exampleTimeout := kingpin.Flag("collector.example.timeout", "Per-request timeout for the example collector.").Default("5s").Duration()
	exampleInterval := kingpin.Flag("collector.example.interval", "Background refresh interval for the example collector.").Default("5m").Duration()
	exampleEnabled := kingpin.Flag("collector.example", "Enable the example collector.").Default("true").Bool()
	// The closure defers every flag dereference and the log reference to the
	// reconciler, which runs after kingpin.Parse() and after log is built. It no
	// longer builds a transport: the Handle owns one per machine, shared by
	// every collector, so a reload can swap it underneath them.
	factories = append(factories, instance.Factory{
		Name:    "example",
		Enabled: exampleEnabled,
		New: func(h *instance.Handle) (instance.BackgroundCollector, error) {
			c, err := h.ClientFor(*exampleTimeout)
			if err != nil {
				return nil, err
			}
			return collector.NewExampleCollector(log, c, *exampleInterval), nil
		},
	})

	kingpin.Version(version.Print("tapelibrary_exporter"))
	kingpin.HelpFlag.Short('h')

	// --config.file is mandatory here; read it before parsing (its value decides
	// which arguments the parser is given).
	configPath := config.ExtractFlagValue(os.Args[1:], "config.file")
	if configPath == "" {
		fmt.Fprintln(os.Stderr, "the multi-instance target model requires --config.file (it lists the instances to watch)")
		stop()     // release the signal handler explicitly before bypassing defer via os.Exit
		os.Exit(1) //nolint:gocritic // stop() called explicitly above
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		stop()     // release the signal handler explicitly before bypassing defer via os.Exit
		os.Exit(1) //nolint:gocritic // stop() called explicitly above
	}
	if err := cfg.Validate(kingpin.CommandLine); err != nil {
		fmt.Fprintln(os.Stderr, err)
		stop()     // release the signal handler explicitly before bypassing defer via os.Exit
		os.Exit(1) //nolint:gocritic // stop() called explicitly above
	}
	kingpin.MustParse(kingpin.CommandLine.Parse(
		append(cfg.ToArgs(config.CLIFlagNames(os.Args[1:])), os.Args[1:]...),
	))

	if *logFormat == "json" {
		log = logger.NewJSONLogger(*logLevel)
	} else {
		log = logger.NewTextLogger(*logLevel)
	}

	warnIfExposedAndUnauthenticated(log, *toolkitFlags.WebListenAddresses, *toolkitFlags.WebConfigFile)

	// Validate and resolve the instance list, fail-fast: at least one instance,
	// unique names, http/https addresses, resolvable module references, no
	// instance label colliding with the identifying label. Each module's
	// http_client_config was already validated by config.Load.
	// "collector" is the variable label StatusTracker's health metrics carry; an
	// instance label reusing it would collide at registration.
	instances, err := cfg.ResolveInstances(instanceLabel, "collector")
	if err != nil {
		log.Error("Invalid instance configuration", "err", err)
		stop()     // release the signal handler explicitly before bypassing defer via os.Exit
		os.Exit(1) //nolint:gocritic // stop() called explicitly above
	}

	// Filter to the globally-enabled collectors once (enablement is global under
	// this model), preserving declaration order.
	var enabled []instance.Factory
	for _, f := range factories {
		if *f.Enabled {
			enabled = append(enabled, f)
			log.Info("Collector enabled", "collector", f.Name)
		} else {
			log.Info("Collector disabled", "collector", f.Name)
		}
	}

	// Custom registry (no global state or third-party metric pollution).
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewBuildInfoCollector())
	if !*disableExporterMetrics {
		reg.MustRegister(
			collectors.NewGoCollector(),
			collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		)
	}
	// Boot is Prepare against an empty live set, then Commit: the exact same two
	// calls a configuration reload makes. That is what lets the reload path be
	// exercised by every start and by every golden cell, not only when somebody
	// sends a SIGHUP. Each instance's series carry the identifying label plus
	// the instance's own extra labels, applied by prometheus.WrapRegistererWith
	// as ConstLabels inside Registry.Commit.
	//
	// *maxRequestsPerTarget: 0 by default, meaning unlimited (Registry's own
	// documented meaning for 0). Each Handle Prepare builds gets its own
	// Limiter from this same ceiling, so an instance added by a later reload
	// is covered too, without anything having to be pre-populated.
	registry := instance.NewRegistry(log, reg, instanceLabel, enabled, *maxRequestsPerTarget)
	plan, err := registry.Prepare(instances)
	if err != nil {
		// A transport that cannot be built (unreadable CA or secret file) is a
		// configuration fault: stop at boot rather than surface it on the first
		// scrape. Prepare mutates nothing on error, so there is nothing to undo.
		log.Error("Invalid instance configuration", "err", err)
		stop()     // release the signal handler explicitly before bypassing defer via os.Exit
		os.Exit(1) //nolint:gocritic // stop() called explicitly above
	}
	registry.Commit(ctx, plan)

	// The reload policy for this target model. ResolveInstances plus Prepare
	// are the prepare phase: they validate the file and build every transport,
	// so an unreadable CA on the third instance fails here, before anything has
	// been started or stopped. Commit is the commit phase and cannot fail.
	reloader := reload.New(log, configPath, cfg, func(next *config.Config) error {
		nextInstances, err := next.ResolveInstances(instanceLabel, "collector")
		if err != nil {
			return err
		}
		plan, err := registry.Prepare(nextInstances)
		if err != nil {
			return err
		}
		registry.Commit(ctx, plan)
		return nil
	})
	// reloaderDone closes only once Run has returned, which is only after any
	// reload it is CURRENTLY running has finished. Run's own goroutine is what
	// calls registry.Commit above, mutating registry's live handles map;
	// registry.Wait below reads that same map by ranging it, so main must join
	// this channel first, or a SIGHUP/POST /-/reload racing shutdown could
	// still be inside Commit while Wait is already iterating.
	reloaderDone := make(chan struct{})
	go func() {
		defer close(reloaderDone)
		reloader.Run(ctx)
	}()

	reg.MustRegister(collector.RequestDuration)
	reg.MustRegister(collector.RequestWait)
	reg.MustRegister(reloader.Collectors()...)

	log.Info("Starting multi-instance exporter server...", "namespace", namespace, "instances", len(instances))

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(indexHTML))
	})
	http.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{
		EnableOpenMetrics: true,
		ErrorHandling:     promhttp.ContinueOnError,
	}))
	http.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	// Built once, outside the handler below: Handler() only wraps r in a
	// closure and allocates nothing expensive itself, but building it fresh
	// on every request would still be pointless per-request allocation for a
	// value that never changes for the life of the process.
	reloadHandler := reloader.Handler()

	// Registered unconditionally, ahead of the "/" handler above, but the flag
	// check inside is what makes an exporter that did not opt in answer 404
	// on this path: http.ServeMux treats "/" as a catch-all for any path with
	// no more specific match, so leaving this path entirely unregistered
	// would fall through to the landing page instead of 404ing.
	http.HandleFunc("/-/reload", func(w http.ResponseWriter, r *http.Request) {
		if !*enableLifecycle {
			http.NotFound(w, r)
			return
		}
		reloadHandler.ServeHTTP(w, r)
	})
	if !*enableLifecycle {
		log.Info("POST /-/reload is disabled; SIGHUP still reloads the configuration",
			"enable_with", "--web.enable-lifecycle")
	}

	server := &http.Server{
		ReadHeaderTimeout: 5 * time.Second, // Mitigate Slowloris attacks (G112).
	}

	// shutdownDone closes only once server.Shutdown has fully drained every
	// in-flight request. That matters beyond the HTTP response itself:
	// web.ListenAndServe below returns as soon as Shutdown closes the
	// listener, well before Shutdown finishes waiting on active handlers, so
	// without joining this channel too, a POST /-/reload handler still
	// running past that point could take reload.Reloader.Reload's inline
	// fallback (taken whenever Run is not there to receive the request,
	// which is guaranteed once Run has already returned) and call
	// registry.Commit directly on the handler's own goroutine, racing
	// registry.Wait below exactly like reloaderDone's own case.
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		log.Info("Shutdown signal received, draining in-flight requests...")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Error("Graceful shutdown failed", "err", err)
		}
	}()

	if err := web.ListenAndServe(server, toolkitFlags, log.Logger); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("Failed to start HTTP server", "err", err)
		stop()     // release the signal handler explicitly before bypassing defer via os.Exit
		os.Exit(1) //nolint:gocritic // stop() called explicitly above
	}

	log.Info("Server stopped")

	// Both goroutines above must be joined before registry.Wait touches the
	// live handles map: shutdownDone proves no HTTP handler is still inside
	// Reload's inline fallback, and reloaderDone proves Run's own goroutine
	// is not still inside Commit. Order between the two does not matter, only
	// that both finish before Wait starts.
	<-shutdownDone
	<-reloaderDone

	// ONE shared 5s budget for ALL background collectors, not 5s each: with N
	// instances x M collectors, a per-collector wait would worst-case at
	// N*M*5s. See Registry.Wait's own doc comment.
	registry.Wait(5 * time.Second)
}
