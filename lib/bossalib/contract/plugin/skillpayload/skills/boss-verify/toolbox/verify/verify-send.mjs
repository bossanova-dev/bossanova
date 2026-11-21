import { CLAIM_RELEASE_TIMEOUT_MS } from './verify-stale-claim.mjs'

// Must exceed freshPaneSendBudget in services/bossd/internal/adapter/tmux/tmux_fresh_send.go.
export const FRESH_CHAT_SEND_TIMEOUT_MS = 25_000

export const freshChatSendTimeout = (remaining) =>
  Math.max(0, Math.min(FRESH_CHAT_SEND_TIMEOUT_MS, remaining - CLAIM_RELEASE_TIMEOUT_MS))

const text = (value) => typeof value === 'string' && value.trim() !== ''

// `boss chat send` exits 0 whether the daemon confirmed the send, queued it behind a running turn,
// or could not confirm it at all, so the route reads `--json`'s delivery state. A queued send is
// held by the agent and runs when its turn ends; anything else leaves the claim to go stale.
export const CONFIRMED_DELIVERY_STATES = new Set([
  'DELIVERY_STATE_SUBMITTED',
  'DELIVERY_STATE_QUEUED',
])

export function sendVerify(run, boss, chatId, message, timeoutMs) {
  if (timeoutMs <= 0) return { ok: false, why: 'DELIVERY_NOT_SUBMITTED' }
  const sent = run(boss, ['chat', 'send', chatId, message, '--json'], { json: true, timeoutMs })
  if (!sent.ok) return { ok: false, why: sent.error }
  const delivery = sent.payload?.delivery
  if (delivery?.delivered === true && CONFIRMED_DELIVERY_STATES.has(delivery.delivery_state))
    return { ok: true }
  return {
    ok: false,
    why: text(delivery?.delivery_state) ? delivery.delivery_state : 'unreadable-delivery',
  }
}
