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
- **Shared label vocabulary:** `library`, `model`, `location`, `state`, `operation`,
  `access`, `logical_library`, `media_type`, `cartridge_type`, `severity`, `gripper`,
  `axis`, `door`, `direction`
  — plus `volser`, reserved exclusively for the opt-in per-cartridge detail and for
  the bounded `cleaning_cartridges` collector.
  - `operation` carries the drive's own `operation` field (`none`, `empty`, `loading`,
    `ready`, `unloading`, `unloaded`); `access` carries the ternary `accessible` field
    (`normal`, `limited`, `no`); `direction` takes `read`/`write` on the cartridge
    error counters. **`type` is deliberately not a label name** — the Prometheus
    exporter guidance names it as the canonical too-generic label — so the cartridge
    generation (`JD`, `JA`, `JK`…) is `cartridge_type`.
  - `location` always carries the library's own native location string verbatim
    (`drive_F1C4R1`, `slot_F7C3R15T1`, `frame_F1`, `accessor_Aa`,
    `powerSupply_F1PSa`, `ioStation_F2IOu`) — never a re-derived frame/column/row
    triple, so a value in a dashboard can be pasted straight into the GUI.
  - `gripper` takes `1`/`2`; `axis` takes `x`/`y`; `door` takes `front`/`rear`/`side`.
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
  - `frames` — critical if `state=~"frontDoorOpenWhileNotAllowed|acUnreachable"` for
    30m; warning if `state=~"frontDoorOpen|rearDoorOpen|sideDoorOpen|calibrationRequired|inventoryPending"` for 30m.
  - `accessors` — critical if `state=~"noMovementAllowed|bothGrippersFailed"` for 30m;
    warning if `state=~"gripper1Failed|gripper2Failed|scannerFailed|noMotorPower|reorienting|onlineStandby|inServiceMode"`
    for 30m; warning if a dual-accessor library drops to a single active accessor.
  - `drives` — critical if `state="unreachable"` for 1h; warning if
    `state=~"restarting|initializing|resetRequired|updating|inServiceMode"` for 1h;
    warning if the count of drives in `state="online"` per `logical_library` falls
    below a configured floor.
  - `power_supplies` — critical if any supply leaves `state="online"` for 15m
    (redundancy lost, and nothing else reports it).
  - `node_cards` — warning if `state=~"inServiceMode|unreachable|noEthernet|noCAN"` for 30m.
  - `io_stations` — warning if the door has been open for more than 1h, or if the
    station has been full for more than 1h (exports are not being collected).
  - `fc_ports` — warning if a port whose drive is `online` sits at
    `state="noLightDetected"` for 15m; warning if negotiated speed drops below the
    port's configured setting.
  - `logical_libraries` — warning if assigned cartridges approach `virtualSlots`.
  - `cleaning_cartridges` — warning if summed `cleansRemaining` per library < 100,
    critical if < 30 (verbatim from the legacy rules); warning if the count of
    cleaning cartridges in a usable state reaches zero.
  - `data_cartridges` — warning on any cartridge count in a non-`normal` state
    sustained for 1h; warning if the count with `lifetimeRemaining` below 20% crosses
    a threshold.
  - `slots` — warning if free slots per library fall below a configured floor
    (a full library cannot accept imports); warning if `putRetries`/`getRetries`
    rates rise, which is the earliest robotics-degradation signal available.
  - `events` — warning on any event of `severity="warning"` in the last window;
    critical on `severity="error"`.
  - `data_cartridges_lifetime` — warning if the count of cartridges with uncorrected
    read or write errors increases (media loss is imminent and silent otherwise).
  - `reports_library` — warning if hourly `mounts` drops to zero during a window
    where it never realistically should; warning if `humidityAverage` > 50%
    (verbatim from the legacy rule) or temperature leaves the operating envelope.
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

- [x] `example`  background  built 2026-07-31 — the scaffold's own starter collector,
      documented as `## ExampleCollector` in `docs/metrics.md`. Not a TS4500 resource:
      it exists to be adapted into the first real collector, or removed once one lands.
- [ ] `library`  background  `GET /v1/library`
- [ ] `frames`  background  `GET /v1/frames`
- [ ] `accessors`  background  `GET /v1/accessors`
- [ ] `drives`  background  `GET /v1/drives`
- [ ] `power_supplies`  background  `GET /v1/powerSupplies`
- [ ] `node_cards`  background  `GET /v1/nodeCards`
- [ ] `io_stations`  background  `GET /v1/ioStations`
- [ ] `fc_ports`  background  `GET /v1/fcPorts`
- [ ] `logical_libraries`  background  `GET /v1/logicalLibraries`
- [ ] `cleaning_cartridges`  background  `GET /v1/cleaningCartridges`
- [ ] `data_cartridges`  background  `GET /v1/dataCartridges`
- [ ] `slots`  background  `GET /v1/slots`
- [ ] `events`  background  `GET /v1/events`
- [ ] `data_cartridges_lifetime`  background  `GET /v1/dataCartridges/lifetimeMetrics`
- [ ] `reports_library`  background  `GET /v1/reports/library`
- [ ] `reports_drives`  background  `GET /v1/reports/drives`
- [ ] `reports_accessors`  background  `GET /v1/reports/accessors`
- [ ] `diagnostic_cartridges`  background  `GET /v1/diagnosticCartridges`

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
  + 1 `_info`; worst case ~24 series.
- `frames`: labels `library`, `model`, `location`, `state`, `door`; 12 frames ×
  (6 states + 3 doors + 4 counts + 1 `_info`); worst case ~168 series.
- `accessors`: labels `library`, `model`, `location`, `state`, `access`, `gripper`,
  `axis`; 2 accessors × (11 states + ~12 counters/gauges); worst case ~46 series.
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
- `power_supplies`: labels `library`, `model`, `location`, `state`; 8 × 3 states;
  worst case ~24 series.
- `node_cards`: labels `library`, `model`, `location`, `state`; 8 × (7 states +
  1 `_info`); worst case ~64 series.
- `io_stations`: labels `library`, `model`, `location`, `state`, `door`;
  2 × (5 states + ~4); worst case ~18 series.
- `fc_ports`: labels `library`, `model`, `location`, `state`; 80 ports × (4 states +
  speed + 1 `_info`); worst case ~480 series.
- `logical_libraries`: labels `library`, `model`, `logical_library`, `media_type`;
  2 × 5; worst case ~10 series.
- `cleaning_cartridges`: labels `library`, `model`, `state`, `volser`, `location`.
  **The one collector where `volser` is on by default**, because its population is
  bounded by cleaning policy (70 in the capture), not by library capacity, and the
  per-cartridge `cleansRemaining` is exactly what the operator needs to find the
  exhausted one. 70 cartridges × 1 + ~5 aggregates; worst case ~75 series.
  Reduction flag if a site runs many more: `--collector.cleaning_cartridges.per-volser`.
- `data_cartridges`: labels `library`, `model`, `state`, `logical_library`,
  `media_type`, `cartridge_type`. **Aggregated by default**: counts by state ×
  logical library, by media type, an encrypted/WORM breakdown, and a
  `lifetimeRemaining` histogram (emitted as a `_ratio`, 0-1, not the API's 0-100).
  Worst case ~40 series. Opt-in `--collector.data_cartridges.per-volser` (default
  `false`) adds `volser` and 3 series per cartridge — a lifetime-remaining ratio, a
  last-usage timestamp, and one `_info` carrying the active state and location:
  **~29 250 series** at the 9 749 cartridges this library actually holds.
- `slots`: labels `library`, `model`, `state`. Aggregated by default: counts by state,
  occupied/empty/total, tier distribution, and summed `puts`/`putRetries`/`getRetries`.
  Worst case ~15 series. Opt-in `--collector.slots.per-slot` (default `false`) adds
  `location` and ~4 series per slot: **~42 800 series** at 10 732 slots.
- `events`: labels `library`, `model`, `severity` (5 documented values). Counts and
  last-seen timestamp per severity over the returned window; worst case ~10 series.
  `errorCode` is deliberately **not** a label — see `## Open questions / assumptions` —
  and neither is `state`, which the manual documents as interpolated free text.
- `data_cartridges_lifetime`: labels `library`, `model`, `direction`. Histograms over
  `motionMeters`, `mounts`, `dataWrittenToCartridge` and the four error counters;
  worst case ~60 series. Opt-in `--collector.data_cartridges_lifetime.per-volser`
  (default `false`) adds `volser` and 7 series per cartridge — `motion_meters_total`,
  `mounts_total`, `written_bytes_total`, and `errors_{corrected,uncorrected}_total`
  × `direction={read,write}`: **~68 250 series**. These are the one place the target
  hands over genuine monotonic lifetime counters, so they are `CounterValue` with
  `_total`, not Gauges.
- `reports_library`: labels `library`, `model`. Newest complete hourly window only:
  7 activity counters + 3 temperature + 3 humidity + 1 window timestamp;
  worst case ~14 series.
- `reports_drives`: labels `library`, `model`, `location`; 40 drives × ~13;
  worst case ~520 series.
- `reports_accessors`: labels `library`, `model`, `location`; 2 × ~13;
  worst case ~26 series.
- `diagnostic_cartridges`: labels `library`, `model`, `state`, `volser` (bounded, 5 in
  the capture); 5 × (5 states + 1 `_info`); worst case ~30 series.

**Fleet totals.** Defaults: ~2 425 series per library, **~12 150 across five
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

## Open questions / assumptions

- **Legacy drive-severity divergence, unresolved.** `samples/legacy/check_library.sh`
  classifies `inServiceMode` as critical (`2`) and `cleaning` as warning (`1`);
  `check_library_30min.sh` classifies the same two as warning (`1`) and normal (`0`).
  The alert candidates above take the 30-minute script's reading (the more recently
  edited of the two), because a drive in a scheduled cleaning cycle is doing exactly
  what it should. **Confirm before the `drives` alert rule ships.**
- **Four legacy states contradict the R1.11.2 manual**, and each needs a decision
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
  - `accessors`: the scripts expect `reorienting`; the manual documents `calibrating`.
    One of the two never matches. The capture shows neither (both accessors were
    `onlineActive`), so this cannot be settled from the material on hand.
  - `nodeCards`: the scripts expect `normal` as the healthy value; the manual
    documents `online`. The capture shows `unknown`, a state the scripts do not map at
    all — so those node cards currently produce no severity value whatsoever.
  - `library`: the scripts classify `restarting` as critical, but `restarting` is not
    in the manual's library-status table (it is a *drive* and *node card* state).
- **The documented state tables are a floor, not a ceiling.** The manual references
  `failedToInitialize` (accessor), `cartridgeFailedMove` and `errorThresholdExceeded`
  (cartridge) in prose while omitting all three from the corresponding tables, and
  lists `lifetimeRemaining` — plainly a numeric attribute — inside the data-cartridge
  *state* table. Hence the emit-observed-values-too rule above. Any state a collector
  meets that is not in its list should also raise a one-line note in
  `docs/exporter-journal.md` so the vocabulary converges on reality over time.
- **Volume figures are inferred, not measured.** The captures are truncated samples
  (60 `dataCartridges` against `totalCartridges: 9749`; 79 `slots` against
  `totalCapacity: 10732`). Worst-case series counts above come from the library's own
  counters. `/add-collector` should record the *observed* count next to each budget
  once a collector runs against a real library.
- **Port 9170 is not registered.** It is free within this fleet (which already uses
  9313, 9315, 9341, 9466, 9610, 9800) but has not been claimed on the Prometheus
  default-port allocation wiki. Claim it before the first public release.
- **`events.errorCode` is excluded from the default budget.** The field is a
  hexadecimal library error code whose value set is large and not enumerated in the
  manual, so it is a cardinality risk that cannot be budgeted from what is on hand.
  The `events` collector needs an explicit decision at `/add-collector` time:
  aggregate only, an allow-list of codes worth alerting on, or a bounded top-N.
- **Session lifetime is undocumented.** The manual states that a session persists
  "until they logout or the session times out due to inactivity based on the library's
  settings" without naming a default, and does not state a per-user concurrent-session
  limit. The poller must therefore treat a `401` as a re-login trigger rather than
  assume a session outlives a chosen interval, and five pollers sharing one monitoring
  account may or may not contend. Verify against a real library early.
- **TLS trust is undecided.** SSL was enabled on these libraries only recently, and
  whether they present a self-signed certificate or one issued by an internal CA is
  unverified. `config.example.yml` should demonstrate `ca_file` rather than
  `insecure_skip_verify`; confirm which the fleet actually needs.
- **Report windows carry their own timestamp, which will not be honoured.**
  `/v1/reports/*` returns several one-hour windows, each with its own `time`. The
  design exposes only the newest complete window, as a Gauge, at scrape time — no
  `honor_timestamps`. A window that stops advancing will therefore look fresh; a
  window-age gauge is the intended mitigation and should be built into
  `reports_library` rather than added later.
- **Per-endpoint poll cadences are not calibrated in this brief.** An earlier session
  ran a timing probe per endpoint per library, but those measurements were not
  preserved on disk and are not restated here from memory. The design assumes
  per-endpoint intervals (a fast tier for hardware health, a slow tier for the heavy
  inventory endpoints, an hourly tier for `reports_*`), with calibrated defaults
  shipped in code and overridable in configuration. Re-measure before fixing the
  defaults.
- **`cleaning_cartridges` is asymmetric on purpose.** It is the only collector
  emitting `volser` by default. The asymmetry is justified by population size, not by
  the resource's nature; if a site runs cleaning cartridges in the thousands, the same
  opt-in flag pattern applies and the default should flip.
- **`dataWrittenToCartridge` is documented as "Number of MB", ambiguously.** The
  design converts to `_bytes_total` (base units) assuming decimal MB (×10⁶). If IBM
  means MiB (×2²⁰), every byte figure is 4.9% low. Verify against a cartridge whose
  written volume is known from the host side before the collector ships.
- **Adding an instance label key later requires a restart.** `model` is included from
  the start for exactly this reason. Any further per-instance dimension (site, floor,
  owner) is cheap to add now and expensive to add after the first deployment.
