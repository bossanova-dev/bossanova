#!/usr/bin/env node

// Falsification suite for the fail-closed region extractor (BOS-737).
//
// Discipline and rationale: docs/solutions/best-practices/feed-a-new-gate-the-values-it-exists-to-reject.md
// Every case below feeds `region()` the exact input it exists to reject and asserts it
// THROWS with a message naming the marker that moved — a helper that merely returned a
// short string on a missing marker would reproduce the bug it replaces. The happy-path
// case asserts byte equality with the raw `slice(indexOf, indexOf)` result, which is what
// makes the converted call sites behaviour-preserving rather than merely green.
//
// The `precedes()` cases at the end do the same for the ordering shape
// `src.indexOf(a) < src.indexOf(b)`, which is fail-OPEN on a deleted `a`: the first case
// asserts the raw expression is `true` on that input before asserting the helper throws, so
// the case cannot be read as a style preference.
//
// Every probe runs against an in-memory fixture string. Nothing here reads, writes, or
// mutates a real source file.

import assert from 'node:assert/strict'
import test from 'node:test'

import { precedes, region, regionUntilNext } from './gate-region-lib.mjs'

const START = '## Preflight'
const END = '## Step 1:'
const SOURCE = `# Skill\n\nintro\n\n${START}\nbody line one\nbody line two\n\n${END}\nafter\n`

test('region returns bytes identical to the raw slice(indexOf, indexOf) form', () => {
  // Spelled out over two index variables rather than inline: the inline form is exactly
  // what scripts/check-vacuous-regions.mjs forbids, and this file is scanned by it. The
  // computation is identical — both indices are searched from position 0.
  const startIndex = SOURCE.indexOf(START)
  const endIndex = SOURCE.indexOf(END)
  const raw = SOURCE.slice(startIndex, endIndex)

  assert.equal(region(SOURCE, START, END), raw)
  assert.equal(region(SOURCE, START, END), '## Preflight\nbody line one\nbody line two\n\n')
})

test('region with no end marker runs to the end of source, like the one-argument raw form', () => {
  const startIndex = SOURCE.indexOf(END)
  assert.equal(region(SOURCE, END), SOURCE.slice(startIndex))
  assert.equal(region(SOURCE, END, null), SOURCE.slice(startIndex))
  assert.equal(region(SOURCE, END, undefined), SOURCE.slice(startIndex))
})

test('region throws naming the start marker when it is absent', () => {
  assert.throws(
    () => region(SOURCE, '## Preflight Checklist', END),
    (error) => {
      assert.match(error.message, /start marker not found/)
      assert.match(error.message, /## Preflight Checklist/)
      // The start-marker failure must not be confusable with the end-marker one.
      assert.doesNotMatch(error.message, /end marker/)
      return true
    },
  )
})

test('region throws naming the end marker when only the end marker is absent', () => {
  assert.throws(
    () => region(SOURCE, START, '## Step 2:'),
    (error) => {
      assert.match(error.message, /end marker not found/)
      assert.match(error.message, /## Step 2:/)
      // Distinguishable from the missing-start message, so the failure says which end moved.
      assert.doesNotMatch(error.message, /start marker not found/)
      return true
    },
  )
})

test('region throws when the end marker is found before the start marker', () => {
  // Both markers exist, so neither not-found path fires; the raw form would silently
  // yield '' here and every negative assertion over it would pass.
  assert.throws(
    () => region(SOURCE, END, START),
    (error) => {
      assert.match(error.message, /precedes start marker/)
      assert.match(error.message, /## Preflight/)
      assert.match(error.message, /## Step 1:/)
      assert.doesNotMatch(error.message, /not found/)
      return true
    },
  )
})

test('region throws when the extracted region is empty or whitespace-only', () => {
  // The live shape of this failure: the end marker is a PREFIX of the start marker, so
  // `indexOf` finds both at the same offset and the region is exactly ''. Neither
  // not-found path fires and the ordering check passes, so this is the only guard left.
  const fenced = 'text\n```json\n{ "commitsMade": [] }\n```\n'
  assert.throws(
    () => region(fenced, '```json', '```'),
    (error) => {
      assert.match(error.message, /empty or whitespace-only/)
      assert.match(error.message, /```json/)
      assert.doesNotMatch(error.message, /not found|precedes/)
      return true
    },
  )

  // Whitespace-only rather than strictly empty: the region carries bytes, but nothing an
  // assertion could ever match, which is the same silent pass by a different route.
  const blank = 'head\n \t \ntail'
  assert.throws(() => region(blank, '\n \t', '\ntail'), /empty or whitespace-only/)
})

test('region names the label it was given', () => {
  assert.throws(() => region(SOURCE, 'nope', null, 'Step 6c'), /^Error: Step 6c: /)
  assert.throws(() => region(SOURCE, 'nope'), /^Error: region: /)
})

test('region accepts a good region unchanged — it does not throw on everything', () => {
  // The mirror-image failure of a hasty fail-closed helper. A gate that blocks
  // everything is as useless as one that blocks nothing.
  assert.equal(region(SOURCE, '# Skill', START), '# Skill\n\nintro\n\n')
  assert.equal(region(SOURCE, 'after'), 'after\n')
})

// --- regionUntilNext: the end marker is searched from after the start marker ------------

// The live case: a fenced block whose closing delimiter is a suffix of its opening one.
// `region(FENCED, '```json', '```')` finds both at the same offset and (correctly) throws
// "empty or whitespace-only" — see the case above. Only a search that starts after the
// opening fence can express this region at all.
const FENCED =
  'prose\n```sh\nnot this block\n```\n\nmore\n\n```json\n{ "commitsMade": [] }\n```\ntail\n'

test('regionUntilNext matches the raw fromIndex form byte for byte', () => {
  // Spelled over index variables rather than inline: the inline form is what
  // scripts/check-vacuous-regions.mjs forbids, and this file is scanned by it.
  const startIndex = FENCED.indexOf('```json')
  const endIndex = FENCED.indexOf('```', startIndex + '```json'.length)
  const raw = FENCED.slice(startIndex, endIndex)

  assert.equal(regionUntilNext(FENCED, '```json', '```'), raw)
  assert.equal(regionUntilNext(FENCED, '```json', '```'), '```json\n{ "commitsMade": [] }\n')
})

test('regionUntilNext does not settle for an earlier end marker', () => {
  // The whole reason this is not `region()`: searching from 0 would land on the ```sh
  // fence, which precedes the start marker, and yield a reversed range.
  const extracted = regionUntilNext(FENCED, '```json', '```')
  assert.ok(!extracted.includes('not this block'))
  assert.ok(extracted.startsWith('```json'))
})

test('regionUntilNext throws naming the start marker when it is absent', () => {
  assert.throws(
    () => regionUntilNext(FENCED, '```yaml', '```'),
    (error) => {
      assert.match(error.message, /start marker not found/)
      assert.match(error.message, /```yaml/)
      assert.doesNotMatch(error.message, /end marker/)
      return true
    },
  )
})

test('regionUntilNext throws when no end marker follows the start marker', () => {
  // An end marker that exists only BEFORE the start is the interesting direction: a
  // fromIndex search must not reach backwards and hand back a reversed slice.
  const unterminated = 'head\n```\nearly fence\n\n```json\n{ "a": 1 }\n'
  assert.throws(
    () => regionUntilNext(unterminated, '```json', '```'),
    (error) => {
      assert.match(error.message, /end marker not found after/)
      assert.match(error.message, /```json/)
      return true
    },
  )
})

test('regionUntilNext throws when the region between the markers is whitespace-only', () => {
  // Narrower than `region()`'s equivalent guard, and deliberately so: because the end
  // search starts AFTER the start marker, the region always contains that marker, so it
  // can only be blank when the marker itself is blank. Pinned anyway — the guard is what
  // makes an accidentally-whitespace marker loud instead of a silently blank region.
  assert.throws(() => regionUntilNext('a\n  \nb', '\n  ', '\n'), /is empty or whitespace-only/)
  // The adjacent-marker case that IS empty under `region()` is non-empty here, because the
  // start marker is part of the region. Non-vacuity of the case above rests on that.
  assert.equal(regionUntilNext('head\nSTARTEND\ntail', 'START', 'END'), 'START')
})

test('regionUntilNext names the label it was given', () => {
  assert.throws(
    () => regionUntilNext(FENCED, 'nope', '```', 'return object'),
    /^Error: return object: /,
  )
  assert.throws(() => regionUntilNext(FENCED, 'nope', '```'), /^Error: region: /)
})

// --- precedes: the same vacuity one operator over ----------------------------------------

test('the raw ordering form passes on exactly the value it forbids — precedes does not', () => {
  // Step 2 of the falsification recipe: construct the forbidden value and assert the helper
  // BLOCKS. The control on the first line is the whole point — the raw expression is not merely
  // imprecise here, it is green, and green is indistinguishable from a correctly ordered file.
  const missing = 'body\n## Phase 1\ntail\n'
  assert.equal(missing.indexOf('## Caller deadline') < missing.indexOf('## Phase 1'), true)

  assert.throws(
    () => precedes(missing, '## Caller deadline', '## Phase 1'),
    (error) => {
      assert.match(error.message, /earlier marker not found/)
      assert.match(error.message, /## Caller deadline/)
      // Distinguishable from the later-marker failure, so the message says which one moved.
      assert.doesNotMatch(error.message, /later marker/)
      return true
    },
  )
})

test('precedes throws naming the later marker when only that one is absent', () => {
  // The mirror direction. Raw, this one is a false NEGATIVE (`n < -1` is false), so it fails —
  // but it fails claiming the order is wrong when the marker is simply gone.
  assert.throws(
    () => precedes(SOURCE, START, '## Step 9:'),
    (error) => {
      assert.match(error.message, /later marker not found/)
      assert.match(error.message, /## Step 9:/)
      assert.doesNotMatch(error.message, /earlier marker not found/)
      return true
    },
  )
})

test('precedes answers both directions when both markers are present', () => {
  // A helper that threw on everything would be as useless as the form it replaces. With both
  // markers present it is exactly the raw comparison, which is what makes converting a call
  // site behaviour-preserving on the happy path.
  assert.equal(precedes(SOURCE, START, END), true)
  assert.equal(precedes(SOURCE, END, START), false)
  assert.equal(precedes(SOURCE, START, END), SOURCE.indexOf(START) < SOURCE.indexOf(END))
})

test('precedes works on an ordered array as well as on text', () => {
  // Several converted sites pin the order of tokens in an argv or a run order, not in prose.
  const argv = ['-c', 'user.name=x', 'commit', '-m', 'subject']
  assert.equal(precedes(argv, '-c', 'commit'), true)
  assert.equal(precedes(argv, 'commit', '-c'), false)
  assert.throws(() => precedes(argv, '--amend', 'commit'), /earlier marker not found/)
})

test('precedes names the label it was given', () => {
  assert.throws(() => precedes(SOURCE, 'nope', END, 'Step 8 order'), /^Error: Step 8 order: /)
  assert.throws(() => precedes(SOURCE, 'nope', END), /^Error: order: /)
})
