import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { test } from 'node:test'
import { assertConforms } from './adapter-core.mjs'
import { createGithubAdapter } from './github.mjs'

function harness({ fail, comments = [], readFails = false } = {}) {
  const calls = []
  const ticket = {
    number: 3,
    title: 'Ticket',
    state: 'OPEN',
    stateReason: null,
    labels: [{ name: 'boss:in-review' }, { name: 'needs-human' }],
    author: { login: 'alice' },
  }
  const mutations = []
  const adapter = createGithubAdapter({
    config: { trackerConfig: { github: { repository: 'acme/tasks' } } },
    spawn: (_command, args) => {
      calls.push(args)
      let output
      if (args[0] === 'label')
        output = ['boss:in-review', 'needs-human', 'bug'].map((name) => ({ name }))
      else if (args[0] === 'issue' && args[1] === 'view') {
        if (readFails) return { status: 1, stderr: 'read failed' }
        output = ticket
      } else if (args[0] === 'api' && args.includes('--method')) {
        mutations.push('comment')
        const { body } = JSON.parse(readFileSync(args[args.indexOf('--input') + 1], 'utf8'))
        if (fail === 'comment-landed') {
          comments.push({ id: 99, body })
          return { status: 1, stderr: 'HTTP 504 write may have applied' }
        }
        if (fail === 'comment') return { status: 1, stderr: 'HTTP 504 write may have applied' }
        comments.push({ id: 99, body })
        output = { id: 99, html_url: 'https://github.com/acme/tasks/issues/3#issuecomment-99' }
      } else if (args[0] === 'api') output = [comments]
      else if (args[0] === 'issue' && ['edit', 'close', 'reopen'].includes(args[1])) {
        const verb = args[1]
        mutations.push(verb)
        if (fail === verb) return { status: 1, stderr: 'request failed' }
        if (verb === 'edit') {
          for (let i = 0; i < args.length; i++) {
            if (args[i] === '--add-label') ticket.labels.push({ name: args[i + 1] })
            if (args[i] === '--remove-label')
              ticket.labels = ticket.labels.filter((label) => label.name !== args[i + 1])
          }
        }
        if (verb === 'close') {
          assert.equal(args[args.indexOf('--reason') + 1], 'completed')
          ticket.state = 'CLOSED'
          ticket.stateReason = 'COMPLETED'
        }
        return { status: 0, stdout: '' }
      } else throw new Error(`unexpected ${args.join(' ')}`)
      return { status: 0, stdout: JSON.stringify(output) }
    },
  })
  return { adapter, calls, mutations, ticket, comments }
}

test('GitHub write capability conforms and probes with only an issue read', async () => {
  const { adapter, calls, mutations } = harness()
  assertConforms(adapter)
  const result = await adapter.applyIssueWrites({ issueId: 'GH-3' })
  assert.deepEqual(result, {
    ok: true,
    outcome: 'ok',
    applied: [],
    issue: { id: 'GH-3', identifier: 'GH-3', labels: ['boss:in-review', 'needs-human'] },
  })
  assert.deepEqual(
    calls.map((args) => args.slice(0, 2)),
    [['issue', 'view']],
  )
  assert.deepEqual(mutations, [])
})

test('GitHub merge write drops human and workflow labels and closes completed', async () => {
  const { adapter, mutations, ticket } = harness()
  const result = await adapter.applyIssueWrites({
    issueId: 'GH-3',
    removeLabels: ['needs-human'],
    stateName: 'closed:completed',
  })
  assert.equal(result.outcome, 'ok')
  assert.deepEqual(result.applied, [
    { kind: 'removeLabel', name: 'needs-human' },
    { kind: 'state', name: 'closed:completed' },
  ])
  assert.deepEqual(ticket.labels, [])
  assert.equal(ticket.state, 'CLOSED')
  assert.deepEqual(mutations, ['edit', 'edit', 'close'])
})

test('GitHub write rejects malformed arguments before contacting gh', async () => {
  for (const input of [
    { issueId: '' },
    { issueId: 'GH-3', extra: true },
    { issueId: 'GH-3', addLabels: 'bug' },
    { issueId: 'GH-3', removeLabels: [''] },
    { issueId: 'GH-3', comment: '' },
    { issueId: 'GH-3', marker: ' ' },
    { issueId: 'GH-3', stateName: '' },
    { issueId: 'GH-3', addLabels: ['BUG'], removeLabels: ['bug'] },
  ]) {
    const { adapter, calls } = harness()
    await assert.rejects(adapter.applyIssueWrites(input))
    assert.deepEqual(calls, [])
  }
})

test('GitHub resolves all names before mutating and rejects role label changes', async () => {
  for (const [writes, reason] of [
    [{ addLabels: ['missing'] }, 'label-not-found: missing'],
    [{ stateName: 'Missing' }, 'state-not-found: Missing'],
    [{ addLabels: ['boss:in-review'] }, 'workflow'],
    [{ removeLabels: ['BOSS:IN-REVIEW'] }, 'workflow'],
    [{ addLabels: ['bug'], stateName: 'Missing', comment: 'no post' }, 'state-not-found: Missing'],
  ]) {
    const { adapter, mutations } = harness()
    const result = await adapter.applyIssueWrites({ issueId: 'GH-3', ...writes })
    assert.equal(result.outcome, 'failed')
    assert.equal(result.permanent, true)
    assert.ok(result.reason.includes(reason))
    assert.deepEqual(mutations, [])
  }
})

test('GitHub comments mention creators, append markers and follow labels before state', async () => {
  const { adapter, mutations, comments } = harness()
  const result = await adapter.applyIssueWrites({
    issueId: 'GH-3',
    addLabels: ['bug'],
    comment: 'Please review',
    mentionCreator: true,
    marker: '<!-- decision -->',
    stateName: 'closed:completed',
  })
  assert.equal(result.outcome, 'ok')
  assert.equal(comments[0].body, '@alice\n\nPlease review\n\n<!-- decision -->')
  assert.deepEqual(mutations, ['edit', 'comment', 'edit', 'close'])
})

test('GitHub comment markers prevent duplicate posts', async () => {
  const { adapter, mutations } = harness({ comments: [{ id: 1, body: 'prior <!-- decision -->' }] })
  const result = await adapter.applyIssueWrites({
    issueId: 'GH-3',
    comment: 'Please review',
    marker: '<!-- decision -->',
  })
  assert.deepEqual(result.applied, [{ kind: 'comment', existing: true }])
  assert.equal(result.outcome, 'ok')
  assert.deepEqual(mutations, [])
})

test('GitHub mutations that may have applied are never resent', async () => {
  for (const fail of ['edit', 'close', 'comment']) {
    const { adapter, mutations } = harness({ fail })
    const writes =
      fail === 'edit'
        ? { addLabels: ['bug'] }
        : fail === 'close'
          ? { stateName: 'closed:completed' }
          : { comment: 'Review' }
    const result = await adapter.applyIssueWrites({ issueId: 'GH-3', ...writes })
    assert.equal(result.outcome, 'indeterminate')
    assert.equal(mutations.filter((verb) => verb === fail).length, 1)
  }
})

test('GitHub settles uncertain comment with marker by reading back once', async () => {
  for (const fail of ['comment', 'comment-landed']) {
    const { adapter, mutations } = harness({ fail })
    const result = await adapter.applyIssueWrites({
      issueId: 'GH-3',
      comment: 'Review',
      marker: '<!-- decision -->',
    })
    assert.equal(result.outcome, fail === 'comment-landed' ? 'ok' : 'indeterminate')
    assert.deepEqual(mutations, ['comment'])
  }
})

test('GitHub failed issue read reports failed without writes', async () => {
  const { adapter, mutations } = harness({ readFails: true })
  const result = await adapter.applyIssueWrites({ issueId: 'GH-3' })
  assert.equal(result.outcome, 'failed')
  assert.match(result.reason, /issue-read-failed/)
  assert.deepEqual(mutations, [])
})
