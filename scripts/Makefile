.PHONY: all format lint test

## all: Format, lint, and test scripts
all: format lint test

## format: Format script files with Prettier
# Covers scripts/ plus the thin gate entry scripts that live inside each skill
# (.claude/skills/*/gate/*.mjs) — their shared logic is tested via scripts/, but
# the entries themselves still need formatting + a syntax check.
format:
	cd .. && pnpm exec prettier --write 'scripts/*.{cjs,mjs}' '.claude/skills/*/gate/*.mjs'

## lint: Check script syntax
lint:
	@for file in *.cjs *.mjs ../.claude/skills/*/gate/*.mjs; do \
		[ -e "$$file" ] || continue; \
		echo "==> Checking $$file"; \
		node --check "$$file"; \
	done

## test: Run script tests
test:
	cd .. && node --test scripts/*.test.mjs
	cd .. && node scripts/check-test-command-manifest.mjs
