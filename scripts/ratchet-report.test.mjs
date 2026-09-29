// Tests for scripts/ratchet-report.mjs (BOS-1341).
//
// The report's value is that it fails closed, so most of these feed it a failing shape and require
// a red verdict. The last test runs a real `node --test` child over a throwaway suite, so the
// recorder -> file -> collector chain is exercised with no stub in between.
//
// Node built-ins only — cron worktrees are dependency-free.

import { test } from 'node:test'
import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { pathToFileURL } from 'node:url'

import {
  FIXTURE_SUITES,
  buildReport,
  classify,
  collectRows,
  countCallSites,
  formatReport,
  main,
  runSuites,
  selectRatchetSuites,
  tripwire,
} from './ratchet-report.mjs'

const repoRoot = path.resolve(import.meta.dirname, '..')
const libUrl = pathToFileURL(path.join(import.meta.dirname, 'size-ratchet-lib.mjs')).href

const noProse = { counts: {}, growth: [], shrink: [], stale: [] }

const budgetRow = (overrides = {}) => ({
  kind: 'budget',
  label: 'demo budget',
  path: 'skills/demo/SKILL.md',
  constName: 'RATCHET',
  constFile: 'scripts/demo-skill.test.mjs',
  unit: 'bytes',
  measured: 90,
  pinned: 100,
  suite: 'scripts/demo-skill.test.mjs',
  callSite: 'scripts/demo-skill.test.mjs:10',
  ...overrides,
})

const exactRow = (overrides = {}) =>
  budgetRow({
    kind: 'exact',
    constName: 'PIN',
    measured: 100,
    pinned: 100,
    callSite: 'scripts/demo-skill.test.mjs:20',
    ...overrides,
  })

const verdicts = (rows) =>
  rows.map(({ constName, verdict, headroom }) => ({ constName, verdict, headroom }))

function scratch() {
  return fs.mkdtempSync(path.join(os.tmpdir(), 'ratchet-report-test-'))
}

// ── classify ──────────────────────────────────────────────────────────────────────────────

test('a budget row under budget is within, with its headroom; one over budget fails', () => {
  const { gates } = classify(
    [budgetRow(), budgetRow({ constName: 'OVER', measured: 104 })],
    noProse,
    {},
  )
  assert.deepEqual(verdicts(gates), [
    { constName: 'RATCHET', verdict: 'within', headroom: 10 },
    { constName: 'OVER', verdict: 'over', headroom: -4 },
  ])
  const report = buildReport({
    files: ['scripts/demo-skill.test.mjs'],
    rows: [budgetRow({ measured: 104 })],
    prosePins: noProse,
    runnerStatus: 0,
    baseline: {},
  })
  assert.equal(report.ok, false)
})

test('an exact row passes on equality and fails on a mismatch either way', () => {
  const { gates } = classify(
    [
      exactRow(),
      exactRow({ constName: 'UNDER', measured: 99 }),
      exactRow({ constName: 'UP', measured: 101 }),
    ],
    noProse,
    {},
  )
  assert.deepEqual(
    gates.map(({ constName, verdict }) => [constName, verdict]),
    [
      ['PIN', 'match'],
      ['UNDER', 'mismatch'],
      ['UP', 'mismatch'],
    ],
  )
})

test('prose-pin growth, shrink and stale each fail and name the file', () => {
  const prosePins = {
    counts: {
      'scripts/a-skill.test.mjs': 5,
      'scripts/b-skill.test.mjs': 1,
      'scripts/c-skill.test.mjs': 2,
    },
    growth: [{ file: 'scripts/a-skill.test.mjs', expected: 4, actual: 5, excess: [] }],
    shrink: [{ file: 'scripts/b-skill.test.mjs', expected: 3, actual: 1 }],
    stale: ['scripts/gone-skill.test.mjs'],
  }
  const baseline = {
    'scripts/a-skill.test.mjs': 4,
    'scripts/b-skill.test.mjs': 3,
    'scripts/c-skill.test.mjs': 2,
    'scripts/gone-skill.test.mjs': 7,
  }
  const report = buildReport({ files: [], rows: [], prosePins, runnerStatus: 0, baseline })
  assert.deepEqual(
    report.prose.map(({ constFile, pinned, measured, verdict }) => [
      constFile,
      pinned,
      measured,
      verdict,
    ]),
    [
      ['scripts/a-skill.test.mjs', 4, 5, 'growth'],
      ['scripts/b-skill.test.mjs', 3, 1, 'shrink'],
      ['scripts/c-skill.test.mjs', 2, 2, 'match'],
      ['scripts/gone-skill.test.mjs', 7, null, 'stale'],
    ],
  )
  assert.equal(report.ok, false)
  assert.deepEqual(report.prosePinTotal, { pinned: 16, measured: 8 })
})

// ── collectRows ───────────────────────────────────────────────────────────────────────────

test('collectRows drops rows whose path is not a repo file, and exact duplicates', () => {
  const root = scratch()
  const dir = scratch()
  fs.mkdirSync(path.join(root, 'skills'), { recursive: true })
  fs.writeFileSync(path.join(root, 'skills', 'real.md'), 'x')
  const real = budgetRow({ path: 'skills/real.md' })
  const rows = [
    real,
    real,
    budgetRow({ path: 'skills/missing.md' }),
    budgetRow({ path: 'skills' }),
    budgetRow({ path: '../outside.md' }),
  ]
  fs.writeFileSync(
    path.join(dir, '1.jsonl'),
    rows.map((row) => JSON.stringify(row)).join('\n') + '\n',
  )
  fs.writeFileSync(path.join(dir, 'ignored.txt'), 'not a row')
  assert.deepEqual(collectRows(dir, root), [real])
})

// ── tripwire and runner status ────────────────────────────────────────────────────────────

test('a selected suite with no row fails the tripwire', () => {
  const files = ['scripts/demo-skill.test.mjs', 'scripts/silent-skill.test.mjs']
  const silent = ['scripts/silent-skill.test.mjs (0 of 1 call site(s) recorded)']
  assert.deepEqual(tripwire(files, [budgetRow()]), silent)
  const report = buildReport({
    files,
    rows: [budgetRow()],
    prosePins: noProse,
    runnerStatus: 0,
    baseline: {},
  })
  assert.equal(report.ok, false)
  assert.deepEqual(report.silentSuites, silent)
})

test('a row counts for the suite that recorded it, whatever constFile it names', () => {
  const files = ['scripts/demo-skill.test.mjs']
  const rows = [budgetRow({ constFile: 'scripts/shared-constants.mjs' })]
  assert.deepEqual(tripwire(files, rows), [])
  const report = buildReport({ files, rows, prosePins: noProse, runnerStatus: 0, baseline: {} })
  assert.equal(report.ok, true)
})

test('a suite recording fewer distinct gates than its call sites fails the tripwire', () => {
  const files = ['scripts/demo-skill.test.mjs']
  const callSites = { 'scripts/demo-skill.test.mjs': 2 }
  // The same gate recorded twice is still one gate.
  const rows = [budgetRow(), budgetRow({ measured: 91 })]
  const report = buildReport({
    files,
    rows,
    prosePins: noProse,
    runnerStatus: 0,
    baseline: {},
    callSites,
  })
  assert.equal(report.ok, false)
  assert.deepEqual(report.silentSuites, [
    'scripts/demo-skill.test.mjs (1 of 2 call site(s) recorded)',
  ])
  assert.deepEqual(tripwire(files, [budgetRow(), exactRow()], callSites), [])
})

test('a loop recording many gates from one call site cannot cover a sibling that never ran', () => {
  const files = ['scripts/demo-skill.test.mjs']
  const callSites = { 'scripts/demo-skill.test.mjs': 2 }
  // Two distinct gates, both recorded from the loop on line 10; the call on line 20 never ran.
  const rows = [budgetRow(), budgetRow({ constName: 'OTHER', path: 'skills/other/SKILL.md' })]
  assert.deepEqual(tripwire(files, rows, callSites), [
    'scripts/demo-skill.test.mjs (1 of 2 call site(s) recorded)',
  ])
})

test('a row with no readable call site, or one in another file, counts for nothing', () => {
  const files = ['scripts/demo-skill.test.mjs']
  assert.deepEqual(tripwire(files, [budgetRow({ callSite: null })]), [
    'scripts/demo-skill.test.mjs (0 of 1 call site(s) recorded)',
  ])
  assert.deepEqual(tripwire(files, [budgetRow({ callSite: 'scripts/helper.mjs:3' })]), [
    'scripts/demo-skill.test.mjs (0 of 1 call site(s) recorded)',
  ])
})

test('rows naming a silent suite as constFile do not mask it', () => {
  const files = ['scripts/demo-skill.test.mjs', 'scripts/silent-skill.test.mjs']
  const rows = [
    budgetRow(),
    exactRow({ constFile: 'scripts/silent-skill.test.mjs', path: 'skills/other/SKILL.md' }),
  ]
  assert.deepEqual(tripwire(files, rows), [
    'scripts/silent-skill.test.mjs (0 of 1 call site(s) recorded)',
  ])
})

test('countCallSites counts every primitive call in a suite', () => {
  const root = scratch()
  fs.mkdirSync(path.join(root, 'scripts'))
  fs.writeFileSync(
    path.join(root, 'scripts', 'two.test.mjs'),
    'assertExactSize({})\nassertDescendingBudget ({})\nassertExactSizeLike()\n',
  )
  assert.deepEqual(countCallSites(['scripts/two.test.mjs'], root), {
    'scripts/two.test.mjs': 2,
  })
})

test('countCallSites ignores primitive names in comments, strings and regex literals', () => {
  const root = scratch()
  fs.mkdirSync(path.join(root, 'scripts'))
  fs.writeFileSync(
    path.join(root, 'scripts', 'prose.test.mjs'),
    [
      '// assertExactSize( in a line comment',
      '/* assertDescendingBudget( in a',
      '   block comment assertExactSize( */',
      'const a = \'assertExactSize(\' + "assertDescendingBudget("',
      'const b = `assertExactSize(',
      '  assertDescendingBudget(`',
      "const c = 'it\\'s assertExactSize('",
      "const d = [/a`b[/`]c/, /it's/]",
      'const e = x / 2 + assertDescendingBudget({})',
      '// a closing comment',
      'assertExactSize({ label: "x" })',
      '',
    ].join('\n'),
  )
  assert.deepEqual(countCallSites(['scripts/prose.test.mjs'], root), {
    'scripts/prose.test.mjs': 2,
  })
})

test('an empty measurement never passes', () => {
  const report = buildReport({
    files: ['scripts/demo-skill.test.mjs'],
    rows: [],
    prosePins: noProse,
    runnerStatus: 0,
    baseline: {},
  })
  assert.equal(report.ok, false)
})

test('a non-zero runner status fails even when every row passes', () => {
  const report = buildReport({
    files: ['scripts/demo-skill.test.mjs'],
    rows: [budgetRow()],
    prosePins: noProse,
    runnerStatus: 1,
    baseline: {},
  })
  assert.equal(report.ok, false)
  assert.deepEqual(report.reasons, ['suite run exited 1'])
  const green = buildReport({
    files: ['scripts/demo-skill.test.mjs'],
    rows: [budgetRow()],
    prosePins: noProse,
    runnerStatus: 0,
    baseline: {},
  })
  assert.equal(green.ok, true)
})

// ── output ────────────────────────────────────────────────────────────────────────────────

test('the text table prints pinned beside measured for each gate, and the verdict line', () => {
  const report = buildReport({
    files: ['scripts/demo-skill.test.mjs'],
    rows: [budgetRow(), exactRow()],
    prosePins: { counts: { 'scripts/demo-skill.test.mjs': 3 }, growth: [], shrink: [], stale: [] },
    runnerStatus: 0,
    baseline: { 'scripts/demo-skill.test.mjs': 3 },
  })
  const lines = formatReport(report).split('\n')
  assert.deepEqual(
    lines.slice(1).map((line) => line.split(/\s+/).slice(0, 7)),
    [
      ['budget', 'scripts/demo-skill.test.mjs', 'RATCHET', '100', '90', 'bytes', 'within'],
      ['exact', 'scripts/demo-skill.test.mjs', 'PIN', '100', '100', 'bytes', 'match'],
      ['prose-pin', 'scripts/demo-skill.test.mjs', 'PROSE_PIN_BASELINE', '3', '3', 'pins', 'match'],
      ['prose-pin', 'total:', 'pinned', '3,', 'measured', '3', 'across'],
      ['ratchet-report:', 'OK', '—', '2', 'size', 'gate(s)', 'from'],
    ],
  )
})

test('--json output parses and carries the same verdicts as the table', () => {
  const root = scratch()
  fs.mkdirSync(path.join(root, 'skills'))
  fs.writeFileSync(path.join(root, 'skills', 'demo.md'), 'x')
  const run = (files, dir) => {
    const rows = [
      budgetRow({ path: 'skills/demo.md' }),
      exactRow({ path: 'skills/demo.md', measured: 99 }),
    ]
    fs.writeFileSync(path.join(dir, '7.jsonl'), rows.map((row) => JSON.stringify(row)).join('\n'))
    return 0
  }
  const deps = {
    repoRoot: root,
    select: () => ['scripts/demo-skill.test.mjs'],
    count: (files) => Object.fromEntries(files.map((file) => [file, 1])),
    run,
    prose: () => noProse,
  }
  const printed = []
  const jsonCode = main(['--json'], { ...deps, log: (text) => printed.push(text) })
  const textCode = main([], { ...deps, log: (text) => printed.push(text) })
  assert.equal(jsonCode, 1)
  assert.equal(textCode, 1)
  const parsed = JSON.parse(printed[0])
  assert.equal(parsed.ok, false)
  assert.deepEqual(
    parsed.gates.map(({ constName, verdict }) => [constName, verdict]),
    [
      ['RATCHET', 'within'],
      ['PIN', 'mismatch'],
    ],
  )
  // The table carries the same two verdicts, in the same order.
  assert.deepEqual(
    printed[1]
      .split('\n')
      .slice(1, 3)
      .map((line) => line.split(/\s+/)[6]),
    ['within', 'mismatch'],
  )
})

// ── selection ─────────────────────────────────────────────────────────────────────────────

test('selection keeps primitive callers, drops fixture suites, and refuses an empty set', () => {
  const selected = selectRatchetSuites(repoRoot)
  for (const fixture of FIXTURE_SUITES) assert.equal(selected.includes(fixture), false, fixture)
  assert.ok(selected.includes('scripts/bs-plan-skill.test.mjs'))
  // Imports the library without calling either primitive, so it has no gate to report.
  assert.equal(selected.includes('scripts/bs-sweep-update-skill.test.mjs'), false)

  const empty = scratch()
  fs.mkdirSync(path.join(empty, 'scripts'))
  fs.writeFileSync(path.join(empty, 'scripts', 'x.test.mjs'), "import { test } from 'node:test'\n")
  assert.throws(() => selectRatchetSuites(empty), /nothing to measure/)
})

// ── integration: a real node --test child, the real recorder, the real collector ──────────

test('integration: a tiny suite yields exactly one within-budget row end to end', () => {
  const root = scratch()
  const reportDir = scratch()
  fs.mkdirSync(path.join(root, 'art'))
  fs.writeFileSync(path.join(root, 'art', 'SKILL.md'), '0123456789')
  fs.writeFileSync(
    path.join(root, 'tiny.test.mjs'),
    [
      "import { test } from 'node:test'",
      `import { assertDescendingBudget, measureFile } from '${libUrl}'`,
      "test('tiny budget', () => {",
      '  assertDescendingBudget({',
      "    label: 'tiny', path: 'art/SKILL.md', constName: 'TINY', constFile: 'tiny.test.mjs',",
      "    measured: measureFile(new URL('./art/SKILL.md', import.meta.url).pathname),",
      "    budget: 16, raise: { from: 16 }, reviewBy: '2999-01-01', stepDown: 1,",
      "    residual: 'fixture',",
      '  })',
      '})',
      '',
    ].join('\n'),
  )
  const status = runSuites(['tiny.test.mjs'], reportDir, { cwd: root, stdio: 'ignore' })
  assert.equal(status, 0)
  const rows = collectRows(reportDir, root)
  assert.deepEqual(rows, [
    {
      kind: 'budget',
      label: 'tiny',
      path: 'art/SKILL.md',
      constName: 'TINY',
      constFile: 'tiny.test.mjs',
      unit: 'bytes',
      measured: 10,
      pinned: 16,
      suite: 'tiny.test.mjs',
      callSite: 'tiny.test.mjs:4',
    },
  ])
  assert.deepEqual(verdicts(classify(rows, noProse, {}).gates), [
    { constName: 'TINY', verdict: 'within', headroom: 6 },
  ])
})
