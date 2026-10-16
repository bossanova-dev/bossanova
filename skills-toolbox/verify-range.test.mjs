import { test } from 'node:test'
import assert from 'node:assert/strict'
import {
  PR_FILES_CAP,
  readChangedPaths,
  resolveApprovalAnchor,
  readApprovalAnchor,
  COMPARE_FILE_CAP,
  readCompareFiles,
  prLevelDelta,
  selectChangedRange,
} from './verify-range.mjs'
const HEAD = 'a'.repeat(40)
const BASE = 'b'.repeat(40)
const REPO = 'o/r'
const NOW = Date.parse('2026-10-06T12:00:00Z')
const iso = (msAgo) => new Date(NOW - msAgo).toISOString()
const unrevoked = () => ({ readable: true, revoked: () => false })
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
  assert.equal(readApprovalAnchor(w.read, REPO, 7, unrevoked).sha, ANCHOR)
  assert.equal(w.graphqlReads().length, 1)
  const args = w.graphqlReads()[0].args
  assert.ok(args.includes('owner=o') && args.includes('name=r') && args.includes('number=7'))
  const query = args[args.indexOf('-f') + 1]
  assert.ok(query.startsWith('query='))
  assert.ok(query.includes('boss/verify'), 'the status context is named in the query')

  const failed = world({ graphql: null })
  assert.deepEqual(readApprovalAnchor(failed.read, REPO, 7, unrevoked), {
    sha: null,
    why: 'anchor-unreadable',
  })
  // With no repo the gh placeholders stand in for owner and name.
  const placeholder = world()
  readApprovalAnchor(placeholder.read, '', 7, unrevoked)
  const pargs = placeholder.graphqlReads()[0].args
  assert.ok(pargs.includes('owner={owner}') && pargs.includes('name={repo}'))
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

// A local recording reader keeps the range subsystem tests independent of the judge harness.
function world({ graphql = anchorPayload(), compare = {}, diff = ['src/full.go'] } = {}) {
  const calls = []
  const read = (command, args) => {
    calls.push({ command, args })
    let payload
    if (args[1] === 'graphql') payload = graphql
    else if (args[1].includes('/compare/')) {
      const side = compare[args[1].split('...')[1]]
      payload = Array.isArray(side) ? { files: side } : side
    } else payload = diff === null ? null : [diff.map((filename) => ({ filename }))]
    return { ok: payload != null, payload }
  }
  return { read, calls, graphqlReads: () => calls.filter((c) => c.args[1] === 'graphql') }
}

test('readApprovalAnchor requires revocations and preserves GraphQL-before-comments order', () => {
  const w = world({ graphql: approvedAt(ANCHOR) })
  assert.throws(() => readApprovalAnchor(w.read, REPO, 7), TypeError)
  assert.equal(w.calls.length, 0)
  const revoked = () => {
    assert.equal(w.calls.length, 1)
    return { readable: true, revoked: (sha) => sha === ANCHOR }
  }
  assert.deepEqual(readApprovalAnchor(w.read, REPO, 7, revoked), { sha: null, why: 'no-anchor' })
  assert.deepEqual(
    readApprovalAnchor(w.read, REPO, 7, () => ({ readable: false })),
    {
      sha: null,
      why: 'comments-unreadable',
    },
  )
  const failed = world({ graphql: null })
  assert.deepEqual(
    readApprovalAnchor(failed.read, REPO, 7, () =>
      assert.fail('revocations read after failed GraphQL'),
    ),
    {
      sha: null,
      why: 'anchor-unreadable',
    },
  )
})

for (const [name, payload, expected] of [
  ['read failure', null, { files: null, why: 'compare-unreadable' }],
  ['missing files', {}, { files: null, why: 'compare-unreadable' }],
  [
    'file cap',
    { files: Array(COMPARE_FILE_CAP).fill(UI_FILE) },
    { files: null, why: 'compare-file-cap' },
  ],
  [
    'under cap',
    { files: Array(COMPARE_FILE_CAP - 1).fill(UI_FILE) },
    { files: Array(COMPARE_FILE_CAP - 1).fill(UI_FILE) },
  ],
])
  test(`readCompareFiles: ${name}`, () => {
    const w = world({ compare: { [HEAD]: payload } })
    assert.deepEqual(readCompareFiles(w.read, REPO, BASE, HEAD), expected)
    assert.deepEqual(
      w.calls.map((c) => c.args),
      [['api', `repos/o/r/compare/${BASE}...${HEAD}`]],
    )
  })

const fullRead = 'repos/o/r/pulls/7/files?per_page=100'
const anchorRead = `repos/o/r/compare/${BASE}...${ANCHOR}`
const headRead = `repos/o/r/compare/${BASE}...${HEAD}`
const cappedFiles = Array(COMPARE_FILE_CAP).fill(UI_FILE)
for (const scenario of [
  { name: 'no-base-sha', baseSha: null, why: 'no-base-sha', reads: [fullRead] },
  {
    name: 'anchor-unreadable',
    graphql: null,
    why: 'anchor-unreadable',
    reads: ['graphql', fullRead],
  },
  {
    name: 'comments-unreadable',
    readRevocations: () => ({ readable: false }),
    why: 'comments-unreadable',
    reads: ['graphql', fullRead],
  },
  { name: 'no-anchor', graphql: anchorPayload(), why: 'no-anchor', reads: ['graphql', fullRead] },
  {
    name: 'anchor equals head case-insensitively',
    headSha: ANCHOR.toUpperCase(),
    delta: true,
    paths: [],
    reads: ['graphql'],
  },
  {
    name: 'anchor compare unreadable',
    compare: {},
    why: 'compare-unreadable',
    anchorSha: ANCHOR,
    reads: ['graphql', anchorRead, fullRead],
  },
  {
    name: 'anchor compare capped',
    compare: { [ANCHOR]: cappedFiles },
    why: 'compare-file-cap',
    anchorSha: ANCHOR,
    reads: ['graphql', anchorRead, fullRead],
  },
  {
    name: 'head compare unreadable',
    compare: { [ANCHOR]: [UI_FILE] },
    why: 'compare-unreadable',
    anchorSha: ANCHOR,
    reads: ['graphql', anchorRead, headRead, fullRead],
  },
  {
    name: 'head compare capped',
    compare: { [ANCHOR]: [UI_FILE], [HEAD]: cappedFiles },
    why: 'compare-file-cap',
    anchorSha: ANCHOR,
    reads: ['graphql', anchorRead, headRead, fullRead],
  },
  {
    name: 'delta never reads full diff',
    compare: { [ANCHOR]: [UI_FILE], [HEAD]: [UI_FILE, TEST_FILE] },
    delta: true,
    paths: ['src/a_test.go'],
    reads: ['graphql', anchorRead, headRead],
  },
  {
    name: 'full diff unreadable',
    graphql: null,
    diff: null,
    why: 'anchor-unreadable',
    paths: null,
    reads: ['graphql', fullRead],
  },
])
  test(`selectChangedRange: ${scenario.name}`, () => {
    const w = world({ graphql: approvedAt(ANCHOR), ...scenario })
    let revocationReads = 0
    const readRevocations = () => {
      assert.equal(w.calls.at(-1).args[1], 'graphql', 'GraphQL precedes revocations')
      revocationReads++
      return scenario.readRevocations ? scenario.readRevocations() : unrevoked()
    }
    const result = selectChangedRange(w.read, {
      repo: REPO,
      pr: 7,
      baseSha: BASE,
      headSha: HEAD,
      ...scenario,
      readRevocations,
    })
    assert.deepEqual(
      result.paths,
      Object.hasOwn(scenario, 'paths') ? scenario.paths : ['src/full.go'],
    )
    assert.deepEqual(
      result.range,
      scenario.delta
        ? { kind: 'delta', anchorSha: ANCHOR }
        : { kind: 'full', anchorSha: scenario.anchorSha ?? null, why: scenario.why },
    )
    assert.deepEqual(
      w.calls.map((c) => c.args[1]),
      scenario.reads,
    )
    assert.equal(
      revocationReads,
      scenario.reads.includes('graphql') && scenario.graphql !== null ? 1 : 0,
    )
  })
