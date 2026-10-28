import { assertCommentBody } from './github-comment.mjs'

export const EPIC_SPEC_COMMENT_MARKER = '<!-- boss-epic-spec v1 -->'

export const PLAN_COMMENT_MARKER = '<!-- boss-implementation-plan v1 -->'

export function planCommentBody(planText) {
  const body = `${PLAN_COMMENT_MARKER}\n${planText}`
  try {
    assertCommentBody(body)
  } catch {
    throw new Error(
      `github tracker: plan comment size ${body.length} exceeds limit 65536 (marker ${PLAN_COMMENT_MARKER.length + 1}); plan not written`,
    )
  }
  return body
}

export function isPlanComment(body) {
  return (
    typeof body === 'string' &&
    (body === PLAN_COMMENT_MARKER || body.startsWith(`${PLAN_COMMENT_MARKER}\n`))
  )
}

export function planTextFromComment(body) {
  if (!isPlanComment(body)) throw new Error('github tracker: comment is not a plan comment')
  return body.slice(PLAN_COMMENT_MARKER.length + 1)
}

// GitHub supplies this association; a public commenter cannot assert repository trust.
export function isTrustedPlanAuthor(comment) {
  return ['OWNER', 'MEMBER', 'COLLABORATOR'].includes(
    comment.authorAssociation ?? comment.author_association,
  )
}

function selectRecordComment(comments, matches) {
  const plans = comments.filter((comment) => isTrustedPlanAuthor(comment) && matches(comment.body))
  if (!plans.length) return { status: 'none' }
  if (plans.length === 1) return { status: 'one', comment: plans[0] }
  return {
    status: 'conflict',
    ids: plans
      .map((comment) => String(comment.id))
      .sort((a, b) => (BigInt(a) < BigInt(b) ? -1 : BigInt(a) > BigInt(b) ? 1 : 0)),
  }
}

export function planAttachmentFromComment(comment, issueId) {
  return {
    id: String(comment.id),
    title: `Implementation plan (${issueId})`,
    contentType: 'text/markdown',
    url: comment.url ?? comment.html_url,
    createdAt: comment.createdAt,
    updatedAt: comment.updatedAt,
  }
}

export function selectPlanComment(comments) {
  return selectRecordComment(comments, isPlanComment)
}

export function epicSpecCommentBody(text) {
  const body = `${EPIC_SPEC_COMMENT_MARKER}\n${text}`
  assertCommentBody(body)
  return body
}

export function isEpicSpecComment(body) {
  return (
    typeof body === 'string' &&
    (body === EPIC_SPEC_COMMENT_MARKER || body.startsWith(`${EPIC_SPEC_COMMENT_MARKER}\n`))
  )
}

export function selectEpicSpecComment(comments) {
  return selectRecordComment(comments, isEpicSpecComment)
}

export function recordTextFromComment(body) {
  if (isPlanComment(body)) return planTextFromComment(body)
  if (isEpicSpecComment(body)) return body.slice(EPIC_SPEC_COMMENT_MARKER.length + 1)
  throw new Error('github tracker: comment is not a trusted boss record kind')
}

export function epicSpecAttachmentFromComment(comment, issueId) {
  return {
    ...planAttachmentFromComment(comment, issueId),
    title: `Epic spec (${issueId})`,
    contentType: 'application/json',
  }
}
