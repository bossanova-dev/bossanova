import { afterMergeNote } from './verify-park-note.mjs'
import { VERIFY_DESCRIPTIONS } from './verify-vocabulary.mjs'
import { acceptedOn, acceptedNote } from './verify-accept.mjs'

export async function postPass(ctx, parsed, options, facts, actions) {
  const { recordCarry, readOperatorMarkers, recordHeadNote, clearStaleLabel } = actions
  const {
    read,
    repo,
    dryRun,
    pr,
    headSha,
    writes,
    status,
    runStatus,
    flushNotes,
    finish,
    current,
  } = ctx
  const { verdict, carryRecord } = parsed
  const { p } = facts
  // A pass a carried approval made posts its record first: a carry nobody can see does not pass.
  let carried = {}
  if (carryRecord) {
    const [, from, patchId] = carryRecord
    carried = recordCarry(read, { repo, pr, headSha, from, patchId, dryRun, writes })
    if (carried.reason) return finish({ verdict, posted: false, reason: carried.reason })
  }
  const description =
    options.reason === 'approved'
      ? VERIFY_DESCRIPTIONS.verifiedApproved
      : VERIFY_DESCRIPTIONS.verified
  // The PR's `## After merge` items, listed for a person. Cosmetic, so it never holds the verdict.
  const recordNotes = () => {
    const markers = readOperatorMarkers(read, repo, pr)
    const accepted = acceptedOn(markers.markers, headSha)
    const acceptedRecord = accepted.size
      ? {
          acceptedNote: recordHeadNote(read, {
            kind: 'accepted-note',
            repo,
            pr,
            headSha,
            note: acceptedNote({ headSha, acceptances: accepted }),
            markers,
            dryRun,
            writes,
          }),
        }
      : {}
    const note = afterMergeNote({ headSha, body: p.body })
    if (!note) return acceptedRecord
    const args = { kind: 'after-merge', repo, pr, headSha, note, markers, dryRun, writes }
    return { ...acceptedRecord, afterMerge: recordHeadNote(read, args) }
  }
  if (current.status?.state === 'success' && current.status.description === description)
    return finish({
      verdict,
      posted: false,
      skipped: 'already-posted',
      ...carried,
      ...recordNotes(),
    })
  const posted = runStatus(status('success', description)).ok
  flushNotes()
  return clearStaleLabel(ctx, facts, {
    verdict,
    posted,
    ...carried,
    ...(posted ? recordNotes() : {}),
  })
}
