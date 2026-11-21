import { test } from 'node:test'
import assert from 'node:assert/strict'
import { decideHoldRelease, runCli } from './release-open-pr-holds.mjs'

const labels = { needsHuman: 'needs-human', build: 'agent-build' }
const marker = 'boss-build open-PR overlap hold: #42;'
const held = () => ({
  id: 'one',
  identifier: 'ABC-1',
  labels: { nodes: [{ id: 'human', name: 'needs-human' }] },
  comments: { nodes: [{ body: marker, createdAt: '2026-11-01T10:00:00Z' }] },
  history: {
    nodes: [
      { createdAt: '2026-11-01T09:59:59Z', addedLabelIds: ['human'], removedLabelIds: ['build'] },
    ],
    pageInfo: { hasNextPage: false },
  },
})

test('release requires a marker and a currently held issue', () => {
  assert.equal(decideHoldRelease(held(), labels).action, 'release')
  const noMarker = held()
  noMarker.comments.nodes = []
  assert.equal(decideHoldRelease(noMarker, labels).reason, 'no-hold-marker')
  const released = held()
  released.labels.nodes = [{ id: 'build', name: 'agent-build' }]
  assert.equal(decideHoldRelease(released, labels).reason, 'not-held')
})

test('label edits after the latest hold prevent release; comments do not', () => {
  const issue = held()
  issue.comments.nodes.push({ body: 'Human follow-up', createdAt: '2026-11-01T11:00:00Z' })
  assert.equal(decideHoldRelease(issue, labels).action, 'release')
  issue.history.nodes.push({
    createdAt: '2026-11-01T10:30:00Z',
    addedLabelIds: ['bug'],
    removedLabelIds: [],
  })
  assert.equal(decideHoldRelease(issue, labels).reason, 'labels-changed-after-hold')
  issue.comments.nodes.push({ body: marker, createdAt: '2026-11-01T12:00:00Z' })
  assert.equal(decideHoldRelease(issue, labels).action, 'release')
})

test('non-label history is ignored and truncated history prevents release', () => {
  const issue = held()
  issue.history.nodes.push({
    createdAt: '2026-11-01T11:00:00Z',
    addedLabelIds: [],
    removedLabelIds: [],
  })
  assert.equal(decideHoldRelease(issue, labels).action, 'release')
  issue.history.pageInfo.hasNextPage = true
  assert.equal(decideHoldRelease(issue, labels).reason, 'history-truncated')
})

function fixture() {
  const mutations = []
  const pages = []
  const fetchImpl = async (_url, init) => {
    const { query, variables } = JSON.parse(init.body)
    let data
    if (query.includes('ReleaseHeldIssues')) {
      pages.push(variables)
      data = {
        issues: {
          nodes: variables.after ? [] : [{ id: 'one', identifier: 'ABC-1', labels: held().labels }],
          pageInfo: { hasNextPage: !variables.after, endCursor: 'next' },
        },
      }
    } else if (query.includes('ReleaseHoldDetails'))
      data = {
        issue: { ...held(), comments: { ...held().comments, pageInfo: { hasNextPage: false } } },
      }
    else if (query.includes('ApplyIssueWritesRead'))
      data = { issue: { ...held(), team: { id: 'team' } } }
    else if (query.includes('ApplyIssueWritesLabels'))
      data = { issueLabels: { nodes: [{ id: 'build', name: 'agent-build' }] } }
    else if (query.includes('mutation')) {
      mutations.push({ query, variables })
      if (query.includes('ApplyAddLabel')) data = { issueAddLabel: { success: true } }
      else if (query.includes('ApplyRemoveLabel')) data = { issueRemoveLabel: { success: true } }
      else if (query.includes('ApplyComment'))
        data = {
          commentCreate: {
            success: true,
            comment: { id: 'comment', url: 'https://example.test/comment' },
          },
        }
      else assert.fail(query)
    } else assert.fail(query)
    return new Response(JSON.stringify({ data }), { status: 200 })
  }
  return { fetchImpl, mutations, pages }
}

test('dry-run paginates without writes; apply releases each issue once', async () => {
  const dry = fixture()
  const env = { LINEAR_API_KEY: 'test-key' }
  const result = await runCli(['--team', 'ABC'], { env, fetchImpl: dry.fetchImpl })
  assert.equal(result.code, 0)
  assert.equal(result.out.decisions[0].action, 'release')
  assert.equal(dry.pages.length, 2)
  assert.equal(dry.mutations.length, 0)
  const apply = fixture()
  const applied = await runCli(['--team', 'ABC', '--apply'], { env, fetchImpl: apply.fetchImpl })
  assert.equal(applied.code, 0)
  assert.equal(applied.out.decisions[0].outcome, 'ok')
  assert.deepEqual(
    apply.mutations.map((m) => m.query.match(/mutation (\w+)/)[1]),
    ['ApplyAddLabel', 'ApplyRemoveLabel', 'ApplyComment'],
  )
  assert.deepEqual(
    apply.mutations.slice(0, 2).map((m) => m.variables.labelId),
    ['build', 'human'],
  )
  assert.match(apply.mutations[2].variables.input.body, /boss-build overlap hold released/)
})

test('usage errors and absent credentials fail before requests', async () => {
  for (const args of [[], ['--wat'], ['--team']]) assert.equal((await runCli(args)).code, 64)
  assert.notEqual((await runCli(['--team', 'ABC'], { env: {} })).code, 0)
})
