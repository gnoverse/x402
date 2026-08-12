# gnoverse/x402 — the gno payment mechanism for x402, its facilitator, and the
# harnesses that prove a payment happens.
#
# The repository is laid out by language, the way x402-foundation/x402 is: go/ and
# js/ are siblings and neither owns the root. Every Go invocation below therefore
# names its directory.
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

GODIR := go
JSDIR := js
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
# The binary lands in the repository's bin/, not the Go tree's, because it is a
# build artifact of the repository.
build: ## Build the facilitator into bin/
	mkdir -p $(BIN)
	cd $(GODIR) && $(GO) build -ldflags "$(LDFLAGS)" -o ../$(FACILITATOR) ./cmd/gnofacilitator

test: ## Run the library tests
	cd $(GODIR) && $(GO) test ./...

test-integration: ## Run the tests that need a network (build tag: integration)
	cd $(GODIR) && $(GO) test -tags=integration -p 1 ./...

# The buyer is half of what this asserts, so the package has to be built first.
test-e2e: $(JSDIR)/dist ## Pay a gno seller end to end against a real in-memory node
	cd $(GODIR)/e2e && $(GO) test -count=1 ./...

# The buyers in js/ are stock @x402/* clients. They are opt-in so a Go-only
# checkout is never blocked on npm. buy.mjs imports the mechanism by package name,
# the way a stranger would, so the package must be built and not merely installed.
js: $(JSDIR)/dist ## Install and build the JS mechanism

js-test: $(JSDIR)/dist ## Run the JS mechanism's tests
	cd $(JSDIR) && $(NPM) test

$(JSDIR)/node_modules: $(JSDIR)/package.json
	cd $(JSDIR) && $(NPM) install --no-fund --no-audit
	touch $@

$(JSDIR)/dist: $(JSDIR)/node_modules $(JSDIR)/tsconfig.json $(JSDIR)/tsdown.config.ts $(wildcard $(JSDIR)/src/*.ts)
	cd $(JSDIR) && $(NPM) run build
	touch $@

lint: ## Vet both Go modules and check formatting
	cd $(GODIR) && $(GO) vet ./...
	cd $(GODIR)/e2e && $(GO) vet ./...
	gofmt -l $(GODIR) | tee /dev/stderr | (! read -r first)

fmt: ## Format the Go sources
	gofmt -w $(GODIR)

# node_modules is a fetched dependency rather than an artifact, and refetching it
# is expensive, so it survives. Remove it by hand to start over.
clean: ## Remove build artifacts
	rm -rf $(BIN) $(JSDIR)/dist

install: ## Install the facilitator into GOPATH/bin
	cd $(GODIR) && $(GO) install ./cmd/gnofacilitator

help: ## List the targets
	@grep -hE '^[a-zA-Z0-9_.$$()-]+:.*## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*## "} {printf "  %-18s %s\n", $$1, $$2}'
