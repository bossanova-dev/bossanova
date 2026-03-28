.PHONY: all generate build plugins test lint clean split format build-all \
	test-bossalib test-boss test-bossd test-bosso test-autopilot test-dependabot test-repair \
	lint-bossalib lint-boss lint-bossd lint-bosso lint-autopilot lint-dependabot lint-repair lint-proto \
	build-boss build-bossd build-bosso build-autopilot build-dependabot build-repair \
	copy-skills

## all: Clean, generate protos, format, and build all binaries (default target)
all: clean generate format build plugins build-all

# Binaries output to bin/
BIN_DIR := bin

# All Go modules in the workspace
MODULES := lib/bossalib services/boss services/bossd services/bosso \
	plugins/bossd-plugin-autopilot plugins/bossd-plugin-dependabot plugins/bossd-plugin-repair

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
WEB_DEPS_STAMP := services/web/node_modules/.package-lock.json

# Skill files destination (shared by boss and bossd via bossalib)
SKILLS_SRC := .claude/skills
SKILLS_DST := lib/bossalib/skilldata/skills

claude:
	claude --dangerously-skip-permissions

## web-deps: Install web dependencies (needed for protoc-gen-es plugin)
$(WEB_DEPS_STAMP): services/web/package.json
	cd services/web && pnpm install
	@touch $(WEB_DEPS_STAMP)

## generate: Run buf generate to produce Go code from proto definitions
generate: $(GEN_STAMP)

$(GEN_STAMP): $(PROTO_SOURCES) $(WEB_DEPS_STAMP)
	rm -rf lib/bossalib/gen
	buf generate
	@touch $(GEN_STAMP)

## build: Build service binaries (generates protos first if needed)
build: $(BIN_DIR)/boss $(BIN_DIR)/bossd $(BIN_DIR)/bosso

$(BIN_DIR)/boss: $(GEN_STAMP) copy-skills
	go build -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/boss ./services/boss/cmd

$(BIN_DIR)/bossd: $(GEN_STAMP) copy-skills
	go build -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/bossd ./services/bossd/cmd

$(BIN_DIR)/bosso: $(GEN_STAMP)
	go build -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/bosso ./services/bosso/cmd

$(BIN_DIR)/bossd-plugin-autopilot: $(GEN_STAMP)
	go build -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/bossd-plugin-autopilot ./plugins/bossd-plugin-autopilot

$(BIN_DIR)/bossd-plugin-dependabot: $(GEN_STAMP)
	go build -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/bossd-plugin-dependabot ./plugins/bossd-plugin-dependabot

$(BIN_DIR)/bossd-plugin-repair: $(GEN_STAMP)
	go build -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/bossd-plugin-repair ./plugins/bossd-plugin-repair

## plugins: Build all plugin binaries
plugins: $(BIN_DIR)/bossd-plugin-autopilot $(BIN_DIR)/bossd-plugin-dependabot $(BIN_DIR)/bossd-plugin-repair

## test: Run tests across all modules (generates protos first if needed)
test: $(GEN_STAMP) copy-skills
	@for mod in $(MODULES); do \
		echo "==> Testing $$mod"; \
		$(MAKE) -C $$mod test; \
	done

## Per-module test targets (no generate dep — CI uses committed gen code)
test-bossalib: copy-skills
	$(MAKE) -C lib/bossalib test

test-boss: copy-skills
	$(MAKE) -C services/boss test

test-bossd: copy-skills
	$(MAKE) -C services/bossd test

test-bosso:
	$(MAKE) -C services/bosso test

test-autopilot:
	$(MAKE) -C plugins/bossd-plugin-autopilot test

test-dependabot:
	$(MAKE) -C plugins/bossd-plugin-dependabot test

test-repair:
	$(MAKE) -C plugins/bossd-plugin-repair test

## lint: Run golangci-lint and buf lint (generates protos first if needed)
lint: $(GEN_STAMP) copy-skills
	buf lint
	@for mod in $(MODULES); do \
		echo "==> Linting $$mod"; \
		(cd $$mod && golangci-lint run ./...); \
	done

## Per-module lint targets
lint-proto:
	buf lint

lint-bossalib: copy-skills
	cd lib/bossalib && golangci-lint run ./...

lint-boss: copy-skills
	cd services/boss && golangci-lint run ./...

lint-bossd: copy-skills
	cd services/bossd && golangci-lint run ./...

lint-bosso:
	cd services/bosso && golangci-lint run ./...

lint-autopilot:
	cd plugins/bossd-plugin-autopilot && golangci-lint run ./...

lint-dependabot:
	cd plugins/bossd-plugin-dependabot && golangci-lint run ./...

lint-repair:
	cd plugins/bossd-plugin-repair && golangci-lint run ./...

## copy-skills: Copy boss skill files into bossalib for embedding
copy-skills:
	rm -rf $(SKILLS_DST)
	mkdir -p $(SKILLS_DST)
	for dir in $(SKILLS_SRC)/boss $(SKILLS_SRC)/boss-*; do \
		[ -d "$$dir" ] || continue; \
		name=$$(basename $$dir); \
		mkdir -p $(SKILLS_DST)/$$name; \
		cp -R $$dir/* $(SKILLS_DST)/$$name/; \
	done

## Per-module build targets (no generate dep — CI uses committed gen code)
build-boss: copy-skills
	go build -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/boss ./services/boss/cmd

build-bossd: copy-skills
	go build -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/bossd ./services/bossd/cmd

build-bosso:
	go build -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/bosso ./services/bosso/cmd

build-autopilot:
	go build -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/bossd-plugin-autopilot ./plugins/bossd-plugin-autopilot

build-dependabot:
	go build -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/bossd-plugin-dependabot ./plugins/bossd-plugin-dependabot

build-repair:
	go build -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/bossd-plugin-repair ./plugins/bossd-plugin-repair

## format: Format Go code, web code, package.json files, and markdown
format:
	pnpm syncpack format
	pnpm syncpack fix
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

build-all: $(GEN_STAMP) copy-skills
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
	rm -rf $(SKILLS_DST)
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
