import { test } from 'node:test'
import assert from 'node:assert/strict'
import { post } from './verify-gate.mjs'
import { memoryClaimHolders } from './verify-stale-claim.mjs'

const head = 'a'.repeat(40)
const now = Date.parse('2026-11-18T00:00:00Z')
const claim = (token, age = 0) => ({
  context: 'boss/verify',
  state: 'pending',
  description: `verifying… ${token}`,
  updated_at: new Date(now - age).toISOString(),
})
const park = {
  context: 'boss/verify',
  state: 'success',
  description: 'needs human: partial-build',
  updated_at: new Date(now - 4000).toISOString(),
}

function claimStatusWorld({ initial = [], back = [], unreadable = false } = {}) {
  let statuses = [...initial]
  let posted = false
  const writes = []
  const sleeps = []
  const ok = (payload) => ({ ok: true, payload, output: JSON.stringify(payload), exit: 0 })
  const read = (command, args) => {
    if (args[0] === 'pr') return ok({ state: 'OPEN', headRefOid: head })
    if (args.includes('POST')) {
      const fields = Object.fromEntries(
        args
          .filter((a) => /^(context|state|description|target_url)=/.test(a))
          .map((a) => {
            const n = a.indexOf('=')
            return [a.slice(0, n), a.slice(n + 1)]
          }),
      )
      if (fields.context) {
        writes.push(fields)
        statuses.unshift({ ...fields, updated_at: new Date(now).toISOString() })
        posted = true
      }
      return ok({})
    }
    if (args.some((a) => /commits\/.*\/statuses/.test(a))) {
      if (unreadable || (posted && back[0] === null)) {
        if (posted) back.shift()
        return { ok: false, output: '', exit: 1 }
      }
      return ok([posted && back.length ? back.shift() : statuses])
    }
    return ok([])
  }
  return {
    writes,
    sleeps,
    statuses: () => statuses,
    deps: {
      read,
      now: () => now,
      randomToken: () => 'mine',
      sleep: (ms) => sleeps.push(ms),
      claimHolders: memoryClaimHolders(),
      loadConfig: () => ({}),
      env: {},
      exists: () => false,
    },
  }
}
const options = { pr: 7, head, verdict: 'claim', repo: 'o/r' }

test('claim read-back retries uncached lag and a stale takeover', async () => {
  for (const initial of [[], [claim('old', 31 * 60_000)]]) {
    const w = claimStatusWorld({ initial, back: [initial] })
    const r = await post({ ...options, ...(initial.length ? { takeover: true } : {}) }, w.deps)
    assert.equal(r.won, true)
    assert.deepEqual(w.sleeps, [250])
    if (initial.length) assert.equal(r.replaced, 'old')
  }
})

test('unconfirmed claims withdraw to the standing park or waiting', async () => {
  for (const initial of [[], [park]]) {
    const w = claimStatusWorld({ initial, back: [null, null, null, null] })
    const r = await post(options, w.deps)
    assert.deepEqual([r.won, r.reason, r.released], [false, 'claim-unconfirmed', true])
    assert.equal(
      w.writes.at(-1).description,
      initial.length ? park.description : 'waiting: claim-unconfirmed',
    )
  }
})

test('a superseding claim or verdict is never withdrawn', async () => {
  for (const newer of [claim('foreign'), { ...park, description: 'verified' }]) {
    const w = claimStatusWorld({ back: [[newer]] })
    const r = await post(options, w.deps)
    assert.deepEqual([r.won, r.reason], [false, 'claim-held'])
    assert.equal(w.writes.length, 1)
  }
})

test('withdrawal preserves a superseding status first observed in its history read', async () => {
  for (const newer of [claim('foreign'), { ...park, description: 'verified' }]) {
    const w = claimStatusWorld({ back: [null, null, null, null, [newer]] })
    const r = await post(options, w.deps)
    assert.deepEqual([r.won, r.reason], [false, 'claim-held'])
    assert.equal(w.writes.length, 1)
  }
})

test('release preserves a superseding status first observed in its history read', async () => {
  for (const newer of [claim('foreign'), { ...park, description: 'verified' }]) {
    const w = claimStatusWorld({ initial: [claim('mine')] })
    const read = w.deps.read
    let reads = 0
    w.deps.read = (command, args, options) => {
      if (args.some((a) => /commits\/.*\/statuses/.test(a)) && ++reads === 2)
        return { ok: true, output: JSON.stringify([newer]), exit: 0 }
      return read(command, args, options)
    }
    const r = await post({ ...options, release: true, token: 'mine' }, w.deps)
    assert.deepEqual([r.released, r.reason, w.writes], [false, 'not-mine', []])
  }
})

test('release without a readable status still makes the head dispatchable', async () => {
  const w = claimStatusWorld({ unreadable: true })
  const r = await post({ ...options, release: true, token: 'mine' }, w.deps)
  assert.equal(r.released, true)
  assert.equal(w.writes.at(-1).description, 'waiting: dispatch-unknown')
  assert.deepEqual(w.sleeps, [250, 500, 1000])
})

test('release preserves a readable empty head after its first read was unreadable', async () => {
  const w = claimStatusWorld()
  let first = true
  const read = w.deps.read
  w.deps.read = (command, args, options) => {
    if (first && args.some((a) => /commits\/.*\/statuses/.test(a))) {
      first = false
      return { ok: false, output: '', exit: 1 }
    }
    return read(command, args, options)
  }
  const r = await post({ ...options, release: true, token: 'mine' }, w.deps)
  assert.deepEqual([r.released, r.reason, w.writes], [false, 'not-mine', []])
})
