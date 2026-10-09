#!/usr/bin/env node

// verify-gate.mjs — the mechanical heart of the verify stage: judge an open PR's current head,
// record the verdict as a `boss/verify` commit status plus tracker state, merge a passing PR through
// the one head-pinned merge path, detect a human hand-back, and re-arm the next-head callback.
//
// Verbs (each prints one JSON line on stdout):
//   judge    --pr <n> [--repo o/r] [--extension-results <file>] [--waive <code>]   never writes
//   post     --pr <n> --head <sha> --verdict claim|wait|pass|defect|human [--reason <code>]
//            [--findings <file>] [--token <t>] [--repo o/r] [--dry-run]
//   merge    --pr <n> --head <sha> [--token <t>] [--repo o/r] [--dry-run]
//   approval --pr <n> [--repo o/r]                                                   never writes
//   rearm    --pr <n> --chat <id> [--repo o/r] [--dry-run]
//   candidates [--ticket <id>] [--limit N] [--repo o/r] [selection flags]           never writes
//
// `--dry-run` on a writing verb still runs every read (the planned writes depend on them) and
// prints `{dryRun: true, writes: [...]}` instead of calling anything mutating. Every subprocess goes
// through one injected reader and every tracker write through the resolved tracker adapter's
// optional `applyIssueWrites` capability, which is what keeps every test offline.
//
// A human verdict is never a `failure` status: bossd reads statuses through `gh pr checks`, and a
// failure starts the repair plugin, which cannot fix anything a human verdict names.

import { spawnSync } from 'node:child_process'
import { randomBytes } from 'node:crypto'
import { existsSync, readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'

import { isMainModule } from './main-module.mjs'
import {
  classifyMergeResult,
  judgeCiOnHead,
  mergeArgv,
  parseLedger,
  sectionToken,
  sections,
  sharedEligibility,
} from './merge-eligibility.mjs'
import { CHECK_REASONS, CHECK_STATES } from './pr-check-state.mjs'
import { RECEIPT_CONTEXT, classifyRead, postArgs, readArgs } from './commit-status.mjs'
import {
  DEFAULT_PIPELINE_LABELS,
  DEFAULT_TRACKER_STATES,
  globToRegExp,
  labelName,
  loadSkillConfig,
  stageSelectionQuery,
  stateName,
  verifyAlwaysHumanPaths,
} from './skill-config.mjs'
import { effectiveSelection, parseSelectionFlags } from './selection.mjs'
import {
  classifyVerifyOutcome,
  discoverExtensions,
  validateVerifyContext,
} from './skill-extensions.mjs'
import { resolveTrackerAdapter } from './tracker/adapter.mjs'

export const VERIFY_CONTEXT = 'boss/verify'
export const VERIFY_CORE = 'boss-verify'

// The description prefixes are a contract with the `boss/verify` status renderer and with candidate
// discovery: `verifying…` is a live claim, `waiting:` is an unclaimed head, `needs human:` is a
// policy park, and `verified` / `defect:` are terminal. Change them here and nowhere else.
export const VERIFY_DESCRIPTIONS = Object.freeze({
  verifying: 'verifying…',
  waiting: 'waiting:',
  needsHuman: 'needs human:',
  verified: 'verified',
  verifiedApproved: 'verified (approved)',
  defect: 'defect:',
  // Appended to a `defect:` description when the defect was also handed to a human (auto-repair
  // off or unreadable), so `approval` can tell a parked defect from one repair is working on.
  defectParkedSuffix: ', needs human',
})

export const HUMAN_REASONS = Object.freeze([
  'no-receipt',
  'repair-exhausted',
  'always-human-path',
  'ledger-open',
  'extension-failed',
  'no-session',
])

export const VERIFY_VERDICTS = Object.freeze([
  'wait',
  'extensions-required',
  'pass',
  'defect',
  'human',
  'reverify',
])

// Mirrors `maxRepairLoopAttempts = 5` in plugins/bossd-plugin-repair/server.go: after that many
// attempts the repair plugin gives up, so another `failure` would only park the PR silently.
export const REPAIR_EXHAUSTED_AFTER = 5

// A claim older than this is presumed abandoned and may be taken over.
export const CLAIM_STALE_AFTER_MS = 30 * 60 * 1000

export const NOTE_TRIGGERS = Object.freeze([
  'evidence-unknown',
  'extension-failed',
  'stale-claim-taken-over',
  'verify-failed-after-clean-review',
])

// Wait reasons that mean a read failed rather than that the PR is not ready yet; each records an
// `evidence-unknown` note, because a run that cannot see is not an expected outcome.
export const EVIDENCE_UNKNOWN_REASONS = Object.freeze([
  'pr-unreadable',
  'base-ci-unknown',
  'receipt-unknown',
  'changed-paths-unknown',
  'config-unreadable',
])

// The stable line every tracker comment carries, so an indeterminate write is settled by reading
// the comments back and a rerun never posts the same comment twice.
export function verifyMarker(headSha) {
  return `boss-verify head ${headSha}`
}

const SHA = /^[0-9a-f]{40}$/i
const isSha = (value) => typeof value === 'string' && SHA.test(value)
const text = (value) => typeof value === 'string' && value.trim() !== ''
const PR_FIELDS =
  'state,mergeCommit,number,url,title,body,isDraft,headRefOid,baseRefName,mergeStateStatus,statusCheckRollup,reviewDecision'

export class UsageError extends Error {}

// ---------------------------------------------------------------------------
// The one subprocess seam. A failed read is unknown, never an empty success: `ok` is false and
// `error` names why. `boss --json` failures carry their machine code in `error`.

export function commandReader({ spawn = spawnSync, cwd = process.cwd(), bossBin = 'boss' } = {}) {
  return (command, args, { input, json = false } = {}) => {
    let response
    try {
      response = spawn(command === 'boss' ? bossBin : command, args, {
        cwd,
        input,
        encoding: 'utf8',
        timeout: 60000,
        maxBuffer: 64 * 1024 * 1024,
        killSignal: 'SIGKILL',
      })
    } catch (error) {
      return { ok: false, error: error?.code ?? 'spawn-error', exit: null, output: '' }
    }
    const output = String(response?.stdout ?? '').trim()
    let payload
    if (json) {
      try {
        payload = JSON.parse(response.stdout)
      } catch {
        /* Failure remains unknown. */
      }
    }
    if (response?.error || response?.status !== 0)
      return {
        ok: false,
        error:
          payload?.error?.code ??
          payload?.code ??
          response?.error?.code ??
          `exit-${response?.status}`,
        exit: response?.status ?? null,
        payload,
        output,
      }
    if (json && payload === undefined)
      return { ok: false, error: 'unreadable-json', exit: 0, output }
    return { ok: true, payload, output, exit: 0 }
  }
}

// ---------------------------------------------------------------------------
// Pure helpers.

// The PR body's first non-blank line is `Linear issue: <url>`. Returns `{url, id}` or null.
export function parseLinearIssueLine(body) {
  if (typeof body !== 'string') return null
  const first = body.split(/\r?\n/).find((line) => line.trim() !== '')
  const match = first?.trim().match(/^Linear issue:\s*(\S+)\s*$/i)
  if (!match) return null
  const id = match[1].match(/\/issue\/([A-Za-z][A-Za-z0-9]*-\d+)(?:[/?#]|$)/)?.[1]
  return id ? { url: match[1], id: id.toUpperCase() } : null
}

export function claimDescription(token) {
  return `${VERIFY_DESCRIPTIONS.verifying} ${token}`
}
export function waitingDescription(reason) {
  return `${VERIFY_DESCRIPTIONS.waiting} ${reason}`
}
export function needsHumanDescription(code) {
  return `${VERIFY_DESCRIPTIONS.needsHuman} ${code}`
}
export function defectDescription(count, parked = false) {
  return `${VERIFY_DESCRIPTIONS.defect} ${count} finding(s)${parked ? VERIFY_DESCRIPTIONS.defectParkedSuffix : ''}`
}

// Classifies the latest `boss/verify` status on a head relative to this run's claim token.
// `kind` is `none` (no status), `mine`, `foreign` (a live claim by another run), `stale` (a
// foreign claim older than CLAIM_STALE_AFTER_MS, or with an unreadable timestamp), `waiting`, or
// `terminal` (anything else: verified, defect, needs human).
export function claimState(status, { token = '', now = Date.now() } = {}) {
  if (!status || typeof status !== 'object') return { kind: 'none' }
  const description = typeof status.description === 'string' ? status.description : ''
  if (status.state === 'pending' && description.startsWith(VERIFY_DESCRIPTIONS.verifying)) {
    const claimed = description.slice(VERIFY_DESCRIPTIONS.verifying.length).trim()
    if (text(token) && claimed === token) return { kind: 'mine', token: claimed }
    const at = Date.parse(status.updatedAt ?? '')
    // An unreadable timestamp is treated as stale: a claim nobody can age would otherwise hold the
    // head forever, and a double run costs tokens, never a double merge.
    const stale = !Number.isFinite(at) || now - at > CLAIM_STALE_AFTER_MS
    return { kind: stale ? 'stale' : 'foreign', token: claimed }
  }
  if (status.state === 'pending' && description.startsWith(VERIFY_DESCRIPTIONS.waiting))
    return { kind: 'waiting' }
  return { kind: 'terminal' }
}

// A claim is lost when the latest status is a live or stale claim that is not this run's token.
export function claimLost(status, token, now) {
  const kind = claimState(status, { token, now }).kind
  return kind === 'foreign' || kind === 'stale'
}

// The PR body shows a clean boss-review: no `## Review findings` section and a non-empty
// `## Review coverage` token.
export function cleanReviewBody(body) {
  return (
    typeof body === 'string' &&
    !sections(body).has('review findings') &&
    sectionToken(body, 'review coverage') !== ''
  )
}

/**
 * Combines `verify` extension results. `results` is the parsed `--extension-results` file (an array
 * of `{extension, optional, timedOut, crashed?, result}`); `installed` is the discovered extension
 * list. An installed extension with no result entry counts as crashed. Any malfunction is a human
 * `extension-failed` verdict (repair cannot fix an extension); otherwise any valid `fail` is a
 * defect; otherwise pass. Result content is data and is never executed or followed.
 */
export function aggregateExtensions(results, installed = []) {
  if (!Array.isArray(results))
    return {
      verdict: 'human',
      reason: 'extension-failed',
      outcomes: [],
      failed: ['(results unreadable)'],
      findings: [],
    }
  const entries = results.filter((entry) => entry && typeof entry === 'object')
  const seen = new Set(entries.map((entry) => entry.extension).filter(text))
  for (const ext of installed)
    if (text(ext?.name) && !seen.has(ext.name))
      entries.push({ extension: ext.name, optional: ext.optional === true, crashed: true })
  const outcomes = entries.map((entry) => ({
    extension: text(entry.extension) ? entry.extension : '(unnamed)',
    ...classifyVerifyOutcome({
      result: entry.result,
      timedOut: entry.timedOut === true,
      crashed: entry.crashed === true,
      optional: entry.optional === true,
    }),
  }))
  const failed = outcomes.filter((o) => o.outcome === 'failed').map((o) => o.extension)
  const findings = []
  entries.forEach((entry, index) => {
    if (outcomes[index].outcome === 'ok' && outcomes[index].verdict === 'fail')
      for (const finding of entry.result.findings)
        findings.push({ extension: outcomes[index].extension, ...finding })
  })
  if (failed.length > 0)
    return { verdict: 'human', reason: 'extension-failed', outcomes, failed, findings }
  if (findings.length > 0)
    return { verdict: 'defect', reason: 'findings', outcomes, failed, findings }
  return { verdict: 'pass', reason: 'verified', outcomes, failed, findings }
}

/**
 * The approval read: `{approval: 'merged-by-human'|'policy-park-approved'|'reverify'|'none', waive?,
 * via?}`. `labels` is the ticket's current label names, or null when they could not be read (then
 * only the review path can approve).
 */
export function classifyApproval({
  prState,
  reviewDecision,
  status,
  labels = null,
  needsHumanLabel = DEFAULT_PIPELINE_LABELS.needsHuman,
} = {}) {
  if (prState === 'MERGED') return { approval: 'merged-by-human' }
  const labelRemoved = Array.isArray(labels) ? !labels.includes(needsHumanLabel) : false
  const approved = reviewDecision === 'APPROVED'
  const via = labelRemoved ? 'label-removed' : approved ? 'review-approved' : null
  const description = typeof status?.description === 'string' ? status.description : ''
  if (status?.state === 'pending' && description.startsWith(VERIFY_DESCRIPTIONS.needsHuman)) {
    const code = description.slice(VERIFY_DESCRIPTIONS.needsHuman.length).trim()
    if (via && HUMAN_REASONS.includes(code))
      return { approval: 'policy-park-approved', waive: code, via }
    return { approval: 'none' }
  }
  if (status?.state === 'failure' && description.startsWith(VERIFY_DESCRIPTIONS.defect)) {
    const parked = description.endsWith(VERIFY_DESCRIPTIONS.defectParkedSuffix)
    // A defect repair is working on has no label to remove, so only a review approval counts.
    if (parked ? via : approved)
      return { approval: 'reverify', via: parked ? via : 'review-approved' }
  }
  return { approval: 'none' }
}

// Splits shared-eligibility reasons into `{wait, ledgerOpen}`: merge state, PR shape and head
// reasons resolve by waiting (the CI reasons are judged separately, with `boss/*` excluded); an
// open or unreadable ledger is the PR itself saying a person must look.
const LEDGER_REASONS = new Set([
  'criteria-unknown',
  'criteria-unmet',
  'do-not-merge-marker',
  'human-follow-up-open',
  'follow-up-section-missing',
  'open-questions-open',
  'open-questions-section-missing',
])
export function splitEligibility(reasons) {
  const wait = []
  let ledgerOpen = false
  for (const reason of reasons ?? []) {
    if (LEDGER_REASONS.has(reason)) ledgerOpen = true
    else if (reason !== 'ci-not-settled' && reason !== 'ci-not-green-on-head') wait.push(reason)
  }
  return { wait, ledgerOpen }
}

/**
 * The verdict decision over gathered facts. Wait reasons win over human reasons: a wait writes
 * nothing and the next tick re-judges, while a human verdict pages a person.
 */
export function decideVerdict(facts) {
  const f = facts
  const base = { headSha: f.headSha ?? '', baseSha: f.baseSha ?? '', ticket: f.ticket ?? null }
  const notes = []
  const wait = (reason) => {
    if (EVIDENCE_UNKNOWN_REASONS.includes(reason))
      notes.push({ trigger: 'evidence-unknown', body: `verify could not read evidence: ${reason}` })
    return { verdict: 'wait', reason, ...base, notes }
  }
  if (f.prUnreadable) return wait('pr-unreadable')
  if (f.prState !== 'OPEN') return wait('pr-not-open')
  if (f.isDraft !== false) return wait('pr-draft')
  if (!f.ci?.settled) return wait('ci-not-settled')
  if (!f.ci?.green) return wait('ci-not-green-on-head')
  const split = splitEligibility(f.eligibilityReasons)
  if (split.wait.length > 0) return wait(split.wait[0])
  if (f.baseState === 'red') return wait('base-ci-red')
  if (f.baseState === 'unknown') return wait('base-ci-unknown')
  if (f.receipt === 'unknown') return wait('receipt-unknown')
  if (f.receipt === 'pending') return wait('receipt-pending')
  if (f.changedPaths === null && f.needChangedPaths) return wait('changed-paths-unknown')
  // An unloadable config means the always-human policy is unknown, never empty.
  if (f.configUnreadable) return wait('config-unreadable')

  // A waiver lifts exactly one human code; every wait reason above has already been checked.
  const waive = f.waive ?? null
  const human = []
  if (f.receipt !== 'success') human.push('no-receipt')
  if (split.ledgerOpen) human.push('ledger-open')
  if (Array.isArray(f.alwaysHumanHits) && f.alwaysHumanHits.length > 0)
    human.push('always-human-path')
  const remaining = human.filter((code) => code !== waive)
  if (remaining.length > 0) return { verdict: 'human', reason: remaining[0], ...base, notes }

  // A run carrying a waiver is the approval path, so its pass is recorded as approved.
  const passReason = waive ? 'approved' : 'verified'
  if (!Array.isArray(f.extensions) || f.extensions.length === 0)
    return { verdict: 'pass', reason: passReason, ...base, notes }
  if (f.extensionResults === undefined)
    return { verdict: 'extensions-required', reason: 'extensions-installed', ...base, notes }
  const agg = aggregateExtensions(f.extensionResults, f.extensions)
  const extensions = { outcomes: agg.outcomes }
  if (agg.verdict === 'human' && waive !== 'extension-failed') {
    notes.push({
      trigger: 'extension-failed',
      body: `verify extension(s) malfunctioned: ${agg.failed.join(', ')}`,
    })
    return { verdict: 'human', reason: 'extension-failed', ...base, extensions, notes }
  }
  // Under an `extension-failed` waiver the malfunctioning results are ignored; findings from the
  // extensions that did run still count.
  const findings = agg.findings
  if (findings.length > 0) {
    if (f.cleanReview)
      notes.push({
        trigger: 'verify-failed-after-clean-review',
        body: `verify found ${findings.length} finding(s) on a head boss-review called clean`,
      })
    return { verdict: 'defect', reason: 'findings', ...base, findings, extensions, notes }
  }
  return { verdict: 'pass', reason: passReason, ...base, extensions, notes }
}

// ---------------------------------------------------------------------------
// Reads.

function repoFor(read, repo) {
  if (text(repo)) return repo
  const view = read('gh', ['repo', 'view', '--json', 'nameWithOwner'], { json: true })
  return view.ok && text(view.payload?.nameWithOwner) ? view.payload.nameWithOwner : ''
}

const apiRepo = (repo) => (text(repo) ? `repos/${repo}` : 'repos/{owner}/{repo}')

export function readPr(read, pr, repo) {
  return read(
    'gh',
    ['pr', 'view', String(pr), ...(text(repo) ? ['--repo', repo] : []), '--json', PR_FIELDS],
    { json: true },
  )
}

// The latest status of one context on a head, or `{verdict: 'unknown'}` when the read failed.
export function readStatus(read, sha, repo, context) {
  const r = read('gh', readArgs({ sha, repo }))
  return classifyRead({
    exit: r.ok ? 0 : (r.exit ?? 1),
    stdout: r.output,
    error: r.ok ? undefined : r.exit === null ? { code: r.error } : undefined,
    context,
    sha,
  })
}

function latestStatus(read, sha, repo) {
  const r = readStatus(read, sha, repo, VERIFY_CONTEXT)
  if (r.verdict === 'unknown') return { readable: false, status: null }
  return { readable: true, status: r.contexts?.[VERIFY_CONTEXT] ?? null }
}

// The base branch tip, classified with `boss/*` excluded. `green`/`pending`/`no gate` proceed;
// `failing` is red; a failed read or an unreadable/unclassified set is unknown (fail closed).
export function readBaseState(read, repo, baseRefName) {
  if (!text(baseRefName)) return { state: 'unknown', sha: '' }
  const tip = read('gh', ['api', `${apiRepo(repo)}/commits/${encodeURIComponent(baseRefName)}`], {
    json: true,
  })
  const sha = tip.ok && isSha(tip.payload?.sha) ? tip.payload.sha : ''
  if (!sha) return { state: 'unknown', sha: '' }
  const runs = read(
    'gh',
    ['api', `${apiRepo(repo)}/commits/${sha}/check-runs?per_page=100`, '--paginate', '--slurp'],
    { json: true },
  )
  const combined = read('gh', ['api', `${apiRepo(repo)}/commits/${sha}/status?per_page=100`], {
    json: true,
  })
  if (!runs.ok || !combined.ok || !Array.isArray(combined.payload?.statuses))
    return { state: 'unknown', sha }
  const judged = judgeCiOnHead({
    headSHA: sha,
    observedSHA: sha,
    rollup: combined.payload.statuses,
    checkRuns: runs.payload,
  })
  const v = judged.verdict
  if (v.state === CHECK_STATES.FAILING) return { state: 'red', sha, check: v.reason }
  if (
    v.state === CHECK_STATES.UNKNOWN &&
    (v.reason === CHECK_REASONS.UNREADABLE || v.reason === CHECK_REASONS.UNCLASSIFIED)
  )
    return { state: 'unknown', sha, check: v.reason }
  return { state: 'ok', sha, check: v.reason }
}

function readChangedPaths(read, pr, repo) {
  const r = read('gh', [
    'pr',
    'diff',
    String(pr),
    ...(text(repo) ? ['--repo', repo] : []),
    '--name-only',
  ])
  if (!r.ok) return null
  return r.output
    .split(/\r?\n/)
    .map((line) => line.trim())
    .filter(Boolean)
}

function loadConfig(deps) {
  return loadConfigResult(deps).config
}

// The judge reads policy (alwaysHumanPaths) from config, so it must tell an invalid config from an
// absent one: a swallowed validation error would silently drop the human-review restriction.
function loadConfigResult(deps) {
  try {
    return { config: (deps.loadConfig ?? loadSkillConfig)({ cwd: deps.cwd ?? process.cwd() }) }
  } catch (err) {
    return { config: null, error: err }
  }
}

function roleName(fn, config, role, fallback) {
  try {
    return fn(config, role)
  } catch {
    return fallback
  }
}

function resolveAdapter(deps) {
  if (deps.adapter !== undefined) return deps.adapter
  try {
    return resolveTrackerAdapter({ env: deps.env ?? process.env })
  } catch {
    return null
  }
}

function readJsonFile(path) {
  try {
    return JSON.parse(readFileSync(path, 'utf8'))
  } catch {
    return undefined
  }
}

// ---------------------------------------------------------------------------
// Notes: one seam, never fatal.

const NOTES_RECORD = fileURLToPath(new URL('./notes-record.mjs', import.meta.url))

export function recordNote({ trigger, body, pr, headSha }, deps = {}) {
  const read = deps.read ?? commandReader(deps)
  const exists = deps.exists ?? existsSync
  try {
    if (exists(NOTES_RECORD)) {
      const node = deps.nodeBin ?? process.execPath
      const add = read(node, [NOTES_RECORD, 'add', '--trigger', trigger, '--body', body])
      const flush = add.ok ? read(node, [NOTES_RECORD, 'flush']) : { ok: false }
      return { recorded: add.ok && flush.ok, via: 'notes-record' }
    }
    const key = `verify:${trigger}:${pr}:${String(headSha ?? '').slice(0, 12)}`
    const r = read(
      'boss',
      [
        'notes',
        'add',
        '--tag',
        'verify',
        '--tag',
        'improvement',
        '--idempotency-key',
        key,
        '--json',
        '--',
        body,
      ],
      { json: true },
    )
    return { recorded: r.ok, via: 'boss-notes' }
  } catch {
    return { recorded: false, via: 'error' }
  }
}

// ---------------------------------------------------------------------------
// Verbs.

export async function judge(options, deps = {}) {
  const read = deps.read ?? commandReader(deps)
  const repo = repoFor(read, options.repo)
  const pr = options.pr
  if (options.waive !== undefined && !HUMAN_REASONS.includes(options.waive))
    throw new UsageError(`verify-gate: --waive must be one of ${HUMAN_REASONS.join(', ')}`)
  const view = readPr(read, pr, repo)
  if (!view.ok) return decideVerdict({ prUnreadable: true })
  const p = view.payload ?? {}
  const headSha = isSha(p.headRefOid) ? p.headRefOid : ''
  const ticket = parseLinearIssueLine(p.body)
  const ci = judgeCiOnHead({
    headSHA: headSha,
    observedSHA: p.headRefOid ?? '',
    rollup: p.statusCheckRollup,
  })
  const ledger = parseLedger(p.body)
  const eligibility = sharedEligibility({
    criteria: ledger.criteria,
    ciWaitState: ci.settled ? 'settled' : 'pending',
    checkVerdict: ci.verdict,
    headSha,
    prView: p,
    heads: [headSha, p.headRefOid],
    humanFollowUp: ledger.humanFollowUp,
    openQuestions: ledger.openQuestions,
  })
  const facts = {
    prState: p.state,
    isDraft: p.isDraft,
    headSha,
    ticket,
    ci: { settled: ci.settled, green: ci.green },
    eligibilityReasons: eligibility.reasons,
    waive: options.waive ?? null,
    cleanReview: cleanReviewBody(p.body),
  }
  // Cheap gates first: nothing below runs for a head that is going to wait anyway.
  const early = decideVerdict(facts)
  const factsOut = (extra = {}) => ({
    prState: p.state,
    isDraft: p.isDraft,
    mergeStateStatus: p.mergeStateStatus ?? '',
    ci: { state: ci.verdict.state, reason: ci.verdict.reason, excluded: ci.excluded },
    eligibilityReasons: eligibility.reasons,
    ...extra,
  })
  if (early.verdict === 'wait') return { ...early, facts: factsOut() }

  const baseState = readBaseState(read, repo, p.baseRefName)
  facts.baseSha = baseState.sha
  facts.baseState = baseState.state
  const receipt = readStatus(read, headSha, repo, RECEIPT_CONTEXT)
  facts.receipt =
    receipt.verdict === 'unknown'
      ? 'unknown'
      : receipt.verdict === 'present' && receipt.state === 'success'
        ? 'success'
        : receipt.verdict === 'present' && receipt.state === 'pending'
          ? 'pending'
          : 'absent'
  const loaded = loadConfigResult(deps)
  facts.configUnreadable = loaded.error !== undefined
  const humanPaths = verifyAlwaysHumanPaths(loaded.config)
  const discovered = (deps.discover ?? discoverExtensions)({
    core: VERIFY_CORE,
    role: 'verify',
    root: deps.cwd ?? process.cwd(),
  })
  facts.extensions = discovered?.extensions ?? []
  // The diff is read only when something consumes it: a non-empty always-human list, or the
  // extension envelope.
  facts.needChangedPaths =
    humanPaths.length > 0 || (facts.extensions.length > 0 && options.extensionResults === undefined)
  const changedPaths = facts.needChangedPaths ? readChangedPaths(read, pr, repo) : []
  facts.changedPaths = changedPaths
  facts.alwaysHumanHits = Array.isArray(changedPaths)
    ? changedPaths.filter((path) => humanPaths.some((glob) => globToRegExp(glob).test(path)))
    : []
  if (options.extensionResults !== undefined) {
    const parsed = readJsonFile(options.extensionResults)
    facts.extensionResults = Array.isArray(parsed) ? parsed : null
  }
  const verdict = decideVerdict(facts)
  const out = {
    ...verdict,
    facts: factsOut({
      base: { sha: baseState.sha, state: baseState.state, check: baseState.check ?? '' },
      receipt: { verdict: receipt.verdict, state: receipt.state ?? '' },
      alwaysHumanHits: facts.alwaysHumanHits,
      extensionsInstalled: facts.extensions.map((ext) => ext.name),
    }),
  }
  if (verdict.verdict === 'extensions-required') {
    const context = {
      pr: {
        number: Number(p.number ?? pr),
        url: p.url ?? '',
        title: p.title ?? '',
        body: p.body ?? '',
      },
      headSha,
      baseSha: baseState.sha,
      changedPaths: changedPaths ?? [],
      ticket: ticket ? { id: ticket.id, url: ticket.url } : null,
    }
    const valid = validateVerifyContext(context)
    out.extensionEnvelope = valid.ok ? context : { invalid: valid.errors }
  }
  return out
}

function trackerNames(deps) {
  const config = loadConfig(deps)
  return {
    needsHuman: roleName(labelName, config, 'needsHuman', DEFAULT_PIPELINE_LABELS.needsHuman),
    done: roleName(stateName, config, 'done', DEFAULT_TRACKER_STATES.done),
  }
}

// Probes the tracker write path with a read-only call (no writes requested), which also resolves
// the ticket and returns its labels. Unavailable when the PR names no ticket, the adapter lacks the
// capability, or its credential is missing.
async function trackerProbe(adapter, ticket) {
  if (!ticket) return { available: false, why: 'no-ticket-line' }
  if (typeof adapter?.applyIssueWrites !== 'function')
    return { available: false, why: 'no-capability' }
  try {
    const r = await adapter.applyIssueWrites({ issueId: ticket.id })
    if (r?.outcome !== 'ok') return { available: false, why: r?.reason ?? 'probe-failed' }
    return { available: true, labels: r.issue?.labels ?? null }
  } catch (error) {
    return { available: false, why: error?.code ?? error?.message ?? 'probe-threw' }
  }
}

function humanComment({ code, pr, url }) {
  return [
    `boss-verify needs a human for PR #${pr}${url ? ` (${url})` : ''}: \`${code}\`.`,
    'Remove the needs-human label or approve the PR to hand it back; the next verify run re-judges the head.',
  ].join('\n\n')
}

function defectTrackerComment({ count, pr, url, commentUrl }) {
  return [
    `boss-verify found ${count} finding(s) on PR #${pr}${url ? ` (${url})` : ''}, and auto-repair is not on for this repository.`,
    commentUrl ? `Findings: ${commentUrl}` : '',
    'Fix the findings, or remove the needs-human label (or approve the PR) to have verify re-judge the head.',
  ]
    .filter(Boolean)
    .join('\n\n')
}

function findingsComment(findings, headSha) {
  const lines = [
    `### boss-verify: ${findings.length} finding(s) on \`${headSha.slice(0, 12)}\``,
    '',
  ]
  for (const f of findings) {
    const where = text(f.file) ? ` (${f.file}${Number.isInteger(f.line) ? `:${f.line}` : ''})` : ''
    lines.push(`- **${String(f.title)}**${where}${text(f.detail) ? ` — ${f.detail}` : ''}`)
  }
  lines.push('', verifyMarker(headSha))
  return lines.join('\n')
}

function readSessionRow(read, prUrl) {
  const ls = read('boss', ['ls', '--json'], { json: true })
  if (!ls.ok || !Array.isArray(ls.payload?.sessions)) return { readable: false, row: null }
  return {
    readable: true,
    row: ls.payload.sessions.find((row) => text(prUrl) && row?.pr_url === prUrl) ?? null,
  }
}

// `{autoRepair: true|false|null, attempts: number|null}`; null means unreadable.
function readRepairFacts(read, prUrl) {
  const session = readSessionRow(read, prUrl)
  if (!session.row || !text(session.row.id)) return { autoRepair: null, attempts: null }
  const show = read('boss', ['show', session.row.id, '--json'], { json: true })
  if (!show.ok) return { autoRepair: null, attempts: null }
  const s = show.payload?.session ?? {}
  const flag = s.repo_can_auto_repair
  const attempts = s.last_repair?.attempt_count
  return {
    autoRepair: typeof flag === 'boolean' ? flag : null,
    attempts: Number.isInteger(attempts) ? attempts : 0,
  }
}

export async function post(options, deps = {}) {
  const read = deps.read ?? commandReader(deps)
  const now = (deps.now ?? Date.now)()
  const dryRun = options.dryRun === true
  const { verdict, pr } = options
  const headSha = typeof options.head === 'string' ? options.head.toLowerCase() : ''
  if (!isSha(headSha)) throw new UsageError('verify-gate: post needs --head <40-hex sha>')
  if (!['claim', 'wait', 'pass', 'defect', 'human'].includes(verdict))
    throw new UsageError('verify-gate: post --verdict must be claim|wait|pass|defect|human')
  if (verdict === 'human' && !HUMAN_REASONS.includes(options.reason))
    throw new UsageError(
      `verify-gate: post --verdict human needs --reason ${HUMAN_REASONS.join('|')}`,
    )
  if (verdict === 'wait' && !text(options.reason))
    throw new UsageError('verify-gate: post --verdict wait needs --reason <code>')
  let findings = []
  if (verdict === 'defect') {
    const parsed = readJsonFile(options.findings ?? '')
    findings = Array.isArray(parsed)
      ? parsed
      : Array.isArray(parsed?.findings)
        ? parsed.findings
        : []
    findings = findings.filter((f) => f && typeof f === 'object' && text(f.title))
    if (findings.length === 0)
      throw new UsageError(
        'verify-gate: post --verdict defect needs --findings <file> with ≥1 finding',
      )
  }
  const repo = repoFor(read, options.repo)
  const writes = []
  const status = (state, description, targetUrl) => {
    const args = postArgs({
      context: VERIFY_CONTEXT,
      state,
      sha: headSha,
      description,
      targetUrl,
      repo,
    })
    writes.push({ kind: 'status', state, description, ...(targetUrl ? { targetUrl } : {}) })
    return args
  }
  const runStatus = (args) => (dryRun ? { ok: true } : read('gh', args))
  const notes = []
  const note = (trigger, body) => notes.push({ trigger, body })
  const flushNotes = () => {
    for (const n of notes) {
      writes.push({ kind: 'note', trigger: n.trigger, body: n.body })
      if (!dryRun) recordNote({ ...n, pr, headSha }, { ...deps, read })
    }
  }
  const finish = (result) => (dryRun ? { dryRun: true, ...result, writes } : { ...result, writes })

  const current = latestStatus(read, headSha, repo)

  if (verdict === 'claim') {
    if (!current.readable) return finish({ won: false, reason: 'status-unreadable' })
    const state = claimState(current.status, { token: options.token, now })
    if (state.kind === 'foreign') return finish({ won: false, reason: 'claim-held' })
    const token = text(options.token) ? options.token : (deps.randomToken ?? defaultToken)()
    const posted = runStatus(status('pending', claimDescription(token)))
    if (state.kind === 'stale')
      note('stale-claim-taken-over', `took over a stale verify claim ${state.token} on PR #${pr}`)
    if (dryRun) {
      flushNotes()
      return finish({ won: true, token })
    }
    if (!posted.ok) return finish({ won: false, token, reason: 'status-post-failed' })
    const back = latestStatus(read, headSha, repo)
    const won = back.readable && claimState(back.status, { token, now }).kind === 'mine'
    if (won) flushNotes()
    return finish({ won, token })
  }

  // Before any terminal post (and a wait that holds a claim): a later claim by another run wins.
  if (!current.readable) return finish({ verdict, posted: false, reason: 'status-unreadable' })
  if (claimLost(current.status, options.token, now))
    return finish({ verdict, abandoned: 'claim-lost' })

  if (verdict === 'wait') {
    if (EVIDENCE_UNKNOWN_REASONS.includes(options.reason))
      note('evidence-unknown', `verify could not read evidence on PR #${pr}: ${options.reason}`)
    let posted = false
    if (claimState(current.status, { token: options.token, now }).kind === 'mine') {
      posted = runStatus(status('pending', waitingDescription(options.reason))).ok
    }
    flushNotes()
    return finish({ verdict, reason: options.reason, posted })
  }

  const view = readPr(read, pr, repo)
  const p = view.ok ? (view.payload ?? {}) : {}
  if (view.ok && p.headRefOid && p.headRefOid.toLowerCase() !== headSha)
    return finish({ verdict: 'reverify', reason: 'head-moved', posted: false })
  const ticket = parseLinearIssueLine(p.body)
  const names = trackerNames(deps)
  const marker = verifyMarker(headSha)

  if (verdict === 'pass') {
    const description =
      options.reason === 'approved'
        ? VERIFY_DESCRIPTIONS.verifiedApproved
        : VERIFY_DESCRIPTIONS.verified
    if (current.status?.state === 'success' && current.status.description === description)
      return finish({ verdict, posted: false, skipped: 'already-posted' })
    const posted = runStatus(status('success', description)).ok
    flushNotes()
    return finish({ verdict, posted })
  }

  let effective = verdict
  let reason = options.reason
  let repair = null
  if (verdict === 'defect') {
    repair = readRepairFacts(read, p.url)
    if (repair.attempts !== null && repair.attempts >= REPAIR_EXHAUSTED_AFTER) {
      effective = 'human'
      reason = 'repair-exhausted'
    } else if (cleanReviewBody(p.body)) {
      note(
        'verify-failed-after-clean-review',
        `verify found ${findings.length} finding(s) on PR #${pr} after a clean boss-review`,
      )
    }
  }
  if (effective === 'human' && reason === 'extension-failed')
    note('extension-failed', `a verify extension malfunctioned on PR #${pr}`)

  const parked = effective === 'human' || repair?.autoRepair !== true
  const adapter = resolveAdapter(deps)
  const probe = parked ? await trackerProbe(adapter, ticket) : { available: true }
  const trackerWrite = parked
    ? {
        issueId: ticket?.id ?? '',
        addLabels: [names.needsHuman],
        comment: '',
        mentionCreator: true,
        marker,
      }
    : null

  if (effective === 'human') {
    trackerWrite.comment = humanComment({ code: reason, pr, url: p.url })
    const description = needsHumanDescription(reason)
    if (current.status?.state === 'pending' && current.status.description === description)
      return finish({ verdict: effective, reason, posted: false, skipped: 'already-posted' })
    writes.push({ kind: 'tracker', ...trackerWrite })
    if (!probe.available) {
      return finish({
        verdict: effective,
        reason,
        posted: false,
        trackerWrites: 'unavailable',
        why: probe.why,
      })
    }
    if (!dryRun) {
      const applied = await adapter.applyIssueWrites(trackerWrite)
      if (applied?.outcome !== 'ok')
        return finish({
          verdict: effective,
          reason,
          posted: false,
          trackerWrites: applied?.outcome ?? 'failed',
        })
    }
    const posted = runStatus(status('pending', description)).ok
    flushNotes()
    return finish({ verdict: effective, reason, posted })
  }

  // Defect: the PR findings comment first (its URL is the status target), then the tracker writes
  // when auto-repair will not pick it up, then the `failure` status.
  if (parked && !probe.available) {
    writes.push({ kind: 'pr-comment', body: findingsComment(findings, headSha) })
    writes.push({
      kind: 'tracker',
      ...trackerWrite,
      comment: defectTrackerComment({ count: findings.length, pr, url: p.url }),
    })
    return finish({
      verdict: effective,
      posted: false,
      trackerWrites: 'unavailable',
      why: probe.why,
    })
  }
  const body = findingsComment(findings, headSha)
  writes.push({ kind: 'pr-comment', body })
  let commentUrl = ''
  if (!dryRun) {
    const c = read(
      'gh',
      ['api', '--method', 'POST', `${apiRepo(repo)}/issues/${pr}/comments`, '-f', `body=${body}`],
      { json: true },
    )
    commentUrl = c.ok && /^https?:\/\//.test(c.payload?.html_url ?? '') ? c.payload.html_url : ''
    if (!commentUrl)
      return finish({ verdict: effective, posted: false, reason: 'pr-comment-failed' })
  }
  if (parked) {
    trackerWrite.comment = defectTrackerComment({
      count: findings.length,
      pr,
      url: p.url,
      commentUrl,
    })
    writes.push({ kind: 'tracker', ...trackerWrite })
    if (!dryRun) {
      const applied = await adapter.applyIssueWrites(trackerWrite)
      if (applied?.outcome !== 'ok')
        return finish({
          verdict: effective,
          posted: false,
          trackerWrites: applied?.outcome ?? 'failed',
          commentUrl,
        })
    }
  }
  const posted = runStatus(
    status('failure', defectDescription(findings.length, parked), commentUrl || undefined),
  ).ok
  flushNotes()
  return finish({ verdict: effective, posted, commentUrl, autoRepair: repair?.autoRepair ?? null })
}

export async function merge(options, deps = {}) {
  const read = deps.read ?? commandReader(deps)
  const now = (deps.now ?? Date.now)()
  const dryRun = options.dryRun === true
  const headSha = typeof options.head === 'string' ? options.head.toLowerCase() : ''
  if (!isSha(headSha)) throw new UsageError('verify-gate: merge needs --head <40-hex sha>')
  const repo = repoFor(read, options.repo)
  const writes = []
  const finish = (result) => (dryRun ? { dryRun: true, ...result, writes } : { ...result, writes })
  const view = readPr(read, options.pr, repo)
  if (!view.ok) return finish({ merged: false, verdict: 'wait', reason: 'pr-unreadable' })
  const p = view.payload ?? {}
  const names = trackerNames(deps)
  const ticket = parseLinearIssueLine(p.body)
  const done = async (result) => {
    const write = {
      issueId: ticket?.id ?? '',
      stateName: names.done,
      removeLabels: [names.needsHuman],
    }
    writes.push({ kind: 'tracker', ...write })
    const adapter = resolveAdapter(deps)
    const probe = await trackerProbe(adapter, ticket)
    if (!probe.available) return finish({ ...result, trackerWrites: 'unavailable', why: probe.why })
    if (dryRun) return finish(result)
    const applied = await adapter.applyIssueWrites(write)
    return finish({ ...result, trackerWrites: applied?.outcome ?? 'failed' })
  }
  if (p.state === 'MERGED' && isSha(p.mergeCommit?.oid))
    return done({ merged: true, mergeSha: p.mergeCommit.oid, observed: 'already-merged' })
  if (String(p.headRefOid ?? '').toLowerCase() !== headSha)
    return finish({ merged: false, verdict: 'reverify', reason: 'head-moved' })

  const current = latestStatus(read, headSha, repo)
  if (!current.readable)
    return finish({ merged: false, verdict: 'wait', reason: 'status-unreadable' })
  if (claimLost(current.status, options.token, now))
    return finish({ merged: false, abandoned: 'claim-lost' })
  if (current.status?.state !== 'success')
    return finish({ merged: false, verdict: 'wait', reason: 'not-verified' })

  const ci = judgeCiOnHead({
    headSHA: headSha,
    observedSHA: p.headRefOid,
    rollup: p.statusCheckRollup,
  })
  const ledger = parseLedger(p.body)
  const eligibility = sharedEligibility({
    criteria: ledger.criteria,
    ciWaitState: ci.settled ? 'settled' : 'pending',
    checkVerdict: ci.verdict,
    headSha,
    prView: p,
    heads: [headSha, p.headRefOid],
    humanFollowUp: ledger.humanFollowUp,
    openQuestions: ledger.openQuestions,
  })
  // `verified (approved)` on this head is a human's waiver of a parked code (judge --waive). The
  // ledger reasons are the only ones a waiver can lift, so merge lets them through on that status
  // and still enforces every merge-state, PR-shape, head and CI reason.
  const approved = current.status?.description === VERIFY_DESCRIPTIONS.verifiedApproved
  const reasons = eligibility.reasons.filter((r) => !(approved && LEDGER_REASONS.has(r)))
  if (ci.settled && !ci.green && !reasons.includes('ci-not-green-on-head'))
    reasons.push('ci-not-green-on-head')
  if (reasons.length > 0) return finish({ merged: false, verdict: 'wait', reasons })

  const session = readSessionRow(read, p.url)
  if (!session.readable)
    return finish({ merged: false, verdict: 'wait', reason: 'sessions-unreadable' })
  if (!session.row || !text(session.row.id))
    return finish({ merged: false, verdict: 'human', reason: 'no-session' })
  const argv = mergeArgv({ sessionId: session.row.id, headSha })
  writes.push({ kind: 'merge', argv })
  if (dryRun) return done({ merged: false, planned: true })

  const result = read('boss', argv, { json: true })
  const mergeVerdict = classifyMergeResult(result)
  if (mergeVerdict === 'head-mismatch')
    return finish({ merged: false, verdict: 'reverify', reason: 'head-mismatch' })
  // A daemon response is not evidence of a merge SHA; one bounded re-read handles a slow update.
  for (let attempt = 0; attempt < 2; attempt++) {
    const live = readPr(read, options.pr, repo)
    if (live.ok && live.payload?.state === 'MERGED' && isSha(live.payload?.mergeCommit?.oid))
      return done({ merged: true, mergeSha: live.payload.mergeCommit.oid })
    if (live.ok && live.payload?.state !== 'MERGED') break
  }
  return finish({
    merged: false,
    verdict: 'wait',
    reason: mergeVerdict === 'accepted' ? 'merge-not-observed' : mergeVerdict,
  })
}

export async function approval(options, deps = {}) {
  const read = deps.read ?? commandReader(deps)
  const repo = repoFor(read, options.repo)
  const view = readPr(read, options.pr, repo)
  if (!view.ok) return { approval: 'none', reason: 'pr-unreadable' }
  const p = view.payload ?? {}
  if (p.state === 'MERGED') return { approval: 'merged-by-human' }
  const headSha = isSha(p.headRefOid) ? p.headRefOid : ''
  const current = headSha ? latestStatus(read, headSha, repo) : { readable: false, status: null }
  if (!current.readable) return { approval: 'none', reason: 'status-unreadable' }
  const ticket = parseLinearIssueLine(p.body)
  const probe = await trackerProbe(resolveAdapter(deps), ticket)
  const names = trackerNames(deps)
  return {
    ...classifyApproval({
      prState: p.state,
      reviewDecision: p.reviewDecision,
      status: current.status,
      labels: probe.available ? probe.labels : null,
      needsHumanLabel: names.needsHuman,
    }),
    headSha,
  }
}

export function rearmArgv({ pr, chat, repo }) {
  return [
    'callback',
    'add',
    String(pr),
    'checks_passed_ready',
    '--on-transition',
    '--chat',
    chat,
    ...(text(repo) ? ['--repo', repo] : []),
    '--message',
    `/boss-verify ${pr}`,
    '--json',
  ]
}

export async function rearm(options, deps = {}) {
  const read = deps.read ?? commandReader(deps)
  if (!text(options.chat)) throw new UsageError('verify-gate: rearm needs --chat <id>')
  const repo = repoFor(read, options.repo)
  const argv = rearmArgv({ pr: options.pr, chat: options.chat, repo })
  const writes = [{ kind: 'callback', argv }]
  if (options.dryRun === true) return { dryRun: true, writes }
  const r = read('boss', argv, { json: true })
  return { armed: r.ok, ...(r.ok ? { callback: r.payload } : { reason: r.error }), writes }
}

function defaultToken() {
  return randomBytes(4).toString('hex')
}

// ---------------------------------------------------------------------------
// candidates: which open PRs need judging, and with what. Never writes.

// One open-PR page is the whole universe a sweep considers; more open PRs than this is a repo the
// verify stage does not scale to yet, and the oldest-first walk below still makes progress.
export const CANDIDATE_PR_LIST_LIMIT = 200
const PR_LIST_FIELDS = 'number,url,body,headRefOid,isDraft'

// The PR numbers a ticket's attachments name in this repository (`…/<owner>/<repo>/pull/<n>`).
export function attachedPrNumbers(ticket, repo) {
  const out = new Set()
  if (!text(repo)) return out
  const escaped = repo.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')
  const re = new RegExp(`github\\.com/${escaped}/pull/(\\d+)(?:[/?#]|$)`, 'i')
  for (const attachment of ticket?.attachments ?? []) {
    const hit = typeof attachment?.url === 'string' ? attachment.url.match(re) : null
    if (hit) out.add(Number(hit[1]))
  }
  return out
}

/**
 * Link tickets to open PRs. A PR belongs to a ticket when its body's first line names the ticket,
 * or the ticket carries an attachment for it. Returns `{linked: [{ticket, pr}], skipped}` where
 * `skipped` holds `no-open-pr`, `ambiguous-pr` (two open PRs, or a PR whose body names a different
 * ticket than the attachment) and `draft`. Nothing is guessed.
 */
export function linkTicketsToPrs(tickets, prs, repo) {
  const linked = []
  const skipped = []
  const bodyTicket = new Map(prs.map((p) => [p.number, parseLinearIssueLine(p.body)?.id ?? null]))
  for (const ticket of tickets) {
    const id = String(ticket.identifier ?? '').toUpperCase()
    const attached = attachedPrNumbers(ticket, repo)
    const mine = prs.filter((p) => bodyTicket.get(p.number) === id || attached.has(p.number))
    if (mine.length === 0) {
      skipped.push({ ticket: id, reason: 'no-open-pr' })
      continue
    }
    const conflicting = mine.some((p) => {
      const named = bodyTicket.get(p.number)
      return named !== null && named !== id
    })
    if (mine.length > 1 || conflicting) {
      skipped.push({ ticket: id, pr: mine[0].number, reason: 'ambiguous-pr' })
      continue
    }
    const [pr] = mine
    if (pr.isDraft === true) {
      skipped.push({ ticket: id, pr: pr.number, reason: 'draft' })
      continue
    }
    linked.push({ ticket, pr })
  }
  linked.sort((a, b) => a.pr.number - b.pr.number)
  return { linked, skipped }
}

// The status half of the per-head read: `{skip}` or `{claim, parked?}` where `parked` names a head
// whose status needs the approval read before it can be admitted.
export function classifyHeadStatus(status, now) {
  if (!status) return { claim: 'none' }
  const description = typeof status.description === 'string' ? status.description : ''
  if (status.state === 'success') return { skip: 'verified' }
  if (status.state === 'failure' && description.startsWith(VERIFY_DESCRIPTIONS.defect))
    return { claim: 'none', parked: 'defect-parked' }
  if (status.state === 'pending' && description.startsWith(VERIFY_DESCRIPTIONS.needsHuman))
    return { claim: 'none', parked: 'parked' }
  const state = claimState(status, { now })
  if (state.kind === 'foreign') return { skip: 'claimed' }
  if (state.kind === 'stale') return { claim: 'stale' }
  if (state.kind === 'waiting') return { claim: 'waiting' }
  return { skip: 'unknown:status' }
}

// The CI half: `boss/*` excluded, pending is unsettled, failing is red, anything else that is not
// a proven green on this head is unknown.
export function classifyHeadCi(view, headSha) {
  const ci = judgeCiOnHead({
    headSHA: headSha,
    observedSHA: view?.headRefOid ?? '',
    rollup: view?.statusCheckRollup,
  })
  if (ci.green) return null
  if (!ci.settled) return 'ci-unsettled'
  if (ci.verdict.state === CHECK_STATES.FAILING) return 'ci-red'
  return 'unknown:ci'
}

function candidateQuery(config, flags, ticket) {
  const base = stageSelectionQuery(config, 'verify', ticket ? {} : flags)
  const needsHuman = roleName(labelName, config, 'needsHuman', DEFAULT_PIPELINE_LABELS.needsHuman)
  return {
    state: stateName(config, 'inReview'),
    // Naming a ticket bypasses the selection (as in boss-build); the core rules still apply.
    selection: ticket ? effectiveSelection(null, 'verify') : base.selection,
    requireLabels: [labelName(config, 'agentBuild')],
    // needs-human comes back so a parked ticket can be checked for a hand-back.
    excludeLabels: base.excludeLabels.filter((label) => label !== needsHuman),
    limit: 250,
  }
}

/**
 * `{candidates: [{ticket, pr, url, headSha, repo, approval?, waive?, claim}], skipped: [{ticket,
 * pr?, reason}]}`. Unknown per-head evidence is a `unknown:<what>` skip; a failed tracker read, an
 * unloadable config or an unreadable `gh pr list` throws, so the CLI exits non-zero rather than
 * printing an empty list.
 */
export async function candidates(options = {}, deps = {}) {
  const read = deps.read ?? commandReader(deps)
  const now = (deps.now ?? Date.now)()
  const limit = Number.isInteger(options.limit) && options.limit > 0 ? options.limit : Infinity
  const loaded = loadConfigResult(deps)
  if (loaded.error) throw new Error(`verify-gate candidates: config: ${loaded.error.message}`)
  const config = loaded.config
  const ticketRef = text(options.ticket) ? options.ticket.trim().toUpperCase() : ''
  const query = candidateQuery(config, options.selection ?? {}, ticketRef)
  const adapter = resolveAdapter(deps)
  if (typeof adapter?.selectPlanned !== 'function')
    throw new Error('verify-gate candidates: the tracker adapter has no selectPlanned capability')
  const fetched = await adapter.selectPlanned(query)
  if (!Array.isArray(fetched))
    throw new Error('verify-gate candidates: the tracker returned an unreadable ticket list')
  let tickets = fetched
  const skipped = []
  if (ticketRef) {
    tickets = fetched.filter((t) => String(t?.identifier ?? '').toUpperCase() === ticketRef)
    if (tickets.length === 0) {
      return {
        candidates: [],
        skipped: [{ ticket: ticketRef, reason: 'not-in-review' }],
      }
    }
  }
  const repo = repoFor(read, options.repo)
  if (!text(repo)) throw new Error('verify-gate candidates: could not resolve the repository')
  const list = read(
    'gh',
    [
      'pr',
      'list',
      '--repo',
      repo,
      '--state',
      'open',
      '--limit',
      String(CANDIDATE_PR_LIST_LIMIT),
      '--json',
      PR_LIST_FIELDS,
    ],
    { json: true },
  )
  if (!list.ok || !Array.isArray(list.payload))
    throw new Error(`verify-gate candidates: gh pr list failed (${list.error ?? 'unreadable'})`)
  const linkedSet = linkTicketsToPrs(tickets, list.payload, repo)
  skipped.push(...linkedSet.skipped)
  const needsHuman = roleName(labelName, config, 'needsHuman', DEFAULT_PIPELINE_LABELS.needsHuman)
  const out = []
  for (const { ticket, pr } of linkedSet.linked) {
    if (out.length >= limit) break
    const id = String(ticket.identifier).toUpperCase()
    const skip = (reason) => skipped.push({ ticket: id, pr: pr.number, reason })
    const headSha = isSha(pr.headRefOid) ? pr.headRefOid.toLowerCase() : ''
    if (!headSha) {
      skip('unknown:head')
      continue
    }
    const read1 = readStatus(read, headSha, repo, VERIFY_CONTEXT)
    if (read1.verdict === 'unknown') {
      skip('unknown:status')
      continue
    }
    const head = classifyHeadStatus(read1.contexts?.[VERIFY_CONTEXT] ?? null, now)
    if (head.skip) {
      skip(head.skip)
      continue
    }
    const entry = { ticket: id, pr: pr.number, url: pr.url ?? '', headSha, repo, claim: head.claim }
    if (head.parked) {
      const appr = await approval({ pr: pr.number, repo }, { ...deps, read })
      if (appr.reason === 'pr-unreadable' || appr.reason === 'status-unreadable') {
        skip('unknown:approval')
        continue
      }
      if (appr.approval === 'policy-park-approved') {
        entry.approval = appr.approval
        entry.waive = appr.waive
      } else if (appr.approval === 'reverify') {
        entry.approval = appr.approval
      } else {
        skip(head.parked)
        continue
      }
    }
    const labels = Array.isArray(ticket.labels) ? ticket.labels : []
    if (labels.includes(needsHuman) && !entry.approval) {
      skip('needs-human')
      continue
    }
    const view = read(
      'gh',
      ['pr', 'view', String(pr.number), '--repo', repo, '--json', 'headRefOid,statusCheckRollup'],
      { json: true },
    )
    if (!view.ok) {
      skip('unknown:rollup')
      continue
    }
    const ci = classifyHeadCi(view.payload, headSha)
    if (ci) {
      skip(ci)
      continue
    }
    out.push(entry)
  }
  return { candidates: out, skipped }
}

const CANDIDATE_FLAGS = new Set(['ticket', 'limit', 'repo'])

export function parseCandidateFlags(argv) {
  const parsed = parseSelectionFlags(argv)
  if (parsed.error) throw new UsageError(`verify-gate: candidates: ${parsed.error}`)
  const flags = {}
  const rest = parsed.positionals
  for (let i = 0; i < rest.length; i++) {
    const key = rest[i]
    const name = typeof key === 'string' && key.startsWith('--') ? key.slice(2) : ''
    if (!CANDIDATE_FLAGS.has(name))
      throw new UsageError(`verify-gate: candidates: unexpected argument ${JSON.stringify(key)}`)
    const value = rest[i + 1]
    if (typeof value !== 'string' || value.startsWith('--'))
      throw new UsageError(`verify-gate: candidates: --${name} needs a value`)
    flags[name] = value
    i++
  }
  let limit
  if (flags.limit !== undefined) {
    limit = Number(flags.limit)
    if (!Number.isInteger(limit) || limit < 1)
      throw new UsageError('verify-gate: candidates: --limit must be a positive integer')
  }
  return { ticket: flags.ticket, repo: flags.repo, limit, selection: parsed.flags }
}

// ---------------------------------------------------------------------------
// CLI.

const FLAGS = Object.freeze({
  judge: { values: ['pr', 'repo', 'extension-results', 'waive'], bools: [] },
  post: {
    values: ['pr', 'head', 'verdict', 'reason', 'findings', 'token', 'repo'],
    bools: ['dry-run'],
  },
  merge: { values: ['pr', 'head', 'token', 'repo'], bools: ['dry-run'] },
  approval: { values: ['pr', 'repo'], bools: [] },
  rearm: { values: ['pr', 'chat', 'repo'], bools: ['dry-run'] },
})

function parseFlags(verb, argv) {
  const spec = FLAGS[verb]
  const flags = {}
  for (let i = 0; i < argv.length; i++) {
    const key = argv[i]
    if (typeof key !== 'string' || !key.startsWith('--'))
      throw new UsageError(`verify-gate: ${verb}: unexpected argument ${JSON.stringify(key)}`)
    const name = key.slice(2)
    if (spec.bools.includes(name)) {
      flags[name] = true
      continue
    }
    if (!spec.values.includes(name))
      throw new UsageError(`verify-gate: ${verb}: unrecognised flag --${name}`)
    const value = argv[i + 1]
    if (typeof value !== 'string' || value.startsWith('--'))
      throw new UsageError(`verify-gate: ${verb}: --${name} needs a value`)
    flags[name] = value
    i++
  }
  const pr = Number(flags.pr)
  if (!Number.isInteger(pr) || pr < 1)
    throw new UsageError(`verify-gate: ${verb} needs --pr <number>`)
  return { ...flags, pr }
}

export async function main(argv, deps = {}) {
  const [cmd, ...rest] = argv
  if (cmd === 'candidates') return candidates(parseCandidateFlags(rest), deps)
  if (cmd === 'judge') {
    const f = parseFlags('judge', rest)
    return judge(
      { pr: f.pr, repo: f.repo, extensionResults: f['extension-results'], waive: f.waive },
      deps,
    )
  }
  if (cmd === 'post') {
    const f = parseFlags('post', rest)
    return post(
      {
        pr: f.pr,
        head: f.head,
        verdict: f.verdict,
        reason: f.reason,
        findings: f.findings,
        token: f.token,
        repo: f.repo,
        dryRun: f['dry-run'] === true,
      },
      deps,
    )
  }
  if (cmd === 'merge') {
    const f = parseFlags('merge', rest)
    return merge(
      { pr: f.pr, head: f.head, token: f.token, repo: f.repo, dryRun: f['dry-run'] === true },
      deps,
    )
  }
  if (cmd === 'approval') {
    const f = parseFlags('approval', rest)
    return approval({ pr: f.pr, repo: f.repo }, deps)
  }
  if (cmd === 'rearm') {
    const f = parseFlags('rearm', rest)
    return rearm({ pr: f.pr, chat: f.chat, repo: f.repo, dryRun: f['dry-run'] === true }, deps)
  }
  throw new UsageError(
    `verify-gate: unknown command ${cmd ?? '(none)'} (expected candidates|judge|post|merge|approval|rearm)`,
  )
}

if (isMainModule(import.meta.url)) {
  main(process.argv.slice(2)).then(
    (result) => {
      process.stdout.write(`${JSON.stringify(result)}\n`)
    },
    (error) => {
      process.stderr.write(`${error instanceof Error ? error.message : String(error)}\n`)
      process.exitCode = error instanceof UsageError ? 2 : 1
    },
  )
}
