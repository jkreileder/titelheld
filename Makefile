MAKEFLAGS += --warn-undefined-variables
SHELL := bash
.SHELLFLAGS := -eu -o pipefail -c
.DEFAULT_GOAL := all
.DELETE_ON_ERROR:
.SUFFIXES:

GO := go
# Pinned so the Makefile, the pre-commit hook and CI all run the same linter.
GOLANGCI_LINT_VERSION := v2.12.2
GOVULNCHECK_VERSION := v1.7.0

.PHONY: all
all: lint test build

.PHONY: fmt
fmt:
	$(GO) run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) fmt

.PHONY: lint
lint:
	$(GO) vet ./...
	$(GO) run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) config verify
	$(GO) run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run

.PHONY: test
test:
	$(GO) test -race -coverprofile=coverage.out -covermode=atomic ./...
	$(GO) tool cover -func=coverage.out | tail -1

.PHONY: cover
cover: test
	$(GO) tool cover -html=coverage.out -o coverage.html

.PHONY: vulncheck
vulncheck:
	$(GO) run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...

.PHONY: check
check: lint test vulncheck

.PHONY: build
build:
	$(GO) build ./...

.PHONY: tidy
tidy:
	$(GO) mod tidy

.PHONY: clean
clean:
	git clean -xdf -e /HANDOFF.md -e /config.yaml -e /.claude/settings.local.json   # hard reset; keep the spec, local config and Claude settings
