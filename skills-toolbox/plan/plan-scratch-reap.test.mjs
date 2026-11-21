import { tempDir } from '../../scripts/test-infra/test-tmpdir.mjs'
import { test } from 'node:test'
import assert from 'node:assert/strict'
import {
  existsSync,
  chmodSync,
  lstatSync,
  mkdirSync,
  symlinkSync,
  writeFileSync,
  readFileSync,
  utimesSync,
} from 'node:fs'
import { join } from 'node:path'
import { spawnSync } from 'node:child_process'
import {
  reapPlanScratch,
  STALE_PLAN_SCRATCH_TTL_MS,
  STALE_PLAN_PIN_TTL_MS,
  UNATTENDED_PLAN_PIN_MARKER,
} from './plan-scratch-reap.mjs'

const staleDate = () => new Date(Date.now() - STALE_PLAN_SCRATCH_TTL_MS - 1000)

test('reapPlanScratch removes stale files directly under scratch and keeps fresh ones', () => {
  const dir = tempDir('plan-scratch-')
  const stale = join(dir, 'BOS-1-old.md')
  const fresh = join(dir, 'BOS-1-new.md')
  writeFileSync(stale, 'old')
  writeFileSync(fresh, 'new')
  const old = staleDate()
  utimesSync(stale, old, old)

  const messages = []
  const result = reapPlanScratch(dir, { onReport: (msg) => messages.push(msg) })

  assert.equal(existsSync(stale), false)
  assert.equal(existsSync(fresh), true)
  assert.deepEqual(result.removed, [stale])
  assert.equal(result.failed.length, 0)
  assert.match(messages.join('\n'), /removed stale plan scratch file/)
})

test('reapPlanScratch removes a stale directory and everything inside it', () => {
  // The contract creates directories (one `run-<RUN-ID>/` per run), so a run that
  // dies before its own cleanup leaves one behind. A file-only sweep could never
  // reach it — which is how the legacy `children/` directory became permanent.
  const dir = tempDir('plan-scratch-dir-')
  const runDir = join(dir, 'run-abc123')
  const inside = join(runDir, 'BOS-1.precheck.json')
  const legacy = join(dir, 'children')
  const legacyFile = join(legacy, 'BOS-780-a-plan.md')
  mkdirSync(runDir)
  writeFileSync(inside, '{}')
  mkdirSync(legacy)
  writeFileSync(legacyFile, 'plan')
  const old = staleDate()
  utimesSync(runDir, old, old)
  utimesSync(legacy, old, old)

  const messages = []
  const result = reapPlanScratch(dir, { onReport: (msg) => messages.push(msg) })

  assert.equal(existsSync(runDir), false)
  assert.equal(existsSync(inside), false)
  assert.equal(existsSync(legacy), false)
  assert.equal(existsSync(legacyFile), false)
  assert.deepEqual(result.removed.sort(), [legacy, runDir].sort())
  assert.equal(result.failed.length, 0)
  assert.match(messages.join('\n'), /removed stale plan scratch directory/)
})

test('reapPlanScratch keeps a directory younger than the TTL', () => {
  // A live peer run's scratch directory must survive this sweep.
  const dir = tempDir('plan-scratch-young-dir-')
  const runDir = join(dir, 'run-live')
  const inside = join(runDir, 'BOS-2.precheck.json')
  mkdirSync(runDir)
  writeFileSync(inside, '{}')

  const result = reapPlanScratch(dir)

  assert.equal(existsSync(runDir), true)
  assert.equal(readFileSync(inside, 'utf8'), '{}')
  assert.deepEqual(result, { removed: [], failed: [] })
})

test('reapPlanScratch leaves an entry that is neither a file nor a directory', () => {
  const dir = tempDir('plan-scratch-dangling-')
  const dangling = join(dir, 'dangling-link')
  symlinkSync(join(dir, 'no-such-target'), dangling)

  const result = reapPlanScratch(dir)

  assert.deepEqual(result, { removed: [], failed: [] })
  assert.equal(existsSync(dangling), false) // existsSync follows the broken link
  assert.equal(lstatSync(dangling).isSymbolicLink(), true) // but the link itself is untouched
})

// The dangling case above passes under statSync too, because statSync throws on a broken
// link and the loop skips it. A RESOLVING link is the one that separates the two calls:
// statSync follows it, so the link is classified and TTL-judged by its target and gets
// unlinked. lstatSync describes the link itself, which is neither a file nor a directory.
test('reapPlanScratch leaves a resolving symlink alone however stale its target', () => {
  const dir = tempDir('plan-scratch-symlink-')
  const targetFile = join(dir, 'target.md')
  const targetDir = join(dir, 'target-dir')
  writeFileSync(targetFile, 'payload')
  mkdirSync(targetDir)
  const old = staleDate()
  utimesSync(targetFile, old, old)
  utimesSync(targetDir, old, old)
  // Both links are created now, so only the TARGET is stale. Under statSync each would be
  // reaped on its target's mtime; under lstatSync neither is a file or a directory at all.
  const fileLink = join(dir, 'link-to-file')
  const dirLink = join(dir, 'link-to-dir')
  symlinkSync(targetFile, fileLink)
  symlinkSync(targetDir, dirLink)

  const result = reapPlanScratch(dir)

  assert.deepEqual(result.failed, [])
  assert.equal(result.removed.includes(fileLink), false)
  assert.equal(result.removed.includes(dirLink), false)
  assert.equal(lstatSync(fileLink).isSymbolicLink(), true)
  assert.equal(lstatSync(dirLink).isSymbolicLink(), true)
  // The stale targets are reaped on their own entries, which is the contract's remit.
  assert.equal(existsSync(targetFile), false)
  assert.equal(existsSync(targetDir), false)
})

test('reapPlanScratch treats a missing scratch directory as a clean no-op', () => {
  const dir = join(tempDir('plan-scratch-missing-'), 'missing')
  assert.deepEqual(reapPlanScratch(dir), { removed: [], failed: [] })
})

test('reapPlanScratch keeps files younger than the TTL', () => {
  const dir = tempDir('plan-scratch-young-')
  const young = join(dir, 'BOS-1-young.md')
  writeFileSync(young, 'young')
  reapPlanScratch(dir)
  assert.equal(readFileSync(young, 'utf8'), 'young')
})

test('reaping stale scratch preserves its ignore file while removing stale runs', () => {
  const root = tempDir('plan-scratch-ignore-reap-')
  const ignore = join(root, '.gitignore')
  const run = join(root, 'run-old')
  writeFileSync(ignore, '# boss-plan scratch\n*\n')
  mkdirSync(run)
  writeFileSync(join(run, 'plan.md'), 'stale')
  const old = staleDate()
  utimesSync(ignore, old, old)
  utimesSync(run, old, old)
  const result = reapPlanScratch(root)
  assert.equal(readFileSync(ignore, 'utf8'), '# boss-plan scratch\n*\n')
  assert.equal(existsSync(run), false)
  assert.deepEqual(result, { removed: [run], failed: [] })
})

test('reaper removes idle read-only pins after six hours and keeps live pins and younger run scratch', () => {
  const root = tempDir('plan-pin-reap-')
  const idle = join(root, 'pin-idle'),
    live = join(root, 'pin-live'),
    run = join(root, 'run-live')
  for (const dir of [idle, live, run]) {
    mkdirSync(dir)
    writeFileSync(join(dir, 'payload'), 'payload')
    chmodSync(join(dir, 'payload'), 0o444)
  }
  writeFileSync(join(idle, UNATTENDED_PLAN_PIN_MARKER), '')
  writeFileSync(join(live, UNATTENDED_PLAN_PIN_MARKER), '')
  const now = Date.now()
  const old = new Date(now - STALE_PLAN_PIN_TTL_MS - 1000)
  utimesSync(idle, old, old)
  utimesSync(run, old, old)
  assert.deepEqual(reapPlanScratch(root, { now }), { removed: [idle], failed: [] })
  assert.equal(existsSync(idle), false)
  assert.equal(existsSync(live), true)
  assert.equal(existsSync(run), true)
})

test('an interactive or unknown owner can resume its pinned runner after a long pause', () => {
  const root = tempDir('plan-pin-paused-')
  const pin = join(root, 'pin-paused')
  const run = join(root, 'run-paused')
  mkdirSync(pin)
  mkdirSync(run)
  const runner = join(pin, 'runner.mjs')
  writeFileSync(runner, "process.stdout.write('resumed')")
  writeFileSync(join(run, 'plan.md'), 'pending confirmation')
  const now = Date.now()
  const old = new Date(now - 30 * STALE_PLAN_SCRATCH_TTL_MS)
  utimesSync(pin, old, old)
  utimesSync(run, old, old)

  assert.deepEqual(reapPlanScratch(root, { now }), { removed: [], failed: [] })
  const resumed = spawnSync(process.execPath, [runner], { encoding: 'utf8' })
  assert.equal(resumed.status, 0, resumed.stderr)
  assert.equal(resumed.stdout, 'resumed')
  assert.equal(readFileSync(join(run, 'plan.md'), 'utf8'), 'pending confirmation')
})
