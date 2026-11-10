import { test } from 'node:test'
import assert from 'node:assert/strict'
import { issueSnapshotProblems } from './issue-snapshot.mjs'
const complete = { id: 'APP-1', status: 'Planned', description: null, attachments: [] }
test('complete snapshots accept every guard state spelling and empty descriptions', () => {
  for (const state of [
    { status: 'Planned' },
    { stateName: 'Planned' },
    { state: 'Planned' },
    { state: { name: 'Planned' } },
  ]) {
    for (const description of [null, '', 'body']) {
      assert.deepEqual(
        issueSnapshotProblems({ identifier: 'APP-1', ...state, description, attachments: [{}] }),
        [],
      )
    }
  }
})
test('missing and invalid fields are refused individually', () => {
  for (const [key, field] of [
    ['id', 'identity'],
    ['status', 'state'],
    ['description', 'description'],
    ['attachments', 'attachments'],
  ]) {
    const value = { ...complete }
    delete value[key]
    assert.deepEqual(issueSnapshotProblems(value), [field])
  }
  for (const attachments of [null, {}, [null], ['attachment']])
    assert.deepEqual(issueSnapshotProblems({ ...complete, attachments }), ['attachments'])
  for (const value of [null, [], 'text', new Date()])
    assert.deepEqual(issueSnapshotProblems(value), [
      'identity',
      'state',
      'description',
      'attachments',
    ])
})
