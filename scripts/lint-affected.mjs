#!/usr/bin/env node
// Content-addressed lint runner (BOS-340).
//
// Runs the exact same pinned host `golangci-lint run ./...` as the Makefile,
// per module, cwd:<module> — but skips a module when a content stamp for its
// current lint-relevant inputs already exists in a machine-wide stamp dir.
// Anything that changes findings changes the key (see lint-stamp-lib.mjs);
// nothing else does. Fail-open: if the stamp dir can't be used we lint
// everything and never skip.
//
//   node scripts/lint-affected.mjs [--module <dir>] [--force]
//   LINT_FORCE=1        ≡ --force (bypass stamps)
//   BOSS_LINT_STAMP_DIR overrides ~/.cache/bossanova-lint-stamps

import { execFileSync, spawnSync } from 'node:child_process'
import { createHash } from 'node:crypto'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { moduleLintKey, hashTree } from './lint-stamp-lib.mjs'

const STAMP_TTL_MS = 30 * 24 * 60 * 60 * 1000 // 30 days

// Discover Go modules exactly like the Makefile:
//   $(wildcard lib/*/go.mod services/*/go.mod plugins/*/go.mod) → dirs.
export function discoverModules(repoRoot) {
  const roots = ['lib', 'services', 'plugins']
  const modules = []
  for (const root of roots) {
    const absRoot = path.join(repoRoot, root)
    let entries
    try {
      entries = fs.readdirSync(absRoot, { withFileTypes: true })
    } catch {
      continue
    }
    for (const entry of entries) {
      if (!entry.isDirectory()) continue
      const dir = `${root}/${entry.name}`
      if (fs.existsSync(path.join(repoRoot, dir, 'go.mod'))) {
        modules.push(dir)
      }
    }
  }
  return modules.sort()
}

// Direct local (filesystem) replace targets of a module's go.mod, as
// repo-relative POSIX dirs. Matches both the single-line form
// (`replace X => ../y`, optionally versioned `replace X v1 => ../y v0`) and the
// block form (`replace (` … `X => ../y` … `)`); a local target is one whose
// replacement path begins with `./` or `../`. Self-references are dropped.
function directLocalReplaces(repoRoot, moduleDir) {
  let goMod
  try {
    goMod = fs.readFileSync(path.join(repoRoot, moduleDir, 'go.mod'), 'utf8')
  } catch {
    return []
  }
  const deps = []
  for (const rawLine of goMod.split('\n')) {
    const line = rawLine.replace(/\/\/.*$/, '').trim() // strip trailing // comment
    const arrow = line.indexOf('=>')
    if (arrow === -1) continue
    // The replacement is the first whitespace-delimited token after `=>`.
    const target = line
      .slice(arrow + 2)
      .trim()
      .split(/\s+/)[0]
    if (!target || !(target.startsWith('./') || target.startsWith('../'))) continue
    const absDep = path.resolve(repoRoot, moduleDir, target)
    const relDep = path.relative(repoRoot, absDep).split(path.sep).join('/')
    if (relDep && relDep !== moduleDir) deps.push(relDep)
  }
  return deps
}

// Transitive closure of a module's local module deps — every other local module
// its lint findings can depend on. lint runs in Go workspace mode, so a module
// is type-checked against the LOCAL source of every module it `replace`s to a
// path (e.g. bosso replaces `../bossd`, and its e2e test imports bossd), and
// those deps transitively pull in their own local replaces (bossd → bossalib).
// Every such dep's tree hash must feed the module's key, or an edit to a
// depended-on module's source would leave this module reporting `(cached)` with
// stale findings (under-invalidation). Derived from go.mod, never hardcoded.
export function deriveDeps(repoRoot, moduleDir) {
  const seen = new Set()
  const queue = [...directLocalReplaces(repoRoot, moduleDir)]
  while (queue.length > 0) {
    const dep = queue.shift()
    if (dep === moduleDir || seen.has(dep)) continue
    seen.add(dep)
    for (const next of directLocalReplaces(repoRoot, dep)) {
      if (next !== moduleDir && !seen.has(next)) queue.push(next)
    }
  }
  return [...seen].sort()
}

function golangciVersion() {
  return execFileSync('golangci-lint', ['--version'], { encoding: 'utf8' }).trim()
}

function goVersion() {
  return execFileSync('go', ['version'], { encoding: 'utf8' }).trim()
}

// A fingerprint of the Go build environment that selects which build-constrained
// files golangci-lint analyzes and therefore its findings: GOOS/GOARCH/CGO plus
// GOFLAGS (build tags) and whether workspace mode is on. `go env` reports the
// effective values (honoring env overrides). GOWORK is normalized to on/off —
// its absolute path is worktree-specific, and folding the path in would defeat
// machine-wide stamp sharing. Its content is folded into the config hash below.
function goBuildEnv() {
  const out = execFileSync('go', ['env', 'GOOS', 'GOARCH', 'CGO_ENABLED', 'GOFLAGS', 'GOWORK'], {
    encoding: 'utf8',
  })
  const [goos = '', goarch = '', cgo = '', goflags = '', gowork = ''] = out.split('\n')
  const workspaceFile = gowork === '' || gowork === 'off' ? null : gowork
  const workspace = workspaceFile === null ? 'off' : 'on'
  return {
    fingerprint: `GOOS=${goos};GOARCH=${goarch};CGO_ENABLED=${cgo};GOFLAGS=${goflags};GOWORK=${workspace}`,
    workspaceFile,
  }
}

// Lint-global inputs shared by every module: the root golangci config plus the
// active Go workspace files. A workspace's directives and go.work.sum alter the
// versions golangci-lint type-checks against, so their content must invalidate
// every module's stamp. The path is deliberately not hashed: identical workspace
// content shares a stamp across worktrees, while an explicit GOWORK is correctly
// distinguished from the repo-root go.work by its content.
export function configHashOf(repoRoot, workspaceFile = path.join(repoRoot, 'go.work')) {
  const hash = createHash('sha256')
  const hashFile = (label, file) => {
    hash.update(label)
    hash.update('\0')
    try {
      hash.update(fs.readFileSync(file))
    } catch {
      // Presence↔absence must flip the hash without crashing the runner.
      hash.update('\0absent\0')
    }
    hash.update('\0')
  }
  hashFile('.golangci.yml', path.join(repoRoot, '.golangci.yml'))
  if (workspaceFile === null) {
    hash.update('GOWORK=off\0')
  } else {
    hashFile('go.work', workspaceFile)
    hashFile('go.work.sum', path.join(path.dirname(workspaceFile), 'go.work.sum'))
  }
  return hash.digest('hex')
}

// The cache-skip decision for one module: reuse the stamp only when stamps are
// enabled (dir usable), the run isn't forced, and a stamp for this key exists.
// Fail-open by construction — !stampsEnabled, force, or a missing stamp all route
// to a real lint, never to a skip. Extracted so the gate can be tested directly.
export function isModuleCached({ stampsEnabled, force, stampPath }) {
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
      // Racing GC / permissions — ignore.
    }
  }
}

function parseArgs(argv) {
  let moduleFilter = null
  let force = process.env.LINT_FORCE === '1'
  for (let i = 0; i < argv.length; i++) {
    if (argv[i] === '--module') {
      moduleFilter = argv[++i]
    } else if (argv[i] === '--force') {
      force = true
    }
  }
  return { moduleFilter, force }
}

function main() {
  const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
  const { moduleFilter, force } = parseArgs(process.argv.slice(2))

  const stampDir =
    process.env.BOSS_LINT_STAMP_DIR || path.join(os.homedir(), '.cache', 'bossanova-lint-stamps')

  // Fail open: a stamp dir we can't create means "lint everything, never skip".
  let stampsEnabled = true
  try {
    fs.mkdirSync(stampDir, { recursive: true })
  } catch {
    stampsEnabled = false
  }
  if (stampsEnabled) gcStamps(stampDir)

  const modules = moduleFilter ? [moduleFilter] : discoverModules(repoRoot)
  const buildEnv = goBuildEnv()
  const versions = { golangci: golangciVersion(), go: goVersion(), buildEnv: buildEnv.fingerprint }
  const configHash = configHashOf(repoRoot, buildEnv.workspaceFile)

  const treeCache = new Map()
  const tree = (dir) => {
    if (!treeCache.has(dir)) treeCache.set(dir, hashTree(repoRoot, dir))
    return treeCache.get(dir)
  }

  for (const mod of modules) {
    const deps = deriveDeps(repoRoot, mod)
    const treeHashes = { [mod]: tree(mod) }
    for (const dep of deps) treeHashes[dep] = tree(dep)
    const key = moduleLintKey({ treeHashes, moduleDir: mod, deps, configHash, versions })
    const stampPath = path.join(stampDir, key)

    if (isModuleCached({ stampsEnabled, force, stampPath })) {
      console.log(`==> Linting ${mod} (cached)`)
      continue
    }

    console.log(`==> Linting ${mod}`)
    const result = spawnSync('golangci-lint', ['run', './...'], {
      cwd: path.join(repoRoot, mod),
      stdio: 'inherit',
    })
    if (result.error) throw result.error
    if (result.status !== 0) process.exit(result.status ?? 1)

    if (stampsEnabled) {
      try {
        fs.writeFileSync(stampPath, `${new Date().toISOString()}\n`)
      } catch {
        // Non-fatal: linting still succeeded; we just don't cache it.
      }
    }
  }
}

if (process.argv[1] && fileURLToPath(import.meta.url) === path.resolve(process.argv[1])) {
  main()
}
