#!/usr/bin/env node

// Schema + drift guard for scripts/bazel/ledger.json — the machine-readable
// mirror of the BOS-344 bazel-`manual` exclusions ledger (BOS-339). The facade's
// `test-native-ledger` step and the fake-bazel contract suite both consume it, so
// a shape or membership drift here would silently break today-parity.

import assert from 'node:assert/strict'
import fs from 'node:fs'
import path from 'node:path'
import test from 'node:test'

const repoRoot = path.resolve(path.dirname(new URL(import.meta.url).pathname), '..')
const ledgerPath = path.join(repoRoot, 'scripts/bazel/ledger.json')

// The exact set of bazel-`manual` test targets that today's `make test` still had
// to cover natively. Kept in lockstep with `bazel query 'attr(tags,"manual",tests(//...))'`
// and the human-mirror table in docs/solutions/build-systems/bazel-sandbox-patterns.md.
const EXPECTED_LABELS = [
  '//lib/bossalib/apiversion:apiversion_test',
  '//services/bossd/internal/testharness:testharness_test',
  '//services/bosso/internal/loadtest:loadtest_test',
  '//services/boss/cmd:cmd_test',
  '//services/boss/cmd/proof-tui-agent:proof-tui-agent_test',
  '//services/boss/internal/clitest:clitest_test',
  '//services/boss/internal/tuitest:tuitest_test',
  '//services/boss/internal/skillinstall:skillinstall_test',
]

const VALID_DISPOSITIONS = new Set(['default-run', 'explicit-only'])

function loadLedger() {
  const raw = fs.readFileSync(ledgerPath, 'utf8')
  return JSON.parse(raw)
}

test('ledger.json is a JSON array', () => {
  const ledger = loadLedger()
  assert.ok(Array.isArray(ledger), 'ledger must be an array')
  assert.ok(ledger.length > 0, 'ledger must not be empty')
})

test('every ledger row has the required well-formed fields', () => {
  const ledger = loadLedger()
  for (const row of ledger) {
    assert.equal(typeof row.label, 'string', `label must be a string: ${JSON.stringify(row)}`)
    assert.ok(row.label.startsWith('//'), `label must be a bazel label: ${row.label}`)
    assert.equal(typeof row.module, 'string', `module must be a string: ${row.label}`)
    assert.ok(row.module.length > 0, `module must be non-empty: ${row.label}`)
    assert.equal(typeof row.mechanism, 'string', `mechanism must be a string: ${row.label}`)
    assert.ok(row.mechanism.length > 0, `mechanism must be non-empty: ${row.label}`)
    assert.equal(typeof row.command, 'string', `command must be a string: ${row.label}`)
    assert.ok(row.command.length > 0, `command must be non-empty: ${row.label}`)
    assert.match(row.command, /go test/, `command must run go test: ${row.label}`)
    assert.ok(
      VALID_DISPOSITIONS.has(row.disposition),
      `disposition must be one of ${[...VALID_DISPOSITIONS].join('|')}: ${row.label} => ${row.disposition}`,
    )
    assert.equal(typeof row.reason, 'string', `reason must be a string: ${row.label}`)
    assert.ok(row.reason.length > 0, `reason must be non-empty: ${row.label}`)
  }
})

test('every ledger module is a real directory', () => {
  const ledger = loadLedger()
  for (const row of ledger) {
    const dir = path.join(repoRoot, row.module)
    assert.ok(
      fs.existsSync(dir) && fs.statSync(dir).isDirectory(),
      `module dir must exist: ${row.module} (${row.label})`,
    )
  }
})

test('ledger label set exactly matches the bazel-manual targets (drift guard)', () => {
  const ledger = loadLedger()
  const got = ledger.map((r) => r.label).sort()
  const want = [...EXPECTED_LABELS].sort()
  assert.deepEqual(got, want, 'ledger labels must exactly match the known manual targets')
})

test('loadtest is the sole explicit-only row (documented parity exception)', () => {
  const ledger = loadLedger()
  const explicitOnly = ledger.filter((r) => r.disposition === 'explicit-only').map((r) => r.label)
  assert.deepEqual(explicitOnly, ['//services/bosso/internal/loadtest:loadtest_test'])
})
