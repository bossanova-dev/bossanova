#!/usr/bin/env node

import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { test } from 'node:test'

import {
  GATE_FILE_GLOB,
  checkProsePinWhitespace,
  discoverGateFiles,
  findRegexLiterals,
  literalSpaceRuns,
  scanProsePinWhitespace,
} from './check-prose-pin-whitespace.mjs'

const REPO_ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')

// Fixtures are assembled from single-quoted lines, never written as regex
// literals in this file. A fixture for "a regex literal with a literal space"
// written AS a regex literal would be an offender in this very file — and while
// this file is not in the gate's own file set today, that is an accident of its
// name, not a property worth depending on.
const fixture = (...lines) => lines.join('\n') + '\n'

// Capture what the gate prints so a test can assert on the text a contributor
// actually sees, not just the boolean.
function captureConsole(run) {
  const out = []
  const err = []
  const originalLog = console.log
  const originalError = console.error
  console.log = (...args) => out.push(args.join(' '))
  console.error = (...args) => err.push(args.join(' '))
  try {
    const result = run()
    return { result, out, err }
  } finally {
    console.log = originalLog
    console.error = originalError
  }
}

function makeTempRepo() {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'check-prose-pin-whitespace-'))
  fs.mkdirSync(path.join(root, 'scripts'))
  return root
}

function writeScript(repoRoot, name, contents) {
  fs.writeFileSync(path.join(repoRoot, 'scripts', name), contents)
}

// ---------------------------------------------------------------------------
// The rule itself
// ---------------------------------------------------------------------------

test('flags a literal-space multi-word regex literal', () => {
  const { findings, desync } = scanProsePinWhitespace(
    fixture('assert.match(body, /is the only proof channel/)'),
  )

  assert.equal(desync, null)
  assert.deepEqual(findings, [{ line: 1, pattern: '/is the only proof channel/' }])
})

test('does not flag a pin whose inter-word gaps are already \\s+', () => {
  const { findings, desync } = scanProsePinWhitespace(
    fixture('assert.match(body, /revert\\s+to\\s+Opus/)'),
  )

  assert.equal(desync, null)
  assert.deepEqual(findings, [])
})

test('does not flag a regex with no inter-word space at all', () => {
  const { findings, desync } = scanProsePinWhitespace(
    fixture('assert.match(body, /references\\//)'),
  )

  assert.equal(desync, null)
  assert.deepEqual(findings, [])
})

test('flags a run of more than one literal space between two words', () => {
  // A per-character rule misses this one: with two spaces, neither space has a
  // word character on BOTH sides, so the defect reads as clean.
  const { findings } = scanProsePinWhitespace(fixture('assert.match(body, /alpha  beta/)'))

  assert.deepEqual(findings, [{ line: 1, pattern: '/alpha  beta/' }])
})

test('does not flag a space that is not flanked by word characters on both sides', () => {
  const { findings } = scanProsePinWhitespace(fixture('assert.match(body, /alpha - beta/)'))

  assert.deepEqual(findings, [])
})

test('does not flag a space inside a character class', () => {
  // `/a[ ]b/` is the documented spelling for a pin that must stay on one line.
  // Rewriting it to \s+ would change what the author asked for, and rewriting
  // the space INSIDE the class would break the class outright.
  const { findings } = scanProsePinWhitespace(fixture('assert.match(body, /alpha[ ]beta/)'))

  assert.deepEqual(findings, [])
})

test('does not flag a backslash-escaped space', () => {
  const { findings } = scanProsePinWhitespace(fixture('assert.match(body, /alpha\\ beta/)'))

  assert.deepEqual(findings, [])
})

test('reports every offending literal on its own line, once per literal', () => {
  const { findings } = scanProsePinWhitespace(
    fixture(
      'const first = /one two/',
      'const clean = /three\\s+four/',
      'const third = /five six seven/',
    ),
  )

  assert.deepEqual(findings, [
    { line: 1, pattern: '/one two/' },
    { line: 3, pattern: '/five six seven/' },
  ])
})

test('keeps regex flags in the reported pattern', () => {
  const { findings } = scanProsePinWhitespace(fixture('assert.match(body, /never green/i)'))

  assert.deepEqual(findings, [{ line: 1, pattern: '/never green/i' }])
})

// ---------------------------------------------------------------------------
// The opt-out marker
// ---------------------------------------------------------------------------

test('honours the opt-out marker on the offending line', () => {
  const { findings, marked } = scanProsePinWhitespace(
    fixture('assert.match(body, /make lint/) // prose-pin: literal-space ok'),
  )

  assert.deepEqual(findings, [])
  assert.equal(marked, 1)
})

test('honours the opt-out marker on the line immediately above', () => {
  const { findings, marked } = scanProsePinWhitespace(
    fixture('// prose-pin: literal-space ok', 'assert.match(body, /make lint/)'),
  )

  assert.deepEqual(findings, [])
  assert.equal(marked, 1)
})

test('does not let an opt-out marker two lines above excuse a pin', () => {
  const { findings, marked } = scanProsePinWhitespace(
    fixture('// prose-pin: literal-space ok', '', 'assert.match(body, /make lint/)'),
  )

  assert.deepEqual(findings, [{ line: 3, pattern: '/make lint/' }])
  assert.equal(marked, 0)
})

// ---------------------------------------------------------------------------
// Lexing: a `/` is only sometimes a regex
// ---------------------------------------------------------------------------

test('ignores a path with a space in a line comment', () => {
  const { findings, desync } = scanProsePinWhitespace(
    fixture('// see scripts/foo.mjs and the other thing', 'const value = 1'),
  )

  assert.equal(desync, null)
  assert.deepEqual(findings, [])
})

test('ignores a slash-delimited span inside a block comment', () => {
  const { findings, desync } = scanProsePinWhitespace(
    fixture('/*', ' * a /literal space here/ inside a comment', ' */', 'const value = 1'),
  )

  assert.equal(desync, null)
  assert.deepEqual(findings, [])
})

test('ignores slashes inside a string literal', () => {
  const { findings, desync } = scanProsePinWhitespace(
    fixture("const value = 'a /literal space here/ inside a string'"),
  )

  assert.equal(desync, null)
  assert.deepEqual(findings, [])
})

test('ignores slashes inside a template literal but still lexes its interpolation', () => {
  const { findings, desync } = scanProsePinWhitespace(
    fixture('const value = `a /literal space here/ ${re.test(x)} tail`', 'const other = /one two/'),
  )

  assert.equal(desync, null)
  assert.deepEqual(findings, [{ line: 2, pattern: '/one two/' }])
})

test('reads division as division, not as an unterminated regex', () => {
  const { findings, desync } = scanProsePinWhitespace(
    fixture('const ratio = total / count', 'const pin = /one two/'),
  )

  assert.equal(desync, null)
  assert.deepEqual(findings, [{ line: 2, pattern: '/one two/' }])
})

test('reports a desync and discards findings when a template literal never closes', () => {
  const { findings, desync } = scanProsePinWhitespace(
    fixture('const pin = /one two/', 'const broken = `never closed'),
  )

  assert.notEqual(desync, null)
  assert.deepEqual(findings, [])
})

// ---------------------------------------------------------------------------
// literalSpaceRuns: the offsets the fixer and the gate share
// ---------------------------------------------------------------------------

test('literalSpaceRuns returns the offsets of each offending run', () => {
  assert.deepEqual(literalSpaceRuns('one two'), [{ start: 3, end: 4 }])
  assert.deepEqual(literalSpaceRuns('a  b'), [{ start: 1, end: 3 }])
  assert.deepEqual(literalSpaceRuns('a\\s+b'), [])
  assert.deepEqual(literalSpaceRuns('a[ ]b'), [])
})

// A shebang is not JavaScript. Left in the stream its `/usr/bin` reads as a regex literal with the
// flags `bin`, which produces no false finding here (`usr` has no space) but is still a wrong
// literal handed to anything that shares this scanner — the codemod included.
test('a shebang line yields no regex literal, and does not shift the lines after it', () => {
  const { literals, desync } = findRegexLiterals(
    fixture('#!/usr/bin/env node', '', 'const pin = /one two/'),
  )
  assert.equal(desync, null)
  assert.equal(literals.length, 1)
  assert.equal(literals[0].line, 3)
  assert.equal(literals[0].pattern, '/one two/')
})

// The `n` of `\n` is a word character to a raw character test, but the token is a newline, so the
// indentation after it is not a prose gap. Flagging it once forced `\n  ` to `\n\s+` in a pin on a
// fenced code block, and since `\s` matches a newline that pin stopped proving the lines were
// consecutive. A false positive in this gate does not merely annoy; it launders a weakening.
test('a run whose left neighbour is the tail of an escape is not a prose gap', () => {
  assert.deepEqual(literalSpaceRuns('\\n  heartbeat'), [])
  assert.deepEqual(literalSpaceRuns('\\d 5'), [])
  // Two backslashes: the `n` is a literal `n`, a real word character, so the gap is still reported.
  assert.deepEqual(literalSpaceRuns('\\\\n b'), [{ start: 3, end: 4 }])
  // And an escape elsewhere in the pattern must not suppress a later, genuine gap.
  assert.deepEqual(literalSpaceRuns('\\n one two'), [{ start: 6, end: 7 }])
})

// ---------------------------------------------------------------------------
// The file set is glob-derived
// ---------------------------------------------------------------------------

test('discoverGateFiles picks up a new gate file without the gate being edited', () => {
  const repoRoot = makeTempRepo()
  writeScript(repoRoot, 'brand-new-skill.test.mjs', fixture('const pin = /one two/'))
  writeScript(repoRoot, 'skill-frontmatter.test.mjs', fixture('const pin = /three four/'))
  writeScript(repoRoot, 'unrelated.test.mjs', fixture('const pin = /five six/'))
  writeScript(repoRoot, 'brand-new-skill.mjs', fixture('const pin = /seven eight/'))

  assert.deepEqual(
    discoverGateFiles(repoRoot).map((file) => path.basename(file)),
    ['brand-new-skill.test.mjs', 'skill-frontmatter.test.mjs'],
  )
})

test('the declared glob is the one the file set is derived from', () => {
  assert.equal(GATE_FILE_GLOB, 'scripts/*skill*.test.mjs')
})

test('prettier explicitly preserves markdown prose wrapping', () => {
  const config = JSON.parse(fs.readFileSync(path.join(REPO_ROOT, '.prettierrc'), 'utf8'))

  assert.equal(
    config.proseWrap,
    'preserve',
    'proseWrap must stay preserve: changing it lets prettier reflow prose and invalidates proximity-window prose pins',
  )
})

// ---------------------------------------------------------------------------
// The gate as a contributor meets it
// ---------------------------------------------------------------------------

test('checkProsePinWhitespace fails naming file, line, and the offending pattern', () => {
  const repoRoot = makeTempRepo()
  writeScript(
    repoRoot,
    'demo-skill.test.mjs',
    fixture('const clean = /already\\s+joined/', 'const pin = /is the only proof channel/'),
  )

  const { result, err } = captureConsole(() => checkProsePinWhitespace(repoRoot))

  assert.equal(result, false)
  const joined = err.join('\n')
  assert.match(joined, /scripts\/demo-skill\.test\.mjs:2/)
  assert.match(joined, /is the only proof channel/)
  assert.match(joined, /prose-pin:\s+literal-space\s+ok/)
})

test('checkProsePinWhitespace passes and reports how many files it scanned', () => {
  const repoRoot = makeTempRepo()
  writeScript(repoRoot, 'demo-skill.test.mjs', fixture('const pin = /already\\s+joined/'))

  const { result, out } = captureConsole(() => checkProsePinWhitespace(repoRoot))

  assert.equal(result, true)
  assert.match(out.join('\n'), /1 file\(s\) scanned/)
})

test('checkProsePinWhitespace fails rather than passing vacuously when the glob matches nothing', () => {
  const repoRoot = makeTempRepo()
  writeScript(repoRoot, 'unrelated.test.mjs', fixture('const pin = /one two/'))

  const { result, err } = captureConsole(() => checkProsePinWhitespace(repoRoot))

  assert.equal(result, false)
  assert.match(err.join('\n'), /scripts\/\*skill\*\.test\.mjs/)
})

test('an unreadable gate file fails the gate rather than being skipped past', () => {
  const repoRoot = makeTempRepo()
  writeScript(repoRoot, 'demo-skill.test.mjs', fixture('const pin = /already\\s+joined/'))
  // A directory in the file set's place: readable as an entry, unreadable as a file.
  fs.mkdirSync(path.join(repoRoot, 'scripts', 'broken-skill.test.mjs'))

  const { result, err } = captureConsole(() => checkProsePinWhitespace(repoRoot))

  assert.equal(result, false)
  const joined = err.join('\n')
  assert.match(joined, /broken-skill\.test\.mjs/)
  assert.match(joined, /could\s+not\s+check\s+every\s+file/)
})

test('a desynced gate file fails the gate, so the offender inside it is never passed over', () => {
  const repoRoot = makeTempRepo()
  // One offending pin, then an unterminated template literal that desyncs the lexer. Before the
  // gate failed on skips this returned true: the offender was discarded with the desync and the
  // run printed a green line having scanned nothing.
  writeScript(
    repoRoot,
    'demo-skill.test.mjs',
    fixture('const pin = /one two/\nconst open = `never closed'),
  )

  const { result, err } = captureConsole(() => checkProsePinWhitespace(repoRoot))

  assert.equal(result, false)
  assert.match(err.join('\n'), /template\s+literal\s+never\s+closed/)
})

test('a file set that matches but scans nothing fails rather than reporting a clean run', () => {
  const repoRoot = makeTempRepo()
  // A directory where a gate file should be: `readFileSync` throws EISDIR, so the file is counted
  // as SKIPPED and the skip guard fails the gate. Naming the guard matters — an earlier version of
  // this test claimed the `scanned === 0` tripwire was what stood here, which it never was: the
  // skip guard returns first, and with it every route to zero scanned files is already closed. The
  // assertion below is on the skip guard's own wording so the test cannot quietly drift onto some
  // other failure and still read as covering this one.
  fs.mkdirSync(path.join(repoRoot, 'scripts', 'broken-skill.test.mjs'))

  const { result, err } = captureConsole(() => checkProsePinWhitespace(repoRoot))

  assert.equal(result, false)
  const joined = err.join('\n')
  assert.match(joined, /broken-skill\.test\.mjs/)
  assert.match(joined, /could\s+not\s+check\s+every\s+file/)
  assert.doesNotMatch(joined, /none\s+were\s+scanned/)
})

// ---------------------------------------------------------------------------
// The live tree
// ---------------------------------------------------------------------------

test('the repository tree is clean under this gate', () => {
  const { result } = captureConsole(() => checkProsePinWhitespace(REPO_ROOT))

  assert.equal(result, true)
})
