# CLAUDE.md

Guidance for working in `tapelibrary_exporter`, a Prometheus exporter written in
Go. This file states what this repository *is*; `CONTRIBUTING.md` states how
to change it, and is the one to read in full before a first change.

## Invariants

| Fact | Value |
|---|---|
| Metric namespace | `tapelibrary` |
| Target model | `multi-instance` |
| I/O flavor | `http` |
| Default port | `9170` |

These four are fixed at scaffold time. Changing any of them is a regeneration,
not an edit: the target model decides how `/metrics` is assembled and the
flavor decides the collector factory signature, so a half-converted repository
compiles into a shape nothing else expects.

## Where state lives

| Question | Answer, on disk |
|---|---|
| What does this exporter emit? | `docs/metrics.md`, enforced by `make docs-check` |
| What is configurable? | `config.example.yml`, `docs/configuration.md` |
| What alerts ship? | `monitoring/prometheus/alerts.yml` |
| What is left to build, and why? | `docs/exporter-journal.md` |

`docs/exporter-journal.md` is the project journal. It carries what the code
cannot state about itself: the collectors still planned, the cardinality
budget as an intention, the naming conventions this exporter agreed on, and
the reasoning behind each decision. Read it before starting work, and treat
the code as authoritative wherever the two disagree.

## `samples/`

`samples/` holds raw output captured from the monitored target and the
target's own API documentation. Its contents are gitignored and **not
anonymized**. Nothing goes from there into `internal/collector/testdata/`
without being trimmed and anonymized first: see `CONTRIBUTING.md`, "Test
Data", and `samples/README.md`.

## The gate

```sh
make check
```

This is the one command that decides whether a change is finished: do not
declare work done on a subset of it. `CONTRIBUTING.md` lists what it runs,
what it requires, and how to run it against a host Go toolchain instead.

## Resuming after a cleared context

This repository is designed to be built across several sessions. Everything
that must survive is on disk, so clearing the context loses nothing that
matters.

**All eighteen collectors are built** (2026-08-04) and the exporter has been
validated against the real five-library fleet, so there is no longer a "first
unticked collector" to resume from. Read `docs/exporter-journal.md` — it is the
source of truth and it is exhaustive. Its `## Open questions / assumptions`
section is status-tagged, so:

```sh
grep '\[OPEN\]' docs/exporter-journal.md
```

answers "what is left" without reading 2 900 lines. `[ACCEPTED]` marks a
deliberate cost that will not change; `[RESOLVED <date>]` keeps the reasoning
rather than deleting it, because several were resolved by contradicting what
they originally assumed.

**One lesson from the first real deployment is worth carrying into any change
here.** Three separate bugs — no authentication, every timeout too short, and
`--version` refusing to run — survived 337 passing tests and a green
`make check`, because every test points at an `httptest` server that answers
200 instantly. A suite built that way is silent on authentication, latency and
concurrency. `make test` proves the code is self-consistent; only
`docs/validation-checklist.md`, run against a real library, proves it works.
