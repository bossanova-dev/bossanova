// bossd question-signal hook for opencode (BOS-486).
//
// This is a STATIC asset embedded into bossd-plugin-opencode (questionhook.go).
// The plugin drops it at <worktree>/.opencode/plugins/bossd-question.js just
// before `opencode run` starts, and removes it when the run ends (and again via
// the RemoveAgentRunHook RPC). opencode auto-loads every local .js/.ts under
// `.opencode/plugins/` (PLURAL) at startup, including in headless `opencode
// run` — verified against opencode 1.18.4; see
// docs/solutions/logic-errors/spike-opencode-question-signal-events-unreachable.md
//
// It carries NO secrets. The loopback port and the per-run bearer token are read
// from `process.env` at RUNTIME: bossd puts BOSS_HOOK_PORT / BOSS_HOOK_TOKEN in
// the agent process's env overlay and opencode's Bun runtime exposes them on
// `process.env`. Keeping them out of the file means the token never lands in the
// worktree and the asset is byte-identical for every run, so (re-)writing it is
// trivially idempotent.
//
// opencode instantiates a plugin MORE THAN ONCE per run (two PLUGIN_LOADED lines
// were observed in the spike), so every handler here must be side-effect-safe
// under duplicate delivery. Posting the same signal twice is harmless: bossd's
// question store is last-write-wins per agent_session_id.

// Hard cap on one hook POST. `opencode run` can exit before async event handlers
// finish (upstream anomalyco/opencode#23380), so the fetch is awaited — never
// fire-and-forget — but is not allowed to hold the run open indefinitely.
const HOOK_TIMEOUT_MS = 2000

export const BossdQuestion = async () => {
  // The plugin context ({ project, client, $, directory, worktree }) is
  // deliberately unused: this hook needs only process.env and the event payload.
  const port = process.env.BOSS_HOOK_PORT
  const token = process.env.BOSS_HOOK_TOKEN
  // Without the loopback context the hook is inert and opencode runs normally.
  const enabled = Boolean(port && token)

  const post = async (sessionID, body) => {
    if (!enabled || !sessionID) return
    try {
      await fetch(`http://127.0.0.1:${port}/hooks/question/${encodeURIComponent(sessionID)}`, {
        method: 'POST',
        headers: {
          'content-type': 'application/json',
          authorization: `Bearer ${token}`,
        },
        body: JSON.stringify(body),
        signal: AbortSignal.timeout(HOOK_TIMEOUT_MS),
      })
    } catch {
      // Best effort only. A hook failure (daemon gone, port closed, timeout)
      // must never break the agent run; bossd reads a missing signal as
      // "no question pending".
    }
  }

  return {
    event: async ({ event }) => {
      const properties = event?.properties ?? {}
      // For session-shaped events the session object rides at
      // `properties.info` (with `.id` and `.parentID`). Only the `session.*`
      // shape was observed live in the spike; `permission.asked`'s payload
      // shape is UNVERIFIED, so parentID is read from both the nested and the
      // top-level position rather than assuming the session shape.
      const info = properties.info ?? {}
      // Sub-agent suppression: only a MAIN session can hold a question for the
      // human. A session with a non-empty parentID is a child/sub-agent session.
      if (info.parentID || properties.parentID) return
      // `properties.sessionID` is opencode's own `ses_…` id, which is exactly
      // the agent_session_id bossd keys its question store by.
      const sessionID = properties.sessionID || info.id
      if (!sessionID) return

      switch (event.type) {
        // permission.asked is opencode's real, documented needs-a-human event.
        case 'permission.asked':
        // question.asked does NOT exist in opencode 1.18.4 — it fired zero times
        // across two live headless runs and is absent from the documented event
        // list (see the spike doc above). It is wired anyway, deliberately, for
        // forward-compatibility: this is a plain switch on `event.type`, NOT an
        // allow-list, so naming an unknown kind costs nothing and suppresses
        // nothing. If a future opencode ships it, this hook picks it up for free.
        case 'question.asked':
          await post(sessionID, {
            notification_type: 'permission_prompt',
            message: `opencode is waiting for a human response (${event.type})`,
          })
          break
        // session.idle means the agent's turn is over — clear any pending signal.
        case 'session.idle':
          await post(sessionID, {
            notification_type: 'session_idle',
            cleared: true,
          })
          break
        default:
          break
      }
    },
  }
}
