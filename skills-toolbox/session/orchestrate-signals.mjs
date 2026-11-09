// The facts about chats, reds and runs that the orchestrate snapshot classifies on: which chats are
// live, which show a dialog, which sit idle with nothing armed to wake them, which reds come from
// infrastructure rather than the change, which sessions are finished cron runs, and which PRs are
// dependabot's.

import { QUESTION_TITLE_PREFIX } from './orchestrate-ask.mjs'

const MINUTE = 60 * 1000
// How long the newest chat of an in-progress session may sit idle, with nothing armed to wake it,
// before it counts as stuck rather than between steps.
export const CHAT_IDLE_AFTER = 10 * MINUTE

// A red caused by infrastructure rather than the change: a 5xx from GitHub or a download host,
// `curl` failing on an HTTP error (exit 22), a download that ran out of attempts, a lost runner.
const INFRA_RED = new RegExp(
  [
    String.raw`\bHTTP[ /]?(?:\d(?:\.\d)? )?5\d\d\b`,
    String.raw`\b5\d\d (?:Bad Gateway|Gateway Time-?out|Service Unavailable|Internal Server Error)\b`,
    String.raw`\bcurl\b.*\bexit(?: code| status)? 22\b`,
    String.raw`\bexit(?: code| status)? 22\b.*\bcurl\b`,
    String.raw`\b(?:exhausted|used up|ran out of)\b.*\b(?:attempts|retries)\b.*\bdownload`,
    String.raw`\bdownload\w*\b.*\b(?:attempts|retries)\b.*\bexhausted\b`,
    String.raw`\brunner\b.*\b(?:lost|went away)\b`,
    String.raw`\blost communication with the server\b`,
  ].join('|'),
  'i',
)

/** Whether `text` (a blocked reason, a residual, a failed step) records an infrastructure red. */
export function isInfraRed(text) {
  return INFRA_RED.test(String(text ?? ''))
}

export function isLive(chat) {
  return chat.spinner_present === true || chat.status === 'WORKING'
}

// The orchestrator's own question chats (orchestrate-ask) show a dialog on purpose.
const isOwnQuestion = (chat) => String(chat.title ?? '').startsWith(QUESTION_TITLE_PREFIX)

/** The chats showing a dialog (`QUESTION`), other than the orchestrator's own question chats. */
export function questionChats(chats = []) {
  return chats.filter((chat) => chat.status === 'QUESTION' && !isOwnQuestion(chat))
}

const OPERATOR_ROLES = new Set(['operator', 'coordinator'])

const timeOf = (value) => {
  const parsed = Date.parse(value ?? '')
  return Number.isNaN(parsed) ? null : parsed
}

/** The most recently created chat, or null when there are none. */
export function newestChat(chats = []) {
  const created = (chat) => timeOf(chat.created_at) ?? 0
  return [...chats].sort((a, b) => created(b) - created(a))[0] ?? null
}

/**
 * How long `chat` has produced no output: since its last substantive output, else its last
 * output. Null when it has neither.
 */
export function chatQuietFor(chat, now = Date.now()) {
  const last = timeOf(chat?.last_substantive_output_at) ?? timeOf(chat?.last_output_at)
  return last === null ? null : now - last
}

/**
 * The idle chat of an in-progress session that nothing will wake, with how long it has been idle,
 * or null: no chat is live, none has armed a callback (`waiting_reason`), and the newest one is
 * IDLE with no output for CHAT_IDLE_AFTER. Idle time is read from the last substantive output, else
 * the last output; a chat with neither is left to the session-level `stalled` rule.
 */
export function stalledChat(chats = [], now = Date.now()) {
  if (!chats.length || chats.some(isLive) || chats.some((chat) => chat.waiting_reason)) return null
  const newest = newestChat(chats)
  // An operator's own conversation (an attended plan chat, say) waits on them, not on a nudge.
  if (newest.status !== 'IDLE' || OPERATOR_ROLES.has(newest.role)) return null
  const quiet = chatQuietFor(newest, now)
  if (quiet === null || quiet < CHAT_IDLE_AFTER) return null
  return { chat: newest, idleMinutes: Math.floor(quiet / MINUTE) }
}

// A cron run opens no PR until bossd finalizes it, and nothing the daemon dump reads records how
// the run ended: `boss show --json` and `boss chats --json` carry no run outcome, only that the
// chat is IDLE. bossd counts an unattended run as over once its agent log has been quiet for 15
// minutes with no dialog or spinner on its pane, then finalizes it: it opens the PR, or deletes a
// run that changed nothing (a build that held its ticket, found no work, or ended NO_CHANGE). So a
// cron session with no PR whose newest chat ended its turn is bossd's to finalize, not stuck; one
// still listed this long after its turn ended has a finalize that did not take.
export const CRON_FINALIZE_WAIT = 60 * MINUTE
// bossd names every fire's branch cron-<job>-<unix>; a zero-output run has no branch, so only the
// cron link (`boss cron ls`'s last_run_session_id) names it.
const CRON_BRANCH = /^cron-/

/**
 * Whether the session is a cron run: its job's latest run, or on a cron fire's branch.
 * @param {{ branch?: string }} session
 * @param {{ id: string } | null} [cronJob]
 */
export function isCronRun(session, cronJob = null) {
  return Boolean(cronJob) || CRON_BRANCH.test(session.branch ?? '')
}

// Dependabot PRs never get a boss/verify status by design: a dependabot sweep and auto-merge own
// them.
const DEPENDABOT_BRANCH = /^dependabot\//

/**
 * Whether the PR is dependabot's, by its head branch (the session's branch when unread).
 * @param {{ head_branch?: string } | null} pr
 * @param {{ branch?: string }} [session]
 */
export function isDependabotPr(pr, session = {}) {
  return DEPENDABOT_BRANCH.test(pr?.head_branch || session.branch || '')
}

/**
 * How long ago a no-PR cron run's newest chat ended its turn, or null when the run is not
 * finished: a chat is live or has armed a callback, the newest is not IDLE, or its quiet time is
 * unknown.
 */
export function cronTurnEndedFor(chats, now) {
  if (chats.some(isLive) || chats.some((chat) => chat.waiting_reason)) return null
  const newest = newestChat(chats)
  return newest?.status === 'IDLE' ? chatQuietFor(newest, now) : null
}
