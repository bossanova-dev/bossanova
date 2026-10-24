import * as nativeFs from 'node:fs'
import { randomBytes } from 'node:crypto'
import { dirname, join } from 'node:path'

const text = (value) => typeof value === 'string' && value.trim() !== ''

// The PR body's first non-blank line is `Linear issue: <url>`. Returns `{url, id}` or null.
export function parseLinearIssueLine(body) {
  if (typeof body !== 'string') return null
  const first = body.split(/\r?\n/).find((line) => line.trim() !== '')
  const match = first?.trim().match(/^Linear issue:\s*(\S+)\s*$/i)
  if (!match) return null
  const id = match[1].match(/\/issue\/([A-Za-z][A-Za-z0-9]*-\d+)(?:[/?#]|$)/)?.[1]
  return id ? { url: match[1], id: id.toUpperCase() } : null
}

// The PR numbers a ticket's attachments name in this repository (`…/<owner>/<repo>/pull/<n>`).
export function attachedPrNumbers(ticket, repo) {
  const out = new Set()
  if (!text(repo)) return out
  const escaped = repo.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')
  const re = new RegExp(`github\\.com/${escaped}/pull/(\\d+)(?:[/?#]|$)`, 'i')
  for (const attachment of ticket?.attachments ?? []) {
    const hit = typeof attachment?.url === 'string' ? attachment.url.match(re) : null
    if (hit) out.add(Number(hit[1]))
  }
  return out
}

// bossd names every cron fire's branch `cron-<slug>-<unix seconds>` (cronBranchName in
// services/bossd/internal/cron/scheduler.go), so a PR on one was opened by an unattended cron run.
const CRON_FIRE_BRANCH = /^cron-[a-z0-9]+(?:-[a-z0-9]+)*-\d+$/
export function isCronFireBranch(name) {
  return typeof name === 'string' && CRON_FIRE_BRANCH.test(name)
}

// A selection admits a PR with no ticket only when it has no include slot: an include filters on
// ticket fields such a PR lacks, and an exclude can never match it.
export function selectionAdmitsTicketless(selection) {
  return !Object.values(selection ?? {}).some(
    (slots) => Array.isArray(slots?.include) && slots.include.length > 0,
  )
}

const once = (read) => {
  let called = false
  let value
  return () => (called ? value : ((called = true), (value = read())))
}
/**
 * Does no tracker ticket stand behind PR `p`? The one answer candidates, post and merge share:
 * 'ticketless' when it is on a bossd cron fire branch, its body has no ticket line, no in-review
 * ticket's attachment names it (`claimed()`, the claimedPrNumbers set or null when unreadable) and
 * its live session row, matched by exact `pr_url`, carries no tracker id (`sessions()`, the
 * `boss ls --json` rows or null when unreadable). Otherwise the reason it is not: 'not-factory' (no
 * cron fire branch, or a ticket line), 'claimed', 'no-session', 'session-has-ticket',
 * 'unknown:claims' or 'unknown:sessions'. Each read runs only when the answer turns on it.
 */
export async function ticketlessReason(p, { claimed, sessions }) {
  if (!isCronFireBranch(p?.headRefName) || parseLinearIssueLine(p?.body) !== null)
    return 'not-factory'
  const claims = await claimed()
  if (!(claims instanceof Set)) return 'unknown:claims'
  if (claims.has(p.number)) return 'claimed'
  const rows = sessions()
  if (!Array.isArray(rows)) return 'unknown:sessions'
  const live = rows.filter((row) => text(p.url) && row?.pr_url === p.url && text(row?.id))
  if (live.length === 0) return 'no-session'
  return live.some((row) => text(row.tracker_id)) ? 'session-has-ticket' : 'ticketless'
}

/**
 * The ticketless factory PRs among the open `prs` (ticketlessReason), each read made at most once.
 * `{prs, skipped}`: a PR some ticket owns, or that is no factory PR, is left out silently; any
 * other skip (`ticket: null`) is `draft` or ticketlessReason's reason.
 */
export async function ticketlessFactoryPrs(
  prs,
  { claimed = async () => null, sessions = () => null } = {},
) {
  const facts = { claimed: once(claimed), sessions: once(sessions) }
  const out = { prs: [], skipped: [] }
  for (const p of Array.isArray(prs) ? prs : []) {
    const reason = await ticketlessReason(p, facts)
    if (reason === 'not-factory' || reason === 'claimed') continue
    const skip = p.isDraft === true ? 'draft' : reason === 'ticketless' ? null : reason
    if (skip) out.skipped.push({ ticket: null, pr: p.number, reason: skip })
    else out.prs.push(p)
  }
  return out
}

/** The policy's answer for every scanned PR, independent of which machine acts on it. */
export function classifyAdmission({
  prs = [],
  linked = [],
  ticketlessSkipped = [],
  tickets = [],
  repo = '',
  admitsTicketless = false,
} = {}) {
  const admitted = new Map(
    linked.map(({ ticket, pr }) => [pr.number, ticket ? 'ticket' : 'ticketless-factory']),
  )
  const attached = new Set(tickets.flatMap((ticket) => [...attachedPrNumbers(ticket, repo)]))
  const skips = new Map(ticketlessSkipped.map((skip) => [skip.pr, skip.reason]))
  return prs.map((pr) => {
    let state = 'excluded'
    let reason = 'no-ticket'
    if (admitted.has(pr.number)) {
      state = 'admitted'
      reason = admitted.get(pr.number)
    } else if (pr.isDraft === true) {
      state = 'pending'
      reason = 'draft'
    } else if (skips.get(pr.number) === 'ambiguous-pr') {
      state = 'pending'
      reason = 'ambiguous-pr'
    } else if (parseLinearIssueLine(pr.body)) {
      state = 'pending'
      reason = 'ticket-not-in-review'
    } else if (attached.has(pr.number)) {
      state = 'pending'
      reason = 'attached'
    } else if (isCronFireBranch(pr.headRefName)) {
      state = admitsTicketless ? 'pending' : 'excluded'
      reason = admitsTicketless ? (skips.get(pr.number) ?? 'claimed') : 'selection'
    }
    return { pr: pr.number, head: String(pr.headRefOid ?? '').toLowerCase(), state, reason }
  })
}

export const ADMISSION_VERSION = 1

export function admissionPath(commonDir) {
  return join(commonDir, 'boss-verify', 'verify-admission.json')
}

/** Atomically replace a JSON snapshot; a failed write never interrupts a sweep. */
export function writeAtomicJson(fs, file, value) {
  let tmp = ''
  try {
    fs.mkdirSync(dirname(file), { recursive: true })
    tmp = `${file}.${process.pid}.${randomBytes(4).toString('hex')}.tmp`
    fs.writeFileSync(tmp, `${JSON.stringify(value)}\n`, { flag: 'wx' })
    fs.renameSync(tmp, file)
    return true
  } catch {
    try {
      if (tmp) fs.rmSync(tmp, { force: true })
    } catch {
      /* Best-effort cleanup. */
    }
    return false
  }
}

function readScopes(fs, file) {
  try {
    const data = JSON.parse(fs.readFileSync(file, 'utf8'))
    return data?.version === ADMISSION_VERSION &&
      data.scopes &&
      typeof data.scopes === 'object' &&
      !Array.isArray(data.scopes)
      ? data.scopes
      : {}
  } catch {
    return {}
  }
}

/** A full scan replaces its scope; an event changes only that PR in the same scope. */
export function storeAdmission(
  file,
  key,
  admission,
  scannedAt,
  { eventPr = null, fs = nativeFs } = {},
) {
  if (!text(file)) return false
  const scopes = readScopes(fs, file)
  const prs = eventPr === null ? {} : { ...scopes[key]?.prs }
  for (const { pr, head, state, reason } of admission) {
    if (eventPr !== null && pr !== eventPr) continue
    prs[pr] = { head: String(head).toLowerCase(), state, reason }
  }
  return writeAtomicJson(fs, file, {
    version: ADMISSION_VERSION,
    scopes: { ...scopes, [key]: { scannedAt, prs } },
  })
}
