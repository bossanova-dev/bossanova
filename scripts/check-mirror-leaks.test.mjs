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
