MAKEFLAGS += --warn-undefined-variables
SHELL := bash
.SHELLFLAGS := -eu -o pipefail -c
.DEFAULT_GOAL := all
.DELETE_ON_ERROR:
.SUFFIXES:

GO := go
# Pinned so the Makefile, the pre-commit hook and CI all run the same linter.
GOLANGCI_LINT_VERSION := v2.13.1
GOVULNCHECK_VERSION := v1.7.0
GOTESTSUM_VERSION := v1.13.0

.PHONY: all
all: lint test build

.PHONY: fmt
fmt:
	$(GO) run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) fmt

.PHONY: lint
lint:
	$(GO) vet ./...
# The smoke-tagged tests are excluded from every other target, so nothing else
# compiles them: a signature change reaches them only when somebody runs them
# against a live provider, which is the worst moment to discover it.
	$(GO) vet -tags smoke ./...
	$(GO) run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) config verify
	$(GO) run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run

# The release workflow's own decision logic, run rather than linted: the writes
# gate against a stubbed gcloud, and the draft-release condition read out of
# release.yaml and evaluated over every combination of job results. CI runs the
# same two scripts.
.PHONY: release-logic
release-logic:
	bash .github/scripts/tests/writes-gate-test.sh
	python3 .github/scripts/tests/draft-condition-test.py

.PHONY: test
test:
	$(GO) test -race -coverprofile=coverage.out -covermode=atomic ./...
	$(GO) tool cover -func=coverage.out | tail -1

# Same test run, plus the JUnit report Codecov's test analytics consumes.
# Kept separate from `test` so the local loop needs no network.
.PHONY: test-junit
test-junit:
	$(GO) run gotest.tools/gotestsum@$(GOTESTSUM_VERSION) \
		--junitfile test-results.xml --format testname \
		-- -race -covermode=atomic -coverprofile=coverage.out ./...
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

# --- Infrastructure ---------------------------------------------------------
# The same three commands CI runs, so a green local run means a green CI run.

.PHONY: tf-fmt
tf-fmt:
	terraform -chdir=infra fmt -recursive

.PHONY: tf-validate
tf-validate:
	terraform -chdir=infra fmt -check -recursive
	terraform -chdir=infra init -backend=false -input=false
	terraform -chdir=infra validate

.PHONY: tf-lint
tf-lint:
	cd infra && tflint --init && tflint --format compact

.PHONY: tf-check
tf-check: tf-validate tf-lint

.PHONY: clean
clean:
	git clean -xdf -e /HANDOFF.md -e /config.yaml -e /.env -e /.claude/settings.local.json   # hard reset; keep the spec, local config, any .env and Claude settings
