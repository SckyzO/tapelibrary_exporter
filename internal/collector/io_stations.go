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

// ioStationStates is the full set of values GET /v1/ioStations documents for
// its "state" field (TS4500 R1.11.2, "I/O stations"). Every one of them is
// emitted per station on every refresh as its own series, exactly one carrying
// 1 and the rest 0: the stateset encoding this exporter uses for every
// enumeration (see docs/exporter-journal.md, "Enum encoding").
//
// Order is the manual's own. Registry.Gather sorts the exposition output by
// label value regardless, so this slice's order is invisible to a scrape and is
// kept as-is to stay diffable against the manual.
//
// Three of the five are marked "TS4500 only" in the manual, because this
// endpoint also serves Diamondback libraries, which have no I/O station door at
// all. They are kept: this exporter watches TS4500 hardware, and a state a
// given library can never report simply sits at 0 rather than costing anything.
var ioStationStates = []string{
	"normal",
	"closedNoMagazine",
	"failedToClose",
	"doorOpenTooLong",
	"unknown",
}

// ioStationDoor is the "door" object of one GET /v1/ioStations entry.
//
// The whole object is nullable, and R1.11.2 says why: a Diamondback I/O station
// is reached through the library's rear door and has no door of its own. Hence
// a pointer on ioStationStats below rather than an inlined struct — a station
// with no door must be distinguishable from a station whose door is closed.
//
// lastChanged is on the wire and deliberately not declared here, following the
// same decision the frames collector already took for its three *LastChanged
// fields: an alert's own `for:` clause already expresses "this door has been
// open too long", which is the entire signal the planned rule wanted, so a
// timestamp family would be a second metric per station for nothing the door
// gauge does not already give.
type ioStationDoor struct {
	// Opened is one of "yes" or "no". A string rather than a bool because
	// that is what the library sends, and because anything outside those two
	// values must be distinguishable rather than silently decoding to false.
	Opened string `json:"opened"`
}

// ioStationMagazine is the "magazine" object of one GET /v1/ioStations entry:
// the removable cartridge carrier an operator slides into the station to import
// or export media.
//
// The whole object is nullable, and R1.11.2 gives it TWO meanings on a TS4500 —
// "the door is open" or "no magazine is inserted". That ambiguity is the
// library's, not this collector's, and it is why magazinePresent below is
// documented as the union of the two rather than as "a magazine is missing".
type ioStationMagazine struct {
	// MediaType is "LTO" or "3592" — a property of the inserted magazine
	// rather than of the station, since the L25/L55 stations accept either.
	MediaType string `json:"mediaType"`

	// IOSlots is the magazine's capacity: 18 for LTO, 16 for 3592.
	IOSlots int `json:"ioSlots"`

	// ContentsVolser lists one entry per slot, top to bottom. A nil entry is
	// an empty slot; the literal string "unknown" is a cartridge present
	// whose barcode the library could not read. []*string rather than
	// []string precisely so those two stay distinguishable: both would decode
	// to "" otherwise, and an unreadable cartridge blocking an import would
	// be counted as an empty slot.
	//
	// The volsers themselves are counted, never emitted as labels. The shared
	// label vocabulary reserves `volser` for the opt-in per-cartridge detail
	// and for the bounded cleaning_cartridges collector, and an I/O station's
	// contents churn on every import — exactly the accumulating index the
	// drives collector's per-volser flag exists to keep off by default.
	ContentsVolser []*string `json:"contentsVolser"`

	// contentsInternalAddress is on the wire and not declared: R1.11.2 warns
	// it changes whenever a cartridge is assigned, unassigned or moved, and
	// it identifies a cartridge rather than measuring anything.
}

// ioStationStats is the parsed shape of one GET /v1/ioStations entry. The
// endpoint returns one element per physical station, keyed by the library's own
// native location string (ioStation_F2IOu, ioStation_F2IOl, ...), which is what
// the `location` label carries verbatim so a value in a dashboard can be pasted
// straight into the library's GUI.
type ioStationStats struct {
	Location string             `json:"location"`
	State    string             `json:"state"`
	Door     *ioStationDoor     `json:"door"`
	Magazine *ioStationMagazine `json:"magazine"`
}

// ioStationsData is this collector's only I/O: it fetches the raw response body
// from the configured library. Kept separate from parsing (parseIOStations,
// below) so parsing stays pure and unit-testable without a live library.
func (c *IOStationsCollector) ioStationsData(ctx context.Context) ([]byte, error) {
	return c.client.Fetch(ctx, "/ioStations")
}

// parseIOStations decodes ioStationsData's response body into one
// ioStationStats per physical station. Pure: no I/O, no logging, no side
// effects, so every input maps deterministically to an output. That is what
// makes it unit-testable with plain byte fixtures (see the test file's
// TestParseIOStations).
//
// Four inputs are rejected rather than passed through, all for the same
// reason — refresh keeps the previous cache on error, which is the right
// outcome for a response this collector cannot interpret:
//
//   - Anything that is not the documented array.
//   - An empty array. R1.11.2 states the L25 and L55 base frames "come with two
//     I/O stations", so every TS4500 has at least two: an empty list is a
//     response that lost its content, not a library with no I/O stations.
//   - An entry with no location. It would emit a series labelled location=""
//     that no operator can trace back to any hardware.
//   - Two entries sharing one location. Every metric below is keyed by location
//     alone — unlike node_cards, where an accessor carries two cards at one
//     location and the key had to widen, a station IS its location here, which
//     the capture's ioStation_F2IOu / ioStation_F2IOl pair confirms. So a
//     duplicate would send two metrics with the same descriptor and the same
//     label set, and Registry.Gather rejects the WHOLE scrape when that
//     happens, not just the offending series. Failing closed here keeps a
//     malformed response from taking out every other collector's metrics too
//     (see CONTRIBUTING.md, "Common Pitfalls").
func parseIOStations(b []byte) ([]ioStationStats, error) {
	var entries []ioStationStats
	if err := json.Unmarshal(b, &entries); err != nil {
		return nil, fmt.Errorf("parse io stations response: %w", err)
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("parse io stations response: empty array, want at least one I/O station")
	}

	seen := make(map[string]struct{}, len(entries))
	for _, e := range entries {
		if e.Location == "" {
			return nil, fmt.Errorf("parse io stations response: entry with an empty location")
		}
		if _, dup := seen[e.Location]; dup {
			return nil, fmt.Errorf("parse io stations response: duplicate location %q", e.Location)
		}
		seen[e.Location] = struct{}{}
	}
	return entries, nil
}

// ioStationsGetMetrics is the glue between the I/O step (ioStationsData) and the
// pure parsing step (parseIOStations): the shape every collector in this
// exporter follows, regardless of flavor. refresh, below, calls this on its own
// background schedule; nothing else in this file calls the library directly.
func (c *IOStationsCollector) ioStationsGetMetrics(ctx context.Context) ([]ioStationStats, error) {
	data, err := c.ioStationsData(ctx)
	if err != nil {
		return nil, err
	}
	return parseIOStations(data)
}

// IOStationsCollector reads GET /v1/ioStations: the health, door position and
// magazine occupancy of every I/O station in the library. An I/O station is how
// media enters and leaves without opening a frame and pausing the library, so a
// station that is stuck, missing its magazine, or sitting full is not a
// service outage — it is imports and exports quietly not happening, which
// nothing else in the library reports.
//
// It is the background-refresh variant, which on this target model is not a
// choice: a scrape serves N libraries through one /metrics and must never block
// on a machine that has gone away. A background goroutine (started by Start,
// below) refreshes a cached metric slice on a fixed interval, and Collect only
// ever reads that cache under mu.
type IOStationsCollector struct {
	client   *Client
	interval time.Duration
	log      *logger.Logger

	state           *prometheus.Desc
	doorOpen        *prometheus.Desc
	magazinePresent *prometheus.Desc
	slots           *prometheus.Desc
	slotsOccupied   *prometheus.Desc
	slotsUnreadable *prometheus.Desc
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

// NewIOStationsCollector builds the collector and its Descs. It is pure: it
// starts no goroutine and performs no I/O, which is what makes it constructible
// in tests with no background refresh running. Call Start once, after
// construction, to begin refreshing.
func NewIOStationsCollector(log *logger.Logger, client *Client, interval time.Duration) *IOStationsCollector {
	return &IOStationsCollector{
		client:   client,
		interval: interval,
		log:      log,
		state: prometheus.NewDesc(
			"tapelibrary_io_station_state",
			"Operational state of the I/O station, as a stateset: 1 on the active state and 0 on every other known state.",
			[]string{"location", "state"}, nil,
		),
		// No `door` label here, unlike tapelibrary_frame_door_open. A frame
		// carries up to three doors and needs one; R1.11.2 gives an I/O
		// station exactly one, so the label could only ever hold a single
		// constant value and `location` already identifies the door.
		doorOpen: prometheus.NewDesc(
			"tapelibrary_io_station_door_open",
			"Whether the I/O station's door is open (1) or closed (0). A station that reports no door at all, or a position this exporter cannot map to open-or-closed, produces no series rather than a 0 asserting a closed door.",
			[]string{"location"}, nil,
		),
		// Deliberately NOT "magazine missing": R1.11.2 returns a null
		// magazine for two different situations, an open door and an absent
		// magazine, and the library does not distinguish them on this
		// endpoint. Reading a 0 as "someone took the magazine out" would be
		// wrong half the time, so the help text states the union.
		magazinePresent: prometheus.NewDesc(
			"tapelibrary_io_station_magazine_present",
			"Whether the I/O station currently reports an inserted magazine (1) or not (0). R1.11.2 returns no magazine both when none is inserted and when the door is open, so a 0 means one of those two, not specifically an empty station.",
			[]string{"location"}, nil,
		),
		slots: prometheus.NewDesc(
			"tapelibrary_io_station_slots",
			"Number of cartridge slots in the magazine currently inserted in this I/O station (18 for LTO, 16 for 3592). Emitted only while a magazine is reported, since a station with none has no capacity rather than a capacity of zero.",
			[]string{"location"}, nil,
		),
		slotsOccupied: prometheus.NewDesc(
			"tapelibrary_io_station_slots_occupied",
			"Number of slots holding a cartridge in the magazine currently inserted in this I/O station, cartridges with an unreadable barcode included. Emitted only while a magazine is reported: a 0 from a station with no magazine would read as an empty magazine ready to accept an import.",
			[]string{"location"}, nil,
		),
		// The subset of slotsOccupied the library cannot identify. This is
		// not on the planned budget line: it was read off R1.11.2's own
		// contract for contentsVolser and shipped because an unidentifiable
		// cartridge sitting in the I/O station blocks the import it arrived
		// for, and no other collector in this exporter can see it.
		slotsUnreadable: prometheus.NewDesc(
			"tapelibrary_io_station_slots_unreadable",
			"Number of occupied slots whose cartridge barcode the library could not read (reported as an unknown VOLSER). A subset of tapelibrary_io_station_slots_occupied: these cartridges are present but unidentifiable, and cannot be imported until someone reseats or replaces the label.",
			[]string{"location"}, nil,
		),
		// media_type is the only identity string this endpoint carries, and
		// it describes the inserted magazine rather than the station, so this
		// series comes and goes with the magazine. That is the intended
		// reading: swapping an LTO magazine for a 3592 one is a real
		// configuration change and worth seeing as a discontinuity, the same
		// argument logical_library already makes on drives.
		info: prometheus.NewDesc(
			"tapelibrary_io_station_info",
			"Identity of the magazine currently inserted in this I/O station, always 1. Identity strings live here rather than on a measurement series, so swapping a magazine changes this series alone instead of breaking the continuity of the occupancy counts.",
			[]string{"location", "media_type"}, nil,
		),
		lastRefreshDesc: prometheus.NewDesc(
			"tapelibrary_io_stations_last_refresh_timestamp_seconds",
			"Unix time of the last successful io stations refresh. Alert if time() - this > 2 x the collector's configured interval.",
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
func (c *IOStationsCollector) Start(ctx context.Context) {
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
func (c *IOStationsCollector) Done() <-chan struct{} {
	return c.done
}

// stateset appends one series per documented state for st, plus the observed
// one if the manual does not document it. The manual's tables are demonstrably
// a floor rather than a ceiling (it names states in prose that appear in no
// table), so an unlisted state must surface as its own series rather than
// silently leave every series at 0 and make the station look stateless.
// `known` is what keeps that extra series from duplicating a label set already
// emitted above, which would fail Gather for this collector's whole scrape.
func (c *IOStationsCollector) stateset(metrics []prometheus.Metric, st *ioStationStats) []prometheus.Metric {
	known := false
	for _, s := range ioStationStates {
		value := 0.0
		if s == st.State {
			value = 1.0
			known = true
		}
		metrics = append(metrics, prometheus.MustNewConstMetric(c.state, prometheus.GaugeValue, value, st.Location, s))
	}
	if !known && st.State != "" {
		c.log.Warn("I/O station reported a state absent from the documented set: emitting it anyway",
			"location", st.Location, "state", st.State)
		metrics = append(metrics, prometheus.MustNewConstMetric(c.state, prometheus.GaugeValue, 1, st.Location, st.State))
	}
	return metrics
}

// door returns the door-open value for st and whether it should be emitted at
// all. Two inputs produce no series rather than a 0, for the same reason the
// frames collector's door handling does: a 0 asserts a door that is present and
// closed, which is a claim this collector cannot make about a station that
// reports no door (every Diamondback station) or a position nobody has
// interpreted. A silently-0 door gauge is exactly how a door alert stops firing
// without anyone noticing.
func (c *IOStationsCollector) door(st *ioStationStats) (float64, bool) {
	if st.Door == nil {
		return 0, false
	}
	switch st.Door.Opened {
	case "yes":
		return 1, true
	case "no":
		return 0, true
	default:
		c.log.Warn("I/O station reported an unrecognized door position: emitting no door series for it",
			"location", st.Location, "opened", st.Door.Opened)
		return 0, false
	}
}

// magazineCounts returns the number of occupied slots and, of those, how many
// hold a cartridge whose barcode the library could not read.
//
// R1.11.2 defines the encoding this reads: a null entry is an empty slot, and
// the literal string "unknown" is a cartridge present but unidentified. The
// empty string is folded into "empty" defensively — it is not a documented
// value, and counting an unnamed slot as occupied would inflate the occupancy
// the IOStationFull rule reads.
func magazineCounts(m *ioStationMagazine) (occupied, unreadable int) {
	for _, v := range m.ContentsVolser {
		if v == nil || *v == "" {
			continue
		}
		occupied++
		if *v == "unknown" {
			unreadable++
		}
	}
	return occupied, unreadable
}

// refresh performs the one I/O call (ioStationsGetMetrics, via the injected
// *Client) and, on success, atomically replaces the cache. On error it logs and
// returns, leaving the previous cache and lastRefresh untouched, fail-open: a
// transient failure serves the last-known-good data instead of dropping the
// series, and the freshness gauge is the signal that a refresh is stale, not a
// dropped scrape.
func (c *IOStationsCollector) refresh(ctx context.Context) {
	stations, err := c.ioStationsGetMetrics(ctx)
	if err != nil {
		c.log.Error("Failed to refresh io stations metrics: keeping previous cache", "err", err)
		return
	}

	// Per station: the stateset, the magazine-present gauge, and up to one
	// door gauge plus four magazine series.
	perStation := len(ioStationStates) + 6
	metrics := make([]prometheus.Metric, 0, len(stations)*perStation)

	// Indexed rather than ranged by value: ioStationStats carries two pointer
	// fields, and taking the address avoids copying the struct per iteration.
	for i := range stations {
		st := &stations[i]

		metrics = c.stateset(metrics, st)

		if open, ok := c.door(st); ok {
			metrics = append(metrics, prometheus.MustNewConstMetric(
				c.doorOpen, prometheus.GaugeValue, open, st.Location))
		}

		// Always emitted, both branches: this is the one series that says
		// whether the four below exist at all, so it must never be the thing
		// that goes missing.
		present := 0.0
		if st.Magazine != nil {
			present = 1.0
		}
		metrics = append(metrics, prometheus.MustNewConstMetric(
			c.magazinePresent, prometheus.GaugeValue, present, st.Location))

		if st.Magazine == nil {
			continue
		}
		m := st.Magazine
		occupied, unreadable := magazineCounts(m)

		// The library reports capacity and contents as two independent
		// fields, so they can disagree. Both are emitted anyway rather than
		// suppressed: an occupancy above capacity makes IOStationFull fire,
		// which is the correct outcome for a station whose own response does
		// not add up, and suppressing would hide it instead.
		if len(m.ContentsVolser) != m.IOSlots {
			c.log.Warn("I/O station magazine reports a slot count that disagrees with its contents list: emitting both as reported",
				"location", st.Location, "ioSlots", m.IOSlots, "contents", len(m.ContentsVolser))
		}

		metrics = append(metrics,
			prometheus.MustNewConstMetric(c.slots, prometheus.GaugeValue, float64(m.IOSlots), st.Location),
			prometheus.MustNewConstMetric(c.slotsOccupied, prometheus.GaugeValue, float64(occupied), st.Location),
			prometheus.MustNewConstMetric(c.slotsUnreadable, prometheus.GaugeValue, float64(unreadable), st.Location),
			prometheus.MustNewConstMetric(c.info, prometheus.GaugeValue, 1, st.Location, m.MediaType),
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
func (c *IOStationsCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.state
	ch <- c.doorOpen
	ch <- c.magazinePresent
	ch <- c.slots
	ch <- c.slotsOccupied
	ch <- c.slotsUnreadable
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
func (c *IOStationsCollector) Collect(ch chan<- prometheus.Metric) {
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
