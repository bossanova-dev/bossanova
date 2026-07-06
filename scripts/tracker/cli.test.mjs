// scripts/tracker/cli.test.mjs
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { runCli, generateClaimToken } from './cli.mjs'

const won = '11111111111111111111111111111111'
const lost = '22222222222222222222222222222222'
const early = '2026-01-01T00:00:00.000Z'
const late = '2026-01-02T00:00:00.000Z'
const marker = (t) => `🔒 bs-implement-claim:${t} (bs-implement run claiming this ticket)`

test('generateClaimToken returns a 32-char lowercase hex token', () => {
  const t = generateClaimToken()
  assert.match(t, /^[0-9a-f]{32}$/)
})

test('claim-token prints a fresh token and exits 0', () => {
  let out = ''
  const code = runCli(['claim-token'], { write: (s) => (out += s) })
  assert.equal(code, 0)
  assert.match(out.trim(), /^[0-9a-f]{32}$/)
})

test('claim-verdict exits 0 when my token is the first writer (WON)', () => {
  const comments = JSON.stringify([
    { body: marker(won), createdAt: early },
    { body: marker(lost), createdAt: late },
  ])
  const code = runCli(['claim-verdict', '--me', won, '--comments', comments], {})
  assert.equal(code, 0)
})

test('claim-verdict exits 3 when another token wins first-writer (LOST)', () => {
  const comments = JSON.stringify([
    { body: marker(won), createdAt: early },
    { body: marker(lost), createdAt: late },
  ])
  const code = runCli(['claim-verdict', '--me', lost, '--comments', comments], {})
  assert.equal(code, 3)
})

test('claim-verdict without --comments exits 2 (parity with the required arg)', () => {
  let err = ''
  const code = runCli(['claim-verdict', '--me', won], { errWrite: (s) => (err += s) })
  assert.equal(code, 2)
  assert.match(err, /--comments <json-array> is required/)
})

test('an unknown capability exits 2', () => {
  let err = ''
  const code = runCli(['bogus'], { errWrite: (s) => (err += s) })
  assert.equal(code, 2)
  assert.match(err, /unknown tracker capability: bogus/)
})
