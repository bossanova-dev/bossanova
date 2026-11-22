import assert from 'node:assert/strict'
import test from 'node:test'
import { runChunk, trendOf } from '../ci-wait.mjs'

const head = 'a'.repeat(40)
const pending = (count) => ({
  ok: true,
  headSha: head,
  payload: {
    statusCheckRollup: Array.from({ length: 3 }, (_, i) => ({
      name: `test-${i}`,
      status: i < count ? 'IN_PROGRESS' : 'COMPLETED',
      conclusion: i < count ? '' : 'SUCCESS',
    })),
  },
})
const unknown = { ok: false, error: 'unreadable' }
function setup(source, crossResult) {
  let clock = 1_800_000_000_000
  let crossChecks = 0
  let reads = 0
  let record = null
  let degraded = null
  const read = async ({ what }) =>
    what === 'runs' ? { ok: true, payload: [] } : ++reads < 3 ? pending(2) : unknown
  read.source = () => source
  read.degraded = () => degraded
  read.crossCheck = async () => {
    crossChecks++
    degraded = { transport: 'core', reason: 'cross-check' }
    return crossResult
  }
  const state = {
    latest: () => record,
    load: () => record,
    save: (r) => {
      record = structuredClone(r)
    },
    remove: () => {},
  }
  return {
    run: () =>
      runChunk({
        state,
        read,
        now: () => clock,
        sleep: async (ms) => {
          clock += ms
        },
        options: { pr: 7, budgetMs: 2000, intervalMs: 1000, chunkMs: 6000, readTimeoutMs: 1000 },
      }),
    crossChecks: () => crossChecks,
  }
}

test('a newest unreadable reading never supplies a frozen or converging trend', () => {
  for (const count of [2, 3])
    assert.equal(
      trendOf([
        { atMs: 0, pending: 3 },
        { atMs: 1000, pending: count },
        { atMs: 2000, pending: null },
      ]),
      'unknown',
    )
})

for (const [name, result, state, trend] of [
  ['falling pending', pending(1), 'timeout', 'converging'],
  [
    'red',
    {
      ok: true,
      headSha: head,
      payload: {
        statusCheckRollup: [{ name: 'test', status: 'COMPLETED', conclusion: 'FAILURE' }],
      },
    },
    'failed',
    'converging',
  ],
  ['unreadable', unknown, 'timeout', 'unknown'],
  ['green without confirmation', pending(0), 'timeout', 'converging'],
]) {
  test(`exhausted gh wait cross-checks once: ${name}`, async () => {
    const h = setup('gh-fallback', result)
    const out = await h.run()
    assert.equal(h.crossChecks(), 1)
    assert.equal(out.verdict.state, state)
    assert.equal(out.verdict.trend, trend)
    assert.deepEqual(out.verdict.degraded, { transport: 'core', reason: 'cross-check' })
    assert.equal(out.messages.filter((line) => line.startsWith('DEGRADED_READ')).length, 1)
  })
}

test('daemon wait never invokes the REST cross-check', async () => {
  const h = setup('daemon', pending(0))
  const out = await h.run()
  assert.equal(h.crossChecks(), 0)
  assert.equal(out.verdict.trend, 'unknown')
})
