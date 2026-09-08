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
  provesGreen,
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
