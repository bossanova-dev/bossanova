.PHONY: all format test clean build

## all: Clean, format, build, and test
all: clean format build test

BIN := ../../bin/bossd-plugin-autopilot

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

## build: Build the plugin binary
build:
	go build -ldflags '$(LDFLAGS)' -o $(BIN) .

## clean: Remove build artifacts
clean:
	rm -f $(BIN)
