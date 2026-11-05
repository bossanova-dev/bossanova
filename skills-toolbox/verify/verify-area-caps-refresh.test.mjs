import assert from 'node:assert/strict'
import { test } from 'node:test'
import * as fs from 'node:fs'
import { join } from 'node:path'
import { tempDir } from '../../scripts/test-infra/test-tmpdir.mjs'
import {
  prepareAreaCaps,
  refreshAreaCaps,
  REFRESH_INTERVAL_MS,
  REFRESH_WAIT_MS,
} from './verify-area-caps-refresh.mjs'

function fixture(state) {
  const dir = tempDir('area-caps-refresh-')
  const path = join(dir, 'boss-verify', 'area-caps.json')
  if (state) {
    fs.mkdirSync(join(dir, 'boss-verify'))
    fs.writeFileSync(path, JSON.stringify(state))
  }
  const spawned = []
  const deps = {
    run: () => ({ ok: true, output: dir }),
    now: () => Date.parse('2026-11-05T00:00:00Z'),
    spawnDetached: (...args) => {
      spawned.push(args)
      return { ok: true }
    },
  }
  return { path, spawned, deps }
}

test('missing or stale snapshots launch one refresh and repeated ticks preserve its claim', () => {
  for (const state of [
    undefined,
    { v: 1, refreshedAt: '2026-11-04T00:00:00Z', caps: [{ key: 'ui' }] },
  ]) {
    const f = fixture(state)
    assert.equal(refreshAreaCaps(f.deps).started, true)
    assert.equal(refreshAreaCaps(f.deps).started, false)
    assert.equal(f.spawned.length, 1)
    assert.equal(f.spawned[0][1][1], 'refresh')
    assert.equal(f.spawned[0][2].label, 'area-caps')
    const saved = JSON.parse(fs.readFileSync(f.path, 'utf8'))
    assert.equal(saved.refreshStartedAt, '2026-11-05T00:00:00.000Z')
    if (state) assert.deepEqual(saved.caps, state.caps)
  }
})

test('fresh state or a recent start suppress refresh', () => {
  for (const state of [
    { refreshedAt: '2026-11-04T23:00:00Z' },
    { refreshedAt: '2026-11-03T00:00:00Z', refreshStartedAt: '2026-11-04T23:00:00Z' },
  ]) {
    const f = fixture(state)
    assert.equal(refreshAreaCaps(f.deps).started, false)
    assert.equal(f.spawned.length, 0)
  }
  assert.equal(REFRESH_INTERVAL_MS, 6 * 60 * 60 * 1000)
})

test('a competing refresh lock prevents duplicate launch', () => {
  const f = fixture()
  fs.mkdirSync(join(f.path, '..'), { recursive: true })
  fs.writeFileSync(`${f.path}.refresh-lock`, '')
  fs.utimesSync(`${f.path}.refresh-lock`, new Date(f.deps.now()), new Date(f.deps.now()))
  assert.equal(refreshAreaCaps(f.deps).started, false)
  assert.equal(f.spawned.length, 0)
})

test('spawn failure is health output and cannot interrupt a sweep', () => {
  const f = fixture()
  f.deps.spawnDetached = () => {
    throw Error('spawn unavailable')
  }
  assert.deepEqual(refreshAreaCaps(f.deps), { started: false, health: 'area-caps-refresh-failed' })
})

test('an overlapping tick sees the lock and a stale abandoned lock recovers', () => {
  const f = fixture()
  const launch = f.deps.spawnDetached
  f.deps.spawnDetached = (...args) => {
    assert.equal(refreshAreaCaps(f.deps).started, false)
    return launch(...args)
  }
  assert.equal(refreshAreaCaps(f.deps).started, true)
  assert.equal(f.spawned.length, 1)
  const expired = fixture()
  fs.mkdirSync(join(expired.path, '..'), { recursive: true })
  fs.writeFileSync(`${expired.path}.refresh-lock`, '')
  const old = new Date(expired.deps.now() - REFRESH_INTERVAL_MS)
  fs.utimesSync(`${expired.path}.refresh-lock`, old, old)
  assert.equal(refreshAreaCaps(expired.deps).started, true)
  assert.equal(expired.spawned.length, 1)
})

test('stale admission waits for completion, then fails open if refresh failed', () => {
  const f = fixture({ refreshedAt: '2026-11-01T00:00:00Z' })
  assert.equal(prepareAreaCaps(f.deps).wait, 'area caps refresh pending')
  assert.equal(prepareAreaCaps(f.deps).wait, 'area caps refresh pending')
  assert.equal(f.spawned.length, 1)
  fs.writeFileSync(
    `${f.path}.refresh-result`,
    JSON.stringify({ completedAt: new Date(f.deps.now()).toISOString() }),
  )
  assert.equal(prepareAreaCaps(f.deps).wait, undefined)
  assert.equal(f.spawned.length, 1)
})

test('a fresh snapshot can judge during refresh; missing state waits; abandoned refresh times out', () => {
  const fresh = fixture({ refreshedAt: '2026-11-04T17:00:00Z' })
  assert.equal(prepareAreaCaps(fresh.deps).wait, undefined)
  assert.equal(fresh.spawned.length, 1)
  const missing = fixture()
  assert.equal(prepareAreaCaps(missing.deps).wait, 'area caps refresh pending')
  const started = missing.deps.now()
  missing.deps.now = () => started + REFRESH_WAIT_MS
  assert.equal(prepareAreaCaps(missing.deps).wait, undefined)
  assert.equal(prepareAreaCaps(missing.deps).health, 'area-caps-refresh-timeout')
  assert.equal(missing.spawned.length, 1)
})
