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

// Not a git repo (no `git init`), so `git ls-files` cannot succeed there.
function runInDir(dir) {
  tempDirs.push(dir)
  try {
    const stdout = execFileSync('bash', [SCRIPT], { cwd: dir, encoding: 'utf8' })
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

test('nosec metadata check fails closed when git ls-files cannot run', () => {
  // A plain mkdtemp dir with no `git init`: `git ls-files` cannot succeed here,
  // so the gate must fail closed rather than silently scan zero files and pass.
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'nosec-metadata-check-nogit-'))
  const result = runInDir(dir)

  assert.notEqual(result.code, 0)
  assert.doesNotMatch(result.stdout, /all suppressions use an auditable/)
})

test('nosec metadata check scans a Go file whose name is non-ASCII', () => {
  // git C-quotes non-ASCII paths by default, so `café.go` arrives as the literal
  // "caf\303\251.go". That name cannot be opened, the `grep … 2>/dev/null ||
  // continue` guard skips the file silently, and its bare suppression is
  // certified as auditable. core.quotePath=false is what keeps it scanned.
  const repo = fs.mkdtempSync(path.join(os.tmpdir(), 'nosec-metadata-check-utf8-'))
  tempDirs.push(repo)
  execFileSync('git', ['init', '-q', repo])
  fs.writeFileSync(path.join(repo, 'café.go'), 'package example\n\n// #nosec\nfunc f() {}\n')
  execFileSync('git', ['-C', repo, 'add', '-A'])

  let result
  try {
    result = { code: 0, stdout: execFileSync('bash', [SCRIPT], { cwd: repo, encoding: 'utf8' }) }
  } catch (error) {
    result = { code: error.status ?? 1, stdout: error.stdout?.toString() ?? '' }
  }

  assert.notEqual(result.code, 0)
  assert.doesNotMatch(result.stdout, /all suppressions use an auditable/)
  assert.match(result.stdout, /non-compliant #nosec suppression/)
})

test('nosec metadata check refuses to certify a Go file it cannot open', () => {
  // core.quotePath only unquotes bytes >= 0x80; git still C-quotes `"`, `\`
  // and control characters, and the quoted name cannot be opened. Without the
  // readability check the `grep … 2>/dev/null || continue` guard swallows that
  // and skips the file, certifying a bare suppression it never read.
  const repo = fs.mkdtempSync(path.join(os.tmpdir(), 'nosec-metadata-check-quoted-'))
  tempDirs.push(repo)
  execFileSync('git', ['init', '-q', repo])
  fs.mkdirSync(path.join(repo, 'pkg'), { recursive: true })
  fs.writeFileSync(
    path.join(repo, 'pkg', 'we"ird.go'),
    'package example\n\n// #nosec\nfunc f() {}\n',
  )
  execFileSync('git', ['-C', repo, 'add', '-A'])

  let result
  try {
    result = { code: 0, stdout: execFileSync('bash', [SCRIPT], { cwd: repo, encoding: 'utf8' }) }
  } catch (error) {
    result = { code: error.status ?? 1, stdout: error.stdout?.toString() ?? '' }
  }

  assert.notEqual(result.code, 0)
  assert.doesNotMatch(result.stdout, /all suppressions use an auditable/)
  assert.match(result.stdout, /unreadable Go file/)
})
