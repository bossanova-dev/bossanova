import assert from 'node:assert/strict'
import { execFileSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { after, test } from 'node:test'

const SCRIPT = path.join(path.dirname(fileURLToPath(import.meta.url)), 'nosec-metadata-check.sh')
const tempDirs = []

function initRepo(source) {
  const repo = fs.mkdtempSync(path.join(os.tmpdir(), 'nosec-metadata-check-'))
  tempDirs.push(repo)
  execFileSync('git', ['init', '-q', repo])
  fs.writeFileSync(path.join(repo, 'main.go'), source)
  execFileSync('git', ['-C', repo, 'add', 'main.go'])
  return repo
}

function runCheck(source) {
  const repo = initRepo(source)
  try {
    const stdout = execFileSync('bash', [SCRIPT], { cwd: repo, encoding: 'utf8' })
    return { code: 0, stdout }
  } catch (error) {
    return { code: error.status ?? 1, stdout: error.stdout?.toString() ?? '' }
  }
}

after(() => {
  for (const dir of tempDirs) fs.rmSync(dir, { recursive: true, force: true })
})

test('nosec metadata check rejects non-gosec rule IDs', () => {
  const result = runCheck('package example\n\n// #nosec X204 -- malformed rule ID\nfunc f() {}\n')

  assert.notEqual(result.code, 0)
  assert.match(result.stdout, /non-compliant #nosec suppression/)
})

test('nosec metadata check accepts G### rule IDs', () => {
  const result = runCheck(
    'package example\n\n// #nosec G204 -- verified subprocess input\n// owner=@recurser review-by=2027-01-18 issue=BOS-28\nfunc f() {}\n',
  )

  assert.equal(result.code, 0, result.stdout)
})

test('nosec metadata check rejects missing suppression approval metadata', () => {
  const result = runCheck(
    'package example\n\n// #nosec G204 -- verified subprocess input\nfunc f() {}\n',
  )

  assert.notEqual(result.code, 0)
  assert.match(result.stdout, /owner=@handle, review-by=YYYY-MM-DD, and issue=BOS-NN/)
})

test('nosec metadata check rejects approval metadata in place of a reason', () => {
  const result = runCheck(
    'package example\n\n// #nosec G204 -- owner=@recurser review-by=2027-01-18 issue=BOS-28\nfunc f() {}\n',
  )

  assert.notEqual(result.code, 0)
  assert.match(result.stdout, /non-metadata reason before approval metadata/)
})
