import test from 'node:test'
import assert from 'node:assert/strict'
import { trace, list, main } from './verify-escapes-trace.mjs'
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
      if (args[1] === 'comment') records.push({ body: args.at(-1) })
      return { status: 0, stdout: '' }
    }
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
