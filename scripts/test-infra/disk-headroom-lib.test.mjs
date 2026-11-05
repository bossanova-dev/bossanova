import assert from 'node:assert/strict'
import test from 'node:test'
import path from 'node:path'
import { admissionConfig } from './gate-admission-lib.mjs'
import {
  measureHeadroom,
  decideHeadroom,
  parseFloorGiB,
  reclaimLockDir,
} from './disk-headroom-lib.mjs'

test('headroom deduplicates filesystems and skips missing paths', () => {
  const entries = measureHeadroom(['a', 'missing', 'b', 'c'], {
    stat(p) {
      if (p === 'missing') throw Object.assign(Error(), { code: 'ENOENT' })
      return { dev: p === 'c' ? 2 : 1 }
    },
    statfs: () => ({ bavail: 3, bsize: 1024 }),
  })
  assert.deepEqual(entries, [
    { path: 'a', freeBytes: 3072 },
    { path: 'c', freeBytes: 3072 },
  ])
})
test('headroom decisions preserve the lowest filesystem', () => {
  const entries = [
    { path: 'a', freeBytes: 50 },
    { path: 'b', freeBytes: 20 },
  ]
  assert.equal(decideHeadroom({ entries, floorBytes: 20 }).verdict, 'ok')
  assert.deepEqual(decideHeadroom({ entries, floorBytes: 21 }), {
    verdict: 'below-floor',
    lowest: entries[1],
  })
  assert.equal(decideHeadroom({ entries, floorBytes: 0 }).verdict, 'disabled')
})
test('floor defaults, disables, and warns on invalid input', () => {
  const warnings = []
  assert.equal(parseFloorGiB(), 20)
  assert.equal(parseFloorGiB('0'), 0)
  assert.equal(parseFloorGiB('1.5'), 1.5)
  for (const value of ['garbage', '-1', ''])
    assert.equal(
      parseFloorGiB(value, (line) => warnings.push(line)),
      20,
    )
  assert.equal(warnings.length, 3)
})

test('reclaim locks share a stable per-cache key across worktree temp directories', () => {
  const cache = '/shared/go-cache'
  const first = reclaimLockDir({ TMPDIR: '/one/tmp' }, cache)
  assert.equal(first, reclaimLockDir({ TMPDIR: '/another/tmp' }, cache))
  assert.equal(path.dirname(first), admissionConfig({}).dir)
  assert.match(path.basename(first), /^go-cache-reclaim-[a-f0-9]{12}$/)
  assert.notEqual(first, reclaimLockDir({}, '/shared/other-cache'))
  const override = { BOSS_GATE_ADMISSION_DIR: '/custom/admission' }
  assert.equal(
    reclaimLockDir(override, cache),
    path.join(admissionConfig(override).dir, path.basename(first)),
  )
})
