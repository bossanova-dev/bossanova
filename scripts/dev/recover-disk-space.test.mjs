#!/usr/bin/env node
import { tempDir } from '../test-infra/test-tmpdir.mjs'

import assert from 'node:assert/strict'
import * as fs from 'node:fs'
import { join } from 'node:path'
import test from 'node:test'

import {
  bazelOutputUserRoots,
  expireLogs,
  expiredFiles,
  findOrphanOutputBases,
  findStaleTempDirs,
  golangciCacheDir,
  parseSwapUsage,
  removeEmptyDirs,
  removeTree,
  recoverGolangciCaches,
  stalePuppeteerBuilds,
  trimGoCache,
} from './recover-disk-space.mjs'

const NOW = Date.UTC(2026, 9, 20)
const HOUR = 60 * 60 * 1000
const DAY = 24 * HOUR

function fixture(t, prefix = 'recover-disk-space-') {
  const dir = fs.realpathSync(tempDir(prefix))
  t.after(() => {
    removeTree(dir)
  })
  return dir
}

function outputBase(root, name, { workspace, pid } = {}) {
  const path = join(root, name)
  fs.mkdirSync(join(path, 'server'), { recursive: true })
  if (workspace !== undefined) fs.writeFileSync(join(path, 'DO_NOT_BUILD_HERE'), workspace)
  if (pid !== undefined) fs.writeFileSync(join(path, 'server', 'server.pid.txt'), String(pid))
  return path
}

function age(path, ms) {
  fs.utimesSync(path, new Date(NOW - ms), new Date(NOW - ms))
}

test('finds output bases of deleted workspaces and keeps live or busy ones', (t) => {
  const root = fixture(t)
  const live = fixture(t, 'recover-disk-space-workspace-')
  const gone = join(live, 'deleted-worktree')
  const orphan = outputBase(root, 'a'.repeat(32), { workspace: gone })
  outputBase(root, 'b'.repeat(32), { workspace: live })
  const busy = outputBase(root, 'c'.repeat(32), { workspace: gone, pid: 4242 })
  const deadServer = outputBase(root, 'd'.repeat(32), { workspace: gone, pid: 4343 })
  outputBase(root, 'e'.repeat(32))
  outputBase(root, 'f'.repeat(32), { workspace: 'relative/path' })
  outputBase(root, 'install', { workspace: gone })
  outputBase(root, '0'.repeat(31), { workspace: gone })

  const result = findOrphanOutputBases(root, { isAlive: (pid) => pid === 4242 })
  assert.deepEqual(result.orphans.map((entry) => entry.path).sort(), [orphan, deadServer].sort())
  assert.deepEqual(result.busy, [{ path: busy, workspace: gone, pid: 4242 }])
})

test('skips output bases owned by another user', (t) => {
  const root = fixture(t)
  outputBase(root, 'a'.repeat(32), { workspace: join(root, 'gone') })
  const uid = fs.lstatSync(root).uid + 1
  assert.deepEqual(findOrphanOutputBases(root, { uid }).orphans, [])
})

test('an absent root yields nothing', () => {
  assert.deepEqual(findOrphanOutputBases('/nonexistent/recover-disk-space'), {
    orphans: [],
    busy: [],
  })
})

test('removes read-only trees without following symlinks out of them', (t) => {
  const root = fixture(t)
  const outside = fixture(t, 'recover-disk-space-outside-')
  fs.writeFileSync(join(outside, 'keep'), 'keep')
  const tree = join(root, 'base')
  fs.mkdirSync(join(tree, 'external', 'repo'), { recursive: true })
  fs.writeFileSync(join(tree, 'external', 'repo', 'BUILD'), '')
  fs.symlinkSync(outside, join(tree, 'external', 'link'))
  fs.chmodSync(join(tree, 'external', 'repo'), 0o555)
  fs.chmodSync(join(tree, 'external'), 0o555)

  removeTree(tree)
  assert.equal(fs.existsSync(tree), false)
  assert.equal(fs.readFileSync(join(outside, 'keep'), 'utf8'), 'keep')
})

test('keeps the newest two puppeteer builds per browser and platform', (t) => {
  const root = fixture(t)
  const builds = {
    chrome: ['mac_arm-99.0.1.1', 'mac_arm-129.0.6668.100', 'mac_arm-130.0.6723.69', 'linux-1.2.3'],
    'chrome-headless-shell': ['mac_arm-131.0.6778.85', 'mac_arm-131.0.6778.108'],
  }
  for (const [browser, names] of Object.entries(builds)) {
    for (const name of names) fs.mkdirSync(join(root, browser, name), { recursive: true })
  }
  fs.writeFileSync(join(root, 'chrome', 'mac_arm-1.0.0.0'), 'not a directory')
  fs.mkdirSync(join(root, 'chrome', 'notes'))

  assert.deepEqual(stalePuppeteerBuilds(root), [join(root, 'chrome', 'mac_arm-99.0.1.1')])
  assert.deepEqual(
    stalePuppeteerBuilds(root, { keep: 1 }).sort(),
    [
      join(root, 'chrome', 'mac_arm-129.0.6668.100'),
      join(root, 'chrome', 'mac_arm-99.0.1.1'),
      join(root, 'chrome-headless-shell', 'mac_arm-131.0.6778.85'),
    ].sort(),
  )
})

test('finds only old, temp-shaped directories with no recently touched children', (t) => {
  const root = fixture(t)
  const make = (name, ms, child) => {
    const path = join(root, name)
    fs.mkdirSync(path)
    if (child) {
      fs.writeFileSync(join(path, child.name), '')
      age(join(path, child.name), child.ms)
    }
    age(path, ms)
    return path
  }
  const nodeTemp = make('plan-image-guard-4G2x0N', 4 * DAY)
  const goTemp = make('bossd-security-1139060453', 4 * DAY)
  make('skill-ext-Ctd34F', 2 * DAY)
  make('bda-2qzvKY', 4 * DAY, { name: 'fresh', ms: DAY })
  make('_bazel_dave', 40 * DAY)
  make('tmux-501', 40 * DAY)
  make('Projects', 40 * DAY)
  fs.writeFileSync(join(root, 'probe-dECTno'), '')
  age(join(root, 'probe-dECTno'), 40 * DAY)
  fs.symlinkSync(nodeTemp, join(root, 'alias-AbCdEf'))

  const stale = findStaleTempDirs([root, root], { now: NOW })
  assert.deepEqual(stale.sort(), [goTemp, nodeTemp].sort())
  assert.deepEqual(findStaleTempDirs([root], { now: NOW, uid: fs.lstatSync(root).uid + 1 }), [])
})

test('resolves cache roots for each OS layout and honours overrides', () => {
  const home = '/home/u'
  assert.equal(
    golangciCacheDir({ env: {}, os: 'darwin', home }),
    '/home/u/Library/Caches/golangci-lint',
  )
  assert.equal(golangciCacheDir({ env: {}, os: 'linux', home }), '/home/u/.cache/golangci-lint')
  assert.equal(
    golangciCacheDir({ env: { XDG_CACHE_HOME: '/x' }, os: 'linux', home }),
    '/x/golangci-lint',
  )
  assert.equal(golangciCacheDir({ env: { GOLANGCI_LINT_CACHE: '/g' }, os: 'darwin', home }), '/g')
  assert.deepEqual(
    bazelOutputUserRoots({ env: { BAZEL_OUTPUT_USER_ROOT: '/b' }, home, user: 'u' }),
    ['/b', '/private/var/tmp/_bazel_u', '/home/u/.cache/bazel/_bazel_u'],
  )
})

test('parses macOS swap usage', () => {
  const text = 'total = 66560.00M  used = 65580.75M  free = 979.25M  (encrypted)'
  assert.equal(parseSwapUsage(text), 65580.75 * 1024 ** 2)
  assert.equal(parseSwapUsage('garbage'), null)
})

test('evicts stale pool entries across existing slots while preserving fresh entries', (t) => {
  const root = fixture(t)
  const slots = join(root, 'slots')
  const callerCache = join(root, 'caller-cache')
  const makeCache = (dir) => {
    fs.mkdirSync(join(dir, 'aa'), { recursive: true })
    fs.writeFileSync(
      join(dir, 'README'),
      'This directory holds cached build artifacts from golangci-lint.\n',
    )
    const stale = join(dir, 'aa', `${'a'.repeat(64)}-d`)
    const fresh = join(dir, 'aa', `${'b'.repeat(64)}-d`)
    fs.writeFileSync(stale, 'stale entry')
    fs.writeFileSync(fresh, 'fresh entry')
    const old = new Date(Date.now() - 2 * 60 * 60 * 1000)
    fs.utimesSync(stale, old, old)
    return { stale, fresh }
  }
  const entries = [0, 3].map((slot) => makeCache(join(slots, 'caches', `slot-${slot}`)))
  const caller = makeCache(callerCache)
  const unrelated = makeCache(join(slots, 'caches', 'unrelated'))
  const env = {
    BOSS_GOLANGCI_SLOT_DIR: slots,
    BOSS_GOLANGCI_SLOTS: '1',
    GOLANGCI_LINT_CACHE: callerCache,
    GOLANGCI_CACHE_MAX_GIB: '0',
  }
  recoverGolangciCaches({ dryRun: true, env, write: () => {} })
  assert.ok(entries.every(({ stale }) => fs.existsSync(stale)))
  recoverGolangciCaches({ env, write: () => {} })
  for (const { stale, fresh } of [...entries, caller]) {
    assert.equal(fs.existsSync(stale), false)
    assert.equal(fs.existsSync(fresh), true)
  }
  assert.equal(fs.existsSync(unrelated.stale), true)
})

test('log retention removes only expired files and the folders they leave empty', (t) => {
  const root = fixture(t)
  const write = (relative, ms) => {
    const path = join(root, relative)
    fs.mkdirSync(join(path, '..'), { recursive: true })
    fs.writeFileSync(path, 'x'.repeat(10))
    age(path, ms)
    return path
  }
  const old = write('2026/09/01/rollout-old.jsonl', 40 * DAY)
  const fresh = write('2026/10/19/rollout-fresh.jsonl', 2 * DAY)
  const mixedOld = write('2026/10/02/rollout-a.jsonl', 40 * DAY)
  const mixedFresh = write('2026/10/02/rollout-b.jsonl', 1 * DAY)

  const { files, bytes } = expiredFiles(root, { cutoffMs: NOW - 30 * DAY })
  assert.deepEqual(files.sort(), [old, mixedOld].sort())
  assert.equal(bytes, 20)

  for (const file of files) fs.rmSync(file)
  removeEmptyDirs(root)
  assert.equal(fs.existsSync(join(root, '2026', '09')), false)
  assert.equal(fs.existsSync(fresh), true)
  assert.equal(fs.existsSync(mixedFresh), true)
  assert.equal(fs.existsSync(root), true)
})

test('expiring logs keeps agent transcripts and session stores however old they are', (t) => {
  const home = fixture(t)
  const old = (relative) => {
    const path = join(home, relative)
    fs.mkdirSync(join(path, '..'), { recursive: true })
    fs.writeFileSync(path, 'x')
    age(path, 400 * DAY)
    return path
  }
  const kept = [
    old('.codex/sessions/2025/09/01/rollout-2025-09-01T00-00-00-idle.jsonl'),
    old('.claude/projects/-Users-me-repo/0f6c3a52-idle.jsonl'),
    old('.bossanova/agent-logs/0f6c3a52-idle.log'),
  ]
  const daemonLog = old('.bossanova/logs/bossd.log.1')

  expireLogs({ dryRun: false, home, write: () => {}, now: NOW })

  for (const path of kept) assert.equal(fs.existsSync(path), true, `kept ${path}`)
  assert.equal(fs.existsSync(daemonLog), false)
})

test('the Go cache trim spares entries a build used within the last two hours', (t) => {
  const dir = fixture(t, 'recover-disk-space-gocache-')
  fs.writeFileSync(
    join(dir, 'README'),
    'This directory holds cached build artifacts from the Go build system.\n',
  )
  const entry = (id, ms) => {
    fs.mkdirSync(join(dir, 'ab'), { recursive: true })
    const path = join(dir, 'ab', `${String(id).padStart(64, '0')}-d`)
    fs.writeFileSync(path, 'x')
    age(path, ms)
    return path
  }
  const recent = entry(1, 90 * 60 * 1000)
  const stale = entry(2, 3 * HOUR)
  const exec = (command, args) => {
    assert.deepEqual([command, ...args], ['go', 'env', 'GOCACHE'])
    return `${dir}\n`
  }

  trimGoCache({ dryRun: false, env: { GO_CACHE_MAX_GIB: '0' }, exec, write: () => {}, now: NOW })

  assert.equal(fs.existsSync(recent), true)
  assert.equal(fs.existsSync(stale), false)
})
