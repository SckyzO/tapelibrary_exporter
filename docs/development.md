# Development Guide

> Back to [README](../README.md)

## Development

### Prerequisites

- A container engine (Docker or Podman): every quality-gate `make` target (`build`, `test`,
  `race`, `vet`, `lint`, `vuln`, `check`, `report`, `report-deps`) runs inside a pinned tools
  image. This is the only requirement.
- Go 1.26+ only if you want the host-toolchain escape hatch (`NATIVE=1`), or just editor /
  language-server support.

### Building from source

1. Clone this repository:

   ```bash
   git clone <this repository's clone URL>
   cd tapelibrary_exporter
   ```

2. Build the exporter binary:

   ```bash
   make build
   ```

   The binary lands in `bin/tapelibrary_exporter`.

### Running tests

```bash
make test          # full unit-test suite
make race          # same, with the race detector
```

### Development commands

**Run the linter:**

```bash
make lint
```

**Clean build artifacts:**

```bash
make clean
```

**Run the exporter locally:**

```bash
bin/tapelibrary_exporter --web.listen-address=:9170
```

**Query metrics:**

```bash
curl http://localhost:9170/metrics
```

**Liveness probe:**

```bash
curl http://localhost:9170/healthz
# returns: ok
```

**Simulate CI locally:**

If this repo has a GitHub Actions layer (`.github/workflows/`), [`act`](https://github.com/nektos/act)
replays those workflows locally before you push, requires Docker, and only applies if the
repo actually has a `.github/workflows/` directory (for example, not on a `--forge none`
scaffold):

```bash
act push
```

**No engine available, or debugging an engine-specific failure:**

If neither Docker nor Podman is detected, every tooling target silently falls back to the
host Go toolchain and prints an "unpinned tool versions" warning banner instead of failing.
Force that same host path even when an engine *is* available with:

```bash
NATIVE=1 make build
```

---

## Local iteration loop

There is no bundled integration-test cluster for `https://<library-address>/web/api/v1` itself: point the binary
at a real instance, or the closest sandbox/staging equivalent you have, and iterate from
there. See the Definition of Done in [../CONTRIBUTING.md](../CONTRIBUTING.md) for the full
loop: build -> test -> lint -> `docs-check` -> run against a real target -> workload ->
validate a metric -> check logs -> `make check`.

To try the containerized image instead of the bare binary:

> Needs a `Dockerfile` and `docker-compose.yml` at the repo root. If your copy of this repo
> doesn't have them yet, the bare-binary loop above works with no extra setup.

```bash
make docker-build
make docker-run    # starts the compose stack; override DOCKER_IMAGE=/DOCKER_TAG= for a different image/tag
```

A minimal, distroless variant is also available (`Dockerfile.minimal` /
`docker-compose.minimal.yml`, ~15MB, always non-root, no shell, no package manager), useful
for confirming the exporter also runs on the hardened image, or for iterating with the
smallest practical attack surface:

```bash
make docker-build-minimal
make docker-run-minimal    # starts docker-compose.minimal.yml
```

Its compose `container_name` has a `-minimal` suffix (distinct from the standard stack's), so
the two can run together, but both default to the same host port (`9170`). To
run them at the same time, set `HOST_PORT` before starting the second one:
`HOST_PORT=<free-port> make docker-run-minimal`.
