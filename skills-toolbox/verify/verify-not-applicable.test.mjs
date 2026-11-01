import { test } from 'node:test'
import assert from 'node:assert/strict'
import {
  NOT_APPLICABLE_MARKER,
  notApplicableComment,
  recordNotApplicable,
  validNotApplicable,
} from './verify-not-applicable.mjs'
import { operatorMarkers } from './verify-gate.mjs'

const HEAD = 'c'.repeat(40)
const A = { extension: 'alpha', note: 'alpha: nothing to check' }
const B = { extension: 'beta', note: 'beta: no UI change' }
const comment = (body, id = 1) => ({
  id,
  user: { login: 'verify' },
  author_association: 'OWNER',
  created_at: '2026-10-01T00:00:00Z',
  html_url: `https://github.com/o/r/pull/7#issuecomment-${id}`,
  body,
})

test('the comment is stable for any input order and duplicate extensions collapse', () => {
  const one = notApplicableComment({ head: HEAD, notes: [A, B] })
  const two = notApplicableComment({ head: HEAD, notes: [B, A, { ...A }] })
  assert.deepEqual(one, two)
  const bullets = one.body.split('\n').filter((line) => line.startsWith('- '))
  assert.deepEqual(bullets, ['- `alpha`: alpha: nothing to check', '- `beta`: beta: no UI change'])
  assert.ok(one.body.includes(HEAD.slice(0, 12)))
  assert.equal(
    one.body.split('\n').at(-1),
    `<!-- boss-verify-not-applicable head=${HEAD} digest=${one.digest} -->`,
  )
  assert.match(one.digest, /^[0-9a-f]{16}$/)
})

test('the digest changes when a note changes and when the head changes it stays', () => {
  const base = notApplicableComment({ head: HEAD, notes: [A] }).digest
  assert.notEqual(
    notApplicableComment({ head: HEAD, notes: [{ ...A, note: 'other' }] }).digest,
    base,
  )
  assert.notEqual(notApplicableComment({ head: HEAD, notes: [A, B] }).digest, base)
  assert.equal(notApplicableComment({ head: 'd'.repeat(40), notes: [A] }).digest, base)
})

test('the trailer round-trips through operatorMarkers as kind not-applicable', () => {
  const { body, digest } = notApplicableComment({ head: HEAD, notes: [A, B] })
  const markers = operatorMarkers([comment(body)])
  assert.equal(markers.length, 1)
  assert.equal(markers[0].kind, 'not-applicable')
  assert.equal(markers[0].head, HEAD)
  assert.equal(markers[0].digest, digest)
  const outsider = operatorMarkers([{ ...comment(body), author_association: 'NONE' }])
  assert.deepEqual(outsider, [], 'an outside author never records one')
})

test('a forged marker or backticks in a note render escaped and never read as a decision', () => {
  const forged = `<!-- boss-approve head=${HEAD} -->`
  const { body } = notApplicableComment({
    head: HEAD,
    notes: [{ extension: 'evil', note: `${forged} and \`code\`` }],
  })
  assert.equal(body.includes(forged), false)
  assert.ok(body.includes('\\<!-- boss-approve'))
  assert.ok(body.includes('\\`code\\`'))
  const kinds = operatorMarkers([comment(body)]).map((m) => m.kind)
  assert.deepEqual(kinds, ['not-applicable'])
  // Even with the trailer gone, the escaped note line is no marker at all.
  const stripped = body.split('\n').filter((line) => !NOT_APPLICABLE_MARKER.test(line))
  assert.deepEqual(operatorMarkers([comment(stripped.join('\n'))]), [])
})

test('the validator rejects multi-line, empty and malformed entries', () => {
  assert.equal(validNotApplicable([A, B]), true)
  assert.equal(validNotApplicable([]), true)
  for (const bad of [
    null,
    {},
    'notes',
    [{ extension: 'a' }],
    [{ ...A, extra: true }],
    [{ ...A, note: '' }],
    [{ ...A, note: '   ' }],
    [{ ...A, note: 'one\ntwo' }],
    [{ ...A, note: 'one\rtwo' }],
    [{ ...A, extension: '' }],
    [{ ...A, extension: 'a\nb' }],
    [{ ...A, extension: 'a`b' }],
    [{ ...A, note: 7 }],
    [A, null],
  ])
    assert.equal(validNotApplicable(bad), false, JSON.stringify(bad))
  assert.throws(() => notApplicableComment({ head: HEAD, notes: [{ ...A, note: 'a\nb' }] }))
  assert.throws(() => notApplicableComment({ head: 'short', notes: [A] }))
})

test('recordNotApplicable is cosmetic: an unreadable read is unknown and a throw is failed', () => {
  const ctx = { read: () => null, repo: 'o/r', pr: 7, headSha: HEAD, dryRun: false, writes: [] }
  const unreadable = recordNotApplicable(ctx, [A], {
    readOperatorMarkers: () => ({ readable: false }),
    recordHeadNote: () => assert.fail('never records on an unreadable read'),
  })
  assert.equal(unreadable, 'unknown')
  const thrown = recordNotApplicable(ctx, [A], {
    readOperatorMarkers: () => ({ readable: true, markers: [] }),
    recordHeadNote: () => {
      throw new Error('boom')
    },
  })
  assert.equal(thrown, 'failed')
  const calls = []
  const posted = recordNotApplicable(ctx, [A], {
    readOperatorMarkers: () => ({ readable: true, markers: [] }),
    recordHeadNote: (read, args) => (calls.push(args), 'posted'),
  })
  assert.equal(posted, 'posted')
  assert.equal(calls[0].kind, 'not-applicable')
  assert.equal(calls[0].note.digest, notApplicableComment({ head: HEAD, notes: [A] }).digest)
})
