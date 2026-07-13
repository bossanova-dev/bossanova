.PHONY: all all-full build build-all build-docs clean codex-skills codex-skills-check copy-skills deps format format-affected format-all generate gen-skill kill kill-all lint lint-all lint-go-fmt vendor-toolbox vendor-toolbox-check \
	buf-check-version deps-golangci lint-check-version lint-docs lint-scripts \
	mutate mutate-coverage mutate-diff mutate-fix mutate-loop mutate-pkg \
	mutate-report mutate-survivors mutate-uncovered \
	debt-knip \
	plugins plugins-all proof proof-plan proof-test proof-tui-prebuild readme-gifs release release-codex-check \
	setup-worktree split stage-release test test-affected test-all test-full test-profile test-race test-smoke test-web \
	test-native-ledger test-bosso-scale test-docs test-integration-bossd test-manifest test-manifest-update \
	test-no-inline-stop-hooks test-public-mirror test-readme test-scripts \
	coverage-bossalib coverage-boss coverage-bossd coverage-bosso coverage-mcp coverage-mcp-gateway \
	build-mcp test-mcp lint-mcp \
	deploy-staging deploy-production db-staging db-production connect-staging connect-production verify-staging verify-production

## all: Fast affected check (default target) — lint + test only the affected/changed
## code. Fast is the default and exhaustive is opt-in (BOS-371): the old exhaustive
## clean+generate+format+build (~60s+, a full cross-platform rebuild that also
## reformats every doc) now lives under `make all-full`. Build binaries with
## `make build` / `make plugins`.
all: lint test

## all-full: Exhaustive clean, generate protos, format everything, and build all
## binaries — the previous default `make`. Slow; use in release prep, not the edit loop.
all-full:
	$(MAKE) clean
	$(MAKE) generate
	$(MAKE) format-all
	$(MAKE) build plugins build-all plugins-all

# Pinned golangci-lint version. Must match the version used in CI
# (.github/workflows/*.yml). Bumping requires coordinated changes to both.
GOLANGCI_LINT_VERSION := v2.11.4

# Pinned buf version — single source of truth for both local (`make deps`,
# `buf-check-version`) and CI (the `version:` on every `bufbuild/buf-setup-action`
# and `bufbuild/buf-action` step in .github/workflows/*.yml). The
# scripts/gen-pin-drift.test.mjs test asserts those workflow `version:` values
# equal this pin. Bumping requires re-running `make generate` and committing any
# regen drift.
BUF_VERSION := 1.66.1

# Technical-debt scanner tools used by `make debt-*` (the bs-sweep-debt skill).
# These are non-blocking, informational detectors (CI runs them continue-on-error,
# matching the existing govulncheck step), so they track `latest` like govulncheck.
# Pin to an explicit version here if you want hermetic local runs.
DEADCODE_PKG    := golang.org/x/tools/cmd/deadcode@latest
DUPL_PKG        := github.com/mibk/dupl@latest
GOCYCLO_PKG     := github.com/fzipp/gocyclo/cmd/gocyclo@latest
GOVULNCHECK_PKG := golang.org/x/vuln/cmd/govulncheck@latest
# Thresholds for the noisier detectors (lower = more findings).
DEBT_DUPL_THRESHOLD  ?= 150
DEBT_CYCLO_THRESHOLD ?= 15

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

# Auto-detect Go modules (works in both private and public repos)
MODULES := $(patsubst %/go.mod,%,$(wildcard lib/*/go.mod services/*/go.mod plugins/*/go.mod))
SERVICE_MODULES := $(filter services/%,$(MODULES))
PLUGIN_MODULES  := $(filter plugins/%,$(MODULES))
SERVICE_BINS    := $(notdir $(SERVICE_MODULES))
PLUGIN_BINS     := $(notdir $(PLUGIN_MODULES))

# Mutation testing output directory
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
	-X github.com/recurser/bossalib/buildinfo.Version=$(VERSION) \
	-X github.com/recurser/bossalib/buildinfo.Commit=$(COMMIT) \
	-X github.com/recurser/bossalib/buildinfo.Date=$(DATE)

# Proto source files — stamp regenerates when these change
PROTO_SOURCES := $(wildcard proto/bossanova/v1/*.proto) buf.gen.yaml lib/bossalib/go.mod lib/bossalib/go.sum
GEN_STAMP := .generate.stamp
WEB_DEPS_STAMP := node_modules/.modules.yaml

# Canonical and mirrored copies of the embedded boss-skill payload.
# services/boss/.../skills is the source of truth; the plugin copy is a mirror
# refreshed by `copy-skills` so both binaries embed identical bytes (otherwise
# they ping-pong overwriting ~/.claude/skills on every restart).
SKILLS_SRC_DIR := services/boss/internal/skillinstall/skills
SKILLS_PLUGIN_DIR := plugins/bossd-plugin-claude/skilldata/skills


claude:
	claude --dangerously-skip-permissions

codex:
	codex --dangerously-bypass-approvals-and-sandbox

codex-skills:
	node scripts/sync-codex-skills.mjs --root .

codex-skills-check:
	node scripts/sync-codex-skills.mjs --root . --check

## vendor-toolbox: Copy the canonical skills-toolbox/ helpers into each skill's toolbox/.
vendor-toolbox:
	node scripts/vendor-toolbox.mjs

## vendor-toolbox-check: Fail if any vendored skill toolbox has drifted.
vendor-toolbox-check:
	node scripts/vendor-toolbox.mjs --check

deploy-staging:
	$(MAKE) -C infra/kustomize deploy-staging

deploy-production:
	$(MAKE) -C infra/kustomize deploy-production

db-staging:
	$(MAKE) -C infra/kustomize db-staging

db-production:
	$(MAKE) -C infra/kustomize db-production

connect-staging:
	$(MAKE) -C infra/kustomize connect-staging

connect-production:
	$(MAKE) -C infra/kustomize connect-production

verify-staging:
	$(MAKE) -C infra/kustomize verify-staging

verify-production:
	$(MAKE) -C infra/kustomize verify-production

## deps: Install required build/dev tools via Homebrew (macOS)
deps:
	@if ! command -v brew >/dev/null 2>&1; then \
		echo "Homebrew is required. Install from https://brew.sh/"; \
		exit 1; \
	fi
	@echo "==> Installing build dependencies via Homebrew"
	@# buf is intentionally NOT installed via brew here — brew can't pin to an
	@# exact version, and the pin ($(BUF_VERSION)) is enforced by
	@# buf-check-version. buf is installed from a GitHub release below instead.
	@# bazelisk provides the `bazel` launcher the test facade delegates to (BOS-339);
	@# `command -v bazelisk` is the right presence check (brew installs it as bazelisk).
	@for pkg in go jq gh pnpm bazelisk; do \
		if command -v $$pkg >/dev/null 2>&1; then \
			echo "    $$pkg: already installed"; \
		else \
			echo "    $$pkg: installing..."; \
			brew install $$pkg; \
		fi; \
	done
	@# buf: install the pinned version from GitHub releases (brew can't pin).
	@# Best-effort: a download failure warns but does not abort `make deps`.
	@gobin=$$(go env GOBIN); [ -z "$$gobin" ] && gobin=$$(go env GOPATH)/bin; \
	want="$(BUF_VERSION)"; \
	if command -v buf >/dev/null 2>&1 && buf --version 2>/dev/null | grep -Eq "^v?$${want#v}$$"; then \
		echo "    buf: $$want already installed"; \
	else \
		echo "    buf: installing $$want from GitHub releases..."; \
		mkdir -p "$$gobin"; \
		url="https://github.com/bufbuild/buf/releases/download/v$${want#v}/buf-$$(uname -s)-$$(uname -m)"; \
		if curl -sSfL "$$url" -o "$$gobin/buf" && chmod +x "$$gobin/buf"; then \
			echo "    buf: installed $$want to $$gobin/buf"; \
		else \
			echo "    (warning: failed to download buf $$want from $$url — install it manually)"; \
		fi; \
	fi
	@# golangci-lint: enforce pinned version so local runs match CI.
	@gobin=$$(go env GOBIN); [ -z "$$gobin" ] && gobin=$$(go env GOPATH)/bin; \
	want="$(GOLANGCI_LINT_VERSION)"; \
	if command -v golangci-lint >/dev/null 2>&1 && \
	   golangci-lint --version 2>/dev/null | grep -Eq "version (v)?$${want#v}( |$$)"; then \
		echo "    golangci-lint: $$want already installed"; \
	else \
		echo "    golangci-lint: installing $$want..."; \
		curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/$$want/install.sh \
			| sh -s -- -b "$$gobin" $$want; \
	fi
	@if command -v gremlins >/dev/null 2>&1; then \
		echo "    gremlins: already installed"; \
	else \
		echo "    gremlins: installing..."; \
		brew install go-gremlins/tap/gremlins; \
	fi
	@echo "==> Installing Go-based buf plugins"
	@if command -v protoc-gen-connect-openapi >/dev/null 2>&1; then \
		echo "    protoc-gen-connect-openapi: already installed"; \
	else \
		echo "    protoc-gen-connect-openapi: installing..."; \
		go install github.com/sudorandom/protoc-gen-connect-openapi@v0.25.7; \
	fi
	@echo "==> Warming technical-debt scanners (make debt-*)"
	@for pkg in "$(DEADCODE_PKG)" "$(DUPL_PKG)" "$(GOCYCLO_PKG)" "$(GOVULNCHECK_PKG)"; do \
		echo "    $$pkg"; \
		go install "$$pkg" || echo "    (warning: failed to install $$pkg; make debt-* will fetch it on demand)"; \
	done
	@echo "==> Done. Run 'make build' to build binaries ('make' is now a fast affected lint+test check; 'make all-full' is the full rebuild)."

## setup-worktree: Copy gitignored local files (.env, .node-version, .bazelrc.user, .config, .compound-engineering/config.local.yaml) from the main repo into a new worktree (for bossanova setup-script)
setup-worktree:
	@if [ -z "$$REPO_DIR" ] || [ -z "$$WORKTREE_DIR" ]; then \
		echo "setup-worktree must be invoked by bossanova (REPO_DIR and WORKTREE_DIR required)"; \
		exit 1; \
	fi
	@if [ -f "$$REPO_DIR/.env" ]; then \
		cp "$$REPO_DIR/.env" "$$WORKTREE_DIR/.env"; \
		echo "Copied .env into $$WORKTREE_DIR"; \
	else \
		echo "No .env in $$REPO_DIR — skipping"; \
	fi
	@# Propagate the gitignored per-worktree BuildBuddy remote-cache config
	@# (.bazelrc.user is %workspace%-relative, so each worktree needs its own).
	@# Copying between local dirs keeps the API key out of git (BOS-345).
	@if [ -f "$$REPO_DIR/.bazelrc.user" ]; then \
		cp "$$REPO_DIR/.bazelrc.user" "$$WORKTREE_DIR/.bazelrc.user"; \
		echo "Copied .bazelrc.user into $$WORKTREE_DIR"; \
	else \
		echo "No .bazelrc.user in $$REPO_DIR — skipping (disk-cache only)"; \
	fi
	@if [ -f "$$REPO_DIR/.node-version" ]; then \
		cp "$$REPO_DIR/.node-version" "$$WORKTREE_DIR/.node-version"; \
		echo "Copied .node-version into $$WORKTREE_DIR"; \
	else \
		echo "No .node-version in $$REPO_DIR — skipping"; \
	fi
	@if [ -d "$$REPO_DIR/.config" ]; then \
		mkdir -p "$$WORKTREE_DIR/.config"; \
		cp -R "$$REPO_DIR/.config/." "$$WORKTREE_DIR/.config/"; \
		echo "Copied .config into $$WORKTREE_DIR"; \
	else \
		echo "No .config in $$REPO_DIR — skipping"; \
	fi
	@if [ -f "$$REPO_DIR/.compound-engineering/config.local.yaml" ]; then \
		mkdir -p "$$WORKTREE_DIR/.compound-engineering"; \
		cp "$$REPO_DIR/.compound-engineering/config.local.yaml" "$$WORKTREE_DIR/.compound-engineering/config.local.yaml"; \
		echo "Copied .compound-engineering/config.local.yaml into $$WORKTREE_DIR"; \
	else \
		echo "No .compound-engineering/config.local.yaml in $$REPO_DIR — skipping"; \
	fi
	@# Install JS deps so EVERY worktree (cron included) has node_modules and the
	@# husky git hooks (`pnpm install` runs the `prepare: husky` script). This is
	@# what keeps the commit-msg / pre-commit guardrails active in agent worktrees
	@# instead of being bypassed. bossd runs this target through the user's login
	@# shell, so nodenv/asdf shims are on PATH and `pnpm` resolves; the
	@# command-v guard below is a belt-and-braces fallback for stripped envs.
	@# Best-effort: a lockfile drift must not abort worktree setup.
	@if command -v pnpm >/dev/null 2>&1; then \
		echo "==> Installing JS deps + git hooks (pnpm)"; \
		( cd "$$WORKTREE_DIR" && pnpm install --frozen-lockfile ) \
			|| echo "pnpm install failed — deps/hooks unavailable (non-fatal)"; \
	else \
		echo "pnpm not found on PATH — skipping JS dep + hook install (non-fatal)"; \
	fi
	@# Install the Playwright Chromium the boss-proof pipeline drives (BOS-138). Best
	@# effort: guarded on pnpm + web node_modules and NEVER fatal — a worktree that
	@# can't fetch Chromium still provisions; `node scripts/proof.mjs doctor` reports
	@# the gap at run time (env-unavailable, not a crash). Host-level tools the proof
	@# video path needs are NOT installable here: run `brew install agg ffmpeg`
	@# (macOS) / your distro equivalent on the host once; doctor reports their absence.
	@if command -v pnpm >/dev/null 2>&1 && [ -d "$$WORKTREE_DIR/services/web/node_modules" ]; then \
		echo "==> Installing Playwright Chromium (boss-proof, best-effort)"; \
		( cd "$$WORKTREE_DIR" && pnpm exec playwright install chromium ) \
			|| echo "playwright install chromium failed — proof web/recipe capture unavailable (non-fatal)"; \
	else \
		echo "pnpm or services/web/node_modules missing — skipping Playwright Chromium install (non-fatal)"; \
	fi
	@# Prebuild the TUI-proof bridge + boss-e2e binaries so a proof run in this
	@# worktree reuses them instead of building inside the capture budget (BOS-215).
	@# Best-effort: guarded on `go` and NEVER fatal — a worktree that can't build
	@# still provisions; the proof dispatch falls back to an in-budget build.
	@if command -v go >/dev/null 2>&1; then \
		echo "==> Prebuilding TUI-proof binaries (bs-proof, best-effort)"; \
		( cd "$$WORKTREE_DIR" && $(MAKE) proof-tui-prebuild ) \
			|| echo "proof-tui-prebuild failed — TUI proof will build in-budget (non-fatal)"; \
	else \
		echo "go not found on PATH — skipping TUI-proof prebuild (non-fatal)"; \
	fi
	@# Approve direnv for the worktree so a developer's interactive shell doesn't hit
	@# a "blocked" prompt. Covers a bare .env too: direnv's global load_dotenv treats
	@# .env as an rc file that must be allowed. Guarded on the file existing, and the
	@# `|| echo` keeps it non-fatal if direnv finds no loadable rc (e.g. a dev without
	@# load_dotenv), so setup never fails on this step.
	@if command -v direnv >/dev/null 2>&1 && { [ -f "$$WORKTREE_DIR/.envrc" ] || [ -f "$$WORKTREE_DIR/.env" ]; }; then \
		( cd "$$WORKTREE_DIR" && direnv allow ) || echo "direnv allow failed (non-fatal)"; \
	fi
	@# Non-fatal bazel presence check (BOS-339). The `make test` facade delegates to
	@# `bazel test //...`, but worktree provisioning must never fail when Homebrew /
	@# bazelisk is absent (e.g. the public mirror has no Bazel workspace) — `make test`
	@# falls back to the native per-module `go test` loop in that case. Warn only.
	@if command -v bazel >/dev/null 2>&1; then \
		echo "bazel available ($$(bazel --version 2>/dev/null || echo 'version unknown')) — make test uses the cached bazel loop"; \
	else \
		echo "bazel not found on PATH — make test will fall back to the native go test loop (run 'make deps' to install bazelisk)"; \
	fi

## web-deps: Install web dependencies (needed for protoc-gen-es plugin)
$(WEB_DEPS_STAMP): services/web/package.json pnpm-lock.yaml
	pnpm install

## copy-skills: Mirror canonical embedded boss skills into the plugin copy.
## go:embed can't reach across module boundaries, so the bossd-plugin-claude
## binary needs its own copy of the skill payload. Without this sync the two
## embedded copies drift and clobber each other's installs in ~/.claude/skills
## on every CLI/daemon restart.
copy-skills:
	@rsync -a --delete "$(SKILLS_SRC_DIR)/" "$(SKILLS_PLUGIN_DIR)/"

## gen-skill: Regenerate the /boss skill command reference from the CLI, then mirror it.
## The reference region of services/boss/internal/skillinstall/skills/boss/SKILL.md is
## generated from the cobra command tree; TestSkillMatchesGenerated fails if it drifts.
gen-skill:
	cd services/boss && go run ./cmd gen-skill
	$(MAKE) copy-skills

## generate: Run buf generate to produce Go code from proto definitions
generate: buf-check-version $(GEN_STAMP)

# Make web deps and buf conditional — public repo has committed gen code and no web/
GEN_DEPS := $(PROTO_SOURCES)
ifneq ($(wildcard services/web/package.json),)
GEN_DEPS += $(WEB_DEPS_STAMP)
endif

$(GEN_STAMP): $(GEN_DEPS)
ifneq ($(shell command -v buf 2>/dev/null),)
	# Remove buf's generated Go output (stale files from renamed/removed protos) but
	# preserve committed Gazelle BUILD.bazel package files, which buf does not own —
	# `rm -rf lib/bossalib/gen` would delete them and trip the generate drift gate.
	find lib/bossalib/gen -type f ! -name 'BUILD.bazel' -delete 2>/dev/null || true
	buf generate
	@changed_go=$$(git diff --name-only -- lib/bossalib/gen | grep '\.go$$' || true); \
	if [ -n "$$changed_go" ]; then \
		printf '%s\n' "$$changed_go" | xargs go tool goimports -w; \
	fi
endif
	@touch $(GEN_STAMP)

## build: Build service binaries (generates protos first if needed)
build: $(addprefix $(BIN_DIR)/,$(SERVICE_BINS))

$(BIN_DIR)/boss: $(GEN_STAMP)
	go build -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/boss ./services/boss/cmd
	@if [ "$$(uname)" = "Darwin" ]; then codesign -s "$(CODESIGN_IDENTITY)" --force $(BIN_DIR)/boss; fi

$(BIN_DIR)/bossd: $(GEN_STAMP)
	go build -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/bossd ./services/bossd/cmd
	@if [ "$$(uname)" = "Darwin" ]; then codesign -s "$(CODESIGN_IDENTITY)" --force $(BIN_DIR)/bossd; fi

$(BIN_DIR)/mcp: $(GEN_STAMP)
	go build -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/mcp ./services/mcp/cmd
	@if [ "$$(uname)" = "Darwin" ]; then codesign -s "$(CODESIGN_IDENTITY)" --force $(BIN_DIR)/mcp; fi

ifneq ($(wildcard services/bosso/go.mod),)
$(BIN_DIR)/bosso: $(GEN_STAMP)
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
# verify them before exec (lib/bossalib/config, docs/plans/BOS-27-*.md). The
# generator excludes platform-suffixed cross-compiles, so this stays correct for
# the native `plugins` build; `plugins-all` does not touch the manifest.
plugins: $(addprefix $(BIN_DIR)/,$(PLUGIN_BINS))
	@node scripts/gen-plugin-sums.mjs $(BIN_DIR)

# Pattern rule for plugin binaries
$(BIN_DIR)/bossd-plugin-%: $(GEN_STAMP)
	go build -ldflags '$(LDFLAGS)' -o $@ ./plugins/bossd-plugin-$*

# bossd-plugin-claude embeds the boss skill payload; refresh the mirror before
# linking so the plugin binary and the boss CLI ship identical bytes.
$(BIN_DIR)/bossd-plugin-claude: copy-skills

## test-native-ledger: run ledger rows whose disposition is default-run (today-parity).
## These are the bazel-`manual` targets today's `make test` still had to cover
## natively; run after `bazel test //...`. RACE=1 injects -race into each command.
test-native-ledger:
	@node scripts/bazel/run-ledger.mjs --disposition default-run $(if $(filter 1,$(RACE)),--race,)

## test: Fast affected test (default) — runs only the tests selected for the files
## changed on this branch (test-affected; falls back to test-smoke when nothing maps).
## Fast is the default (BOS-371); the exhaustive whole-graph suite is `make test-all`,
## which CI/release runs.
test: test-affected

## test-all: Run tests across all modules (generates protos first if needed). Was
## `make test`. Delegates the Go module loop to `bazel test //...` (cached) + the
## native ledger step; falls back to the legacy per-module loop when bazel is
## unavailable (BOSS_NO_BAZEL set or not on PATH — keeps the public mirror green).
test-all: $(GEN_STAMP) copy-skills codex-skills-check vendor-toolbox-check
	$(MAKE) test-scripts
	$(MAKE) test-readme
	$(MAKE) test-no-inline-stop-hooks
	$(MAKE) test-public-mirror
ifeq ($(BAZEL_USABLE),1)
	$(BAZEL) test $(BAZEL_TEST_FLAGS) //...
	$(MAKE) test-native-ledger
else
	@echo "==> bazel unavailable (BOSS_NO_BAZEL set or bazel not on PATH) — native module loop (public-mirror fallback)"
	@for mod in $(MODULES); do \
		echo "==> Testing $$mod"; \
		$(MAKE) -C $$mod test-all; \
	done
endif
	@if [ -d services/docs ]; then \
		echo "==> Testing services/docs"; \
		$(MAKE) -C services/docs test; \
	fi
# Net-new web coverage: `make test` had none before BOS-342. Guarded so the
# public mirror (no services/web) never invokes turbo.
ifneq ($(wildcard services/web/package.json),)
	$(MAKE) test-web
endif

## test-full: Alias for the exhaustive suite (`make test-all`). Kept for the agent
## command ladder / docs that name the "full" suite explicitly.
test-full:
	$(MAKE) test-all

ifneq ($(wildcard services/web/package.json),)
## test-web: Run web+marketing tests/lint/typecheck through turbo (cached).
## Machine-wide TURBO_CACHE_DIR (exported above) lets sibling worktrees share
## hits. Deliberately kept out of test-smoke — node startup + turbo hash cost.
test-web: $(WEB_DEPS_STAMP)
	pnpm turbo run test lint typecheck --filter=web --filter=marketing
endif

## test-smoke: Fast agent loop. No coverage, no race, no forced cache bypass.
## Delegates to the cached bazel loop under `-test.short`; falls back to the native
## `-short` module loop when bazel is unavailable (public-mirror fallback).
test-smoke: $(GEN_STAMP) copy-skills codex-skills-check vendor-toolbox-check
	node --test scripts/bs-*-skill.test.mjs
ifeq ($(BAZEL_USABLE),1)
	$(BAZEL) test $(BAZEL_TEST_FLAGS) --test_arg=-test.short //...
else
	@echo "==> bazel unavailable — native -short module loop (public-mirror fallback)"
	@for mod in $(MODULES); do \
		echo "==> Smoke-testing $$mod"; \
		$(MAKE) -C $$mod test-fast; \
	done
endif

## test-profile: Run full Go modules with JSON output and print only the slowest results.
test-profile:
	@log=$$(mktemp); \
	trap 'rm -f "$$log"' EXIT; \
	test_status=0; \
	for mod in lib/bossalib services/boss services/bossd services/bosso; do \
		echo "==> $$mod"; \
		(cd $$mod && go test -json -timeout 300s ./...) >>"$$log" 2>&1; \
		mod_status=$$?; \
		if [ "$$mod_status" -ne 0 ] && [ "$$test_status" -eq 0 ]; then \
			test_status=$$mod_status; \
		fi; \
	done; \
	node scripts/test-profile-summary.mjs "$$log"; \
	summary_status=$$?; \
	if [ "$$test_status" -ne 0 ]; then \
		exit "$$test_status"; \
	fi; \
	exit "$$summary_status"

test-affected:
	@commands="$$(node scripts/select-affected-tests.mjs)" || exit $$?; \
	if [ -z "$$commands" ]; then \
		echo "make test-smoke"; \
		$(MAKE) test-smoke || exit $$?; \
	else \
		printf '%s\n' "$$commands" | while IFS= read -r command; do \
			[ -z "$$command" ] && continue; \
			echo "$$command"; \
			if { [ "$$command" = "make test-smoke" ] && ! grep -Eq '^test-smoke:' Makefile; }; then \
				echo "Pending target fallback: make test-scripts"; \
				$(MAKE) test-scripts || exit $$?; \
			else \
				eval "$$command" || exit $$?; \
			fi; \
		done; \
	fi

## format-affected: Format only files changed vs origin/main (+ staged/untracked). Fast local/agent loop.
format-affected:
	@node scripts/format-affected.mjs

## test-race: Run the full test suite under -race (sets RACE=1 for sub-makes).
## Race detector is opt-in: `make test-all` skips it; `make test-race` or
## `RACE=1 make test-all` enables it. Runs the exhaustive suite, not the affected one.
test-race:
	@$(MAKE) test-all RACE=1

## Per-module test targets (no generate dep — CI uses committed gen code).
## Each delegates to `bazel test //<module>/...` (cached) + that module's
## default-run ledger rows, with a native-loop fallback when bazel is absent.
test-bossalib:
ifeq ($(BAZEL_USABLE),1)
	$(BAZEL) test $(BAZEL_TEST_FLAGS) //lib/bossalib/...
	@node scripts/bazel/run-ledger.mjs --module lib/bossalib --disposition default-run $(if $(filter 1,$(RACE)),--race,)
else
	$(MAKE) -C lib/bossalib test-all
endif

test-boss:
ifeq ($(BAZEL_USABLE),1)
	$(BAZEL) test $(BAZEL_TEST_FLAGS) //services/boss/...
	@node scripts/bazel/run-ledger.mjs --module services/boss --disposition default-run $(if $(filter 1,$(RACE)),--race,)
else
	$(MAKE) -C services/boss test-all
endif

test-bossd:
ifeq ($(BAZEL_USABLE),1)
	$(BAZEL) test $(BAZEL_TEST_FLAGS) //services/bossd/...
	@node scripts/bazel/run-ledger.mjs --module services/bossd --disposition default-run $(if $(filter 1,$(RACE)),--race,)
else
	$(MAKE) -C services/bossd test-all
endif

test-mcp:
ifeq ($(BAZEL_USABLE),1)
	$(BAZEL) test $(BAZEL_TEST_FLAGS) //services/mcp/...
	@node scripts/bazel/run-ledger.mjs --module services/mcp --disposition default-run $(if $(filter 1,$(RACE)),--race,)
else
	$(MAKE) -C services/mcp test-all
endif

ifneq ($(wildcard services/mcp-gateway/go.mod),)
test-mcp-gateway:
ifeq ($(BAZEL_USABLE),1)
	$(BAZEL) test $(BAZEL_TEST_FLAGS) //services/mcp-gateway/...
	@node scripts/bazel/run-ledger.mjs --module services/mcp-gateway --disposition default-run $(if $(filter 1,$(RACE)),--race,)
else
	$(MAKE) -C services/mcp-gateway test-all
endif
endif

## test-integration-bossd: Run bossd integration tests (requires tmux on PATH; gated by 'integration' build tag)
test-integration-bossd:
	cd services/bossd && go test -tags=integration -race ./internal/server/... ./internal/testharness/... -count=1

ifneq ($(wildcard services/bosso/go.mod),)
test-bosso:
ifeq ($(BAZEL_USABLE),1)
	$(BAZEL) test $(BAZEL_TEST_FLAGS) //services/bosso/...
	@node scripts/bazel/run-ledger.mjs --module services/bosso --disposition default-run $(if $(filter 1,$(RACE)),--race,)
else
	$(MAKE) -C services/bosso test-all
endif

test-bosso-scale:
	cd services/bosso && go test ./internal/loadtest -run TestOrchestratorScaleSmoke -count=1
endif

# Auto-generate per-plugin test targets from detected modules. Same bazel-facade
# pattern as the service modules; plugins have no ledger rows so run-ledger is a
# no-op for them (kept uniform). Double-$ so the vars survive $(call)/$(eval).
define define-plugin-test
test-$(2):
ifeq ($$(BAZEL_USABLE),1)
	$$(BAZEL) test $$(BAZEL_TEST_FLAGS) //$(1)/...
	@node scripts/bazel/run-ledger.mjs --module $(1) --disposition default-run $$(if $$(filter 1,$$(RACE)),--race,)
else
	$$(MAKE) -C $(1) test-all
endif
endef
$(foreach p,$(PLUGIN_MODULES),$(eval \
  $(call define-plugin-test,$(p),$(patsubst bossd-plugin-%,%,$(notdir $(p))))))

# Plugin claude tests rely on the embedded skill payload — keep it in sync.
test-claude: copy-skills

## coverage-<module>: native coverage profile (D12 — explicit; the bazel hot loop
## emits no coverage.out). The module `test-all` target runs `go test` with
## `-coverprofile=coverage.out` (mk/go-test.mk; BOS-373 — the fast default `test`
## carries no coverage), so each is just that native exhaustive run.
coverage-bossalib:
	$(MAKE) -C lib/bossalib test-all

coverage-boss:
	$(MAKE) -C services/boss test-all

coverage-bossd:
	$(MAKE) -C services/bossd test-all

coverage-mcp:
	$(MAKE) -C services/mcp test-all

ifneq ($(wildcard services/mcp-gateway/go.mod),)
coverage-mcp-gateway:
	$(MAKE) -C services/mcp-gateway test-all
endif

ifneq ($(wildcard services/bosso/go.mod),)
coverage-bosso:
	$(MAKE) -C services/bosso test-all
endif

## buf-check-version: Fail if an installed buf does not match $(BUF_VERSION).
## Soft-passes when buf is absent so the public mirror's bufless path (committed
## gen code, no buf) stays green — only a PRESENT-but-mismatched buf is fatal.
buf-check-version:
	@if ! command -v buf >/dev/null 2>&1; then \
		echo "buf not installed — skipping version check (committed gen used; run 'make deps' to install the pinned buf)"; \
	else \
		want="$(BUF_VERSION)"; \
		if ! buf --version 2>/dev/null | grep -Eq "^v?$${want#v}$$"; then \
			echo "buf version mismatch: want $$want"; \
			echo "  got: $$(buf --version 2>/dev/null)"; \
			echo "  run 'make deps' to install the pinned version"; \
			exit 1; \
		fi; \
	fi

## deps-golangci: Install ONLY the pinned golangci-lint (no Homebrew/full deps) — for CI lint jobs
deps-golangci:
	@# Same pinned install as `make deps`, isolated so lint-only CI runners don't need Homebrew.
	@gobin=$$(go env GOBIN); [ -z "$$gobin" ] && gobin=$$(go env GOPATH)/bin; \
	want="$(GOLANGCI_LINT_VERSION)"; \
	if command -v golangci-lint >/dev/null 2>&1 && \
	   golangci-lint --version 2>/dev/null | grep -Eq "version (v)?$${want#v}( |$$)"; then \
		echo "    golangci-lint: $$want already installed"; \
	else \
		echo "    golangci-lint: installing $$want..."; \
		curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/$$want/install.sh \
			| sh -s -- -b "$$gobin" $$want; \
	fi

## lint-check-version: Fail if installed golangci-lint does not match $(GOLANGCI_LINT_VERSION)
lint-check-version:
	@if ! command -v golangci-lint >/dev/null 2>&1; then \
		echo "golangci-lint not installed. Run 'make deps'."; \
		exit 1; \
	fi
	@want="$(GOLANGCI_LINT_VERSION)"; \
	if ! golangci-lint --version 2>/dev/null | grep -Eq "version (v)?$${want#v}( |$$)"; then \
		echo "golangci-lint version mismatch: want $$want"; \
		echo "  got: $$(golangci-lint --version 2>/dev/null)"; \
		echo "  run 'make deps' to install the pinned version"; \
		exit 1; \
	fi

## lint: Fast affected lint (default) — buf lint + cached golangci (lint-affected,
## content-stamp-skipped) + the standalone gofmt/goimports gate + docs lint, with the
## markdown format check scoped to CHANGED files (lint-docs-affected). Fast is the
## default (BOS-371); the exhaustive whole-tree markdown pass is `make lint-all`.
lint: buf-check-version lint-check-version $(GEN_STAMP)
	buf lint
	$(MAKE) lint-scripts
	@node scripts/lint-affected.mjs
	$(MAKE) lint-go-fmt
	@if [ -d services/docs ]; then \
		echo "==> Linting services/docs"; \
		$(MAKE) -C services/docs lint; \
	fi
	@if command -v pnpm >/dev/null 2>&1 && [ -f package.json ]; then \
		echo "==> Checking changed docs markdown formatting"; \
		node scripts/lint-docs-affected.mjs; \
	fi

## lint-all: Exhaustive lint — the same buf/golangci/gofmt/docs gates as `make lint`,
## but with the whole-tree `prettier --check` over ALL markdown (was `make lint`).
## CI/release gate.
lint-all: buf-check-version lint-check-version $(GEN_STAMP)
	buf lint
	$(MAKE) lint-scripts
	@node scripts/lint-affected.mjs
	$(MAKE) lint-go-fmt
	@if [ -d services/docs ]; then \
		echo "==> Linting services/docs"; \
		$(MAKE) -C services/docs lint; \
	fi
	@if command -v pnpm >/dev/null 2>&1 && [ -f package.json ]; then \
		echo "==> Checking docs markdown formatting"; \
		pnpm run lint:docs; \
	fi

## lint-go-fmt: Cheap standalone whole-tree Go format gate (~1s): gofmt -l + goimports -l.
## Replaces the golangci `formatters:` block dropped in BOS-371 — golangci re-checked
## formatting slowly (measured bossd 22s → 7s without it); this closes the same
## whole-tree drift hole (e.g. `--no-verify` cron commits) far more cheaply.
lint-go-fmt:
	@node scripts/check-go-format.mjs

## Per-module lint targets
lint-proto:
	buf lint

lint-bossalib: lint-check-version
	node scripts/lint-affected.mjs --module lib/bossalib

lint-boss: lint-check-version
	node scripts/lint-affected.mjs --module services/boss

lint-bossd: lint-check-version
	node scripts/lint-affected.mjs --module services/bossd

lint-mcp: lint-check-version
	node scripts/lint-affected.mjs --module services/mcp

ifneq ($(wildcard services/mcp-gateway/go.mod),)
lint-mcp-gateway: lint-check-version
	node scripts/lint-affected.mjs --module services/mcp-gateway
endif

test-readme:
	node scripts/check-readme-assets.mjs

## readme-gifs: Generate README GIFs from tour asciinema casts
readme-gifs:
	@if ! command -v agg >/dev/null 2>&1; then \
		echo "agg is required. Install with: brew install asciinema/agg/agg"; \
		exit 1; \
	fi
	@mkdir -p "$(README_TOUR_GIF_DIR)"
	@set -e; \
	casts=$$(find "$(README_TOUR_CAST_DIR)" -maxdepth 1 -name '*.cast' -type f | sort); \
	if [ -z "$$casts" ]; then \
		echo "No asciinema casts found in $(README_TOUR_CAST_DIR)"; \
		exit 1; \
	fi; \
	for cast in $$casts; do \
		name=$$(basename "$$cast" .cast); \
		echo "==> $$name.gif"; \
		agg \
			--text-font-family "JetBrains Mono,Fira Code,SF Mono,Menlo,Consolas,DejaVu Sans Mono,Liberation Mono" \
			--font-size 24 \
			--line-height 1.25 \
			--speed 1 \
			"$$cast" \
			"$(README_TOUR_GIF_DIR)/$$name.gif"; \
	done

test-public-mirror:
	node scripts/check-public-mirror-workflows.mjs

test-no-inline-stop-hooks:
	node scripts/check-no-inline-stop-hooks.mjs

## test-manifest-update: Regenerate the checked-in test command manifest.
test-manifest-update:
	mkdir -p docs/testing
	node scripts/generate-test-command-manifest.mjs > docs/testing/test-command-manifest.md

## test-manifest: Fail if agent test guidance manifest is stale.
test-manifest:
	node scripts/check-test-command-manifest.mjs

test-docs:
	$(MAKE) -C services/docs test

proof:
	node scripts/proof.mjs run

proof-plan:
	node scripts/proof.mjs plan

proof-test:
	node --test scripts/proof-lib.test.mjs scripts/proof-playwright-runner.test.mjs scripts/proof-video.test.mjs scripts/proof-video-intro.test.mjs scripts/proof-poster.test.mjs

## proof-tui-prebuild: Build the TUI-proof bridge + boss-e2e binaries into ./bin so a
## proof run reuses them instead of building inside the capture budget (BOS-215).
## Always rebuilds — the binary paths are authoritative once written; the proof
## dispatch only checks existence, so re-running this target owns freshness.
proof-tui-prebuild:
	go build -tags e2e -o $(BIN_DIR)/proof-tui-bridge ./services/boss/cmd/proof-tui-agent
	go build -tags e2e -o $(BIN_DIR)/boss-e2e ./services/boss/cmd

test-scripts:
	$(MAKE) -C scripts test

lint-docs:
	$(MAKE) -C services/docs lint

lint-scripts:
	$(MAKE) -C scripts lint

build-docs:
	$(MAKE) -C services/docs build

ifneq ($(wildcard services/bosso/go.mod),)
lint-bosso: lint-check-version
	node scripts/lint-affected.mjs --module services/bosso
endif

# Auto-generate per-plugin lint targets from detected modules
define define-plugin-lint
lint-$(2): lint-check-version
	node scripts/lint-affected.mjs --module $(1)
endef
$(foreach p,$(PLUGIN_MODULES),$(eval \
  $(call define-plugin-lint,$(p),$(patsubst bossd-plugin-%,%,$(notdir $(p))))))

## Technical-debt scanners (make debt-<kind>-<module>) — narrow, per-module, NON-BLOCKING
## detectors used by the bs-sweep-debt skill. Each auto-fetches its pinned tool via
## `go run`. `<module>` is the short name: bossalib, boss, bossd, bosso, or a plugin short
## name (claude, codex, dependabot, linear, repair, sentry). Examples:
##   make debt-deadcode-bossd   # unreachable code (whole-program, includes tests)
##   make debt-dupl-bossalib    # duplicated token sequences within the module
##   make debt-cyclo-boss       # functions over the cyclomatic-complexity threshold
##   make debt-vuln-bosso       # reachable known vulnerabilities (govulncheck)
define define-debt-targets
debt-deadcode-$(2):
	cd $(1) && go run $$(DEADCODE_PKG) -test ./...
debt-dupl-$(2):
	cd $(1) && go run $$(DUPL_PKG) -t $$(DEBT_DUPL_THRESHOLD) .
debt-cyclo-$(2):
	cd $(1) && go run $$(GOCYCLO_PKG) -over $$(DEBT_CYCLO_THRESHOLD) .
debt-vuln-$(2):
	cd $(1) && go run $$(GOVULNCHECK_PKG) ./...
endef
$(foreach m,$(MODULES),$(eval \
  $(call define-debt-targets,$(m),$(patsubst bossd-plugin-%,%,$(notdir $(m))))))

## debt-knip: Report unused TS exports/files/deps in services/web (non-blocking detector).
debt-knip:
	$(MAKE) -C services/web knip

## Per-module build targets (no generate dep — CI uses committed gen code)
build-boss:
	go build -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/boss ./services/boss/cmd
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

# bossd-plugin-claude embeds the boss skill payload — keep the per-plugin
# convenience target in sync with the pattern rule above.
build-claude: copy-skills

## format: Fast affected format (default) — format only the files changed on this
## branch (format-affected: gofmt/goimports on changed .go; prettier/biome on changed
## web/markdown; syncpack when a package.json changed). Fast is the default (BOS-371);
## the whole-tree format is `make format-all`.
format:
	@node scripts/format-affected.mjs

## format-all: Exhaustive whole-tree format — Go (gofmt), web (biome), package.json
## (syncpack), and ALL markdown (prettier). Slow (was `make format`); CI/release and
## BOS-372's sweep are the hard gate.
format-all:
	@if command -v pnpm >/dev/null 2>&1 && [ -f package.json ]; then \
		pnpm syncpack format; \
		pnpm syncpack fix; \
	fi
	@for mod in $(MODULES); do \
		echo "==> Formatting $$mod"; \
		$(MAKE) -C $$mod format; \
	done
	@if [ -d services/web ]; then \
		$(MAKE) -C services/web format; \
	fi
	@if [ -d services/docs ]; then \
		$(MAKE) -C services/docs format; \
	fi
	$(MAKE) -C scripts format
	@if command -v pnpm >/dev/null 2>&1 && [ -f package.json ]; then \
		pnpm run format:docs; \
	fi

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
plugins-all: $(GEN_STAMP) copy-skills
	@for platform in $(PLATFORMS); do \
		os=$${platform%%/*}; \
		arch=$${platform##*/}; \
		for plugin in $(DIST_PLUGINS); do \
			echo "==> Building $$plugin ($$os/$$arch)"; \
			GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 go build -ldflags '$(LDFLAGS)' \
				-o $(BIN_DIR)/$$plugin-$$os-$$arch ./plugins/$$plugin; \
		done; \
	done

## release-codex-check: Verify the codex plugin builds for every distribution
## platform. Invoked from CI (and by humans before merging codex changes) so
## the release workflow never fails on a missing GOOS/GOARCH cross-build. The
## per-platform set mirrors PLATFORMS plus linux/arm64 (added explicitly here
## because the broader PLATFORMS list will catch up in a future commit).
##
## Deliberately does NOT depend on $(GEN_STAMP). The cross-build only needs
## the committed proto-generated code under lib/bossalib/gen/, which is
## already on disk in any clean checkout. Pulling GEN_STAMP would chain in
## $(WEB_DEPS_STAMP) (pnpm install for protoc-gen-es) — and CI for the
## codex plugin doesn't install pnpm, which would fail this gate at the
## point of the dependency resolution rather than the actual build.
RELEASE_CODEX_PLATFORMS := darwin/amd64 darwin/arm64 linux/amd64 linux/arm64
release-codex-check:
	@for platform in $(RELEASE_CODEX_PLATFORMS); do \
		os=$${platform%%/*}; \
		arch=$${platform##*/}; \
		echo "==> Building bossd-plugin-codex ($$os/$$arch)"; \
		GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 go build -ldflags '$(LDFLAGS)' \
			-o $(BIN_DIR)/bossd-plugin-codex-$$os-$$arch ./plugins/bossd-plugin-codex \
			|| { echo "FAIL: bossd-plugin-codex did not build for $$os/$$arch"; exit 1; }; \
	done
	@echo "==> bossd-plugin-codex builds clean across $(RELEASE_CODEX_PLATFORMS)"

## clean: Remove build artifacts and generated code
clean:
	rm -rf $(BIN_DIR)
	rm -f $(GEN_STAMP)
	rm -rf $(MUTATE_DIR)
	@for mod in $(MODULES); do \
		$(MAKE) -C $$mod clean; \
	done
	@if [ -d services/web ]; then \
		$(MAKE) -C services/web clean; \
	fi
	@if [ -d services/docs ]; then \
		$(MAKE) -C services/docs clean; \
	fi

## kill: Stop all local boss dev processes for this repo (daemon, plugins, web/FE)
kill:
	@echo "==> Killing boss dev processes for $(CURDIR)"
	@pids=""; \
	add() { for p in $$1; do case " $$pids " in *" $$p "*) ;; *) pids="$$pids $$p" ;; esac; done; }; \
	for sock in \
		"$(CURDIR)/.config/bossd.sock" \
		"$$HOME/Library/Application Support/bossanova/bossd.sock" \
		"$$HOME/.config/bossanova/bossd.sock"; do \
		[ -S "$$sock" ] && add "$$(lsof -t "$$sock" 2>/dev/null)"; \
	done; \
	add "$$(pgrep -f '$(CURDIR)/bin/boss' 2>/dev/null)"; \
	for d in services/web services/marketing services/bosso/web services/docs; do \
		add "$$(pgrep -f "$(CURDIR)/$$d" 2>/dev/null)"; \
	done; \
	for p in $$(pgrep -f '/go-build.*/exe/' 2>/dev/null); do \
		lsof -p $$p 2>/dev/null | grep -qF '$(CURDIR)' && add "$$p"; \
	done; \
	self=$$$$; pids="$$(printf '%s\n' $$pids | sort -un | grep -vx $$self || true)"; \
	if [ -z "$$pids" ]; then echo "  nothing running"; exit 0; fi; \
	for p in $$pids; do ps -o pid=,command= -p $$p 2>/dev/null | sed 's/^/  kill /'; done; \
	kill $$pids 2>/dev/null || true; \
	sleep 1; \
	still=""; for p in $$pids; do kill -0 $$p 2>/dev/null && still="$$still $$p"; done; \
	[ -n "$$still" ] && { echo "  force -9:$$still"; kill -9 $$still 2>/dev/null || true; } || true; \
	echo "==> Done."

## kill-all: Stop EVERY local boss process on this machine — across ALL worktrees
## AND the main checkout. Unlike `kill` (scoped to $(CURDIR)), this matches by
## EXECUTABLE basename + socket ownership, never by argv text, so it (a) catches
## daemons started from another repo dir and (b) will NOT false-kill an unrelated
## process whose arguments merely mention a boss path (e.g. an agent carrying a
## prompt). WARNING: this also stops boss daemons/plugins owned by OTHER running
## worktrees (e.g. concurrent cron sessions). Use `make kill` for just this repo.
kill-all:
	@echo "==> Killing ALL boss processes on this machine (every worktree + main checkout)"
	@pids=""; \
	add() { for p in $$1; do case " $$pids " in *" $$p "*) ;; *) pids="$$pids $$p" ;; esac; done; }; \
	add "$$(ps -axo pid=,comm= | awk '$$2 ~ /\/(boss|bossd|bosso|bossd?-plugin-[a-z0-9-]+)$$/ {print $$1}')"; \
	for p in $$(pgrep -x cmd 2>/dev/null); do \
		lsof -p $$p 2>/dev/null | grep -qiE 'boss[od]\.(sock|db)' && add "$$p"; \
	done; \
	self=$$$$; pids="$$(printf '%s\n' $$pids | sort -un | grep -vx $$self || true)"; \
	if [ -z "$$pids" ]; then echo "  nothing running"; exit 0; fi; \
	for p in $$pids; do ps -o pid=,command= -p $$p 2>/dev/null | cut -c1-100 | sed 's/^/  kill /'; done; \
	kill $$pids 2>/dev/null || true; \
	sleep 1; \
	still=""; for p in $$pids; do kill -0 $$p 2>/dev/null && still="$$still $$p"; done; \
	[ -n "$$still" ] && { echo "  force -9:$$still"; kill -9 $$still 2>/dev/null || true; } || true; \
	echo "==> Done."

## release: Trigger the production release workflow (creates a PR from main → production)
release:
	gh workflow run create-production-release.yml --ref main

## stage-release: Trigger the staging release workflow (creates a PR from main → staging)
stage-release:
	gh workflow run create-staging-release.yml --ref main

## split: Mirror subtrees to separate repos via splitsh/lite
split:
	splitsh-lite --prefix=proto --target=refs/heads/split/proto
	splitsh-lite --prefix=lib/bossalib --target=refs/heads/split/bossalib
	splitsh-lite --prefix=services/boss --target=refs/heads/split/boss
	splitsh-lite --prefix=services/bossd --target=refs/heads/split/bossd

# --- Mutation Testing ---------------------------------------------------
# NOTE: gremlins ./... silently produces no results in Go workspaces.
# We work around this by iterating over individual packages per module.
# We also skip packages with no test files to avoid "go: no such tool covdata"
# errors from gremlins on Go 1.25+ (covdata moved out of GOROOT/pkg/tool/).
MUTATE_TIMEOUT := 30

## mutate: Run mutation testing across all modules
mutate: $(GEN_STAMP)
	@mkdir -p $(MUTATE_DIR)
	@root=$$(git rev-parse --show-toplevel); \
	failed=0; \
	for mod in $(MODULES); do \
		echo "==> Mutating $$mod"; \
		modname=$$(basename $$mod); \
		modabs=$$(cd $$mod && pwd); \
		for pkg in $$(cd $$mod && go list -f '{{if .TestGoFiles}}{{.ImportPath}}{{end}}' ./...); do \
			pkgdir=$$(cd $$mod && go list -f '{{.Dir}}' "$$pkg"); \
			reldir=$${pkgdir#$$modabs/}; \
			[ "$$reldir" = "$$pkgdir" ] && reldir="."; \
			if [ "$$reldir" = "." ]; then safename="root"; else safename=$$(echo "$$reldir" | tr '/' '-'); fi; \
			echo "    -> $$modname/$$reldir"; \
			(cd $$mod && gremlins unleash \
				-o "$$root/$(MUTATE_DIR)/$$modname--$$safename.json" \
				--timeout-coefficient $(MUTATE_TIMEOUT) \
				--workers 0 \
				"./$$reldir") || failed=1; \
		done; \
	done; \
	echo ""; \
	echo "==> Reports in $(MUTATE_DIR)/"; \
	echo "==> Run 'make mutate-report' for summary"; \
	if [ "$$failed" = "1" ]; then exit 1; fi

## mutate-diff: Mutation testing on changed code only (fast, for PRs)
mutate-diff: $(GEN_STAMP)
	@mkdir -p $(MUTATE_DIR)
	@root=$$(git rev-parse --show-toplevel); \
	for mod in $(MODULES); do \
		echo "==> Mutating changed code in $$mod"; \
		modname=$$(basename $$mod); \
		modabs=$$(cd $$mod && pwd); \
		for pkg in $$(cd $$mod && go list -f '{{if .TestGoFiles}}{{.ImportPath}}{{end}}' ./...); do \
			pkgdir=$$(cd $$mod && go list -f '{{.Dir}}' "$$pkg"); \
			reldir=$${pkgdir#$$modabs/}; \
			[ "$$reldir" = "$$pkgdir" ] && reldir="."; \
			if [ "$$reldir" = "." ]; then safename="root"; else safename=$$(echo "$$reldir" | tr '/' '-'); fi; \
			(cd $$mod && gremlins unleash \
				--diff main \
				-o "$$root/$(MUTATE_DIR)/$$modname--$$safename.json" \
				--timeout-coefficient $(MUTATE_TIMEOUT) \
				--workers 0 \
				"./$$reldir") || true; \
		done; \
	done

## mutate-report: Summarize mutation testing results
mutate-report:
	@echo "=== Mutation Testing Summary ==="
	@for f in $(MUTATE_DIR)/*.json; do \
		[ -f "$$f" ] || continue; \
		name=$$(basename "$$f" .json); \
		echo ""; \
		echo "--- $$name ---"; \
		jq -r '"  Efficacy:     \(.test_efficacy // "n/a")%\n  Coverage:     \(.mutations_coverage // "n/a")%\n  Total:        \(.mutants_total // 0)\n  Killed:       \(.mutants_killed // 0)\n  Lived:        \(.mutants_lived // 0)\n  Not covered:  \(.mutants_not_covered // 0)"' "$$f" 2>/dev/null \
			|| echo "  (no results)"; \
	done
	@echo ""
	@echo "==> Surviving mutants: make mutate-survivors"

## mutate-survivors: List surviving mutants (for LLM consumption)
mutate-survivors:
	@for f in $(MUTATE_DIR)/*.json; do \
		[ -f "$$f" ] || continue; \
		name=$$(basename "$$f" .json); \
		jq -r --arg mod "$$name" \
			'.files[]? | .file_name as $$file | .mutations[]? | select(.status == "LIVED") | "[\($$mod)] \($$file):\(.line) \(.type)"' \
			"$$f" 2>/dev/null; \
	done

## mutate-uncovered: List NOT_COVERED mutants (untested code, for coverage work)
mutate-uncovered:
	@for f in $(MUTATE_DIR)/*.json; do \
		[ -f "$$f" ] || continue; \
		name=$$(basename "$$f" .json); \
		jq -r --arg mod "$$name" \
			'.files[]? | .file_name as $$file | .mutations[]? | select(.status == "NOT_COVERED" or .status == "NOT COVERED") | "[\($$mod)] \($$file):\(.line) \(.type)"' \
			"$$f" 2>/dev/null; \
	done

## mutate-coverage: Rank packages by mutation coverage, lowest first.
## Columns (tab-separated): coverage%  not_covered  package
mutate-coverage:
	@for f in $(MUTATE_DIR)/*.json; do \
		[ -f "$$f" ] || continue; \
		name=$$(basename "$$f" .json); \
		jq -r --arg mod "$$name" \
			'"\(.mutations_coverage // 0)\t\(.mutants_not_covered // 0)\t\($$mod)"' \
			"$$f" 2>/dev/null; \
	done | sort -n

## mutate-fix: Feed surviving mutants to Claude Code to generate tests
mutate-fix:
	@mkdir -p $(MUTATE_DIR)
	@$(MAKE) --no-print-directory mutate-survivors > $(MUTATE_DIR)/survivors.txt 2>/dev/null
	@count=$$(wc -l < $(MUTATE_DIR)/survivors.txt | tr -d ' '); \
	if [ "$$count" = "0" ]; then \
		echo "No surviving mutants. Run 'make mutate' first."; \
		exit 0; \
	fi; \
	echo "==> $$count surviving mutants found"; \
	echo "==> Launching Claude Code to generate tests..."; \
	cat $(MUTATE_DIR)/survivors.txt | claude -p --dangerously-skip-permissions "$$(cat .claude/prompts/mutate-fix.md)"

## mutate-loop: Full cycle — mutate, fix survivors, verify
mutate-loop:
	@$(MAKE) mutate
	@$(MAKE) mutate-fix
	@echo ""
	@echo "==> Verifying fixes..."
	@$(MAKE) mutate
	@$(MAKE) mutate-report

## Per-module mutation targets
define run-mutate-module
	@mkdir -p $(MUTATE_DIR)
	@root=$$(git rev-parse --show-toplevel); \
	modabs=$$(cd $(1) && pwd); \
	for pkg in $$(cd $(1) && go list -f '{{if .TestGoFiles}}{{.ImportPath}}{{end}}' ./...); do \
		pkgdir=$$(cd $(1) && go list -f '{{.Dir}}' "$$pkg"); \
		reldir=$${pkgdir#$$modabs/}; \
		[ "$$reldir" = "$$pkgdir" ] && reldir="."; \
		if [ "$$reldir" = "." ]; then safename="root"; else safename=$$(echo "$$reldir" | tr '/' '-'); fi; \
		echo "==> $(2)/$$reldir"; \
		(cd $(1) && gremlins unleash \
			-o "$$root/$(MUTATE_DIR)/$(2)--$$safename.json" \
			--timeout-coefficient $(3) \
			--workers 0 \
			"./$$reldir") || true; \
	done
endef

## mutate-pkg: Mutation-test a single package (fast, for targeted verification).
## Usage: make mutate-pkg MODULE=services/bosso PKG=./internal/auth
## MODNAME defaults to $(notdir MODULE), matching the full `mutate` target's
## JSON naming (e.g. plugins stay bossd-plugin-claude); override only to retarget.
mutate-pkg:
	@test -n "$(MODULE)" || { echo "MODULE is required (e.g. MODULE=services/bosso)"; exit 2; }
	@test -n "$(PKG)" || { echo "PKG is required (e.g. PKG=./internal/auth)"; exit 2; }
	@$(MAKE) --no-print-directory $(GEN_STAMP)
	@mkdir -p $(MUTATE_DIR)
	@root=$$(git rev-parse --show-toplevel); \
	modname="$(MODNAME)"; [ -n "$$modname" ] || modname=$$(basename "$(MODULE)"); \
	reldir=$$(echo "$(PKG)" | sed -e 's|^\./||' -e 's|/$$||'); \
	if [ -z "$$reldir" ] || [ "$$reldir" = "." ]; then safename="root"; else safename=$$(echo "$$reldir" | tr '/' '-'); fi; \
	echo "==> $$modname/$$reldir"; \
	(cd "$(MODULE)" && gremlins unleash \
		-o "$$root/$(MUTATE_DIR)/$$modname--$$safename.json" \
		--timeout-coefficient $(MUTATE_TIMEOUT) \
		--workers 0 \
		"$(PKG)")

mutate-bossalib:
	$(call run-mutate-module,lib/bossalib,bossalib,$(MUTATE_TIMEOUT))

mutate-boss:
	$(call run-mutate-module,services/boss,boss,$(MUTATE_TIMEOUT))

mutate-bossd:
	$(call run-mutate-module,services/bossd,bossd,$(MUTATE_TIMEOUT))

ifneq ($(wildcard services/bosso/go.mod),)
mutate-bosso:
	$(call run-mutate-module,services/bosso,bosso,$(MUTATE_TIMEOUT))
endif

# Auto-generate per-plugin mutate targets from detected modules
define define-plugin-mutate
mutate-$(2):
	$$(call run-mutate-module,$(1),$(notdir $(1)),$$(MUTATE_TIMEOUT))
endef
$(foreach p,$(PLUGIN_MODULES),$(eval \
  $(call define-plugin-mutate,$(p),$(patsubst bossd-plugin-%,%,$(notdir $(p))))))

ngrok:
	ngrok http --url=bossanova.ngrok.app 8080
