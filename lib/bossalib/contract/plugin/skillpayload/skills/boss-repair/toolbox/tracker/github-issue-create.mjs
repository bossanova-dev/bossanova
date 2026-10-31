import { indeterminateWriteError, isDefinitiveWriteRejection } from './outcome.mjs'
import { mkdtempSync, writeFileSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { assertCommentBody } from './github-comment.mjs'
import { githubSelectMarked } from './github-marked.mjs'

/** Replace only the ranking fields explicitly supplied by the caller. */
export function applyRanking(labels, { priority, estimate }) {
  let desired = [...labels]
  if (priority !== undefined) {
    if (!Number.isInteger(priority) || priority < 0 || priority > 4)
      throw new Error('github tracker: priority must be 0..4')
    desired = desired.filter((label) => !/^boss:priority-[1-4]$/i.test(label))
    if (priority) desired.push(`boss:priority-${priority}`)
  }
  if (estimate !== undefined) {
    if (!Number.isInteger(estimate) || estimate < 0)
      throw new Error('github tracker: estimate must be a non-negative integer')
    desired = desired.filter((label) => !/^boss:estimate-\d+$/i.test(label))
    desired.push(`boss:estimate-${estimate}`)
  }
  return desired
}

/** Create inertly, verify native parent membership, then expose the workflow state. */
export async function createIssue(args, ctx, moveState) {
  if (args.project !== undefined) throw new Error('project is not supported by the github tracker')
  if (typeof args.title !== 'string' || !args.title.trim() || args.title.length > 256)
    throw new Error('github tracker: title must contain 1..256 characters')
  if (typeof args.description !== 'string' || !args.description.trim())
    throw new Error('github tracker: description is required')
  assertCommentBody(args.description)
  const role = Object.keys(ctx.states).find((role) => ctx.states[role] === args.state)
  if (!role) throw new Error('github tracker: unknown workflow state')
  if (
    !Array.isArray(args.labels ?? []) ||
    (args.labels ?? []).some((name) => typeof name !== 'string')
  )
    throw new Error('github tracker: labels must be an array of strings')
  const roleLabels = ctx.openRoles.map((role) => ctx.states[role].toLowerCase())
  let labels = (args.labels ?? []).map(ctx.resolveLabel)
  if (labels.some((name) => roleLabels.includes(name.toLowerCase())))
    throw new Error('github tracker: labels must not include a workflow state')
  if (ctx.openRoles.includes(role)) ctx.resolveLabel(args.state)
  const resolved = applyRanking(labels, args).map(ctx.resolveLabel)
  const seen = new Set()
  labels = resolved.filter((name) => {
    const key = name.toLowerCase()
    if (seen.has(key)) return false
    seen.add(key)
    return true
  })
  const parent = args.parentId === undefined ? null : ctx.relations.localNumber(args.parentId)
  const root = `repos/${ctx.identity.owner}/${ctx.identity.repo}/issues`
  const api = (path, flags) => ctx.api(path, ['-H', 'X-GitHub-Api-Version: 2026-03-10', ...flags])
  const temp = mkdtempSync(join(tmpdir(), 'github-create-'))
  let number
  let created = false
  try {
    const file = join(temp, 'issue.json')
    writeFileSync(file, JSON.stringify({ title: args.title, body: args.description, labels }), {
      mode: 0o600,
    })
    const child = api(root, ['--method', 'POST', '--input', file])
    created = true
    number = child.number
    if (
      !Number.isSafeInteger(number) ||
      number < 1 ||
      !Number.isSafeInteger(child.id) ||
      child.id < 1
    )
      throw new Error('unreadable created issue REST id or number')
    if (parent) {
      // GitHub REST requires the issue's global id, not its repository number.
      // https://docs.github.com/en/rest/issues/sub-issues
      writeFileSync(file, JSON.stringify({ sub_issue_id: child.id }), { mode: 0o600 })
      api(`${root}/${parent}/sub_issues`, ['--method', 'POST', '--input', file])
      const pages = api(`${root}/${parent}/sub_issues`, ['--paginate', '--slurp'])
      if (
        !Array.isArray(pages) ||
        pages.some((page) => !Array.isArray(page)) ||
        !pages.flat().some((row) => row.id === child.id && row.number === number)
      )
        throw new Error('sub-issue link read-back disagrees')
    }
    await moveState({ id: `GH-${number}`, state: args.state })
    return {
      id: `GH-${number}`,
      identifier: `GH-${number}`,
      url: child.html_url,
      parentId: parent ? `GH-${parent}` : null,
    }
  } catch (error) {
    const context = `github tracker: create-issue${number ? ` orphan GH-${number}` : ''}`
    if (created || !isDefinitiveWriteRejection(error))
      throw indeterminateWriteError(`${context}; write may have applied`, error)
    throw new Error(`${context}; ${error.message}`, { cause: error })
  } finally {
    rmSync(temp, { recursive: true, force: true, maxRetries: 3 })
  }
}

/** Resolve names before creating, and settle uncertain writes without sending another POST. */
export async function githubApplyIssueCreate(input, ctx) {
  const keys = ['title', 'description', 'stateName', 'labels', 'marker']
  const nonEmpty = (value) => typeof value === 'string' && value.trim().length > 0
  if (!input || typeof input !== 'object' || Array.isArray(input))
    throw new Error('github applyIssueCreate: input must be an object')
  if (Object.keys(input).some((key) => !keys.includes(key)))
    throw new Error('github applyIssueCreate: unknown input keys')
  if (!nonEmpty(input.title) || input.title.length > 256)
    throw new Error('github applyIssueCreate: title must contain 1..256 characters')
  if (!nonEmpty(input.description))
    throw new Error('github applyIssueCreate: description is required')
  if (!nonEmpty(input.stateName)) throw new Error('github applyIssueCreate: stateName is required')
  if (input.labels !== undefined && (!Array.isArray(input.labels) || !input.labels.every(nonEmpty)))
    throw new Error('github applyIssueCreate: labels must be an array of non-empty names')
  if (input.marker !== undefined && (!nonEmpty(input.marker) || /[\r\n]/.test(input.marker)))
    throw new Error('github applyIssueCreate: marker must be a non-empty single line')
  let description = input.description
  if (input.marker && !description.includes(input.marker)) description += `\n\n${input.marker}`
  assertCommentBody(description)
  const failed = (reason, extra = {}) => ({ ok: false, outcome: 'failed', reason, ...extra })
  const success = (row, existing = false) => ({
    ok: true,
    outcome: 'ok',
    issue: { id: row.id, identifier: row.identifier, url: row.url },
    ...(existing ? { existing: true } : {}),
  })
  const role = Object.keys(ctx.states).find((role) => ctx.states[role] === input.stateName)
  if (!role) return failed(`state-not-found: ${input.stateName}`, { permanent: true })
  const roleLabels = ctx.openRoles.map((role) => ctx.states[role].toLowerCase())
  if ((input.labels ?? []).some((name) => roleLabels.includes(name.toLowerCase())))
    return failed('workflow-label: use stateName to change the workflow state', { permanent: true })
  try {
    for (const name of [
      ...(input.labels ?? []),
      ...(ctx.openRoles.includes(role) ? [input.stateName] : []),
    ])
      ctx.resolveLabel(name)
  } catch (error) {
    return failed(
      `label-resolution: ${error.message}`,
      error.message.includes('missing label') ? { permanent: true } : {},
    )
  }
  const lookup = async () => {
    const rows = await githubSelectMarked({ markerPrefix: input.marker }, ctx)
    if (rows.length > 1) return failed('marker-ambiguous')
    return rows.length === 1 ? success(rows[0], true) : null
  }
  if (input.marker) {
    try {
      const existing = await lookup()
      if (existing) return existing
    } catch (error) {
      return failed(`marker-read-failed: ${error.message}`)
    }
  }
  try {
    return success(
      await createIssue(
        {
          title: input.title,
          description,
          state: input.stateName,
          labels: input.labels,
        },
        ctx,
        ctx.moveState,
      ),
    )
  } catch (error) {
    if (error.outcome !== 'indeterminate') return failed(`create: ${error.message}`)
    if (input.marker) {
      try {
        const settled = await lookup()
        if (settled) return settled
      } catch {
        // An unsuccessful read cannot establish whether the one write applied.
      }
    }
    return { ok: false, outcome: 'indeterminate', reason: `create: ${error.message}` }
  }
}
