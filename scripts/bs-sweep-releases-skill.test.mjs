// Behaviour tests for the bs-sweep-releases cron gate.

import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { test } from 'node:test'

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
const gatePath = path.join(repoRoot, '.claude/skills/bs-sweep-releases/gate/gate.mjs')

function writeFakeBoss(dir, output = '[]') {
  const fake = path.join(dir, 'boss')
  fs.writeFileSync(fake, `#!/bin/sh\nprintf '%s\\n' '${output}'\n`)
  fs.chmodSync(fake, 0o755)
  return fake
}

function writeFakeGh(dir) {
  const fake = path.join(dir, 'gh')
  fs.writeFileSync(
    fake,
    `#!/bin/sh
case "$*" in
  *'/comments?'*)
    printf '%s\\n' '[[{"user":{"login":"chatgpt-codex-connector[bot]"},"path":"services/example.go","line":12,"body":"![P1 Badge](x) Release regression","html_url":"https://example.test/org/repo/pull/1#discussion_r1"}]]'
    ;;
  *'base=staging'*)
    printf 'HTTP/1.1 200 OK\\r\\n\\r\\n'
    printf '%s\\n' '[{"number":1,"base":{"ref":"staging"},"created_at":"2099-01-01T00:00:00Z","updated_at":"2099-01-01T00:00:00Z","state":"open"}]'
    ;;
  *)
    printf 'HTTP/1.1 200 OK\\r\\n\\r\\n[]'
    ;;
esac
`,
  )
  fs.chmodSync(fake, 0o755)
  return fake
}

function runGate({ bossBin, path: pathEntries, cwd }) {
  return spawnSync(process.execPath, [gatePath], {
    cwd,
    env: { BOSS_BIN: bossBin, PATH: pathEntries },
    encoding: 'utf8',
  })
}

test('live gate succeeds when BOSS_BIN names a healthy boss binary', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'bs-sweep-releases-boss-ok-'))
  try {
    writeFakeGh(dir)
    const result = runGate({ bossBin: writeFakeBoss(dir), path: dir, cwd: dir })
    assert.equal(result.status, 0, result.stderr)
  } finally {
    fs.rmSync(dir, { recursive: true, force: true })
  }
})

test('live gate falls back to PATH when BOSS_BIN names a deleted worktree', () => {
  const pathDir = fs.mkdtempSync(path.join(os.tmpdir(), 'bs-sweep-releases-boss-path-'))
  const reaped = fs.mkdtempSync(path.join(os.tmpdir(), 'bs-sweep-releases-reaped-worktree-'))
  const stale = path.join(reaped, 'bin', 'boss')
  fs.rmSync(reaped, { recursive: true, force: true })
  try {
    writeFakeGh(pathDir)
    writeFakeBoss(pathDir)
    const result = runGate({ bossBin: stale, path: pathDir, cwd: pathDir })
    assert.equal(result.status, 0, result.stderr)
  } finally {
    fs.rmSync(pathDir, { recursive: true, force: true })
  }
})

test('live gate reports the resolver reason when no boss binary resolves at all', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'bs-sweep-releases-boss-missing-'))
  const stale = path.join(dir, 'gone', 'bin', 'boss')
  try {
    writeFakeGh(dir)
    const result = runGate({ bossBin: stale, path: dir, cwd: dir })
    assert.notEqual(result.status, 0)
    assert.match(result.stderr, /no\s+usable\s+boss\s+executable/)
    assert.ok(result.stderr.includes(stale), `reason must name BOSS_BIN: ${result.stderr}`)
    assert.doesNotMatch(result.stderr, /ENOENT|spawnSync/)
  } finally {
    fs.rmSync(dir, { recursive: true, force: true })
  }
})
