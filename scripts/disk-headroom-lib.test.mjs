import assert from 'node:assert/strict'
import test from 'node:test'
import { measureHeadroom, decideHeadroom, parseFloorGiB } from './disk-headroom-lib.mjs'

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
