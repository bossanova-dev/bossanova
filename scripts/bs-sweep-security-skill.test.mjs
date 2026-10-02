// Behaviour tests for the bs-sweep-security gate (scripts/sweep-security-gate.mjs).

import { test } from 'node:test'
import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { fileURLToPath } from 'node:url'

// ---------------------------------------------------------------------------
// Triage behavior-parity — the gate selects a known batch on a NON-EMPTY set.
// The subagent-in-the-loop path runs exactly this gate CLI, so pinning the CLI
// selection proves the triage behavior did not change.
// ---------------------------------------------------------------------------

const gatePath = fileURLToPath(new URL('./sweep-security-gate.mjs', import.meta.url))
const alertsFixture = fileURLToPath(
  new URL('./fixtures/bs-sweep-security/alerts.sample.json', import.meta.url),
)
const prsFixture = fileURLToPath(
  new URL('./fixtures/bs-sweep-security/prs.sample.json', import.meta.url),
)

test('triage parity: gate select-batch picks the expected non-empty batch on fixtures', () => {
  const res = spawnSync(
    process.execPath,
    [gatePath, 'select-batch', alertsFixture, prsFixture, '10'],
    {
      encoding: 'utf8',
    },
  )
  assert.equal(res.status, 0, res.stderr)
  const sel = JSON.parse(res.stdout)

  assert.equal(sel.manifest, 'pnpm-lock.yaml')
  assert.equal(sel.ecosystem, 'npm')

  // Batch is non-empty and ranked by score desc (critical axios before high lodash).
  assert.deepEqual(
    sel.batch.map((a) => a.number),
    [102, 101],
  )
  assert.deepEqual(
    sel.batch.map((a) => a.security_advisory.ghsa_id),
    ['GHSA-axios-0002', 'GHSA-lodash-0001'],
  )

  // Deferred covers the major-version bump; dropped covers the no-patch alert.
  assert.deepEqual(sel.deferred, [
    { number: 103, ghsa: 'GHSA-next-0003', reason: 'major version bump' },
  ])
  assert.deepEqual(sel.dropped, [
    { number: 104, ghsa: 'GHSA-request-0004', reason: 'no patched version' },
  ])
})
