.PHONY: all format lint test

## all: Format, lint, and test scripts
all: format lint test

## format: Format script files with Prettier
# Covers scripts/ plus the thin gate entry scripts that live inside each skill
# (.claude/skills/*/gate/*.mjs) — their shared logic is tested via scripts/, but
# the entries themselves still need formatting + a syntax check.
format:
	cd .. && pnpm exec prettier --write 'scripts/*.{cjs,mjs}' 'scripts/bazel/*.mjs' 'scripts/changelog/*.{cjs,mjs}' 'scripts/skill-parity/*.{cjs,mjs}' 'skills-toolbox/*.mjs' 'skills-toolbox/{callback,cron-gates,session,finalize,tracker}/*.{cjs,mjs}' '.claude/skills/*/gate/*.mjs'

## lint: Check script syntax + skill-tree shell fences + docs mcp props (content-hash
## stamped; skips unchanged trees)
# lint-scripts.mjs `node --check`s the same .cjs/.mjs set the old shell loop did,
# but stamps the file-set content in a machine-wide dir so a warm re-run over an
# unchanged tree skips ~200 per-file node spawns. LINT_FORCE=1 bypasses.
# check-skill-shell.mjs runs `bash -n` over every fenced shell block under the two
# authoring skill roots (unstamped; ~0.5-2s pooled). This target is what CI's
# test-scripts.yml reaches via the root `make lint-scripts`.
# check-docs-mcp-props.mjs asserts every `<CommandTabs mcp="…">` prop names a tool
# lib/bossalib/bossmcp registers. It hangs off this target rather than off
# lint-scripts.mjs on purpose: that script's stamp key covers script-file content
# only, so a docs-only or bossmcp-only change would hit a warm stamp and skip this
# gate entirely. Unstamped and cheap (~30ms, node built-ins only).
lint:
	@node lint-scripts.mjs
	@node check-skill-shell.mjs
	@node check-docs-mcp-props.mjs

## test: Run script tests
test:
	cd .. && node --test scripts/*.test.mjs scripts/bazel/*.test.mjs scripts/changelog/*.test.mjs scripts/skill-parity/*.test.mjs
	cd .. && node --test skills-toolbox/*.test.mjs skills-toolbox/{callback,cron-gates,session,finalize,tracker}/*.test.mjs
	cd .. && node scripts/check-test-command-manifest.mjs
	cd .. && node scripts/check-skill-symbols.mjs
# BOS-737: the vacuity scan runs here, not only from the root `test-all`. The suites above
# exercise the detector against stub fixtures; this runs it against the real tree. `test-all`
# is not reached by any feature-push job (see .github/workflows/bazel.yml), so wiring the
# ratchet there alone would let a new offender merge green and only surface post-merge.
	cd .. && node scripts/check-vacuous-regions.mjs
