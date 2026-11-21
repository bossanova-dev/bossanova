// A PR verify parked as a partial build is never merged as a part: the orchestrate snapshot sends
// its session back to finish the build, once, and lists it for the operator only when that finish
// chat ends with the park still standing.

import { chatQuietFor, isLive, newestChat } from './orchestrate-signals.mjs'

const MINUTE = 60 * 1000
export const PARTIAL_BUILD = 'partial-build'
// How long after a finish chat's last sign of life the park may still stand before the chat counts
// as ended: a fresh head takes a while to reach verify.
const FINISH_GRACE = 15 * MINUTE

// The title of the chat a `finish-build` row's `next` opens, by which a later round finds it.
export const FINISH_CHAT_TITLE = 'finish-build: orchestrator'

/**
 * The verdict for a PR verify parked as a partial build. With no finish chat yet, `finish-build`
 * sends one. A finish chat is sent once: while a chat is live or the finish chat was recently
 * active the row is `working` or `grace`, and once it has ended with the park still standing (it
 * reported BLOCKED, or stopped short) the row is the operator's.
 * @param {{ session: any, chats: any[], why: string, now: number }} input
 */
export function finishBuild({ session, chats, why, now }) {
  const live = chats.filter(isLive)
  if (live.length)
    return { action: 'working', why: `${why}; finishing, ${live.length} live chat(s)` }
  const finish = newestChat(chats.filter((chat) => chat.title === FINISH_CHAT_TITLE))
  if (!finish) return { action: 'finish-build', why }
  const quiet = chatQuietFor(finish, now) ?? now - Date.parse(finish.created_at ?? '')
  const id = finish.agent_session_id
  if (quiet < FINISH_GRACE) {
    const minutes = Math.ceil((FINISH_GRACE - quiet) / MINUTE)
    return { action: 'grace', why: `${why}; finish chat ${id} sent, waiting ${minutes}m more` }
  }
  const report =
    session.state === 'BLOCKED' && session.blocked_reason
      ? `it reported ${session.blocked_reason}`
      : 'read its report'
  return {
    action: 'awaiting-operator',
    why: `${why}; finish chat ${id} ended with the park standing (${report})`,
  }
}

const TICKET_ID = /\b[A-Z][A-Z0-9]*-\d+\b/

/**
 * The message a finish chat is sent: the build skill for the session's ticket (its tracker link,
 * else an id in its title) and what finishing takes. With no ticket to name it never sends a bare
 * invocation, which would claim the next ticket.
 * @param {{ session: any, pr?: number, why: string, skill: (name: string) => string }} input
 */
export function finishMessage({ session, pr, why, skill }) {
  const ticket = session.tracker_id || TICKET_ID.exec(session.title ?? '')?.[0] || ''
  const invoke = ticket
    ? `${skill('boss-build')} ${ticket}`
    : `Resume the build of this session's ticket (${skill('boss-build')} <ticket>).`
  return `${invoke} Operator: verify parked PR #${pr ?? '?'} as a partial build (${why}). A partial PR is not merged: finish it. Complete every remaining acceptance criterion on this same PR and branch. Every review, repair, retry and time cap is waived. You are authorised to fix every error in your way, inherited and environment ones included, each in its own commit. Then complete the hand-off: lease-push, the boss/build receipt, Ready for Review, verify armed. Only if finishing is genuinely impossible for you, stop and report BLOCKED with the exact reason, so the operator is alerted.`
}
