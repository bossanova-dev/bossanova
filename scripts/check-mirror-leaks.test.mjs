#!/usr/bin/env node

import assert from 'node:assert/strict'
import { execFileSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { after, test } from 'node:test'

const scriptPath = path.join(path.dirname(fileURLToPath(import.meta.url)), 'check-mirror-leaks.sh')

const tempRoots = []

function git(cwd, ...args) {
  return execFileSync('git', args, { cwd, encoding: 'utf8' })
}

// Create a fresh repo with a single empty "base" commit. Returns the repo dir
// and the base SHA so tests can scan base..HEAD like the mirror does.
function initRepo() {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'mirror-leak-'))
  tempRoots.push(dir)
  git(dir, 'init', '-q', '-b', 'main')
  git(dir, 'config', 'user.email', 'test@example.com')
  git(dir, 'config', 'user.name', 'Test')
  git(dir, 'config', 'commit.gpgsign', 'false')
  git(dir, 'commit', '-q', '--allow-empty', '-m', 'base')
  return { dir, base: git(dir, 'rev-parse', 'HEAD').trim() }
}

function commit(dir, files, message) {
  for (const [rel, content] of Object.entries(files)) {
    const full = path.join(dir, rel)
    fs.mkdirSync(path.dirname(full), { recursive: true })
    fs.writeFileSync(full, content)
    git(dir, 'add', rel)
  }
  git(dir, 'commit', '-q', '-m', message)
}

function removeFile(dir, rel, message) {
  git(dir, 'rm', '-q', rel)
  git(dir, 'commit', '-q', '-m', message)
}

function runGuard(dir, base, head = 'public-mirror-head') {
  // The mirror is always on the branch being pushed; alias HEAD so the guard's
  // arguments read like the real invocation.
  git(dir, 'branch', '-f', head)
  try {
    const stdout = execFileSync('bash', [scriptPath, base, head], {
      cwd: dir,
      encoding: 'utf8',
      // Capture stderr onto the result instead of letting execFileSync echo the
      // guard's "LEAK …/refusing to push" diagnostics to the parent's stderr.
      stdio: ['ignore', 'pipe', 'pipe'],
    })
    return { code: 0, stdout, stderr: '' }
  } catch (err) {
    return {
      code: err.status ?? 1,
      stdout: err.stdout?.toString() ?? '',
      stderr: err.stderr?.toString() ?? '',
    }
  }
}

// Not a git repo (no `git init`), so the guard's git producers (rev-list,
// ls-tree, show) cannot succeed here. Invokes the script directly, bypassing
// runGuard's `git branch -f` wiring which assumes a real repo.
function runInDir(dir, base, head) {
  tempRoots.push(dir)
  try {
    const stdout = execFileSync('bash', [scriptPath, base, head], {
      cwd: dir,
      encoding: 'utf8',
      stdio: ['ignore', 'pipe', 'pipe'],
    })
    return { code: 0, stdout, stderr: '' }
  } catch (err) {
    return {
      code: err.status ?? 1,
      stdout: err.stdout?.toString() ?? '',
      stderr: err.stderr?.toString() ?? '',
    }
  }
}

after(() => {
  for (const dir of tempRoots) {
    fs.rmSync(dir, { recursive: true, force: true })
  }
})

test('passes when new commits carry only public files', () => {
  const { dir, base } = initRepo()
  commit(dir, { 'README.md': '# hi\n', 'services/boss/main.go': 'package main\n' }, 'public work')

  const result = runGuard(dir, base)

  assert.equal(result.code, 0, result.stderr)
  assert.match(result.stdout, /Leak guard passed/)
})

test('fails on a forbidden root file (AGENTS.md / CLAUDE.md)', () => {
  const { dir, base } = initRepo()
  commit(dir, { 'AGENTS.md': 'internal\n' }, 'leak agents')

  const result = runGuard(dir, base)

  assert.equal(result.code, 1)
  assert.match(result.stderr, /forbidden path: AGENTS\.md/)
})

test('fails on a stray root plugin binary but not plugins/ sources', () => {
  const leaked = initRepo()
  commit(leaked.dir, { 'bossd-plugin-repair': 'binary\n' }, 'leak binary')
  assert.equal(runGuard(leaked.dir, leaked.base).code, 1)

  const safe = initRepo()
  commit(safe.dir, { 'plugins/bossd-plugin-repair/main.go': 'package main\n' }, 'plugin source')
  assert.equal(runGuard(safe.dir, safe.base).code, 0)
})

test('fails on private env assignments but allows public BOSS_/BOSSD_ ones', () => {
  const leaked = initRepo()
  commit(leaked.dir, { '.env.example': 'BOSSO_WORKOS_CLIENT_ID=client_secret\n' }, 'leak env')
  const leakedResult = runGuard(leaked.dir, leaked.base)
  assert.equal(leakedResult.code, 1)
  assert.match(leakedResult.stderr, /private env assignment in \.env\.example/)

  const safe = initRepo()
  commit(
    safe.dir,
    { '.env.example': 'CODESIGN_IDENTITY=\nBOSS_WORKOS_CLIENT_ID=\nBOSSD_ORCHESTRATOR_URL=\n' },
    'sanitized env',
  )
  assert.equal(runGuard(safe.dir, safe.base).code, 0)
})

test('fails on the .env.example.public source variant reaching public', () => {
  const { dir, base } = initRepo()
  commit(dir, { '.env.example.public': 'CODESIGN_IDENTITY=\n' }, 'leak source variant')

  const result = runGuard(dir, base)

  assert.equal(result.code, 1)
  assert.match(result.stderr, /forbidden path: \.env\.example\.public/)
})

test('catches an intermediate leak even when the final tree is clean', () => {
  const { dir, base } = initRepo()
  commit(dir, { 'AGENTS.md': 'internal\n' }, 'intermediate leak')
  removeFile(dir, 'AGENTS.md', 'remove leak from final tree')

  // Final tree is clean, but the range base..HEAD still contains the leak.
  const result = runGuard(dir, base)

  assert.equal(result.code, 1, 'guard must scan the commit range, not just the final tree')
  assert.match(result.stderr, /forbidden path: AGENTS\.md/)
})

test('fails on private directories (plans/, docs/)', () => {
  const { dir, base } = initRepo()
  commit(dir, { 'plans/x.md': 'plan\n', 'docs/y.md': 'doc\n' }, 'leak dirs')

  const result = runGuard(dir, base)

  assert.equal(result.code, 1)
  assert.match(result.stderr, /forbidden path: (plans|docs)\//)
})

test('scans all of HEAD when the base ref does not resolve (first-run orphan)', () => {
  const { dir } = initRepo()
  commit(dir, { 'CLAUDE.md': 'internal\n' }, 'leak on orphan')

  const result = runGuard(dir, 'does-not-exist')

  assert.equal(result.code, 1)
  assert.match(result.stderr, /forbidden path: CLAUDE\.md/)
})

test('fails when tree contains services/mcp-gateway (private hosted gateway)', () => {
  const { dir, base } = initRepo()
  commit(dir, { 'services/mcp-gateway/cmd/main.go': 'package main\n' }, 'leak gateway')

  const result = runGuard(dir, base)

  assert.equal(result.code, 1, result.stderr)
  assert.match(result.stderr, /forbidden path: services\/mcp-gateway\//)
})

test('passes when tree contains services/mcp and lib/bossalib/bossmcp (both public)', () => {
  const { dir, base } = initRepo()
  commit(
    dir,
    {
      'services/mcp/cmd/main.go': 'package main\n',
      'lib/bossalib/bossmcp/tools.go': 'package bossmcp\n',
    },
    'public mcp paths',
  )

  const result = runGuard(dir, base)

  assert.equal(result.code, 0, result.stderr)
  assert.match(result.stdout, /Leak guard passed/)
})

// Delete a loose object from the repo's object store, so a specific git
// producer downstream of it fails while the ones upstream still succeed. This
// is what lets each of the three hoists be exercised independently.
function deleteObject(dir, sha) {
  fs.rmSync(path.join(dir, '.git', 'objects', sha.slice(0, 2), sha.slice(2)), { force: true })
}

test('fails closed when git rev-list cannot run (not a git repo at all)', () => {
  // A plain mkdtemp dir with no `git init`, so `git rev-list` cannot succeed.
  // Note this case reaches ONLY the rev-list hoist: the script aborts there
  // under `set -e` and never gets as far as ls-tree or git show, which is why
  // those two have dedicated cases below. Before the fix, the failing
  // `git rev-list` inside `for sha in $(...)` yielded zero iterations instead
  // of aborting, so `leak` stayed 0 and the script printed a false pass.
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'mirror-leak-nogit-'))
  const result = runInDir(dir, 'base', 'head')

  assert.notEqual(result.code, 0)
  assert.doesNotMatch(result.stdout, /Leak guard passed/)
})

test('fails closed when git ls-tree cannot read a commit tree (rev-list still succeeds)', () => {
  // Removing the commit's tree object leaves rev-list working (it only walks
  // commit objects) but makes ls-tree fail — isolating the second hoist. Before
  // the fix, `done < <(git ls-tree …)` discarded that failure, scanned zero
  // paths, and printed the pass line.
  const { dir, base } = initRepo()
  commit(dir, { 'README.md': '# hi\n' }, 'a commit whose tree we then break')
  deleteObject(dir, git(dir, 'rev-parse', 'HEAD^{tree}').trim())

  const result = runGuard(dir, base)

  assert.notEqual(result.code, 0)
  assert.doesNotMatch(result.stdout, /Leak guard passed/)
})

test('fails closed when git show cannot read a .env* blob (ls-tree still succeeds)', () => {
  // Removing only the .env.local blob leaves rev-list and ls-tree working, so
  // the run reaches the content check — isolating the third hoist. Before the
  // fix, `if git show … | grep -q …` swallowed the failure inside the `if`
  // condition and a missing blob read exactly like "no private assignment".
  const { dir, base } = initRepo()
  commit(dir, { '.env.local': 'PUBLIC_OK=1\n' }, 'env file whose blob we then break')
  deleteObject(dir, git(dir, 'rev-parse', 'HEAD:.env.local').trim())

  const result = runGuard(dir, base)

  assert.notEqual(result.code, 0)
  assert.doesNotMatch(result.stdout, /Leak guard passed/)
})

test('catches a private assignment in a .env* blob larger than the pipe buffer', () => {
  // `grep -q` exits on its first match, so feeding it a large blob through a
  // pipe kills the writer with SIGPIPE and `pipefail` turns the pipeline's
  // status into 141 — the `if` takes the false branch and the guard certifies
  // a commit that really does carry a secret. Verified against the pre-fix
  // script: 200 KB with the secret on line 1 printed "Leak guard passed",
  // exit 0, while the same secret in a small file was caught.
  const { dir, base } = initRepo()
  commit(
    dir,
    { '.env.example': `BOSSO_WORKOS_CLIENT_SECRET=supersecret\n${'A'.repeat(200_000)}\n` },
    'oversized env file with a secret on line 1',
  )

  const result = runGuard(dir, base)

  assert.equal(result.code, 1)
  assert.doesNotMatch(result.stdout, /Leak guard passed/)
  assert.match(result.stderr, /private env assignment in \.env\.example/)
})

test('refuses to certify a path git still C-quotes (quote in the filename)', () => {
  // core.quotePath only unquotes bytes >= 0x80; git C-quotes `"`, `\` and
  // control characters regardless. Such a path cannot be matched against
  // FORBIDDEN_PATH_RE nor handed back to `git show`, so the gate must fail
  // closed rather than pass a path it never actually evaluated.
  const { dir, base } = initRepo()
  commit(dir, { 'docs/we"ird.md': 'private\n' }, 'quoted path')

  const result = runGuard(dir, base)

  assert.equal(result.code, 1)
  assert.doesNotMatch(result.stdout, /Leak guard passed/)
  assert.match(result.stderr, /unparseable \(C-quoted\) path/)
})

test('catches a forbidden path whose name is non-ASCII (git C-quoting does not hide it)', () => {
  // git C-quotes non-ASCII paths by default, so `docs/wéird.md` reaches the
  // path check as the literal "docs/w\303\251ird.md" — which does not match
  // FORBIDDEN_PATH_RE's `^docs/`, and the private file sails onto the public
  // mirror under a "Leak guard passed". core.quotePath=false is what stops it.
  const { dir, base } = initRepo()
  commit(dir, { 'docs/wéird.md': 'private\n' }, 'non-ascii forbidden path')

  const result = runGuard(dir, base)

  assert.equal(result.code, 1)
  assert.doesNotMatch(result.stdout, /Leak guard passed/)
  assert.match(result.stderr, /forbidden path/)
})

test('passes on a clean range whose commits contain no .env* file at all (|| true preserved)', () => {
  // Distinct from "passes when new commits carry only public files" above:
  // this case exists specifically to pin the `|| true` on the .env grep
  // (site 3) so a future edit that drops or widens it is caught immediately,
  // even though the existing public-files case already exercises this shape.
  const { dir, base } = initRepo()
  commit(
    dir,
    { 'README.md': '# hi\n', 'services/boss/main.go': 'package main\n' },
    'no env files at all',
  )

  const result = runGuard(dir, base)

  assert.equal(result.code, 0, result.stderr)
  assert.match(result.stdout, /Leak guard passed/)
})
