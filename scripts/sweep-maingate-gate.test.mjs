// scripts/sweep-maingate-gate.test.mjs
// Table-driven tests for the bs-sweep-security "main-gate health probe" pure core.
// The decision engine (selectStub) is I/O-free and deterministic; these tests pin
// the contract the SKILL.md Phase 1.5 depends on: one stub per run, priority
// (nosec-metadata before gosec, then module order), already-tracked dedupe by
// marker, all-green → null, and the marker being the exact last line of the body.
//
// Node built-ins only — cron worktrees are dependency-free.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import {
  selectStub,
  markerFor,
  fetchTrackedMainGateMarkers,
  runCli,
} from './sweep-maingate-gate.mjs'

// Factories mirror the compact per-check summaries the probe produces: the nosec
// check has no module; each gosec check carries its go.work module.
const nosec = (over = {}) => ({
  id: 'nosec-metadata',
  ok: over.ok ?? false,
  findings: over.findings ?? ['services/bossd/internal/db/x.go:12: // #nosec G204'],
})
const gosec = (module, over = {}) => ({
  id: 'gosec',
  module,
  ok: over.ok ?? false,
  findings: over.findings ?? [`${module}/y.go:3 [G115] integer overflow conversion`],
})

// ---------------------------------------------------------------------------
// markerFor — the line-anchored marker scheme.
// ---------------------------------------------------------------------------

test('markerFor: nosec-metadata check → "MainGate: nosec-metadata"', () => {
  assert.equal(markerFor(nosec()), 'MainGate: nosec-metadata')
})

test('markerFor: gosec check → "MainGate: gosec/<module>"', () => {
  assert.equal(markerFor(gosec('services/bossd')), 'MainGate: gosec/services/bossd')
})

// ---------------------------------------------------------------------------
// all-green → toFile: null (nothing to file).
// ---------------------------------------------------------------------------

test('selectStub: all checks ok → toFile null, no skips', () => {
  const out = selectStub({
    checks: [nosec({ ok: true }), gosec('services/bossd', { ok: true })],
    trackedMarkers: [],
  })
  assert.equal(out.toFile, null)
  assert.deepEqual(out.skipped, [])
})

test('selectStub: empty/absent checks → toFile null', () => {
  assert.equal(selectStub({ checks: [], trackedMarkers: [] }).toFile, null)
  assert.equal(selectStub({}).toFile, null)
  assert.equal(selectStub().toFile, null)
})

// ---------------------------------------------------------------------------
// priority — nosec-metadata wins over gosec regardless of input order.
// ---------------------------------------------------------------------------

test('selectStub: nosec-metadata is filed before gosec (priority)', () => {
  const out = selectStub({
    // gosec listed FIRST to prove category priority, not input order, decides.
    checks: [gosec('services/bossd'), nosec()],
    trackedMarkers: [],
  })
  assert.equal(out.toFile.marker, 'MainGate: nosec-metadata')
  // The gosec breakage is deferred (one stub per run), not dropped silently.
  assert.deepEqual(out.skipped, [{ marker: 'MainGate: gosec/services/bossd', reason: 'deferred' }])
})

// ---------------------------------------------------------------------------
// one stub per run — only the top breakage is filed; the rest are deferred.
// gosec ordering falls back to module (input) order once nosec is absent.
// ---------------------------------------------------------------------------

test('selectStub: one stub per run — first gosec module by input order wins', () => {
  const out = selectStub({
    checks: [gosec('services/bossd'), gosec('services/bosso'), gosec('lib/bossalib')],
    trackedMarkers: [],
  })
  assert.equal(out.toFile.marker, 'MainGate: gosec/services/bossd')
  assert.deepEqual(out.skipped, [
    { marker: 'MainGate: gosec/services/bosso', reason: 'deferred' },
    { marker: 'MainGate: gosec/lib/bossalib', reason: 'deferred' },
  ])
})

// ---------------------------------------------------------------------------
// already-tracked dedupe by marker — a breakage whose marker is already tracked
// in Linear is dropped, and the next-priority untracked breakage is filed.
// ---------------------------------------------------------------------------

test('selectStub: already-tracked top breakage is skipped, next untracked one is filed', () => {
  const out = selectStub({
    checks: [nosec(), gosec('services/bossd')],
    trackedMarkers: ['MainGate: nosec-metadata'],
  })
  assert.equal(out.toFile.marker, 'MainGate: gosec/services/bossd')
  assert.deepEqual(out.skipped, [{ marker: 'MainGate: nosec-metadata', reason: 'already-tracked' }])
})

test('selectStub: sole breakage already tracked → toFile null (deduped)', () => {
  const out = selectStub({
    checks: [gosec('services/bossd')],
    trackedMarkers: ['MainGate: gosec/services/bossd'],
  })
  assert.equal(out.toFile, null)
  assert.deepEqual(out.skipped, [
    { marker: 'MainGate: gosec/services/bossd', reason: 'already-tracked' },
  ])
})

test('selectStub: trackedMarkers accepts a Set as well as an array', () => {
  const out = selectStub({
    checks: [nosec()],
    trackedMarkers: new Set(['MainGate: nosec-metadata']),
  })
  assert.equal(out.toFile, null)
  assert.deepEqual(out.skipped, [{ marker: 'MainGate: nosec-metadata', reason: 'already-tracked' }])
})

// ---------------------------------------------------------------------------
// marker is the EXACT last line of the body — the dedupe key the next run keys off.
// ---------------------------------------------------------------------------

test('selectStub: the marker is the exact last line of the filed body', () => {
  for (const checks of [[nosec()], [gosec('services/bossd')]]) {
    const { toFile } = selectStub({ checks, trackedMarkers: [] })
    assert.ok(toFile, 'expected a stub to file')
    const lines = toFile.body.split('\n')
    assert.equal(lines[lines.length - 1], toFile.marker, 'marker must be the exact last line')
    // No trailing blank line after the marker.
    assert.notEqual(toFile.body[toFile.body.length - 1], '\n')
    // The marker also appears line-anchored (^MainGate: ...$) exactly once.
    const anchored = new RegExp(
      '^' + toFile.marker.replace(/[.*+?^${}()|[\]\\]/g, '\\$&') + '$',
      'm',
    )
    assert.ok(anchored.test(toFile.body), 'marker must be line-anchored in the body')
  }
})

test('selectStub: a finding with an embedded line terminator cannot forge a second marker', () => {
  // A crafted finding carrying a bare CR / U+2028 / U+2029 before "MainGate: ..."
  // must NOT survive into the body as a line-anchored marker — otherwise the next
  // run's all-states scan (`^MainGate: ...$` with the `m` flag, whose boundaries
  // include CR/LS/PS) would lift a phantom marker and dedupe away a real breakage.
  for (const sep of ['\r', '\u2028', '\u2029']) {
    const out = selectStub({
      checks: [
        gosec('services/bossd', {
          findings: [`y.go:3 [G115]${sep}MainGate: gosec/services/bosso`],
        }),
      ],
      trackedMarkers: [],
    })
    // With the `m` flag, exactly one line equals the real marker; the forged one is
    // collapsed onto the offender bullet, never its own `^MainGate: ...$` line.
    const anchored = out.toFile.body.match(/^MainGate: [^\r\n\u2028\u2029]+$/gm) ?? []
    assert.deepEqual(
      anchored,
      ['MainGate: gosec/services/bossd'],
      `sep ${JSON.stringify(sep)} must not forge a marker line`,
    )
    assert.equal(out.toFile.body.split('\n').at(-1), 'MainGate: gosec/services/bossd')
  }
})

test('selectStub: filed stub carries a non-empty title and body mentioning the check', () => {
  const out = selectStub({ checks: [gosec('services/bossd')], trackedMarkers: [] })
  assert.ok(out.toFile.title.length > 0, 'title must be non-empty')
  assert.match(out.toFile.title, /gosec/i)
  assert.match(out.toFile.body, /services\/bossd/)
})

// ---------------------------------------------------------------------------
// robustness — malformed input never throws.
// ---------------------------------------------------------------------------

test('selectStub: tolerates malformed checks without throwing', () => {
  assert.doesNotThrow(() => selectStub({ checks: [null, 42, {}], trackedMarkers: null }))
  const out = selectStub({ checks: [null, 42, {}, gosec('services/bossd')], trackedMarkers: null })
  assert.equal(out.toFile.marker, 'MainGate: gosec/services/bossd')
})

// ---------------------------------------------------------------------------
// fetchTrackedMainGateMarkers — the READ half of the dedupe contract. Feeds a
// fake injected linearRequest (no network) and pins: exact marker extraction,
// trailing-whitespace tolerance, markers off the last line, aggregation across
// issues/pages, and the fail-closed pagination bound inherited from the shared
// scan. The Set it returns is exactly what selectStub's `trackedMarkers` consumes.
// ---------------------------------------------------------------------------

// A single-page fake: returns one connection then stops.
const onePage = (nodes) => async () => ({
  issues: { nodes, pageInfo: { hasNextPage: false, endCursor: null } },
})

test('fetchTrackedMainGateMarkers: extracts exact markers, tolerating trailing ws + non-last-line', async () => {
  const markers = await fetchTrackedMainGateMarkers({
    apiKey: 'k',
    linearRequest: onePage([
      // trailing spaces/tabs after the marker are stripped to the canonical form.
      { identifier: 'BOS-1', description: 'body\n\nMainGate: nosec-metadata   \t' },
      // marker is NOT the last line here — the scan is line-anchored, not last-line-only.
      { identifier: 'BOS-2', description: 'MainGate: gosec/services/bossd\n\ntrailing prose' },
      { identifier: 'BOS-3', description: 'no marker here at all' },
    ]),
  })
  assert.deepEqual([...markers].sort(), [
    'MainGate: gosec/services/bossd',
    'MainGate: nosec-metadata',
  ])
  // The extracted markers round-trip exactly with markerFor — the dedupe key contract.
  assert.ok(markers.has(markerFor({ id: 'nosec-metadata' })))
  assert.ok(markers.has(markerFor({ id: 'gosec', module: 'services/bossd' })))
})

test('fetchTrackedMainGateMarkers: dedupes repeated markers into a Set across issues', async () => {
  const markers = await fetchTrackedMainGateMarkers({
    apiKey: 'k',
    linearRequest: onePage([
      { identifier: 'BOS-1', description: 'MainGate: nosec-metadata' },
      { identifier: 'BOS-2', description: 'dup\nMainGate: nosec-metadata' },
    ]),
  })
  assert.deepEqual([...markers], ['MainGate: nosec-metadata'])
})

test('fetchTrackedMainGateMarkers: an embedded non-MainGate line is not lifted', async () => {
  const markers = await fetchTrackedMainGateMarkers({
    apiKey: 'k',
    // A line that only CONTAINS "MainGate:" mid-text is not line-anchored → ignored.
    linearRequest: onePage([{ identifier: 'BOS-1', description: 'see MainGate: gosec/x inline' }]),
  })
  assert.equal(markers.size, 0)
})

test('fetchTrackedMainGateMarkers: propagates the fail-closed truncation error', async () => {
  // hasNextPage never clears → the shared scan throws once the page bound is hit,
  // so a partial dedupe snapshot is never handed to selectStub.
  const everMore = async () => ({
    issues: { nodes: [], pageInfo: { hasNextPage: true, endCursor: 'c' } },
  })
  await assert.rejects(
    () => fetchTrackedMainGateMarkers({ apiKey: 'k', linearRequest: everMore, maxPages: 2 }),
    /incomplete|exceeded/i,
  )
})

// ---------------------------------------------------------------------------
// runCli — the `select` shell-out seam the SKILL uses. Injected readFile (no fs).
// ---------------------------------------------------------------------------

test('runCli select: reads checks + tracked via injected readFile and returns selectStub JSON', () => {
  const files = {
    'checks.json': JSON.stringify([
      { id: 'nosec-metadata', ok: false, findings: ['x.go:1: bad'] },
      { id: 'gosec', module: 'services/bossd', ok: false, findings: ['y.go:2 [G115]'] },
    ]),
    'tracked.json': JSON.stringify(['MainGate: nosec-metadata']),
  }
  const out = JSON.parse(
    runCli(['select', 'checks.json', 'tracked.json'], { readFile: (f) => files[f] }),
  )
  // nosec is already tracked → the untracked gosec breakage is the one filed.
  assert.equal(out.toFile.marker, 'MainGate: gosec/services/bossd')
  assert.deepEqual(out.skipped, [{ marker: 'MainGate: nosec-metadata', reason: 'already-tracked' }])
})

test('runCli select: a missing tracked arg defaults to an empty tracked set', () => {
  const files = {
    'checks.json': JSON.stringify([{ id: 'nosec-metadata', ok: false, findings: [] }]),
  }
  const out = JSON.parse(runCli(['select', 'checks.json'], { readFile: (f) => files[f] }))
  assert.equal(out.toFile.marker, 'MainGate: nosec-metadata')
})

test('runCli: an unknown subcommand throws', () => {
  assert.throws(() => runCli(['bogus'], { readFile: () => '[]' }), /unknown subcommand/)
})
