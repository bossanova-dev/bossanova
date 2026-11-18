import { test } from 'node:test'
import assert from 'node:assert/strict'
import { existsSync, readFileSync, readdirSync, statSync, writeFileSync } from 'node:fs'
import { join } from 'node:path'
import { spawnSync } from 'node:child_process'
import { fileURLToPath } from 'node:url'
import { tempDir } from '../../scripts/test-infra/test-tmpdir.mjs'
import { runCli } from './cli.mjs'
import { TRACKER_CREDENTIALS_MISSING } from './adapter-core.mjs'

process.env.BOSS_GATE_OUTCOME_FILE = join(tempDir('gate-outcome-suite-'), 'outcomes.tsv')

function fixture(t, premises = []) {
  const dir = tempDir('premise-states-', t)
  const input = join(dir, 'premises.json')
  const output = join(dir, 'states.json')
  writeFileSync(input, JSON.stringify(premises))
  return {
    dir,
    input,
    output,
    async run(resolveAdapter, flags = ['--premises', input, '--out-file', output]) {
      let stdout = '',
        stderr = ''
      const code = await runCli(['premise-states', ...flags], {
        resolveAdapter,
        write: (text) => {
          stdout += text
        },
        errWrite: (text) => {
          stderr += text
        },
      })
      return { code, stdout, stderr }
    },
  }
}
const premise = (id) => ({ id, state: 'Todo' })
const state = (identifier, name = 'Done') => ({
  id: 'uuid',
  identifier,
  state: { name, type: 'completed' },
})

test('empty premises write exactly an empty map without resolving an adapter', async (t) => {
  const f = fixture(t)
  const r = await f.run(() => {
    throw new Error('must not resolve')
  })
  assert.equal(r.code, 0, r.stderr)
  assert.equal(readFileSync(f.output, 'utf8'), '{}')
  assert.deepEqual(JSON.parse(r.stdout), {
    outcome: 'premise-states-written',
    count: 0,
    bytes: 2,
    out: f.output,
  })
})

test('unique requested ids are read in order and receipt measures the written map', async (t) => {
  const f = fixture(t, [premise('ABC-1'), premise('ABC-2'), premise('ABC-1')])
  const calls = []
  const r = await f.run(() => ({
    readIssueState: async (id) => {
      calls.push(id)
      return state(id, id === 'ABC-1' ? 'In Review' : 'Done')
    },
  }))
  assert.equal(r.code, 0, r.stderr)
  assert.deepEqual(calls, ['ABC-1', 'ABC-2'])
  assert.deepEqual(JSON.parse(readFileSync(f.output)), { 'ABC-1': 'In Review', 'ABC-2': 'Done' })
  assert.deepEqual(JSON.parse(r.stdout), {
    outcome: 'premise-states-written',
    count: 2,
    bytes: statSync(f.output).size,
    out: f.output,
  })
})

test('moved identifiers keep requested keys and report aliases', async (t) => {
  const f = fixture(t, [premise('OLD-1')])
  const r = await f.run(() => ({ readIssueState: async () => state('NEW-2') }))
  assert.equal(r.code, 0, r.stderr)
  assert.deepEqual(JSON.parse(readFileSync(f.output)), { 'OLD-1': 'Done' })
  assert.deepEqual(JSON.parse(r.stdout).aliases, { 'OLD-1': 'NEW-2' })
})

for (const [name, resolveAdapter] of [
  ['no capability', () => ({})],
  [
    'missing credentials',
    () => ({
      readIssueState: async () => {
        throw Object.assign(new Error('credential missing'), { code: TRACKER_CREDENTIALS_MISSING })
      },
    }),
  ],
  [
    'second read fails',
    () => ({
      readIssueState: async (id) => {
        if (id === 'ABC-2') throw new Error('transport\nfailed')
        return state(id)
      },
    }),
  ],
  ['unreadable result', () => ({ readIssueState: async () => ({ state: { name: '' } }) })],
  [
    'adapter resolution fails',
    () => {
      throw new Error('unconfigured')
    },
  ],
]) {
  test(`${name} removes stale states and temporary files and names fallback`, async (t) => {
    const f = fixture(t, [premise('ABC-1'), premise('ABC-2')])
    writeFileSync(f.output, '{"ABC-1":"stale"}')
    const r = await f.run(resolveAdapter)
    assert.equal(r.code, 2)
    assert.equal(r.stdout, '')
    assert.match(r.stderr, /getIssue/)
    assert.equal(r.stderr.trim().split('\n').length, 1)
    assert.deepEqual(readdirSync(f.dir), ['premises.json'])
  })
}

for (const malformed of [{}, [{ id: '' }], [{ id: 'ABC-1' }], [null]]) {
  test(`malformed premises refuse usage: ${JSON.stringify(malformed)}`, async (t) => {
    const f = fixture(t, malformed)
    const r = await f.run(() => {
      throw new Error('must not resolve')
    })
    assert.equal(r.code, 64)
    assert.equal(r.stdout, '')
    assert.equal(existsSync(f.output), false)
  })
}

test('invalid flags are usage errors without writing', async (t) => {
  const f = fixture(t)
  for (const flags of [
    [],
    ['--unknown', 'x'],
    ['--premises', f.input],
    ['--premises', ' ', '--out-file', f.output],
    ['--premises', '--oops', '--out-file', f.output],
    ['--premises', f.input, '--premises', f.input, '--out-file', f.output],
  ]) {
    const r = await f.run(() => {
      throw new Error('must not resolve')
    }, flags)
    assert.equal(r.code, 64, JSON.stringify(flags))
    assert.equal(r.stdout, '')
    assert.equal(existsSync(f.output), false)
  }
})

test('write failure leaves no temporary file and names fallback', async (t) => {
  const f = fixture(t)
  const r = await f.run(
    () => ({}),
    ['--premises', f.input, '--out-file', join(f.dir, 'missing', 'states.json')],
  )
  assert.equal(r.code, 2)
  assert.equal(r.stdout, '')
  assert.match(r.stderr, /getIssue/)
  assert.deepEqual(readdirSync(f.dir), ['premises.json'])
})

test('exported states round trip through the guard for matching and drifted states', async (t) => {
  const f = fixture(t, [premise('ABC-1')])
  const guard = fileURLToPath(new URL('../plan/plan-run-guards.mjs', import.meta.url))
  for (const name of ['Todo', 'Done']) {
    const r = await f.run(() => ({ readIssueState: async () => state('ABC-1', name) }))
    assert.equal(r.code, 0, r.stderr)
    const checked = spawnSync(process.execPath, [guard, 'premises', f.input, f.output], {
      encoding: 'utf8',
      env: { ...process.env, BOSS_GATE_OUTCOME_FILE: join(f.dir, 'outcomes.tsv') },
    })
    assert.equal(checked.status, 0, checked.stderr)
    assert.match(checked.stderr, /premises: verified 1 of 1/)
    assert.equal(checked.stderr.includes('premise-drift:'), name !== 'Todo')
  }
})
