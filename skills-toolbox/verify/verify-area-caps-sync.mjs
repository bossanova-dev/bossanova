import { spawnSync } from 'node:child_process'
import * as nodeFs from 'node:fs'
import { parseArgs } from 'node:util'
import { isMainModule } from '../main-module.mjs'
import { writeAtomicJson } from './verify-admission.mjs'
import {
  activeCaps,
  capHistory,
  parseClearComment,
  renderClearComment,
} from './verify-area-caps.mjs'
import { areaCapsPath } from './verify-area-caps-state.mjs'
import { trace as traceEscapes, list as listEscapes } from './verify-escapes-trace.mjs'

export const TRACE_DAYS = 3
const EPOCH = '1970-01-01T00:00:00.000Z'
/** @typedef {import('./verify-area-caps.mjs').CapClear} CapClear */
/** @typedef {import('./verify-area-caps.mjs').EscapeInput} EscapeInput */
/** @typedef {{status:number|null,stdout?:string,stderr?:string,error?:Error}} RunResult */
/** @typedef {(command:string,args:string[])=>RunResult} Run */
/** @typedef {{repo:string,since:string}} TraceOptions */
/** @typedef {{escapes:EscapeInput[],clears:CapClear[],unreadable:number}} Records */
/** @typedef {{failures?:{reason:string}[],truncated?:boolean}} TraceResult */
/** @typedef {{run?:Run,now?:()=>string|number|Date,fs?:Pick<typeof nodeFs,'mkdirSync'|'writeFileSync'|'renameSync'|'rmSync'>,trace?:(options:TraceOptions,deps:object)=>Promise<TraceResult>,list?:(options:TraceOptions,deps:object)=>Promise<Records>,stdout?:(text:string)=>void,stderr?:(text:string)=>void}} Dependencies */
/** @typedef {{repo?:string,area?:string,reason?:string,since?:string,'dry-run'?:boolean}} Options */

/** @type {Run} */
const defaultRun = (command, args) =>
  spawnSync(command, args, { encoding: 'utf8', timeout: 30000, maxBuffer: 16 * 1024 * 1024 })

/** @param {Run} run @param {string} command @param {string[]} args */
function checked(run, command, args) {
  const result = run(command, args)
  if (result.status !== 0 || result.error)
    throw new Error(`${command} failed${result.status === null ? '' : ` (${result.status})`}`)
  return result.stdout ?? ''
}

/** @param {unknown} error */
function failureReason(error) {
  const text = error instanceof Error ? error.message : String(error)
  if (/rate.?limit|429/i.test(text)) return 'rate-limited'
  if (/auth|log.?in|401/i.test(text)) return 'unauthenticated'
  if (/timeout|ETIMEDOUT/i.test(text)) return 'timeout'
  return 'trace-failed'
}

/** @param {string[]} argv */
function argumentsFor(argv) {
  const parsed = parseArgs({
    args: argv,
    strict: true,
    allowPositionals: true,
    options: {
      repo: { type: 'string' },
      area: { type: 'string' },
      reason: { type: 'string' },
      since: { type: 'string' },
      'dry-run': { type: 'boolean' },
    },
  })
  const [command, ...extra] = parsed.positionals
  const options = parsed.values
  if (extra.length || !['refresh', 'status', 'clear', 'history'].includes(command))
    throw new Error(
      'usage: refresh | status | clear --area KEY --reason TEXT [--dry-run] | history [--since ISO] [--repo OWNER/NAME]',
    )
  if (command === 'clear') {
    if (!options.area?.trim() || !options.reason?.trim())
      throw new Error('clear requires --area and a non-empty --reason')
  } else if (
    options.area !== undefined ||
    options.reason !== undefined ||
    options['dry-run'] !== undefined
  )
    throw new Error('--area, --reason and --dry-run are only valid for clear')
  if (
    options.since !== undefined &&
    (command !== 'history' || !Number.isFinite(Date.parse(options.since)))
  )
    throw new Error('--since requires an ISO date and is only valid for history')
  if (options.repo !== undefined && !/^[\w.-]+\/[\w.-]+$/.test(options.repo))
    throw new Error('--repo requires OWNER/NAME')
  if (options.area !== undefined && !/^[^\x00-\x1f<>]+$/.test(options.area))
    throw new Error('--area contains unsupported characters')
  return { command, options }
}

/**
 * Run a cap synchronization verb. All writes follow a complete stored-record read.
 * @param {string[]} argv @param {Dependencies} deps
 */
export async function main(argv = process.argv.slice(2), deps = {}) {
  const stdout = deps.stdout ?? ((text) => process.stdout.write(text))
  const stderr = deps.stderr ?? ((text) => process.stderr.write(text))
  let parsed
  try {
    parsed = argumentsFor(argv)
  } catch (error) {
    stderr(`${error instanceof Error ? error.message : 'invalid arguments'}\n`)
    return 64
  }
  try {
    const { command, options } = parsed
    const run = deps.run ?? defaultRun
    const fs = deps.fs ?? nodeFs
    const now = new Date(deps.now?.() ?? Date.now()).toISOString()
    const repo = checked(run, 'gh', [
      'repo',
      'view',
      ...(options.repo ? [options.repo] : []),
      '--json',
      'nameWithOwner',
      '--jq',
      '.nameWithOwner',
    ]).trim()
    if (!/^[\w.-]+\/[\w.-]+$/.test(repo)) throw new Error('repository unavailable')
    /** @type {{ok:boolean,reason?:string}} */
    let trace = { ok: true }
    if (command === 'refresh') {
      try {
        const result = await (deps.trace ?? traceEscapes)(
          { repo, since: new Date(Date.parse(now) - TRACE_DAYS * 86400000).toISOString() },
          { run, now: () => now },
        )
        if (result.failures?.length || result.truncated)
          trace = {
            ok: false,
            reason: result.truncated
              ? 'trace-truncated'
              : result.failures.map((failure) => failure.reason).join(', '),
          }
      } catch (error) {
        trace = { ok: false, reason: failureReason(error) }
      }
    }
    const records = await (deps.list ?? listEscapes)({ repo, since: EPOCH }, { run })
    const clears = records.clears ?? []
    const caps = activeCaps({ escapes: records.escapes, clears })
    if (command === 'history') {
      const since = options.since ? Date.parse(options.since) : -Infinity
      const episodes = capHistory({ escapes: records.escapes, clears }).filter(
        (episode) => Date.parse(episode.clearedAt ?? episode.cappedAt) >= since,
      )
      stdout(`${JSON.stringify({ repo, episodes, unreadable: records.unreadable }, null, 2)}\n`)
      return 0
    }
    if (command === 'clear') {
      const cap = caps.find((entry) => entry.key === options.area)
      if (!cap) {
        stderr(
          `Area ${options.area} is not actively capped. Active keys: ${caps.map((entry) => entry.key).join(', ') || '(none)'}\n`,
        )
        return 1
      }
      const clearedBy = checked(run, 'gh', ['api', 'user', '--jq', '.login']).trim()
      /** @type {CapClear} */
      const draft = {
        v: 1,
        repo,
        key: cap.key,
        clearedAt: now,
        clearedBy,
        reason: options.reason.trim(),
        through: cap.escapes[0].fix,
      }
      const comment = renderClearComment(draft)
      const clear = parseClearComment(comment)
      if (!clear) throw new Error('Invalid area cap clear record')
      const original = cap.escapes[0].original
      if (options['dry-run']) {
        stdout(`${JSON.stringify({ dryRun: true, original, clear, comment }, null, 2)}\n`)
        return 0
      }
      const file = areaCapsPath((cmd, args) => ({ ok: true, output: checked(run, cmd, args) }))
      checked(run, 'gh', ['pr', 'comment', String(original), '--repo', repo, '--body', comment])
      const state = {
        v: 1,
        repo,
        refreshedAt: now,
        caps: activeCaps({ escapes: records.escapes, clears: [...clears, clear] }),
        trace,
        unreadable: records.unreadable,
      }
      if (!writeAtomicJson(fs, file, state))
        throw new Error('clear posted; local cap state write failed, run status to retry')
      stdout(`${JSON.stringify({ clear, original, caps: state.caps }, null, 2)}\n`)
      return 0
    }
    if (command === 'status' || command === 'refresh') {
      const file = areaCapsPath((cmd, args) => ({ ok: true, output: checked(run, cmd, args) }))
      const state = { v: 1, repo, refreshedAt: now, caps, trace, unreadable: records.unreadable }
      if (!writeAtomicJson(fs, file, state)) throw new Error('cap state write failed')
      stdout(`${JSON.stringify(state, null, 2)}\n`)
      return trace.ok ? 0 : 1
    }
    return 64
  } catch (error) {
    stderr(`${error instanceof Error ? error.message : 'cap synchronization failed'}\n`)
    return 1
  } finally {
    if (parsed.command === 'refresh') {
      try {
        const run = deps.run ?? defaultRun
        const file = areaCapsPath((cmd, args) => ({ ok: true, output: checked(run, cmd, args) }))
        writeAtomicJson(deps.fs ?? nodeFs, `${file}.refresh-result`, {
          completedAt: new Date(deps.now?.() ?? Date.now()).toISOString(),
        })
      } catch {}
    }
  }
}

if (isMainModule(import.meta.url)) process.exitCode = await main()
