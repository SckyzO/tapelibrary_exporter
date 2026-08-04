package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/sckyzo/tapelibrary_exporter/internal/logger"
)

// fcPortStates is the full set of values GET /v1/fcPorts documents for its
// "state" field (TS4500 R1.11.2, "Fibre Channel ports"). Every one of them is
// emitted per port on every refresh as its own series, exactly one carrying 1
// and the rest 0: the stateset encoding this exporter uses for every
// enumeration (see docs/exporter-journal.md, "Enum encoding").
//
// Order is the manual's own. Registry.Gather sorts the exposition output by
// label value regardless, so this slice's order is invisible to a scrape and is
// kept as-is to stay diffable against the manual.
var fcPortStates = []string{
	"unknown",
	"noLightDetected",
	"communicationNotEstablished",
	"communicationEstablished",
}

// fcPortSpeeds maps the speed strings R1.11.2 documents for speedSetting and
// speedActual onto the nominal signalling rate in BYTES per second, converted
// at 8 bits per byte.
//
// Two things this deliberately is not:
//
//   - It is not achievable payload throughput. 16GFC signals at 16 Gbit/s but
//     moves ~1600 MB/s of payload, not the 2000 MB/s below, because of 64b/66b
//     line encoding and FC framing overhead. The value here is the negotiated
//     LINK RATE, which is what the library reports and what a SAN engineer
//     means by "this port came up at 16 gig"; the help text on the descriptor
//     says so explicitly so nobody builds a utilisation ratio on it.
//   - It is not exhaustive of what the wire sends. The 2026-07-28 capture
//     returns the literal string "unknown" on 36 of 80 ports, a value R1.11.2
//     tabulates for neither speed field. Anything absent from this map emits no
//     speed series at all rather than a 0 — see speedBytesPerSecond below.
//
// Bytes rather than bits because Prometheus's own naming conventions take bytes
// as the base unit, and this exporter's metric-name shape (see
// docs/exporter-journal.md, "Metric name shape") already reserves `_bytes` for
// exactly this and lists no `_bits`.
var fcPortSpeeds = map[string]float64{
	"1Gbps":  1e9 / 8,
	"2Gbps":  2e9 / 8,
	"4Gbps":  4e9 / 8,
	"8Gbps":  8e9 / 8,
	"16Gbps": 16e9 / 8,
}

// fcPortStats is the parsed shape of one GET /v1/fcPorts entry. The endpoint
// returns one element per physical port — two per drive on this hardware, 80
// across the 40 drives of the 2026-07-28 capture — keyed by the library's own
// native location string (fcPort_F1C4R1P0, fcPort_F1C4R1P1, ...), which is what
// the `location` label carries verbatim so a value in a dashboard can be pasted
// straight into the library's GUI.
//
// portNumber is on the wire and deliberately NOT declared here, for two
// reasons that reinforce each other. It is already the last character of
// location (...P0 / ...P1), so a label would restate what an operator reads off
// the location anyway. And it is the one field where R1.11.2's attribute table
// and the capture disagree outright: the manual types it "(string)" while every
// entry in the capture sends a bare JSON number. Declaring it as either type
// would decode-fail against the other, so the field this collector has no use
// for is also the field that would cost it a firmware-dependent parse error.
// Logged under "Open questions / assumptions" in the journal.
type fcPortStats struct {
	Location string `json:"location"`
	State    string `json:"state"`

	// DriveLocation is the location of the drive this port is installed on,
	// and the join key between this collector and the drives collector: a
	// port going dark matters enormously if its drive is online and not at
	// all if the drive is in service mode. It rides on the measurement
	// series below rather than on _info alone, on the same terms
	// logical_library already does on drives — it is a grouping key, not an
	// identity string, it is constant per port so it costs no extra series,
	// and putting it there is what lets FCPortNoLight be a plain join
	// instead of a group_left against _info.
	DriveLocation string `json:"driveLocation"`

	// DriveSn is the serial of the drive this port belongs to, not of the
	// port (a port has none of its own). Spelled drive_serial as a label,
	// extending the _info vocabulary's `serial` for a resource whose only
	// serial belongs to its parent. R1.11.2 documents it as a supported
	// filter on this endpoint, so operators already think in these terms.
	DriveSn string `json:"driveSn"`

	// WWPN is the port's globally unique worldwide port name: the identifier
	// the SAN fabric itself zones on, and the one string that lets a switch
	// port be tied back to a library port with no library-side knowledge at
	// all. An identity string, so it lives on _info.
	WWPN string `json:"wwpn"`

	// SpeedSetting is the configured rate: "auto" on 79 of the capture's 80
	// ports, an explicit rate on the remaining one. It is what
	// FCPortSpeedBelowPeers filters on, so it must be joinable — hence a
	// label on _info rather than a second gauge, which would be absent for
	// exactly the "auto" ports the rule cares about.
	SpeedSetting string `json:"speedSetting"`

	// SpeedActual is the rate actually negotiated with the fabric. Mapped
	// through fcPortSpeeds into a real gauge; "unknown" and anything else
	// unmapped emit nothing.
	SpeedActual string `json:"speedActual"`

	// TopologySetting is the configured topology ("auto-L" on every port in
	// the capture). Configuration, constant per port, so it is free on
	// _info.
	TopologySetting string `json:"topologySetting"`

	// TopologyActual is the topology actually negotiated. It is the one
	// _info label here that is not constant: it flips between its real value
	// and "unknown" as the link comes and goes. That churn is bounded at two
	// label sets per port over the port's life — nothing like the
	// accumulating index a volser label would build — and it is kept because
	// a port that comes up "L-Port" where its peers came up "N-Port" is a
	// real fabric misconfiguration that no other field on this endpoint
	// reports.
	TopologyActual string `json:"topologyActual"`

	// LoopID is the port's SCSI arbitrated-loop physical address, which
	// R1.11.2 defines as identifying the drive's position in the library. A
	// stable per-port identifier used in SAN troubleshooting, so it rides on
	// _info as a label rather than becoming a gauge nobody would do
	// arithmetic on.
	LoopID int `json:"loopID"`
}

// fcPortsData is this collector's only I/O: it fetches the raw response body
// from the configured library. Kept separate from parsing (parseFCPorts, below)
// so parsing stays pure and unit-testable without a live library.
func (c *FCPortsCollector) fcPortsData(ctx context.Context) ([]byte, error) {
	return c.client.Fetch(ctx, "/fcPorts")
}

// parseFCPorts decodes fcPortsData's response body into one fcPortStats per
// physical port. Pure: no I/O, no logging, no side effects, so every input maps
// deterministically to an output. That is what makes it unit-testable with
// plain byte fixtures (see the test file's TestParseFCPorts).
//
// Four inputs are rejected rather than passed through, all for the same reason
// — refresh keeps the previous cache on error, which is the right outcome for a
// response this collector cannot interpret:
//
//   - Anything that is not the documented array.
//   - An empty array. Every TS4500 drive ships with Fibre Channel ports and the
//     library has drives by definition, so an empty list is a response that lost
//     its content, not a library with no FC ports. (An empty list is what
//     /v1/sasPorts legitimately returns on this fibre-channel hardware; this
//     endpoint is not that one.)
//   - An entry with no location. It would emit a series labelled location=""
//     that no operator can trace back to any hardware.
//   - Two entries sharing one location. Unlike node_cards, where an accessor
//     carries two cards at one location and the key had to widen to
//     location+card_type, location is unique across all 80 entries of the
//     2026-07-28 capture — the port number is already baked into it (...P0 /
//     ...P1), which is what makes the drive's two ports distinct here. That was
//     verified against the capture rather than inherited from the collector
//     written before this one, per the key rule in docs/exporter-journal.md. So
//     a duplicate would send two metrics with the same descriptor and the same
//     label set, and Registry.Gather rejects the WHOLE scrape when that happens,
//     not just the offending series. Failing closed here keeps a malformed
//     response from taking out every other collector's metrics too (see
//     CONTRIBUTING.md, "Common Pitfalls").
func parseFCPorts(b []byte) ([]fcPortStats, error) {
	var entries []fcPortStats
	if err := json.Unmarshal(b, &entries); err != nil {
		return nil, fmt.Errorf("parse fc ports response: %w", err)
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("parse fc ports response: empty array, want at least one Fibre Channel port")
	}

	seen := make(map[string]struct{}, len(entries))
	// Indexed rather than ranged by value: fcPortStats is a wide struct (nine
	// string fields plus the loop ID), so copying one per iteration is what
	// gocritic's rangeValCopy flags. refresh below takes the address for the
	// same reason.
	for i := range entries {
		e := &entries[i]
		if e.Location == "" {
			return nil, fmt.Errorf("parse fc ports response: entry with an empty location")
		}
		if _, dup := seen[e.Location]; dup {
			return nil, fmt.Errorf("parse fc ports response: duplicate location %q", e.Location)
		}
		seen[e.Location] = struct{}{}
	}
	return entries, nil
}

// fcPortsGetMetrics is the glue between the I/O step (fcPortsData) and the pure
// parsing step (parseFCPorts): the shape every collector in this exporter
// follows, regardless of flavor. refresh, below, calls this on its own
// background schedule; nothing else in this file calls the library directly.
func (c *FCPortsCollector) fcPortsGetMetrics(ctx context.Context) ([]fcPortStats, error) {
	data, err := c.fcPortsData(ctx)
	if err != nil {
		return nil, err
	}
	return parseFCPorts(data)
}

// FCPortsCollector reads GET /v1/fcPorts: the link state, negotiated speed and
// SAN identity of every Fibre Channel port on every drive in the library. These
// ports are the path every read, write and library-control command takes
// between the host and the drive, so a port that has gone dark removes a drive
// from a host's view while the drive itself keeps reporting perfectly healthy
// on /v1/drives — a failure mode no other collector in this exporter can see.
//
// It is the background-refresh variant, which on this target model is not a
// choice: a scrape serves N libraries through one /metrics and must never block
// on a machine that has gone away. A background goroutine (started by Start,
// below) refreshes a cached metric slice on a fixed interval, and Collect only
// ever reads that cache under mu.
type FCPortsCollector struct {
	client   *Client
	interval time.Duration
	log      *logger.Logger

	state           *prometheus.Desc
	speed           *prometheus.Desc
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

// NewFCPortsCollector builds the collector and its Descs. It is pure: it starts
// no goroutine and performs no I/O, which is what makes it constructible in
// tests with no background refresh running. Call Start once, after
// construction, to begin refreshing.
func NewFCPortsCollector(log *logger.Logger, client *Client, interval time.Duration) *FCPortsCollector {
	return &FCPortsCollector{
		client:   client,
		interval: interval,
		log:      log,
		// drive_location rides on the stateset rather than living on _info
		// alone, so FCPortNoLight can join this straight against
		// tapelibrary_drive_state instead of pulling the drive through a
		// group_left. Constant per port, so it costs no extra series.
		state: prometheus.NewDesc(
			"tapelibrary_fc_port_state",
			"Link state of the Fibre Channel port, as a stateset: 1 on the active state and 0 on every other known state.",
			[]string{"location", "drive_location", "state"}, nil,
		),
		// Nominal link rate, NOT achievable payload throughput: 16GFC signals
		// at 16 Gbit/s and moves roughly 1600 MB/s after 64b/66b encoding, so
		// dividing traffic by this value does not give a utilisation ratio.
		speed: prometheus.NewDesc(
			"tapelibrary_fc_port_speed_bytes_per_second",
			"Negotiated link rate of the Fibre Channel port, in bytes per second, converted from the library's own Gbps figure at 8 bits per byte. This is the nominal signalling rate, not achievable payload throughput. A port reporting a rate this exporter cannot map (the capture's undocumented \"unknown\" on every dark port) produces no series rather than a 0 asserting a link running at zero.",
			[]string{"location", "drive_location"}, nil,
		),
		info: prometheus.NewDesc(
			"tapelibrary_fc_port_info",
			"Fibre Channel port identity and configuration, always 1. Identity strings live here rather than on a measurement series, so a drive swap or a re-negotiated topology changes this series alone instead of breaking the continuity of the link state and speed.",
			[]string{"location", "drive_location", "drive_serial", "wwpn", "speed_setting", "topology_setting", "topology_actual", "loop_id"}, nil,
		),
		lastRefreshDesc: prometheus.NewDesc(
			"tapelibrary_fc_ports_last_refresh_timestamp_seconds",
			"Unix time of the last successful fc ports refresh. Alert if time() - this > 2 x the collector's configured interval.",
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
func (c *FCPortsCollector) Start(ctx context.Context) {
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
func (c *FCPortsCollector) Done() <-chan struct{} {
	return c.done
}

// stateset appends one series per documented state for p, plus the observed one
// if the manual does not document it. The manual's tables are demonstrably a
// floor rather than a ceiling (it names states in prose that appear in no table,
// and this endpoint's own speedActual returns an "unknown" tabulated nowhere),
// so an unlisted state must surface as its own series rather than silently leave
// every series at 0 and make the port look stateless. `known` is what keeps that
// extra series from duplicating a label set already emitted above, which would
// fail Gather for this collector's whole scrape.
func (c *FCPortsCollector) stateset(metrics []prometheus.Metric, p *fcPortStats) []prometheus.Metric {
	known := false
	for _, s := range fcPortStates {
		value := 0.0
		if s == p.State {
			value = 1.0
			known = true
		}
		metrics = append(metrics, prometheus.MustNewConstMetric(
			c.state, prometheus.GaugeValue, value, p.Location, p.DriveLocation, s))
	}
	if !known && p.State != "" {
		c.log.Warn("Fibre Channel port reported a state absent from the documented set: emitting it anyway",
			"location", p.Location, "state", p.State)
		metrics = append(metrics, prometheus.MustNewConstMetric(
			c.state, prometheus.GaugeValue, 1, p.Location, p.DriveLocation, p.State))
	}
	return metrics
}

// speedBytesPerSecond returns the port's negotiated link rate in bytes per
// second and whether it should be emitted at all.
//
// An unmapped value produces no series rather than a 0, for the same reason the
// io_stations collector emits no door gauge for a station with no door: a 0
// here asserts a link that is up and running at zero bytes per second, which is
// not a state Fibre Channel has. The capture's 36 dark ports all report the
// literal "unknown", so this is the ordinary case on real hardware rather than
// an error path — hence no warning log for it, only for a value that is neither
// "unknown" nor a rate this exporter knows.
func (c *FCPortsCollector) speedBytesPerSecond(p *fcPortStats) (float64, bool) {
	if bps, ok := fcPortSpeeds[p.SpeedActual]; ok {
		return bps, true
	}
	if p.SpeedActual != "" && p.SpeedActual != "unknown" {
		c.log.Warn("Fibre Channel port reported an unrecognized speed: emitting no speed series for it",
			"location", p.Location, "speedActual", p.SpeedActual)
	}
	return 0, false
}

// refresh performs the one I/O call (fcPortsGetMetrics, via the injected
// *Client) and, on success, atomically replaces the cache. On error it logs and
// returns, leaving the previous cache and lastRefresh untouched, fail-open: a
// transient failure serves the last-known-good data instead of dropping the
// series, and the freshness gauge is the signal that a refresh is stale, not a
// dropped scrape.
func (c *FCPortsCollector) refresh(ctx context.Context) {
	ports, err := c.fcPortsGetMetrics(ctx)
	if err != nil {
		c.log.Error("Failed to refresh fc ports metrics: keeping previous cache", "err", err)
		return
	}

	// Per port: the stateset, the _info series, and at most one speed gauge.
	perPort := len(fcPortStates) + 2
	metrics := make([]prometheus.Metric, 0, len(ports)*perPort)

	// Indexed rather than ranged by value: fcPortStats is a wide struct and
	// taking the address avoids copying it per iteration.
	for i := range ports {
		p := &ports[i]

		metrics = c.stateset(metrics, p)

		if bps, ok := c.speedBytesPerSecond(p); ok {
			metrics = append(metrics, prometheus.MustNewConstMetric(
				c.speed, prometheus.GaugeValue, bps, p.Location, p.DriveLocation))
		}

		// Always emitted, on every port and in every state: this is the
		// series FCPortSpeedBelowPeers joins against to find the
		// auto-negotiating ports, so a port dropping out of it would
		// silently drop out of the rule too.
		metrics = append(metrics, prometheus.MustNewConstMetric(
			c.info, prometheus.GaugeValue, 1,
			p.Location, p.DriveLocation, p.DriveSn, p.WWPN,
			p.SpeedSetting, p.TopologySetting, p.TopologyActual,
			strconv.Itoa(p.LoopID)))
	}

	c.mu.Lock()
	c.cached = metrics
	c.lastRefresh = time.Now()
	c.mu.Unlock()
}

// Describe sends every one of this collector's descriptors, including the
// freshness gauge. Constant regardless of scrape or refresh outcome, which is
// what makes prometheus.DescribeByCollect unnecessary here.
func (c *FCPortsCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.state
	ch <- c.speed
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
func (c *FCPortsCollector) Collect(ch chan<- prometheus.Metric) {
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
