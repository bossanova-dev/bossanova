import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { copyFileSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { pathToFileURL } from 'node:url'
import test from 'node:test'
import hostsParser from '../../../skills-toolbox/ci/gh-hosts.generated.cjs'
const { hostKeys } = hostsParser
import { OUTPUT_FILE, run } from './build.mjs'

function scratch(t) {
  const dir = mkdtempSync(join(tmpdir(), 'gh-host-parser-'))
  t.after(() => rmSync(dir, { recursive: true, force: true, maxRetries: 3 }))
  return dir
}

test('build --check accepts the committed parser', async () => {
  assert.equal((await run({ check: true })).ok, true)
})

test('build --check rejects a modified generated parser', async (t) => {
  const outPath = join(scratch(t), 'parser.mjs')
  await run({ outPath })
  writeFileSync(outPath, 'export const stale = true\n')
  assert.equal((await run({ check: true, outPath })).ok, false)
})

test('regeneration is idempotent and missing artifacts fail their check', async (t) => {
  const outPath = join(scratch(t), 'parser.mjs')
  assert.equal((await run({ check: true, outPath })).ok, false)
  assert.equal((await run({ outPath })).changed, true)
  assert.equal((await run({ outPath })).changed, false)
  assert.equal((await run({ check: true, outPath })).ok, true)
})

test('the copied parser runs under plain Node without packages or credential output', (t) => {
  const dir = scratch(t)
  const outPath = join(dir, 'parser.cjs')
  copyFileSync(OUTPUT_FILE, outPath)
  const input = '--- # managed\n{github.com: {oauth_token: "fixture-secret: {,} # data"}}\n'
  const program = `import parser from ${JSON.stringify(pathToFileURL(outPath).href)}; console.log(JSON.stringify(parser.hostKeys(${JSON.stringify(input)})))`
  const result = spawnSync(process.execPath, ['--input-type=module', '-e', program], {
    cwd: dir,
    env: { HOME: dir },
    encoding: 'utf8',
    timeout: 10000,
  })
  assert.equal(result.status, 0, result.stderr)
  assert.deepEqual(JSON.parse(result.stdout), ['github.com'])
  assert.equal(result.stderr, '')
  assert.ok(!result.stdout.includes('fixture-secret'))
})

test('host keys preserve raw YAML mapping order and duplicates without expanding values', () => {
  assert.deepEqual(hostKeys(''), [])
  assert.deepEqual(hostKeys('{}'), [])
  assert.deepEqual(
    hostKeys(
      '--- # managed\n{"gith\\u0075b.com": &account {user: agent}, ghe.example: *account}\n',
    ),
    ['github.com', 'ghe.example'],
  )
  assert.deepEqual(hostKeys('ghe.example: {user: first}\nghe.example: {user: second}\n'), [
    'ghe.example',
    'ghe.example',
  ])
  assert.deepEqual(hostKeys('? github.com\n: {user: agent}\n'), ['github.com'])
  assert.deepEqual(
    hostKeys('github.com:\n  oauth_token: |\n    other.example: credential text\n'),
    ['github.com'],
  )
})

test('invalid YAML and non-mapping documents fail without diagnostic credential leaks', (t) => {
  const warnings = t.mock.method(console, 'warn', () => {})
  const errors = t.mock.method(console, 'error', () => {})
  for (const text of [
    '[github.com]',
    'fixture-secret',
    'github.com: {oauth_token: fixture-secret',
  ]) {
    assert.throws(
      () => hostKeys(text),
      (error) => {
        assert.ok(!error.message.includes('fixture-secret'))
        return true
      },
    )
  }
  assert.equal(warnings.mock.callCount(), 0)
  assert.equal(errors.mock.callCount(), 0)
})
