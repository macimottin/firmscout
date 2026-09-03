# syntax=docker/dockerfile:1.7
#
# Multi-stage build for FirmScout's Go binaries: apps/api, apps/worker, apps/cli.
#
# One Dockerfile builds all three so the build environment (Go version, module
# cache, build flags) can never drift between them -- only the ARG differs.
#
# Build examples (context must be the repository root):
#   docker build -f infrastructure/docker/go.Dockerfile --build-arg BINARY=api    -t firmscout/api:dev    .
#   docker build -f infrastructure/docker/go.Dockerfile --build-arg BINARY=worker -t firmscout/worker:dev .
#   docker build -f infrastructure/docker/go.Dockerfile --build-arg BINARY=cli    -t firmscout/cli:dev    .
#
# NOTE: apps/api, apps/worker, and apps/cli main.go files may not exist yet --
# they are being written concurrently with this Dockerfile. This build will
# fail with "no Go files" (or similar) until each `apps/<BINARY>` package has
# a `main` package in it. Nothing below assumes anything about their internal
# structure beyond "go build ./apps/<BINARY> produces a single binary."

ARG GO_VERSION=1.27

# ---------------------------------------------------------------------------
# Stage 1: builder
# ---------------------------------------------------------------------------
FROM golang:${GO_VERSION}-alpine AS builder

# Selects which binary this build produces. Must be one of: api, worker, cli.
ARG BINARY
# Embedded into the binary as main.version via -ldflags below. Set from CI to
# a build SHA or tag; defaults to "dev" for local builds.
ARG VERSION=dev

# ca-certificates: the binaries make outbound TLS requests to arbitrary
# vendor sites and AI providers, so a valid CA bundle must be baked in here
# and copied into the distroless final stage, which has no package manager of
# its own to install one later.
# git: some Go module resolution paths need it even with an already-populated
# module cache (e.g. pseudo-versions, replace directives against VCS refs).
RUN apk add --no-cache ca-certificates git

WORKDIR /src

# Copy only the module files first so `go mod download` is cached
# independently of source changes -- editing application code should never
# invalidate the downloaded-modules layer.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

# Now bring in the rest of the source tree.
COPY . .

RUN test -n "$BINARY" || (echo "error: --build-arg BINARY is required (api|worker|cli)" >&2 && exit 1)

# CGO_ENABLED=0 for a fully static binary (required for the distroless
# "static" base image below, which has no libc at all).
# -trimpath strips local filesystem paths from the compiled binary.
# -ldflags="-s -w" strips symbol tables/debug info to shrink the binary;
# -X main.version=$VERSION embeds the build version so `firmscout --version`
# (or an equivalent) can report it without a separate build-info mechanism.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build \
      -trimpath \
      -ldflags="-s -w -X main.version=${VERSION}" \
      -o /out/firmscout \
      ./apps/${BINARY}

# ---------------------------------------------------------------------------
# Stage 2: final -- distroless, non-root, nothing but the binary.
# ---------------------------------------------------------------------------
# Distroless "static" is chosen deliberately, not "base" or a debug variant:
# these processes (the collector fetcher especially) make outbound requests
# to arbitrary third-party vendor sites, so the attack surface of the running
# container should be as close to "just the binary" as achievable -- no
# shell, no package manager, no coreutils, nothing an attacker who achieves
# code execution inside the process could pivot to.
FROM gcr.io/distroless/static-debian12:nonroot

# The nonroot variant already runs as uid/gid 65532 ("nonroot"); no USER
# instruction is needed (and distroless has no /etc/passwd entry to switch
# to anyway beyond the ones baked into the image).

COPY --from=builder /out/firmscout /firmscout

# NOTE on HEALTHCHECK: gcr.io/distroless/static-debian12 has no shell (no
# /bin/sh) and no HTTP client (no curl, no wget). A Dockerfile `HEALTHCHECK`
# instruction using the default CMD-SHELL form cannot run in this image, and
# there is no coreutils to build an exec-form probe out of either. We do NOT
# add a HEALTHCHECK here. Instead, docker-compose.yml's `api` and `worker`
# services define Compose-level healthchecks. Because Compose healthchecks
# also exec *inside* the container, they rely on the binary itself exposing a
# small `healthcheck` subcommand (a local, dependency-free HTTP GET against
# its own /healthz, exiting 0 on success) -- see the comment on those
# services in docker-compose.yml for the exact assumption this makes about
# apps/api and apps/worker's CLI surface, which does not exist yet.

ENTRYPOINT ["/firmscout"]
