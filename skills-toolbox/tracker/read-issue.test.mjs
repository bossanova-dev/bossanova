import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync, writeFileSync, readdirSync, mkdirSync } from 'node:fs'
import { join } from 'node:path'
import { tempDir } from '../../scripts/test-infra/test-tmpdir.mjs'
import { runCli } from './cli.mjs'
const snapshot = {
  id: 'APP-1',
  identifier: 'APP-1',
  uuid: 'uuid',
  status: 'Planned',
  description: 'private body',
  attachments: [],
}
async function run(t, adapter = { readIssueSnapshot: async () => snapshot }, flags) {
  const dir = tempDir('issue-read-', t),
    target = join(dir, 'snapshot.json')
  writeFileSync(target, 'prior')
  let stdout = '',
    stderr = ''
  const code = await runCli(
    ['read-issue', ...(flags ? flags(target, dir) : ['--id', 'APP-1', '--out-file', target])],
    {
      resolveAdapter: () => adapter,
      write: (s) => {
        stdout += s
      },
      errWrite: (s) => {
        stderr += s
      },
    },
  )
  return { code, stdout, stderr, target, dir }
}
test('writes snapshot and a body-free receipt', async (t) => {
  const r = await run(t)
  assert.equal(r.code, 0)
  assert.deepEqual(JSON.parse(readFileSync(r.target, 'utf8')), snapshot)
  assert.deepEqual(JSON.parse(r.stdout), {
    outcome: 'issue-snapshot-written',
    bytes: Buffer.byteLength(JSON.stringify(snapshot) + '\n'),
    id: 'APP-1',
    identifier: 'APP-1',
    uuid: 'uuid',
    state: 'Planned',
    attachments: 0,
  })
  assert.deepEqual(readdirSync(r.dir), ['snapshot.json'])
})
for (const [name, adapter] of [
  ['unsupported', {}],
  [
    'credentials',
    {
      readIssueSnapshot: async () => {
        throw Object.assign(new Error('missing'), { code: 'TRACKER_CREDENTIALS_MISSING' })
      },
    },
  ],
  [
    'transport',
    {
      readIssueSnapshot: async () => {
        throw new Error('network\nfailed')
      },
    },
  ],
  ['incomplete', { readIssueSnapshot: async () => ({ id: 'APP-1' }) }],
])
  test(`refuses ${name} without replacing file`, async (t) => {
    const r = await run(t, adapter)
    assert.equal(r.code, 2)
    assert.equal(r.stdout, '')
    assert.equal(readFileSync(r.target, 'utf8'), 'prior')
    assert.equal(r.stderr.trim().split('\n').length, 1)
    if (['unsupported', 'credentials'].includes(name)) assert.match(r.stderr, /getIssue/)
  })
for (const flags of [
  () => [],
  (p) => ['--id', 'APP-1', '--out-file', p, '--id', 'APP-2'],
  (p) => ['--id', '--out-file', p],
  (p) => ['--id', 'APP-1', '--out-file', p, '--unknown'],
  (p) => ['--id', 'APP-1', '--out-file', p, 'extra'],
])
  test('invalid usage preserves output', async (t) => {
    const r = await run(t, undefined, flags)
    assert.equal(r.code, 64)
    assert.equal(r.stdout, '')
    assert.equal(readFileSync(r.target, 'utf8'), 'prior')
  })
test('checks destination directory before reading and cleans failed writes', async (t) => {
  let calls = 0
  const adapter = {
    readIssueSnapshot: async () => {
      calls++
      return snapshot
    },
  }
  const r = await run(t, adapter, (_, dir) => [
    '--id',
    'APP-1',
    '--out-file',
    join(dir, 'missing', 'out'),
  ])
  assert.equal(r.code, 2)
  assert.equal(calls, 0)
  const failed = await run(t, adapter, (_, dir) => {
    const target = join(dir, 'directory')
    mkdirSync(target)
    return ['--id', 'APP-1', '--out-file', target]
  })
  assert.equal(failed.code, 2)
  assert.equal(failed.stdout, '')
  assert.deepEqual(readdirSync(failed.dir).sort(), ['directory', 'snapshot.json'])
})
