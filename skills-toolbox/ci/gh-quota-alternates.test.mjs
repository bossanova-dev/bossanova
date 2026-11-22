import assert from 'node:assert/strict'
import test from 'node:test'
import {
  readAcrossQuotas,
  repoNameRest,
  pullCoreRest,
  draftTransitionsRest,
  issueCommentsGraphql,
  collaboratorPermissionGraphql,
  commitStatusesGraphql,
} from './gh-quota-alternates.mjs'

test('alternate responses preserve the consumer shapes', () => {
  assert.equal(repoNameRest().normalize({ full_name: 'o/r' }), 'o/r')
  for (const [state, merged, expected] of [
    ['open', false, 'OPEN'],
    ['closed', false, 'CLOSED'],
    ['closed', true, 'MERGED'],
  ])
    assert.deepEqual(
      pullCoreRest('o/r', 1).normalize({ state, merged, draft: true, head: { sha: 'abc' } }),
      { state: expected, isDraft: true, headRefOid: 'abc' },
    )
  assert.deepEqual(
    draftTransitionsRest('o/r', 1).normalize([
      [{ event: 'ready_for_review', created_at: '1' }, { event: 'labeled' }],
      [
        { event: 'convert_to_draft', created_at: '2' },
        { event: 'ready_for_review', created_at: '3' },
      ],
    ]),
    [
      { __typename: 'ConvertToDraftEvent', createdAt: '2' },
      { __typename: 'ReadyForReviewEvent', createdAt: '3' },
    ],
  )
  const comments = issueCommentsGraphql('o/r', 1).normalize([
    {
      data: {
        repository: {
          pullRequest: {
            comments: {
              nodes: [
                {
                  databaseId: 1,
                  body: 'bot',
                  createdAt: 'now',
                  updatedAt: 'later',
                  authorAssociation: 'MEMBER',
                  author: { __typename: 'Bot', login: 'robot' },
                },
                { databaseId: 2, body: 'gone', createdAt: 'then', author: null },
              ],
            },
          },
        },
      },
    },
  ])
  assert.equal(comments[0][0].user.login, 'robot[bot]')
  assert.equal(comments[0][0].updated_at, 'later')
  assert.equal(comments[0][0].author_association, 'MEMBER')
  assert.equal(comments[0][1].user.login, 'ghost')
  for (const [permission, expected] of [
    ['ADMIN', true],
    ['MAINTAIN', true],
    ['WRITE', true],
    ['TRIAGE', false],
  ])
    assert.equal(
      collaboratorPermissionGraphql('o/r', 'Writer').normalize({
        data: {
          repository: { collaborators: { edges: [{ permission, node: { login: 'writer' } }] } },
        },
      }),
      expected,
    )
  assert.equal(
    collaboratorPermissionGraphql('o/r', 'writer').normalize({
      data: { repository: { collaborators: { edges: [] } } },
    }),
    false,
  )
  const statuses = commitStatusesGraphql('o/r', 'abc')
  assert.deepEqual(statuses.normalize({ data: { repository: { object: { status: null } } } }), [])
  assert.deepEqual(
    statuses.normalize({
      data: {
        repository: {
          object: {
            status: {
              contexts: [
                {
                  context: 'boss/build',
                  state: 'SUCCESS',
                  description: 'ok',
                  targetUrl: 'https://example.test',
                  createdAt: 'now',
                },
                { context: 'future', state: 'EXPECTED' },
              ],
            },
          },
        },
      },
    }),
    [
      {
        context: 'boss/build',
        state: 'success',
        description: 'ok',
        target_url: 'https://example.test',
        updated_at: 'now',
      },
    ],
  )
  assert.throws(() => statuses.normalize({ data: { repository: { object: null } } }))
})

test('only a refused quota spends the alternate; double refusal remains unknown', () => {
  const primary = {
    call: ['gh', ['repo', 'view'], { json: true }],
    normalize: (p) => p.nameWithOwner,
  }
  for (const refusal of [
    'API rate limit exceeded',
    'was submitted too quickly',
    'rate limit exceeded',
  ]) {
    let calls = 0
    const read = () =>
      ++calls === 1 ? { ok: false, stderr: refusal } : { ok: true, payload: { full_name: 'o/r' } }
    assert.deepEqual(readAcrossQuotas(read, primary, repoNameRest()), {
      ok: true,
      value: 'o/r',
      degraded: { transport: 'core', reason: 'graphql-rate-limited' },
    })
    assert.equal(calls, 2)
  }
  for (const stderr of ['HTTP 404', 'bad credentials', 'timeout']) {
    let calls = 0
    assert.equal(
      readAcrossQuotas(
        () => {
          calls++
          return { ok: false, stderr }
        },
        primary,
        repoNameRest(),
      ).code,
      'UNREADABLE',
    )
    assert.equal(calls, 1)
  }
  let refusedCalls = 0
  assert.equal(
    readAcrossQuotas(
      () => {
        refusedCalls++
        return { ok: false, stderr: 'API rate limit exceeded' }
      },
      primary,
      repoNameRest(),
    ).code,
    'RATE_LIMITED',
  )
  assert.equal(refusedCalls, 2)
  let calls = 0
  const result = readAcrossQuotas(
    () =>
      ++calls === 1
        ? { ok: true, payload: { errors: [{ type: 'RATE_LIMITED' }] } }
        : { ok: true, payload: { full_name: 'o/r' } },
    primary,
    repoNameRest(),
  )
  assert.equal(result.value, 'o/r')
  assert.equal(calls, 2)
})

test('healthy and malformed primary responses never spend the alternate quota', () => {
  const primary = {
    call: ['gh', ['repo', 'view'], { json: true }],
    normalize: (p) => p.nameWithOwner,
  }
  for (const [response, expected] of [
    [
      { ok: true, payload: { nameWithOwner: 'o/r' } },
      { ok: true, value: 'o/r', degraded: null },
    ],
    [
      { ok: true, payload: null },
      { ok: false, code: 'UNREADABLE' },
    ],
    [
      { ok: false, error: 'invalid JSON', stderr: 'rate limit exceeded' },
      { ok: false, code: 'UNREADABLE' },
    ],
    [
      { ok: true, payload: { errors: [{ type: 'NOT_FOUND' }] } },
      { ok: false, code: 'UNREADABLE' },
    ],
  ]) {
    let calls = 0
    const result = readAcrossQuotas(
      () => {
        calls++
        return response
      },
      primary,
      repoNameRest(),
    )
    assert.deepEqual(result, expected)
    assert.equal(calls, 1)
  }
})
