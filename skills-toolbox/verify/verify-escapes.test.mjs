import test from 'node:test'
import assert from 'node:assert/strict'
import {
  linkFix,
  escapeRecord,
  renderEscapeComment,
  parseEscapeComment,
  escapeSignals,
  weakCandidates,
} from './verify-escapes.mjs'
import { renderPrSection } from '../review-verdict.mjs'

const original = {
  number: 10,
  repo: 'a/b',
  title: 'feature',
  mergedAt: '2026-10-01T00:00:00Z',
  baseRefName: 'main',
  files: ['src/a.js'],
  mergeHeadSha: 'abc',
}
const fix = { ...original, number: 11, title: 'fix: repair', mergedAt: '2026-10-02T00:00:00Z' }
const links = (body, more = {}) =>
  linkFix({ fix: { ...fix, body, ...more }, originals: [original], tickets: {}, ticketPrs: {} })
test('binding references link; bare mentions and ineligible originals do not', () => {
  for (const body of ['Fixes #10.', 'regression introduced by #10.', '(added in #10)'])
    assert.equal(links(body).length, 1, body)
  for (const body of ['see #10', 'follow-up to #10', 'builds on #10'])
    assert.equal(links(body).length, 0)
  assert.equal(links('(added in #10)', { title: 'feat: another feature' }).length, 0)
  for (const patch of [
    { number: 11 },
    { mergedAt: null },
    { mergedAt: '2026-10-03' },
    { baseRefName: 'release' },
    { repo: 'c/d' },
  ])
    assert.equal(
      linkFix({ fix: { ...fix, body: 'fixes #10' }, originals: [{ ...original, ...patch }] })
        .length,
      0,
    )
})
test('follow-up findings resolve direct PRs and uniquely resolved ticket keys', () => {
  const args = {
    fix,
    originals: [original],
    tickets: { 'TASK-2': 'Found verified P2 in TASK-1 independent review.' },
    ticketPrs: { 'TASK-1': [original] },
  }
  assert.deepEqual(
    linkFix(args).map(({ original, basis, ticket, severity }) => [
      original.number,
      basis,
      ticket,
      severity,
    ]),
    [[10, 'followup-ticket', 'TASK-2', 'P2']],
  )
  assert.equal(linkFix({ ...args, ticketPrs: { 'TASK-1': [original, original] } }).length, 0)
  assert.equal(
    linkFix({ ...args, tickets: { 'TASK-2': 'Track separately from TASK-1.' } }).length,
    0,
  )
})
test('revert wins precedence and bare overlap is only a candidate', () => {
  assert.equal(links('Reverts a/b#10. Fixes #10.')[0].basis, 'revert')
  assert.equal(
    weakCandidates({ fix: { ...fix, body: 'see #10' }, originals: [original] }).length,
    1,
  )
})
test('records roundtrip, reject damaged payloads and drop secrets', () => {
  const record = escapeRecord({
    repo: 'a/b',
    fix,
    link: links('fixes #10')[0],
    tracedAt: '2026-10-03T00:00:00Z',
  })
  assert.deepEqual(parseEscapeComment(renderEscapeComment(record)), record)
  assert.equal(parseEscapeComment(renderEscapeComment(record).replace('"v":1', '"v":2')), null)
  assert.equal(parseEscapeComment(renderEscapeComment(record).slice(0, -10)), null)
  assert.equal(parseEscapeComment(renderEscapeComment({ ...record, areas: [{}] })), null)
  assert.equal(parseEscapeComment(renderEscapeComment({ ...record, tracedAt: 'invalid' })), null)
  assert.equal(record.areaBasis, 'overlap')
  assert.equal(record.levelAtMerge, null)
  assert.equal(
    escapeRecord({
      repo: 'a/b',
      fix,
      link: {
        ...links('fixes #10')[0],
        evidence: 'token ghp_abcdefghijklmnopqrstuvwxyz1234567890abcd',
      },
      tracedAt: record.tracedAt,
    }).evidence,
    '',
  )
})
test('signals parse the review renderer, size and merge-head status', () => {
  const finding = {
    id: 'R1',
    severity: 'P2',
    introduced: true,
    verified: true,
    applied: false,
    file: 'a',
    line: 1,
    title: 'defect',
    evidence: 'proof',
  }
  const body = renderPrSection({
    status: 'clean',
    verdict: { findings: [finding], secondModel: { status: 'skipped', reason: 'unavailable' } },
  })
  const signals = escapeSignals({
    ...original,
    body,
    additions: 2,
    deletions: 1,
    changedFiles: 1,
    verifyState: 'success',
  })
  assert.equal(signals.findings.unapplied, 1)
  assert.equal(signals.findings.maxSeverity, 'P2')
  assert.equal(signals.secondModel, 'skipped')
  assert.equal(signals.verify, 'success')
})

test('October history precision pins confusion counts and identifies false-positive patterns', async (t) => {
  const { readFileSync } = await import('node:fs')
  const fixture = JSON.parse(
    readFileSync(
      new URL('../../scripts/fixtures/verify-escapes/2026-10.json', import.meta.url),
      'utf8',
    ),
  )
  const measured = { tp: 0, fp: 0, fn: 0, tn: 0 }
  for (const row of fixture.rows) {
    const result = linkFix(row)
    const predicted = result.length > 0
    measured[row.label ? (predicted ? 'tp' : 'fn') : predicted ? 'fp' : 'tn']++
    if (predicted && !row.label)
      assert.fail(
        `false positive ${row.fix.number}: ${result.map((l) => l.pattern ?? l.basis).join(',')}`,
      )
  }
  assert.deepEqual(measured, fixture.expected)
  const precision = measured.tp / (measured.tp + measured.fp)
  t.diagnostic(
    `October confusion ${JSON.stringify(measured)}; precision ${precision}; recall against retro labels unavailable`,
  )
  assert.ok(precision >= 0.9)
  for (const [fix, original, basis] of [
    [3668, 3647, 'explicit-reference'],
    [3759, 3752, 'followup-ticket'],
    [3760, 3714, 'followup-ticket'],
  ]) {
    const row = fixture.rows.find((r) => r.fix.number === fix)
    assert.deepEqual(
      linkFix(row).map((l) => [l.original.number, l.basis]),
      [[original, basis]],
    )
  }
})

test('separate fixes retain distinct markers; areas fall back to all original paths', () => {
  const first = escapeRecord({
    repo: 'a/b',
    fix,
    link: links('fixes #10')[0],
    tracedAt: '2026-10-03',
  })
  const second = escapeRecord({
    repo: 'a/b',
    fix: { ...fix, number: 12, files: ['unrelated'] },
    link: links('fixes #10')[0],
    tracedAt: '2026-10-03',
  })
  assert.notEqual(renderEscapeComment(first), renderEscapeComment(second))
  assert.equal(new Set([first, second].map((r) => r.original.number)).size, 1)
  assert.equal(second.areaBasis, 'original')
  assert.deepEqual(second.areas, [{ area: 'other', paths: ['src/a.js'] }])
})
test('renderer findings count blocking and unapplied items; absent second model is unknown', () => {
  const f = {
    id: 'R1',
    severity: 'P1',
    introduced: true,
    verified: true,
    applied: false,
    file: 'a',
    line: 1,
    title: 'defect',
    evidence: 'proof',
  }
  const body = renderPrSection({
    status: 'blocked',
    verdict: {
      findings: [f, { ...f, id: 'R2', severity: 'P2' }, { ...f, id: 'R3', severity: 'P3' }],
      secondModel: { ran: true, agent: 'independent' },
    },
  })
  const signals = escapeSignals({ ...original, body })
  assert.deepEqual(signals.findings, { blocking: 1, unapplied: 1, maxSeverity: 'P1' })
  assert.equal(signals.secondModel, 'ran')
  assert.equal(escapeSignals(original).secondModel, 'unknown')
  assert.equal(escapeSignals(original).verify, 'absent')
})

test('follow-up feature prose, lifecycle states and tool names do not imply findings', () => {
  for (const description of [
    'BOS-1439 collects human review comments on agent PRs.',
    'File overlap: TASK-1 (skills/boss-review/SKILL.md).',
    'TASK-1 is now In Review.',
    'The boss-verify merge and #10 completion path pass the verified head.',
    'Follow-up to PR #10, which adds analytics.',
  ])
    assert.equal(
      linkFix({
        fix,
        originals: [original],
        tickets: { 'TASK-2': description },
        ticketPrs: { 'TASK-1': [original], 'BOS-1439': [original] },
      }).length,
      0,
      description,
    )
})

test('weak candidate output drops secret-shaped evidence', () => {
  assert.equal(
    weakCandidates({
      fix: { ...fix, body: 'see #10 token ghp_abcdefghijklmnopqrstuvwxyz1234567890abcd' },
      originals: [original],
    })[0].evidence,
    '',
  )
})

test('one binding can repair coordinated PRs while a later bare mention stays unrelated', () => {
  const originals = [original, { ...original, number: 9 }, { ...original, number: 8 }]
  assert.deepEqual(
    linkFix({ fix: { ...fix, body: 'Fixes #10 and #9; see #8.' }, originals }).map(
      (l) => l.original.number,
    ),
    [9, 10],
  )
})

test('finding ordinals are not PR references even when their number is a merged PR', () => {
  const originals = [original, { ...original, number: 2 }]
  assert.deepEqual(
    linkFix({
      fix,
      originals,
      tickets: { 'TASK-2': 'Verified P2 finding #2 in the independent review of PR #10.' },
    }).map((l) => l.original.number),
    [10],
  )
})

test('a fix describing an example affected run does not blame the example PR', () => {
  assert.equal(
    links('Fixes verify passing runs that captured an empty TUI home (for example #10).').length,
    0,
  )
})
