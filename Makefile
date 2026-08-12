# gnoverse/x402 — the gno payment mechanism for x402, its facilitator, and the
# harnesses that prove a payment happens.
#
# Sources are laid out by language, go/ and js/, but both manifests live at the
# root where tooling expects them: `go test ./...` and `npm test` work without
# changing directory, and setup-go, dependabot and pkg.go.dev all find them.
#
# The test layers are separate on purpose, cheapest first:
#   test       the library. No network, no chain, no npm.
#   js-test    the buyer mechanism, including the sign doc byte-equality check.
#   test-e2e   one real payment, through a real in-memory node and the JS buyer.
#
# go/e2e is its own Go module, so `test` cannot start a chain by accident and
# needs no build tag to stay out of the way.

# A failed `go build -o` or `npm run build` must not leave a partial artifact that
# a later run mistakes for finished work.
.DELETE_ON_ERROR:

SHELL := /bin/sh

GO ?= go
NPM ?= npm

E2E := go/e2e
BIN := bin
FACILITATOR := $(BIN)/gnofacilitator

# A released binary is stamped by goreleaser; a local one says where it came from.
# Falls back to "dev", which is what the source declares, so the two never
# disagree about what an unstamped build is called.
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X main.version=$(VERSION)

.PHONY: all build test test-integration test-e2e js js-test lint fmt clean install help

all: build ## Build everything (default)

# Phony rather than a file target with a hand-listed prerequisite set: Go's build
# cache already decides what to recompile, and a manual source list goes stale.
build: ## Build the facilitator into bin/
	mkdir -p $(BIN)
	$(GO) build -ldflags "$(LDFLAGS)" -o $(FACILITATOR) ./go/cmd/gnofacilitator

test: ## Run the library tests
	$(GO) test ./...

test-integration: ## Run the tests that need a network (build tag: integration)
	$(GO) test -tags=integration -p 1 ./...

# The buyer is half of what this asserts, so the package has to be built first.
test-e2e: dist ## Pay a gno seller end to end against a real in-memory node
	cd $(E2E) && $(GO) test -count=1 ./...

# The buyers in js/ are stock @x402/* clients. They are opt-in so a Go-only
# checkout is never blocked on npm. buy.mjs imports the mechanism by package name,
# the way a stranger would, so the package must be built and not merely installed.
js: dist ## Install and build the JS mechanism

js-test: dist ## Run the JS mechanism's tests
	$(NPM) test

node_modules: package.json
	$(NPM) install --no-fund --no-audit
	touch $@

# Both levels are listed because Make's $(wildcard) does not recurse, and the
# sources sit one directory deep: js/src/exact/ mirrors the scheme/role subpath
# the package publishes.
TS_SOURCES := $(wildcard js/src/*.ts) $(wildcard js/src/exact/*.ts)

dist: node_modules tsconfig.json tsdown.config.ts $(TS_SOURCES)
	$(NPM) run build
	touch $@

lint: ## Vet both Go modules and check formatting
	$(GO) vet ./...
	cd $(E2E) && $(GO) vet ./...
	gofmt -l go | tee /dev/stderr | (! read -r first)

fmt: ## Format the Go sources
	gofmt -w go

# node_modules is a fetched dependency rather than an artifact, and refetching it
# is expensive, so it survives. Remove it by hand to start over.
clean: ## Remove build artifacts
	rm -rf $(BIN) dist

install: ## Install the facilitator into GOPATH/bin
	$(GO) install ./go/cmd/gnofacilitator

help: ## List the targets
	@grep -hE '^[a-zA-Z0-9_.$$()-]+:.*## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*## "} {printf "  %-18s %s\n", $$1, $$2}'
