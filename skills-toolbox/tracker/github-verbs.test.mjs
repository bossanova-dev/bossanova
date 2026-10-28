import assert from 'node:assert/strict'
import { test } from 'node:test'
import { mkdtempSync, writeFileSync, readFileSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { main } from './github.mjs'
const roles = ['boss:unplanned', 'boss:todo', 'boss:in-progress', 'boss:in-review']
const ranking = [
  ...[1, 2, 3, 4].map((n) => `boss:priority-${n}`),
  ...[0, 1, 2, 3, 5, 8].map((n) => `boss:estimate-${n}`),
]
const pipeline = ['agent-build', 'agent-plan', 'needs-human', 'agent-question', 'epic']
function fake({
  state = 'OPEN',
  labels = ['boss:todo', 'other'],
  version = '2.88.0',
  fail,
  available = [
    ...roles,
    ...pipeline,
    ...ranking,
    'other',
    'new',
    'boss:priority-1',
    'boss:estimate-3',
  ],
  hasIssues = true,
  reason = null,
} = {}) {
  const writes = [],
    calls = []
  const raw = {
    number: 42,
    title: 'Work',
    body: 'body',
    state,
    stateReason: reason,
    labels,
    assignees: [],
    author: { login: 'alice' },
  }
  const spawn = (cmd, args, options) => {
    calls.push(args)
    if (fail?.(args)) return { status: 1, stderr: 'HTTP 500: failed' }
    let result = ''
    if (args[0] === '--version') result = `gh version ${version}`
    else if (args[0] === 'auth') result = 'authenticated'
    else if (args[0] === 'label') result = available.map((name) => ({ name }))
    else if (args[0] === 'issue') {
      const verb = args[1]
      if (verb === 'list') result = []
      else if (verb === 'view') result = { ...raw, labels: raw.labels.map((name) => ({ name })) }
      else {
        writes.push(args)
        if (verb === 'reopen') raw.state = 'OPEN'
        if (verb === 'close') {
          raw.state = 'CLOSED'
          raw.stateReason = {
            completed: 'COMPLETED',
            'not planned': 'NOT_PLANNED',
            duplicate: 'DUPLICATE',
          }[args[args.indexOf('--reason') + 1]]
        }
        if (verb === 'edit')
          for (let i = 0; i < args.length; i++) {
            if (args[i] === '--add-label') raw.labels.push(args[i + 1])
            if (args[i] === '--remove-label')
              raw.labels = raw.labels.filter((name) => name !== args[i + 1])
          }
      }
    } else if (args[0] === 'api') {
      if (args.includes('--input')) {
        writes.push(args)
        assert.equal(args.includes('secret body'), false)
        assert.deepEqual(JSON.parse(readFileSync(args[args.indexOf('--input') + 1], 'utf8')), {
          body: 'secret body',
        })
        result = { id: 123, html_url: 'https://github.com/acme/tasks/issues/42#issuecomment-123' }
      } else if (args[1] === 'user') result = { login: 'alice' }
      else if (args[1].endsWith('/comments'))
        result = [
          [{ id: 9, body: 'one', created_at: 'now', user: { login: 'a' } }],
          [{ id: 10, body: 'two', created_at: 'later', user: { login: 'b' } }],
        ]
      else if (args[1] === 'graphql')
        result = {
          data: {
            __type: {
              fields: ['subIssues', 'parent', 'blockedBy', 'blocking'].map((name) => ({ name })),
            },
          },
        }
      else if (
        args[1].endsWith('/dependencies/blocked_by') ||
        args[1].endsWith('/dependencies/blocking')
      )
        result = [[]]
      else if (args[1].endsWith('/parent')) return { status: 1, stderr: 'HTTP 404: Not Found' }
      else if (/issues\/42$/.test(args[1]))
        result = {
          ...raw,
          state: raw.state.toLowerCase(),
          html_url: 'https://github.com/acme/tasks/issues/42',
          issue_dependencies_summary: { total_blocked_by: 0, total_blocking: 0 },
        }
      else result = { has_issues: hasIssues }
    }
    return { status: 0, stdout: typeof result === 'string' ? result : JSON.stringify(result) }
  }
  const invoke = async (verb, args) => {
    const temp = mkdtempSync(join(tmpdir(), 'github-verb-test-'))
    let output = '',
      error = ''
    try {
      const file = join(temp, 'args.json')
      writeFileSync(file, JSON.stringify(args))
      const status = await main([verb, '--args-file', file], {
        config: { trackerConfig: { github: { repository: 'acme/tasks' } } },
        spawn,
        stdout: (text) => (output += text),
        stderr: (text) => (error += text),
      })
      return { status, result: output ? JSON.parse(output) : null, error }
    } finally {
      rmSync(temp, { recursive: true, force: true, maxRetries: 3 })
    }
  }
  return { invoke, writes, calls, raw }
}
test('state transitions swap role labels, reopen, and close with each reason', async () => {
  const h = fake({ state: 'CLOSED', labels: ['boss:todo', 'boss:in-progress', 'other'] })
  const open = await h.invoke('move-state', { id: 'GH-42', state: 'boss:in-review' })
  assert.equal(open.status, 0, open.error)
  assert.deepEqual(open.result.labels, ['other', 'boss:in-review'])
  assert.equal(h.writes[0][1], 'reopen')
  assert.equal(h.writes[1][1], 'edit')
  for (const [state, reason] of [
    ['closed:completed', 'completed'],
    ['closed:not-planned', 'not planned'],
    ['closed:duplicate', 'duplicate'],
  ]) {
    const h = fake()
    const result = await h.invoke('move-state', { id: '#42', state })
    assert.equal(result.status, 0, result.error)
    assert.equal(result.result.state, state)
    assert.equal(h.writes.at(-1)[h.writes.at(-1).indexOf('--reason') + 1], reason)
    assert.deepEqual(result.result.labels, ['other'])
  }
  const closed = fake({ state: 'CLOSED', reason: 'COMPLETED' })
  assert.equal(
    (await closed.invoke('move-state', { id: '42', state: 'closed:duplicate' })).result.state,
    'closed:duplicate',
  )
  assert.equal(closed.writes[0][1], 'reopen')
})
test('whole label sets preserve workflow ownership and validate before any write', async () => {
  const h = fake()
  const moved = await h.invoke('move-state', { id: '42', labels: ['boss:todo', 'new'] })
  assert.equal(moved.status, 0, moved.error)
  assert.deepEqual(moved.result.labels, ['boss:todo', 'new'])
  for (const args of [
    { id: '42', labels: ['new'] },
    { id: '42', state: 'boss:in-review', labels: ['boss:todo', 'missing'] },
  ]) {
    const h = fake()
    const result = await h.invoke('move-state', args)
    assert.equal(result.status, 2)
    assert.equal(h.writes.length, 0)
    assert.match(result.error, /use state|missing label.*create/)
  }
  const absent = fake({ state: 'CLOSED', available: ['boss:todo', 'other'] })
  const failure = await absent.invoke('move-state', { id: '42', state: 'boss:in-review' })
  assert.equal(failure.status, 2)
  assert.match(failure.error, /boss init --merge --create-labels/)
  assert.equal(absent.writes.length, 0)
  const partial = fake({ state: 'CLOSED', fail: (args) => args[1] === 'edit' })
  assert.match(
    (await partial.invoke('move-state', { id: '42', state: 'boss:in-review' })).error,
    /edit labels.*may have applied/,
  )
  assert.equal(partial.writes[0][1], 'reopen')
})
test('comments retain numeric IDs across pages and send bodies via input files', async () => {
  const h = fake()
  const issue = await h.invoke('get-issue', { id: 'https://github.com/acme/tasks/issues/42' })
  assert.deepEqual(
    issue.result.comments.map((comment) => comment.id),
    ['9', '10'],
  )
  for (const [verb, args] of [
    ['write-comment', { issueId: 'GH-42', body: 'secret body' }],
    ['update-comment', { id: '123', body: 'secret body' }],
  ]) {
    assert.equal((await h.invoke(verb, args)).result.id, '123')
  }
  const oversized = fake()
  assert.equal(
    (await oversized.invoke('write-comment', { issueId: '42', body: 'x'.repeat(65537) })).status,
    2,
  )
  assert.equal(oversized.calls.length, 0)
})
test('priority and estimate replace ranking labels', async () => {
  const h = fake({ labels: ['boss:todo', 'boss:priority-4', 'boss:estimate-8'] })
  const result = await h.invoke('set-priority-estimate', { id: '42', priority: 1, estimate: 3 })
  assert.equal(result.status, 0, result.error)
  assert.deepEqual(result.result.labels, ['boss:todo', 'boss:priority-1', 'boss:estimate-3'])
})
test('preflight stops at the first failed prerequisite with a remedy', async () => {
  for (const [options, status] of [
    [{ fail: (args) => args[0] === '--version' }, 'gh-missing'],
    [{ version: '2.87.9' }, 'gh-outdated'],
    [{ fail: (args) => args[0] === 'auth' }, 'unauthenticated'],
    [{ fail: (args) => args[0] === 'api' }, 'repository-unavailable'],
    [{ hasIssues: false }, 'issues-disabled'],
    [{ available: [] }, 'labels-missing'],
  ]) {
    const h = fake(options)
    const result = await h.invoke('preflight', {})
    assert.equal(result.status, 2)
    assert.match(result.error, new RegExp(`tracker preflight: ${status}:`))
    assert.equal(h.writes.length, 0)
  }
  assert.deepEqual((await fake().invoke('preflight', {})).result, {
    ok: true,
    repository: 'acme/tasks',
    viewer: 'alice',
  })
})

test('delete-comment validates before spawning and preserves DELETE failures and host', async () => {
  for (const repository of ['acme/tasks', 'git.example/acme/tasks']) {
    const temp = mkdtempSync(join(tmpdir(), 'github-delete-test-'))
    const calls = []
    let failure = false
    try {
      const file = join(temp, 'args.json')
      const invoke = async (id) => {
        writeFileSync(file, JSON.stringify({ id }))
        let out = '',
          error = ''
        const status = await main(['delete-comment', '--args-file', file], {
          config: { trackerConfig: { github: { repository } } },
          spawn: (command, args, options) => {
            assert.equal(command, 'gh')
            assert.equal(options.shell, false)
            calls.push(args)
            return failure
              ? { status: 1, stderr: 'HTTP 404: Not Found' }
              : { status: 0, stdout: '' }
          },
          stdout: (text) => {
            out += text
          },
          stderr: (text) => {
            error += text
          },
        })
        return { status, out, error }
      }
      for (const id of ['oops', '0', '-1', '1.5']) assert.equal((await invoke(id)).status, 2)
      assert.equal(calls.length, 0)
      const deleted = await invoke('123')
      assert.equal(deleted.status, 0, deleted.error)
      assert.deepEqual(JSON.parse(deleted.out), { id: '123', deleted: true })
      assert.deepEqual(calls[0], [
        'api',
        'repos/acme/tasks/issues/comments/123',
        '--hostname',
        repository.includes('git.example') ? 'git.example' : 'github.com',
        '-X',
        'DELETE',
      ])
      failure = true
      const failed = await invoke('123')
      assert.equal(failed.status, 2)
      assert.match(failed.error, /HTTP 404/)
    } finally {
      rmSync(temp, { recursive: true, force: true, maxRetries: 3 })
    }
  }
})

test('preflight requires ranking labels before any writes and gives the init remedy', async () => {
  const available = [...roles, ...pipeline, ...ranking].filter((name) => name !== 'boss:estimate-5')
  const h = fake({ available })
  const result = await h.invoke('preflight', {})
  assert.equal(result.status, 2)
  assert.match(result.error, /labels-missing: boss:estimate-5;/)
  assert.match(result.error, /boss init --merge --create-labels/)
  assert.equal(h.writes.length, 0)
  available.push('BOSS:ESTIMATE-5')
  assert.equal((await h.invoke('preflight', {})).status, 0)
})
