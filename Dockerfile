# syntax=docker/dockerfile:1.7
#
# tapelibrary_exporter: Prometheus exporter.
#
# Multi-stage, self-contained build: stage 1 compiles a static binary
# straight from this repo's own source (no external pre-build step, no
# GoReleaser needed - a bare `docker build .` is enough), using the exact
# same pinned Go toolchain as scripts/docker/tools/Dockerfile (that file
# remains the single source of truth for the Go version - see its own
# header comment). Stage 2 runs the binary as a dedicated non-root user on
# a small, general-purpose base image that still has a shell and a package
# manager, for operators who want to `docker exec` in and poke around.
#
# Want the smallest, most locked-down image instead (no shell, no package
# manager, fixed nonroot UID)? See Dockerfile.minimal.
#
# CGO_ENABLED=0 produces a fully static binary with zero runtime library
# dependencies - nothing dynamically linked, not even libc - which is also
# what makes Dockerfile.minimal's distroless/static runtime stage possible.
#
# Build-time metadata (VERSION/COMMIT/BRANCH/BUILD_USER/BUILD_DATE) is
# injected with the exact same github.com/prometheus/common/version.* keys
# Makefile's own LDFLAGS uses for `make build` - so `--version` reports
# identical values regardless of which of the two built the binary. `make
# docker-build` supplies real values from git automatically; a bare `docker
# build .` with no --build-arg still works, it just reports the generic
# defaults below.

# ---- build stage -----------------------------------------------------------
FROM golang:1.26.5-alpine@sha256:0178a641fbb4858c5f1b48e34bdaabe0350a330a1b1149aabd498d0699ff5fb2 AS build

WORKDIR /src

# Cache module downloads in their own layer: invalidated only when
# go.mod/go.sum change, never by a plain source edit.
COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ cmd/
COPY internal/ internal/

ARG VERSION=dev
ARG COMMIT=
ARG BRANCH=
ARG BUILD_USER=
ARG BUILD_DATE=

RUN CGO_ENABLED=0 GOOS=linux go build \
      -ldflags "-s -w \
        -X github.com/prometheus/common/version.Version=${VERSION} \
        -X github.com/prometheus/common/version.Revision=${COMMIT} \
        -X github.com/prometheus/common/version.Branch=${BRANCH} \
        -X github.com/prometheus/common/version.BuildUser=${BUILD_USER} \
        -X github.com/prometheus/common/version.BuildDate=${BUILD_DATE}" \
      -o /out/tapelibrary_exporter \
      ./cmd/tapelibrary_exporter

# ---- runtime stage ----------------------------------------------------------
FROM debian:13-slim@sha256:020c0d20b9880058cbe785a9db107156c3c75c2ac944a6aa7ab59f2add76a7bd

RUN apt-get update && \
    DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        ca-certificates \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*

# CLI-flavor note: if this exporter's collectors shell out to an external CLI
# tool instead of calling an HTTP API (see internal/collector's use of
# Execute, if this scaffold picked the "cli" flavor), that tool is NOT
# bundled in this image - do not hardcode a specific vendor's client into
# this generic template. Either install it above with apt-get (rebuild
# afterwards) or bind-mount it read-only from the host via
# docker-compose.yml and make sure it ends up on PATH.

# Dedicated unprivileged user. --uid mirrors this exporter's own default
# port (a stable, collision-resistant choice per exporter, same idea as
# useradd --system just pinned to a specific value instead of whatever the
# next free system UID happens to be).
RUN useradd --system --no-create-home --shell /usr/sbin/nologin \
        --uid 9170 tapelibrary_exporter

COPY --from=build /out/tapelibrary_exporter /usr/local/bin/tapelibrary_exporter

USER tapelibrary_exporter

EXPOSE 9170

ARG VERSION=dev
ARG COMMIT=
ARG BUILD_DATE=

LABEL org.opencontainers.image.title="tapelibrary_exporter" \
      org.opencontainers.image.description="Prometheus exporter that collects metrics from https://<library-address>/web/api/v1" \
      org.opencontainers.image.licenses="gpl-3.0" \
      org.opencontainers.image.vendor="sckyzo" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}" \
      org.opencontainers.image.created="${BUILD_DATE}"

ENTRYPOINT ["/usr/local/bin/tapelibrary_exporter"]
CMD ["--web.listen-address=:9170"]
