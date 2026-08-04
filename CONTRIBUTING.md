# Contributing to tapelibrary_exporter

Thank you for your interest in contributing!

---

## Language

English for everything that ships: code, comments, commit messages, issues, and pull
requests. This keeps the project reviewable by the widest possible set of contributors.

---

## Development Setup

### Prerequisites

- A container engine (Docker or Podman): every quality-gate `make` target runs inside a
  pinned tools image, so this is the only hard requirement.
- Go 1.26+ if you want to run targets on the host instead (`NATIVE=1 make <target>`), or just
  to get editor/language-server support.

### Quick start

```bash
git clone <this repository's clone URL>
cd tapelibrary_exporter
make build
make test
make lint
```

---

## Definition of Done

Every feature, bug fix, or refactor **must** pass all the steps below before being merged.
This applies equally to external contributions and to work by a regular maintainer.

### Step-by-step protocol

```
1. make build
   -> Binary compiles without errors or warnings.

2. make test
   -> All tests pass.
   -> Coverage must not decrease vs. the previous commit:
        go test -count=1 -coverprofile=coverage.out ./...
        go tool cover -func=coverage.out | grep total

3. make lint
   -> golangci-lint run ./... reports 0 issues.

4. make docs-check
   -> docs/metrics.md matches what the code actually emits: no metric or label documented
      there that the code doesn't produce (undocumented code metrics are a warning, not a
      failure, but should still get added before you ship).

5. Run against a real target
   -> Start the built binary against an actual instance of your data source (or the closest
      sandbox/staging equivalent you have) - not just the fixture-fed unit tests:
        bin/tapelibrary_exporter --web.listen-address=:9170 --log.level=debug

6. Generate a representative workload
   -> Exercise the target enough that every collector touched by this change reports real,
      non-degenerate data - not just the empty/zero-value case.

7. Validate by metric, not by eye
   -> For every metric your change adds or touches, confirm it is present with a plausible
      value:
        curl -s http://localhost:9170/metrics | grep <metric_name>

8. Check logs
   -> No unexpected ERROR or WARN entries during the run above.

9. CI-local green
   -> make check    # vet + lint + test + vuln + actionlint + zizmor + deadcode + docs-check + rules-check, containerized
```

### Shortcut for small changes

For docs-only or comment-only changes, `make check` (plus `make docs-check` if you touched a
metric name or a doc file) is enough: skip the live-target run.

Steps 5-8 are required for any change to collector code (`internal/collector/`) or the main
entrypoint (`cmd/tapelibrary_exporter/`).

If your fork uses the GitHub Actions layer, `act push` can simulate CI locally before you
open a PR: see [docs/development.md](docs/development.md).

---

## Code Conventions

### Naming (Go idioms)

- **Initialisms stay all-caps**: `HTTP`, `URL`, `ID`, `API`, `CPU`, `JSON`.
- **Unexported**: camelCase (`parseExample`, `numWorkers`).
- **Exported**: PascalCase with the initialism fully capitalized (`NewExampleCollector`,
  `ParseHTTPResponse`).
- **Avoid a plural on an initialism**: `NewIDCollector`, not `NewIDsCollector`.

### Adding a new collector

`/add-collector <name>` (if you have the scaffolding plugin available) materializes the five
pieces and the test triad below for you and wires the registration automatically. Doing it by
hand, every new collector needs:

1. A `<name>Data(ctx)` method: the collector's only I/O (HTTP fetch, command execution,
   query, ...). This is the one piece that varies by flavor; the other four don't.
2. A `parse<Name>(b []byte)` function: pure logic, no I/O, no logging, so it's unit-testable
   with a plain byte fixture.
3. A `<name>GetMetrics(ctx)` method: the glue between the two pieces above.
4. A `<Name>Collector` struct holding its `*prometheus.Desc` fields, implementing
   `Describe`/`Collect`.
5. A `New<Name>Collector(...)` constructor.

Plus:

- A `testdata/<name>.{txt,json}` fixture with anonymized sample data (see Test Data below).
- The test triad: a parser test (fixture in, struct out, plus malformed/empty/edge-case
  inputs), a `_Collect` test (real registry + `Gather`, comparing the exact exposition text),
  a `_Describe` test (pins the exact descriptor count), and an `_ErrorHandling` test (I/O
  failure -> zero metrics, no panic).
- Metric names and label **keys** must be static: a plain string literal, or
  `prometheus.BuildFQName(ns, sub, name)` with string-literal arguments, never a
  computed/variable name or label key. This isn't just style: dynamic metric names and label
  keys are a Prometheus anti-pattern on their own, and `make docs-check`
  (`internal/collector/docs_check_test.go`) statically extracts every metric from source at
  exactly this precision: anything it can't resolve this way is reported as an unverifiable
  warning, not silently skipped.
- Registration in `cmd/tapelibrary_exporter/main.go`'s registry at the
  `// @@COLLECTOR_REGISTRY@@` marker, with its own `--[no-]collector.<name>` flag (see
  `register()` in that file).

**Collector-authoring rule:** on a successful scrape, always emit your metrics, with zero
*values* when there's nothing to report, never zero metrics. Emitting nothing is exactly what
the shared `StatusTracker` (`internal/collector/status_tracker.go`) treats as a *failed*
scrape. A collector that wants to report "no data this scrape" as a normal outcome must still
send a metric (a gauge set to `0`, for example), not a bare `return`.

### Commit messages

Follow [Conventional Commits](https://www.conventionalcommits.org/):

```
feat(collector): add <name> collector
fix(<collector>): <one-line summary>
docs: update CONTRIBUTING.md with the Definition of Done
chore: bump CHANGELOG for vX.Y.Z
```

---

## Test Data

Every fixture under `internal/collector/testdata/` must be anonymized before it's committed:

- Real hostnames/endpoints -> `host1`, `host2`, `example.internal`
- Real usernames/identifiers -> `user1`, `user2`, `alice`, `bob`
- Real account/organization/tenant names -> `team_a`, `org_b`
- Anything else that could identify a real system, person, or organization -> a placeholder
  that preserves the fixture's *shape* (field count, rough magnitude) without preserving its
  content.

Never commit a fixture copy-pasted straight from a production system.

---

## Performance Considerations

Before adding a new collector, or a new request/command inside an existing one, check:

1. **Can it be merged** with an existing call? (same endpoint or command, different fields)
2. **Is it cacheable?** (does the underlying data actually change on every scrape, or far less
   often)
3. **What's the cardinality?** (a metric with one series per item, on a large fleet, adds up
   fast, consider a flag to opt out of the highest-cardinality label, documented in
   `docs/configuration.md` alongside the rest of this collector's flags)
4. **Should it be opt-in?** (an expensive call belongs behind a `--collector.<name>` that
   defaults to disabled, not a hope that nobody enables it on a busy target)

---

## Common Pitfalls

### Zero metrics is not the same as zero-valued metrics

A collector that returns from `Collect` without sending anything (even on a legitimate "no
data this scrape" outcome) is indistinguishable, to the shared `StatusTracker`, from one that
just failed. Always send your metrics, with `0`/empty values when there's genuinely nothing to
report. See the collector-authoring rule above.

### Two metrics with the same label set on one descriptor break the whole collector's scrape

`Registry.Gather` rejects a scrape where two `MustNewConstMetric` calls share both a
descriptor and an identical label set. If your parser can legitimately produce duplicate keys
from raw input, reject or de-duplicate them in the pure parsing step (fail closed). Don't let
it reach `Collect` and surface as a `Gather` error downstream.

### Flags read before `kingpin.Parse()` are always zero

`register()` declares each collector's flags eagerly (kingpin requires every `Flag()` call to
happen before `Parse()`), but the constructor closure it stores is only invoked *after*
`Parse()` runs, in `main()`. A closure that dereferences a flag pointer at declaration time
instead of at construction time will always see the flag's zero value, never the parsed
default or the user's override.

---

## Reviewing contributions

When a change doesn't meet the bar above, name the blocking concern, not a judgment on the
contributor: say what the change gets right, say exactly what blocks merging today, and say
what happens next. "I'm not closing the PR, I just want to make sure we get this right
together" beats silence or a cold rejection every time.

---

## Releasing

Everything before a release tag (branch strategy, integrating contributions with credit,
validating against a real target) follows the workflow in
[docs/release-process.md](docs/release-process.md). The companion
[docs/validation-checklist.md](docs/validation-checklist.md) is a copy-pasteable,
command/expected/if-it-fails procedure written so a human or an AI agent can run it without
prior context.
