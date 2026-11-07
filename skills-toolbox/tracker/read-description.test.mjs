import { test } from 'node:test'
import assert from 'node:assert/strict'
import { writeFileSync, readFileSync, readdirSync } from 'node:fs'
import { join } from 'node:path'
import { tempDir } from '../../scripts/test-infra/test-tmpdir.mjs'
import { runCli } from './cli.mjs'

async function run(t, values, flags = []) {
  const dir = tempDir('settled-description-', t)
  const expect = join(dir, 'expect.md')
  const stale = join(dir, 'stale.md')
  const output = join(dir, 'output.md')
  writeFileSync(expect, 'new')
  writeFileSync(stale, 'old')
  let time = 0
  let calls = 0
  let stdout = ''
  let stderr = ''
  const args =
    typeof flags === 'function'
      ? flags({ expect, stale, dir })
      : ['--expect-file', expect, '--stale-file', stale, ...flags]
  const code = await runCli(['read-description', '--id', 'ABC-1', '--out-file', output, ...args], {
    write: (s) => {
      stdout += s
    },
    errWrite: (s) => {
      stderr += s
    },
    resolveAdapter: () => ({
      readDescription: async () => {
        const value = values[Math.min(calls++, values.length - 1)]
        if (value instanceof Error) throw value
        return { id: 'uuid', identifier: 'ABC-1', description: value }
      },
    }),
    now: () => time,
    sleep: async (ms) => {
      time += ms
    },
  })
  return { code, calls, stdout, stderr, dir, output, receipt: stdout ? JSON.parse(stdout) : null }
}
for (const [name, values, attempts] of [
  ['expect match', ['new'], 1],
  ['lagged write', ['old', 'old', 'new'], 3],
  ['normalized write', ['normalized'], 1],
]) {
  test(`read-description settles ${name}`, async (t) => {
    const result = await run(t, values)
    assert.equal(result.code, 0)
    assert.equal(result.receipt.settled, true)
    assert.equal(result.receipt.attempts, attempts)
    assert.equal(result.receipt.elapsedMs, (attempts - 1) * 5000)
    assert.equal(readFileSync(result.output, 'utf8'), values.at(-1))
  })
}
test('a no-op save settles even when expect and stale are identical', async (t) => {
  const result = await run(t, ['old'], ({ stale }) => [
    '--expect-file',
    stale,
    '--stale-file',
    stale,
  ])
  assert.equal(result.receipt.settled, true)
  assert.equal(result.calls, 1)
})
test('budget exhaustion writes the last stale read and succeeds with an unsettled receipt', async (t) => {
  const result = await run(t, ['old'], ['--settle-ms', '10000'])
  assert.equal(result.code, 0)
  assert.equal(result.receipt.settled, false)
  assert.equal(result.calls, 3)
  assert.equal(readFileSync(result.output, 'utf8'), 'old')
})
for (const flags of [
  () => ['--settle-ms', '1'],
  ({ expect }) => ['--expect-file', expect, '--settle-ms', '0'],
  ({ expect }) => ['--expect-file', expect, '--settle-ms', '300001'],
  ({ expect }) => ['--expect-file', expect, '--expect-file', expect],
  ({ dir }) => ['--expect-file', join(dir, 'missing')],
]) {
  test(`invalid settle flags refuse before adapter read: ${flags}`, async (t) => {
    const result = await run(t, ['new'], flags)
    assert.equal(result.code, 64)
    assert.equal(result.stdout, '')
    assert.equal(result.calls, 0)
  })
}
test('transport failure mid-poll leaves no partial or temporary file', async (t) => {
  const result = await run(t, ['old', new Error('transport')])
  assert.equal(result.code, 2)
  assert.equal(result.stdout, '')
  assert.match(result.stderr, /transport/)
  assert.deepEqual(readdirSync(result.dir).sort(), ['expect.md', 'stale.md'])
})
