// Best-effort reader for a session's chat transcript when a PR has no linked tracker ticket.
// Uses the host daemon's ListChats and GetChatTranscript RPCs to extract user prompts so verify
// and proof-author can still judge whether the PR satisfies what was asked of the agent.
import { readSessionRow } from './verify-admission.mjs'
import { daemonSocket, daemonUnary } from './verify-daemon-rpc.mjs'

export const MAX_CHAT_TRANSCRIPT_CHARS = 8_000
export const MAX_CHAT_TRANSCRIPT_CHATS = 3
export const MAX_CHAT_TRANSCRIPT_MESSAGES = 100

// Chat roles that belong to automated verification/repair rather than the user's request.
const AUTOMATION_ROLES = new Set(['CHAT_ROLE_VERIFY', 'CHAT_ROLE_REPAIR', 2, 3])
// Slash-command-only turns that invoke automation rather than describing desired behaviour.
const AUTOMATION_COMMAND = /^\/boss-(?:verify|review|finalize|repair|release)(?:\s|$)/i

const text = (value) => typeof value === 'string' && value.trim() !== ''

/**
 * Select the chats most likely to contain what was asked of the agent, oldest first so the
 * initial prompt leads. Falls back to all chats if every chat is tagged with an automation role.
 * @param {Array<Record<string, any>>} chats
 */
export function selectTranscriptChats(chats) {
  if (!Array.isArray(chats) || chats.length === 0) return []
  const withId = chats.filter((chat) => text(chat?.agentSessionId))
  const nonAuto = withId.filter((chat) => !AUTOMATION_ROLES.has(chat.role))
  const chosen = nonAuto.length > 0 ? nonAuto : withId
  return [...chosen]
    .sort((a, b) => String(a.createdAt ?? '').localeCompare(String(b.createdAt ?? '')))
    .slice(0, MAX_CHAT_TRANSCRIPT_CHATS)
}

/**
 * Extract the user's prompts from a GetChatTranscript response, skipping empty turns and
 * automation-only slash commands.
 * @param {Array<Record<string, any>>} messages
 */
export function extractUserPrompts(messages) {
  if (!Array.isArray(messages)) return []
  const out = []
  for (const msg of messages) {
    if (msg?.role !== 'user' || !text(msg?.text)) continue
    const trimmed = msg.text.trim()
    if (AUTOMATION_COMMAND.test(trimmed)) continue
    out.push({
      role: 'user',
      text: trimmed,
      ...(text(msg.timestamp) ? { timestamp: msg.timestamp } : {}),
    })
  }
  return out
}

/**
 * Format extracted chat prompts into one bounded plain-text block for intent derivation and the
 * proof-author bundle.
 * @param {Array<{chatId: string, title?: string, messages: Array<{role: string, text: string, timestamp?: string}>}>} chats
 * @param {number} [maxChars]
 */
export function formatChatTranscript(chats, maxChars = MAX_CHAT_TRANSCRIPT_CHARS) {
  const sections = []
  for (const chat of chats) {
    const header = text(chat.title) ? `Chat: ${chat.title.trim()}` : `Chat: ${chat.chatId}`
    const lines = chat.messages.map((m) => m.text)
    if (lines.length > 0) sections.push(`${header}\n\n${lines.join('\n\n')}`)
  }
  const joined = sections.join('\n\n---\n\n')
  if (joined.length <= maxChars) return { text: joined, truncated: false }
  return { text: joined.slice(0, maxChars), truncated: true }
}

/**
 * Read the owning session's user prompts for `prUrl`. Best-effort: never throws, and returns
 * `{read: false, why}` when no session exists, the daemon is unreachable, or no user prompts
 * were recorded.
 * @param {{prUrl?: string, read?: Function, env?: NodeJS.ProcessEnv, callDaemon?: typeof daemonUnary, timeoutMs?: number}} [options]
 */
export async function readSessionChatTranscript({
  prUrl,
  read,
  env = process.env,
  callDaemon = daemonUnary,
  timeoutMs = 5_000,
} = {}) {
  if (!text(prUrl) || typeof read !== 'function') return { read: false, why: 'no-session' }
  let session
  try {
    session = readSessionRow(read, prUrl)
  } catch {
    return { read: false, why: 'sessions-unreadable' }
  }
  if (!session?.readable) return { read: false, why: 'sessions-unreadable' }
  const sessionId = session.row?.id
  if (!text(sessionId)) return { read: false, why: 'no-session' }

  const socket = daemonSocket(env)
  let listRes
  try {
    listRes = await callDaemon(socket, 'ListChats', { sessionId }, { timeoutMs })
  } catch {
    return { read: false, why: 'daemon-unreadable' }
  }
  const selected = selectTranscriptChats(listRes?.chats)
  if (selected.length === 0) return { read: false, why: 'no-chats' }

  const chats = []
  for (const chat of selected) {
    let res
    try {
      res = await callDaemon(
        socket,
        'GetChatTranscript',
        {
          agentSessionId: chat.agentSessionId,
          sessionId,
          maxMessages: MAX_CHAT_TRANSCRIPT_MESSAGES,
          shouldIncludeActivity: false,
        },
        { timeoutMs },
      )
    } catch {
      continue
    }
    if (!res?.exists) continue
    const messages = extractUserPrompts(res.messages)
    if (messages.length > 0) {
      chats.push({
        chatId: chat.agentSessionId,
        ...(text(chat.title) ? { title: chat.title.trim() } : {}),
        messages,
      })
    }
  }
  if (chats.length === 0) return { read: false, why: 'empty-transcript' }
  const formatted = formatChatTranscript(chats)
  return {
    read: true,
    sessionId,
    chats,
    text: formatted.text,
    truncated: formatted.truncated,
  }
}
