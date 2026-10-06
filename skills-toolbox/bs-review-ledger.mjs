#!/usr/bin/env node
// Durable per-run dispatch ledger for boss-review.
//
// The review skill writes one ledger-<run-id>.json before dispatch starts, then
// updates rows as it resolves tiers and skips. Reconcile can derive completed
// rows from findings files alone so a surviving reviewer artifact still counts
// even if the orchestrator context vanished before it recorded the row.

import {
  existsSync,
  mkdirSync,
  readFileSync,
  readdirSync,
  renameSync,
  statSync,
  unlinkSync,
  writeFileSync,
} from 'node:fs'
import { basename, dirname, join } from 'node:path'

import { isMainModule } from './main-module.mjs'
import { validateResult } from './skill-extensions.mjs'

export const LEDGER_SCHEMA_VERSION = 1
export const OUTCOMES = Object.freeze({
  completed: 'completed',
  skipped: 'skipped',
  timedOut: 'timed-out',
  notReached: 'not-reached',
})
export const MODES = Object.freeze({
  dispatched: 'dispatched',
  inlined: 'inlined',
  unknown: 'unknown',
})
const FALLBACK_ROUNDS = new Set(['builtin', 'inline'])

export const ROW_KEYS = Object.freeze([
  'name',
  'phase',
  'tier',
  'mode',
  'outcome',
  'cause',
  'completedAtMs',
  'durationMs',
])

function nowMs() {
  return Date.now()
}

function asArray(value) {
  return Array.isArray(value) ? value : []
}

function assertObject(value, label) {
  if (!value || typeof value !== 'object' || Array.isArray(value)) {
    throw new Error(`${label} must be an object`)
  }
  return value
}

function nonEmptyString(value) {
  return typeof value === 'string' && value.length > 0
}

function lensID(lens, index) {
  return lens?.lens ?? lens?.id ?? lens?.skill ?? `lens-${index}`
}

function lensRowName(lens, index, lenses) {
  const id = lensID(lens, index)
  const duplicateCount = asArray(lenses).filter((candidate, candidateIndex) => {
    return lensID(candidate, candidateIndex) === id
  }).length
  return duplicateCount > 1 ? `lens:${index}:${id}` : `lens:${id}`
}

function phaseForFinding(info) {
  if (!info) return null
  if (info.type === 'lens') return 'Phase 1'
  if (info.type === 'default') return 'Phase D'
  if (info.type === 'round') return 'Phase R'
  return null
}

function tierMode(tier, marker = '') {
  const t = String(tier || '').toLowerCase()
  if (t === 'tier1' || t === 'default') return MODES.dispatched
  if (t === 'tier3' || String(marker).trim() === 'inlined') return MODES.inlined
  if (String(marker).trim() === 'dispatched') return MODES.dispatched
  return MODES.unknown
}

function normalizeTierAndMode(tier, mode, marker = '') {
  if (Object.values(MODES).includes(tier)) {
    return { tier: null, mode: mode || tier }
  }
  return { tier, mode: mode || tierMode(tier, marker) }
}

function fallbackTier(info, marker = '') {
  if (info?.type === 'lens' && marker === 'dispatched') return 'tier2'
  if (info?.type === 'lens' && marker === 'inlined') return 'tier3'
  if (info?.type === 'default') return 'default'
  if (info?.type === 'round' && info.id === 'builtin') return 'tier2'
  if (info?.type === 'round' && info.id === 'inline') return 'tier3'
  if (info?.type === 'lens' || info?.type === 'round') return 'tier1'
  return null
}

export function rowFor({
  name,
  phase,
  tier = null,
  mode = MODES.unknown,
  outcome = OUTCOMES.notReached,
  cause = null,
  completedAtMs = null,
  durationMs = null,
} = {}) {
  if (!nonEmptyString(name)) throw new Error('row name must be a non-empty string')
  if (!nonEmptyString(phase)) throw new Error(`row ${name} phase must be a non-empty string`)
  const row = {
    name,
    phase,
    tier,
    mode,
    outcome,
    cause,
    completedAtMs,
    durationMs,
  }
  return normalizeRow(row)
}

export function normalizeRow(row) {
  assertObject(row, 'ledger row')
  const normalized = {}
  for (const key of ROW_KEYS) normalized[key] = row[key] ?? null
  if (!nonEmptyString(normalized.name)) throw new Error('ledger row needs name')
  if (!nonEmptyString(normalized.phase))
    throw new Error(`ledger row ${normalized.name} needs phase`)
  if (!Object.values(OUTCOMES).includes(normalized.outcome)) {
    throw new Error(`ledger row ${normalized.name} has invalid outcome ${normalized.outcome}`)
  }
  if (!Object.values(MODES).includes(normalized.mode)) {
    throw new Error(`ledger row ${normalized.name} has invalid mode ${normalized.mode}`)
  }
  if (
    normalized.completedAtMs !== null &&
    (!Number.isInteger(normalized.completedAtMs) || normalized.completedAtMs < 0)
  ) {
    throw new Error(
      `ledger row ${normalized.name} completedAtMs must be null or a non-negative integer`,
    )
  }
  if (
    normalized.durationMs !== null &&
    (!Number.isInteger(normalized.durationMs) || normalized.durationMs < 0)
  ) {
    throw new Error(
      `ledger row ${normalized.name} durationMs must be null or a non-negative integer`,
    )
  }
  if (normalized.outcome !== OUTCOMES.completed) {
    normalized.completedAtMs = null
    normalized.durationMs = null
  }
  return normalized
}

function dedupeRows(rows) {
  const map = new Map()
  for (const row of rows) {
    const normalized = normalizeRow(row)
    if (!map.has(normalized.name)) map.set(normalized.name, normalized)
  }
  return [...map.values()].sort(
    (a, b) => a.phase.localeCompare(b.phase) || a.name.localeCompare(b.name),
  )
}

export function populationsFrom({ lenses = [], rounds = [], defaultRounds = [] } = {}) {
  const rows = []
  for (const [index, lens] of asArray(lenses).entries()) {
    rows.push(rowFor({ name: lensRowName(lens, index, lenses), phase: 'Phase 1' }))
  }
  for (const round of asArray(rounds)) {
    const name = round?.name ?? round?.extension ?? round?.capability
    if (nonEmptyString(name)) rows.push(rowFor({ name: `round:${name}`, phase: 'Phase R' }))
  }
  for (const round of asArray(defaultRounds)) {
    const capability = round?.capability ?? round?.name
    if (nonEmptyString(capability)) {
      rows.push(rowFor({ name: `default:${capability}`, phase: 'Phase D' }))
    }
  }
  return dedupeRows(rows)
}

export function seedLedger({ runId, populations = {}, now = nowMs() } = {}) {
  if (!nonEmptyString(runId)) throw new Error('runId must be a non-empty string')
  return {
    schemaVersion: LEDGER_SCHEMA_VERSION,
    runId,
    seededAtMs: now,
    rows: populationsFrom(populations),
  }
}

export function readLedger(path) {
  const parsed = JSON.parse(readFileSync(path, 'utf8'))
  return normalizeLedger(parsed)
}

export function writeLedger(path, ledger) {
  mkdirSync(dirname(path), { recursive: true })
  const tmpPath = join(dirname(path), `.${basename(path)}.${process.pid}.${Date.now()}.tmp`)
  try {
    writeFileSync(tmpPath, `${JSON.stringify(normalizeLedger(ledger), null, 2)}\n`, { flag: 'wx' })
    renameSync(tmpPath, path)
  } catch (e) {
    try {
      unlinkSync(tmpPath)
    } catch {}
    throw e
  }
}

export function normalizeLedger(ledger) {
  assertObject(ledger, 'ledger')
  if (ledger.schemaVersion !== LEDGER_SCHEMA_VERSION) {
    throw new Error(`ledger schemaVersion must be ${LEDGER_SCHEMA_VERSION}`)
  }
  if (!nonEmptyString(ledger.runId)) throw new Error('ledger runId must be a non-empty string')
  if (!Number.isInteger(ledger.seededAtMs) || ledger.seededAtMs < 0) {
    throw new Error('ledger seededAtMs must be a non-negative integer')
  }
  return {
    schemaVersion: LEDGER_SCHEMA_VERSION,
    runId: ledger.runId,
    seededAtMs: ledger.seededAtMs,
    rows: dedupeRows(ledger.rows),
  }
}

export function record(ledger, name, update = {}) {
  const next = normalizeLedger(ledger)
  const index = next.rows.findIndex((row) => row.name === name)
  const base =
    index >= 0
      ? next.rows[index]
      : rowFor({ name, phase: update.phase || 'unknown', tier: update.tier || null })
  const outcome =
    update.outcome ||
    (update.reason || update.cause ? OUTCOMES.skipped : base.outcome || OUTCOMES.notReached)
  const tierAndMode = normalizeTierAndMode(update.tier ?? base.tier, update.mode, update.modeMarker)
  const row = normalizeRow({
    ...base,
    phase: update.phase || base.phase,
    tier: tierAndMode.tier,
    mode: tierAndMode.mode,
    outcome,
    cause: update.reason ?? update.cause ?? base.cause,
  })
  if (index >= 0) next.rows[index] = row
  else next.rows.push(row)
  next.rows = dedupeRows(next.rows)
  return next
}

// Extension-authored causes land in the single-line coverage token, so collapse every whitespace
// run (including newlines from a captured stderr tail) to one space.
function oneLine(value) {
  return typeof value === 'string' ? value.replace(/\s+/g, ' ').trim() : ''
}

// Classify one findings file: `valid` (completes its row, `fallback` set when the extension ran its
// inline rubric instead of the reviewer it wraps), `failed` (an `ok: false` envelope — skips its row
// with the extension's own error text), or `ignored`.
function classifyFindingsFile(path, info, marker = '') {
  let parsed
  try {
    parsed = JSON.parse(readFileSync(path, 'utf8'))
  } catch {
    return { kind: 'ignored' }
  }
  if (Array.isArray(parsed)) {
    const valid = info?.type !== 'lens' || ['dispatched', 'inlined'].includes(marker)
    return valid ? { kind: 'valid', fallback: null } : { kind: 'ignored' }
  }
  if (!parsed || typeof parsed !== 'object') return { kind: 'ignored' }
  if (parsed.ok === false) {
    const error = oneLine(parsed.error)
    return { kind: 'failed', cause: error || 'no error detail provided' }
  }
  const expectedRole = info?.type === 'lens' ? 'lens' : 'round'
  if (!validateResult(parsed, expectedRole).ok) return { kind: 'ignored' }
  const fallback = oneLine(parsed.fallback)
  return { kind: 'valid', fallback: fallback || null }
}

function reviewerNameForFindings(filename) {
  const base = basename(filename)
  let m = /^findings-lens-(\d+)-(.+)\.json$/.exec(base)
  if (m) return { type: 'lens', index: Number.parseInt(m[1], 10), id: m[2] }
  m = /^findings-round-default-(.+)\.json$/.exec(base)
  if (m) return { type: 'default', id: m[1] }
  m = /^findings-round-(.+)\.json$/.exec(base)
  if (m) return { type: 'round', id: m[1] }
  return null
}

function lensNames(rows) {
  return rows.filter((row) => row.name.startsWith('lens:')).map((row) => row.name)
}

function lensRowNameForFinding(info, rows, populations) {
  const lens = asArray(populations.lenses)[info.index]
  if (lens) return lensRowName(lens, info.index, populations.lenses)
  return lensNames(rows)[info.index] || `lens:${info.id}`
}

function rowNameForFinding(info, rows, populations) {
  if (!info) return null
  if (info.type === 'default') return `default:${info.id}`
  if (info.type === 'round') return `round:${info.id}`
  if (info.type === 'lens') return lensRowNameForFinding(info, rows, populations)
  return null
}

function tierMarkerFor(path) {
  const markerPath = `${path}.tier`
  if (!existsSync(markerPath)) return ''
  try {
    return readFileSync(markerPath, 'utf8').trim()
  } catch {
    return ''
  }
}

export function reconcile(
  ledger,
  { findingsDir = '', populations = {}, invalid = [], now = nowMs() } = {},
) {
  let next = normalizeLedger(ledger)
  const invalidSources = new Set(
    asArray(invalid)
      .map((entry) => entry?.source?.filename || entry?.filename)
      .filter(nonEmptyString),
  )
  for (const row of populationsFrom(populations)) {
    if (!next.rows.some((existing) => existing.name === row.name)) next.rows.push(row)
  }
  next.rows = dedupeRows(next.rows)
  if (findingsDir && existsSync(findingsDir)) {
    for (const filename of readdirSync(findingsDir).filter((name) =>
      /^findings-.*\.json$/.test(name),
    )) {
      if (invalidSources.has(filename)) continue
      const path = join(findingsDir, filename)
      const info = reviewerNameForFindings(filename)
      const marker = tierMarkerFor(path)
      const result = classifyFindingsFile(path, info, marker)
      if (result.kind === 'ignored') continue
      const rowName = rowNameForFinding(info, next.rows, populations)
      if (result.kind === 'failed') {
        const index = next.rows.findIndex((row) => row.name === rowName)
        const current = index >= 0 ? next.rows[index] : null
        // A failure never downgrades a completed row, and never rewrites a recorded timeout.
        if (
          current &&
          (current.outcome === OUTCOMES.notReached || current.outcome === OUTCOMES.skipped)
        ) {
          next.rows[index] = normalizeRow({
            ...current,
            outcome: OUTCOMES.skipped,
            cause: result.cause,
          })
        }
        continue
      }
      const index = next.rows.findIndex((row) => row.name === rowName)
      if (index < 0) {
        if (info?.type !== 'round' || !FALLBACK_ROUNDS.has(info.id)) continue
        next.rows.push(rowFor({ name: rowName, phase: phaseForFinding(info) }))
        next.rows = dedupeRows(next.rows)
      }
      const rowIndex = next.rows.findIndex((row) => row.name === rowName)
      if (rowIndex < 0) continue
      const completedAtMs = Math.floor(statSync(path).mtimeMs)
      const current = next.rows[rowIndex]
      const isSelectedFallback =
        (info?.type === 'round' && FALLBACK_ROUNDS.has(info.id)) ||
        (info?.type === 'lens' && ['dispatched', 'inlined'].includes(marker))
      const canCompleteTerminalRow = isSelectedFallback || info?.type === 'lens'
      if (
        (current.outcome === OUTCOMES.timedOut || current.outcome === OUTCOMES.skipped) &&
        !canCompleteTerminalRow
      ) {
        continue
      }
      const tier = current.tier || fallbackTier(info, marker)
      const tierAndMode = normalizeTierAndMode(
        tier,
        current.mode === MODES.unknown ? null : current.mode,
        marker,
      )
      next.rows[rowIndex] = normalizeRow({
        ...current,
        tier: tierAndMode.tier,
        mode: result.fallback ? MODES.inlined : tierAndMode.mode,
        outcome: OUTCOMES.completed,
        cause: result.fallback,
        completedAtMs,
        durationMs: Math.max(0, completedAtMs - next.seededAtMs),
      })
    }
  }
  for (const row of next.rows) {
    if (row.outcome === OUTCOMES.timedOut) {
      row.completedAtMs = null
      row.durationMs = null
      if (!row.cause) row.cause = 'timeout'
    }
  }
  next.rows = dedupeRows(next.rows)
  return normalizeLedger({ ...next, reconciledAtMs: now })
}

export function coverage(ledger) {
  const rows = normalizeLedger(ledger).rows
  const counts = {
    discovered: rows.length,
    completed: 0,
    skipped: 0,
    timedOut: 0,
    notReached: 0,
  }
  for (const row of rows) {
    if (row.outcome === OUTCOMES.completed) counts.completed += 1
    else if (row.outcome === OUTCOMES.skipped) counts.skipped += 1
    else if (row.outcome === OUTCOMES.timedOut) counts.timedOut += 1
    else counts.notReached += 1
  }
  return counts
}

const MISS_LABELS = Object.freeze({
  'inline-fallback': 'inline fallback',
  [OUTCOMES.skipped]: 'skipped',
  [OUTCOMES.timedOut]: 'timed out',
  [OUTCOMES.notReached]: 'not reached',
})
const SECOND_VOICE = 'second-voice'

function roundCapabilities(populations = {}) {
  const capabilities = new Map()
  for (const round of asArray(populations?.rounds)) {
    const name = round?.name ?? round?.extension ?? round?.capability
    if (nonEmptyString(name) && nonEmptyString(round?.capability)) {
      capabilities.set(`round:${name}`, round.capability)
    }
  }
  return capabilities
}

function capabilityOf(row, capabilities) {
  if (row.name.startsWith('default:')) return row.name.slice('default:'.length)
  return capabilities.get(row.name) ?? null
}

function ranForReal(row) {
  return row.outcome === OUTCOMES.completed && row.mode !== MODES.inlined
}

function missFor(row) {
  if (ranForReal(row)) return null
  const kind = row.outcome === OUTCOMES.completed ? 'inline-fallback' : row.outcome
  const fallbackCause = row.tier === 'tier3' ? 'inline rubric tier' : 'no cause recorded'
  return { name: row.name, kind, cause: oneLine(row.cause) || fallbackCause }
}

function renderMiss(miss) {
  return `${miss.name} ${MISS_LABELS[miss.kind]} (${miss.cause})`
}

/**
 * Derive the published review-coverage and cross-model tokens from a reconciled ledger. Pure: the
 * tokens can never claim more than the ledger rows record. A row that did not complete, or that
 * completed by running an inline rubric, is a miss named with its recorded cause — except a
 * `default:<capability>` row a completed same-capability round extension already covered.
 */
export function coverageTokens(ledger, populations = {}) {
  const rows = normalizeLedger(ledger).rows
  const capabilities = roundCapabilities(populations)
  const coveredCapabilities = new Set(
    rows
      .filter((row) => row.name.startsWith('round:') && ranForReal(row))
      .map((row) => capabilities.get(row.name))
      .filter(nonEmptyString),
  )
  const misses = []
  for (const row of rows) {
    const miss = missFor(row)
    if (!miss) continue
    if (row.name.startsWith('default:') && coveredCapabilities.has(capabilityOf(row, capabilities)))
      continue
    misses.push(miss)
  }
  let coverageToken = 'full'
  if (rows.length === 0) coverageToken = 'reduced (no reviewers recorded)'
  else if (misses.length > 0) coverageToken = `reduced (${misses.map(renderMiss).join('; ')})`

  const secondVoiceRows = rows.filter((row) => capabilityOf(row, capabilities) === SECOND_VOICE)
  const ran = secondVoiceRows.some(ranForReal)
  let crossModelToken = null
  if (!ran) {
    crossModelToken =
      secondVoiceRows.length === 0
        ? `skipped: no ${SECOND_VOICE} reviewer in this run`
        : `skipped: ${secondVoiceRows.map((row) => renderMiss(missFor(row))).join('; ')}`
  }
  return { coverage: coverageToken, crossModel: { ran, token: crossModelToken }, misses }
}

function readJSONArg(value, label) {
  if (!value) return {}
  if (existsSync(value)) return JSON.parse(readFileSync(value, 'utf8'))
  try {
    return JSON.parse(value)
  } catch (e) {
    throw new Error(`${label} must be JSON or a readable path: ${e.message}`)
  }
}

function readOptionalJSONArg(value, fallback, label) {
  if (!value) return fallback
  if (existsSync(value)) return JSON.parse(readFileSync(value, 'utf8'))
  try {
    return JSON.parse(value)
  } catch {
    return fallback
  }
}

function parseArgs(argv) {
  const args = { _: [] }
  for (let i = 0; i < argv.length; i += 1) {
    const arg = argv[i]
    if (arg.startsWith('--')) {
      args[arg.slice(2)] = argv[++i]
    } else {
      args._.push(arg)
    }
  }
  return args
}

function usage() {
  return [
    'usage: bs-review-ledger <seed|record|reconcile|coverage|tokens> [options]',
    '  seed --run-id <id> --populations <json-or-file> --out <path> [--now <ms>]',
    '  record --in <path> --out <path> --name <row> [--phase <phase>] [--tier <tier>] [--outcome <outcome>] [--cause <reason>]',
    '  reconcile --in <path> --out <path> --findings-dir <dir> --populations <json-or-file> [--invalid <json-or-file>]',
    '  coverage --in <path>',
    '  tokens --in <path> --populations <json-or-file>',
  ].join('\n')
}

async function main(argv = process.argv.slice(2)) {
  const command = argv[0]
  const args = parseArgs(argv.slice(1))
  if (command === 'seed') {
    const ledger = seedLedger({
      runId: args['run-id'],
      populations: readJSONArg(args.populations, '--populations'),
      now: args.now ? Number.parseInt(args.now, 10) : nowMs(),
    })
    if (args.out) writeLedger(args.out, ledger)
    else process.stdout.write(`${JSON.stringify(ledger, null, 2)}\n`)
    return
  }
  if (command === 'record') {
    const ledger = record(readLedger(args.in), args.name, {
      phase: args.phase,
      tier: args.tier,
      outcome: args.outcome,
      cause: args.cause,
      mode: args.mode,
    })
    if (args.out) writeLedger(args.out, ledger)
    else process.stdout.write(`${JSON.stringify(ledger, null, 2)}\n`)
    return
  }
  if (command === 'reconcile') {
    const ledger = reconcile(readLedger(args.in), {
      findingsDir: args['findings-dir'],
      populations: readJSONArg(args.populations, '--populations'),
      invalid: readOptionalJSONArg(args.invalid, [], '--invalid'),
    })
    if (args.out) writeLedger(args.out, ledger)
    else process.stdout.write(`${JSON.stringify(ledger, null, 2)}\n`)
    return
  }
  if (command === 'coverage') {
    process.stdout.write(`${JSON.stringify(coverage(readLedger(args.in)))}\n`)
    return
  }
  if (command === 'tokens') {
    const tokens = coverageTokens(
      readLedger(args.in),
      readJSONArg(args.populations, '--populations'),
    )
    process.stdout.write(`${JSON.stringify(tokens)}\n`)
    return
  }
  throw new Error(usage())
}

if (isMainModule(import.meta.url)) {
  main().catch((e) => {
    process.stderr.write(`bs-review-ledger: ${e.message}\n`)
    process.exit(1)
  })
}
