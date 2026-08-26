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
## skill-tree shell fences + docs mcp props/counts + docs absolute self-links +
## template-literal comment backticks + proto bool prefixes +
## prose-pin whitespace + settings readiness figure + exec WaitDelay verdicts
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
# check-docs-tool-counts.mjs asserts the live-doc local-catalog MCP tool-count claims
# match the same registered set. It deliberately checks an allowlist rather than
# historical docs under docs/plans and docs/solutions, where old counts are dated records.
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
# check-proto-bool-prefix.mjs enforces the DB/proto boolean naming rule for new
# bossanova.v1 fields while allowlisting the legacy fields that predate it. It is
# unstamped because it reads proto files outside scripts/, and it ratchets: a stale
# allowlist entry fails once the corresponding field is renamed.
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
# check-settings-readiness-figure.mjs asserts the worst-case session-start figure on
# services/docs/docs/reference/settings.md equals the product the three Go constants
# compose — DefaultSessionStartReadyDeadline (lib/bossalib/config), plus
# sessionStartReadyAttempts and modalProbeTimeout (services/bossd/internal/tmux), which
# no single module can import together (BOS-949). Unstamped for the same reason as
# check-docs-mcp-props.mjs, and the reason is load-bearing here rather than incidental:
# every input this gate reads lives OUTSIDE scripts/, so a lint-scripts.mjs stamp keyed on
# script-file content is warm on precisely the docs-only and Go-only edits that produce the
# drift. That is the drift it exists to catch — the page had already gone stale under a
# manual "keep these in step" comment before this gate existed. Cheap (~25ms, node
# built-ins only, four files read).
# check-exec-waitdelay.mjs enforces the BOS-927 inherited-pipe invariant for Go exec.Cmd
# sites that capture stdout/stderr into non-*os.File writers. It is unstamped because the
# inputs are Go files outside scripts/, and it intentionally fails closed on command
# variables it cannot locally resolve.
lint:
	@node lint-scripts.mjs
	@node check-skill-shell.mjs
	@node check-skill-node-fences.mjs
	@node check-dispatch-contract.mjs
	@node check-dispatch-batching.mjs
	@node check-extension-dispatch-parity.mjs
	@node check-docs-mcp-props.mjs
	@node check-docs-tool-counts.mjs
	@node check-docs-absolute-selflinks.mjs
	@node check-template-literal-comments.mjs
	@node check-proto-bool-prefix.mjs
	@node check-prose-pin-whitespace.mjs
	@node check-settings-readiness-figure.mjs
	@node check-exec-waitdelay.mjs

## test: Run script tests
test:
	cd .. && node --test scripts/*.test.mjs scripts/bazel/*.test.mjs scripts/changelog/*.test.mjs scripts/skill-parity/*.test.mjs
	cd .. && node --test skills-toolbox/*.test.mjs skills-toolbox/{callback,cron-gates,session,finalize,tracker}/*.test.mjs
# BOS-737: the vacuity scan runs here, not only from the root `test-all`. The suites above
# exercise the detector against stub fixtures; this runs it against the real tree. `test-all`
# is not reached by any feature-push job (see .github/workflows/bazel.yml), so wiring the
# ratchet there alone would let a new offender merge green and only surface post-merge.
	cd .. && node scripts/run-nontap-gates.mjs scripts/check-test-command-manifest.mjs scripts/check-skill-symbols.mjs scripts/check-vacuous-regions.mjs scripts/check-raw-size-ratchets.mjs
