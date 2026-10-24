.PHONY: all format lint test

## all: Format, lint, and test scripts
all: format lint test

## format: Format script files with Prettier
# Covers scripts/ plus the thin gate entry scripts that live inside each skill
# (.claude/skills/*/gate/*.mjs) — their shared logic is tested via scripts/, but
# the entries themselves still need formatting + a syntax check — and the verify
# skill's control CLI (.claude/skills/verify/{bin,lib}), whose tests run under `test`.
format:
	cd .. && node scripts/layout/file-sets.mjs format

## lint: Check script syntax (content-hash stamped; skips unchanged trees) +
## skill-tree shell fences + docs mcp props/counts + docs absolute self-links +
## template-literal comment backticks + proto bool prefixes +
## settings readiness figure + exec WaitDelay verdicts + no-explicit-any +
## inline Stop hooks + README assets + public mirror workflows
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
# historical docs under docs/solutions and docs/solutions, where old counts are dated records.
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
# Proto field names and web filenames read inputs outside scripts/, so these
# shrinking-baseline guards remain unstamped.
# check-settings-readiness-figure.mjs asserts the worst-case session-start figure on
# services/docs/docs/reference/settings.md equals the product the three Go constants
# compose — DefaultSessionStartReadyDeadline (lib/bossalib/config), plus
# sessionStartReadyAttempts and modalProbeTimeout (services/bossd/internal/adapter/tmux), which
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
# check-email-course.mjs gates the trial onboarding course under docs/email-course/ —
# send count and offsets, README/subject agreement, word bands, one runnable block per
# teaching send, the banned register, published skill names, live docs links, and the
# absent price. Like the gates above it is unstamped, and for the same load-bearing
# reason: every input it reads lives OUTSIDE scripts/ (the course itself, the docs tree,
# and skills_manifest_test.go), so a stamp keyed on script-file content would be warm on
# exactly the prose-only edits it exists to catch. docs/email-course/** is therefore also
# listed in test-scripts.yml's two path filters, or an email-only edit would never reach
# this target in CI. Cheap (~30ms, node built-ins only, no network).
# check-price-parity.mjs gates the DISPLAYED Bossanova Cloud price across three build
# surfaces that cannot import from one another: services/web, services/marketing, and
# PRODUCT.md (prose, which can import nothing at all). It enforces both halves — the
# surfaces that render a price import it from that app's declaration module rather than
# re-typing it, and every price-shaped literal left anywhere in those trees states an
# amount the declarations declare or derive, so a drifting comment fails too. Unstamped
# for the same load-bearing reason as the gates above: every input lives OUTSIDE scripts/,
# so a lint-scripts.mjs stamp keyed on script-file content would be warm on exactly the
# app-and-prose edits that produce the drift. services/web/src/**, services/marketing/src/**
# and PRODUCT.md are therefore also listed in test-scripts.yml's two path filters, or a
# price edit would never reach this target in CI. It gates the displayed price only: the
# charged amount lives in Stripe and is not readable at build time. Cheap (~0.1s CPU, node
# built-ins only, no network).
# check-ellipsis-consistency.mjs keeps BOS-1087 converged: user-facing copy elides with the
# ellipsis character U+2026, never with a run of three full stops. It reads STRING LITERALS only —
# Go interpreted and raw literals, TypeScript quoted and template strings, JSX attribute values and
# JSX text nodes — across the five trees where rendered copy actually lives (the TUI's views, the
# accountflow and auth flows and the cmd package that print CLI progress lines of their own, plus
# services/web/src). Trees that merely PRODUCE a string a human later sees — internal/agent,
# bossd/internal/server, the two plugins — are deliberately outside that boundary; the measurement
# behind the line is recorded on SCAN_TREES. Comments are deliberately not scanned: what remains
# in this repo's comments is elision inside quoted prose and shape description, none of it a
# rendering surface, and scanning it would grow the exception list past the point where anyone
# audits it. Language syntax that shares the spelling — JS spread, Go variadics — is excluded
# structurally rather than by pattern, because a literal-only scanner never sees it. What survives
# inside a literal is enumerated, never matched: an inline `ellipsis: literal-dots ok` marker at the
# site, or an exemption baseline entry that ratchets, so a converted survivor cannot leave its licence
# behind. Unstamped for the same load-bearing reason as the gates above: every input lives OUTSIDE
# scripts/, so a lint-scripts.mjs stamp keyed on script-file content would be warm on exactly the
# Go-and-TSX copy edits that reopen the split. The four services/boss trees are therefore
# also listed in test-scripts.yml's two path filters (services/web/src/** is already there for the
# price gate), or a copy edit would never reach this target in CI. Cheap (~0.5s, node built-ins
# only, ~493 files read).
# check-cli-flag-parsers.mjs ratchets hand-rolled argv parsers in scripts/ and skills-toolbox/: a new
# CLI parses with node:util parseArgs (strict), which rejects the misnamed and value-less flags that
# hand-rolled parsers silently accepted. Its baseline fails on stale entries, so it only shrinks.
# check-published-core-identity.mjs applies TestPublishedCoresAreProjectAgnostic's forbiddenIdentity
# rules (read from the Go file, not copied) to the skills-toolbox sources VENDOR_MAP ships into a
# published core. That Go test is bazel-manual, so without this a ticket id in a toolbox comment
# surfaces only in CI or review.
lint:
	@node lint/lint-scripts.mjs
# Guidance wording pins are baseline-relative; weight signals remain audit-only.
	@cd .. && node skills-toolbox/retro/guidance-audit.mjs --gate --baseline scripts/guards/guidance-audit-baseline.json
	node guards/check-todo-tickets.mjs
	node guards/check-comment-paths.mjs
	node guards/docs-layout.mjs
	node guards/docs-summary.mjs
	node guards/docs-paths.mjs
	@node guards/check-skill-shell.mjs
	@node guards/check-skill-node-fences.mjs
	@node guards/check-dispatch-contract.mjs
	@node guards/check-docs-mcp-props.mjs
	@node guards/check-docs-tool-counts.mjs
	@node guards/check-docs-absolute-selflinks.mjs
	@node guards/check-template-literal-comments.mjs
	@node guards/check-proto-field-names.mjs
	@node guards/web-feature-boundaries.mjs overrides --check
	@node guards/web-feature-boundaries.mjs boundaries --check
	@node guards/web-feature-boundaries.mjs layout --check
	@node guards/check-web-filenames.mjs
	@node guards/check-web-e2e-harness-imports.mjs
	@node guards/check-settings-readiness-figure.mjs
	@node guards/check-exec-waitdelay.mjs
	@node guards/check-timing-bounds.mjs
	@node guards/check-email-course.mjs
	@node guards/check-price-parity.mjs
# sync-docs-web-helpers.mjs --check: services/docs/src/vendor/web-helpers/ is a byte copy of
# lib/web-helpers/src (docs sits outside the pnpm workspace), and no consumer tree re-declares a
# shared export. Unstamped for the price gate's reason: its inputs live outside scripts/.
	@node skills/sync-docs-web-helpers.mjs --check
	@node guards/check-ellipsis-consistency.mjs
	@node guards/check-cli-flag-parsers.mjs
	@node guards/bosso-tenant-access-compile.mjs
	@node guards/check-published-core-identity.mjs
	@node guards/check-flaky-quarantine.mjs
	@node guards/check-fixture-rm-retries.mjs
	@node guards/check-shared-test-fakes.mjs
	@node guards/check-test-sleeps.mjs
# Four gates that had no CI home (BOS-1719): check-no-explicit-any.mjs ran only as duplicate steps
# in the two TS workflows, and check-no-inline-stop-hooks.mjs, check-readme-assets.mjs and
# check-public-mirror-workflows.mjs only behind their root `make test-*` targets. Running them here
# gives each one wiring, local `make lint` and CI's scripts job alike. The last two resolve their
# inputs against the working directory, so they run from the repo root.
	@node guards/check-no-explicit-any.mjs
	@node guards/check-web-api-casts.mjs
	@node guards/check-ts-complexity.mjs
	@node guards/check-no-inline-stop-hooks.mjs
	@cd .. && node scripts/guards/check-readme-assets.mjs
	@cd .. && node scripts/guards/check-public-mirror-workflows.mjs
# The guard registry (scripts/guards/): check-registry.mjs proves every registered guard names a
# proof test, scans at least one tracked file and is reached by its CI job's path filter, and
# runs the golangci caps check; rules-table.mjs --check fails when the generated rules tables
# drift from the registry. Both are unstamped: their inputs (CLAUDE.md, docs/philosophy/,
# .golangci.yml, workflows) live outside scripts/, so a stamp keyed on script content would be
# warm on exactly the edits that break them.
	@node guards/check-handwritten-grpc-glue.mjs
	@node guards/check-registry.mjs
	@node guards/rules-table.mjs --check
	@node guards/docs-index.mjs --check
	@node guards/sqlc-no-slice.mjs
	@node guards/store-verbs.mjs
	@node guards/store-only-sql.mjs --check
	@node guards/store-imports.mjs
	@node guards/doors-no-store.mjs --check
	@node guards/proto-at-the-edge.mjs --check
	@node guards/boss-connect-only-in-adapters.mjs --check
	@node guards/e2e-harness-imports.mjs --check
	@node guards/boss-tui-screens-isolated.mjs --check
	@node guards/boss-tui-screens-no-adapters.mjs --check
	@node guards/domain-imports.mjs
	@node guards/bossalib-consumers.mjs
	@node guards/entity-names.mjs
	@cd .. && node scripts/guards/script-layout.mjs
	@cd .. && node scripts/guards/file-size.mjs
	@cd .. && node scripts/guards/script-file-names.mjs
	@cd .. && node scripts/guards/warehouse-model-names.mjs
	@cd .. && node scripts/guards/warehouse-mart-tests.mjs
	@node guards/glossary.mjs --check
	@node guards/sweep-schedule.mjs --check
	@node guards/merging-doc.mjs --check
	@node guards/boundaries-doc.mjs --check
	@node guards/feature-map.mjs ownership --check
	@node guards/feature-map.mjs guides --check
	@node guards/feature-map.mjs index --check
# check-js-types.mjs runs tsc --checkJs (scripts/tsconfig.json) over scripts/ and skills-toolbox/
# against a shrink-only baseline; unstamped (~4 s), since a stamp would hash the same files.
	@node guards/check-js-types.mjs
# The compiled .boss-skills.json validator (skills-toolbox/skill-config-validate.generated.mjs)
# must match scripts/skills/skill-config-schema/schema.mjs byte-for-byte. Unstamped: the generated file
# is not in lint-scripts.mjs's stamp key, so a stamp would be warm on a hand edit to it.
	@node skills/skill-config-schema/build.mjs --check

## test: Run script tests
test:
	GOWORK=off go test -C ../tools/testscan -count=1 ./...
	cd .. && node scripts/test-infra/run-gate.mjs --label "scripts test" -- node scripts/layout/file-sets.mjs test
# BOS-737: the vacuity scan runs here, not only from the root `test-all`. The suites above
# exercise the detector against stub fixtures; this runs it against the real tree. `test-all`
# is not reached by any feature-push job (see .github/workflows/bazel.yml), so wiring the
# ratchet there alone would let a new offender merge green and only surface post-merge.
	cd .. && node scripts/guards/run-nontap-gates.mjs scripts/guards/check-test-command-manifest.mjs scripts/guards/check-skill-symbols.mjs scripts/guards/check-vacuous-regions.mjs scripts/guards/check-vacuous-job-gates.mjs
