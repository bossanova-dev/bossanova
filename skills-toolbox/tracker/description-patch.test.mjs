import { test } from 'node:test'
import assert from 'node:assert/strict'
import { computeDescriptionPatch, applyDescriptionPatch } from './description-patch.mjs'

const padding = '\n## Original notes\n' + 'unchanged source notes with details\n'.repeat(40)

function roundTrip(base, intended) {
  const { ops } = computeDescriptionPatch(base, intended)
  assert.ok(ops)
  assert.equal(applyDescriptionPatch(base, ops), intended)
  for (const op of ops) {
    const anchor = op.anchor ?? op.old_string
    assert.equal(base.indexOf(anchor), base.lastIndexOf(anchor))
    assert.ok(anchor.length)
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
