.PHONY: all format lint test

## all: Format, lint, and test scripts
all: format lint test

## format: Format script files with Prettier
# Covers scripts/ plus the thin gate entry scripts that live inside each skill
# (.claude/skills/*/gate/*.mjs) — their shared logic is tested via scripts/, but
# the entries themselves still need formatting + a syntax check.
format:
	cd .. && pnpm exec prettier --write 'scripts/*.{cjs,mjs}' 'scripts/bazel/*.mjs' 'scripts/changelog/*.{cjs,mjs}' 'scripts/skill-parity/*.{cjs,mjs}' 'skills-toolbox/*.mjs' 'skills-toolbox/{callback,cron-gates,session,finalize,tracker}/*.{cjs,mjs}' '.claude/skills/*/gate/*.mjs'

## lint: Check script syntax + skill-tree shell fences (content-hash stamped; skips unchanged trees)
# lint-scripts.mjs `node --check`s the same .cjs/.mjs set the old shell loop did,
# but stamps the file-set content in a machine-wide dir so a warm re-run over an
# unchanged tree skips ~200 per-file node spawns. LINT_FORCE=1 bypasses.
# check-skill-shell.mjs runs `bash -n` over every fenced shell block under the two
# authoring skill roots (unstamped; ~0.5-2s pooled). This target is what CI's
# test-scripts.yml reaches via the root `make lint-scripts`.
lint:
	@node lint-scripts.mjs
	@node check-skill-shell.mjs

## test: Run script tests
test:
	cd .. && node --test scripts/*.test.mjs scripts/bazel/*.test.mjs scripts/changelog/*.test.mjs scripts/skill-parity/*.test.mjs
	cd .. && node --test skills-toolbox/*.test.mjs skills-toolbox/{callback,cron-gates,session,finalize,tracker}/*.test.mjs
	cd .. && node scripts/check-test-command-manifest.mjs
	cd .. && node scripts/check-skill-symbols.mjs
