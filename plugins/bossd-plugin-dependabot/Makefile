.PHONY: all format test clean build

## all: Clean, format, build, and test
all: clean format build test

BIN := ../../bin/bossd-plugin-dependabot

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

## build: Build the plugin binary
build:
	go build -ldflags '$(LDFLAGS)' -o $(BIN) .

## clean: Remove build artifacts
clean:
	rm -f $(BIN)
