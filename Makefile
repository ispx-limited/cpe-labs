SHELL := /bin/sh

MODULE  := github.com/ispx-limited/cpe-labs
BIN_DIR := bin
BIN     := $(BIN_DIR)/cpe-sim

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || echo unknown)

LDFLAGS := -X $(MODULE)/internal/version.Version=$(VERSION) \
           -X $(MODULE)/internal/version.Commit=$(COMMIT) \
           -X $(MODULE)/internal/version.Date=$(DATE)

.PHONY: all build test test-race golden lint fmt vet tidy clean

all: build

build:
	@mkdir -p $(BIN_DIR)
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/cpe-sim

test:
	go test ./...

test-race:
	go test -race ./...

# Only packages whose tests import the golden helper define -update, and go
# test fails a package given a flag it does not define.
GOLDEN_PKGS = $(shell go list -f '{{.ImportPath}} {{join .TestImports " "}} {{join .XTestImports " "}}' ./... | \
	awk '{for (i = 2; i <= NF; i++) if ($$i == "$(MODULE)/internal/testgolden") {print $$1; next}}')

golden:
	go test $(GOLDEN_PKGS) -update

lint:
	golangci-lint run

fmt:
	gofmt -s -w .

vet:
	go vet ./...

tidy:
	go mod tidy

clean:
	rm -rf $(BIN_DIR) coverage.out coverage.html
