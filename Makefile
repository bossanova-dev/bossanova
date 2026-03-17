.PHONY: all generate build test lint clean split format build-all \
	test-bossalib test-boss test-bossd test-bosso \
	lint-bossalib lint-boss lint-bossd lint-bosso lint-proto \
	build-boss build-bossd build-bosso

## all: Clean, generate protos, format, and build all binaries (default target)
all: clean generate format build build-all

# Binaries output to bin/
BIN_DIR := bin

# All Go modules in the workspace
MODULES := lib/bossalib services/boss services/bossd services/bosso

# Suppress clang deployment-version warnings from CGO dependencies
export MACOSX_DEPLOYMENT_TARGET ?= $(shell sw_vers -productVersion 2>/dev/null)

# Version info injected via ldflags
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
DATE    ?= $(shell date -u '+%Y-%m-%dT%H:%M:%SZ')
LDFLAGS := -s -w \
	-X github.com/recurser/bossalib/buildinfo.Version=$(VERSION) \
	-X github.com/recurser/bossalib/buildinfo.Commit=$(COMMIT) \
	-X github.com/recurser/bossalib/buildinfo.Date=$(DATE)

## generate: Run buf generate to produce Go code from proto definitions
generate:
	rm -rf lib/bossalib/gen
	buf generate

## build: Build all three binaries
build: $(BIN_DIR)/boss $(BIN_DIR)/bossd $(BIN_DIR)/bosso

$(BIN_DIR)/boss:
	go build -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/boss ./services/boss/cmd

$(BIN_DIR)/bossd:
	go build -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/bossd ./services/bossd/cmd

$(BIN_DIR)/bosso:
	go build -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/bosso ./services/bosso/cmd

## test: Run tests across all modules
test:
	@for mod in $(MODULES); do \
		echo "==> Testing $$mod"; \
		(cd $$mod && go test ./...); \
	done

## Per-module test targets
test-bossalib:
	cd lib/bossalib && go test ./...

test-boss:
	cd services/boss && go test ./...

test-bossd:
	cd services/bossd && go test ./...

test-bosso:
	cd services/bosso && go test ./...

## lint: Run golangci-lint and buf lint
lint:
	buf lint
	@for mod in $(MODULES); do \
		echo "==> Linting $$mod"; \
		(cd $$mod && golangci-lint run ./...); \
	done

## Per-module lint targets
lint-proto:
	buf lint

lint-bossalib:
	cd lib/bossalib && golangci-lint run ./...

lint-boss:
	cd services/boss && golangci-lint run ./...

lint-bossd:
	cd services/bossd && golangci-lint run ./...

lint-bosso:
	cd services/bosso && golangci-lint run ./...

## Per-module build targets
build-boss: $(BIN_DIR)/boss

build-bossd: $(BIN_DIR)/bossd

build-bosso: $(BIN_DIR)/bosso

## format: Format Go code and markdown
format:
	@for mod in $(MODULES); do \
		(cd $$mod && gofmt -w .); \
	done
	pnpm run format:docs

## build-all: Cross-platform builds for distribution
PLATFORMS := darwin/amd64 darwin/arm64 linux/amd64
# Only boss and bossd are distributed (bosso is deployed to Fly.io directly)
DIST_BINS := boss bossd

build-all:
	@for platform in $(PLATFORMS); do \
		os=$${platform%%/*}; \
		arch=$${platform##*/}; \
		for bin in $(DIST_BINS); do \
			echo "==> Building $$bin ($$os/$$arch)"; \
			GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 go build -ldflags '$(LDFLAGS)' \
				-o $(BIN_DIR)/$$bin-$$os-$$arch ./services/$$bin/cmd; \
		done; \
	done
	@echo "==> Building bosso (linux/amd64 only)"
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags '$(LDFLAGS)' \
		-o $(BIN_DIR)/bosso-linux-amd64 ./services/bosso/cmd

## clean: Remove build artifacts and generated code
clean:
	rm -rf $(BIN_DIR)
	rm -rf lib/bossalib/gen

## split: Mirror subtrees to separate repos via splitsh/lite
split:
	splitsh-lite --prefix=proto --target=refs/heads/split/proto
	splitsh-lite --prefix=lib/bossalib --target=refs/heads/split/bossalib
	splitsh-lite --prefix=services/boss --target=refs/heads/split/boss
	splitsh-lite --prefix=services/bossd --target=refs/heads/split/bossd
