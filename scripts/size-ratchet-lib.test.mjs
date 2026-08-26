// Unit tests for scripts/size-ratchet-lib.mjs (BOS-768).
//
// These assert on failure-message CONTENT, not merely on throw/no-throw. The helper's entire
// value is its text: a two-sided pin that reds without naming the measured value, the constant
// to repin, the commit the repin has to land in, and the check's own blind spot leaves the
// reader guessing, and the wrong guess re-spends a banked saving. A throw/no-throw suite would
// stay green through every one of those regressions.
//
// Node built-ins only — cron worktrees are dependency-free.

import { test } from 'node:test'
import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'

import {
  MOVE_TO_REFERENCE_REMEDY,
  NO_REFERENCE_REMEDY,
  assertArtifactSet,
  assertExactSize,
  assertMirrorRegenerated,
  measureFile,
} from './size-ratchet-lib.mjs'

const RESIDUAL = 'a restructure landing on the identical byte count'

const base = (overrides = {}) => ({
  label: 'resident body ratchet',
  path: 'skills/demo/SKILL.md',
  measured: 100,
  expected: 100,
  constName: 'RATCHET',
  constFile: 'scripts/demo-skill.test.mjs',
  residual: RESIDUAL,
  ...overrides,
})

const messageOf = (fn) => {
  try {
    fn()
  } catch (error) {
    return error.message
  }
  return null
}

// ── assertExactSize: the exact-match happy path ────────────────────────────────────────────

test('assertExactSize passes when the measurement equals the pin', () => {
  assert.doesNotThrow(() => assertExactSize(base()))
})

// ── assertExactSize: the OVER branch ───────────────────────────────────────────────────────

test('assertExactSize over the pin names measurement, pin, constant and file', () => {
  const message = messageOf(() => assertExactSize(base({ measured: 137 })))
  assert.ok(message, 'a measurement over the pin must throw')
  assert.match(message, /137/, 'the measured value must be in the message')
  assert.match(message, /100/, 'the pinned value must be in the message')
  assert.match(message, /RATCHET/, 'constName must be in the message')
  assert.match(message, /scripts\/demo-skill\.test\.mjs/, 'constFile must be in the message')
  assert.match(message, /skills\/demo\/SKILL\.md/, 'the artifact path must be in the message')
  assert.match(message, /over by 37/, 'the message must state how far over it is')
})

test('assertExactSize over the pin says the ratchet only moves DOWN', () => {
  const message = messageOf(() => assertExactSize(base({ measured: 101 })))
  assert.match(message, /only moves DOWN/)
})

test('assertExactSize over the pin demands the repin land in the same commit', () => {
  const message = messageOf(() => assertExactSize(base({ measured: 101 })))
  assert.match(message, /SAME commit that changed skills\/demo\/SKILL\.md/)
})

test('assertExactSize over the pin carries the default move-to-a-reference remedy', () => {
  const message = messageOf(() => assertExactSize(base({ measured: 101 })))
  assert.ok(
    message.includes(MOVE_TO_REFERENCE_REMEDY),
    'the default remedy points at extraction into a reference',
  )
})

test('assertExactSize accepts a remedy override for a skill with no references directory', () => {
  const message = messageOf(() =>
    assertExactSize(base({ measured: 101, remedy: NO_REFERENCE_REMEDY })),
  )
  assert.ok(message.includes(NO_REFERENCE_REMEDY), 'the override must reach the message')
  assert.ok(
    !message.includes(MOVE_TO_REFERENCE_REMEDY),
    'an artifact with nowhere to extract to must not be told to extract',
  )
})

// ── assertExactSize: the UNDER branch ──────────────────────────────────────────────────────

test('assertExactSize under the pin says the reduction was never banked', () => {
  const message = messageOf(() => assertExactSize(base({ measured: 91 })))
  assert.ok(message, 'a measurement under the pin must throw — that is the whole point')
  assert.match(message, /never banked/)
  assert.match(message, /silent headroom/)
  assert.match(message, /under by 9/)
})

test('assertExactSize under the pin routes an inherited red at origin/main', () => {
  const message = messageOf(() => assertExactSize(base({ measured: 91 })))
  assert.match(message, /origin\/main/)
  assert.match(message, /If you did not touch skills\/demo\/SKILL\.md/)
})

test('assertExactSize under the pin names the exact value to write', () => {
  const message = messageOf(() => assertExactSize(base({ measured: 91 })))
  assert.match(message, /Repin RATCHET to 91 in scripts\/demo-skill\.test\.mjs/)
})

// ── assertExactSize: residual is required and terminal ─────────────────────────────────────

test('both directions end with the stated residual', () => {
  for (const measured of [101, 91]) {
    const message = messageOf(() => assertExactSize(base({ measured })))
    assert.ok(
      message.endsWith(`Not covered by this check: ${RESIDUAL}.`),
      `the ${measured > 100 ? 'over' : 'under'} message must end with the residual`,
    )
  }
})

test('omitting residual throws a distinct blind-spot error, even on a passing measurement', () => {
  const { residual, ...withoutResidual } = base()
  assert.equal(typeof residual, 'string')
  const message = messageOf(() => assertExactSize(withoutResidual))
  assert.match(message, /must state its blind spot/)
  assert.match(message, /wiring error in the gate, not a finding about the artifact/)
  assert.ok(!message.includes('only moves DOWN'), 'this is not a size finding')
})

test('an empty residual is rejected the same way as a missing one', () => {
  const message = messageOf(() => assertExactSize(base({ residual: '   ' })))
  assert.match(message, /must state its blind spot/)
})

// ── assertExactSize: the `below` dual-reading bound ────────────────────────────────────────

test('a satisfied below bound does not throw', () => {
  assert.doesNotThrow(() =>
    assertExactSize(base({ below: { name: 'PRE_EXTRACTION_BASELINE', value: 120 } })),
  )
})

test('assertExactSize derives the recorded re-baseline delta from previous', () => {
  assert.doesNotThrow(() =>
    assertExactSize(base({ expected: 96, measured: 96, previous: { value: 100, delta: -4 } })),
  )
})

test('assertExactSize reds when the recorded re-baseline delta lies', () => {
  const message = messageOf(() =>
    assertExactSize(
      base({
        expected: 96,
        measured: 96,
        previous: { value: 100, delta: -3, label: 'BOS-123 banks' },
      }),
    ),
  )
  assert.match(message, /recorded BOS-123 banks delta is -3/)
  assert.match(message, /96 - 100 derives -4/)
  assert.match(message, /lying re-baseline comment/)
})

test('a violated below bound names both readings rather than prescribing one cause', () => {
  const message = messageOf(() =>
    assertExactSize(base({ below: { name: 'PRE_EXTRACTION_BASELINE', value: 90 } })),
  )
  assert.ok(message, 'a pin at or above its baseline must throw')
  assert.match(message, /RATCHET = 100/, 'the pin must be named with its value')
  assert.match(message, /PRE_EXTRACTION_BASELINE = 90/, 'the baseline must be named with its value')
  assert.match(message, /the pin was raised toward the baseline/)
  assert.match(message, /the baseline needs re-deriving/)
  assert.ok(
    !message.includes('trim resident prose'),
    'the old message prescribed one cause; this one must not',
  )
  assert.ok(message.endsWith(`Not covered by this check: ${RESIDUAL}.`))
})

// ── assertExactSize: units ─────────────────────────────────────────────────────────────────

test('assertExactSize reports the unit it was given', () => {
  const message = messageOf(() =>
    assertExactSize(base({ measured: 181, expected: 176, unit: 'lines' })),
  )
  assert.match(message, /181 lines/)
  assert.match(message, /176 lines/)
  assert.ok(!message.includes('bytes'), 'a line gate must not talk about bytes')
})

// ── measureFile ────────────────────────────────────────────────────────────────────────────

test('measureFile returns the byte count of a real file', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'size-ratchet-'))
  const file = path.join(dir, 'a.md')
  fs.writeFileSync(file, 'héllo\n')
  assert.equal(measureFile(file), Buffer.byteLength('héllo\n', 'utf8'))
  fs.rmSync(dir, { force: true, recursive: true })
})

test('measureFile counts lines the way the CLAUDE.md gate does', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'size-ratchet-'))
  const file = path.join(dir, 'a.md')
  fs.writeFileSync(file, 'one\ntwo\nthree\n')
  assert.equal(measureFile(file, { unit: 'lines' }), 3, 'a trailing newline is not a fourth line')
  fs.rmSync(dir, { force: true, recursive: true })
})

test('measureFile throws naming the path when the file is missing', () => {
  const missing = path.join(os.tmpdir(), 'size-ratchet-absent', 'nope.md')
  const message = messageOf(() => measureFile(missing))
  assert.ok(message, 'a missing artifact must throw, never measure as 0')
  assert.ok(message.includes(missing), 'the failure must name the path it could not read')
  assert.match(message, /pass on nothing at all/)
})

test('measureFile throws rather than returning 0 on an empty file', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'size-ratchet-'))
  const file = path.join(dir, 'empty.md')
  fs.writeFileSync(file, '')
  const message = messageOf(() => measureFile(file))
  assert.ok(message, 'an empty artifact is a collapsed measurement, not a small artifact')
  assert.match(message, /collapsed measurement/)
  assert.match(message, /satisfies every ceiling ever written/)
  fs.rmSync(dir, { force: true, recursive: true })
})

// ── assertArtifactSet ──────────────────────────────────────────────────────────────────────

test('assertArtifactSet passes on a list of the expected length', () => {
  assert.doesNotThrow(() => assertArtifactSet(['a', 'b'], 2, 'BUILD_MIRRORS'))
})

test('assertArtifactSet on a shortened list names expected vs actual', () => {
  const message = messageOf(() => assertArtifactSet(['a'], 2, 'BUILD_MIRRORS'))
  assert.ok(message, 'a shortened guarded list must throw')
  assert.match(message, /BUILD_MIRRORS has 1 entries, expected 2/)
  assert.match(message, /nothing going red to say so/)
})

test('assertArtifactSet refuses an empty list as vacuous', () => {
  const message = messageOf(() => assertArtifactSet([], 0, 'BUILD_MIRRORS'))
  assert.match(message, /passes vacuously/)
})

// ── assertMirrorRegenerated ────────────────────────────────────────────────────────────────

const HEADER = '<!-- Generated. Do not edit directly. -->\n\n'
const fakeRead = (files) => (p) => {
  if (!(p in files)) throw new Error(`unexpected read of ${p}`)
  return files[p]
}

test('assertMirrorRegenerated passes when the mirror equals its regeneration', () => {
  assert.doesNotThrow(() =>
    assertMirrorRegenerated({
      mirrorPath: 'mirror.md',
      read: fakeRead({ 'mirror.md': 'body\n', 'source.md': 'body\n' }),
      regenerate: (source) => source,
      sourcePath: 'source.md',
    }),
  )
})

test('a mirror LARGER than its source but regenerating exactly passes', () => {
  // Guards against reintroducing the false rule "a mirror larger than its own source is the
  // tell". The generator unconditionally prepends a header, so a healthy mirror is ALWAYS
  // larger; that rule would ship a permanently-red gate. Exact regeneration equality is the
  // discriminator, and this case is the one that proves the two are not the same test.
  const source = 'body\n'
  const mirror = HEADER + source
  assert.ok(
    Buffer.byteLength(mirror) > Buffer.byteLength(source),
    'the fixture must actually exercise a larger-than-source mirror',
  )
  assert.doesNotThrow(() =>
    assertMirrorRegenerated({
      mirrorPath: 'mirror.md',
      read: fakeRead({ 'mirror.md': mirror, 'source.md': source }),
      regenerate: (s) => HEADER + s,
      sourcePath: 'source.md',
    }),
  )
})

test('an unequal mirror names `make codex-skills` and never the prose remedy', () => {
  const message = messageOf(() =>
    assertMirrorRegenerated({
      mirrorPath: 'mirror.md',
      read: fakeRead({ 'mirror.md': HEADER + 'hand edited\n', 'source.md': 'body\n' }),
      regenerate: (s) => HEADER + s,
      sourcePath: 'source.md',
    }),
  )
  assert.ok(message, 'a hand-edited mirror must throw')
  assert.match(message, /make codex-skills/)
  assert.ok(
    !message.includes('move situational content'),
    'a generated mirror is regenerated, never trimmed',
  )
  assert.ok(
    !message.toLowerCase().includes('move situational content into a reference'),
    'the extraction remedy must not reach a generated mirror',
  )
})

test('the unequal-mirror message states that size is not the discriminator', () => {
  const message = messageOf(() =>
    assertMirrorRegenerated({
      mirrorPath: 'mirror.md',
      read: fakeRead({ 'mirror.md': 'x\n', 'source.md': 'body\n' }),
      regenerate: (s) => HEADER + s,
      sourcePath: 'source.md',
    }),
  )
  assert.match(message, /size is NOT the discriminator/)
  assert.match(message, /a healthy mirror is always larger than its source/)
  assert.ok(message.endsWith('.'), 'the message ends with its residual sentence')
  assert.match(message, /Not covered by this check:/)
})

test('an empty source is refused rather than compared', () => {
  const message = messageOf(() =>
    assertMirrorRegenerated({
      mirrorPath: 'mirror.md',
      read: fakeRead({ 'mirror.md': '', 'source.md': '' }),
      regenerate: (s) => s,
      sourcePath: 'source.md',
    }),
  )
  assert.match(message, /is empty, so regeneration proves nothing/)
})
