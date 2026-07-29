#!/usr/bin/env node
// Content-hash-stamped `node --check` syntax gate for the repo's script files
// (BOS-340 pattern, mirroring the cached Go lint in lint-affected.mjs +
// lint-stamp-lib.mjs). The old `scripts/Makefile` target spawned one `node
// --check` process per file over ~200 .cjs/.mjs files on every run — the
// dominant cost of a warm `make lint` even when no script changed. This skips
// the whole per-file sweep when a stamp for the current file-set content
// already exists in a machine-wide stamp dir, so an unchanged tree is ~instant.
//
// The key is a sha256 over every checked file's content plus the Node version
// (which `node --check` parsing tracks). Anything that changes what the sweep
// would report changes the key; nothing else does. Fail-open by construction: a
// stamp dir we can't use, or a forced run, lints everything and never skips.
//
//   LINT_FORCE=1 / --force   bypass stamps (always re-check)
//   BOSS_LINT_STAMP_DIR      overrides ~/.cache/bossanova-lint-stamps

import { createHash } from 'node:crypto'
import { execFileSync, spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'

const STAMP_TTL_MS = 30 * 24 * 60 * 60 * 1000 // 30 days

// (repo-root-relative dir, extensions) pairs mirroring the file globs the
// scripts/Makefile `lint` target used to feed `node --check`. Kept in lockstep
// with that target: any glob added there must be added here or the stamp would
// report a clean tree while a new file went unchecked.
const LINT_DIRS = [
  ['scripts', ['.cjs', '.mjs']],
  ['scripts/bazel', ['.mjs']],
  ['scripts/changelog', ['.mjs']],
  ['scripts/skill-parity', ['.mjs']],
  ['skills-toolbox', ['.mjs']],
  ['skills-toolbox/callback', ['.mjs']],
  ['skills-toolbox/cron-gates', ['.mjs']],
  ['skills-toolbox/session', ['.mjs']],
  ['skills-toolbox/finalize', ['.mjs']],
  ['skills-toolbox/tracker', ['.mjs']],
]

// Direct (non-recursive) files under `repoRoot/relDir` whose name ends with one
// of `exts`, returned as repo-root-relative POSIX paths. Missing dirs yield [].
function filesInDir(repoRoot, relDir, exts) {
  const abs = path.join(repoRoot, relDir)
  let names
  try {
    names = fs.readdirSync(abs)
  } catch {
    return []
  }
  const out = []
  for (const name of names) {
    if (!exts.some((ext) => name.endsWith(ext))) continue
    const full = path.join(abs, name)
    try {
      if (fs.statSync(full).isFile()) out.push(`${relDir}/${name}`)
    } catch {
      // Vanished between readdir and stat: skip.
    }
  }
  return out
}

// Resolve the exact file set `node --check` covers, repo-root-relative, sorted.
// Includes the wildcard-middle `.claude/skills/*/gate/*.mjs` entries.
export function resolveScriptFiles(repoRoot) {
  const out = []
  for (const [relDir, exts] of LINT_DIRS) out.push(...filesInDir(repoRoot, relDir, exts))
  let skills = []
  try {
    skills = fs.readdirSync(path.join(repoRoot, '.claude', 'skills'))
  } catch {
    skills = []
  }
  for (const skill of skills) {
    out.push(...filesInDir(repoRoot, `.claude/skills/${skill}/gate`, ['.mjs']))
  }
  return out.sort()
}

// Deterministic hex sha256 over the Node version and every file's
// (relpath, sha256(content)). `files` is [{ path, content }]; order-independent
// (sorted internally). Node version is folded in because the same source can
// parse differently across `node --check` runtimes.
export function computeScriptsLintKey({ files, nodeVersion }) {
  const hash = createHash('sha256')
  hash.update('node\0')
  hash.update(nodeVersion)
  hash.update('\n')
  const sorted = [...files].sort((a, b) => (a.path < b.path ? -1 : a.path > b.path ? 1 : 0))
  for (const file of sorted) {
    hash.update(file.path)
    hash.update('\0')
    hash.update(createHash('sha256').update(file.content).digest('hex'))
    hash.update('\n')
  }
  return hash.digest('hex')
}

// Reuse the stamp only when stamps are enabled, the run isn't forced, and a
// stamp for this key exists. Fail-open: any of those false routes to a re-check.
export function isCached({ stampsEnabled, force, stampPath }) {
  return Boolean(stampsEnabled) && !force && fs.existsSync(stampPath)
}

// Best-effort GC: drop stamps older than the TTL so the dir stays bounded.
export function gcStamps(stampDir) {
  let names
  try {
    names = fs.readdirSync(stampDir)
  } catch {
    return
  }
  const now = Date.now()
  for (const name of names) {
    const p = path.join(stampDir, name)
    try {
      if (now - fs.statSync(p).mtimeMs > STAMP_TTL_MS) fs.unlinkSync(p)
    } catch {
      // Racing GC / permissions: ignore.
    }
  }
}

function main() {
  const repoRoot = execFileSync('git', ['rev-parse', '--show-toplevel'], {
    encoding: 'utf8',
  }).trim()

  const relFiles = resolveScriptFiles(repoRoot)
  const files = relFiles.map((rel) => ({
    path: rel,
    content: fs.readFileSync(path.join(repoRoot, rel)),
  }))
  const key = computeScriptsLintKey({ files, nodeVersion: process.version })

  const force = process.env.LINT_FORCE === '1' || process.argv.includes('--force')
  const stampDir =
    process.env.BOSS_LINT_STAMP_DIR || path.join(os.homedir(), '.cache', 'bossanova-lint-stamps')

  // Fail open: a stamp dir we can't create means "check everything, never skip".
  let stampsEnabled = true
  try {
    fs.mkdirSync(stampDir, { recursive: true })
  } catch {
    stampsEnabled = false
  }
  if (stampsEnabled) gcStamps(stampDir)

  const stampPath = path.join(stampDir, `scripts-lint-${key}`)
  if (isCached({ stampsEnabled, force, stampPath })) {
    console.log(`==> Checking scripts syntax (cached, ${files.length} files)`)
    return
  }

  for (const rel of relFiles) {
    console.log(`==> Checking ${rel}`)
    const result = spawnSync(process.execPath, ['--check', rel], {
      cwd: repoRoot,
      stdio: 'inherit',
    })
    if (result.status !== 0) {
      process.exitCode = result.status ?? 1
      return
    }
  }

  if (stampsEnabled) {
    try {
      fs.writeFileSync(stampPath, `${new Date().toISOString()}\n`)
    } catch {
      // Non-fatal: the check passed; we just don't cache it.
    }
  }
}

import { isMainModule } from '../skills-toolbox/main-module.mjs'

if (isMainModule(import.meta.url)) {
  main()
}
