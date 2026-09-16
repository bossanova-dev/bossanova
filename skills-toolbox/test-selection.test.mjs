// Tests for test-selection.mjs — the portable affected-test decision helper.
//
// Every fail-safe branch is an ABSENCE assertion: the helper must be SEEN returning
// `full` for its own reason code. A suite that only ever observed the narrow path would
// stay green if every fail-safe in the ladder were deleted outright, which is the exact
// vacuous green this module exists to avoid. So each branch below asserts the reason CODE,
// not prose, and asserts that `tests` came back empty so a "full" verdict can never be
// mistaken for a narrow one that happened to select nothing.

import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { decideTestSelection, renderSelectionReport, SELECTION_REASONS } from './test-selection.mjs'

// A minimal well-formed block: one rule, no sharedFiles, no testRoots. Individual tests
// override only what they are pinning, so a scenario reads as its own delta.
const block = (overrides = {}) => ({
  testSelection: {
    rules: [{ changed: 'src/**', tests: ['t/src.test.js'] }],
    ...overrides,
  },
})

const codes = (decision) => decision.reasons.map((r) => r.code)

// --- Ladder stop 1: no config -------------------------------------------------------

test('no testSelection declared forces a full run with no-selection-config', () => {
  const decision = decideTestSelection({ config: {}, changedFiles: ['src/a.js'] })
  assert.equal(decision.mode, 'full')
  assert.deepEqual(codes(decision), [SELECTION_REASONS.NO_SELECTION_CONFIG])
  assert.deepEqual(decision.tests, [])
  assert.equal(decision.selectedCount, 0)
})

test('a malformed block resolves through the accessor to no-selection-config, not a throw', () => {
  // `rules: []` is malformed rather than empty per the config contract; the accessor
  // returns null for it, and the helper must inherit that absence contract rather than
  // re-deriving one (and rather than throwing on a hand-built config).
  for (const bad of [
    { testSelection: { rules: [] } },
    { testSelection: {} },
    {},
    null,
    undefined,
  ]) {
    const decision = decideTestSelection({ config: bad, changedFiles: ['src/a.js'] })
    assert.equal(decision.mode, 'full', `expected full for ${JSON.stringify(bad)}`)
    assert.deepEqual(codes(decision), [SELECTION_REASONS.NO_SELECTION_CONFIG])
  }
})

// --- Ladder stop 2: no changed files ------------------------------------------------

test('an empty changed-file list forces a full run with no-changed-files', () => {
  const decision = decideTestSelection({ config: block(), changedFiles: [] })
  assert.equal(decision.mode, 'full')
  assert.deepEqual(codes(decision), [SELECTION_REASONS.NO_CHANGED_FILES])
  assert.deepEqual(decision.tests, [])
})

test('a caller that supplied no changed-file list at all is the same fail-safe', () => {
  for (const missing of [undefined, null, 'src/a.js']) {
    const decision = decideTestSelection({ config: block(), changedFiles: missing })
    assert.equal(decision.mode, 'full')
    assert.deepEqual(codes(decision), [SELECTION_REASONS.NO_CHANGED_FILES])
  }
})

// --- Ladder stop 3: shared files, and the ORDER that makes them load-bearing ---------

test('a sharedFiles hit forces a full run and the reason names that path', () => {
  const decision = decideTestSelection({
    config: block({ sharedFiles: ['package.json'] }),
    changedFiles: ['src/a.js', 'package.json'],
  })
  assert.equal(decision.mode, 'full')
  assert.deepEqual(decision.reasons, [
    { code: SELECTION_REASONS.SHARED_FILE, path: 'package.json' },
  ])
  assert.deepEqual(decision.tests, [])
})

test('LADDER ORDER: a shared file wins even when a rule would also have matched it', () => {
  // The load-bearing case. If rule matching ran first this config would produce a
  // confident-looking narrow selection for exactly the change that invalidates narrow
  // selection. One path, matched by BOTH a shared pattern and a rule.
  const decision = decideTestSelection({
    config: {
      testSelection: {
        rules: [{ changed: 'package.json', tests: ['t/pkg.test.js'] }],
        sharedFiles: ['package.json'],
      },
    },
    changedFiles: ['package.json'],
  })
  assert.equal(decision.mode, 'full')
  assert.deepEqual(codes(decision), [SELECTION_REASONS.SHARED_FILE])
  assert.deepEqual(decision.tests, [], 'the matching rule must not contribute its tests')
})

test('one invalidating shared file beats any number of cleanly classified ones', () => {
  const decision = decideTestSelection({
    config: block({ sharedFiles: ['**/*.lock'] }),
    changedFiles: ['src/a.js', 'src/b.js', 'src/c.js', 'deps/pnpm.lock'],
  })
  assert.equal(decision.mode, 'full')
  assert.deepEqual(decision.reasons, [
    { code: SELECTION_REASONS.SHARED_FILE, path: 'deps/pnpm.lock' },
  ])
})

// --- Ladder stop 4: unclassified ----------------------------------------------------

test('a path matched by no rule and no shared pattern forces full with unclassified', () => {
  const decision = decideTestSelection({
    config: block(),
    changedFiles: ['src/a.js', 'docs/readme.md'],
  })
  assert.equal(decision.mode, 'full')
  assert.deepEqual(decision.reasons, [
    { code: SELECTION_REASONS.UNCLASSIFIED, path: 'docs/readme.md' },
  ])
  assert.deepEqual(decision.tests, [])
})

test('an absolute path is rejected as unclassified rather than relativized', () => {
  const decision = decideTestSelection({
    config: block(),
    changedFiles: ['/Users/somebody/checkout/src/a.js'],
  })
  assert.equal(decision.mode, 'full')
  assert.deepEqual(decision.reasons, [
    { code: SELECTION_REASONS.UNCLASSIFIED, path: '/Users/somebody/checkout/src/a.js' },
  ])
})

test('an unusable changed-file entry is unclassified, never silently dropped', () => {
  for (const junk of ['', '   ', 42, null]) {
    const decision = decideTestSelection({ config: block(), changedFiles: ['src/a.js', junk] })
    assert.equal(decision.mode, 'full', `expected full for ${JSON.stringify(junk)}`)
    assert.deepEqual(codes(decision), [SELECTION_REASONS.UNCLASSIFIED])
  }
})

test('a leading ./ is normalized and classified rather than rejected', () => {
  const decision = decideTestSelection({ config: block(), changedFiles: ['./src/a.js'] })
  assert.equal(decision.mode, 'narrow')
  assert.deepEqual(decision.tests, ['t/src.test.js'])
})

// --- Ladder stop 5: an empty computed selection is never a pass ----------------------

test('a selection resolving to zero test files forces full with empty-selection', () => {
  // Reachable only because the caller supplied the universe of test files it knows about:
  // the rule matched, but its declared tests resolve against that universe to nothing.
  // R2's direct pin — this must never come back as a narrow run that trivially succeeds.
  const decision = decideTestSelection({
    config: block({ rules: [{ changed: 'src/**', tests: ['t/gone/**'] }] }),
    changedFiles: ['src/a.js'],
    testFiles: ['t/src.test.js', 't/other.test.js'],
  })
  assert.equal(decision.mode, 'full')
  assert.deepEqual(codes(decision), [SELECTION_REASONS.EMPTY_SELECTION])
  assert.deepEqual(decision.tests, [])
  assert.equal(decision.selectedCount, 0)
})

// --- The narrow path ----------------------------------------------------------------

test('one changed file matching one rule narrows to exactly that rule tests', () => {
  const decision = decideTestSelection({ config: block(), changedFiles: ['src/a.js'] })
  assert.equal(decision.mode, 'narrow')
  assert.deepEqual(decision.tests, ['t/src.test.js'])
  assert.deepEqual(decision.reasons, [])
  assert.equal(decision.selectedCount, 1)
})

test('one changed file matching two rules returns the deduplicated union', () => {
  const decision = decideTestSelection({
    config: block({
      rules: [
        { changed: 'src/**', tests: ['t/a.test.js', 't/shared.test.js'] },
        { changed: '**/*.js', tests: ['t/shared.test.js', 't/b.test.js'] },
      ],
    }),
    changedFiles: ['src/a.js'],
  })
  assert.equal(decision.mode, 'narrow')
  assert.deepEqual(decision.tests, ['t/a.test.js', 't/shared.test.js', 't/b.test.js'])
  assert.equal(decision.selectedCount, 3)
})

test('several classified changed files union across all of their rules', () => {
  const decision = decideTestSelection({
    config: block({
      rules: [
        { changed: 'src/**', tests: ['t/src.test.js'] },
        { changed: 'web/**', tests: ['t/web.test.js', 't/src.test.js'] },
      ],
    }),
    changedFiles: ['src/a.js', 'web/b.tsx'],
  })
  assert.equal(decision.mode, 'narrow')
  assert.deepEqual(decision.tests, ['t/src.test.js', 't/web.test.js'])
})

test('the selection is stably ordered, so a caller command line is reproducible', () => {
  const args = {
    config: block({
      rules: [
        { changed: 'src/**', tests: ['t/z.test.js', 't/a.test.js'] },
        { changed: 'src/a.js', tests: ['t/a.test.js', 't/m.test.js'] },
      ],
    }),
    changedFiles: ['src/a.js'],
  }
  const first = decideTestSelection(args)
  const second = decideTestSelection(args)
  assert.deepEqual(first.tests, second.tests)
  assert.deepEqual(first.tests, ['t/z.test.js', 't/a.test.js', 't/m.test.js'])
})

test('a trailing-slash pattern matches by directory prefix', () => {
  const decision = decideTestSelection({
    config: block({ rules: [{ changed: 'src/', tests: ['t/src.test.js'] }] }),
    changedFiles: ['src/deep/nested/a.js'],
  })
  assert.equal(decision.mode, 'narrow')
  assert.deepEqual(decision.tests, ['t/src.test.js'])
})

test('a supplied universe resolves test globs to concrete files', () => {
  const decision = decideTestSelection({
    config: block({ rules: [{ changed: 'src/**', tests: ['t/**'] }] }),
    changedFiles: ['src/a.js'],
    testFiles: ['t/one.test.js', 't/two.test.js', 'other/three.test.js'],
  })
  assert.equal(decision.mode, 'narrow')
  assert.deepEqual(decision.tests, ['t/one.test.js', 't/two.test.js'])
})

// --- U3: the report -----------------------------------------------------------------

test('a narrow decision with testRoots reports the selected count and the total', () => {
  const decision = decideTestSelection({
    config: block({ testRoots: ['t/**'] }),
    changedFiles: ['src/a.js'],
    testFiles: ['t/src.test.js', 't/one.test.js', 't/two.test.js', 'notatest.js'],
  })
  assert.equal(decision.mode, 'narrow')
  assert.equal(decision.selectedCount, 1)
  assert.equal(decision.totalCount, 3)
  assert.match(decision.report, /selected 1 of 3 test files/)
  assert.equal(decision.report, renderSelectionReport(decision))
})

test('a narrow decision without testRoots reports an explicit unknown total', () => {
  // A universe IS supplied here, so `tests` really are files and "unknown total" is a
  // statement about the missing `testRoots`, not about the missing universe.
  const decision = decideTestSelection({
    config: block(),
    changedFiles: ['src/a.js'],
    testFiles: ['t/src.test.js'],
  })
  assert.equal(decision.resolved, true)
  assert.equal(decision.totalCount, null)
  assert.match(decision.report, /unknown total/)
  assert.doesNotMatch(decision.report, /of 0 /, 'must not fabricate a denominator')
  assert.match(decision.report, /selected 1 test file\b/)
})

// --- The two-mode split: tests in the DIRECTION OF ITS RISK -------------------------
//
// Pass-through mode (no `testFiles`) is the module's default and its riskiest surface:
// stop 5 cannot fire there, so a narrow verdict can carry patterns that expand to nothing.
// These pin that the decision and the report SAY so, rather than presenting a pattern
// count as a file count.

test('pass-through mode reports unexpanded PATTERNS, not a file count', () => {
  const decision = decideTestSelection({ config: block(), changedFiles: ['src/a.js'] })
  assert.equal(decision.mode, 'narrow')
  assert.equal(decision.resolved, false, 'no universe was supplied, so nothing was resolved')
  assert.deepEqual(decision.tests, ['t/src.test.js'])
  assert.match(decision.report, /unexpanded test pattern\b/)
  assert.doesNotMatch(
    decision.report,
    /test file/,
    'a pattern count must never be reported as a file count',
  )
})

test('pass-through mode discloses that the empty-selection guard did not run', () => {
  // R2 is the caller's obligation in this mode; the decision must not imply otherwise.
  const decision = decideTestSelection({
    config: block({ rules: [{ changed: 'src/**', tests: ['t/gone/**'] }] }),
    changedFiles: ['src/a.js'],
  })
  assert.equal(decision.mode, 'narrow')
  assert.equal(decision.resolved, false)
  assert.deepEqual(decision.tests, ['t/gone/**'], 'the pattern passes through verbatim')
  assert.match(decision.report, /empty-selection guard did not run/)
})

test('a resolved decision is marked resolved, so a consumer can tell the modes apart', () => {
  const decision = decideTestSelection({
    config: block({ rules: [{ changed: 'src/**', tests: ['t/**'] }] }),
    changedFiles: ['src/a.js'],
    testFiles: ['t/one.test.js'],
  })
  assert.equal(decision.resolved, true)
  assert.deepEqual(decision.tests, ['t/one.test.js'])
})

test('testRoots declared but NO universe supplied still reports an unknown total', () => {
  // AC7's diagonal. There is genuinely nothing to count without a universe, so `null` is
  // the honest answer — the helper must not invent a denominator from the patterns.
  const decision = decideTestSelection({
    config: block({ testRoots: ['t/**'] }),
    changedFiles: ['src/a.js'],
  })
  assert.equal(decision.resolved, false)
  assert.equal(decision.totalCount, null, 'no universe means no countable total')
  assert.doesNotMatch(decision.report, /of \d+ test files/, 'must not fabricate a denominator')
})

test('a full decision report names the forcing reason and claims no narrow selection', () => {
  const decision = decideTestSelection({ config: {}, changedFiles: ['src/a.js'] })
  assert.match(decision.report, /full run/)
  assert.match(decision.report, /no-selection-config/)
  assert.doesNotMatch(decision.report, /narrow/)
})

test('a full decision forced by several reasons surfaces ALL of them, not just the first', () => {
  const decision = decideTestSelection({
    config: block({ sharedFiles: ['package.json'] }),
    changedFiles: ['package.json', 'docs/readme.md', 'src/a.js'],
  })
  assert.equal(decision.mode, 'full')
  assert.deepEqual(decision.reasons, [
    { code: SELECTION_REASONS.SHARED_FILE, path: 'package.json' },
    { code: SELECTION_REASONS.UNCLASSIFIED, path: 'docs/readme.md' },
  ])
  assert.match(decision.report, /shared-file \(package\.json\)/)
  assert.match(decision.report, /unclassified \(docs\/readme\.md\)/)
})

test('the report is a single line with no ANSI escapes and no trailing whitespace', () => {
  // Fed a hostile path carrying a newline and a real ESC byte: the report must still be
  // one line safe to embed in any core's log.
  const hostile = 'weird\x1b[31m/pa\nth.md'
  const decisions = [
    decideTestSelection({ config: block(), changedFiles: [hostile] }),
    decideTestSelection({ config: block(), changedFiles: ['src/a.js'] }),
    decideTestSelection({ config: {}, changedFiles: ['src/a.js'] }),
    decideTestSelection({ config: block(), changedFiles: [] }),
  ]
  for (const decision of decisions) {
    const { report } = decision
    assert.equal(typeof report, 'string')
    assert.ok(report.length > 0)
    assert.doesNotMatch(report, /\n|\r/, `multi-line report: ${JSON.stringify(report)}`)
    assert.doesNotMatch(report, /\x1b/, `ANSI escape in report: ${JSON.stringify(report)}`)
    assert.equal(report, report.trimEnd(), 'trailing whitespace')
  }
})

// --- R6/R7: structural, not prose ---------------------------------------------------

const helperSource = readFileSync(
  fileURLToPath(new URL('./test-selection.mjs', import.meta.url)),
  'utf8',
)

test('the helper spawns no process and reads no file (asserted structurally)', () => {
  for (const forbidden of [
    'child_process',
    'execSync',
    'spawnSync',
    'readFileSync',
    'node:fs',
    'node:child_process',
  ]) {
    assert.ok(
      !helperSource.includes(forbidden),
      `${forbidden} must not appear in the helper source`,
    )
  }
})

test('the helper carries no host-repo path, module name, runner or build tool', () => {
  // It is published into every user's global skill directory, so a single local convention
  // baked in here would ship to thousands of unrelated checkouts.
  for (const forbidden of [/bossanova/i, /bazel/i, /services\/boss/i, /lib\/bossalib/i]) {
    assert.doesNotMatch(helperSource, forbidden)
  }
})
