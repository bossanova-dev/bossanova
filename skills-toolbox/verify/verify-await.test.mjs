import assert from 'node:assert/strict'
import test from 'node:test'
import { awaitVerdict, CEILING_MS, parseOptions, verdictLine } from './verify-await.mjs'
import { CLAIM_STALE_AFTER_MS } from './verify-gate.mjs'

const head = 'ab0585c' + '0'.repeat(33)
const live = (token = 'holder', updatedAt = 0) => ({
  state: 'pending',
  description: `verifying… ${token}`,
  updated_at: new Date(updatedAt).toISOString(),
})
function fixture(statuses, { start = 0, views = [], chunkMs = 100000 } = {}) {
  let time = start,
    index = 0
  const read = (_command, args) => {
    if (args[0] === 'pr')
      return { ok: true, payload: views[index] ?? { state: 'OPEN', headRefOid: head } }
    const status = statuses[Math.min(index++, statuses.length - 1)]
    if (status === null) return { ok: false, exit: 1, output: '' }
    return { ok: true, output: JSON.stringify([{ context: 'boss/verify', ...status }]) }
  }
  return {
    run: () =>
      awaitVerdict({
        read,
        now: () => time,
        delay: async (ms) => {
          time += ms
        },
        options: { pr: 1, head, chunkMs },
      }),
    elapsed: () => time - start,
  }
}
test('follows a claim to its operator park label', async () => {
  const f = fixture([
    live(),
    { state: 'success', description: 'needs human: verify-policy · changes the verify judge' },
  ])
  const result = await f.run()
  assert.equal(result.outcome, 'verdict')
  assert.equal(result.line, 'Parked for you on ab0585c: changes the verify judge')
})
test('verified status reports verification and a merged PR reports merge', async () => {
  assert.equal(
    (await fixture([live(), { state: 'success', description: 'verified' }]).run()).line,
    'Verified ab0585c',
  )
  assert.deepEqual(
    await fixture([live()], { views: [{ state: 'MERGED', headRefOid: head }] }).run(),
    { outcome: 'merged', line: 'Verified and merged ab0585c' },
  )
})
test('reports send-back and waiting reasons', () => {
  for (const prefix of ['defect:', 'blocked:'])
    assert.equal(
      verdictLine({ state: 'failure', description: `${prefix} failed checks`, head }),
      `Sent back for repair on ab0585c: ${prefix} failed checks`,
    )
  assert.equal(
    verdictLine({ state: 'pending', description: 'waiting: CI pending', head }),
    'Waiting on ab0585c: CI pending',
  )
})
test('claim age uses the shared stale rule', async () => {
  assert.deepEqual(await fixture([live()], { start: CLAIM_STALE_AFTER_MS + 1 }).run(), {
    outcome: 'stale',
    token: 'holder',
  })
})
test('chunk is bounded and oversized chunks are refused', async () => {
  const f = fixture([live()], { chunkMs: 45000 })
  assert.deepEqual(await f.run(), {
    outcome: 'continue',
    token: 'holder',
    deadline: CLAIM_STALE_AFTER_MS,
  })
  assert.equal(f.elapsed(), 45000)
  assert.throws(
    () => parseOptions(['--pr', '1', '--head', head, '--chunk-ms', String(CEILING_MS + 1)]),
    /ceiling/,
  )
})
test('follows a replacement live claim and its deadline', async () => {
  const f = fixture([live(), live('replacement', 30000)], { chunkMs: 45000 })
  assert.deepEqual(await f.run(), {
    outcome: 'continue',
    token: 'replacement',
    deadline: 30000 + CLAIM_STALE_AFTER_MS,
  })
})
test('head moves and closed PRs end the wait', async () => {
  const newHead = 'b'.repeat(40)
  assert.deepEqual(
    await fixture([live()], { views: [{ state: 'OPEN', headRefOid: newHead }] }).run(),
    {
      outcome: 'head-moved',
      newHead,
      line: 'Head moved from ab0585c to bbbbbbb; the new head gets its own verify run',
    },
  )
  assert.equal(
    (await fixture([live()], { views: [{ state: 'CLOSED', headRefOid: head }] }).run()).outcome,
    'closed',
  )
})
test('unreadable or unrecognized status is unknown', async () => {
  assert.equal((await fixture([null]).run()).outcome, 'unknown')
  assert.equal(
    (await fixture([{ state: 'success', description: 'unrecognized' }]).run()).outcome,
    'unknown',
  )
})

test('a live claim crossing its deadline returns stale within the chunk', async () => {
  const f = fixture([live()], { start: CLAIM_STALE_AFTER_MS - 100, chunkMs: 1000 })
  assert.deepEqual(await f.run(), { outcome: 'stale', token: 'holder' })
  assert.equal(f.elapsed(), 101)
})
test('failed PR reads and missing statuses are unknown', async () => {
  assert.deepEqual(await awaitVerdict({ read: () => ({ ok: false }), options: { pr: 1, head } }), {
    outcome: 'unknown',
    why: 'pr-unreadable',
  })
  assert.equal((await fixture([{}]).run()).outcome, 'unknown')
})
test('CLI parsing is strict and carries repository scope', () => {
  assert.equal(
    parseOptions(['--pr', '1', '--head', head, '--repo', 'owner/repo']).repo,
    'owner/repo',
  )
  assert.throws(() => parseOptions(['--pr', '1', '--head', head, '--typo', 'yes']))
  assert.throws(() => parseOptions(['--pr', '0', '--head', head]))
  assert.throws(() => parseOptions(['--pr', '1', '--head', 'short']))
})

test('an expired chunk without a claim is unknown rather than a followable wait', async () => {
  let time = 0
  const result = await awaitVerdict({
    now: () => time,
    read: () => {
      time = 10
      return { ok: true, payload: { state: 'OPEN', headRefOid: head } }
    },
    options: { pr: 1, head, chunkMs: 5 },
  })
  assert.deepEqual(result, { outcome: 'unknown', why: 'chunk-exhausted-before-claim' })
})

test('merging a different head never certifies the head this wait followed', async () => {
  const newHead = 'c'.repeat(40)
  const result = await fixture([live()], {
    views: [{ state: 'MERGED', headRefOid: newHead }],
  }).run()
  assert.equal(result.outcome, 'head-moved')
  assert.equal(result.newHead, newHead)
})
