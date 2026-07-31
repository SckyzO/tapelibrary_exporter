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

## ExampleCollector

Defined in `internal/collector/collector.go`, the background-refresh variant: a
goroutine polls the target on `--collector.example.interval` and every scrape
serves the last cached result. Replace this section's rows when you adapt
`ExampleCollector` into your real collector.

| Metric | Type | Labels | Description |
|---|---|---|---|
| `tapelibrary_items` | Gauge | - | Number of items reported by the example target. |
| `tapelibrary_healthy` | Gauge | - | Whether the example target reports itself healthy (1) or not (0). |
| `tapelibrary_example_last_refresh_timestamp_seconds` | Gauge | - | Unix time of the last successful example refresh. Alert if time() minus this exceeds 2x the collector's configured interval. |

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

## The instance label

This exporter watches every instance listed in its `--config.file` and serves
them all through one `/metrics`. The ExampleCollector metrics and the
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
