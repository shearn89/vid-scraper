# vid-scraper
#
# `make` on its own lists the targets. The lint and test targets run the same
# tools as the pre-commit hooks, so what passes here passes on commit.

BINARY  := vid-scraper
PKG     := github.com/shearn89/vid-scraper

# Prefer a tag, fall back to a short SHA, then to "dev" outside a repo. -dirty
# marks a build made from uncommitted changes, which is worth seeing in
# --version when chasing a bug in something you handed to someone else.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

# -s -w drop the symbol table and DWARF: a smaller binary, and nothing here
# needs a debugger attached to a release build.
LDFLAGS := -s -w -X '$(PKG)/internal/cli.Version=$(VERSION)'
# -trimpath keeps local filesystem paths out of the binary, so two people
# building the same commit get the same bytes.
GOFLAGS := -trimpath -ldflags "$(LDFLAGS)"

GOBIN := $(shell go env GOBIN)
ifeq ($(GOBIN),)
GOBIN := $(shell go env GOPATH)/bin
endif

# have,<tool> is empty when the tool is absent, letting a target explain what
# to install rather than dying with "command not found".
have = $(shell command -v $(1) 2>/dev/null)

.DEFAULT_GOAL := help
.PHONY: help build install uninstall run fmt fmt-check vet lint test test-race \
	cover tidy tools hooks clean check ci

help: ## List targets
	@echo "vid-scraper $(VERSION)"
	@echo
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "} {printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'

build: ## Build ./vid-scraper
	go build $(GOFLAGS) -o $(BINARY) .

install: ## Install vid-scraper into GOBIN
	go install $(GOFLAGS) .
	@echo "installed $(GOBIN)/$(BINARY)"
	@command -v $(BINARY) >/dev/null || echo "warning: $(GOBIN) is not on your PATH"

uninstall: ## Remove the installed binary
	rm -f "$(GOBIN)/$(BINARY)"

run: build ## Build, then run with ARGS="..."
	./$(BINARY) $(ARGS)

fmt: ## Format the tree in place
	gofmt -w .
	@if [ -n "$(call have,goimports)" ]; then goimports -w .; \
	else echo "goimports not installed; skipping (make tools)"; fi

fmt-check: ## Fail if anything is unformatted
	@out=$$(gofmt -l .); \
	if [ -n "$$out" ]; then echo "unformatted:"; echo "$$out"; exit 1; fi
	@echo "formatting ok"

vet: ## go vet
	go vet ./...

lint: vet ## go vet, staticcheck and golangci-lint
	@if [ -n "$(call have,staticcheck)" ]; then staticcheck ./...; \
	else echo "staticcheck not installed; skipping (make tools)"; fi
	@if [ -n "$(call have,golangci-lint)" ]; then golangci-lint run; \
	else echo "golangci-lint not installed; skipping (make tools)"; fi

test: ## Run tests
	go test ./...

test-race: ## Run tests under the race detector
	go test -race ./...

cover: ## Write and summarise a coverage profile
	go test -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -1
	@echo "html report: go tool cover -html=coverage.out"

tidy: ## Tidy and verify go.mod
	go mod tidy
	go mod verify

tools: ## Install the optional dev tools
	go install honnef.co/go/tools/cmd/staticcheck@latest
	go install golang.org/x/tools/cmd/goimports@latest
	go install golang.org/x/vuln/cmd/govulncheck@latest
	@echo "golangci-lint: see https://golangci-lint.run/welcome/install/"

hooks: ## Install the pre-commit and pre-push hooks
	pre-commit install
	pre-commit install --hook-type pre-push

clean: ## Remove build and coverage artefacts
	rm -f $(BINARY) coverage.out

check: fmt-check lint test-race ## Everything the hooks check, without building

ci: check build ## What CI should run
