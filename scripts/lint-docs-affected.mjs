#!/usr/bin/env node

// lint-docs-affected.mjs — the check-mode sibling of format-affected.mjs (BOS-371).
//
// The fast `make lint` used to run `pnpm run lint:docs` — a whole-tree
// `prettier --check` over all 558 markdown files (~14s) regardless of what
// changed. This scopes that check to the files changed on the branch, reusing
// format-affected's diff routing (three-dot vs origin/main + staged + unstaged +
// untracked) and its prettier bucket, so the fast loop only verifies what you
// touched. The exhaustive whole-tree `prettier --check` stays in `make lint-all`.
//
// prettier honors `.prettierignore` (which excludes docs/plans/**, biome-owned
// packages, generated payloads, …), so ignored paths are skipped exactly as the
// whole-tree check skips them.
//
//   node scripts/lint-docs-affected.mjs            # check changed files
//   node scripts/lint-docs-affected.mjs a.md b.md  # check an explicit set (test seam)
//
// Exits non-zero when a changed file is mis-formatted; 0 when clean or when
// prettier is unavailable (the whole-tree `lint-all` + pre-commit hook remain the
// hard gate, matching format-affected's missing-tool tolerance).

import { execFileSync } from 'node:child_process'
import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

import { collectChangedFiles, routeFiles } from './format-affected.mjs'

/** True only for an existing regular, non-symlink file. */
function isCheckableFile(file) {
  try {
    return fs.lstatSync(file).isFile()
  } catch {
    return false
  }
}

/** Resolve prettier: prefer node_modules/.bin/prettier, else PATH `prettier`. */
function resolvePrettier() {
  const local = path.resolve('node_modules/.bin/prettier')
  try {
    fs.accessSync(local, fs.constants.X_OK)
    if (fs.statSync(local).isFile()) return local
  } catch {
    // fall through to PATH lookup
  }
  const dirs = (process.env.PATH || '').split(path.delimiter)
  for (const dir of dirs) {
    if (dir === '') continue
    const candidate = path.join(dir, 'prettier')
    try {
      fs.accessSync(candidate, fs.constants.X_OK)
      if (fs.statSync(candidate).isFile()) return 'prettier'
    } catch {
      // keep looking
    }
  }
  return null
}

/**
 * Check the prettier-owned changed files. Returns an exit code (0 clean/skipped,
 * non-zero when prettier reports a mis-formatted file). Side-effecting only via the
 * injected `exec`/`log` (defaulted for the CLI, overridden in tests).
 */
export function run(argv = process.argv.slice(2), opts = {}) {
  const log = opts.log ?? ((line) => console.log(line))
  const files = argv.length > 0 ? argv : collectChangedFiles()
  const { prettier } = routeFiles(files)
  const existing = prettier.filter(isCheckableFile)

  if (existing.length === 0) {
    log('lint-docs-affected: no changed prettier-owned files')
    return 0
  }

  const prettierCmd = 'prettierCmd' in opts ? opts.prettierCmd : resolvePrettier()
  if (!prettierCmd) {
    log('lint-docs-affected: prettier not found — skipping (lint-all is the hard gate)')
    return 0
  }

  const exec =
    opts.exec ??
    ((cmd, args) => {
      execFileSync(cmd, args, { stdio: ['ignore', 'inherit', 'inherit'] })
    })

  log(`lint-docs-affected: prettier --check (${existing.length} file(s))`)
  try {
    exec(prettierCmd, ['--check', ...existing])
  } catch {
    return 1
  }
  return 0
}

import { isMainModule } from '../skills-toolbox/main-module.mjs'

if (isMainModule(import.meta.url)) {
  process.exit(run())
}
