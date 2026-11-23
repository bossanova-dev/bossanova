import { test } from 'node:test'
import assert from 'node:assert/strict'
import { existsSync, readdirSync } from 'node:fs'
import { join } from 'node:path'
import { tempDir } from '../../scripts/test-infra/test-tmpdir.mjs'
import { runCli } from './cli.mjs'
import { linearReadIssueSnapshot } from './linear-issue-snapshot.mjs'
const issue = {
  id: 'uuid',
  identifier: 'APP-1',
  title: 'title',
  description: 'original\nbody',
  state: { name: 'Planned', type: 'unstarted' },
  team: { name: 'Team' },
  labels: {
    nodes: [{ name: 'agent-build' }, { name: 'epic' }],
    pageInfo: { hasNextPage: false },
  },
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
    labels: ['agent-build', 'epic'],
    attachments: issue.attachments.nodes,
  })
  assert.deepEqual(
    (
      await linearReadIssueSnapshot(
        options({ ...issue, labels: { nodes: [], pageInfo: { hasNextPage: false } } }),
      )
    ).labels,
    [],
  )
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
    { ...issue, labels: undefined },
    { ...issue, labels: null },
    { ...issue, labels: { ...issue.labels, pageInfo: { hasNextPage: true } } },
    { ...issue, labels: { nodes: [], pageInfo: {} } },
    { ...issue, labels: { nodes: null, pageInfo: { hasNextPage: false } } },
    { ...issue, labels: { nodes: [null], pageInfo: { hasNextPage: false } } },
    { ...issue, labels: { nodes: [{}], pageInfo: { hasNextPage: false } } },
    { ...issue, labels: { nodes: [{ name: 42 }], pageInfo: { hasNextPage: false } } },
    { ...issue, labels: { nodes: [{ name: '' }], pageInfo: { hasNextPage: false } } },
    { ...issue, labels: { nodes: [{ name: '   ' }], pageInfo: { hasNextPage: false } } },
    { ...issue, labels: { nodes: ['agent-build'], pageInfo: { hasNextPage: false } } },
  ])
    await assert.rejects(linearReadIssueSnapshot(options(value)))
})
test('read-issue CLI exits 2 without writing file when labels are truncated', async (t) => {
  const dir = tempDir('linear-issue-snapshot-', t)
  const target = join(dir, 'snapshot.json')
  let stdout = ''
  let stderr = ''
  const code = await runCli(['read-issue', '--id', 'APP-1', '--out-file', target], {
    resolveAdapter: () => ({
      readIssueSnapshot: (issueId) =>
        linearReadIssueSnapshot({
          ...options({ ...issue, labels: { ...issue.labels, pageInfo: { hasNextPage: true } } }),
          issueId,
        }),
    }),
    write: (s) => {
      stdout += s
    },
    errWrite: (s) => {
      stderr += s
    },
  })
  assert.equal(code, 2)
  assert.equal(stdout, '')
  assert.equal(stderr.trim().split('\n').length, 1)
  assert.match(stderr, /incomplete labels/)
  assert.equal(existsSync(target), false)
  assert.deepEqual(readdirSync(dir), [])
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
