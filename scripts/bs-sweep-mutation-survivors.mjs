// scripts/bs-sweep-mutation-survivors.mjs
//
// Deterministic, loss-proof compact-survivor extraction for the bs-sweep-mutation
// skill (BOS-145, child 2 of the BOS-143 token-efficiency epic).
//
// The mutation-suite run streams bulk output ("all four views" — report.txt,
// survivors.txt, uncovered.txt, coverage.txt). BOS-145 runs that inside ONE awaited
// subagent so the bulk never enters the orchestrator context; the subagent returns
// only a COMPACT survivor list + Phase-B uncovered/coverage rows via a run-file
// sentinel.
//
// Because a cheap-tier subagent could silently drop a survivor if it hand-transcribed
// the list, the extraction is a DETERMINISTIC parser here — not LLM judgment — so a
// survivor can never be lost by construction. The content test's loss-check fixture
// pins that `extractSurvivors` reproduces EVERY survivor present in a captured
// mutation-suite output (scripts/bs-sweep-mutation-skill.test.mjs).
//
// This layers its own token vocabularies on top of skills-toolbox/bs-run-sentinel.mjs's
// generic run-file mechanics (BOS-144 convention: children reuse the mechanics
// verbatim and add their own token sets). The finalize completion-watch reuses
// bs-run-sentinel.mjs's REPAIR_RESULTS set directly.
//
// Node built-ins only — cron worktrees are dependency-free. Mirrors the shape of
// scripts/sweep-security-gate.mjs (pure module + thin CLI + byte-stable token sets
// pinned by a content test).

import { readFileSync } from 'node:fs'

// The mutation-run sentinel `kind` vocabulary. The mutation-run subagent writes
// exactly one of these; the payload carries the compact survivor list + coverage
// rows. `extracted` = the suite ran and produced the compact views; `empty` = the
// suite ran and found neither a killable survivor nor a coverage candidate. A
// missing/stale sentinel is NEVER one of these — the orchestrator synthesizes
// `dispatch-failure` (from bs-run-sentinel.mjs) and routes to the safe NO_CHANGE
// branch, never a false "no survivors".
export const MUTATION_RESULTS = ['extracted', 'empty']

// The per-survivor sentinel `kind` vocabulary. Each per-survivor subagent writes
// exactly one of these terminal decisions: `killed` (a test now defeats the
// mutant), `equivalent` (no input distinguishes mutant from original — documented,
// not chased), `support-code` (a minimal production change justified by a changed
// test).
export const PER_MUTANT_RESULTS = ['killed', 'equivalent', 'support-code']

// The Phase-B sentinel `kind` vocabulary. The Phase-B subagent owns bounded batch
// planning + test writing and reports exactly one of these decisions.
export const PHASE_B_RESULTS = ['tests-written', 'skipped', 'no-target']

// A well-formed compact survivor line: `[<modname--safename>] <file>:<line> <TYPE>`,
// exactly the shape `make mutate-survivors` emits (jq: LIVED mutants only). The same
// shape `make mutate-uncovered` uses for NOT_COVERED mutants — which is why survivor
// extraction runs on the survivors view specifically, never on a concatenated blob.
const SURVIVOR_LINE = /^\[[^\]]+\]\s+\S+:\d+\s+\S+/

/**
 * Extract the compact mutant list from a captured `make mutate-survivors` or
 * `make mutate-uncovered` view. Deterministic and loss-proof: every well-formed
 * mutant line is preserved (whitespace-trimmed); blank lines and stray non-matching
 * noise are dropped. Duplicates are preserved because the compact Makefile views do
 * not carry mutant IDs, so an exact duplicate may still represent a distinct mutant.
 * @param {string} text  the raw survivors view (stdout of `make mutate-survivors`)
 * @returns {string[]}   one compact line per unique survivor
 */
export function extractSurvivors(text) {
  const out = []
  for (const raw of String(text ?? '').split(/\r?\n/)) {
    const line = raw.trim()
    if (!line || !SURVIVOR_LINE.test(line)) continue
    out.push(line)
  }
  return out
}

/**
 * Extract the compact uncovered-mutant list from a captured `make mutate-uncovered`
 * view. Same line shape as survivors, same loss-proof duplicate preservation.
 * @param {string} text  the raw uncovered view (stdout of `make mutate-uncovered`)
 * @returns {string[]}   one compact line per uncovered mutant
 */
export function extractUncoveredRows(text) {
  return extractSurvivors(text)
}

/**
 * Extract the Phase-B coverage rows from a captured `make mutate-coverage` view.
 * Rows are tab-separated `coverage%\tnot_covered\tpackage`, lowest coverage first.
 * @param {string} text  the raw coverage view (stdout of `make mutate-coverage`)
 * @returns {{coverage: string, notCovered: number, name: string}[]}
 */
export function extractCoverageRows(text) {
  const out = []
  for (const raw of String(text ?? '').split(/\r?\n/)) {
    const line = raw.trim()
    if (!line) continue
    const cols = line.split('\t')
    if (cols.length < 3) continue
    const [coverage, notCovered, name] = cols
    // Skip any header-ish row whose first column is not a number.
    if (!/^\d/.test(coverage)) continue
    out.push({ coverage, notCovered: Number(notCovered), name })
  }
  return out
}

/**
 * Classify a mutation-run sentinel token. Returns `{result}` iff `token` is a
 * published MUTATION_RESULTS member, else `null` (a missing/stale sentinel is the
 * orchestrator's `dispatch-failure`, never a valid token here).
 * @param {string} token
 * @returns {{result: string}|null}
 */
export function matchMutationResult(token) {
  if (typeof token !== 'string') return null
  const t = token.trim()
  return MUTATION_RESULTS.includes(t) ? { result: t } : null
}

/**
 * Classify a per-survivor sentinel token. Returns `{result}` iff `token` is a
 * published PER_MUTANT_RESULTS member, else `null`.
 * @param {string} token
 * @returns {{result: string}|null}
 */
export function matchPerMutantResult(token) {
  if (typeof token !== 'string') return null
  const t = token.trim()
  return PER_MUTANT_RESULTS.includes(t) ? { result: t } : null
}

/**
 * Classify a Phase-B sentinel token. Returns `{result}` iff `token` is a published
 * PHASE_B_RESULTS member, else `null`.
 * @param {string} token
 * @returns {{result: string}|null}
 */
export function matchPhaseBResult(token) {
  if (typeof token !== 'string') return null
  const t = token.trim()
  return PHASE_B_RESULTS.includes(t) ? { result: t } : null
}

// Thin CLI — the surface the skill body shells out to from inside the subagent:
//   node scripts/bs-sweep-mutation-survivors.mjs extract-survivors <survivors.txt>
//   node scripts/bs-sweep-mutation-survivors.mjs extract-uncovered <uncovered.txt>
//   node scripts/bs-sweep-mutation-survivors.mjs extract-coverage  <coverage.txt>
//   node scripts/bs-sweep-mutation-survivors.mjs match-mutation <token>
//   node scripts/bs-sweep-mutation-survivors.mjs match-mutant   <token>
//   node scripts/bs-sweep-mutation-survivors.mjs match-phase-b  <token>
import { isMainModule } from '../skills-toolbox/main-module.mjs'

if (isMainModule(import.meta.url)) {
  const [cmd, ...rest] = process.argv.slice(2)
  const fail = (msg) => {
    process.stderr.write(`${msg}\n`)
    process.exit(2)
  }
  if (cmd === 'extract-survivors') {
    if (!rest[0]) fail('extract-survivors requires <survivors.txt>')
    process.stdout.write(extractSurvivors(readFileSync(rest[0], 'utf8')).join('\n') + '\n')
  } else if (cmd === 'extract-uncovered') {
    if (!rest[0]) fail('extract-uncovered requires <uncovered.txt>')
    process.stdout.write(extractUncoveredRows(readFileSync(rest[0], 'utf8')).join('\n') + '\n')
  } else if (cmd === 'extract-coverage') {
    if (!rest[0]) fail('extract-coverage requires <coverage.txt>')
    process.stdout.write(JSON.stringify(extractCoverageRows(readFileSync(rest[0], 'utf8'))) + '\n')
  } else if (cmd === 'match-mutation') {
    process.stdout.write(`${JSON.stringify(matchMutationResult(rest[0] ?? ''))}\n`)
  } else if (cmd === 'match-mutant') {
    process.stdout.write(`${JSON.stringify(matchPerMutantResult(rest[0] ?? ''))}\n`)
  } else if (cmd === 'match-phase-b') {
    process.stdout.write(`${JSON.stringify(matchPhaseBResult(rest[0] ?? ''))}\n`)
  } else {
    fail(
      'usage: bs-sweep-mutation-survivors.mjs <extract-survivors <survivors.txt> | extract-uncovered <uncovered.txt> | extract-coverage <coverage.txt> | match-mutation <token> | match-mutant <token> | match-phase-b <token>>',
    )
  }
}
