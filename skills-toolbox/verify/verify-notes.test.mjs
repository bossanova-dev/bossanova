import assert from 'node:assert/strict'
import { test } from 'node:test'
import { buildBlockers } from './verify-blockers.mjs'
import {
  buildNotes,
  notesComment,
  parseNotesTrailer,
  carryTickets,
  followUpIssue,
} from './verify-notes.mjs'

const head = 'a'.repeat(40)
const finding = {
  severity: 'P3',
  file: 'src/a.mjs',
  line: 3,
  title: 'Simplify this',
  evidence: 'A useful detail',
}

test('note ids are stable and use the blocker finding identity', () => {
  const [note] = buildNotes([finding])
  const [blocker] = buildBlockers({
    reviewFindings: [{ ...finding, severity: 'P2', verified: true, introduced: true }],
  })
  assert.equal(note.id.replace('note:', ''), blocker.id.replace('review:', ''))
  assert.deepEqual(buildNotes([finding]), [note])
})

test('normalizes both producer shapes, orders notes and deduplicates independent of input order', () => {
  const extension = {
    extension: 'quality',
    category: 'style',
    file: 'z.mjs',
    line: 2,
    title: 'Spacing',
    detail: 'Use one space',
  }
  const input = [
    finding,
    { ...finding, evidence: 'Z detail' },
    extension,
    { severity: 'P2', title: 'Not a note' },
  ]
  const notes = buildNotes(input)
  assert.deepEqual(notes, buildNotes([...input].reverse()))
  assert.equal(notes.length, 2)
  assert.equal(notes[0].source, 'extension:quality')
  assert.equal(notes[0].severity, 'style')
  assert.equal(notes[0].text, 'Use one space')
  assert.equal(notes[1].text, finding.evidence)
})

test('renders folded plain bullets, one-line evidence and ticket links with a stable digest', () => {
  const notes = buildNotes([{ ...finding, evidence: 'first\nsecond' }])
  const tickets = { [notes[0].id]: { identifier: 'TASK-7', url: 'https://tracker.example/task/7' } }
  const result = notesComment({ head, notes, tickets })
  assert.equal(result.digest, notesComment({ head, notes, tickets }).digest)
  assert.match(result.body, /\*\*1 note\(s\)\*\* on `aaaaaaaaaaaa` — notes never block this PR\./)
  assert.match(
    result.body,
    /<details><summary>Notes<\/summary>[\s\S]*first second[\s\S]*<\/details>/,
  )
  assert.match(result.body, /→ \[TASK-7\]\(https:\/\/tracker.example\/task\/7\)/)
  assert.ok(!result.body.includes('- [ ]'))
  assert.deepEqual(parseNotesTrailer(result.body), { head, digest: result.digest, notes, tickets })
  assert.notEqual(result.digest, notesComment({ head, notes, tickets: {} }).digest)
})

test('bounds large comments while preserving every note identity and ticket', () => {
  const notes = buildNotes(
    Array.from({ length: 200 }, (_, index) => ({
      ...finding,
      title: `Note ${index}`,
      evidence: 'evidence '.repeat(2000),
    })),
  )
  const tickets = Object.fromEntries(
    notes
      .slice(0, 2)
      .map(({ id }, index) => [
        id,
        { identifier: `TASK-${index}`, url: `https://tracker.example/${index}` },
      ]),
  )
  const result = notesComment({ head, notes, tickets })
  assert.ok(result.body.length <= 65536)
  const parsed = parseNotesTrailer(result.body)
  assert.deepEqual(
    parsed.notes.map(({ id }) => id),
    notes.map(({ id }) => id),
  )
  assert.deepEqual(parsed.tickets, tickets)
  assert.ok(result.body.includes('…'))
})

test('rejects non-final, corrupt and malformed trailers', () => {
  const { body } = notesComment({ head, notes: buildNotes([finding]), tickets: {} })
  assert.equal(parseNotesTrailer(body + '\na later line'), null)
  assert.equal(
    parseNotesTrailer(body.replace(/digest=[a-f0-9]{16}/, 'digest=0000000000000000')),
    null,
  )
  assert.equal(parseNotesTrailer(body.replace(/ ([\w-]+) -->$/, ' _ -->')), null)
  const encode = (payload) => Buffer.from(JSON.stringify(payload)).toString('base64url')
  const trailer = (payload) =>
    `<!-- boss-verify-notes head=${head} digest=0000000000000000 ${encode(payload)} -->`
  assert.equal(parseNotesTrailer(trailer({ version: 2, notes: [], tickets: {} })), null)
  assert.equal(parseNotesTrailer(null), null)
})

test('carries tickets only for surviving note identities', () => {
  const notes = buildNotes([finding])
  const link = { identifier: 'TASK-7', url: 'https://tracker.example/task/7' }
  assert.deepEqual(
    carryTickets({ tickets: { [notes[0].id]: link, 'note:000000000000': link } }, notes),
    { [notes[0].id]: link },
  )
  assert.deepEqual(carryTickets(null, notes), {})
})

test('follow-up issue describes the note with a complete deterministic marker and bounded title', () => {
  const note = buildNotes([finding])[0]
  const input = { repo: 'o/r', pr: 7, prUrl: 'https://github.com/o/r/pull/7', note }
  const issue = followUpIssue(input)
  assert.equal(issue.title, 'Follow-up: Simplify this')
  assert.equal(issue.marker, `<!-- boss-follow-up repo=o/r pr=7 note=${note.id} -->`)
  assert.equal(issue.description.split('\n').at(-1), issue.marker)
  for (const text of [input.prUrl, note.id, note.severity, note.source, 'src/a.mjs:3', note.text])
    assert.ok(issue.description.includes(text))
  assert.deepEqual(followUpIssue(input), issue)
  const long = followUpIssue({ ...input, note: { ...note, summary: 'x'.repeat(300) } })
  assert.equal(long.title.length, 256)
  assert.ok(long.title.endsWith('…'))
})
