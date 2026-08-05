# Monitoring assets

Everything needed to plug `tapelibrary_exporter` into a Prometheus + Grafana
stack: starter alerting rules, a recording rule, and a ready-to-import
Grafana health dashboard.

```
monitoring/
├── grafana/
│   ├── health-dashboard.json    Collector health, scrape duration, build info
│   └── provisioning/            Datasource + dashboard provider (stack only)
└── prometheus/
    ├── alerts.yml               Alerting rules (severity-based, site-neutral)
    ├── prometheus.yml           Complete config for docker-compose.stack.yml
    ├── rules.yml                Recording rules (pre-computed expressions)
    └── scrape-config.example.yml  Fragment to merge into an existing Prometheus
```

**Boundary: alerting is a Prometheus concern (core, shipped here); dashboards
are a Grafana concern** (this repo ships one, health, plus the pattern to
build your own). See [Observability](../README.md#observability) for where
this fits among the rest of the exporter's docs.

## Scraping this exporter

`prometheus/scrape-config.example.yml` is the `scrape_configs` entry to copy into
your own `prometheus.yml`, with the two rule files beside it.

**One target, not one per library.** A single exporter process watches every
library in its `--config.file` and serves them all through one `/metrics`, so
`instance` is the exporter's host and `library` is the machine. Group and alert
by `library`.

## Running the whole thing

If you have no Prometheus to merge that fragment into — or you want one watching
a real fleet before committing to thresholds — `docker-compose.stack.yml` at the
repository root brings up the exporter, Prometheus and Grafana together, with
`prometheus/prometheus.yml` and the dashboards in `grafana/` provisioned
automatically:

```bash
cp config.example.yml config.yml     # then put your libraries in it
make docker-build
docker compose -f docker-compose.stack.yml up -d
```

Prometheus lands on `:9090` with both rule files loaded, Grafana on `:3000`.
Every `.json` in `grafana/` is picked up at startup, so a dashboard added to
this directory needs no import step.

Two things to know before running it anywhere real. It is **not** hardened the
way `docker-compose.yml` is — Grafana starts on `admin/admin` and every port is
published — so it belongs on a machine you control. And its Prometheus keeps 30
days in a named volume: `docker compose … down` preserves that history, `down
-v` deletes it, which matters because accumulating that history is the point.

## Two tiers, one pattern

Everything in `alerts.yml` follows the same shape:

- **Health** (active): built entirely from this exporter's own
  self-instrumentation (`tapelibrary_exporter_collector_success`,
  `tapelibrary_exporter_collector_duration_seconds`) plus Prometheus's
  standard `up`. Identical for every I/O flavor this plugin scaffolds, and
  safe to load as-is.
- **Business** (commented-out example): what your own collectors measure is
  specific to your target, so there is no version of a business alert
  that's true out of the box for every exporter. The commented block at the
  bottom of `alerts.yml` teaches the same warning/critical + `for:` +
  portable-labels pattern against a placeholder metric name.
  Uncomment and adapt it, or add a new one, as you build real collectors.
  `/add-collector` (if you have the scaffolding plugin available) proposes
  a real alert here for every collector's own metrics as you add them.

Both tiers share the same conventions:

| Convention | Value | Purpose |
|---|---|---|
| `severity` | `warning`, `critical` | Alertmanager routing |
| `component` | `exporter` | Filtering / namespacing |
| `for:` | set on every alert | De-flaps transient blips |
| `summary` / `description` | annotations | One-line headline + full sentence |

Site-specific labels (`team`, `runbook_url`, `dashboard_url`, cluster/env
tags, etc.) are **intentionally not in this file**. Add them via your local
override, Prometheus `external_labels`, or Alertmanager routing config.
Keeping them out keeps this file portable across environments.

Alert thresholds are reasonable defaults; tune them to your deployment size
and operational tolerance: see the comments in `alerts.yml` and
[docs/configuration.md](../docs/configuration.md)'s `scrape_interval` /
`scrape_timeout` guidance.

## `rules.yml`

One active recording rule,
`job:tapelibrary_exporter_collector_duration_seconds:avg_healthy`:
average scrape duration across collectors that are currently succeeding,
guarded against the 0/0 case where every collector is failing at once (see
the comment above the rule for exactly when that guard is real, not
decorative). A second, commented-out example shows the classic
counter-based `rate(...) / (rate(...) > 0)` form of the same guard, applied
to this exporter's own request/command duration histogram: the pattern to
reach for once you add a collector with a real counter metric.

## Grafana dashboard

`grafana/health-dashboard.json` is importable as-is via the Grafana UI
("+ → Import → Upload JSON file"), the Grafana HTTP API, or file
provisioning. It has two template variables:

- `datasource`: pick your Prometheus data source.
- `job`: the Prometheus scrape `job` label(s) to show (multi-select,
  defaults to all).

Panels cover exactly what's generic across every exporter this plugin
scaffolds: target up/down, per-collector success/failure (current status,
percentage healthy, and history), scrape duration (current, sorted, and over
time), and Go build info. There is no business-metric panel here by design:
see [Business dashboards](#business-dashboards) below.

## Wiring it up in Prometheus

```yaml
# /etc/prometheus/prometheus.yml
scrape_configs:
  - job_name: 'tapelibrary_exporter'
    static_configs:
      - targets: ['tapelibrary_exporter.example.internal:9170']

rule_files:
  - /etc/prometheus/rules/tapelibrary_exporter_alerts.yml
  - /etc/prometheus/rules/tapelibrary_exporter_rules.yml
```

Place the two YAML files under `/etc/prometheus/rules/` (or your equivalent
path), reload Prometheus (`SIGHUP` or `/-/reload`), and check **Status →
Rules** to confirm the `tapelibrary.alerts` and `tapelibrary.rules`
groups load without errors.

**Match your real `job_name`.** `alerts.yml`'s `ExporterDown` rule hardcodes
`up{job="tapelibrary_exporter"} == 0`, matching the `job_name` in the
`scrape_configs` snippet above. If your own `scrape_configs` entry uses a
different `job_name`, edit that selector (and any other `job=` selector you
rely on across `alerts.yml` / `rules.yml`) to match. Otherwise the alert
silently never fires, since it's just comparing against a `job` label value
that no series will ever have.

### Validating the rule files

```bash
# Locally, if you have promtool:
promtool check rules monitoring/prometheus/rules.yml monitoring/prometheus/alerts.yml

# Via Docker, no local install:
docker run --rm -v "$(pwd):/rules" --entrypoint promtool prom/prometheus:latest \
  check rules /rules/monitoring/prometheus/rules.yml /rules/monitoring/prometheus/alerts.yml
```

Both should report `SUCCESS`.

## Business dashboards

A business-metric dashboard (what your collectors actually measure: queue
depth, error rates, saturation, whatever your target's domain is) is
deliberately **not** shipped here: unlike the health dashboard above, there
is no version of it that's generic across every exporter. Generate one from
this repo's own `docs/metrics.md` with `/generate-dashboard` (if you have the
scaffolding plugin available), a short design pass (audience, the RED/USE
method, key metrics, template variables, drill-down) on top of a deterministic
backbone that emits exportable Grafana JSON, one panel per documented metric.
The generated dashboards land here in `grafana/` next to `health-dashboard.json`
and import the same way; every panel query references only a metric documented
in `docs/metrics.md`.

## What's not in this folder

- **Alertmanager configuration**: routing, silencing, notification
  receivers. Site-specific: see the
  [Alertmanager docs](https://prometheus.io/docs/alerting/latest/configuration/).
- **Prometheus storage / retention / scrape interval**: same reason, see
  [docs/configuration.md](../docs/configuration.md).
- **Grafana datasource provisioning**: depends on your Grafana topology.
