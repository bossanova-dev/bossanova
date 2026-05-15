.PHONY: all build build-all build-docs clean copy-skills deps format generate lint \
	lint-check-version lint-docs \
	mutate mutate-diff mutate-fix mutate-loop mutate-report mutate-survivors \
	plugins plugins-all release release-codex-check \
	setup-worktree split stage-release test test-race \
	test-docs test-integration-bossd

## all: Clean, generate protos, format, and build all binaries (default target)
all:
	$(MAKE) clean
	$(MAKE) generate
	$(MAKE) format
	$(MAKE) build plugins build-all plugins-all

# Pinned golangci-lint version. Must match the version used in CI
# (.github/workflows/*.yml). Bumping requires coordinated changes to both.
GOLANGCI_LINT_VERSION := v2.11.4

# Binaries output to bin/
BIN_DIR := bin

# Auto-detect Go modules (works in both private and public repos)
MODULES := $(patsubst %/go.mod,%,$(wildcard lib/*/go.mod services/*/go.mod plugins/*/go.mod))
SERVICE_MODULES := $(filter services/%,$(MODULES))
PLUGIN_MODULES  := $(filter plugins/%,$(MODULES))
SERVICE_BINS    := $(notdir $(SERVICE_MODULES))
PLUGIN_BINS     := $(notdir $(PLUGIN_MODULES))

# Mutation testing output directory
MUTATE_DIR := .mutate

# Suppress clang deployment-version warnings from CGO dependencies
export MACOSX_DEPLOYMENT_TARGET ?= $(shell sw_vers -productVersion 2>/dev/null)
export CGO_CFLAGS ?= -Wno-overriding-deployment-version

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
PROTO_SOURCES := $(wildcard proto/bossanova/v1/*.proto) buf.gen.yaml
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

## deps: Install required build/dev tools via Homebrew (macOS)
deps:
	@if ! command -v brew >/dev/null 2>&1; then \
		echo "Homebrew is required. Install from https://brew.sh/"; \
		exit 1; \
	fi
	@echo "==> Installing build dependencies via Homebrew"
	@for pkg in go buf jq gh pnpm; do \
		if command -v $$pkg >/dev/null 2>&1; then \
			echo "    $$pkg: already installed"; \
		else \
			echo "    $$pkg: installing..."; \
			brew install $$pkg; \
		fi; \
	done
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
	@if command -v protoc-gen-go >/dev/null 2>&1; then \
		echo "    protoc-gen-go: already installed"; \
	else \
		echo "    protoc-gen-go: installing..."; \
		go install google.golang.org/protobuf/cmd/protoc-gen-go@latest; \
	fi
	@if command -v protoc-gen-connect-go >/dev/null 2>&1; then \
		echo "    protoc-gen-connect-go: already installed"; \
	else \
		echo "    protoc-gen-connect-go: installing..."; \
		go install connectrpc.com/connect/cmd/protoc-gen-connect-go@latest; \
	fi
	@gobin=$$(go env GOBIN); [ -z "$$gobin" ] && gobin=$$(go env GOPATH)/bin; \
	case ":$$PATH:" in *":$$gobin:"*) ;; \
		*) echo ""; echo "NOTE: $$gobin is not on your PATH — add it so buf can find protoc-gen-go."; \
		echo "      fish: fish_add_path $$gobin"; \
		echo "      bash/zsh: export PATH=\"$$gobin:\$$PATH\"";; \
	esac
	@echo "==> Done. Run 'make' to build."

## setup-worktree: Copy .env from the main repo into a new worktree (for bossanova setup-script)
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
	@if [ -f "$$REPO_DIR/.node-version" ]; then \
		cp "$$REPO_DIR/.node-version" "$$WORKTREE_DIR/.node-version"; \
		echo "Copied .node-version into $$WORKTREE_DIR"; \
	else \
		echo "No .node-version in $$REPO_DIR — skipping"; \
	fi
	direnv allow

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

## generate: Run buf generate to produce Go code from proto definitions
generate: $(GEN_STAMP)

# Make web deps and buf conditional — public repo has committed gen code and no web/
GEN_DEPS := $(PROTO_SOURCES)
ifneq ($(wildcard services/web/package.json),)
GEN_DEPS += $(WEB_DEPS_STAMP)
endif

$(GEN_STAMP): $(GEN_DEPS)
ifneq ($(shell command -v buf 2>/dev/null),)
	rm -rf lib/bossalib/gen
	buf generate
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

ifneq ($(wildcard services/bosso/go.mod),)
$(BIN_DIR)/bosso: $(GEN_STAMP)
	go build -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/bosso ./services/bosso/cmd
endif

## plugins: Build all plugin binaries
plugins: $(addprefix $(BIN_DIR)/,$(PLUGIN_BINS))

# Pattern rule for plugin binaries
$(BIN_DIR)/bossd-plugin-%: $(GEN_STAMP)
	go build -ldflags '$(LDFLAGS)' -o $@ ./plugins/bossd-plugin-$*

# bossd-plugin-claude embeds the boss skill payload; refresh the mirror before
# linking so the plugin binary and the boss CLI ship identical bytes.
$(BIN_DIR)/bossd-plugin-claude: copy-skills

## test: Run tests across all modules (generates protos first if needed)
test: $(GEN_STAMP) copy-skills
	@for mod in $(MODULES); do \
		echo "==> Testing $$mod"; \
		$(MAKE) -C $$mod test; \
	done
	@if [ -d services/docs ]; then \
		echo "==> Testing services/docs"; \
		$(MAKE) -C services/docs test; \
	fi

## test-race: Run the full test suite under -race (sets RACE=1 for sub-makes).
## Race detector is opt-in: `make test` skips it; `make test-race` or `RACE=1 make test` enables it.
test-race:
	@$(MAKE) test RACE=1

## Per-module test targets (no generate dep — CI uses committed gen code)
test-bossalib:
	$(MAKE) -C lib/bossalib test

test-boss:
	$(MAKE) -C services/boss test

test-bossd:
	$(MAKE) -C services/bossd test

## test-integration-bossd: Run bossd integration tests (requires tmux on PATH; gated by 'integration' build tag)
test-integration-bossd:
	cd services/bossd && go test -tags=integration -race ./internal/server/... -count=1

ifneq ($(wildcard services/bosso/go.mod),)
test-bosso:
	$(MAKE) -C services/bosso test
endif

# Auto-generate per-plugin test targets from detected modules
define define-plugin-test
test-$(2):
	$$(MAKE) -C $(1) test
endef
$(foreach p,$(PLUGIN_MODULES),$(eval \
  $(call define-plugin-test,$(p),$(patsubst bossd-plugin-%,%,$(notdir $(p))))))

# Plugin claude tests rely on the embedded skill payload — keep it in sync.
test-claude: copy-skills

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

## lint: Run golangci-lint and buf lint (generates protos first if needed)
lint: lint-check-version $(GEN_STAMP)
	buf lint
	@for mod in $(MODULES); do \
		echo "==> Linting $$mod"; \
		(cd $$mod && golangci-lint run ./...); \
	done
	@if [ -d services/docs ]; then \
		echo "==> Linting services/docs"; \
		$(MAKE) -C services/docs lint; \
	fi

## Per-module lint targets
lint-proto:
	buf lint

lint-bossalib: lint-check-version
	cd lib/bossalib && golangci-lint run ./...

lint-boss: lint-check-version
	cd services/boss && golangci-lint run ./...

lint-bossd: lint-check-version
	cd services/bossd && golangci-lint run ./...

## Per-module docs targets
test-docs:
	$(MAKE) -C services/docs test

lint-docs:
	$(MAKE) -C services/docs lint

build-docs:
	$(MAKE) -C services/docs build

ifneq ($(wildcard services/bosso/go.mod),)
lint-bosso: lint-check-version
	cd services/bosso && golangci-lint run ./...
endif

# Auto-generate per-plugin lint targets from detected modules
define define-plugin-lint
lint-$(2): lint-check-version
	cd $(1) && golangci-lint run ./...
endef
$(foreach p,$(PLUGIN_MODULES),$(eval \
  $(call define-plugin-lint,$(p),$(patsubst bossd-plugin-%,%,$(notdir $(p))))))

## Per-module build targets (no generate dep — CI uses committed gen code)
build-boss:
	go build -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/boss ./services/boss/cmd
	@if [ "$$(uname)" = "Darwin" ]; then codesign -s "$(CODESIGN_IDENTITY)" --force $(BIN_DIR)/boss; fi

build-bossd:
	go build -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/bossd ./services/bossd/cmd
	@if [ "$$(uname)" = "Darwin" ]; then codesign -s "$(CODESIGN_IDENTITY)" --force $(BIN_DIR)/bossd; fi

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

## format: Format Go code (gofmt + golangci-lint), web code, package.json files, and markdown
format: lint-check-version
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
	@if command -v pnpm >/dev/null 2>&1 && [ -f package.json ]; then \
		pnpm run format:docs; \
	fi

## build-all: Cross-platform builds for distribution (generates protos first if needed)
PLATFORMS := darwin/amd64 darwin/arm64 linux/amd64
# Only boss and bossd are distributed (bosso is deployed to Fly.io directly)
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
