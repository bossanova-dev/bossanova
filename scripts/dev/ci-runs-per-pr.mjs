#!/usr/bin/env node

// Measures how many runs of one GitHub Actions workflow each merged PR cost, split by the
// PR's phase when the run started: before the PR existed, while it was a draft, and after
// it was last marked ready. With --classify it also says what each post-ready push changed
// (a rebase, a same-tree re-push, a fix), read from local git objects.
//
// This is the measurement BOS-2179 asks for: "bazel runs per merged PR", compared before and
// after a CI trigger change. See docs/release/ci.md.

import { execFileSync, spawnSync } from 'node:child_process'
import { writeFileSync } from 'node:fs'
import { parseArgs } from 'node:util'
import { isMainModule } from '../../skills-toolbox/main-module.mjs'

// GitHub's search and workflow-runs endpoints return at most 1,000 results per query.
export const RESULT_CAP = 1000
// Commits fetched per `git fetch`, so a long list of missing SHAs stays under argv limits.
const FETCH_BATCH = 50
const DATE = /^\d{4}-\d{2}-\d{2}$/
const TIMESTAMP = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}(:\d{2})?Z$/

const USAGE = `usage: ci-runs-per-pr.mjs --since <when> --until <when> [options]

Counts runs of one workflow per PR merged into --base between --since and --until, split into
pre-PR, draft and post-ready phases. <when> is a UTC date (YYYY-MM-DD, the whole day) or a UTC
timestamp (YYYY-MM-DDTHH:MM[:SS]Z).

  --created-after <when> count only PRs created at or after this time, so a PR merged after a
                         trigger change does not bring runs from before it

  --repo <owner/name>    repository (default: the current gh repository)
  --base <branch>        merge base branch (default: main)
  --workflow <file>      workflow file name (default: bazel.yml)
  --classify             classify each post-ready push from local git objects; fetches
                         missing commits from origin into this repository's object store
  --json                 print the summary as JSON
  --out <file>           write every post-ready run with its classification as JSON
`

export function parseCliArgs(argv) {
  const { values } = parseArgs({
    args: argv,
    strict: true,
    allowPositionals: false,
    options: {
      since: { type: 'string' },
      until: { type: 'string' },
      'created-after': { type: 'string' },
      repo: { type: 'string' },
      base: { type: 'string', default: 'main' },
      workflow: { type: 'string', default: 'bazel.yml' },
      classify: { type: 'boolean', default: false },
      json: { type: 'boolean', default: false },
      out: { type: 'string' },
      help: { type: 'boolean', default: false },
    },
  })
  if (values.help) return { ...values, from: '', to: '', createdAfter: null }
  const from = instant(values.since, 'since', false)
  const to = instant(values.until, 'until', true)
  if (from > to) throw new Error('--since must not be after --until')
  const createdAfter =
    values['created-after'] === undefined
      ? null
      : instant(values['created-after'], 'created-after', false)
  return { ...values, from, to, createdAfter, help: false }
}

/** A date names the whole UTC day: its first second, or its last when `endOfDay`. */
function instant(value, flag, endOfDay) {
  if (DATE.test(value ?? '')) return `${value}T${endOfDay ? '23:59:59' : '00:00:00'}Z`
  if (TIMESTAMP.test(value ?? '') && !Number.isNaN(Date.parse(value))) return iso(Date.parse(value))
  throw new Error(`--${flag} must be a UTC date (YYYY-MM-DD) or timestamp (YYYY-MM-DDTHH:MM[:SS]Z)`)
}

/**
 * Collect every result in [from, to] from an endpoint that caps each query at RESULT_CAP,
 * halving the window until each piece fits. `count` and `fetch` take ISO timestamps.
 */
export function fetchCapped({ from, to, count, fetch, cap = RESULT_CAP }) {
  const total = count(from, to)
  if (total === 0) return []
  const start = Date.parse(from)
  const end = Date.parse(to)
  if (total <= cap || end - start < 2000) {
    const items = fetch(from, to)
    if (items.length < Math.min(total, cap)) {
      throw new Error(
        `expected ${Math.min(total, cap)} results for ${from}..${to}, got ${items.length}`,
      )
    }
    return items
  }
  const mid = Math.floor((start + end) / 2000) * 1000
  return [
    ...fetchCapped({ from, to: iso(mid), count, fetch, cap }),
    ...fetchCapped({ from: iso(mid + 1000), to, count, fetch, cap }),
  ]
}

function iso(ms) {
  return new Date(ms).toISOString().replace('.000Z', 'Z')
}

const PR_QUERY = `query($q:String!,$c:String){search(query:$q,type:ISSUE,first:50,after:$c){
 issueCount pageInfo{hasNextPage endCursor}
 nodes{... on PullRequest{number headRefName createdAt mergedAt author{login}
  timelineItems(first:100,itemTypes:[READY_FOR_REVIEW_EVENT]){nodes{... on ReadyForReviewEvent{createdAt}}}}}}}`

export function fetchMergedPrs({ gh, repo, base, from, to }) {
  const search = (a, b) => `repo:${repo} is:pr is:merged base:${base} merged:${a}..${b}`
  const query = (q, cursor) => {
    const args = ['api', 'graphql', '-f', `query=${PR_QUERY}`, '-f', `q=${q}`]
    if (cursor) args.push('-f', `c=${cursor}`)
    return JSON.parse(gh(args)).data.search
  }
  const prs = fetchCapped({
    from,
    to,
    count: (a, b) => query(search(a, b)).issueCount,
    fetch: (a, b) => {
      const nodes = []
      let cursor = null
      do {
        const page = query(search(a, b), cursor)
        nodes.push(...page.nodes)
        cursor = page.pageInfo.hasNextPage ? page.pageInfo.endCursor : null
      } while (cursor)
      return nodes
    },
  })
  return prs.map((pr) => ({
    number: pr.number,
    branch: pr.headRefName,
    author: pr.author?.login ?? '',
    createdAt: pr.createdAt,
    mergedAt: pr.mergedAt,
    readyAts: pr.timelineItems.nodes.map((event) => event.createdAt).filter(Boolean),
  }))
}

export function fetchWorkflowRuns({ gh, repo, workflow, from, to }) {
  const page = (a, b, n) =>
    JSON.parse(
      gh([
        'api',
        `repos/${repo}/actions/workflows/${workflow}/runs?created=${a}..${b}&per_page=100&page=${n}`,
      ]),
    )
  const runs = fetchCapped({
    from,
    to,
    count: (a, b) => page(a, b, 1).total_count,
    fetch: (a, b) => {
      const items = []
      for (let n = 1; n <= RESULT_CAP / 100; n++) {
        const batch = page(a, b, n).workflow_runs
        items.push(...batch)
        if (batch.length < 100) break
      }
      return items
    },
  })
  return runs.map((run) => ({
    id: run.id,
    branch: run.head_branch,
    sha: run.head_sha,
    event: run.event,
    createdAt: run.created_at,
    startedAt: run.run_started_at,
    updatedAt: run.updated_at,
    conclusion: run.conclusion,
  }))
}

/** The time a PR last became ready: its last ready-for-review event, else its creation. */
export function readyAt(pr) {
  return pr.readyAts.length ? [...pr.readyAts].sort().at(-1) : pr.createdAt
}

/**
 * Assign each run on a merged PR's branch, up to the merge, to the phase it started in, and
 * list post-ready runs with the head that preceded them (the previous distinct SHA).
 */
export function bucketRuns(prs, runs) {
  const byBranch = new Map()
  for (const run of runs) {
    if (!byBranch.has(run.branch)) byBranch.set(run.branch, [])
    byBranch.get(run.branch).push(run)
  }
  const perPr = []
  const postReady = []
  for (const pr of prs) {
    const ready = readyAt(pr)
    const own = (byBranch.get(pr.branch) ?? [])
      .filter((run) => run.createdAt <= pr.mergedAt)
      .sort((a, b) => a.createdAt.localeCompare(b.createdAt))
    const counts = { prePr: 0, draft: 0, postReady: 0 }
    const shas = []
    for (const run of own) {
      const firstOfSha = !shas.includes(run.sha)
      if (firstOfSha) shas.push(run.sha)
      if (run.createdAt < pr.createdAt) counts.prePr++
      else if (run.createdAt <= ready) counts.draft++
      else {
        counts.postReady++
        const index = shas.indexOf(run.sha)
        postReady.push({
          pr: pr.number,
          run: run.id,
          sha: run.sha,
          prev: index > 0 ? shas[index - 1] : null,
          firstOfSha,
          neverDraft: pr.readyAts.length === 0,
          conclusion: run.conclusion,
          wallMinutes: wallMinutes(run),
        })
      }
    }
    perPr.push({ pr: pr.number, author: pr.author, runs: own, ...counts })
  }
  return { perPr, postReady }
}

function wallMinutes(run) {
  if (!run.startedAt || !run.updatedAt) return 0
  return Math.max(0, (Date.parse(run.updatedAt) - Date.parse(run.startedAt)) / 60000)
}

export function summarize({ perPr, postReady }) {
  const prs = perPr.length
  const runs = perPr.flatMap((pr) => pr.runs)
  const sum = (key) => perPr.reduce((total, pr) => total + pr[key], 0)
  const per = (n) => (prs ? round(n / prs) : 0)
  const conclusions = {}
  for (const run of runs) {
    const key = run.conclusion ?? 'pending'
    conclusions[key] = (conclusions[key] ?? 0) + 1
  }
  const classes = {}
  for (const run of postReady) {
    if (!run.class) continue
    const entry = (classes[run.class] ??= { runs: 0, wallMinutes: 0 })
    entry.runs++
    entry.wallMinutes += run.wallMinutes
  }
  for (const entry of Object.values(classes)) entry.wallMinutes = Math.round(entry.wallMinutes)
  return {
    prs,
    runs: runs.length,
    runsPerPr: per(runs.length),
    phasesPerPr: {
      prePr: per(sum('prePr')),
      draft: per(sum('draft')),
      postReady: per(sum('postReady')),
    },
    conclusions,
    wallMinutes: Math.round(runs.reduce((total, run) => total + wallMinutes(run), 0)),
    postReadyClasses: classes,
  }
}

function round(n) {
  return Math.round(n * 100) / 100
}

/**
 * What a post-ready push changed, relative to the head it replaced. `git` answers object
 * questions; `base` is the remote-tracking base ref the branch forks from.
 *
 *   same-sha-repeat   another run on a SHA that already ran (re-run, reopen)
 *   first-sha         the branch's first run started after ready (a PR opened non-draft)
 *   same-tree         a new commit with an identical tree (e.g. a message-only rewrite)
 *   fast-forward      new commits on top of the previous head
 *   rebase-clean      the same patches on a newer base that would have merged cleanly
 *   rebase-conflict   the same patches on a newer base the old head conflicted with
 *   rebase-edit-*     a newer base and changed patches (a fix, or a conflict resolution)
 *   rewrite-same-base changed patches on the same base (an amend or a fixup)
 */
export function classifyPush(run, git, base) {
  if (!run.firstOfSha) return 'same-sha-repeat'
  if (!run.prev) return 'first-sha'
  if (git.tree(run.sha) === git.tree(run.prev)) return 'same-tree'
  if (git.isAncestor(run.prev, run.sha)) return 'fast-forward'
  const oldBase = git.mergeBase(run.prev, base)
  const newBase = git.mergeBase(run.sha, base)
  if (oldBase === newBase) return 'rewrite-same-base'
  const conflict = git.conflicts(newBase, run.prev)
  const samePatches = git.patchIds(oldBase, run.prev) === git.patchIds(newBase, run.sha)
  if (samePatches) return conflict ? 'rebase-conflict' : 'rebase-clean'
  return conflict ? 'rebase-edit-conflict' : 'rebase-edit-clean'
}

export function localGit(cwd = process.cwd()) {
  const run = (args, input) =>
    spawnSync('git', ['-C', cwd, ...args], { input, maxBuffer: 1 << 28, encoding: 'utf8' })
  const out = (args) => {
    const result = run(args)
    if (result.status !== 0) throw new Error(`git ${args.join(' ')}: ${result.stderr.trim()}`)
    return result.stdout.trim()
  }
  return {
    has: (sha) => run(['cat-file', '-e', `${sha}^{commit}`]).status === 0,
    fetch: (shas) => run(['fetch', '-q', '--no-write-fetch-head', '--no-tags', 'origin', ...shas]),
    tree: (sha) => out(['rev-parse', `${sha}^{tree}`]),
    isAncestor: (a, b) => run(['merge-base', '--is-ancestor', a, b]).status === 0,
    mergeBase: (a, b) => out(['merge-base', a, b]),
    conflicts: (base, head) => {
      // merge-tree exits 1 for a conflicted merge and >1 when it could not merge at all.
      const result = run(['merge-tree', '--write-tree', base, head])
      if (result.status > 1)
        throw new Error(`git merge-tree ${base} ${head}: ${result.stderr.trim()}`)
      return result.status === 1
    },
    patchIds: (base, tip) => {
      const diff = out(['log', '-p', '--reverse', '--format=commit %H', `${base}..${tip}`])
      return run(['patch-id', '--stable'], `${diff}\n`)
        .stdout.split('\n')
        .filter(Boolean)
        .map((line) => line.split(' ')[0])
        .join(',')
    },
  }
}

/** Classify every post-ready run in place; a run whose commits cannot be read is `unavailable`. */
export function classifyAll(postReady, git, base) {
  const wanted = [...new Set(postReady.flatMap((run) => [run.sha, run.prev]).filter(Boolean))]
  const missing = wanted.filter((sha) => !git.has(sha))
  for (let i = 0; i < missing.length; i += FETCH_BATCH) git.fetch(missing.slice(i, i + FETCH_BATCH))
  for (const run of postReady) {
    const needed = [run.sha, run.prev].filter(Boolean)
    run.class = needed.every((sha) => git.has(sha)) ? classifyPush(run, git, base) : 'unavailable'
  }
  return postReady
}

export function formatSummary(summary, { repo, workflow, base, from, to, createdAfter }) {
  const phases = summary.phasesPerPr
  const created = createdAfter ? `, created at or after ${createdAfter}` : ''
  const lines = [
    `${workflow} runs for ${summary.prs} PRs merged into ${base} in ${repo}, ${from}..${to}${created}`,
    `runs per merged PR: ${summary.runsPerPr} (pre-PR ${phases.prePr}, draft ${phases.draft}, post-ready ${phases.postReady})`,
    `runs: ${summary.runs}, wall-minutes: ${summary.wallMinutes}, conclusions: ${Object.entries(
      summary.conclusions,
    )
      .map(([key, n]) => `${key} ${n}`)
      .join(', ')}`,
  ]
  const classes = Object.entries(summary.postReadyClasses).sort((a, b) => b[1].runs - a[1].runs)
  if (classes.length) {
    lines.push('post-ready pushes:')
    for (const [name, entry] of classes) {
      lines.push(
        `  ${name.padEnd(22)} ${String(entry.runs).padStart(5)} runs  ${entry.wallMinutes} wall-min`,
      )
    }
  }
  return `${lines.join('\n')}\n`
}

export function measure(options, { gh, git }) {
  const { from, to, createdAfter } = options
  const prs = fetchMergedPrs({ gh, repo: options.repo, base: options.base, from, to }).filter(
    (pr) => !createdAfter || pr.createdAt >= createdAfter,
  )
  if (!prs.length) return { summary: summarize({ perPr: [], postReady: [] }), postReady: [] }
  const earliest = prs.map((pr) => pr.createdAt).sort()[0]
  // Pre-PR runs start before the PR does; a day of slack catches the first push.
  const runsFrom = iso(Date.parse(earliest) - 24 * 60 * 60 * 1000)
  const runs = fetchWorkflowRuns({
    gh,
    repo: options.repo,
    workflow: options.workflow,
    from: runsFrom,
    to,
  })
  const bucketed = bucketRuns(prs, runs)
  if (options.classify) classifyAll(bucketed.postReady, git, `refs/remotes/origin/${options.base}`)
  return { summary: summarize(bucketed), postReady: bucketed.postReady }
}

function main() {
  try {
    const options = parseCliArgs(process.argv.slice(2))
    if (options.help) {
      process.stdout.write(USAGE)
      return
    }
    const gh = (args) => execFileSync('gh', args, { encoding: 'utf8', maxBuffer: 1 << 28 })
    const repo =
      options.repo ?? gh(['repo', 'view', '--json', 'nameWithOwner', '-q', '.nameWithOwner']).trim()
    const resolved = { ...options, repo }
    const { summary, postReady } = measure(resolved, { gh, git: localGit() })
    if (options.out) writeFileSync(options.out, `${JSON.stringify(postReady, null, 2)}\n`)
    process.stdout.write(
      options.json ? `${JSON.stringify(summary, null, 2)}\n` : formatSummary(summary, resolved),
    )
  } catch (error) {
    process.stderr.write(`ci-runs-per-pr: ${error.message}\n${USAGE}`)
    process.exitCode = 1
  }
}

if (isMainModule(import.meta.url)) main()
