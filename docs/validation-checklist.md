# Release Validation Checklist

> Back to [README](../README.md) - see also [release-process.md](release-process.md)

A step-by-step procedure to validate a release candidate of `tapelibrary_exporter` end-to-end
against a real (or realistic sandbox) target. Designed so that a human or an AI agent can
execute it without prior context.

**Each step has:**

- A **Command** - a copy-pasteable shell block (adapt the placeholders to your own target).
- An **Expected** outcome - what the command should print or produce.
- An **If it fails** section - how to diagnose and what to fix before continuing.

**Pre-requisites**:

- The branch you want to validate is checked out and `make build` succeeds.
- A container engine (Docker or Podman) is running, for the containerized `make` targets.
- You have access to a real instance of `https://<library-address>/web/api/v1`, or a sandbox/staging equivalent.

---

## Step 1 - Pre-flight static checks

Confirm the workspace is clean and the binary builds.

### Command

`make check`, `make report`, `make build`, and `make race` all run inside a containerized
toolchain (`scripts/docker/tools/`). The only host requirement is a container engine.

```bash
cd "$(git rev-parse --show-toplevel)"
git status --short
make check      # vet + lint + test + vuln + actionlint + zizmor + deadcode + docs-check, containerized
make report     # offline goreportcard.com equivalent, containerized
make build      # full ldflags build (produces bin/tapelibrary_exporter)
bin/tapelibrary_exporter --version; echo "exit=$?"
bin/tapelibrary_exporter --help >/dev/null; echo "exit=$?"
bin/tapelibrary_exporter >/dev/null 2>&1; echo "no-config exit=$?"
```

### Expected

- `git status --short` shows only intentional changes.
- `make check` exits 0.
- `make report` exits 0 - grade >= B (a drop in grade versus the default branch is a release
  blocker).
- `make build` produces `bin/tapelibrary_exporter` without warnings.
- `bin/tapelibrary_exporter --version` prints a first line of the form `tapelibrary_exporter, version
  <VERSION> (branch: <BRANCH>, revision: <REVISION>)`, followed by indented `build user`/
  `build date`/`go version`/`platform`/`tags` lines: branch and revision are separate fields,
  never fused into the version string itself. For an unreleased build, `<VERSION>` comes from
  `git describe --tags --always --dirty --abbrev=7` and typically looks like `v0.2.0-dirty` or
  `v0.2.0-3-gabcdef1`, depending on how many commits/tags exist since the last tag.
- **`--version` and `--help` both exit 0**, and neither needs `--config.file`. Check the exit
  code, not just the output: this build requires `--config.file` to run, and a regression that
  enforced it too early made both of them print that refusal and exit 1 instead (fixed
  2026-08-04). Packaging, CI and container healthchecks all call `--version` on a binary they
  have no configuration for.
- **Running it with no arguments still exits 1**, with
  `the multi-instance target model requires --config.file`. That refusal is required behaviour,
  not a bug: the two assertions above must not be satisfied by weakening it.

> **Run this step against the artefact `make build` produces, never a bare `go build`.**
> The 2026-08-04 regression above was invisible to every other check in this file, because
> every one of them starts the exporter *with* a configuration file rather than asking the
> binary to describe itself.

### If it fails

- **`make check` errors**: fix before continuing. Don't validate a branch that doesn't pass
  static checks.
- **`make report` drops a grade**: read the per-check breakdown - most commonly a new function
  above the `gocyclo` threshold. Refactor, or annotate with a `//nolint` comment and a
  rationale.
- **First `make` call is slow**: the tools image is being built. Subsequent runs reuse the
  cache and start in seconds.

---

## Step 2 - Bring up (or connect to) a real target

There's no bundled integration-test harness for `https://<library-address>/web/api/v1` itself - replace this step
with whatever your own target's staging/sandbox setup looks like.

### Command

```bash
# Adapt to your own target, for example:
# docker compose -f staging/docker-compose.yml up -d
# or: ssh staging-host "systemctl start <your-data-source>"
```

### Expected

Your target is reachable and in a known, reproducible state before you point the exporter at
it.

### If it fails

Fix the target environment before continuing - a validation run against a flaky or
unreachable target produces noise, not signal.

---

## Step 3 - Restart the exporter with every collector and debug logs

Restart it with `--log.level=debug` so you can see everything it does. Every collector
defaults to enabled, so none needs to be named explicitly; pass `--no-collector.<name>` only
to take one out.

`--config.file` is **mandatory** on this multi-instance build: it is what lists the libraries
to watch, and there is no flag equivalent. Point it at a file describing at least one real
target.

### Command

```bash
pkill -f tapelibrary_exporter 2>/dev/null
sleep 1
nohup bin/tapelibrary_exporter \
  --config.file=/etc/tapelibrary_exporter/config.yml \
  --web.listen-address=:9170 \
  --log.level=debug \
  > /tmp/exporter.log 2>&1 &
sleep 2
curl -s http://localhost:9170/healthz
```

### Expected

- Final output line is `ok` (from `/healthz`).
- `/tmp/exporter.log` contains `msg="Collector enabled"` for every collector you passed.
- One `msg="Starting exporter server..."` line.
- A startup line from `prometheus/exporter-toolkit` noting the listen address (exact wording
  may vary by exporter-toolkit version).
- **No unexpected ERROR or WARN entries** during startup.

### If it fails

- `address already in use`: another instance is alive. Re-run the `pkill` and check
  `ps aux | grep tapelibrary_exporter`.
- `Collector enabled` missing for one collector: a flag name changed. Compare with
  `bin/tapelibrary_exporter --help`.
- Warnings during startup: capture them - they often signal a real bug introduced by the
  branch.

---

## Step 4 - Verify a full scrape with no errors

A single `/metrics` request must succeed without producing an error log entry.

### Command

```bash
curl -s -o /tmp/scrape.txt -w "HTTP %{http_code} in %{time_total}s\n" \
  http://localhost:9170/metrics

echo "=== Metric series count ==="
grep -cE "^tapelibrary_" /tmp/scrape.txt
echo "=== Errors / warnings in logs ==="
grep -cE "level=ERROR|level=WARN" /tmp/exporter.log
```

### Expected

- HTTP 200, scrape duration well under 1 second. A scrape never contacts a library: every
  collector serves its cache, so this is fast even while a refresh is in flight.
- Metric series count >= 1 per enabled collector. Expect a few thousand per watched library
  (the reference fleet ranges 2 566 to 5 473, scaling with drive count; see
  `docs/exporter-journal.md`'s cardinality budget).
- **A collector reporting `tapelibrary_exporter_collector_success 1` is not proof it has
  data.** That metric says Collect returned something, and a background collector always
  returns at least its freshness gauge. Check
  `tapelibrary_<subsystem>_last_refresh_timestamp_seconds` is non-zero for every collector:
  a `0` means no refresh has ever completed. `CollectorNeverRefreshed` alerts on exactly
  this, and on 2026-08-03 all nineteen collectors reported success while collecting nothing.
- **0 errors and 0 warnings** in the log file.

### If it fails

- HTTP 500/503: the exporter failed mid-scrape. Read the last lines of `/tmp/exporter.log` for
  a panic trace (should not happen - every collector is wrapped so a panic is caught and
  reported as `collector_success=0`, not a crash).
- Scrape duration far above your target's own response time: a collector is slow or timing
  out. Inspect `tapelibrary_exporter_collector_duration_seconds` to find which one.
- Any `level=ERROR`: a real problem to investigate before tagging.

---

## Step 5 - Validate all collectors succeeded

Each collector exposes a success gauge via the shared status tracker. They must all be `1`.

### Command

```bash
grep -E "^tapelibrary_exporter_collector_success" /tmp/scrape.txt | sort
```

### Expected

One line per enabled collector, all ending with ` 1`. For a fresh scaffold, exactly two lines
(names depend on the I/O flavor):

```
tapelibrary_exporter_collector_success{collector="example"} 1
tapelibrary_exporter_collector_success{collector="http_client_requests"} 1
```

(the second line reads `collector="command_exec"` on the CLI flavor). Add one line here for
every collector you've added since.

### If it fails

- Any `... 0` value: that collector failed. Find its error in `/tmp/exporter.log` and fix it
  before tagging. Remember: a collector that emits zero metrics on a technically-successful
  call is *still* reported as `0` here (see the collector-authoring rule in
  [../CONTRIBUTING.md](../CONTRIBUTING.md)) - check whether the underlying call actually
  failed, or whether the collector just has nothing to report and should emit a zero-valued
  metric instead of nothing.
- Missing line for a collector: the flag wasn't picked up. Re-check the command in Step 3.

---

## Step 6 - Confirm the exporter's outbound calls actually happened

This is the moment to confirm any release-specific change is wired through.

### Command

```bash
grep -E '^tapelibrary_exporter_(request|command)_duration_seconds' /tmp/scrape.txt
```

### Expected

At least one `outcome="success"` observation for every collector that talks to
`https://<library-address>/web/api/v1`. Any `outcome="error"` observation is a signal to go read
`/tmp/exporter.log` for the corresponding failure.

### If it fails

- No observations at all: the collector never actually called out - check whether it's
  enabled and whether its target/timeout flags point where you expect.
- Unexpected `outcome="error"` counts: a regression in the release branch. Check the matching
  `*Data` method in `internal/collector/`.

---

## Step 7 - Generate a realistic workload

An idle target doesn't exercise every code path. Generate a representative workload against
`https://<library-address>/web/api/v1`, then re-scrape.

### Command

```bash
# Exercise your target however is representative for it, then:
curl -s http://localhost:9170/metrics > /tmp/scrape_workload.txt
wc -l /tmp/scrape_workload.txt
```

### Expected

More series, and non-default values, versus the idle-target scrape in Step 4.

### If it fails

If the workload didn't actually reach the target (wrong credentials, wrong endpoint, wrong
command arguments), fix that before re-scraping - an idle-equivalent scrape after a workload
step tells you nothing.

---

## Step 8 - Diff exposed metrics against `docs/metrics.md`

Catch undocumented or stale metrics. The exporter is the source of truth. `make docs-check`
(part of `make check`) automates this; to do it by hand:

### Command

```bash
grep -oE 'tapelibrary_[a-z_]+' /tmp/scrape_workload.txt | LC_ALL=C sort -u > /tmp/exposed.txt
grep -oE '`tapelibrary_[a-z_]+`' docs/metrics.md | tr -d '`' | LC_ALL=C sort -u > /tmp/doc.txt

echo "=== Documented but NOT exposed ==="
comm -23 /tmp/doc.txt /tmp/exposed.txt

echo
echo "=== Exposed but NOT documented ==="
comm -13 /tmp/doc.txt /tmp/exposed.txt
```

### Expected

- **Documented but not exposed** is small and *contextual* - every entry has a defensible
  reason for not appearing against this particular target (a collector needs specific
  conditions to emit anything).
- **Exposed but not documented** is **empty**, or contains only histogram suffixes
  (`_bucket`, `_count`, `_sum`), which are implicit from the parent metric name.

### If it fails

Any new exposed-but-not-documented name (other than histogram suffixes): open
`docs/metrics.md`, add a row under the relevant collector's section, commit before tagging.

---

## Step 9 - Release-specific assertions

Replace this section's contents for each release. The pattern is the same:

1. Identify the bug or feature changing in this release.
2. Find a metric or log signal that proves the fix is wired.
3. Assert it explicitly with `grep`.

### Example shape

```bash
echo "=== Issue #N fix: <short description> ==="
grep -E '<the signal that proves it>' /tmp/scrape.txt
# Expected: a line appears / a value crosses a threshold / a label is present
```

### Expected

Each assertion prints the expected signal. Any failure here is **a release blocker** - the fix
didn't make it into the binary.

### If it fails

Re-check the binary version (`bin/tapelibrary_exporter --version`) matches the branch, rebuild,
and redeploy to your target before re-running this step.

---

## Step 10 - Visual inspection of the health dashboard

If you've provisioned the bundled Grafana health dashboard (see
[`../monitoring/`](../monitoring/)), give it a final visual pass.

### Command

Open the dashboard in Grafana (or drive it with Playwright/Chromium for an AI agent) and walk
through every panel.

### Expected

- No "No data" on panels that should have values (collector health, scrape duration).
- No PromQL parse errors (red banner).
- Time ranges show plausible values.

### If it fails

Open the panel's edit view, copy its expression, run it directly in Prometheus. If Prometheus
also returns nothing, the metric was renamed or removed in this branch - update the dashboard
JSON or the metric name.

---

## Step 11 - Tear down

When done, free the resources you brought up in Step 2.

### Command

```bash
pkill -f tapelibrary_exporter
# plus whatever teardown your own target's staging setup needs
```

---

## Summary checklist (for quick reference)

- [ ] Step 1 - `make check`, `make report` (grade >= B), `make build` all green
- [ ] Step 2 - Target environment up and reachable
- [ ] Step 3 - Exporter restarted with every collector + debug logs
- [ ] Step 4 - `/metrics` returns 200, 0 errors/warnings in the log
- [ ] Step 5 - Every collector reports `success = 1`
- [ ] Step 6 - Outbound calls observed with the expected outcome
- [ ] Step 7 - Workload submitted, metrics reflect it
- [ ] Step 8 - `docs/metrics.md` <-> `/metrics` diff is clean
- [ ] Step 9 - Release-specific assertions pass
- [ ] Step 10 - Health dashboard renders with data
- [ ] Step 11 - Target environment torn down

Once every box is ticked, the release is ready for the real-target validation step described
in [release-process.md § Test on a real target before the final tag](release-process.md#test-on-a-real-target-before-the-final-tag).
