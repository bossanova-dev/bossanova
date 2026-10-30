import { test } from 'node:test'
import assert from 'node:assert/strict'
import { decideVerdict, judge } from './verify-gate.mjs'
import { headFirstCiStartMs, REPAIR_RECEIPT_WAIT_MS } from './verify-session.mjs'

const NOW = Date.parse('2026-10-29T12:00:00Z')
const HEAD = 'a'.repeat(40)
const BASE = 'b'.repeat(40)
const URL = 'https://github.com/o/r/pull/7'
const greenFacts = {
  prState: 'OPEN',
  isDraft: false,
  ci: { settled: true, green: true },
  receipt: 'absent',
}

for (const tier of [undefined, { review: 'verify' }]) {
  test(`live repair waits ahead of receipt review (${tier?.review ?? 'build'})`, () => {
    const result = decideVerdict({ ...greenFacts, tier, repairInFlight: true })
    assert.equal(result.verdict, 'wait')
    assert.equal(result.reason, 'receipt-repair-pending')
    assert.deepEqual(result.notes, [])
  })
}
for (const repairInFlight of [false, undefined]) {
  test(`absent receipt still parks without a live repair (${repairInFlight})`, () => {
    const result = decideVerdict({ ...greenFacts, repairInFlight })
    assert.equal(result.verdict, 'human')
    assert.equal(result.reason, 'no-receipt')
  })
}

export function fixture({
  active = true,
  flagAbsent = false,
  age = 1000,
  sessions = [{ id: 's', pr_url: URL }],
  showFails = false,
  lsFails = false,
  receipt = 'absent',
  startedAt = new Date(NOW - age).toISOString(),
} = {}) {
  const calls = []
  const ok = (payload) => ({ ok: true, payload, output: JSON.stringify(payload), exit: 0 })
  const fail = () => ({ ok: false, exit: 1, output: '' })
  const read = (command, args) => {
    calls.push({ command, args })
    if (command === 'boss' && args[0] === 'ls') return lsFails ? fail() : ok({ sessions })
    if (command === 'boss' && args[0] === 'show')
      return showFails ? fail() : ok({ session: flagAbsent ? {} : { repair_active: active } })
    if (command !== 'gh') return fail()
    if (args[0] === 'pr')
      return ok({
        state: 'OPEN',
        number: 7,
        url: URL,
        body: 'Tracker issue: https://linear.app/acme/issue/ABC-1/thing\n\n## Review coverage\n\nfull (3 lenses)',
        isDraft: false,
        headRefOid: HEAD,
        baseRefName: 'main',
        mergeStateStatus: 'CLEAN',
        statusCheckRollup: [{ name: 'ci', status: 'COMPLETED', conclusion: 'SUCCESS', startedAt }],
      })
    const path = args[1]
    if (path === 'graphql')
      return ok({
        data: {
          repository: { pullRequest: { reviews: { nodes: [] }, timelineItems: { nodes: [] } } },
        },
      })
    if (path.includes('/comments?')) return ok([[]])
    if (path === 'repos/o/r/commits/main') return ok({ sha: BASE })
    if (path.includes('/contents/'))
      return { ...fail(), payload: { message: 'Not Found', status: '404' } }
    if (path.includes('/statuses')) {
      if (receipt === 'unknown') return fail()
      return ok([receipt === 'absent' ? [] : [{ context: 'boss/build', state: receipt }]])
    }
    if (path.includes('actions/runs?'))
      return ok([
        {
          workflow_runs: path.includes(HEAD)
            ? [{ name: 'CI', status: 'completed', conclusion: 'success', head_sha: HEAD }]
            : [],
        },
      ])
    if (path.includes('/check-runs'))
      return ok([
        {
          total_count: 1,
          check_runs: [{ name: 'ci', status: 'completed', conclusion: 'success' }],
        },
      ])
    if (path.endsWith('/status')) return ok({ statuses: [] })
    return fail()
  }
  return {
    calls,
    deps: {
      read,
      now: () => NOW,
      discover: () => ({ extensions: [], skipped: [] }),
      trustedTree: () => ({ ok: true, root: '/trusted' }),
      loadConfig: () => ({}),
      exists: () => false,
    },
  }
}

for (const [label, options, why] of [
  ['live', {}, 'repair-active'],
  ['exact cap', { age: 60 * 60 * 1000 }, 'repair-active'],
  ['expired', { age: 60 * 60 * 1000 + 1 }, 'wait-expired'],
  ['idle', { active: false }, 'repair-idle'],
  ['absent flag', { flagAbsent: true }, 'repair-active-absent'],
  ['nonboolean flag', { active: 'true' }, 'repair-active-absent'],
  ['no session', { sessions: [] }, 'no-session'],
  ['list failure', { lsFails: true }, 'session-unreadable'],
  ['detail failure', { showFails: true }, 'session-unreadable'],
  ['unknown age', { startedAt: 'invalid' }, 'head-age-unknown'],
]) {
  test(`judge absent receipt: ${label}`, async () => {
    const w = fixture(options)
    const result = await judge({ pr: 7, repo: 'o/r', dryRun: true }, w.deps)
    const inFlight = why === 'repair-active'
    assert.equal(result.reason, inFlight ? 'receipt-repair-pending' : 'blockers')
    if (!inFlight) {
      assert.equal(result.verdict, 'blocked')
      assert.deepEqual(
        result.blockers.map(({ kind }) => kind),
        ['no-review'],
      )
    }
    assert.deepEqual(result.facts.repairReceipt, { inFlight, why })
  })
}
for (const receipt of ['success', 'pending', 'unknown']) {
  test(`judge avoids session reads for ${receipt} receipt`, async () => {
    const w = fixture({ receipt })
    await judge({ pr: 7, repo: 'o/r' }, w.deps)
    assert.deepEqual(
      w.calls.filter(({ command }) => command === 'boss'),
      [],
    )
  })
}

test('head age starts at the earliest parseable non-boss CI entry', () => {
  assert.equal(REPAIR_RECEIPT_WAIT_MS, 60 * 60 * 1000)
  assert.equal(
    headFirstCiStartMs([
      { name: 'boss/build', startedAt: new Date(NOW - 999999).toISOString() },
      { context: 'boss/verify', startedAt: new Date(NOW - 999999).toISOString() },
      { name: 'ci', startedAt: new Date(NOW - 1000).toISOString() },
      { name: 'lint', startedAt: new Date(NOW - 2000).toISOString() },
      { name: 'invalid', startedAt: 'invalid' },
    ]),
    NOW - 2000,
  )
  assert.equal(headFirstCiStartMs(undefined), null)
  assert.equal(headFirstCiStartMs([{ name: 'ci' }]), null)
})
