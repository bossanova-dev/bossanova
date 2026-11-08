import { test } from 'node:test'
import assert from 'node:assert/strict'
import { linearReadClaimComments, linearReadIssueState } from './linear-settle-reads.mjs'
import { TRACKER_CREDENTIALS_MISSING } from './adapter-core.mjs'
const issue = { id: 'uuid', identifier: 'APP-1' }
const comment = (id, createdAt) => ({ id, createdAt, body: 'claim' })
function options(pages) {
  let calls = 0
  return {
    apiKey: 'key',
    issueId: 'uuid',
    fetchImpl: async () => ({
      ok: true,
      json: async () => ({ data: { issue: { ...issue, ...pages[calls++] } } }),
    }),
  }
}
test('comment pages return every field sorted by creation time', async () => {
  const result = await linearReadClaimComments(
    options([
      {
        comments: {
          nodes: [comment('new', '2026-02-01')],
          pageInfo: { hasNextPage: true, endCursor: 'next' },
        },
      },
      { comments: { nodes: [comment('old', '2026-01-01')], pageInfo: { hasNextPage: false } } },
    ]),
  )
  assert.deepEqual(result.comments, [comment('old', '2026-01-01'), comment('new', '2026-02-01')])
})
test('comment reads refuse partial pagination and incomplete nodes', async () => {
  const page = { comments: { nodes: [], pageInfo: { hasNextPage: true, endCursor: 'next' } } }
  await assert.rejects(linearReadClaimComments(options(Array(5).fill(page))), /5/)
  await assert.rejects(
    linearReadClaimComments(
      options([{ comments: { nodes: [{ id: 'a', body: '' }], pageInfo: { hasNextPage: false } } }]),
    ),
    /createdAt/,
  )
})
test('both reads reject missing credentials and blank ids before network', async () => {
  for (const read of [linearReadClaimComments, linearReadIssueState]) {
    let calls = 0
    const opts = {
      issueId: 'uuid',
      fetchImpl: async () => {
        calls++
        throw new Error('network')
      },
    }
    await assert.rejects(read(opts), { code: TRACKER_CREDENTIALS_MISSING })
    await assert.rejects(read({ ...opts, issueId: ' ' }), /id/)
    assert.equal(calls, 0)
  }
})
test('state read preserves name and type and refuses a missing state', async () => {
  assert.deepEqual(
    await linearReadIssueState(options([{ state: { name: 'Review', type: 'started' } }])),
    { ...issue, state: { name: 'Review', type: 'started' } },
  )
  await assert.rejects(linearReadIssueState(options([{}])), /state/)
})
