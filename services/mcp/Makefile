.PHONY: all all-full format test dev clean build

## all: Clean, format, build, and test
all: clean format build test

## all-full: Clean, format, build, and the exhaustive coverage test suite (test-all)
all-full: clean format build test-all

# Suppress clang deployment-version warnings from CGO dependencies
export MACOSX_DEPLOYMENT_TARGET ?= $(shell sw_vers -productVersion 2>/dev/null)

BIN := ../../bin/mcp

LDFLAGS := -s -w \
	-X github.com/recurser/bossalib/buildinfo.Version=$(or $(VERSION),dev) \
	-X github.com/recurser/bossalib/buildinfo.Commit=$(shell git rev-parse --short HEAD 2>/dev/null || echo unknown) \
	-X github.com/recurser/bossalib/buildinfo.Date=$(shell date -u '+%Y-%m-%dT%H:%M:%SZ')

## format: Format Go source files (gofmt).
format:
	gofmt -w .

GO_TEST_MAKEFILE := $(lastword $(MAKEFILE_LIST))
include $(dir $(lastword $(MAKEFILE_LIST)))../../mk/go-test.mk

## dev: Run the MCP server in dev mode
dev:
	go run -ldflags '$(LDFLAGS)' ./cmd

## build: Build the mcp binary
build:
	go build -ldflags '$(LDFLAGS)' -o $(BIN) ./cmd

## clean: Remove build artifacts
clean:
	rm -f $(BIN)
