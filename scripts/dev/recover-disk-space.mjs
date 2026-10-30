#!/usr/bin/env node

// Reclaims disk from build and test leftovers that pile up on any developer machine. Each step
// finds its own target and skips cleanly when the tool or directory is absent, so one command is
// safe on hosts with different layouts. A step only removes what it can prove is regenerable:
// - Bazel output bases whose workspace no longer exists (deleted worktrees), unless their server
//   is still alive;
// - golangci-lint cache entries unused for an hour, down to GOLANGCI_CACHE_MAX_GIB (8 GiB): with
//   many worktrees nearly all of it is under a day old, so the Go cache's one-day floor frees nothing;
// - puppeteer browsers beyond the newest two per browser and platform;
// - os.mkdtemp / os.MkdirTemp-shaped directories in the temp roots that this user owns and that
//   nothing has touched for three days;
// - package-manager caches (npm, yarn, pnpm, uv, bun, and brew's old versions and downloads)
//   through each manager's own clean command, plus the npx cache npm's command leaves behind;
// - simulator devices whose runtime is gone (`xcrun simctl delete unavailable`).
// It also reports when swap is holding a large share of the disk, which no cleanup can reclaim.
// `--dry-run` lists what each step would remove and runs nothing.

import { execFileSync } from 'node:child_process'
import * as fs from 'node:fs'
import { homedir, platform, tmpdir, userInfo } from 'node:os'
import { isAbsolute, join } from 'node:path'
import { parseArgs } from 'node:util'

import { isMainModule } from '../../skills-toolbox/main-module.mjs'
import { golangciAdmissionConfig } from '../test-infra/golangci-admission.mjs'
import { GOLANGCI_CACHE_SIGNATURE, GiB, evictGoCache, formatGiB } from './clean-go-build-cache.mjs'

const HOUR = 60 * 60 * 1000
const DAY = 24 * HOUR
const OUTPUT_BASE = /^[0-9a-f]{32}$/
// os.mkdtemp appends six alphanumerics to its prefix; Go's os.MkdirTemp appends a decimal run.
export const TEMP_NAMES = [/^[A-Za-z][\w.-]*-[A-Za-z0-9]{6}$/, /^[A-Za-z][\w.-]*?\d{6,10}$/]
const BROWSER_BUILD = /^(.+)-(\d+(?:\.\d+)+)$/
const SWAP_REPORT_BYTES = 8 * GiB

function lstat(fsImpl, path) {
  try {
    return fsImpl.lstatSync(path)
  } catch (error) {
    if (error.code === 'ENOENT' || error.code === 'ENOTDIR') return null
    throw error
  }
}

function readdir(fsImpl, path) {
  try {
    return fsImpl.readdirSync(path)
  } catch (error) {
    if (['ENOENT', 'ENOTDIR', 'EACCES', 'EPERM'].includes(error.code)) return []
    throw error
  }
}

function readText(fsImpl, path) {
  try {
    return fsImpl.readFileSync(path, 'utf8').trim()
  } catch (error) {
    if (['ENOENT', 'ENOTDIR', 'EACCES', 'EPERM'].includes(error.code)) return ''
    throw error
  }
}

function pidAlive(pid) {
  try {
    process.kill(pid, 0)
    return true
  } catch (error) {
    return error.code === 'EPERM'
  }
}

/** Every Bazel output user root this host could be using, whichever OS defaults apply. */
/** @param {{ env?: NodeJS.ProcessEnv, home?: string, user?: string }} [options] */
export function bazelOutputUserRoots({ env = process.env, home = homedir(), user } = {}) {
  const name = `_bazel_${user ?? userInfo().username}`
  const roots = [
    env.BAZEL_OUTPUT_USER_ROOT,
    join('/private/var/tmp', name),
    join(home, '.cache', 'bazel', name),
    env.XDG_CACHE_HOME && join(env.XDG_CACHE_HOME, 'bazel', name),
  ]
  return [...new Set(roots.filter(Boolean))]
}

/** Output bases whose DO_NOT_BUILD_HERE workspace is gone; ones with a live server are busy. */
export function findOrphanOutputBases(
  root,
  { fsImpl = fs, isAlive = pidAlive, uid = process.getuid?.() } = {},
) {
  const orphans = []
  const busy = []
  for (const name of readdir(fsImpl, root)) {
    if (!OUTPUT_BASE.test(name)) continue
    const path = join(root, name)
    const stat = lstat(fsImpl, path)
    if (!stat?.isDirectory() || (uid !== undefined && stat.uid !== uid)) continue
    const workspace = readText(fsImpl, join(path, 'DO_NOT_BUILD_HERE'))
    if (!isAbsolute(workspace) || lstat(fsImpl, workspace)) continue
    const pid = Number(readText(fsImpl, join(path, 'server', 'server.pid.txt')))
    if (Number.isInteger(pid) && pid > 0 && isAlive(pid)) busy.push({ path, workspace, pid })
    else orphans.push({ path, workspace })
  }
  return { orphans, busy }
}

/** Remove a tree whose directories Bazel left read-only, never following a symlink out of it. */
export function removeTree(path, fsImpl = fs) {
  try {
    fsImpl.rmSync(path, { recursive: true, force: true, maxRetries: 3 })
    return
  } catch (error) {
    if (error.code !== 'EACCES' && error.code !== 'EPERM') throw error
  }
  const stack = [path]
  while (stack.length > 0) {
    const dir = stack.pop()
    const stat = lstat(fsImpl, dir)
    if (!stat?.isDirectory()) continue
    fsImpl.chmodSync(dir, stat.mode | 0o700)
    for (const name of readdir(fsImpl, dir)) stack.push(join(dir, name))
  }
  fsImpl.rmSync(path, { recursive: true, force: true, maxRetries: 3 })
}

function compareVersions(a, b) {
  const left = a.split('.').map(Number)
  const right = b.split('.').map(Number)
  for (let i = 0; i < Math.max(left.length, right.length); i++) {
    const delta = (left[i] ?? 0) - (right[i] ?? 0)
    if (delta !== 0) return delta
  }
  return 0
}

/** Puppeteer browser builds older than the newest `keep` for each browser and platform. */
export function stalePuppeteerBuilds(root, { keep = 2, fsImpl = fs } = {}) {
  const stale = []
  for (const browser of readdir(fsImpl, root)) {
    const groups = new Map()
    for (const build of readdir(fsImpl, join(root, browser))) {
      const match = BROWSER_BUILD.exec(build)
      const path = join(root, browser, build)
      if (!match || !lstat(fsImpl, path)?.isDirectory()) continue
      const builds = groups.get(match[1]) ?? []
      builds.push({ path, version: match[2] })
      groups.set(match[1], builds)
    }
    for (const builds of groups.values()) {
      builds.sort((a, b) => compareVersions(b.version, a.version))
      stale.push(...builds.slice(keep).map((build) => build.path))
    }
  }
  return stale
}

/** Temp-shaped directories this user owns that neither they nor their children touched lately. */
export function findStaleTempDirs(
  roots,
  { now = Date.now(), minAgeMs = 3 * DAY, uid = process.getuid?.(), fsImpl = fs } = {},
) {
  const cutoff = now - minAgeMs
  const seen = new Set()
  const stale = []
  for (const root of roots) {
    let real
    try {
      real = fsImpl.realpathSync(root)
    } catch {
      continue
    }
    if (seen.has(real)) continue
    seen.add(real)
    for (const name of readdir(fsImpl, real)) {
      if (!TEMP_NAMES.some((pattern) => pattern.test(name))) continue
      const path = join(real, name)
      const stat = lstat(fsImpl, path)
      if (!stat?.isDirectory() || stat.mtimeMs > cutoff) continue
      if (uid !== undefined && stat.uid !== uid) continue
      const children = readdir(fsImpl, path).map((child) => lstat(fsImpl, join(path, child)))
      if (children.every((child) => !child || child.mtimeMs <= cutoff)) stale.push(path)
    }
  }
  return stale
}

/** The cache directory golangci-lint uses on this OS, unless GOLANGCI_LINT_CACHE moves it. */
export function golangciCacheDir({ env = process.env, os = platform(), home = homedir() } = {}) {
  if (env.GOLANGCI_LINT_CACHE) return env.GOLANGCI_LINT_CACHE
  if (os === 'darwin') return join(home, 'Library', 'Caches', 'golangci-lint')
  return join(env.XDG_CACHE_HOME || join(home, '.cache'), 'golangci-lint')
}

/** Reclaim lint entries without running the other host cleanup steps. */
export function recoverGolangciCaches({
  dryRun = false,
  env = process.env,
  home = homedir(),
  os = platform(),
  write = (line) => process.stdout.write(`recover-disk-space: ${line}\n`),
} = {}) {
  const pool = join(golangciAdmissionConfig(env).dir, 'caches')
  const caches = new Set([
    golangciCacheDir({ env, os, home }),
    ...readdir(fs, pool)
      .filter((name) => /^slot-\d+$/.test(name))
      .map((name) => join(pool, name)),
  ])
  for (const lintCache of caches) {
    if (!lstat(fs, lintCache)?.isDirectory()) continue
    const capGiB = Number(env.GOLANGCI_CACHE_MAX_GIB ?? '8')
    if (!Number.isFinite(capGiB) || capGiB < 0) {
      write('golangci-lint: GOLANGCI_CACHE_MAX_GIB must be a non-negative number; skipping')
    } else if (dryRun) {
      write(
        `golangci-lint: would evict ${lintCache} entries unused for an hour down to ${capGiB} GiB`,
      )
    } else {
      try {
        const result = evictGoCache({
          dir: lintCache,
          targetBytes: capGiB * GiB,
          minAgeMs: HOUR,
          signature: GOLANGCI_CACHE_SIGNATURE,
        })
        write(
          `golangci-lint: ${lintCache}: ${formatGiB(result.initialBytes)} -> ${formatGiB(result.bytes)} (${result.entriesRemoved} entries)`,
        )
      } catch (error) {
        write(`golangci-lint: skipped ${lintCache}: ${error.message}`)
      }
    }
  }
}

export function parseSwapUsage(text) {
  const used = /used = ([\d.]+)M/.exec(text)
  return used ? Number(used[1]) * 1024 ** 2 : null
}

/** @type {Array<[string, string[]]>} */
const TOOL_CLEANERS = [
  ['npm', ['cache', 'clean', '--force']],
  ['yarn', ['cache', 'clean']],
  ['pnpm', ['store', 'prune']],
  ['uv', ['cache', 'prune']],
  ['bun', ['pm', 'cache', 'rm']],
  ['brew', ['cleanup', '--prune=all']],
]

function freeBytes(path) {
  const { bavail, bsize } = fs.statfsSync(path)
  return Number(bavail) * Number(bsize)
}

export function run({
  dryRun = false,
  env = process.env,
  home = homedir(),
  os = platform(),
  exec = execFileSync,
  write = (line) => process.stdout.write(`recover-disk-space: ${line}\n`),
} = {}) {
  const removeAll = (label, paths) => {
    if (paths.length === 0) return write(`${label}: nothing to remove`)
    if (dryRun) {
      for (const path of paths) write(`${label}: would remove ${path}`)
      return
    }
    let removed = 0
    for (const path of paths) {
      try {
        removeTree(path)
        removed++
      } catch (error) {
        write(`${label}: could not remove ${path}: ${error.message}`)
      }
    }
    write(`${label}: removed ${removed} of ${paths.length}`)
  }

  for (const root of bazelOutputUserRoots({ env, home })) {
    const { orphans, busy } = findOrphanOutputBases(root)
    for (const { path, pid } of busy) write(`bazel: kept ${path}; its server (pid ${pid}) is alive`)
    if (orphans.length > 0 || busy.length > 0) {
      removeAll(
        `bazel output bases of deleted workspaces in ${root}`,
        orphans.map((orphan) => orphan.path),
      )
    }
  }

  recoverGolangciCaches({ dryRun, env, home, os, write })

  const puppeteer = env.PUPPETEER_CACHE_DIR || join(home, '.cache', 'puppeteer')
  if (lstat(fs, puppeteer)?.isDirectory()) {
    removeAll('puppeteer browsers', stalePuppeteerBuilds(puppeteer))
    write(
      'puppeteer: a project pinned to a removed build restores it with `npx puppeteer browsers install`',
    )
  }

  removeAll('stale temp dirs', findStaleTempDirs([tmpdir(), '/tmp', '/var/tmp']))

  // An empty project: bun's cache command refuses to run without a package.json, and with no
  // packageManager field a surrounding repo's pin cannot redirect a manager to another version.
  const project = dryRun ? '' : fs.mkdtempSync(join(tmpdir(), 'recover-disk-space-'))
  if (project) fs.writeFileSync(join(project, 'package.json'), '{ "private": true }\n')
  for (const [tool, args] of TOOL_CLEANERS) {
    if (dryRun) {
      write(`${tool}: would run \`${tool} ${args.join(' ')}\` if installed`)
      continue
    }
    try {
      exec(tool, args, {
        cwd: project,
        stdio: ['ignore', 'ignore', 'pipe'],
        timeout: 10 * 60 * 1000,
        env: { ...env, COREPACK_ENABLE_DOWNLOAD_PROMPT: '0' },
      })
      write(`${tool}: ran \`${tool} ${args.join(' ')}\``)
    } catch (error) {
      if (error.code !== 'ENOENT') write(`${tool}: \`${tool} ${args.join(' ')}\` failed; skipped`)
    }
  }
  if (project) removeTree(project)

  // `npm cache clean` empties _cacache only; npx keeps its installed packages beside it.
  const npx = join(env.npm_config_cache || join(home, '.npm'), '_npx')
  if (lstat(fs, npx)?.isDirectory()) removeAll('npx packages', [npx])

  // xcrun without Xcode opens the Command Line Tools installer, so require simulator state first.
  if (
    os === 'darwin' &&
    lstat(fs, join(home, 'Library', 'Developer', 'CoreSimulator', 'Devices'))
  ) {
    if (dryRun) write('simulators: would run `xcrun simctl delete unavailable`')
    else {
      try {
        exec('xcrun', ['simctl', 'delete', 'unavailable'], {
          stdio: 'ignore',
          timeout: 5 * 60 * 1000,
        })
        write('simulators: deleted devices whose runtime is gone')
      } catch {
        write('simulators: `xcrun simctl delete unavailable` failed; skipped')
      }
    }
  }

  if (os === 'darwin') {
    try {
      const used = parseSwapUsage(
        String(exec('sysctl', ['-n', 'vm.swapusage'], { encoding: 'utf8' })),
      )
      if (used !== null && used >= SWAP_REPORT_BYTES) {
        write(
          `swap holds ${formatGiB(used)} on disk; no cleanup reclaims it. Find the process with \`top -o mem\` and restart it, or reboot`,
        )
      }
    } catch {
      // Reporting only: a host without sysctl simply has nothing to say about swap.
    }
  }
}

function main() {
  const { values } = parseArgs({
    options: { 'dry-run': { type: 'boolean', default: false } },
    strict: true,
  })
  const home = homedir()
  const before = freeBytes(home)
  run({ dryRun: values['dry-run'], home })
  const after = freeBytes(home)
  process.stdout.write(
    `recover-disk-space: free space ${formatGiB(before)} -> ${formatGiB(after)} on the home volume\n`,
  )
}

if (isMainModule(import.meta.url)) main()
