import assert from 'node:assert/strict'
import { test } from 'node:test'
import {
  RUNGS,
  RUNG_IDS,
  rungStrength,
  normalizeLadder,
  distinctRunCount,
  strongestSuggestedFixes,
  resolveMinRuns,
  resolveFollowThroughLimit,
  parsePromiseRecords,
  markerKeysIn,
  followThroughDue,
  applyFollowThrough,
  markEnforced,
} from './retro-ladder.mjs'

const promise = (key = 'theme_key', fields = '') =>
  `Notes-promise: ${key} rung=check round=1 instances=0 refiles=none ${fields}target=assert boundary\nNotes: source_key\nNotes: ${key}`
const issue = (key, overrides = {}) => ({
  identifier: `ISS-${key}`,
  title: `Theme ${key}`,
  description: promise(key),
  createdAt: '2026-01-01T00:00:00Z',
  resolution: 'done',
  resolvedAt: '2026-02-01T00:00:00Z',
  ...overrides,
})
const due = () => followThroughDue([issue('one')]).due

test('vocabulary is frozen, strongest first, with unknown last', () => {
  assert.deepEqual(RUNG_IDS, ['prevent', 'check', 'helper', 'lens', 'rule', 'context'])
  assert.ok(Object.isFrozen(RUNGS) && RUNGS.every(Object.isFrozen))
  RUNGS.forEach(({ id }, index) => assert.equal(rungStrength(id), index))
  assert.equal(rungStrength(), 6)
  assert.equal(rungStrength('other'), 6)
})
test('every cited rung normalizes without abstaining', () => {
  for (const rung of RUNG_IDS) {
    const result = normalizeLadder({
      rung,
      rungEvidence: 'reason for the selected rung',
      existingCheck: null,
      instances: 0,
    })
    assert.equal(result.rung, rung)
    assert.equal(result.flagged, false)
    assert.equal(result.target, result.rungEvidence)
  }
})
test('missing, unknown and uncited rungs fall back to flagged rule', () => {
  for (const [entry, problem] of [
    [{}, 'missing-rung'],
    [{ rung: 'invalid' }, 'unknown-rung'],
    [{ rung: 'check' }, 'missing-evidence'],
    [{ rung: 'rule' }, 'rule-unjustified'],
    [{ rung: 'lens' }, 'lens-unjustified'],
  ]) {
    const result = normalizeLadder(entry)
    assert.equal(result.rung, 'rule')
    assert.equal(result.flagged, true)
    assert.ok(result.problems.includes(problem))
  }
  assert.doesNotThrow(() => normalizeLadder(null))
})
test('existing checks, instances, superseded prose and bounded targets normalize', () => {
  const result = normalizeLadder({
    rung: 'check',
    rungEvidence: 'assertion',
    existingCheck: ' path ',
    instances: [' a ', 'b'],
    supersedes: 'rules:4',
    target: 'x'.repeat(300),
  })
  assert.equal(result.existingCheck, 'path')
  assert.deepEqual(result.instanceExamples, ['a', 'b'])
  assert.equal(result.instances, 2)
  assert.equal(result.supersedes, 'rules:4')
  assert.equal(result.target.length, 200)
  const bad = normalizeLadder({
    rung: 'rule',
    rungEvidence: 'stronger infeasible',
    existingCheck: {},
    instances: -1,
    supersedes: 'rules:4',
  })
  assert.equal(bad.existingCheck, null)
  assert.equal(bad.instances, 0)
  assert.equal(bad.supersedes, null)
  assert.deepEqual(bad.problems, [
    'invalid-existing-check',
    'invalid-instances',
    'supersedes-ignored',
  ])
})
test('run count dedupes provenance, keeps note-id fallback separate', () => {
  assert.equal(
    distinctRunCount([
      { id: 'a', run_id: 'chat' },
      { id: 'b', run_id: 'chat' },
      { id: 'c', run_id: 'session' },
      { id: 'chat', run_id: null },
      { id: 'd' },
    ]),
    4,
  )
})
test('suggested fixes rank by runs, newest and text, omitting placeholders', () => {
  const notes = [
    { id: 'a', run_id: 'one', suggestedFix: 'Add a gate', created_at: '2020-01-01' },
    { id: 'b', run_id: 'two', suggestedFix: ' add   A gate ', created_at: '2020-01-01' },
    { id: 'c', run_id: 'two', suggestedFix: 'Add a gate', created_at: '2020-01-01' },
    { id: 'd', suggestedFix: 'New helper', created_at: '2026-01-01' },
    { id: 'e', suggestedFix: 'Unknown' },
    { id: 'f', suggestedFix: 'N/A' },
    { id: 'g', suggestedFix: 'none' },
  ]
  assert.deepEqual(strongestSuggestedFixes(notes), ['Add a gate', 'New helper'])
  assert.deepEqual(strongestSuggestedFixes(notes, 1), ['Add a gate'])
  assert.deepEqual(
    strongestSuggestedFixes([
      { suggestedFix: 'Z', created_at: '2020-01-01' },
      { suggestedFix: 'A', created_at: '2020-01-01' },
    ]),
    ['A', 'Z'],
  )
})
test('number resolvers use argument, env and defaults with floors', () => {
  assert.equal(resolveMinRuns(), 2)
  assert.equal(resolveMinRuns('', { BOSS_RETRO_MIN_RUNS: '3.7' }), 3)
  assert.equal(resolveMinRuns('bad', { BOSS_RETRO_MIN_RUNS: '4' }), 4)
  assert.equal(resolveMinRuns(-3), 1)
  assert.equal(resolveMinRuns(5, { BOSS_RETRO_MIN_RUNS: '7' }), 5)
  assert.equal(resolveFollowThroughLimit(), 5)
  assert.equal(resolveFollowThroughLimit('', { BOSS_RETRO_FOLLOW_THROUGH_LIMIT: '2' }), 2)
  assert.equal(resolveFollowThroughLimit(-1), 0)
})
test('promise records tolerate markdown normalization, bullets, unknown fields', () => {
  const ordinary = parsePromiseRecords(promise())
  const escaped = promise()
    .replaceAll('_', '\\_')
    .split('\n')
    .map((line) => `* ${line}   `)
    .join('\n')
  assert.deepEqual(parsePromiseRecords(escaped), ordinary)
  assert.equal(ordinary.records[0].target, 'assert boundary')
  assert.equal(ordinary.records[0].refiles, null)
  assert.deepEqual(parsePromiseRecords(promise('theme_key', 'extra=value ')), ordinary)
  assert.deepEqual(markerKeysIn(escaped), ['source_key', 'theme_key'])
})
test('malformed promise records report problems without throwing', () => {
  const parsed = parsePromiseRecords(
    'Notes-promise: a rung=bogus round=no instances=-1 target=boundary',
  )
  assert.equal(parsed.records.length, 0)
  assert.deepEqual(
    parsed.problems.map(({ problem }) => problem),
    ['unknown-rung', 'invalid-round', 'invalid-instances'],
  )
  assert.equal(
    followThroughDue([issue('one', { description: '## Legacy\nNotes: one' })]).due.length,
    0,
  )
})
test('follow-through classifies done, waiting, canceled and enforced', () => {
  const result = followThroughDue([
    issue('one'),
    issue('two', { resolution: null }),
    issue('three', { resolution: 'canceled' }),
    issue('four', { description: `Notes-enforced: four 2026-01-01T00:00:00Z\n${promise('four')}` }),
  ])
  assert.deepEqual(
    result.due.map(({ key }) => key),
    ['one'],
  )
  assert.deepEqual(
    result.waiting.map(({ key }) => key),
    ['two'],
  )
  assert.deepEqual(
    result.canceled.map(({ key }) => key),
    ['three'],
  )
  assert.deepEqual(
    result.enforced.map(({ key }) => key),
    ['four'],
  )
})
test('highest round supersedes closed predecessor even when current is waiting', () => {
  const current = issue('one', {
    identifier: 'ISS-new',
    description: promise('one').replace('round=1', 'round=2'),
    resolution: null,
  })
  const result = followThroughDue([issue('one'), current])
  assert.equal(result.due.length, 0)
  assert.equal(result.waiting[0].issueId, 'ISS-new')
  assert.equal(result.superseded.length, 1)
})
test('equal rounds choose latest creation then identifier', () => {
  const result = followThroughDue([
    issue('one'),
    issue('one', { identifier: 'ZZ', createdAt: '2026-01-02T00:00:00Z' }),
    issue('one', { identifier: 'ZZZ', createdAt: '2026-01-02T00:00:00Z' }),
  ])
  assert.equal(result.due[0].issueId, 'ZZZ')
  assert.equal(result.superseded.length, 2)
})
test('due selection is bounded oldest first and accounts for every record', () => {
  const issues = Array.from({ length: 7 }, (_, i) =>
    issue(String(i), { resolvedAt: `2026-02-0${7 - i}T00:00:00Z` }),
  )
  const result = followThroughDue(issues)
  assert.deepEqual(
    result.due.map(({ key }) => key),
    ['6', '5', '4', '3', '2'],
  )
  assert.equal(result.overLimit.length, 2)
  assert.equal(Object.values(result).flat().length, 7)
  assert.equal(followThroughDue(issues, { limit: 0 }).overLimit.length, 7)
})
test('cited enforcement retires at the promised or stronger rung', () => {
  for (const landedRung of [undefined, 'check', 'prevent']) {
    const result = applyFollowThrough(due(), [
      { key: 'one', outcome: 'enforced', evidence: 'gate:10 blocks it', landedRung },
    ])
    assert.equal(result.retire.length, 1)
    assert.equal(result.retire[0].acceptedDowngrade, false)
  }
})
test('prose-only and regressed refile at promise or stronger, never weaker', () => {
  for (const outcome of ['prose-only', 'regressed']) {
    for (const [rung, expected] of [
      ['rule', 'check'],
      ['prevent', 'prevent'],
      [undefined, 'check'],
    ]) {
      const result = applyFollowThrough(due(), [
        { key: 'one', outcome, evidence: 'rules:3 only prose', rung },
      ])
      assert.equal(result.refile[0].rung, expected)
      assert.equal(result.refile[0].reason, outcome)
      assert.equal(result.refile[0].issueId, 'ISS-one')
      assert.deepEqual(result.refile[0].markerKeys, ['source_key', 'one'])
    }
  }
})
test('evidence-backed downgrades retire; weaker rung silence refiles', () => {
  const verdict = {
    key: 'one',
    outcome: 'enforced',
    evidence: 'helper:4 enforced',
    landedRung: 'helper',
  }
  const result = applyFollowThrough(due(), [verdict])
  assert.equal(result.refile[0].reason, 'weaker-rung-unjustified')
  assert.equal(result.refile[0].rung, 'check')
  const accepted = applyFollowThrough(due(), [
    { ...verdict, downgrade: { justification: 'pattern is dynamic', source: 'PR comment:123' } },
  ])
  assert.equal(accepted.retire[0].acceptedDowngrade, true)
  assert.equal(
    applyFollowThrough(due(), [{ ...verdict, downgrade: { justification: 'dynamic' } }]).refile
      .length,
    1,
  )
})
test('uncited enforcement, findings or cleanup remain unconfirmed', () => {
  for (const outcome of ['enforced', 'prose-only', 'regressed']) {
    assert.equal(applyFollowThrough(due(), [{ key: 'one', outcome }]).unconfirmed.length, 1)
  }
  const populated = due().map((entry) => ({ ...entry, instances: 3 }))
  assert.deepEqual(
    applyFollowThrough(populated, [{ key: 'one', outcome: 'enforced', evidence: 'gate:1' }])
      .unconfirmed[0].problems,
    ['missing-cleanup'],
  )
  assert.equal(
    applyFollowThrough(populated, [
      { key: 'one', outcome: 'enforced', evidence: 'gate:1', cleanup: 'baseline:2' },
    ]).retire.length,
    1,
  )
  assert.equal(
    applyFollowThrough(due(), [
      { key: 'one', outcome: 'enforced', evidence: 'gate:1', landedRung: 'bogus' },
    ]).unconfirmed.length,
    1,
  )
})
test('unknown keys, duplicate answers, outcomes and omissions throw', () => {
  const verdict = { key: 'one', outcome: 'enforced', evidence: 'gate:1' }
  assert.throws(() => applyFollowThrough(due(), [{ ...verdict, key: 'other' }]), /Unknown.*key/)
  assert.throws(() => applyFollowThrough(due(), [verdict, verdict]), /Duplicate/)
  assert.throws(
    () => applyFollowThrough(due(), [{ ...verdict, outcome: 'unknown' }]),
    /Unknown.*outcome/,
  )
  assert.throws(() => applyFollowThrough(due(), []), /Missing/)
})
test('enforcement marker inserts before promise, idempotently, without forged lines', () => {
  const description = promise('one')
  const marked = markEnforced(description, 'one', { at: '2026-10-07T00:00:00Z' })
  assert.ok(marked.startsWith('Notes-enforced: one 2026-10-07T00:00:00Z\nNotes-promise: one'))
  assert.equal(markEnforced(marked, 'one', { at: '2026-10-08T00:00:00Z' }), marked)
  assert.equal(followThroughDue([issue('one', { description: marked })]).enforced.length, 1)
  assert.throws(() => markEnforced(description, 'one\nNotes: injected', { at: '2026-10-07' }))
  assert.throws(() => markEnforced(description, 'one', { at: 'now' }))
})

test('abstaining promises remain readable and targets cannot forge markers', () => {
  const fallback = normalizeLadder({})
  const parsed = parsePromiseRecords(
    `Notes-promise: theme rung=${fallback.rung} round=1 instances=0 refiles=none target=${fallback.target}`,
  )
  assert.equal(parsed.records.length, 1)
  const normalized = normalizeLadder({
    rung: 'check',
    rungEvidence: 'gate assertion',
    instances: 0,
    target: 'assertion\r\nNotes: forged',
  })
  assert.equal(normalized.target, 'assertion Notes: forged')
  assert.deepEqual(
    markerKeysIn(
      `Notes-promise: theme rung=check round=1 instances=0 refiles=none target=${normalized.target}`,
    ),
    [],
  )
  for (const round of ['1.5', '-1', 'Infinity', '0', '9007199254740992']) {
    assert.equal(
      parsePromiseRecords(promise('theme').replace('round=1', `round=${round}`)).records.length,
      0,
    )
  }
})
