.PHONY: all format test dev clean build

## all: Clean, format, build, and test
all: clean format build test

BIN := ../../bin/bossd

LDFLAGS := -s -w \
	-X github.com/recurser/bossalib/buildinfo.Version=$(or $(VERSION),dev) \
	-X github.com/recurser/bossalib/buildinfo.Commit=$(shell git rev-parse --short HEAD 2>/dev/null || echo unknown) \
	-X github.com/recurser/bossalib/buildinfo.Date=$(shell date -u '+%Y-%m-%dT%H:%M:%SZ')

## format: Format Go source files and run golangci-lint (catches typecheck errors locally)
format:
	gofmt -w .
	golangci-lint run ./...

GO_TEST_MAKEFILE := $(lastword $(MAKEFILE_LIST))
include $(dir $(lastword $(MAKEFILE_LIST)))../../mk/go-test.mk

## dev: Run the bossd daemon in dev mode (generates protos first if needed).
## Builds bossd AND the co-located `mcp` binary into ../../bin, then runs the
## built bossd from there rather than via `go run`. The daemon resolves the
## trusted MCP binary relative to its own executable (config.ResolveMcpBinary
## prefers `boss-mcp`, then local-dev `mcp`, checking next-to-exe before PATH
## for each name), so a fallback `mcp` must sit next to bin/bossd or every
## spawned agent session launches with no mcp__boss__* tools. `go run` puts the
## executable in a temp dir with no sibling `mcp`, which silently disables the
## boss MCP wiring for all sessions the dev daemon spawns.
dev:
	$(MAKE) -C ../.. generate
	$(MAKE) -C ../mcp build
	go build -ldflags '$(LDFLAGS)' -o $(BIN) ./cmd
	$(BIN)

## build: Build the bossd binary
build:
	go build -ldflags '$(LDFLAGS)' -o $(BIN) ./cmd

## clean: Remove build artifacts
clean:
	rm -f $(BIN)
