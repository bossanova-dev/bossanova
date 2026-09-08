#!/usr/bin/env node

import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { test } from 'node:test'

import { region } from '../scripts/gate-region-lib.mjs'

import { REPAIR_RESULTS } from './bs-run-sentinel.mjs'
import {
  ACTION_BLOCKED,
  ACTION_ESCALATE_STRATEGY,
  ACTION_REPORT,
  ACTION_REPORT_REPEATING,
  BLOCK_AFTER_OCCURRENCES,
  ESCALATE_AFTER_OCCURRENCES,
  ESCALATION_ACTIONS,
  ESCALATION_REASONS,
  IDENTITY_NO_LINE,
  REASON_CEILING_REACHED,
  REASON_FIRST_SIGHTING,
  REASON_REPEATING_ACTIONABLE,
  REASON_REPEATING_BELOW_THRESHOLD,
  REASON_REPEATING_NOT_ACTIONABLE,
  RESIDUAL_IDENTITY_ARITY,
  RUNG_BLOCKED,
  RUNG_ESCALATE,
  RUNG_FIRST_SIGHTING,
  RUNG_REPEATING,
  classifyResidual,
  residualIdentityKey,
  runCli,
} from './bs-repair-escalation.mjs'

const IDENTITY = ['services/app/handler.go', 41, 'guard admits a zero-padded value']
const KEY = JSON.stringify(IDENTITY)

// ---------------------------------------------------------------------------
// Rung coverage. One test per rung, each naming the branch it pins so a failure
// says which rung moved.

test('rung 0: a first sighting reports the residual and does not mark it repeating', () => {
  const verdict = classifyResidual({ identity: IDENTITY, priorOccurrences: 0 })
  assert.equal(verdict.rung, RUNG_FIRST_SIGHTING)
  assert.equal(verdict.action, ACTION_REPORT)
  assert.equal(verdict.reason, REASON_FIRST_SIGHTING)
  assert.equal(verdict.identity, KEY)
})

test('rung 1: a repeat below the threshold marks it repeating, it does not escalate', () => {
  const verdict = classifyResidual({ identity: IDENTITY, priorOccurrences: 1 })
  assert.equal(verdict.rung, RUNG_REPEATING)
  assert.equal(verdict.action, ACTION_REPORT_REPEATING)
  assert.equal(verdict.reason, REASON_REPEATING_BELOW_THRESHOLD)
  // The threshold has to leave this rung reachable, or a first sighting would
  // escalate on its very first repeat and ordinary churn would blow the ladder.
  assert.ok(ESCALATE_AFTER_OCCURRENCES >= 2, 'rung 1 must be reachable')
  assert.ok(1 < ESCALATE_AFTER_OCCURRENCES)
})

test('rung 2: between the threshold and the ceiling, an actionable repeat escalates strategy', () => {
  for (
    let priorOccurrences = ESCALATE_AFTER_OCCURRENCES;
    priorOccurrences < BLOCK_AFTER_OCCURRENCES;
    priorOccurrences += 1
  ) {
    const verdict = classifyResidual({ identity: IDENTITY, priorOccurrences, actionable: true })
    assert.equal(verdict.rung, RUNG_ESCALATE, `priorOccurrences=${priorOccurrences}`)
    assert.equal(verdict.action, ACTION_ESCALATE_STRATEGY)
    assert.equal(verdict.reason, REASON_REPEATING_ACTIONABLE)
  }
})

test('the ceiling ends the ladder however actionable the caller says it is', () => {
  // Without this branch `actionable: true` returned rung 2 at 2, 5, 50 and 999 alike,
  // so a residual could re-fire without limit and never reach the human rung the
  // ladder exists to reach. `actionable` is the caller's own unverifiable claim; the
  // ceiling is the one branch it cannot talk past.
  assert.ok(BLOCK_AFTER_OCCURRENCES > ESCALATE_AFTER_OCCURRENCES, 'rung 2 must be reachable')
  for (const priorOccurrences of [BLOCK_AFTER_OCCURRENCES, BLOCK_AFTER_OCCURRENCES + 1, 999]) {
    for (const actionable of [true, false]) {
      const verdict = classifyResidual({ identity: IDENTITY, priorOccurrences, actionable })
      assert.equal(
        verdict.rung,
        RUNG_BLOCKED,
        `priorOccurrences=${priorOccurrences} actionable=${actionable}`,
      )
      assert.equal(verdict.action, ACTION_BLOCKED)
    }
  }
  // The ceiling reports its own reason, so "you kept claiming you could act" is
  // distinguishable from "you said you could not".
  assert.equal(
    classifyResidual({
      identity: IDENTITY,
      priorOccurrences: BLOCK_AFTER_OCCURRENCES,
      actionable: true,
    }).reason,
    REASON_CEILING_REACHED,
  )
})

test('the last sighting below the ceiling still honours actionable in both directions', () => {
  // The boundary that moved: one below the ceiling is still the caller's call.
  const below = BLOCK_AFTER_OCCURRENCES - 1
  assert.equal(
    classifyResidual({ identity: IDENTITY, priorOccurrences: below, actionable: true }).rung,
    RUNG_ESCALATE,
  )
  assert.equal(
    classifyResidual({ identity: IDENTITY, priorOccurrences: below, actionable: false }).rung,
    RUNG_BLOCKED,
  )
})

test('rung 3: at the threshold with nothing to do, the residual needs a human', () => {
  const verdict = classifyResidual({
    identity: IDENTITY,
    priorOccurrences: ESCALATE_AFTER_OCCURRENCES,
    actionable: false,
  })
  assert.equal(verdict.rung, RUNG_BLOCKED)
  assert.equal(verdict.action, ACTION_BLOCKED)
  assert.equal(verdict.reason, REASON_REPEATING_NOT_ACTIONABLE)
})

test('actionable is only consulted at or above the threshold', () => {
  // Below the threshold both answers are the same rung: the round reports either
  // way, so an actionable flag must not smuggle an early escalation in.
  for (const actionable of [true, false]) {
    const verdict = classifyResidual({ identity: IDENTITY, priorOccurrences: 1, actionable })
    assert.equal(verdict.rung, RUNG_REPEATING, `actionable=${actionable}`)
  }
})

test('a truthy-but-not-true actionable value is not actionable', () => {
  // Fail closed: only a strict `true` escalates. A string, a 1, or an object
  // arriving from a shell round-trip must land on the human rung, never on the
  // "this pass can fix it" rung.
  for (const actionable of ['true', 1, {}]) {
    const verdict = classifyResidual({
      identity: IDENTITY,
      priorOccurrences: ESCALATE_AFTER_OCCURRENCES,
      actionable,
    })
    assert.equal(verdict.rung, RUNG_BLOCKED, `actionable=${JSON.stringify(actionable)}`)
    // Below the ceiling, so this is the actionable check failing closed and not the
    // ceiling answering for it.
    assert.ok(ESCALATE_AFTER_OCCURRENCES < BLOCK_AFTER_OCCURRENCES)
    assert.equal(verdict.reason, REASON_REPEATING_NOT_ACTIONABLE)
  }
})

// ---------------------------------------------------------------------------
// Enumerated-vocabulary closure. Every value the ladder can return is a member
// of its exported set, and the terminal rung reuses the shipped terminal token
// rather than minting a parallel one.

test('every returned reason and action is a member of the exported sets', () => {
  const seenReasons = new Set()
  const seenActions = new Set()
  for (const priorOccurrences of [0, 1, 2, 3, 7, BLOCK_AFTER_OCCURRENCES]) {
    for (const actionable of [true, false]) {
      const verdict = classifyResidual({ identity: IDENTITY, priorOccurrences, actionable })
      assert.ok(
        ESCALATION_REASONS.includes(verdict.reason),
        `unenumerated reason: ${verdict.reason}`,
      )
      assert.ok(
        ESCALATION_ACTIONS.includes(verdict.action),
        `unenumerated action: ${verdict.action}`,
      )
      seenReasons.add(verdict.reason)
      seenActions.add(verdict.action)
    }
  }
  // The closure runs both ways: an exported member no branch can produce is a
  // reason nobody will ever read, and it rots silently.
  assert.deepEqual([...seenReasons].sort(), [...ESCALATION_REASONS].sort())
  assert.deepEqual([...seenActions].sort(), [...ESCALATION_ACTIONS].sort())
  // Reasons outnumber actions because the terminal rung is reachable two ways — the
  // caller declining to act, and the ceiling overriding a caller who claims it can.
  // Every reason still maps onto one of the enumerated actions.
  assert.ok(ESCALATION_REASONS.length >= ESCALATION_ACTIONS.length)
})

test('the terminal action is the shipped terminal token, not a new spelling', () => {
  assert.ok(
    REPAIR_RESULTS.includes(ACTION_BLOCKED),
    'the terminal rung must report a shipped terminal token',
  )
  // The non-terminal rungs are the extension, and they must not collide with the
  // shipped vocabulary: an action that is both a rung name and a terminal token
  // would be classified as a run outcome by a caller reading the sentinel.
  const overlap = ESCALATION_ACTIONS.filter((action) => REPAIR_RESULTS.includes(action))
  assert.deepEqual(overlap, [ACTION_BLOCKED])
})

// ---------------------------------------------------------------------------
// Identity. Same key as the review-side oscillation guard, and fail-closed.

test('the identity key matches the oscillation guard tuple, from a tuple or an object', () => {
  assert.equal(residualIdentityKey(IDENTITY), KEY)
  assert.equal(
    residualIdentityKey({ file: IDENTITY[0], line: IDENTITY[1], title: IDENTITY[2] }),
    KEY,
  )
  // A whole-file finding legitimately carries no line, and the guard encodes that as
  // the STRING 'null'. This is the case where the two keys used to diverge.
  assert.equal(residualIdentityKey(['a.go', null, 't']), '["a.go","null","t"]')
  assert.equal(IDENTITY_NO_LINE, 'null')
  assert.equal(RESIDUAL_IDENTITY_ARITY, 3)
})

test('the line-less encoding is pinned against the oscillation guard it claims to share', () => {
  // The module's central design claim is that review and repair key a finding the
  // same way. Asserting our own output alone would restate the claim rather than
  // check it, so read the guard's source and re-implement nothing: if the guard
  // ever stops coercing a null line to the string 'null', this fails and the claim
  // in the module header and the skill body has to be narrowed with it.
  const guardSource = readFileSync(new URL('./bs-review-caps.mjs', import.meta.url), 'utf8')
  const guardBody = region(
    guardSource,
    'function oscillationFindingKey',
    'function oscillationDispositionSet',
    'oscillation guard key',
  )
  assert.ok(
    guardBody.includes("finding.line === null ? 'null'"),
    'oscillationFindingKey no longer encodes a null line as the string "null"',
  )
  assert.ok(
    guardBody.includes('JSON.stringify([file, line, title])'),
    'oscillationFindingKey no longer keys on JSON.stringify([file, line, title])',
  )
  // Which is exactly what residualIdentityKey produces for the same finding.
  assert.equal(residualIdentityKey({ file: 'a.go', line: null, title: 't' }), '["a.go","null","t"]')
  assert.equal(residualIdentityKey({ file: 'a.go', line: 12, title: 't' }), '["a.go",12,"t"]')
})

test('a recorded line-less key replays, through the function and through --in', () => {
  // The skill body tells a later round to copy the RECORDED key verbatim rather than
  // re-derive a tuple from a file whose lines have moved. Following that instruction
  // means JSON.parse-ing a key this module emitted and handing it straight back — so
  // the accept side has to take the string IDENTITY_NO_LINE the emit side writes. An
  // asymmetric validator would exit 2 on exactly the whole-file class the header calls
  // the residual class most likely to re-fire, stranding it on rung 0 forever.
  const recorded = residualIdentityKey(['a.go', null, 't'])
  assert.equal(recorded, `["a.go",${JSON.stringify(IDENTITY_NO_LINE)},"t"]`)
  assert.equal(residualIdentityKey(JSON.parse(recorded)), recorded)
  assert.equal(residualIdentityKey({ file: 'a.go', line: IDENTITY_NO_LINE, title: 't' }), recorded)

  // And through the CLI path the body actually prescribes: write the key to a file,
  // pass --in. A prior sighting must reach a rung, not an input error.
  const cap = capture()
  const io = { ...cap.io, readFile: () => recorded }
  assert.equal(runCli(['classify', '--in', 'residual.json', '1'], io), 0)
  const verdict = JSON.parse(cap.out())
  assert.equal(verdict.identity, recorded)
  assert.equal(verdict.rung, RUNG_REPEATING)
})

test('the identity trims file and title, so trailing whitespace is not a new residual', () => {
  // The tuple is hand-typed from a prose template; keying "a.go" apart from "a.go "
  // would reset a repeat count on an invisible character.
  assert.equal(residualIdentityKey([' a.go ', 41, ' t ']), residualIdentityKey(['a.go', 41, 't']))
  assert.equal(residualIdentityKey([' a.go ', null, ' t ']), '["a.go","null","t"]')
})

test('a malformed identity throws instead of defaulting to a first sighting', () => {
  // This is the fail-closed case the whole ladder rests on: a silent default
  // would make every repeat read as rung 0, and nothing would ever escalate.
  const malformed = [
    ['services/app/handler.go', 41], // wrong arity: missing title
    ['services/app/handler.go', 41, 'title', 'extra'], // wrong arity: too long
    ['', 41, 'title'], // missing member: blank file
    ['a.go', 41, '   '], // missing member: blank title
    ['a.go', 1.5, 'title'], // line is not an integer
    ['a.go', '41', 'title'], // line is not an integer
    { file: 'a.go', title: 'title' }, // missing member: no line
    null,
    'a.go:41',
    undefined,
  ]
  for (const identity of malformed) {
    assert.throws(
      () => classifyResidual({ identity, priorOccurrences: 0 }),
      TypeError,
      `expected a throw for ${JSON.stringify(identity)}`,
    )
  }
})

test('a malformed priorOccurrences throws instead of defaulting to a first sighting', () => {
  for (const priorOccurrences of [-1, 1.5, '2', null, undefined, Number.NaN]) {
    assert.throws(
      () => classifyResidual({ identity: IDENTITY, priorOccurrences }),
      TypeError,
      `expected a throw for ${String(priorOccurrences)}`,
    )
  }
})

// ---------------------------------------------------------------------------
// CLI.

function capture() {
  const out = []
  const err = []
  return {
    io: { stdout: (t) => out.push(t), stderr: (t) => err.push(t) },
    out: () => out.join(''),
    err: () => err.join(''),
  }
}

test('runCli returns 0 and prints the verdict on success', () => {
  const cap = capture()
  assert.equal(runCli(['classify', JSON.stringify(IDENTITY), '0'], cap.io), 0)
  assert.deepEqual(JSON.parse(cap.out()), {
    identity: KEY,
    rung: RUNG_FIRST_SIGHTING,
    action: ACTION_REPORT,
    reason: REASON_FIRST_SIGHTING,
  })
  assert.equal(cap.err(), '')
})

test('runCli passes the actionable flag through to the ladder', () => {
  const cap = capture()
  assert.equal(
    runCli(
      ['classify', JSON.stringify(IDENTITY), String(ESCALATE_AFTER_OCCURRENCES), 'true'],
      cap.io,
    ),
    0,
  )
  assert.equal(JSON.parse(cap.out()).rung, RUNG_ESCALATE)
})

test('runCli returns 2 and writes nothing to stdout on every error path', () => {
  const cases = [
    [],
    ['bogus'],
    ['classify'],
    ['classify', JSON.stringify(IDENTITY)],
    ['classify', 'not json', '0'],
    ['classify', JSON.stringify(['a.go', 41]), '0'],
    ['classify', JSON.stringify(IDENTITY), 'abc'],
    ['classify', JSON.stringify(IDENTITY), '-1'],
    // The coerce-first hole, in both directions. Every one of these used to reach
    // classifyResidual as a valid non-negative integer and print a confident rung.
    ['classify', JSON.stringify(IDENTITY), ''], // Number('')   === 0 -> looked like a first sighting
    ['classify', JSON.stringify(IDENTITY), '  '], // Number('  ') === 0 -> same
    ['classify', JSON.stringify(IDENTITY), '\n'], // Number('\n') === 0 -> same
    ['classify', JSON.stringify(IDENTITY), '+0'], // Number('+0') === 0 -> same
    ['classify', JSON.stringify(IDENTITY), ' 2 '], // padded    -> reached the terminal rung
    ['classify', JSON.stringify(IDENTITY), '0x3'], // hex       -> reached the terminal rung
    ['classify', JSON.stringify(IDENTITY), '1e2'], // exponent  -> reached the terminal rung
    ['classify', JSON.stringify(IDENTITY), '1.0'], // decimal point is not a count
    // An unrecognised actionable token is a typo, not a `false`.
    ['classify', JSON.stringify(IDENTITY), '2', '1'],
    ['classify', JSON.stringify(IDENTITY), '2', 'yes'],
    ['classify', JSON.stringify(IDENTITY), '2', 'TRUE'],
    ['classify', JSON.stringify(IDENTITY), '2', ''],
    // Argument count, through BOTH argv slices. `rest` is argv.slice(2) inline and argv.slice(3)
    // for --in, and the two forms share one `rest.length > 2` guard — so the guard has to be
    // driven at each offset or an off-by-one in the file form goes undetected. This is the seam
    // round 1 proved fragile, and the file form is the one the skill body prescribes.
    ['classify', JSON.stringify(IDENTITY), '2', 'true', 'extra'],
    ['classify', '--in', '/tmp/does-not-matter', '2', 'true', 'extra'],
    // --in with no path, and with the count omitted.
    ['classify', '--in'],
    ['classify', '--in', '/tmp/does-not-matter'],
  ]
  for (const argv of cases) {
    const cap = capture()
    assert.equal(runCli(argv, cap.io), 2, `argv=${JSON.stringify(argv)}`)
    assert.equal(cap.out(), '', `argv=${JSON.stringify(argv)} wrote to stdout`)
    assert.ok(cap.err().length > 0, `argv=${JSON.stringify(argv)} wrote no diagnosis`)
  }
})

test('the arity guard fires through the --in argv slice, not only the inline one', () => {
  // `rest` is argv.slice(2) inline and argv.slice(3) for --in, and both forms share one
  // `rest.length > 2` guard. Driving it only inline leaves the file form — the form the skill
  // body prescribes — unexercised. Supply a readFile that WOULD succeed, so a guard that failed
  // to fire here would return 0 with a rung printed rather than erroring for some other reason:
  // that is what makes this case falsifiable rather than merely non-zero.
  const cap = capture()
  const io = { ...cap.io, readFile: () => KEY }
  assert.equal(runCli(['classify', '--in', 'residual.json', '2', 'true', 'extra'], io), 2)
  assert.equal(cap.out(), '')
  assert.match(cap.err(), /classify takes at most <priorOccurrences> and <actionable>/)

  // Control: the same argv without the extra token is accepted through that same slice, so the
  // assertion above pins the guard rather than a broken --in path.
  const ok = capture()
  assert.equal(
    runCli(['classify', '--in', 'residual.json', String(ESCALATE_AFTER_OCCURRENCES), 'true'], {
      ...ok.io,
      readFile: () => KEY,
    }),
    0,
  )
  assert.equal(JSON.parse(ok.out()).rung, RUNG_ESCALATE)
})

test('runCli accepts an explicit actionable=false and an omitted one alike', () => {
  // The tightened token check must not reject the two spellings a caller legitimately
  // produces; only the boundary moved, and the values that still pass still pass.
  for (const argv of [
    ['classify', JSON.stringify(IDENTITY), String(ESCALATE_AFTER_OCCURRENCES), 'false'],
    ['classify', JSON.stringify(IDENTITY), String(ESCALATE_AFTER_OCCURRENCES)],
  ]) {
    const cap = capture()
    assert.equal(runCli(argv, cap.io), 0, `argv=${JSON.stringify(argv)}`)
    assert.equal(JSON.parse(cap.out()).rung, RUNG_BLOCKED)
  }
})

test('runCli reads the identity from a file, so no title is ever spliced into a shell literal', () => {
  // A finding title is arbitrary English prose. "the caller's guard" ends a
  // single-quoted argument early; a title containing a double quote makes the inline
  // JSON unparseable. The file mode is what keeps both out of the command line, and
  // it must produce the identical verdict to the inline form for a safe title.
  const awkward = ["services/app/handler's.go", 41, 'the caller\'s guard "admits" a padded value']
  const reads = []
  const cap = capture()
  const io = {
    ...cap.io,
    readFile: (path) => {
      reads.push(path)
      return `${JSON.stringify(awkward)}\n`
    },
  }
  assert.equal(runCli(['classify', '--in', '/tmp/identity.json', '1'], io), 0)
  assert.deepEqual(reads, ['/tmp/identity.json'])
  const verdict = JSON.parse(cap.out())
  assert.equal(verdict.identity, residualIdentityKey(awkward))
  assert.equal(verdict.rung, RUNG_REPEATING)

  // Same tuple, same verdict, whichever way it arrived.
  const inline = capture()
  assert.equal(runCli(['classify', JSON.stringify(awkward), '1'], inline.io), 0)
  assert.deepEqual(JSON.parse(inline.out()), verdict)
})

test('runCli passes actionable through in file mode too', () => {
  const cap = capture()
  const io = { ...cap.io, readFile: () => JSON.stringify(IDENTITY) }
  assert.equal(
    runCli(['classify', '--in', 'x.json', String(ESCALATE_AFTER_OCCURRENCES), 'true'], io),
    0,
  )
  assert.equal(JSON.parse(cap.out()).rung, RUNG_ESCALATE)
})

test('an unreadable identity file is refused, not treated as a first sighting', () => {
  const cap = capture()
  const io = {
    ...cap.io,
    readFile: () => {
      throw new Error('ENOENT: no such file or directory')
    },
  }
  assert.equal(runCli(['classify', '--in', 'missing.json', '3'], io), 2)
  assert.equal(cap.out(), '')
  assert.match(cap.err(), /cannot read identity file missing\.json/)
})

test('runCli never calls process.exit', () => {
  // The module is imported in-process by this suite; a runCli that called
  // process.exit would have killed the run before reaching here. Assert the
  // contract explicitly so the reason is recorded rather than incidental.
  const cap = capture()
  const code = runCli(['classify', JSON.stringify(IDENTITY), '0'], cap.io)
  assert.equal(typeof code, 'number')
})
