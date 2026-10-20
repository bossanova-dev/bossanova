import assert from 'node:assert/strict'
import { execFileSync, spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import test from 'node:test'
import { fileURLToPath } from 'node:url'

import { findGlue } from './check-handwritten-grpc-glue.mjs'

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
const guard = path.join(repoRoot, 'scripts/check-handwritten-grpc-glue.mjs')

function fixture(t) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'grpc-glue-'))
  t.after(() => fs.rmSync(root, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 }))
  fs.mkdirSync(path.join(root, 'scripts'))
  fs.mkdirSync(path.join(root, 'plugins/example'), { recursive: true })
  fs.writeFileSync(path.join(root, 'scripts/handwritten-grpc-glue-baseline.json'), '[]\n')
  execFileSync('git', ['init', '-q', root])
  return root
}

function writeGo(root, body) {
  fs.writeFileSync(path.join(root, 'plugins/example/plugin.go'), `package example\n${body}\n`)
  execFileSync('git', ['add', '--', 'plugins/example/plugin.go'], { cwd: root })
}

function run(root) {
  return spawnSync(process.execPath, [guard, '--root', root], { encoding: 'utf8' })
}

test('guard rejects seeded descriptors and method paths', (t) => {
  const root = fixture(t)
  for (const [body, kind] of [
    ['var desc = grpc.ServiceDesc{}', 'service-descriptor'],
    ['const method = "/bossanova.v1.HostService/ListSessions"', 'literal-method-path'],
  ]) {
    writeGo(root, body)
    const result = run(root)
    assert.equal(result.status, 1, result.stderr)
    assert.match(result.stderr, new RegExp(`plugin.go:2:${kind}`))
    assert.match(result.stdout, /1 files scanned/)
  }
  writeGo(root, 'const method = bossanovav1.HostService_ListSessions_FullMethodName')
  assert.equal(run(root).status, 0)
})

test('guard fails on an empty scan and refuses baseline growth', (t) => {
  const root = fixture(t)
  assert.match(run(root).stderr, /no Go files scanned/)
  writeGo(root, 'var desc = grpc.ServiceDesc{}')
  fs.writeFileSync(
    path.join(root, 'scripts/handwritten-grpc-glue-baseline.json'),
    JSON.stringify(['plugins/example/plugin.go:2:service-descriptor']) + '\n',
  )
  const result = run(root)
  assert.equal(result.status, 1)
  assert.match(result.stderr, /empty baseline cannot grow/)
})

test('scanner distinguishes Go glue from comments and generated constants', () => {
  assert.deepEqual(
    findGlue('// grpc.ServiceDesc{}\n/* "/bossanova.v1.HostService/ListSessions" */'),
    [],
  )
  assert.deepEqual(
    findGlue('const method = bossanovav1.HostService_ListSessions_FullMethodName'),
    [],
  )
  assert.deepEqual(findGlue('const text = "grpc.ServiceDesc{"'), [])
  assert.equal(findGlue('var desc = grpc . ServiceDesc { }').length, 1)
  for (const service of [
    'AgentRunnerService',
    'TaskSourceService',
    'WorkflowService',
    'HostService',
  ]) {
    assert.equal(findGlue(`const method = \`/bossanova.v1.${service}/Example\``).length, 1)
  }
  assert.deepEqual(findGlue('const method = "/bossanova.v1.OrchestratorService/ListSessions"'), [])
})

test('guard passes on the migrated repository with a nonempty scan', () => {
  const result = run(repoRoot)
  assert.equal(result.status, 0, result.stderr)
  assert.match(result.stdout, /[1-9]\d* files scanned/)
})
