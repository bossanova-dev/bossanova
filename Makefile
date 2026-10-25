# (.github/workflows/*.yml). Bumping requires coordinated changes to both.
GOLANGCI_LINT_VERSION := v2.11.4

# Pinned buf version — single source of truth for both local (`make deps`,
# `buf-check-version`) and CI (the `version:` on every `bufbuild/buf-setup-action`
# and `bufbuild/buf-action` step in .github/workflows/*.yml). The
# scripts/dev/gen-pin-drift.test.mjs test asserts those workflow `version:` values
# equal this pin. Bumping requires re-running `make generate` and committing any
# regen drift. Recipes use the shared versioned cache, never a PATH buf.
BUF_VERSION := 1.66.1
BUF_CACHE_DIR ?= $(HOME)/.cache/bossanova-tools
BUF ?= $(BUF_CACHE_DIR)/buf-$(BUF_VERSION)/buf
# BUF must name a path: a bare command name is a PATH lookup, which the pin forbids.
$(if $(findstring /,$(BUF)),,$(error BUF=$(BUF) names no path; buf is never looked up on PATH. Unset BUF to use the pinned binary or set BUF=/absolute/path/to/buf))

# Technical-debt scanner tools used by `make debt-*` (the bs-sweep-debt skill).
# Debt targets are non-blocking informational detectors. dupl also backs the
# blocking intra-module clone ratchet in lint-affected, so its shared version is pinned.
DEADCODE_PKG    := golang.org/x/tools/cmd/deadcode@latest
DUPL_PKG        := github.com/mibk/dupl@v1.1.0
GOCYCLO_PKG     := github.com/fzipp/gocyclo/cmd/gocyclo@latest
GOVULNCHECK_PKG := golang.org/x/vuln/cmd/govulncheck@latest
# revive backs the FILE-size detector (`debt-filesize-*`). It reads the
# detector-only scripts/debt/revive-filesize.toml, NOT .golangci.yml — the
# blocking lint gate stays untouched (see that file's header for why).
REVIVE_PKG      := github.com/mgechev/revive@latest
# Thresholds for the noisier detectors (lower = more findings).
DEBT_DUPL_THRESHOLD  ?= 150
DEBT_CYCLO_THRESHOLD ?= 15
# Effective (comment/blank-skipping) file-length limit for `debt-filesize-*`.
# revive takes rule arguments from its config file only (no CLI flag), so the
# recipe seds this value over the committed default in revive-filesize.toml.
DEBT_FILESIZE_THRESHOLD ?= 800

# Binaries output to bin/
BIN_DIR := bin

# Bazel facade plumbing (BOS-339). `make test` and friends delegate to
# `bazel test //...` for content-addressed caching, then run the native ledger
# step for the bazel-`manual` targets today's loop still covers.
#
# The Makefile is mirrored to a public repo that has NO working Bazel workspace,
# so the facade keeps a bazel-absent fallback: when `bazel` is not on PATH OR
# `BOSS_NO_BAZEL` is set, the test targets fall back to the legacy native
# per-module `go test` loop. BAZEL_USABLE is computed once at parse time.
BAZEL ?= bazel
# $(BOSS_NO_BAZEL) (not $$BOSS_NO_BAZEL) so a command-line override
# `make BOSS_NO_BAZEL=1 ...` is honored — make imports env vars as make vars too,
# so this covers both the exported-env and the command-line-override cases.
BAZEL_USABLE := $(shell { [ -z "$(BOSS_NO_BAZEL)" ] && command -v $(BAZEL) >/dev/null 2>&1; } && echo 1)
# RACE=1 (CI / `make test-race`) maps to --config=race on every bazel invocation.
BAZEL_TEST_FLAGS := --test_output=errors
ifeq ($(RACE),1)
BAZEL_TEST_FLAGS += --config=race
endif
BAZEL_TEST_EXTRA_FLAGS ?=
ifeq ($(BOSS_GATE_FORCE_UNCACHED),1)
BAZEL_TEST_EXTRA_FLAGS += --nocache_test_results
GO_TEST_COUNT ?= 1
endif
BAZEL_TEST_FLAGS += $(BAZEL_TEST_EXTRA_FLAGS)

# Auto-detect Go modules (works in both private and public repos)
MODULES := $(patsubst %/go.mod,%,$(wildcard lib/*/go.mod services/*/go.mod plugins/*/go.mod))
SERVICE_MODULES := $(filter services/%,$(MODULES))
PLUGIN_MODULES  := $(filter plugins/%,$(MODULES))
SERVICE_BINS    := $(notdir $(SERVICE_MODULES))
PLUGIN_BINS     := $(notdir $(PLUGIN_MODULES))

## tidy: Run go mod tidy across every workspace module and leave go.mod/go.sum fixes in place.
MUTATE_DIR := .mutate

# README tour recordings and generated GIFs.
README_TOUR_CAST_DIR := services/docs/static/img/screenshots/tour
README_TOUR_GIF_DIR := services/marketing/public/screenshots/tour/gifs

# Suppress clang deployment-version warnings from CGO dependencies
export MACOSX_DEPLOYMENT_TARGET ?= $(shell sw_vers -productVersion 2>/dev/null)
export CGO_CFLAGS ?= -Wno-overriding-deployment-version

# Turborepo (web/marketing/ui-tokens TS caching). The cache is defaulted HERE,
# not in shell env, because daemon/cron worktrees don't inherit an interactive
# shell's exports — a machine-wide dir lets sibling worktrees share cache hits.
# TURBO_DAEMON=false: no per-worktree turbod on the agent farm.
export TURBO_CACHE_DIR ?= $(HOME)/.cache/bossanova-turbo
export TURBO_DAEMON ?= false
export TURBO_TELEMETRY_DISABLED ?= 1

# Codesign identity for local macOS builds. Default '-' is ad-hoc, which produces
# an unstable code identity so macOS keychain "Always Allow" entries don't
# survive rebuilds. Override with a stable self-signed identity (e.g.
# `export CODESIGN_IDENTITY=bossanova.dev`) to make Keychain ACLs persist.
# CI release builds sign with an Apple Developer ID in the release workflow,
# which supersedes this variable.
CODESIGN_IDENTITY ?= -

# Version info injected via ldflags
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
DATE    ?= $(shell date -u '+%Y-%m-%dT%H:%M:%SZ')
LDFLAGS := -s -w \
	-X github.com/recurser/bossalib/kit/buildinfo.Version=$(VERSION) \
	-X github.com/recurser/bossalib/kit/buildinfo.Commit=$(COMMIT) \
	-X github.com/recurser/bossalib/kit/buildinfo.Date=$(DATE)

# Proto source files — stamp regenerates when these change
PROTO_SOURCES := $(wildcard proto/bossanova/v1/*.proto) buf.gen.yaml lib/bossalib/go.mod lib/bossalib/go.sum \
	scripts/dev/protoc-gen-connect-openapi.sh tools/protoc-gen-connect-openapi/go.mod tools/protoc-gen-connect-openapi/go.sum
# Non-proto generator inputs: the hand-authored OpenAPI base document that
# protoc-gen-connect-openapi merges (buf.gen.yaml `base=`), and buf's module
# config, which every `buf generate` reads.
OPENAPI_SOURCES := services/docs/openapi/base.openapi.yaml buf.yaml
GEN_STAMP := .generate.stamp
WEB_DEPS_STAMP := node_modules/.modules.yaml

# The embedded boss-skill payload. lib/bossalib/contract/plugin/skillpayload is the one package
# both boss and bossd-plugin-claude embed, so the two binaries ship identical
# bytes without a mirror (BOS-1722). It must match skillinstall.SourceRelPath
# (lib/bossalib/contract/plugin/skillinstall/extract.go) and move with it.
SKILLS_SRC_DIR := lib/bossalib/contract/plugin/skillpayload/skills

# Non-Go payload files pulled into binaries by `go:embed`. The Go build cache
# keys on their contents, but only once `go build` is actually invoked — and
# make will not invoke it unless the payload files are prerequisites of the
# binary that embeds them. Without these lists, editing a skill or a migration
# leaves make with no newer prerequisite, it skips the recipe, and `make build`
# silently ships a stale payload (BOS-676). Each list is therefore wired onto
# the rule of the binary that embeds it.
#
# `find` runs at parse time like the VERSION/COMMIT shell-outs above; the
# `2>/dev/null` keeps a missing private-only directory (services/bosso is
# stripped from the public mirror) expanding to an empty list rather than
# printing a find error.
#
# The containing directories are listed alongside the files, because a file list
# only covers *edits*. Deleting a payload file drops it out of the list and every
# surviving prerequisite keeps its old mtime, so make would call the target up to
# date and ship an embed that still contains the deleted file — the same
# self-concealing loop, reached from the other direction. Unlinking bumps the
# directory's mtime, so naming the directories catches deletion (and addition).
#
# BOSS_SKILL_FILES: `go:embed all:skills` takes every file type, dotfiles
# included, so this is an unfiltered recursive walk — do not filter by suffix.
BOSS_SKILL_FILES := $(shell find $(SKILLS_SRC_DIR) -type f 2>/dev/null)
# The payload roots are named unconditionally, not discovered: a discovered list
# goes empty when the whole directory is deleted or renamed, which would leave the
# rule with no payload prerequisite at all and silently reinstate the stale-embed
# bug. Named, make instead fails loudly with "No rule to make target".
BOSS_SKILL_DIRS := $(SKILLS_SRC_DIR) $(shell find $(SKILLS_SRC_DIR) -type d 2>/dev/null)
# BOSS_HERMES_PLUGIN_*: services/boss/internal/hermes embeds `plugin/*.py` and
# `plugin/skills/*/SKILL.md` — never plugin/tests/ or __pycache__/ — so the walk
# is filtered to exactly those patterns. Its directories are named for the same
# deletion reason as the skill payload above.
HERMES_PLUGIN_DIR := services/boss/internal/hermes/plugin
BOSS_HERMES_PLUGIN_FILES := $(shell find $(HERMES_PLUGIN_DIR) -maxdepth 1 -type f -name '*.py' 2>/dev/null) \
	$(shell find $(HERMES_PLUGIN_DIR)/skills -type f -name SKILL.md 2>/dev/null)
BOSS_HERMES_PLUGIN_DIRS := $(HERMES_PLUGIN_DIR) $(HERMES_PLUGIN_DIR)/skills \
	$(shell find $(HERMES_PLUGIN_DIR)/skills -mindepth 1 -type d 2>/dev/null)
# PROJECTCONFIG_SCHEMA: lib/bossalib/kit/projectconfig embeds exactly this one file,
# so it is named rather than discovered; every binary that links the package
# lists it, or editing the schema would leave that binary's embed stale.
PROJECTCONFIG_SCHEMA := lib/bossalib/kit/projectconfig/schema/boss-project.schema.json
BOSSD_MIGRATION_FILES := $(shell find services/bossd/migrations -type f -name '*.sql' 2>/dev/null)
BOSSD_MIGRATION_DIRS := services/bossd/migrations
BOSSO_MIGRATION_FILES := $(shell find services/bosso/migrations_postgres -type f -name '*.sql' 2>/dev/null)
# Named, not wildcarded, for the same reason. Safe in the public mirror, where
# services/bosso is stripped: this list is only ever consumed inside the existing
# `ifneq ($(wildcard services/bosso/go.mod),)` guard.
BOSSO_MIGRATION_DIRS := services/bosso/migrations_postgres
# A single known file, so it is named rather than discovered. No directory entry
# is needed: `go:embed bossd-question.js` names one file, so deleting it fails the
# compile loudly instead of silently shipping a stale embed.
OPENCODE_HOOK_FILES := plugins/bossd-plugin-opencode/bossd-question.js


.PHONY: all all-full

## all: Fast affected check (default target) — lint + test only the affected/changed
## code. Fast is the default and exhaustive is opt-in (BOS-371): the old exhaustive
## clean+generate+format+build (~60s+, a full cross-platform rebuild that also
## reformats every doc) now lives under `make all-full`. As a convenience, if bin/
## has been removed (e.g. after `make clean`) this self-heals the binaries first via
## `make build plugins`, then runs the fast check. The guard keys off bin/boss, so a
## populated bin/ adds ~0ms — the fast loop is untouched. `plugins` is deliberately
## NOT an unconditional prereq: it regenerates plugins.sum on every run, which would
## tax every warm `make`. Rebuild explicitly with `make build` / `make plugins`.
all:
	@[ -x $(BIN_DIR)/boss ] || $(MAKE) build plugins
	$(MAKE) lint test

## all-full: Exhaustive clean, generate protos, format everything, and build all
## binaries — the previous default `make`. Slow; use in release prep, not the edit loop.
all-full:
	$(MAKE) clean
	$(MAKE) generate
	$(MAKE) format-all
	$(MAKE) build plugins build-all plugins-all

# Pinned golangci-lint version. Must match the version used in CI

include mk/setup.mk
include mk/generate.mk
include mk/skills.mk
include mk/build.mk
include mk/test.mk
include mk/postgres.mk
include mk/lint.mk
include mk/format.mk
include mk/debt.mk
include mk/architecture.mk
include mk/mutate.mk
include mk/release.mk
include mk/dev.mk
