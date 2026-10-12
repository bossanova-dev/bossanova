#!/usr/bin/env node

// verify-gate.mjs — the mechanical heart of the verify stage: judge an open PR's current head,
// record the verdict as a `boss/verify` commit status plus tracker state, merge a passing PR through
// the one head-pinned merge path, detect a human hand-back, and re-arm the next-head callback.
//
// Verbs (each prints one JSON line on stdout):
//   judge    --pr <n> [--repo o/r] [--extension-results <file>] [--waive <code>[,<code>…]]
//                                                                                   never writes
//   post     --pr <n> --head <sha> --verdict claim|wait|pass|defect|human [--reason <code>[,…]]
//            [--findings <file>] [--token <t>] [--repo o/r] [--dry-run]
//   merge    --pr <n> --head <sha> [--token <t>] [--repo o/r] [--dry-run]
//   approval --pr <n> [--repo o/r]                                                   never writes
//   rearm    --pr <n> --chat <id> [--repo o/r] [--dry-run]
//   candidates [--ticket <id>] [--limit N] [--repo o/r] [selection flags]           never writes
//   policy                                                                           never writes
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
import { parseArgs } from 'node:util'

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
import { CHECK_REASONS, CHECK_STATES, epochMs } from './pr-check-state.mjs'
import {
  DESCRIPTION_LIMIT,
  RECEIPT_CONTEXT,
  classifyRead,
  postArgs,
  readArgs,
} from './commit-status.mjs'
import {
  CONFIG_FILENAME,
  DEFAULT_PIPELINE_LABELS,
  DEFAULT_TRACKER_STATES,
  VERIFY_HUMAN_REVIEW_CATEGORIES,
  extensionRootsFor,
  globToRegExp,
  labelName,
  loadSkillConfig,
  stageSelectionQuery,
  stateName,
  unmatchedPolicyGlobs,
  verifyAlwaysHumanPaths,
  verifyHumanReviewPaths,
  verifyPolicyFromConfigText,
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

// The named human-review categories (`verifyDefaults.humanReview.<category>`) and the prefix their
// park codes share. `unproven` is a code with no config category: the evidence step produces it.
export const HUMAN_REVIEW_REASON_PREFIX = 'human-review:'
export const HUMAN_REVIEW_CATEGORIES = VERIFY_HUMAN_REVIEW_CATEGORIES

// The closed vocabulary of park codes. Its order is the report order of a set-valued park.
export const HUMAN_REASONS = Object.freeze([
  'no-receipt',
  'repair-exhausted',
  'always-human-path',
  'ledger-open',
  'extension-failed',
  'no-session',
  ...HUMAN_REVIEW_CATEGORIES.map((category) => `${HUMAN_REVIEW_REASON_PREFIX}${category}`),
  `${HUMAN_REVIEW_REASON_PREFIX}unproven`,
])

/**
 * Parses a park-code set — a comma list (`a,b`) or an array — into HUMAN_REASONS order, deduped.
 * Returns null when the set is empty or names any code outside the closed vocabulary, so a caller
 * never acts on a partially understood set.
 * @returns {string[] | null}
 */
export function parseHumanReasonList(value) {
  const parts = Array.isArray(value) ? value : typeof value === 'string' ? value.split(',') : null
  if (!parts) return null
  const codes = new Set()
  for (const part of parts) {
    const code = typeof part === 'string' ? part.trim() : ''
    if (!HUMAN_REASONS.includes(code)) return null
    codes.add(code)
  }
  if (codes.size === 0) return null
  return HUMAN_REASONS.filter((code) => codes.has(code))
}

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
 * of `{extension, optional, timedOut, crashed?, result}`); `installed` is the applicable extension
 * list. An installed extension with no result entry counts as crashed. Precedence is malfunction →
 * defect → wait → human → pass: any malfunction is a human `extension-failed` verdict (repair cannot
 * fix an extension), including a `human` result whose reason is outside HUMAN_REASONS; otherwise any
 * valid `fail` is a defect; otherwise any valid `wait` waits with the first wait's reason (in result
 * order), so evidence that could still find a defect runs before a person is paged; otherwise any
 * valid `human` parks with its reasons; otherwise pass.
 * `humanReasons` (HUMAN_REASONS order) and `waitReason` (or null) are returned whatever the
 * verdict, so a caller can still wait on or park
 * them when it ignores a malfunction. A required installed extension that malfunctions also adds
 * the `malfunctionReason` its descriptor declares (when that reason is in HUMAN_REASONS), read from
 * `installed` only, never from the results file. Result content is data, never executed or followed.
 */
export function aggregateExtensions(results, installed = []) {
  const declaredReasons = new Map()
  for (const ext of Array.isArray(installed) ? installed : [])
    if (text(ext?.name) && ext.optional !== true && HUMAN_REASONS.includes(ext.malfunctionReason))
      declaredReasons.set(ext.name, ext.malfunctionReason)
  if (!Array.isArray(results))
    return {
      verdict: 'human',
      reason: 'extension-failed',
      outcomes: [],
      failed: ['(results unreadable)'],
      findings: [],
      humanReasons: HUMAN_REASONS.filter((code) => [...declaredReasons.values()].includes(code)),
      waitReason: null,
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
      humanReasons: HUMAN_REASONS,
    }),
  }))
  const failed = outcomes.filter((o) => o.outcome === 'failed').map((o) => o.extension)
  const malfunctionReasons = outcomes
    .filter((o) => o.outcome === 'failed')
    .map((o) => declaredReasons.get(o.extension))
  const humanReasons = HUMAN_REASONS.filter(
    (code) =>
      malfunctionReasons.includes(code) ||
      outcomes.some((o) => o.outcome === 'ok' && o.verdict === 'human' && o.humanReason === code),
  )
  const findings = []
  entries.forEach((entry, index) => {
    if (outcomes[index].outcome === 'ok' && outcomes[index].verdict === 'fail')
      for (const finding of entry.result.findings)
        findings.push({ extension: outcomes[index].extension, ...finding })
  })
  // `waitReason` is returned whatever the verdict, like `humanReasons`, so a caller that ignores a
  // malfunction under a waiver still waits on the extensions that did run.
  const waitReason = outcomes.find((o) => o.outcome === 'ok' && o.verdict === 'wait')?.waitReason
  const out = { outcomes, failed, findings, humanReasons, waitReason: waitReason ?? null }
  if (failed.length > 0) return { verdict: 'human', reason: 'extension-failed', ...out }
  if (findings.length > 0) return { verdict: 'defect', reason: 'findings', ...out }
  if (waitReason) return { verdict: 'wait', reason: waitReason, ...out }
  if (humanReasons.length > 0) return { verdict: 'human', reason: humanReasons.join(','), ...out }
  return { verdict: 'pass', reason: 'verified', ...out }
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
    // A park names a set; the approval waives exactly that set, and only when every code is known.
    const codes = parseHumanReasonList(
      description.slice(VERIFY_DESCRIPTIONS.needsHuman.length).trim(),
    )
    if (via && codes) return { approval: 'policy-park-approved', waive: codes.join(','), via }
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
 * nothing and the next tick re-judges, while a human verdict pages a person. PR-state codes
 * (`no-receipt`, `ledger-open`) park before extensions run; path codes do not, so an applicable
 * extension's defect reaches repair before a person is paged. `f.extensions` is the applicable
 * extension list and `f.extensionsNotApplicable` the names whose check said they do not apply.
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
  // An unresolvable base tip leaves the review policy unread, so it always waits. The base's own CI
  // gates only a repo whose policy sets requireGreenBase: by default a PR merges on its own merits,
  // so a slow or flaky base never holds up a green, judged PR.
  if (f.baseTipUnreadable) return wait('base-ci-unknown')
  if (f.requireGreenBase && f.baseState === 'red') return wait('base-ci-red')
  if (f.requireGreenBase && f.baseState === 'unknown') return wait('base-ci-unknown')
  if (f.receipt === 'unknown') return wait('receipt-unknown')
  if (f.receipt === 'pending') return wait('receipt-pending')
  if (f.changedPaths === null && f.needChangedPaths) return wait('changed-paths-unknown')
  // An unloadable config means the always-human policy is unknown, never empty.
  if (f.configUnreadable) return wait('config-unreadable')

  // A waiver lifts exactly the set of codes the approved park named; every wait reason above has
  // already been checked. A code outside that set still parks.
  const waived = new Set(f.waive ? (parseHumanReasonList(f.waive) ?? []) : [])
  const unwaived = (codes) =>
    HUMAN_REASONS.filter((code) => codes.includes(code) && !waived.has(code))
  // The whole remaining set parks at once, in HUMAN_REASONS order, so one approval covers it.
  const park = (codes, extra = {}) => {
    const reasons = unwaived(codes)
    return { verdict: 'human', reason: reasons.join(','), reasons, ...base, ...extra, notes }
  }
  const stateCodes = []
  if (f.receipt !== 'success') stateCodes.push('no-receipt')
  if (split.ledgerOpen) stateCodes.push('ledger-open')
  const pathCodes = []
  if (Array.isArray(f.alwaysHumanHits) && f.alwaysHumanHits.length > 0)
    pathCodes.push('always-human-path')
  // A policy-surface edit could rewrite any category, so it hits them all.
  for (const category of HUMAN_REVIEW_CATEGORIES)
    if (f.policySurfaceHit === true || f.humanReviewHits?.[category]?.length > 0)
      pathCodes.push(`${HUMAN_REVIEW_REASON_PREFIX}${category}`)
  if (unwaived(stateCodes).length > 0) return park([...stateCodes, ...pathCodes])

  // A run carrying a waiver is the approval path, so its pass is recorded as approved.
  const passReason = waived.size > 0 ? 'approved' : 'verified'
  const settle = (codes, extra = {}) =>
    unwaived(codes).length > 0
      ? park(codes, extra)
      : { verdict: 'pass', reason: passReason, ...base, ...extra, notes }
  if (!Array.isArray(f.extensions) || f.extensions.length === 0) return settle(pathCodes)
  if (f.extensionResults === undefined)
    return { verdict: 'extensions-required', reason: 'extensions-installed', ...base, notes }
  // A result from an extension whose applicability check said it does not apply is not judged.
  const notApplicable = new Set(f.extensionsNotApplicable ?? [])
  const results = Array.isArray(f.extensionResults)
    ? f.extensionResults.filter((entry) => !notApplicable.has(entry?.extension))
    : f.extensionResults
  const agg = aggregateExtensions(results, f.extensions)
  const extensions = { outcomes: agg.outcomes }
  if (agg.failed.length > 0 && !waived.has('extension-failed')) {
    notes.push({
      trigger: 'extension-failed',
      body: `verify extension(s) malfunctioned: ${agg.failed.join(', ')}`,
    })
    return park(['extension-failed', ...pathCodes, ...agg.humanReasons], { extensions })
  }
  // Under an `extension-failed` waiver the malfunctioning results are ignored; findings and human
  // reasons from the extensions that did run still count. Findings beat every park.
  const findings = agg.findings
  if (findings.length > 0) {
    if (f.cleanReview)
      notes.push({
        trigger: 'verify-failed-after-clean-review',
        body: `verify found ${findings.length} finding(s) on a head boss-review called clean`,
      })
    return { verdict: 'defect', reason: 'findings', ...base, findings, extensions, notes }
  }
  // An extension that asked to retry later (a busy shared resource) waits above every park: no
  // approval waives it, and the next tick re-judges.
  if (agg.waitReason) return { ...wait(agg.waitReason), extensions }
  return settle([...pathCodes, ...agg.humanReasons], { extensions })
}

/**
 * Whether `facts` reach the extension stage: no wait, no PR-state park, and at least one extension.
 * Results are excluded from the question, so the answer is the same on a first judge and a
 * re-judge whatever `facts.extensionResults` holds.
 */
export function extensionsConsulted(facts) {
  return decideVerdict({ ...facts, extensionResults: undefined }).verdict === 'extensions-required'
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

// Workflow-run events that test another PR's merge or queue ref rather than the base itself. A
// release PR headed on the base attaches these runs to the base tip's SHA, so they are not the
// base's own CI. Every other event (`push`, `schedule`, `workflow_dispatch`, …) is.
export const FOREIGN_BASE_EVENTS = Object.freeze([
  'pull_request',
  'pull_request_target',
  'pull_request_review',
  'pull_request_review_comment',
  'merge_group',
])

// Flattens a `--paginate --slurp` payload (or a single page) into its `key` entries; `null` when a
// page is malformed, `truncated` when any page's `total_count` exceeds what was read.
function pageEntries(payload, key) {
  const pages = Array.isArray(payload) ? payload : [payload]
  if (pages.some((page) => !page || typeof page !== 'object' || !Array.isArray(page[key])))
    return null
  const entries = pages.flatMap((page) => page[key])
  return { entries, truncated: pages.some((page) => Number(page.total_count) > entries.length) }
}

const timeOf = (value) => epochMs(value) ?? -Infinity
const idOf = (run) => (Number.isFinite(Number(run?.id)) ? Number(run.id) : -Infinity)

// Is `a` newer than `b`? Completed runs compare by `completed_at`, others by `started_at`; the
// larger id breaks a tie.
function newer(a, b, field) {
  const ta = timeOf(a[field])
  const tb = timeOf(b[field])
  return ta !== tb ? ta > tb : idOf(a) > idOf(b)
}

// The base tip's own check runs: GitHub Actions runs whose suite belongs to a foreign (PR-family)
// event are dropped and counted, an Actions run that no workflow run on `sha` attributes is
// ambiguous, and duplicates within one workflow (or app) and check name are decided by the newest
// completed run. Runs from other apps carry no event and are kept.
export function selectBaseOwnCheckRuns({ checkRuns, workflowRuns, sha } = {}) {
  const runs = pageEntries(workflowRuns, 'workflow_runs')
  if (!runs) return { ok: false, reason: 'base-runs-unreadable' }
  if (runs.truncated) return { ok: false, reason: 'base-runs-truncated' }
  const checks = pageEntries(checkRuns, 'check_runs')
  if (!checks) return { ok: false, reason: 'base-check-runs-unreadable' }
  if (checks.truncated) return { ok: false, reason: 'base-check-runs-truncated' }
  const suites = new Map()
  for (const run of runs.entries) {
    if (run?.head_sha && run.head_sha !== sha) continue
    if (run?.check_suite_id === undefined || run.check_suite_id === null) continue
    suites.set(String(run.check_suite_id), {
      event: String(run.event ?? ''),
      // The stable workflow id, not the display name: two workflow files may share a name.
      workflow: String(run.workflow_id ?? run.name ?? ''),
    })
  }
  let foreign = 0
  const groups = new Map()
  for (const check of checks.entries) {
    const app = String(check?.app?.slug ?? '')
    let owner = app
    if (app === 'github-actions') {
      const suiteId = check?.check_suite?.id
      const suite =
        suiteId === undefined || suiteId === null ? undefined : suites.get(String(suiteId))
      if (!suite) return { ok: false, reason: 'base-suite-unattributed' }
      if (FOREIGN_BASE_EVENTS.includes(suite.event)) {
        foreign += 1
        continue
      }
      owner = `workflow:${suite.workflow}`
    }
    const key = `${owner}\u0000${check?.name ?? ''}`
    const group = groups.get(key)
    if (!group) groups.set(key, [check])
    else group.push(check)
  }
  const selected = []
  for (const group of groups.values()) {
    const completed = group.filter((run) => run?.status === 'completed')
    const pool = completed.length > 0 ? completed : group
    const field = completed.length > 0 ? 'completed_at' : 'started_at'
    selected.push(pool.reduce((best, run) => (newer(run, best, field) ? run : best)))
  }
  return { ok: true, checkRuns: selected, foreign }
}

// The base branch tip, classified with `boss/*` excluded and judged from the base's own CI only
// (`selectBaseOwnCheckRuns`). `green`/`pending`/`no gate` proceed; `failing` is red; a failed read,
// an unattributable run or an unreadable/unclassified set is unknown (fail closed).
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
  const workflows = read(
    'gh',
    ['api', `${apiRepo(repo)}/actions/runs?head_sha=${sha}&per_page=100`, '--paginate', '--slurp'],
    { json: true },
  )
  if (!workflows.ok) return { state: 'unknown', sha, check: 'base-runs-unreadable' }
  const own = selectBaseOwnCheckRuns({
    checkRuns: runs.payload,
    workflowRuns: workflows.payload,
    sha,
  })
  if (!own.ok) return { state: 'unknown', sha, check: own.reason }
  const judged = judgeCiOnHead({
    headSHA: sha,
    observedSHA: sha,
    rollup: combined.payload.statuses,
    checkRuns: own.checkRuns,
  })
  const v = judged.verdict
  const foreign = own.foreign
  if (v.state === CHECK_STATES.FAILING) return { state: 'red', sha, check: v.reason, foreign }
  if (
    v.state === CHECK_STATES.UNKNOWN &&
    (v.reason === CHECK_REASONS.UNREADABLE || v.reason === CHECK_REASONS.UNCLASSIFIED)
  )
    return { state: 'unknown', sha, check: v.reason, foreign }
  return { state: 'ok', sha, check: v.reason, foreign }
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

// ---------------------------------------------------------------------------
// The approval anchor: the head a person last approved. Read only when something consumes a range.

const VERIFY_CONTEXT_FIELDS = `context(name: ${JSON.stringify(VERIFY_CONTEXT)}) { state description createdAt }`

// One read: the PR's reviews (every state, so a dismissed review is seen and ignored) and its last
// commits and force-pushes, each with its latest `boss/verify` status, so a head that was
// force-pushed away still anchors.
export const APPROVAL_ANCHOR_QUERY = `query($owner: String!, $name: String!, $number: Int!) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {
      reviews(last: 100) { nodes { state submittedAt authorAssociation commit { oid } } }
      timelineItems(last: 100, itemTypes: [PULL_REQUEST_COMMIT, HEAD_REF_FORCE_PUSHED_EVENT]) {
        nodes {
          ... on PullRequestCommit { commit { oid status { ${VERIFY_CONTEXT_FIELDS} } } }
          ... on HeadRefForcePushedEvent { beforeCommit { oid status { ${VERIFY_CONTEXT_FIELDS} } } }
        }
      }
    }
  }
}`

const plainObject = (value) => value !== null && typeof value === 'object' && !Array.isArray(value)

// Only a review from someone with write access can anchor: an outsider's approval must not narrow
// the judged range, and an outsider's approval on the head must not empty it.
const ANCHOR_REVIEW_ASSOCIATIONS = new Set(['OWNER', 'MEMBER', 'COLLABORATOR'])

/**
 * Classifies the anchor read. Candidates are a commit whose latest `boss/verify` status is a success
 * described exactly `verified (approved)`, and the commit of an `APPROVED` review by an owner,
 * member or collaborator; the latest by
 * timestamp wins. Returns `{sha, via: 'verified-approved'|'review-approved', at}`, or
 * `{sha: null, why: 'anchor-unreadable'|'no-anchor'}`: a malformed payload is unknown, never empty.
 */
export function resolveApprovalAnchor(payload) {
  const unreadable = { sha: null, why: 'anchor-unreadable' }
  if (!plainObject(payload) || payload.errors !== undefined) return unreadable
  const pull = payload.data?.repository?.pullRequest
  if (!plainObject(pull)) return unreadable
  const reviews = pull.reviews?.nodes
  const timeline = pull.timelineItems?.nodes
  if (!Array.isArray(reviews) || !Array.isArray(timeline)) return unreadable
  const candidates = []
  for (const node of timeline) {
    const commit = plainObject(node) ? (node.commit ?? node.beforeCommit) : null
    const ctx = commit?.status?.context
    if (
      isSha(commit?.oid) &&
      plainObject(ctx) &&
      String(ctx.state).toUpperCase() === 'SUCCESS' &&
      ctx.description === VERIFY_DESCRIPTIONS.verifiedApproved
    )
      candidates.push({ sha: commit.oid, via: 'verified-approved', at: ctx.createdAt })
  }
  for (const review of reviews)
    if (
      plainObject(review) &&
      review.state === 'APPROVED' &&
      ANCHOR_REVIEW_ASSOCIATIONS.has(review.authorAssociation) &&
      isSha(review.commit?.oid)
    )
      candidates.push({ sha: review.commit.oid, via: 'review-approved', at: review.submittedAt })
  // A candidate with no readable timestamp cannot be ordered, so it is dropped: fewer anchors only
  // ever widen the judged range.
  let best = null
  for (const c of candidates) {
    const ms = Date.parse(c.at ?? '')
    if (Number.isFinite(ms) && (best === null || ms > best.ms)) best = { ...c, ms }
  }
  if (!best) return { sha: null, why: 'no-anchor' }
  return { sha: best.sha.toLowerCase(), via: best.via, at: best.at }
}

export function readApprovalAnchor(read, repo, pr) {
  const [owner, name] = text(repo) ? repo.split('/') : []
  const r = read(
    'gh',
    [
      'api',
      'graphql',
      '-f',
      `query=${APPROVAL_ANCHOR_QUERY}`,
      ...(owner && name
        ? ['-f', `owner=${owner}`, '-f', `name=${name}`]
        : ['-F', 'owner={owner}', '-F', 'name={repo}']),
      '-F',
      `number=${pr}`,
    ],
    { json: true },
  )
  if (!r.ok) return { sha: null, why: 'anchor-unreadable' }
  return resolveApprovalAnchor(r.payload)
}

// ---------------------------------------------------------------------------
// The PR-level delta: what the PR's own changes gained since the anchor, each side diffed against
// its own merge-base with the base tip, so a rebase never imports base commits.

// The compare API lists at most this many files; a side at the cap may be truncated.
export const COMPARE_FILE_CAP = 300

// A patch with its hunk-header line numbers removed: a rebase that only shifts a hunk is no change.
const normalizePatch = (patch) => patch.replace(/^@@ -\d+(?:,\d+)? \+\d+(?:,\d+)? @@/gm, '@@ @@')

const fileChanged = (a, b) => {
  if (a.status !== b.status || a.previous_filename !== b.previous_filename) return true
  if (typeof a.patch === 'string' && typeof b.patch === 'string')
    return normalizePatch(a.patch) !== normalizePatch(b.patch)
  return !text(a.sha) || !text(b.sha) || a.sha !== b.sha
}

/**
 * The paths whose PR-level change differs between two compare `files` lists (`base...anchor` and
 * `base...head`): on one side only, a different status or rename source, a different patch once
 * hunk headers are normalized, or, without a patch on both sides, a different blob. A renamed entry
 * in the delta also names its old path. Returns a sorted, deduped path list.
 */
export function prLevelDelta(filesAtAnchor, filesAtHead) {
  const byName = (files) =>
    new Map(
      (Array.isArray(files) ? files : [])
        .filter((f) => plainObject(f) && text(f.filename))
        .map((f) => [f.filename, f]),
    )
  const atAnchor = byName(filesAtAnchor)
  const atHead = byName(filesAtHead)
  const delta = new Set()
  const add = (f) => {
    delta.add(f.filename)
    if (text(f.previous_filename)) delta.add(f.previous_filename)
  }
  for (const [name, head] of atHead) {
    const anchor = atAnchor.get(name)
    if (!anchor || fileChanged(anchor, head)) {
      add(head)
      if (anchor) add(anchor)
    }
  }
  for (const [name, anchor] of atAnchor) if (!atHead.has(name)) add(anchor)
  return [...delta].sort()
}

// `{files}` for `base...sha`, or `{files: null, why}` when the read fails, is malformed, or hits the
// file cap (a truncated list cannot prove a path unchanged).
export function readCompareFiles(read, repo, baseSha, sha) {
  const r = read('gh', ['api', `${apiRepo(repo)}/compare/${baseSha}...${sha}`], { json: true })
  if (!r.ok || !Array.isArray(r.payload?.files)) return { files: null, why: 'compare-unreadable' }
  if (r.payload.files.length >= COMPARE_FILE_CAP) return { files: null, why: 'compare-file-cap' }
  return { files: r.payload.files }
}

/**
 * Chooses the range the path codes and the extension envelope are judged on. With an anchor A and
 * both compares readable it is the delta since A (empty when A is the head); otherwise the full
 * `base..head` diff with `why` naming the fallback. `{paths, range: {kind, anchorSha, why?}}`, where
 * `paths` is null when even the full diff is unreadable.
 */
export function selectChangedRange(read, { repo, pr, baseSha, headSha }) {
  const full = (anchorSha, why) => ({
    paths: readChangedPaths(read, pr, repo),
    range: { kind: 'full', anchorSha, why },
  })
  if (!isSha(baseSha)) return full(null, 'no-base-sha')
  const anchor = readApprovalAnchor(read, repo, pr)
  if (!anchor.sha) return full(null, anchor.why)
  const delta = { kind: 'delta', anchorSha: anchor.sha }
  if (anchor.sha === String(headSha).toLowerCase()) return { paths: [], range: delta }
  const atAnchor = readCompareFiles(read, repo, baseSha, anchor.sha)
  if (!atAnchor.files) return full(anchor.sha, atAnchor.why)
  const atHead = readCompareFiles(read, repo, baseSha, headSha)
  if (!atHead.files) return full(anchor.sha, atHead.why)
  return { paths: prLevelDelta(atAnchor.files, atHead.files), range: delta }
}

const EMPTY_POLICY = Object.freeze({
  requireGreenBase: false,
  alwaysHumanPaths: [],
  humanReview: Object.fromEntries(HUMAN_REVIEW_CATEGORIES.map((c) => [c, []])),
})

/**
 * The verify policy as the base tip holds it: the config file's contents at `baseSha`, read through
 * the forge so a PR head can never narrow its own policy. `{state: 'ok'|'absent', policy}` or
 * `{state: 'unreadable'|'invalid', policy: null, why}`; an absent file is an empty policy, and
 * anything else that is not a readable, valid `verifyDefaults` is unknown, never empty.
 */
export function readBasePolicy(read, repo, baseSha) {
  if (!isSha(baseSha)) return { state: 'unreadable', policy: null, why: 'no-base-sha' }
  const r = read('gh', ['api', `${apiRepo(repo)}/contents/${CONFIG_FILENAME}?ref=${baseSha}`], {
    json: true,
  })
  if (!r.ok) {
    const notFound = r.payload?.status === '404' || r.payload?.message === 'Not Found'
    return notFound
      ? { state: 'absent', policy: EMPTY_POLICY }
      : { state: 'unreadable', policy: null, why: r.error ?? 'read-failed' }
  }
  const p = r.payload
  if (p?.type !== 'file' || p?.encoding !== 'base64' || typeof p?.content !== 'string')
    return { state: 'unreadable', policy: null, why: 'not-a-file' }
  try {
    const raw = Buffer.from(p.content, 'base64').toString('utf8')
    return { state: 'ok', policy: verifyPolicyFromConfigText(raw, `${CONFIG_FILENAME}@${baseSha}`) }
  } catch (error) {
    return { state: 'invalid', policy: null, why: error?.message ?? 'invalid' }
  }
}

const policyNonEmpty = (p) =>
  p.alwaysHumanPaths.length > 0 || HUMAN_REVIEW_CATEGORIES.some((c) => p.humanReview[c].length > 0)

const normalizeRoot = (root) => String(root).replace(/^\.\//, '').replace(/\/+$/, '')

/**
 * The changed paths that touch the verify policy surface: the config file at the repo root, any
 * path under an extension root whose directory name starts `boss-verify-` (matched by name, so a
 * deleted extension that discovery can no longer see still counts), and any declared applicability
 * script (`appliesScripts`, repo-relative), so a PR cannot skip a judge by editing its check.
 */
export function policySurfaceHits(
  paths,
  { extensionRoots = extensionRootsFor(null), appliesScripts = [] } = {},
) {
  const prefixes = extensionRoots.map((root) => `${normalizeRoot(root)}/boss-verify-`)
  const scripts = new Set(appliesScripts.filter(text).map(normalizeRoot))
  return (Array.isArray(paths) ? paths : []).filter(
    (path) =>
      path === CONFIG_FILENAME ||
      scripts.has(normalizeRoot(path)) ||
      prefixes.some((prefix) => path.startsWith(prefix) && path.indexOf('/', prefix.length) > 0),
  )
}

// The applicability-check exit code that means "this extension does not apply". Every other
// outcome — exit 0, exit 1 (also Node's uncaught-exception code), a crash, a timeout, a missing
// script — applies: a check can only add work, never remove a park.
export const APPLIES_NOT_APPLICABLE_EXIT = 10

/**
 * Splits discovered verify extensions into `{applicable, notApplicable}` by running each declared
 * `applies` script with node from the repo root, the would-be extension envelope on stdin. An
 * extension with no `applies` always applies.
 */
export function partitionApplicable(extensions, context, { read, nodeBin = process.execPath }) {
  const applicable = []
  const notApplicable = []
  for (const ext of extensions) {
    const skip =
      text(ext?.applies) &&
      read(nodeBin, [ext.applies], { input: JSON.stringify(context) }).exit ===
        APPLIES_NOT_APPLICABLE_EXIT
    ;(skip ? notApplicable : applicable).push(ext)
  }
  return { applicable, notApplicable }
}

const globHits = (paths, globs) => {
  const res = globs.map((glob) => globToRegExp(glob))
  return paths.filter((path) => res.some((re) => re.test(path)))
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
  const waive = options.waive === undefined ? null : parseHumanReasonList(options.waive)
  if (options.waive !== undefined && !waive)
    throw new UsageError(`verify-gate: --waive must be a comma list of ${HUMAN_REASONS.join(', ')}`)
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
    waive,
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
  // The rest of the config (extension roots) comes from the working tree; the policy itself comes
  // from the base tip.
  const loaded = loadConfigResult(deps)
  const basePolicy = baseState.sha ? readBasePolicy(read, repo, baseState.sha) : null
  const verifyPolicy = basePolicy?.policy ?? EMPTY_POLICY
  facts.baseTipUnreadable = !baseState.sha
  facts.requireGreenBase = verifyPolicy.requireGreenBase === true
  facts.configUnreadable =
    loaded.error !== undefined || (basePolicy !== null && basePolicy.policy === null)
  const humanPaths = verifyPolicy.alwaysHumanPaths
  const discovered = (deps.discover ?? discoverExtensions)({
    core: VERIFY_CORE,
    role: 'verify',
    root: deps.cwd ?? process.cwd(),
  })
  const installed = discovered?.extensions ?? []
  facts.extensions = installed
  const appliesScripts = installed.map((ext) => ext?.applies).filter(text)
  // The diff is read only when something consumes it: a non-empty policy list, an applicability
  // check (recomputed on every judge, re-judges included), or the extension envelope.
  const policyConfigured = policyNonEmpty(verifyPolicy)
  facts.needChangedPaths =
    policyConfigured ||
    appliesScripts.length > 0 ||
    (installed.length > 0 && options.extensionResults === undefined)
  // After an approval the range is the PR's own delta since the anchor; see selectChangedRange.
  const selected = facts.needChangedPaths
    ? selectChangedRange(read, { repo, pr, baseSha: baseState.sha, headSha })
    : { paths: [], range: null }
  const changedPaths = selected.paths
  facts.changedPaths = changedPaths
  const paths = Array.isArray(changedPaths) ? changedPaths : []
  facts.alwaysHumanHits = globHits(paths, humanPaths)
  facts.humanReviewHits = {}
  for (const category of HUMAN_REVIEW_CATEGORIES) {
    const hits = globHits(paths, verifyPolicy.humanReview[category])
    if (hits.length > 0) facts.humanReviewHits[category] = hits
  }
  // Touching the policy surface costs nothing while the base policy is empty.
  facts.policySurfaceHit =
    policyConfigured &&
    policySurfaceHits(paths, {
      extensionRoots: extensionRootsFor(loaded.config),
      appliesScripts,
    }).length > 0
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
    ...(selected.range
      ? { range: { kind: selected.range.kind, anchorSha: selected.range.anchorSha } }
      : {}),
  }
  // Applicability is checked only where extensions would be consulted: a head that waits or parks
  // on a PR-state code never runs a check.
  let notApplicable = []
  if (extensionsConsulted(facts)) {
    const split = partitionApplicable(installed, context, { read, nodeBin: deps.nodeBin })
    facts.extensions = split.applicable
    notApplicable = split.notApplicable
  }
  facts.extensionsNotApplicable = notApplicable.map((ext) => ext.name)
  if (options.extensionResults !== undefined) {
    const parsed = readJsonFile(options.extensionResults)
    facts.extensionResults = Array.isArray(parsed) ? parsed : null
  }
  const verdict = decideVerdict(facts)
  const out = {
    ...verdict,
    facts: factsOut({
      base: {
        sha: baseState.sha,
        state: baseState.state,
        check: baseState.check ?? '',
        foreign: baseState.foreign ?? 0,
      },
      receipt: { verdict: receipt.verdict, state: receipt.state ?? '' },
      alwaysHumanHits: facts.alwaysHumanHits,
      humanReviewHits: facts.humanReviewHits,
      policySurfaceHit: facts.policySurfaceHit,
      range: selected.range,
      extensionsInstalled: installed.map((ext) => ext.name),
      extensionsApplicable: facts.extensions.map((ext) => ext.name),
      extensionsNotApplicable: facts.extensionsNotApplicable,
    }),
  }
  if (verdict.verdict === 'extensions-required') {
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
  const humanCodes = verdict === 'human' ? parseHumanReasonList(options.reason) : null
  if (verdict === 'human' && !humanCodes)
    throw new UsageError(
      `verify-gate: post --verdict human needs --reason <code>[,<code>…] from ${HUMAN_REASONS.join('|')}`,
    )
  // A truncated park description no longer names its set, so approval could never match it.
  if (
    humanCodes &&
    Array.from(needsHumanDescription(humanCodes.join(','))).length > DESCRIPTION_LIMIT
  )
    throw new UsageError(
      `verify-gate: post --reason set is too long for a ${DESCRIPTION_LIMIT}-char commit status`,
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
  // A human set is posted in its normalised order, so a re-post of the same set is a no-op.
  let reason = humanCodes ? humanCodes.join(',') : options.reason
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
  if (effective === 'human' && reason.split(',').includes('extension-failed'))
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

/**
 * The local verify policy plus a warning for every glob that matches no tracked file. Never writes.
 * An invalid config, or an unreadable file list, throws (non-zero exit); an unmatched glob is only
 * ever a warning.
 */
export async function policy(options = {}, deps = {}) {
  const read = deps.read ?? commandReader(deps)
  const loaded = loadConfigResult(deps)
  if (loaded.error) throw new Error(`verify-gate policy: config: ${loaded.error.message}`)
  const current = {
    alwaysHumanPaths: verifyAlwaysHumanPaths(loaded.config),
    humanReview: verifyHumanReviewPaths(loaded.config),
  }
  const files = read('git', ['ls-files', '-z'])
  if (!files.ok) throw new Error(`verify-gate policy: git ls-files failed (${files.error})`)
  const tracked = files.output.split('\0').filter(Boolean)
  return { policy: current, warnings: unmatchedPolicyGlobs(current, tracked) }
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
  if (cmd === 'policy') {
    try {
      parseArgs({ args: rest, options: {}, strict: true, allowPositionals: false })
    } catch (error) {
      throw new UsageError(`verify-gate: policy: ${error.message}`)
    }
    return policy({}, deps)
  }
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
    `verify-gate: unknown command ${cmd ?? '(none)'} (expected candidates|judge|post|merge|approval|rearm|policy)`,
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
