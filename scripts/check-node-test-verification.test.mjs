#!/usr/bin/env node

/**
 * Gate for CLAUDE.md's `node --test` verification recipe (BOS-862).
 *
 * One derivation, two consumers:
 *   - the behaviour half spawns real `node --test --test-reporter=tap` runs over
 *     throwaway fixtures and reads the tokens and exit codes out of that output;
 *   - the prose half asserts CLAUDE.md's bullet documents those same tokens, plus
 *     the English clauses no reporter can emit.
 *
 * The recipe this gates replaced one that named the *default* reporter, which is
 * true in one cell of the node-version x TTY matrix and false in another. Pinning
 * the reporter collapses that matrix, so nothing here is version-conditional
 * except the documented node floor.
 */

import assert from 'node:assert/strict'
import { execFileSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { after, test } from 'node:test'

const REPO_ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
const CLAUDE_MD = path.join(REPO_ROOT, 'CLAUDE.md')

// Every assertion message is stamped with the interpreter: a green here is only
// ever a green *on this node*, and the zero-match semantics are major-dependent.
const NODE_VERSION = process.versions.node
const NODE_MAJOR = Number.parseInt(NODE_VERSION.split('.')[0], 10)
const NODE_FLOOR = 22

// --- shared count-free prefixes -------------------------------------------
// The behaviour half composes these with an expected count; the prose half finds
// them verbatim in CLAUDE.md. Sharing the instantiated token instead would make
// the prose half unsatisfiable (docs write the shape, not the count); sharing
// nothing at all would let the two halves drift apart, which is the defect this
// file exists to prevent.
const TAP_HEADER = 'TAP version 13'
const PASS_PREFIX = '# pass '
const FAIL_PREFIX = '# fail '
const TESTS_PREFIX = '# tests '
const SKIPPED_PREFIX = '# skipped '
const TODO_PREFIX = '# todo '
const PLAN_PREFIX = '1..'

// Per-line TAP directives. The summary counters are necessary but not sufficient:
// a skipped suite and a failing todo both leave every counter clean, so the
// markers on the `ok`/`not ok` lines are what actually disqualify a run.
const SKIP_MARKER = '# SKIP'
const TODO_MARKER = '# TODO'
const NOT_OK_MARKER = 'not ok'

// The pin itself, and the override hook for a command whose recipe you must not
// edit (`make -C scripts test` hardcodes an unpinned `node --test`).
const PINNED_FLAG = '--test-reporter=tap'
const NODE_OPTIONS_TOKEN = 'NODE_OPTIONS'

// Prose-only clauses. No reporter can emit these, so they are fixed by the BOS-862
// plan rather than chosen here: a phrase invented alongside the CLAUDE.md rewrite
// and matched against itself would gate nothing.
const COUNT_PHRASE = 'against the count you expected'
const UNKNOWN_PHRASE = 'unknown, never pass'
// "the named tests are visible" is not enough: a filter that matched nothing
// prints the file's own name. The recipe has to say *whose* names.
const NAMES_PHRASE = 'every test name you expected'
const RETIRED_CLAIM = 'those tokens never appear'

// Located by stable leading text, never by line number. A whole-file match would
// be satisfiable by unrelated prose added later.
const SECTION_ANCHOR = '### Commands whose result lies'
const BULLET_ANCHOR = '- **A filter over human-readable output lies about structure.**'

// The all-pass fixture's test count, known in advance so the pass assertion is an
// equality against a number rather than a loose `# pass \d+` presence match.
const ALL_PASS_COUNT = 3

// The skip fixture's shape, likewise known in advance.
const SKIPPED_TEST_NAME = 'the test you cared about'
const SKIPPED_FIXTURE_TESTS = 2
const SKIPPED_FIXTURE_SKIPS = 1

// The two escape hatches that leave every summary counter clean.
const SKIPPED_SUITE_NAME = 'the suite you cared about'
const SKIPPED_SUITE_TESTS = 1 // the one real test; the suite's two children are not counted at all
const TODO_TEST_NAME = 'the test whose failure was not counted'
const TODO_FIXTURE_TODOS = 1

// A filter that matches nothing: the one case that defeats every counter AND
// every per-line marker. Node reports the *file* as a passing test.
const FILTER_FIXTURE_TEST_NAMES = ['alpha', 'beta']
const FILTER_MATCHES_NOTHING = 'no-such-test-name'

// A *partial* filter is worse still: it leaves a real test name on stdout, so
// "some names are visible" is satisfied while the one you cared about is gone
// from every counter and every marker. Only "every name you expected" catches it.
const PARTIAL_KEPT_NAME = 'the test that was kept'
const PARTIAL_DROPPED_NAME = 'the test you cared about'

// The filter flags the bullet must name, so a reader knows which switches do this.
const FILTER_FLAGS = ['--test-only', '--test-name-pattern', '--test-skip-pattern', '--test-shard']

const tempDirs = []

function mkTemp(prefix) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), prefix))
  tempDirs.push(dir)
  return dir
}

/**
 * Spawn a real `node --test --test-reporter=tap` run and capture stdout + status.
 *
 *   - `process.execPath`, never the PATH `node`: that is an nodenv shim which can
 *     die before node ever starts under an altered environment.
 *   - shell-free, so nothing expands the zero-match glob argument.
 *   - `NODE_TEST_CONTEXT` deleted: under `make test-scripts` this very file runs as
 *     a node-test child with `NODE_TEST_CONTEXT=child-v8` set, and a grandchild
 *     that inherits it switches to the V8-serializer reporter and writes zero
 *     bytes to stdout regardless of `--test-reporter=tap`. That is this ticket's
 *     own failure mode reproduced inside its fix.
 *   - `NODE_OPTIONS` deleted, so an ambient reporter pin can never be the reason
 *     this file is green.
 *
 * `stderr` is returned, not discarded: below the documented node floor a
 * zero-match glob writes nothing to stdout and puts the real cause
 * (`Could not find '<path>'`) on stderr. Without it the reader gets "the child
 * wrote nothing to stdout" and never sees why.
 */
function runNodeTest(args, { pinVia = 'flag' } = {}) {
  const env = { ...process.env }
  delete env.NODE_TEST_CONTEXT
  delete env.NODE_OPTIONS
  // `pinVia: 'env'` exercises the other half of the documented recipe — the form
  // for a command whose argv you cannot edit. The flag is omitted entirely, so
  // this leg is green only if the environment variable really did the pinning.
  if (pinVia !== 'flag') env.NODE_OPTIONS = `--test-reporter=${pinVia}`
  const argv = pinVia === 'flag' ? ['--test', PINNED_FLAG, ...args] : ['--test', ...args]
  try {
    const stdout = execFileSync(process.execPath, argv, {
      encoding: 'utf8',
      env,
      stdio: ['ignore', 'pipe', 'pipe'],
    })
    return { code: 0, stdout, stderr: '' }
  } catch (err) {
    return {
      code: err.status ?? 1,
      stdout: err.stdout?.toString() ?? '',
      stderr: err.stderr?.toString() ?? '',
      signal: err.signal ?? null,
    }
  }
}

function writeAllPassFixture() {
  const file = path.join(mkTemp('nodetestverify-pass-'), 'fixture.test.mjs')
  const cases = []
  for (let i = 1; i <= ALL_PASS_COUNT; i += 1) {
    cases.push(`test('all-pass fixture case ${i}', () => {\n  assert.equal(${i}, ${i})\n})\n`)
  }
  const header = "import assert from 'node:assert/strict'\nimport test from 'node:test'\n\n"
  fs.writeFileSync(file, header + cases.join('\n'))
  return file
}

function writeFailingFixture() {
  const file = path.join(mkTemp('nodetestverify-fail-'), 'fixture.test.mjs')
  fs.writeFileSync(
    file,
    "import test from 'node:test'\n\n" +
      "test('deliberate throw', () => {\n" +
      "  throw new Error('deliberate BOS-862 fixture failure')\n" +
      '})\n',
  )
  return file
}

/**
 * One skipped test beside one real one. This is the ticket's own title case: a
 * skip still prints `ok N - <name> # SKIP`, still counts in `# tests N`, still
 * leaves `# fail 0`, and still exits 0 — so "the named tests are visible and
 * `# tests N` is non-zero" is satisfied by a run that never executed the test
 * you cared about. Only `# skipped 0` separates them.
 */
function writeSkippedFixture() {
  const file = path.join(mkTemp('nodetestverify-skip-'), 'fixture.test.mjs')
  fs.writeFileSync(
    file,
    "import test from 'node:test'\n\n" +
      `test('${SKIPPED_TEST_NAME}', { skip: 'guarded away' }, () => {\n` +
      "  throw new Error('a skipped test must never execute its body')\n" +
      '})\n\n' +
      "test('the test that really ran', () => {})\n",
  )
  return file
}

/**
 * A skipped `describe` beside a test that really runs. Worse than a skipped
 * test: the suite marks one `ok … # SKIP` and its children vanish from
 * `# tests N` entirely, so `# skipped 0` stays clean and the count never
 * mentions them. Every summary counter the recipe names is satisfied.
 */
function writeSkippedSuiteFixture() {
  const file = path.join(mkTemp('nodetestverify-suiteskip-'), 'fixture.test.mjs')
  fs.writeFileSync(
    file,
    "import test, { describe, it } from 'node:test'\n\n" +
      "test('a test that really ran', () => {})\n\n" +
      `describe.skip('${SKIPPED_SUITE_NAME}', () => {\n` +
      "  it('inner one', () => {\n" +
      "    throw new Error('a skipped suite must never execute its children')\n" +
      '  })\n' +
      "  it('inner two', () => {\n" +
      "    throw new Error('a skipped suite must never execute its children')\n" +
      '  })\n' +
      '})\n',
  )
  return file
}

/**
 * A `todo` test whose body runs and throws. Node routes the failure into
 * `# todo`, not `# fail` — so `# fail 0` holds, `# skipped 0` holds, and the
 * process still exits 0 while a test demonstrably failed.
 */
function writeFailingTodoFixture() {
  const file = path.join(mkTemp('nodetestverify-todo-'), 'fixture.test.mjs')
  fs.writeFileSync(
    file,
    "import test from 'node:test'\n\n" +
      `test('${TODO_TEST_NAME}', { todo: true }, () => {\n` +
      "  throw new Error('this body runs and throws, yet is not counted a failure')\n" +
      '})\n\n' +
      "test('the test that really ran', () => {})\n",
  )
  return file
}

/**
 * Two real tests, run behind a name filter that matches neither — the mistyped
 * `--test-name-pattern`, sibling of the mistyped `go test -run` two bullets up.
 * This is the only case that defeats every summary counter AND every per-line
 * marker: node reports the *file* as `ok N - <file path>` and counts it 1.
 * The sole tell is that your own test names are absent.
 */
function writeFilterFixture() {
  const file = path.join(mkTemp('nodetestverify-filter-'), 'fixture.test.mjs')
  const cases = FILTER_FIXTURE_TEST_NAMES.map((name) => `test('${name}', () => {})\n`)
  fs.writeFileSync(file, "import test from 'node:test'\n\n" + cases.join('\n'))
  return file
}

/**
 * Two real tests, one marked `only`, run under `--test-only`. The excluded test
 * is dropped from `# tests`, from `# skipped`, from `# todo` and from every
 * per-line marker — and a real test name is still on stdout, so the file-path
 * tell never fires either. `--test-skip-pattern` behaves identically.
 */
function writePartialFilterFixture() {
  const file = path.join(mkTemp('nodetestverify-partial-'), 'fixture.test.mjs')
  fs.writeFileSync(
    file,
    "import test from 'node:test'\n\n" +
      `test('${PARTIAL_KEPT_NAME}', { only: true }, () => {})\n\n` +
      `test('${PARTIAL_DROPPED_NAME}', () => {\n` +
      "  throw new Error('this would fail loudly if the filter had let it run')\n" +
      '})\n',
  )
  return file
}

/** A literal glob argument inside an empty temp dir — nothing can expand it. */
function zeroMatchGlob() {
  return path.join(mkTemp('nodetestverify-empty-'), 'no-such-file-*.test.mjs')
}

// Measured once, at load, so the prose half can only ever assert tokens a real
// run produced.
const RUNS = {
  allPass: runNodeTest([writeAllPassFixture()]),
  failing: runNodeTest([writeFailingFixture()]),
  skipped: runNodeTest([writeSkippedFixture()]),
  skippedSuite: runNodeTest([writeSkippedSuiteFixture()]),
  failingTodo: runNodeTest([writeFailingTodoFixture()]),
  filterMatchedNothing: runNodeTest([
    `--test-name-pattern=${FILTER_MATCHES_NOTHING}`,
    writeFilterFixture(),
  ]),
  partialFilter: runNodeTest(['--test-only', writePartialFilterFixture()]),
  skipPattern: runNodeTest([
    `--test-skip-pattern=${PARTIAL_DROPPED_NAME}`,
    writePartialFilterFixture(),
  ]),
  zeroMatch: runNodeTest([zeroMatchGlob()]),
  // The env-pinned half of the recipe, with no `--test-reporter` flag in argv.
  envPinned: runNodeTest([writeAllPassFixture()], { pinVia: 'tap' }),
  // Control. On node 22 the *default* reporter is already TAP, so the leg above
  // would pass with NODE_OPTIONS unset — vacuously. Selecting a reporter that is
  // the default nowhere proves the variable is actually consulted.
  envDot: runNodeTest([writeAllPassFixture()], { pinVia: 'dot' }),
}

// Behaviour-proved constants the prose half is then required to find.
const ZERO_MATCH_TOKENS = [`${TESTS_PREFIX}0`, `${PLAN_PREFIX}0`, `${PASS_PREFIX}0`]

// Below the documented floor, "node changed, update the docs" would tell an agent
// to rewrite correct guidance to match a stale interpreter.
const FLOOR_ADVICE =
  NODE_MAJOR >= NODE_FLOOR
    ? "node's behaviour appears to have changed; update CLAUDE.md to match"
    : `this interpreter (v${NODE_VERSION}) is older than the documented node >= ${NODE_FLOOR} floor — CLAUDE.md is not the thing to change`

after(() => {
  for (const dir of tempDirs) fs.rmSync(dir, { recursive: true, force: true })
})

function escapeRegExp(value) {
  return value.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')
}

/** Multi-word phrases tolerate any inter-word whitespace; a reflow is not a regression. */
function phraseRegExp(phrase) {
  return new RegExp(phrase.split(/\s+/).map(escapeRegExp).join('\\s+'))
}

/** The integer on a `<prefix><n>` TAP summary line, or null when absent. */
function tapCount(stdout, prefix) {
  const match = stdout.match(new RegExp(`^${escapeRegExp(prefix)}(\\d+)$`, 'm'))
  return match ? Number(match[1]) : null
}

/**
 * Without this, an env leak that silences the child makes every token assertion
 * vacuous while the exit code still reads 0 — the exact false green being fixed.
 */
function diagnose(run) {
  const parts = []
  if (run.signal) parts.push(`child killed by signal ${run.signal}`)
  const stderr = (run.stderr ?? '').trim()
  if (stderr) parts.push(`stderr: ${stderr.slice(0, 400)}`)
  parts.push(FLOOR_ADVICE)
  return parts.join(' | ')
}

function assertTapStdout(run, label) {
  assert.ok(
    run.stdout.length > 0,
    `${label}: the child wrote nothing to stdout on node ${NODE_VERSION}; every token assertion below would be vacuous. ${diagnose(run)}`,
  )
  assert.ok(
    run.stdout.includes(TAP_HEADER),
    `${label}: stdout has no '${TAP_HEADER}' on node ${NODE_VERSION}, so ${PINNED_FLAG} did not take effect. ${diagnose(run)}; got:\n${run.stdout}`,
  )
}

function bulletText() {
  const markdown = fs.readFileSync(CLAUDE_MD, 'utf8')
  const sectionAt = markdown.indexOf(SECTION_ANCHOR)
  assert.notEqual(
    sectionAt,
    -1,
    `anchor moved: CLAUDE.md no longer has the section heading '${SECTION_ANCHOR}' — re-point this gate rather than deleting it`,
  )
  const bulletAt = markdown.indexOf(BULLET_ANCHOR, sectionAt)
  assert.notEqual(
    bulletAt,
    -1,
    `anchor moved: CLAUDE.md has no bullet beginning '${BULLET_ANCHOR}' under '${SECTION_ANCHOR}' — re-point this gate rather than deleting it`,
  )
  const newlineAt = markdown.indexOf('\n', bulletAt)
  return markdown.slice(bulletAt, newlineAt === -1 ? markdown.length : newlineAt)
}

// ---------------------------------------------------------------------------
// Behaviour: what a pinned `node --test` run actually prints
// ---------------------------------------------------------------------------

test('node-test verification: the documented semantics require node >= 22', () => {
  // Loud, never a skip: a skip here is indistinguishable from the gate passing.
  assert.ok(
    NODE_MAJOR >= NODE_FLOOR,
    `documented semantics require node >= ${NODE_FLOOR}; running v${NODE_VERSION}`,
  )
})

test('node-test verification: an all-pass run exits 0 with the expected TAP counts', () => {
  assertTapStdout(RUNS.allPass, 'all-pass')
  assert.equal(
    RUNS.allPass.code,
    0,
    `all-pass run must exit 0 on node ${NODE_VERSION}; got ${RUNS.allPass.code}`,
  )
  assert.equal(
    tapCount(RUNS.allPass.stdout, PASS_PREFIX),
    ALL_PASS_COUNT,
    `expected '${PASS_PREFIX}${ALL_PASS_COUNT}' from a ${ALL_PASS_COUNT}-test fixture on node ${NODE_VERSION}, got '${PASS_PREFIX}${tapCount(RUNS.allPass.stdout, PASS_PREFIX)}' — the count rule is an equality against a number you knew in advance, not a presence match`,
  )
  assert.equal(
    tapCount(RUNS.allPass.stdout, FAIL_PREFIX),
    0,
    `expected '${FAIL_PREFIX}0' from an all-pass fixture on node ${NODE_VERSION}`,
  )
})

test('node-test verification: a deliberate throw exits 1 and is counted as a TAP failure', () => {
  assertTapStdout(RUNS.failing, 'deliberate-failure')
  assert.equal(
    RUNS.failing.code,
    1,
    `a failing test must exit 1 on node ${NODE_VERSION}; got ${RUNS.failing.code}`,
  )
  assert.equal(
    tapCount(RUNS.failing.stdout, FAIL_PREFIX),
    1,
    `expected '${FAIL_PREFIX}1' from a fixture whose single test throws, on node ${NODE_VERSION}`,
  )
})

test('node-test verification: a zero-match glob exits 0 while reporting that nothing ran', () => {
  assertTapStdout(RUNS.zeroMatch, 'zero-match')
  assert.equal(
    RUNS.zeroMatch.code,
    0,
    `a zero-match glob still exits 0 on node ${NODE_VERSION} — that is why exit 0 alone is not proof; got ${RUNS.zeroMatch.code}. ${FLOOR_ADVICE}`,
  )
  for (const token of ZERO_MATCH_TOKENS) {
    assert.ok(
      RUNS.zeroMatch.stdout.includes(token),
      `the zero-match run did not print '${token}' on node ${NODE_VERSION}. ${FLOOR_ADVICE}`,
    )
  }
})

test('node-test verification: a skipped test satisfies every rule except `# skipped 0`', () => {
  assertTapStdout(RUNS.skipped, 'skipped')
  // Everything the weaker suite-level rule looks at is satisfied here...
  assert.equal(
    RUNS.skipped.code,
    0,
    `a run whose test was skipped still exits 0 on node ${NODE_VERSION}; got ${RUNS.skipped.code}`,
  )
  assert.ok(
    RUNS.skipped.stdout.includes(SKIPPED_TEST_NAME),
    `the skipped test's name must still be visible on node ${NODE_VERSION} — that is precisely why "the named tests are visible" cannot prove it ran`,
  )
  assert.equal(
    tapCount(RUNS.skipped.stdout, TESTS_PREFIX),
    SKIPPED_FIXTURE_TESTS,
    `a skipped test is still counted in '${TESTS_PREFIX}<n>' on node ${NODE_VERSION}, so a non-zero count cannot prove it ran`,
  )
  assert.equal(
    tapCount(RUNS.skipped.stdout, FAIL_PREFIX),
    0,
    `a skipped test still leaves '${FAIL_PREFIX}0' on node ${NODE_VERSION}`,
  )
  // ...and only this token tells the two apart.
  assert.equal(
    tapCount(RUNS.skipped.stdout, SKIPPED_PREFIX),
    SKIPPED_FIXTURE_SKIPS,
    `expected '${SKIPPED_PREFIX}${SKIPPED_FIXTURE_SKIPS}' on node ${NODE_VERSION} — this is the only token separating "ran and passed" from "never ran". ${FLOOR_ADVICE}`,
  )
  assert.equal(
    tapCount(RUNS.allPass.stdout, SKIPPED_PREFIX),
    0,
    `an all-pass run must print '${SKIPPED_PREFIX}0' on node ${NODE_VERSION}, or CLAUDE.md documenting it as the proof-of-execution token would be unbacked`,
  )
})

test('node-test verification: a skipped suite leaves every summary counter clean', () => {
  assertTapStdout(RUNS.skippedSuite, 'skipped-suite')
  assert.equal(
    RUNS.skippedSuite.code,
    0,
    `a skipped suite still exits 0 on node ${NODE_VERSION}; got ${RUNS.skippedSuite.code}`,
  )
  // Every counter the recipe names is clean — including the skip counter.
  for (const prefix of [FAIL_PREFIX, SKIPPED_PREFIX, TODO_PREFIX]) {
    assert.equal(
      tapCount(RUNS.skippedSuite.stdout, prefix),
      0,
      `a skipped suite still leaves '${prefix}0' on node ${NODE_VERSION} — counters alone cannot disqualify it`,
    )
  }
  // ...and its children are not merely skipped, they are never counted at all.
  assert.equal(
    tapCount(RUNS.skippedSuite.stdout, TESTS_PREFIX),
    SKIPPED_SUITE_TESTS,
    `a skipped suite's children vanish from '${TESTS_PREFIX}<n>' on node ${NODE_VERSION}, so a non-zero count cannot prove they ran`,
  )
  // Only the per-line marker gives it away.
  assert.match(
    RUNS.skippedSuite.stdout,
    new RegExp(`^ok \\d+ - ${escapeRegExp(SKIPPED_SUITE_NAME)} ${escapeRegExp(SKIP_MARKER)}`, 'm'),
    `expected an '${SKIP_MARKER}' marker on the suite line on node ${NODE_VERSION} — it is the only signal a whole suite was skipped. ${FLOOR_ADVICE}`,
  )
})

test('node-test verification: a failing todo test is not counted as a failure', () => {
  assertTapStdout(RUNS.failingTodo, 'failing-todo')
  assert.equal(
    RUNS.failingTodo.code,
    0,
    `a todo test whose body throws still exits 0 on node ${NODE_VERSION}; got ${RUNS.failingTodo.code}`,
  )
  assert.equal(
    tapCount(RUNS.failingTodo.stdout, FAIL_PREFIX),
    0,
    `a thrown todo test is routed away from '${FAIL_PREFIX}<n>' on node ${NODE_VERSION} — '${FAIL_PREFIX}0' does not mean nothing failed`,
  )
  assert.equal(
    tapCount(RUNS.failingTodo.stdout, SKIPPED_PREFIX),
    0,
    `a thrown todo test is not counted in '${SKIPPED_PREFIX}<n>' either on node ${NODE_VERSION}`,
  )
  assert.equal(
    tapCount(RUNS.failingTodo.stdout, TODO_PREFIX),
    TODO_FIXTURE_TODOS,
    `expected '${TODO_PREFIX}${TODO_FIXTURE_TODOS}' on node ${NODE_VERSION}. ${FLOOR_ADVICE}`,
  )
  // The per-line evidence the recipe tells the reader to look for.
  assert.match(
    RUNS.failingTodo.stdout,
    new RegExp(
      `^${escapeRegExp(NOT_OK_MARKER)} \\d+ - ${escapeRegExp(TODO_TEST_NAME)} ${escapeRegExp(TODO_MARKER)}`,
      'm',
    ),
    `expected a '${NOT_OK_MARKER} … ${TODO_MARKER}' line on node ${NODE_VERSION} — the only trace that a test failed while every counter stayed clean. ${FLOOR_ADVICE}`,
  )
  assert.equal(
    tapCount(RUNS.allPass.stdout, TODO_PREFIX),
    0,
    `an all-pass run must print '${TODO_PREFIX}0' on node ${NODE_VERSION}, or CLAUDE.md documenting it would be unbacked`,
  )
})

test('node-test verification: a filter matching nothing reports the file as a pass', () => {
  const run = RUNS.filterMatchedNothing
  assertTapStdout(run, 'filter-matched-nothing')
  assert.equal(
    run.code,
    0,
    `a filter matching no test still exits 0 on node ${NODE_VERSION}; got ${run.code}`,
  )
  // Defeats every counter...
  for (const prefix of [FAIL_PREFIX, SKIPPED_PREFIX, TODO_PREFIX]) {
    assert.equal(
      tapCount(run.stdout, prefix),
      0,
      `a filter matching nothing still leaves '${prefix}0' on node ${NODE_VERSION}`,
    )
  }
  // ...and every per-line marker: no `not ok`, no `# SKIP`, no `# TODO`.
  for (const marker of [NOT_OK_MARKER, SKIP_MARKER, TODO_MARKER]) {
    assert.ok(
      !run.stdout.includes(marker),
      `a filter matching nothing prints no '${marker}' on node ${NODE_VERSION} — that is why the marker rule alone is not enough`,
    )
  }
  assert.ok(
    tapCount(run.stdout, TESTS_PREFIX) > 0,
    `a filter matching nothing still reports a non-zero '${TESTS_PREFIX}<n>' on node ${NODE_VERSION} — the file is counted as the test`,
  )
  // The one and only tell: the reader's own test names are absent.
  for (const name of FILTER_FIXTURE_TEST_NAMES) {
    assert.ok(
      !run.stdout.includes(`- ${name}`),
      `the filtered-out test '${name}' must not appear on node ${NODE_VERSION}`,
    )
  }
  assert.match(
    run.stdout,
    /^ok \d+ - \S*fixture\.test\.mjs$/m,
    `expected node to report the file path itself as a passing test on node ${NODE_VERSION} — the absence of your own test names is the only signal. ${FLOOR_ADVICE}`,
  )
})

test('node-test verification: a partial filter drops a test with no counter and no marker', () => {
  const run = RUNS.partialFilter
  assertTapStdout(run, 'partial-filter')
  assert.equal(
    run.code,
    0,
    `a partial filter still exits 0 on node ${NODE_VERSION}; got ${run.code}`,
  )
  for (const prefix of [FAIL_PREFIX, SKIPPED_PREFIX, TODO_PREFIX]) {
    assert.equal(
      tapCount(run.stdout, prefix),
      0,
      `a filtered-out test is absent from '${prefix}<n>' on node ${NODE_VERSION} — no counter records it`,
    )
  }
  for (const marker of [NOT_OK_MARKER, SKIP_MARKER, TODO_MARKER]) {
    assert.ok(
      !run.stdout.includes(marker),
      `a filtered-out test leaves no '${marker}' on node ${NODE_VERSION}`,
    )
  }
  // A real test name IS present, so "some names are visible" is satisfied...
  assert.ok(
    run.stdout.includes(PARTIAL_KEPT_NAME),
    `the kept test must still be named on node ${NODE_VERSION}`,
  )
  // ...and the file-path tell does not fire either, because a test did run.
  assert.doesNotMatch(
    run.stdout,
    /^ok \d+ - \S*fixture\.test\.mjs$/m,
    `a partial filter prints no file-path line on node ${NODE_VERSION} — that tell only fires when nothing matched`,
  )
  // Only "every name you expected" catches it.
  assert.ok(
    !run.stdout.includes(PARTIAL_DROPPED_NAME),
    `the filtered-out test '${PARTIAL_DROPPED_NAME}' must be absent on node ${NODE_VERSION} — its absence is the only signal. ${FLOOR_ADVICE}`,
  )
})

test('node-test verification: NODE_OPTIONS pins the reporter with no flag in argv', () => {
  // Without this leg the bullet's `NODE_OPTIONS` literal has no behaviour behind
  // it, and a rewrite to "unset NODE_OPTIONS" would still satisfy the prose half
  // — a check that passes for the wrong reason, which is the defect this file
  // exists to prevent.
  assertTapStdout(RUNS.envPinned, 'env-pinned')
  assert.equal(
    RUNS.envPinned.code,
    0,
    `the env-pinned all-pass run must exit 0 on node ${NODE_VERSION}; got ${RUNS.envPinned.code}`,
  )
  assert.equal(
    tapCount(RUNS.envPinned.stdout, PASS_PREFIX),
    ALL_PASS_COUNT,
    `${NODE_OPTIONS_TOKEN}=${PINNED_FLAG} must produce the same '${PASS_PREFIX}${ALL_PASS_COUNT}' as the flag form on node ${NODE_VERSION}. ${FLOOR_ADVICE}`,
  )
  // The control. Without it the assertion above is satisfied by node 22's default
  // reporter, which is already TAP — it would pass with NODE_OPTIONS unset.
  assert.ok(
    !RUNS.envDot.stdout.includes(TAP_HEADER),
    `${NODE_OPTIONS_TOKEN}=--test-reporter=dot must NOT produce '${TAP_HEADER}' on node ${NODE_VERSION}; without this control the tap assertion above rides on the default reporter and proves nothing`,
  )
  assert.ok(
    RUNS.envDot.stdout.length > 0,
    `the dot-reporter control wrote nothing on node ${NODE_VERSION}, so its negative assertion is vacuous. ${diagnose(RUNS.envDot)}`,
  )
})

test('node-test verification: --test-skip-pattern drops a test the same silent way', () => {
  // The bullet names this flag, so it must be measured and not merely asserted.
  assertTapStdout(RUNS.skipPattern, 'skip-pattern')
  assert.equal(
    RUNS.skipPattern.code,
    0,
    `--test-skip-pattern still exits 0 on node ${NODE_VERSION}; got ${RUNS.skipPattern.code}`,
  )
  assert.ok(
    !RUNS.skipPattern.stdout.includes(PARTIAL_DROPPED_NAME),
    `--test-skip-pattern must remove '${PARTIAL_DROPPED_NAME}' from stdout on node ${NODE_VERSION}. ${FLOOR_ADVICE}`,
  )
  assert.ok(
    RUNS.skipPattern.stdout.includes(PARTIAL_KEPT_NAME),
    `the unfiltered test must still be named on node ${NODE_VERSION}`,
  )
})

// ---------------------------------------------------------------------------
// Prose: CLAUDE.md's bullet, sliced by anchor
// ---------------------------------------------------------------------------

test("node-test verification: CLAUDE.md's bullet carries every required literal", () => {
  const bullet = bulletText()
  const required = [
    PINNED_FLAG,
    NODE_OPTIONS_TOKEN,
    PASS_PREFIX,
    FAIL_PREFIX,
    TESTS_PREFIX,
    `${TESTS_PREFIX}0`,
    `${PLAN_PREFIX}0`,
    // Without these the suite-level rule is satisfied by a test that never ran,
    // by a whole suite that never ran, or by one that ran and threw.
    `${SKIPPED_PREFIX}0`,
    `${TODO_PREFIX}0`,
    SKIP_MARKER,
    TODO_MARKER,
    NOT_OK_MARKER,
    COUNT_PHRASE,
    NAMES_PHRASE,
    UNKNOWN_PHRASE,
    // A reader has to know which switches silently drop tests.
    ...FILTER_FLAGS,
  ]
  for (const literal of required) {
    assert.match(
      bullet,
      phraseRegExp(literal),
      `CLAUDE.md's '${BULLET_ANCHOR}' bullet no longer contains the required literal '${literal}'`,
    )
  }
})

test('node-test verification: the retired reporter claim stays out of CLAUDE.md', () => {
  // The direction routinely skipped: proving the gate catches an addition, not
  // only a deletion.
  assert.doesNotMatch(
    bulletText(),
    phraseRegExp(RETIRED_CLAIM),
    `CLAUDE.md's bullet contains the retired claim '${RETIRED_CLAIM}' again — node's default reporter is version- and TTY-dependent, so pin ${PINNED_FLAG} instead of asserting which tokens appear`,
  )
})

test('node-test verification: every prefix CLAUDE.md documents was observed in a real run', () => {
  const bullet = bulletText()
  const observed = [
    { prefix: PASS_PREFIX, run: RUNS.allPass, label: 'all-pass' },
    { prefix: FAIL_PREFIX, run: RUNS.failing, label: 'deliberate-failure' },
    { prefix: TESTS_PREFIX, run: RUNS.zeroMatch, label: 'zero-match' },
    { prefix: PLAN_PREFIX, run: RUNS.zeroMatch, label: 'zero-match' },
    { prefix: SKIPPED_PREFIX, run: RUNS.skipped, label: 'skipped' },
    { prefix: TODO_PREFIX, run: RUNS.failingTodo, label: 'failing-todo' },
  ]
  for (const { prefix, run, label } of observed) {
    assert.notEqual(
      tapCount(run.stdout, prefix),
      null,
      `the ${label} run printed no '${prefix}<n>' line on node ${NODE_VERSION}, so CLAUDE.md documenting '${prefix}' would be unbacked`,
    )
    assert.match(
      bullet,
      phraseRegExp(prefix),
      `a real run prints '${prefix}<n>' but CLAUDE.md's bullet no longer documents '${prefix}'`,
    )
  }
})

test('node-test verification: the gh pr checks half of the same bullet is untouched', () => {
  assert.match(
    bulletText(),
    phraseRegExp('gh pr checks <n> --json name,state'),
    `CLAUDE.md's '${BULLET_ANCHOR}' bullet lost its 'gh pr checks <n> --json name,state' half, which BOS-862 left deliberately unchanged`,
  )
})
