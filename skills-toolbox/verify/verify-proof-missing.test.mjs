import assert from 'node:assert/strict'
import test from 'node:test'
import { operatorMarkers } from './verify-gate.mjs'
import { proofCommentMarker } from './verify-proof-publish.mjs'
import { parseProofLine } from './verify-proof.mjs'
import {
  BENIGN_CHECK_RUN_REASONS,
  BENIGN_PAGE_REASONS,
  BENIGN_RECORD_REASONS,
  PROOF_MISSING_MARKER,
  proofMissingBossNote,
  proofMissingMarker,
  proofMissingNote,
  proofProblems,
  proofResolvedNote,
} from './verify-proof-missing.mjs'

const HEAD = 'd4e332342dad0123456789abcdef0123456789ab'
const SHORT = HEAD.slice(0, 12)

const lastNonEmptyLine = (body) =>
  body
    .split('\n')
    .map((line) => line.trim())
    .filter(Boolean)
    .at(-1) ?? ''

test('benign record, page, and check-run reasons yield no problems', () => {
  assert.ok(Object.isFrozen(BENIGN_RECORD_REASONS))
  assert.ok(Object.isFrozen(BENIGN_PAGE_REASONS))
  assert.ok(Object.isFrozen(BENIGN_CHECK_RUN_REASONS))

  assert.deepEqual(BENIGN_RECORD_REASONS, ['repo-proof-present'])
  assert.deepEqual(BENIGN_PAGE_REASONS, [
    'not-parked',
    'dry-run',
    'not-configured',
    'disabled',
    'opted-out',
  ])
  assert.deepEqual(BENIGN_CHECK_RUN_REASONS, ['fallback-disabled', 'dry-run', 'daemon-unsupported'])

  for (const reason of BENIGN_RECORD_REASONS) {
    assert.deepEqual(proofProblems({ recorded: false, reason }), [], `record:${reason}`)
  }
  for (const reason of BENIGN_PAGE_REASONS) {
    assert.deepEqual(
      proofProblems({ recorded: true, page: { published: false, reason } }),
      [],
      `page:${reason}`,
    )
  }
  for (const reason of BENIGN_CHECK_RUN_REASONS) {
    assert.deepEqual(
      proofProblems({ recorded: true, checkRun: { posted: false, reason } }),
      [],
      `check-run:${reason}`,
    )
  }
})

test('non-benign record, page, and check-run failures yield one expected problem each', () => {
  const cases = [
    [
      { recorded: false, reason: 'record-invalid' },
      { part: 'record', reason: 'record-invalid' },
    ],
    [
      { recorded: false, reason: 'comments-unreadable' },
      { part: 'record', reason: 'comments-unreadable' },
    ],
    [
      { recorded: false, reason: 'proof-failed' },
      { part: 'record', reason: 'proof-failed' },
    ],
    [
      { recorded: false, note: 'failed' },
      { part: 'record', reason: 'note-failed' },
    ],
    [
      { recorded: false, note: 'unknown' },
      { part: 'record', reason: 'note-unknown' },
    ],
    [{ recorded: false }, { part: 'record', reason: 'unknown' }],
    [
      { recorded: true, page: { published: false, reason: 'render-failed' } },
      { part: 'page', reason: 'render-failed' },
    ],
    [
      { recorded: true, page: { published: false, reason: 'upload-failed' } },
      { part: 'page', reason: 'upload-failed' },
    ],
    [
      { recorded: true, page: { published: false } },
      { part: 'page', reason: 'unknown' },
    ],
    [
      { recorded: true, checkRun: { posted: false, reason: 'check-run-failed' } },
      { part: 'check-run', reason: 'check-run-failed' },
    ],
    [
      { recorded: true, checkRun: { posted: false } },
      { part: 'check-run', reason: 'unknown' },
    ],
  ]

  for (const [outcome, expected] of cases) {
    assert.deepEqual(proofProblems(outcome), [expected], `${expected.part}:${expected.reason}`)
  }
})

test('detached, healthy, and empty outcomes yield no problems', () => {
  assert.deepEqual(proofProblems({ detached: true, log: '/tmp/x.log' }), [])
  assert.deepEqual(
    proofProblems({
      detached: true,
      recorded: false,
      reason: 'record-invalid',
      page: { published: false, reason: 'upload-failed' },
    }),
    [],
  )
  assert.deepEqual(
    proofProblems({
      recorded: true,
      page: { published: true, url: 'https://proof.example/index.html' },
      checkRun: { posted: true, url: 'https://github.com/acme/repo/runs/1' },
    }),
    [],
  )
  assert.deepEqual(proofProblems(null), [])
  assert.deepEqual(proofProblems(undefined), [])
})

test('simultaneous failures return all non-benign problems in order', () => {
  assert.deepEqual(
    proofProblems({
      recorded: true,
      page: { published: false, reason: 'upload-failed' },
      checkRun: { posted: false, reason: 'check-run-failed' },
    }),
    [
      { part: 'page', reason: 'upload-failed' },
      { part: 'check-run', reason: 'check-run-failed' },
    ],
  )
  assert.deepEqual(
    proofProblems({
      recorded: false,
      note: 'failed',
      page: { published: false, reason: 'render-failed' },
      checkRun: { posted: false, reason: 'check-run-failed' },
    }),
    [
      { part: 'record', reason: 'note-failed' },
      { part: 'page', reason: 'render-failed' },
      { part: 'check-run', reason: 'check-run-failed' },
    ],
  )
})

test('proofMissingNote lists problems, republish command, marker, and no boss-proof line', () => {
  const problems = [
    { part: 'page', reason: 'upload-failed' },
    { part: 'check-run', reason: 'check-run-failed' },
  ]
  const note = proofMissingNote({ headSha: HEAD, pr: 42, repo: 'Acme/Widgets', problems })

  assert.equal(parseProofLine(note.body), null)
  assert.equal(note.body.includes('<!-- boss-proof:'), false)
  assert.match(note.body, new RegExp(`\`${SHORT}\``))
  assert.match(note.body, /- `page`: `upload-failed`/)
  assert.match(note.body, /- `check-run`: `check-run-failed`/)
  assert.match(
    note.body,
    new RegExp(`verify-gate\\.mjs proof --pr 42 --head ${HEAD} --judge <file> --repo Acme/Widgets`),
  )

  const last = lastNonEmptyLine(note.body)
  const match = last.match(PROOF_MISSING_MARKER)
  assert.ok(match, `expected last line to match marker: ${last}`)
  assert.equal(match[1], HEAD)
  assert.equal(match[2], note.digest)
  assert.match(note.digest, /^[0-9a-f]{16}$/)
  assert.equal(last, proofMissingMarker(HEAD, note.digest))
  assert.ok(note.body.endsWith(`\n\n${proofMissingMarker(HEAD, note.digest)}`))

  const fallbackRepo = proofMissingNote({ headSha: HEAD, pr: 42, repo: '', problems })
  assert.match(
    fallbackRepo.body,
    new RegExp(`verify-gate\\.mjs proof --pr 42 --head ${HEAD} --judge <file> --repo <owner/name>`),
  )
  assert.notEqual(fallbackRepo.digest, note.digest)
})

test('proofResolvedNote formats with and without pageUrl and carries a distinct marker digest', () => {
  const missing = proofMissingNote({
    headSha: HEAD,
    pr: 42,
    repo: 'Acme/Widgets',
    problems: [{ part: 'record', reason: 'record-invalid' }],
  })
  const pageUrl = `https://proof.example/Acme/Widgets/42/${HEAD}/index.html`
  const withUrl = proofResolvedNote({ headSha: HEAD, pageUrl })
  const withoutUrl = proofResolvedNote({ headSha: HEAD, pageUrl: null })

  for (const [label, note] of [
    ['withUrl', withUrl],
    ['withoutUrl', withoutUrl],
  ]) {
    assert.equal(parseProofLine(note.body), null, label)
    assert.equal(note.body.includes('<!-- boss-proof:'), false, label)
    assert.match(note.body, new RegExp(`\`${SHORT}\``), label)
    const last = lastNonEmptyLine(note.body)
    const match = last.match(PROOF_MISSING_MARKER)
    assert.ok(match, `${label}: ${last}`)
    assert.equal(match[1], HEAD, label)
    assert.equal(match[2], note.digest, label)
    assert.equal(match[3], 'resolved', label)
    assert.notEqual(note.digest, missing.digest, label)
    assert.ok(note.body.endsWith(`\n\n${proofMissingMarker(HEAD, note.digest, true)}`), label)
  }

  assert.ok(withUrl.body.includes(`: ${pageUrl}`))
  assert.ok(withoutUrl.body.includes(`\`${SHORT}\` is now recorded.`))
  assert.notEqual(withUrl.digest, withoutUrl.digest)
})

test('proofMissingBossNote returns one concise line naming PR, short head, and each problem', () => {
  const line = proofMissingBossNote({
    pr: 42,
    headSha: HEAD,
    problems: [
      { part: 'record', reason: 'record-invalid' },
      { part: 'check-run', reason: 'check-run-failed' },
    ],
  })
  assert.equal(line.includes('\n'), false)
  assert.match(line, /#42\b/)
  assert.ok(line.includes(SHORT))
  assert.ok(line.includes('record: record-invalid'))
  assert.ok(line.includes('check-run: check-run-failed'))
})

test('proofCommentMarker and operatorMarkers recognize proof-missing and resolved notes exclusively', () => {
  const missing = proofMissingNote({
    headSha: HEAD,
    pr: 42,
    repo: 'Acme/Widgets',
    problems: [{ part: 'record', reason: 'record-invalid' }],
  })
  const resolved = proofResolvedNote({
    headSha: HEAD,
    pageUrl: `https://proof.example/Acme/Widgets/42/${HEAD}/index.html`,
  })

  for (const [note, resolvedFlag] of [
    [missing, false],
    [resolved, true],
  ]) {
    const last = lastNonEmptyLine(note.body)
    assert.deepEqual(proofCommentMarker(note.body, last), {
      kind: 'proof-missing',
      head: HEAD,
      fields: { digest: note.digest, resolved: resolvedFlag },
    })
  }

  const comment = (id, body) => ({
    id,
    body,
    user: { login: 'verify' },
    author_association: 'MEMBER',
    created_at: '2026-11-10T00:00:00Z',
    html_url: `https://github.com/Acme/Widgets/pull/42#issuecomment-${id}`,
  })
  const markers = operatorMarkers([comment(1, missing.body), comment(2, resolved.body)])
  assert.equal(markers.length, 2)
  assert.deepEqual(
    markers.map((m) => ({
      kind: m.kind,
      head: m.head,
      digest: m.digest,
      resolved: m.resolved,
      id: m.id,
    })),
    [
      { kind: 'proof-missing', head: HEAD, digest: missing.digest, resolved: false, id: 1 },
      { kind: 'proof-missing', head: HEAD, digest: resolved.digest, resolved: true, id: 2 },
    ],
  )
})
