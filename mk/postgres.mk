.PHONY: bootstrap-posthog-raw deploy-warehouse-flow grant-warehouse-reads postgres-test-down postgres-test-up test-bosso-postgres test-bosso-scale test-hermes-plugin test-warehouse

ifneq ($(wildcard services/bosso/go.mod),)

# BOS-1083: say out loud, at point of use, that a bare `make test-bosso` no
# longer covers the database suites. Before this ticket they ran against an
# in-memory SQLite database and needed no setup, so this target really did cover
# services/bosso/internal/db; now they need a Postgres server and t.Skip without
# one -- and a skip is indistinguishable from a pass in the target's own output.
# The narrowing is invisible precisely where someone would rely on it: locally,
# on a developer machine with no server running.
#
# `origin` is the right test rather than the value: test-bosso-postgres passes
# BOSSO_REQUIRE_POSTGRES_TESTS=1 as a sub-make override (origin `command line`)
# and CI exports it (origin `environment`), so `undefined` isolates exactly the
# bare invocation. The notice goes to stderr and the recipe is otherwise
# unchanged -- CI is already fail-closed on this (bazel.yml and
# bazel-linux-smoke.yml both set the variable, and .bazelrc forwards it), so
# this is a local-visibility fix, not a gate.
ifeq ($(origin BOSSO_REQUIRE_POSTGRES_TESTS),undefined)
BOSSO_PG_SKIP_NOTICE := printf '%s\n' \
	'test-bosso: NOTE - the Postgres-backed suites will SKIP, not run.' \
	'  BOSSO_REQUIRE_POSTGRES_TESTS is unset, and since BOS-1083 deleted the SQLite path those' \
	'  suites need a real server. Run `make test-bosso-postgres` (starts a throwaway container)' \
	'  for the full suite. CI always sets it, so this narrowing is local only.' >&2
else
BOSSO_PG_SKIP_NOTICE := :
endif

test-bosso:
	@$(BOSSO_PG_SKIP_NOTICE)
ifeq ($(BAZEL_USABLE),1)
	@node scripts/gate-cache.mjs run --site test-bosso --command '$(BAZEL) test $(BAZEL_TEST_FLAGS) $${BOSS_GATE_FORCE_UNCACHED:+--nocache_test_results} //services/bosso/... && node scripts/bazel/run-ledger.mjs --module services/bosso --disposition default-run $(if $(filter 1,$(RACE)),--race,)' -- sh -c '$(BAZEL) test $(BAZEL_TEST_FLAGS) $${BOSS_GATE_FORCE_UNCACHED:+--nocache_test_results} //services/bosso/... && node scripts/bazel/run-ledger.mjs --module services/bosso --disposition default-run $(if $(filter 1,$(RACE)),--race,)'
else
	@node scripts/gate-cache.mjs run --site test-bosso --command '$(MAKE) -C services/bosso test-all' -- $(MAKE) -C services/bosso test-all
endif

# A worktree owns its test container and volume. Supplied URLs remain external:
# they are probed but never started, recreated or removed by this helper.
BOSSO_TEST_PG_WAIT_SECONDS ?= 60
TEST_POSTGRES = node scripts/test-postgres.mjs
TEST_POSTGRES_OPTIONS = $(if $(BOSSO_TEST_PG_CONTAINER),--name '$(BOSSO_TEST_PG_CONTAINER)',) $(if $(BOSSO_TEST_PG_PORT),--port '$(BOSSO_TEST_PG_PORT)',) --wait-seconds '$(BOSSO_TEST_PG_WAIT_SECONDS)'

# Undefined means no server was supplied. Recipe-time capture keeps the selected
# port out of make's parse phase and passes it into the gate-cache fingerprint.
ifeq ($(filter undefined file,$(origin BOSSO_TEST_DATABASE_URL)),)
BOSSO_TEST_PG_URL = $(BOSSO_TEST_DATABASE_URL)
else
BOSSO_TEST_PG_URL =
endif
ifeq ($(filter undefined file,$(origin WAREHOUSE_TEST_ADMIN_URL)),)
WAREHOUSE_TEST_PG_URL = $(WAREHOUSE_TEST_ADMIN_URL)
else
WAREHOUSE_TEST_PG_URL =
endif

# Shared recipe pieces preserve the test status unless the post-run probe proves
# the database broke. Silent recipes keep supplied credentials out of make output.
define bosso-test-postgres-url
url='$(BOSSO_TEST_PG_URL)'; \
if [ -z "$$url" ]; then url=$$($(TEST_POSTGRES) ensure $(TEST_POSTGRES_OPTIONS) --print bosso-url) || exit $$?; \
else $(TEST_POSTGRES) check --url "$$url" || exit $$?; fi
endef

define test-postgres-result
status=$$?; \
if [ $$status -ne 0 ]; then $(TEST_POSTGRES) check --url "$$url" || exit $$?; fi; \
exit $$status
endef

## test-bosso-postgres: Run the bosso suite against healthy Postgres, provisioning
## this worktree's server unless BOSSO_TEST_DATABASE_URL is supplied.
## Sub-make overrides keep required Postgres runs distinct in the gate cache.
test-bosso-postgres:
	@$(bosso-test-postgres-url); \
	$(MAKE) test-bosso BOSSO_TEST_DATABASE_URL="$$url" BOSSO_REQUIRE_POSTGRES_TESTS=1; \
	$(test-postgres-result)

## test-bosso-scale: Run the complete orchestrator scale smoke against this
## worktree's Postgres, or pre-flight the supplied BOSSO_TEST_DATABASE_URL.
test-bosso-scale:
	@$(bosso-test-postgres-url); \
	(cd services/bosso && BOSSO_TEST_DATABASE_URL="$$url" BOSSO_REQUIRE_POSTGRES_TESTS=1 go test ./internal/loadtest -count=1); \
	$(test-postgres-result)

## postgres-test-up: Ensure this worktree's healthy loopback Postgres; print exports
## for eval "$$(make -s postgres-test-up)". Optional name/port overrides are respected.
postgres-test-up:
	@$(TEST_POSTGRES) ensure $(TEST_POSTGRES_OPTIONS)

## postgres-test-down: Remove only this worktree's labelled container and volume.
postgres-test-down:
	@$(TEST_POSTGRES) down $(TEST_POSTGRES_OPTIONS)

## test-warehouse: Run migrations, grants, denial probes and dbt against healthy
## Postgres. Provision this worktree's server unless WAREHOUSE_TEST_ADMIN_URL is supplied.
test-warehouse:
	@url='$(WAREHOUSE_TEST_PG_URL)'; \
	if [ -z "$$url" ]; then url=$$($(TEST_POSTGRES) ensure $(TEST_POSTGRES_OPTIONS) --print admin-url) || exit $$?; \
	else $(TEST_POSTGRES) check --url "$$url" || exit $$?; fi; \
	WAREHOUSE_TEST_ADMIN_URL="$$url" bash services/warehouse/scripts/test.sh; \
	$(test-postgres-result)

## test-hermes-plugin: Run the Hermes plugin's standard-library unittest suite
## (services/boss/internal/hermes/plugin/tests) against a fake stdio MCP server:
## registration, tool round trips, error mapping, restart after a crash, timeouts
## and config precedence. Needs python3 >= 3.10 and nothing else. The Go renderer
## that embeds the plugin is tested by test-boss.
test-hermes-plugin:
	PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s services/boss/internal/hermes/plugin/tests -v

## grant-warehouse-reads: Owner step. Apply the warehouse role's read grants, minus
## withheld credential columns, as the app-table owner. Needs WAREHOUSE_ADMIN_URL
## (the app user's URL, e.g. through `make db-production`) and WAREHOUSE_ROLE.
## Re-run after a migration adds a table dbt reads or the withheld list changes.
grant-warehouse-reads:
	@: "$${WAREHOUSE_ADMIN_URL:?set WAREHOUSE_ADMIN_URL to the database URL of the app user}"
	@: "$${WAREHOUSE_ROLE:?set WAREHOUSE_ROLE, e.g. bossanova_warehouse_production}"
	cd services/warehouse && uv run --frozen python scripts/grant_warehouse_reads.py

## bootstrap-posthog-raw: Owner step. Create the app-owned raw_posthog landing
## tables and grant the PostHog export role and the warehouse role on them, as the
## app user. Needs POSTHOG_ADMIN_URL (the app user's URL, e.g. through
## `make db-production`), POSTHOG_EXPORT_ROLE and WAREHOUSE_ROLE. With
## POSTHOG_PROBE_EXPORT_URL (the export user's URL) it then probes that user's
## privileges and fails unless every probe passes. Idempotent.
bootstrap-posthog-raw:
	@: "$${POSTHOG_ADMIN_URL:?set POSTHOG_ADMIN_URL to the database URL of the app user}"
	@: "$${POSTHOG_EXPORT_ROLE:?set POSTHOG_EXPORT_ROLE, e.g. bossanova_posthog_export_production}"
	@: "$${WAREHOUSE_ROLE:?set WAREHOUSE_ROLE, e.g. bossanova_warehouse_production}"
	cd services/warehouse && uv run --frozen python scripts/bootstrap_posthog_raw.py

## deploy-warehouse-flow: Owner step. Create or update the bossanova.production
## warehouse-dbt-build flow on the shared Kestra instance. Needs KESTRA_API_URL,
## KESTRA_USERNAME and KESTRA_PASSWORD; refuses any non-bossanova.* namespace.
deploy-warehouse-flow:
	cd services/warehouse && uv run --frozen python scripts/kestra_deploy.py
endif
