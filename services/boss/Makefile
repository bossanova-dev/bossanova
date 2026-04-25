.PHONY: all format test dev clean build

## all: Clean, format, build, and test
all: clean format build test

# Suppress clang deployment-version warnings from CGO dependencies
export MACOSX_DEPLOYMENT_TARGET ?= $(shell sw_vers -productVersion 2>/dev/null)

BIN := ../../bin/boss

LDFLAGS := -s -w \
	-X github.com/recurser/bossalib/buildinfo.Version=$(or $(VERSION),dev) \
	-X github.com/recurser/bossalib/buildinfo.Commit=$(shell git rev-parse --short HEAD 2>/dev/null || echo unknown) \
	-X github.com/recurser/bossalib/buildinfo.Date=$(shell date -u '+%Y-%m-%dT%H:%M:%SZ')

## format: Format Go source files and run golangci-lint (catches typecheck errors locally)
format:
	gofmt -w .
	golangci-lint run ./...

## test: Run tests under -race with coverage (300s timeout covers E2E TUI tests under race instrumentation)
test:
	go test -race -timeout 300s -coverprofile=coverage.out ./...

## dev: Run the boss CLI TUI in dev mode (generates protos first if needed)
dev:
	$(MAKE) -C ../.. generate
	go run -ldflags '$(LDFLAGS)' ./cmd

## build: Build the boss binary
build:
	go build -ldflags '$(LDFLAGS)' -o $(BIN) ./cmd

## clean: Remove build artifacts
clean:
	rm -f $(BIN)
