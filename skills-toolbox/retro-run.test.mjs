import test from 'node:test'
import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { spawnSync } from 'node:child_process'
import { settings, filing, augment, finalize, prunePlan, recordRun, preview } from './retro-run.mjs'
import { clusterNotes, selectClusters } from './retro-notes.mjs'

test('settings writes by default, dry-run wins and legacy no-dry-run is a no-op', () => {
  assert.equal(settings().mode, 'write')
  assert.equal(settings(['--no-dry-run']).mode, 'write')
  assert.equal(settings(['--dry-run', '--no-dry-run']).mode, 'dry-run')
})
test('settings flag > env > config > omitted and sanitizes run ids', () => {
  const config = { retro: { maxIssues: 9, staleDays: 30, minRuns: 3 } }
  assert.deepEqual(
    settings(['--max-issues=4'], {
      config,
      env: { BOSS_RETRO_MAX_ISSUES: '6', BOSS_RETRO_STALE_DAYS: '20' },
    }).selectArgs,
    ['4', '20', '3'],
  )
  assert.deepEqual(settings().selectArgs, [])
  assert.equal(settings([], { env: { BOSS_AGENT_SESSION_ID: 'a/b' }, now: () => 1 }).runId, 'a_b')
  assert.equal(settings([], { now: () => 1 }).runId, 'retro-1')
})
test('settings rejects usage mistakes with exit 64', () => {
  for (const args of [
    ['--wat'],
    ['--min-runs', '0'],
    ['--stale-days=-1'],
    ['--max-issues'],
    ['--max-issues', '1.5'],
  ])
    assert.throws(() => settings(args), { exitCode: 64 })
  const result = spawnSync(
    process.execPath,
    ['skills-toolbox/retro-run.mjs', 'settings', '--', '--wat'],
    { encoding: 'utf8' },
  )
  assert.equal(result.status, 64)
})
test('filing selection ignores shared unsupported slots and rejects explicit slots', () => {
  const config = {
    adapters: { tracker: 'linear' },
    trackerConfig: { linear: { selection: { labels: { exclude: ['infra'] } } } },
  }
  assert.deepEqual(settings([], { config }).ignored, ['labels.exclude'])
  assert.equal(settings().needsResolution, false)
  assert.throws(() => settings(['--exclude-label', 'infra']), { exitCode: 64 })
  assert.throws(() => settings(['--project', 'one,two']), { exitCode: 64 })
  assert.throws(
    () =>
      settings([], {
        config: {
          adapters: { tracker: 'linear' },
          trackerConfig: {
            linear: { selection: { stages: { retro: { creators: { include: ['a'] } } } } },
          },
        },
      }),
    { exitCode: 64 },
  )
  assert.deepEqual(
    filing({ selection: { labels: { include: ['Docs'] }, projects: { include: ['uuid'] } } }),
    { labels: ['Docs'], project: 'uuid', warnings: [] },
  )
})
test('augment keeps real notes, creates stable synthetic provenance and repeat exemptions', () => {
  const real = { id: 'real', body: 'real' }
  const notes = augment([real], {
    audit: [{ kind: 'rule-of-the-run', body: 'Rule\nRun: run:chat', repeatExempt: true }],
    signals: { planned: [{ body: 'Signal' }] },
    runId: 'fallback',
    now: () => 0,
  })
  assert.equal(notes[0], real)
  assert.match(notes[1].id, /^synthetic:guidance-audit:rule-of-run:/)
  assert.equal(notes[1].chat_id, 'chat')
  assert.equal(notes[1].repeatExempt, true)
  assert.equal(notes[2].session_id, 'fallback')
  assert.equal(notes[2].repeatExempt, false)
  assert.equal(notes[1].created_at, '1970-01-01T00:00:00.000Z')
})
const rule = { id: 'synthetic:guidance-audit:rule-of-run:abc', body: 'rule' }
const selection = (notes = [rule], ladder = { rung: 'rule' }) => ({
  selected: [{ cluster: { key: 'k', title: 'Rule', notes, ladder }, reason: 'selected' }],
  deferred: [],
  dropped: [],
  expired: [],
})
test('finalize stays-rule only for unpromoted pure rule themes', () => {
  assert.equal(finalize(selection(), [], [rule]).deferred[0].reason, 'stays-rule')
  assert.equal(finalize(selection([rule], { rung: 'context' }), [], [rule]).selected.length, 0)
  assert.equal(
    finalize(selection([rule], { rung: 'rule', existingCheck: 'check' }), [], [rule]).selected
      .length,
    1,
  )
  assert.equal(finalize(selection([rule, { id: 'real' }]), ['real'], [rule]).selected.length, 1)
})
test('finalize refills capped selection after deferring pure rule candidates', () => {
  const now = Date.now()
  const notes = ['older rule', 'check gap', 'later rule', 'later gap'].map((body, i) => ({
    id: `synthetic:guidance-audit:rule-of-run:${i}`,
    body,
    repeatExempt: true,
    created_at: new Date(now - (4 - i) * 1000).toISOString(),
  }))
  const clusters = clusterNotes(notes)
  for (const cluster of clusters)
    cluster.ladder = {
      rung: 'rule',
      ...(cluster.statement.includes('gap') ? { existingCheck: 'check' } : {}),
    }
  const capped = selectClusters(clusters, [], { now, cap: 1 })
  assert.equal(capped.selected[0].cluster.statement, 'older rule')
  const finalized = finalize(capped, [], notes)
  assert.deepEqual(
    finalized.selected.map(({ cluster }) => cluster.statement),
    ['check gap'],
  )
  assert.deepEqual(
    finalized.deferred.map(({ cluster, reason }) => [cluster.statement, reason]),
    [
      ['later rule', 'stays-rule'],
      ['later gap', 'over-cap'],
      ['older rule', 'stays-rule'],
    ],
  )
  const zero = finalize(selectClusters(clusters, [], { now, cap: 0 }), [], notes)
  assert.equal(zero.selected.length, 0)
  assert.equal(zero.deferred.filter(({ reason }) => reason === 'stays-rule').length, 2)
  const protectedBuckets = structuredClone(capped)
  protectedBuckets.deferred[0].reason = 'below-threshold'
  protectedBuckets.expired.push({ ...protectedBuckets.deferred.splice(1, 1)[0], reason: 'expired' })
  protectedBuckets.dropped.push({
    ...protectedBuckets.deferred.splice(1, 1)[0],
    reason: 'already-tracked',
  })
  const protectedResult = finalize(protectedBuckets, [], notes)
  assert.equal(protectedResult.selected.length, 0)
  assert.equal(protectedResult.deferred[0].reason, 'below-threshold')
  assert.deepEqual(protectedResult.expired, protectedBuckets.expired)
  assert.deepEqual(protectedResult.dropped, protectedBuckets.dropped)
})
test('finalize rejects unknown ids, keys, buckets and reasons', () => {
  assert.throws(() => finalize(selection(), [], []), /outside/)
  assert.throws(() => finalize({ ...selection(), extra: [] }, [], [rule]), /four/)
  const duplicate = selection()
  duplicate.deferred.push({ ...duplicate.selected[0], reason: 'over-cap' })
  assert.throws(() => finalize(duplicate, [], [rule]), /duplicate/)
  const unknown = selection()
  unknown.selected[0].reason = 'mystery'
  assert.throws(() => finalize(unknown, [], [rule]), /reason/)
})
test('prune-plan removes synthetic ids and recomputes counts', () => {
  assert.deepEqual(prunePlan({ delete: ['real', rule.id], retag: [rule.id] }), {
    delete: ['real'],
    retag: [],
    counts: { delete: 1, retag: 0, drain: 1 },
  })
})
test('record-run writes atomically and preview carries Codify as', (t) => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'retro-run-'))
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }))
  recordRun('chat', { commonDir: dir, now: () => 0 })
  assert.deepEqual(JSON.parse(fs.readFileSync(path.join(dir, 'boss-retro/state.json'))), {
    version: 1,
    lastRunAt: '1970-01-01T00:00:00.000Z',
    runId: 'chat',
  })
  assert.deepEqual(fs.readdirSync(path.join(dir, 'boss-retro')), ['state.json'])
  const cluster = clusterNotes([
    { id: 'real', body: 'Problem\nWhere: a\nRun: run:a', created_at: new Date().toISOString() },
  ])[0]
  cluster.ladder = { rung: 'helper', target: 'a', rungEvidence: 'a:1', instances: 0 }
  assert.match(
    preview(selection(cluster.notes, cluster.ladder)).children[0].description,
    /## Codify as/,
  )
})
test('exempt singleton bypasses repeats while expiry, tracking and cap retain priority', () => {
  const now = Date.now(),
    clusters = clusterNotes([
      { id: 'a', body: 'Problem', repeatExempt: true, created_at: new Date(now).toISOString() },
    ])
  assert.equal(selectClusters(clusters, [], { now, minRuns: 2 }).selected.length, 1)
  clusters[0].notes[0].repeatExempt = false
  assert.equal(
    selectClusters(clusters, [], { now, minRuns: 2 }).deferred[0].reason,
    'below-threshold',
  )
  clusters[0].notes[0].repeatExempt = true
  assert.equal(selectClusters(clusters, [], { now, cap: 0 }).deferred[0].reason, 'over-cap')
  assert.equal(
    selectClusters(clusters, [{ description: `Notes: ${clusters[0].key}` }], { now }).dropped
      .length,
    1,
  )
  assert.equal(selectClusters(clusters, [], { now: now + 31 * 86400000 }).expired.length, 1)
})

test('settings writes optional audit config only into caller scratch directory', (t) => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'retro-settings-'))
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }))
  const result = settings([], {
    config: { retro: { guidanceAudit: { maxLines: 100 } } },
    runDir: dir,
  })
  assert.deepEqual(JSON.parse(fs.readFileSync(result.guidanceAuditConfig)), { maxLines: 100 })
})
test('preview signals preserve collector run tokens from normal five-line notes', () => {
  const result = augment([], {
    runId: 'current',
    signals: { planned: [{ body: 'Signal\nRun: retro-signals / ci / headless / run:pr:9' }] },
  })
  assert.equal(result[0].chat_id, 'pr:9')
})
