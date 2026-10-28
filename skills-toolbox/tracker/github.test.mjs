import { declaredOperationArgKeys } from './adapter.mjs'
import { createLinearAdapter } from './linear.mjs'
import assert from 'node:assert/strict'
import { test } from 'node:test'
import { createGithubAdapter } from './github.mjs'
import { runCli } from './cli.mjs'
import { readFileSync } from 'node:fs'
import { assertConforms, operationTransport } from './adapter-core.mjs'
const config = { trackerConfig: { github: { repository: 'acme/tasks' } } }
const raw = (number, labels = ['boss:todo']) => ({
  number,
  title: `Issue ${number}`,
  state: 'OPEN',
  stateReason: null,
  labels: labels.map((name) => ({ name })),
  assignees: [],
  author: { login: 'alice' },
  body: 'text',
  createdAt: '2026-01-01',
  url: `https://github.com/acme/tasks/issues/${number}`,
})
function harness(rows = []) {
  const calls = []
  const spawn = (cmd, args, options) => {
    calls.push({ cmd, args, options })
    let output
    if (args[0] === 'label')
      output = ['boss:todo', 'agent-build', 'needs-human', 'other', 'boss:priority-1'].map(
        (name) => ({ name }),
      )
    else if (args[0] === 'issue' && args[1] === 'list') output = rows
    else if (args[0] === 'api' && args[1] === 'user') output = { login: 'alice' }
    else throw new Error(`unexpected ${args.join(' ')}`)
    return { status: 0, stdout: JSON.stringify(output) }
  }
  return { adapter: createGithubAdapter({ config, spawn }), calls }
}
test('conforming command adapter normalizes role conflicts and closed reasons', () => {
  const { adapter } = harness()
  assertConforms(adapter)
  assert.ok(adapter.operationMap.listChildren)
  assert.equal(adapter.operationMap.appendRelatedTo, undefined)
  for (const operation of Object.values(adapter.operationMap))
    assert.equal(operationTransport(operation), 'command')
  assert.equal(adapter.normalizeTicket(raw(1)).stateName, 'boss:todo')
  assert.equal(adapter.normalizeTicket(raw(1, [])).stateName, null)
  assert.deepEqual(adapter.normalizeTicket(raw(1, ['boss:todo', 'boss:in-review'])).stateConflict, [
    'boss:todo',
    'boss:in-review',
  ])
  for (const [reason, role] of [
    ['COMPLETED', 'closed:completed'],
    ['NOT_PLANNED', 'closed:not-planned'],
    ['DUPLICATE', 'closed:duplicate'],
    [null, 'closed:completed'],
  ])
    assert.equal(
      adapter.normalizeTicket({ ...raw(1), state: 'CLOSED', stateReason: reason }).stateName,
      role,
    )
  assert.equal(
    adapter.normalizeTicket({ ...raw(1), assignees: [{ login: 'a' }, { login: 'b' }] }).assigneeId,
    undefined,
  )
})
test('selection sends AND labels and applies disjunction, exclusions, identity and ranking', async () => {
  const { adapter, calls } = harness([
    raw(1, ['boss:todo', 'agent-build', 'needs-human']),
    raw(2, ['boss:todo', 'agent-build', 'other']),
    raw(3, ['boss:todo', 'agent-build', 'other', 'boss:priority-1']),
    raw(4, ['boss:todo', 'boss:in-review', 'agent-build', 'other']),
  ])
  const query = {
    state: 'boss:todo',
    requireLabels: ['agent-build'],
    excludeLabels: ['needs-human'],
    selection: {
      labels: { include: ['OTHER', 'missing-optional'].slice(0, 1), exclude: [] },
      assignees: { include: [], exclude: [] },
      creators: { include: ['me'], exclude: [] },
      projects: { include: [], exclude: [] },
    },
  }
  assert.deepEqual(
    (await adapter.selectPlanned(query)).map((row) => row.id),
    ['GH-3', 'GH-2'],
  )
  assert.equal(await adapter.hasWork(query), true)
  const request = calls.find((call) => call.args[0] === 'issue').args
  assert.deepEqual(
    request.filter((_, i) => request[i - 1] === '--label'),
    ['boss:todo', 'agent-build'],
  )
  for (const extra of [{ unknown: 1 }, { label: 'x' }])
    await assert.rejects(adapter.selectPlanned({ ...query, ...extra }), /unsupported/)
  for (const selection of [
    { creators: { include: ['a@example.com'] } },
    { projects: { include: ['x'] } },
    { labels: { include: ['missing'] } },
  ])
    await assert.rejects(adapter.selectPlanned({ ...query, selection }), /selector|supported|label/)
  assert.equal(await harness([]).adapter.hasWork(query), false)
  await assert.rejects(
    harness(Array.from({ length: 1000 }, () => raw(1))).adapter.selectPlanned(query),
    /incomplete/,
  )
})

test('logical operation argument names match the reference adapter', () => {
  const { adapter } = harness()
  const linear = createLinearAdapter({ apiKey: 'unused' })
  for (const candidate of [adapter, linear]) {
    for (const [name, operation] of Object.entries(candidate.operationMap)) {
      const keys = declaredOperationArgKeys(operation)
      if (name === 'preflight') {
        assert.equal(keys.size, 0)
        continue
      }
      assert.ok(keys.size > 0, `${name} must declare argument keys`)
      for (const key of keys) assert.ok(!key.includes('?'), `${name}: ${key}`)
    }
  }
  assert.deepEqual(
    declaredOperationArgKeys(adapter.operationMap.readPlanAttachment),
    new Set(['id', 'outFile']),
  )
  assert.deepEqual(
    declaredOperationArgKeys(linear.operationMap.readPlanAttachment),
    new Set(['id', 'format']),
  )
  assert.deepEqual(
    declaredOperationArgKeys(adapter.operationMap.readComments),
    new Set(['issueId']),
  )
  assert.deepEqual(declaredOperationArgKeys(adapter.operationMap.readLabels), new Set(['id']))
  for (const op of [
    'moveState',
    'readComments',
    'writeComment',
    'updateComment',
    'deleteComment',
    'readLabels',
    'setPriorityEstimate',
    'appendDependency',
  ])
    assert.deepEqual(
      declaredOperationArgKeys(adapter.operationMap[op]),
      declaredOperationArgKeys(linear.operationMap[op]),
    )
})

test('configured repository pins every transport host despite inherited GH_HOST', async () => {
  for (const [repository, host] of [
    ['acme/tasks', 'github.com'],
    ['git.example/acme/tasks', 'git.example'],
  ]) {
    const calls = []
    const adapter = createGithubAdapter({
      config: { trackerConfig: { github: { repository } } },
      env: { GH_HOST: 'other.example' },
      spawn: (_command, args, options) => {
        const flag = args[0] === 'api' ? '--hostname' : '--repo'
        const target = args[args.indexOf(flag) + 1]
        const effectiveHost =
          flag === '--hostname'
            ? target
            : target.split('/').length === 3
              ? target.split('/')[0]
              : options.env.GH_HOST
        assert.equal(effectiveHost, host)
        calls.push(args[0])
        const output =
          args[0] === 'api'
            ? { login: 'alice' }
            : args[0] === 'label'
              ? [{ name: 'boss:todo' }, { name: 'needs-human' }]
              : [raw(1)]
        return { status: 0, stdout: JSON.stringify(output) }
      },
    })
    assert.equal(
      (
        await adapter.selectPlanned({
          state: 'boss:todo',
          selection: { creators: { include: ['me'] } },
        })
      )[0].id,
      'GH-1',
    )
    assert.deepEqual(calls, ['label', 'api', 'issue'])
  }
})

test('GitHub claims conform and format identically to Linear in both scopes', () => {
  const { adapter } = harness()
  const linear = createLinearAdapter({ apiKey: 'unused' })
  assertConforms(adapter)
  assert.deepEqual(adapter.claimScopes, ['build', 'plan'])
  assert.equal(operationTransport(adapter.operationMap.deleteComment), 'command')
  for (const scope of ['build', 'plan'])
    for (const session of [null, 'session-123'])
      assert.equal(
        adapter.formatClaimComment('a'.repeat(32), session, { scope }),
        linear.formatClaimComment('a'.repeat(32), session, { scope }),
      )
})

test('concurrent GitHub leases agree on the first comment and the loser releases its claim', async () => {
  for (const scope of ['build', 'plan']) {
    const createdAt = new Date().toISOString().replace(/\.\d{3}Z$/, 'Z')
    let nextId = 99
    let store = []
    const adapter = createGithubAdapter({
      config,
      spawn: (command, args, options) => {
        assert.equal(command, 'gh')
        assert.equal(options.shell, false)
        const method = args[args.indexOf('--method') + 1]
        let result
        if (method === 'POST') {
          const { body } = JSON.parse(readFileSync(args[args.indexOf('--input') + 1], 'utf8'))
          const comment = { id: nextId++, body, created_at: createdAt, user: { login: 'runner' } }
          store.push(comment)
          result = comment
        } else if (args.includes('DELETE')) {
          store = store.filter((comment) => String(comment.id) !== args[1].split('/').at(-1))
          return { status: 0, stdout: '' }
        } else result = [store]
        return { status: 0, stdout: JSON.stringify(result) }
      },
    })
    const cli = (args, write = () => {}) =>
      runCli(args, { resolveAdapter: () => adapter, write, errWrite: () => {} })
    const tokens = ['b'.repeat(32), 'a'.repeat(32)]
    const ids = []
    for (const token of tokens) {
      let body = ''
      assert.equal(
        cli(['claim-comment', '--scope', scope, '--token', token], (text) => {
          body += text
        }),
        0,
      )
      ids.push(
        (await adapter.runOperation('write-comment', { issueId: 'GH-42', body: body.trim() })).id,
      )
    }
    const comments = await adapter.runOperation('read-comments', { issueId: 'GH-42' })
    assert.equal(comments[0].createdAt, comments[1].createdAt)
    for (const evidence of [comments, { comments }]) {
      assert.equal(
        cli([
          'claim-verdict',
          '--scope',
          scope,
          '--me',
          tokens[0],
          '--comments',
          JSON.stringify(evidence),
        ]),
        0,
      )
      assert.equal(
        cli([
          'claim-verdict',
          '--scope',
          scope,
          '--me',
          tokens[1],
          '--comments',
          JSON.stringify(evidence),
        ]),
        3,
      )
    }
    const op = adapter.operationMap.deleteComment
    assert.deepEqual(await adapter.runOperation(op.command.verb, { id: ids[1] }), {
      id: ids[1],
      deleted: true,
    })
    assert.deepEqual(
      store.map((comment) => String(comment.id)),
      [ids[0]],
    )
    for (const id of [undefined, '', 'bad', '0']) {
      const malformed = [{ ...comments[0], id }]
      assert.equal(
        cli([
          'claim-verdict',
          '--scope',
          scope,
          '--me',
          tokens[0],
          '--comments',
          JSON.stringify(malformed),
        ]),
        2,
      )
    }
  }
})

test('selection accepts only the configured repository as team', async () => {
  for (const repository of ['acme/tasks', 'github.com/acme/tasks', 'git.example/acme/tasks']) {
    const adapter = createGithubAdapter({
      config: { trackerConfig: { github: { repository } } },
      spawn: (_cmd, args) => ({
        status: 0,
        stdout: JSON.stringify(args[0] === 'label' ? [{ name: 'boss:todo' }] : [raw(1)]),
      }),
    })
    const query = { state: 'boss:todo', selection: {}, excludeLabels: ['absent'] }
    const baseline = await adapter.selectPlanned(query)
    const spellings = repository.startsWith('git.example')
      ? [repository.toUpperCase()]
      : ['ACME/TASKS', 'GITHUB.COM/ACME/TASKS']
    for (const team of spellings)
      assert.deepEqual(await adapter.selectPlanned({ ...query, team }), baseline)
    for (const team of ['elsewhere/tasks', 'git.other/acme/tasks', ''])
      await assert.rejects(
        adapter.selectPlanned({ ...query, team }),
        /--team .* does not name the configured repository/,
      )
  }
})
