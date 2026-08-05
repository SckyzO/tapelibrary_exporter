# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

<!--
  Write every entry for the OPERATOR running this exporter, not just for a code reviewer:
  a metric renamed or retyped, a flag whose default changed, a collector that's now
  enabled/disabled by default, a label that's new on an existing metric. For a breaking
  change, add a before/after table and say what alerts or dashboards to re-check afterward:

  | Old | New |
  | --- | --- |
  | `tapelibrary_foo_total` (Counter) | `tapelibrary_foo` (Gauge) |

  Sub-sections, in this order, only when they have content:

  ### Added        - new collectors, metrics, flags
  ### Changed      - behavior or default changes (call out breaking ones explicitly)
  ### Deprecated   - behavior that will be removed in a future release
  ### Removed      - removed collectors, metrics, flags
  ### Fixed        - bug fixes (say which metric/label was affected and how)
  ### Security     - vulnerability fixes
-->

## [0.1.1] - 2026-08-05

### Fixed

- **The documented Docker install path could never have worked.** Both
  `docker-compose.yml` and `docker-compose.minimal.yml` started the exporter without
  `--config.file` and mounted no configuration, so the container exited immediately
  with `the multi-instance target model requires --config.file`; with
  `restart: unless-stopped` also set, it crash-looped, while `make docker-run`
  cheerfully printed `Metrics at http://localhost:9170/metrics`. Both files now mount
  `./config.yml` read-only at `/etc/tapelibrary_exporter/config.yml` and pass the
  flag. **If you scripted anything around `make docker-run`, it was failing silently
  until now.**
- **`config.example.yml` did not start after being copied**, which is the only thing
  anyone does with it. It shipped an active `flags:` section but no `instances:` — the
  one section this exporter cannot boot without. It now carries a placeholder instance,
  so `cp config.example.yml config.yml` is enough to get a running exporter serving
  `/metrics`; the collectors will report failures until the address points at a real
  library, which is the correct behavior rather than a defect.
- **`config.example.yml` documented an endpoint this exporter does not serve.** Its
  `modules:` section explained scraping through `/probe?target=…&module=…`, a route
  belonging to a different target model; the routes actually registered are `/`,
  `/healthz`, `/metrics` and `/-/reload`. Its two module examples also used a
  `collectors:` key that this build refuses, and named the example collector removed
  before 0.1.0. The section is now written for this exporter alone.
- **`config.yml` is now ignored by git.** It is the file both compose stacks mount and
  the name everyone reaches by copying the example, so it is the one most likely to
  collect a real fleet's addresses and credentials and then be committed by reflex.
  `config.example.yml` stays tracked.

## [0.1.0] - 2026-08-05

First tagged release. Everything below describes the exporter as it stands at this
tag, validated against a five-library TS4500 fleet.

### Added

- **Two collectors: `reports_accessors` and `diagnostic_cartridges`**, completing the
  planned set at eighteen. `reports_accessors` reads `GET /v1/reports/accessors` and
  emits `tapelibrary_accessor_report_*`: the per-hour counterpart of the lifetime
  counters `accessors` already reports, which is what makes a stopped accessor visible
  — a lifetime counter at 2.7 million gets barely moves when one stops, while its
  hourly window drops to zero at once. `diagnostic_cartridges` reads
  `GET /v1/diagnosticCartridges` and answers whether the library can still service
  itself; `tapelibrary_diagnostic_cartridges_usable` reaching 0 means the next service
  action needing media is blocked.
- **RoE session authentication.** The TS4500 accepts no HTTP authentication scheme, so
  before this the exporter could not authenticate against its own target at all: every
  collector took a 401 on every refresh. `basic_auth`'s credentials are now POSTed to
  `/v1/login` and the resulting session cookie rides every request, renewed on
  expiry and ended at shutdown. **No `Authorization` header is ever sent** — the block
  carries the credentials, the handshake consumes them.
- **`tapelibrary_exporter_build_info`**, the conventional `<name>_build_info` gauge.
  Until now only Go's `go_build_info` was exposed, so identifying a running build meant
  decoding a commit hash out of a pseudo-version.
- **`tapelibrary_data_cartridges_usage_duplicate_volsers`**, counting barcodes carried
  by more than one cartridge. Read by the new `DataCartridgeDuplicateVolser` alert.
- **`--exporter.max-queue-wait`** (default `15m`), bounding the wait for a request slot
  separately from the per-collector request timeout.
- **Alerting:** `CollectorNeverRefreshed` and `CollectorRefreshStale` on every
  collector's freshness gauge, plus business rules for the two new collectors and the
  duplicate-barcode condition. See **Fixed** for why the freshness pair matters.
- **`monitoring/prometheus/scrape-config.example.yml`**, and `promtool check rules` is
  now part of `make check` as the `rules-check` target.

### Changed

- **BREAKING (defaults): `--exporter.max-requests-per-target` now defaults to `1`,
  was `0` (unlimited).** The TS4500 serializes REST commands internally — R1.11.2
  requires each response to be retrieved before the next command is sent — so
  concurrency measurably buys nothing: ten concurrent requests took 9s in total
  against 12s issued one at a time, while per-request latency went from ~1s to as much
  as 8.2s. With the old default, **every collector failed on every refresh** against a
  real library.
- **BREAKING (defaults): per-collector timeouts and two intervals are recalibrated**
  against measurements taken on real hardware. Nothing about a payload predicts its
  latency here: `/v1/library` returns 762 bytes more slowly than `/v1/fcPorts` returns
  23 KB.

  | Collector | Old timeout | New | Measured |
  | --- | --- | --- | --- |
  | most collectors | `5s` | `60s` | 0.7–2.7s, with `/v1/library` seen once at 45s |
  | `slots` | `30s` | `180s` | 23.7s |
  | `reports_drives` | `60s` | `180s` | 49.8s |
  | `data_cartridges_lifetime` | `60s` | `900s` | **343.9s** |
  | `data_cartridges` | `60s` | `900s` | **over 600s** |

  The last two **could never complete** at their old timeouts and had therefore never
  succeeded once against real hardware. Both also move from a `15m` interval to `1h`:
  together they needed more than 900s of the library's single request slot per 900s
  cycle, starving every sibling.
- `data_cartridges_lifetime` **no longer rejects an entire response over one duplicate
  barcode.** A live library produced exactly one among 9 673 distinct ones, and that
  single ambiguity was taking out all five aggregate families permanently. The
  per-cartridge series still skip the ambiguous barcodes — two metrics sharing a
  descriptor and a label set fail the whole scrape — but every aggregate now counts
  them.

### Removed

- **The `example` collector**, the scaffold's starter. It polled `/v1/library` under a
  second name, so it duplicated another collector's request against a machine that
  serializes, and was the only registered collector that never refreshed.
  `--collector.example`, `--collector.example.timeout` and
  `--collector.example.interval` are gone, and so is
  `tapelibrary_example_last_refresh_timestamp_seconds`. **Re-check any dashboard or
  rule referencing it.**

### Fixed

- **`--version` and `--help` exited 1 demanding `--config.file`.** The mandatory-config
  check ran before the flag parser that handles them. Packaging, CI and container
  healthchecks all call `--version` on a binary they have no configuration for.
- **The systemd unit could not start.** Its `ExecStart` never passed `--config.file`,
  which is mandatory on this build and has no flag equivalent. The unit is also
  sandboxed now (`ProtectSystem=strict`, empty `CapabilityBoundingSet`) and creates
  `/etc/tapelibrary_exporter` at `0750` via `ConfigurationDirectory`.
- **`tapelibrary_exporter_collector_success` reads `1` for a collector that has never
  successfully refreshed**, and that is by design: a background collector always emits
  at least its freshness gauge, so `Collect` returning data is not the same claim as
  the data being fresh. Nothing read those gauges before, so eighteen collectors could
  and did report healthy while collecting nothing. **Alert on
  `CollectorNeverRefreshed` rather than on `collector_success`**, and re-check any
  dashboard panel built on the latter.
- Documentation now states that the factory certificate these libraries ship carries
  **no `subjectAltName`**, so no `ca_file`/`server_name` combination can verify it and
  `insecure_skip_verify` is forced rather than preferred. The fix is a certificate
  reissue on the library. Note that `curl -k` succeeds against it while no Go client
  can, which makes `curl` an invalid probe for this question.
