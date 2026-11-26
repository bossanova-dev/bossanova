import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import test from 'node:test'
import { fileURLToPath } from 'node:url'
import { resolvePinnedBuf } from './pinned-buf.mjs'

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..')
const helper = path.join(repoRoot, 'scripts/dev/proto-breaking-check.mjs')

function requireBuf(t) {
  const result = resolvePinnedBuf({ repoRoot })
  if ('error' in result) assert.fail(result.error)
  if ('skip' in result) {
    assert.notEqual(process.env.GUARD_PROOF_REQUIRE_TOOLS, '1', result.skip)
    t.skip(result.skip)
    return null
  }
  return result.buf
}

function tempDir(t, prefix) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), prefix))
  t.after(() => fs.rmSync(dir, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 }))
  return dir
}

function git(cwd, ...args) {
  const result = spawnSync('git', args, { cwd, encoding: 'utf8' })
  assert.equal(result.status, 0, `git ${args.join(' ')}: ${result.stderr}`)
  return result.stdout.trim()
}

const protoPath = 'proto/seed/v1/seed.proto'

function writeProto(dir, { featureField = 'string name = 1;', mainOnly = false } = {}) {
  fs.writeFileSync(
    path.join(dir, protoPath),
    `syntax = "proto3";
package seed.v1;

message Kept {
  ${featureField}
}
${mainOnly ? '\nmessage MainOnly {\n  string id = 1;\n}\n' : ''}`,
  )
}

function commit(dir, message) {
  git(dir, 'add', '-A')
  git(dir, 'commit', '-q', '-m', message)
}

// main adds MainOnly after feature branched off; feature changes only its own field set.
function staleBranchRepo(t, featureField) {
  const dir = tempDir(t, 'proto-breaking-base-')
  git(dir, 'init', '-q', '-b', 'main')
  git(dir, 'config', 'commit.gpgsign', 'false')
  git(dir, 'config', 'user.email', 'fixture@example.invalid')
  git(dir, 'config', 'user.name', 'fixture')
  fs.writeFileSync(
    path.join(dir, 'buf.yaml'),
    'version: v2\nmodules:\n  - path: proto\nbreaking:\n  use:\n    - FILE\n',
  )
  fs.mkdirSync(path.join(dir, path.dirname(protoPath)), { recursive: true })
  writeProto(dir)
  commit(dir, 'base')
  git(dir, 'checkout', '-q', '-b', 'feature')
  writeProto(dir, { featureField })
  commit(dir, 'feature change')
  git(dir, 'checkout', '-q', 'main')
  writeProto(dir, { mainOnly: true })
  commit(dir, 'main adds MainOnly')
  git(dir, 'update-ref', 'refs/remotes/origin/main', 'main')
  git(dir, 'checkout', '-q', 'feature')
  return dir
}

function check(cwd, buf, against = '') {
  const result = spawnSync('node', [helper, '--buf', buf, '--against', against], {
    cwd,
    encoding: 'utf8',
    timeout: 60_000,
  })
  assert.ifError(result.error)
  return { status: result.status, output: result.stdout + result.stderr }
}

test('default base ignores proto main added after the branch point; the tip form reports it deleted', (t) => {
  const buf = requireBuf(t)
  if (!buf) return
  const dir = staleBranchRepo(t, 'string name = 1;\n  string added = 2;')

  const merged = check(dir, buf)
  assert.equal(merged.status, 0, merged.output)

  const tip = check(dir, buf, '.git#branch=origin/main')
  assert.notEqual(tip.status, 0, tip.output)
  assert.match(tip.output, /Previously present message "MainOnly" was deleted/)
})

test('default base still fails a deletion the branch itself makes', (t) => {
  const buf = requireBuf(t)
  if (!buf) return
  const dir = staleBranchRepo(t, '')

  const result = check(dir, buf)
  assert.notEqual(result.status, 0, result.output)
  assert.match(result.output, /field "1" with name "name" on message "Kept" was deleted/)
  assert.doesNotMatch(result.output, /MainOnly/)
})

test('a missing origin/main fails loudly instead of falling back to a tip', (t) => {
  const dir = tempDir(t, 'proto-breaking-noorigin-')
  git(dir, 'init', '-q', '-b', 'main')
  const result = check(dir, 'buf-never-runs')
  assert.equal(result.status, 2, result.output)
  assert.match(result.output, /origin\/main does not resolve to a commit/)
})

test('a shallow clone without the merge base fails loudly and says it is shallow', (t) => {
  const source = staleBranchRepo(t, 'string name = 1;\n  string added = 2;')
  const dir = path.join(tempDir(t, 'proto-breaking-shallow-'), 'clone')
  git(
    os.tmpdir(),
    'clone',
    '-q',
    '--depth',
    '1',
    '--no-single-branch',
    '--branch',
    'feature',
    `file://${source}`,
    dir,
  )
  const result = check(dir, 'buf-never-runs')
  assert.equal(result.status, 2, result.output)
  assert.match(result.output, /no merge base of HEAD and origin\/main \(this clone is shallow\)/)
})
