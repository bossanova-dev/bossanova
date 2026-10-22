#!/usr/bin/env node
import { tempDir } from './test-tmpdir.mjs'

import assert from 'node:assert/strict'
import * as fs from 'node:fs'
import { join } from 'node:path'
import test from 'node:test'

import {
  bazelOutputUserRoots,
  findOrphanOutputBases,
  findStaleTempDirs,
  golangciCacheDir,
  parseSwapUsage,
  removeTree,
  stalePuppeteerBuilds,
} from './recover-disk-space.mjs'

const NOW = Date.UTC(2026, 9, 20)
const DAY = 24 * 60 * 60 * 1000

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
