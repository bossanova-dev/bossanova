#!/usr/bin/env node
import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { spawnSync } from 'node:child_process'
import { parseArgs } from 'node:util'
import { resolveCheckout } from '../git-checkout.mjs'
import { resolveBossBinary } from '../boss-binary.mjs'
import { isMainModule } from '../main-module.mjs'
import { checked, graphql, SKIP_REASONS } from './retro-signals.mjs'
import {
  MAX_PRS,
  RATE_WINDOW_DAYS,
  factory,
  mergeVerdict,
  escapeRows,
  laterFixRates,
  MAX_HOLD_HEADS,
  HOLD_MINUTES,
  JUDGE_GRACE_MINUTES,
  hasTerminalVerdict,
  receiptHeads,
  approvedHeads,
  mergesWithoutVerdict,
  unjudgedHeads,
  unnecessaryHolds,
  verifyCronHealth,
  secondModelHealth,
  buildReport,
  renderReport,
} from './retro-outcomes.mjs'

export const MAX_RATE_PRS = 1500
export const TRACE_TIMEOUT_MS = 20 * 60_000
export const LIST_TIMEOUT_MS = 5 * 60_000
const tracerPath = fileURLToPath(new URL('../verify/verify-escapes-trace.mjs', import.meta.url))
const day = 86400000
const pageSize = 20
const selection = `number title body state mergedAt closedAt headRefOid commits(last:20){pageInfo{hasPreviousPage} nodes{commit{oid statusCheckRollup{contexts(first:100){pageInfo{hasNextPage} nodes{... on StatusContext{context state description createdAt}}}}}}} reviews(first:30){pageInfo{hasNextPage} nodes{state body submittedAt author{__typename}}} reviewThreads(first:100){pageInfo{hasNextPage} nodes{comments(first:100){pageInfo{hasNextPage} nodes{createdAt author{__typename}}}}}`
const nodes = (connection) => connection?.nodes ?? []
function nestedTruncated(pr) {
  return (
    pr.commits?.pageInfo?.hasPreviousPage ||
    pr.reviews?.pageInfo?.hasNextPage ||
    pr.reviewThreads?.pageInfo?.hasNextPage ||
    nodes(pr.reviewThreads).some((t) => t.comments?.pageInfo?.hasNextPage) ||
    nodes(pr.commits).some((c) => c.commit?.statusCheckRollup?.contexts?.pageInfo?.hasNextPage)
  )
}
/** @param {string} repo @param {string | null} start @param {Function} run @param {{searchText?: string, limit?: number}} [options] */
function fetchPrs(repo, start, run, { searchText, limit = MAX_PRS } = {}) {
  const prs = []
  let cursor = '',
    truncated = false
  const query = `query($search:String!,$cursor:String){search(query:$search,type:ISSUE,first:${pageSize},after:$cursor){issueCount pageInfo{hasNextPage endCursor} nodes{... on PullRequest{${selection}}}}}`
  for (let page = 0; page < Math.ceil(limit / pageSize); page++) {
    const { search } = graphql(run, query, {
      search: searchText ?? `repo:${repo} is:pr updated:>=${start} sort:updated-desc`,
      ...(cursor ? { cursor } : {}),
    })
    if (!Array.isArray(search?.nodes) || typeof search.pageInfo?.hasNextPage !== 'boolean')
      throw new Error('malformed search')
    const rows = search.nodes.slice(0, limit - prs.length)
    prs.push(...rows)
    truncated ||= search.issueCount > limit || rows.some(nestedTruncated)
    if (!search.pageInfo.hasNextPage) break
    if (prs.length >= limit || page === Math.ceil(limit / pageSize) - 1) {
      truncated = true
      break
    }
    if (!search.pageInfo.endCursor || search.pageInfo.endCursor === cursor)
      throw new Error('missing pagination cursor')
    cursor = search.pageInfo.endCursor
  }
  return { prs, truncated }
}
function reason(error) {
  return [...SKIP_REASONS, 'status-history-limit', 'tracer-missing', 'tracer-failed'].includes(
    error?.reason,
  )
    ? error.reason
    : 'graphql-error'
}
/** @typedef {{status: 'failed', reason: string, detail?: string}} ReadFailure */
/** @param {any} error @returns {ReadFailure} */
const failed = (error) => ({
  status: 'failed',
  reason: reason(error),
  ...(error?.detail ? { detail: error.detail } : {}),
})
function fetchRatePrs(repo, window, run) {
  const unique = new Map()
  let truncated = false
  for (let start = Date.parse(window.start); start < Date.parse(window.end); start += 7 * day) {
    const end = Math.min(start + 7 * day, Date.parse(window.end))
    const result = fetchPrs(repo, null, run, {
      searchText: `repo:${repo} is:pr is:merged merged:${new Date(start).toISOString()}..${new Date(end).toISOString()}`,
      limit: Math.min(1000, MAX_RATE_PRS - unique.size),
    })
    truncated ||= result.truncated
    for (const pr of result.prs) unique.set(pr.number, pr)
    if (unique.size >= MAX_RATE_PRS) {
      truncated = true
      break
    }
  }
  return { merges: [...unique.values()].filter(factory).map(mergeVerdict), truncated }
}
/** @returns {ReadFailure | {status: 'ok', records: Record<string, any>[], skipped: Record<string, unknown>, truncated: boolean}} */
function readEscapes(repo, window, rateWindow, dryRun, run, exists) {
  if (!exists(tracerPath)) return { status: 'failed', reason: 'tracer-missing' }
  const invoke = (command, args, timeout) => {
    let result
    try {
      result = run(process.execPath, [tracerPath, command, ...args], {
        timeout,
        maxBuffer: 32 * 1024 * 1024,
      })
    } catch (error) {
      result = { error }
    }
    let data
    try {
      data = JSON.parse(String(result.stdout ?? ''))
    } catch {}
    const timeoutFailure = result.error?.code === 'ETIMEDOUT' || result.signal === 'SIGTERM'
    if (result.error || result.status !== 0 || !data) {
      const failure = data?.failures?.[0]
      throw Object.assign(new Error('escape tracer failed'), {
        reason: timeoutFailure
          ? 'timeout'
          : result.error?.code === 'ENOENT'
            ? 'gh-unavailable'
            : 'tracer-failed',
        detail: String(
          failure?.reason ?? failure?.operation ?? String(result.stderr ?? '').split(/\r?\n/)[0],
        ).slice(0, 200),
      })
    }
    return data
  }
  try {
    const trace = invoke(
      'trace',
      [
        '--since',
        window.start,
        '--until',
        window.end,
        '--repo',
        repo,
        ...(dryRun ? ['--dry-run'] : []),
      ],
      TRACE_TIMEOUT_MS,
    )
    const list = invoke(
      'list',
      [
        '--since',
        window.start < rateWindow.start ? window.start : rateWindow.start,
        '--repo',
        repo,
      ],
      LIST_TIMEOUT_MS,
    )
    if (!Array.isArray(trace.links) || !Array.isArray(list.escapes) || list.unreadable > 0)
      throw Object.assign(new Error('incomplete tracer records'), {
        reason: 'tracer-failed',
        detail:
          list.unreadable > 0
            ? `${list.unreadable} unreadable escape records`
            : 'malformed tracer output',
      })
    const unique = new Map()
    for (const record of [...list.escapes, ...trace.links]) {
      const key = `${record.original.number}/${record.fix.number}`
      if (!unique.has(key)) unique.set(key, record)
    }
    return {
      status: 'ok',
      records: [...unique.values()],
      skipped: trace.skipped ?? [],
      truncated: Boolean(trace.truncated),
    }
  } catch (error) {
    return failed(error)
  }
}

export function collect(options = {}, deps = {}) {
  const io = deps.fs ?? fs
  const execute =
    deps.run ?? ((cmd, args, opts) => spawnSync(cmd, args, { encoding: 'utf8', ...opts }))
  const now = new Date((deps.now ?? (() => new Date()))())
  let checkout, checkoutError
  try {
    checkout = resolveCheckout({ cwd: deps.cwd, run: execute })
  } catch (error) {
    checkoutError = error
  }
  const run = (cmd, args, opts) => execute(cmd, args, { ...opts, cwd: checkout?.root ?? deps.cwd })
  let repo, repoError
  let lastRunAt
  if (checkout)
    try {
      lastRunAt = JSON.parse(
        io.readFileSync(path.join(checkout.commonDir, 'boss-retro/state.json'), 'utf8'),
      ).lastRunAt
    } catch {}
  const requested = Date.parse(options.since ?? lastRunAt ?? '')
  const window = {
    start: new Date(
      Math.min(
        now.getTime(),
        Math.max(
          now.getTime() - 31 * day,
          Number.isFinite(requested) ? requested : now.getTime() - 7 * day,
        ),
      ),
    ).toISOString(),
    end: now.toISOString(),
  }
  /** @type {Record<string, Record<string, any>>} */
  const sections = {}
  let truncated = false,
    prs = [],
    heads = [],
    verdictDataUnavailable = false
  try {
    if (checkoutError) throw checkoutError
    repo =
      options.repo ??
      JSON.parse(checked(run, 'gh', ['repo', 'view', '--json', 'nameWithOwner'])).nameWithOwner
    if (!repo) throw Object.assign(new Error('missing repository'), { reason: 'no-repo' })
    const fetched = fetchPrs(repo, window.start, run)
    prs = fetched.prs
    truncated ||= fetched.truncated
    const judgeOptions = {
      now: now.getTime(),
      graceMinutes: options.graceMinutes ?? JUDGE_GRACE_MINUTES,
      window,
    }
    const histories = new Map()
    let historyReads = 0
    const history = (head) => {
      if (histories.has(head)) return histories.get(head)
      if (historyReads >= MAX_HOLD_HEADS) {
        truncated = true
        throw Object.assign(new Error('status history read limit reached'), {
          reason: 'status-history-limit',
        })
      }
      historyReads++
      const statuses = JSON.parse(
        checked(run, 'gh', ['api', `repos/${repo}/commits/${head}/statuses?per_page=100`]),
      )
      if (!Array.isArray(statuses)) throw new Error('malformed status history')
      truncated ||= statuses.length >= 100
      const normalized = statuses.map((status) => ({
        ...status,
        createdAt: status.createdAt ?? status.created_at,
      }))
      histories.set(head, normalized)
      return normalized
    }
    const mergeCandidates = mergesWithoutVerdict(prs, window).rows
    const unjudgedCandidates = unjudgedHeads(prs, judgeOptions).rows
    const candidates = new Set([
      ...mergeCandidates.map((head) => head.head),
      ...unjudgedCandidates.map((head) => head.head),
      ...receiptHeads(prs, window).map((head) => head.head),
    ])
    const unavailable = new Map()
    for (const pr of prs)
      for (const { commit } of nodes(pr.commits)) {
        const contexts = commit.statusCheckRollup?.contexts
        if (
          !candidates.has(commit.oid) ||
          hasTerminalVerdict(nodes(contexts)) ||
          unavailable.has(commit.oid)
        )
          continue
        try {
          const statuses = history(commit.oid)
          if (statuses.length >= 100 && !hasTerminalVerdict(statuses))
            throw Object.assign(new Error('terminal verdict history may be truncated'), {
              reason: 'status-history-limit',
            })
          commit.statusCheckRollup = {
            contexts: { nodes: [...nodes(contexts), ...statuses] },
          }
        } catch (error) {
          unavailable.set(commit.oid, error)
        }
      }
    const mergeError = mergeCandidates.find((head) => unavailable.has(head.head))
    const unjudgedError = unjudgedCandidates.find((head) => unavailable.has(head.head))
    sections.mergesWithoutVerdict = mergeError
      ? failed(unavailable.get(mergeError.head))
      : mergesWithoutVerdict(prs, window)
    sections.unjudgedHeads = unjudgedError
      ? failed(unavailable.get(unjudgedError.head))
      : unjudgedHeads(prs, judgeOptions)
    heads = receiptHeads(prs, window)
    verdictDataUnavailable = heads.some((head) => unavailable.has(head.head))
    sections.secondModelHealth = secondModelHealth(prs)
    try {
      const candidates = approvedHeads(prs, window)
      truncated ||= candidates.length > MAX_HOLD_HEADS
      const holdHistory = candidates
        .slice(0, MAX_HOLD_HEADS)
        .map((head) => ({ ...head, statuses: history(head.head) }))
      sections.unnecessaryHolds = unnecessaryHolds(holdHistory, prs, {
        holdMinutes: options.holdMinutes ?? HOLD_MINUTES,
        window,
      })
    } catch (error) {
      sections.unnecessaryHolds = failed(error)
    }
  } catch (error) {
    if (!repo) repoError = error
    verdictDataUnavailable = true
    for (const name of [
      'mergesWithoutVerdict',
      'unjudgedHeads',
      'unnecessaryHolds',
      'secondModelHealth',
    ])
      sections[name] = failed(error)
  }
  try {
    const binary = (deps.resolveBoss ?? resolveBossBinary)(deps.env ?? process.env, {
      fs: io,
      cwd: deps.cwd,
    })
    if (!binary.ok)
      throw Object.assign(new Error('unavailable boss'), { reason: 'boss-unavailable' })
    const env = JSON.parse(
      checked(run, path.resolve(deps.cwd ?? process.cwd(), binary.path), ['env', '--json']),
    )
    const repoId = env?.session?.repo_id
    if (!repoId) sections.verifyCronHealth = { status: 'skipped', reason: 'no-repo-id' }
    else {
      const data = JSON.parse(
        checked(run, path.resolve(deps.cwd ?? process.cwd(), binary.path), [
          'cron',
          'ls',
          '--repo',
          repoId,
          '--json',
        ]),
      )
      const jobs = Array.isArray(data) ? data : data.jobs
      if (!Array.isArray(jobs)) throw new Error('malformed cron list')
      sections.verifyCronHealth = verifyCronHealth(jobs, { now: now.getTime(), heads })
    }
  } catch (error) {
    sections.verifyCronHealth = failed(error)
  }
  const rateWindow = {
    start: new Date(now.getTime() - RATE_WINDOW_DAYS * day).toISOString(),
    end: window.end,
  }
  if (repoError || !repo) {
    sections.escapeRows = failed(repoError)
    sections.laterFixRates = failed(repoError)
  } else {
    const escapes = readEscapes(
      repo,
      window,
      rateWindow,
      options.dryRun,
      run,
      deps.existsSync ?? io.existsSync?.bind(io) ?? fs.existsSync,
    )
    truncated ||= escapes.status === 'ok' && escapes.truncated
    sections.escapeRows =
      escapes.status === 'ok'
        ? { ...escapeRows(escapes.records, window), skipped: escapes.skipped }
        : escapes
    if (escapes.status !== 'ok') sections.laterFixRates = escapes
    else
      try {
        const rates = fetchRatePrs(repo, rateWindow, run)
        truncated ||= rates.truncated
        sections.laterFixRates = laterFixRates(rates.merges, escapes.records, rateWindow)
      } catch (error) {
        sections.laterFixRates = failed(error)
      }
  }
  if (verdictDataUnavailable && sections.verifyCronHealth.status === 'ok')
    sections.verifyCronHealth = { ...sections.verifyCronHealth, verifySilent: null }
  return buildReport({ window, truncated, sections })
}

function flags(argv) {
  const parsed = parseArgs({
    args: argv,
    allowPositionals: true,
    strict: true,
    options: {
      since: { type: 'string' },
      'dry-run': { type: 'boolean' },
      repo: { type: 'string' },
      out: { type: 'string' },
      'hold-minutes': { type: 'string' },
      'grace-minutes': { type: 'string' },
    },
  })
  const [command, file, ...extra] = parsed.positionals
  const values = parsed.values
  if (command === 'render' && file && !extra.length && !Object.keys(values).length)
    return { command, file }
  if (command !== 'report' || file || !values.out)
    throw new Error('expected report --out <file> or render <report.json>')
  if (values.since && !Number.isFinite(Date.parse(values.since))) throw new Error('invalid --since')
  const options = { since: values.since, repo: values.repo, dryRun: values['dry-run'] ?? false }
  for (const [flag, key] of [
    ['hold-minutes', 'holdMinutes'],
    ['grace-minutes', 'graceMinutes'],
  ]) {
    if (values[flag] === undefined) continue
    const n = Number(values[flag])
    if (!/^\d+$/.test(values[flag]) || !Number.isSafeInteger(n) || n < 1)
      throw new Error(`invalid --${flag}`)
    options[key] = n
  }
  return { command, file: values.out, options }
}
export function main(argv, deps = {}) {
  const stdout = deps.stdout ?? ((s) => process.stdout.write(s))
  const stderr = deps.stderr ?? ((s) => process.stderr.write(s))
  let parsed
  try {
    parsed = flags(argv)
  } catch (error) {
    stderr(`retro-outcomes-report: ${error.message}\n`)
    return 64
  }
  const io = deps.fs ?? fs
  try {
    if (parsed.command === 'render')
      stdout(renderReport(JSON.parse(io.readFileSync(parsed.file, 'utf8'))))
    else io.writeFileSync(parsed.file, JSON.stringify(collect(parsed.options, deps)) + '\n')
    return 0
  } catch (error) {
    stderr(`retro-outcomes-report: ${error.message}\n`)
    return 1
  }
}
if (isMainModule(import.meta.url)) process.exitCode = main(process.argv.slice(2))
