# Metrics

> Back to [README](../README.md)

Every metric `tapelibrary_exporter` can emit, grouped by collector. This file is kept
truthful by `make docs-check` (part of `make check`, see
[CONTRIBUTING.md](../CONTRIBUTING.md)'s Definition of Done): any metric or label
listed below that the code cannot actually produce fails the build. A metric the
code emits but this file doesn't document is only a warning: see
`internal/collector/docs_check_test.go`.

<!--
docs-check parses this file as a sequence of markdown tables, one metric per
row, in this exact 4-cell shape:

| `metric_name` | Type | `label1`, `label2` | Description |

- Metric name: backtick-quoted, matching the fqName passed to
  prometheus.NewDesc / HistogramOpts.Name in internal/collector/*.go.
- Type: Gauge, Counter, Histogram, or Summary (informational only, not
  itself verified by docs-check).
- Labels: each backtick-quoted, comma-separated; a literal `-` when the
  metric has none.
- Description: free text. Avoid a literal `|` character in this cell (it
  breaks table parsing).

Standard Go runtime, process, and build-info metrics (from
`prometheus/client_golang/prometheus/collectors`, registered in
`cmd/tapelibrary_exporter/main.go`, disable with `--web.disable-exporter-metrics`)
are intentionally not listed here.
-->

## LibraryCollector

Defined in `internal/collector/library.go`, the background-refresh variant: a
goroutine polls `GET /v1/library` on `--collector.library.interval` and every
scrape serves the last cached result.

`tapelibrary_library_state` is a **stateset**: every status the TS4500 R1.11.2
manual documents is emitted as its own series, exactly one carrying `1` and the
rest `0`. A status the manual does not document is emitted too, as an extra
series, because the manual's tables are demonstrably a floor rather than a
ceiling. Severity classification lives in
[monitoring/prometheus/alerts.yml](../monitoring/prometheus/alerts.yml), never
in the metric value.

| Metric | Type | Labels | Description |
|---|---|---|---|
| `tapelibrary_library_state` | Gauge | `state` | Operational status of the library, as a stateset: 1 on the active status and 0 on every other known status. |
| `tapelibrary_library_slots_capacity` | Gauge | - | Total number of cartridge slots the library physically holds. |
| `tapelibrary_library_slots_licensed` | Gauge | - | Number of cartridge slots the library is currently licensed to use. |
| `tapelibrary_library_cartridges_present` | Gauge | - | Number of cartridges currently present in the library. |
| `tapelibrary_library_cartridges_assigned` | Gauge | - | Number of cartridges currently assigned to a logical library. |
| `tapelibrary_library_capacity_util_threshold_ratio` | Gauge | - | Capacity utilization threshold configured on the library, as a ratio from 0 to 1 (the API reports a 0-100 percentage). |
| `tapelibrary_library_dual_accessor_util_threshold_ratio` | Gauge | - | Dual-accessor utilization threshold configured on the library, as a ratio from 0 to 1 (the API reports a 0-100 percentage). |
| `tapelibrary_library_info` | Gauge | `name`, `serial`, `firmware` | Library identity, always 1. Identity strings live here rather than on a measurement series, so a firmware upgrade changes this series alone instead of breaking the continuity of every other. |
| `tapelibrary_library_last_refresh_timestamp_seconds` | Gauge | - | Unix time of the last successful library refresh. Alert if time() minus this exceeds 2x the collector's configured interval. |

## FramesCollector

Defined in `internal/collector/frames.go`, the background-refresh variant: a
goroutine polls `GET /v1/frames` on `--collector.frames.interval` and every
scrape serves the last cached result. Every metric below carries `location`,
the library's own native frame string (`frame_F1`, `frame_F2`, ...) verbatim,
so a value in a dashboard can be pasted straight into the library's GUI.

`tapelibrary_frame_state` is a **stateset**, on the same terms as
`tapelibrary_library_state` above: every state the TS4500 R1.11.2 manual
documents is emitted per frame as its own series, exactly one carrying `1` and
the rest `0`, and an undocumented state observed on the wire is emitted too.

`tapelibrary_frame_door_open` is the **only** source of door position on this
exporter. R1.11.2 does not define `frontDoorOpen`, `rearDoorOpen` or
`sideDoorOpen` as frame *states*, whatever the legacy RoS scripts assumed, so
no `tapelibrary_frame_state` series ever reports a door. A door position the
frame does not physically have (the API reports `null`, which is every frame's
rear door on the hardware captured so far) produces no series at all, rather
than a `0` asserting that a door which does not exist is closed.

| Metric | Type | Labels | Description |
|---|---|---|---|
| `tapelibrary_frame_state` | Gauge | `location`, `state` | Operational state of the frame, as a stateset: 1 on the active state and 0 on every other known state. |
| `tapelibrary_frame_door_open` | Gauge | `location`, `door` | Whether the frame's door is open (1) or closed (0), for `door` in front, rear, side. A door position the frame does not physically have reports no series at all. |
| `tapelibrary_frame_slots_capacity` | Gauge | `location` | Number of cartridge slots this frame physically holds. |
| `tapelibrary_frame_cartridges_present` | Gauge | `location` | Number of cartridges currently present in this frame. |
| `tapelibrary_frame_drives_installed` | Gauge | `location` | Number of tape drives installed in this frame. |
| `tapelibrary_frame_io_stations_installed` | Gauge | `location` | Number of I/O stations installed in this frame. |
| `tapelibrary_frame_info` | Gauge | `location`, `serial`, `mtm`, `frame_type`, `media_type` | Frame identity, always 1. Identity strings live here rather than on a measurement series, so replacing a frame changes this series alone instead of breaking the continuity of every other. |
| `tapelibrary_frames_last_refresh_timestamp_seconds` | Gauge | - | Unix time of the last successful frames refresh. Alert if time() minus this exceeds 2x the collector's configured interval. |

## AccessorsCollector

Defined in `internal/collector/accessors.go`, the background-refresh variant: a
goroutine polls `GET /v1/accessors` on `--collector.accessors.interval` and
every scrape serves the last cached result. Every metric below carries
`location`, the library's own native accessor string (`accessor_Aa`,
`accessor_Ab`) verbatim, so a value in a dashboard can be pasted straight into
the library's GUI.

`tapelibrary_accessor_state` is a **stateset**, on the same terms as
`tapelibrary_library_state` and `tapelibrary_frame_state` above. Its known set
is the manual's own ten, plus `failedToInitialize`, which R1.11.2 names in the
description of the library status `accessorsUnavailable` and tabulates nowhere.
`reorienting` is **not** in the set: the legacy RoS scripts expect it and
R1.11.2 documents the same phase as `calibrating`, so a rule matching
`reorienting` can never have fired on this firmware.

`tapelibrary_accessor_drive_access` and `tapelibrary_accessor_cartridge_access`
are two further statesets, over `normal` and `limited`. They exist because the
state of a robot does not tell you what it can currently reach: on a
dual-accessor library, an accessor can be `onlineActive` and still report
`limited` because the *other* accessor is parked somewhere that blocks its path.

The five lifetime totals are Counters, not Gauges: the library reports them as
monotonic device counters that survive a restart of this exporter.

Four fields the API documents as nullable produce **no series at all** when
null, rather than a `0`. `tapelibrary_accessor_temperature_celsius` and
`tapelibrary_accessor_humidity_ratio` are the ones this matters for today: no
TS4500 accessor carries an environmental sensor ("For TS4500, null is returned
as there is no sensor"), so on this hardware neither metric is ever emitted,
and both descriptors exist so that hardware which does carry the sensor
reports it without a code change. `tapelibrary_accessor_pivots_total` and
`tapelibrary_accessor_velocity_scaling_pivot_ratio` follow the same rule for an
accessor that does not pivot.

| Metric | Type | Labels | Description |
|---|---|---|---|
| `tapelibrary_accessor_state` | Gauge | `location`, `state` | Operational state of the robotic accessor, as a stateset: 1 on the active state and 0 on every other known state. |
| `tapelibrary_accessor_drive_access` | Gauge | `location`, `access` | Whether the accessor can reach the library's drives, as a stateset over `normal` and `limited`. On a dual-accessor library this also reflects the other accessor's position. |
| `tapelibrary_accessor_cartridge_access` | Gauge | `location`, `access` | Whether the accessor can reach the library's cartridges, as a stateset over `normal` and `limited`. On a dual-accessor library this also reflects the other accessor's position. |
| `tapelibrary_accessor_pivots_total` | Counter | `location` | Number of pivots this accessor has performed in its lifetime. An accessor that does not pivot reports no series at all. |
| `tapelibrary_accessor_bar_code_scans_total` | Counter | `location` | Number of bar code scans this accessor has performed in its lifetime. |
| `tapelibrary_accessor_travel_meters_total` | Counter | `location`, `axis` | Distance in meters this accessor has travelled in its lifetime, for `axis` x (horizontal) and y (vertical). |
| `tapelibrary_accessor_gets_total` | Counter | `location`, `gripper` | Number of times gripper 1 or 2 has engaged to retrieve a cartridge into this accessor, in its lifetime. |
| `tapelibrary_accessor_puts_total` | Counter | `location`, `gripper` | Number of times gripper 1 or 2 has engaged to place a cartridge out of this accessor, in its lifetime. |
| `tapelibrary_accessor_velocity_scaling_xy_ratio` | Gauge | `location` | Scaling applied to the accessor's maximum velocity in the X and Y directions, as a ratio from 0 to 1 (the API reports a 0-100 percentage). Anything below 1 means the accessor has been deliberately slowed. |
| `tapelibrary_accessor_velocity_scaling_pivot_ratio` | Gauge | `location` | Scaling applied to the accessor's maximum pivot velocity, as a ratio from 0 to 1. An accessor that does not pivot reports no series at all. |
| `tapelibrary_accessor_temperature_celsius` | Gauge | `location` | Temperature in Celsius measured inside the library by a sensor on this accessor. No TS4500 accessor carries this sensor, so on that hardware no series is emitted. |
| `tapelibrary_accessor_humidity_ratio` | Gauge | `location` | Relative humidity measured inside the library by a sensor on this accessor, as a ratio from 0 to 1. No TS4500 accessor carries this sensor, so on that hardware no series is emitted. |
| `tapelibrary_accessors_last_refresh_timestamp_seconds` | Gauge | - | Unix time of the last successful accessors refresh. Alert if time() minus this exceeds 2x the collector's configured interval. |

## DrivesCollector

Defined in `internal/collector/drives.go`, the background-refresh variant: a
goroutine polls `GET /v1/drives` on `--collector.drives.interval` and every
scrape serves the last cached result. Every metric below carries `location`,
the library's own native drive string (`drive_F1C4R1`) verbatim, so a value in
a dashboard can be pasted straight into the library's GUI.

This is the largest per-object collector on the fleet: 40 drives x 20 series is
roughly a third of what a library emits at default settings.

**Three statesets, not one.** `tapelibrary_drive_state` is a stateset on the
same terms as `tapelibrary_library_state` and `tapelibrary_frame_state` above,
over the manual's own nine values. `tapelibrary_drive_operation` is a second
one, because the state of a drive does not tell you what it is doing: a drive
can be `online` and idle or `online` and mid-unload, and only `operation`
separates the two. The API reports `null` there when nothing is in progress,
which this collector renders as the documented value `none` — this is the one
field in the exporter where a JSON null becomes a series rather than
suppressing one, because here null is a documented value rather than a missing
reading. `tapelibrary_drive_access` is the third, over the `accessible`
ternary: a drive can be `online` and still be unreachable, in which case no
cartridge can be mounted in it.

**`access` has three values here, not two.** The set is read per-resource, from
the endpoint being collected: `normal`, `limited` and `no` on a drive, against
the two `tapelibrary_accessor_drive_access` carries above. Same label key, same
meaning, two documented sets.

**`logical_library` rides on the measurement series**, not only on the info
one. It costs nothing (it is constant per drive) and it lets a
"too few online drives in this logical library" rule be a plain
`count by (job, library, logical_library)` rather than a `group_left` join. The
accepted cost is that reassigning a drive between logical libraries breaks the
continuity of its measurement series — a real configuration change, and one
worth seeing as a discontinuity.

`tapelibrary_drive_last_cleaned_timestamp_seconds` produces **no series at
all** for a drive that has never been cleaned, rather than a `0`: a zero there
would place the last cleaning at the Unix epoch and make every "cleaned within
the last N days" query silently answer about the wrong drives. A timestamp the
collector cannot parse takes the same branch, and is logged rather than
guessed at.

`tapelibrary_drive_loaded_cartridge_info` is **off by default**, behind
`--collector.drives.per-volser`. Only 40 volsers are ever loaded at once, but
the drive holding a given tape changes constantly, so `location` x `volser`
accumulates an index entry in Prometheus for every pairing that has ever
existed (up to 9 749 x 40 on this hardware) even though the active set stays at
40. Drives holding no cartridge emit nothing on this metric whether the flag is
set or not.

| Metric | Type | Labels | Description |
|---|---|---|---|
| `tapelibrary_drive_state` | Gauge | `location`, `logical_library`, `state` | Operational state of the tape drive, as a stateset: 1 on the active state and 0 on every other known state. |
| `tapelibrary_drive_operation` | Gauge | `location`, `logical_library`, `operation` | Operation the tape drive is currently performing, as a stateset. Orthogonal to the drive's state. The API's `null` is reported as `none`. |
| `tapelibrary_drive_access` | Gauge | `location`, `logical_library`, `access` | Whether the accessor can reach this drive, as a stateset over `normal`, `limited` and `no`. A drive can be online and still be unreachable. |
| `tapelibrary_drive_last_cleaned_timestamp_seconds` | Gauge | `location`, `logical_library` | Unix time at which this drive was last cleaned. A drive that has never been cleaned reports no series at all. |
| `tapelibrary_drive_info` | Gauge | `location`, `logical_library`, `serial`, `firmware`, `mtm`, `media_type`, `use`, `encryption`, `interface`, `wwnn` | Drive identity, always 1. Identity strings live here rather than on a measurement series. `use=controlPath` marks the drives the host talks to the library through. |
| `tapelibrary_drive_loaded_cartridge_info` | Gauge | `location`, `logical_library`, `volser` | The cartridge currently loaded in this drive, always 1. Emitted only with `--collector.drives.per-volser`, and only for drives that hold a cartridge. |
| `tapelibrary_drives_last_refresh_timestamp_seconds` | Gauge | - | Unix time of the last successful drives refresh. Alert if time() minus this exceeds 2x the collector's configured interval. |

## PowerSuppliesCollector

Defined in `internal/collector/power_supplies.go`, the background-refresh
variant: a goroutine polls `GET /v1/powerSupplies` on
`--collector.power_supplies.interval` and every scrape serves the last cached
result. The stateset carries `location`, the library's own native power-supply
string (`powerSupply_F1PSa`, `powerSupply_F1PSb`, ...) verbatim, so a value in
a dashboard can be pasted straight into the library's GUI.

`tapelibrary_power_supply_state` is a **stateset**, on the same terms as
`tapelibrary_library_state` above: all three states the TS4500 R1.11.2 manual
documents (`unknown`, `failed`, `online`) are emitted per supply as their own
series, exactly one carrying `1` and the rest `0`, and an undocumented state
observed on the wire is emitted too.

This is the narrowest collector in the exporter, and deliberately so: R1.11.2
gives `/v1/powerSupplies` exactly two attributes, `location` and `state`, so
there is no identity to carry on an `_info` series and no measurement to carry
beside the stateset. Supplies are installed as a redundant pair per frame and
a single one is adequate to power its frame, so a supply leaving `online`
costs redundancy rather than service — which is precisely why it is worth a
series: nothing else in the library reports it, and the loss stays invisible
until the surviving supply also goes. Only L25/L55 and D25/D55 frames carry
supplies, so a twelve-frame library reports eight of them rather than
twenty-four.

| Metric | Type | Labels | Description |
|---|---|---|---|
| `tapelibrary_power_supply_state` | Gauge | `location`, `state` | Operational state of the power supply, as a stateset: 1 on the active state and 0 on every other known state. |
| `tapelibrary_power_supplies_last_refresh_timestamp_seconds` | Gauge | - | Unix time of the last successful power supplies refresh. Alert if time() minus this exceeds 2x the collector's configured interval. |

## NodeCardsCollector

Defined in `internal/collector/node_cards.go`, the background-refresh variant:
a goroutine polls `GET /v1/nodeCards` on `--collector.node_cards.interval` and
every scrape serves the last cached result. Node cards are the library's own
nervous system — the LCC (Library Control Card) in each frame, and the MDA and
ACC cards riding on each accessor — which is why they earn a collector rather
than a footnote: R1.11.2 attributes a power supply reporting `unknown` to an
unreachable LCC rather than to the supply, so a degraded card shows up as noise
on several other collectors before anything names it.

**`location` is not a unique key on this endpoint, and this is the only
collector where that is true.** An accessor carries an MDA *and* an ACC card,
so `accessor_Aa` names two different cards. Every metric below is therefore
keyed by `location` **and** `card_type`, and the parser rejects a duplicate of
that *pair* rather than of `location` alone. The label is `card_type`, not
`type`: the Prometheus exporter guidance names `type` as the canonical
too-generic label, which is the same reason the cartridge generation is
`cartridge_type` and the frame class is `frame_type`.

`tapelibrary_node_card_state` is a **stateset**, on the same terms as
`tapelibrary_library_state` above: all seven states R1.11.2 documents
(`unknown`, `restarting`, `inServiceMode`, `unreachable`, `noEthernet`,
`noCAN`, `online`) are emitted per card as their own series, exactly one
carrying `1` and the rest `0`, and an undocumented state observed on the wire
is emitted too. The healthy value is `online`, **not** `normal`: the legacy
check scripts expect `normal`, this firmware documents `online`, and the
emit-observed-anyway branch is what would surface `normal` as its own series if
any library ever reported it.

**The two LCC role gauges produce no series at all for a card that is not an
LCC**, rather than a `0`. Null there means "this question does not apply to
this card", which is a different fact from "no", and a `0` would put every MDA
and ACC card into the denominator of an "exactly one primary LCC" query. The
two roles are orthogonal: `primary` is the configured leader and `reporting` is
whichever LCC is answering right now, and a failover moves the second one
first.

`tapelibrary_node_card_last_restart_timestamp_seconds` likewise produces **no
series** for a card the library reports no restart for, rather than a `0` that
would place the restart at the Unix epoch and make every "restarted within the
last N days" query silently answer about the wrong cards. A timestamp the
collector cannot parse takes the same branch, and is logged rather than guessed
at.

| Metric | Type | Labels | Description |
|---|---|---|---|
| `tapelibrary_node_card_state` | Gauge | `location`, `card_type`, `state` | Operational state of the node card, as a stateset: 1 on the active state and 0 on every other known state. |
| `tapelibrary_node_card_info` | Gauge | `location`, `card_type`, `serial`, `part_number`, `firmware` | Node card identity, always 1. Identity strings live here rather than on a measurement series, so a firmware upgrade or a card swap changes this series alone instead of breaking the continuity of every other. |
| `tapelibrary_node_card_last_restart_timestamp_seconds` | Gauge | `location`, `card_type` | Unix time at which this node card last restarted. A card the library reports no restart for produces no series at all. |
| `tapelibrary_node_card_primary_lcc` | Gauge | `location`, `card_type` | Whether this card is the library's primary LCC (1) or not (0). Emitted only for cards that report the role, which means the LCC cards. |
| `tapelibrary_node_card_reporting_lcc` | Gauge | `location`, `card_type` | Whether this card is the LCC currently answering for the library (1) or not (0). Orthogonal to the primary role: a failover moves this one first. |
| `tapelibrary_node_cards_last_refresh_timestamp_seconds` | Gauge | - | Unix time of the last successful node cards refresh. Alert if time() minus this exceeds 2x the collector's configured interval. |

## IOStationsCollector

Defined in `internal/collector/io_stations.go`, the background-refresh variant:
a goroutine polls `GET /v1/ioStations` on `--collector.io_stations.interval` and
every scrape serves the last cached result. An I/O station is how media enters
and leaves the library without opening a frame and pausing it, so nothing here
is a service-down signal — a stuck, empty or full station is imports and
exports quietly not happening, which no other endpoint reports.

`tapelibrary_io_station_state` is a **stateset**, on the same terms as
`tapelibrary_library_state` above: all five states R1.11.2 documents (`normal`,
`closedNoMagazine`, `failedToClose`, `doorOpenTooLong`, `unknown`) are emitted
per station as their own series, exactly one carrying `1` and the rest `0`, and
an undocumented state observed on the wire is emitted too.

**There is no `door` label here**, unlike `tapelibrary_frame_door_open`. A frame
carries up to three doors and needs one to tell them apart; R1.11.2 gives an I/O
station exactly one, so a label could only ever hold a single constant value and
`location` already identifies it.

**A station that reports no door produces no door series at all**, rather than a
`0`. R1.11.2 returns a null door for every Diamondback station, which reaches its
I/O station through the library's rear door and has none of its own; a `0` would
assert a door that is present and closed. A door position outside the documented
`yes`/`no` pair takes the same branch and is logged rather than guessed at. This
is the same rule the frames collector applies to its own door positions, and it
exists because a silently-`0` door gauge is exactly how a door alert stops firing
without anyone noticing.

**`tapelibrary_io_station_magazine_present` is not "a magazine is missing".**
R1.11.2 returns a null magazine in two different situations — the door is open,
*or* no magazine is inserted — and does not distinguish them on this endpoint.
The four magazine series below (capacity, occupancy, unreadable count and
`_info`) are emitted **only** while a magazine is reported: a `0` occupancy from
a station with no magazine would read as an empty magazine ready to accept an
import, which is the exact opposite of the truth.

`tapelibrary_io_station_slots_unreadable` is the subset of the occupancy whose
barcode the library could not read (`contentsVolser` carries the literal string
`unknown` for those). Such a cartridge is physically present but unidentifiable,
so it occupies a slot and cannot be imported until someone reseats or replaces
the label.

**Cartridge VOLSERs are counted, never labelled.** The shared label vocabulary
reserves `volser` for the opt-in per-cartridge detail and for the bounded
`cleaning_cartridges` collector; an I/O station's contents turn over on every
import, so labelling them would accumulate Prometheus index entries for every
cartridge that has ever passed through.

| Metric | Type | Labels | Description |
|---|---|---|---|
| `tapelibrary_io_station_state` | Gauge | `location`, `state` | Operational state of the I/O station, as a stateset: 1 on the active state and 0 on every other known state. |
| `tapelibrary_io_station_door_open` | Gauge | `location` | Whether the I/O station's door is open (1) or closed (0). A station that reports no door at all, or a position this exporter cannot map to open-or-closed, produces no series rather than a 0 asserting a closed door. |
| `tapelibrary_io_station_magazine_present` | Gauge | `location` | Whether the I/O station currently reports an inserted magazine (1) or not (0). R1.11.2 returns no magazine both when none is inserted and when the door is open, so a 0 means one of those two, not specifically an empty station. |
| `tapelibrary_io_station_slots` | Gauge | `location` | Number of cartridge slots in the magazine currently inserted in this I/O station (18 for LTO, 16 for 3592). Emitted only while a magazine is reported. |
| `tapelibrary_io_station_slots_occupied` | Gauge | `location` | Number of slots holding a cartridge in the inserted magazine, cartridges with an unreadable barcode included. Emitted only while a magazine is reported. |
| `tapelibrary_io_station_slots_unreadable` | Gauge | `location` | Number of occupied slots whose cartridge barcode the library could not read. A subset of the occupancy: present but unidentifiable, and not importable until the label is reseated or replaced. |
| `tapelibrary_io_station_info` | Gauge | `location`, `media_type` | Identity of the magazine currently inserted in this I/O station, always 1. Identity strings live here rather than on a measurement series, so swapping a magazine changes this series alone instead of breaking the continuity of the occupancy counts. |
| `tapelibrary_io_stations_last_refresh_timestamp_seconds` | Gauge | - | Unix time of the last successful io stations refresh. Alert if time() minus this exceeds 2x the collector's configured interval. |

## FCPortsCollector

Defined in `internal/collector/fc_ports.go`, the background-refresh variant: a
goroutine polls `GET /v1/fcPorts` on `--collector.fc_ports.interval` and every
scrape serves the last cached result. Fibre Channel ports are the path every
read, write and library-control command takes between a host and a drive, which
makes this the one collector that can see a drive vanish from its host while the
drive itself keeps reporting perfectly healthy on `/v1/drives`.

The endpoint returns one entry per physical port — two per drive on this
hardware, 80 across the 40 drives of the 2026-07-28 capture. **`location` alone
is the key**, unlike `tapelibrary_node_card_state`: the port number is already
baked into the location string (`fcPort_F1C4R1P0` / `...P1`), which is what
keeps a drive's two ports distinct without a second label. That was verified
against the capture rather than inherited from the collector written before it.

`tapelibrary_fc_port_state` is a **stateset**, on the same terms as
`tapelibrary_library_state` above: all four states R1.11.2 documents (`unknown`,
`noLightDetected`, `communicationNotEstablished`, `communicationEstablished`)
are emitted per port as their own series, exactly one carrying `1` and the rest
`0`, and an undocumented state observed on the wire is emitted too.

**`drive_location` rides on the measurement series, not on `_info` alone.** It
is a grouping key rather than an identity string and is constant per port, so it
costs no extra series — the same argument `logical_library` already makes on
drives. Putting it there is what lets `FCPortNoLight` join straight against
`tapelibrary_drive_state`: a port going dark matters enormously if its drive is
online and not at all if the drive is in service mode, and only the join can
tell those apart.

**`tapelibrary_fc_port_speed_bytes_per_second` is a nominal link rate, not
throughput.** The library reports a Gbps string (`16Gbps`, `8Gbps`, …) and this
converts it at 8 bits per byte. 16GFC signals at 16 Gbit/s but moves roughly
1600 MB/s of payload rather than the 2000 MB/s this metric reports, because of
64b/66b line encoding and FC framing overhead, so **dividing traffic by this
value does not give a utilisation ratio**. Bytes rather than bits because
Prometheus takes bytes as its base unit and this exporter's metric-name shape
reserves `_bytes` for exactly this.

**A port whose rate this exporter cannot map produces no speed series at all**,
rather than a `0`. That is the ordinary case on real hardware, not an error
path: 36 of the capture's 80 ports report the literal string `unknown`, a value
R1.11.2 tabulates for neither speed field. A `0` would assert a link that is up
and running at zero bytes per second — not a state Fibre Channel has — and would
make `FCPortSpeedBelowPeers` fire on every dark port forever instead of on a
genuinely renegotiated one. A rate that is neither `unknown` nor one this
exporter knows (32GFC hardware, say) takes the same branch and is logged.

**`topology_actual` is the one `_info` label here that is not constant**: it
flips between its negotiated value and `unknown` as the link comes and goes.
That churn is bounded at two label sets per port over the port's life — nothing
like the accumulating index a `volser` label would build — and it is kept
because a port that comes up `L-Port` where its peers came up `N-Port` is a real
fabric misconfiguration no other field on this endpoint reports.

**`portNumber` is deliberately not emitted.** It is already the last character
of `location`, and it is the one field where R1.11.2's attribute table and the
capture disagree outright: the manual types it `(string)` while every entry on
the wire sends a bare JSON number. The field this collector has no use for is
also the field that would cost it a firmware-dependent parse error.

| Metric | Type | Labels | Description |
|---|---|---|---|
| `tapelibrary_fc_port_state` | Gauge | `location`, `drive_location`, `state` | Link state of the Fibre Channel port, as a stateset: 1 on the active state and 0 on every other known state. |
| `tapelibrary_fc_port_speed_bytes_per_second` | Gauge | `location`, `drive_location` | Negotiated link rate of the Fibre Channel port, in bytes per second, converted from the library's own Gbps figure at 8 bits per byte. The nominal signalling rate, not achievable payload throughput. A port reporting a rate this exporter cannot map produces no series rather than a 0. |
| `tapelibrary_fc_port_info` | Gauge | `location`, `drive_location`, `drive_serial`, `wwpn`, `speed_setting`, `topology_setting`, `topology_actual`, `loop_id` | Fibre Channel port identity and configuration, always 1. Identity strings live here rather than on a measurement series, so a drive swap or a re-negotiated topology changes this series alone instead of breaking the continuity of the link state and speed. |
| `tapelibrary_fc_ports_last_refresh_timestamp_seconds` | Gauge | - | Unix time of the last successful fc ports refresh. Alert if time() minus this exceeds 2x the collector's configured interval. |

## LogicalLibrariesCollector

Defined in `internal/collector/logical_libraries.go`, the background-refresh
variant: a goroutine polls `GET /v1/logicalLibraries` on
`--collector.logical_libraries.interval` and every scrape serves the last cached
result. A logical library is a partition of the physical library, and it is what
a backup application actually mounts — so this collector reports the promise the
library made to each host, and how much of it is already spent.

**This is the only collector in the exporter with no stateset.** R1.11.2 gives
`/v1/logicalLibraries` exactly seven attributes and none of them is an
enumeration of health, so there is no `state` label here and no
emit-an-undocumented-state-anyway branch to go with it. What a partition reports
instead is capacity.

**`tapelibrary_logical_library_cartridges` divided by
`tapelibrary_logical_library_virtual_slots` is the partition's saturation, and it
is the reason this collector exists.** R1.11.2 will not let the virtual slot
count fall below the assigned cartridge count, so the quotient is a real ratio
bounded at 1 rather than an arbitrary division. At 1 the partition **stops
accepting imports while the physical library still reports thousands of free
slots** — a failure mode no other collector here can see, because
`SlotsCollector` counts the physical library and `DrivesCollector` knows only
which partition each drive belongs to. Both series carry an identical label set
so the division matches; a test pins that, since an extra label on either side
would make `LogicalLibraryNearlyFull` silently never fire.

`name` **is the key**, verified against the 2026-07-28 capture rather than
inherited from the collector written before it — the rule `NodeCardsCollector`
established when `location` turned out not to key that endpoint. R1.11.2
documents it as unique per library and the capture's two partitions agree.

**`logical_library` is the join key to `DrivesCollector`**, which reads the same
string from `/v1/drives` into the same label. This collector is what turns a
per-drive `logical_library` into something a query can size against the
partition's own slot count.

**`media_type` and `encryption_method` live on `_info` alone.** `media_type` is
identity rather than a grouping key, on the terms `DrivesCollector` fixed when it
put `logical_library` on its measurement series and left `media_type` behind.
`encryption_method` is configuration, constant per partition, and therefore free
on an `_info` series that is emitted anyway — the same terms `fc_ports` put
`speed_setting` and `topology_setting` there on. It is a label rather than a
stateset because nothing alerts on a *particular* mode: what matters is that a
partition's mode **changed**, which a label surfaces as a new `_info` series.

**Zero counts are emitted, not withheld.** This departs deliberately from the
absent-rather-than-zero habit elsewhere in this exporter
(`accessors.temperature`, a frame's missing rear door, an unmappable FC port
speed), and the test is the one `docs/exporter-journal.md` states: those are
readings nobody took, whereas R1.11.2 types all four fields here as plain
numbers. A partition holding zero cartridges is empty and a partition with zero
drives assigned cannot mount anything — both real, alarming readings that must
reach a dashboard rather than vanish from it.

**An empty array is rejected rather than cached.** A TS4500 with no partitions
serves no host application at all, and reports that condition on its own endpoint
as `library.status = notConfigured`, which `LibraryCollector` already emits. So
nothing is lost by treating `[]` as a response that lost its content — the far
likelier cause on a fleet whose five libraries are all partitioned.

| Metric | Type | Labels | Description |
|---|---|---|---|
| `tapelibrary_logical_library_drives` | Gauge | `logical_library` | Number of drives assigned to the logical library and reported to its host application as data transfer element addresses. |
| `tapelibrary_logical_library_virtual_slots` | Gauge | `logical_library` | Number of virtual storage slots the logical library reports to its host application as storage element addresses. The library will not let this fall below the assigned cartridge count, so it is the ceiling that count saturates against. |
| `tapelibrary_logical_library_virtual_io_slots` | Gauge | `logical_library` | Number of virtual I/O slots the logical library reports to its host application as import/export element addresses. Capped at 255 by the library, and never fewer than the physical I/O slots behind it. |
| `tapelibrary_logical_library_cartridges` | Gauge | `logical_library` | Number of cartridges currently assigned to the logical library. Divide by `tapelibrary_logical_library_virtual_slots` for the partition's saturation: at 1 it can accept no further imports, however much free space the physical library still reports. |
| `tapelibrary_logical_library_info` | Gauge | `logical_library`, `media_type`, `encryption_method` | Logical library identity and configuration, always 1. Identity strings live here rather than on a measurement series, so reconfiguring a partition's encryption changes this series alone instead of breaking the continuity of its capacity counts. |
| `tapelibrary_logical_libraries_last_refresh_timestamp_seconds` | Gauge | - | Unix time of the last successful logical libraries refresh. Alert if time() minus this exceeds 2x the collector's configured interval. |

## CleaningCartridgesCollector

Defined in `internal/collector/cleaning_cartridges.go`, the background-refresh
variant: a goroutine polls `GET /v1/cleaningCartridges` on
`--collector.cleaning_cartridges.interval` and every scrape serves the last
cached result. It reports how much cleaning capacity the library has left, and
which cartridge is running out of it.

**A drive that cannot be cleaned degrades and eventually fails, and the library
gives no other warning.** `/v1/drives` reports a `lastCleaned` timestamp with no
threshold attached, which is why `DrivesCollector` deliberately ships no rule on
it. The cleaning supply is the grounded signal instead: the legacy scripts
alerted on its summed `cleansRemaining` at 100 and 30 for years, and those two
thresholds are carried across verbatim — the only measured thresholds in this
exporter that did not have to be re-derived.

**`volser` is not a unique key on this endpoint, and every series is keyed on
`volser` AND `location`.** The 2026-07-28 capture holds 70 cartridges under only
63 distinct volsers: seven barcodes appear twice, at different locations, with
different `cleansRemaining`. R1.11.2's own worked example for this endpoint shows
two `CLNI01L1` entries side by side and states that `internalAddress` is the
tie-breaker when VOLSERs are duplicated. Duplicate barcodes are an ordinary fact
of a tape library's life, not a fault. This is the rule `NodeCardsCollector`
established for its own `location` + `card_type` pair: a key is whatever makes an
object unique on its own endpoint, verified against the capture. The parser
rejects a duplicate of the pair, because two metrics sharing a descriptor and a
label set fail `Gather` for the whole scrape, every other collector included.
The legacy scripts did emit those duplicates, which a textfile scrape tolerated
and `client_golang` will not.

**`internalAddress` is deliberately not collected**, despite being the manual's
nominated tie-breaker and genuinely unique. R1.11.2 documents it as changing
whenever the cartridge is assigned, unassigned or moved, so as a label it would
churn a fresh series out of every move while identifying nothing an operator can
act on. `location` is unique across the whole capture, stable while the cartridge
sits still, and can be pasted straight into the GUI.

**An empty array is accepted, and this is the only collector in the exporter
where that is true.** Every other one treats `[]` as a response that lost its
content, because a TS4500 always has at least one frame, accessor, LCC and
partition. It does not always have a cleaning cartridge: they are consumable,
exported once exhausted, and a library running out of them is precisely what
`CleaningCartridgesExhausted` pages on. Rejecting `[]` would keep serving a
comfortable list of cartridges that no longer exist at the moment the alert
needed to fire.

**The active state rides on the measurement series rather than on a
per-cartridge stateset.** At 70 cartridges a full stateset would cost 210 series
to say what the three `tapelibrary_cleaning_cartridges` counts already say. The
library-wide counts are emitted for every documented state whether or not any
cartridge is in it, so a rule matching `state="normal"` still has a series to
read when the count reaches zero — an absent series satisfies no matcher.

**`tapelibrary_cleaning_cartridges_usable` is deliberately narrower than the
`state="normal"` count.** A cartridge with no cleans left is still reported, still
occupies a slot and still counts in its state, but it cannot clean a drive; nor
can one queued for export or mid-import. On the 2026-07-28 capture that is the
difference between 70 present and 69 usable.

**`--collector.cleaning_cartridges.per-volser` defaults to `true`**, the inverse
of `--collector.drives.per-volser`, and it is the only per-item detail in this
exporter that ships on. The population is bounded by the site's cleaning policy
(70 here) rather than by library capacity (9 749 data cartridges), and a cleaning
cartridge sits in one slot until used or exported, so its volser does not churn
the way a drive's loaded volser does. The flag is a pure cardinality lever: it
gates the two per-cartridge families and nothing else, so turning it off costs
the ability to name the exhausted cartridge but never silences an alert.

| Metric | Type | Labels | Description |
|---|---|---|---|
| `tapelibrary_cleaning_cartridge_cleans_remaining` | Gauge | `volser`, `location`, `state`, `access`, `media_type` | Number of clean operations left on this cleaning cartridge. Emitted only when `--collector.cleaning_cartridges.per-volser` is set, which it is by default. Keyed by volser AND location: a barcode is not unique in a tape library, and this endpoint legitimately reports two cartridges under one volser. |
| `tapelibrary_cleaning_cartridge_last_usage_timestamp_seconds` | Gauge | `volser`, `location` | Unix time at which this cleaning cartridge was last mounted into a drive. A cartridge the library reports no usage for produces no series at all, rather than a 0 that would place its last clean in 1970. Emitted only when `--collector.cleaning_cartridges.per-volser` is set. |
| `tapelibrary_cleaning_cartridges` | Gauge | `state` | Number of cleaning cartridges the library holds in each documented state. Always emitted, one series per known state, so a library holding none at all reports 0 rather than dropping the series. |
| `tapelibrary_cleaning_cartridges_cleans_remaining` | Gauge | - | Total clean operations left across every cleaning cartridge in the library. Always emitted, independently of `--collector.cleaning_cartridges.per-volser`, so turning that flag off costs per-cartridge detail but never the alert this value drives. |
| `tapelibrary_cleaning_cartridges_usable` | Gauge | - | Number of cleaning cartridges that are both in the normal state and have at least one clean left. Reaching 0 means the library can no longer clean a drive, whatever the raw cartridge count says. |
| `tapelibrary_cleaning_cartridges_last_refresh_timestamp_seconds` | Gauge | - | Unix time of the last successful cleaning cartridges refresh. Alert if time() minus this exceeds 2x the collector's configured interval. |

## DataCartridgesCollector

Defined in `internal/collector/data_cartridges.go`, the background-refresh
variant: a goroutine polls `GET /v1/dataCartridges` on
`--collector.data_cartridges.interval` and every scrape serves the last cached
result. It reports what the library holds, how it is spread across partitions
and media, and how much useful life is left in it.

**This is the largest endpoint the exporter reads** — 9 749 cartridges on this
library, returned unpaginated over the slow SCSI/LCC-backed path — and the whole
collector is shaped by that. Everything emitted by default is an aggregate;
per-cartridge detail sits behind `--collector.data_cartridges.per-volser`, off
by default, because turning it on costs roughly 29 250 series per library and
five times that across the fleet.

**Its timeout is the highest in the exporter and its interval the longest**,
at `60s` and `15m`; `SlotsCollector` is the only other one to depart from the
shared 5s/5m, at `30s`/`15m`. Both follow from the
endpoint rather than from preference: 5s does not fetch 9 749 unpaginated
entries, so a 5s default would ship a collector that fails every refresh, and at
a ceiling of one in-flight request per library a refresh this long holds the
slot against its seventeen siblings while it runs. A cartridge inventory turns
over in hours, so it is polled less often rather than more. The queueing that
remains is visible in `tapelibrary_exporter_request_wait_seconds`.

**45% of the cartridges in the 2026-07-28 capture report no cartridge memory at
all.** 27 of 60 return `null` for `type`, `worm`, `vendor`, `sn`, `format`,
`nativeCapacity` **and `lifetimeRemaining`** simultaneously, while still
reporting volser, state, location, mediaType and mostRecentUsage. That is the
endpoint's ordinary shape, not an edge case, and it drives two decisions:

- `cartridge_type`, `worm` and `encrypted` carry an `unknown` value for the
  library's null, so every typed breakdown still sums to the real cartridge
  count. A panel showing 33 JD cartridges out of a parc of 60, with nothing
  explaining the missing 27, is what this avoids. The token cannot collide:
  cartridge types are 2-character barcode codes, `worm` is `true`/`false` and
  `encrypted` is `yes`/`no`.
- **A null `lifetimeRemaining` is kept out of the histogram entirely** and
  counted in `tapelibrary_data_cartridges_lifetime_unknown` instead. R1.11.2
  defines 0% as "at risk of data loss, may not be covered by warranty", so
  observing a missing reading as 0 would report nearly half a library's parc as
  spent and page on it. `_count` plus `lifetime_unknown` is the full parc; the
  histogram alone is not, which is precisely why that companion gauge exists.

**A null `logicalLibrary` becomes `logical_library="unassigned"`, not `""`.**
R1.11.2 documents that null as "not assigned to a logical library" — a
documented value, so it becomes a named member of the enumeration, following the
rule `DrivesCollector`'s `operation` established for a null the manual gives a
meaning to. An unassigned cartridge is exactly what the `assignmentRequired`
state reports on, so its count has to be readable. `DrivesCollector` carries the
same label and would decode its null to `""`, but all 40 drives in the capture
are assigned, so that path has never run: this collector is where the convention
is fixed.

**`volser` is not a unique key on this endpoint, and every per-cartridge series
is keyed on `volser` AND `location`.** R1.11.2 states that `internalAddress` is
the tie-breaker "if there are duplicate VOLSERs", and the sibling
`cleaningCartridges` capture proves the case is real rather than theoretical (70
cartridges under 63 volsers). The 60-entry `dataCartridges` trim happens to carry
60 distinct volsers, but a 60-entry sample of a 9 749-entry inventory proves
nothing about the full set. Getting this wrong here is worse than anywhere else:
two metrics sharing a descriptor and a label set fail `Gather` for the whole
scrape, so a duplicate barcode on the largest endpoint would take out all
eighteen collectors at once. `internalAddress` itself is deliberately not
collected — the manual documents it as changing on every assignment and move, so
it would churn a fresh series out of each one.

**An empty array is rejected**, unlike `CleaningCartridgesCollector`, the only
collector here that accepts one. The asymmetry is the resource: cleaning
cartridges are consumable and a library legitimately runs out of them, which is
what `CleaningCartridgesExhausted` pages on, whereas a library reporting zero
data cartridges has lost its entire inventory or truncated the body. Keeping the
previous cache and letting the freshness gauge go stale is the safer reading, and
it is visible rather than silent: before the first successful refresh there is no
cache, so the collector emits only its freshness gauge at 0.

**The state family is counted per partition rather than emitted as a
per-cartridge stateset.** At 9 749 cartridges a stateset would cost 87 741 series
to say what these 27 say. Every documented state is emitted for every partition
the response mentions, whether or not any cartridge is in it, so a rule matching
`state="atEndOfLife"` still has a series to read when the last spent cartridge is
migrated out. Two further states, `cartridgeFailedMove` and
`errorThresholdExceeded`, are named in the manual's prose under
`library.cartridgeDegraded` but tabulated nowhere; they are not pre-emitted,
because on this collector each entry costs one series per partition rather than
one outright. Should either appear, it becomes its own series and a logged
warning.

The `_ratio` histogram's buckets are bunched at the bottom of the range —
`0, 0.1, 0.2, 0.3, 0.5, 0.75, 0.9, 1` — because the operational question is how
many cartridges are close to end of life, never how many sit at 55% versus 60%.
`le="0"` answers "how many are AT end of life" exactly, and `le="0.2"` is what
`DataCartridgesWearingOut` reads.

| Metric | Type | Labels | Description |
|---|---|---|---|
| `tapelibrary_data_cartridges` | Gauge | `state`, `logical_library` | Number of data cartridges the library holds in each documented state, per logical library. Every documented state is emitted for every partition seen, whether or not any cartridge is in it, so a rule matching a state still has a series to read when its count reaches zero. A cartridge assigned to no partition is counted under `logical_library="unassigned"`. |
| `tapelibrary_data_cartridges_media` | Gauge | `media_type`, `cartridge_type` | Number of data cartridges of each media and cartridge type. Only combinations the library actually reports are emitted: the manual documents 28 cartridge type codes and a site runs one or two. A cartridge whose type the library has not read from cartridge memory is counted under `cartridge_type="unknown"` rather than dropped, so these counts always sum to the library's real cartridge count. |
| `tapelibrary_data_cartridges_encryption` | Gauge | `encrypted` | Number of data cartridges by encryption state. Always emitted for all three values. `encrypted="unknown"` is the manual's null, which it defines as a cartridge that has not been mounted, and is a distinct fact from `no`: nothing is known about the cartridge's contents either way. |
| `tapelibrary_data_cartridges_worm` | Gauge | `worm` | Number of data cartridges by write-once (WORM) status. Always emitted for all three values. `worm="unknown"` is a cartridge whose cartridge memory the library has not read. |
| `tapelibrary_data_cartridges_access` | Gauge | `access` | Number of data cartridges by accessor reach. Always emitted for all three documented values. On a dual-accessor library a non-zero `access="limited"` or `access="no"` count is the only inventory-scale signal that a robotics fault has put part of the cartridge parc out of reach. |
| `tapelibrary_data_cartridges_lifetime_unknown` | Gauge | - | Number of data cartridges for which the library reports no remaining-life reading. Always emitted. These are excluded from the lifetime histogram entirely, so this value plus that histogram's `_count` is the library's full cartridge parc; without it, the histogram would read as covering every cartridge when it can cover only those whose cartridge memory has been read. |
| `tapelibrary_data_cartridges_lifetime_remaining_ratio` | Histogram | - | Distribution of remaining media life across the library's data cartridges, as a ratio from 0 (at end of life, at risk of data loss and out of warranty) to 1 (unused). Converted from the API's 0-100 percentage. Cartridges the library reports no reading for are counted in `tapelibrary_data_cartridges_lifetime_unknown` instead of being observed here as 0, which would falsely report them as spent. |
| `tapelibrary_data_cartridge_lifetime_remaining_ratio` | Gauge | `volser`, `location` | Remaining media life of this individual data cartridge, as a ratio from 0 to 1. Emitted only when `--collector.data_cartridges.per-volser` is set, which it is not by default. A cartridge the library reports no reading for produces no series at all, rather than a 0 that would falsely mark it spent. Keyed by volser AND location: a barcode is not unique in a tape library. |
| `tapelibrary_data_cartridge_last_usage_timestamp_seconds` | Gauge | `volser`, `location` | Unix time at which this data cartridge was last mounted into a drive. A cartridge the library reports no usage for produces no series at all, rather than a 0 that would place its last mount in 1970. Emitted only when `--collector.data_cartridges.per-volser` is set. |
| `tapelibrary_data_cartridge_info` | Gauge | `volser`, `location`, `state`, `logical_library`, `media_type`, `cartridge_type`, `access`, `encrypted`, `worm` | Always 1. Carries this data cartridge's current state, partition and identity as labels, joinable to the per-cartridge measurements on volser and location. Emitted only when `--collector.data_cartridges.per-volser` is set. |
| `tapelibrary_data_cartridges_last_refresh_timestamp_seconds` | Gauge | - | Unix time of the last successful data cartridges refresh. Alert if time() minus this exceeds 2x the collector's configured interval. |

## SlotsCollector

Defined in `internal/collector/slots.go`, the background-refresh variant: a
goroutine polls `GET /v1/slots` on `--collector.slots.interval` and every
scrape serves the last cached result. It reports how much of the library's
physical cartridge capacity is in use, how much of it the robot may actually
target, and how hard the robotics is having to work to reach it.

**A slot entry is a column, not a cartridge position**, and this is the one
thing to get right before reading anything else here. `/v1/slots` returns a
location with no tier suffix (`slot_F2C1R1`) plus a `contents` array holding
one entry per tier, whereas the same physical position seen from
`/v1/dataCartridges` carries the suffix (`slot_F7C3R15T1`). The 2026-07-28
capture holds 79 entries covering 199 positions, so counting entries instead
of positions would report a library roughly 60% smaller than it is. Every
capacity figure below is counted over positions. The corollary for queries:
`tapelibrary_slot_info`'s `location` does **not** join to a cartridge location
from `DataCartridgesCollector` without a tier suffix being appended first.

**`tapelibrary_slots_positions_available` is the reason this collector earns
its place beside `LibraryCollector`.** That collector already reports total,
licensed and used capacity, so "the library is full" is alertable without
this one. What it cannot say is that a free position sits in a slot the robot
may not target: R1.11.2 defines `inServiceMode` as a state in which a slot
"cannot be selected as a cartridge destination but cartridges can be moved
from the slot". The available gauge counts only free positions in `normal`
slots, and on the eight-slot fixture that is 5 rather than the 6 a naive
free-position count would report. `SlotsNearlyFull` reads it as a ratio
against the total rather than against a literal, following
`LogicalLibraryDrivesBelowFloor` and `FCPortSpeedBelowPeers`: a tape library
runs full, so an absolute floor would be a number nobody here has measured.

**The three robotics counters are summed across slots rather than emitted per
slot**, which is what keeps the default build at 13 series regardless of
library size. The cost is stated rather than hidden, in each help text: a sum
over a set that can shrink reads as a counter reset when a frame is removed.
That happens during a service action, not during a week of operation, and the
alternative is 3 series per slot in every deployment.

`putRetries` is 0 on all 79 slots of the capture against 25 023 lifetime
puts, which makes the put-retry **ratio** a rule grounded in observation
rather than in a chosen number. `getRetries` has no such denominator — this
endpoint reports no `gets` counter at all — so `SlotGetRetryRateHigh` is an
absolute rate and is recorded as a re-derived default in
`docs/exporter-journal.md`.

**Per-slot detail sits behind `--collector.slots.per-slot`, off by default.**
The population is bounded by library capacity rather than by any policy. The
capture is a 79-entry trim of an unknown whole, so the real entry count is not
measured: the library reports 10 732 cartridge positions, which at this
fleet's mix of 1- and 4-tier slots is somewhere near 4 300 entries and so
roughly 21 500 extra series per library. Turning the flag on adds detail and
silences nothing — `TestSlotsCollector_PerSlotKeepsAggregatesIdentical` pins
that every aggregate an alert reads is byte-identical either way.

The active state rides on `tapelibrary_slot_info` rather than on a per-slot
stateset. That is the cardinality budget's tens-versus-thousands rule: a full
stateset over every slot would cost twice the entry count to say what one
label already says, and nothing selects on a slot's state per slot.

`tapelibrary_slots_positions_unreadable` is speculative and says so. R1.11.2
documents the `unknown` VOLSER token on `/v1/ioStations` and not on this
endpoint, so on firmware that never reports it the gauge sits at a permanent
0. It is handled anyway on `IOStationsCollector`'s terms: such a cartridge
occupies its position, carries no volser, and is therefore invisible to every
other collector in this exporter.

| Metric | Type | Labels | Description |
|---|---|---|---|
| `tapelibrary_slots` | Gauge | `state` | Number of storage slots in each documented state. A slot is a column holding one or more stacked tiers, not a single cartridge position: use `tapelibrary_slots_positions` for capacity. Both documented states are always emitted, so a rule matching one still has a series to read when its count reaches zero. |
| `tapelibrary_slots_positions` | Gauge | `state` | Number of cartridge positions across the library's storage slots, by the state of the slot holding them. One high-density slot contributes up to five positions. Both documented states are always emitted. |
| `tapelibrary_slots_positions_occupied` | Gauge | `state` | Number of cartridge positions holding a cartridge, by the state of the slot holding them. Cartridges whose barcode the library could not read are counted here too, and again in `tapelibrary_slots_positions_unreadable`. Both documented states are always emitted. |
| `tapelibrary_slots_positions_available` | Gauge | - | Number of empty cartridge positions the library can actually put a cartridge into: free positions in slots whose state is normal. Positions in a slot placed in service mode are excluded, because R1.11.2 defines that state as one the robot may not select as a destination even though cartridges can still be moved out of it. This is the figure `SlotsNearlyFull` reads, and it is not derivable from `LibraryCollector`'s capacity gauges, which know nothing about slot state. |
| `tapelibrary_slots_positions_unreadable` | Gauge | - | Number of occupied cartridge positions whose cartridge barcode the library could not read (reported as an unknown VOLSER). A subset of `tapelibrary_slots_positions_occupied`. Such a cartridge occupies its position and carries no volser, so it appears in no other collector in this exporter. R1.11.2 documents the unknown token on the I/O station endpoint and not on this one, so on firmware that never reports it this gauge stays at 0. |
| `tapelibrary_slots_depth` | Gauge | `tiers` | Number of storage slots of each tier depth: 1 for a single-deep slot, up to 5 for a high-density one. Only depths the library actually reports are emitted, since nothing alerts on a particular depth. This is the library's physical geometry, so it changes only when a frame is added or removed. |
| `tapelibrary_slots_puts_total` | Counter | - | Total number of times a cartridge has been placed into a storage slot over the lifetime of the library's slots, summed across every slot. Summed rather than emitted per slot to keep the default build affordable, so removing a frame drops its slots out of the sum and reads as a counter reset. This is the denominator of the put-retry ratio `SlotPutRetryRateHigh` reads. |
| `tapelibrary_slots_put_retries_total` | Counter | - | Total number of retries required while placing a cartridge into a storage slot, over the lifetime of the library's slots, summed across every slot. Zero on all 79 slots of the 2026-07-28 capture against 25 023 lifetime puts, so any sustained non-zero rate against `tapelibrary_slots_puts_total` is a robotics degradation signal rather than normal wear. Summed, so removing a frame reads as a counter reset. |
| `tapelibrary_slots_get_retries_total` | Counter | - | Total number of retries required while getting a cartridge from a storage slot, over the lifetime of the library's slots, summed across every slot. This endpoint reports no matching gets counter, so unlike the put side there is no denominator available and no ratio can be formed. Summed, so removing a frame reads as a counter reset. |
| `tapelibrary_slot_positions_occupied` | Gauge | `location` | Number of tiers in this individual storage slot holding a cartridge. Emitted only when `--collector.slots.per-slot` is set, which it is not by default. The slot's total depth is on `tapelibrary_slot_info`'s tiers label. |
| `tapelibrary_slot_puts_total` | Counter | `location` | Number of times a cartridge has been placed into this individual storage slot over its lifetime. Emitted only when `--collector.slots.per-slot` is set. |
| `tapelibrary_slot_put_retries_total` | Counter | `location` | Number of retries required while placing a cartridge into this individual storage slot, over its lifetime. Emitted only when `--collector.slots.per-slot` is set: this is what names the slot the library-wide retry rate is coming from. |
| `tapelibrary_slot_get_retries_total` | Counter | `location` | Number of retries required while getting a cartridge from this individual storage slot, over its lifetime. Emitted only when `--collector.slots.per-slot` is set. |
| `tapelibrary_slot_info` | Gauge | `location`, `state`, `tiers` | Always 1. Carries this storage slot's current state and its physical tier depth as labels, joinable to the per-slot measurements on location. The active state rides here rather than on a per-slot stateset: at library scale a full stateset over every slot is what the cardinality budget rules out. Emitted only when `--collector.slots.per-slot` is set. Note that this location has no tier suffix, so joining it to a cartridge location from `DataCartridgesCollector` needs one appended. |
| `tapelibrary_slots_last_refresh_timestamp_seconds` | Gauge | - | Unix time of the last successful slots refresh. Alert if time() minus this exceeds 2x the collector's configured interval. |

## EventsCollector

Defined in `internal/collector/events.go`.

The only collector here that reads a log rather than an inventory of hardware,
which changes what its numbers mean. Every gauge below covers a **trailing
window** — the last `--collector.events.lookback`, one hour by default — and is
recomputed from scratch at every refresh, so a count falls back to zero once
the events that raised it age out. They are not counters and no `rate()` should
be taken over them: the library reports no cumulative event total, and
reconstructing one in the exporter would reset at every restart.

R1.11.2 is explicit that a bare `GET /v1/events` returns *every* event the
library has ever recorded, so the request is always bounded by that lookback.
The window is what makes `tapelibrary_events{severity="error"} > 0` read as
"an error was raised recently", which is what the shipped rules match.

Two of the five severities mean the opposite of what their name suggests:
`inactiveError` and `inactiveWarning` mean **resolved**. Never match them as
active conditions, and prefer an exact `severity="error"` to any regex that
would sweep them in.

Three fields of the endpoint are deliberately not exposed. `state` and
`description` are interpolated free text (R1.11.2 documents values such as
`Assigned PMR <PMR number>. Service action required`), so either would carry a
PMR number straight into the label cardinality; `user` and `location` are
unbounded in principle and answer neither "how bad" nor "when". The event's
`ID` is what an operator takes to `GET /v1/events/<ID>` after an alert fires.

| Metric | Type | Labels | Description |
|---|---|---|---|
| `tapelibrary_events` | Gauge | `severity` | Number of events of this severity the library raised within the collector's lookback window. All five documented severities are always emitted, zero included, so a rule reading `> 0` has a series from the first scrape rather than only after the library's first error. A severity the manual does not document surfaces as its own extra series rather than being folded in or dropped, and an event whose severity the library reported as null is counted under `unknown` so the breakdown still sums to the number of events returned. |
| `tapelibrary_events_last_seen_timestamp_seconds` | Gauge | `severity` | Unix time of the most recent event of this severity within the lookback window. Absent, never zero, when that severity raised nothing in the window: zero would assert an event at the Unix epoch and silently corrupt every "within the last N" query rather than merely being missing from it. An event whose timestamp the library reports unparseably still counts on `tapelibrary_events` but contributes no reading here. |
| `tapelibrary_events_by_code` | Gauge | `severity`, `error_code` | Number of events carrying this library error code within the lookback window. Emitted only for codes named in `--collector.events.error-codes`, which is empty by default: `errorCode` is a 4-digit hex field the manual never enumerates, so naming codes explicitly is the only bounded form. Only codes actually observed get a series, so absence means the code did not occur in the window rather than that it is unwatched. Matching is case-insensitive; the label carries the library's own spelling. |
| `tapelibrary_events_last_refresh_timestamp_seconds` | Gauge | - | Unix time of the last successful events refresh. Alert if time() minus this exceeds 2x the collector's configured interval. Note that this reports on the *fetch*, not on the window: a library whose clock trails this host by more than the lookback returns an empty window on every successful refresh, so a permanently quiet `tapelibrary_events` alongside a fresh timestamp is worth checking against clock skew. |

## DataCartridgesLifetimeCollector

Defined in `internal/collector/data_cartridges_lifetime.go`, the
background-refresh variant: a goroutine polls
`GET /v1/dataCartridges/lifetimeMetrics` on
`--collector.data_cartridges_lifetime.interval` and every scrape serves the last
cached result. It reports how much work every data cartridge has done over its
life, and how many errors it took doing it.

**Its metrics are prefixed `..._data_cartridges_usage_`, not
`..._data_cartridges_lifetime_`, and that is deliberate.** This is the one
collector in the exporter whose metric subsystem differs from its registered
name. `DataCartridgesCollector` already owns the `..._data_cartridges_lifetime_`
prefix, where it means media life **remaining**
(`tapelibrary_data_cartridges_lifetime_remaining_ratio` and
`..._lifetime_unknown`). This collector reads the opposite quantity from a
different endpoint on a different schedule: lifetime work **done**. Two
collectors under one prefix meaning two opposed things is a trap for anyone
reading a dashboard, so the prefix says which of the two it is. The flag
namespace still follows the endpoint: `--collector.data_cartridges_lifetime.*`.

**These are the only genuine device counters the library hands over**, which is
why the per-cartridge series here are the exporter's only `CounterValue` metrics
with a `_total` suffix — `rate()` and `increase()` are meaningful on them for as
long as a cartridge stays in the library. The library-wide view of the same data
cannot be a counter, because a parc gains and loses cartridges, so it ships as
seven histograms over the same population instead.

**Its timeout and interval match `DataCartridgesCollector` at `60s` and `15m`**,
taken outright rather than derived. This endpoint walks the *same* population —
one entry per cartridge, 9 749 on this library, unpaginated over the same slow
SCSI/LCC path — and reads each cartridge's own memory to do it. It returns fewer
bytes per entry (~280 against ~640 in the reference capture), but the bytes are
not what makes it slow; the inventory walk is, and that walk is identical.

**Two distinct ways a cartridge can have no usable reading, counted apart.**
`tapelibrary_data_cartridges_usage_unknown` carries a `reason` label because
| `tapelibrary_data_cartridges_usage_duplicate_volsers` | Gauge | - | Number of barcodes this endpoint reported on more than one cartridge. Normally 0, and anything above it is an operational fault rather than a reading: two cartridges sharing a barcode cannot be told apart by a human either, and this endpoint reports no `location` to separate them. Those cartridges still count towards every aggregate here; only their per-cartridge series are withheld, since two metrics sharing a descriptor and a label set would fail the whole scrape. `DataCartridgeDuplicateVolser` reads this. |
merging them would hide the one that matters:

- `reason="unread"` — the library has not read the cartridge's memory, so all
  seven counters return `null` together. **39% of the reference capture** (27 of
  69), the same cartridge-memory-absent cohort `DataCartridgesCollector`
  documents at 45%. This is the endpoint's ordinary shape, not a fault, and
  nothing should alert on it.
- `reason="invalid"` — the library returned something this collector will not
  trust. The capture contains one: a cartridge reporting `motionMeters`
  **-285 211 648** beside 50 mounts and 0 bytes written. Negative is impossible
  on a cumulative counter, and read as unsigned 32-bit it would be four million
  kilometres of tape on a cartridge mounted fifty times. Cartridge memory that
  wrong about one field has not earned trust on the other six, so **one bad
  counter disqualifies the whole record** — which is what keeps a single
  invariant true for every family at once:

  ```
  <family>_count + usage_unknown{reason="unread"} + usage_unknown{reason="invalid"} = the parc
  ```

  Clamping the negative to `0` instead would assert that a mounted cartridge has
  never moved; passing it through would drag the histogram's `_sum` negative and
  make `rate()` on it meaningless.

**`le="0"` on the error family is load-bearing.** The number of cartridges
carrying at least one error of a given kind is `_count` minus that bucket, which
is the quantity `DataCartridgeUncorrectedErrorsRising` reads. The top bound
(`10000`) sits deliberately *below* the capture's observed maximum of 16 161, so
a cartridge that bad lands in `+Inf` and is findable rather than buried.

`dataWrittenToCartridge` is documented by R1.11.2 as "Number of MB"; it is
converted to bytes decimally (1 MB = 1e6), matching how this media's capacity is
advertised.

**`volser` alone keys the per-cartridge series here**, unlike
`DataCartridgesCollector` and `CleaningCartridgesCollector`, which key on volser
*and* location. This endpoint reports no location, so that pair does not exist,
and the parser rejects a duplicate volser outright rather than emitting two
metrics with the same label set — which would fail `Gather` for the whole scrape,
across every collector and every library. `internalAddress` is parsed (so the
error names both offending cartridges) but never labelled: R1.11.2 documents it
as changing on every move and every re-assignment, so it would churn a fresh
series out of every robot move while naming nothing an operator can act on. Join
these to `tapelibrary_data_cartridge_info` on `volser` for state, location and
partition.

| Metric | Type | Labels | Description |
|---|---|---|---|
| `tapelibrary_data_cartridges_usage_motion_meters` | Histogram | - | Distribution of metres of tape drawn across the drive head over each data cartridge's lifetime. Always emitted. Cartridges the library reports no usable reading for are counted in `tapelibrary_data_cartridges_usage_unknown` instead of being observed here, so this histogram's `_count` plus that gauge is the library's full cartridge parc. |
| `tapelibrary_data_cartridges_usage_mounts` | Histogram | - | Distribution of the number of times each data cartridge has been loaded into a drive over its lifetime. Always emitted. Cartridges with no usable reading are counted in `tapelibrary_data_cartridges_usage_unknown` instead. |
| `tapelibrary_data_cartridges_usage_written_bytes` | Histogram | - | Distribution of bytes written to each data cartridge over its lifetime. Converted from the API's megabytes, read decimally (1 MB = 1e6 bytes) to match how this media's capacity is advertised. Always emitted. Cartridges with no usable reading are counted in `tapelibrary_data_cartridges_usage_unknown` instead. |
| `tapelibrary_data_cartridges_usage_errors` | Histogram | `direction`, `correction` | Distribution of lifetime error counts across the library's data cartridges, by transfer direction and by whether the library was able to correct them. Always emitted for all four combinations. The number of cartridges carrying at least one error of a given kind is `_count` minus the `le="0"` bucket; a rising uncorrected count is media loss in progress. |
| `tapelibrary_data_cartridges_usage_unknown` | Gauge | `reason` | Number of data cartridges excluded from the usage histograms, by reason. Always emitted for both reasons. `reason="unread"` is a cartridge whose memory the library has not read (all seven counters null at once — the endpoint's ordinary shape, 39% of the reference capture). `reason="invalid"` is a cartridge that reported a negative counter or a partial record, which is a cartridge-memory fault worth investigating. |
| `tapelibrary_data_cartridge_usage_motion_meters_total` | Counter | `volser` | Metres of tape drawn across the drive head over this individual cartridge's lifetime, as the library's own cumulative counter. Emitted only when `--collector.data_cartridges_lifetime.per-volser` is set, which it is not by default. A cartridge with no usable reading produces no series at all, rather than a 0 that would assert it has never moved. |
| `tapelibrary_data_cartridge_usage_mounts_total` | Counter | `volser` | Number of times this individual cartridge has been loaded into a drive over its lifetime. Emitted only under `--collector.data_cartridges_lifetime.per-volser`. Absent, never zero, for a cartridge with no usable reading. |
| `tapelibrary_data_cartridge_usage_written_bytes_total` | Counter | `volser` | Bytes written to this individual cartridge over its lifetime, converted from the API's decimal megabytes. Emitted only under `--collector.data_cartridges_lifetime.per-volser`. Absent, never zero, for a cartridge with no usable reading. |
| `tapelibrary_data_cartridge_usage_errors_total` | Counter | `volser`, `direction`, `correction` | Lifetime error count for this individual cartridge, by transfer direction and by whether the library corrected them. Emitted only under `--collector.data_cartridges_lifetime.per-volser`, and then for all four combinations. Absent, never zero, for a cartridge with no usable reading. |
| `tapelibrary_data_cartridges_usage_last_refresh_timestamp_seconds` | Gauge | - | Unix time of the last successful data cartridges lifetime metrics refresh. Alert if time() minus this exceeds 2x the collector's configured interval. Named for this collector's metric subsystem rather than its registered name, so a subsystem sweep finds the freshness of the data it is reading. |

## ReportsLibraryCollector

Defined in `internal/collector/reports_library.go`, the background-refresh
variant: a goroutine polls `GET /v1/reports/library` on
`--collector.reports_library.interval` and every scrape serves the last cached
result. It reports the library's own hourly activity and environmental
report — mounts, moves, imports, exports, host I/O volumes, and the
temperature and humidity its drives measured.

**Its metrics are prefixed `..._library_report_`, not `..._reports_library_`.**
The subsystem names the resource being reported on (the library) with `report`
as the qualifier, matching how every other per-resource family in this exporter
is already spelled (`tapelibrary_drive_state`,
`tapelibrary_accessor_humidity_ratio`). The two sibling collectors still to be
built take the same shape as `..._drive_report_` and `..._accessor_report_`.
The flag namespace still follows the endpoint:
`--collector.reports_library.*`. This is the second collector whose metric
subsystem differs from its registered name, after
`DataCartridgesLifetimeCollector`, and for an unrelated reason: that one moved
to avoid a prefix collision, this one to keep the resource at the front of the
name.

**Every metric here is a Gauge, including the seven activity figures, and that
is not an oversight.** These are per-window quantities: `mounts` is the number
of mounts during one specific hour, and the next window restarts the count from
zero. `rate()` and `increase()` are meaningless on them and a `_total` suffix
would be a lie. The library's genuinely monotonic device counters live on
`/v1/accessors`, `/v1/slots` and `/v1/dataCartridges/lifetimeMetrics` instead.

**The exporter exposes the newest window only, selected by the entry's own
`time` field rather than by its position in the array.** R1.11.2 documents no
ordering for this endpoint; the reference capture happens to arrive newest
first, and a collector that depended on that would report week-old activity as
current the first time a firmware release changed it. A window whose timestamp
cannot be parsed is skipped for selection rather than ranked as a zero time,
and a response in which no window carries a parseable timestamp is rejected
outright, leaving the previous cache in place.

**`tapelibrary_library_report_window_timestamp_seconds` is the guard that makes
the rest of this family trustworthy.** The values are a snapshot of an
already-closed window served at scrape time, with Prometheus's
`honor_timestamps` deliberately not used (see `docs/exporter-journal.md`, "Open
questions"). A library that stopped publishing new windows would otherwise
serve its last one forever and look perfectly healthy, with a *current*
`..._last_refresh_timestamp_seconds` to match — the refresh really is
succeeding; it is the data behind it that has stopped moving. Those two gauges
answer different questions and both ship for that reason.

**Temperature and humidity are six metrics rather than one carrying a
`stat="average|min|max"` label.** Prometheus's own exporter guidance is that a
metric should make sense when summed or averaged across its labels, and summing
an average with a minimum and a maximum is meaningless — the same reason its
naming guidance prefers separate families over a
`{result="success"|"failure"}` label. Six names also let an operating-envelope
rule name the statistic it means in the metric rather than in a matcher.

**These readings are taken at the drives, inside the library, so they read
above the ambient figures R1.11.2's operating envelope is written against**
(allowable 16–32 °C and 20–80% RH; recommended 16–25 °C and 20–50% RH, which
the manual states for the customer-supplied ambient sensors outside the
enclosure). The reference capture's healthy library sits at ~25 °C average and
~28.5 °C maximum, already above the *recommended* ambient ceiling, which is why
the shipped rules in `monitoring/prometheus/alerts.yml` read against the
*allowable* bound and not the recommended one. An absent reading emits no
series at all rather than a `0`, since a 0 °C / 0% RH would sit outside the
envelope in both directions and page on a library that is merely quiet about
its sensors.

The three data-volume fields are documented by R1.11.2 as "the number of MB"
without saying whether the MB is decimal or binary; they are converted to bytes
decimally (1 MB = 1e6), matching `DataCartridgesLifetimeCollector`, so that if
IBM means MiB both families are 4.9% low together rather than disagreeing.
`written_by_hosts` divided by `written_to_cartridges` is the window's average
compression ratio, which is how the manual itself describes the pair.

| Metric | Type | Labels | Description |
|---|---|---|---|
| `tapelibrary_library_report_mounts` | Gauge | - | Number of cartridges mounted into a drive during the reporting window. A per-window figure, not a cumulative counter: the next window restarts from zero. |
| `tapelibrary_library_report_imports` | Gauge | - | Number of cartridges added to the library during the reporting window. R1.11.2 counts an import only once the host has issued the SCSI move media command or the cartridge was manually assigned to a logical library. |
| `tapelibrary_library_report_exports` | Gauge | - | Number of cartridges removed from the library during the reporting window, counted once the cartridge has physically reached the I/O station. |
| `tapelibrary_library_report_moves` | Gauge | - | Number of times a cartridge was moved from one location to another during the reporting window, host-initiated and library-initiated alike. Each move is one get plus one put by the gripper, and the figure includes mounts, demounts, imports and exports. Cartridges shuffled aside to reach a deeper tier are not counted. |
| `tapelibrary_library_report_read_by_hosts_bytes` | Gauge | - | Bytes read from cartridges by all drives during the reporting window. Converted from the API's megabytes, read decimally (1 MB = 1e6 bytes). |
| `tapelibrary_library_report_written_by_hosts_bytes` | Gauge | - | Bytes written to cartridges by all drives during the reporting window, measured before compression. Divide by `tapelibrary_library_report_written_to_cartridges_bytes` for the window's average compression ratio. Converted from the API's decimal megabytes. |
| `tapelibrary_library_report_written_to_cartridges_bytes` | Gauge | - | Bytes actually written onto the media by all drives during the reporting window, after compression. Converted from the API's decimal megabytes. |
| `tapelibrary_library_report_temperature_average_celsius` | Gauge | - | Average temperature in Celsius across all drives over the reporting window. Measured inside the library at the drives, so it reads above the ambient figure R1.11.2's operating envelope is written against. Absent, never zero, when no drive reported a reading. |
| `tapelibrary_library_report_temperature_min_celsius` | Gauge | - | Lowest temperature in Celsius reported by any drive over the reporting window. Absent, never zero, when no drive reported a reading. |
| `tapelibrary_library_report_temperature_max_celsius` | Gauge | - | Highest temperature in Celsius reported by any drive over the reporting window. Absent, never zero, when no drive reported a reading. |
| `tapelibrary_library_report_humidity_average_ratio` | Gauge | - | Average relative humidity across all drives over the reporting window, as a ratio from 0 to 1 (the API reports a 0-100 percentage). Absent, never zero, when no drive reported a reading. |
| `tapelibrary_library_report_humidity_min_ratio` | Gauge | - | Lowest relative humidity reported by any drive over the reporting window, as a ratio from 0 to 1. Absent, never zero, when no drive reported a reading. |
| `tapelibrary_library_report_humidity_max_ratio` | Gauge | - | Highest relative humidity reported by any drive over the reporting window, as a ratio from 0 to 1. Absent, never zero, when no drive reported a reading. |
| `tapelibrary_library_report_window_timestamp_seconds` | Gauge | - | Unix time the library stamped on the reporting window these metrics describe. The library publishes one window per completed hour, so alert if time() minus this exceeds a few hours: every other metric in this family would otherwise keep serving a stale window's values indefinitely, looking healthy. |
| `tapelibrary_library_report_window_duration_seconds` | Gauge | - | Number of seconds the reporting window covers, as the library reports it. 3600 on every window in the reference capture. Exposed so that a window covering less than a full hour is visible rather than assumed away: its activity figures would be proportionally low through no fault of the library. |
| `tapelibrary_library_report_last_refresh_timestamp_seconds` | Gauge | - | Unix time of the last successful reports/library refresh. Alert if time() minus this exceeds 2x the collector's configured interval. Named for this collector's metric subsystem rather than its registered name, so a subsystem sweep finds the freshness of the data it is reading. Distinct from `tapelibrary_library_report_window_timestamp_seconds`, which ages even while this one stays current. |

## ReportsDrivesCollector

Defined in `internal/collector/reports_drives.go`, the background-refresh
variant: a goroutine polls `GET /v1/reports/drives` on
`--collector.reports_drives.interval` and every scrape serves the last cached
result. It is `ReportsLibraryCollector`'s per-drive counterpart — the same
hourly publication, resolved to the individual drive rather than aggregated
over the library — and adds the error figures the library-wide report does not
carry at all.

**Its metrics are prefixed `..._drive_report_`, not `..._reports_drives_`.**
The subsystem names the resource being reported on (the drive) with `report` as
the qualifier, the shape `ReportsLibraryCollector` established, and it matches
how every other per-drive family here is already spelled
(`tapelibrary_drive_state`, `tapelibrary_drive_info`). The flag namespace still
follows the endpoint: `--collector.reports_drives.*`.

**`location` is the key, and the join to the rest of the exporter.** It carries
the library's own native drive location (`drive_F1C4R1`) verbatim, so every
series here lines up with `tapelibrary_drive_state`, `tapelibrary_drive_info`
and `tapelibrary_fc_port_*`'s `drive_location`. The endpoint also returns each
drive's serial as `sn`; it is deliberately **not** emitted, because
`tapelibrary_drive_info` already carries it against the same key and an
identity attribute belongs on exactly one `_info` series.

**Every metric here is a Gauge, including the three error figures.** These are
per-window quantities: `errorsCorrectedRead` counts the read errors this drive
corrected during one specific hour, and the next window restarts from zero.
`rate()` and `increase()` are meaningless on them and a `_total` suffix would
be a lie. The library's genuinely monotonic error counters live on
`/v1/dataCartridges/lifetimeMetrics`, per cartridge rather than per drive.

**The errors are three metric names rather than one carrying a `direction`
label,** which is where this collector deliberately parts company with
`DataCartridgesLifetimeCollector`'s `{direction, correction}` pair. Prometheus's
own [exporter-writing guidance](https://prometheus.io/docs/instrumenting/writing_exporters)
names read/write as its canonical example of related-but-distinct concepts that
are easier to use as separate metrics than as one metric with a label. That
collector's cross product is complete — corrected and uncorrected, each read
and write — which is what justifies the labelled histogram there. This endpoint
reports `errorsUncorrected` with no direction breakdown at all, so a `direction`
label here would need a synthetic value the library never sends, and summing
across it would double-count.

**The window selection is per drive, not library-wide.** All 40 drives share
the newest window in the reference capture, but a drive that was removed, went
offline, or was installed part-way through the week has its own last reported
hour. Selection is by the entry's own `time` field rather than by its position
in the array, for the reason `ReportsLibraryCollector` already documents. An
entry missing a location or a parseable timestamp is skipped rather than
failing the response — one unusable hour out of a week must not discard the
other 167 for every drive — while a response with nothing selectable at all,
or an empty array, is rejected and leaves the previous cache in place.

**`tapelibrary_drive_report_window_timestamp_seconds` is per drive for exactly
that reason.** The values are a snapshot of an already-closed window served at
scrape time, with `honor_timestamps` deliberately not used (see
`docs/exporter-journal.md`, "Open questions"), so a drive that stopped
publishing would otherwise serve its last window forever with a *current*
`..._last_refresh_timestamp_seconds` beside it — the refresh really is
succeeding; it is the data behind it that has stopped moving. A library-wide
timestamp would hide a single dropped-out drive behind its 39 healthy siblings.

**This is the most expensive endpoint per byte in the exporter, and its
defaults reflect that.** The default week is ~168 windows x 40 drives, roughly
3.3 MB against `reports/library`'s ~67 KB for the same week, and the
concurrency ceiling of 1 means that transfer blocks this library's other
collectors while it runs. The interval is therefore **1h**, matching the
endpoint's own publication cadence rather than quartering it as
`reports_library` does at 15m, and the timeout is **60s**, in line with
`data_cartridges` and `data_cartridges_lifetime`. The request carries no
`after` parameter: bounding it would trade ~3.3 MB for a clock-skew failure
mode in which a library running ahead of the exporter's host answers with an
empty array, which presents as a collector that silently stops advancing.

Temperature and humidity are six metrics rather than one carrying a
`stat="average|min|max"` label, and the readings are taken at the drive, inside
the library, so they read above the ambient figures R1.11.2's operating
envelope is written against — both points are argued at length under
`ReportsLibraryCollector` above and hold identically here. An absent reading
emits no series at all rather than a `0`, per drive, so one sensorless drive
never collapses the family for the others.

| Metric | Type | Labels | Description |
|---|---|---|---|
| `tapelibrary_drive_report_mounts` | Gauge | `location` | Number of cartridges mounted into this drive during the reporting window. A per-window figure, not a cumulative counter: the next window restarts from zero. |
| `tapelibrary_drive_report_cleans` | Gauge | `location` | Number of times this drive was cleaned during the reporting window. A per-window figure, not a cumulative counter. Zero in every window of the reference capture: a drive requests cleaning rarely, so a window recording one is the event worth looking at. |
| `tapelibrary_drive_report_read_by_hosts_bytes` | Gauge | `location` | Bytes read from cartridges by this drive during the reporting window. Converted from the API's megabytes, read decimally (1 MB = 1e6 bytes) to match how this exporter already converts the library report and the cartridge lifetime counters. |
| `tapelibrary_drive_report_written_by_hosts_bytes` | Gauge | `location` | Bytes written to cartridges by this drive during the reporting window, measured before compression. Divide by `tapelibrary_drive_report_written_to_cartridges_bytes` for this drive's average compression ratio over the window. Converted from the API's decimal megabytes. |
| `tapelibrary_drive_report_written_to_cartridges_bytes` | Gauge | `location` | Bytes this drive actually wrote onto the media during the reporting window, after compression. Converted from the API's decimal megabytes. |
| `tapelibrary_drive_report_errors_corrected_read` | Gauge | `location` | Read errors this drive corrected during the reporting window. A corrected error cost throughput but lost no data; a drive whose corrected count runs far above its peers is the classic early signature of a failing head or a dirty tape path. Compare against `tapelibrary_drive_report_read_by_hosts_bytes` before reading a high count as a fault, since a busy drive corrects more. |
| `tapelibrary_drive_report_errors_corrected_write` | Gauge | `location` | Write errors this drive corrected during the reporting window, typically by rewriting the affected block further along the tape. Compare against `tapelibrary_drive_report_written_by_hosts_bytes` before reading a high count as a fault. |
| `tapelibrary_drive_report_errors_uncorrected` | Gauge | `location` | Errors this drive could not correct during the reporting window, read and write together: R1.11.2 reports no direction breakdown for these, unlike the corrected pair. Any non-zero value is data the drive failed to move, and is what `DriveReportUncorrectedErrors` reads. |
| `tapelibrary_drive_report_temperature_average_celsius` | Gauge | `location` | Average temperature in Celsius this drive measured over the reporting window. Measured inside the library at the drive, so it reads above the ambient figure R1.11.2's operating envelope is written against. Absent, never zero, when the drive reported no reading. |
| `tapelibrary_drive_report_temperature_min_celsius` | Gauge | `location` | Lowest temperature in Celsius this drive measured over the reporting window. Absent, never zero, when the drive reported no reading. |
| `tapelibrary_drive_report_temperature_max_celsius` | Gauge | `location` | Highest temperature in Celsius this drive measured over the reporting window. Absent, never zero, when the drive reported no reading. |
| `tapelibrary_drive_report_humidity_average_ratio` | Gauge | `location` | Average relative humidity this drive measured over the reporting window, as a ratio from 0 to 1 (the API reports a 0-100 percentage). Absent, never zero, when the drive reported no reading. |
| `tapelibrary_drive_report_humidity_min_ratio` | Gauge | `location` | Lowest relative humidity this drive measured over the reporting window, as a ratio from 0 to 1. Absent, never zero, when the drive reported no reading. |
| `tapelibrary_drive_report_humidity_max_ratio` | Gauge | `location` | Highest relative humidity this drive measured over the reporting window, as a ratio from 0 to 1. Absent, never zero, when the drive reported no reading. |
| `tapelibrary_drive_report_window_timestamp_seconds` | Gauge | `location` | Unix time the library stamped on the reporting window these metrics describe, for this drive. Per drive rather than library-wide so that a single drive dropping out of the report is visible: the library publishes one window per completed hour, so alert if time() minus this exceeds a few hours. Every other metric in this family would otherwise keep serving a stale window's values indefinitely, looking healthy. |
| `tapelibrary_drive_report_window_duration_seconds` | Gauge | `location` | Number of seconds this drive's reporting window covers, as the library reports it. 3600 on every window in the reference capture. Exposed so that a window covering less than a full hour is visible rather than assumed away: its activity figures would be proportionally low through no fault of the drive. |
| `tapelibrary_drive_report_last_refresh_timestamp_seconds` | Gauge | - | Unix time of the last successful reports/drives refresh. Alert if time() minus this exceeds 2x the collector's configured interval. Named for this collector's metric subsystem rather than its registered name, so a subsystem sweep finds the freshness of the data it is reading. Distinct from `tapelibrary_drive_report_window_timestamp_seconds`, which ages even while this one stays current. |

## ReportsAccessorsCollector

Defined in `internal/collector/reports_accessors.go`, the background-refresh
variant: a goroutine polls `GET /v1/reports/accessors` on
`--collector.reports_accessors.interval` and every scrape serves the last
cached result. It is the third and last of the `/v1/reports/*` family — the
same hourly publication as `ReportsLibraryCollector` and
`ReportsDrivesCollector`, resolved to the individual robotic accessor.

**Its metrics are prefixed `..._accessor_report_`, not
`..._reports_accessors_`.** The subsystem names the resource being reported on
(the accessor) with `report` as the qualifier, the shape
`ReportsLibraryCollector` established and `ReportsDrivesCollector` followed,
and it matches how every other per-accessor family here is already spelled
(`tapelibrary_accessor_state`, `tapelibrary_accessor_pivots_total`). The flag
namespace still follows the endpoint: `--collector.reports_accessors.*`.

**Every metric here is the per-window counterpart of a lifetime counter
`AccessorsCollector` already emits, and that pairing is the whole point of
this collector.** `/v1/accessors` reports `pivots`, `barCodeScans`, `travelX`,
`travelY` and the four gripper counters as monotonic device totals that have
accumulated into the millions over the machine's life; this endpoint reports
the same five quantities for one completed hour. A lifetime counter moves
imperceptibly when an accessor stops working — millions of gets, plus zero —
while the hourly window it stops contributing to drops to zero immediately.
Read the two together: `tapelibrary_accessor_gets_total` for wear,
`tapelibrary_accessor_report_gets` for whether the robot is working right now.

**Every metric here is therefore a Gauge, and none carries `_total`.** These
are per-window quantities: the next window restarts from zero, so `rate()` and
`increase()` are meaningless on them and a `_total` suffix would be a lie. The
`_total`-suffixed counterparts on `AccessorsCollector` are the monotonic ones.

**`gets` and `puts` are two metric names, while `gripper` and `axis` are
labels,** which is not a contradiction — the two follow the same test applied
to different fields. Prometheus's own
[exporter-writing guidance](https://prometheus.io/docs/instrumenting/writing_exporters)
names read/write and send/receive as its canonical example of
related-but-distinct concepts easier to use as separate metrics than under one
label; get/put is that shape. `gripper` passes the opposite test: R1.11.2
reports the complete cross product (`getsGripper1`, `getsGripper2`,
`putsGripper1`, `putsGripper2`), so summing across it is meaningful and no
synthetic value has to be invented to square the table. `axis` is complete for
the same reason (`travelX`, `travelY`). This mirrors how
`AccessorsCollector` already spells its lifetime counterparts.

**The window selection is per accessor, not library-wide.** Both accessors
share the newest window in the reference capture, but one taken into service
mode or removed part-way through the week has its own last reported hour. On a
two-accessor library that matters more than it does on a forty-drive one: the
sibling is not one of forty, it is the only other one. Selection is by the
entry's own `time` field rather than by its position in the array, for the
reason `ReportsLibraryCollector` already documents. An entry missing a location
or a parseable timestamp is skipped rather than failing the response, while a
response with nothing selectable at all, or an empty array, is rejected and
leaves the previous cache in place.

**`tapelibrary_accessor_report_window_timestamp_seconds` is per accessor for
exactly that reason.** The values are a snapshot of an already-closed window
served at scrape time, with `honor_timestamps` deliberately not used (see
`docs/exporter-journal.md`, "Open questions"), so an accessor that stopped
publishing would otherwise serve its last window forever with a *current*
`..._last_refresh_timestamp_seconds` beside it — the refresh really is
succeeding; it is the data behind it that has stopped moving.

**The six environmental metrics emit nothing on the reference fleet, and that
is the hardware rather than a defect.** These accessors carry no temperature or
humidity sensor and report `null` in every window, exactly as
`tapelibrary_accessor_temperature_celsius` and
`tapelibrary_accessor_humidity_ratio` already do on `/v1/accessors`. They are
shipped anyway, absent-never-zero, so a site whose accessors do carry sensors
gets them without a code change: a `0` °C / `0`% RH standing in for "unknown"
would sit outside R1.11.2's operating envelope in both directions. They are six
metrics rather than one carrying a `stat="average|min|max"` label, argued at
length under `ReportsLibraryCollector` above and holding identically here.

**Its defaults sit between its two siblings'.** The interval is **15m**,
matching `reports_library` rather than `reports_drives`' 1h: what pushed that
collector to the endpoint's own cadence was ~3.3 MB against the concurrency
ceiling of 1, and a library has two accessors, so the default week is ~168
windows x 2, roughly 148 KB. Quartering the hourly cadence costs little and
makes a freshly published window visible within 15 minutes rather than up to an
hour, which matters on the one endpoint whose alert is about an accessor having
stopped. The timeout is **60s** despite that small response, a deliberate
departure from `reports_library`'s 5s: the RoE path can be slow in ways payload
size does not predict, and a timeout that fires serves a permanently empty
cache rather than late data. The request carries no `after` parameter, for the
clock-skew reason its siblings document.

| Metric | Type | Labels | Description |
|---|---|---|---|
| `tapelibrary_accessor_report_pivots` | Gauge | `location` | Number of pivots this accessor performed during the reporting window. A per-window figure, not a cumulative counter: the next window restarts from zero. Its lifetime counterpart is `tapelibrary_accessor_pivots_total`. |
| `tapelibrary_accessor_report_bar_code_scans` | Gauge | `location` | Number of bar code scans this accessor performed during the reporting window. A per-window figure, not a cumulative counter. Zero in every window of the reference capture: this fleet scans on inventory rather than on every move, so a window recording scans is an inventory pass. Its lifetime counterpart is `tapelibrary_accessor_bar_code_scans_total`. |
| `tapelibrary_accessor_report_travel_meters` | Gauge | `location`, `axis` | Distance in meters this accessor travelled during the reporting window, per axis: x is horizontal, y is vertical. A per-window figure, not a cumulative counter. Its lifetime counterpart is `tapelibrary_accessor_travel_meters_total`. |
| `tapelibrary_accessor_report_gets` | Gauge | `location`, `gripper` | Number of times this accessor's gripper engaged to retrieve a cartridge during the reporting window. A per-window figure, not a cumulative counter. Compare the two accessors' shares of the library total: a lifetime counter cannot show one of them stopping, which is what `AccessorReportShareCollapsed` reads. Its lifetime counterpart is `tapelibrary_accessor_gets_total`. |
| `tapelibrary_accessor_report_puts` | Gauge | `location`, `gripper` | Number of times this accessor's gripper engaged to place a cartridge during the reporting window. A per-window figure, not a cumulative counter. Normally tracks gets closely, since a cartridge retrieved is a cartridge put somewhere. Its lifetime counterpart is `tapelibrary_accessor_puts_total`. |
| `tapelibrary_accessor_report_temperature_average_celsius` | Gauge | `location` | Average temperature in Celsius this accessor measured over the reporting window. Absent, never zero, when the accessor reported no reading: the accessors on the reference fleet carry no such sensor and report null in every window, exactly as `tapelibrary_accessor_temperature_celsius` does. |
| `tapelibrary_accessor_report_temperature_min_celsius` | Gauge | `location` | Lowest temperature in Celsius this accessor measured over the reporting window. Absent, never zero, when the accessor reported no reading. |
| `tapelibrary_accessor_report_temperature_max_celsius` | Gauge | `location` | Highest temperature in Celsius this accessor measured over the reporting window. Absent, never zero, when the accessor reported no reading. |
| `tapelibrary_accessor_report_humidity_average_ratio` | Gauge | `location` | Average relative humidity this accessor measured over the reporting window, as a ratio from 0 to 1 (the API reports a 0-100 percentage). Absent, never zero, when the accessor reported no reading. |
| `tapelibrary_accessor_report_humidity_min_ratio` | Gauge | `location` | Lowest relative humidity this accessor measured over the reporting window, as a ratio from 0 to 1. Absent, never zero, when the accessor reported no reading. |
| `tapelibrary_accessor_report_humidity_max_ratio` | Gauge | `location` | Highest relative humidity this accessor measured over the reporting window, as a ratio from 0 to 1. Absent, never zero, when the accessor reported no reading. |
| `tapelibrary_accessor_report_window_timestamp_seconds` | Gauge | `location` | Unix time the library stamped on the reporting window these metrics describe, for this accessor. Per accessor rather than library-wide so that one of the two dropping out of the report is visible: the library publishes one window per completed hour, so alert if time() minus this exceeds a few hours. Every other metric in this family would otherwise keep serving a stale window's values indefinitely, looking healthy. |
| `tapelibrary_accessor_report_window_duration_seconds` | Gauge | `location` | Number of seconds this accessor's reporting window covers, as the library reports it. 3600 on every window in the reference capture. Exposed so that a window covering less than a full hour is visible rather than assumed away: its activity figures would be proportionally low through no fault of the accessor. |
| `tapelibrary_accessor_report_last_refresh_timestamp_seconds` | Gauge | - | Unix time of the last successful reports/accessors refresh. Alert if time() minus this exceeds 2x the collector's configured interval. Named for this collector's metric subsystem rather than its registered name, so a subsystem sweep finds the freshness of the data it is reading. Distinct from `tapelibrary_accessor_report_window_timestamp_seconds`, which ages even while this one stays current. |

## DiagnosticCartridgesCollector

Defined in `internal/collector/diagnostic_cartridges.go`, the background-refresh
variant: a goroutine polls `GET /v1/diagnosticCartridges` on
`--collector.diagnostic_cartridges.interval` and every scrape serves the last
cached result.

**What it exists to answer is "can the library still diagnose itself".** A
diagnostic cartridge is never read or written by a host, so none of the
throughput or media-error questions the data-cartridge collectors ask apply
here. The one that does is availability, and it is invisible until the day a
service action needs a cartridge and finds none usable — at which point the fix
is ordering media, not something an engineer on site can do.
`tapelibrary_diagnostic_cartridges_usable` is that signal.

**`usable` is deliberately narrower than the `normal` state count.** A
cartridge whose state is `normal` but which the accessor cannot reach
(`accessible` reading `no`) is one the library cannot select, so it is excluded.
Reading the state count alone would report a healthy supply in exactly the
situation where nothing can be picked up.

**The full stateset is affordable here and is not a precedent.** Five cartridges
against R1.11.2's five documented states costs 25 series, so the state count is
emitted per state library-wide rather than collapsed. `DataCartridgesCollector`
carries the active state as a label on its `_info` instead, because the same
shape over 9 749 cartridges would cost tens of thousands of series. The rule is
in `docs/exporter-journal.md`'s cardinality budget: full statesets where objects
number in the tens, active-state-only where they number in the thousands.

**`volser` is emitted by default**, making this the second collector to do so
after `CleaningCartridgesCollector`, on the same justification rather than a new
one: the population is bounded by service policy rather than by library
capacity, and at five cartridges the argument is stronger than where it was
first made. `--collector.diagnostic_cartridges.per-volser` turns the three
per-cartridge families off; every library-wide aggregate an alert reads is
emitted regardless, so the flag costs the ability to name *which* cartridge to
pull and nothing else.

**Most of these cartridges report no cartridge memory at all.** Three of the
five in the reference capture return `null` for `type`, `vendor`, `sn`, `worm`,
`format` and `lifetimeRemaining` simultaneously, while still reporting volser,
state, accessible, location and mediaType. That is the same pattern
`DataCartridgesCollector` documents, handled the same way: a nullable **label**
takes the literal token `unknown`, and a nullable **measurement** emits no
series at all. `tapelibrary_diagnostic_cartridges_lifetime_unknown` counts the
second case, so the remaining-life series are read as covering a subset rather
than the whole population. The token is deliberately not applied to `state`,
whose documented set already contains `unknown`.

**An empty response is a real reading of zero, not an error**, and this is the
one place this collector parts company with its cartridge siblings. A library
with no data cartridge cannot serve a host and one with no cleaning cartridge
cannot clean a drive, so both of those reject an empty array as a response that
lost its content. A library with no diagnostic cartridge is merely one nobody
has loaded a cartridge into — and zero is exactly what the exhaustion alert has
to be able to see.

`volser` is not a unique key on this endpoint any more than on the others: the
key is the `volser` + `location` pair, and the parser rejects a duplicate of the
pair. `internalAddress`, R1.11.2's nominated tie-breaker, stays off the wire
because the manual documents it as changing whenever a cartridge moves.

| Metric | Type | Labels | Description |
|---|---|---|---|
| `tapelibrary_diagnostic_cartridges` | Gauge | `state` | Number of diagnostic cartridges in each state, as a stateset over R1.11.2's five documented values plus any value actually observed. Always emitted, independently of `--collector.diagnostic_cartridges.per-volser`. Summing across state gives the library's whole diagnostic population. |
| `tapelibrary_diagnostic_cartridges_access` | Gauge | `access` | Number of diagnostic cartridges the accessor can reach, by access level. A cartridge behind a blocking position reads `limited` or `no` while its state stays `normal`, so this is a different question from the state count above and both are needed to explain a low usable figure. |
| `tapelibrary_diagnostic_cartridges_usable` | Gauge | - | Number of diagnostic cartridges the library could actually select for a service action right now: state `normal` AND reachable by the accessor. Deliberately narrower than the normal state count, because a cartridge the robot cannot reach is one it cannot use. Reaching 0 means the next service action requiring media will be blocked, and is what `DiagnosticCartridgesExhausted` reads. Always emitted, independently of the per-volser flag. |
| `tapelibrary_diagnostic_cartridges_lifetime_unknown` | Gauge | - | Number of diagnostic cartridges reporting no remaining-life reading at all, because the library has not read their cartridge memory. Three of the five in the reference capture, so a high value here is the ordinary state of this endpoint rather than a fault: it is published so that the remaining-life series below are read as covering a subset, never as covering the whole population. |
| `tapelibrary_diagnostic_cartridge_info` | Gauge | `volser`, `location`, `state`, `media_type`, `cartridge_type`, `access`, `worm` | Always 1. Carries this diagnostic cartridge's current state and identity as labels, joinable to the per-cartridge measurements on volser and location. Labels the library did not read carry the literal token `unknown` rather than an empty string. Emitted only when `--collector.diagnostic_cartridges.per-volser` is set. |
| `tapelibrary_diagnostic_cartridge_last_usage_timestamp_seconds` | Gauge | `volser`, `location` | Unix time this diagnostic cartridge was last mounted. Absent, never zero, when the library reports no usage timestamp or one that cannot be parsed: a 0 here would place the mount at the Unix epoch and quietly corrupt every query asking what has been used recently. Emitted only when `--collector.diagnostic_cartridges.per-volser` is set. |
| `tapelibrary_diagnostic_cartridge_lifetime_remaining_ratio` | Gauge | `volser`, `location` | Estimated media life left on this diagnostic cartridge, as a ratio from 0 to 1 (the API reports a 0-100 percentage). Absent, never zero, for a cartridge whose memory the library has not read, which is the majority case on this endpoint: a 0 would read as a cartridge at end of life. Count the absent ones with `tapelibrary_diagnostic_cartridges_lifetime_unknown`. Emitted only when `--collector.diagnostic_cartridges.per-volser` is set. |
| `tapelibrary_diagnostic_cartridges_last_refresh_timestamp_seconds` | Gauge | - | Unix time of the last successful diagnostic cartridges refresh. Alert if time() minus this exceeds 2x the collector's configured interval; `CollectorNeverRefreshed` and `CollectorRefreshStale` already do. |

## Self-instrumentation

Always registered on this target model, with no `--collector.*` flag gating
either row below: unlike a `single` build, `multi-instance` exposes no
per-metric enable/disable flag for its own self-instrumentation. See
[docs/configuration.md](configuration.md).

| Metric | Type | Labels | Description |
|---|---|---|---|
| `tapelibrary_exporter_request_duration_seconds` | Histogram | `outcome` | Duration in seconds of HTTP requests issued by this exporter's collectors, by outcome (`success` or `error`). Defined in `internal/collector/client.go`. |
| `tapelibrary_exporter_request_wait_seconds` | Histogram | `outcome` | Duration in seconds a request waited for a per-target concurrency slot, by outcome (`success` if a slot was granted, `error` if the caller's own deadline expired first). Both series stay at a permanent zero once `--exporter.max-requests-per-target` is unset (the default). Defined in `internal/collector/limiter.go`. |
| `tapelibrary_exporter_collector_success` | Gauge | `collector` | Whether the last scrape of the collector succeeded (1=success, 0=failure). Defined in `internal/collector/status_tracker.go`. |
| `tapelibrary_exporter_collector_duration_seconds` | Gauge | `collector` | Duration of the last scrape for the collector, in seconds. Defined in `internal/collector/status_tracker.go`. |

**`tapelibrary_exporter_build_info` is emitted too, and is deliberately not in
the table above.** That table is exactly what `internal/collector/*.go` defines,
which is the invariant `make docs-check` enforces; this one comes from
`client_golang`'s own version collector, registered in `cmd/*/main.go`, so
documenting it as a row would fail the check for a metric that is nonetheless
real. It is a Gauge, always `1`, carrying `version`, `revision`, `branch`,
`goversion`, `goos`, `goarch` and `tags` — this exporter's identity as stamped
by the Makefile's ldflags.

Distinct from `go_build_info`, which carries Go's module metadata as a
pseudo-version: `tapelibrary_exporter_build_info` is the one to join against in
a dashboard, and the one that answers "which build is running" without decoding
a commit hash out of a pseudo-version. Both are emitted regardless of
`--web.disable-exporter-metrics`, since identity is not runtime instrumentation.

## The instance label

This exporter watches every instance listed in its `--config.file` and serves
them all through one `/metrics`. Every collector's metrics and the
per-collector health metrics (`tapelibrary_exporter_collector_success` /
`_duration_seconds`) additionally carry the `library` label (plus any
per-instance labels you declare), applied by the exporter per instance rather
than by the collector, so it is not part of the collector's own descriptor and
`make docs-check` does not see it. The two exceptions are
`tapelibrary_exporter_request_duration_seconds` and
`tapelibrary_exporter_request_wait_seconds`: both single, process-wide
histograms shared across every instance (the concurrency ceiling itself is
per-instance, see `internal/instance/instance.go`'s `Handle`, but the
histogram recording it is not), so neither carries `library`; both
are labeled only by `outcome`. See [docs/configuration.md](configuration.md).

## Configuration reload metrics

Two gauges, defined in `internal/reload/reload.go`, a sibling package to
`internal/collector/`. Listed here as prose rather than a
`docs-check`-parsed table row on purpose: `make docs-check` (see this file's
own header comment) only scans `internal/collector/*.go`, so it can never
see `internal/reload`'s metrics either way; documenting them as a regular
table row would make `docs-check` report them as a lie against a directory
it was never told to look at.

- `tapelibrary_exporter_config_last_reload_successful` (Gauge, no labels):
  whether the last configuration reload attempt succeeded (`1`) or failed
  (`0`). Set to `1` before the server starts serving, so the gauge is
  meaningful from the very first scrape rather than absent until somebody
  reloads.
- `tapelibrary_exporter_config_last_reload_success_timestamp_seconds`
  (Gauge, no labels): Unix time of the last SUCCESSFUL configuration reload.

This target model always wires reload: `SIGHUP` needs no flag, and
`POST /-/reload` is available behind `--web.enable-lifecycle`. Neither gauge
carries the `library` label: a reload is a property of the
configuration file as a whole, not of any one watched instance. See
[docs/configuration.md](configuration.md) and `SECURITY.md`.
