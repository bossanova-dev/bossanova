#!/usr/bin/env node

// One read-only snapshot of every boss session on the given daemons (this machine and any SSH
// hosts), classified into the next orchestrating action, with the in-session command that takes
// it. The orchestrate skill acts on its output. It writes nothing but its own grace state.
//
// Every fact comes from the daemon that owns the session — `boss show`, `boss chats` and
// `boss pr state`, which reads through bossd's webhook-invalidated GitHub cache — in one round
// trip per host, so an orchestrating loop does not spend the GitHub quota. Only a draft PR whose
// verify chat waits on it costs one `gh` timeline read. The same round trip reads the host's
// `repair-residual` notes, which a repair records when it stops on a red it found on the base.
//
//   node orchestrate-snapshot.mjs collect [--host <ssh-host>]... [--no-local] [--json]
//        [--boss <local boss>] [--remote-boss <boss on hosts>] [--remote-path <PATH on hosts>]
//        [--root <repo>] [--core <skill name>] [--state <grace state file>]
//   node orchestrate-snapshot.mjs pane --chat <chat-id> [--host <ssh-host>] [--lines 40] [--approve]

import { execFileSync, spawnSync } from 'node:child_process'
import { mkdirSync, readFileSync, realpathSync, statSync, writeFileSync } from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { parseArgs } from 'node:util'

import { isMainModule } from '../main-module.mjs'
import { discoverExtensions, validateResult } from '../skill-extensions.mjs'
import { DEFAULT_STATE_DIR, holdRows, readOutage } from './orchestrate-outage.mjs'
import { paneVerb } from './orchestrate-pane.mjs'
import { printTable } from './orchestrate-table.mjs'
import {
  CRON_FINALIZE_WAIT,
  cronTurnEndedFor,
  isCronRun,
  isDependabotPr,
  isInfraRed,
  isLive,
  questionChats,
  stalledChat,
} from './orchestrate-signals.mjs'

const MINUTE = 60 * 1000
const STALL_AFTER = 45 * MINUTE
const TERMINAL_STATES = new Set(['CLOSED', 'MERGED', 'ARCHIVED'])
// A verify blocked verdict converts the PR to draft seconds after posting this comment.
const VERIFY_BLOCKERS = /^### Verification found \d+ blocker/
const REPAIR_OUTCOMES = /^### Repair blocker outcomes/
const VERIFY_DRAFT_WINDOW = 2 * MINUTE

/**
 * Who holds a draft PR. `verify` when the last draft conversion came straight after a verify
 * blockers comment, `born-draft` when it was never converted, otherwise `operator` (a person, or
 * an automation we cannot attribute — treat as a human hold). `repaired` is true when a repair
 * outcomes comment landed after that conversion.
 */
export function draftHold(timeline = []) {
  const items = [...timeline].sort((a, b) => (a.createdAt < b.createdAt ? -1 : 1))
  let lastDraft = -1
  for (let i = 0; i < items.length; i++)
    if (items[i].__typename === 'ConvertToDraftEvent') lastDraft = i
  if (lastDraft === -1) return { holder: 'born-draft', repaired: false }
  const at = Date.parse(items[lastDraft].createdAt)
  const byVerify = items
    .slice(0, lastDraft)
    .some(
      (item) =>
        item.__typename === 'IssueComment' &&
        VERIFY_BLOCKERS.test(item.body ?? '') &&
        at - Date.parse(item.createdAt) <= VERIFY_DRAFT_WINDOW,
    )
  const repaired = items
    .slice(lastDraft + 1)
    .some((item) => item.__typename === 'IssueComment' && REPAIR_OUTCOMES.test(item.body ?? ''))
  return { holder: byVerify ? 'verify' : 'operator', repaired }
}

// Park codes only the operator settles: those a verify policy raises on purpose, and
// `partial-build`, a PR built partially on purpose (merge the part, or continue) that a fresh head
// would park again. Every other code in verify-gate.mjs HUMAN_REASONS records something mechanical
// (repair-exhausted, worktree-unsynced, no-receipt, ...) that an agent can clear by giving verify a
// fresh head.
function isOperatorPark(code) {
  return (
    code === 'always-human-path' ||
    code === 'human-merge' ||
    code === 'partial-build' ||
    code.startsWith('human-review:')
  )
}

function parkCodes(reason = '') {
  return reason
    .split(',')
    .map((code) => code.trim())
    .filter(Boolean)
}

// A run that stopped because the base branch is red: the fix is upstream of its ticket.
const INHERITED_RED =
  /inherited|reproduc\w* on (base|main)|(fails|failing|red) on (base|main)|pre-?existing (failure|red)/i

const FAILED_CONCLUSIONS = new Set([
  'failure',
  'cancelled',
  'timed_out',
  'action_required',
  'startup_failure',
])

/** The names of the checks failing on a `boss pr state` head. */
export function failingChecks(pr) {
  return (pr?.checks ?? [])
    .filter((check) => FAILED_CONCLUSIONS.has(check.conclusion) && !check.name.startsWith('boss/'))
    .map((check) => check.name)
}

/**
 * Failing checks shared by two or more open PRs of one repo: one upstream cause. The orchestrator
 * elects one session to fix it (the smallest, fastest change, preferably one that can auto-merge)
 * and tells the others to hold, then rebase once the fix merges. A group is `inherited` when a
 * member's repair already recorded the red as one found on the base (`residual`).
 */
export function sharedReds(rows) {
  const groups = new Map()
  for (const row of rows) {
    for (const check of row.failing ?? []) {
      const key = `${row.repo}\u0000${check}`
      if (!groups.has(key))
        groups.set(key, { repo: row.repo, check, inherited: false, infra: false, prs: [] })
      const group = groups.get(key)
      const residual = row.residual?.id ?? ''
      if (residual) group.inherited = true
      if (row.infra) group.infra = true
      group.prs.push({
        pr: row.pr,
        session: row.session,
        host: row.host,
        action: row.action,
        ...(residual ? { residual } : {}),
      })
    }
  }
  return [...groups.values()].filter((group) => group.prs.length >= 2)
}

// The tag a repair puts on the durable note it records when it stops short (boss-repair's
// RESIDUAL_NOTE_TAG); a stop on a red found on the base is one of them.
export const RESIDUAL_TAG = 'repair-residual'
const PR_REFERENCE = /(?:\bPR\s*#|\/pull\/)(\d+)\b/gi

/** The residual notes that name this session: recorded by it, or citing its PR in its repo. */
export function residualsFor(session, prNumber, residuals = []) {
  const cites = (body) =>
    prNumber != null &&
    [...String(body ?? '').matchAll(PR_REFERENCE)].some((match) => Number(match[1]) === prNumber)
  return residuals.filter(
    (note) =>
      note.session_id === session.id ||
      (note.repo_id && note.repo_id === session.repo_id && cites(note.body)),
  )
}

/**
 * The newest residual that records an inherited red on a check failing now (`failing`) and still
 * stands, or null. Residuals also record declined review findings and defects found on the base, so
 * one counts only when it names a failing check. A repair that bossd started after the note was
 * written, and that recorded no newer residual, supersedes it.
 */
export function openResidual(session, residuals = [], failing = []) {
  const since = Date.parse(session.last_repair?.started_at ?? '')
  const checks = failing.map((name) => name.toLowerCase())
  const standing = residuals.filter((note) => {
    const body = String(note.body ?? '')
    const at = Date.parse(note.created_at ?? '')
    if (!INHERITED_RED.test(body)) return false
    if (!checks.some((check) => body.toLowerCase().includes(check))) return false
    return Number.isNaN(since) || (!Number.isNaN(at) && at >= since)
  })
  standing.sort((a, b) => Date.parse(b.created_at) - Date.parse(a.created_at))
  return standing[0] ?? null
}

/** A residual's first non-empty line, as one short line for `why`. */
export function residualSummary(note) {
  const line =
    String(note.body ?? '')
      .split('\n')
      .map((text) => text.trim())
      .find(Boolean) ?? ''
  // gate-region-ok: a display cap on one summary line, not a section window.
  return line.replace(/\s+/g, ' ').slice(0, 160)
}

// bossd's repair workflow sets the session's repair_active flag and runs in a repair-role chat
// that goes idle when the repair ends, while the flag can lag behind. A flag older than this is a
// run that never cleared it, not a repair in progress.
const REPAIR_FRESH = 2 * 60 * MINUTE
// How long a flagged repair may run before its repair-role chat appears.
const REPAIR_STARTUP = 10 * MINUTE

// Repair and verify are launched by the factory itself (auto-repair, verify crons and events), so a
// conflict, red check or missing verify first waits this long for that automation before the
// orchestrator steps in and risks a duplicate run.
export const AUTOMATION_GRACE = 15 * MINUTE

/**
 * Holds a freshly seen `repair` or `verify` row as `grace` until it has stood for AUTOMATION_GRACE.
 * `seen` maps a row key to when its action was first seen; it is updated in place and keys that are
 * gone this round are dropped, except those of `unreadHosts`, whose rows this round could not see.
 * A repo with auto-repair off gets no repair grace: nothing will come.
 */
export function applyGrace(rows, seen, now, unreadHosts = []) {
  const current = new Set()
  for (const row of rows) {
    const waits = row.action === 'verify' || (row.action === 'repair' && row.autoRepair !== false)
    if (!waits) continue
    const key = `${row.host}/${row.session}/${row.action}`
    current.add(key)
    const first = seen[key] ?? now
    seen[key] = first
    if (now - first < AUTOMATION_GRACE) {
      const minutes = Math.ceil((AUTOMATION_GRACE - (now - first)) / MINUTE)
      row.why = `${row.why}; waiting ${minutes}m more for the factory's own ${row.action}`
      row.action = 'grace'
      row.next = ''
    }
  }
  const unseen = (key) => unreadHosts.some((host) => key.startsWith(`${host}/`))
  for (const key of Object.keys(seen)) if (!current.has(key) && !unseen(key)) delete seen[key]
  return rows
}

/**
 * When bossd's repair workflow is running on the session now, the time it started; else ''. The
 * flag alone is not enough: once a repair-role chat created at or after the start exists, the
 * newest one decides, and an idle one means the repair finished. Before that chat appears the
 * flag counts only for REPAIR_STARTUP.
 */
export function daemonRepairSince(session, chats, now) {
  if (session.repair_active !== true) return ''
  const started = session.last_repair?.started_at ?? ''
  const at = Date.parse(started)
  if (Number.isNaN(at) || now - at > REPAIR_FRESH) return ''
  const created = (chat) => Date.parse(chat.created_at ?? '')
  const repairChats = chats
    .filter((chat) => chat.role === 'repair' && created(chat) >= at)
    .sort((a, b) => created(b) - created(a))
  if (!repairChats.length) return now - at <= REPAIR_STARTUP ? started : ''
  return isLive(repairChats[0]) ? started : ''
}

// The verdicts a stuck chat replaces: the ones that would otherwise send nothing, or only read
// the pane. A PR already handed off (ready, with a build receipt) is the factory's to verify, so
// its idle chats are finished, not stuck.
const NUDGEABLE = new Set(['ok', 'stalled', 'wait-ci', 'waiting'])

function waitingOnReady(chat) {
  return /checks_passed_ready/.test(chat.waiting_reason ?? '')
}

function idleFor(session, now) {
  const last = Date.parse(session.last_agent_activity_at || session.updated_at || '')
  return Number.isNaN(last) ? Infinity : now - last
}

function verifyPassed(pr) {
  return (pr.checks ?? []).some(
    (check) => check.name === 'boss/verify' && check.conclusion === 'success',
  )
}

/**
 * The next orchestrating action for one session. `pr` is `boss pr state --json` for the session's
 * PR (null when it has none), plus `draftHold` when a timeline was read. `residual` is the
 * session's open inherited-red residual note (`openResidual`), or null. `cronJob` is the cron job
 * whose latest run the session is (`boss cron ls`), or null.
 */
export function classifySession({
  session,
  chats = [],
  pr = null,
  residual = null,
  cronJob = null,
  now = Date.now(),
}) {
  if (TERMINAL_STATES.has(session.state) || session.archived_at)
    return { action: 'skip', why: 'terminal' }
  if (pr && pr.state !== 'open')
    return { action: 'stale-session', why: `PR is ${pr.state || 'unreadable'}` }
  const asking = questionChats(chats)
  if (asking.length) {
    const more = asking.length > 1 ? ` (+${asking.length - 1} more)` : ''
    return {
      action: 'question-chat',
      why: `chat ${asking[0].agent_session_id} shows a dialog${more}`,
      chat: asking[0].agent_session_id,
    }
  }
  const progress = classifyProgress({ session, chats, pr, residual, cronJob, now })
  const handedOff = Boolean(pr && !pr.is_draft && pr.has_build_receipt)
  const stuck = NUDGEABLE.has(progress.action) && !handedOff ? stalledChat(chats, now) : null
  if (!stuck) return progress
  return {
    action: 'stalled-chat',
    why: `newest chat idle ${stuck.idleMinutes}m with nothing armed to wake it (${progress.why})`,
    chat: stuck.chat.agent_session_id,
  }
}

function classifyProgress({ session, chats, pr, residual, cronJob, now }) {
  const verdict = (action, why) => ({ action, why })
  const live = chats.filter(isLive)
  const working = (why) => verdict('working', `${why}; ${live.length} live chat(s)`)
  const repairing = daemonRepairSince(session, chats, now)
  const inherited = () =>
    verdict('upstream-red', `repair residual ${residual.id}: ${residualSummary(residual)}`)
  if (repairing) return verdict('working', `bossd repair running since ${repairing}`)
  const phase = pr?.verify_phase ?? { kind: '', reason: '', label: '' }
  // A run can block after its PR was already handed off (a later finalize pass tripping on
  // incidental churn, say). refresh-pr releases such a block, but an older daemon or an
  // unrefreshed session stays BLOCKED, so the PR decides.
  const handedOff = Boolean(
    pr && !pr.is_draft && pr.has_build_receipt && (phase.kind || verifyPassed(pr)),
  )
  if (session.state === 'BLOCKED' && !handedOff) {
    if (live.length) return working('blocked')
    const reason = session.blocked_reason || 'no reason recorded'
    if (isInfraRed(reason)) return { action: 'waive-cap', why: reason, cause: 'infra' }
    if (INHERITED_RED.test(reason)) return verdict('upstream-red', reason)
    if (residual) return inherited()
    const cap = /\bcap\b|re-review|retries exhausted|budget/i.test(reason)
    return verdict(cap ? 'waive-cap' : 'unblock', reason)
  }
  if (!pr) {
    if (live.length) return working('no PR yet')
    const ended = isCronRun(session, cronJob) ? cronTurnEndedFor(chats, now) : null
    if (ended !== null && ended < CRON_FINALIZE_WAIT)
      return verdict(
        'finalizing',
        `cron run ended its turn ${Math.floor(ended / MINUTE)}m ago with no PR; bossd finalizes it`,
      )
    return idleFor(session, now) > STALL_AFTER
      ? verdict('stalled', 'no PR, no live chat, no recent activity')
      : verdict('ok', 'recently active')
  }
  const checks = pr.check_verdict ?? {}
  if (pr.merge_state_status === 'dirty' || pr.is_mergeable === false) {
    return live.length ? working('merge conflict') : verdict('repair', 'merge conflict')
  }
  if (checks.state === 'failing') {
    const why = `${checks.failed_count ?? '?'} failing check(s)`
    if (live.length) return working(why)
    return residual ? inherited() : verdict('repair', why)
  }
  if (pr.is_draft) {
    if (live.length) return working('draft')
    if (!chats.some(waitingOnReady))
      return verdict('stalled', 'draft PR with no live chat and no armed verify')
    const hold = pr.draftHold ?? { holder: 'born-draft', repaired: false }
    if (hold.holder === 'operator')
      return verdict('ask-operator', 'draft held by an unattributed conversion')
    if (hold.holder === 'verify' && !hold.repaired)
      return verdict('repair', 'verify blockers not yet repaired')
    return verdict(
      'mark-ready',
      `draft (${hold.holder}) deadlocks the checks_passed_ready verify callback`,
    )
  }
  if (phase.kind === 'needs_human') {
    const mechanical = parkCodes(phase.reason).filter((code) => !isOperatorPark(code))
    const label = phase.label ? ` · ${phase.label}` : ''
    if (!mechanical.length) return verdict('awaiting-operator', `${phase.reason}${label}`)
    if (live.length) return working(`parked ${mechanical.join(',')}`)
    if (checks.state === 'pending')
      return verdict('wait-ci', `parked ${mechanical.join(',')}; checks pending`)
    return verdict(
      'refresh-park',
      `parked ${mechanical.join(',')}${label}; checks ${checks.state || 'unknown'}`,
    )
  }
  if (verifyPassed(pr) && phase.kind !== 'verifying' && phase.kind !== 'waiting') {
    return verdict('done', 'verified, ready for review')
  }
  if (live.length) return working(phase.kind ? `verify ${phase.kind}` : 'in flight')
  if (checks.state === 'pending')
    return verdict('wait-ci', `${checks.pending_count ?? '?'} check(s) pending`)
  if (isDependabotPr(pr, session))
    return verdict(
      'dependabot',
      'dependabot PR: its sweep and auto-merge own it, never boss/verify',
    )
  if (phase.kind === 'verifying' || phase.kind === 'waiting') {
    return idleFor(session, now) > STALL_AFTER
      ? verdict(
          'verify',
          `verify ${phase.kind}${phase.reason ? ` (${phase.reason})` : ''} with no live chat`,
        )
      : verdict('waiting', `verify ${phase.kind}${phase.reason ? ` (${phase.reason})` : ''}`)
  }
  return verdict('verify', 'ready PR with no boss/verify status')
}

/**
 * The in-session command that takes `action`, or '' when there is nothing to send. Orchestration
 * never creates a session: it opens a chat inside the session that owns the PR. `chat` names the
 * chat a `question-chat` or `stalled-chat` row is about; `cause: 'infra'` marks an infrastructure
 * red behind a `waive-cap`.
 * @param {{ action: string, why: string, chat?: string, cause?: string }} verdict
 * @param {{ host: string, session: any, pr?: number }} context
 */
export function remedy({ action, why, chat: chatId, cause }, { host, session, pr }) {
  const boss = host ? `boss --host ${host}` : 'boss'
  const skill = (name) => (session.agent === 'codex' ? `$${name}` : `/${name}`)
  const quote = (text) => `'${String(text).replace(/'/g, `'\\''`)}'`
  const send = (id, message) => `${boss} chat send --json ${id || '<chat-id>'} ${quote(message)}`
  const chat = (role, message) =>
    `${boss} chat new ${session.id} --role ${role} --title '${role}: orchestrator' --json  # then: ${send('', message)}`
  const pane = (flag) =>
    `node ${SNAPSHOT_SCRIPT} pane --chat ${chatId || '<chat-id>'}${host ? ` --host ${host}` : ''}${flag}`
  switch (action) {
    case 'question-chat':
      return `${pane('')}  # permission prompt: ${pane(' --approve')}  # no dialog left: ${send(chatId, STALE_DIALOG)}`
    case 'stalled-chat':
      return send(chatId, NUDGE)
    case 'waive-cap':
      if (cause === 'infra')
        return chat(
          'repair',
          `Operator: this red is infrastructure, not your change (${why}), so the cap is waived. Rerun the failed jobs (gh run rerun <run-id> --failed), and once CI is green finish the boss-build hand-off.`,
        )
      return chat(
        'repair',
        `Operator waives the cap (${why}). Run ${skill('boss-repair')}, a fresh review, then the boss-build hand-off.`,
      )
    case 'repair':
      return chat('repair', `${skill('boss-repair')} ${pr ?? ''} (${why}).`)
    case 'upstream-red':
      return chat(
        'repair',
        `Operator: do not stop on the inherited red (${why}). If another session already owns the fix, hold, then rebase onto the base branch once it merges. Otherwise fix the cause on the base yourself as its own commit (or a small PR against the base branch), get CI green, then finish the boss-build hand-off.`,
      )
    case 'refresh-park':
      return chat(
        'repair',
        `Verify's park (${why}) is stale. Run ${skill('boss-repair')}: rebase onto main, gates, lease-push a fresh head, re-arm verify. Keep it ready.`,
      )
    case 'verify':
      return chat('verify', `${skill('boss-verify')} ${pr ?? ''}`)
    case 'mark-ready':
      return `gh pr ready ${pr} -R <repo>  # the armed verify callback then fires`
    default:
      return ''
  }
}

const SNAPSHOT_SCRIPT = fileURLToPath(import.meta.url)
const NUDGE =
  'Operator: this session is still in progress, but you went idle with nothing armed to wake you. Re-check your state (branch, PR, checks, review) and continue toward the hand-off now. Never wait on a monitor or a background sleep.'
const STALE_DIALOG =
  'Operator: the dialog you showed was answered or dismissed. Re-check your state and continue toward the hand-off.'

// Runs on the daemon's machine (over SSH for a remote host) so one round trip lists everything.
// argv: boss. Prints { sessions, residuals, errors }; `residuals` are the host's repair-residual
// notes across every repo (`--repo ""`, since the notes list defaults to the current repo). A
// session that is a cron job's latest run carries that job as `cron`.
const DAEMON_DUMP = `
const { execFile } = require('node:child_process')
const boss = process.argv[2] || 'boss'
const run = (args) => new Promise((resolve, reject) =>
  execFile(boss, args, { encoding: 'utf8', maxBuffer: 64 << 20, timeout: 90000 }, (err, out) =>
    err ? reject(err) : resolve(JSON.parse(out))))
const settle = (p) => p.then((value) => ({ status: 'fulfilled', value }), (reason) => ({ status: 'rejected', reason }))
const TERMINAL = new Set(['CLOSED', 'MERGED', 'ARCHIVED'])
// The cron job whose latest run each session is, from one cron list.
const cronBySession = new Map()
async function one(s) {
  const entry = { session: s, chats: [], pr: null }
  if (TERMINAL.has(s.state)) return entry
  const cron = cronBySession.get(s.id)
  if (cron) entry.cron = cron
  const ref = /github\\.com\\/([^/]+\\/[^/]+)\\/pull\\/(\\d+)/.exec(s.pr_url || '')
  // Sequential per session and few workers: a burst of parallel calls makes bossd drop connections.
  const pr = await settle(ref ? run(['pr', 'state', ref[2], '--repo', ref[1], '--json']) : Promise.resolve(null))
  if (pr.status === 'fulfilled') entry.pr = pr.value
  else entry.errors = [...(entry.errors ?? []), 'pr state: ' + String(pr.reason.message).slice(0, 160)]
  if (ref) entry.repo = ref[1]
  const chats = await settle(run(['chats', '--json', s.id]))
  const shown = await settle(run(['show', '--json', s.id]))
  if (chats.status === 'fulfilled') entry.chats = chats.value.chats ?? []
  else entry.errors = [...(entry.errors ?? []), 'chats: ' + String(chats.reason.message).slice(0, 160)]
  if (shown.status === 'fulfilled') entry.session = shown.value.session ?? s
  return entry
}
;(async () => {
  const errors = []
  const crons = await settle(run(['cron', 'ls', '--json']))
  if (crons.status === 'fulfilled') {
    const jobs = Array.isArray(crons.value) ? crons.value : (crons.value.jobs ?? [])
    for (const job of jobs) {
      if (job.last_run_session_id) cronBySession.set(job.last_run_session_id, { id: job.id, name: job.name })
    }
  } else errors.push('cron jobs: ' + String(crons.reason.message).slice(0, 160))
  const listed = await run(['ls', '--json'])
  const queue = [...(listed.sessions ?? listed)]
  const out = []
  await Promise.all(Array.from({ length: 4 }, async () => {
    while (queue.length) out.push(await one(queue.shift()))
  }))
  const notes = await settle(run(['notes', 'ls', '--tag', '${RESIDUAL_TAG}', '--repo', '', '--json']))
  let residuals = []
  if (notes.status === 'fulfilled') {
    const list = Array.isArray(notes.value) ? notes.value : (notes.value.notes ?? [])
    residuals = list.map((n) => ({ id: n.id, repo_id: n.repo_id, session_id: n.session_id,
      created_at: n.created_at, body: String(n.body ?? '').slice(0, 4000) }))
  } else errors.push('residual notes: ' + String(notes.reason.message).slice(0, 160))
  process.stdout.write(JSON.stringify({ sessions: out, residuals, errors }))
})().catch((e) => { process.stderr.write(String(e.message)); process.exit(1) })
`

// A non-interactive SSH command gets a minimal PATH, and the host's login shell may not be POSIX,
// so remote commands run through `env` with a PATH covering the usual install locations.
export const DEFAULT_REMOTE_PATH = '/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin'

/**
 * @param {string} host
 * @param {{ bossBin: string, remoteBoss: string, remotePath: string }} where
 */
function dumpDaemon(host, { bossBin, remoteBoss, remotePath }) {
  /** @type {import('node:child_process').ExecFileSyncOptionsWithStringEncoding} */
  const opts = { input: DAEMON_DUMP, encoding: 'utf8', maxBuffer: 64 << 20 }
  const raw = host
    ? execFileSync(
        'ssh',
        ['-o', 'BatchMode=yes', host, 'env', `PATH=${remotePath}`, 'node', '-', remoteBoss],
        opts,
      )
    : execFileSync('node', ['-', bossBin], opts)
  return JSON.parse(raw)
}

const TIMELINE_QUERY = `query($o:String!,$r:String!,$n:Int!){repository(owner:$o,name:$r){pullRequest(number:$n){
timelineItems(last:60,itemTypes:[CONVERT_TO_DRAFT_EVENT,READY_FOR_REVIEW_EVENT,ISSUE_COMMENT]){nodes{__typename
... on ConvertToDraftEvent{createdAt} ... on ReadyForReviewEvent{createdAt} ... on IssueComment{createdAt body}}}}}}`

function timeline(repo, number) {
  const [o, r] = repo.split('/')
  const res = JSON.parse(
    execFileSync(
      'gh',
      [
        'api',
        'graphql',
        '-f',
        `query=${TIMELINE_QUERY}`,
        '-f',
        `o=${o}`,
        '-f',
        `r=${r}`,
        '-F',
        `n=${number}`,
      ],
      {
        encoding: 'utf8',
        maxBuffer: 16 << 20,
      },
    ),
  )
  return res.data.repository.pullRequest.timelineItems.nodes.map((node) => ({
    ...node,
    body: (node.body ?? '').slice(0, 200),
  }))
}

// The skill whose repo-local extensions the snapshot discovers (`<core>-<suffix>` directories).
export const ORCHESTRATE_CORE = 'bs-orchestrate'
const EXTENSION_ROLE_NAMES = ['orchestrate-rules', 'orchestrate-classify', 'orchestrate-round']
export const EXTENSION_SCRIPT_TIMEOUT_MS = 60 * 1000
const MAX_EXTENSION_OUTPUT_BYTES = 1 << 20

/**
 * The orchestrate extensions under `root`, by role, plus a ledger line for every misconfigured
 * one. Discovery never decides the round: a broken extension is reported and left out.
 */
export function discoverOrchestrateExtensions({ root, core = ORCHESTRATE_CORE }) {
  const found = { rules: [], classify: [], round: [] }
  const ledger = []
  for (const role of EXTENSION_ROLE_NAMES) {
    let discovered
    try {
      discovered = discoverExtensions({ core, root, role })
    } catch (error) {
      ledger.push(`extensions: ${role} discovery failed (${String(error.message).split('\n')[0]})`)
      continue
    }
    for (const skip of discovered.skipped)
      if (!skip.deliberate) ledger.push(`extension ${skip.name}: skipped (${skip.reason})`)
    found[role.slice('orchestrate-'.length)] = discovered.extensions
  }
  return { ...found, ledger }
}

function extensionScript(extension) {
  try {
    const script = realpathSync(path.resolve(extension.dir, extension.script))
    const relative = path.relative(realpathSync(extension.dir), script)
    if (relative.startsWith('..') || path.isAbsolute(relative) || !statSync(script).isFile())
      return null
    return script
  } catch {
    return null
  }
}

/**
 * Runs one script-role extension with `input` as JSON on stdin, from `cwd`. Returns the validated
 * result, or the reason it was skipped: `script-missing`, `timeout`, `oversize`, `signal`,
 * `exit:<n>`, `not-json` or `invalid: <first error>`. Never throws.
 */
export function runExtensionScript(
  extension,
  input,
  { cwd, timeoutMs = EXTENSION_SCRIPT_TIMEOUT_MS },
) {
  const script = extensionScript(extension)
  if (!script) return { reason: 'script-missing' }
  const run = spawnSync(process.execPath, [script], {
    cwd,
    input: JSON.stringify(input),
    encoding: 'utf8',
    timeout: timeoutMs,
    killSignal: 'SIGKILL',
    maxBuffer: MAX_EXTENSION_OUTPUT_BYTES,
  })
  const code = /** @type {NodeJS.ErrnoException | undefined} */ (run.error)?.code
  if (code === 'ETIMEDOUT') return { reason: 'timeout' }
  if (code === 'ENOBUFS') return { reason: 'oversize' }
  if (run.error) return { reason: `spawn: ${run.error.message}` }
  if (run.signal) return { reason: 'signal' }
  if (run.status !== 0) return { reason: `exit:${run.status}` }
  let result
  try {
    result = JSON.parse(run.stdout)
  } catch {
    return { reason: 'not-json' }
  }
  const checked = validateResult(result, extension.role)
  if (!checked.ok) return { reason: `invalid: ${checked.errors[0]}` }
  return { result }
}

const rowKey = (host, session) => `${host}\u0000${session}`

/**
 * Applies every orchestrate-classify extension, in discovery order, to the round's rows. Each
 * script sees the rows as earlier extensions left them. An override replaces a row's `action` and
 * `why`, and its `next` when given (otherwise `nextFor` recomputes it for the new action), and
 * records the extension in `classifiedBy`.
 */
export function applyClassifiers({ extensions, rows, core, cwd, nextFor, ledger, timeoutMs }) {
  const byKey = new Map(rows.map((row) => [rowKey(row.host, row.session), row]))
  for (const extension of extensions) {
    const input = { role: 'orchestrate-classify', core, extension: extension.name, rows }
    const { result, reason } = runExtensionScript(extension, input, { cwd, timeoutMs })
    if (!result) {
      ledger.push(`extension ${extension.name}: skipped (${reason})`)
      continue
    }
    for (const override of result.overrides) {
      const row = byKey.get(rowKey(override.host, override.session))
      if (!row) {
        ledger.push(
          `extension ${extension.name}: override for ${override.host} ${override.session} matches no row`,
        )
        continue
      }
      row.action = override.action
      row.why = override.why
      row.next = override.next ?? nextFor(row)
      row.classifiedBy = extension.name
    }
  }
}

/**
 * Runs every orchestrate-round extension once and returns its items for the operator list. `ran`
 * collects the names of the extensions that returned a valid result.
 */
export function runRoundChecks({
  extensions,
  rows,
  sharedReds,
  core,
  cwd,
  ledger,
  timeoutMs,
  ran = [],
}) {
  const items = []
  for (const extension of extensions) {
    const input = { role: 'orchestrate-round', core, extension: extension.name, rows, sharedReds }
    const { result, reason } = runExtensionScript(extension, input, { cwd, timeoutMs })
    if (!result) {
      ledger.push(`extension ${extension.name}: skipped (${reason})`)
      continue
    }
    ran.push(extension.name)
    for (const item of result.items)
      items.push({ extension: extension.name, title: item.title, detail: item.detail })
  }
  return items
}

export function collect({
  hosts,
  local,
  bossBin,
  remoteBoss = 'boss',
  remotePath = DEFAULT_REMOTE_PATH,
  now = Date.now(),
  root = process.cwd(),
  core = ORCHESTRATE_CORE,
  dump = dumpDaemon,
  timeoutMs = EXTENSION_SCRIPT_TIMEOUT_MS,
  seen = null,
  // Where the outage record lives; null reads none.
  stateDir = null,
}) {
  const where = { bossBin, remoteBoss, remotePath }
  const targets = [
    ...(local ? [{ host: '', label: 'local' }] : []),
    ...hosts.map((host) => ({ host, label: host })),
  ]
  const rows = []
  const errors = []
  // Hosts whose daemon could not be read this round: nothing about their sessions is known.
  const unread = []
  // What `remedy` needs to rebuild a row's `next` after an extension changes its action.
  const remedyContext = new Map()
  for (const target of targets) {
    let read
    try {
      read = dump(target.host, where)
    } catch (error) {
      errors.push(`${target.label}: ${String(error.message).split('\n')[0]}`)
      unread.push(target.label)
      continue
    }
    const { sessions: entries, residuals = [] } = read
    for (const error of read.errors ?? []) errors.push(`${target.label}: ${error}`)
    for (const entry of entries) {
      const { session, chats } = entry
      const pr = entry.pr
      for (const error of entry.errors ?? []) errors.push(`${target.label} ${session.id}: ${error}`)
      // The draft holder is the one fact the daemon cache does not carry; read it only when it decides.
      if (
        pr?.is_draft &&
        pr.state === 'open' &&
        !chats.some(isLive) &&
        chats.some(waitingOnReady) &&
        entry.repo
      ) {
        try {
          pr.draftHold = draftHold(timeline(entry.repo, pr.pr_number))
        } catch (error) {
          errors.push(
            `timeline ${entry.repo}#${pr.pr_number}: ${String(error.message).split('\n')[0]}`,
          )
        }
      }
      const terminal = TERMINAL_STATES.has(session.state) || Boolean(session.archived_at)
      const residual = terminal
        ? null
        : openResidual(session, residualsFor(session, pr?.pr_number, residuals), failingChecks(pr))
      const verdict = classifySession({ session, chats, pr, residual, cronJob: entry.cron, now })
      rows.push({
        host: target.label,
        session: session.id,
        agent: session.agent,
        title: session.title,
        state: session.state,
        display: session.display_status ?? '',
        repo: entry.repo ?? session.repo_display_name ?? session.repo_id,
        pr: pr?.pr_number ?? null,
        chats: chats.map((chat) => ({
          id: chat.agent_session_id,
          title: chat.title,
          role: chat.role,
          status: chat.status,
          waiting: chat.waiting_reason || '',
          live: isLive(chat),
          lastOutputAt: chat.last_substantive_output_at || chat.last_output_at || '',
        })),
        failing: failingChecks(pr),
        // Evidence that the red is infrastructure, from what the round already read.
        infra: isInfraRed(
          [session.blocked_reason, residual?.body]
            .concat((pr?.checks ?? []).filter((c) => FAILED_CONCLUSIONS.has(c.conclusion)))
            .map((item) => (typeof item === 'object' && item ? item.description : item) ?? '')
            .join('\n'),
        ),
        autoRepair: session.repo_can_auto_repair !== false,
        ...(residual ? { residual: { id: residual.id, summary: residualSummary(residual) } } : {}),
        // A fact failed to read, so the row's action may be wrong.
        ...((entry.errors ?? []).length ? { unread: true } : {}),
        ...verdict,
        next: remedy(verdict, { host: target.host, session, pr: pr?.pr_number }),
      })
      remedyContext.set(rowKey(target.label, session.id), {
        host: target.host,
        session,
        pr: pr?.pr_number,
      })
    }
  }
  // The grace spans rounds, so it applies only when the caller carries `seen` between them.
  if (seen) applyGrace(rows, seen, now, unread)
  // An open outage record holds its sessions until it resumes.
  const outage = stateDir ? readOutage(stateDir) : null
  holdRows(rows, outage)
  const extensions = discoverOrchestrateExtensions({ root, core })
  const ledger = [...extensions.ledger]
  const nextFor = (row) => remedy(row, remedyContext.get(rowKey(row.host, row.session)))
  applyClassifiers({
    extensions: extensions.classify,
    rows,
    core,
    cwd: root,
    nextFor,
    ledger,
    timeoutMs,
  })
  const shared = sharedReds(rows)
  const roundExtensions = []
  const operatorItems = runRoundChecks({
    extensions: extensions.round,
    rows,
    sharedReds: shared,
    core,
    cwd: root,
    ledger,
    timeoutMs,
    ran: roundExtensions,
  })
  return {
    rows,
    errors,
    unread,
    sharedReds: shared,
    // The orchestrator reads each rules extension's SKILL.md and applies it every round.
    rules: extensions.rules.map(({ name, skillPath }) => ({ name, skillPath })),
    operatorItems,
    // The round extensions that ran: an item one of them stopped reporting has cleared.
    roundExtensions,
    ledger,
    seen: seen ?? {},
    outage,
  }
}

export const DEFAULT_STATE = path.join(DEFAULT_STATE_DIR, 'seen.json')

function readSeen(file) {
  try {
    const parsed = JSON.parse(readFileSync(file, 'utf8'))
    return parsed && typeof parsed === 'object' && !Array.isArray(parsed) ? parsed : {}
  } catch {
    return {}
  }
}

function main(argv) {
  const { values, positionals } = parseArgs({
    args: argv,
    allowPositionals: true,
    strict: true,
    options: {
      host: { type: 'string', multiple: true, default: [] },
      'no-local': { type: 'boolean', default: false },
      json: { type: 'boolean', default: false },
      chat: { type: 'string' },
      lines: { type: 'string', default: '40' },
      approve: { type: 'boolean', default: false },
      boss: { type: 'string', default: process.env.BOSS_BIN || 'boss' },
      'remote-boss': { type: 'string', default: 'boss' },
      'remote-path': { type: 'string', default: DEFAULT_REMOTE_PATH },
      root: { type: 'string', default: process.cwd() },
      core: { type: 'string', default: ORCHESTRATE_CORE },
      // Remembers when each repair/verify row was first seen, so the grace spans the attended
      // chat's repeated collect rounds.
      state: { type: 'string', default: DEFAULT_STATE },
    },
  })
  const [command] = positionals
  const options = {
    hosts: values.host,
    local: !values['no-local'],
    bossBin: values.boss,
    remoteBoss: values['remote-boss'],
    remotePath: values['remote-path'],
    root: values.root,
    core: values.core,
    seen: readSeen(values.state),
    stateDir: path.dirname(values.state),
  }
  const saveSeen = (result) => {
    try {
      mkdirSync(path.dirname(values.state), { recursive: true })
      writeFileSync(values.state, `${JSON.stringify(result.seen)}\n`)
    } catch (error) {
      result.errors.push(`state ${values.state}: ${String(error.message).split('\n')[0]}`)
    }
  }
  if (command === 'collect') {
    const result = collect(options)
    saveSeen(result)
    if (values.json) process.stdout.write(`${JSON.stringify(result, null, 2)}\n`)
    else printTable(result)
    return result.errors.length ? 2 : 0
  }
  if (command === 'pane') {
    if (!values.chat) throw new Error('pane needs --chat <chat-id>')
    return paneVerb({
      host: values.host[0] ?? '',
      chat: values.chat,
      lines: Number(values.lines),
      remotePath: values['remote-path'],
      approve: values.approve,
    })
  }
  process.stderr.write(
    'usage: orchestrate-snapshot.mjs collect|pane [--host <ssh-host>]... [--no-local] [--json]\n',
  )
  return 64
}

/** Runs one verb and returns its exit code. */
export function run(argv) {
  try {
    return main(argv)
  } catch (error) {
    process.stderr.write(`orchestrate-snapshot: ${String(error.message).split('\n')[0]}\n`)
    return 1
  }
}

if (isMainModule(import.meta.url)) process.exitCode = run(process.argv.slice(2))
