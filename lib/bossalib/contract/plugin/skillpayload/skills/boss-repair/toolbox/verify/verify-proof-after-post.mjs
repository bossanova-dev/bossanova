// The proof after a verdict: once the verdict a `post` carries has landed, record the head's proof
// (verify-proof-publish.mjs) in process or in a detached `verify-gate.mjs proof` process, then
// report a lost proof through a `proof-missing` comment and boss note, or resolve a standing one.
// The `proof` verb runs the same path for a saved judge output. Nothing here changes a verdict.
import { spawn as nodeSpawn } from 'node:child_process'
import { closeSync, openSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'
import {
  proofMissingBossNote,
  proofMissingNote,
  proofProblems,
  proofResolvedNote,
} from './verify-proof-missing.mjs'
import { parseProofOptions, recordHeadProof } from './verify-proof-publish.mjs'

const GATE = fileURLToPath(new URL('./verify-gate.mjs', import.meta.url))

/**
 * Start `verify-gate.mjs proof` for the head in its own process group, its JSON line appended to
 * `boss-verify-proof-<pr>-<head>.log` under `logDir`: `{detached: true, log}`, or a skipped
 * proof when it cannot start. The proof is the head's own idempotent note, so nothing waits on it.
 */
export function spawnProof({
  pr,
  headSha,
  repo,
  files,
  cwd,
  env,
  spawn = nodeSpawn,
  logDir = tmpdir(),
}) {
  try {
    const log = join(logDir, `boss-verify-proof-${pr}-${headSha}.log`)
    const fd = openSync(log, 'a')
    try {
      const args = ['proof', '--pr', String(pr), '--head', headSha, '--judge', String(files.judge)]
      if (files.intent) args.push('--intent', String(files.intent))
      if (files.spec) args.push('--proof-spec', String(files.spec))
      if (files.media) args.push('--proof-media', String(files.media))
      if (repo) args.push('--repo', repo)
      const child = spawn(process.execPath, [GATE, ...args], {
        cwd,
        env,
        detached: true,
        stdio: ['ignore', fd, fd],
      })
      child.unref()
      return { detached: true, log }
    } finally {
      closeSync(fd)
    }
  } catch {
    return { recorded: false, reason: 'spawn-failed' }
  }
}

/**
 * Report non-benign proof losses via a PR comment and boss note, or resolve a standing
 * `proof-missing` comment in place once the head's proof has no problems. Never throws.
 * @param {{read?: any, repo?: string, pr?: number | string, headSha: string, dryRun?: boolean, writes?: any[]}} ctx
 * @param {import('./verify-proof-missing.mjs').ProofOutcome & {page?: {published?: boolean, url?: string, reason?: unknown} | null}} outcome
 * @param {{readOperatorMarkers?: Function, recordHeadNote?: Function, recordNote?: Function}} actions
 * @returns {{problems: import('./verify-proof-missing.mjs').ProofProblem[], comment: string, note: boolean} | null}
 */
function reportProofProblems(ctx, outcome, actions) {
  try {
    const problems = proofProblems(outcome)
    let markers = { readable: false }
    try {
      if (typeof actions?.readOperatorMarkers === 'function')
        markers = actions.readOperatorMarkers(ctx.read, ctx.repo, ctx.pr) ?? { readable: false }
    } catch {
      markers = { readable: false }
    }
    const baseNote = {
      kind: 'proof-missing',
      repo: ctx.repo,
      pr: ctx.pr,
      headSha: ctx.headSha,
      markers,
      dryRun: ctx.dryRun === true,
      writes: ctx.writes ?? [],
    }
    if (problems.length > 0) {
      let comment = 'unknown'
      try {
        const note = proofMissingNote({
          headSha: ctx.headSha,
          pr: ctx.pr ?? '',
          repo: ctx.repo,
          problems,
        })
        const noted = actions?.recordHeadNote?.(ctx.read, { ...baseNote, note })
        if (typeof noted?.outcome === 'string' && noted.outcome) comment = noted.outcome
      } catch {
        comment = 'unknown'
      }
      let note = false
      if (ctx.dryRun !== true && typeof actions?.recordNote === 'function') {
        try {
          const body = proofMissingBossNote({ pr: ctx.pr ?? '', headSha: ctx.headSha, problems })
          note =
            actions.recordNote(
              { trigger: 'proof-missing', body, pr: ctx.pr, headSha: ctx.headSha },
              { read: ctx.read },
            )?.recorded === true
        } catch {
          note = false
        }
      }
      return { problems, comment, note }
    }
    const head = String(ctx.headSha ?? '').toLowerCase()
    const standing =
      markers?.readable && Array.isArray(markers.markers)
        ? markers.markers
            .filter((m) => m.kind === 'proof-missing' && m.head === head)
            .sort((a, b) => (b.at ?? 0) - (a.at ?? 0) || (b.id ?? 0) - (a.id ?? 0))[0]
        : null
    if (standing && !standing.resolved && typeof actions?.recordHeadNote === 'function') {
      try {
        const pageUrl = outcome?.page?.published ? (outcome.page.url ?? null) : null
        actions.recordHeadNote(ctx.read, {
          ...baseNote,
          note: proofResolvedNote({ headSha: ctx.headSha, pageUrl }),
        })
      } catch {
        /* Best-effort resolution: never disturb the landed proof outcome. */
      }
    }
    return null
  } catch {
    return null
  }
}

/**
 * `result` with the head's `proof` outcome once the verdict it carries has landed (posted, or
 * already standing). The proof runs after every verdict write, so it can only add to the result.
 */
export async function proofAfterPost(ctx, proof, result, actions) {
  if (!proof || !(result.posted || result.skipped === 'already-posted')) return result
  /** @type {any} */
  let cachedMarkers = null
  const scoped =
    typeof actions?.readOperatorMarkers === 'function'
      ? {
          ...actions,
          readOperatorMarkers: (...a) =>
            (cachedMarkers ??= actions.readOperatorMarkers(...a) ?? { readable: false }),
        }
      : actions
  const outcome =
    proof.detach && !ctx.dryRun && proof.judged
      ? (scoped.spawnProof ?? spawnProof)({ cwd: scoped.cwd, env: scoped.env, ...ctx, ...proof })
      : proof.judged
        ? await recordHeadProof({ cwd: scoped.cwd, env: scoped.env, ...ctx, ...proof }, scoped)
        : { recorded: false, reason: 'judge-unreadable' }
  if (outcome?.detached === true) return { ...result, proof: outcome }
  const proofMissing = reportProofProblems(ctx, outcome, scoped)
  return { ...result, proof: outcome, ...(proofMissing ? { proofMissing } : {}) }
}

/**
 * The `proof` verb: record the proof for `--head` from a saved judge output after its verdict was
 * posted elsewhere (the sweep runs it detached, off its post budget). Never writes a status.
 */
export async function proofVerb(options, deps, actions) {
  const { UsageError, writeAwareReader, commandReader, repoFor, readPr } = actions
  const headSha = String(options.head ?? '').toLowerCase()
  if (!/^[0-9a-f]{40}$/.test(headSha) || options.judge === undefined)
    throw new UsageError('verify-gate: proof needs --head <40-hex sha> and --judge <file>')
  const read = writeAwareReader(deps.read ?? commandReader(deps))
  const repo = repoFor(read, options.repo)
  const view = readPr(read, options.pr, repo)
  if (!view.ok) return { recorded: false, reason: 'pr-unreadable' }
  if (String(view.payload?.headRefOid ?? '').toLowerCase() !== headSha)
    return { recorded: false, reason: 'head-moved' }
  const writes = []
  const result = await proofAfterPost(
    {
      read,
      repo,
      pr: options.pr,
      headSha,
      dryRun: options['dry-run'] === true,
      writes,
      prView: view.payload ?? {},
    },
    parseProofOptions(
      { ...options, proofSpec: options['proof-spec'], proofMedia: options['proof-media'] },
      'pass',
      UsageError,
    ).proof,
    { skipped: 'already-posted' },
    actions,
  )
  return {
    ...result.proof,
    ...(result.proofMissing ? { proofMissing: result.proofMissing } : {}),
    ...(options['dry-run'] === true ? { dryRun: true, writes } : {}),
  }
}
