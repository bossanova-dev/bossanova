import { runVerb } from './github-verbs.mjs'

const KEYS = new Set([
  'issueId',
  'addLabels',
  'removeLabels',
  'comment',
  'mentionCreator',
  'marker',
  'stateName',
])
const nonEmpty = (value) => typeof value === 'string' && value.trim().length > 0
const key = (name) => name.toLowerCase()

function nameList(value, name) {
  if (value === undefined || value === null) return []
  if (!Array.isArray(value) || !value.every(nonEmpty))
    throw new Error(`github applyIssueWrites: ${name} must be an array of non-empty names`)
  return [...new Map(value.map((name) => [key(name.trim()), name.trim()])).values()]
}

/** Apply verify's label, comment and state writes through the adapter's existing command verbs. */
export async function githubApplyIssueWrites(writes, ctx) {
  const unknown = Object.keys(writes).filter((name) => !KEYS.has(name))
  if (unknown.length)
    throw new Error(`github applyIssueWrites: unknown key(s) ${unknown.join(', ')}`)
  if (!nonEmpty(writes.issueId)) throw new Error('github applyIssueWrites: issue id is required')
  const add = nameList(writes.addLabels, 'addLabels')
  const remove = nameList(writes.removeLabels, 'removeLabels')
  for (const name of ['comment', 'marker', 'stateName'])
    if (writes[name] !== undefined && writes[name] !== null && !nonEmpty(writes[name]))
      throw new Error(`github applyIssueWrites: ${name} must be a non-empty string`)
  if (add.some((name) => remove.some((other) => key(name) === key(other))))
    throw new Error('github applyIssueWrites: a label cannot be both added and removed')
  const issueId = writes.issueId.trim()
  // Validate identifier grammar before any gh request, as the command verbs do.
  const number = ctx.relations.localNumber(issueId)
  const applied = []
  let summary
  const done = (outcome, extra = {}) => ({
    ok: outcome === 'ok',
    outcome,
    applied,
    ...(summary ? { issue: summary } : {}),
    ...extra,
  })
  const failed = (reason, extra = {}) => done('failed', { reason, ...extra })
  let ticket
  try {
    ticket = ctx.normalizeTicket(ctx.issue('view', [number, '--json', ctx.fields]))
    summary = { id: ticket.id, identifier: ticket.identifier, labels: ticket.labels }
  } catch (error) {
    return failed(`issue-read-failed: ${error.message}`)
  }
  const state = nonEmpty(writes.stateName) ? writes.stateName.trim() : null
  if (state && !Object.values(ctx.states).includes(state))
    return failed(`state-not-found: ${state}`, { permanent: true })
  const roles = ctx.openRoles.map((role) => key(ctx.states[role]))
  if ([...add, ...remove].some((name) => roles.includes(key(name))))
    return failed('workflow-label: use stateName to change the workflow state', { permanent: true })
  const resolved = []
  for (const name of [...add, ...(state && roles.includes(key(state)) ? [state] : [])]) {
    try {
      resolved.push(ctx.resolveLabel(name))
    } catch (error) {
      return failed(
        error.message.includes('missing label')
          ? `label-not-found: ${name}`
          : `label-read-failed: ${error.message}`,
        error.message.includes('missing label') ? { permanent: true } : {},
      )
    }
  }
  const toAdd = resolved
    .slice(0, add.length)
    .filter((name) => !ticket.labels.some((label) => key(label) === key(name)))
  const toRemove = ticket.labels.filter((label) => remove.some((name) => key(label) === key(name)))
  const hasMarker = (comments) =>
    nonEmpty(writes.marker) &&
    comments.some(
      (comment) => typeof comment.body === 'string' && comment.body.includes(writes.marker),
    )
  let existing = false
  if (nonEmpty(writes.comment) && nonEmpty(writes.marker)) {
    try {
      existing = hasMarker(await runVerb('read-comments', { issueId }, ctx))
    } catch (error) {
      return failed(`comment-read-failed: ${error.message}`)
    }
  }
  if (toAdd.length || toRemove.length) {
    try {
      await runVerb(
        'move-state',
        {
          id: issueId,
          labels: [...ticket.labels.filter((label) => !toRemove.includes(label)), ...toAdd],
        },
        ctx,
      )
      applied.push(
        ...toAdd.map((name) => ({ kind: 'addLabel', name })),
        ...toRemove.map((name) => ({ kind: 'removeLabel', name })),
      )
    } catch (error) {
      return done('indeterminate', {
        reason: `labels: ${error.message}`,
      })
    }
  }
  if (nonEmpty(writes.comment)) {
    if (existing) applied.push({ kind: 'comment', existing: true })
    else {
      const mention =
        writes.mentionCreator === true && nonEmpty(ticket.creatorId)
          ? `@${ticket.creatorId}\n\n`
          : ''
      let body = `${mention}${writes.comment}`
      if (nonEmpty(writes.marker) && !body.includes(writes.marker)) body += `\n\n${writes.marker}`
      try {
        await runVerb('write-comment', { issueId, body }, ctx)
        applied.push({ kind: 'comment' })
      } catch (error) {
        // The command was sent once. A marker can establish success without retrying the write.
        let landed = false
        if (nonEmpty(writes.marker)) {
          try {
            landed = hasMarker(await runVerb('read-comments', { issueId }, ctx))
          } catch {
            /* Keep the indeterminate result. */
          }
        }
        if (landed) applied.push({ kind: 'comment', settled: true })
        else return done('indeterminate', { reason: `comment: ${error.message}` })
      }
    }
  }
  if (state) {
    try {
      await runVerb('move-state', { id: issueId, state }, ctx)
      applied.push({ kind: 'state', name: state })
    } catch (error) {
      return done('indeterminate', {
        reason: `state: ${error.message}`,
      })
    }
  }
  return done('ok')
}
