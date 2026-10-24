.PHONY: apiversion-check lint lint-all lint-docs lint-go-fmt lint-mcp lint-proto-breaking lint-scripts post-rebase-check

## lint: Fast affected lint (default) — buf lint + cached golangci (lint-affected,
## content-stamp-skipped) + the standalone gofmt/goimports gate + affected web Biome
## checks + docs lint, with the markdown format check scoped to CHANGED files
## (lint-docs-affected). Fast is the
## default (BOS-371); the exhaustive whole-tree markdown pass is `make lint-all`.
lint: buf-check-version lint-check-version $(GEN_STAMP)
	node scripts/test-infra/run-gate.mjs --label "buf lint" -- "$(BUF)" lint
	node scripts/test-infra/run-gate.mjs --label "make lint-scripts" -- $(MAKE) lint-scripts
	@node scripts/test-infra/run-gate.mjs --label "lint-affected" -- node scripts/lint/lint-affected.mjs
	node scripts/test-infra/run-gate.mjs --label "make lint-go-fmt" -- $(MAKE) lint-go-fmt
	@if command -v pnpm >/dev/null 2>&1 && [ -f package.json ]; then \
		node scripts/test-infra/run-gate.mjs --label "lint-web-affected" -- node scripts/lint/lint-web-affected.mjs; \
	fi
	@if [ -d services/docs ]; then \
		echo "==> Linting services/docs"; \
		node scripts/test-infra/run-gate.mjs --label "services/docs lint" -- $(MAKE) -C services/docs lint; \
	fi
	@if command -v pnpm >/dev/null 2>&1 && [ -f package.json ]; then \
		echo "==> Checking changed docs markdown formatting"; \
		node scripts/test-infra/run-gate.mjs --label "lint-docs-affected" -- node scripts/lint/lint-docs-affected.mjs; \
	fi

## lint-all: Exhaustive lint — the same buf/golangci/gofmt/docs gates as `make lint`,
## but with the whole-tree `prettier --check` over ALL markdown (was `make lint`).
## CI/release gate.
lint-all: buf-check-version lint-check-version $(GEN_STAMP)
	"$(BUF)" lint
	$(MAKE) lint-scripts
	@node scripts/lint/lint-affected.mjs
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
	@node scripts/guards/check-go-format.mjs

## Per-module lint targets
lint-proto: buf-check-version
	"$(BUF)" lint

BUF_BREAKING_BASE ?= .git\#branch=origin/main

## lint-proto-breaking: Check proto wire compatibility against the base branch.
lint-proto-breaking: buf-check-version
	"$(BUF)" breaking --against '$(BUF_BREAKING_BASE)' --against-config buf.yaml

## apiversion-check: Reconcile this branch's API-version contracts against origin/main and
## origin/production, then run the apiversion ledger/registry/OpenAPI/web/changelog sync tests.
# --fetch is deliberate and has no offline bypass: a stale local origin/production is exactly the
# bug this gate exists to catch. Offline it exits 2 (unevaluated) and says why.
apiversion-check:
	node scripts/release/apiversion-ledger.mjs check --fetch
	cd lib/bossalib && go test -count=1 -run 'Released|DefaultRegistry|OpenAPI|WebAPIVersion|Changelog|ProductionChanges' ./apiversion/

## post-rebase-check: Re-run deterministic checks and re-measure pinned values after a rebase.
post-rebase-check: test-manifest lint-proto lint-proto-breaking apiversion-check

lint-bossalib: lint-check-version
	node scripts/lint/lint-affected.mjs --module lib/bossalib

lint-boss: lint-check-version
	node scripts/lint/lint-affected.mjs --module services/boss

lint-bossd: lint-check-version
	node scripts/lint/lint-affected.mjs --module services/bossd

lint-mcp: lint-check-version
	node scripts/lint/lint-affected.mjs --module services/mcp

ifneq ($(wildcard services/mcp-gateway/go.mod),)
lint-mcp-gateway: lint-check-version
	node scripts/lint/lint-affected.mjs --module services/mcp-gateway
endif

lint-docs:
	$(MAKE) -C services/docs lint

lint-scripts:
	$(MAKE) -C scripts lint

ifneq ($(wildcard services/bosso/go.mod),)
lint-bosso: lint-check-version
	node scripts/lint/lint-affected.mjs --module services/bosso
endif

# Auto-generate per-plugin lint targets from detected modules
define define-plugin-lint
lint-$(2): lint-check-version
	node scripts/lint/lint-affected.mjs --module $(1)
endef
$(foreach p,$(PLUGIN_MODULES),$(eval \
  $(call define-plugin-lint,$(p),$(patsubst bossd-plugin-%,%,$(notdir $(p))))))
