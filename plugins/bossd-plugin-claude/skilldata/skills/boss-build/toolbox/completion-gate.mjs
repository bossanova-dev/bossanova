// Completion extensions may consent; this helper alone executes the sanctioned merge.
import { spawnSync } from 'node:child_process'
import { readFileSync, writeFileSync, mkdirSync, existsSync } from 'node:fs'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { isMainModule } from './main-module.mjs'
import { classifyChecks, isGreen, verdictAt, mergeStateVerdict } from './pr-check-state.mjs'
import { completionMergeAllowed, loadSkillConfig } from './skill-config.mjs'

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
  'archive-after-merge',
  'archive-after-merge-unknown',
  'not-opted-in',
  'no-completion-extension',
])

// Ignore fenced examples before recognizing headings or checkboxes.
function sections(body) {
  const result = new Map()
  if (typeof body !== 'string') return result
  let fence = null
  let current = null
  for (const line of body.split(/\r?\n/)) {
    const marker = line.match(/^\s{0,3}(`{3,}|~{3,})/)
    if (marker) {
      if (!fence) fence = marker[1]
      else if (
        marker[1][0] === fence[0] &&
        marker[1].length >= fence.length &&
        /^\s*(`+|~+)\s*$/.test(line)
      )
        fence = null
      continue
    }
    if (fence) continue
    const heading = line.match(/^\s{0,3}##\s+(.+?)\s*#*\s*$/)
    if (heading) {
      current = heading[1].trim().toLowerCase()
      // Duplicate sections are undecidable, rather than silently dropping open items.
      if (result.has(current)) result.set(current, null)
      else result.set(current, [])
    } else if (current && result.get(current)) result.get(current).push(line)
  }
  return result
}
function checklist(lines, allowNone = false) {
  if (!Array.isArray(lines)) return { status: 'missing', open: null, done: null, total: null }
  const items = lines.map((line) => line.trim()).filter(Boolean)
  if (allowNone && items.length === 1 && /^- none$/i.test(items[0]))
    return { status: 'ok', open: 0, done: 0, total: 0 }
  let open = 0,
    done = 0
  for (const line of items) {
    const item = line.match(/^- \[([ xX])\]\s+\S.*$/)
    if (!item) return { status: 'malformed', open: null, done: null, total: null }
    if (item[1] === ' ') open++
    else done++
  }
  if (!items.length) return { status: 'malformed', open: null, done: null, total: null }
  return { status: 'ok', open, done, total: open + done }
}
export function parseFollowUps(body) {
  const parsed = sections(body)
  const humanFollowUp = checklist(parsed.get('human follow-up'), true)
  const openQuestions = checklist(parsed.get('open questions'), true)
  return {
    humanFollowUp,
    openQuestions,
    ok: humanFollowUp.status === 'ok' && openQuestions.status === 'ok',
  }
}
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
  const c = i.criteria
  if (
    !c ||
    !Number.isInteger(c.total) ||
    c.total < 1 ||
    !Number.isInteger(c.met) ||
    c.met < 0 ||
    c.met > c.total
  )
    add('criteria-unknown')
  else if (c.met !== c.total) add('criteria-unmet')
  if (i.ciWaitState !== 'settled') add('ci-not-settled')
  if (!sha(i.pushedHead) || !isGreen(verdictAt(i.checkVerdict ?? {}, i.pushedHead)))
    add('ci-not-green-on-head')
  const state =
    typeof i.prView?.mergeStateStatus === 'string' ? i.prView.mergeStateStatus.toUpperCase() : ''
  if (!state || state === 'UNKNOWN') add('merge-state-unknown')
  else if (
    !['CLEAN', 'HAS_HOOKS', 'UNSTABLE'].includes(state) ||
    mergeStateVerdict({ mergeState: state }).blocking
  )
    add(
      INELIGIBLE_REASONS.includes(`merge-state-${state.toLowerCase()}`)
        ? `merge-state-${state.toLowerCase()}`
        : 'merge-state-unknown',
    )
  if (i.prView?.state !== 'OPEN') add('pr-not-open')
  if (i.prView?.isDraft !== false) add('pr-draft')
  if (
    typeof i.prView?.title !== 'string' ||
    typeof i.prView?.body !== 'string' ||
    /do not merge/i.test(`${i.prView.title}\n${i.prView.body}`) ||
    /\(partial\s/i.test(i.prView.title)
  )
    add('do-not-merge-marker')
  const heads = [i.pushedHead, i.localHead, i.upstreamHead, i.prView?.headRefOid]
  if (!heads.every(sha)) add('head-unknown')
  else if (new Set(heads).size !== 1) add('head-mismatch')
  if (i.treeDrift === 'changed') add('tree-changed-since-review')
  else if (i.treeDrift !== 'none') add('tree-drift-unknown')
  for (const [value, openReason, unknownReason] of [
    [i.humanFollowUp, 'human-follow-up-open', 'follow-up-section-missing'],
    [i.openQuestions, 'open-questions-open', 'open-questions-section-missing'],
  ]) {
    if (value?.status !== 'ok' || !Number.isInteger(value.open) || value.open < 0)
      add(unknownReason)
    else if (value.open !== 0) add(openReason)
  }
  if (i.launchOrigin === 'epic-child') add('epic-child')
  else if (i.launchOrigin !== 'standalone') add('launch-origin-unknown')
  if (i.watchers === 'foreign-pr-watcher') add('foreign-pr-watcher')
  else if (!['own-chat-only', 'not-checked'].includes(i.watchers)) add('pr-watchers-unknown')
  if (i.archiveAfterMerge === true) add('archive-after-merge')
  else if (i.archiveAfterMerge !== false) add('archive-after-merge-unknown')
  const authorizationReasons = [...reasons]
  if (!completionMergeAllowed(i.config)) authorizationReasons.push('not-opted-in')
  if (!Number.isInteger(i.extensionCount) || i.extensionCount < 1)
    authorizationReasons.push('no-completion-extension')
  return {
    mergeEligible: reasons.length === 0,
    ineligibleReasons: reasons,
    mergeAuthorized: authorizationReasons.length === 0,
    authorizationReasons,
  }
}

// The only subprocess seam. A failed read is unknown, never an empty successful payload.
export function commandReader({ spawn = spawnSync, cwd = process.cwd(), bossBin = 'boss' } = {}) {
  return (command, args, { input, json = false } = {}) => {
    let response
    try {
      response = spawn(command === 'boss' ? bossBin : command, args, {
        cwd,
        input,
        encoding: 'utf8',
        timeout: 30000,
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
function token(body, heading) {
  const lines = sections(body).get(heading)
  return lines?.map((s) => s.trim()).find(Boolean) ?? ''
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
  const criteria = checklist(sections(prView?.body).get('acceptance criteria'))
  let config
  try {
    const loaded = (deps.loadConfig ?? loadSkillConfig)({ cwd: deps.cwd ?? process.cwd() })
    config = { completionDefaults: { allowMerge: loaded?.completionDefaults?.allowMerge } }
  } catch {
    config = null
  }
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
    reviewCoverage: token(prView?.body, 'review coverage'),
    crossModelReview: token(prView?.body, 'cross-model review'),
    openFindings:
      reviewVerdict === 'clean' && !sections(prView?.body).has('review findings') ? 0 : null,
    criteria: criteria.status === 'ok' ? { met: criteria.done, total: criteria.total } : null,
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
    extensionCount: options.extensionCount,
    config,
  }
  // Keep the settled two-SHA verdict, but also veto gates that have since gone red or disappeared.
  const liveChecks = classifyChecks({
    headSHA: envelope.pushedHead,
    observedSHA: prView?.headRefOid ?? '',
    rollup: prView?.statusCheckRollup,
    priorContexts: options.priorRollup ?? null,
  })
  if (!isGreen(liveChecks)) envelope.checkVerdict = liveChecks
  const envelopePath = options.out ?? options.envelopePath
  const attemptFile =
    options.attemptFile ??
    (gitDir && text(options.runId)
      ? resolve(gitDir, `boss-build-completion-attempt-${encodeURIComponent(options.runId)}.json`)
      : '')
  envelope.attemptFile = attemptFile
  envelope.mergeCommand =
    envelopePath && attemptFile
      ? [
          process.execPath,
          fileURLToPath(import.meta.url),
          'merge',
          '--envelope',
          resolve(envelopePath),
          '--attempt-file',
          attemptFile,
        ]
      : []
  return { ...envelope, ...computeEligibility(envelope) }
}

const skipped = (reason) => ({ action: 'skipped', reason, mergeSha: '' })
export function mergeCompletion(envelope, { attemptFile = envelope?.attemptFile, ...deps } = {}) {
  if (!text(envelope?.runId) || !text(attemptFile)) return skipped('attempt-record-unknown')
  if (existsSync(attemptFile)) return skipped('already-attempted')
  const read = commandReader(deps)
  const directory = read('git', ['rev-parse', '--absolute-git-dir'])
  if (!directory.ok || !text(directory.output)) return skipped('attempt-record-unknown')
  const guardFile = resolve(
    directory.output,
    `boss-build-completion-attempt-${encodeURIComponent(envelope.runId)}.json`,
  )
  if (existsSync(guardFile)) return skipped('already-attempted')
  // Re-read even the local facts and opt-in; envelope booleans are advisory, never authority.
  const fresh = buildEnvelope(
    {
      ...envelope,
      pr: envelope.pr,
      reviewedHeadFile: envelope.reviewedHeadFile,
      reviewVerdictFile: envelope.reviewVerdictFile,
    },
    deps,
  )
  if (fresh.repo !== envelope.repo || fresh.sessionId !== envelope.sessionId)
    return skipped('identity-mismatch')
  const snapshot = computeEligibility({ ...envelope, config: fresh.config })
  if (!snapshot.mergeAuthorized)
    return skipped(
      snapshot.authorizationReasons.includes('epic-child')
        ? 'epic-child'
        : snapshot.authorizationReasons[0],
    )
  if (!fresh.mergeAuthorized)
    return skipped(
      fresh.authorizationReasons.includes('epic-child')
        ? 'epic-child'
        : fresh.authorizationReasons[0],
    )
  if (fresh.pushedHead !== envelope.pushedHead) return skipped('head-mismatch')
  const record = {
    runId: envelope.runId,
    attemptedAt: new Date().toISOString(),
    invoked: true,
    pushedHead: fresh.pushedHead,
  }
  try {
    // The canonical git-dir guard cannot be bypassed with a different caller-supplied path.
    writeJSON(guardFile, record, { flag: 'wx' })
    if (resolve(attemptFile) !== guardFile) writeJSON(attemptFile, record, { flag: 'wx' })
  } catch (error) {
    return skipped(error.code === 'EEXIST' ? 'already-attempted' : 'attempt-record-unwritable')
  }
  const merged = read('boss', ['merge', fresh.sessionId, '--yes', '--json'], { json: true })
  let result = skipped(merged.ok ? 'merge-not-observed' : `merge-refused:${merged.error}`)
  // A daemon response is not evidence of a merge SHA. One bounded re-read handles transient reads.
  for (let attempt = 0; attempt < 2; attempt++) {
    const live = prRead(read, fresh.pr, fresh.repo)
    if (live.ok && live.payload?.state === 'MERGED' && sha(live.payload?.mergeCommit?.oid)) {
      result = {
        action: 'merged',
        reason: 'merged-by-completion',
        mergeSha: live.payload.mergeCommit.oid,
      }
      break
    }
    if (live.ok && live.payload?.state !== 'MERGED') break
  }
  try {
    writeJSON(attemptFile, {
      runId: envelope.runId,
      invoked: true,
      daemonAccepted: merged.ok,
      pushedHead: fresh.pushedHead,
      ...result,
    })
  } catch {
    /* Live settle remains authoritative. */
  }
  return result
}

export function settleCompletion({
  runId,
  livePr,
  attempt,
  results = [],
  extensionCount = results.length,
} = {}) {
  const claimed = Array.isArray(results) && results.some((result) => result?.action === 'merged')
  if (livePr?.state === 'MERGED' && sha(livePr?.mergeCommit?.oid)) {
    const byGate =
      attempt?.runId === runId &&
      (attempt?.daemonAccepted === true ||
        (attempt?.action === 'merged' && attempt.mergeSha === livePr.mergeCommit.oid))
    return {
      runId,
      action: 'merged',
      reason: byGate ? 'merged-by-completion' : 'merged-outside-gate',
      mergeSha: livePr.mergeCommit.oid,
    }
  }
  if (!livePr || !['OPEN', 'CLOSED', 'MERGED'].includes(livePr.state) || livePr.state === 'MERGED')
    return { runId, ...skipped('merge-state-unreadable') }
  if (claimed) return { runId, ...skipped('claimed-merge-not-observed') }
  if (!extensionCount) return { runId, ...skipped('no-completion-extension') }
  const reason = Array.isArray(results)
    ? results.find(
        (result) => result?.action === 'skipped' && text(result.reason) && result.mergeSha === '',
      )?.reason
    : null
  return { runId, ...skipped(reason ?? 'extension-result-invalid') }
}

export function main(argv, deps = {}) {
  const [subcommand, ...args] = argv
  const opts = {}
  for (let index = 0; index < args.length; index += 2) {
    if (!args[index]?.startsWith('--') || args[index + 1] === undefined)
      throw new Error('expected --option value')
    opts[args[index].slice(2)] = args[index + 1]
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
        extensionCount: Number(opts['extension-count']),
        callbacksAvailable:
          opts['callbacks-available'] === 'true'
            ? true
            : opts['callbacks-available'] === 'false'
              ? false
              : undefined,
        reviewedHeadFile: opts['reviewed-head-file'],
        reviewVerdictFile: opts['review-verdict-file'],
        attemptFile: opts['attempt-file'],
        out: opts.out,
      },
      deps,
    )
    writeJSON(opts.out, result)
    return result
  }
  if (subcommand === 'merge')
    return mergeCompletion(readJSON(opts.envelope), { ...deps, attemptFile: opts['attempt-file'] })
  if (subcommand === 'settle') {
    const envelope = readJSON(opts.envelope)
    if (!text(envelope?.runId) || !opts.out)
      throw new Error('settle requires valid --envelope and --out')
    const read = commandReader(deps)
    const live = prRead(read, envelope.pr, envelope.repo)
    const result = settleCompletion({
      runId: envelope.runId,
      livePr: live.payload,
      attempt: readJSON(opts['attempt-file'] ?? envelope.attemptFile),
      results: readJSON(opts['results-file']) ?? [],
      extensionCount: envelope.extensionCount,
    })
    writeJSON(opts.out, result)
    return result
  }
  throw new Error('usage: completion-gate.mjs followups|envelope|merge|settle --option value')
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
