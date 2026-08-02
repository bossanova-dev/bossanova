// toolbox-drift.mjs — compare an INSTALLED skill toolbox against the canonical helper
// source tree and report drift as a single warning line.
//
// Scope limit (load-bearing, do not "fix"): the INSTALLED tree is the enumeration domain.
// Each skill installs a deliberate subset of the source helpers, so a source file that is
// not installed is NOT drift and is never reported. Enumerating the source instead would
// emit a phantom drift for every un-vendored helper, on a perfectly clean tree, on every
// run.
//
// The probe is an observation, never a gate: it prints at most one line to stderr and
// always exits 0 — including when the source tree is absent, which is the normal case for
// a skill installed globally into repos that carry no helper source at all.
import { createHash } from 'node:crypto'
import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

// The direct-invocation predicate has exactly one definition repo-wide (enforced by
// main-module.test.mjs), so the probe shares it rather than inlining a second copy — every
// skill that vendors this file already vendors main-module.mjs alongside it.
import { isMainModule } from './main-module.mjs'

const SOURCE_DIR_NAME = 'skills-toolbox'

export function hashFile(file) {
  return createHash('sha256').update(fs.readFileSync(file)).digest('hex')
}

function isDir(target) {
  try {
    return fs.statSync(target).isDirectory()
  } catch {
    return false
  }
}

// Report resolved paths: an installed skill directory is commonly reached through a
// symlink, and printing the symlinked form has already invited a wasted investigation
// into two paths that name the same file.
function resolved(target) {
  try {
    return fs.realpathSync(target)
  } catch {
    return path.resolve(target)
  }
}

// Only real files and directories are enumerated: a symlinked entry is neither, so it is
// skipped rather than followed. Installers copy, so this costs nothing today, and erring
// toward under-reporting keeps a link loop from turning the probe into the failure it exists
// to warn about.
function walk(dir, prefix = '') {
  const out = []
  for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
    const rel = prefix ? `${prefix}/${entry.name}` : entry.name
    if (entry.isDirectory()) out.push(...walk(path.join(dir, entry.name), rel))
    else if (entry.isFile()) out.push(rel)
  }
  return out
}

// Locate the canonical helper source: an explicit path, else $BOSS_TOOLBOX_SOURCE, else the
// nearest ancestor of cwd that contains a source directory (so the probe still works when
// invoked from a subdirectory). Returns null when there is none — never an error.
export function resolveSourceDir({ explicit, env = process.env, cwd = process.cwd() } = {}) {
  const named = explicit || env.BOSS_TOOLBOX_SOURCE
  if (named) return isDir(named) ? named : null
  let dir = path.resolve(cwd)
  for (;;) {
    const probe = path.join(dir, SOURCE_DIR_NAME)
    if (isDir(probe)) return probe
    const parent = path.dirname(dir)
    if (parent === dir) return null
    dir = parent
  }
}

export function compareToolbox({ installedDir, sourceDir }) {
  const result = {
    sourcePresent: Boolean(sourceDir) && isDir(sourceDir),
    installedDir: resolved(installedDir),
    sourceDir: sourceDir ? resolved(sourceDir) : null,
    checked: 0,
    drifted: [],
  }
  if (!result.sourcePresent) return result
  let files
  try {
    files = walk(installedDir)
  } catch {
    return result
  }
  for (const file of files) {
    result.checked += 1
    const installedPath = path.join(installedDir, file)
    const sourcePath = path.join(sourceDir, file)
    let sourceStat = null
    try {
      sourceStat = fs.statSync(sourcePath)
    } catch {
      /* no counterpart */
    }
    if (!sourceStat || !sourceStat.isFile()) {
      result.drifted.push({ file, reason: 'absent-from-source' })
      continue
    }
    try {
      // Identical bytes can still be a broken install: a copy that LOST its executable bit
      // hashes the same yet fails the moment it is invoked directly. Only that direction is
      // drift. Installers routinely hand out a bit the source does not carry (measured: 9 of
      // the helpers installed by the skills here are 755 against a 644 source), so treating a
      // gained bit as drift would warn on nearly every clean install and make the one warning
      // line worthless.
      if (hashFile(installedPath) !== hashFile(sourcePath)) {
        result.drifted.push({ file, reason: 'content' })
      } else if (sourceStat.mode & 0o111 && !(fs.statSync(installedPath).mode & 0o111)) {
        result.drifted.push({ file, reason: 'mode' })
      }
    } catch {
      /* unreadable on either side — not something a warning can help with */
    }
  }
  return result
}

function parseArgs(argv) {
  const args = { toolbox: null, source: null }
  for (let i = 0; i < argv.length; i += 1) {
    if (argv[i] === '--toolbox') args.toolbox = argv[i + 1] ?? null
    else if (argv[i] === '--source') args.source = argv[i + 1] ?? null
  }
  return args
}

export function main(argv, { stderr = process.stderr, env = process.env, cwd } = {}) {
  try {
    const args = parseArgs(argv)
    const installedDir = args.toolbox || path.dirname(fileURLToPath(import.meta.url))
    const report = compareToolbox({
      installedDir,
      sourceDir: resolveSourceDir({ explicit: args.source, env, cwd: cwd ?? process.cwd() }),
    })
    if (report.sourcePresent && report.drifted.length > 0) {
      const names = report.drifted.map((entry) => entry.file).join(', ')
      stderr.write(
        `boss-toolbox-drift: ${report.drifted.length} of ${report.checked} installed helpers ` +
          `differ from source (${names}) ` +
          `[installed=${report.installedDir} source=${report.sourceDir}] - ` +
          `re-vendor the toolbox and reinstall the skills; continuing.\n`,
      )
    }
  } catch {
    /* the probe warns or stays silent; it never becomes the failure it is reporting on */
  }
  return 0
}

if (isMainModule(import.meta.url)) {
  process.exit(main(process.argv.slice(2)))
}
