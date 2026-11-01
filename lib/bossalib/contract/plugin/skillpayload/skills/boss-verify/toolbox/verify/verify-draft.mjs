#!/usr/bin/env node
import { parseArgs } from 'node:util'
import { readFileSync, writeFileSync, mkdirSync, renameSync, unlinkSync } from 'node:fs'
import { dirname } from 'node:path'
import { setTimeout as pause } from 'node:timers/promises'
import { isMainModule } from '../main-module.mjs'
import { blockedComment, findingsComment } from './verify-blockers.mjs'
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
      // only the original GitHub status, marker and writer permission grant ownership.
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
  const result = readStatus(read, startSha, repo, VERIFY_CONTEXT)
  if (result.verdict === 'unknown') return unreadable()
  const status = result.contexts?.[VERIFY_CONTEXT]
  if (status?.state !== 'failure' || !isSendBack(status.description)) return no('no-send-back')
  if (status.description.endsWith(VERIFY_DESCRIPTIONS.defectParkedSuffix)) return no('parked')
  const comments = read(...commentsReadCall(repo, options.pr))
  if (
    !comments.ok ||
    !Array.isArray(comments.payload) ||
    comments.payload.some((p) => !Array.isArray(p))
  )
    return unreadable()
  let trusted = false
  for (const comment of comments.payload.flat()) {
    if (typeof comment?.body !== 'string') return unreadable()
    if (!hasDraftMarker(comment.body, startSha) || !hasLine(comment.body, headMarker(startSha)))
      continue
    if (typeof comment.user?.login !== 'string' || !comment.user.login) return unreadable()
    const writer = canWrite(read, repo, comment.user.login)
    if (writer === null) return unreadable()
    trusted ||= writer
  }
  if (!trusted) return no('no-marker')
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
