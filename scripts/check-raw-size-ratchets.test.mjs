#!/usr/bin/env node

// Falsification suite for the raw-size-ratchet gate.
//
// RK4: this gate reports zero matches across its whole scope today, and a zero-match scan is
// exactly what a BROKEN detector also reports. So the positive fixtures below come first and
// carry the weight — each feeds the detector the shape it exists to find, verbatim, and
// requires a hit. The whole-tree clean run is only meaningful once these pass.
//
// This file is the one place that must carry both forbidden shapes literally, which is why it
// is in the gate's own SCAN_EXCLUSIONS.
//
// Node built-ins only — cron worktrees are dependency-free.

import { test } from 'node:test'
import assert from 'node:assert/strict'
import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

import {
  RESIDUAL,
  SCANNED_EXTRA,
  SCANNED_NAME,
  SCAN_EXCLUSIONS,
  findRawSizeRatchets,
  findRawSizeRatchetsInRepo,
  scannedFiles,
} from './check-raw-size-ratchets.mjs'

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')

// ─── Positive fixtures: the detector must FIRE ────────────────────────────────────────────

test('the byte rule fires on the exact shape all nine call sites used', () => {
  const source = [
    "  const bytes = Buffer.byteLength(SKILL, 'utf8')",
    '  assert.ok(bytes <= RATCHET, `too big`)',
  ].join('\n')
  const offenders = findRawSizeRatchets(source)
  assert.equal(offenders.length, 1, 'the hand-rolled byte measurement must be reported')
  assert.equal(offenders[0].rule, 'raw-byte-measure')
  assert.equal(offenders[0].line, 1, 'the reported line must be where the measurement sits')
  assert.match(offenders[0].remedy, /measureFile/, 'the remedy must name the replacement')
})

test('the byte rule fires without the encoding argument', () => {
  // sweep-releases-gate.mjs spells it this way; a rule that only matched the two-argument
  // form would be blind to half the population.
  assert.equal(findRawSizeRatchets('const n = Buffer.byteLength(body)').length, 1)
})

test('the byte rule fires through the whitespace Prettier can introduce', () => {
  assert.equal(findRawSizeRatchets('Buffer . byteLength ( x )').length, 1)
})

test('the line rule fires on the CLAUDE.md line-count idiom', () => {
  const source = "  const lines = text.split('\\n').length - (text.endsWith('\\n') ? 1 : 0)"
  const offenders = findRawSizeRatchets(source)
  assert.equal(offenders.length, 1, 'the hand-rolled line count must be reported')
  assert.equal(offenders[0].rule, 'raw-line-measure')
  assert.match(offenders[0].remedy, /unit: 'lines'/, 'the remedy must name the lines unit')
})

test('the line rule fires for double-quoted and backtick spellings too', () => {
  assert.equal(findRawSizeRatchets('text.split("\\n").length').length, 1)
  assert.equal(findRawSizeRatchets('text.split(`\\n`).length').length, 1)
})

test('both rules fire in one file, reported in source order', () => {
  const source = [
    'const a = Buffer.byteLength(x)',
    'const b = 1',
    "const c = t.split('\\n').length",
  ].join('\n')
  const offenders = findRawSizeRatchets(source)
  assert.deepEqual(
    offenders.map((o) => [o.line, o.rule]),
    [
      [1, 'raw-byte-measure'],
      [3, 'raw-line-measure'],
    ],
  )
})

// ─── Negative fixtures: the detector must NOT fire ────────────────────────────────────────

test('occurrence counting is not a size measurement', () => {
  // `split(needle).length - 1` counts occurrences and is all over the skill tests. Flagging it
  // would make this gate unusable, so the rule is anchored to the newline separator.
  assert.deepEqual(findRawSizeRatchets("const n = body.split('${TRACKER:-').length - 1"), [])
  assert.deepEqual(findRawSizeRatchets('const n = exits.length - 1'), [])
})

test('a same-named property on another receiver is not matched', () => {
  assert.deepEqual(findRawSizeRatchets('const n = myBuffer.byteLength'), [])
  assert.deepEqual(findRawSizeRatchets('const n = view.byteLength'), [])
})

test('the routed helper call is not an offence', () => {
  assert.deepEqual(findRawSizeRatchets("measureFile(abs('../x/SKILL.md'), { unit: 'lines' })"), [])
})

// ─── Opt-out ──────────────────────────────────────────────────────────────────────────────

test('a reasoned opt-out on the line above suppresses the offence', () => {
  const source = [
    '// size-ratchet-ok: asserting what measureFile itself returns',
    'Buffer.byteLength(x)',
  ].join('\n')
  assert.deepEqual(findRawSizeRatchets(source), [])
})

test('a reasoned opt-out on the same line suppresses the offence', () => {
  assert.deepEqual(findRawSizeRatchets('Buffer.byteLength(x) // size-ratchet-ok: fixture text'), [])
})

test('an opt-out with no reason does NOT suppress — an unexplained hatch is the next leak', () => {
  const source = ['// size-ratchet-ok:', 'Buffer.byteLength(x)'].join('\n')
  assert.equal(findRawSizeRatchets(source).length, 1)
})

test('a marker inside a string literal does NOT suppress', () => {
  // Otherwise `const s = '// size-ratchet-ok: x'` silences an offence with nothing a reviewer
  // would ever read as an opt-out. Shares check-vacuous-regions' scanner rather than forking it.
  const source = [
    "const s = '// size-ratchet-ok: not a real comment'",
    'Buffer.byteLength(x)',
  ].join('\n')
  assert.equal(findRawSizeRatchets(source).length, 1)
})

test('an opt-out three lines above is out of range', () => {
  const source = ['// size-ratchet-ok: too far away', '', '', 'Buffer.byteLength(x)'].join('\n')
  assert.equal(findRawSizeRatchets(source).length, 1)
})

// ─── Scope ────────────────────────────────────────────────────────────────────────────────

test('the scan covers every converted call site', () => {
  const files = scannedFiles(repoRoot).map((f) => path.relative(repoRoot, f))
  for (const converted of [
    'scripts/boss-skill.test.mjs',
    'scripts/boss-build-skill.test.mjs',
    'scripts/bs-plan-skill.test.mjs',
    'scripts/bs-epic-skill.test.mjs',
    'scripts/bs-sweep-tests-skill.test.mjs',
    'scripts/bs-sweep-debt-skill.test.mjs',
    'scripts/bs-sweep-mutation-skill.test.mjs',
    'scripts/bs-sweep-prettify-skill.test.mjs',
    'scripts/check-agent-test-guidance.test.mjs',
  ]) {
    assert.ok(files.includes(converted), `${converted} must be in scope`)
  }
})

test('the scan is not vacuous: it looks at a non-trivial number of real files', () => {
  const files = scannedFiles(repoRoot)
  assert.ok(files.length >= 15, `expected the scope to hold real files, got ${files.length}`)
  for (const file of files) {
    assert.ok(fs.existsSync(file), `${file} must exist`)
  }
})

test('this suite and the lib suite are excluded, so their fixtures cannot red the gate', () => {
  const files = scannedFiles(repoRoot).map((f) => path.relative(repoRoot, f))
  for (const excluded of SCAN_EXCLUSIONS) {
    assert.ok(!files.includes(excluded), `${excluded} must be excluded`)
  }
})

test('the named extras are real files, not a stale list', () => {
  for (const extra of SCANNED_EXTRA) {
    assert.ok(fs.existsSync(path.join(repoRoot, extra)), `${extra} must exist`)
  }
})

test('the name rule matches skill test files and nothing else', () => {
  assert.ok(SCANNED_NAME.test('bs-sweep-tests-skill.test.mjs'))
  assert.ok(!SCANNED_NAME.test('size-ratchet-lib.test.mjs'))
  assert.ok(!SCANNED_NAME.test('bs-sweep-tests-skill.mjs'))
})

// ─── The whole-tree verdict, which only counts because the fixtures above passed ──────────

test('the repository is clean of hand-rolled size measurements in scope', () => {
  const offenders = findRawSizeRatchetsInRepo(repoRoot)
  assert.deepEqual(
    offenders.map((o) => `${path.relative(repoRoot, o.file)}:${o.line} [${o.rule}]`),
    [],
  )
})

test('the gate states what a green run does not establish', () => {
  assert.match(RESIDUAL, /not that any pin is correct/)
  assert.match(RESIDUAL, /outside SCANNED_NAME/)
  // The likeliest way past this gate is not an opt-out, it is writing the measurement a
  // different way. The residual has to name that, or the success line reads as "nothing
  // measures by hand here" when it only means "neither of two spellings appears here".
  assert.match(RESIDUAL, /TWO SPELLINGS/)
  assert.match(RESIDUAL, /statSync\(\)\.size/)
  assert.match(RESIDUAL, /readFileSync\(\)\.length/)
})
