import { test } from 'node:test'
import assert from 'node:assert/strict'
import { linearReadIssueSnapshot } from './linear-issue-snapshot.mjs'
const issue = {
  id: 'uuid',
  identifier: 'APP-1',
  title: 'title',
  description: 'original\nbody',
  state: { name: 'Planned', type: 'unstarted' },
  team: { name: 'Team' },
  attachments: {
    nodes: [{ id: 'a', title: 'plan', url: 'https://example.com', createdAt: '2026-01-01' }],
    pageInfo: { hasNextPage: false },
  },
}
const options = (value = issue) => ({
  apiKey: 'key',
  issueId: 'APP-1',
  fetchImpl: async () => ({ ok: true, json: async () => ({ data: { issue: value } }) }),
})
test('reads complete issue without changing description bytes', async () => {
  assert.deepEqual(await linearReadIssueSnapshot(options()), {
    id: 'APP-1',
    identifier: 'APP-1',
    uuid: 'uuid',
    title: 'title',
    description: issue.description,
    status: 'Planned',
    state: issue.state,
    team: 'Team',
    attachments: issue.attachments.nodes,
  })
  assert.equal(
    (await linearReadIssueSnapshot(options({ ...issue, description: null }))).description,
    '',
  )
})
test('refuses incomplete issue reads', async () => {
  for (const value of [
    null,
    { ...issue, id: '' },
    { ...issue, identifier: '' },
    { ...issue, description: undefined },
    { ...issue, state: {} },
    { ...issue, attachments: { ...issue.attachments, pageInfo: { hasNextPage: true } } },
    { ...issue, attachments: { nodes: [], pageInfo: {} } },
  ])
    await assert.rejects(linearReadIssueSnapshot(options(value)))
})
test('invalid id and credentials fail before network', async () => {
  let calls = 0
  const opts = {
    ...options(),
    fetchImpl: async () => {
      calls++
      throw new Error('network')
    },
  }
  await assert.rejects(linearReadIssueSnapshot({ ...opts, issueId: ' ' }), /id/)
  await assert.rejects(linearReadIssueSnapshot({ ...opts, apiKey: undefined }), {
    code: 'TRACKER_CREDENTIALS_MISSING',
  })
  assert.equal(calls, 0)
})
