.PHONY: all all-full format test clean build

## all: Clean, format, build, and test
all: clean format build test

## all-full: Clean, format, build, and the exhaustive coverage test suite (test-all)
all-full: clean format build test-all

PLUGIN_MAKEFILE := $(firstword $(MAKEFILE_LIST))
PLUGIN_DIR := $(dir $(PLUGIN_MAKEFILE))
PLUGIN_NAME := $(notdir $(abspath $(PLUGIN_DIR)))
REPO_DIR := $(abspath $(PLUGIN_DIR)/../..)
BIN := $(REPO_DIR)/bin/$(PLUGIN_NAME)

LDFLAGS := -s -w \
	-X github.com/recurser/bossalib/kit/buildinfo.Version=$(or $(VERSION),dev) \
	-X github.com/recurser/bossalib/kit/buildinfo.Commit=$(shell git rev-parse --short HEAD 2>/dev/null || echo unknown) \
	-X github.com/recurser/bossalib/kit/buildinfo.Date=$(shell date -u '+%Y-%m-%dT%H:%M:%SZ')

## format: Format Go source files (gofmt).
format:
	cd "$(PLUGIN_DIR)" && gofmt -w .

GO_TEST_MAKEFILE := $(PLUGIN_MAKEFILE)
include $(REPO_DIR)/mk/go-test.mk

## build: Build the plugin binary
build:
	cd "$(PLUGIN_DIR)" && go build -ldflags '$(LDFLAGS)' -o $(BIN) .

## clean: Remove build artifacts
clean:
	rm -f $(BIN)
