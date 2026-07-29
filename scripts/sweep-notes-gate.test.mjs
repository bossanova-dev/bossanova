import assert from 'node:assert/strict'
import { test } from 'node:test'

import {
  MARKED_ISSUES_QUERY,
  clusterNotes,
  fetchMarkedLinearIssues,
  mergeClusters,
  parseNote,
  renderClusterMarkers,
  runCli,
  selectClusters,
} from './sweep-notes-gate.mjs'

const note = (id, body) => ({ id, body, created_at: `2026-07-2${id}T00:00:00Z` })

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
        key: 'stale-lock-blocks-worktree-creation--cmd-other-go--c3RhbGUgbG9jayBibG9ja3Mgd29ya3RyZWUgY3JlYXRpb24AY21kL290aGVyLmdv',
        statement: 'Stale lock blocks worktree creation',
        where: 'cmd/other.go',
        ids: ['2'],
      },
      {
        key: 'stale-lock-blocks-worktree-creation--cmd-worktree-go--c3RhbGUgbG9jayBibG9ja3Mgd29ya3RyZWUgY3JlYXRpb24AY21kL3dvcmt0cmVlLmdv',
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

  const marked = selectClusters(first, [{ description: `Notes: ${first[0].key}` }])
  assert.deepEqual(
    marked.dropped.map((entry) => entry.cluster.key),
    [first[0].key],
  )
  assert.deepEqual(
    marked.selected.map((entry) => entry.cluster.key),
    [first[1].key],
  )
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
  assert.throws(() => mergeClusters(clusters, [[...related, unrelated]]), /different Where targets/)
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
  const output = selectClusters(laterSingleton, [{ description: issueDescription }])
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

  const output = selectClusters([merged], [{ description: `Notes: ${aliasKey}` }])

  assert.deepEqual(
    output.dropped.map((entry) => entry.cluster.key),
    [merged.key],
  )
  assert.deepEqual(output.selected, [])
})

test('selectClusters drops an existing identity marker after a new slug collision appears', () => {
  const firstRun = clusterNotes([note('1', 'foo/bar\nWhere: a')])
  const laterRun = clusterNotes([note('1', 'foo/bar\nWhere: a'), note('2', 'foo bar\nWhere: a')])

  const output = selectClusters(laterRun, [{ description: `Notes: ${firstRun[0].key}` }])
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
  const output = selectClusters(clusters, [
    { description: `Notes: ${clusters[0].key}` },
    { description: `See Notes: ${clusters[1].key} for detail` },
    { description: `Notes: ${clusters[2].key}-extra` },
  ])
  assert.deepEqual(
    output.selected.map((entry) => entry.cluster.key),
    [clusters[1].key, clusters[2].key],
  )
  assert.deepEqual(
    output.dropped.map((entry) => [entry.cluster.key, entry.reason]),
    [[clusters[0].key, 'already-tracked']],
  )
  assert.deepEqual(output.deferred, [])
  const accounted = [...output.selected, ...output.deferred, ...output.dropped]
  assert.equal(accounted.length, clusters.length)
  assert.ok(accounted.every((entry) => typeof entry.reason === 'string' && entry.reason.length > 0))
})

test('selectClusters defaults to five selections and defers overflow', () => {
  const clusters = clusterNotes(
    ['f', 'e', 'd', 'c', 'b', 'a'].map((letter, index) => note(String(index), `${letter} problem`)),
  )
  const output = selectClusters(clusters, [])
  assert.equal(output.selected.length, 5)
  assert.deepEqual(
    output.deferred.map((entry) => entry.reason),
    ['over-cap'],
  )
  assert.deepEqual(output.dropped, [])
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
