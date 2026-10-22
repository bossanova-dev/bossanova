.PHONY: check-race-budget coverage-boss coverage-bossalib coverage-bossd coverage-bosso coverage-mcp coverage-mcp-gateway test test-affected test-all test-docs test-full test-golangci-rules test-legacy-refs test-manifest test-mcp test-native-ledger test-native-ledger-affected test-no-inline-stop-hooks test-no-vacuous-regions test-profile test-public-mirror test-race test-readme test-scripts test-smoke test-web test-web-e2e

## test-native-ledger: run ledger rows whose disposition is default-run (today-parity).
## These are the bazel-`manual` targets today's `make test` still had to cover
## natively; run after `bazel test //...`. RACE=1 injects -race into each command.
test-native-ledger:
	@node scripts/bazel/run-ledger.mjs --disposition default-run $(if $(filter 1,$(RACE)),--race,)

## test-native-ledger-affected: like test-native-ledger, but only the rows whose
## module the current diff (BASE_REF...HEAD, default origin/main) actually touches.
## These rows are native `go test` — no bazel cache, no sandbox — so unlike the
## bazel graph they cost full wall time on every run regardless of the diff. The
## release tier still runs the unfiltered `test-native-ledger`, so an
## under-selection here can never reach a release. A selector failure prints
## nothing, which the `ALL` default below turns into the full (safe) run.
test-native-ledger-affected:
	@MODULES="$$(node scripts/select-affected-tests.mjs --ledger-modules || true)"; \
	[ -n "$$MODULES" ] || MODULES=ALL; \
	if [ "$$MODULES" = NONE ]; then \
		echo "run-ledger: no ledger module affected by this diff — nothing to run"; \
	elif [ "$$MODULES" = ALL ]; then \
		node scripts/bazel/run-ledger.mjs --disposition default-run $(if $(filter 1,$(RACE)),--race,); \
	else \
		ARGS=""; for m in $$MODULES; do ARGS="$$ARGS --module $$m"; done; \
		node scripts/bazel/run-ledger.mjs --disposition default-run $$ARGS $(if $(filter 1,$(RACE)),--race,); \
	fi

## test: Fast affected test (default) — runs only the tests selected for the files
## changed on this branch (test-affected; falls back to test-smoke when nothing maps).
## Fast is the default (BOS-371); the exhaustive whole-graph suite is `make test-all`,
## which CI/release runs.
test: test-affected

## test-all: Run tests across all modules (generates protos first if needed). Was
## `make test`. Delegates the Go module loop to `bazel test //...` (cached) + the
## native ledger step; falls back to the legacy per-module loop when bazel is
## unavailable (BOSS_NO_BAZEL set or not on PATH — keeps the public mirror green).
## NOT in this suite: the DB-backed legs `make test-warehouse` and
## `make test-bosso-postgres` (Docker Postgres, uv/dbt). They run from the affected
## selector (scripts/select-affected-tests.mjs routes services/warehouse/ and the
## bosso Postgres migrations to test-warehouse) and from CI (the path-filtered
## test-warehouse.yml job; bazel.yml's Postgres service for the bosso suites).
test-all: $(GEN_STAMP) codex-skills-check vendor-toolbox-check build-drift-check
	$(MAKE) test-scripts
	$(MAKE) test-readme
	$(MAKE) test-no-inline-stop-hooks
	$(MAKE) test-no-vacuous-regions
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
## command ladder / docs that name the "full" suite explicitly (commands.test). Bazel-
## and Go-cached as-is; BOSS_GATE_FORCE_UNCACHED=1 adds --nocache_test_results and
## GO_TEST_COUNT=1.
## Like test-all it does not run test-warehouse or test-bosso-postgres.
test-full:
	$(MAKE) test-all

ifneq ($(wildcard services/web/package.json),)
## test-web: Run web+marketing+web-helpers tests/lint/typecheck through turbo (cached).
## Machine-wide TURBO_CACHE_DIR (exported above) lets sibling worktrees share
## hits. Deliberately kept out of test-smoke — node startup + turbo hash cost.
test-web: $(WEB_DEPS_STAMP)
	pnpm turbo run test lint typecheck --filter=web --filter=marketing --filter=@bossanova/web-helpers
endif

ifneq ($(wildcard services/web/package.json),)
## test-web-e2e: Opt-in/release-tier web Playwright Tier-1 faked E2E suite.
## NOT part of the default test/test-web/test-smoke graph — it rebuilds the VITE_E2E
## bundle and needs chrome-headless-shell, so it stays opt-in and matches the release-only CI job.
test-web-e2e: $(WEB_DEPS_STAMP)
	@# Best-effort chrome-headless-shell install (mirrors setup-worktree) — never fatal so the
	@# target itself is correct even where the browser can't be fetched.
	( cd services/web && pnpm exec playwright install chromium-headless-shell ) || echo "playwright install chromium-headless-shell failed (non-fatal)";
	$(MAKE) -C services/web test-e2e
endif

## test-smoke: Fast agent loop. No coverage, no race, no forced cache bypass.
## Delegates to the cached bazel loop under `-test.short`; falls back to the native
## `-short` module loop when bazel is unavailable (public-mirror fallback).
test-smoke: $(GEN_STAMP) codex-skills-check vendor-toolbox-check
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

# test-affected: when the selector maps the change to commands, a branch that adds or
# renames a file (committed, staged or untracked) runs them uncached
# (BOSS_GATE_FORCE_UNCACHED=1 -> --nocache_test_results / GO_TEST_COUNT=1), using the gate
# cache's own predicate. Only the printed `adds-or-renames: no` verdict WITH its exit 1 keeps the
# cache: `yes`, `unknown`, a `no` under any other exit status, and a probe that crashed or printed
# nothing all force uncached. The smoke fallback, taken when nothing maps, is never forced (it
# would re-run the whole-repo -short suite).
test-affected:
	@commands="$$(node scripts/select-affected-tests.mjs)" || exit $$?; \
	if [ -z "$$commands" ]; then \
		echo "make test-smoke"; \
		node scripts/run-gate.mjs --label "make test-smoke" -- node scripts/gate-cache.mjs run --site "test-smoke" --command "make test-smoke" -- $(MAKE) test-smoke || exit $$?; \
	else \
		verdict="$$(node scripts/gate-cache.mjs adds-or-renames)" && probe_rc=0 || probe_rc=$$?; \
		echo "$${verdict:-adds-or-renames: unknown (the probe printed nothing; forcing uncached)}"; \
		case "$$probe_rc:$$verdict" in '1:adds-or-renames: no '*) ;; *) BOSS_GATE_FORCE_UNCACHED=1; export BOSS_GATE_FORCE_UNCACHED;; esac; \
		printf '%s\n' "$$commands" | while IFS= read -r command; do \
			[ -z "$$command" ] && continue; \
			echo "$$command"; \
			if { [ "$$command" = "make test-smoke" ] && ! grep -Eq '^test-smoke:' $(MAKEFILE_LIST); }; then \
				echo "Pending target fallback: make test-scripts"; \
				node scripts/run-gate.mjs --label "make test-scripts" -- node scripts/gate-cache.mjs run --site "test-scripts" --command "make test-scripts" -- $(MAKE) test-scripts || exit $$?; \
			else \
				node scripts/run-gate.mjs --label "$$command" -- sh -c "$$command" || exit $$?; \
			fi; \
		done; \
	fi

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
	@node scripts/gate-cache.mjs run --site test-bossalib --command '$(BAZEL) test $(BAZEL_TEST_FLAGS) $${BOSS_GATE_FORCE_UNCACHED:+--nocache_test_results} //lib/bossalib/... && node scripts/bazel/run-ledger.mjs --module lib/bossalib --disposition default-run $(if $(filter 1,$(RACE)),--race,)' -- sh -c '$(BAZEL) test $(BAZEL_TEST_FLAGS) $${BOSS_GATE_FORCE_UNCACHED:+--nocache_test_results} //lib/bossalib/... && node scripts/bazel/run-ledger.mjs --module lib/bossalib --disposition default-run $(if $(filter 1,$(RACE)),--race,)'
else
	@node scripts/gate-cache.mjs run --site test-bossalib --command '$(MAKE) -C lib/bossalib test-all' -- $(MAKE) -C lib/bossalib test-all
endif

test-boss:
ifeq ($(BAZEL_USABLE),1)
	@node scripts/gate-cache.mjs run --site test-boss --command '$(BAZEL) test $(BAZEL_TEST_FLAGS) $${BOSS_GATE_FORCE_UNCACHED:+--nocache_test_results} //services/boss/... && node scripts/bazel/run-ledger.mjs --module services/boss --disposition default-run $(if $(filter 1,$(RACE)),--race,)' -- sh -c '$(BAZEL) test $(BAZEL_TEST_FLAGS) $${BOSS_GATE_FORCE_UNCACHED:+--nocache_test_results} //services/boss/... && node scripts/bazel/run-ledger.mjs --module services/boss --disposition default-run $(if $(filter 1,$(RACE)),--race,)'
else
	@node scripts/gate-cache.mjs run --site test-boss --command '$(MAKE) -C services/boss test-all' -- $(MAKE) -C services/boss test-all
endif

test-bossd:
ifeq ($(BAZEL_USABLE),1)
	@node scripts/gate-cache.mjs run --site test-bossd --command '$(BAZEL) test $(BAZEL_TEST_FLAGS) $${BOSS_GATE_FORCE_UNCACHED:+--nocache_test_results} //services/bossd/... && node scripts/bazel/run-ledger.mjs --module services/bossd --disposition default-run $(if $(filter 1,$(RACE)),--race,)' -- sh -c '$(BAZEL) test $(BAZEL_TEST_FLAGS) $${BOSS_GATE_FORCE_UNCACHED:+--nocache_test_results} //services/bossd/... && node scripts/bazel/run-ledger.mjs --module services/bossd --disposition default-run $(if $(filter 1,$(RACE)),--race,)'
else
	@node scripts/gate-cache.mjs run --site test-bossd --command '$(MAKE) -C services/bossd test-all' -- $(MAKE) -C services/bossd test-all
endif

test-mcp:
ifeq ($(BAZEL_USABLE),1)
	@node scripts/gate-cache.mjs run --site test-mcp --command '$(BAZEL) test $(BAZEL_TEST_FLAGS) $${BOSS_GATE_FORCE_UNCACHED:+--nocache_test_results} //services/mcp/... && node scripts/bazel/run-ledger.mjs --module services/mcp --disposition default-run $(if $(filter 1,$(RACE)),--race,)' -- sh -c '$(BAZEL) test $(BAZEL_TEST_FLAGS) $${BOSS_GATE_FORCE_UNCACHED:+--nocache_test_results} //services/mcp/... && node scripts/bazel/run-ledger.mjs --module services/mcp --disposition default-run $(if $(filter 1,$(RACE)),--race,)'
else
	@node scripts/gate-cache.mjs run --site test-mcp --command '$(MAKE) -C services/mcp test-all' -- $(MAKE) -C services/mcp test-all
endif

ifneq ($(wildcard services/mcp-gateway/go.mod),)
test-mcp-gateway:
ifeq ($(BAZEL_USABLE),1)
	@node scripts/gate-cache.mjs run --site test-mcp-gateway --command '$(BAZEL) test $(BAZEL_TEST_FLAGS) $${BOSS_GATE_FORCE_UNCACHED:+--nocache_test_results} //services/mcp-gateway/... && node scripts/bazel/run-ledger.mjs --module services/mcp-gateway --disposition default-run $(if $(filter 1,$(RACE)),--race,)' -- sh -c '$(BAZEL) test $(BAZEL_TEST_FLAGS) $${BOSS_GATE_FORCE_UNCACHED:+--nocache_test_results} //services/mcp-gateway/... && node scripts/bazel/run-ledger.mjs --module services/mcp-gateway --disposition default-run $(if $(filter 1,$(RACE)),--race,)'
else
	@node scripts/gate-cache.mjs run --site test-mcp-gateway --command '$(MAKE) -C services/mcp-gateway test-all' -- $(MAKE) -C services/mcp-gateway test-all
endif
endif

# Auto-generate per-plugin test targets from detected modules. Same bazel-facade
# pattern as the service modules; plugins have no ledger rows so run-ledger is a
# no-op for them (kept uniform). Double-$ so the vars survive $(call)/$(eval).
define define-plugin-test
test-$(2):
ifeq ($$(BAZEL_USABLE),1)
	@node scripts/gate-cache.mjs run --site test-$(2) --command '$$(BAZEL) test $$(BAZEL_TEST_FLAGS) $$$${BOSS_GATE_FORCE_UNCACHED:+--nocache_test_results} //$(1)/... && node scripts/bazel/run-ledger.mjs --module $(1) --disposition default-run $$(if $$(filter 1,$$(RACE)),--race,)' -- sh -c '$$(BAZEL) test $$(BAZEL_TEST_FLAGS) $$$${BOSS_GATE_FORCE_UNCACHED:+--nocache_test_results} //$(1)/... && node scripts/bazel/run-ledger.mjs --module $(1) --disposition default-run $$(if $$(filter 1,$$(RACE)),--race,)'
else
	@node scripts/gate-cache.mjs run --site test-$(2) --command '$$(MAKE) -C $(1) test-all' -- $$(MAKE) -C $(1) test-all
endif
endef
$(foreach p,$(PLUGIN_MODULES),$(eval \
  $(call define-plugin-test,$(p),$(patsubst bossd-plugin-%,%,$(notdir $(p))))))

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

test-readme:
	node scripts/check-readme-assets.mjs

test-public-mirror:
	node scripts/check-public-mirror-workflows.mjs

test-no-inline-stop-hooks:
	node scripts/check-no-inline-stop-hooks.mjs

## test-no-vacuous-regions: BOS-737 — fail if a raw `X.slice(X.indexOf(…))` region
## extraction lands. `indexOf` returns -1 on a moved marker and `slice` reads -1 as
## "one char from the end", so every negative assertion over the region passes while
## the forbidden content sits in the file. Use region() from scripts/gate-region-lib.mjs.
test-no-vacuous-regions:
	node scripts/check-vacuous-regions.mjs

## test-legacy-refs: BOS-815 tree-wide retirement scan — fail if any ACTIVE file
## reintroduces a reference to either retired system (the scan names them; this
## comment deliberately does not, because the scan reads this file too). Its input is
## the WHOLE `git ls-files` tree, so no path rule can predict which change reintroduces
## one: an ordinary Go production file is as capable of it as a scripts/** file.
## `make test-scripts` (and so `make test-all`) already runs this as one of the
## scripts/*.test.mjs files, but that target is reached only behind the
## test-scripts.yml path filter, which omits ordinary production paths. This
## standalone, dependency-free (~0.5s, node built-ins only) target exists so an
## UNCONDITIONALLY triggered CI job — bazel.yml `go-lint`, which carries no path
## filter and no event `if:` — can run the scan on every push regardless of what
## changed. Do not give this target prerequisites; cheapness is what makes the
## unconditional wiring affordable.
test-legacy-refs:
	node --test scripts/legacy-support-refs.test.mjs

## test-golangci-rules: prove the depguard `plugins-no-host-imports` and forbidigo bare
## `exec.Command` rules in .golangci.yml fire, by linting a hermetic fixture module with the
## pinned golangci-lint. Needs golangci-lint on PATH (`make deps`); bazel.yml `go-lint` runs it.
test-golangci-rules:
	node --test --test-reporter=tap scripts/guards/golangci/*.test.mjs

## test-manifest: Fail if agent test guidance manifest is stale.
test-manifest:
	node scripts/check-test-command-manifest.mjs

test-docs:
	$(MAKE) -C services/docs test

test-scripts:
	node scripts/check-test-tmp-leaks.mjs -- $(MAKE) -C scripts test

## check-race-budget: score the per-package `-race` wall-clock budgets in
## scripts/race-budgets.json (BOS-1022's ratchet against per-test migration
## replay creeping back).
##
## CI runs this on the RELEASE tier only, inside bazel-linux-smoke.yml's race leg,
## where a full race pass is already paid for. This target exists so the same gate
## is reproducible locally when a budget needs re-measuring — it is deliberately
## NOT wired into `make lint` or `make test`, which are the fast affected loop.
##
## Durations come from the Build Event Protocol stream, not from JUnit test.xml:
## rules_go writes an empty <testsuites></testsuites> for a PASSING target, so a
## test.xml-based gate can never score a green run. --nocache_test_results is what
## makes the numbers this run's own; the checker refuses any shard the BEP reports
## as cache-served, so a cached pass fails loudly instead of reading as fast.
check-race-budget:
	@targets="$$(node -e 'process.stdout.write(require("./scripts/race-budgets.json").targets.map(t=>t.label).join(" "))')"; \
	bep="$$(mktemp -t race-budget-bep)"; \
	trap 'rm -f "$$bep"' EXIT; \
	bazel test --config=race $$targets \
		--nocache_test_results \
		--build_event_json_file="$$bep" \
		--test_output=errors || exit $$?; \
	node scripts/check-race-budget.mjs --bep "$$bep"
