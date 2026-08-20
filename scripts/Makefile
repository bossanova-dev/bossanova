.PHONY: all format lint test

## all: Format, lint, and test scripts
all: format lint test

## format: Format script files with Prettier
# Covers scripts/ plus the thin gate entry scripts that live inside each skill
# (.claude/skills/*/gate/*.mjs) — their shared logic is tested via scripts/, but
# the entries themselves still need formatting + a syntax check.
format:
	cd .. && pnpm exec prettier --write 'scripts/*.{cjs,mjs}' 'scripts/bazel/*.mjs' 'scripts/changelog/*.{cjs,mjs}' 'scripts/skill-parity/*.{cjs,mjs}' 'skills-toolbox/*.mjs' 'skills-toolbox/{callback,cron-gates,session,finalize,tracker}/*.{cjs,mjs}' '.claude/skills/*/gate/*.mjs'

## lint: Check script syntax (content-hash stamped; skips unchanged trees) +
## skill-tree shell fences + docs mcp props + docs absolute self-links +
## template-literal comment backticks + prose-pin whitespace
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
# check-docs-absolute-selflinks.mjs forbids a docs page linking to another docs page
# by absolute https://docs.bossanova.dev/ URL — Docusaurus's onBrokenLinks:'throw'
# only resolves relative links, so an absolute self-link is never checked and a dead
# one never reds. Hangs off this target for the same stamp reason as the gate above.
# check-template-literal-comments.mjs forbids a backtick in a comment nested inside a
# JS template literal (BOS-787). Inside template text there are no comments, so the
# backtick ENDS the literal: the file still parses, lint-scripts.mjs stays green, and
# prettier reformats the wreckage into something a reviewer reads as a harmless
# reformat. It reuses lint-scripts.mjs's own resolveScriptFiles() so the two gates
# cannot drift apart on what "the repo's scripts" are, but is unstamped like
# check-skill-shell.mjs — string scanning only, ~150ms over ~300 files (296 today).
# check-prose-pin-whitespace.mjs forbids a regex literal that pins a multi-word phrase
# with literal spaces in a skill-content gate file (BOS-797). Prettier is not what moves
# that prose: the root .prettierrc sets no proseWrap, so markdown paragraphs keep the
# breaks their author gave them. A human or an agent rewraps a paragraph while editing a
# neighbouring sentence, the pinned phrase crosses a line break with no word of it
# changing, and the pin goes red naming no real defect. The same \s+ rule governs a falsification
# pattern used to MUTATE that prose, where the same mistake fails silently instead —
# the substitution matches nothing, exits 0, and leaves the suite green, which is
# byte-identical to a genuine "this pin is vacuous" finding. This gate can only see the
# first half; docs/skills/README.md states both. Its file set is the glob
# scripts/*skill*.test.mjs rather than a hand-list, so a new gate file is covered without
# editing it, and it is unstamped like the two gates above — string scanning only, ~80ms
# over the 20 files that glob matches today.
lint:
	@node lint-scripts.mjs
	@node check-skill-shell.mjs
	@node check-docs-mcp-props.mjs
	@node check-docs-absolute-selflinks.mjs
	@node check-template-literal-comments.mjs
	@node check-prose-pin-whitespace.mjs

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
	cd .. && node scripts/check-raw-size-ratchets.mjs
