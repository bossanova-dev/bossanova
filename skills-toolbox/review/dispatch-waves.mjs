#!/usr/bin/env node
// Plan bounded independent review waves; unavailable workers are recorded by the caller.
import { readFileSync } from 'node:fs'
import { parseArgs } from 'node:util'
import { isMainModule } from '../main-module.mjs'

/** @typedef {{id: string, severity?: 'P0'|'P1'|'P2'|'P3'}} Job */
/** @typedef {'finder'|'verifier'|'resolver'} Kind */
/** @typedef {'claude'|'codex'} Host */
const ranks = { P0: 0, P1: 1, P2: 2, P3: 3 }

/** @param {unknown} input @returns {asserts input is Job[]} */
function validateJobs(input) {
  if (!Array.isArray(input)) throw new Error('jobs must be an array')
  const ids = new Set()
  for (const job of input) {
    if (
      !job ||
      typeof job.id !== 'string' ||
      !job.id.trim() ||
      (job.severity !== undefined && !Object.hasOwn(ranks, job.severity))
    )
      throw new Error('each job needs a non-empty id and optional P0/P1/P2/P3 severity')
    if (ids.has(job.id)) throw new Error(`duplicate job id: ${job.id}`)
    ids.add(job.id)
  }
}

/** @param {Kind} kind @param {Job[]} jobs */
export function selectJobs(kind, jobs) {
  validateJobs(jobs)
  if (!['finder', 'verifier', 'resolver'].includes(kind)) throw new Error('invalid kind')
  const sorted =
    kind === 'finder'
      ? [...jobs]
      : jobs.toSorted((a, b) => ranks[a.severity ?? 'P3'] - ranks[b.severity ?? 'P3'])
  const limit =
    kind === 'finder'
      ? jobs.length
      : kind === 'resolver'
        ? 6
        : Math.max(12, jobs.filter((j) => j.severity === 'P0' || j.severity === 'P1').length)
  const kept = sorted.slice(0, limit)
  const keptIds = new Set(kept.map((j) => j.id))
  return { kept, dropped: jobs.filter((j) => !keptIds.has(j.id)) }
}

/** @param {unknown} value */
function positiveInteger(value) {
  if (typeof value !== 'string' || !/^\d+$/.test(value)) return undefined
  const n = Number(value)
  return Number.isSafeInteger(n) && n > 0 ? n : undefined
}

/** @param {Host} host @param {number} count @param {string} [slots]
 * @param {NodeJS.ProcessEnv} [env] */
export function resolveSlots(host, count, slots, env = process.env) {
  return (
    positiveInteger(slots) ??
    positiveInteger(env.BOSS_REVIEW_AGENT_SLOTS) ??
    (host === 'codex' ? 3 : Math.max(1, count))
  )
}

/** @param {{host: Host, kind: Kind, jobs: Job[], slots?: string, env?: NodeJS.ProcessEnv}} input */
export function planWaves({ host, kind, jobs, slots, env }) {
  if (!['claude', 'codex'].includes(host)) throw new Error('invalid host')
  const selected = selectJobs(kind, jobs)
  const size = resolveSlots(host, selected.kept.length, slots, env)
  const waves = []
  for (let i = 0; i < selected.kept.length; i += size)
    waves.push(selected.kept.slice(i, i + size).map((j) => j.id))
  return { host, kind, slots: size, waves, dropped: selected.dropped.map((j) => j.id) }
}

/** @param {string[]} argv
 * @param {{readFile?: (file: string) => string, stdout?: (s: string) => void,
 * stderr?: (s: string) => void, env?: NodeJS.ProcessEnv}} [deps] */
export function main(argv, deps = {}) {
  const stdout = deps.stdout ?? ((s) => process.stdout.write(s))
  const stderr = deps.stderr ?? ((s) => process.stderr.write(s))
  try {
    const { values, positionals } = parseArgs({
      args: argv,
      strict: true,
      allowPositionals: true,
      options: {
        host: { type: 'string' },
        kind: { type: 'string' },
        jobs: { type: 'string' },
        slots: { type: 'string' },
      },
    })
    const command = positionals[0]
    if (
      positionals.length !== 1 ||
      command !== 'plan' ||
      !values.jobs ||
      !['claude', 'codex'].includes(values.host ?? '') ||
      !['finder', 'verifier', 'resolver'].includes(values.kind ?? '')
    )
      throw new Error(
        'usage: dispatch-waves.mjs plan --host claude|codex --kind finder|verifier|resolver --jobs <file> [--slots <n>]',
      )
    const jobs = JSON.parse((deps.readFile ?? ((f) => readFileSync(f, 'utf8')))(values.jobs))
    validateJobs(jobs)
    const result = planWaves({
      host: /** @type {Host} */ (values.host),
      kind: /** @type {Kind} */ (values.kind),
      jobs,
      slots: values.slots,
      env: deps.env,
    })
    stdout(`${JSON.stringify(result)}\n`)
    return 0
  } catch (error) {
    stderr(`dispatch-waves: ${error instanceof Error ? error.message : String(error)}\n`)
    return 2
  }
}

if (isMainModule(import.meta.url)) process.exitCode = main(process.argv.slice(2))
