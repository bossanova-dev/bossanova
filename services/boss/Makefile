.PHONY: all all-full format test dev clean build

## all: Clean, format, build, and test
all: clean format build test

## all-full: Clean, format, build, and the exhaustive coverage test suite (test-all)
all-full: clean format build test-all

# Suppress clang deployment-version warnings from CGO dependencies
export MACOSX_DEPLOYMENT_TARGET ?= $(shell sw_vers -productVersion 2>/dev/null)

BIN := ../../bin/boss
PRETTIER := ../../node_modules/.bin/prettier

LDFLAGS := -s -w \
	-X github.com/recurser/bossalib/buildinfo.Version=$(or $(VERSION),dev) \
	-X github.com/recurser/bossalib/buildinfo.Commit=$(shell git rev-parse --short HEAD 2>/dev/null || echo unknown) \
	-X github.com/recurser/bossalib/buildinfo.Date=$(shell date -u '+%Y-%m-%dT%H:%M:%SZ')

## format: Format Go and JS/TS source files (gofmt + prettier).
format:
	gofmt -w .
	@files=$$(find . -type f \( -name '*.js' -o -name '*.jsx' -o -name '*.ts' -o -name '*.tsx' \) -not -path './node_modules/*' -print); \
	if [ -n "$$files" ]; then \
		if [ ! -x "$(PRETTIER)" ]; then \
			echo "prettier not found at $(PRETTIER); run npm install from the repo root" >&2; \
			exit 1; \
		fi; \
		find . -type f \( -name '*.js' -o -name '*.jsx' -o -name '*.ts' -o -name '*.tsx' \) -not -path './node_modules/*' -exec "$(PRETTIER)" --write {} +; \
	fi

GO_TEST_MAKEFILE := $(lastword $(MAKEFILE_LIST))
include $(dir $(lastword $(MAKEFILE_LIST)))../../mk/go-test.mk

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
