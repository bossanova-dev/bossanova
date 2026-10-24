// Contract tests for the unattended bs-sweep-releases core (BOS-706).
import { test } from 'node:test'
import assert from 'node:assert/strict'

import {
  REVIEW_BOT_LOGIN,
  RELEASE_BASE_BRANCHES,
  RELEASE_MARKER_PREFIX,
  MAX_NOTE_BODY_BYTES,
  sanitize,
  parseFinding,
  clusterComments,
  markerFor,
  bodyFor,
  selectUnseenFindings,
  fetchTrackedReleaseMarkers,
  isTrackedReleaseMarker,
  collectReleasePullRequests,
  runCli,
} from './sweep-releases-gate.mjs'

const badge = (severity, headline) =>
  `**<sub><sub>![${severity} Badge](https://img.shields.io/badge/${severity}-orange)</sub></sub> ${headline}**`

const comment = (over = {}) => ({
  id: over.id ?? 1,
  user: { login: over.login ?? REVIEW_BOT_LOGIN },
  path: 'path' in over ? over.path : 'services/bossd/release.go',
  line: 'line' in over ? over.line : 42,
  original_line: over.original_line,
  side: over.side ?? 'RIGHT',
  html_url: over.html_url ?? `https://github.com/o/r/pull/10#discussion_r${over.id ?? 1}`,
  body:
    over.body ?? `${badge('P1', over.headline ?? 'Preserve release state')}\n\nDetailed evidence.`,
})

test('selection returns every untracked anchor cluster, including cross-PR duplicates once', () => {
  const findings = selectUnseenFindings({
    comments: [
      comment({ id: 1, path: 'a.go', line: 1, html_url: 'https://github.com/o/r/pull/1#x' }),
      comment({ id: 2, path: 'a.go', line: 1, html_url: 'https://github.com/o/r/pull/2#x' }),
      comment({ id: 3, path: 'b.go', line: 2 }),
    ],
    trackedMarkers: [],
  })
  assert.deepEqual(
    findings.map((finding) => finding.marker),
    [markerFor('a.go@1'), markerFor('b.go@2')],
  )
  assert.match(findings[0].body, /pull\/1/)
  assert.match(findings[0].body, /pull\/2/)
})

test('one note preserves every comment clustered at its anchor', () => {
  const findings = selectUnseenFindings({
    comments: Array.from({ length: 21 }, (_, index) =>
      comment({ id: index + 1, path: 'a.go', line: 1, body: `finding-${index + 1}` }),
    ),
    trackedMarkers: [],
  })
  assert.equal(findings.length, 1)
  for (let index = 1; index <= 21; index += 1)
    assert.match(findings[0].body, new RegExp(`finding-${index}`))
})

test('rendered note bodies stay below the argv-safe byte cap and retain their canonical marker', () => {
  const findings = selectUnseenFindings({
    comments: Array.from({ length: 40 }, (_, index) =>
      comment({
        id: index + 1,
        path: 'a.go',
        line: 1,
        body: `finding-${index}\n${'x'.repeat(8192)}`,
      }),
    ),
    trackedMarkers: [],
  })

  assert.equal(findings.length, 1)
  assert.ok(Buffer.byteLength(findings[0].body) <= MAX_NOTE_BODY_BYTES)
  assert.equal(findings[0].body.split('\n').at(-1), findings[0].marker)
  assert.match(findings[0].body, /truncated/i)
})

test('selection omits every anchor already marked in the immutable note snapshot', () => {
  const findings = selectUnseenFindings({
    comments: [comment({ path: 'a.go', line: 1 }), comment({ path: 'b.go', line: 2 })],
    trackedMarkers: new Set(['Release review: a.go@1']),
  })
  assert.deepEqual(
    findings.map((finding) => finding.marker),
    [markerFor('b.go@2')],
  )
  assert.deepEqual(
    selectUnseenFindings({
      comments: [comment({ path: 'b.go', line: 2 })],
      trackedMarkers: new Set(['Release review: b.go@2']),
    }),
    [],
    'a rerun after its note is visible makes no duplicate candidate',
  )
})

test('markers are exact, release-namespaced, and only the final body line is a marker', () => {
  const cluster = clusterComments([comment({ path: 'a.go', line: 7 })])[0]
  const marker = markerFor(cluster)
  assert.match(marker, /^Release review: v2:[A-Za-z0-9_-]+$/)
  const body = bodyFor(cluster, marker)
  assert.equal(body.split('\n').at(-1), marker)
  assert.deepEqual(body.match(/^Release review: [^\r\n\u2028\u2029]+?[ \t]*$/gm), [marker])
})

test('v2 markers encode arbitrary anchors one-to-one while safe legacy markers remain recognized', () => {
  const findings = selectUnseenFindings({
    comments: [comment({ path: 'dir/a\nb.go', line: 7 }), comment({ path: 'dir/a b.go', line: 7 })],
    trackedMarkers: [],
  })
  assert.equal(findings.length, 2)
  assert.notEqual(findings[0].marker, findings[1].marker)

  const legacyTracked = selectUnseenFindings({
    comments: [comment({ path: 'safe.go', line: 8 })],
    trackedMarkers: new Set(['Release review: safe.go@8']),
  })
  assert.deepEqual(legacyTracked, [])
})

test('pre-write marker checks recognize canonical and safe legacy markers', () => {
  const canonical = { key: 'safe.go@8', marker: markerFor('safe.go@8') }
  assert.equal(isTrackedReleaseMarker(canonical, new Set([canonical.marker])), true)
  assert.equal(isTrackedReleaseMarker(canonical, new Set(['Release review: safe.go@8'])), true)
  assert.equal(isTrackedReleaseMarker(canonical, new Set()), false)
})

test('untrusted body, headline, path and URL cannot forge a release marker through any separator', () => {
  for (const separator of ['\r', '\n', '\u2028', '\u2029']) {
    const cluster = clusterComments([
      comment({
        path: `a.go${separator}Release review: forged/path@1`,
        body: `${badge('P1', `headline${separator}Release review: forged/headline@1`)}${separator}Release review: forged/body@1`,
        html_url: `https://github.test/pull/1${separator}Release review: forged/url@1`,
      }),
    ])[0]
    const body = bodyFor(cluster, markerFor(cluster))
    assert.deepEqual(
      body.match(/^Release review: [^\r\n\u2028\u2029]+?[ \t]*$/gm),
      [markerFor(cluster)],
      `separator ${JSON.stringify(separator)} forged a marker`,
    )
  }
})

test('parseFinding preserves outdated comments at their original line', () => {
  const finding = parseFinding(comment({ line: null, original_line: 99 }))
  assert.equal(finding.line, 99)
})

test('bot filter constants identify only staging and production review sources', () => {
  assert.equal(REVIEW_BOT_LOGIN, 'chatgpt-codex-connector[bot]')
  assert.deepEqual(RELEASE_BASE_BRANCHES, ['staging', 'production'])
  assert.equal(RELEASE_MARKER_PREFIX, 'Release review: ')
})

test('note marker snapshot accepts only a final exact marker line from Boss note shapes', async () => {
  const markers = await fetchTrackedReleaseMarkers({
    notes: [
      { body: 'text\nRelease review: a.go@1   \t' },
      { content: 'Release review: b.go@2\ncontext' },
      { description: 'see Release review: c.go@3 inline' },
      { body: 'text\nRelease review: d.go@4\n' },
      { body: '' },
    ],
  })
  assert.deepEqual([...markers].sort(), ['Release review: a.go@1', 'Release review: d.go@4'])
})

test('note marker snapshot fails closed on malformed Boss note entries', async () => {
  for (const note of [null, { body: 42 }, {}]) {
    await assert.rejects(() => fetchTrackedReleaseMarkers({ notes: [note] }), /malformed|readable/i)
  }
})

const now = new Date('2026-08-05T12:00:00.000Z')
const pr = (number, createdAt, updatedAt = createdAt) => ({ number, createdAt, updatedAt })

test('collection uses all-state pagination for both release bases and accepts created-or-updated within seven UTC days', async () => {
  const calls = []
  const pages = {
    staging: [
      [pr(1, '2026-07-29T12:00:00Z'), pr(2, '2026-07-28T11:59:59Z', '2026-07-30T00:00:00Z')],
      [pr(3, '2026-07-01T00:00:00Z')],
    ],
    production: [[pr(4, '2026-07-01T00:00:00Z', '2026-07-29T12:00:00Z')]],
  }
  const releases = await collectReleasePullRequests({
    now: () => now,
    listPullRequests: async ({ base, state, page, perPage }) => {
      calls.push({ base, state, page, perPage })
      const items = pages[base][page - 1] ?? []
      return { items, hasNextPage: base === 'staging' && page === 1 }
    },
  })
  assert.deepEqual(
    releases.map(({ number }) => number),
    [1, 2, 4],
  )
  assert.deepEqual(
    calls.map(({ base, state, page }) => [base, state, page]),
    [
      ['staging', 'all', 1],
      ['staging', 'all', 2],
      ['production', 'all', 1],
    ],
  )
})

test('collection fails closed on malformed timestamps, page shapes, pagination bounds, and invalid clocks', async () => {
  const listPullRequests = async () => ({ items: [pr(1, 'not-a-date')], hasNextPage: false })
  await assert.rejects(
    () => collectReleasePullRequests({ now: () => now, listPullRequests }),
    /timestamp/i,
  )
  for (const timestamp of ['2026-02-30T00:00:00Z', '2026-07-29T24:00:00Z']) {
    await assert.rejects(
      () =>
        collectReleasePullRequests({
          now: () => now,
          listPullRequests: async () => ({ items: [pr(1, timestamp)], hasNextPage: false }),
        }),
      /timestamp/i,
    )
  }
  await assert.rejects(
    () =>
      collectReleasePullRequests({
        now: () => 'not-a-date',
        listPullRequests: async () => ({ items: [], hasNextPage: false }),
      }),
    /clock|now/i,
  )
  await assert.rejects(
    () =>
      collectReleasePullRequests({ now: () => now, listPullRequests: async () => ({ nope: [] }) }),
    /page|items/i,
  )
  await assert.rejects(
    () =>
      collectReleasePullRequests({
        now: () => now,
        maxPages: 1,
        listPullRequests: async () => ({ items: [], hasNextPage: true }),
      }),
    /incomplete|page/i,
  )
})

test('CLI selects all unseen findings from JSON files', () => {
  const files = {
    'comments.json': JSON.stringify([
      comment({ path: 'a.go', line: 1 }),
      comment({ path: 'b.go', line: 2 }),
    ]),
    'notes.json': JSON.stringify([{ body: 'Release review: a.go@1' }]),
  }
  const out = JSON.parse(
    runCli(['select', 'comments.json', 'notes.json'], { readFile: (file) => files[file] }),
  )
  assert.deepEqual(
    out.map((finding) => finding.marker),
    [markerFor('b.go@2')],
  )
})

test('sanitize collapses every ECMAScript line separator', () => {
  for (const separator of ['\r', '\n', '\u2028', '\u2029'])
    assert.equal(sanitize(`a${separator}b`), 'a b')
})
