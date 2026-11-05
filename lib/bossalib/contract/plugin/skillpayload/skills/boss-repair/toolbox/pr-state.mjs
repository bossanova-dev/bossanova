#!/usr/bin/env node

// pr-state.mjs — the daemon-first PR state reader the CI waits and check classification share.
//
// A PR read goes to the local daemon first: `boss pr state <pr> --json` is served from the daemon's
// webhook-invalidated read cache, and `boss pr wait <pr> --until checks-settled` blocks until a
// webhook says something changed. Raw `gh` is read only when no daemon answers — no `boss` binary,
// a CLI too old to print JSON, or a daemon answering UNAVAILABLE, UNIMPLEMENTED or NOT_FOUND — and
// every such spawn carries a `fallback:` usage label, so `boss github-usage` shows it apart from the
// daemon-served reads. Any other daemon failure (a rate limit, a refused argument, a timeout) is an
// unreadable reading, never a reason to spend GitHub quota from here instead.
//
// The daemon reads a PR's status, checks and runs separately, so an envelope whose workflow runs
// name another head is re-read once with `--fresh`, and is unreadable if it still disagrees.
//
//   node pr-state.mjs read --pr <n> [--repo <owner/name>] [--fresh] [--timeout-ms <ms>]
//
// prints `{ source, fallbackReason?, state }` (exit 0), or `{ source, fallbackReason?, error }`
// (exit 1). On the fallback path `state` has the daemon envelope's shape, with the fields only the
// daemon computes (`check_verdict`, `verify_phase`, `has_build_receipt`, `observed_at`) null.
//
//   node pr-state.mjs view [--pr <n>] [--repo <owner/name>] [--fresh] [--field <name>]
//
// prints a `gh pr view --json` shaped object (`number`, `state`, `isDraft`, `headRefOid`,
// `headRefName`, `baseRefName`, `mergeable`, `mergeStateStatus`, `reviewDecision`,
// `statusCheckRollup`) plus `source`; with no --pr, the session's PR (`session-pr`).
//
//   node pr-state.mjs session-pr [--field <name>]
//
// prints `{ number, url, source }` for this session's PR from the daemon's session record, else
// from a labelled `gh pr list --head <branch>`; `number` is null when the branch has no open PR.
// `--field` prints one field's raw value instead (nothing on failure, the JSON on stderr).
//
// Project-agnostic: `boss`, `gh`, Node builtins and sibling toolbox modules only.

import { spawnSync } from 'node:child_process'
import { parseArgs } from 'node:util'

import { resolveBossBinary } from './boss-binary.mjs'
import {
  headShaMismatch,
  rollupFromEnvelope,
  workflowRunsFromEnvelope,
} from './ci/pr-state-envelope.mjs'
import { readArgs as statusReadArgs } from './commit-status.mjs'
import { isMainModule } from './main-module.mjs'
import { WORKFLOW_RUNS_LIMIT } from './pr-check-state.mjs'

export const FALLBACK_LABEL_PREFIX = 'fallback:'
export const FALLBACK_LABELS = Object.freeze({
  CI_WAIT: `${FALLBACK_LABEL_PREFIX}ci-wait`,
  PR_STATE: `${FALLBACK_LABEL_PREFIX}pr-state`,
})
// The environment variable the bossd `gh` wrapper reads its usage label from.
export const GH_USAGE_LABEL_ENV = 'BOSS_GH_USAGE_LABEL'
// `boss pr wait --timeout` ceiling; a longer wait is clamped to it.
export const PR_WAIT_CAP_MS = 8 * 60 * 1000
export const DEFAULT_READ_TIMEOUT_MS = 60 * 1000
// The spawn outlives the wait's own --timeout by this much, so the wait answers rather than dies.
const WAIT_SPAWN_HEADROOM_MS = 5000
const MIN_SPAWN_MS = 1000
const MAX_BUFFER = 64 * 1024 * 1024
const NO_DAEMON_CODES = new Set(['UNAVAILABLE', 'UNIMPLEMENTED', 'NOT_FOUND'])
// cobra's refusal of a verb or flag this CLI predates: the CLI is too old, not the request wrong.
const TOO_OLD_MESSAGE = /^unknown (command|flag|shorthand flag)\b/i
const RUN_LIST_FIELDS = 'name,workflowName,status,conclusion,headSha,event'
const VIEW_FIELDS =
  'headRefOid,headRefName,baseRefName,state,isDraft,mergeable,mergeStateStatus,reviewDecision,statusCheckRollup'

const upper = (value) => (typeof value === 'string' ? value.trim().toUpperCase() : '')
const lower = (value) => upper(value).toLowerCase()
const isObject = (value) => value !== null && typeof value === 'object' && !Array.isArray(value)

function io(deps = {}) {
  const env = deps.env ?? process.env
  const cwd = deps.cwd
  return {
    spawn: deps.spawn ?? spawnSync,
    env,
    cwd: cwd ? { cwd } : {},
    now: deps.now ?? Date.now,
    resolveBoss: deps.resolveBoss ?? (() => resolveBossBinary(env, cwd ? { cwd } : {})),
  }
}

// bossCall runs the resolved `boss` binary and returns exactly one of `{kind:'ok', body}`,
// `{kind:'fallback', reason}` (no daemon answered) or `{kind:'error', error, code}`. A help screen
// can exit 0, so success is a JSON object on stdout that is not a failure envelope, never the exit
// status alone.
export function bossCall({ argv, timeoutMs, deps }) {
  const { spawn, env, cwd, resolveBoss } = io(deps)
  const binary = resolveBoss(env)
  if (!binary?.ok || typeof binary.path !== 'string') {
    return { kind: 'fallback', reason: 'no-boss-binary' }
  }
  const res = spawn(binary.path, argv, {
    ...cwd,
    encoding: 'utf8',
    timeout: Math.max(MIN_SPAWN_MS, Math.floor(timeoutMs)),
    killSignal: 'SIGKILL',
    maxBuffer: MAX_BUFFER,
    env,
  })
  return classifyBossResponse(res)
}

export function classifyBossResponse(res) {
  if (res?.error) {
    const code = res.error.code ?? res.error.message
    if (code === 'ENOENT' || code === 'EACCES')
      return { kind: 'fallback', reason: 'no-boss-binary' }
    if (code === 'ETIMEDOUT')
      return { kind: 'error', error: 'boss timed out', code: 'SPAWN_TIMEOUT' }
    return { kind: 'error', error: `boss did not run: ${code}`, code: 'SPAWN_FAILED' }
  }
  let body
  try {
    body = JSON.parse(String(res?.stdout ?? ''))
  } catch {
    return { kind: 'fallback', reason: 'boss-too-old' }
  }
  if (!isObject(body)) return { kind: 'fallback', reason: 'boss-too-old' }
  if (isObject(body.error)) {
    const code = upper(body.error.code)
    const connect = upper(body.error.connect_code)
    const message = typeof body.error.message === 'string' ? body.error.message : ''
    const noDaemon = [code, connect].find((c) => NO_DAEMON_CODES.has(c))
    if (noDaemon) return { kind: 'fallback', reason: `daemon-${noDaemon.toLowerCase()}` }
    if (TOO_OLD_MESSAGE.test(message)) return { kind: 'fallback', reason: 'boss-too-old' }
    return {
      kind: 'error',
      error: `${code || connect || 'UNKNOWN'}: ${message}`,
      code: code || connect,
    }
  }
  if (res.status !== 0) return { kind: 'error', error: `boss exited ${res.status}`, code: 'EXIT' }
  return { kind: 'ok', body }
}

export function readPRState({
  pr,
  repo = '',
  fresh = false,
  withWorkflowRuns = true,
  timeoutMs,
  deps,
}) {
  const argv = ['pr', 'state', String(pr), ...(withWorkflowRuns ? ['--with-workflow-runs'] : [])]
  argv.push('--json')
  if (repo) argv.push('--repo', repo)
  if (fresh) argv.push('--fresh')
  return bossCall({ argv, timeoutMs, deps })
}

// readConsistentPRState is readPRState plus the one-head check: runs naming another head are
// re-read once with `--fresh`, within the same timeout, and an envelope that still disagrees is an
// error.
export function readConsistentPRState({ pr, repo = '', fresh = false, timeoutMs, deps }) {
  const { now } = io(deps)
  const started = now()
  const first = readPRState({ pr, repo, fresh, timeoutMs, deps })
  if (first.kind !== 'ok' || !headShaMismatch(first.body)) return first
  const left = timeoutMs - (now() - started)
  const mismatch = { kind: 'error', error: 'daemon read named two heads', code: 'HEAD_MISMATCH' }
  if (left < MIN_SPAWN_MS) return mismatch
  const second = readPRState({ pr, repo, fresh: true, timeoutMs: left, deps })
  return second.kind === 'ok' && headShaMismatch(second.body) ? mismatch : second
}

// waitPRState blocks on `boss pr wait` for at most `timeoutMs` (clamped to the wait's cap, in whole
// seconds). The spawn gets a few seconds of headroom over the wait's own timeout.
export function waitPRState({ pr, repo = '', until, timeoutMs, deps }) {
  const seconds = Math.max(1, Math.floor(Math.min(timeoutMs, PR_WAIT_CAP_MS) / 1000))
  const argv = ['pr', 'wait', String(pr), '--until', until, '--timeout', `${seconds}s`, '--json']
  if (repo) argv.push('--repo', repo)
  return bossCall({ argv, timeoutMs: seconds * 1000 + WAIT_SPAWN_HEADROOM_MS, deps })
}

// A wait that answered is a real delay: any outcome it printed, or its own deadline.
export function waitAnswered(result) {
  return result?.kind === 'ok' || result?.code === 'DEADLINE_EXCEEDED'
}

// ghSpawn runs `gh` with the given usage label and the rest of the environment unchanged.
export function ghSpawn(args, { label, timeoutMs, deps }) {
  const { spawn, env, cwd } = io(deps)
  return spawn('gh', args, {
    ...cwd,
    encoding: 'utf8',
    timeout: Math.max(1, Math.floor(timeoutMs)),
    killSignal: 'SIGKILL',
    maxBuffer: MAX_BUFFER,
    env: { ...env, [GH_USAGE_LABEL_ENV]: label },
  })
}

function ghArgs({ what, pr, headSha }) {
  if (what === 'view')
    return ['pr', 'view', String(pr), '--json', 'headRefOid,statusCheckRollup,baseRefName']
  if (what === 'verify-status') return statusReadArgs({ sha: headSha, repo: '' })
  if (what === 'runs') {
    return [
      'run',
      'list',
      '--commit',
      headSha,
      '--json',
      RUN_LIST_FIELDS,
      '--limit',
      String(WORKFLOW_RUNS_LIMIT),
    ]
  }
  return ['pr', 'checks', String(pr), '--json', 'name,state,bucket']
}

// ghCheckReader is ci-wait's raw-gh reader: `read({what, pr, headSha?, timeoutMs})` →
// `{ok, headSha?, payload?, error?}`, every spawn labelled.
/** @param {{label?: string, deps?: object}} [options] */
export function ghCheckReader({ label = FALLBACK_LABELS.CI_WAIT, deps } = {}) {
  return async ({ what, pr, headSha = '', timeoutMs }) => {
    if ((what === 'runs' || what === 'verify-status') && headSha === '')
      return { ok: false, error: 'no head SHA' }
    let args
    try {
      args = ghArgs({ what, pr, headSha })
    } catch (error) {
      // statusReadArgs refuses a SHA that is not 40 hex; gh is never spawned for it.
      return { ok: false, error: error?.message ?? String(error) }
    }
    const res = ghSpawn(args, { label, timeoutMs, deps })
    // `gh pr checks` exits 8 while checks are pending and still prints the payload.
    if (res.error || (res.status !== 0 && !(what === 'checks' && res.status === 8))) {
      return { ok: false, error: res.error?.code ?? `gh exited ${res.status}` }
    }
    try {
      const payload = JSON.parse(res.stdout)
      return what === 'view'
        ? { ok: true, headSha: payload?.headRefOid ?? '', payload }
        : { ok: true, payload }
    } catch {
      return { ok: false, error: 'unparseable gh output' }
    }
  }
}

function viewOf(envelope, fresh) {
  return {
    ok: true,
    fresh,
    headSha: typeof envelope.head_sha === 'string' ? envelope.head_sha : '',
    payload: {
      headRefOid: envelope.head_sha ?? '',
      baseRefName: envelope.base_branch ?? '',
      statusCheckRollup: rollupFromEnvelope(envelope),
      verifyPhase: envelope.verify_phase?.kind || 'none',
    },
  }
}

// daemonFirstReader serves ci-wait's `read` contract from `boss pr state`. A view read with `fresh`
// bypasses the daemon's read cache and says so (`fresh: true`). The view read caches the
// envelope for the rest of the reading; `runs` and `checks` are answered from it with no spawn, and
// `verify-status` is not read (the envelope's verify phase stands in). The first fallback switches
// this reader to labelled `gh` for the rest of the process.
/** @param {{label?: string, deps?: object}} [options] */
export function daemonFirstReader({ label = FALLBACK_LABELS.CI_WAIT, deps } = {}) {
  let gh = null
  let source = null
  let reason = null
  let envelope = null
  const read = async (req) => {
    if (gh !== null) return gh(req)
    if (req.what === 'view') {
      envelope = null
      const fresh = req.fresh === true
      const res = readConsistentPRState({ pr: req.pr, fresh, timeoutMs: req.timeoutMs, deps })
      if (res.kind === 'fallback') {
        gh = ghCheckReader({ label, deps })
        source = 'gh-fallback'
        reason = res.reason
        return gh(req)
      }
      source = 'daemon'
      if (res.kind === 'error') return { ok: false, error: `boss pr state: ${res.error}` }
      envelope = res.body
      return viewOf(envelope, fresh)
    }
    if (envelope === null) return { ok: false, error: 'no daemon PR state read' }
    if (req.what === 'runs') {
      if (req.headSha && req.headSha !== envelope.head_sha)
        return { ok: false, error: 'head moved' }
      const { runs, error } = workflowRunsFromEnvelope(envelope)
      return runs === null ? { ok: false, error } : { ok: true, payload: runs }
    }
    if (req.what === 'checks') return { ok: true, payload: rollupFromEnvelope(envelope) }
    return { ok: false, error: 'verify statuses are not read on the daemon path' }
  }
  read.source = () => source
  read.fallbackReason = () => reason
  return read
}

const MERGEABLE = Object.freeze({ MERGEABLE: true, CONFLICTING: false })
const REVIEW_DECISIONS = Object.freeze({
  APPROVED: 'approved',
  CHANGES_REQUESTED: 'changes_requested',
  REVIEW_REQUIRED: 'required',
})
const PENDING_STATES = new Set(['PENDING', 'EXPECTED', 'QUEUED', 'WAITING', 'REQUESTED'])

// envelopeCheck maps a rollup node to an envelope check that maps back to the same classification:
// a check run keeps its status and conclusion, a commit status's state becomes one of them.
function envelopeCheck(node) {
  const status = upper(node?.status)
  const state = upper(node?.conclusion ?? node?.state)
  const pending = status === '' ? PENDING_STATES.has(state) : status !== 'COMPLETED'
  const statusName = pending ? 'queued' : 'completed'
  return {
    name: String(node?.name || node?.context || node?.workflowName || '').trim(),
    status: status === '' ? statusName : status.toLowerCase(),
    conclusion: pending ? '' : state.toLowerCase(),
    description: typeof node?.description === 'string' ? node.description : '',
    is_unclassified: !pending && state === '',
    workflow_name: String(node?.workflowName || node?.workflow || '').trim(),
  }
}

// ghPrState builds the daemon envelope's shape from one labelled `gh pr view` and one labelled
// `gh run list`; either failing is an error, never a partial state.
export function ghPrState({ pr, repo = '', timeoutMs, label = FALLBACK_LABELS.PR_STATE, deps }) {
  const repoArgs = repo ? ['--repo', repo] : []
  const json = (args) => {
    const res = ghSpawn([...args, ...repoArgs], { label, timeoutMs, deps })
    if (res.error || res.status !== 0)
      return { error: res.error?.code ?? `gh exited ${res.status}` }
    try {
      return { value: JSON.parse(res.stdout) }
    } catch {
      return { error: 'unparseable gh output' }
    }
  }
  const view = json(['pr', 'view', String(pr), '--json', VIEW_FIELDS])
  if (view.error) return { ok: false, error: `gh pr view: ${view.error}` }
  const v = view.value ?? {}
  const head = typeof v.headRefOid === 'string' ? v.headRefOid : ''
  if (head === '') return { ok: false, error: 'gh pr view: no head SHA' }
  const runList = ['run', 'list', '--commit', head, '--json', RUN_LIST_FIELDS]
  const runs = json([...runList, '--limit', String(WORKFLOW_RUNS_LIMIT)])
  if (runs.error) return { ok: false, error: `gh run list: ${runs.error}` }
  const rollup = Array.isArray(v.statusCheckRollup) ? v.statusCheckRollup : []
  const state = {
    repo: repo.toLowerCase(),
    pr_number: Number(pr),
    state: lower(v.state),
    is_draft: v.isDraft === true,
    head_sha: head,
    head_branch: v.headRefName ?? '',
    base_branch: v.baseRefName ?? '',
    is_mergeable: MERGEABLE[upper(v.mergeable)] ?? null,
    merge_state_status: lower(v.mergeStateStatus),
    review_decision: REVIEW_DECISIONS[upper(v.reviewDecision)] ?? lower(v.reviewDecision),
    latest_review_state: null,
    checks: rollup.map(envelopeCheck).filter((check) => check.name !== ''),
    check_verdict: null,
    verify_phase: null,
    has_build_receipt: null,
    has_settled_workflow_runs: null,
    held_workflow_runs: [],
    review_comments: [],
    observed_at: null,
    workflow_runs: (Array.isArray(runs.value) ? runs.value : []).map((run) => ({
      name: run?.workflowName || run?.name || '',
      status: lower(run?.status),
      conclusion: lower(run?.conclusion),
      event: run?.event ?? '',
      head_sha: run?.headSha ?? '',
    })),
    is_workflow_runs_read: true,
  }
  return { ok: true, state }
}

// readCommand is the `read` verb: the daemon's envelope, or the labelled gh one when no daemon
// answered. `exit` is 0 on a reading and 1 on an error.
export function readCommand({
  pr,
  repo = '',
  fresh = false,
  timeoutMs = DEFAULT_READ_TIMEOUT_MS,
  deps,
}) {
  const res = fresh
    ? readPRState({ pr, repo, fresh, timeoutMs, deps })
    : readConsistentPRState({ pr, repo, timeoutMs, deps })
  const failed = (out, code, message) => ({ exit: 1, out: { ...out, error: { code, message } } })
  if (res.kind === 'ok') {
    if (headShaMismatch(res.body))
      return failed({ source: 'daemon' }, 'HEAD_MISMATCH', 'daemon read named two heads')
    return { exit: 0, out: { source: 'daemon', state: res.body } }
  }
  if (res.kind === 'error') return failed({ source: 'daemon' }, res.code || 'UNKNOWN', res.error)
  const out = { source: 'gh-fallback', fallbackReason: res.reason }
  const gh = ghPrState({ pr, repo, timeoutMs, deps })
  return gh.ok ? { exit: 0, out: { ...out, state: gh.state } } : failed(out, 'UNREADABLE', gh.error)
}

const GH_VIEW_FIELDS = `number,${VIEW_FIELDS}`
const MERGEABLE_NAMES = Object.freeze({ true: 'MERGEABLE', false: 'CONFLICTING' })
const REVIEW_DECISION_NAMES = Object.freeze(
  Object.fromEntries(Object.entries(REVIEW_DECISIONS).map(([name, value]) => [value, name])),
)
const failedRead = (out, code, message) => ({ exit: 1, out: { ...out, error: { code, message } } })
const text = (value) => (typeof value === 'string' ? value : '')

// viewFromEnvelope maps a `boss pr state` envelope to the `gh pr view --json` fields it covers, so
// a gh-shaped consumer parses both paths alike. Nothing the envelope lacks is invented: no
// `mergedAt`, no title or URL; an unset mergeability or merge state is gh's own `UNKNOWN`.
export function viewFromEnvelope(envelope, pr) {
  return {
    number: Number.isInteger(envelope?.pr_number) ? envelope.pr_number : Number(pr),
    state: upper(envelope?.state),
    isDraft: envelope?.is_draft === true,
    headRefOid: text(envelope?.head_sha),
    headRefName: text(envelope?.head_branch),
    baseRefName: text(envelope?.base_branch),
    mergeable: MERGEABLE_NAMES[String(envelope?.is_mergeable)] ?? 'UNKNOWN',
    mergeStateStatus: upper(envelope?.merge_state_status) || 'UNKNOWN',
    reviewDecision:
      REVIEW_DECISION_NAMES[lower(envelope?.review_decision)] ?? upper(envelope?.review_decision),
    statusCheckRollup: rollupFromEnvelope(envelope),
  }
}

function ghJSON(args, { timeoutMs, deps }) {
  const res = ghSpawn(args, { label: FALLBACK_LABELS.PR_STATE, timeoutMs, deps })
  if (res.error || res.status !== 0) return { error: res.error?.code ?? `gh exited ${res.status}` }
  try {
    return { value: JSON.parse(res.stdout) }
  } catch {
    return { error: 'unparseable gh output' }
  }
}

// readPRView is the `view` verb for one PR: the daemon's envelope mapped by viewFromEnvelope, or a
// labelled `gh pr view` when no daemon answered. Any other daemon failure, or an envelope with no
// head SHA, is a failed read (exit 1) and spawns no gh.
export function readPRView({
  pr,
  repo = '',
  fresh = false,
  timeoutMs = DEFAULT_READ_TIMEOUT_MS,
  deps,
}) {
  const res = readPRState({ pr, repo, fresh, withWorkflowRuns: false, timeoutMs, deps })
  if (res.kind === 'error')
    return failedRead({ source: 'daemon' }, res.code || 'UNKNOWN', res.error)
  if (res.kind === 'ok') {
    if (text(res.body.head_sha) === '')
      return failedRead({ source: 'daemon' }, 'MALFORMED', 'daemon read carried no head SHA')
    return { exit: 0, out: { ...viewFromEnvelope(res.body, pr), source: 'daemon' } }
  }
  const out = { source: 'gh-fallback', fallbackReason: res.reason }
  const repoArgs = repo ? ['--repo', repo] : []
  const gh = ghJSON(['pr', 'view', String(pr), '--json', GH_VIEW_FIELDS, ...repoArgs], {
    timeoutMs,
    deps,
  })
  if (gh.error) return failedRead(out, 'UNREADABLE', `gh pr view: ${gh.error}`)
  if (!isObject(gh.value)) return failedRead(out, 'UNREADABLE', 'gh pr view: not an object')
  return { exit: 0, out: { ...gh.value, ...out } }
}

// ghSessionPR answers `session-pr` from the open PR whose head is the current branch.
function ghSessionPR({ reason, timeoutMs, deps }) {
  const { spawn, env, cwd } = io(deps)
  const out = { number: null, url: null, source: 'gh-fallback', fallbackReason: reason }
  const branchRead = spawn('git', ['branch', '--show-current'], {
    ...cwd,
    encoding: 'utf8',
    timeout: Math.max(MIN_SPAWN_MS, Math.floor(timeoutMs)),
    env,
  })
  const branch = branchRead?.status === 0 ? String(branchRead.stdout ?? '').trim() : ''
  if (branch === '') return { exit: 0, out }
  const list = ['pr', 'list', '--head', branch, '--state', 'open', '--json', 'number,url']
  const gh = ghJSON(list, { timeoutMs, deps })
  if (gh.error) return failedRead(out, 'UNREADABLE', `gh pr list: ${gh.error}`)
  const [first] = Array.isArray(gh.value) ? gh.value : []
  if (!Number.isInteger(first?.number)) return { exit: 0, out }
  return { exit: 0, out: { ...out, number: first.number, url: text(first.url) || null } }
}

// readSessionPR is the `session-pr` verb: the PR the daemon recorded for this session
// (`boss env --json`, then `boss show <id> --json`), which costs no GitHub call. It reads labelled
// gh when no daemon answered, there is no session, or the session has no PR recorded yet.
/** @param {{timeoutMs?: number, deps?: object}} [options] */
export function readSessionPR({ timeoutMs = DEFAULT_READ_TIMEOUT_MS, deps } = {}) {
  const viaGh = (reason) => ghSessionPR({ reason, timeoutMs, deps })
  const daemonError = (res) => failedRead({ source: 'daemon' }, res.code || 'UNKNOWN', res.error)
  const env = bossCall({ argv: ['env', '--json'], timeoutMs, deps })
  if (env.kind === 'fallback') return viaGh(env.reason)
  if (env.kind === 'error') return daemonError(env)
  const sessionId = text(env.body.session?.session_id).trim()
  if (sessionId === '') return viaGh('no-session')
  const show = bossCall({ argv: ['show', sessionId, '--json'], timeoutMs, deps })
  if (show.kind === 'fallback') return viaGh(show.reason)
  if (show.kind === 'error') return daemonError(show)
  const session = isObject(show.body.session) ? show.body.session : {}
  if (!Number.isInteger(session.pr_number) || session.pr_number < 1) return viaGh('no-session-pr')
  const url = text(session.pr_url) || null
  return { exit: 0, out: { number: session.pr_number, url, source: 'daemon' } }
}

// viewCommand is `view`: the named PR, else the session's PR; no PR at all is a failed read.
/** @param {{pr?: number, repo?: string, fresh?: boolean, timeoutMs?: number, deps?: object}} [options] */
export function viewCommand({ pr, repo = '', fresh = false, timeoutMs, deps } = {}) {
  if (pr !== undefined) return readPRView({ pr, repo, fresh, timeoutMs, deps })
  const session = readSessionPR({ timeoutMs, deps })
  if (session.exit !== 0) return session
  if (session.out.number === null) {
    const { source, fallbackReason } = session.out
    return failedRead({ source, fallbackReason }, 'NO_PR', 'no open PR for this session')
  }
  return readPRView({ pr: session.out.number, repo, fresh, timeoutMs, deps })
}

// fieldText renders one field the way `gh … -q .<field>` does: a string raw, null or absent as
// nothing, anything else as JSON.
export function fieldText(out, field) {
  const value = out?.[field]
  if (value === undefined || value === null) return ''
  return typeof value === 'string' ? value : JSON.stringify(value)
}

export class UsageError extends Error {}

const USAGE =
  'pr-state: usage: pr-state.mjs read --pr <n> [--repo <owner/name>] [--fresh] [--timeout-ms <ms>]' +
  ' | view [--pr <n>] [--repo <owner/name>] [--fresh] [--field <name>] [--timeout-ms <ms>]' +
  ' | session-pr [--field <name>] [--timeout-ms <ms>]'
const VERB_OPTIONS = Object.freeze({
  read: ['pr', 'repo', 'fresh', 'timeout-ms'],
  view: ['pr', 'repo', 'fresh', 'field', 'timeout-ms'],
  'session-pr': ['field', 'timeout-ms'],
})

// main returns `{ exit, out }`; with --field, `text` (the line to print instead) or `toStderr`.
export function main(argv, deps) {
  let parsed
  try {
    parsed = parseArgs({
      args: argv,
      allowPositionals: true,
      strict: true,
      options: {
        pr: { type: 'string' },
        repo: { type: 'string' },
        fresh: { type: 'boolean' },
        field: { type: 'string' },
        'timeout-ms': { type: 'string' },
      },
    })
  } catch (error) {
    throw new UsageError(`pr-state: ${error instanceof Error ? error.message : String(error)}`)
  }
  const { values, positionals } = parsed
  const [command, ...extra] = positionals
  const allowed = VERB_OPTIONS[command]
  if (!allowed || extra.length > 0) throw new UsageError(USAGE)
  if (command === 'view' && values.pr === undefined && values.repo !== undefined)
    throw new UsageError('pr-state: view --repo needs --pr (the session PR is this repo)')
  const stray = Object.keys(values).find((name) => !allowed.includes(name))
  if (stray) throw new UsageError(`pr-state: ${command} does not take --${stray}`)
  let pr
  if (values.pr !== undefined || command === 'read') {
    pr = Number(values.pr)
    if (!Number.isInteger(pr) || pr < 1)
      throw new UsageError('pr-state: --pr must be a positive integer')
  }
  if (values.field !== undefined && !/^[A-Za-z_]\w*$/.test(values.field))
    throw new UsageError('pr-state: --field must name one top-level field')
  const timeoutMs =
    values['timeout-ms'] === undefined ? DEFAULT_READ_TIMEOUT_MS : Number(values['timeout-ms'])
  if (!Number.isInteger(timeoutMs) || timeoutMs < MIN_SPAWN_MS) {
    throw new UsageError(`pr-state: --timeout-ms must be an integer >= ${MIN_SPAWN_MS}`)
  }
  const request = { pr, repo: values.repo ?? '', fresh: values.fresh === true, timeoutMs, deps }
  if (command === 'read') return readCommand(request)
  const result = command === 'session-pr' ? readSessionPR(request) : viewCommand(request)
  if (values.field === undefined) return result
  // With --field, stdout carries only the value, so a failure's JSON goes to stderr.
  if (result.exit !== 0) return { ...result, toStderr: true }
  return { ...result, text: fieldText(result.out, values.field) }
}

if (isMainModule(import.meta.url)) {
  try {
    const { exit, out, text: line, toStderr } = main(process.argv.slice(2))
    if (line !== undefined) process.stdout.write(`${line}\n`)
    else (toStderr ? process.stderr : process.stdout).write(`${JSON.stringify(out)}\n`)
    process.exitCode = exit
  } catch (error) {
    process.stderr.write(`${error instanceof Error ? error.message : String(error)}\n`)
    process.exitCode = error instanceof UsageError ? 2 : 1
  }
}
