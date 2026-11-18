import { test } from 'node:test'
import assert from 'node:assert/strict'
import {
  APPLY_ISSUE_WRITES_BLOCKER_QUERY,
  APPLY_ISSUE_WRITES_LABELS_QUERY,
  APPLY_ISSUE_WRITES_READ_QUERY,
  linearApplyIssueWrites,
} from './linear-issue-writes.mjs'

const issue = {
  id: 'issue-uuid',
  identifier: 'BOS-1',
  team: { id: 'team-1', states: { nodes: [] } },
  labels: { nodes: [{ id: 'label-agent', name: 'agent-build' }] },
  comments: { nodes: [] },
  inverseRelations: {
    nodes: [
      { type: 'blocks', issue: { id: 'b5', identifier: 'BOS-5' } },
      { type: 'related', issue: { id: 'b7', identifier: 'BOS-7' } },
    ],
  },
}
const blockers = {
  'BOS-6': { id: 'b6', identifier: 'BOS-6' },
  'BOS-7': { id: 'b7', identifier: 'BOS-7' },
}

function linearFetch(extra = {}) {
  const calls = []
  const impl = async (url, init) => {
    const body = JSON.parse(init.body)
    calls.push(body)
    let data
    if (body.query === APPLY_ISSUE_WRITES_READ_QUERY) data = { issue }
    else if (body.query === APPLY_ISSUE_WRITES_BLOCKER_QUERY)
      data = { issue: { ...blockers, ...extra }[body.variables.id] ?? null }
    else if (body.query === APPLY_ISSUE_WRITES_LABELS_QUERY)
      data = {
        issueLabels: { nodes: [{ id: 'label-plan', name: 'agent-plan', team: { id: 'team-1' } }] },
      }
    else if (body.query.startsWith('mutation')) {
      const field = body.query.match(/\{\s*(\w+)\(/)[1]
      data = { [field]: { success: true } }
    } else throw new Error(`unexpected query: ${body.query.slice(0, 60)}`)
    return { ok: true, json: async () => ({ data }) }
  }
  return { impl, calls }
}
const mutations = (calls) => calls.filter((c) => c.query.startsWith('mutation'))

test('addBlockedBy writes only missing blocking edges, before label writes', async () => {
  const { impl, calls } = linearFetch()
  const result = await linearApplyIssueWrites({
    apiKey: 'k',
    fetchImpl: impl,
    issueId: 'BOS-1',
    addBlockedBy: ['bos-5', 'BOS-6', 'BOS-7'],
    addLabels: ['agent-plan'],
  })
  assert.equal(result.outcome, 'ok')
  const sent = mutations(calls)
  assert.deepEqual(
    sent.map((c) => c.variables),
    [
      { input: { issueId: 'b6', relatedIssueId: 'issue-uuid', type: 'blocks' } },
      { input: { issueId: 'b7', relatedIssueId: 'issue-uuid', type: 'blocks' } },
      { id: 'issue-uuid', labelId: 'label-plan' },
    ],
  )
  assert.match(sent[0].query, /issueRelationCreate/)
  assert.deepEqual(
    result.applied.map((a) => [a.kind, a.identifier ?? a.name]),
    [
      ['addBlockedBy', 'BOS-6'],
      ['addBlockedBy', 'BOS-7'],
      ['addLabel', 'agent-plan'],
    ],
  )
  // BOS-5 already blocks the issue (matched case-insensitively), so it is never even resolved.
  assert.ok(!calls.some((c) => c.variables?.id === 'bos-5'))
})

test('an unknown or self blocker fails permanently with nothing written', async () => {
  for (const [ref, reason] of [
    ['BOS-404', /blocker-not-found: BOS-404/],
    ['SELF', /blocker-is-self: SELF/],
  ]) {
    const { impl, calls } = linearFetch({ SELF: { id: 'issue-uuid', identifier: 'BOS-1' } })
    const result = await linearApplyIssueWrites({
      apiKey: 'k',
      fetchImpl: impl,
      issueId: 'BOS-1',
      addBlockedBy: ['BOS-6', ref],
      comment: 'held',
    })
    assert.equal(result.outcome, 'failed')
    assert.equal(result.permanent, true)
    assert.match(result.reason, reason)
    assert.deepEqual(mutations(calls), [])
  }
})

test('addBlockedBy must be a list of non-empty identifiers', async () => {
  await assert.rejects(
    linearApplyIssueWrites({ apiKey: 'k', issueId: 'BOS-1', addBlockedBy: 'BOS-6' }),
    /addBlockedBy must be an array/,
  )
})
