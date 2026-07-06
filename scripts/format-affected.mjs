#!/usr/bin/env node

/**
 * format-affected.mjs — format only the files changed on this branch.
 *
 * A fast, change-scoped formatter for the local/agent edit loop. It formats the
 * union of:
 *   1. branch diff vs BASE_REF (default origin/main, three-dot merge-base),
 *   2. staged (uncommitted) changes,
 *   3. unstaged edits to tracked files,
 *   4. untracked files,
 * routing each ecosystem's formatter to just the changed paths and skipping any
 * ecosystem with no changes.
 *
 * This is the sibling of scripts/format-staged.sh (the pre-commit hook, scoped to
 * the git index) and scripts/select-affected-tests.mjs (the diff-scoped test
 * selector). Unlike the pre-commit hook it does NOT re-stage (`git add`) anything
 * and does NOT skip files that merely have unstaged edits — it formats the working
 * tree in place and leaves staging to the caller.
 *
 * Routing mirrors `make format`'s per-ecosystem ownership, NOT a repo-wide prettier
 * sweep: `services/web/` and `services/marketing/` are biome-owned (each has its own
 * biome.json; their `format` target is `biome check --write` and their `lint` gate is
 * `biome check`) and are `.prettierignore`d — root prettier would silently no-op on
 * them while `make lint`'s biome check still fails. Those dirs route to per-package
 * biome instead. `package.json` is owned by syncpack (+ biome for the web packages),
 * never root prettier. Everything else web-ish/markdown routes to root prettier, and
 * `.go` to gofmt/goimports.
 *
 * It never runs the Go linter or any standalone lint step. `biome
 * check --write` is used only because it IS the repo's canonical web *formatter* (the
 * `format` target for those packages), so matching it is what keeps `make format`
 * diff-free and `make lint`'s biome check green. CI's full `make format` / `make lint`
 * remain the hard gate; this command is the fast local loop only.
 *
 * Shape mirrors select-affected-tests.mjs: pure exported functions plus a thin
 * `import.meta.url === ...` CLI shim. With explicit file args it uses them
 * verbatim (the testability seam); with no args it computes the set from git.
 */

import { execFileSync } from 'node:child_process'
import fs from 'node:fs'
import path from 'node:path'

const PRETTIER_EXTENSIONS = new Set([
  '.ts',
  '.tsx',
  '.js',
  '.jsx',
  '.mjs',
  '.cjs',
  '.json',
  '.md',
  '.mdx',
  '.css',
])

// Biome-owned package roots (see .prettierignore). Files under these route to
// per-package `biome check --write`, never root prettier.
const BIOME_ROOTS = ['services/web/', 'services/marketing/']

// Package roots that live OUTSIDE the pnpm workspace, so syncpack (whose default
// source discovery follows pnpm-workspace.yaml) never touches their package.json.
// Their `make format` target is their own prettier (which globs `*.json`, i.e. the
// manifest), so a `package.json` under one of these routes to the prettier bucket,
// NOT syncpack. services/docs (the Docusaurus site, `pnpm install --ignore-workspace`)
// has no own .prettierrc and inherits the root config, so root prettier reproduces
// its formatting exactly. Everything else under these roots already routes to root
// prettier via the normal extension logic; only package.json needs this exception.
const PRETTIER_PACKAGE_ROOTS = ['services/docs/']

// Extensions biome formats. Anything else under a biome root (e.g. `.md`) is
// dropped — `make format` does not format it either (biome skips it, and the
// dir is `.prettierignore`d), so formatting it here would diverge from CI.
const BIOME_EXTENSIONS = new Set([
  '.ts',
  '.tsx',
  '.js',
  '.jsx',
  '.mjs',
  '.cjs',
  '.json',
  '.jsonc',
  '.css',
])

/** Normalize a raw path: String, trim, backslashes → slashes, strip leading `./`. */
export function normalizePath(file) {
  return String(file ?? '')
    .trim()
    .replaceAll('\\', '/')
    .replace(/^\.\//, '')
}

/**
 * Route changed paths into per-ecosystem buckets. Pure.
 *
 * Returns `{ go, prettier, biome, packageJson }` where `biome` is an object mapping
 * each biome-owned package root (only those with ≥1 file) to its normalized, deduped
 * file list. Paths are normalized and deduped; empty entries ignored. A changed
 * `package.json` in a syncpack-covered (pnpm-workspace) dir sets `packageJson` so
 * repo-wide syncpack runs and is NOT added to the prettier bucket (syncpack, or biome
 * for the web packages, owns it); a `package.json` under a PRETTIER_PACKAGE_ROOT
 * (outside the workspace, e.g. services/docs) instead routes to prettier and does
 * not set the flag.
 */
export function routeFiles(files) {
  const go = new Set()
  const prettier = new Set()
  const biome = new Map(BIOME_ROOTS.map((root) => [root, new Set()]))
  let packageJson = false

  for (const rawFile of files) {
    const file = normalizePath(rawFile)
    if (file === '') {
      continue
    }

    const isPackageJson = path.basename(file) === 'package.json'
    const ext = path.extname(file)

    if (ext === '.go') {
      go.add(file)
      continue
    }

    const biomeRoot = BIOME_ROOTS.find((root) => file.startsWith(root))
    if (biomeRoot) {
      if (isPackageJson) {
        packageJson = true
      }
      if (BIOME_EXTENSIONS.has(ext)) {
        biome.get(biomeRoot).add(file)
      }
      continue
    }

    const prettierPackageRoot = PRETTIER_PACKAGE_ROOTS.some((root) => file.startsWith(root))
    if (isPackageJson && !prettierPackageRoot) {
      // Owned by syncpack, never root prettier (matches `make format`).
      packageJson = true
      continue
    }
    // A package.json under a PRETTIER_PACKAGE_ROOT falls through here: syncpack
    // does not cover it (outside the pnpm workspace), so route it to prettier
    // like that package's own `make format` does — and do NOT set packageJson,
    // to avoid a spurious repo-wide syncpack run for a docs-only manifest change.
    if (PRETTIER_EXTENSIONS.has(ext)) {
      prettier.add(file)
    }
  }

  const biomeOut = {}
  for (const [root, set] of biome) {
    if (set.size > 0) {
      biomeOut[root] = [...set]
    }
  }

  return { go: [...go], prettier: [...prettier], biome: biomeOut, packageJson }
}

/** Collect changed-file lines from a git command, degrading to [] on any failure. */
function gitLines(args) {
  try {
    return execFileSync('git', args, { encoding: 'utf8' }).split(/\r?\n/).filter(Boolean)
  } catch {
    return []
  }
}

/**
 * Union of branch-diff (vs BASE_REF, default origin/main) + staged + unstaged
 * tracked edits + untracked files, normalized and deduped. Each git source is
 * guarded so a failure (e.g. a fresh clone without origin/main, or a detached HEAD)
 * degrades gracefully to the remaining sources rather than crashing.
 * `--diff-filter=ACMR` keeps deletions out.
 *
 * The plain worktree diff (no ref, uncached) is what catches the common edit-loop
 * case: a tracked file edited but not yet staged or committed. Without it the branch
 * diff only sees HEAD, the cached diff only sees the index, and `ls-files --others`
 * only sees untracked files, so an unstaged edit would be skipped entirely.
 */
export function collectChangedFiles() {
  const baseRef = process.env.BASE_REF || 'origin/main'
  const union = new Set()

  const sources = [
    ['diff', '--name-only', '--diff-filter=ACMR', `${baseRef}...HEAD`],
    ['diff', '--cached', '--name-only', '--diff-filter=ACMR'],
    ['diff', '--name-only', '--diff-filter=ACMR'],
    ['ls-files', '--others', '--exclude-standard'],
  ]
  for (const args of sources) {
    for (const file of gitLines(args)) {
      const normalized = normalizePath(file)
      if (normalized !== '') {
        union.add(normalized)
      }
    }
  }

  return [...union]
}

/** True only for an existing REGULAR, NON-SYMLINK file (skips symlinks/deletions). */
function isFormattableFile(file) {
  try {
    return fs.lstatSync(file).isFile()
  } catch {
    return false
  }
}

/** True if `name` resolves to an executable file on PATH. */
function onPath(name) {
  const dirs = (process.env.PATH || '').split(path.delimiter)
  for (const dir of dirs) {
    if (dir === '') {
      continue
    }
    const candidate = path.join(dir, name)
    try {
      fs.accessSync(candidate, fs.constants.X_OK)
      if (fs.statSync(candidate).isFile()) {
        return true
      }
    } catch {
      // not here; keep looking
    }
  }
  return false
}

/** Resolve prettier: prefer node_modules/.bin/prettier, else PATH `prettier`. */
function resolvePrettier() {
  const local = path.resolve('node_modules/.bin/prettier')
  try {
    fs.accessSync(local, fs.constants.X_OK)
    if (fs.statSync(local).isFile()) {
      return local
    }
  } catch {
    // fall through to PATH lookup
  }
  return onPath('prettier') ? 'prettier' : null
}

/** Resolve biome for a package root: prefer its local .bin/biome, else PATH `biome`. */
function resolveBiome(root) {
  const local = path.resolve(root, 'node_modules/.bin/biome')
  try {
    fs.accessSync(local, fs.constants.X_OK)
    if (fs.statSync(local).isFile()) {
      return local
    }
  } catch {
    // fall through to PATH lookup
  }
  return onPath('biome') ? 'biome' : null
}

/**
 * Execute the per-bucket plan against the real formatters. Side-effecting.
 *
 * Prints one line per bucket ONLY when that bucket actually runs. Missing tools →
 * that bucket is skipped silently (exit 0). Returns a non-zero code only when a
 * formatter itself errored (a real failure a human/agent should see).
 */
export function formatFiles(plan, opts = {}) {
  const log = opts.log ?? ((line) => console.log(line))
  const exec = (cmd, args, execOpts = {}) => {
    execFileSync(cmd, args, { stdio: ['ignore', 'inherit', 'inherit'], ...execOpts })
  }
  let hadError = false

  // Order mirrors `make format`: syncpack → go → biome (web) → prettier, so the
  // final writer on each path matches CI (e.g. biome, not syncpack, is last on a
  // web package.json).

  // Syncpack: only when a package.json changed (repo-wide, matches `make format`).
  if (plan.packageJson && onPath('pnpm')) {
    log('format-affected: syncpack (package.json changed)')
    try {
      exec('pnpm', ['syncpack', 'format'])
      exec('pnpm', ['syncpack', 'fix'])
    } catch {
      hadError = true
    }
  }

  // Go bucket: gofmt then goimports (both -w), never a linter.
  const goFiles = plan.go.filter(isFormattableFile)
  if (goFiles.length > 0 && onPath('gofmt')) {
    log(`format-affected: gofmt/goimports (${goFiles.length} file(s))`)
    try {
      exec('gofmt', ['-w', ...goFiles])
    } catch {
      hadError = true
    }
    if (onPath('goimports')) {
      try {
        exec('goimports', ['-w', ...goFiles])
      } catch {
        hadError = true
      }
    }
  }

  // Biome buckets: one `biome check --write` per package root, run from that root
  // with package-relative paths so each package's biome.json applies (matches
  // `make format`'s `$(MAKE) -C services/web format`). This IS the web *formatter*,
  // not an added linter step.
  for (const [root, files] of Object.entries(plan.biome)) {
    const existing = files.filter(isFormattableFile)
    if (existing.length === 0) {
      continue
    }
    const biomeCmd = resolveBiome(root)
    if (!biomeCmd) {
      continue
    }
    const relative = existing.map((file) => path.relative(root, file))
    log(`format-affected: biome check --write (${existing.length} file(s) in ${root})`)
    try {
      exec(biomeCmd, ['check', '--write', ...relative], { cwd: root })
    } catch {
      hadError = true
    }
  }

  // Prettier bucket: a single --write invocation over the changed paths.
  const prettierFiles = plan.prettier.filter(isFormattableFile)
  const prettierCmd = resolvePrettier()
  if (prettierFiles.length > 0 && prettierCmd) {
    log(`format-affected: prettier --write (${prettierFiles.length} file(s))`)
    try {
      exec(prettierCmd, ['--write', ...prettierFiles])
    } catch {
      hadError = true
    }
  }

  return hadError ? 1 : 0
}

/** Entry point: resolve the file list, route it, format. Returns an exit code. */
export function run(argv = process.argv.slice(2), opts = {}) {
  const log = opts.log ?? ((line) => console.log(line))
  const files = argv.length > 0 ? argv : collectChangedFiles()
  const plan = routeFiles(files)

  const nothing =
    plan.go.length === 0 &&
    plan.prettier.length === 0 &&
    Object.keys(plan.biome).length === 0 &&
    !plan.packageJson
  if (nothing) {
    log('format-affected: nothing to format')
    return 0
  }

  return formatFiles(plan, { ...opts, log })
}

if (import.meta.url === `file://${process.argv[1]}`) {
  process.exit(run())
}
