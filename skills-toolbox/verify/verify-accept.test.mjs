import assert from 'node:assert/strict'
import test from 'node:test'
import {
  acceptMarker,
  followUpMarker,
  parseAcceptLine,
  acceptedOn,
  splitAccepted,
  followUpIssue,
  acceptedLines,
  acceptedNote,
} from './verify-accept.mjs'

const head = 'a'.repeat(40)
const blocker = {
  id: 'review:123456789abc',
  kind: 'review-finding',
  severity: 'P1',
  file: 'app.go',
  line: 12,
  summary: 'Handle the error',
  text: 'evidence with ``` inside',
}
const acceptance = {
  kind: 'accept',
  head,
  blocker: blocker.id,
  ticket: 'TASK-12',
  at: 2,
  url: 'https://example.com/comment/12',
}

test('acceptance records round-trip and malformed markers fail closed', () => {
  assert.deepEqual(
    parseAcceptLine(acceptMarker({ head, blocker: blocker.id, ticket: 'TASK-12' })),
    { kind: 'accept', head, blocker: blocker.id, ticket: 'TASK-12' },
  )
  assert.equal(
    parseAcceptLine(`<!-- boss-accept head=${head} blocker=${blocker.id} ticket= -->`),
    null,
  )
  assert.equal(parseAcceptLine(`prefix ${acceptMarker(acceptance)}`), null)
  assert.equal(
    followUpMarker({ repo: 'owner/repo', pr: 9, blocker: blocker.id }),
    '<!-- boss-accept-followup repo=owner/repo pr=9 blocker=review:123456789abc -->',
  )
})

test('newest acceptance wins only on the exact head and only review findings disappear', () => {
  const latest = { ...acceptance, at: 5, ticket: 'TASK-13' }
  const map = acceptedOn([latest, acceptance, { ...latest, head: 'b'.repeat(40), at: 6 }], head)
  assert.equal(map.get(blocker.id), latest)
  map.set('check:unit', acceptance)
  const check = { id: 'check:unit', kind: 'check' }
  assert.deepEqual(splitAccepted([blocker, check], map), {
    blockers: [check],
    accepted: [{ id: blocker.id, ticket: 'TASK-13' }],
  })
})

test('follow-up content preserves fenced evidence and reason, truncates title and pins marker last', () => {
  const issue = followUpIssue({
    repo: 'owner/repo',
    pr: 9,
    head,
    blocker: { ...blocker, summary: 'x'.repeat(300) },
    reason: 'reason ``` fence',
    prUrl: 'https://example.com/pr/9',
    ticket: { id: 'TASK-1', url: 'https://example.com/task/1' },
  })
  assert.equal(Array.from(issue.title).length, 256)
  assert.ok(issue.title.endsWith('…'))
  assert.ok(issue.description.includes('````\nevidence with ``` inside\n````'))
  assert.ok(issue.description.includes('````\nreason ``` fence\n````'))
  assert.ok(issue.description.includes('TASK-1'))
  assert.ok(issue.description.endsWith(issue.marker))
})

test('accepted notes have stable digest and preserve follow-up and comment links', () => {
  const note = acceptedNote({ headSha: head, acceptances: acceptedOn([acceptance], head) })
  assert.deepEqual(parseAcceptLine(note.body.split('\n').at(-1)), {
    kind: 'accepted-note',
    head,
    digest: note.digest,
  })
  assert.equal(
    note.digest,
    acceptedNote({ headSha: head, acceptances: acceptedOn([acceptance], head) }).digest,
  )
  assert.ok(acceptedLines([acceptance], head)[0].includes(acceptance.url))
  assert.ok(note.body.includes('TASK-12'))
  assert.notEqual(
    note.digest,
    acceptedNote({
      headSha: head,
      acceptances: acceptedOn([{ ...acceptance, ticket: 'TASK-13' }], head),
    }).digest,
  )
})
