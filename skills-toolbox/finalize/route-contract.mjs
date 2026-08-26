// skills-toolbox/finalize/route-contract.mjs
//
// Executable terminal-route obligations for boss-build. The route receipt is
// keyed by a run id so stale receipts from earlier runs fail closed.

import { existsSync, readFileSync, renameSync, writeFileSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { randomUUID } from 'node:crypto'
import { isMainModule } from '../main-module.mjs'

export const TERMINAL_ROUTES = Object.freeze({
  REVIEW_READY: Object.freeze([
    'verify-only-evidence-validated',
    'premise-discharged',
    'required-deferred-asserted',
    'pr-ready',
    'please-review-added',
    'claim-deleted',
    'notes-before-lock-release',
    'stop-hooks-removed',
    'lock-released',
  ]),
  PARTIAL: Object.freeze([
    'partial-gate-satisfied',
    'pr-ready',
    'do-not-merge-marked',
    'claim-deleted',
    'notes-before-lock-release',
    'stop-hooks-removed',
    'lock-released',
  ]),
  BLOCKED: Object.freeze([
    'claim-deleted',
    'notes-before-lock-release',
    'stop-hooks-removed',
    'lock-released',
  ]),
  NO_CHANGE: Object.freeze([
    'claim-deleted',
    'notes-before-lock-release',
    'stop-hooks-removed',
    'lock-released',
  ]),
})

export const OPTIONAL_ROUTE_TOKENS = Object.freeze([
  'blocked-pr-left-draft',
  'entry-state-restored',
  'no-change-breadcrumb-written',
])

export const TERMINAL_OUTCOMES = Object.freeze(Object.keys(TERMINAL_ROUTES))
const KNOWN_TOKENS = new Set(Object.values(TERMINAL_ROUTES).flat().concat(OPTIONAL_ROUTE_TOKENS))

function runIdFrom(opts = {}) {
  return opts.runId ?? process.env.BOSS_BUILD_RUN_ID ?? process.env.BLI_RUNID ?? ''
}

function emptyReceipt(runId) {
  return { runId, stamps: [] }
}

function readReceipt(receiptPath, opts = {}) {
  const runId = runIdFrom(opts)
  let raw
  try {
    raw = readFileSync(receiptPath, 'utf8')
  } catch (err) {
    if (err && err.code === 'ENOENT') return { status: 'absent', runId, stamps: [] }
    return { status: 'unreadable', runId, stamps: [] }
  }
  if (raw.trim() === '') return { status: 'empty', runId, stamps: [] }
  try {
    const parsed = JSON.parse(raw)
    if (parsed.runId !== runId) return { status: 'absent', runId, stamps: [] }
    if (!Array.isArray(parsed.stamps)) return { status: 'unreadable', runId, stamps: [] }
    return { status: 'ok', runId, stamps: parsed.stamps }
  } catch {
    return { status: 'unreadable', runId, stamps: [] }
  }
}

export function stampObligation(receiptPath, token, opts = {}) {
  if (!KNOWN_TOKENS.has(token)) throw new Error(`unknown obligation token: ${token}`)
  const runId = runIdFrom(opts)
  if (!runId) throw new Error('run id is required')
  const receipt = readReceipt(receiptPath, { runId })
  const next =
    receipt.status === 'ok' ? { runId, stamps: [...receipt.stamps] } : emptyReceipt(runId)
  next.stamps.push({ token, at: opts.now ?? new Date().toISOString() })
  const tmp = join(dirname(receiptPath), `.route-contract-${randomUUID()}.tmp`)
  writeFileSync(tmp, JSON.stringify(next, null, 2))
  renameSync(tmp, receiptPath)
  return receiptPath
}

export function assertRouteSatisfied(outcome, receiptOrStamps) {
  const owed = TERMINAL_ROUTES[outcome]
  if (!owed) {
    return {
      ok: false,
      honestOutcome: 'ROUTE_UNSATISFIED',
      missing: [],
      unknown: [],
      error: 'unknown outcome',
    }
  }
  const stamps = Array.isArray(receiptOrStamps)
    ? receiptOrStamps
    : Array.isArray(receiptOrStamps?.stamps)
      ? receiptOrStamps.stamps
      : []
  const stampTokens = stamps.map((stamp) => stamp?.token).filter(Boolean)
  const seen = new Set(stampTokens)
  const unknown = [...seen].filter((token) => !KNOWN_TOKENS.has(token)).sort()
  const missing = owed.filter((token) => !seen.has(token))
  let ordered = 0
  for (const token of stampTokens) {
    if (token === owed[ordered]) ordered += 1
  }
  const orderMissing = owed.slice(ordered)
  if (missing.length === 0 && unknown.length === 0) {
    if (orderMissing.length === 0) return { ok: true, honestOutcome: outcome, missing, unknown }
    return {
      ok: false,
      honestOutcome: 'ROUTE_UNSATISFIED',
      missing: orderMissing,
      unknown,
      error: 'obligations out of order',
    }
  }
  const blockedMissing =
    outcome === 'BLOCKED' ? missing : TERMINAL_ROUTES.BLOCKED.filter((token) => !seen.has(token))
  if (outcome !== 'BLOCKED' && blockedMissing.length === 0 && unknown.length === 0) {
    let blockedOrdered = 0
    for (const token of stampTokens) {
      if (token === TERMINAL_ROUTES.BLOCKED[blockedOrdered]) blockedOrdered += 1
    }
    if (blockedOrdered === TERMINAL_ROUTES.BLOCKED.length) {
      return { ok: true, honestOutcome: 'BLOCKED', missing, unknown, downgraded: true }
    }
    return {
      ok: false,
      honestOutcome: 'ROUTE_UNSATISFIED',
      missing: TERMINAL_ROUTES.BLOCKED.slice(blockedOrdered),
      unknown,
      error: 'BLOCKED obligations out of order',
    }
  }
  return { ok: false, honestOutcome: 'ROUTE_UNSATISFIED', missing, unknown }
}

function usage(errWrite) {
  errWrite(
    'usage: route-contract.mjs assert --outcome <OUTCOME> --receipt <path> [--run-id <id>]\n' +
      '       route-contract.mjs stamp --receipt <path> --token <token> [--run-id <id>]\n' +
      '       route-contract.mjs table\n',
  )
}

function argValue(args, name) {
  const index = args.indexOf(name)
  return index >= 0 ? args[index + 1] : undefined
}

export function runCli(
  argv,
  { outWrite = (s) => process.stdout.write(s), errWrite = (s) => process.stderr.write(s) } = {},
) {
  const [cmd, ...args] = argv
  if (cmd === 'table') {
    outWrite(JSON.stringify(TERMINAL_ROUTES, null, 2) + '\n')
    return 0
  }
  if (cmd === 'stamp') {
    const receipt = argValue(args, '--receipt')
    const token = argValue(args, '--token')
    const runId = argValue(args, '--run-id')
    if (!receipt || !token) {
      usage(errWrite)
      return 2
    }
    try {
      stampObligation(receipt, token, { runId })
      return 0
    } catch (err) {
      errWrite(`route-contract stamp: ${err?.message ?? String(err)}\n`)
      return 1
    }
  }
  if (cmd === 'assert') {
    const outcome = argValue(args, '--outcome')
    const receipt = argValue(args, '--receipt')
    const runId = argValue(args, '--run-id')
    if (!outcome || !receipt) {
      usage(errWrite)
      return 2
    }
    const read = existsSync(receipt)
      ? readReceipt(receipt, { runId })
      : { status: 'absent', stamps: [] }
    const verdict = assertRouteSatisfied(outcome, read.stamps)
    outWrite(verdict.honestOutcome + '\n')
    if (!verdict.ok) {
      errWrite(
        JSON.stringify({
          status: read.status,
          missing: verdict.missing,
          unknown: verdict.unknown,
          error: verdict.error,
        }) + '\n',
      )
    }
    return verdict.ok ? 0 : 1
  }
  usage(errWrite)
  return 2
}

if (isMainModule(import.meta.url)) {
  process.exit(runCli(process.argv.slice(2)))
}
