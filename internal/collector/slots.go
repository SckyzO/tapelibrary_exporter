package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/sckyzo/tapelibrary_exporter/internal/logger"
)

// slotStates is the set of values GET /v1/slots tabulates for its "state"
// field (TS4500 R1.11.2, "Get slots"). The shortest state table in the whole
// API — two values — and the split between them is operationally load-bearing
// rather than cosmetic: the manual defines inServiceMode as "It cannot be
// selected as a cartridge destination but cartridges can be moved from the
// slot". A slot in that state still holds whatever it held, and still counts
// towards the library's physical capacity, but the robot may not put anything
// into it. That is what slotsPositionsAvailable below exists to say.
var slotStates = []string{
	"inServiceMode",
	"normal",
}

// slotUnreadableVolser is the token R1.11.2 documents on the sibling
// /v1/ioStations endpoint for a cartridge that is present but whose barcode
// the library could not read. The slots endpoint's own contents attribute is
// documented only as "Any empty tier is listed as null", so the token is NOT
// confirmed to appear here.
//
// It is handled anyway, on the same terms IOStationsCollector already handles
// it: such a cartridge occupies its tier, blocks the position, and carries no
// volser, so it is invisible to DataCartridgesCollector. Counting it as
// occupied is correct whether or not the token ever appears; the companion
// gauge that reports how many there are sits at a permanent 0 on firmware that
// never emits it, which is a cheap and honest way to be ready for firmware
// that does. See docs/exporter-journal.md, which records this as unverified.
const slotUnreadableVolser = "unknown"

// slotStats is the parsed shape of one GET /v1/slots entry.
//
// **A slot entry is a COLUMN, not a cartridge position**, and confusing the
// two is the single easiest mistake to make on this endpoint. Its location
// carries no tier suffix (slot_F2C1R1), while the same physical position seen
// from /v1/dataCartridges does (slot_F7C3R15T1). One entry here describes
// `tiers` stacked positions, listed in Contents; the 2026-07-28 capture holds
// 79 entries covering 199 positions. Every "how full is the library" figure
// this collector emits is therefore counted over positions, never over
// entries, and its location label does NOT join to DataCartridgesCollector's
// without the tier suffix being appended first.
type slotStats struct {
	// Location is the slot's un-tiered native position string. Unique on this
	// endpoint and the collector's key: parseSlots rejects a duplicate.
	Location string `json:"location"`

	// State is one of slotStates. Counted in aggregate and carried as a label
	// on the per-slot _info series, never emitted as a per-slot stateset: the
	// budget's tens-versus-thousands rule (docs/exporter-journal.md) puts
	// slots firmly in the thousands.
	State string `json:"state"`

	// Contents holds one entry per tier, in tier order, with null for an empty
	// tier. A pointer element because null is the documented empty marker and
	// a plain string would decode it to "", losing the distinction between "no
	// cartridge" and "a cartridge this exporter failed to read".
	Contents []*string `json:"contents"`

	// Puts is the lifetime number of times a cartridge was placed into this
	// slot. R1.11.2's own text for this field reads "The number of times a
	// cartridge is placed into the I/O station", which is plainly copied from
	// the I/O station endpoint: the sibling PutRetries below says "into this
	// slot over the lifetime of this slot", and the value is reported per
	// slot. This exporter documents it as the slot's own count.
	Puts float64 `json:"puts"`

	// PutRetries is the lifetime number of retries while placing a cartridge
	// into this slot. Zero on all 79 slots of the capture against 25 023
	// lifetime puts, which is what makes any non-zero rate meaningful.
	PutRetries float64 `json:"putRetries"`

	// GetRetries is the lifetime number of retries while getting a cartridge
	// from this slot. There is no matching "gets" counter on this endpoint, so
	// this one has no denominator and cannot be turned into a ratio the way
	// PutRetries can (see docs/exporter-journal.md's open questions).
	GetRetries float64 `json:"getRetries"`

	// Tiers is the slot's declared depth: 1 for a single-deep slot, up to 5
	// for a high-density one. Carried as the depth distribution and as an
	// _info label; capacity itself is counted over Contents, see positions.
	Tiers int `json:"tiers"`
}

// positions returns the number of cartridge positions this slot holds, counted
// over Contents rather than read off Tiers.
//
// The two describe the same physical thing and agree on all 79 slots of the
// capture, but occupancy is counted over Contents, so deriving capacity from
// that same array is what makes `occupied <= positions` true by construction
// rather than by a branch that could be wrong. slotsPositionsAvailable is a
// subtraction of exactly those two numbers, and a firmware that truncated
// Contents while still reporting the full Tiers would otherwise produce a
// negative "available" figure out of two individually plausible readings.
//
// Tiers is not discarded: it is what the depth distribution and the per-slot
// _info label report, and refresh logs when the two disagree.
func (s *slotStats) positions() int {
	return len(s.Contents)
}

// occupancy returns the number of tiers in this slot holding a cartridge and,
// of those, how many hold one whose barcode the library could not read.
//
// Mirrors magazineCounts in io_stations.go, deliberately: the encoding is the
// same one R1.11.2 defines for that endpoint, so the two must not drift. A
// null entry is an empty tier; the empty string is folded into "empty"
// defensively, since it is not a documented value and counting an unnamed tier
// as occupied would understate the free capacity SlotsNearlyFull reads.
func (s *slotStats) occupancy() (occupied, unreadable int) {
	for _, v := range s.Contents {
		if v == nil || *v == "" {
			continue
		}
		occupied++
		if *v == slotUnreadableVolser {
			unreadable++
		}
	}
	return occupied, unreadable
}

// slotsData is this collector's only I/O: it fetches the raw response body
// from the configured library. Kept separate from parsing (parseSlots, below)
// so parsing stays pure and unit-testable without a live library.
func (c *SlotsCollector) slotsData(ctx context.Context) ([]byte, error) {
	return c.client.Fetch(ctx, "/slots")
}

// parseSlots decodes slotsData's response body into one slotStats per storage
// slot. Pure: no I/O, no logging, no side effects, so every input maps
// deterministically to an output. That is what makes it unit-testable with
// plain byte fixtures (see the test file's TestParseSlots).
//
// Three inputs are rejected rather than passed through, all for the same
// reason — refresh keeps the previous cache on error, which is the right
// outcome for a response this collector cannot interpret:
//
//   - An entry with no location. It is this endpoint's only key, and a series
//     labelled with an empty one names no physical slot and would collide with
//     every other entry missing it.
//   - Two entries sharing a location. Unlike NodeCardsCollector, where an MDA
//     and an ACC card share one location and the key had to be widened, the
//     capture's 79 slots carry 79 distinct locations, which is what the manual
//     means by "The unique location of the slot". Failing closed here keeps a
//     malformed response from sending two metrics with one descriptor and one
//     label set, which fails Registry.Gather for the WHOLE scrape and would
//     take out all eighteen collectors rather than just this one.
//   - An empty array. Every TS4500 has storage slots — 10 730 across this
//     library's twelve frames — so an empty response is content loss over the
//     slow SCSI/LCC path rather than a library that has none. Serving the
//     previous cache and letting the freshness gauge go stale is the safer
//     reading, following DataCartridgesCollector rather than
//     CleaningCartridgesCollector, whose resource is genuinely consumable.
func parseSlots(b []byte) ([]slotStats, error) {
	var entries []slotStats
	if err := json.Unmarshal(b, &entries); err != nil {
		return nil, fmt.Errorf("parse slots response: %w", err)
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("parse slots response: no slots returned (every TS4500 has storage slots, so this is a truncated response rather than a library with none; keeping the previous cache)")
	}

	seen := make(map[string]struct{}, len(entries))
	// Indexed rather than ranged by value: slotStats carries two strings, a
	// slice and four numbers, so copying one per iteration is what gocritic's
	// rangeValCopy flags. refresh below takes the address for the same reason.
	for i := range entries {
		e := &entries[i]
		if e.Location == "" {
			return nil, fmt.Errorf("parse slots response: entry with an empty location")
		}
		if _, dup := seen[e.Location]; dup {
			return nil, fmt.Errorf("parse slots response: duplicate slot at location %q", e.Location)
		}
		seen[e.Location] = struct{}{}
	}
	return entries, nil
}

// slotsGetMetrics is the glue between the I/O step (slotsData) and the pure
// parsing step (parseSlots): the shape every collector in this exporter
// follows, regardless of flavor. refresh, below, calls this on its own
// background schedule; nothing else in this file calls the library directly.
func (c *SlotsCollector) slotsGetMetrics(ctx context.Context) ([]slotStats, error) {
	data, err := c.slotsData(ctx)
	if err != nil {
		return nil, err
	}
	return parseSlots(data)
}

// SlotsCollector reads GET /v1/slots: how much of the library's physical
// cartridge capacity is in use, how much of it the robot may actually target,
// and how hard the robotics is having to work to reach it.
//
// Everything emitted by default is an AGGREGATE — slot and position counts by
// state, the tier-depth distribution, and library-wide sums of the three
// lifetime robotics counters. Per-slot detail sits behind
// --collector.slots.per-slot, off by default: the population is bounded by
// library capacity rather than by any policy (see docs/exporter-journal.md,
// "Cardinality budget").
//
// Two things here exist nowhere else in this exporter:
//
//   - **Service-mode-aware free capacity.** LibraryCollector already reports
//     total, licensed and used capacity, so "the library is full" is alertable
//     without this collector. What it cannot say is that a free position sits
//     in a slot the robot may not target, which R1.11.2 defines inServiceMode
//     to mean. slotsPositionsAvailable counts only the positions that can
//     actually receive a cartridge, and SlotsNearlyFull reads it.
//   - **The robotics retry counters.** puts, putRetries and getRetries are
//     lifetime device counters, and the earliest degradation signal the API
//     offers: a gripper that is starting to miss retries before it fails.
//
// It is the background-refresh variant, which on this target model is not a
// choice: a scrape serves N libraries through one /metrics and must never
// block on a machine that has gone away.
type SlotsCollector struct {
	client   *Client
	interval time.Duration
	log      *logger.Logger

	// perSlot gates the five per-slot series, and defaults to FALSE, matching
	// --collector.data_cartridges.per-volser.
	//
	// The population is bounded by library capacity rather than by any policy.
	// The 2026-07-28 capture is a 79-entry trim of an unknown whole, so the
	// real entry count is not measured — the library reports 10 732 cartridge
	// POSITIONS, and at the capture's mix of 1- and 4-tier slots that is
	// somewhere near 4 300 entries, hence roughly 21 500 extra series per
	// library. That is a Prometheus sizing decision in its own right, so it is
	// the operator's to take rather than this collector's.
	//
	// Every aggregate below is computed from the full parsed list and emitted
	// unconditionally, so turning the flag on adds detail and turning it off
	// costs detail — neither can silence an alert.
	perSlot bool

	slotCount           *prometheus.Desc
	positions           *prometheus.Desc
	positionsOccupied   *prometheus.Desc
	positionsAvailable  *prometheus.Desc
	positionsUnreadable *prometheus.Desc
	depth               *prometheus.Desc
	puts                *prometheus.Desc
	putRetries          *prometheus.Desc
	getRetries          *prometheus.Desc

	slotPositionsOccupied *prometheus.Desc
	slotPuts              *prometheus.Desc
	slotPutRetries        *prometheus.Desc
	slotGetRetries        *prometheus.Desc
	info                  *prometheus.Desc

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

// NewSlotsCollector builds the collector and its Descs. It is pure: it starts
// no goroutine and performs no I/O, which is what makes it constructible in
// tests with no background refresh running. Call Start once, after
// construction, to begin refreshing.
//
// The three robotics counters are CounterValue with a _total suffix, on the
// same terms as the accessors' lifetime counters: R1.11.2 documents them as
// per-slot lifetime totals, so rate() over them is meaningful and a reset is a
// real event. Everything else the device reports as a snapshot is a Gauge (see
// docs/exporter-journal.md, "Metric name shape").
//
// The library-wide sums carry one caveat their help text states rather than
// hides: they are sums over a set that can shrink. Removing a frame drops
// every slot in it, and Prometheus reads the resulting fall as a counter
// reset. That is rare (a frame comes out during a service action, not during a
// week of operation) and the alternative is 3 series per slot in the default
// build, which the budget does not admit.
//
// The plural/singular split in the metric names is load-bearing rather than
// cosmetic, and follows the convention DataCartridgesCollector and
// CleaningCartridgesCollector already set: the SINGULAR subsystem
// (tapelibrary_slot_*) is per-slot and gated by perSlot, the PLURAL one
// (tapelibrary_slots*) is library-wide and always emitted.
func NewSlotsCollector(log *logger.Logger, client *Client, interval time.Duration, perSlot bool) *SlotsCollector {
	return &SlotsCollector{
		client:   client,
		interval: interval,
		log:      log,
		perSlot:  perSlot,
		slotCount: prometheus.NewDesc(
			"tapelibrary_slots",
			"Number of storage slots in each documented state. A slot is a column holding one or more stacked tiers, not a single cartridge position: use tapelibrary_slots_positions for capacity. Both documented states are always emitted, so a rule matching one still has a series to read when its count reaches zero.",
			[]string{"state"}, nil,
		),
		positions: prometheus.NewDesc(
			"tapelibrary_slots_positions",
			"Number of cartridge positions across the library's storage slots, by the state of the slot holding them. One high-density slot contributes up to five positions. Both documented states are always emitted.",
			[]string{"state"}, nil,
		),
		positionsOccupied: prometheus.NewDesc(
			"tapelibrary_slots_positions_occupied",
			"Number of cartridge positions holding a cartridge, by the state of the slot holding them. Cartridges whose barcode the library could not read are counted here too, and again in tapelibrary_slots_positions_unreadable. Both documented states are always emitted.",
			[]string{"state"}, nil,
		),
		positionsAvailable: prometheus.NewDesc(
			"tapelibrary_slots_positions_available",
			"Number of empty cartridge positions the library can actually put a cartridge into: free positions in slots whose state is normal. Positions in a slot placed in service mode are excluded, because R1.11.2 defines that state as one the robot may not select as a destination even though cartridges can still be moved out of it. This is the figure SlotsNearlyFull reads, and it is not derivable from LibraryCollector's capacity gauges, which know nothing about slot state.",
			nil, nil,
		),
		positionsUnreadable: prometheus.NewDesc(
			"tapelibrary_slots_positions_unreadable",
			"Number of occupied cartridge positions whose cartridge barcode the library could not read (reported as an unknown VOLSER). A subset of tapelibrary_slots_positions_occupied. Such a cartridge occupies its position and carries no volser, so it appears in no other collector in this exporter. R1.11.2 documents the unknown token on the I/O station endpoint and not on this one, so on firmware that never reports it this gauge stays at 0.",
			nil, nil,
		),
		depth: prometheus.NewDesc(
			"tapelibrary_slots_depth",
			"Number of storage slots of each tier depth: 1 for a single-deep slot, up to 5 for a high-density one. Only depths the library actually reports are emitted, since nothing alerts on a particular depth. This is the library's physical geometry, so it changes only when a frame is added or removed.",
			[]string{"tiers"}, nil,
		),
		puts: prometheus.NewDesc(
			"tapelibrary_slots_puts_total",
			"Total number of times a cartridge has been placed into a storage slot over the lifetime of the library's slots, summed across every slot. Summed rather than emitted per slot to keep the default build affordable, so removing a frame drops its slots out of the sum and reads as a counter reset. This is the denominator of the put-retry ratio SlotPutRetryRateHigh reads.",
			nil, nil,
		),
		putRetries: prometheus.NewDesc(
			"tapelibrary_slots_put_retries_total",
			"Total number of retries required while placing a cartridge into a storage slot, over the lifetime of the library's slots, summed across every slot. Zero on all 79 slots of the 2026-07-28 capture against 25 023 lifetime puts, so any sustained non-zero rate against tapelibrary_slots_puts_total is a robotics degradation signal rather than normal wear. Summed, so removing a frame reads as a counter reset.",
			nil, nil,
		),
		getRetries: prometheus.NewDesc(
			"tapelibrary_slots_get_retries_total",
			"Total number of retries required while getting a cartridge from a storage slot, over the lifetime of the library's slots, summed across every slot. This endpoint reports no matching gets counter, so unlike the put side there is no denominator available and no ratio can be formed. Summed, so removing a frame reads as a counter reset.",
			nil, nil,
		),
		slotPositionsOccupied: prometheus.NewDesc(
			"tapelibrary_slot_positions_occupied",
			"Number of tiers in this individual storage slot holding a cartridge. Emitted only when --collector.slots.per-slot is set, which it is not by default. The slot's total depth is on tapelibrary_slot_info's tiers label.",
			[]string{"location"}, nil,
		),
		slotPuts: prometheus.NewDesc(
			"tapelibrary_slot_puts_total",
			"Number of times a cartridge has been placed into this individual storage slot over its lifetime. Emitted only when --collector.slots.per-slot is set.",
			[]string{"location"}, nil,
		),
		slotPutRetries: prometheus.NewDesc(
			"tapelibrary_slot_put_retries_total",
			"Number of retries required while placing a cartridge into this individual storage slot, over its lifetime. Emitted only when --collector.slots.per-slot is set: this is what names the slot the library-wide retry rate is coming from.",
			[]string{"location"}, nil,
		),
		slotGetRetries: prometheus.NewDesc(
			"tapelibrary_slot_get_retries_total",
			"Number of retries required while getting a cartridge from this individual storage slot, over its lifetime. Emitted only when --collector.slots.per-slot is set.",
			[]string{"location"}, nil,
		),
		info: prometheus.NewDesc(
			"tapelibrary_slot_info",
			"Always 1. Carries this storage slot's current state and its physical tier depth as labels, joinable to the per-slot measurements on location. The active state rides here rather than on a per-slot stateset: at library scale a full stateset over every slot is what the cardinality budget rules out. Emitted only when --collector.slots.per-slot is set. Note that this location has no tier suffix, so joining it to a cartridge location from DataCartridgesCollector needs one appended.",
			[]string{"location", "state", "tiers"}, nil,
		),
		lastRefreshDesc: prometheus.NewDesc(
			"tapelibrary_slots_last_refresh_timestamp_seconds",
			"Unix time of the last successful slots refresh. Alert if time() - this > 2 x the collector's configured interval.",
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
func (c *SlotsCollector) Start(ctx context.Context) {
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
func (c *SlotsCollector) Done() <-chan struct{} {
	return c.done
}

// slotTotals is the accumulator refresh fills in its single pass over the
// parsed slots. Grouped into one struct rather than eight locals so the
// per-state maps and the library-wide scalars cannot drift apart as this
// collector grows.
type slotTotals struct {
	// slots, positions and occupied are keyed by slot state.
	slots     map[string]int
	positions map[string]int
	occupied  map[string]int

	// depth is keyed by the slot's declared tier count, already rendered as
	// the label value it will carry.
	depth map[string]int

	unreadable int

	puts       float64
	putRetries float64
	getRetries float64
}

// accumulate walks the parsed slots once, filling every counter this collector
// emits. Pulled out of refresh so the arithmetic is testable and readable on
// its own; it does no I/O and touches no descriptor.
//
// A slot whose declared Tiers disagrees with the length of its Contents array
// is counted from Contents (see slotStats.positions) and logged: the two
// describe the same physical thing and agree on all 79 slots of the capture,
// so a disagreement is a firmware surprise worth surfacing rather than
// silently reconciling.
func (c *SlotsCollector) accumulate(slots []slotStats) slotTotals {
	t := slotTotals{
		slots:     make(map[string]int, len(slotStates)),
		positions: make(map[string]int, len(slotStates)),
		occupied:  make(map[string]int, len(slotStates)),
		depth:     make(map[string]int, 4),
	}

	// Indexed rather than ranged by value, same reason as parseSlots above.
	for i := range slots {
		s := &slots[i]
		occupied, unreadable := s.occupancy()
		positions := s.positions()

		if positions != s.Tiers {
			c.log.Warn("Slot reported a tier count disagreeing with the length of its contents array: counting capacity from contents",
				"location", s.Location, "tiers", s.Tiers, "contents", positions)
		}

		t.slots[s.State]++
		t.positions[s.State] += positions
		t.occupied[s.State] += occupied
		t.depth[strconv.Itoa(s.Tiers)]++
		t.unreadable += unreadable

		t.puts += s.Puts
		t.putRetries += s.PutRetries
		t.getRetries += s.GetRetries
	}
	return t
}

// stateCounts emits the three per-state families — slot count, positions and
// occupied positions — one series per documented state, plus one per observed
// state the manual does not tabulate.
//
// Every documented state is emitted whether or not any slot is in it, so a
// rule matching state="inServiceMode" still has a series to read when the last
// slot comes out of service: an absent series satisfies no matcher, so the
// alert would go quiet rather than resolve.
//
// The three families share one loop rather than three, because they share the
// same key space and the emit-observed-anyway branch below — the thing that
// catches the manual being wrong — must not be present on one and forgotten on
// another. R1.11.2 tabulates only two states here, the shortest state table in
// the API, which makes an untabulated third more likely rather than less.
func (c *SlotsCollector) stateCounts(metrics []prometheus.Metric, t *slotTotals) []prometheus.Metric {
	emit := func(state string) []prometheus.Metric {
		return []prometheus.Metric{
			prometheus.MustNewConstMetric(c.slotCount, prometheus.GaugeValue, float64(t.slots[state]), state),
			prometheus.MustNewConstMetric(c.positions, prometheus.GaugeValue, float64(t.positions[state]), state),
			prometheus.MustNewConstMetric(c.positionsOccupied, prometheus.GaugeValue, float64(t.occupied[state]), state),
		}
	}

	observed := maps.Clone(t.slots)
	for _, s := range slotStates {
		metrics = append(metrics, emit(s)...)
		// Deleting as we go leaves exactly the undocumented states behind,
		// which is also what keeps the loop below from duplicating a label set
		// already emitted here and failing Gather for the whole scrape.
		delete(observed, s)
	}

	for _, s := range slices.Sorted(maps.Keys(observed)) {
		if s == "" {
			// A slot with no state at all. It is counted in no state series
			// rather than emitted under state="", which no operator can act
			// on. Its positions are counted nowhere either, which is why this
			// is a warning rather than a silent skip.
			c.log.Warn("Slots reported with an empty state: counted in no state series",
				"count", observed[s])
			continue
		}
		c.log.Warn("Slot reported a state absent from the documented set: emitting it anyway",
			"state", s, "count", observed[s])
		metrics = append(metrics, emit(s)...)
	}
	return metrics
}

// depthCounts emits one series per tier depth the library actually reports.
//
// Unlike the state families this one is NOT a full cross product of documented
// values: R1.11.2's worked example shows a 5-tier slot and this fleet runs 1-
// and 4-tier ones, so the plausible range is small but the documented set is
// not enumerated anywhere. Nothing alerts on a particular depth, so there is no
// matcher that needs a zero to read — the same reasoning that shapes
// DataCartridgesCollector's media family.
func (c *SlotsCollector) depthCounts(metrics []prometheus.Metric, t *slotTotals) []prometheus.Metric {
	for _, d := range slices.Sorted(maps.Keys(t.depth)) {
		metrics = append(metrics, prometheus.MustNewConstMetric(
			c.depth, prometheus.GaugeValue, float64(t.depth[d]), d))
	}
	return metrics
}

// refresh performs the one I/O call (slotsGetMetrics, via the injected
// *Client) and, on success, atomically replaces the cache. On error it logs and
// returns, leaving the previous cache and lastRefresh untouched, fail-open: a
// transient failure serves the last-known-good data instead of dropping the
// series, and the freshness gauge is the signal that a refresh is stale, not a
// dropped scrape.
//
// Every aggregate is computed from every parsed slot and emitted whatever
// perSlot says. Only the five per-slot families depend on it, which is what
// makes the flag a pure cardinality lever: flipping it off cannot silence
// SlotsNearlyFull or either retry rule.
func (c *SlotsCollector) refresh(ctx context.Context) {
	slots, err := c.slotsGetMetrics(ctx)
	if err != nil {
		c.log.Error("Failed to refresh slots metrics: keeping previous cache", "err", err)
		return
	}

	totals := c.accumulate(slots)

	// Per slot, when perSlot is on: an _info, an occupancy gauge and the three
	// lifetime counters. Plus the library-wide aggregates, whose dominant term
	// is the three-family state block at one series each per documented state.
	const perSlotSeries = 5
	aggregates := len(slotStates)*3 + 5 + 4
	metrics := make([]prometheus.Metric, 0, len(slots)*perSlotSeries+aggregates)

	if c.perSlot {
		for i := range slots {
			s := &slots[i]
			occupied, _ := s.occupancy()
			metrics = append(metrics,
				prometheus.MustNewConstMetric(c.info, prometheus.GaugeValue, 1,
					s.Location, s.State, strconv.Itoa(s.Tiers)),
				prometheus.MustNewConstMetric(c.slotPositionsOccupied, prometheus.GaugeValue, float64(occupied), s.Location),
				prometheus.MustNewConstMetric(c.slotPuts, prometheus.CounterValue, s.Puts, s.Location),
				prometheus.MustNewConstMetric(c.slotPutRetries, prometheus.CounterValue, s.PutRetries, s.Location),
				prometheus.MustNewConstMetric(c.slotGetRetries, prometheus.CounterValue, s.GetRetries, s.Location),
			)
		}
	}

	metrics = c.stateCounts(metrics, &totals)
	metrics = c.depthCounts(metrics, &totals)

	// Only slots in the normal state can receive a cartridge; a free position
	// in a slot placed in service mode is capacity the robot may not use. This
	// subtraction is over the same array on both sides (see
	// slotStats.positions), so it cannot go negative.
	available := totals.positions["normal"] - totals.occupied["normal"]

	metrics = append(metrics,
		prometheus.MustNewConstMetric(c.positionsAvailable, prometheus.GaugeValue, float64(available)),
		prometheus.MustNewConstMetric(c.positionsUnreadable, prometheus.GaugeValue, float64(totals.unreadable)),
		prometheus.MustNewConstMetric(c.puts, prometheus.CounterValue, totals.puts),
		prometheus.MustNewConstMetric(c.putRetries, prometheus.CounterValue, totals.putRetries),
		prometheus.MustNewConstMetric(c.getRetries, prometheus.CounterValue, totals.getRetries),
	)

	c.mu.Lock()
	c.cached = metrics
	c.lastRefresh = time.Now()
	c.mu.Unlock()
}

// Describe sends every one of this collector's descriptors, including the
// freshness gauge. Constant regardless of scrape or refresh outcome, which is
// what makes prometheus.DescribeByCollect unnecessary here.
//
// The five per-slot descriptors are described even when
// --collector.slots.per-slot is off: a descriptor is what this collector CAN
// emit, not what it did last time, and docs/metrics.md documents them on that
// basis.
func (c *SlotsCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.slotCount
	ch <- c.positions
	ch <- c.positionsOccupied
	ch <- c.positionsAvailable
	ch <- c.positionsUnreadable
	ch <- c.depth
	ch <- c.puts
	ch <- c.putRetries
	ch <- c.getRetries
	ch <- c.slotPositionsOccupied
	ch <- c.slotPuts
	ch <- c.slotPutRetries
	ch <- c.slotGetRetries
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
// startup window, not a failure. The gauge's own value (0 until the first
// refresh lands) is the separate, correct signal for staleness.
func (c *SlotsCollector) Collect(ch chan<- prometheus.Metric) {
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
