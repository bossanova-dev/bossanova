.PHONY: buf-check-version deps deps-golangci lint-check-version setup-worktree tidy tidy-check

tidy:
	bash .github/scripts/tidy-drift-all.sh --fix

## tidy-check: Check every workspace module for go.mod/go.sum drift without modifying the tree.
tidy-check:
	bash .github/scripts/tidy-drift-all.sh

# Mutation testing output directory
## deps: Install required build/dev tools via Homebrew (macOS)
deps:
	@if ! command -v brew >/dev/null 2>&1; then \
		echo "Homebrew is required. Install from https://brew.sh/"; \
		exit 1; \
	fi
	@echo "==> Installing build dependencies via Homebrew"
	@# buf is intentionally NOT installed via brew here — brew can't pin to an
	@# exact version, and the pin ($(BUF_VERSION)) is enforced by
	@# buf-check-version. buf is cached from a GitHub release below instead.
	@# bazelisk provides the `bazel` launcher the test facade delegates to (BOS-339);
	@# `command -v bazelisk` is the right presence check (brew installs it as bazelisk).
	@for pkg in go jq gh pnpm bazelisk vale; do \
		if command -v $$pkg >/dev/null 2>&1; then \
			echo "    $$pkg: already installed"; \
		else \
			echo "    $$pkg: installing..."; \
			brew install $$pkg; \
		fi; \
	done
	@# Best-effort pinned cache provisioning; never replace a host buf.
	@sh scripts/ensure-buf.sh install "$(BUF_VERSION)" "$(BUF)" \
		|| echo "warning: pinned buf provisioning failed (non-fatal)"
	@# golangci-lint: enforce pinned version so local runs match CI.
	@gobin=$$(go env GOBIN); [ -z "$$gobin" ] && gobin=$$(go env GOPATH)/bin; \
	want="$(GOLANGCI_LINT_VERSION)"; \
	if command -v golangci-lint >/dev/null 2>&1 && \
	   golangci-lint --version 2>/dev/null | grep -Eq "version (v)?$${want#v}( |$$)"; then \
		echo "    golangci-lint: $$want already installed"; \
	else \
		echo "    golangci-lint: installing $$want..."; \
		want="$$want" gobin="$$gobin" scripts/retry.sh 3 5 sh -c 'set -e; tmp=$$(mktemp); trap "rm -f $$tmp" EXIT; curl -sSfL -o "$$tmp" "https://raw.githubusercontent.com/golangci/golangci-lint/$$want/install.sh"; sh "$$tmp" -b "$$gobin" "$$want"'; \
	fi
	@if command -v gremlins >/dev/null 2>&1; then \
		echo "    gremlins: already installed"; \
	else \
		echo "    gremlins: installing..."; \
		brew install go-gremlins/tap/gremlins; \
	fi
	@echo "==> Warming technical-debt scanners (make debt-*)"
	@for pkg in "$(DEADCODE_PKG)" "$(DUPL_PKG)" "$(GOCYCLO_PKG)" "$(GOVULNCHECK_PKG)" "$(REVIVE_PKG)"; do \
		echo "    $$pkg"; \
		go install "$$pkg" || echo "    (warning: failed to install $$pkg; make debt-* will fetch it on demand)"; \
	done
	@echo "==> Done. Run 'make build' to build binaries ('make' is now a fast affected lint+test check; 'make all-full' is the full rebuild)."

# Keys from the checkout's .env that setup-worktree writes into an agent worktree's .env
# (scripts/write-worktree-env.mjs). Everything else stays in the checkout: an agent session
# inherits the worktree .env, so only keys agents actually need belong here. Dev-daemon keys
# (BOSS_SETTINGS_PATH, BOSS_KEYRING_BACKEND, BOSS_WEB_PORT, BOSS_CLOUD_URL,
# BOSSD_ORCHESTRATOR_URL) are deliberately absent (BOS-1512). Database URLs are
# absent too: each worktree provisions its own test server unless explicitly exported.
WORKTREE_ENV_KEYS := LINEAR_API_KEY,CLOUDFLARE_API_TOKEN,CLOUDFLARE_ACCOUNT_ID,BOSS_PROOF_R2_BUCKET,BOSS_PROOF_PUBLIC_BASE_URL,BOSS_PROOF_HW_ENCODE,CC

## setup-worktree: Write the WORKTREE_ENV_KEYS subset of .env and copy gitignored local files (.node-version, .bazelrc.user, .config excluding .config/accounts, .compound-engineering/config.local.yaml) from the main repo into a new worktree (for bossanova setup-script)
setup-worktree:
	@if [ -z "$$REPO_DIR" ] || [ -z "$$WORKTREE_DIR" ]; then \
		echo "setup-worktree must be invoked by bossanova (REPO_DIR and WORKTREE_DIR required)"; \
		exit 1; \
	fi
	@copy_status=0; \
		node "$$WORKTREE_DIR/scripts/setup-worktree-copy.mjs" --repo-dir "$$REPO_DIR" --worktree-dir "$$WORKTREE_DIR" --env-keys "$(WORKTREE_ENV_KEYS)" || copy_status=$$?; \
		$(MAKE) --no-print-directory -f "$(firstword $(MAKEFILE_LIST))" setup-worktree-provision; \
		exit $$copy_status

.PHONY: setup-worktree-provision
setup-worktree-provision:
	@sh "$$WORKTREE_DIR/scripts/ensure-buf.sh" install "$(BUF_VERSION)" "$(BUF)" \
		|| echo "warning: pinned buf provisioning failed (non-fatal)"
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
	@# services/docs is deliberately OUTSIDE the pnpm workspace (adding docusaurus
	@# pulled terser into the shared peer graph and broke the marketing vite build),
	@# so it carries its own pnpm-lock.yaml and the root install above does NOT
	@# provision it. Without this step a fresh worktree fails `make lint` / `make
	@# test` at "==> Linting services/docs" with `sh: tsc: command not found` even
	@# though the diff is Go-only. Delegate to the docs package's own `deps` target
	@# so the standalone-install knowledge stays single-sourced there. Best-effort,
	@# mirroring the root install: a docs dep failure must not abort worktree setup.
	@if command -v pnpm >/dev/null 2>&1 && [ -d "$$WORKTREE_DIR/services/docs" ]; then \
		echo "==> Installing services/docs deps (standalone, outside the workspace)"; \
		( cd "$$WORKTREE_DIR" && $(MAKE) -C services/docs deps ) \
			|| echo "services/docs deps failed — make lint/test will fail there (non-fatal)"; \
	else \
		echo "pnpm or services/docs missing — skipping docs dep install (non-fatal)"; \
	fi
	@# Install the Playwright headless Chromium shell the verify skill's control CLI
	@# (.claude/skills/verify) drives. Best effort: guarded on pnpm + web node_modules and
	@# NEVER fatal — a worktree that can't fetch Chromium still provisions; the control
	@# CLI's doctor reports the gap at run time. Host-level tools the evidence video path
	@# needs are NOT installable here: run `brew install agg ffmpeg` (macOS) / your distro
	@# equivalent on the host once; doctor reports their absence.
	@if command -v pnpm >/dev/null 2>&1 && [ -d "$$WORKTREE_DIR/services/web/node_modules" ]; then \
		echo "==> Installing Playwright chrome-headless-shell (verify, best-effort)"; \
		( cd "$$WORKTREE_DIR/services/web" && pnpm exec playwright install chromium-headless-shell ) \
			|| echo "playwright install chromium-headless-shell failed — verify browser capture unavailable (non-fatal)"; \
	else \
		echo "pnpm or services/web/node_modules missing — skipping Playwright chrome-headless-shell install (non-fatal)"; \
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

## buf-check-version: Provision absent pinned buf and reject wrong-version overrides.
buf-check-version:
	@sh scripts/ensure-buf.sh check "$(BUF_VERSION)" "$(BUF)"; result=$$?; \
	case $$result in \
		0) ;; \
		3) sh scripts/ensure-buf.sh install "$(BUF_VERSION)" "$(BUF)" || exit $$? ;; \
		*) exit $$result ;; \
	esac
	@sh scripts/ensure-buf.sh check "$(BUF_VERSION)" "$(BUF)"

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
		want="$$want" gobin="$$gobin" scripts/retry.sh 3 5 sh -c 'set -e; tmp=$$(mktemp); trap "rm -f $$tmp" EXIT; curl -sSfL -o "$$tmp" "https://raw.githubusercontent.com/golangci/golangci-lint/$$want/install.sh"; sh "$$tmp" -b "$$gobin" "$$want"'; \
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
