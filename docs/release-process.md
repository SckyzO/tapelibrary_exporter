# Release Process

> Back to [README](../README.md)

This document describes the maintainer workflow for cutting a release of
`tapelibrary_exporter`. It is the playbook for any future patch, minor, or major release.

The goal is to keep releases:

1. **Coherent**: every change in a release fits the release theme (patch = bug fixes only;
   minor = features and non-breaking improvements; major = breaking changes).
2. **Reviewable**: atomic commits, Conventional Commit messages, breaking changes flagged
   explicitly.
3. **Trustworthy**: every fix lands with a non-regression test, and every metric in
   `docs/metrics.md` matches what `/metrics` actually exposes.
4. **Respectful of contributors**: community PRs are credited via `Co-authored-by`, not
   silently absorbed.

---

## 1. Open the release branch

Always release from a dedicated branch. Never commit straight to the default branch.

```bash
git checkout main && git pull
git checkout -b fix/vX.Y.Z       # use feat/vX.Y for minor releases
```

Naming convention:

| Branch prefix | Used for |
|---|---|
| `fix/vX.Y.Z` | Patch release: bug fixes only |
| `feat/vX.Y` | Minor release: features and non-breaking improvements |
| `release/vX` | Major release: breaking changes |

---

## 2. Triage the backlog

Before opening the branch, list everything that *could* go in (if you use GitHub):

```bash
gh pr list    --repo sckyzo/tapelibrary_exporter --state open
gh issue list --repo sckyzo/tapelibrary_exporter --state open
```

For each item, decide:

- **In scope** for this release: note the issue/PR number in your scratchpad.
- **Out of scope** but actionable: comment with the planned release (e.g. "tracked for
  v1.3").
- **Stale**: close with a polite explanation.

Aim for a release that ships **one theme** (for example, "silent metric loss in collector X +
community PR backlog"). Avoid mixing unrelated themes in the same release.

---

## 3. Integrate community PRs

For each PR you accept, follow the same loop:

### 3.1 Analyse

For every PR, run a four-step analysis:

1. **What it claims to fix**: read the issue and the PR description.
2. **What the diff actually changes**: `gh pr diff <N>`.
3. **Whether it conflicts with the current branch**: does it touch lines you've already
   modified in this release?
4. **Whether it changes a metric's name, type, or labels**: anything visible to an operator.

If any of these aren't clear, comment on the PR for clarification before integrating.

### 3.2 Integrate locally with credit

Rather than merging community PRs through the forge's UI one by one (which scatters the
release across many merge commits), integrate the diff locally on the release branch with a
`Co-authored-by:` trailer:

```bash
# Apply the change manually (edit the files, or apply the PR's diff)
git add <files>
git commit -m "$(cat <<'EOF'
fix(<collector>): <one-line summary> (#<issue>, #<pr>)

<longer explanation>

Refs: #<issue>
Co-authored-by: <author> <<author>@users.noreply.github.com>
EOF
)"
```

This preserves the contributor's authorship on the forge's contributor page without
scattering the release across many separate merges.

### 3.3 One commit = one logical change

If a PR fixes two distinct bugs, split it into two commits. Each commit must:

- Compile and pass `go test` on its own (so `git bisect` works).
- Have its own non-regression test if applicable.
- Carry the `Co-authored-by:` trailer.

### 3.4 Test of non-regression

Every code change must come with a test that:

- Fails before the fix (verify this briefly: write the test first, run it, watch it fail).
- Passes after the fix.
- Reuses the project's existing fixtures in `internal/collector/testdata/` when possible.

If the PR doesn't come with a test, write one. If a fix is purely defensive (for example,
logging on an empty parse instead of panicking), a unit test that exercises the empty path is
enough.

---

## 4. Defensive audit

For each bug you fix, ask: **is the same class of bug present anywhere else in the
codebase?**

Example: a fix for a truncated field in one collector's parser often means the same
fixed-width parsing pattern exists in a sibling collector that reads a similarly-shaped
response. Search for it:

```bash
# Find every collector that does similar raw-string field splitting
grep -rn 'strings.Fields\|strings.Split' internal/collector/
```

Defensive fixes ship as separate commits with `(defensive)` in the summary, and no
`Co-authored-by:` (they aren't derived from the PR that prompted them).

---

## 5. Validate continuously

**Every command below runs inside a containerized toolchain** (a container engine is the
only requirement on the developer machine), and the result is identical on every host. See
[`scripts/docker/tools/`](../scripts/docker/tools/) for the pinned tools image.

After every commit, run:

```bash
make check    # vet + lint + test + vuln + actionlint + zizmor + deadcode + docs-check + rules-check, containerized
```

**Before tagging, both of these must be green (release blockers):**

```bash
make check    # exit 0 required
make report   # exit 0 -> grade >= B; aim for A or A+
```

`make report` is the offline equivalent of [goreportcard.com](https://goreportcard.com): runs
`gofmt -s`, `go vet`, `gocyclo`, `ineffassign`, `misspell`, and a `LICENSE` check, prints a
per-check score, and assigns a global grade. It exits non-zero below grade B, so it can gate
CI or a pre-commit hook.

Also run at least once before tagging:

```bash
make race     # race detector (containerized)
make build    # full ldflags build (native or containerized)
```

If `make race` fails on a pre-existing test (not something you introduced), fix it in this
release if cheap, otherwise file a follow-up issue.

If `make report` drops a grade versus the default branch (for example, a new function above
the `gocyclo` threshold), either refactor before tagging or annotate explicitly with a
`//nolint` comment and a rationale.

---

## 6. End-to-end test against a real target

Run the exporter against a real (or realistic sandbox) instance of `https://<library-address>/web/api/v1`, with
every collector enabled and debug logging on, and make release-specific assertions explicit.

The detailed step-by-step playbook is in
**[docs/validation-checklist.md](validation-checklist.md)**, a copy-pasteable
command/expected/if-it-fails procedure designed so a human or an AI agent can execute the
validation end-to-end without prior context.

Short version of what that checklist covers:

```bash
bin/tapelibrary_exporter --web.listen-address=:9170 --log.level=debug &

# Verify the scrape returns 200 and every collector reports success=1
curl -s http://localhost:9170/metrics | grep exporter_collector_success

# Submit a representative workload against your target, then re-scrape
# Diff /metrics against docs/metrics.md (see below)
# Make the release-specific assertions explicit (see the checklist's Step 9)
```

### Spot-check the release theme

For every release, explicitly verify:

- The fix actually fires under realistic conditions (feed the fixture that previously
  triggered the bug and confirm the corrected behavior now shows up).
- A renamed metric uses its new name everywhere, including any dashboard or alert that
  referenced the old one.
- The health dashboard under [`monitoring/`](../monitoring/) still renders.

### Diff exposed metrics against docs

`make docs-check` (part of `make check`) automates this comparison and fails the build on a
lying doc. To inspect it by hand:

```bash
# Names emitted by the live exporter
grep -oE 'tapelibrary_[a-z_]+' /tmp/metrics.txt | LC_ALL=C sort -u > /tmp/exposed.txt

# Names documented in docs/metrics.md
grep -oE '`tapelibrary_[a-z_]+`' docs/metrics.md | tr -d '`' | LC_ALL=C sort -u > /tmp/doc.txt

# Gaps in both directions
comm -23 /tmp/doc.txt /tmp/exposed.txt   # documented but not exposed
comm -13 /tmp/doc.txt /tmp/exposed.txt   # exposed but not documented
```

Treat "exposed but not documented" entries as bugs to fix in this release (unless they're
histogram `_bucket`/`_count`/`_sum` suffixes, which are implicit from the parent metric name).
"Documented but not exposed" entries are usually contextual (a collector needs specific
conditions on the target to emit anything); verify each one before declaring the doc clean.

---

## 7. Update documentation

For every change that affects operator-visible behavior:

| File | Update when |
|---|---|
| `CHANGELOG.md` | Always. New version section, only the sub-headings that have content |
| `docs/metrics.md` | Any metric added, renamed, removed, or label-changed |
| `docs/configuration.md` | Any new flag, default change, or behavior toggle |
| `README.md` | Headline features only |
| `monitoring/prometheus/*.yml` | Any metric rename/drop, or a new opt-in collector that recording rules or alerts depend on |
| `monitoring/grafana/*.json` | Any rename or drop of a metric actually used in a panel |

For breaking changes, include a **migration table** in the CHANGELOG:

```markdown
| Old | New |
| --- | --- |
| `tapelibrary_foo_total` (Counter) | `tapelibrary_foo` (Gauge) |
```

---

## 8. Push and open the PR

```bash
git push -u origin fix/vX.Y.Z
gh pr create --base main --head fix/vX.Y.Z \
  --title "fix(vX.Y.Z): <theme> + N community PRs" \
  --body-file /tmp/pr_body.md
```

PR body structure:

```
## Summary
1-3 bullets.

## Breaking change          (omit if none)
Migration table + rationale (how long has the affected metric existed).

## Bug fixes
### <Theme of this release>
### Integrated community PRs   (table: PR | issue | author | subject)
### Other hardening

## Test plan
Checklist already ticked by the time the PR opens.

## Follow-ups (next release)
Bullet list with issue numbers.
```

Self-review the diff on the forge before requesting outside review.

---

## 9. Post-merge cleanup

Once the PR is merged to the default branch:

### Close integrated community PRs

```bash
for pr in <list of integrated PRs>; do
  gh pr close "$pr" --repo sckyzo/tapelibrary_exporter \
    --comment "Integrated into vX.Y.Z (see release notes). \
Thanks for the contribution - your authorship is preserved via Co-authored-by in the commit."
done
```

### Respond to issues

For issues that were fixed: post a short comment pointing to the release and close.

For issues that are *acknowledged but planned for a later release*: leave them open and
comment with the planned milestone and any workaround available in the meantime.

### Test on a real target before the final tag

A local or CI validation environment catches structural bugs, but it rarely has the full
variability of a real target: real-world scale, real error conditions, version-specific
quirks. **Before tagging the final release, run the binary against an actual target.**

Two approaches depending on risk:

#### A. Release candidate tag (recommended for minor/major and breaking changes)

For releases that rename metrics, change types, or touch shared internals, tag a release
candidate first. If you use the GitHub layer, `prerelease: auto` in `.goreleaser.yaml`
publishes it as a pre-release automatically, so users won't grab it by accident from
automation that only tracks the floating `latest` pointer.

```bash
git checkout main && git pull
git tag -a vX.Y.Z-rc1 -m "vX.Y.Z release candidate 1"
git push origin vX.Y.Z-rc1
```

Deploy the rc1 binary to a staging target, leave it running through at least one full
work cycle, and watch for:

- Unexpected dips or spikes in any metric series.
- New error logs from the exporter.
- Dashboard panels suddenly showing "No data" where they used to.
- `tapelibrary_exporter_collector_duration_seconds` getting much longer.

If you find an issue: fix it on a hotfix branch, merge, then tag `vX.Y.Z-rc2`. Repeat as
needed. Once stable, ship the final tag (see below).

#### B. Local build + manual deploy (for trivial patches)

For a patch that only touches docs, tests, or a clearly isolated bug, the RC dance is
overkill. Build the binary from the merged default branch and ship it manually to staging:

```bash
git checkout main && git pull
make build

scp bin/tapelibrary_exporter staging:/usr/local/bin/tapelibrary_exporter.next
ssh staging "
  /usr/local/bin/tapelibrary_exporter.next --version &&
  systemctl stop tapelibrary_exporter &&
  mv /usr/local/bin/tapelibrary_exporter.next /usr/local/bin/tapelibrary_exporter &&
  systemctl start tapelibrary_exporter
"
```

Watch for the same signals as approach A, for a shorter window, before tagging.

#### Decision matrix

| Change type | Approach |
|---|---|
| Breaking change (metric rename/retyped, label change) | **A: RC tag**, staging, full work cycle |
| New collector / new exposed metric | **A: RC tag**, staging, a few hours |
| Bug fix to an existing collector | **B: local build**, ~30 min validation |
| Docs / test-only patch | None required, `make check` is enough |

### Tag the final release

Once you're confident:

```bash
git tag -a vX.Y.Z -m "vX.Y.Z - <short headline>"
git push origin vX.Y.Z
```

If you use the GitHub layer, CI's release workflow picks up the tag, re-verifies the build
(`make check`), then runs GoReleaser, which cross-compiles, signs, SBOMs, and publishes the
binaries and container images (see [Security & supply chain](../README.md#security--supply-chain)).
Without a forge, run the equivalent locally:

```bash
make release-snapshot   # builds every archive into dist/, publishes nothing
```

Like every other target here it runs in a container, so GoReleaser does not need to be
installed, and it supplies the three environment variables the release workflow sets for
GoReleaser (`BUILD_USER`, `BUILD_DATE`, `GO_VERSION`) — without them the run dies on
`map has no entry for key "BUILD_USER"` before building anything.

**`BUILD_USER` must not contain whitespace.** GoReleaser folds its `ldflags` into a single
string, so `Name <email>` becomes two linker arguments and the build fails printing the
linker's usage. `make release-snapshot` therefore passes git's `user.email`, matching what
`make docker-build` already does; CI passes `github.actor` for the same reason. Quoting the
value inside `.goreleaser.yaml` does not help.

`make release-snapshot` prints the contents of the linux/amd64 archive when it finishes,
which is the cheapest way to catch a file that should ship and doesn't. To validate both
GoReleaser configs without building anything:

```bash
make release-check
```

GoReleaser's own SBOM step only ever covers the release **archives** (CycloneDX, via its
`sboms:` block) plus an automatic, registry-embedded **SPDX** attestation on each container
image (`dockers_v2 … sbom: "true"`), it cannot generate a CycloneDX SBOM for an image it
builds. For the container image's own canonical CycloneDX SBOM, run `make sbom-image` (syft,
the same tool GoReleaser uses for the archives, pulled as a pinned container — nothing to
install) against the tag you just published, e.g.:

```bash
make sbom-image IMAGE=ghcr.io/sckyzo/tapelibrary_exporter:vX.Y.Z
```

---

## 10. Handle PRs that don't ship in this release

When you receive a PR that has the right idea but doesn't fit the current release (wrong
scope, missing tests, conflicts with in-flight work), the right answer is **not** to leave it
rotting.

Comment on the PR with:

1. **What's good**: what the PR gets right.
2. **What blocks merging today**: concrete blockers with reasons, not opinions.
3. **What the plan is**: which release you'll integrate it in, and what you'll adapt or add
   (tests, flags, a dashboard panel).
4. **A concrete signal of intent**: for example, "I'll adapt and ship this in vX.Y this
   month. You'll be credited via Co-authored-by."

Close with a variant of: *"I'm not closing the PR, keeping it open as the reference
discussion while I adapt. Thanks again for the contribution."*
