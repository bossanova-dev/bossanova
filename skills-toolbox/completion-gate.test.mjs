import test from 'node:test'
import { spawnSync } from 'node:child_process'
import assert from 'node:assert/strict'
import { mkdtempSync, writeFileSync, readFileSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import {
  computeEligibility,
  parseFollowUps,
  classifyLaunchOrigin,
  classifyWatchers,
  settleCompletion,
  buildEnvelope,
  mergeCompletion,
  reviewedTreeDrift,
  commandReader,
  main,
  INELIGIBLE_REASONS,
} from './completion-gate.mjs'

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
    config: { completionDefaults: { allowMerge: true } },
    extensionCount: 1,
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
  ['archiveAfterMerge', true, 'archive-after-merge'],
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
  ['archiveAfterMerge', 'archive-after-merge-unknown'],
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
  ['archiveAfterMerge', 'false', 'archive-after-merge-unknown'],
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
for (const config of [
  undefined,
  {},
  { completionDefaults: { allowMerge: 'true' } },
  { completionDefaults: { allowMerge: false } },
]) {
  test(`literal opt-in is required: ${JSON.stringify(config)}`, () => {
    const result = computeEligibility({ ...green(), config })
    assert.equal(result.mergeEligible, true)
    assert.equal(result.mergeAuthorized, false)
    assert.deepEqual(result.authorizationReasons, ['not-opted-in'])
  })
}
test('merge authorization requires an extension', () => {
  const result = computeEligibility({ ...green(), extensionCount: 0 })
  assert.equal(result.mergeEligible, true)
  assert.deepEqual(result.authorizationReasons, ['no-completion-extension'])
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

// All subprocesses below are fake, including boss merge. Unexpected calls fail the test.
function harness(t, changes = {}) {
  const dir = mkdtempSync(join(tmpdir(), 'completion-gate-test-'))
  t.after(() => rmSync(dir, { recursive: true, force: true }))
  writeFileSync(join(dir, 'boss-build-review-verdict'), 'REVIEW_VERDICT=clean\n')
  writeFileSync(join(dir, 'boss-build-reviewed-head'), HEAD + '\n')
  const state = {
    merged: false,
    mergeCount: 0,
    calls: [],
    config: { completionDefaults: { allowMerge: true } },
    pr: view(),
    mode: 'managed',
    archive: false,
    watches: [],
    ...changes,
  }
  const answer = (payload, status = 0) => ({ status, stdout: JSON.stringify(payload) })
  const spawn = (command, args, options) => {
    state.calls.push([command, args])
    if (command === 'gh' && args[0] === 'repo') return answer({ nameWithOwner: 'owner/repo' })
    if (command === 'gh' && args[0] === 'pr') {
      if (state.merged && state.shaReadErrors > 0) {
        state.shaReadErrors--
        return { status: 1, stdout: '' }
      }
      return answer(
        state.merged ? { ...state.pr, state: 'MERGED', mergeCommit: { oid: MERGED } } : state.pr,
      )
    }
    if (command === 'boss' && args[0] === 'env')
      return answer({
        mode: state.mode,
        session: { session_id: 'session', agent_session_id: 'chat' },
      })
    if (command === 'boss' && args[0] === 'show')
      return answer({
        session: { id: 'session', repo_should_archive_sessions_after_merge: state.archive },
      })
    if (command === 'boss' && args[0] === 'callback') return answer(state.watches)
    if (command === 'boss' && args[0] === 'merge') {
      state.mergeCount++
      assert.deepEqual(args, ['merge', 'session', '--yes', '--json'])
      if (state.mergeError) return answer({ error: { code: state.mergeError } }, 1)
      state.merged = true
      return answer({ session: { id: 'session' }, pr: { number: 42 } })
    }
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
  const deps = { spawn, cwd: dir, loadConfig: () => state.config }
  const options = {
    runId: 'run',
    outcome: 'REVIEW_READY',
    pr: 42,
    ciWaitState: 'settled',
    checkVerdict: green().checkVerdict,
    extensionCount: 1,
    callbacksAvailable: true,
    out: join(dir, 'envelope.json'),
  }
  return { dir, state, deps, options, envelope: () => buildEnvelope(options, deps) }
}
for (const [archive, reason] of [
  [false, null],
  [true, 'archive-after-merge'],
  [null, 'archive-after-merge-unknown'],
])
  test(`boss show session wrapper archive policy ${archive} controls merge execution`, (t) => {
    const h = harness(t, { archive }),
      envelope = h.envelope()
    assert.equal(envelope.archiveAfterMerge, archive)
    assert.equal(envelope.mergeAuthorized, reason === null)
    const result = mergeCompletion(envelope, h.deps)
    assert.equal(result.action, reason === null ? 'merged' : 'skipped')
    if (reason !== null) assert.equal(result.reason, reason)
    assert.equal(h.state.mergeCount, reason === null ? 1 : 0)
  })
test('envelope reads live PR, env, archive and patch-ids through spawn seam', (t) => {
  const h = harness(t),
    envelope = h.envelope()
  assert.equal(envelope.mergeAuthorized, true)
  assert.equal(envelope.sessionId, 'session')
  assert.equal(envelope.treeDrift, 'none')
  assert.deepEqual(envelope.mergeCommand.slice(2), [
    'merge',
    '--envelope',
    h.options.out,
    '--attempt-file',
    join(h.dir, 'boss-build-completion-attempt-run.json'),
  ])
})
for (const [changes, reason] of [
  [{ config: {} }, 'not-opted-in'],
  [{ config: { completionDefaults: { allowMerge: 'true' } } }, 'not-opted-in'],
  [{ mode: 'unattended' }, 'epic-child'],
  [{ archive: true }, 'archive-after-merge'],
  [{ archive: null }, 'archive-after-merge-unknown'],
])
  test(`merge refuses without subprocess when ${reason}`, (t) => {
    const h = harness(t),
      envelope = h.envelope()
    Object.assign(h.state, changes)
    const result = mergeCompletion(envelope, h.deps)
    assert.equal(result.reason, reason)
    assert.equal(h.state.mergeCount, 0)
  })
for (const [field, value, reason] of [
  ['headRefOid', OTHER, 'ci-not-green-on-head'],
  ['isDraft', true, 'pr-draft'],
  ['mergeStateStatus', 'BLOCKED', 'merge-state-blocked'],
])
  test(`merge re-reads changed live ${field}`, (t) => {
    const h = harness(t),
      envelope = h.envelope()
    h.state.pr[field] = value
    assert.equal(mergeCompletion(envelope, h.deps).reason, reason)
    assert.equal(h.state.mergeCount, 0)
  })
test('merge re-read vetoes a check that turned red', (t) => {
  const h = harness(t),
    envelope = h.envelope()
  h.state.pr.statusCheckRollup[0].conclusion = 'FAILURE'
  assert.equal(mergeCompletion(envelope, h.deps).reason, 'ci-not-green-on-head')
  assert.equal(h.state.mergeCount, 0)
})
test('merge executes once and returns the observed merge SHA', (t) => {
  const h = harness(t),
    envelope = h.envelope()
  const result = mergeCompletion(envelope, h.deps)
  assert.deepEqual(result, { action: 'merged', reason: 'merged-by-completion', mergeSha: MERGED })
  assert.equal(h.state.mergeCount, 1)
  assert.equal(JSON.parse(readFileSync(envelope.attemptFile, 'utf8')).runId, 'run')
  assert.equal(mergeCompletion(envelope, h.deps).reason, 'already-attempted')
  assert.equal(h.state.mergeCount, 1)
})
test('existing attempt file prevents every merge subprocess', (t) => {
  const h = harness(t),
    envelope = h.envelope()
  writeFileSync(envelope.attemptFile, '{}')
  assert.equal(mergeCompletion(envelope, h.deps).reason, 'already-attempted')
  assert.equal(h.state.mergeCount, 0)
})
test('daemon refusal retains its structured error code', (t) => {
  const h = harness(t, { mergeError: 'MERGE_GATE_REFUSED' })
  assert.deepEqual(mergeCompletion(h.envelope(), h.deps), {
    action: 'skipped',
    reason: 'merge-refused:MERGE_GATE_REFUSED',
    mergeSha: '',
  })
  assert.equal(h.state.mergeCount, 1)
})
test('merge SHA read failure requires a confirming reread', (t) => {
  const h = harness(t, { shaReadErrors: 1 })
  assert.equal(mergeCompletion(h.envelope(), h.deps).mergeSha, MERGED)
  assert.equal(h.state.mergeCount, 1)
})
test('unconfirmed merge stays skipped until live settle verifies it', (t) => {
  const h = harness(t, { shaReadErrors: 2 }),
    envelope = h.envelope()
  assert.equal(mergeCompletion(envelope, h.deps).action, 'skipped')
  const attempt = JSON.parse(readFileSync(envelope.attemptFile, 'utf8'))
  assert.equal(
    settleCompletion({
      runId: 'run',
      livePr: { state: 'MERGED', mergeCommit: { oid: MERGED } },
      attempt,
    }).action,
    'merged',
  )
  assert.equal(h.state.mergeCount, 1)
})
test('live foreign watches make a standalone launch ineligible', (t) => {
  const h = harness(t, {
    watches: [{ pr_number: 42, state: 'active', target_chat_id: 'coordinator' }],
  })
  assert.deepEqual(h.envelope().ineligibleReasons, ['foreign-pr-watcher'])
  assert.equal(mergeCompletion(h.envelope(), h.deps).reason, 'foreign-pr-watcher')
  assert.equal(h.state.mergeCount, 0)
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
    'verified gate merge',
    {
      results: [{ action: 'merged', reason: 'ok', mergeSha: MERGED }],
      attempt: { runId: 'run', action: 'merged', mergeSha: MERGED },
      livePr: { state: 'MERGED', mergeCommit: { oid: MERGED } },
    },
    ['merged', 'merged-by-completion'],
  ],
  [
    'claimed merge did not land',
    { results: [{ action: 'merged' }], livePr: { state: 'OPEN' } },
    ['skipped', 'claimed-merge-not-observed'],
  ],
  [
    'skipped extension but externally merged',
    { results: [{ action: 'skipped' }], livePr: { state: 'MERGED', mergeCommit: { oid: MERGED } } },
    ['merged', 'merged-outside-gate'],
  ],
  [
    'invalid extension result',
    { results: [{}], livePr: { state: 'OPEN' } },
    ['skipped', 'extension-result-invalid'],
  ],
  [
    'no extension',
    { results: [], extensionCount: 0, livePr: { state: 'OPEN' } },
    ['skipped', 'no-completion-extension'],
  ],
  [
    'unreadable live state',
    { results: [{ action: 'merged' }], livePr: null },
    ['skipped', 'merge-state-unreadable'],
  ],
  [
    'merged state missing SHA',
    { results: [{ action: 'merged' }], livePr: { state: 'MERGED' } },
    ['skipped', 'merge-state-unreadable'],
  ],
  [
    'foreign attempt record',
    {
      attempt: { runId: 'another-run', invoked: true },
      livePr: { state: 'MERGED', mergeCommit: { oid: MERGED } },
    },
    ['merged', 'merged-outside-gate'],
  ],
])
  test(`settle ${name}`, () => {
    const result = settleCompletion({ runId: 'run', ...opts })
    assert.equal(result.action, expected[0])
    assert.equal(result.reason, expected[1])
    assert.equal(result.mergeSha, result.action === 'merged' ? MERGED : '')
  })
test('CLI envelope and settle persist run-keyed records, followups validates body files', (t) => {
  const h = harness(t)
  const verdictFile = join(h.dir, 'checks.json'),
    resultsFile = join(h.dir, 'results.json'),
    out = join(h.dir, 'completion.json'),
    bodyFile = join(h.dir, 'body.md')
  writeFileSync(verdictFile, JSON.stringify(green().checkVerdict))
  writeFileSync(resultsFile, '[]')
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
      '--extension-count',
      '1',
      '--callbacks-available',
      'true',
      '--out',
      h.options.out,
    ],
    h.deps,
  )
  assert.equal(envelope.mergeAuthorized, true)
  assert.equal(JSON.parse(readFileSync(h.options.out, 'utf8')).runId, 'run')
  const result = main(
    ['settle', '--envelope', h.options.out, '--results-file', resultsFile, '--out', out],
    h.deps,
  )
  assert.equal(result.action, 'skipped')
  assert.deepEqual(JSON.parse(readFileSync(out, 'utf8')), result)
  h.state.merged = true
  assert.equal(
    main(
      ['settle', '--envelope', h.options.out, '--results-file', resultsFile, '--out', out],
      h.deps,
    ).action,
    'merged',
  )
})

test('ineligible snapshot cannot become mergeable through favorable live rereads', (t) => {
  const h = harness(t),
    envelope = h.envelope()
  envelope.humanFollowUp = { status: 'ok', open: 1 }
  assert.equal(mergeCompletion(envelope, h.deps).reason, 'human-follow-up-open')
  assert.equal(h.state.mergeCount, 0)
})

test('alternate attempt paths cannot bypass the same-run guard', (t) => {
  const h = harness(t),
    envelope = h.envelope()
  assert.equal(
    mergeCompletion(envelope, { ...h.deps, attemptFile: join(h.dir, 'first.json') }).action,
    'merged',
  )
  assert.equal(
    mergeCompletion(envelope, { ...h.deps, attemptFile: join(h.dir, 'second.json') }).reason,
    'already-attempted',
  )
  assert.equal(h.state.mergeCount, 1)
})
test('settle does not attribute an externally observed merge to a refused gate', () => {
  const result = settleCompletion({
    runId: 'run',
    livePr: { state: 'MERGED', mergeCommit: { oid: MERGED } },
    attempt: { runId: 'run', invoked: true, daemonAccepted: false, action: 'skipped' },
  })
  assert.equal(result.reason, 'merged-outside-gate')
})
test('envelope does not expose unrelated repo configuration', (t) => {
  const h = harness(t)
  h.state.config.secret = 'do-not-copy-to-envelope'
  assert.equal(JSON.stringify(h.envelope()).includes('do-not-copy-to-envelope'), false)
})

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
