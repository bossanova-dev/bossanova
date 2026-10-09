import test from 'node:test'
import { spawnSync } from 'node:child_process'
import assert from 'node:assert/strict'
import { mkdtempSync, writeFileSync, readFileSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'
import {
  computeEligibility,
  parseFollowUps,
  classifyLaunchOrigin,
  classifyWatchers,
  classifyVerifyConsent,
  settleCompletion,
  buildEnvelope,
  fastPath,
  reviewedTreeDrift,
  commandReader,
  main,
  INELIGIBLE_REASONS,
  VERIFY_GATE,
} from './completion-gate.mjs'
import { SHARED_INELIGIBLE_REASONS, sharedEligibility } from './merge-eligibility.mjs'

const HEAD = 'a'.repeat(40),
  MERGED = 'b'.repeat(40),
  OTHER = 'c'.repeat(40)
const body = `## Acceptance criteria
- [x] implementation demonstrated
## Review coverage
full
## Cross-model review
clean
## Human follow-up
- none
## Open questions
- none
`
const view = () => ({
  state: 'OPEN',
  isDraft: false,
  title: 'Feature',
  body,
  headRefOid: HEAD,
  mergeStateStatus: 'CLEAN',
  baseRefName: 'main',
  statusCheckRollup: [
    { __typename: 'CheckRun', name: 'unit', status: 'COMPLETED', conclusion: 'SUCCESS' },
  ],
})
function green() {
  return {
    runId: 'test-run',
    outcome: 'REVIEW_READY',
    sessionId: 'session',
    pr: 42,
    repo: 'owner/repo',
    reviewVerdict: 'clean',
    openFindings: 0,
    reviewCoverage: 'full',
    crossModelReview: 'clean',
    criteria: { met: 1, total: 1 },
    ciWaitState: 'settled',
    checkVerdict: { state: 'green', observedSHA: HEAD },
    prView: view(),
    pushedHead: HEAD,
    localHead: HEAD,
    upstreamHead: HEAD,
    treeDrift: 'none',
    humanFollowUp: { status: 'ok', open: 0 },
    openQuestions: { status: 'ok', open: 0 },
    launchOrigin: 'standalone',
    watchers: 'own-chat-only',
    archiveAfterMerge: false,
    verifyConsent: 'present',
    callbacksAvailable: true,
  }
}
test('all-green evidence is eligible and authorized', () => {
  assert.deepEqual(computeEligibility(green()), {
    mergeEligible: true,
    ineligibleReasons: [],
    mergeAuthorized: true,
    authorizationReasons: [],
  })
  assert.equal(Object.isFrozen(INELIGIBLE_REASONS), true)
})
const falseCases = [
  ['outcome', 'BLOCKED', 'outcome-not-review-ready'],
  ['sessionId', '', 'no-session'],
  ['pr', 0, 'no-pr'],
  ['repo', 'repo', 'repo-unknown'],
  ['reviewVerdict', 'capped', 'review-not-clean'],
  ['openFindings', 1, 'open-findings'],
  ['reviewCoverage', 'full (skipped: round)', 'coverage-not-full'],
  ['crossModelReview', 'error: absent', 'cross-model-error'],
  ['criteria', { met: 0, total: 1 }, 'criteria-unmet'],
  ['ciWaitState', 'timeout', 'ci-not-settled'],
  ['checkVerdict', { state: 'failing', observedSHA: HEAD }, 'ci-not-green-on-head'],
  ['treeDrift', 'changed', 'tree-changed-since-review'],
  ['humanFollowUp', { status: 'ok', open: 1 }, 'human-follow-up-open'],
  ['openQuestions', { status: 'ok', open: 1 }, 'open-questions-open'],
  ['launchOrigin', 'epic-child', 'epic-child'],
  ['watchers', 'foreign-pr-watcher', 'foreign-pr-watcher'],
]
for (const [field, value, reason] of falseCases) {
  test(`eligibility rejects ${field}: ${reason}`, () => {
    const result = computeEligibility({ ...green(), [field]: value })
    assert.equal(result.mergeEligible, false)
    assert.deepEqual(result.ineligibleReasons, [reason])
  })
}
const unknownCases = [
  ['outcome', 'outcome-not-review-ready'],
  ['sessionId', 'no-session'],
  ['pr', 'no-pr'],
  ['repo', 'repo-unknown'],
  ['reviewVerdict', 'review-verdict-unknown'],
  ['openFindings', 'open-findings'],
  ['reviewCoverage', 'coverage-unknown'],
  ['crossModelReview', 'cross-model-unknown'],
  ['criteria', 'criteria-unknown'],
  ['ciWaitState', 'ci-not-settled'],
  ['checkVerdict', 'ci-not-green-on-head'],
  ['treeDrift', 'tree-drift-unknown'],
  ['humanFollowUp', 'follow-up-section-missing'],
  ['openQuestions', 'open-questions-section-missing'],
  ['launchOrigin', 'launch-origin-unknown'],
  ['watchers', 'pr-watchers-unknown'],
]
for (const [field, reason] of unknownCases) {
  test(`eligibility fails closed when ${field} is unknown`, () => {
    const result = computeEligibility({ ...green(), [field]: undefined })
    assert.equal(result.mergeEligible, false)
    assert.deepEqual(result.ineligibleReasons, [reason])
  })
}
for (const [field, value, reason] of [
  ['pr', '42', 'no-pr'],
  ['reviewVerdict', {}, 'review-verdict-unknown'],
  ['reviewCoverage', {}, 'coverage-unknown'],
  ['crossModelReview', [], 'cross-model-unknown'],
  ['criteria', { total: '1', met: 1 }, 'criteria-unknown'],
  ['treeDrift', true, 'tree-drift-unknown'],
  ['launchOrigin', true, 'launch-origin-unknown'],
  ['watchers', true, 'pr-watchers-unknown'],
  ['humanFollowUp', { status: 'ok', open: '0' }, 'follow-up-section-missing'],
  ['openQuestions', { status: 'malformed', open: 0 }, 'open-questions-section-missing'],
]) {
  test(`eligibility rejects malformed ${field}`, () =>
    assert.deepEqual(computeEligibility({ ...green(), [field]: value }).ineligibleReasons, [
      reason,
    ]))
}
for (const [field, value, reason] of [
  ['state', 'CLOSED', 'pr-not-open'],
  ['isDraft', true, 'pr-draft'],
  ['title', 'Feature (partial scope)', 'do-not-merge-marker'],
  ['body', body + '\nDo Not Merge', 'do-not-merge-marker'],
  ['headRefOid', OTHER, 'head-mismatch'],
  ['headRefOid', undefined, 'head-unknown'],
  ['mergeStateStatus', 'BLOCKED', 'merge-state-blocked'],
  ['mergeStateStatus', 'BEHIND', 'merge-state-behind'],
  ['mergeStateStatus', undefined, 'merge-state-unknown'],
  ['mergeStateStatus', {}, 'merge-state-unknown'],
  ['state', undefined, 'pr-not-open'],
  ['isDraft', undefined, 'pr-draft'],
  ['title', undefined, 'do-not-merge-marker'],
  ['body', undefined, 'do-not-merge-marker'],
]) {
  test(`eligibility rejects live PR ${field}=${String(value)}`, () => {
    const fixture = green()
    fixture.prView[field] = value
    assert.deepEqual(computeEligibility(fixture).ineligibleReasons, [reason])
  })
}
for (const field of ['localHead', 'upstreamHead']) {
  test(`eligibility rejects missing ${field}`, () =>
    assert.deepEqual(computeEligibility({ ...green(), [field]: undefined }).ineligibleReasons, [
      'head-unknown',
    ]))
  test(`eligibility rejects changed ${field}`, () =>
    assert.deepEqual(computeEligibility({ ...green(), [field]: OTHER }).ineligibleReasons, [
      'head-mismatch',
    ]))
}
test('pushed head must be known and bind the green verdict', () => {
  assert.deepEqual(computeEligibility({ ...green(), pushedHead: undefined }).ineligibleReasons, [
    'ci-not-green-on-head',
    'head-unknown',
  ])
  assert.deepEqual(
    computeEligibility({ ...green(), checkVerdict: { state: 'green', observedSHA: OTHER } })
      .ineligibleReasons,
    ['ci-not-green-on-head'],
  )
  assert.deepEqual(
    computeEligibility({ ...green(), checkVerdict: { state: 'green' } }).ineligibleReasons,
    ['ci-not-green-on-head'],
  )
})
test('quick coverage is admitted only without reduced rounds', () => {
  assert.equal(
    computeEligibility({ ...green(), reviewCoverage: 'quick: no configured lens' }).mergeEligible,
    true,
  )
  assert.deepEqual(
    computeEligibility({ ...green(), reviewCoverage: 'quick: reduced (round)' }).ineligibleReasons,
    ['coverage-not-full'],
  )
})
for (const state of ['HAS_HOOKS', 'UNSTABLE'])
  test(`merge raw allow-list admits ${state}`, () => {
    const fixture = green()
    fixture.prView.mergeStateStatus = state
    assert.equal(computeEligibility(fixture).mergeEligible, true)
  })
for (const [verifyConsent, reason] of [
  ['absent', 'no-verify-cron'],
  ['unknown', 'verify-consent-unknown'],
  [undefined, 'verify-consent-unknown'],
  [true, 'verify-consent-unknown'],
]) {
  test(`verify consent ${String(verifyConsent)} withholds authorization as ${reason}`, () => {
    const result = computeEligibility({ ...green(), verifyConsent })
    assert.equal(result.mergeEligible, true)
    assert.equal(result.mergeAuthorized, false)
    assert.deepEqual(result.authorizationReasons, [reason])
  })
}
test('the retired completion consent reasons are gone from the vocabulary and every output', () => {
  for (const retired of ['not-opted-in', 'no-completion-extension']) {
    assert.equal(INELIGIBLE_REASONS.includes(retired), false, retired)
    for (const fixture of [{}, green(), { ...green(), verifyConsent: 'absent' }]) {
      const result = computeEligibility(fixture)
      assert.equal(result.ineligibleReasons.includes(retired), false, retired)
      assert.equal(result.authorizationReasons.includes(retired), false, retired)
    }
  }
  assert.ok(INELIGIBLE_REASONS.includes('no-verify-cron'))
  assert.ok(INELIGIBLE_REASONS.includes('verify-consent-unknown'))
})
test('missing evidence never authorizes a merge', () =>
  assert.equal(computeEligibility({}).mergeAuthorized, false))

test('follow-ups count open, completed and empty sections', () => {
  const result = parseFollowUps(
    '## Human follow-up\n- [ ] verify live\n- [x] done\n## Open questions\n- none',
  )
  assert.equal(result.ok, true)
  assert.deepEqual(result.humanFollowUp, { status: 'ok', open: 1, done: 1, total: 2 })
  assert.deepEqual(result.openQuestions, { status: 'ok', open: 0, done: 0, total: 0 })
})
test('follow-ups ignore fenced examples and match heading case', () => {
  const parsed = parseFollowUps(
    '```markdown\n## Human follow-up\n- [ ] example\n```\n## HUMAN FOLLOW-UP\n~~~\n- [ ] example\n~~~\n- [X] done\n## OPEN QUESTIONS\n- NONE',
  )
  assert.equal(parsed.ok, true)
  assert.equal(parsed.humanFollowUp.open, 0)
  assert.equal(parsed.humanFollowUp.done, 1)
})
for (const text of ['prose', '- [ ]', '- none\n- [ ] item', '', '### nested prose'])
  test(`follow-up malformed content: ${JSON.stringify(text)}`, () => {
    const parsed = parseFollowUps(`## Human follow-up\n${text}\n## Open questions\n- none`)
    assert.equal(parsed.humanFollowUp.status, 'malformed')
    assert.equal(parsed.ok, false)
  })
test('missing and duplicate follow-up headings fail closed', () => {
  assert.equal(parseFollowUps('').humanFollowUp.status, 'missing')
  assert.equal(
    parseFollowUps(
      '## Human follow-up\n- none\n## Human follow-up\n- [ ] open\n## Open questions\n- none',
    ).ok,
    false,
  )
})
for (const [mode, expected] of [
  ['cron', 'standalone'],
  ['managed', 'standalone'],
  ['unattended', 'epic-child'],
  ['standalone', 'launch-origin-unknown'],
  [undefined, 'launch-origin-unknown'],
  ['garbage', 'launch-origin-unknown'],
]) {
  test(`launch origin ${mode}`, () => assert.equal(classifyLaunchOrigin({ mode }), expected))
}
test('watchers distinguish own, foreign, failed and unavailable reads', () => {
  const options = { callbacksAvailable: true, targetChatId: 'chat', pr: 42 }
  assert.equal(
    classifyWatchers({
      ...options,
      watches: [{ pr_number: 42, state: 'active', target_chat_id: 'chat' }],
    }),
    'own-chat-only',
  )
  assert.equal(
    classifyWatchers({
      ...options,
      watches: [{ pr_number: 42, state: 'leased', target_chat_id: 'coordinator' }],
    }),
    'foreign-pr-watcher',
  )
  assert.equal(
    classifyWatchers({ ...options, watches: [], listingOk: false }),
    'pr-watchers-unknown',
  )
  assert.equal(classifyWatchers({ callbacksAvailable: false }), 'not-checked')
  assert.equal(classifyWatchers({ ...options, watches: [{}] }), 'pr-watchers-unknown')
  assert.equal(
    classifyWatchers({ ...options, watches: [{ pr_number: 42, state: 'active' }] }),
    'pr-watchers-unknown',
  )
  assert.equal(
    classifyWatchers({
      ...options,
      watches: [{ pr_number: 42, state: 'expired', target_chat_id: 'coordinator' }],
    }),
    'own-chat-only',
  )
})

// All subprocesses below are fake. Unexpected calls — including any `boss merge` — fail the test.
function harness(t, changes = {}) {
  const dir = mkdtempSync(join(tmpdir(), 'completion-gate-test-'))
  t.after(() => rmSync(dir, { recursive: true, force: true }))
  writeFileSync(join(dir, 'boss-build-review-verdict'), 'REVIEW_VERDICT=clean\n')
  writeFileSync(join(dir, 'boss-build-reviewed-head'), HEAD + '\n')
  const state = {
    calls: [],
    pr: view(),
    mode: 'managed',
    repoId: 'repo-1',
    cronJobs: [{ repo_id: 'repo-1', prompt: '/boss-verify', enabled: true }],
    cronError: false,
    archive: false,
    watches: [],
    ...changes,
  }
  const answer = (payload, status = 0) => ({ status, stdout: JSON.stringify(payload) })
  const spawn = (command, args, options) => {
    state.calls.push([command, args])
    if (command === 'gh' && args[0] === 'repo') return answer({ nameWithOwner: 'owner/repo' })
    if (command === 'gh' && args[0] === 'pr') return answer(state.pr)
    if (command === 'boss' && args[0] === 'env')
      return answer({
        mode: state.mode,
        session: { session_id: 'session', agent_session_id: 'chat', repo_id: state.repoId },
      })
    if (command === 'boss' && args[0] === 'cron')
      return state.cronError ? { status: 1, stdout: '' } : answer(state.cronJobs)
    if (command === 'boss' && args[0] === 'show')
      return answer({
        session: { id: 'session', repo_should_archive_sessions_after_merge: state.archive },
      })
    if (command === 'boss' && args[0] === 'callback') return answer(state.watches)
    if (command === 'git' && args[0] === 'rev-parse')
      return { status: 0, stdout: args[1] === '--absolute-git-dir' ? dir : HEAD }
    if (command === 'git' && args[0] === 'merge-base') return { status: 0, stdout: OTHER }
    if (command === 'git' && args[0] === 'diff')
      return { status: 0, stdout: 'diff --git a/x b/x\n+new\n' }
    if (command === 'git' && args[0] === 'patch-id') {
      assert.ok(options.input.includes('diff'))
      return { status: 0, stdout: `${HEAD} ${OTHER}\n` }
    }
    assert.fail(`unexpected command ${command} ${args.join(' ')}`)
  }
  const deps = { spawn, cwd: dir }
  const options = {
    runId: 'run',
    outcome: 'REVIEW_READY',
    pr: 42,
    ciWaitState: 'settled',
    checkVerdict: green().checkVerdict,
    callbacksAvailable: true,
  }
  return { dir, state, deps, options, envelope: () => buildEnvelope(options, deps) }
}
// BOS-1380: bossd defers an archive until every chat is idle, so the repo's
// archive-after-merge policy — on, off or unknown — is envelope context only
// and never withholds merge authorization.
for (const archive of [false, true, null])
  test(`boss show session wrapper archive policy ${archive} does not withhold authorization`, (t) => {
    const h = harness(t, { archive }),
      envelope = h.envelope()
    assert.equal(envelope.archiveAfterMerge, archive)
    assert.equal(envelope.mergeAuthorized, true)
  })
for (const value of [true, null, undefined, 'false'])
  test(`eligibility ignores archiveAfterMerge ${String(value)}`, () => {
    const result = computeEligibility({ ...green(), archiveAfterMerge: value })
    assert.equal(result.mergeEligible, true)
    assert.deepEqual(result.ineligibleReasons, [])
  })
test('archiveAfterMerge: true leaves mergeEligible true', () => {
  assert.equal(computeEligibility({ ...green(), archiveAfterMerge: true }).mergeEligible, true)
})
test('INELIGIBLE_REASONS no longer lists the archive-after-merge reasons', () => {
  assert.equal(INELIGIBLE_REASONS.includes('archive-after-merge'), false)
  assert.equal(INELIGIBLE_REASONS.includes('archive-after-merge-unknown'), false)
})
test('envelope reads live PR, env, cron consent, archive and patch-ids through spawn seam', (t) => {
  const h = harness(t),
    envelope = h.envelope()
  assert.equal(envelope.mergeAuthorized, true)
  assert.equal(envelope.sessionId, 'session')
  assert.equal(envelope.treeDrift, 'none')
  assert.equal(envelope.verifyConsent, 'present')
  assert.equal('mergeCommand' in envelope, false)
  assert.equal('attemptFile' in envelope, false)
  assert.deepEqual(
    h.state.calls.filter(([command, args]) => command === 'boss' && args[0] === 'cron'),
    [['boss', ['cron', 'ls', '--repo', 'repo-1', '--json']]],
  )
})
for (const [changes, consent, reason] of [
  [{ cronJobs: [] }, 'absent', 'no-verify-cron'],
  [{ cronError: true }, 'unknown', 'verify-consent-unknown'],
  [{ cronJobs: { jobs: [] } }, 'unknown', 'verify-consent-unknown'],
])
  test(`envelope maps cron consent ${consent} to ${reason}`, (t) => {
    const envelope = harness(t, changes).envelope()
    assert.equal(envelope.verifyConsent, consent)
    assert.equal(envelope.mergeEligible, true)
    assert.deepEqual(envelope.authorizationReasons, [reason])
  })
test('envelope without a session repo id never reads cron and is consent-unknown', (t) => {
  const h = harness(t, { repoId: '' }),
    envelope = h.envelope()
  assert.equal(envelope.verifyConsent, 'unknown')
  assert.deepEqual(envelope.authorizationReasons, ['verify-consent-unknown'])
  assert.equal(
    h.state.calls.some(([command, args]) => command === 'boss' && args[0] === 'cron'),
    false,
  )
})
test('live foreign watches make a standalone launch ineligible', (t) => {
  const h = harness(t, {
    watches: [{ pr_number: 42, state: 'active', target_chat_id: 'coordinator' }],
  })
  assert.deepEqual(h.envelope().ineligibleReasons, ['foreign-pr-watcher'])
})
test('patch-id distinguishes rebases from changed cumulative work and read failure', () => {
  let count = 0
  const read = (cmd, args) =>
    args[0] === 'merge-base'
      ? { ok: true, output: OTHER }
      : args[0] === 'diff'
        ? { ok: true, output: 'diff' }
        : { ok: true, output: `${++count === 1 ? HEAD : OTHER} ${HEAD}` }
  assert.equal(reviewedTreeDrift(read, 'main', HEAD, OTHER), 'changed')
  assert.equal(
    reviewedTreeDrift(() => ({ ok: false }), 'main', HEAD, OTHER),
    'unknown',
  )
  assert.equal(reviewedTreeDrift(read, 'main', undefined, HEAD), 'unknown')
})
test('spawn exceptions and malformed JSON are unreadable', () => {
  assert.equal(
    commandReader({
      spawn: () => {
        throw new Error('failed')
      },
    })('gh', []).ok,
    false,
  )
  assert.equal(
    commandReader({ spawn: () => ({ status: 0, stdout: 'garbage' }) })('gh', [], { json: true }).ok,
    false,
  )
})

for (const [name, opts, expected] of [
  [
    'merge named by verify-gate',
    {
      mergeResult: { merged: true, mergeSha: MERGED },
      livePr: { state: 'MERGED', mergeCommit: { oid: MERGED } },
    },
    ['merged', 'merged-by-verify'],
  ],
  [
    'merge observed but not named by verify-gate',
    { reason: 'human:no-receipt', livePr: { state: 'MERGED', mergeCommit: { oid: MERGED } } },
    ['merged', 'merged-outside-gate'],
  ],
  [
    'verify-gate named another SHA',
    {
      mergeResult: { merged: true, mergeSha: OTHER },
      livePr: { state: 'MERGED', mergeCommit: { oid: MERGED } },
    },
    ['merged', 'merged-outside-gate'],
  ],
  [
    'claimed merge did not land',
    { mergeResult: { merged: true, mergeSha: MERGED }, livePr: { state: 'OPEN' } },
    ['skipped', 'merge-not-observed'],
  ],
  [
    'reverify merge result',
    {
      mergeResult: { merged: false, verdict: 'reverify', reason: 'head-moved' },
      livePr: { state: 'OPEN' },
    },
    ['skipped', 'reverify'],
  ],
  [
    'lost claim at merge',
    { mergeResult: { merged: false, abandoned: 'claim-lost' }, livePr: { state: 'OPEN' } },
    ['skipped', 'claim-lost'],
  ],
  [
    'wait merge result with reasons',
    {
      mergeResult: { merged: false, verdict: 'wait', reasons: ['merge-state-blocked'] },
      livePr: { state: 'OPEN' },
    },
    ['skipped', 'merge-state-blocked'],
  ],
  [
    'no merge attempted',
    { reason: 'defect:findings', livePr: { state: 'OPEN' } },
    ['skipped', 'defect:findings'],
  ],
  [
    'unreadable live state',
    { mergeResult: { merged: true, mergeSha: MERGED }, livePr: null },
    ['skipped', 'merge-state-unreadable'],
  ],
  [
    'merged state missing SHA',
    { mergeResult: { merged: true, mergeSha: MERGED }, livePr: { state: 'MERGED' } },
    ['skipped', 'merge-state-unreadable'],
  ],
])
  test(`settle ${name}`, () => {
    const result = settleCompletion({ runId: 'run', ...opts })
    assert.equal(result.runId, 'run')
    assert.equal(result.action, expected[0])
    assert.equal(result.reason, expected[1])
    assert.equal(result.mergeSha, result.action === 'merged' ? MERGED : '')
  })
test('settle keeps a degraded tracker write beside an observed verify merge', () => {
  const result = settleCompletion({
    runId: 'run',
    mergeResult: { merged: true, mergeSha: MERGED, trackerWrites: 'unavailable' },
    livePr: { state: 'MERGED', mergeCommit: { oid: MERGED } },
  })
  assert.equal(result.reason, 'merged-by-verify')
  assert.equal(result.trackerWrites, 'unavailable')
})

test('classifyVerifyConsent requires an enabled /boss-verify job for this repo', () => {
  const job = (changes = {}) => ({
    repo_id: 'repo-1',
    prompt: '/boss-verify',
    enabled: true,
    ...changes,
  })
  for (const jobs of [
    [],
    [job({ enabled: false })],
    [job({ enabled: 'true' })],
    [job({ prompt: '/boss-verify-x' })],
    [job({ prompt: '/boss-verifyx' })],
    [job({ prompt: '/boss-build' })],
    [job({ prompt: 'run boss-verify' })],
    [job({ repo_id: 'repo-2' })],
  ])
    assert.equal(classifyVerifyConsent({ jobs, repoId: 'repo-1' }), 'absent', JSON.stringify(jobs))
  for (const prompt of [
    '/boss-verify',
    '/boss-verify --dry-run',
    'please /boss-verify\n',
    '$boss-verify',
  ])
    assert.equal(classifyVerifyConsent({ jobs: [job({ prompt })], repoId: 'repo-1' }), 'present')
  assert.equal(
    classifyVerifyConsent({
      jobs: [job({ enabled: false }), job({ repo_id: 'repo-2' }), job(), job()],
      repoId: 'repo-1',
    }),
    'present',
  )
  for (const [jobs, repoId] of [
    [[job()], ''],
    [[job()], undefined],
    [{ jobs: [job()] }, 'repo-1'],
    [null, 'repo-1'],
    [[job(), null], 'repo-1'],
    [[job(), ['nested']], 'repo-1'],
  ])
    assert.equal(
      classifyVerifyConsent({ jobs, repoId }),
      'unknown',
      JSON.stringify({ jobs, repoId }),
    )
  assert.equal(classifyVerifyConsent(), 'unknown')
})

// The fast path. Every verify-gate call is recorded as [verb, ...flags]; the only other subprocess
// it may spawn is the live `gh pr view` settle read. Anything else — including `boss merge` — fails.
function fastHarness(t, gate = {}) {
  const dir = mkdtempSync(join(tmpdir(), 'completion-fast-path-test-'))
  t.after(() => rmSync(dir, { recursive: true, force: true }))
  const state = { live: view(), liveReads: 0, calls: [] }
  const spawn = (command, args) => {
    if (command === process.execPath && args[0] === VERIFY_GATE) {
      const [, verb, ...rest] = args
      state.calls.push([verb, ...rest])
      const reply = typeof gate[verb] === 'function' ? gate[verb](rest, state) : gate[verb]
      if (reply === undefined) assert.fail(`unexpected verify-gate ${verb} ${rest.join(' ')}`)
      if (reply.raw) return reply.raw
      const { exit = 0, ...payload } = reply
      return { status: exit, stdout: `${JSON.stringify(payload)}\n` }
    }
    if (command === 'gh' && args[0] === 'pr' && args[1] === 'view') {
      state.liveReads++
      return { status: 0, stdout: JSON.stringify(state.live) }
    }
    assert.fail(`unexpected command ${command} ${args.join(' ')}`)
  }
  const out = join(dir, 'boss-build-completion.json')
  const run = (opts = {}) => fastPath({ envelope: green(), out, ...opts }, { spawn, cwd: dir })
  const record = () => {
    try {
      return JSON.parse(readFileSync(out, 'utf8'))
    } catch {
      return null
    }
  }
  return { dir, state, out, run, record }
}
const pr = ['--pr', '42']
const repoFlags = ['--repo', 'owner/repo']
const judge = (extra = []) => ['judge', ...pr, ...repoFlags, ...extra]
const postCall = (verdict, extra = []) => [
  'post',
  ...pr,
  '--head',
  HEAD,
  '--verdict',
  verdict,
  ...extra,
  ...repoFlags,
]
const mergeCall = (extra = []) => ['merge', ...pr, '--head', HEAD, ...extra, ...repoFlags]
const mergeReply = (_args, state) => {
  state.live = { ...state.live, state: 'MERGED', mergeCommit: { oid: MERGED } }
  return { merged: true, mergeSha: MERGED, writes: [] }
}
const passGate = {
  judge: { verdict: 'pass', reason: 'verified', headSha: HEAD },
  post: { verdict: 'pass', posted: true, writes: [] },
  merge: mergeReply,
}

test('fast path: green, consented, session-eligible pass judges, posts and merges via verify-gate', (t) => {
  const h = fastHarness(t, passGate)
  const result = h.run()
  assert.deepEqual(h.state.calls, [judge(), postCall('pass'), mergeCall()])
  assert.deepEqual(result, {
    runId: 'test-run',
    action: 'merged',
    reason: 'merged-by-verify',
    mergeSha: MERGED,
  })
  assert.deepEqual(h.record(), result)
})
test('fast path runs at most once per run id', (t) => {
  const h = fastHarness(t, passGate)
  const first = h.run()
  const calls = h.state.calls.length
  assert.deepEqual(h.run(), first)
  assert.equal(h.state.calls.length, calls)
})
for (const [label, changes, reason] of [
  ['CI not settled', { ciWaitState: 'pending' }, 'ci-not-settled'],
  ['epic-child launch', { launchOrigin: 'epic-child' }, 'epic-child'],
  ['review not clean', { reviewVerdict: 'capped' }, 'review-not-clean'],
  ['tree drift', { treeDrift: 'changed' }, 'tree-changed-since-review'],
  ['no verify cron', { verifyConsent: 'absent' }, 'no-verify-cron'],
  ['unknown consent', { verifyConsent: 'unknown' }, 'verify-consent-unknown'],
])
  test(`fast path: ${label} makes zero verify-gate calls`, (t) => {
    const h = fastHarness(t)
    const result = fastPath(
      { envelope: { ...green(), ...changes }, out: h.out },
      { spawn: () => assert.fail('spawned') },
    )
    assert.deepEqual(result, { runId: 'test-run', action: 'skipped', reason, mergeSha: '' })
    assert.deepEqual(h.record(), result)
  })
test('fast path re-derives authorization rather than trusting the envelope booleans', (t) => {
  const h = fastHarness(t)
  const envelope = {
    ...green(),
    ciWaitState: 'pending',
    mergeAuthorized: true,
    authorizationReasons: [],
  }
  assert.equal(
    fastPath({ envelope, out: h.out }, { spawn: () => assert.fail('spawned') }).reason,
    'ci-not-settled',
  )
})
test('fast path: extensions-required claims, returns the envelope, then judges with results', (t) => {
  const extensionEnvelope = { pr: { number: 42 }, headSha: HEAD, changedPaths: ['a.go'] }
  let phase = 1
  const h = fastHarness(t, {
    judge: () =>
      phase === 1
        ? {
            verdict: 'extensions-required',
            reason: 'extensions-installed',
            headSha: HEAD,
            extensionEnvelope,
          }
        : { verdict: 'pass', reason: 'verified', headSha: HEAD },
    post: (args) =>
      args.includes('claim') ? { won: true, token: 'tok' } : { verdict: 'pass', posted: true },
    merge: mergeReply,
  })
  const first = h.run()
  assert.deepEqual(first, {
    runId: 'test-run',
    action: 'extensions-required',
    token: 'tok',
    extensionEnvelope,
  })
  assert.equal(h.record(), null, 'a won claim writes no record')
  assert.deepEqual(h.state.calls, [judge(), postCall('claim')])
  phase = 2
  const results = join(h.dir, 'results.json')
  writeFileSync(results, '[]')
  const second = h.run({ extensionResults: results, token: 'tok' })
  assert.deepEqual(h.state.calls.slice(2), [
    judge(['--extension-results', results]),
    postCall('pass', ['--token', 'tok']),
    mergeCall(['--token', 'tok']),
  ])
  assert.equal(second.reason, 'merged-by-verify')
})
test('fast path: a lost claim records claim-lost and makes no further calls', (t) => {
  const h = fastHarness(t, {
    judge: {
      verdict: 'extensions-required',
      reason: 'extensions-installed',
      headSha: HEAD,
      extensionEnvelope: { headSha: HEAD },
    },
    post: { won: false, reason: 'claim-held' },
  })
  assert.equal(h.run().reason, 'claim-lost')
  assert.deepEqual(h.state.calls, [judge(), postCall('claim')])
  assert.equal(h.record().reason, 'claim-lost')
})
test('fast path: an invalid extension envelope never claims the head', (t) => {
  const h = fastHarness(t, {
    judge: { verdict: 'extensions-required', headSha: HEAD, extensionEnvelope: { invalid: ['x'] } },
  })
  assert.equal(h.run().reason, 'extension-envelope-invalid')
  assert.deepEqual(h.state.calls, [judge()])
})
test('fast path: human posts the verdict with its reason and never merges', (t) => {
  const h = fastHarness(t, {
    judge: { verdict: 'human', reason: 'no-receipt', headSha: HEAD },
    post: { verdict: 'human', reason: 'no-receipt', posted: true },
  })
  assert.deepEqual(h.run(), {
    runId: 'test-run',
    action: 'skipped',
    reason: 'human:no-receipt',
    mergeSha: '',
  })
  assert.deepEqual(h.state.calls, [judge(), postCall('human', ['--reason', 'no-receipt'])])
})
test('fast path: defect posts the verdict, reason and findings file and never merges', (t) => {
  const findings = [{ title: 'broken', detail: 'x' }]
  const h = fastHarness(t, {
    judge: { verdict: 'defect', reason: 'findings', headSha: HEAD, findings },
    post: { verdict: 'defect', posted: true },
  })
  const result = h.run()
  const file = join(h.dir, 'boss-build-verify-findings-test-run.json')
  assert.deepEqual(h.state.calls, [
    judge(),
    postCall('defect', ['--reason', 'findings', '--findings', file]),
  ])
  assert.deepEqual(JSON.parse(readFileSync(file, 'utf8')), findings)
  assert.equal(result.reason, 'defect:findings')
})
test('fast path: wait without a token posts nothing', (t) => {
  const h = fastHarness(t, { judge: { verdict: 'wait', reason: 'receipt-pending', headSha: HEAD } })
  assert.equal(h.run().reason, 'wait:receipt-pending')
  assert.deepEqual(h.state.calls, [judge()])
})
test('fast path: wait with a token clears the claim', (t) => {
  const h = fastHarness(t, {
    judge: { verdict: 'wait', reason: 'base-ci-red', headSha: HEAD },
    post: { verdict: 'wait', reason: 'base-ci-red', posted: true },
  })
  const results = join(h.dir, 'results.json')
  assert.equal(h.run({ extensionResults: results, token: 'tok' }).reason, 'wait:base-ci-red')
  assert.deepEqual(h.state.calls, [
    judge(['--extension-results', results]),
    postCall('wait', ['--reason', 'base-ci-red', '--token', 'tok']),
  ])
})
for (const verdict of ['pass', 'human'])
  test(`fast path: ${verdict} post with tracker writes unavailable stops without a merge`, (t) => {
    const h = fastHarness(t, {
      judge: { verdict, reason: verdict === 'pass' ? 'verified' : 'ledger-open', headSha: HEAD },
      post: { verdict, posted: false, trackerWrites: 'unavailable' },
    })
    assert.equal(h.run().reason, 'tracker-writes-unavailable')
    assert.equal(
      h.state.calls.some(([verb]) => verb === 'merge'),
      false,
    )
  })
test('fast path: a lost claim at post time stops without a merge', (t) => {
  const h = fastHarness(t, {
    judge: { verdict: 'pass', reason: 'verified', headSha: HEAD },
    post: { verdict: 'pass', abandoned: 'claim-lost' },
  })
  assert.equal(h.run().reason, 'claim-lost')
  assert.equal(h.state.calls.length, 2)
})
test('fast path: merge returning reverify records skipped reverify', (t) => {
  const h = fastHarness(t, {
    ...passGate,
    merge: { merged: false, verdict: 'reverify', reason: 'head-moved' },
  })
  assert.deepEqual(h.run(), {
    runId: 'test-run',
    action: 'skipped',
    reason: 'reverify',
    mergeSha: '',
  })
})
test('fast path: a non-zero verify-gate exit with JSON is still read for its verdict', (t) => {
  const h = fastHarness(t, {
    ...passGate,
    merge: { exit: 1, merged: false, verdict: 'wait', reason: 'not-verified' },
  })
  assert.equal(h.run().reason, 'not-verified')
})
for (const verb of ['judge', 'post', 'merge'])
  test(`fast path: verify-gate ${verb} exiting non-zero with no JSON is unreadable, never merged`, (t) => {
    const h = fastHarness(t, {
      ...passGate,
      [verb]: { raw: { status: 2, stdout: '', stderr: 'boom' } },
    })
    const result = h.run()
    assert.equal(result.action, 'skipped')
    assert.equal(result.reason, 'verify-gate-unreadable')
    assert.equal(h.state.calls.at(-1)[0], verb)
  })
test('fast path: a judged head other than the pushed head posts nothing', (t) => {
  const h = fastHarness(t, { judge: { verdict: 'pass', reason: 'verified', headSha: OTHER } })
  assert.equal(h.run().reason, 'head-mismatch')
  assert.deepEqual(h.state.calls, [judge()])
})
test('fast path: --dry-run reaches post and merge and writes no record', (t) => {
  const h = fastHarness(t, {
    judge: { verdict: 'pass', reason: 'verified', headSha: HEAD },
    post: { dryRun: true, verdict: 'pass', posted: true, writes: [] },
    merge: { dryRun: true, merged: false, planned: true, writes: [] },
  })
  const result = h.run({ dryRun: true })
  assert.deepEqual(h.state.calls, [
    judge(),
    [...postCall('pass'), '--dry-run'],
    [...mergeCall(), '--dry-run'],
  ])
  assert.equal(result.reason, 'dry-run')
  assert.equal(h.record(), null)
})
test('CLI: envelope persists a run-keyed record, fast-path parses bare --dry-run, followups validates', (t) => {
  const h = harness(t)
  const verdictFile = join(h.dir, 'checks.json'),
    envelopeFile = join(h.dir, 'envelope.json'),
    out = join(h.dir, 'completion.json'),
    bodyFile = join(h.dir, 'body.md')
  writeFileSync(verdictFile, JSON.stringify(green().checkVerdict))
  writeFileSync(bodyFile, body)
  assert.equal(main(['followups', '--body-file', bodyFile], h.deps).ok, true)
  const envelope = main(
    [
      'envelope',
      '--run-id',
      'run',
      '--outcome',
      'REVIEW_READY',
      '--pr',
      '42',
      '--ci-wait-state',
      'settled',
      '--check-verdict-file',
      verdictFile,
      '--callbacks-available',
      'true',
      '--out',
      envelopeFile,
    ],
    h.deps,
  )
  assert.equal(envelope.mergeAuthorized, true)
  assert.equal(JSON.parse(readFileSync(envelopeFile, 'utf8')).runId, 'run')
  h.state.cronJobs = []
  const absent = main(
    [
      'envelope',
      '--run-id',
      'run',
      '--outcome',
      'REVIEW_READY',
      '--pr',
      '42',
      '--ci-wait-state',
      'settled',
      '--check-verdict-file',
      verdictFile,
      '--callbacks-available',
      'true',
      '--out',
      envelopeFile,
    ],
    h.deps,
  )
  assert.deepEqual(absent.authorizationReasons, ['no-verify-cron'])
  const calls = h.state.calls.length
  const result = main(['fast-path', '--envelope', envelopeFile, '--out', out, '--dry-run'], h.deps)
  assert.deepEqual(result, {
    runId: 'run',
    action: 'skipped',
    reason: 'no-verify-cron',
    mergeSha: '',
  })
  assert.equal(h.state.calls.length, calls, 'no subprocess for an unconsented repo')
  assert.throws(
    () => main(['fast-path', '--envelope', envelopeFile, '--out', out, '--token', 't'], h.deps),
    /together/,
  )
  assert.throws(() => main(['fast-path', '--envelope', envelopeFile], h.deps), /--out/)
  assert.throws(() => main(['envelope', '--out'], h.deps), /--option value/)
})
for (const retired of ['merge', 'settle'])
  test(`the retired ${retired} verb is a usage error`, () =>
    assert.throws(
      () => main([retired, '--envelope', '/nonexistent', '--out', '/nonexistent']),
      /usage: completion-gate\.mjs followups\|envelope\|fast-path/,
    ))
test('VERIFY_GATE is the sibling verify-gate.mjs', () =>
  assert.equal(VERIFY_GATE, fileURLToPath(new URL('./verify-gate.mjs', import.meta.url))))
test('real cumulative patch-ids survive a rebase but detect a repair commit', (t) => {
  const dir = mkdtempSync(join(tmpdir(), 'completion-patch-id-test-'))
  t.after(() => rmSync(dir, { recursive: true, force: true }))
  const git = (args) => {
    const result = spawnSync(
      'git',
      [
        '-c',
        'user.name=Completion test',
        '-c',
        'user.email=test@example.invalid',
        '-c',
        'commit.gpgsign=false',
        ...args,
      ],
      { cwd: dir, encoding: 'utf8' },
    )
    assert.equal(result.status, 0, result.stderr)
    return result.stdout.trim()
  }
  git(['init', '--initial-branch=main'])
  writeFileSync(join(dir, 'base'), 'base\n')
  git(['add', 'base'])
  git(['commit', '-m', 'base'])
  git(['update-ref', 'refs/remotes/origin/main', 'HEAD'])
  git(['checkout', '-b', 'feature'])
  writeFileSync(join(dir, 'feature'), 'feature\n')
  git(['add', 'feature'])
  git(['commit', '-m', 'feature'])
  const reviewed = git(['rev-parse', 'HEAD'])
  git(['checkout', 'main'])
  writeFileSync(join(dir, 'unrelated'), 'base advanced\n')
  git(['add', 'unrelated'])
  git(['commit', '-m', 'advance base'])
  git(['update-ref', 'refs/remotes/origin/main', 'HEAD'])
  git(['checkout', 'feature'])
  git(['rebase', 'main'])
  const read = commandReader({ cwd: dir })
  assert.equal(reviewedTreeDrift(read, 'main', reviewed, git(['rev-parse', 'HEAD'])), 'none')
  writeFileSync(join(dir, 'feature'), 'feature repaired\n')
  git(['add', 'feature'])
  git(['commit', '-m', 'repair'])
  assert.equal(reviewedTreeDrift(read, 'main', reviewed, git(['rev-parse', 'HEAD'])), 'changed')
})

// Parity: the shared library yields exactly the shared subset of computeEligibility's reasons, in
// the same order, on every fixture this suite builds. The two anonymous row tables above are
// restated here because naming them would edit existing lines.
function sharedFactsOf(f) {
  return {
    criteria: f.criteria,
    ciWaitState: f.ciWaitState,
    checkVerdict: f.checkVerdict,
    headSha: f.pushedHead,
    prView: f.prView,
    heads: [f.pushedHead, f.localHead, f.upstreamHead, f.prView?.headRefOid],
    humanFollowUp: f.humanFollowUp,
    openQuestions: f.openQuestions,
  }
}
function parityFixtures() {
  const withPr = (changes) => {
    const fixture = green()
    Object.assign(fixture.prView, changes)
    return fixture
  }
  return [
    ['green', green()],
    ['empty', {}],
    ...falseCases.map(([field, value, reason]) => [
      `false ${field}:${reason}`,
      { ...green(), [field]: value },
    ]),
    ...unknownCases.map(([field]) => [`unknown ${field}`, { ...green(), [field]: undefined }]),
    ...[
      ['pr', '42'],
      ['reviewVerdict', {}],
      ['reviewCoverage', {}],
      ['crossModelReview', []],
      ['criteria', { total: '1', met: 1 }],
      ['treeDrift', true],
      ['launchOrigin', true],
      ['watchers', true],
      ['humanFollowUp', { status: 'ok', open: '0' }],
      ['openQuestions', { status: 'malformed', open: 0 }],
    ].map(([field, value]) => [`malformed ${field}`, { ...green(), [field]: value }]),
    ...[
      ['state', 'CLOSED'],
      ['isDraft', true],
      ['title', 'Feature (partial scope)'],
      ['body', body + '\nDo Not Merge'],
      ['headRefOid', OTHER],
      ['headRefOid', undefined],
      ['mergeStateStatus', 'BLOCKED'],
      ['mergeStateStatus', 'BEHIND'],
      ['mergeStateStatus', undefined],
      ['mergeStateStatus', {}],
      ['mergeStateStatus', 'HAS_HOOKS'],
      ['mergeStateStatus', 'UNSTABLE'],
      ['state', undefined],
      ['isDraft', undefined],
      ['title', undefined],
      ['body', undefined],
    ].map(([field, value]) => [`live PR ${field}=${String(value)}`, withPr({ [field]: value })]),
    ...['localHead', 'upstreamHead'].flatMap((field) => [
      [`missing ${field}`, { ...green(), [field]: undefined }],
      [`changed ${field}`, { ...green(), [field]: OTHER }],
    ]),
    ['no pushed head', { ...green(), pushedHead: undefined }],
    ['verdict on other head', { ...green(), checkVerdict: { state: 'green', observedSHA: OTHER } }],
    ['verdict without head', { ...green(), checkVerdict: { state: 'green' } }],
    ['quick coverage', { ...green(), reviewCoverage: 'quick: no configured lens' }],
    ['reduced coverage', { ...green(), reviewCoverage: 'quick: reduced (round)' }],
    [
      'several shared reasons at once',
      {
        ...withPr({ isDraft: true, mergeStateStatus: 'BEHIND', headRefOid: OTHER }),
        criteria: { met: 0, total: 2 },
        ciWaitState: 'timeout',
        treeDrift: 'changed',
        humanFollowUp: { status: 'ok', open: 1 },
        openQuestions: undefined,
      },
    ],
  ]
}
test('SHARED_INELIGIBLE_REASONS is one vocabulary with INELIGIBLE_REASONS', () => {
  assert.equal(Object.isFrozen(SHARED_INELIGIBLE_REASONS), true)
  for (const reason of SHARED_INELIGIBLE_REASONS)
    assert.ok(INELIGIBLE_REASONS.includes(reason), reason)
})
test('sharedEligibility matches the shared subset of computeEligibility on every fixture', () => {
  const fixtures = parityFixtures()
  assert.ok(fixtures.length > 60, `expected the full fixture set, got ${fixtures.length}`)
  let sawShared = 0
  for (const [label, fixture] of fixtures) {
    const expected = computeEligibility(fixture).ineligibleReasons.filter((reason) =>
      SHARED_INELIGIBLE_REASONS.includes(reason),
    )
    const shared = sharedEligibility(sharedFactsOf(fixture))
    assert.deepEqual(shared.reasons, expected, label)
    assert.equal(shared.eligible, expected.length === 0, label)
    if (expected.length > 1) sawShared++
  }
  assert.ok(sawShared >= 2, 'parity must cover multi-reason orderings')
})
