package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/sckyzo/tapelibrary_exporter/internal/logger"
)

// logicalLibraryStats is the parsed shape of one GET /v1/logicalLibraries
// entry. The endpoint returns one element per partition configured on the
// library — two on the 2026-07-28 capture — keyed by the partition's own name,
// which is what the `logical_library` label carries verbatim.
//
// This is the first collector in this exporter with NO state field. R1.11.2
// gives /v1/logicalLibraries exactly the seven attributes below and none of
// them is an enumeration of health, so there is no stateset here and no
// emit-an-undocumented-state-anyway branch to go with it. What a logical
// library reports instead is capacity: how much of what the host was promised
// is already consumed.
//
// `name` is the key, verified against the capture rather than inherited from
// the collector written before this one (the rule node_cards established, where
// location alone turned out not to key that endpoint). R1.11.2 documents it as
// "The unique name of the logical library in the library", and the capture's
// two entries carry distinct ones.
type logicalLibraryStats struct {
	// Name is the partition's name, and the value of the logical_library
	// label. That label is already emitted by the drives collector, which
	// reads the same string from /v1/drives, so the two join directly: this
	// collector is what turns a per-drive logical_library into something a
	// query can size against the partition's own slot count.
	Name string `json:"name"`

	// MediaType is "LTO" or "3592" per R1.11.2. Identity rather than a
	// grouping key, so it lives on _info alone — the terms
	// docs/exporter-journal.md fixed when drives put logical_library on its
	// measurement series and left media_type behind.
	MediaType string `json:"mediaType"`

	// Drives is the number of drives assigned to this partition and reported
	// to the host as data-transfer-element addresses. A partition with zero
	// assigned drives is configured but unusable, so a 0 here is a real
	// reading rather than a missing one and is emitted as such.
	Drives int `json:"drives"`

	// VirtualSlots is the number of storage-element addresses the partition
	// advertises to its host application. R1.11.2: it cannot be less than the
	// number of cartridges assigned to the partition, and defaults to the
	// library's totalCapacity. That lower bound is what makes Cartridges /
	// VirtualSlots a genuine saturation ratio bounded at 1 rather than an
	// arbitrary quotient, and it is what LogicalLibraryNearlyFull reads.
	VirtualSlots int `json:"virtualSlots"`

	// VirtualIOSlots is the number of import/export-element addresses the
	// partition advertises. R1.11.2 defaults it to 255 and caps it there, and
	// requires it to be at least the number of physical I/O slots — which is
	// what ties it to the io_stations collector's own slot counts.
	VirtualIOSlots int `json:"virtualIOSlots"`

	// Cartridges is the number of cartridges currently assigned to the
	// partition. The numerator of the saturation ratio above.
	Cartridges int `json:"cartridges"`

	// EncryptionMethod is the partition's configured encryption mode: one of
	// none, systemManaged, applicationManaged, libraryManagedBarcode,
	// libraryManagedInternalLabelSelective or libraryManagedInternalLabelAll.
	// Configuration, constant per partition, so it is free on _info — the
	// same terms fc_ports put speed_setting and topology_setting there on.
	// It is emitted as a label rather than a stateset because nothing alerts
	// on a *particular* mode: what matters is that a partition's mode
	// changed, which a label makes visible as a new _info series.
	EncryptionMethod string `json:"encryptionMethod"`
}

// logicalLibrariesData is this collector's only I/O: it fetches the raw
// response body from the configured library. Kept separate from parsing
// (parseLogicalLibraries, below) so parsing stays pure and unit-testable
// without a live library.
func (c *LogicalLibrariesCollector) logicalLibrariesData(ctx context.Context) ([]byte, error) {
	return c.client.Fetch(ctx, "/logicalLibraries")
}

// parseLogicalLibraries decodes logicalLibrariesData's response body into one
// logicalLibraryStats per partition. Pure: no I/O, no logging, no side effects,
// so every input maps deterministically to an output. That is what makes it
// unit-testable with plain byte fixtures (see the test file's
// TestParseLogicalLibraries).
//
// Four inputs are rejected rather than passed through, all for the same reason
// — refresh keeps the previous cache on error, which is the right outcome for a
// response this collector cannot interpret:
//
//   - Anything that is not the documented array.
//   - An empty array. A TS4500 with no partitions serves no host application at
//     all, and the library reports that condition on its own endpoint as
//     library.status = notConfigured, which the library collector already emits
//     as a stateset. So nothing is lost by treating [] here as a response that
//     lost its content — the far likelier cause on a fleet whose five libraries
//     are all partitioned — rather than as a de-partitioned library. Accepting
//     it would replace a good cache with nothing.
//   - An entry with no name. It would emit a series labelled logical_library=""
//     that no operator can trace back to any partition, and that would silently
//     fail to join against the drives collector's own logical_library.
//   - Two entries sharing one name. R1.11.2 documents the name as unique and
//     the capture agrees, so a duplicate would send two metrics sharing a
//     descriptor AND a label set, and Registry.Gather rejects the WHOLE scrape
//     when that happens, not just the offending series. Failing closed here
//     keeps a malformed response from taking out every other collector's
//     metrics too (see CONTRIBUTING.md, "Common Pitfalls").
func parseLogicalLibraries(b []byte) ([]logicalLibraryStats, error) {
	var entries []logicalLibraryStats
	if err := json.Unmarshal(b, &entries); err != nil {
		return nil, fmt.Errorf("parse logical libraries response: %w", err)
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("parse logical libraries response: empty array, want at least one logical library")
	}

	seen := make(map[string]struct{}, len(entries))
	// Indexed rather than ranged by value: logicalLibraryStats carries three
	// strings and four ints, so copying one per iteration is what gocritic's
	// rangeValCopy flags. refresh below takes the address for the same reason.
	for i := range entries {
		e := &entries[i]
		if e.Name == "" {
			return nil, fmt.Errorf("parse logical libraries response: entry with an empty name")
		}
		if _, dup := seen[e.Name]; dup {
			return nil, fmt.Errorf("parse logical libraries response: duplicate name %q", e.Name)
		}
		seen[e.Name] = struct{}{}
	}
	return entries, nil
}

// logicalLibrariesGetMetrics is the glue between the I/O step
// (logicalLibrariesData) and the pure parsing step (parseLogicalLibraries): the
// shape every collector in this exporter follows, regardless of flavor. refresh,
// below, calls this on its own background schedule; nothing else in this file
// calls the library directly.
func (c *LogicalLibrariesCollector) logicalLibrariesGetMetrics(ctx context.Context) ([]logicalLibraryStats, error) {
	data, err := c.logicalLibrariesData(ctx)
	if err != nil {
		return nil, err
	}
	return parseLogicalLibraries(data)
}

// LogicalLibrariesCollector reads GET /v1/logicalLibraries: the partitioning of
// the physical library into the logical libraries each host application
// actually sees. A partition is what a backup application mounts, and its
// virtual slot count is the promise the library made to that application — so a
// partition whose assigned cartridges have caught up with its virtual slots
// stops accepting imports while the physical library still reports thousands of
// free slots. No other collector in this exporter can see that: the slots
// collector counts the physical library and the drives collector knows only
// which partition each drive belongs to.
//
// It is the background-refresh variant, which on this target model is not a
// choice: a scrape serves N libraries through one /metrics and must never block
// on a machine that has gone away. A background goroutine (started by Start,
// below) refreshes a cached metric slice on a fixed interval, and Collect only
// ever reads that cache under mu.
type LogicalLibrariesCollector struct {
	client   *Client
	interval time.Duration
	log      *logger.Logger

	drives          *prometheus.Desc
	virtualSlots    *prometheus.Desc
	virtualIOSlots  *prometheus.Desc
	cartridges      *prometheus.Desc
	info            *prometheus.Desc
	lastRefreshDesc *prometheus.Desc

	// mu guards cached and lastRefresh: refresh (below) writes them from the
	// background goroutine started by Start, Collect reads them from
	// whichever goroutine calls it (a Prometheus scrape). RWMutex, not a
	// plain Mutex, because Collect only ever reads.
	mu          sync.RWMutex
	cached      []prometheus.Metric
	lastRefresh time.Time

	// done is closed when the background goroutine launched by Start exits.
	// main.go waits on Done() (via instance.BackgroundCollector) after the
	// HTTP server has shut down, so the process doesn't exit mid-refresh.
	done chan struct{}
}

// NewLogicalLibrariesCollector builds the collector and its Descs. It is pure:
// it starts no goroutine and performs no I/O, which is what makes it
// constructible in tests with no background refresh running. Call Start once,
// after construction, to begin refreshing.
func NewLogicalLibrariesCollector(log *logger.Logger, client *Client, interval time.Duration) *LogicalLibrariesCollector {
	return &LogicalLibrariesCollector{
		client:   client,
		interval: interval,
		log:      log,
		drives: prometheus.NewDesc(
			"tapelibrary_logical_library_drives",
			"Number of drives assigned to the logical library and reported to its host application as data transfer element addresses.",
			[]string{"logical_library"}, nil,
		),
		virtualSlots: prometheus.NewDesc(
			"tapelibrary_logical_library_virtual_slots",
			"Number of virtual storage slots the logical library reports to its host application as storage element addresses. The library will not let this fall below the assigned cartridge count, so it is the ceiling that count saturates against.",
			[]string{"logical_library"}, nil,
		),
		virtualIOSlots: prometheus.NewDesc(
			"tapelibrary_logical_library_virtual_io_slots",
			"Number of virtual I/O slots the logical library reports to its host application as import/export element addresses. Capped at 255 by the library, and never fewer than the physical I/O slots behind it.",
			[]string{"logical_library"}, nil,
		),
		cartridges: prometheus.NewDesc(
			"tapelibrary_logical_library_cartridges",
			"Number of cartridges currently assigned to the logical library. Divide by tapelibrary_logical_library_virtual_slots for the partition's saturation: at 1 it can accept no further imports, however much free space the physical library still reports.",
			[]string{"logical_library"}, nil,
		),
		info: prometheus.NewDesc(
			"tapelibrary_logical_library_info",
			"Logical library identity and configuration, always 1. Identity strings live here rather than on a measurement series, so reconfiguring a partition's encryption changes this series alone instead of breaking the continuity of its capacity counts.",
			[]string{"logical_library", "media_type", "encryption_method"}, nil,
		),
		lastRefreshDesc: prometheus.NewDesc(
			"tapelibrary_logical_libraries_last_refresh_timestamp_seconds",
			"Unix time of the last successful logical libraries refresh. Alert if time() - this > 2 x the collector's configured interval.",
			nil, nil,
		),
		done: make(chan struct{}),
	}
}

// Start launches the background refresh goroutine. Call once, after
// construction. The first refresh runs immediately (so the cache starts filling
// as soon as the process starts) without Start itself waiting for it: a slow
// first fetch never blocks process startup. The goroutine exits when ctx is
// cancelled; Done() can then be used to wait for it to finish.
func (c *LogicalLibrariesCollector) Start(ctx context.Context) {
	go func() {
		defer close(c.done)
		c.refresh(ctx)
		ticker := time.NewTicker(c.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				c.refresh(ctx)
			case <-ctx.Done():
				return
			}
		}
	}()
}

// Done returns a channel that is closed when the background goroutine started
// by Start has fully exited. main.go's shutdown seam waits on this (bounded, so
// a stuck refresh can't hang process exit forever) after the HTTP server itself
// has stopped.
func (c *LogicalLibrariesCollector) Done() <-chan struct{} {
	return c.done
}

// refresh performs the one I/O call (logicalLibrariesGetMetrics, via the
// injected *Client) and, on success, atomically replaces the cache. On error it
// logs and returns, leaving the previous cache and lastRefresh untouched,
// fail-open: a transient failure serves the last-known-good data instead of
// dropping the series, and the freshness gauge is the signal that a refresh is
// stale, not a dropped scrape.
//
// Every one of the five series is emitted for every partition, unconditionally.
// Unlike the nullable fields elsewhere in this exporter (accessors.temperature,
// fcPorts.speedActual, ioStations.magazine), none of these can be absent for a
// reason that would make a 0 a lie: R1.11.2 types all four as plain numbers,
// and a partition genuinely holding zero cartridges, or genuinely having zero
// drives assigned, is reporting a real and rather alarming reading rather than
// declining to report one.
func (c *LogicalLibrariesCollector) refresh(ctx context.Context) {
	libraries, err := c.logicalLibrariesGetMetrics(ctx)
	if err != nil {
		c.log.Error("Failed to refresh logical libraries metrics: keeping previous cache", "err", err)
		return
	}

	// Per partition: four capacity gauges and the _info series.
	const perLibrary = 5
	metrics := make([]prometheus.Metric, 0, len(libraries)*perLibrary)

	// Indexed rather than ranged by value: logicalLibraryStats is wide enough
	// that taking the address avoids copying it per iteration.
	for i := range libraries {
		l := &libraries[i]

		metrics = append(metrics,
			prometheus.MustNewConstMetric(
				c.drives, prometheus.GaugeValue, float64(l.Drives), l.Name),
			prometheus.MustNewConstMetric(
				c.virtualSlots, prometheus.GaugeValue, float64(l.VirtualSlots), l.Name),
			prometheus.MustNewConstMetric(
				c.virtualIOSlots, prometheus.GaugeValue, float64(l.VirtualIOSlots), l.Name),
			prometheus.MustNewConstMetric(
				c.cartridges, prometheus.GaugeValue, float64(l.Cartridges), l.Name),
			prometheus.MustNewConstMetric(
				c.info, prometheus.GaugeValue, 1, l.Name, l.MediaType, l.EncryptionMethod),
		)
	}

	c.mu.Lock()
	c.cached = metrics
	c.lastRefresh = time.Now()
	c.mu.Unlock()
}

// Describe sends every one of this collector's descriptors, including the
// freshness gauge. Constant regardless of scrape or refresh outcome, which is
// what makes prometheus.DescribeByCollect unnecessary here.
func (c *LogicalLibrariesCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.drives
	ch <- c.virtualSlots
	ch <- c.virtualIOSlots
	ch <- c.cartridges
	ch <- c.info
	ch <- c.lastRefreshDesc
}

// Collect replays the cached metrics from the last successful refresh:
// O(cached size), never touches the library, never blocks on it. It then ALWAYS
// sends the freshness gauge, even before any refresh has ever completed (value
// 0 in that case, since a zero time.Time's Unix() is a large negative, not a
// meaningful "ancient" epoch marker). A collector must never emit zero metrics
// on what it considers a healthy outcome: StatusTracker counts emitted metrics
// per scrape, and an empty cache before the first refresh completes is a normal
// startup window, not a failure. The freshness gauge alone guarantees at least
// one metric every scrape, so StatusTracker reports this collector as alive;
// the gauge's own value (0 until the first refresh lands) is the separate,
// correct signal for staleness.
func (c *LogicalLibrariesCollector) Collect(ch chan<- prometheus.Metric) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	for _, m := range c.cached {
		ch <- m
	}

	refreshUnix := 0.0
	if !c.lastRefresh.IsZero() {
		refreshUnix = float64(c.lastRefresh.Unix())
	}
	ch <- prometheus.MustNewConstMetric(c.lastRefreshDesc, prometheus.GaugeValue, refreshUnix)
}
