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

test('formatClaimComment without a session id preserves the legacy bytes', () => {
  assert.equal(
    formatClaimComment('a'.repeat(32)),
    `🔒 ${CLAIM_MARKER}:${'a'.repeat(32)} (bs-implement run claiming this ticket)`,
  )
})

test('formatClaimComment appends an owner suffix when a session id is supplied', () => {
  assert.equal(
    formatClaimComment('a'.repeat(32), 'session-123'),
    `🔒 ${CLAIM_MARKER}:${'a'.repeat(32)} (bs-implement run claiming this ticket) owner:session-123`,
  )
})

test('formatClaimComment rejects session ids that the parser cannot recover', () => {
  assert.throws(() => formatClaimComment('a'.repeat(32), 'session/123'), /invalid claim session id/)
})

test('parseClaimComments extracts token + createdAt from claim bodies only', () => {
  const comments = [
    { body: `${CLAIM_MARKER}:${'a'.repeat(32)}`, createdAt: '2026-06-12T00:00:01Z' },
    { body: 'unrelated chatter', createdAt: '2026-06-12T00:00:02Z' },
    { body: `claimed ${CLAIM_MARKER}:${'b'.repeat(32)} now`, createdAt: '2026-06-12T00:00:03Z' },
  ]
  assert.deepEqual(parseClaimComments(comments), [
    { token: 'a'.repeat(32), createdAt: '2026-06-12T00:00:01Z', sessionId: null },
    { token: 'b'.repeat(32), createdAt: '2026-06-12T00:00:03Z', sessionId: null },
  ])
})

test('parseClaimComments recovers sessionId from new-form comments', () => {
  const comments = [
    { body: formatClaimComment('a'.repeat(32), 'session-123'), createdAt: '2026-06-12T00:00:01Z' },
    { body: formatClaimComment('b'.repeat(32)), createdAt: '2026-06-12T00:00:02Z' },
  ]
  assert.deepEqual(parseClaimComments(comments), [
    { token: 'a'.repeat(32), createdAt: '2026-06-12T00:00:01Z', sessionId: 'session-123' },
    { token: 'b'.repeat(32), createdAt: '2026-06-12T00:00:02Z', sessionId: null },
  ])
})

test('parseClaimComments does not recover partial malformed owner suffixes', () => {
  const comments = [
    {
      body: `🔒 ${CLAIM_MARKER}:${'a'.repeat(32)} (bs-implement run claiming this ticket) owner:session/123`,
      createdAt: '2026-06-12T00:00:01Z',
    },
  ]
  assert.deepEqual(parseClaimComments(comments), [
    { token: 'a'.repeat(32), createdAt: '2026-06-12T00:00:01Z', sessionId: null },
  ])
})

test('parseClaimComments does not recover owner-looking text outside the emitted suffix', () => {
  const comments = [
    {
      body: `claimed ${CLAIM_MARKER}:${'a'.repeat(32)} from a note owner:dead-session`,
      createdAt: '2026-06-12T00:00:01Z',
    },
  ]
  assert.deepEqual(parseClaimComments(comments), [
    { token: 'a'.repeat(32), createdAt: '2026-06-12T00:00:01Z', sessionId: null },
  ])
})

test('parseClaimComments does not recover copied owner suffixes from arbitrary text', () => {
  const comments = [
    {
      body: `copied ${CLAIM_MARKER}:${'a'.repeat(32)} (bs-implement run claiming this ticket) owner:dead-session`,
      createdAt: '2026-06-12T00:00:01Z',
    },
  ]
  assert.deepEqual(parseClaimComments(comments), [
    { token: 'a'.repeat(32), createdAt: '2026-06-12T00:00:01Z', sessionId: null },
  ])
})

test('parseClaimComments ignores an over-long hex run (anchored boundary)', () => {
  // A crafted body with 40 hex chars must not have its first 32 captured.
  const comments = [
    { body: `${CLAIM_MARKER}:${'a'.repeat(40)}`, createdAt: '2026-06-12T00:00:01Z' },
    { body: `${CLAIM_MARKER}:${'b'.repeat(32)}!`, createdAt: '2026-06-12T00:00:02Z' },
  ]
  assert.deepEqual(parseClaimComments(comments), [
    { token: 'b'.repeat(32), createdAt: '2026-06-12T00:00:02Z', sessionId: null },
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

test('isClaimWon returns null when no claim survives', () => {
  assert.equal(isClaimWon([], 'a'.repeat(32)), null)
})

test('claimWinner returns null on an empty set', () => {
  assert.equal(claimWinner([]), null)
})

test('claimWinner forfeits a provably inactive owner and picks the runner-up', () => {
  const claims = [
    { token: 'a'.repeat(32), createdAt: '2026-06-12T00:00:01Z', sessionId: 'dead-session' },
    { token: 'b'.repeat(32), createdAt: '2026-06-12T00:00:02Z', sessionId: 'live-session' },
  ]
  assert.equal(
    claimWinner(claims, {
      now: '2026-06-12T00:10:00Z',
      inactiveAfterMs: 60_000,
      sessions: {
        'dead-session': { lastActivityAt: '2026-06-12T00:00:00Z' },
        'live-session': { lastActivityAt: '2026-06-12T00:09:30Z' },
      },
    }),
    'b'.repeat(32),
  )
})

test('claimWinner treats null liveness entries as unknown, not dead', () => {
  const claims = [
    { token: 'a'.repeat(32), createdAt: '2026-06-12T00:00:01Z', sessionId: 'unreadable' },
    { token: 'b'.repeat(32), createdAt: '2026-06-12T00:00:02Z', sessionId: null },
  ]
  assert.equal(
    claimWinner(claims, {
      inactiveAfterMs: 60_000,
      sessions: {
        unreadable: null,
      },
    }),
    'a'.repeat(32),
  )
})

test('claimWinner rejects known-owner liveness entries without last activity', () => {
  const claims = [
    { token: 'a'.repeat(32), createdAt: '2026-06-12T00:00:01Z', sessionId: 'malformed' },
  ]
  assert.throws(
    () =>
      claimWinner(claims, {
        inactiveAfterMs: 60_000,
        sessions: {
          malformed: {},
        },
      }),
    /session last activity is required for known claim owner: malformed/,
  )
})

test('claimWinner never forfeits an unknown owner on liveness basis', () => {
  const claims = [
    { token: 'a'.repeat(32), createdAt: '2026-06-12T00:00:01Z', sessionId: 'unknown-session' },
    { token: 'b'.repeat(32), createdAt: '2026-06-12T00:00:02Z', sessionId: 'live-session' },
  ]
  assert.equal(
    claimWinner(claims, {
      now: '2026-06-12T00:10:00Z',
      inactiveAfterMs: 60_000,
      sessions: {
        'live-session': { lastActivityAt: '2026-06-12T00:09:30Z' },
      },
    }),
    'a'.repeat(32),
  )
})

test('claimWinner returns null when every claim is forfeited', () => {
  const claims = [
    { token: 'a'.repeat(32), createdAt: '2026-06-12T00:00:01Z', sessionId: 'dead-a' },
    { token: 'b'.repeat(32), createdAt: '2026-06-12T00:00:02Z', sessionId: 'dead-b' },
  ]
  assert.equal(
    claimWinner(claims, {
      now: '2026-06-12T00:10:00Z',
      inactiveAfterMs: 60_000,
      sessions: {
        'dead-a': { lastActivityAt: '2026-06-12T00:00:00Z' },
        'dead-b': { lastActivityAt: '2026-06-12T00:00:10Z' },
      },
    }),
    null,
  )
})

test('claimWinner applies comment-age forfeiture only when explicitly opted in', () => {
  const claims = [
    { token: 'a'.repeat(32), createdAt: '2026-06-12T00:00:01Z', sessionId: null },
    { token: 'b'.repeat(32), createdAt: '2026-06-12T00:09:30Z', sessionId: null },
  ]
  const options = { now: '2026-06-12T00:10:00Z', commentAgeAfterMs: 60_000 }
  assert.equal(claimWinner(claims, options), 'a'.repeat(32))
  assert.equal(claimWinner(claims, { ...options, forfeitByCommentAge: true }), 'b'.repeat(32))
})

test('claimWinner does not apply comment-age forfeiture after a known owner stays live', () => {
  const claims = [
    { token: 'a'.repeat(32), createdAt: '2026-06-12T00:00:01Z', sessionId: 'live-session' },
    { token: 'b'.repeat(32), createdAt: '2026-06-12T00:09:30Z', sessionId: null },
  ]
  assert.equal(
    claimWinner(claims, {
      now: '2026-06-12T00:10:00Z',
      inactiveAfterMs: 60_000,
      forfeitByCommentAge: true,
      commentAgeAfterMs: 60_000,
      sessions: {
        'live-session': { lastActivityAt: '2026-06-12T00:09:45Z' },
      },
    }),
    'a'.repeat(32),
  )
})

test('claimWinner rejects comment-age opt-in without a window', () => {
  const claims = [{ token: 'a'.repeat(32), createdAt: '2026-06-12T00:00:01Z', sessionId: null }]
  assert.throws(
    () =>
      claimWinner(claims, {
        now: '2026-06-12T00:10:00Z',
        forfeitByCommentAge: true,
      }),
    /commentAgeAfterMs/,
  )
})

test('claimWinner ignores empty liveness options and preserves first-writer-wins', () => {
  const claims = [
    { token: 'a'.repeat(32), createdAt: '2026-06-12T00:00:01Z', sessionId: 'session-a' },
    { token: 'b'.repeat(32), createdAt: '2026-06-12T00:00:02Z', sessionId: 'session-b' },
  ]
  assert.equal(claimWinner(claims, {}), 'a'.repeat(32))
  assert.equal(claimWinner(claims, { sessions: {} }), 'a'.repeat(32))
})

test('claimWinner rejects primitive liveness options', () => {
  const claims = [
    { token: 'a'.repeat(32), createdAt: '2026-06-12T00:00:01Z', sessionId: 'session-a' },
  ]
  assert.throws(() => claimWinner(claims, true), /claim liveness options must be an object/)
})

test('claimWinner does not require now when no liveness evidence is actionable', () => {
  const claims = [
    { token: 'a'.repeat(32), createdAt: '2026-06-12T00:00:01Z', sessionId: 'unknown-session' },
    { token: 'b'.repeat(32), createdAt: '2026-06-12T00:00:02Z', sessionId: null },
  ]
  assert.equal(
    claimWinner(claims, {
      inactiveAfterMs: 60_000,
      sessions: {},
    }),
    'a'.repeat(32),
  )
})

test('legacy CLI verdict exits 3 when this run lost', () => {
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

test('legacy CLI verdict exits 4 when no claim survives', () => {
  const result = spawnSync(
    process.execPath,
    [SCRIPT_PATH, 'verdict', '--me', 'a'.repeat(32), '--comments', '[]'],
    { encoding: 'utf8' },
  )
  assert.equal(result.status, 4)
  assert.match(result.stdout, /^NO_WINNER/)
  assert.equal(result.stderr, '')
})

test('legacy CLI verdict exits 1 when required flags are missing', () => {
  const result = spawnSync(process.execPath, [SCRIPT_PATH, 'verdict'], { encoding: 'utf8' })
  assert.equal(result.status, 1)
  assert.equal(result.stdout, '')
  assert.match(result.stderr, /--me <token> is required/)
})
