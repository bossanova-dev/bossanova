// guard-id: proto-rpc-naming
import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import test from 'node:test'
import { fileURLToPath } from 'node:url'

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..')

function lintSeed(t, clean) {
  const buf = process.env.BUF || 'buf'
  const probe = spawnSync(buf, ['--version'], { encoding: 'utf8' })
  if (probe.error?.code === 'ENOENT') {
    assert.notEqual(process.env.GUARD_PROOF_REQUIRE_TOOLS, '1', 'buf is required on PATH')
    t.skip('buf is not on PATH')
    return null
  }
  assert.equal(probe.status, 0, probe.stderr || probe.error?.message)
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
