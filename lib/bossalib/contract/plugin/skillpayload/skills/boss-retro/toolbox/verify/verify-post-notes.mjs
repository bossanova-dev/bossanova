import { buildNotes, carryTickets, notesComment } from './verify-notes.mjs'

// Notes are cosmetic: neither unreadable history nor a failed render/write changes the verdict.
export function recordFindingNotes(
  ctx,
  findings,
  { readOperatorMarkers, viewerLogin, recordHeadNote },
) {
  const { read, repo, pr, headSha, dryRun, writes } = ctx
  try {
    const notes = buildNotes(findings)
    if (!notes.length) return {}
    const markers = readOperatorMarkers(read, repo, pr)
    if (!markers.readable) return { notes: 'unknown' }
    const login = viewerLogin(read)
    const previous = markers.markers
      .filter((m) => m.kind === 'verify-notes' && m.login === login)
      .sort((a, b) => b.at - a.at || b.id - a.id)[0]
    const tickets = carryTickets(previous, notes)
    const note = notesComment({ head: headSha, notes, tickets })
    return {
      notes: recordHeadNote(read, {
        kind: 'verify-notes',
        repo,
        pr,
        headSha,
        note,
        markers,
        dryRun,
        writes,
      }),
    }
  } catch {
    return { notes: 'failed' }
  }
}
