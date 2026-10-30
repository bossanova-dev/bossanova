import { indeterminateWriteError, isDefinitiveWriteRejection } from './outcome.mjs'
import { createHash } from 'node:crypto'
import { readFileSync, mkdtempSync, writeFileSync, renameSync, rmSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { tmpdir } from 'node:os'
import {
  sourceNotesCommentBody,
  selectSourceNotesComment,
  sourceNotesAttachmentFromComment,
  isTrustedPlanAuthor,
  planCommentBody,
  recordTextFromComment,
  epicSpecCommentBody,
  selectEpicSpecComment,
  epicSpecAttachmentFromComment,
  selectPlanComment,
  planAttachmentFromComment,
} from './github-plan.mjs'

const digest = (text) => createHash('sha256').update(text).digest('hex')

export function issuePlanView(ticket, comments) {
  const selected = selectPlanComment(comments)
  const attachments =
    selected.status === 'one' ? [planAttachmentFromComment(selected.comment, ticket.id)] : []
  const spec = selectEpicSpecComment(comments)
  // Preserve every trusted spec view so existing epic detection sees ambiguity.
  const specIds =
    spec.status === 'conflict' ? spec.ids : spec.status === 'one' ? [String(spec.comment.id)] : []
  const specs = comments
    .filter((comment) => specIds.includes(String(comment.id)))
    .map((comment) => epicSpecAttachmentFromComment(comment, ticket.id))
  const notes = selectSourceNotesComment(comments)
  const sourceNotes =
    notes.status === 'one' ? [sourceNotesAttachmentFromComment(notes.comment, ticket.id)] : []
  return {
    ...ticket,
    comments,
    attachments: [...attachments, ...specs, ...sourceNotes],
    planAttachment: attachments[0] ?? null,
    ...(notes.status === 'conflict' ? { sourceNotesConflict: notes.ids } : {}),
    ...(spec.status === 'conflict' ? { specConflict: spec.ids } : {}),
    ...(selected.status === 'conflict' ? { planConflict: selected.ids } : {}),
  }
}

export function runPlanVerb(verb, args, ctx) {
  const root = `repos/${ctx.identity.owner}/${ctx.identity.repo}`
  if (verb === 'read-plan') {
    if (!/^[1-9]\d*$/.test(String(args.id))) {
      let issueId = false
      try {
        ctx.issueNumber(args.id)
        issueId = true
      } catch {
        // Invalid issue syntax still needs the plan-record contract below.
      }
      throw new Error(
        `github tracker: readPlanAttachment expects a plan record (comment) id${issueId ? ', not an issue id' : ''}; ` +
          "use writePlan's result id or getIssue's planAttachment.id (the matching attachments[].id for an Epic spec record)",
      )
    }
    if (typeof args.outFile !== 'string' || !args.outFile)
      throw new Error('github tracker: outFile is required')
    const comment = ctx.api(`${root}/issues/comments/${args.id}`)
    if (!isTrustedPlanAuthor(comment))
      throw new Error(
        'github tracker: plan author must be a repository owner, member or collaborator',
      )
    const text = recordTextFromComment(comment.body)
    const temp = mkdtempSync(join(dirname(args.outFile), '.plan-read-'))
    try {
      const file = join(temp, 'plan.md')
      writeFileSync(file, text, { mode: 0o600 })
      renameSync(file, args.outFile)
    } finally {
      rmSync(temp, { recursive: true, force: true, maxRetries: 3 })
    }
    return {
      id: String(args.id),
      bytes: Buffer.byteLength(text),
      sha256: digest(text),
      outFile: args.outFile,
    }
  }
  const spec = verb === 'write-epic-spec'
  const notes = verb === 'write-source-notes'
  const kind = notes ? 'source notes' : spec ? 'epic spec' : 'plan'
  const select = notes ? selectSourceNotesComment : spec ? selectEpicSpecComment : selectPlanComment
  const number = ctx.issueNumber(args.issue)
  const title = `${notes ? 'Source notes' : spec ? 'Epic spec' : 'Implementation plan'} (GH-${number})`
  if (args.title !== title) throw new Error(`github tracker: ${kind} title must equal ${title}`)
  const text = readFileSync(args.file, 'utf8')
  if (!text.trim()) throw new Error(`github tracker: empty ${kind} not written`)
  const body = notes
    ? sourceNotesCommentBody(text)
    : spec
      ? epicSpecCommentBody(text)
      : planCommentBody(text)
  const selected = select(ctx.comments(args.issue))
  if (selected.status === 'conflict')
    throw new Error(
      `github tracker: several ${kind} comments (${selected.ids.join(', ')}); delete all but one ${kind} comment, then re-plan`,
    )
  if (
    (spec || notes) &&
    selected.status === 'one' &&
    recordTextFromComment(selected.comment.body) !== text
  )
    throw new Error(
      `github tracker: the ${notes ? 'source notes record' : 'spec'} is written exactly once; resume against the stored ${kind}`,
    )
  let id = selected.status === 'one' ? String(selected.comment.id) : null
  let outcome = 'unchanged'
  if (selected.status === 'none' || recordTextFromComment(selected.comment.body) !== text) {
    outcome = id ? 'updated' : 'created'
    const temp = mkdtempSync(join(tmpdir(), 'github-plan-write-'))
    let written = false
    try {
      const file = join(temp, 'body.json')
      writeFileSync(file, JSON.stringify({ body }), { mode: 0o600 })
      const result = ctx.api(
        id ? `${root}/issues/comments/${id}` : `${root}/issues/${number}/comments`,
        ['--method', id ? 'PATCH' : 'POST', '--input', file],
      )
      written = true
      if (!Number.isSafeInteger(result.id) || result.id < 1 || (id && String(result.id) !== id))
        throw new Error('unreadable comment id')
      id = String(result.id)
    } catch (error) {
      if (written || !isDefinitiveWriteRejection(error))
        throw indeterminateWriteError(`github tracker: ${verb} may have applied`, error)
      throw new Error(`github tracker: ${verb}; ${error.message}`, { cause: error })
    } finally {
      rmSync(temp, { recursive: true, force: true, maxRetries: 3 })
    }
  }
  try {
    const readback = select(ctx.comments(args.issue))
    if (
      readback.status !== 'one' ||
      String(readback.comment.id) !== id ||
      digest(recordTextFromComment(readback.comment.body)) !== digest(text)
    )
      throw new Error('plan read-back disagrees')
  } catch (error) {
    if (outcome !== 'unchanged')
      throw indeterminateWriteError(`github tracker: ${verb} may have applied`, error)
    throw error
  }
  return { id, title, outcome, bytes: Buffer.byteLength(text), sha256: digest(text) }
}
