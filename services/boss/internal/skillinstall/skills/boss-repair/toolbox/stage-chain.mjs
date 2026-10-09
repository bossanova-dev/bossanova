#!/usr/bin/env node
// skills-toolbox/stage-chain.mjs
// Event-driven stage chaining and phase reporting for the plan → build → verify → release cores.
//
// A finished stage hands off to the next one instead of waiting for that stage's cron tick, and
// every working core reports which phase it is in. This helper owns every argv those hand-offs and
// phase calls run, so each call site is one line and the behaviour is tested here, not in prose.
//
//   node stage-chain.mjs next       --stage <plan|build|verify|release>              [--dry-run]
//   node stage-chain.mjs run-next   --stage <plan|build|verify|release>              [--dry-run]
//   node stage-chain.mjs arm-verify --pr <n> [--completion <file>] [--run-id <id>]
//                                   [--session <id>]                                 [--dry-run]
//   node stage-chain.mjs phase      <name>                                           [--dry-run]
//
// Every verb prints exactly one JSON line `{"verb","action",...}` on stdout and exits 0 — chaining
// only accelerates a stage whose cron job still fires on schedule, so it is never fatal. Exit 64
// only on a usage error. Every `boss` call has a timeout; a timeout is `error`, still exit 0.
//
// Matching is the first `/<skill>` or `$<skill>` token of a cron job's prompt, compared exactly, so
// `/boss-build-ce` never counts as `/boss-build`. `arm-verify` never sends `/boss-verify` now: the
// `checks_passed_ready` callback is the deferred send, so verify judges a settled head.
//
// Node built-ins plus ./main-module.mjs, ./boss-binary.mjs and ./bossd-present.mjs only.

import { execFile as defaultExecFile } from 'node:child_process'
import defaultFs from 'node:fs'
import path from 'node:path'

import { resolveBossBinary } from './boss-binary.mjs'
import { isBossdManaged } from './bossd-present.mjs'
import { isMainModule } from './main-module.mjs'

export const STAGE_SKILL = Object.freeze({
  plan: 'boss-plan',
  build: 'boss-build',
  verify: 'boss-verify',
  release: 'boss-release',
})
export const NEXT_STAGE = Object.freeze({
  plan: 'build',
  build: 'verify',
  verify: 'release',
  release: null,
})
// The one title both this helper and the verify router use for a PR session's verify chat.
export const VERIFY_CHAT_TITLE = 'verify'
export const VERIFY_TRIGGER = 'checks_passed_ready'
// If the callback lapses, the verify cron gate still covers the PR.
export const VERIFY_CALLBACK_EXPIRY = '7d'
// `cron run-now` runs the job's gate, which alone may take 60 s.
export const RUN_NOW_TIMEOUT_MS = 120_000
export const CALL_TIMEOUT_MS = 30_000
export const EXIT_USAGE = 64

const STOPPED = 'STOPPED'
const text = (value) => typeof value === 'string' && value.trim() !== ''

// ---------------------------------------------------------------------------------------------
// Pure decisions
// ---------------------------------------------------------------------------------------------

// A skill token: `/name` or `$name` (codex) at start-of-string or after whitespace. The name is
// taken greedily and must not be followed by another name character, a path separator or a plugin
// namespace colon, so `/boss-build-ce`, `/usr/bin/x` and `https://x/boss-build` never yield
// `boss-build`.
const SKILL_TOKEN = /(?:^|\s)[/$]([a-z0-9][a-z0-9-]*)(?![A-Za-z0-9_\-/:])/

/** The first skill-invocation token in a cron prompt, or null. */
export function invokedSkill(prompt) {
  if (typeof prompt !== 'string') return null
  const match = SKILL_TOKEN.exec(prompt)
  return match ? match[1] : null
}

const invokes = (job, skill) =>
  Boolean(job) && typeof job === 'object' && invokedSkill(job.prompt) === skill

/** True when at least one enabled job invokes `stage`'s skill — the consent predicate. */
export function hasStageJob({ stage, jobs } = {}) {
  const skill = STAGE_SKILL[stage]
  if (!skill || !Array.isArray(jobs)) return false
  return jobs.some((job) => invokes(job, skill) && job.enabled === true)
}

/**
 * Which cron job, if any, runs the stage after `stage`.
 * @returns {{action: 'run-now'|'none'|'ambiguous', jobId: string, nextStage: string|null,
 *   reason: string, matches: string[]}}
 */
export function resolveNext({ stage, jobs } = {}) {
  const nextStage = NEXT_STAGE[stage] ?? null
  const result = (action, reason, matches = [], jobId = '') => ({
    action,
    jobId,
    nextStage,
    reason,
    matches,
  })
  if (!nextStage) return result('none', 'terminal-stage')
  if (!Array.isArray(jobs)) return result('none', 'unreadable-jobs')
  const skill = STAGE_SKILL[nextStage]
  const matching = jobs.filter((job) => invokes(job, skill))
  const enabled = matching.filter((job) => job.enabled === true).map((job) => String(job.id ?? ''))
  if (enabled.length === 1) return result('run-now', '', enabled, enabled[0])
  if (enabled.length > 1) return result('ambiguous', 'several-jobs', enabled)
  return result('none', matching.length > 0 ? 'disabled' : 'no-job')
}

function normalizeStatus(status) {
  return String(status ?? '')
    .trim()
    .toUpperCase()
    .replace(/^CHAT_STATUS_/, '')
}

const createdMs = (chat) => {
  const ms = Date.parse(String(chat?.created_at ?? ''))
  return Number.isNaN(ms) ? -Infinity : ms
}

/**
 * The PR session's verify chat: the newest chat titled `verify` (trimmed, case-insensitive) that
 * has an id and is not STOPPED, or null. Ties keep the listing's order.
 */
export function pickVerifyChat(chats) {
  if (!Array.isArray(chats)) return null
  let best = null
  for (const chat of chats) {
    if (!chat || typeof chat !== 'object') continue
    if (typeof chat.title !== 'string' || chat.title.trim().toLowerCase() !== VERIFY_CHAT_TITLE)
      continue
    if (!text(chat.agent_session_id) || normalizeStatus(chat.status) === STOPPED) continue
    if (!best || createdMs(chat) > createdMs(best)) best = chat
  }
  return best
}

/**
 * How to arm the verify callback after a boss-build completion record (the fast path's
 * `{runId, action, reason}`). State-matched is the default: the verify receiver is idempotent per
 * head, so a callback that fires on an already-settled head costs one cheap judge.
 * @returns {{arm: 'state'|'transition'|'none', reason: string}}
 */
export function armDecision(record, { runId } = {}) {
  if (!record || typeof record !== 'object' || Array.isArray(record))
    return { arm: 'state', reason: 'no-record' }
  if (text(runId) && record.runId !== runId) return { arm: 'state', reason: 'stale-record' }
  if (record.action === 'merged') return { arm: 'none', reason: 'merged' }
  const reason = typeof record.reason === 'string' ? record.reason.trim() : ''
  const kind = reason.split(':')[0]
  if (kind === 'human') return { arm: 'none', reason: 'human' }
  if (kind === 'epic-child') return { arm: 'none', reason: 'epic-child' }
  // Only a new head can change a defect verdict.
  if (kind === 'defect') return { arm: 'transition', reason: 'defect' }
  return { arm: 'state', reason: reason || 'unrecognized-record' }
}

/** An epic child or prompt-carrying unattended launch: unattended with no cron job id. */
export function isEpicChild(env = {}) {
  const unattended = env.BOSS_UNATTENDED === 'true' || env.BOSS_CRON === 'true'
  return unattended && !text(env.BOSS_CRON_JOB_ID)
}

export function runNowArgv(jobId) {
  return ['cron', 'run-now', jobId]
}

export function cronListArgv(repoId) {
  return ['cron', 'ls', '--repo', repoId, '--json']
}

export function verifyChatArgv(sessionId) {
  return ['chat', 'new', sessionId, '--title', VERIFY_CHAT_TITLE, '--json']
}

export function armedListArgv({ pr, chatId }) {
  return [
    'callback',
    'list',
    '--chat',
    chatId,
    '--pr',
    String(pr),
    '--trigger',
    VERIFY_TRIGGER,
    '--state',
    'active',
    '--json',
  ]
}

export function verifyCallbackArgv({ pr, chatId, onTransition = false }) {
  return [
    'callback',
    'add',
    String(pr),
    VERIFY_TRIGGER,
    '--chat',
    chatId,
    '--message',
    `/boss-verify ${pr}`,
    '--expires-in',
    VERIFY_CALLBACK_EXPIRY,
    ...(onTransition ? ['--on-transition'] : []),
    '--json',
  ]
}

export function phaseArgv(name) {
  return ['session', 'phase', name]
}

/** Read `boss cron run-now` output: it has no --json and exits 0 whether it fired or skipped. */
export function parseRunNow(stdout) {
  const out = String(stdout ?? '')
  const started = /Started session (\S+) for cron job/.exec(out)
  if (started) return { action: 'started', sessionId: started[1] }
  const skippedLine = /Fire skipped for cron job \S+: (.+)$/m.exec(out)
  if (skippedLine) return { action: 'skipped', reason: skippedLine[1].trim() }
  return { action: 'error', reason: 'unrecognized-output' }
}

// ---------------------------------------------------------------------------------------------
// CLI
// ---------------------------------------------------------------------------------------------

export class UsageError extends Error {}

export const USAGE = [
  'usage: node stage-chain.mjs next|run-next --stage <plan|build|verify|release> [--dry-run]',
  '       node stage-chain.mjs arm-verify --pr <n> [--completion <file>] [--run-id <id>] [--session <id>] [--dry-run]',
  '       node stage-chain.mjs phase <name> [--dry-run]',
].join('\n')

export function parseArgs(argv = []) {
  const [subcommand, ...rest] = argv
  const known =
    subcommand === 'next' ||
    subcommand === 'run-next' ||
    subcommand === 'arm-verify' ||
    subcommand === 'phase'
  if (!known) throw new UsageError(`unknown verb: ${subcommand ?? '(none)'}`)
  const verb = subcommand
  const opts = { verb, dryRun: false }
  const positionals = []
  for (let i = 0; i < rest.length; i++) {
    const arg = rest[i]
    const value = () => {
      const next = rest[++i]
      if (next === undefined || next.startsWith('--')) throw new UsageError(`${arg} needs a value`)
      return next
    }
    if (arg === '--dry-run') opts.dryRun = true
    else if (arg === '--stage' && (verb === 'next' || verb === 'run-next')) opts.stage = value()
    else if (arg === '--pr' && verb === 'arm-verify') opts.pr = value()
    else if (arg === '--completion' && verb === 'arm-verify') opts.completion = value()
    else if (arg === '--run-id' && verb === 'arm-verify') opts.runId = value()
    else if (arg === '--session' && verb === 'arm-verify') opts.session = value()
    else if (!arg.startsWith('--') && verb === 'phase') positionals.push(arg)
    else throw new UsageError(`unknown argument: ${arg}`)
  }
  if (verb === 'next' || verb === 'run-next') {
    if (!opts.stage) throw new UsageError('--stage is required')
    if (!Object.hasOwn(STAGE_SKILL, opts.stage))
      throw new UsageError(`--stage must be one of ${Object.keys(STAGE_SKILL).join(', ')}`)
  }
  if (verb === 'arm-verify') {
    if (!/^[1-9]\d*$/.test(opts.pr ?? '')) throw new UsageError('--pr must be a PR number')
    opts.pr = Number(opts.pr)
  }
  if (verb === 'phase') {
    if (positionals.length !== 1 || !text(positionals[0]))
      throw new UsageError('phase takes exactly one phase name')
    opts.phase = positionals[0]
  }
  return opts
}

function bossCall(execFile, bossPath, args, { timeout, cwd }) {
  return new Promise((resolve) => {
    let child
    const done = (error, stdout, stderr) => {
      const out = String(stdout ?? '')
      const err = String(stderr ?? '')
      if (!error) return resolve({ ok: true, stdout: out, stderr: err })
      const timedOut = Boolean(error.killed) || error.code === 'ETIMEDOUT'
      resolve({
        ok: false,
        stdout: out,
        stderr: err,
        error: timedOut ? 'timeout' : String(error.code ?? 'exec-failed'),
      })
    }
    try {
      child = execFile(
        bossPath,
        args,
        { timeout, cwd, maxBuffer: 16 * 1024 * 1024, killSignal: 'SIGKILL' },
        done,
      )
      child?.on?.('error', () => {})
    } catch (error) {
      done(error)
    }
  })
}

function parseJson(stdout) {
  try {
    return { ok: true, value: JSON.parse(stdout) }
  } catch {
    return { ok: false }
  }
}

const firstLine = (s) =>
  String(s ?? '')
    .split('\n')
    .map((line) => line.trim())
    .find(Boolean) ?? ''

/**
 * Run one verb. Every side effect is injected — `execFile`, `env`, `fs`, `stderr`, `cwd` and
 * `resolveBoss` — so tests never touch a daemon. Returns the JSON object to print.
 */
export async function runVerb(opts, deps = {}) {
  const env = deps.env ?? process.env
  const execFile = deps.execFile ?? defaultExecFile
  const fs = deps.fs ?? defaultFs
  const stderr = deps.stderr ?? process.stderr
  const cwd = deps.cwd ?? process.cwd()
  const resolveBoss = deps.resolveBoss ?? ((e) => resolveBossBinary(e, { cwd }))
  const out = (fields) => ({ verb: opts.verb, ...fields })
  const skipped = (reason, detail) =>
    out(detail ? { action: 'skipped', reason, detail } : { action: 'skipped', reason })
  const failed = (reason, detail) =>
    out(detail ? { action: 'error', reason, detail } : { action: 'error', reason })

  if (!isBossdManaged(env)) return skipped('not-managed')
  // boss-epic owns merge ordering for its children; verify routing for epics is out of scope.
  if (opts.verb === 'arm-verify' && isEpicChild(env)) return skipped('epic-child')
  const repoId = String(env.BOSS_REPO_ID ?? '').trim()
  if (opts.verb !== 'phase' && !repoId) return skipped('no-repo-id')
  const resolved = resolveBoss(env)
  if (!resolved?.ok) return skipped('no-boss-binary', resolved?.reason)
  const bossPath = path.resolve(cwd, resolved.path)
  const call = (args, timeout = CALL_TIMEOUT_MS) =>
    bossCall(execFile, bossPath, args, { timeout, cwd })
  const callJson = async (args) => {
    const r = await call(args)
    if (!r.ok) return { ok: false, reason: r.error, detail: firstLine(r.stderr) }
    const parsed = parseJson(r.stdout)
    if (!parsed.ok) return { ok: false, reason: 'unreadable-json' }
    return { ok: true, value: parsed.value }
  }

  if (opts.verb === 'phase') {
    const argv = phaseArgv(opts.phase)
    if (opts.dryRun) return out({ action: 'dry-run', argv: [argv] })
    const r = await call(argv)
    if (r.ok) return out({ action: 'set', phase: opts.phase })
    if (/unknown command/i.test(`${r.stderr}\n${r.stdout}`)) return skipped('phase-unsupported')
    return failed(r.error, firstLine(r.stderr))
  }

  if (opts.verb === 'next' || opts.verb === 'run-next') {
    const listArgv = cronListArgv(repoId)
    if (opts.dryRun)
      return out({
        action: 'dry-run',
        argv: opts.verb === 'run-next' ? [listArgv, runNowArgv('<jobId>')] : [listArgv],
      })
    const jobs = await callJson(listArgv)
    if (!jobs.ok) return failed(`cron-ls-${jobs.reason}`, jobs.detail)
    const next = resolveNext({ stage: opts.stage, jobs: jobs.value })
    if (next.action === 'ambiguous')
      stderr.write(
        `stage-chain: ${next.matches.length} enabled cron jobs run /${STAGE_SKILL[next.nextStage]} (${next.matches.join(', ')}); not chaining\n`,
      )
    const base = { stage: opts.stage, ...next }
    if (opts.verb === 'next' || next.action !== 'run-now') return out(base)
    const r = await call(runNowArgv(next.jobId), RUN_NOW_TIMEOUT_MS)
    if (!r.ok) return out({ ...base, ...failedFields(r) })
    const fired = parseRunNow(r.stdout)
    return out({ ...base, ...fired })
  }

  // arm-verify
  const sessionId = text(opts.session) ? opts.session.trim() : String(env.BOSS_SESSION_ID).trim()
  if (opts.dryRun)
    return out({
      action: 'dry-run',
      argv: [
        cronListArgv(repoId),
        ['chats', sessionId, '--json'],
        verifyChatArgv(sessionId),
        armedListArgv({ pr: opts.pr, chatId: '<verifyChat>' }),
        verifyCallbackArgv({ pr: opts.pr, chatId: '<verifyChat>' }),
      ],
    })
  const jobs = await callJson(cronListArgv(repoId))
  if (!jobs.ok) return failed(`cron-ls-${jobs.reason}`, jobs.detail)
  if (!hasStageJob({ stage: 'verify', jobs: jobs.value }))
    return out({ action: 'none', reason: 'no-verify-cron' })

  let record = null
  if (text(opts.completion)) {
    try {
      record = JSON.parse(fs.readFileSync(opts.completion, 'utf8'))
    } catch {
      record = null
    }
  }
  const decision = armDecision(record, { runId: opts.runId })
  if (decision.arm === 'none') return out({ action: 'none', reason: decision.reason })
  const mode = decision.arm

  const listed = await callJson(['chats', sessionId, '--json'])
  if (!listed.ok) return failed(`chats-${listed.reason}`, listed.detail)
  if (!Array.isArray(listed.value?.chats)) return failed('chats-unreadable-json')
  let chatId = pickVerifyChat(listed.value.chats)?.agent_session_id ?? ''
  let chatCreated = false
  if (chatId) {
    const armed = await callJson(armedListArgv({ pr: opts.pr, chatId }))
    if (!armed.ok) return failed(`callback-list-${armed.reason}`, armed.detail)
    if (!Array.isArray(armed.value)) return failed('callback-list-unreadable-json')
    const live = armed.value.find(
      (row) =>
        row &&
        row.pr_number === opts.pr &&
        row.target_chat_id === chatId &&
        row.trigger === VERIFY_TRIGGER &&
        String(row.state).toLowerCase() === 'active',
    )
    if (live) return out({ action: 'already-armed', chatId, callbackId: String(live.id ?? '') })
  } else {
    const created = await callJson(verifyChatArgv(sessionId))
    if (!created.ok) return failed(`chat-new-${created.reason}`, created.detail)
    chatId = String(created.value?.chat?.agent_session_id ?? '').trim()
    if (!chatId) return failed('chat-new-no-chat-id')
    chatCreated = true
  }

  const added = await callJson(
    verifyCallbackArgv({ pr: opts.pr, chatId, onTransition: mode === 'transition' }),
  )
  if (!added.ok) return failed(`callback-add-${added.reason}`, added.detail)
  return out({
    action: 'armed',
    chatId,
    chatCreated,
    mode,
    reason: decision.reason,
    callbackId: String(added.value?.id ?? ''),
  })
}

function failedFields(r) {
  const detail = firstLine(r.stderr)
  return detail
    ? { action: 'error', reason: r.error, detail }
    : { action: 'error', reason: r.error }
}

/** Parse, run and render. Returns `{exitCode, line}`; never throws. */
export async function main(argv, deps = {}) {
  const stderr = deps.stderr ?? process.stderr
  let opts
  try {
    opts = parseArgs(argv)
  } catch (error) {
    if (!(error instanceof UsageError)) throw error
    stderr.write(`stage-chain: ${error.message}\n${USAGE}\n`)
    return { exitCode: EXIT_USAGE, line: '' }
  }
  let result
  try {
    result = await runVerb(opts, deps)
  } catch (error) {
    // Never fatal: an unexpected failure is an error line the agent prints and moves past.
    result = { verb: opts.verb, action: 'error', reason: String(error?.message ?? error) }
  }
  return { exitCode: 0, line: `${JSON.stringify(result)}\n` }
}

if (isMainModule(import.meta.url)) {
  const { exitCode, line } = await main(process.argv.slice(2))
  if (line) process.stdout.write(line)
  process.exitCode = exitCode
}
