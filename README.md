# tapelibrary_exporter

[![Go Report Card](https://goreportcard.com/badge/github.com/sckyzo/tapelibrary_exporter)](https://goreportcard.com/report/github.com/sckyzo/tapelibrary_exporter)
[![License](https://img.shields.io/badge/license-gpl-3.0-blue.svg)](LICENSE)

A [Prometheus](https://prometheus.io/) exporter that collects metrics from `https://<library-address>/web/api/v1`
and exposes them at `/metrics` for scraping.

## Table of contents

- [Features](#features)
- [Get started](#get-started)
- [Endpoints](#endpoints)
- [Configuration & development](#configuration--development)
- [Metrics](#metrics)
- [Observability](#observability)
- [Security & supply chain](#security--supply-chain)
- [Contributing](#contributing)
- [License](#license)

## Features

- Every collector is optional and toggles independently via `--collector.<name>` /
  `--no-collector.<name>` flags.
- Per-collector health metrics (`tapelibrary_exporter_collector_success`,
  `tapelibrary_exporter_collector_duration_seconds`) so a single failing collector never
  hides behind an otherwise-healthy scrape.
- OpenMetrics exposition format on `/metrics`, with a liveness probe at `/healthz` for
  Kubernetes or systemd.
- TLS and Basic Authentication available out of the box via `--web.config.file`.
- An optional YAML configuration file, `--config.file`, for setting flags from a file and
  for the authentication and TLS this exporter uses on its own outbound requests. The flag
  defaults to empty, so nothing is read until you pass it. Start from
  [`config.example.yml`](config.example.yml) in this repository; the format is documented in
  [docs/configuration.md](docs/configuration.md).
- Signal-aware shutdown: `SIGTERM`/`SIGINT` drain in-flight requests before exiting.
- `--exporter.max-requests-per-target` optionally bounds outbound concurrency (single-target
  and multi-instance builds; absent on multi-target). See
  [docs/configuration.md](docs/configuration.md#bounding-outbound-concurrency).
- Container-first tooling: a container engine (Docker or Podman) is the only development
  requirement; every quality gate also runs on the host Go toolchain via `NATIVE=1`.
- Multi-arch, signed container images, each with a CycloneDX SBOM available via
  `make sbom-image` (see [Security & supply chain](#security--supply-chain)).
- Starter Prometheus alerting rules and a ready-to-import Grafana health dashboard under
  [`monitoring/`](monitoring/).

## Get started

### From source

```bash
git clone <this repository's clone URL>
cd tapelibrary_exporter
make build
bin/tapelibrary_exporter --web.listen-address=:9170
```

```bash
curl http://localhost:9170/metrics | head
```

See [CONTRIBUTING.md](CONTRIBUTING.md) for the full development setup: a container engine
is the only hard requirement; a host Go toolchain is optional (`NATIVE=1`).

### Docker

These targets need a `Dockerfile` and `docker-compose.yml` at the repo root. If your copy of
this repo doesn't have them yet, [build from source](#from-source) above works with no extra
setup.

Build and run a local image with the bundled Make targets:

```bash
make docker-build
make docker-run   # starts the compose stack; override DOCKER_IMAGE=/DOCKER_TAG= for a different image/tag
```

A minimal, distroless variant (`Dockerfile.minimal` / `docker-compose.minimal.yml`, ~15MB,
always non-root, no shell, no package manager) builds and runs the same way. Reach for it
once you've confirmed the standard image works for your target, or whenever the smaller
attack surface matters more than being able to `docker exec` in for debugging:

```bash
make docker-build-minimal
make docker-run-minimal   # starts docker-compose.minimal.yml; same DOCKER_IMAGE=/DOCKER_TAG= override
```

The minimal stack's `container_name` has a `-minimal` suffix so the two stacks never collide,
but they still default to the same host port (`9170`). To run both at the same
time, give the second one a free port: `HOST_PORT=<free-port> make docker-run-minimal`.

Once a release is tagged, GoReleaser (see [docs/release-process.md](docs/release-process.md))
publishes two image variants for every release (a standard image and a minimal,
distroless one), both signed and both multi-arch. Verification recipes are in
[Security & supply chain](#security--supply-chain) below.

### Pre-compiled binary

Tagged releases publish signed, checksummed binaries with a CycloneDX SBOM attached: see
[docs/release-process.md](docs/release-process.md).

## Endpoints

| Endpoint | Description |
|---|---|
| `/metrics` | Prometheus scrape endpoint (OpenMetrics enabled) |
| `/healthz` | Liveness probe: returns `200 OK` as long as the HTTP server is up, independently of whether `https://<library-address>/web/api/v1` itself is reachable |
| `/` | Landing page with a link to `/metrics` |

Multi-target builds (`--target-model multi`, http flavor only) also expose:

| Endpoint | Description |
|---|---|
| `/probe?target=<url>` | Probes one target on demand: a fresh registry and collector set scoped to `target`, plus `probe_success`/`probe_duration_seconds`, see [docs/configuration.md](docs/configuration.md) and [SECURITY.md](SECURITY.md) (SSRF posture) |

Multi-target and multi-instance builds (`--target-model multi` or
`multi-instance`) also expose:

| Endpoint | Description |
|---|---|
| `POST /-/reload` | Reloads `--config.file` in place, no restart. Behind `--web.enable-lifecycle` (default off); `SIGHUP` always works and needs no flag. See [Configuration reload](docs/configuration.md#configuration-reload-multi-and-multi-instance-builds-only) |

A single-target build (the default) has no `/-/reload`: see
[docs/configuration.md](docs/configuration.md) for why.

```bash
curl 'http://localhost:9170/probe?target=http://target-a:9100'
```

Point Prometheus at `/probe` with `metrics_path` and the standard
[multi-target relabeling](https://prometheus.io/docs/guides/multi-target-exporter/):
the static targets in `scrape_configs` are the hosts to *probe*, not this
exporter itself: relabeling turns each one into `instance` while
`__address__` is rewritten to this exporter's own address.

```yaml
scrape_configs:
  - job_name: 'tapelibrary_exporter-probe'
    metrics_path: /probe
    static_configs:
      - targets:
          - http://target-a:9170   # hosts this exporter should probe
          - http://target-b:9170
    relabel_configs:
      - source_labels: [__address__]
        target_label: __param_target
      - source_labels: [__param_target]
        target_label: instance
      - target_label: __address__
        replacement: exporter-host:9170   # this exporter's own address
```

## Configuration & development

| Topic | Where |
|---|---|
| Flags, collectors, Prometheus scrape config | [docs/configuration.md](docs/configuration.md) |
| All exported metrics, per-collector reference | [docs/metrics.md](docs/metrics.md) |
| Build, test, lint, local dev loop | [docs/development.md](docs/development.md) |
| Contribution rules and the Definition of Done | [CONTRIBUTING.md](CONTRIBUTING.md) |
| Release process | [docs/release-process.md](docs/release-process.md) |
| Release validation playbook | [docs/validation-checklist.md](docs/validation-checklist.md) |

### Make targets

| Category | Target | What it does |
|---|---|---|
| Build | `make build` | Compiles `bin/tapelibrary_exporter` (containerized; `NATIVE=1` uses the host Go toolchain) |
| | `make clean` | Removes build artifacts (`bin/`) |
| Test | `make test` | Runs the full unit-test suite |
| | `make race` | Tests with the race detector |
| Quality | `make vet` | `go vet` |
| | `make lint` | `golangci-lint run ./...` |
| | `make vuln` | `govulncheck` (reachable-vulnerability scan) |
| | `make check` | `vet` + `lint` + `test` + `vuln` + `actionlint` + `zizmor` + `deadcode` + `docs-check` (the pre-merge gate) |
| | `make report` | Offline goreportcard-equivalent grade; fails below `B` |
| | `make report-deps` | Tabular dependency status (direct + indirect, patch/minor/major) |
| Security | `make actionlint` | Lints `.github/workflows/` (skips gracefully if there is no GitHub layer) |
| | `make zizmor` | Static security analysis for GitHub Actions (same skip behavior) |
| | `make secrets` | `gitleaks` secret scan over the working tree |
| | `make osv` | Dependency vulnerability scan against the OSV database |
| | `make deadcode` | Fails if any unreachable Go code is found |
| | `make docs-check` | `docs/metrics.md` documents no metric/label the code doesn't emit (warns on the reverse gap) |
| Docker | `make docker-build` | Builds a local image tagged `tapelibrary_exporter:dev` |
| | `make docker-run` | Starts the compose stack (override `DOCKER_IMAGE=`/`DOCKER_TAG=`) |
| | `make docker-build-minimal` | Builds the minimal, distroless image tagged `tapelibrary_exporter:dev-minimal` |
| | `make docker-run-minimal` | Starts the minimal compose stack (`container_name` and `HOST_PORT`, see [Docker](#docker) above) |
| | `make sbom-image` | Generates a CycloneDX SBOM for the container image via `syft` (needs `syft` on `PATH`; run after `make docker-build`) |

`make check`, `make report`, and `make report-deps` run **inside a container**: contributors
only need Docker or Podman, no host Go install required.

## Metrics

<!-- BEGIN GENERATED COLLECTORS -->
<!-- Regenerated from docs/metrics.md. Edits inside this block are overwritten. -->
- [`example`](docs/metrics.md#examplecollector)
<!-- END GENERATED COLLECTORS -->

Every metric this exporter can emit, grouped by collector, is documented in
[docs/metrics.md](docs/metrics.md), kept truthful by `make docs-check` (see
[CONTRIBUTING.md](CONTRIBUTING.md)): no metric or label is documented there that the code
doesn't actually emit.

## Observability

Starter Prometheus alerting rules (collector health plus a template for alerts on your own
collectors' metrics) and a ready-to-import Grafana health dashboard live under
[`monitoring/`](monitoring/). Wiring instructions (scrape config, `rule_files`, Alertmanager)
are in [`monitoring/README.md`](monitoring/README.md).

## Security & supply chain

Found a vulnerability? See [SECURITY.md](SECURITY.md) for how to report it privately.

Tagged releases are built by GoReleaser (see [docs/release-process.md](docs/release-process.md))
with verifiable supply-chain provenance:

- **Signed container images.** Every published image is signed keylessly with
  [cosign](https://github.com/sigstore/cosign) (Sigstore/Fulcio) under the release workflow's
  own OIDC identity:

  ```bash
  cosign verify ghcr.io/sckyzo/tapelibrary_exporter:latest \
    --certificate-identity-regexp 'https://github.com/sckyzo/tapelibrary_exporter/.github/workflows/release.yml@.*' \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com
  ```

  Published to GHCR only by default (no extra registry secrets required) - see
  `.goreleaser.yaml`'s `dockers_v2` section to add a second registry. Adjust the identity
  regexp and issuer if releases are cut from a different CI provider (or signed via an
  interactive login) instead of GitHub Actions. Note the image path is lowercased even if
  your GitHub handle isn't (`ghcr.io`/OCI registries require lowercase repository names).

- **Signed checksums.** Every release ships a checksums file plus a Sigstore bundle for
  offline verification:
  `cosign verify-blob --bundle tapelibrary_exporter_checksums.txt.sigstore.json tapelibrary_exporter_checksums.txt`.
- **CycloneDX SBOM.** One `*.sbom.json` per release archive, listing every compiled Go module
  with its version and package URL. The container image gets its own CycloneDX SBOM on demand
  via `make sbom-image` (syft), see [docs/release-process.md](docs/release-process.md); it
  also carries a supplementary SPDX attestation that `docker buildx` embeds in the image
  manifest automatically at build time.
- **Two image variants.** A standard image (includes a shell, easiest to debug) and a minimal
  image (distroless, non-root, no shell, no package manager) are published side by side.
  Prefer the minimal one once you've confirmed the standard image works for your target.
- **Non-root by default.** Neither image variant runs as root.
- **Vulnerability scanning.** `make vuln` (govulncheck, reachable CVEs) and `make osv` (OSV
  database) gate every local `make check`; wire the same commands into CI for continuous
  coverage.
- **Pinned toolchain.** The Go version used to build this exporter and its dev-tooling image
  is pinned in exactly one place: [`scripts/docker/tools/Dockerfile`](scripts/docker/tools/Dockerfile).

## Contributing

PRs and issues welcome. Before sending a contribution, read [CONTRIBUTING.md](CONTRIBUTING.md)
for the Definition of Done, the collector-authoring pattern, and how to add fixtures without
leaking real data. Run `make check` (containerized vet + lint + test + vuln + deadcode) and
`make docs-check` before opening a PR.

## License

This project is licensed under the gpl-3.0 License: see [LICENSE](LICENSE).
