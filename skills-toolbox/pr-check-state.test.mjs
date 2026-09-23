#!/usr/bin/env node

import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { readFileSync, mkdtempSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import path from 'node:path'
import test from 'node:test'
import { fileURLToPath } from 'node:url'

import {
  CHECK_REASONS,
  CHECK_STATES,
  RUN_LIVENESS,
  classifyChecks,
  compareInstants,
  contextNames,
  diffCheckSets,
  epochMs,
  isGreen,
  mergeStateVerdict,
  ABSENT_GATE_REMEDIES,
  absentGateRemedy,
  PROVES_GREEN_REASONS,
  provesGreen,
  provesGreenAgrees,
  provesGreenReason,
  runLiveness,
  verdictAt,
} from './pr-check-state.mjs'

const SCRIPT_PATH = fileURLToPath(new URL('./pr-check-state.mjs', import.meta.url))
const GO_VERDICT_SOURCE = fileURLToPath(
  new URL('../lib/bossalib/vcs/checks_verdict.go', import.meta.url),
)

// ---------------------------------------------------------------------------
// Vocabulary

test('CHECK_STATES is frozen and names exactly green, failing, pending, unknown', () => {
  assert.ok(Object.isFrozen(CHECK_STATES))
  assert.deepEqual([...Object.values(CHECK_STATES)].sort(), [
    'failing',
    'green',
    'pending',
    'unknown',
  ])
})

test('CHECK_REASONS is frozen and adds exactly the two agent-side reasons', () => {
  assert.ok(Object.isFrozen(CHECK_REASONS))
  const values = new Set(Object.values(CHECK_REASONS))
  assert.ok(values.has('absent-gate'), 'absent-gate must be in the reason vocabulary')
  assert.ok(values.has('advisory-unsettled'), 'advisory-unsettled must be in the reason vocabulary')
  assert.equal(values.size, 10)
})

// The pin. This reads the Go source at test time rather than restating its tokens, which is what
// makes a rename on EITHER side fail a gate instead of drifting silently into two different
// meanings of the word green.
test('every CheckVerdictReason* token in the Go verdict is present in CHECK_REASONS', () => {
  const go = readFileSync(GO_VERDICT_SOURCE, 'utf8')
  const tokens = [...go.matchAll(/CheckVerdictReason[A-Za-z]+\s*=\s*"([a-z0-9-]+)"/g)].map(
    (m) => m[1],
  )
  // Fail closed: an empty match set would make this assertion vacuous, and a moved or renamed Go
  // file is exactly the drift this test exists to catch.
  assert.ok(
    tokens.length >= 8,
    `expected at least 8 CheckVerdictReason* tokens in ${GO_VERDICT_SOURCE}, found ${tokens.length}`,
  )
  const values = new Set(Object.values(CHECK_REASONS))
  for (const token of tokens) {
    assert.ok(
      values.has(token),
      `Go reason ${JSON.stringify(token)} is missing from CHECK_REASONS — the two definitions have drifted`,
    )
  }
  // stale-sha is named explicitly so the required-proof token is visible in the run output.
  assert.ok(tokens.includes('stale-sha'))
})

// ---------------------------------------------------------------------------
// classifyChecks — the table

const checkRun = (name, status, conclusion) => ({ name, status, conclusion })
const bucketRow = (name, state, bucket) => ({ name, state, bucket })

test('classifyChecks — a passing set on the head SHA is green with reason ok', () => {
  const verdict = classifyChecks({
    headSHA: 'abc123',
    observedSHA: 'abc123',
    checkRuns: { check_runs: [checkRun('test-go', 'completed', 'success')] },
    priorContexts: ['test-go'],
  })
  assert.equal(verdict.state, CHECK_STATES.GREEN)
  assert.equal(verdict.reason, CHECK_REASONS.OK)
  assert.ok(isGreen(verdict))
  assert.ok(provesGreen(verdict))
})

test('classifyChecks — a failing node dominates every other reading', () => {
  const verdict = classifyChecks({
    checkRuns: {
      check_runs: [
        checkRun('test-go', 'completed', 'success'),
        checkRun('lint', 'completed', 'failure'),
        checkRun('web-e2e', 'in_progress', null),
      ],
    },
  })
  assert.equal(verdict.state, CHECK_STATES.FAILING)
  assert.equal(verdict.reason, CHECK_REASONS.FAILED)
  assert.equal(isGreen(verdict), false)
})

test('classifyChecks — a pending node holds the set at pending, however many passes sit beside it', () => {
  const verdict = classifyChecks({
    checkRuns: {
      check_runs: [
        checkRun('test-go', 'completed', 'success'),
        checkRun('release-gates', 'queued', null),
      ],
    },
    priorContexts: ['test-go', 'release-gates'],
  })
  assert.equal(verdict.state, CHECK_STATES.PENDING)
  assert.equal(verdict.reason, CHECK_REASONS.PENDING)
  assert.equal(isGreen(verdict), false)
})

// A GraphQL `StatusContext` (an external commit-status reporter: CircleCI, Buildkite, a bot) carries
// its whole state in `state`, not `status`/`conclusion`. Every fixture above uses the CheckRun
// shape, so nothing else here exercises the status half of the rollup vocabulary.
test('classifyChecks — a pending commit STATUS in the rollup is pending, not unclassified', () => {
  for (const state of ['PENDING', 'EXPECTED']) {
    const verdict = classifyChecks({
      headSha: 'abc123',
      sha: 'abc123',
      rollup: {
        statusCheckRollup: [
          { __typename: 'StatusContext', context: 'ci/circleci', state },
          { __typename: 'CheckRun', name: 'test-go', status: 'COMPLETED', conclusion: 'SUCCESS' },
        ],
      },
    })
    assert.equal(verdict.state, CHECK_STATES.PENDING, `${state} must classify as pending`)
    assert.equal(verdict.reason, CHECK_REASONS.PENDING, `${state} must report the pending reason`)
    assert.equal(verdict.pending, 1, `${state} must be counted as one pending entry`)
    assert.equal(verdict.unclassified, 0, `${state} must not be counted as unclassified`)
    assert.equal(isGreen(verdict), false)
  }
})

test('classifyChecks — a commit STATUS failure still dominates a pending sibling status', () => {
  const verdict = classifyChecks({
    headSha: 'abc123',
    sha: 'abc123',
    rollup: {
      statusCheckRollup: [
        { __typename: 'StatusContext', context: 'ci/circleci', state: 'PENDING' },
        { __typename: 'StatusContext', context: 'ci/other', state: 'FAILURE' },
      ],
    },
  })
  assert.equal(verdict.state, CHECK_STATES.FAILING)
  assert.equal(verdict.reason, CHECK_REASONS.FAILED)
})

// The recorded skip-heavy observation: 8 pass / 5 skipping plus one NEUTRAL, where four of the
// skips were substantive gates. The bucket view alone cannot separate a gate that ran and passed
// from one that never attached, so the same payload reads green — and `provesGreen` is what says
// the completeness claim was never established.
test('classifyChecks — a skip-heavy bucket-only set is green but does not PROVE green', () => {
  const buckets = [
    ...['a', 'b', 'c', 'd', 'e', 'f', 'g', 'h'].map((n) =>
      bucketRow(`pass-${n}`, 'SUCCESS', 'pass'),
    ),
    ...['test-go', 'web-e2e', 'dedup', 'release-gates', 'docs'].map((n) =>
      bucketRow(n, 'SKIPPED', 'skipping'),
    ),
    bucketRow('advisory', 'NEUTRAL', 'skipping'),
  ]
  const verdict = classifyChecks({ buckets })
  assert.equal(verdict.state, CHECK_STATES.GREEN)
  assert.equal(verdict.passed, 8)
  assert.equal(verdict.skipped, 6)
  assert.equal(verdict.priorKnown, false)
  assert.equal(provesGreen(verdict), false, 'no prior SHA recorded ⇒ completeness is unproven')
})

test('classifyChecks — an all-skipped set is unknown/no-gate-ran unless the caller accepts it', () => {
  const buckets = ['test-go', 'web-e2e'].map((n) => bucketRow(n, 'SKIPPED', 'skipping'))
  const strict = classifyChecks({ buckets, priorContexts: ['test-go', 'web-e2e'] })
  assert.equal(strict.state, CHECK_STATES.UNKNOWN)
  assert.equal(strict.reason, CHECK_REASONS.NO_GATE_RAN)
  assert.equal(isGreen(strict), false)

  const accepted = classifyChecks({
    buckets,
    priorContexts: ['test-go', 'web-e2e'],
    acceptNoGateRan: true,
  })
  assert.equal(accepted.state, CHECK_STATES.GREEN)
  assert.equal(accepted.reason, CHECK_REASONS.NO_GATE_RAN)
  assert.equal(provesGreen(accepted), false, 'nothing ran, so nothing was demonstrated')
})

// AC 3 — the recorded 18 → 9 shrink. A path-filtered follow-up push drops the jobs entirely, and
// waiting on them never resolves.
test('classifyChecks — a gate on the prior SHA and missing on the head is pending/absent-gate, never green', () => {
  const prior = Array.from({ length: 18 }, (_, i) => `gate-${i + 1}`)
  const head = prior.slice(0, 9).map((n) => checkRun(n, 'completed', 'success'))
  const verdict = classifyChecks({
    headSHA: 'head1',
    observedSHA: 'head1',
    checkRuns: { check_runs: head },
    priorContexts: prior,
  })
  assert.equal(verdict.state, CHECK_STATES.PENDING)
  assert.equal(verdict.reason, CHECK_REASONS.ABSENT_GATE)
  assert.equal(isGreen(verdict), false)
  assert.equal(verdict.absent.length, 9)
  assert.ok(verdict.absent.includes('gate-18'))
})

// AC 4 — PR #2420's shape.
test('classifyChecks — a null-shaped rollup node whose named context is successful is green, not unknown', () => {
  const verdict = classifyChecks({
    headSHA: 'sha2420',
    observedSHA: 'sha2420',
    rollup: {
      statusCheckRollup: [
        { name: 'test-go', status: 'COMPLETED', conclusion: 'SUCCESS' },
        { name: 'release-gates', status: 'COMPLETED', conclusion: null },
      ],
    },
    buckets: [
      bucketRow('test-go', 'SUCCESS', 'pass'),
      bucketRow('release-gates', 'SUCCESS', 'pass'),
    ],
    priorContexts: ['test-go', 'release-gates'],
  })
  assert.equal(verdict.state, CHECK_STATES.GREEN)
  assert.equal(verdict.reason, CHECK_REASONS.OK)
  assert.equal(verdict.unclassified, 0)
  assert.ok(provesGreen(verdict))
})

test('classifyChecks — a null-shaped rollup node with NO reconciling context stays unknown', () => {
  const verdict = classifyChecks({
    rollup: {
      statusCheckRollup: [
        { name: 'test-go', status: 'COMPLETED', conclusion: 'SUCCESS' },
        { name: 'release-gates', status: 'COMPLETED', conclusion: null },
      ],
    },
  })
  assert.equal(verdict.state, CHECK_STATES.UNKNOWN)
  assert.equal(verdict.reason, CHECK_REASONS.UNCLASSIFIED)
  assert.equal(isGreen(verdict), false)
})

test('classifyChecks — a FAILURE never hides behind a same-named SUCCESS', () => {
  const verdict = classifyChecks({
    rollup: { statusCheckRollup: [{ name: 'test', status: 'COMPLETED', conclusion: 'FAILURE' }] },
    buckets: [bucketRow('test', 'SUCCESS', 'pass')],
  })
  assert.equal(verdict.state, CHECK_STATES.FAILING)
})

// AC 7 — the three fail-closed shapes.
test('classifyChecks — an unreadable read is unknown/unreadable and not green', () => {
  const verdict = classifyChecks({
    checkRuns: { check_runs: [checkRun('test-go', 'completed', 'success')] },
    readError: new Error('HTTP 502'),
  })
  assert.equal(verdict.state, CHECK_STATES.UNKNOWN)
  assert.equal(verdict.reason, CHECK_REASONS.UNREADABLE)
  assert.equal(isGreen(verdict), false)
  assert.equal(provesGreen(verdict), false)
})

test('classifyChecks — an unclassifiable state is unknown/unclassified and not green', () => {
  const verdict = classifyChecks({
    checkRuns: { check_runs: [checkRun('test-go', 'completed', 'invented_conclusion')] },
  })
  assert.equal(verdict.state, CHECK_STATES.UNKNOWN)
  assert.equal(verdict.reason, CHECK_REASONS.UNCLASSIFIED)
  assert.equal(isGreen(verdict), false)
})

test('classifyChecks — a verdict bound to a SHA other than the head is unknown/stale-sha', () => {
  const verdict = classifyChecks({
    headSHA: 'newhead',
    observedSHA: 'oldhead',
    checkRuns: { check_runs: [checkRun('test-go', 'completed', 'success')] },
    priorContexts: ['test-go'],
  })
  assert.equal(verdict.state, CHECK_STATES.UNKNOWN)
  assert.equal(verdict.reason, CHECK_REASONS.STALE_SHA)
  assert.equal(isGreen(verdict), false)
  assert.equal(provesGreen(verdict), false)
})

test('verdictAt — rebinding a green verdict to a different SHA yields unknown/stale-sha', () => {
  const green = classifyChecks({
    headSHA: 'sha-a',
    observedSHA: 'sha-a',
    checkRuns: { check_runs: [checkRun('test-go', 'completed', 'success')] },
    priorContexts: ['test-go'],
  })
  assert.ok(isGreen(green))
  const rebound = verdictAt(green, 'sha-b')
  assert.equal(rebound.state, CHECK_STATES.UNKNOWN)
  assert.equal(rebound.reason, CHECK_REASONS.STALE_SHA)
  assert.equal(isGreen(rebound), false)
  assert.equal(isGreen(verdictAt(green, 'sha-a')), true)
})

test('classifyChecks — an empty read is unknown/no-checks, never a pass', () => {
  const verdict = classifyChecks({ rollup: { statusCheckRollup: [] } })
  assert.equal(verdict.state, CHECK_STATES.UNKNOWN)
  assert.equal(verdict.reason, CHECK_REASONS.NO_CHECKS)
  assert.equal(verdict.total, 0)
  assert.equal(isGreen(verdict), false)
})

// ---------------------------------------------------------------------------
// diffCheckSets

test('diffCheckSets — reports ran, pending and absent, and flags an unknown prior side', () => {
  const head = {
    check_runs: [checkRun('test-go', 'completed', 'success'), checkRun('lint', 'queued', null)],
  }
  const withPrior = diffCheckSets({ head, prior: ['test-go', 'lint', 'web-e2e'] })
  assert.deepEqual(withPrior.ran, ['test-go'])
  assert.deepEqual(withPrior.pending, ['lint'])
  assert.deepEqual(withPrior.absent, ['web-e2e'])
  assert.equal(withPrior.priorKnown, true)

  const withoutPrior = diffCheckSets({ head })
  assert.deepEqual(withoutPrior.absent, [])
  assert.equal(
    withoutPrior.priorKnown,
    false,
    'no prior side ⇒ completeness unproven, which is not the same as "nothing absent"',
  )
})

// The absent set requires EVIDENCE the prior context ran. A path-filtered follow-up push shrinks
// the check set, and comparing head against prior is what tells an absent job from a queued one —
// but a context that attached to the prior SHA and deliberately did NOT run proved nothing about
// that SHA either, so counting its disappearance turns an all-green head into an `absent-gate`
// verdict that never resolves by waiting.

test('diffCheckSets — a prior context that classified skipped is not absent', () => {
  const head = { check_runs: [checkRun('test-go', 'completed', 'success')] }
  const prior = {
    check_runs: [
      checkRun('test-go', 'completed', 'success'),
      checkRun('web-e2e', 'completed', 'skipped'),
    ],
  }
  assert.deepEqual(diffCheckSets({ head, prior }).absent, [])
  assert.equal(diffCheckSets({ head, prior }).priorKnown, true, 'the prior side is still known')
})

test('diffCheckSets — a prior context that classified SUCCESS is still absent', () => {
  // The converse direction. The discount must not widen into the genuine-lost-gate case, which is
  // the whole reason the two-SHA comparison exists.
  const head = { check_runs: [checkRun('test-go', 'completed', 'success')] }
  const prior = {
    check_runs: [
      checkRun('test-go', 'completed', 'success'),
      checkRun('web-e2e', 'completed', 'success'),
    ],
  }
  assert.deepEqual(diffCheckSets({ head, prior }).absent, ['web-e2e'])
})

test('diffCheckSets — a prior side of bare names carries no conclusion, so it keeps counting', () => {
  // Fail-closed: a name is not evidence that the gate was skipped, and the shipped recipes pass a
  // bare `--prior` name list. Discounting one would silence a genuinely lost gate.
  const head = { check_runs: [checkRun('test-go', 'completed', 'success')] }
  assert.deepEqual(diffCheckSets({ head, prior: ['test-go', 'web-e2e'] }).absent, ['web-e2e'])
})

test('classifyChecks — an all-green head whose only missing prior context was skipped is green/ok', () => {
  const verdict = classifyChecks({
    headSHA: 'abc',
    observedSHA: 'abc',
    checkRuns: { check_runs: [checkRun('test-go', 'completed', 'success')] },
    priorContexts: {
      check_runs: [
        checkRun('test-go', 'completed', 'success'),
        checkRun('web-e2e', 'completed', 'skipped'),
      ],
    },
  })
  assert.equal(verdict.state, CHECK_STATES.GREEN)
  assert.equal(verdict.reason, CHECK_REASONS.OK)
  assert.deepEqual(verdict.absent, [])
  assert.equal(provesGreen(verdict), true)
  assert.equal(provesGreenReason(verdict), PROVES_GREEN_REASONS.OK)
})

test('classifyChecks — a SUCCESS prior context absent from the head still reports absent-gate', () => {
  const verdict = classifyChecks({
    headSHA: 'abc',
    observedSHA: 'abc',
    checkRuns: { check_runs: [checkRun('test-go', 'completed', 'success')] },
    priorContexts: {
      check_runs: [
        checkRun('test-go', 'completed', 'success'),
        checkRun('web-e2e', 'completed', 'success'),
      ],
    },
  })
  assert.equal(verdict.state, CHECK_STATES.PENDING)
  assert.equal(verdict.reason, CHECK_REASONS.ABSENT_GATE)
  assert.deepEqual(verdict.absent, ['web-e2e'])
  assert.equal(provesGreenReason(verdict), PROVES_GREEN_REASONS.INCOMPLETE_HEAD_SET)
})

// `absent-gate` names its remedy. The verdict says a gate the prior head carried is missing from
// this one, but two different situations wear that shape and only one is worth re-triggering: a
// context that CANNOT attach to this head (a workflow whose triggers a push cannot produce, so a
// draft PR never gets it) versus a job that genuinely went missing. Neither resolves by waiting.
//
// ADDITIVE ONLY. The structural input is caller-supplied and defaults to empty, and the verdict's
// existing fields keep their current values for every existing caller.

test('ABSENT_GATE_REMEDIES is frozen and names exactly the four outcomes', () => {
  assert.ok(Object.isFrozen(ABSENT_GATE_REMEDIES))
  assert.deepEqual([...Object.values(ABSENT_GATE_REMEDIES)].sort(), [
    'mixed-absent-gates',
    'none',
    're-trigger-absent-gate',
    'structurally-unreachable',
  ])
})

function absentGateVerdict(structurallyAbsentContexts) {
  return classifyChecks({
    headSHA: 'abc',
    observedSHA: 'abc',
    checkRuns: { check_runs: [checkRun('test-go', 'completed', 'success')] },
    priorContexts: ['test-go', 'pr_agent', 'web-e2e'],
    ...(structurallyAbsentContexts === undefined ? {} : { structurallyAbsentContexts }),
  })
}

test('absentGateRemedy — with no caller input every absent gate is worth re-triggering', () => {
  const verdict = absentGateVerdict(undefined)
  assert.equal(verdict.reason, CHECK_REASONS.ABSENT_GATE)
  assert.deepEqual(verdict.absent, ['pr_agent', 'web-e2e'])
  assert.deepEqual(verdict.structurallyAbsent, [], 'the input defaults to empty')
  assert.equal(absentGateRemedy(verdict), ABSENT_GATE_REMEDIES.RETRIGGER)
})

test('absentGateRemedy — an all-structural absent set is structurally unreachable', () => {
  const verdict = absentGateVerdict(['pr_agent', 'web-e2e'])
  assert.equal(absentGateRemedy(verdict), ABSENT_GATE_REMEDIES.STRUCTURAL)
  assert.deepEqual(verdict.structurallyAbsent, ['pr_agent', 'web-e2e'])
})

test('absentGateRemedy — a partly-structural absent set is mixed, never silenced', () => {
  const verdict = absentGateVerdict(['pr_agent'])
  assert.equal(absentGateRemedy(verdict), ABSENT_GATE_REMEDIES.MIXED)
  assert.deepEqual(verdict.structurallyAbsent, ['pr_agent'])
  assert.deepEqual(verdict.absent, ['pr_agent', 'web-e2e'], 'absent is unchanged')
})

test('absentGateRemedy — a verdict that is not absent-gate has no remedy to name', () => {
  for (const verdict of [
    classifyChecks({ rollup: { statusCheckRollup: [{ name: 'test-go', conclusion: 'SUCCESS' }] } }),
    classifyChecks({ rollup: { statusCheckRollup: [{ name: 'test-go', conclusion: 'FAILURE' }] } }),
    classifyChecks(),
    null,
    undefined,
  ]) {
    assert.equal(absentGateRemedy(verdict), ABSENT_GATE_REMEDIES.NONE)
  }
})

test('the structural input is additive — it changes no existing field or predicate', () => {
  // The one invariant that makes this safe to ship: a caller can use the input to silence a real
  // missing gate in the REMEDY, and must not be able to use it to change the verdict.
  const without = absentGateVerdict(undefined)
  const withAll = absentGateVerdict(['pr_agent', 'web-e2e'])
  for (const key of ['state', 'reason', 'total', 'passed', 'pending', 'priorKnown']) {
    assert.equal(withAll[key], without[key], key)
  }
  assert.deepEqual(withAll.absent, without.absent)
  assert.equal(isGreen(withAll), isGreen(without))
  assert.equal(provesGreen(withAll), provesGreen(without))
  assert.equal(provesGreenReason(withAll), provesGreenReason(without))
})

test('CLI classify — the absent-gate remedy is printed beside the existing reported fields', () => {
  const base = [
    'classify',
    '--head-sha',
    'abc',
    '--observed-sha',
    'abc',
    '--check-runs',
    JSON.stringify({ check_runs: [checkRun('test-go', 'completed', 'success')] }),
    '--prior',
    JSON.stringify(['test-go', 'pr_agent']),
  ]

  const plain = JSON.parse(runCli(base).stdout)
  assert.equal(plain.reason, CHECK_REASONS.ABSENT_GATE)
  assert.equal(plain.absentGateRemedy, ABSENT_GATE_REMEDIES.RETRIGGER)
  assert.equal(plain.provesGreen, false)

  const declared = runCli([...base, '--structurally-absent', 'pr_agent'])
  assert.equal(declared.status, 0, declared.stderr)
  const parsed = JSON.parse(declared.stdout)
  assert.equal(parsed.absentGateRemedy, ABSENT_GATE_REMEDIES.STRUCTURAL)
  assert.deepEqual(parsed.absent, ['pr_agent'], 'the absent set itself is unchanged')
  assert.deepEqual(parsed.structurallyAbsent, ['pr_agent'])
})

test('contextNames — extracts names from all three payload shapes and from a bare name list', () => {
  assert.deepEqual(contextNames(['a', 'b']), ['a', 'b'])
  assert.deepEqual(contextNames({ statusCheckRollup: [{ name: 'roll' }] }), ['roll'])
  assert.deepEqual(contextNames({ check_runs: [{ name: 'run' }] }), ['run'])
  assert.deepEqual(contextNames([{ name: 'bucket', bucket: 'pass' }]), ['bucket'])
  assert.deepEqual(contextNames(null), [])
})

// ---------------------------------------------------------------------------
// mergeStateVerdict — AC 5

test('mergeStateVerdict — UNSTABLE with zero failures and zero threads is non-blocking and pending', () => {
  const checkVerdict = classifyChecks({
    checkRuns: { check_runs: [checkRun('test-go', 'in_progress', null)] },
  })
  const verdict = mergeStateVerdict({
    mergeState: 'UNSTABLE',
    checkVerdict,
    unresolvedThreads: 0,
  })
  assert.equal(verdict.blocking, false, 'UNSTABLE from pending checks must not open a repair cycle')
  assert.equal(verdict.state, CHECK_STATES.PENDING)
  assert.equal(verdict.reason, CHECK_REASONS.PENDING)
})

test('mergeStateVerdict — the post-ready degrade is pending/advisory-unsettled, not red CI', () => {
  const green = classifyChecks({
    headSHA: 'sha1',
    observedSHA: 'sha1',
    checkRuns: { check_runs: [checkRun('test-go', 'completed', 'success')] },
    priorContexts: ['test-go'],
  })
  assert.ok(isGreen(green), 'the branch was certified green before the ready call')
  const verdict = mergeStateVerdict({
    mergeState: 'UNSTABLE',
    checkVerdict: green,
    unresolvedThreads: 0,
    readiedThisRun: true,
  })
  assert.equal(verdict.blocking, false)
  assert.equal(verdict.state, CHECK_STATES.PENDING)
  assert.equal(verdict.reason, CHECK_REASONS.ADVISORY_UNSETTLED)
})

test('mergeStateVerdict — a failing check verdict, unresolved threads, and DIRTY all block', () => {
  const failing = classifyChecks({
    checkRuns: { check_runs: [checkRun('test-go', 'completed', 'failure')] },
  })
  for (const input of [
    { mergeState: 'UNSTABLE', checkVerdict: failing },
    { mergeState: 'UNSTABLE', unresolvedThreads: 2 },
    { mergeState: 'DIRTY' },
    { mergeState: 'CONFLICTING' },
  ]) {
    const verdict = mergeStateVerdict(input)
    assert.equal(verdict.blocking, true, `${JSON.stringify(input)} must block`)
    assert.equal(verdict.state, CHECK_STATES.FAILING)
  }
})

test('mergeStateVerdict — CLEAN never upgrades a non-green check verdict, and UNKNOWN is unknown', () => {
  const pending = classifyChecks({
    checkRuns: { check_runs: [checkRun('test-go', 'queued', null)] },
  })
  const clean = mergeStateVerdict({ mergeState: 'CLEAN', checkVerdict: pending })
  assert.equal(clean.blocking, false)
  assert.equal(clean.state, CHECK_STATES.PENDING)

  const unreadable = mergeStateVerdict({ mergeState: 'UNKNOWN' })
  assert.equal(unreadable.state, CHECK_STATES.UNKNOWN)
  assert.equal(unreadable.reason, CHECK_REASONS.UNREADABLE)
  assert.equal(unreadable.blocking, false, 'unknown is never green and never red')

  const invented = mergeStateVerdict({ mergeState: 'SOMETHING_NEW' })
  assert.equal(invented.state, CHECK_STATES.UNKNOWN)
  assert.equal(invented.reason, CHECK_REASONS.UNCLASSIFIED)
})

test('mergeStateVerdict — BLOCKED and BEHIND are unsettled, not red', () => {
  for (const state of ['BLOCKED', 'BEHIND']) {
    const verdict = mergeStateVerdict({ mergeState: state })
    assert.equal(verdict.blocking, false)
    assert.equal(verdict.state, CHECK_STATES.PENDING)
  }
})

// ---------------------------------------------------------------------------
// runLiveness / instants — AC 6

// The recorded pair: `git show -s --format=%cI` renders in the commit's local offset while the
// GitHub API returns Zulu. 13:35:33+09:00 is 04:35:33Z, so it precedes 04:42:51Z by instant — and
// FOLLOWS it as a string. This test asserts both halves, which is what proves the epoch parse is
// load-bearing rather than incidental.
const LOCAL_OFFSET_TS = '2026-08-09T13:35:33+09:00'
const ZULU_TS = '2026-08-09T04:42:51Z'

test('compareInstants — a +09:00 timestamp orders against a Z timestamp by instant, and a string compare gives the OPPOSITE answer', () => {
  assert.equal(
    compareInstants(LOCAL_OFFSET_TS, ZULU_TS),
    -1,
    `${LOCAL_OFFSET_TS} is 04:35:33Z, which precedes ${ZULU_TS}`,
  )
  // The falsifier: the same pair, compared the way the recorded defect compared them.
  const stringOrder = LOCAL_OFFSET_TS < ZULU_TS ? -1 : LOCAL_OFFSET_TS > ZULU_TS ? 1 : 0
  assert.equal(stringOrder, 1, 'a lexicographic compare puts the +09:00 side LAST')
  assert.notEqual(
    stringOrder,
    compareInstants(LOCAL_OFFSET_TS, ZULU_TS),
    'the string compare must give the opposite answer, or this comparison is not load-bearing',
  )
  assert.equal(compareInstants(ZULU_TS, LOCAL_OFFSET_TS), 1)
  assert.equal(compareInstants(ZULU_TS, ZULU_TS), 0)
})

test('compareInstants — an unparseable side is null, never 0', () => {
  assert.equal(compareInstants('not-a-time', ZULU_TS), null)
  assert.equal(compareInstants(ZULU_TS, undefined), null)
  assert.equal(epochMs('not-a-time'), null)
  assert.equal(epochMs(ZULU_TS), Date.parse(ZULU_TS))
})

test('runLiveness — orders a +09:00 start against a Z update by instant', () => {
  const live = runLiveness({
    startedAt: LOCAL_OFFSET_TS,
    updatedAt: ZULU_TS,
    now: '2026-08-09T04:45:00Z',
  })
  assert.equal(live.state, RUN_LIVENESS.ALIVE)
  // Instant ordering: the update is 7m18s AFTER the start, so elapsed exceeds sinceUpdate. Compared
  // as strings the update sorts BEFORE the start, which would make the update look pre-run.
  assert.ok(live.elapsedMs > live.sinceUpdateMs, 'the Z update must land after the +09:00 start')
  assert.equal(live.elapsedMs, Date.parse('2026-08-09T04:45:00Z') - Date.parse(LOCAL_OFFSET_TS))
  assert.equal(live.sinceUpdateMs, Date.parse('2026-08-09T04:45:00Z') - Date.parse(ZULU_TS))
})

test('runLiveness — a 5-minute run updated 4 minutes ago is alive; a 70-minute run never updated is stalled', () => {
  const now = Date.parse('2026-09-04T09:05:00Z')
  const alive = runLiveness({
    startedAt: '2026-09-04T09:00:00Z',
    updatedAt: '2026-09-04T09:01:00Z',
    now,
  })
  assert.equal(alive.state, RUN_LIVENESS.ALIVE)
  assert.equal(alive.elapsedMs, 5 * 60 * 1000)
  assert.equal(alive.sinceUpdateMs, 4 * 60 * 1000)

  const wedged = runLiveness({
    startedAt: '2026-09-04T07:55:00Z',
    updatedAt: null,
    now,
  })
  assert.equal(wedged.state, RUN_LIVENESS.STALLED)
  assert.equal(wedged.elapsedMs, 70 * 60 * 1000)
  assert.equal(wedged.sinceUpdateMs, 70 * 60 * 1000)
})

test('runLiveness — an unparseable timestamp is unknown, never alive', () => {
  const verdict = runLiveness({ startedAt: 'whenever', updatedAt: ZULU_TS, now: ZULU_TS })
  assert.equal(verdict.state, RUN_LIVENESS.UNKNOWN)
  assert.equal(verdict.elapsedMs, null)
})

// ---------------------------------------------------------------------------
// CLI

function runCli(args, input) {
  return spawnSync(process.execPath, [SCRIPT_PATH, ...args], { encoding: 'utf8', input })
}

test('CLI classify — reads payload files and prints one JSON line', () => {
  const dir = mkdtempSync(path.join(tmpdir(), 'pr-check-state-'))
  const checksPath = path.join(dir, 'checks.json')
  const priorPath = path.join(dir, 'prior.json')
  writeFileSync(
    checksPath,
    JSON.stringify({ check_runs: [checkRun('test-go', 'completed', 'success')] }),
  )
  writeFileSync(priorPath, JSON.stringify(['test-go', 'web-e2e']))

  const result = runCli([
    'classify',
    '--head-sha',
    'abc',
    '--observed-sha',
    'abc',
    '--check-runs',
    checksPath,
    '--prior',
    priorPath,
  ])
  assert.equal(result.status, 0, result.stderr)
  assert.equal(result.stdout.trimEnd().split('\n').length, 1, 'exactly one JSON line')
  const parsed = JSON.parse(result.stdout)
  assert.equal(parsed.state, CHECK_STATES.PENDING)
  assert.equal(parsed.reason, CHECK_REASONS.ABSENT_GATE)
  assert.deepEqual(parsed.absent, ['web-e2e'])
  assert.equal(parsed.green, false)
  assert.equal(parsed.provesGreen, false)
})

test('CLI classify — reads a rollup from stdin', () => {
  const result = runCli(
    ['classify', '--rollup', '-'],
    JSON.stringify({ statusCheckRollup: [{ name: 'test-go', conclusion: 'SUCCESS' }] }),
  )
  assert.equal(result.status, 0, result.stderr)
  const parsed = JSON.parse(result.stdout)
  assert.equal(parsed.state, CHECK_STATES.GREEN)
  assert.equal(parsed.reason, CHECK_REASONS.OK)
})

test('CLI merge-state — the post-ready UNSTABLE degrade prints advisory-unsettled', () => {
  const result = runCli([
    'merge-state',
    '--merge-state',
    'UNSTABLE',
    '--check-state',
    'green',
    '--check-reason',
    'ok',
    '--readied-this-run',
  ])
  assert.equal(result.status, 0, result.stderr)
  const parsed = JSON.parse(result.stdout)
  assert.equal(parsed.blocking, false)
  assert.equal(parsed.reason, CHECK_REASONS.ADVISORY_UNSETTLED)
})

test('CLI liveness — prints the instant-ordered verdict', () => {
  const result = runCli([
    'liveness',
    '--started-at',
    LOCAL_OFFSET_TS,
    '--updated-at',
    ZULU_TS,
    '--now',
    '2026-08-09T04:45:00Z',
  ])
  assert.equal(result.status, 0, result.stderr)
  const parsed = JSON.parse(result.stdout)
  assert.equal(parsed.state, RUN_LIVENESS.ALIVE)
})

test('CLI — an unknown command exits 1', () => {
  const result = runCli(['bogus'])
  assert.equal(result.status, 1)
  assert.match(result.stderr, /unknown command/)
})

// Per-verb flag validation. `parseFlags` accepts any `--name value` pair and each verb read only
// the keys it knew, so an unrecognised name landed in the bag and was never looked at. The verb
// that makes this a false GREEN is `classify`: the stale-SHA arm requires BOTH SHA fields
// non-empty, so a dropped `--observed-sha` skips it entirely and a check set read against a
// superseded head reports `green`/`ok`.

test('CLI classify — a misspelled --observed-sha is rejected rather than silently dropped', () => {
  const result = runCli([
    'classify',
    '--head-sha',
    'abc',
    '--observedSha',
    'def',
    '--rollup',
    JSON.stringify({ statusCheckRollup: [{ name: 'test-go', conclusion: 'SUCCESS' }] }),
  ])
  assert.notEqual(result.status, 0)
  assert.match(result.stderr, /--observedSha/)
  assert.match(result.stderr, /unrecognised flag/)
  assert.equal(result.stdout, '', 'no verdict is printed for a rejected invocation')
})

test('CLI — every verb rejects an unrecognised flag and names it', () => {
  for (const [verb, args] of [
    ['classify', ['--head-sha', 'abc']],
    ['merge-state', ['--merge-state', 'CLEAN']],
    ['liveness', ['--started-at', ZULU_TS]],
  ]) {
    const result = runCli([verb, ...args, '--not-a-flag', 'x'])
    assert.notEqual(result.status, 0, verb)
    assert.match(result.stderr, /--not-a-flag/, verb)
    assert.match(result.stderr, /pr-check-state: /, verb)
    assert.match(result.stderr, new RegExp(`${verb}\\(`), verb)
    assert.equal(result.stdout, '', verb)
  }
})

test('diffCheckSets — only a conclusion meaning DID NOT RUN is discounted from absent', () => {
  // `classifyKind` folds NEUTRAL, STALE and SKIPPED into one `skipped` kind, but a check only
  // reaches NEUTRAL or STALE by RUNNING. Discounting by kind hid a gate that ran and then vanished
  // — the genuine lost gate this comparison exists to find.
  const head = { statusCheckRollup: [{ name: 'a', conclusion: 'SUCCESS', status: 'COMPLETED' }] }
  const withPrior = (conclusion) =>
    classifyChecks({
      headSHA: 's',
      observedSHA: 's',
      rollup: head,
      priorContexts: {
        statusCheckRollup: [
          { name: 'a', conclusion: 'SUCCESS', status: 'COMPLETED' },
          { name: 'g', conclusion, status: 'COMPLETED' },
        ],
      },
    })
  assert.deepEqual(withPrior('SKIPPED').absent, [], 'a gate that did not run is discounted')
  assert.equal(withPrior('SKIPPED').state, 'green')
  for (const ran of ['NEUTRAL', 'STALE', 'SUCCESS', 'FAILURE']) {
    assert.deepEqual(
      withPrior(ran).absent,
      ['g'],
      `${ran} ran, so its disappearance is a lost gate`,
    )
    assert.equal(withPrior(ran).reason, CHECK_REASONS.ABSENT_GATE, ran)
  }
  // The bucket view has no conclusion; `skipping` is its own did-not-run spelling.
  assert.deepEqual(
    classifyChecks({
      headSHA: 's',
      observedSHA: 's',
      rollup: head,
      priorContexts: {
        checks: [
          { name: 'a', bucket: 'pass' },
          { name: 'g', bucket: 'skipping' },
        ],
      },
    }).absent,
    [],
  )
})

test('CLI — a recognised flag that lost its value is refused, not silently emptied', () => {
  // `parseFlags` renders a value-less flag as boolean `true`, which every read type-tests away to
  // an empty default — so a LOST VALUE was byte-indistinguishable from an omitted flag exactly as a
  // misspelt NAME was. On classify that is the same false GREEN: the stale-SHA arm needs both SHAs.
  const lost = runCli([
    'classify',
    '--observed-sha',
    '--head-sha',
    'deadbeef',
    '--rollup',
    JSON.stringify([{ name: 'x', conclusion: 'SUCCESS', status: 'COMPLETED' }]),
    '--prior',
    JSON.stringify(['x']),
  ])
  assert.notEqual(lost.status, 0)
  assert.match(lost.stderr, /--observed-sha needs a value/)
  assert.equal(lost.stdout, '', 'no verdict is printed for a rejected invocation')
  // The genuinely value-less flags are unaffected.
  for (const args of [
    ['classify', '--rollup', JSON.stringify([]), '--accept-no-gate-ran'],
    ['merge-state', '--merge-state', 'CLEAN', '--readied-this-run'],
  ]) {
    assert.equal(runCli(args).status, 0, args.join(' '))
  }
})

test('CLI — every flag each verb reads is accepted', () => {
  // The guard is only as good as its accepted set: a name left out of it turns a working shipped
  // invocation into a hard failure. Each flag is probed on top of a known-good baseline.
  const probes = [
    ['classify', ['--head-sha', 'abc']],
    ['classify', ['--observed-sha', 'abc']],
    ['classify', ['--rollup', JSON.stringify({ statusCheckRollup: [] })]],
    ['classify', ['--checks', JSON.stringify({ checks: [] })]],
    ['classify', ['--check-runs', JSON.stringify({ check_runs: [] })]],
    ['classify', ['--prior', JSON.stringify(['test-go'])]],
    ['classify', ['--read-error', 'quota exhausted']],
    ['classify', ['--accept-no-gate-ran']],
    ['classify', ['--structurally-absent', 'pr_agent']],
    ['merge-state', ['--merge-state', 'CLEAN']],
    ['merge-state', ['--check-state', 'green']],
    ['merge-state', ['--check-reason', 'ok']],
    ['merge-state', ['--unresolved-threads', '0']],
    ['merge-state', ['--readied-this-run']],
    ['liveness', ['--started-at', ZULU_TS]],
    ['liveness', ['--updated-at', ZULU_TS]],
    ['liveness', ['--now', ZULU_TS]],
    ['liveness', ['--stalled-after-ms', '900000']],
  ]
  for (const [verb, args] of probes) {
    const result = runCli([verb, ...args])
    assert.equal(result.status, 0, `${verb} ${args[0]}: ${result.stderr}`)
    assert.ok(JSON.parse(result.stdout), `${verb} ${args[0]}`)
  }
})

// ---------------------------------------------------------------------------
// Paginated and absent payloads. Both shapes are what the shipped skill recipes
// actually produce, and both used to be read as "nothing here" or not read at all.
// ---------------------------------------------------------------------------

test('classifyChecks — a `gh api --paginate --slurp` page array is flattened, not read as zero checks', () => {
  // `--slurp` yields an ARRAY OF PAGE OBJECTS. Read naively, every page maps to a nameless entry and
  // is filtered out, so a real check set silently reports total 0 — the too-small-to-prove-green
  // misreading this module exists to close.
  const slurped = [
    { total_count: 2, check_runs: [checkRun('test-go', 'completed', 'success')] },
    { total_count: 2, check_runs: [checkRun('web-e2e', 'completed', 'success')] },
  ]
  const verdict = classifyChecks({
    headSHA: 'abc123',
    observedSHA: 'abc123',
    checkRuns: slurped,
    priorContexts: ['test-go', 'web-e2e'],
  })
  assert.equal(
    verdict.total,
    2,
    'both pages must contribute; 0 means the page array was not flattened',
  )
  assert.equal(verdict.passed, 2)
  assert.equal(verdict.state, CHECK_STATES.GREEN)
  assert.equal(verdict.reason, CHECK_REASONS.OK)
  assert.equal(provesGreen(verdict), true, 'a flattened, prior-matched set proves green')

  // The single-page and bare-array shapes must keep working unchanged.
  assert.equal(
    classifyChecks({
      headSHA: 'abc123',
      observedSHA: 'abc123',
      checkRuns: { check_runs: [checkRun('test-go', 'completed', 'success')] },
    }).total,
    1,
  )
  assert.equal(
    classifyChecks({
      headSHA: 'abc123',
      observedSHA: 'abc123',
      checkRuns: [checkRun('test-go', 'completed', 'success')],
    }).total,
    1,
    'a bare array of check-run nodes must not be mistaken for a page array',
  )
})

test('CLI classify — a payload file that does not exist degrades to absent, and never aborts the verdict', () => {
  // The shipped recipes pass --prior unconditionally into a fresh mktemp dir. Throwing ENOENT there
  // exits 1 with no verdict at all, which is strictly worse than the documented fail-closed reading.
  const dir = mkdtempSync(path.join(tmpdir(), 'pr-check-state-'))
  const checksPath = path.join(dir, 'checks.json')
  writeFileSync(
    checksPath,
    JSON.stringify({ check_runs: [checkRun('test-go', 'completed', 'success')] }),
  )

  const result = runCli([
    'classify',
    '--head-sha',
    'abc',
    '--observed-sha',
    'abc',
    '--check-runs',
    checksPath,
    '--prior',
    path.join(dir, 'prior-contexts.json'),
  ])
  assert.equal(result.status, 0, `a missing optional payload must not abort: ${result.stderr}`)
  assert.equal(result.stdout.trimEnd().split('\n').length, 1, 'exactly one JSON line on stdout')
  const parsed = JSON.parse(result.stdout)
  assert.equal(parsed.priorKnown, false, 'an absent prior set is unknown, exactly as documented')
  assert.equal(parsed.provesGreen, false, 'and therefore never proves green')
  assert.match(result.stderr, /payload not found/, 'the miss must stay visible on stderr')

  // A missing REQUIRED payload is fail-closed too — unknown, never green.
  const noChecks = runCli(['classify', '--checks', path.join(dir, 'nope.json')])
  assert.equal(noChecks.status, 0)
  const noChecksParsed = JSON.parse(noChecks.stdout)
  assert.equal(noChecksParsed.state, CHECK_STATES.UNKNOWN)
  assert.equal(noChecksParsed.green, false)
})

// ---------------------------------------------------------------------------
// Shape guards (BOS-1244). Each of these was a call that returned a well-formed verdict the caller
// then acted on. The assertions match the STABLE FRAGMENT of each message — the function name and the
// expected shape — never the whole sentence, so rewording the diagnostic does not red the suite.
// ---------------------------------------------------------------------------

test('classifyChecks — a positional array raises instead of returning a no-checks verdict', () => {
  // The recorded misuse: `classifyChecks(buckets)` next to `contextNames(buckets)` in the same
  // expression. The sibling accepts the bare array, so the wrong call looks right, and the verdict
  // came back `state:unknown, reason:no-checks, total:0` for a PR whose checks were all green.
  const buckets = [
    { name: 'test-go', state: 'SUCCESS', bucket: 'pass' },
    { name: 'web-e2e', state: 'SUCCESS', bucket: 'pass' },
  ]
  assert.throws(
    () => classifyChecks(buckets),
    (err) => {
      assert.match(err.message, /classifyChecks\(/, 'the message must name the function')
      assert.match(err.message, /checkRuns/, 'and the expected options shape')
      assert.match(err.message, /an array of 2 item\(s\)/, 'and what was actually passed')
      return true
    },
  )
  // Proof the wrong call really was silent before: the RIGHT call on the same payload is green.
  const right = classifyChecks({ buckets, priorContexts: ['test-go', 'web-e2e'] })
  assert.equal(right.state, CHECK_STATES.GREEN)
  assert.equal(right.total, 2)
})

test('classifyChecks — a string, null and a keyless object all raise; no argument stays all-defaults', () => {
  for (const bad of ['abc123', null, 42, { totally: 'unrelated' }, {}]) {
    assert.throws(
      () => classifyChecks(bad),
      /classifyChecks\(/,
      `${JSON.stringify(bad)} must raise, not return a verdict`,
    )
  }
  // The documented no-argument call is untouched: every field is optional and the answer is unknown.
  const defaults = classifyChecks()
  assert.equal(defaults.state, CHECK_STATES.UNKNOWN)
  assert.equal(defaults.reason, CHECK_REASONS.NO_CHECKS)
  assert.equal(defaults.total, 0)
})

test('mergeStateVerdict — the raw gh mergeStateStatus field is an alias for mergeState', () => {
  // `gh pr view --json mergeable,mergeStateStatus` returns `mergeStateStatus`; the parameter was
  // `mergeState`, so handing the gh object straight in reported `{unknown, unreadable}` — a verdict.
  for (const state of ['CLEAN', 'DIRTY', 'UNSTABLE', 'BLOCKED', 'BEHIND']) {
    assert.deepEqual(
      mergeStateVerdict({ mergeStateStatus: state }),
      mergeStateVerdict({ mergeState: state }),
      `${state} must read identically under either spelling`,
    )
  }
  // And the whole gh object, extra fields and all, is accepted.
  const fromGh = mergeStateVerdict({ mergeable: 'MERGEABLE', mergeStateStatus: 'CLEAN' })
  assert.equal(fromGh.mergeState, 'CLEAN')
  assert.equal(fromGh.blocking, false)
})

test('mergeStateVerdict — an argument carrying NEITHER key raises', () => {
  // The guard must fire on shapes carrying neither key, not merely on a missing `mergeState` —
  // otherwise accepting the alias would have widened the silent-wrong-answer surface, not closed it.
  for (const bad of ['CLEAN', null, { mergeable: 'MERGEABLE' }, {}, ['CLEAN']]) {
    assert.throws(
      () => mergeStateVerdict(bad),
      (err) => {
        assert.match(err.message, /mergeStateVerdict\(/)
        assert.match(err.message, /mergeStateStatus/, 'the shape must name both accepted spellings')
        return true
      },
      `${JSON.stringify(bad)} must raise`,
    )
  }
  // `mergeStateVerdict()` with no argument keeps its documented unreadable answer.
  assert.equal(mergeStateVerdict().reason, CHECK_REASONS.UNREADABLE)
})

test('provesGreenReason — no-prior-sha is distinguished from an incomplete head set', () => {
  const buckets = [{ name: 'test-go', state: 'SUCCESS', bucket: 'pass' }]

  // Green, a gate ran, but no prior SHA was ever recorded: completeness was never established.
  const noPrior = classifyChecks({ buckets })
  assert.equal(noPrior.state, CHECK_STATES.GREEN)
  assert.equal(provesGreen(noPrior), false)
  assert.equal(provesGreenReason(noPrior), PROVES_GREEN_REASONS.NO_PRIOR_SHA)

  // A prior SHA IS known and carried a gate the head set does not: waiting never resolves this one.
  const absent = classifyChecks({ buckets, priorContexts: ['test-go', 'web-e2e'] })
  assert.equal(absent.reason, CHECK_REASONS.ABSENT_GATE)
  assert.equal(provesGreenReason(absent), PROVES_GREEN_REASONS.INCOMPLETE_HEAD_SET)
  assert.notEqual(
    provesGreenReason(absent),
    provesGreenReason(noPrior),
    'the two remedies differ, so the two reasons must too',
  )

  // The proving case, and the two remaining non-proving ones.
  const proves = classifyChecks({ buckets, priorContexts: ['test-go'] })
  assert.equal(provesGreen(proves), true)
  assert.equal(provesGreenReason(proves), PROVES_GREEN_REASONS.OK)

  const failing = classifyChecks({
    buckets: [{ name: 'test-go', state: 'FAILURE', bucket: 'fail' }],
    priorContexts: ['test-go'],
  })
  assert.equal(provesGreenReason(failing), PROVES_GREEN_REASONS.NOT_GREEN)

  const nothingRan = classifyChecks({
    buckets: [{ name: 'test-go', state: 'SKIPPED', bucket: 'skipping' }],
    priorContexts: ['test-go'],
    acceptNoGateRan: true,
  })
  assert.equal(nothingRan.state, CHECK_STATES.GREEN)
  assert.equal(provesGreenReason(nothingRan), PROVES_GREEN_REASONS.NO_GATE_RAN)
})

test('provesGreen — the boolean is unchanged for every shape the reason now discriminates', () => {
  // The reason is ADDITIVE. `provesGreen` must still be exactly green ∧ passed>0 ∧ priorKnown, so the
  // suite's existing green-gate assertions keep meaning what they meant.
  const cases = [
    classifyChecks({ buckets: [{ name: 'test-go', state: 'SUCCESS', bucket: 'pass' }] }),
    classifyChecks({
      buckets: [{ name: 'test-go', state: 'SUCCESS', bucket: 'pass' }],
      priorContexts: ['test-go'],
    }),
    classifyChecks({
      buckets: [{ name: 'test-go', state: 'SUCCESS', bucket: 'pass' }],
      priorContexts: ['test-go', 'web-e2e'],
    }),
    classifyChecks({ buckets: [{ name: 'test-go', state: 'FAILURE', bucket: 'fail' }] }),
  ]
  for (const verdict of cases) {
    assert.equal(
      provesGreen(verdict),
      isGreen(verdict) && verdict.passed > 0 && verdict.priorKnown === true,
    )
    assert.equal(
      provesGreen(verdict),
      provesGreenReason(verdict) === PROVES_GREEN_REASONS.OK,
      'the reason and the boolean must agree on the proving case',
    )
  }
})

test('CLI classify — inline JSON is parsed, and the named reason is printed beside provesGreen', () => {
  const inline = JSON.stringify([
    { name: 'test-go', state: 'SUCCESS', bucket: 'pass' },
    { name: 'web-e2e', state: 'SUCCESS', bucket: 'pass' },
  ])
  const result = runCli([
    'classify',
    '--head-sha',
    'abc',
    '--observed-sha',
    'abc',
    '--checks',
    inline,
  ])
  assert.equal(result.status, 0, result.stderr)
  const parsed = JSON.parse(result.stdout)
  assert.equal(parsed.total, 2, 'inline JSON must be read, not swallowed as a missing path')
  assert.equal(parsed.state, CHECK_STATES.GREEN)
  assert.notEqual(parsed.reason, CHECK_REASONS.NO_CHECKS)
  assert.equal(parsed.provesGreenReason, PROVES_GREEN_REASONS.NO_PRIOR_SHA)
  assert.equal(parsed.provesGreen, false, 'the boolean is unchanged by the new reason')

  // An inline payload far past the filesystem name limit takes the same arm — it used to throw a raw
  // ENAMETOOLONG straight past the ENOENT guard.
  const long = JSON.stringify(
    Array.from({ length: 400 }, (_, i) => ({
      name: `gate-${i}`,
      state: 'SUCCESS',
      bucket: 'pass',
    })),
  )
  assert.ok(long.length > 4096, 'the payload must exceed any plausible path length')
  const longResult = runCli(['classify', '--checks', long])
  assert.equal(longResult.status, 0, longResult.stderr)
  assert.equal(JSON.parse(longResult.stdout).total, 400)
})

test('CLI classify — an unreadable non-inline value raises naming the path-or-stdin contract', () => {
  const dir = mkdtempSync(path.join(tmpdir(), 'pr-check-state-'))
  // A DIRECTORY is a value that is not a usable path at all: it used to throw a raw EISDIR with no
  // mention of what the flag actually accepts.
  const result = runCli(['classify', '--checks', dir])
  assert.equal(result.status, 1, 'an unusable value must not reach a verdict')
  assert.equal(result.stdout, '', 'and must print no verdict line')
  assert.match(result.stderr, /--checks \(/, 'the message must name the flag')
  assert.match(result.stderr, /inline JSON/, 'and the path-or-`-`-or-inline contract')

  // Malformed inline JSON is named as such rather than reported as a missing file.
  const malformed = runCli(['classify', '--checks', '[{"name":'])
  assert.equal(malformed.status, 1)
  assert.match(malformed.stderr, /inline JSON payload is malformed/)

  // And the deliberately-narrow ENOENT degrade is untouched: the shipped recipes pass --prior
  // unconditionally into a fresh mktemp dir, so a path that is simply absent stays absent.
  const absent = runCli(['classify', '--prior', path.join(dir, 'prior-contexts.json')])
  assert.equal(absent.status, 0, absent.stderr)
  assert.equal(JSON.parse(absent.stdout).priorKnown, false)
})

// BOS-1244 review round 1 (boss-review-ce + boss-review-thermonuclear, converging on one region).
// The reason ladder is what the CLI prints beside `provesGreen` on every `classify` line, so a rung
// that describes the wrong fault is this ticket's own misleading-verdict class inside the helper
// added to end it. Two rungs were in the wrong order, and the existing agreement test could not see
// either: it asserts `provesGreen === (reason === OK)`, which is satisfied by EVERY wrong non-OK
// reason. These pin the reason itself.
test('provesGreenReason — a failing set reports its failure, not a missing prior SHA', () => {
  // The first-push shape: the shipped recipes pass `--prior` into a fresh mktemp dir, so
  // `priorKnown:false` accompanies most real red CI. Reporting `no-prior-sha` there named a remedy
  // ("record a prior SHA") that cannot fix a failing gate, while the failure went unreported.
  const failingNoPrior = classifyChecks({
    buckets: [{ name: 'test-go', state: 'FAILURE', bucket: 'fail' }],
  })
  assert.equal(failingNoPrior.state, CHECK_STATES.FAILING)
  assert.equal(failingNoPrior.priorKnown, false)
  assert.equal(provesGreenReason(failingNoPrior), PROVES_GREEN_REASONS.NOT_GREEN)
  assert.notEqual(
    provesGreenReason(failingNoPrior),
    PROVES_GREEN_REASONS.NO_PRIOR_SHA,
    'red CI must not be reported as an unrecorded prior SHA',
  )

  // And the converse ordering: a FAILING set can carry a non-empty `absent` too, so the
  // incomplete-head-set rung must key on the verdict's own `absent-gate` reason rather than on
  // `absent.length > 0` — otherwise a red gate is reported as a missing job to re-trigger.
  const failingWithAbsent = classifyChecks({
    buckets: [{ name: 'test-go', state: 'FAILURE', bucket: 'fail' }],
    priorContexts: ['test-go', 'web-e2e'],
  })
  assert.ok(failingWithAbsent.absent.length > 0, 'the fixture must actually carry an absent gate')
  assert.equal(provesGreenReason(failingWithAbsent), PROVES_GREEN_REASONS.NOT_GREEN)

  // The absent-gate arm itself is unchanged — that reason is still reachable and still distinct.
  const absentGate = classifyChecks({
    buckets: [{ name: 'test-go', state: 'SUCCESS', bucket: 'pass' }],
    priorContexts: ['test-go', 'web-e2e'],
  })
  assert.equal(absentGate.reason, CHECK_REASONS.ABSENT_GATE)
  assert.equal(provesGreenReason(absentGate), PROVES_GREEN_REASONS.INCOMPLETE_HEAD_SET)
})

test('provesGreenAgrees — the boolean and the reason agree on every verdict classifyChecks builds', () => {
  // `main()` prints both onto one JSON line, so a drift between them ships `provesGreen:true` beside
  // a named failure reason. Asserted over the verdicts this module can actually construct: a
  // hand-built object may pair `state:'green'` with `reason:'absent-gate'`, which no path here
  // produces and which the two answer differently by construction.
  const pass = { name: 'test-go', state: 'SUCCESS', bucket: 'pass' }
  const fail = { name: 'test-go', state: 'FAILURE', bucket: 'fail' }
  const pending = { name: 'test-go', state: 'PENDING', bucket: 'pending' }
  const skipped = { name: 'test-go', state: 'SKIPPED', bucket: 'skipping' }
  const verdicts = [
    classifyChecks(),
    classifyChecks({ buckets: [] }),
    classifyChecks({ buckets: [], priorContexts: [] }),
    classifyChecks({ buckets: [pass] }),
    classifyChecks({ buckets: [pass], priorContexts: ['test-go'] }),
    classifyChecks({ buckets: [pass], priorContexts: ['test-go', 'web-e2e'] }),
    classifyChecks({ buckets: [fail] }),
    classifyChecks({ buckets: [fail], priorContexts: ['test-go'] }),
    classifyChecks({ buckets: [fail], priorContexts: ['test-go', 'web-e2e'] }),
    classifyChecks({ buckets: [pending] }),
    classifyChecks({ buckets: [pending], priorContexts: ['test-go'] }),
    classifyChecks({ buckets: [skipped], priorContexts: ['test-go'] }),
    classifyChecks({ buckets: [skipped], priorContexts: ['test-go'], acceptNoGateRan: true }),
    classifyChecks({ buckets: [pass], priorContexts: ['test-go'], readError: 'boom' }),
  ]
  for (const verdict of verdicts) {
    assert.equal(
      provesGreenAgrees(verdict),
      true,
      `provesGreen ${provesGreen(verdict)} disagrees with reason ${provesGreenReason(verdict)} for ${JSON.stringify(verdict)}`,
    )
  }
  // Non-vacuity: the matrix must actually exercise the OK arm and at least one non-OK arm, or a
  // ladder that answered OK for everything would pass this test.
  const reasons = new Set(verdicts.map((v) => provesGreenReason(v)))
  assert.ok(reasons.has(PROVES_GREEN_REASONS.OK), 'the matrix must include a proving verdict')
  assert.ok(reasons.size > 1, 'and at least one non-proving one')
})
