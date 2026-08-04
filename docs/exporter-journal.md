# Exporter journal: tapelibrary_exporter

## Provenance

- **Grounded by:**
  - **Rung 2 (docs, medium confidence)** — `samples/ts4500-tape-library-1.11.2-documentation.pdf`,
    section 6 ("How to use REST API" / "URL endpoints and resources"). Documents the
    RoE base URL `https://<IP>/web/api/v1/<endpoint>`, the `POST /v1/login`
    `{"user","password"}` session handshake and its `POST /v1/logout` counterpart,
    every `GET` endpoint with its response fields, the location-attribute grammar
    (`drive_F<f>C<c>R<r>`, `slot_F<f>C<c>R<r>T<t>`, `frame_F<f>`, `accessor_A<a|b>`,
    `powerSupply_F<f>PS<a|b>`, `ioStation_F<f>IO<u|l>`), and the error-response shape.
  - **Rung 4 material (live capture, high fidelity)** — `samples/test_data/probe-2026-07-28/`,
    21 JSON captures taken from a real library on 2026-07-28 and anonymised. Reused as
    captured; **no fresh probe was run this session** (see Skipped). These supplied
    every response shape, enum value and volume figure the PDF states only in prose:
    real per-endpoint entry counts, the `duration: 3600` hourly-window semantics of
    `/v1/reports/*`, the null-heavy fields on `accessors` (no temperature/humidity
    sensor reporting on this hardware), and the empty `sasPorts` array.
  - **Legacy monitoring (supplementary)** — `samples/legacy/`, copied out of the
    ephemeral `/tmp/lib/` during this session. Five textfile-collector scripts over
    RoS/ITDT, three `.prom` outputs, and `library-alerts.yml` (8 live rules). Their
    `declare -A` maps carry the operational state→severity classification for
    `library`, `accessor`, `frame`, `drive` and `nodeCard` — knowledge that exists
    nowhere in IBM's documentation and is the single most valuable input to
    `## Architecture decisions`' alert candidates below.

- **Skipped:**
  - **Rung 1 (local API spec)** — IBM publishes no OpenAPI/Swagger document and no
    `.proto` for the RoE API; confirmed with the maintainer. Section 6 of the manual
    is the only endpoint contract that exists.
  - **Rung 3 (context7)** — `resolve-library-id` returned no entry for the TS4500 or
    its RoE API. The nearest matches (`/websites/ibm_en_3592-enterprise-tape`,
    `/ibm/tape-automation`, `/websites/ibm_en_cloud-tape-connector`) describe
    different products; none was substituted, and nothing was drawn from memory in
    their place.
  - **Rung 4 (live re-probe)** — declined by the maintainer. The 2026-07-28 capture is
    real output from the real hardware and was judged sufficient, so no live
    instance was contacted during this session and `scripts/probe-target.sh` was
    never invoked here.
  - **Rung 5 (dialogue)** — not needed as a *grounding* rung; used only for the six
    architecture decisions, which are the maintainer's to make regardless of how
    well-grounded the target's API surface is.

- **Confidence: high.** Every endpoint that becomes a collector below has both a
  documented contract (rung 2) and a real captured response (rung 4 material). The
  residual uncertainty is volume, not shape: the captures are truncated samples, so
  worst-case series counts are derived from `library.json`'s own `totalCapacity`
  (10 732) and `totalCartridges` (9 749) counters rather than from the captures'
  lengths. See `## Open questions / assumptions`.

- **Source material:**
  - `samples/ts4500-tape-library-1.11.2-documentation.pdf` (IBM TS4500 R1.11.2, 466 pp.)
  - `samples/test_data/probe-2026-07-28/*.json` (21 endpoint captures, anonymised)
  - `samples/legacy/` (5 RoS/ITDT scripts, 3 `.prom` outputs, `library-alerts.yml`)
  - Original of the manual: `<workspace>/tools/hpss/docs/library/`
  - Reference exporter for layout and CI: `../slurm_exporter/slurm_exporter/`

## Architecture decisions

- **Data source:** REST API — REST over Ethernet (RoE), `https://<library>/web/api/v1`.
  Chosen at the top of the preference order; the target exposes a documented,
  versioned HTTP interface. **Read-only by construction:** the only non-`GET`
  requests the exporter ever issues are `POST /v1/login` and `POST /v1/logout`.
  `POST /v1/tasks` and `POST /v1/workItems` are out of scope permanently — a task
  moves physical media, and the library allows exactly one task in progress at a
  time library-wide, so an exporter that started one would block the operators'
  own work.
- **I/O flavor:** `http`. Follows directly from the data source, and is required by
  the target model below.
- **Target model:** `multi-instance`. The five libraries are physically distinct
  units at fixed addresses, known at boot, and the heavy endpoints
  (`dataCartridges`, `slots`, `dataCartridges/lifetimeMetrics`) return thousands of
  unpaginated entries over a slow SCSI/LCC-backed path. Those cannot be refreshed
  synchronously per scrape, and they cannot be refreshed by simply widening
  `scrape_interval` either: Prometheus's 5-minute staleness window would make the
  series flicker in and out of existence between refreshes. Background pollers plus
  a per-instance cache re-served at scrape cadence is the only shape that fits.
  `--config.file` is therefore required at runtime, and **every collector on this
  build is the `background` variant by construction**, not by choice.
- **Instance identity:** `--instance-label library`, whose value is the name given to
  each `instances:` entry. `model: ts4500` is carried in each instance's own
  `labels:` map rather than emitted by any collector, which both keeps the
  namespace generic for a future non-TS4500 library and satisfies the
  multi-instance rule that an instance's `labels:` may not reuse a key a collector
  already emits. Every instance must declare the same label key set; adding a key
  later requires a restart, not a reload.
- **Credential convention:** one shared monitoring account across all five
  libraries. `config.example.yml` therefore demonstrates a single
  `http_client_config:` covering every instance, with no per-instance `modules:`
  section. (The `a|b|c` convention proper is a `multi`-model question and does not
  apply here.) Secrets are supported in three forms — `password_file` (recommended
  and shown in the example), environment variable, and cleartext, the last warning
  at startup.
- **Concurrency ceiling: 1.** Each watched instance is bounded independently to one
  in-flight request at a time. The LCC and the robotics path serialize internally,
  so parallel requests buy nothing and risk degrading the machine the exporter is
  supposed to observe. The accepted cost is that a slow collector delays its
  siblings on the same library; that delay is observable rather than hidden, via
  `tapelibrary_exporter_request_wait_seconds` and the per-instance freshness gauge.
- **Metric name shape:** `tapelibrary_<subsystem>_<name>_<unit>`, where `<subsystem>`
  is the resource the collector reads (`library`, `frame`, `accessor`, `drive`,
  `slot`, `cartridge`, `event`, `report`…). Unit suffixes only where the quantity
  has a physical dimension (`_seconds`, `_bytes`, `_meters`, `_ratio`); bare counts
  carry none. Genuine monotonic device counters (`accessors.pivots`,
  `slots.puts`, `slots.putRetries`) are emitted as `prometheus.CounterValue` with a
  `_total` suffix; everything the device reports as a snapshot is a Gauge.
  - **A rate the device reports in bits becomes `_bytes_per_second`, not
    `_bits_per_second`** (decided 2026-08-01 with the maintainer, on the
    `fc_ports` collector, the first to carry a rate at all). `/v1/fcPorts`
    reports `speedActual` as a Gbps *string* (`16Gbps`), and it is converted at
    8 bits per byte: Prometheus takes bytes as its base unit, and the shape
    above already reserves `_bytes` while listing no `_bits`. The cost is
    stated rather than hidden, in the descriptor's own help text and in
    `docs/metrics.md`: 16GFC signals at 16 Gbit/s but moves roughly 1600 MB/s
    of payload after 64b/66b encoding and framing, so the value is a **nominal
    signalling rate** and dividing traffic by it does not give a utilisation
    ratio. Binding on any future collector reporting a link or transfer rate.
  - **The subsystem names the RESOURCE, not the registered collector name**
    (restated 2026-08-02 with the maintainer, on `reports_library`). The two
    coincide for fourteen of the seventeen collectors simply because those are
    named after what they read, which made "subsystem = registered name" look
    like the rule until a third exception arrived. `reports_library` emits
    `tapelibrary_library_report_*`, and its two siblings will emit
    `..._drive_report_*` and `..._accessor_report_*`: the resource stays at the
    front, where every other family in this exporter already puts it
    (`..._drive_state`, `..._accessor_humidity_ratio`), with `report` as the
    qualifier. The flag namespace still follows the endpoint
    (`--collector.reports_library.*`), so the two are deliberately allowed to
    differ. `data_cartridges_lifetime` remains an exception on separate grounds
    (a prefix collision), not an instance of this one.
  - **A statistic is never a label** (decided 2026-08-02 on `reports_library`,
    checked against Prometheus's own exporter-writing guidance rather than
    settled by preference). `/v1/reports/*` reports temperature and humidity as
    average/min/max triplets, and the tempting shape is one metric carrying
    `stat="average|min|max"`. It is wrong by Prometheus's own test: **a metric
    should make sense when summed or averaged across its labels**, and summing
    an average with a minimum and a maximum is meaningless — the same reason
    that guidance prefers separate families over a
    `{result="success"|"failure"}` label. Six metric names ship instead
    (`..._temperature_average_celsius`, `_min_`, `_max_`, and the humidity
    triplet), which also lets an alert name the statistic it means in the
    metric rather than in a matcher. **No `stat` key is therefore added to the
    shared label vocabulary above**, and the same reasoning binds
    `reports_drives` and `reports_accessors`, which carry the same triplets.
  - **read/write is a metric-name distinction, not a label** (decided 2026-08-03
    on `reports_drives`, against the maintainer's instruction to follow
    Prometheus's own guidance rather than this repository's habit). Prometheus's
    *Writing Exporters* page — the same document the `stat` rule above is checked
    against — names **read/write and send/receive** as its canonical example of
    "related but distinct concepts" that are easier to use as separate metrics
    than combined under one label. `reports_drives` therefore ships
    `..._errors_corrected_read` and `..._errors_corrected_write` as two names,
    not `..._errors_corrected{direction=…}`.
    **This does not retract `data_cartridges_lifetime`'s `{direction, correction}`
    pair, and the difference between the two cases is the rule worth carrying
    forward.** There, R1.11.2 reports a *complete* cross product — corrected and
    uncorrected, each split read and write — so the labels describe a real
    two-dimensional table and a histogram over it is the shape of the data.
    `/v1/reports/drives` reports `errorsUncorrected` with **no direction
    breakdown at all**, so the same pair of labels would have needed a synthetic
    `direction="all"` the library never sends, and summing across `direction`
    would then double-count. **The test is whether the source splits the
    dimension completely: it does → labels are available; it does not → separate
    names, and never a placeholder value invented to square the table.**
    Binding on `reports_accessors`, whose gets/puts pair is the same shape and
    should ship as two names rather than an `operation` label.
- **Shared label vocabulary:** `library`, `model`, `location`, `state`, `operation`,
  `access`, `logical_library`, `media_type`, `cartridge_type`, `frame_type`,
  `card_type`, `severity`, `gripper`, `axis`, `door`, `direction`, `correction`,
  `drive_location`, `encryption_method`, `tiers`, `error_code`, `reason`
  — plus `volser`, reserved exclusively for the opt-in per-cartridge detail and for
  the bounded `cleaning_cartridges` collector.
  - **`error_code` (added 2026-08-01 by the `events` collector) is the second label
    reserved to an opt-in flag**, on the same terms as `volser` and for the same
    reason: its value set is not bounded by anything the exporter can see. R1.11.2
    describes `errorCode` as a 4-digit hex code and enumerates none of the 65 536 it
    admits, so it appears only on `tapelibrary_events_by_code` and only for codes an
    operator names in `--collector.events.error-codes` (empty by default). Matching
    is case-insensitive but the label carries the library's own spelling, so nothing
    on the wire is rewritten by that convenience. Binding on any future collector
    tempted to label by a code the manual does not enumerate.
  - **`correction` and `reason` (added 2026-08-01 by the `data_cartridges_lifetime`
    collector).** `correction` takes `corrected`/`uncorrected` and exists only
    alongside `direction`, on the cartridge error counters: R1.11.2 reports the cross
    product (`errorsCorrectedRead`, `errorsCorrectedWrite`, `errorsUncorrectedRead`,
    `errorsUncorrectedWrite`), so one family with two labels is the shape of the data
    rather than a naming preference, and it lets a rule select every uncorrected error
    in one matcher. `reason` takes `unread`/`invalid` on
    `tapelibrary_data_cartridges_usage_unknown` and answers "why is this object not in
    the distribution". **The rule it sets for the collectors still unbuilt: where an
    object can be excluded from an aggregate for more than one reason, and the reasons
    differ in whether an operator should care, the exclusion counter takes a `reason`
    label rather than being one number.** `data_cartridges`'
    `..._lifetime_unknown` predates this and stays unlabelled, correctly — it has only
    one reason. Merging a 39% benign baseline with a genuine cartridge-memory fault
    into a single gauge would have made the fault unalertable, which is exactly what
    the split avoids.
  - **`tiers` (added 2026-08-01 by the `slots` collector) is the first label in this
    vocabulary whose values are NUMBERS**, and the reason it is a label rather than a
    gauge is worth recording. A slot's depth is physical geometry: 1 for a single-deep
    slot, 4 or 5 for a high-density one, changing only when a frame is added or removed.
    As a gauge per slot it would cost one series per slot to report a constant; as a
    label on the depth distribution it costs one series per DISTINCT depth (2 on this
    fleet), and as a label on the per-slot `_info` it rides for free beside the state
    that is already there. The rule this sets for the collectors still unbuilt: a small,
    bounded, constant-per-object integer belongs on `_info` and on a distribution, never
    on a measurement series of its own. The bound is what matters — `elementAddress`,
    which `data_cartridges` also sees, is an integer too and would be a catastrophic
    label.
  - **`location` on the `slots` collector is NOT in the same key space as `location`
    everywhere else** (established 2026-08-01, and the sharpest trap this vocabulary
    has). Every other collector's `location` is the library's own native string for the
    object it collects, and two collectors naming the same physical thing spell it the
    same way. `/v1/slots` breaks that: it reports `slot_F2C1R1`, a column, while
    `/v1/dataCartridges` reports `slot_F7C3R15T1`, a tiered position inside one. A join
    between them needs the tier suffix appended to the slots side, and a join written
    without it silently matches nothing rather than failing. This is not a new label key
    — inventing `slot_location` would have been worse, since the value genuinely IS the
    library's native location string for a slot — but every dashboard or rule crossing
    the two must know it. `drive_location`'s rule (a cross-reference to another
    resource's location is spelled `<subsystem>_location`) does not apply here, because
    this is the resource's OWN location; the two are simply at different granularities.
  - **`volser` IS NOT A UNIQUE KEY, on any cartridge endpoint** (established
    2026-08-01 by the `cleaning_cartridges` collector, and binding on
    `data_cartridges` and `diagnostic_cartridges`, both still unbuilt and both
    planning a `volser` label). The 2026-07-28 `cleaningCartridges` capture holds
    70 cartridges under 63 distinct volsers: seven barcodes appear twice, at
    different locations, with different `cleansRemaining`. This is not a capture
    artifact and not a fault at this site — R1.11.2's own worked example for the
    endpoint prints two `CLNI01L1` entries side by side, and every cartridge
    endpoint's attribute list repeats the same sentence: "if there are duplicate
    VOLSERs, this value [`internalAddress`] is used to identify the cartridge."
    The pair `volser` + `location` is the key, and the parser must reject a
    duplicate of the PAIR: two metrics sharing a descriptor and a label set fail
    `Registry.Gather` for the whole scrape, so getting this wrong on
    `data_cartridges` would take out all 18 collectors at once rather than just
    its own. The legacy scripts under `samples/legacy/` did emit the duplicate
    series — visible in their own captured `.prom` output — because a textfile
    scrape tolerated what `client_golang` will not, so the legacy behaviour is
    not a precedent to port. This is the same rule `card_type` states below, and
    the third endpoint in a row where the key had to be verified against the
    capture rather than inherited.
    **`internalAddress` is nevertheless the wrong label**, despite being the
    manual's nominated tie-breaker and genuinely unique: R1.11.2 documents it as
    changing whenever the cartridge is assigned, unassigned or moved, so it would
    churn a fresh series out of every move while identifying nothing an operator
    can act on. It stays off the wire entirely.
  - **`encryption_method` (added 2026-08-01 by the `logical_libraries` collector)
    is a configuration mode carried as an `_info` label rather than as a
    stateset**, and that is the decision worth recording rather than the key
    itself. R1.11.2 enumerates six values (`none`, `systemManaged`,
    `applicationManaged`, `libraryManagedBarcode`,
    `libraryManagedInternalLabelSelective`, `libraryManagedInternalLabelAll`), so
    a stateset was available and was rejected: nothing alerts on a *particular*
    mode, and what an operator needs to see is that a partition's mode
    **changed**, which a label surfaces as a new `_info` series for free. A
    stateset would have cost 6 series per partition to answer a question nobody
    asks. The rule this sets for the collectors still unbuilt: an enumeration
    becomes a stateset when a rule selects on its *values*, and an `_info` label
    when only its *transitions* matter.
  - **`drive_location` (added 2026-08-01 by the `fc_ports` collector) is the first
    label in this vocabulary that names ANOTHER resource's location.** It is not a
    second spelling of `location`, and the two must never be confused: on
    `/v1/fcPorts` the port has its own `location` (`fcPort_F1C4R1P0`) and
    `drive_location` is the drive it is installed on (`drive_F1C4R1`), which is a
    key into the `drives` collector. It rides on the measurement series rather
    than on `_info` alone, for the reason `logical_library` already gives below,
    and `FCPortNoLight` is the rule that reads it. The rule this establishes for
    the collectors still unbuilt: a cross-reference to another endpoint's resource
    is spelled `<subsystem>_location`, never bare `location`, so that a join can
    name both sides in one expression.
  - `operation` carries the drive's own `operation` field (`none`, `empty`, `loading`,
    `ready`, `unloading`, `unloaded`); `access` carries the ternary `accessible` field
    (`normal`, `limited`, `no`); `direction` takes `read`/`write` on the cartridge
    error counters. **The `access` label's value set is per-resource, not global**
    (established 2026-07-31 by the `accessors` collector): on drives and cartridges it
    is the three-value `accessible` ternary above, and on accessors it carries
    `driveAccess`/`cartridgeAccess`, which R1.11.2 documents in prose as `normal` or
    `limited` only, with no third value. Same label key, same meaning ("what can this
    thing still reach"), two documented sets — so a stateset's known list is read from
    the endpoint being collected, never inherited from another one. Every collector's
    emit-observed-anyway branch is what catches the manual being wrong about either.
    **`type` is deliberately not a label name** — the Prometheus
    exporter guidance names it as the canonical too-generic label — so the cartridge
    generation (`JD`, `JA`, `JK`…) is `cartridge_type` and the node card class
    (`LCC`, `MDA`, `ACC`) is `card_type`.
  - **`card_type` (added 2026-08-01 by the `node_cards` collector) is the first label
    in this vocabulary that exists because `location` is not a unique key.** Every
    other collector keys on `location` alone; `/v1/nodeCards` puts an MDA *and* an ACC
    card at the same `accessor_Aa`, so the pair `location` + `card_type` is the key
    there, and the parser rejects a duplicate of the pair rather than of `location`.
    The rule this establishes for the collectors still unbuilt: a key is whatever
    makes an object unique on ITS OWN endpoint, verified against the capture, not
    inherited from the collector written before it. `fc_ports` (80 ports across
    frames) is the next one worth checking against this before its labels are fixed.
  - **The rule cuts both ways, established 2026-08-01 by the `io_stations`
    collector: a planned label can be too WIDE as well as too narrow.** `door`
    was on that collector's budget line by analogy with `frames`, which carries
    it because a frame has up to three doors. R1.11.2 gives an I/O station
    exactly one, so the label was dropped rather than shipped at a constant
    value: `location` already identifies the door, and a label that can never
    discriminate anything is a column every query has to carry and no query can
    use. `door` therefore stays in this vocabulary as a `frames`-only key,
    taking `front`/`rear`/`side`, and no collector after this one should add it
    without a resource that genuinely has more than one door.
  - `location` always carries the library's own native location string verbatim
    (`drive_F1C4R1`, `slot_F7C3R15T1`, `frame_F1`, `accessor_Aa`,
    `powerSupply_F1PSa`, `ioStation_F2IOu`) — never a re-derived frame/column/row
    triple, so a value in a dashboard can be pasted straight into the GUI.
  - `gripper` takes `1`/`2`; `axis` takes `x`/`y`; `door` takes `front`/`rear`/`side`.
  - **`_info` label keys** (added 2026-07-31 by the `library` collector, the first to
    emit an `_info` metric, and binding on every collector after it): `name`, `serial`,
    `firmware`. The spelling is `serial`, not the API's own `sn` — the field list in
    the bullet below names the API *fields* that must stay off a measurement series,
    not the label spellings, and a label key is read by operators.
    Extended 2026-07-31 by the `frames` collector with `mtm`, `frame_type` and
    `media_type`, which is the same question resolved for a resource that has
    neither a name nor its own firmware. `mtm` keeps the API's own spelling:
    it is IBM's own machine type-model term and an operator reads the frame's
    label plate in exactly those characters. `type` becomes `frame_type` for
    the reason `cartridge_type` already exists — the Prometheus exporter
    guidance names bare `type` as the canonical too-generic label — so every
    future `_info` carrying a resource kind spells it `<subsystem>_type`,
    never `type`.
    Extended 2026-07-31 by the `drives` collector with `use`, `encryption`,
    `interface` and `wwnn`. Each is a per-drive constant and therefore free
    on `_info`, and `use` (`access`/`controlPath`) is the one that earns its
    place on merit: a control-path drive is how the host talks to the library
    at all, nothing else on that endpoint distinguishes the two roles, and
    `ControlPathRedundancyLost` is the rule that reads it. `wwnn` was already
    named in the identity-strings list below; this is the first collector to
    have one.
    Extended 2026-08-01 by the `fc_ports` collector with `drive_serial`,
    `wwpn`, `speed_setting`, `topology_setting`, `topology_actual` and
    `loop_id`. Three things this settles for the collectors still to come.
    **`drive_serial`, not `serial`**: a Fibre Channel port has no serial of
    its own and the field is the parent drive's, so the `<resource>_` prefix
    says whose identity it is — the same discipline that produced
    `drive_location`, applied to an identity string rather than a grouping
    key. **`speed_setting` is on `_info` because a rule has to join against
    it**: `FCPortSpeedBelowPeers` must exclude ports deliberately pinned to a
    non-auto rate, and a second gauge would be absent for exactly the `auto`
    ports the rule cares about — the `ControlPathRedundancyLost` shape again.
    **`topology_actual` is the first `_info` label in this exporter that is
    not constant**, flipping between its negotiated value and `unknown` as the
    link comes and goes. It is kept because a port negotiating `L-Port` where
    its peers reached `N-Port` is a real fabric misconfiguration nothing else
    on that endpoint reports, and the churn is bounded at two label sets per
    port over its life. That bound is the test to apply to any future
    non-constant `_info` label, rather than this being a blanket permission:
    `volser` fails it, which is why it is behind a flag everywhere.
  - **`logical_library` rides on measurement series, not only on `_info`**
    (decided 2026-07-31 with the maintainer, on the `drives` collector). It is
    a grouping key rather than an identity string, it costs nothing (constant
    per drive), and putting it on the statesets is what lets
    `LogicalLibraryDrivesBelowFloor` be a plain
    `count by (job, library, logical_library)` instead of a `group_left` join
    against `_info`. The accepted cost is that reassigning a drive between
    logical libraries breaks the continuity of its measurement series — a real
    configuration change, and one worth seeing as a discontinuity. `media_type`
    stays on `_info` alone, as on `frames`: it is identity, not a grouping key.
    Binding on `data_cartridges` and `logical_libraries`, which carry the same
    label.
- **Timestamps the library reports** (established 2026-07-31 by the `drives`
  collector, the first to emit one): the wire format is
  `2026-07-23T20:43:36+0000`, whose zone offset carries no colon, so it is
  **not** `time.RFC3339` and parsing it as such fails. The layout is
  `2006-01-02T15:04:05-0700`, named once per collector as a `const`. A null,
  empty or unparseable timestamp emits **no series at all**, never a `0`: zero
  here is not a missing reading, it is an assertion that the event happened at
  the Unix epoch, which silently corrupts every "within the last N days" query
  rather than merely being absent from it. Unparseable additionally logs.
  Binding on `data_cartridges`, `events` and the three `reports_*` collectors.
- **Null is not always the absence of a reading.** `drives.operation` is the
  counter-example that fixes the rule's scope: R1.11.2 documents `null` there
  as meaning "no operation is in progress", a documented *value*, so it becomes
  the `none` member of the stateset rather than suppressing the series. The
  test is whether the manual assigns the null a meaning. `accessors.temperature`
  null means "this hardware cannot report", so it emits nothing;
  `drives.operation` null means "idle", so it emits `none`. An empty string
  takes the same branch as null in both cases.
  - **The rule needed a THIRD branch, added 2026-08-01 by the `data_cartridges`
    collector: a null the manual calls unknown, on a field that is a LABEL
    rather than a measurement.** The two branches above both decide whether to
    emit a *series*. They say nothing about what to write in a label when the
    library reports null for the thing the label names, and on this endpoint
    that is not a corner case: 27 of the capture's 60 cartridges (45%) return
    null for every cartridge-memory field at once — `type`, `typeDescription`,
    `vendor`, `manufactureDate`, `sn`, `worm`, `format`, `density`,
    `densityCode`, `nativeCapacity` and `lifetimeRemaining` — while still
    reporting volser, state, location, mediaType and mostRecentUsage.
    Suppressing those cartridges from the typed counts would leave every
    breakdown failing to sum to the library's real cartridge count, with nothing
    on the wire explaining the gap; emitting them under `""` would be a label no
    operator can read, which `cleaning_cartridges` already refuses for `state`.
    So a nullable **label** field takes the literal token **`unknown`**, and the
    counts stay complete. Binding on `diagnostic_cartridges` and
    `data_cartridges_lifetime`, which read the same cartridge-memory fields.
    Check the collision before reusing it: it is safe here only because
    cartridge types are 2-character barcode codes, `worm` is `true`/`false` and
    `encrypted` is `yes`/`no`, so none can legitimately BE the string "unknown".
    A field whose documented value set already contains `unknown` — every
    `state` enumeration in this exporter does — must not use this branch, since
    the token would merge a real state with a missing reading.
  - **`logical_library="unassigned"` (established 2026-08-01 by the
    `data_cartridges` collector).** R1.11.2 documents a null `logicalLibrary` as
    "not assigned to a logical library", which is a documented value and
    therefore the `drives.operation` branch, not the branch above: it becomes a
    named member of the enumeration rather than the `unknown` token. An
    unassigned cartridge is precisely what the `assignmentRequired` state reports
    on, so its count has to be readable. Note that `drives` carries the same
    label and would decode its own null to `""`, by construction (a plain string
    field, no pointer) — all 40 drives in the capture are assigned, so that path
    has never run, and this collector is where the convention is actually fixed.
    Binding on `slots` and `diagnostic_cartridges`. The theoretical collision, a
    site naming a real partition `unassigned`, is accepted and would be visible
    as a partition of that name in `/v1/logicalLibraries`.
- **Per-collector timeout and interval defaults may depart from the shared
  5s/5m, and `data_cartridges` is the first to (2026-08-01).** It ships `60s`
  and `15m`. Both follow from the endpoint rather than from taste: `GET
  /v1/dataCartridges` returns 9 749 entries unpaginated over the slow
  SCSI/LCC path this journal already names as the reason the build is
  multi-instance at all, so a 5s default would ship a collector that fails
  every refresh and serves a permanently empty cache — worse than a deviation.
  The interval moves with the timeout because of the concurrency ceiling of 1:
  a refresh that long holds the library's only request slot against its
  seventeen siblings while it runs, and a cartridge inventory turns over in
  hours. The queueing stays observable through
  `tapelibrary_exporter_request_wait_seconds`. Binding on the three heavy
  endpoints still unbuilt — `slots`, `data_cartridges_lifetime` and to a lesser
  degree `events` — which must size their own defaults against the capture
  rather than inheriting 5s/5m.
  - **Identity strings never label a measurement series.** `sn`, `firmware`, `mtm`,
    `wwnn`, `wwpn`, `type`, `vendor` and friends live on a dedicated
    `tapelibrary_<subsystem>_info{...} = 1` metric, joined by `location`. A firmware
    upgrade then changes one info series instead of breaking the continuity of every
    measurement series on that device.
- **Enum encoding: stateset.** Each enumerated state becomes one series per known
  value, `tapelibrary_drive_state{location="drive_F1C4R1",state="online"} 1` with
  `0` on every other known state. The severity classification stays in the alerting
  rules, where an operator can change it without rebuilding the exporter, and a
  state the exporter has never seen before stays visible instead of silently
  evaluating to nothing. This is a deliberate departure from the legacy scripts,
  which encoded severity in the metric *value* (`0`/`1`/`2`); the classification
  itself is preserved verbatim in the alert candidates below.
- **State vocabulary, extracted from the manual's own tables** (R1.11.2, §"URL
  endpoints and resources"). This is the authoritative list; the legacy scripts'
  `declare -A` maps are **not**, and disagree with it in four places (see
  `## Open questions / assumptions`). Every collector emits the full documented set
  as a stateset, **plus any value it actually observes that is not on the list** —
  the manual is demonstrably incomplete, and an untabulated state must surface as a
  new series rather than silently leave every series at `0`.
  - `library.status` (17): `unknown`, `doorOpenWhileNotAllowed`, `notConfigured`,
    `doorOpen`, `initializing`, `inServiceMode`, `accessorsUnavailable`,
    `calibrationRequired`, `accessorDegraded`, `nodeCardDegraded`, `driveDegraded`,
    `cartridgeDegraded`, `updating`, `pausing`, `paused`, `scanningInventory`,
    `online`.
  - `accessors.state` (10): `inServiceMode`, `noMovementAllowed`, `bothGrippersFailed`,
    `gripper1Failed`, `gripper2Failed`, `scannerFailed`, `noMotorPower`, `calibrating`,
    `onlineStandby`, `onlineActive` — plus `failedToInitialize`, which the manual
    references under `library.accessorsUnavailable` but never tabulates.
  - `frames.state` (6): `unknown`, `frontDoorOpenWhileNotAllowed`, `acUnreachable`,
    `calibrationRequired`, `inventoryPending`, `normal`. Door position is **not** a
    frame state in this firmware: it is the separate `frontDoor`/`rearDoor`/`sideDoor`
    fields (`open`/`closed`/`null`), which is what `tapelibrary_frame_door_open{door=}`
    reads.
  - `drives.state` (9): `unknown`, `inServiceMode`, `restarting`, `initializing`,
    `unreachable`, `resetRequired`, `updating`, `cleaning`, `online`.
  - `drives.operation` (6): `none` (the manual's `null`), `empty`, `loading`, `ready`,
    `unloading`, `unloaded`.
  - `nodeCards.state` (7): `unknown`, `restarting`, `inServiceMode`, `unreachable`,
    `noEthernet`, `noCAN`, `online`.
  - `powerSupplies.state` (3): `unknown`, `failed`, `online`.
  - `ioStations.state` (5): `normal`, `closedNoMagazine`, `failedToClose`,
    `doorOpenTooLong`, `unknown`.
  - `fcPorts.state` (4): `unknown`, `noLightDetected`, `communicationNotEstablished`,
    `communicationEstablished`.
  - `slots.state` (2): `inServiceMode`, `normal`.
  - `dataCartridges.state` (9): `unknown`, `failedVerification`, `atEndOfLife`,
    `assignmentRequired`, `uncertainBarcode`, `exportQueued`, `importing`, `verifying`,
    `normal` — plus `cartridgeFailedMove` and `errorThresholdExceeded`, referenced
    under `library.cartridgeDegraded` but never tabulated.
  - `cleaningCartridges.state` (3): `exportQueued`, `importing`, `normal`. No
    `unknown` in this table, unlike every other cartridge type.
  - `diagnosticCartridges.state` (5): `unknown`, `atEndOfLife`, `exportQueued`,
    `importing`, `normal`.
  - `accessible` (3, shared by drives and all cartridge types, label `access`):
    `normal`, `limited`, `no`.
  - `events.severity` (5): `error`, `warning`, `inactiveError`, `inactiveWarning`,
    `information`. **`inactiveError`/`inactiveWarning` mean resolved**, and alerting
    must not treat them as active.
  - `events.state` is **not an enumeration and must never become a label**: the manual
    documents it as interpolated free text (`Assigned PMR <PMR number>. Service action
    required`, `Command failed with error code <error code>`), so a label would carry
    a PMR number straight into the cardinality.
- **Business-alert candidates** (one line per collector; the legacy severity
  classification from `samples/legacy/` is carried across into the `state=~` matchers):
  - `library` — critical if `state=~"doorOpenWhileNotAllowed|restarting|accessorsUnavailable"`
    for 30m; warning if `state=~"doorOpen|pausing|paused|inServiceMode|accessorDegraded|nodeCardDegraded|driveDegraded|updating|scanningInventory"`
    for 30m; warning if used capacity / `licensedCapacity` exceeds `capacityUtilThresh`.
  - `frames` — **shipped 2026-07-31**, and the one candidate on this list that
    changed shape on contact with the manual. Critical if
    `state=~"frontDoorOpenWhileNotAllowed|acUnreachable"` for 30m; warning if
    `state=~"calibrationRequired|inventoryPending"` for 30m. The three door
    pseudo-states this line used to carry (`frontDoorOpen`, `rearDoorOpen`,
    `sideDoorOpen`) were dropped rather than ported: R1.11.2 does not define
    them as frame states, so those legacy selectors could never match. The
    door signal ships instead as `FrameDoorOpen`, on
    `tapelibrary_frame_door_open == 1` for 30m — a re-derived default, not a
    measured threshold, aligned with the existing `LibraryDegraded` `doorOpen`
    rule so the pair fires together and the frame-level rule answers *which*
    frame. `unknown` is in neither rule: unclassified, exactly like the five
    unclassified `library` statuses.
  - `accessors` — **shipped 2026-07-31**, as five rules rather than the three planned
    here. Critical if
    `state=~"noMovementAllowed|bothGrippersFailed|failedToInitialize"` for 30m;
    warning if `state=~"gripper1Failed|gripper2Failed|scannerFailed|noMotorPower|calibrating|onlineStandby|inServiceMode"`
    for 30m. Two changes to the ported selectors: `reorienting` became `calibrating`
    (the second of the four legacy-vs-manual contradictions, resolved below), and
    `failedToInitialize` was **added** to the critical rule — the one selector on this
    collector that comes from neither the legacy scripts nor a state table, but from
    the manual's own prose grouping it with `noMovementAllowed` and
    `bothGrippersFailed` under `library.accessorsUnavailable`.
    The dual-accessor candidate ships as `AccessorNotFullyAvailable`, written as
    `sum by (job,library) (…{state="onlineActive"}) < count by (job,library) (…)`
    rather than against a hardcoded 2, so it is correct on a single-accessor library
    and doubles as the safety net for any state in neither severity rule.
    Two rules beyond the plan, following from shipping the access statesets:
    `AccessorDriveAccessLimited` and `AccessorCartridgeAccessLimited`, both warning at
    30m. They are separate rules rather than one `or` because the two metrics carry an
    identical label set, so `or` would silently drop the second whenever both fire.
    Their 30m is a re-derived **default**, not a measured threshold: the legacy scripts
    emitted both fields at a constant `0` and never alerted on either, so nobody has
    data on how long a blocking position legitimately persists here.
  - `drives` — **shipped 2026-07-31**, as six rules rather than the three planned
    here. Critical if `state="unreachable"` for 1h; warning if
    `state=~"restarting|initializing|resetRequired|updating|inServiceMode"` for 1h.
    `unknown` and `cleaning` are in neither, unclassified exactly like the five
    unclassified `library` statuses — `cleaning` is the library doing what it
    should, and the legacy scripts never mapped `unknown`.
    The per-`logical_library` floor ships as `LogicalLibraryDrivesBelowFloor`,
    written as a **ratio** (`sum(...online) / count(...) < 0.8`) rather than
    against a hardcoded drive count. The accessors collector's
    `sum < count` shape was rejected here for a reason worth keeping: at 40
    drives, one in service mode is normal, and the 2026-07-28 capture already
    shows exactly that, so an all-online rule would fire permanently on this
    fleet. 0.8 is a round-number default to adjust per deployment.
    Three rules beyond the plan, each following from something that shipped:
    `DriveAccessLimited` at warning on `access="limited"` and at critical on
    `access="no"` (separate rules, not `or`, because the two selectors carry an
    identical label set — the same argument the two accessor access rules
    already make), both 30m and both re-derived defaults, since the legacy
    scripts never collected this field. And `ControlPathRedundancyLost` at
    `< 2` online control-path drives for 30m, the one rule here needing a
    `group_left` join because `use` is an identity string living on `_info`:
    a logical library that loses its last control-path drive disappears from
    the host regardless of how many data drives remain, and nothing else on
    this endpoint can say so.
    **No rule ships on `last_cleaned`**, deliberately. The metric has a real
    direction but no grounded threshold: the legacy scripts never collected the
    field, and the cause an overdue drive would signal — cleaning cartridges
    running out — is covered with a real ported threshold by the planned
    `cleaning_cartridges` rules (< 100 warning, < 30 critical). A made-up
    "90 days" would page on an arbitrary number beside a measured one.
  - `power_supplies` — **shipped 2026-08-01**, as two rules rather than the one
    planned here, and the split is the only judgement call on this collector. The
    planned expression ships verbatim as `PowerSupplyNotOnline`
    (`{state="online"} == 0`, 15m) but at **warning**, not critical, because it also
    fires on `unknown`, which R1.11.2 attributes to an unreachable LCC node card
    rather than to the supply — the same reason `unknown` is unclassified on
    `library`, `frames` and `drives`. The critical tier is the narrower
    `PowerSupplyFailed` (`{state="failed"} == 1`, 15m), the state the manual defines
    as an actual failure. The two overlap by design: the narrow rule names the
    actionable fault and the broad one is the safety net for every state not in it,
    including any R1.11.2 does not tabulate — the shape `AccessorNotFullyAvailable`
    already established. 15m is a re-derived **default**, not a measured threshold:
    the legacy scripts never collected this endpoint at all (`samples/legacy/`
    mentions power only in an unrelated `noMotorPower` accessor key), so nobody here
    has data on how long a supply legitimately sits outside `online`. It is shorter
    than the 30m/1h used elsewhere because no routine operation takes a power supply
    offline and then returns it.
  - `node_cards` — warning if `state=~"inServiceMode|unreachable|noEthernet|noCAN"` for 30m.
  - `io_stations` — warning if the door has been open for more than 1h, or if the
    station has been full for more than 1h (exports are not being collected).
  - `fc_ports` — **shipped 2026-08-01**, as two rules, both warning. The first
    ships verbatim as `FCPortNoLight`: `state="noLightDetected"` joined against
    `tapelibrary_drive_state{state="online"}` for 15m, the join being the entire
    point — a dark port on a drive in service mode is someone unplugging a cable
    on purpose, and only the drive's own state tells the two apart. It needs a
    `label_replace` to rewrite `drive_location` into `location` so both sides
    share a join key; that is applied to the FC side so the result keeps the
    port's labels for the annotations.
    The second changed shape on contact with the capture. "Below the port's
    configured setting" is **undefined on this fleet** — `speedSetting` is `auto`
    on 79 of the capture's 80 ports, so there is no configured rate to fall below
    — and it ships as `FCPortSpeedBelowPeers`, comparing each port against the
    fastest rate any *auto-negotiating* port on the same library reached. Self
    calibrating rather than thresholded on a literal, which is the house style
    `LogicalLibraryDrivesBelowFloor`'s ratio and `AccessorNotFullyAvailable`'s
    `sum < count` already set: a hardcoded `< 2e9` would be invented, would fire
    forever on the one port deliberately pinned to 8Gbps, and would be wrong the
    day a 32GFC drive lands. The `group_left` against `_info` is what excludes
    that pinned port. Dark ports drop out for free, since they emit no speed
    series at all, which is what keeps this a renegotiation signal rather than a
    second link-down one.
    **No critical tier ships on this collector**, deliberately. A single dark
    port is the two-port redundancy doing its job; what would deserve one is
    both ports of a drive going dark together, and that rule is not written —
    see `## Open questions / assumptions`.
  - `logical_libraries` — **shipped 2026-08-01**, as the planned single candidate
    split into a warning/critical pair on one expression. Ships as
    `LogicalLibraryNearlyFull`:
    `tapelibrary_logical_library_cartridges / tapelibrary_logical_library_virtual_slots`
    over 0.98 (warning) and 0.99 (critical), both for 1h. A bare division rather
    than an explicit `on (...)`, because both series come from this collector on
    the same instance and carry an identical label set — so the result keeps
    `instance`/`library`/`model`/`logical_library` for the annotations, which an
    `on()` clause would strip. `TestLogicalLibrariesCollector_SaturationIsComputable`
    pins that precondition, since one extra label on either side would make the
    division match nothing and the rule would silently never fire.
    **The thresholds are re-derived defaults and are deliberately higher than
    this file's usual round numbers.** The 2026-07-28 capture already sits at
    0.964 (4819/5000) and 0.971 (4853/5000), so the 0.90/0.95 pair the shipped
    example suggests would have paged on both partitions of every library from
    the moment it loaded — the same trap `LogicalLibraryDrivesBelowFloor`'s ratio
    and `FCPortSpeedBelowPeers`' peer comparison were each written to avoid. A
    tape library runs full; 97% is its working state, not an incident. 0.98
    leaves ~100 free slots per partition on this fleet and 0.99 leaves ~50.
    **Confirm both against how fast these partitions actually fill**, which
    nobody here has measured — this is the third re-derived default in this file
    and the first where the observed value is already inside the range the
    obvious threshold would have covered.
  - `cleaning_cartridges` — **shipped 2026-08-01**, as the three rules planned, and
    the only rules in this exporter carrying MEASURED rather than re-derived
    thresholds. `CleaningCartridgesLow` fires at warning below 100 and at critical
    below 30, both verbatim from the legacy rules, both `for: 1h`. The 1h is
    re-derived (the legacy `for` was `1m`, which on a signal with days of runway
    only buys flapping on a scrape gap); the thresholds themselves are the
    site's own, measured over years.
    **No `sum()`, unlike the legacy expression.** The collector emits the
    library-wide total itself, unconditionally, so the rule survives
    `--collector.cleaning_cartridges.per-volser=false`; a `sum()` over the
    per-cartridge series would have matched nothing the moment a site flipped
    that flag to cut cardinality, which is the whole purpose of the flag.
    `CleaningCartridgesExhausted` ships at **critical, where this line planned
    warning**. The plan predates the `usable` metric: zero usable cartridges is
    strictly worse than the `< 30` total above, since it means the library cannot
    clean a drive at all right now, and paging the strictly-worse condition at
    the lower severity would invert the pair. It reads
    `tapelibrary_cleaning_cartridges_usable`, deliberately narrower than the
    `state="normal"` count — a spent cartridge, one queued for export and one
    mid-import are all still reported and still counted in their state, and the
    robot will select none of them.
  - `data_cartridges` — warning on any cartridge count in a non-`normal` state
    sustained for 1h; warning if the count with `lifetimeRemaining` below 20% crosses
    a threshold.
  - `slots` — **shipped 2026-08-01**, as four rules, and the free-slot candidate
    changed shape on contact with the other collectors rather than with the manual.
    "Free slots below a configured floor" as planned would have duplicated
    `LibraryCapacityThresholdExceeded`, which already reads the library's own
    used/licensed ratio against the threshold the library itself publishes — and on
    the 2026-07-28 capture that is 99.03% against a `capacityUtilThresh` of 99, so
    the binding rule is already there and already firing. Taken with the maintainer,
    who chose the service-mode-aware form over both the planned absolute floor and
    shipping nothing.
    It ships as `SlotsNearlyFull` at warning below 0.02 and critical below 0.01, on
    `positions_available / positions`, both `for: 1h`. What makes it distinct rather
    than duplicative is the numerator: R1.11.2 defines `inServiceMode` as a state in
    which a slot "cannot be selected as a cartridge destination", so free positions
    inside one are capacity the robot may not use, and no other collector in this
    exporter can see slot state. A ratio rather than a literal, the house style
    `LogicalLibraryDrivesBelowFloor` and `FCPortSpeedBelowPeers` already set: a tape
    library runs full, and an absolute floor would be a number nobody here has
    measured. The thresholds are round-number **re-derived defaults** — roughly 215
    and 107 free positions on a 10 732-position library — and should be confirmed
    against how fast this fleet's slots actually fill.
    The retry candidate splits in two, and the split is forced by what the endpoint
    reports rather than chosen. `SlotPutRetryRateHigh` is a genuine **ratio**,
    `rate(put_retries)/rate(puts) > 0.01` for 1h, and its threshold is the
    best-grounded one in this file after the cleaning-cartridge pair: `putRetries` is
    0 on all 79 slots of the capture against 25 023 lifetime puts, so the healthy
    value is not "low", it is zero, and 1% is the margin above that rather than an
    estimate of normal wear. When no puts are happening the division is NaN and the
    rule cannot fire, which is correct for an idle library.
    `SlotGetRetryRateHigh` cannot be a ratio: **`/v1/slots` reports `getRetries` but
    no matching `gets` counter**, so there is no denominator on that side. It ships
    as an absolute `rate(...) > 0.01` (about 36 an hour library-wide) at warning
    only, explicitly a re-derived default — see `## Open questions / assumptions`.
    **No rule ships on `tapelibrary_slots{state="inServiceMode"}`**, deliberately,
    and this is the one place the capture is misleading rather than helpful. 9 of its
    79 slots are in service mode (11%), which would look alarming as a threshold —
    but the trim is stratified, deliberately carrying both states, so that ratio says
    nothing about the fleet. `SlotsNearlyFull` is the rule that would notice service
    mode mattering, because slots taken out of service leave the available count
    while staying in the total. Revisit once a real per-library figure exists.
  - `events` — **shipped 2026-08-01**, both rules, as planned and with one addition
    the plan left implicit. `LibraryEventWarning` on
    `tapelibrary_events{severity="warning"} > 0` at warning, `LibraryEventError` on
    `severity="error"` at critical, both `for: 5m`. The matcher is an exact equality
    rather than a regex, and that is load-bearing rather than stylistic: two of the
    five documented severities (`inactiveError`, `inactiveWarning`) mean the
    condition has been RESOLVED, so any pattern sweeping them in would page on
    errors already fixed. `for` is 5m against the 30m every hardware rule uses,
    because there is no transient to wait out — the library has already decided the
    event is worth recording, and the value stays put for a whole lookback window
    regardless, so a longer `for` would only delay the operator. Nothing ships on
    `severity="information"`: the capture is 42 information events in 3h45m, every
    one a login or logout, and this exporter authenticates per request, so that
    series counts the monitoring traffic as much as anything an operator did.
  - `data_cartridges_lifetime` — warning if the count of cartridges with uncorrected
    read or write errors increases (media loss is imminent and silent otherwise).
  - `reports_library` — warning if hourly `mounts` drops to zero during a window
    where it never realistically should; warning if `humidityAverage` > 50%
    (verbatim from the legacy rule) or temperature leaves the operating envelope.
    **Shipped 2026-08-02, five rules, with the operating envelope pinned to a
    number the manual actually states.** R1.11.2's "Operating environmental
    specifications" gives an **allowable** envelope of 16–32 °C / 20–80% RH and
    a **recommended** one of 16–25 °C / 20–50% RH. The rules read the
    *allowable* bound, because the manual writes both for the customer-supplied
    **ambient** sensors outside the enclosure while `/v1/reports/*` reports what
    the **drives** measure inside it, which runs hotter: the capture's healthy
    library sits at ~25 °C average and 28.5 °C maximum, already past the
    recommended ambient ceiling, so a rule on it would fire continuously on a
    library that is fine. The two temperature tiers therefore differ by
    **scope, not by threshold** — warning when the hottest drive passes 32 °C,
    critical when the whole population averages past it — which keeps both
    anchored to the single documented number instead of inventing a second one.
    Humidity's warning tier is the one deliberate use of the *recommended*
    bound, and it earns it twice: 50% RH is simultaneously that ceiling and the
    exact threshold the legacy RoS rule ran on this fleet for years without
    being a nuisance, `for: 1h` included. Its critical tier is the allowable
    80%, where the manual's "non-condensing" qualifier stops holding.
    `LibraryReportNoMounts` is the one rule here with **no grounding in
    R1.11.2**, and it is flagged as such in the file: `== 0 for 6h` is a blunt
    default, because nobody has measured what this fleet does overnight, at
    weekends or during a maintenance freeze, and a legitimately idle library
    must not page. Nothing ships on imports, exports, moves or the three
    data-volume gauges — every one of them measures how much work the hosts
    asked for, so any threshold would page on a quiet week.
    `for:` is counted in whole windows throughout: a value cannot change
    between hourly publications, so a sub-hour `for:` buys nothing.
  - `reports_drives` — warning if a single drive accounts for a disproportionate
    share of the library's uncorrected errors over 24h (the classic bad-drive
    signature); warning on per-drive humidity/temperature excursions.
  - `reports_accessors` — warning if one accessor's share of gets/puts collapses
    while the other's rises (silent failover), which the cumulative counters on
    `/v1/accessors` cannot show as clearly.
  - `diagnostic_cartridges` — warning if no usable diagnostic cartridge remains
    (service actions will be blocked when they are next needed).

## Scaffold inputs

- `EXPORTER_NAME`: `tapelibrary_exporter`
- `NAMESPACE`: `tapelibrary`
- `DATA_SOURCE`: `https://<library-address>/web/api/v1`
- `DATA_SOURCE_PATH`: `/library`
- `DEFAULT_PORT`: `9170`
- `MODULE_PATH`: `github.com/sckyzo/tapelibrary_exporter`
- `OWNER`: `sckyzo`
- `LICENSE`: `gpl-3.0`
- Selectors actually passed: `--flavor http`, `--target-model multi-instance`,
  `--forge github`, `--instance-label library`, plus
  `--var COLLECTOR_HEALTH_BY=job,library --var COLLECTOR_LOCATION=library` so the
  shipped health rules break down per watched library rather than per exporter host.
- `DATA_SOURCE` was passed verbatim from the line above, angle brackets included, at
  the maintainer's explicit choice after the alternative was raised. The consequence
  is local and understood: `--help` and `docs/configuration.md` show a placeholder
  rather than a parseable URL as the flag's default. It is inert on this build —
  a `multi-instance` exporter takes every real address from `instances:` in
  `--config.file` — but a future `single`-target build would need a real value.

## Collectors

Ordered as `/add-collector` should work through them: hardware health first (it
replaces the live legacy alerting soonest), then inventory, then the heavy
aggregates, then the hourly report windows. **Every entry is the `background`
variant** — `multi-instance` admits no other.

- [x] `example`  background  built 2026-07-31, **REMOVED 2026-08-03** — the scaffold's
      own starter collector. Never a TS4500 resource: it existed to be adapted into the
      first real collector or deleted once one landed, and this entry always said so.
      Nineteen real collectors landed, and the first run against a real library made the
      case concrete: it was the only registered collector that never refreshed, because
      it polls `/library` under a second name and had nothing of its own to report. Kept
      on this list rather than deleted from it, so the count below reads against the
      right denominator and a later session does not rediscover it as a gap.
- [x] `library`  background  built 2026-07-31 — `GET /v1/library`
- [x] `frames`  background  built 2026-07-31 — `GET /v1/frames`
- [x] `accessors`  background  built 2026-07-31 — `GET /v1/accessors`
- [x] `drives`  background  built 2026-07-31 — `GET /v1/drives`
- [x] `power_supplies`  background  built 2026-08-01 — `GET /v1/powerSupplies`
- [x] `node_cards`  background  built 2026-08-01 — `GET /v1/nodeCards`
- [x] `io_stations`  background  built 2026-08-01 — `GET /v1/ioStations`
- [x] `fc_ports`  background  built 2026-08-01 — `GET /v1/fcPorts`
- [x] `logical_libraries`  background  built 2026-08-01 — `GET /v1/logicalLibraries`
- [x] `cleaning_cartridges`  background  built 2026-08-01 — `GET /v1/cleaningCartridges`
- [x] `data_cartridges`  background  built 2026-08-01 — `GET /v1/dataCartridges`
- [x] `slots`  background  built 2026-08-01 — `GET /v1/slots`
- [x] `events`  background  built 2026-08-01 — `GET /v1/events?after=…`. The only
      collector reading a log rather than hardware, and the only one whose request
      carries a query parameter: R1.11.2 states a bare `GET /v1/events` returns
      **every** event the library ever recorded, so the window is bounded by
      `--collector.events.lookback` (`1h`) rather than being whatever the endpoint
      felt like returning.
- [x] `data_cartridges_lifetime`  background  built 2026-08-01 —
      `GET /v1/dataCartridges/lifetimeMetrics`. **The one collector whose metric
      subsystem is not its registered name**: it emits
      `tapelibrary_data_cartridges_usage_*`, because `data_cartridges` already owns
      `..._data_cartridges_lifetime_*` for media life REMAINING and this reads work
      DONE. Also the only source of genuine monotonic device counters, hence the
      exporter's only `_total` metrics.
- [x] `reports_library`  background  built 2026-08-02 — `GET /v1/reports/library`.
      **The second collector whose metric subsystem is not its registered name**,
      for a reason unrelated to `data_cartridges_lifetime`'s: that one moved to
      avoid a prefix collision, this one to keep the resource at the front of the
      name. It emits `tapelibrary_library_report_*`, and the two siblings below
      follow as `..._drive_report_*` and `..._accessor_report_*`, matching how
      every other per-resource family here is already spelled (`..._drive_state`,
      `..._accessor_humidity_ratio`). **The exception is now the rule for three
      collectors out of seventeen, so the convention is better stated as: the
      subsystem names the RESOURCE, which coincides with the registered name
      wherever the collector is named after its resource.** Also the first
      interval chosen against the data's cadence rather than the endpoint's cost
      (`15m` against an hourly publication), and the only collector whose values
      describe a window that closed before the scrape.
- [x] `reports_drives`  background  built 2026-08-03 — `GET /v1/reports/drives`.
      **The third collector whose metric subsystem is not its registered name**, and
      the first to follow `reports_library`'s rule rather than establish an exception
      to it: `tapelibrary_drive_report_*`, exactly as that entry predicted. Two
      things set it apart from its sibling. It is the **most expensive endpoint per
      byte in the exporter** — one entry per drive per hour, so the same default week
      that costs `reports/library` ~67 KB costs this ~3.3 MB — which is why it is the
      only collector whose interval (`1h`) matches its endpoint's publication cadence
      instead of subdividing it, and why its timeout is `60s`. And it is the first
      `reports_*` collector to carry a label at all: `location`, which joins to
      `drives` and `fc_ports`. Its window timestamp is per drive rather than
      library-wide, because a single drive dropping out of the report is precisely
      what a library-wide one would hide.
- [x] `reports_accessors`  background  built 2026-08-03 — `GET /v1/reports/accessors`.
      **The fourth collector whose metric subsystem is not its registered name**
      (`tapelibrary_accessor_report_*`), following `reports_library`'s rule exactly as
      that entry predicted for both siblings. Three things distinguish it. It is the
      only collector in the exporter whose **every** metric is the per-window
      counterpart of a lifetime counter another collector already emits: `accessors`
      ships `pivots`, `bar_code_scans`, `travel_meters`, `gets` and `puts` as
      `_total`-suffixed monotonic device counters, and this ships the same five
      quantities for one closed hour, as Gauges. That pairing is the collector's whole
      justification — a lifetime counter at 2.7 million gets moves imperceptibly when
      an accessor stops, while its hourly window drops to zero at once. It is the
      first collector whose defaults are **split between its two siblings**: 15m from
      `reports_library` (the response is ~148 KB, so `reports_drives`' size argument
      for an hourly cadence does not reach), 60s from `reports_drives` but for a
      different reason — taken with the maintainer, because the RoE path is slow in
      ways payload size does not predict. And it is the first collector whose
      environmental metrics **emit nothing on the entire fleet**: these accessors carry
      no temperature or humidity sensor and report null in every window, exactly as
      `/v1/accessors` does. All six ship anyway, absent-never-zero, so hardware that
      does report them needs no code change; the test triad pins both halves rather
      than leaving the dead branch untested.
- [x] `diagnostic_cartridges`  background  built 2026-08-03 — `GET /v1/diagnosticCartridges`.
      **The last of the nineteen, and the only collector whose empty response is a
      valid reading rather than an error.** Every other cartridge endpoint rejects an
      empty array as a response that lost its content — a library with no data
      cartridge cannot serve a host, one with no cleaning cartridge cannot clean a
      drive. A library with no diagnostic cartridge is merely one nobody has loaded
      one into, and rejecting that would replace a true zero with a stale cache, in
      exactly the case `DiagnosticCartridgesExhausted` exists to see.
      Two other things set it apart. It is the **second collector to emit `volser` by
      default**, after `cleaning_cartridges` and on the same justification applied to
      a smaller population (five against seventy): bounded by service policy rather
      than by library capacity. And it is the only cartridge collector carrying a
      **full per-state stateset**, which the cardinality rule permits at five objects
      and forbids at 9 749 — `data_cartridges` carries state as an `_info` label for
      exactly that reason.
      `usable` is deliberately narrower than the `normal` state count: a cartridge the
      accessor cannot reach is one the library cannot select, and reading the state
      count alone would report a healthy supply in the one situation where nothing can
      be picked up.

Deliberately excluded, with reasons, so a later session does not rediscover them as
gaps: `ethernetPorts` carries IPv4/IPv6 addressing only and has no `state` field or
any other quantity worth a series; `sasPorts` returns `[]` on fibre-channel hardware.
`guiSettings`, `logs`, `tasks`, `workItems` and everything under `authentication` are
either configuration surface, write paths, or credential material, and none belongs in
`/metrics`.

## Cardinality budget

Per library, at default flag settings. Multiply by 5 for the fleet.

**The rule that makes this budget work, and that must be applied consistently:**
a *full* stateset (one series per known state value, exactly one at `1`) is emitted
only where the objects number in the tens — drives, frames, accessors, node cards,
power supplies, I/O stations, FC ports. Where they number in the thousands —
cartridges, slots — only the *active* state is emitted, carried as a label on that
object's `_info` series. A full stateset over 9 749 cartridges × 6 states would cost
58 494 series for the state alone; the active-state-only form costs one. Both forms
are stateset encoding; only the second is affordable at inventory scale.

- `library`: labels `library`, `model`, `state`; 17 state series + 6 capacity gauges
  + 1 `_info`; worst case ~24 series; **observed 25** — the 24 planned, plus the
  background variant's own `_last_refresh_timestamp_seconds` freshness gauge, which
  every background collector emits and which the plan did not count. Expect the same
  +1 on all 18 collectors, so the fleet total rises by 18 × 5 = 90 series over the
  figure below rather than being wrong in kind.
- `frames`: labels `library`, `model`, `location`, `state`, `door`; 12 frames ×
  (6 states + 3 doors + 4 counts + 1 `_info`); worst case ~168 series;
  **observed 40** against the 3-frame fixture, **157 projected** for the
  capture's real 12 frames (72 stateset + 24 door + 48 counts + 12 `_info` +
  1 freshness). The gap to the planned ~168 is the rear door, not a dropped
  metric: every frame in the 2026-07-28 capture reports `rearDoor: null`, and
  a door the frame does not physically have emits no series at all rather
  than a `0` asserting it is closed, so the door family costs 2 per frame
  here instead of 3. Hardware with a real rear door lands on the plan.
- `accessors`: labels `library`, `model`, `location`, `state`, `access`, `gripper`,
  `axis`; 2 accessors × (11 states + ~12 counters/gauges); worst case ~46 series;
  **observed 51**, and the gap is two decisions rather than a miscount. **+8**: the
  `access` label was on this line while the arithmetic counted no access series, the
  same inconsistency `library.cartridgeAccess` hit; resolved at build time in favour
  of shipping both `driveAccess` and `cartridgeAccess` as statesets (2 documented
  values each, `normal`/`limited`), because they are the only signal that says one
  accessor is physically blocking the other's path. **−4**: `temperature` and
  `humidity` were counted in the ~12 and emit *nothing* on this hardware — the manual
  states "For TS4500, null is returned as there is no sensor", which both accessors in
  the capture confirm. Their descriptors ship anyway, so hardware carrying the sensor
  reports it without a code change; on this fleet they cost 2 descriptors and 0
  series. **+1**: the background variant's freshness gauge, as on every collector.
  46 + 8 − 4 + 1 = 51, which is what the fixture actually gathers. Worst case if a
  future library reports both sensors and does not pivot: unchanged at 51.
- `drives`: labels `library`, `model`, `location`, `state`, `operation`, `access`,
  `logical_library`, `media_type`; 40 drives × (9 states + 6 operations + 3 access
  values + 1 last-cleaned timestamp + 1 `_info`); worst case **~800 series**.
  The enumerations are the manual's own (§drives): `state` ∈ {unknown, inServiceMode,
  restarting, initializing, unreachable, resetRequired, updating, cleaning, online},
  `operation` ∈ {none, empty, loading, ready, unloading, unloaded} (the manual's
  `null` rendered as `none`), `access` ∈ {normal, limited, no}.
  **`drives.volser` — the cartridge currently loaded — is excluded from the default**
  and belongs behind the per-volser flag: only 40 series are ever active at once, but
  the volser churns, so `location × volser` accumulates index entries across every
  pairing that has ever existed (up to 9 749 × 40) even though the active set stays
  at 40.
  **Observed 81** against the 4-drive fixture, **801 projected** for the capture's
  real 40 drives: 20 series per drive exactly as planned (9 + 6 + 3 + 1 + 1), plus
  the background variant's freshness gauge. This is the first collector where the
  planned arithmetic and the built shape agree with no correction — `library`,
  `frames` and `accessors` each moved. The volser detail shipped as planned, behind
  `--collector.drives.per-volser` (default `false`), which makes it the **fourth**
  per-item flag rather than the three counted under "Fleet totals" below; enabling
  it adds one series per *loaded* drive, so ~15-40 per library at any instant and an
  unbounded index over time, which is the whole reason it is off.
- `power_supplies`: labels `library`, `model`, `location`, `state`; 8 × 3 states;
  worst case ~24 series; **observed 25**, and this is the first collector whose
  fixture is the library's *full* complement rather than a trim, so 25 is also the
  real per-library figure with no projection step. The single series over the plan
  is the background variant's freshness gauge, the same +1 every collector carries.
  8 is not a sample: only L25/L55 and D25/D55 frames hold supplies, so the
  twelve-frame capture reports eight, in four redundant a/b pairs. This is the
  narrowest collector in the exporter and will stay so — R1.11.2 gives
  `/v1/powerSupplies` exactly two attributes, `location` and `state`, so there is no
  `_info` series to add later and no measurement to carry beside the stateset.
- `node_cards`: labels `library`, `model`, `location`, `card_type`, `state`; 8 ×
  (7 states + 1 `_info`); worst case ~64 series; **observed 77**, and the gap is
  three decisions rather than a miscount. **+1 label**: `card_type` was not on the
  planned line and is not optional — the 2026-07-28 capture puts an MDA *and* an ACC
  card at `accessor_Aa`, so `location` alone is not a unique key here and a stateset
  keyed on it would send two metrics sharing a descriptor and a label set, failing
  `Gather` for the WHOLE scrape rather than just this collector. It costs no extra
  series (it discriminates cards that were always counted separately), only a
  vocabulary entry. **+12**: `lastRestart` and the `primaryLCC`/`reportingLCC` pair
  were on the wire and not in the plan, and ship as 3 metrics × the 4 LCC cards —
  the restart instant is the only signal that a controller bounced, and the role pair
  is the only one that says which LCC is in charge during a failover, which is what
  `NodeCardNoReportingLCC` reads. All three emit **nothing** on a card that does not
  report them, rather than a `0`: an MDA card never stood in the primary election, so
  a `0` would put it in the denominator of every "exactly one primary LCC" query.
  **+1**: the background variant's freshness gauge, as on every collector.
  64 + 12 + 1 = 77, which is what the fixture actually gathers. This is also the full
  per-library figure with no projection step — like `power_supplies`, the capture is
  the library's complete complement (8 cards) rather than a trim. Worst case if a
  future library reports the role fields on every card type: 8 × 2 = 16 role series
  instead of 8, so 85.
- `io_stations`: labels `library`, `model`, `location`, `state`, `door`;
  2 × (5 states + ~4); worst case ~18 series; **observed 23**, and the gap is
  three decisions rather than a miscount. **−1 label**: `door` is **dropped**,
  not deferred. It was planned by analogy with `frames`, which needs it because
  a frame carries up to three doors; R1.11.2 gives an I/O station exactly one,
  so the label could only ever hold a single constant value while `location`
  already identifies it. This is the mirror image of `node_cards` — there the
  planned label set was too narrow to key the resource, here it was too wide —
  and it makes the same point: a label set is read off the endpoint being
  collected, never inherited from the collector written before it.
  **+2 on the ~4**: five per-station metrics ship rather than four, because the
  magazine turned out to be a nullable *object* rather than a scalar. It costs a
  `magazine_present` gauge to say whether the other four exist at all, and
  `slots`/`slots_occupied`/`_info` were the planned three. **+1 beyond that**:
  `slots_unreadable`, read off R1.11.2's own contract for `contentsVolser`
  (the literal string `unknown` is a cartridge present but unidentifiable) and
  taken with the maintainer — such a cartridge occupies a slot, blocks the
  import it arrived for, and is invisible to every other collector here.
  **+1**: the background variant's freshness gauge, as on every collector.
  10 + 6 + 6 + 1 = 23, which is what the fixture actually gathers. Like
  `power_supplies` and `node_cards` this is the full per-library figure with no
  projection step: a TS4500 base frame ships exactly two I/O stations and this
  library has no expansion-frame pair, so the capture is the complete
  complement.
  **The four magazine series are absent, not zero, while no magazine is
  reported**, so the real steady-state figure drops to 14 for a library with
  both doors open or both magazines out. That is deliberate and is the one
  cardinality decision here worth restating: a `0` occupancy from a station with
  no magazine reads as an empty magazine ready to accept an import, which is the
  opposite of the truth.
  `volser` does **not** appear on this collector and is not behind a flag either
  — the contents are counted, never labelled. An I/O station's population turns
  over on every import, so `location × volser` would accumulate a Prometheus
  index entry for every cartridge that has ever passed through, for a set that
  is never larger than 18 at any instant. This is the `drives.volser` argument
  with a worse ratio, so there is no opt-in flag to add later.
- `fc_ports`: labels `library`, `model`, `location`, `drive_location`, `state`;
  80 ports × (4 states + speed + 1 `_info`); worst case ~480 series;
  **observed 24** against the 4-port fixture, **445 projected** for the capture's
  real 80 ports, against a worst case that rises to **481**. Three decisions
  rather than a miscount, and this is the first collector whose projection lands
  *below* its worst case for a reason that is not hardware absence.
  **+1 label, 0 series**: `drive_location` was not on the planned line and is not
  optional — it is the join key to the `drives` collector, and `FCPortNoLight`
  cannot be written without it. It rides on the measurement series rather than on
  `_info` alone, on the terms `logical_library` established on `drives`: a
  grouping key, constant per port, so it discriminates nothing that was not
  already counted separately and costs only a vocabulary entry.
  **−36 projected**: a port whose `speedActual` this exporter cannot map emits no
  speed series at all, and 36 of the capture's 80 ports report the literal
  `unknown` — a value R1.11.2 tabulates for neither speed field. The planned
  arithmetic assumed one speed series per port; on a fleet where nearly half the
  ports are dark, that is 36 series that do not exist rather than 36 zeros. This
  is the same shape as the absent rear door on `frames`: the worst case (every
  port lit) is still the planned figure, and the steady state is lower.
  **+1**: the background variant's freshness gauge, as on every collector.
  480 + 1 = 481 worst case; 481 − 36 = 445 at the capture's observed link states.
  `location` alone is the key here, verified against the capture rather than
  inherited: all 80 entries carry a distinct one, because the port number is baked
  into the location string (`...P0` / `...P1`). This is the check the `node_cards`
  entry above explicitly deferred to this collector, and it lands the opposite way
  — no widening needed.
  `volser` does not appear here and there is no per-item flag to add later: an FC
  port has no contents to label.
- `logical_libraries`: labels `library`, `model`, `logical_library`, `media_type`,
  `encryption_method`; 2 × 5; worst case ~10 series; **observed 11**, and this is
  the first collector in the exporter whose planned arithmetic needed no
  correction at all beyond the freshness gauge every collector carries — 2
  partitions × (4 capacity gauges + 1 `_info`) + 1. Like `power_supplies`,
  `node_cards` and `io_stations`, the capture is the library's full complement (2
  partitions, 373 bytes) rather than a trim, so 11 is also the real per-library
  figure with no projection step.
  **+1 label, 0 series**: `encryption_method` was not on the planned line. It is
  configuration, constant per partition, and rides on an `_info` series that is
  emitted anyway, so it costs a vocabulary entry and nothing else — the terms
  `fc_ports` put `speed_setting` and `topology_setting` on `_info` on.
  This is the only collector in the exporter with **no stateset**: R1.11.2 gives
  `/v1/logicalLibraries` seven attributes and none is an enumeration of health,
  so there is no state family here and no emit-observed-anyway branch. It is
  also the narrowest collector after `power_supplies`, and it will stay so — the
  endpoint has no further attributes to add later.
- `cleaning_cartridges`: labels `library`, `model`, `state`, `volser`, `location`,
  plus `access` and `media_type` added at build time.
  **The one collector where `volser` is on by default**, because its population is
  bounded by cleaning policy (70 in the capture), not by library capacity, and the
  per-cartridge `cleansRemaining` is exactly what the operator needs to find the
  exhausted one. 70 cartridges × 1 + ~5 aggregates; worst case ~75 series.
  Reduction flag if a site runs many more: `--collector.cleaning_cartridges.per-volser`,
  which shipped as planned and defaults to `true`.
  **Observed 18** against the 6-cartridge fixture, **146 projected** for the capture's
  real 70, and the doubling is one decision rather than a miscount. The planned 75
  shipped exactly as planned — 70 `cleans_remaining` gauges plus 5 aggregates (3 state
  counts, the summed cleans, the usable count). **+70**: a per-cartridge
  `last_usage_timestamp_seconds`, from `mostRecentUsage`, which was on neither the
  budget nor any alert and was added at the maintainer's explicit choice after the
  cost was put to them — it answers whether a cartridge is sitting unused while its
  neighbours wear out, which nothing else on this fleet can. **+1**: the freshness
  gauge, as on every collector. 75 + 70 + 1 = 146. It is gated by the same
  `--collector.cleaning_cartridges.per-volser` flag, so the reduction lever still
  takes the collector back to 6 series.
  **+2 labels, 0 series**: `access` and `media_type` ride on the `cleans_remaining`
  series, which is emitted anyway. Both were already in the shared vocabulary, so
  they cost nothing there either.
  **The state encoding is the aggregate form, not a per-cartridge stateset**, and
  that is what keeps the figure at one series per cartridge: 70 cartridges × 3
  documented states would have cost 210 series to say what
  `tapelibrary_cleaning_cartridges{state}`'s three say. This is the middle case the
  budget's own tens-versus-thousands rule did not cover — 70 objects is neither —
  and the tie-break used was that no rule selects on a *particular* cartridge's
  state, only on how many are in each.
- `data_cartridges`: labels `library`, `model`, `state`, `logical_library`,
  `media_type`, `cartridge_type`, plus `access` added at build time.
  **Aggregated by default**: counts by state ×
  logical library, by media type, an encrypted/WORM breakdown, and a
  `lifetimeRemaining` histogram (emitted as a `_ratio`, 0-1, not the API's 0-100).
  Worst case ~40 series. Opt-in `--collector.data_cartridges.per-volser` (default
  `false`) adds `volser` and 3 series per cartridge — a lifetime-remaining ratio, a
  last-usage timestamp, and one `_info` carrying the active state and location:
  **~29 250 series** at the 9 749 cartridges this library actually holds.
  **Observed 51 series** against the 8-cartridge fixture, and 51 is also the real
  per-library figure with no projection step — every aggregate here is sized by
  the number of partitions and media types, not by the number of cartridges, so
  the fixture and the 9 749-cartridge library produce the same count. Four
  decisions rather than a miscount, and one arithmetic trap worth recording.
  **The trap first: a histogram is ONE metric but ELEVEN series.** `GatherAndCount`
  returns 41 for this collector because it counts metrics; the lifetime histogram
  expands on the wire to 8 explicit buckets + `+Inf` + `_sum` + `_count`. Every
  earlier line in this budget counted gauges, where the two figures coincide, so
  this is the first collector where the test assertion and the cardinality figure
  legitimately differ. 41 metrics = 51 series. Read every `_ratio`/histogram line
  below this one the same way.
  **+11 over the planned ~40**: the state family is 9 documented states × **3**
  partitions, not 2 — the capture holds one cartridge with a null `logicalLibrary`,
  which is now its own `unassigned` member (see `## Architecture decisions`), so
  that family costs 27 rather than 18.
  **+3, 0 planned**: `tapelibrary_data_cartridges_access`, the documented
  ternary, taken with the maintainer. It was on no plan; on a dual-accessor
  library it is the only inventory-scale signal that a robotics fault has put
  part of the parc out of reach, and 3 fixed series is the cheapest form that
  question has.
  **+1, and this one is the collector's defining decision**:
  `tapelibrary_data_cartridges_lifetime_unknown`. 27 of the capture's 60
  cartridges (45%) report `null` for every cartridge-memory field at once,
  `lifetimeRemaining` included. R1.11.2 defines 0% as "at risk of data loss, may
  not be covered by warranty", so observing that null as 0 would report nearly
  half the parc as spent and page on it. Those cartridges are kept out of the
  histogram entirely and counted here instead, so `_count` + this gauge is the
  full parc. Without it the histogram reads as covering every cartridge when it
  covers 55% of them, which is a silently wrong denominator rather than a missing
  series.
  **−2 on the media family**: counts by media × cartridge type emit only the
  pairs observed, not the cross product. R1.11.2 lists 16 LTO and 12 3592 type
  codes, so a full family would cost 56 series per library to describe an
  inventory that runs one or two types; nothing selects on a particular type, so
  no matcher needs a zero. The capture yields 2 pairs (`JD`, and `unknown` for
  the cartridge-memory-absent cohort).
  27 + 2 + 3 + 3 + 3 + 1 + 11 + 1 = 51.
  **The per-volser projection lands BELOW its plan, at ~24 900 rather than
  ~29 250**, and for the same reason the plan needed correcting at all: only the
  `_info` is emitted for every cartridge. The lifetime ratio is absent for the
  ~45% with no reading (~4 400 series that do not exist rather than 4 400 zeros)
  and the last-usage timestamp is absent for any cartridge never mounted. This is
  the same shape as `frames`' absent rear door and `fc_ports`' unlit ports: the
  worst case is still the planned figure, and the steady state is lower. The flag
  stays `false`, and `TestDataCartridgesCollector_PerVolserKeepsAggregatesIdentical`
  pins that flipping it changes no aggregate, so it is a pure sizing lever and no
  alert depends on it.
- `slots`: labels `library`, `model`, `state`, `tiers`. Aggregated by default: counts by
  state, occupied/empty/total, tier distribution, and summed
  `puts`/`putRetries`/`getRetries`. Worst case ~15 series. Opt-in
  `--collector.slots.per-slot` (default `false`) adds `location` and ~4 series per slot:
  ~~**~42 800 series** at 10 732 slots~~ **(reconciled 2026-08-01: the per-slot figure was
  wrong by the ratio below; see the observation.)**
  **Observed 14** against the 8-slot fixture, and 14 is also the real per-library figure
  with no projection step — the whole point of aggregating is that the default cost does
  not scale with the library. Three corrections, none of them a miscount.
  **+1 label**: `tiers` was not on this line. It carries the slot's physical depth on the
  depth distribution and on the per-slot `_info`, and it is the first label in this
  vocabulary whose values are numeric.
  **The planned ~15 was right and the per-slot ~42 800 was not**, for a reason worth
  stating precisely because it is the shape of the endpoint rather than an arithmetic
  slip: **a `/v1/slots` entry is a COLUMN, not a cartridge position.** Its location
  carries no tier suffix (`slot_F2C1R1`) while the same physical position seen from
  `/v1/dataCartridges` does (`slot_F7C3R15T1`), and one entry describes `tiers` stacked
  positions listed in its `contents` array. The capture's 79 entries cover 199 positions,
  a ratio of 2.52. So `location`-keyed series cost one per COLUMN, and the library's
  10 732 positions are somewhere near **4 300 entries**, hence ~5 × 4 300 =
  **~21 500 series** rather than ~42 800. The entry count itself is still unmeasured: the
  capture is a trim, so 4 300 is the position count divided by the trim's tier ratio, not
  an observation. See `## Open questions / assumptions`.
  **+1 per slot on the ~4**: five per-slot series ship rather than four —
  `positions_occupied`, the three lifetime counters, and `_info`. The `_info` carries both
  the active state and `tiers`, which is what keeps a per-slot stateset out of the build:
  at inventory scale that is the tens-versus-thousands rule, and it would have cost twice
  the entry count to say what one label says.
  The default 14 breaks down as 2 states × 3 families (slot count, positions, occupied),
  2 observed tier depths, `positions_available`, `positions_unreadable`, the three summed
  counters, and the freshness gauge.
  `tapelibrary_slots_positions_available` is the one figure here that exists nowhere else
  in the exporter: free positions in `normal` slots only. R1.11.2 defines `inServiceMode`
  as a state in which a slot "cannot be selected as a cartridge destination", so a free
  position inside one is capacity the robot may not use, and `LibraryCollector`'s capacity
  gauges — which know nothing about slot state — cannot express it. On the 8-slot fixture
  it is 5 where a naive free-position count gives 6.
- `events`: labels `library`, `model`, `severity` (5 documented values). Counts and
  last-seen timestamp per severity over the returned window; worst case ~10 series.
  `state` is not a label, the manual documenting it as interpolated free text.
  **Observed 7** against the 10-event fixture — 5 severity counts (all five always
  emitted, four at 0) + 1 last-seen (only `information` occurs in the capture) +
  the freshness gauge — and **~11 projected** as the realistic per-library worst
  case at defaults: the same 5 counts, up to 5 last-seen readings if every severity
  fires within one window, and the freshness gauge. Two series beyond that are
  reachable but unlikely, a count and a last-seen under the `unknown` token, which
  only a library reporting a null severity would produce.
  The planned figure needed no correction, but the shape underneath it did: the
  plan said "over the returned window", and there is no such thing — see the
  `## Session log` entry for this collector. The budget survives because bounding
  the window by `after` restores exactly the semantics the count was planned with.
  `errorCode` resolved **opt-in** rather than excluded outright: the open question
  offered aggregate-only, an allow-list or a bounded top-N, and the maintainer took
  the allow-list. `--collector.events.error-codes` is empty by default, so the
  default budget is the one above and unchanged; each code named adds one series per
  severity that code is actually observed with, which is 1 in practice and bounded by
  the operator's own flag rather than by the 65 536 values a 4-digit hex field admits.
  Nothing on this fleet has yet raised an error-severity event, so there is no code
  to put in it today — the flag exists so that there is a bounded answer ready when
  one appears, rather than a cardinality decision taken under incident pressure.
- `data_cartridges_lifetime`: labels `library`, `model`, `direction`, **`correction`**.
  Histograms over `motionMeters`, `mounts`, `dataWrittenToCartridge` and the four
  error counters; worst case ~60 series. Opt-in
  `--collector.data_cartridges_lifetime.per-volser`
  (default `false`) adds `volser` and 7 series per cartridge — `motion_meters_total`,
  `mounts_total`, `written_bytes_total`, and `errors_{corrected,uncorrected}_total`
  × `direction={read,write}`: **~68 250 series**. These are the one place the target
  hands over genuine monotonic lifetime counters, so they are `CounterValue` with
  `_total`, not Gauges.
  **Observed 66 series (10 metrics)** against the 8-cartridge fixture, and 66 is also
  the real per-library figure with no projection step: every aggregate is a fixed
  number of histograms over the parc, so the fixture and the 9 749-cartridge library
  produce the same count. Read the metric/series gap the way the `data_cartridges`
  line above already sets out — `GatherAndCount` returns **10** because it counts
  metrics, and the seven histograms each expand on the wire to 6 explicit buckets +
  `+Inf` + `_sum` + `_count`. 7 × 9 = 63, + 2 `unknown` reasons + the freshness gauge
  = 66. The per-volser projection lands at 6 valid cartridges × 7 = 42 on the
  fixture, and ~68 250 per library, matching the plan.
  **+6 over the planned ~60, and all six are the error family's second label.** The
  plan carried `direction` alone, which would have made the four error counters two
  metrics of two series. R1.11.2 reports the cross product —
  `errorsCorrected{Read,Write}` and `errorsUncorrected{Read,Write}` — so the honest
  shape is one family with `direction` AND `correction`, which is 4 histograms rather
  than the planned 2. Same information either way; one family means a rule can select
  every uncorrected error in a single matcher, which
  `DataCartridgeUncorrectedErrorsRising` does.
  **Bucket count is 6 per histogram, chosen against the capture rather than by
  taste**, and it is the lever that kept this collector near its budget: 8 bounds
  (the `data_cartridges` lifetime histogram's count) would have cost 7 × 2 = 14 more
  series to resolve distributions nothing alerts on at that precision.
  **The two `unknown` reasons are the collector's defining decision**, and they are
  the reason this line reads 2 rather than 1 where `data_cartridges` reads 1.
  `reason="unread"` is the cartridge-memory cohort, 27 of the capture's 69 (39%) —
  the same population `data_cartridges` meets at 45%, seen from the endpoint that
  reads nothing else. `reason="invalid"` is new, and the capture forced it: one
  cartridge reports `motionMeters` **-285 211 648** beside 50 mounts and 0 bytes
  written. Negative is impossible on a cumulative counter, and read as unsigned
  32-bit it is four million kilometres of tape on a cartridge mounted fifty times, so
  it is not a wrap either — it is cartridge memory that is simply wrong. Merging the
  two reasons would have hidden a genuine fault inside a 39% baseline that nothing
  should ever alert on. **One bad counter disqualifies the whole cartridge record**,
  which is what keeps `<family>_count + unread + invalid == the parc` true for all
  seven families at once instead of giving each its own denominator.
- `reports_library`: labels `library`, `model`. Newest complete hourly window only:
  7 activity counters + 3 temperature + 3 humidity + 1 window timestamp;
  worst case ~14 series; **observed 15** (2026-08-02).
  The extra one is `..._window_duration_seconds`, added during the build and not
  in the plan. The plan already required a window-age mitigation (see
  `## Open questions`, now resolved), and building it surfaced the adjacent
  question the timestamp alone cannot answer: a window is only comparable to the
  one before it if it covers the same span, and "newest **complete** window" was
  an assumption this budget stated without giving anyone a way to check it. One
  series turns that assumption into a reading. All four windows in the capture
  report `3600`.
  Emitted series vary from 9 to 15 rather than being fixed, which no other
  collector in this budget does: the six environmental readings are absent, not
  zeroed, when no drive reports them, and a 0 °C / 0% RH would sit outside the
  operating envelope in both directions and page. The nine always-emitted series
  are what keep `StatusTracker` truthful on a sensorless library.
- `reports_drives`: labels `library`, `model`, `location`; 40 drives × ~13;
  worst case ~520 series; **observed 641** (2026-08-03), i.e. 40 × 16 + 1.
  The gap is 3 series per drive, and every one of them was a decision taken with
  the maintainer during the build rather than a metric that crept in.
  **Two are the window's own identity** (`..._window_timestamp_seconds` and
  `..._window_duration_seconds`), carried at per-drive granularity rather than
  once for the collector. `reports_library` pays 2 series for the same pair and
  the plan simply did not restate the cost when the same shape was applied to 40
  objects. Per drive was chosen anyway, against a cheaper single global pair,
  because the timestamp is the only thing that shows a *single* drive falling out
  of the report — a library-wide one reads as healthy while 1 of 40 drives has
  gone silent, which is the exact failure the gauge exists for. The duration
  follows it rather than being split off, to avoid an exporter-invented aggregate
  ("the oldest window across drives") standing where the library's own number
  should be.
  **The third is the error split**: `errors_corrected_read`,
  `errors_corrected_write` and `errors_uncorrected` are three series where the
  ~13 estimate assumed two. See `## Architecture decisions`.
  The always-emitted floor is 10 series per drive; the 6 environmental readings
  are absent rather than zeroed per drive, so a library whose drives report no
  sensors emits 401 rather than 641, and one sensorless drive among 39 healthy
  ones costs only its own 6.
  **Fleet impact: ~3 205 series across five libraries against the ~2 600 planned**,
  which the totals below absorb without changing any conclusion they draw.
- `reports_accessors`: labels `library`, `model`, `location`, plus `gripper` and
  `axis`; 2 × ~13; worst case ~26 series; **observed 21** (2026-08-03), against a
  worst case that is actually **33** (2 × 16 + 1). Both figures differ from the plan
  and in opposite directions, which is the interesting part.
  **The worst case is higher than planned because the plan counted 13 metric NAMES
  and the collector emits 16 SERIES per accessor.** Three of the five activity
  families carry a second label the plan did not price: `travel_meters` splits on
  `axis` (2 series), `gets` and `puts` each split on `gripper` (2 each). Those
  labels are not an addition — `accessors` already spells its lifetime counterparts
  the same way, and R1.11.2 reports both cross products completely, which is the
  test `## Architecture decisions` sets for a label being available at all. The
  per-accessor window timestamp and duration are the same 2 series `reports_drives`
  already paid for and the plan again did not restate.
  **The observed figure is lower than planned because all six environmental
  readings are absent on this hardware.** The always-emitted floor is 10 series per
  accessor, and every entry of the 2026-07-28 capture reports null for
  temperature and humidity alike, so this fleet emits 2 × 10 + 1 = 21. A library
  whose accessors carried sensors would reach 33. That gap is the collector's
  documented shape rather than a discrepancy: the six descriptors ship so hardware
  that reports them needs no code change.
  **Fleet impact: ~105 series across five libraries** — the smallest of the three
  `reports_*` collectors by an order of magnitude, and negligible against the
  totals below.
- `diagnostic_cartridges`: labels `library`, `model`, `state`, `access`, `volser`,
  `location`, `media_type`, `cartridge_type`, `worm` (bounded, 5 cartridges in the
  capture); 5 × (5 states + 1 `_info`); worst case ~30 series; **observed 23**
  (2026-08-03).
  The plan's arithmetic assumed a per-CARTRIDGE stateset (5 × 5). What shipped
  puts the stateset library-WIDE instead — `tapelibrary_diagnostic_cartridges{state}`
  counts cartridges per state, 5 series total rather than 25 — because a per-cartridge
  stateset answers a question the `_info` label already answers, at five times the
  cost. The library-wide form additionally survives
  `--collector.diagnostic_cartridges.per-volser=false`, which is what lets both alerts
  keep working when a site turns the detail off.
  The 23 break down as 5 state + 3 `access` + `usable` + `lifetime_unknown` (10
  always-emitted, flag-independent), then 5 `_info` + 5 `last_usage` + 2
  `lifetime_remaining_ratio` behind the flag, plus the freshness gauge. The
  remaining-life series number 2 rather than 5 because three of the five cartridges
  report no cartridge memory at all.
  With `--collector.diagnostic_cartridges.per-volser=false` it drops to **11**, and
  no alert loses its input.
  **Fleet impact: ~115 series across five libraries.**

**Fleet totals, MEASURED 2026-08-03 against all five libraries.** The estimate
below stood for a month and was wrong by half, for a reason no capture could
have shown: **this fleet is not homogeneous.** Every figure in this section was
derived from `library1` and multiplied by five.

| library | drives | FC ports | frames | cartridges | **series** |
|---|---|---|---|---|---|
| `library1` | 40 | 80 | 12 | 9 749 | 2 637 |
| `library2` | 40 | 80 | 11 | 8 779 | 2 566 |
| `library3` | 64 | 128 | 11 | 8 401 | 3 636 |
| `library4` | 64 | 128 | 11 | 8 378 | 3 703 |
| `library5` | 96 | 192 | 15 | 11 405 | 5 473 |
| **fleet** | **304** | **608** | **60** | **46 712** | **18 015** |

Plus 44 series of self-instrumentation, for **18 059 on `/metrics`**.

**Observed 18 015 against the ~12 150 planned, +49%, and drives are the whole
of it.** The budget assumed 200 drives (40 x 5); the fleet has 304, and
`library5` alone carries 96. Drives are also the most expensive object here,
at 18 stateset series each before anything else (`_state` 9 + `_operation` 6 +
`_access` 3), so the three largest families on the wire are
`tapelibrary_drive_state` (2 736), `tapelibrary_fc_port_state` (2 432) and
`tapelibrary_drive_operation` (1 824) — 39% of the fleet total between them.
FC ports track drives at exactly 2:1 on all five machines.

**The rule for anyone re-running this arithmetic: size a per-object budget from
the LARGEST member, not from the one that happened to be captured.** `library5`
is 2.1x `library2`, and nothing in the 2026-07-28 capture hinted at it.

Even so the conclusion the estimate drew still holds, which is why it is
corrected rather than rewritten: 18 059 series is comfortable, and the reason
to keep the three per-item flags off by default is unchanged.

**Superseded estimate.** Defaults: ~2 425 series per library, **~12 150 across five
libraries** — comfortable. With all three per-item flags enabled: ~142 800 per
library, **~714 000 across five**, which is a Prometheus sizing decision in its own
right and the reason all three default to `false`.

Every figure above is derived from the manual's own state tables, not from the legacy
scripts' maps and not from the capture's observed values — the capture shows a healthy
library, so it exercises perhaps a third of the enumerated states.

## Dashboards

- (none yet)

## Session log

- 2026-07-31 `/design-exporter` tapelibrary_exporter: walked the discovery ladder
  (rung 2 = IBM R1.11.2 manual §6; rung-4 material = preserved 2026-07-28 live
  capture, 21 endpoints; rungs 1 and 3 unavailable, rung 4 re-probe declined).
  Confirmed all six architecture decisions with the maintainer: RoE REST read-only,
  `http` flavor, `multi-instance`, namespace `tapelibrary`, concurrency ceiling 1,
  stateset enum encoding, 18 planned collectors, per-item detail behind three
  default-off flags. Copied the ephemeral legacy scripts and alert rules out of
  `/tmp/lib/` into `samples/legacy/` before they were lost.
- 2026-07-31 `/design-exporter` tapelibrary_exporter (follow-up, same session): worked
  `drives` and the per-volser detail through to concrete metric shapes against the
  manual's own enumerations, which corrected three things written earlier in the
  session. The `drives` budget rises from ~640 to ~800 series (9 documented states,
  not 8, plus the `access` ternary). The shared label vocabulary gains `operation`,
  `access` and `direction`, and its generic `type` becomes `cartridge_type` —
  Prometheus's exporter guidance names `type` as the canonical too-generic label. And
  the full-stateset-vs-active-state-only rule is now written down under
  `## Cardinality budget`, since it is what keeps the per-volser figures affordable
  and it existed only in conversation before.
- 2026-07-31 `/design-exporter` tapelibrary_exporter (follow-up, same session):
  extracted every state enumeration from the manual's own tables after the maintainer
  asked whether the legacy maps were complete. **They were not**, and on four
  resources they are actively wrong for R1.11.2 — the frame door states no longer
  exist, `accessors.reorienting` is documented as `calibrating`, `nodeCards.normal` is
  documented as `online`, and `library.restarting` is not a library state at all. The
  full vocabulary is now recorded under `## Architecture decisions`, the four
  contradictions under `## Open questions / assumptions`, and the budget re-derived
  from real counts: ~2 425 series per library, ~12 150 across the fleet. Also found
  that `events.severity` has five values (two meaning *resolved*) rather than the
  three assumed earlier, and that `events.state` is interpolated free text that must
  never become a label.
- 2026-07-31 `/new-prometheus-exporter` tapelibrary_exporter: scaffolded into
  `./tapelibrary_exporter` with `--flavor http --target-model multi-instance
  --forge github --instance-label library`, module
  `github.com/sckyzo/tapelibrary_exporter`, owner `sckyzo`, GPL-3.0. Every
  `## Architecture decisions` line was read back to the maintainer and confirmed
  unchanged; nothing was overruled. Applied `exporter.max-requests-per-target: 1` to
  `config.example.yml` from the recorded concurrency ceiling. Copied the full source
  material — IBM manual, 21 endpoint captures, legacy scripts — into the repository's
  `samples/`, which `.gitignore` keeps out of every commit. This brief became the
  journal here; the original `./exporter-design-brief.md` was removed in the parent
  directory, its content living on in this file. Ticked the scaffold's own `example`
  collector, the background variant this target model requires.
- 2026-07-31 `/add-collector` library: built the `library` collector (background, the
  only variant `multi-instance` admits) against `GET /v1/library`. Fixture derived
  from `samples/test_data/probe-2026-07-28/library.json`, trimmed to the fields the
  parser reads and re-anonymised (`LIB-A` → `library1`, `SN00000044` → `SN00000001`);
  the real magnitudes were kept. Nine descriptors, 25 series observed against 24
  planned — the extra one is the background variant's freshness gauge, which the
  budget had not counted for any collector. Established two conventions binding on
  the 17 collectors still to come: the `_info` label keys (`name`, `serial`,
  `firmware`, recorded under `## Architecture decisions`) and the stateset's
  emit-an-undocumented-status-anyway branch, which has its own test. Dropped
  `restarting` from the ported critical selector because R1.11.2 does not list it as
  a library status; logged that, the five unclassified statuses, and the unused
  `cartridgeAccess` field under `## Open questions`. `make check` green.
- 2026-07-31 `/add-collector` frames: built the `frames` collector (background,
  the only variant `multi-instance` admits) against `GET /v1/frames`. Named
  `frames`, not the `frame` the command was invoked with, to match this list
  and the endpoint. Fixture derived from
  `samples/test_data/probe-2026-07-28/frames.json`, trimmed from 12 frames to
  the 3 that carry every distinct shape in the capture (expansion with drives,
  base with I/O stations, storage-only with neither) and re-anonymised
  (`SN00000043`/`44`/`45` → `SN00000001`/`2`/`3`); the three `*LastChanged`
  fields the parser does not read were dropped, and every per-frame magnitude
  kept. Eight descriptors, 40 series observed against the fixture and 157
  projected for a real 12-frame library, under the ~168 planned — the
  difference is the absent rear door, recorded in the budget. Two conventions
  extended for the 16 collectors still to come: `_info` gains `mtm`,
  `frame_type` and `media_type` (a resource with neither a name nor its own
  firmware), and `frame_type` joins the shared label vocabulary under the same
  no-bare-`type` rule that produced `cartridge_type`. Resolved the first of the
  four legacy-vs-manual contradictions: the three door pseudo-states are not in
  the shipped selectors, and the door signal ships as `FrameDoorOpen` over
  `tapelibrary_frame_door_open` instead. A door position that is null (every
  frame's rear door on this hardware) or unrecognised emits no series rather
  than a `0` asserting a closed door, which has its own test. `make check`
  green.
- 2026-07-31 `/add-collector` accessors: built the `accessors` collector
  (background, the only variant `multi-instance` admits) against
  `GET /v1/accessors`. Fixture derived from
  `samples/test_data/probe-2026-07-28/accessors.json`, both accessors kept
  (that is the real population), `stateReferenceEvent` dropped as the parser
  does not read it. **Nothing needed anonymising**: this endpoint returns no
  serial, hostname, account or firmware string — only generic hardware
  locations (`accessor_Aa`, `accessor_Ab`, identical on every TS4500) and
  lifetime robotics counters, all kept at their real magnitudes.
  Thirteen descriptors, 51 series observed against 46 planned; the +8/−4/+1
  breakdown is recorded against the budget line rather than replacing it.
  Resolved the second of the four legacy-vs-manual contradictions
  (`reorienting` → `calibrating`). Two decisions taken with the maintainer
  rather than from the plan: the `access` label was budgeted for this
  collector but no access series were, and `driveAccess`/`cartridgeAccess`
  both ship as statesets, because an accessor can be `onlineActive` and still
  be blocked by the other one — which no other metric on this endpoint can
  say. That established a convention for the 15 collectors still to come: the
  `access` label's value set is read per-resource, two values here and three
  on drives and cartridges, never inherited. Five alert rules shipped rather
  than the three planned, two of them following from the access statesets.
  `failedToInitialize` was added to the critical selector on the manual's own
  prose grouping, which is the only classification in this file that came
  from neither the legacy scripts nor a state table; it is logged under
  `## Open questions` for confirmation. Verified against the manual (§`/v1/accessors`)
  rather than assumed: `travelX`/`travelY` are meters, `velocityScaling*` are
  0-100 percentages emitted as ratios, and the four nullable fields are
  nullable by IBM's own documentation. `make check` green, `make docs-check`
  green with no warnings.
- 2026-07-31 `/add-collector` drives: built the `drives` collector (background,
  the only variant `multi-instance` admits) against `GET /v1/drives`. Fixture
  derived from `samples/test_data/probe-2026-07-28/drives.json`, trimmed from
  40 drives to the 4 that carry every distinct shape in the capture (both
  states, all three operation shapes including the null, both `use` values,
  both logical libraries, both firmware levels, and volser present on two of
  the four); `barcode`, `interfaceMode`, `elementAddress` and `beacon` dropped
  as the parser does not read them, and `sn`/`wwnn` re-anonymised from their
  capture positions to `SN0000000{1..4}` / `500507640000000{1..4}`. All other
  magnitudes and strings kept, `Library-5`/`Library-6` included — they are
  IBM's own default logical-library naming and identify nothing.
  Seven descriptors, 81 series observed against the fixture and 801 projected
  for a real 40-drive library. **The first collector whose planned arithmetic
  needed no correction**: 20 series per drive, exactly the 9 + 6 + 3 + 1 + 1
  the budget line predicted, plus the freshness gauge every background
  collector adds.
  Four decisions taken with the maintainer rather than from the plan.
  `logical_library` rides on the measurement series as well as `_info`, which
  is what makes the per-logical-library floor rule a plain aggregation instead
  of a join, at the cost of breaking series continuity when a drive is
  reassigned — recorded as a convention binding on `data_cartridges` and
  `logical_libraries`. `volser` ships behind `--collector.drives.per-volser`
  (default `false`), the fourth per-item flag rather than the three the budget
  counted. `use`, `encryption`, `interface` and `wwnn` join `_info`; `use` is
  the one that earns it, and `ControlPathRedundancyLost` is what reads it.
  And `media_type` stays on `_info` alone, following `frames`.
  Two conventions established for the 14 collectors still to come, both from
  fields no earlier collector had: the timestamp layout
  (`2006-01-02T15:04:05-0700`, not RFC3339 — the offset carries no colon) with
  null/unparseable emitting no series rather than a 1970 epoch; and the
  narrowing of the null rule, since `drives.operation`'s null is a documented
  *value* (`none`) rather than a missing reading, which is the opposite of
  `accessors.temperature`. Both have their own test.
  Six alert rules shipped rather than the three planned, and one deliberately
  not shipped: `last_cleaned` has a real direction but no grounded threshold,
  and the cause it would signal is already covered by the ported
  `cleaning_cartridges` figures. `make check` green (vet, lint 0 issues, all
  packages' tests, govulncheck, actionlint, zizmor, deadcode, docs-check);
  `make docs-check` green with no warnings. `promtool` is not installed on this
  host, so the six new rules were checked as YAML and by eye, not by
  `promtool check rules`.
- 2026-08-01 `/add-collector` power_supplies: built the `power_supplies` collector
  (background, the only variant `multi-instance` admits) against
  `GET /v1/powerSupplies`. Fixture derived from
  `samples/test_data/probe-2026-07-28/powerSupplies.json`, kept **whole** — all
  eight supplies, untrimmed, the first fixture in this repository that is the real
  population rather than a subset, because eight is what a twelve-frame library
  actually reports (only L25/L55 and D25/D55 frames carry supplies) and 8 × 3 states
  is small enough to pin in full. **Nothing needed anonymising**, on the same terms
  as `accessors` but more strongly: this endpoint returns exactly two attributes, a
  generic hardware location (`powerSupply_F1PSa`, identical in grammar on every
  TS4500) and a state enum — no serial, hostname, firmware, account or capacity
  figure exists on it to anonymise. The original was left in `samples/`.
  Two descriptors, 25 series observed against 24 planned, the +1 being the freshness
  gauge as on every collector. The narrowest collector in the exporter, and
  structurally so rather than by omission: with only `location` and `state`
  documented there is no `_info` series to add later. No new label keys and no new
  conventions — `location` and `state` were already in the shared vocabulary, and
  the stateset's emit-an-undocumented-state-anyway branch was inherited unchanged
  and retested. The capture shows a wholly healthy library (every supply `online`),
  so the `failed` transition the critical rule reads is covered by a synthetic-body
  test rather than by the fixture; without it the one event this collector exists to
  detect would have shipped untested. Two alert rules shipped rather than the one
  planned, splitting the planned expression into a warning safety net and a narrower
  critical on `failed` — reasoning recorded against the candidate under
  `## Architecture decisions`. Also added the three flags and the collector row to
  `docs/configuration.md`, which no gate covers. `make check` green (vet, lint, all
  packages' tests, govulncheck, actionlint, zizmor, deadcode, docs-check);
  `make docs-check` green with no warnings. `promtool` is still not installed on
  this host, so the two new rules were parsed as YAML and checked by eye, not by
  `promtool check rules`.
- 2026-08-01 `/add-collector` node_cards: built the `node_cards` background collector
  against `GET /v1/nodeCards`, fixture derived from
  `samples/test_data/probe-2026-07-28/nodeCards.json` (trimmed to the nine fields the
  parser reads, serials renumbered `SN00000055-62` → `SN00000001-8` to match the
  per-collector convention every other fixture already follows; IBM part numbers and
  the `LCC`/`MDA`/`ACC` type strings kept verbatim as public product identifiers, and
  the `frame_F*`/`accessor_A*` locations kept because they are the label's whole
  point). **This is the collector that broke the one-key-per-location assumption every
  earlier one was built on**: an accessor carries two cards at a single `location`, so
  the key here is `location` + the new `card_type` label and the parser rejects a
  duplicate of the pair. Recorded against `## Architecture decisions` as a rule for
  the collectors still unbuilt, not just as this collector's quirk. Shipped 12 series
  beyond the planned budget (`lastRestart`, `primaryLCC`, `reportingLCC` on the 4 LCC
  cards) after reading them off the capture — reasoning under `## Cardinality budget`.
  Confirmed R1.11.2's `online` over the legacy scripts' `normal` for the healthy
  state, the third time the manual has won that kind of disagreement; the
  emit-observed-anyway branch has a dedicated test named for it, so a library
  reporting `normal` reopens the question instead of losing the signal. Two alert
  rules shipped rather than the one planned: the planned per-card warning verbatim,
  plus a critical `NodeCardNoReportingLCC` that aggregates rather than matching a
  card, because no LCC holding the reporting role is a library-level fact and no
  per-card rule can see it. `make check` green (vet, lint, all packages' tests,
  govulncheck, actionlint, zizmor, deadcode, docs-check); `make docs-check` green with
  no warnings. `promtool` is still not installed on this host, so the two new rules
  were parsed as YAML and checked by eye, not by `promtool check rules`.
- 2026-08-01 `/add-collector` io_stations: built the `io_stations` background
  collector against `GET /v1/ioStations`. Fixture derived from
  `samples/test_data/probe-2026-07-28/ioStations.json`, kept **whole** at both
  stations (the library's real complement — a TS4500 base frame ships exactly
  two and this one has no expansion pair), trimmed to the fields the parser
  reads (`contentsInternalAddress` and `door.lastChanged` dropped) and the four
  cartridge VOLSERs renumbered `TST065/017/054/048JA` → `TST001-004JA`. Those
  four were the only thing needing anonymisation: everything else on this
  endpoint is a generic hardware location (`ioStation_F2IOu`, identical in
  grammar on every TS4500), a state enum, a media type and a slot count. The
  original was left in `samples/`.
  Eight descriptors, 23 series observed against 18 planned; the −1 label / +2 /
  +1 / +1 breakdown is recorded against the budget line rather than replacing
  it.
  **Three decisions taken with the maintainer rather than from the plan**, all
  three resolved toward the conservative option. The planned `door` label was
  **dropped**: an I/O station has one door, so the label could only hold a
  constant, and the shared vocabulary now records `door` as a `frames`-only key
  — the first time a planned label turned out too wide rather than too narrow,
  which is the mirror of what `node_cards` established the day before.
  `door.lastChanged` is **read by nothing**, following the precedent `frames`
  set for its three `*LastChanged` fields: an alert's own `for:` clause already
  expresses "open too long", which was the entire planned signal. And
  `slots_unreadable` **ships**, the one metric here that comes from R1.11.2's
  contract rather than from the capture or the plan.
  Two conventions applied rather than established, both inherited and both
  load-bearing here: a door position that is null (every Diamondback station) or
  unrecognised emits **no series** rather than a `0` asserting a closed door,
  and the four magazine series are **absent** rather than zeroed while no
  magazine is reported — a `0` occupancy reads as "empty and ready for an
  import", the opposite of the truth. Both have their own test.
  Four alert rules shipped rather than the two planned, and **every one of them
  is a warning**, which is a deliberate departure from the warning/critical
  tiering every collector above uses: R1.11.2 defines no state on this endpoint
  as a hard failure the way it defines `powerSupplies` `failed`, and nothing
  here stops the library serving mounts. The two planned candidates ship
  verbatim (`IOStationDoorOpen`, `IOStationFull`); `IOStationDegraded` is the
  usual stateset safety net, and `IOStationUnreadableCartridge` follows from the
  metric beyond the plan. `IOStationFull` compares the two gauges rather than a
  hardcoded capacity, so it is correct for both magazine sizes (18 LTO, 16
  3592). Every `for:` here is a re-derived **default**, not a measured
  threshold — the legacy scripts in `samples/legacy/` never collected this
  endpoint. Also added the three flags and the collector row to
  `docs/configuration.md`, which no gate covers.
  `make check` green (vet, lint 0 issues, all packages' tests, govulncheck,
  actionlint, zizmor, deadcode, docs-check); `make docs-check` green with no
  warnings. `promtool` is still not installed on this host, so the four new
  rules were parsed as YAML and checked by eye, not by `promtool check rules`.
- 2026-08-01 `/add-collector` fc_ports: built the `fc_ports` collector
  (background, the only variant `multi-instance` admits) against
  `GET /v1/fcPorts`. Fixture derived from
  `samples/test_data/probe-2026-07-28/fcPorts.json`, trimmed from 80 ports to
  the 4 that carry every distinct shape in the capture (both states, the common
  16Gbps link, the deliberately pinned 8Gbps port, and the odd dark-but-reporting
  1Gbps one). Re-anonymised into the fixture's own positions so it lines up with
  `testdata/drives.json`: `SN00000027`/`SN00000030` → `SN00000004`/`SN00000005`,
  and the four WWPNs → `50050764000001{01,02}` / `...0401` / `...0501`, a shape
  that keeps the drive's WWNN prefix visible in its ports. `portNumber` dropped
  as the parser does not read it; locations and loop IDs kept at their real
  values, being generic hardware positions identical on every TS4500 of this
  layout. Four descriptors, 24 series observed against the fixture and 445
  projected for the real 80 ports, under the 481 worst case.
  Resolved the key check the `node_cards` entry explicitly deferred to this
  collector: `location` alone IS the key here — all 80 entries carry a distinct
  one because the port number is baked into the location string — so unlike
  `node_cards` the label set did not need widening. That is now two collectors
  where the planned key was checked against the capture and landed differently,
  which is the whole point of checking.
  Three decisions taken with the maintainer rather than from the plan. The speed
  string becomes a **`_bytes_per_second` gauge** converted at 8 bits per byte,
  with the nominal-rate-not-throughput caveat stated in the help text, the docs
  and `## Architecture decisions`; bits was rejected as a non-base unit outside
  this exporter's own name shape. **`drive_location` rides on the measurement
  series**, extending the shared vocabulary with its first cross-resource label
  and making `FCPortNoLight` a join rather than a `group_left`. And the planned
  speed alert, **undefined on a fleet that is 99% auto-negotiated**, ships as a
  self-calibrating peer-max comparison instead of an invented threshold.
  One convention applied rather than established, and it is what moves the
  projection off the plan: a port whose rate this exporter cannot map emits **no
  speed series** rather than a `0`, and 36 of the capture's 80 ports report an
  `unknown` R1.11.2 tabulates nowhere. A `0` would assert a link running at zero
  bytes per second and would make `FCPortSpeedBelowPeers` fire on every dark port
  forever. Has its own test, across all three unmappable shapes.
  `gocritic` caught a 152-byte struct copy in `parseFCPorts`'s validation loop on
  the first `make check`; fixed by indexing, the same way `refresh` already did.
  Also added the three flags and the collector row to `docs/configuration.md`,
  which no gate covers.
  `make check` green (vet, lint 0 issues, all packages' tests, govulncheck,
  actionlint, zizmor, deadcode, docs-check); `make docs-check` green with no
  warnings. `promtool` is still not installed on this host, so the two new rules
  were parsed as YAML and checked by eye, not by `promtool check rules` — and
  both are joins, which is the rule shape that check would be most worth having.
- 2026-08-01 `/add-collector` logical_libraries: built the `logical_libraries`
  collector (background, the only variant `multi-instance` admits) against
  `GET /v1/logicalLibraries`. Fixture taken from
  `samples/test_data/probe-2026-07-28/logicalLibraries.json` **unchanged**: the
  capture is only 373 bytes and is already the library's full complement, and its
  two partition names (`Library-5`, `Library-6`) are the same values
  `testdata/drives.json` already carries, so re-anonymising them here would have
  broken the cross-collector join the `logical_library` label exists to serve.
  Nothing was trimmed and nothing renamed; the counts are the real ones. Six
  descriptors, 11 series observed against 10 planned, the difference being the
  freshness gauge every background collector carries.
  **The first collector in this exporter with no stateset.** R1.11.2 gives this
  endpoint seven attributes and not one of them enumerates health, so there is no
  state family, no emit-observed-anyway branch and no severity classification to
  port — which also makes it the first collector whose alert reads a *ratio of
  two of its own gauges* rather than a state selector.
  Two decisions beyond the plan. **`encryption_method` ships as an `_info`
  label** rather than the stateset its six documented values would have allowed,
  costing a vocabulary entry and zero series; the general rule that follows
  (stateset when a rule selects on values, `_info` when only transitions matter)
  is recorded under `## Architecture decisions`. And **the saturation
  thresholds were raised to 0.98/0.99** from the round 0.90/0.95 this file uses
  elsewhere, because the capture already sits at 0.964 and 0.971 and the obvious
  numbers would have paged on the whole fleet on day one.
  One convention applied rather than established, and it cuts the opposite way to
  the `fc_ports` precedent: **zero counts here are emitted, not withheld**. The
  absent-rather-than-zero habit covers readings nobody took (a missing sensor, an
  absent rear door, an unmappable link rate); R1.11.2 types all four fields here
  as plain numbers, so a partition with zero cartridges or zero assigned drives
  is reporting a real and alarming value that must reach a dashboard. Has its own
  test.
  `make check` green (vet, lint, all packages' tests, govulncheck, actionlint,
  zizmor, deadcode, docs-check); `make docs-check` green with no warnings.
  **`promtool` was run this session for the first time**, via
  `prom/prometheus:latest` in Docker rather than a host install — and it
  immediately found that `FCPortSpeedBelowPeers`, shipped earlier the same day,
  cannot parse. See `## Open questions / assumptions`. The two rules added here
  were validated in isolation (`SUCCESS: 2 rules found`) because that pre-existing
  error aborts the file before promtool reaches them.
- 2026-08-01 `/add-collector` cleaning_cartridges: background variant, the eleventh
  collector and the fifth of the day. Fixture derived from
  `samples/test_data/probe-2026-07-28/cleaningCartridges.json`, trimmed from 70
  entries to 6 and kept faithful (every cartridge `normal`, every one reporting a
  usage timestamp); the branch tests feed inline bodies instead, per the
  `node_cards` convention. Nothing needed anonymising beyond what the capture
  already carried — volsers were already `TSTnnnJA` placeholders and locations are
  structural — and `internalAddress` was dropped in the trim because the collector
  does not parse it.
  **The finding of this session is that `volser` is not a unique key**, on this or
  any cartridge endpoint: 70 cartridges under 63 volsers in the capture, and
  R1.11.2's own example for the endpoint prints two `CLNI01L1` entries. The plan
  had `volser` as the key. Every series is keyed on `volser` + `location` instead,
  the parser rejects a duplicate of the pair, and the rule is recorded under
  `## Architecture decisions` because `data_cartridges` and
  `diagnostic_cartridges` both plan a `volser` label and would otherwise fail
  `Gather` for all 18 collectors at once, not merely their own.
  Two decisions put to the maintainer before any code was written, both on the
  cardinality budget: the per-cartridge series shape (**one series**, active state
  on the measurement, keeping the planned 70 × 1) and `mostRecentUsage`
  (**shipped**, +70). Net 146 projected against a planned ~75, itemised under
  `## Cardinality budget`.
  One convention established rather than applied: **this is the only collector in
  the exporter that accepts an empty array**. Every other one treats `[]` as a
  response that lost its content, because a TS4500 always has a frame, an
  accessor, an LCC and a partition. It does not always have a cleaning cartridge —
  they are consumable, and running out is exactly what
  `CleaningCartridgesExhausted` pages on, so rejecting `[]` would keep serving a
  comfortable stale list at the moment the alert needed to fire. Has its own test,
  including the `StatusTracker` half.
  `make check` green (vet, lint 0 issues, all packages' tests, govulncheck,
  actionlint, zizmor, deadcode, docs-check); `make docs-check` green with no
  warnings. The three new rules were validated in isolation via
  `prom/prometheus:latest` (`SUCCESS: 3 rules found`); the full file still fails on
  the same pre-existing `FCPortSpeedBelowPeers` parse error at line 801, byte for
  byte the message the previous session recorded, unchanged by this commit and
  still deliberately not fixed here. **It is now blocking a second set of rules,
  including the only measured thresholds in the exporter.**

- 2026-08-01 `/add-collector data_cartridges`: built the `data_cartridges`
  background collector against `GET /v1/dataCartridges`, fixture derived from
  `samples/test_data/probe-2026-07-28/dataCartridges.json` (60 entries trimmed to
  8, chosen to carry every shape the capture has rather than the first eight).
  Eleven descriptors, 41 metrics / **51 series** observed against the fixture, and
  51 is also the real per-library figure: every aggregate is sized by partition
  and media-type count, not by cartridge count, so the 8-cartridge fixture and the
  9 749-cartridge library produce the same total. Itemised under
  `## Cardinality budget`, along with the arithmetic trap this is the first
  collector to hit — a histogram is one metric to `GatherAndCount` but eleven
  series on the wire.
  **The endpoint's defining fact, which no plan anticipated: 45% of cartridges
  report no cartridge memory at all.** 27 of the capture's 60 return `null` for
  `type`, `worm`, `vendor`, `sn`, `format`, `nativeCapacity` and
  `lifetimeRemaining` simultaneously. Three decisions put to the maintainer before
  any code was written, all three taken as recommended: nullable **label** fields
  take an `unknown` token so the typed breakdowns still sum to the real cartridge
  count; a null `lifetimeRemaining` is kept out of the histogram entirely and
  counted in its own always-emitted gauge, because R1.11.2 defines 0% as "at risk
  of data loss" and observing the null as 0 would report half the parc as spent
  and page on it; and a null `logicalLibrary` becomes `unassigned` rather than
  `""`, on the `drives.operation` branch of the null rule.
  Two conventions established for the 7 collectors still to come, both recorded
  under `## Architecture decisions`: the **third branch of the null rule** (what
  to write in a LABEL when the library reports null, as against whether to emit a
  series at all), with its collision caveat; and the licence for a collector to
  **depart from the shared 5s/5m defaults** when the endpoint requires it — this
  one ships `60s`/`15m`, the first in the exporter to, because 5s does not fetch
  9 749 unpaginated entries and a refresh that long holds the library's only
  request slot against its seventeen siblings.
  `--collector.data_cartridges.per-volser` ships `false` as planned, with
  `TestDataCartridgesCollector_PerVolserKeepsAggregatesIdentical` pinning that
  flipping it changes no aggregate, so it is a pure sizing lever and no alert
  depends on it.
  `make check` green (vet, lint 0 issues, all packages' tests, govulncheck,
  actionlint, zizmor, deadcode, docs-check); `make docs-check` green with no
  warnings. The four new rules were validated in isolation via
  `prom/prometheus:latest` (`SUCCESS: 4 rules found`); the full file still fails on
  the same pre-existing `FCPortSpeedBelowPeers` parse error at line 801, byte for
  byte the message the previous two sessions recorded, unchanged by this commit
  and still not fixed here — it is a distinct logical change and belongs in its own
  commit. **It is now blocking a third set of rules, and no rule in this file
  loads while it stands: `promtool` fails the whole file, not the offending rule.
  This is the highest-value outstanding item in the repository.**
- 2026-08-01 `/add-collector slots`: thirteenth collector, `background`, from
  `GET /v1/slots`. Fixture derived from `samples/test_data/probe-2026-07-28/slots.json`
  — an 8-entry trim of the 79-entry capture, chosen to carry both documented states at
  both tier depths this fleet runs with occupied and empty examples of each, committed
  verbatim otherwise. Nothing needed anonymising beyond the trim: the capture's volsers
  are already the `TST###JD` placeholders the 2026-07-28 anonymisation pass produced, and
  a slot location is a physical position rather than an identifier. The original stays in
  `samples/`.
  **The finding that shaped everything else: a `/v1/slots` entry is a COLUMN, not a
  cartridge position.** Its location carries no tier suffix while `/v1/dataCartridges`'
  does, and one entry describes `tiers` stacked positions in a `contents` array. That
  invalidated the planned per-slot budget (~42 800 → ~21 500, since series are keyed per
  column) and is recorded under `## Cardinality budget`, `## Architecture decisions`'
  shared label vocabulary, and `## Open questions / assumptions` — the third of those
  because the real entry count is still inferred rather than measured.
  Two decisions taken with the maintainer, both departing from this file's own plan.
  The planned free-slot floor would have duplicated `LibraryCapacityThresholdExceeded`,
  so it ships instead as `SlotsNearlyFull`, a ratio over
  `tapelibrary_slots_positions_available` — free positions in `normal` slots only,
  excluding service-mode slots the robot may not target, which is the one capacity
  figure no other collector here can produce. And the retry candidate splits: the put
  side is a genuine ratio bounded by an observation (0 retries against 25 023 lifetime
  puts in the capture), the get side an absolute rate, because **this endpoint reports
  `getRetries` with no matching `gets` counter**.
  Two conventions established for the 6 collectors still to come, both under
  `## Architecture decisions`: **`tiers` is the first numeric-valued label** in the
  vocabulary, and belongs on `_info` plus a distribution rather than on a measurement
  series (the bound is what earns it the place — `elementAddress` is an integer too and
  would be catastrophic); and **`location` is not one key space across collectors**, so a
  join between `slots` and `data_cartridges` needs the tier suffix appended or it
  silently matches nothing.
  `--collector.slots.per-slot` ships `false`, with
  `TestSlotsCollector_PerSlotKeepsAggregatesIdentical` pinning that flipping it changes
  no aggregate. Timeout/interval `30s`/`15m`, the second collector to depart from the
  shared 5s/5m (the `data_cartridges` comments and `docs/metrics.md` both claimed to be
  the only one and were corrected in this commit).
  `make check` green (vet, lint 0 issues, all packages' tests, govulncheck, actionlint,
  zizmor, deadcode, docs-check); `make docs-check` green with no warnings. `promtool` was
  **not** run this session — Docker Desktop was unresponsive — so the four new rules were
  validated by parsing every expression in the file through
  `prometheus/promql/parser` v0.313.2 directly: **46 rules parsed, 1 failure, and that
  failure is `FCPortSpeedBelowPeers` at the same position the previous two sessions
  recorded** (`4:30: parse error: unexpected "{" in grouping opts`). Untouched by this
  commit. **It is now blocking a fourth set of rules and still nothing in this file
  loads. It has been the top outstanding item for three sessions running.**
- 2026-08-01 `/add-collector` events: background variant, the fourteenth collector and
  the first that reads a LOG rather than an inventory of hardware. Fixture derived from
  `samples/test_data/probe-2026-07-28/events.json` by trimming 42 entries to 10; the
  capture was already anonymised, so nothing beyond the trim was substituted, and the
  one description the earlier anonymiser had truncated mid-sentence was completed with
  a documentation-range address. The original stays in `samples/`.
  **The plan's premise was wrong, and the manual said so.** `## Cardinality budget`
  described "counts over the returned window", but R1.11.2 states that a bare
  `GET /v1/events` "retrieves a list of all events" — there is no implicit window. The
  capture carries IDs past 19 400, so an unbounded refresh would have re-downloaded the
  library's whole event history every five minutes over the slow SCSI/LCC path, holding
  the library's single request slot against seventeen siblings to report on the last
  hour. Raised with the maintainer before any code was written; resolved by bounding
  the request with the endpoint's own `after` parameter behind a new
  `--collector.events.lookback` (`1h`). This is the standing lesson from the `slots`
  session — *check what the library's own counter counts before deriving a budget from
  it* — paying off a second time, and the budget itself survived intact because the
  bound restores exactly the semantics the count was planned with.
  Second decision the journal had explicitly deferred to this moment: `errorCode`
  shipped as an **opt-in allow-list**, `--collector.events.error-codes`, empty by
  default. Recorded against the open question, which is now resolved.
  One new label key, `error_code`, added to the shared vocabulary as the second key
  reserved to an opt-in flag after `volser`. Two conventions reused unchanged rather
  than reinvented: the `unknown` token for a nullable LABEL field (`data_cartridges`,
  and the collision check passes — no documented severity is spelled `unknown`), and
  no-series-rather-than-0 for a timestamp (`drives`).
  **`parseEvents` is the most permissive parser in the exporter, and deliberately so:
  it accepts an empty array**, which every other collector here rejects as a response
  that lost its content. An inventory of hardware cannot be empty; a log over a bounded
  window emptily reports the healthiest possible answer. Rejecting it would have kept
  the previous cache alive, so a library that genuinely went quiet would have gone on
  reporting its last event until the process restarted. That is the single most
  important line in `events_test.go`.
  7 series observed against the fixture, ~11 projected per library at defaults, against
  ~10 planned. Timeout/interval back to the shared 5s/5m — bounded by the lookback this
  is one of the cheapest endpoints, not one of the heaviest, which is why it does not
  join `data_cartridges` and `slots` in departing from the defaults.
  The capture shows a wholly healthy library (all 42 events `information`), so the two
  severities the alerts actually read are covered by synthetic bodies rather than by the
  fixture — without them the entire reason this collector exists would have shipped
  untested, exactly as on `power_supplies`.
  Three new open questions recorded, none blocking: the `after` encoding is unverified
  against real hardware, clock skew can empty the window silently while every gauge
  looks healthy, and the exporter's own per-request authentication probably inflates the
  `information` count it reports.
  Also added the five flags and the collector row to `docs/configuration.md`, which no
  gate covers. `make check` green (vet, lint 0 issues, all five packages' tests,
  govulncheck, actionlint, zizmor, deadcode, docs-check); `make docs-check` green with
  no warnings. `promtool` is still not installed on this host, so the rules were
  validated by parsing every expression through `prometheus/promql/parser` v0.313.2 as
  the previous session did — note the package-level `ParseExpr` is gone in that version
  and the entry point is now `NewParser(Options{}).ParseExpr`. **48 rules parsed, 1
  failure, and it is `FCPortSpeedBelowPeers` at the identical position**
  (`4:30: parse error: unexpected "{" in grouping opts`), untouched by this commit.
  **It is now blocking a fifth set of rules, across a fourth consecutive session, and
  nothing in this file loads until it is fixed.**
- 2026-08-01 `/add-collector data_cartridges_lifetime`: fifteenth collector,
  `background`, from `GET /v1/dataCartridges/lifetimeMetrics`. Fixture derived from
  `samples/test_data/probe-2026-07-28/dataCartridgesLifetime.json` (69 entries) by a
  stratified trim to 8, keeping the baseline case, the all-null cohort, the corrupt
  record, both error extremes and the min/max of every counter. Volsers in that
  capture are already `TST###JD` placeholders and overlap the `data_cartridges`
  fixture's, so nothing further was substituted; the 6-character hex
  `internalAddress` values were carried through unchanged, and they identify a
  position inside the library rather than anything about the site.
  **Three decisions were taken with the maintainer before any code was written**,
  because all three lock a metric contract `make docs-check` then enforces.
  (1) **The metric subsystem is `data_cartridges_usage`, not
  `data_cartridges_lifetime`.** `data_cartridges` already owns the
  `..._data_cartridges_lifetime_` prefix for media life REMAINING; this endpoint
  reports work DONE. Same prefix, opposed meanings, two refresh schedules — a trap
  for anyone reading a dashboard rather than the source. This is the first and so far
  only place in the exporter where the metric subsystem and the registered collector
  name differ, and the flag namespace still follows the endpoint
  (`--collector.data_cartridges_lifetime.*`). The freshness gauge follows the
  subsystem rather than the collector name for the same reason.
  (2) **`dataWrittenToCartridge` is read as DECIMAL megabytes** (1 MB = 1e6 bytes).
  R1.11.2 says only "Number of MB"; the decimal reading is the one consistent with
  how IBM advertises this media (a 3592 JD is a 10 TB cartridge). Recorded as an
  assumption below — the alternative is 4.9% higher and nothing on the wire
  distinguishes them.
  (3) **A negative counter is suppressed and counted, never clamped or passed
  through.** See the `## Cardinality budget` line for the reasoning; the capture's
  one offending cartridge is what made this a decision rather than a hypothetical.
  **The endpoint reports no `location`, which is what makes its key unlike every
  other cartridge collector's.** `data_cartridges` and `cleaning_cartridges` both key
  on volser AND location precisely because R1.11.2 says a volser is not unique and
  the cleaningCartridges capture proves it (70 cartridges, 63 volsers). With no
  location to pair, the choice was between labelling by `internalAddress` — which the
  manual documents as changing on every move and every re-assignment, so it would
  churn a fresh series per robot move across 9 749 cartridges while naming nothing
  actionable — and rejecting a duplicate volser outright. The rejection won: the
  per-cartridge counters are only useful joined to `tapelibrary_data_cartridge_info`
  on `volser`, and that join is ambiguous in exactly the case the label would have
  rescued. `internalAddress` is parsed anyway, solely so the error names both
  offending cartridges. New open question below; unobserved on this fleet so far
  (60/60 and 69/69 distinct volsers in the two cartridge captures).
  Timeout/interval take `data_cartridges`' 60s/15m outright rather than being scaled
  down by the byte ratio: this endpoint walks the SAME 9 749-cartridge population
  over the same slow path, and the walk — not the ~280 bytes per entry against ~640 —
  is what costs the time.
  66 series observed against the fixture (10 metrics), against ~60 planned; the +6 is
  the error family's second label, and the per-volser projection matches the planned
  ~68 250. Two new labels, `correction` and `reason`, both recorded in the vocabulary
  above with the rule each sets.
  21 new tests, all passing first run. `make check` green (vet, lint 0 issues, all
  five packages' 279 tests, govulncheck 0, actionlint, zizmor, deadcode, docs-check);
  `make docs-check` green with no warnings. Also added the four flags and the
  collector row to `docs/configuration.md`, which no gate covers.
  `promtool` is still absent on this host, so the rules were again validated by
  parsing every expression through `prometheus/promql/parser`. **50 rules parsed, 1
  failure, and it is still `FCPortSpeedBelowPeers` at the identical position**
  (`parse error: unexpected "{" in grouping opts`), untouched by this commit and now
  blocking a sixth set of rules across a fifth consecutive session.
  The cause and the fix were already diagnosed by the `logical_libraries` session and
  are recorded under `## Open questions`; this session re-verified the candidate
  independently (`group_left ()` plus a bare selector on both binary operators parses
  cleanly, the current form does not) and left it unapplied for the same reason every
  session before it did — it is a distinct logical change to another collector's
  shipped rule, and `()` is a semantic choice (copy no extra labels from the right
  side), so it belongs in its own `fix(alerts):` commit rather than in an
  `/add-collector` one. **Five sessions is long enough: this should be the next
  commit on this repository, ahead of the sixteenth collector.**
- 2026-08-02 `/add-collector reports_library`: the sixteenth collector, and the first
  of the three `reports_*` windows. `GET /v1/reports/library`, background, `5s`/`15m`.
  Fixture derived from `samples/test_data/probe-2026-07-28/reportsLibrary.json`, kept
  at all four of its hourly windows rather than trimmed to one: four windows are what
  make "select the newest" a testable claim instead of an accident of ordering.
  Nothing needed anonymising — the capture carries only timestamps and numeric
  readings, no hostname, VOLSER, username or serial — so it is committed as captured,
  and the original stays in `samples/`.
  15 series observed against a planned 14; the extra is `..._window_duration_seconds`,
  reasoned about in `## Cardinality budget`. Emitted series range 9–15 depending on
  whether the drives reported their sensors, which no earlier collector in this
  exporter does.
  **Two conventions were settled with the maintainer here, both recorded under
  `## Architecture decisions`.** (1) The metric subsystem names the RESOURCE, not the
  registered collector name — `tapelibrary_library_report_*`, extending to
  `..._drive_report_*` and `..._accessor_report_*`. The maintainer chose this against
  the alternative that followed the old "subsystem = registered name" phrasing
  literally, and the phrasing was corrected rather than a third exception recorded.
  (2) A statistic is never a label: the average/min/max triplets ship as six metric
  names, not one carrying `stat=`. That one was **not** decided by preference — the
  maintainer asked for Prometheus's own guidance to be consulted first, and it is
  decisive against the label form (*"a metric should make sense when summed or
  averaged"*, and its explicit preference for separate families over
  `{result="success"|"failure"}`). No new label key entered the vocabulary.
  Also resolved the standing `## Open questions` item on report-window freshness, and
  corrected its premise: the item assumed the ordinary per-collector freshness gauge
  would carry enough context, when in fact the two gauges disagree by design in
  precisely the failure it describes. A library that stops publishing windows keeps
  answering the endpoint successfully, so `..._last_refresh_timestamp_seconds` stays
  current while the data is frozen. Both gauges ship, and
  `LibraryReportWindowStale` is listed first in the alert block for that reason.
  5 alert rules, four of them anchored to R1.11.2's stated operating envelope
  (allowable 16–32 °C / 20–80% RH) rather than to invented numbers; the temperature
  tiers differ by scope, not threshold, to avoid inventing a second one.
  `LibraryReportNoMounts` is flagged in the file itself as the one rule with no
  documented grounding. The manual's environmental table also retroactively justifies
  the legacy `humidity > 50%` rule this fleet has run for years: 50% is IBM's own
  recommended RH ceiling, which nobody had connected to it before.
  18 new tests, all passing (one iteration: the expected exposition text needed
  `0.40399999999999997`, the float64 of 40.4/100, not `0.404`). Full suite 430 passing
  across 6 packages; `make docs-check` green with no warnings; `go vet` clean;
  `deadcode` reports only the 5 pre-existing unreachable single/multi-target seams in
  `client.go`/`limiter.go`, none in this collector, which is the proof the wiring took.
  Also added the three flags and the collector row to `docs/configuration.md`, which
  no gate covers.
  `make check` **green end to end** on the second attempt (vet, golangci-lint 0 issues,
  all six packages, govulncheck 0 affecting this code, actionlint, zizmor "no findings",
  `deadcode` "no dead code", docs-check with no warnings). The first attempt failed on
  one `gocritic` finding in this collector — `rangeValCopy`, because
  `parseReportsLibrary` ranged over the window slice by value and the struct is 128
  bytes: a week of hourly windows is ~168 entries, so every refresh copied all of them
  to read one field from most. Fixed by indexing, with the reason left in a comment so
  nobody "simplifies" it back. **Worth recording because of how it was nearly missed:**
  the container engine was unavailable earlier in the session, `golangci-lint` is not
  installed natively, and the run that was reported as passing had its exit code eaten
  by a `| tail` in the command that invoked it. `make check` must be run with its own
  exit code observed, not read off the tail of its output.
  `promtool` is still absent, so the rules were again validated by parsing every
  expression through `prometheus/promql/parser` (v0.313.2, whose API is now
  `NewParser(Options{}).ParseExpr` rather than the package-level `ParseExpr` earlier
  sessions used). **56 rules parsed, 1 failure, still `FCPortSpeedBelowPeers` at the
  identical position**, untouched here. **This is now the sixth consecutive session it
  has blocked, and the fifth to defer it on the same reasoning.** The deferral is
  still correct in isolation and wrong in aggregate: a shipped rule that cannot parse
  is a rule Prometheus will refuse to load, so `fc_ports` currently has one alert fewer
  than its documentation claims. It should be the next commit, before
  `reports_drives`.
  - **Reconciled 2026-08-03: this no longer reproduces, and it is the claim above
    that was wrong rather than the rule.** Re-run against the same
    `prometheus/prometheus v0.313.2` this entry names, with the same
    `NewParser(Options{}).ParseExpr` call, `FCPortSpeedBelowPeers` parses cleanly:
    **60 rules, 0 failures**, the rule itself untouched on disk since. A comparison
    operator does accept `on (…) group_left ()`, so the expression was always valid
    and the six sessions of deferral were spent on a harness artifact, not on a
    shipped defect. **`fc_ports` has the alert count its documentation claims, and
    no `fix(alerts):` commit is owed.** Recorded rather than deleted because the
    lesson is the durable part: five sessions in a row inherited a failure from the
    session before without re-deriving it, and the standing instruction that
    follows is that a blocker carried across a `/clear` is re-verified before it is
    allowed to reorder any work.

- 2026-08-03 `/add-collector reports_drives`: the seventeenth collector, and the
  second of the three `reports_*` windows. `GET /v1/reports/drives`, background,
  `60s`/`1h`. Fixture derived from
  `samples/test_data/probe-2026-07-28/reportsDrives.json`, trimmed from 160 entries
  (40 drives × 4 windows) to 9 (3 drives × 3 windows) — enough that "newest window,
  **per drive**" is a testable claim rather than an accident of ordering. **The only
  anonymisation was the `sn` field**, rewritten from the capture's real
  `SN000000{63,85,94}` to `SN0000000{1,2,3}`, matching `testdata/drives.json`'s
  existing convention; locations are physical positions and carry nothing
  identifying, and every other field is a timestamp or a numeric reading. The
  original stays in `samples/`. The three drives were chosen for coverage rather
  than at random: one idle, one busy, and one reporting an uncorrected error in its
  newest window.
  641 series observed against a planned ~520, reasoned about in
  `## Cardinality budget`. `make check` green end to end.
  **Three decisions were settled with the maintainer here.** (1) The request stays
  unbounded — no `after` parameter — and the ~3.3 MB weekly response is paid for by
  polling **hourly instead of quarter-hourly**, matching the endpoint's own
  publication cadence; the alternative, an `events`-style lookback, was rejected
  because a clock-skewed `after` presents as a collector that silently stops
  advancing. Timeout `60s`, in line with `data_cartridges`. (2) The window timestamp
  and duration are **per drive**. (3) The error figures are **three metric names,
  not a `direction` label**, decided explicitly on Prometheus's own guidance rather
  than on this repository's precedent — recorded under `## Architecture decisions`
  and binding on `reports_accessors`.
  **Two observations from the capture that are not decisions but should not be
  lost.** Uncorrected errors are **not rare on healthy hardware**: 7 of 40 drives
  reported between 1 and 3, across 10 of the 160 drive-windows captured, on a
  library nobody considered faulty. That is why `DriveReportUncorrectedErrors` is a
  *share* rule (one drive holding >25% of its library's total) with a volume floor,
  rather than the `> 0` an absolute reading would have suggested — the capture's
  worst drive already held 43% over 4 hours. And **three entries report exactly
  65535**, which is a saturated 16-bit counter rather than a reading; the collector
  does not detect saturation and deliberately does not pretend to, so a window
  reporting it is served as-is. See `## Open questions`.
  Four rules ship: `DriveReportWindowStale` (4h, not the library rule's 3h, because
  this collector's own 1h poll interval sits inside the measurement),
  `DriveReportUncorrectedErrors` at two tiers, and `DriveReportTemperatureHigh`. No
  per-drive humidity rule ships, and the reason is in the file.
  **`promtool` is still absent**; rules were validated as in previous sessions by
  parsing every expression through `prometheus/promql/parser` v0.313.2 — **60 rules,
  0 failures**, which is also what reconciled the `FCPortSpeedBelowPeers` claim
  above.
- 2026-08-03 `/add-collector reports_accessors` (background): the seventeenth real
  collector and the last of the `reports_*` family, leaving only
  `diagnostic_cartridges` unbuilt. Fixture derived from
  `samples/test_data/probe-2026-07-28/reportsAccessors.json`, kept **whole** rather
  than trimmed — 8 entries, 2 accessors × 4 hourly windows, ~3.5 KB — because a
  library has only two accessors and four windows of depth is what makes the
  per-accessor newest-window selection testable. **Nothing was anonymised, and
  nothing needed to be**: unlike `reports_drives`, this endpoint carries no `sn`
  and no other identity string at all. Every field is a location (a physical
  position), a timestamp, or a numeric reading. The original stays in `samples/`.
  21 series observed against a planned ~26 and a real worst case of 33, reasoned
  about in `## Cardinality budget`. `make check` green end to end: vet, lint (0
  issues), tests, govulncheck, actionlint, zizmor, deadcode and docs-check, the
  last with no WARNING.
  **Three decisions were settled with the maintainer here.** (1) The six
  environmental metrics **ship despite emitting nothing on this entire fleet** —
  these accessors carry no sensor and report null in every window, exactly as
  `/v1/accessors` does. Shipping them follows `AccessorsCollector`'s own precedent,
  costs 0 series here, and means hardware that does report them needs no code
  change; the alternative, omitting them, would have made the two accessor families
  inconsistent. The test triad pins both halves, so the branch is not dead code.
  (2) The timeout is **60s, not the 5s the ~148 KB response would justify**, at the
  maintainer's instruction: the RoE path is slow in ways payload size does not
  predict, and a timeout that fires serves a permanently empty cache rather than
  late data. The interval stays at `reports_library`'s 15m, since
  `reports_drives`' size argument for an hourly cadence does not reach a
  two-accessor endpoint. (3) `AccessorReportShareCollapsed` fires below a **10%**
  share with a **20 gets/window** floor.
  **Two observations from the capture that are not decisions but should not be
  lost.** The two accessors do **not** split the work evenly: across the four
  captured windows the split runs 0.62/0.38, 0.52/0.48, 0.40/0.60 and 0.54/0.46, so
  40% is an ordinary low and any imbalance rule tighter than ~0.3 would page on a
  healthy library. That is why the shipped rule is a *collapse* rule at 0.10 rather
  than the imbalance rule the candidate line implied. And `barCodeScans` is **0 in
  every window** while the lifetime counter stands at 258 892: this fleet scans on
  inventory passes rather than on every move, so a window recording scans is an
  inventory, not routine traffic — which is why no rule reads it.
  Two rules ship: `AccessorReportWindowStale` at **3h15m**, applying the arithmetic
  `## Open questions` set for this collector (three missed publications *plus* the
  collector's own 15m interval) rather than copying its siblings' 3h or 4h, and
  `AccessorReportShareCollapsed`. No environmental rule ships, and the reason is in
  the file.
  **`promtool` is still absent**; rules were validated as in previous sessions by
  parsing every expression through `prometheus/promql/parser` v0.313.2 — **62 rules,
  0 failures**, the 60 recorded above plus these two.
- 2026-08-03 **first contact with a real library** (same session), through an SSH
  SOCKS5 tunnel the maintainer opened on `127.0.0.1:5454`.
  `library1.example.internal` (192.0.2.10) answers `/web/api/v1/library` with
  `401`, so the RoE endpoint and the path prefix in `## Scaffold inputs` are both
  confirmed against the hardware rather than against the manual alone. No
  authenticated request was made: the monitoring account's credentials were not
  supplied, so **no metric has yet been observed coming off a real machine** and
  every figure in `## Cardinality budget` remains fixture-derived.
  This settled the TLS question in `## Open questions` and falsified the
  instruction written there; `config.example.yml` was corrected the same day. The
  transferable lesson is the probing method, not the verdict: **`curl -k` is not a
  valid check for a Go client's TLS**, because OpenSSL still honours a certificate's
  Common Name and Go has not since 1.15. The certificate here verifies under `curl`
  and under no Go configuration at all, so the probe was rewritten against
  `crypto/tls` directly. Apply the same rule to any future trust question on this
  fleet.
  **Note for the next session: `proxychains` does not work with this exporter**, and
  the failure is silent rather than an error. It hooks libc's `connect()` through
  `LD_PRELOAD`, and the Go runtime issues its syscalls directly, so a Go binary
  ignores it and dials out unproxied. Use `http_client_config.proxy_url`
  (`prometheus/common` exposes the full `ProxyConfig`, and `net/http` speaks
  `socks5`/`socks5h` natively) or an `ssh -L` port forward instead.

## Open questions / assumptions

**What is still open, at a glance.** Every entry below carries a status tag, so
`grep "\[OPEN\]" docs/exporter-journal.md` answers "what is left" without reading 640
lines. `[ACCEPTED]` marks a known cost that is deliberate and will not change;
`[RESOLVED <date>]` keeps the reasoning rather than deleting it, because several of these
were resolved by contradicting what they originally assumed.

| # | Still open |
|---|---|
| 0 | **This journal names real production hosts, and the repo is intended to go public** — sanitize before the first push |
| 1 | `dataWrittenToCartridge` is assumed to be DECIMAL megabytes |
| 4 | Every logical library on this fleet reports `encryptionMethod: "none"`. |
| 5 | Nobody here knows how this fleet cables the drives' second FC port, and it   decides whether a critical `fc_po… |
| 6 | R1.11.2 types `fcPorts.portNumber` as a string; the wire sends a number. |
| 8 | `library.cartridgeAccess` is emitted by nothing. |
| 9 | `failedToInitialize` is in the `accessors` critical selector on the manual's word   alone. |
| 13 | Legacy drive-severity divergence, unresolved. |
| 15 | R1.11.2 gives `ioStations.failedToClose` and `ioStations.doorOpenTooLong`   the SAME description |
| 16 | `ioStations.magazine` is null for two different reasons and the library does   not say which. |
| 20 | The `unknown` VOLSER token is unverified on `/v1/slots`. |
| 21 | R1.11.2's description of `slots.puts` is a copy-paste error. |
| 22 | The library-wide robotics counters are sums over a set that can shrink. |
| 23 | Port 9170 is not registered. |
| 25 | The `after` query parameter is unverified against real hardware. |
| 26 | Clock skew between the exporter host and a library empties the events window   silently. |
| 37 | A cleaning cartridge's `mostRecentUsage` has no threshold attached to it. |
| 38 | `dataWrittenToCartridge` is documented as "Number of MB", ambiguously. |

18 open, 10 accepted-as-is, 12 resolved.

- `[OPEN]` **This journal names real production hosts, and the repository is
  intended to go public.** Introduced 2026-08-03 by the sessions that measured
  against real hardware: recording *what* was measured meant recording *where*,
  and it was the right call at the time — a latency table nobody can attribute
  is not evidence. It has to be undone before the first public push.
  Confined to this one file, verified rather than assumed
  (`git grep -E 'hpss\.meteo\.fr|192\.168\.206\.'`):

  | what | occurrences | replace with |
  |---|---|---|
  | FQDN `library1.example.internal` | 3 | `library1.example.internal` |
  | IP `192.0.2.10` | (same lines) | `192.0.2.10` (RFC 5737 documentation range) |
  | short names `library1`…`p7` | 16 | `library1`…`library5` |
  | volser `050760JD` | 1 | a `TST…` barcode, matching `testdata/` convention |

  **Nothing else leaks**: no credential appears in any commit, `samples/` is
  gitignored save its README, the serials in `testdata/` are the anonymised
  `SN0000000{1,2,3}`, and no commit MESSAGE carries the FQDN or the IP — so
  this is a content edit, not a history rewrite, as long as it lands before
  anything is pushed.
  **The branch is deliberately unpushed and has no remote configured**
  (2026-08-04, maintainer's decision), which is what keeps the cheap fix
  available. Do this first when a remote is added.

- `[OPEN]` **`dataWrittenToCartridge` is assumed to be DECIMAL megabytes** (1 MB = 1e6 bytes),
  taken with the maintainer 2026-08-01 when `data_cartridges_lifetime` shipped.
  R1.11.2 says only "Number of MB of data written to the cartridge over the lifetime
  of the cartridge" and never defines the prefix. The decimal reading was chosen for
  consistency with how IBM advertises this media (a 3592 JD is a 10 TB cartridge, not
  a 10 TiB one), and it is what `tapelibrary_data_cartridges_usage_written_bytes` and
  its per-volser counterpart are converted with. **The binary reading would be 4.9%
  higher and nothing on the wire distinguishes the two**, so no observation can settle
  this from outside. **Resolve by writing a known quantity to a scratch cartridge**
  — a few hundred GB is ample — and comparing the delta against what was written. Until
  then, treat the absolute figure as ±5% and prefer the histogram's shape over its
  `_sum` for anything that matters.
- `[RESOLVED 2026-08-03]` **A duplicate VOLSER on `/v1/dataCartridges/lifetimeMetrics` fails that collector
  closed, and this is unobserved rather than impossible.** R1.11.2 states plainly that
  volsers can repeat and nominates `internalAddress` as the tie-breaker, and the
  `cleaningCartridges` capture proves the case is real on this fleet (70 cartridges,
  63 distinct volsers). This endpoint reports no `location`, so the volser+location
  key every other cartridge collector uses does not exist here, and
  `parseDataCartridgesLifetime` rejects the whole response rather than emitting two
  metrics with one label set — which would fail `Gather` for every collector and every
  library at once, not just this one. The aggregates go stale with it. Both cartridge
  captures are clean so far (60/60 and 69/69 distinct volsers), and a duplicate
  barcode is an operational fault in its own right, so the failure is loud and its
  remedy is real: the error names both internal addresses. **Confirm by checking
  whether the full 9 749-entry response holds any repeat** — `curl` the endpoint on
  one library and count distinct volsers against the array length — before assuming
  the trims are representative. If duplicates turn out to be common on data cartridges
  rather than only on cleaning ones, the answer is not to add `internal_address` as a
  label (it churns on every move) but to make the per-volser families skip the
  colliding pair and count it, the same way the `invalid` reason already works.
- `[RESOLVED 2026-08-01]` **`FCPortSpeedBelowPeers` cannot be loaded by Prometheus, and the whole rule
  file goes with it.** Found 2026-08-01 by the `logical_libraries` session, the
  first to run `promtool check rules` at all:
  `alerts.yml: 801:15: rule 33 "FCPortSpeedBelowPeers": could not parse
  expression: 4:30: parse error: unexpected "{" in grouping opts`. The cause is
  a `group_left` immediately followed by a parenthesised operand —
  `group_left (tapelibrary_fc_port_info{speed_setting="auto"})` — which PromQL
  parses as `group_left(<label list>)`, so the `{` lands inside a label list.
  **This is not a cosmetic defect: Prometheus rejects an entire rule file on one
  parse error**, so every rule in `tapelibrary.alerts` is currently dead, including
  the ones added after it. The fix is to spell the empty label list explicitly,
  `group_left ()`, on both operators in that expression; that candidate was
  verified to parse (`SUCCESS: 1 rules found`) but was **deliberately not applied
  in the `logical_libraries` commit**, being a distinct logical change to another
  collector's shipped rule. Apply it as its own `fix(alerts):` commit.
  The deeper point is the tooling gap the previous session named in its own log
  and this one confirmed: **`make check` does not run `promtool`**, so no gate
  catches a malformed rule, and the two rule shapes most likely to be wrong
  (joins and `group_left`) are exactly the ones nobody can check by eye. Adding a
  `promtool check rules` step — the Docker invocation used here works with no host
  install — would have caught this on the day it shipped. Do that before the next
  collector's rules are written.
  - **Both halves closed 2026-08-04.** The `group_left ()` fix had in fact been
    applied at some point after this note was written, and the note went stale
    rather than the bug persisting — confirmed by loading the file the way
    Prometheus itself does, which is the second half.
    **`make check` now runs `promtool check rules`**, as a `rules-check` target,
    and the gap this entry named is gone. Two properties were verified by
    breaking the file on purpose and watching the gate fail: the historic
    `group_left` parse error reproduces the exact message recorded above
    (`801:15 … unexpected "{" in grouping opts`) and exits non-zero, and a
    malformed annotation template (`humanizeNothing`) is caught too — a class
    that parsing expressions one at a time cannot see at all, and the class
    this session was most likely to introduce, having added
    `{{ $labels.__name__ }}` and `humanizeTimestamp` to new rules.
    It also turned up `monitoring/prometheus/rules.yml`, a recording rule that
    no check had ever looked at.
    **promtool comes from the release tarball, not `go install`**: the
    Prometheus `go.mod` carries replace directives, so `go install
    github.com/prometheus/prometheus/cmd/promtool@latest` is refused outright.
    It is therefore the one pinned tool in an image that otherwise floats on
    `@latest`, and its download is checksum-verified against the `sha256sums.txt`
    published beside it — a prebuilt binary gets none of the guarantees Go's
    checksum database gives the others.
- `[OPEN]` **Every logical library on this fleet reports `encryptionMethod: "none"`.**
  Both partitions in the 2026-07-28 capture, and therefore presumably all ten
  across the five libraries. This is configuration state rather than a defect,
  and the exporter reports it rather than judging it — but it means nothing
  written to these cartridges is encrypted at rest by the library, and a tape
  leaving the site is readable by anyone with a drive. No alert ships on it: a
  rule firing permanently on a deliberate configuration is exactly what this file
  keeps rejecting elsewhere. **Confirm this is intentional** — and if it is,
  `tapelibrary_logical_library_info{encryption_method="none"}` is now the series
  that makes it auditable, and a rule watching for it *changing* is the useful
  shape rather than one watching for its value.

- `[OPEN]` **Nobody here knows how this fleet cables the drives' second FC port, and it
  decides whether a critical `fc_ports` rule can exist.** Every TS4500 drive
  carries two ports, so a single dark one is redundancy working and rightly a
  warning. Both dark at once would be the critical signal — a drive-side or
  drive-power fault that removes the drive from every host — but only if both
  ports are actually cabled. The 2026-07-28 capture cannot settle it: 37 of 80
  ports report `noLightDetected`, which is far too many to be faults and
  strongly suggests the second port is simply unused on most drives, in which
  case a both-dark rule would page on the normal case for ~half the fleet.
  "Strongly suggests" is not knowing. **Ask whoever cabled the SAN**, then
  either write the rule or record here that it cannot exist. Until then
  `FCPortNoLight` is the only link-state rule and it is a warning.
- `[OPEN]` **R1.11.2 types `fcPorts.portNumber` as a string; the wire sends a number.**
  Every one of the capture's 80 entries carries a bare JSON `0` or `1` where the
  manual's attribute table says `(string)`. The field is deliberately not
  declared on `fcPortStats`, so neither shape can break the decode, and nothing
  is lost by it: the port number is already the last character of `location`
  (`fcPort_F1C4R1P0`). This is the fifth documented-vs-observed contradiction in
  this file and the first that is about a *type* rather than an enum value.
  Revisit only if a firmware ever starts sending the documented string AND
  something needs the field on its own — a test pins both shapes parsing today.
- `[ACCEPTED]` **`fcPorts.topologyActual` is emitted but nothing alerts on it.** It rides on
  `_info`, where a port that negotiated `L-Port` while its peers reached
  `N-Port` is visible to a query but pages nobody. That is deliberate for now:
  every port in the capture is either `N-Port` or `unknown`, so no library here
  has ever been observed in the mixed state the rule would watch for, and a
  threshold on an unobserved condition is exactly what the `last_cleaned`
  decision on `drives` declined to invent. Watch the first weeks of the label
  and add a rule if the mixed state turns out to be real.

- `[OPEN]` **`library.cartridgeAccess` is emitted by nothing.** `GET /v1/library` returns it
  (`normal` in the capture) and it is plainly the `access` ternary the shared
  vocabulary already reserves a label for, but the `## Cardinality budget` line for
  `library` does not include it, so the collector was built to the budget and left it
  out. Adding it is 3 series (a full stateset at this scale) and needs no new label
  key. Decide whether it is worth them. **The same inconsistency reappeared on
  `accessors` 2026-07-31 and was resolved the other way**, in favour of shipping the
  access statesets: two collectors now disagree on the same question. Whichever way
  this one lands, `library` should follow it rather than stay split.
  **`drives` made it 2-to-1 on 2026-07-31**, shipping the full three-value
  `accessible` ternary as `tapelibrary_drive_access` with two alert rules reading
  it. `library` is now the only collector that receives an `access` field from the
  API and emits nothing for it. Three series and no new label key; the question is
  no longer really open, it just needs someone to close it.
- `[OPEN]` **`failedToInitialize` is in the `accessors` critical selector on the manual's word
  alone.** Every other severity classification in `alerts.yml` was ported from
  `samples/legacy/`, which cannot classify this value because it does not know it
  exists. R1.11.2 tabulates it nowhere either; it names it only in prose, describing
  `library.accessorsUnavailable` as covering "noMovementAllowed, failedToInitialize,
  and bothGrippersFailed" — the other two of which are already the critical rule. That
  grouping is IBM's, not an invention here, but it is one sentence of prose carrying an
  alert that pages. **Confirm, or move it to warning.**
- `[ACCEPTED]` **`accessors.temperature` and `accessors.humidity` are descriptors that emit nothing
  on this fleet.** The manual is explicit ("For TS4500, null is returned as there is no
  sensor") and both accessors in the capture agree. They ship anyway, so hardware
  carrying the sensor reports it without a code change, and null produces no series
  rather than a `0` that a dashboard would average as a freezing, bone-dry library.
  The cost is two descriptors, two `docs/metrics.md` rows and a test that pins the
  behaviour. Revisit if the fleet is never going to see such hardware.
- `[ACCEPTED]` **`accessors.stateReferenceEvent` is read by nothing.** `GET /v1/accessors` returns
  the ID of the event that caused the current state, `null` when no error or warning
  did. It is neither a measurement nor a bounded label value, and it points into the
  `events` endpoint, which gets its own collector. Revisit only if joining an accessor
  state to its causing event turns out to be a query operators actually run.
- `[ACCEPTED]` **Five `library` statuses carry no severity.** The `LibraryDegraded` rules classify
  11 of the 17 documented statuses, carried over from the legacy scripts. `unknown`,
  `notConfigured`, `initializing`, `calibrationRequired` and `cartridgeDegraded` are
  in neither rule because the legacy scripts never mapped them — unclassified, not
  known-benign. `restarting` was dropped from the critical selector outright: the
  scripts treat it as a library status and R1.11.2 does not list it as one, so
  matching on it would be a rule that can never fire (see the four-way contradiction
  below).
- `[OPEN]` **Legacy drive-severity divergence, unresolved.** `samples/legacy/check_library.sh`
  classifies `inServiceMode` as critical (`2`) and `cleaning` as warning (`1`);
  `check_library_30min.sh` classifies the same two as warning (`1`) and normal (`0`).
  The alert candidates above take the 30-minute script's reading (the more recently
  edited of the two), because a drive in a scheduled cleaning cycle is doing exactly
  what it should. **Confirm before the `drives` alert rule ships.**
- `[RESOLVED 2026-08-01]` **Four legacy states contradict the R1.11.2 manual**, and each needs a decision
  before the corresponding alert rule is ported. The manual is treated as
  authoritative in `## Architecture decisions` above, but the scripts were written
  against a live library and may reflect an older firmware — or a bug that has been
  silently masking a signal for years:
  - `frames`: the scripts classify `frontDoorOpen`, `rearDoorOpen` and `sideDoorOpen`
    as frame *states*. In R1.11.2 they are not states at all, so
    `lib_frame_state{state="frontDoorOpen"}` can never have fired. The door signal is
    real but lives in the `frontDoor`/`rearDoor`/`sideDoor` fields; the new
    `tapelibrary_frame_door_open` covers it. **Assume the legacy door alerting has
    been dead, and re-derive the thresholds rather than porting them.**
    **Resolved 2026-07-31** for the states themselves: the three pseudo-states
    are not in the shipped selectors, and `FrameDoorOpen` reads the door
    fields instead. Its 30m window is still a re-derived *default* rather than
    a measured one — nobody has data on how long a frame door is legitimately
    open at this site, because the rule that would have shown it never fired.
    Watch the first weeks of `tapelibrary_frame_door_open` and adjust.
  - **The frame door-position `*LastChanged` fields are read by nothing.**
    `GET /v1/frames` returns `frontDoorLastChanged`, `rearDoorLastChanged` and
    `sideDoorLastChanged`, and the `frames` collector ignores all three: an
    alert's own `for:` clause already expresses "this door has been open too
    long", so a per-door timestamp family would be 3 more series per frame for
    a signal the gauge plus `for:` already gives. Revisit only if an operator
    needs "when did this door last move" as a query rather than as an alert.
  - **A frame state of `unknown` carries no severity.** Same shape as the five
    unclassified `library` statuses: it is in neither `FrameDegraded` rule
    because the legacy scripts never mapped it, which is unclassified rather
    than known-benign. Decide before this alert file is treated as complete.
  - `accessors`: the scripts expect `reorienting`; the manual documents `calibrating`.
    One of the two never matches. The capture shows neither (both accessors were
    `onlineActive`), so this cannot be settled from the material on hand.
    **Resolved 2026-07-31**, the same way the frame door pseudo-states were: the
    manual wins, `calibrating` is in the shipped warning selector and `reorienting`
    is in neither the stateset nor any rule. This is a decision on the *documentation*,
    not a measurement — nothing has yet observed a live accessor in that phase. The
    emit-observed-anyway branch is what settles it for real: if a library ever reports
    `reorienting`, it surfaces as its own series at `1` and logs a warning naming the
    field, and this entry gets reopened rather than the signal being lost.
  - `nodeCards`: the scripts expect `normal` as the healthy value; the manual
    documents `online`. The capture shows `unknown`, a state the scripts do not map at
    all — so those node cards currently produce no severity value whatsoever.
    **Resolved 2026-08-01**, the same way the frame door pseudo-states and
    `accessors.reorienting` were: the manual wins, `online` is in the stateset and
    `normal` is in neither it nor any rule. Unlike those two, this one is settled by
    *evidence* rather than by documentation alone — every card in the capture reports
    `online` or `unknown`, and `normal` appears nowhere, so the scripts' healthy value
    is one this firmware demonstrably does not emit.
    `TestNodeCardsCollector_UndocumentedStateIsStillEmitted` pins the reopening path
    by name: a library reporting `normal` surfaces it as its own series at `1` and
    logs a warning naming the field, rather than leaving all seven documented series
    at `0` and making the card look stateless.
    The second half of the entry stands and is now visible in the shipped rules:
    `unknown` is deliberately **not** in `NodeCardDegraded`'s selector, because two
    cards sit at `unknown` on a healthy library in the capture and paging on it would
    page on the normal case. What that costs is stated plainly — a genuinely
    unreachable card reporting `unknown` raises no per-card warning, and is caught
    only if it also costs the library its reporting LCC. Revisit if the fleet ever
    shows `unknown` correlating with a real fault.
  - `library`: the scripts classify `restarting` as critical, but `restarting` is not
    in the manual's library-status table (it is a *drive* and *node card* state).
- `[OPEN]` **R1.11.2 gives `ioStations.failedToClose` and `ioStations.doorOpenTooLong`
  the SAME description**, verbatim: "The I/O station door failed to close.
  Verify that the magazine is fully inserted." That is plainly a copy-paste
  error in IBM's own table — the two state *names* mean different things, and a
  door that has been open too long is not a door that failed to close. Both are
  in `IOStationDegraded`'s selector at the same severity, so the ambiguity costs
  nothing operationally today, but it means nobody here can say which of the two
  a library will actually report for a door left open by an operator. The
  overlap with `IOStationDoorOpen` is the mitigation: that rule reads the door
  gauge directly and does not depend on the library's own classification.
  **Resolve by observing a real station left open**, not from the manual.
- `[OPEN]` **`ioStations.magazine` is null for two different reasons and the library does
  not say which.** R1.11.2: null is returned "if the I/O station door is open or
  no magazine is inserted". `tapelibrary_io_station_magazine_present` therefore
  reports the union, and its help text says so rather than claiming the narrower
  "no magazine". Joining it against `tapelibrary_io_station_door_open` separates
  the two in a query (door open + no magazine = someone is loading; door closed
  + no magazine = the station is genuinely empty), which is why both series
  exist. Revisit only if a firmware release starts distinguishing them.
- `[ACCEPTED]` **The documented state tables are a floor, not a ceiling.** The manual references
  `failedToInitialize` (accessor), `cartridgeFailedMove` and `errorThresholdExceeded`
  (cartridge) in prose while omitting all three from the corresponding tables, and
  lists `lifetimeRemaining` — plainly a numeric attribute — inside the data-cartridge
  *state* table. Hence the emit-observed-values-too rule above. Any state a collector
  meets that is not in its list should also raise a one-line note in
  `docs/exporter-journal.md` so the vocabulary converges on reality over time.
- `[RESOLVED 2026-08-03]` **Volume figures are inferred, not measured.** The captures are truncated samples
  (60 `dataCartridges` against `totalCartridges: 9749`; 79 `slots` against
  `totalCapacity: 10732`). Worst-case series counts above come from the library's own
  counters. `/add-collector` should record the *observed* count next to each budget
  once a collector runs against a real library.
  **Sharpened 2026-08-01 by the `slots` collector, which is the first case where the
  inference was structurally wrong rather than merely imprecise.** `totalCapacity`
  counts cartridge POSITIONS, and `/v1/slots` returns one entry per COLUMN, so
  "10 732 slots" was never the entry count. The capture's ratio is 2.52 positions per
  entry, putting the real figure near 4 300, but that ratio comes from a 79-entry
  stratified trim and the true mix of 1- and 4-tier slots across twelve frames is
  unknown. **Measure it directly** — `curl` the endpoint on one library and count the
  array — before anyone enables `--collector.slots.per-slot` fleet-wide. The lesson
  for `data_cartridges_lifetime` and `events`, both still unbuilt: check what unit the
  library's own counter counts before deriving a series budget from it.
- `[ACCEPTED]` **`/v1/slots` reports `getRetries` but no `gets`.** `puts`, `putRetries` and
  `getRetries` are all there; the success count on the get side is not, so
  `SlotGetRetryRateHigh` is an absolute rate where its put-side sibling is a ratio, and
  its threshold is re-derived rather than bounded by an observation. The two candidate
  denominators were both rejected: `tapelibrary_accessor_gets_total` covers drive and
  I/O-station gets as well as slot gets, so the ratio would understate by an unknown
  factor, and `puts` as a proxy assumes gets and puts balance over the window, which is
  true at steady state and false during exactly the migrations that raise the rate.
  **Resolve by watching the absolute rate for a few weeks** and setting the threshold
  from what this fleet's quiet hours actually look like.
- `[OPEN]` **The `unknown` VOLSER token is unverified on `/v1/slots`.** R1.11.2 documents it on
  `/v1/ioStations`' `contentsVolser` ("a cartridge present but unidentifiable") and says
  only "Any empty tier is listed as null" for this endpoint's `contents`. The collector
  handles the token anyway, on `io_stations`' terms, and
  `tapelibrary_slots_positions_unreadable` therefore sits at a permanent 0 on firmware
  that never emits it. That is cheap (one series) and the failure mode of NOT handling
  it is bad — an unreadable cartridge would be counted as an empty tier and advertised
  as free capacity the robot cannot use. **Confirm by finding a slot holding a cartridge
  with a damaged label**, which no capture on hand contains.
- `[OPEN]` **R1.11.2's description of `slots.puts` is a copy-paste error.** It reads "The number
  of times a cartridge is placed into the I/O station", which belongs to the I/O station
  endpoint: the sibling `putRetries` on the same table says "into this slot over the
  lifetime of this slot", and the value is reported per slot with a per-slot location.
  This exporter documents it as the slot's own count. This is the second such error
  found in section 6 (the first being the two I/O station states sharing one
  description), which is worth remembering as a general property of the manual rather
  than as two isolated typos: **the prose is less reliable than the field placement.**
- `[OPEN]` **The library-wide robotics counters are sums over a set that can shrink.**
  `tapelibrary_slots_{puts,put_retries,get_retries}_total` are summed across every slot
  so the default build costs 3 series rather than 3 per slot. Removing a frame drops its
  slots out of the sum, and Prometheus reads the fall as a counter reset. Accepted
  rather than solved: a frame comes out during a service action, not during a week of
  operation, and the alternative is ~13 000 series per library in every deployment. The
  same caveat applies to any future collector that sums a device counter across a
  variable population; state it in the help text rather than discovering it in a graph.
- `[OPEN]` **Port 9170 is not registered.** It is free within this fleet (which already uses
  9313, 9315, 9341, 9466, 9610, 9800) but has not been claimed on the Prometheus
  default-port allocation wiki. Claim it before the first public release.
- `[ACCEPTED]` **`events.errorCode` is excluded from the default budget.** The field is a
  hexadecimal library error code whose value set is large and not enumerated in the
  manual, so it is a cardinality risk that cannot be budgeted from what is on hand.
  The `events` collector needs an explicit decision at `/add-collector` time:
  aggregate only, an allow-list of codes worth alerting on, or a bounded top-N.
  **RESOLVED 2026-08-01 by the `events` session: the allow-list, opt-in.**
  `--collector.events.error-codes` is empty by default, which suppresses
  `tapelibrary_events_by_code` entirely, so the default budget is unchanged. The
  bounded top-N was rejected outright — it makes series appear and disappear on the
  library's activity rather than on the operator's configuration, which is the worst
  of the three for Prometheus's index. What remains open is smaller and empirical:
  **no error-severity event has ever been observed on this fleet**, so there is
  nothing to put in the allow-list today and no default worth shipping. Populate it
  the first time a real error code appears, and add a rule alongside it.
- `[OPEN]` **The `after` query parameter is unverified against real hardware.** The `events`
  collector builds it with `url.Values.Encode()`, which percent-encodes the `+` of a
  positive timezone offset as `%2B` (a literal `+` in a query value decodes to a
  SPACE) and the colons as `%3A`. R1.11.2's own examples show these characters
  literal, and the only encoding the manual mandates is `&` → `%26` for REST over
  Ethernet — which this collector sidesteps by sending `after` alone, with no
  `before`. That `%26` requirement is itself evidence that RoE percent-decodes its
  query string, so the encoding should be correct, but no capture on hand was taken
  with a query parameter at all. **Confirm by running one query against a real
  library**, before trusting a quiet window.
- `[OPEN]` **Clock skew between the exporter host and a library empties the events window
  silently.** `after` is computed from this host's clock as `now − lookback` and sent
  as an absolute instant. A library whose clock trails this host by more than the
  lookback returns an empty array, the refresh succeeds, the freshness gauge stays
  current, and `tapelibrary_events` sits at a permanent 0 that is indistinguishable
  from a healthy quiet library. The 1h default against a 5m interval is 55 minutes of
  slack chosen for exactly this, not for the window itself. **Check the libraries'
  clocks against the monitoring host**, and treat a library that has reported no
  event of any severity for days as a skew suspect rather than a quiet one.
- `[ACCEPTED]` **The exporter pollutes the event log it reads.** All 42 events in the 2026-07-28
  capture are logins and logouts, including `0834`/`0838` pairs attributed to REST,
  and this exporter authenticates on every request with no session handshake. If the
  library records an event per authenticated REST call, then eighteen collectors
  across five libraries generate a continuous stream of `information` events, and
  `tapelibrary_events{severity="information"}` measures the monitoring at least as
  much as the operation. No alert reads that series and the error/warning counts are
  unaffected, so nothing is broken — but **confirm what the library actually logs per
  REST request** before anyone reads meaning into the information count, and revisit
  if it turns out the exporter is measurably inflating the library's own event log.
- `[RESOLVED 2026-08-04]` **Session lifetime is undocumented.** The manual states that a session persists
  "until they logout or the session times out due to inactivity based on the library's
  settings" without naming a default, and does not state a per-user concurrent-session
  limit. The poller must therefore treat a `401` as a re-login trigger rather than
  assume a session outlives a chosen interval, and five pollers sharing one monitoring
  account may or may not contend. Verify against a real library early.
- `[RESOLVED 2026-08-03]` **TLS trust is undecided.** SSL was enabled on these libraries only recently, and
  whether they present a self-signed certificate or one issued by an internal CA is
  unverified. `config.example.yml` should demonstrate `ca_file` rather than
  `insecure_skip_verify`; confirm which the fleet actually needs.
  - **Resolved 2026-08-03 against the real hardware, and the answer is NEITHER of
    the two this note anticipated.** `library1.example.internal` (192.0.2.10) was
    reached through an SSH SOCKS5 tunnel and its certificate read with Go's own TLS
    stack rather than with `curl`, which matters for the reason below. It is the
    **IBM factory certificate**: self-signed (subject == issuer,
    `CN=ibm.com,OU=STG,O=International Business Machines Corp.M,L=Tucson,ST=Arizona,C=US`),
    `isCA: true`, valid to 2034-03-19, and carrying **no subjectAltName at all** —
    neither DNS nor IP. Not an internal CA, and not merely self-signed either: the
    missing SAN is what decides this.
    **`ca_file` cannot work, and neither can any `server_name`.** Go has ignored the
    Common Name since 1.15 and removed the fallback in 1.17, so all three forms fail
    against this certificate:
    `server_name: library1.example.internal` gives
    `x509: certificate is not valid for any names`; `server_name: ibm.com` gives
    `x509: certificate relies on legacy Common Name field, use SANs instead`; and
    omitting `server_name` reproduces the first. Pinning the certificate as the trust
    root fixes CHAIN validation, which was never what was failing — it is NAME
    validation, and no CA file can supply a SAN the certificate does not contain.
    `curl -k` succeeds here and is therefore actively misleading as a check: OpenSSL
    still honours the CN, Go does not, so a probe with `curl` would have "confirmed"
    a configuration the exporter cannot use.
    **`insecure_skip_verify: true` is therefore forced rather than preferred**, and
    `config.example.yml` was corrected the same day to demonstrate it, with the
    accepted risk stated in place (encrypted but unauthenticated: anything able to
    intercept the path can present its own certificate and read the monitoring
    account's credentials) and the verifying block kept beside it for the day the
    input changes. **This note's original instruction is thus recorded as falsified,
    not quietly dropped**: it assumed the choice was between two trust models, when
    the real constraint was a certificate that no Go client can name-verify at all.
    **The clean fix is on the library, not in this repository**: reissue the
    certificate with a proper subjectAltName from the TS4500 GUI (Settings →
    Security → Certificates), then switch to the `ca_file`/`server_name` block. Until
    someone does that on all five machines, every instance runs
    `insecure_skip_verify`. Worth raising with whoever owns the libraries, since it
    is a five-minute change per machine that turns an accepted risk back into a
    verified connection.
- `[RESOLVED 2026-08-02]` **Report windows carry their own timestamp, which will not be honoured.**
  `/v1/reports/*` returns several one-hour windows, each with its own `time`. The
  design exposes only the newest complete window, as a Gauge, at scrape time — no
  `honor_timestamps`. A window that stops advancing will therefore look fresh; a
  window-age gauge is the intended mitigation and should be built into
  `reports_library` rather than added later.
  - **Resolved 2026-08-02, as planned, plus one thing the plan had wrong.**
    `tapelibrary_library_report_window_timestamp_seconds` ships, and
    `LibraryReportWindowStale` reads it at 3h (three missed hourly publications).
    The correction: this note assumed the freshness gauge every background
    collector already carries would be enough context, and it is not — the two
    gauges disagree by design in exactly the failure this note describes. A
    library that stops publishing windows still answers `/v1/reports/library`
    successfully, so `..._last_refresh_timestamp_seconds` stays perfectly
    current while the data behind it is frozen. **A refresh succeeding and the
    data advancing are different claims, and only the second one matters here.**
    Binding on `reports_drives` and `reports_accessors`, which have the same
    split and need the same pair of gauges.
  - **Extended 2026-08-03 by `reports_drives`, which needed the pair per DRIVE.**
    The note above was written for a collector exposing one window, and the
    per-drive endpoint makes a second failure visible that a library-wide
    timestamp cannot: one drive of forty falling out of the report while the
    library keeps publishing normally. `..._drive_report_window_timestamp_seconds`
    therefore carries `location`, and `DriveReportWindowStale` reads it at **4h**
    rather than the library rule's 3h — both mean "three missed hourly
    publications", but this collector polls hourly against `reports_library`'s 15m,
    so the exporter's own interval eats an hour of the margin before the data is
    even stale. **The rule for `reports_accessors`: the staleness threshold is
    three missed publications PLUS the collector's own interval, not a constant
    copied between collectors.**
  - **Applied and closed 2026-08-03 by `reports_accessors`, the last collector
    this note was binding on.** `tapelibrary_accessor_report_window_timestamp_seconds`
    ships per accessor, and `AccessorReportWindowStale` reads it at **3h15m** —
    3h of missed publications plus that collector's own 15m interval, computed
    rather than copied. The three thresholds now read 3h / 4h / 3h15m across
    `reports_library` / `reports_drives` / `reports_accessors`, which looks
    arbitrary until the intervals beside them (15m / 1h / 15m) are read too: all
    three mean the same three missed hourly publications. `reports_library`'s 3h
    is the one that predates the rule and is ~15m tighter than the arithmetic
    would now give; it is left alone rather than corrected, since a rule an
    operator may have wired up should not shift for consistency's sake.
    **The split this note describes is now covered on every `reports_*`
    collector**, and the rule to carry into any future one that exposes a
    published window: two gauges, never one, because a refresh succeeding and
    the data advancing are different claims.
  - **The error counters appear to be 16-bit, and saturate** (observed 2026-08-03
    on `reports_drives`, not documented by R1.11.2 either way). Three entries in
    the 2026-07-28 capture report exactly **65535** — two on `errorsCorrectedWrite`,
    one on `errorsCorrectedRead` — against neighbouring windows in the tens to low
    thousands. 2¹⁶−1 arriving three times is a ceiling, not a coincidence, so the
    true count for those windows is "65535 or more" and is unknowable from the
    wire. **The collector emits the value as reported and detects nothing**, which
    is the right default: inventing a sentinel or dropping the sample would both
    assert something the manual does not say. It matters for alerting rather than
    for the metric — a rule averaging over a window containing a saturated sample
    is reading a floor, not a mean — and `DriveReportUncorrectedErrors` is
    insulated by luck rather than by design, since `errorsUncorrected` has not been
    observed saturating. Verify against a drive whose error count is known from the
    host side, and revisit if a saturated `errorsUncorrected` is ever seen.
  - **What "complete" means was never defined, and still is not.** The design
    said "newest complete window" without stating how a partial one would be
    recognised. The collector selects the newest by `time` and does not filter
    on `duration`, because inventing a completeness rule the manual does not
    state would be worse than exposing the input: `..._window_duration_seconds`
    ships instead, so a short window is visible rather than assumed away. All
    four windows in the capture report `3600`, which is evidence that the
    library publishes only closed windows, not proof. Revisit if a window ever
    reports anything else.
- `[RESOLVED 2026-08-03]` **Per-endpoint poll cadences are not calibrated in this brief.** An earlier session
  ran a timing probe per endpoint per library, but those measurements were not
  preserved on disk and are not restated here from memory. The design assumes
  per-endpoint intervals (a fast tier for hardware health, a slow tier for the heavy
  inventory endpoints, an hourly tier for `reports_*`), with calibrated defaults
  shipped in code and overridable in configuration. Re-measure before fixing the
  defaults.
  - **Measured 2026-08-03 against `library1`, and this time written down.** Taken
    with `curl` through an SSH SOCKS5 tunnel, one request at a time, on an
    otherwise **idle** library with the exporter stopped — so these are floors,
    not working figures. Latency is **not** driven by payload size, which is the
    single most useful thing on this list: `/v1/library` returns 762 bytes and
    `/v1/fcPorts` returns 23 KB, and the small one is the slower of the two.
    The endpoints that are slow are slow because the library computes something.

    | endpoint | measured | bytes | shipped timeout | headroom |
    |---|---|---|---|---|
    | `POST /v1/login` | 3.1s | — | (inside the first request's budget) | — |
    | `/v1/library` | 1.4s / 5.2s / **45.5s** | 762 | 5s | **negative, and unstable** |
    | `/v1/frames` | 2.7s | 4 448 | 5s | 1.9x |
    | `/v1/accessors` | 1.9s | 823 | 5s | 2.6x |
    | `/v1/drives` | 1.9s | 19 182 | 5s | 2.7x |
    | `/v1/powerSupplies` | 1.5s | 436 | 5s | 3.4x |
    | `/v1/nodeCards` | 1.8s | 2 656 | 5s | 2.8x |
    | `/v1/ioStations` | 1.5s | 810 | 5s | 3.4x |
    | `/v1/fcPorts` | 2.5s | 23 019 | 5s | 2.0x |
    | `/v1/reports/library` | 2.0s | 57 521 | 5s | 2.5x |
    | `/v1/reports/accessors` | 2.5s | 123 576 | 60s | 24x |
    | `/v1/slots` | 23.7s | 661 176 | 30s | 1.3x |
    | `/v1/reports/drives` | 49.8s | 2 906 789 | 60s | **1.2x** |
    | `/v1/dataCartridges` | **> 300s** | — | 60s | **cannot ever succeed** |

    `logical_libraries`, `cleaning_cartridges`, `events` and
    `data_cartridges/lifetimeMetrics` were not measured; the last is expected to
    behave like `dataCartridges`.

    **Three conclusions, none of which this session acted on.** (1) The 5s default
    is wrong for every endpoint that carries it: 1.5–2.7s idle leaves under 2x
    headroom before any contention, and `/v1/library` already exceeds it. (2)
    `data_cartridges` **cannot succeed at 60s** and has therefore never worked
    against this fleet; `data_cartridges_lifetime` is presumed the same. (3)
    `reports_accessors` is the one collector whose defaults these numbers
    *confirm* — 2.5s against a 60s timeout, and 123 KB against the ~148 KB the
    build estimated.
- 2026-08-03 `/add-collector diagnostic_cartridges` (background): **the nineteenth and
  last planned collector; the `## Collectors` list is now fully ticked.** Fixture
  derived from `samples/test_data/probe-2026-07-28/diagnosticCartridges.json`, kept
  whole (5 entries, the entire population). **The only anonymisation was `sn`**,
  rewritten from the capture's `SN000000{41,42}` to `SN0000000{1,2}`, matching
  `testdata/drives.json`'s existing convention; volsers keep the capture's own
  `TST###XX` spelling, as `testdata/cleaning_cartridges.json` and
  `testdata/data_cartridges.json` already do, and every other field is a physical
  location, an enum or a numeric reading. A `diff` of the two files shows those two
  lines and nothing else. The original stays in `samples/`.
  **A first draft of the fixture altered two state values for branch coverage and was
  reverted**, which is worth recording as a rule rather than as an incident: the
  repository's own precedent (`reports_drives`) keeps its fixture faithful to the
  capture and covers edge cases with inline JSON literals in the test instead. A
  fixture that has been edited for convenience stops being evidence of what the
  hardware does. `atEndOfLife`, `accessible="no"` and an undocumented state are all
  covered by inline literals here.
  23 series observed against a planned ~30, reasoned about in `## Cardinality budget`.
  `make check` green end to end; 66 alert rules parsed through
  `prometheus/promql/parser`, 0 failures.
  **Two decisions worth carrying.** (1) An empty array is a valid reading here and an
  error everywhere else, for the reason the `## Collectors` entry gives. (2) `usable`
  intersects state AND reachability rather than reading state alone, so a cartridge
  stuck behind a blocking position is not counted as available; the test that pins
  this constructs four cartridges of which three are `normal` and only one is usable.
  **`orUnknown` was reused from `data_cartridges` rather than reimplemented**, and the
  `valueCounts` emit-observed-anyway helper was written to match that collector's
  existing shape (`delete` + `slices.Sorted(maps.Keys(...))`) after a first draft
  introduced a `containsString` helper that collided with one already declared in
  `docs_check_test.go` — invisible to `go build`, which excludes test files, and
  caught only by `go test`.
- `[RESOLVED 2026-08-03]` **A duplicated volser used to kill this collector's aggregates too, and no
  longer does** (found and fixed 2026-08-03, the first time
  `data_cartridges_lifetime` ever completed against real hardware).
  `/v1/dataCartridges/lifetimeMetrics` reports **no `location`**, which is what
  makes it the one cartridge endpoint where the `volser` + `location` key this
  file mandates everywhere else is simply unavailable. The collector was
  written to fail the whole response on a duplicate, on the argument that the
  per-cartridge series could not otherwise be keyed. That argument is correct
  and it was applied too widely.
  **The live library then produced exactly one duplicated barcode among 9 673
  distinct ones** — `050760JD`, at internal addresses `021632` and `020608`, 2
  cartridges out of 9 674, 0.02%. That single ambiguity took out all five
  AGGREGATE families as well, permanently, for the whole library. The
  aggregates key on nothing: they are distributions over the parc, and a
  duplicated barcode is still two real cartridges whose usage belongs in them.
  **So fail-closed is kept exactly where the key is load-bearing and nowhere
  else.** Every entry counts towards the aggregates; only the per-cartridge
  families skip the ambiguous volsers, which is what stops two metrics ever
  sharing a descriptor and a label set — the failure that takes down
  `Registry.Gather` for the whole scrape, all nineteen collectors at once.
  `internalAddress` was again rejected as a tie-breaker, for the reason this
  file already records: R1.11.2 documents it as changing whenever a cartridge
  is assigned, unassigned or moved, so at 9 674 cartridges it would churn a
  fresh series out of every robot move.
  **The duplicate is surfaced rather than absorbed**, at the maintainer's
  instruction and correctly: two cartridges answering to one barcode is an
  operational fault a human cannot resolve either.
  `tapelibrary_data_cartridges_usage_duplicate_volsers` publishes the count and
  `DataCartridgeDuplicateVolser` pages on `> 0` after 24h — a condition, not a
  level, with the `for` sized to a physical relabel rather than to a page.
  **The rule this sets for every collector after it: validate a key only where
  a key is actually used.** A response that cannot be keyed for one family is
  not necessarily unusable for the others, and refusing it wholesale trades
  everything for the part that is ambiguous.
- `[RESOLVED 2026-08-03]` **The five libraries have been reached, all at once, and the multi-instance
  model works** (2026-08-03). One process, five instances, one shared account,
  the SOCKS tunnel in front of all of them: **90 of 90 collectors populated,
  zero errors**. This is the first time anything beyond `library1` was
  contacted, and it exercised three things nothing else had. The per-instance
  limiters really are independent — the five libraries drained their queues at
  visibly different rates (18/18, 15/18, 12/18, 10/18, 3/18 at one point) with
  no interference. One RoE session per machine, five in total, well inside the
  ~100 the manual documents. And `--instance-label library` carries cleanly:
  every series is attributed, and the only unlabelled ones are the exporter's
  own self-instrumentation, as designed.
  **Two things this contradicted.** The fleet is NOT homogeneous (see
  `## Cardinality budget`, corrected the same day), and the duplicated volser
  that took out `data_cartridges_lifetime` exists on `library1` ONLY — the
  other four report zero. So it is a site-specific data-quality fault on one
  machine rather than a property of the endpoint, which is exactly what
  `DataCartridgeDuplicateVolser` was shaped to say.
- `[RESOLVED 2026-08-03]` **The concurrency ceiling decided in `## Architecture decisions` is not the
  shipped default** (found 2026-08-03). That section fixes it at 1, and
  `--exporter.max-requests-per-target` defaults to **0, meaning unlimited**. The
  consequence is not theoretical: run against `library1`, all eighteen collectors
  fire their first refresh simultaneously against a machine whose LCC and robotics
  paths serialize internally, and **every one of them fails with
  `context deadline exceeded`** — including the ones with 60s budgets.
  **Setting the default to 1 is necessary but not sufficient**, which is why it was
  not simply changed. With a ceiling of 1 the queue wait is charged against each
  collector's own timeout (see `Client.Fetch`, which applies `c.timeout` to the
  context *before* `acquire`), and the measured latencies above sum past 400s, so
  eighteen collectors starting together would still starve each other. The fix has
  three parts that must land together: the ceiling at 1, a staggered or jittered
  first refresh so the boot storm spreads, and timeouts sized against the table
  above rather than against a shared 5s. Decide all three at once.
  - **Resolved 2026-08-03, with the middle part replaced by something better.**
    The ceiling now defaults to 1, the timeouts are calibrated from the table
    above, and the two heavy cartridge collectors moved from 15m to 1h (together
    they needed more than 900s of the single slot per 900s cycle). **The stagger
    was NOT implemented, and deliberately so**: delaying `Start` is unsafe on
    this lifecycle, because `done` is created in the constructor and closed by
    `Start`'s goroutine, so a `Start` that never runs (context cancelled during
    the delay) leaves `Done()` open and hangs shutdown for its whole budget.
    What shipped instead is a **split of the queue budget from the request
    budget** in `Client.Fetch`: the wait for a slot is bounded by
    `--exporter.max-queue-wait` (15m) and the request by the collector's own
    timeout. That fixes any alignment rather than only the boot storm, touches
    no collector, and needs no lifecycle change. R1.11.2 turned out to support
    the ceiling directly, which the original decision only inferred: its "Query
    and task flow" notes require each REST response to be retrieved before the
    next command is sent.
    **Result against the real fleet: 17 of 19 collectors populated, from 0.**
- `[RESOLVED 2026-08-03]` **`success=1` is emitted while nothing works** (found 2026-08-03, and the most
  serious observability gap in this exporter). `StatusTracker` counts the metrics a
  collector emits per scrape, and a background collector ALWAYS emits its
  `..._last_refresh_timestamp_seconds` gauge — by design, so that the startup
  window before the first refresh is not reported as a failure. The consequence is
  that a collector whose every refresh has failed since boot still reports
  `tapelibrary_exporter_collector_success{collector="…"} 1`. Observed on all
  eighteen collectors at once during the run above: eighteen `success=1`, eighteen
  freshness gauges reading `0`, and no data at all.
  **The freshness gauge is the real health signal and nothing reads it.** Before
  2026-08-03 the eighteen gauges appeared in `monitoring/prometheus/alerts.yml`
  exactly once, inside a comment. The `== 0` case is unambiguous and needs no
  threshold — it means no refresh has EVER completed — and is what would have
  caught this immediately.
  **Why the test suite could not have caught any of this**, which is the part worth
  carrying forward: every collector test points at an `httptest` server that
  answers 200 instantly. No latency, no contention, no authentication. A suite
  built that way is silent on timeouts, on ceilings, and on auth — the three things
  that were actually broken. The fake library added in `session_test.go` closes the
  auth third; the other two remain untested by construction.
- `[ACCEPTED]` **`cleaning_cartridges` is asymmetric on purpose.** It is the only collector
  emitting `volser` by default. The asymmetry is justified by population size, not by
  the resource's nature; if a site runs cleaning cartridges in the thousands, the same
  opt-in flag pattern applies and the default should flip.
  **Shipped 2026-08-01 as planned**: `--collector.cleaning_cartridges.per-volser`
  defaults to `true`, and flipping it to `false` takes the collector from 146 series
  to 6 without silencing any of the three supply alerts, which read library-wide
  aggregates emitted either way. What remains open is only the trigger for flipping
  it: nobody has measured how a site running cleaning cartridges in the thousands
  actually behaves, so the default stands on this fleet's 70.
- `[OPEN]` **A cleaning cartridge's `mostRecentUsage` has no threshold attached to it.**
  The per-cartridge `last_usage_timestamp_seconds` gauge shipped 2026-08-01, but no
  rule reads it and none should be invented: nobody here has measured how long a
  healthy cleaning cartridge legitimately sits between mounts on a library that
  holds 70 of them and cleans a drive rarely. It is there to be looked at when the
  supply alerts fire, and to answer "is the robot always reaching for the same
  handful of cartridges" once a few months of history exist. Revisit once it does.
- `[OPEN]` **`dataWrittenToCartridge` is documented as "Number of MB", ambiguously.** The
  design converts to `_bytes_total` (base units) assuming decimal MB (×10⁶). If IBM
  means MiB (×2²⁰), every byte figure is 4.9% low. Verify against a cartridge whose
  written volume is known from the host side before the collector ships.
- `[ACCEPTED]` **Adding an instance label key later requires a restart.** `model` is included from
  the start for exactly this reason. Any further per-instance dimension (site, floor,
  owner) is cheap to add now and expensive to add after the first deployment.
