# Configuration Reference

> Back to [README](../README.md)

## Usage

The exporter is configured through command-line flags. Optionally, a YAML file
passed via `--config.file` can set some of those same flags and add settings no
flag can express, such as authentication for outbound requests; see
[Configuration file](#configuration-file) below. Nothing changes for a
deployment that never passes `--config.file`.

**Basic execution, flags only:**

```bash
./tapelibrary_exporter --web.listen-address=":9170"
```

**Using a configuration file for web settings (TLS/Basic Auth):**

```bash
./tapelibrary_exporter --web.config.file=/path/to/web-config.yml
```

For the `web-config.yml` format, see
[Prometheus's TLS/Basic Auth configuration reference](https://prometheus.io/docs/prometheus/latest/configuration/https/).
`tapelibrary_exporter` parses this file with the same `prometheus/exporter-toolkit` library
Prometheus itself uses.

**View help and every available flag:**

```bash
./tapelibrary_exporter --help
```

### Command-line options

| Flag | Description | Default |
|------|-------------|---------|
| `--web.listen-address` | Address to listen on for web interface and telemetry | `:9170` |
| `--web.config.file` | Path to a configuration file for TLS/Basic Auth | (none) |
| `--config.file` | Path to this exporter's own YAML configuration file (see [Configuration file](#configuration-file)). Not the same thing as `--web.config.file`. **Required** for a multi-instance build (`--target-model multi-instance`); optional and empty by default everywhere else | (none) |
| `--log.level` | Log level: `debug`, `info`, `warn`, `error` | `info` |
| `--log.format` | Log format: `json`, `text` | `text` |
| `--web.disable-exporter-metrics` | Exclude Go runtime and process metrics from `/metrics` | `false` |
| `--[no-]collector.<name>` | *(single-target and multi-instance builds)* Enable or disable a collector (kingpin boolean flag) | see below |
| `--exporter.max-requests-per-target` | *(single-target and multi-instance builds only, absent on `multi`)* Bounds how many outbound requests/commands this exporter has in flight at once. `0` means unlimited. Scope differs by build; see [Bounding outbound concurrency](#bounding-outbound-concurrency) below | `0` |
| `--web.enable-lifecycle` | *(multi-target and multi-instance builds only)* Expose `POST /-/reload`, which reloads `--config.file`. `SIGHUP` always works and needs no flag; see [Configuration reload](#configuration-reload-multi-and-multi-instance-builds-only) below | `false` |
| `--probe.target-allowlist` | *(multi-target builds only, `--target-model multi`)* Restrict `/probe` to these target hosts (repeatable; host or host:port, exact match) | (empty = allow-any) |
| `--probe.timeout` | *(multi-target builds only)* Ceiling on each `/probe` request's own deadline | `5s` |
| `--probe.timeout-offset` | *(multi-target builds only)* Subtracted from Prometheus's scrape timeout when computing a probe's deadline, so the exporter answers before Prometheus abandons the scrape | `0.5s` |

This is a **multi-instance** build, so every collector is the background-refresh
variant: `--collector.<name>.timeout` bounds one request and
`--collector.<name>.interval` sets how often the poller refreshes, both applying
identically to every watched instance. There is no `--collector.<name>.target`,
since each instance's address comes from `instances:` in the configuration file
(see below) rather than from a flag, and `--[no-]collector.<name>` toggles the
collector on or off.

This build's own collectors follow that same multi-instance pattern:

| Flag | Description | Default |
|------|-------------|---------|
| `--collector.library.timeout` | Per-request timeout for the `library` collector | `5s` |
| `--collector.library.interval` | Background refresh interval for the `library` collector | `5m` |
| `--[no-]collector.library` | Enable or disable the `library` collector | enabled |
| `--collector.frames.timeout` | Per-request timeout for the `frames` collector | `5s` |
| `--collector.frames.interval` | Background refresh interval for the `frames` collector | `5m` |
| `--[no-]collector.frames` | Enable or disable the `frames` collector | enabled |
| `--collector.accessors.timeout` | Per-request timeout for the `accessors` collector | `5s` |
| `--collector.accessors.interval` | Background refresh interval for the `accessors` collector | `5m` |
| `--[no-]collector.accessors` | Enable or disable the `accessors` collector | enabled |
| `--collector.drives.timeout` | Per-request timeout for the `drives` collector | `5s` |
| `--collector.drives.interval` | Background refresh interval for the `drives` collector | `5m` |
| `--[no-]collector.drives` | Enable or disable the `drives` collector | enabled |
| `--collector.drives.per-volser` | Emit `tapelibrary_drive_loaded_cartridge_info`, labelling each drive with the cartridge it currently holds. Off by default: only 40 volsers are loaded at once, but the drive holding a given tape changes constantly, so `location` x `volser` accumulates an index entry in Prometheus for every pairing that has ever existed | `false` |
| `--collector.power_supplies.timeout` | Per-request timeout for the `power_supplies` collector | `5s` |
| `--collector.power_supplies.interval` | Background refresh interval for the `power_supplies` collector | `5m` |
| `--[no-]collector.power_supplies` | Enable or disable the `power_supplies` collector | enabled |
| `--collector.node_cards.timeout` | Per-request timeout for the `node_cards` collector | `5s` |
| `--collector.node_cards.interval` | Background refresh interval for the `node_cards` collector | `5m` |
| `--[no-]collector.node_cards` | Enable or disable the `node_cards` collector | enabled |
| `--collector.io_stations.timeout` | Per-request timeout for the `io_stations` collector | `5s` |
| `--collector.io_stations.interval` | Background refresh interval for the `io_stations` collector | `5m` |
| `--[no-]collector.io_stations` | Enable or disable the `io_stations` collector | enabled |
| `--collector.fc_ports.timeout` | Per-request timeout for the `fc_ports` collector | `5s` |
| `--collector.fc_ports.interval` | Background refresh interval for the `fc_ports` collector | `5m` |
| `--[no-]collector.fc_ports` | Enable or disable the `fc_ports` collector | enabled |
| `--collector.logical_libraries.timeout` | Per-request timeout for the `logical_libraries` collector | `5s` |
| `--collector.logical_libraries.interval` | Background refresh interval for the `logical_libraries` collector | `5m` |
| `--[no-]collector.logical_libraries` | Enable or disable the `logical_libraries` collector | enabled |
| `--collector.cleaning_cartridges.timeout` | Per-request timeout for the `cleaning_cartridges` collector | `5s` |
| `--collector.cleaning_cartridges.interval` | Background refresh interval for the `cleaning_cartridges` collector | `5m` |
| `--[no-]collector.cleaning_cartridges` | Enable or disable the `cleaning_cartridges` collector | enabled |
| `--collector.cleaning_cartridges.per-volser` | Emit one `cleans_remaining` gauge and one last-usage timestamp per cleaning cartridge. On by default, unlike `--collector.drives.per-volser`: the population is bounded by cleaning policy rather than library capacity, and only the per-cartridge series can name which cartridge to pull. Turning it off drops those two families and nothing else — the library-wide aggregates the supply alerts read are emitted either way | `true` |
| `--collector.data_cartridges.timeout` | Per-request timeout for the `data_cartridges` collector. **Higher than every other collector's**: `GET /v1/dataCartridges` returns the library's entire inventory (9 749 entries on this fleet) unpaginated over the slow SCSI/LCC path, which 5s does not fetch | `60s` |
| `--collector.data_cartridges.interval` | Background refresh interval for the `data_cartridges` collector. **Longer than most collectors', and shared with `slots`**: at a ceiling of one in-flight request per library, a refresh this size holds the slot against its siblings while it runs, and a cartridge inventory does not turn over in minutes | `15m` |
| `--[no-]collector.data_cartridges` | Enable or disable the `data_cartridges` collector | enabled |
| `--collector.data_cartridges.per-volser` | Emit one `_info`, one lifetime-remaining ratio and one last-usage timestamp per data cartridge. Off by default, like `--collector.drives.per-volser` and unlike the cleaning-cartridge flag: at ~9 750 cartridges per library this is ~29 250 extra series per library and ~146 000 across a five-library fleet, which is a Prometheus sizing decision rather than a monitoring one. Turning it on adds those three families and changes no aggregate, so no alert depends on it | `false` |
| `--collector.slots.timeout` | Per-request timeout for the `slots` collector. Higher than the shared `5s` but below `data_cartridges`: `GET /v1/slots` walks the whole slot inventory over the same slow path, but returns one entry per slot **column** (roughly 4 300) rather than one per cartridge | `30s` |
| `--collector.slots.interval` | Background refresh interval for the `slots` collector. Matched to `data_cartridges` rather than split between it and the shared `5m`: the two describe the same physical movement, so there is nothing to gain from learning about it twice as often on one endpoint as on the other | `15m` |
| `--[no-]collector.slots` | Enable or disable the `slots` collector | enabled |
| `--collector.slots.per-slot` | Emit one `_info`, one occupancy gauge and the three lifetime robotics counters per storage slot. Off by default, like `--collector.data_cartridges.per-volser`: at roughly 4 300 slots per library this is about 21 500 extra series per library and five times that across the fleet. Turning it on adds those five families and changes no aggregate, so no alert depends on it — it is what names *which* slot a rising retry rate comes from | `false` |
| `--collector.events.timeout` | Per-request timeout for the `events` collector. Back to the shared `5s`, because the lookback below makes this one of the cheapest endpoints rather than one of the heaviest: the reference capture is 42 events over 3h45m | `5s` |
| `--collector.events.interval` | Background refresh interval for the `events` collector | `5m` |
| `--[no-]collector.events` | Enable or disable the `events` collector | enabled |
| `--collector.events.lookback` | How far back the collector asks the library to look, sent as the endpoint's `after` parameter. **Not a tuning knob.** R1.11.2 states a bare `GET /v1/events` returns *every* event the library has recorded, so without this each refresh would re-download the whole history over the slow path. Must exceed `--collector.events.interval`, or events landing between two refreshes are never counted; the rest of the default is slack for clock skew between this host and the library, which would otherwise silently empty the window while refreshes kept succeeding | `1h` |
| `--collector.events.error-codes` | Comma-separated library error codes to break out as `tapelibrary_events_by_code`, e.g. `"B792,0217"`. Empty by default, which suppresses that family entirely: `errorCode` is a 4-digit hex field R1.11.2 never enumerates, so naming codes explicitly is the only bounded form. Matching is case-insensitive and the label carries the library's own spelling. The severity breakdown every shipped alert reads is emitted regardless | *(empty)* |
| `--collector.data_cartridges_lifetime.timeout` | Per-request timeout for the `data_cartridges_lifetime` collector. Takes `data_cartridges`' figure outright rather than deriving its own: this endpoint walks the *same* 9 749-cartridge inventory over the same slow path, reading each cartridge's memory. It returns fewer bytes per entry (~280 against ~640), but the bytes are not what makes it slow — the inventory walk is, and that walk is identical | `60s` |
| `--collector.data_cartridges_lifetime.interval` | Background refresh interval for the `data_cartridges_lifetime` collector. Matches `data_cartridges` for a second reason on top of the timeout's: the two describe the same cartridges, and at a ceiling of one in-flight request per library there is nothing to gain from polling one twice as often as the other. Lifetime counters move slower than inventory, if anything | `15m` |
| `--[no-]collector.data_cartridges_lifetime` | Enable or disable the `data_cartridges_lifetime` collector | enabled |
| `--collector.reports_library.timeout` | Per-request timeout for the `reports_library` collector. Stays at the common default: the response is the last week of hourly entries (~67 KB) and involves no inventory walk | `5s` |
| `--collector.reports_library.interval` | Background refresh interval for the `reports_library` collector. The first interval in this file chosen against the *data's* cadence rather than the endpoint's cost: R1.11.2 publishes one report per completed hour, so polling faster returns the same window again. A quarter of that bounds how long a freshly published window sits unseen without pretending the exporter can resolve anything finer | `15m` |
| `--[no-]collector.reports_library` | Enable or disable the `reports_library` collector | enabled |
| `--collector.reports_drives.timeout` | Per-request timeout for the `reports_drives` collector. Higher than the common default, in line with `data_cartridges`: the response is a week of hourly windows for *every* drive, ~3.3 MB on a 40-drive library against `reports_library`'s ~67 KB | `60s` |
| `--collector.reports_drives.interval` | Background refresh interval for the `reports_drives` collector. Matches the endpoint's own hourly publication cadence rather than quartering it as `reports_library` does, because here the repetition is expensive: re-transferring ~3.3 MB four times an hour to re-read the same 40 rows blocks this library's other collectors, the concurrency ceiling being 1. The cost is that a freshly published window can sit up to an hour unseen, which `tapelibrary_drive_report_window_timestamp_seconds` makes visible | `1h` |
| `--[no-]collector.reports_drives` | Enable or disable the `reports_drives` collector | enabled |
| `--collector.reports_accessors.timeout` | Per-request timeout for the `reports_accessors` collector. Generous relative to the ~148 KB response, and a deliberate departure from `reports_library`'s `5s`: the RoE path can be slow in ways payload size does not predict, and a timeout that fires leaves the cache empty rather than late | `60s` |
| `--collector.reports_accessors.interval` | Background refresh interval for the `reports_accessors` collector. Quarters the endpoint's hourly publication as `reports_library` does, rather than matching it as `reports_drives` does: a library has two accessors, so the default week is ~168 windows × 2, roughly 148 KB, and re-reading that costs little. The gain is that a freshly published window is visible within 15 minutes rather than up to an hour, which matters on the one endpoint whose alert is about an accessor having stopped | `15m` |
| `--[no-]collector.reports_accessors` | Enable or disable the `reports_accessors` collector | enabled |
| `--collector.diagnostic_cartridges.timeout` | Per-request timeout for the `diagnostic_cartridges` collector | `5s` |
| `--collector.diagnostic_cartridges.interval` | Background refresh interval for the `diagnostic_cartridges` collector. Deliberately unhurried: the population changes only when somebody physically loads or removes a cartridge | `5m` |
| `--collector.diagnostic_cartridges.per-volser` | Emit `tapelibrary_diagnostic_cartridge_info`, `..._last_usage_timestamp_seconds` and `..._lifetime_remaining_ratio`, one set per diagnostic cartridge. **On by default**, like `--collector.cleaning_cartridges.per-volser` and unlike the two data-cartridge flags: the population is bounded by service policy rather than by library capacity (five on the reference fleet), and only the per-cartridge series can name which cartridge to pull. Turning it off takes the collector from 23 series to 11 and silences no alert — both supply rules read library-wide aggregates emitted either way | `true` |
| `--[no-]collector.diagnostic_cartridges` | Enable or disable the `diagnostic_cartridges` collector | enabled |
| `--collector.data_cartridges_lifetime.per-volser` | Emit the four per-cartridge lifetime counter families (`_motion_meters_total`, `_mounts_total`, `_written_bytes_total` and the four `_errors_total` combinations), one set per data cartridge. Off by default, like `--collector.data_cartridges.per-volser`, and the largest per-object cost in the exporter: seven series per cartridge against that flag's three, so ~68 250 extra series per library and ~341 000 across a five-library fleet. Turning it on adds those four families and changes no aggregate, so no alert depends on it — it is what names *which* cartridge a rising uncorrected-error count comes from | `false` |

There is no single global command/request timeout: each collector owns its own, on the
pattern above, so a slow endpoint can be given room without loosening every other one.
Run `--help` to see the full, current flag list.

### Available collectors

| Collector | Default | Description |
|-----------|---------|-------------|
| `library` | enabled | Library status, capacity and cartridge counters, and identity, from `GET /v1/library` |
| `frames` | enabled | Per-frame state, door positions, slot/cartridge/drive/IO-station counts and identity, from `GET /v1/frames` |
| `accessors` | enabled | Per-accessor state, drive/cartridge reachability, and lifetime robotics counters, from `GET /v1/accessors` |
| `drives` | enabled | Per-drive state, current operation, accessor reachability, last-cleaned time and identity, from `GET /v1/drives` |
| `power_supplies` | enabled | Per-supply health state, from `GET /v1/powerSupplies`. Supplies sit in a redundant pair per frame, so one leaving `online` costs redundancy rather than service |
| `node_cards` | enabled | Per-card state, identity, last-restart time and LCC primary/reporting roles, from `GET /v1/nodeCards`. Keyed by `location` **and** `card_type`: an accessor carries two cards at one location |
| `io_stations` | enabled | Per-station state, door position and magazine occupancy, from `GET /v1/ioStations`. Cartridge VOLSERs are counted, never labelled; the magazine series are absent while no magazine is reported rather than zeroed |
| `fc_ports` | enabled | Per-port Fibre Channel link state, negotiated rate and SAN identity, from `GET /v1/fcPorts`. Keyed by `location` alone (the port number is baked into it); `drive_location` rides on the measurement series so a dark port can be joined against drive state |
| `logical_libraries` | enabled | Per-partition drive, virtual-slot, virtual-I/O-slot and cartridge counts plus identity, from `GET /v1/logicalLibraries`. The only collector with no stateset: the endpoint reports capacity, not health. Cartridges divided by virtual slots is the partition's saturation, and at 1 it refuses imports while the physical library still reports free slots |
| `cleaning_cartridges` | enabled | Cleaning supply: per-cartridge cleans-remaining and last-usage time plus library-wide counts by state, total cleans left and a usable count, from `GET /v1/cleaningCartridges`. Keyed by `volser` **and** `location` — a barcode is not unique, and this endpoint legitimately reports two cartridges under one. The only collector that accepts an empty array, because a library really can run out |
| `data_cartridges` | enabled | The cartridge inventory, aggregated: counts by state per partition, by media and cartridge type, by encryption, by WORM and by accessor reach, plus a remaining-media-life histogram, from `GET /v1/dataCartridges`. The largest endpoint the exporter reads, hence its own `60s`/`15m` defaults. 45% of cartridges report no cartridge memory at all, so `cartridge_type`, `worm` and `encrypted` carry an `unknown` value and a null `lifetimeRemaining` is counted separately rather than observed as 0 — the manual defines 0% as at risk of data loss. Per-cartridge detail is behind `--collector.data_cartridges.per-volser`, off by default |
| `slots` | enabled | Storage-slot capacity and robotics health, aggregated: slot and cartridge-position counts by state, the tier-depth distribution, and library-wide sums of the lifetime `puts`/`putRetries`/`getRetries` counters, from `GET /v1/slots`. A slot entry is a **column** holding up to five stacked tiers, and its `location` carries no tier suffix — unlike a cartridge location from `data_cartridges` — so capacity is counted over positions, never over entries. `tapelibrary_slots_positions_available` excludes free positions in service-mode slots, which the robot may not target, and is the one capacity figure `library` cannot produce. Per-slot detail is behind `--collector.slots.per-slot`, off by default |
| `events` | enabled | The library event log over a trailing window: counts by severity and the most recent event time per severity, from `GET /v1/events?after=…`. The only collector here that reads a log rather than hardware, so its gauges cover the last `--collector.events.lookback` and fall back to zero as events age out — never `rate()` them. `inactiveError` and `inactiveWarning` mean **resolved**, so alerting matches the bare severities exactly rather than by regex. `state`, `description`, `user` and `location` are deliberately not labelled: the first two are interpolated free text carrying PMR numbers, the other two are unbounded. Per-code detail is behind `--collector.events.error-codes`, empty by default |
| `data_cartridges_lifetime` | enabled | Per-cartridge lifetime usage counters, aggregated: distributions of tape motion, mounts, bytes written and the four error counters, from `GET /v1/dataCartridges/lifetimeMetrics`. The only endpoint handing over genuine monotonic device counters, so the per-cartridge series are the exporter's only `_total` counters. **Its metrics are prefixed `tapelibrary_data_cartridges_usage_`, not `..._lifetime_`** — `data_cartridges` already owns that prefix for media life *remaining*, and this reads work *done*. Walks the same 9 749-cartridge inventory as `data_cartridges`, hence the same `60s`/`15m` defaults. A cartridge with no usable reading is counted under `..._usage_unknown{reason}` rather than observed as 0: `unread` (no cartridge memory, 39% of the reference capture, benign) and `invalid` (a negative or partial counter — the capture holds one cartridge reporting `motionMeters` of -285 211 648) are counted apart. Per-cartridge detail is behind `--collector.data_cartridges_lifetime.per-volser`, off by default |
| `reports_library` | enabled | The library's own hourly activity and environmental report, newest completed window only, from `GET /v1/reports/library`. **Its metrics are prefixed `tapelibrary_library_report_`, not `..._reports_library_`** — the subsystem names the resource with `report` as the qualifier, the shape the two sibling `reports_*` collectors will follow. Every metric is a Gauge, including the activity figures: these are per-window quantities that restart from zero each hour, so `rate()` is meaningless on them. The window is chosen by its own `time` field rather than by array position, since the manual documents no ordering. `..._window_timestamp_seconds` is the guard that matters: a library that stops publishing windows keeps serving its last one at full *refresh* freshness, and only that gauge ages. Temperature and humidity are six separate metrics rather than a `stat` label, and are absent rather than zero when no drive reported |
| `reports_drives` | enabled | The same hourly publication as `reports_library`, resolved to the individual drive, from `GET /v1/reports/drives`, plus the per-drive error figures the library-wide report does not carry at all. **Its metrics are prefixed `tapelibrary_drive_report_`**, keyed by `location`, which joins to `tapelibrary_drive_info` and `tapelibrary_drive_state`. The drive's serial is deliberately not re-emitted: `tapelibrary_drive_info` already carries it against the same key. The three error figures are three metric names rather than one carrying a `direction` label, following Prometheus's own exporter guidance, which names read/write as the canonical case for separate metrics; the endpoint reports `errorsUncorrected` with no direction breakdown anyway. Window selection is **per drive**, so a drive that went offline part-way through the week keeps its own last reported hour instead of being dropped, and `..._window_timestamp_seconds` is per drive for the same reason: a single drive falling out of the report is exactly what a library-wide timestamp would hide. This is the most expensive endpoint per byte in the exporter — see its two flags above |
| `reports_accessors` | enabled | The same hourly publication as its two `reports_*` siblings, resolved to the individual robotic accessor, from `GET /v1/reports/accessors`. **Its metrics are prefixed `tapelibrary_accessor_report_`**, keyed by `location`, which joins to `tapelibrary_accessor_state` and `tapelibrary_accessor_info`. Every metric here is the **per-window counterpart of a lifetime counter `accessors` already emits** — that pairing is the point: `tapelibrary_accessor_gets_total` has accumulated into the millions and barely moves when an accessor stops, while the hourly window it stops contributing to drops to zero at once. All are therefore Gauges with no `_total` suffix. `gets` and `puts` are two metric names (Prometheus's own guidance on read/write-shaped pairs) while `gripper` and `axis` are labels, because the library reports those cross products completely. Window selection is **per accessor**, and `..._window_timestamp_seconds` is per accessor for the same reason. The six environmental metrics emit nothing on this fleet: these accessors carry no temperature or humidity sensor and report null in every window, exactly as `/v1/accessors` does — they are shipped absent-never-zero so hardware that does report them needs no code change |
| `diagnostic_cartridges` | enabled | The cartridges the library keeps for its own service actions, from `GET /v1/diagnosticCartridges`. Never read or written by a host, so the only question worth alerting on is availability — `tapelibrary_diagnostic_cartridges_usable` intersects state `normal` with accessor reachability, because a cartridge the robot cannot reach is one the library cannot select. **An empty response is a real reading of zero here, not an error**, unlike on every other cartridge endpoint: a library with no diagnostic cartridge is one nobody has loaded a cartridge into, and zero is exactly what `DiagnosticCartridgesExhausted` must be able to see. Carries a full per-state stateset, which the cardinality budget permits at five cartridges and forbids at 9 749 — `data_cartridges` carries state as an `_info` label for that reason. Most of these cartridges report no cartridge memory at all (three of five in the reference capture), so nullable labels take the token `unknown` and nullable measurements emit nothing; `tapelibrary_diagnostic_cartridges_lifetime_unknown` counts the second case |
| `http_client_requests` *(HTTP flavor)* | enabled | Self-instrumentation: HTTP request duration by outcome |
| `command_exec` *(CLI flavor)* | enabled | Self-instrumentation: command execution duration by outcome |

Collectors and the self-instrumentation histogram are registered through the same
`--[no-]collector.<name>` mechanism: there is nothing special about self-instrumentation
from the flag's point of view.

### Enabling and disabling collectors

Use `--[no-]collector.<name>` (kingpin boolean syntax) to enable or disable a collector.

**Example: disable the self-instrumentation collector**

```bash
./tapelibrary_exporter --no-collector.http_client_requests
```

**Example: custom timeout and logging**

```bash
./tapelibrary_exporter \
  --collector.drives.timeout=10s \
  --log.level=debug \
  --log.format=json
```

## Configuration file

`--config.file` optionally points at a YAML file that sets flag values and,
for an HTTP-flavor exporter, the outbound authentication and TLS settings no
flag can express. It is **empty by default**, so nothing in this section
changes anything for a deployment that never passes `--config.file`.

```bash
./tapelibrary_exporter --config.file=config.example.yml
```

A commented, ready-to-copy example ships at the repo root:
[`config.example.yml`](../config.example.yml).

**`--config.file` is not `--web.config.file`.** `--config.file` configures
what this exporter does: its own flags, and how it authenticates to whatever
it queries. `--web.config.file` is a separate flag, from
`prometheus/exporter-toolkit`, that configures the TLS/Basic Auth the
exporter's own HTTP server presents to Prometheus. They govern opposite
directions of the same deployment and neither substitutes for the other.

The file has two sections, and one rule decides which a setting belongs to:
anything a command-line flag can already express goes under `flags:`, keyed
by the flag's own long name (without the leading `--`). Anything a flag
cannot express, such as `basic_auth` or `tls_config` for an HTTP-flavor
exporter's outbound requests, gets its own section instead,
`http_client_config:`. No setting is ever expressible in both places, so a
value never has two conflicting sources.

`http_client_config:` is HTTP-flavor only. A CLI-flavor exporter runs a local
command rather than issuing HTTP requests, so it has nothing to authenticate.
Given that section it exits with an error instead of starting, because a
setting that looks applied but does nothing is worse than a refusal.

**Precedence, highest wins:**

1. Command line
2. `--config.file`
3. Process environment
4. Each flag's own built-in default

A value set on the command line always overrides the same key set in the
file. The file itself sits above the process environment, not below it: for
a flag that also reads an environment variable, the file wins if both are
set.

Paths written inside the file (`password_file`, `ca_file`, `cert_file`,
`key_file`, and similar) resolve relative to the file's own location, not to
the exporter's working directory. Prefer a `*_file` form over an inline
secret, `password_file` over `password` for example, so the secret itself
never has to live in the file.

## Bounding outbound concurrency

`--exporter.max-requests-per-target` (default `0`, unlimited) bounds how
many outbound requests (HTTP flavor) or command invocations (CLI flavor)
this exporter has in flight at once. It exists because a slow or throttled
target can otherwise receive as many concurrent requests as this exporter
has collectors and background pollers willing to issue at the same moment,
turning one slow target into a source of starvation for its own siblings.
It is absent on multi-target builds (`--target-model multi`): `/probe`
builds a client per incoming request from a caller-controlled target, so
there is no fixed, finite key space to index a ceiling by the way the other
two models have.

**What the ceiling actually bounds differs by build, and the difference
matters before you set a number:**

- **Single-target, HTTP flavor**: one ceiling per distinct
  `--collector.<name>.target` address, via a shared `LimiterSet`. Two
  collectors pointed at the same address share one ceiling; two collectors
  pointed at different addresses are bounded independently.
- **Single-target, CLI flavor**: one ceiling for the whole process. There is
  no per-target `Client` to index by, only the single package-level command
  boundary every collector calls through, so this flag caps total concurrent
  command invocations across every collector regardless of which machine or
  binary each one targets.
- **Multi-instance builds**: one ceiling per watched *instance* (the
  `instances:` entry, from `--config.file`), not per physical address. Two
  `instances:` entries that happen to name the same physical machine are
  bounded **independently**, not jointly, so this exporter's aggregate
  concurrency against that one machine can exceed the configured value. This
  is a deliberate trade-off, not an oversight: a per-address index would
  either need to be pre-populated to cover an instance a later reload adds,
  or rebuilt on every reload, and per-instance does neither.

A request that has to wait for a slot is not silent: `tapelibrary_exporter_request_wait_seconds`
(see [docs/metrics.md](metrics.md)) records how long, so queuing shows up in
your monitoring instead of just quietly lengthening scrape times. The wait
is bounded by the collector's own `--collector.<name>.timeout`, which is why
a positive timeout is required once a ceiling is configured: without one,
the wait would have no deadline of its own to honor.

## Configuration reload (`multi` and `multi-instance` builds only)

A single-target build has no reload at all: its `--config.file`, if any,
holds only a `flags:` section (applied once at startup, and a running
process cannot adopt a new value for a flag) and an `http_client_config:`
section whose file-backed secrets and TLS material `prometheus/common`
already re-reads from disk on every outbound request, so there is nothing
left for a reload to apply. That covers every `_file` variant the section
accepts: `username_file`, `password_file`, `credentials_file`,
`bearer_token_file`, `client_secret_file`, `client_certificate_key_file`,
`ca_file`, `cert_file` and `key_file`. See `SECURITY.md` for the full
reasoning.

Multi-target and multi-instance builds both reload `--config.file` in
place, two ways:

- **`SIGHUP`**: always available, no flag needed. Sending it already
  requires being on the machine, which is why it needs no separate opt-in.
- **`POST /-/reload`**: gated behind `--web.enable-lifecycle` (default
  `false`), the same conservative-default posture as everywhere else in this
  scaffold: an unauthenticated exporter that also exposed an unauthenticated
  way to force a reload would be a real change to the default security
  posture. With the flag unset, this route answers `404` for every method,
  exactly as if it were never registered: the flag is checked, and the
  request refused, before it ever reaches the code that would distinguish
  `POST` from anything else. With the flag set, `GET /-/reload` (and any
  method other than `POST`) instead answers `405 Method Not Allowed` with
  an `Allow: POST` header.

**On a `multi` build with no `--config.file` set** (optional and empty by
default there, see above), `SIGHUP` and `POST /-/reload` are a no-op
success, not a failure: there is nothing on disk to re-read, so
`tapelibrary_exporter_config_last_reload_successful` stays at `1` rather
than dropping to `0`. A `multi-instance` build never hits this case:
`--config.file` is required for that target model.

**A reload either fully succeeds or changes nothing.** Everything that can
fail (re-reading the file, parsing it, validating it, building any new
transport a credential change needs) happens before anything currently
running is touched. If any of it fails, the process keeps serving exactly
the configuration it had before, `tapelibrary_exporter_config_last_reload_successful`
drops to `0`, and the failure is logged at `error` with the specific reason.
A successful reload sets that same gauge back to `1` and advances
`tapelibrary_exporter_config_last_reload_success_timestamp_seconds`; see
[docs/metrics.md](metrics.md).

**A changed `flags:` section refuses the whole reload**, naming the keys
that changed: those are applied once at startup by rendering them into
kingpin arguments, and a running process has no mechanism to re-parse them,
so accepting the rest of a file while silently ignoring a `flags:` edit
would leave the process describing neither the old configuration nor the
new one. Restart the process to pick up a `flags:` change.

**On a multi-instance build specifically, a reload cannot change which
label *keys* an instance's series carry**, though it can freely change
their *values*: adding an instance label, removing one, or renaming one
across every instance is refused with a restart-required error, because a
Prometheus registry never releases a metric family's label-name dimension
once it has registered a series under it, even after every series of that
family is later unregistered, so silently allowing it would panic rather
than reload. Everything else on this build reloads with no restart: which
instances exist, an instance's address, its credentials, and the values of
its labels.

## The `/probe?target=` endpoint (multi-target builds only)

Multi-target builds (scaffolded with `--target-model multi`, http flavor
only) also expose `/probe?target=<url>`: Prometheus's own
[multi-target exporter pattern](https://prometheus.io/docs/guides/multi-target-exporter/):
each request builds a fresh registry and collector set scoped to `target`,
instead of the fixed, process-lifetime registry `/metrics` reports on. A
single-target build (`--target-model single`, the default) has no `/probe`
endpoint at all; everything below only applies once `--target-model multi`
was chosen at scaffold time.

```bash
curl 'http://localhost:9170/probe?target=http://some-host:9100'
```

- **Always-on floor:** `target` must parse as an `http`/`https` URL:
  a missing scheme, `file://`, or any other scheme gets a `400`, regardless of
  the allowlist below.
- **`--probe.target-allowlist`** (repeatable, empty by default): when set, a
  target's host must match one of these entries exactly (case- and
  trailing-dot-sensitive; matches the parsed host or `host:port` verbatim:
  `node1` will not match `NODE1` or `node1.`) or the probe returns `403`.
  Empty (the default) accepts any target that clears the floor above.
- **`X-Prometheus-Scrape-Timeout-Seconds`**: Prometheus sends this header on
  every scrape; the handler computes the probe's own deadline as
  `min(--probe.timeout, X-Prometheus-Scrape-Timeout-Seconds - --probe.timeout-offset)`,
  so a probe never outruns Prometheus's deadline or the operator's configured
  ceiling. `--probe.timeout-offset` (default `0.5s`) is subtracted from
  Prometheus's own scrape timeout so the exporter answers before Prometheus
  abandons the scrape: a probe that uses its full budget and replies a
  moment too late is a wasted scrape. `--probe.timeout` (default `5s`) is
  the ceiling applied when the header is missing, unparseable, or would
  leave no budget once the offset is subtracted.
- **SSRF posture**: `/probe` making this exporter fetch an operator-supplied
  URL is a deliberate, ecosystem-standard trade-off (Blackbox/SNMP/IPMI all
  work this way): see [SECURITY.md](../SECURITY.md) before exposing this
  endpoint beyond a trusted network.

### Selecting a module

`?module=` names one or more modules declared in `--config.file`'s `modules:`
section. It is repeatable and comma-separated, so `?module=a&module=b,c`
selects a, b and c. For the YAML shape of that section, see the commented
`modules:` block in `config.example.yml` at the repository root, which spells
out both conventions (one complete bundle per group of targets, or
credentials and collector subsets as independent axes); the `modules:` sample
further down this page is the multi-instance variant, where a module is
resolved once at boot rather than per request.

- **Collectors combine.** The probe runs the union of the selected modules'
  `collectors:` lists, always in the exporter's own declared order. A module
  that lists none contributes none, and if the union ends up empty, for
  example because the only module selected was a credentials-only one such as
  `default`, every registered collector runs: the same fallback an absent
  `module=` parameter takes.
- **Credentials do not combine.** A request makes one connection, so it needs
  one set of credentials. They resolve in this order, first hit wins: the
  unique selected module carrying an `http_client_config:`, then a module
  named `default`, then the top-level `http_client_config:`. Selecting two
  modules that both carry credentials returns **400**.
- **A probe that resolves no credentials against a file that declares some
  returns 400.** Probing in the clear would return 200 with series nobody
  questions; the 400 makes `up` go to 0 and shows in your monitoring.
- **An unknown module returns 400**, and a request naming no module at all
  runs every collector.

A matching scrape config carries the module as a label and relabels it:

```yaml
  - job_name: 'tapelibrary_exporter-probe'
    metrics_path: /probe
    file_sd_configs: [{ files: ['targets/*.yml'] }]   # each entry carries a `module` label
    relabel_configs:
      - { source_labels: [__address__],    target_label: __param_target }
      - { source_labels: [module],         target_label: __param_module }
      - { source_labels: [__param_target], target_label: instance }
      - { target_label: __address__, replacement: 'exporter-host:9170' }
```

## The multi-instance target model (multi-instance builds only)

A multi-instance build (scaffolded with `--target-model multi-instance`, http
flavor only) watches a fixed list of machines declared in a configuration
file, instead of one target baked in at scaffold time (single-target) or one
target per `/probe` request (multi-target). `--config.file` is **required**
for this build: without it there is no instance list to load, and the
exporter refuses to start. Single-target and multi-target builds keep
`--config.file` optional, unchanged.

```bash
./tapelibrary_exporter --config.file=config.yml
```

### The `modules:` and `instances:` sections

On top of the `flags:` and `http_client_config:` sections described above, a
multi-instance config file adds two more:

```yaml
# modules: named credential/TLS bundles, one exporter-wide set an instance
# can reference by name.
modules:
  default:
    http_client_config:
      basic_auth:
        username: monitor
        password_file: /etc/tapelibrary_exporter/default.pass
  privileged:
    http_client_config:
      tls_config:
        ca_file: /etc/tapelibrary_exporter/internal-ca.pem

# instances: the machines this process watches. Required: at least one.
instances:
  - name: machine-a
    address: https://machine-a.example.net
    labels:
      site: paris
  - name: machine-b
    address: https://machine-b.example.net
    module: privileged
```

- **`modules:`** is optional. Each entry names an `http_client_config:`
  bundle; an instance picks one by name in its own `module:` key. Leaving
  `module:` off an instance uses the `default` module; with no `modules:`
  section at all, a top-level `http_client_config:` (if present) is used
  instead, so a plain pre-multi-instance config file keeps working
  unchanged.
- **`instances:`** is required and needs at least one entry. Each instance
  needs a unique `name` and an `address` that parses as an `http`/`https`
  URL; `module` is optional (`default` if omitted); `labels` are optional
  extra key/value pairs added to every series that instance emits. All
  instances must declare the same set of label keys.
- A module's `collectors:` key (used on multi-target builds to narrow a
  `/probe` request) is meaningless here, since collector enablement is
  global via `--[no-]collector.<name>` under multi-instance; setting it on
  any module in the file is refused at boot rather than silently ignored.
- An instance's `labels:` may not reuse the identifying label (see below).

Any problem here (a missing instance list, a duplicate name, an address that
is not `http`/`https`, an unknown module, a label collision) fails at boot
with a clear message, not at the first scrape.

### Instance label

Every per-collector series this exporter emits, across every instance and every
collector including the per-collector health metrics, carries one identifying
label: `library` by default, for example
`tapelibrary_exporter_collector_success{collector="example", library="machine-a"}`.
The two exceptions are `tapelibrary_exporter_request_duration_seconds` and
`tapelibrary_exporter_request_wait_seconds`: both single, process-wide
histograms shared across all instances, so neither carries
`library`. Both are labeled only by `outcome`.
Its name is fixed at scaffold time via `scaffold.sh --instance-label`, never
a runtime flag. It is applied by the exporter itself, per instance, rather
than by any collector's own descriptor, so `make docs-check` cannot see or
verify it; keeping the name fixed at scaffold time is what lets
`docs/metrics.md` state it as fact instead of describing a value that could
drift at runtime with nothing left to catch it.

### Prometheus configuration for a multi-instance build

A multi-instance build is scraped exactly like a single-target one: one
static target, no relabeling.

```yaml
scrape_configs:
  - job_name: 'tapelibrary_exporter'
    static_configs:
      - targets: ['tapelibrary_exporter.example.internal:9170']
```

`scrape_timeout` only has to cover this one process answering from its own
caches, not N live fetches against N instances: `/metrics` never talks to an
instance directly. Each instance's collectors are background pollers that
refresh a cache on their own schedule (`--collector.<name>.interval`); a
scrape just reads whatever is already cached.

---

## Prometheus configuration

```yaml
scrape_configs:
  - job_name: 'tapelibrary_exporter'
    scrape_interval: 30s
    scrape_timeout: 30s
    static_configs:
      - targets: ['tapelibrary_exporter.example.internal:9170']
```

- **scrape_interval**: pick a value that comfortably exceeds how long a full scrape takes
  against your real target: start at 30s and tighten only once you've measured
  `tapelibrary_exporter_collector_duration_seconds` under load.
- **scrape_timeout**: should be equal to or less than `scrape_interval`, to avoid
  `context_deadline_exceeded` errors piling up in Prometheus itself.

Check the config:

```bash
promtool check-config prometheus.yml
```

### Internal exporter metrics

Every collector is wrapped by a shared status tracker
that emits two self-monitoring metrics regardless of what the collector itself reports:

| Metric | Description | Labels |
|---|---|---|
| `tapelibrary_exporter_collector_success` | `1` if the last scrape emitted at least one metric and did not panic, `0` otherwise | `collector` |
| `tapelibrary_exporter_collector_duration_seconds` | Wall time of the last `Collect()` call | `collector` |

These let you alert per-collector, independently of Prometheus's own global `up` metric.

In addition, each I/O flavor exposes a histogram timing its own outbound calls:

| Metric | Description | Labels | Flavor |
|---|---|---|---|
| `tapelibrary_exporter_request_duration_seconds` | Duration of HTTP requests issued by collectors | `outcome` (`success`/`error`) | HTTP |
| `tapelibrary_exporter_command_duration_seconds` | Duration of external commands executed by collectors | `outcome` (`success`/`error`) | CLI |
| `tapelibrary_exporter_request_wait_seconds` | Duration a request or command waited for a concurrency slot, by outcome. Both series stay at a permanent zero once `--exporter.max-requests-per-target` is unset (the default); see [Bounding outbound concurrency](#bounding-outbound-concurrency) above | `outcome` (`success`/`error`) | HTTP and CLI |

On a multi-target or multi-instance build, two further gauges report the
health of the configuration-reload mechanism itself (absent on
single-target builds, which have no reload; see
[Configuration reload](#configuration-reload-multi-and-multi-instance-builds-only)
above):

| Metric | Description | Labels |
|---|---|---|
| `tapelibrary_exporter_config_last_reload_successful` | Whether the last configuration reload attempt succeeded (`1`) or failed (`0`) | - |
| `tapelibrary_exporter_config_last_reload_success_timestamp_seconds` | Unix time of the last successful configuration reload | - |

---

### Performance considerations

- **Per-collector timeouts**: raise a collector's own `--collector.<name>.timeout` flag if
  your target is slow to respond, but remember Prometheus's `scrape_timeout` still applies
  across *all* collectors combined, so a single slow collector can still starve the others.
- **Scrape interval**: use an interval that comfortably exceeds your measured
  `tapelibrary_exporter_collector_duration_seconds` to avoid overlapping scrapes.
- **Collector selection**: disable collectors you don't need to reduce load:

  ```bash
  ./tapelibrary_exporter --no-collector.http_client_requests
  ```
