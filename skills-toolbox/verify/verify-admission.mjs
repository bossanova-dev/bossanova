import * as nativeFs from 'node:fs'
import { randomBytes } from 'node:crypto'
import { dirname, join } from 'node:path'

const text = (value) => typeof value === 'string' && value.trim() !== ''

// The PR body's first non-blank line names its tracker ticket. Returns `{url, id}` or null.
export function parseTicketLine(body) {
  if (typeof body !== 'string') return null
  const first = body.split(/\r?\n/).find((line) => line.trim() !== '')
  const match = first?.trim().match(/^(?:Tracker|Linear) issue:\s*(\S+)\s*$/i)
  if (!match) return null
  const id = match[1].match(/\/issue\/([A-Za-z][A-Za-z0-9]*-\d+)(?:[/?#]|$)/)?.[1]
  if (id) return { url: match[1], id: id.toUpperCase() }
  const githubNumber = match[1].match(
    /^https:\/\/[^/?#]+\/[^/?#]+\/[^/?#]+\/issues\/([1-9]\d*)(?:\/)?(?:[?#].*)?$/,
  )?.[1]
  return githubNumber ? { url: match[1], id: `GH-${githubNumber}` } : null
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

// GraphQL names a GitHub App author `Bot`; `gh --json author` says `is_bot`, and its login is
// `app/<slug>` (REST and webhooks spell it `<slug>[bot]`).
const isBotAuthor = (author) => {
  const login = typeof author?.login === 'string' ? author.login : ''
  return (
    author?.__typename === 'Bot' ||
    author?.is_bot === true ||
    /\[bot\]$/i.test(login) ||
    /^app\//i.test(login)
  )
}

/**
 * Who stands behind PR `pr`, the admission policy's trust class: `fork` when its head lives in
 * another repository; `boss` when `linked` (a candidates link: a ticket or a ticketless cron PR), its
 * body has a ticket line or it is on a cron fire branch; `bot` when a GitHub App opened it; `human`
 * otherwise. Judge has no attachment linkage and asks with `linked: false`, so a PR an in-review
 * ticket links only by attachment is `boss` at admission but `human` at judge, which errs toward
 * more review and a person merging.
 * @returns {'fork' | 'boss' | 'bot' | 'human'}
 */
export function prClass(pr, { linked = false } = {}) {
  if (pr?.isCrossRepository === true) return 'fork'
  if (linked || parseTicketLine(pr?.body) !== null || isCronFireBranch(pr?.headRefName))
    return 'boss'
  return isBotAuthor(pr?.author) ? 'bot' : 'human'
}

// A PR's label names, lower-cased: the batched read maps them to strings, `gh --json labels` gives
// `{name}` objects. GitHub label names are unique without regard to case.
const labelSet = (pr) =>
  new Set(
    (Array.isArray(pr?.labels) ? pr.labels : [])
      .map((label) => (typeof label === 'string' ? label : label?.name))
      .filter(text)
      .map((name) => name.trim().toLowerCase()),
  )

/**
 * The admission label roles (`optIn`, `optOut`, `mergeOnPass`) PR `pr` carries under `policy` (a
 * verifyAdmissionPolicy result), sorted; `[]` when `policy` is null.
 * @returns {string[]}
 */
export function admissionLabelHits(pr, policy) {
  if (!policy) return []
  const labels = labelSet(pr)
  return Object.entries(policy.labels ?? {})
    .filter(([, name]) => text(name) && labels.has(name.trim().toLowerCase()))
    .map(([role]) => role)
    .sort()
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
  if (!isCronFireBranch(p?.headRefName) || parseTicketLine(p?.body) !== null) return 'not-factory'
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

/**
 * The policy's answer for every scanned PR, independent of which machine acts on it. With an
 * admission `policy` (verifyAdmissionPolicy; null keeps the ticket-only answers and ignores labels), the
 * optOut label excludes any PR (`opted-out`), and a PR no ticket stands behind is `policy:<class>`
 * when its class admits or it carries the optIn label (only on a scan that admits ticketless PRs,
 * else `selection`), and `not-admitted:<class>` otherwise.
 */
export function classifyAdmission({
  prs = [],
  linked = [],
  ticketlessSkipped = [],
  tickets = [],
  repo = '',
  admitsTicketless = false,
  policy = null,
} = {}) {
  const admitted = new Map(
    linked.map(({ ticket, pr }) => [pr.number, ticket ? 'ticket' : 'ticketless-factory']),
  )
  const attached = new Set(tickets.flatMap((ticket) => [...attachedPrNumbers(ticket, repo)]))
  const skips = new Map(ticketlessSkipped.map((skip) => [skip.pr, skip.reason]))
  return prs.map((pr) => {
    let state = 'excluded'
    let reason = 'no-ticket'
    const hits = admissionLabelHits(pr, policy)
    if (hits.includes('optOut')) {
      reason = 'opted-out'
    } else if (admitted.has(pr.number)) {
      state = 'admitted'
      reason = admitted.get(pr.number)
    } else if (pr.isDraft === true) {
      state = 'pending'
      reason = 'draft'
    } else if (skips.get(pr.number) === 'ambiguous-pr') {
      state = 'pending'
      reason = 'ambiguous-pr'
    } else if (parseTicketLine(pr.body)) {
      state = 'pending'
      reason = 'ticket-not-in-review'
    } else if (attached.has(pr.number)) {
      state = 'pending'
      reason = 'attached'
    } else if (isCronFireBranch(pr.headRefName)) {
      state = admitsTicketless ? 'pending' : 'excluded'
      reason = admitsTicketless ? (skips.get(pr.number) ?? 'claimed') : 'selection'
    } else if (policy) {
      const cls = prClass(pr)
      const admits = policy[cls]?.admit === true || hits.includes('optIn')
      if (admits && admitsTicketless) state = 'admitted'
      reason = admits ? (admitsTicketless ? `policy:${cls}` : 'selection') : `not-admitted:${cls}`
    }
    return { pr: pr.number, head: String(pr.headRefOid ?? '').toLowerCase(), state, reason }
  })
}

/**
 * The candidates links a scan works from once `admission` (classifyAdmission) has answered: `linked`
 * without the PRs the optOut label excluded, plus every PR the policy admitted (`policy:<class>`),
 * with `ticket: null` and its `class`, unless `excluded` (a URL set) names it, ordered by PR number.
 * `optedOut` is a candidates skip (`opted-out`) for each link dropped.
 */
export function admittedLinks({
  linked = [],
  admission = [],
  prs = [],
  excluded = new Set(),
} = {}) {
  const answers = new Map(admission.map((answer) => [answer.pr, answer]))
  const optedOut = linked.filter(({ pr }) => answers.get(pr.number)?.reason === 'opted-out')
  const admitted = prs
    .filter((pr) => answers.get(pr.number)?.reason?.startsWith('policy:') && !excluded.has(pr.url))
    .map((pr) => ({ ticket: null, pr, class: prClass(pr) }))
  return {
    linked: [...linked.filter((link) => !optedOut.includes(link)), ...admitted].sort(
      (a, b) => a.pr.number - b.pr.number,
    ),
    optedOut: optedOut.map(({ ticket, pr }) => ({
      ticket: ticket ? String(ticket.identifier).toUpperCase() : null,
      pr: pr.number,
      reason: 'opted-out',
    })),
  }
}

/**
 * Under an admission `policy`, whether no tracker ticket stands behind PR `p` that ticketlessReason
 * left to a ticket (`not-factory`): it names no ticket, is on no cron fire branch, and no in-review
 * ticket's attachment claims it (`claimed()`, null when unreadable, which keeps the tracker path).
 * Without a policy no such PR reaches post or merge, so the answer is false, as before.
 */
export async function admittedTicketless(p, { claimed, policy }) {
  if (!policy || parseTicketLine(p?.body) !== null || isCronFireBranch(p?.headRefName)) return false
  const claims = await claimed()
  return claims instanceof Set && !claims.has(p.number)
}

/**
 * What a quiet-tick fingerprint adds for PR `pr` under an admission `policy`: its class and the
 * admission labels it carries, so adding an opt-in label breaks a quiet tick but an unrelated label
 * does not. Undefined without a policy, which leaves the fingerprint as before.
 */
export function admissionFacts(pr, policy) {
  return policy ? { class: prClass(pr), labels: admissionLabelHits(pr, policy) } : undefined
}

/**
 * The trust tier judge holds PR view `pr` to under the base tip's admission `policy`
 * (verifyAdmissionPolicy of the base revision's config, never the working tree's, so a head cannot
 * admit itself, skip its review or grant itself merge-on-pass). Null without a policy, which keeps
 * today's rules. Otherwise `{class, admitted, review, merge}`: `review` is what stands behind a
 * missing receipt (`receipt`: the `no-receipt` park; `verify`: verify's own review; `none`: no agent
 * ever reads the PR), and `merge` is who merges a pass (`auto` or `human`). Judge has no attachment
 * linkage, so the class is asked with `linked: false` (see prClass). A PR the policy does not admit
 * keeps today's review rule, except a fork: whatever its policy says, it never runs an extension or
 * reaches a session, and a person merges it. Merge authority never depends on admission, so opting
 * a human or bot PR out (after verify's own review receipted it) cannot turn its merge automatic.
 * @returns {null | {class: 'fork' | 'boss' | 'bot' | 'human', admitted: boolean, review: 'receipt' | 'verify' | 'none', merge: 'auto' | 'human'}}
 */
export function judgeTier(pr, policy) {
  if (!policy) return null
  const cls = prClass(pr, { linked: false })
  const hits = admissionLabelHits(pr, policy)
  const optedOut = hits.includes('optOut')
  if (cls === 'boss') return { class: cls, admitted: !optedOut, review: 'receipt', merge: 'auto' }
  const admitted = !optedOut && (policy[cls]?.admit === true || hits.includes('optIn'))
  if (cls === 'fork') return { class: cls, admitted, review: 'none', merge: 'human' }
  const merge = policy[cls]?.merge === 'auto' && hits.includes('mergeOnPass') ? 'auto' : 'human'
  return { class: cls, admitted, review: admitted ? 'verify' : 'receipt', merge }
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
