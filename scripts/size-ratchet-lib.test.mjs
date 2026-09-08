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
  assertDescendingBudget,
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

// ── assertDescendingBudget: the asymmetric price (BOS-1208) ────────────────────────────────
//
// The arm that matters most here is the one that does NOT throw. `assertExactSize` charged the
// same one-line repin for a deletion as for an addition, which is why a de-ceremony trim
// regrew; the budget's whole claim is that a shrink now costs nothing. A suite that only
// asserted the throwing arms would stay green if that claim quietly stopped being true, so the
// shrink case is proved against a real file on disk rather than against a hand-written number.

const budgetBase = (overrides = {}) => ({
  budget: 100,
  constFile: 'scripts/demo-skill.test.mjs',
  constName: 'RATCHET',
  label: 'resident body budget',
  measured: 100,
  now: new Date('2026-01-01T00:00:00Z'),
  path: 'skills/demo/SKILL.md',
  raise: { from: 100 },
  residual: RESIDUAL,
  reviewBy: '2026-06-01',
  stepDown: 8,
  ...overrides,
})

test('assertDescendingBudget passes under the budget', () => {
  assert.doesNotThrow(() => assertDescendingBudget(budgetBase({ measured: 42 })))
})

test('assertDescendingBudget passes exactly at the budget', () => {
  assert.doesNotThrow(() => assertDescendingBudget(budgetBase({ measured: 100 })))
})

test('assertDescendingBudget over budget names measurement, budget, constant and file', () => {
  const message = messageOf(() => assertDescendingBudget(budgetBase({ measured: 137 })))
  assert.ok(message, 'a measurement over the budget must throw')
  assert.match(message, /137/, 'the measured value must be in the message')
  assert.match(message, /100/, 'the budget must be in the message')
  assert.match(message, /RATCHET/, 'constName must be in the message')
  assert.match(message, /scripts\/demo-skill\.test\.mjs/, 'constFile must be in the message')
  assert.match(message, /skills\/demo\/SKILL\.md/, 'the artifact path must be in the message')
  assert.match(message, /over by 37/, 'the message must state how far over it is')
})

test('the over-budget message states that a shrink is free and only a raise is priced', () => {
  const message = messageOf(() => assertDescendingBudget(budgetBase({ measured: 101 })))
  assert.match(message, /SHRINK needs no edit to RATCHET/)
  assert.match(message, /only a RAISE costs anything/)
  assert.match(message, /recorded justification/)
  assert.ok(
    message.includes(MOVE_TO_REFERENCE_REMEDY),
    'the default remedy points at extraction into a reference',
  )
  assert.ok(
    !message.includes('only moves DOWN'),
    'the equality pin’s two-sided wording must not leak into an asymmetric budget',
  )
})

test('assertDescendingBudget accepts a remedy override', () => {
  const message = messageOf(() =>
    assertDescendingBudget(budgetBase({ measured: 101, remedy: NO_REFERENCE_REMEDY })),
  )
  assert.ok(message.includes(NO_REFERENCE_REMEDY), 'the override must reach the message')
  assert.ok(!message.includes(MOVE_TO_REFERENCE_REMEDY))
})

test('a budget raised with no recorded justification reds, naming the delta and the constant', () => {
  const message = messageOf(() =>
    assertDescendingBudget(budgetBase({ budget: 160, measured: 120, raise: { from: 100 } })),
  )
  assert.ok(message, 'an un-justified raise must throw')
  assert.match(message, /raised from 100 to 160/)
  assert.match(message, /up by 60/)
  assert.match(message, /RATCHET in scripts\/demo-skill\.test\.mjs/)
  assert.match(message, /raise\.justification/)
  assert.match(message, /SAME commit that raised the budget/)
  assert.match(message, /shrinking the artifact costs nothing/)
})

test('a blank justification does not buy a raise', () => {
  const message = messageOf(() =>
    assertDescendingBudget(
      budgetBase({ budget: 160, measured: 120, raise: { from: 100, justification: '   ' } }),
    ),
  )
  assert.match(message, /no recorded reason/)
})

test('a recorded justification buys the raise', () => {
  assert.doesNotThrow(() =>
    assertDescendingBudget(
      budgetBase({
        budget: 160,
        measured: 120,
        raise: { from: 100, justification: 'the preflight fence is executed, not explained' },
      }),
    ),
  )
})

test('LOWERING the budget below its recorded previous value costs no justification at all', () => {
  assert.doesNotThrow(() =>
    assertDescendingBudget(budgetBase({ budget: 60, measured: 50, raise: { from: 100 } })),
  )
})

test('the un-justified raise is reported BEFORE the over-budget verdict', () => {
  // An unjustified constant makes every other verdict untrustworthy: there is no point telling
  // a reader they are over a ceiling nobody recorded a reason for.
  const message = messageOf(() =>
    assertDescendingBudget(budgetBase({ budget: 160, measured: 999, raise: { from: 100 } })),
  )
  assert.match(message, /no recorded reason/)
  assert.ok(!message.includes('over by'), 'the raise verdict wins the tie')
})

test('a passed review date with an unlowered budget reds on the CALENDAR, not on the artifact', () => {
  const message = messageOf(() =>
    assertDescendingBudget(
      budgetBase({ measured: 90, now: new Date('2026-07-04T00:00:00Z'), stepDown: 8 }),
    ),
  )
  assert.ok(message, 'a stale review date must throw even though the artifact fits')
  assert.match(message, /passed its review date 2026-06-01/)
  assert.match(message, /today is 2026-07-04/)
  assert.match(message, /THE CAUSE IS THE CALENDAR, NOT A CODE CHANGE/)
  assert.match(
    message,
    /nothing in skills\/demo\/SKILL\.md did this/,
    'the message must exonerate the artifact by name — an unrelated branch can hit this',
  )
  assert.match(message, /Lower RATCHET to at most 92/, 'it must name the value to lower to')
  assert.match(message, /move `reviewBy` forward/)
})

test('a review date still in the future does not red', () => {
  assert.doesNotThrow(() =>
    assertDescendingBudget(budgetBase({ now: new Date('2026-05-31T23:59:59Z') })),
  )
})

test('the over-budget verdict is reported BEFORE the stale review date', () => {
  // A branch that broke the ceiling should hear about its own change before it hears about
  // scheduled maintenance it did not cause.
  const message = messageOf(() =>
    assertDescendingBudget(budgetBase({ measured: 120, now: new Date('2026-07-04T00:00:00Z') })),
  )
  assert.match(message, /over by 20/)
  assert.ok(!message.includes('THE CAUSE IS THE CALENDAR'))
})

test('every assertDescendingBudget failure ends with the stated residual', () => {
  const cases = [
    budgetBase({ measured: 101 }),
    budgetBase({ budget: 160, measured: 120, raise: { from: 100 } }),
    budgetBase({ now: new Date('2026-07-04T00:00:00Z') }),
    budgetBase({ below: { name: 'PRE_EXTRACTION_BASELINE', value: 90 } }),
  ]
  for (const options of cases) {
    const message = messageOf(() => assertDescendingBudget(options))
    assert.ok(
      message.endsWith(`Not covered by this check: ${RESIDUAL}.`),
      `every failure must end with the residual, got: ${message}`,
    )
  }
})

test('assertDescendingBudget omitting residual throws a blind-spot error on a PASSING measurement', () => {
  const { residual, ...withoutResidual } = budgetBase({ measured: 10 })
  assert.equal(typeof residual, 'string')
  const message = messageOf(() => assertDescendingBudget(withoutResidual))
  assert.match(message, /must state its blind spot/)
  assert.match(message, /wiring error in the gate, not a finding about the artifact/)
})

test('assertDescendingBudget rejects an empty residual the same way as a missing one', () => {
  const message = messageOf(() => assertDescendingBudget(budgetBase({ residual: '   ' })))
  assert.match(message, /must state its blind spot/)
})

test('omitting `raise` is refused, so the priced direction cannot be dropped to dodge the check', () => {
  const { raise, ...withoutRaise } = budgetBase({ measured: 10 })
  assert.equal(typeof raise, 'object')
  const message = messageOf(() => assertDescendingBudget(withoutRaise))
  assert.match(message, /requires a `raise`/)
  assert.match(message, /the one priced direction becomes free/)
})

test('a malformed or unreal reviewBy is a wiring error, not a size finding', () => {
  for (const reviewBy of ['2026/06/01', 'soon', '2026-13-01']) {
    const message = messageOf(() => assertDescendingBudget(budgetBase({ reviewBy })))
    assert.ok(message, `${reviewBy} must be refused`)
    assert.match(message, /Wiring error\.$/)
  }
})

test('a stepDown of 0 is refused: a budget that never descends is the flat ceiling', () => {
  const message = messageOf(() => assertDescendingBudget(budgetBase({ stepDown: 0 })))
  assert.match(message, /must be a POSITIVE integer/)
  assert.match(message, /never descends/)
})

test('a violated below bound names both readings for the budget too', () => {
  const message = messageOf(() =>
    assertDescendingBudget(budgetBase({ below: { name: 'PRE_EXTRACTION_BASELINE', value: 90 } })),
  )
  assert.match(message, /RATCHET = 100/)
  assert.match(message, /PRE_EXTRACTION_BASELINE = 90/)
  assert.match(message, /the budget was raised toward the baseline/)
  assert.match(message, /the baseline needs re-deriving/)
})

test('a satisfied below bound does not throw for the budget', () => {
  assert.doesNotThrow(() =>
    assertDescendingBudget(budgetBase({ below: { name: 'PRE_EXTRACTION_BASELINE', value: 120 } })),
  )
})

test('assertDescendingBudget reports the unit it was given', () => {
  const message = messageOf(() =>
    assertDescendingBudget(
      budgetBase({ budget: 176, measured: 181, raise: { from: 176 }, unit: 'lines' }),
    ),
  )
  assert.match(message, /181 lines/)
  assert.match(message, /176 lines/)
  assert.ok(!message.includes('bytes'), 'a line gate must not talk about bytes')
})

// ── assertDescendingBudget: the acceptance criteria, proved against a real file ─────────────

test('BOS-1208 AC: deleting bytes leaves the gate green with the budget constant UNCHANGED', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'size-budget-'))
  const file = path.join(dir, 'SKILL.md')
  fs.writeFileSync(file, 'x'.repeat(400))

  // Written once. Nothing below is allowed to touch it — that is the claim under test.
  const BUDGET = 400
  const gate = () =>
    assertDescendingBudget(
      budgetBase({ budget: BUDGET, measured: measureFile(file), raise: { from: BUDGET } }),
    )

  assert.equal(measureFile(file), 400, 'seeded at the measured size, so the budget binds at once')
  assert.doesNotThrow(gate, 'an artifact at its seeded budget passes')

  fs.writeFileSync(file, 'x'.repeat(150))
  assert.equal(measureFile(file), 150, 'the fixture must actually have shrunk')
  assert.doesNotThrow(
    gate,
    'a shrink must cost NO edit to the budget constant — this is the incentive change the ' +
      'equality pin could not make, and the arm a throw/no-throw suite would lose silently',
  )

  fs.rmSync(dir, { force: true, recursive: true })
})

test('BOS-1208 AC: the budget is shown ABLE TO FIRE — the same fixture grown past it reds', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'size-budget-'))
  const file = path.join(dir, 'SKILL.md')
  const BUDGET = 400
  const gate = () =>
    assertDescendingBudget(
      budgetBase({ budget: BUDGET, measured: measureFile(file), raise: { from: BUDGET } }),
    )

  fs.writeFileSync(file, 'x'.repeat(400))
  assert.doesNotThrow(gate, 'the green reading has to be real before the red one means anything')

  fs.writeFileSync(file, 'x'.repeat(441))
  const message = messageOf(gate)
  assert.ok(message, 'growth past the budget must red — a gate assumed to fire is not a gate')
  assert.match(message, /measured 441/)
  assert.match(message, /budget 400/)
  assert.match(message, /over by 41/)

  fs.rmSync(dir, { force: true, recursive: true })
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
