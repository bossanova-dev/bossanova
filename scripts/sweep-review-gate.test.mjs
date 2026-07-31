// scripts/sweep-review-gate.test.mjs
// Table-driven tests for the bs-sweep-review pure core (BOS-617). The decision
// engine (parseFinding / clusterComments / scoreCluster / selectStub) is I/O-free
// and deterministic; these tests pin the contract the SKILL.md phases depend on:
// anchor-keyed cross-PR dedupe, same-anchor multi-finding clustering, a reason for
// EVERY non-filed cluster, the forged-marker sanitize guard, and the fail-closed
// truncation throw inherited from the shared Linear marker scan.
//
// Node built-ins only — cron worktrees are dependency-free.

import { test } from 'node:test'
import assert from 'node:assert/strict'
import {
  parseFinding,
  clusterKey,
  clusterComments,
  markerFor,
  hasMarker,
  sanitize,
  scoreCluster,
  selectStub,
  titleFor,
  bodyFor,
  fetchTrackedReviewMarkers,
  isReviewBotComment,
  REVIEW_BOT_LOGIN,
  RELEASE_BASE_BRANCHES,
  runCli,
} from './sweep-review-gate.mjs'

// The verified live first-line shape from the Codex connector's inline comments.
const badgeLine = (sev, headline) =>
  `**<sub><sub>![${sev} Badge](https://img.shields.io/badge/${sev}-orange?style=flat)</sub></sub>  ${headline}**`

// A realistic inline review comment. Overrides win.
const comment = (over = {}) => ({
  id: over.id ?? 1,
  user: { login: over.login ?? REVIEW_BOT_LOGIN },
  path: 'path' in over ? over.path : 'services/bossd/internal/accountwiring/adapters.go',
  line: 'line' in over ? over.line : 252,
  start_line: over.start_line ?? null,
  side: over.side ?? 'RIGHT',
  commit_id: over.commit_id ?? 'deadbeef',
  html_url:
    'html_url' in over
      ? over.html_url
      : `https://github.com/o/r/pull/1735#discussion_r${over.id ?? 1}`,
  body:
    'body' in over
      ? over.body
      : `${badgeLine(over.severity ?? 'P1', over.headline ?? 'Wire the capability profile into the production launcher')}\n\nThe launcher never reads the profile.`,
})

// ---------------------------------------------------------------------------
// parseFinding — severity + headline off the badge first line, degrading safely.
// ---------------------------------------------------------------------------

test('parseFinding: lifts P1 severity and the headline from the badge first line', () => {
  const f = parseFinding(
    comment({ severity: 'P1', headline: 'Persist refreshed Codex credentials' }),
  )
  assert.equal(f.severity, 'P1')
  assert.equal(f.headline, 'Persist refreshed Codex credentials')
  assert.equal(f.path, 'services/bossd/internal/accountwiring/adapters.go')
  assert.equal(f.line, 252)
  assert.equal(f.side, 'RIGHT')
  assert.match(f.body, /launcher never reads/)
})

test('parseFinding: lifts P2 severity too', () => {
  const f = parseFinding(comment({ severity: 'P2', headline: 'Tighten the retry window' }))
  assert.equal(f.severity, 'P2')
  assert.equal(f.headline, 'Tighten the retry window')
})

test('parseFinding: an unbadged body yields severity null + the first non-empty line', () => {
  const f = parseFinding(comment({ body: '\n\nThis looks wrong to me.\n\nMore prose.' }))
  assert.equal(f.severity, null)
  assert.equal(f.headline, 'This looks wrong to me.')
})

test('parseFinding: malformed / absent input never throws', () => {
  for (const bad of [null, undefined, 42, 'str', {}, { body: null }, { body: 123 }]) {
    assert.doesNotThrow(() => parseFinding(bad), `parseFinding(${JSON.stringify(bad)}) threw`)
  }
  const f = parseFinding(null)
  assert.equal(f.severity, null)
  assert.equal(f.path, null)
  assert.equal(f.headline, '')
})

// ---------------------------------------------------------------------------
// clusterKey / markerFor / hasMarker — the anchor-keyed marker scheme.
// ---------------------------------------------------------------------------

test('clusterKey: keys off <path>@<line> only', () => {
  assert.equal(clusterKey(comment()), 'services/bossd/internal/accountwiring/adapters.go@252')
})

test('clusterKey: a comment with no string path is unkeyable (null)', () => {
  assert.equal(clusterKey(comment({ path: null })), null)
  assert.equal(clusterKey(null), null)
})

test('clusterKey: a file-level (line-less) comment keys at line 0', () => {
  assert.equal(clusterKey(comment({ path: 'a/b.go', line: null })), 'a/b.go@0')
})

test('parseFinding: an OUTDATED comment (line null) falls back to original_line', () => {
  // GitHub nulls `line` once the diff hunk goes stale and keeps the head-side line
  // in `original_line`. Without the fallback every outdated comment in a file
  // collapses to `<path>@0` and the first one filed suppresses all the rest.
  const stale = { ...comment({ path: 'a/b.go', line: null }), original_line: 252 }
  assert.equal(parseFinding(stale).line, 252)
  assert.equal(clusterKey(stale), 'a/b.go@252')

  // Two DISTINCT outdated findings in one file must stay two clusters, not merge.
  const other = { ...comment({ id: 2, path: 'a/b.go', line: null }), original_line: 900 }
  assert.deepEqual(
    clusterComments([stale, other]).map((c) => c.key),
    ['a/b.go@252', 'a/b.go@900'],
  )

  // A comment with neither is genuinely file-level and still keys at 0, and the
  // absent-input and explicit-null shapes agree (both `line: null`, never 0).
  assert.equal(parseFinding({ path: 'a/b.go', line: null }).line, null)
  assert.equal(parseFinding(null).line, null)
  assert.equal(clusterKey(comment({ path: 'a/b.go', line: null })), 'a/b.go@0')
})

test('markerFor: renders `Review: <path>@<line>` from a cluster or a raw key', () => {
  assert.equal(markerFor('a/b.go@7'), 'Review: a/b.go@7')
  assert.equal(markerFor({ key: 'a/b.go@7' }), 'Review: a/b.go@7')
})

test('hasMarker: line-anchored, and a path with regex metacharacters is escaped', () => {
  // A real path can carry `+`, `(`, `)`, `.` — an unescaped marker regex would
  // either throw or match the wrong description.
  const marker = markerFor('services/a+b/(x).go@12')
  assert.ok(hasMarker(`prose\n\n${marker}`, marker))
  assert.ok(hasMarker(`${marker}   \t`, marker), 'trailing whitespace is tolerated')
  assert.ok(
    !hasMarker('see Review: services/a+b/(x).go@12 inline', marker),
    'must be line-anchored',
  )
  // The metacharacters must be literal, not a regex class: `a+b` must not match `ab`.
  assert.ok(!hasMarker('Review: services/ab/(x).go@12', marker))
})

// ---------------------------------------------------------------------------
// THE cross-PR duplicate regression — the ticket's whole point. The same finding
// on the staging and production release PRs carries a DIFFERENT GitHub `id` and a
// RE-PARAPHRASED headline, but identical path/line/side. It must collapse to ONE
// cluster, therefore one stub.
// ---------------------------------------------------------------------------

test('cross-PR duplicate: different ids + re-paraphrased headlines collapse to ONE cluster', () => {
  const staging = comment({
    id: 111,
    html_url: 'https://github.com/o/r/pull/1735#discussion_r111',
    severity: 'P1',
    headline: 'Wire the capability profile into the production launcher',
  })
  const production = comment({
    id: 222,
    html_url: 'https://github.com/o/r/pull/1736#discussion_r222',
    severity: 'P1',
    headline: 'Wire the capability profile into production launches',
  })

  const clusters = clusterComments([staging, production])
  assert.equal(clusters.length, 1, 'the staging/production pair must be ONE cluster')
  assert.equal(clusters[0].key, 'services/bossd/internal/accountwiring/adapters.go@252')
  assert.equal(clusters[0].findings.length, 2)

  // …and therefore exactly one stub.
  const out = selectStub({ comments: [staging, production], trackedMarkers: [] })
  assert.ok(out.toFile)
  assert.equal(out.toFile.marker, 'Review: services/bossd/internal/accountwiring/adapters.go@252')
  assert.deepEqual(out.deferred, [])
  assert.deepEqual(out.dropped, [])
})

test('cross-PR duplicate: the stub records BOTH release PRs it was seen on', () => {
  const out = selectStub({
    comments: [
      comment({ id: 111, html_url: 'https://github.com/o/r/pull/1735#discussion_r111' }),
      comment({ id: 222, html_url: 'https://github.com/o/r/pull/1736#discussion_r222' }),
    ],
    trackedMarkers: [],
  })
  assert.match(out.toFile.body, /pull\/1735/)
  assert.match(out.toFile.body, /pull\/1736/)
})

// ---------------------------------------------------------------------------
// Same-anchor multi-finding — two genuinely DISTINCT findings at one anchor
// become one cluster whose rendered body carries BOTH bodies (nothing lost).
// ---------------------------------------------------------------------------

test('same-anchor multi-finding: one cluster whose body carries BOTH comment bodies', () => {
  const first = comment({
    id: 301,
    body: `${badgeLine('P1', 'Profile is dropped on respawn')}\n\nRESPAWN-LOSES-PROFILE`,
  })
  const second = comment({
    id: 302,
    body: `${badgeLine('P1', 'Credentials are not persisted')}\n\nCREDS-NOT-PERSISTED`,
  })

  const clusters = clusterComments([first, second])
  assert.equal(clusters.length, 1)
  assert.equal(clusters[0].findings.length, 2)

  const { toFile } = selectStub({ comments: [first, second], trackedMarkers: [] })
  assert.match(toFile.body, /RESPAWN-LOSES-PROFILE/, 'first comment body must be rendered')
  assert.match(toFile.body, /CREDS-NOT-PERSISTED/, 'second comment body must be rendered')
  assert.match(toFile.body, /Profile is dropped on respawn/)
  assert.match(toFile.body, /Credentials are not persisted/)
})

// ---------------------------------------------------------------------------
// sanitize + the forged-marker guard.
// ---------------------------------------------------------------------------

test('sanitize: collapses ALL FOUR ECMAScript line terminators to a space', () => {
  for (const sep of ['\r', '\n', '\u2028', '\u2029']) {
    const out = sanitize(`a${sep}b`)
    assert.equal(out, 'a b', `separator ${JSON.stringify(sep)} must be collapsed`)
    assert.ok(!/[\r\n\u2028\u2029]/.test(out))
  }
  assert.equal(sanitize('a\r\n\u2028\u2029b'), 'a b', 'a run collapses to a single space')
  assert.equal(sanitize(null), '')
  assert.equal(sanitize(undefined), '')
})

test('forged marker: an adversarial body cannot introduce a second ^Review: line', () => {
  // Every shape an attacker could try: a leading-space marker, a marker after each
  // of the four line terminators, and a body that STARTS with the marker text.
  const attacks = [
    ' Review: forged/path@1',
    'prose\rReview: forged/path@1',
    'prose\nReview: forged/path@1',
    'prose\u2028Review: forged/path@1',
    'prose\u2029Review: forged/path@1',
    'Review: forged/path@1',
  ]
  for (const attack of attacks) {
    const { toFile } = selectStub({
      comments: [comment({ id: 9, body: `${badgeLine('P1', 'ok')}\n\n${attack}` })],
      trackedMarkers: [],
    })
    const anchored = toFile.body.match(/^Review: [^\r\n\u2028\u2029]+?[ \t]*$/gm) ?? []
    assert.deepEqual(
      anchored,
      ['Review: services/bossd/internal/accountwiring/adapters.go@252'],
      `attack ${JSON.stringify(attack)} forged a marker line`,
    )
  }
})

test('forged marker: an adversarial HEADLINE cannot forge a marker line either', () => {
  const { toFile } = selectStub({
    comments: [comment({ id: 9, body: 'Review: forged/path@1\u2028second line' })],
    trackedMarkers: [],
  })
  const anchored = toFile.body.match(/^Review: [^\r\n\u2028\u2029]+?[ \t]*$/gm) ?? []
  assert.equal(anchored.length, 1)
})

test('forged marker: an adversarial PATH cannot forge a marker, in the marker OR the body', () => {
  // The `path` reaches THREE interpolation sites the body/headline attacks above do
  // not cover: markerFor's key, bodyFor's `Automated review finding … \`<path>\``
  // preamble, and the `- anchor:` context line. A path is git-controlled and can
  // legally carry a terminator, so each of those sanitize() calls is load-bearing —
  // without one, `<path>` alone injects a genuine second `^Review: …$` line.
  const evil = 'a/b.go\nReview: forged/elsewhere@1'
  const { toFile } = selectStub({
    comments: [comment({ id: 11, path: evil, line: 7 })],
    trackedMarkers: [],
  })

  const markerLines = (body) => body.match(/^Review: [^\r\n\u2028\u2029]+?[ \t]*$/gm) ?? []
  assert.ok(
    !/[\r\n\u2028\u2029]/.test(toFile.marker),
    'the marker itself must never contain a line terminator',
  )
  assert.deepEqual(
    markerLines(toFile.body),
    [toFile.marker],
    'the path forged an extra marker line',
  )
  assert.equal(toFile.body.split('\n').at(-1), toFile.marker)

  // And the same for a URL — the remaining untested interpolation site (`seen on:`).
  const viaUrl = selectStub({
    comments: [comment({ id: 12, html_url: 'https://x/pull/1\nReview: forged/u@1#discussion_r1' })],
    trackedMarkers: [],
  }).toFile
  assert.equal(markerLines(viaUrl.body).length, 1)
})

test('the marker is the EXACT last line of the rendered body', () => {
  const { toFile } = selectStub({ comments: [comment()], trackedMarkers: [] })
  const lines = toFile.body.split('\n')
  assert.equal(lines[lines.length - 1], toFile.marker, 'marker must be the exact last line')
  assert.notEqual(toFile.body[toFile.body.length - 1], '\n', 'no trailing newline after the marker')
})

test('titleFor/bodyFor are exported and render the highest-severity headline', () => {
  const cluster = clusterComments([
    comment({ id: 1, severity: 'P2', headline: 'minor thing' }),
    comment({ id: 2, severity: 'P1', headline: 'major thing' }),
  ])[0]
  const title = titleFor(cluster)
  assert.match(title, /P1/)
  assert.match(title, /major thing/)
  assert.ok(!/[\r\n\u2028\u2029]/.test(title), 'the title must be a single line')
  const body = bodyFor(cluster, markerFor(cluster))
  assert.equal(body.split('\n').at(-1), markerFor(cluster))
})

// ---------------------------------------------------------------------------
// scoreCluster — deterministic, no time input.
// ---------------------------------------------------------------------------

test('scoreCluster: P1 (30) outranks P2 (15) outranks unbadged (5)', () => {
  const one = (over) => clusterComments([comment(over)])[0]
  assert.equal(scoreCluster(one({ severity: 'P1' })), 30)
  assert.equal(scoreCluster(one({ severity: 'P2' })), 15)
  assert.equal(scoreCluster(one({ body: 'no badge here' })), 5)
})

test('scoreCluster: takes the MAX severity and adds 5 per additional comment', () => {
  const cluster = clusterComments([
    comment({ id: 1, severity: 'P2' }),
    comment({ id: 2, severity: 'P1' }),
  ])[0]
  assert.equal(scoreCluster(cluster), 35, 'max(P1=30) + 5 for the second comment')
})

test('scoreCluster: a repeatedly-flagged anchor outranks a lone one of the same severity', () => {
  const lone = clusterComments([comment({ id: 1, severity: 'P2' })])[0]
  const twice = clusterComments([
    comment({ id: 1, severity: 'P2' }),
    comment({ id: 2, severity: 'P2' }),
  ])[0]
  assert.ok(scoreCluster(twice) > scoreCluster(lone))
})

test('scoreCluster: an empty / malformed cluster scores 0 without throwing', () => {
  assert.equal(scoreCluster(null), 0)
  assert.equal(scoreCluster({}), 0)
  assert.equal(scoreCluster({ findings: [] }), 0)
})

// ---------------------------------------------------------------------------
// selectStub — a reason for EVERY input cluster; nothing silently dropped.
// ---------------------------------------------------------------------------

const at = (path, line, over = {}) => comment({ path, line, ...over })

test('selectStub: ordering is score desc, tie-broken by key asc', () => {
  const out = selectStub({
    comments: [at('b.go', 1, { id: 1, severity: 'P2' }), at('a.go', 1, { id: 2, severity: 'P1' })],
    trackedMarkers: [],
  })
  assert.equal(out.toFile.marker, 'Review: a.go@1')
  assert.deepEqual(
    out.deferred.map((d) => d.marker),
    ['Review: b.go@1'],
  )
})

test('selectStub: equal scores tie-break on key ascending, deterministically', () => {
  const comments = [at('z.go', 1, { id: 1 }), at('a.go', 1, { id: 2 }), at('m.go', 1, { id: 3 })]
  for (const order of [comments, [...comments].reverse()]) {
    const out = selectStub({ comments: order, trackedMarkers: [] })
    assert.equal(out.toFile.marker, 'Review: a.go@1')
    assert.deepEqual(
      out.deferred.map((d) => d.marker),
      ['Review: m.go@1', 'Review: z.go@1'],
    )
  }
})

test('selectStub: EVERY non-filed cluster carries a reason — nothing silently dropped', () => {
  const comments = [
    at('a.go', 1, { id: 1, severity: 'P1' }),
    at('b.go', 2, { id: 2, severity: 'P1' }),
    at('c.go', 3, { id: 3, severity: 'P1' }),
    at('d.go', 4, { id: 4, body: 'unbadged and cheap' }),
    at('e.go', 5, { id: 5, severity: 'P1' }),
  ]
  const out = selectStub({
    comments,
    trackedMarkers: ['Review: e.go@5'],
    threshold: 10,
    cap: 1,
  })
  const accounted = [...out.deferred, ...out.dropped]
  assert.equal(
    accounted.length + (out.toFile ? 1 : 0),
    5,
    'every input cluster must be accounted for',
  )
  for (const entry of accounted) {
    assert.ok(
      ['already-tracked', 'below-threshold', 'over-cap'].includes(entry.reason),
      `unexpected reason ${entry.reason}`,
    )
    assert.equal(typeof entry.marker, 'string')
    assert.equal(typeof entry.score, 'number')
  }
  const byMarker = Object.fromEntries(accounted.map((e) => [e.marker, e.reason]))
  assert.equal(byMarker['Review: e.go@5'], 'already-tracked')
  assert.equal(byMarker['Review: d.go@4'], 'below-threshold')
  // cap=1 → exactly one deferred; the remaining eligible cluster is dropped over-cap.
  assert.equal(out.deferred.length, 1)
  assert.equal(out.deferred[0].reason, 'over-cap')
  assert.ok(out.dropped.some((d) => d.reason === 'over-cap'))
})

test('selectStub: an already-tracked cluster is dropped and the next one is filed', () => {
  const out = selectStub({
    comments: [at('a.go', 1, { id: 1, severity: 'P1' }), at('b.go', 2, { id: 2, severity: 'P2' })],
    trackedMarkers: ['Review: a.go@1'],
  })
  assert.equal(out.toFile.marker, 'Review: b.go@2')
  assert.deepEqual(
    out.dropped.map((d) => [d.marker, d.reason]),
    [['Review: a.go@1', 'already-tracked']],
  )
})

test('selectStub: all clusters tracked → toFile null (nothing to create)', () => {
  const out = selectStub({
    comments: [at('a.go', 1, { id: 1 }), at('b.go', 2, { id: 2 })],
    trackedMarkers: new Set(['Review: a.go@1', 'Review: b.go@2']),
  })
  assert.equal(out.toFile, null)
  assert.equal(out.dropped.length, 2)
  assert.ok(out.dropped.every((d) => d.reason === 'already-tracked'))
})

test('selectStub: trackedMarkers accepts a Set, an array, or nothing', () => {
  for (const tracked of [undefined, null, [], new Set()]) {
    const out = selectStub({ comments: [at('a.go', 1)], trackedMarkers: tracked })
    assert.equal(out.toFile.marker, 'Review: a.go@1')
  }
})

test('selectStub: no comments → toFile null and empty partitions', () => {
  for (const input of [undefined, {}, { comments: [] }, { comments: 'nope' }]) {
    const out = selectStub(input)
    assert.equal(out.toFile, null)
    assert.deepEqual(out.deferred, [])
    assert.deepEqual(out.dropped, [])
  }
})

test('selectStub: non-object comments and path-less comments are sanitized out', () => {
  const out = selectStub({
    comments: [null, 42, 'str', {}, comment({ path: null, id: 7 }), at('a.go', 1, { id: 8 })],
    trackedMarkers: [],
  })
  assert.equal(out.toFile.marker, 'Review: a.go@1')
  assert.deepEqual(out.deferred, [])
  assert.deepEqual(out.dropped, [], 'sanitized-out payloads never appear as reasoned drops')
})

test('selectStub: the filed stub exposes key, marker, title, body, score and count', () => {
  const out = selectStub({ comments: [at('a.go', 1, { id: 1 }), at('a.go', 1, { id: 2 })] })
  assert.equal(out.toFile.key, 'a.go@1')
  assert.equal(out.toFile.marker, 'Review: a.go@1')
  assert.ok(out.toFile.title.length > 0)
  assert.ok(out.toFile.body.length > 0)
  assert.equal(out.toFile.count, 2)
  assert.equal(typeof out.toFile.score, 'number')
})

// ---------------------------------------------------------------------------
// Scoping constants — the gate filters to the release PRs + the review bot.
// ---------------------------------------------------------------------------

test('scoping: the review bot login and release base branches are pinned', () => {
  assert.equal(REVIEW_BOT_LOGIN, 'chatgpt-codex-connector[bot]')
  assert.deepEqual(RELEASE_BASE_BRANCHES, ['staging', 'production'])
})

test('isReviewBotComment: only the review bot qualifies', () => {
  assert.ok(isReviewBotComment(comment()))
  assert.ok(!isReviewBotComment(comment({ login: 'dave' })))
  assert.ok(!isReviewBotComment(null))
  assert.ok(!isReviewBotComment({}))
})

// ---------------------------------------------------------------------------
// fetchTrackedReviewMarkers — the READ half of dedupe, over an injected
// linearRequest (no network). Reuses fetchMarkedLinearIssues, so the fail-closed
// truncation throw must propagate.
// ---------------------------------------------------------------------------

const onePage = (nodes) => async () => ({
  issues: { nodes, pageInfo: { hasNextPage: false, endCursor: null } },
})

test('fetchTrackedReviewMarkers: lifts exactly the `Review: ` marker lines', async () => {
  const markers = await fetchTrackedReviewMarkers({
    apiKey: 'k',
    linearRequest: onePage([
      { identifier: 'BOS-1', description: 'body\n\nReview: a/b.go@12   \t' },
      { identifier: 'BOS-2', description: 'Review: c/d.go@3\n\ntrailing prose' },
      { identifier: 'BOS-3', description: 'no marker at all' },
      { identifier: 'BOS-4', description: 'see Review: e/f.go@9 inline only' },
      { identifier: 'BOS-5', description: 'MainGate: gosec/services/bossd' },
    ]),
  })
  assert.deepEqual([...markers].sort(), ['Review: a/b.go@12', 'Review: c/d.go@3'])
  assert.ok(markers.has(markerFor('a/b.go@12')), 'round-trips with markerFor')
})

test('fetchTrackedReviewMarkers: dedupes repeated markers into a Set', async () => {
  const markers = await fetchTrackedReviewMarkers({
    apiKey: 'k',
    linearRequest: onePage([
      { identifier: 'BOS-1', description: 'Review: a/b.go@12' },
      { identifier: 'BOS-2', description: 'dup\nReview: a/b.go@12' },
    ]),
  })
  assert.deepEqual([...markers], ['Review: a/b.go@12'])
})

test('fetchTrackedReviewMarkers: propagates the fail-closed truncation throw', async () => {
  // hasNextPage never clears → the shared scan throws at the page bound, so a
  // PARTIAL dedupe snapshot is never handed to selectStub.
  const everMore = async () => ({
    issues: { nodes: [], pageInfo: { hasNextPage: true, endCursor: 'c' } },
  })
  await assert.rejects(
    () => fetchTrackedReviewMarkers({ apiKey: 'k', linearRequest: everMore, maxPages: 2 }),
    /incomplete|exceeded/i,
  )
})

test('fetchTrackedReviewMarkers: requires a linearRequest implementation (fail-closed)', async () => {
  await assert.rejects(() => fetchTrackedReviewMarkers({ apiKey: 'k' }), /linearRequest/)
})

test('fetchTrackedReviewMarkers: scans under the `Review: ` prefix, all states + archived', async () => {
  let seen = null
  await fetchTrackedReviewMarkers({
    apiKey: 'k',
    linearRequest: async (args) => {
      seen = args
      return { issues: { nodes: [], pageInfo: { hasNextPage: false } } }
    },
  })
  assert.deepEqual(seen.variables.filter, { description: { contains: 'Review: ' } })
  assert.match(seen.query, /includeArchived: true/)
})

// ---------------------------------------------------------------------------
// runCli — the `select` shell-out seam the SKILL + gate use. Injected readFile.
// ---------------------------------------------------------------------------

test('runCli select: round-trips through an injected readFile with no real I/O', () => {
  const files = {
    'comments.json': JSON.stringify([
      at('a.go', 1, { id: 1, severity: 'P1' }),
      at('b.go', 2, { id: 2, severity: 'P2' }),
    ]),
    'tracked.json': JSON.stringify(['Review: a.go@1']),
  }
  const out = JSON.parse(
    runCli(['select', 'comments.json', 'tracked.json'], { readFile: (f) => files[f] }),
  )
  assert.equal(out.toFile.marker, 'Review: b.go@2')
  assert.deepEqual(
    out.dropped.map((d) => [d.marker, d.reason]),
    [['Review: a.go@1', 'already-tracked']],
  )
})

test('runCli select: a missing tracked arg defaults to an empty tracked set', () => {
  const files = { 'comments.json': JSON.stringify([at('a.go', 1, { id: 1 })]) }
  const out = JSON.parse(runCli(['select', 'comments.json'], { readFile: (f) => files[f] }))
  assert.equal(out.toFile.marker, 'Review: a.go@1')
})

test('runCli select: threshold and cap are overridable positionally', () => {
  const files = {
    'comments.json': JSON.stringify([
      at('a.go', 1, { id: 1, body: 'unbadged' }),
      at('b.go', 2, { id: 2, body: 'unbadged' }),
    ]),
    'tracked.json': '[]',
  }
  const out = JSON.parse(
    runCli(['select', 'comments.json', 'tracked.json', '10', '0'], { readFile: (f) => files[f] }),
  )
  assert.equal(out.toFile, null, 'threshold 10 drops both unbadged (score 5) clusters')
  assert.ok(out.dropped.every((d) => d.reason === 'below-threshold'))
})

test('runCli: an unknown subcommand throws', () => {
  assert.throws(() => runCli(['bogus'], { readFile: () => '[]' }), /unknown subcommand/)
})
