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
.PHONY: help build test unit contract lint licences dev doctor images up down status server-down release-check lint-scripts scan-secrets release-build compose-up compose-down compose-logs compose-test e2e e2e-browser e2e-up e2e-down docs-check

help: ## list targets
	@grep -E '^[a-z0-9-]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-10s %s\n", $$1, $$2}'

build: ## build bin/can, bin/can-static and bin/can-core
	$(GO) build -ldflags '$(LDFLAGS)' -o bin/can ./cmd/can
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '-s -w' -o bin/can-static ./cmd/can-static
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '-s -w' -o bin/can-core ./cmd/can-core

unit: ## unit tests: -race, shuffled, uncached (a failure prints -test.shuffle SEED)
	$(GO) test -race -count=1 -shuffle=on ./cmd/... ./core/... ./test/wiring/... ./test/e2e/harness/... ./test/acceptance/... ./dev/...

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

# Where `make images` writes the tarballs and where `make up` reads them from (IMAGES_FROM).
IMAGES_DIR ?= build/images
IMAGES_FROM ?=

images: ## build every in-house image and can-hooks locally (podman or docker) into build/images
	sh scripts/build-images.sh $(IMAGES_DIR)

up: ## make a live company: provision, k3s, can-core, site, forms, backup, DNS (IMAGES_FROM=build/images, SMTP_CREDENTIALS=file, ARGS=--plan to preview)
	@$(GO) run ./cmd/can up $(if $(IMAGES_FROM),--images-from $(IMAGES_FROM)) $(if $(SMTP_CREDENTIALS),--smtp-credentials-file $(SMTP_CREDENTIALS)) $(ARGS)

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

# End-to-end suite against the compose stack through the Caddy edge (epic E23, specs/032-e2e-docs).
# The stack is project `can-e2e` with the dev/compose/e2e.yml overlay (fake token introspection for people and
# equity). Override with E2E_COMPOSE / E2E_PORT / E2E_APPS_PORT. Needs docker compose v2 or podman-compose, curl-free, Go only.
E2E_COMPOSE ?= docker compose -p can-e2e -f docker-compose.yml -f dev/compose/e2e.yml

e2e-up: ## DEV ONLY: build and start the compose stack for the e2e suite (project can-e2e)
	$(E2E_COMPOSE) up -d --build

e2e: ## run the end-to-end suite against the running e2e stack (make e2e-up first); E2E_RESTART=0 skips the restart test
	E2E_COMPOSE="$(E2E_COMPOSE)" $(GO) test -tags e2e -count=1 -timeout 15m -v ./test/e2e/...

e2e-browser: ## run the headless-Chrome suite against the running e2e stack (make e2e-up first; E2E_CHROME=path)
	E2E_COMPOSE="$(E2E_COMPOSE)" $(GO) test -tags browser -count=1 -timeout 15m -v ./test/browser/

e2e-down: ## tear down the e2e stack and delete its volumes
	$(E2E_COMPOSE) down -v

docs-check: ## check that every relative link in the Markdown docs resolves (offline)
	sh scripts/check-doc-links.sh
