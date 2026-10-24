.PHONY: build build-all build-docs build-drift-check build-mcp plugins plugins-all

## build-drift-check: Fail if committed BUILD.bazel files drift from gazelle, or if
## scripts/bazel/binary-inventory.json / ledger.json no longer match the live graph.
## A second leg runs scripts/test-infra/select-affected-external-inputs.test.mjs, which re-derives every
## cross-tree bazel `data` dep from the BUILD files, so a BUILD-only `data` edit is checked
## wherever this gate runs (the affected `make test` selects it for any gazelle input).
## Deliberately does NOT depend on $(GEN_STAMP), even though generated protobuf sources
## are also gazelle inputs: $(GEN_DEPS) pulls in $(WEB_DEPS_STAMP) -> `pnpm install`, and
## bazel.yml's go-test job has no pnpm (it never installs web deps), so that edge makes
## the gate fail with exit 127 in CI while staying green locally. Generated sources are
## committed, so gazelle always has valid inputs; test-all lists $(GEN_STAMP) itself, which
## is what keeps a generate-then-check ordering available to callers that need it.
## Requires the pnpm node_modules entries in .bazelignore — `bazel query //...` treats
## pnpm's self-referential node_modules symlink as a hard BUILD error.
build-drift-check:
ifeq ($(BAZEL_USABLE),1)
	node scripts/test-infra/run-gate.mjs --label "build-drift-check" -- ./scripts/bazel/check-build-drift.sh
	node scripts/test-infra/run-gate.mjs --label "build-drift-check: external inputs" -- node --test scripts/test-infra/select-affected-external-inputs.test.mjs
else
	@echo "==> bazel unavailable (BOSS_NO_BAZEL set or bazel not on PATH) — skipping BUILD drift check (public-mirror fallback)"
endif

## build: Build service binaries (generates protos first if needed)
build: $(addprefix $(BIN_DIR)/,$(SERVICE_BINS))

$(BIN_DIR)/boss: $(GEN_STAMP) $(BOSS_SKILL_DIRS) $(BOSS_SKILL_FILES) $(BOSS_HERMES_PLUGIN_DIRS) $(BOSS_HERMES_PLUGIN_FILES) $(PROJECTCONFIG_SCHEMA)
	go build -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/boss ./services/boss/cmd/boss
	@if [ "$$(uname)" = "Darwin" ]; then codesign -s "$(CODESIGN_IDENTITY)" --force $(BIN_DIR)/boss; fi

$(BIN_DIR)/bossd: $(GEN_STAMP) $(BOSSD_MIGRATION_DIRS) $(BOSSD_MIGRATION_FILES)
	go build -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/bossd ./services/bossd/cmd
	@if [ "$$(uname)" = "Darwin" ]; then codesign -s "$(CODESIGN_IDENTITY)" --force $(BIN_DIR)/bossd; fi

$(BIN_DIR)/mcp: $(GEN_STAMP)
	go build -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/mcp ./services/mcp/cmd
	@if [ "$$(uname)" = "Darwin" ]; then codesign -s "$(CODESIGN_IDENTITY)" --force $(BIN_DIR)/mcp; fi

ifneq ($(wildcard services/bosso/go.mod),)
$(BIN_DIR)/bosso: $(GEN_STAMP) $(BOSSO_MIGRATION_DIRS) $(BOSSO_MIGRATION_FILES)
	go build -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/bosso ./services/bosso/cmd
endif

# mcp-gateway is a private, hosted-only service (stripped from the public
# mirror); guard its build rule on the module's presence like bosso so the
# public Makefile does not reference a missing module.
ifneq ($(wildcard services/mcp-gateway/go.mod),)
$(BIN_DIR)/mcp-gateway: $(GEN_STAMP)
	go build -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/mcp-gateway ./services/mcp-gateway/cmd
endif

## plugins: Build all plugin binaries
# Regenerate plugins.sum from the freshly built binaries so release builds can
# verify them before exec (lib/bossalib/config, BOS-27). The
# generator excludes platform-suffixed cross-compiles, so this stays correct for
# the native `plugins` build; `plugins-all` does not touch the manifest.
plugins: $(addprefix $(BIN_DIR)/,$(PLUGIN_BINS))
	@node scripts/release/gen-plugin-sums.mjs $(BIN_DIR)

# Pattern rule for plugin binaries
$(BIN_DIR)/bossd-plugin-%: $(GEN_STAMP)
	go build -ldflags '$(LDFLAGS)' -o $@ ./plugins/bossd-plugin-$*

# bossd-plugin-claude embeds the same skillpayload tree as the boss CLI; list the
# payload exactly as $(BIN_DIR)/boss does so a skill edit relinks the plugin
# (BOS-676). Prerequisite-only (no recipe) so the pattern rule above builds it.
$(BIN_DIR)/bossd-plugin-claude: $(BOSS_SKILL_DIRS) $(BOSS_SKILL_FILES)

# bossd-plugin-opencode embeds its question hook; list it so an edit relinks the
# binary. Prerequisite-only (no recipe) so the pattern rule above still builds it.
$(BIN_DIR)/bossd-plugin-opencode: $(OPENCODE_HOOK_FILES)

build-docs:
	$(MAKE) -C services/docs build

## Per-module build targets (no generate dep — CI uses committed gen code)
build-boss:
	go build -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/boss ./services/boss/cmd/boss
	@if [ "$$(uname)" = "Darwin" ]; then codesign -s "$(CODESIGN_IDENTITY)" --force $(BIN_DIR)/boss; fi

build-bossd:
	go build -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/bossd ./services/bossd/cmd
	@if [ "$$(uname)" = "Darwin" ]; then codesign -s "$(CODESIGN_IDENTITY)" --force $(BIN_DIR)/bossd; fi

build-mcp:
	go build -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/mcp ./services/mcp/cmd
	@if [ "$$(uname)" = "Darwin" ]; then codesign -s "$(CODESIGN_IDENTITY)" --force $(BIN_DIR)/mcp; fi

ifneq ($(wildcard services/mcp-gateway/go.mod),)
build-mcp-gateway:
	go build -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/mcp-gateway ./services/mcp-gateway/cmd
endif

ifneq ($(wildcard services/bosso/go.mod),)
build-bosso:
	go build -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/bosso ./services/bosso/cmd
endif

# Auto-generate per-plugin build targets from detected modules
define define-plugin-build
build-$(2):
	go build -ldflags '$$(LDFLAGS)' -o $$(BIN_DIR)/$(notdir $(1)) ./$(1)
endef
$(foreach p,$(PLUGIN_MODULES),$(eval \
  $(call define-plugin-build,$(p),$(patsubst bossd-plugin-%,%,$(notdir $(p))))))

## build-all: Cross-platform builds for distribution (generates protos first if needed)
PLATFORMS := darwin/amd64 darwin/arm64 linux/amd64
# Only boss and bossd are distributed to users (bosso deploys as a GKE service)
DIST_BINS := boss bossd
# Plugins for distribution (auto-detected)
DIST_PLUGINS := $(PLUGIN_BINS)

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
	@if [ -d services/bosso ]; then \
		echo "==> Building bosso (linux/amd64 only)"; \
		GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags '$(LDFLAGS)' \
			-o $(BIN_DIR)/bosso-linux-amd64 ./services/bosso/cmd; \
	fi

## plugins-all: Cross-platform plugin builds for distribution
plugins-all: $(GEN_STAMP)
	@for platform in $(PLATFORMS); do \
		os=$${platform%%/*}; \
		arch=$${platform##*/}; \
		for plugin in $(DIST_PLUGINS); do \
			echo "==> Building $$plugin ($$os/$$arch)"; \
			GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 go build -ldflags '$(LDFLAGS)' \
				-o $(BIN_DIR)/$$plugin-$$os-$$arch ./plugins/$$plugin; \
		done; \
	done

