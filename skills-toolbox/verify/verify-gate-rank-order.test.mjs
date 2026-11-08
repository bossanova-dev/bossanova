import { test } from 'node:test'
import assert from 'node:assert/strict'
import { candidates, writeAwareReader, classifyReadCall } from './verify-gate.mjs'
import { DEFAULT_CONFIG, mergeConfig } from '../skill-config.mjs'

const sha = (n) => n.toString(16).padStart(40, '0')
const rank = (tier) => ({
  tier,
  tierFrom: 'own',
  deadline: null,
  deadlineFrom: null,
  scheduled: true,
  leverage: 0,
})
const config = mergeConfig(DEFAULT_CONFIG, {
  adapters: { ...DEFAULT_CONFIG.adapters, tracker: 'demo' },
  trackerConfig: {
    demo: {
      mcpServer: 'demo',
      team: 'Demo',
      states: { inReview: 'In Review' },
      labels: {
        agentBuild: 'agent-build',
        needsHuman: 'needs-human',
        agentQuestion: 'agent-question',
      },
    },
  },
})
function fixture({ pending = false } = {}) {
  const tickets = [10, 11, 12].map((n) => ({
    identifier: `BOS-${n}`,
    labels: ['agent-build'],
    attachments: [],
    ...(n === 12 ? {} : { rank: rank(n === 11 ? 1 : 3) }),
  }))
  const nodes = [9, 10, 11, 12].map((n) => ({
    number: n,
    url: `https://github.com/o/r/pull/${n}`,
    title: '',
    body: n === 9 ? '' : `Linear issue: https://linear.app/acme/issue/BOS-${n}/t`,
    headRefOid: sha(n),
    headRefName: n === 9 ? 'cron-bs-build-123' : `feat/${n}`,
    isDraft: false,
    baseRefName: 'main',
    mergeStateStatus: 'CLEAN',
    updatedAt: '2026-11-01T00:00:00Z',
    baseRef: { target: { oid: sha(100) } },
    labels: { nodes: [] },
    commits: {
      nodes: [
        {
          commit: {
            oid: sha(n),
            status: { context: null },
            statusCheckRollup: {
              contexts: {
                pageInfo: { hasNextPage: false },
                nodes: [
                  {
                    __typename: 'CheckRun',
                    name: 'test',
                    status: pending && n === 10 ? 'IN_PROGRESS' : 'COMPLETED',
                    conclusion: pending && n === 10 ? null : 'SUCCESS',
                  },
                ],
              },
            },
          },
        },
      ],
    },
  }))
  const calls = []
  const read = writeAwareReader((cmd, args) => {
    calls.push({ cmd, args })
    const ok = (payload) => ({ ok: true, payload, output: JSON.stringify(payload), exit: 0 })
    if (cmd === 'boss' && args[0] === 'ls')
      return ok({ sessions: [{ id: 's9', pr_url: 'https://github.com/o/r/pull/9' }] })
    if (
      cmd === 'gh' &&
      args[0] === 'api' &&
      args[1] === 'graphql' &&
      args.some((a) => a.includes('pullRequests(states: OPEN'))
    )
      return ok({
        data: { repository: { pullRequests: { pageInfo: { hasNextPage: false }, nodes } } },
      })
    return { ok: false, error: 'unexpected-read', exit: 1, output: '' }
  })
  return {
    tickets,
    calls,
    deps: {
      read,
      loadConfig: () => config,
      now: () => Date.parse('2026-11-02'),
      adapter: { selectPlanned: async () => tickets },
    },
  }
}

test('candidates rank linked work before ticketless/unranked work and honor the limit', async () => {
  const w = fixture()
  const full = await candidates({ repo: 'o/r' }, w.deps)
  assert.deepEqual(
    full.candidates.map((c) => c.pr),
    [11, 10, 9, 12],
  )
  const limited = await candidates({ repo: 'o/r', limit: 1 }, w.deps)
  assert.deepEqual(
    limited.candidates.map((c) => c.pr),
    [11],
  )
  assert.ok(w.calls.every((c) => classifyReadCall(c.cmd, c.args) !== 'invalidating'))
})

test('rank payload includes skipped heads and excludes out-of-scope and ticketless PRs', async () => {
  const w = fixture({ pending: true })
  const result = await candidates({ repo: 'o/r' }, w.deps)
  assert.deepEqual(
    result.ranks.map((r) => r.pr),
    [10, 11],
  )
  assert.ok(result.skipped.some((s) => s.pr === 10 && s.reason === 'ci-unsettled'))
  const scoped = await candidates(
    { repo: 'o/r', excludePrUrls: ['https://github.com/o/r/pull/10'] },
    w.deps,
  )
  assert.deepEqual(
    scoped.ranks.map((r) => r.pr),
    [11],
  )
})

test('a changed tracker rank invalidates a quiet fingerprint', async () => {
  const w = fixture()
  const before = await candidates({ repo: 'o/r' }, w.deps)
  const quiet = await candidates({ repo: 'o/r', quietFingerprint: before.fingerprint }, w.deps)
  assert.equal(quiet.unchanged, true)
  assert.deepEqual(quiet.ranks, before.ranks)
  w.tickets[0].rank.tier = 1
  const after = await candidates({ repo: 'o/r', quietFingerprint: before.fingerprint }, w.deps)
  assert.notEqual(after.fingerprint, before.fingerprint)
  assert.notEqual(after.unchanged, true)
})
