import assert from 'node:assert/strict'
import test from 'node:test'
import { repoFor, canWrite, readOperatorMarkers } from './verify-gate.mjs'

const refused = { ok: false, stderr: 'API rate limit exceeded' }
const answer = (payload) => ({ ok: true, payload, output: JSON.stringify(payload) })

test('repository, permission and operator comments survive one refused quota', () => {
  const degraded = []
  assert.equal(
    repoFor(
      (_cmd, args) => (args[0] === 'repo' ? refused : answer({ full_name: 'o/r' })),
      undefined,
      degraded,
    ),
    'o/r',
  )
  assert.equal(degraded[0].transport, 'core')
  const permission = {
    data: {
      repository: {
        collaborators: { edges: [{ permission: 'MAINTAIN', node: { login: 'writer' } }] },
      },
    },
  }
  assert.equal(
    canWrite(
      (_cmd, args) => (args[1] === 'graphql' ? answer([permission]) : refused),
      'o/r',
      'writer',
    ),
    true,
  )
  const comments = [
    {
      data: {
        repository: {
          pullRequest: {
            comments: {
              nodes: [
                {
                  databaseId: 1,
                  body: 'No marker',
                  createdAt: '2026-11-01T12:00:00Z',
                  author: null,
                },
              ],
            },
          },
        },
      },
    },
  ]
  const result = readOperatorMarkers(
    (_cmd, args) => (args[1] === 'graphql' ? answer(comments) : refused),
    'o/r',
    1,
  )
  assert.equal(result.readable, true)
  assert.equal(result.degraded[0].read, 'issue-comments')
  assert.deepEqual(result.markers, [])
})

test('both quotas refused never becomes an empty marker set or permission denial', () => {
  const read = () => refused
  assert.equal(repoFor(read), '')
  assert.equal(canWrite(read, 'o/r', 'writer'), null)
  assert.deepEqual(readOperatorMarkers(read, 'o/r', 1), { readable: false })
  assert.equal(
    canWrite(
      () => ({ ok: false, exit: 1, payload: { status: '404' }, stderr: 'HTTP 404: Not Found' }),
      'o/r',
      'writer',
    ),
    false,
  )
})
