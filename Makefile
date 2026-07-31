# Makefile - container-first dev tooling for tapelibrary_exporter.
#
# build, test, race, vet, lint, vuln, report, and report-deps all run inside
# a single pinned *-tools image (scripts/docker/tools/) that bundles the Go
# toolchain, golangci-lint, and every scanner these targets need - so the
# only host requirement is a container engine (Docker or Podman), never a
# local Go install. The Go version is pinned in exactly ONE place:
# scripts/docker/tools/Dockerfile. There is no separate host GO_VERSION
# here - keeping one in parallel would drift from the image and lie about
# which version actually built your binary.
#
# Two paths when no engine is available (or you don't want one):
#   - No docker/podman detected: tooling silently falls back to the host
#     Go toolchain, printing a visible "unpinned tool versions" warning on
#     every invocation.
#   - NATIVE=1 make <target>: forces the host path even when an engine IS
#     present (e.g. to debug an engine-specific failure).
# Both paths run the identical underlying command; only WHERE it runs
# differs.

EXPORTER_NAME := tapelibrary_exporter

# -eu -o pipefail: fail fast on an unset variable, a non-zero exit anywhere
# in a pipeline, or the last command in one.
SHELL := $(shell which bash) -eu -o pipefail

VERSION    ?= $(shell git describe --tags --always --dirty --abbrev=7 || echo "untagged")
REVISION   ?= $(shell git rev-parse HEAD)
BRANCH     ?= $(shell git rev-parse --abbrev-ref HEAD)
BUILD_USER ?= $(shell git config user.name) <$(shell git config user.email)>
BUILD_DATE ?= $(shell date -u +'%Y-%m-%dT%H:%M:%SZ')

# LDFLAGS inject build metadata into prometheus/common/version - the same
# package every Prometheus exporter uses to serve --version / build_info.
LDFLAGS = \
	-X 'github.com/prometheus/common/version.Version=$(VERSION)' \
	-X 'github.com/prometheus/common/version.Revision=$(REVISION)' \
	-X 'github.com/prometheus/common/version.Branch=$(BRANCH)' \
	-X 'github.com/prometheus/common/version.BuildUser=$(BUILD_USER)' \
	-X 'github.com/prometheus/common/version.BuildDate=$(BUILD_DATE)'

.PHONY: all
all: build

# --- Container-first tooling -------------------------------------------------
# CONTAINER_ENGINE auto-detects docker, then podman, then falls back to
# "none". NATIVE=1 forces the host-toolchain path below even when an engine
# IS available.
CONTAINER_ENGINE ?= $(shell command -v docker >/dev/null 2>&1 && echo docker || (command -v podman >/dev/null 2>&1 && echo podman || echo none))
# Freeze whatever the line above produced (a user override or the $(shell...)
# detection) into an immediately-expanded value. Without this, CONTAINER_ENGINE
# stays a recursive (lazily re-evaluated) variable and every later reference -
# the ifeq below, IN_TOOLS, and every docker-build/docker-run/tools-image
# recipe - would re-run the detection shell command independently. Harmless in
# practice (detection is deterministic within one `make` run) but wasteful,
# and the ifeq below needs a single, stable answer to branch on.
CONTAINER_ENGINE := $(CONTAINER_ENGINE)
NATIVE ?= 0

ifeq ($(NATIVE),1)
RUN_NATIVE := 1
else ifeq ($(CONTAINER_ENGINE),none)
RUN_NATIVE := 1
else
RUN_NATIVE := 0
endif

TOOLS_IMG := $(EXPORTER_NAME)-tools:latest
TOOLS_CTX := scripts/docker/tools

# IN_TOOLS is the program every tooling target below hands its command to.
# Every call site appends "-c '<command>'" itself, mirroring the container
# path's own tools image `ENTRYPOINT ["/bin/bash"]` (which turns
# "-c '<command>'" into `bash -c '<command>'`) - so the exact same call-site
# shape works unchanged whichever path is active:
#   native:        sh -c '<command>'
#   containerised: <engine> run --rm -v "$(CURDIR):/repo" -w /repo IMG -c '<command>'
ifeq ($(RUN_NATIVE),1)
IN_TOOLS := sh
else
IN_TOOLS := $(CONTAINER_ENGINE) run --rm -v "$(CURDIR):/repo" -w /repo $(TOOLS_IMG)
endif

# Prerequisite of every tooling target below: prints the "unpinned
# versions" warning exactly once per invocation (Make only evaluates a
# given prerequisite once per run, no matter how many targets depend on
# it) when running the host path - whether because no engine was found, or
# NATIVE=1 forced it.
.PHONY: native-warning
native-warning:
ifeq ($(RUN_NATIVE),1)
	@echo "----------------------------------------------------------------"
	@echo "No docker/podman detected (or NATIVE=1): running on the HOST Go"
	@echo "toolchain and tools. Versions are NOT pinned - results may differ"
	@echo "from CI. Install docker or podman for reproducible, pinned tooling."
	@echo "----------------------------------------------------------------"
endif

# Builds the tools image if missing or if its Dockerfile changed. A no-op
# on the native path - there is no image to build.
.PHONY: tools-image
tools-image:
ifneq ($(RUN_NATIVE),1)
	@if ! $(CONTAINER_ENGINE) image inspect $(TOOLS_IMG) >/dev/null 2>&1 || \
	   [ $(TOOLS_CTX)/Dockerfile -nt /tmp/.$(TOOLS_IMG).stamp ]; then \
	  echo "Building $(TOOLS_IMG)..."; \
	  $(CONTAINER_ENGINE) build -t $(TOOLS_IMG) $(TOOLS_CTX) && touch /tmp/.$(TOOLS_IMG).stamp; \
	fi
endif

# Build target: compiles bin/tapelibrary_exporter. Containerised like every
# other tooling target here - this is what makes "a container engine is the
# only host requirement" true. (Previously `build` alone required a host Go
# toolchain, contradicting that claim - see the header comment above.)
.PHONY: build
build: native-warning tools-image
	@echo "Building bin/$(EXPORTER_NAME)"
	@mkdir -p bin
	@$(IN_TOOLS) -c "CGO_ENABLED=0 go build -v -ldflags \"$(LDFLAGS)\" -o bin/$(EXPORTER_NAME) ./cmd/$(EXPORTER_NAME)"

# Runs all tests.
.PHONY: test
test: native-warning tools-image
	@echo "Running tests"
	@$(IN_TOOLS) -c 'go test -v ./...'

# Tests with the race detector. Useful to catch concurrency bugs in
# collectors with background goroutines. CGO_ENABLED=1 is required: the
# race detector is implemented in C (the tools image bundles a C toolchain).
.PHONY: race
race: native-warning tools-image
	@echo "Running tests with race detector"
	@$(IN_TOOLS) -c 'CGO_ENABLED=1 go test -race -count=1 ./...'

# go vet.
.PHONY: vet
vet: native-warning tools-image
	@echo "Running go vet"
	@$(IN_TOOLS) -c 'go vet ./...'

# golangci-lint, same tool + config as CI.
.PHONY: lint
lint: native-warning tools-image
	@echo "Running golangci-lint"
	@$(IN_TOOLS) -c 'golangci-lint run ./...'

# govulncheck - Go call-graph vulnerability scanner. Catches reachable
# stdlib / dependency CVEs that image scanners (e.g. Trivy) and module-list
# scanners (osv-scanner, below) miss. Needs network to fetch the vuln DB.
.PHONY: vuln
vuln: native-warning tools-image
	@echo "Running govulncheck"
	@$(IN_TOOLS) -c 'govulncheck ./...'

# actionlint - GitHub Actions workflow linter. Auto-discovers
# .github/workflows/. Despite what its own docs imply, it does NOT treat a
# missing .github/workflows/ as "nothing to lint" - it hard-errors ("no
# project was found in any parent directories") - so a repo scaffolded with
# --forge none (no workflows at all) must guard the call itself, or `check`
# would never pass on such a repo. Verified empirically against actionlint,
# not assumed.
#
# actionlint ALSO hard-requires running inside a git repository - the exact
# same "no project was found in any parent directories" error, for a
# DIFFERENT reason (its own project-root detection, not the workflows
# directory) - fires even with .github/workflows/ present, on a freshly
# scaffolded repo before `git init`. --forge none never exercises this at all
# (it never reaches actionlint in the first place, see above), so this was
# only found once a --forge github scaffold's real workflow content made it
# to this target - guarded the same way, with its own distinct message,
# rather than one skip reason silently covering two different causes.
.PHONY: actionlint
actionlint: native-warning tools-image
	@if [ ! -d .github/workflows ]; then \
	  echo "Skipping actionlint: no .github/workflows/ found (--forge none, or none added yet)"; \
	elif [ ! -d .git ]; then \
	  echo "Skipping actionlint: not a git repository yet (run 'git init' first - actionlint requires one to detect the project root, even with .github/workflows/ present)"; \
	else \
	  echo "Running actionlint"; \
	  $(IN_TOOLS) -c 'actionlint -color'; \
	fi

# zizmor - static analysis (security) for GitHub Actions. --offline keeps
# it deterministic (no GitHub API calls). Same missing-.github/workflows/
# guard as actionlint above, and for the same reason: --forge none ships no
# workflows for it to scan.
.PHONY: zizmor
zizmor: native-warning tools-image
	@if [ -d .github/workflows ]; then \
	  echo "Running zizmor"; \
	  $(IN_TOOLS) -c 'zizmor --offline .'; \
	else \
	  echo "Skipping zizmor: no .github/workflows/ found (--forge none, or none added yet)"; \
	fi

# gitleaks - secret scanner over the working tree. Kept out of `check`
# (it's a prevention tool, not a build gate); run it before committing.
.PHONY: secrets
secrets: native-warning tools-image
	@echo "Running gitleaks secret scan"
	@$(IN_TOOLS) -c 'gitleaks dir . --no-banner --redact'

# osv-scanner - dependency vulnerability scan against the OSV database.
# Complements govulncheck (call graph) with the OSV feed. Needs network, so
# it's a separate target rather than part of `check`.
.PHONY: osv
osv: native-warning tools-image
	@echo "Running osv-scanner"
	@$(IN_TOOLS) -c 'osv-scanner scan source --lockfile go.mod'

# deadcode - unreachable Go functions (reachability from main + tests).
# Fails if any dead code is found. Part of `check` so dead code cannot
# creep back in.
.PHONY: deadcode
deadcode: native-warning tools-image
	@echo "Running deadcode"
	@$(IN_TOOLS) -c 'out=$$(deadcode -test ./...); if [ -n "$$out" ]; then echo "$$out"; echo "dead code found"; exit 1; fi; echo "no dead code"'

# docs-check - docs/metrics.md must never document a metric or label the code
# cannot produce (see internal/collector/docs_check_test.go): fails on that
# direction, since a lying doc is worse than none. The reverse gap - a real
# metric missing from docs/metrics.md - is only a warning (the -v below
# surfaces it), per CONTRIBUTING.md's Definition of Done, step 4.
# -count=1 disables go test's result cache: this test's real input,
# docs/metrics.md, is a plain file the Go toolchain has no reason to track as
# a build dependency, so without -count=1 a second run could serve a stale
# cached PASS/FAIL after only that file changed - the exact failure mode a
# lie-detector for that file must never have.
.PHONY: docs-check
docs-check: native-warning tools-image
	@echo "Running docs-check (docs/metrics.md vs internal/collector/*.go)"
	@$(IN_TOOLS) -c 'go test -run TestDocsCheck -v -count=1 ./internal/collector/...'

# Full pre-commit / pre-release verification gate - mirrors what CI runs.
.PHONY: check
check: vet lint test vuln actionlint zizmor deadcode docs-check

# `lint` (above) and `report` (below) deliberately overlap: gofmt, go vet,
# ineffassign, and misspell are each covered by both. That's by design, not
# redundancy - they answer different questions. `lint` (part of `check`) is
# a pass/fail GATE: any finding fails the build, same as CI. `report` is a
# goreportcard-style GRADE: a percentage per check, tolerant of a few
# findings as long as the overall average stays >= B. A clean `check`
# doesn't guarantee an A on `report`, and a passing `report` grade can
# still hide the handful of issues `check` would fail on - keep both.
#
# Offline equivalent of the goreportcard.com checks. Runs gofmt -s, go vet,
# gocyclo, ineffassign, misspell, and a LICENSE check, then prints a
# per-check score and an overall grade. Exits non-zero below B.
.PHONY: report
report: native-warning tools-image
	@$(IN_TOOLS) -c '$(TOOLS_CTX)/goreport.sh'

# Reports the state of Go module dependencies in a tabular form: which
# direct deps are up to date, which indirect ones have an upgrade
# available, and whether each pending bump is patch / minor / major.
# Read-only; never runs `go get` automatically - that's left for
# `go get -u ./... && go mod tidy`.
.PHONY: report-deps
report-deps: native-warning tools-image
	@$(IN_TOOLS) -c '$(TOOLS_CTX)/deps-report.sh'

# --- Docker images (local debug) ---------------------------------------------
# For building/running a local image outside the release pipeline (release
# images are built by GoReleaser on tag push). Unlike the tooling targets
# above, these have no native fallback: building or running a container
# image inherently needs a container engine, so all four targets fail fast
# with a clear message if CONTAINER_ENGINE is "none", regardless of NATIVE.
#
# Dockerfile/Dockerfile.minimal are self-contained multi-stage builds: each
# COPYs this repo's Go source into its own build stage and compiles it there
# (see either file's own header comment) - unlike GoReleaser's release
# pipeline, which cross-compiles externally first and stages the result at
# $TARGETPLATFORM/tapelibrary_exporter for dockers_v2 to COPY (see
# .goreleaser.yaml). That means these four local-debug targets need nothing
# beyond the container engine itself: no host Go toolchain, no pre-build or
# staging step - `docker build .` and `make docker-build` do exactly the
# same work.

DOCKER_IMAGE       ?= $(EXPORTER_NAME)
DOCKER_TAG         ?= dev
DOCKER_REF         := $(DOCKER_IMAGE):$(DOCKER_TAG)
DOCKER_REF_MINIMAL := $(DOCKER_IMAGE):$(DOCKER_TAG)-minimal

# IMAGE is sbom-image's own override point (below), same ?= idiom as
# DOCKER_IMAGE/DOCKER_TAG above: defaults to the standard local image this
# section already builds, override on the command line to point sbom-image
# at anything else already present in the local engine/registry instead,
# e.g. the minimal variant or a pulled release tag:
#   make sbom-image IMAGE=$(DOCKER_REF_MINIMAL)
#   make sbom-image IMAGE=ghcr.io/<owner>/tapelibrary_exporter:vX.Y.Z
IMAGE ?= $(DOCKER_REF)

# Build args shared by every image build below. Both Dockerfiles declare
# matching ARGs and inject them into their own build stage's -ldflags with
# the exact same github.com/prometheus/common/version.* keys LDFLAGS (top of
# this file) uses - so `--version` reports identical values whether the
# binary was built by `make build` or by `make docker-build`/-minimal. A
# bare `docker build .` with no --build-arg still works (each ARG defaults
# to a generic value in the Dockerfile), it just reports less specific
# version info.
DOCKER_BUILD_ARGS = \
	--build-arg VERSION=$$(git describe --tags --dirty 2>/dev/null || echo dev) \
	--build-arg COMMIT=$$(git rev-parse HEAD) \
	--build-arg BRANCH=$$(git rev-parse --abbrev-ref HEAD) \
	--build-arg BUILD_USER="$$(git config user.email)" \
	--build-arg BUILD_DATE=$$(date -u +%Y-%m-%dT%H:%M:%SZ)

.PHONY: docker-build
docker-build:
	@[ "$(CONTAINER_ENGINE)" != none ] || { echo "no container engine (docker or podman) found" >&2; exit 1; }
	@echo "Building $(DOCKER_REF)"
	$(CONTAINER_ENGINE) build $(DOCKER_BUILD_ARGS) -f Dockerfile -t $(DOCKER_REF) .
	@echo "built $(DOCKER_REF)"

# Distroless/minimal counterpart to docker-build above - same source, same
# build args, just -f Dockerfile.minimal and a -minimal tag suffix so it
# never collides with the standard image.
.PHONY: docker-build-minimal
docker-build-minimal:
	@[ "$(CONTAINER_ENGINE)" != none ] || { echo "no container engine (docker or podman) found" >&2; exit 1; }
	@echo "Building $(DOCKER_REF_MINIMAL)"
	$(CONTAINER_ENGINE) build $(DOCKER_BUILD_ARGS) -f Dockerfile.minimal -t $(DOCKER_REF_MINIMAL) .
	@echo "built $(DOCKER_REF_MINIMAL)"

.PHONY: docker-run
docker-run:
	@[ "$(CONTAINER_ENGINE)" != none ] || { echo "no container engine (docker or podman) found" >&2; exit 1; }
	@echo "Starting compose stack: $(DOCKER_REF) (override DOCKER_IMAGE=/DOCKER_TAG= to change)"
	IMAGE=$(DOCKER_REF) $(CONTAINER_ENGINE) compose -f docker-compose.yml up -d
	@echo "Metrics at http://localhost:9170/metrics"

# Distroless/minimal counterpart to docker-run above - same compose
# hardening, just docker-compose.minimal.yml and the -minimal image tag.
.PHONY: docker-run-minimal
docker-run-minimal:
	@[ "$(CONTAINER_ENGINE)" != none ] || { echo "no container engine (docker or podman) found" >&2; exit 1; }
	@echo "Starting compose stack: $(DOCKER_REF_MINIMAL) (override DOCKER_IMAGE=/DOCKER_TAG= to change)"
	IMAGE=$(DOCKER_REF_MINIMAL) $(CONTAINER_ENGINE) compose -f docker-compose.minimal.yml up -d
	@echo "Metrics at http://localhost:9170/metrics"

# CycloneDX SBOM for the container image (Tranche A hardening): GoReleaser's
# own sboms: block (.goreleaser.yaml) can only catalog release ARCHIVES -
# `artifacts:` never accepts an image value, a documented upstream
# limitation, not a missing config knob - so the image's own canonical SBOM
# has to come from a standalone syft invocation instead. This is that
# invocation: run it after `make docker-build` (or against any other image
# IMAGE points at). Unlike the four docker-* targets above, this one is NOT
# gated on CONTAINER_ENGINE/IN_TOOLS - syft itself talks to the local
# engine/registry directly, so the only real requirement is syft on PATH
# (already a documented release-time dependency alongside cosign - see
# .goreleaser.yaml's own header comment).
#
# The image ALSO carries a second, different SBOM layer: dockers_v2's own
# `sbom: "true"` (.goreleaser.yaml) is a supplementary, buildx-native SPDX
# attestation embedded directly in the image manifest at release time, for
# registry-native tooling (`docker sbom`, `docker buildx imagetools
# inspect`) - it is NOT CycloneDX and it is NOT a substitute for the
# artifact this target produces; the two are independent and both ship.
.PHONY: sbom-image
sbom-image:
	@command -v syft >/dev/null 2>&1 || { echo "syft not found on PATH - install it (https://github.com/anchore/syft) to generate an image SBOM" >&2; exit 1; }
	@echo "Generating $(EXPORTER_NAME).image.cdx.json for $(IMAGE)"
	syft "$(IMAGE)" -o cyclonedx-json=$(EXPORTER_NAME).image.cdx.json
	@echo "wrote $(EXPORTER_NAME).image.cdx.json"

# Cleans up build artifacts.
.PHONY: clean
clean:
	@echo "Cleaning up"
	rm -fr bin/
	rm -f $(EXPORTER_NAME).image.cdx.json
