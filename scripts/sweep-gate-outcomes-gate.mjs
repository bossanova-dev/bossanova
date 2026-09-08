// scripts/sweep-gate-outcomes-gate.mjs
// The sweep entry point over the gate outcomes `skills-toolbox/gate-outcome.mjs`
// records. Reading and aggregation live in `skills-toolbox/gate-outcome-report.mjs`
// as a pure function over parsed lines; this file owns only the output format
// and the CLI adapter, so the report's verdicts can be unit-tested without ever
// rendering a table and the table can be tested without a real run.
//
// It reports; it retires nothing. A verdict here is an input to a retirement
// argument, never its conclusion — see docs/skills/gate-firing-rates.md for
// what the numbers do and do not settle.
//
// Node built-ins only — the cron worktree is dependency-free.

import { readFileSync } from 'node:fs'

import {
  GATE_OUTCOME_FILE_EXTENSION,
  INSUFFICIENT_DATA_THRESHOLD,
  aggregateGateOutcomes,
  readGateOutcomeStore,
  resolveGateOutcomeStore,
} from '../skills-toolbox/gate-outcome-report.mjs'
import { isMainModule } from '../skills-toolbox/main-module.mjs'

/**
 * The closing note every rendered report carries. It is in the output rather
 * than only in the docs because the number a reader acts on is the one on
 * screen, and a low fire rate reads as "useless gate" to anyone who has not
 * gone looking for the caveat. A gate that fires twice a year and catches a
 * data-loss defect both times is the class this project keeps.
 */
export const RARITY_CAVEAT =
  'Rarity is not worthlessness: this report says how often a gate fired, never whether it was worth having.'

/** Header shown when the store holds no readable record at all. */
export const NO_DATA_HEADING = 'gate outcomes: NO DATA'

/** Render a fire rate for display. `null` is an absent rate, not zero. */
export function formatFireRate(rate) {
  return rate === null || rate === undefined ? '—' : `${(rate * 100).toFixed(1)}%`
}

const COLUMNS = [
  { key: 'gateId', label: 'GATE', align: 'left' },
  { key: 'invocations', label: 'INVOCATIONS', align: 'right' },
  { key: 'fires', label: 'FIRES', align: 'right' },
  { key: 'fireRate', label: 'FIRE RATE', align: 'right' },
  { key: 'verdict', label: 'VERDICT', align: 'left' },
  { key: 'rarity', label: 'FIRES HOW OFTEN', align: 'left' },
]

function tableRows(gates) {
  return gates.map((gate) => ({
    gateId: gate.gateId,
    invocations: String(gate.invocations),
    fires: String(gate.fires),
    fireRate: formatFireRate(gate.fireRate),
    verdict: gate.verdict,
    rarity: gate.rarity,
  }))
}

function renderTable(gates) {
  const rows = tableRows(gates)
  const widths = COLUMNS.map((column) =>
    Math.max(column.label.length, ...rows.map((row) => row[column.key].length)),
  )
  const pad = (text, size, align) => (align === 'right' ? text.padStart(size) : text.padEnd(size))
  const line = (cells) =>
    COLUMNS.map((column, i) => pad(cells[i], widths[i], column.align))
      .join('  ')
      .trimEnd()
  return [
    line(COLUMNS.map((column) => column.label)),
    ...rows.map((row) => line(COLUMNS.map((column) => row[column.key]))),
  ]
}

/**
 * Render the aggregated report as plain text.
 *
 * The empty store gets its own rendering rather than an empty table, because an
 * empty table under a "fires" column is read as "nothing fires" — the exact
 * inversion this report exists to prevent. No data is stated as an absence of
 * records and produces no retirement candidates.
 *
 * @param {object} report the value from `aggregateGateOutcomes`
 * @param {{store?: {kind: string, path: string}, files?: string[], skippedLines?: number, unreadableFiles?: number}} [read]
 * @returns {string}
 */
export function renderGateOutcomeReport(report, read = {}) {
  const where = read.store?.path ? ` in ${read.store.path}` : ''
  const skipped = Number(read.skippedLines ?? 0)
  const unreadable = Number(read.unreadableFiles ?? 0)
  const degraded = []
  if (skipped > 0) degraded.push(`${skipped} malformed line(s) skipped`)
  if (unreadable > 0) degraded.push(`${unreadable} unreadable file(s) skipped`)

  if (!report.hasData) {
    return [
      NO_DATA_HEADING,
      '',
      `No recorded gate outcomes were found${where}.`,
      'That is an absence of records, not a finding about the gates: nothing here says',
      'that no gate fires. No gate is a retirement candidate on this report.',
      ...(degraded.length > 0 ? ['', `Degraded: ${degraded.join('; ')}.`] : []),
      ...(report.gates.length > 0
        ? [
            '',
            'Gates with no recorded invocation at all:',
            ...report.gates.map((gate) => `  ${gate.gateId} — insufficient-data`),
          ]
        : []),
      '',
      RARITY_CAVEAT,
    ].join('\n')
  }

  const runs = Array.isArray(read.files) ? read.files.length : 0
  const scope =
    `gate outcomes: ${report.totals.gates} gate(s), ${report.totals.invocations} invocation(s), ` +
    `${report.totals.fires} fire(s)` +
    (runs > 0 ? ` across ${runs} recorded run(s)` : '')

  return [
    scope,
    `insufficient-data threshold: ${report.threshold} invocation(s)`,
    ...(degraded.length > 0 ? [`degraded: ${degraded.join('; ')}`] : []),
    '',
    ...renderTable(report.gates),
    '',
    ...(report.retirementCandidates.length > 0
      ? [
          `Retirement candidates (never fired in >= ${report.threshold} recorded invocations):`,
          ...report.retirementCandidates.map((gateId) => `  ${gateId}`),
        ]
      : [
          `Retirement candidates: none (no gate has been silent across ${report.threshold}+ runs).`,
        ]),
    '',
    RARITY_CAVEAT,
  ].join('\n')
}

/** Parse `--flag value` pairs; unknown positional arguments are ignored. */
export function parseArgs(args) {
  const options = {}
  for (let i = 0; i < args.length; i += 1) {
    const arg = args[i]
    if (arg === '--threshold') options.threshold = Number(args[(i += 1)])
    else if (arg === '--store') options.store = args[(i += 1)]
    else if (arg === '--gates') options.gates = args[(i += 1)]
  }
  return options
}

/**
 * Build the report for a set of CLI options. Separated from `runCli` so the
 * store plumbing is testable without going through argv parsing.
 * @returns {{report: object, read: object}}
 */
export function buildReport(options = {}, deps = {}) {
  const env = deps.env ?? process.env
  const readFile = deps.readFile ?? ((path) => readFileSync(path, 'utf8'))
  // A `--store` ending in the recorded extension names one run's file; anything
  // else is the directory of per-run files the recorder writes into.
  const store = options.store
    ? {
        kind: options.store.endsWith(GATE_OUTCOME_FILE_EXTENSION) ? 'file' : 'directory',
        path: options.store,
      }
    : (deps.store ?? resolveGateOutcomeStore(env))
  const read = readGateOutcomeStore({ ...deps, store, readFile })
  let gateIds = []
  if (options.gates) {
    try {
      const parsed = JSON.parse(readFile(options.gates))
      if (Array.isArray(parsed)) gateIds = parsed
    } catch {
      // A missing or malformed roster costs the report its zero-invocation
      // rows, never the rows it can actually count.
      gateIds = []
    }
  }
  const threshold = Number.isInteger(options.threshold)
    ? options.threshold
    : INSUFFICIENT_DATA_THRESHOLD
  return { report: aggregateGateOutcomes(read.records, { threshold, gateIds }), read }
}

/** Small CLI adapter, injection-friendly for node:test. */
export function runCli(argv, deps = {}) {
  const [command = 'report', ...args] = Array.isArray(argv) ? argv : []
  const options = parseArgs(args)
  switch (command) {
    case 'report': {
      const { report, read } = buildReport(options, deps)
      return renderGateOutcomeReport(report, read)
    }
    case 'json': {
      const { report, read } = buildReport(options, deps)
      return JSON.stringify({
        ...report,
        sources: read.files.length,
        skippedLines: read.skippedLines,
        unreadableFiles: read.unreadableFiles,
      })
    }
    default:
      throw new Error(`unknown subcommand: ${command}`)
  }
}

if (isMainModule(import.meta.url)) {
  try {
    process.stdout.write(`${runCli(process.argv.slice(2))}\n`)
  } catch (error) {
    process.stderr.write(`sweep-gate-outcomes-gate: ${error.message}\n`)
    process.exitCode = 1
  }
}
