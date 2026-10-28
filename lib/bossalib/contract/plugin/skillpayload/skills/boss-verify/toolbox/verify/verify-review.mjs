#!/usr/bin/env node

// verify-review.mjs — verify's own review of a head boss did not build, which stands in for its
// missing `boss/build` receipt (verify-gate's `review-required` verdict). Two calls around the
// review dispatches, in the shape repair/receipt-carry.mjs uses:
//
//   node verify-review.mjs plan --pr <n> --head <40-hex> [--repo <owner/name>]
//     prints {decision: 'review', review: {skills, base, head, outDirs, verdictFiles, hangGuardMs}}
//     or {decision: 'refused', reasons}
//     exit 0 review · 3 refused (a fork, a moved head, an already-receipted head, or a fact that
//     could not be read) · 2 usage error (stderr, nothing printed) · 1 internal error
//
//   node verify-review.mjs record --pr <n> --head <40-hex> [--review-verdict <file>]...
//                                 [--repo <owner/name>]
//     prints {decision: 'receipted'|'findings'|'unusable'|'failed', ...}
//     exit 0 receipted (`boss/build` success posted) · 3 findings or unusable (nothing posted)
//          1 the post itself failed (gh), or an internal error · 2 usage error
//
// The review chain and its time bound come from the base tip's config, never the head's, so a PR
// cannot pick its own reviewer. `record` pools the verdicts (default: every file `plan` named) and
// requires each one to have reviewed exactly the planned base and head. A clean pool posts
// `boss/build` success through commit-status.mjs, whose ready gate is the only judge of "clean on
// this tree"; a blocked pool writes its blocking findings in the shape `verify-gate post --verdict
// defect --findings` reads. Anything else is unusable: verdict `human`, reason `extension-failed`,
// because a review that malfunctions is a verify step that malfunctions, and retrying it every
// tick would only burn tokens.

import { mkdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import { join } from 'node:path'
import { spawnSync } from 'node:child_process'
import { parseArgs } from 'node:util'

import { main as commitStatus, RECEIPT_CONTEXT } from '../commit-status.mjs'
import { isMainModule } from '../main-module.mjs'
import { commandReader, readyGateReport } from '../build/ready-gate.mjs'
import { isBlocking, mergeVerdicts, verdictStatus, VERDICT_FILENAME } from '../review-verdict.mjs'
import { CONFIG_FILENAME, reviewSkillFor, reviewTimeDefaults } from '../skill-config.mjs'

export const DECISIONS = Object.freeze({
  REVIEW: 'review',
  REFUSED: 'refused',
  RECEIPTED: 'receipted',
  FINDINGS: 'findings',
  UNUSABLE: 'unusable',
  FAILED: 'failed',
})

// Every receipt this helper posts starts with this, next to bossd's `carried by bossd from `.
export const RECEIPT_DESCRIPTION_PREFIX = 'reviewed by boss-verify '

const SHA = /^[0-9a-f]{40}$/i
const sha = (value) => typeof value === 'string' && SHA.test(value)
const short = (value) => String(value).slice(0, 12)
const text = (value) => typeof value === 'string' && value.trim() !== ''

const refused = (reason) => ({ decision: DECISIONS.REFUSED, reasons: [reason] })

// The refusal the live PR view alone settles, or null. A fork is refused before anything else is
// looked at: no agent reviews untrusted code on this machine.
function viewRefusal(pr, head) {
  if (!pr) return refused('pr-unreadable')
  if (pr.isCrossRepository !== false) return refused(pr.isCrossRepository ? 'fork' : 'fork-unknown')
  if (String(pr.headRefOid ?? '').toLowerCase() !== String(head).toLowerCase())
    return refused('head-moved')
  return null
}

/**
 * The plan decision, from facts already read. `pr` is the live PR view (`null` when unreadable);
 * `receipt` is `absent`, `success`, `pending` or `unknown`; `base` the merge-base (null when it
 * could not be computed); `config` the base tip's parsed config (null when unreadable).
 * @param {{pr?: any, head?: string, receipt?: string, base?: string | null, config?: object | null}} facts
 */
export function decidePlan({ pr, head, receipt, base, config } = {}) {
  const early = viewRefusal(pr, head)
  if (early) return early
  if (receipt === 'success') return refused('already-receipted')
  if (receipt !== 'absent')
    return refused(`receipt-${receipt === 'pending' ? 'pending' : 'unknown'}`)
  if (!sha(base)) return refused('base-unknown')
  if (!config) return refused('base-config-unreadable')
  return { decision: DECISIONS.REVIEW, reasons: [] }
}

function receiptState(read) {
  if (read?.verdict === 'absent') return 'absent'
  if (read?.verdict !== 'present') return 'unknown'
  return read.state === 'success' || read.state === 'pending' ? read.state : 'absent'
}

// The base tip's config as parsed JSON: `{}` when the file is absent there, null when unreadable.
function baseConfig(read, ref) {
  const listed = read('git', ['ls-tree', '--name-only', ref, '--', CONFIG_FILENAME])
  if (!listed.ok) return null
  if (listed.output === '') return {}
  const shown = read('git', ['show', `${ref}:${CONFIG_FILENAME}`])
  if (!shown.ok) return null
  try {
    const parsed = JSON.parse(shown.output)
    return parsed && typeof parsed === 'object' && !Array.isArray(parsed) ? parsed : null
  } catch {
    return null
  }
}

function runDirFor(read, head) {
  const gitDir = read('git', ['rev-parse', '--absolute-git-dir'])
  return gitDir.ok && text(gitDir.output)
    ? join(gitDir.output, 'boss-verify-review', short(head).toLowerCase())
    : ''
}

function readJson(file) {
  try {
    return JSON.parse(readFileSync(file, 'utf8'))
  } catch {
    return null
  }
}

/**
 * Read the facts and plan the review: one fresh output directory per configured review skill under
 * the per-worktree run directory, and the plan itself recorded there for `record`.
 * options: {pr, head, repo}; deps: {spawn (git and gh), cwd, env}.
 */
export function planReview(options, deps = {}) {
  const spawn = deps.spawn ?? spawnSync
  const cwd = deps.cwd ?? process.cwd()
  const env = deps.env ?? process.env
  const read = commandReader({ spawn, cwd })
  const head = options.head.toLowerCase()
  const repoFlag = options.repo ? ['--repo', options.repo] : []
  const view = read(
    'gh',
    [
      'pr',
      'view',
      String(options.pr),
      ...repoFlag,
      '--json',
      'headRefOid,baseRefName,isCrossRepository',
    ],
    { json: true },
  )
  const pr = view.ok && text(view.payload?.baseRefName) ? view.payload : null
  const facts = { pr, head }
  // A fork, a moved head or an unreadable view is refused before any further read.
  const early = viewRefusal(pr, head)
  if (early) return { ...early, pr: options.pr, head }
  facts.receipt = receiptState(
    commitStatus(['read', '--sha', head, '--context', RECEIPT_CONTEXT, ...repoFlag], { spawn })
      .result,
  )
  const ref = `origin/${pr.baseRefName}`
  const base = read('git', ['merge-base', head, ref])
  facts.base = base.ok && sha(base.output) ? base.output.toLowerCase() : null
  facts.config = baseConfig(read, ref)
  const decided = decidePlan(facts)
  const report = { ...decided, pr: options.pr, head }
  if (decided.decision !== DECISIONS.REVIEW) return report
  const runDir = runDirFor(read, head)
  if (!runDir) return { ...report, decision: DECISIONS.REFUSED, reasons: ['git-dir-unknown'] }
  // A verdict left by an earlier run is never read as this run's.
  rmSync(runDir, { recursive: true, force: true })
  const skills = reviewSkillFor(facts.config).skills
  const outDirs = skills.map((_, i) => join(runDir, 'review', String(i + 1)))
  for (const dir of outDirs) mkdirSync(dir, { recursive: true })
  const review = {
    skills,
    base: facts.base,
    head,
    baseRef: pr.baseRefName,
    outDirs,
    verdictFiles: outDirs.map((dir) => join(dir, VERDICT_FILENAME)),
    hangGuardMs: reviewTimeDefaults(facts.config, env).hangGuardMs,
  }
  writeFileSync(join(runDir, 'plan.json'), `${JSON.stringify({ pr: options.pr, ...review })}\n`)
  return { ...report, review }
}

function readVerdict(file) {
  const parsed = readJson(file)
  return parsed === null
    ? { status: 'error', warnings: [], errors: [`cannot read ${file}`], verdict: null }
    : verdictStatus(parsed)
}

// A verdict's base and head may be abbreviated; each must name the planned commit.
const names = (planned, stated) =>
  text(stated) && String(planned).toLowerCase().startsWith(stated.trim().toLowerCase())

/**
 * The record decision over the pooled verdicts, before anything is posted: `clean`, `blocked`
 * (with the blocking findings) or `unusable` (with the reason).
 */
export function judgeVerdicts(plan, files) {
  if (files.length !== plan.skills.length) return { status: 'unusable', why: 'review-incomplete' }
  const results = files.map(readVerdict)
  const pooled = verdictStatus(mergeVerdicts(results))
  if (pooled.status === 'error') return { status: 'unusable', why: 'review-error' }
  const covers = results.every(
    ({ verdict }) => names(plan.base, verdict.base) && names(plan.head, verdict.head),
  )
  if (!covers) return { status: 'unusable', why: 'review-range-mismatch' }
  if (pooled.status === 'blocked')
    return { status: 'blocked', findings: pooled.verdict.findings.filter(isBlocking) }
  return { status: 'clean' }
}

/**
 * Pool the review verdicts for the planned head and act: post the receipt, write the findings, or
 * report the review unusable. options: {pr, head, reviewVerdicts, repo}; deps: {spawn, cwd,
 * readyGateReport}.
 */
export function recordReview(options, deps = {}) {
  const spawn = deps.spawn ?? spawnSync
  const gate = deps.readyGateReport ?? readyGateReport
  const cwd = deps.cwd ?? process.cwd()
  const read = commandReader({ spawn, cwd })
  const head = options.head.toLowerCase()
  const report = { pr: options.pr, head }
  const runDir = runDirFor(read, head)
  const plan = runDir ? readJson(join(runDir, 'plan.json')) : null
  const planned =
    plan &&
    String(plan.pr) === String(options.pr) &&
    plan.head === head &&
    Array.isArray(plan.skills)
  const files = options.reviewVerdicts?.length ? options.reviewVerdicts : (plan?.verdictFiles ?? [])
  const judged = planned ? judgeVerdicts(plan, files) : { status: 'unusable', why: 'no-plan' }
  const unusable = (why, extra = {}) => ({
    ...report,
    decision: DECISIONS.UNUSABLE,
    verdict: 'human',
    reason: 'extension-failed',
    why,
    note: `verify's review (${plan?.skills?.join(', ') || 'unplanned'}) of ${short(head)} is unusable: ${why}`,
    ...extra,
  })
  if (judged.status === 'unusable') return unusable(judged.why)
  if (judged.status === 'blocked') {
    const findings = judged.findings.map((f) => ({
      title: `${f.severity} ${f.title}`,
      file: f.file,
      line: f.line,
      detail: f.evidence,
    }))
    const findingsFile = join(runDir, 'findings.json')
    writeFileSync(findingsFile, `${JSON.stringify(findings)}\n`)
    return { ...report, decision: DECISIONS.FINDINGS, verdict: 'defect', findingsFile }
  }
  writeFileSync(join(runDir, 'review-verdict'), 'REVIEW_VERDICT=clean\n')
  writeFileSync(join(runDir, 'reviewed-head'), `${head}\n`)
  const posted = commitStatus(
    [
      'post',
      '--context',
      RECEIPT_CONTEXT,
      '--state',
      'success',
      '--sha',
      head,
      '--description',
      `${RECEIPT_DESCRIPTION_PREFIX}${short(head)}`,
      '--review-verdict-file',
      join(runDir, 'review-verdict'),
      '--reviewed-head-file',
      join(runDir, 'reviewed-head'),
      '--base',
      plan.baseRef,
      ...(options.repo ? ['--repo', options.repo] : []),
    ],
    { spawn, readyGateReport: (gateOptions) => gate(gateOptions, { spawn, cwd }) },
  ).result
  if (posted.verdict === 'posted') return { ...report, decision: DECISIONS.RECEIPTED, posted }
  if (posted.verdict === 'refused')
    return unusable(`ready-gate-refused:${('reasons' in posted ? posted.reasons : []).join(',')}`, {
      posted,
    })
  return { ...report, decision: DECISIONS.FAILED, posted }
}

export class UsageError extends Error {}

const OPTIONS = /** @type {const} */ ({
  pr: { type: 'string' },
  head: { type: 'string' },
  repo: { type: 'string' },
  'review-verdict': { type: 'string', multiple: true },
})

export function parseCli(args) {
  let values
  let positionals
  try {
    ;({ values, positionals } = parseArgs({
      args,
      options: OPTIONS,
      strict: true,
      allowPositionals: true,
    }))
  } catch (error) {
    throw new UsageError(`verify-review: ${error.message}`)
  }
  const [cmd, ...extra] = positionals
  if ((cmd !== 'plan' && cmd !== 'record') || extra.length > 0)
    throw new UsageError('verify-review: expected `plan` or `record`')
  if (!/^[1-9]\d*$/.test(String(values.pr ?? '')))
    throw new UsageError('verify-review: --pr must be a PR number')
  if (!sha(values.head))
    throw new UsageError('verify-review: --head must be a 40-character hex SHA')
  if (cmd === 'plan' && values['review-verdict'])
    throw new UsageError('verify-review: --review-verdict applies only to record')
  if (values.repo !== undefined && !/^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/.test(String(values.repo)))
    throw new UsageError('verify-review: --repo must be owner/name')
  return {
    verb: cmd,
    pr: Number(values.pr),
    head: values.head,
    repo: values.repo,
    reviewVerdicts: values['review-verdict'] ?? [],
  }
}

export function exitCodeFor(report) {
  if (report.decision === DECISIONS.REVIEW || report.decision === DECISIONS.RECEIPTED) return 0
  return report.decision === DECISIONS.FAILED ? 1 : 3
}

if (isMainModule(import.meta.url)) {
  try {
    const options = parseCli(process.argv.slice(2))
    const report = options.verb === 'plan' ? planReview(options) : recordReview(options)
    process.stdout.write(`${JSON.stringify(report)}\n`)
    process.exitCode = exitCodeFor(report)
  } catch (error) {
    process.stderr.write(`${error instanceof Error ? error.message : String(error)}\n`)
    process.exitCode = error instanceof UsageError ? 2 : 1
  }
}
