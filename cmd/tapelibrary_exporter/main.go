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
	// siblings, so it is a deliberate choice with a visible cost.
	// tapelibrary_exporter_request_wait_seconds is what makes queueing
	// visible rather than silent, and --exporter.max-queue-wait below is what
	// bounds it without eating the request's own budget.
	//
	// **The default is 1, not 0, and that is not a preference.** R1.11.2's
	// own "Query and task flow" notes require the response to a REST command
	// to be retrieved before the next command is sent: the library serializes
	// by construction. Measured against a real machine on 2026-08-03, ten
	// concurrent requests took 9s in total against 12s issued one at a time —
	// no useful gain — while per-request latency went from ~1s to as much as
	// 8.2s. Concurrency here buys nothing and costs every collector its
	// timeout, which is exactly what left all nineteen of them failing on the
	// first run against real hardware.
	maxRequestsPerTarget = kingpin.Flag(
		"exporter.max-requests-per-target",
		"Maximum concurrent requests this exporter issues per watched instance. Defaults to 1 because the TS4500 serializes REST commands internally (R1.11.2 requires each response to be retrieved before the next command), so concurrency measurably buys nothing and inflates per-request latency. 0 means unlimited. Two instances sharing one physical address are bounded independently, not together.",
	).Default("1").Int()

	// maxQueueWait bounds the wait for a request slot, separately from the
	// per-collector request timeout that follows it. The split is what makes
	// a ceiling of 1 usable at all: with nineteen collectors per library a
	// queue is the normal state, and a collector's position in it has nothing
	// to do with how long its own request needs. Before the two were split,
	// a collector queued behind its siblings burned its whole 5s budget
	// waiting and had nothing left for a one-second request — 14 of 19
	// starved on every sweep, permanently, because same-interval tickers all
	// fire together and the alignment never breaks up on its own.
	//
	// 15m rather than something tighter because ONE collector can legitimately
	// hold the slot for a long time: /v1/dataCartridges measures over 600s on
	// the reference fleet, and everything else queues behind it. A budget
	// shorter than that would abandon healthy refreshes for a reason that is
	// not their fault.
	maxQueueWait = kingpin.Flag(
		"exporter.max-queue-wait",
		"How long a collector waits for its instance's request slot before abandoning the round. Separate from --collector.<name>.timeout, which bounds the request itself once a slot is held: with a concurrency ceiling of 1 a queue is normal, and charging the wait to the request budget starves collectors deep in the queue.",
	).Default("15m").Duration()

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
	exampleTimeout := kingpin.Flag("collector.example.timeout", "Per-request timeout for the example collector. Calibrated 2026-08-03 against real hardware: this endpoint measures under 2s idle, but /v1/library was observed once at 45s and the SCSI buffer path makes latency unpredictable from payload size. A generous ceiling costs nothing when the endpoint is fast and is the difference between a served cache and a permanently empty one.").Default("60s").Duration()
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

	libraryTimeout := kingpin.Flag("collector.library.timeout", "Per-request timeout for the library collector. Calibrated 2026-08-03 against real hardware: this endpoint measures under 2s idle, but /v1/library was observed once at 45s and the SCSI buffer path makes latency unpredictable from payload size. A generous ceiling costs nothing when the endpoint is fast and is the difference between a served cache and a permanently empty one.").Default("60s").Duration()
	libraryInterval := kingpin.Flag("collector.library.interval", "Background refresh interval for the library collector.").Default("5m").Duration()
	libraryEnabled := kingpin.Flag("collector.library", "Enable the library collector.").Default("true").Bool()
	factories = append(factories, instance.Factory{
		Name:    "library",
		Enabled: libraryEnabled,
		New: func(h *instance.Handle) (instance.BackgroundCollector, error) {
			c, err := h.ClientFor(*libraryTimeout)
			if err != nil {
				return nil, err
			}
			return collector.NewLibraryCollector(log, c, *libraryInterval), nil
		},
	})

	framesTimeout := kingpin.Flag("collector.frames.timeout", "Per-request timeout for the frames collector. Calibrated 2026-08-03 against real hardware: this endpoint measures under 2s idle, but /v1/library was observed once at 45s and the SCSI buffer path makes latency unpredictable from payload size. A generous ceiling costs nothing when the endpoint is fast and is the difference between a served cache and a permanently empty one.").Default("60s").Duration()
	framesInterval := kingpin.Flag("collector.frames.interval", "Background refresh interval for the frames collector.").Default("5m").Duration()
	framesEnabled := kingpin.Flag("collector.frames", "Enable the frames collector.").Default("true").Bool()
	factories = append(factories, instance.Factory{
		Name:    "frames",
		Enabled: framesEnabled,
		New: func(h *instance.Handle) (instance.BackgroundCollector, error) {
			c, err := h.ClientFor(*framesTimeout)
			if err != nil {
				return nil, err
			}
			return collector.NewFramesCollector(log, c, *framesInterval), nil
		},
	})

	accessorsTimeout := kingpin.Flag("collector.accessors.timeout", "Per-request timeout for the accessors collector. Calibrated 2026-08-03 against real hardware: this endpoint measures under 2s idle, but /v1/library was observed once at 45s and the SCSI buffer path makes latency unpredictable from payload size. A generous ceiling costs nothing when the endpoint is fast and is the difference between a served cache and a permanently empty one.").Default("60s").Duration()
	accessorsInterval := kingpin.Flag("collector.accessors.interval", "Background refresh interval for the accessors collector.").Default("5m").Duration()
	accessorsEnabled := kingpin.Flag("collector.accessors", "Enable the accessors collector.").Default("true").Bool()
	factories = append(factories, instance.Factory{
		Name:    "accessors",
		Enabled: accessorsEnabled,
		New: func(h *instance.Handle) (instance.BackgroundCollector, error) {
			c, err := h.ClientFor(*accessorsTimeout)
			if err != nil {
				return nil, err
			}
			return collector.NewAccessorsCollector(log, c, *accessorsInterval), nil
		},
	})

	drivesTimeout := kingpin.Flag("collector.drives.timeout", "Per-request timeout for the drives collector. Calibrated 2026-08-03 against real hardware: this endpoint measures under 2s idle, but /v1/library was observed once at 45s and the SCSI buffer path makes latency unpredictable from payload size. A generous ceiling costs nothing when the endpoint is fast and is the difference between a served cache and a permanently empty one.").Default("60s").Duration()
	drivesInterval := kingpin.Flag("collector.drives.interval", "Background refresh interval for the drives collector.").Default("5m").Duration()
	drivesEnabled := kingpin.Flag("collector.drives", "Enable the drives collector.").Default("true").Bool()
	// Default false, and the reason is Prometheus's index rather than this
	// exporter's memory: only 40 volsers are loaded at once, but the drive
	// holding a given tape changes constantly, so location x volser
	// accumulates an index entry for every pairing that has ever existed.
	drivesPerVolser := kingpin.Flag("collector.drives.per-volser", "Emit tapelibrary_drive_loaded_cartridge_info, labelling each drive with the cartridge it currently holds. Off by default: the volser churns, so this pairing accumulates far more series in Prometheus over time than the number ever active at once.").Default("false").Bool()
	factories = append(factories, instance.Factory{
		Name:    "drives",
		Enabled: drivesEnabled,
		New: func(h *instance.Handle) (instance.BackgroundCollector, error) {
			c, err := h.ClientFor(*drivesTimeout)
			if err != nil {
				return nil, err
			}
			return collector.NewDrivesCollector(log, c, *drivesInterval, *drivesPerVolser), nil
		},
	})

	powerSuppliesTimeout := kingpin.Flag("collector.power_supplies.timeout", "Per-request timeout for the power_supplies collector. Calibrated 2026-08-03 against real hardware: this endpoint measures under 2s idle, but /v1/library was observed once at 45s and the SCSI buffer path makes latency unpredictable from payload size. A generous ceiling costs nothing when the endpoint is fast and is the difference between a served cache and a permanently empty one.").Default("60s").Duration()
	powerSuppliesInterval := kingpin.Flag("collector.power_supplies.interval", "Background refresh interval for the power_supplies collector.").Default("5m").Duration()
	powerSuppliesEnabled := kingpin.Flag("collector.power_supplies", "Enable the power_supplies collector.").Default("true").Bool()
	factories = append(factories, instance.Factory{
		Name:    "power_supplies",
		Enabled: powerSuppliesEnabled,
		New: func(h *instance.Handle) (instance.BackgroundCollector, error) {
			c, err := h.ClientFor(*powerSuppliesTimeout)
			if err != nil {
				return nil, err
			}
			return collector.NewPowerSuppliesCollector(log, c, *powerSuppliesInterval), nil
		},
	})

	nodeCardsTimeout := kingpin.Flag("collector.node_cards.timeout", "Per-request timeout for the node_cards collector. Calibrated 2026-08-03 against real hardware: this endpoint measures under 2s idle, but /v1/library was observed once at 45s and the SCSI buffer path makes latency unpredictable from payload size. A generous ceiling costs nothing when the endpoint is fast and is the difference between a served cache and a permanently empty one.").Default("60s").Duration()
	nodeCardsInterval := kingpin.Flag("collector.node_cards.interval", "Background refresh interval for the node_cards collector.").Default("5m").Duration()
	nodeCardsEnabled := kingpin.Flag("collector.node_cards", "Enable the node_cards collector.").Default("true").Bool()
	factories = append(factories, instance.Factory{
		Name:    "node_cards",
		Enabled: nodeCardsEnabled,
		New: func(h *instance.Handle) (instance.BackgroundCollector, error) {
			c, err := h.ClientFor(*nodeCardsTimeout)
			if err != nil {
				return nil, err
			}
			return collector.NewNodeCardsCollector(log, c, *nodeCardsInterval), nil
		},
	})

	ioStationsTimeout := kingpin.Flag("collector.io_stations.timeout", "Per-request timeout for the io_stations collector. Calibrated 2026-08-03 against real hardware: this endpoint measures under 2s idle, but /v1/library was observed once at 45s and the SCSI buffer path makes latency unpredictable from payload size. A generous ceiling costs nothing when the endpoint is fast and is the difference between a served cache and a permanently empty one.").Default("60s").Duration()
	ioStationsInterval := kingpin.Flag("collector.io_stations.interval", "Background refresh interval for the io_stations collector.").Default("5m").Duration()
	ioStationsEnabled := kingpin.Flag("collector.io_stations", "Enable the io_stations collector.").Default("true").Bool()
	factories = append(factories, instance.Factory{
		Name:    "io_stations",
		Enabled: ioStationsEnabled,
		New: func(h *instance.Handle) (instance.BackgroundCollector, error) {
			c, err := h.ClientFor(*ioStationsTimeout)
			if err != nil {
				return nil, err
			}
			return collector.NewIOStationsCollector(log, c, *ioStationsInterval), nil
		},
	})

	fcPortsTimeout := kingpin.Flag("collector.fc_ports.timeout", "Per-request timeout for the fc_ports collector. Calibrated 2026-08-03 against real hardware: this endpoint measures under 2s idle, but /v1/library was observed once at 45s and the SCSI buffer path makes latency unpredictable from payload size. A generous ceiling costs nothing when the endpoint is fast and is the difference between a served cache and a permanently empty one.").Default("60s").Duration()
	fcPortsInterval := kingpin.Flag("collector.fc_ports.interval", "Background refresh interval for the fc_ports collector.").Default("5m").Duration()
	fcPortsEnabled := kingpin.Flag("collector.fc_ports", "Enable the fc_ports collector.").Default("true").Bool()
	factories = append(factories, instance.Factory{
		Name:    "fc_ports",
		Enabled: fcPortsEnabled,
		New: func(h *instance.Handle) (instance.BackgroundCollector, error) {
			c, err := h.ClientFor(*fcPortsTimeout)
			if err != nil {
				return nil, err
			}
			return collector.NewFCPortsCollector(log, c, *fcPortsInterval), nil
		},
	})

	logicalLibrariesTimeout := kingpin.Flag("collector.logical_libraries.timeout", "Per-request timeout for the logical_libraries collector. Calibrated 2026-08-03 against real hardware: this endpoint measures under 2s idle, but /v1/library was observed once at 45s and the SCSI buffer path makes latency unpredictable from payload size. A generous ceiling costs nothing when the endpoint is fast and is the difference between a served cache and a permanently empty one.").Default("60s").Duration()
	logicalLibrariesInterval := kingpin.Flag("collector.logical_libraries.interval", "Background refresh interval for the logical_libraries collector.").Default("5m").Duration()
	logicalLibrariesEnabled := kingpin.Flag("collector.logical_libraries", "Enable the logical_libraries collector.").Default("true").Bool()
	factories = append(factories, instance.Factory{
		Name:    "logical_libraries",
		Enabled: logicalLibrariesEnabled,
		New: func(h *instance.Handle) (instance.BackgroundCollector, error) {
			c, err := h.ClientFor(*logicalLibrariesTimeout)
			if err != nil {
				return nil, err
			}
			return collector.NewLogicalLibrariesCollector(log, c, *logicalLibrariesInterval), nil
		},
	})

	cleaningCartridgesTimeout := kingpin.Flag("collector.cleaning_cartridges.timeout", "Per-request timeout for the cleaning_cartridges collector. Calibrated 2026-08-03 against real hardware: this endpoint measures under 2s idle, but /v1/library was observed once at 45s and the SCSI buffer path makes latency unpredictable from payload size. A generous ceiling costs nothing when the endpoint is fast and is the difference between a served cache and a permanently empty one.").Default("60s").Duration()
	cleaningCartridgesInterval := kingpin.Flag("collector.cleaning_cartridges.interval", "Background refresh interval for the cleaning_cartridges collector.").Default("5m").Duration()
	cleaningCartridgesEnabled := kingpin.Flag("collector.cleaning_cartridges", "Enable the cleaning_cartridges collector.").Default("true").Bool()
	// Default TRUE, the inverse of --collector.drives.per-volser, and the only
	// per-item detail in this exporter that ships on. The population is bounded
	// by the site's cleaning policy (70 on this fleet) rather than by library
	// capacity, and a cleaning cartridge sits in one slot until it is used or
	// exported, so its volser does not churn the way a drive's loaded volser
	// does. Naming the exhausted cartridge is also the point of the collector.
	// Turning it off costs that detail and nothing else: the three supply
	// alerts read library-wide aggregates that are emitted either way.
	cleaningCartridgesPerVolser := kingpin.Flag("collector.cleaning_cartridges.per-volser", "Emit tapelibrary_cleaning_cartridge_cleans_remaining and tapelibrary_cleaning_cartridge_last_usage_timestamp_seconds, one of each per cleaning cartridge. On by default: the population is bounded by cleaning policy rather than library capacity, and only the per-cartridge series can name which cartridge to pull. Turn it off on a site running cleaning cartridges in the thousands; the library-wide aggregates the alerts read are emitted regardless.").Default("true").Bool()
	factories = append(factories, instance.Factory{
		Name:    "cleaning_cartridges",
		Enabled: cleaningCartridgesEnabled,
		New: func(h *instance.Handle) (instance.BackgroundCollector, error) {
			c, err := h.ClientFor(*cleaningCartridgesTimeout)
			if err != nil {
				return nil, err
			}
			return collector.NewCleaningCartridgesCollector(log, c, *cleaningCartridgesInterval, *cleaningCartridgesPerVolser), nil
		},
	})

	// The first collector in this exporter whose timeout and interval defaults
	// depart from the shared 5s/5m, and the furthest: slots below is the only
	// other one, at 30s/15m. Both departures are the endpoint's rather than a
	// preference. /v1/dataCartridges returns every cartridge the
	// library holds — 9 749 on this fleet — unpaginated, over the same slow
	// SCSI/LCC-backed path docs/exporter-journal.md names when it explains why
	// this build is multi-instance at all. 5s does not fetch that, so a 5s
	// default would ship a collector that fails every refresh and serves a
	// permanently empty cache.
	//
	// The interval moves with it. At a ceiling of one in-flight request per
	// library, a refresh this long blocks its seventeen siblings while it runs,
	// so it is run less often rather than more: the inventory turns over in
	// hours, not seconds, and nothing here is worth a minute of queueing every
	// five. That delay stays observable rather than hidden, through
	// tapelibrary_exporter_request_wait_seconds.
	dataCartridgesTimeout := kingpin.Flag("collector.data_cartridges.timeout", "Per-request timeout for the data_cartridges collector. Defaults higher than every other collector: this endpoint returns the library's entire cartridge inventory unpaginated over a slow path. Measured over 600s on the reference fleet: the previous 60s could never complete, so this collector had never once succeeded against real hardware.").Default("900s").Duration()
	dataCartridgesInterval := kingpin.Flag("collector.data_cartridges.interval", "Background refresh interval for the data_cartridges collector. Defaults longer than every other collector: a refresh holds the library's single request slot for as long as it runs, and a cartridge inventory does not turn over in minutes. Raised from 15m to 1h on 2026-08-03: with a concurrency ceiling of 1 this endpoint alone holds the library's only request slot for several minutes per refresh, and the two heavy cartridge collectors together needed more than a 15m cycle, starving every sibling.").Default("1h").Duration()
	dataCartridgesEnabled := kingpin.Flag("collector.data_cartridges", "Enable the data_cartridges collector.").Default("true").Bool()
	// Default FALSE, matching --collector.drives.per-volser and inverting
	// --collector.cleaning_cartridges.per-volser. The population is bounded by
	// library capacity rather than by any policy, so this is ~29 250 series per
	// library and ~146 000 across the fleet, against the ~2 425 per library the
	// exporter costs at defaults. That is a Prometheus sizing decision, so it is
	// the operator's to take. Turning it on adds detail and silences nothing:
	// every aggregate the alerts read is emitted either way.
	dataCartridgesPerVolser := kingpin.Flag("collector.data_cartridges.per-volser", "Emit tapelibrary_data_cartridge_info, tapelibrary_data_cartridge_lifetime_remaining_ratio and tapelibrary_data_cartridge_last_usage_timestamp_seconds, one of each per data cartridge. Off by default: at ~9 750 cartridges per library this is ~29 250 extra series per library and ~146 000 across a five-library fleet. The library-wide aggregates the alerts read are emitted regardless.").Default("false").Bool()
	factories = append(factories, instance.Factory{
		Name:    "data_cartridges",
		Enabled: dataCartridgesEnabled,
		New: func(h *instance.Handle) (instance.BackgroundCollector, error) {
			c, err := h.ClientFor(*dataCartridgesTimeout)
			if err != nil {
				return nil, err
			}
			return collector.NewDataCartridgesCollector(log, c, *dataCartridgesInterval, *dataCartridgesPerVolser), nil
		},
	})

	// The second collector whose defaults depart from the shared 5s/5m, and it
	// departs by less than data_cartridges does. /v1/slots walks the library's
	// whole slot inventory over the same slow SCSI/LCC path, but it returns one
	// entry per slot COLUMN rather than per cartridge — roughly 4 300 entries
	// against dataCartridges' 9 749, at about a third of the bytes each — so
	// 30s is the middle ground between a 5s default that would fail every
	// refresh and a 60s one this endpoint does not need.
	//
	// The interval matches data_cartridges at 15m rather than splitting the
	// difference, because the two describe the same physical movement: a
	// cartridge changing slots changes both. At a ceiling of one in-flight
	// request per library there is nothing to gain from learning about it twice
	// as often on one endpoint as on the other, and the queueing stays visible
	// in tapelibrary_exporter_request_wait_seconds.
	//
	// Neither figure is measured. docs/exporter-journal.md carries the standing
	// open question that per-endpoint cadences were never calibrated on this
	// fleet; these are sized against the capture, and are the operator's to
	// adjust once somebody times a real refresh.
	slotsTimeout := kingpin.Flag("collector.slots.timeout", "Per-request timeout for the slots collector. Defaults higher than most collectors: this endpoint returns the library's entire slot inventory unpaginated over a slow path. Measured 23.7s for 646 KB on the reference fleet, which left the previous 30s with almost no margin.").Default("180s").Duration()
	slotsInterval := kingpin.Flag("collector.slots.interval", "Background refresh interval for the slots collector. Defaults longer than most collectors: a refresh holds the library's single request slot for as long as it runs, and slot occupancy turns over at the same rate as the cartridge inventory.").Default("15m").Duration()
	slotsEnabled := kingpin.Flag("collector.slots", "Enable the slots collector.").Default("true").Bool()
	// Default FALSE, matching --collector.data_cartridges.per-volser. The
	// population is bounded by library capacity rather than by any policy: the
	// library reports 10 732 cartridge POSITIONS, which at this fleet's mix of
	// 1- and 4-tier slots is somewhere near 4 300 slot entries and so roughly
	// 21 500 extra series per library. That is a Prometheus sizing decision, so
	// it is the operator's to take. Turning it on adds detail and silences
	// nothing: every aggregate the alerts read is emitted either way.
	slotsPerSlot := kingpin.Flag("collector.slots.per-slot", "Emit tapelibrary_slot_info, tapelibrary_slot_positions_occupied and the three per-slot lifetime counters, one of each per storage slot. Off by default: at roughly 4 300 slots per library this is about 21 500 extra series per library and five times that across the fleet. The library-wide aggregates the alerts read are emitted regardless; turn it on to name which slot a rising retry rate is coming from.").Default("false").Bool()
	factories = append(factories, instance.Factory{
		Name:    "slots",
		Enabled: slotsEnabled,
		New: func(h *instance.Handle) (instance.BackgroundCollector, error) {
			c, err := h.ClientFor(*slotsTimeout)
			if err != nil {
				return nil, err
			}
			return collector.NewSlotsCollector(log, c, *slotsInterval, *slotsPerSlot), nil
		},
	})

	// The only collector in this exporter that reads a log rather than a
	// hardware inventory, and the only one whose request carries a query
	// parameter. Both defaults return to the shared 5s/5m, and that is the
	// endpoint's doing rather than a reversion to taste: bounded by lookback
	// below, GET /v1/events returns the handful of entries the library raised
	// in the last hour — 42 over a 3h45m span in the 2026-07-28 capture — so it
	// is one of the cheapest endpoints here, not one of the heaviest.
	eventsTimeout := kingpin.Flag("collector.events.timeout", "Per-request timeout for the events collector. Measured 58s unbounded on the reference fleet; the exporter narrows the window with --collector.events.lookback, so its real cost is lower, but the endpoint returns megabytes and 5s could never cover it.").Default("120s").Duration()
	eventsInterval := kingpin.Flag("collector.events.interval", "Background refresh interval for the events collector.").Default("5m").Duration()
	eventsEnabled := kingpin.Flag("collector.events", "Enable the events collector.").Default("true").Bool()
	// Not a tuning knob: without it this collector is unusable. R1.11.2 states
	// that a bare GET /v1/events "retrieves a list of all events", and the
	// 2026-07-28 capture carries IDs past 19 400, so an unbounded request would
	// re-download the library's entire event history every interval over the
	// slow SCSI/LCC path, holding the library's single request slot against its
	// seventeen siblings to report on the last hour.
	//
	// 1h against a 5m interval is deliberately generous. The window is a
	// trailing one recomputed at every refresh, so it only has to exceed the
	// interval for no event to fall in a gap; the remaining 55 minutes are
	// slack against clock skew between this host and the library, which is the
	// one failure mode that would silently empty the window rather than
	// announcing itself (a library whose clock trails this host by more than
	// the lookback returns nothing, and the refresh still succeeds).
	eventsLookback := kingpin.Flag("collector.events.lookback", "How far back the events collector asks the library to look, sent as the endpoint's `after` parameter. Must exceed --collector.events.interval or events raised between two refreshes are never counted. Also absorbs clock skew between this host and the library.").Default("1h").Duration()
	// Empty by default, which suppresses tapelibrary_events_by_code entirely.
	// errorCode is a 4-digit hex code — up to 65 536 values — and R1.11.2
	// enumerates none of them, so it is a cardinality risk that cannot be
	// budgeted in advance (see docs/exporter-journal.md, "Open questions").
	// Naming codes explicitly is the bounded form: an operator adds the handful
	// their fleet actually raises, once they have seen them. The severity
	// breakdown every alert reads is emitted regardless.
	eventsErrorCodes := kingpin.Flag("collector.events.error-codes", "Comma-separated library error codes to break out as tapelibrary_events_by_code, e.g. \"B792,0217\". Empty by default: errorCode is an unenumerated 4-digit hex field, so only codes named here get their own series. Matching is case-insensitive.").Default("").String()
	factories = append(factories, instance.Factory{
		Name:    "events",
		Enabled: eventsEnabled,
		New: func(h *instance.Handle) (instance.BackgroundCollector, error) {
			c, err := h.ClientFor(*eventsTimeout)
			if err != nil {
				return nil, err
			}
			return collector.NewEventsCollector(log, c, *eventsInterval, *eventsLookback, *eventsErrorCodes), nil
		},
	})

	// The third collector to depart from the shared 5s/5m, and the only one to
	// take data_cartridges' figures outright rather than deriving its own.
	//
	// GET /v1/dataCartridges/lifetimeMetrics walks the SAME population as
	// GET /v1/dataCartridges — one entry per cartridge, 9 749 on this library,
	// unpaginated over the same slow SCSI/LCC path — and reads each cartridge's
	// own memory to do it. It returns fewer bytes per entry (roughly 280 against
	// 640 in the 2026-07-28 capture), but the bytes are not what makes this
	// endpoint slow: walking the inventory is, and that walk is identical. So
	// the timeout matches at 60s rather than being scaled down by the byte
	// ratio, which would ship a collector that fails every refresh.
	//
	// The interval matches at 15m for a second reason on top of that one: the
	// two endpoints describe the same cartridges, and at a ceiling of one
	// in-flight request per library there is nothing to gain from learning about
	// one twice as often as the other. Lifetime counters move slower than
	// inventory, if anything. The queueing stays visible in
	// tapelibrary_exporter_request_wait_seconds.
	//
	// Neither figure is measured. docs/exporter-journal.md carries the standing
	// open question that per-endpoint cadences were never calibrated on this
	// fleet; these are sized against the capture, and are the operator's to
	// adjust once somebody times a real refresh.
	dataCartridgesLifetimeTimeout := kingpin.Flag("collector.data_cartridges_lifetime.timeout", "Per-request timeout for the data_cartridges_lifetime collector. Defaults as high as the data_cartridges collector: this endpoint walks the library's entire cartridge inventory unpaginated over the same slow path, reading each cartridge's own memory. Measured 343.9s for 2.35 MB on the reference fleet: the previous 60s could never complete, so this collector had never once succeeded against real hardware.").Default("900s").Duration()
	dataCartridgesLifetimeInterval := kingpin.Flag("collector.data_cartridges_lifetime.interval", "Background refresh interval for the data_cartridges_lifetime collector. Defaults as long as the data_cartridges collector: a refresh holds the library's single request slot for as long as it runs, and lifetime counters move slower than the inventory itself. Raised from 15m to 1h on 2026-08-03: with a concurrency ceiling of 1 this endpoint alone holds the library's only request slot for several minutes per refresh, and the two heavy cartridge collectors together needed more than a 15m cycle, starving every sibling.").Default("1h").Duration()
	dataCartridgesLifetimeEnabled := kingpin.Flag("collector.data_cartridges_lifetime", "Enable the data_cartridges_lifetime collector.").Default("true").Bool()
	// Default FALSE, matching --collector.data_cartridges.per-volser and
	// --collector.slots.per-slot. This is the largest per-object cost in the
	// exporter: seven series per cartridge against data_cartridges' three, so
	// ~68 250 per library and ~341 000 across the fleet. That is a Prometheus
	// sizing decision, so it is the operator's to take. Turning it on adds
	// detail and silences nothing: every aggregate the alerts read is emitted
	// either way.
	dataCartridgesLifetimePerVolser := kingpin.Flag("collector.data_cartridges_lifetime.per-volser", "Emit tapelibrary_data_cartridge_usage_motion_meters_total, _mounts_total, _written_bytes_total and the four _errors_total combinations, one set per data cartridge. Off by default: at ~9 750 cartridges per library this is ~68 250 extra series per library and ~341 000 across a five-library fleet. The library-wide distributions the alerts read are emitted regardless; turn it on to name which cartridge a rising uncorrected-error count is coming from.").Default("false").Bool()
	factories = append(factories, instance.Factory{
		Name:    "data_cartridges_lifetime",
		Enabled: dataCartridgesLifetimeEnabled,
		New: func(h *instance.Handle) (instance.BackgroundCollector, error) {
			c, err := h.ClientFor(*dataCartridgesLifetimeTimeout)
			if err != nil {
				return nil, err
			}
			return collector.NewDataCartridgesLifetimeCollector(log, c, *dataCartridgesLifetimeInterval, *dataCartridgesLifetimePerVolser), nil
		},
	})

	// The interval is the first one in this file chosen against the DATA's own
	// cadence rather than against the endpoint's cost. R1.11.2 records one
	// report per completed hour, so there is nothing new to fetch more often
	// than hourly; 15m is a quarter of that, which bounds how long a freshly
	// published window sits unseen without pretending the exporter can resolve
	// anything finer. Polling at 5m like the hardware collectors would triple
	// the requests to serve the same four values an hour.
	//
	// The timeout stays at the 5s default: the response is the last week of
	// hourly entries, ~67 KB, and needs no inventory walk.
	reportsLibraryTimeout := kingpin.Flag("collector.reports_library.timeout", "Per-request timeout for the reports_library collector. Calibrated 2026-08-03 against real hardware: this endpoint measures under 2s idle, but /v1/library was observed once at 45s and the SCSI buffer path makes latency unpredictable from payload size. A generous ceiling costs nothing when the endpoint is fast and is the difference between a served cache and a permanently empty one.").Default("60s").Duration()
	reportsLibraryInterval := kingpin.Flag("collector.reports_library.interval", "Background refresh interval for the reports_library collector. Defaults to a quarter of the endpoint's own hourly cadence: R1.11.2 publishes one report per completed hour, so polling faster returns the same window again.").Default("15m").Duration()
	reportsLibraryEnabled := kingpin.Flag("collector.reports_library", "Enable the reports_library collector.").Default("true").Bool()
	factories = append(factories, instance.Factory{
		Name:    "reports_library",
		Enabled: reportsLibraryEnabled,
		New: func(h *instance.Handle) (instance.BackgroundCollector, error) {
			c, err := h.ClientFor(*reportsLibraryTimeout)
			if err != nil {
				return nil, err
			}
			return collector.NewReportsLibraryCollector(log, c, *reportsLibraryInterval), nil
		},
	})

	// reports_drives reads the same hourly publication as reports_library, but
	// one entry per drive rather than one per library, and that changes both
	// defaults away from its sibling's.
	//
	// The interval is 1h, not 15m: the endpoint publishes one window per
	// completed hour, so a quarter-hourly poll returns the same 40 rows four
	// times over. What makes that repetition expensive here rather than merely
	// wasteful is the response size — the default week is ~168 windows x 40
	// drives, roughly 3.3 MB against reports/library's ~67 KB for the same
	// week — combined with the concurrency ceiling of 1, which means every
	// byte of it blocks this library's other collectors. Matching the
	// publication cadence instead of quartering it cuts the transfer fourfold
	// for data that cannot change in between. The cost is stated rather than
	// hidden: a freshly published window can sit up to an hour unseen, which
	// tapelibrary_drive_report_window_timestamp_seconds makes visible.
	//
	// The timeout is 60s for the same reason it is on data_cartridges and
	// data_cartridges_lifetime: 3.3 MB over the LCC-backed path is not a 5s
	// request.
	reportsDrivesTimeout := kingpin.Flag("collector.reports_drives.timeout", "Per-request timeout for the reports_drives collector. Higher than most collectors: the endpoint returns a week of hourly windows for every drive, ~3.3 MB on a 40-drive library. Measured 49.8s for 2.9 MB on the reference fleet, which left the previous 60s with almost no margin.").Default("180s").Duration()
	reportsDrivesInterval := kingpin.Flag("collector.reports_drives.interval", "Background refresh interval for the reports_drives collector. Defaults to the endpoint's own hourly cadence: R1.11.2 publishes one report per drive per completed hour, so polling faster re-transfers the same windows.").Default("1h").Duration()
	reportsDrivesEnabled := kingpin.Flag("collector.reports_drives", "Enable the reports_drives collector.").Default("true").Bool()
	factories = append(factories, instance.Factory{
		Name:    "reports_drives",
		Enabled: reportsDrivesEnabled,
		New: func(h *instance.Handle) (instance.BackgroundCollector, error) {
			c, err := h.ClientFor(*reportsDrivesTimeout)
			if err != nil {
				return nil, err
			}
			return collector.NewReportsDrivesCollector(log, c, *reportsDrivesInterval), nil
		},
	})

	// reports_accessors reads the same hourly publication as its two
	// siblings, one entry per accessor, and its defaults land between theirs
	// rather than copying either wholesale.
	//
	// The interval is 15m, matching reports_library rather than
	// reports_drives' 1h. What pushed reports_drives to the endpoint's own
	// cadence was size against the concurrency ceiling of 1 — a ~3.3 MB
	// transfer blocking every sibling on the library, four times an hour.
	// That argument does not reach here: a library has two accessors, so the
	// default week is ~168 windows x 2, roughly 148 KB at the capture's ~440
	// bytes per entry. Quartering the hourly cadence costs little and means a
	// freshly published window is visible within 15 minutes rather than up to
	// an hour, which matters on the one endpoint whose alert
	// (AccessorReportShareCollapsed) is about an accessor having stopped.
	//
	// The timeout is 60s despite that small response, and it is a deliberate
	// departure from reports_library's 5s: the RoE path is slow in ways the
	// response size does not predict, and a timeout that fires serves a
	// permanently empty cache rather than late data. The cost of the higher
	// value is bounded by the concurrency ceiling being per instance.
	reportsAccessorsTimeout := kingpin.Flag("collector.reports_accessors.timeout", "Per-request timeout for the reports_accessors collector. Generous relative to the ~148 KB response: the RoE path can be slow regardless of payload size, and a timeout that fires leaves the cache empty.").Default("60s").Duration()
	reportsAccessorsInterval := kingpin.Flag("collector.reports_accessors.interval", "Background refresh interval for the reports_accessors collector. Defaults to a quarter of the endpoint's own hourly cadence, as reports_library does: R1.11.2 publishes one report per accessor per completed hour, and the response is small enough that re-reading it costs little.").Default("15m").Duration()
	reportsAccessorsEnabled := kingpin.Flag("collector.reports_accessors", "Enable the reports_accessors collector.").Default("true").Bool()
	factories = append(factories, instance.Factory{
		Name:    "reports_accessors",
		Enabled: reportsAccessorsEnabled,
		New: func(h *instance.Handle) (instance.BackgroundCollector, error) {
			c, err := h.ClientFor(*reportsAccessorsTimeout)
			if err != nil {
				return nil, err
			}
			return collector.NewReportsAccessorsCollector(log, c, *reportsAccessorsInterval), nil
		},
	})

	// diagnostic_cartridges keeps the shared 5s/5m defaults: the endpoint
	// returns one entry per diagnostic cartridge, five on the reference
	// fleet, and the population changes only when somebody physically loads
	// or removes one.
	//
	// --collector.diagnostic_cartridges.per-volser defaults TRUE, making this
	// the second collector after cleaning_cartridges to emit volser without
	// being asked. Same justification, applied to a smaller population: the
	// count is bounded by service policy rather than by library capacity, and
	// only the per-cartridge series can name WHICH cartridge to pull. The
	// library-wide aggregates every alert reads are emitted regardless, so
	// turning it off costs detail and never coverage.
	diagnosticCartridgesTimeout := kingpin.Flag("collector.diagnostic_cartridges.timeout", "Per-request timeout for the diagnostic_cartridges collector. Calibrated 2026-08-03 against real hardware: this endpoint measures under 2s idle, but /v1/library was observed once at 45s and the SCSI buffer path makes latency unpredictable from payload size. A generous ceiling costs nothing when the endpoint is fast and is the difference between a served cache and a permanently empty one.").Default("60s").Duration()
	diagnosticCartridgesInterval := kingpin.Flag("collector.diagnostic_cartridges.interval", "Background refresh interval for the diagnostic_cartridges collector. The population changes only when somebody loads or removes a cartridge, so this is deliberately unhurried.").Default("5m").Duration()
	diagnosticCartridgesPerVolser := kingpin.Flag("collector.diagnostic_cartridges.per-volser", "Emit tapelibrary_diagnostic_cartridge_info, tapelibrary_diagnostic_cartridge_last_usage_timestamp_seconds and tapelibrary_diagnostic_cartridge_lifetime_remaining_ratio, one set per diagnostic cartridge. On by default: the population is bounded by service policy rather than library capacity (five on the reference fleet), and only the per-cartridge series can name which cartridge to pull. The library-wide aggregates the alerts read are emitted regardless.").Default("true").Bool()
	diagnosticCartridgesEnabled := kingpin.Flag("collector.diagnostic_cartridges", "Enable the diagnostic_cartridges collector.").Default("true").Bool()
	factories = append(factories, instance.Factory{
		Name:    "diagnostic_cartridges",
		Enabled: diagnosticCartridgesEnabled,
		New: func(h *instance.Handle) (instance.BackgroundCollector, error) {
			c, err := h.ClientFor(*diagnosticCartridgesTimeout)
			if err != nil {
				return nil, err
			}
			return collector.NewDiagnosticCartridgesCollector(log, c, *diagnosticCartridgesInterval, *diagnosticCartridgesPerVolser), nil
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
	registry := instance.NewRegistry(log, reg, instanceLabel, enabled, *maxRequestsPerTarget, *maxQueueWait)
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
