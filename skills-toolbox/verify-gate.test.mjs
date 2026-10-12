import { test } from 'node:test'
import assert from 'node:assert/strict'
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'

import {
  CLAIM_STALE_AFTER_MS,
  HUMAN_REASONS,
  HUMAN_REVIEW_CATEGORIES,
  HUMAN_REVIEW_REASON_PREFIX,
  REPAIR_EXHAUSTED_AFTER,
  UsageError,
  VERIFY_DESCRIPTIONS,
  aggregateExtensions,
  approval,
  commandReader,
  candidates,
  extensionsConsulted,
  claimState,
  classifyApproval,
  judge,
  main,
  merge,
  needsHumanDescription,
  parseHumanReasonList,
  parseLinearIssueLine,
  policySurfaceHits,
  post,
  prLevelDelta,
  readApprovalAnchor,
  resolveApprovalAnchor,
  rearm,
  rearmArgv,
  verifyMarker,
} from './verify-gate.mjs'
import { DEFAULT_CONFIG, mergeConfig, validateConfig } from './skill-config.mjs'
import { DESCRIPTION_LIMIT } from './commit-status.mjs'

const HEAD = 'a'.repeat(40)
const OTHER = 'd'.repeat(40)
const BASE = 'b'.repeat(40)
const MERGED_SHA = 'c'.repeat(40)
const NOW = Date.parse('2026-10-06T12:00:00Z')
const iso = (msAgo) => new Date(NOW - msAgo).toISOString()
const REPO = 'o/r'
const PR_URL = 'https://github.com/o/r/pull/7'

const BODY = [
  'Linear issue: https://linear.app/acme/issue/BOS-1/some-title',
  '',
  '## Acceptance criteria',
  '',
  '- [x] it works',
  '',
  '## Human follow-up',
  '',
  '- none',
  '',
  '## Open questions',
  '',
  '- none',
  '',
  '## Review coverage',
  '',
  'full (3 lenses)',
  '',
].join('\n')

const green = [{ name: 'ci', status: 'COMPLETED', conclusion: 'SUCCESS' }]

function prView(overrides = {}) {
  return {
    state: 'OPEN',
    number: 7,
    url: PR_URL,
    title: 'feat: a thing',
    body: BODY,
    isDraft: false,
    headRefOid: HEAD,
    baseRefName: 'main',
    mergeStateStatus: 'CLEAN',
    statusCheckRollup: green,
    reviewDecision: '',
    mergeCommit: null,
    ...overrides,
  }
}

const receipt = () => ({ context: 'boss/build', state: 'success', updated_at: iso(60_000) })

// The approval-anchor GraphQL payload: the PR's reviews plus its commit and force-push timeline.
const ANCHOR = 'e'.repeat(40)
const OLDER = 'f'.repeat(40)
const verifyCtx = (description, createdAt, state = 'SUCCESS') => ({ state, description, createdAt })
const commitNode = (oid, context = null) => ({
  commit: { oid, status: context ? { context } : null },
})
const forcePushNode = (oid, context = null) => ({
  beforeCommit: { oid, status: context ? { context } : null },
})
const reviewNode = (oid, state, submittedAt, authorAssociation = 'MEMBER') => ({
  state,
  submittedAt,
  authorAssociation,
  commit: { oid },
})
function anchorPayload({ reviews = [], timeline = [] } = {}) {
  return {
    data: {
      repository: {
        pullRequest: { reviews: { nodes: reviews }, timelineItems: { nodes: timeline } },
      },
    },
  }
}
// A head approved through `verified (approved)` at `sha`.
const approvedAt = (sha, msAgo = 3_600_000) =>
  anchorPayload({ timeline: [commitNode(sha, verifyCtx('verified (approved)', iso(msAgo)))] })

// A scripted world behind one fake reader. Every call is recorded; statuses posted through
// `gh api --method POST …/statuses/<sha>` are prepended to that SHA's list, so read-backs are real.
function world(opts = {}) {
  // `null` is a meaningful value here (an unreadable source), so only an ABSENT key takes the default.
  const pick = (key, fallback) => (key in opts ? opts[key] : fallback)
  const w = {
    pr: prView(opts.pr),
    prAfterMerge: opts.prAfterMerge ?? null,
    statuses: { [HEAD]: opts.statuses ?? [receipt()] },
    statusReadFails: opts.statusReadFails ?? false,
    base: pick('base', {
      runs: [
        {
          total_count: 1,
          check_runs: [{ name: 'ci', status: 'completed', conclusion: 'success' }],
        },
      ],
      statuses: [],
    }),
    diff: pick('diff', ['src/a.go']),
    // The approval-anchor GraphQL read: a readable payload with no approvals by default, `null`
    // for a failed read.
    graphql: pick('graphql', anchorPayload()),
    // Compare reads `<BASE>...<sha>`: `{[sha]: files[] | payload | null}`; a missing key fails.
    compare: opts.compare ?? {},
    // The config file at the base tip: absent (a 404) by default, `null` for a failed read, a
    // string for its raw contents.
    baseConfig: pick('baseConfig', undefined),
    // A raw read result for the base config, overriding `baseConfig` when set.
    baseConfigRead: opts.baseConfigRead,
    sessions: opts.sessions ?? [{ id: 'sess-1', pr_url: PR_URL }],
    show: pick('show', { repo_can_auto_repair: true, last_repair: null }),
    merge: opts.merge ?? { ok: true },
    race: opts.race ?? null,
    calls: [],
  }
  w.read = (command, args, { json } = {}) => {
    w.calls.push({ command, args: [...args] })
    const ok = (payload) => ({ ok: true, payload, output: JSON.stringify(payload), exit: 0 })
    const fail = (error = 'exit-1') => ({ ok: false, error, exit: 1, output: '' })
    if (command === 'gh' && args[0] === 'pr' && args[1] === 'view') {
      if (w.pr === null) return fail()
      const merged = w.calls.some((c) => c.command === 'boss' && c.args[0] === 'merge')
      return ok(merged && w.prAfterMerge ? w.prAfterMerge : w.pr)
    }
    if (command === 'gh' && args[0] === 'pr' && args[1] === 'diff') {
      if (w.diff === null) return fail()
      return { ok: true, output: w.diff.join('\n'), exit: 0 }
    }
    if (command === 'gh' && args[0] === 'repo') return ok({ nameWithOwner: REPO })
    if (command === 'gh' && args[0] === 'api') {
      if (args[1] === 'graphql') return w.graphql === null ? fail() : ok(w.graphql)
      const compareMatch = String(args[1]).match(
        /^repos\/o\/r\/compare\/([0-9a-f]{40})\.\.\.([0-9a-f]{40})$/,
      )
      if (compareMatch) {
        const served = compareMatch[1] === BASE ? w.compare[compareMatch[2]] : undefined
        if (served === undefined || served === null) return fail()
        return ok(Array.isArray(served) ? { files: served } : served)
      }
      if (args[1] === '--method' && args[2] === 'POST') {
        const path = args[3]
        const fields = Object.fromEntries(
          args
            .slice(4)
            .filter((_, i) => i % 2 === 1)
            .map((kv) => kv.match(/^([^=]*)=(.*)$/s).slice(1)),
        )
        const statusMatch = path.match(/\/statuses\/([0-9a-f]{40})$/)
        if (statusMatch) {
          const list = (w.statuses[statusMatch[1]] ??= [])
          list.unshift({
            context: fields.context,
            state: fields.state,
            description: fields.description ?? '',
            target_url: fields.target_url ?? '',
            updated_at: iso(0),
          })
          if (w.race)
            list.unshift({
              context: 'boss/verify',
              state: 'pending',
              description: `${VERIFY_DESCRIPTIONS.verifying} ${w.race}`,
              updated_at: new Date(NOW + 1000).toISOString(),
            })
          return ok({})
        }
        if (/\/issues\/\d+\/comments$/.test(path))
          return ok({ html_url: `${PR_URL}#issuecomment-99` })
        return fail()
      }
      const path = args[1]
      if (path === `repos/o/r/contents/.boss-skills.json?ref=${BASE}`) {
        if (w.baseConfigRead !== undefined) return w.baseConfigRead
        if (w.baseConfig === undefined)
          return {
            ok: false,
            error: 'exit-1',
            exit: 1,
            payload: { message: 'Not Found', status: '404' },
            output: '',
          }
        if (w.baseConfig === null) return fail()
        return ok({
          type: 'file',
          encoding: 'base64',
          content: Buffer.from(w.baseConfig).toString('base64'),
        })
      }
      const statusList = path.match(/commits\/([0-9a-f]{40})\/statuses/)
      if (statusList) {
        if (w.statusReadFails) return fail()
        return { ok: true, output: JSON.stringify([w.statuses[statusList[1]] ?? []]), exit: 0 }
      }
      if (path === 'repos/o/r/commits/main') {
        if (w.base === null) return fail()
        return ok({ sha: BASE })
      }
      if (path.includes(`commits/${BASE}/check-runs`)) return ok(w.base.runs)
      if (path.includes(`commits/${BASE}/status`)) return ok({ statuses: w.base.statuses })
      return fail()
    }
    if (command === 'boss' && args[0] === 'ls') return ok({ sessions: w.sessions })
    if (command === 'boss' && args[0] === 'show') {
      if (w.show === null) return fail()
      return ok({ session: w.show })
    }
    if (command === 'boss' && args[0] === 'merge')
      return w.merge.ok ? ok({ merged: true }) : { ...fail(w.merge.error), payload: {} }
    if (command === 'boss' && args[0] === 'callback') return ok({ id: 'cb-1' })
    if (command === 'boss' && args[0] === 'notes') return ok({ id: 'note-1' })
    return fail('unexpected')
  }
  w.mutating = () =>
    w.calls.filter(
      ({ command, args }) =>
        (command === 'gh' && args[0] === 'api' && args[1] === '--method') ||
        (command === 'gh' && args[0] === 'pr' && ['comment', 'merge'].includes(args[1])) ||
        (command === 'boss' && ['merge', 'callback', 'notes'].includes(args[0])),
    )
  w.statusPosts = () =>
    w
      .mutating()
      .filter((c) => c.command === 'gh' && /\/statuses\//.test(c.args[3]))
      .map((c) => {
        const get = (k) => c.args.find((a) => a.startsWith(`${k}=`))?.slice(k.length + 1)
        return { state: get('state'), description: get('description'), target: get('target_url') }
      })
  w.notes = () => w.calls.filter((c) => c.command === 'boss' && c.args[0] === 'notes')
  w.graphqlReads = () => w.calls.filter((c) => c.args[0] === 'api' && c.args[1] === 'graphql')
  w.compareReads = () =>
    w.calls.filter((c) => c.args[0] === 'api' && String(c.args[1]).includes('/compare/'))
  return w
}

function fakeAdapter({ labels = ['needs-human'], outcome = 'ok', missingKey = false } = {}) {
  const calls = []
  return {
    calls,
    writeCalls: () =>
      calls.filter((w) =>
        ['addLabels', 'removeLabels', 'comment', 'stateName'].some((k) => k in w),
      ),
    applyIssueWrites: async (writes) => {
      calls.push(writes)
      if (missingKey)
        throw Object.assign(new Error('LINEAR_API_KEY is not set'), {
          code: 'TRACKER_CREDENTIALS_MISSING',
        })
      const isWrite = ['addLabels', 'removeLabels', 'comment', 'stateName'].some((k) => k in writes)
      return {
        ok: !isWrite || outcome === 'ok',
        outcome: isWrite ? outcome : 'ok',
        applied: [],
        issue: { id: 'uuid', identifier: 'BOS-1', labels },
      }
    },
  }
}

function deps(w, extra = {}) {
  return {
    read: w.read,
    now: () => NOW,
    randomToken: () => 'feedf00d',
    discover: () => ({ extensions: [], skipped: [] }),
    loadConfig: () => ({}),
    adapter: fakeAdapter(),
    exists: () => false,
    ...extra,
  }
}

const tmp = () => mkdtempSync(join(tmpdir(), 'verify-gate-'))
function jsonFile(dir, name, value) {
  const path = join(dir, name)
  writeFileSync(path, JSON.stringify(value))
  return path
}

// ---------------------------------------------------------------------------
// Pure helpers.

test('VERIFY_DESCRIPTIONS is frozen and carries the BOS-1382 prefixes', () => {
  assert.ok(Object.isFrozen(VERIFY_DESCRIPTIONS))
  assert.equal(VERIFY_DESCRIPTIONS.verifying, 'verifying…')
  assert.equal(VERIFY_DESCRIPTIONS.waiting, 'waiting:')
  assert.equal(VERIFY_DESCRIPTIONS.needsHuman, 'needs human:')
  assert.equal(VERIFY_DESCRIPTIONS.verified, 'verified')
  assert.equal(VERIFY_DESCRIPTIONS.verifiedApproved, 'verified (approved)')
  assert.equal(VERIFY_DESCRIPTIONS.defect, 'defect:')
  assert.equal(REPAIR_EXHAUSTED_AFTER, 5)
  assert.deepEqual(HUMAN_REASONS, [
    'no-receipt',
    'repair-exhausted',
    'always-human-path',
    'ledger-open',
    'extension-failed',
    'no-session',
    'human-review:database',
    'human-review:security',
    'human-review:ui',
    'human-review:unproven',
  ])
  assert.equal(HUMAN_REVIEW_REASON_PREFIX, 'human-review:')
  assert.deepEqual(HUMAN_REVIEW_CATEGORIES, ['database', 'security', 'ui'])
})

test('parseLinearIssueLine reads only the first non-blank line', () => {
  assert.deepEqual(parseLinearIssueLine(BODY), {
    url: 'https://linear.app/acme/issue/BOS-1/some-title',
    id: 'BOS-1',
  })
  assert.deepEqual(parseLinearIssueLine('\n\nLinear issue: https://linear.app/a/issue/ab-12'), {
    url: 'https://linear.app/a/issue/ab-12',
    id: 'AB-12',
  })
  assert.equal(parseLinearIssueLine('Summary\nLinear issue: https://linear.app/a/issue/A-1'), null)
  assert.equal(parseLinearIssueLine('Linear issue: none'), null)
  assert.equal(parseLinearIssueLine(undefined), null)
})

test('claimState tells mine, a fresh foreign claim, a stale one, waiting and terminal apart', () => {
  const claim = (token, msAgo) => ({
    state: 'pending',
    description: `verifying… ${token}`,
    updatedAt: iso(msAgo),
  })
  assert.equal(claimState(null).kind, 'none')
  assert.equal(claimState(claim('t1', 1000), { token: 't1', now: NOW }).kind, 'mine')
  assert.equal(claimState(claim('t2', 1000), { token: 't1', now: NOW }).kind, 'foreign')
  assert.equal(
    claimState(claim('t2', CLAIM_STALE_AFTER_MS + 1), { token: 't1', now: NOW }).kind,
    'stale',
  )
  assert.equal(claimState({ ...claim('t2', 0), updatedAt: 'garbage' }, { now: NOW }).kind, 'stale')
  assert.equal(claimState({ state: 'pending', description: 'waiting: ci' }).kind, 'waiting')
  assert.equal(claimState({ state: 'success', description: 'verified' }).kind, 'terminal')
})

// ---------------------------------------------------------------------------
// judge.

test('judge passes a green, receipted, clean-ledger head and writes nothing', async () => {
  const w = world()
  const r = await judge({ pr: 7, repo: REPO }, deps(w))
  assert.equal(r.verdict, 'pass')
  assert.equal(r.reason, 'verified')
  assert.equal(r.headSha, HEAD)
  assert.equal(r.baseSha, BASE)
  assert.deepEqual(r.ticket, { url: 'https://linear.app/acme/issue/BOS-1/some-title', id: 'BOS-1' })
  assert.equal(w.mutating().length, 0)
  assert.equal(
    w.calls.filter((c) => c.args[0] === 'pr' && c.args[1] === 'diff').length,
    0,
    'the default empty alwaysHumanPaths reads no diff',
  )
})

test('judge excludes boss/* contexts from the head CI judgment', async () => {
  const w = world({
    pr: {
      statusCheckRollup: [
        ...green,
        { context: 'boss/build', state: 'FAILURE' },
        { context: 'boss/verify', state: 'PENDING' },
      ],
    },
  })
  const r = await judge({ pr: 7, repo: REPO }, deps(w))
  assert.equal(r.verdict, 'pass')
  assert.deepEqual(r.facts.ci.excluded, ['boss/build', 'boss/verify'])
})

test('judge waits, never defects, on unsettled or red head CI, a draft and a closed PR', async () => {
  for (const [pr, reason] of [
    [{ statusCheckRollup: [{ name: 'ci', status: 'IN_PROGRESS' }] }, 'ci-not-settled'],
    [
      { statusCheckRollup: [{ name: 'ci', status: 'COMPLETED', conclusion: 'FAILURE' }] },
      'ci-not-green-on-head',
    ],
    [{ isDraft: true }, 'pr-draft'],
    [{ state: 'CLOSED' }, 'pr-not-open'],
    [{ mergeStateStatus: 'DIRTY' }, 'merge-state-dirty'],
    [{ mergeStateStatus: 'UNKNOWN' }, 'merge-state-unknown'],
  ]) {
    const w = world({ pr })
    const r = await judge({ pr: 7, repo: REPO }, deps(w))
    assert.equal(r.verdict, 'wait', reason)
    assert.equal(r.reason, reason)
    assert.equal(w.mutating().length, 0)
  }
})

test('judge base-red guard: red waits, unreadable waits with a note, pending and green proceed', async () => {
  const red = world({
    base: {
      runs: [{ check_runs: [{ name: 'ci', status: 'completed', conclusion: 'failure' }] }],
      statuses: [{ context: 'boss/build', state: 'success' }],
    },
  })
  const r1 = await judge({ pr: 7, repo: REPO }, deps(red))
  assert.equal(r1.verdict, 'wait')
  assert.equal(r1.reason, 'base-ci-red')

  const unreadable = world({ base: null })
  const r2 = await judge({ pr: 7, repo: REPO }, deps(unreadable))
  assert.equal(r2.reason, 'base-ci-unknown')
  assert.deepEqual(
    r2.notes.map((n) => n.trigger),
    ['evidence-unknown'],
  )

  const pending = world({
    base: { runs: [{ check_runs: [{ name: 'ci', status: 'in_progress' }] }], statuses: [] },
  })
  assert.equal((await judge({ pr: 7, repo: REPO }, deps(pending))).verdict, 'pass')

  const bossOnlyRed = world({
    base: {
      runs: [{ check_runs: [{ name: 'ci', status: 'completed', conclusion: 'success' }] }],
      statuses: [{ context: 'boss/verify', state: 'failure' }],
    },
  })
  assert.equal(
    (await judge({ pr: 7, repo: REPO }, deps(bossOnlyRed))).verdict,
    'pass',
    'a red boss/* status on the base is not base CI',
  )
})

test('judge routes a missing receipt to a human and an unreadable one to a wait', async () => {
  const none = world({ statuses: [] })
  const r1 = await judge({ pr: 7, repo: REPO }, deps(none))
  assert.equal(r1.verdict, 'human')
  assert.equal(r1.reason, 'no-receipt')
  const failed = world({ statuses: [{ ...receipt(), state: 'failure' }] })
  assert.equal((await judge({ pr: 7, repo: REPO }, deps(failed))).reason, 'no-receipt')
  const unknown = world({ statusReadFails: true })
  const r3 = await judge({ pr: 7, repo: REPO }, deps(unknown))
  assert.equal(r3.verdict, 'wait')
  assert.equal(r3.reason, 'receipt-unknown')
})

test('judge routes an open ledger or a do-not-merge marker to a human ledger-open', async () => {
  for (const body of [
    BODY.replace('- [x] it works', '- [ ] it works'),
    BODY.replace('## Human follow-up\n\n- none', '## Human follow-up\n\n- [ ] check prod'),
    BODY.replace('## Open questions\n\n- none\n', ''),
    `${BODY}\nDO NOT MERGE\n`,
  ]) {
    const w = world({ pr: { body } })
    const r = await judge({ pr: 7, repo: REPO }, deps(w))
    assert.equal(r.verdict, 'human')
    assert.equal(r.reason, 'ledger-open')
  }
})

test('judge alwaysHumanPaths: an invalid config waits instead of dropping the policy', async () => {
  const w = world({ diff: ['migrations/001.sql'] })
  const r = await judge(
    { pr: 7, repo: REPO },
    deps(w, {
      loadConfig: () => {
        throw new Error(
          'verifyDefaults.alwaysHumanPaths must be an array of non-empty glob strings',
        )
      },
    }),
  )
  assert.equal(r.verdict, 'wait')
  assert.equal(r.reason, 'config-unreadable')
  assert.deepEqual(
    r.notes.map((n) => n.trigger),
    ['evidence-unknown'],
  )
})

test('judge alwaysHumanPaths: a hit is human, an unreadable diff waits with a note', async () => {
  const baseConfig = JSON.stringify({ verifyDefaults: { alwaysHumanPaths: ['migrations/**'] } })
  const hit = world({ diff: ['src/a.go', 'migrations/001.sql'], baseConfig })
  const r1 = await judge({ pr: 7, repo: REPO }, deps(hit))
  assert.equal(r1.verdict, 'human')
  assert.equal(r1.reason, 'always-human-path')
  assert.deepEqual(r1.facts.alwaysHumanHits, ['migrations/001.sql'])

  const miss = world({ diff: ['src/a.go'], baseConfig })
  assert.equal((await judge({ pr: 7, repo: REPO }, deps(miss))).verdict, 'pass')

  const unreadable = world({ diff: null, baseConfig })
  const r3 = await judge({ pr: 7, repo: REPO }, deps(unreadable))
  assert.equal(r3.verdict, 'wait')
  assert.equal(r3.reason, 'changed-paths-unknown')
  assert.deepEqual(
    r3.notes.map((n) => n.trigger),
    ['evidence-unknown'],
  )
})

const policyText = (verifyDefaults) => JSON.stringify({ verifyDefaults })
const diffReads = (w) => w.calls.filter((c) => c.args[0] === 'pr' && c.args[1] === 'diff').length

test('judge with no policy anywhere reads the base config once, reads no diff and passes', async () => {
  const w = world()
  const r = await judge({ pr: 7, repo: REPO }, deps(w))
  assert.equal(r.verdict, 'pass')
  assert.equal(diffReads(w), 0)
  assert.equal(
    w.calls.filter((c) => c.args[0] === 'api' && String(c.args[1]).includes('/contents/')).length,
    1,
  )
  assert.deepEqual(r.facts.humanReviewHits, {})
  assert.equal(r.facts.policySurfaceHit, false)
})

test('judge humanReview: a ui hit parks human-review:ui, a miss passes', async () => {
  const baseConfig = policyText({ humanReview: { ui: ['web/**/*.tsx'], database: ['db/**'] } })
  const hit = world({ diff: ['src/a.go', 'web/src/App.tsx'], baseConfig })
  const r1 = await judge({ pr: 7, repo: REPO }, deps(hit))
  assert.equal(r1.verdict, 'human')
  assert.equal(r1.reason, 'human-review:ui')
  assert.deepEqual(r1.facts.humanReviewHits, { ui: ['web/src/App.tsx'] })
  assert.equal(r1.facts.policySurfaceHit, false)

  const miss = world({ diff: ['src/a.go'], baseConfig })
  const r2 = await judge({ pr: 7, repo: REPO }, deps(miss))
  assert.equal(r2.verdict, 'pass')
  assert.equal(diffReads(miss), 1, 'a non-empty policy reads the diff')
})

test('judge reads the verify policy from the base tip, never the PR head', async () => {
  const baseConfig = policyText({ humanReview: { ui: ['web/**'] } })
  const narrowed = world({ diff: ['web/a.tsx'], baseConfig })
  const headNarrows = { verifyDefaults: { humanReview: { ui: [] } } }
  const r1 = await judge({ pr: 7, repo: REPO }, deps(narrowed, { loadConfig: () => headNarrows }))
  assert.equal(r1.verdict, 'human', "the head's narrowed ui list is ignored")
  assert.equal(r1.reason, 'human-review:ui')

  const widened = world({ diff: ['billing/pay.go'] })
  const headWidens = { verifyDefaults: { alwaysHumanPaths: ['billing/**'] } }
  const r2 = await judge({ pr: 7, repo: REPO }, deps(widened, { loadConfig: () => headWidens }))
  assert.equal(r2.verdict, 'pass', 'a glob only the head adds is ignored')
  assert.deepEqual(r2.facts.alwaysHumanHits, [])
})

test('judge base policy: absent is empty, unreadable or invalid waits config-unreadable', async () => {
  assert.equal(
    (await judge({ pr: 7, repo: REPO }, deps(world({ baseConfig: undefined })))).verdict,
    'pass',
  )
  for (const baseConfig of [
    null,
    '{not json',
    policyText({ humanReview: { uii: ['web/**'] } }),
    policyText({ alwaysHumanPaths: 'x' }),
  ]) {
    const r = await judge({ pr: 7, repo: REPO }, deps(world({ baseConfig })))
    assert.equal(r.verdict, 'wait', String(baseConfig))
    assert.equal(r.reason, 'config-unreadable', String(baseConfig))
    assert.deepEqual(
      r.notes.map((n) => n.trigger),
      ['evidence-unknown'],
    )
  }
  const otherBlocksBroken = JSON.stringify({ lensMap: 'nope', verifyDefaults: {} })
  assert.equal(
    (await judge({ pr: 7, repo: REPO }, deps(world({ baseConfig: otherBlocksBroken })))).verdict,
    'pass',
    'only the base verifyDefaults block is validated',
  )
})

test('judge base policy: a non-404 failure, a non-file payload or a bad humanReview block waits', async () => {
  const content = Buffer.from(policyText({ humanReview: { ui: ['web/**'] } })).toString('base64')
  const okRead = (payload) => ({ ok: true, payload, output: JSON.stringify(payload), exit: 0 })
  for (const [label, baseConfigRead] of [
    [
      'a 500',
      { ok: false, error: 'exit-1', exit: 1, payload: { message: 'Server Error', status: '500' } },
    ],
    ['a directory', okRead([{ type: 'file', name: '.boss-skills.json' }])],
    ['a symlink', okRead({ type: 'symlink', encoding: 'base64', content })],
    ['no encoding', okRead({ type: 'file', encoding: 'none', content })],
    ['no content', okRead({ type: 'file', encoding: 'base64' })],
  ]) {
    const r = await judge({ pr: 7, repo: REPO }, deps(world({ baseConfigRead })))
    assert.equal(r.verdict, 'wait', label)
    assert.equal(r.reason, 'config-unreadable', label)
  }
  for (const humanReview of [{ ui: 'web/**' }, { ui: [7] }, ['web/**'], 'ui']) {
    const r = await judge(
      { pr: 7, repo: REPO },
      deps(world({ baseConfig: policyText({ humanReview }) })),
    )
    assert.equal(r.verdict, 'wait', JSON.stringify(humanReview))
    assert.equal(r.reason, 'config-unreadable', JSON.stringify(humanReview))
  }
})

test('judge policy surface: a config or verify-extension edit hits every category only under a policy', async () => {
  const baseConfig = policyText({ humanReview: { ui: ['web/**'] } })
  const edit = world({ diff: ['.boss-skills.json'], baseConfig })
  const r1 = await judge({ pr: 7, repo: REPO }, deps(edit))
  assert.equal(r1.verdict, 'human')
  assert.equal(r1.facts.policySurfaceHit, true)
  assert.ok(r1.reason.startsWith('human-review:'))

  const unconfigured = world({ diff: ['.boss-skills.json'], baseConfig: policyText({}) })
  assert.equal((await judge({ pr: 7, repo: REPO }, deps(unconfigured))).verdict, 'pass')

  const deleted = world({ diff: ['.claude/skills/boss-verify-x/SKILL.md'], baseConfig })
  const r3 = await judge({ pr: 7, repo: REPO }, deps(deleted))
  assert.equal(r3.verdict, 'human')
  assert.equal(r3.facts.policySurfaceHit, true)
})

test('policySurfaceHits names the config file and paths under a boss-verify-* extension dir', () => {
  const roots = ['.claude/skills', './.codex/skills/']
  assert.deepEqual(
    policySurfaceHits(
      [
        '.boss-skills.json',
        'nested/.boss-skills.json',
        '.claude/skills/boss-verify-x/SKILL.md',
        '.codex/skills/boss-verify-proof/run.mjs',
        '.claude/skills/boss-review-x/SKILL.md',
        '.claude/skills/boss-verify.md',
        'src/boss-verify-x/a.go',
      ],
      { extensionRoots: roots },
    ),
    [
      '.boss-skills.json',
      '.claude/skills/boss-verify-x/SKILL.md',
      '.codex/skills/boss-verify-proof/run.mjs',
    ],
  )
  assert.deepEqual(policySurfaceHits(['.claude/skills/boss-verify-y/x']), [
    '.claude/skills/boss-verify-y/x',
  ])
})

// ---------------------------------------------------------------------------
// Set-valued parks and waivers.

test('parseHumanReasonList validates every code and normalises to HUMAN_REASONS order', () => {
  assert.deepEqual(parseHumanReasonList('human-review:ui,always-human-path'), [
    'always-human-path',
    'human-review:ui',
  ])
  assert.deepEqual(parseHumanReasonList(' no-receipt , no-receipt '), ['no-receipt'])
  assert.deepEqual(parseHumanReasonList(['human-review:ui', 'ledger-open']), [
    'ledger-open',
    'human-review:ui',
  ])
  for (const bad of ['', ',', 'human-review:ux', 'no-receipt,bogus', null, undefined, 3])
    assert.equal(parseHumanReasonList(bad), null, String(bad))
})

test('judge parks one status naming every hit category, in HUMAN_REASONS order', async () => {
  const baseConfig = policyText({ humanReview: { ui: ['web/**'], database: ['db/**'] } })
  const w = world({ diff: ['web/a.tsx', 'db/001.sql'], baseConfig })
  const r = await judge({ pr: 7, repo: REPO }, deps(w))
  assert.equal(r.verdict, 'human')
  assert.equal(r.reason, 'human-review:database,human-review:ui')
  assert.deepEqual(r.reasons, ['human-review:database', 'human-review:ui'])

  const surface = world({ diff: ['.boss-skills.json'], baseConfig })
  assert.equal(
    (await judge({ pr: 7, repo: REPO }, deps(surface))).reason,
    'human-review:database,human-review:security,human-review:ui',
  )
})

test('judge: one waiver of the parked set passes, including a mirrored always-human-path', async () => {
  const baseConfig = policyText({
    alwaysHumanPaths: ['web/**'],
    humanReview: { ui: ['web/**'] },
  })
  const w = world({ diff: ['web/a.tsx'], baseConfig })
  const parked = await judge({ pr: 7, repo: REPO }, deps(w))
  assert.equal(parked.reason, 'always-human-path,human-review:ui')

  const waived = await judge(
    { pr: 7, repo: REPO, waive: 'human-review:ui,always-human-path' },
    deps(world({ diff: ['web/a.tsx'], baseConfig })),
  )
  assert.equal(waived.verdict, 'pass')
  assert.equal(waived.reason, 'approved')

  const pw = world()
  const posted = await post(
    { pr: 7, head: HEAD, verdict: 'pass', reason: waived.reason, repo: REPO },
    deps(pw),
  )
  assert.equal(posted.posted, true)
  assert.deepEqual(pw.statusPosts(), [
    { state: 'success', description: 'verified (approved)', target: undefined },
  ])
})

test('judge: a code outside the waived set still parks', async () => {
  const baseConfig = policyText({ humanReview: { ui: ['web/**'], database: ['db/**'] } })
  const r = await judge(
    { pr: 7, repo: REPO, waive: 'human-review:database' },
    deps(world({ diff: ['web/a.tsx', 'db/001.sql'], baseConfig })),
  )
  assert.equal(r.verdict, 'human')
  assert.equal(r.reason, 'human-review:ui')
  assert.deepEqual(r.reasons, ['human-review:ui'])
})

test('judge: a state code and a path code park together and one waiver covers both', async () => {
  const baseConfig = policyText({ humanReview: { ui: ['web/**'] } })
  const parked = await judge(
    { pr: 7, repo: REPO },
    deps(world({ statuses: [], diff: ['web/a.tsx'], baseConfig })),
  )
  assert.equal(parked.reason, 'no-receipt,human-review:ui')
  const waived = await judge(
    { pr: 7, repo: REPO, waive: parked.reason },
    deps(world({ statuses: [], diff: ['web/a.tsx'], baseConfig })),
  )
  assert.equal(waived.verdict, 'pass')
  assert.equal(waived.reason, 'approved')
})

test('the worst-case judged set description fits the commit-status limit', () => {
  // A state code (no-receipt, ledger-open) parks before extensions run, so extension-failed never
  // joins one; the widest judged set is every state and path code, plus unproven on top of it.
  const worst = [
    'no-receipt',
    'always-human-path',
    'ledger-open',
    'human-review:database',
    'human-review:security',
    'human-review:ui',
    'human-review:unproven',
  ]
  const description = needsHumanDescription(parseHumanReasonList(worst).join(','))
  assert.equal(description.length, 135)
  assert.ok(description.length <= DESCRIPTION_LIMIT)
  // The widest set an extension run can park: a malfunction plus every path code and category.
  const withExtensions = HUMAN_REASONS.filter((c) => !['no-receipt', 'ledger-open'].includes(c))
    .filter((c) => !['repair-exhausted', 'no-session'].includes(c))
    .join(',')
  assert.ok(needsHumanDescription(withExtensions).length <= DESCRIPTION_LIMIT)
})

test('post refuses a human set whose description would exceed the commit-status limit', async () => {
  const w = world()
  await assert.rejects(
    post(
      { pr: 7, head: HEAD, verdict: 'human', reason: HUMAN_REASONS.join(','), repo: REPO },
      deps(w),
    ),
    UsageError,
  )
  assert.equal(w.calls.length, 0)
})

const EXT = { name: 'boss-verify-proof', order: 10 }
const envelope = (fields) => ({ ok: true, extension: EXT.name, role: 'verify', ...fields })
const passResult = envelope({
  verdict: 'pass',
  evidence: [{ kind: 'test', ref: 'go test' }],
  findings: [],
})
const failResult = envelope({ verdict: 'fail', evidence: [], findings: [{ title: 'nil deref' }] })

test('judge asks for extensions only when built-ins pass and extensions are installed', async () => {
  const w = world()
  const r = await judge({ pr: 7, repo: REPO }, deps(w, { discover: () => ({ extensions: [EXT] }) }))
  assert.equal(r.verdict, 'extensions-required')
  assert.deepEqual(r.extensionEnvelope.changedPaths, ['src/a.go'])
  assert.equal(r.extensionEnvelope.headSha, HEAD)
  assert.equal(r.extensionEnvelope.baseSha, BASE)
  assert.deepEqual(r.extensionEnvelope.ticket, {
    id: 'BOS-1',
    url: 'https://linear.app/acme/issue/BOS-1/some-title',
  })
  assert.equal(w.mutating().length, 0, 'judge never claims; the caller does')

  const waiting = world({ pr: { isDraft: true } })
  const r2 = await judge(
    { pr: 7, repo: REPO },
    deps(waiting, { discover: () => ({ extensions: [EXT] }) }),
  )
  assert.equal(r2.verdict, 'wait', 'a head that waits never reaches the extension step')
  assert.equal(r2.extensionEnvelope, undefined)
})

test('judge maps every extension malfunction to human extension-failed with a note', async () => {
  const dir = tmp()
  try {
    for (const [label, entry] of [
      ['invalid result', { extension: EXT.name, result: { ok: true, verdict: 'maybe' } }],
      ['crash', { extension: EXT.name, crashed: true }],
      [
        'pass with empty evidence',
        { extension: EXT.name, result: envelope({ verdict: 'pass', evidence: [], findings: [] }) },
      ],
      ['non-optional timeout', { extension: EXT.name, timedOut: true }],
      ['missing result', null],
    ]) {
      const file = jsonFile(dir, 'r.json', entry ? [entry] : [])
      const w = world()
      const r = await judge(
        { pr: 7, repo: REPO, extensionResults: file },
        deps(w, { discover: () => ({ extensions: [EXT] }) }),
      )
      assert.equal(r.verdict, 'human', label)
      assert.equal(r.reason, 'extension-failed', label)
      assert.deepEqual(
        r.notes.map((n) => n.trigger),
        ['extension-failed'],
        label,
      )
    }
    const garbage = join(dir, 'garbage.json')
    writeFileSync(garbage, 'not json')
    const r = await judge(
      { pr: 7, repo: REPO, extensionResults: garbage },
      deps(world(), { discover: () => ({ extensions: [EXT] }) }),
    )
    assert.equal(r.reason, 'extension-failed')
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

test('judge: optional timeout abstains, a valid fail is a defect, clean review records a note', async () => {
  const dir = tmp()
  try {
    const optional = { ...EXT, name: 'opt', optional: true }
    const discover = () => ({ extensions: [EXT, optional] })
    const ok = jsonFile(dir, 'ok.json', [
      { extension: EXT.name, result: passResult },
      { extension: 'opt', optional: true, timedOut: true },
    ])
    const r1 = await judge({ pr: 7, repo: REPO, extensionResults: ok }, deps(world(), { discover }))
    assert.equal(r1.verdict, 'pass')

    const bad = jsonFile(dir, 'bad.json', [
      { extension: EXT.name, result: failResult },
      {
        extension: 'opt',
        optional: true,
        result: envelope({ verdict: 'abstain', evidence: [], findings: [] }),
      },
    ])
    const r2 = await judge(
      { pr: 7, repo: REPO, extensionResults: bad },
      deps(world(), { discover }),
    )
    assert.equal(r2.verdict, 'defect')
    assert.deepEqual(r2.findings, [{ extension: EXT.name, title: 'nil deref' }])
    assert.deepEqual(
      r2.notes.map((n) => n.trigger),
      ['verify-failed-after-clean-review'],
    )

    const reviewed = world({ pr: { body: `${BODY}\n## Review findings\n\n- one\n` } })
    const r3 = await judge(
      { pr: 7, repo: REPO, extensionResults: bad },
      deps(reviewed, { discover }),
    )
    assert.equal(r3.verdict, 'defect')
    assert.deepEqual(r3.notes, [], 'a review that already had findings is not a clean review')
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

test('aggregateExtensions treats an unreadable results list as a malfunction', () => {
  assert.equal(aggregateExtensions(null).reason, 'extension-failed')
  assert.equal(aggregateExtensions([], []).verdict, 'pass')
})

// BOS-1484: an extension's human verdict, the malfunction → defect → human → pass precedence, and
// path-code parks that no longer skip extensions.
const OTHER_EXT = { name: 'boss-verify-judge', order: 20 }
const humanResult = (reason, name = OTHER_EXT.name) => ({
  ok: true,
  extension: name,
  role: 'verify',
  verdict: 'human',
  reason,
  evidence: [{ kind: 'video', ref: 'proof.mp4' }],
  findings: [],
})
const both = [EXT, OTHER_EXT]
const discoverBoth = () => ({ extensions: both })

test('aggregateExtensions: malfunction beats defect beats human beats pass', () => {
  const human = { extension: OTHER_EXT.name, result: humanResult('human-review:ui') }
  const pass = { extension: EXT.name, result: passResult }
  const fail = { extension: EXT.name, result: failResult }

  const h = aggregateExtensions([pass, human], both)
  assert.equal(h.verdict, 'human')
  assert.equal(h.reason, 'human-review:ui')
  assert.deepEqual(h.humanReasons, ['human-review:ui'])
  assert.deepEqual(h.failed, [])

  const d = aggregateExtensions([fail, human], both)
  assert.equal(d.verdict, 'defect')
  assert.deepEqual(d.humanReasons, ['human-review:ui'])

  const unknown = aggregateExtensions(
    [pass, { extension: OTHER_EXT.name, result: humanResult('human-review:legal') }],
    both,
  )
  assert.equal(unknown.verdict, 'human')
  assert.equal(unknown.reason, 'extension-failed')
  assert.deepEqual(unknown.failed, [OTHER_EXT.name])
  assert.deepEqual(unknown.humanReasons, [])

  const m = aggregateExtensions([fail, { extension: OTHER_EXT.name, crashed: true }], both)
  assert.equal(m.reason, 'extension-failed')

  const ordered = aggregateExtensions(
    [
      { extension: EXT.name, result: humanResult('human-review:ui', EXT.name) },
      { extension: OTHER_EXT.name, result: humanResult('human-review:database') },
    ],
    both,
  )
  assert.deepEqual(ordered.humanReasons, ['human-review:database', 'human-review:ui'])
  assert.equal(ordered.reason, 'human-review:database,human-review:ui')

  // An optional extension that invents a reason abstains, like every other optional malfunction.
  const optional = aggregateExtensions(
    [
      pass,
      {
        extension: OTHER_EXT.name,
        optional: true,
        result: humanResult('human-review:legal'),
      },
    ],
    both,
  )
  assert.equal(optional.verdict, 'pass')
})

// An extension's `wait` verdict: malfunction → defect → wait → human → pass.
const waitResult = (reason = 'stack-busy') => ({
  ok: true,
  extension: OTHER_EXT.name,
  role: 'verify',
  verdict: 'wait',
  reason,
  evidence: [],
  findings: [],
})

test('aggregateExtensions: a wait sits below malfunction and defect and above human and pass', () => {
  const wait = { extension: OTHER_EXT.name, result: waitResult() }
  const pass = { extension: EXT.name, result: passResult }
  const fail = { extension: EXT.name, result: failResult }

  const w = aggregateExtensions([pass, wait], both)
  assert.equal(w.verdict, 'wait')
  assert.equal(w.reason, 'stack-busy')
  assert.deepEqual(w.failed, [])
  assert.deepEqual(w.findings, [])

  assert.equal(aggregateExtensions([fail, wait], both).verdict, 'defect')
  const m = aggregateExtensions([wait, { extension: EXT.name, crashed: true }], both)
  assert.equal(m.verdict, 'human')
  assert.equal(m.reason, 'extension-failed')

  const human = { extension: EXT.name, result: humanResult('human-review:ui', EXT.name) }
  const wh = aggregateExtensions([human, wait], both)
  assert.equal(wh.verdict, 'wait')
  assert.equal(wh.reason, 'stack-busy')
  assert.deepEqual(wh.humanReasons, ['human-review:ui'], 'human reasons still reported')

  // Several waits: the first in result order names the reason.
  const first = aggregateExtensions(
    [{ extension: EXT.name, result: { ...waitResult('head-moved'), extension: EXT.name } }, wait],
    both,
  )
  assert.equal(first.reason, 'head-moved')

  // Without a wait, the same set aggregates exactly as before.
  assert.equal(aggregateExtensions([pass, human], both).verdict, 'human')
  assert.equal(aggregateExtensions([pass], [EXT]).verdict, 'pass')
})

test('judge: a re-judged extension wait waits, above a path-code park', async () => {
  const dir = tmp()
  try {
    const file = jsonFile(dir, 'r.json', [
      { extension: EXT.name, result: passResult },
      { extension: OTHER_EXT.name, result: waitResult() },
    ])
    const r = await judge(
      { pr: 7, repo: REPO, extensionResults: file },
      deps(world(), { discover: discoverBoth }),
    )
    assert.equal(r.verdict, 'wait')
    assert.equal(r.reason, 'stack-busy')
    assert.equal(r.extensions.outcomes.length, 2)

    const baseConfig = policyText({ humanReview: { ui: ['web/**'] } })
    const parked = await judge(
      { pr: 7, repo: REPO, extensionResults: file },
      deps(world({ diff: ['web/a.tsx'], baseConfig }), { discover: discoverBoth }),
    )
    assert.equal(parked.verdict, 'wait', 'evidence that could still find a defect runs first')
    assert.equal(parked.reason, 'stack-busy')

    // Under an extension-failed waiver the malfunction is ignored, and the wait still waits.
    const mixed = jsonFile(dir, 'm.json', [
      { extension: EXT.name, crashed: true },
      { extension: OTHER_EXT.name, result: waitResult() },
    ])
    const waived = await judge(
      { pr: 7, repo: REPO, extensionResults: mixed, waive: 'extension-failed' },
      deps(world(), { discover: discoverBoth }),
    )
    assert.equal(waived.verdict, 'wait')
    assert.equal(waived.reason, 'stack-busy')
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

test('post wait for an extension reason posts waiting only under the run own claim', async () => {
  const claimed = (token) =>
    world({
      statuses: [
        {
          context: 'boss/verify',
          state: 'pending',
          description: `verifying… ${token}`,
          updated_at: iso(1000),
        },
        receipt(),
      ],
    })
  const mine = claimed('feedf00d')
  const r = await post(
    { pr: 7, head: HEAD, verdict: 'wait', reason: 'stack-busy', token: 'feedf00d', repo: REPO },
    deps(mine),
  )
  assert.equal(r.posted, true)
  assert.deepEqual(mine.statusPosts(), [
    { state: 'pending', description: 'waiting: stack-busy', target: undefined },
  ])

  const foreign = claimed('0ther0ne')
  const r2 = await post(
    { pr: 7, head: HEAD, verdict: 'wait', reason: 'stack-busy', token: 'feedf00d', repo: REPO },
    deps(foreign),
  )
  assert.equal(r2.abandoned, 'claim-lost')
  assert.equal(foreign.mutating().length, 0)
})

// A verify extension's declared `malfunctionReason` joins the park when it malfunctions. It is read
// from the installed descriptor only, never from the agent-written results file.
const JUDGE_EXT = {
  name: 'boss-verify-judge',
  order: 20,
  malfunctionReason: 'human-review:security',
}

test('aggregateExtensions adds a malfunctioning extension declared malfunctionReason', () => {
  const installed = [EXT, JUDGE_EXT]
  const pass = { extension: EXT.name, result: passResult }
  for (const [label, entry] of [
    ['crash', { extension: JUDGE_EXT.name, crashed: true }],
    ['timeout', { extension: JUDGE_EXT.name, timedOut: true }],
    ['malformed', { extension: JUDGE_EXT.name, result: { ok: true, verdict: 'maybe' } }],
    ['reported failure', { extension: JUDGE_EXT.name, result: { ok: false } }],
    ['missing entry', null],
  ]) {
    const r = aggregateExtensions(entry ? [pass, entry] : [pass], installed)
    assert.equal(r.verdict, 'human', label)
    assert.equal(r.reason, 'extension-failed', label)
    assert.deepEqual(r.failed, [JUDGE_EXT.name], label)
    assert.deepEqual(r.humanReasons, ['human-review:security'], label)
  }
  // Unreadable results malfunction every installed extension, so the declared reason parks too.
  assert.deepEqual(aggregateExtensions(null, installed).humanReasons, ['human-review:security'])
  // An optional extension abstains on a malfunction, so its declared reason has no effect.
  const optional = { ...JUDGE_EXT, optional: true }
  for (const results of [
    [pass, { extension: JUDGE_EXT.name, optional: true, crashed: true }],
    [pass],
  ]) {
    const r = aggregateExtensions(results, [EXT, optional])
    assert.equal(r.verdict, 'pass')
    assert.deepEqual(r.humanReasons, [])
  }
  assert.deepEqual(aggregateExtensions(null, [EXT, optional]).humanReasons, [])
  // A result that ran cleanly never adds the malfunction reason.
  const ran = aggregateExtensions(
    [pass, { extension: JUDGE_EXT.name, result: passResult }],
    installed,
  )
  assert.equal(ran.verdict, 'pass')
  assert.deepEqual(ran.humanReasons, [])
})

test('aggregateExtensions reads malfunctionReason from the descriptor, never the results file', () => {
  const crashed = { extension: EXT.name, crashed: true, malfunctionReason: 'human-review:security' }
  const r = aggregateExtensions([crashed], [EXT])
  assert.equal(r.reason, 'extension-failed')
  assert.deepEqual(r.humanReasons, [])
  // A declared reason outside HUMAN_REASONS is dropped: the park is extension-failed alone.
  const unknown = { ...JUDGE_EXT, malfunctionReason: 'human-review:legal' }
  assert.ok(!HUMAN_REASONS.includes('human-review:legal'))
  const u = aggregateExtensions([{ extension: JUDGE_EXT.name, crashed: true }], [unknown])
  assert.equal(u.reason, 'extension-failed')
  assert.deepEqual(u.humanReasons, [])
  // Without the key, behaviour is unchanged.
  const plain = aggregateExtensions([{ extension: EXT.name, crashed: true }], [EXT])
  assert.deepEqual(plain.humanReasons, [])
})

test('judge: a malfunctioning judge with malfunctionReason parks it beside extension-failed', async () => {
  const dir = tmp()
  try {
    const discover = () => ({ extensions: [EXT, JUDGE_EXT] })
    const file = jsonFile(dir, 'r.json', [
      { extension: EXT.name, result: passResult },
      { extension: JUDGE_EXT.name, timedOut: true },
    ])
    const parked = await judge(
      { pr: 7, repo: REPO, extensionResults: file },
      deps(world(), { discover }),
    )
    assert.equal(parked.verdict, 'human')
    assert.equal(parked.reason, 'extension-failed,human-review:security')

    // A waiver of extension-failed alone still parks the declared security reason.
    const partial = await judge(
      { pr: 7, repo: REPO, extensionResults: file, waive: 'extension-failed' },
      deps(world(), { discover }),
    )
    assert.equal(partial.verdict, 'human')
    assert.equal(partial.reason, 'human-review:security')

    // A waiver of the parked set passes.
    const approved = await judge(
      { pr: 7, repo: REPO, extensionResults: file, waive: parked.reason },
      deps(world(), { discover }),
    )
    assert.equal(approved.verdict, 'pass')
    assert.equal(approved.reason, 'approved')
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

test('judge: an extension human verdict parks with its reason alongside a passing one', async () => {
  const dir = tmp()
  try {
    const file = jsonFile(dir, 'r.json', [
      { extension: EXT.name, result: passResult },
      { extension: OTHER_EXT.name, result: humanResult('human-review:security') },
    ])
    const r = await judge(
      { pr: 7, repo: REPO, extensionResults: file },
      deps(world(), { discover: discoverBoth }),
    )
    assert.equal(r.verdict, 'human')
    assert.equal(r.reason, 'human-review:security')
    assert.deepEqual(r.reasons, ['human-review:security'])
    assert.equal(r.extensions.outcomes.length, 2)
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

test('judge: a path-code park asks for extensions, then parks on pass and defects on fail', async () => {
  const baseConfig = policyText({ humanReview: { ui: ['web/**'] } })
  const discover = () => ({ extensions: [EXT] })
  const asked = await judge(
    { pr: 7, repo: REPO },
    deps(world({ diff: ['web/a.tsx'], baseConfig }), { discover }),
  )
  assert.equal(asked.verdict, 'extensions-required')
  assert.deepEqual(asked.extensionEnvelope.changedPaths, ['web/a.tsx'])

  const dir = tmp()
  try {
    const passed = await judge(
      {
        pr: 7,
        repo: REPO,
        extensionResults: jsonFile(dir, 'p.json', [{ extension: EXT.name, result: passResult }]),
      },
      deps(world({ diff: ['web/a.tsx'], baseConfig }), { discover }),
    )
    assert.equal(passed.verdict, 'human')
    assert.equal(passed.reason, 'human-review:ui')

    const failed = await judge(
      {
        pr: 7,
        repo: REPO,
        extensionResults: jsonFile(dir, 'f.json', [{ extension: EXT.name, result: failResult }]),
      },
      deps(world({ diff: ['web/a.tsx'], baseConfig }), { discover }),
    )
    assert.equal(failed.verdict, 'defect', 'findings go to repair before a person is paged')
    assert.deepEqual(failed.findings, [{ extension: EXT.name, title: 'nil deref' }])
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

test('judge: a waiver lifts an extension human reason only when it names that reason', async () => {
  const dir = tmp()
  try {
    const file = jsonFile(dir, 'r.json', [
      { extension: EXT.name, result: passResult },
      { extension: OTHER_EXT.name, result: humanResult('human-review:security') },
    ])
    const lifted = await judge(
      { pr: 7, repo: REPO, extensionResults: file, waive: 'human-review:security' },
      deps(world(), { discover: discoverBoth }),
    )
    assert.equal(lifted.verdict, 'pass')
    assert.equal(lifted.reason, 'approved')

    const kept = await judge(
      { pr: 7, repo: REPO, extensionResults: file, waive: 'human-review:ui' },
      deps(world(), { discover: discoverBoth }),
    )
    assert.equal(kept.verdict, 'human')
    assert.equal(kept.reason, 'human-review:security')
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

test('judge: a malfunction parks with the path codes and extension reasons in one status', async () => {
  const baseConfig = policyText({ humanReview: { ui: ['web/**'] } })
  const dir = tmp()
  try {
    const crashed = jsonFile(dir, 'c.json', [{ extension: EXT.name, crashed: true }])
    const r1 = await judge(
      { pr: 7, repo: REPO, extensionResults: crashed },
      deps(world({ diff: ['web/a.tsx'], baseConfig }), { discover: () => ({ extensions: [EXT] }) }),
    )
    assert.equal(r1.verdict, 'human')
    assert.equal(r1.reason, 'extension-failed,human-review:ui')
    assert.deepEqual(r1.reasons, ['extension-failed', 'human-review:ui'])
    assert.deepEqual(
      r1.notes.map((n) => n.trigger),
      ['extension-failed'],
    )

    const mixed = jsonFile(dir, 'm.json', [
      { extension: EXT.name, crashed: true },
      { extension: OTHER_EXT.name, result: humanResult('human-review:security') },
    ])
    const r2 = await judge(
      { pr: 7, repo: REPO, extensionResults: mixed },
      deps(world({ diff: ['web/a.tsx'], baseConfig }), { discover: discoverBoth }),
    )
    assert.equal(r2.reason, 'extension-failed,human-review:security,human-review:ui')

    // The approval of that park waives the whole set and passes.
    const approved = await judge(
      { pr: 7, repo: REPO, extensionResults: mixed, waive: r2.reason },
      deps(world({ diff: ['web/a.tsx'], baseConfig }), { discover: discoverBoth }),
    )
    assert.equal(approved.verdict, 'pass')
    assert.equal(approved.reason, 'approved')
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

test('judge: an extension-failed waiver ignores malfunctions but keeps what ran', async () => {
  const dir = tmp()
  try {
    const human = jsonFile(dir, 'h.json', [
      { extension: EXT.name, crashed: true },
      { extension: OTHER_EXT.name, result: humanResult('human-review:security') },
    ])
    const r1 = await judge(
      { pr: 7, repo: REPO, extensionResults: human, waive: 'extension-failed' },
      deps(world(), { discover: discoverBoth }),
    )
    assert.equal(r1.verdict, 'human')
    assert.equal(r1.reason, 'human-review:security')
    assert.deepEqual(r1.notes, [])

    const fail = jsonFile(dir, 'f.json', [
      { extension: EXT.name, result: failResult },
      { extension: OTHER_EXT.name, crashed: true },
    ])
    const r2 = await judge(
      { pr: 7, repo: REPO, extensionResults: fail, waive: 'extension-failed' },
      deps(world(), { discover: discoverBoth }),
    )
    assert.equal(r2.verdict, 'defect')
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

test('judge: no-receipt still parks with the path codes and never asks for extensions', async () => {
  const baseConfig = policyText({ humanReview: { ui: ['web/**'] } })
  const r = await judge(
    { pr: 7, repo: REPO },
    deps(world({ statuses: [], diff: ['web/a.tsx'], baseConfig }), {
      discover: () => ({ extensions: [EXT] }),
    }),
  )
  assert.equal(r.verdict, 'human')
  assert.equal(r.reason, 'no-receipt,human-review:ui')
  assert.equal(r.extensionEnvelope, undefined)
})

// BOS-1484: an extension's zero-token applicability script (`applies`). Exit 10 means does not
// apply; every other outcome applies.
const GATED = { name: 'boss-verify-gated', order: 30, applies: 'scripts/verify-applies.mjs' }
const exited = (exit) =>
  exit === 0
    ? { ok: true, exit: 0, output: '' }
    : { ok: false, error: exit === null ? 'ETIMEDOUT' : `exit-${exit}`, exit, output: '' }
// Routes applicability-script runs (the node binary with a script argument) to `scripts`, a map of
// script path → reader result or function of the stdin input; every other read goes to the world.
function withApplies(w, scripts) {
  const runs = []
  const read = (command, args, opts = {}) => {
    if (command === process.execPath && args.length === 1 && args[0] in scripts) {
      runs.push({ script: args[0], input: opts.input })
      const outcome = scripts[args[0]]
      return typeof outcome === 'function' ? outcome(opts.input) : outcome
    }
    return w.read(command, args, opts)
  }
  return { read, runs }
}

test('judge: an extension whose check exits 10 is not dispatched and a green head passes', async () => {
  const w = world()
  const gated = withApplies(w, { [GATED.applies]: exited(10) })
  const r = await judge(
    { pr: 7, repo: REPO },
    deps(w, { read: gated.read, discover: () => ({ extensions: [GATED] }) }),
  )
  assert.equal(r.verdict, 'pass')
  assert.equal(r.extensionEnvelope, undefined)
  assert.deepEqual(r.facts.extensionsInstalled, [GATED.name])
  assert.deepEqual(r.facts.extensionsApplicable, [])
  assert.deepEqual(r.facts.extensionsNotApplicable, [GATED.name])
  assert.equal(gated.runs.length, 1)
  assert.equal(w.mutating().length, 0)
})

test('judge: every check outcome but exit 10 dispatches, and no applies always dispatches', async () => {
  for (const [label, outcome] of [
    ['exit 0', exited(0)],
    ['exit 1 (an uncaught throw)', exited(1)],
    ['exit 2', exited(2)],
    ['a timeout', exited(null)],
  ]) {
    const w = world()
    const gated = withApplies(w, { [GATED.applies]: outcome })
    const r = await judge(
      { pr: 7, repo: REPO },
      deps(w, { read: gated.read, discover: () => ({ extensions: [GATED, EXT] }) }),
    )
    assert.equal(r.verdict, 'extensions-required', label)
    assert.deepEqual(r.facts.extensionsApplicable, [GATED.name, EXT.name], label)
    assert.deepEqual(r.facts.extensionsNotApplicable, [], label)
    assert.equal(gated.runs.length, 1, `${label}: only the declared check runs`)
  }
})

test('judge runs real applicability scripts from the repo root with the envelope on stdin', async () => {
  const dir = tmp()
  try {
    mkdirSync(join(dir, 'scripts'))
    const script = (name, body) => writeFileSync(join(dir, 'scripts', name), body)
    // Applies only when the PR touches web/: reads the envelope's changedPaths from stdin.
    script(
      'web-only.mjs',
      [
        "import { readFileSync } from 'node:fs'",
        "const envelope = JSON.parse(readFileSync(0, 'utf8'))",
        "process.exit(envelope.changedPaths.some((p) => p.startsWith('web/')) ? 0 : 10)",
      ].join('\n'),
    )
    script('throws.mjs', "throw new Error('broken check')\n")
    script('two.mjs', 'process.exit(2)\n')
    const real = commandReader({ cwd: dir })
    const ext = (name, applies) => ({ name, order: 10, applies })
    const extensions = [
      ext('boss-verify-web', 'scripts/web-only.mjs'),
      ext('boss-verify-throws', 'scripts/throws.mjs'),
      ext('boss-verify-two', 'scripts/two.mjs'),
      ext('boss-verify-missing', 'scripts/absent.mjs'),
    ]
    const run = async (diff) => {
      const w = world({ diff })
      const read = (command, args, opts) =>
        command === process.execPath ? real(command, args, opts) : w.read(command, args, opts)
      return judge(
        { pr: 7, repo: REPO },
        deps(w, { read, cwd: dir, discover: () => ({ extensions }) }),
      )
    }
    const backend = await run(['src/a.go'])
    assert.equal(backend.verdict, 'extensions-required')
    assert.deepEqual(backend.facts.extensionsNotApplicable, ['boss-verify-web'])
    assert.deepEqual(backend.facts.extensionsApplicable, [
      'boss-verify-throws',
      'boss-verify-two',
      'boss-verify-missing',
    ])
    const web = await run(['web/a.tsx'])
    assert.deepEqual(web.facts.extensionsNotApplicable, [])
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

test('judge hands the check the same envelope it would give the extension', async () => {
  const w = world({ diff: ['web/a.tsx', 'src/a.go'] })
  const gated = withApplies(w, { [GATED.applies]: exited(0) })
  const r = await judge(
    { pr: 7, repo: REPO },
    deps(w, { read: gated.read, discover: () => ({ extensions: [GATED] }) }),
  )
  assert.equal(r.verdict, 'extensions-required')
  const input = JSON.parse(gated.runs[0].input)
  assert.deepEqual(input, r.extensionEnvelope)
  assert.deepEqual(input.changedPaths, ['web/a.tsx', 'src/a.go'])
})

test('judge: a re-judge with only the applicable results never counts a skipped one as crashed', async () => {
  const dir = tmp()
  try {
    const discover = () => ({ extensions: [GATED, EXT] })
    const onlyApplicable = jsonFile(dir, 'r.json', [{ extension: EXT.name, result: passResult }])
    const w = world()
    const gated = withApplies(w, { [GATED.applies]: exited(10) })
    const r = await judge(
      { pr: 7, repo: REPO, extensionResults: onlyApplicable },
      deps(w, { read: gated.read, discover }),
    )
    assert.equal(r.verdict, 'pass')
    assert.equal(gated.runs.length, 1, 'applicability is recomputed on a re-judge')
    assert.equal(diffReads(w), 1, 'a declared check needs the diff even with results')

    // A result from an extension that does not apply is not judged at all.
    const extra = jsonFile(dir, 'x.json', [
      { extension: EXT.name, result: passResult },
      { extension: GATED.name, crashed: true },
    ])
    const w2 = world()
    const gated2 = withApplies(w2, { [GATED.applies]: exited(10) })
    const r2 = await judge(
      { pr: 7, repo: REPO, extensionResults: extra },
      deps(w2, { read: gated2.read, discover }),
    )
    assert.equal(r2.verdict, 'pass')

    // An unreadable diff waits rather than guessing applicability.
    const w3 = world({ diff: null })
    const r3 = await judge(
      { pr: 7, repo: REPO, extensionResults: onlyApplicable },
      deps(w3, { read: withApplies(w3, {}).read, discover }),
    )
    assert.equal(r3.verdict, 'wait')
    assert.equal(r3.reason, 'changed-paths-unknown')
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

test('judge: changing a declared applicability script is a policy-surface hit under a policy', async () => {
  const discover = () => ({ extensions: [GATED] })
  const baseConfig = policyText({ humanReview: { ui: ['web/**'] } })
  const w = world({ diff: [GATED.applies], baseConfig })
  const r = await judge(
    { pr: 7, repo: REPO },
    deps(w, { read: withApplies(w, { [GATED.applies]: exited(10) }).read, discover }),
  )
  assert.equal(r.facts.policySurfaceHit, true)
  assert.equal(r.verdict, 'human')
  assert.ok(r.reason.startsWith('human-review:'))

  const unconfigured = world({ diff: [GATED.applies], baseConfig: policyText({}) })
  const r2 = await judge(
    { pr: 7, repo: REPO },
    deps(unconfigured, {
      read: withApplies(unconfigured, { [GATED.applies]: exited(10) }).read,
      discover,
    }),
  )
  assert.equal(r2.facts.policySurfaceHit, false)
  assert.equal(r2.verdict, 'pass')

  assert.deepEqual(
    policySurfaceHits(['scripts/verify-applies.mjs', './scripts/verify-applies.mjs', 'x.mjs'], {
      extensionRoots: ['.claude/skills'],
      appliesScripts: ['scripts/verify-applies.mjs'],
    }),
    ['scripts/verify-applies.mjs', './scripts/verify-applies.mjs'],
  )
})

test('judge --waive lifts only that code and still refuses on red CI', async () => {
  const none = world({ statuses: [] })
  const r1 = await judge({ pr: 7, repo: REPO, waive: 'no-receipt' }, deps(none))
  assert.equal(r1.verdict, 'pass')
  assert.equal(r1.reason, 'approved')

  const two = world({
    statuses: [],
    pr: { body: BODY.replace('- [x] it works', '- [ ] it works') },
  })
  const r2 = await judge({ pr: 7, repo: REPO, waive: 'no-receipt' }, deps(two))
  assert.equal(r2.verdict, 'human')
  assert.equal(r2.reason, 'ledger-open', 'only the waived code is lifted')

  const red = world({
    statuses: [],
    pr: { statusCheckRollup: [{ name: 'ci', status: 'COMPLETED', conclusion: 'FAILURE' }] },
  })
  const r3 = await judge({ pr: 7, repo: REPO, waive: 'no-receipt' }, deps(red))
  assert.equal(r3.verdict, 'wait')
  assert.equal(r3.reason, 'ci-not-green-on-head')

  await assert.rejects(
    judge({ pr: 7, repo: REPO, waive: 'ci-not-settled' }, deps(world())),
    UsageError,
  )
})

// ---------------------------------------------------------------------------
// Approval anchor.

test('resolveApprovalAnchor: a head whose latest boss/verify is verified (approved) is the anchor', () => {
  assert.deepEqual(resolveApprovalAnchor(approvedAt(ANCHOR, 60_000)), {
    sha: ANCHOR,
    via: 'verified-approved',
    at: iso(60_000),
  })
  // GraphQL enums are upper-case; a lower-case state still counts.
  const lower = anchorPayload({
    timeline: [commitNode(ANCHOR, verifyCtx('verified (approved)', iso(1), 'success'))],
  })
  assert.equal(resolveApprovalAnchor(lower).sha, ANCHOR)
  // A plain `verified` pass is not an approval.
  const plain = anchorPayload({ timeline: [commitNode(ANCHOR, verifyCtx('verified', iso(1)))] })
  assert.deepEqual(resolveApprovalAnchor(plain), { sha: null, why: 'no-anchor' })
})

test('resolveApprovalAnchor: the later of an approved status and an APPROVED review wins', () => {
  const statusOld = commitNode(OLDER, verifyCtx('verified (approved)', iso(7_200_000)))
  const reviewNew = reviewNode(ANCHOR, 'APPROVED', iso(60_000))
  assert.deepEqual(
    resolveApprovalAnchor(anchorPayload({ reviews: [reviewNew], timeline: [statusOld] })),
    { sha: ANCHOR, via: 'review-approved', at: iso(60_000) },
  )
  const statusNew = commitNode(ANCHOR, verifyCtx('verified (approved)', iso(60_000)))
  const reviewOld = reviewNode(OLDER, 'APPROVED', iso(7_200_000))
  assert.deepEqual(
    resolveApprovalAnchor(anchorPayload({ reviews: [reviewOld], timeline: [statusNew] })),
    { sha: ANCHOR, via: 'verified-approved', at: iso(60_000) },
  )
})

test('resolveApprovalAnchor: dismissed, commented and changes-requested reviews are ignored', () => {
  for (const state of ['DISMISSED', 'COMMENTED', 'CHANGES_REQUESTED', 'PENDING'])
    assert.deepEqual(
      resolveApprovalAnchor(anchorPayload({ reviews: [reviewNode(ANCHOR, state, iso(1))] })),
      { sha: null, why: 'no-anchor' },
      state,
    )
  // An approved review on a newer commit loses to nothing when it was dismissed.
  const p = anchorPayload({
    reviews: [reviewNode(ANCHOR, 'DISMISSED', iso(1)), reviewNode(OLDER, 'APPROVED', iso(9_000))],
  })
  assert.equal(resolveApprovalAnchor(p).sha, OLDER)
  // A review whose commit is not a full SHA cannot anchor a compare.
  const short = anchorPayload({ reviews: [reviewNode('abc123', 'APPROVED', iso(1))] })
  assert.deepEqual(resolveApprovalAnchor(short), { sha: null, why: 'no-anchor' })
  // An approval from someone without write access never anchors, on the head or anywhere else.
  for (const association of ['CONTRIBUTOR', 'FIRST_TIME_CONTRIBUTOR', 'NONE', null]) {
    const outsider = anchorPayload({
      reviews: [
        reviewNode(ANCHOR, 'APPROVED', iso(1), association),
        reviewNode(OLDER, 'APPROVED', iso(9_000)),
      ],
    })
    assert.equal(resolveApprovalAnchor(outsider).sha, OLDER, `association ${association}`)
  }
})

test('resolveApprovalAnchor: a force-pushed-away head carrying verified (approved) is found', () => {
  const p = anchorPayload({
    timeline: [
      commitNode(HEAD, verifyCtx('verifying… t1', iso(1), 'PENDING')),
      forcePushNode(ANCHOR, verifyCtx('verified (approved)', iso(60_000))),
    ],
  })
  assert.deepEqual(resolveApprovalAnchor(p), {
    sha: ANCHOR,
    via: 'verified-approved',
    at: iso(60_000),
  })
  // A force-push whose before-commit is gone is skipped, not fatal.
  const gone = anchorPayload({ timeline: [{ beforeCommit: null }, {}] })
  assert.deepEqual(resolveApprovalAnchor(gone), { sha: null, why: 'no-anchor' })
})

test('resolveApprovalAnchor: a head whose latest status replaced verified (approved) is no anchor', () => {
  // GraphQL `context(name:)` returns the latest status, so an overwrite is all the read sees.
  const p = anchorPayload({
    timeline: [commitNode(ANCHOR, verifyCtx('needs human: human-review:ui', iso(1), 'PENDING'))],
  })
  assert.deepEqual(resolveApprovalAnchor(p), { sha: null, why: 'no-anchor' })
  const failed = anchorPayload({
    timeline: [commitNode(ANCHOR, verifyCtx('verified (approved)', iso(1), 'FAILURE'))],
  })
  assert.deepEqual(resolveApprovalAnchor(failed), { sha: null, why: 'no-anchor' })
})

test('resolveApprovalAnchor: an unreadable payload is no anchor with a reason, never empty', () => {
  for (const bad of [
    null,
    undefined,
    'text',
    [],
    {},
    { errors: [{ message: 'boom' }], data: anchorPayload().data },
    { data: { repository: null } },
    { data: { repository: { pullRequest: null } } },
    { data: { repository: { pullRequest: { reviews: { nodes: [] } } } } },
    {
      data: {
        repository: {
          pullRequest: { reviews: { nodes: {} }, timelineItems: { nodes: [] } },
        },
      },
    },
  ])
    assert.deepEqual(
      resolveApprovalAnchor(bad),
      { sha: null, why: 'anchor-unreadable' },
      JSON.stringify(bad),
    )
  assert.deepEqual(resolveApprovalAnchor(anchorPayload()), { sha: null, why: 'no-anchor' })
})

test('readApprovalAnchor makes one graphql read and a failed read is unreadable', () => {
  const w = world({ graphql: approvedAt(ANCHOR) })
  assert.equal(readApprovalAnchor(w.read, REPO, 7).sha, ANCHOR)
  assert.equal(w.graphqlReads().length, 1)
  const args = w.graphqlReads()[0].args
  assert.ok(args.includes('owner=o') && args.includes('name=r') && args.includes('number=7'))
  const query = args[args.indexOf('-f') + 1]
  assert.ok(query.startsWith('query='))
  assert.ok(query.includes('boss/verify'), 'the status context is named in the query')

  const failed = world({ graphql: null })
  assert.deepEqual(readApprovalAnchor(failed.read, REPO, 7), {
    sha: null,
    why: 'anchor-unreadable',
  })
  // With no repo the gh placeholders stand in for owner and name.
  const placeholder = world()
  readApprovalAnchor(placeholder.read, '', 7)
  const pargs = placeholder.graphqlReads()[0].args
  assert.ok(pargs.includes('owner={owner}') && pargs.includes('name={repo}'))
})

test('judge with no policy and no extensions reads no anchor, compare or diff', async () => {
  const w = world({ graphql: approvedAt(ANCHOR) })
  const r = await judge({ pr: 7, repo: REPO }, deps(w))
  assert.equal(r.verdict, 'pass')
  assert.equal(w.graphqlReads().length, 0)
  assert.equal(w.compareReads().length, 0)
  assert.equal(diffReads(w), 0)

  // A second-pass judge carrying extension results consumes no range either.
  const dir = tmp()
  try {
    const results = jsonFile(dir, 'r.json', [{ extension: EXT.name, result: passResult }])
    const second = world({ graphql: approvedAt(ANCHOR) })
    const r2 = await judge(
      { pr: 7, repo: REPO, extensionResults: results },
      deps(second, { discover: () => ({ extensions: [EXT] }) }),
    )
    assert.equal(r2.verdict, 'pass')
    assert.equal(second.graphqlReads().length, 0)
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

// ---------------------------------------------------------------------------
// The PR-level delta since the approval anchor.

const cfile = (filename, patch, extra = {}) => ({
  filename,
  status: 'modified',
  sha: `${filename}-blob`,
  ...(patch === undefined ? {} : { patch }),
  ...extra,
})
const UI_PATCH = '@@ -1,2 +1,3 @@ export function App() {\n   return x\n+  // tweak'
const UI_FILE = cfile('web/a.tsx', UI_PATCH)
const TEST_FILE = cfile('src/a_test.go', '@@ -0,0 +1,2 @@\n+package a\n+// test', {
  status: 'added',
})

test('prLevelDelta: shifted hunk headers are not a change; a changed hunk is', () => {
  const shifted = cfile('web/a.tsx', UI_PATCH.replace('@@ -1,2 +1,3 @@', '@@ -40,2 +44,3 @@'))
  assert.deepEqual(prLevelDelta([UI_FILE], [shifted]), [])
  const changed = cfile('web/a.tsx', `${UI_PATCH}\n+  // more`)
  assert.deepEqual(prLevelDelta([UI_FILE], [changed]), ['web/a.tsx'])
  // The hunk header's trailing context is part of the patch, so a different function is a change.
  const moved = cfile('web/a.tsx', UI_PATCH.replace('export function App()', 'function Other()'))
  assert.deepEqual(prLevelDelta([UI_FILE], [moved]), ['web/a.tsx'])
})

test('prLevelDelta: one-side paths, status changes and patchless blobs', () => {
  assert.deepEqual(prLevelDelta([UI_FILE], [UI_FILE, TEST_FILE]), ['src/a_test.go'])
  assert.deepEqual(prLevelDelta([UI_FILE, TEST_FILE], [UI_FILE]), ['src/a_test.go'])
  assert.deepEqual(prLevelDelta([UI_FILE], [{ ...UI_FILE, status: 'removed' }]), ['web/a.tsx'])
  const bin = cfile('img/logo.png', undefined, { sha: '1'.repeat(40) })
  assert.deepEqual(prLevelDelta([bin], [bin]), [])
  assert.deepEqual(prLevelDelta([bin], [{ ...bin, sha: '2'.repeat(40) }]), ['img/logo.png'])
  // A patch on one side only falls back to the blob.
  assert.deepEqual(prLevelDelta([UI_FILE], [{ ...UI_FILE, patch: undefined }]), [])
  assert.deepEqual(prLevelDelta([bin], [{ ...bin, sha: undefined }]), ['img/logo.png'])
})

test('prLevelDelta: a renamed entry in the delta also names its old path; output is sorted', () => {
  const renamed = cfile('web/b.tsx', UI_PATCH, {
    status: 'renamed',
    previous_filename: 'web/a.tsx',
  })
  assert.deepEqual(prLevelDelta([UI_FILE], [renamed]), ['web/a.tsx', 'web/b.tsx'])
  assert.deepEqual(prLevelDelta([renamed], [renamed]), [])
  const retargeted = { ...renamed, previous_filename: 'web/z.tsx' }
  assert.deepEqual(prLevelDelta([renamed], [retargeted]), ['web/a.tsx', 'web/b.tsx', 'web/z.tsx'])
  assert.deepEqual(prLevelDelta([], [TEST_FILE, UI_FILE, UI_FILE]), ['src/a_test.go', 'web/a.tsx'])
})

const uiPolicy = policyText({ humanReview: { ui: ['web/**'], database: ['migrations/**'] } })
const approvedWorld = (opts = {}) =>
  world({ graphql: approvedAt(ANCHOR), baseConfig: uiPolicy, ...opts })

test('judge after an approval: a tests-only push passes on the delta', async () => {
  const w = approvedWorld({
    diff: ['src/a_test.go', 'web/a.tsx'],
    compare: { [ANCHOR]: [UI_FILE], [HEAD]: [UI_FILE, TEST_FILE] },
  })
  const r = await judge({ pr: 7, repo: REPO }, deps(w))
  assert.equal(r.verdict, 'pass')
  assert.deepEqual(r.facts.range, { kind: 'delta', anchorSha: ANCHOR })
  assert.deepEqual(r.facts.humanReviewHits, {})
  assert.equal(w.graphqlReads().length, 1)
  assert.equal(w.compareReads().length, 2)
  assert.equal(diffReads(w), 0, 'a resolved delta never reads the full diff')
})

test('judge after an approval: a migration push re-parks database naming the anchor', async () => {
  const migration = cfile('migrations/x.sql', '@@ -0,0 +1 @@\n+create table x;', {
    status: 'added',
  })
  const w = approvedWorld({
    diff: ['migrations/x.sql', 'web/a.tsx'],
    compare: { [ANCHOR]: [UI_FILE], [HEAD]: [UI_FILE, migration] },
  })
  const r = await judge({ pr: 7, repo: REPO }, deps(w))
  assert.equal(r.verdict, 'human')
  assert.equal(r.reason, 'human-review:database')
  assert.deepEqual(r.facts.humanReviewHits, { database: ['migrations/x.sql'] })
  assert.deepEqual(r.facts.range, { kind: 'delta', anchorSha: ANCHOR })
})

test('judge after an approval: a rebase over base migrations with unchanged patches passes', async () => {
  // Each compare is three-dot, so the base's own migrations appear on neither side; only the hunk
  // headers moved.
  const rebased = cfile('web/a.tsx', UI_PATCH.replace('@@ -1,2 +1,3 @@', '@@ -7,2 +9,3 @@'))
  const w = approvedWorld({
    diff: ['web/a.tsx'],
    compare: { [ANCHOR]: [UI_FILE], [HEAD]: [rebased] },
  })
  const r = await judge({ pr: 7, repo: REPO }, deps(w))
  assert.equal(r.verdict, 'pass')
  assert.deepEqual(r.facts.range, { kind: 'delta', anchorSha: ANCHOR })
})

test('judge after an approval: a .boss-skills.json push parks every category', async () => {
  const config = cfile('.boss-skills.json', '@@ -1 +1 @@\n-{}\n+{"x":1}')
  const w = approvedWorld({
    diff: ['.boss-skills.json', 'web/a.tsx'],
    compare: { [ANCHOR]: [UI_FILE], [HEAD]: [UI_FILE, config] },
  })
  const r = await judge({ pr: 7, repo: REPO }, deps(w))
  assert.equal(r.verdict, 'human')
  assert.equal(r.reason, 'human-review:database,human-review:security,human-review:ui')
  assert.equal(r.facts.policySurfaceHit, true)
  assert.equal(r.facts.range.kind, 'delta')
})

test('judge: an unresolvable delta falls back to the full range and re-parks with a reason', async () => {
  const many = Array.from({ length: 300 }, (_, i) => cfile(`src/f${i}.go`, '@@ -1 +1 @@\n+x'))
  for (const [label, compare, why] of [
    ['compare at A fails', { [HEAD]: [UI_FILE, TEST_FILE] }, 'compare-unreadable'],
    ['compare at head fails', { [ANCHOR]: [UI_FILE] }, 'compare-unreadable'],
    [
      'files is not an array',
      { [ANCHOR]: { files: null }, [HEAD]: [UI_FILE] },
      'compare-unreadable',
    ],
    ['A reaches the file cap', { [ANCHOR]: many, [HEAD]: [UI_FILE] }, 'compare-file-cap'],
    ['head reaches the file cap', { [ANCHOR]: [UI_FILE], [HEAD]: many }, 'compare-file-cap'],
  ]) {
    const w = approvedWorld({ diff: ['src/a_test.go', 'web/a.tsx'], compare })
    const r = await judge({ pr: 7, repo: REPO }, deps(w))
    assert.equal(r.verdict, 'human', label)
    assert.equal(r.reason, 'human-review:ui', label)
    assert.deepEqual(r.facts.range, { kind: 'full', anchorSha: ANCHOR, why }, label)
    assert.equal(diffReads(w), 1, label)
  }
})

test('judge: no anchor or an unreadable anchor judges the full range', async () => {
  for (const [graphql, why] of [
    [anchorPayload(), 'no-anchor'],
    [null, 'anchor-unreadable'],
    [{ errors: [{ message: 'x' }] }, 'anchor-unreadable'],
  ]) {
    const w = world({ graphql, baseConfig: uiPolicy, diff: ['web/a.tsx'] })
    const r = await judge({ pr: 7, repo: REPO }, deps(w))
    assert.equal(r.reason, 'human-review:ui', why)
    assert.deepEqual(r.facts.range, { kind: 'full', anchorSha: null, why })
    assert.equal(w.compareReads().length, 0, why)
  }
})

test('judge: an anchor on the head itself is an empty delta with no compare reads', async () => {
  const w = world({ graphql: approvedAt(HEAD), baseConfig: uiPolicy, diff: ['web/a.tsx'] })
  const r = await judge({ pr: 7, repo: REPO }, deps(w))
  assert.equal(r.verdict, 'pass')
  assert.deepEqual(r.facts.range, { kind: 'delta', anchorSha: HEAD })
  assert.equal(w.compareReads().length, 0)
  assert.equal(diffReads(w), 0)
  // PR-state codes are still judged on the head.
  const noReceipt = world({
    graphql: approvedAt(HEAD),
    baseConfig: uiPolicy,
    statuses: [],
  })
  assert.equal((await judge({ pr: 7, repo: REPO }, deps(noReceipt))).reason, 'no-receipt')
})

test('judge with nothing consuming a range records facts.range null', async () => {
  const w = world({ graphql: approvedAt(ANCHOR) })
  const r = await judge({ pr: 7, repo: REPO }, deps(w))
  assert.equal(r.facts.range, null)
  assert.equal(w.graphqlReads().length, 0)
})

test('judge after an approval hands extensions only the delta, marked range delta', async () => {
  const w = world({
    graphql: approvedAt(ANCHOR),
    diff: ['src/a_test.go', 'web/a.tsx'],
    compare: { [ANCHOR]: [UI_FILE], [HEAD]: [UI_FILE, TEST_FILE] },
  })
  const r = await judge({ pr: 7, repo: REPO }, deps(w, { discover: () => ({ extensions: [EXT] }) }))
  assert.equal(r.verdict, 'extensions-required')
  assert.deepEqual(r.extensionEnvelope.changedPaths, ['src/a_test.go'])
  assert.deepEqual(r.extensionEnvelope.range, { kind: 'delta', anchorSha: ANCHOR })
  assert.equal(diffReads(w), 0)

  // With no anchor the envelope carries the full diff and a null anchor.
  const full = world()
  const r2 = await judge(
    { pr: 7, repo: REPO },
    deps(full, { discover: () => ({ extensions: [EXT] }) }),
  )
  assert.deepEqual(r2.extensionEnvelope.changedPaths, ['src/a.go'])
  assert.deepEqual(r2.extensionEnvelope.range, { kind: 'full', anchorSha: null })
})

// ---------------------------------------------------------------------------
// policy: the never-writing glob check.

test('policy verb prints unmatched globs as warnings and exits 0; an invalid config throws', async () => {
  const calls = []
  const read = (command, args) => {
    calls.push({ command, args })
    if (command === 'git' && args[0] === 'ls-files')
      return { ok: true, output: ['web/App.tsx', 'migrations/001.sql', ''].join('\0'), exit: 0 }
    return { ok: false, error: 'unexpected', exit: 1, output: '' }
  }
  const config = {
    verifyDefaults: {
      alwaysHumanPaths: ['migrations/**'],
      humanReview: { ui: ['web/**/*.tsx', 'web/**/*.vue'] },
    },
  }
  const r = await main(['policy'], { read, loadConfig: () => config })
  assert.deepEqual(r.policy, {
    alwaysHumanPaths: ['migrations/**'],
    humanReview: { database: [], security: [], ui: ['web/**/*.tsx', 'web/**/*.vue'] },
  })
  assert.deepEqual(r.warnings, [{ list: 'humanReview.ui', glob: 'web/**/*.vue' }])
  assert.deepEqual(calls[0], { command: 'git', args: ['ls-files', '-z'] })
  assert.equal(
    calls.filter((c) => c.command === 'gh').length,
    0,
    'the policy verb never touches the forge',
  )

  const invalid = main(['policy'], {
    read,
    loadConfig: () => {
      throw new Error('verifyDefaults.humanReview.uii is unknown')
    },
  })
  await assert.rejects(invalid, (err) => !(err instanceof UsageError) && /uii/.test(err.message))
  await assert.rejects(main(['policy', '--pr', '7'], { read }), UsageError)
})

// ---------------------------------------------------------------------------
// post: claims.

test('post claim posts a verifying status, reads it back and wins', async () => {
  const w = world()
  const r = await post({ pr: 7, head: HEAD, verdict: 'claim', repo: REPO }, deps(w))
  assert.equal(r.won, true)
  assert.equal(r.token, 'feedf00d')
  assert.deepEqual(w.statusPosts(), [
    { state: 'pending', description: 'verifying… feedf00d', target: undefined },
  ])
  assert.equal(w.notes().length, 0)
})

test('post claim loses to a later token, and a lost claim abandons the terminal post', async () => {
  const w = world({ race: 'cafe0001' })
  const r = await post({ pr: 7, head: HEAD, verdict: 'claim', repo: REPO }, deps(w))
  assert.equal(r.won, false)

  const before = w.statusPosts().length
  const p = await post(
    { pr: 7, head: HEAD, verdict: 'pass', token: 'feedf00d', repo: REPO },
    deps(w),
  )
  assert.equal(p.abandoned, 'claim-lost')
  assert.equal(w.statusPosts().length, before, 'an abandoned run posts nothing')
})

test('post claim takes over a stale claim with a note but not a fresh foreign one', async () => {
  const stale = world({
    statuses: [
      {
        context: 'boss/verify',
        state: 'pending',
        description: 'verifying… 0ld0ld00',
        updated_at: iso(CLAIM_STALE_AFTER_MS + 60_000),
      },
      receipt(),
    ],
  })
  const r1 = await post({ pr: 7, head: HEAD, verdict: 'claim', repo: REPO }, deps(stale))
  assert.equal(r1.won, true)
  assert.equal(stale.notes().length, 1)
  const noteArgs = stale.notes()[0].args
  assert.ok(noteArgs.includes('--idempotency-key'))
  assert.equal(
    noteArgs[noteArgs.indexOf('--idempotency-key') + 1],
    `verify:stale-claim-taken-over:7:${HEAD.slice(0, 12)}`,
  )
  assert.equal(noteArgs.at(-2), '--', 'the body follows -- so a leading -- is never a flag')

  const fresh = world({
    statuses: [
      {
        context: 'boss/verify',
        state: 'pending',
        description: 'verifying… 0ld0ld00',
        updated_at: iso(60_000),
      },
      receipt(),
    ],
  })
  const r2 = await post({ pr: 7, head: HEAD, verdict: 'claim', repo: REPO }, deps(fresh))
  assert.equal(r2.won, false)
  assert.equal(r2.reason, 'claim-held')
  assert.equal(fresh.mutating().length, 0)
})

// ---------------------------------------------------------------------------
// post: wait and pass.

test('post wait without a claim writes nothing; after a claim it posts waiting', async () => {
  const w = world()
  const r = await post(
    { pr: 7, head: HEAD, verdict: 'wait', reason: 'ci-not-settled', repo: REPO },
    deps(w),
  )
  assert.equal(r.posted, false)
  assert.equal(w.mutating().length, 0)

  const claimed = world({
    statuses: [
      {
        context: 'boss/verify',
        state: 'pending',
        description: 'verifying… feedf00d',
        updated_at: iso(1000),
      },
      receipt(),
    ],
  })
  const r2 = await post(
    { pr: 7, head: HEAD, verdict: 'wait', reason: 'ci-not-settled', token: 'feedf00d', repo: REPO },
    deps(claimed),
  )
  assert.equal(r2.posted, true)
  assert.deepEqual(claimed.statusPosts(), [
    { state: 'pending', description: 'waiting: ci-not-settled', target: undefined },
  ])
})

test('post wait for an unreadable base records an evidence-unknown note', async () => {
  const w = world()
  await post({ pr: 7, head: HEAD, verdict: 'wait', reason: 'base-ci-unknown', repo: REPO }, deps(w))
  assert.equal(w.statusPosts().length, 0)
  assert.equal(w.notes().length, 1)
  assert.ok(w.notes()[0].args.includes(`verify:evidence-unknown:7:${HEAD.slice(0, 12)}`))
})

test('post pass writes verified, and verified (approved) under a waiver', async () => {
  const w = world()
  const r = await post({ pr: 7, head: HEAD, verdict: 'pass', repo: REPO }, deps(w))
  assert.equal(r.posted, true)
  const approved = world()
  await post({ pr: 7, head: HEAD, verdict: 'pass', reason: 'approved', repo: REPO }, deps(approved))
  assert.deepEqual(
    [...w.statusPosts(), ...approved.statusPosts()].map((s) => [s.state, s.description]),
    [
      ['success', 'verified'],
      ['success', 'verified (approved)'],
    ],
  )
})

// ---------------------------------------------------------------------------
// post: defect.

function findingsFile(dir) {
  return jsonFile(dir, 'findings.json', [{ title: 'nil deref', file: 'a.go', line: 3 }])
}

test('post defect with auto-repair on: findings comment, then failure targeting it, no tracker writes', async () => {
  const dir = tmp()
  try {
    const w = world()
    const adapter = fakeAdapter()
    const r = await post(
      { pr: 7, head: HEAD, verdict: 'defect', findings: findingsFile(dir), repo: REPO },
      deps(w, { adapter }),
    )
    assert.equal(r.verdict, 'defect')
    const order = w
      .mutating()
      .filter((c) => c.command === 'gh')
      .map((c) => c.args[3])
    assert.deepEqual(order, ['repos/o/r/issues/7/comments', `repos/o/r/statuses/${HEAD}`])
    assert.deepEqual(w.statusPosts(), [
      {
        state: 'failure',
        description: 'defect: 1 finding(s)',
        target: `${PR_URL}#issuecomment-99`,
      },
    ])
    assert.equal(adapter.writeCalls().length, 0)
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

test('post defect with auto-repair off or unreadable also parks to a human', async () => {
  const dir = tmp()
  try {
    for (const w of [
      world({ show: { repo_can_auto_repair: false, last_repair: null } }),
      world({ show: { repo_can_auto_repair: null } }),
      world({ show: null }),
      world({ sessions: [] }),
    ]) {
      const adapter = fakeAdapter()
      const r = await post(
        { pr: 7, head: HEAD, verdict: 'defect', findings: findingsFile(dir), repo: REPO },
        deps(w, { adapter }),
      )
      assert.equal(r.verdict, 'defect')
      assert.equal(adapter.writeCalls().length, 1)
      const write = adapter.writeCalls()[0]
      assert.deepEqual(write.addLabels, ['needs-human'])
      assert.equal(write.mentionCreator, true)
      assert.equal(write.marker, verifyMarker(HEAD))
      assert.match(write.comment, /issuecomment-99/)
      assert.deepEqual(w.statusPosts(), [
        {
          state: 'failure',
          description: 'defect: 1 finding(s), needs human',
          target: `${PR_URL}#issuecomment-99`,
        },
      ])
    }
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

test('post defect after repair is exhausted becomes human repair-exhausted, never failure', async () => {
  const dir = tmp()
  try {
    const w = world({
      show: { repo_can_auto_repair: true, last_repair: { attempt_count: REPAIR_EXHAUSTED_AFTER } },
    })
    const adapter = fakeAdapter()
    const r = await post(
      { pr: 7, head: HEAD, verdict: 'defect', findings: findingsFile(dir), repo: REPO },
      deps(w, { adapter }),
    )
    assert.equal(r.verdict, 'human')
    assert.equal(r.reason, 'repair-exhausted')
    assert.deepEqual(w.statusPosts(), [
      { state: 'pending', description: 'needs human: repair-exhausted', target: undefined },
    ])
    assert.ok(w.statusPosts().every((s) => s.state !== 'failure'))
    assert.equal(adapter.writeCalls().length, 1)
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

// ---------------------------------------------------------------------------
// post: human.

test('every human reason posts needs human, adds the label and writes exactly one marked comment', async () => {
  for (const code of HUMAN_REASONS) {
    const w = world()
    const adapter = fakeAdapter()
    const r = await post(
      { pr: 7, head: HEAD, verdict: 'human', reason: code, repo: REPO },
      deps(w, { adapter }),
    )
    assert.equal(r.posted, true, code)
    assert.deepEqual(w.statusPosts(), [
      { state: 'pending', description: `needs human: ${code}`, target: undefined },
    ])
    const writes = adapter.writeCalls()
    assert.equal(writes.length, 1, code)
    assert.equal(writes[0].issueId, 'BOS-1')
    assert.deepEqual(writes[0].addLabels, ['needs-human'])
    assert.equal(writes[0].mentionCreator, true)
    assert.equal(writes[0].marker, `boss-verify head ${HEAD}`)
    assert.match(writes[0].comment, new RegExp(code))
    assert.equal(
      w.notes().length,
      code === 'extension-failed' ? 1 : 0,
      `${code}: only an extension malfunction records a note`,
    )
  }
})

test('post --reason accepts a comma list, normalises its order and names the set', async () => {
  const w = world()
  const adapter = fakeAdapter()
  const r = await post(
    {
      pr: 7,
      head: HEAD,
      verdict: 'human',
      reason: 'human-review:ui,always-human-path',
      repo: REPO,
    },
    deps(w, { adapter }),
  )
  assert.equal(r.posted, true)
  assert.equal(r.reason, 'always-human-path,human-review:ui')
  assert.deepEqual(w.statusPosts(), [
    {
      state: 'pending',
      description: 'needs human: always-human-path,human-review:ui',
      target: undefined,
    },
  ])
  assert.match(adapter.writeCalls()[0].comment, /always-human-path,human-review:ui/)

  await assert.rejects(
    post(
      { pr: 7, head: HEAD, verdict: 'human', reason: 'human-review:ui,bogus', repo: REPO },
      deps(world()),
    ),
    UsageError,
  )
})

test('post never writes a human or defect status without its tracker writes', async () => {
  const dir = tmp()
  try {
    for (const adapter of [null, {}, fakeAdapter({ missingKey: true })]) {
      for (const options of [
        { verdict: 'human', reason: 'no-receipt' },
        { verdict: 'defect', findings: findingsFile(dir) },
      ]) {
        const w = world({ show: { repo_can_auto_repair: false } })
        const r = await post({ pr: 7, head: HEAD, repo: REPO, ...options }, deps(w, { adapter }))
        assert.equal(r.trackerWrites, 'unavailable')
        assert.equal(r.posted, false)
        assert.equal(w.mutating().length, 0, 'no status, no PR comment')
        assert.ok(r.writes.some((write) => write.kind === 'tracker'))
      }
    }
    const noTicket = world({ pr: { body: BODY.replace(/^Linear issue:.*\n/, '') } })
    const r = await post(
      { pr: 7, head: HEAD, verdict: 'human', reason: 'no-receipt', repo: REPO },
      deps(noTicket),
    )
    assert.equal(r.trackerWrites, 'unavailable')
    assert.equal(noTicket.statusPosts().length, 0)
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

test('post holds the status when the tracker write fails or is indeterminate', async () => {
  for (const outcome of ['failed', 'indeterminate']) {
    const w = world()
    const r = await post(
      { pr: 7, head: HEAD, verdict: 'human', reason: 'no-receipt', repo: REPO },
      deps(w, { adapter: fakeAdapter({ outcome }) }),
    )
    assert.equal(r.trackerWrites, outcome)
    assert.equal(w.statusPosts().length, 0)
  }
})

test('post --dry-run plans every verdict and calls nothing mutating', async () => {
  const dir = tmp()
  try {
    for (const options of [
      { verdict: 'claim' },
      { verdict: 'wait', reason: 'base-ci-unknown' },
      { verdict: 'pass' },
      { verdict: 'defect', findings: findingsFile(dir) },
      { verdict: 'human', reason: 'ledger-open' },
    ]) {
      const w = world({ show: { repo_can_auto_repair: false } })
      const adapter = fakeAdapter()
      const r = await post(
        { pr: 7, head: HEAD, repo: REPO, dryRun: true, ...options },
        deps(w, { adapter }),
      )
      assert.equal(r.dryRun, true, options.verdict)
      assert.equal(w.mutating().length, 0, options.verdict)
      assert.equal(adapter.writeCalls().length, 0, options.verdict)
      assert.ok(r.writes.length > 0, `${options.verdict} lists its planned writes`)
    }
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

test('post rejects malformed input before any read', async () => {
  const w = world()
  for (const options of [
    { verdict: 'pass', head: 'abc' },
    { verdict: 'nope', head: HEAD },
    { verdict: 'human', head: HEAD, reason: 'ci-not-settled' },
    { verdict: 'wait', head: HEAD },
    { verdict: 'defect', head: HEAD, findings: '/nonexistent/findings.json' },
  ]) {
    await assert.rejects(post({ pr: 7, repo: REPO, ...options }, deps(w)), UsageError)
  }
  assert.equal(w.calls.length, 0)
})

// ---------------------------------------------------------------------------
// merge.

const verified = () => [
  { context: 'boss/verify', state: 'success', description: 'verified', updated_at: iso(1000) },
  receipt(),
]

test('merge runs the one pinned argv, observes MERGED, then moves the ticket to Done', async () => {
  const w = world({
    statuses: verified(),
    prAfterMerge: prView({ state: 'MERGED', mergeCommit: { oid: MERGED_SHA } }),
  })
  const adapter = fakeAdapter()
  const r = await merge({ pr: 7, head: HEAD, repo: REPO }, deps(w, { adapter }))
  assert.equal(r.merged, true)
  assert.equal(r.mergeSha, MERGED_SHA)
  const mergeCall = w.calls.find((c) => c.command === 'boss' && c.args[0] === 'merge')
  assert.deepEqual(mergeCall.args, ['merge', 'sess-1', '--yes', '--json', '--match-head', HEAD])
  assert.deepEqual(adapter.writeCalls(), [
    { issueId: 'BOS-1', stateName: 'Done', removeLabels: ['needs-human'] },
  ])
  assert.equal(r.trackerWrites, 'ok')
})

test('merge returns reverify on HEAD_MISMATCH and on a moved head without calling boss merge', async () => {
  const mismatch = world({ statuses: verified(), merge: { ok: false, error: 'HEAD_MISMATCH' } })
  const r1 = await merge({ pr: 7, head: HEAD, repo: REPO }, deps(mismatch))
  assert.equal(r1.verdict, 'reverify')
  assert.equal(r1.merged, false)

  const moved = world({ statuses: verified(), pr: { headRefOid: OTHER } })
  const r2 = await merge({ pr: 7, head: HEAD, repo: REPO }, deps(moved))
  assert.equal(r2.verdict, 'reverify')
  assert.equal(moved.calls.filter((c) => c.command === 'boss' && c.args[0] === 'merge').length, 0)
})

test('merge with no session row is human no-session and never falls back to gh pr merge', async () => {
  const w = world({
    statuses: verified(),
    sessions: [{ id: 'other', pr_url: 'https://github.com/o/r/pull/8' }],
  })
  const r = await merge({ pr: 7, head: HEAD, repo: REPO }, deps(w))
  assert.equal(r.verdict, 'human')
  assert.equal(r.reason, 'no-session')
  assert.equal(w.mutating().length, 0)
  assert.equal(w.calls.filter((c) => c.args[0] === 'pr' && c.args[1] === 'merge').length, 0)
})

test('merge re-checks eligibility and the verified status before merging', async () => {
  const blocked = world({ statuses: verified(), pr: { mergeStateStatus: 'BLOCKED' } })
  const r1 = await merge({ pr: 7, head: HEAD, repo: REPO }, deps(blocked))
  assert.equal(r1.verdict, 'wait')
  assert.ok(r1.reasons.includes('merge-state-blocked'))
  assert.equal(blocked.mutating().length, 0)

  const unverified = world()
  const r2 = await merge({ pr: 7, head: HEAD, repo: REPO }, deps(unverified))
  assert.equal(r2.reason, 'not-verified')
  assert.equal(unverified.mutating().length, 0)

  const claimed = world({
    statuses: [
      {
        context: 'boss/verify',
        state: 'pending',
        description: 'verifying… 11112222',
        updated_at: iso(1000),
      },
    ],
  })
  const r3 = await merge({ pr: 7, head: HEAD, repo: REPO }, deps(claimed))
  assert.equal(r3.abandoned, 'claim-lost')
})

test('merge honours an approved ledger waiver but still enforces merge state', async () => {
  const openBody = BODY.replace('- [x] it works', '- [ ] it works')
  const approvedStatuses = () => [
    { ...verified()[0], description: 'verified (approved)' },
    ...verified().slice(1),
  ]
  const waived = world({
    statuses: approvedStatuses(),
    pr: { body: openBody },
    prAfterMerge: prView({ state: 'MERGED', mergeCommit: { oid: MERGED_SHA } }),
  })
  const r1 = await merge(
    { pr: 7, head: HEAD, repo: REPO },
    deps(waived, { adapter: fakeAdapter() }),
  )
  assert.equal(r1.merged, true)

  const unwaived = world({ statuses: verified(), pr: { body: openBody } })
  const r2 = await merge({ pr: 7, head: HEAD, repo: REPO }, deps(unwaived))
  assert.equal(r2.verdict, 'wait')
  assert.ok(r2.reasons.includes('criteria-unmet'))
  assert.equal(unwaived.mutating().length, 0)

  const blocked = world({
    statuses: approvedStatuses(),
    pr: { body: openBody, mergeStateStatus: 'BLOCKED' },
  })
  const r3 = await merge({ pr: 7, head: HEAD, repo: REPO }, deps(blocked))
  assert.equal(r3.verdict, 'wait')
  assert.deepEqual(r3.reasons, ['merge-state-blocked'])
})

test('merge --dry-run plans the merge and the Done move and calls nothing mutating', async () => {
  const w = world({ statuses: verified() })
  const adapter = fakeAdapter()
  const r = await merge({ pr: 7, head: HEAD, repo: REPO, dryRun: true }, deps(w, { adapter }))
  assert.equal(r.dryRun, true)
  assert.equal(w.mutating().length, 0)
  assert.equal(adapter.writeCalls().length, 0)
  assert.deepEqual(
    r.writes.map((write) => write.kind),
    ['merge', 'tracker'],
  )
  assert.deepEqual(r.writes[0].argv, ['merge', 'sess-1', '--yes', '--json', '--match-head', HEAD])
})

// ---------------------------------------------------------------------------
// approval.

const parkedHuman = (code) => [
  {
    context: 'boss/verify',
    state: 'pending',
    description: `needs human: ${code}`,
    updated_at: iso(1000),
  },
  receipt(),
]

test('approval: label removal on a needs-human head waives only that code', async () => {
  const w = world({ statuses: parkedHuman('no-receipt') })
  const r = await approval(
    { pr: 7, repo: REPO },
    deps(w, { adapter: fakeAdapter({ labels: ['agent-build'] }) }),
  )
  assert.deepEqual(
    { approval: r.approval, waive: r.waive, via: r.via },
    { approval: 'policy-park-approved', waive: 'no-receipt', via: 'label-removed' },
  )
  assert.equal(w.mutating().length, 0)

  // The waived judge still refuses on red CI.
  const red = world({
    statuses: [],
    pr: { statusCheckRollup: [{ name: 'ci', status: 'COMPLETED', conclusion: 'FAILURE' }] },
  })
  const j = await judge({ pr: 7, repo: REPO, waive: r.waive }, deps(red))
  assert.equal(j.verdict, 'wait')
})

test('approval: a review approval counts, a human merge counts, otherwise none', async () => {
  const approved = world({
    statuses: parkedHuman('ledger-open'),
    pr: { reviewDecision: 'APPROVED' },
  })
  const r1 = await approval({ pr: 7, repo: REPO }, deps(approved))
  assert.equal(r1.approval, 'policy-park-approved')
  assert.equal(r1.via, 'review-approved')
  assert.equal(r1.waive, 'ledger-open')

  const merged = world({ pr: { state: 'MERGED', mergeCommit: { oid: MERGED_SHA } } })
  assert.equal((await approval({ pr: 7, repo: REPO }, deps(merged))).approval, 'merged-by-human')

  const still = world({ statuses: parkedHuman('ledger-open') })
  assert.equal((await approval({ pr: 7, repo: REPO }, deps(still))).approval, 'none')
})

test('classifyApproval: a set park returns the whole set as the waiver; an unknown code returns none', () => {
  const status = (description) => ({ state: 'pending', description })
  assert.deepEqual(
    classifyApproval({
      status: status('needs human: always-human-path,human-review:ui'),
      labels: ['agent-build'],
    }),
    {
      approval: 'policy-park-approved',
      waive: 'always-human-path,human-review:ui',
      via: 'label-removed',
    },
  )
  assert.deepEqual(
    classifyApproval({
      status: status('needs human: always-human-path,human-review:ux'),
      labels: ['agent-build'],
    }),
    { approval: 'none' },
  )
  assert.deepEqual(
    classifyApproval({ status: status('needs human: no-receipt'), reviewDecision: 'APPROVED' }),
    { approval: 'policy-park-approved', waive: 'no-receipt', via: 'review-approved' },
    'a single-code park behaves as before',
  )
})

test('judge --waive rejects a list with an unknown code before any read', async () => {
  const w = world()
  await assert.rejects(
    judge({ pr: 7, repo: REPO, waive: 'human-review:ui,ci-not-settled' }, deps(w)),
    UsageError,
  )
  assert.equal(w.calls.filter((c) => c.args[1] === 'view').length, 0)
})

test('approval: label removal on a parked defect head means reverify, not merge', async () => {
  const w = world({
    statuses: [
      {
        context: 'boss/verify',
        state: 'failure',
        description: 'defect: 2 finding(s), needs human',
        updated_at: iso(1000),
      },
      receipt(),
    ],
  })
  const r = await approval({ pr: 7, repo: REPO }, deps(w, { adapter: fakeAdapter({ labels: [] }) }))
  assert.equal(r.approval, 'reverify')
  assert.equal(r.waive, undefined)
})

test('classifyApproval: a repair-path defect has no label to remove; unreadable labels never approve', () => {
  const repairDefect = { state: 'failure', description: 'defect: 1 finding(s)' }
  assert.equal(
    classifyApproval({ prState: 'OPEN', status: repairDefect, labels: [] }).approval,
    'none',
  )
  assert.equal(
    classifyApproval({ prState: 'OPEN', status: repairDefect, reviewDecision: 'APPROVED' })
      .approval,
    'reverify',
  )
  const park = { state: 'pending', description: 'needs human: no-receipt' }
  assert.equal(classifyApproval({ prState: 'OPEN', status: park, labels: null }).approval, 'none')
  assert.equal(
    classifyApproval({
      prState: 'OPEN',
      status: { state: 'success', description: 'verified' },
      labels: [],
    }).approval,
    'none',
  )
})

// ---------------------------------------------------------------------------
// rearm and CLI.

test('rearm arms exactly the checks_passed_ready on-transition callback', async () => {
  const expected = [
    'callback',
    'add',
    '7',
    'checks_passed_ready',
    '--on-transition',
    '--chat',
    'chat-1',
    '--repo',
    'o/r',
    '--message',
    '/boss-verify 7',
    '--json',
  ]
  assert.deepEqual(rearmArgv({ pr: 7, chat: 'chat-1', repo: REPO }), expected)
  const w = world()
  const r = await rearm({ pr: 7, chat: 'chat-1', repo: REPO }, deps(w))
  assert.equal(r.armed, true)
  assert.deepEqual(w.mutating()[0].args, expected)

  const dry = world()
  const d = await rearm({ pr: 7, chat: 'chat-1', repo: REPO, dryRun: true }, deps(dry))
  assert.equal(d.dryRun, true)
  assert.equal(dry.mutating().length, 0)
  assert.deepEqual(d.writes, [{ kind: 'callback', argv: expected }])
})

test('main dispatches verbs and rejects usage errors before any read', async () => {
  const w = world()
  const r = await main(['judge', '--pr', '7', '--repo', REPO], deps(w))
  assert.equal(r.verdict, 'pass')
  const dry = await main(
    ['post', '--pr', '7', '--head', HEAD, '--verdict', 'pass', '--repo', REPO, '--dry-run'],
    deps(world()),
  )
  assert.equal(dry.dryRun, true)
  const fresh = world()
  for (const argv of [
    [],
    ['nope', '--pr', '7'],
    ['judge'],
    ['judge', '--pr', 'x'],
    ['judge', '--pr', '7', '--bogus', '1'],
    ['rearm', '--pr', '7'],
  ]) {
    await assert.rejects(main(argv, deps(fresh)), UsageError, JSON.stringify(argv))
  }
  assert.equal(fresh.calls.length, 0)
})

// ---------------------------------------------------------------------------
// candidates.

const SHA_OF = (n) => n.toString(16).padStart(40, '0')
const ticketBody = (id) => `Linear issue: https://linear.app/acme/issue/${id}/t\n\nbody`

/** A validated config whose tracker adapter carries the given `selection` value (or none). */
function candidateConfig(selection) {
  const config = mergeConfig(DEFAULT_CONFIG, {
    adapters: { ...DEFAULT_CONFIG.adapters, tracker: 'demo' },
    trackerConfig: {
      demo: {
        mcpServer: 'demo-tracker',
        team: 'Demo',
        states: { inReview: 'In Review' },
        labels: { agentBuild: 'agent-build', needsHuman: 'needs-human' },
        ...(selection === undefined ? {} : { selection }),
      },
    },
  })
  validateConfig(config, 'test')
  return config
}

// One open PR per ticket by default: PR n carries ticket BOS-n in its body, head SHA_OF(n).
function candidateWorld(opts = {}) {
  const tickets = opts.tickets ?? [
    { identifier: 'BOS-1', labels: ['agent-build'], attachments: [] },
  ]
  const prs =
    opts.prs ??
    tickets.map((t) => {
      const n = Number(t.identifier.split('-')[1])
      return {
        number: n,
        url: `https://github.com/o/r/pull/${n}`,
        body: ticketBody(t.identifier),
        headRefOid: SHA_OF(n),
        isDraft: false,
      }
    })
  const w = { calls: [], queries: [], probes: [] }
  w.adapter = {
    selectPlanned: async (query) => {
      w.queries.push(query)
      if (opts.trackerThrows) throw new Error('linear unreachable')
      return tickets
    },
    applyIssueWrites: async (writes) => {
      w.probes.push(writes)
      return { outcome: 'ok', issue: { labels: opts.labels?.[writes.issueId] ?? ['agent-build'] } }
    },
  }
  w.read = (command, args) => {
    w.calls.push({ command, args: [...args] })
    const ok = (payload) => ({ ok: true, payload, output: JSON.stringify(payload), exit: 0 })
    const fail = () => ({ ok: false, error: 'exit-1', exit: 1, output: '' })
    if (command !== 'gh') return fail()
    if (args[0] === 'repo') return ok({ nameWithOwner: REPO })
    if (args[0] === 'pr' && args[1] === 'list') return opts.listFails ? fail() : ok(prs)
    if (args[0] === 'pr' && args[1] === 'view') {
      const n = Number(args[2])
      const pr = prs.find((p) => p.number === n)
      if (!pr || opts.viewFails?.includes(n)) return fail()
      return ok({
        ...prView({ number: n, url: pr.url, body: pr.body, headRefOid: pr.headRefOid }),
        statusCheckRollup: opts.rollups?.[n] ?? green,
        reviewDecision: opts.reviews?.[n] ?? '',
      })
    }
    if (args[0] === 'api') {
      const sha = args[1].match(/commits\/([0-9a-f]{40})\/statuses/)?.[1]
      if (!sha) return fail()
      if (opts.statusFails?.includes(sha)) return fail()
      return { ok: true, output: JSON.stringify([opts.statuses?.[sha] ?? []]), exit: 0 }
    }
    return fail()
  }
  w.statusReads = () => w.calls.filter((c) => c.args[0] === 'api').map((c) => c.args[1])
  return w
}

const candDeps = (w, config = candidateConfig()) => ({
  read: w.read,
  now: () => NOW,
  loadConfig: () => config,
  adapter: w.adapter,
})
const verifyStatus = (state, description, msAgo = 1000) => [
  { context: 'boss/verify', state, description, updated_at: iso(msAgo) },
]

test('candidates: the selection matrix pins state to inReview, requires the build label and re-admits needs-human', async () => {
  const cases = [
    [
      'config only',
      { labels: { include: ['backend'] }, stages: { verify: { labels: { exclude: ['infra'] } } } },
      {},
      { include: ['backend'], exclude: ['infra'] },
    ],
    [
      'flag only',
      undefined,
      { labels: { exclude: ['infra'] } },
      { include: [], exclude: ['infra'] },
    ],
    [
      'flag over config',
      { labels: { include: ['backend'], exclude: ['shared-out'] } },
      { labels: { exclude: ['flag-out'] } },
      { include: ['backend'], exclude: ['flag-out'] },
    ],
  ]
  for (const [name, selection, flags, labels] of cases) {
    const w = candidateWorld()
    await candidates({ repo: REPO, selection: flags }, candDeps(w, candidateConfig(selection)))
    assert.equal(w.queries.length, 1, name)
    const q = w.queries[0]
    assert.equal(q.state, 'In Review', name)
    assert.deepEqual(q.requireLabels, ['agent-build'], name)
    assert.deepEqual(q.excludeLabels, [], name)
    assert.deepEqual(q.selection.labels, labels, name)
  }

  // --ticket bypasses the selection (flags and config) but keeps the core rules.
  const w = candidateWorld({
    tickets: [
      { identifier: 'BOS-1', labels: ['agent-build'], attachments: [] },
      { identifier: 'BOS-2', labels: ['agent-build'], attachments: [] },
    ],
  })
  const r = await candidates(
    { repo: REPO, ticket: 'bos-2', selection: { labels: { include: ['x'] } } },
    candDeps(w, candidateConfig({ labels: { include: ['backend'] } })),
  )
  assert.deepEqual(w.queries[0].selection.labels, { include: [], exclude: [] })
  assert.deepEqual(w.queries[0].requireLabels, ['agent-build'])
  assert.deepEqual(
    r.candidates.map((c) => c.pr),
    [2],
  )
  const missing = await candidates({ repo: REPO, ticket: 'BOS-9' }, candDeps(candidateWorld()))
  assert.deepEqual(missing, {
    candidates: [],
    skipped: [{ ticket: 'BOS-9', reason: 'not-in-review' }],
  })
})

test('candidates: a clean head is a candidate with its PR, head, repo and claim none', async () => {
  const w = candidateWorld()
  const r = await candidates({ repo: REPO }, candDeps(w))
  assert.deepEqual(r, {
    candidates: [
      {
        ticket: 'BOS-1',
        pr: 1,
        url: 'https://github.com/o/r/pull/1',
        headSha: SHA_OF(1),
        repo: REPO,
        claim: 'none',
      },
    ],
    skipped: [],
  })
})

test('candidates: the skip rules — verified, claimed, stale and waiting admitted, CI, draft, no PR, ambiguous', async () => {
  const ids = [1, 2, 3, 4, 5, 6]
  const tickets = [...ids, 7, 8, 9, 10].map((n) => ({
    identifier: `BOS-${n}`,
    labels: ['agent-build'],
    attachments: [],
  }))
  const prs = ids.map((n) => ({
    number: n,
    url: `https://github.com/o/r/pull/${n}`,
    body: ticketBody(`BOS-${n}`),
    headRefOid: SHA_OF(n),
    isDraft: false,
  }))
  // BOS-7: draft. BOS-8: no PR. BOS-9: two open PRs. BOS-10: attached PR whose body names BOS-1.
  prs.push({
    number: 7,
    url: 'u7',
    body: ticketBody('BOS-7'),
    headRefOid: SHA_OF(7),
    isDraft: true,
  })
  prs.push({
    number: 90,
    url: 'u90',
    body: ticketBody('BOS-9'),
    headRefOid: SHA_OF(90),
    isDraft: false,
  })
  prs.push({
    number: 91,
    url: 'u91',
    body: ticketBody('BOS-9'),
    headRefOid: SHA_OF(91),
    isDraft: false,
  })
  tickets[9].attachments = [{ url: 'https://github.com/o/r/pull/1' }]
  const w = candidateWorld({
    tickets,
    prs,
    statuses: {
      [SHA_OF(1)]: verifyStatus('success', 'verified'),
      [SHA_OF(2)]: verifyStatus('pending', 'verifying… t1', 60_000),
      [SHA_OF(3)]: verifyStatus('pending', 'verifying… t1', CLAIM_STALE_AFTER_MS + 1),
      [SHA_OF(4)]: verifyStatus('pending', 'waiting: ci-not-settled'),
    },
    rollups: {
      5: [{ name: 'ci', status: 'IN_PROGRESS', conclusion: '' }],
      6: [{ name: 'ci', status: 'COMPLETED', conclusion: 'FAILURE' }],
    },
  })
  const r = await candidates({ repo: REPO }, candDeps(w))
  assert.deepEqual(
    r.candidates.map((c) => [c.pr, c.claim]),
    [
      [3, 'stale'],
      [4, 'waiting'],
    ],
  )
  const reasons = Object.fromEntries(r.skipped.map((s) => [s.ticket, s.reason]))
  assert.deepEqual(reasons, {
    'BOS-1': 'verified',
    'BOS-2': 'claimed',
    'BOS-5': 'ci-unsettled',
    'BOS-6': 'ci-red',
    'BOS-7': 'draft',
    'BOS-8': 'no-open-pr',
    'BOS-9': 'ambiguous-pr',
    'BOS-10': 'ambiguous-pr',
  })
})

test('candidates: an attachment links a PR whose body carries no ticket line', async () => {
  const w = candidateWorld({
    tickets: [
      {
        identifier: 'BOS-4',
        labels: ['agent-build'],
        attachments: [{ url: 'https://github.com/O/R/pull/12' }],
      },
    ],
    prs: [
      { number: 12, url: 'u12', body: 'no ticket line', headRefOid: SHA_OF(12), isDraft: false },
    ],
  })
  const r = await candidates({ repo: REPO }, candDeps(w))
  assert.deepEqual(
    r.candidates.map((c) => [c.ticket, c.pr]),
    [['BOS-4', 12]],
  )
})

test('candidates: needs-human is excluded unless approval hands it back', async () => {
  const nh = (n) => ({
    identifier: `BOS-${n}`,
    labels: ['agent-build', 'needs-human'],
    attachments: [],
  })
  const w = candidateWorld({
    tickets: [nh(1), nh(2), nh(3), nh(4)],
    statuses: {
      [SHA_OF(2)]: verifyStatus('pending', 'needs human: ledger-open'),
      [SHA_OF(3)]: verifyStatus('failure', 'defect: 2 finding(s), needs human'),
      [SHA_OF(4)]: verifyStatus('pending', 'needs human: no-receipt'),
    },
    // A review approval hands BOS-2 and BOS-3 back; BOS-4 is still parked.
    reviews: { 2: 'APPROVED', 3: 'APPROVED' },
    labels: { 'BOS-2': ['needs-human'], 'BOS-3': ['needs-human'], 'BOS-4': ['needs-human'] },
  })
  const r = await candidates({ repo: REPO }, candDeps(w))
  assert.deepEqual(
    r.candidates.map((c) => ({ pr: c.pr, approval: c.approval, waive: c.waive })),
    [
      { pr: 2, approval: 'policy-park-approved', waive: 'ledger-open' },
      { pr: 3, approval: 'reverify', waive: undefined },
    ],
  )
  assert.deepEqual(
    r.skipped.map((s) => [s.ticket, s.reason]),
    [
      ['BOS-1', 'needs-human'],
      ['BOS-4', 'parked'],
    ],
  )
})

test('candidates: unreadable per-head evidence is unknown:<what>, never a candidate', async () => {
  const w = candidateWorld({
    tickets: [1, 2].map((n) => ({
      identifier: `BOS-${n}`,
      labels: ['agent-build'],
      attachments: [],
    })),
    statusFails: [SHA_OF(1)],
    viewFails: [2],
  })
  const r = await candidates({ repo: REPO }, candDeps(w))
  assert.deepEqual(r.candidates, [])
  assert.deepEqual(
    r.skipped.map((s) => s.reason),
    ['unknown:status', 'unknown:rollup'],
  )
})

test('candidates: a failed tracker read or gh pr list throws, never an empty list', async () => {
  await assert.rejects(
    candidates({ repo: REPO }, candDeps(candidateWorld({ trackerThrows: true }))),
    /linear unreachable/,
  )
  await assert.rejects(
    candidates({ repo: REPO }, candDeps(candidateWorld({ listFails: true }))),
    /gh pr list failed/,
  )
  // Through the CLI: an ordinary Error (exit 1), not a UsageError (exit 2).
  const err = await main(
    ['candidates', '--repo', REPO],
    candDeps(candidateWorld({ listFails: true })),
  ).catch((e) => e)
  assert.ok(err instanceof Error && !(err instanceof UsageError))
  await assert.rejects(main(['candidates', 'BOS-1'], candDeps(candidateWorld())), UsageError)
  await assert.rejects(main(['candidates', '--limit', '0'], candDeps(candidateWorld())), UsageError)
})

test('candidates: --limit stops the oldest-first walk once enough candidates are found', async () => {
  const w = candidateWorld({
    tickets: [3, 1, 2].map((n) => ({
      identifier: `BOS-${n}`,
      labels: ['agent-build'],
      attachments: [],
    })),
  })
  const r = await main(['candidates', '--repo', REPO, '--limit', '2'], candDeps(w))
  assert.deepEqual(
    r.candidates.map((c) => c.pr),
    [1, 2],
  )
  assert.equal(
    w.statusReads().some((path) => path.includes(SHA_OF(3))),
    false,
  )
})

test('extensionsConsulted answers the same on a first judge and a re-judge', () => {
  const facts = {
    prState: 'OPEN',
    isDraft: false,
    ci: { settled: true, green: true },
    eligibilityReasons: [],
    baseState: 'green',
    receipt: 'success',
    changedPaths: [],
    extensions: [{ name: 'boss-verify-x' }],
  }
  assert.equal(extensionsConsulted(facts), true)
  for (const extensionResults of [[], null, [{ extension: 'boss-verify-x', crashed: true }]])
    assert.equal(extensionsConsulted({ ...facts, extensionResults }), true)
  assert.equal(extensionsConsulted({ ...facts, extensions: [] }), false)
  assert.equal(extensionsConsulted({ ...facts, receipt: 'absent' }), false)
  assert.equal(extensionsConsulted({ ...facts, isDraft: true }), false)
})
