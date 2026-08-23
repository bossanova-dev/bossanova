import { test } from 'node:test'
import assert from 'node:assert/strict'
import {
  existsSync,
  mkdirSync,
  mkdtempSync,
  writeFileSync,
  readFileSync,
  utimesSync,
} from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { reapPlanScratch, STALE_PLAN_SCRATCH_TTL_MS } from './plan-scratch-reap.mjs'

test('reapPlanScratch removes only stale regular files directly under scratch', () => {
  const dir = mkdtempSync(join(tmpdir(), 'plan-scratch-'))
  const stale = join(dir, 'BOS-1-old.md')
  const fresh = join(dir, 'BOS-1-new.md')
  const nestedDir = join(dir, 'nested')
  const nested = join(nestedDir, 'old.md')
  writeFileSync(stale, 'old')
  writeFileSync(fresh, 'new')
  mkdirSync(nestedDir)
  writeFileSync(nested, 'nested')
  const old = new Date(Date.now() - STALE_PLAN_SCRATCH_TTL_MS - 1000)
  utimesSync(stale, old, old)
  utimesSync(nestedDir, old, old)
  utimesSync(nested, old, old)

  const messages = []
  const result = reapPlanScratch(dir, { onReport: (msg) => messages.push(msg) })

  assert.equal(existsSync(stale), false)
  assert.equal(existsSync(fresh), true)
  assert.equal(existsSync(nested), true)
  assert.deepEqual(result.removed, [stale])
  assert.equal(result.failed.length, 0)
  assert.match(messages.join('\n'), /removed stale plan scratch/)
})

test('reapPlanScratch treats a missing scratch directory as a clean no-op', () => {
  const dir = join(mkdtempSync(join(tmpdir(), 'plan-scratch-missing-')), 'missing')
  assert.deepEqual(reapPlanScratch(dir), { removed: [], failed: [] })
})

test('reapPlanScratch keeps files younger than the TTL', () => {
  const dir = mkdtempSync(join(tmpdir(), 'plan-scratch-young-'))
  const young = join(dir, 'BOS-1-young.md')
  writeFileSync(young, 'young')
  reapPlanScratch(dir)
  assert.equal(readFileSync(young, 'utf8'), 'young')
})
