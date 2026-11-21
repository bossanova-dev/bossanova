import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync, readdirSync, statSync } from 'node:fs'
import { join } from 'node:path'
import { tempDir } from '../../scripts/test-infra/test-tmpdir.mjs'
import { runCli } from './cli.mjs'
const graph = { seeds: ['A'], maxNodes: 100, complete: true, nodes: [] }
test('fetch-blocker-graph writes atomic private file and a single receipt', async (t) => {
  const dir = tempDir('blocker-graph-', t),
    out = join(dir, 'graph.json')
  let stdout = '',
    stderr = '',
    query
  const code = await runCli(
    ['fetch-blocker-graph', '--out-file', out, '--id', 'A,B', '--id', 'A'],
    {
      write: (s) => (stdout += s),
      errWrite: (s) => (stderr += s),
      resolveAdapter: () => ({
        readBlockerGraph: async (q) => {
          query = q
          return { ...graph, seeds: q.ids }
        },
      }),
    },
  )
  assert.equal(code, 0)
  assert.equal(stderr, '')
  assert.deepEqual(query, { ids: ['A', 'B'], maxNodes: 100 })
  assert.equal(stdout.trim().split('\n').length, 1)
  assert.equal(JSON.parse(stdout).outcome, 'blocker-graph-written')
  assert.deepEqual(JSON.parse(readFileSync(out, 'utf8')).seeds, ['A', 'B'])
  assert.equal(statSync(out).mode & 0o777, 0o600)
  assert.deepEqual(readdirSync(dir), ['graph.json'])
})
test('graph verb fails closed on usage, unsupported adapters and read errors without output', async (t) => {
  const dir = tempDir('blocker-graph-', t),
    out = join(dir, 'graph.json')
  for (const [args, adapter, expected] of [
    [[], {}, 64],
    [['--id', 'A', '--out-file', out, '--max-nodes', '501'], {}, 64],
    [['--id', 'A', '--out-file', out, '--out-file', out], {}, 64],
    [['--id', 'A', '--out-file', out], {}, 2],
    [
      ['--id', 'A', '--out-file', out],
      {
        readBlockerGraph: async () => {
          throw new Error('secret-value')
        },
      },
      2,
    ],
    [['--id', 'A', '--out-file', out], { readBlockerGraph: async () => ({}) }, 2],
  ]) {
    let stdout = '',
      stderr = ''
    assert.equal(
      await runCli(['fetch-blocker-graph', ...args], {
        write: (s) => (stdout += s),
        errWrite: (s) => (stderr += s),
        resolveAdapter: () => adapter,
      }),
      expected,
    )
    assert.equal(stdout, '')
    assert.ok(stderr)
    assert.ok(!stderr.includes('secret-value'))
    assert.deepEqual(readdirSync(dir), [])
  }
})
