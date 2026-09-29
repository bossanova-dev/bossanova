#!/usr/bin/env node

// ratchet-report — pinned beside measured for every size gate and every prose-pin baseline (BOS-1341).
//
// WHY THIS EXISTS. A size budget, an exact size pin and a prose-pin count are MEASUREMENTS of the
// tree, not mergeable data. When two branches both move one, a rebase either conflicts on the
// constant or merges cleanly and leaves a number nothing re-measured. A descending budget stays
// green over an over-stated number, so the lie survives as silent headroom. The resolution rule is
// "re-derive the value from the tree", and this report is the one command that does it: it runs
// every suite that calls `assertExactSize` / `assertDescendingBudget` once with
// `SIZE_RATCHET_REPORT_DIR` set, reads the rows the library recorded, adds the per-file prose-pin
// counts from scripts/check-prose-pins.mjs, and prints each pinned value beside its measurement —
// including budgets that pass with headroom, which no gate otherwise reports.
//
// IT FAILS CLOSED. An over-budget or mismatched row, a prose-pin growth/shrink/stale entry, a
// selected suite that recorded from fewer distinct call sites in its own file than it has
// primitive call sites outside comments and literals (one crashed, was skipped, or returned
// early), and a non-zero suite run each red it. A row counts for the suite whose process recorded
// it, not the `constFile` its caller typed; a row with no readable call site counts for nothing.
// An empty measurement never passes.
//
// `make post-rebase-check` runs it, so both unattended rebasers see it once a rebase completes.
//
// RESIDUAL — a green run does NOT establish that a pinned value is the RIGHT budget (only that the
// tree fits it), that a size gate outside `scripts/*.test.mjs` exists, or that the written ledger
// prose beside a constant is true; scripts/check-raw-size-ratchets.mjs checks that arithmetic. A
// loop that skips some iterations still records its one call site, and a primitive called
// through a wrapper in another file is not counted.

import { spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

import { isMainModule } from '../skills-toolbox/main-module.mjs'
import { PROSE_PIN_BASELINE, measureProsePins } from './check-prose-pins.mjs'
import { REPORT_DIR_ENV } from './size-ratchet-lib.mjs'

const REPO_ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')

// The library's own fixture callers: their rows describe fake artifacts, and the library suite
// sets the report variable itself.
export const FIXTURE_SUITES = [
  'scripts/size-ratchet-lib.test.mjs',
  'scripts/check-raw-size-ratchets.test.mjs',
]

const LIB_IMPORT = /from\s+['"]\.\/size-ratchet-lib\.mjs['"]/
const PRIMITIVE_CALL = /\b(?:assertExactSize|assertDescendingBudget)\s*\(/
const PRIMITIVE_CALLS = new RegExp(PRIMITIVE_CALL.source, 'g')

// A `/` after one of these (or at the start) opens a regex literal rather than dividing.
const REGEX_PRECEDERS = new Set('(,=:[!&|?{};+-*%<>~^')

/**
 * `text` with comments and string, template and regex literal contents blanked to spaces,
 * newlines kept, so a primitive named in prose is not counted as a call. Quote strings and regex
 * literals end at a newline, which bounds a misread to its own line.
 */
export function blankCommentsAndStrings(text) {
  let out = ''
  let mode = null
  let inClass = false
  let last = ''
  for (let i = 0; i < text.length; i++) {
    const ch = text[i]
    const next = text[i + 1]
    if (mode === null) {
      if (ch === '/' && next === '/') mode = 'line'
      else if (ch === '/' && next === '*') mode = 'block'
      else if (ch === "'" || ch === '"' || ch === '`') mode = ch
      else if (ch === '/' && (last === '' || REGEX_PRECEDERS.has(last))) mode = '/'
      if (mode === "'" || mode === '"' || mode === '`' || mode === '/') last = 'x'
      else if (!/\s/.test(ch)) last = ch
      if (mode === 'block') {
        out += '  '
        i++
        continue
      }
      out += mode === null ? ch : ' '
      continue
    }
    if (ch === '\n') {
      out += '\n'
      if (mode !== 'block' && mode !== '`') mode = null
      inClass = false
      continue
    }
    if (mode === 'block' && ch === '*' && next === '/') {
      out += '  '
      i++
      mode = null
      continue
    }
    if (mode !== 'line' && mode !== 'block' && ch === '\\') {
      out += next === '\n' ? ' \n' : '  '
      i++
      continue
    }
    if (mode === '/' && (ch === '[' || ch === ']')) inClass = ch === '['
    else if (ch === mode && !inClass) mode = null
    out += ' '
  }
  return out
}

export const FAILING_VERDICTS = new Set(['over', 'mismatch', 'growth', 'shrink', 'stale'])

/**
 * Repo-relative `scripts/*.test.mjs` suites that import the size-ratchet library and call one of
 * its two primitives, minus the library's fixture suites. Throws on an empty selection: a report
 * that ran nothing would print a clean table.
 */
export function selectRatchetSuites(repoRoot = REPO_ROOT, deps = {}) {
  const fsImpl = deps.fs || fs
  const dir = path.join(repoRoot, 'scripts')
  const excluded = new Set(FIXTURE_SUITES)
  const selected = fsImpl
    .readdirSync(dir)
    .filter((name) => name.endsWith('.test.mjs'))
    .map((name) => `scripts/${name}`)
    .filter((rel) => !excluded.has(rel))
    .filter((rel) => {
      const text = fsImpl.readFileSync(path.join(repoRoot, rel), 'utf8')
      return LIB_IMPORT.test(text) && PRIMITIVE_CALL.test(text)
    })
    .sort()
  if (selected.length === 0) {
    throw new Error(
      'ratchet-report: no scripts/*.test.mjs suite imports ./size-ratchet-lib.mjs and calls ' +
        'assertExactSize or assertDescendingBudget, so there is nothing to measure. A report ' +
        'over an empty selection would print a clean table; fix the selection rule instead.',
    )
  }
  return selected
}

/**
 * Static primitive call-site count per suite, ignoring comments and strings: the minimum number
 * of distinct call sites each selected suite must record for the report to trust that none of its
 * calls was skipped.
 */
export function countCallSites(files, repoRoot = REPO_ROOT, deps = {}) {
  const fsImpl = deps.fs || fs
  return Object.fromEntries(
    files.map((rel) => {
      const text = fsImpl.readFileSync(path.join(repoRoot, rel), 'utf8')
      return [rel, (blankCommentsAndStrings(text).match(PRIMITIVE_CALLS) || []).length]
    }),
  )
}

/**
 * Run `files` in one `node --test` child with the report variable set to `dir`. The child's
 * output goes to stderr so stdout stays the report. Returns the child's exit status.
 */
export function runSuites(files, dir, deps = {}) {
  const spawn = deps.spawnSync || spawnSync
  const env = { ...process.env, [REPORT_DIR_ENV]: dir }
  // A nested `node --test` inheriting its parent runner's context would report over the parent's
  // protocol instead of running as a top-level runner.
  delete env.NODE_TEST_CONTEXT
  const result = spawn(process.execPath, ['--test', '--test-reporter=tap', ...files], {
    cwd: deps.cwd || REPO_ROOT,
    env,
    stdio: deps.stdio || ['ignore', 2, 2],
  })
  if (result.error) throw result.error
  return result.status === null ? 1 : result.status
}

/**
 * The literal root and its real form. Node reports the entry script and module URLs with symlinks
 * resolved, so a root reached through one is tried in its real form too.
 */
function rootForms(repoRoot) {
  try {
    return [repoRoot, fs.realpathSync(repoRoot)]
  } catch {
    // An unresolvable root leaves only the literal form to try.
    return [repoRoot]
  }
}

/** An absolute path under one of `roots`, repo-relative (`scripts/x.test.mjs`). */
function relativeTo(file, roots) {
  if (typeof file !== 'string' || !path.isAbsolute(file)) return file
  for (const root of roots) {
    const rel = path.relative(root, file)
    if (!rel.startsWith('..') && !path.isAbsolute(rel)) return rel.split(path.sep).join('/')
  }
  return file
}

/** Split `<file>:<line>` into its parts, or null when it is not that shape. */
function parseCallSite(callSite) {
  const match = typeof callSite === 'string' ? /^(.+):(\d+)$/.exec(callSite) : null
  return match ? { file: match[1], line: match[2] } : null
}

/** A row's `callSite` with its file made repo-relative; null when unreadable. */
function relativeCallSite(callSite, roots) {
  const site = parseCallSite(callSite)
  return site ? `${relativeTo(site.file, roots)}:${site.line}` : null
}

/**
 * Parse every `*.jsonl` row under `dir`, drop rows whose `path` is not a regular file under
 * `repoRoot` (fixture rows), make each row's `suite` and `callSite` repo-relative, and drop exact
 * duplicates (a suite run twice over one gate).
 */
export function collectRows(dir, repoRoot = REPO_ROOT) {
  const roots = rootForms(repoRoot)
  const seen = new Set()
  const rows = []
  for (const name of fs.readdirSync(dir).sort()) {
    if (!name.endsWith('.jsonl')) continue
    const lines = fs.readFileSync(path.join(dir, name), 'utf8').split('\n').filter(Boolean)
    for (const line of lines) {
      const row = JSON.parse(line)
      const abs = path.resolve(repoRoot, String(row.path))
      if (!abs.startsWith(repoRoot + path.sep)) continue
      let stat
      try {
        stat = fs.statSync(abs)
      } catch {
        continue
      }
      if (!stat.isFile()) continue
      row.suite = relativeTo(row.suite, roots)
      row.callSite = relativeCallSite(row.callSite, roots)
      const key = JSON.stringify([
        row.suite,
        row.callSite,
        row.constFile,
        row.constName,
        row.path,
        row.kind,
        row.unit,
        row.measured,
        row.pinned,
      ])
      if (seen.has(key)) continue
      seen.add(key)
      rows.push(row)
    }
  }
  return rows
}

/**
 * Verdict rows for size gates plus one row per prose-pin baseline file.
 *
 * @param {object[]} rows Rows from `collectRows`.
 * @param {{counts: Record<string, number>, growth: object[], shrink: object[], stale: string[]}}
 *   prosePins The result of `measureProsePins`.
 * @param {Record<string, number>} [baseline] The prose-pin baseline those were measured against.
 */
export function classify(rows, prosePins, baseline = PROSE_PIN_BASELINE) {
  const gates = rows.map((row) => {
    if (row.kind === 'budget') {
      const within = row.measured <= row.pinned
      return {
        ...row,
        verdict: within ? 'within' : 'over',
        headroom: row.pinned - row.measured,
      }
    }
    return { ...row, verdict: row.measured === row.pinned ? 'match' : 'mismatch' }
  })

  const growth = new Set(prosePins.growth.map((entry) => entry.file))
  const shrink = new Set(prosePins.shrink.map((entry) => entry.file))
  const files = [...new Set([...Object.keys(prosePins.counts), ...prosePins.stale])].sort()
  const prose = files.map((file) => {
    const pinned = Object.hasOwn(baseline, file) ? baseline[file] : 0
    if (prosePins.stale.includes(file)) {
      return { kind: 'prose-pin', constFile: file, pinned, measured: null, verdict: 'stale' }
    }
    const measured = prosePins.counts[file]
    let verdict = 'match'
    if (growth.has(file)) verdict = 'growth'
    else if (shrink.has(file)) verdict = 'shrink'
    return { kind: 'prose-pin', constFile: file, pinned, measured, verdict }
  })

  return { gates, prose }
}

/**
 * Selected suites that recorded from fewer distinct call sites in their own file than their static
 * call-site count (default 1), attributed by the recording `suite`, each described as
 * `<file> (<recorded> of <expected> call site(s) recorded)`. A loop recording many gates is one
 * call site, so it cannot cover a sibling that never ran; a null `callSite` counts for nothing.
 */
export function tripwire(files, rows, callSites = {}) {
  const sites = new Map(files.map((file) => [file, new Set()]))
  for (const row of rows) {
    const site = parseCallSite(row.callSite)
    if (site && site.file === row.suite) sites.get(row.suite)?.add(site.line)
  }
  return files.flatMap((file) => {
    const expected = Math.max(callSites[file] ?? 1, 1)
    const recorded = sites.get(file).size
    return recorded < expected ? [`${file} (${recorded} of ${expected} call site(s) recorded)`] : []
  })
}

/** Assemble the full verdict: ok only when every row passes, no tripwire hit, runner exit 0. */
export function buildReport({ files, rows, prosePins, runnerStatus, baseline, callSites }) {
  const { gates, prose } = classify(rows, prosePins, baseline)
  const silent = tripwire(files, rows, callSites)
  const failing = [...gates, ...prose].filter((row) => FAILING_VERDICTS.has(row.verdict))
  const sum = (key) => prose.reduce((total, row) => total + (row[key] ?? 0), 0)
  const reasons = []
  if (failing.length > 0) reasons.push(`${failing.length} failing row(s)`)
  if (silent.length > 0) reasons.push(`suite(s) recorded too few gates: ${silent.join(', ')}`)
  if (runnerStatus !== 0) reasons.push(`suite run exited ${runnerStatus}`)
  return {
    ok: reasons.length === 0,
    reasons,
    suites: files,
    gates,
    prose,
    prosePinTotal: { pinned: sum('pinned'), measured: sum('measured') },
    silentSuites: silent,
    runnerStatus,
  }
}

/** Plain-text table: one line per gate and per prose-pin file, then the total and the verdict. */
export function formatReport(report) {
  const lines = ['kind       constFile  constName  pinned  measured  unit  verdict']
  for (const row of report.gates) {
    const extra = row.verdict === 'within' ? ` (headroom ${row.headroom} ${row.unit})` : ''
    lines.push(
      `${row.kind.padEnd(10)} ${row.constFile} ${row.constName} ${row.pinned} ${row.measured} ` +
        `${row.unit} ${row.verdict}${extra}`,
    )
  }
  for (const row of report.prose) {
    lines.push(
      `${row.kind.padEnd(10)} ${row.constFile} PROSE_PIN_BASELINE ${row.pinned} ` +
        `${row.measured ?? '-'} pins ${row.verdict}`,
    )
  }
  lines.push(
    `prose-pin total: pinned ${report.prosePinTotal.pinned}, measured ` +
      `${report.prosePinTotal.measured} across ${report.prose.length} file(s)`,
  )
  lines.push(
    report.ok
      ? `ratchet-report: OK — ${report.gates.length} size gate(s) from ${report.suites.length} ` +
          'suite(s), every prose-pin baseline at its measurement.'
      : `ratchet-report: FAIL — ${report.reasons.join('; ')}`,
  )
  return lines.join('\n')
}

/**
 * Select, run, collect and print. Returns the exit code rather than exiting, so the whole path is
 * testable with injected collaborators.
 *
 * @param {string[]} argv CLI arguments; `--json` prints the report object instead of the table.
 * @param {object} [deps] Injectable `select`, `count`, `run`, `collect`, `prose`, `log`,
 *   `repoRoot`.
 * @returns {number} 0 when the report is clean, 1 otherwise.
 */
export function main(argv, deps = {}) {
  const {
    repoRoot = REPO_ROOT,
    select = selectRatchetSuites,
    count = countCallSites,
    run = runSuites,
    collect = collectRows,
    prose = measureProsePins,
    log = console.log,
  } = deps
  const json = argv.includes('--json')
  const files = select(repoRoot)
  const callSites = count(files, repoRoot)
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'ratchet-report-'))
  let report
  try {
    const runnerStatus = run(files, dir, { cwd: repoRoot })
    report = buildReport({
      files,
      rows: collect(dir, repoRoot),
      prosePins: prose(repoRoot),
      runnerStatus,
      callSites,
    })
  } finally {
    fs.rmSync(dir, { recursive: true, force: true })
  }
  log(json ? JSON.stringify(report, null, 2) : formatReport(report))
  return report.ok ? 0 : 1
}

if (isMainModule(import.meta.url)) process.exitCode = main(process.argv.slice(2))
