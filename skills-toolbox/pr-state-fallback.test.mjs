import assert from 'node:assert/strict'
import test from 'node:test'
import { ghCheckReader, daemonFirstReader, main } from './pr-state.mjs'

const head = 'a'.repeat(40)
const refused = { status: 1, stderr: 'API rate limit exceeded' }
const json = (value) => ({ status: 0, stdout: JSON.stringify(value) })
const degraded = { transport: 'core', reason: 'graphql-rate-limited' }
const pull = {
  number: 7,
  state: 'open',
  head: { sha: head, ref: 'feature' },
  base: { ref: 'main' },
  mergeable: null,
}
function fixture({ view = refused, restFailure = null, runs = json([]), suites = null } = {}) {
  const calls = []
  const deps = {
    env: { GH_REPO: 'ghe.example.test/acme/widgets' },
    resolveBoss: () => ({ ok: false }),
    spawn(cmd, args, options) {
      assert.equal(cmd, 'gh')
      calls.push({ args, options })
      if (args[0] === 'pr') return view
      if (args[0] === 'run') return runs
      if (args[1] === 'graphql') return json(suites)
      assert.equal(args[args.indexOf('--hostname') + 1], 'ghe.example.test')
      if (restFailure) return restFailure
      const path = args[1]
      return json(
        path.includes('/pulls/')
          ? pull
          : path.includes('/check-runs?')
            ? [
                {
                  total_count: 1,
                  check_runs: [
                    {
                      name: 'test',
                      status: 'completed',
                      conclusion: 'success',
                      check_suite: { id: 3 },
                    },
                  ],
                },
              ]
            : path.includes('/actions/runs?')
              ? [
                  {
                    total_count: 1,
                    workflow_runs: [
                      {
                        name: 'CI',
                        head_sha: head,
                        workflow_id: 2,
                        check_suite_id: 3,
                        status: 'completed',
                        conclusion: 'success',
                        event: 'push',
                      },
                    ],
                  },
                ]
              : [{ total_count: 0, statuses: [] }],
      )
    },
  }
  return { deps, calls }
}

test('gh view falls back to REST after refusal, with cached runs and usage labels', async () => {
  const f = fixture()
  const read = ghCheckReader({ deps: f.deps })
  const view = await read({ what: 'view', pr: 7, timeoutMs: 60_000 })
  assert.equal(view.ok, true)
  assert.equal(view.headSha, head)
  assert.equal(view.payload.statusCheckRollup[0].workflowName, 'CI')
  assert.deepEqual(view.degraded, degraded)
  const count = f.calls.length
  const runs = await read({ what: 'runs', pr: 7, headSha: head, timeoutMs: 60_000 })
  assert.equal(runs.payload[0].workflowDatabaseId, 2)
  assert.equal(f.calls.length, count)
  assert.deepEqual(read.degraded(), degraded)
  assert.ok(f.calls.every((call) => call.options.env.BOSS_GH_USAGE_LABEL === 'fallback:ci-wait'))
})

test('core run refusal uses GraphQL suites and pins the repository host', async () => {
  const f = fixture({
    runs: refused,
    suites: {
      data: {
        repository: {
          object: {
            checkSuites: {
              totalCount: 1,
              nodes: [
                {
                  status: 'COMPLETED',
                  conclusion: 'SUCCESS',
                  workflowRun: { event: 'push', workflow: { name: 'CI', databaseId: 2 } },
                },
              ],
            },
          },
        },
      },
    },
  })
  const read = ghCheckReader({ deps: f.deps })
  const result = await read({ what: 'runs', pr: 7, headSha: head, timeoutMs: 60_000 })
  assert.equal(result.ok, true)
  assert.equal(result.payload[0].workflowName, 'CI')
  assert.deepEqual(result.degraded, { transport: 'graphql', reason: 'core-rate-limited' })
  assert.ok(f.calls.at(-1).args.includes('ghe.example.test'))
})

for (const verb of ['read', 'view']) {
  test(`${verb} prints degraded REST state, preserves unknown decisions, fails closed on both refusals`, () => {
    const f = fixture()
    const result = main([verb, '--pr', '7', '--repo', 'ghe.example.test/acme/widgets'], f.deps)
    assert.equal(result.exit, 0)
    assert.deepEqual(result.out.degraded, degraded)
    if (verb === 'view') {
      assert.equal(result.out.mergeable, 'UNKNOWN')
      assert.equal(result.out.reviewDecision, null)
    } else {
      assert.equal(result.out.state.review_decision, null)
      assert.equal(result.out.state.is_workflow_runs_read, true)
      assert.equal(result.out.state.workflow_runs[0].name, 'CI')
    }
    assert.ok(f.calls.every((call) => call.options.env.BOSS_GH_USAGE_LABEL === 'fallback:pr-state'))
    const fail = main([verb, '--pr', '7'], fixture({ restFailure: refused }).deps)
    assert.equal(fail.exit, 1)
    assert.equal(fail.out.error.code, 'RATE_LIMITED')
    assert.equal('state' in fail.out, false)
  })
}

test('non-rate-limit failures never switch quotas or resolve the repository', async () => {
  for (const failure of [
    { status: 1, stderr: 'HTTP 404' },
    { status: 0, stdout: '{' },
    { error: { code: 'ETIMEDOUT' } },
  ]) {
    const f = fixture({ view: failure })
    const result = await ghCheckReader({ deps: f.deps })({ what: 'view', pr: 7, timeoutMs: 60_000 })
    assert.equal(result.ok, false)
    assert.equal(f.calls.length, 1)
  }
})

test('cross-check is exposed only after daemon fallback and reads one REST snapshot', async () => {
  const f = fixture()
  const read = daemonFirstReader({ deps: f.deps })
  assert.equal(read.crossCheck, undefined)
  await read({ what: 'view', pr: 7, timeoutMs: 60_000 })
  const before = f.calls.length
  const result = await read.crossCheck({ pr: 7, timeoutMs: 60_000 })
  assert.equal(result.ok, true)
  assert.deepEqual(read.degraded(), { transport: 'core', reason: 'cross-check' })
  assert.equal(f.calls.length - before, 4)
})

test('multi-spawn fallbacks share one read deadline and never spawn after it', async () => {
  const f = fixture()
  let now = 0
  const spawn = f.deps.spawn
  const timeouts = []
  f.deps.now = () => now
  f.deps.spawn = (cmd, args, options) => {
    timeouts.push(options.timeout)
    const result = spawn(cmd, args, options)
    now += 25_000
    return result
  }
  const result = await ghCheckReader({ deps: f.deps })({ what: 'view', pr: 7, timeoutMs: 60_000 })
  assert.equal(result.ok, false)
  assert.deepEqual(timeouts, [60_000, 35_000, 10_000])
  assert.equal(f.calls.length, 3)
})
