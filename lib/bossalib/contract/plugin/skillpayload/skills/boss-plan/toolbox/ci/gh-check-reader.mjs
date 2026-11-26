import { spawnSync } from 'node:child_process'
import {
  FALLBACK_LABELS,
  ghSpawn,
  isRateLimitRefusal,
  readWithQuotaFallback,
} from './gh-quota-read.mjs'
import { repository } from './gh-repository.mjs'
import { restCheckSnapshot, graphqlRunsArgs, runsFromCheckSuites } from './rest-check-state.mjs'
import { checkRunsFromSuites, graphqlCheckSuitesArgs } from './dispatched-checks.mjs'
import { readArgs as statusReadArgs } from '../commit-status.mjs'
import { WORKFLOW_RUNS_LIMIT } from '../pr-check-state.mjs'

const RUN_LIST_FIELDS = 'name,workflowName,status,conclusion,headSha,event'
/** @returns {{ok:false,error:{code:string,message:string}}} */
const failure = (code, message) => ({ ok: false, error: { code, message } })

/** @param {{label:string,timeoutMs:number,deps?:object}} options */
function boundedOptions(options) {
  const now = options.deps?.now ?? Date.now
  const deadline = now() + options.timeoutMs
  let firstSpawn = true
  const spawn = options.deps?.spawn ?? spawnSync
  return {
    ...options,
    deps: {
      ...options.deps,
      spawn: (command, args, spawnOptions) => {
        const remaining = firstSpawn ? options.timeoutMs : Math.floor(deadline - now())
        firstSpawn = false
        if (remaining < 1) return { error: { code: 'ETIMEDOUT' } }
        return spawn(command, args, {
          ...spawnOptions,
          timeout: Math.min(spawnOptions.timeout, remaining),
        })
      },
    },
  }
}

function jsonRead(args, options, pending = false) {
  const res = ghSpawn(args, options)
  if (res.error || (res.status !== 0 && !(pending && res.status === 8)))
    return {
      ok: false,
      refused: !res.error && isRateLimitRefusal(String(res.stderr ?? '')),
      error: res.error?.code ?? `gh exited ${res.status}`,
    }
  try {
    return { ok: true, json: JSON.parse(res.stdout) }
  } catch {
    return { ok: false, refused: false, error: 'unparseable gh output' }
  }
}

/** @param {{pr:number,repo?:string,label:string,timeoutMs:number,deps?:object}} options */
export function restSnapshot({ pr, repo = '', ...options }) {
  options = boundedOptions(options)
  try {
    return restCheckSnapshot({
      pr,
      repo: repository(repo, options.deps, options.timeoutMs, options.label),
      gh: (args) => jsonRead(args, options),
    })
  } catch (error) {
    return failure('UNREADABLE', error.message)
  }
}

/** @param {{pr:number,repo?:string,fields:string,label:string,timeoutMs:number,deps?:object}} options */
export function readView({ pr, repo = '', fields, ...options }) {
  options = boundedOptions(options)
  const first = jsonRead(
    ['pr', 'view', String(pr), '--json', fields, ...(repo ? ['--repo', repo] : [])],
    options,
  )
  if (first.ok) {
    if (!first.json || typeof first.json !== 'object' || Array.isArray(first.json))
      return failure('UNREADABLE', 'not an object')
    return {
      ok: /** @type {const} */ (true),
      value: { view: first.json, runs: null },
      degraded: null,
    }
  }
  if (!first.refused) return failure('UNREADABLE', first.error)
  const alternate = restSnapshot({ pr, repo, ...options })
  return alternate.ok
    ? { ...alternate, degraded: { transport: 'core', reason: 'graphql-rate-limited' } }
    : alternate
}

/** @param {{headSha:string,repo?:string,label:string,timeoutMs:number,deps?:object}} options */
export function readRuns({ headSha, repo = '', ...options }) {
  options = boundedOptions(options)
  try {
    return readWithQuotaFallback({
      ...options,
      primary: {
        args: [
          'run',
          'list',
          '--commit',
          headSha,
          '--json',
          RUN_LIST_FIELDS,
          '--limit',
          String(WORKFLOW_RUNS_LIMIT),
          ...(repo ? ['--repo', repo] : []),
        ],
        normalize: (value) => {
          if (!Array.isArray(value)) throw new Error('malformed runs')
          return value
        },
      },
      alternate: {
        get args() {
          const parts = repository(repo, options.deps, options.timeoutMs, options.label).split('/')
          const host = parts.length === 3 ? parts.shift() : null
          return [
            ...graphqlRunsArgs({ owner: parts[0], name: parts[1], headSha }),
            ...(host ? ['--hostname', host] : []),
          ]
        },
        normalize: (value) => runsFromCheckSuites(value, headSha),
      },
    })
  } catch (error) {
    return failure('UNREADABLE', error.message)
  }
}

// The head commit's check runs with each suite's event and commit (./dispatched-checks.mjs): the
// rollup leaves out `workflow_dispatch` suites. One GraphQL read; a refusal is a failed read.
/** @param {{headSha:string,repo?:string,label:string,timeoutMs:number,deps?:object}} options */
export function readDispatchedChecks({ headSha, repo = '', ...options }) {
  options = boundedOptions(options)
  try {
    const parts = repository(repo, options.deps, options.timeoutMs, options.label).split('/')
    const host = parts.length === 3 ? parts.shift() : null
    const result = jsonRead(
      [
        ...graphqlCheckSuitesArgs({ owner: parts[0], name: parts[1], headSha }),
        ...(host ? ['--hostname', host] : []),
      ],
      options,
    )
    if (!result.ok) return failure(result.refused ? 'RATE_LIMITED' : 'UNREADABLE', result.error)
    return { ok: /** @type {const} */ (true), value: checkRunsFromSuites(result.json) }
  } catch (error) {
    return failure('UNREADABLE', error.message)
  }
}

// One REST snapshot supplies the view and subsequent runs for that exact head.
/** @param {{label?: string, deps?: object}} [options] */
export function ghCheckReader({ label = FALLBACK_LABELS.CI_WAIT, deps } = {}) {
  let degraded = null
  let snapshot = null
  const options = (timeoutMs) => ({ label, deps, timeoutMs })
  const answer = (result, what) => {
    if (!result.ok) {
      degraded = null
      return { ok: false, error: result.error.message }
    }
    degraded = result.degraded ?? null
    const payload = what === 'view' ? result.value.view : result.value
    return {
      ok: true,
      ...(what === 'view' ? { headSha: payload.headRefOid ?? '' } : {}),
      payload,
      ...(degraded ? { degraded } : {}),
    }
  }
  const read = async ({ what, pr, headSha = '', timeoutMs }) => {
    if (['runs', 'verify-status', 'dispatched-checks'].includes(what) && !headSha)
      return { ok: false, error: 'no head SHA' }
    if (what === 'dispatched-checks') {
      const result = readDispatchedChecks({ headSha, ...options(timeoutMs) })
      if ('error' in result) return { ok: false, error: result.error.message }
      return { ok: true, payload: result.value }
    }
    if (what === 'view') {
      snapshot = null
      const result = readView({
        pr,
        fields: 'headRefOid,statusCheckRollup,baseRefName',
        ...options(timeoutMs),
      })
      if (result.ok && result.value.runs !== null) snapshot = result
      return answer(result, what)
    }
    if (what === 'runs') {
      if (snapshot?.value.view.headRefOid === headSha)
        return answer({ ok: true, value: snapshot.value.runs, degraded: snapshot.degraded }, what)
      return answer(readRuns({ headSha, ...options(timeoutMs) }), what)
    }
    try {
      const args =
        what === 'verify-status'
          ? statusReadArgs({ sha: headSha, repo: '' })
          : ['pr', 'checks', String(pr), '--json', 'name,state,bucket']
      const result = jsonRead(args, options(timeoutMs), what === 'checks')
      return result.ok ? { ok: true, payload: result.json } : { ok: false, error: result.error }
    } catch (error) {
      return { ok: false, error: error.message }
    }
  }
  read.degraded = () => degraded
  read.crossCheck = async ({ pr, timeoutMs }) => {
    const result = restSnapshot({ pr, ...options(timeoutMs) })
    snapshot = result.ok
      ? { ...result, degraded: { transport: 'core', reason: 'cross-check' } }
      : null
    return answer(snapshot ?? result, 'view')
  }
  return read
}
