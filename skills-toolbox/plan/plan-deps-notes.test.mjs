import test from 'node:test'
import assert from 'node:assert/strict'

import { planDependencyEdges } from './plan-deps-lib.mjs'
import { renderPlanningLines } from './plan-deps-notes.mjs'

const fileNote = (candidate) => ({ destination: 'planning', reason: 'file-overlap', candidate })
const prNote = (pr, candidate, shared, level = 'file') => ({
  destination: 'planning',
  reason: 'open-pr-overlap',
  pr,
  candidate,
  shared,
  level,
})

test('groups real overlap records by identical shared sets and names each ticket once', () => {
  const result = planDependencyEdges({
    subject: { id: 'subject', identifier: 'TCK-1', areas: ['app/a.mjs', 'app/b.md', 'app/c.go'] },
    candidates: [
      { id: 'two', identifier: 'TCK-2', areas: ['app/b.md', 'app/a.mjs'] },
      { id: 'five', identifier: 'TCK-5', areas: ['app/a.mjs', 'app/b.md'] },
      { id: 'nine', identifier: 'TCK-9', areas: ['app/c.go'] },
    ],
    candidateSetComplete: true,
  })
  assert.equal(result.notes.filter((note) => note.reason === 'file-overlap').length, 3)
  assert.deepEqual(renderPlanningLines(result), [
    'Related, non-blocking file overlap (rebase risk only): TCK-2, TCK-5 (shared: app/a.mjs, app/b.md); TCK-9 (shared: app/c.go).',
  ])
})

test('passes other planning notes through verbatim in input order and excludes risks', () => {
  const notes = [
    fileNote('TCK-2'),
    {
      destination: 'planning',
      reason: 'prerequisite-satisfied',
      text: 'Already landed.\nKeep this.',
    },
    { destination: 'risks', reason: 'no-candidates-compared', text: 'Not planning.' },
    { destination: 'planning', reason: 'hot-overlap', text: 'One hot-region note.' },
    { destination: 'planning', reason: 'future-reason', text: 'Future note, unchanged.' },
    { ...prNote(99, '', ['app/a.mjs']), destination: 'risks', text: 'Unmapped PR.' },
    { destination: 'risks', reason: 'open-prs-truncated', text: 'Truncated.' },
  ]
  assert.deepEqual(renderPlanningLines({ notes }), [
    'Already landed.\nKeep this.',
    'One hot-region note.',
    'Future note, unchanged.',
    'Related, non-blocking file overlap (rebase risk only): TCK-2.',
  ])
})

test('groups open PR notes numerically with levels and caps sorted shared paths', () => {
  const notes = [
    prNote(15, 'TCK-5', ['lib'], 'directory'),
    prNote(12, 'TCK-2', ['g', 'f', 'e', 'd', 'c', 'b', 'a']),
  ]
  assert.deepEqual(renderPlanningLines({ notes }), [
    'Open PRs on the same files (rebase risk only): #12 for TCK-2 (file level; shared: a, b, c, d, e, +2 more); #15 for TCK-5 (directory level; shared: lib).',
  ])
})

test('grouped output is stable when notes and owning records are reordered', () => {
  const nine = fileNote('TCK-9')
  const ten = fileNote('TCK-10')
  const result = {
    notes: [ten, prNote(21, 'TCK-10', ['z', 'a']), nine, prNote(3, 'TCK-9', ['b'])],
    edges: [{ identifier: 'TCK-10', note: ten, shared: ['b', 'a', 'a'] }],
    skipped: [{ id: 'TCK-9', shared: ['a', 'b'] }],
  }
  const expected = [
    'Related, non-blocking file overlap (rebase risk only): TCK-9, TCK-10 (shared: a, b).',
    'Open PRs on the same files (rebase risk only): #3 for TCK-9 (file level; shared: b); #21 for TCK-10 (file level; shared: a, z).',
  ]
  assert.deepEqual(renderPlanningLines(result), expected)
  assert.deepEqual(
    renderPlanningLines({
      notes: [...result.notes].reverse(),
      edges: [...result.edges].reverse(),
      skipped: [...result.skipped].reverse(),
    }),
    expected,
  )
})

test('prefers the note-owning record over an identifier fallback', () => {
  const note = fileNote('TCK-2')
  assert.deepEqual(
    renderPlanningLines({
      notes: [note],
      edges: [{ identifier: 'TCK-2', shared: ['wrong'] }],
      skipped: [{ identifier: 'other', note, shared: ['right'] }],
    }),
    ['Related, non-blocking file overlap (rebase risk only): TCK-2 (shared: right).'],
  )
})

test('caps file paths without truncating identifiers, and retains tickets with no owning record', () => {
  const notes = Array.from({ length: 30 }, (_, i) => fileNote(`TCK-${i + 2}`))
  notes.push(notes[0])
  const result = {
    notes,
    edges: notes.slice(0, 30).map((note) => ({
      note,
      shared: ['g', 'f', 'e', 'd', 'c', 'b', 'a'],
    })),
  }
  assert.deepEqual(renderPlanningLines(result), [
    `Related, non-blocking file overlap (rebase risk only): ${Array.from({ length: 30 }, (_, i) => `TCK-${i + 2}`).join(', ')} (shared: a, b, c, d, e, +2 more).`,
  ])
  assert.deepEqual(renderPlanningLines({ notes: [fileNote('TCK-7')] }), [
    'Related, non-blocking file overlap (rebase risk only): TCK-7.',
  ])
})

test('empty or malformed inputs are ignored without throwing', () => {
  for (const input of [
    undefined,
    null,
    3,
    'notes',
    [],
    {},
    { notes: {} },
    {
      notes: [
        null,
        3,
        {},
        { destination: 'planning', text: {} },
        fileNote(null),
        prNote('bad', 'TCK-2', []),
      ],
    },
  ]) {
    assert.deepEqual(renderPlanningLines(input), [])
  }
})
