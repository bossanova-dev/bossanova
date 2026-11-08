#!/usr/bin/env node

// One read-only snapshot of every boss session on the given daemons (this machine and any SSH
// hosts), classified into the next orchestrating action, with the in-session command that takes
// it. The orchestrate skill acts on its output. It writes nothing but its own grace state.
// `gate` is the cron gate for an unattended worker round (exit 0 work, 1 none, 69 unknown).
//
// Every fact comes from the daemon that owns the session — `boss show`, `boss chats` and
// `boss pr state`, which reads through bossd's webhook-invalidated GitHub cache — in one round
// trip per host, so an orchestrating loop does not spend the GitHub quota. Only a draft PR whose
// verify chat waits on it costs one `gh` timeline read.
//
//   node orchestrate-snapshot.mjs collect [--host <ssh-host>]... [--no-local] [--unattended] [--json]
//        [--respect-owners]
//        [--boss <local boss>] [--remote-boss <boss on hosts>] [--remote-path <PATH on hosts>]
//        [--root <repo>] [--core <skill name>] [--state <grace state file>]
//   node orchestrate-snapshot.mjs gate [the collect options] [--tag <queue tag>] [--repo <repo-id>]
//        [--queue-host <ssh-host>]
//   node orchestrate-snapshot.mjs pane --chat <chat-id> [--host <ssh-host>] [--lines 40]

import { execFileSync, spawnSync } from 'node:child_process'
import { mkdirSync, readFileSync, realpathSync, statSync, writeFileSync } from 'node:fs'
import { homedir } from 'node:os'
import path from 'node:path'
import { parseArgs } from 'node:util'

import { isMainModule } from '../main-module.mjs'
import { discoverExtensions, validateResult } from '../skill-extensions.mjs'
import { claimTargets, readCrons } from './orchestrate-owners.mjs'
import { QUEUE_TAG, bossRunner, gateVerdict, notesClient } from './orchestrate-queue.mjs'

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
 * and tells the others to hold, then rebase once the fix merges.
 */
export function sharedReds(rows) {
  const groups = new Map()
  for (const row of rows) {
    for (const check of row.failing ?? []) {
      const key = `${row.repo}\u0000${check}`
      if (!groups.has(key)) groups.set(key, { repo: row.repo, check, prs: [] })
      groups
        .get(key)
        .prs.push({ pr: row.pr, session: row.session, host: row.host, action: row.action })
    }
  }
  return [...groups.values()].filter((group) => group.prs.length >= 2)
}

function isLive(chat) {
  return chat.spinner_present === true || chat.status === 'WORKING'
}

// bossd's own repair workflow runs outside any chat pane, so only the session's repair_active
// flag shows it. A flag older than this is a run that never cleared it, not a repair in progress.
const REPAIR_FRESH = 2 * 60 * MINUTE

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

/** When bossd's repair workflow is running on the session now, the time it started; else ''. */
export function daemonRepairSince(session, now) {
  if (session.repair_active !== true) return ''
  const started = session.last_repair?.started_at ?? ''
  const at = Date.parse(started)
  return !Number.isNaN(at) && now - at <= REPAIR_FRESH ? started : ''
}

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
 * PR (null when it has none), plus `draftHold` when a timeline was read.
 */
export function classifySession({ session, chats = [], pr = null, now = Date.now() }) {
  const verdict = (action, why) => ({ action, why })
  if (TERMINAL_STATES.has(session.state) || session.archived_at) return verdict('skip', 'terminal')
  const live = chats.filter(isLive)
  const working = (why) => verdict('working', `${why}; ${live.length} live chat(s)`)
  if (pr && pr.state !== 'open')
    return verdict('stale-session', `PR is ${pr.state || 'unreadable'}`)
  const repairing = daemonRepairSince(session, now)
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
    if (INHERITED_RED.test(reason)) return verdict('upstream-red', reason)
    const cap = /\bcap\b|re-review|retries exhausted|budget/i.test(reason)
    return verdict(cap ? 'waive-cap' : 'unblock', reason)
  }
  if (!pr) {
    if (live.length) return working('no PR yet')
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
    return live.length ? working(why) : verdict('repair', why)
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
 * never creates a session: it opens a chat inside the session that owns the PR.
 */
export function remedy({ action, why }, { host, session, pr }) {
  const boss = host ? `boss --host ${host}` : 'boss'
  const skill = (name) => (session.agent === 'codex' ? `$${name}` : `/${name}`)
  const chat = (role, message) =>
    `${boss} chat new ${session.id} --role ${role} --title '${role}: orchestrator' --json  # then: ${boss} chat send <chat-id> '${message}'`
  switch (action) {
    case 'waive-cap':
      return chat(
        'repair',
        `Operator waives the cap (${why}). Run ${skill('boss-repair')}, a fresh review, then the boss-build hand-off.`,
      )
    case 'repair':
      return chat('repair', `${skill('boss-repair')} ${pr ?? ''} (${why}).`)
    case 'upstream-red':
      return chat(
        'repair',
        `Operator: do not stop on the inherited red (${why}). Fix the upstream cause as its own commit (or a small PR against main), get CI green, then finish the boss-build hand-off.`,
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

// Runs on the daemon's machine (over SSH for a remote host) so one round trip lists everything.
// argv: boss, then the mode: `full` (every fact), `pr` (the session list and PR state only), or
// `detail` followed by the session ids to read in full.
const DAEMON_DUMP = `
const { execFile } = require('node:child_process')
const boss = process.argv[2] || 'boss'
const mode = process.argv[3] || 'full'
const only = new Set(process.argv.slice(4))
const run = (args) => new Promise((resolve, reject) =>
  execFile(boss, args, { encoding: 'utf8', maxBuffer: 64 << 20, timeout: 90000 }, (err, out) =>
    err ? reject(err) : resolve(JSON.parse(out))))
const TERMINAL = new Set(['CLOSED', 'MERGED', 'ARCHIVED'])
async function one(s) {
  const entry = { session: s, chats: [], pr: null }
  if (TERMINAL.has(s.state)) return entry
  const ref = /github\\.com\\/([^/]+\\/[^/]+)\\/pull\\/(\\d+)/.exec(s.pr_url || '')
  // Sequential per session and few workers: a burst of parallel calls makes bossd drop connections.
  const settle = (p) => p.then((value) => ({ status: 'fulfilled', value }), (reason) => ({ status: 'rejected', reason }))
  const pr = await settle(ref ? run(['pr', 'state', ref[2], '--repo', ref[1], '--json']) : Promise.resolve(null))
  if (pr.status === 'fulfilled') entry.pr = pr.value
  else entry.errors = [...(entry.errors ?? []), 'pr state: ' + String(pr.reason.message).slice(0, 160)]
  if (ref) entry.repo = ref[1]
  if (mode === 'pr') {
    entry.prOnly = true
    return entry
  }
  const chats = await settle(run(['chats', '--json', s.id]))
  const shown = await settle(run(['show', '--json', s.id]))
  if (chats.status === 'fulfilled') entry.chats = chats.value.chats ?? []
  else entry.errors = [...(entry.errors ?? []), 'chats: ' + String(chats.reason.message).slice(0, 160)]
  if (shown.status === 'fulfilled') entry.session = shown.value.session ?? s
  return entry
}
;(async () => {
  const listed = await run(['ls', '--json'])
  const queue = [...(listed.sessions ?? listed)].filter((s) => mode !== 'detail' || only.has(s.id))
  const out = []
  await Promise.all(Array.from({ length: 4 }, async () => {
    while (queue.length) out.push(await one(queue.shift()))
  }))
  process.stdout.write(JSON.stringify(out))
})().catch((e) => { process.stderr.write(String(e.message)); process.exit(1) })
`

// A non-interactive SSH command gets a minimal PATH, and the host's login shell may not be POSIX,
// so remote commands run through `env` with a PATH covering the usual install locations.
export const DEFAULT_REMOTE_PATH = '/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin'

/**
 * @param {string} host
 * @param {{ bossBin: string, remoteBoss: string, remotePath: string }} where
 * @param {{ mode?: 'full' | 'pr' | 'detail', ids?: string[] }} [scope]
 */
function dumpDaemon(host, { bossBin, remoteBoss, remotePath }, { mode = 'full', ids = [] } = {}) {
  /** @type {import('node:child_process').ExecFileSyncOptionsWithStringEncoding} */
  const opts = { input: DAEMON_DUMP, encoding: 'utf8', maxBuffer: 64 << 20 }
  const scope = [mode, ...ids]
  const raw = host
    ? execFileSync(
        'ssh',
        [
          '-o',
          'BatchMode=yes',
          host,
          'env',
          `PATH=${remotePath}`,
          'node',
          '-',
          remoteBoss,
          ...scope,
        ],
        opts,
      )
    : execFileSync('node', ['-', bossBin, ...scope], opts)
  return JSON.parse(raw)
}

// Outcomes the PR state alone settles, whatever the session's chats say; none of them is
// agent-movable.
const SETTLED_BY_PR = new Set(['awaiting-operator', 'done', 'stale-session', 'skip'])

/**
 * The quick read a cron gate can afford: the session list and PR state for every session, then
 * chats and `boss show` only for the sessions whose PR state does not settle them. A full read of a
 * busy daemon takes longer than a gate may run.
 */
function quickEntries(host, where, dump, now) {
  const entries = dump(host, where, { mode: 'pr' })
  const settled = (entry) =>
    SETTLED_BY_PR.has(
      classifySession({ session: entry.session, chats: [], pr: entry.pr, now }).action,
    )
  const open = entries.filter((entry) => (entry.errors ?? []).length || !settled(entry))
  if (!open.length) return entries
  const detailed = new Map(
    dump(host, where, { mode: 'detail', ids: open.map((entry) => entry.session.id) }).map(
      (entry) => [entry.session.id, entry],
    ),
  )
  return entries.map((entry) => detailed.get(entry.session.id) ?? entry)
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
  crons = readCrons,
  unattended = false,
  respectOwners = false,
  timeoutMs = EXTENSION_SCRIPT_TIMEOUT_MS,
  seen = null,
  quick = false,
}) {
  const where = { bossBin, remoteBoss, remotePath }
  const claimed = claimTargets({
    targets: [
      ...(local ? [{ host: '', label: 'local' }] : []),
      ...hosts.map((host) => ({ host, label: host })),
    ],
    unattended,
    crons,
    where,
    core,
    respectOwners,
  })
  const rows = []
  const errors = [...claimed.errors]
  // Hosts whose daemon could not be read this round: nothing about their sessions is known.
  const unread = [...claimed.unread]
  // What `remedy` needs to rebuild a row's `next` after an extension changes its action.
  const remedyContext = new Map()
  for (const target of claimed.mine) {
    let entries
    try {
      entries = quick ? quickEntries(target.host, where, dump, now) : dump(target.host, where)
    } catch (error) {
      errors.push(`${target.label}: ${String(error.message).split('\n')[0]}`)
      unread.push(target.label)
      continue
    }
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
      const verdict = classifySession({ session, chats, pr, now })
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
        })),
        failing: failingChecks(pr),
        autoRepair: session.repo_can_auto_repair !== false,
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
  // The grace spans rounds, so it applies only when the caller carries `seen` between them. An
  // owned daemon's clocks are its worker's, which may share this state file.
  if (seen) applyGrace(rows, seen, now, [...unread, ...claimed.owners.map((owner) => owner.host)])
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
    // With `respectOwners`, the daemons a worker cron owns: the attended chat reads their queues,
    // never their rows. Empty otherwise.
    owners: claimed.owners,
    sharedReds: shared,
    // The orchestrator reads each rules extension's SKILL.md and applies it every round.
    rules: extensions.rules.map(({ name, skillPath }) => ({ name, skillPath })),
    operatorItems,
    // The round extensions that ran: an item one of them stopped reporting has cleared.
    roundExtensions,
    ledger,
    seen: seen ?? {},
  }
}

export const DEFAULT_STATE = path.join(homedir(), '.cache', 'boss-orchestrate', 'seen.json')

/**
 * The cron gate for an unattended worker round, following the cron-gate contract: exit 0 when a
 * readable row is agent-movable after the grace window and not already queued for the operator, or
 * when the operator queue needs syncing; 1 when there is nothing to do; 69 when it could not
 * evaluate. It takes the quick read, so an idle factory costs no agent tokens and the gate stays
 * inside the scheduler's timeout. `onSnapshot` receives the snapshot before the verdict (the CLI
 * saves the grace state there).
 * @param {Parameters<typeof collect>[0] & {
 *   client: { list: () => ReturnType<ReturnType<typeof notesClient>['list']> },
 *   onSnapshot?: (snapshot: ReturnType<typeof collect>) => void,
 * }} options
 */
export function evaluateGate({ client, onSnapshot = () => {}, ...collectOptions }) {
  try {
    // The gate is the worker's own, so it keeps the worker's daemon.
    const snapshot = collect({ ...collectOptions, quick: true, unattended: true })
    onSnapshot(snapshot)
    return gateVerdict({ snapshot, items: client.list() })
  } catch (error) {
    return { exit: 69, reason: `could not evaluate: ${String(error.message).split('\n')[0]}` }
  }
}

function readSeen(file) {
  try {
    const parsed = JSON.parse(readFileSync(file, 'utf8'))
    return parsed && typeof parsed === 'object' && !Array.isArray(parsed) ? parsed : {}
  } catch {
    return {}
  }
}

function pane({ host, chat, lines, remotePath }) {
  const script = `export PATH=${remotePath}
t=$(tmux ls -F '#S' 2>/dev/null | grep -- "-${chat.slice(0, 8)}$" | head -1)
[ -n "$t" ] || { echo "no tmux pane for chat ${chat}" >&2; exit 3; }
echo "== $t $(tmux display -p -t "$t" '#{pane_current_path}')"
tmux capture-pane -p -t "$t" -S -${lines * 3} | grep -v '^[[:space:]]*$' | tail -${lines}`
  /** @type {import('node:child_process').ExecFileSyncOptionsWithStringEncoding} */
  const opts = { input: script, encoding: 'utf8', stdio: ['pipe', 'inherit', 'inherit'] }
  if (host) execFileSync('ssh', ['-o', 'BatchMode=yes', host, 'bash', '-s'], opts)
  else execFileSync('bash', ['-s'], opts)
}

const ACTION_ORDER = [
  'waive-cap',
  'unblock',
  'upstream-red',
  'mark-ready',
  'refresh-park',
  'ask-operator',
  'repair',
  'verify',
  'stalled',
  'stale-session',
  'grace',
  'wait-ci',
  'waiting',
  'working',
  'awaiting-operator',
  'ok',
  'done',
  'skip',
]

function printTable({
  rows,
  errors,
  owners = [],
  sharedReds: shared = [],
  rules = [],
  operatorItems = [],
  ledger = [],
}) {
  const sorted = rows
    .filter((row) => row.action !== 'skip')
    .sort((a, b) => ACTION_ORDER.indexOf(a.action) - ACTION_ORDER.indexOf(b.action))
  const counts = {}
  for (const row of sorted) counts[row.action] = (counts[row.action] ?? 0) + 1
  const short = (row) =>
    row.pr ? `${String(row.repo).split('/').pop()}#${row.pr}` : String(row.repo)
  for (const row of sorted) {
    if (row.action === 'done' || row.action === 'awaiting-operator') continue
    process.stdout.write(
      `${row.action.padEnd(13)} ${row.host.padEnd(7)} ${row.session} ${row.state.padEnd(18)} ${short(row)}\n` +
        `    ${(row.title ?? '').slice(0, 90)}\n    why: ${row.why}\n` +
        (row.classifiedBy ? `    classified by: ${row.classifiedBy}\n` : '') +
        (row.next ? `    next: ${row.next}\n` : ''),
    )
  }
  // Parked and done rows are re-judged every round; list them so a regression or a gap shows.
  const list = (action) => sorted.filter((row) => row.action === action)
  const awaiting = list('awaiting-operator')
  if (awaiting.length) {
    process.stdout.write(`\nwaiting on the operator (verified as far as policy allows):\n`)
    for (const row of awaiting)
      // gate-region-ok: a display cap on one reason line, not a section window.
      process.stdout.write(`    ${short(row).padEnd(22)} ${row.why.slice(0, 100)}\n`)
  }
  const done = list('done')
  if (done.length)
    process.stdout.write(`\ndone (verified, ready for review): ${done.map(short).join(' ')}\n`)
  for (const group of shared) {
    const prs = group.prs.map((p) => `#${p.pr} (${p.session}, ${p.action})`).join(', ')
    process.stdout.write(
      `\nshared red: ${group.repo} check "${group.check}" fails on ${prs}; elect one fixer, hold the rest\n`,
    )
  }
  if (operatorItems.length) {
    process.stdout.write(`\nfrom extensions, for the operator:\n`)
    for (const item of operatorItems)
      process.stdout.write(`    ${item.title} (${item.extension})\n      ${item.detail}\n`)
  }
  for (const rule of rules) process.stdout.write(`\nrules: ${rule.name} ${rule.skillPath}\n`)
  for (const { host, cron } of owners) {
    const queue = host === 'local' ? 'here' : `with --queue-host ${host}`
    process.stdout.write(`\nowned: ${host} by worker cron ${cron}; read its queue ${queue}\n`)
  }
  process.stdout.write(
    `\nsummary: ${Object.entries(counts)
      .map(([k, v]) => `${k}=${v}`)
      .join(' ')}\n`,
  )
  for (const error of errors) process.stdout.write(`error: ${error}\n`)
  for (const line of ledger) process.stdout.write(`${line}\n`)
}

function main(argv) {
  const { values, positionals } = parseArgs({
    args: argv,
    allowPositionals: true,
    strict: true,
    options: {
      host: { type: 'string', multiple: true, default: [] },
      'no-local': { type: 'boolean', default: false },
      // A worker round: keep this machine's daemon even though its worker cron owns it.
      unattended: { type: 'boolean', default: false },
      // Leave out each daemon whose own cron list runs an unattended worker (`owners`).
      'respect-owners': { type: 'boolean', default: false },
      json: { type: 'boolean', default: false },
      chat: { type: 'string' },
      lines: { type: 'string', default: '40' },
      boss: { type: 'string', default: process.env.BOSS_BIN || 'boss' },
      'remote-boss': { type: 'string', default: 'boss' },
      'remote-path': { type: 'string', default: DEFAULT_REMOTE_PATH },
      root: { type: 'string', default: process.cwd() },
      core: { type: 'string', default: ORCHESTRATE_CORE },
      // Remembers when each repair/verify row was first seen, so the grace spans rounds. The cron
      // gate and the worker it wakes run in different environments, so it lives under $HOME.
      state: { type: 'string', default: DEFAULT_STATE },
      tag: { type: 'string', default: QUEUE_TAG },
      repo: { type: 'string' },
      'queue-host': { type: 'string' },
    },
  })
  const [command] = positionals
  const options = {
    hosts: values.host,
    local: !values['no-local'],
    unattended: values.unattended,
    respectOwners: values['respect-owners'],
    bossBin: values.boss,
    remoteBoss: values['remote-boss'],
    remotePath: values['remote-path'],
    root: values.root,
    core: values.core,
    seen: readSeen(values.state),
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
  if (command === 'gate') {
    const client = notesClient({
      run: bossRunner(values.boss),
      tag: values.tag,
      repo: values.repo,
      bossHost: values['queue-host'],
    })
    const { exit, reason } = evaluateGate({ ...options, client, onSnapshot: saveSeen })
    process.stderr.write(`bs-orchestrate gate: ${reason}\n`)
    return exit
  }
  if (command === 'pane') {
    if (!values.chat) throw new Error('pane needs --chat <chat-id>')
    pane({
      host: values.host[0] ?? '',
      chat: values.chat,
      lines: Number(values.lines),
      remotePath: values['remote-path'],
    })
    return 0
  }
  process.stderr.write(
    'usage: orchestrate-snapshot.mjs collect|gate|pane [--host <ssh-host>]... [--no-local] [--json]\n',
  )
  return 64
}

/** Runs one verb and returns its exit code. A repo-local gate wrapper calls it with its hosts. */
export function run(argv) {
  try {
    return main(argv)
  } catch (error) {
    process.stderr.write(`orchestrate-snapshot: ${String(error.message).split('\n')[0]}\n`)
    // A gate that cannot parse its own arguments could not evaluate.
    return argv[0] === 'gate' ? 69 : 1
  }
}

if (isMainModule(import.meta.url)) process.exitCode = run(process.argv.slice(2))
