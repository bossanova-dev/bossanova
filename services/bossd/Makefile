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

## test: Run tests under -race with coverage
test:
	go test -race -timeout 300s -coverprofile=coverage.out ./...

## dev: Run the bossd daemon in dev mode (generates protos first if needed)
dev:
	$(MAKE) -C ../.. generate copy-skills
	go run -ldflags '$(LDFLAGS)' ./cmd

## build: Build the bossd binary
build:
	go build -ldflags '$(LDFLAGS)' -o $(BIN) ./cmd

## clean: Remove build artifacts
clean:
	rm -f $(BIN)
