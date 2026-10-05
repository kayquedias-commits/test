# syntax=docker/dockerfile:1
# Financial Management System backend. One build stage emits both process
# binaries; the runtime stage is kept distroless, static, and non-root so the
# images satisfy least-privilege and minimized-attack-surface constraints.
#
# Build context is the repository root. Only backend/ is copied into the build
# stage for a hermetic build; no config, secrets, or migrations are baked in —
# both processes read config.Config from the environment at runtime.

# ---------------------------------------------------------------------------
# cert — minimal CA bundle provider for TLS egress (Postgres, Redis, RabbitMQ,
# S3). Sourced from a slim base rather than the alpine toolchain to keep
# certificate provenance independent of the Go build image.
FROM debian:bookworm-slim AS cert
COPY /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt

# ---------------------------------------------------------------------------
# build — compiles both binaries from the shared module graph.
FROM golang:1.22-alpine AS build

# Optional Go toolchain passthrough; resolved at build time.
ARG GOTOOLCHAIN=auto

WORKDIR /src

# Copy module manifests first so dependency resolution layers cache cleanly.
# go mod download populates go.sum on first resolve, so this matches even when
# go.sum is not yet tracked in the repo.
COPY backend/go.mod backend/go.sum ./backend/

# Download the module graph with a buildkit cache, then verify checksums so a
# tampered/typosquatted dependency fails the build before compilation.
RUN --mount=type=cache,target=/go/pkg/mod go mod download && go mod verify

# Full source tree for the hermetic compile.
COPY backend/ ./backend/

ENV CGO_ENABLED=0 GOOS=linux GOFLAGS=-mod=readonly

WORKDIR /src/backend

# -trimpath hides build paths; -s -w strips debug info and symbols. CGO_ENABLED=0
# produces static binaries with no libc attack surface on the static runtime.
RUN go build -trimpath -ldflags="-s -w" -o /out/api ./cmd/api \
 && go build -trimpath -ldflags="-s -w" -o /out/worker ./cmd/worker

# ---------------------------------------------------------------------------
# runtime — distroless, static, non-root (UID 65532). No shell, no package
# manager. No VOLUME is declared: authoritative state lives outside the
# container (spooling /tmp is mounted as a k8s emptyDir at deploy time).
FROM gcr.io/distroless/static-debian12:nonroot AS runtime

WORKDIR /app

# CA trust material for TLS egress to datastores and object storage.
COPY --from=cert /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt

COPY --from=build /out/api /out/worker /app/

# Codify the non-root execution user explicitly.
USER 65532:65532

# Documentation-only: the API binds its HTTP address from config env.
EXPOSE 8080

# ---------------------------------------------------------------------------
# api — default target; serves the synchronous REST surface and /health.
FROM runtime AS api
CMD ["/app/api"]

# ---------------------------------------------------------------------------
# worker — event/batch consumer; no HTTP surface.
FROM runtime AS worker
CMD ["/app/worker"]