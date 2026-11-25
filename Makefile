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
.PHONY: help build test unit contract lint licences dev up

help: ## list targets
	@grep -E '^[a-z-]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-10s %s\n", $$1, $$2}'

build: ## build bin/can
	$(GO) build -ldflags '$(LDFLAGS)' -o bin/can ./cmd/can

unit: ## unit tests
	$(GO) test ./cmd/... ./core/...

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

dev: ## local k3s cluster (not implemented yet, epic E02)
	@$(GO) run ./cmd/can dev

up: ## provision a server and deploy (not implemented yet, epic E06)
	@$(GO) run ./cmd/can up
