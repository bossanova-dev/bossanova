// guard-id: proto-rpc-naming
import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import test from 'node:test'
import { fileURLToPath } from 'node:url'

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..')

function requireBuf(t) {
  const candidate = process.env.BUF || 'buf'
  // Make treats a bare BUF override as a repository-relative file, so pass its resolved path.
  const paths = candidate.includes(path.sep)
    ? [path.resolve(candidate)]
    : (process.env.PATH || '').split(path.delimiter).map((dir) => path.resolve(dir, candidate))
  const buf =
    paths.find((file) => {
      try {
        fs.accessSync(file, fs.constants.X_OK)
        return fs.statSync(file).isFile()
      } catch {
        return false
      }
    }) || candidate
  const probe = spawnSync(buf, ['--version'], { encoding: 'utf8' })
  if (probe.error?.code === 'ENOENT') {
    assert.notEqual(process.env.GUARD_PROOF_REQUIRE_TOOLS, '1', 'buf is required on PATH')
    t.skip('buf is not on PATH')
    return null
  }
  assert.equal(probe.status, 0, probe.stderr || probe.error?.message)
  return buf
}

function lintSeed(t, clean) {
  const buf = requireBuf(t)
  if (!buf) return null
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'proto-rpc-naming-proof-'))
  t.after(() => fs.rmSync(dir, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 }))
  fs.copyFileSync(path.join(repoRoot, 'buf.yaml'), path.join(dir, 'buf.yaml'))
  fs.mkdirSync(path.join(dir, 'proto/seed/v1'), { recursive: true })
  fs.writeFileSync(
    path.join(dir, 'proto/seed/v1/seed.proto'),
    `syntax = "proto3";
package seed.v1;

service SeedService {
  rpc DoThing(${clean ? 'DoThingRequest' : 'WrongName'}) returns (DoThingResponse);
}

message ${clean ? 'DoThingRequest' : 'WrongName'} {}
message DoThingResponse {}
`,
  )
  const result = spawnSync(buf, ['lint', '--error-format=json'], {
    cwd: dir,
    encoding: 'utf8',
    timeout: 30_000,
  })
  assert.ifError(result.error)
  return { status: result.status, output: result.stdout + result.stderr }
}

test('buf lint fails a seeded RPC whose request message name does not match', (t) => {
  const result = lintSeed(t, false)
  if (!result) return
  assert.notEqual(result.status, 0, result.output)
  const records = result.output
    .trim()
    .split('\n')
    .map((line) => JSON.parse(line))
  assert.ok(
    records.some((record) => record.type === 'RPC_REQUEST_STANDARD_NAME'),
    result.output,
  )
})

test('buf lint passes the standard-named clean twin', (t) => {
  const result = lintSeed(t, true)
  if (!result) return
  assert.equal(result.status, 0, result.output)
})

test('breaking check repairs invalid base configuration while rejecting a deleted message', (t) => {
  const buf = requireBuf(t)
  if (!buf) return
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'proto-breaking-config-'))
  t.after(() => fs.rmSync(dir, { recursive: true, force: true, maxRetries: 5 }))
  fs.cpSync(path.join(repoRoot, 'proto'), path.join(dir, 'proto'), { recursive: true })
  fs.writeFileSync(
    path.join(dir, 'buf.yaml'),
    'version: v2\nversion: v2\nmodules:\n  - path: proto\n',
  )
  const check = () =>
    spawnSync(
      'make',
      ['--no-print-directory', 'lint-proto-breaking', `BUF=${buf}`, `BUF_BREAKING_BASE=${dir}`],
      { cwd: repoRoot, encoding: 'utf8', timeout: 30_000 },
    )
  const unchanged = check()
  assert.ifError(unchanged.error)
  assert.equal(unchanged.status, 0, unchanged.stdout + unchanged.stderr)

  fs.appendFileSync(
    path.join(dir, 'proto/bossanova/v1/authz.proto'),
    '\nmessage DeletedConfigProof {}\n',
  )
  const deleted = check()
  assert.ifError(deleted.error)
  assert.notEqual(deleted.status, 0)
  assert.match(deleted.stdout + deleted.stderr, /DeletedConfigProof/)
})
