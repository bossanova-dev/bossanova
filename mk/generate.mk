.PHONY: gen-skill generate glossary-update projectconfig-schema projectconfig-schema-check readme-gifs rules-table-update skill-config-validator skill-config-validator-check test-manifest-update

## gen-skill: Regenerate the /boss skill command reference from the CLI.
## The reference region of lib/bossalib/skillpayload/skills/boss/SKILL.md is
## generated from the cobra command tree; TestSkillMatchesGenerated fails if it drifts.
gen-skill:
	node scripts/skill-source-rewrite-lock.mjs --repo-root "$(CURDIR)" -- sh -c 'cd services/boss && go run ./cmd gen-skill'

## generate: Run buf generate to produce Go code from proto definitions
generate: buf-check-version
	$(MAKE) --no-print-directory $(GEN_STAMP)

# Make web deps and buf conditional — public repo has committed gen code and no web/
GEN_DEPS := $(PROTO_SOURCES) $(OPENAPI_SOURCES)
ifneq ($(wildcard services/web/package.json),)
GEN_DEPS += $(WEB_DEPS_STAMP)
endif

# Stage generation and formatting, then sync changed files with atomic replacement.
# An absent pin uses committed output and leaves the stamp untouched.
$(GEN_STAMP): $(GEN_DEPS)
	@sh scripts/ensure-buf.sh check "$(BUF_VERSION)" "$(BUF)"; result=$$?; \
	case $$result in \
		0) ;; \
		3) echo "pinned buf absent — committed gen used (run 'make generate' to provision)"; exit 0 ;; \
		*) exit $$result ;; \
	esac; \
	set -e; \
	stage=$$(mktemp -d "$${TMPDIR:-/tmp}/bossanova-gen.XXXXXX"); \
	trap 'rm -rf "$$stage"' EXIT; \
	"$(BUF)" generate -o "$$stage"; \
	find "$$stage/lib/bossalib/gen" -name '*.go' -print0 | xargs -0 go tool goimports -w; \
	node scripts/sync-generated-tree.mjs --from "$$stage/lib/bossalib/gen" --to lib/bossalib/gen --prune '**/*.go'; \
	node scripts/sync-generated-tree.mjs --from "$$stage/services/web/src/gen" --to services/web/src/gen --prune '**/*_pb.ts'; \
	node scripts/sync-generated-tree.mjs --from "$$stage/services/docs/openapi" --to services/docs/openapi; \
	touch $(GEN_STAMP)

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

## test-manifest-update: Regenerate the checked-in test command manifest.
test-manifest-update:
	mkdir -p docs/testing
	node scripts/generate-test-command-manifest.mjs > docs/testing/test-command-manifest.md

## rules-table-update: Regenerate the guard rules tables (the generated region in CLAUDE.md
## and docs/philosophy/rules.md) from scripts/guards/registry/. `make lint-scripts` fails
## when either is stale; on a rebase conflict in them, take either side and rerun this.
glossary-update:
	@node scripts/guards/glossary.mjs --write

rules-table-update:
	node scripts/guards/rules-table.mjs --write

## projectconfig-schema: Regenerate $(PROJECTCONFIG_SCHEMA) from the Go types in
## lib/bossalib/projectconfig (the TestSchemaGenerated golden test with -update).
projectconfig-schema:
	cd lib/bossalib && go test -count=1 -run '^TestSchemaGenerated$$' ./projectconfig -update

## projectconfig-schema-check: Fail if $(PROJECTCONFIG_SCHEMA) is stale against the Go types.
projectconfig-schema-check:
	cd lib/bossalib && go test -count=1 -run '^TestSchemaGenerated$$' ./projectconfig

## skill-config-validator: Regenerate skills-toolbox/skill-config-validate.generated.mjs (the
## dependency-free .boss-skills.json validator) from scripts/skill-config-schema/schema.mjs, then
## vendor it into every core that ships skill-config.mjs with `make vendor-toolbox`.
skill-config-validator:
	node scripts/skill-config-schema/build.mjs

## skill-config-validator-check: Fail if the generated .boss-skills.json validator is stale.
skill-config-validator-check:
	node scripts/skill-config-schema/build.mjs --check

.PHONY: sqlc-bin sqlc-generate sqlc-diff sqlc-vet sqlc-check store-only-sql-update

## sqlc-bin: Build the pinned standalone sqlc tool
sqlc-bin:
	GOWORK=off go build -C tools/sqlc -o $(CURDIR)/bin/sqlc github.com/sqlc-dev/sqlc/cmd/sqlc

## sqlc-generate: Regenerate both services' SQL bindings
sqlc-generate: sqlc-bin
	bin/sqlc generate -f services/bossd/sqlc.yaml
	bin/sqlc generate -f services/bosso/sqlc.yaml

## sqlc-diff: Check committed SQL bindings for drift
sqlc-diff: sqlc-bin
	bin/sqlc diff -f services/bossd/sqlc.yaml
	bin/sqlc diff -f services/bosso/sqlc.yaml

## sqlc-vet: Prepare SQL queries against migrated scratch databases
sqlc-vet: sqlc-bin
	go run ./services/bossd/internal/dbtest/migratedb -- $(CURDIR)/services/bossd/sqlc.vet.yaml
	go run ./services/bosso/internal/dbtest/migratedb -- $(CURDIR)/services/bosso/sqlc.vet.yaml

## sqlc-check: Check generated SQL freshness and query preparation
sqlc-check: sqlc-diff sqlc-vet

## store-only-sql-update: Refresh the SQL import baseline and depguard exclusions
store-only-sql-update:
	node scripts/guards/store-only-sql.mjs --write
