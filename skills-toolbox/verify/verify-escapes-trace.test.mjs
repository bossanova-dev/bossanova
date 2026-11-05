import test from 'node:test'
import assert from 'node:assert/strict'
import { trace, list, main } from './verify-escapes-trace.mjs'
import { activeCaps, capHistory, renderClearComment } from './verify-area-caps.mjs'
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
const fix = {
  ...original,
  number: 11,
  title: 'fix: defect',
  body: 'Fixes #10.',
  mergedAt: '2026-10-02T00:00:00Z',
}
function harness() {
  const writes = []
  const records = []
  const claims = new Map()
  const run = async (_cmd, args) => {
    if (args.includes('--method')) {
      const ref = args.find((arg) => arg.startsWith('ref=')).slice(4)
      const sha = args.find((arg) => arg.startsWith('sha=')).slice(4)
      if (claims.has(ref)) return { status: 1, stderr: 'already exists' }
      claims.set(ref, sha)
      writes.push(args)
      return { status: 0, stdout: JSON.stringify({ ref, object: { sha } }) }
    }
    if (args[0] === 'repo')
      return {
        status: 0,
        stdout: JSON.stringify({ nameWithOwner: 'a/b', defaultBranchRef: { name: 'main' } }),
      }
    if (args[0] === 'label' || args[0] === 'pr') {
      writes.push(args)
      if (args[1] === 'comment') records.push({ body: args.at(-1), author: { login: 'operator' } })
      return { status: 0, stdout: '' }
    }
    if (args[0] === 'api' && args[1].includes('/permission'))
      return { status: 0, stdout: JSON.stringify({ permission: 'write' }) }
    const query = args[args.indexOf('-f') + 1]
    let data
    if (query.includes('search('))
      data = {
        search: {
          nodes: [{ number: query.includes('label:boss-escape') ? 10 : 11 }],
          pageInfo: { hasNextPage: false },
        },
      }
    else {
      const number = Number(query.match(/pullRequest\(number:(\d+)/)[1])
      data = {
        repository: {
          pullRequest:
            number === 11
              ? fix
              : { ...original, comments: { nodes: records, pageInfo: { hasNextPage: false } } },
        },
      }
    }
    return { status: 0, stdout: JSON.stringify({ data }) }
  }
  return { run, writes }
}
const options = { repo: 'a/b', since: '2026-10-01', until: '2026-10-08' }
test('trace stores comments and label once; rerun and dry-run never duplicate writes', async () => {
  const h = harness()
  const result = await trace(options, { ...h, adapter: {}, now: () => new Date('2026-10-08') })
  assert.equal(result.written, 1)
  assert.equal(h.writes.length, 4)
  assert.equal((await trace(options, { ...h, adapter: {} })).existing, 1)
  assert.equal(h.writes.length, 4)
  const dry = harness()
  assert.equal((await trace({ ...options, dryRun: true }, { ...dry, adapter: {} })).links.length, 1)
  assert.equal(dry.writes.length, 0)
  assert.match(result.skipped.followups, /readDescription/)
})
test('failed reads never write and expose shared failure reason', async () => {
  const result = await trace(options, {
    run: () => ({ status: 1, stderr: 'API rate limit exceeded' }),
    adapter: {},
  })
  assert.equal(result.failures[0].reason, 'rate-limited')
  assert.equal(result.written, 0)
})
test('explicit repository casing retains canonical escape records', async () => {
  const h = harness()
  await trace(options, { ...h, adapter: {} })
  const result = await list({ repo: 'A/B', since: options.since }, h)
  assert.equal(result.escapes.length, 1)
  assert.equal(result.unreadable, 0)
  assert.equal(result.escapes[0].repo, 'a/b')
})

test('escape records from a reader or a comment without author metadata cannot impose caps', async () => {
  for (const author of [{ login: 'reader' }, undefined]) {
    const h = harness()
    await trace(options, { ...h, adapter: {} })
    const run = async (cmd, args) => {
      if (args[0] === 'api' && args[1].includes('/permission'))
        return { status: 0, stdout: JSON.stringify({ permission: 'read' }) }
      const response = await h.run(cmd, args)
      const query = args[args.indexOf('-f') + 1] ?? ''
      if (!query.includes('pullRequest(number:10)')) return response
      const data = JSON.parse(response.stdout)
      for (const comment of data.data.repository.pullRequest.comments.nodes) comment.author = author
      return { status: 0, stdout: JSON.stringify(data) }
    }
    const result = await list(options, { run })
    assert.deepEqual(result.escapes, [])
    assert.equal(result.unreadable, 1)
  }
})
test('list filters records and unknown flags return 64', async () => {
  const h = harness()
  await trace(options, { ...h, adapter: {} })
  assert.equal((await list({ repo: 'a/b', since: '2026-10-01' }, { ...h })).escapes.length, 1)
  assert.equal((await list({ repo: 'a/b', since: '2026-10-03' }, { ...h })).escapes.length, 0)
  assert.equal(
    (await list({ repo: 'a/b', since: '2026-10-01', area: 'ui' }, { ...h })).escapes.length,
    0,
  )
  assert.equal(await main(['trace', '--unknown'], { stdout: () => {}, stderr: () => {} }), 64)
})

test('judgements accept only listed overlap candidates', async () => {
  const h = harness()
  const run = async (cmd, args) => {
    const response = await h.run(cmd, args)
    if (args[0] === 'api' && response.stdout.includes('Fixes #10.'))
      response.stdout = response.stdout.replace('Fixes #10.', 'See #10.')
    return response
  }
  const result = await trace(
    {
      ...options,
      dryRun: true,
      judgements: [
        { fix: 11, original: 10, verdict: 'escape', reason: 'review confirms defect' },
        { fix: 11, original: 999, verdict: 'escape', reason: 'invented' },
        { fix: 999, original: 10, verdict: 'escape', reason: 'outside window' },
      ],
    },
    { run, adapter: {} },
  )
  assert.equal(result.candidates.length, 1)
  assert.equal(result.links.length, 1)
  assert.equal(result.links[0].basis, 'judge')
  assert.equal(result.warnings.length, 2)
})
test('missing tracker credentials skip followups and preserve direct links', async () => {
  const h = harness()
  const run = async (cmd, args) => {
    const r = await h.run(cmd, args)
    if (r.stdout.includes('Fixes #10.'))
      r.stdout = r.stdout.replace('fix: defect', '[TASK-2] fix: defect')
    return r
  }
  const result = await trace(
    { ...options, dryRun: true },
    {
      run,
      adapter: {
        readDescription: async () => {
          throw Object.assign(new Error('missing'), { code: 'TRACKER_CREDENTIALS_MISSING' })
        },
      },
    },
  )
  assert.equal(result.links.length, 1)
  assert.equal(result.skipped.followups, 'tracker credentials missing')
})
test('scan cap reports truncation, malformed comments count as unreadable', async () => {
  const h = harness()
  let searchCalls = 0
  const run = async (cmd, args) => {
    const r = await h.run(cmd, args)
    if (args[0] === 'api' && args[args.indexOf('-f') + 1].includes('search(')) {
      searchCalls++
      return {
        status: 0,
        stdout: JSON.stringify({
          data: {
            search: {
              nodes: Array.from({ length: 50 }, () => ({ number: 11 })),
              pageInfo: { hasNextPage: true, endCursor: `page${searchCalls}` },
            },
          },
        }),
      }
    }
    return r
  }
  const result = await trace({ ...options, dryRun: true }, { run, adapter: {} })
  assert.equal(result.scanned, 500)
  assert.equal(result.truncated, true)
  assert.equal(searchCalls, 10)
  const malformed = async (cmd, args) => {
    const r = await h.run(cmd, args)
    if (args[0] === 'api' && r.stdout.includes('"number":10')) {
      const parsed = JSON.parse(r.stdout)
      if (parsed.data.repository)
        parsed.data.repository.pullRequest.comments.nodes = [
          { body: '<!-- boss-escape v1 fix=11 -->\n<!-- boss-escape-record: broken -->' },
        ]
      return { status: 0, stdout: JSON.stringify(parsed) }
    }
    return r
  }
  assert.equal((await list({ repo: 'a/b', since: '2026-10-01' }, { run: malformed })).unreadable, 1)
})

test('a bare reference to an issue is a typed negative lookup, not a scan failure', async () => {
  const h = harness()
  const run = async (cmd, args) => {
    if (args[0] === 'api' && args[args.indexOf('-f') + 1].includes('pullRequest(number:10)'))
      return {
        status: 1,
        stdout: JSON.stringify({
          data: { repository: { pullRequest: null } },
          errors: [
            {
              type: 'NOT_FOUND',
              path: ['repository', 'pullRequest'],
              message: 'Could not resolve to a PullRequest with the number of 10.',
            },
          ],
        }),
        stderr: 'gh: Could not resolve to a PullRequest with the number of 10.',
      }
    return h.run(cmd, args)
  }
  const result = await trace({ ...options, dryRun: true }, { run, adapter: {} })
  assert.equal(result.scanned, 1)
  assert.deepEqual(result.failures, [])
  assert.deepEqual(result.links, [])
})

test('an explicit missing ticket is counted while unavailable tracker reads still fail', async () => {
  const h = harness()
  const run = async (cmd, args) => {
    const r = await h.run(cmd, args)
    if (r.stdout.includes('Fixes #10.'))
      r.stdout = r.stdout.replace('fix: defect', '[FOREIGN-2] fix: defect')
    return r
  }
  const missing = await trace(
    { ...options, dryRun: true },
    {
      run,
      adapter: {
        readDescription: async () => {
          throw new Error('Linear GraphQL error: Entity not found: Issue')
        },
      },
    },
  )
  assert.equal(missing.links.length, 1)
  assert.deepEqual(missing.failures, [])
  assert.equal(missing.unresolved['ticket-unresolved'], 1)
  const unavailable = await trace(
    { ...options, dryRun: true },
    {
      run,
      adapter: {
        readDescription: async () => {
          throw new Error('connection failed')
        },
      },
    },
  )
  assert.equal(unavailable.failures.length, 1)
  assert.equal(unavailable.written, 0)
})

test('trace reads the verified description from the original merge head', async () => {
  const h = harness()
  const run = async (cmd, args) => {
    const response = await h.run(cmd, args)
    const query = args[args.indexOf('-f') + 1] ?? ''
    if (!query.includes('pullRequest(number:10)')) return response
    const data = JSON.parse(response.stdout)
    data.data.repository.pullRequest.commits = {
      nodes: [
        {
          commit: {
            oid: 'abc',
            status: {
              contexts: [
                {
                  context: 'boss/verify',
                  state: 'SUCCESS',
                  ...(query.includes('description') ? { description: 'verified' } : {}),
                },
              ],
            },
          },
        },
      ],
    }
    return { status: 0, stdout: JSON.stringify(data) }
  }
  const result = await trace({ ...options, dryRun: true }, { run, adapter: {} })
  assert.equal(result.links[0].levelAtMerge, 'high')
})

test('list returns clear records, ignores ordinary comments and counts damaged clears', async () => {
  const clear = {
    v: 1,
    repo: 'a/b',
    key: 'ui',
    clearedAt: '2026-10-04T00:00:00.000Z',
    clearedBy: 'operator',
    reason: 'reviewed fixes',
    through: 11,
  }
  const comment = `<!-- boss-area-cap-clear v1 key=ui -->\n<!-- boss-area-cap-clear-record: ${JSON.stringify(clear)} -->`
  const h = harness()
  const run = async (cmd, args) => {
    if (args[1] === 'repos/a/b/collaborators/operator/permission')
      return { status: 0, stdout: JSON.stringify({ permission: 'write' }) }
    const response = await h.run(cmd, args)
    const query = args[args.indexOf('-f') + 1] ?? ''
    if (!query.includes('pullRequest(number:10)')) return response
    const data = JSON.parse(response.stdout)
    data.data.repository.pullRequest.comments.nodes = [
      { body: comment, author: { login: 'operator' }, createdAt: clear.clearedAt },
      { body: comment.replace('"v":1', '"v":2') },
      { body: 'An ordinary review comment.' },
    ]
    return { status: 0, stdout: JSON.stringify(data) }
  }
  const result = await list({ repo: 'a/b', since: '2026-10-01' }, { run })
  assert.deepEqual(result.clears, [clear])
  assert.equal(result.unreadable, 1)
  assert.deepEqual(result.escapes, [])
})

for (const attack of ['reader', 'forged-author', 'missing-metadata']) {
  test(`list rejects cap clear from ${attack}`, async () => {
    const h = harness()
    const clear = {
      v: 1,
      repo: 'a/b',
      key: 'ui',
      clearedAt: '2026-10-04T00:00:00Z',
      clearedBy: attack === 'forged-author' ? 'operator' : 'commenter',
      reason: 'clear',
      through: 11,
    }
    const run = async (cmd, args) => {
      if (args[1]?.includes('/permission'))
        return {
          status: 0,
          stdout: JSON.stringify({ permission: attack === 'reader' ? 'read' : 'write' }),
        }
      const response = await h.run(cmd, args)
      const query = args[args.indexOf('-f') + 1] ?? ''
      if (!query.includes('pullRequest(number:10)')) return response
      const data = JSON.parse(response.stdout)
      data.data.repository.pullRequest.comments.nodes = [
        {
          body: `<!-- boss-area-cap-clear v1 key=ui -->\n<!-- boss-area-cap-clear-record: ${JSON.stringify(clear)} -->`,
          ...(attack === 'missing-metadata'
            ? {}
            : { author: { login: 'commenter' }, createdAt: '2026-10-04T00:00:00Z' }),
        },
      ]
      return { status: 0, stdout: JSON.stringify(data) }
    }
    const result = await list({ repo: 'a/b', since: '2026-10-01' }, { run })
    assert.deepEqual(result.clears, [])
    assert.equal(result.unreadable, 1)
  })
}

test('clear boundaries use creation time despite later edits and a skewed operator clock', async () => {
  const createdAt = '2026-10-04T00:00:00.000Z'
  for (const clearedAt of [createdAt, '2026-10-04T00:00:30.000Z', '2099-01-01T00:00:00Z']) {
    const clear = {
      v: 1,
      repo: 'a/b',
      key: 'ui',
      clearedAt,
      clearedBy: 'operator',
      reason: 'reviewed fixes',
      through: 11,
    }
    const h = harness()
    const run = async (cmd, args) => {
      if (args[1] === 'repos/a/b/collaborators/operator/permission')
        return { status: 0, stdout: JSON.stringify({ permission: 'write' }) }
      const response = await h.run(cmd, args)
      const query = args[args.indexOf('-f') + 1] ?? ''
      if (!query.includes('pullRequest(number:10)')) return response
      const data = JSON.parse(response.stdout)
      data.data.repository.pullRequest.comments.nodes = [
        {
          body: renderClearComment(clear),
          author: { login: 'operator' },
          createdAt,
          updatedAt: '2026-10-08T00:00:00.000Z',
        },
      ]
      return { status: 0, stdout: JSON.stringify(data) }
    }
    const { clears, unreadable } = await list({ repo: 'a/b', since: '2026-10-01' }, { run })
    assert.equal(unreadable, 0)
    assert.deepEqual(clears, [{ ...clear, clearedAt: createdAt }])
    const escapes = [1, 2, 5, 6].map((day) => ({
      original: { number: day },
      fix: { number: day + 10, mergedAt: `2026-10-0${day}T00:00:00.000Z` },
      levelAtMerge: 'high',
      areas: [{ area: 'ui', paths: ['src/View.tsx'] }],
    }))
    assert.deepEqual(activeCaps({ escapes: escapes.slice(0, 2), clears }), [])
    assert.deepEqual(
      activeCaps({ escapes, clears })[0].escapes.map((e) => e.original),
      [6, 5],
    )
    const history = capHistory({ escapes, clears })
    assert.equal(history.length, 2)
    assert.equal(history[0].clearedAt, createdAt)
    assert.equal(history[1].clearedAt, undefined)
  }
})
