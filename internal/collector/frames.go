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

// frameStates is the full set of values GET /v1/frames documents for its
// "state" field (TS4500 R1.11.2, "URL endpoints and resources"). Every one of
// them is emitted per frame on every refresh as its own series, exactly one
// carrying 1 and the rest 0: the stateset encoding this exporter uses for
// every enumeration (see docs/exporter-journal.md, "Enum encoding").
//
// Door position is deliberately NOT in this list. The legacy RoS scripts in
// samples/legacy/ classify frontDoorOpen, rearDoorOpen and sideDoorOpen as
// frame states, and on R1.11.2 they are not states at all — those selectors
// can never have matched, so that alerting has been silently dead. The door
// signal is real but lives in the separate frontDoor/rearDoor/sideDoor
// fields, which is what tapelibrary_frame_door_open reads.
//
// Order is the manual's own, not alphabetical; Registry.Gather sorts the
// exposition output by label value regardless, so this slice's order is
// invisible to a scrape and kept as-is to stay diffable against the manual.
var frameStates = []string{
	"unknown",
	"frontDoorOpenWhileNotAllowed",
	"acUnreachable",
	"calibrationRequired",
	"inventoryPending",
	"normal",
}

// frameStats is the parsed shape of one GET /v1/frames entry. The endpoint
// returns one element per physical frame, keyed by the library's own native
// location string (frame_F1, frame_F2, ...), which is what the `location`
// label carries verbatim so a value in a dashboard can be pasted straight
// into the library's GUI.
//
// Only the fields this collector emits are declared. The response also
// carries frontDoorLastChanged / rearDoorLastChanged / sideDoorLastChanged,
// which are left out: the door's current position plus an alert's own `for:`
// clause already express "this door has been open too long", and a
// last-changed timestamp per door would add a third metric family per frame
// for no signal the door gauge does not already give.
//
// The three door fields are plain strings rather than pointers on purpose:
// encoding/json leaves a string field untouched when the wire value is null,
// so a frame with no rear door decodes to "" — distinguishable from both
// "open" and "closed" without a nil check at every use.
type frameStats struct {
	Location   string  `json:"location"`
	State      string  `json:"state"`
	Type       string  `json:"type"`
	MTM        string  `json:"mtm"`
	SN         string  `json:"sn"`
	MediaType  string  `json:"mediaType"`
	FrontDoor  string  `json:"frontDoor"`
	RearDoor   string  `json:"rearDoor"`
	SideDoor   string  `json:"sideDoor"`
	Slots      float64 `json:"slots"`
	Cartridges float64 `json:"cartridges"`
	Drives     float64 `json:"drives"`
	IOStations float64 `json:"ioStations"`
}

// framesData is this collector's only I/O: it fetches the raw response body
// from the configured library. Kept separate from parsing (parseFrames,
// below) so parsing stays pure and unit-testable without a live library.
func (c *FramesCollector) framesData(ctx context.Context) ([]byte, error) {
	return c.client.Fetch(ctx, "/frames")
}

// parseFrames decodes framesData's response body into one frameStats per
// physical frame. Pure: no I/O, no logging, no side effects, so every input
// maps deterministically to an output. That is what makes it unit-testable
// with plain byte fixtures (see the test file's TestParseFrames).
//
// Three inputs are rejected rather than passed through, all for the same
// reason — refresh keeps the previous cache on error, which is the right
// outcome for a response this collector cannot interpret:
//
//   - An empty array. Every TS4500 has at least a base frame, so an empty
//     list is a response that lost its content, not a library with no frames.
//   - An entry with no location. It would emit a series labelled location=""
//     that no operator can trace back to any hardware.
//   - Two entries sharing one location. Every metric below is keyed by
//     location alone, so a duplicate would send two metrics with the same
//     descriptor and the same label set, and Registry.Gather rejects the
//     WHOLE scrape when that happens, not just the offending series. Failing
//     closed here keeps a malformed response from taking out every other
//     collector's metrics too (see CONTRIBUTING.md, "Common Pitfalls").
func parseFrames(b []byte) ([]frameStats, error) {
	var entries []frameStats
	if err := json.Unmarshal(b, &entries); err != nil {
		return nil, fmt.Errorf("parse frames response: %w", err)
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("parse frames response: empty array, want at least one frame")
	}

	// Indexed rather than ranged by value: frameStats is wide enough that
	// copying one per iteration is what gocritic's rangeValCopy flags.
	seen := make(map[string]struct{}, len(entries))
	for i := range entries {
		e := &entries[i]
		if e.Location == "" {
			return nil, fmt.Errorf("parse frames response: entry with an empty location")
		}
		if _, dup := seen[e.Location]; dup {
			return nil, fmt.Errorf("parse frames response: duplicate location %q", e.Location)
		}
		seen[e.Location] = struct{}{}
	}
	return entries, nil
}

// framesGetMetrics is the glue between the I/O step (framesData) and the pure
// parsing step (parseFrames): the shape every collector in this exporter
// follows, regardless of flavor. refresh, below, calls this on its own
// background schedule; nothing else in this file calls the library directly.
func (c *FramesCollector) framesGetMetrics(ctx context.Context) ([]frameStats, error) {
	data, err := c.framesData(ctx)
	if err != nil {
		return nil, err
	}
	return parseFrames(data)
}

// FramesCollector reads GET /v1/frames: the operational state of every
// physical frame in the library, its door positions, and how much of the
// library's slots, cartridges, drives and I/O stations each frame accounts
// for. It is the background-refresh variant, which on this target model is
// not a choice: a scrape serves N libraries through one /metrics and must
// never block on a machine that has gone away. A background goroutine
// (started by Start, below) refreshes a cached metric slice on a fixed
// interval, and Collect only ever reads that cache under mu.
type FramesCollector struct {
	client   *Client
	interval time.Duration
	log      *logger.Logger

	state               *prometheus.Desc
	doorOpen            *prometheus.Desc
	slotsCapacity       *prometheus.Desc
	cartridgesPresent   *prometheus.Desc
	drivesInstalled     *prometheus.Desc
	ioStationsInstalled *prometheus.Desc
	info                *prometheus.Desc
	lastRefreshDesc     *prometheus.Desc

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

// NewFramesCollector builds the collector and its Descs. It is pure: it
// starts no goroutine and performs no I/O, which is what makes it
// constructible in tests with no background refresh running. Call Start
// once, after construction, to begin refreshing.
func NewFramesCollector(log *logger.Logger, client *Client, interval time.Duration) *FramesCollector {
	return &FramesCollector{
		client:   client,
		interval: interval,
		log:      log,
		state: prometheus.NewDesc(
			"tapelibrary_frame_state",
			"Operational state of the frame, as a stateset: 1 on the active state and 0 on every other known state.",
			[]string{"location", "state"}, nil,
		),
		doorOpen: prometheus.NewDesc(
			"tapelibrary_frame_door_open",
			"Whether the frame's door is open (1) or closed (0). A door position the frame does not physically have reports no series at all, rather than a 0 claiming a closed door that does not exist.",
			[]string{"location", "door"}, nil,
		),
		slotsCapacity: prometheus.NewDesc(
			"tapelibrary_frame_slots_capacity",
			"Number of cartridge slots this frame physically holds.",
			[]string{"location"}, nil,
		),
		cartridgesPresent: prometheus.NewDesc(
			"tapelibrary_frame_cartridges_present",
			"Number of cartridges currently present in this frame.",
			[]string{"location"}, nil,
		),
		drivesInstalled: prometheus.NewDesc(
			"tapelibrary_frame_drives_installed",
			"Number of tape drives installed in this frame.",
			[]string{"location"}, nil,
		),
		ioStationsInstalled: prometheus.NewDesc(
			"tapelibrary_frame_io_stations_installed",
			"Number of I/O stations installed in this frame.",
			[]string{"location"}, nil,
		),
		info: prometheus.NewDesc(
			"tapelibrary_frame_info",
			"Frame identity, always 1. Identity strings live here rather than on a measurement series, so replacing a frame changes this series alone instead of breaking the continuity of every other.",
			[]string{"location", "serial", "mtm", "frame_type", "media_type"}, nil,
		),
		lastRefreshDesc: prometheus.NewDesc(
			"tapelibrary_frames_last_refresh_timestamp_seconds",
			"Unix time of the last successful frames refresh. Alert if time() - this > 2 x the collector's configured interval.",
			nil, nil,
		),
		done: make(chan struct{}),
	}
}

// Start launches the background refresh goroutine. Call once, after
// construction. The first refresh runs immediately (so the cache starts
// filling as soon as the process starts) without Start itself waiting for
// it: a slow first fetch never blocks process startup. The goroutine exits
// when ctx is cancelled; Done() can then be used to wait for it to finish.
func (c *FramesCollector) Start(ctx context.Context) {
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

// Done returns a channel that is closed when the background goroutine
// started by Start has fully exited. main.go's shutdown seam waits on this
// (bounded, so a stuck refresh can't hang process exit forever) after the
// HTTP server itself has stopped.
func (c *FramesCollector) Done() <-chan struct{} {
	return c.done
}

// refresh performs the one I/O call (framesGetMetrics, via the injected
// *Client) and, on success, atomically replaces the cache. On error it logs
// and returns, leaving the previous cache and lastRefresh untouched,
// fail-open: a transient failure serves the last-known-good data instead of
// dropping the series, and the freshness gauge is the signal that a refresh
// is stale, not a dropped scrape.
func (c *FramesCollector) refresh(ctx context.Context) {
	frames, err := c.framesGetMetrics(ctx)
	if err != nil {
		c.log.Error("Failed to refresh frames metrics: keeping previous cache", "err", err)
		return
	}

	// Per frame: the stateset, up to three door gauges, four counts and one
	// info series.
	metrics := make([]prometheus.Metric, 0, len(frames)*(len(frameStates)+8))

	// Indexed rather than ranged by value, same reason as parseFrames above.
	for i := range frames {
		f := &frames[i]
		// The stateset: one series per documented state, plus the observed
		// one if the manual does not document it. The manual's tables are
		// demonstrably a floor rather than a ceiling (it names states in
		// prose that appear in no table), so an unlisted state must surface
		// as its own series rather than silently leave every series at 0 and
		// make the frame look stateless. `known` is what keeps that extra
		// series from duplicating a label set already emitted above, which
		// would fail Gather for this collector's whole scrape.
		known := false
		for _, s := range frameStates {
			value := 0.0
			if s == f.State {
				value = 1.0
				known = true
			}
			metrics = append(metrics, prometheus.MustNewConstMetric(c.state, prometheus.GaugeValue, value, f.Location, s))
		}
		if !known && f.State != "" {
			c.log.Warn("Frame reported a state absent from the documented set: emitting it anyway",
				"location", f.Location, "state", f.State)
			metrics = append(metrics, prometheus.MustNewConstMetric(c.state, prometheus.GaugeValue, 1, f.Location, f.State))
		}

		for _, d := range []struct {
			label    string
			position string
		}{
			{"front", f.FrontDoor},
			{"rear", f.RearDoor},
			{"side", f.SideDoor},
		} {
			// null on the wire, so this frame has no door in this position.
			// Emitting 0 would assert a closed door that does not exist; the
			// whole capture reports rearDoor null on every frame, so this is
			// the common case, not an edge case.
			if d.position == "" {
				continue
			}
			// Anything other than the two documented positions is a value
			// this collector cannot map to open-or-closed. Reporting 0 would
			// silently assert "closed" for a state nobody has interpreted,
			// which is exactly how a door alert stops firing without anyone
			// noticing, so it surfaces as a log line and no series.
			if d.position != "open" && d.position != "closed" {
				c.log.Warn("Frame reported an unrecognized door position: emitting no series for that door",
					"location", f.Location, "door", d.label, "position", d.position)
				continue
			}
			open := 0.0
			if d.position == "open" {
				open = 1.0
			}
			metrics = append(metrics, prometheus.MustNewConstMetric(c.doorOpen, prometheus.GaugeValue, open, f.Location, d.label))
		}

		metrics = append(metrics,
			prometheus.MustNewConstMetric(c.slotsCapacity, prometheus.GaugeValue, f.Slots, f.Location),
			prometheus.MustNewConstMetric(c.cartridgesPresent, prometheus.GaugeValue, f.Cartridges, f.Location),
			prometheus.MustNewConstMetric(c.drivesInstalled, prometheus.GaugeValue, f.Drives, f.Location),
			prometheus.MustNewConstMetric(c.ioStationsInstalled, prometheus.GaugeValue, f.IOStations, f.Location),
			prometheus.MustNewConstMetric(c.info, prometheus.GaugeValue, 1, f.Location, f.SN, f.MTM, f.Type, f.MediaType),
		)
	}

	c.mu.Lock()
	c.cached = metrics
	c.lastRefresh = time.Now()
	c.mu.Unlock()
}

// Describe sends every one of this collector's descriptors, including the
// freshness gauge. Constant regardless of scrape or refresh outcome, which
// is what makes prometheus.DescribeByCollect unnecessary here.
func (c *FramesCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.state
	ch <- c.doorOpen
	ch <- c.slotsCapacity
	ch <- c.cartridgesPresent
	ch <- c.drivesInstalled
	ch <- c.ioStationsInstalled
	ch <- c.info
	ch <- c.lastRefreshDesc
}

// Collect replays the cached metrics from the last successful refresh:
// O(cached size), never touches the library, never blocks on it. It then
// ALWAYS sends the freshness gauge, even before any refresh has ever
// completed (value 0 in that case, since a zero time.Time's Unix() is a
// large negative, not a meaningful "ancient" epoch marker). A collector must
// never emit zero metrics on what it considers a healthy outcome:
// StatusTracker counts emitted metrics per scrape, and an empty cache before
// the first refresh completes is a normal startup window, not a failure. The
// freshness gauge alone guarantees at least one metric every scrape, so
// StatusTracker reports this collector as alive; the gauge's own value (0
// until the first refresh lands) is the separate, correct signal for
// staleness.
func (c *FramesCollector) Collect(ch chan<- prometheus.Metric) {
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
