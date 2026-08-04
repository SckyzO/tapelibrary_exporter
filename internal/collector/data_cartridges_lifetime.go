package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/sckyzo/tapelibrary_exporter/internal/logger"
)

// The metric subsystem for this collector is `data_cartridges_usage`, NOT
// `data_cartridges_lifetime`, and that is the one place in this exporter where
// the subsystem and the collector name deliberately differ.
//
// DataCartridgesCollector already owns the `..._data_cartridges_lifetime_`
// prefix, where it means media life REMAINING —
// tapelibrary_data_cartridges_lifetime_remaining_ratio and its companion
// tapelibrary_data_cartridges_lifetime_unknown. This collector reads the
// opposite quantity from a different endpoint: lifetime work DONE. Two
// collectors sharing one prefix with two opposed meanings, refreshing on two
// schedules, is a trap for anyone reading a dashboard rather than this file, so
// the prefix says which of the two it is.
//
// The collector keeps the name the plan and the endpoint give it
// (data_cartridges_lifetime, GET /v1/dataCartridges/lifetimeMetrics), so
// --collector.data_cartridges_lifetime.* still matches the resource an operator
// looks up in the manual.

// dataCartridgeUsageDirections and dataCartridgeUsageCorrections are the two
// label dimensions of the error family. R1.11.2 reports exactly four error
// counters per cartridge — errorsCorrectedRead, errorsCorrectedWrite,
// errorsUncorrectedRead, errorsUncorrectedWrite — which is the cross product of
// these two, so they ship as one metric family with two labels rather than as
// four separately named metrics. Same series count either way; one family means
// a rule can select every uncorrected error in one matcher.
//
// `direction` is the vocabulary's existing label, reserved by
// docs/exporter-journal.md for exactly this use ("`direction` takes
// `read`/`write` on the cartridge error counters"). `correction` is new with
// this collector and has been added to that vocabulary.
var (
	dataCartridgeUsageDirections  = []string{"read", "write"}
	dataCartridgeUsageCorrections = []string{"corrected", "uncorrected"}
)

// The reason label on tapelibrary_data_cartridges_usage_unknown. Both values
// are always emitted, so a rule reading either still has a series when its
// count falls to zero.
const (
	// dataCartridgeUsageUnread is a cartridge whose memory the library has not
	// read: every one of the seven counters comes back null together. 27 of the
	// 69 cartridges in the 2026-07-28 capture (39%) are in this state, the same
	// cohort DataCartridgesCollector meets at 45% on /v1/dataCartridges. It is
	// the endpoint's ordinary shape rather than a fault, and nothing should
	// alert on it.
	dataCartridgeUsageUnread = "unread"

	// dataCartridgeUsageInvalid is a cartridge that reported something this
	// collector will not trust: a negative counter, or a partial record with
	// some counters present and others null. Unlike unread, this IS worth
	// looking at — see dataCartridgeUsageStats.valid for what the library
	// actually returns here.
	dataCartridgeUsageInvalid = "invalid"
)

// dataCartridgeUsageMBToBytes converts the API's dataWrittenToCartridge to the
// base unit Prometheus takes. R1.11.2 documents the field as "Number of MB of
// data written to the cartridge over the lifetime of the cartridge", and the MB
// is read as the decimal 10^6 rather than the binary 2^20: IBM advertises this
// media decimally (a 3592 JD is a 10 TB cartridge), so the decimal reading is
// the one consistent with every other capacity figure a site has for the same
// hardware. The capture's largest cartridge is 71 207 511 MB, or 71.21 TB.
const dataCartridgeUsageMBToBytes = 1e6

// The histogram bounds below are sized against the 2026-07-28 capture's 41
// cartridges with a usable reading rather than picked as round numbers, and
// each is wide enough that the observed maximum is not already in the +Inf
// bucket — except on the error family, where landing in +Inf is the point.
var (
	// dataCartridgeUsageMotionBuckets spans the observed 3.3e5 .. 1.47e7 metres
	// of tape drawn across the head. A 3592 cartridge holds roughly a kilometre
	// of tape, so the top bound is on the order of 20 000 full passes.
	dataCartridgeUsageMotionBuckets = []float64{5e5, 1e6, 2.5e6, 5e6, 1e7, 2e7}

	// dataCartridgeUsageMountsBuckets spans the observed 16 .. 2 114 mounts.
	// The bottom bound is what separates a barely-used cartridge from the
	// working parc; the top two are headroom, since nothing in the capture
	// exceeds 2 500 and media is retired long before 5 000.
	dataCartridgeUsageMountsBuckets = []float64{50, 250, 500, 1000, 2500, 5000}

	// dataCartridgeUsageWrittenBuckets are bytes, spanning the observed 13.1 TB
	// .. 71.2 TB written over a cartridge's life. A JD holds 10 TB native, so
	// these count rewrites as much as capacity: the 1 TB bound is the
	// barely-written case and 100 TB is ten full rewrites.
	dataCartridgeUsageWrittenBuckets = []float64{1e12, 5e12, 1e13, 2.5e13, 5e13, 1e14}

	// dataCartridgeUsageErrorBuckets are shared by all four error counters.
	//
	// le="0" is load-bearing and not merely the bottom of the range: it is what
	// makes "how many cartridges have ANY uncorrected error" expressible as
	// _count minus that bucket, which is the quantity
	// DataCartridgeUncorrectedErrorsRising alerts on. Without it the question
	// has no answer on the wire.
	//
	// The top bound is deliberately below the observed maximum of 16 161: a
	// cartridge in the +Inf bucket of the uncorrected family is exactly the one
	// an operator wants named, and burying it inside a wider bound would hide
	// it among the merely noisy.
	dataCartridgeUsageErrorBuckets = []float64{0, 1, 10, 100, 1000, 10000}
)

// dataCartridgeUsageStats is the parsed shape of one
// GET /v1/dataCartridges/lifetimeMetrics entry: the seven cumulative counters
// the library keeps in each cartridge's own memory, plus the two identity
// fields.
//
// **Every counter is a pointer, and all seven move together.** R1.11.2 does not
// document a null on any of them, but the capture returns null on all seven at
// once for 27 of 69 cartridges — the same cartridge-memory-absent cohort
// DataCartridgesCollector documents at dataCartridgeUnknown, seen from the
// endpoint that reads nothing else. Decoded as plain ints those nulls would
// become 0, which on a cumulative counter asserts the cartridge has never
// moved, never been mounted and never taken an error: a fabricated reading
// rather than a missing one, and one that would drag every histogram's _sum and
// _count towards a parc-wide understatement.
//
// **This endpoint carries no `location`, which is what makes its key different
// from every other cartridge collector's.** DataCartridgesCollector and
// CleaningCartridgesCollector both key on volser AND location, because R1.11.2
// is explicit that a volser is not unique in a library and the sibling
// cleaningCartridges capture proves it (70 cartridges, 63 distinct volsers).
// Here there is no location to pair with, so the per-volser series are keyed on
// volser alone and parseDataCartridgesLifetime rejects a duplicate outright
// rather than emitting one. See that function for why failing closed is the
// right outcome, and why internalAddress is parsed but never labelled.
type dataCartridgeUsageStats struct {
	// Volser is the cartridge's barcode, and the only label this collector's
	// per-cartridge series carry.
	Volser string `json:"volser"`

	// InternalAddress is R1.11.2's documented unique identifier for a cartridge
	// within the library, a 6-character hex string. It is parsed so that a
	// duplicate volser can be reported with both addresses named, which is what
	// turns that error from "something is wrong" into an entry an operator can
	// look up. It is deliberately NEVER emitted as a label: the manual
	// documents it as changing "if the cartridge is assigned or unassigned from
	// a logical library or if the cartridge is moved by the host or library",
	// so at up to 9 749 cartridges it would churn a fresh series out of every
	// robot move while naming nothing an operator can act on. That is the same
	// judgement DataCartridgesCollector already recorded for the same field.
	InternalAddress string `json:"internalAddress"`

	// MotionMeters is metres of tape drawn across the drive head over the
	// cartridge's life. This is the field the capture proves can come back
	// corrupt: see valid() below.
	MotionMeters *int64 `json:"motionMeters"`

	// Mounts is the number of times the cartridge has been loaded into a drive.
	Mounts *int64 `json:"mounts"`

	// DataWrittenToCartridge is megabytes written over the cartridge's life,
	// converted to bytes on emission (dataCartridgeUsageMBToBytes).
	DataWrittenToCartridge *int64 `json:"dataWrittenToCartridge"`

	ErrorsCorrectedRead    *int64 `json:"errorsCorrectedRead"`
	ErrorsCorrectedWrite   *int64 `json:"errorsCorrectedWrite"`
	ErrorsUncorrectedRead  *int64 `json:"errorsUncorrectedRead"`
	ErrorsUncorrectedWrite *int64 `json:"errorsUncorrectedWrite"`
}

// counters returns the seven counters in a fixed order, so that the null and
// negative tests below cannot be applied to six of them and forgotten on the
// seventh.
func (d *dataCartridgeUsageStats) counters() [7]*int64 {
	return [7]*int64{
		d.MotionMeters, d.Mounts, d.DataWrittenToCartridge,
		d.ErrorsCorrectedRead, d.ErrorsCorrectedWrite,
		d.ErrorsUncorrectedRead, d.ErrorsUncorrectedWrite,
	}
}

// unread reports whether the library returned no reading at all for this
// cartridge: all seven counters null together. That is the cartridge-memory
// cohort, it is 39% of the capture, and it is not a fault.
func (d *dataCartridgeUsageStats) unread() bool {
	for _, c := range d.counters() {
		if c != nil {
			return false
		}
	}
	return true
}

// valid reports whether this cartridge's record can be trusted as a whole.
//
// **One bad counter disqualifies the whole record, deliberately.** The
// alternative — suppressing per counter — would give each histogram family its
// own denominator, so tapelibrary_data_cartridges_usage_unknown could no longer
// be a single figure that closes the arithmetic, and no dashboard could state
// what fraction of the parc any one distribution actually covers. Disqualifying
// the cartridge keeps one invariant that holds for every family at once:
//
//	_count + unknown{reason="unread"} + unknown{reason="invalid"} == the parc
//
// It is also the more honest reading of what the library returned. The capture's
// one offending cartridge is TST090JD, reporting motionMeters -285 211 648
// alongside 50 mounts and 0 bytes written. Negative is impossible on a
// cumulative counter; read as unsigned 32-bit it would be 4.0e9 metres, or four
// million kilometres of tape on a cartridge that was mounted fifty times and
// never written to, which is impossible for a different reason. Cartridge
// memory that wrong about one field has not earned trust on the other six.
//
// Two shapes fail here:
//
//   - A negative counter, as above.
//   - A partial record: some counters present, others null. Unobserved in the
//     capture, where the nulls are all-or-nothing, and handled anyway because
//     the cost is one branch and the failure mode of NOT handling it is a
//     silent 0 in whichever family lost its field.
func (d *dataCartridgeUsageStats) valid() bool {
	for _, c := range d.counters() {
		if c == nil || *c < 0 {
			return false
		}
	}
	return true
}

// dataCartridgesLifetimeData is this collector's only I/O: it fetches the raw
// response body from the configured library. Kept separate from parsing
// (parseDataCartridgesLifetime, below) so parsing stays pure and unit-testable
// without a live library.
func (c *DataCartridgesLifetimeCollector) dataCartridgesLifetimeData(ctx context.Context) ([]byte, error) {
	return c.client.Fetch(ctx, "/dataCartridges/lifetimeMetrics")
}

// parseDataCartridgesLifetime decodes dataCartridgesLifetimeData's response
// body into one dataCartridgeUsageStats per cartridge. Pure: no I/O, no
// logging, no side effects, so every input maps deterministically to an output.
//
// Four inputs are rejected rather than passed through. All four leave the
// previous cache in place, which is the right outcome for a response this
// collector cannot interpret:
//
//   - An empty array. Same reading as DataCartridgesCollector's: a library
//     reporting zero cartridges has either lost its inventory or truncated the
//     body over the slow SCSI/LCC path, and serving the last known good data
//     while the freshness gauge goes stale is the safer of the two.
//   - An entry with no volser. It would emit a per-cartridge series labelled
//     with an empty key, which no operator can trace back to a physical
//     cartridge.
//   - An entry with no internalAddress. R1.11.2 documents it as the identifier
//     that disambiguates cartridges, so a response missing it is one whose
//     duplicate-volser reporting below could not be trusted either.
//   - Two entries sharing a volser.
//
// **That last one deserves its reasoning stated, because it is the one place
// this collector is stricter than its siblings.** A volser is NOT unique in a
// tape library — R1.11.2 says so, and the cleaningCartridges capture holds 70
// cartridges under 63 distinct volsers — so DataCartridgesCollector keys on
// volser AND location and tolerates the duplicate barcode. This endpoint
// returns no location, so that pair does not exist here, and the choice is
// between two failure modes rather than between a failure and a success:
//
//   - Emitting both entries under one volser would give two metrics the same
//     descriptor and label set, which fails Registry.Gather for the WHOLE
//     scrape. One duplicate barcode would take out all nineteen collectors and
//     every library, not just this one.
//   - Labelling by internalAddress instead would avoid that, and was rejected:
//     the manual documents that field as changing on every move and every
//     (un)assignment, so it would churn a new series per robot move across up
//     to 9 749 cartridges, and it identifies nothing an operator can act on.
//     It also buys nothing, because these per-cartridge counters are only
//     useful joined to tapelibrary_data_cartridge_info on volser, and that join
//     is ambiguous precisely when the volser is duplicated.
//
// **A duplicate volser is reported, not fatal**, and that balance was set on
// 2026-08-03 against the real fleet rather than reasoned about in advance.
// This collector used to reject the whole response on one, on the argument
// that a volser must be unique for the per-cartridge series to be keyed at
// all. The live library then produced exactly one duplicate among 9 673
// distinct barcodes — 2 cartridges out of 9 674, 0.02% — and that single
// ambiguity took out all five AGGREGATE families as well, permanently, for
// the whole library. The aggregates key on nothing: they are distributions
// over the parc, and a duplicated barcode is still two real cartridges whose
// usage belongs in them.
//
// So the fail-closed behaviour is kept exactly where the key is load-bearing
// and nowhere else. Every entry counts towards the aggregates. The
// per-cartridge families skip the ambiguous volsers, which is what keeps two
// series from ever sharing a descriptor and a label set — the failure that
// takes down Registry.Gather for the whole scrape, every collector included.
// The parser therefore reports which volsers are duplicated rather than
// refusing, and refresh decides what to do with them.
//
// The duplicate is not swept under the carpet either: it is an operational
// fault worth fixing (two cartridges cannot be told apart by barcode, so
// neither can a human), and
// tapelibrary_data_cartridges_usage_duplicate_volsers publishes the count so
// DataCartridgeDuplicateVolser can page on it.
func parseDataCartridgesLifetime(b []byte) ([]dataCartridgeUsageStats, map[string]struct{}, error) {
	var entries []dataCartridgeUsageStats
	if err := json.Unmarshal(b, &entries); err != nil {
		return nil, nil, fmt.Errorf("parse data cartridges lifetime response: %w", err)
	}
	if len(entries) == 0 {
		return nil, nil, fmt.Errorf("parse data cartridges lifetime response: no cartridges returned (a library that reports none has lost its inventory or truncated the response; keeping the previous cache)")
	}

	// Indexed rather than ranged by value: dataCartridgeUsageStats carries two
	// strings and seven pointers, so copying one per iteration is what
	// gocritic's rangeValCopy flags. refresh below takes the address for the
	// same reason.
	seen := make(map[string]string, len(entries))
	duplicated := map[string]struct{}{}
	for i := range entries {
		e := &entries[i]
		// An entry with no barcode at all, or no internal address, is a
		// malformed response rather than an ambiguous cartridge: neither can
		// be counted or named, so these stay fatal.
		if e.Volser == "" {
			return nil, nil, fmt.Errorf("parse data cartridges lifetime response: entry at internal address %q with an empty volser", e.InternalAddress)
		}
		if e.InternalAddress == "" {
			return nil, nil, fmt.Errorf("parse data cartridges lifetime response: entry with volser %q and an empty internal address", e.Volser)
		}
		if _, dup := seen[e.Volser]; dup {
			duplicated[e.Volser] = struct{}{}
			continue
		}
		seen[e.Volser] = e.InternalAddress
	}
	return entries, duplicated, nil
}

// dataCartridgesLifetimeGetMetrics is the glue between the I/O step
// (dataCartridgesLifetimeData) and the pure parsing step
// (parseDataCartridgesLifetime): the shape every collector in this exporter
// follows, regardless of flavor. refresh, below, calls this on its own
// background schedule; nothing else in this file calls the library directly.
func (c *DataCartridgesLifetimeCollector) dataCartridgesLifetimeGetMetrics(ctx context.Context) ([]dataCartridgeUsageStats, map[string]struct{}, error) {
	data, err := c.dataCartridgesLifetimeData(ctx)
	if err != nil {
		return nil, nil, err
	}
	return parseDataCartridgesLifetime(data)
}

// usageHistogram accumulates one const histogram's state over a refresh.
// Written once and shared by all seven distributions so that the cumulative
// bucket convention MustNewConstHistogram expects — each bound holding the
// number of observations less than or equal to it — cannot be right in one
// family and subtly wrong in another.
type usageHistogram struct {
	bounds  []float64
	buckets map[float64]uint64
	count   uint64
	sum     float64
}

func newUsageHistogram(bounds []float64) *usageHistogram {
	h := &usageHistogram{bounds: bounds, buckets: make(map[float64]uint64, len(bounds))}
	// A bound no observation reaches still needs its (zero) entry, or the
	// exposition would skip that bucket entirely and every le= a rule names
	// would have to be one the fixture happened to exercise.
	for _, b := range bounds {
		h.buckets[b] = 0
	}
	return h
}

func (h *usageHistogram) observe(v float64) {
	h.count++
	h.sum += v
	for _, b := range h.bounds {
		if v <= b {
			h.buckets[b]++
		}
	}
}

func (h *usageHistogram) metric(desc *prometheus.Desc, labels ...string) prometheus.Metric {
	return prometheus.MustNewConstHistogram(desc, h.count, h.sum, h.buckets, labels...)
}

// DataCartridgesLifetimeCollector reads
// GET /v1/dataCartridges/lifetimeMetrics: how much work every data cartridge in
// the library has done over its life, and how many errors it took doing it.
//
// This is the only endpoint in the exporter that hands over genuine monotonic
// device counters, which is why the per-cartridge series here are the exporter's
// only CounterValue metrics with a _total suffix. The library-wide view of the
// same data cannot be a counter — a parc gains and loses cartridges — so it
// ships as distributions instead: seven histograms over the same population,
// answering "how worn is this library's media" without one series per cartridge.
//
// Everything emitted by default is an aggregate. Per-cartridge detail sits
// behind --collector.data_cartridges_lifetime.per-volser, off by default,
// because turning it on costs 7 series per cartridge — roughly 68 250 per
// library and five times that across the fleet.
//
// It is the background-refresh variant, which on this target model is not a
// choice: a scrape serves N libraries through one /metrics and must never block
// on a machine that has gone away. A background goroutine (started by Start,
// below) refreshes a cached metric slice on a fixed interval, and Collect only
// ever reads that cache under mu.
type DataCartridgesLifetimeCollector struct {
	client   *Client
	interval time.Duration
	log      *logger.Logger

	// perVolser gates the four per-cartridge families, and defaults to FALSE,
	// matching --collector.data_cartridges.per-volser and
	// --collector.slots.per-slot.
	//
	// The population is bounded by library capacity rather than by any policy:
	// 9 749 cartridges × 7 series is ~68 250 per library and ~341 000 across
	// the fleet. That is a Prometheus sizing decision in its own right, so it
	// is the operator's to take rather than this collector's.
	//
	// Every aggregate below is computed from the full parsed list and emitted
	// unconditionally, so turning the flag on adds detail and turning it off
	// costs detail — neither can silence an alert.
	perVolser bool

	motion           *prometheus.Desc
	mounts           *prometheus.Desc
	written          *prometheus.Desc
	errors           *prometheus.Desc
	unknown          *prometheus.Desc
	duplicateVolsers *prometheus.Desc
	cartMotion       *prometheus.Desc
	cartMounts       *prometheus.Desc
	cartWritten      *prometheus.Desc
	cartErrors       *prometheus.Desc
	lastRefreshDesc  *prometheus.Desc

	// mu guards cached and lastRefresh: refresh (below) writes them from the
	// background goroutine started by Start, Collect reads them from whichever
	// goroutine calls it (a Prometheus scrape). RWMutex, not a plain Mutex,
	// because Collect only ever reads.
	mu          sync.RWMutex
	cached      []prometheus.Metric
	lastRefresh time.Time

	// done is closed when the background goroutine launched by Start exits.
	// main.go waits on Done() (via instance.BackgroundCollector) after the HTTP
	// server has shut down, so the process doesn't exit mid-refresh.
	done chan struct{}
}

// NewDataCartridgesLifetimeCollector builds the collector and its Descs. It is
// pure: it starts no goroutine and performs no I/O, which is what makes it
// constructible in tests with no background refresh running. Call Start once,
// after construction, to begin refreshing.
//
// The plural/singular split in the metric names is load-bearing rather than
// cosmetic, and follows the convention DataCartridgesCollector and
// CleaningCartridgesCollector already set: the SINGULAR subsystem
// (tapelibrary_data_cartridge_usage_*) is per-cartridge and gated by perVolser,
// the PLURAL one (tapelibrary_data_cartridges_usage_*) is library-wide and
// always emitted.
//
// Only the per-cartridge four are CounterValue with _total. They are the
// library's own cumulative readings, monotonic for as long as a cartridge
// stays in the library, so rate() and increase() are meaningful on them. The
// aggregates are distributions over a population that changes, so their _sum
// and _count are not counters in the Prometheus sense and take no suffix.
func NewDataCartridgesLifetimeCollector(log *logger.Logger, client *Client, interval time.Duration, perVolser bool) *DataCartridgesLifetimeCollector {
	return &DataCartridgesLifetimeCollector{
		client:    client,
		interval:  interval,
		log:       log,
		perVolser: perVolser,
		motion: prometheus.NewDesc(
			"tapelibrary_data_cartridges_usage_motion_meters",
			"Distribution of metres of tape drawn across the drive head over each data cartridge's lifetime. Always emitted. Cartridges the library reports no usable reading for are counted in tapelibrary_data_cartridges_usage_unknown instead of being observed here, so this histogram's _count plus that gauge is the library's full cartridge parc.",
			nil, nil,
		),
		mounts: prometheus.NewDesc(
			"tapelibrary_data_cartridges_usage_mounts",
			"Distribution of the number of times each data cartridge has been loaded into a drive over its lifetime. Always emitted. Cartridges the library reports no usable reading for are counted in tapelibrary_data_cartridges_usage_unknown instead of being observed here.",
			nil, nil,
		),
		written: prometheus.NewDesc(
			"tapelibrary_data_cartridges_usage_written_bytes",
			"Distribution of bytes written to each data cartridge over its lifetime. Converted from the API's megabytes, read decimally (1 MB = 1e6 bytes) to match how this media's capacity is advertised. Always emitted. Cartridges the library reports no usable reading for are counted in tapelibrary_data_cartridges_usage_unknown instead of being observed here.",
			nil, nil,
		),
		errors: prometheus.NewDesc(
			"tapelibrary_data_cartridges_usage_errors",
			"Distribution of lifetime error counts across the library's data cartridges, by transfer direction and by whether the library was able to correct them. Always emitted for all four combinations. The number of cartridges carrying at least one error of a given kind is this histogram's _count minus its le=\"0\" bucket; a rising uncorrected count is media loss in progress and is what DataCartridgeUncorrectedErrorsRising reads.",
			[]string{"direction", "correction"}, nil,
		),
		unknown: prometheus.NewDesc(
			"tapelibrary_data_cartridges_usage_unknown",
			"Number of data cartridges excluded from the usage histograms, by reason. Always emitted for both reasons. reason=\"unread\" is a cartridge whose memory the library has not read, which returns all seven counters as null at once and is the endpoint's ordinary shape rather than a fault (39% of the reference capture). reason=\"invalid\" is a cartridge that reported a negative counter or a partial record, which is a cartridge-memory fault worth investigating. Each histogram's _count plus both of these is the library's full cartridge parc.",
			[]string{"reason"}, nil,
		),
		duplicateVolsers: prometheus.NewDesc(
			"tapelibrary_data_cartridges_usage_duplicate_volsers",
			"Number of barcodes this endpoint reported on more than one cartridge. Normally 0, and anything above it is an operational fault rather than a reading: two cartridges sharing a barcode cannot be told apart by a human either, and this endpoint reports no location to separate them. Those cartridges still count towards every aggregate here; only their per-cartridge series are withheld, since two metrics sharing a descriptor and a label set would fail the whole scrape. DataCartridgeDuplicateVolser reads this.",
			nil, nil,
		),
		cartMotion: prometheus.NewDesc(
			"tapelibrary_data_cartridge_usage_motion_meters_total",
			"Metres of tape drawn across the drive head over this individual data cartridge's lifetime, as the library's own cumulative counter. Emitted only when --collector.data_cartridges_lifetime.per-volser is set, which it is not by default. A cartridge with no usable reading produces no series at all, rather than a 0 that would assert it has never moved. Join to tapelibrary_data_cartridge_info on volser for its state, location and partition.",
			[]string{"volser"}, nil,
		),
		cartMounts: prometheus.NewDesc(
			"tapelibrary_data_cartridge_usage_mounts_total",
			"Number of times this individual data cartridge has been loaded into a drive over its lifetime, as the library's own cumulative counter. Emitted only when --collector.data_cartridges_lifetime.per-volser is set. A cartridge with no usable reading produces no series at all.",
			[]string{"volser"}, nil,
		),
		cartWritten: prometheus.NewDesc(
			"tapelibrary_data_cartridge_usage_written_bytes_total",
			"Bytes written to this individual data cartridge over its lifetime, as the library's own cumulative counter converted from the API's decimal megabytes. Emitted only when --collector.data_cartridges_lifetime.per-volser is set. A cartridge with no usable reading produces no series at all.",
			[]string{"volser"}, nil,
		),
		cartErrors: prometheus.NewDesc(
			"tapelibrary_data_cartridge_usage_errors_total",
			"Lifetime error count for this individual data cartridge, by transfer direction and by whether the library was able to correct them, as the library's own cumulative counters. Emitted only when --collector.data_cartridges_lifetime.per-volser is set, and then for all four combinations. A cartridge with no usable reading produces no series at all.",
			[]string{"volser", "direction", "correction"}, nil,
		),
		lastRefreshDesc: prometheus.NewDesc(
			// Named for this collector's metric subsystem rather than for its
			// registered name, which are the same string on every other
			// collector in this exporter and differ only here (see the note at
			// the top of this file). An operator grepping
			// tapelibrary_data_cartridges_usage_ should find the freshness of
			// the data they are looking at in the same sweep.
			"tapelibrary_data_cartridges_usage_last_refresh_timestamp_seconds",
			"Unix time of the last successful data cartridges lifetime metrics refresh. Alert if time() - this > 2 x the collector's configured interval.",
			nil, nil,
		),
		done: make(chan struct{}),
	}
}

// Start launches the background refresh goroutine. Call once, after
// construction. The first refresh runs immediately (so the cache starts filling
// as soon as the process starts) without Start itself waiting for it: a slow
// first fetch never blocks process startup, which on this endpoint is the
// difference between a prompt boot and a minute of silence. The goroutine exits
// when ctx is cancelled; Done() can then be used to wait for it to finish.
func (c *DataCartridgesLifetimeCollector) Start(ctx context.Context) {
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
func (c *DataCartridgesLifetimeCollector) Done() <-chan struct{} {
	return c.done
}

// errorCounter returns the cartridge's counter for one (direction, correction)
// pair. Written as a lookup rather than as four inline field reads so the
// aggregate loop and the per-cartridge loop cannot disagree about which field
// belongs to which label pair — the kind of transposition that produces a
// perfectly plausible dashboard reporting read errors as write errors.
func (d *dataCartridgeUsageStats) errorCounter(direction, correction string) int64 {
	switch {
	case direction == "read" && correction == "corrected":
		return *d.ErrorsCorrectedRead
	case direction == "read" && correction == "uncorrected":
		return *d.ErrorsUncorrectedRead
	case direction == "write" && correction == "corrected":
		return *d.ErrorsCorrectedWrite
	default:
		return *d.ErrorsUncorrectedWrite
	}
}

// refresh performs the one I/O call (dataCartridgesLifetimeGetMetrics, via the
// injected *Client) and, on success, atomically replaces the cache. On error it
// logs and returns, leaving the previous cache and lastRefresh untouched,
// fail-open: a transient failure serves the last-known-good data instead of
// dropping the series, and the freshness gauge is the signal that a refresh is
// stale, not a dropped scrape.
//
// Every aggregate is computed from every parsed cartridge and emitted whatever
// perVolser says. Only the four per-cartridge families depend on it, which is
// what makes the flag a pure cardinality lever: flipping it off cannot silence
// DataCartridgeUncorrectedErrorsRising.
func (c *DataCartridgesLifetimeCollector) refresh(ctx context.Context) {
	carts, duplicated, err := c.dataCartridgesLifetimeGetMetrics(ctx)
	if err != nil {
		c.log.Error("Failed to refresh data cartridges lifetime metrics: keeping previous cache", "err", err)
		return
	}
	if len(duplicated) > 0 {
		// Logged once per refresh with the barcodes named, because the fix is
		// operational: two cartridges carrying one barcode cannot be told
		// apart by a human either. The count also ships as a metric, so this
		// is discoverable without reading logs.
		c.log.Warn("Duplicate volsers on the lifetime endpoint: excluding them from the per-cartridge series, aggregates are unaffected",
			"count", len(duplicated), "volsers", slices.Sorted(maps.Keys(duplicated)))
	}

	// Per cartridge, when perVolser is on: motion, mounts, written and the four
	// error combinations. Plus the aggregates, whose dominant term is the four
	// error histograms.
	const perCartridge = 3 + 4
	const aggregates = 3 + 4 + 2 + 1
	metrics := make([]prometheus.Metric, 0, len(carts)*perCartridge+aggregates)

	motion := newUsageHistogram(dataCartridgeUsageMotionBuckets)
	mounts := newUsageHistogram(dataCartridgeUsageMountsBuckets)
	written := newUsageHistogram(dataCartridgeUsageWrittenBuckets)

	type errorKey struct{ direction, correction string }
	errorHistograms := make(map[errorKey]*usageHistogram, len(dataCartridgeUsageDirections)*len(dataCartridgeUsageCorrections))
	for _, d := range dataCartridgeUsageDirections {
		for _, corr := range dataCartridgeUsageCorrections {
			errorHistograms[errorKey{d, corr}] = newUsageHistogram(dataCartridgeUsageErrorBuckets)
		}
	}

	unread, invalid := 0, 0

	// Indexed rather than ranged by value, same reason as
	// parseDataCartridgesLifetime above.
	for i := range carts {
		dc := &carts[i]

		if dc.unread() {
			unread++
			continue
		}
		if !dc.valid() {
			invalid++
			// Logged per cartridge per refresh, and worth the line: unlike the
			// unread cohort this is the library contradicting itself, and the
			// volser is what makes it actionable.
			c.log.Warn("Data cartridge reported an unusable lifetime record: excluding it from every usage histogram",
				"volser", dc.Volser, "internal_address", dc.InternalAddress)
			continue
		}

		motion.observe(float64(*dc.MotionMeters))
		mounts.observe(float64(*dc.Mounts))
		writtenBytes := float64(*dc.DataWrittenToCartridge) * dataCartridgeUsageMBToBytes
		written.observe(writtenBytes)

		for _, d := range dataCartridgeUsageDirections {
			for _, corr := range dataCartridgeUsageCorrections {
				errorHistograms[errorKey{d, corr}].observe(float64(dc.errorCounter(d, corr)))
			}
		}

		if !c.perVolser {
			continue
		}
		// The one place the missing `location` actually bites. Every other
		// cartridge collector keys on volser+location; this endpoint reports
		// no location, so a duplicated barcode cannot be resolved into two
		// series. Emitting both would give two metrics the same descriptor and
		// the same label set, which fails Registry.Gather for the WHOLE scrape
		// — all nineteen collectors, not just this one. Skipping the pair is
		// the narrowest possible response: it costs 2 series out of 9 674 on
		// the reference fleet and leaves every aggregate whole.
		if _, ambiguous := duplicated[dc.Volser]; ambiguous {
			continue
		}

		metrics = append(metrics,
			prometheus.MustNewConstMetric(c.cartMotion, prometheus.CounterValue, float64(*dc.MotionMeters), dc.Volser),
			prometheus.MustNewConstMetric(c.cartMounts, prometheus.CounterValue, float64(*dc.Mounts), dc.Volser),
			prometheus.MustNewConstMetric(c.cartWritten, prometheus.CounterValue, writtenBytes, dc.Volser),
		)
		for _, d := range dataCartridgeUsageDirections {
			for _, corr := range dataCartridgeUsageCorrections {
				metrics = append(metrics, prometheus.MustNewConstMetric(
					c.cartErrors, prometheus.CounterValue, float64(dc.errorCounter(d, corr)), dc.Volser, d, corr))
			}
		}
	}

	metrics = append(metrics,
		motion.metric(c.motion),
		mounts.metric(c.mounts),
		written.metric(c.written),
	)
	// Ranged over the ordered slices rather than over the map, so the emitted
	// order is deterministic. Registry.Gather sorts the exposition regardless,
	// but a deterministic cache keeps the tests reading the same way twice.
	for _, d := range dataCartridgeUsageDirections {
		for _, corr := range dataCartridgeUsageCorrections {
			metrics = append(metrics, errorHistograms[errorKey{d, corr}].metric(c.errors, d, corr))
		}
	}
	metrics = append(metrics,
		prometheus.MustNewConstMetric(c.unknown, prometheus.GaugeValue, float64(invalid), dataCartridgeUsageInvalid),
		prometheus.MustNewConstMetric(c.unknown, prometheus.GaugeValue, float64(unread), dataCartridgeUsageUnread),
		prometheus.MustNewConstMetric(c.duplicateVolsers, prometheus.GaugeValue, float64(len(duplicated))),
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
// The four per-cartridge descriptors are described even when
// --collector.data_cartridges_lifetime.per-volser is off: a descriptor is what
// this collector CAN emit, not what it did last time, and docs/metrics.md
// documents them on that basis.
func (c *DataCartridgesLifetimeCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.motion
	ch <- c.mounts
	ch <- c.written
	ch <- c.errors
	ch <- c.unknown
	ch <- c.duplicateVolsers
	ch <- c.cartMotion
	ch <- c.cartMounts
	ch <- c.cartWritten
	ch <- c.cartErrors
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
func (c *DataCartridgesLifetimeCollector) Collect(ch chan<- prometheus.Metric) {
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
