import { mkdtempSync, readFileSync, writeFileSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { operatorNoteBlocker, validateBlockerFile } from './verify-blockers.mjs'

/** Send an operator's pinned head through the same blocked-post path verify uses. */
export async function runSendBack(
  options,
  deps,
  {
    UsageError,
    isSha,
    text,
    writeAwareReader,
    commandReader,
    repoFor,
    readPr,
    readOperatorMarkers,
    newestFor,
    post,
  },
) {
  options = {
    ...options,
    noteFile: options.noteFile ?? options['note-file'],
    dryRun: options.dryRun ?? options['dry-run'] === true,
  }
  const head = typeof options.head === 'string' ? options.head.toLowerCase() : ''
  if (!isSha(head) || !text(options.noteFile))
    throw new UsageError('verify-gate: send-back needs --head <40-hex sha> and --note-file <file>')
  let note
  try {
    note = operatorNoteBlocker(readFileSync(options.noteFile, 'utf8'))
  } catch (error) {
    throw new UsageError(`verify-gate: send-back: ${error.message}`)
  }
  const read = writeAwareReader(deps.read ?? commandReader(deps))
  const repo = repoFor(read, options.repo)
  const refuse = (reason) => ({ sentBack: false, reason, head })
  const view = readPr(read, options.pr, repo)
  if (!view.ok) return refuse('pr-unreadable')
  if (view.payload?.state !== 'OPEN') return refuse('not-open')
  if (String(view.payload.headRefOid ?? '').toLowerCase() !== head) return refuse('head-moved')
  const markers = readOperatorMarkers(read, repo, options.pr)
  if (!markers.readable) return refuse('comments-unreadable')
  const standing = newestFor(markers.markers, head, (kind) => kind === 'blockers')?.blockerFile
  const file = {
    version: 1,
    pr: options.pr,
    head,
    blockers: [
      note,
      ...(standing?.pr === options.pr ? standing.blockers : []).filter(
        (b) => !['operator-note', 'no-review'].includes(b.kind),
      ),
    ],
  }
  const checked = validateBlockerFile(file)
  if (!checked.ok) throw new UsageError(`verify-gate: send-back: ${checked.errors.join('; ')}`)
  const dir = mkdtempSync(join(tmpdir(), 'verify-send-back-'))
  try {
    const path = join(dir, 'blockers.json')
    writeFileSync(path, JSON.stringify(file) + '\n', { mode: 0o600 })
    const result = await post(
      {
        pr: options.pr,
        repo,
        head,
        verdict: 'blocked',
        reason: 'blockers',
        blockers: path,
        dryRun: options.dryRun,
      },
      { ...deps, read },
    )
    return {
      sentBack: result.posted === true && result.verdict === 'blocked',
      head,
      ...(!(result.posted === true && result.verdict === 'blocked')
        ? { reason: result.reason ?? result.abandoned ?? 'post-failed' }
        : {}),
      ...(result.commentUrl ? { commentUrl: result.commentUrl } : {}),
      ...(result.autoRepair !== undefined ? { autoRepair: result.autoRepair } : {}),
      ...(result.dryRun ? { dryRun: true, writes: result.writes } : {}),
    }
  } finally {
    rmSync(dir, { recursive: true, force: true, maxRetries: 3 })
  }
}

export async function readBlockers(
  options,
  deps,
  { text, isSha, UsageError, commandReader, repoFor, readPr, readOperatorMarkers, newestFor },
) {
  if (!text(options.out) || (options.head !== undefined && !isSha(options.head)))
    throw new UsageError('verify-gate: blockers needs --out <file> and an optional valid --head')
  const read = deps.read ?? commandReader(deps),
    repo = repoFor(read, options.repo)
  const view = options.head ? null : readPr(read, options.pr, repo)
  const head = options.head ?? view?.payload?.headRefOid
  const result = readOperatorMarkers(read, repo, options.pr)
  if (!isSha(head) || !result.readable) {
    process.exitCode = 1
    return { outcome: 'unreadable' }
  }
  const file = newestFor(result.markers, head, (kind) => kind === 'blockers')?.blockerFile
  if (!file || file.pr !== options.pr) {
    process.exitCode = 3
    return { outcome: 'absent' }
  }
  writeFileSync(options.out, JSON.stringify(file) + '\n')
  return { outcome: 'written', out: options.out }
}
