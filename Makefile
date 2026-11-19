# Strata developer Makefile.
#
# All targets expect the tools provided by the Nix development shell:
#   nix develop        # or: direnv allow (with `use flake` in .envrc)
#
# Run `make help` to list targets.

SHELL         := bash
.SHELLFLAGS   := -eu -o pipefail -c
.DEFAULT_GOAL := help

MODULE   := github.com/letthemplay/Strata
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.0.0-dev)
COMMIT   ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BIN_DIR  := bin
CMDS     := strata strata-api strata-agent strata-controller
LDFLAGS  := -s -w \
	-X $(MODULE)/internal/version.version=$(VERSION) \
	-X $(MODULE)/internal/version.commit=$(COMMIT)

export GOTOOLCHAIN := local
export CGO_ENABLED ?= 0

.PHONY: help
help: ## Show this help
	@awk 'BEGIN {FS = ":.*## "; printf "Usage: make <target>\n\nTargets:\n"} \
		/^[a-zA-Z0-9_-]+:.*## / {printf "  %-12s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

.PHONY: build
build: ## Build all binaries into ./bin
	@mkdir -p $(BIN_DIR)
	@for cmd in $(CMDS); do \
		echo "go build $$cmd"; \
		go build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/$$cmd ./cmd/$$cmd; \
	done

.PHONY: test
test: ## Run Go unit tests
	go test ./...

.PHONY: test-race
test-race: ## Run Go unit tests with the race detector (requires cgo)
	CGO_ENABLED=1 go test -race ./...

.PHONY: fmt
fmt: ## Format Go and Nix sources
	gofumpt -w cmd internal
	nix fmt

.PHONY: fmt-check
fmt-check: ## Fail if Go or Nix sources are not formatted
	@out=$$(gofumpt -l cmd internal); \
	if [ -n "$$out" ]; then echo "Unformatted Go files:"; echo "$$out"; exit 1; fi
	@git ls-files -z --cached --others --exclude-standard '*.nix' | xargs -0 nixfmt --check

.PHONY: lint
lint: ## Run golangci-lint, statix and deadnix
	golangci-lint run ./...
	statix check .
	deadnix --fail .

.PHONY: tidy
tidy: ## Tidy go.mod
	go mod tidy

.PHONY: nix-check
nix-check: ## Run `nix flake check` (builds, tests, format and lint checks)
	nix flake check

.PHONY: check
check: fmt-check lint test nix-check ## Run all local checks

.PHONY: clean
clean: ## Remove build artifacts (./bin and nix result links)
	rm -f -- result result-*
	rm -rf -- ./bin
