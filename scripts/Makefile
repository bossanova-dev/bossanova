.PHONY: all format lint test

## all: Format, lint, and test scripts
all: format lint test

## format: Format script files with Prettier
# Covers scripts/ plus the thin gate entry scripts that live inside each skill
# (.claude/skills/*/gate/*.mjs) — their shared logic is tested via scripts/, but
# the entries themselves still need formatting + a syntax check.
format:
	cd .. && pnpm exec prettier --write 'scripts/*.{cjs,mjs}' 'scripts/publish/*.{cjs,mjs}' 'scripts/tracker/*.{cjs,mjs}' 'scripts/finalize/*.{cjs,mjs}' 'scripts/session/*.{cjs,mjs}' 'scripts/changelog/*.{cjs,mjs}' 'scripts/skill-parity/*.{cjs,mjs}' 'skills-toolbox/*.mjs' '.claude/skills/*/gate/*.mjs'

## lint: Check script syntax
lint:
	@for file in *.cjs *.mjs publish/*.mjs tracker/*.mjs finalize/*.mjs session/*.mjs changelog/*.mjs skill-parity/*.mjs ../skills-toolbox/*.mjs ../.claude/skills/*/gate/*.mjs; do \
		[ -e "$$file" ] || continue; \
		echo "==> Checking $$file"; \
		node --check "$$file"; \
	done

## test: Run script tests
test:
	cd .. && node --test scripts/*.test.mjs scripts/publish/*.test.mjs scripts/tracker/*.test.mjs scripts/finalize/*.test.mjs scripts/session/*.test.mjs scripts/changelog/*.test.mjs scripts/skill-parity/*.test.mjs
	cd .. && node --test skills-toolbox/*.test.mjs
	cd .. && node scripts/check-test-command-manifest.mjs
