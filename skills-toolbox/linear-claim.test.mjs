#!/usr/bin/env node

import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import test from 'node:test'
import { fileURLToPath } from 'node:url'

import {
  CLAIM_MARKER,
  claimWinner,
  formatClaimComment,
  generateRunToken,
  isClaimWon,
  parseClaimComments,
} from './linear-claim.mjs'

const SCRIPT_PATH = fileURLToPath(new URL('./linear-claim.mjs', import.meta.url))

test('generateRunToken returns 32 lowercase hex chars', () => {
  assert.match(generateRunToken(), /^[0-9a-f]{32}$/)
})

test('generateRunToken is non-deterministic', () => {
  assert.notEqual(generateRunToken(), generateRunToken())
})

test('formatClaimComment embeds the marker and token', () => {
  const body = formatClaimComment('a'.repeat(32))
  assert.ok(body.includes(CLAIM_MARKER))
  assert.ok(body.includes('a'.repeat(32)))
})

test('parseClaimComments extracts token + createdAt from claim bodies only', () => {
  const comments = [
    { body: `${CLAIM_MARKER}:${'a'.repeat(32)}`, createdAt: '2026-06-12T00:00:01Z' },
    { body: 'unrelated chatter', createdAt: '2026-06-12T00:00:02Z' },
    { body: `claimed ${CLAIM_MARKER}:${'b'.repeat(32)} now`, createdAt: '2026-06-12T00:00:03Z' },
  ]
  assert.deepEqual(parseClaimComments(comments), [
    { token: 'a'.repeat(32), createdAt: '2026-06-12T00:00:01Z' },
    { token: 'b'.repeat(32), createdAt: '2026-06-12T00:00:03Z' },
  ])
})

test('parseClaimComments ignores an over-long hex run (anchored boundary)', () => {
  // A crafted body with 40 hex chars must not have its first 32 captured.
  const comments = [
    { body: `${CLAIM_MARKER}:${'a'.repeat(40)}`, createdAt: '2026-06-12T00:00:01Z' },
    { body: `${CLAIM_MARKER}:${'b'.repeat(32)}!`, createdAt: '2026-06-12T00:00:02Z' },
  ]
  assert.deepEqual(parseClaimComments(comments), [
    { token: 'b'.repeat(32), createdAt: '2026-06-12T00:00:02Z' },
  ])
})

test('claimWinner picks the earliest createdAt', () => {
  const claims = [
    { token: 'b'.repeat(32), createdAt: '2026-06-12T00:00:03Z' },
    { token: 'a'.repeat(32), createdAt: '2026-06-12T00:00:01Z' },
  ]
  assert.equal(claimWinner(claims), 'a'.repeat(32))
})

test('claimWinner compares timestamps by parsed time including precision', () => {
  const claims = [
    { token: 'f'.repeat(32), createdAt: '2026-06-12T00:00:00.100Z' },
    { token: '0'.repeat(32), createdAt: '2026-06-12T00:00:00Z' },
  ]
  assert.equal(claimWinner(claims), '0'.repeat(32))
})

test('claimWinner compares equivalent offset timestamps by token tie-break', () => {
  const claims = [
    { token: 'f'.repeat(32), createdAt: '2026-06-12T00:00:00Z' },
    { token: '0'.repeat(32), createdAt: '2026-06-12T01:00:00+01:00' },
  ]
  assert.equal(claimWinner(claims), '0'.repeat(32))
})

test('claimWinner rejects invalid timestamps', () => {
  assert.throws(
    () => claimWinner([{ token: 'a'.repeat(32), createdAt: 'not-a-date' }]),
    /invalid claim createdAt/,
  )
})

test('claimWinner tie-breaks equal timestamps by lexicographically smallest token', () => {
  const t = '2026-06-12T00:00:01Z'
  const claims = [
    { token: 'f'.repeat(32), createdAt: t },
    { token: '0'.repeat(32), createdAt: t },
  ]
  assert.equal(claimWinner(claims), '0'.repeat(32))
})

test('isClaimWon is true only for the winning token', () => {
  const claims = [
    { token: 'a'.repeat(32), createdAt: '2026-06-12T00:00:01Z' },
    { token: 'b'.repeat(32), createdAt: '2026-06-12T00:00:02Z' },
  ]
  assert.equal(isClaimWon(claims, 'a'.repeat(32)), true)
  assert.equal(isClaimWon(claims, 'b'.repeat(32)), false)
})

test('claimWinner returns null on an empty set', () => {
  assert.equal(claimWinner([]), null)
})

test('CLI verdict exits 3 when this run lost', () => {
  const result = spawnSync(
    process.execPath,
    [
      SCRIPT_PATH,
      'verdict',
      '--me',
      'b'.repeat(32),
      '--comments',
      JSON.stringify([
        { body: `${CLAIM_MARKER}:${'a'.repeat(32)}`, createdAt: '2026-06-12T00:00:01Z' },
        { body: `${CLAIM_MARKER}:${'b'.repeat(32)}`, createdAt: '2026-06-12T00:00:02Z' },
      ]),
    ],
    { encoding: 'utf8' },
  )
  assert.equal(result.status, 3)
  assert.match(result.stdout, /^LOST \(winner: a{32}\)/)
  assert.equal(result.stderr, '')
})

test('CLI verdict exits 1 when required flags are missing', () => {
  const result = spawnSync(process.execPath, [SCRIPT_PATH, 'verdict'], { encoding: 'utf8' })
  assert.equal(result.status, 1)
  assert.equal(result.stdout, '')
  assert.match(result.stderr, /--me <token> is required/)
})
