// scripts/sweep-sentry-gate.test.mjs
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { scoreIssue, dedupeAgainstIssues, selectTop } from './sweep-sentry-gate.mjs'

const HOUR = 60 * 60 * 1000
const DAY = 24 * HOUR
const NOW = Date.parse('2026-07-12T00:00:00Z')

// Factory mirrors the live Sentry issue shape. `count` is a STRING, like the real API.
const mkIssue = (over = {}) => ({
  shortId: over.shortId ?? 'BOSS-1',
  title: over.title ?? 'Something broke',
  level: over.level ?? 'error',
  count: over.count ?? '10',
  userCount: over.userCount ?? 5,
  isUnhandled: over.isUnhandled ?? false,
  lastSeen: over.lastSeen ?? new Date(NOW - HOUR).toISOString(),
  permalink: over.permalink ?? 'https://sentry.io/organizations/acme/issues/1/',
})

// Default mkIssue() score, per the documented weight table:
// level error=30, count '10' (tier >=10)=10, userCount 5 (tier >=2)=10,
// isUnhandled false=0, lastSeen 1h ago (within 24h)=10 → 60.
const DEFAULT_SCORE = 60

test('scoreIssue: default mkIssue scores per the documented weight table', () => {
  assert.equal(scoreIssue(mkIssue(), { now: NOW }), DEFAULT_SCORE)
})

test('scoreIssue: fatal > error > warning at equal other signals', () => {
  const fatal = scoreIssue(mkIssue({ level: 'fatal' }), { now: NOW })
  const error = scoreIssue(mkIssue({ level: 'error' }), { now: NOW })
  const warning = scoreIssue(mkIssue({ level: 'warning' }), { now: NOW })
  assert.ok(fatal > error, `${fatal} > ${error}`)
  assert.ok(error > warning, `${error} > ${warning}`)
})

test('scoreIssue: higher event count (string-coerced) scores higher', () => {
  const high = scoreIssue(mkIssue({ count: '1000' }), { now: NOW })
  const low = scoreIssue(mkIssue({ count: '1' }), { now: NOW })
  assert.ok(high > low, `${high} > ${low}`)
})

test('scoreIssue: higher userCount scores higher', () => {
  const high = scoreIssue(mkIssue({ userCount: 100 }), { now: NOW })
  const low = scoreIssue(mkIssue({ userCount: 0 }), { now: NOW })
  assert.ok(high > low, `${high} > ${low}`)
})

test('scoreIssue: isUnhandled true beats false', () => {
  const unhandled = scoreIssue(mkIssue({ isUnhandled: true }), { now: NOW })
  const handled = scoreIssue(mkIssue({ isUnhandled: false }), { now: NOW })
  assert.ok(unhandled > handled, `${unhandled} > ${handled}`)
})

test('scoreIssue: lastSeen 1h ago beats 30 days ago (injected now)', () => {
  const recent = scoreIssue(mkIssue({ lastSeen: new Date(NOW - HOUR).toISOString() }), {
    now: NOW,
  })
  const stale = scoreIssue(mkIssue({ lastSeen: new Date(NOW - 30 * DAY).toISOString() }), {
    now: NOW,
  })
  assert.ok(recent > stale, `${recent} > ${stale}`)
})

test('scoreIssue: tolerates malformed input without throwing', () => {
  assert.equal(typeof scoreIssue(null, { now: NOW }), 'number')
  assert.equal(typeof scoreIssue(undefined, { now: NOW }), 'number')
  assert.equal(typeof scoreIssue({}, { now: NOW }), 'number')
  assert.doesNotThrow(() => scoreIssue(null, { now: NOW }))
})

test('scoreIssue: non-numeric count contributes 0 to the volume tier', () => {
  // '1' also falls in the "else 0" tier (n < 2), so these two must match.
  const bogus = scoreIssue(mkIssue({ count: 'not-a-number' }), { now: NOW })
  const zeroTier = scoreIssue(mkIssue({ count: '1' }), { now: NOW })
  assert.equal(bogus, zeroTier)
})

// append to scripts/sweep-sentry-gate.test.mjs
test('selectTop: threshold boundary — score exactly at threshold is eligible (>=)', () => {
  // level error(30) + count '2' (tier >=2)=5 + userCount 1 (tier >=1)=5
  // + isUnhandled false=0 + lastSeen 30d ago (stale)=0 → exactly 40.
  const boundary = mkIssue({
    shortId: 'BOSS-BOUNDARY',
    level: 'error',
    count: '2',
    userCount: 1,
    isUnhandled: false,
    lastSeen: new Date(NOW - 30 * DAY).toISOString(),
  })
  assert.equal(scoreIssue(boundary, { now: NOW }), 40)
  const out = selectTop([boundary], [], { now: NOW })
  assert.equal(out.top?.issue.shortId, 'BOSS-BOUNDARY')
  assert.deepEqual(out.dropped, [])
})

test('selectTop: below-threshold issue is dropped with reason below-threshold', () => {
  // level error(30) + count '2' (tier >=2)=5 + userCount 0=0 + unhandled false=0
  // + stale lastSeen=0 → 35, below the default threshold of 40.
  const low = mkIssue({
    shortId: 'BOSS-LOW',
    level: 'error',
    count: '2',
    userCount: 0,
    isUnhandled: false,
    lastSeen: new Date(NOW - 30 * DAY).toISOString(),
  })
  assert.equal(scoreIssue(low, { now: NOW }), 35)
  const out = selectTop([low], [], { now: NOW })
  assert.equal(out.top, null)
  assert.deepEqual(out.dropped, [
    { shortId: 'BOSS-LOW', title: low.title, score: 35, reason: 'below-threshold' },
  ])
})

test('selectTop: cap + reasons — top/deferred/dropped account for every eligible issue', () => {
  const a = mkIssue({
    shortId: 'BOSS-A',
    level: 'fatal',
    count: '1000',
    userCount: 100,
    isUnhandled: true,
    lastSeen: new Date(NOW - HOUR).toISOString(),
  }) // score 100
  const b = mkIssue({
    shortId: 'BOSS-B',
    level: 'error',
    count: '100',
    userCount: 10,
    isUnhandled: true,
    lastSeen: new Date(NOW - HOUR).toISOString(),
  }) // score 80
  const c = mkIssue({
    shortId: 'BOSS-C',
    level: 'warning',
    count: '10',
    userCount: 2,
    isUnhandled: false,
    lastSeen: new Date(NOW - 2 * DAY).toISOString(),
  }) // score 40
  assert.equal(scoreIssue(a, { now: NOW }), 100)
  assert.equal(scoreIssue(b, { now: NOW }), 80)
  assert.equal(scoreIssue(c, { now: NOW }), 40)

  const out = selectTop([a, b, c], [], { cap: 1, now: NOW })
  assert.equal(out.top.issue.shortId, 'BOSS-A')
  assert.deepEqual(out.deferred, [
    { shortId: 'BOSS-B', title: b.title, score: 80, reason: 'over-cap' },
  ])
  assert.deepEqual(out.dropped, [
    { shortId: 'BOSS-C', title: c.title, score: 40, reason: 'over-cap' },
  ])
  // Nothing silently missing: top + deferred + dropped accounts for all 3 inputs.
  assert.equal(1 + out.deferred.length + out.dropped.length, 3)
})

test('selectTop: top is the highest scorer with deterministic shortId tie-break', () => {
  const z = mkIssue({ shortId: 'BOSS-Z' }) // score 60 (default)
  const a = mkIssue({ shortId: 'BOSS-A' }) // score 60 (default, tied)
  const out = selectTop([z, a], [], { now: NOW })
  assert.equal(out.top.issue.shortId, 'BOSS-A')
  assert.deepEqual(out.deferred, [
    { shortId: 'BOSS-Z', title: z.title, score: DEFAULT_SCORE, reason: 'over-cap' },
  ])
})

// append to scripts/sweep-sentry-gate.test.mjs
test('dedupeAgainstIssues: drops a Sentry issue with a matching marker line', () => {
  const boss1 = mkIssue({ shortId: 'BOSS-1' })
  const linear = [{ description: 'tracking this.\n\nSentry: BOSS-1\n' }]
  assert.deepEqual(dedupeAgainstIssues([boss1], linear), [])
})

test('dedupeAgainstIssues: marker for BOSS-12 does not dedupe BOSS-1 (line-anchor)', () => {
  const boss1 = mkIssue({ shortId: 'BOSS-1' })
  const linear = [{ description: 'Sentry: BOSS-12' }]
  assert.deepEqual(dedupeAgainstIssues([boss1], linear), [boss1])
})

test('dedupeAgainstIssues: inline non-line occurrence does not match', () => {
  const boss1 = mkIssue({ shortId: 'BOSS-1' })
  const linear = [{ description: 'see Sentry: BOSS-1 for details' }]
  assert.deepEqual(dedupeAgainstIssues([boss1], linear), [boss1])
})

test('dedupeAgainstIssues: tolerates null/missing descriptions', () => {
  const boss1 = mkIssue({ shortId: 'BOSS-1' })
  const linear = [{ description: null }, {}]
  assert.deepEqual(dedupeAgainstIssues([boss1], linear), [boss1])
})

test('selectTop: already-tracked issue is dropped with reason already-tracked', () => {
  const boss1 = mkIssue({ shortId: 'BOSS-1' })
  const linear = [{ description: 'Sentry: BOSS-1' }]
  const out = selectTop([boss1], linear, { now: NOW })
  assert.equal(out.top, null)
  assert.deepEqual(out.dropped, [
    { shortId: 'BOSS-1', title: boss1.title, score: DEFAULT_SCORE, reason: 'already-tracked' },
  ])
})

// append to scripts/sweep-sentry-gate.test.mjs
test('selectTop: malformed payload never throws and yields no top', () => {
  assert.doesNotThrow(() => selectTop(null, []))
  assert.doesNotThrow(() => selectTop('garbage', []))
  assert.doesNotThrow(() => selectTop([{}, 42, null], []))
  assert.equal(selectTop(null, []).top, null)
  assert.equal(selectTop('garbage', []).top, null)
  assert.equal(selectTop([{}, 42, null], []).top, null)
})

test('selectTop: non-object entries and entries lacking a string shortId are absent from output', () => {
  const out = selectTop([{}, 42, null, mkIssue({ shortId: 'BOSS-OK' })], [], { now: NOW })
  const allShortIds = [
    out.top?.issue?.shortId,
    ...out.deferred.map((d) => d.shortId),
    ...out.dropped.map((d) => d.shortId),
  ].filter(Boolean)
  assert.deepEqual(allShortIds, ['BOSS-OK'])
})

// append to scripts/sweep-sentry-gate.test.mjs
import { fetchSentryIssues } from './sweep-sentry-gate.mjs'

test('fetchSentryIssues: 200 resolves to the parsed array with correct URL and header', async () => {
  let seenUrl
  let seenHeaders
  const fetchImpl = async (url, opts) => {
    seenUrl = url
    seenHeaders = opts.headers
    return { ok: true, status: 200, json: async () => [mkIssue()] }
  }
  const result = await fetchSentryIssues({
    token: 'tok123',
    org: 'acme',
    query: 'is:unresolved',
    fetchImpl,
  })
  assert.equal(
    seenUrl,
    'https://sentry.io/api/0/organizations/acme/issues/?query=is%3Aunresolved&limit=100',
  )
  assert.equal(seenHeaders.Authorization, 'Bearer tok123')
  assert.equal(result.length, 1)
})

test('fetchSentryIssues: follows Link rel="next" results="true" and concatenates pages', async () => {
  const page2 = 'https://sentry.io/api/0/organizations/acme/issues/?cursor=100:1:0'
  const urls = []
  const fetchImpl = async (url) => {
    urls.push(url)
    if (urls.length === 1) {
      return {
        ok: true,
        status: 200,
        headers: { get: () => `<${page2}>; rel="next"; results="true"; cursor="100:1:0"` },
        json: async () => [mkIssue({ shortId: 'BOSS-P1' })],
      }
    }
    return {
      ok: true,
      status: 200,
      headers: { get: () => `<x>; rel="next"; results="false"; cursor="100:2:0"` },
      json: async () => [mkIssue({ shortId: 'BOSS-P2' })],
    }
  }
  const result = await fetchSentryIssues({ token: 't', org: 'acme', fetchImpl })
  assert.equal(urls.length, 2)
  assert.equal(urls[1], page2)
  assert.deepEqual(
    result.map((i) => i.shortId),
    ['BOSS-P1', 'BOSS-P2'],
  )
})

test('fetchSentryIssues: stops after a single page when the Link header reports no more results', async () => {
  let calls = 0
  const fetchImpl = async () => {
    calls++
    return {
      ok: true,
      status: 200,
      headers: { get: () => '<x>; rel="next"; results="false"; cursor="0:100:0"' },
      json: async () => [mkIssue()],
    }
  }
  const result = await fetchSentryIssues({ token: 't', org: 'acme', fetchImpl })
  assert.equal(calls, 1)
  assert.equal(result.length, 1)
})

test('fetchSentryIssues: pagination is bounded by maxPages even when more pages exist', async () => {
  let calls = 0
  const fetchImpl = async () => {
    calls++
    return {
      ok: true,
      status: 200,
      headers: { get: () => `<next-${calls}>; rel="next"; results="true"; cursor="c"` },
      json: async () => [mkIssue({ shortId: `BOSS-${calls}` })],
    }
  }
  const result = await fetchSentryIssues({ token: 't', org: 'acme', fetchImpl, maxPages: 2 })
  assert.equal(calls, 2)
  assert.equal(result.length, 2)
})

test('fetchSentryIssues: 401 rejects with HTTP status, scope hint, and err.status', async () => {
  const fetchImpl = async () => ({ ok: false, status: 401, json: async () => ({}) })
  await assert.rejects(fetchSentryIssues({ token: 't', org: 'acme', fetchImpl }), (err) => {
    assert.match(err.message, /Sentry API HTTP 401/)
    assert.match(err.message, /event:read/)
    assert.equal(err.status, 401)
    return true
  })
})

test('fetchSentryIssues: 404 rejects with org-slug hint and err.status', async () => {
  const fetchImpl = async () => ({ ok: false, status: 404, json: async () => ({}) })
  await assert.rejects(fetchSentryIssues({ token: 't', org: 'acme', fetchImpl }), (err) => {
    assert.match(err.message, /org slug/)
    assert.equal(err.status, 404)
    return true
  })
})

test('fetchSentryIssues: missing token/org reject without calling fetch', async () => {
  let called = false
  const fetchImpl = async () => {
    called = true
    return { ok: true, status: 200, json: async () => [] }
  }
  await assert.rejects(
    fetchSentryIssues({ token: '', org: 'acme', fetchImpl }),
    /^Error: SENTRY_API_KEY is not set$/,
  )
  await assert.rejects(
    fetchSentryIssues({ token: 'tok', org: '', fetchImpl }),
    /^Error: SENTRY_ORG is not set$/,
  )
  assert.equal(called, false)
})

// append to scripts/sweep-sentry-gate.test.mjs
import { fetchMarkedLinearIssues, MARKED_ISSUES_QUERY } from './sweep-sentry-gate.mjs'

test('fetchMarkedLinearIssues: follows cursor pagination and concatenates all pages', async () => {
  const calls = []
  const linearRequest = async ({ apiKey, query, variables }) => {
    calls.push({ apiKey, query, variables })
    if (variables.after === null) {
      return {
        issues: {
          nodes: [{ identifier: 'BOS-1', description: 'Sentry: BOSS-1' }],
          pageInfo: { hasNextPage: true, endCursor: 'c1' },
        },
      }
    }
    return {
      issues: {
        nodes: [{ identifier: 'BOS-2', description: 'Sentry: BOSS-2' }],
        pageInfo: { hasNextPage: false, endCursor: null },
      },
    }
  }
  const nodes = await fetchMarkedLinearIssues({ apiKey: 'lk', linearRequest })
  assert.equal(calls.length, 2)
  assert.equal(calls[0].apiKey, 'lk')
  assert.equal(calls[0].query, MARKED_ISSUES_QUERY)
  assert.deepEqual(calls[0].variables.filter, { description: { contains: 'Sentry: ' } })
  assert.equal(calls[1].variables.after, 'c1')
  assert.deepEqual(
    nodes.map((n) => n.identifier),
    ['BOS-1', 'BOS-2'],
  )
})

test('fetchMarkedLinearIssues: empty workspace resolves to []', async () => {
  const linearRequest = async () => ({
    issues: { nodes: [], pageInfo: { hasNextPage: false, endCursor: null } },
  })
  assert.deepEqual(await fetchMarkedLinearIssues({ apiKey: 'lk', linearRequest }), [])
})

test('fetchMarkedLinearIssues: throws fail-closed when the page bound is hit with pages left', async () => {
  let calls = 0
  const linearRequest = async () => {
    calls++
    return {
      issues: {
        nodes: [{ identifier: `BOS-${calls}`, description: 'Sentry: X' }],
        pageInfo: { hasNextPage: true, endCursor: `c${calls}` },
      },
    }
  }
  await assert.rejects(
    fetchMarkedLinearIssues({ apiKey: 'lk', linearRequest, maxPages: 3 }),
    /exceeded 3 pages — dedupe snapshot incomplete/,
  )
  assert.equal(calls, 3)
})

test('fetchMarkedLinearIssues: throws on a response without an issues connection', async () => {
  const linearRequest = async () => ({ somethingElse: true })
  await assert.rejects(
    fetchMarkedLinearIssues({ apiKey: 'lk', linearRequest }),
    /no issues connection/,
  )
})

test('fetchMarkedLinearIssues: requires an injected linearRequest', async () => {
  await assert.rejects(fetchMarkedLinearIssues({ apiKey: 'lk' }), /requires a linearRequest/)
})

// append to scripts/sweep-sentry-gate.test.mjs
import { runCli } from './sweep-sentry-gate.mjs'

test('runCli select-top reads files via injected reader and returns the right top shortId', () => {
  const files = {
    's.json': JSON.stringify([mkIssue({ shortId: 'BOSS-1' })]),
    'l.json': '[]',
  }
  const out = runCli(['select-top', 's.json', 'l.json'], { readFile: (f) => files[f] })
  const parsed = JSON.parse(out)
  assert.equal(parsed.top.issue.shortId, 'BOSS-1')
})

test('runCli dedupe prints surviving shortIds', () => {
  const files = {
    's.json': JSON.stringify([mkIssue({ shortId: 'BOSS-1' }), mkIssue({ shortId: 'BOSS-2' })]),
    'l.json': JSON.stringify([{ description: 'Sentry: BOSS-1' }]),
  }
  const out = runCli(['dedupe', 's.json', 'l.json'], { readFile: (f) => files[f] })
  assert.deepEqual(JSON.parse(out), ['BOSS-2'])
})

test('runCli throws on unknown subcommand', () => {
  assert.throws(() => runCli(['bogus'], {}))
})
