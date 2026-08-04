package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/sckyzo/tapelibrary_exporter/internal/logger"
)

// dataCartridgeStates is the set of values GET /v1/dataCartridges tabulates for
// its "state" field (TS4500 R1.11.2, "Get data cartridges", Table 1). Order is
// the manual's own priority order, not alphabetical; Registry.Gather sorts the
// exposition output by label value regardless, so this slice's order is
// invisible to a scrape and kept as-is to stay diffable against the manual.
//
// Two further states, cartridgeFailedMove and errorThresholdExceeded, are named
// in the manual's prose under library.cartridgeDegraded but appear in no table,
// and they are deliberately NOT listed here. On this collector the state family
// is keyed by logical library as well as by state, so every entry added to this
// slice costs one series per partition rather than one series outright, and
// pre-emitting a zero for a state the manual never tabulated is not worth that
// multiple. The emit-observed-anyway branch in stateCounts below is what covers
// them: should a library ever report either, it becomes its own series and a
// logged warning naming the value, which is exactly the case that branch exists
// for (see docs/exporter-journal.md, which flags these two by name).
var dataCartridgeStates = []string{
	"unknown",
	"failedVerification",
	"atEndOfLife",
	"assignmentRequired",
	"uncertainBarcode",
	"exportQueued",
	"importing",
	"verifying",
	"normal",
}

// dataCartridgeAccessValues is the ternary "accessible" field, shared with
// drives and every other cartridge type. Its value set is per-resource, not
// global (the rule AccessorsCollector established): on cartridges R1.11.2
// documents exactly these three.
var dataCartridgeAccessValues = []string{"normal", "limited", "no"}

// dataCartridgeEncryptedValues is the "encrypted" field's documented pair, plus
// the unknown token below. R1.11.2: "If yes, the data on the cartridge is
// encrypted and if no, the data on the cartridge is not encrypted. If the
// cartridge has not been mounted, this is null."
var dataCartridgeEncryptedValues = []string{"yes", "no", dataCartridgeUnknown}

// dataCartridgeWormValues is the "worm" field's documented pair, plus the
// unknown token. The API reports these as JSON *strings*, not booleans, so they
// pass through verbatim rather than being re-spelled true/false by this
// exporter.
var dataCartridgeWormValues = []string{"true", "false", dataCartridgeUnknown}

// dataCartridgeUnknown is the label value standing in for a field the library
// returned as null.
//
// 27 of the 60 cartridges in the 2026-07-28 capture report null for every
// cartridge-memory field at once — type, typeDescription, vendor,
// manufactureDate, sn, worm, format, density, densityCode, nativeCapacity and
// lifetimeRemaining — while still reporting volser, state, location, mediaType
// and mostRecentUsage. That is 45% of the sampled population, so it is the
// endpoint's ordinary shape rather than an edge case, and the aggregates below
// have to say something about it.
//
// Counting those cartridges under this token rather than dropping them keeps
// every typed breakdown summing to the library's real cartridge count. A
// dashboard panel showing 33 JD cartridges out of a parc of 60, with nothing on
// the wire explaining the missing 27, is the failure mode this avoids. The
// token is safe against collision on all three fields that use it: cartridge
// types are 2-character barcode codes (JD, L8, ...), worm is "true"/"false" and
// encrypted is "yes"/"no", so none of them can legitimately be the string
// "unknown".
//
// It is deliberately NOT used for lifetimeRemaining, which is a *number*: see
// lifetimeUnknown's descriptor below for why a missing life reading gets a
// counter of its own instead of a bucket.
const dataCartridgeUnknown = "unknown"

// dataCartridgeUnassigned is the logical_library label value for a cartridge
// R1.11.2 reports with a null logicalLibrary: "If it is not assigned to a
// logical library, this is null."
//
// A named token rather than an empty label, following the rule
// DrivesCollector's `operation` established: a null the manual assigns a
// meaning to is a documented VALUE and becomes a member of the enumeration,
// where a null the manual calls unknown suppresses the series instead. An
// unassigned cartridge is precisely what the assignmentRequired state reports
// on, so its count has to be readable rather than hidden under "".
//
// DrivesCollector carries the same logical_library label and decodes its null
// to "" by construction (a plain string field, no pointer), but all 40 drives
// in the capture are assigned, so that path has never run. This collector is
// where the convention is actually fixed; the theoretical collision (a site
// naming a real partition "unassigned") is accepted, and is visible as a
// partition of that name in /v1/logicalLibraries if it ever happens.
const dataCartridgeUnassigned = "unassigned"

// dataCartridgeUsageLayout is the timestamp format GET /v1/dataCartridges
// returns in its mostRecentUsage field: 2026-06-26T07:12:33+0000. Note the zone
// offset carries no colon, so this is NOT time.RFC3339 and parsing it as such
// fails. Reuses the constant DrivesCollector established rather than
// re-deriving a copy free to drift from it.
//
// R1.11.2 documents the format as YYYY-MM-DDThh:mm:ss±hh:mm, with a colon the
// wire does not actually carry. The capture is authoritative over the manual
// here, as it was for every other timestamp in this exporter.
const dataCartridgeUsageLayout = driveLastCleanedLayout

// dataCartridgeLifetimeBuckets are the upper bounds of the lifetime-remaining
// histogram, as a ratio in 0..1 rather than the API's 0..100 percentage (see
// docs/exporter-journal.md, "Metric name shape": Prometheus takes base units,
// and a _ratio is the base unit of a percentage).
//
// Bunched towards the bottom of the range on purpose: the operational question
// is "how many cartridges are close to end of life", never "how many are at
// 55% versus 60%". le="0" answers "how many are AT end of life" exactly, since
// R1.11.2 defines 0% as the at-risk point, and le="0.2" is the bound
// DataCartridgesWearingOut reads.
var dataCartridgeLifetimeBuckets = []float64{0, 0.1, 0.2, 0.3, 0.5, 0.75, 0.9, 1}

// dataCartridgeStats is the parsed shape of one GET /v1/dataCartridges entry.
// The endpoint returns one element per host-accessible or unassigned data
// cartridge — 9 749 on this library, of which the 2026-07-28 capture keeps 60.
// That population is bounded by library capacity rather than by any policy,
// which is what puts every per-cartridge series here behind
// --collector.data_cartridges.per-volser and leaves that flag OFF by default,
// the inverse of CleaningCartridgesCollector's flag of the same name.
//
// **`volser` is NOT a unique key on this endpoint, and must never be used as
// one.** R1.11.2 states plainly that "if there are duplicate VOLSERs, this
// value [internalAddress] is used to identify the cartridge", and the sibling
// cleaningCartridges capture proves the case is real rather than theoretical:
// 70 cartridges under 63 distinct volsers. The 2026-07-28 dataCartridges trim
// happens to carry 60 distinct volsers, but a 60-entry sample of a 9 749-entry
// inventory proves nothing about the full set, so this collector is keyed on
// volser AND location exactly as CleaningCartridgesCollector is, and
// parseDataCartridges rejects a duplicate of the PAIR.
//
// Getting that wrong here is worse than anywhere else in the exporter: two
// metrics sharing a descriptor and a label set fail Registry.Gather for the
// WHOLE scrape, so a duplicate barcode on the largest endpoint would take out
// all eighteen collectors at once rather than just this one.
//
// internalAddress is deliberately NOT parsed, even though it is the manual's
// nominated tie-breaker and genuinely unique. R1.11.2 documents it as changing
// "if the cartridge is assigned or unassigned from a logical library or if the
// cartridge is moved by the host or library", so as a label it would churn a
// fresh series out of every move while identifying nothing an operator can act
// on — at 9 749 cartridges, a churning label is the one thing this collector
// can least afford.
type dataCartridgeStats struct {
	// Volser is the cartridge's barcode. Half of this endpoint's key, and NOT
	// unique on its own: see the type comment above.
	Volser string `json:"volser"`

	// State is the cartridge's health status, one of dataCartridgeStates.
	// Counted per logical library rather than emitted as a per-cartridge
	// stateset: at 9 749 cartridges a full stateset would cost 87 741 series
	// (see docs/exporter-journal.md, "Cardinality budget", which makes the
	// tens-versus-thousands rule explicit).
	State string `json:"state"`

	// Accessible is the ternary reach field (normal, limited, no). Counted in
	// aggregate: on a dual-accessor library it is the only inventory-scale
	// signal that a robotics fault has put part of the parc out of reach.
	Accessible string `json:"accessible"`

	// Location is the cartridge's current position — a slot, an I/O slot, a
	// drive or a gripper. The other half of this endpoint's key.
	Location string `json:"location"`

	// MediaType is "3592" or "LTO" per R1.11.2. Always reported, including on
	// the cartridge-memory-absent cohort described at dataCartridgeUnknown.
	MediaType string `json:"mediaType"`

	// Type is the 2-character code at the end of the barcode (JD, L8, ...). A
	// pointer because it is one of the cartridge-memory fields the library
	// reports as null for 45% of the capture; a plain string would silently
	// decode that null to "" and emit a label no operator can read.
	Type *string `json:"type"`

	// Worm is "true" or "false" as a JSON string, or null on the
	// cartridge-memory-absent cohort. A WORM cartridge cannot be rewritten, so
	// the split matters for capacity planning even though nothing alerts on it.
	Worm *string `json:"worm"`

	// Encrypted is "yes" or "no", or null — R1.11.2: "If the cartridge has not
	// been mounted, this is null." Three of the capture's 60 are in that state,
	// a different and smaller set than the cartridge-memory cohort, which is
	// why every nullable field here is decoded independently rather than by
	// testing one of them and assuming the rest followed.
	Encrypted *string `json:"encrypted"`

	// LogicalLibrary is the partition this cartridge is assigned to, or null
	// when it is assigned to none. Decoded to dataCartridgeUnassigned rather
	// than "": see that constant.
	LogicalLibrary *string `json:"logicalLibrary"`

	// LifetimeRemaining is the estimated percentage of media life left, 0..100,
	// or null "if this is unknown". A pointer because that null is a MISSING
	// READING and not a value: decoded to a plain int it would become 0, which
	// R1.11.2 defines as the cartridge being at risk of data loss and out of
	// warranty. Silently reporting 45% of a library's parc as at end of life is
	// the single worst thing this collector could do, so the null is counted
	// separately (lifetimeUnknown) and kept out of the histogram entirely.
	LifetimeRemaining *int `json:"lifetimeRemaining"`

	// MostRecentUsage is the last time this cartridge was mounted into a drive,
	// or null "if this is unknown or the cartridge has not been mounted". A
	// zero timestamp would place that mount in 1970 and make every "unused
	// since" query quietly wrong, so a null produces no series at all.
	MostRecentUsage *string `json:"mostRecentUsage"`
}

// logicalLibrary returns the partition label for this cartridge, substituting
// the unassigned token for the API's null.
func (d *dataCartridgeStats) logicalLibrary() string {
	if d.LogicalLibrary == nil || *d.LogicalLibrary == "" {
		return dataCartridgeUnassigned
	}
	return *d.LogicalLibrary
}

// cartridgeType returns the barcode type code, substituting the unknown token
// for the API's null.
func (d *dataCartridgeStats) cartridgeType() string {
	return orUnknown(d.Type)
}

// orUnknown maps a nullable API string field onto a label value, substituting
// dataCartridgeUnknown for both null and the empty string. Shared by the three
// fields that take that token so the substitution cannot drift between them.
func orUnknown(s *string) string {
	if s == nil || *s == "" {
		return dataCartridgeUnknown
	}
	return *s
}

// dataCartridgesData is this collector's only I/O: it fetches the raw response
// body from the configured library. Kept separate from parsing
// (parseDataCartridges, below) so parsing stays pure and unit-testable without
// a live library.
func (c *DataCartridgesCollector) dataCartridgesData(ctx context.Context) ([]byte, error) {
	return c.client.Fetch(ctx, "/dataCartridges")
}

// parseDataCartridges decodes dataCartridgesData's response body into one
// dataCartridgeStats per data cartridge. Pure: no I/O, no logging, no side
// effects, so every input maps deterministically to an output. That is what
// makes it unit-testable with plain byte fixtures (see the test file's
// TestParseDataCartridges).
//
// Three inputs are rejected rather than passed through, all for the same
// reason — refresh keeps the previous cache on error, which is the right
// outcome for a response this collector cannot interpret:
//
//   - An entry with no volser, or none with no location. Either would emit a
//     series labelled with an empty half of the key, which no operator can
//     trace back to a physical cartridge, and which would silently collide
//     with any other entry missing the same half.
//   - Two entries sharing one volser AND location. Keyed on the pair, not on
//     volser alone, which R1.11.2 documents as legitimately shared by two
//     physically distinct cartridges. Failing closed here keeps a malformed
//     response from taking out every other collector's metrics too.
//   - An empty array. Unlike CleaningCartridgesCollector, which accepts [] and
//     is the only collector in this exporter that does, an empty
//     dataCartridges response is treated as content loss. The asymmetry is the
//     resource, not an oversight: cleaning cartridges are consumable and a
//     library legitimately runs out of them, which is the exact condition
//     CleaningCartridgesExhausted pages on, whereas a library reporting zero
//     data cartridges has either lost its entire inventory or returned a
//     truncated body over the slow SCSI/LCC path this endpoint reads. Serving
//     the previous cache and letting the freshness gauge go stale is the safer
//     reading of the two, and it is visible rather than silent: before the
//     first successful refresh there is no cache to serve, so the collector
//     emits only its freshness gauge at 0.
func parseDataCartridges(b []byte) ([]dataCartridgeStats, error) {
	var entries []dataCartridgeStats
	if err := json.Unmarshal(b, &entries); err != nil {
		return nil, fmt.Errorf("parse data cartridges response: %w", err)
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("parse data cartridges response: no cartridges returned (a library that reports none has lost its inventory or truncated the response; keeping the previous cache)")
	}

	type dataCartridgeKey struct{ volser, location string }
	seen := make(map[dataCartridgeKey]struct{}, len(entries))
	// Indexed rather than ranged by value: dataCartridgeStats carries five
	// strings and six pointers, so copying one per iteration is what
	// gocritic's rangeValCopy flags. refresh below takes the address for the
	// same reason.
	for i := range entries {
		e := &entries[i]
		if e.Volser == "" {
			return nil, fmt.Errorf("parse data cartridges response: entry at location %q with an empty volser", e.Location)
		}
		if e.Location == "" {
			return nil, fmt.Errorf("parse data cartridges response: entry with volser %q and an empty location", e.Volser)
		}
		key := dataCartridgeKey{e.Volser, e.Location}
		if _, dup := seen[key]; dup {
			return nil, fmt.Errorf("parse data cartridges response: duplicate cartridge %q at location %q", e.Volser, e.Location)
		}
		seen[key] = struct{}{}
	}
	return entries, nil
}

// dataCartridgesGetMetrics is the glue between the I/O step
// (dataCartridgesData) and the pure parsing step (parseDataCartridges): the
// shape every collector in this exporter follows, regardless of flavor. refresh,
// below, calls this on its own background schedule; nothing else in this file
// calls the library directly.
func (c *DataCartridgesCollector) dataCartridgesGetMetrics(ctx context.Context) ([]dataCartridgeStats, error) {
	data, err := c.dataCartridgesData(ctx)
	if err != nil {
		return nil, err
	}
	return parseDataCartridges(data)
}

// DataCartridgesCollector reads GET /v1/dataCartridges: what the library holds,
// how it is distributed across partitions and media, and how much useful life
// is left in it.
//
// This is the largest endpoint the exporter reads — 9 749 cartridges on this
// library, returned unpaginated over a slow SCSI/LCC-backed path — and the
// whole collector is shaped by that. Everything emitted by default is an
// AGGREGATE: counts by state and partition, by media and cartridge type, by
// encryption, by WORM and by accessor reach, plus a histogram of remaining
// media life. Per-cartridge detail exists but is gated behind
// --collector.data_cartridges.per-volser, off by default, because turning it on
// costs roughly 29 000 series per library and five times that across the fleet
// (see docs/exporter-journal.md, "Cardinality budget").
//
// It is the background-refresh variant, which on this target model is not a
// choice: a scrape serves N libraries through one /metrics and must never block
// on a machine that has gone away. A background goroutine (started by Start,
// below) refreshes a cached metric slice on a fixed interval, and Collect only
// ever reads that cache under mu. On this endpoint that is doubly true — a
// synchronous fetch of 9 749 entries would time out a scrape long before it
// returned.
type DataCartridgesCollector struct {
	client   *Client
	interval time.Duration
	log      *logger.Logger

	// perVolser gates the three per-cartridge series, and defaults to FALSE,
	// matching DrivesCollector's flag of the same name and inverting
	// CleaningCartridgesCollector's.
	//
	// The population is bounded by library capacity rather than by any policy:
	// 9 749 cartridges × 3 series is ~29 250 per library and ~146 000 across
	// the fleet, against the ~2 425 per library this exporter costs at
	// defaults. That is a Prometheus sizing decision in its own right, so it
	// is the operator's to take rather than this collector's.
	//
	// Every aggregate below is computed from the full parsed list and emitted
	// unconditionally, so turning the flag on adds detail and turning it off
	// costs detail — neither can silence an alert.
	perVolser bool

	stateCount      *prometheus.Desc
	mediaCount      *prometheus.Desc
	encryptionCount *prometheus.Desc
	wormCount       *prometheus.Desc
	accessCount     *prometheus.Desc
	lifetimeUnknown *prometheus.Desc
	lifetime        *prometheus.Desc
	cartridgeLife   *prometheus.Desc
	lastUsage       *prometheus.Desc
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

// NewDataCartridgesCollector builds the collector and its Descs. It is pure: it
// starts no goroutine and performs no I/O, which is what makes it constructible
// in tests with no background refresh running. Call Start once, after
// construction, to begin refreshing.
//
// Every metric here is a Gauge except the lifetime histogram. None of the
// counts is monotonic — a cartridge moves between states, partitions and the
// library itself — so none takes a _total suffix (see
// docs/exporter-journal.md, "Metric name shape": _total is reserved for the
// genuine device counters, which on cartridges live on
// /v1/dataCartridges/lifetimeMetrics and not here).
//
// The plural/singular split in the metric names is load-bearing rather than
// cosmetic, and follows the convention CleaningCartridgesCollector and
// LogicalLibrariesCollector already set: the SINGULAR subsystem
// (tapelibrary_data_cartridge_*) is per-cartridge and gated by perVolser, the
// PLURAL one (tapelibrary_data_cartridges*) is library-wide and always emitted.
// The two lifetime-remaining ratios differ by exactly that `s`, which is the
// same shape as tapelibrary_cleaning_cartridge_cleans_remaining against
// tapelibrary_cleaning_cartridges_cleans_remaining.
func NewDataCartridgesCollector(log *logger.Logger, client *Client, interval time.Duration, perVolser bool) *DataCartridgesCollector {
	return &DataCartridgesCollector{
		client:    client,
		interval:  interval,
		log:       log,
		perVolser: perVolser,
		stateCount: prometheus.NewDesc(
			"tapelibrary_data_cartridges",
			"Number of data cartridges the library holds in each documented state, per logical library. Every documented state is emitted for every partition seen, whether or not any cartridge is in it, so a rule matching a state still has a series to read when its count reaches zero. A cartridge assigned to no partition is counted under logical_library=\"unassigned\".",
			[]string{"state", "logical_library"}, nil,
		),
		mediaCount: prometheus.NewDesc(
			"tapelibrary_data_cartridges_media",
			"Number of data cartridges of each media and cartridge type. Only combinations the library actually reports are emitted: the manual documents 28 cartridge type codes and a site runs one or two. A cartridge whose type the library has not read from cartridge memory is counted under cartridge_type=\"unknown\" rather than dropped, so these counts always sum to the library's real cartridge count.",
			[]string{"media_type", "cartridge_type"}, nil,
		),
		encryptionCount: prometheus.NewDesc(
			"tapelibrary_data_cartridges_encryption",
			"Number of data cartridges by encryption state. Always emitted for all three values. encrypted=\"unknown\" is the manual's null, which it defines as a cartridge that has not been mounted, and is a distinct fact from \"no\": nothing is known about the cartridge's contents either way.",
			[]string{"encrypted"}, nil,
		),
		wormCount: prometheus.NewDesc(
			"tapelibrary_data_cartridges_worm",
			"Number of data cartridges by write-once (WORM) status. Always emitted for all three values. worm=\"unknown\" is a cartridge whose cartridge memory the library has not read.",
			[]string{"worm"}, nil,
		),
		accessCount: prometheus.NewDesc(
			"tapelibrary_data_cartridges_access",
			"Number of data cartridges by accessor reach. Always emitted for all three documented values. On a dual-accessor library a non-zero access=\"limited\" or access=\"no\" count is the only inventory-scale signal that a robotics fault has put part of the cartridge parc out of reach.",
			[]string{"access"}, nil,
		),
		lifetimeUnknown: prometheus.NewDesc(
			"tapelibrary_data_cartridges_lifetime_unknown",
			"Number of data cartridges for which the library reports no remaining-life reading. Always emitted. These are excluded from tapelibrary_data_cartridges_lifetime_remaining_ratio entirely, so this value plus that histogram's _count is the library's full cartridge parc; without it, the histogram would read as covering every cartridge when it can cover only those whose cartridge memory has been read.",
			nil, nil,
		),
		lifetime: prometheus.NewDesc(
			"tapelibrary_data_cartridges_lifetime_remaining_ratio",
			"Distribution of remaining media life across the library's data cartridges, as a ratio from 0 (at end of life, at risk of data loss and out of warranty) to 1 (unused). Converted from the API's 0-100 percentage. Cartridges the library reports no reading for are counted in tapelibrary_data_cartridges_lifetime_unknown instead of being observed here as 0, which would falsely report them as spent.",
			nil, nil,
		),
		cartridgeLife: prometheus.NewDesc(
			"tapelibrary_data_cartridge_lifetime_remaining_ratio",
			"Remaining media life of this individual data cartridge, as a ratio from 0 to 1. Emitted only when --collector.data_cartridges.per-volser is set, which it is not by default. A cartridge the library reports no reading for produces no series at all, rather than a 0 that would falsely mark it spent. Keyed by volser AND location: a barcode is not unique in a tape library.",
			[]string{"volser", "location"}, nil,
		),
		lastUsage: prometheus.NewDesc(
			"tapelibrary_data_cartridge_last_usage_timestamp_seconds",
			"Unix time at which this data cartridge was last mounted into a drive. A cartridge the library reports no usage for produces no series at all, rather than a 0 that would place its last mount in 1970. Emitted only when --collector.data_cartridges.per-volser is set.",
			[]string{"volser", "location"}, nil,
		),
		info: prometheus.NewDesc(
			"tapelibrary_data_cartridge_info",
			"Always 1. Carries this data cartridge's current state, partition and identity as labels, joinable to the per-cartridge measurements on volser and location. Emitted only when --collector.data_cartridges.per-volser is set.",
			[]string{"volser", "location", "state", "logical_library", "media_type", "cartridge_type", "access", "encrypted", "worm"}, nil,
		),
		lastRefreshDesc: prometheus.NewDesc(
			"tapelibrary_data_cartridges_last_refresh_timestamp_seconds",
			"Unix time of the last successful data cartridges refresh. Alert if time() - this > 2 x the collector's configured interval.",
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
func (c *DataCartridgesCollector) Start(ctx context.Context) {
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
func (c *DataCartridgesCollector) Done() <-chan struct{} {
	return c.done
}

// stateCounts emits one series per documented state for every logical library
// observed, carrying the number of cartridges in it, plus one per observed
// state the manual does not tabulate.
//
// This is the aggregate counterpart of the per-object stateset the hardware
// collectors in this exporter emit, and the budget's tens-versus-thousands rule
// is what selects it: at 9 749 cartridges a per-cartridge stateset would cost
// 87 741 series to say what these say, and the active state is on each
// cartridge's own _info series anyway for anyone running with --per-volser.
//
// Every documented state is emitted for every partition seen whether or not any
// cartridge is in it, so a rule matching state="atEndOfLife" still has a series
// to read when the count falls to zero — an absent series satisfies no matcher,
// so a library that had just migrated its last spent cartridge would otherwise
// go quiet rather than resolve.
//
// The partitions themselves are read from the response rather than enumerated,
// because this collector has no view of /v1/logicalLibraries. A partition
// holding no cartridges at all is therefore absent from this family entirely,
// which is the one gap in the always-emitted property above and is visible in
// LogicalLibrariesCollector's own tapelibrary_logical_library_cartridges.
func (c *DataCartridgesCollector) stateCounts(metrics []prometheus.Metric, carts []dataCartridgeStats) []prometheus.Metric {
	type stateKey struct{ logicalLibrary, state string }

	counts := make(map[stateKey]int, len(dataCartridgeStates))
	partitions := make(map[string]struct{}, 4)
	for i := range carts {
		ll := carts[i].logicalLibrary()
		partitions[ll] = struct{}{}
		counts[stateKey{ll, carts[i].State}]++
	}

	// Sorted rather than ranged over the map directly: map order is random,
	// and the warning loop below names its states in a log a human reads.
	for _, ll := range slices.Sorted(maps.Keys(partitions)) {
		for _, s := range dataCartridgeStates {
			key := stateKey{ll, s}
			metrics = append(metrics, prometheus.MustNewConstMetric(
				c.stateCount, prometheus.GaugeValue, float64(counts[key]), s, ll))
			// Deleting as we go leaves exactly the undocumented pairs behind,
			// which is also what keeps the loop below from duplicating a label
			// set already emitted here and failing Gather for the whole scrape.
			delete(counts, key)
		}
	}

	remaining := slices.SortedFunc(maps.Keys(counts), func(a, b stateKey) int {
		if a.logicalLibrary != b.logicalLibrary {
			return strings.Compare(a.logicalLibrary, b.logicalLibrary)
		}
		return strings.Compare(a.state, b.state)
	})
	for _, key := range remaining {
		if key.state == "" {
			// An entry with no state at all. It is counted nowhere rather
			// than emitted under state="", which would be a series no
			// operator can act on.
			c.log.Warn("Data cartridges reported with an empty state: counted in no state series",
				"logical_library", key.logicalLibrary, "count", counts[key])
			continue
		}
		// The manual's tables are demonstrably a floor rather than a ceiling —
		// it names cartridgeFailedMove and errorThresholdExceeded in prose
		// under library.cartridgeDegraded and tabulates neither — so an
		// unlisted state surfaces as its own series and a logged warning
		// rather than silently vanishing from the counts.
		c.log.Warn("Data cartridge reported a state absent from the documented set: emitting it anyway",
			"state", key.state, "logical_library", key.logicalLibrary, "count", counts[key])
		metrics = append(metrics, prometheus.MustNewConstMetric(
			c.stateCount, prometheus.GaugeValue, float64(counts[key]), key.state, key.logicalLibrary))
	}
	return metrics
}

// mediaCounts emits one series per (media type, cartridge type) pair the
// library actually reports.
//
// Unlike every other aggregate family here this one is NOT a full cross product
// of documented values: R1.11.2 lists 16 LTO and 12 3592 type codes, so
// pre-emitting zeros would cost 56 series per library to describe an inventory
// that in practice runs one or two types. Nothing alerts on a particular type,
// so there is no matcher that needs a zero to read.
func (c *DataCartridgesCollector) mediaCounts(metrics []prometheus.Metric, carts []dataCartridgeStats) []prometheus.Metric {
	type mediaKey struct{ mediaType, cartridgeType string }

	counts := make(map[mediaKey]int, 4)
	for i := range carts {
		counts[mediaKey{carts[i].MediaType, carts[i].cartridgeType()}]++
	}

	keys := slices.SortedFunc(maps.Keys(counts), func(a, b mediaKey) int {
		if a.mediaType != b.mediaType {
			return strings.Compare(a.mediaType, b.mediaType)
		}
		return strings.Compare(a.cartridgeType, b.cartridgeType)
	})
	for _, k := range keys {
		metrics = append(metrics, prometheus.MustNewConstMetric(
			c.mediaCount, prometheus.GaugeValue, float64(counts[k]), k.mediaType, k.cartridgeType))
	}
	return metrics
}

// valueCounts emits one series per documented value of a single-label
// enumeration, counting how many cartridges carry each, plus one per observed
// value the manual does not document.
//
// Shared by the encryption, WORM and access families, which differ only in
// their descriptor and their value set. Written once rather than three times so
// the emit-observed-anyway branch — the thing that catches the manual being
// wrong — cannot be present on one family and forgotten on another.
func (c *DataCartridgesCollector) valueCounts(
	metrics []prometheus.Metric,
	desc *prometheus.Desc,
	field string,
	known []string,
	observed map[string]int,
) []prometheus.Metric {
	for _, v := range known {
		metrics = append(metrics, prometheus.MustNewConstMetric(
			desc, prometheus.GaugeValue, float64(observed[v]), v))
		delete(observed, v)
	}
	for _, v := range slices.Sorted(maps.Keys(observed)) {
		c.log.Warn("Data cartridge reported a value absent from the documented set: emitting it anyway",
			"field", field, "value", v, "count", observed[v])
		metrics = append(metrics, prometheus.MustNewConstMetric(
			desc, prometheus.GaugeValue, float64(observed[v]), v))
	}
	return metrics
}

// parseMostRecentUsage turns the API's mostRecentUsage string into an instant,
// or reports that no series should be emitted. Null and empty are the
// documented "never mounted, or the library has lost the record" case and pass
// silently; anything else that fails to parse is a response this collector does
// not understand and is logged once per refresh, per cartridge.
func (c *DataCartridgesCollector) parseMostRecentUsage(dc *dataCartridgeStats) (time.Time, bool) {
	if dc.MostRecentUsage == nil || *dc.MostRecentUsage == "" {
		return time.Time{}, false
	}
	ts, err := time.Parse(dataCartridgeUsageLayout, *dc.MostRecentUsage)
	if err != nil {
		c.log.Warn("Data cartridge reported an unparseable mostRecentUsage timestamp: emitting no series for it",
			"volser", dc.Volser, "location", dc.Location, "value", *dc.MostRecentUsage, "err", err)
		return time.Time{}, false
	}
	return ts, true
}

// refresh performs the one I/O call (dataCartridgesGetMetrics, via the injected
// *Client) and, on success, atomically replaces the cache. On error it logs and
// returns, leaving the previous cache and lastRefresh untouched, fail-open: a
// transient failure serves the last-known-good data instead of dropping the
// series, and the freshness gauge is the signal that a refresh is stale, not a
// dropped scrape.
//
// Every aggregate is computed from every parsed cartridge and emitted whatever
// perVolser says. Only the three per-cartridge families depend on it, which is
// what makes the flag a pure cardinality lever: flipping it off cannot silence
// DataCartridgesDegraded or DataCartridgesWearingOut.
func (c *DataCartridgesCollector) refresh(ctx context.Context) {
	carts, err := c.dataCartridgesGetMetrics(ctx)
	if err != nil {
		c.log.Error("Failed to refresh data cartridges metrics: keeping previous cache", "err", err)
		return
	}

	// Per cartridge, when perVolser is on: an _info, a lifetime ratio and a
	// last-usage timestamp. Plus the library-wide aggregates, whose dominant
	// term is the state family at one series per documented state per
	// partition.
	const perCartridge = 3
	aggregates := len(dataCartridgeStates)*4 + len(dataCartridgeAccessValues) +
		len(dataCartridgeEncryptedValues) + len(dataCartridgeWormValues) + 8
	metrics := make([]prometheus.Metric, 0, len(carts)*perCartridge+aggregates)

	encrypted := make(map[string]int, len(dataCartridgeEncryptedValues))
	worm := make(map[string]int, len(dataCartridgeWormValues))
	access := make(map[string]int, len(dataCartridgeAccessValues))

	// Histogram accumulators. Bucket counts are cumulative, which is what
	// MustNewConstHistogram expects: each bound holds the number of
	// observations less than or equal to it.
	buckets := make(map[float64]uint64, len(dataCartridgeLifetimeBuckets))
	var lifetimeCount uint64
	var lifetimeSum float64
	unknownLifetime := 0

	// Indexed rather than ranged by value, same reason as parseDataCartridges
	// above. The inner bucket loop is over a fixed 8-element slice, so this
	// stays O(n) rather than becoming a nested scan.
	for i := range carts {
		dc := &carts[i]

		encrypted[orUnknown(dc.Encrypted)]++
		worm[orUnknown(dc.Worm)]++
		access[dc.Accessible]++

		ratio, hasLife := 0.0, dc.LifetimeRemaining != nil
		if hasLife {
			// The API reports a 0-100 percentage; Prometheus takes ratios.
			ratio = float64(*dc.LifetimeRemaining) / 100
			lifetimeCount++
			lifetimeSum += ratio
			for _, b := range dataCartridgeLifetimeBuckets {
				if ratio <= b {
					buckets[b]++
				}
			}
		} else {
			unknownLifetime++
		}

		if !c.perVolser {
			continue
		}

		metrics = append(metrics, prometheus.MustNewConstMetric(
			c.info, prometheus.GaugeValue, 1,
			dc.Volser, dc.Location, dc.State, dc.logicalLibrary(), dc.MediaType,
			dc.cartridgeType(), dc.Accessible, orUnknown(dc.Encrypted), orUnknown(dc.Worm),
		))

		// Absent, never zero: a cartridge with no reading is not a spent one.
		if hasLife {
			metrics = append(metrics, prometheus.MustNewConstMetric(
				c.cartridgeLife, prometheus.GaugeValue, ratio, dc.Volser, dc.Location))
		}

		if ts, ok := c.parseMostRecentUsage(dc); ok {
			metrics = append(metrics, prometheus.MustNewConstMetric(
				c.lastUsage, prometheus.GaugeValue, float64(ts.Unix()), dc.Volser, dc.Location))
		}
	}

	// A bound no observation reached still needs its (zero) entry, or the
	// exposition would skip that bucket entirely and every le= a rule names
	// would have to be one the fixture happened to exercise.
	for _, b := range dataCartridgeLifetimeBuckets {
		if _, ok := buckets[b]; !ok {
			buckets[b] = 0
		}
	}

	metrics = c.stateCounts(metrics, carts)
	metrics = c.mediaCounts(metrics, carts)
	metrics = c.valueCounts(metrics, c.encryptionCount, "encrypted", dataCartridgeEncryptedValues, encrypted)
	metrics = c.valueCounts(metrics, c.wormCount, "worm", dataCartridgeWormValues, worm)
	metrics = c.valueCounts(metrics, c.accessCount, "accessible", dataCartridgeAccessValues, access)
	metrics = append(metrics,
		prometheus.MustNewConstMetric(c.lifetimeUnknown, prometheus.GaugeValue, float64(unknownLifetime)),
		prometheus.MustNewConstHistogram(c.lifetime, lifetimeCount, lifetimeSum, buckets),
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
// The three per-cartridge descriptors are described even when
// --collector.data_cartridges.per-volser is off: a descriptor is what this
// collector CAN emit, not what it did last time, and docs/metrics.md documents
// them on that basis.
func (c *DataCartridgesCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.stateCount
	ch <- c.mediaCount
	ch <- c.encryptionCount
	ch <- c.wormCount
	ch <- c.accessCount
	ch <- c.lifetimeUnknown
	ch <- c.lifetime
	ch <- c.cartridgeLife
	ch <- c.lastUsage
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
// startup window, not a failure. On this endpoint that window is the longest in
// the exporter — 9 749 entries over a slow path — so the guarantee matters more
// here than anywhere else. The gauge's own value (0 until the first refresh
// lands) is the separate, correct signal for staleness.
func (c *DataCartridgesCollector) Collect(ch chan<- prometheus.Metric) {
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
