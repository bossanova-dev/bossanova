// The I/O half of the epic driver: `reconcileEpic`'s EPIC_DRIVER_IO contract implemented over the
// `boss` CLI (sessions, chats, callbacks, subscriptions, merge), `gh` (provider PR state) and the
// configured tracker adapter. Every external call goes through one injected `exec`, so a test
// drives the whole cycle with fakes and asserts exactly what ran.
//
// Tracker operations whose resolved transport is `command` run here. An `mcp` operation cannot be
// called from a node process, so it is returned to the host as a descriptor in `deferred`, the same
// contract `epic-cli.mjs upsert-progress` uses; the host executes each one exactly once.

import { spawnSync } from 'node:child_process'
import { resolveOperation, runOperation } from '../tracker/operation-runner.mjs'
import { epicWaitGroup } from '../callback/epic-target.mjs'

/** Callback rows the daemon still holds for delivery count as live coverage. */
const LIVE_CALLBACK_STATES = new Set(['active', 'leased'])
/** Callback rows that already delivered are consumed one-shots. */
const FIRED_CALLBACK_STATES = new Set(['triggered', 'delivered', 'fired'])

/** The daemon's display statuses folded onto the driver's three-state check verdict. */
const PASSING_DISPLAY = new Set(['passing'])
const FAILING_DISPLAY = new Set(['failing', 'conflict', 'rejected'])

const TERMINAL_SESSION_STATES = new Set(['CLOSED', 'MERGED'])

/** Check-run conclusions GitHub's `checks_failed` trigger treats as a failure. */
const FAILING_CONCLUSIONS = new Set([
  'failure',
  'timed_out',
  'cancelled',
  'action_required',
  'startup_failure',
])

/**
 * `exec(cmd, args, {input, cwd}) → {status, stdout, stderr}` over `spawnSync`.
 * @param {string} cmd @param {string[]} args @param {{input?: string, cwd?: string}} [options]
 */
export function defaultExec(cmd, args, { input, cwd } = {}) {
  const result = spawnSync(cmd, args, {
    cwd,
    input,
    encoding: 'utf8',
    maxBuffer: 64 * 1024 * 1024,
  })
  if (result.error) return { status: 127, stdout: '', stderr: result.error.message }
  return { status: result.status ?? 1, stdout: result.stdout ?? '', stderr: result.stderr ?? '' }
}

function sleepMs(ms) {
  Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, ms)
}

function isNonEmptyString(value) {
  return typeof value === 'string' && value.trim().length > 0
}

function lastLine(text) {
  return (
    String(text ?? '')
      .trim()
      .split(/\r?\n/)
      .at(-1) || 'no diagnostic'
  )
}

/** Normalize a `boss callback list` row's state onto `needsRearm`'s vocabulary. */
export function normalizeCallbackRow(row) {
  const raw = String(row?.state ?? '').toLowerCase()
  const state = LIVE_CALLBACK_STATES.has(raw)
    ? 'active'
    : FIRED_CALLBACK_STATES.has(raw)
      ? 'fired'
      : raw
  return { ...row, id: row?.id ?? '', trigger: row?.trigger ?? '', state }
}

/** Fold the daemon's display status onto `greenAdmissionBlockers`' `checkVerdict`. */
export function checkVerdictFromDisplayStatus(displayStatus) {
  const status = String(displayStatus ?? '')
    .trim()
    .toLowerCase()
  if (PASSING_DISPLAY.has(status)) return { state: 'passing', reason: `display:${status}` }
  if (FAILING_DISPLAY.has(status)) return { state: 'failing', reason: `display:${status}` }
  return { state: 'pending', reason: status ? `display:${status}` : 'display:unknown' }
}

/** A REST pull-request payload in `gh pr view --json` vocabulary (`MERGED`/`OPEN`/`CLOSED`). */
export function pullFromRest(pull) {
  return {
    number: pull.number,
    url: pull.html_url,
    isDraft: pull.draft === true,
    title: pull.title,
    body: pull.body ?? '',
    state: pull.merged === true || pull.merged_at ? 'MERGED' : String(pull.state).toUpperCase(),
    mergeable:
      pull.mergeable === true ? 'MERGEABLE' : pull.mergeable === false ? 'CONFLICTING' : 'UNKNOWN',
    headRefName: pull.head?.ref ?? '',
    headSha: pull.head?.sha ?? '',
  }
}

/** True when `launchedAt` is older than the budget. An unreadable budget or start never expires. */
export function wallClockExceeded({ launchedAt, budgetMinutes, now }) {
  const started = Date.parse(launchedAt ?? '')
  const current = Date.parse(now ?? '')
  if (!Number.isFinite(started) || !Number.isFinite(current)) return false
  if (!Number.isFinite(budgetMinutes) || budgetMinutes <= 0) return false
  return current - started > budgetMinutes * 60_000
}

/**
 * Build the driver I/O for one reconciliation cycle.
 *
 * @param {object} options
 * @param {object} options.state       the persisted epic state the cycle starts from
 * @param {string} options.repoId      boss repository id
 * @param {string} options.prRepo      provider repository as owner/repo
 * @param {string} options.driverChatId the orchestrator chat every wake is delivered to
 * @param {string} [options.boss]      boss binary path
 * @param {string} [options.cwd]       working directory for boss/gh
 * @param {string} [options.model]     child model id
 * @param {string} [options.implementCommand] child prompt prefix, default `/boss-build`
 * @param {string} [options.promptSuffix] appended to every child prompt (e.g. ` --team X`)
 * @param {string} [options.trackerSource] `boss new --tracker-source` value; empty omits it
 * @param {{operationMap: object}} options.trackerAdapter
 * @param {string} [options.doneState] the tracker state a merged child moves to
 * @param {Record<string, 'cleared'|'open'>} [options.blockerStates] host-read external blockers
 * @param {Record<string, string>} [options.ticketUrls] tracker URL per ticket id
 * @param {string} [options.requiredStatus] a commit status (e.g. the repo's verify gate) that must be
 *   `success` on the PR head before checks count as passing; empty requires none
 * @param {{mechanism: string, nextWakeAt: string}|null} [options.fallbackWake] the host's armed in-session wake
 * @param {string} [options.expiresIn] callback/subscription lifetime
 * @param {(input: any, deps: any) => Promise<any>} options.upsertProgressImpl `epic-cli` upsertProgress
 * @param {typeof defaultExec} [options.exec]
 * @param {typeof runOperation} [options.runOperationImpl]
 * @param {string} [options.now]
 * @param {(ms: number) => void} [options.sleep]
 */
export function createEpicIo({
  state,
  repoId,
  prRepo,
  driverChatId,
  boss = 'boss',
  cwd,
  model = '',
  implementCommand = '/boss-build',
  promptSuffix = '',
  trackerSource = '',
  trackerAdapter,
  doneState = 'completed',
  blockerStates = {},
  ticketUrls = {},
  requiredStatus = '',
  fallbackWake = null,
  expiresIn = '7d',
  upsertProgressImpl,
  exec = defaultExec,
  runOperationImpl = runOperation,
  now = new Date().toISOString(),
  sleep = sleepMs,
}) {
  if (!isNonEmptyString(repoId)) throw new Error('createEpicIo: repoId is required')
  if (!isNonEmptyString(prRepo)) throw new Error('createEpicIo: prRepo (owner/repo) is required')
  if (!isNonEmptyString(driverChatId)) throw new Error('createEpicIo: driverChatId is required')
  if (typeof upsertProgressImpl !== 'function')
    throw new Error('createEpicIo: upsertProgressImpl is required')

  /** Tracker operations the host must still execute (MCP transport), in order. */
  const deferred = []
  /** Every non-fatal I/O problem, so the cycle's caller can report it. */
  const warnings = []
  const ticketIds = new Set((state.tickets ?? []).map((ticket) => ticket.id))

  const run = (cmd, args, input) => exec(cmd, args, { input, cwd })
  const bossJson = (args, input) => {
    const result = run(boss, args, input)
    if (result.status !== 0) {
      throw new Error(`boss ${args.slice(0, 2).join(' ')} failed: ${lastLine(result.stderr)}`)
    }
    try {
      return JSON.parse(result.stdout)
    } catch {
      throw new Error(`boss ${args.slice(0, 2).join(' ')} printed no JSON`)
    }
  }
  const chatsOf = (sessionId) => {
    const payload = bossJson(['chats', sessionId, '--json'])
    return Array.isArray(payload) ? payload : (payload?.chats ?? [])
  }
  const primaryChatId = (sessionId) => {
    const rows = chatsOf(sessionId)
      .filter((row) => isNonEmptyString(row?.agent_session_id))
      .sort((a, b) => String(a.created_at ?? '').localeCompare(String(b.created_at ?? '')))
    return rows[0]?.agent_session_id ?? ''
  }
  // REST, not `gh pr view`: GraphQL is the bucket every agent on the account drains, and an
  // exhausted one would hold every child as unreadable.
  const prView = (prNumber) => {
    const result = run('gh', ['api', `repos/${prRepo}/pulls/${prNumber}`])
    if (result.status !== 0) {
      warnings.push(`gh api pulls/${prNumber}: ${lastLine(result.stderr)}`)
      return null
    }
    try {
      return pullFromRest(JSON.parse(result.stdout))
    } catch {
      warnings.push(`gh api pulls/${prNumber}: printed no JSON`)
      return null
    }
  }
  /** The epic triggers whose condition the PR satisfies right now. An unreadable PR holds none. */
  const satisfiedTriggers = (prNumber) => {
    const pr = prView(prNumber)
    if (!pr) return new Set()
    const open = pr.state === 'OPEN'
    const held = new Set()
    if (pr.state === 'MERGED') held.add('merged')
    if (pr.state === 'CLOSED') held.add('closed')
    if (open && !pr.isDraft) held.add('ready_for_review')
    // GitHub judges the check triggers on the head's own check runs, not on the daemon's display
    // status (which also folds in conflicts and verify), so read the same thing it reads.
    const runs = checkRunSummary(pr.headSha)
    if (open && runs.failing) held.add('checks_failed')
    // An unreadable run list counts as passing here: arming on a ready PR is what loops.
    if (open && !pr.isDraft && !runs.failing && (!runs.readable || !runs.pending)) {
      held.add('checks_passed_ready')
    }
    return held
  }
  /** `{readable, failing, pending}` over the head's GitHub check runs. */
  const checkRunSummary = (sha) => {
    if (!isNonEmptyString(sha)) return { readable: false, failing: false, pending: false }
    const result = run('gh', ['api', `repos/${prRepo}/commits/${sha}/check-runs?per_page=100`])
    if (result.status !== 0) return { readable: false, failing: false, pending: false }
    try {
      const rows = JSON.parse(result.stdout)?.check_runs ?? []
      return {
        readable: true,
        failing: rows.some((row) => FAILING_CONCLUSIONS.has(String(row?.conclusion ?? ''))),
        pending: rows.some((row) => row?.status !== 'completed'),
      }
    } catch {
      return { readable: false, failing: false, pending: false }
    }
  }
  /** The latest state of `requiredStatus` on `sha`, or '' when unread or not required. */
  const requiredStatusState = (sha) => {
    if (!isNonEmptyString(requiredStatus) || !isNonEmptyString(sha)) return ''
    const result = run('gh', ['api', `repos/${prRepo}/commits/${sha}/status`])
    if (result.status !== 0) {
      warnings.push(`gh api status ${sha.slice(0, 7)}: ${lastLine(result.stderr)}`)
      return 'unknown'
    }
    try {
      const row = (JSON.parse(result.stdout)?.statuses ?? []).find(
        (status) => status?.context === requiredStatus,
      )
      return row?.state ?? 'missing'
    } catch {
      return 'unknown'
    }
  }
  /** Passing checks only count once the required status succeeded; it never upgrades a verdict. */
  const gateOnRequiredStatus = (verdict, statusState) =>
    !isNonEmptyString(requiredStatus) || verdict.state !== 'passing' || statusState === 'success'
      ? verdict
      : { state: 'pending', reason: `${requiredStatus}:${statusState || 'unknown'}` }
  const tracker = (key, args) => {
    const op = resolveOperation(trackerAdapter?.operationMap, key)
    if (!op) throw new Error(`tracker operation ${key} is not available`)
    if (op.transport === 'mcp') {
      deferred.push({ operation: key, tool: op.tool, args, outcome: 'descriptor-emitted' })
      return { deferred: true }
    }
    return { deferred: false, response: runOperationImpl(op, args) }
  }

  const io = {
    listSessions() {
      const payload = bossJson(['ls', '--repo', repoId, '--json'])
      const sessions = Array.isArray(payload) ? payload : (payload?.sessions ?? [])
      return sessions
        .filter((session) => !TERMINAL_SESSION_STATES.has(String(session?.state ?? '')))
        .filter((session) => {
          const tracked = isNonEmptyString(session?.tracker_id) && ticketIds.has(session.tracker_id)
          const titled = [...ticketIds].some((id) =>
            String(session?.title ?? '').startsWith(`[${id}]`),
          )
          return tracked || titled
        })
        .map((session) => ({
          ...session,
          agent_session_id: session.agent_session_id ?? primaryChatId(session.id),
        }))
    },

    readSessionSnapshot({ ticketId, sessionId, chatId }) {
      const session = bossJson(['show', sessionId, '--json'])?.session ?? {}
      let chats = []
      let chatsReadable = true
      try {
        chats = chatsOf(sessionId)
      } catch (error) {
        chatsReadable = false
        warnings.push(`${ticketId}: ${error.message}`)
      }
      const chat = chats.find((row) => row?.agent_session_id === chatId) ?? null
      const pr = Number.isFinite(session.pr_number) ? prView(session.pr_number) : null
      const record = state.sessions?.[ticketId] ?? {}
      return {
        ticketId,
        sessionState: session.state,
        // The daemon records a run's concluding BLOCKED report as `blocked_reason`; with the chat
        // idle, that is the agent's own conclusion, which routes the child to repair.
        lastMessageKind:
          session.state === 'BLOCKED' &&
          isNonEmptyString(session.blocked_reason) &&
          ['IDLE', 'STOPPED'].includes(String(chat?.status ?? ''))
            ? 'agent-conclusion'
            : undefined,
        blockedReason: session.blocked_reason ?? '',
        displayLabel: session.display_label ?? undefined,
        displayStatus: session.display_status ?? '',
        chatStatus: chat?.status,
        chatStatusReadable: chatsReadable && chat !== null,
        spinnerPresent: chat?.spinner_present === true,
        waitingReason: chat?.waiting_reason ?? '',
        repairActive: session.repair_active === true,
        checkVerdict: gateOnRequiredStatus(
          checkVerdictFromDisplayStatus(session.display_status),
          pr ? requiredStatusState(pr.headSha) : '',
        ),
        wallClockExceeded: wallClockExceeded({
          launchedAt: record.launchedAt,
          budgetMinutes: state.childWallClockMinutes,
          now,
        }),
        prView: pr
          ? {
              number: pr.number,
              repo: prRepo,
              url: pr.url,
              isDraft: pr.isDraft,
              title: pr.title,
              body: pr.body,
              state: pr.state,
              mergeable: pr.mergeable,
              headRefName: pr.headRefName,
            }
          : Number.isFinite(session.pr_number)
            ? { number: session.pr_number, repo: prRepo, url: session.pr_url ?? '' }
            : undefined,
      }
    },

    readExternalBlockers(ids) {
      const missing = ids.filter((id) => !Object.hasOwn(blockerStates, id))
      if (missing.length > 0) {
        throw new Error(`external blocker state not supplied for ${missing.join(', ')}`)
      }
      return Object.fromEntries(ids.map((id) => [id, blockerStates[id]]))
    },

    createSession({ ticket }) {
      const args = [
        'new',
        '--repo',
        repoId,
        '--prompt',
        `${implementCommand} ${ticket.id}${promptSuffix}`,
        '--title',
        `[${ticket.id}] ${ticket.title ?? ''}`.trim(),
        '--agent',
        state.agent || 'claude',
        '--tmux-unattended',
        '--tracker-id',
        ticket.id,
        '--json',
      ]
      if (isNonEmptyString(model)) args.push('--model', model)
      if (isNonEmptyString(trackerSource)) args.push('--tracker-source', trackerSource)
      const url = ticket.url ?? ticketUrls[ticket.id]
      if (isNonEmptyString(url)) args.push('--tracker-url', url)
      const created = bossJson(args)
      const sessionId = created?.session?.id ?? created?.id ?? ''
      if (!isNonEmptyString(sessionId)) throw new Error(`boss new for ${ticket.id} returned no id`)
      let chatId = created?.session?.chat_id ?? created?.chat_id ?? ''
      // The daemon can report the chat after the session; wait a bounded time for it.
      for (let attempt = 0; !isNonEmptyString(chatId) && attempt < 12; attempt += 1) {
        sleep(5_000)
        chatId = primaryChatId(sessionId)
      }
      if (!isNonEmptyString(chatId)) {
        throw new Error(`boss new for ${ticket.id} (${sessionId}) produced no chat id`)
      }
      return { sessionId, chatId }
    },

    armWatches({ prNumber, triggers, message }) {
      if (!Number.isFinite(prNumber)) return { callbacks: [], error: 'child has no PR yet' }
      const callbacks = []
      const errors = []
      // Triggers match on state, so one armed while its condition holds fires at once, is
      // re-armed by the next cycle, and fires again: a wake loop. Leave those to the fallback.
      const held = satisfiedTriggers(prNumber)
      const skipped = triggers.filter((trigger) => held.has(trigger))
      if (skipped.length > 0) {
        errors.push(`condition already holds for ${skipped.join(',')}; the fallback wake covers it`)
      }
      for (const trigger of triggers.filter((candidate) => !held.has(candidate))) {
        const result = run(boss, [
          'callback',
          'add',
          String(prNumber),
          trigger,
          '--repo',
          prRepo,
          '--chat',
          driverChatId,
          '--group',
          epicWaitGroup(prNumber, trigger),
          '--expires-in',
          expiresIn,
          '--message',
          message,
          '--json',
        ])
        if (result.status !== 0) {
          errors.push(`${trigger}: ${lastLine(result.stderr)}`)
          continue
        }
        try {
          callbacks.push(normalizeCallbackRow(JSON.parse(result.stdout)))
        } catch {
          errors.push(`${trigger}: printed no JSON`)
        }
      }
      return { callbacks, ...(errors.length ? { error: errors.join('; ') } : {}) }
    },

    listWatches({ prNumber }) {
      if (!Number.isFinite(prNumber)) return []
      const payload = bossJson([
        'callback',
        'list',
        '--repo',
        prRepo,
        '--chat',
        driverChatId,
        '--pr',
        String(prNumber),
        '--json',
      ])
      const rows = Array.isArray(payload) ? payload : (payload?.callbacks ?? [])
      // Only live rows are coverage; a delivered one-shot must read as absent so it re-arms.
      return rows.map(normalizeCallbackRow).filter((row) => row.state === 'active')
    },

    subscribeSessionOutcome({ sessionId, message }) {
      const result = run(
        boss,
        [
          'broadcast',
          'subscribe',
          '--on',
          'settled',
          '--session',
          sessionId,
          '--to',
          `chat:${driverChatId}`,
          '--from',
          driverChatId,
          '--expires-in',
          expiresIn,
          '--message',
          '-',
          '--json',
        ],
        message,
      )
      if (result.status !== 0) return { error: lastLine(result.stderr) }
      try {
        const payload = JSON.parse(result.stdout)
        return { subscription: payload?.subscription ?? payload }
      } catch {
        return { error: 'boss broadcast subscribe printed no JSON' }
      }
    },

    listSessionSubscriptions({ sessionId }) {
      const payload = bossJson([
        'broadcast',
        'subscriptions',
        '--session',
        sessionId,
        '--on',
        'settled',
        '--json',
      ])
      return Array.isArray(payload) ? payload : (payload?.subscriptions ?? [])
    },

    removeWatches({ ticketId, callbackIds = [], subscriptionId = '' }) {
      for (const id of callbackIds) {
        const result = run(boss, ['callback', 'remove', id, '--chat', driverChatId])
        if (result.status !== 0) warnings.push(`${ticketId}: callback remove ${id} failed`)
      }
      if (isNonEmptyString(subscriptionId)) {
        const result = run(boss, ['broadcast', 'unsubscribe', subscriptionId])
        if (result.status !== 0) warnings.push(`${ticketId}: unsubscribe ${subscriptionId} failed`)
      }
    },

    scheduleFallbackWake() {
      return fallbackWake && isNonEmptyString(fallbackWake.mechanism) ? { ...fallbackWake } : null
    },

    mergeChild({ sessionId }) {
      const result = run(boss, ['merge', sessionId, '-y', '--json'])
      if (result.status !== 0) return { merged: false, error: lastLine(result.stderr) }
      try {
        const payload = JSON.parse(result.stdout)
        return { merged: String(payload?.session?.state ?? payload?.state ?? '') === 'MERGED' }
      } catch {
        return { merged: false, error: 'boss merge printed no JSON' }
      }
    },

    verifyMerged({ ticketId, sessionId }) {
      const number = state.sessions?.[ticketId]?.prNumber
      if (Number.isFinite(number)) {
        const pr = prView(number)
        if (pr) return pr.state === 'MERGED'
      }
      const session = bossJson(['show', sessionId, '--json'])?.session ?? {}
      return session.state === 'MERGED' || session.display_status === 'merged'
    },

    moveTicketDone({ ticketId }) {
      try {
        tracker('moveState', { id: ticketId, state: doneState })
        return true
      } catch (error) {
        warnings.push(`${ticketId}: move to ${doneState} failed: ${error.message}`)
        return false
      }
    },

    async upsertProgress({ state: current, progressState }) {
      const result = await upsertProgressImpl(
        {
          state: current,
          now: () => progressState.updatedAt || now,
          nextWake: progressState.run?.nextWake ?? '',
        },
        { adapter: trackerAdapter, runOperation: runOperationImpl },
      )
      const rows = result?.results ?? []
      for (const row of rows) {
        if (row.error) warnings.push(`progress ${row.issueId}: ${row.error}`)
      }
      const pending = rows.filter(
        (row) => row.outcome === 'comments-required' || row.outcome === 'descriptor-emitted',
      )
      if (pending.length > 0) {
        // Only the cycle's LAST progress write matters; an earlier descriptor is superseded.
        for (let i = deferred.length - 1; i >= 0; i -= 1) {
          if (deferred[i].operation === 'progress') deferred.splice(i, 1)
        }
        deferred.push({ operation: 'progress', results: pending })
      }
      const written = rows.find((row) => isNonEmptyString(row.commentId))
      return { commentId: written?.commentId ?? current.progressCommentId ?? '' }
    },
  }

  return { io, deferred, warnings, tracker, prView }
}
