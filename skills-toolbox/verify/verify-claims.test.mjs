import test from 'node:test'
import assert from 'node:assert/strict'
import {
  CLAIM_PROOFS_SCHEMA,
  acceptClaimProofs,
  capForClaims,
  validateClaimProofs,
} from './verify-claims.mjs'
import { MAX_LOWERED_BY, MAX_TEXT_CHARS } from './verify-proof.mjs'

const BULLETS = ['Shows the balance', 'Sends the invoice', 'Keeps the old export']
const TEST_FILE = 'src/balance.test.mjs'
const HEAD_FILES = {
  [TEST_FILE]:
    "test('shows the customer balance', () => {})\ntest('shows the customer', () => {})\n",
  'src/balance.mjs': '// shows the customer balance\nexport const balance = 1\n',
}
const readHeadFile = (path) => HEAD_FILES[path] ?? null
const CHAPTERS = [
  {
    criterion: 'change.AC1',
    title: 'Sends the invoice',
    verdict: 'proven',
    sweep: false,
    rendered: true,
    published: true,
    atMs: 65_000,
  },
  {
    criterion: 'change.AC2',
    title: 'Unproven chapter',
    verdict: 'unproven',
    sweep: false,
    rendered: true,
    published: true,
    atMs: 0,
  },
  {
    criterion: 'ui.AC1',
    title: 'Sweep chapter',
    verdict: 'proven',
    sweep: true,
    rendered: false,
    published: true,
    atMs: null,
  },
  {
    criterion: 'change.AC3',
    title: 'Unpublished chapter',
    verdict: 'proven',
    sweep: false,
    rendered: true,
    published: false,
    atMs: 1000,
  },
]

const file = (claims) => ({ schema: CLAIM_PROOFS_SCHEMA, claims })
const accept = (claims, extra = {}) =>
  acceptClaimProofs({
    bullets: BULLETS,
    proofs: file(claims),
    changedPaths: [TEST_FILE, 'src/balance.mjs'],
    readHeadFile,
    chapters: CHAPTERS,
    ...extra,
  })

test('accepts a test, a clip and nothing as test, clip and unproven claims', () => {
  const claims = accept([
    { claim: 1, proof: 'test', test: 'shows the customer balance', path: TEST_FILE },
    { claim: 2, proof: 'clip', criterion: 'change.AC1' },
  ])
  assert.deepEqual(claims, [
    { proof: 'test', behaviour: true, test: 'shows the customer balance', path: TEST_FILE },
    { proof: 'clip', behaviour: true, chapter: 'Sends the invoice', atMs: 65_000 },
    { proof: 'unproven', behaviour: true, note: 'no proof was named' },
  ])
})

test('downgrades a test or clip entry it cannot check to unproven with a note', () => {
  const cases = {
    'path not changed': {
      proof: 'test',
      test: 'shows the customer balance',
      path: 'src/other.test.mjs',
    },
    'name absent': { proof: 'test', test: 'refunds the customer', path: TEST_FILE },
    'name only a prefix of a longer token': {
      proof: 'test',
      test: 'shows the cust',
      path: TEST_FILE,
    },
    'not a test file': {
      proof: 'test',
      test: 'shows the customer balance',
      path: 'src/balance.mjs',
    },
    'unreadable head file': { proof: 'test', test: 'x', path: 'src/gone.test.mjs' },
    'criterion unproven': { proof: 'clip', criterion: 'change.AC2' },
    'criterion sweep-only': { proof: 'clip', criterion: 'ui.AC1' },
    'video unpublished': { proof: 'clip', criterion: 'change.AC3' },
    'criterion unknown': { proof: 'clip', criterion: 'change.AC9' },
  }
  for (const [name, entry] of Object.entries(cases)) {
    const changedPaths = [TEST_FILE, 'src/balance.mjs', 'src/gone.test.mjs']
    const [claim] = accept([{ claim: 1, ...entry }], { changedPaths })
    assert.equal(claim.proof, 'unproven', name)
    assert.ok(typeof claim.note === 'string' && claim.note.length > 0, name)
    assert.ok(!('test' in claim) && !('chapter' in claim), name)
  }
})

test('a whole-token test name in a changed test file still stands when a longer one also exists', () => {
  const [claim] = accept([{ claim: 1, proof: 'test', test: 'shows the customer', path: TEST_FILE }])
  assert.equal(claim.proof, 'test')
})

test('rejects a malformed prover file and records every claim unproven', () => {
  const long = 'x'.repeat(MAX_TEXT_CHARS + 1)
  const invalid = {
    duplicate: [
      { claim: 1, proof: 'unproven' },
      { claim: 1, proof: 'unproven' },
    ],
    'out of range': [{ claim: 4, proof: 'unproven' }],
    zero: [{ claim: 0, proof: 'unproven' }],
    'text key': [{ claim: 1, proof: 'unproven', text: 'A rewritten claim' }],
    'bullet key': [{ claim: 1, proof: 'unproven', bullet: 'A rewritten claim' }],
    'over-long note': [{ claim: 1, proof: 'unproven', note: long }],
    'over-long test': [{ claim: 1, proof: 'test', test: long, path: TEST_FILE }],
    'test without path': [{ claim: 1, proof: 'test', test: 'shows the customer balance' }],
    'clip without criterion': [{ claim: 1, proof: 'clip' }],
    'clip with test': [{ claim: 1, proof: 'clip', criterion: 'change.AC1', test: 'x' }],
    'unknown proof': [{ claim: 1, proof: 'vibes' }],
    'non-boolean behaviour': [{ claim: 1, proof: 'unproven', behaviour: 'no' }],
  }
  for (const [name, claims] of Object.entries(invalid)) {
    const result = validateClaimProofs(file(claims), BULLETS.length)
    assert.equal(result.ok, false, name)
    assert.ok(result.errors.length > 0, name)
    assert.deepEqual(
      accept(claims).map((claim) => [claim.proof, claim.note, claim.behaviour]),
      BULLETS.map(() => ['unproven', 'claim proofs unreadable', true]),
      name,
    )
  }
  for (const input of [
    null,
    'text',
    [],
    { claims: [] },
    { schema: 'other/1', claims: [] },
    { ...file([]), extra: 1 },
  ])
    assert.equal(validateClaimProofs(input, 3).ok, false)
  assert.equal(validateClaimProofs(file([{ claim: 1, proof: 'unproven' }]), 3).ok, true)
})

test('with no prover file every claim is unproven, and behaviour false is kept', () => {
  assert.deepEqual(
    acceptClaimProofs({
      bullets: BULLETS,
      proofs: null,
      changedPaths: [],
      readHeadFile,
      chapters: [],
    }).map((claim) => claim.proof),
    ['unproven', 'unproven', 'unproven'],
  )
  const [claim] = accept([{ claim: 1, proof: 'unproven', behaviour: false, note: 'docs only' }])
  assert.deepEqual(claim, { proof: 'unproven', behaviour: false, note: 'docs only' })
})

const confidence = (level, loweredBy = []) => ({ level, asks: [], loweredBy })
const unprovenBehaviour = { proof: 'unproven', behaviour: true, note: 'x' }

test('capForClaims lowers high to moderate and adds the reason to lower levels', () => {
  const capped = capForClaims(confidence('high'), [unprovenBehaviour, unprovenBehaviour])
  assert.equal(capped.level, 'moderate')
  assert.deepEqual(capped.loweredBy, [
    { code: 'unproven-claim', text: '2 behaviour claim(s) unproven.' },
  ])
  for (const level of ['moderate', 'low', 'blocked', 'cannot-assess']) {
    const before = confidence(level, [{ code: 'other', text: 'Other.' }])
    const after = capForClaims(before, [unprovenBehaviour])
    assert.equal(after.level, level)
    assert.deepEqual(
      after.loweredBy.map((r) => r.code),
      ['other', 'unproven-claim'],
    )
    assert.deepEqual(capForClaims(after, [unprovenBehaviour]).loweredBy, after.loweredBy)
  }
})

test('capForClaims leaves proven and non-behaviour claims alone, and a missing behaviour counts', () => {
  const high = confidence('high')
  for (const claims of [
    undefined,
    [],
    [{ proof: 'test', behaviour: true, test: 't', path: 'p' }],
    [{ proof: 'clip', behaviour: true, chapter: 'c', atMs: 0 }],
    [{ proof: 'unproven', behaviour: false }],
  ])
    assert.deepEqual(capForClaims(high, claims), high)
  assert.equal(capForClaims(high, [{ proof: 'unproven' }]).level, 'moderate')
})

test('capForClaims stays within MAX_LOWERED_BY', () => {
  const full = Array.from({ length: MAX_LOWERED_BY }, (_, i) => ({ code: `r${i}`, text: `R${i}.` }))
  const capped = capForClaims(confidence('low', full), [unprovenBehaviour])
  assert.equal(capped.loweredBy.length, MAX_LOWERED_BY)
  assert.ok(capped.loweredBy.some((r) => r.code === 'unproven-claim'))
})
