#!/usr/bin/env node

// Ratchet for hand-rolled CLI flag parsers in scripts/ and skills-toolbox/. A parser written as
// `argv[i] === '--foo'` or a `parseFlags` loop that collects any `--name value` pair accepts a
// misnamed flag as if it were absent, and a skill reading only `$?` then proceeds on a vacuous
// verdict (ci-watch classify, pr-check-state, commit-status: #2642, #2644, #2779). `parseArgs`
// from `node:util` with its default `strict: true` rejects an unknown flag, a missing value, and
// a value on a boolean flag, so new CLIs use it. Files that already hand-roll a parser live in
// the baseline; a stale baseline entry fails so the list only shrinks.
//
// A file that matches for another reason (it analyses someone else's command line) takes an
// inline marker carrying a reason, an expiry date, and the approving human:
//   // flag-parser-exempt: <reason>; expires YYYY-MM-DD; approved-by <name>

import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

import { isMainModule } from '../skills-toolbox/main-module.mjs'
import { compareBaseline, readBaseline } from './guards/baseline.mjs'

const REPO_ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
const SCAN_ROOTS = ['scripts', 'skills-toolbox']
const SKIP_DIRS = new Set(['node_modules', 'fixtures', 'testdata'])
const BASELINE_PATH = 'scripts/cli-flag-parsers-baseline.json'
const OWN_FILE = path.join('scripts', 'check-cli-flag-parsers.mjs')

// A flag literal compared, switched on, or looked up: the shapes every hand-rolled parser uses.
const FLAG_READS = [
  /(?:===|!==|\bcase)\s*(['"`])--[A-Za-z]/,
  /(['"`])--[A-Za-z][\w-]*=?\1\s*(?:===|!==)/,
  /\.(?:startsWith|includes|indexOf|has|get)\(\s*(['"`])--/,
]
const STRICT_FALSE = /\bstrict\s*:\s*false\b/
const EXEMPT =
  /flag-parser-exempt:\s*(.+?);\s*expires\s+(\d{4}-\d{2}-\d{2});\s*approved-by\s+(\S.*)$/m
const EXEMPT_MARKER = /flag-parser-exempt:/

const REMEDY =
  "parse argv with `parseArgs` from 'node:util' (leave `strict` at its default true, " +
  'add `allowPositionals: true` for a verb) instead of comparing `--flag` literals by hand'

function walk(dir) {
  let entries
  try {
    entries = fs.readdirSync(dir, { withFileTypes: true })
  } catch {
    return []
  }
  return entries.flatMap((entry) => {
    const file = path.join(dir, entry.name)
    if (entry.isDirectory()) return SKIP_DIRS.has(entry.name) ? [] : walk(file)
    if (!/\.(?:mjs|cjs|js)$/.test(entry.name) || /\.test\.(?:mjs|cjs|js)$/.test(entry.name)) {
      return []
    }
    return [file]
  })
}

// Returns why `source` counts as a hand-rolled parser, or null when it does not.
export function classifySource(source) {
  if (STRICT_FALSE.test(source)) return 'passes `strict: false` to parseArgs'
  if (FLAG_READS.some((re) => re.test(source))) return 'compares `--flag` literals by hand'
  return null
}

// Returns an error string for a malformed or expired exemption, or null for a valid one.
export function exemptionError(source, today) {
  const match = EXEMPT.exec(source)
  if (!match) {
    return 'flag-parser-exempt marker must read `<reason>; expires YYYY-MM-DD; approved-by <name>`'
  }
  const [, reason, expires] = match
  if (reason.trim().length === 0) return 'flag-parser-exempt marker has an empty reason'
  if (Number.isNaN(Date.parse(expires))) return `flag-parser-exempt expiry ${expires} is not a date`
  if (expires < today) return `flag-parser-exempt expired on ${expires}`
  return null
}

export function checkCliFlagParsers({
  repoRoot = REPO_ROOT,
  today = new Date().toISOString().slice(0, 10),
} = {}) {
  const { entries, errors } = readBaseline(repoRoot, BASELINE_PATH)
  const failures = [...errors]
  const offenders = new Map()

  for (const root of SCAN_ROOTS) {
    for (const file of walk(path.join(repoRoot, root))) {
      const relative = path.relative(repoRoot, file)
      if (relative === OWN_FILE) continue
      const source = fs.readFileSync(file, 'utf8')
      const reason = classifySource(source)
      if (EXEMPT_MARKER.test(source)) {
        const problem = exemptionError(source, today)
        if (problem) failures.push(`${relative}: ${problem}`)
        continue
      }
      if (reason) offenders.set(relative, reason)
    }
  }

  const { newViolations, staleEntries } = compareBaseline({
    baselinePath: BASELINE_PATH,
    entries,
    offenders: offenders.keys(),
    remedy: REMEDY,
  })
  for (const relative of newViolations) {
    failures.push(`${relative}: ${offenders.get(relative)} — ${REMEDY}`)
  }
  for (const entry of staleEntries) {
    failures.push(
      `${BASELINE_PATH}: ${entry} no longer hand-rolls a flag parser (or is gone) — remove it`,
    )
  }

  return { failures, offenders: [...offenders.keys()].sort(), baselined: entries.length }
}

if (isMainModule(import.meta.url)) {
  const { failures, offenders, baselined } = checkCliFlagParsers()
  if (failures.length > 0) {
    console.error('CLI flag parser check failed:')
    for (const failure of failures) console.error(`  ${failure}`)
    process.exit(1)
  }
  console.log(`CLI flag parsers OK (${offenders.length} hand-rolled, ${baselined} baselined)`)
}
