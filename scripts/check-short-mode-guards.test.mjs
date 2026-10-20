#!/usr/bin/env node

import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import test from 'node:test'

const repoRoot = path.resolve(path.dirname(new URL(import.meta.url).pathname), '..')
const slowTestFiles = [
  'services/boss/internal/tuitest/cron_test.go',
  'services/boss/internal/tuitest/newsession_test.go',
  'services/boss/internal/tuitest/navigation_test.go',
  'services/boss/internal/tuitest/login_subscription_test.go',
  'services/bossd/internal/session/tmux_chat_test.go',
  'services/bossd/internal/status/tmux_poller_test.go',
  'services/bossd/internal/testharness/e2e_record_chat_tmux_test.go',
]

// The gate: each listed slow test file under `root` guards itself with testing.Short()
// and names the command that runs it in full.
function assertSlowTestsGuardShortMode(root, files) {
  for (const file of files) {
    const text = fs.readFileSync(path.join(root, file), 'utf8')
    assert.match(text, /testing\.Short\(\)/, file)
    assert.match(text, /run .*full|make test-|go test/i, file)
  }
}

test('slow tmux and TUI test files explicitly support go test -short', () => {
  assertSlowTestsGuardShortMode(repoRoot, slowTestFiles)
})

test('a slow test file without a testing.Short() guard fails the gate', () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'short-mode-guards-'))
  try {
    fs.writeFileSync(
      path.join(root, 'guarded_test.go'),
      'package x\n\nfunc TestSlow(t *testing.T) {\n\tif testing.Short() {\n\t\tt.Skip("run make test-x for the full suite")\n\t}\n}\n',
    )
    fs.writeFileSync(
      path.join(root, 'unguarded_test.go'),
      'package x\n\n// Run with make test-x.\nfunc TestSlow(t *testing.T) {}\n',
    )
    assertSlowTestsGuardShortMode(root, ['guarded_test.go'])
    assert.throws(
      () => assertSlowTestsGuardShortMode(root, ['guarded_test.go', 'unguarded_test.go']),
      {
        message: 'unguarded_test.go',
      },
    )
  } finally {
    fs.rmSync(root, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 })
  }
})
