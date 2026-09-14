#!/usr/bin/env node

// Non-vacuity is the point of this suite (BOS-1252 Requirement 6): a gate that only ever runs on a
// clean tree proves nothing, so every violating fixture below asserts that the gate actually fires,
// and the fail-closed fixtures assert that an input the gate cannot decide reports rather than
// passes silently.

import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { test } from 'node:test'

import {
  assertionForm,
  checkTimingBounds,
  classifyLimit,
  defaultIsTestSource,
  scanTimingBounds,
  stripCommentsAndStrings,
} from './check-timing-bounds.mjs'

function fixtureRoot(files) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'check-timing-bounds-'))
  for (const [name, contents] of Object.entries(files)) {
    const full = path.join(root, name)
    fs.mkdirSync(path.dirname(full), { recursive: true })
    fs.writeFileSync(full, contents)
  }
  return root
}

function scanFixture(files, options = {}) {
  return scanTimingBounds({ roots: [fixtureRoot(files)], ...options })
}

function goTest(body) {
  return ['package pkg', '', 'func TestThing(t *testing.T) {', ...body, '}', ''].join('\n')
}

function silenced(fn) {
  const log = console.log
  const error = console.error
  console.log = () => {}
  console.error = () => {}
  try {
    return fn()
  } finally {
    console.log = log
    console.error = error
  }
}

// --- Go: the fail-when-too-slow bare-literal shapes the gate must catch -----------------------

test('Go bare unit literal upper bound fires', () => {
  const violations = scanFixture({
    'pkg/thing_test.go': goTest(['\tif elapsed > time.Second {', '\t\tt.Fatalf("slow")', '\t}']),
  })
  assert.equal(violations.length, 1)
  assert.equal(violations[0].kind, 'bare-literal-bound')
  assert.equal(violations[0].line, 4)
})

test('Go scaled unit literal upper bound fires', () => {
  const violations = scanFixture({
    'pkg/thing_test.go': goTest([
      '\tif elapsed >= 250*time.Millisecond {',
      '\t\tt.Fatal("slow")',
      '\t}',
    ]),
  })
  assert.equal(violations.length, 1)
  assert.equal(violations[0].kind, 'bare-literal-bound')
})

test('Go inline time.Since assignment form fires', () => {
  const violations = scanFixture({
    'pkg/thing_test.go': goTest([
      '\tif elapsed := time.Since(start); elapsed > 2*time.Second {',
      '\t\tt.Fatal("slow")',
      '\t}',
    ]),
  })
  assert.equal(violations.length, 1)
  assert.equal(violations[0].kind, 'bare-literal-bound')
})

test('Go literal on the left of elapsed fires, because the direction is the same', () => {
  const violations = scanFixture({
    'pkg/thing_test.go': goTest(['\tif time.Second < elapsed {', '\t\tt.Fatal("slow")', '\t}']),
  })
  assert.equal(violations.length, 1)
  assert.equal(violations[0].kind, 'bare-literal-bound')
})

test('a compound line carrying both directions fires once, on the upper half only', () => {
  const violations = scanFixture({
    'pkg/thing_test.go': goTest([
      '\tif elapsed > 500*time.Millisecond || elapsed < 50*time.Millisecond {',
      '\t\tt.Fatal("out of band")',
      '\t}',
    ]),
  })
  assert.equal(violations.length, 1)
  assert.match(violations[0].message, /500\*time\.Millisecond/)
})

// --- Go: the shapes that are not defects ------------------------------------------------------

test('a bound derived from a named budget passes', () => {
  assert.deepEqual(
    scanFixture({
      'pkg/thing_test.go': goTest(['\tif elapsed > 2*modalBudget {', '\t\tt.Fatal("slow")', '\t}']),
    }),
    [],
  )
})

test('a bound derived from a struct field passes', () => {
  assert.deepEqual(
    scanFixture({
      'pkg/thing_test.go': goTest([
        '\tif elapsed > tt.wantMaxTime {',
        '\t\tt.Fatal("slow")',
        '\t}',
      ]),
    }),
    [],
  )
})

test('a fail-when-too-fast literal bound passes, because load only pushes it further into passing', () => {
  assert.deepEqual(
    scanFixture({
      'pkg/thing_test.go': goTest([
        '\tif elapsed < 100*time.Millisecond {',
        '\t\tt.Fatal("returned early")',
        '\t}',
      ]),
    }),
    [],
  )
})

test('elapsed appearing only in a comment or a message string passes', () => {
  assert.deepEqual(
    scanFixture({
      'pkg/thing_test.go': goTest([
        '\t// The JS sibling spells this elapsed > 4000.',
        '\tt.Logf("elapsed > %v is fine", d)',
      ]),
    }),
    [],
  )
})

// --- The named-excuse annotation --------------------------------------------------------------

test('an annotation with a reason on the asserting line excuses the bound', () => {
  assert.deepEqual(
    scanFixture({
      'pkg/thing_test.go': goTest([
        '\tif elapsed > 6*time.Second { // timing-bound: bounds subprocess spawn + first-line read',
        '\t\tt.Fatal("slow")',
        '\t}',
      ]),
    }),
    [],
  )
})

test('an annotation with a reason on the line above excuses the bound', () => {
  assert.deepEqual(
    scanFixture({
      'pkg/thing_test.go': goTest([
        '\t// timing-bound: bounds a TCP connect to a loopback listener; no budget is in scope',
        '\tif elapsed > 6*time.Second {',
        '\t\tt.Fatal("slow")',
        '\t}',
      ]),
    }),
    [],
  )
})

test('an annotation two lines above does not reach the bound', () => {
  const violations = scanFixture({
    'pkg/thing_test.go': goTest([
      '\t// timing-bound: too far away to excuse anything',
      '\tstart := time.Now()',
      '\tif elapsed > 6*time.Second {',
      '\t\tt.Fatal("slow")',
      '\t}',
    ]),
  })
  assert.equal(violations.length, 1)
  assert.equal(violations[0].kind, 'bare-literal-bound')
})

// --- JS ---------------------------------------------------------------------------------------

// The limit is interpolated rather than written out, so this file does not itself read as a source
// carrying the very assertion shape the gate rejects. The fixture the gate scans is fully rendered.
const jsUpperBound = (limit) => `assert.ok(elapsed < ${limit}, \`too slow: \${elapsed}\`)\n`

test('JS bare numeric literal upper bound fires', () => {
  const violations = scanFixture({ 'pkg/thing.test.mjs': jsUpperBound('3_000') })
  assert.equal(violations.length, 1)
  assert.equal(violations[0].kind, 'bare-literal-bound')
  assert.match(violations[0].message, /3_000/)
})

test('JS bound derived from the budget the test handed the runner passes', () => {
  assert.deepEqual(scanFixture({ 'pkg/thing.test.mjs': jsUpperBound('timeoutMs * 3') }), [])
})

test('JS fail-when-too-fast bound passes', () => {
  assert.deepEqual(
    scanFixture({ 'pkg/thing.test.mjs': 'assert.ok(elapsed > 4_000, "too fast")\n' }),
    [],
  )
})

// --- Direction comes from the assertion form, not the file's language ---------------------------

test('a JS failure-condition bound fires, the shape a language-keyed direction inverted away', () => {
  const violations = scanFixture({
    'pkg/thing.test.mjs': 'if (elapsed > 5_000) throw new Error(`too slow`)\n',
  })
  assert.equal(violations.length, 1)
  assert.equal(violations[0].kind, 'bare-literal-bound')
  assert.match(violations[0].message, /5_000/)
})

test('a JS value-range comparison is not an elapsed-time bound and does not fire', () => {
  assert.deepEqual(
    scanFixture({ 'pkg/thing.test.mjs': 'const slow = rows.filter((r) => r.duration < 5)\n' }),
    [],
  )
})

test('a JS failure condition in the too-fast direction still passes', () => {
  assert.deepEqual(
    scanFixture({ 'pkg/thing.test.mjs': 'if (elapsed < 5_000) throw new Error(`too fast`)\n' }),
    [],
  )
})

test('assertionForm names the construct a comparison sits in', () => {
  const at = (source, needle) => assertionForm(source, source.indexOf(needle))
  // Interpolated for the same reason as `jsUpperBound` above: spelled out, this line would itself
  // read as a source carrying the assertion shape the gate rejects. The string `at` sees is identical.
  const limit = 3000
  assert.equal(at(`assert.ok(elapsed < ${limit})`, '<'), 'assertion')
  assert.equal(at('if (elapsed > 3000) throw e', '>'), 'condition')
  assert.equal(at('\tif elapsed > 3000 {', '>'), 'condition')
  assert.equal(at('rows.filter((r) => r.duration < 5)', '< 5'), 'none')
  assert.equal(at('const n = elapsed < 5', '<'), 'none')
})

// --- Comparisons wrapped across a line break ----------------------------------------------------

test('a Go reversed bound wrapped onto the next line still fires', () => {
  const violations = scanFixture({
    'pkg/thing_test.go': goTest([
      '\tif time.Second <',
      '\t\telapsed {',
      '\t\tt.Fatal("slow")',
      '\t}',
    ]),
  })
  assert.equal(violations.length, 1)
  assert.equal(violations[0].kind, 'bare-literal-bound')
})

test('a JS reversed bound wrapped onto the next line still fires, as the literal it is', () => {
  const violations = scanFixture({
    'pkg/thing.test.mjs': 'assert.ok(3_000 >\n  elapsed, `too slow`)\n',
  })
  assert.equal(violations.length, 1)
  assert.equal(violations[0].kind, 'bare-literal-bound')
  assert.match(violations[0].message, /3_000/)
})

test('a reversed bound derived from a budget is still clean when wrapped', () => {
  assert.deepEqual(
    scanFixture({ 'pkg/thing.test.mjs': 'assert.ok(timeoutMs * 3 >\n  elapsed)\n' }),
    [],
  )
})

// --- Fail-closed obligations (Requirement 2) ---------------------------------------------------

test('a walked file with no registered analyzer reports rather than being skipped', () => {
  const violations = scanFixture(
    { 'pkg/thing_test.py': 'assert elapsed < 3000\n' },
    { isTestSource: (name) => /(_test|\.test)\./.test(name) },
  )
  assert.equal(violations.length, 1)
  assert.equal(violations[0].kind, 'unrecognised-file')
  assert.match(violations[0].message, /\.py/)
})

test('a comparison operand the gate cannot classify reports rather than passing', () => {
  const violations = scanFixture({
    'pkg/thing_test.go': goTest(['\tif elapsed > budgets["slow"] {', '\t\tt.Fatal("slow")', '\t}']),
  })
  assert.equal(violations.length, 1)
  assert.equal(violations[0].kind, 'unclassifiable-operand')
})

test('a root the gate cannot list reports rather than exiting 0 on an empty scan', () => {
  const missing = path.join(os.tmpdir(), `check-timing-bounds-absent-${process.pid}-${Date.now()}`)
  const violations = scanTimingBounds({ roots: [missing] })
  assert.equal(violations.length, 1)
  assert.equal(violations[0].kind, 'unreadable-directory')
  assert.match(violations[0].message, /ENOENT/)
  // And the exit path agrees: a scan that read nothing must not report the tree clean.
  assert.equal(
    silenced(() => checkTimingBounds({ roots: [missing] })),
    false,
  )
})

test('a root that is a file rather than a directory reports', () => {
  const root = fixtureRoot({
    'pkg/thing_test.go': goTest(['\tif elapsed > 2*modalBudget {', '\t}']),
  })
  const violations = scanTimingBounds({ roots: [path.join(root, 'pkg/thing_test.go')] })
  assert.equal(violations.length, 1)
  assert.equal(violations[0].kind, 'unreadable-directory')
  assert.match(violations[0].message, /ENOTDIR/)
})

test('an annotation with an empty reason reports', () => {
  const violations = scanFixture({
    'pkg/thing_test.go': goTest([
      '\t// timing-bound:',
      '\tif elapsed > 6*time.Second {',
      '\t\tt.Fatal("slow")',
      '\t}',
    ]),
  })
  assert.ok(violations.some((violation) => violation.kind === 'empty-annotation'))
})

test('an annotation whose reason is only whitespace reports', () => {
  const violations = scanFixture({
    'pkg/thing_test.go': goTest([
      '\t// timing-bound:    ',
      '\tif elapsed > 6*time.Second {',
      '\t\tt.Fatal("slow")',
      '\t}',
    ]),
  })
  assert.ok(violations.some((violation) => violation.kind === 'empty-annotation'))
})

// --- Corpus and helper units ------------------------------------------------------------------

test('the default corpus is exactly *_test.go and *.test.mjs', () => {
  assert.equal(defaultIsTestSource('thing_test.go'), true)
  assert.equal(defaultIsTestSource('thing.test.mjs'), true)
  assert.equal(defaultIsTestSource('thing.go'), false)
  assert.equal(defaultIsTestSource('thing.test.ts'), false)
  assert.equal(defaultIsTestSource('thing_test.py'), false)
})

test('classifyLimit separates literals, derived bounds and undecidable operands', () => {
  assert.equal(classifyLimit('2*time.Second', { language: 'go' }).kind, 'literal')
  assert.equal(classifyLimit('time.Millisecond', { language: 'go' }).kind, 'literal')
  assert.equal(classifyLimit('2*modalBudget', { language: 'go' }).kind, 'derived')
  assert.equal(classifyLimit('attemptBudget/2', { language: 'go' }).kind, 'derived')
  assert.equal(classifyLimit('3_000', { language: 'js' }).kind, 'literal')
  assert.equal(classifyLimit('timeoutMs * 3', { language: 'js' }).kind, 'derived')
  assert.equal(classifyLimit('', { language: 'go' }).kind, 'unclassifiable')
})

test('stripCommentsAndStrings blanks comments and strings while preserving line positions', () => {
  const source = 'a := "elapsed > 5" // elapsed > 5\nb := 1\n'
  const stripped = stripCommentsAndStrings(source)
  assert.equal(stripped.split('\n').length, source.split('\n').length)
  assert.equal(stripped.includes('elapsed'), false)
  assert.equal(stripped.includes('b := 1'), true)
})

// --- Exit-code behaviour ------------------------------------------------------------------------

test('checkTimingBounds reports false on a violating tree and true on a clean one', () => {
  const violating = fixtureRoot({
    'pkg/thing_test.go': goTest(['\tif elapsed > time.Second {', '\t\tt.Fatal("slow")', '\t}']),
  })
  const clean = fixtureRoot({
    'pkg/thing_test.go': goTest(['\tif elapsed > 2*modalBudget {', '\t\tt.Fatal("slow")', '\t}']),
  })
  assert.equal(
    silenced(() => checkTimingBounds({ roots: [violating] })),
    false,
  )
  assert.equal(
    silenced(() => checkTimingBounds({ roots: [clean] })),
    true,
  )
})
