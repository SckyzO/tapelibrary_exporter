# Image with every CI / report tool baked in.
#
# Used by `make check` and `make report` so contributors can run them
# without installing Go, golangci-lint, gocyclo, etc. on their host.
# Only a container engine (Docker or Podman) is required.
#
# This Dockerfile is the SINGLE source of truth for the Go version used to
# build and test this exporter: there is no separate GO_VERSION in the
# Makefile. Bump the base image tag below to change it.
#
# Reciprocal note: Dockerfile and Dockerfile.minimal (repo root) pin this
# exact same golang:*-alpine tag/digest in their own build stage: bump
# those two along with this one, or the three will silently drift apart.
#
# Built locally on first use via `make tools-image` (a prerequisite of every
# other tooling target); rebuilds are incremental thanks to the layer cache.

FROM golang:1.27.0-alpine@sha256:4c9fe60190a2a3350ddc51de80d0224b8a6698d12bdfc999fee45ea9d6c46dbc

# shellcheck  → used by actionlint to lint `run:` shell blocks
# zizmor      → static analysis (security) for GitHub Actions workflows
# promtool    → validates monitoring/prometheus/*.yml, used by `make rules-check`.
#               Prometheus rejects an ENTIRE rule file on one parse error, so a
#               single malformed expression silently kills every alert this
#               exporter ships. That happened once and went unnoticed for two
#               days, because nothing in `make check` looked at the rules.
# build-base  → C toolchain (gcc): `go test -race` requires cgo
RUN apk add --no-cache git bash curl ca-certificates findutils \
        shellcheck zizmor build-base

# This image always runs as root (no USER below) against a bind-mounted repo
# owned by whatever host UID ran `make` (see Makefile's IN_TOOLS: `-v
# $(CURDIR):/repo`) - a UID mismatch Git's own "dubious ownership" check
# (CVE-2022-24765 hardening) refuses by default with "fatal: detected
# dubious ownership in repository at '/repo'". That check exists to stop one
# LOCAL USER trusting a repo planted by ANOTHER local user/process; it does
# not apply here - this container only ever touches the single repo its
# caller explicitly bind-mounted, nothing it wasn't already handed. Trusting
# any directory system-wide is safe in that narrow context and needed for
# more than actionlint's own git-repository requirement (see actionlint's
# guard in Makefile.tmpl): Go's own `-buildvcs` auto-detection shells out to
# `git status --porcelain` on every `go build`/`go test` once main.go's
# module sits under ANY .git tree, so a bare `git init` (the very first thing
# a real new repo gets) without this would break plain `make build` too, not
# just actionlint - found empirically running this exact sequence.
RUN git config --system --add safe.directory '*'

# Static-analysis tools used by `make report` (the same set goreportcard.com
# runs server-side), by `make check` (lint + vet + test + vuln + actionlint),
# and by `make vuln` / `make actionlint`. Deliberately floating (@latest):
# what's pinned above is the Go toolchain itself (for reproducible builds);
# these auxiliary linters/scanners are kept current on every image rebuild so
# contributors always run the newest checks and vulnerability data.
RUN go install github.com/fzipp/gocyclo/cmd/gocyclo@latest \
 && go install github.com/client9/misspell/cmd/misspell@latest \
 && go install github.com/gordonklaus/ineffassign@latest \
 && go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest \
 && go install golang.org/x/vuln/cmd/govulncheck@latest \
 && go install github.com/rhysd/actionlint/cmd/actionlint@latest \
 && go install github.com/zricethezav/gitleaks/v8@latest \
 && go install github.com/google/osv-scanner/v2/cmd/osv-scanner@latest \
 && go install golang.org/x/tools/cmd/deadcode@latest \
 && go clean -cache -modcache -testcache

# promtool comes from the release tarball, not `go install`: the Prometheus
# go.mod carries replace directives, so `go install
# github.com/prometheus/prometheus/cmd/promtool@latest` is refused outright
# ("must not contain directives that would cause it to be interpreted
# differently than if it were the main module"). That is a long-standing
# property of that repository, not a transient breakage.
#
# Pinned, unlike the `go install` block above, and deliberately so: those pull
# source Go verifies against the checksum database, while this pulls a
# prebuilt binary. Version and digest are checked against the sha256sums file
# published beside it in the same release, so a truncated or swapped download
# fails the build instead of silently shipping.
ARG PROMTOOL_VERSION=3.7.3
RUN set -eu;     case "$(go env GOARCH)" in       amd64) PT_ARCH=amd64 ;;       arm64) PT_ARCH=arm64 ;;       *) echo "unsupported arch for promtool: $(go env GOARCH)" >&2; exit 1 ;;     esac;     PT="prometheus-${PROMTOOL_VERSION}.linux-${PT_ARCH}";     BASE="https://github.com/prometheus/prometheus/releases/download/v${PROMTOOL_VERSION}";     curl -fsSL -o /tmp/p.tgz "${BASE}/${PT}.tar.gz";     curl -fsSL -o /tmp/sums "${BASE}/sha256sums.txt";     (cd /tmp && grep " ${PT}.tar.gz$" sums | sed "s| .*| p.tgz|" | sha256sum -c -);     tar -xzf /tmp/p.tgz -C /tmp "${PT}/promtool";     install -m 0755 "/tmp/${PT}/promtool" /go/bin/promtool;     rm -rf /tmp/p.tgz /tmp/sums "/tmp/${PT}";     promtool --version

# All tools land in /go/bin which is already on $PATH.
WORKDIR /repo

# Default to a bash so callers can `docker run ... bash -c '...'`.
ENTRYPOINT ["/bin/bash"]
