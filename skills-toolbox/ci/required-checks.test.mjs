#!/usr/bin/env node
import assert from 'node:assert/strict'
import test from 'node:test'

import {
  entryRan,
  notRequestedChecks,
  parseRequiredChecks,
  requiredChecksFor,
} from './required-checks.mjs'

test('parseRequiredChecks — an absent block requires nothing', () => {
  assert.deepEqual(parseRequiredChecks(undefined), { checks: [], exemptBases: [] })
  assert.deepEqual(parseRequiredChecks(null), { checks: [], exemptBases: [] })
})

test('parseRequiredChecks — rejects malformed blocks with a ci.required message', () => {
  for (const block of [
    'go-test',
    [],
    { checks: 'go-test' },
    { checks: [''] },
    { checks: [1] },
    { checks: ['go-test'], exemptBases: 'staging' },
    { checks: ['go-test'], extra: true },
  ]) {
    assert.throws(() => parseRequiredChecks(block), /ci\.required/, JSON.stringify(block))
  }
})

test('requiredChecksFor — exempt bases and empty lists require nothing', () => {
  const block = { checks: ['go-test', 'scripts'], exemptBases: ['staging', 'production'] }
  assert.deepEqual(requiredChecksFor(block, 'main'), ['go-test', 'scripts'])
  assert.deepEqual(requiredChecksFor(block, 'refs/heads/main'), ['go-test', 'scripts'])
  assert.deepEqual(requiredChecksFor(block, 'staging'), [])
  assert.deepEqual(requiredChecksFor({ checks: [] }, 'main'), [])
  assert.deepEqual(requiredChecksFor(undefined, 'main'), [])
})

test('entryRan — only a skipped entry that says it did not run fails to count', () => {
  assert.equal(entryRan({ kind: 'passed' }), true)
  assert.equal(entryRan({ kind: 'pending' }), true)
  assert.equal(entryRan({ kind: 'unclassified' }), true)
  assert.equal(entryRan({ kind: 'failed' }), true)
  assert.equal(entryRan({ kind: 'skipped', conclusion: 'SKIPPED' }), false)
  assert.equal(entryRan({ kind: 'skipped', conclusion: 'neutral' }), false)
  assert.equal(entryRan({ kind: 'skipped', conclusion: '', bucket: 'skipping' }), false)
  assert.equal(entryRan({ kind: 'skipped', conclusion: 'STALE' }), true)
})

test('notRequestedChecks — lists required names with no run, in requirement order', () => {
  const entries = [
    { name: 'lint', kind: 'passed' },
    { name: 'go-test', kind: 'skipped', conclusion: 'SKIPPED' },
    { name: 'scripts', kind: 'passed' },
  ]
  assert.deepEqual(notRequestedChecks(entries, ['scripts', 'go-test', 'web']), ['go-test', 'web'])
  assert.deepEqual(notRequestedChecks(entries, []), [])
  assert.deepEqual(notRequestedChecks(null, ['go-test']), ['go-test'])
})
