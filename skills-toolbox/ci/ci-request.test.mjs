import test from 'node:test'
import assert from 'node:assert/strict'
import { main, exitCodeFor } from './ci-request.mjs'

const head = 'a'.repeat(40)
const pr = {
  number: 7,
  state: 'OPEN',
  isDraft: true,
  mergeable: 'MERGEABLE',
  headRefOid: head,
  headRefName: 'feature',
  baseRefName: 'main',
}
const run = (extra = {}) => ({
  id: 11,
  path: '.github/workflows/heavy.yml',
  head_sha: head,
  event: 'workflow_dispatch',
  status: 'completed',
  conclusion: 'success',
  ...extra,
})

function harness({
  config = { ci: { request: { workflows: ['heavy.yml'] } } },
  view = pr,
  runs = [],
  confirmed = [run()],
  jobs = [],
  dispatchError = null,
} = {}) {
  const calls = []
  let dispatched = false
  let time = 0
  const deps = {
    loadConfig: () => config,
    now: () => time,
    sleep: async (ms) => {
      time += ms
    },
    gh: async (args) => {
      calls.push(args)
      if (args[0] === 'pr') return typeof view === 'function' ? view() : view
      if (args[0] === 'repo') return { nameWithOwner: 'owner/repo' }
      if (args[0] === 'workflow') {
        if (dispatchError) throw new Error(dispatchError)
        dispatched = true
        return ''
      }
      if (args[1].includes('/jobs')) return { jobs }
      return { workflow_runs: dispatched ? confirmed : runs }
    },
  }
  return {
    deps,
    calls,
    request: (args = []) =>
      main(['request', '--pr', '7', '--settle-ms', '10', '--confirm-ms', '20', ...args], deps),
  }
}

test('unconfigured requests make no GitHub calls', async () => {
  for (const config of [{}, { ci: { request: { workflows: [] } } }]) {
    const h = harness({ config })
    assert.equal((await h.request()).outcome, 'not-configured')
    assert.deepEqual(h.calls, [])
  }
})

test('closed, non-draft, conflicting and moved PRs never dispatch', async () => {
  for (const [view, args, outcome] of [
    [{ ...pr, state: 'CLOSED' }, [], 'skipped-not-open'],
    [{ ...pr, isDraft: false }, [], 'skipped-non-draft'],
    [{ ...pr, mergeable: 'CONFLICTING' }, [], 'skipped-conflicting'],
    [pr, ['--head', 'b'.repeat(40)], 'head-moved'],
  ]) {
    const h = harness({ view })
    assert.equal((await h.request(args)).outcome, outcome)
    assert.ok(!h.calls.some((a) => a[0] === 'workflow'))
  }
})

test('dispatch is confirmed on the requested head with workflow inputs', async () => {
  const h = harness()
  const result = await h.request(['--head', head, '--repo', 'owner/repo'])
  assert.equal(result.outcome, 'requested')
  assert.deepEqual(result.workflows, [{ workflow: 'heavy.yml', outcome: 'requested', runId: 11 }])
  assert.deepEqual(
    h.calls.find((a) => a[0] === 'workflow'),
    [
      'workflow',
      'run',
      'heavy.yml',
      '--repo',
      'owner/repo',
      '--ref',
      'feature',
      '-f',
      'pr=7',
      '-f',
      'base=main',
    ],
  )
})

test('existing dispatch and genuine pull request runs cover a head', async () => {
  for (const [runs, jobs] of [
    [[run()], []],
    [[run({ event: 'pull_request' })], [{ conclusion: 'skipped' }, { conclusion: 'success' }]],
    [
      [run({ event: 'pull_request', status: 'in_progress', conclusion: null })],
      [{ conclusion: null }],
    ],
  ]) {
    const h = harness({ runs, jobs })
    const result = await h.request()
    assert.equal(result.outcome, 'already-covered')
    assert.equal(result.workflows[0].runId, 11)
    assert.ok(!h.calls.some((a) => a[0] === 'workflow'))
  }
})

test('skipped pull request and cancelled dispatch runs do not cover', async () => {
  for (const runs of [[run({ event: 'pull_request' })], [run({ conclusion: 'cancelled' })]]) {
    const h = harness({ runs, jobs: [{ conclusion: 'skipped' }], confirmed: [run({ id: 12 })] })
    assert.equal((await h.request()).outcome, 'requested')
  }
})

test('queued runs settle before deciding to dispatch', async () => {
  const h = harness({ runs: [run({ event: 'pull_request', status: 'queued', conclusion: null })] })
  let reads = 0
  const gh = h.deps.gh
  h.deps.gh = async (args) =>
    args[1]?.includes('/jobs') ? { jobs: ++reads > 1 ? [{ conclusion: null }] : [] } : gh(args)
  assert.equal((await h.request()).outcome, 'already-covered')
  assert.ok(reads > 1)
})

test('confirmation distinguishes moved head, missing runs and dispatch failure', async () => {
  for (const [options, outcome] of [
    [{ confirmed: [run({ head_sha: 'b'.repeat(40) })] }, 'head-moved'],
    [{ confirmed: [] }, 'unconfirmed'],
    [{ dispatchError: 'first line\npermission denied' }, 'dispatch-failed'],
  ]) {
    const h = harness(options)
    const result = await h.request()
    assert.equal(result.outcome, outcome)
    assert.equal(exitCodeFor(result.outcome), 3)
  }
})

test('a moved branch with no returned run is detected by fresh PR reads', async () => {
  const h = harness({ confirmed: [] })
  const gh = h.deps.gh
  let views = 0
  h.deps.gh = async (args) =>
    args[0] === 'pr' && ++views > 1 ? { ...pr, headRefOid: 'b'.repeat(40) } : gh(args)
  assert.equal((await h.request()).outcome, 'head-moved')
})

test('run and job pagination cannot hide coverage', async () => {
  const h = harness()
  h.deps.gh = async (args) => {
    h.calls.push(args)
    if (args[0] === 'pr') return pr
    if (args[0] === 'repo') return { nameWithOwner: 'owner/repo' }
    if (args[1].includes('/jobs'))
      return {
        jobs: args[1].endsWith('page=2')
          ? [{ conclusion: 'success' }]
          : Array.from({ length: 100 }, () => ({ conclusion: 'skipped' })),
      }
    return {
      workflow_runs: args[1].endsWith('page=2')
        ? [run({ event: 'pull_request' })]
        : Array.from({ length: 100 }, () => run({ path: '.github/workflows/other.yml' })),
    }
  }
  assert.equal((await h.request()).outcome, 'already-covered')
})

test('unknown mergeability settles then permits dispatch', async () => {
  const h = harness({ view: { ...pr, mergeable: 'UNKNOWN' } })
  assert.equal((await h.request()).outcome, 'requested')
  assert.ok(h.calls.filter((a) => a[0] === 'pr').length > 1)
})

test('usage errors reject unknown options and unsafe inputs', async () => {
  for (const args of [
    ['unknown'],
    ['request'],
    ['request', '--pr', '0'],
    ['request', '--pr', '1', '--wat'],
    ['request', '--pr', '1', '--confirm-ms', '-1'],
  ]) {
    await assert.rejects(main(args, harness().deps))
  }
})

test('outcome exit codes distinguish skips from required action', () => {
  for (const outcome of [
    'requested',
    'already-covered',
    'not-configured',
    'skipped-not-open',
    'skipped-non-draft',
  ])
    assert.equal(exitCodeFor(outcome), 0)
  for (const outcome of ['skipped-conflicting', 'head-moved', 'unconfirmed', 'dispatch-failed'])
    assert.equal(exitCodeFor(outcome), 3)
})

test('configured coverage jobs exclude successful probes and skipped heavy jobs', async () => {
  const config = {
    ci: {
      request: {
        workflows: ['heavy.yml'],
        coverageJobs: { 'heavy.yml': ['test'] },
      },
    },
  }
  for (const conclusion of ['skipped', 'success', null]) {
    const h = harness({
      config,
      runs: [run({ event: 'pull_request' })],
      jobs: [
        { name: 'roadmap-only', conclusion: 'success' },
        { name: 'test', conclusion },
      ],
      confirmed: [run({ id: 12 })],
    })
    assert.equal(
      (await h.request()).outcome,
      conclusion === 'skipped' ? 'requested' : 'already-covered',
    )
  }
  const h = harness({
    config,
    runs: [run({ event: 'pull_request' })],
    jobs: [{ name: 'dedup', conclusion: 'success' }],
    confirmed: [run({ id: 12 })],
  })
  assert.equal((await h.request()).outcome, 'requested')
})
