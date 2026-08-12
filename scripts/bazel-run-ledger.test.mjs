#!/usr/bin/env node

// Behavior contract for scripts/bazel/run-ledger.mjs — the native ledger step the
// `make` facade runs after `bazel test //...` (BOS-339). Uses --print / LEDGER_DRY_RUN
// so nothing executes: we assert on the printed command construction, not test runs.

import assert from 'node:assert/strict'
import { execFileSync } from 'node:child_process'
import path from 'node:path'
import test from 'node:test'

const repoRoot = path.resolve(path.dirname(new URL(import.meta.url).pathname), '..')
const script = path.join(repoRoot, 'scripts/bazel/run-ledger.mjs')

function run(args, extraEnv = {}) {
  return execFileSync('node', [script, ...args], {
    cwd: repoRoot,
    encoding: 'utf8',
    env: { ...process.env, ...extraEnv },
  })
}

test('an unknown --disposition fails loudly (exit 2), not a silent empty run', () => {
  let threw = false
  try {
    run(['--disposition', 'defualt-run', '--print'])
  } catch (err) {
    threw = true
    assert.equal(err.status, 2, `expected exit 2, got ${err.status}`)
    assert.match(String(err.stderr), /unknown --disposition/)
  }
  assert.ok(threw, 'a mistyped disposition must not exit 0 and skip every manual target')
})

test('--disposition default-run --print lists the default-run commands, not loadtest', () => {
  const out = run(['--disposition', 'default-run', '--print'])
  assert.match(out, /cd lib\/bossalib && go test -timeout 300s \.\/apiversion\/\.\.\./)
  // testharness carries a larger budget than the other rows: RACE=1 injects
  // -race into this command, and under race the real-tmux e2e bundle ran right
  // on a 300s ceiling in CI (300.054s timeout panic vs 300.260s pass).
  assert.match(out, /cd services\/bossd && go test -timeout 900s \.\/internal\/testharness\/\.\.\./)
  // loadtest is explicit-only — it must NOT appear in the default-run selection.
  assert.doesNotMatch(out, /internal\/loadtest/)
})

test('--race injects -race right after "go test "', () => {
  const out = run(['--disposition', 'default-run', '--race', '--print'])
  assert.match(out, /go test -race -timeout 300s/)
  assert.doesNotMatch(out, /go test -timeout 300s/)
})

test('--module services/boss selects only the 5 boss rows', () => {
  const out = run(['--disposition', 'default-run', '--module', 'services/boss', '--print'])
  const lines = out.split('\n').filter((l) => l.includes('go test'))
  assert.equal(lines.length, 5, `expected 5 boss commands, got: ${out}`)
  for (const l of lines) {
    assert.match(l, /cd services\/boss &&/)
  }
})

test('--disposition explicit-only selects only loadtest', () => {
  const out = run(['--disposition', 'explicit-only', '--print'])
  const lines = out.split('\n').filter((l) => l.includes('go test'))
  assert.equal(lines.length, 1, `expected only loadtest, got: ${out}`)
  assert.match(lines[0], /internal\/loadtest -run TestOrchestratorScaleSmoke/)
})

test('LEDGER_DRY_RUN=1 prints without a --print flag and executes nothing', () => {
  const out = run(['--disposition', 'default-run'], { LEDGER_DRY_RUN: '1' })
  assert.match(out, /go test/)
})

test('empty selection (module with no rows) prints a note and exits 0', () => {
  const out = run([
    '--disposition',
    'default-run',
    '--module',
    'plugins/bossd-plugin-claude',
    '--print',
  ])
  assert.doesNotMatch(out, /go test/)
  assert.match(out, /no ledger rows/i)
})
