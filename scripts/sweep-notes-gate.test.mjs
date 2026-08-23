import assert from 'node:assert/strict'
import { test } from 'node:test'

import {
  DEFAULT_CAP,
  DEFAULT_STALE_DAYS,
  KEY_DIGEST_LENGTH,
  MARKED_ISSUES_QUERY,
  MAX_SLUG_SEGMENT,
  MAX_TITLE_LENGTH,
  applyVerdicts,
  clusterNotes,
  fetchMarkedLinearIssues,
  mergeClusters,
  parseNote,
  rankClusters,
  renderClusterMarkers,
  resolveCap,
  resolveStaleDays,
  retiredNoteIds,
  runCli,
  selectClusters,
  stalenessSignals,
} from './sweep-notes-gate.mjs'

const note = (id, body) => ({ id, body, created_at: `2026-07-2${id}T00:00:00Z` })
const FRESH_SELECTION_NOW = Date.parse('2026-08-01T00:00:00Z')

test('parseNote extracts structured fields while retaining a free-form statement', () => {
  assert.deepEqual(
    parseNote(
      note(
        '1',
        'Opening a worktree fails after a stale lock.\nWhere: services/boss/cmd/worktree.go\nWhy it matters: Blocks recovery.\nSuggested fix: Reclaim expired locks.\nRun: make test-boss',
      ),
    ),
    {
      id: '1',
      body: 'Opening a worktree fails after a stale lock.\nWhere: services/boss/cmd/worktree.go\nWhy it matters: Blocks recovery.\nSuggested fix: Reclaim expired locks.\nRun: make test-boss',
      created_at: '2026-07-21T00:00:00Z',
      statement: 'Opening a worktree fails after a stale lock.',
      where: 'services/boss/cmd/worktree.go',
      whyItMatters: 'Blocks recovery.',
      suggestedFix: 'Reclaim expired locks.',
      run: 'make test-boss',
    },
  )
  assert.deepEqual(parseNote(note('2', 'A free-form observation')), {
    id: '2',
    body: 'A free-form observation',
    created_at: '2026-07-22T00:00:00Z',
    statement: 'A free-form observation',
    where: null,
    whyItMatters: null,
    suggestedFix: null,
    run: null,
  })
})

test('clusterNotes deterministically groups normalized statements and Where targets', () => {
  const notes = [
    note('3', 'Stale lock blocks worktree creation\nWhere: cmd/worktree.go'),
    note('1', '  stale   lock blocks worktree creation  \nWhere: cmd/worktree.go'),
    note('2', 'Stale lock blocks worktree creation\nWhere: cmd/other.go'),
  ]
  const clusters = clusterNotes(notes)
  assert.deepEqual(
    clusters.map(({ key, statement, where, notes: grouped }) => ({
      key,
      statement,
      where,
      ids: grouped.map(({ id }) => id),
    })),
    [
      {
        key: 'stale-lock-blocks-worktree-creation--cmd-other-go--f4c764221221258d',
        statement: 'Stale lock blocks worktree creation',
        where: 'cmd/other.go',
        ids: ['2'],
      },
      {
        key: 'stale-lock-blocks-worktree-creation--cmd-worktree-go--1e66775a53e1ac7c',
        statement: 'stale lock blocks worktree creation',
        where: 'cmd/worktree.go',
        ids: ['1', '3'],
      },
    ],
  )
})

test('clusterNotes presentation is independent of equivalent note input order', () => {
  const notes = [
    note('2', '  STALE   lock blocks worktree creation  \nWhere: CMD/worktree.go'),
    note('1', 'Stale lock blocks worktree creation\nWhere: cmd/worktree.go'),
  ]

  assert.deepEqual(clusterNotes(notes), clusterNotes(notes.toReversed()))
  assert.equal(clusterNotes(notes)[0].statement, 'Stale lock blocks worktree creation')
  assert.equal(clusterNotes(notes)[0].where, 'cmd/worktree.go')
})

test('clusterNotes gives colliding slugs distinct stable marker keys', () => {
  const first = clusterNotes([note('1', 'foo/bar\nWhere: a'), note('2', 'foo bar\nWhere: a')])
  const reversed = clusterNotes([note('2', 'foo bar\nWhere: a'), note('1', 'foo/bar\nWhere: a')])

  assert.equal(first.length, 2)
  assert.equal(new Set(first.map((cluster) => cluster.key)).size, 2)
  assert.deepEqual(
    first.map((cluster) => cluster.key),
    reversed.map((cluster) => cluster.key),
  )

  const marked = selectClusters(first, [{ description: `Notes: ${first[0].key}` }], {
    now: FRESH_SELECTION_NOW,
  })
  assert.deepEqual(
    marked.dropped.map((entry) => entry.cluster.key),
    [first[0].key],
  )
  assert.deepEqual(
    marked.selected.map((entry) => entry.cluster.key),
    [first[1].key],
  )
})

test('marker keys stay bounded however long the note body is', () => {
  // parseNote folds every non-field line into `statement`, so a note carrying
  // recurrence appendices used to produce a multi-kilobyte key: the marker block
  // became enormous and the pre-create Linear query unusable.
  const huge = `${'a very long recurrence appendix sentence. '.repeat(300)}\nWhere: ${'services/very/deep/path/'.repeat(40)}file.go`
  const clusters = clusterNotes([note('1', huge), note('2', 'short problem\nWhere: a.go')])

  assert.equal(clusters.length, 2)
  for (const cluster of clusters) {
    assert.ok(
      cluster.key.length <= 2 * MAX_SLUG_SEGMENT + KEY_DIGEST_LENGTH + 4,
      `key must stay bounded, got ${cluster.key.length}`,
    )
    assert.match(cluster.key, new RegExp(`--[0-9a-f]{${KEY_DIGEST_LENGTH}}$`))
  }

  // Two notes sharing a truncated slug prefix must still get distinct keys,
  // because identity lives in the digest rather than in the readable segments.
  const sharedPrefix = 'x'.repeat(MAX_SLUG_SEGMENT + 40)
  const twins = clusterNotes([
    note('1', `${sharedPrefix} alpha\nWhere: a.go`),
    note('2', `${sharedPrefix} beta\nWhere: a.go`),
  ])
  assert.equal(twins.length, 2)
  assert.notEqual(twins[0].key, twins[1].key)
  assert.equal(twins[0].key.split('--')[0], twins[1].key.split('--')[0])
})

test('mergeClusters deterministically applies a complete near-duplicate partition', () => {
  const clusters = clusterNotes([
    note('1', 'Stale lock blocks worktree creation\nWhere: cmd/worktree.go'),
    note('2', 'Worktree creation is blocked by stale locks\nWhere: cmd/worktree.go'),
    note('3', 'Unrelated problem\nWhere: cmd/other.go'),
  ])
  const related = clusters
    .filter((cluster) => cluster.where === 'cmd/worktree.go')
    .map((cluster) => cluster.key)
  const unrelated = clusters.find((cluster) => cluster.where === 'cmd/other.go').key
  const first = mergeClusters(clusters, [related.toReversed(), [unrelated]])
  const second = mergeClusters(clusters, [[unrelated], related])

  assert.deepEqual(first, second)
  assert.equal(first.length, 2)
  assert.deepEqual(
    first.find((cluster) => cluster.sourceKeys.length === 2).notes.map(({ id }) => id),
    ['1', '2'],
  )
  assert.throws(
    () => mergeClusters(clusters, [[related[0]], [unrelated]]),
    /account for every cluster/,
  )
})

test('mergeClusters merges across different Where targets and records each one', () => {
  const clusters = clusterNotes([
    note('1', 'Exit code lies\nWhere: cmd/worktree.go'),
    note('2', 'Exit status is the wrapper\nWhere: Makefile'),
    note('3', 'Status is the launcher shell\nWhere: cmd/worktree.go'),
  ])
  const merged = mergeClusters(clusters, [
    { title: 'Commands whose result lies', keys: clusters.map((cluster) => cluster.key) },
  ])

  assert.equal(merged.length, 1)
  assert.equal(merged[0].title, 'Commands whose result lies')
  assert.deepEqual(merged[0].wheres, ['cmd/worktree.go', 'Makefile'])
  assert.deepEqual(
    merged[0].notes.map(({ id }) => id),
    ['1', '2', '3'],
  )
  assert.equal(merged[0].sourceKeys.length, 3)
  assert.ok(merged[0].sourceKeys.includes(merged[0].key))
})

test('mergeClusters titles are order-independent and validated', () => {
  const clusters = clusterNotes([
    note('1', 'First problem\nWhere: a.go'),
    note('2', 'Second problem\nWhere: b.go'),
  ])
  const keys = clusters.map((cluster) => cluster.key)
  const forward = mergeClusters(clusters, [{ title: 'Shared theme', keys }])
  const reversed = mergeClusters(clusters.toReversed(), [
    { title: 'Shared theme', keys: keys.toReversed() },
  ])
  assert.deepEqual(forward, reversed)

  // An untitled legacy group still works and falls back to the member statement.
  const legacy = mergeClusters(clusters, [keys])
  assert.equal(legacy[0].title, legacy[0].statement)

  const bad = (title) => () => mergeClusters(clusters, [{ title, keys }])
  assert.throws(bad(''), /title must be non-empty/)
  assert.throws(bad('   '), /title must be non-empty/)
  assert.throws(bad(42), /title must be a string/)
  assert.throws(bad('two\nlines'), /title must be a single line/)
  assert.throws(bad('x'.repeat(MAX_TITLE_LENGTH + 1)), /exceeds 200 characters/)
  assert.throws(() => mergeClusters(clusters, ['not-a-group']), /key arrays or \{ title, keys \}/)
  assert.throws(() => mergeClusters(clusters, [{ title: 'x', keys: [] }]), /must be non-empty/)
})

test('rankClusters puts corroboration before alphabetical order', () => {
  const clusters = clusterNotes([
    note('1', 'Zulu problem\nWhere: z.go'),
    note('2', 'Zulu problem\nWhere: z.go'),
    note('3', 'Alpha problem\nWhere: a.go'),
  ])
  const merged = mergeClusters(
    clusters,
    clusters.map((cluster) => [cluster.key]),
  )
  // Key order is alphabetical, so Alpha leads until ranking is applied.
  assert.match(merged[0].statement, /Alpha/)

  const ranked = rankClusters(merged)
  assert.match(ranked[0].statement, /Zulu/)
  assert.equal(ranked[0].notes.length, 2)
  assert.match(ranked[1].statement, /Alpha/)

  // selectClusters must rank internally rather than trusting its caller.
  const output = selectClusters(merged, [], { cap: 1, now: FRESH_SELECTION_NOW })
  assert.match(output.selected[0].cluster.statement, /Zulu/)
  assert.deepEqual(
    output.deferred.map((entry) => entry.reason),
    ['over-cap'],
  )
})

test('merged issue markers dedupe a later singleton alias recurrence', () => {
  const firstRun = clusterNotes([
    note('1', 'Stale lock blocks worktree creation\nWhere: cmd/worktree.go'),
    note('2', 'Worktree creation is blocked by stale locks\nWhere: cmd/worktree.go'),
  ])
  const merged = mergeClusters(firstRun, [firstRun.map((cluster) => cluster.key)])[0]
  const aliasKey = merged.sourceKeys.find((key) => key !== merged.key)

  const issueDescription = renderClusterMarkers(merged)

  assert.deepEqual(issueDescription.split('\n'), [`Notes: ${aliasKey}`, `Notes: ${merged.key}`])
  const laterSingleton = firstRun.filter((cluster) => cluster.key === aliasKey)
  const output = selectClusters(laterSingleton, [{ description: issueDescription }], {
    now: FRESH_SELECTION_NOW,
  })
  assert.deepEqual(
    output.dropped.map((entry) => entry.cluster.key),
    [aliasKey],
  )
  assert.deepEqual(output.selected, [])
})

test('selectClusters recognizes every source-key alias on a merged cluster', () => {
  const clusters = clusterNotes([
    note('1', 'Stale lock blocks worktree creation\nWhere: cmd/worktree.go'),
    note('2', 'Worktree creation is blocked by stale locks\nWhere: cmd/worktree.go'),
  ])
  const merged = mergeClusters(clusters, [clusters.map((cluster) => cluster.key)])[0]
  const aliasKey = merged.sourceKeys.find((key) => key !== merged.key)

  const output = selectClusters([merged], [{ description: `Notes: ${aliasKey}` }], {
    now: FRESH_SELECTION_NOW,
  })

  assert.deepEqual(
    output.dropped.map((entry) => entry.cluster.key),
    [merged.key],
  )
  assert.deepEqual(output.selected, [])
})

test('selectClusters drops an existing identity marker after a new slug collision appears', () => {
  const firstRun = clusterNotes([note('1', 'foo/bar\nWhere: a')])
  const laterRun = clusterNotes([note('1', 'foo/bar\nWhere: a'), note('2', 'foo bar\nWhere: a')])

  const output = selectClusters(laterRun, [{ description: `Notes: ${firstRun[0].key}` }], {
    now: FRESH_SELECTION_NOW,
  })
  assert.deepEqual(
    output.dropped.map((entry) => entry.cluster.statement),
    ['foo/bar'],
  )
  assert.deepEqual(
    output.selected.map((entry) => entry.cluster.statement),
    ['foo bar'],
  )
})

test('selectClusters line-anchors Notes markers and accounts for every input', () => {
  const clusters = clusterNotes([
    note('1', 'Alpha problem\nWhere: a.go'),
    note('2', 'Beta problem\nWhere: b.go'),
    note('3', 'Gamma problem\nWhere: c.go'),
  ])
  const output = selectClusters(
    clusters,
    [
      { description: `Notes: ${clusters[0].key}` },
      { description: `See Notes: ${clusters[1].key} for detail` },
      { description: `Notes: ${clusters[2].key}-extra` },
    ],
    { now: FRESH_SELECTION_NOW },
  )
  assert.deepEqual(
    output.selected.map((entry) => entry.cluster.key),
    [clusters[1].key, clusters[2].key],
  )
  assert.deepEqual(
    output.dropped.map((entry) => [entry.cluster.key, entry.reason]),
    [[clusters[0].key, 'already-tracked']],
  )
  assert.deepEqual(output.deferred, [])
  assert.deepEqual(output.expired, [])
  const accounted = [...output.selected, ...output.deferred, ...output.dropped, ...output.expired]
  assert.equal(accounted.length, clusters.length)
  assert.ok(accounted.every((entry) => typeof entry.reason === 'string' && entry.reason.length > 0))
})

test('selectClusters defaults to the documented cap and defers overflow', () => {
  const clusters = clusterNotes(
    Array.from({ length: DEFAULT_CAP + 1 }, (_, index) =>
      note(String(index), `problem number ${index}`),
    ),
  )
  const output = selectClusters(clusters, [], { now: FRESH_SELECTION_NOW })
  assert.equal(output.selected.length, DEFAULT_CAP)
  assert.deepEqual(
    output.deferred.map((entry) => entry.reason),
    ['over-cap'],
  )
  assert.deepEqual(output.dropped, [])
  assert.deepEqual(output.expired, [])
})

test('selectClusters expires themes by newest parseable note timestamp', () => {
  const oldOnly = clusterNotes([
    { id: '1', body: 'Old problem\nWhere: a.go', created_at: '2026-07-01T00:00:00Z' },
  ])
  const recurringClusters = clusterNotes([
    { id: '2', body: 'Recurring problem\nWhere: b.go', created_at: '2026-07-01T00:00:00Z' },
    { id: '3', body: 'Recurring problem\nWhere: b.go', created_at: '2026-08-15T00:00:00Z' },
  ])
  const recurring = mergeClusters(recurringClusters, [
    recurringClusters.map((cluster) => cluster.key),
  ])
  const unknown = [
    {
      key: 'unknown',
      statement: 'Unknown timestamp',
      where: 'c.go',
      notes: [{ id: '4', body: 'Unknown timestamp\nWhere: c.go', created_at: 'not-a-date' }],
    },
  ]

  const output = selectClusters([...oldOnly, ...recurring, ...unknown], [], {
    cap: 10,
    staleDays: 30,
    now: Date.parse('2026-08-20T00:00:00Z'),
  })

  assert.deepEqual(
    output.expired.map((entry) => [entry.cluster.statement, entry.reason]),
    [['Old problem', 'expired']],
  )
  assert.deepEqual(
    output.selected.map((entry) => entry.cluster.statement),
    ['Recurring problem', 'Unknown timestamp'],
  )
})

test('selectClusters applies already-tracked before expiry and expiry before cap', () => {
  const clusters = clusterNotes([
    { id: '1', body: 'Tracked old\nWhere: a.go', created_at: '2026-07-01T00:00:00Z' },
    { id: '2', body: 'Expired old\nWhere: b.go', created_at: '2026-07-01T00:00:00Z' },
    { id: '3', body: 'Fresh one\nWhere: c.go', created_at: '2026-08-19T00:00:00Z' },
  ])
  const tracked = clusters.find((cluster) => cluster.statement === 'Tracked old')
  const output = selectClusters(clusters, [{ description: `Notes: ${tracked.key}` }], {
    cap: 0,
    staleDays: 30,
    now: Date.parse('2026-08-20T00:00:00Z'),
  })

  assert.deepEqual(
    output.dropped.map((entry) => [entry.cluster.statement, entry.reason]),
    [['Tracked old', 'already-tracked']],
  )
  assert.deepEqual(
    output.expired.map((entry) => [entry.cluster.statement, entry.reason]),
    [['Expired old', 'expired']],
  )
  assert.deepEqual(
    output.deferred.map((entry) => [entry.cluster.statement, entry.reason]),
    [['Fresh one', 'over-cap']],
  )
})

test('resolveCap prefers an argument, then the environment, then the default', () => {
  assert.equal(resolveCap('50', {}), 50)
  assert.equal(resolveCap(undefined, { BS_SWEEP_NOTES_MAX_ISSUES: '50' }), 50)
  // An explicit argument still wins, so an operator override is never silently lost.
  assert.equal(resolveCap('7', { BS_SWEEP_NOTES_MAX_ISSUES: '50' }), 7)
  assert.equal(resolveCap(undefined, {}), DEFAULT_CAP)
  assert.equal(resolveCap('', { BS_SWEEP_NOTES_MAX_ISSUES: '' }), DEFAULT_CAP)
  assert.equal(resolveCap('nonsense', {}), DEFAULT_CAP)
  assert.equal(resolveCap('0', {}), 0)
  assert.equal(resolveCap('-4', {}), 0)
  assert.equal(resolveCap('3.9', {}), 3)
})

test('resolveStaleDays prefers an argument, then the environment, then the default', () => {
  assert.equal(resolveStaleDays('45', {}), 45)
  assert.equal(resolveStaleDays(undefined, { BS_SWEEP_NOTES_STALE_DAYS: '45' }), 45)
  assert.equal(resolveStaleDays('7', { BS_SWEEP_NOTES_STALE_DAYS: '45' }), 7)
  assert.equal(resolveStaleDays(undefined, {}), DEFAULT_STALE_DAYS)
  assert.equal(resolveStaleDays('', { BS_SWEEP_NOTES_STALE_DAYS: '' }), DEFAULT_STALE_DAYS)
  assert.equal(resolveStaleDays('nonsense', {}), DEFAULT_STALE_DAYS)
  assert.equal(resolveStaleDays('-4', {}), DEFAULT_STALE_DAYS)
  assert.equal(resolveStaleDays('3.9', {}), 3)
})

test('runCli select honours the environment cap without a shell expansion', () => {
  const clusters = clusterNotes(
    Array.from({ length: 4 }, (_, index) => ({
      id: String(index),
      body: `problem number ${index}`,
      created_at: '2026-08-20T00:00:00Z',
    })),
  )
  const files = { 'c.json': JSON.stringify(clusters), 'l.json': '[]' }
  const read = (file) => files[file]

  // This is the case that silently regressed: the cap arrives only in the
  // environment, exactly as a `BS_SWEEP_NOTES_MAX_ISSUES=2 node …` prefix delivers it.
  const viaEnv = JSON.parse(
    runCli(['select', 'c.json', 'l.json'], {
      readFile: read,
      env: { BS_SWEEP_NOTES_MAX_ISSUES: '2', BS_SWEEP_NOTES_STALE_DAYS: '100000' },
    }),
  )
  assert.equal(viaEnv.selected.length, 2)
  assert.equal(viaEnv.deferred.length, 2)

  const viaArg = JSON.parse(
    runCli(['select', 'c.json', 'l.json', '1'], {
      readFile: read,
      env: { BS_SWEEP_NOTES_MAX_ISSUES: '2', BS_SWEEP_NOTES_STALE_DAYS: '100000' },
    }),
  )
  assert.equal(viaArg.selected.length, 1)

  const viaDefault = JSON.parse(
    runCli(['select', 'c.json', 'l.json'], {
      readFile: read,
      env: { BS_SWEEP_NOTES_STALE_DAYS: '100000' },
    }),
  )
  assert.equal(viaDefault.selected.length, 4)
})

test('runCli select honours the stale-days environment without a shell expansion', () => {
  const clusters = clusterNotes([
    { id: '1', body: 'Old problem\nWhere: a.go', created_at: '2026-07-01T00:00:00Z' },
  ])
  const files = { 'c.json': JSON.stringify(clusters), 'l.json': '[]' }
  const read = (file) => files[file]

  const viaEnv = JSON.parse(
    runCli(['select', 'c.json', 'l.json'], {
      readFile: read,
      env: {
        BS_SWEEP_NOTES_STALE_DAYS: '100000',
      },
    }),
  )
  assert.equal(viaEnv.selected.length, 1)
  assert.equal(viaEnv.expired.length, 0)

  const viaArg = JSON.parse(
    runCli(['select', 'c.json', 'l.json', '', '0'], {
      readFile: read,
      env: { BS_SWEEP_NOTES_STALE_DAYS: '100000' },
    }),
  )
  assert.equal(viaArg.selected.length, 0)
  assert.equal(viaArg.expired.length, 1)
})

test('stalenessSignals reports missing and post-note-change paths, never a verdict', () => {
  const clusters = clusterNotes([
    note('1', 'Glob aborts cleanup\nWhere: skills/boss-plan/SKILL.md Phase 5 and cmd/gone.go'),
  ])
  const merged = mergeClusters(clusters, [clusters.map((cluster) => cluster.key)])
  const signals = stalenessSignals(merged, {
    pathExists: (path) => path !== 'cmd/gone.go',
    // Same instant as the note, expressed in a different offset: a naive string
    // compare would call this "changed since".
    lastChangeAt: () => '2026-07-21T09:00:00+09:00',
  })

  assert.equal(signals.length, 1)
  assert.deepEqual(signals[0].paths, ['cmd/gone.go', 'skills/boss-plan/SKILL.md'])
  assert.deepEqual(signals[0].missing, ['cmd/gone.go'])
  assert.deepEqual(signals[0].changedSince, [])
  assert.equal(signals[0].newestNoteAt, '2026-07-21T00:00:00.000Z')

  const changed = stalenessSignals(merged, {
    pathExists: () => true,
    lastChangeAt: () => '2026-08-01T00:00:00Z',
  })
  assert.deepEqual(changed[0].changedSince, ['cmd/gone.go', 'skills/boss-plan/SKILL.md'])

  assert.throws(() => stalenessSignals(merged, {}), /requires pathExists and lastChangeAt/)
})

test('stalenessSignals ignores prose, absolute and home-relative Where tokens', () => {
  const clusters = clusterNotes([
    note('1', 'Installed copy drifts\nWhere: ~/.claude/skills/boss-plan/SKILL.md step 2'),
    note('2', 'Absolute path cited\nWhere: /Users/dave/x/y.go and boss-build Step 11'),
  ])
  const merged = mergeClusters(clusters, [clusters.map((cluster) => cluster.key)])
  const signals = stalenessSignals(merged, {
    pathExists: () => false,
    lastChangeAt: () => null,
  })

  assert.deepEqual(signals[0].paths, [])
  assert.deepEqual(signals[0].missing, [])
})

test('applyVerdicts partitions every theme and demands evidence for fixed', () => {
  const clusters = clusterNotes([
    note('1', 'Alpha problem\nWhere: a.go'),
    note('2', 'Beta problem\nWhere: b.go'),
    note('3', 'Gamma problem\nWhere: c.go'),
  ])
  const merged = mergeClusters(
    clusters,
    clusters.map((cluster) => [cluster.key]),
  )
  const [alpha, beta, gamma] = merged.map((cluster) => cluster.key)

  const buckets = applyVerdicts(merged, [
    { key: gamma, verdict: 'unverifiable' },
    { key: beta, verdict: 'fixed', evidence: 'cmd/b.go:12 already guards this' },
    { key: alpha, verdict: 'live' },
  ])

  assert.deepEqual(
    buckets.live.map((entry) => entry.cluster.key),
    [alpha],
  )
  assert.deepEqual(
    buckets.fixed.map((entry) => [entry.cluster.key, entry.evidence]),
    [[beta, 'cmd/b.go:12 already guards this']],
  )
  assert.deepEqual(
    buckets.unverifiable.map((entry) => entry.cluster.key),
    [gamma],
  )
  assert.equal(
    buckets.live.length + buckets.fixed.length + buckets.unverifiable.length,
    merged.length,
  )

  assert.throws(
    () => applyVerdicts(merged, [{ key: alpha, verdict: 'live' }]),
    /account for every cluster/,
  )
  assert.throws(
    () =>
      applyVerdicts(merged, [
        { key: alpha, verdict: 'live' },
        { key: beta, verdict: 'fixed' },
        { key: gamma, verdict: 'unverifiable' },
      ]),
    /requires evidence for a fixed verdict/,
  )
  assert.throws(
    () =>
      applyVerdicts(merged, [
        { key: alpha, verdict: 'resolved' },
        { key: beta, verdict: 'live' },
        { key: gamma, verdict: 'live' },
      ]),
    /unknown verdict/,
  )
  assert.throws(
    () =>
      applyVerdicts(merged, [
        { key: alpha, verdict: 'live' },
        { key: alpha, verdict: 'live' },
        { key: gamma, verdict: 'live' },
      ]),
    /unknown or duplicate key/,
  )
})

test('a live theme can retire individual fixed members', () => {
  const clusters = clusterNotes([
    note('1', 'Glob aborts cleanup\nWhere: internal/a.go'),
    note('2', 'Scratch survives an abort\nWhere: internal/b.go'),
  ])
  const theme = mergeClusters(clusters, [
    { title: 'Cleanup leaves scratch behind', keys: clusters.map((c) => c.key) },
  ])
  const ids = theme[0].notes.map((n) => n.id)

  // The whole point: the theme stays live and still gets filed, but the member
  // whose defect is provably gone is retired anyway.
  const buckets = applyVerdicts(theme, [
    {
      key: theme[0].key,
      verdict: 'live',
      evidence: 'internal/a.go:12 now guards it',
      fixedNotes: [ids[0]],
    },
  ])
  assert.equal(buckets.live.length, 1)
  assert.deepEqual(buckets.live[0].fixedNoteIds, [ids[0]])
  assert.deepEqual(retiredNoteIds(buckets), [ids[0]])
  assert.deepEqual(
    retiredNoteIds(buckets, { expired: [{ cluster: theme[0], reason: 'expired' }] }),
    [...ids].sort((a, b) => a.localeCompare(b)),
  )

  // A wholly fixed theme retires all of its notes without naming them.
  const whole = applyVerdicts(theme, [
    { key: theme[0].key, verdict: 'fixed', evidence: 'internal/a.go:12' },
  ])
  assert.deepEqual(
    retiredNoteIds(whole),
    [...ids].sort((a, b) => a.localeCompare(b)),
  )

  // Nothing is retired by default.
  const none = applyVerdicts(theme, [{ key: theme[0].key, verdict: 'live' }])
  assert.deepEqual(retiredNoteIds(none), [])
  assert.deepEqual(none.live[0].fixedNoteIds, [])
})

test('fixedNotes cannot reach outside its theme or skip evidence', () => {
  const clusters = clusterNotes([
    note('1', 'Alpha problem\nWhere: internal/a.go'),
    note('2', 'Beta problem\nWhere: internal/b.go'),
  ])
  const themes = mergeClusters(
    clusters,
    clusters.map((c) => [c.key]),
  )
  const [alpha, beta] = themes
  const foreign = beta.notes[0].id
  const own = alpha.notes[0].id
  const verdict = (extra) => [
    { key: alpha.key, verdict: 'live', ...extra },
    { key: beta.key, verdict: 'live' },
  ]

  assert.throws(
    () => applyVerdicts(themes, verdict({ evidence: 'x', fixedNotes: [foreign] })),
    /names a note outside/,
  )
  assert.throws(
    () => applyVerdicts(themes, verdict({ fixedNotes: [own] })),
    /requires evidence to retire notes/,
  )
  assert.throws(
    () => applyVerdicts(themes, verdict({ evidence: 'x', fixedNotes: [own, own] })),
    /repeats a note id/,
  )
  assert.throws(
    () => applyVerdicts(themes, verdict({ evidence: 'x', fixedNotes: own })),
    /must be an array/,
  )
  assert.throws(
    () => applyVerdicts(themes, verdict({ evidence: 'x', fixedNotes: [''] })),
    /must be note ids/,
  )
})

test('runCli exposes rank, stale and verdicts through injected probes', () => {
  // The first two notes share an identity, so clusterNotes already yields two
  // clusters: a two-note Alpha and a one-note Zulu.
  const clusters = clusterNotes([
    note('1', 'Alpha problem\nWhere: internal/a.go'),
    note('2', 'Alpha problem\nWhere: internal/a.go'),
    note('3', 'Zulu problem\nWhere: internal/z.go'),
  ])
  assert.equal(clusters.length, 2)
  const merged = mergeClusters(
    clusters,
    clusters.map((cluster) => [cluster.key]),
  )
  const files = {
    'merged.json': JSON.stringify(merged),
    'verdicts.json': JSON.stringify(
      merged.map((cluster) => ({ key: cluster.key, verdict: 'live' })),
    ),
  }
  const options = {
    readFile: (file) => files[file],
    pathExists: () => true,
    lastChangeAt: () => '2026-08-01T00:00:00Z',
  }

  const ranked = JSON.parse(runCli(['rank', 'merged.json'], options))
  assert.equal(ranked[0].notes.length, 2)

  const signals = JSON.parse(runCli(['stale', 'merged.json'], options))
  assert.deepEqual(signals[0].changedSince, ['internal/a.go'])

  const buckets = JSON.parse(runCli(['verdicts', 'merged.json', 'verdicts.json'], options))
  assert.equal(buckets.live.length, merged.length)
  assert.deepEqual(buckets.fixed, [])

  files['buckets.json'] = JSON.stringify(buckets)
  assert.deepEqual(JSON.parse(runCli(['retired', 'buckets.json'], options)), [])
})

test('fetchMarkedLinearIssues paginates archived issues and fails closed on truncation', async () => {
  const calls = []
  const linearRequest = async (request) => {
    calls.push(request)
    return calls.length === 1
      ? {
          issues: {
            nodes: [{ identifier: 'BOS-1', description: 'Notes: a' }],
            pageInfo: { hasNextPage: true, endCursor: 'next' },
          },
        }
      : {
          issues: {
            nodes: [{ identifier: 'BOS-2', description: 'Notes: b' }],
            pageInfo: { hasNextPage: false, endCursor: null },
          },
        }
  }
  assert.deepEqual(await fetchMarkedLinearIssues({ apiKey: 'key', linearRequest }), [
    { identifier: 'BOS-1', description: 'Notes: a' },
    { identifier: 'BOS-2', description: 'Notes: b' },
  ])
  assert.equal(calls[0].query, MARKED_ISSUES_QUERY)
  assert.deepEqual(calls[0].variables.filter, { description: { contains: 'Notes: ' } })
  assert.match(MARKED_ISSUES_QUERY, /includeArchived: true/)

  await assert.rejects(
    fetchMarkedLinearIssues({
      linearRequest: async () => ({
        issues: { nodes: [], pageInfo: { hasNextPage: true, endCursor: 'more' } },
      }),
      maxPages: 1,
    }),
    /dedupe snapshot incomplete/,
  )

  for (const issues of [
    { nodes: null, pageInfo: { hasNextPage: false, endCursor: null } },
    { nodes: {}, pageInfo: { hasNextPage: false, endCursor: null } },
    { nodes: [], pageInfo: null },
    { nodes: [], pageInfo: { hasNextPage: 'false', endCursor: null } },
    { nodes: [], pageInfo: { hasNextPage: true, endCursor: null } },
  ]) {
    await assert.rejects(
      fetchMarkedLinearIssues({
        linearRequest: async () => ({ issues }),
      }),
      /malformed|continuation cursor/,
    )
  }

  for (const nodes of [
    [null],
    [{ identifier: '', description: 'Notes: a' }],
    [{ identifier: 'BOS-1', description: null }],
  ]) {
    await assert.rejects(
      fetchMarkedLinearIssues({
        linearRequest: async () => ({
          issues: { nodes, pageInfo: { hasNextPage: false, endCursor: null } },
        }),
      }),
      /malformed issue entry/,
    )
  }
})

test('runCli parses note JSON through its injected reader', () => {
  const output = runCli(['parse', 'notes.json'], {
    readFile: () => JSON.stringify([note('1', 'A problem\nWhere: a.go')]),
  })
  assert.deepEqual(JSON.parse(output), [
    {
      id: '1',
      body: 'A problem\nWhere: a.go',
      created_at: '2026-07-21T00:00:00Z',
      statement: 'A problem',
      where: 'a.go',
      whyItMatters: null,
      suggestedFix: null,
      run: null,
    },
  ])
})
