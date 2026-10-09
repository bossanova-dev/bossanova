// boss-build's Step 12 helper: the session-side merge envelope, and the verify fast path that judges,
// posts and merges through the sibling verify-gate.mjs — the one head-pinned merge path.
import { spawnSync } from 'node:child_process'
import { readFileSync, writeFileSync, mkdirSync } from 'node:fs'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { isMainModule } from './main-module.mjs'
import { classifyChecks, isGreen } from './pr-check-state.mjs'
import { STAGE_SKILL, invokedSkill } from './stage-chain.mjs'
import {
  ciReasons,
  criteriaReasons,
  followUpReasons,
  headReasons,
  mergeStateReasons,
  parseFollowUps,
  parseLedger,
  prShapeReasons,
  sectionToken,
  sections,
} from './merge-eligibility.mjs'

// Resolved beside this file, so the vendored boss-build toolbox calls its own vendored copy.
export const VERIFY_GATE = fileURLToPath(new URL('./verify-gate.mjs', import.meta.url))
// verify-gate's merge verb chains several reads, the daemon merge and the tracker writes.
const VERIFY_GATE_TIMEOUT_MS = 180000

const SHA = /^[0-9a-f]{40}$/i
const text = (value) => typeof value === 'string' && value.trim() !== ''
const sha = (value) => typeof value === 'string' && SHA.test(value)
export const INELIGIBLE_REASONS = Object.freeze([
  'outcome-not-review-ready',
  'no-session',
  'no-pr',
  'repo-unknown',
  'review-not-clean',
  'review-verdict-unknown',
  'open-findings',
  'coverage-not-full',
  'coverage-unknown',
  'cross-model-error',
  'cross-model-unknown',
  'criteria-unmet',
  'criteria-unknown',
  'ci-not-settled',
  'ci-not-green-on-head',
  'merge-state-unknown',
  ...['CLEAN', 'HAS_HOOKS', 'UNSTABLE', 'BLOCKED', 'BEHIND', 'DRAFT', 'DIRTY', 'UNKNOWN'].map(
    (s) => `merge-state-${s.toLowerCase()}`,
  ),
  'pr-not-open',
  'pr-draft',
  'do-not-merge-marker',
  'head-mismatch',
  'head-unknown',
  'tree-changed-since-review',
  'tree-drift-unknown',
  'human-follow-up-open',
  'follow-up-section-missing',
  'open-questions-open',
  'open-questions-section-missing',
  'epic-child',
  'launch-origin-unknown',
  'foreign-pr-watcher',
  'pr-watchers-unknown',
  'no-verify-cron',
  'verify-consent-unknown',
])

// Merge consent (D12): the repo has opted into the verify stage when an enabled cron job for this
// repo runs `/boss-verify` (or codex `$boss-verify`). The matcher is stage-chain.mjs's
// `invokedSkill`, the one prefix-safe prompt matcher: `/boss-verify-x` and `/boss-verifyx` are
// other prompts.
export function classifyVerifyConsent({ jobs, repoId } = {}) {
  if (!text(repoId)) return 'unknown'
  if (
    !Array.isArray(jobs) ||
    jobs.some((job) => !job || typeof job !== 'object' || Array.isArray(job))
  )
    return 'unknown'
  const consenting = (job) =>
    job.enabled === true &&
    job.repo_id === repoId &&
    invokedSkill(job.prompt) === STAGE_SKILL.verify
  return jobs.some(consenting) ? 'present' : 'absent'
}

// The PR body parsers live in merge-eligibility.mjs; parseFollowUps stays exported from here for
// the `followups` verb and existing importers.
export { parseFollowUps }
export function classifyLaunchOrigin(report) {
  const mode = typeof report === 'string' ? report : report?.mode
  if (mode === 'cron' || mode === 'managed') return 'standalone'
  if (mode === 'unattended') return 'epic-child'
  return 'launch-origin-unknown'
}
export function classifyWatchers({
  callbacksAvailable,
  watches,
  targetChatId,
  listingOk = true,
  pr,
} = {}) {
  if (callbacksAvailable === false) return 'not-checked'
  if (callbacksAvailable !== true || !listingOk || !Array.isArray(watches) || !text(targetChatId))
    return 'pr-watchers-unknown'
  for (const watch of watches) {
    if (!watch || typeof watch !== 'object') return 'pr-watchers-unknown'
    if (!Number.isInteger(watch.pr_number) || watch.pr_number < 1) return 'pr-watchers-unknown'
    if (pr && watch.pr_number !== pr) continue
    if (
      ['delivered', 'expired', 'cancelled', 'canceled', 'failed'].includes(
        String(watch.state).toLowerCase(),
      )
    )
      continue
    if (
      !['active', 'triggered', 'leased'].includes(String(watch.state).toLowerCase()) ||
      !text(watch.target_chat_id)
    )
      return 'pr-watchers-unknown'
    if (watch.target_chat_id !== targetChatId) return 'foreign-pr-watcher'
  }
  return 'own-chat-only'
}

export function computeEligibility(inputs = {}) {
  const i = inputs ?? {}
  const reasons = []
  const add = (reason) => reasons.push(reason)
  if (i.outcome !== 'REVIEW_READY') add('outcome-not-review-ready')
  if (!text(i.sessionId)) add('no-session')
  if (!Number.isInteger(i.pr) || i.pr < 1) add('no-pr')
  if (typeof i.repo !== 'string' || !/^[\w.-]+\/[\w.-]+$/.test(i.repo)) add('repo-unknown')
  if (!text(i.reviewVerdict)) add('review-verdict-unknown')
  else if (i.reviewVerdict !== 'clean') add('review-not-clean')
  if (i.openFindings !== 0) add('open-findings')
  if (!text(i.reviewCoverage)) add('coverage-unknown')
  else if (!(
    i.reviewCoverage === 'full' ||
    (/^quick:\s*\S/.test(i.reviewCoverage) && !/reduced\s*\(/i.test(i.reviewCoverage))
  ))
    add('coverage-not-full')
  if (!text(i.crossModelReview)) add('cross-model-unknown')
  else if (/^error:/i.test(i.crossModelReview)) add('cross-model-error')
  reasons.push(...criteriaReasons(i.criteria))
  reasons.push(
    ...ciReasons({
      ciWaitState: i.ciWaitState,
      checkVerdict: i.checkVerdict,
      headSha: i.pushedHead,
    }),
  )
  reasons.push(...mergeStateReasons(i.prView))
  reasons.push(...prShapeReasons(i.prView))
  reasons.push(...headReasons([i.pushedHead, i.localHead, i.upstreamHead, i.prView?.headRefOid]))
  if (i.treeDrift === 'changed') add('tree-changed-since-review')
  else if (i.treeDrift !== 'none') add('tree-drift-unknown')
  reasons.push(
    ...followUpReasons({ humanFollowUp: i.humanFollowUp, openQuestions: i.openQuestions }),
  )
  if (i.launchOrigin === 'epic-child') add('epic-child')
  else if (i.launchOrigin !== 'standalone') add('launch-origin-unknown')
  if (i.watchers === 'foreign-pr-watcher') add('foreign-pr-watcher')
  else if (!['own-chat-only', 'not-checked'].includes(i.watchers)) add('pr-watchers-unknown')
  // archiveAfterMerge is context only: bossd defers an archive until every chat
  // in the session is idle, so archive-after-merge no longer tears the
  // worktree out from under the agent that performed the merge.
  const authorizationReasons = [...reasons]
  if (i.verifyConsent === 'absent') authorizationReasons.push('no-verify-cron')
  else if (i.verifyConsent !== 'present') authorizationReasons.push('verify-consent-unknown')
  return {
    mergeEligible: reasons.length === 0,
    ineligibleReasons: reasons,
    mergeAuthorized: authorizationReasons.length === 0,
    authorizationReasons,
  }
}

// The only subprocess seam. A failed read is unknown, never an empty successful payload.
export function commandReader({ spawn = spawnSync, cwd = process.cwd(), bossBin = 'boss' } = {}) {
  return (command, args, { input, json = false, timeout = 30000 } = {}) => {
    let response
    try {
      response = spawn(command === 'boss' ? bossBin : command, args, {
        cwd,
        input,
        encoding: 'utf8',
        timeout,
        maxBuffer: 16 * 1024 * 1024,
        killSignal: 'SIGKILL',
      })
    } catch (error) {
      return { ok: false, error: error.code ?? 'spawn-error' }
    }
    let payload
    if (json) {
      try {
        payload = JSON.parse(response.stdout)
      } catch {
        /* Failure remains unknown. */
      }
    }
    if (response.error || response.status !== 0)
      return {
        ok: false,
        error:
          payload?.error?.code ??
          payload?.code ??
          response.error?.code ??
          `exit-${response.status}`,
        payload,
      }
    if (json && payload === undefined) return { ok: false, error: 'unreadable-json' }
    return { ok: true, payload, output: String(response.stdout ?? '').trim() }
  }
}
function readText(path) {
  try {
    return readFileSync(path, 'utf8').trim()
  } catch {
    return ''
  }
}
function readJSON(path) {
  try {
    return JSON.parse(readFileSync(path, 'utf8'))
  } catch {
    return null
  }
}
function writeJSON(path, value, options = {}) {
  mkdirSync(dirname(path), { recursive: true })
  writeFileSync(path, `${JSON.stringify(value, null, 2)}\n`, { mode: 0o600, ...options })
}
const PR_FIELDS =
  'state,mergeCommit,number,title,body,isDraft,headRefOid,baseRefName,mergeStateStatus,statusCheckRollup'
function prRead(read, pr, repo) {
  return read(
    'gh',
    ['pr', 'view', String(pr), ...(repo ? ['--repo', repo] : []), '--json', PR_FIELDS],
    { json: true },
  )
}

export function reviewedTreeDrift(read, base, reviewedHead, pushedHead) {
  if (!sha(reviewedHead) || !sha(pushedHead) || !text(base)) return 'unknown'
  const patch = (head) => {
    const ancestor = read('git', ['merge-base', `origin/${base}`, head])
    if (!ancestor.ok || !sha(ancestor.output)) return null
    const diff = read('git', ['diff', '--no-ext-diff', '--binary', ancestor.output, head, '--'])
    if (!diff.ok) return null
    const id = read('git', ['patch-id', '--stable'], { input: diff.output })
    if (!id.ok) return null
    // Empty diffs have the same empty patch-id; nonempty unparsable results are unknown.
    if (diff.output && !SHA.test(id.output.split(/\s/)[0])) return null
    return id.output.split(/\s/)[0]
  }
  const before = patch(reviewedHead),
    after = patch(pushedHead)
  return before === null || after === null ? 'unknown' : before === after ? 'none' : 'changed'
}

export function buildEnvelope(options, deps = {}) {
  const read = commandReader(deps)
  const gitDirRead = read('git', ['rev-parse', '--absolute-git-dir'])
  const gitDir = gitDirRead.ok ? gitDirRead.output : ''
  const repoResult = read('gh', ['repo', 'view', '--json', 'nameWithOwner'], { json: true })
  const repo = repoResult.ok ? repoResult.payload?.nameWithOwner : ''
  const envResult = read('boss', ['env', '--json'], { json: true })
  const env = envResult.ok ? envResult.payload : null
  const sessionId = env?.session?.session_id ?? ''
  const targetChatId = env?.session?.agent_session_id ?? ''
  const repoId = env?.session?.repo_id ?? ''
  const cron = text(repoId)
    ? read('boss', ['cron', 'ls', '--repo', repoId, '--json'], { json: true })
    : { ok: false }
  const prResult = prRead(read, options.pr, repo)
  const prView = prResult.ok ? prResult.payload : null
  const local = read('git', ['rev-parse', 'HEAD'])
  const upstream = read('git', ['rev-parse', '@{u}'])
  const show = text(sessionId)
    ? read('boss', ['show', sessionId, '--json'], { json: true })
    : { ok: false }
  const watches =
    options.callbacksAvailable === true && text(repo)
      ? read('boss', ['callback', 'list', '--repo', repo, '--pr', String(options.pr), '--json'], {
          json: true,
        })
      : { ok: false }
  const reviewFile =
    options.reviewVerdictFile ?? (gitDir ? resolve(gitDir, 'boss-build-review-verdict') : '')
  const reviewedFile =
    options.reviewedHeadFile ?? (gitDir ? resolve(gitDir, 'boss-build-reviewed-head') : '')
  const reviewVerdict = readText(reviewFile).match(/^REVIEW_VERDICT=(.*)$/m)?.[1] ?? ''
  const reviewedHead = readText(reviewedFile)
  const { criteria } = parseLedger(prView?.body)
  const envelope = {
    runId: options.runId,
    outcome: options.outcome,
    pr: options.pr,
    repo,
    sessionId,
    targetChatId,
    pushedHead: options.pushedHead ?? upstream.output ?? '',
    localHead: local.ok ? local.output : '',
    upstreamHead: upstream.ok ? upstream.output : '',
    reviewVerdict,
    reviewedHead,
    reviewVerdictFile: reviewFile,
    reviewedHeadFile: reviewedFile,
    reviewCoverage: sectionToken(prView?.body, 'review coverage'),
    crossModelReview: sectionToken(prView?.body, 'cross-model review'),
    openFindings:
      reviewVerdict === 'clean' && !sections(prView?.body).has('review findings') ? 0 : null,
    criteria,
    ciWaitState: options.ciWaitState,
    checkVerdict: options.checkVerdict,
    prView,
    treeDrift: reviewedTreeDrift(
      read,
      prView?.baseRefName,
      reviewedHead,
      upstream.ok ? upstream.output : '',
    ),
    ...parseFollowUps(prView?.body),
    launchOrigin: classifyLaunchOrigin(env),
    callbacksAvailable: options.callbacksAvailable,
    watchers: classifyWatchers({
      callbacksAvailable: options.callbacksAvailable,
      watches: watches.payload,
      listingOk: watches.ok,
      targetChatId,
      pr: options.pr,
    }),
    archiveAfterMerge: show.ok
      ? show.payload?.session?.repo_should_archive_sessions_after_merge
      : null,
    // A failed cron read is unknown, which never authorizes: fail toward a human.
    verifyConsent: cron.ok ? classifyVerifyConsent({ jobs: cron.payload, repoId }) : 'unknown',
  }
  // Keep the settled two-SHA verdict, but also veto gates that have since gone red or disappeared.
  const liveChecks = classifyChecks({
    headSHA: envelope.pushedHead,
    observedSHA: prView?.headRefOid ?? '',
    rollup: prView?.statusCheckRollup,
    priorContexts: options.priorRollup ?? null,
  })
  if (!isGreen(liveChecks)) envelope.checkVerdict = liveChecks
  return { ...envelope, ...computeEligibility(envelope) }
}

const skipped = (reason) => ({ action: 'skipped', reason, mergeSha: '' })

// The final record, re-keyed on the verify-gate merge result: only an observed MERGED with a 40-hex
// merge SHA is a merge, and it is `merged-by-verify` only when verify-gate's merge named that SHA.
export function settleCompletion({ runId, livePr, mergeResult = null, reason = '' } = {}) {
  if (livePr?.state === 'MERGED' && sha(livePr?.mergeCommit?.oid)) {
    const byGate = mergeResult?.merged === true && mergeResult.mergeSha === livePr.mergeCommit.oid
    return {
      runId,
      action: 'merged',
      reason: byGate ? 'merged-by-verify' : 'merged-outside-gate',
      mergeSha: livePr.mergeCommit.oid,
      ...(text(mergeResult?.trackerWrites) && mergeResult.trackerWrites !== 'ok'
        ? { trackerWrites: mergeResult.trackerWrites }
        : {}),
    }
  }
  if (!livePr || !['OPEN', 'CLOSED', 'MERGED'].includes(livePr.state) || livePr.state === 'MERGED')
    return { runId, ...skipped('merge-state-unreadable') }
  if (!mergeResult) return { runId, ...skipped(text(reason) ? reason : 'merge-not-attempted') }
  if (mergeResult.abandoned) return { runId, ...skipped('claim-lost') }
  if (mergeResult.verdict === 'reverify') return { runId, ...skipped('reverify') }
  if (mergeResult.dryRun === true) return { runId, ...skipped('dry-run') }
  if (mergeResult.merged === true) return { runId, ...skipped('merge-not-observed') }
  const why =
    [mergeResult.reason, mergeResult.reasons?.[0], mergeResult.verdict].find(text) ??
    'merge-not-observed'
  return { runId, ...skipped(why) }
}

// A post result that must stop the fast path, or null when it may continue.
function postStop(result) {
  if (!result) return 'verify-gate-unreadable'
  if (result.abandoned) return 'claim-lost'
  if (result.verdict === 'reverify') return 'reverify'
  if (text(result.trackerWrites) && result.trackerWrites !== 'ok')
    return `tracker-writes-${result.trackerWrites}`
  if (result.posted !== true && result.skipped !== 'already-posted')
    return text(result.reason) ? result.reason : 'status-not-posted'
  return null
}

/**
 * Step 12's verify fast path. Judges the head through the sibling verify-gate.mjs, posts the
 * verdict and, on `pass`, merges through `verify-gate.mjs merge`. Session-only and consent reasons
 * stop it before any verify-gate call. `extensions-required` claims the head and returns the
 * extension envelope WITHOUT a record; the caller runs the verify extensions and calls again with
 * `extensionResults` and the claim `token`. Every other path writes the run-keyed record to `out`
 * (never in dry-run) and returns it.
 */
export function fastPath(
  { envelope, out, extensionResults, token, dryRun = false } = {},
  deps = {},
) {
  if (!text(envelope?.runId) || !text(out))
    throw new Error('fast-path requires valid --envelope and --out')
  const runId = envelope.runId
  const prior = readJSON(out)
  if (prior?.runId === runId && ['merged', 'skipped'].includes(prior.action)) return prior
  const finish = (result) => {
    if (!dryRun) writeJSON(out, result)
    return result
  }
  const stop = (reason) => finish({ runId, ...skipped(reason) })
  // The envelope's booleans are advisory: re-derive authorization from its recorded facts.
  const eligibility = computeEligibility(envelope)
  if (!eligibility.mergeAuthorized) {
    const reasons = eligibility.authorizationReasons
    return stop(reasons.includes('epic-child') ? 'epic-child' : reasons[0])
  }
  const read = commandReader(deps)
  const gate = (verb, args) => {
    const r = read(process.execPath, [VERIFY_GATE, verb, ...args], {
      json: true,
      timeout: VERIFY_GATE_TIMEOUT_MS,
    })
    // A non-zero exit still carries its verdict when it printed one; no payload is unreadable.
    return r.payload && typeof r.payload === 'object' && !Array.isArray(r.payload)
      ? r.payload
      : null
  }
  const { pr, repo, pushedHead: head } = envelope
  const tail = (extra = []) => [
    ...extra,
    ...(text(token) ? ['--token', token] : []),
    '--repo',
    repo,
    ...(dryRun ? ['--dry-run'] : []),
  ]
  const post = (verdict, extra = []) =>
    gate('post', ['--pr', String(pr), '--head', head, '--verdict', verdict, ...tail(extra)])
  const settle = (reason, mergeResult = null) => {
    const live = prRead(read, pr, repo)
    return finish(
      settleCompletion({ runId, livePr: live.ok ? live.payload : null, mergeResult, reason }),
    )
  }

  const judged = gate('judge', [
    '--pr',
    String(pr),
    '--repo',
    repo,
    ...(text(extensionResults) ? ['--extension-results', extensionResults] : []),
  ])
  if (!judged || !text(judged.verdict)) return stop('verify-gate-unreadable')
  if (sha(judged.headSha) && judged.headSha.toLowerCase() !== String(head).toLowerCase())
    return stop('head-mismatch')
  let verdict = judged.verdict
  let reason = text(judged.reason) ? judged.reason : 'unknown'
  // Results were supplied, so a second `extensions-required` cannot resolve here: release the claim.
  if (verdict === 'extensions-required' && text(token)) {
    verdict = 'wait'
    reason = 'extensions-unresolved'
  }
  if (verdict === 'wait') {
    if (!text(token)) return settle(`wait:${reason}`)
    return settle(postStop(post('wait', ['--reason', reason])) ?? `wait:${reason}`)
  }
  if (verdict === 'extensions-required') {
    if (!judged.extensionEnvelope || judged.extensionEnvelope.invalid)
      return stop('extension-envelope-invalid')
    const claim = post('claim')
    if (!claim) return stop('verify-gate-unreadable')
    if (claim.won !== true || !text(claim.token))
      return stop(
        !text(claim.reason) || claim.reason === 'claim-held' ? 'claim-lost' : claim.reason,
      )
    return {
      runId,
      action: 'extensions-required',
      token: claim.token,
      extensionEnvelope: judged.extensionEnvelope,
    }
  }
  if (verdict === 'human' || verdict === 'defect') {
    const extra = ['--reason', reason]
    if (verdict === 'defect') {
      const findingsFile = resolve(
        dirname(out),
        `boss-build-verify-findings-${encodeURIComponent(runId)}.json`,
      )
      writeJSON(findingsFile, Array.isArray(judged.findings) ? judged.findings : [])
      extra.push('--findings', findingsFile)
    }
    const posted = post(verdict, extra)
    return settle(
      postStop(posted) ??
        `${posted.verdict ?? verdict}:${text(posted.reason) ? posted.reason : reason}`,
    )
  }
  if (verdict !== 'pass') return settle(verdict)
  const stopped = postStop(post('pass'))
  if (stopped) return settle(stopped)
  const merged = gate('merge', ['--pr', String(pr), '--head', head, ...tail()])
  if (!merged) return settle('verify-gate-unreadable')
  return settle('', merged)
}

const BOOLEAN_FLAGS = new Set(['dry-run'])
export const USAGE =
  'usage: completion-gate.mjs followups|envelope|fast-path --option value [--dry-run]'
export function main(argv, deps = {}) {
  const [subcommand, ...args] = argv
  const opts = {}
  for (let index = 0; index < args.length; index++) {
    const flag = args[index]
    if (!flag?.startsWith('--')) throw new Error('expected --option value')
    const name = flag.slice(2)
    if (BOOLEAN_FLAGS.has(name)) {
      opts[name] = true
      continue
    }
    if (args[index + 1] === undefined) throw new Error('expected --option value')
    opts[name] = args[++index]
  }
  if (subcommand === 'followups') return parseFollowUps(readText(opts['body-file']))
  if (subcommand === 'envelope') {
    if (!opts.out || !opts['run-id']) throw new Error('envelope requires --out and --run-id')
    const result = buildEnvelope(
      {
        runId: opts['run-id'],
        outcome: opts.outcome,
        pr: Number(opts.pr),
        ciWaitState: opts['ci-wait-state'],
        checkVerdict: readJSON(opts['check-verdict-file']),
        callbacksAvailable:
          opts['callbacks-available'] === 'true'
            ? true
            : opts['callbacks-available'] === 'false'
              ? false
              : undefined,
        reviewedHeadFile: opts['reviewed-head-file'],
        reviewVerdictFile: opts['review-verdict-file'],
      },
      deps,
    )
    writeJSON(opts.out, result)
    return result
  }
  if (subcommand === 'fast-path') {
    const envelope = readJSON(opts.envelope)
    if (!text(envelope?.runId) || !text(opts.out))
      throw new Error('fast-path requires valid --envelope and --out')
    if (text(opts['extension-results']) !== text(opts.token))
      throw new Error('fast-path takes --extension-results and --token together')
    return fastPath(
      {
        envelope,
        out: opts.out,
        extensionResults: opts['extension-results'],
        token: opts.token,
        dryRun: opts['dry-run'] === true,
      },
      deps,
    )
  }
  throw new Error(USAGE)
}
if (isMainModule(import.meta.url)) {
  try {
    const result = main(process.argv.slice(2))
    process.stdout.write(`${JSON.stringify(result)}\n`)
    if (process.argv[2] === 'followups' && !result.ok) process.exitCode = 1
  } catch (error) {
    process.stderr.write(`completion-gate: ${error.message}\n`)
    process.exitCode = 1
  }
}
