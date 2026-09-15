# HOP build targets. Every target operates on HOP source only: the go tool's
# ./... pattern never enters nested modules, hidden directories, testdata or
# vendor trees, and the checkers in internal/ exclude repos/ and .worktrees/
# explicitly. `make check` is the non-mutating gate; only `make fmt` rewrites.

# Pinned tool versions. The Go toolchain pin is go.mod's go directive, which
# must carry a full patch version; every go invocation below runs on exactly
# that release, downloading it on first use.
GO_VERSION := $(shell sed -n 's/^go //p' go.mod)
export GOTOOLCHAIN := go$(GO_VERSION)

# golangci-lint bundles the gofumpt and goimports formatters it runs, so this
# one pin also fixes the formatter versions. The binary is installed into
# .bin/ by the release's official install script, which is downloaded from the
# tagged reference, verified against the pinned sha256 below and only then
# run; it verifies the binary's own checksum in turn. A version mismatch
# reinstalls. Both pins move together when the release is bumped.
GOLANGCI_LINT_VERSION := v2.13.2
GOLANGCI_LINT_INSTALLER_SHA256 := 1022ddb4d87ed252350ed03fc9677e250a4ae95cc6bcd4658c2a20a8a23d390f

BIN_DIR := $(CURDIR)/.bin
GOLANGCI_LINT := $(BIN_DIR)/golangci-lint
GOLANGCI_LINT_INSTALLER := https://raw.githubusercontent.com/golangci/golangci-lint/$(GOLANGCI_LINT_VERSION)/install.sh
SHA256SUM := $(shell command -v sha256sum >/dev/null 2>&1 && echo sha256sum || echo "shasum -a 256")

# VERSION, when set, is stamped into the binary as main.version. Unset, the
# binary reports the module version recorded by the Go toolchain.
VERSION ?=

.DEFAULT_GOAL := help

.PHONY: help
help: ## List targets
	@awk 'BEGIN {FS = ":.*##"} /^[a-zA-Z0-9_-]+:.*##/ { printf "  %-14s %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

.PHONY: build
build: ## Build the hop binary into .bin/hop
	go build $(if $(VERSION),-ldflags "-X main.version=$(VERSION)") -o $(BIN_DIR)/hop ./cmd/hop

.PHONY: fmt
fmt: golangci-lint ## Rewrite HOP Go formatting (gofumpt extra rules, goimports local prefix)
	$(GOLANGCI_LINT) fmt ./...

.PHONY: fmt-check
fmt-check: golangci-lint ## Report formatting differences without rewriting; fails when any exist
	$(GOLANGCI_LINT) fmt --diff ./...

.PHONY: arch
arch: ## Import/purity rules and checker fixture tests
	go test -count=1 -run '^TestArchitecture' ./internal

.PHONY: docs-check
docs-check: ## Guide coverage, immediate-child navigation and local links
	go test -count=1 -run '^TestGuides' ./internal

.PHONY: lint
lint: golangci-lint ## Pinned linter with schema-validated config, vet and module tidiness
	$(GOLANGCI_LINT) config verify
	$(GOLANGCI_LINT) run ./...
	go vet ./...
	go mod tidy -diff

.PHONY: test
test: ## Race and shuffle tests, including the architecture and guide checks
	go test -race -shuffle=on ./...

.PHONY: check
check: fmt-check docs-check lint test ## Non-mutating gate: fmt-check, docs-check, lint and test

.PHONY: test-live
test-live: ## Opt-in: real Claude Code against the fixture repo (costs tokens, never part of check)
	HOP_LIVE_HARNESS=1 go test -count=1 -v -run '^TestLiveClaudeDefaultProfileRun$$' -timeout 20m ./test/integration

.PHONY: golangci-lint
golangci-lint: ## Install the pinned golangci-lint into .bin when missing or at another version
	@if [ "$$($(GOLANGCI_LINT) version --short 2>/dev/null)" != "$(GOLANGCI_LINT_VERSION:v%=%)" ]; then \
		echo "installing golangci-lint $(GOLANGCI_LINT_VERSION) into $(BIN_DIR)" && \
		mkdir -p $(BIN_DIR) && \
		curl -sSfL $(GOLANGCI_LINT_INSTALLER) -o $(BIN_DIR)/install-golangci-lint.sh && \
		{ echo "$(GOLANGCI_LINT_INSTALLER_SHA256)  $(BIN_DIR)/install-golangci-lint.sh" | $(SHA256SUM) -c - || \
			{ rm -f $(BIN_DIR)/install-golangci-lint.sh; echo "golangci-lint install script does not match the pinned sha256" >&2; exit 1; }; } && \
		sh $(BIN_DIR)/install-golangci-lint.sh -b $(BIN_DIR) $(GOLANGCI_LINT_VERSION) && \
		rm -f $(BIN_DIR)/install-golangci-lint.sh; \
	fi
