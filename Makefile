# Relay — bounded reliability lab. See docs/DEMO.md for the five-minute demo.
#
# `go` is expected on PATH (Homebrew installs it at /opt/homebrew/bin). Override
# with `make GO=/path/to/go <target>` if it is not.

GO ?= go
GOLANGCI_VERSION ?= v2.12.2
GOBIN := $(shell $(GO) env GOPATH)/bin
PKG := ./...

# Binaries build with cgo disabled: modernc.org/sqlite is pure Go, so the result
# is a reproducible static binary. Tests keep cgo enabled (the -race detector
# requires it); modernc works either way.

.PHONY: build run tools vet test test-race cover fuzz boundary lint doc-lint tidy vendor db-reset clean help

help: ## List available targets
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-12s\033[0m %s\n",$$1,$$2}'

build: ## Build both binaries into bin/ (static, cgo-free)
	CGO_ENABLED=0 $(GO) build -o bin/relay ./cmd/relay
	CGO_ENABLED=0 $(GO) build -o bin/relay-tools ./cmd/relay-tools

run: ## Run the control-room web server
	$(GO) run ./cmd/relay

tools: ## Run the MCP tool server over stdio
	$(GO) run ./cmd/relay-tools

vet: ## go vet
	$(GO) vet $(PKG)

test: ## Unit + integration tests
	$(GO) test -count=1 $(PKG)

test-race: ## Tests with the race detector
	$(GO) test -race -count=1 $(PKG)

cover: ## Coverage summary
	$(GO) test -count=1 -coverprofile=coverage.out $(PKG)
	$(GO) tool cover -func=coverage.out | tail -1

fuzz: ## Fuzz smoke over the parsing/decoding targets (short)
	-$(GO) test -run=^$$ -fuzz=FuzzParseToolResponse -fuzztime=20s ./internal/mcp
	-$(GO) test -run=^$$ -fuzz=FuzzCorpusReader -fuzztime=20s ./internal/corpus
	-$(GO) test -run=^$$ -fuzz=FuzzStateDecode -fuzztime=20s ./internal/core

boundary: ## Enforce the hexagonal boundary (core/planner import no adapters)
	@bad=$$($(GO) list -deps ./internal/core/... ./internal/planner/... 2>/dev/null | \
	  grep -E 'database/sql|net/http|modelcontextprotocol/go-sdk|modernc\.org/sqlite|go\.opentelemetry\.io' || true); \
	if [ -n "$$bad" ]; then \
	  echo "BOUNDARY VIOLATION — core/planner must not import:"; echo "$$bad"; exit 1; \
	fi; \
	echo "boundary ok: core/planner depend only on the standard library"

$(GOBIN)/golangci-lint:
	$(GO) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_VERSION)

lint: $(GOBIN)/golangci-lint ## Run golangci-lint (installs the pinned version if missing)
	$(GOBIN)/golangci-lint run

doc-lint: ## Reject unsafe template types and anthropomorphic "AI reasoning" copy
	@if grep -RInE 'template\.(HTML|JS|URL|CSS|Srcset)\b' internal/web 2>/dev/null; then \
	  echo "doc-lint: raw template types are forbidden in the UI (all payloads must be escaped)"; exit 1; fi
	@if grep -RIniE 'AI reasoning|the ai (thinks|decides|reasons)' internal docs 2>/dev/null; then \
	  echo "doc-lint: the planner is scripted/deterministic — never label it as AI reasoning"; exit 1; fi
	@echo "doc-lint ok"

tidy: ## go mod tidy
	$(GO) mod tidy

vendor: ## Vendor dependencies for fully offline builds
	$(GO) mod vendor

db-reset: ## Remove local SQLite databases
	rm -f *.db *.db-wal *.db-shm

clean: ## Remove build/test artifacts
	rm -rf bin coverage.out coverage.html
