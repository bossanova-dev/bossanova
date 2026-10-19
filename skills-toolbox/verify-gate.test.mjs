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
  MERGE_RETRY_AFTER_MS,
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
  readFailureDetail,
  candidates,
  carryMarker,
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
  markMerged,
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
  parsePostOptions,
  resolveEffectiveVerdict,
  readOperatorMarkers,
  resolvePriorHead,
  rearm,
  rearmArgv,
  selectBaseOwnCheckRuns,
  sendBackMarker,
  verifyMarker,
  standingPark,
  parkStillRaised,
  extensionRunBeforePark,
  policyFingerprint,
  policyMarker,
  remoteSessionMarker,
} from './verify-gate.mjs'
import { resolveApprovalAnchor } from './verify-range.mjs'
import { DEFAULT_CONFIG, mergeConfig, validateConfig } from './skill-config.mjs'
import { DESCRIPTION_LIMIT } from './commit-status.mjs'
import { discoverExtensions, validateVerifyContext } from './skill-extensions.mjs'
import {
  AFTER_MERGE_MARKER,
  PARK_NOTE_MARKER,
  parkCategories,
  parkNoteMarker,
} from './verify-park-note.mjs'

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
  '## Review coverage',
  '',
  'full (3 lenses)',
  '',
].join('\n')

// boss-build's PARTIAL marker, the one explicit hold a PR body carries.
const PARTIAL_BODY = `${BODY}\n## Partial\n\ndo not merge — partial: 2/5 acceptance criteria\n`

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
    priorHead: pick('priorHead', priorPayload([])),
    headRuns: pick('headRuns', [
      {
        workflow_runs: [{ name: 'CI', status: 'completed', conclusion: 'success', head_sha: HEAD }],
      },
    ]),
    // Compare reads `<BASE>...<sha>`: `{[sha]: files[] | payload | null}`; a missing key fails.
    compare: opts.compare ?? {},
    // The config file at the base tip: absent (a 404) by default, `null` for a failed read, a
    // string for its raw contents.
    baseConfig: pick('baseConfig', undefined),
    // A raw read result for the base config, overriding `baseConfig` when set.
    baseConfigRead: opts.baseConfigRead,
    // `null` is an unreadable session list.
    sessions: pick('sessions', [{ id: 'sess-1', pr_url: PR_URL }]),
    show: pick('show', { repo_can_auto_repair: true, last_repair: null }),
    merge: opts.merge ?? { ok: true },
    race: opts.race ?? null,
    // The PR's issue comments; comments POSTed through `…/issues/<n>/comments` are appended (as
    // `poster`, the repo owner by default, stamped after every seeded one), so operator-marker
    // read-backs are real.
    comments: opts.comments ?? [],
    poster: opts.poster ?? { login: 'operator', association: 'OWNER' },
    commentsReadFails: opts.commentsReadFails ?? false,
    // Every comment POST and PATCH is refused, as GitHub refuses one over its size limit.
    commentWritesFail: opts.commentWritesFail ?? false,
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
      if (args[1] === 'graphql') {
        const payload = args.some((a) => a.includes('timelineItems(last: 30'))
          ? w.priorHead
          : w.graphql
        return payload === null ? fail() : ok(payload)
      }
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
          if (w.commentWritesFail) return fail()
          const id = 99 + w.commentPosts++
          const comment = {
            id,
            body: fields.body ?? '',
            author_association: w.poster.association,
            user: { login: w.poster.login },
            created_at: new Date(NOW + w.commentPosts * 1000).toISOString(),
            html_url: `${PR_URL}#issuecomment-${id}`,
          }
          w.comments.push(comment)
          return ok({ html_url: comment.html_url })
        }
        return fail()
      }
      if (args[1] === '--method' && args[2] === 'PATCH') {
        const id = Number(String(args[3]).match(/\/issues\/comments\/(\d+)$/)?.[1])
        const comment = w.comments.find((c) => c.id === id)
        if (!comment || w.commentWritesFail) return fail()
        comment.body = String(args[5]).replace(/^body=/s, '')
        return ok({ html_url: comment.html_url })
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
      if (/actions\/runs\?head_sha=/.test(path) && !path.includes(BASE))
        return w.headRuns === null ? fail() : ok(w.headRuns)
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
  w.graphqlReads = () =>
    w.calls.filter(
      (c) =>
        c.args[0] === 'api' &&
        c.args[1] === 'graphql' &&
        c.args.some((a) => a.includes('reviews(last: 100)')),
    )
  w.compareReads = () =>
    w.calls.filter((c) => c.args[0] === 'api' && String(c.args[1]).includes('/compare/'))
  return w
}

// `tickets` gives the adapter a selectPlanned that answers every query with them (`null` throws).
function fakeAdapter({
  labels = ['needs-human'],
  outcome = 'ok',
  missingKey = false,
  tickets = undefined,
  reason = undefined,
  permanent = undefined,
} = {}) {
  const calls = []
  const queries = []
  return {
    calls,
    queries,
    ...(tickets === undefined
      ? {}
      : {
          selectPlanned: async (query) => {
            queries.push(query)
            if (tickets === null) throw new Error('linear unreachable')
            return tickets
          },
        }),
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
        ...(isWrite && reason !== undefined ? { reason } : {}),
        ...(isWrite && permanent !== undefined ? { permanent } : {}),
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
    'partial-build',
    'extension-failed',
    'no-session',
    'worktree-diverged',
    'worktree-unsynced',
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

test('judge passes a green, receipted, clean head and writes nothing', async () => {
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

test("judge parks boss-build's PARTIAL marker, in the body or the title, as partial", async () => {
  for (const pr of [{ body: PARTIAL_BODY }, { title: '[BOS-1] Some title (partial 2/5)' }]) {
    const r = await judge({ pr: 7, repo: REPO }, deps(world({ pr })))
    assert.deepEqual([r.verdict, r.reason], ['human', 'partial-build'], JSON.stringify(pr))
  }
})

test('judge passes a body with no checklist sections, unchecked boxes or "do not merge" prose', async () => {
  for (const pr of [
    { body: BODY },
    {
      body: `${BODY}\n## Acceptance criteria\n\n- [ ] it works\n\n## Human follow-up\n\n- [ ] check prod\n\n## Open questions\n\n- [ ] which way?\n`,
    },
    { title: 'Do not merge yet', body: `${BODY}\nPlease DO NOT MERGE before Friday.\n` },
    { title: 'Feature (partial scope)', body: BODY },
  ]) {
    const r = await judge({ pr: 7, repo: REPO }, deps(world({ pr })))
    assert.deepEqual([r.verdict, r.reason], ['pass', 'verified'], JSON.stringify(pr))
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
  assert.deepEqual(parseHumanReasonList(['human-review:ui', 'partial-build']), [
    'partial-build',
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
  // A state code (no-receipt, partial) parks before extensions run, so extension-failed never
  // joins one; the widest judged set is every state and path code, plus unproven on top of it.
  const worst = [
    'no-receipt',
    'always-human-path',
    'partial-build',
    'human-review:database',
    'human-review:security',
    'human-review:ui',
    'human-review:unproven',
  ]
  const description = needsHumanDescription(parseHumanReasonList(worst).join(','))
  assert.equal(description.length, 137)
  assert.ok(description.length <= DESCRIPTION_LIMIT)
  // The widest set an extension run can park: a malfunction plus every path code and category.
  const withExtensions = HUMAN_REASONS.filter((c) => !['no-receipt', 'partial-build'].includes(c))
    .filter(
      (c) =>
        !['repair-exhausted', 'no-session', 'worktree-diverged', 'worktree-unsynced'].includes(c),
    )
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
    rmSync(dir, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 })
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
    rmSync(dir, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 })
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
    rmSync(dir, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 })
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
    rmSync(dir, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 })
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
    rmSync(dir, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 })
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
    rmSync(dir, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 })
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
    rmSync(dir, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 })
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
    assert.ok(
      w.comments.some((c) => c.body.split('\n').at(-1) === extensionRecordMarker(HEAD, RECORD)),
    )

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
    rmSync(dir, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 })
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
  const planned = dry.writes.filter((x) => x.kind === 'pr-comment').map((x) => x.body)
  assert.equal(planned.filter((b) => b.endsWith(extensionRecordMarker(HEAD, RECORD))).length, 1)
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
    rmSync(dir, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 })
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
    rmSync(dir, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 })
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
    rmSync(dir, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 })
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
    rmSync(dir, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 })
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

  const two = world({ statuses: [], pr: { body: PARTIAL_BODY } })
  const r2 = await judge(
    { pr: 7, repo: REPO, waive: 'no-receipt' },
    deps(approving(two, 'no-receipt')),
  )
  assert.equal(r2.verdict, 'human')
  assert.equal(r2.reason, 'partial-build', 'only the waived code is lifted')

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
    rmSync(dir, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 })
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

test('post pass lists the After merge items once per head, and they never hold the verdict', async () => {
  const body = `${BODY}\n\n## After merge\n\n- [ ] Observe the nightly run\n`
  const notes = (w) => w.comments.filter((c) => AFTER_MERGE_MARKER.test(c.body.split('\n').at(-1)))
  const pass = (w) => post({ pr: 7, head: HEAD, verdict: 'pass', repo: REPO }, deps(w))
  const verified = (w) => w.statusPosts().map((s) => [s.state, s.description])

  const w = world({ pr: { body } })
  const first = await pass(w)
  assert.deepEqual([first.verdict, first.posted, first.afterMerge], ['pass', true, 'posted'])
  assert.deepEqual(verified(w), [['success', 'verified']])
  assert.equal(notes(w).length, 1)
  assert.ok(notes(w)[0].body.includes('**After merge, check:**\n- Observe the nightly run'))
  const again = await pass(w)
  assert.deepEqual([again.skipped, again.afterMerge], ['already-posted', 'current'])
  assert.equal(w.comments.length, 1, 'the standing note is the only comment')
  w.pr.body = body.replace('nightly run', 'nightly run\n- [ ] Check the dashboard')
  assert.equal((await pass(w)).afterMerge, 'updated')
  assert.equal(notes(w).length, 1)
  assert.ok(notes(w)[0].body.includes('- Check the dashboard'))

  for (const [opts, outcome] of [
    [{ commentWritesFail: true }, 'failed'],
    [{ commentsReadFails: true }, 'unknown'],
  ]) {
    const held = world({ pr: { body }, ...opts })
    const r = await pass(held)
    assert.deepEqual([r.verdict, r.posted, r.afterMerge], ['pass', true, outcome], outcome)
    assert.deepEqual(verified(held), [['success', 'verified']], outcome)
  }

  const plain = world()
  const none = await pass(plain)
  assert.deepEqual([none.posted, 'afterMerge' in none], [true, false])
  assert.equal(
    plain.calls.some((c) => c.args.some((a) => /\/comments/.test(String(a)))),
    false,
    'no After merge items, no comment read',
  )
})

// ---------------------------------------------------------------------------
// post: a verdict that parks nothing clears a needs-human label an older head left.

test('post clears a stale needs-human label only once the head stands on a verdict that parks nothing', async () => {
  const dir = tmp()
  try {
    for (const verdict of ['pass', 'defect']) {
      const w = world()
      const adapter = fakeAdapter()
      const r = await post(
        { pr: 7, head: HEAD, verdict, findings: findingsFile(dir), repo: REPO },
        deps(w, { adapter }),
      )
      assert.deepEqual([r.verdict, r.posted, r.staleLabel], [verdict, true, { outcome: 'ok' }])
      assert.deepEqual(
        adapter.writeCalls(),
        [{ issueId: 'BOS-1', removeLabels: ['needs-human'] }],
        verdict,
      )
    }
    const parked = world()
    const parkAdapter = fakeAdapter()
    const park = await post(
      { pr: 7, head: HEAD, verdict: 'human', reason: 'partial-build', repo: REPO },
      deps(parked, { adapter: parkAdapter }),
    )
    assert.equal(park.posted, true)
    assert.equal(park.staleLabel, undefined)
    assert.deepEqual(
      parkAdapter.writeCalls().map((c) => [c.addLabels, c.removeLabels]),
      [[['needs-human'], undefined]],
    )
  } finally {
    rmSync(dir, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 })
  }
})

test('post keeps the needs-human label when the head moved after its verdict', async () => {
  const w = world()
  const read = w.read
  w.read = (command, args, o) => {
    const r = read(command, args, o)
    if (command === 'gh' && args[1] === '--method' && /\/statuses\//.test(args[3]))
      w.pr = prView({ headRefOid: OTHER })
    return r
  }
  const adapter = fakeAdapter()
  const r = await post({ pr: 7, head: HEAD, verdict: 'pass', repo: REPO }, deps(w, { adapter }))
  assert.deepEqual([r.posted, r.staleLabel], [true, { outcome: 'skipped', why: 'head-moved' }])
  assert.deepEqual(adapter.writeCalls(), [])
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
    const adapter = fakeAdapter({ labels: ['agent-build'] })
    const r = await post(
      { pr: 7, head: HEAD, verdict: 'defect', findings: findingsFile(dir), repo: REPO },
      deps(w, { adapter }),
    )
    assert.equal(r.verdict, 'defect')
    assert.equal(r.staleLabel, undefined)
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
    rmSync(dir, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 })
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
    rmSync(dir, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 })
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
    rmSync(dir, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 })
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
    assert.ok(writes[0].comment.includes(parkCategories([code])[0]), writes[0].comment)
    assert.ok(!writes[0].comment.includes(code), `${code} shown to people: ${writes[0].comment}`)
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
  assert.equal(
    adapter.writeCalls()[0].comment,
    `PR #7 (${PR_URL}) is waiting for a person: security, UI change.`,
  )

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
    // A body that lost its ticket line is not ticketless while a ticket may still stand behind it:
    // its session names one, there is no session row, or the session list cannot be read.
    for (const sessions of [[{ id: 'sess-1', pr_url: PR_URL, tracker_id: 'BOS-1' }], [], null]) {
      const noTicket = world({ pr: { body: BODY.replace(/^Linear issue:.*\n/, '') }, sessions })
      const r = await post(
        { pr: 7, head: HEAD, verdict: 'human', reason: 'no-receipt', repo: REPO },
        deps(noTicket),
      )
      assert.equal(r.trackerWrites, 'unavailable')
      assert.equal(noTicket.statusPosts().length, 0)
    }
  } finally {
    rmSync(dir, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 })
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
      { verdict: 'human', reason: 'partial-build' },
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
    rmSync(dir, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 })
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

test('resolveEffectiveVerdict preserves verdicts and downgrades only exhausted defects', () => {
  for (const [input, expected] of [
    [
      { verdict: 'defect', reason: 'found', repairAttempts: 1, exhaustedAfter: 2 },
      { effective: 'defect', reason: 'found' },
    ],
    [
      { verdict: 'defect', reason: 'found', repairAttempts: 2, exhaustedAfter: 2 },
      { effective: 'human', reason: 'repair-exhausted' },
    ],
    [
      { verdict: 'defect', reason: 'found', repairAttempts: null },
      { effective: 'defect', reason: 'found' },
    ],
    [
      {
        verdict: 'human',
        humanCodes: ['human-review:security', 'human-review:ui'],
        reason: 'ignored',
      },
      { effective: 'human', reason: 'human-review:security,human-review:ui' },
    ],
    [
      { verdict: 'pass', reason: 'approved', repairAttempts: 99 },
      { effective: 'pass', reason: 'approved' },
    ],
    [
      { verdict: 'wait', reason: 'stack-busy' },
      { effective: 'wait', reason: 'stack-busy' },
    ],
  ])
    assert.deepEqual(resolveEffectiveVerdict(input), expected)
})

test('parsePostOptions preserves validation order and every invalid option diagnostic', () => {
  const valid = { verdict: 'pass', head: HEAD }
  for (const [options, message] of [
    [{ verdict: 'invalid', head: 'bad' }, 'post needs --head'],
    [{ verdict: 'invalid' }, 'post --verdict must be'],
    [{ verdict: 'human', reason: 'invalid' }, 'post --verdict human needs --reason'],
    [{ verdict: 'human', reason: HUMAN_REASONS.join(',') }, 'post --reason set is too long'],
    [{ verdict: 'wait' }, 'post --verdict wait needs --reason'],
    [{ extensionRecord: 'bad' }, 'post --extension-record takes'],
    [{ carryRecord: 'bad' }, 'post --carry-record takes'],
    [{ policy: 'bad' }, 'post --policy takes'],
    [{ extensionResults: 'bad' }, 'post --extension-results takes'],
    [
      { verdict: 'defect', findings: '/nonexistent/findings.json' },
      'post --verdict defect needs --findings',
    ],
  ])
    assert.throws(
      () => parsePostOptions({ ...valid, ...options }),
      (error) => error instanceof UsageError && error.message.includes(message),
    )
  assert.deepEqual(parsePostOptions({ ...valid, head: HEAD.toUpperCase(), pr: 7, dryRun: true }), {
    verdict: 'pass',
    headSha: HEAD,
    pr: 7,
    dryRun: true,
    humanCodes: null,
    record: null,
    carryRecord: null,
    extensionResults: undefined,
    findings: [],
  })
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

const elsewhere = [{ id: 'other', pr_url: 'https://github.com/o/r/pull/8' }]
const remoteNotices = (w) =>
  w.comments.filter((c) => c.body.split('\n').at(-1) === remoteSessionMarker(HEAD))

test('merge with no session row says so on the PR once per head and never merges', async () => {
  const w = world({ statuses: verified(), sessions: elsewhere })
  const r = await merge({ pr: 7, head: HEAD, repo: REPO }, deps(w))
  assert.deepEqual(
    [r.merged, r.verdict, r.reason, r.notice],
    [false, 'human', 'no-session', 'posted'],
  )
  const [notice] = remoteNotices(w)
  assert.equal(
    notice.body.split('\n')[0],
    `Verified at \`${HEAD.slice(0, 12)}\`, but this PR's session isn't on this machine, so this machine can't merge it.`,
  )
  assert.equal(w.mutating().length, 1, 'the one notice, and no merge or status write')
  assert.equal(w.calls.filter((c) => c.args[0] === 'pr' && c.args[1] === 'merge').length, 0)

  // The notice stands, so a second merge on the same head posts nothing.
  const again = await merge({ pr: 7, head: HEAD, repo: REPO }, deps(w))
  assert.equal(again.notice, 'standing')
  assert.equal(w.commentPosts, 1)
  assert.equal(remoteNotices(w).length, 1)

  // A notice for an older head does not speak for this one.
  const older = world({
    statuses: verified(),
    sessions: elsewhere,
    comments: [operatorComment(remoteSessionMarker(OTHER))],
  })
  assert.equal((await merge({ pr: 7, head: HEAD, repo: REPO }, deps(older))).notice, 'posted')

  const dry = world({ statuses: verified(), sessions: elsewhere })
  const planned = await merge({ pr: 7, head: HEAD, repo: REPO, dryRun: true }, deps(dry))
  assert.equal(planned.notice, 'planned')
  assert.deepEqual(dry.mutating(), [])

  const refused = world({ statuses: verified(), sessions: elsewhere, commentWritesFail: true })
  assert.equal((await merge({ pr: 7, head: HEAD, repo: REPO }, deps(refused))).notice, 'failed')
})

test('merge with no session row posts no notice on a head that is not ready to merge', async () => {
  const sentBack = world({
    statuses: verified(),
    sessions: elsewhere,
    comments: [operatorComment(sendBackMarker(HEAD))],
  })
  const r1 = await merge({ pr: 7, head: HEAD, repo: REPO }, deps(sentBack))
  assert.deepEqual([r1.verdict, r1.reason], ['wait', 'sent-back'])
  assert.deepEqual(sentBack.mutating(), [])

  const blocked = world({
    statuses: verified(),
    sessions: elsewhere,
    pr: { mergeStateStatus: 'BLOCKED' },
  })
  assert.equal((await merge({ pr: 7, head: HEAD, repo: REPO }, deps(blocked))).verdict, 'wait')
  assert.deepEqual(blocked.mutating(), [])

  const unread = world({ statuses: verified(), sessions: elsewhere, commentsReadFails: true })
  const r3 = await merge({ pr: 7, head: HEAD, repo: REPO }, deps(unread))
  assert.deepEqual([r3.verdict, r3.reason], ['wait', 'comments-unreadable'])
  assert.deepEqual(unread.mutating(), [])
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

test('merge honours an approved partial waiver but still enforces merge state', async () => {
  const openBody = PARTIAL_BODY
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
  assert.ok(r2.reasons.includes('partial-marker'))
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
    park: 'no-receipt',
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
  const park = v('pending', 'needs human: partial-build', 3000)
  assert.deepEqual(standingPark([v('pending', 'waiting: stack-busy', 1000), park, receipt()]), {
    state: 'pending',
    description: 'needs human: partial-build',
  })
  assert.equal(
    standingPark([v('pending', 'verifying… abc', 1000), park]).description,
    'needs human: partial-build',
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
    ...parkedHuman('partial-build'),
  ]
  const comments = () => [operatorComment(approveMarker(HEAD, ['partial-build']))]
  const approved = () =>
    deps(world({ statuses, comments: comments() }), {
      adapter: fakeAdapter({ labels: ['agent-build'] }),
    })
  const r = await approval({ pr: 7, repo: REPO }, approved())
  assert.deepEqual(
    { approval: r.approval, waive: r.waive, via: r.via },
    { approval: 'policy-park-approved', waive: 'partial-build', via: 'operator-approved' },
  )

  // The run re-entering after the wait passes no --waive; it must not re-park on the approved code.
  const partialPr = { body: PARTIAL_BODY }
  const reentered = deps(world({ statuses, pr: partialPr, comments: comments() }), {
    adapter: fakeAdapter({ labels: ['agent-build'] }),
  })
  const j = await judge({ pr: 7, repo: REPO }, reentered)
  assert.notEqual(j.reason, 'partial-build')
  assert.notEqual(j.verdict, 'human')

  // Without the approval record the same re-entry parks again, label or no label.
  const unapproved = deps(world({ statuses, pr: partialPr }), {
    adapter: fakeAdapter({ labels: ['agent-build'] }),
  })
  const u = await judge({ pr: 7, repo: REPO }, unapproved)
  assert.equal(u.verdict, 'human')
  assert.match(u.reason, /partial/)
})

test('approval: a review approval counts, a human merge counts, otherwise none', async () => {
  const approved = world({
    statuses: parkedHuman('partial-build'),
    pr: { reviewDecision: 'APPROVED' },
  })
  const r1 = await approval({ pr: 7, repo: REPO }, deps(approved))
  assert.equal(r1.approval, 'policy-park-approved')
  assert.equal(r1.via, 'review-approved')
  assert.equal(r1.waive, 'partial-build')

  const merged = world({ pr: { state: 'MERGED', mergeCommit: { oid: MERGED_SHA } } })
  assert.equal((await approval({ pr: 7, repo: REPO }, deps(merged))).approval, 'merged-by-human')

  const still = world({ statuses: parkedHuman('partial-build') })
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
  assert.equal(needsHumanDescription('partial-build'), 'needs human: partial-build')
  assert.equal(needsHumanDescription('partial-build', '  '), 'needs human: partial-build')
  assert.equal(
    needsHumanDescription('human-review:security', 'verify gate rules'),
    'needs human: human-review:security · verify gate rules',
  )
  assert.equal(
    needsHumanDescription('partial-build', 'two\nlines'),
    'needs human: partial-build · two lines',
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
      (c) =>
        ![
          'repair-exhausted',
          'no-session',
          'extension-failed',
          'worktree-diverged',
          'worktree-unsynced',
        ].includes(c),
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

test('main post derives the park label from policy and changed paths', async () => {
  const w = world({ diff: ['gate/rules.mjs', 'ci/build.yml'], baseConfig: labelledPolicy() })
  const r = await main(
    [
      'post',
      '--pr',
      '7',
      '--head',
      HEAD,
      '--repo',
      REPO,
      '--verdict',
      'human',
      '--reason',
      'human-review:security',
      '--dry-run',
    ],
    deps(w),
  )
  assert.equal(r.label, 'verify gate rules, CI workflows')
  assert.equal(
    r.writes.find((x) => x.kind === 'status').description,
    'needs human: human-review:security · verify gate rules, CI workflows',
  )
  assert.match(
    r.writes.find((x) => x.kind === 'tracker').comment,
    /verify gate rules, CI workflows/,
  )
  assert.equal(w.mutating().length, 0)
})

test('main post rejects the removed label flag', async () => {
  await assert.rejects(
    main(
      [
        'post',
        '--pr',
        '7',
        '--head',
        HEAD,
        '--verdict',
        'human',
        '--reason',
        'no-receipt',
        '--label',
        'invented',
      ],
      deps(world()),
    ),
    UsageError,
  )
})

const humanPost = (reason, extra = {}) => ({
  pr: 7,
  head: HEAD,
  repo: REPO,
  verdict: 'human',
  reason,
  ...extra,
})

test('post labels only posted codes, deduplicating labels in code order', async () => {
  for (const [reason, label] of [
    ['human-review:ui', 'web UI'],
    ['always-human-path,human-review:security', 'verify gate rules'],
    ['human-review:security,human-review:ui', 'verify gate rules, web UI'],
  ]) {
    const w = world({ diff: ['gate/rules.mjs', 'web/a.tsx'], baseConfig: labelledPolicy() })
    const r = await post(humanPost(reason), deps(w))
    assert.equal(r.label, label)
    assert.equal(w.statusPosts()[0].description, needsHumanDescription(r.reason, label))
    assert.ok(r.writes.find((x) => x.kind === 'tracker').comment.includes(label))
    if (reason === 'human-review:ui')
      assert.ok(!r.writes.find((x) => x.kind === 'tracker').comment.includes('verify gate rules'))
  }
})

test('post avoids label reads for non-path codes and diff reads for unlabelled codes', async () => {
  const w = world({ baseConfig: labelledPolicy() })
  const r = await post(humanPost('no-receipt'), deps(w))
  assert.equal(r.label, '')
  assert.equal(
    w.calls.some((c) =>
      c.args.some((a) => String(a).includes('/contents/') || String(a).includes('/commits/main')),
    ),
    false,
  )
  assert.equal(diffReads(w), 0)
  const unlabelled = world({ baseConfig: policyText({ humanReview: { ui: ['web/**'] } }) })
  assert.equal((await post(humanPost('human-review:ui'), deps(unlabelled))).label, '')
  assert.equal(diffReads(unlabelled), 0)
})

test('post preserves a park with labelUnread when base, policy or diff is unreadable', async () => {
  for (const options of [
    { base: null },
    { baseConfig: null },
    { baseConfig: '{' },
    { diff: null },
  ]) {
    const w = world({ baseConfig: labelledPolicy(), diff: ['web/a.tsx'], ...options })
    const r = await post(humanPost('human-review:ui'), deps(w))
    assert.equal(r.label, '')
    assert.ok(r.labelUnread)
    assert.equal(r.posted, true)
    assert.equal(w.statusPosts()[0].description, 'needs human: human-review:ui')
    assert.ok(r.writes.some((x) => x.kind === 'tracker'))
  }
})

test('post derives the same label as judge from an approved park delta', async () => {
  const w = world({
    baseConfig: labelledPolicy(),
    graphql: approvedAt(ANCHOR),
    diff: ['gate/rules.mjs', 'web/a.tsx'],
    compare: {
      [ANCHOR]: [cfile('gate/rules.mjs', 'old')],
      [HEAD]: [cfile('gate/rules.mjs', 'old'), UI_FILE],
    },
  })
  const j = await judge({ pr: 7, repo: REPO }, deps(w))
  assert.equal(j.reason, 'always-human-path,human-review:ui')
  const r = await post(humanPost(j.reason), deps(w))
  assert.equal(r.label, j.labels.join(', '))
  assert.equal(r.label, 'web UI')
  assert.equal(diffReads(w), 0)
})

test('post keeps repair-exhausted defects unlabelled', async () => {
  const w = world({
    diff: ['gate/rules.mjs'],
    baseConfig: labelledPolicy(),
    show: { repo_can_auto_repair: true, last_repair: { attempt_count: REPAIR_EXHAUSTED_AFTER } },
  })
  const dir = tmp()
  let r
  try {
    r = await post({ ...humanPost(''), verdict: 'defect', findings: findingsFile(dir) }, deps(w))
  } finally {
    rmSync(dir, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 })
  }
  assert.equal(r.reason, 'repair-exhausted')
  assert.equal(r.label, '')
  assert.equal(w.statusPosts()[0].description, 'needs human: repair-exhausted')
})

test('post skips an identical derived park but refreshes a stale label', async () => {
  const w = world({ diff: ['web/a.tsx'], baseConfig: labelledPolicy() })
  assert.equal((await post(humanPost('human-review:ui'), deps(w))).posted, true)
  assert.equal((await post(humanPost('human-review:ui'), deps(w))).skipped, 'already-posted')
  assert.equal(w.statusPosts().length, 1)
  w.baseConfig = policyText({ humanReview: { ui: [{ glob: 'web/**', label: 'new UI label' }] } })
  assert.equal((await post(humanPost('human-review:ui'), deps(w))).posted, true)
  assert.equal(w.statusPosts()[1].description, 'needs human: human-review:ui · new UI label')
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
  const park = v('pending', 'needs human: partial-build · verify gate rules', 3000)
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
    if (command === 'boss' && args[0] === 'ls')
      return opts.sessions === undefined ? fail() : ok({ sessions: opts.sessions })
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
    mergeRetries: [],
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
    mergeRetries: [],
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

test('candidates: a parked needs-human head is excluded unless approval hands it back', async () => {
  const nh = (n) => ({
    identifier: `BOS-${n}`,
    labels: ['agent-build', 'needs-human'],
    attachments: [],
  })
  const w = candidateWorld({
    tickets: [nh(1), nh(2), nh(3), nh(4)],
    statuses: {
      [SHA_OF(2)]: verifyStatus('pending', 'needs human: partial-build'),
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
      { pr: 1, approval: undefined, waive: undefined },
      { pr: 2, approval: 'policy-park-approved', waive: 'partial-build' },
      { pr: 3, approval: 'reverify', waive: undefined },
    ],
  )
  assert.deepEqual(
    r.skipped.map((s) => [s.ticket, s.reason]),
    [['BOS-4', 'parked']],
  )
})

// The newest status first; `at` is ms ago.
const verifyNodes = (...nodes) =>
  nodes.map(([state, description, at]) => ({
    context: 'boss/verify',
    state,
    description,
    updated_at: iso(at),
  }))

test('candidates: needs-human holds only a head its own park stands on', async () => {
  const nh = (n) => ({
    identifier: `BOS-${n}`,
    labels: ['agent-build', 'needs-human'],
    attachments: [],
  })
  const w = candidateWorld({
    tickets: [1, 2, 3, 4, 5, 6].map(nh),
    statuses: {
      // BOS-1: a new head with no verify status. BOS-2: the park sits on an older head only.
      [SHA_OF(102)]: verifyStatus('pending', 'needs human: partial-build'),
      [SHA_OF(3)]: verifyStatus('pending', 'needs human: partial-build'),
      [SHA_OF(4)]: verifyNodes(
        ['pending', 'waiting: stack-busy', 1000],
        ['pending', 'needs human: no-receipt', 5000],
      ),
      [SHA_OF(5)]: verifyNodes(
        ['pending', 'waiting: stack-busy', 1000],
        ['failure', 'defect: 1 finding(s)', 5000],
      ),
      [SHA_OF(6)]: verifyNodes(
        ['pending', 'verifying… t0k3n', 3_600_000],
        ['failure', 'defect: 2 finding(s), needs human', 3_700_000],
      ),
    },
  })
  const r = await candidates({ repo: REPO }, candDeps(w))
  assert.deepEqual(
    r.candidates.map((c) => [c.ticket, c.headSha, c.claim]),
    [
      ['BOS-1', SHA_OF(1), 'none'],
      ['BOS-2', SHA_OF(2), 'none'],
      ['BOS-5', SHA_OF(5), 'waiting'],
    ],
  )
  assert.deepEqual(
    r.skipped.map((s) => [s.ticket, s.reason]),
    [
      ['BOS-3', 'parked'],
      ['BOS-4', 'needs-human'],
      ['BOS-6', 'needs-human'],
    ],
  )
})

test('candidates: a needs-human head whose park beneath a wait cannot be read is unknown', async () => {
  const w = candidateWorld({
    tickets: [{ identifier: 'BOS-1', labels: ['agent-build', 'needs-human'], attachments: [] }],
    statuses: { [SHA_OF(1)]: verifyStatus('pending', 'waiting: stack-busy') },
  })
  const read = w.read
  let statusReads = 0
  w.read = (command, args, o) => {
    if (command === 'gh' && args[0] === 'api' && /\/statuses/.test(args[1]) && ++statusReads > 1)
      return { ok: false, error: 'exit-1', exit: 1, output: '' }
    return read(command, args, o)
  }
  const r = await candidates({ repo: REPO }, candDeps(w))
  assert.deepEqual(r.candidates, [])
  assert.deepEqual(
    r.skipped.map((s) => [s.ticket, s.reason]),
    [['BOS-1', 'unknown:status']],
  )
  assert.equal(statusReads, 2)
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

test('candidates: --limit bounds fresh CI reads while status discovery continues', async () => {
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
    true,
  )
  assert.equal(
    w.calls.some((c) => c.args[0] === 'pr' && c.args[1] === 'view' && c.args[2] === '3'),
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
    rmSync(head, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 })
    rmSync(base, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 })
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
    rmSync(head, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 })
    rmSync(base, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 })
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
    rmSync(repo, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 })
    rmSync(tmpRoot, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 })
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
    rmSync(repo, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 })
    rmSync(tmpRoot, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 })
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
    rmSync(parent, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 })
    const elsewhere = join(tmpRoot, 'planted')
    mkdirSync(elsewhere, { mode: 0o700 })
    symlinkSync(elsewhere, parent)
    assert.deepEqual(materializeTrustedTree({ baseSha: sha, cwd: repo, tmpRoot }), {
      ok: false,
      why: 'untrusted-parent',
    })
  } finally {
    rmSync(repo, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 })
    rmSync(tmpRoot, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 })
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
    rmSync(repo, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 })
    rmSync(tmpRoot, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 })
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
    rmSync(tmpRoot, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 })
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
    rmSync(repo, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 })
    rmSync(tmpRoot, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 })
  }
})

// ---------------------------------------------------------------------------
// approve: the operator verb, and the approval an operator merge records.

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
    approveMarker(HEAD, ['partial-build']),
    `<!-- boss-approve head=${HEAD} waives=partial-build -->`,
  )
})

test('approve uses the configured needs-human label name', async () => {
  const w = world({ statuses: parkedHuman('partial-build') })
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
    ...parkedHuman('partial-build'),
  ]
  const adapter = fakeAdapter()
  const r = await approve({ pr: 7, repo: REPO }, deps(world({ statuses }), { adapter }))
  assert.equal(r.approved, true)
  assert.equal(r.waives, 'partial-build')
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
          ...parkedHuman('partial-build'),
        ],
      }),
      'no-standing-park',
    ],
    [
      'the park stands on an older head',
      (() => {
        const w = world({ statuses: [receipt()] })
        w.statuses[OTHER] = parkedHuman('partial-build')
        return w
      })(),
      'no-standing-park',
    ],
    [
      'the park names a code outside the vocabulary',
      world({ statuses: parkedHuman('partial-build,human-review:ux') }),
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
  const w = world({ statuses: parkedHuman('partial-build') })
  const read = w.read
  let views = 0
  w.read = (command, args, opts) => {
    // approve reads HEAD and its park; the push lands before its pre-post check.
    if (command === 'gh' && args[0] === 'pr' && args[1] === 'view' && ++views === 2) {
      w.pr.headRefOid = OTHER
      w.statuses[OTHER] = parkedHuman('partial-build,human-review:ui')
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
    statuses: parkedHuman('partial-build'),
    pr: { state: 'MERGED', mergeCommit: { oid: MERGED_SHA } },
  })
  assert.equal((await approve({ pr: 7, repo: REPO }, deps(merged))).reason, 'pr-not-open')
  const unreadable = world({ pr: null })
  unreadable.pr = null
  assert.equal((await approve({ pr: 7, repo: REPO }, deps(unreadable))).reason, 'pr-unreadable')
  const blind = world({ statuses: parkedHuman('partial-build'), statusReadFails: true })
  assert.equal((await approve({ pr: 7, repo: REPO }, deps(blind))).reason, 'status-unreadable')
})

test('approve names why the PR read failed, as gh said it, with any token redacted', async () => {
  // delta, PR 6671: a stale GITHUB_TOKEN in the worktree .env made every gh read a 401.
  const w = world({ statuses: parkedHuman('partial-build') })
  const read = (command, args, opts) =>
    command === 'gh' && args[0] === 'pr' && args[1] === 'view'
      ? {
          ok: false,
          error: 'exit-1',
          exit: 1,
          output: '',
          stderr:
            'HTTP 401: Bad credentials (https://api.github.com/graphql)\nTry authenticating with:  gh auth login -h github.com',
        }
      : w.read(command, args, opts)
  const r = await approve({ pr: 7, repo: REPO }, deps(w, { read }))
  assert.deepEqual(
    [r.approved, r.reason, r.detail],
    [false, 'pr-unreadable', 'gh: HTTP 401: Bad credentials (https://api.github.com/graphql)'],
  )
  assert.equal(
    readFailureDetail('gh', { error: 'exit-1', stderr: `bad token ghp_${'a'.repeat(36)} here` }),
    'gh: bad token ghp_REDACTED here',
  )
  assert.equal(readFailureDetail('gh', { error: 'ENOENT', stderr: '' }), 'gh: ENOENT')
})

test('readFailureDetail redacts every GitHub token shape and leaves a commit SHA alone', () => {
  const tok = 'a1B2'.repeat(9)
  const hex = '0123456789abcdef0123456789abcdef01234567'
  for (const [stderr, want] of [
    [`bad github_pat_${'Ab9_'.repeat(20)} here`, 'bad github_pat_REDACTED here'],
    [`MY_ghp_${tok} set`, 'MY_ghp_REDACTED set'],
    [`tokenghs_${tok}`, 'tokenghs_REDACTED'],
    [`ghp_${tok}_x`, 'ghp_REDACTED_x'],
    [`Authorization: token ${hex}`, 'Authorization: token REDACTED'],
    [`Authorization: Bearer ${hex}`, 'Authorization: Bearer REDACTED'],
    [`GET /user?access_token=${hex}&x=1`, 'GET /user?access_token=REDACTED&x=1'],
    [`GITHUB_TOKEN="${hex}"`, 'GITHUB_TOKEN="REDACTED"'],
    [
      `https://x-access-token:${hex}@github.com/o/r`,
      'https://x-access-token:REDACTED@github.com/o/r',
    ],
    [`https://${hex}@github.com/o/r`, 'https://REDACTED@github.com/o/r'],
    [`{"token":"${hex}"}`, '{"token":"REDACTED"}'],
    [`{"access_token": "${hex}"}`, '{"access_token": "REDACTED"}'],
    [`{'token': '${hex}'}`, `{'token': 'REDACTED'}`],
    [`password=${hex}`, 'password=REDACTED'],
    [`{"password":"${hex}"}`, '{"password":"REDACTED"}'],
    [`no commit ${hex} on main`, `no commit ${hex} on main`],
    [`{"sha":"${hex}"}`, `{"sha":"${hex}"}`],
    [`{"head": "${hex}", "base": "${hex}"}`, `{"head": "${hex}", "base": "${hex}"}`],
  ])
    assert.equal(readFailureDetail('gh', { error: 'exit-1', stderr }), `gh: ${want}`, stderr)
})

test("commandReader keeps a failed command's stderr, so a refusal can say why", () => {
  const spawn = () => ({ status: 1, stdout: '', stderr: 'HTTP 401: Bad credentials\n' })
  const r = commandReader({ spawn })('gh', ['pr', 'view', '7', '--json', 'state'], { json: true })
  assert.deepEqual([r.ok, r.error, r.stderr], [false, 'exit-1', 'HTTP 401: Bad credentials'])
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
    park: { statuses: parkedHuman('partial-build') },
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
  const partialPr = { body: PARTIAL_BODY }
  for (const waive of [undefined, 'partial-build']) {
    const w = approving(world({ pr: partialPr }), 'partial-build')
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
  const partialPr = { body: PARTIAL_BODY }
  const w = approving(world({ pr: partialPr }), 'partial-build')
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

test('approve reports the precise reason a head stopped standing', async () => {
  // A PR read that fails after the record settled: the label stays, and the reason says so.
  const w = world({ statuses: parkedHuman('partial-build') })
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
  const closing = world({ statuses: parkedHuman('partial-build') })
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
    const blind = world({ statuses: parkedHuman('partial-build') })
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
  const w = world({ statuses: parkedHuman('partial-build') })
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
  const w = world({ statuses: parkedHuman('partial-build') })
  const probe = fakeAdapter()
  const adapter = {
    ...probe,
    applyIssueWrites: async (writes) => {
      // The record has settled on HEAD; a push lands while the tracker is probed.
      if (!('removeLabels' in writes)) {
        w.pr.headRefOid = OTHER
        w.statuses[OTHER] = parkedHuman('partial-build')
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
  const moved = world({ statuses: parkedHuman('partial-build') })
  const read = moved.read
  moved.read = (command, args, opts) => {
    const out = read(command, args, opts)
    if (command === 'gh' && args[2] === 'POST') moved.pr.headRefOid = OTHER
    return out
  }
  const m = await approve({ pr: 7, repo: REPO }, deps(moved))
  assert.deepEqual([m.reason, m.writes.map((x) => x.kind)], ['head-moved', ['pr-comment']])
  const bare = world({ statuses: parkedHuman('partial-build'), pr: { body: 'no ticket line' } })
  const b = await approve({ pr: 7, repo: REPO }, deps(bare))
  assert.deepEqual(
    [b.approved, b.trackerWrites, b.writes.map((x) => x.kind)],
    [true, 'unavailable', ['pr-comment']],
  )
})

test('approve --dry-run plans the record and the removal and writes nothing', async () => {
  const w = world({ statuses: parkedHuman('partial-build') })
  const adapter = fakeAdapter()
  const r = await approve({ pr: 7, repo: REPO, dryRun: true }, deps(w, { adapter }))
  assert.equal(r.dryRun, true)
  assert.equal(r.approved, false)
  assert.equal(r.planned, true)
  assert.equal(r.waives, 'partial-build')
  assert.deepEqual(
    r.writes.map((x) => x.kind),
    ['pr-comment', 'tracker'],
  )
  assert.ok(r.writes[0].body.endsWith(approveMarker(HEAD, ['partial-build'])))
  assert.deepEqual(r.writes[1], {
    kind: 'tracker',
    issueId: 'BOS-1',
    removeLabels: ['needs-human'],
  })
  assert.deepEqual(adapter.writeCalls(), [])
  assert.equal(w.mutating().length, 0)
})

test('approve through the waiver path: the next verify run waives exactly the parked codes', async () => {
  const w = world({ statuses: parkedHuman('partial-build') })
  const r = await approve({ pr: 7, repo: REPO }, deps(w))
  assert.equal(r.approved, true)
  // The record is on the PR; approval (what the verify run reads) waives exactly that set, whatever
  // the tracker label says.
  const after = await approval({ pr: 7, repo: REPO }, deps(w))
  assert.equal(after.approval, 'policy-park-approved')
  assert.equal(after.via, 'operator-approved')
  assert.equal(after.waive, r.waives)
})

// The bodies of the PR comments a world was sent.
const commentBodies = (w) =>
  w
    .mutating()
    .filter((c) => c.command === 'gh' && /\/issues\/\d+\/comments$/.test(c.args[3] ?? ''))
    .map((c) => c.args.find((a) => a.startsWith('body=')).slice('body='.length))

test('approve --head refuses a head other than the one pinned, before any read past the PR or any write', async () => {
  for (const dryRun of [false, true]) {
    const w = world({ statuses: parkedHuman('partial-build') })
    const adapter = fakeAdapter()
    const r = await approve({ pr: 7, repo: REPO, head: OTHER, dryRun }, deps(w, { adapter }))
    assert.deepEqual(
      [r.approved, r.reason, r.headSha, r.planned],
      [false, 'head-moved', HEAD, undefined],
      `dryRun=${dryRun}`,
    )
    assert.deepEqual(r.writes, [])
    assert.deepEqual(
      w.calls.map((c) => c.args.slice(0, 2)),
      [['pr', 'view']],
      'only the PR is read',
    )
    assert.equal(w.mutating().length, 0, 'no comment POST')
    assert.deepEqual(adapter.calls, [], 'no tracker probe or write')
  }
  // The pinned head is compared without regard to case.
  const w = world({ statuses: parkedHuman('partial-build') })
  const r = await approve({ pr: 7, repo: REPO, head: HEAD.toUpperCase() }, deps(w))
  assert.deepEqual([r.approved, r.waives], [true, 'partial-build'])
})

test('approve --waives refuses unless exactly the pinned codes stand, before any write', async () => {
  const cases = [
    ['another park stands', parkedHuman('partial-build,human-review:ui'), 'partial-build'],
    ['a park stands where none was pinned', parkedHuman('partial-build'), ''],
    ['the pinned park is gone', verified(), 'partial-build'],
    [
      'the park names a code outside the vocabulary',
      parkedHuman('human-review:ux'),
      'partial-build',
    ],
  ]
  for (const [name, statuses, waives] of cases)
    for (const forMerge of [false, true]) {
      const w = world({ statuses })
      const adapter = fakeAdapter()
      const r = await approve(
        { pr: 7, repo: REPO, head: HEAD, waives, forMerge },
        deps(w, { adapter }),
      )
      const park = statuses[0].state === 'pending' ? statuses[0].description : ''
      assert.deepEqual(
        r,
        { approved: false, reason: 'park-changed', waives, park, headSha: HEAD, writes: [] },
        `${name} forMerge=${forMerge}`,
      )
      assert.equal(w.mutating().length, 0, name)
      assert.deepEqual(adapter.calls, [], name)
    }
  // The pinned codes standing: a plain approval of the park, label removal included.
  const w = world({ statuses: parkedHuman('partial-build') })
  const adapter = fakeAdapter()
  const r = await approve(
    { pr: 7, repo: REPO, head: HEAD, waives: 'partial-build', forMerge: true },
    deps(w, { adapter }),
  )
  assert.deepEqual(
    [r.approved, r.waives, r.trackerWrites, r.signOff, r.lifted],
    [true, 'partial-build', 'ok', undefined, undefined],
  )
  assert.deepEqual(trackerRemovals(adapter), [{ issueId: 'BOS-1', removeLabels: ['needs-human'] }])
})

test('approve --for-merge on an unparked head records an approval that waives nothing', async () => {
  for (const statuses of [verified(), [receipt()]]) {
    const w = world({ statuses })
    const adapter = fakeAdapter()
    const pins = { pr: 7, repo: REPO, head: HEAD, waives: '', forMerge: true }
    const planned = await approve({ ...pins, dryRun: true }, deps(w, { adapter }))
    assert.deepEqual(
      [planned.dryRun, planned.approved, planned.planned, planned.waives, planned.signOff],
      [true, false, true, '', 'merge'],
    )
    assert.deepEqual(
      planned.writes.map((x) => x.kind),
      ['pr-comment'],
    )
    assert.equal(w.mutating().length, 0, 'a dry run posts nothing')

    const r = await approve(pins, deps(w, { adapter }))
    assert.deepEqual(r, {
      approved: true,
      waives: '',
      headSha: HEAD,
      approvalUrl: `${PR_URL}#issuecomment-99`,
      signOff: 'merge',
      writes: [planned.writes[0]],
    })
    assert.deepEqual(adapter.writeCalls(), [], 'no tracker label write')
    assert.deepEqual(commentBodies(w), [
      [
        '**Approved by the operator**',
        `Signed off for a merge of \`${HEAD}\`; nothing was waiting, so it waives nothing.`,
        approveMarker(HEAD, []),
      ].join('\n\n'),
    ])
    const decision = operatorDecision(operatorMarkers(w.comments), HEAD)
    assert.deepEqual([decision.decision, decision.waives], ['approved', ''])
  }
  // Without --for-merge an unparked head is still refused.
  const plain = await approve(
    { pr: 7, repo: REPO, head: HEAD, waives: '' },
    deps(world({ statuses: verified() })),
  )
  assert.equal(plain.reason, 'no-standing-park')
})

test('approve --for-merge refuses a non-writer, and a park raised before the post, posting nothing', async () => {
  const pins = { pr: 7, repo: REPO, head: HEAD, waives: '', forMerge: true }
  for (const dryRun of [false, true]) {
    const w = world({ statuses: verified(), permissions: { operator: 'read' } })
    const r = await approve({ ...pins, dryRun }, deps(w))
    assert.deepEqual([r.approved, r.reason, r.planned], [false, 'approver-not-writer', undefined])
    assert.equal(w.mutating().length, 0)
  }
  const w = world({ statuses: verified() })
  const read = w.read
  let views = 0
  w.read = (command, args, opts) => {
    // A park lands on the head between approve's read and its pre-post check.
    if (command === 'gh' && args[0] === 'pr' && args[1] === 'view' && ++views === 2)
      w.statuses[HEAD] = parkedHuman('partial-build')
    return read(command, args, opts)
  }
  const r = await approve(pins, deps(w))
  assert.deepEqual([r.approved, r.reason], [false, 'head-moved'])
  assert.equal(w.mutating().length, 0)
})

test('approve --for-merge needs both pins, and --head a full sha', async () => {
  const w = world({ statuses: verified() })
  for (const options of [
    { forMerge: true },
    { forMerge: true, head: HEAD },
    { forMerge: true, waives: '' },
    { head: 'abc123' },
    { head: '' },
  ])
    await assert.rejects(
      approve({ pr: 7, repo: REPO, ...options }, deps(w)),
      UsageError,
      JSON.stringify(options),
    )
  assert.deepEqual(w.calls, [], 'a usage error reads nothing')
})

// ---------------------------------------------------------------------------
// mark-merged: the tracker bookkeeping after an operator merge.

const mergedPr = { state: 'MERGED', mergeCommit: { oid: MERGED_SHA } }

test('mark-merged makes the same tracker write merge makes, without merging or posting', async () => {
  const viaMerge = fakeAdapter()
  const m = await merge(
    { pr: 7, head: HEAD, repo: REPO },
    deps(world({ statuses: verified(), prAfterMerge: prView(mergedPr) }), { adapter: viaMerge }),
  )
  assert.equal(m.trackerWrites, 'ok')
  const w = world({ pr: mergedPr })
  const adapter = fakeAdapter()
  const r = await markMerged({ pr: 7, repo: REPO }, deps(w, { adapter }))
  assert.deepEqual(r, {
    merged: true,
    mergeSha: MERGED_SHA,
    trackerWrites: 'ok',
    writes: m.writes.filter((x) => x.kind === 'tracker'),
  })
  assert.deepEqual(adapter.writeCalls(), [
    { issueId: 'BOS-1', stateName: 'Done', removeLabels: ['needs-human'] },
  ])
  assert.deepEqual(adapter.writeCalls(), viaMerge.writeCalls())
  assert.equal(w.mutating().length, 0, 'it never merges, comments or posts a status')
})

test('mark-merged reports a ticketless PR, an unwritable tracker and a dry run', async () => {
  const base = ticketlessWorld()
  const ticketless = ticketlessWorld({ pr: { ...base.pr, ...mergedPr } })
  const none = fakeAdapter({ tickets: [] })
  const t = await markMerged({ pr: 7, repo: REPO }, deps(ticketless, { adapter: none }))
  assert.deepEqual([t.merged, t.trackerWrites, t.why, t.writes], [true, 'none', 'no-ticket', []])
  assert.deepEqual(none.calls, [])

  const locked = fakeAdapter({ missingKey: true })
  const u = await markMerged(
    { pr: 7, repo: REPO },
    deps(world({ pr: mergedPr }), { adapter: locked }),
  )
  assert.deepEqual(
    [u.merged, u.trackerWrites, u.why],
    [true, 'unavailable', 'TRACKER_CREDENTIALS_MISSING'],
  )
  assert.deepEqual(locked.writeCalls(), [])

  const adapter = fakeAdapter()
  const w = world({ pr: mergedPr })
  const d = await markMerged({ pr: 7, repo: REPO, dryRun: true }, deps(w, { adapter }))
  assert.deepEqual(d, {
    dryRun: true,
    merged: true,
    mergeSha: MERGED_SHA,
    writes: [
      { kind: 'tracker', issueId: 'BOS-1', stateName: 'Done', removeLabels: ['needs-human'] },
    ],
  })
  assert.deepEqual(adapter.writeCalls(), [], 'a dry run applies nothing')
  assert.equal(w.mutating().length, 0)
})

test('mark-merged reports a write the tracker refuses for good as permanent, with the reason', async () => {
  const cases = [
    [{ outcome: 'failed', reason: 'state-not-found: Done', permanent: true }, 'permanent'],
    [{ outcome: 'failed', reason: 'issue-read-failed: socket hang up' }, 'failed'],
    [{ outcome: 'indeterminate', reason: 'removeLabel: socket hang up' }, 'indeterminate'],
  ]
  for (const [answer, trackerWrites] of cases) {
    const adapter = fakeAdapter(answer)
    const r = await markMerged({ pr: 7, repo: REPO }, deps(world({ pr: mergedPr }), { adapter }))
    assert.deepEqual(
      [r.merged, r.trackerWrites, r.why, r.writes[0].issueId],
      [true, trackerWrites, answer.reason, 'BOS-1'],
      JSON.stringify(answer),
    )
  }
})

test('mark-merged re-reads a PR not yet merged once, then reports it not merged or unreadable', async () => {
  const views = (w) => w.calls.filter((c) => c.args[0] === 'pr' && c.args[1] === 'view').length
  const lag = world()
  const read = lag.read
  lag.read = (command, args, opts) => {
    // GitHub shows the merge only on the second read.
    if (command === 'gh' && args[0] === 'pr' && args[1] === 'view' && views(lag) === 1)
      lag.pr = prView(mergedPr)
    return read(command, args, opts)
  }
  const l = await markMerged({ pr: 7, repo: REPO }, deps(lag))
  assert.deepEqual([l.merged, l.mergeSha, views(lag)], [true, MERGED_SHA, 2])

  const open = world()
  const adapter = fakeAdapter()
  const o = await markMerged({ pr: 7, repo: REPO }, deps(open, { adapter }))
  assert.deepEqual(o, { merged: false, reason: 'not-merged', state: 'OPEN', writes: [] })
  assert.equal(views(open), 2)
  assert.deepEqual(adapter.calls, [])

  const noCommit = world({ pr: { state: 'MERGED', mergeCommit: null } })
  assert.equal((await markMerged({ pr: 7, repo: REPO }, deps(noCommit))).reason, 'not-merged')

  const blind = world()
  blind.pr = null
  assert.deepEqual(await markMerged({ pr: 7, repo: REPO }, deps(blind)), {
    merged: false,
    reason: 'pr-unreadable',
    writes: [],
  })
})

// ---------------------------------------------------------------------------
// Head-bound operator decisions (BOS-1611): an approval waives only the head it names, and a
// legacy send-back marker on that head revokes it.

// A legacy send-back record on `head`, posted through the world so it is stamped after every
// comment already there (older toolboxes wrote these; open PRs may still carry one).
const legacySendBack = (w, head) =>
  w.read('gh', [
    'api',
    '--method',
    'POST',
    'repos/o/r/issues/7/comments',
    '-f',
    `body=**Sent back by the operator**

not like this

${sendBackMarker(head)}`,
  ])

const mergeCalls = (w) => w.calls.filter((c) => c.command === 'boss' && c.args[0] === 'merge')

test('race: a head pushed and parked while approve writes its record is never waived', async () => {
  const w = world({ statuses: parkedHuman('partial-build') })
  const read = w.read
  // The push lands after approve's last pre-write check, while the record POST is in flight.
  w.read = (command, args, opts) => {
    const r = read(command, args, opts)
    if (command === 'gh' && args[2] === 'POST' && /\/comments$/.test(args[3])) {
      w.pr.headRefOid = OTHER
      w.statuses[OTHER] = parkedHuman('partial-build')
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
  const partialPr = { body: PARTIAL_BODY }
  for (const waive of [undefined, 'partial-build']) {
    const w = approving(world({ pr: partialPr }), 'partial-build')
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
      ['human', 'partial-build', OTHER],
      String(waive),
    )
  }
})

test('judge: a waiver handed over for one head is dropped when another head is judged', async () => {
  const partialPr = { body: PARTIAL_BODY }
  const w = approving(world({ pr: partialPr }), 'partial-build')
  // The router read HEAD's approval; a push lands before the judge runs.
  w.pr.headRefOid = OTHER
  w.statuses[OTHER] = [receipt()]
  const j = await judge({ pr: 7, repo: REPO, waive: 'partial-build' }, deps(w))
  assert.deepEqual([j.verdict, j.reason], ['human', 'partial-build'])

  // On the approved head the same waiver stands; a waiver wider than the approval does not.
  const ok = approving(world({ pr: partialPr }), 'partial-build')
  const k = await judge({ pr: 7, repo: REPO, waive: 'partial-build' }, deps(ok))
  assert.deepEqual([k.verdict, k.reason], ['pass', 'approved'])
  const wide = approving(world({ pr: partialPr }), 'partial-build')
  const x = await judge({ pr: 7, repo: REPO, waive: 'partial-build,no-receipt' }, deps(wide))
  assert.equal(x.verdict, 'human')
})

test('approval: a record binds to its head, the exact parked codes and a trusted author', async () => {
  const run = async (comments, statuses = parkedHuman('partial-build')) => {
    const w = world({ statuses, comments })
    const r = await approval(
      { pr: 7, repo: REPO },
      deps(w, { adapter: fakeAdapter({ labels: [] }) }),
    )
    return r.approval
  }
  assert.equal(
    await run([operatorComment(approveMarker(HEAD, ['partial-build']))]),
    'policy-park-approved',
  )
  assert.equal(await run([operatorComment(approveMarker(OTHER, ['partial-build']))]), 'none')
  assert.equal(
    await run([operatorComment(approveMarker(HEAD, ['partial-build', 'human-review:ui']))]),
    'none',
  )
  assert.equal(
    await run([operatorComment(approveMarker(HEAD, ['partial-build']), { association: 'NONE' })]),
    'none',
  )
  assert.equal(
    await run([
      operatorComment(approveMarker(HEAD, ['partial-build']), { association: 'CONTRIBUTOR' }),
    ]),
    'none',
  )
})

test('race: a legacy send-back after an approval revokes it until a later approve of the same head', async () => {
  const partialPr = { body: PARTIAL_BODY }
  const w = world({ statuses: parkedHuman('partial-build'), pr: partialPr })
  const unlabelled = () => deps(w, { adapter: fakeAdapter({ labels: ['agent-build'] }) })
  assert.equal((await approve({ pr: 7, repo: REPO }, deps(w))).approved, true)
  assert.equal(
    (await approval({ pr: 7, repo: REPO }, unlabelled())).approval,
    'policy-park-approved',
  )

  assert.equal(legacySendBack(w, HEAD).ok, true)
  assert.deepEqual(await approval({ pr: 7, repo: REPO }, unlabelled()), {
    approval: 'none',
    reason: 'sent-back',
    headSha: HEAD,
    park: 'partial-build',
  })
  // The verify run re-entering with no --waive re-parks the rejected head.
  const j = await judge({ pr: 7, repo: REPO }, unlabelled())
  assert.equal(j.verdict, 'human')
  assert.match(j.reason, /partial/)
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
    statuses: parkedHuman('partial-build'),
    comments: [
      operatorComment(approveMarker(HEAD, ['partial-build']), { msAgo: 60_000 }),
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
      operatorComment(approveMarker(HEAD, ['partial-build']), { msAgo: 1000 }),
    ],
  })
  assert.equal((await merge({ pr: 7, head: HEAD, repo: REPO }, deps(reapproved))).merged, true)
})

test('an unreadable comment read is unknown: approval none, merge waits, never empty', async () => {
  const blind = world({
    statuses: parkedHuman('partial-build'),
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
      [2, 3, 4, 5].map((n) => [SHA_OF(n), verifyStatus('pending', 'needs human: partial-build')]),
    ),
    reviews: { 5: 'APPROVED' },
    comments: {
      2: [operatorComment(approveMarker(SHA_OF(2), ['partial-build']))],
      3: [operatorComment(approveMarker(OTHER, ['partial-build']))],
      5: [operatorComment(sendBackMarker(SHA_OF(5)))],
    },
    commentsFail: [4],
  })
  const r = await candidates({ repo: REPO }, candDeps(w))
  assert.deepEqual(
    r.candidates.map((c) => [c.pr, c.approval, c.waive]),
    [[2, 'policy-park-approved', 'partial-build']],
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
    deps(failing(world({ statuses: parkedHuman('partial-build') })), { adapter }),
  )
  assert.deepEqual(
    [r.approved, r.reason, r.approvalUrl],
    [false, 'approval-record-failed', undefined],
  )
  assert.deepEqual(adapter.writeCalls(), [], 'the label is never touched')

  // GitHub took the comment but the response was lost: the read-back finds the record.
  const landed = failing(world({ statuses: parkedHuman('partial-build') }), { lands: true })
  const l = await approve({ pr: 7, repo: REPO }, deps(landed))
  assert.deepEqual(
    [l.approved, l.approvalUrl, l.trackerWrites],
    [true, `${PR_URL}#issuecomment-99`, 'ok'],
  )
})

test('approve: an unreadable settle is unconfirmed, and a send-back racing the record supersedes it', async () => {
  const w = world({ statuses: parkedHuman('partial-build') })
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

  const raced = world({ statuses: parkedHuman('partial-build') })
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
  const run = async (permissions, marker = approveMarker(HEAD, ['partial-build'])) => {
    const w = world({
      statuses: parkedHuman('partial-build'),
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
    statuses: parkedHuman('partial-build'),
    comments: [
      operatorComment(approveMarker(HEAD, ['partial-build']), { msAgo: 60_000 }),
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
    c(approveMarker(HEAD, ['human-review:ui', 'partial-build'])),
    c(sendBackMarker(OTHER), { author_association: 'COLLABORATOR' }),
    c(approveMarker(HEAD, [])),
    c(`${approveMarker(HEAD, ['partial-build'])}\n\nquoted afterwards`),
    c(approveMarker(HEAD, ['partial-build']), { author_association: 'NONE' }),
    c(approveMarker(HEAD, ['partial-build']), { user: null }),
    c(approveMarker(HEAD, ['partial-build', 'human-review:ux'])),
    c(`<!-- boss-approve head=${HEAD} waives= -->`),
    c(`<!-- boss-send-back head=${HEAD} waives=partial-build -->`),
    c(`<!-- boss-approve head=${HEAD.slice(1)} waives=partial-build -->`),
    c(null),
    null,
  ])
  assert.deepEqual(
    markers.map((m) => [m.kind, m.head, m.waives, m.login]),
    [
      ['approve', HEAD, 'partial-build,human-review:ui', 'op'],
      ['send-back', OTHER, undefined, 'op'],
      ['approve', HEAD, '', 'op'],
    ],
  )
  assert.deepEqual(operatorMarkers(null), [])

  const m = (kind, head, at, id, waives) => ({ kind, head, at, id, waives, url: `u${id}` })
  assert.deepEqual(
    operatorDecision([m('approve', HEAD, 1, 1, 'partial-build')], HEAD.toUpperCase()),
    { decision: 'approved', waives: 'partial-build', url: 'u1' },
  )
  assert.deepEqual(
    operatorDecision([m('send-back', HEAD, 2, 1), m('approve', HEAD, 1, 9, 'partial-build')], HEAD),
    { decision: 'sent-back', url: 'u1' },
  )
  assert.deepEqual(
    operatorDecision([m('approve', HEAD, 2, 2, 'partial-build'), m('send-back', HEAD, 2, 1)], HEAD),
    { decision: 'approved', waives: 'partial-build', url: 'u2' },
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

test('main wires approve and mark-merged, accepts --json and rejects stray flags', async () => {
  const w = world({ statuses: parkedHuman('partial-build') })
  const a = await main(['approve', '--pr', '7', '--repo', REPO, '--dry-run', '--json'], deps(w))
  assert.equal(a.planned, true)
  const merged = world({ pr: mergedPr })
  const m = await main(
    ['mark-merged', '--pr', '7', '--repo', REPO, '--dry-run', '--json'],
    deps(merged),
  )
  assert.deepEqual([m.dryRun, m.merged, m.mergeSha], [true, true, MERGED_SHA])
  for (const argv of [
    ['approve', '--pr', '7', '--note', 'x'],
    ['send-back', '--pr', '7', '--note', 'x'],
    ['mark-merged', '--pr', '7', '--head', HEAD],
    ['approve', '--pr', '7', '--for-merge', '--head', HEAD],
    ['approve', '--pr', '7', '--for-merge=yes', '--head', HEAD, '--waives='],
    ['approve'],
  ])
    await assert.rejects(main(argv, deps(world())), UsageError, JSON.stringify(argv))
  assert.equal(w.mutating().length + merged.mutating().length, 0)
})

test('main passes the approve pins through, an empty --waives= included', async () => {
  const w = world({ statuses: verified() })
  const r = await main(
    ['approve', '--pr=7', `--repo=${REPO}`, `--head=${HEAD}`, '--waives=', '--for-merge', '--json'],
    deps(w),
  )
  assert.deepEqual([r.approved, r.waives, r.signOff], [true, '', 'merge'])
  const moved = await main(
    ['approve', '--pr', '7', '--repo', REPO, '--head', OTHER, '--waives', 'partial-build'],
    deps(world({ statuses: parkedHuman('partial-build') })),
  )
  assert.equal(moved.reason, 'head-moved')
  await assert.rejects(main(['approve', '--pr=7', '--dry-run=yes'], deps(world())), UsageError)
})

// ---------------------------------------------------------------------------
// Stale parks: a park the base's current verify policy would no longer raise is re-judged.

const OLD_POLICY = 'f'.repeat(16)
const UI_POLICY = policyText({ humanReview: { ui: ['web/**'] } })
const parkStatus = (description, msAgo = 1000) => ({
  context: 'boss/verify',
  state: 'pending',
  description,
  updated_at: iso(msAgo),
})
const claimStatus = (msAgo) => parkStatus('verifying… t0k3n', msAgo)

// The single-PR world, also answering the candidates read for its one PR on ticket BOS-1.
function sweepWorld(opts = {}) {
  const w = world(opts)
  const read = w.read
  w.read = (command, args, o) => {
    if (command === 'gh' && args[0] === 'pr' && args[1] === 'list') {
      w.calls.push({ command, args: [...args] })
      const { number, url, body, headRefOid, isDraft, baseRefName } = w.pr
      const pr = { number, url, body, headRefOid, isDraft, baseRefName }
      return { ok: true, payload: [pr], output: JSON.stringify([pr]), exit: 0 }
    }
    return read(command, args, o)
  }
  return w
}
function sweepDeps(w, { discover = () => ({ extensions: [] }), adapter = fakeAdapter() } = {}) {
  const ticket = { identifier: 'BOS-1', labels: ['agent-build', 'needs-human'], attachments: [] }
  return deps(w, {
    discover,
    loadConfig: () => candidateConfig(),
    adapter: { ...adapter, selectPlanned: async () => [ticket] },
  })
}
const judged = (w, d) => judge({ pr: 7, repo: REPO }, d)
const parkPost = (j, d) =>
  post(
    {
      pr: 7,
      head: j.headSha,
      verdict: 'human',
      reason: j.reason,
      policy: j.policyFingerprint,
      repo: REPO,
    },
    d,
  )
const policyComments = (w) =>
  w.comments.filter((c) => c.body.trim().split('\n').at(-1).startsWith('<!-- boss-policy-record'))

test('a park raised under a path list that is now empty is re-judged, passes and merges', async () => {
  const w = sweepWorld({
    statuses: [parkStatus('needs human: human-review:security'), receipt()],
    baseConfig: policyText({ humanReview: { security: [] } }),
    diff: ['src/auth/login.go'],
    comments: [operatorComment(policyMarker(HEAD, OLD_POLICY))],
    prAfterMerge: prView({ state: 'MERGED', mergeCommit: { oid: MERGED_SHA } }),
  })
  const adapter = fakeAdapter()
  const d = sweepDeps(w, { adapter })
  const found = await candidates({ repo: REPO }, d)
  assert.deepEqual(
    found.candidates.map((c) => [c.pr, c.rejudge, c.approval]),
    [[7, 'policy-changed', undefined]],
  )
  const j = await judged(w, d)
  assert.deepEqual([j.verdict, j.reason, j.held], ['pass', 'verified', undefined])
  const passed = await post({ pr: 7, head: j.headSha, verdict: 'pass', repo: REPO }, d)
  assert.equal(passed.posted, true)
  const merged = await merge({ pr: 7, head: j.headSha, repo: REPO }, d)
  assert.deepEqual([merged.merged, merged.mergeSha], [true, MERGED_SHA])
  assert.deepEqual(w.statusPosts(), [
    { state: 'success', description: 'verified', target: undefined },
  ])
  // The pass clears the label the superseded park left; the merge's Done write repeats it.
  assert.deepEqual(
    adapter.writeCalls().map((c) => c.removeLabels),
    [['needs-human'], ['needs-human']],
  )
})

test('a new head under a needs-human label an older head left is judged, and its pass clears the label', async () => {
  const w = sweepWorld({ statuses: [receipt()] })
  const adapter = fakeAdapter()
  const d = sweepDeps(w, { adapter })
  const found = await candidates({ repo: REPO }, d)
  assert.deepEqual(
    found.candidates.map((c) => [c.pr, c.headSha, c.claim]),
    [[7, HEAD, 'none']],
  )
  const j = await judged(w, d)
  assert.deepEqual([j.verdict, j.reason], ['pass', 'verified'])
  const passed = await post({ pr: 7, head: j.headSha, verdict: 'pass', repo: REPO }, d)
  assert.deepEqual([passed.posted, passed.staleLabel], [true, { outcome: 'ok' }])
  assert.deepEqual(adapter.writeCalls(), [{ issueId: 'BOS-1', removeLabels: ['needs-human'] }])
})

test('a park whose codes still stand under the recorded policy is never re-judged and writes nothing', async () => {
  const fingerprint = policyFingerprint(JSON.parse(UI_POLICY).verifyDefaults, [], { root: TRUSTED })
  const w = sweepWorld({
    statuses: [parkStatus('needs human: human-review:ui'), receipt()],
    baseConfig: UI_POLICY,
    diff: ['web/a.tsx'],
    comments: [operatorComment(policyMarker(HEAD, fingerprint))],
  })
  const adapter = fakeAdapter()
  const found = await candidates({ repo: REPO }, sweepDeps(w, { adapter }))
  assert.deepEqual(found.candidates, [])
  assert.deepEqual(
    found.skipped.map((x) => [x.pr, x.reason]),
    [[7, 'parked']],
  )
  assert.deepEqual(w.mutating(), [])
  assert.deepEqual(adapter.writeCalls(), [])
})

test('an unmarked legacy park is re-judged once, left standing, and skipped on the next sweep', async () => {
  const labelled = 'needs human: human-review:ui · web UI'
  const w = sweepWorld({
    statuses: [parkStatus(labelled), receipt()],
    baseConfig: policyText({ humanReview: { ui: [{ glob: 'web/**', label: 'web UI' }] } }),
    diff: ['web/a.tsx'],
  })
  const adapter = fakeAdapter()
  const d = sweepDeps(w, { adapter })
  const first = await candidates({ repo: REPO }, d)
  assert.deepEqual(
    first.candidates.map((c) => [c.pr, c.rejudge]),
    [[7, 'unrecorded']],
  )
  const j = await judged(w, d)
  assert.deepEqual([j.verdict, j.reason, j.held], ['human', 'human-review:ui', true])
  assert.match(j.policyFingerprint, /^[0-9a-f]{16}$/)
  const posted = await parkPost(j, d)
  assert.deepEqual(
    [posted.posted, posted.skipped, posted.policyRecord],
    [false, 'already-posted', 'posted'],
  )
  assert.deepEqual(w.statusPosts(), [], 'the park and its label are not re-posted')
  assert.deepEqual(adapter.writeCalls(), [], 'no tracker label or comment')
  assert.equal(policyComments(w).length, 1)
  assert.equal(
    policyComments(w)[0].body.trim().split('\n').at(-1),
    policyMarker(HEAD, j.policyFingerprint),
  )

  const before = w.mutating().length
  const second = await candidates({ repo: REPO }, d)
  assert.deepEqual(second.candidates, [])
  assert.deepEqual(
    second.skipped.map((x) => [x.pr, x.reason]),
    [[7, 'parked']],
  )
  // A repeated post of the same park records nothing twice.
  assert.equal((await parkPost(j, d)).policyRecord, 'current')
  assert.equal(w.mutating().length, before)
})

test('a park raised by an extension stands while it is installed and is released once it is removed', async () => {
  const record = `${EXT.name}:pass,${OTHER_EXT.name}:${SECURITY}`
  const parkedWorld = () =>
    sweepWorld({
      statuses: [parkStatus(`needs human: ${SECURITY}`), claimStatus(120_000), receipt()],
      comments: [recordComment(HEAD, record), operatorComment(policyMarker(HEAD, OLD_POLICY))],
    })

  const installed = parkedWorld()
  const stands = await judged(installed, sweepDeps(installed, { discover: discoverBoth }))
  assert.deepEqual([stands.verdict, stands.reason, stands.held], ['human', SECURITY, true])

  const removed = parkedWorld()
  const d = sweepDeps(removed, { discover: () => ({ extensions: [EXT] }) })
  assert.deepEqual(
    (await candidates({ repo: REPO }, d)).candidates.map((c) => c.rejudge),
    ['policy-changed'],
  )
  const released = await judged(removed, d)
  assert.equal(released.verdict, 'extensions-required', 'the remaining extension runs, unparked')
  assert.deepEqual(released.facts.extensionsApplicable, [EXT.name])
  assert.deepEqual(removed.mutating(), [])
})

test('parkStillRaised attributes each code to the zero-token judge, a recorded extension, or any live one', () => {
  const rec = [
    { extension: 'a', outcome: 'pass' },
    { extension: 'b', outcome: SECURITY },
  ]
  const ui = 'human-review:ui'
  const cases = [
    ['still a path hit', [ui], { fresh: [ui] }, true],
    ['path list emptied, an extension still applies', [ui], { applicable: ['a'] }, true],
    ['no extension ran', [ui], { extensionsRan: false, applicable: ['a'] }, false],
    ['no extension left', [SECURITY], { applicable: [] }, false],
    ['recorded raiser live', [SECURITY], { applicable: ['b'], record: rec }, true],
    ['recorded raiser gone', [SECURITY], { applicable: ['a'], record: rec }, false],
    ['path code beside a record', [ui], { applicable: ['a'], record: rec }, false],
    [
      'malfunction ignores the record',
      ['extension-failed', SECURITY],
      { applicable: ['a'], record: rec },
      true,
    ],
    ['repair gave up', ['repair-exhausted'], { extensionsRan: false }, true],
    ['worktree stuck off the head', ['worktree-diverged'], { extensionsRan: false }, true],
    ['worktree sync failed', ['worktree-unsynced'], { extensionsRan: false }, true],
    [
      'one stale code releases',
      [ui, 'partial-build'],
      { fresh: ['partial-build'], extensionsRan: false },
      false,
    ],
  ]
  for (const [name, codes, facts, want] of cases)
    assert.equal(parkStillRaised(codes, facts), want, name)
})

test('extensionRunBeforePark: only a claim beneath the newest park counts; unreadable is yes', () => {
  const park = parkStatus('needs human: human-review:ui', 1000)
  assert.equal(extensionRunBeforePark([park, receipt()]), false)
  assert.equal(extensionRunBeforePark([park, claimStatus(60_000)]), true)
  assert.equal(extensionRunBeforePark([claimStatus(500), park]), false, 'a claim after the park')
  assert.equal(extensionRunBeforePark(null), true)
})

test('an approval and a send-back on a park the policy no longer raises behave as before', async () => {
  const stale = (comments) =>
    sweepWorld({
      statuses: [parkStatus('needs human: human-review:security'), receipt()],
      baseConfig: policyText({ humanReview: { security: [] } }),
      comments,
    })
  const approved = stale([operatorComment(approveMarker(HEAD, [SECURITY]))])
  const handed = await candidates({ repo: REPO }, sweepDeps(approved))
  assert.deepEqual(
    handed.candidates.map((c) => [c.approval, c.waive, c.rejudge]),
    [['policy-park-approved', SECURITY, undefined]],
  )
  const j = await judge({ pr: 7, repo: REPO, waive: SECURITY }, sweepDeps(approved))
  assert.deepEqual([j.verdict, j.reason], ['pass', 'approved'])

  const sent = stale([operatorComment(sendBackMarker(HEAD))])
  const kept = await candidates({ repo: REPO }, sweepDeps(sent))
  assert.deepEqual(kept.candidates, [])
  assert.deepEqual(
    kept.skipped.map((x) => x.reason),
    ['parked'],
  )
  assert.deepEqual(sent.mutating(), [])
})

test('candidates leaves a park standing when the policy it would be re-judged under is unreadable', async () => {
  const w = sweepWorld({
    statuses: [parkStatus('needs human: human-review:security'), receipt()],
    baseConfig: null,
  })
  const r = await candidates({ repo: REPO }, sweepDeps(w))
  assert.deepEqual(
    r.skipped.map((x) => x.reason),
    ['parked'],
  )
})

test('post --policy goes with a human verdict and a well-formed fingerprint only', async () => {
  for (const options of [
    { verdict: 'pass', policy: OLD_POLICY },
    { verdict: 'human', reason: 'partial-build', policy: 'not-a-print' },
  ])
    await assert.rejects(
      post({ pr: 7, head: HEAD, repo: REPO, ...options }, deps(world())),
      UsageError,
      JSON.stringify(options),
    )
})

function priorPayload(nodes) {
  return { data: { repository: { pullRequest: { timelineItems: { nodes } } } } }
}
function priorCommit(contexts, sha = OTHER, hasNextPage = false) {
  return {
    commit: {
      oid: sha,
      statusCheckRollup: { contexts: { pageInfo: { hasNextPage }, nodes: contexts } },
    },
  }
}
const priorAction = {
  __typename: 'CheckRun',
  name: 'test-go',
  status: 'COMPLETED',
  conclusion: 'SUCCESS',
  checkSuite: { workflowRun: { workflow: { name: 'CI' } } },
}

test('judge and merge hold a shrinking push whose Actions workflow ran on the head', async () => {
  const opts = { priorHead: priorPayload([priorCommit([priorAction])]) }
  const w = world(opts)
  const r = await judge({ pr: 7, repo: REPO }, deps(w))
  assert.equal(r.verdict, 'wait')
  assert.equal(r.reason, 'ci-absent-gate')
  const m = world({ ...opts, statuses: verifyStatus('success', 'verified') })
  const merged = await merge({ pr: 7, repo: REPO, head: HEAD }, deps(m))
  assert.equal(merged.merged, false)
  assert.equal(merged.verdict, 'wait')
  assert.equal(merged.reason, 'ci-absent-gate')
  assert.equal(
    m.calls.some((c) => c.command === 'boss' && c.args[0] === 'merge'),
    false,
  )
})

test('judge and merge name a conflict ahead of any CI wait', async () => {
  const absent = { priorHead: priorPayload([priorCommit([priorAction])]) }
  const pending = { statusCheckRollup: [{ name: 'ci', status: 'IN_PROGRESS' }] }
  for (const opts of [
    { ...absent, pr: { mergeStateStatus: 'DIRTY' } },
    { pr: { ...pending, mergeStateStatus: 'DIRTY' } },
  ]) {
    const r = await judge({ pr: 7, repo: REPO }, deps(world(opts)))
    assert.equal(r.verdict, 'wait')
    assert.equal(r.reason, 'merge-state-dirty')
    const m = world({ ...opts, statuses: verifyStatus('success', 'verified') })
    const merged = await merge({ pr: 7, repo: REPO, head: HEAD }, deps(m))
    assert.equal(merged.merged, false)
    assert.equal(merged.verdict, 'wait')
    assert.equal(merged.reason, 'merge-state-dirty')
  }
})

test('judge matches a cancelled prior job to its own workflow by id, not by a shared name', async () => {
  // Two workflow files named `CI`: the prior job's own (id 11) ended on the head before any job
  // attached, so the other file's completed run must not supersede it.
  const cancelled = {
    __typename: 'CheckRun',
    name: 'matrix.id',
    status: 'COMPLETED',
    conclusion: 'CANCELLED',
    checkSuite: { workflowRun: { workflow: { name: 'CI', databaseId: 11 } } },
  }
  const headRuns = (ownConclusion) => [
    {
      workflow_runs: [
        {
          name: 'CI',
          workflow_id: 11,
          status: 'completed',
          conclusion: ownConclusion,
          head_sha: HEAD,
        },
        { name: 'CI', workflow_id: 12, status: 'completed', conclusion: 'success', head_sha: HEAD },
      ],
    },
  ]
  const priorHead = priorPayload([priorCommit([cancelled])])
  const held = await judge(
    { pr: 7, repo: REPO },
    deps(world({ priorHead, headRuns: headRuns('cancelled') })),
  )
  assert.equal(held.reason, 'ci-absent-gate')
  const passed = await judge(
    { pr: 7, repo: REPO },
    deps(world({ priorHead, headRuns: headRuns('success') })),
  )
  assert.equal(passed.verdict, 'pass')
})

test('judge fails closed on failed prior and head run reads', async () => {
  for (const opts of [
    { priorHead: null },
    { headRuns: null },
    { priorHead: {} },
    { headRuns: {} },
    { headRuns: [] },
    { headRuns: [null] },
    { headRuns: [{ workflow_runs: [], total_count: 1 }] },
  ]) {
    const r = await judge({ pr: 7, repo: REPO }, deps(world(opts)))
    assert.equal(r.verdict, 'wait')
    assert.equal(r.facts.ci.reason, 'unreadable')
    const merging = world({ ...opts, statuses: verifyStatus('success', 'verified') })
    const result = await merge({ pr: 7, repo: REPO, head: HEAD }, deps(merging))
    assert.equal(result.merged, false)
    assert.equal(result.verdict, 'wait')
    assert.equal(
      merging.calls.some((c) => c.command === 'boss' && c.args[0] === 'merge'),
      false,
    )
  }
})

test('judge discounts untriggered workflows and successful workflow-less prior contexts', async () => {
  for (const opts of [
    {
      priorHead: priorPayload([priorCommit([priorAction])]),
      headRuns: [
        { workflow_runs: [{ name: 'Other workflow', status: 'completed', head_sha: HEAD }] },
      ],
    },
    {
      priorHead: priorPayload([
        priorCommit([{ __typename: 'StatusContext', context: 'review', state: 'SUCCESS' }]),
      ]),
    },
    {
      priorHead: priorPayload([
        priorCommit([
          {
            name: 'app-review',
            status: 'COMPLETED',
            conclusion: 'NEUTRAL',
            checkSuite: { workflowRun: null },
          },
        ]),
      ]),
    },
    {},
  ])
    assert.equal((await judge({ pr: 7, repo: REPO }, deps(world(opts)))).verdict, 'pass')
  const failed = world({
    priorHead: priorPayload([priorCommit([{ context: 'review', state: 'FAILURE' }])]),
  })
  assert.equal((await judge({ pr: 7, repo: REPO }, deps(failed))).reason, 'ci-absent-gate')
  const blocked = world({
    priorHead: priorPayload([priorCommit([{ context: 'review', state: 'SUCCESS' }])]),
    pr: { mergeStateStatus: 'BLOCKED' },
  })
  assert.equal((await judge({ pr: 7, repo: REPO }, deps(blocked))).verdict, 'wait')
  const running = world({
    headRuns: [{ workflow_runs: [{ name: 'CI', status: 'in_progress', head_sha: HEAD }] }],
  })
  assert.equal((await judge({ pr: 7, repo: REPO }, deps(running))).reason, 'ci-not-settled')
})

test('resolvePriorHead resolves complete prior sets and rejects incomplete evidence', () => {
  const prior = priorCommit([priorAction])
  assert.deepEqual(resolvePriorHead(priorPayload([{ beforeCommit: prior.commit }]), HEAD), {
    known: true,
    sha: OTHER,
    contexts: [{ ...priorAction, workflowName: 'CI' }],
  })
  const identified = {
    ...priorAction,
    checkSuite: { workflowRun: { workflow: { name: 'CI', databaseId: 11 } } },
  }
  assert.deepEqual(resolvePriorHead(priorPayload([priorCommit([identified])]), HEAD).contexts, [
    { ...identified, workflowName: 'CI', workflowId: 11 },
  ])
  for (const node of [
    priorCommit([], HEAD),
    priorCommit([]),
    priorCommit([{ context: 'boss/build', state: 'SUCCESS' }]),
  ])
    assert.equal(resolvePriorHead(priorPayload([prior, node]), HEAD).sha, OTHER)
  assert.deepEqual(resolvePriorHead(priorPayload([]), HEAD), { known: true, sha: null })
  assert.deepEqual(
    resolvePriorHead(priorPayload([priorCommit([priorAction], OTHER, true)]), HEAD),
    { known: false },
  )
  assert.deepEqual(resolvePriorHead({}, HEAD), { known: false })
})

// ---------------------------------------------------------------------------
// Approval carry: a rebase that leaves the approved change unchanged keeps the approval.

// A real repository: main M0 -> M1 (a generator bump rewrites gen/api_pb.ts), the approved head A
// on M0, its rebase B onto M1 with gen/api_pb.ts regenerated differently, and C, a rebase that also
// edits hand-written code. `approved` and `rebased` replace the hand-written change A and B make.
const FEATURE = { 'src/app.go': 'package app\n\nfunc Feature() {}\n' }
function carryRepo({ approved = FEATURE, rebased = FEATURE } = {}) {
  const dir = tmp()
  const env = { ...process.env, GIT_CONFIG_GLOBAL: '/dev/null', GIT_CONFIG_NOSYSTEM: '1' }
  const git = (...args) => {
    const r = spawnSync(
      'git',
      [
        '-c',
        'user.name=t',
        '-c',
        'user.email=t@t',
        '-c',
        'commit.gpgsign=false',
        '-c',
        'gc.auto=0',
        '-c',
        'maintenance.auto=false',
        ...args,
      ],
      { cwd: dir, env, encoding: 'utf8' },
    )
    assert.equal(r.status, 0, `git ${args.join(' ')}: ${r.stderr}`)
    return r.stdout.trim()
  }
  const commit = (files, message) => {
    for (const [path, body] of Object.entries(files)) {
      mkdirSync(join(dir, path, '..'), { recursive: true })
      writeFileSync(join(dir, path), body)
    }
    git('add', '-A')
    git('commit', '-q', '-m', message)
    return git('rev-parse', 'HEAD')
  }
  git('init', '-q', '-b', 'main')
  const m0 = commit({ 'src/app.go': 'package app\n', 'gen/api_pb.ts': 'v0\n' }, 'm0')
  const m1 = commit({ 'src/other.go': 'package other\n', 'gen/api_pb.ts': 'v1\n' }, 'm1')
  git('checkout', '-q', '-b', 'pr', m0)
  const a = commit({ ...approved, 'gen/api_pb.ts': 'v0\nfeature\n' }, 'feature')
  git('checkout', '-q', '-b', 'rebased', m1)
  const b = commit({ ...rebased, 'gen/api_pb.ts': 'v1\nfeature-v1\n' }, 'feature')
  const c = commit({ 'src/app.go': 'package app\n\nfunc Feature() { panic(1) }\n' }, 'edit')
  return { dir, m1, a, b, c }
}

// The world's gh reads, with git reads served by the real repository: the fake base tip is M1, the
// approved head (ANCHOR) is A, and the judged head (HEAD) is `head`. `failGit` fails matching reads.
function carryRead(w, repo, head, failGit = () => false) {
  const real = commandReader({ cwd: repo.dir })
  const shas = [
    [BASE, repo.m1],
    [ANCHOR, repo.a],
    [HEAD, head],
  ]
  const map = (arg) => shas.reduce((out, [fake, sha]) => out.replaceAll(fake, sha), arg)
  return (command, args, opts) => {
    if (command !== 'git') return w.read(command, args, opts)
    if (failGit(args)) return { ok: false, error: 'exit-1', exit: 1, output: '' }
    return real(command, args.map(map), opts)
  }
}

const CARRIED = 'no-receipt,human-review:ui'
const carryPolicy = (extra = {}) =>
  policyText({
    humanReview: { ui: ['web/**'] },
    generatedPaths: ['gen/**'],
    generatedChecks: ['drift'],
    ...extra,
  })
const drift = (conclusion) => ({ name: 'drift', status: 'COMPLETED', conclusion })
const PROVEN = [...green, drift('SUCCESS')]

// An unreceipted head that touches ui paths, so it parks `no-receipt,human-review:ui` unless an
// approval waives both. CI, `drift` included, is green.
function carryWorld({ comments, pr, ...opts } = {}) {
  return world({
    statuses: [],
    diff: ['web/a.tsx'],
    baseConfig: carryPolicy(),
    comments: comments ?? [operatorComment(approveMarker(ANCHOR, CARRIED.split(',')))],
    pr: { statusCheckRollup: PROVEN, ...pr },
    ...opts,
  })
}

const judgeCarried = (w, repo, head = repo.b, failGit) =>
  judge({ pr: 7, repo: REPO }, deps(w, { read: carryRead(w, repo, head, failGit) }))

const withCarryRepo = async (fn, shape) => {
  const repo = carryRepo(shape)
  try {
    await fn(repo)
  } finally {
    rmSync(repo.dir, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 })
  }
}

test('judge carries an approval across a rebase whose only drift is in generated paths', () =>
  withCarryRepo(async (repo) => {
    const r = await judgeCarried(carryWorld(), repo)
    assert.equal(r.verdict, 'pass')
    assert.equal(r.reason, 'approved')
    assert.equal(r.facts.carry.from, ANCHOR)
    assert.equal(r.facts.carry.waives, CARRIED)
    assert.match(r.facts.carry.patchId, /^[0-9a-f]{40}$/)
    assert.equal(r.carryRecord, `${ANCHOR}:${r.facts.carry.patchId}`)

    // Without generatedPaths nothing is left out, so the regenerated file is a different change.
    const bare = carryWorld({ baseConfig: policyText({ humanReview: { ui: ['web/**'] } }) })
    const parked = await judgeCarried(bare, repo)
    assert.equal(parked.verdict, 'human')
    assert.equal(parked.reason, CARRIED)
    assert.equal(parked.carryRecord, undefined)
    assert.deepEqual(parked.facts.carry, { why: 'change-differs', from: ANCHOR })
  }))

test('judge does not carry an approval to a rebase that changes hand-written code', () =>
  withCarryRepo(async (repo) => {
    const r = await judgeCarried(carryWorld(), repo, repo.c)
    assert.equal(r.verdict, 'human')
    assert.equal(r.reason, CARRIED)
    assert.deepEqual(r.facts.carry, { why: 'change-differs', from: ANCHOR })
  }))

test('judge: a carried approval never grows, so a code it does not name parks the whole set', () =>
  withCarryRepo(async (repo) => {
    const comments = [operatorComment(approveMarker(ANCHOR, ['no-receipt']))]
    const r = await judgeCarried(carryWorld({ comments }), repo)
    assert.equal(r.verdict, 'human')
    assert.equal(r.reason, CARRIED)
    assert.equal(r.carryRecord, undefined)
    assert.deepEqual(r.facts.carry, { why: 'codes-not-covered', from: ANCHOR })
  }))

test('judge: a send-back after the approval, of that head or any other, blocks the carry', () =>
  withCarryRepo(async (repo) => {
    const approved = operatorComment(approveMarker(ANCHOR, CARRIED.split(',')), { msAgo: 120_000 })
    for (const sentBack of [ANCHOR, OTHER]) {
      const comments = [approved, operatorComment(sendBackMarker(sentBack), { msAgo: 60_000 })]
      const r = await judgeCarried(carryWorld({ comments }), repo)
      assert.equal(r.verdict, 'human', sentBack)
      assert.deepEqual(r.facts.carry, { why: 'sent-back-after-approval' })
    }
    // A send-back the approval came after does not block it.
    const earlier = [operatorComment(sendBackMarker(OTHER), { msAgo: 180_000 }), approved]
    assert.equal((await judgeCarried(carryWorld({ comments: earlier }), repo)).verdict, 'pass')
    // A head the operator already decided on is judged on its own decision, even a narrower one.
    const own = [approved, operatorComment(approveMarker(HEAD, ['no-receipt']), { msAgo: 60_000 })]
    const decided = await judgeCarried(carryWorld({ comments: own }), repo)
    assert.equal(decided.verdict, 'human')
    assert.deepEqual(decided.facts.carry, { why: 'head-decided' })
  }))

test('judge: an untrusted approval or a forged carry record carries nothing', () =>
  withCarryRepo(async (repo) => {
    const marker = approveMarker(ANCHOR, CARRIED.split(','))
    const untrusted = [
      { comments: [operatorComment(marker, { association: 'NONE' })] },
      { comments: [operatorComment(marker, { login: 'reader' })], permissions: { reader: 'read' } },
    ]
    for (const opts of untrusted) {
      const r = await judgeCarried(carryWorld(opts), repo)
      assert.equal(r.verdict, 'human')
      assert.deepEqual(r.facts.carry, { why: 'no-earlier-approval' })
    }
    // A carry record is never a waiver, whoever wrote it.
    const forged = operatorComment(carryMarker(HEAD, ANCHOR, 'f'.repeat(40)))
    const r = await judgeCarried(carryWorld({ comments: [forged] }), repo)
    assert.equal(r.verdict, 'human')
    assert.deepEqual(r.facts.carry, { why: 'no-earlier-approval' })
  }))

test('judge carries only on green CI and passed generation checks', () =>
  withCarryRepo(async (repo) => {
    const red = [{ name: 'ci', status: 'COMPLETED', conclusion: 'FAILURE' }]
    const waits = await judgeCarried(carryWorld({ pr: { statusCheckRollup: red } }), repo)
    assert.equal(waits.verdict, 'wait')
    assert.equal(waits.reason, 'ci-not-green-on-head')

    const baseConfig = carryPolicy()
    for (const rollup of [green, [...green, drift('SKIPPED')]]) {
      const r = await judgeCarried(
        carryWorld({ baseConfig, pr: { statusCheckRollup: rollup } }),
        repo,
      )
      assert.equal(r.verdict, 'human')
      assert.deepEqual(r.facts.carry, { why: 'generation-checks-not-passed', from: ANCHOR })
    }
    const rollup = [...green, drift('SUCCESS')]
    const proven = await judgeCarried(
      carryWorld({ baseConfig, pr: { statusCheckRollup: rollup } }),
      repo,
    )
    assert.equal(proven.verdict, 'pass')
  }))

test('judge: generatedPaths with no generatedChecks carries nothing, since nothing proves them', () =>
  withCarryRepo(async (repo) => {
    const unchecked = [
      policyText({ humanReview: { ui: ['web/**'] }, generatedPaths: ['gen/**'] }),
      carryPolicy({ generatedChecks: [] }),
    ]
    for (const baseConfig of unchecked) {
      const r = await judgeCarried(carryWorld({ baseConfig }), repo)
      assert.equal(r.verdict, 'human')
      assert.equal(r.carryRecord, undefined)
      assert.deepEqual(r.facts.carry, { why: 'generated-paths-unproven', from: ANCHOR })
    }
  }))

// Approved and rebased hand-written changes that differ only in whitespace or a file name's
// leading space: each is code the operator never saw, so none carries.
const UNSEEN = [
  {
    name: 'indentation only',
    approved: { 'src/job.py': 'def run(x):\n    if x:\n        a()\n    c()\n' },
    rebased: { 'src/job.py': 'def run(x):\n    if x:\n        a()\n        c()\n' },
  },
  {
    name: 'whitespace inside a line',
    approved: { 'scripts/clean.sh': 'rm -rf /tmp/x /tmp/y\n' },
    rebased: { 'scripts/clean.sh': 'rm -rf /tmp/x/tmp/y\n' },
  },
  {
    name: 'trailing whitespace on the last line of the diff',
    approved: { 'src/z.txt': 'x = 1\n' },
    rebased: { 'src/z.txt': 'x = 1  \n' },
  },
  {
    name: 'a file whose name starts with a space',
    approved: { ...FEATURE, ' lead.txt': 'benign\n' },
    rebased: { ...FEATURE, ' lead.txt': 'MALICIOUS\n' },
  },
]

for (const { name, approved, rebased } of UNSEEN)
  test(`judge does not carry an approval across a change in ${name}`, () =>
    withCarryRepo(
      async (repo) => {
        const r = await judgeCarried(carryWorld(), repo)
        assert.equal(r.verdict, 'human')
        assert.equal(r.carryRecord, undefined)
        assert.deepEqual(r.facts.carry, { why: 'change-differs', from: ANCHOR })
      },
      { approved, rebased },
    ))

test('judge: a commit, diff or comment read that fails carries nothing', () =>
  withCarryRepo(async (repo) => {
    const missing = await judgeCarried(carryWorld(), repo, '1'.repeat(40))
    assert.equal(missing.verdict, 'human')
    assert.deepEqual(missing.facts.carry, { why: 'head-commit-unavailable', from: ANCHOR })

    const noDiff = await judgeCarried(carryWorld(), repo, repo.b, (args) => args[0] === 'diff')
    assert.equal(noDiff.verdict, 'human')
    assert.deepEqual(noDiff.facts.carry, { why: 'approved-head-diff-unreadable', from: ANCHOR })

    const unread = await judgeCarried(carryWorld({ commentsReadFails: true }), repo)
    assert.notEqual(unread.verdict, 'pass')
    assert.equal(unread.carryRecord, undefined)
  }))

test('post --carry-record posts the visible carry record once, then the approved pass', async () => {
  const patch = '9'.repeat(40)
  const record = `${ANCHOR}:${patch}`
  const approved = operatorComment(approveMarker(ANCHOR, CARRIED.split(',')))
  const args = { pr: 7, head: HEAD, verdict: 'pass', reason: 'approved', repo: REPO }
  const w = world({ comments: [approved] })
  const r = await post({ ...args, carryRecord: record }, deps(w))
  assert.equal(r.posted, true)
  assert.equal(r.carryRecord, 'posted')
  const comment = w.comments.at(-1)
  assert.ok(comment.body.endsWith(carryMarker(HEAD, ANCHOR, patch)))
  assert.ok(comment.body.includes(approved.html_url))
  assert.ok(
    comment.body.includes(`It waives ${parkCategories(CARRIED.split(',')).join(', ')} only.`),
  )
  assert.ok(!comment.body.split('\n').slice(0, -1).join('\n').includes(CARRIED), comment.body)
  assert.deepEqual(
    operatorMarkers([comment]).map((m) => [m.kind, m.head, m.from, m.patch]),
    [['carry', HEAD, ANCHOR, patch]],
  )
  assert.deepEqual(w.statusPosts(), [
    { state: 'success', description: 'verified (approved)', target: undefined },
  ])

  // The trusted record already stands, so a re-post writes no second one.
  const again = world({ comments: [approved, comment] })
  const r2 = await post({ ...args, carryRecord: record }, deps(again))
  assert.equal(r2.carryRecord, 'current')
  assert.equal(again.commentPosts, 0)

  // A dry run plans the record and writes nothing.
  const dry = world({ comments: [approved] })
  const planned = await post({ ...args, carryRecord: record, dryRun: true }, deps(dry))
  assert.equal(planned.carryRecord, 'planned')
  assert.deepEqual(dry.mutating(), [])
  assert.ok(planned.writes.some((x) => x.kind === 'pr-comment' && x.body.includes(ANCHOR)))
})

test('post --carry-record refuses a carry the comments no longer support and posts no pass', async () => {
  const record = `${ANCHOR}:${'9'.repeat(40)}`
  const args = { pr: 7, head: HEAD, verdict: 'pass', reason: 'approved', repo: REPO }
  const approved = operatorComment(approveMarker(ANCHOR, CARRIED.split(',')), { msAgo: 120_000 })
  for (const [comments, reason] of [
    [[approved, operatorComment(sendBackMarker(ANCHOR), { msAgo: 60_000 })], 'carry-unconfirmed'],
    [
      [operatorComment(approveMarker(ANCHOR, ['no-receipt']), { association: 'NONE' })],
      'carry-unconfirmed',
    ],
    [[approved], 'comments-unreadable'],
  ]) {
    const w = world({ comments, commentsReadFails: reason === 'comments-unreadable' })
    const r = await post({ ...args, carryRecord: record }, deps(w))
    assert.equal(r.posted, false)
    assert.equal(r.reason, reason)
    assert.deepEqual(w.mutating(), [])
  }

  for (const options of [
    { verdict: 'pass', carryRecord: record },
    { verdict: 'human', reason: 'no-receipt', carryRecord: record },
    { verdict: 'pass', reason: 'approved', carryRecord: 'not-a-record' },
  ])
    await assert.rejects(
      post({ pr: 7, head: HEAD, repo: REPO, ...options }, deps(world())),
      UsageError,
      JSON.stringify(options),
    )
})

test('policy CLI exits non-zero for a real deferred verifyDefaults fault', (t) => {
  const dir = mkdtempSync(join(tmpdir(), 'verify-policy-deferred-'))
  t.after(() => rmSync(dir, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 }))
  writeFileSync(
    join(dir, '.boss-skills.json'),
    JSON.stringify({
      verifyDefaults: { alwaysHumanPaths: [{ glob: 'src/**', unexpected: true }] },
    }),
  )
  const result = spawnSync(
    process.execPath,
    [new URL('./verify-gate.mjs', import.meta.url).pathname, 'policy'],
    {
      cwd: dir,
      encoding: 'utf8',
    },
  )
  assert.equal(result.status, 1)
  assert.equal(result.stdout, '')
  assert.match(result.stderr, /deferred verifyDefaults/)
  assert.match(
    result.stderr,
    /skill-config: invalid config from .*verifyDefaults.alwaysHumanPaths must be/,
  )
})

test('verified heads retry only after the merge grace window, including unreadable dates', () => {
  for (const updatedAt of [iso(MERGE_RETRY_AFTER_MS + 1), '', 'garbled']) {
    assert.deepEqual(classifyHeadStatus({ state: 'success', updatedAt }, NOW), {
      skip: 'verified',
      mergeRetry: true,
    })
  }
  for (const age of [1000, MERGE_RETRY_AFTER_MS]) {
    assert.deepEqual(classifyHeadStatus({ state: 'success', updatedAt: iso(age) }, NOW), {
      skip: 'verified',
    })
  }
})

test('candidates collect merge retries beyond the fresh limit without CI or approval reads', async () => {
  const tickets = [1, 2, 3, 4].map((n) => ({
    identifier: `BOS-${n}`,
    labels: ['agent-build', 'needs-human'],
  }))
  tickets[0].labels = ['agent-build']
  const w = candidateWorld({
    tickets,
    statuses: {
      [SHA_OF(2)]: verifyStatus('success', 'verified', MERGE_RETRY_AFTER_MS + 1),
      [SHA_OF(3)]: verifyStatus('success', 'verified'),
      [SHA_OF(4)]: verifyStatus('success', 'verified', MERGE_RETRY_AFTER_MS + 1),
    },
  })
  const r = await candidates({ repo: REPO, limit: 1 }, candDeps(w))
  assert.deepEqual(
    r.candidates.map((c) => c.pr),
    [1],
  )
  assert.deepEqual(
    r.mergeRetries,
    [2, 4].map((n) => ({
      ticket: `BOS-${n}`,
      pr: n,
      url: `https://github.com/o/r/pull/${n}`,
      headSha: SHA_OF(n),
      repo: REPO,
    })),
  )
  assert.deepEqual(
    r.skipped,
    [2, 3, 4].map((n) => ({ ticket: `BOS-${n}`, pr: n, reason: 'verified' })),
  )
  assert.deepEqual(
    w.calls.filter((c) => c.args[0] === 'pr' && c.args[1] === 'view').map((c) => c.args[2]),
    ['1'],
  )
  assert.equal(
    w.calls.some((c) => /comments/.test(c.args[1])),
    false,
  )
  assert.equal(w.statusReads().length, 4)
})

test('candidates stop retrying a verified head whose session is elsewhere once the PR says so', async () => {
  const old = MERGE_RETRY_AFTER_MS + 1
  const prUrl = (n) => `https://github.com/o/r/pull/${n}`
  const noticeOn = (sha) => [operatorComment(remoteSessionMarker(sha))]
  const tickets = [2, 3, 4, 5, 6].map((n) => ({ identifier: `BOS-${n}`, labels: ['agent-build'] }))
  const w = candidateWorld({
    tickets,
    statuses: Object.fromEntries(
      [2, 3, 4, 5, 6].map((n) => [SHA_OF(n), verifyStatus('success', 'verified', old)]),
    ),
    // #2's session is here; every other PR's is not.
    sessions: [{ id: 's2', pr_url: prUrl(2) }],
    comments: { 2: noticeOn(SHA_OF(2)), 4: noticeOn(SHA_OF(4)), 5: noticeOn(SHA_OF(99)) },
    commentsFail: [6],
  })
  const r = await candidates({ repo: REPO }, candDeps(w))
  // #2 has its session here; #3 has no notice yet; #5's notice is for an older head; #6's comments
  // are unreadable. Only #4, whose PR already says this machine cannot merge it, stops.
  assert.deepEqual(
    r.mergeRetries.map((c) => c.pr),
    [2, 3, 5, 6],
  )
  assert.deepEqual(
    r.skipped.find((s) => s.pr === 4),
    { ticket: 'BOS-4', pr: 4, reason: 'no-session' },
  )
  assert.equal(w.calls.filter((c) => c.command === 'boss').length, 1, 'one session read')
  assert.equal(
    w.calls.some((c) => c.args[1] === 'repos/o/r/issues/2/comments?per_page=100'),
    false,
    'a local session needs no comment read',
  )

  const unread = candidateWorld({
    tickets,
    statuses: { [SHA_OF(4)]: verifyStatus('success', 'verified', old) },
    comments: { 4: noticeOn(SHA_OF(4)) },
  })
  const r2 = await candidates({ repo: REPO }, candDeps(unread))
  assert.deepEqual(
    r2.mergeRetries.map((c) => c.pr),
    [4],
    'an unreadable session list keeps the retry',
  )
})

// One PR seen by the candidates scan and by merge over `ticks` sweeps: the comments merge posts are
// what the next scan reads. Returns the merge retries per tick and the world merge wrote to.
async function remoteSessionSweeps(ticks, { poster, permissions = {} } = {}) {
  const pr = world({ statuses: verified(), sessions: elsewhere, poster, permissions })
  const tick = () =>
    candidates(
      { repo: REPO },
      candDeps(
        candidateWorld({
          tickets: [{ identifier: 'BOS-1', labels: ['agent-build'] }],
          prs: [{ number: 7, url: PR_URL, body: BODY, headRefOid: HEAD, isDraft: false }],
          statuses: { [HEAD]: verifyStatus('success', 'verified', MERGE_RETRY_AFTER_MS + 1) },
          sessions: elsewhere,
          comments: { 7: pr.comments },
          permissions,
        }),
      ),
    )
  const retried = []
  for (let n = 1; n <= ticks; n++) {
    const found = await tick()
    retried.push(found.mergeRetries.length)
    for (const c of found.mergeRetries)
      await merge({ pr: c.pr, head: c.headSha, repo: REPO }, deps(pr))
  }
  return { retried, pr }
}

test('a verified head with its session elsewhere is retried until the PR is told, then never again', async () => {
  const { retried, pr } = await remoteSessionSweeps(4)
  assert.deepEqual(retried, [1, 0, 0, 0])
  assert.equal(pr.commentPosts, 1, 'the operator is told once')
  assert.equal(pr.calls.filter((c) => c.command === 'boss' && c.args[0] === 'merge').length, 0)
})

test('the no-session notice stands whoever the gate posts as: an app, a bot or a read-only collaborator', async () => {
  for (const [poster, permission] of [
    [{ login: 'gate-app', association: 'NONE' }, 'not-found'],
    [{ login: 'ci-bot', association: 'CONTRIBUTOR' }, null],
    [{ login: 'reader', association: 'COLLABORATOR' }, 'read'],
  ]) {
    const { retried, pr } = await remoteSessionSweeps(4, {
      poster,
      permissions: { [poster.login]: permission },
    })
    assert.deepEqual(retried, [1, 0, 0, 0], `${poster.login}: retried until told`)
    assert.equal(pr.commentPosts, 1, `${poster.login}: told once`)
  }
})

test('only a marker that is the whole last line stands for the no-session notice', async () => {
  const marker = remoteSessionMarker(HEAD)
  for (const [last, stands] of [
    [marker, true],
    [`  ${marker.replace(HEAD, HEAD.toUpperCase())}  `, true],
    [`> ${marker}`, false],
    [`${marker} thanks`, false],
    [`see ${marker}`, false],
    [marker.replace(HEAD, HEAD.slice(0, 12)), false],
  ]) {
    const comment = { ...operatorComment(marker), body: `Quoting the gate:\n\n${last}` }
    const w = world({ statuses: verified(), sessions: elsewhere, comments: [comment] })
    const r = await merge({ pr: 7, head: HEAD, repo: REPO }, deps(w))
    assert.equal(r.notice, stands ? 'standing' : 'posted', JSON.stringify(last))

    const scan = await candidates(
      { repo: REPO },
      candDeps(
        candidateWorld({
          tickets: [{ identifier: 'BOS-1', labels: ['agent-build'] }],
          prs: [{ number: 7, url: PR_URL, body: BODY, headRefOid: HEAD, isDraft: false }],
          statuses: { [HEAD]: verifyStatus('success', 'verified', MERGE_RETRY_AFTER_MS + 1) },
          sessions: elsewhere,
          comments: { 7: [comment] },
        }),
      ),
    )
    assert.equal(scan.mergeRetries.length, stands ? 0 : 1, JSON.stringify(last))
  }
})

// ---------------------------------------------------------------------------
// Ticketless factory PRs: a cron run's PR that no ticket names is still verified.

const cronPr = (n, extra = {}) => ({
  number: n,
  url: `https://github.com/o/r/pull/${n}`,
  body: 'A debt fix.\n\nDebt-Category: portability\n',
  headRefOid: SHA_OF(n),
  headRefName: `cron-bs-debt-${1792084200 + n}`,
  isDraft: false,
  ...extra,
})
const ticketedPr = (n, id) => ({ ...cronPr(n), body: ticketBody(id), headRefName: `feat/${n}` })
const lsCalls = (w) => w.calls.filter((c) => c.command === 'boss').length

test('candidates: a ticketless factory PR is judged; one a ticket, a person or nobody owns is not', async () => {
  const w = candidateWorld({
    tickets: [
      { identifier: 'BOS-1', labels: ['agent-build'], attachments: [] },
      {
        identifier: 'BOS-2',
        labels: ['agent-build'],
        attachments: [{ url: 'https://github.com/o/r/pull/10' }],
      },
    ],
    prs: [
      ticketedPr(1, 'BOS-1'),
      cronPr(5),
      cronPr(6),
      { ...cronPr(7), headRefName: 'feature/by-hand' },
      cronPr(8),
      cronPr(9, { isDraft: true }),
      cronPr(10),
    ],
    sessions: [
      { id: 's5', pr_url: 'https://github.com/o/r/pull/5', tracker_id: null },
      { id: 's6', pr_url: 'https://github.com/o/r/pull/6', tracker_id: 'BOS-6' },
      { id: 's7', pr_url: 'https://github.com/o/r/pull/7', tracker_id: null },
      { id: 's9', pr_url: 'https://github.com/o/r/pull/9', tracker_id: null },
      { id: 's10', pr_url: 'https://github.com/o/r/pull/10', tracker_id: null },
    ],
  })
  const r = await candidates({ repo: REPO }, candDeps(w))
  const entry = (ticket, n) => ({
    ticket,
    pr: n,
    url: `https://github.com/o/r/pull/${n}`,
    headSha: SHA_OF(n),
    repo: REPO,
    claim: 'none',
  })
  // #10 is BOS-2's by attachment, so it is judged once, as a ticketed PR.
  assert.deepEqual(r.candidates, [entry('BOS-1', 1), entry(null, 5), entry('BOS-2', 10)])
  assert.deepEqual(r.skipped, [
    { ticket: null, pr: 6, reason: 'session-has-ticket' },
    { ticket: null, pr: 8, reason: 'no-session' },
    { ticket: null, pr: 9, reason: 'draft' },
  ])
  assert.equal(lsCalls(w), 1)
})

test('candidates: only an unscoped scan admits ticketless PRs, and an unreadable session list skips only them', async () => {
  const prs = [ticketedPr(1, 'BOS-1'), cronPr(5)]
  const sessions = [{ id: 's5', pr_url: 'https://github.com/o/r/pull/5' }]
  const scan = async (options, config, w = candidateWorld({ prs, sessions })) => {
    const r = await candidates({ repo: REPO, ...options }, candDeps(w, config))
    return { prs: r.candidates.map((c) => c.pr), skipped: r.skipped, ls: lsCalls(w) }
  }
  assert.deepEqual(await scan({ ticket: 'BOS-1' }), { prs: [1], skipped: [], ls: 0 })
  assert.deepEqual(await scan({ selection: { labels: { include: ['backend'] } } }), {
    prs: [1],
    skipped: [],
    ls: 0,
  })
  assert.deepEqual(await scan({}, candidateConfig({ projects: { include: ['Core'] } })), {
    prs: [1],
    skipped: [],
    ls: 0,
  })
  assert.deepEqual(await scan({ selection: { labels: { exclude: ['infra'] } } }), {
    prs: [1, 5],
    skipped: [],
    ls: 1,
  })
  assert.deepEqual(await scan({}, undefined, candidateWorld({ prs })), {
    prs: [1],
    skipped: [{ ticket: null, pr: 5, reason: 'unknown:sessions' }],
    ls: 1,
  })
})

const TICKETLESS_BODY =
  'The cron branch pruner used the wrong path.\n\nDebt-Category: portability\n'
const ticketlessWorld = (opts = {}) =>
  world({
    pr: {
      title: 'Honor XDG config in cron branch pruning',
      body: TICKETLESS_BODY,
      headRefName: 'cron-bs-debt-1792084207',
    },
    sessions: [{ id: 'sess-1', pr_url: PR_URL, tracker_id: null }],
    statuses: [],
    ...opts,
  })
const parkNotes = (w) => w.comments.filter((c) => PARK_NOTE_MARKER.test(c.body.split('\n').at(-1)))

test('a ticketless factory PR parks on its status and a park note, and writes no tracker state', async () => {
  const w = ticketlessWorld()
  const adapter = fakeAdapter({ tickets: [] })
  const judged = await judge({ pr: 7, repo: REPO }, deps(w, { adapter }))
  assert.equal(judged.verdict, 'human')
  // A factory PR written without boss-build's sections parks only for the missing review.
  assert.equal(judged.reason, 'no-receipt')
  const r = await post(
    { pr: 7, head: HEAD, verdict: 'human', reason: judged.reason, repo: REPO },
    deps(w, { adapter }),
  )
  assert.deepEqual(
    [r.posted, r.trackerWrites, r.why, r.parkNote],
    [true, 'none', 'no-ticket', 'posted'],
  )
  assert.deepEqual(adapter.calls, [], 'no probe and no write')
  assert.deepEqual(
    w.statusPosts().map((s) => s.description),
    ['needs human: no-receipt'],
  )
  const [note] = parkNotes(w)
  for (const line of [
    'No automated build and review has passed on this commit.',
    'This PR has no tracker ticket, so no ticket was updated.',
  ])
    assert.ok(note.body.includes(line), line)

  const dir = tmp()
  try {
    const d = ticketlessWorld({ show: { repo_can_auto_repair: false } })
    const defect = await post(
      { pr: 7, head: HEAD, verdict: 'defect', findings: findingsFile(dir), repo: REPO },
      deps(d, { adapter }),
    )
    assert.deepEqual([defect.posted, defect.trackerWrites], [true, 'none'])
    assert.deepEqual(
      d.statusPosts().map((s) => s.state),
      ['failure'],
    )
    assert.deepEqual(adapter.calls, [])
  } finally {
    rmSync(dir, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 })
  }
})

test('merge: an approved ticketless PR merges and moves no ticket', async () => {
  const w = ticketlessWorld({
    statuses: [{ ...verified()[0], description: 'verified (approved)' }, receipt()],
    prAfterMerge: prView({ state: 'MERGED', mergeCommit: { oid: MERGED_SHA } }),
  })
  const adapter = fakeAdapter({ tickets: [] })
  const r = await merge({ pr: 7, head: HEAD, repo: REPO }, deps(w, { adapter }))
  assert.deepEqual(
    [r.merged, r.mergeSha, r.trackerWrites, r.why],
    [true, MERGED_SHA, 'none', 'no-ticket'],
  )
  assert.deepEqual(adapter.calls, [])
})

test('candidates: a ticketless verified head lands in mergeRetries, and a full judge list does not stop the scan', async () => {
  const old = MERGE_RETRY_AFTER_MS + 1
  const w = candidateWorld({
    prs: [ticketedPr(1, 'BOS-1'), cronPr(5), cronPr(6), cronPr(8)],
    sessions: [5, 6, 8].map((n) => ({ id: `s${n}`, pr_url: `https://github.com/o/r/pull/${n}` })),
    statuses: {
      [SHA_OF(6)]: verifyStatus('success', 'verified', old),
      [SHA_OF(8)]: verifyStatus('success', 'verified', old),
    },
  })
  const retry = (n) => ({
    ticket: null,
    pr: n,
    url: `https://github.com/o/r/pull/${n}`,
    headSha: SHA_OF(n),
    repo: REPO,
  })
  const all = await candidates({ repo: REPO }, candDeps(w))
  assert.deepEqual(
    all.candidates.map((c) => [c.ticket, c.pr]),
    [
      ['BOS-1', 1],
      [null, 5],
    ],
  )
  assert.deepEqual(all.mergeRetries, [retry(6), retry(8)])
  // #1 fills a one-entry judge list; #5 is left for a later sweep and #6, #8 are still retried.
  const full = await candidates({ repo: REPO, limit: 1 }, candDeps(w))
  assert.deepEqual(
    full.candidates.map((c) => c.pr),
    [1],
  )
  assert.deepEqual(full.mergeRetries, [retry(6), retry(8)])
})

test('candidates: an attachment claim is read with no selection or label rule, and an unreadable one skips only ticketless PRs', async () => {
  const w = candidateWorld({
    prs: [ticketedPr(1, 'BOS-1'), cronPr(5)],
    sessions: [{ id: 's5', pr_url: 'https://github.com/o/r/pull/5' }],
  })
  const r = await candidates(
    { repo: REPO, selection: { labels: { exclude: ['infra'] } } },
    candDeps(w),
  )
  assert.deepEqual(
    r.candidates.map((c) => c.pr),
    [1, 5],
  )
  const claim = w.queries.at(-1)
  assert.deepEqual(
    [claim.state, claim.requireLabels, claim.excludeLabels, claim.selection.labels.exclude],
    ['In Review', [], [], []],
  )

  const broken = candidateWorld({
    prs: [ticketedPr(1, 'BOS-1'), cronPr(5)],
    sessions: [{ id: 's5', pr_url: 'https://github.com/o/r/pull/5' }],
  })
  const adapter = {
    ...broken.adapter,
    selectPlanned: async (query) =>
      query.requireLabels.length === 0 ? null : broken.adapter.selectPlanned(query),
  }
  const unread = await candidates({ repo: REPO }, { ...candDeps(broken), adapter })
  assert.deepEqual(
    unread.candidates.map((c) => c.pr),
    [1],
  )
  assert.deepEqual(unread.skipped, [{ ticket: null, pr: 5, reason: 'unknown:claims' }])
})

test('post and merge share the discovery predicate: a PR some ticket may stand behind keeps the tracker path', async () => {
  const cases = [
    ['ticketless', {}, {}],
    // A manual /boss-verify on a person's branch whose ticket links the PR only by attachment.
    ['human branch', { pr: { headRefName: 'feature/by-hand' } }, {}],
    [
      'attachment claim',
      {},
      { tickets: [{ identifier: 'BOS-9', attachments: [{ url: PR_URL }] }] },
    ],
    ['claims unreadable', {}, { tickets: null }],
    [
      'session has a tracker id',
      { sessions: [{ id: 'sess-1', pr_url: PR_URL, tracker_id: 'BOS-1' }] },
      {},
    ],
    ['no session row', { sessions: [] }, {}],
    ['session list unreadable', { sessions: null }, {}],
  ]
  for (const [name, worldOpts, adapterOpts] of cases) {
    const base = ticketlessWorld()
    const w = ticketlessWorld({
      ...worldOpts,
      pr: { ...base.pr, ...(worldOpts.pr ?? {}) },
    })
    const adapter = fakeAdapter({ tickets: [], ...adapterOpts })
    const r = await post(
      { pr: 7, head: HEAD, verdict: 'human', reason: 'no-receipt', repo: REPO },
      deps(w, { adapter }),
    )
    const expected =
      name === 'ticketless' ? ['none', 'no-ticket', 1] : ['unavailable', 'no-ticket-line', 0]
    assert.deepEqual([r.trackerWrites, r.why, w.statusPosts().length], expected, name)
  }

  const claimed = ticketlessWorld({
    statuses: [{ ...verified()[0], description: 'verified (approved)' }, receipt()],
    prAfterMerge: prView({ state: 'MERGED', mergeCommit: { oid: MERGED_SHA } }),
  })
  const adapter = fakeAdapter({
    tickets: [{ identifier: 'BOS-9', attachments: [{ url: PR_URL }] }],
  })
  const merged = await merge({ pr: 7, head: HEAD, repo: REPO }, deps(claimed, { adapter }))
  assert.deepEqual(
    [merged.merged, merged.trackerWrites, merged.why],
    [true, 'unavailable', 'no-ticket-line'],
  )
})

// ---------------------------------------------------------------------------
// The park note: what each code means, what verify saw, and what to do next.

test('post: one park note per head, left alone while it holds and rewritten in place when it changes', async () => {
  const openBody = PARTIAL_BODY
  const w = world({ pr: { body: openBody } })
  const human = () =>
    post({ pr: 7, head: HEAD, verdict: 'human', reason: 'partial-build', repo: REPO }, deps(w))

  const first = await human()
  assert.deepEqual([first.posted, first.parkNote], [true, 'posted'])
  assert.equal(parkNotes(w).length, 1)
  assert.ok(parkNotes(w)[0].body.includes('partial: 2/5 acceptance criteria'))

  const again = await human()
  assert.deepEqual([again.skipped, again.parkNote], ['already-posted', 'current'])
  assert.equal(w.comments.length, 1, 'the standing note is the only comment')

  w.pr.body = openBody.replace('partial: 2/5', 'partial: 3/5')
  const edited = await human()
  assert.equal(edited.parkNote, 'updated')
  assert.equal(parkNotes(w).length, 1)
  assert.ok(parkNotes(w)[0].body.includes('partial: 3/5 acceptance criteria'))
  const patches = w.calls.filter((c) => c.args[0] === 'api' && c.args[2] === 'PATCH')
  assert.equal(patches.length, 1)

  const dry = world({ pr: { body: openBody } })
  const planned = await post(
    { pr: 7, head: HEAD, verdict: 'human', reason: 'partial-build', repo: REPO, dryRun: true },
    deps(dry),
  )
  assert.equal(planned.parkNote, 'planned')
  assert.ok(
    planned.writes.some(
      (x) => x.kind === 'pr-comment' && PARK_NOTE_MARKER.test(x.body.split('\n').at(-1)),
    ),
  )
  assert.equal(dry.mutating().length, 0)
})

test('post --extension-results puts the failing extension and its own error in the park note', async () => {
  const dir = tmp()
  try {
    const error = 'stopped without publication after detecting a misleading archive-sweep assertion'
    const results = jsonFile(dir, 'r.json', [
      {
        extension: 'boss-verify-evidence',
        result: { ok: false, extension: 'boss-verify-evidence', role: 'verify', error },
      },
    ])
    const w = world()
    const argv = ['--pr', '7', '--head', HEAD, '--repo', REPO, '--extension-results', results]
    const r = await main(
      ['post', ...argv, '--verdict', 'human', '--reason', 'extension-failed,human-review:ui'],
      deps(w),
    )
    assert.equal(r.parkNote, 'posted')
    assert.ok(
      parkNotes(w)[0].body.includes(`\`boss-verify-evidence\` reported a failure: ${error}`),
    )
    await assert.rejects(main(['post', ...argv, '--verdict', 'pass'], deps(world())), UsageError)
  } finally {
    rmSync(dir, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 })
  }
})

// ---------------------------------------------------------------------------
// Parks recorded under the retired PR checklist gate name `ledger-open`.

test('candidates: a standing park naming the retired ledger-open code is re-judged, not stranded', async () => {
  const pr = (n) => ({
    number: n,
    url: `https://github.com/o/r/pull/${n}`,
    body: ticketBody(`BOS-${n}`),
    headRefOid: SHA_OF(n),
    isDraft: false,
  })
  const w = candidateWorld({
    tickets: [1, 2, 3].map((n) => ({
      identifier: `BOS-${n}`,
      labels: ['agent-build'],
      attachments: [],
    })),
    prs: [pr(1), pr(2), pr(3)],
    statuses: {
      [SHA_OF(1)]: verifyStatus('pending', 'needs human: ledger-open'),
      [SHA_OF(2)]: verifyStatus('pending', 'needs human: no-receipt,ledger-open'),
      [SHA_OF(3)]: verifyStatus('pending', 'needs human: partial-build'),
    },
    comments: {
      1: [
        operatorComment(
          parkNoteMarker(SHA_OF(1), '0'.repeat(16)).replace(
            ' -->',
            ` ledger=${'1'.repeat(16)} -->`,
          ),
        ),
      ],
    },
  })
  const r = await candidates({ repo: REPO }, candDeps(w))
  assert.deepEqual(
    r.candidates.map((c) => [c.pr, c.rejudge]),
    [
      [1, 'codes-unknown'],
      [2, 'codes-unknown'],
    ],
  )
  assert.deepEqual(
    r.skipped.map((s) => [s.pr, s.reason]),
    [[3, 'parked']],
  )
})

const parkedWorld = (description, pr) =>
  world({
    pr,
    statuses: [{ context: 'boss/verify', state: 'pending', description }, receipt()],
  })

test('judge: a re-judged ledger-open park passes unless the PR carries the PARTIAL marker', async () => {
  const released = await judge(
    { pr: 7, repo: REPO },
    deps(parkedWorld('needs human: ledger-open', { body: BODY })),
  )
  assert.deepEqual([released.verdict, released.reason], ['pass', 'verified'])
  const partial = await judge(
    { pr: 7, repo: REPO },
    deps(parkedWorld('needs human: ledger-open', { body: PARTIAL_BODY })),
  )
  assert.deepEqual(
    [partial.verdict, partial.reason, partial.held],
    ['human', 'partial-build', undefined],
  )
})

test('judge: a standing partial park is held while the marker stands and released once it goes', async () => {
  const held = await judge(
    { pr: 7, repo: REPO },
    deps(parkedWorld('needs human: partial-build', { body: PARTIAL_BODY })),
  )
  assert.deepEqual([held.verdict, held.reason, held.held], ['human', 'partial-build', true])
  assert.equal('ledgerFingerprint' in held, false)
  const released = await judge(
    { pr: 7, repo: REPO },
    deps(parkedWorld('needs human: partial-build', { body: BODY })),
  )
  assert.deepEqual([released.verdict, released.reason], ['pass', 'verified'])
})

test("post rewrites only a park note verify itself wrote, and posts fresh beside anyone else's", async () => {
  const forged = (login) => operatorComment(parkNoteMarker(HEAD, '0'.repeat(16)), { login, id: 41 })
  const run = async ({ login, user = 'operator' }) => {
    const w = world({ pr: { body: PARTIAL_BODY }, comments: [forged(login)], user })
    const before = w.comments[0].body
    const r = await post(
      { pr: 7, head: HEAD, verdict: 'human', reason: 'partial-build', repo: REPO },
      deps(w),
    )
    const patched = w.calls.some((c) => c.args[0] === 'api' && c.args[2] === 'PATCH')
    return [r.parkNote, patched, w.comments[0].body === before, w.comments.length]
  }
  assert.deepEqual(await run({ login: 'operator' }), ['updated', true, false, 1])
  assert.deepEqual(await run({ login: 'collaborator' }), ['posted', false, true, 2])
  assert.deepEqual(await run({ login: 'operator', user: null }), ['posted', false, true, 2])
})

test('post no longer takes --ledger', async () => {
  await assert.rejects(
    main(
      [
        'post',
        '--pr',
        '7',
        '--head',
        HEAD,
        '--verdict',
        'human',
        '--reason',
        'partial-build',
        '--ledger',
        '0123456789abcdef',
      ],
      deps(world()),
    ),
    UsageError,
  )
})

test('policy verb reports partial mirror gaps without changing unmatched warnings', async () => {
  const r = await main(['policy'], {
    read: () => ({ ok: true, output: 'db/001.sql\0web/App.tsx\0', exit: 0 }),
    loadConfig: () => ({
      verifyDefaults: {
        alwaysHumanPaths: ['db/**'],
        humanReview: { database: ['db/**'], ui: ['web/**'] },
      },
    }),
  })
  assert.deepEqual(r.mirrorGaps, [{ list: 'humanReview.ui', glob: 'web/**' }])
  assert.deepEqual(r.warnings, [])
})
