# Financial Management System (FMS) — developer & CI orchestration entrypoint.
#
# One discoverable surface for the operational lifecycle: local dev, migrations,
# tests, lint, Docker builds, k8s apply/delete, and backup/restore. The CI
# pipeline and developer onboarding both drive through these targets so commands
# stay consistent between local, CI, and production.
#
# Every target delegates to a dedicated executable or script (docker compose,
# kubectl, go, npm, scripts/*.sh) — this file only wires them and never
# re-implements their logic (single source of truth per concern, Blueprint §7).
# Production migrations are applied only by a controlled job/manual step; the
# migrate targets here target the running dev database brought up by `make dev`.

SHELL := /usr/bin/env bash

# ---------------------------------------------------------------------------
# Variables. All are ?=(environment-overridable). Every value is treated as
# untrusted operator input and validated/quoted before shell interpolation
# (security: CWE-78 command injection; charset guards applied per-target).
# ---------------------------------------------------------------------------

# Container registry prefix used for all built images.
IMAGE_REGISTRY ?= ghcr.io/yourorg/fms

# Image tag. Defaults to the current git commit SHA so every built image is
# traceable to a source commit for audit and rollback (CWE-494 image integrity);
# falls back to a wall-clock timestamp when running outside a git tree.
VERSION ?= $(shell git rev-parse --short HEAD 2>/dev/null || date -u +%Y%m%d%H%M%S)

# Kubernetes namespace the FMS stack is applied to/destroyed from.
NAMESPACE ?= fms

# Dev-only DB credentials default. These mirror the `${POSTGRES_PASSWORD:-fms}`
# default in docker-compose.yml; they are NOT production credentials and are
# never written into this file — prod secrets come from env/.env/k8s secrets.
POSTGRES_PASSWORD ?= fms

# Dev DB connection used by the migrate targets. Passed to the migration tool
# via an environment variable only (never spliced inline into a recipe line).
DATABASE_URL ?= postgres://fms:$(POSTGRES_PASSWORD)@127.0.0.1:5432/fms?sslmode=disable

# Migration adapter and path. Parameterized so the tool is swappable without
# touching the orchestration surface (Clean Architecture rec: MIGRATE_CMD
# indirection). golang-migrate is the default adapter for the Go backend.
MIGRATE_CMD   ?= golang-migrate
MIGRATE_PATH  ?= backend/migrations

# Restore point-in-time target. ISO 8601 UTC, consumed by scripts/restore.sh.
# Defaults to "now" (replay WAL from the last full base backup to the latest
# consistent point).
RESTORE_TARGET ?= $(shell date -u +'%Y-%m-%d %H:%M:%S UTC')

# When "yes", append --force to the restore invocation. Default "no" preserves
# scripts/restore.sh's interactive safety interlock (it requires a TTY
# confirmation even with --force). Destructive operations are never silent.
RESTORE_FORCE ?= no

# Required to be "yes" for the destructive k8s-delete target.
K8S_CONFIRM ?= no

# Ordered secondary k8s manifest groups applied last (hpa/pdb/networkpolicy).
# Empty by default; populate to slot future manifests into the ordering without
# editing the apply sequence.
K8S_HPA     ?=
K8S_PDB     ?=
K8S_NETPOL  ?=

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# Validate operator-supplied variables against strict charsets before they are
# interpolated into docker/kubectl commands (CWE-78 / CWE-1188 namespace hygiene).
.PHONY: validate-vars
validate-vars:
	@set -euo pipefail; \
	if ! printf '%s' "$(VERSION)" | grep -Eq '^[A-Za-z0-9._\-\/:]+$$'; then \
		printf 'Makefile: VERSION contains invalid characters: %s\n' "$(VERSION)" >&2; exit 1; \
	fi; \
	if ! printf '%s' "$(IMAGE_REGISTRY)" | grep -Eq '^[A-Za-z0-9._\-\/:]+$$'; then \
		printf 'Makefile: IMAGE_REGISTRY contains invalid characters: %s\n' "$(IMAGE_REGISTRY)" >&2; exit 1; \
	fi; \
	if [ "$${#NAMESPACE}" -gt 63 ] || ! printf '%s' "$(NAMESPACE)" | grep -Eq '^[a-z0-9][a-z0-9-]*$$'; then \
		printf 'Makefile: NAMESPACE invalid (must be [a-z0-9-], <= 63 chars): %s\n' "$(NAMESPACE)" >&2; exit 1; \
	fi

# Fail fast if DATABASE_URL is unset; the migration client otherwise hangs on an
# empty DSN.
.PHONY: require-db-url
require-db-url:
	@set -euo pipefail; \
	if [ -z "$${DATABASE_URL:-}" ]; then \
		printf "DATABASE_URL is required to run migrations\n" >&2; exit 1; \
	fi

# Seed-guard: fail if deploy/k8s/secrets.yaml embeds a plaintext secret value.
# Uses an allowlist-ish heuristic to reject obvious plaintext credential keys.
.PHONY: k8s-secret-gate
k8s-secret-gate:
	@set -euo pipefail; \
	if grep -nE '(^[[:space:]]*[A-Za-z0-9_]*(PASSWORD|PASSWD|SECRET|PRIVATE)[A-Za-z0-9_]*[[:space:]]*:[[:space:]]*[^"+{ ]|postgres://[^ :]+:[^ @]+@)' deploy/k8s/secrets.yaml 2>/dev/null; then \
		printf 'Makefile: deploy/k8s/secrets.yaml contains a plaintext secret; use SealedSecrets/ESO instead.\n' >&2; exit 1; \
	fi

# ---------------------------------------------------------------------------
# Local development (docker compose is the sole orchestration)
# ---------------------------------------------------------------------------

## help          : list all targets and their purpose
.PHONY: help
help:
	@set -euo pipefail; \
	printf '\nFinancial Management System — orchestration targets\n\n'; \
	grep -E '^## ' Makefile | sed 's/^## //' || true; \
	printf '\n  NOTE: production migrations are applied only by a controlled\n  job/manual step; make migrate/down/version target the dev DB.\n\n'

## dev           : build and start the full local stack (docker compose up -d --build)
.PHONY: dev
dev:
	set -euo pipefail; docker compose up -d --build

## docker-up     : alias of dev where compose is the sole orchestration
.PHONY: docker-up
docker-up:
	set -euo pipefail; docker compose up -d --build

## dev-down      : stop and remove the local stack (docker compose down)
.PHONY: dev-down
dev-down:
	set -euo pipefail; docker compose down

## dev-logs      : stream logs from all compose services
.PHONY: dev-logs
dev-logs:
	set -euo pipefail; docker compose logs -f

# ---------------------------------------------------------------------------
# Versioned migrations (dev database only; golang-migrate adapter)
# ---------------------------------------------------------------------------

# The migration client is run via an installed golang-migrate binary when
# present; otherwise it is built from the module already pinned in backend/go.mod
# (github.com/golang-migrate/migrate/v4 v4.18.1) via `go run`. DATABASE_URL is
# always passed through the environment, keeping DSN metacharacters out of the
# recipe command line (CWE-78).

## migrate       : apply all pending versioned migrations to the dev DB
.PHONY: migrate
migrate: require-db-url
	set -euo pipefail; \
	if command -v golang-migrate >/dev/null 2>&1; then \
		cd backend && DATABASE_URL="$(DATABASE_URL)" golang-migrate -path ../$(MIGRATE_PATH) -database "$$DATABASE_URL" up; \
	else \
		cd backend && DATABASE_URL="$(DATABASE_URL)" go run -mod=mod github.com/golang-migrate/migrate/v4/cmd/migrate -path ../$(MIGRATE_PATH) -database "$$DATABASE_URL" up; \
	fi

## migrate-down  : roll back exactly one migration (dev-only; never "down all")
.PHONY: migrate-down
migrate-down: require-db-url
	set -euo pipefail; \
	if command -v golang-migrate >/dev/null 2>&1; then \
		cd backend && DATABASE_URL="$(DATABASE_URL)" golang-migrate -path ../$(MIGRATE_PATH) -database "$$DATABASE_URL" down 1; \
	else \
		cd backend && DATABASE_URL="$(DATABASE_URL)" go run -mod=mod github.com/golang-migrate/migrate/v4/cmd/migrate -path ../$(MIGRATE_PATH) -database "$$DATABASE_URL" down 1; \
	fi

## migrate-version : print current schema migration version (drift check for CI)
.PHONY: migrate-version
migrate-version: require-db-url
	set -euo pipefail; \
	if command -v golang-migrate >/dev/null 2>&1; then \
		cd backend && DATABASE_URL="$(DATABASE_URL)" golang-migrate -path ../$(MIGRATE_PATH) -database "$$DATABASE_URL" version; \
	else \
		cd backend && DATABASE_URL="$(DATABASE_URL)" go run -mod=mod github.com/golang-migrate/migrate/v4/cmd/migrate -path ../$(MIGRATE_PATH) -database "$$DATABASE_URL" version; \
	fi

# ---------------------------------------------------------------------------
# Tests, lint, secret scan
# ---------------------------------------------------------------------------

## test          : run backend (go test -race) and frontend (vitest) suites
.PHONY: test
test:
	set -euo pipefail; \
	cd backend && go test -race ./...; \
	cd frontend && npm run test

## lint          : backend go vet + frontend eslint + repo secret-pattern scan
.PHONY: lint
lint:
	set -euo pipefail; \
	cd backend && go vet ./...; \
	cd frontend && npm run lint; \
	printf 'make: scanning tracked files for secret patterns\n'; \
	if git ls-files -z | xargs -0 grep -nE '(AKIA[0-9A-Z]{16}|BEGIN (RSA |EC |OPENSSH )?PRIVATE KEY|(PASSWORD|PASSWD)[[:space:]]*=|postgres://[^ :]+:[^ @]+@)' 2>/dev/null; then \
		printf 'make: potential secret found in tracked files (see lines above)\n' >&2; exit 1; \
	fi

# ---------------------------------------------------------------------------
# Docker builds (contexts match docker-compose.yml so make build == make dev)
# ---------------------------------------------------------------------------

## build         : build fms-api/fms-worker (backend) and fms-frontend images
.PHONY: build
build: validate-vars
	set -euo pipefail; \
	docker build --target api -t "$(IMAGE_REGISTRY)/fms-api:$(VERSION)" ./backend; \
	docker build --target worker -t "$(IMAGE_REGISTRY)/fms-worker:$(VERSION)" ./backend; \
	docker build -t "$(IMAGE_REGISTRY)/fms-frontend:$(VERSION)" ./frontend
	@printf 'Built fms-api/worker/frontend tagged $(VERSION)\n'

# ---------------------------------------------------------------------------
# Kubernetes lifecycle
# ---------------------------------------------------------------------------

# Ordered manifest groups for k8s-apply. Dependency order ensures the namespace,
# configmap and secrets exist before workloads, services reference selectors
# before they are applied, and ingress targets existing pods. hpa/pdb/networkpolicy
# groups are applied last and are empty unless populated via the variables above.
K8S_NS         := deploy/k8s/namespace.yaml
K8S_CONFIG     := deploy/k8s/configmap.yaml deploy/k8s/secrets.yaml
K8S_WORKLOADS  := deploy/k8s/postgres-statefulset.yaml deploy/k8s/redis-deployment.yaml \
                  deploy/k8s/rabbitmq-deployment.yaml deploy/k8s/api-deployment.yaml \
                  deploy/k8s/worker-deployment.yaml deploy/k8s/postgres-backup-cronjob.yaml
K8S_SERVICES   := deploy/k8s/postgres-service.yaml deploy/k8s/redis-service.yaml \
                  deploy/k8s/rabbitmq-service.yaml deploy/k8s/frontend-service.yaml
K8S_INGRESS    := deploy/k8s/api-ingress.yaml

## k8s-validate  : client-side dry-run only — fail fast on malformed manifests
.PHONY: k8s-validate
k8s-validate:
	set -euo pipefail; \
	kubectl apply --dry-run=client -f $(K8S_NS); \
	kubectl apply --dry-run=client -f $(K8S_CONFIG); \
	kubectl apply --dry-run=client -f $(K8S_WORKLOADS); \
	kubectl apply --dry-run=client -f $(K8S_SERVICES); \
	kubectl apply --dry-run=client -f $(K8S_INGRESS)

## k8s-apply     : apply all manifests declaratively in dependency order (idempotent)
.PHONY: k8s-apply
k8s-apply: validate-vars k8s-secret-gate
	set -euo pipefail; \
	kubectl apply -f $(K8S_NS); \
	kubectl apply -f $(K8S_CONFIG) -n "$(NAMESPACE)"; \
	kubectl apply -f $(K8S_WORKLOADS) -n "$(NAMESPACE)"; \
	kubectl apply -f $(K8S_SERVICES) -n "$(NAMESPACE)"; \
	kubectl apply -f $(K8S_INGRESS) -n "$(NAMESPACE)"; \
	if [ -n "$(K8S_HPA)" ]; then kubectl apply -f $(K8S_HPA) -n "$(NAMESPACE)"; fi; \
	if [ -n "$(K8S_PDB)" ]; then kubectl apply -f $(K8S_PDB) -n "$(NAMESPACE)"; fi; \
	if [ -n "$(K8S_NETPOL)" ]; then kubectl apply -f $(K8S_NETPOL) -n "$(NAMESPACE)"; fi

## k8s-delete    : DESTRUCTIVE — tears down the namespace; requires K8S_CONFIRM=yes
.PHONY: k8s-delete
k8s-delete: validate-vars
	@set -euo pipefail; \
	if [ "$(K8S_CONFIRM)" != "yes" ]; then \
		printf 'make: destructive — rerun with K8S_CONFIRM=yes to delete namespace $(NAMESPACE)\n' >&2; exit 1; \
	fi; \
	kubectl delete -f deploy/k8s/ -n "$(NAMESPACE)" --ignore-not-found

# ---------------------------------------------------------------------------
# Backup / restore (delegate to scripts; safety interlock inherited)
# ---------------------------------------------------------------------------

## backup        : run scripts/backup.sh (env/creds read from .env/k8s secrets)
.PHONY: backup
backup:
	set -euo pipefail; bash scripts/backup.sh

## restore       : run scripts/restore.sh --target-time; --force only if RESTORE_FORCE=yes
.PHONY: restore
restore:
	set -euo pipefail; \
	if [ "$(RESTORE_FORCE)" = "yes" ]; then \
		bash scripts/restore.sh --target-time "$(RESTORE_TARGET)" --force; \
	else \
		bash scripts/restore.sh --target-time "$(RESTORE_TARGET)"; \
	fi