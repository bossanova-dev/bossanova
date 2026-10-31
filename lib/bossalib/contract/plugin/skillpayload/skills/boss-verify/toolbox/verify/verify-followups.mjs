import { parseArgs } from 'node:util'
import { isMainModule } from '../main-module.mjs'
import { loadSkillConfig, stateName, labelName } from '../skill-config.mjs'
import { resolveTrackerAdapter } from '../tracker/adapter.mjs'
import {
  UsageError,
  commandReader,
  repoFor,
  readPr,
  viewerLogin,
  commentsReadCall,
  patchPrComment,
} from './verify-gate.mjs'
import { notesComment, parseNotesTrailer, carryTickets, followUpIssue } from './verify-notes.mjs'

function newestNotes(read, repo, pr, login) {
  const result = read(...commentsReadCall(repo, pr))
  if (
    !result.ok ||
    !Array.isArray(result.payload) ||
    !result.payload.every(
      (page) =>
        Array.isArray(page) &&
        page.every(
          (row) =>
            row &&
            typeof row === 'object' &&
            Number.isSafeInteger(row.id) &&
            typeof row.body === 'string' &&
            typeof row.user?.login === 'string' &&
            Number.isFinite(Date.parse(row.created_at)),
        ),
    )
  )
    return { reason: 'comments-unreadable' }
  const comments = result.payload
    .flat()
    .filter((row) => row.user.login === login)
    .map((row) => ({ ...row, trailer: parseNotesTrailer(row.body) }))
    .filter((row) => row.trailer)
    .sort((a, b) => Date.parse(b.created_at) - Date.parse(a.created_at) || b.id - a.id)
  return comments[0] ? { target: comments[0] } : { reason: 'notes-absent' }
}

/** File selected notes through the tracker capability and update only our existing notes comment. */
export async function followUp(options, deps = {}) {
  // Tracker creates do not invalidate the gate reader cache; this re-read must reach GitHub.
  const read = deps.read ?? commandReader(deps)
  const repo = repoFor(read, options.repo)
  const refuse = (reason, extra = {}) => ({ ok: false, pr: options.pr, repo, reason, ...extra })
  const view = readPr(read, options.pr, repo)
  if (!view.ok || !/^https?:\/\//.test(view.payload?.url ?? '')) return refuse('pr-unreadable')
  const login = viewerLogin(read)
  if (!login) return refuse('viewer-unreadable')
  const initial = newestNotes(read, repo, options.pr, login)
  if (initial.reason) return refuse(initial.reason)
  const selectedIds = options.note ?? []
  const unknown = selectedIds.filter((id) => !initial.target.trailer.notes.some((n) => n.id === id))
  if (unknown.length) return refuse('unknown-note', { ids: unknown })
  let adapter
  try {
    adapter = Object.hasOwn(deps, 'adapter')
      ? deps.adapter
      : resolveTrackerAdapter({ env: deps.env ?? process.env })
  } catch {
    adapter = null
  }
  if (typeof adapter?.applyIssueCreate !== 'function') return refuse('tracker-cannot-create')
  let unplanned, agentPlan
  try {
    const config = (deps.loadConfig ?? loadSkillConfig)()
    unplanned = stateName(config, 'unplanned')
    agentPlan = labelName(config, 'agentPlan')
  } catch {
    return refuse('tracker-unconfigured')
  }
  const selected = initial.target.trailer.notes.filter(
    (n) => options.all || selectedIds.includes(n.id),
  )
  /** @type {Array<{note: string, outcome: string, ticket?: {identifier: string, url?: string}, linked?: boolean, reason?: string}>} */
  const results = [],
    writes = [],
    linked = {}
  for (const note of selected) {
    const existing = initial.target.trailer.tickets[note.id]
    if (existing) {
      results.push({ note: note.id, outcome: 'already-filed', ticket: existing })
      continue
    }
    const issue = followUpIssue({ repo, pr: options.pr, prUrl: view.payload.url, note })
    if (options.dryRun) {
      writes.push({
        kind: 'tracker-create',
        note: note.id,
        title: issue.title,
        marker: issue.marker,
      })
      results.push({ note: note.id, outcome: 'planned' })
      continue
    }
    let created
    try {
      created = await adapter.applyIssueCreate({
        ...issue,
        stateName: unplanned,
        labels: [agentPlan],
      })
    } catch {
      created = { outcome: 'failed', reason: 'tracker-error' }
    }
    if (created.outcome !== 'ok') {
      results.push({
        note: note.id,
        outcome: created.outcome === 'indeterminate' ? 'indeterminate' : 'failed',
        ...(created.reason ? { reason: created.reason } : {}),
      })
      continue
    }
    const ticket = { identifier: created.issue?.identifier, url: created.issue?.url }
    if (
      typeof ticket.identifier !== 'string' ||
      !ticket.identifier.trim() ||
      /[\r\n]/.test(ticket.identifier) ||
      !/^https?:\/\/[^\s]+$/.test(ticket.url ?? '')
    ) {
      results.push({ note: note.id, outcome: 'failed', reason: 'ticket-url-missing', ticket })
      continue
    }
    linked[note.id] = ticket
    results.push({ note: note.id, outcome: created.existing ? 'already-filed' : 'created', ticket })
  }
  const latest = newestNotes(read, repo, options.pr, login)
  const base = {
    pr: options.pr,
    repo,
    head: latest.target?.trailer.head ?? initial.target.trailer.head,
    results,
  }
  if (latest.reason) return { ...base, ok: false, comment: 'failed', reason: latest.reason }
  const target = latest.target
  if (options.dryRun) {
    if (writes.length) writes.push({ kind: 'pr-comment', replaces: target.id })
    return { ...base, ok: true, comment: 'planned', dryRun: true, writes }
  }
  for (const result of results) {
    if (
      ['created', 'already-filed'].includes(result.outcome) &&
      !target.trailer.notes.some((n) => n.id === result.note)
    ) {
      result.linked = false
      result.reason = 'note-gone'
    }
  }
  const tickets = {
    ...target.trailer.tickets,
    ...carryTickets({ tickets: linked }, target.trailer.notes),
  }
  const rendered = notesComment({ head: target.trailer.head, notes: target.trailer.notes, tickets })
  const comment =
    rendered.digest === target.trailer.digest
      ? 'current'
      : patchPrComment(read, repo, target.id, rendered.body)
        ? 'updated'
        : 'failed'
  return {
    ...base,
    ok:
      comment !== 'failed' &&
      results.every((r) => ['created', 'already-filed'].includes(r.outcome)),
    comment,
  }
}

export async function main(argv = process.argv.slice(2), deps = {}) {
  let values, positionals
  try {
    ;({ values, positionals } = parseArgs({
      args: argv,
      strict: true,
      allowPositionals: true,
      options: {
        pr: { type: 'string' },
        repo: { type: 'string' },
        note: { type: 'string', multiple: true },
        all: { type: 'boolean' },
        'dry-run': { type: 'boolean' },
        json: { type: 'boolean' },
      },
    }))
  } catch (error) {
    throw new UsageError(error.message)
  }
  const [command] = positionals
  if (
    positionals.length !== 1 ||
    command !== 'follow-up' ||
    !/^[1-9]\d*$/.test(values.pr ?? '') ||
    !Number.isSafeInteger(Number(values.pr)) ||
    Boolean(values.all) === Boolean(values.note?.length) ||
    values.note?.some((id) => !/^note:[a-f0-9]{12}$/.test(id))
  )
    throw new UsageError('follow-up needs --pr <integer> and either --all or --note <note:12-hex>')
  return followUp({ ...values, pr: Number(values.pr), dryRun: values['dry-run'] === true }, deps)
}

if (isMainModule(import.meta.url)) {
  try {
    const result = await main()
    console.log(JSON.stringify(result))
    process.exitCode = result.ok ? 0 : 1
  } catch (error) {
    console.log(JSON.stringify({ ok: false, reason: error.message }))
    process.exitCode = error instanceof UsageError ? 2 : 1
  }
}
