#!/usr/bin/env node

// Behavior contract for scripts/bazel/run-ledger.mjs — the native ledger step the
// `make` facade runs after `bazel test //...` (BOS-339). Uses --print / LEDGER_DRY_RUN
// so nothing executes: we assert on the printed command construction, not test runs.

import assert from 'node:assert/strict'
import { execFileSync, spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
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
  assert.match(out, /cd services\/bossd && go test -timeout 900s \.\/internal\/testharness$/m)
  // loadtest is explicit-only — it must NOT appear in the default-run selection.
  assert.doesNotMatch(out, /internal\/loadtest/)
})

test('--race injects -race right after "go test "', () => {
  const out = run(['--disposition', 'default-run', '--race', '--print'])
  assert.match(out, /go test -race -timeout 300s/)
  assert.doesNotMatch(out, /go test -timeout 300s/)
})

test('--module services/boss selects only the 4 boss rows', () => {
  const out = run(['--disposition', 'default-run', '--module', 'services/boss', '--print'])
  const lines = out.split('\n').filter((l) => l.includes('go test'))
  assert.equal(lines.length, 4, `expected 4 boss commands, got: ${out}`)
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

// Retry contract: a row whose label is in the quarantine runs up to 3 times and reports FLAKY
// when a retry passes; an unlisted row still stops on its first failure. Driven for real against
// fixture rows whose command fails until a counter file reaches a threshold.
function retryFixture(t, { failuresBeforePass, quarantined }) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'run-ledger-retry-'))
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }))
  const counter = path.join(dir, 'count')
  const label = '//fixture/pkg:pkg_test'
  const command = `n=$(cat '${counter}' 2>/dev/null || echo 0); n=$((n+1)); echo $n > '${counter}'; [ $n -gt ${failuresBeforePass} ] && echo go test fixture`
  fs.writeFileSync(
    path.join(dir, 'ledger.json'),
    JSON.stringify([{ label, module: 'fixture', command, disposition: 'default-run' }]),
  )
  fs.writeFileSync(path.join(dir, 'flaky.json'), JSON.stringify(quarantined ? [{ label }] : []))
  const env = {
    ...process.env,
    RUN_LEDGER_FILE: path.join(dir, 'ledger.json'),
    RUN_LEDGER_QUARANTINE_FILE: path.join(dir, 'flaky.json'),
  }
  const result = spawnSync('node', [script, '--disposition', 'default-run'], {
    env,
    encoding: 'utf8',
  })
  return { ...result, attempts: Number(fs.readFileSync(counter, 'utf8').trim()) }
}

test('a quarantined row that fails once is retried, passes, and is reported FLAKY', (t) => {
  const result = retryFixture(t, { failuresBeforePass: 1, quarantined: true })
  assert.equal(result.status, 0, result.stderr)
  assert.equal(result.attempts, 2)
  assert.match(result.stdout, /run-ledger: FLAKY \/\/fixture\/pkg:pkg_test passed on attempt 2\/3/)
})

test('a quarantined row that fails every attempt still fails after 3 runs', (t) => {
  const result = retryFixture(t, { failuresBeforePass: 5, quarantined: true })
  assert.notEqual(result.status, 0)
  assert.equal(result.attempts, 3)
  assert.match(result.stderr, /command failed/)
})

test('an unlisted row is not retried', (t) => {
  const result = retryFixture(t, { failuresBeforePass: 1, quarantined: false })
  assert.notEqual(result.status, 0)
  assert.equal(result.attempts, 1)
  assert.doesNotMatch(result.stdout, /FLAKY/)
})
