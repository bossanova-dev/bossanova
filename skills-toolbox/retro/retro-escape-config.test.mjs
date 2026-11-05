import test from 'node:test'
import assert from 'node:assert/strict'
import { escapeRecord } from '../verify/verify-escapes.mjs'
import { escapeRows } from './retro-outcomes.mjs'
import { reviewPolicyFromBlock } from '../verify/verify-review-policy.mjs'
import { verifyPolicyFromConfigText } from '../skill-config.mjs'
import { isCronFireBranch, parseTicketLine } from '../verify/verify-admission.mjs'
import { proposeAlwaysReview, applyProposal, escapeBranch, prBody } from './retro-escape-config.mjs'

const window = { start: '2026-10-01T00:00:00Z', end: '2026-11-01T00:00:00Z' }
function row(paths, fixPaths = paths) {
  const original = { number: 1, title: 'Original', mergedAt: window.start, files: paths }
  const fix = { number: 2, title: 'Fix', mergedAt: '2026-10-02T00:00:00Z', files: fixPaths }
  return escapeRows(
    [
      escapeRecord({
        repo: 'owner/repo',
        fix,
        link: { original, basis: 'explicit-reference', evidence: 'Fixes #1' },
        tracedAt: window.end,
      }),
    ],
    window,
  ).rows[0]
}
const propose = (paths, block = {}) =>
  proposeAlwaysReview([row(paths)], reviewPolicyFromBlock(block))

test('shared directories collapse, root files stay exact and paths sort deterministically', () => {
  const paths = ['services/a/sub/y.go', 'main.go', 'services/a/x.go']
  const result = propose(paths)
  assert.deepEqual(result.proposals[0].globs, ['main.go', 'services/a/**'])
  assert.deepEqual(result, propose([...paths].reverse()))
  assert.equal(result.proposals[0].name, 'Escaped: Other changes')
  assert.deepEqual(result.proposals[0].paths, [...paths].sort())
})

test('skip reasons reflect overlap, generated content, configured coverage and built-in asks', () => {
  assert.equal(
    proposeAlwaysReview([row(['a.go'], ['b.go'])], reviewPolicyFromBlock({})).skipped[0].reason,
    'no-paths',
  )
  assert.equal(propose([]).skipped[0].reason, 'no-paths')
  assert.equal(propose(['a/x.pb.go']).skipped[0].reason, 'generated')
  assert.equal(
    propose(['a/x.go'], { alwaysReview: { Existing: ['a/**'] } }).skipped[0].reason,
    'covered',
  )
  assert.equal(propose(['db/migrations/x.sql']).skipped[0].reason, 'already-asks')
  assert.equal(
    propose(['a/x.go', 'db/x.sql'], { alwaysReview: { Existing: ['a/**'] } }).skipped[0].reason,
    'already-asks',
  )
  assert.equal(propose(['a/x.go'], { neverReview: ['a/**'] }).proposals.length, 1)
})

test('mixed rows retain uncovered test/docs paths and name their first remaining area', () => {
  const result = propose(['db/x.sql', 'a/x.pb.go', 'ui/a.tsx', 'a/x.test.mjs', 'docs/x.md'], {
    alwaysReview: { UI: ['ui/**'] },
  })
  assert.deepEqual(result.proposals[0].paths, ['a/x.test.mjs', 'docs/x.md'])
  assert.equal(result.proposals[0].name, 'Escaped: Housekeeping')
})

test('config splice preserves unrelated text and validates every insertion depth', () => {
  const proposal = propose(['a/x.go']).proposals[0]
  for (const input of [
    null,
    '{}\n',
    '{\n  "commands": { "test": "test" }\n}\n',
    '{\n  "verifyDefaults": {\n    "evidence": true\n  }\n}\n',
    '{\n  "verifyDefaults": {\n    "alwaysReview": {\n      "Other": ["z/**"]\n    }\n  }\n}\n',
    '{\n  "verifyDefaults": {\n    "alwaysReview": {\n      "Escaped: Other changes": ["z/**"]\n    }\n  }\n}\n',
  ]) {
    const original = JSON.parse(input ?? '{}')
    const expected = structuredClone(original)
    expected.verifyDefaults ??= {}
    expected.verifyDefaults.alwaysReview ??= {}
    expected.verifyDefaults.alwaysReview[proposal.name] = [
      ...new Set([
        ...(expected.verifyDefaults.alwaysReview[proposal.name] ?? []),
        ...proposal.globs,
      ]),
    ]
    const result = applyProposal(input, proposal)
    assert.equal(typeof result, 'string')
    assert.deepEqual(JSON.parse(result), expected)
    assert.doesNotThrow(() => verifyPolicyFromConfigText(result, 'test'))
    if (input?.includes('"commands"'))
      assert.ok(result.includes('  "commands": { "test": "test" }'))
    if (input?.includes('"evidence"')) assert.ok(result.includes('    "evidence": true'))
    if (input?.includes('"Other"')) assert.ok(result.includes('      "Other": ["z/**"]'))
    assert.deepEqual(applyProposal(result, proposal), { unchanged: true })
  }
})

test('scanner handles escaped keys, nested strings and structural mismatch fallback', () => {
  const proposal = { ...propose(['a/x.go']).proposals[0], name: 'Escaped: "quoted"' }
  const input =
    '{"commands":{"test":"brace } and \\\" quote"},"verifyDefaults":{"alwaysReview":{}}}\n'
  const result = applyProposal(input, proposal)
  assert.equal(typeof result, 'string')
  assert.deepEqual(JSON.parse(result).verifyDefaults.alwaysReview[proposal.name], ['a/**'])
  const duplicate = '{"verifyDefaults":{},"verifyDefaults":{"alwaysReview":{"Kept":["z/**"]}}}'
  const fallback = applyProposal(duplicate, proposal)
  assert.equal(typeof fallback, 'string')
  assert.deepEqual(JSON.parse(fallback).verifyDefaults.alwaysReview, {
    Kept: ['z/**'],
    [proposal.name]: ['a/**'],
  })
  assert.equal(fallback.split('"verifyDefaults"').length, 2)
})

test('invalid config/proposal opens nothing and generated PR cannot enter verify candidates', () => {
  const proposal = propose(['a/x.go']).proposals[0]
  for (const input of ['no json', '[]', '{"verifyDefaults":false}'])
    assert.deepEqual(applyProposal(input, proposal), { error: 'invalid-config' })
  assert.deepEqual(applyProposal('{}', { ...proposal, name: 'bad\nname' }), {
    error: 'invalid-config',
  })
  assert.equal(escapeBranch(row(['a/x.go'])), 'boss-retro/escape-1-2')
  assert.equal(isCronFireBranch(proposal.branch), false)
  const body = prBody(proposal)
  assert.equal(parseTicketLine(body), null)
  assert.ok(body.includes('#1') && body.includes('#2'))
})
