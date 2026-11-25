# Thin wrapper around the `can` CLI and the project checks. See contracts/cli.md.
GO ?= go
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -X github.com/dodwmd/companyinacan/core/cli.version=$(VERSION) \
           -X github.com/dodwmd/companyinacan/core/cli.commit=$(COMMIT) \
           -X github.com/dodwmd/companyinacan/core/cli.date=$(DATE)
GOLANGCI_LINT_VERSION := v2.15.0

.DEFAULT_GOAL := help
.PHONY: help build test unit contract lint licences dev doctor up down status server-down release-check lint-scripts scan-secrets release-build compose-up compose-down compose-logs compose-test

help: ## list targets
	@grep -E '^[a-z-]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-10s %s\n", $$1, $$2}'

build: ## build bin/can, bin/can-static and bin/can-core
	$(GO) build -ldflags '$(LDFLAGS)' -o bin/can ./cmd/can
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '-s -w' -o bin/can-static ./cmd/can-static
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '-s -w' -o bin/can-core ./cmd/can-core

unit: ## unit tests
	$(GO) test -race ./cmd/... ./core/... ./test/wiring/...

contract: ## schema and example contract tests
	$(GO) test ./test/contract/...

LINT := $(GO) run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

lint: ## golangci-lint (pinned); falls back to go vet only when the linter cannot be fetched
	@if $(LINT) version >/dev/null 2>&1; then $(LINT) run ./...; \
	 else echo "lint: golangci-lint unavailable (offline?), falling back to go vet"; $(GO) vet ./...; fi

licences: ## regenerate THIRD_PARTY_LICENSES
	$(GO) run ./cmd/can licences --modules-dir modules --write

test: lint unit contract ## full local check suite
	$(GO) run ./cmd/can licences --modules-dir modules --check

dev: ## create the local k3d cluster and apply the base manifests (epic E02)
	@$(GO) run ./cmd/can dev up

down: ## tear down: the local k3d cluster (provider local) or the server (ssh, hetzner; needs ARGS=--yes)
	@$(GO) run ./cmd/can down $(ARGS)

doctor: ## check the prerequisites for `make dev`
	@$(GO) run ./cmd/can dev doctor

# Extra flags for the lifecycle targets, e.g. `make up ARGS=--plan` or `make down ARGS=--yes`.
ARGS ?=

up: ## provision a server, bootstrap k3s, print URL and DNS records (ARGS=--plan to preview)
	@$(GO) run ./cmd/can up $(ARGS)

server-down: down ## alias of down, kept for scripts that used the E06 name

status: ## show the server state; exit 0 only when ready
	@$(GO) run ./cmd/can status $(ARGS)

release-check: ## fail if deploy Dockerfiles or module charts still have placeholder image digests
	sh scripts/check-image-pins.sh

lint-scripts: ## shellcheck the scripts and check the deploy Dockerfiles (epic E20)
	sh scripts/lint-shell.sh
	sh scripts/lint-dockerfiles.sh

scan-secrets: ## in-repo secret scan of the tracked files (allowlist: scripts/secret-allowlist.txt)
	sh scripts/scan-secrets.sh

release-build: ## local, unpublished build of the release archives into dist/ (VERSION=vX.Y.Z); never pushes anything
	sh scripts/build-release.sh $(VERSION) dist

# Whole stack in containers, no Kubernetes (docker compose v2 or podman-compose). See docs/local-compose.md.
COMPOSE ?= docker compose -p companyinacan

compose-up: ## DEV ONLY: build and start the whole stack with docker compose (no Kubernetes)
	$(COMPOSE) up -d --build

compose-down: ## stop the compose stack, keep its data (ARGS=-v also deletes the volumes)
	$(COMPOSE) down $(ARGS)

compose-logs: ## follow the compose stack logs
	$(COMPOSE) logs -f --tail=100

compose-test: ## run the smoke test against the running compose stack
	dev/compose/smoke.sh
