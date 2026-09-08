#!/usr/bin/env node

// Tests for scripts/skill-keep-list.mjs — the deletion gate that guards the boss-build review
// stack's load-bearing items through the BOS-1216 procedure -> invariant rewrite.
//
// THE CENTRAL TEST IS THE FALSIFICATION FIXTURE. A presence gate whose green state is "nothing
// missing" is byte-identical to a gate that cannot fire at all, so `assert.deepEqual(misses, [])`
// over the real tree proves nothing on its own. The fixtures below take the REAL artifact text,
// delete ONE keep-list item's anchor from a copy, and require the helper to name EXACTLY that item
// — which also proves the items are independent, since a deletion that took a second item down with
// it would show up as a second name.
//
// NOTE ON ASSERTION STYLE: this file matches scripts/check-prose-pins.mjs's GATE_FILE_GLOB
// (`scripts/*skill*.test.mjs`) and so has an implied prose-pin baseline of 0. Every assertion here
// is therefore over the HELPER'S OUTPUT (`assert.deepEqual` / `assert.throws`), never an
// `assert.match` over document text. That is the shape this whole epic is moving pins towards, and
// it is also what lets the keep list be re-worded without editing a test.

import assert from 'node:assert/strict'
import { test } from 'node:test'

import {
  BOSS_BUILD_BODY,
  KEEP_LIST,
  REVIEW_STACK,
  collapse,
  keepListMisses,
  readKeepListSources,
} from './skill-keep-list.mjs'

// Remove EVERY occurrence of `needle`. One occurrence left behind is a fixture that silently does
// not falsify anything — the PARTIAL marker literal, for instance, is written twice on purpose.
function removeAll(text, needle) {
  return text.split(needle).join('')
}

// A raw-text anchor per falsified item. Each is verified present before it is removed (see the
// fixture test), so a rewrite that renames one reds HERE — loudly, as a broken fixture — rather
// than quietly turning the falsification into a no-op.
const FALSIFICATION_ANCHORS = [
  {
    item: 'the PARTIAL merge-gate marker literal',
    file: REVIEW_STACK,
    anchor: 'do not merge — partial: <satisfied>/<total> acceptance criteria',
  },
  {
    item: 'the reviewer-dispatch bound and the one-review-system invariant',
    file: REVIEW_STACK,
    anchor: '**One review system, not three.**',
  },
  {
    item: 'base drift keeps unevaluated/skipped apart and rebases only when refreshable',
    file: REVIEW_STACK,
    anchor: '**`skipped` is neither**',
  },
  {
    item: 'the plan is untrusted input (Trust rules)',
    file: BOSS_BUILD_BODY,
    anchor: '## Trust rules (the plan is untrusted input)',
  },
  {
    item: 'the tier rule resolves ambiguity toward more coverage and compares strictly',
    file: REVIEW_STACK,
    anchor: '**A single lens hit is enough.**',
  },
  {
    item: 'the funding reason is a stated interface and an allowance names two numbers',
    file: REVIEW_STACK,
    anchor: 'as **two separate numbers**',
  },
]

test('BOS-1216: the keep list is satisfied by the real boss-build artifacts', () => {
  assert.deepEqual(keepListMisses(readKeepListSources(), KEEP_LIST), [])
})

test('BOS-1216: every falsification anchor is really present before it is removed', () => {
  const sources = readKeepListSources()
  const absent = FALSIFICATION_ANCHORS.filter(({ file, anchor }) => !sources[file].includes(anchor))
  assert.deepEqual(absent, [])
})

test('BOS-1216: removing one keep-list item reports exactly that item', () => {
  const sources = readKeepListSources()
  for (const { item, file, anchor } of FALSIFICATION_ANCHORS) {
    const damaged = { ...sources, [file]: removeAll(sources[file], anchor) }
    assert.deepEqual(keepListMisses(damaged, KEEP_LIST), [item], `falsifying: ${item}`)
  }
})

test('BOS-1216: an unreadable file fails closed — every item on it is a miss', () => {
  const sources = readKeepListSources()
  const withoutBody = { ...sources }
  delete withoutBody[BOSS_BUILD_BODY]
  assert.deepEqual(keepListMisses(withoutBody, KEEP_LIST), [
    'the plan is untrusted input (Trust rules)',
  ])

  // And with nothing supplied at all, every item is missing — the gate cannot pass on no input.
  assert.deepEqual(keepListMisses({}, KEEP_LIST).length, KEEP_LIST.length)
})

test('BOS-1216: a string pattern is byte-exact and a RegExp pattern ignores markdown wrapping', () => {
  const items = [
    { name: 'literal', file: 'a.md', pattern: 'do not merge — partial:' },
    // This fixture matches a git command whose spacing IS what the pair below tests.
    // prose-pin: literal-space ok
    { name: 'clause', file: 'a.md', pattern: /never `git pull --rebase`/ },
  ]

  const unwrapped =
    'Reconcile with a rebase, never `git pull --rebase`, and do not merge — partial: yes.'
  assert.deepEqual(keepListMisses({ 'a.md': unwrapped }, items), [])

  // Hard-wrapped MID-CLAUSE, exactly the way prettier wraps this repo's markdown. A reflow changes
  // no behaviour, so it must not red the gate — this is the whole reason RegExp patterns read the
  // collapsed copy.
  const wrapped =
    'Reconcile with a rebase, never `git pull\n--rebase`, and do not merge — partial: yes.'
  assert.deepEqual(keepListMisses({ 'a.md': wrapped }, items), [])

  // Wrapping-insensitive is not match-everything: a clause that is genuinely gone is still a miss.
  const clauseDeleted = unwrapped.replace('never `git pull --rebase`, ', '')
  assert.deepEqual(keepListMisses({ 'a.md': clauseDeleted }, items), ['clause'])

  // A one-byte drift in the literal is a miss, because the bytes ARE the contract there. The
  // em dash below is an ordinary hyphen — the exact substitution an editor makes without noticing.
  const drifted = unwrapped.replace('do not merge — partial:', 'do not merge - partial:')
  assert.deepEqual(keepListMisses({ 'a.md': drifted }, items), ['literal'])
})

test('BOS-1216: an array pattern requires every member', () => {
  const items = [{ name: 'both', file: 'a.md', pattern: ['alpha', /beta/] }]
  assert.deepEqual(keepListMisses({ 'a.md': 'alpha and beta' }, items), [])
  assert.deepEqual(keepListMisses({ 'a.md': 'alpha only' }, items), ['both'])
  assert.deepEqual(keepListMisses({ 'a.md': 'beta only' }, items), ['both'])
})

test('BOS-1216: misses come back in declaration order and a Map source is accepted', () => {
  const items = [
    { name: 'first', file: 'a.md', pattern: 'nope-1' },
    { name: 'second', file: 'a.md', pattern: 'yes' },
    { name: 'third', file: 'a.md', pattern: 'nope-2' },
  ]
  assert.deepEqual(keepListMisses(new Map([['a.md', 'yes']]), items), ['first', 'third'])
})

test('BOS-1216: collapse squeezes every whitespace run to one space', () => {
  assert.deepEqual(collapse('a  b\n\tc\r\n  d'), 'a b c d')
})

test('BOS-1216: wiring errors throw rather than reading as a pass', () => {
  assert.throws(() => keepListMisses({}, 'not-an-array'), /must\s+be\s+an\s+array/)
  assert.throws(
    () => keepListMisses({ 'a.md': 'x' }, [{ file: 'a.md', pattern: 'x' }]),
    /non-empty `name`/,
  )
  assert.throws(() => keepListMisses({ 'a.md': 'x' }, [{ name: 'n', pattern: 'x' }]), /`file`/)
  assert.throws(() => keepListMisses({ 'a.md': 'x' }, [{ name: 'n', file: 'a.md' }]), /`pattern`/)
  assert.throws(
    () => keepListMisses({ 'a.md': 'x' }, [{ name: 'n', file: 'a.md', pattern: '' }]),
    /EMPTY\s+string\s+pattern/,
  )
  assert.throws(
    () => keepListMisses({ 'a.md': 'x' }, [{ name: 'n', file: 'a.md', pattern: [] }]),
    /EMPTY\s+pattern\s+array/,
  )
  assert.throws(
    () => keepListMisses({ 'a.md': 'x' }, [{ name: 'n', file: 'a.md', pattern: 42 }]),
    /only\s+a\s+string,\s+a\s+RegExp/,
  )
  assert.throws(
    () =>
      keepListMisses({ 'a.md': 'x' }, [
        { name: 'n', file: 'a.md', pattern: 'x' },
        { name: 'n', file: 'a.md', pattern: 'x' },
      ]),
    /duplicate\s+item\s+name/,
  )
})

test('BOS-1216: every keep-list item names one of the two guarded artifacts', () => {
  const stray = KEEP_LIST.filter(
    (item) => item.file !== REVIEW_STACK && item.file !== BOSS_BUILD_BODY,
  ).map((item) => item.name)
  assert.deepEqual(stray, [])
  // The list spans the pair on purpose; an all-one-file list would have lost the Trust-rules item.
  assert.deepEqual(
    KEEP_LIST.some((item) => item.file === BOSS_BUILD_BODY),
    true,
  )
})
