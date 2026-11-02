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
  return {
    spawn: deps.spawn ?? spawnSync,
    env,
    now: deps.now ?? Date.now,
    resolveBoss: deps.resolveBoss ?? (() => resolveBossBinary(env)),
  }
}

// bossCall runs the resolved `boss` binary and returns exactly one of `{kind:'ok', body}`,
// `{kind:'fallback', reason}` (no daemon answered) or `{kind:'error', error, code}`. A help screen
// can exit 0, so success is a JSON object on stdout that is not a failure envelope, never the exit
// status alone.
export function bossCall({ argv, timeoutMs, deps }) {
  const { spawn, env, resolveBoss } = io(deps)
  const binary = resolveBoss(env)
  if (!binary?.ok || typeof binary.path !== 'string') {
    return { kind: 'fallback', reason: 'no-boss-binary' }
  }
  const res = spawn(binary.path, argv, {
    encoding: 'utf8',
    timeout: Math.max(MIN_SPAWN_MS, Math.floor(timeoutMs)),
    killSignal: 'SIGKILL',
    maxBuffer: MAX_BUFFER,
    env,
  })
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

export function readPRState({ pr, repo = '', fresh = false, timeoutMs, deps }) {
  const argv = ['pr', 'state', String(pr), '--with-workflow-runs', '--json']
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
  const { spawn, env } = io(deps)
  return spawn('gh', args, {
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

export class UsageError extends Error {}

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
        'timeout-ms': { type: 'string' },
      },
    })
  } catch (error) {
    throw new UsageError(`pr-state: ${error instanceof Error ? error.message : String(error)}`)
  }
  const { values, positionals } = parsed
  const [command, ...extra] = positionals
  if (command !== 'read' || extra.length > 0) {
    throw new UsageError(
      'pr-state: usage: pr-state.mjs read --pr <n> [--repo <owner/name>] [--fresh] [--timeout-ms <ms>]',
    )
  }
  const pr = Number(values.pr)
  if (!Number.isInteger(pr) || pr < 1)
    throw new UsageError('pr-state: --pr must be a positive integer')
  const timeoutMs =
    values['timeout-ms'] === undefined ? DEFAULT_READ_TIMEOUT_MS : Number(values['timeout-ms'])
  if (!Number.isInteger(timeoutMs) || timeoutMs < MIN_SPAWN_MS) {
    throw new UsageError(`pr-state: --timeout-ms must be an integer >= ${MIN_SPAWN_MS}`)
  }
  return readCommand({ pr, repo: values.repo ?? '', fresh: values.fresh === true, timeoutMs, deps })
}

if (isMainModule(import.meta.url)) {
  try {
    const { exit, out } = main(process.argv.slice(2))
    process.stdout.write(`${JSON.stringify(out)}\n`)
    process.exitCode = exit
  } catch (error) {
    process.stderr.write(`${error instanceof Error ? error.message : String(error)}\n`)
    process.exitCode = error instanceof UsageError ? 2 : 1
  }
}
