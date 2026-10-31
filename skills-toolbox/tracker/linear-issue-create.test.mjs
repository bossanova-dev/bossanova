import assert from 'node:assert/strict'
import { test } from 'node:test'
import { createLinearAdapter } from './linear.mjs'
import { linearApplyIssueCreate } from './linear-issue-create.mjs'
import { assertConforms, TRACKER_CREDENTIALS_MISSING } from './adapter-core.mjs'

const issue = { id: 'issue-id', identifier: 'APP-9', url: 'https://linear.app/app/issue/APP-9' }
const marker = '<!-- follow-up:9 -->'
const input = { title: 'Follow up', description: 'Details', stateName: 'Todo', labels: ['bug'] }
function fixture(options = {}) {
  const calls = []
  let markerReads = 0
  const adapter = createLinearAdapter({
    apiKey: options.noKey ? undefined : 'test-key',
    fetchImpl: async (_url, init) => {
      const body = JSON.parse(init.body)
      calls.push(body)
      let data
      if (body.query.includes('query CreateTeam'))
        data = {
          teams: {
            nodes: options.noTeam
              ? []
              : [
                  {
                    id: 'team-id',
                    states: { nodes: options.noState ? [] : [{ id: 'state-id', name: 'Todo' }] },
                  },
                ],
          },
        }
      else if (body.query.includes('query CreateLabels'))
        data = {
          issueLabels: {
            nodes: options.noLabel
              ? []
              : [
                  { id: 'workspace-label', name: 'bug' },
                  { id: 'team-label', name: 'bug', team: { id: 'team-id' } },
                ],
            pageInfo: { hasNextPage: false },
          },
        }
      else if (body.query.includes('query CreateMarker')) {
        markerReads++
        if (options.lookupFail) return { ok: false, status: 403 }
        const nodes = markerReads === 1 ? (options.matches ?? []) : (options.settled ?? [])
        data = {
          issues: {
            nodes,
            pageInfo: options.incomplete ? { hasNextPage: true } : { hasNextPage: false },
          },
        }
      } else if (body.query.includes('mutation IssueCreate')) {
        if (options.dropped) throw new Error('socket hang up')
        if (options.truncated)
          return { ok: true, json: async () => JSON.parse('{"data": {"issueCreate" ') }
        data = { issueCreate: { success: true, issue: options.unreadable ? null : issue } }
      } else throw new Error(`unexpected query: ${body.query}`)
      return { ok: true, json: async () => ({ data }) }
    },
  })
  return { adapter, calls, mutations: () => calls.filter((c) => c.query.includes('mutation')) }
}

test('real Linear adapter conforms and creates with resolved team, state and preferred label', async () => {
  const h = fixture()
  assert.doesNotThrow(() => assertConforms(h.adapter))
  assert.deepEqual(await h.adapter.applyIssueCreate(input), { ok: true, outcome: 'ok', issue })
  assert.deepEqual(
    h.mutations().map((c) => c.variables.input),
    [
      {
        teamId: 'team-id',
        title: input.title,
        description: input.description,
        stateId: 'state-id',
        labelIds: ['team-label'],
      },
    ],
  )
})
test('unknown team, state and label fail permanently without mutation', async () => {
  for (const flag of ['noTeam', 'noState', 'noLabel']) {
    const h = fixture({ [flag]: true })
    const result = await h.adapter.applyIssueCreate(input)
    assert.equal(result.outcome, 'failed')
    assert.equal(result.permanent, true)
    assert.equal(h.mutations().length, 0)
  }
})
test('malformed input, unknown keys, missing team and credentials throw before requests', async () => {
  for (const extra of [
    { title: '' },
    { description: null },
    { stateName: '' },
    { labels: [9] },
    { marker: 'two\nlines' },
    { parentId: 'APP-1' },
  ]) {
    const h = fixture()
    await assert.rejects(async () => h.adapter.applyIssueCreate({ ...input, ...extra }))
    assert.equal(h.calls.length, 0)
  }
  const h = fixture({ noKey: true })
  await assert.rejects(async () => h.adapter.applyIssueCreate(input), {
    code: TRACKER_CREDENTIALS_MISSING,
  })
  assert.equal(h.calls.length, 0)
  await assert.rejects(async () => linearApplyIssueCreate({ apiKey: 'key', ...input }), /team/)
})
test('a dropped mutation stays indeterminate and is never resent', async () => {
  const h = fixture({ dropped: true })
  assert.equal((await h.adapter.applyIssueCreate(input)).outcome, 'indeterminate')
  assert.equal(h.mutations().length, 1)
})
test('marker existing match, ambiguity and failed lookup create nothing', async () => {
  const stored = { ...issue, description: `normalized\n${marker}\n` }
  for (const [options, expected] of [
    [{ matches: [stored] }, 'ok'],
    [{ matches: [stored, stored] }, 'failed'],
    [{ lookupFail: true }, 'failed'],
  ]) {
    const h = fixture(options)
    const result = await h.adapter.applyIssueCreate({ ...input, marker })
    assert.equal(result.outcome, expected)
    if (expected === 'ok')
      assert.deepEqual(result, { ok: true, outcome: 'ok', existing: true, issue })
    if (options.matches?.length === 2) assert.equal(result.reason, 'marker-ambiguous')
    assert.equal(h.mutations().length, 0)
  }
})
test('marker is appended as final description line when absent', async () => {
  for (const description of ['Details', `Details\n${marker}`]) {
    const h = fixture()
    await h.adapter.applyIssueCreate({ ...input, description, marker })
    assert.equal(h.mutations()[0].variables.input.description, `Details\n${marker}`)
  }
})
test('indeterminate create settles by marker read without resending', async () => {
  for (const settled of [[], [{ ...issue, description: marker }]]) {
    const h = fixture({ dropped: true, settled })
    const result = await h.adapter.applyIssueCreate({ ...input, marker })
    assert.equal(result.outcome, settled.length ? 'ok' : 'indeterminate')
    if (settled.length) assert.deepEqual(result.issue, issue)
    assert.equal(h.mutations().length, 1)
  }
})
test('an unreadable mutation response cannot certify creation', async () => {
  const h = fixture({ unreadable: true })
  assert.equal((await h.adapter.applyIssueCreate(input)).outcome, 'indeterminate')
  assert.equal(h.mutations().length, 1)
})

test('truncated mutation JSON stays uncertain or settles by marker without resending', async () => {
  for (const extra of [{}, { marker }]) {
    const h = fixture({ truncated: true })
    assert.equal(
      (await h.adapter.applyIssueCreate({ ...input, ...extra })).outcome,
      'indeterminate',
    )
    assert.equal(h.mutations().length, 1)
  }
  const h = fixture({ truncated: true, settled: [{ ...issue, description: marker }] })
  assert.deepEqual(await h.adapter.applyIssueCreate({ ...input, marker }), {
    ok: true,
    outcome: 'ok',
    existing: true,
    issue,
  })
  assert.equal(h.mutations().length, 1)
})

test('incomplete marker read cannot create an issue', async () => {
  const h = fixture({ incomplete: true })
  assert.equal((await h.adapter.applyIssueCreate({ ...input, marker })).outcome, 'failed')
  assert.equal(h.mutations().length, 0)
})
