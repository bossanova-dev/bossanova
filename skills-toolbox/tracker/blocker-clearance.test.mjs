import assert from 'node:assert/strict'
import { test } from 'node:test'
import {
  BLOCKER_CLEARED_STATE_TYPES,
  prUrlsOf,
  classifyBlocker,
  blockerVerdict,
  dependentPathsFrom,
  softClearedBlockers,
} from './blocker-clearance.mjs'
const url = 'https://github.com/example/project/pull/12'
const blocker = (type = 'started', urls = [url]) => ({
  state: { type },
  attachments: { nodes: urls.map((url) => ({ url })) },
})
test('closed states clear without PR facts', () => {
  assert.deepEqual([...BLOCKER_CLEARED_STATE_TYPES].sort(), ['canceled', 'completed'])
  for (const type of BLOCKER_CLEARED_STATE_TYPES)
    assert.deepEqual(classifyBlocker(blocker(type)), { clearance: 'cleared', reason: 'state' })
})
test('PR links normalize suffixes and ignore other attachments', () => {
  assert.deepEqual(
    prUrlsOf(
      blocker('started', [
        url + '/files',
        url,
        'https://github.com/example/project/issues/12',
        'https://other.com/example/project/pull/12',
      ]),
    ),
    [url],
  )
})
test('PR facts distinguish merged, open, missing and failed reads', () => {
  assert.equal(classifyBlocker(blocker()).clearance, 'needs-pr-facts')
  assert.deepEqual(classifyBlocker(blocker(), { prFacts: new Map([[url, { state: 'MERGED' }]]) }), {
    clearance: 'cleared',
    reason: 'merged-pr',
  })
  assert.equal(
    classifyBlocker(blocker(), { prFacts: new Map([[url, { state: 'OPEN' }]]) }).clearance,
    'uncleared',
  )
  assert.deepEqual(classifyBlocker(blocker(), { prFacts: new Map([[url, { error: 'failed' }]]) }), {
    clearance: 'uncleared',
    reason: 'pr-unknown',
  })
  assert.equal(classifyBlocker(blocker('started', [])).clearance, 'uncleared')
})
test('aggregate fails closed while preserving unknown verdicts', () => {
  assert.equal(blockerVerdict([blocker()]), 'unknown')
  assert.equal(blockerVerdict([blocker('completed'), blocker('started', [])]), 'blocked')
  assert.equal(
    blockerVerdict([blocker()], { prFacts: new Map([[url, { state: 'MERGED' }]]) }),
    'unblocked',
  )
  assert.equal(blockerVerdict([]), 'unblocked')
})

const reviewed = { id: 'ROOT', ...blocker(), state: { name: 'Code_Review', type: 'started' } }
const green = { state: 'OPEN', isDraft: false, green: true, files: ['src/root.mjs'] }
const selection = (fact = green, paths = ['src/child.mjs']) => ({
  mode: 'selection',
  inReviewState: 'Code Review',
  dependentPaths: paths,
  prFacts: new Map([[url, fact]]),
})
test('selection alone clears reviewed green disjoint work and reports its ordering', () => {
  assert.equal(classifyBlocker(reviewed, selection()).clearance, 'review-clearable')
  assert.equal(blockerVerdict([reviewed], selection()), 'unblocked')
  assert.deepEqual(softClearedBlockers([reviewed], selection()), [{ id: 'ROOT', pr: url }])
  assert.equal(blockerVerdict([reviewed], { ...selection(), mode: 'strict' }), 'blocked')
  assert.equal(blockerVerdict([reviewed, blocker('started', [])], selection()), 'blocked')
})
test('selection fails closed for overlap, incomplete facts and non-review states', () => {
  for (const fact of [
    { ...green, files: ['src/child.mjs'] },
    { ...green, files: null },
    { ...green, isDraft: true },
    { ...green, green: false },
    { ...green, state: 'CLOSED' },
    { error: 'unavailable' },
    null,
  ])
    assert.equal(classifyBlocker(reviewed, selection(fact)).clearance, 'uncleared')
  for (const paths of [[], ['src/'], ['src'], ['./src/child.mjs']]) {
    const fact = { ...green, files: ['src/child.mjs'] }
    assert.equal(classifyBlocker(reviewed, selection(fact, paths)).clearance, 'uncleared')
  }
  assert.equal(
    classifyBlocker({ ...reviewed, state: { name: 'In Progress', type: 'started' } }, selection())
      .clearance,
    'uncleared',
  )
  assert.equal(
    classifyBlocker(reviewed, { ...selection(), inReviewState: undefined }).clearance,
    'uncleared',
  )
  assert.equal(classifyBlocker(reviewed, selection({ state: 'MERGED' })).reason, 'merged-pr')
})
test('every linked open PR must be readable, green and disjoint', () => {
  const other = url.replace('/12', '/13')
  const root = { ...reviewed, attachments: { nodes: [{ url }, { url: other }] } }
  const opts = selection()
  assert.equal(classifyBlocker(root, opts).clearance, 'uncleared')
  opts.prFacts.set(other, { ...green, files: ['elsewhere.mjs'] })
  assert.equal(classifyBlocker(root, opts).clearance, 'review-clearable')
  opts.prFacts.set(other, { ...green, green: false })
  assert.equal(classifyBlocker(root, opts).clearance, 'uncleared')
})
test('gate permits possibly disjoint work without reading plan paths or files', () => {
  assert.equal(
    classifyBlocker(reviewed, { ...selection({ ...green, files: null }, []), mode: 'gate' })
      .clearance,
    'review-clearable',
  )
})
test('dependent paths use only repository path tokens in the supplied Key changes section', () => {
  assert.deepEqual(
    dependentPathsFrom(
      'Edit `./src/child.mjs`, `src/`, `README.md`, `not a path`, `--mode`, `scripts`, `/tmp/a`, `../outside`, `https://example.com/a`. Duplicate `src/child.mjs`.',
    ),
    ['src/child.mjs', 'src/', 'README.md', 'scripts'],
  )
  assert.deepEqual(dependentPathsFrom(null), [])
})
