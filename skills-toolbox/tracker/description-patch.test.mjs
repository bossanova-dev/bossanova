import { test } from 'node:test'
import assert from 'node:assert/strict'
import {
  computeDescriptionPatch,
  applyDescriptionPatch,
  REFERENCE_PATTERNS,
} from './description-patch.mjs'

const padding = '\n## Original notes\n' + 'unchanged source notes with details\n'.repeat(40)

function roundTrip(base, intended) {
  const { ops } = computeDescriptionPatch(base, intended)
  assert.ok(ops)
  assert.equal(applyDescriptionPatch(base, ops), intended)
  for (const op of ops) {
    const anchor = op.anchor ?? op.old_string
    assert.equal(base.indexOf(anchor), base.lastIndexOf(anchor))
    assert.ok(anchor.length)
    assertReferenceFree(anchor)
  }
  return ops
}

test('prepends a planning head with a single anchored insertion', () => {
  const ops = roundTrip(padding, '## Summary\nA plan\n' + padding)
  assert.equal(ops.length, 1)
  assert.equal(ops[0].op, 'insert_before')
})

test('adds notes under two headings without resending the source', () => {
  const base = '## Planning\n- existing\n\n## Open Questions\n- existing question\n' + padding
  const intended = base
    .replace('- existing\n', '- existing\n- dependency\n')
    .replace('- existing question\n', '- existing question\n- new question\n')
  const ops = roundTrip(base, intended)
  assert.equal(ops.length, 2)
  assert.ok(ops.every((op) => op.op.startsWith('insert_')))
})

test('replaces inline premise annotations in both directions', () => {
  const base = '- first premise\n- second premise (premise drift: old)\n' + padding
  const intended = base
    .replace('first premise', 'first premise (premise drift: new)')
    .replace(' (premise drift: old)', '')
  assert.ok(roundTrip(base, intended).some((op) => op.op === 'replace'))
})

test('grows repeated anchors with neighboring source context', () => {
  const base = '## First\n- [ ]\n## Second\n- [ ]\n' + padding
  roundTrip(base, base.replace('## Second\n- [ ]', '## Second\n- [ ]\n- added'))
})

test('insertions containing another anchor never emit an unprovable patch', () => {
  const base = 'first unique line\nsecond unique line\nthird unique line\n' + padding
  const intended = base
    .replace('first unique line\n', 'first unique line\nsecond unique line\n')
    .replace('third unique line\n', 'third unique line\nnew note\n')
  const { ops, reason } = computeDescriptionPatch(base, intended)
  if (ops) assert.equal(applyDescriptionPatch(base, ops), intended)
  else assert.ok(reason)
})

test('falls back for identical, empty, oversized and uneconomical patches', () => {
  assert.equal(computeDescriptionPatch(padding, padding).ops, null)
  assert.equal(computeDescriptionPatch('', padding).ops, null)
  assert.equal(computeDescriptionPatch('a', 'b').ops, null)
  const base = Array.from({ length: 52 }, (_, i) => `line ${i}\nkeep ${i}\n`).join('') + padding
  const intended = base.replace(/line (\d+)\n/g, 'changed $1\n')
  const result = computeDescriptionPatch(base, intended)
  assert.equal(result.ops, null)
  assert.match(result.reason, /max|many|limit/)
})

test('preserves multibyte content and CRLF bytes', () => {
  const base = '## 計画\r\n- café ✅\r\n' + padding.replaceAll('\n', '\r\n')
  roundTrip(base, base.replace('café ✅', 'café ✅ — 完了'))
})

test('grows an insertion anchor across repeated initial blank lines', () => {
  const base = '\n\n' + padding
  const intended = '\nnew note\n' + base.slice(1)
  const { ops, reason } = computeDescriptionPatch(base, intended)
  if (ops) assert.equal(applyDescriptionPatch(base, ops), intended)
  else assert.ok(reason)
})

test('simulation rejects missing, repeated and stale anchors atomically', () => {
  assert.throws(() =>
    applyDescriptionPatch('same same', [{ op: 'insert_after', anchor: 'same', text: 'x' }]),
  )
  assert.throws(() =>
    applyDescriptionPatch('body', [{ op: 'replace', old_string: 'absent', new_string: 'x' }]),
  )
  assert.throws(() =>
    applyDescriptionPatch('body', [
      { op: 'replace', old_string: 'body', new_string: 'new' },
      { op: 'insert_before', anchor: 'body', text: 'x' },
    ]),
  )
})

const references = [
  '[ISSUE-123](https://tracker.example/issue/123)',
  '![image](https://tracker.example/image.png)',
  '<issue id="abc" />',
  '<https://tracker.example/123>',
  'https://tracker.example/123',
  'ISSUE-123',
  'issue-123',
  '#4138',
  '@someone',
]

function assertReferenceFree(anchor) {
  const masked = anchor.replace(/(`+)[^\n]*?\1/g, '')
  for (const reference of references) assert.ok(!masked.includes(reference), reference)
  for (const pattern of REFERENCE_PATTERNS) assert.doesNotMatch(masked, new RegExp(pattern.source))
}

test('clips past a link to a unique reference-free insertion anchor', () => {
  const base = '## Evidence\n- fixed [ISSUE-123](https://tracker.example/issue/123)\n' + padding
  const intended = base.replace('\n## Original notes', '\n- new evidence\n## Original notes')
  roundTrip(base, intended)
})

test('falls back when links fence both sides of an insertion', () => {
  const base = '- [A-1](u1)\n- [A-2](u2)\n- [A-3](u3)\n' + padding
  const intended = base.replace('- [A-2]', '- added\n- [A-2]')
  assert.deepEqual(computeDescriptionPatch(base, intended), { ops: null, reason: 'link-anchor' })
})

for (const reference of references) {
  test(`anchors never span ${reference}`, () => {
    const base = `- ${reference}\n- ${reference}\n` + padding
    const intended = `- ${reference}\n- added\n- ${reference}\n` + padding
    const result = computeDescriptionPatch(base, intended)
    if (result.ops) roundTrip(base, intended)
    else assert.equal(result.reason, 'link-anchor')
  })
}

test('changing a reference label falls back rather than matching partial reference text', () => {
  const base = '- [ISSUE-123](https://tracker.example/issue/123)\n' + padding
  assert.deepEqual(computeDescriptionPatch(base, base.replace('ISSUE-123', 'ISSUE-456')), {
    ops: null,
    reason: 'link-anchor',
  })
})

test('code-spanned attachment references remain usable as insertion anchors', () => {
  const attachment = '- Plan attachment: `Implementation plan (ISSUE-123)`\n'
  const base = '## Planning\n' + attachment + padding
  const ops = roundTrip(base, base.replace(attachment, attachment + '- Dependencies: none\n'))
  assert.equal(ops[0].op, 'insert_after')
  assert.equal(ops[0].anchor, attachment)
})

test('first and second planning saves still patch descriptions with linked notes', () => {
  const original = '## Reporter notes\n- [ISSUE-123](https://tracker.example/issue/123)\n' + padding
  const planned = '## Planning\n- Plan attachment: `Implementation plan (ISSUE-123)`\n\n' + original
  roundTrip(original, planned)
  roundTrip(planned, planned.replace('\n\n## Reporter', '\n- Dependencies: none\n\n## Reporter'))
})

test('clipped unique punctuation is a valid anchor between two link lines', () => {
  const base = '- [A-1](u1)\n- [A-2](u2)\n' + padding
  roundTrip(base, base.replace('- [A-2]', '- added\n- [A-2]'))
})

test('multibyte text before a reference preserves hazard offsets', () => {
  const base = '## 計画 ✅\n- [ISSUE-123](https://tracker.example/issue/123)\n' + padding
  assert.equal(
    computeDescriptionPatch(base, base.replace('ISSUE-123', 'ISSUE-456')).reason,
    'link-anchor',
  )
})

test('references within fenced code remain usable when context grows across the fence', () => {
  for (const fence of ['```', '~~~']) {
    const base = `${fence}text\nISSUE-123\nrepeat\n${fence}\nrepeat\n` + padding
    const intended = base.replace('repeat\n' + fence, 'changed\n' + fence)
    const { ops } = computeDescriptionPatch(base, intended)
    assert.ok(ops)
    assert.equal(applyDescriptionPatch(base, ops), intended)
  }
})

test('unequal inline delimiters do not hide reference hazards', () => {
  const base = '- ``literal ISSUE-123 ```\n' + padding
  assert.equal(
    computeDescriptionPatch(base, base.replace('ISSUE-123', 'ISSUE-456')).reason,
    'link-anchor',
  )
})

test('a longer closing fence still protects literal references from rewriting', () => {
  const base = '```text\nISSUE-123\n````\n' + padding
  const intended = base.replace('ISSUE-123', 'ISSUE-456')
  const { ops } = computeDescriptionPatch(base, intended)
  assert.ok(ops)
  assert.equal(applyDescriptionPatch(base, ops), intended)
})
