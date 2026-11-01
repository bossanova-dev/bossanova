import { createHash } from 'node:crypto'
import { capDescription } from '../commit-status.mjs'
import { VERIFY_CONTEXT, VERIFY_DESCRIPTIONS, isCannotAssess } from './verify-vocabulary.mjs'

// The first unreadable run plus two reruns.
export const CANNOT_ASSESS_AFTER_RUNS = 3

/** Count consecutive unreadable outcomes in a head's newest-first status history. */
export function unreadableRunCount(verifyNodesNewestFirst, reasons) {
  let runs = 0
  for (const status of verifyNodesNewestFirst) {
    if (status.context !== VERIFY_CONTEXT) continue
    const description = typeof status.description === 'string' ? status.description : ''
    if (status.state === 'pending' && description.startsWith(VERIFY_DESCRIPTIONS.verifying))
      continue
    const reason = description.startsWith(`${VERIFY_DESCRIPTIONS.waiting} `)
      ? description.slice(VERIFY_DESCRIPTIONS.waiting.length + 1)
      : null
    if (isCannotAssess(status) || reasons.includes(reason)) runs++
    else break
  }
  return runs
}

export function cannotAssessDescription(reason) {
  return capDescription(`${VERIFY_DESCRIPTIONS.cannotAssess} ${reason}`)
}

export function cannotAssessMarker(headSha, digest) {
  return `<!-- boss-cannot-assess head=${headSha} digest=${digest} -->`
}

export const CANNOT_ASSESS_NOTE_MARKER =
  /^<!-- boss-cannot-assess head=([0-9a-fA-F]{40}) digest=([0-9a-f]{16}) -->$/

/** A head's explanation; unchanged evidence needs no rewrite as the run count grows. */
export function cannotAssessNote({ headSha, reason, runs }) {
  const digest = createHash('sha256')
    .update(JSON.stringify({ headSha, reason }))
    .digest('hex')
    .slice(0, 16)
  const body = [
    '**Verify cannot assess this head**',
    '',
    `Verify could not read evidence on \`${headSha}\` for ${runs} consecutive runs: \`${reason}\`.`,
    '',
    'Verify keeps rerunning this head automatically and re-judges it once the evidence can be read.',
    'Check `gh auth status`, access to the base branch and its CI, and the head’s `boss/build` receipt.',
    '',
    cannotAssessMarker(headSha, digest),
  ].join('\n')
  return { body, digest }
}
