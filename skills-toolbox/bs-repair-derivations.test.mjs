#!/usr/bin/env node

// The specification for bs-repair-derivations.mjs.
//
// These tests assert BEHAVIOUR, never sentences: what each classifier returns for an input, which
// arm an unreadable input takes, and that every member of each frozen verdict set is reachable. A
// verdict nobody can reach is prose with extra steps, and a fail-closed arm nobody pins is removable
// without a red.

import assert from 'node:assert/strict'
import { test } from 'node:test'

import {
  DECLINE_REPLY_REASONS,
  DECLINE_REPLY_RECORD_CITED,
  DECLINE_REPLY_UNBACKED_CLAIM,
  PROBLEM_SOURCES,
  PROBLEM_SOURCE_FAILING_CHECKS,
  PROBLEM_SOURCE_MERGE_CONFLICT,
  PROBLEM_SOURCE_NONE,
  PROBLEM_SOURCE_REPORT_ONLY,
  PROBLEM_SOURCE_REVIEW_FEEDBACK,
  PUSH_REASONS,
  PUSH_REASON_NO_COMMON_DESCENDANT,
  PUSH_REASON_UNREADABLE,
  PUSH_STATES,
  PUSH_STATE_DIVERGED,
  PUSH_STATE_PUBLISHED,
  PUSH_STATE_PUSH_OWED,
  PUSH_STATE_REMOTE_AHEAD,
  PUSH_STATE_WITHHELD,
  RESIDUAL_BODY_PLACEHOLDER,
  RESIDUAL_NOTE_TAG,
  RESIDUAL_SINKS,
  RESIDUAL_SINK_BOSS_NOTES,
  RESIDUAL_SINK_PR_COMMENT,
  classifyDeclineReply,
  classifyProblemSources,
  classifyPushState,
  resolveResidualSink,
  runCli,
} from './bs-repair-derivations.mjs'

const LOCAL = 'a'.repeat(40)
const REMOTE = 'b'.repeat(40)

/** A CLI harness that captures both streams instead of writing to the process's. */
function cli(argv, files = {}) {
  let out = ''
  let err = ''
  const code = runCli(argv, {
    stdout: (text) => {
      out += text
    },
    stderr: (text) => {
      err += text
    },
    readFile: (path) => {
      if (!(path in files)) throw new Error(`ENOENT: no such file ${path}`)
      return files[path]
    },
  })
  return { code, out, err }
}

// ---------------------------------------------------------------------------
// classifyPushState — one test per state, then the fail-closed arms.

test('push state published: equal SHAs owe no push', () => {
  const verdict = classifyPushState({ localSha: LOCAL, remoteSha: LOCAL })
  assert.equal(verdict.state, PUSH_STATE_PUBLISHED)
  assert.equal(verdict.owed, false)
  assert.ok(verdict.line.length > 0)
})

test('push state remote-ahead: local is an ancestor of origin, so nothing is owed', () => {
  const verdict = classifyPushState({
    localSha: LOCAL,
    remoteSha: REMOTE,
    localIsAncestorOfRemote: true,
  })
  assert.equal(verdict.state, PUSH_STATE_REMOTE_AHEAD)
  assert.equal(verdict.owed, false)
})

test('push state push-owed: origin is an ancestor of local, and this is the only owing state', () => {
  const verdict = classifyPushState({
    localSha: LOCAL,
    remoteSha: REMOTE,
    remoteIsAncestorOfLocal: true,
  })
  assert.equal(verdict.state, PUSH_STATE_PUSH_OWED)
  assert.equal(verdict.owed, true)
})

test('push state diverged: neither side descends from the other', () => {
  const verdict = classifyPushState({ localSha: LOCAL, remoteSha: REMOTE })
  assert.equal(verdict.state, PUSH_STATE_DIVERGED)
  assert.equal(verdict.owed, false)
  // Distinct from the fail-closed arm: a real divergence was observed, not an unreadable input.
  assert.equal(verdict.reason, PUSH_REASON_NO_COMMON_DESCENDANT)
})

test('push state withheld: a declared withhold is reported, not pushed', () => {
  const verdict = classifyPushState({ localSha: LOCAL, remoteSha: REMOTE, withheld: true })
  assert.equal(verdict.state, PUSH_STATE_WITHHELD)
  assert.equal(verdict.owed, false)
})

test('push state fails closed: no SHAs at all never classifies as published', () => {
  // The acceptance criterion in its own right. `published` is the arm that lets a round which forgot
  // to push say nothing; reaching it from an empty input would restore exactly the silence this
  // module exists to end. Removing the readableSha guard turns this red.
  const verdict = classifyPushState({})
  assert.notEqual(verdict.state, PUSH_STATE_PUBLISHED)
  assert.equal(verdict.state, PUSH_STATE_DIVERGED)
  assert.equal(verdict.reason, PUSH_REASON_UNREADABLE)
  assert.equal(verdict.owed, false)
})

test('push state fails closed: an empty or whitespace SHA on either side is unreadable', () => {
  for (const input of [
    { localSha: '', remoteSha: REMOTE },
    { localSha: LOCAL, remoteSha: '' },
    { localSha: '   ', remoteSha: '   ' },
    { localSha: LOCAL, remoteSha: '\n' },
    { localSha: null, remoteSha: REMOTE },
    { localSha: LOCAL, remoteSha: 42 },
  ]) {
    const verdict = classifyPushState(input)
    assert.equal(verdict.state, PUSH_STATE_DIVERGED, JSON.stringify(input))
    assert.equal(verdict.reason, PUSH_REASON_UNREADABLE, JSON.stringify(input))
  }
  // Two whitespace-only SHAs are equal as strings. Trimming before the equality test without
  // rejecting them first would report `published` for a round that read nothing at all.
})

test('push state: only a boolean true asserts an ancestry', () => {
  // An unset shell variable arrives as the empty string, and `'false'` is a non-empty string. A
  // truthiness check would read the second as an assertion and route a diverged branch to a push.
  for (const truthy of ['true', 'false', 1, {}]) {
    const verdict = classifyPushState({
      localSha: LOCAL,
      remoteSha: REMOTE,
      remoteIsAncestorOfLocal: truthy,
    })
    assert.equal(verdict.state, PUSH_STATE_DIVERGED, String(truthy))
    assert.equal(verdict.owed, false, String(truthy))
  }
})

test('push state fails closed: both ancestries asserted at once is unreadable, not a push', () => {
  const verdict = classifyPushState({
    localSha: LOCAL,
    remoteSha: REMOTE,
    remoteIsAncestorOfLocal: true,
    localIsAncestorOfRemote: true,
  })
  assert.equal(verdict.state, PUSH_STATE_DIVERGED)
  assert.equal(verdict.reason, PUSH_REASON_UNREADABLE)
})

test('push state: every frozen state is reachable and renders a distinct non-empty line', () => {
  const reached = new Map()
  for (const input of [
    { localSha: LOCAL, remoteSha: LOCAL },
    { localSha: LOCAL, remoteSha: REMOTE, localIsAncestorOfRemote: true },
    { localSha: LOCAL, remoteSha: REMOTE, remoteIsAncestorOfLocal: true },
    { localSha: LOCAL, remoteSha: REMOTE },
    { localSha: LOCAL, remoteSha: REMOTE, withheld: true },
  ]) {
    const verdict = classifyPushState(input)
    assert.ok(PUSH_STATES.includes(verdict.state))
    assert.ok(PUSH_REASONS.includes(verdict.reason))
    reached.set(verdict.state, verdict.line)
  }
  assert.deepEqual([...reached.keys()].sort(), [...PUSH_STATES].sort())
  // A round that landed no commits must emit the same field SHAPE as one that pushed, and the five
  // values must be tellable apart in that field.
  const lines = [...reached.values()]
  assert.equal(new Set(lines).size, PUSH_STATES.length)
  for (const line of lines) assert.ok(line.trim().length > 0)
})

test('push state: exactly one state owes a push', () => {
  assert.ok(Object.isFrozen(PUSH_STATES))
  const owing = [
    { localSha: LOCAL, remoteSha: LOCAL },
    { localSha: LOCAL, remoteSha: REMOTE, localIsAncestorOfRemote: true },
    { localSha: LOCAL, remoteSha: REMOTE, remoteIsAncestorOfLocal: true },
    { localSha: LOCAL, remoteSha: REMOTE },
    { localSha: LOCAL, remoteSha: REMOTE, withheld: true },
    {},
  ].filter((input) => classifyPushState(input).owed)
  assert.equal(owing.length, 1)
})

// ---------------------------------------------------------------------------
// classifyDeclineReply — the record-before-claim ordering gate.

test('decline reply record-cited: a non-blank id admits the reply', () => {
  const verdict = classifyDeclineReply({ residualRecordId: 'note_123' })
  assert.equal(verdict.admit, true)
  assert.equal(verdict.reason, DECLINE_REPLY_RECORD_CITED)
  assert.equal(verdict.recordId, 'note_123')
})

test('decline reply unbacked-claim: absent, empty and whitespace-only ids all refuse the reply', () => {
  // The id arrives from a command substitution, so a failed write most easily yields '' or '\n'.
  // Dropping the trim would admit a reply citing a record that was never written.
  for (const residualRecordId of [undefined, null, '', '   ', '\n', '\t ', 123, {}]) {
    const verdict = classifyDeclineReply({ residualRecordId })
    assert.equal(verdict.admit, false, JSON.stringify(residualRecordId))
    assert.equal(verdict.reason, DECLINE_REPLY_UNBACKED_CLAIM, JSON.stringify(residualRecordId))
    assert.equal(verdict.recordId, null)
  }
  assert.equal(classifyDeclineReply().admit, false)
  assert.equal(classifyDeclineReply({}).reason, DECLINE_REPLY_UNBACKED_CLAIM)
})

test('decline reply: every frozen reason is reachable', () => {
  assert.ok(Object.isFrozen(DECLINE_REPLY_REASONS))
  const reached = new Set([
    classifyDeclineReply({ residualRecordId: 'n1' }).reason,
    classifyDeclineReply({}).reason,
  ])
  assert.deepEqual([...reached].sort(), [...DECLINE_REPLY_REASONS].sort())
})

// ---------------------------------------------------------------------------
// resolveResidualSink — the named destination, and the explicit degradation.

test('residual sink boss-notes: a resolved binary yields a boss notes add command', () => {
  const verdict = resolveResidualSink({ bossBinary: { ok: true, path: '/usr/local/bin/boss' } })
  assert.equal(verdict.sink, RESIDUAL_SINK_BOSS_NOTES)
  assert.equal(verdict.degraded, false)
  assert.equal(verdict.binaryPath, '/usr/local/bin/boss')
  assert.match(verdict.command, /notes add/)
  assert.ok(verdict.command.includes(`--tag ${RESIDUAL_NOTE_TAG}`))
  // The binary is invoked by the resolved PATH, never as a bare `boss`: two of the resolver's three
  // arms name something a bare `boss` in a shell would not find, or would find as a different file.
  assert.ok(verdict.command.includes('/usr/local/bin/boss'))
  // `--` before the body, because the body is positional and a residual beginning with `--` is
  // otherwise eaten as a flag and the note is never written.
  assert.ok(verdict.command.includes(' -- '))
  assert.ok(verdict.command.indexOf(' -- ') < verdict.command.indexOf(RESIDUAL_BODY_PLACEHOLDER))
  // The body comes from a file, never spliced: residual prose carries apostrophes.
  assert.ok(verdict.command.includes(RESIDUAL_BODY_PLACEHOLDER))
})

test('residual sink boss-notes: a bare path string resolves the same way as a verdict object', () => {
  const verdict = resolveResidualSink({ bossBinary: '/opt/boss' })
  assert.equal(verdict.sink, RESIDUAL_SINK_BOSS_NOTES)
  assert.equal(verdict.degraded, false)
  assert.equal(verdict.binaryPath, '/opt/boss')
})

test('residual sink pr-comment: no binary degrades explicitly and still names a usable sink', () => {
  const verdict = resolveResidualSink({ bossBinary: null })
  assert.equal(verdict.sink, RESIDUAL_SINK_PR_COMMENT)
  assert.equal(verdict.degraded, true)
  assert.equal(verdict.binaryPath, null)
  // A degraded sink must still be usable, or the fallback records nothing at all.
  assert.ok(verdict.command.trim().length > 0)
  assert.ok(verdict.command.includes(RESIDUAL_BODY_PLACEHOLDER))
  assert.ok(verdict.reason.length > 0)
})

test('residual sink fails closed: an unusable resolver verdict degrades rather than assuming boss', () => {
  // This core installs into unrelated repositories. Assuming the binary would fail silently in
  // exactly the repos it ships to, so every shape that is not a usable path degrades.
  for (const bossBinary of [
    null,
    '',
    '   ',
    { ok: false, path: null },
    { ok: false, path: '/nope/boss' },
    { ok: true, path: '' },
    { ok: true, path: '   ' },
    { ok: true },
    {},
    42,
  ]) {
    const verdict = resolveResidualSink({ bossBinary })
    assert.equal(verdict.sink, RESIDUAL_SINK_PR_COMMENT, JSON.stringify(bossBinary))
    assert.equal(verdict.degraded, true, JSON.stringify(bossBinary))
  }
})

test('residual sink: an absent bossBinary key consults the vendored resolver', () => {
  // Detection, not assumption: with an env whose every arm fails, the resolver returns no path and
  // the sink degrades — proving the resolver was actually consulted rather than skipped.
  const verdict = resolveResidualSink({}, { env: { PATH: '' }, cwd: '/' })
  assert.equal(verdict.sink, RESIDUAL_SINK_PR_COMMENT)
  assert.equal(verdict.degraded, true)
})

test('residual sink: every frozen sink is reachable', () => {
  assert.ok(Object.isFrozen(RESIDUAL_SINKS))
  const reached = new Set([
    resolveResidualSink({ bossBinary: '/opt/boss' }).sink,
    resolveResidualSink({ bossBinary: null }).sink,
  ])
  assert.deepEqual([...reached].sort(), [...RESIDUAL_SINKS].sort())
})

// ---------------------------------------------------------------------------
// classifyProblemSources — the widened problem-source list.

test('problem source merge-conflict: a conflict alone is reported', () => {
  const verdict = classifyProblemSources({ conflict: true })
  assert.deepEqual(verdict.sources, [PROBLEM_SOURCE_MERGE_CONFLICT])
  assert.equal(verdict.none, false)
})

test('problem source failing-checks: a failing check alone is reported', () => {
  const verdict = classifyProblemSources({ failingChecks: ['go-lint'] })
  assert.deepEqual(verdict.sources, [PROBLEM_SOURCE_FAILING_CHECKS])
  assert.equal(verdict.none, false)
})

test('problem source review-feedback: an open review thread alone is reported', () => {
  const verdict = classifyProblemSources({ reviewThreads: 2 })
  assert.deepEqual(verdict.sources, [PROBLEM_SOURCE_REVIEW_FEEDBACK])
  assert.equal(verdict.none, false)
})

test('problem source report-only-finding: a report-only finding alone opens a repair cycle', () => {
  // The defect this widening closes: a must-fix finding written only in a build-review report or the
  // PR body matched none of the three literal values, so the round reported None and burned itself.
  const verdict = classifyProblemSources({ reportOnlyFindings: ['H1 tracker adapter resolution'] })
  assert.deepEqual(verdict.sources, [PROBLEM_SOURCE_REPORT_ONLY])
  assert.equal(verdict.none, false)
  assert.notEqual(verdict.line, 'None')
})

test('problem source none: returned only when all four sources are absent', () => {
  const absent = classifyProblemSources({
    conflict: false,
    failingChecks: [],
    reviewThreads: 0,
    reportOnlyFindings: '',
  })
  assert.deepEqual(absent.sources, [PROBLEM_SOURCE_NONE])
  assert.equal(absent.none, true)
  assert.equal(absent.line, 'None')
  assert.deepEqual(classifyProblemSources({}).sources, [PROBLEM_SOURCE_NONE])
  assert.deepEqual(classifyProblemSources().sources, [PROBLEM_SOURCE_NONE])

  // Any single source present removes None entirely — it is never one entry among several.
  for (const key of ['conflict', 'failingChecks', 'reviewThreads', 'reportOnlyFindings']) {
    const verdict = classifyProblemSources({ [key]: true })
    assert.equal(verdict.none, false, key)
    assert.ok(!verdict.sources.includes(PROBLEM_SOURCE_NONE), key)
  }
})

test('problem source fails closed: an unrecognised probe answer is present, not absent', () => {
  // `None` is the report's silence. A probe whose answer this module cannot read must not reach it.
  for (const value of [{}, { total: 0 }, 'unknown', -1, () => {}]) {
    const verdict = classifyProblemSources({ failingChecks: value })
    assert.equal(verdict.none, false, String(value))
    assert.deepEqual(verdict.sources, [PROBLEM_SOURCE_FAILING_CHECKS], String(value))
  }
})

test('problem source: several sources render in report order', () => {
  const verdict = classifyProblemSources({
    conflict: true,
    failingChecks: ['x'],
    reviewThreads: 1,
    reportOnlyFindings: ['y'],
  })
  assert.deepEqual(verdict.sources, [
    PROBLEM_SOURCE_MERGE_CONFLICT,
    PROBLEM_SOURCE_FAILING_CHECKS,
    PROBLEM_SOURCE_REVIEW_FEEDBACK,
    PROBLEM_SOURCE_REPORT_ONLY,
  ])
  assert.equal(verdict.line, 'Merge conflict, Failing tests, Review feedback, Report-only finding')
})

test('problem source: every frozen source is reachable', () => {
  assert.ok(Object.isFrozen(PROBLEM_SOURCES))
  const reached = new Set([
    ...classifyProblemSources({ conflict: true }).sources,
    ...classifyProblemSources({ failingChecks: true }).sources,
    ...classifyProblemSources({ reviewThreads: true }).sources,
    ...classifyProblemSources({ reportOnlyFindings: true }).sources,
    ...classifyProblemSources({}).sources,
  ])
  assert.deepEqual([...reached].sort(), [...PROBLEM_SOURCES].sort())
})

// ---------------------------------------------------------------------------
// CLI

test('cli: each command prints its verdict as JSON and exits zero', () => {
  const push = cli(['push-state', '--in', '/in.json'], {
    '/in.json': JSON.stringify({ localSha: LOCAL, remoteSha: LOCAL }),
  })
  assert.equal(push.code, 0)
  assert.equal(JSON.parse(push.out).state, PUSH_STATE_PUBLISHED)

  const reply = cli(['decline-reply', '--in', '/in.json'], {
    '/in.json': JSON.stringify({ residualRecordId: 'n1' }),
  })
  assert.equal(reply.code, 0)
  assert.equal(JSON.parse(reply.out).admit, true)

  const sink = cli(['residual-sink', '--in', '/in.json'], {
    '/in.json': JSON.stringify({ bossBinary: '/opt/boss' }),
  })
  assert.equal(sink.code, 0)
  assert.equal(JSON.parse(sink.out).sink, RESIDUAL_SINK_BOSS_NOTES)

  const sources = cli(['problem-sources', '--in', '/in.json'], {
    '/in.json': JSON.stringify({ reportOnlyFindings: ['H1'] }),
  })
  assert.equal(sources.code, 0)
  assert.deepEqual(JSON.parse(sources.out).sources, [PROBLEM_SOURCE_REPORT_ONLY])
})

test('cli: a classifying command refuses to run with no input rather than classifying {}', () => {
  // Every one of these has a real verdict for the empty object, so a permissive CLI would exit zero
  // and print a plausible answer nobody derived — the silent default this module exists to end.
  for (const command of ['push-state', 'decline-reply', 'problem-sources']) {
    const result = cli([command])
    assert.equal(result.code, 2, command)
    assert.equal(result.out, '', command)
  }
  // residual-sink is the exception: with no input it DETECTS the binary, which is its normal form.
  const detected = cli(['residual-sink'])
  assert.equal(detected.code, 0)
  assert.ok(RESIDUAL_SINKS.includes(JSON.parse(detected.out).sink))
})

test('cli: usage, unknown-option and unreadable-input errors exit 2 and print nothing to stdout', () => {
  for (const argv of [
    [],
    ['nope', '--in', '/in.json'],
    ['push-state', '--nope', '/in.json'],
    ['push-state', '--in'],
    ['push-state', '--in', '/missing.json'],
    ['push-state', '--in', '/in.json', 'extra'],
  ]) {
    const result = cli(argv, { '/in.json': '{}' })
    assert.equal(result.code, 2, JSON.stringify(argv))
    assert.equal(result.out, '', JSON.stringify(argv))
    assert.ok(result.err.length > 0, JSON.stringify(argv))
  }
})

test('cli: malformed JSON and non-object input are refused, never coerced', () => {
  for (const text of ['not json', '[]', '"a string"', 'null', '7']) {
    const result = cli(['push-state', '--in', '/in.json'], { '/in.json': text })
    assert.equal(result.code, 2, text)
    assert.equal(result.out, '', text)
  }
})
