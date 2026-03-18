.PHONY: format test dev clean build

# Suppress clang deployment-version warnings from CGO dependencies
export MACOSX_DEPLOYMENT_TARGET ?= $(shell sw_vers -productVersion 2>/dev/null)

BIN := ../../bin/boss

LDFLAGS := -s -w \
	-X github.com/recurser/bossalib/buildinfo.Version=$(or $(VERSION),dev) \
	-X github.com/recurser/bossalib/buildinfo.Commit=$(shell git rev-parse --short HEAD 2>/dev/null || echo unknown) \
	-X github.com/recurser/bossalib/buildinfo.Date=$(shell date -u '+%Y-%m-%dT%H:%M:%SZ')

## format: Format Go source files
format:
	gofmt -w .

## test: Run tests
test:
	go test ./...

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
