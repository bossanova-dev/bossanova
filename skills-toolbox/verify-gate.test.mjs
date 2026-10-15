import { test } from 'node:test'
import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import {
  chmodSync,
  existsSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  readdirSync,
  rmSync,
  statSync,
  symlinkSync,
  utimesSync,
  writeFileSync,
} from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'

import {
  CLAIM_STALE_AFTER_MS,
  FOREIGN_BASE_EVENTS,
  HUMAN_REASONS,
  HUMAN_REVIEW_CATEGORIES,
  HUMAN_REVIEW_REASON_PREFIX,
  REPAIR_EXHAUSTED_AFTER,
  UsageError,
  VERIFY_DESCRIPTIONS,
  aggregateExtensions,
  approval,
  approve,
  approveMarker,
  commandReader,
  candidates,
  extensionsConsulted,
  extensionRecord,
  extensionRecordMarker,
  parseExtensionRecord,
  claimState,
  classifyApproval,
  decideVerdict,
  judge,
  main,
  materializeTrustedTree,
  reapTrustedTrees,
  TRUSTED_TREE_PREFIX,
  trustedTreeParent,
  trustedTreePath,
  merge,
  observeMerge,
  operatorDecision,
  operatorMarkers,
  needsHumanDescription,
  parseHumanReasonList,
  parseLinearIssueLine,
  parseParkDescription,
  classifyHeadStatus,
  PARK_LABEL_SEPARATOR,
  policySurfaceHits,
  post,
  prLevelDelta,
  PR_FILES_CAP,
  readChangedPaths,
  readApprovalAnchor,
  readOperatorMarkers,
  resolveApprovalAnchor,
  rearm,
  rearmArgv,
  selectBaseOwnCheckRuns,
  sendBack,
  sendBackMarker,
  verifyMarker,
  standingPark,
} from './verify-gate.mjs'
import { DEFAULT_CONFIG, mergeConfig, validateConfig } from './skill-config.mjs'
import { DESCRIPTION_LIMIT } from './commit-status.mjs'
import { discoverExtensions, validateVerifyContext } from './skill-extensions.mjs'

const HEAD = 'a'.repeat(40)
const OTHER = 'd'.repeat(40)
const BASE = 'b'.repeat(40)
const MERGED_SHA = 'c'.repeat(40)
const NOW = Date.parse('2026-10-06T12:00:00Z')
const iso = (msAgo) => new Date(NOW - msAgo).toISOString()
const REPO = 'o/r'
// Where the judge's injected trusted tree (the base snapshot) lives in offline tests.
const TRUSTED = '/trusted/base-tree'
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

// When the PR was opened; a ticket description last set at or before it is honoured.
const PR_OPENED = iso(3_600_000)
const BEFORE_PR = iso(7_200_000)
const AFTER_PR = iso(60_000)

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
    createdAt: PR_OPENED,
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

// The repository-permission read for one comment author: `undefined` is an admin, `null` a failed
// read, 'not-found' a 404 (no collaborator), anything else that permission.
function permissionRead(permission) {
  if (permission === null) return { ok: false, error: 'exit-1', exit: 1, output: '' }
  if (permission === 'not-found')
    return {
      ok: false,
      error: 'exit-1',
      exit: 1,
      payload: { message: 'Not Found', status: '404' },
      output: '',
    }
  const payload = { permission: permission ?? 'admin' }
  return { ok: true, payload, output: JSON.stringify(payload), exit: 0 }
}
// The changed-paths read: the PR's files list, rename sources included.
const isPrFilesRead = (c) =>
  c.command === 'gh' && c.args[0] === 'api' && /^repos\/o\/r\/pulls\/\d+\/files\b/.test(c.args[1])

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
    // The PR's issue comments; comments POSTed through `…/issues/<n>/comments` are appended (as the
    // repo owner, stamped after every seeded one), so operator-marker read-backs are real.
    comments: opts.comments ?? [],
    commentsReadFails: opts.commentsReadFails ?? false,
    commentPosts: 0,
    // Each comment author's repository permission: 'admin' by default, `null` for a failed read,
    // 'not-found' for an account that is no collaborator (a 404).
    permissions: opts.permissions ?? {},
    user: pick('user', 'operator'),
    calls: [],
  }
  w.read = (command, args, { json } = {}) => {
    w.calls.push({ command, args: [...args] })
    const ok = (payload) => ({ ok: true, payload, output: JSON.stringify(payload), exit: 0 })
    const fail = (error = 'exit-1') => ({ ok: false, error, exit: 1, output: '' })
    if (command === 'gh' && args[0] === 'pr' && args[1] === 'view') {
      if (w.pr === null) return fail()
      const merged = w.calls.some((c) => c.command === 'boss' && c.args[0] === 'merge')
      const view = merged && w.prAfterMerge ? w.prAfterMerge : w.pr
      // Like gh, answer only the `--json` fields asked for.
      const at = args.indexOf('--json')
      if (at < 0) return ok(view)
      const fields = args[at + 1].split(',').filter((field) => field in view)
      return ok(Object.fromEntries(fields.map((field) => [field, view[field]])))
    }
    if (command === 'gh' && args[0] === 'repo') return ok({ nameWithOwner: REPO })
    if (command === 'gh' && args[0] === 'api') {
      if (args[1] === 'graphql') return w.graphql === null ? fail() : ok(w.graphql)
      // The account approve runs as: 'operator' (an admin by default), `null` for a failed read.
      if (args[1] === 'user') return w.user === null ? fail() : ok({ login: w.user })
      // The PR's files, as `--paginate --slurp` returns them: one array per page of 100. A `diff`
      // entry is a path, or `{filename, previous_filename}` for a rename.
      if (isPrFilesRead({ command, args })) {
        if (w.diff === null) return fail()
        const files = w.diff.map((d) => (typeof d === 'string' ? { filename: d } : d))
        const pages = []
        // gate-region-ok: chunks the fake files list into API pages; not a text section window.
        for (let i = 0; i < files.length; i += 100) pages.push(files.slice(i, i + 100))
        return ok(pages.length > 0 ? pages : [[]])
      }
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
        if (/\/issues\/\d+\/comments$/.test(path)) {
          const id = 99 + w.commentPosts++
          const comment = {
            id,
            body: fields.body ?? '',
            author_association: 'OWNER',
            user: { login: 'operator' },
            created_at: new Date(NOW + w.commentPosts * 1000).toISOString(),
            html_url: `${PR_URL}#issuecomment-${id}`,
          }
          w.comments.push(comment)
          return ok({ html_url: comment.html_url })
        }
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
      if (/^repos\/o\/r\/issues\/\d+\/comments\?per_page=100$/.test(path)) {
        if (w.commentsReadFails) return fail()
        return ok([w.comments])
      }
      const permission = path.match(/^repos\/o\/r\/collaborators\/([^/]+)\/permission$/)
      if (permission) return permissionRead(w.permissions[decodeURIComponent(permission[1])])
      const statusList = path.match(/commits\/([0-9a-f]{40})\/statuses/)
      if (statusList) {
        if (w.statusReadFails) return fail()
        return { ok: true, output: JSON.stringify([w.statuses[statusList[1]] ?? []]), exit: 0 }
      }
      if (path === 'repos/o/r/commits/main') {
        if (w.base === null) return fail()
        return ok({ sha: BASE })
      }
      if (path.includes(`actions/runs?head_sha=${BASE}`)) {
        if (w.base.workflowRunsFail) return fail()
        return ok(w.base.workflowRuns ?? [{ total_count: 0, workflow_runs: [] }])
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
    trustedTree: () => ({ ok: true, root: TRUSTED }),
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
    w.calls.filter(isPrFilesRead).length,
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
  const redBase = {
    runs: [{ check_runs: [{ name: 'ci', status: 'completed', conclusion: 'failure' }] }],
    statuses: [{ context: 'boss/build', state: 'success' }],
  }
  // A PR merges on its own merits by default: a red base holds it only when the base's policy
  // opts in with requireGreenBase.
  assert.equal(
    (await judge({ pr: 7, repo: REPO }, deps(world({ base: redBase })))).verdict,
    'pass',
    'a red base does not hold a green, judged PR by default',
  )
  const red = world({
    base: redBase,
    baseConfig: JSON.stringify({ verifyDefaults: { requireGreenBase: true } }),
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

// Base-own CI fixtures: a workflow run per suite and an Actions check run attached to it.
const wfRun = (suite, event, name = `wf-${suite}`, headSha = BASE) => ({
  id: suite * 10,
  check_suite_id: suite,
  event,
  name,
  head_sha: headSha,
})
const actionsCheck = (suite, name, conclusion, extra = {}) => ({
  id: suite * 100,
  name,
  app: { slug: 'github-actions' },
  check_suite: { id: suite },
  status: conclusion === null ? 'in_progress' : 'completed',
  conclusion,
  ...extra,
})
// The base guard only runs for a repo that opts in, so these worlds carry a base policy that does.
const REQUIRE_GREEN_BASE = JSON.stringify({ verifyDefaults: { requireGreenBase: true } })

const baseWorld = (workflowRuns, checkRuns, extra = {}) =>
  world({
    base: {
      runs: [{ total_count: checkRuns.length, check_runs: checkRuns }],
      statuses: [],
      workflowRuns: [{ total_count: workflowRuns.length, workflow_runs: workflowRuns }],
      ...extra,
    },
    baseConfig: REQUIRE_GREEN_BASE,
  })

test('selectBaseOwnCheckRuns drops PR-event suites, keeps base-own and non-Actions runs', () => {
  const r = selectBaseOwnCheckRuns({
    sha: BASE,
    workflowRuns: [
      { total_count: 3, workflow_runs: [wfRun(1, 'push'), wfRun(2, 'pull_request')] },
      { total_count: 3, workflow_runs: [wfRun(3, 'merge_group')] },
    ],
    checkRuns: [
      {
        total_count: 4,
        check_runs: [
          actionsCheck(1, 'scripts', 'success'),
          actionsCheck(2, 'web-e2e', 'failure'),
          actionsCheck(3, 'queue', 'failure'),
          { id: 9, name: 'gitguardian', app: { slug: 'gitguardian' }, status: 'completed' },
        ],
      },
    ],
  })
  assert.equal(r.ok, true)
  assert.equal(r.foreign, 2)
  assert.deepEqual(r.checkRuns.map((c) => c.name).sort(), ['gitguardian', 'scripts'])
  assert.ok(Object.isFrozen(FOREIGN_BASE_EVENTS))
})

test('selectBaseOwnCheckRuns names unreadable, truncated and unattributed inputs', () => {
  const page = (runs) => [{ total_count: runs.length, workflow_runs: runs }]
  const checks = (list) => [{ total_count: list.length, check_runs: list }]
  for (const [input, reason] of [
    [{ workflowRuns: null, checkRuns: checks([]) }, 'base-runs-unreadable'],
    [{ workflowRuns: [{ workflow_runs: 'x' }], checkRuns: checks([]) }, 'base-runs-unreadable'],
    [
      {
        workflowRuns: [{ total_count: 2, workflow_runs: [wfRun(1, 'push')] }],
        checkRuns: checks([]),
      },
      'base-runs-truncated',
    ],
    [{ workflowRuns: page([]), checkRuns: null }, 'base-check-runs-unreadable'],
    [
      { workflowRuns: page([]), checkRuns: [{ total_count: 5, check_runs: [] }] },
      'base-check-runs-truncated',
    ],
    [
      {
        workflowRuns: page([wfRun(1, 'push')]),
        checkRuns: checks([actionsCheck(7, 'ci', 'success')]),
      },
      'base-suite-unattributed',
    ],
    [
      {
        workflowRuns: page([wfRun(1, 'push', 'wf', 'c'.repeat(40))]),
        checkRuns: checks([actionsCheck(1, 'ci', 'success')]),
      },
      'base-suite-unattributed',
    ],
    [
      {
        workflowRuns: page([]),
        checkRuns: checks([{ ...actionsCheck(1, 'ci', 'success'), check_suite: undefined }]),
      },
      'base-suite-unattributed',
    ],
  ]) {
    const r = selectBaseOwnCheckRuns({ sha: BASE, ...input })
    assert.deepEqual(r, { ok: false, reason }, reason)
  }
})

test('selectBaseOwnCheckRuns keeps same-named workflows with distinct ids apart', () => {
  const run = (suite, workflowId) => ({ ...wfRun(suite, 'push', 'CI'), workflow_id: workflowId })
  const r = selectBaseOwnCheckRuns({
    sha: BASE,
    workflowRuns: [{ total_count: 2, workflow_runs: [run(1, 11), run(2, 22)] }],
    checkRuns: [
      {
        total_count: 2,
        check_runs: [
          actionsCheck(1, 'test', 'failure', { completed_at: '2026-01-01T00:00:00Z' }),
          actionsCheck(2, 'test', 'success', { completed_at: '2026-01-02T00:00:00Z' }),
        ],
      },
    ],
  })
  assert.equal(r.ok, true)
  assert.deepEqual(
    r.checkRuns.map((c) => c.conclusion).sort(),
    ['failure', 'success'],
    'a newer pass in one workflow must not hide a failure in another workflow of the same name',
  )
})

test('selectBaseOwnCheckRuns lets the newest completed run decide duplicates in one workflow', () => {
  const pick = (checkRuns) =>
    selectBaseOwnCheckRuns({
      sha: BASE,
      workflowRuns: [
        { workflow_runs: [wfRun(1, 'schedule', 'nightly'), wfRun(2, 'schedule', 'nightly')] },
      ],
      checkRuns: [{ check_runs: checkRuns }],
    }).checkRuns
  const older = { completed_at: '2026-10-12T00:00:00Z', started_at: '2026-10-11T23:50:00Z' }
  const newer = { completed_at: '2026-10-12T01:00:00Z', started_at: '2026-10-12T00:50:00Z' }
  const decided = pick([
    actionsCheck(1, 'job', 'failure', older),
    actionsCheck(2, 'job', 'success', newer),
  ])
  assert.deepEqual(
    decided.map((c) => c.conclusion),
    ['success'],
  )
  const pending = pick([
    actionsCheck(1, 'job', null, { started_at: older.started_at }),
    actionsCheck(2, 'job', null, { started_at: newer.started_at }),
  ])
  assert.equal(pending.length, 1)
  assert.equal(pending[0].check_suite.id, 2, 'no completed run keeps the newest')
  const separate = selectBaseOwnCheckRuns({
    sha: BASE,
    workflowRuns: [{ workflow_runs: [wfRun(1, 'push', 'a'), wfRun(2, 'push', 'b')] }],
    checkRuns: [
      { check_runs: [actionsCheck(1, 'test', 'success'), actionsCheck(2, 'test', 'failure')] },
    ],
  }).checkRuns
  assert.equal(separate.length, 2, 'same-named checks in different workflows stay separate')
})

test('judge base-own CI: a failing release-PR run on the base tip does not block the merge', async () => {
  const w = baseWorld(
    [wfRun(1, 'push', 'scripts'), wfRun(2, 'pull_request', 'web (test)')],
    [actionsCheck(1, 'scripts', 'success'), actionsCheck(2, 'web-e2e', 'failure')],
  )
  const r = await judge({ pr: 7, repo: REPO }, deps(w))
  assert.notEqual(r.reason, 'base-ci-red')
  assert.equal(r.verdict, 'pass')
  assert.equal(r.facts.base.state, 'ok')
  assert.ok(r.facts.base.foreign >= 1)

  const prOnly = baseWorld(
    [wfRun(2, 'pull_request', 'web (test)'), wfRun(3, 'pull_request', 'web (test)')],
    [actionsCheck(2, 'web-e2e', 'failure'), actionsCheck(3, 'web-e2e', 'success')],
  )
  const r2 = await judge({ pr: 7, repo: REPO }, deps(prOnly))
  assert.equal(r2.verdict, 'pass', 'no base-own gate ran')
  assert.equal(r2.facts.base.foreign, 2)
})

test('judge base-own CI: a failing push, schedule or non-Actions run still waits base-ci-red', async () => {
  for (const event of ['push', 'schedule']) {
    const w = baseWorld(
      [wfRun(1, event), wfRun(2, 'pull_request')],
      [actionsCheck(1, 'ci', 'failure'), actionsCheck(2, 'other', 'success')],
    )
    const r = await judge({ pr: 7, repo: REPO }, deps(w))
    assert.equal(r.reason, 'base-ci-red', event)
    assert.equal(r.facts.base.state, 'red')
  }
  const thirdParty = baseWorld(
    [],
    [
      {
        id: 5,
        name: 'gitguardian',
        app: { slug: 'gitguardian' },
        status: 'completed',
        conclusion: 'failure',
      },
    ],
  )
  assert.equal((await judge({ pr: 7, repo: REPO }, deps(thirdParty))).reason, 'base-ci-red')
})

test('judge base-own CI: the newest completed run of one workflow decides', async () => {
  const at = (h) => ({
    completed_at: `2026-10-12T0${h}:00:00Z`,
    started_at: `2026-10-12T0${h}:00:00Z`,
  })
  const runs = [wfRun(1, 'schedule', 'nightly'), wfRun(2, 'schedule', 'nightly')]
  const reasonFor = async (checks) =>
    (await judge({ pr: 7, repo: REPO }, deps(baseWorld(runs, checks)))).reason
  assert.equal(
    await reasonFor([
      actionsCheck(1, 'job', 'failure', at(1)),
      actionsCheck(2, 'job', 'success', at(2)),
    ]),
    'verified',
  )
  assert.equal(
    await reasonFor([
      actionsCheck(1, 'job', 'success', at(1)),
      actionsCheck(2, 'job', 'failure', at(2)),
    ]),
    'base-ci-red',
  )
  assert.equal(
    await reasonFor([
      actionsCheck(1, 'job', 'failure', at(1)),
      actionsCheck(2, 'job', null, { started_at: at(2).started_at }),
    ]),
    'base-ci-red',
    'an in-progress run does not outvote the newest completed failure',
  )
})

test('judge base-own CI: an unattributable, unreadable or truncated read waits base-ci-unknown', async () => {
  const cases = [
    [baseWorld([wfRun(1, 'push')], [actionsCheck(9, 'ci', 'success')]), 'base-suite-unattributed'],
    [baseWorld([], [], { workflowRunsFail: true }), 'base-runs-unreadable'],
    [
      world({
        base: {
          runs: [{ total_count: 0, check_runs: [] }],
          statuses: [],
          workflowRuns: [{ total_count: 150, workflow_runs: [wfRun(1, 'push')] }],
        },
        baseConfig: REQUIRE_GREEN_BASE,
      }),
      'base-runs-truncated',
    ],
  ]
  for (const [w, check] of cases) {
    const r = await judge({ pr: 7, repo: REPO }, deps(w))
    assert.equal(r.verdict, 'wait', check)
    assert.equal(r.reason, 'base-ci-unknown', check)
    assert.equal(r.facts.base.check, check)
    assert.deepEqual(
      r.notes.map((n) => n.trigger),
      ['evidence-unknown'],
      check,
    )
  }
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

test('judge counts a rename source as changed: renaming an always-human file away parks', async () => {
  const baseConfig = JSON.stringify({ verifyDefaults: { alwaysHumanPaths: ['migrations/**'] } })
  const w = world({
    diff: [{ filename: 'archive/001.sql', previous_filename: 'migrations/001.sql' }],
    baseConfig,
  })
  const r = await judge({ pr: 7, repo: REPO }, deps(w))
  assert.equal(r.verdict, 'human')
  assert.equal(r.reason, 'always-human-path')
  assert.deepEqual(r.facts.alwaysHumanHits, ['migrations/001.sql'])
})

test('the extension envelope lists a renamed file under both its names', async () => {
  const w = world({
    diff: [
      'src/a.go',
      { filename: 'docs/x.md', previous_filename: '.claude/skills/verify/features/x.md' },
    ],
  })
  const r = await judge({ pr: 7, repo: REPO }, deps(w, { discover: () => ({ extensions: [EXT] }) }))
  assert.equal(r.verdict, 'extensions-required')
  assert.deepEqual(r.extensionEnvelope.changedPaths, [
    'src/a.go',
    'docs/x.md',
    '.claude/skills/verify/features/x.md',
  ])
})

test('readChangedPaths flattens pages, dedupes, and is null on a failed, malformed or capped read', () => {
  const reader = (result) => () => result
  const ok = (payload) => ({ ok: true, payload })
  assert.deepEqual(
    readChangedPaths(
      reader(
        ok([
          [{ filename: 'a' }, { filename: 'b', previous_filename: 'a' }],
          [{ filename: 'c', previous_filename: 'd' }],
        ]),
      ),
      7,
      REPO,
    ),
    ['a', 'b', 'c', 'd'],
  )
  assert.equal(readChangedPaths(reader({ ok: false }), 7, REPO), null)
  assert.equal(readChangedPaths(reader(ok({ files: [] })), 7, REPO), null)
  assert.equal(readChangedPaths(reader(ok([[{ status: 'added' }]])), 7, REPO), null)
  const capped = Array.from({ length: PR_FILES_CAP }, (_, i) => ({ filename: `f${i}` }))
  assert.equal(readChangedPaths(reader(ok([capped])), 7, REPO), null)
  assert.equal(readChangedPaths(reader(ok([capped.slice(1)])), 7, REPO).length, PR_FILES_CAP - 1)
})

test('readChangedPaths reads every page of the PR files endpoint as one slurped JSON array', () => {
  const calls = []
  const paths = readChangedPaths(
    (...call) => {
      calls.push(call)
      return { ok: true, payload: [[{ filename: 'a' }]] }
    },
    7,
    REPO,
  )
  assert.deepEqual(paths, ['a'])
  assert.deepEqual(calls, [
    [
      'gh',
      ['api', 'repos/o/r/pulls/7/files?per_page=100', '--paginate', '--slurp'],
      { json: true },
    ],
  ])
})

const policyText = (verifyDefaults) => JSON.stringify({ verifyDefaults })
const diffReads = (w) => w.calls.filter(isPrFilesRead).length

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
    deps(
      approving(world({ diff: ['web/a.tsx'], baseConfig }), 'human-review:ui,always-human-path'),
    ),
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
    deps(
      approving(world({ diff: ['web/a.tsx', 'db/001.sql'], baseConfig }), 'human-review:database'),
    ),
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
    deps(approving(world({ statuses: [], diff: ['web/a.tsx'], baseConfig }), parked.reason)),
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

// A tracker that also answers readDescription, as the Linear adapter does.
const describingAdapter = (answer) => ({ ...fakeAdapter(), readDescription: answer })
const STORED = '## Feature map changes\n- remove `sessions.AC3`\n'

test("judge puts the ticket's stored description into the extension envelope", async () => {
  const asked = []
  const adapter = describingAdapter(async (id) => {
    asked.push(id)
    return { id: 'uuid', identifier: 'BOS-1', description: STORED, descriptionUpdatedAt: BEFORE_PR }
  })
  const r = await judge(
    { pr: 7, repo: REPO },
    deps(world(), { discover: () => ({ extensions: [EXT] }), adapter }),
  )
  assert.equal(r.verdict, 'extensions-required')
  assert.deepEqual(asked, ['BOS-1'])
  assert.deepEqual(r.extensionEnvelope.ticket, {
    id: 'BOS-1',
    url: 'https://linear.app/acme/issue/BOS-1/some-title',
    description: STORED,
  })
  assert.equal(r.facts.ticketDescription, 'read')
  assert.deepEqual(validateVerifyContext(r.extensionEnvelope), { ok: true, errors: [] })

  const waiting = world({ pr: { isDraft: true } })
  const r2 = await judge(
    { pr: 7, repo: REPO },
    deps(waiting, { discover: () => ({ extensions: [EXT] }), adapter }),
  )
  assert.equal(r2.verdict, 'wait')
  assert.deepEqual(asked, ['BOS-1'], 'a head that never reaches the extensions reads nothing')
})

test("judge leaves the description out when it cannot read the ticket's own", async () => {
  const cases = [
    [fakeAdapter(), 'no-capability'],
    [
      describingAdapter(async () => {
        throw Object.assign(new Error('LINEAR_API_KEY is not set'), {
          code: 'TRACKER_CREDENTIALS_MISSING',
        })
      }),
      'TRACKER_CREDENTIALS_MISSING',
    ],
    [describingAdapter(async () => ({ identifier: 'BOS-1', description: null })), 'unreadable'],
    [
      describingAdapter(async () => ({
        id: 'u',
        identifier: 'BOS-2',
        description: STORED,
        descriptionUpdatedAt: BEFORE_PR,
      })),
      'other-issue',
    ],
  ]
  for (const [adapter, why] of cases) {
    const r = await judge(
      { pr: 7, repo: REPO },
      deps(world(), { discover: () => ({ extensions: [EXT] }), adapter }),
    )
    assert.equal(r.verdict, 'extensions-required', why)
    assert.equal('description' in r.extensionEnvelope.ticket, false, why)
    assert.equal(r.facts.ticketDescription, why)
  }
})

test('judge ignores a ticket description edited after the PR opened, or edited at an unknown time', async () => {
  const at = (descriptionUpdatedAt) =>
    describingAdapter(async () => ({
      id: 'uuid',
      identifier: 'BOS-1',
      description: STORED,
      descriptionUpdatedAt,
    }))
  const cases = [
    [at(AFTER_PR), world(), 'edited-after-pr'],
    [at(null), world(), 'edit-time-unknown'],
    [at(undefined), world(), 'edit-time-unknown'],
    [at('not a time'), world(), 'edit-time-unknown'],
    [at(BEFORE_PR), world({ pr: { createdAt: undefined } }), 'edit-time-unknown'],
    [at(BEFORE_PR), world({ pr: { createdAt: 'garbage' } }), 'edit-time-unknown'],
  ]
  for (const [adapter, w, why] of cases) {
    const r = await judge(
      { pr: 7, repo: REPO },
      deps(w, { discover: () => ({ extensions: [EXT] }), adapter }),
    )
    assert.equal(r.verdict, 'extensions-required', why)
    assert.equal('description' in r.extensionEnvelope.ticket, false, why)
    assert.equal(r.facts.ticketDescription, why)
  }

  // Set exactly when the PR opened still counts as planned before it.
  const r = await judge(
    { pr: 7, repo: REPO },
    deps(world(), { discover: () => ({ extensions: [EXT] }), adapter: at(PR_OPENED) }),
  )
  assert.equal(r.facts.ticketDescription, 'read')
  assert.equal(r.extensionEnvelope.ticket.description, STORED)
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
      deps(approving(world(), 'extension-failed'), { discover: discoverBoth }),
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
      deps(approving(world(), 'extension-failed'), { discover }),
    )
    assert.equal(partial.verdict, 'human')
    assert.equal(partial.reason, 'human-review:security')

    // A waiver of the parked set passes.
    const approved = await judge(
      { pr: 7, repo: REPO, extensionResults: file, waive: parked.reason },
      deps(approving(world(), parked.reason), { discover }),
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
      deps(approving(world(), 'human-review:security'), { discover: discoverBoth }),
    )
    assert.equal(lifted.verdict, 'pass')
    assert.equal(lifted.reason, 'approved')

    const kept = await judge(
      { pr: 7, repo: REPO, extensionResults: file, waive: 'human-review:ui' },
      deps(approving(world(), 'human-review:ui'), { discover: discoverBoth }),
    )
    assert.equal(kept.verdict, 'human')
    assert.equal(kept.reason, 'human-review:security')
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

// An approved park whose clean extension run was recorded on the head settles from that record.
const SECURITY = 'human-review:security'
const RECORD = `${EXT.name}:pass,${OTHER_EXT.name}:${SECURITY}`
const recordComment = (head, record, opts) =>
  operatorComment(extensionRecordMarker(head, record), opts)
const judgeWith = (w, options = {}, discover = discoverBoth) =>
  judge({ pr: 7, repo: REPO, ...options }, deps(w, { discover }))

test('judge: an approved park after a clean extension run settles from its recorded run', async () => {
  const dir = tmp()
  try {
    const w = world()
    const results = jsonFile(dir, 'r.json', [
      { extension: EXT.name, result: passResult },
      { extension: OTHER_EXT.name, result: humanResult(SECURITY) },
    ])
    const parked = await judgeWith(w, { extensionResults: results })
    assert.equal(parked.verdict, 'human')
    assert.equal(parked.extensionRecord, RECORD)

    const posted = await post(
      {
        pr: 7,
        head: HEAD,
        verdict: 'human',
        reason: parked.reason,
        extensionRecord: parked.extensionRecord,
        repo: REPO,
      },
      deps(w),
    )
    assert.equal(posted.posted, true)
    assert.equal(posted.extensionRecord, 'posted')
    assert.equal(w.comments.at(-1).body.split('\n').at(-1), extensionRecordMarker(HEAD, RECORD))

    approving(w, SECURITY)
    const before = w.mutating().length
    for (const options of [{ waive: SECURITY }, {}]) {
      const settled = await judgeWith(w, options)
      assert.equal(settled.verdict, 'pass', 'no extensions-required, so no claim and no dispatch')
      assert.equal(settled.reason, 'approved')
      assert.equal(settled.extensions.reused, true)
      assert.deepEqual(
        settled.extensions.outcomes.map((o) => [o.extension, o.verdict]),
        [
          [EXT.name, 'pass'],
          [OTHER_EXT.name, 'human'],
        ],
      )
      assert.equal(settled.extensionEnvelope, undefined)
    }
    assert.equal(w.mutating().length, before, 'judge never writes')
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

test('judge: a recorded run is reused only under a waiver, on its own head, covering every applicable extension', async () => {
  const unapproved = world({ comments: [recordComment(HEAD, RECORD)] })
  assert.equal((await judgeWith(unapproved)).verdict, 'extensions-required', 'no waiver')
  const facts = {
    prState: 'OPEN',
    isDraft: false,
    ci: { settled: true, green: true },
    receipt: 'success',
    extensions: both,
    extensionRecord: parseExtensionRecord(RECORD),
  }
  assert.equal(decideVerdict(facts).verdict, 'extensions-required', 'a record without a waiver')
  assert.equal(decideVerdict({ ...facts, waive: SECURITY }).verdict, 'pass')

  const otherHead = approving(world({ comments: [recordComment(OTHER, RECORD)] }), SECURITY)
  assert.equal((await judgeWith(otherHead)).verdict, 'extensions-required', 'another head')

  const onePass = () =>
    approving(world({ comments: [recordComment(HEAD, `${EXT.name}:pass`)] }), SECURITY)
  assert.equal((await judgeWith(onePass())).verdict, 'extensions-required', 'a newly added one')
  assert.equal(
    (await judgeWith(onePass(), {}, () => ({ extensions: [EXT] }))).verdict,
    'pass',
    'the same record covers the one extension installed',
  )
})

test('judge: a record by an account without write access, or one that does not parse, is no record', async () => {
  const cases = [
    {
      comments: [recordComment(HEAD, RECORD, { login: 'mallory' })],
      permissions: { mallory: 'read' },
    },
    { comments: [recordComment(HEAD, RECORD, { association: 'CONTRIBUTOR' })] },
    { comments: [recordComment(HEAD, `${EXT.name}:pass,${OTHER_EXT.name}:fail`)] },
    { comments: [recordComment(HEAD, `${EXT.name}:pass,${EXT.name}:pass`)] },
  ]
  for (const opts of cases) {
    const r = await judgeWith(approving(world(opts), SECURITY))
    assert.equal(r.verdict, 'extensions-required', opts.comments[0].body)
  }
  const unreadable = await judgeWith(
    approving(
      world({ comments: [recordComment(HEAD, RECORD)], commentsReadFails: true }),
      SECURITY,
    ),
  )
  assert.notEqual(unreadable.verdict, 'pass', 'an unreadable comment read never passes')
})

test('judge: a recorded human reason outside the waiver still parks', async () => {
  const baseConfig = policyText({ humanReview: { ui: ['web/**'] } })
  const w = approving(
    world({ diff: ['web/a.tsx'], baseConfig, comments: [recordComment(HEAD, RECORD)] }),
    'human-review:ui',
  )
  const r = await judgeWith(w)
  assert.equal(r.verdict, 'human')
  assert.equal(r.reason, SECURITY)
})

test('extensionRecord records only a clean run: no malfunction, finding or wait', () => {
  const agg = (entries, installed = []) => aggregateExtensions(entries, installed)
  const abstained = envelope({ verdict: 'abstain', evidence: [], findings: [] })
  assert.equal(
    extensionRecord(
      agg([
        { extension: EXT.name, result: passResult },
        { extension: OTHER_EXT.name, result: humanResult(SECURITY) },
      ]),
    ),
    RECORD,
  )
  assert.equal(
    extensionRecord(agg([{ extension: EXT.name, result: abstained }])),
    `${EXT.name}:abstain`,
  )
  assert.equal(extensionRecord(agg([{ extension: EXT.name, result: failResult }])), null)
  assert.equal(extensionRecord(agg([{ extension: EXT.name, result: waitResult() }])), null)
  assert.equal(
    extensionRecord(agg([{ extension: EXT.name, optional: true, timedOut: true }])),
    null,
    'an optional extension that timed out',
  )
  assert.equal(extensionRecord(agg([], [EXT])), null, 'an installed extension with no result')
  assert.deepEqual(parseExtensionRecord(RECORD), [
    { extension: EXT.name, outcome: 'pass' },
    { extension: OTHER_EXT.name, outcome: SECURITY },
  ])
  for (const bad of ['', 'x', ':pass', 'a:pass,', 'a b:pass', 'a:human-review:nope'])
    assert.equal(parseExtensionRecord(bad), null, bad)
})

test('post --extension-record goes with a human verdict only and must parse', async () => {
  for (const options of [
    { verdict: 'pass', extensionRecord: RECORD },
    { verdict: 'human', reason: SECURITY, extensionRecord: 'not a record' },
  ])
    await assert.rejects(
      post({ pr: 7, head: HEAD, repo: REPO, ...options }, deps(world())),
      UsageError,
    )
  const w = world()
  const dry = await post(
    {
      pr: 7,
      head: HEAD,
      verdict: 'human',
      reason: SECURITY,
      extensionRecord: RECORD,
      repo: REPO,
      dryRun: true,
    },
    deps(w),
  )
  assert.equal(dry.writes.filter((x) => x.kind === 'pr-comment').length, 1)
  assert.equal(w.mutating().length, 0, 'a dry run plans the record and posts nothing')
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
      deps(approving(world({ diff: ['web/a.tsx'], baseConfig }), r2.reason), {
        discover: discoverBoth,
      }),
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
      deps(approving(world(), 'extension-failed'), { discover: discoverBoth }),
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
      deps(approving(world(), 'extension-failed'), { discover: discoverBoth }),
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
    const script = String(args[0] ?? '').startsWith(`${TRUSTED}/`)
      ? args[0].slice(TRUSTED.length + 1)
      : null
    if (command === process.execPath && args.length === 1 && script in scripts) {
      runs.push({ script, path: args[0], input: opts.input })
      const outcome = scripts[script]
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
        deps(w, {
          read,
          cwd: dir,
          trustedTree: () => ({ ok: true, root: dir }),
          discover: () => ({ extensions }),
        }),
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
  const r1 = await judge(
    { pr: 7, repo: REPO, waive: 'no-receipt' },
    deps(approving(none, 'no-receipt')),
  )
  assert.equal(r1.verdict, 'pass')
  assert.equal(r1.reason, 'approved')

  const two = world({
    statuses: [],
    pr: { body: BODY.replace('- [x] it works', '- [ ] it works') },
  })
  const r2 = await judge(
    { pr: 7, repo: REPO, waive: 'no-receipt' },
    deps(approving(two, 'no-receipt')),
  )
  assert.equal(r2.verdict, 'human')
  assert.equal(r2.reason, 'ledger-open', 'only the waived code is lifted')

  const red = world({
    statuses: [],
    pr: { statusCheckRollup: [{ name: 'ci', status: 'COMPLETED', conclusion: 'FAILURE' }] },
  })
  const r3 = await judge(
    { pr: 7, repo: REPO, waive: 'no-receipt' },
    deps(approving(red, 'no-receipt')),
  )
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

test('judge: a sent-back head never anchors the delta, so its waived paths are judged again', async () => {
  // A passed under a waiver, then the operator sent it back; B is a tests-only push.
  const sentBack = approvedWorld({
    diff: ['src/a_test.go', 'web/a.tsx'],
    compare: { [ANCHOR]: [UI_FILE], [HEAD]: [UI_FILE, TEST_FILE] },
    comments: [operatorComment(sendBackMarker(ANCHOR))],
  })
  const r = await judge({ pr: 7, repo: REPO }, deps(sentBack))
  assert.deepEqual([r.verdict, r.reason], ['human', 'human-review:ui'])
  assert.deepEqual(r.facts.range, { kind: 'full', anchorSha: null, why: 'no-anchor' })

  // Re-approved after the send-back, it anchors again; unreadable send-backs anchor nothing.
  const reapproved = approvedWorld({
    diff: ['src/a_test.go', 'web/a.tsx'],
    compare: { [ANCHOR]: [UI_FILE], [HEAD]: [UI_FILE, TEST_FILE] },
    comments: [
      operatorComment(sendBackMarker(ANCHOR), { msAgo: 120_000 }),
      operatorComment(approveMarker(ANCHOR, ['human-review:ui']), { msAgo: 60_000 }),
    ],
  })
  assert.deepEqual((await judge({ pr: 7, repo: REPO }, deps(reapproved))).facts.range, {
    kind: 'delta',
    anchorSha: ANCHOR,
  })
  const blind = approvedWorld({
    diff: ['src/a_test.go', 'web/a.tsx'],
    compare: { [ANCHOR]: [UI_FILE], [HEAD]: [UI_FILE, TEST_FILE] },
    commentsReadFails: true,
  })
  const b = await judge({ pr: 7, repo: REPO }, deps(blind))
  assert.deepEqual(b.facts.range, { kind: 'full', anchorSha: null, why: 'comments-unreadable' })
  assert.equal(b.verdict, 'human')

  const revoked = (sha) => sha === ANCHOR
  assert.deepEqual(resolveApprovalAnchor(approvedAt(ANCHOR), { revoked }), {
    sha: null,
    why: 'no-anchor',
  })
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
// observe-merge.

test('observe-merge reports a merged PR and its merge commit without merging or writing', async () => {
  const w = world({ pr: { state: 'MERGED', mergeCommit: { oid: MERGED_SHA } } })
  const adapter = fakeAdapter()
  const r = await observeMerge({ pr: 7, repo: REPO }, deps(w, { adapter }))
  assert.deepEqual(r, { merged: true, mergeSha: MERGED_SHA, observed: 'merged' })
  assert.equal(w.mutating().length, 0)
  assert.equal(
    w.calls.some((c) => c.command === 'boss'),
    false,
    'observe-merge never calls boss',
  )
  assert.equal(adapter.calls.length, 0, 'observe-merge never touches the tracker')
})

test('observe-merge: an open PR, a merged PR without a merge sha and a failed read are not merged', async () => {
  const open = world()
  assert.deepEqual(await observeMerge({ pr: 7, repo: REPO }, deps(open)), {
    merged: false,
    state: 'OPEN',
  })
  const noSha = world({ pr: { state: 'MERGED', mergeCommit: null } })
  assert.deepEqual(await observeMerge({ pr: 7, repo: REPO }, deps(noSha)), {
    merged: false,
    state: 'MERGED',
  })
  const unreadable = world()
  unreadable.pr = null
  assert.deepEqual(await observeMerge({ pr: 7, repo: REPO }, deps(unreadable)), {
    merged: false,
    reason: 'pr-unreadable',
  })
  for (const w of [open, noSha, unreadable]) {
    assert.equal(w.mutating().length, 0)
    assert.equal(
      w.calls.some((c) => c.command === 'boss'),
      false,
    )
  }
})

test('main dispatches observe-merge and refuses a merge-only flag on it', async () => {
  const w = world({ pr: { state: 'MERGED', mergeCommit: { oid: MERGED_SHA } } })
  const r = await main(['observe-merge', '--pr', '7', '--repo', REPO], deps(w))
  assert.equal(r.merged, true)
  const fresh = world()
  for (const argv of [
    ['observe-merge'],
    ['observe-merge', '--pr', '7', '--head', HEAD],
    ['observe-merge', '--pr', '7', '--dry-run'],
  ])
    await assert.rejects(main(argv, deps(fresh)), UsageError, JSON.stringify(argv))
  assert.equal(fresh.calls.length, 0)
})

// ---------------------------------------------------------------------------
// approval.

// Grants `codes` on the world's head the way a person does: a park of exactly those codes stands
// on it and the operator's approval record names them, so a judge run is handed that waiver.
function approving(w, codes) {
  const list = parseHumanReasonList(codes)
  w.statuses[HEAD] = [
    {
      context: 'boss/verify',
      state: 'pending',
      description: `needs human: ${list.join(',')}`,
      updated_at: iso(500),
    },
    ...(w.statuses[HEAD] ?? []),
  ]
  w.comments.push(operatorComment(approveMarker(HEAD, list)))
  return w
}

// A PR comment carrying an operator marker as its last line, by a trusted author unless told otherwise.
let operatorCommentId = 1
const operatorComment = (
  marker,
  { association = 'OWNER', login = 'operator', msAgo = 60_000, id = operatorCommentId++ } = {},
) => ({
  id,
  body: `**Operator**\n\n${marker}`,
  author_association: association,
  user: { login },
  created_at: iso(msAgo),
  html_url: `${PR_URL}#issuecomment-${id}`,
})

const parkedHuman = (code) => [
  {
    context: 'boss/verify',
    state: 'pending',
    description: `needs human: ${code}`,
    updated_at: iso(1000),
  },
  receipt(),
]

test('approval: label removal alone never waives a park; a head-bound record waives only its codes', async () => {
  const unlabelled = () => fakeAdapter({ labels: ['agent-build'] })
  const bare = world({ statuses: parkedHuman('no-receipt') })
  assert.deepEqual(await approval({ pr: 7, repo: REPO }, deps(bare, { adapter: unlabelled() })), {
    approval: 'none',
    headSha: HEAD,
  })

  const w = world({
    statuses: parkedHuman('no-receipt'),
    comments: [operatorComment(approveMarker(HEAD, ['no-receipt']))],
  })
  const r = await approval({ pr: 7, repo: REPO }, deps(w, { adapter: unlabelled() }))
  assert.deepEqual(
    { approval: r.approval, waive: r.waive, via: r.via },
    { approval: 'policy-park-approved', waive: 'no-receipt', via: 'operator-approved' },
  )
  assert.equal(w.mutating().length, 0)

  // The waived judge still refuses on red CI.
  const red = world({
    statuses: [],
    pr: { statusCheckRollup: [{ name: 'ci', status: 'COMPLETED', conclusion: 'FAILURE' }] },
  })
  const j = await judge({ pr: 7, repo: REPO, waive: r.waive }, deps(approving(red, r.waive)))
  assert.equal(j.verdict, 'wait')
})

test('standingPark: the newest park survives a later wait or claim, not a later verdict', () => {
  const v = (state, description, msAgo) => ({
    context: 'boss/verify',
    state,
    description,
    updated_at: iso(msAgo),
  })
  const park = v('pending', 'needs human: ledger-open', 3000)
  assert.deepEqual(standingPark([v('pending', 'waiting: stack-busy', 1000), park, receipt()]), {
    state: 'pending',
    description: 'needs human: ledger-open',
  })
  assert.equal(
    standingPark([v('pending', 'verifying… abc', 1000), park]).description,
    'needs human: ledger-open',
  )
  assert.equal(standingPark([v('success', 'verified', 1000), park]), null)
  assert.equal(standingPark([v('failure', 'defect: x', 1000), park]), null)
  assert.equal(standingPark([v('pending', 'waiting: stack-busy', 1000), receipt()]), null)
  assert.equal(standingPark(null), null)
})

test('approval and judge: an approved park survives a stack-busy wait', async () => {
  const statuses = [
    {
      context: 'boss/verify',
      state: 'pending',
      description: 'waiting: stack-busy',
      updated_at: iso(500),
    },
    ...parkedHuman('ledger-open'),
  ]
  const comments = () => [operatorComment(approveMarker(HEAD, ['ledger-open']))]
  const approved = () =>
    deps(world({ statuses, comments: comments() }), {
      adapter: fakeAdapter({ labels: ['agent-build'] }),
    })
  const r = await approval({ pr: 7, repo: REPO }, approved())
  assert.deepEqual(
    { approval: r.approval, waive: r.waive, via: r.via },
    { approval: 'policy-park-approved', waive: 'ledger-open', via: 'operator-approved' },
  )

  // The run re-entering after the wait passes no --waive; it must not re-park on the approved code.
  const openLedger = {
    body: `${BODY}\n\n## Human follow-up\n- [ ] optional dev-host check\n`,
  }
  const reentered = deps(world({ statuses, pr: openLedger, comments: comments() }), {
    adapter: fakeAdapter({ labels: ['agent-build'] }),
  })
  const j = await judge({ pr: 7, repo: REPO }, reentered)
  assert.notEqual(j.reason, 'ledger-open')
  assert.notEqual(j.verdict, 'human')

  // Without the approval record the same re-entry parks again, label or no label.
  const unapproved = deps(world({ statuses, pr: openLedger }), {
    adapter: fakeAdapter({ labels: ['agent-build'] }),
  })
  const u = await judge({ pr: 7, repo: REPO }, unapproved)
  assert.equal(u.verdict, 'human')
  assert.match(u.reason, /ledger-open/)
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
  const operator = { decision: 'approved', waives: 'always-human-path,human-review:ui' }
  assert.deepEqual(
    classifyApproval({
      status: status('needs human: always-human-path,human-review:ui'),
      labels: ['agent-build'],
      operator,
    }),
    {
      approval: 'policy-park-approved',
      waive: 'always-human-path,human-review:ui',
      via: 'operator-approved',
    },
  )
  assert.deepEqual(
    classifyApproval({
      status: status('needs human: always-human-path,human-review:ui'),
      labels: ['agent-build'],
    }),
    { approval: 'none' },
    'the label alone is not bound to a head, so it waives nothing',
  )
  assert.deepEqual(
    classifyApproval({
      status: status('needs human: always-human-path,human-review:ui'),
      operator: { decision: 'approved', waives: 'always-human-path' },
    }),
    { approval: 'none' },
    'a record for a different code set waives nothing',
  )
  assert.deepEqual(
    classifyApproval({
      status: status('needs human: always-human-path,human-review:ux'),
      labels: ['agent-build'],
      reviewDecision: 'APPROVED',
    }),
    { approval: 'none' },
  )
  assert.deepEqual(
    classifyApproval({ status: status('needs human: no-receipt'), reviewDecision: 'APPROVED' }),
    { approval: 'policy-park-approved', waive: 'no-receipt', via: 'review-approved' },
    'a single-code park behaves as before',
  )
})

// ---------------------------------------------------------------------------
// Operator-written labels on a park.

const labelledPolicy = () =>
  policyText({
    alwaysHumanPaths: [{ glob: 'gate/**', label: 'verify gate rules' }, 'web/**'],
    humanReview: {
      security: [
        { glob: 'gate/**', label: 'verify gate rules' },
        { glob: 'ci/**', label: 'CI workflows' },
      ],
      ui: [{ glob: 'web/**', label: 'web UI' }],
    },
  })

test('judge returns the labels of the matched globs, deduped, beside the codes', async () => {
  const w = world({
    diff: ['gate/rules.mjs', 'ci/build.yml', 'src/a.go'],
    baseConfig: labelledPolicy(),
  })
  const r = await judge({ pr: 7, repo: REPO }, deps(w))
  assert.equal(r.verdict, 'human')
  assert.equal(r.reason, 'always-human-path,human-review:security')
  assert.deepEqual(r.labels, ['verify gate rules', 'CI workflows'])

  const unlabelled = await judge(
    { pr: 7, repo: REPO },
    deps(
      world({ diff: ['web/a.tsx'], baseConfig: policyText({ humanReview: { ui: ['web/**'] } }) }),
    ),
  )
  assert.equal(unlabelled.reason, 'human-review:ui')
  assert.deepEqual(unlabelled.labels, [], 'an unlabelled park carries no labels')
})

test('judge drops the labels of a waived code and keeps those still parked', async () => {
  const r = await judge(
    { pr: 7, repo: REPO, waive: 'always-human-path,human-review:security' },
    deps(
      approving(
        world({ diff: ['gate/rules.mjs', 'web/a.tsx'], baseConfig: labelledPolicy() }),
        'always-human-path,human-review:security',
      ),
    ),
  )
  assert.equal(r.verdict, 'human')
  assert.equal(r.reason, 'human-review:ui')
  assert.deepEqual(r.labels, ['web UI'])
})

test('needsHumanDescription appends the label after the separator and never truncates codes', () => {
  assert.equal(PARK_LABEL_SEPARATOR, ' · ')
  assert.equal(needsHumanDescription('ledger-open'), 'needs human: ledger-open')
  assert.equal(needsHumanDescription('ledger-open', '  '), 'needs human: ledger-open')
  assert.equal(
    needsHumanDescription('human-review:security', 'verify gate rules'),
    'needs human: human-review:security · verify gate rules',
  )
  assert.equal(
    needsHumanDescription('ledger-open', 'two\nlines'),
    'needs human: ledger-open · two lines',
    'whitespace runs collapse to one space',
  )
  const codes = 'always-human-path,human-review:security'
  const long = needsHumanDescription(codes, 'x'.repeat(200))
  assert.equal(Array.from(long).length, DESCRIPTION_LIMIT)
  assert.ok(long.startsWith(`needs human: ${codes} · x`))
  assert.ok(long.endsWith('…'))
  // The widest judged set leaves room for a single label character plus the ellipsis at most.
  const worst = parseHumanReasonList(
    HUMAN_REASONS.filter(
      (c) => c !== 'repair-exhausted' && c !== 'no-session' && c !== 'extension-failed',
    ),
  ).join(',')
  const crowded = needsHumanDescription(worst, 'verify gate rules')
  assert.ok(Array.from(crowded).length <= DESCRIPTION_LIMIT)
  assert.ok(crowded.startsWith(`needs human: ${worst}`), 'codes are intact')
  // When not even one label character fits beside the codes, the label is dropped entirely.
  const full = 'x'.repeat(DESCRIPTION_LIMIT - 'needs human: '.length - 3)
  assert.equal(needsHumanDescription(full, 'label'), `needs human: ${full}`)
})

test('parseParkDescription splits codes from the label; unlabelled parks parse as before', () => {
  assert.deepEqual(parseParkDescription('needs human: human-review:security · verify gate rules'), {
    codes: ['human-review:security'],
    codesText: 'human-review:security',
    label: 'verify gate rules',
  })
  assert.deepEqual(parseParkDescription('needs human: human-review:ui,always-human-path'), {
    codes: ['always-human-path', 'human-review:ui'],
    codesText: 'human-review:ui,always-human-path',
    label: '',
  })
  assert.deepEqual(parseParkDescription('needs human: bogus · a · b'), {
    codes: null,
    codesText: 'bogus',
    label: 'a · b',
  })
  for (const notPark of ['verified', 'waiting: stack-busy', '', null, undefined, 3])
    assert.equal(parseParkDescription(notPark), null, String(notPark))
})

test('post writes the labelled description; a long label is truncated with the codes intact', async () => {
  const w = world()
  const adapter = fakeAdapter()
  const r = await post(
    {
      pr: 7,
      head: HEAD,
      verdict: 'human',
      reason: 'human-review:security,always-human-path',
      label: 'verify gate rules, CI workflows',
      repo: REPO,
      dryRun: true,
    },
    deps(w, { adapter }),
  )
  assert.equal(r.reason, 'always-human-path,human-review:security')
  const status = r.writes.find((x) => x.kind === 'status')
  assert.equal(
    status.description,
    'needs human: always-human-path,human-review:security · verify gate rules, CI workflows',
  )
  assert.match(
    r.writes.find((x) => x.kind === 'tracker').comment,
    /verify gate rules, CI workflows/,
  )

  const lw = world()
  await post(
    {
      pr: 7,
      head: HEAD,
      verdict: 'human',
      reason: 'ledger-open',
      label: 'y'.repeat(300),
      repo: REPO,
    },
    deps(lw, { adapter: fakeAdapter() }),
  )
  const [posted] = lw.statusPosts()
  assert.equal(Array.from(posted.description).length, DESCRIPTION_LIMIT)
  assert.ok(posted.description.startsWith('needs human: ledger-open · y'))
  assert.ok(posted.description.endsWith('…'))
})

test('post: a labelled park already standing is not re-posted', async () => {
  const description = 'needs human: ledger-open · verify gate rules'
  const w = world({
    statuses: [
      { context: 'boss/verify', state: 'pending', description, updated_at: iso(1000) },
      receipt(),
    ],
  })
  const r = await post(
    {
      pr: 7,
      head: HEAD,
      verdict: 'human',
      reason: 'ledger-open',
      label: 'verify gate rules',
      repo: REPO,
    },
    deps(w, { adapter: fakeAdapter() }),
  )
  assert.equal(r.skipped, 'already-posted')
  assert.equal(w.statusPosts().length, 0)
})

test('approval of a labelled park waives exactly its codes, never the label', async () => {
  const status = {
    state: 'pending',
    description: 'needs human: human-review:security,always-human-path · verify gate rules',
  }
  const operator = { decision: 'approved', waives: 'always-human-path,human-review:security' }
  assert.deepEqual(classifyApproval({ status, operator }), {
    approval: 'policy-park-approved',
    waive: 'always-human-path,human-review:security',
    via: 'operator-approved',
  })
  assert.deepEqual(
    classifyApproval({
      status: { state: 'pending', description: 'needs human: human-review:ux · verify gate rules' },
      reviewDecision: 'APPROVED',
    }),
    { approval: 'none' },
    'an unknown code in a labelled park still approves nothing',
  )

  const w = world({ statuses: parkedHuman('human-review:security · verify gate rules') })
  const r = await approve({ pr: 7, repo: REPO }, deps(w, { adapter: fakeAdapter() }))
  assert.equal(r.approved, true)
  assert.equal(r.waives, 'human-review:security')

  const v = (state, description, msAgo) => ({
    context: 'boss/verify',
    state,
    description,
    updated_at: iso(msAgo),
  })
  const park = v('pending', 'needs human: ledger-open · verify gate rules', 3000)
  assert.equal(
    standingPark([v('pending', 'waiting: stack-busy', 1000), park]).description,
    park.description,
  )
  assert.deepEqual(
    classifyHeadStatus({ state: 'pending', description: park.description }, Date.now()),
    {
      claim: 'none',
      parked: 'parked',
    },
  )
})

test('main post passes --label through to the park description', async () => {
  const w = world()
  const r = await main(
    [
      'post',
      '--pr',
      '7',
      '--head',
      HEAD,
      '--verdict',
      'human',
      '--reason',
      'human-review:security',
      '--label',
      'verify gate rules',
      '--repo',
      REPO,
      '--dry-run',
    ],
    deps(w, { adapter: fakeAdapter() }),
  )
  assert.equal(
    r.writes.find((x) => x.kind === 'status').description,
    'needs human: human-review:security · verify gate rules',
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
      const pr = args[1].match(/^repos\/o\/r\/issues\/(\d+)\/comments\?per_page=100$/)?.[1]
      if (pr)
        return opts.commentsFail?.includes(Number(pr)) ? fail() : ok([opts.comments?.[pr] ?? []])
      const login = args[1].match(/^repos\/o\/r\/collaborators\/([^/]+)\/permission$/)?.[1]
      if (login) return permissionRead(opts.permissions?.[login])
      const sha = args[1].match(/commits\/([0-9a-f]{40})\/statuses/)?.[1]
      if (!sha) return fail()
      if (opts.statusFails?.includes(sha)) return fail()
      return { ok: true, output: JSON.stringify([opts.statuses?.[sha] ?? []]), exit: 0 }
    }
    return fail()
  }
  w.statusReads = () =>
    w.calls.filter((c) => c.args[0] === 'api' && /\/statuses/.test(c.args[1])).map((c) => c.args[1])
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

// ---------------------------------------------------------------------------
// The trusted tree: extensions come from the base revision, never the PR worktree.

function writeVerifyExtension(root, name, { applies } = {}) {
  const dir = join(root, '.claude', 'skills', name)
  mkdirSync(dir, { recursive: true })
  writeFileSync(
    join(dir, 'SKILL.md'),
    [
      '---',
      `name: ${name}`,
      `description: ${name} test extension`,
      'x-boss-extension:',
      '  extends: boss-verify',
      '  role: verify',
      '  order: 10',
      ...(applies ? [`  applies: ${applies}`] : []),
      '---',
      '',
      `# ${name}`,
      '',
    ].join('\n'),
  )
}

test('judge discovers verify extensions from the trusted base tree, never the working tree', async () => {
  const head = tmp()
  const base = tmp()
  try {
    writeVerifyExtension(head, 'boss-verify-x')
    writeVerifyExtension(base, 'boss-verify-y')
    const roots = []
    const r = await judge(
      { pr: 7, repo: REPO },
      deps(world(), {
        cwd: head,
        trustedTree: ({ baseSha, cwd }) => {
          assert.equal(baseSha, BASE)
          assert.equal(cwd, head)
          return { ok: true, root: base }
        },
        discover: (options) => {
          roots.push(options.root)
          return discoverExtensions({ ...options, builtinDir: null })
        },
      }),
    )
    assert.deepEqual(roots, [base])
    assert.equal(r.verdict, 'extensions-required')
    assert.deepEqual(r.facts.extensionsInstalled, ['boss-verify-y'])
    assert.deepEqual(r.facts.extensionsApplicable, ['boss-verify-y'])
    assert.equal(r.facts.trustedRoot, base)
    assert.equal(r.facts.extensionPaths.length, 1)
    assert.ok(r.facts.extensionPaths[0].skillPath.startsWith(base))
    assert.ok(r.facts.extensionPaths[0].dir.startsWith(base))
  } finally {
    rmSync(head, { recursive: true, force: true })
    rmSync(base, { recursive: true, force: true })
  }
})

test('judge runs applies as node <trustedRoot>/<applies>', async () => {
  const w = world()
  const gated = withApplies(w, { [GATED.applies]: exited(0) })
  const r = await judge(
    { pr: 7, repo: REPO },
    deps(w, { read: gated.read, discover: () => ({ extensions: [GATED] }) }),
  )
  assert.equal(r.verdict, 'extensions-required')
  assert.equal(r.facts.trustedRoot, TRUSTED)
  assert.deepEqual(
    gated.runs.map((run) => run.path),
    [join(TRUSTED, GATED.applies)],
  )
})

test('a head-only edit to what an applies script imports changes nothing; cwd stays the checkout', async () => {
  const head = tmp()
  const base = tmp()
  try {
    // The same script in both trees; only the module it imports differs. The base copy says
    // "does not apply" (10) — but only when the cwd is the checkout (the marker is head-only).
    for (const [root, code] of [
      [base, 10],
      [head, 0],
    ]) {
      mkdirSync(join(root, 'scripts'), { recursive: true })
      writeFileSync(join(root, 'scripts', 'lib.mjs'), `export const code = ${code}\n`)
      writeFileSync(
        join(root, 'scripts', 'verify-applies.mjs'),
        [
          "import { existsSync } from 'node:fs'",
          "import { code } from './lib.mjs'",
          "process.exit(existsSync('HEAD_MARKER') ? code : 3)",
        ].join('\n'),
      )
    }
    writeFileSync(join(head, 'HEAD_MARKER'), '')
    const real = commandReader({ cwd: head })
    const w = world()
    const read = (command, args, opts) =>
      command === process.execPath ? real(command, args, opts) : w.read(command, args, opts)
    const r = await judge(
      { pr: 7, repo: REPO },
      deps(w, {
        read,
        cwd: head,
        trustedTree: () => ({ ok: true, root: base }),
        discover: () => ({ extensions: [GATED] }),
      }),
    )
    assert.equal(r.verdict, 'pass')
    assert.deepEqual(r.facts.extensionsNotApplicable, [GATED.name])
  } finally {
    rmSync(head, { recursive: true, force: true })
    rmSync(base, { recursive: true, force: true })
  }
})

test('judge: a trusted tree reaped after discovery waits and discards what it discovered', async () => {
  const w = world()
  const gated = withApplies(w, { [GATED.applies]: exited(0) })
  const r = await judge(
    { pr: 7, repo: REPO },
    deps(w, {
      read: gated.read,
      treeExists: () => false,
      discover: () => ({ extensions: [GATED, EXT] }),
    }),
  )
  assert.equal(r.verdict, 'wait')
  assert.equal(r.reason, 'trusted-tree-unavailable')
  assert.deepEqual(r.facts.extensionsInstalled, [])
  assert.equal(r.extensionEnvelope, undefined)
})

test('judge waits trusted-tree-unavailable, notes evidence-unknown, and consults no extension', async () => {
  const w = world()
  const gated = withApplies(w, { [GATED.applies]: exited(0) })
  let discovered = 0
  const r = await judge(
    { pr: 7, repo: REPO },
    deps(w, {
      read: gated.read,
      trustedTree: () => ({ ok: false, why: 'commit-unavailable' }),
      discover: () => {
        discovered += 1
        return { extensions: [GATED, EXT] }
      },
    }),
  )
  assert.equal(r.verdict, 'wait')
  assert.equal(r.reason, 'trusted-tree-unavailable')
  assert.deepEqual(
    r.notes.map((n) => n.trigger),
    ['evidence-unknown'],
  )
  assert.equal(discovered, 0)
  assert.equal(gated.runs.length, 0)
  assert.equal(r.extensionEnvelope, undefined)
  assert.equal(r.facts.trustedRoot, '')
  assert.equal(r.facts.trustedTree, 'commit-unavailable')
  assert.deepEqual(r.facts.extensionsInstalled, [])
  assert.equal(w.mutating().length, 0)
})

const git = (cwd, ...args) => {
  const r = spawnSync(
    'git',
    ['-c', 'user.name=t', '-c', 'user.email=t@t', '-c', 'commit.gpgsign=false', ...args],
    { cwd, encoding: 'utf8' },
  )
  assert.equal(r.status, 0, `git ${args.join(' ')}: ${r.stderr}`)
  return r.stdout.trim()
}

function gitRepo() {
  const repo = tmp()
  git(repo, 'init', '-q')
  writeVerifyExtension(repo, 'boss-verify-y')
  writeFileSync(join(repo, 'a.txt'), 'base\n')
  git(repo, 'add', '-A')
  git(repo, 'commit', '-q', '-m', 'base')
  return { repo, sha: git(repo, 'rev-parse', 'HEAD') }
}

test('materializeTrustedTree extracts the commit into a content-addressed path and reuses it', () => {
  const { repo, sha } = gitRepo()
  const tmpRoot = tmp()
  try {
    // An abandoned staging sibling from a crashed run is never mistaken for the tree.
    mkdirSync(trustedTreeParent(tmpRoot), { mode: 0o700 })
    const partial = `${trustedTreePath(sha, tmpRoot)}.partial-crashed`
    mkdirSync(partial)
    writeFileSync(join(partial, 'junk'), '')
    writeFileSync(join(repo, 'a.txt'), 'working tree edit\n')

    const first = materializeTrustedTree({ baseSha: sha, cwd: repo, tmpRoot })
    assert.deepEqual(first, { ok: true, root: trustedTreePath(sha, tmpRoot), reused: false })
    assert.equal(readFileSync(join(first.root, 'a.txt'), 'utf8'), 'base\n')
    assert.ok(existsSync(join(first.root, '.claude', 'skills', 'boss-verify-y', 'SKILL.md')))
    assert.equal(existsSync(join(first.root, 'junk')), false)
    assert.ok(existsSync(partial), 'a fresh staging sibling is not reaped')

    // A reuse never writes: a marker dropped into the tree survives.
    writeFileSync(join(first.root, 'marker'), '')
    const second = materializeTrustedTree({ baseSha: sha, cwd: repo, tmpRoot })
    assert.deepEqual(second, { ok: true, root: first.root, reused: true })
    assert.ok(existsSync(join(first.root, 'marker')))

    // A snapshot, not a worktree: nothing is registered in the repo's worktree list.
    assert.equal(git(repo, 'worktree', 'list', '--porcelain').split('\n\n').length, 1)
  } finally {
    rmSync(repo, { recursive: true, force: true })
    rmSync(tmpRoot, { recursive: true, force: true })
  }
})

test('materializeTrustedTree reports an unfetchable commit as unavailable', () => {
  const { repo } = gitRepo()
  const tmpRoot = tmp()
  try {
    const r = materializeTrustedTree({ baseSha: 'e'.repeat(40), cwd: repo, tmpRoot })
    assert.deepEqual(r, { ok: false, why: 'commit-unavailable' })
    assert.equal(existsSync(trustedTreePath('e'.repeat(40), tmpRoot)), false)
    assert.deepEqual(materializeTrustedTree({ baseSha: 'nope', cwd: repo, tmpRoot }), {
      ok: false,
      why: 'no-base-sha',
    })
  } finally {
    rmSync(repo, { recursive: true, force: true })
    rmSync(tmpRoot, { recursive: true, force: true })
  }
})

test('materializeTrustedTree refuses a group/world-writable or symlinked trusted parent', () => {
  const { repo, sha } = gitRepo()
  const tmpRoot = tmp()
  try {
    const parent = trustedTreeParent(tmpRoot)
    mkdirSync(parent)
    chmodSync(parent, 0o777)
    mkdirSync(trustedTreePath(sha, tmpRoot))
    assert.deepEqual(materializeTrustedTree({ baseSha: sha, cwd: repo, tmpRoot }), {
      ok: false,
      why: 'untrusted-parent',
    })
    rmSync(parent, { recursive: true, force: true })
    const elsewhere = join(tmpRoot, 'planted')
    mkdirSync(elsewhere, { mode: 0o700 })
    symlinkSync(elsewhere, parent)
    assert.deepEqual(materializeTrustedTree({ baseSha: sha, cwd: repo, tmpRoot }), {
      ok: false,
      why: 'untrusted-parent',
    })
  } finally {
    rmSync(repo, { recursive: true, force: true })
    rmSync(tmpRoot, { recursive: true, force: true })
  }
})

test('materializeTrustedTree creates a private 0700 parent', () => {
  const { repo, sha } = gitRepo()
  const tmpRoot = tmp()
  try {
    const r = materializeTrustedTree({ baseSha: sha, cwd: repo, tmpRoot })
    assert.equal(r.ok, true)
    assert.equal(statSync(trustedTreeParent(tmpRoot)).mode & 0o777, 0o700)
  } finally {
    rmSync(repo, { recursive: true, force: true })
    rmSync(tmpRoot, { recursive: true, force: true })
  }
})

test('the reaper renames a stale tree out of the prefixed namespace before deleting it', () => {
  const tmpRoot = tmp()
  try {
    const parent = trustedTreeParent(tmpRoot)
    mkdirSync(parent, { mode: 0o700 })
    const stale = trustedTreePath('f'.repeat(40), tmpRoot)
    mkdirSync(join(stale, 'locked'), { recursive: true })
    writeFileSync(join(stale, 'locked', 'f'), 'x')
    const old = new Date(NOW - 3 * 60 * 60 * 1000)
    utimesSync(stale, old, old)
    assert.deepEqual(reapTrustedTrees({ tmpRoot, now: NOW }), [stale])
    assert.equal(existsSync(stale), false)
    assert.deepEqual(
      readdirSync(parent).filter((n) => n.startsWith(TRUSTED_TREE_PREFIX)),
      [],
      'no prefixed path is left behind',
    )
  } finally {
    rmSync(tmpRoot, { recursive: true, force: true })
  }
})

test('the reaper removes stale trees for other bases and keeps the current one', () => {
  const { repo, sha } = gitRepo()
  const tmpRoot = tmp()
  try {
    const old = new Date(NOW - 3 * 60 * 60 * 1000)
    const current = trustedTreePath(sha, tmpRoot)
    const stale = trustedTreePath('f'.repeat(40), tmpRoot)
    const fresh = trustedTreePath('9'.repeat(40), tmpRoot)
    const unrelated = join(trustedTreeParent(tmpRoot), 'something-else')
    mkdirSync(trustedTreeParent(tmpRoot), { mode: 0o700 })
    for (const dir of [current, stale, fresh, unrelated]) mkdirSync(dir)
    for (const dir of [current, stale, unrelated]) utimesSync(dir, old, old)
    utimesSync(fresh, new Date(NOW), new Date(NOW))

    const r = materializeTrustedTree({ baseSha: sha, cwd: repo, tmpRoot, now: NOW })
    assert.deepEqual(r, { ok: true, root: current, reused: true })
    assert.ok(existsSync(current), 'the tree this call returns is never reaped')
    assert.equal(existsSync(stale), false)
    assert.ok(existsSync(fresh))
    assert.ok(existsSync(unrelated))
    assert.deepEqual(reapTrustedTrees({ tmpRoot, keep: current, now: NOW }), [])
  } finally {
    rmSync(repo, { recursive: true, force: true })
    rmSync(tmpRoot, { recursive: true, force: true })
  }
})

// ---------------------------------------------------------------------------
// approve / send-back: the operator verbs.

const trackerRemovals = (adapter) => adapter.calls.filter((c) => 'removeLabels' in c)

test('approve removes exactly the needs-human label for a standing, fully known park', async () => {
  const w = world({ statuses: parkedHuman('always-human-path,human-review:ui') })
  const adapter = fakeAdapter()
  const r = await approve({ pr: 7, repo: REPO }, deps(w, { adapter }))
  assert.equal(r.approved, true)
  assert.equal(r.waives, 'always-human-path,human-review:ui')
  assert.equal(r.headSha, HEAD)
  assert.equal(r.trackerWrites, 'ok')
  assert.deepEqual(trackerRemovals(adapter), [{ issueId: 'BOS-1', removeLabels: ['needs-human'] }])
  assert.deepEqual(
    adapter.writeCalls().map((c) => Object.keys(c).sort()),
    [['issueId', 'removeLabels']],
    'approve adds nothing, comments nothing on the ticket and moves no state',
  )
  const posts = w.mutating()
  assert.equal(posts.length, 1, 'approve writes exactly one PR comment and no status')
  assert.equal(posts[0].args[3], 'repos/o/r/issues/7/comments')
  const body = posts[0].args.find((a) => a.startsWith('body=')).slice('body='.length)
  assert.ok(body.trimEnd().endsWith(approveMarker(HEAD, ['always-human-path', 'human-review:ui'])))
  assert.equal(r.approvalUrl, `${PR_URL}#issuecomment-99`)
  assert.equal(
    approveMarker(HEAD, ['ledger-open']),
    `<!-- boss-approve head=${HEAD} waives=ledger-open -->`,
  )
})

test('approve uses the configured needs-human label name', async () => {
  const w = world({ statuses: parkedHuman('ledger-open') })
  const adapter = fakeAdapter({ labels: ['agent-parked'] })
  const r = await approve(
    { pr: 7, repo: REPO },
    deps(w, {
      adapter,
      loadConfig: () => ({
        adapters: { tracker: 'demo' },
        trackerConfig: { demo: { labels: { needsHuman: 'agent-parked' } } },
      }),
    }),
  )
  assert.equal(r.approved, true)
  assert.deepEqual(trackerRemovals(adapter)[0].removeLabels, ['agent-parked'])
})

test('approve judges the park beneath a later wait or claim', async () => {
  const statuses = [
    {
      context: 'boss/verify',
      state: 'pending',
      description: 'waiting: stack-busy',
      updated_at: iso(500),
    },
    ...parkedHuman('ledger-open'),
  ]
  const adapter = fakeAdapter()
  const r = await approve({ pr: 7, repo: REPO }, deps(world({ statuses }), { adapter }))
  assert.equal(r.approved, true)
  assert.equal(r.waives, 'ledger-open')
})

test('approve refuses with no park, a superseded park, an older-head park or an unknown code', async () => {
  const cases = [
    ['no status at all', world({ statuses: [receipt()] }), 'no-standing-park'],
    [
      'a later verdict superseded the park',
      world({
        statuses: [
          {
            context: 'boss/verify',
            state: 'success',
            description: 'verified',
            updated_at: iso(10),
          },
          ...parkedHuman('ledger-open'),
        ],
      }),
      'no-standing-park',
    ],
    [
      'the park stands on an older head',
      (() => {
        const w = world({ statuses: [receipt()] })
        w.statuses[OTHER] = parkedHuman('ledger-open')
        return w
      })(),
      'no-standing-park',
    ],
    [
      'the park names a code outside the vocabulary',
      world({ statuses: parkedHuman('ledger-open,human-review:ux') }),
      'park-codes-unknown',
    ],
  ]
  for (const [name, w, reason] of cases) {
    const adapter = fakeAdapter()
    const r = await approve({ pr: 7, repo: REPO }, deps(w, { adapter }))
    assert.equal(r.approved, false, name)
    assert.equal(r.reason, reason, name)
    assert.equal(r.headSha, HEAD, name)
    assert.deepEqual(adapter.writeCalls(), [], `${name}: no tracker write`)
  }
})

test('approve posts nothing for a head pushed after it read the park', async () => {
  const w = world({ statuses: parkedHuman('ledger-open') })
  const read = w.read
  let views = 0
  w.read = (command, args, opts) => {
    // approve reads HEAD and its park; the push lands before its pre-post check.
    if (command === 'gh' && args[0] === 'pr' && args[1] === 'view' && ++views === 2) {
      w.pr.headRefOid = OTHER
      w.statuses[OTHER] = parkedHuman('ledger-open,human-review:ui')
    }
    return read(command, args, opts)
  }
  const adapter = fakeAdapter()
  const r = await approve({ pr: 7, repo: REPO }, deps(w, { adapter }))
  assert.deepEqual([r.approved, r.reason, r.headSha], [false, 'head-moved', HEAD])
  assert.equal(w.mutating().length, 0, 'no record for a park nobody inspected')
  assert.deepEqual(adapter.writeCalls(), [])
})

test('approve refuses a closed or merged PR and an unreadable PR or status', async () => {
  const merged = world({
    statuses: parkedHuman('ledger-open'),
    pr: { state: 'MERGED', mergeCommit: { oid: MERGED_SHA } },
  })
  assert.equal((await approve({ pr: 7, repo: REPO }, deps(merged))).reason, 'pr-not-open')
  const unreadable = world({ pr: null })
  unreadable.pr = null
  assert.equal((await approve({ pr: 7, repo: REPO }, deps(unreadable))).reason, 'pr-unreadable')
  const blind = world({ statuses: parkedHuman('ledger-open'), statusReadFails: true })
  assert.equal((await approve({ pr: 7, repo: REPO }, deps(blind))).reason, 'status-unreadable')
})

test('approve: the label removal is housekeeping, so a tracker that cannot be written never refuses', async () => {
  for (const [name, w, adapter, outcome, why] of [
    [
      'missing credentials',
      world({ statuses: parkedHuman('no-receipt') }),
      fakeAdapter({ missingKey: true }),
      'unavailable',
      'TRACKER_CREDENTIALS_MISSING',
    ],
    [
      'no ticket line',
      world({ statuses: parkedHuman('no-receipt'), pr: { body: 'no ticket line' } }),
      fakeAdapter(),
      'unavailable',
      'no-ticket-line',
    ],
    [
      'a failed write',
      world({ statuses: parkedHuman('no-receipt') }),
      fakeAdapter({ outcome: 'failed' }),
      'failed',
      undefined,
    ],
  ]) {
    const r = await approve({ pr: 7, repo: REPO }, deps(w, { adapter }))
    assert.deepEqual(
      [r.approved, r.trackerWrites, r.why, r.approvalUrl],
      [true, outcome, why, `${PR_URL}#issuecomment-99`],
      name,
    )
    assert.equal((await approval({ pr: 7, repo: REPO }, deps(w))).approval, 'policy-park-approved')
  }
})

test('approve refuses, posting nothing, when the account it runs as cannot write to the repo', async () => {
  // A park to approve, and a send-back of a verified head to lift: both need a writer's record.
  const heads = {
    park: { statuses: parkedHuman('ledger-open') },
    // The send-back is a writer's; only the account approve runs as varies.
    lift: {
      statuses: verified(),
      comments: [operatorComment(sendBackMarker(HEAD), { login: 'lead' })],
    },
  }
  for (const [kind, head] of Object.entries(heads))
    for (const [opts, reason] of [
      [{ permissions: { operator: 'read' } }, 'approver-not-writer'],
      [{ permissions: { operator: 'not-found' } }, 'approver-not-writer'],
      [{ permissions: { operator: null } }, 'approver-unreadable'],
      [{ user: null }, 'approver-unreadable'],
    ])
      for (const dryRun of [false, true]) {
        const w = world({ ...head, ...opts })
        const adapter = fakeAdapter()
        const r = await approve({ pr: 7, repo: REPO, dryRun }, deps(w, { adapter }))
        const name = `${kind} ${JSON.stringify(opts)} dryRun=${dryRun}`
        assert.deepEqual([r.approved, r.reason, r.planned], [false, reason, undefined], name)
        assert.equal(w.mutating().length, 0, name)
        assert.deepEqual(adapter.writeCalls(), [], name)
      }
})

test('judge waits, never re-parks, when the approval read cannot be read', async () => {
  const openLedger = { body: `${BODY}\n\n## Human follow-up\n- [ ] optional dev-host check\n` }
  for (const waive of [undefined, 'ledger-open']) {
    const w = approving(world({ pr: openLedger }), 'ledger-open')
    w.commentsReadFails = true
    const j = await judge({ pr: 7, repo: REPO, ...(waive ? { waive } : {}) }, deps(w))
    assert.deepEqual([j.verdict, j.reason], ['wait', 'approval-unreadable'], String(waive))
    assert.ok(
      j.notes.some((n) => n.trigger === 'evidence-unknown'),
      'an unreadable approval is evidence-unknown',
    )
  }
})

test('judge waits when the park beneath a claim cannot be read', async () => {
  const openLedger = { body: `${BODY}\n\n## Human follow-up\n- [ ] optional dev-host check\n` }
  const w = approving(world({ pr: openLedger }), 'ledger-open')
  // The run claimed the approved park, so the park is judged beneath the claim.
  w.statuses[HEAD].unshift({
    context: 'boss/verify',
    state: 'pending',
    description: 'verifying… t1',
    updated_at: iso(100),
  })
  const read = w.read
  let statusReads = 0
  w.read = (command, args, opts) => {
    // The newest-status read succeeds; the full list the park is found in does not.
    if (command === 'gh' && /commits\/[0-9a-f]{40}\/statuses/.test(String(args[1])))
      if (++statusReads === 2) return { ok: false, error: 'exit-1', exit: 1, output: '' }
    return read(command, args, opts)
  }
  const j = await judge({ pr: 7, repo: REPO }, deps(w))
  assert.deepEqual([j.verdict, j.reason], ['wait', 'approval-unreadable'])
})

test('send-back refuses, posting nothing, when the account it runs as cannot write', async () => {
  for (const [opts, reason] of [
    [{ permissions: { operator: 'read' } }, 'approver-not-writer'],
    [{ user: null }, 'approver-unreadable'],
  ])
    for (const dryRun of [false, true]) {
      const w = world({ statuses: parkedHuman('ledger-open'), ...opts })
      const r = await sendBack({ pr: 7, repo: REPO, note: 'no', dryRun }, deps(w))
      assert.deepEqual([r.posted, r.reason, r.planned], [false, reason, undefined])
      assert.equal(w.mutating().length, 0)
    }
})

test('approve reports the precise reason a head stopped standing', async () => {
  // A PR read that fails after the record settled: the label stays, and the reason says so.
  const w = world({ statuses: parkedHuman('ledger-open') })
  const probe = fakeAdapter()
  const adapter = {
    ...probe,
    applyIssueWrites: async (writes) => {
      if (!('removeLabels' in writes)) w.pr = null
      return probe.applyIssueWrites(writes)
    },
  }
  const r = await approve({ pr: 7, repo: REPO }, deps(w, { adapter }))
  assert.deepEqual([r.approved, r.trackerWrites, r.why], [true, 'unavailable', 'pr-unreadable'])

  // A PR closed while the record is posted: nothing that stands is approved.
  const closing = world({ statuses: parkedHuman('ledger-open') })
  const read = closing.read
  closing.read = (command, args, opts) => {
    const out = read(command, args, opts)
    if (command === 'gh' && args[2] === 'POST') closing.pr.state = 'CLOSED'
    return out
  }
  const c = await approve({ pr: 7, repo: REPO }, deps(closing))
  assert.deepEqual([c.approved, c.reason], [false, 'pr-not-open'])

  // A PR or status read that fails right after the post leaves the record's effect unknown.
  for (const fail of [(w) => (w.pr = null), (w) => (w.statusReadFails = true)]) {
    const blind = world({ statuses: parkedHuman('ledger-open') })
    const inner = blind.read
    blind.read = (command, args, opts) => {
      const out = inner(command, args, opts)
      if (command === 'gh' && args[2] === 'POST') fail(blind)
      return out
    }
    const adapter = fakeAdapter()
    const u = await approve({ pr: 7, repo: REPO }, deps(blind, { adapter }))
    assert.deepEqual(
      [u.approved, u.reason, u.approvalUrl],
      [false, 'approval-unconfirmed', `${PR_URL}#issuecomment-99`],
    )
    assert.deepEqual(adapter.writeCalls(), [])
  }
})

test('approve keeps the label when the head cannot be re-read after the tracker probe', async () => {
  const w = world({ statuses: parkedHuman('ledger-open') })
  const probe = fakeAdapter()
  const adapter = {
    ...probe,
    applyIssueWrites: async (writes) => {
      // The record has settled; the status read fails while the tracker is probed.
      if (!('removeLabels' in writes)) w.statusReadFails = true
      return probe.applyIssueWrites(writes)
    },
  }
  const r = await approve({ pr: 7, repo: REPO }, deps(w, { adapter }))
  assert.deepEqual([r.approved, r.trackerWrites, r.why], [true, 'unavailable', 'status-unreadable'])
  assert.deepEqual(trackerRemovals(probe), [])
})

test('approve keeps the label of a head pushed while the tracker is probed, and lists only the writes it made', async () => {
  const w = world({ statuses: parkedHuman('ledger-open') })
  const probe = fakeAdapter()
  const adapter = {
    ...probe,
    applyIssueWrites: async (writes) => {
      // The record has settled on HEAD; a push lands while the tracker is probed.
      if (!('removeLabels' in writes)) {
        w.pr.headRefOid = OTHER
        w.statuses[OTHER] = parkedHuman('ledger-open')
      }
      return probe.applyIssueWrites(writes)
    },
  }
  const r = await approve({ pr: 7, repo: REPO }, deps(w, { adapter }))
  assert.deepEqual(
    [r.approved, r.trackerWrites, r.why, r.headSha],
    [true, 'skipped', 'head-moved', HEAD],
  )
  assert.deepEqual(trackerRemovals(probe), [], 'the newly parked head keeps its label')
  assert.deepEqual(
    r.writes.map((x) => x.kind),
    ['pr-comment'],
  )

  // A refusal lists the record it posted and no label write; an unavailable tracker lists none.
  const moved = world({ statuses: parkedHuman('ledger-open') })
  const read = moved.read
  moved.read = (command, args, opts) => {
    const out = read(command, args, opts)
    if (command === 'gh' && args[2] === 'POST') moved.pr.headRefOid = OTHER
    return out
  }
  const m = await approve({ pr: 7, repo: REPO }, deps(moved))
  assert.deepEqual([m.reason, m.writes.map((x) => x.kind)], ['head-moved', ['pr-comment']])
  const bare = world({ statuses: parkedHuman('ledger-open'), pr: { body: 'no ticket line' } })
  const b = await approve({ pr: 7, repo: REPO }, deps(bare))
  assert.deepEqual(
    [b.approved, b.trackerWrites, b.writes.map((x) => x.kind)],
    [true, 'unavailable', ['pr-comment']],
  )
})

test('approve --dry-run plans the record and the removal and writes nothing', async () => {
  const w = world({ statuses: parkedHuman('ledger-open') })
  const adapter = fakeAdapter()
  const r = await approve({ pr: 7, repo: REPO, dryRun: true }, deps(w, { adapter }))
  assert.equal(r.dryRun, true)
  assert.equal(r.approved, false)
  assert.equal(r.planned, true)
  assert.equal(r.waives, 'ledger-open')
  assert.deepEqual(
    r.writes.map((x) => x.kind),
    ['pr-comment', 'tracker'],
  )
  assert.ok(r.writes[0].body.endsWith(approveMarker(HEAD, ['ledger-open'])))
  assert.deepEqual(r.writes[1], {
    kind: 'tracker',
    issueId: 'BOS-1',
    removeLabels: ['needs-human'],
  })
  assert.deepEqual(adapter.writeCalls(), [])
  assert.equal(w.mutating().length, 0)
})

test('approve through the waiver path: the next verify run waives exactly the parked codes', async () => {
  const w = world({ statuses: parkedHuman('ledger-open') })
  const r = await approve({ pr: 7, repo: REPO }, deps(w))
  assert.equal(r.approved, true)
  // The record is on the PR; approval (what the verify run reads) waives exactly that set, whatever
  // the tracker label says.
  const after = await approval({ pr: 7, repo: REPO }, deps(w))
  assert.equal(after.approval, 'policy-park-approved')
  assert.equal(after.via, 'operator-approved')
  assert.equal(after.waive, r.waives)
})

test('send-back posts one marked PR comment and leaves the park and tracker alone', async () => {
  const w = world({ statuses: parkedHuman('ledger-open') })
  const adapter = fakeAdapter()
  const note = 'The empty state copy is wrong.\n\nUse the design doc wording.'
  const r = await sendBack({ pr: 7, repo: REPO, note }, deps(w, { adapter }))
  assert.equal(r.posted, true)
  assert.equal(r.commentUrl, `${PR_URL}#issuecomment-99`)
  assert.equal(r.headSha, HEAD)
  const posts = w.mutating()
  assert.equal(posts.length, 1)
  assert.equal(posts[0].args[3], 'repos/o/r/issues/7/comments')
  const body = posts[0].args.find((a) => a.startsWith('body=')).slice('body='.length)
  assert.ok(body.startsWith('**Sent back by the operator**'), body)
  assert.ok(body.includes(note), body)
  assert.ok(body.includes(sendBackMarker(HEAD)), body)
  assert.equal(sendBackMarker(HEAD), `<!-- boss-send-back head=${HEAD} -->`)
  assert.equal(w.statusPosts().length, 0, 'the park stays')
  assert.deepEqual(adapter.writeCalls(), [])
})

test('send-back refuses an empty note, a closed PR and reports a failed comment', async () => {
  await assert.rejects(sendBack({ pr: 7, repo: REPO, note: '  ' }, deps(world())), UsageError)
  const closed = world({ pr: { state: 'CLOSED' } })
  const c = await sendBack({ pr: 7, repo: REPO, note: 'x' }, deps(closed))
  assert.deepEqual([c.posted, c.reason], [false, 'pr-not-open'])
  assert.equal(closed.mutating().length, 0)

  const w = world()
  const read = w.read
  w.read = (command, args, opts) =>
    command === 'gh' && args[2] === 'POST'
      ? { ok: false, error: 'exit-1', exit: 1, output: '' }
      : read(command, args, opts)
  const f = await sendBack({ pr: 7, repo: REPO, note: 'x' }, deps(w))
  assert.deepEqual([f.posted, f.reason], [false, 'pr-comment-failed'])
})

test('send-back --dry-run plans the comment and posts nothing', async () => {
  const w = world()
  const r = await sendBack({ pr: 7, repo: REPO, note: 'fix it', dryRun: true }, deps(w))
  assert.equal(r.dryRun, true)
  assert.equal(r.posted, false)
  assert.equal(r.planned, true)
  assert.equal(r.writes.length, 1)
  assert.equal(r.writes[0].kind, 'pr-comment')
  assert.equal(w.mutating().length, 0)
})

// ---------------------------------------------------------------------------
// Head-bound operator decisions (BOS-1611): an approval waives only the head it names, and a
// send-back of that head revokes it.

const mergeCalls = (w) => w.calls.filter((c) => c.command === 'boss' && c.args[0] === 'merge')

test('race: a head pushed and parked while approve writes its record is never waived', async () => {
  const w = world({ statuses: parkedHuman('ledger-open') })
  const read = w.read
  // The push lands after approve's last pre-write check, while the record POST is in flight.
  w.read = (command, args, opts) => {
    const r = read(command, args, opts)
    if (command === 'gh' && args[2] === 'POST' && /\/comments$/.test(args[3])) {
      w.pr.headRefOid = OTHER
      w.statuses[OTHER] = parkedHuman('ledger-open')
    }
    return r
  }
  const adapter = fakeAdapter()
  const r = await approve({ pr: 7, repo: REPO }, deps(w, { adapter }))
  assert.deepEqual(
    [r.approved, r.reason, r.approvalUrl],
    [false, 'head-moved', `${PR_URL}#issuecomment-99`],
  )
  assert.deepEqual(trackerRemovals(adapter), [], 'the label stays, so the new head stays queued')

  // Label or no label, the new head is not approved, and the record for the old one is inert.
  const unlabelled = () => deps(w, { adapter: fakeAdapter({ labels: ['agent-build'] }) })
  const after = await approval({ pr: 7, repo: REPO }, unlabelled())
  assert.deepEqual([after.approval, after.headSha], ['none', OTHER])
  const j = await judge({ pr: 7, repo: REPO }, unlabelled())
  assert.notEqual(j.reason, 'approved', 'the new head is judged on its own merits, unwaived')
})

test("race: a head pushed between judge's approval read and its own read gets no waiver", async () => {
  const openLedger = { body: `${BODY}\n\n## Human follow-up\n- [ ] optional dev-host check\n` }
  for (const waive of [undefined, 'ledger-open']) {
    const w = approving(world({ pr: openLedger }), 'ledger-open')
    w.statuses[OTHER] = [receipt()]
    const read = w.read
    let views = 0
    w.read = (command, args, opts) => {
      // The approval read sees HEAD; the push lands before judge reads the PR itself.
      if (command === 'gh' && args[0] === 'pr' && args[1] === 'view' && ++views === 2)
        w.pr.headRefOid = OTHER
      return read(command, args, opts)
    }
    const j = await judge({ pr: 7, repo: REPO, ...(waive ? { waive } : {}) }, deps(w))
    assert.deepEqual(
      [j.verdict, j.reason, j.headSha],
      ['human', 'ledger-open', OTHER],
      String(waive),
    )
  }
})

test('judge: a waiver handed over for one head is dropped when another head is judged', async () => {
  const openLedger = { body: `${BODY}\n\n## Human follow-up\n- [ ] optional dev-host check\n` }
  const w = approving(world({ pr: openLedger }), 'ledger-open')
  // The router read HEAD's approval; a push lands before the judge runs.
  w.pr.headRefOid = OTHER
  w.statuses[OTHER] = [receipt()]
  const j = await judge({ pr: 7, repo: REPO, waive: 'ledger-open' }, deps(w))
  assert.deepEqual([j.verdict, j.reason], ['human', 'ledger-open'])

  // On the approved head the same waiver stands; a waiver wider than the approval does not.
  const ok = approving(world({ pr: openLedger }), 'ledger-open')
  const k = await judge({ pr: 7, repo: REPO, waive: 'ledger-open' }, deps(ok))
  assert.deepEqual([k.verdict, k.reason], ['pass', 'approved'])
  const wide = approving(world({ pr: openLedger }), 'ledger-open')
  const x = await judge({ pr: 7, repo: REPO, waive: 'ledger-open,no-receipt' }, deps(wide))
  assert.equal(x.verdict, 'human')
})

test('approval: a record binds to its head, the exact parked codes and a trusted author', async () => {
  const run = async (comments, statuses = parkedHuman('ledger-open')) => {
    const w = world({ statuses, comments })
    const r = await approval(
      { pr: 7, repo: REPO },
      deps(w, { adapter: fakeAdapter({ labels: [] }) }),
    )
    return r.approval
  }
  assert.equal(
    await run([operatorComment(approveMarker(HEAD, ['ledger-open']))]),
    'policy-park-approved',
  )
  assert.equal(await run([operatorComment(approveMarker(OTHER, ['ledger-open']))]), 'none')
  assert.equal(
    await run([operatorComment(approveMarker(HEAD, ['ledger-open', 'human-review:ui']))]),
    'none',
  )
  assert.equal(
    await run([operatorComment(approveMarker(HEAD, ['ledger-open']), { association: 'NONE' })]),
    'none',
  )
  assert.equal(
    await run([
      operatorComment(approveMarker(HEAD, ['ledger-open']), { association: 'CONTRIBUTOR' }),
    ]),
    'none',
  )
})

test('race: a send-back after an approval revokes it until a later approve of the same head', async () => {
  const openLedger = { body: `${BODY}\n\n## Human follow-up\n- [ ] optional dev-host check\n` }
  const w = world({ statuses: parkedHuman('ledger-open'), pr: openLedger })
  const unlabelled = () => deps(w, { adapter: fakeAdapter({ labels: ['agent-build'] }) })
  assert.equal((await approve({ pr: 7, repo: REPO }, deps(w))).approved, true)
  assert.equal(
    (await approval({ pr: 7, repo: REPO }, unlabelled())).approval,
    'policy-park-approved',
  )

  const sent = await sendBack({ pr: 7, repo: REPO, note: 'not like this' }, deps(w))
  assert.equal(sent.posted, true)
  assert.deepEqual(await approval({ pr: 7, repo: REPO }, unlabelled()), {
    approval: 'none',
    reason: 'sent-back',
    headSha: HEAD,
  })
  // The verify run re-entering with no --waive re-parks the rejected head.
  const j = await judge({ pr: 7, repo: REPO }, unlabelled())
  assert.equal(j.verdict, 'human')
  assert.match(j.reason, /ledger-open/)
  // A review approval of the same head does not override the send-back either.
  w.pr.reviewDecision = 'APPROVED'
  assert.equal((await approval({ pr: 7, repo: REPO }, unlabelled())).reason, 'sent-back')
  w.pr.reviewDecision = ''

  // The operator changing their mind: the newest marker for the head wins.
  assert.equal((await approve({ pr: 7, repo: REPO }, deps(w))).approved, true)
  assert.equal(
    (await approval({ pr: 7, repo: REPO }, unlabelled())).approval,
    'policy-park-approved',
  )
})

test('approval: an untrusted send-back marker revokes nothing; a parked defect still re-judges', async () => {
  const w = world({
    statuses: parkedHuman('ledger-open'),
    comments: [
      operatorComment(approveMarker(HEAD, ['ledger-open']), { msAgo: 60_000 }),
      operatorComment(sendBackMarker(HEAD), { association: 'NONE', msAgo: 1000 }),
    ],
  })
  const r = await approval({ pr: 7, repo: REPO }, deps(w))
  assert.equal(r.approval, 'policy-park-approved')

  const defect = world({
    statuses: [
      {
        context: 'boss/verify',
        state: 'failure',
        description: 'defect: 2 finding(s), needs human',
        updated_at: iso(1000),
      },
      receipt(),
    ],
    comments: [operatorComment(sendBackMarker(HEAD))],
  })
  const unlabelled = deps(defect, { adapter: fakeAdapter({ labels: [] }) })
  assert.equal((await approval({ pr: 7, repo: REPO }, unlabelled)).reason, 'sent-back')
  defect.comments = []
  assert.equal((await approval({ pr: 7, repo: REPO }, unlabelled)).approval, 'reverify')
})

test('merge refuses a sent-back head, whatever verdict it carries, until a later approve', async () => {
  const approvedStatuses = () => [
    { ...verified()[0], description: 'verified (approved)' },
    ...verified().slice(1),
  ]
  const after = prView({ state: 'MERGED', mergeCommit: { oid: MERGED_SHA } })
  for (const statuses of [approvedStatuses(), verified()]) {
    const w = world({ statuses, comments: [operatorComment(sendBackMarker(HEAD))] })
    const adapter = fakeAdapter()
    const r = await merge({ pr: 7, head: HEAD, repo: REPO }, deps(w, { adapter }))
    assert.deepEqual([r.merged, r.verdict, r.reason], [false, 'wait', 'sent-back'])
    assert.equal(mergeCalls(w).length, 0)
    assert.equal(w.mutating().length, 0)
    assert.deepEqual(adapter.writeCalls(), [])
  }

  const elsewhere = world({
    statuses: approvedStatuses(),
    prAfterMerge: after,
    comments: [operatorComment(sendBackMarker(OTHER))],
  })
  assert.equal((await merge({ pr: 7, head: HEAD, repo: REPO }, deps(elsewhere))).merged, true)

  const reapproved = world({
    statuses: approvedStatuses(),
    prAfterMerge: after,
    comments: [
      operatorComment(sendBackMarker(HEAD), { msAgo: 60_000 }),
      operatorComment(approveMarker(HEAD, ['ledger-open']), { msAgo: 1000 }),
    ],
  })
  assert.equal((await merge({ pr: 7, head: HEAD, repo: REPO }, deps(reapproved))).merged, true)
})

test('an unreadable comment read is unknown: approval none, merge waits, never empty', async () => {
  const blind = world({
    statuses: parkedHuman('ledger-open'),
    pr: { reviewDecision: 'APPROVED' },
    commentsReadFails: true,
  })
  assert.deepEqual(await approval({ pr: 7, repo: REPO }, deps(blind)), {
    approval: 'none',
    reason: 'comments-unreadable',
    headSha: HEAD,
  })
  const m = world({ statuses: verified(), commentsReadFails: true })
  const r = await merge({ pr: 7, head: HEAD, repo: REPO }, deps(m))
  assert.deepEqual([r.merged, r.verdict, r.reason], [false, 'wait', 'comments-unreadable'])
  assert.equal(mergeCalls(m).length, 0)

  // A verified head with no park never needs the read.
  const quiet = world({ statuses: verified(), commentsReadFails: true })
  assert.deepEqual(await approval({ pr: 7, repo: REPO }, deps(quiet)), {
    approval: 'none',
    headSha: HEAD,
  })
})

test('candidates: a park is handed back only by a record for its own head', async () => {
  const nh = (n) => ({ identifier: `BOS-${n}`, labels: ['agent-build'], attachments: [] })
  const w = candidateWorld({
    tickets: [nh(2), nh(3), nh(4), nh(5)],
    statuses: Object.fromEntries(
      [2, 3, 4, 5].map((n) => [SHA_OF(n), verifyStatus('pending', 'needs human: ledger-open')]),
    ),
    reviews: { 5: 'APPROVED' },
    comments: {
      2: [operatorComment(approveMarker(SHA_OF(2), ['ledger-open']))],
      3: [operatorComment(approveMarker(OTHER, ['ledger-open']))],
      5: [operatorComment(sendBackMarker(SHA_OF(5)))],
    },
    commentsFail: [4],
  })
  const r = await candidates({ repo: REPO }, candDeps(w))
  assert.deepEqual(
    r.candidates.map((c) => [c.pr, c.approval, c.waive]),
    [[2, 'policy-park-approved', 'ledger-open']],
  )
  assert.deepEqual(
    r.skipped.map((x) => [x.ticket, x.reason]),
    [
      ['BOS-3', 'parked'],
      ['BOS-4', 'unknown:approval'],
      ['BOS-5', 'parked'],
    ],
  )
})

test('approve: a failed post approves nothing and writes nothing else; a lost response is read back', async () => {
  const failing = (w, { lands = false } = {}) => {
    const read = w.read
    w.read = (command, args, opts) => {
      if (command === 'gh' && args[2] === 'POST') {
        if (lands) read(command, args, opts)
        return { ok: false, error: 'exit-1', exit: 1, output: '' }
      }
      return read(command, args, opts)
    }
    return w
  }
  const adapter = fakeAdapter()
  const r = await approve(
    { pr: 7, repo: REPO },
    deps(failing(world({ statuses: parkedHuman('ledger-open') })), { adapter }),
  )
  assert.deepEqual(
    [r.approved, r.reason, r.approvalUrl],
    [false, 'approval-record-failed', undefined],
  )
  assert.deepEqual(adapter.writeCalls(), [], 'the label is never touched')

  // GitHub took the comment but the response was lost: the read-back finds the record.
  const landed = failing(world({ statuses: parkedHuman('ledger-open') }), { lands: true })
  const l = await approve({ pr: 7, repo: REPO }, deps(landed))
  assert.deepEqual(
    [l.approved, l.approvalUrl, l.trackerWrites],
    [true, `${PR_URL}#issuecomment-99`, 'ok'],
  )
})

test('approve: an unreadable settle is unconfirmed, and a send-back racing the record supersedes it', async () => {
  const w = world({ statuses: parkedHuman('ledger-open') })
  const read = w.read
  w.read = (command, args, opts) => {
    const r = read(command, args, opts)
    if (command === 'gh' && args[2] === 'POST') w.commentsReadFails = true
    return r
  }
  const adapter = fakeAdapter()
  const r = await approve({ pr: 7, repo: REPO }, deps(w, { adapter }))
  assert.deepEqual(
    [r.approved, r.reason, r.approvalUrl],
    [false, 'approval-unconfirmed', `${PR_URL}#issuecomment-99`],
  )
  assert.deepEqual(adapter.writeCalls(), [])

  const raced = world({ statuses: parkedHuman('ledger-open') })
  const inner = raced.read
  raced.read = (command, args, opts) => {
    const out = inner(command, args, opts)
    if (command === 'gh' && args[2] === 'POST' && raced.comments.length === 1)
      raced.comments.push(operatorComment(sendBackMarker(HEAD), { msAgo: -60_000 }))
    return out
  }
  const s = await approve({ pr: 7, repo: REPO }, deps(raced))
  assert.deepEqual([s.approved, s.reason], [false, 'approval-superseded'])
})

test('approve lifts a send-back of a head verify passed outright, and only then', async () => {
  const w = world({ statuses: verified(), comments: [operatorComment(sendBackMarker(HEAD))] })
  const adapter = fakeAdapter()
  const planned = await approve({ pr: 7, repo: REPO, dryRun: true }, deps(w, { adapter }))
  assert.deepEqual([planned.planned, planned.lifted], [true, 'sent-back'])
  assert.deepEqual(
    planned.writes.map((x) => x.kind),
    ['pr-comment'],
  )
  const r = await approve({ pr: 7, repo: REPO }, deps(w, { adapter }))
  assert.deepEqual(
    [r.approved, r.lifted, r.waives, r.approvalUrl],
    [true, 'sent-back', '', `${PR_URL}#issuecomment-99`],
  )
  assert.deepEqual(adapter.writeCalls(), [], 'a lift touches no tracker state')
  const posted = w.mutating()[0].args.find((a) => a.startsWith('body='))
  assert.ok(posted.endsWith(`<!-- boss-approve head=${HEAD} -->`), posted)
  // The re-run verify then judges the head plainly and merges it.
  w.prAfterMerge = prView({ state: 'MERGED', mergeCommit: { oid: MERGED_SHA } })
  assert.equal((await judge({ pr: 7, repo: REPO }, deps(w))).verdict, 'pass')
  assert.equal((await merge({ pr: 7, head: HEAD, repo: REPO }, deps(w))).merged, true)

  // A head that passed under a waiver is re-judged and its re-raised park approved instead.
  const waived = world({
    statuses: [{ ...verified()[0], description: 'verified (approved)' }, ...verified().slice(1)],
    comments: [operatorComment(sendBackMarker(HEAD))],
  })
  const a = await approve({ pr: 7, repo: REPO }, deps(waived))
  assert.deepEqual([a.approved, a.reason], [false, 'sent-back-approved-head'])
  assert.equal(waived.mutating().length, 0)

  // A verified head nobody sent back, or one whose comments cannot be read, has nothing to lift.
  const plain = await approve({ pr: 7, repo: REPO }, deps(world({ statuses: verified() })))
  assert.equal(plain.reason, 'no-standing-park')
  const blind = world({ statuses: verified(), commentsReadFails: true })
  assert.equal((await approve({ pr: 7, repo: REPO }, deps(blind))).reason, 'comments-unreadable')
})

test('approval: only markers by an author with write access count', async () => {
  const run = async (permissions, marker = approveMarker(HEAD, ['ledger-open'])) => {
    const w = world({
      statuses: parkedHuman('ledger-open'),
      comments: [operatorComment(marker, { association: 'MEMBER', login: 'someone' })],
      permissions,
    })
    return approval({ pr: 7, repo: REPO }, deps(w))
  }
  assert.equal((await run({ someone: 'write' })).approval, 'policy-park-approved')
  assert.equal((await run({ someone: 'read' })).approval, 'none', 'a read-only collaborator')
  assert.equal(
    (await run({ someone: 'not-found' })).approval,
    'none',
    'an org member with no access',
  )
  assert.deepEqual(
    [(await run({ someone: null })).approval, (await run({ someone: null })).reason],
    ['none', 'comments-unreadable'],
  )
  // A read-only account's send-back revokes nothing either.
  const w = world({
    statuses: parkedHuman('ledger-open'),
    comments: [
      operatorComment(approveMarker(HEAD, ['ledger-open']), { msAgo: 60_000 }),
      operatorComment(sendBackMarker(HEAD), { login: 'reader', msAgo: 1000 }),
    ],
    permissions: { reader: 'read' },
  })
  assert.equal((await approval({ pr: 7, repo: REPO }, deps(w))).approval, 'policy-park-approved')
})

test('operatorMarkers keeps only well-formed trusted last-line markers; operatorDecision takes the newest', () => {
  const c = (body, extra = {}) => ({
    id: 1,
    body,
    author_association: 'MEMBER',
    user: { login: 'op' },
    created_at: iso(1000),
    html_url: 'https://x/1',
    ...extra,
  })
  const markers = operatorMarkers([
    c(approveMarker(HEAD, ['human-review:ui', 'ledger-open'])),
    c(sendBackMarker(OTHER), { author_association: 'COLLABORATOR' }),
    c(approveMarker(HEAD, [])),
    c(`${approveMarker(HEAD, ['ledger-open'])}\n\nquoted afterwards`),
    c(approveMarker(HEAD, ['ledger-open']), { author_association: 'NONE' }),
    c(approveMarker(HEAD, ['ledger-open']), { user: null }),
    c(approveMarker(HEAD, ['ledger-open', 'human-review:ux'])),
    c(`<!-- boss-approve head=${HEAD} waives= -->`),
    c(`<!-- boss-send-back head=${HEAD} waives=ledger-open -->`),
    c(`<!-- boss-approve head=${HEAD.slice(1)} waives=ledger-open -->`),
    c(null),
    null,
  ])
  assert.deepEqual(
    markers.map((m) => [m.kind, m.head, m.waives, m.login]),
    [
      ['approve', HEAD, 'ledger-open,human-review:ui', 'op'],
      ['send-back', OTHER, undefined, 'op'],
      ['approve', HEAD, '', 'op'],
    ],
  )
  assert.deepEqual(operatorMarkers(null), [])

  const m = (kind, head, at, id, waives) => ({ kind, head, at, id, waives, url: `u${id}` })
  assert.deepEqual(
    operatorDecision([m('approve', HEAD, 1, 1, 'ledger-open')], HEAD.toUpperCase()),
    { decision: 'approved', waives: 'ledger-open', url: 'u1' },
  )
  assert.deepEqual(
    operatorDecision([m('send-back', HEAD, 2, 1), m('approve', HEAD, 1, 9, 'ledger-open')], HEAD),
    { decision: 'sent-back', url: 'u1' },
  )
  assert.deepEqual(
    operatorDecision([m('approve', HEAD, 2, 2, 'ledger-open'), m('send-back', HEAD, 2, 1)], HEAD),
    { decision: 'approved', waives: 'ledger-open', url: 'u2' },
    'the larger id breaks a timestamp tie',
  )
  assert.deepEqual(operatorDecision([m('send-back', OTHER, 9, 9)], HEAD), { decision: null })
  assert.deepEqual(operatorDecision([], ''), { decision: null })
})

test('readOperatorMarkers: a failed or malformed read is unreadable, never an empty list', () => {
  const reader =
    (result, permission = { ok: true, payload: { permission: 'admin' } }) =>
    (command, args) =>
      /\/permission$/.test(args[1]) ? permission : result
  const ok = (payload) => ({ ok: true, payload })
  const good = {
    id: 1,
    body: sendBackMarker(HEAD),
    author_association: 'OWNER',
    user: { login: 'op' },
    created_at: iso(0),
  }
  assert.equal(readOperatorMarkers(reader(ok([[good], []])), REPO, 7).markers.length, 1)
  assert.deepEqual(readOperatorMarkers(reader(ok([[]])), REPO, 7), { readable: true, markers: [] })
  for (const bad of [
    { ok: false, error: 'exit-1' },
    ok(null),
    ok({ message: 'Not Found' }),
    ok([{ id: 1 }]),
    ok([[null]]),
    ok([[{ ...good, created_at: 'garbage' }]]),
  ])
    assert.deepEqual(
      readOperatorMarkers(reader(bad), REPO, 7),
      { readable: false },
      JSON.stringify(bad),
    )
  // A permission read that fails, or answers without a permission, is unknown too.
  for (const permission of [{ ok: false, error: 'exit-1' }, ok({})])
    assert.deepEqual(readOperatorMarkers(reader(ok([[good]]), permission), REPO, 7), {
      readable: false,
    })
})

test('main wires approve and send-back, accepts --json and rejects stray flags', async () => {
  const w = world({ statuses: parkedHuman('ledger-open') })
  const a = await main(['approve', '--pr', '7', '--repo', REPO, '--dry-run', '--json'], deps(w))
  assert.equal(a.planned, true)
  const s = await main(
    ['send-back', '--pr', '7', '--repo', REPO, '--note', 'fix it', '--dry-run', '--json'],
    deps(w),
  )
  assert.equal(s.planned, true)
  for (const argv of [
    ['approve', '--pr', '7', '--note', 'x'],
    ['send-back', '--pr', '7'],
    ['approve'],
  ])
    await assert.rejects(main(argv, deps(world())), UsageError, JSON.stringify(argv))
  assert.equal(w.mutating().length, 0)
})

test('main accepts --flag=value, so a note that looks like a flag reaches send-back verbatim', async () => {
  const w = world()
  const r = await main(
    ['send-back', '--pr=7', `--repo=${REPO}`, '--note=--dry-run is not a flag here', '--json'],
    deps(w),
  )
  assert.equal(r.posted, true)
  const body = w
    .mutating()[0]
    .args.find((a) => a.startsWith('body='))
    .slice('body='.length)
  assert.ok(body.includes('--dry-run is not a flag here'), body)
  await assert.rejects(main(['approve', '--pr=7', '--dry-run=yes'], deps(world())), UsageError)
  await assert.rejects(main(['approve', '--pr=7', '--note=x'], deps(world())), UsageError)
})
