import assert from 'node:assert/strict'
import test from 'node:test'
import { spawnSync } from 'node:child_process'
import { mkdtempSync, writeFileSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'
import { createEpicState } from './epic-driver.mjs'
import { main, scheduleSnapshot, upsertProgress } from './epic-cli.mjs'

const now = '2026-01-01T00:00:00Z'
const ticket = (id, blockedBy = [], extra = {}) => ({
  id,
  title: `Work ${id}`,
  priority: 2,
  createdAt: now,
  stateName: 'Planned',
  labels: ['agent-build'],
  blockedBy,
  ...extra,
})
const tickets = [
  ticket('A', ['D']),
  ticket('B', ['A']),
  ticket('C', ['B']),
  ticket('D', [], { stateType: 'completed' }),
]
const state = (extra = {}) => ({
  ...createEpicState({ runId: 'run', epicId: 'ROOT', tickets }),
  ...extra,
})
const schedule = (extra = {}) => scheduleSnapshot({ tickets, plannedState: 'Planned', ...extra })
const adapter = (transport) => ({
  operationMap: Object.fromEntries(
    ['readComments', 'writeComment', 'updateComment'].map((key) => [
      key,
      transport === 'mcp'
        ? { tool: `mcp__tracker__${key}` }
        : { command: { helper: 'tracker/fake.mjs', verb: key } },
    ]),
  ),
})
function tracker(comments = []) {
  const calls = []
  return {
    calls,
    comments,
    adapter: adapter('command'),
    runOperation(op, args) {
      const key = op.command.verb
      calls.push({ key, args })
      if (key === 'readComments') return { transport: 'command', result: comments }
      if (key === 'writeComment') {
        const row = { id: 'comment-1', body: args.body }
        comments.push(row)
        return { transport: 'command', result: row }
      }
      Object.assign(
        comments.find((row) => row.id === args.id),
        { body: args.body },
      )
      return { transport: 'command', result: { id: args.id } }
    },
  }
}
const upsert = (deps, extra = {}) =>
  upsertProgress({ state: state(), now: () => now, ...extra }, deps)

test('schedule emits a plain snapshot of the chain and completed blocker', () => {
  const result = schedule()
  assert.deepEqual(result.ready, ['A'])
  assert.deepEqual(
    result.done.map((row) => row.id),
    ['D'],
  )
  assert.deepEqual(result.externallyCleared, ['D'])
  assert.deepEqual(result.externalBlockersByTicket, {})
  assert.equal(result.nextToMerge, null)
  assert.deepEqual(result.clearedForMerge, [])
  assert.deepEqual(JSON.parse(JSON.stringify(result)), result)
})

test('schedule resumes in-flight/green work and cascade-skips failures', () => {
  assert.deepEqual(
    schedule({ state: state({ merged: ['A'], greens: ['B'], inFlight: ['B'] }) }).ready,
    [],
  )
  assert.equal(
    schedule({ state: state({ merged: ['A'], greens: ['B'], inFlight: ['B'] }) }).nextToMerge,
    'B',
  )
  assert.deepEqual(schedule({ state: state({ failed: ['A'] }) }).ready, [])
  assert.deepEqual(schedule({ state: state({ needsHuman: ['A'] }) }).ready, [])
})

test('schedule retains adopted green children outside the planned graph for merging', () => {
  for (const stateName of ['In Progress', 'In Review']) {
    const adopted = ticket('A', [], { stateName })
    const saved = createEpicState({ runId: 'run', epicId: 'ROOT', tickets: [adopted] })
    const result = schedule({
      tickets: [adopted],
      state: { ...saved, inFlight: ['A'], greens: ['A'] },
    })
    assert.deepEqual(result.eligible, [])
    assert.deepEqual(result.ready, [])
    assert.equal(result.nextToMerge, 'A')
  }
})

test('schedule reports external blockers and launch/merge override distinctions', () => {
  const input = { tickets: [ticket('X', ['E'])] }
  assert.deepEqual(schedule(input).externalBlockersByTicket, { X: ['E'] })
  for (const override of [
    { assumeCleared: ['E'] },
    { assumeClearedAndMerge: ['E'] },
    { state: state({ externallyCleared: ['E'] }) },
  ]) {
    const result = schedule({ ...input, ...override })
    assert.deepEqual(result.ready, ['X'])
    assert.deepEqual(result.externalBlockersByTicket, {})
  }
  assert.deepEqual(schedule({ ...input, assumeClearedAndMerge: ['E'] }).clearedForMerge, ['E'])
})

test('schedule preserves skip reasons and selection', () => {
  const result = schedule({
    tickets: [
      ticket('H', [], { labels: ['needs-human'] }),
      ticket('U', [], { stateName: 'Backlog' }),
    ],
  })
  assert.deepEqual(
    result.skipped.map((row) => row.id),
    ['H', 'U'],
  )
  assert.ok(result.skipped.every((row) => row.reason))
  const selected = schedule({ selection: { labels: { exclude: ['agent-build'] } } })
  assert.equal(selected.skipped.length, 3)
  assert.ok(selected.skipped.every((row) => row.reason.includes('excluded by selection:')))
})

test('schedule CLI accepts listChildren envelopes, environment selection, usage and invalid states', async () => {
  const bare = await main(['schedule', '--tickets-file', 'tickets', '--planned-state', 'Planned'], {
    readJson: () => tickets,
  })
  assert.equal(bare.exitCode, 0)
  assert.deepEqual(JSON.parse(bare.stdout), schedule())
  for (const payload of [
    { parent: {}, children: tickets },
    { result: { parent: {}, children: tickets } },
  ]) {
    const result = await main(
      ['schedule', '--tickets-file', 'tickets', '--planned-state', 'Planned'],
      { readJson: () => payload },
    )
    assert.equal(result.stdout, bare.stdout)
  }
  for (const argv of [
    ['schedule', '--tickets-file', 'tickets'],
    ['schedule', '--unknown'],
  ]) {
    assert.equal((await main(argv)).exitCode, 64)
  }
  const invalid = await main(
    [
      'schedule',
      '--tickets-file',
      'tickets',
      '--planned-state',
      'Planned',
      '--state-file',
      'state',
    ],
    { readJson: (file) => (file === 'tickets' ? tickets : {}) },
  )
  assert.equal(invalid.exitCode, 2)
  assert.equal(invalid.stdout, '')
  const selected = await main(
    ['schedule', '--tickets-file', 'tickets', '--planned-state', 'Planned'],
    {
      readJson: () => tickets,
      env: {
        BOSS_EPIC_SELECTION: JSON.stringify({
          selection: { labels: { exclude: ['agent-build'] } },
        }),
      },
    },
  )
  assert.deepEqual(JSON.parse(selected.stdout).ready, [])
})

test('command upsert creates once then updates the same marker comment', async () => {
  const deps = tracker()
  const before = JSON.stringify(state())
  const first = await upsert(deps)
  assert.deepEqual(first, {
    results: [
      { issueId: 'ROOT', op: 'create', commentId: 'comment-1', outcome: 'progress-upserted' },
    ],
    outcome: 'progress-upserted',
  })
  const second = await upsert(deps)
  assert.equal(second.results[0].op, 'update')
  assert.equal(deps.comments.length, 1)
  assert.equal(deps.comments[0].body.split('\n')[0], '<!-- boss-epic-progress -->')
  assert.equal(deps.calls.filter((call) => call.key === 'writeComment').length, 1)
  assert.equal(before, JSON.stringify(state()))
})

test('duplicate markers update the newest and disclose the warning', async () => {
  const deps = tracker([
    { id: 'old', body: '<!-- boss-epic-progress -->', createdAt: '2025-01-01' },
    { id: 'new', body: '<!-- boss-epic-progress -->', createdAt: now },
  ])
  const result = await upsert(deps)
  assert.equal(result.results[0].commentId, 'new')
  assert.match(result.results[0].warning, /2 comments/)
  assert.equal(deps.calls.at(-1).key, 'updateComment')
})

test('MCP upsert emits read then precisely one write descriptor without executing operations', async () => {
  const deps = {
    adapter: adapter('mcp'),
    runOperation: () => assert.fail('must not execute MCP operations'),
  }
  const read = await upsert(deps)
  assert.deepEqual(read.results[0], {
    issueId: 'ROOT',
    tool: 'mcp__tracker__readComments',
    args: { issueId: 'ROOT' },
    outcome: 'comments-required',
  })
  for (const comments of [
    [],
    { comments: [{ id: 'existing', body: '<!-- boss-epic-progress -->' }] },
  ]) {
    const result = await upsert(deps, { comments })
    assert.equal(result.results.length, 1)
    assert.equal(result.results[0].outcome, 'descriptor-emitted')
    assert.equal(result.results[0].op, Array.isArray(comments) ? 'create' : 'update')
    assert.equal(result.results[0].args.body.split('\n')[0], '<!-- boss-epic-progress -->')
  }
})

test('multi-root comments project membership and continue after a parent fails', async () => {
  const deps = tracker()
  const run = deps.runOperation
  deps.runOperation = (op, args) => {
    if (args.issueId === 'P1' && op.command.verb === 'writeComment') throw new Error('write failed')
    return run(op, args)
  }
  const membership = {
    parentIds: ['P1', 'P2'],
    childrenByParent: { P1: ['A', 'B'], P2: ['B', 'C'] },
  }
  const result = await upsert(deps, { membership })
  assert.equal(result.results[0].error, 'write failed')
  assert.equal(result.results[1].outcome, 'progress-upserted')
  const writes = deps.calls.filter((call) => call.key === 'writeComment')
  assert.equal(writes.length, 1)
  assert.match(writes[0].args.body, /Work B/)
  assert.match(writes[0].args.body, /Work C/)
  assert.doesNotMatch(writes[0].args.body, /Work A/)
  const mcp = await upsert(
    { adapter: adapter('mcp') },
    { membership, comments: { P1: [], P2: [] } },
  )
  assert.match(mcp.results[0].args.body, /Work A/)
  assert.match(mcp.results[0].args.body, /Work B/)
  assert.doesNotMatch(mcp.results[0].args.body, /Work C/)
})

test('invalid state, unreadable comments and planner errors cannot write', async () => {
  for (const comments of [{}, 'bad', [{ body: '<!-- boss-epic-progress -->' }]]) {
    const deps = tracker()
    const result = await upsert(deps, { comments })
    assert.ok(result.results[0].error)
    assert.equal(deps.calls.length, 0)
  }
  const deps = tracker()
  await assert.rejects(() => upsert(deps, { state: {} }), /runId/)
  await assert.rejects(() => upsert(deps, { state: state({ inFlight: {} }) }), /inFlight/)
  assert.equal(deps.calls.length, 0)
  const result = await main(
    ['upsert-progress', '--state-file', 'state', '--comments-file', 'comments'],
    {
      readJson: (file) => (file === 'state' ? state() : {}),
      adapter: deps.adapter,
      runOperation: deps.runOperation,
    },
  )
  assert.equal(result.exitCode, 2)
  assert.ok(JSON.parse(result.stdout).results[0].error)
})

test('CLI upsert reports partial root failures with exit 2 and preserves the inputs', async () => {
  const saved = state()
  const original = JSON.stringify(saved)
  const membership = { parentIds: ['P1', 'P2'], childrenByParent: { P1: ['A'], P2: ['B'] } }
  const result = await main(
    [
      'upsert-progress',
      '--state-file',
      'state',
      '--membership-file',
      'membership',
      '--comments-file',
      'comments',
      '--updated-at',
      now,
      '--next-wake',
      'callback',
    ],
    {
      adapter: adapter('mcp'),
      readJson: (file) => ({ state: saved, membership, comments: { P1: {}, P2: [] } })[file],
    },
  )
  assert.equal(result.exitCode, 2)
  const parsed = JSON.parse(result.stdout)
  assert.ok(parsed.results[0].error)
  assert.equal(parsed.results[1].op, 'create')
  assert.match(parsed.results[1].args.body, /Updated: 2026-01-01T00:00:00Z/)
  assert.equal(JSON.stringify(saved), original)
})

test('CLI schedule applies repeated overrides and selection-file precedence', async () => {
  const result = await main(
    [
      'schedule',
      '--tickets-file',
      'tickets',
      '--planned-state',
      'Planned',
      '--selection-file',
      'selection',
      '--assume-cleared',
      'E',
      '--assume-cleared',
      'F',
      '--assume-cleared-and-merge',
      'G',
    ],
    {
      readJson: (file) => (file === 'tickets' ? [ticket('X', ['E', 'F', 'G'])] : {}),
      env: { BOSS_EPIC_SELECTION: 'unreadable because file takes precedence' },
    },
  )
  assert.equal(result.exitCode, 0)
  assert.deepEqual(JSON.parse(result.stdout).ready, ['X'])
  assert.deepEqual(JSON.parse(result.stdout).clearedForMerge, ['G'])
  for (const payload of [{}, 'invalid']) {
    const invalid = await main(
      ['schedule', '--tickets-file', 'tickets', '--planned-state', 'Planned'],
      { readJson: () => payload },
    )
    assert.equal(invalid.exitCode, 2)
    assert.equal(invalid.stdout, '')
    assert.equal(invalid.stderr.trim().split('\n').length, 1)
  }
})

test('schedule helper accepts pipeline label options without loading repository config', () => {
  const result = schedule({
    tickets: [
      ticket('A', [], { labels: ['build-me'] }),
      ticket('H', [], { labels: ['build-me', 'human-review'] }),
    ],
    agentBuildLabel: 'build-me',
    needsHumanLabel: 'human-review',
  })
  assert.deepEqual(result.ready, ['A'])
  assert.deepEqual(
    result.skipped.map(({ id }) => id),
    ['H'],
  )
})

test('schedule executable resolves configured pipeline roles and retains unmapped defaults', () => {
  const dir = mkdtempSync(join(tmpdir(), 'epic-labels-'))
  try {
    for (const labels of [
      { agentBuild: 'build-me', needsHuman: 'human-review' },
      { agentBuild: 'build-me' },
      {},
    ]) {
      writeFileSync(
        join(dir, '.boss-skills.json'),
        JSON.stringify({ trackerConfig: { linear: { labels } } }),
      )
      const build = labels.agentBuild ?? 'agent-build'
      const human = labels.needsHuman ?? 'needs-human'
      writeFileSync(
        join(dir, 'tickets.json'),
        JSON.stringify([
          ticket('A', [], { labels: [build] }),
          ticket('H', [], { labels: [build, human] }),
          ticket('U', [], { labels: [] }),
        ]),
      )
      const run = spawnSync(
        process.execPath,
        [
          fileURLToPath(new URL('./epic-cli.mjs', import.meta.url)),
          'schedule',
          '--tickets-file',
          'tickets.json',
          '--planned-state',
          'Planned',
        ],
        {
          cwd: dir,
          encoding: 'utf8',
          env: { ...process.env, BOSS_EPIC_SELECTION: '', TRACKER: 'linear' },
        },
      )
      assert.equal(run.status, 0, run.stderr)
      const result = JSON.parse(run.stdout)
      assert.deepEqual(result.ready, ['A'])
      assert.deepEqual(
        result.eligible.map(({ id }) => id),
        ['A'],
      )
      assert.deepEqual(
        result.skipped.map(({ id }) => id),
        ['H', 'U'],
      )
      assert.match(result.skipped[0].reason, /needs.human/i)
    }
  } finally {
    rmSync(dir, { recursive: true, force: true, maxRetries: 3 })
  }
})
