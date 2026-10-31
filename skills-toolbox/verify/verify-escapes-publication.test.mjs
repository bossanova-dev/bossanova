import test from 'node:test'
import assert from 'node:assert/strict'
import { trace, main } from './verify-escapes-trace.mjs'

const options = { repo: 'a/b', since: '2026-10-01', until: '2026-10-08' }
const claimRef = 'refs/boss-escape-claims/v1/10/11'
const original = {
  number: 10,
  title: 'feature',
  body: '',
  mergedAt: '2026-10-01T00:00:00Z',
  baseRefName: 'main',
  headRefOid: 'abc',
  files: { nodes: [{ path: 'src/a' }], pageInfo: { hasNextPage: false } },
  comments: { nodes: [], pageInfo: { hasNextPage: false } },
  commits: { nodes: [] },
}
/** A shared server, with a barrier forcing both clients to finish stale reads. */
function server({ clients = 1, fixes = [11], claimFailure, readFailure, commentFailure } = {}) {
  const claims = new Map(),
    comments = [],
    writes = []
  let reads = 0,
    release
  const barrier = new Promise((resolve) => {
    release = resolve
  })
  const ok = (value) => ({ status: 0, stdout: JSON.stringify(value) })
  const run = async (_cmd, args) => {
    if (args[0] === 'repo') return ok({ nameWithOwner: 'a/b', defaultBranchRef: { name: 'main' } })
    if (args.includes('--method')) {
      writes.push(args)
      if (claimFailure) return claimFailure
      const ref = args.find((a) => a.startsWith('ref=')).slice(4)
      if (claims.has(ref)) return { status: 1, stderr: 'HTTP 422 already exists' }
      const claim = { ref, object: { sha: 'abc' } }
      claims.set(ref, claim)
      return ok(claim)
    }
    if (args[0] === 'api' && args[1] !== 'graphql') {
      if (readFailure) return readFailure
      const ref = `refs/${args[1].split('/git/ref/')[1]}`
      return claims.has(ref) ? ok(claims.get(ref)) : { status: 1, stderr: 'HTTP 404' }
    }
    if (args[0] === 'label' || args[0] === 'pr') {
      writes.push(args)
      if (args[1] === 'comment') {
        if (commentFailure === 'rejected') return { status: 1, stderr: 'HTTP 403' }
        comments.push({ body: args.at(-1) })
        if (commentFailure === 'lost-response') return { status: 1, stderr: 'timeout' }
      }
      return ok({})
    }
    const query = args.at(-1)
    if (query.includes('search('))
      return ok({
        data: {
          search: {
            nodes: fixes.map((number) => ({ number })),
            pageInfo: { hasNextPage: false },
          },
        },
      })
    const number = Number(query.match(/pullRequest\(number:(\d+)/)[1])
    const pr =
      number === 10
        ? { ...original, comments: { ...original.comments, nodes: structuredClone(comments) } }
        : {
            ...original,
            number,
            title: 'fix: defect',
            body: 'Fixes #10.',
            mergedAt: '2026-10-02T00:00:00Z',
          }
    if (number === 10 && reads++ < clients) {
      if (reads === clients) release()
      await barrier
    }
    return ok({ data: { repository: { pullRequest: pr } } })
  }
  return { run, claims, comments, writes }
}
const invoke = (s, opts = options) => trace(opts, { run: s.run, adapter: {} })

test('two complete stale-snapshot traces share one create-only publication fence', async () => {
  const s = server({ clients: 2 })
  const results = await Promise.all([invoke(s), invoke(s)])
  assert.equal(s.comments.length, 1)
  assert.equal(s.claims.size, 1)
  assert.equal(
    results.reduce((sum, r) => sum + r.written, 0),
    1,
  )
  assert.equal(
    results.reduce((sum, r) => sum + r.existing + r.pending.length, 0),
    1,
  )
  assert.equal((await invoke(s)).existing, 1)
})
test('an existing claim without a marker remains pending and makes CLI fail', async () => {
  const s = server()
  s.claims.set(claimRef, { ref: claimRef, object: { sha: 'abc' } })
  const result = await invoke(s)
  assert.equal(result.failures[0].reason, 'publication-pending')
  assert.equal(result.pending[0].ref, claimRef)
  assert.equal(s.comments.length, 0)
  assert.equal(
    await main(['trace', '--repo', 'a/b', '--since', '2026-10-01', '--until', '2026-10-08'], {
      run: s.run,
      adapter: {},
      stdout: () => {},
      stderr: () => {},
    }),
    1,
  )
})
test('failed or ambiguous claim creation and unavailable claim reads never publish', async () => {
  for (const stderr of ['HTTP 403', 'timeout', 'HTTP 422']) {
    const s = server({
      claimFailure: { status: 1, stderr },
      readFailure: { status: 1, stderr: 'HTTP 503' },
    })
    assert.equal((await invoke(s)).failures[0].reason, 'claim-unavailable')
    assert.equal(s.comments.length, 0)
    assert.equal(s.writes.length, 1)
  }
})
test('unknown create outcome with a visible claim never grants ownership', async () => {
  const s = server({ claimFailure: { status: 1, stderr: 'timeout' } })
  s.claims.set(claimRef, { ref: claimRef, object: { sha: 'abc' } })
  assert.equal((await invoke(s)).failures[0].reason, 'publication-pending')
  assert.equal(s.comments.length, 0)
})
test('claim conflict re-reads the winner marker after a stale scan', async () => {
  const s = server()
  s.claims.set(claimRef, { ref: claimRef, object: { sha: 'abc' } })
  const run = async (cmd, args) => {
    if (args[0] === 'api' && args[1].includes('/git/ref/'))
      s.comments.push({ body: '<!-- boss-escape v1 fix=11 -->' })
    return s.run(cmd, args)
  }
  const result = await trace(options, { run, adapter: {} })
  assert.equal(result.existing, 1)
  assert.deepEqual(result.pending, [])
  assert.deepEqual(result.failures, [])
  assert.equal(s.writes.filter((a) => a[1] === 'comment').length, 0)
})
test('malformed successful claim response cannot authorize a comment', async () => {
  const s = server({ claimFailure: { status: 0, stdout: '{}' } })
  assert.equal((await invoke(s)).failures[0].reason, 'claim-unavailable')
  assert.equal(s.comments.length, 0)
})
test('rejected or ambiguous publication retains claim and does not repost', async () => {
  for (const commentFailure of ['rejected', 'lost-response']) {
    const s = server({ commentFailure })
    assert.equal((await invoke(s)).failures[0].reason, 'publication-indeterminate')
    assert.equal(s.claims.size, 1)
    const retry = await invoke(s)
    assert.equal(retry.written, 0)
    assert.equal(s.comments.length, commentFailure === 'lost-response' ? 1 : 0)
    assert.equal(s.writes.filter((a) => a[1] === 'comment').length, 1)
  }
})
test('dry-run creates no claim and separate fixes get separate claims', async () => {
  const s = server({ fixes: [11, 12] })
  assert.equal((await invoke(s, { ...options, dryRun: true })).links.length, 2)
  assert.equal(s.writes.length, 0)
  assert.equal(s.claims.size, 0)
  assert.equal((await invoke(s)).written, 2)
  assert.equal(s.claims.size, 2)
  assert.equal(s.comments.length, 2)
})
