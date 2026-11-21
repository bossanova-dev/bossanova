import { test } from 'node:test'
import assert from 'node:assert/strict'
import { routeCandidate, CALL_TIMEOUT_MS } from './verify-route.mjs'

for (const [existing, remaining, expected] of [
  [false, 45000, 25000],
  [false, 18000, 8000],
  [true, 45000, CALL_TIMEOUT_MS],
  [false, 10000, null],
]) {
  test(`fresh send timeout preserves the release reserve (${existing}, ${remaining})`, () => {
    const calls = []
    const url = 'https://github.com/o/r/pull/1'
    const routed = routeCandidate(
      { pr: 1, headSha: 'a'.repeat(40), url, token: 'token' },
      {
        boss: 'boss',
        sessions: [{ id: 's', pr_url: url }],
        dryRun: false,
        timeout: () => CALL_TIMEOUT_MS,
        remaining: () => remaining,
        run: (_cmd, args, options) => {
          calls.push({ args, options })
          if (args[0] === 'chats')
            return {
              ok: true,
              payload: {
                chats: existing ? [{ role: 'verify', agent_session_id: 'c', status: 'IDLE' }] : [],
              },
            }
          if (args[0] === 'chat' && args[1] === 'new')
            return { ok: true, payload: { chat: { agent_session_id: 'c' } } }
          if (args[0] === 'chat' && args[1] === 'send')
            return {
              ok: true,
              payload: {
                delivery: { delivered: true, delivery_state: 'DELIVERY_STATE_SUBMITTED' },
              },
            }
          return { ok: true, payload: [] }
        },
      },
    )
    const send = calls.find((c) => c.args[0] === 'chat' && c.args[1] === 'send')
    if (expected === null) {
      assert.equal(send, undefined)
      assert.equal(routed.why, 'DELIVERY_NOT_SUBMITTED')
    } else assert.equal(send?.options.timeoutMs, expected)
  })
}
