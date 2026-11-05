import { execFile } from 'node:child_process'
import { setTimeout as realSleep } from 'node:timers/promises'
import { promisify } from 'node:util'
import { parseArgs } from 'node:util'
import { loadSkillConfig } from '../skill-config.mjs'
import { isMainModule } from '../main-module.mjs'

const execute = promisify(execFile)
const OK = new Set([
  'requested',
  'already-covered',
  'not-configured',
  'skipped-not-open',
  'skipped-non-draft',
])
const RANK = [
  'already-covered',
  'requested',
  'skipped-conflicting',
  'unconfirmed',
  'dispatch-failed',
  'head-moved',
]
const PR_FIELDS = 'number,state,isDraft,mergeable,headRefOid,headRefName,baseRefName'

class UsageError extends Error {}

export function exitCodeFor(outcome) {
  return OK.has(outcome) ? 0 : 3
}

async function github(args, { cwd, timeoutMs }) {
  const { stdout } = await execute('gh', args, {
    cwd,
    timeout: timeoutMs,
    maxBuffer: 16 * 1024 * 1024,
  })
  return args[0] === 'workflow' || stdout.trim() === '' ? null : JSON.parse(stdout)
}

function options(argv) {
  try {
    const { values, positionals } = parseArgs({
      args: argv,
      strict: true,
      allowPositionals: true,
      options: {
        pr: { type: 'string' },
        head: { type: 'string' },
        repo: { type: 'string' },
        'settle-ms': { type: 'string', default: '30000' },
        'confirm-ms': { type: 'string', default: '60000' },
      },
    })
    const [command] = positionals
    if (positionals.length !== 1 || command !== 'request') throw new Error('expected request')
    const integer = (value, name, max) => {
      const n = Number(value)
      if (!/^\d+$/.test(value ?? '') || !Number.isSafeInteger(n) || n < 1 || n > max)
        throw new Error(`invalid --${name}`)
      return n
    }
    const pr = integer(values.pr, 'pr', Number.MAX_SAFE_INTEGER)
    if (values.head !== undefined && !/^[a-fA-F0-9]{40,64}$/.test(values.head))
      throw new Error('invalid --head')
    if (values.repo !== undefined && !/^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/.test(values.repo))
      throw new Error('invalid --repo')
    return {
      pr,
      head: values.head,
      repo: values.repo,
      settleMs: integer(values['settle-ms'], 'settle-ms', 30000),
      confirmMs: integer(values['confirm-ms'], 'confirm-ms', 60000),
    }
  } catch (error) {
    throw new UsageError(
      `${error instanceof Error ? error.message : error}; usage: ci-request.mjs request --pr <n> [--head <sha>] [--repo <owner/name>] [--settle-ms <ms>] [--confirm-ms <ms>]`,
    )
  }
}

function lastError(error) {
  const message = error?.stderr || (error instanceof Error ? error.message : String(error))
  return String(message).trim().split(/\r?\n/).at(-1)
}

export async function main(argv, deps = {}) {
  const opts = options(argv)
  const cwd = deps.cwd ?? process.cwd()
  const config = (deps.loadConfig ?? loadSkillConfig)({ cwd })
  const configured = config?.ci?.request?.workflows ?? []
  if (configured.length === 0) return { outcome: 'not-configured' }
  if (
    !Array.isArray(configured) ||
    configured.some((w) => typeof w !== 'string' || !/^[A-Za-z0-9._-]+\.ya?ml$/.test(w))
  )
    throw new UsageError('ci.request.workflows must contain workflow file names')
  const workflows = [...new Set(configured)]
  const now = deps.now ?? Date.now
  const sleep = deps.sleep ?? realSleep
  const gh = deps.gh ?? github
  const deadline = now() + opts.settleMs + opts.confirmMs + 30000
  const settleDeadline = now() + opts.settleMs
  const call = async (args) => {
    const remaining = deadline - now()
    if (remaining <= 0) throw new Error('CI request time budget exhausted')
    return gh(args, { cwd, timeoutMs: Math.max(1, Math.min(15000, remaining)) })
  }
  const pause = async (until) => {
    const remaining = Math.min(until, deadline) - now()
    if (remaining > 0) await sleep(Math.min(1000, remaining))
  }
  const repoArgs = opts.repo ? ['--repo', opts.repo] : []
  const view = () => call(['pr', 'view', String(opts.pr), ...repoArgs, '--json', PR_FIELDS])
  const paginated = async (endpoint, key) => {
    const items = []
    for (let page = 1; ; page++) {
      const result = await call([
        'api',
        `${endpoint}${endpoint.includes('?') ? '&' : '?'}per_page=100&page=${page}`,
      ])
      if (!Array.isArray(result?.[key])) throw new Error(`GitHub returned no ${key}`)
      items.push(...result[key])
      if (result[key].length < 100) return items
    }
  }
  let head = opts.head ?? null
  const results = []
  const finish = (outcome) => ({ outcome, pr: opts.pr, head, workflows: results })
  try {
    let pr = await view()
    const skip = (value) => {
      if (value?.state !== 'OPEN') return 'skipped-not-open'
      if (value.isDraft !== true) return 'skipped-non-draft'
      if (opts.head && value.headRefOid !== opts.head) return 'head-moved'
      if (value.mergeable === 'CONFLICTING') return 'skipped-conflicting'
      return null
    }
    head = pr?.headRefOid ?? head
    let skipped = skip(pr)
    if (skipped) return finish(skipped)
    while (pr.mergeable === 'UNKNOWN' && now() < settleDeadline) {
      await pause(settleDeadline)
      const next = await view()
      if (next.headRefOid !== head) return finish('head-moved')
      pr = next
      skipped = skip(pr)
      if (skipped) return finish(skipped)
    }
    if (!head || !pr.headRefName || !pr.baseRefName)
      throw new Error('GitHub returned incomplete PR refs')
    const repo =
      opts.repo ?? (await call(['repo', 'view', '--json', 'nameWithOwner']))?.nameWithOwner
    if (typeof repo !== 'string' || !/^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/.test(repo))
      throw new Error('GitHub returned no repository')
    const runsForHead = () =>
      paginated(`repos/${repo}/actions/runs?head_sha=${head}`, 'workflow_runs')
    let runs = await runsForHead()
    const priorIds = new Set(runs.map((r) => r.id))
    const matches = (run, workflow) =>
      run.path === `.github/workflows/${workflow}` && run.head_sha === head
    const coverage = async (workflow) => {
      for (;;) {
        let settling = false
        for (const run of runs.filter(
          (r) => matches(r, workflow) && r.conclusion !== 'cancelled',
        )) {
          if (run.event === 'workflow_dispatch') return run
          const jobs = await paginated(`repos/${repo}/actions/runs/${run.id}/jobs`, 'jobs')
          const coverageJobs = config.ci.request.coverageJobs?.[workflow]
          const eligible = jobs.filter((j) => !coverageJobs || coverageJobs.includes(j.name))
          if (eligible.some((j) => j.conclusion !== 'skipped')) return run
          if (eligible.length === 0 && run.status !== 'completed') settling = true
        }
        if (!settling || now() >= settleDeadline) return null
        await pause(settleDeadline)
        runs = await runsForHead()
      }
    }
    const pending = new Set()
    for (const workflow of workflows) {
      const covered = await coverage(workflow)
      if (covered) {
        results.push({ workflow, outcome: 'already-covered', runId: covered.id })
        continue
      }
      const current = await view()
      if (current.headRefOid !== head) return finish('head-moved')
      const reason = skip(current)
      if (reason) return finish(reason)
      try {
        await call([
          'workflow',
          'run',
          workflow,
          '--repo',
          repo,
          '--ref',
          pr.headRefName,
          '-f',
          `pr=${opts.pr}`,
          '-f',
          `base=${pr.baseRefName}`,
        ])
        results.push({ workflow, outcome: 'unconfirmed' })
        pending.add(workflow)
      } catch (error) {
        results.push({ workflow, outcome: 'dispatch-failed', error: lastError(error) })
      }
    }
    const confirmDeadline = Math.min(deadline, now() + opts.confirmMs)
    while (pending.size > 0) {
      const confirmed = await paginated(
        `repos/${repo}/actions/runs?head_sha=${head}&event=workflow_dispatch`,
        'workflow_runs',
      )
      for (const workflow of pending) {
        const found = confirmed.find(
          (r) =>
            r.event === 'workflow_dispatch' &&
            !priorIds.has(r.id) &&
            r.path === `.github/workflows/${workflow}`,
        )
        if (!found) continue
        const result = results.find((r) => r.workflow === workflow)
        result.outcome = found.head_sha === head ? 'requested' : 'head-moved'
        result.runId = found.id
        pending.delete(workflow)
      }
      if (results.some((r) => r.outcome === 'head-moved')) return finish('head-moved')
      if (pending.size === 0) break
      if ((await view()).headRefOid !== head) return finish('head-moved')
      if (now() >= confirmDeadline) break
      await pause(confirmDeadline)
    }
    return finish(
      results.reduce(
        (worst, r) => (RANK.indexOf(r.outcome) > RANK.indexOf(worst) ? r.outcome : worst),
        'already-covered',
      ),
    )
  } catch (error) {
    return { ...finish('dispatch-failed'), error: lastError(error) }
  }
}

if (isMainModule(import.meta.url)) {
  try {
    const result = await main(process.argv.slice(2))
    process.stdout.write(`${JSON.stringify(result)}\n`)
    process.exitCode = exitCodeFor(result.outcome)
  } catch (error) {
    process.stderr.write(`${error instanceof Error ? error.message : error}\n`)
    process.exitCode = error instanceof UsageError ? 2 : 3
  }
}
