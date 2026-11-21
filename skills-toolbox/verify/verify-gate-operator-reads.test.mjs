import assert from 'node:assert/strict'
import test from 'node:test'
import { setImmediate } from 'node:timers/promises'
import { approve, markMerged, sendBackMarker } from './verify-gate.mjs'

const HEAD = 'a'.repeat(40)
const OTHER = 'b'.repeat(40)
const NOW = Date.parse('2026-11-21T00:00:00Z')
const REPO = 'example/project'
const URL = `https://github.com/${REPO}/pull/7`
const ok = (payload) => ({ ok: true, payload, output: JSON.stringify(payload), exit: 0 })
const failed = () => ({ ok: false, error: 'exit-1', output: '', exit: 1 })
const comment = (body, id = 1) => ({
  id,
  body,
  user: { login: 'operator' },
  author_association: 'OWNER',
  created_at: new Date(NOW + id * 1000).toISOString(),
  html_url: `${URL}#issuecomment-${id}`,
})

// Each asynchronous batch stays in flight until the next event-loop turn. Counts measure
// dependency rounds rather than elapsed time, so host contention cannot change the result.
function recordingWorld(scenario = 'park') {
  const calls = [],
    writes = [],
    tracker = [],
    comments = []
  let active = 0,
    rounds = 0,
    views = 0,
    head = HEAD
  const statuses = [
    {
      context: 'boss/verify',
      state: 'failure',
      description: 'needs human: partial-build',
      updated_at: new Date(NOW - 1000).toISOString(),
    },
  ]
  if (scenario === 'wait')
    statuses.unshift({
      context: 'boss/verify',
      state: 'pending',
      description: 'waiting: stack-busy',
      updated_at: new Date(NOW).toISOString(),
    })
  if (scenario === 'merge' || scenario === 'lift') {
    statuses[0] = { ...statuses[0], state: 'success', description: 'verified' }
    if (scenario === 'lift') comments.push(comment(sendBackMarker(HEAD)))
  }
  const serve = (command, args) => {
    assert.equal(command, 'gh')
    calls.push([...args])
    if (args[0] === 'pr') {
      views++
      if (scenario === 'before-post' && views === 2) head = OTHER
      const merged = scenario === 'merged' || (scenario === 'lagging' && views === 2)
      const p = {
        state: merged ? 'MERGED' : 'OPEN',
        headRefOid: head,
        body: 'Tracker issue: https://linear.app/example/issue/EX-1/example',
        mergeCommit: merged ? { oid: OTHER } : null,
        number: 7,
        url: URL,
      }
      return ok(
        Object.fromEntries(
          args[args.indexOf('--json') + 1]
            .split(',')
            .filter((k) => k in p)
            .map((k) => [k, p[k]]),
        ),
      )
    }
    if (args[1] === 'user') return ok({ login: 'operator' })
    if (args[1].includes('/permission'))
      return ok({ permission: scenario === 'non-writer' ? 'read' : 'admin' })
    if (args.includes('POST')) {
      writes.push([...args])
      if (scenario === 'failed-post') return failed()
      const posted = comment(args.find((a) => a.startsWith('body=')).slice(5), 99)
      comments.push(posted)
      return ok({ html_url: posted.html_url })
    }
    if (args[1].includes('/comments')) return ok([comments])
    if (args[1].includes('/statuses')) return ok([statuses])
    assert.fail(`unexpected read: ${args.join(' ')}`)
  }
  const read = (...args) => {
    rounds++
    return serve(...args)
  }
  const readAsync = async (...args) => {
    if (active++ === 0) rounds++
    try {
      await setImmediate()
      return serve(...args)
    } finally {
      active--
    }
  }
  const adapter = {
    applyIssueWrites: async (request) => {
      tracker.push(request)
      if (scenario === 'during-probe' && !request.removeLabels) head = OTHER
      return {
        ok: true,
        outcome: 'ok',
        issue: { id: 'uuid', identifier: 'EX-1', labels: ['needs-human'] },
      }
    },
  }
  return {
    read,
    readAsync,
    adapter,
    calls,
    writes,
    tracker,
    counts: () => ({ calls: calls.length, rounds }),
  }
}

async function run(scenario, concurrent) {
  const w = recordingWorld(scenario)
  const options = {
    pr: 7,
    repo: REPO,
    ...(scenario === 'merge' ? { head: HEAD, waives: '', forMerge: true } : {}),
    ...(scenario === 'pinned-moved' ? { head: OTHER } : {}),
  }
  const deps = {
    read: w.read,
    ...(concurrent ? { readAsync: w.readAsync } : {}),
    adapter: w.adapter,
    now: () => NOW,
    loadConfig: () => ({}),
  }
  const result = await (['merged', 'lagging'].includes(scenario) ? markMerged : approve)(
    options,
    deps,
  )
  return { result, w }
}

for (const [scenario, rounds, calls] of [
  ['park', 6, 12],
  ['wait', 6, 12],
  ['merge', 5, 10],
  ['merged', 1, 1],
  ['lagging', 2, 2],
]) {
  test(`${scenario}: bounded GitHub calls and dependency rounds`, async (t) => {
    const { result, w } = await run(scenario, true)
    assert.equal(result.approved ?? result.merged, true)
    const counts = w.counts()
    t.diagnostic(`${scenario}: ${counts.calls} gh calls in ${counts.rounds} rounds`)
    assert.ok(counts.calls <= calls, `${counts.calls} calls exceeds ${calls}`)
    assert.ok(counts.rounds <= rounds, `${counts.rounds} rounds exceeds ${rounds}`)
    assert.equal(w.writes.length, ['merged', 'lagging'].includes(scenario) ? 0 : 1)
  })
}

for (const scenario of [
  'park',
  'wait',
  'merge',
  'non-writer',
  'pinned-moved',
  'before-post',
  'during-probe',
  'failed-post',
  'lift',
]) {
  test(`${scenario}: concurrent reads preserve results and writes`, async () => {
    const serial = await run(scenario, false)
    const concurrent = await run(scenario, true)
    assert.deepEqual(concurrent.result, serial.result)
    assert.deepEqual(concurrent.w.writes, serial.w.writes)
    assert.deepEqual(concurrent.w.tracker, serial.w.tracker)
    if (['pinned-moved', 'before-post', 'non-writer'].includes(scenario))
      assert.equal(concurrent.w.writes.length, 0)
    if (scenario === 'pinned-moved') assert.equal(concurrent.w.calls.length, 1)
    if (scenario === 'during-probe') {
      assert.equal(concurrent.result.why, 'head-moved')
      assert.equal(concurrent.w.tracker.length, 1)
    }
  })
}

test('stable memo retries failures and remembers only successful reads', async () => {
  const { operatorReads } = await import('./verify-read-rounds.mjs')
  let calls = 0
  const reads = operatorReads(() => (++calls === 1 ? failed() : ok({ login: 'operator' })), null, {
    sticky: () => true,
  })
  assert.equal(reads.read('gh', ['api', 'user']).ok, false)
  assert.equal(reads.read('gh', ['api', 'user']).ok, true)
  assert.equal(reads.read('gh', ['api', 'user']).ok, true)
  assert.equal(calls, 2)
})

test('warmed stable reads share the memo with later rounds and synchronous reads', async () => {
  const { operatorReads } = await import('./verify-read-rounds.mjs')
  let calls = 0
  const read = () => {
    calls++
    return ok({ permission: 'admin' })
  }
  const reads = operatorReads(read, async () => read(), { sticky: () => true })
  const call = ['gh', ['api', 'permission'], { json: true }]
  const first = await reads.round([call])
  const second = await reads.round([call])
  assert.deepEqual(second(...call), first(...call))
  assert.deepEqual(reads.read(...call), first(...call))
  assert.equal(calls, 1)
})

test('failed warmed stable reads are retried in a fresh round', async () => {
  const { operatorReads } = await import('./verify-read-rounds.mjs')
  let calls = 0
  const read = () => (++calls === 1 ? failed() : ok({ permission: 'admin' }))
  const reads = operatorReads(read, async () => read(), { sticky: () => true })
  const call = ['gh', ['api', 'permission']]
  assert.equal((await reads.round([call]))(...call).ok, false)
  assert.equal((await reads.round([call]))(...call).ok, true)
  assert.equal(reads.read(...call).ok, true)
  assert.equal(calls, 2)
})
