// gate-outcome-report.mjs
//
// The reader over the lines `gate-outcome.mjs` writes. That recorder answers
// "what did this gate decide?"; this module answers "how often has it ever
// decided anything?", which is the question a retirement argument actually
// turns on. Without it the recorded lines are a write-only file and gate
// retirement stays a contest between two plausible stories.
//
// Three properties are load-bearing here, and each is a structural choice
// rather than a tested-for hope:
//
//   1. **"Never fired" and "not enough data" are different verdicts.** They are
//      separate members of GATE_REPORT_VERDICTS, not one verdict with two
//      wordings, and only the first is ever a retirement candidate. A gate that
//      has not run enough times for silence to mean anything must not be
//      indistinguishable, at a glance, from one that has run hundreds of times
//      without firing — collapsing them is the single easiest way for this
//      report to argue for deleting a gate that works.
//
//   2. **A torn line degrades, never aborts.** The recorder's own header
//      explains why the format is TSV rather than JSON-lines: a run killed
//      mid-append leaves a short final line. A reader that throws on it turns
//      one truncated record into zero readable records. Every unparseable line
//      is counted and skipped, and the report still prints.
//
//   3. **An empty store reports NO DATA, never "no gate fires".** An absent
//      directory, an absent file, and a file of zero valid lines all set
//      `hasData: false` and produce no retirement candidates. The failure mode
//      this guards against is a machine that has simply never run the
//      instrumented guards being read as evidence that the guards are inert.
//
// The line grammar is NOT redeclared here. `GATE_OUTCOME_LINE_PATTERN` and the
// destination constants are imported from the recorder, so the writer and the
// reader cannot drift apart on what a record is.
//
// Node built-ins only — cron worktrees are dependency-free.

import { readFileSync, readdirSync, statSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'

import {
  GATE_OUTCOME_DIR,
  GATE_OUTCOME_FILE_ENV,
  GATE_OUTCOME_LINE_PATTERN,
} from './gate-outcome.mjs'

/**
 * Recorded invocations a gate needs before its silence is allowed to mean
 * anything.
 *
 * Twenty, from the rule of three: for a gate that has not fired once in n
 * invocations, the 95% upper bound on its true firing rate is about 3/n. At
 * n = 20 that bound is ~15%, which is the first point at which "it stayed
 * silent" constrains the truth at all rather than merely restating that the
 * sample is small. Below it, silence and "has not run yet" are the same
 * observation and the report must say so.
 *
 * This is deliberately a pinned constant rather than a tuned parameter: the
 * number is what stops a two-day sample from reading exactly like a two-week
 * one, so moving it moves the report's entire claim and belongs in a commit
 * that argues for the new bound.
 */
export const INSUFFICIENT_DATA_THRESHOLD = 20

/**
 * Fire rate below which a gate is labelled rare. One firing in twenty runs or
 * fewer is rare enough that a reader will not have watched it fire personally,
 * which is exactly the condition under which recollection reports "that gate
 * never does anything". The label exists so the report says "rare" out loud
 * instead of leaving a reader to infer "worthless" from a small number.
 */
export const RARE_FIRE_RATE = 0.05

/**
 * The closed verdict vocabulary. `insufficient-data` means the sample is too
 * small to say anything; `never-fired` means the gate has been invoked enough
 * times for its silence to be evidence; `fires` means it has refused at least
 * once. Only `never-fired` is ever a retirement candidate.
 */
export const GATE_REPORT_VERDICTS = Object.freeze(['insufficient-data', 'never-fired', 'fires'])

/**
 * How often the gate fires, as a label. `unknown` is the honest answer under
 * the threshold, and is a distinct value from `never` for the same reason the
 * verdicts are distinct.
 */
export const GATE_RARITY_LABELS = Object.freeze(['unknown', 'never', 'rare', 'regular'])

/** Extension of a recorded outcome file, as written by the recorder. */
export const GATE_OUTCOME_FILE_EXTENSION = '.tsv'

/**
 * Parse one recorded line into its fields, or `null` when the line is not a
 * complete record. Validation is `GATE_OUTCOME_LINE_PATTERN` itself — the
 * recorder's published grammar — so a field this reader accepts is exactly a
 * field the recorder could have written, including the closed outcome
 * vocabulary.
 * @param {unknown} line a single line, without its trailing newline
 * @returns {{timestamp: string, gateId: string, outcome: string, reason: string}|null}
 */
export function parseGateOutcomeLine(line) {
  if (typeof line !== 'string') return null
  if (!GATE_OUTCOME_LINE_PATTERN.test(line)) return null
  const [timestamp, gateId, outcome, reason] = line.split('\t')
  return { timestamp, gateId, outcome, reason }
}

/**
 * Parse the whole text of one recorded file. Blank lines are the newline
 * terminator and are not malformed; every other unparseable line increments
 * `skipped` and is dropped. Never throws.
 * @param {unknown} text
 * @returns {{records: Array<{timestamp: string, gateId: string, outcome: string, reason: string}>, skipped: number}}
 */
export function parseGateOutcomeText(text) {
  const records = []
  let skipped = 0
  if (typeof text !== 'string') return { records, skipped }
  for (const line of text.split('\n')) {
    if (line === '') continue
    const record = parseGateOutcomeLine(line)
    if (record) records.push(record)
    else skipped += 1
  }
  return { records, skipped }
}

/**
 * Where the recorded outcomes live for reading. An explicit
 * `BOSS_GATE_OUTCOME_FILE` names one file and wins; otherwise the store is the
 * whole per-run directory under the OS temp root, because the recorder writes
 * one file per run and a report over a single run is not a report.
 * @param {NodeJS.ProcessEnv} [env]
 * @returns {{kind: 'file'|'directory', path: string}}
 */
export function resolveGateOutcomeStore(env = process.env) {
  const explicit = env?.[GATE_OUTCOME_FILE_ENV]
  if (typeof explicit === 'string' && explicit.trim() !== '') {
    return { kind: 'file', path: explicit.trim() }
  }
  return { kind: 'directory', path: join(tmpdir(), GATE_OUTCOME_DIR) }
}

/**
 * List the recorded files a store contributes, newest name last. An absent or
 * unreadable store contributes none — it is the empty case, not an error.
 * @param {{kind: 'file'|'directory', path: string}} store
 * @param {{readdir?: (path: string) => string[], stat?: (path: string) => {isFile: () => boolean}}} [deps]
 * @returns {string[]} absolute or store-relative paths, sorted
 */
export function listGateOutcomeFiles(store, deps = {}) {
  const readdir = deps.readdir ?? ((path) => readdirSync(path))
  const stat = deps.stat ?? ((path) => statSync(path))
  if (!store || typeof store.path !== 'string') return []
  if (store.kind === 'file') {
    try {
      return stat(store.path).isFile() ? [store.path] : []
    } catch {
      return []
    }
  }
  try {
    return readdir(store.path)
      .filter((name) => name.endsWith(GATE_OUTCOME_FILE_EXTENSION))
      .sort()
      .map((name) => join(store.path, name))
  } catch {
    return []
  }
}

/**
 * Read every recorded file in a store into parsed records. Each unreadable
 * file is counted rather than thrown: one file whose permissions changed must
 * not cost the report every other run's data.
 * @param {{env?: NodeJS.ProcessEnv, store?: {kind: 'file'|'directory', path: string}, readFile?: (path: string) => string, readdir?: (path: string) => string[], stat?: (path: string) => {isFile: () => boolean}}} [opts]
 * @returns {{store: {kind: string, path: string}, files: string[], records: Array<object>, skippedLines: number, unreadableFiles: number}}
 */
export function readGateOutcomeStore(opts = {}) {
  const store = opts.store ?? resolveGateOutcomeStore(opts.env ?? process.env)
  const readFile = opts.readFile ?? ((path) => readFileSync(path, 'utf8'))
  const files = listGateOutcomeFiles(store, opts)
  const records = []
  let skippedLines = 0
  let unreadableFiles = 0
  for (const file of files) {
    let text
    try {
      text = readFile(file)
    } catch {
      unreadableFiles += 1
      continue
    }
    const parsed = parseGateOutcomeText(text)
    records.push(...parsed.records)
    skippedLines += parsed.skipped
  }
  return { store, files, records, skippedLines, unreadableFiles }
}

/**
 * Verdict for one gate's counts. Kept separate from the aggregation so the
 * "never fired" / "not enough data" distinction has one definition rather than
 * being re-derived at every call site.
 * @param {number} invocations
 * @param {number} fires
 * @param {number} threshold
 * @returns {string} a GATE_REPORT_VERDICTS member
 */
export function gateVerdict(invocations, fires, threshold = INSUFFICIENT_DATA_THRESHOLD) {
  if (fires > 0) return 'fires'
  // The zero-invocation case is structural, not threshold-derived: no record at
  // all is the least evidence there is, so it can never be `never-fired` however
  // the threshold is set. Deriving it from `invocations >= threshold` instead
  // collapses at `--threshold 0`, where `0 >= 0` would make an un-run gate a
  // retirement candidate — the one outcome this verdict split exists to prevent.
  if (invocations === 0) return 'insufficient-data'
  return invocations >= threshold ? 'never-fired' : 'insufficient-data'
}

/**
 * Rarity label for one gate's counts. `unknown` under the threshold, because a
 * fire rate computed from four runs is a number, not a frequency.
 * @param {number} invocations
 * @param {number} fires
 * @param {number} threshold
 * @returns {string} a GATE_RARITY_LABELS member
 */
export function gateRarity(invocations, fires, threshold = INSUFFICIENT_DATA_THRESHOLD) {
  // Same structural guard as `gateVerdict`: a gate that never RAN must not be
  // labelled one that never FIRES, which is what `never` would claim.
  if (invocations === 0) return 'unknown'
  if (invocations < threshold) return 'unknown'
  if (fires === 0) return 'never'
  return fires / invocations < RARE_FIRE_RATE ? 'rare' : 'regular'
}

/**
 * Aggregate parsed records into the per-gate report. Pure: no filesystem, no
 * clock, no environment — the whole report is a function of the records and the
 * threshold, which is what makes every verdict in it unit-testable against
 * fixture lines rather than against a real run.
 *
 * `gateIds` seeds the roster with gates that have zero recorded invocations.
 * Such a gate is reported as `insufficient-data` and is never a retirement
 * candidate: no record at all is the least evidence there is, not the most.
 *
 * @param {Array<{gateId: string, outcome: string}>} records
 * @param {{threshold?: number, gateIds?: string[]}} [opts]
 * @returns {{hasData: boolean, threshold: number, gates: Array<object>, retirementCandidates: string[], totals: {gates: number, records: number, invocations: number, fires: number}}}
 */
export function aggregateGateOutcomes(records, opts = {}) {
  const threshold = Number.isInteger(opts.threshold) ? opts.threshold : INSUFFICIENT_DATA_THRESHOLD
  const counts = new Map()
  const seed = (gateId) => {
    if (!counts.has(gateId)) counts.set(gateId, { gateId, invocations: 0, fires: 0, passes: 0 })
    return counts.get(gateId)
  }
  for (const gateId of Array.isArray(opts.gateIds) ? opts.gateIds : []) {
    if (typeof gateId === 'string' && gateId !== '') seed(gateId)
  }

  let counted = 0
  for (const record of Array.isArray(records) ? records : []) {
    const gateId = record?.gateId
    const outcome = record?.outcome
    if (typeof gateId !== 'string' || gateId === '') continue
    if (outcome !== 'pass' && outcome !== 'fire') continue
    const entry = seed(gateId)
    entry.invocations += 1
    if (outcome === 'fire') entry.fires += 1
    else entry.passes += 1
    counted += 1
  }

  const gates = [...counts.values()]
    .map((entry) => {
      const verdict = gateVerdict(entry.invocations, entry.fires, threshold)
      return {
        ...entry,
        // 0/0 is not 0%. A gate with no invocations has no fire rate, and
        // rendering one as "0.0%" is the report telling a story its data
        // cannot support.
        fireRate: entry.invocations === 0 ? null : entry.fires / entry.invocations,
        verdict,
        rarity: gateRarity(entry.invocations, entry.fires, threshold),
        retirementCandidate: verdict === 'never-fired',
      }
    })
    .sort((a, b) => b.invocations - a.invocations || (a.gateId < b.gateId ? -1 : 1))

  return {
    hasData: counted > 0,
    threshold,
    gates,
    retirementCandidates: gates.filter((g) => g.retirementCandidate).map((g) => g.gateId),
    totals: {
      gates: gates.length,
      records: counted,
      invocations: gates.reduce((sum, g) => sum + g.invocations, 0),
      fires: gates.reduce((sum, g) => sum + g.fires, 0),
    },
  }
}
