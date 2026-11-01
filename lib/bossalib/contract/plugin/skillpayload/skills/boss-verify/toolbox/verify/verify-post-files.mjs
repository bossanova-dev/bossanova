// Materializes the judge's verdict inputs. Blockers are required; notes are cosmetic.
import { join } from 'node:path'
import { reviewerArgs } from './verify-review-request.mjs'
import {
  POST_FAILURE_PARK_AFTER,
  notePostFailure,
  clearPostFailure,
  readSweepEntries,
  writeSweepEntries,
} from './verify-post-failures.mjs'
import { validateBlockerFile } from './verify-blockers.mjs'
import { writeAtomicJson } from './verify-admission.mjs'

export function verdictFileArgs({ fs, logDir, pr, head, judged }) {
  const args = []
  if (judged.verdict === 'human' && judged.confidence) {
    const file = join(logDir, `boss-verify-confidence-${pr}-${head}.json`)
    if (!writeAtomicJson(fs, file, judged.confidence)) return null
    args.push('--confidence', file)
  }
  if (judged.verdict === 'blocked') {
    const file = join(logDir, `boss-verify-blockers-${pr}-${head}.json`)
    const payload = { version: 1, pr: judged.pr ?? pr, head, blockers: judged.blockers }
    if (!validateBlockerFile(payload).ok || !writeAtomicJson(fs, file, payload)) return null
    args.push('--blockers', file)
  }
  if (Array.isArray(judged.findingNotes) && judged.findingNotes.length) {
    const file = join(logDir, `boss-verify-notes-${pr}-${head}.json`)
    if (writeAtomicJson(fs, file, judged.findingNotes)) args.push('--notes', file)
  }
  return args
}

export function postArgv({
  pr,
  head,
  judged,
  fileArgs = [],
  repoArgs = [],
  dry = [],
  verdict = judged.verdict,
  reason = judged.reason,
}) {
  return [
    'post',
    '--pr',
    String(pr),
    '--head',
    head,
    '--verdict',
    verdict,
    ...fileArgs,
    ...(reason ? ['--reason', reason] : []),
    ...(verdict === 'human' && judged.policyFingerprint
      ? ['--policy', judged.policyFingerprint]
      : []),
    ...(['human', 'blocked'].includes(verdict) ? reviewerArgs(judged.reviewers) : []),
    ...repoArgs,
    ...dry,
  ]
}

const cappedStderr = (value) => {
  const line =
    String(value ?? '')
      .split('\n')
      .filter((s) => s.trim())
      .pop() ?? ''
  const runes = Array.from(line)
  // gate-region-ok: a rune cap on one stderr line, not a section window.
  return runes.length > 200 ? `${runes.slice(0, 199).join('')}…` : line
}
const postWhy = (result) => {
  const why =
    result?.payload?.reason ?? result?.payload?.trackerWrites ?? result?.error ?? 'unknown'
  const stderr = cappedStderr(result?.stderr)
  return stderr ? `${why}: ${stderr}` : why
}

// A successful park uses the existing approval/policy retry machinery; failed parks keep counting.
export function failedPostRecord(context, failure) {
  const { fs, stateFile, pr, head, judged, repo, repoArgs, dry, gate, now, logDir } = context
  const entries = stateFile ? readSweepEntries(fs, stateFile) : {}
  if (!failure) {
    clearPostFailure(entries, repo, pr)
    if (stateFile) writeSweepEntries(fs, stateFile, entries)
    return null
  }
  const stderr = cappedStderr(failure.stderr)
  let line = `skipped: post failed (${postWhy(failure)})`
  const failed = notePostFailure(
    entries,
    repo,
    pr,
    head,
    postWhy(failure),
    new Date(now()).toISOString(),
  )
  if (stateFile) writeSweepEntries(fs, stateFile, entries)
  if (!stateFile || failed.count < POST_FAILURE_PARK_AFTER)
    return { outcome: 'skipped', line, stderr }
  const notes = join(logDir, `boss-verify-post-failure-${pr}-${head}.json`)
  const message = `${judged.verdict} post failed ${failed.count} times: ${failed.why}`
  const written = writeAtomicJson(fs, notes, [
    { title: 'Verify could not post', severity: 'style', detail: message },
  ])
  const posted = written
    ? gate(
        postArgv({
          pr,
          head,
          judged,
          verdict: 'human',
          reason: 'verify-post-failed',
          fileArgs: ['--notes', notes],
          repoArgs,
          dry,
        }),
      )
    : null
  if (posted?.ok && (posted.payload?.posted || posted.payload?.skipped)) {
    clearPostFailure(entries, repo, pr)
    writeSweepEntries(fs, stateFile, entries)
    return { outcome: 'parked', line: `parked: needs human (verify-post-failed) · ${line}`, stderr }
  }
  line += ` · escalation failed (${posted ? postWhy(posted) : 'notes unwritable'})`
  return { outcome: 'skipped', line, stderr }
}
