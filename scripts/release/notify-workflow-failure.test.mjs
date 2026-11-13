import assert from 'node:assert/strict'
import { test } from 'node:test'
import { notifyWorkflowFailure } from './notify-workflow-failure.mjs'

function fixture({ existing = false, missingLabel = false, jobsFail = false, replies = {} } = {}) {
  const requests = []
  const warnings = []
  const info = []
  const responses = {
    NotifyTeam: { teams: { nodes: [{ id: 'team-id' }] } },
    NotifyLabel: { issueLabels: { nodes: missingLabel ? [] : [{ id: 'label-id' }] } },
    NotifyCreateLabel: { issueLabelCreate: { success: true, issueLabel: { id: 'label-id' } } },
    NotifyOpenIssue: {
      issues: { nodes: existing ? [{ id: 'issue-id', identifier: 'BOS-42' }] : [] },
    },
    NotifyCreateIssue: { issueCreate: { success: true, issue: { identifier: 'BOS-43' } } },
    NotifyComment: { commentCreate: { success: true } },
    ...replies,
  }
  const options = {
    apiKey: 'secret-key',
    teamKey: 'BOS',
    label: 'nightly-failure',
    titlePrefix: 'Nightly run failed',
    context: {
      repo: { owner: 'owner', repo: 'repo' },
      serverUrl: 'https://github.example',
      runId: 123,
      ref: 'refs/heads/main',
      workflow: 'nightly',
      eventName: 'schedule',
      sha: 'abcdef',
    },
    github: {
      rest: { actions: { listJobsForWorkflowRun: Symbol('jobs') } },
      async paginate(method, args) {
        assert.equal(method, options.github.rest.actions.listJobsForWorkflowRun)
        assert.deepEqual(args, {
          owner: 'owner',
          repo: 'repo',
          run_id: 123,
          filter: 'latest',
          per_page: 100,
        })
        if (jobsFail) throw new Error('denied')
        return [
          {
            name: 'tests',
            conclusion: 'failure',
            steps: [{ name: 'race', conclusion: 'failure' }],
          },
          { name: 'build', conclusion: 'success', steps: [] },
          {
            name: 'sweep',
            conclusion: 'cancelled',
            steps: [{ name: 'tmux', conclusion: 'timed_out' }],
          },
        ]
      },
    },
    core: { warning: (message) => warnings.push(message), info: (message) => info.push(message) },
    async fetch(url, init) {
      assert.equal(url, 'https://api.linear.app/graphql')
      assert.equal(init.method, 'POST')
      assert.equal(init.headers.authorization, options.apiKey)
      const request = JSON.parse(init.body)
      assert.ok(!init.body.includes(options.apiKey))
      const operation = request.query.match(/(?:query|mutation) (\w+)/)[1]
      requests.push({ operation, variables: request.variables })
      assert.ok(operation in responses, `Unexpected operation: ${operation}`)
      return { ok: true, status: 200, json: async () => ({ data: responses[operation] }) }
    },
  }
  return { options, requests, warnings, info }
}

test('creates a labelled issue with the failed run and steps', async () => {
  const f = fixture()
  await notifyWorkflowFailure(f.options)
  const request = f.requests.at(-1)
  assert.equal(request.operation, 'NotifyCreateIssue')
  assert.equal(request.variables.input.title, 'Nightly run failed on main')
  assert.equal(request.variables.input.teamId, 'team-id')
  assert.deepEqual(request.variables.input.labelIds, ['label-id'])
  assert.match(
    request.variables.input.description,
    /https:\/\/github.example\/owner\/repo\/actions\/runs\/123/,
  )
  assert.match(request.variables.input.description, /Job `tests` \(failure\): `race` \(failure\)/)
  assert.match(
    request.variables.input.description,
    /Job `sweep` \(cancelled\): `tmux` \(timed_out\)/,
  )
  assert.doesNotMatch(request.variables.input.description, /Job `build`/)
  assert.deepEqual(f.requests[0].variables, { key: 'BOS' })
  assert.deepEqual(f.requests.find((r) => r.operation === 'NotifyOpenIssue').variables.filter, {
    team: { id: { eq: 'team-id' } },
    labels: { some: { id: { eq: 'label-id' } } },
    state: { type: { nin: ['completed', 'canceled'] } },
  })
})

test('comments on an open issue instead of creating another', async () => {
  const f = fixture({ existing: true })
  await notifyWorkflowFailure(f.options)
  assert.equal(f.requests.at(-1).operation, 'NotifyComment')
  assert.equal(f.requests.at(-1).variables.input.issueId, 'issue-id')
  assert.match(f.requests.at(-1).variables.input.body, /Commit: `abcdef`/)
  assert.ok(!f.requests.some((r) => r.operation === 'NotifyCreateIssue'))
})

test('finds the label in the team or workspace and creates it when absent', async () => {
  const f = fixture({ missingLabel: true })
  await notifyWorkflowFailure(f.options)
  assert.deepEqual(f.requests.find((r) => r.operation === 'NotifyLabel').variables.filter, {
    name: { eq: 'nightly-failure' },
    or: [{ team: { id: { eq: 'team-id' } } }, { team: { null: true } }],
  })
  assert.deepEqual(f.requests.find((r) => r.operation === 'NotifyCreateLabel').variables.input, {
    name: 'nightly-failure',
    teamId: 'team-id',
    color: '#B60205',
  })
})

test('still notifies when reading jobs fails, omitting the steps', async () => {
  const f = fixture({ jobsFail: true })
  await notifyWorkflowFailure(f.options)
  assert.equal(f.warnings.length, 1)
  assert.doesNotMatch(f.requests.at(-1).variables.input.description, /Failed:|Job `/)
})

test('rejects a missing key before sending requests', async () => {
  const f = fixture()
  f.options.apiKey = ''
  await assert.rejects(notifyWorkflowFailure(f.options), /Linear API key is required/)
  assert.deepEqual(f.requests, [])
})

test('rejects an unknown team', async () => {
  const f = fixture({ replies: { NotifyTeam: { teams: { nodes: [] } } } })
  await assert.rejects(notifyWorkflowFailure(f.options), /Linear team.*BOS.*not found/)
})

test('reports a non-2xx Linear response without echoing its body', async () => {
  const f = fixture()
  f.options.fetch = async () => ({
    ok: false,
    status: 401,
    json: async () => ({ secret: 'secret-key' }),
  })
  await assert.rejects(notifyWorkflowFailure(f.options), {
    message: 'Linear NotifyTeam failed: HTTP 401',
  })
})

test('reports GraphQL errors without exposing the API key', async () => {
  const f = fixture()
  f.options.fetch = async () => ({
    ok: true,
    status: 200,
    json: async () => ({ errors: [{ message: 'permission denied secret-key' }] }),
  })
  await assert.rejects(notifyWorkflowFailure(f.options), {
    message: 'Linear NotifyTeam failed: permission denied [redacted]',
  })
})

test('rejects an unsuccessful Linear mutation', async () => {
  const f = fixture({ replies: { NotifyCreateIssue: { issueCreate: { success: false } } } })
  await assert.rejects(notifyWorkflowFailure(f.options), /Linear issue creation failed/)
})
