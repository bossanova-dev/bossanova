#!/usr/bin/env node

// receipt-carry.mjs — may a repair pass's pushed head carry the start head's `boss/build` receipt,
// and if its push changed the reviewed tree, what must a review cover before it can?
//
//   node receipt-carry.mjs --start-sha <40-hex> --head <40-hex> [--base <branch>]
//                          [--review-verdict <file>]... [--target-url <url>] [--repo <owner/name>]
//     prints {decision, reasons, startSha, head[, baselineSha][, drift][, review][, posted]
//             [, findings][, reportFile]}
//     exit 0 posted (`carry`, `post`) · 3 nothing posted (`refused`, `blocked`)
//          4 `re-review`: review `review.base`...`review.head`, then re-run adding each verdict file
//          1 the post failed, or an internal error · 2 usage error (stderr, nothing printed)
//
// The start head's receipt certifies that its tree was reviewed clean, so it is the baseline. A
// start head with no receipt (an earlier push dropped it) takes its baseline from the newest
// receipted head in a run of the remote-tracking ref's reflog: pushes whose head this worktree made
// itself, ending at a fetch, a pull, or a push from another worktree. The first head not made here
// is still a candidate before the run stops, so a peer's push head can be the baseline, and at most
// 20 heads are read. The baseline is reported as `baselineSha`; the delta from it covers every
// commit since, so the receipt is re-earned rather than lost for good. A pushed head whose tree is the baseline replayed onto the pushed head's base
// carries it. One whose tree differs needs a review of exactly the delta no review saw: the commits
// since the baseline when it is an ancestor, else the diff from the baseline's automatic replay onto
// the new base (conflict markers included, as `git show --remerge-diff` shows them) to the pushed
// tree. That delta is materialised as two deterministic unreferenced commits so a `BASE...HEAD`
// review reads it. Every post goes through commit-status.mjs, whose ready gate re-checks the tree.

import { spawnSync } from 'node:child_process'
import { mkdirSync, readFileSync, writeFileSync } from 'node:fs'
import { join } from 'node:path'
import { parseArgs } from 'node:util'

import { main as commitStatus, RECEIPT_CONTEXT } from '../commit-status.mjs'
import { isMainModule } from '../main-module.mjs'
import { baseBranch, readyGateReport, reviewedTreeDrift } from '../build/ready-gate.mjs'
import {
  isBlocking,
  mergeVerdicts,
  renderReport,
  verdictStatus,
  VERDICT_FILENAME,
} from '../review-verdict.mjs'
import { loadSkillConfig, reviewSkillFor, reviewTimeDefaults } from '../skill-config.mjs'

export const DECISIONS = Object.freeze({
  CARRY: 'carry',
  RE_REVIEW: 're-review',
  POST: 'post',
  BLOCKED: 'blocked',
  REFUSED: 'refused',
})

const SHA = /^[0-9a-f]{40}$/i
const sha = (value) => typeof value === 'string' && SHA.test(value)
const short = (value) => String(value).slice(0, 12)

// One identity and timestamp for the synthetic delta commits, so the same inputs always name the
// same SHAs and the verdict's base can be compared exactly on the second call.
const SYNTHETIC_ENV = Object.freeze({
  GIT_AUTHOR_NAME: 'boss-repair',
  GIT_AUTHOR_EMAIL: 'boss-repair@localhost',
  GIT_AUTHOR_DATE: '1000000000 +0000',
  GIT_COMMITTER_NAME: 'boss-repair',
  GIT_COMMITTER_EMAIL: 'boss-repair@localhost',
  GIT_COMMITTER_DATE: '1000000000 +0000',
})

/**
 * The decision, from facts already read.
 * - `startReceipt`: `success` (present with state success), `absent`, or `unknown`.
 * - `gateReady`: the default ready gate (the review verdict for the reviewed head, plus tree drift
 *   against it) calls the head ready.
 * - `drift`: the pushed head against the start head, as `reviewedTreeDrift` reads it.
 * - `range`: the delta to review, or null when it could not be computed.
 * - `review`: absent before a review, else `{status, rangeMatches}`.
 */
export function decideCarry({ startReceipt, gateReady, drift, range, review } = {}) {
  if (startReceipt !== 'success')
    return {
      decision: DECISIONS.REFUSED,
      reasons: [startReceipt === 'absent' ? 'start-head-not-receipted' : 'start-receipt-unknown'],
    }
  if (gateReady === true)
    return { decision: DECISIONS.CARRY, reasons: [], baseline: 'review-state' }
  if (drift === 'none') return { decision: DECISIONS.CARRY, reasons: [], baseline: 'start-receipt' }
  if (drift !== 'changed') return { decision: DECISIONS.REFUSED, reasons: ['tree-drift-unknown'] }
  if (!range) return { decision: DECISIONS.REFUSED, reasons: ['review-range-unknown'] }
  if (!review) return { decision: DECISIONS.RE_REVIEW, reasons: ['tree-changed-since-review'] }
  if (review.status !== 'pass' && review.status !== 'blocked')
    return { decision: DECISIONS.REFUSED, reasons: ['review-unusable'] }
  if (review.rangeMatches !== true)
    return { decision: DECISIONS.REFUSED, reasons: ['review-range-mismatch'] }
  if (review.status === 'blocked')
    return { decision: DECISIONS.BLOCKED, reasons: ['review-blocked'] }
  return { decision: DECISIONS.POST, reasons: [], baseline: 'review' }
}

// A reader in ready-gate.mjs's shape that keeps stdout on failure too: a conflicted
// `git merge-tree --write-tree` exits 1 and still prints the tree it wrote.
function commandRunner({ spawn = spawnSync, cwd = process.cwd(), env = process.env } = {}) {
  return (command, args, { json = false, extraEnv } = {}) => {
    let response
    try {
      response = spawn(command, args, {
        cwd,
        encoding: 'utf8',
        timeout: 30000,
        maxBuffer: 64 * 1024 * 1024,
        killSignal: 'SIGKILL',
        ...(extraEnv ? { env: { ...env, ...extraEnv } } : {}),
      })
    } catch {
      return { ok: false, status: null, output: '' }
    }
    const output = String(response?.stdout ?? '').trim()
    if (response?.error || response?.status !== 0)
      return { ok: false, status: response?.status ?? null, output }
    if (!json) return { ok: true, status: 0, output }
    try {
      return { ok: true, status: 0, output, payload: JSON.parse(output) }
    } catch {
      return { ok: false, status: 0, output }
    }
  }
}

function oid(read, args) {
  const result = read('git', args)
  const value = result.ok ? result.output.split('\n')[0].trim() : ''
  return sha(value) ? value.toLowerCase() : null
}

/**
 * The delta no review saw, as `{base, head, replayed, conflicted}` refs a `BASE...HEAD` review
 * reads, or null. A start head that is an ancestor of the pushed head needs only the repair
 * commits. Otherwise the start head's replay onto the pushed head's merge base (`git merge-tree`,
 * whose conflicted result keeps the markers) is committed on that base, and the pushed tree on top.
 * `base` is a branch or a function returning one, called only when the replay is needed.
 */
export function reviewRange(read, base, startSha, head) {
  if (!sha(startSha) || !sha(head)) return null
  const start = startSha.toLowerCase()
  const pushed = head.toLowerCase()
  if (read('git', ['merge-base', '--is-ancestor', start, pushed]).ok)
    return { base: start, head: pushed, replayed: false, conflicted: false }
  const branch = typeof base === 'function' ? base() : base
  if (typeof branch !== 'string' || branch.trim() === '') return null
  const remote = `origin/${branch.trim().replace(/^origin\//, '')}`
  const startBase = oid(read, ['merge-base', remote, start])
  const pushedBase = oid(read, ['merge-base', remote, pushed])
  const pushedTree = oid(read, ['rev-parse', '--verify', '--quiet', `${pushed}^{tree}`])
  if (!startBase || !pushedBase || !pushedTree) return null
  const replay = read('git', [
    'merge-tree',
    '--write-tree',
    `--merge-base=${startBase}`,
    pushedBase,
    start,
  ])
  if (!replay.ok && replay.status !== 1) return null
  const replayTree = replay.output.split('\n')[0].trim()
  if (!sha(replayTree)) return null
  const commit = (tree, parent, message) => {
    const result = read(
      'git',
      ['commit-tree', '--no-gpg-sign', tree, '-p', parent, '-m', message],
      {
        extraEnv: SYNTHETIC_ENV,
      },
    )
    return result.ok && sha(result.output) ? result.output.toLowerCase() : null
  }
  const replayed = commit(
    replayTree,
    pushedBase,
    `boss-repair: reviewed head ${short(start)} replayed onto ${short(pushedBase)}`,
  )
  if (!replayed) return null
  const tip = commit(pushedTree, replayed, `boss-repair: tree of pushed head ${short(pushed)}`)
  if (!tip) return null
  return { base: replayed, head: tip, replayed: true, conflicted: !replay.ok }
}

function readVerdict(file) {
  try {
    return verdictStatus(JSON.parse(readFileSync(file, 'utf8')))
  } catch (error) {
    return { status: 'error', warnings: [], errors: [`cannot read ${file}: ${error.message}`] }
  }
}

/**
 * The verdict files, pooled as one verdict, and whether each one reviewed this whole range on its
 * own: its base is the range base and its head carries the pushed head's tree. The pool's range
 * runs from the first base to the last head, so it cannot stand in for a per-file check.
 */
export function judgeReview(read, { files, range, head }) {
  const verdicts = files.map(readVerdict)
  const result = verdictStatus(mergeVerdicts(verdicts))
  if (result.status === 'error') return { status: 'error', rangeMatches: false, result }
  const tree = (ref) => oid(read, ['rev-parse', '--verify', '--quiet', `${ref}^{tree}`])
  const pushedTree = tree(head)
  const heads = verdicts.map(({ verdict }) => {
    const verdictHead = oid(read, ['rev-parse', '--verify', '--quiet', `${verdict.head}^{commit}`])
    const covers =
      range.base.startsWith(String(verdict.base).toLowerCase()) &&
      verdictHead !== null &&
      pushedTree !== null &&
      tree(verdictHead) === pushedTree
    return covers ? verdictHead : null
  })
  const rangeMatches = heads.every((verdictHead) => verdictHead !== null)
  return { status: result.status, rangeMatches, result, verdictHead: heads.at(-1) }
}

function startReceiptState(read) {
  if (read?.verdict === 'present') return read.state === 'success' ? 'success' : 'absent'
  return read?.verdict === 'absent' ? 'absent' : 'unknown'
}

// How many of the branch's pushed heads the baseline lookup reads a receipt for, newest first.
const BASELINE_LOOKBACK = 20

/**
 * Whether this worktree's HEAD reflog shows it made `target`: the oldest entry at `target` is a
 * commit, cherry-pick, revert, or rebase step other than its start, not a fast-forward, merge, pull,
 * reset or checkout onto a commit made elsewhere. HEAD's reflog is per-worktree; the remote-tracking
 * reflog is shared by every worktree of the clone.
 */
function authoredHere(headLog, target) {
  let arrival
  for (const line of headLog) {
    const [oidText = '', ...subject] = line.trim().split(' ')
    if (oidText.toLowerCase() === target) arrival = subject.join(' ')
  }
  if (arrival === undefined) return false
  if (/^(cherry-pick|rebase[^:]*): fast-forward$/.test(arrival)) return false
  if (/^(commit|cherry-pick|revert)/.test(arrival)) return true
  return arrival.startsWith('rebase') && !arrival.includes('(start)')
}

/**
 * The newest head the current branch pushed, other than `skip`, whose receipt reads `success`, or
 * null; `receiptOf(sha)` reads one head's receipt state. Candidates are the remote-tracking ref's
 * reflog, newest first, while each entry is a push (`update by push`) and every newer pushed head
 * was made in this worktree: a fetch or pull, or a push from another worktree of the clone, ends
 * the run. Commits a pushed head carries are not checked one by one, so one merged in from
 * FETCH_HEAD stays inside the run; the review range from the baseline covers it.
 */
export function receiptedBaseline(read, receiptOf, skip = []) {
  const branch = read('git', ['symbolic-ref', '--quiet', '--short', 'HEAD'])
  if (!branch.ok || branch.output === '') return null
  const log = read('git', [
    'reflog',
    'show',
    '--format=%H %gs',
    `refs/remotes/origin/${branch.output}`,
  ])
  const headLog = read('git', ['reflog', 'show', '--format=%H %gs', 'HEAD'])
  if (!log.ok || !headLog.ok) return null
  const headLines = headLog.output.split('\n')
  const seen = new Set(skip.map((value) => String(value).toLowerCase()))
  const candidates = []
  for (const line of log.output.split('\n')) {
    const [oidText = '', ...subject] = line.trim().split(' ')
    if (subject.join(' ') !== 'update by push') break
    const value = oidText.toLowerCase()
    if (!sha(value)) break
    if (!seen.has(value)) {
      seen.add(value)
      candidates.push(value)
      if (candidates.length === BASELINE_LOOKBACK) break
    }
    if (!authoredHere(headLines, value)) break
  }
  return candidates.find((candidate) => receiptOf(candidate) === 'success') ?? null
}

function refuse(report, reason) {
  return { ...report, decision: DECISIONS.REFUSED, reasons: [reason] }
}

/**
 * Read the facts, decide, and act on the decision: post on `carry`/`post`, prepare the review
 * directories on `re-review`, write the review report on `blocked`.
 * options: {startSha, head, base, reviewVerdicts, targetUrl, repo};
 * deps: {spawn (git and gh), cwd, env, loadConfig}.
 */
export function carryReport(options, deps = {}) {
  const spawn = deps.spawn ?? spawnSync
  const cwd = deps.cwd ?? process.cwd()
  const env = deps.env ?? process.env
  const read = commandRunner({ spawn, cwd, env })
  const gateDeps = { spawn, cwd }
  const startSha = options.startSha.toLowerCase()
  const head = options.head.toLowerCase()
  const repoFlag = options.repo ? ['--repo', options.repo] : []
  let branch
  const base = () => {
    if (branch === undefined) branch = options.base ?? baseBranch(read)
    return branch
  }

  const receiptOf = (target) =>
    startReceiptState(
      commitStatus(['read', '--sha', target, '--context', RECEIPT_CONTEXT, ...repoFlag], { spawn })
        .result,
    )
  const facts = { startReceipt: receiptOf(startSha) }
  const report = { startSha, head }
  let baseline = startSha
  if (facts.startReceipt !== 'success') {
    const found = receiptedBaseline(read, receiptOf, [startSha, head])
    if (found) {
      baseline = found
      facts.startReceipt = 'success'
      report.baselineSha = found
    }
  }
  if (facts.startReceipt === 'success') {
    facts.gateReady = readyGateReport({ head, base: options.base }, gateDeps).ready === true
    if (!facts.gateReady) {
      facts.drift = reviewedTreeDrift(read, base, baseline, head)
      report.drift = facts.drift
      if (facts.drift === 'changed') facts.range = reviewRange(read, base, baseline, head)
    }
  }
  const files = options.reviewVerdicts ?? []
  const judged =
    facts.range && files.length > 0 ? judgeReview(read, { files, range: facts.range, head }) : null
  if (judged) facts.review = { status: judged.status, rangeMatches: judged.rangeMatches }
  const decided = decideCarry(facts)
  Object.assign(report, decided)

  const gitDir = read('git', ['rev-parse', '--absolute-git-dir'])
  const stateRoot = gitDir.ok ? join(gitDir.output, 'boss-repair') : ''
  const headDir = (kind) => {
    const dir = join(stateRoot, kind, short(head))
    mkdirSync(dir, { recursive: true })
    return dir
  }

  if (decided.decision === DECISIONS.RE_REVIEW) {
    if (!stateRoot) return refuse(report, 'git-dir-unknown')
    const config = (deps.loadConfig ?? (() => loadSkillConfig({ cwd })))()
    const skills = reviewSkillFor(config).skills
    const root = headDir('review')
    const outDirs = skills.map((_, i) => join(root, String(i + 1)))
    for (const dir of outDirs) mkdirSync(dir, { recursive: true })
    report.review = {
      base: facts.range.base,
      head: facts.range.head,
      replayed: facts.range.replayed,
      conflicted: facts.range.conflicted,
      skills,
      outDirs,
      verdictFiles: outDirs.map((dir) => join(dir, VERDICT_FILENAME)),
      hangGuardMs: reviewTimeDefaults(config, env).hangGuardMs,
    }
    return report
  }

  if (judged && judged.status !== 'error') {
    report.findings = judged.result.verdict.findings.filter(isBlocking)
    report.secondModel = judged.result.verdict.secondModel
  }
  if (decided.decision === DECISIONS.BLOCKED && stateRoot) {
    report.reportFile = join(headDir('receipt'), 'review-report.md')
    writeFileSync(report.reportFile, `${renderReport(judged.result)}\n`)
  }
  if (decided.decision !== DECISIONS.CARRY && decided.decision !== DECISIONS.POST) return report

  const post = [
    'post',
    '--context',
    RECEIPT_CONTEXT,
    '--state',
    'success',
    '--sha',
    head,
    '--description',
    decided.decision === DECISIONS.POST
      ? `Automatic repair pushed ${short(head)}; the changes since ${short(baseline)} were reviewed`
      : `Automatic repair pushed ${short(head)}`,
    ...(options.targetUrl ? ['--target-url', options.targetUrl] : []),
    ...repoFlag,
  ]
  // Off the worktree's review state, the gate is fed the baseline this decision rests on, so it
  // still re-checks that the pushed tree is the one that baseline reviewed.
  if (decided.baseline !== 'review-state') {
    if (!stateRoot) return refuse(report, 'git-dir-unknown')
    const dir = headDir('receipt')
    const reviewedHead = decided.decision === DECISIONS.POST ? judged.verdictHead : baseline
    writeFileSync(join(dir, 'review-verdict'), 'REVIEW_VERDICT=clean\n')
    writeFileSync(join(dir, 'reviewed-head'), `${reviewedHead}\n`)
    post.push('--review-verdict-file', join(dir, 'review-verdict'))
    post.push('--reviewed-head-file', join(dir, 'reviewed-head'))
    if (base()) post.push('--base', base())
  }
  report.posted = commitStatus(post, {
    spawn,
    readyGateReport: (gateOptions) =>
      readyGateReport({ ...gateOptions, base: gateOptions.base ?? options.base }, gateDeps),
  }).result
  return report
}

export class UsageError extends Error {}

const OPTIONS = {
  'start-sha': { type: 'string' },
  head: { type: 'string' },
  base: { type: 'string' },
  'review-verdict': { type: 'string', multiple: true },
  'target-url': { type: 'string' },
  repo: { type: 'string' },
}

export function parseCli(args) {
  let values
  try {
    ;({ values } = parseArgs({ args, options: OPTIONS, strict: true, allowPositionals: false }))
  } catch (error) {
    throw new UsageError(`receipt-carry: ${error.message}`)
  }
  for (const name of ['start-sha', 'head'])
    if (!sha(values[name]))
      throw new UsageError(`receipt-carry: --${name} must be a 40-character hex SHA`)
  const optional = (value) => (typeof value === 'string' && value.trim() !== '' ? value : undefined)
  return {
    startSha: values['start-sha'],
    head: values.head,
    base: optional(values.base),
    reviewVerdicts: values['review-verdict'] ?? [],
    targetUrl: optional(values['target-url']),
    repo: optional(values.repo),
  }
}

export function exitCodeFor(report) {
  if (report.decision === DECISIONS.RE_REVIEW) return 4
  if (report.decision !== DECISIONS.CARRY && report.decision !== DECISIONS.POST) return 3
  if (report.posted?.verdict === 'posted') return 0
  return report.posted?.verdict === 'refused' ? 3 : 1
}

if (isMainModule(import.meta.url)) {
  try {
    const report = carryReport(parseCli(process.argv.slice(2)))
    process.stdout.write(`${JSON.stringify(report)}\n`)
    process.exitCode = exitCodeFor(report)
  } catch (error) {
    process.stderr.write(`${error instanceof Error ? error.message : String(error)}\n`)
    process.exitCode = error instanceof UsageError ? 2 : 1
  }
}
