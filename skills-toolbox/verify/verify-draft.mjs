#!/usr/bin/env node
import { parseArgs } from 'node:util'
import { readFileSync, writeFileSync, mkdirSync, renameSync, unlinkSync } from 'node:fs'
import { dirname } from 'node:path'
import { setTimeout as pause } from 'node:timers/promises'
import { isMainModule } from '../main-module.mjs'
import { blockedComment, findingsComment, parseOutcomesTrailer } from './verify-blockers.mjs'
import { acceptedLines } from './verify-accept.mjs'
import { VERIFY_CONTEXT, VERIFY_DESCRIPTIONS, isSendBack } from './verify-vocabulary.mjs'

const isSha = (sha) => typeof sha === 'string' && /^[a-f0-9]{40}$/i.test(sha)
export const draftMarker = (sha) => `boss-verify draft ${sha.toLowerCase()}`
const headMarker = (sha) => `boss-verify head ${sha.toLowerCase()}`
const hasLine = (body, line) =>
  typeof body === 'string' && body.split('\n').some((value) => value.trim() === line)
export const hasDraftMarker = (body, sha) => hasLine(body, draftMarker(sha))
export function withDraftMarker(body, sha) {
  return body
    .split('\n')
    .flatMap((line) => (line.trim() === headMarker(sha) ? [draftMarker(sha), line] : [line]))
    .join('\n')
}

const draftIntent = (facts) => !facts.parked && facts.p.isDraft === false
export function sendBackComment(parsed, facts, sha, mention) {
  const body =
    parsed.verdict === 'blocked'
      ? blockedComment(
          parsed.blockers,
          headMarker(sha),
          acceptedLines(facts.operatorMarkers?.markers, sha),
          mention,
        )
      : findingsComment(parsed.findings, sha, mention)
  return draftIntent(facts) ? withDraftMarker(body, sha) : body
}

export function convertToDraft(read, repo, pr) {
  return read('gh', ['pr', 'ready', String(pr), '--undo', '--repo', repo]).ok
    ? 'converted'
    : 'failed'
}

export function draftSendBack(ctx, facts, posted) {
  if (!draftIntent(facts) || !posted) return {}
  if (ctx.dryRun) {
    ctx.writes.push({ kind: 'pr-draft' })
    return { draft: 'planned' }
  }
  return { draft: convertToDraft(ctx.read, ctx.repo, ctx.pr) }
}

export function fileRecovery(path) {
  return {
    load() {
      try {
        return JSON.parse(readFileSync(path, 'utf8'))
      } catch (error) {
        if (error.code === 'ENOENT') return null
        throw error
      }
    },
    save(value) {
      mkdirSync(dirname(path), { recursive: true })
      const pending = `${path}.${process.pid}.tmp`
      try {
        writeFileSync(pending, JSON.stringify(value) + '\n')
        renameSync(pending, path)
      } finally {
        try {
          unlinkSync(pending)
        } catch (error) {
          if (error.code !== 'ENOENT') throw error
        }
      }
    },
    clear() {
      try {
        unlinkSync(path)
      } catch (error) {
        if (error.code !== 'ENOENT') throw error
      }
    },
  }
}

const UNREADABLE = Symbol('unreadable')
const MAX_OUTCOME_HOPS = 20
const TRANSITIONS = ['ReadyForReviewEvent', 'ConvertToDraftEvent']
const DRAFT_HISTORY_QUERY = `query($owner: String!, $name: String!, $number: Int!) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {
      timelineItems(last: 2, itemTypes: [READY_FOR_REVIEW_EVENT, CONVERT_TO_DRAFT_EVENT]) {
        nodes { __typename ... on ReadyForReviewEvent { createdAt } ... on ConvertToDraftEvent { createdAt } }
      }
    }
  }
}`

// Walks posted repair outcomes from a pushed head back to its blockers head, trusting only
// writer-authored hops, until a head carries a verify status. Returns that head when the status
// is a send-back, null when the chain ends elsewhere, or UNREADABLE.
function sendBackThroughOutcomes(startSha, comments, writer, statusOf) {
  const links = comments.flatMap((comment) => {
    const file = parseOutcomesTrailer(comment.body)
    if (!file) return []
    return [{ comment, from: file.blockersHead.toLowerCase(), to: file.pushedHead.toLowerCase() }]
  })
  const seen = new Set([startSha])
  let frontier = [startSha]
  for (let hop = 0; hop < MAX_OUTCOME_HOPS && frontier.length > 0; hop++) {
    const next = []
    for (const link of links) {
      if (!frontier.includes(link.to) || seen.has(link.from)) continue
      const trust = writer(link.comment)
      if (trust === null) return UNREADABLE
      if (!trust) continue
      seen.add(link.from)
      const status = statusOf(link.from)
      if (status === UNREADABLE) return UNREADABLE
      if (!status) next.push(link.from)
      else if (status.state === 'failure' && isSendBack(status.description))
        return { sha: link.from, status }
    }
    frontier = next
  }
  return null
}

// Verify still holds the draft when the only ready/draft transition since its latest marker
// comment is one convert to draft. A later ready-and-redraft pair is a person's hold. null when
// the timeline cannot be read.
function verifyHoldsDraft(read, repo, pr, markedAt) {
  const [owner, name] = repo.split('/')
  const r = read(
    'gh',
    [
      'api',
      'graphql',
      '-f',
      `query=${DRAFT_HISTORY_QUERY}`,
      '-f',
      `owner=${owner}`,
      '-f',
      `name=${name}`,
      '-F',
      `number=${pr}`,
    ],
    { json: true },
  )
  const nodes = r.payload?.data?.repository?.pullRequest?.timelineItems?.nodes
  if (!r.ok || r.payload?.errors?.length || !Array.isArray(nodes) || nodes.length > 2) return null
  const times = nodes.map((node) => Date.parse(node?.createdAt))
  if (
    nodes.some((node, i) => !TRANSITIONS.includes(node?.__typename) || !Number.isFinite(times[i]))
  )
    return null
  return (
    nodes.at(-1)?.__typename === 'ConvertToDraftEvent' &&
    times.at(-1) >= markedAt &&
    (nodes.length < 2 || times[0] < markedAt)
  )
}

export async function runReady(options, deps, helpers) {
  const { readPr, readStatus, repoFor, commandReader, canWrite, commentsReadCall } = helpers
  const read = deps.read ?? commandReader(deps)
  const repo = repoFor(read, options.repo)
  const unreadable = () => ({ outcome: 'unreadable' })
  if (!repo) return unreadable()
  let recovery
  try {
    const path = deps.recovery
      ? null
      : read('git', ['rev-parse', '--git-path', `boss-repair/verify-draft-${options.pr}.json`])
    if (!deps.recovery && (!path.ok || !path.output)) return unreadable()
    recovery = deps.recovery ?? fileRecovery(path.output)
  } catch {
    return unreadable()
  }
  let startSha = options.startSha
  let target
  try {
    const saved = recovery.load()
    const local = read('git', ['rev-parse', 'HEAD'])
    target = local.output?.trim()
    if (!local.ok || !isSha(target)) return unreadable()
    if (
      saved?.repo === repo &&
      saved.pr === options.pr &&
      isSha(saved.startSha) &&
      (saved.headSha === options.startSha ||
        (saved.headSha === target && saved.requestSha === options.startSha))
    ) {
      // START_SHA identifies the PR head this repair captured before its push, even
      // when a rebase rewrites ancestry. The checkpoint selects evidence to re-read;
      // only the original GitHub status, marker and writer permission grant ownership, with
      // writer-authored repair outcomes linking a later start head back to that status.
      startSha = saved.startSha
    }
    if (!options.dryRun)
      recovery.save({
        repo,
        pr: options.pr,
        startSha,
        headSha: target,
        requestSha: options.startSha,
      })
  } catch {
    return unreadable()
  }
  const view = readPr(read, options.pr, repo)
  if (
    !view.ok ||
    typeof view.payload?.state !== 'string' ||
    typeof view.payload?.isDraft !== 'boolean'
  )
    return unreadable()
  const no = (reason) => ({ outcome: 'not-verify-drafted', reason })
  if (view.payload.state !== 'OPEN') return no('no-send-back')
  if (!view.payload.isDraft) return { outcome: 'not-draft' }
  if (view.payload.headRefOid !== target) return unreadable()
  const statusOf = (sha) => {
    const result = readStatus(read, sha, repo, VERIFY_CONTEXT)
    return result.verdict === 'unknown' ? UNREADABLE : (result.contexts?.[VERIFY_CONTEXT] ?? null)
  }
  const direct = statusOf(startSha)
  if (direct === UNREADABLE) return unreadable()
  const readComments = () => {
    const r = read(...commentsReadCall(repo, options.pr))
    if (!r.ok || !Array.isArray(r.payload) || r.payload.some((p) => !Array.isArray(p))) return null
    const list = r.payload.flat()
    return list.every((c) => typeof c?.body === 'string') ? list : null
  }
  const writers = new Map()
  const writer = (comment) => {
    const login = comment.user?.login
    if (typeof login !== 'string' || !login) return null
    if (!writers.has(login)) writers.set(login, canWrite(read, repo, login))
    return writers.get(login)
  }
  // A repair that starts from a head verify never judged inherits the hold of the send-back its
  // writer-authored outcomes chain reaches, but only while verify's own draft is the latest one.
  let comments = null
  let owner = { sha: startSha, status: direct, chained: false }
  if (!direct) {
    comments = readComments()
    if (!comments) return unreadable()
    const reached = sendBackThroughOutcomes(startSha, comments, writer, statusOf)
    if (reached === UNREADABLE) return unreadable()
    if (!reached) return no('no-send-back')
    owner = { ...reached, chained: true }
  }
  const { status } = owner
  if (status.state !== 'failure' || !isSendBack(status.description)) return no('no-send-back')
  if (status.description.endsWith(VERIFY_DESCRIPTIONS.defectParkedSuffix)) return no('parked')
  comments ??= readComments()
  if (!comments) return unreadable()
  let trusted = false
  let markedAt = -Infinity
  for (const comment of comments) {
    if (!hasDraftMarker(comment.body, owner.sha) || !hasLine(comment.body, headMarker(owner.sha)))
      continue
    const trust = writer(comment)
    if (trust === null) return unreadable()
    if (!trust) continue
    trusted = true
    if (!owner.chained) continue
    const at = Date.parse(comment.created_at)
    if (!Number.isFinite(at)) return unreadable()
    markedAt = Math.max(markedAt, at)
  }
  if (!trusted) return no('no-marker')
  if (owner.chained) {
    const held = verifyHoldsDraft(read, repo, options.pr, markedAt)
    if (held === null) return unreadable()
    if (!held) return no('redrafted')
  }
  if (options.dryRun) return { outcome: 'planned' }
  return {
    outcome: read('gh', ['pr', 'ready', String(options.pr), '--repo', repo]).ok
      ? 'readied'
      : 'failed',
  }
}

export async function main(argv = process.argv.slice(2), deps = {}) {
  let parsed
  try {
    parsed = parseArgs({
      args: argv,
      strict: true,
      allowPositionals: true,
      options: {
        pr: { type: 'string' },
        'start-sha': { type: 'string' },
        repo: { type: 'string' },
        'dry-run': { type: 'boolean' },
      },
    })
    const command = parsed.positionals[0]
    if (
      parsed.positionals.length !== 1 ||
      command !== 'ready' ||
      !/^[1-9][0-9]*$/.test(parsed.values.pr ?? '') ||
      !isSha(parsed.values['start-sha'])
    )
      throw new Error('invalid arguments')
  } catch (cause) {
    throw Object.assign(
      new Error('verify-draft: ready needs --pr <n> --start-sha <40-hex sha>', { cause }),
      { exitCode: 2 },
    )
  }
  const helpers = await import('./verify-gate.mjs')
  const options = {
    pr: Number(parsed.values.pr),
    startSha: parsed.values['start-sha'].toLowerCase(),
    repo: parsed.values.repo,
    dryRun: parsed.values['dry-run'] === true,
  }
  let result
  for (let attempt = 0; attempt < 3; attempt++) {
    result = await runReady(options, deps, helpers)
    if (!['unreadable', 'failed'].includes(result.outcome)) break
    if (attempt < 2) await (deps.pause ?? pause)(1000)
  }
  // Failed attempts keep the original ownership for the next repair pass. A cache is
  // never authority: runReady revalidates all GitHub evidence on every attempt.
  if (!options.dryRun && !['unreadable', 'failed'].includes(result.outcome)) {
    try {
      const read = deps.read ?? helpers.commandReader(deps)
      const path = deps.recovery
        ? null
        : read('git', ['rev-parse', '--git-path', `boss-repair/verify-draft-${options.pr}.json`])
      if (deps.recovery) deps.recovery.clear()
      else if (path.ok && path.output) fileRecovery(path.output).clear()
    } catch {
      return { outcome: 'unreadable' }
    }
  }
  return result
}

if (isMainModule(import.meta.url)) {
  main()
    .then((result) => {
      process.stdout.write(JSON.stringify(result) + '\n')
      if (['unreadable', 'failed'].includes(result.outcome)) process.exitCode = 1
    })
    .catch((error) => {
      process.stderr.write(error.message + '\n')
      process.exitCode = error.exitCode ?? 1
    })
}
