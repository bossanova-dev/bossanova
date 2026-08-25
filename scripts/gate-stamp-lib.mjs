import { createHash } from 'node:crypto'
import { execFileSync } from 'node:child_process'
import fs from 'node:fs'
import path from 'node:path'
import { hashTree } from './lint-stamp-lib.mjs'

const STAMP_TTL_MS = 30 * 24 * 60 * 60 * 1000

function sha256Hex(data) {
  return createHash('sha256').update(data).digest('hex')
}

function canonicalize(value) {
  if (Array.isArray(value)) return value.map(canonicalize)
  if (value && typeof value === 'object') {
    const out = {}
    for (const key of Object.keys(value).sort()) out[key] = canonicalize(value[key])
    return out
  }
  return value
}

export function gateKey({ treeHash, command, baseCommit, versions }) {
  return sha256Hex(
    JSON.stringify(
      canonicalize({
        baseCommit,
        command,
        treeHash,
        versions,
      }),
    ),
  )
}

export function normalizeGateSite(site) {
  const text = String(site || '').trim()
  const make = text.match(/(?:^|\s)(?:[\w_]+=.*\s+)*make\s+(?:-[A-Za-z0-9]+\s+)*([A-Za-z0-9_-]+)/)
  if (make) return make[1]
  return text
}

export function eligibleGate(config, site) {
  const normalized = normalizeGateSite(site)
  const entry = config?.gateCache?.eligible?.[normalized]
  if (!entry) return { eligible: false, site: normalized, reason: 'not declared cache-eligible' }
  return {
    eligible: entry.cacheable === true,
    site: normalized,
    reason: String(entry.reason || ''),
  }
}

export function computeTreeHash(repoRoot) {
  return hashTree(repoRoot, '.')
}

export function resolveBaseCommit(repoRoot, baseRef) {
  return execFileSync('git', ['merge-base', baseRef, 'HEAD'], {
    cwd: repoRoot,
    encoding: 'utf8',
  }).trim()
}

export function expansionVersions(env = process.env) {
  const goVersion = safeOutput('go', ['version'])
  const bazelVersion = safeOutput('bazel', ['version'])
  return {
    node: process.version,
    go: goVersion,
    bazel: bazelVersion,
    RACE: env.RACE || '',
    BOSS_NO_BAZEL: env.BOSS_NO_BAZEL || '',
    BAZEL_TEST_FLAGS: env.BAZEL_TEST_FLAGS || '',
    BAZEL_TEST_EXTRA_FLAGS: env.BAZEL_TEST_EXTRA_FLAGS || '',
    MAKEFLAGS: env.MAKEFLAGS || '',
    MAKEOVERRIDES: env.MAKEOVERRIDES || '',
  }
}

function safeOutput(cmd, args) {
  try {
    return execFileSync(cmd, args, { encoding: 'utf8', stdio: ['ignore', 'pipe', 'ignore'] }).trim()
  } catch {
    return 'unavailable'
  }
}

export function makeStampDir(baseDir) {
  try {
    fs.mkdirSync(baseDir, { recursive: true })
    const probe = path.join(baseDir, `.write-probe-${process.pid}-${Date.now()}`)
    const moved = `${probe}.ok`
    fs.writeFileSync(probe, '')
    fs.renameSync(probe, moved)
    fs.unlinkSync(moved)
    gcStamps(baseDir)
    return { ok: true, dir: baseDir }
  } catch {
    return { ok: false, dir: baseDir }
  }
}

export function readStamp(stampDir, key) {
  const file = path.join(stampDir, `gate-${key}.json`)
  try {
    const raw = fs.readFileSync(file, 'utf8')
    const stamp = JSON.parse(raw)
    if (!stamp || stamp.status !== 'success') return { hit: false, corrupt: false, file }
    return { hit: true, stamp, file }
  } catch (err) {
    if (err?.code === 'ENOENT') return { hit: false, corrupt: false, file }
    return { hit: false, corrupt: true, file }
  }
}

export function recordStamp(stampDir, key, stamp) {
  if (stamp.exitStatus !== 0) return false
  const file = path.join(stampDir, `gate-${key}.json`)
  try {
    fs.writeFileSync(
      file,
      JSON.stringify(
        {
          ...stamp,
          status: 'success',
          recordedAt: stamp.recordedAt || new Date().toISOString(),
        },
        null,
        2,
      ) + '\n',
    )
    return true
  } catch {
    return false
  }
}

export function gcStamps(stampDir, now = Date.now()) {
  let names
  try {
    names = fs.readdirSync(stampDir)
  } catch {
    return
  }
  for (const name of names) {
    if (!name.startsWith('gate-')) continue
    const file = path.join(stampDir, name)
    try {
      if (now - fs.statSync(file).mtimeMs > STAMP_TTL_MS) fs.unlinkSync(file)
    } catch {
      // Best-effort GC only.
    }
  }
}

export function branchAddsOrRenames(repoRoot, baseRef) {
  const committed = gitMaybe(repoRoot, ['diff', '--name-status', `${baseRef}...HEAD`])
  if (statusTextHasAddOrRename(committed)) return true

  const status = gitMaybe(repoRoot, ['status', '--porcelain', '-z', '--untracked-files=all'])
  const records = status.split('\0')
  for (let i = 0; i < records.length; i++) {
    const rec = records[i]
    if (!rec) continue
    const xy = rec.slice(0, 2)
    if (xy.includes('A') || xy.includes('R') || xy === '??') return true
    if (xy[0] === 'R' || xy[0] === 'C' || xy[1] === 'R' || xy[1] === 'C') i++
  }
  return false
}

function statusTextHasAddOrRename(text) {
  for (const line of text.split('\n')) {
    if (!line) continue
    const status = line.split('\t')[0]
    if (status.startsWith('A') || status.startsWith('R')) return true
  }
  return false
}

function gitMaybe(repoRoot, args) {
  return execFileSync('git', args, { cwd: repoRoot, encoding: 'utf8', maxBuffer: 1024 * 1024 * 64 })
}
