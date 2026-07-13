#!/usr/bin/env node

// check-go-format.mjs — the cheap standalone whole-tree Go format gate (BOS-371).
//
// golangci-lint used to carry a `formatters:` (gofmt + goimports) block that
// *re-checked* formatting on every `make lint`, dominating its wall-time (measured
// bossd cold 22s → 7s, ~3× without it). Formatting is already *applied* by `make
// format` / `format-affected` / the pre-commit hook, so golangci only slowly
// re-verified it. This gate replaces that re-check with a ~1s standalone pass:
// `gofmt -l` + `goimports -l` over every tracked `.go` file, so a `--no-verify`
// commit (cron agents) still can't drift formatting unnoticed.
//
// Generated code under any `gen/` directory is excluded — buf owns its import
// grouping and goimports would fight it (exactly why the old golangci
// `formatters:` block also excluded `gen`).
//
//   node scripts/check-go-format.mjs            # check the whole tree
//   node scripts/check-go-format.mjs a.go b.go  # check an explicit file set (test seam)
//
// Exits non-zero and lists the offending files when any need gofmt/goimports;
// exits 0 (silent-ish) when the tree is clean. Fail-safe on tooling errors: a
// gofmt/goimports that cannot run is a hard failure, never a silent pass.

import { createHash } from 'node:crypto'
import { execFileSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const STAMP_TTL_MS = 30 * 24 * 60 * 60 * 1000 // 30 days

// A path segment of exactly `gen` (e.g. `lib/bossalib/gen/...`) marks buf-generated
// Go output. Matches `gen/` at the start or after a slash; not substrings like
// `gendata`.
export function isGeneratedGoPath(file) {
  return /(^|\/)gen\//.test(file)
}

/** Tracked `.go` files (via `git ls-files`), excluding any under a `gen/` dir. */
export function collectGoFiles(repoRoot) {
  const out = execFileSync('git', ['ls-files', '-z', '*.go'], {
    cwd: repoRoot,
    encoding: 'utf8',
    maxBuffer: 64 * 1024 * 1024,
  })
  return out
    .split('\0')
    .filter(Boolean)
    .filter((file) => !isGeneratedGoPath(file))
    .sort()
}

// ── Content-stamp cache (BOS-340 pattern, mirroring lint-affected.mjs +
// lint-scripts.mjs) ──────────────────────────────────────────────────────────
// The whole-tree gofmt/goimports sweep is cheap per-file but runs over every
// tracked .go file on every `make lint` — a few seconds on a large tree. Skip
// it when a stamp for the current .go content + tool versions already exists in
// a machine-wide stamp dir. Fail-open; LINT_FORCE=1 / --force bypasses.

// Deterministic hex sha256 over the tool versions and every checked file's
// (relpath, sha256(content)). `files` is [{ path, content }] (order-independent);
// `toolVersions` folds in `go version` + the resolved goimports invocation, since
// a gofmt/goimports upgrade can change what the gate flags.
export function goFormatKey({ files, toolVersions }) {
  const hash = createHash('sha256')
  hash.update('tools\0')
  hash.update(toolVersions)
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

/** Split a list into fixed-size chunks so a huge argv never overflows ARG_MAX. */
export function chunk(items, size) {
  const chunks = []
  for (let i = 0; i < items.length; i += size) {
    chunks.push(items.slice(i, i + size))
  }
  return chunks
}

/**
 * Run one `-l` (list) formatter over the files and return the set of paths it
 * reports as needing formatting. `run(cmd, args)` returns stdout; injected in
 * tests. Paths are normalized to repo-relative POSIX for stable comparison.
 */
export function listUnformatted({ label, cmd, baseArgs, files, run, repoRoot }) {
  const flagged = new Set()
  for (const batch of chunk(files, 400)) {
    let stdout
    try {
      stdout = run(cmd, [...baseArgs, ...batch])
    } catch (err) {
      throw new Error(`${label} failed to run: ${err.message}`)
    }
    for (const line of String(stdout).split(/\r?\n/)) {
      const trimmed = line.trim()
      if (trimmed === '') continue
      const abs = path.isAbsolute(trimmed) ? trimmed : path.join(repoRoot, trimmed)
      flagged.add(path.relative(repoRoot, abs).split(path.sep).join('/'))
    }
  }
  return flagged
}

/** True if `name` resolves to an executable file on PATH. */
export function onPath(name) {
  for (const dir of (process.env.PATH || '').split(path.delimiter)) {
    if (dir === '') continue
    try {
      const candidate = path.join(dir, name)
      fs.accessSync(candidate, fs.constants.X_OK)
      if (fs.statSync(candidate).isFile()) return true
    } catch {
      // not here; keep looking
    }
  }
  return false
}

/**
 * Resolve how to invoke goimports in `-l` mode. Prefer the standalone `goimports`
 * binary on PATH: it is a plain executable that never touches the Go module graph.
 * `go tool goimports` (the fallback when the binary is absent) resolves via
 * lib/bossalib's go.mod `tool` directive but, being a `go` subprocess, COMPLETES a
 * missing `go.work.sum` hash on first run — which would leave a dirty tree after
 * every `make lint`. So the binary is strongly preferred; the fallback keeps the
 * gate working on a bare host at the cost of that one-time sum completion.
 */
export function resolveGoimports() {
  return onPath('goimports')
    ? { label: 'goimports -l', cmd: 'goimports', baseArgs: ['-l'] }
    : { label: 'go tool goimports -l', cmd: 'go', baseArgs: ['tool', 'goimports', '-l'] }
}

/**
 * Check every file with gofmt and goimports. Returns the sorted union of paths
 * flagged by either. `run` defaults to a real execFileSync; tests inject a fake.
 * `goimports` defaults to the PATH-preferring resolver (overridable in tests).
 */
export function checkFormat({ repoRoot, files, run, goimports = resolveGoimports() }) {
  if (files.length === 0) return []
  const gofmtFlagged = listUnformatted({
    label: 'gofmt -l',
    cmd: 'gofmt',
    baseArgs: ['-l'],
    files,
    run,
    repoRoot,
  })
  const goimportsFlagged = listUnformatted({
    label: goimports.label,
    cmd: goimports.cmd,
    baseArgs: goimports.baseArgs,
    files,
    run,
    repoRoot,
  })
  return [...new Set([...gofmtFlagged, ...goimportsFlagged])].sort()
}

function defaultRun(cmd, args, repoRoot) {
  return execFileSync(cmd, args, {
    cwd: repoRoot,
    encoding: 'utf8',
    maxBuffer: 64 * 1024 * 1024,
  })
}

function main(argv) {
  const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
  const explicit = argv.filter((a) => !a.startsWith('-'))
  const files = explicit.length > 0 ? explicit : collectGoFiles(repoRoot)
  const run = (cmd, args) => defaultRun(cmd, args, repoRoot)
  const goimports = resolveGoimports()

  // Stamp the whole-tree run only; an explicit file set is a targeted check /
  // test seam, not worth stamping. Fail-open: a stamp dir we can't use just
  // runs the full sweep.
  const wholeTree = explicit.length === 0
  const force = process.env.LINT_FORCE === '1' || argv.includes('--force')
  let stampPath = null
  let stampsEnabled = false
  if (wholeTree) {
    const toolVersions = `${defaultRun('go', ['version'], repoRoot).trim()} | ${goimports.label}`
    const withContent = files.map((rel) => ({
      path: rel,
      content: fs.readFileSync(path.join(repoRoot, rel)),
    }))
    const key = goFormatKey({ files: withContent, toolVersions })
    const stampDir =
      process.env.BOSS_LINT_STAMP_DIR || path.join(os.homedir(), '.cache', 'bossanova-lint-stamps')
    try {
      fs.mkdirSync(stampDir, { recursive: true })
      stampsEnabled = true
    } catch {
      stampsEnabled = false
    }
    if (stampsEnabled) {
      gcStamps(stampDir)
      stampPath = path.join(stampDir, `go-format-${key}`)
      if (!force && fs.existsSync(stampPath)) {
        console.log(`Go format OK (cached, ${files.length} file(s))`)
        return
      }
    }
  }

  const offenders = checkFormat({ repoRoot, files, run, goimports })
  if (offenders.length > 0) {
    console.error(
      'Go files are not formatted (run `make format-affected` or `gofmt -w`/`goimports -w`):',
    )
    for (const file of offenders) console.error(`  - ${file}`)
    process.exit(1)
  }

  if (stampsEnabled && stampPath) {
    try {
      fs.writeFileSync(stampPath, `${new Date().toISOString()}\n`)
    } catch {
      // Non-fatal: the check passed; we just don't cache it.
    }
  }
  console.log(`Go format OK (${files.length} file(s) checked)`)
}

if (process.argv[1] && fileURLToPath(import.meta.url) === path.resolve(process.argv[1])) {
  main(process.argv.slice(2))
}
