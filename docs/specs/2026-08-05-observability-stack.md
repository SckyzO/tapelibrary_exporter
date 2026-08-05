# Design: a full observability stack for validation

> Status: approved 2026-08-05, not yet implemented.

## Why

Two things this repository needs are blocked on the same missing piece.

**The alert thresholds are unmeasured.** 67 rules ship, and apart from the
cleaning-cartridge pair — which comes from the previous generation of
monitoring — their thresholds are re-derived defaults rather than values
observed on this fleet. Confirming them needs weeks of real history.

**The dashboards cannot be written blind.** `monitoring/grafana/` holds one
health dashboard. Writing panels against metrics nobody has plotted produces
JSON that looks plausible and may show nothing: a panel querying a metric this
fleet never emits renders "No data" and passes every check this repository has,
because none of them render anything.

Both are answered by running the exporter, Prometheus and Grafana together
against the real libraries, and leaving it running.

## What this is not

It is not a mock. An earlier version of this design proposed a fixture server
replaying `internal/collector/testdata/` so the stack could run anywhere. That
was rejected, correctly: dashboards calibrated against invented values are worth
less than no dashboards, because they carry the authority of something that
renders. The stack points at real hardware or it is not run.

It is also not a production deployment. `docker-compose.yml` remains the file
for running the exporter alone, hardened; an operator deploying it must not
inherit a Grafana.

## Shape

A new `docker-compose.stack.yml` at the repository root, versioned. The same
file serves two audiences: a maintainer validating against the fleet, and
someone evaluating the project. The instance list stays in `config.yml`, which
is gitignored, so the fleet's real addresses never reach a commit while the
compose file itself is shareable.

| service | role | port |
|---|---|---|
| `tapelibrary_exporter` | standard image, `./config.yml` mounted read-only, same hardening as `docker-compose.yml` | 9170 |
| `prometheus` | scrapes the exporter, loads `alerts.yml` and `rules.yml`, persistent volume | 9090 |
| `grafana` | Prometheus datasource and a dashboard directory, both provisioned | 3000 |

**No Alertmanager.** Rules evaluate and reach `firing` in the Prometheus UI,
which is what validating a threshold requires. Routing notifications is a
separate concern with its own secrets, and adding it now would be scope this
design does not need. It can be added later without changing anything here.

## Files this adds

- `monitoring/prometheus/prometheus.yml` — a **complete** configuration for this
  stack. Distinct from `scrape-config.example.yml`, which is a fragment meant to
  be merged into somebody's existing Prometheus. Both are kept: they serve
  different readers, and collapsing them would leave one of the two audiences
  editing a file that does not fit their situation.
- `monitoring/grafana/provisioning/datasources/prometheus.yml`
- `monitoring/grafana/provisioning/dashboards/dashboards.yml`, pointing at
  `monitoring/grafana/`

The consequence of that last file is the point of it: every `.json` dropped into
`monitoring/grafana/` is loaded at startup. Dashboards written later appear with
no manual import step, and `health-dashboard.json` is visible on first run.

## Retention

Named volumes for Prometheus and Grafana; **30 days** retention.

At the shipped defaults the fleet produces roughly 18 060 series, which at a 60s
scrape interval is on the order of 1.5 GB over 30 days — small enough not to
need thought. Without a named volume, `docker compose down` would erase the
weeks of history that are the entire reason for running this.

The per-volser collectors stay **off**, as they are by default. Turning
`collector.data_cartridges.per-volser` and
`collector.data_cartridges_lifetime.per-volser` on raises the fleet to roughly
500 000 series and the 30-day footprint to tens of gigabytes. That is a decision
to take deliberately, after watching the stack run, not one to inherit from an
example file. The compose file will carry both flags commented out with that
number written next to them, so switching is one line rather than a
reconstruction.

What the switch buys, and why it will probably happen: the aggregates answer
"how many cartridges are near end of life", while the per-volser series answer
"which ones" — `topk(20, tapelibrary_data_cartridge_lifetime_remaining_ratio)`
is a list somebody can act on. Dashboards should be laid out so those panels can
be added without rearranging what is already there.

## Security posture, stated rather than implied

Grafana starts on `admin/admin` and all three ports are published. That is
reasonable for a validation stack on a trusted network and unreasonable
anywhere else. The file's header will say so, because the surrounding files in
this repository are hardened and a reader is entitled to assume this one is too
unless told otherwise.

## Verification

Not "the YAML parses". The stack is accepted when, on the test server:

1. All three containers report `Up`.
2. Prometheus `/targets` shows the exporter target `UP`.
3. `tapelibrary_exporter_collector_success` is present for all five libraries.
4. Grafana's datasource answers, and `health-dashboard.json` renders with data.

Every one of the four is checked by running the stack. This repository has
already shipped three separate defects that a green `make check` could not see —
no authentication, timeouts too short, `--version` refusing to start — plus, on
2026-08-05, a documented Docker install path that never started. The pattern is
consistent enough to treat as a rule: what is never executed is never verified.

## After this

Prometheus becomes reachable through a tunnel, and dashboards get written
against the series it has actually collected rather than against the metric
names in `docs/metrics.md`. Threshold validation starts accumulating history the
moment the stack comes up, and is revisited once there are weeks of it.
