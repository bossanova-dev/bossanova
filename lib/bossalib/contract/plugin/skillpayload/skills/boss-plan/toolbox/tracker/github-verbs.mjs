import { mkdtempSync, writeFileSync, rmSync } from 'node:fs'
import { join } from 'node:path'
import { tmpdir } from 'node:os'
import { applyRanking, createIssue } from './github-issue-create.mjs'
import { assertCommentBody } from './github-comment.mjs'
import { runPlanVerb, issuePlanView } from './github-plan-verbs.mjs'
import { checkRelationsAvailable } from './github-relations-preflight.mjs'
import { GITHUB_CLOSING_ROLES, githubRequiredLabels } from './github-config.mjs'

/** Execute a command operation using one invocation's cached repository labels. */
export async function runVerb(verb, args, ctx) {
  const { api, issue, normalizeTicket, issueNumber, states, openRoles, resolveLabel } = ctx
  const readIssue = (id) => issue('view', [ctx.relations.localNumber(id), '--json', ctx.fields])
  const comments = (id) => {
    const pages = api(`${ctx.relations.readPath(id)}/comments`, ['--paginate', '--slurp'])
    if (!Array.isArray(pages) || pages.some((page) => !Array.isArray(page)))
      throw new Error('github tracker: unreadable comments pagination')
    return pages.flat().map((comment) => ({
      id: String(comment.id),
      body: comment.body,
      createdAt: comment.created_at,
      updatedAt: comment.updated_at,
      url: comment.html_url,
      author: comment.user?.login,
      authorAssociation: comment.author_association,
    }))
  }
  const readPlanView = (ticket) => issuePlanView(ticket, comments(ticket.id))
  const mutate = (step, action) => {
    try {
      return action()
    } catch (error) {
      throw new Error(`github tracker: ${step}: HTTP 504 write may have applied; ${error.message}`)
    }
  }
  if (verb === 'create-issue')
    return createIssue(args, ctx, (input) => runVerb('move-state', input, ctx))
  if (verb === 'write-plan' || verb === 'write-epic-spec' || verb === 'read-plan')
    return runPlanVerb(verb, args, { ...ctx, comments })
  if (verb === 'preflight') return preflight(ctx)
  if (verb === 'append-dependency') return ctx.relations.appendDependency(args)
  if (verb === 'list-children') {
    const result = ctx.relations.listChildren(args.parentId)
    return {
      ...result,
      children: result.children.map((ticket) => {
        const { comments: _comments, ...child } = readPlanView(ticket)
        return child
      }),
    }
  }
  if (verb === 'select-planned')
    return ctx.selectPlanned({
      state: args.state,
      requireLabels: args.labels ?? [],
      selection: {},
      limit: args.limit,
    })
  if (verb === 'delete-comment') {
    const id = commentId(args.id)
    api(
      `repos/${ctx.identity.owner}/${ctx.identity.repo}/issues/comments/${id}`,
      ['-X', 'DELETE'],
      { json: false },
    )
    return { id, deleted: true }
  }
  if (verb === 'read-comments') return comments(args.issueId)
  if (verb === 'get-issue') return readPlanView(ctx.relations.hydrate(args.id))
  if (verb === 'read-labels') return normalizeTicket(readIssue(args.id)).labels
  if (verb === 'write-comment' || verb === 'update-comment') {
    assertCommentBody(args.body)
    if (verb === 'write-comment') ctx.relations.localNumber(args.issueId)
    const suffix =
      verb === 'write-comment'
        ? `issues/${issueNumber(args.issueId)}/comments`
        : `issues/comments/${commentId(args.id)}`
    const temp = mkdtempSync(join(tmpdir(), 'github-comment-'))
    try {
      const file = join(temp, 'body.json')
      writeFileSync(file, JSON.stringify({ body: args.body }), { mode: 0o600 })
      const result = api(`repos/${ctx.identity.owner}/${ctx.identity.repo}/${suffix}`, [
        '--method',
        verb === 'write-comment' ? 'POST' : 'PATCH',
        '--input',
        file,
      ])
      if (!Number.isSafeInteger(result.id) || result.id < 1)
        throw new Error('github tracker: unreadable comment id; write may have applied')
      return { id: String(result.id), url: result.html_url }
    } finally {
      rmSync(temp, { recursive: true, force: true, maxRetries: 3 })
    }
  }
  if (verb === 'set-priority-estimate') {
    const current = normalizeTicket(readIssue(args.id)).labels
    const labels = args.labels ?? current
    if (!Array.isArray(labels)) throw new Error('github tracker: labels must be an array')
    const desired = applyRanking(labels, args)
    return runVerb('move-state', { id: args.id, labels: desired }, ctx)
  }
  if (verb !== 'move-state') throw new Error(`github tracker: unsupported operation ${verb}`)
  if (args.state === undefined && args.labels === undefined)
    throw new Error('github tracker: move-state requires state or labels')
  const role =
    args.state === undefined
      ? null
      : Object.keys(states).find((role) => states[role] === args.state)
  if (args.state !== undefined && !role)
    throw new Error(`github tracker: unknown workflow state ${args.state}`)
  const raw = readIssue(args.id)
  const current = normalizeTicket(raw).labels
  const roleLabels = openRoles.map((role) => states[role])
  const isRole = (label) => roleLabels.some((name) => name.toLowerCase() === label.toLowerCase())
  if (
    args.labels !== undefined &&
    (!Array.isArray(args.labels) || args.labels.some((label) => typeof label !== 'string'))
  )
    throw new Error('github tracker: labels must be an array of strings')
  let desired = args.labels === undefined ? [...current] : args.labels.map(resolveLabel)
  const existing = current.map((label) => label.toLowerCase())
  const wanted = desired.map((label) => label.toLowerCase())
  if (
    args.labels !== undefined &&
    (current.some((label) => isRole(label) && !wanted.includes(label.toLowerCase())) ||
      desired.some((label) => isRole(label) && !existing.includes(label.toLowerCase())))
  )
    throw new Error('github tracker: use state to change the workflow state')
  if (role)
    desired = [
      ...desired.filter((label) => !isRole(label)),
      ...(openRoles.includes(role) ? [resolveLabel(states[role])] : []),
    ]
  // Validate all labels before reopening, closing or editing anything.
  desired = [...new Set(desired.map(resolveLabel))]
  const add = desired.filter((label) => !existing.includes(label.toLowerCase()))
  const remove = current.filter(
    (label) => !desired.some((name) => name.toLowerCase() === label.toLowerCase()),
  )
  const closed = String(raw.state).toUpperCase() === 'CLOSED'
  const number = issueNumber(args.id)
  if (
    role &&
    closed &&
    (openRoles.includes(role) || normalizeTicket(raw).stateName !== states[role])
  )
    mutate('reopen', () => issue('reopen', [number], { json: false }))
  if (add.length || remove.length)
    mutate('edit labels', () =>
      issue(
        'edit',
        [
          number,
          ...add.flatMap((label) => ['--add-label', label]),
          ...remove.flatMap((label) => ['--remove-label', label]),
        ],
        { json: false },
      ),
    )
  if (role && Object.hasOwn(GITHUB_CLOSING_ROLES, role))
    mutate('close', () =>
      issue('close', [number, '--reason', GITHUB_CLOSING_ROLES[role]], { json: false }),
    )
  const result = normalizeTicket(readIssue(args.id))
  if (role && result.stateName !== states[role])
    throw new Error('github tracker: state read-back disagrees; HTTP 504 write may have applied')
  return { id: result.id, state: result.stateName, labels: result.labels }
}

function commentId(value) {
  if (!/^[1-9]\d*$/.test(String(value))) throw new Error('github tracker: invalid comment id')
  return String(value)
}

// gh 2.88.0 added duplicate close reasons; --slurp is available since 2.48.0.
// https://github.com/cli/cli/releases/tag/v2.88.0
function preflight(ctx) {
  const fail = (status, remedy) => {
    throw new Error(`tracker preflight: ${status}: ${remedy}`)
  }
  let version
  try {
    version = ctx.gh(['--version'], { json: false })
  } catch {
    fail('gh-missing', 'install the GitHub CLI (https://cli.github.com), then run gh auth login')
  }
  const match = /gh version (\d+)\.(\d+)\.(\d+)/.exec(version)
  if (!match || Number(match[1]) < 2 || (Number(match[1]) === 2 && Number(match[2]) < 88))
    fail('gh-outdated', 'upgrade the GitHub CLI to 2.88.0 or newer')
  try {
    ctx.gh(['auth', 'status', '--hostname', ctx.identity.host], { json: false })
  } catch {
    fail('unauthenticated', `run gh auth login --hostname ${ctx.identity.host}`)
  }
  let repo
  try {
    repo = ctx.api(`repos/${ctx.identity.owner}/${ctx.identity.repo}`)
  } catch {
    fail('repository-unavailable', `check access to ${ctx.identity.repository} with gh repo view`)
  }
  if (!repo.has_issues)
    fail('issues-disabled', "enable Issues in the repository's Settings, General, Features")
  const required = githubRequiredLabels(ctx.block).map(({ name }) => name)
  const known = ctx.repositoryLabels().map((name) => name.toLowerCase())
  const missing = required.filter((name) => !known.includes(name.toLowerCase()))
  if (missing.length)
    fail(
      'labels-missing',
      `${missing.join(', ')}; create them (gh label create <name>) or run boss init --merge --create-labels`,
    )
  checkRelationsAvailable(ctx, fail)
  const viewer = ctx.api('user').login
  if (typeof viewer !== 'string' || !viewer) fail('unauthenticated', 'run gh auth login')
  return { ok: true, repository: ctx.identity.repository, viewer }
}
