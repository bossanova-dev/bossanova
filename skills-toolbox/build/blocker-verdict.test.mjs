import assert from 'node:assert/strict'
import test from 'node:test'
import { main } from './blocker-verdict.mjs'

const url = 'https://github.com/example/project/pull/12'
const blocker = (id, prs = [], type = 'started') => ({
  id,
  state: { name: 'In Review', type },
  attachments: { nodes: prs.map((url) => ({ url })) },
})

async function invoke(argv, tracker, prLookup = async () => ({ state: 'OPEN' })) {
  let output = ''
  let errors = ''
  const code = await main(argv, {
    tracker,
    prLookup,
    stdout: {
      write: (text) => {
        output += text
      },
    },
    stderr: {
      write: (text) => {
        errors += text
      },
    },
  })
  return { code, rows: output.trim() ? output.trim().split('\n').map(JSON.parse) : [], errors }
}

test('prints one verdict per requested id and shares PR lookups', async () => {
  let calls = 0
  const result = await invoke(
    ['--id', 'TASK-1', '--id', 'TASK-2'],
    {
      readBlockers: async (ids) => ids.map((id) => ({ id, blockers: [blocker('ROOT', [url])] })),
    },
    async (pr) => {
      assert.equal(pr, url)
      calls++
      return { state: 'MERGED', mergedAt: '2026-01-01T00:00:00Z' }
    },
  )
  assert.equal(result.code, 0)
  assert.equal(calls, 1)
  assert.deepEqual(
    result.rows.map(({ id, verdict }) => ({ id, verdict })),
    [
      { id: 'TASK-1', verdict: 'unblocked' },
      { id: 'TASK-2', verdict: 'unblocked' },
    ],
  )
  assert.equal(result.rows[0].blockers[0].reason, 'merged-pr')
  assert.deepEqual(result.rows[0].blockers[0].prs, [url])
})

test('summary ranks uncleared blockers by unique dependents held', async () => {
  const result = await invoke(['--id', 'TASK-1', '--id', 'TASK-2', '--summary'], {
    readBlockers: async () => [
      {
        id: 'TASK-1',
        blockers: [blocker('B'), blocker('A'), blocker('A'), blocker('DONE', [], 'completed')],
      },
      { id: 'TASK-2', blockers: [blocker('A')] },
    ],
  })
  assert.equal(result.code, 0)
  assert.deepEqual(
    result.rows[2].gating.map(({ id, dependentsHeld }) => ({ id, dependentsHeld })),
    [
      { id: 'A', dependentsHeld: 2 },
      { id: 'B', dependentsHeld: 1 },
    ],
  )
  assert.deepEqual(result.rows[2].gating[0].state, blocker('A').state)
})

test('state-cleared and attachment-free blockers make no PR calls', async () => {
  const result = await invoke(
    ['--id', 'TASK-1'],
    {
      readBlockers: async () => [
        { id: 'TASK-1', blockers: [blocker('DONE', [url], 'completed'), blocker('OPEN')] },
      ],
    },
    async () => {
      throw new Error('unexpected lookup')
    },
  )
  assert.equal(result.code, 0)
  assert.equal(result.rows[0].verdict, 'blocked')
})

test('lookup failures fail closed without failing the adapter evaluation', async () => {
  const result = await invoke(
    ['--id', 'TASK-1'],
    {
      readBlockers: async () => [{ id: 'TASK-1', blockers: [blocker('ROOT', [url])] }],
    },
    async () => {
      throw new Error('lookup unavailable')
    },
  )
  assert.equal(result.code, 0)
  assert.equal(result.rows[0].verdict, 'blocked')
  assert.equal(result.rows[0].blockers[0].reason, 'pr-unknown')
})

for (const [name, argv, tracker] of [
  ['missing capability', ['--id', 'TASK-1'], {}],
  ['unknown flag', ['--wat'], {}],
  ['missing id', ['--summary'], {}],
  ['blank id', ['--id', '  '], {}],
  [
    'read failure',
    ['--id', 'TASK-1'],
    {
      readBlockers: async () => {
        throw new Error('unavailable')
      },
    },
  ],
  ['omitted candidate', ['--id', 'TASK-1'], { readBlockers: async () => [] }],
  [
    'invalid blockers',
    ['--id', 'TASK-1'],
    { readBlockers: async () => [{ id: 'TASK-1', blockers: null }] },
  ],
]) {
  test(`exits 2 on ${name}`, async () => {
    const result = await invoke(argv, tracker)
    assert.equal(result.code, 2)
    assert.notEqual(result.errors, '')
    assert.deepEqual(result.rows, [])
  })
}

test('selection emits soft-cleared ordering from dependent Key changes', async () => {
  const result = await invoke(
    ['--id', 'TASK-1', '--mode', 'selection'],
    {
      readBlockers: async (ids, options) => {
        assert.equal(options.mode, 'selection')
        return [
          {
            id: ids[0],
            description: '## Key changes\nEdit `src/child.mjs`.',
            blockers: [blocker('ROOT', [url])],
          },
        ]
      },
    },
    async () => ({ state: 'OPEN', isDraft: false, green: true, files: ['src/root.mjs'] }),
  )
  assert.equal(result.code, 0, result.errors)
  assert.equal(result.rows[0].verdict, 'unblocked')
  assert.deepEqual(result.rows[0].softCleared, [{ id: 'ROOT', pr: url }])
})
test('selection without a readable dependent plan remains blocked', async () => {
  const result = await invoke(
    ['--id', 'TASK-1', '--mode', 'selection'],
    {
      readBlockers: async () => [{ id: 'TASK-1', blockers: [blocker('ROOT', [url])] }],
    },
    async () => ({ state: 'OPEN', isDraft: false, green: true, files: ['src/root.mjs'] }),
  )
  assert.equal(result.code, 0, result.errors)
  assert.equal(result.rows[0].verdict, 'blocked')
  assert.deepEqual(result.rows[0].softCleared, [])
})

test('selection preserves top-level directories alongside individual files', async () => {
  for (const directory of ['scripts', './scripts', 'scripts/']) {
    const result = await invoke(
      ['--id', 'TASK-1', '--mode', 'selection'],
      {
        readBlockers: async () => [
          {
            id: 'TASK-1',
            description: `## Key changes\nEdit \`${directory}\` and \`README.md\`.`,
            blockers: [blocker('ROOT', [url])],
          },
        ],
      },
      async () => ({ state: 'OPEN', isDraft: false, green: true, files: ['scripts/foo.mjs'] }),
    )
    assert.equal(result.code, 0, result.errors)
    assert.equal(result.rows[0].verdict, 'blocked', directory)
    assert.deepEqual(result.rows[0].softCleared, [], directory)
  }
})
