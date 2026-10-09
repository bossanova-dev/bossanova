import assert from 'node:assert/strict'
import { test } from 'node:test'
import {
  collectReviews,
  collectParks,
  collectCi,
  collectReverts,
  areaOf,
  main,
  fetchAgentPrs,
} from './retro-signals.mjs'
import { parseNote } from './retro-notes.mjs'

const at = '2026-10-07T00:00:00.000Z'
const author = { login: 'operator', __typename: 'User' }
const status = (context, state, description = '') => ({
  __typename: 'StatusContext',
  context,
  state,
  description,
})
function pr(number = 1, contexts = []) {
  return {
    number,
    title: 'Repair behavior',
    state: 'OPEN',
    updatedAt: at,
    headRefOid: `head${number}`,
    files: { nodes: [{ path: 'src/lib/a.js' }] },
    commits: {
      nodes: [
        {
          commit: {
            oid: `head${number}`,
            statusCheckRollup: {
              contexts: { nodes: [status('boss/build', 'SUCCESS'), ...contexts] },
            },
          },
        },
      ],
    },
    reviews: { nodes: [] },
    reviewThreads: { nodes: [] },
  }
}
const comment = (body = 'Please handle the error', who = author, file = 'src/lib/a.js') => ({
  body,
  author: who,
  path: file,
  createdAt: at,
})
const thread = (...comments) => ({ comments: { nodes: comments } })

test('review starters include the PR author, exclude replies, bots and configured agent authors', () => {
  const p = pr()
  p.author = author
  p.reviewThreads.nodes = [
    thread(comment(), comment('reply', author, 'other/a.js')),
    thread(comment('bot', { login: 'x', __typename: 'Bot' })),
    thread(comment('bot', { login: 'x[bot]' })),
    thread(comment('agent', { login: 'agent' })),
  ]
  assert.equal(collectReviews([p], { agentAuthors: ['agent'] }).notes.length, 1)
})

test('submitted reviews count changes requested with empty body and ignore empty approval or pending draft', () => {
  const p = pr()
  p.reviews.nodes = [
    { state: 'CHANGES_REQUESTED', body: '', author, submittedAt: at },
    { state: 'APPROVED', body: '', author },
    { state: 'PENDING', body: 'draft', author },
  ]
  assert.equal(collectReviews([p]).notes.length, 1)
})
test('two comments in one directory on one PR collapse; different PRs remain distinct', () => {
  const p = pr()
  p.reviewThreads.nodes = [
    thread(comment()),
    thread(comment('Another correction', author, 'src/lib/b.js')),
  ]
  const q = { ...p, number: 2 }
  assert.equal(collectReviews([p, q]).notes.length, 2)
})
test('park reasons include unknown codes, defects and the two approval paths', () => {
  const p = pr(1, [status('boss/verify', 'PENDING', 'needs human: repair-exhausted')])
  const q = pr(2, [status('boss/verify', 'FAILURE', 'defect: 1 finding(s), needs human')])
  const r = pr(3, [status('boss/verify', 'SUCCESS', 'verified (approved)')])
  const s = pr(4, [status('boss/verify', 'PENDING', 'needs human: future-reason')])
  s.state = 'MERGED'
  const t = pr(5, [status('boss/verify', 'SUCCESS', 'verified')])
  assert.deepEqual(
    collectParks([p, q, r, s, t]).notes.map((n) => n.where),
    [
      'boss/verify needs human: repair-exhausted',
      'boss/verify needs human: defect',
      'boss/verify approval: verified-approved',
      'boss/verify needs human: future-reason',
      'boss/verify approval: merged-by-human',
    ],
  )
})
test('merged approval only uses the head park, while older commits retain park occurrences', () => {
  const p = pr(1, [status('boss/verify', 'PENDING', 'needs human: ledger-open')])
  p.state = 'MERGED'
  p.headRefOid = 'new'
  p.commits.nodes.push({
    commit: {
      oid: 'new',
      statusCheckRollup: { contexts: { nodes: [status('boss/verify', 'SUCCESS', 'verified')] } },
    },
  })
  assert.equal(collectParks([p]).notes.length, 1)
})
test('three distinct PRs repeat a failing check; two, duplicate commits and boss contexts do not', () => {
  const prs = [1, 2, 3].map((n) =>
    pr(n, [
      { name: 'unit', conclusion: 'FAILURE' },
      status('status-test', 'ERROR'),
      status('boss/verify', 'FAILURE'),
      { name: 'pair', conclusion: n === 3 ? 'SUCCESS' : 'TIMED_OUT' },
    ]),
  )
  prs[0].commits.nodes.push(prs[0].commits.nodes[0])
  assert.equal(collectCi(prs).notes.length, 6)
  assert.equal(collectCi(prs.slice(0, 2)).notes.length, 0)
})
test('all supported CheckRun failures count', () => {
  assert.equal(
    collectCi(
      ['FAILURE', 'TIMED_OUT', 'STARTUP_FAILURE'].map((conclusion, i) =>
        pr(i + 1, [{ name: 'unit', conclusion }]),
      ),
    ).notes.length,
    3,
  )
})
test('areaOf uses a common directory prefix capped at three segments or root', () => {
  assert.equal(areaOf(['a/b/c/d/f.js', 'a/b/c/e/g.js']), 'a/b/c')
  assert.equal(areaOf(['a/x/c/f.js', 'a/y/c/g.js']), 'a')
  assert.equal(areaOf(['a/f.js', 'b/g.js']), '.')
  assert.equal(areaOf(['root.js']), '.')
  assert.equal(areaOf([]), '.')
})
test('pure reverts ignore non-agent and unmerged PRs', () => {
  const p = pr()
  p.state = 'MERGED'
  const q = pr(2)
  q.state = 'MERGED'
  q.commits.nodes = []
  assert.equal(
    collectReverts([
      { pr: p, at },
      { pr: q, at },
      { pr: pr(3), at },
    ]).notes.length,
    1,
  )
})
test('notes round-trip the landed parseNote statement, where and run_id contract for all collectors', () => {
  const p = pr(1, [status('boss/verify', 'PENDING', 'needs human: repair-exhausted')])
  p.reviewThreads.nodes = [thread(comment())]
  p.state = 'MERGED'
  const notes = [
    ...collectReviews([p]).notes,
    ...collectParks([p]).notes,
    ...collectCi([1, 2, 3].map((n) => pr(n, [status('test', 'FAILURE')]))).notes,
    ...collectReverts([{ pr: p, at }]).notes,
  ]
  for (const note of notes) {
    const parsed = parseNote({ body: note.body })
    assert.equal(parsed.statement, note.body.split('\n')[0])
    assert.equal(parsed.where, note.where)
    assert.equal(parsed.run_id, note.runId)
    assert.equal(note.body.split('\n').length, 5)
  }
})
test('cap is newest-first after dedupe and reports overflow; secret-shaped feedback is dropped', () => {
  const prs = Array.from({ length: 12 }, (_, i) => {
    const p = pr(i + 1)
    p.reviewThreads.nodes = [
      thread({ ...comment(), createdAt: `2026-10-${String(i + 1).padStart(2, '0')}T00:00:00Z` }),
    ]
    return p
  })
  const got = collectReviews(prs)
  assert.equal(got.notes.length, 10)
  assert.equal(got.notes[0].runId, 'pr:12')
  assert.equal(got.dropped.overCap, 2)
  const p = pr()
  p.reviewThreads.nodes = [thread(comment('token ghp_abcdefghijklmnopqrstuvwxyz0123456789AB'))]
  assert.equal(collectReviews([p]).dropped.secretShape, 1)
})

function harness({ prs = [], log = '', changedRun, watermark, bossAvailable = true } = {}) {
  const files = new Map(
    watermark === undefined
      ? []
      : [
          [
            '/git/boss-retro-signals.json',
            typeof watermark === 'string' ? watermark : JSON.stringify(watermark),
          ],
        ],
  )
  const calls = []
  const output = []
  const warnings = []
  const notes = new Map()
  let currentPr = pr()
  currentPr.state = 'MERGED'
  const json = (value) => ({ status: 0, stdout: JSON.stringify(value), stderr: '' })
  const run = (cmd, args, opts) => {
    calls.push({ cmd, args, opts })
    const override = changedRun?.(cmd, args, opts)
    if (override !== undefined) return override
    if (cmd === 'git') {
      if (args[0] === 'rev-parse' && args[1] === '--git-common-dir')
        return { status: 0, stdout: '/git' }
      if (args[0] === 'log') return { status: 0, stdout: log }
      if (args[0] === 'symbolic-ref') return { status: 0, stdout: 'refs/remotes/origin/main' }
      return { status: 0, stdout: 'sha' }
    }
    if (cmd === 'gh') {
      if (args[0] === 'auth') return { status: 0, stdout: '' }
      if (args[0] === 'repo')
        return json({ nameWithOwner: 'owner/repo', defaultBranchRef: { name: 'main' } })
      if (args[1] === 'graphql') {
        if (args.some((a) => a.includes('pullRequest(number:')))
          return json({ data: { repository: { pullRequest: currentPr } } })
        return json({
          data: { search: { nodes: prs, pageInfo: { hasNextPage: false, endCursor: null } } },
        })
      }
      if (args[1].includes('/commits/')) return json([{ number: currentPr.number, merged_at: at }])
    }
    if (cmd === '/boss') {
      const key = args[args.indexOf('--idempotency-key') + 1]
      if (notes.has(key)) return json({ id: notes.get(key), created_at: '2020-01-01T00:00:00Z' })
      notes.set(key, `note-${notes.size + 1}`)
      return json({
        id: notes.get(key),
        created_at: new Date(Math.floor(Date.now() / 1000) * 1000).toISOString(),
      })
    }
    throw new Error(`Unexpected command ${cmd} ${args}`)
  }
  const deps = {
    run,
    now: () => new Date('2026-10-08T00:00:00Z'),
    stdout: (s) => output.push(s),
    stderr: (s) => warnings.push(s),
    resolveBoss: () => ({ ok: bossAvailable, path: '/boss' }),
    fs: {
      readFileSync: (file) => {
        if (!files.has(file)) throw Object.assign(new Error('absent'), { code: 'ENOENT' })
        return files.get(file)
      },
      writeFileSync: (file, data) => files.set(file, data),
      renameSync: (from, to) => {
        files.set(to, files.get(from))
        files.delete(from)
      },
      unlinkSync: (file) => files.delete(file),
    },
  }
  const invoke = (...args) => {
    assert.equal(main(['collect', ...args], deps), 0)
    return JSON.parse(output.at(-1))
  }
  return {
    deps,
    invoke,
    files,
    calls,
    notes,
    warnings,
    output,
    setPr: (p) => {
      currentPr = p
    },
  }
}
const revertLog = (subject, body = '', sha = 'a'.repeat(40)) =>
  `${sha}\0${at}\0${subject}\0${body}\0\x1e`

for (const [shape, subject, body] of [
  ['sha', 'Revert change', `This reverts commit ${'b'.repeat(40)}`],
  ['body', 'Undo change', 'Reverts owner/repo#1'],
  ['subject', 'Revert "change (#1)"', ''],
])
  test(`revert ${shape} shape resolves and records the agent PR`, () => {
    const h = harness({ log: revertLog(subject, body) })
    const result = h.invoke('--only', 'reverts', '--repo', 'owner/repo')
    assert.equal(result.collectors.reverts.recorded, 1)
    assert.equal(
      h.calls.filter((c) => c.cmd === 'gh' && c.args[1]?.includes('/commits/')).length,
      shape === 'sha' ? 1 : 0,
    )
  })
test('revert non-agent is ignored and inspection stops after twenty occurrences', () => {
  const h = harness({
    log: Array.from({ length: 21 }, (_, i) => revertLog(`Revert "change (#${i + 1})"`)).join(''),
  })
  const p = pr()
  p.commits.nodes = []
  p.state = 'MERGED'
  h.setPr(p)
  const result = h.invoke('--only', 'reverts')
  assert.equal(result.collectors.reverts.found, 0)
  assert.equal(result.truncated, true)
  assert.equal(
    h.calls.filter((c) => c.args.some((a) => a.includes('pullRequest(number:'))).length,
    20,
  )
  assert.equal(
    JSON.parse(h.files.get('/git/boss-retro-signals.json')).collectors.reverts,
    undefined,
  )
})
test('fresh whole-second note timestamps count as recorded', (t) => {
  t.mock.method(Date, 'now', () => Date.parse('2026-10-08T00:00:00.987Z'))
  const p = pr()
  p.reviewThreads.nodes = [thread(comment())]
  const h = harness({ prs: [p] })
  const result = h.invoke('--only', 'reviews', '--since', '2026-10-01')
  assert.equal(result.collectors.reviews.recorded, 1)
  assert.equal(result.collectors.reviews.existing, 0)
})
test('overlapping windows keep the same key and return existing notes', () => {
  const p = pr()
  p.reviewThreads.nodes = [thread(comment())]
  const h = harness({ prs: [p] })
  const a = h.invoke('--only', 'reviews', '--since', '2026-10-01')
  const b = h.invoke('--only', 'reviews', '--since', '2026-10-02')
  assert.equal(a.collectors.reviews.recorded, 1)
  assert.equal(b.collectors.reviews.existing, 1)
  assert.equal(h.notes.size, 1)
})
test('dry run needs no boss, writes neither notes nor watermark, and clamps the window', () => {
  const p = pr()
  p.reviewThreads.nodes = [thread(comment())]
  const h = harness({
    prs: [p],
    watermark: { v: 1, collectors: { reviews: '2026-10-01T00:00:00Z' } },
    bossAvailable: false,
  })
  const before = h.files.get('/git/boss-retro-signals.json')
  const result = h.invoke('--dry-run', '--only', 'reviews', '--since', '2020-01-01')
  assert.equal(result.window.start, '2026-09-08T00:00:00.000Z')
  assert.equal(result.planned.length, 1)
  assert.equal(h.notes.size, 0)
  assert.equal(h.files.get('/git/boss-retro-signals.json'), before)
})
test('watermarks filter each collector separately and advance on complete successful writes', () => {
  const p = pr()
  p.reviewThreads.nodes = [thread(comment())]
  const h = harness({
    prs: [p],
    watermark: {
      v: 1,
      collectors: { reviews: '2026-10-07T01:00:00Z', parks: '2026-10-01T00:00:00Z' },
    },
  })
  const result = h.invoke('--only', 'reviews,parks')
  assert.equal(result.window.start, '2026-10-01T00:00:00.000Z')
  assert.equal(result.collectors.reviews.found, 0)
  const marks = JSON.parse(h.files.get('/git/boss-retro-signals.json'))
  assert.equal(marks.collectors.reviews, '2026-10-08T00:00:00.000Z')
  assert.equal(marks.collectors.parks, '2026-10-08T00:00:00.000Z')
})
test('malformed watermark warns and uses fourteen-day default', () => {
  const h = harness({ watermark: 'broken' })
  assert.equal(h.invoke('--only', 'ci').window.start, '2026-09-24T00:00:00.000Z')
  assert.ok(h.warnings.some((s) => s.includes('malformed watermark')))
})
test('GraphQL search stops after 100 PRs and blocks watermark advancement', () => {
  let pages = 0
  const h = harness({
    changedRun: (cmd, args) =>
      cmd === 'gh' && args[1] === 'graphql'
        ? {
            status: 0,
            stdout: JSON.stringify({
              data: {
                search: {
                  nodes: Array.from({ length: 20 }, (_, i) => pr(++pages * 100 + i)),
                  pageInfo: { hasNextPage: true, endCursor: `cursor-${pages}` },
                },
              },
            }),
          }
        : undefined,
  })
  const result = h.invoke('--only', 'parks')
  assert.equal(result.truncated, true)
  assert.equal(h.calls.filter((c) => c.args[1] === 'graphql').length, 5)
  assert.equal(JSON.parse(h.files.get('/git/boss-retro-signals.json')).collectors.parks, undefined)
})
test('fetchAgentPrs drops PRs without a successful build receipt and selects only thread starters', () => {
  const p = pr()
  const q = pr(2)
  q.commits.nodes = []
  const h = harness({ prs: [p, q] })
  assert.equal(fetchAgentPrs({ repo: 'owner/repo', start: at }, h.deps).prs.length, 1)
  assert.ok(
    h.calls.find((c) => c.args[1] === 'graphql').args.some((a) => a.includes('comments(first:1)')),
  )
})
test('nested GraphQL caps report truncation and preserve watermark', () => {
  const p = pr()
  p.reviewThreads.pageInfo = { hasNextPage: true }
  const h = harness({ prs: [p] })
  assert.equal(h.invoke('--only', 'reviews').truncated, true)
  assert.equal(
    JSON.parse(h.files.get('/git/boss-retro-signals.json')).collectors.reviews,
    undefined,
  )
})
for (const [name, response, reason] of [
  ['missing', { status: null, error: { code: 'ENOENT' } }, 'gh-unavailable'],
  ['authentication', { status: 1, stderr: 'not logged in' }, 'gh-unauthenticated'],
  ['timeout', { status: null, error: { code: 'ETIMEDOUT' } }, 'timeout'],
])
  test(`${name} gh read never fails the retro and reports ${reason}`, () => {
    const h = harness({
      changedRun: (cmd, args) => (cmd === 'gh' && args[0] === 'auth' ? response : undefined),
    })
    assert.equal(h.invoke('--only', 'reviews').collectors.reviews.reason, reason)
  })
for (const [name, payload, reason] of [
  ['errors', { errors: [{ message: 'no access' }] }, 'graphql-error'],
  ['rate limit', { errors: [{ message: 'rate limit exceeded' }] }, 'rate-limited'],
  ['malformed', { data: { search: {} } }, 'graphql-error'],
])
  test(`GraphQL ${name} skips shared collectors while reverts still runs`, () => {
    const h = harness({
      changedRun: (cmd, args) =>
        cmd === 'gh' && args[1] === 'graphql'
          ? { status: 0, stdout: JSON.stringify(payload) }
          : undefined,
    })
    const result = h.invoke()
    for (const name of ['reviews', 'parks', 'ci'])
      assert.equal(result.collectors[name].reason, reason)
    assert.equal(result.collectors.reverts.status, 'ok')
  })
test('missing base and git are classified without failing the retro', () => {
  for (const [response, reason] of [
    [{ status: 128 }, 'no-base-ref'],
    [{ status: null, error: { code: 'ENOENT' } }, 'git-unavailable'],
  ]) {
    const h = harness({
      changedRun: (cmd, args) => (cmd === 'git' && args[1] === '--verify' ? response : undefined),
    })
    assert.equal(h.invoke('--only', 'reverts').collectors.reverts.reason, reason)
  }
})
test('missing boss is a skip; note write throws are isolated and prevent watermark advance', () => {
  const p = pr()
  p.reviewThreads.nodes = [thread(comment())]
  const absent = harness({ prs: [p], bossAvailable: false })
  assert.equal(absent.invoke('--only', 'reviews').collectors.reviews.reason, 'boss-unavailable')
  const h = harness({
    prs: [p],
    changedRun: (cmd) => {
      if (cmd === '/boss') throw new Error('offline')
    },
  })
  const result = h.invoke('--only', 'reviews,parks')
  assert.deepEqual(result.failures, [{ collector: 'reviews', reason: 'note-write-failed' }])
  assert.equal(result.collectors.parks.status, 'ok')
  assert.equal(
    JSON.parse(h.files.get('/git/boss-retro-signals.json')).collectors.reviews,
    undefined,
  )
})
test('throwing collector does not prevent another collector from completing', () => {
  const h = harness()
  h.deps.collectors = {
    reviews: () => {
      throw new Error('bad collector')
    },
  }
  const result = h.invoke('--only', 'reviews,parks')
  assert.equal(result.failures[0].collector, 'reviews')
  assert.equal(result.collectors.parks.status, 'ok')
})
test('overflow is reported while completed planned writes permit watermark advancement', () => {
  const prs = Array.from({ length: 11 }, (_, i) => {
    const p = pr(i + 1)
    p.reviewThreads.nodes = [thread(comment())]
    return p
  })
  const h = harness({ prs })
  const result = h.invoke('--only', 'reviews')
  assert.equal(result.collectors.reviews.dropped.overCap, 1)
  assert.equal(
    JSON.parse(h.files.get('/git/boss-retro-signals.json')).collectors.reviews,
    '2026-10-08T00:00:00.000Z',
  )
})
test('unknown collector names warn without failing and unsupported command still returns zero JSON', () => {
  const h = harness()
  assert.equal(h.invoke('--only', 'unknown,ci').collectors.ci.status, 'ok')
  assert.ok(h.warnings.length)
  assert.equal(main(['bad'], h.deps), 0)
  assert.deepEqual(JSON.parse(h.output.at(-1)).failures, [{ reason: 'collect-failed' }])
})

test('secret-shaped text beyond the excerpt is dropped before truncation', () => {
  const p = pr()
  p.reviewThreads.nodes = [thread(comment('x'.repeat(250) + ' token=supersecretvalue'))]
  assert.equal(collectReviews([p]).dropped.secretShape, 1)
})
test('window filters old and future events and excludes unsubmitted reviews', () => {
  const p = pr()
  p.reviewThreads.nodes = [
    thread({ ...comment(), createdAt: '2026-10-01T00:00:00Z' }),
    thread({ ...comment('future', author, 'future/a.js'), createdAt: '2026-10-09T00:00:00Z' }),
  ]
  assert.equal(
    collectReviews([p], { start: '2026-10-02T00:00:00Z', end: '2026-10-08T00:00:00Z' }).found,
    0,
  )
  p.updatedAt = '2026-10-01T00:00:00Z'
  assert.equal(collectParks([p], { start: at }).found, 0)
})
test('git log stops at five hundred base commits and preserves its watermark', () => {
  const h = harness({
    log: Array.from({ length: 500 }, () => revertLog('ordinary change')).join(''),
  })
  const result = h.invoke('--only', 'reverts')
  assert.equal(result.truncated, true)
  const call = h.calls.find((c) => c.args[0] === 'log')
  assert.equal(call.args[call.args.indexOf('-n') + 1], '500')
  assert.equal(
    JSON.parse(h.files.get('/git/boss-retro-signals.json')).collectors.reverts,
    undefined,
  )
})
test('failed or malformed note writes cannot advance the watermark', () => {
  const p = pr()
  p.reviewThreads.nodes = [thread(comment())]
  for (const response of [
    { status: 1, stderr: 'offline' },
    { status: 0, stdout: '{}' },
    { status: 0, stdout: 'malformed' },
  ]) {
    const h = harness({ prs: [p], changedRun: (cmd) => (cmd === '/boss' ? response : undefined) })
    assert.equal(h.invoke('--only', 'reviews').failures.length, 1)
    assert.equal(
      JSON.parse(h.files.get('/git/boss-retro-signals.json')).collectors.reviews,
      undefined,
    )
  }
})
test('atomic watermark failure warns without failing the collector or leaving a temp file', () => {
  const h = harness()
  h.deps.fs.renameSync = () => {
    throw new Error('permission denied')
  }
  assert.equal(h.invoke('--only', 'parks').collectors.parks.status, 'ok')
  assert.equal(h.files.size, 0)
  assert.ok(h.warnings.some((s) => s.includes('watermark write failed')))
})
test('subprocesses have a bounded timeout and unknown repository is a closed skip', () => {
  const h = harness({
    changedRun: (cmd, args) =>
      cmd === 'gh' && args[0] === 'repo' ? { status: 0, stdout: '{}' } : undefined,
  })
  assert.equal(h.invoke('--only', 'ci').collectors.ci.reason, 'no-repo')
  assert.ok(h.calls.every((c) => c.opts.timeout === 30000))
})
