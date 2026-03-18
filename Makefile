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

# Proto source files — stamp regenerates when these change
PROTO_SOURCES := $(wildcard proto/bossanova/v1/*.proto) buf.gen.yaml
GEN_STAMP := .generate.stamp

claude:
	claude --dangerously-skip-permissions

## generate: Run buf generate to produce Go code from proto definitions
generate: $(GEN_STAMP)

$(GEN_STAMP): $(PROTO_SOURCES)
	rm -rf lib/bossalib/gen
	buf generate
	@touch $(GEN_STAMP)

## build: Build all three binaries (generates protos first if needed)
build: $(BIN_DIR)/boss $(BIN_DIR)/bossd $(BIN_DIR)/bosso

$(BIN_DIR)/boss: $(GEN_STAMP)
	go build -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/boss ./services/boss/cmd

$(BIN_DIR)/bossd: $(GEN_STAMP)
	go build -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/bossd ./services/bossd/cmd

$(BIN_DIR)/bosso: $(GEN_STAMP)
	go build -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/bosso ./services/bosso/cmd

## test: Run tests across all modules (generates protos first if needed)
test: $(GEN_STAMP)
	@for mod in $(MODULES); do \
		echo "==> Testing $$mod"; \
		$(MAKE) -C $$mod test; \
	done

## Per-module test targets
test-bossalib: $(GEN_STAMP)
	$(MAKE) -C lib/bossalib test

test-boss: $(GEN_STAMP)
	$(MAKE) -C services/boss test

test-bossd: $(GEN_STAMP)
	$(MAKE) -C services/bossd test

test-bosso: $(GEN_STAMP)
	$(MAKE) -C services/bosso test

## lint: Run golangci-lint and buf lint (generates protos first if needed)
lint: $(GEN_STAMP)
	buf lint
	@for mod in $(MODULES); do \
		echo "==> Linting $$mod"; \
		(cd $$mod && golangci-lint run ./...); \
	done

## Per-module lint targets
lint-proto:
	buf lint

lint-bossalib: $(GEN_STAMP)
	cd lib/bossalib && golangci-lint run ./...

lint-boss: $(GEN_STAMP)
	cd services/boss && golangci-lint run ./...

lint-bossd: $(GEN_STAMP)
	cd services/bossd && golangci-lint run ./...

lint-bosso: $(GEN_STAMP)
	cd services/bosso && golangci-lint run ./...

## Per-module build targets
build-boss: $(BIN_DIR)/boss

build-bossd: $(BIN_DIR)/bossd

build-bosso: $(BIN_DIR)/bosso

## format: Format Go code, web code, and markdown
format:
	@for mod in $(MODULES); do \
		echo "==> Formatting $$mod"; \
		$(MAKE) -C $$mod format; \
	done
	$(MAKE) -C services/web format
	pnpm run format:docs

## build-all: Cross-platform builds for distribution (generates protos first if needed)
PLATFORMS := darwin/amd64 darwin/arm64 linux/amd64
# Only boss and bossd are distributed (bosso is deployed to Fly.io directly)
DIST_BINS := boss bossd

build-all: $(GEN_STAMP)
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
	rm -f $(GEN_STAMP)
	@for mod in $(MODULES); do \
		$(MAKE) -C $$mod clean; \
	done
	$(MAKE) -C services/web clean

## split: Mirror subtrees to separate repos via splitsh/lite
split:
	splitsh-lite --prefix=proto --target=refs/heads/split/proto
	splitsh-lite --prefix=lib/bossalib --target=refs/heads/split/bossalib
	splitsh-lite --prefix=services/boss --target=refs/heads/split/boss
	splitsh-lite --prefix=services/bossd --target=refs/heads/split/bossd
