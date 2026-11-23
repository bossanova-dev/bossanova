import assert from 'node:assert/strict'
import test from 'node:test'
import {
  extractUserPrompts,
  formatChatTranscript,
  readSessionChatTranscript,
  selectTranscriptChats,
} from './verify-chat-transcript.mjs'

test('selectTranscriptChats prefers non-automation chats oldest first', () => {
  const chats = [
    { agentSessionId: 'c-verify', role: 'CHAT_ROLE_VERIFY', createdAt: '2026-01-01T00:00:00Z' },
    { agentSessionId: 'c-late', role: 'CHAT_ROLE_OPERATOR', createdAt: '2026-01-01T02:00:00Z' },
    { agentSessionId: 'c-first', role: 'CHAT_ROLE_BUILD', createdAt: '2026-01-01T01:00:00Z' },
    { agentSessionId: 'c-repair', role: 'CHAT_ROLE_REPAIR', createdAt: '2026-01-01T01:30:00Z' },
  ]
  assert.deepEqual(
    selectTranscriptChats(chats).map((c) => c.agentSessionId),
    ['c-first', 'c-late'],
  )
})

test('extractUserPrompts keeps user messages and skips automation slash commands', () => {
  const prompts = extractUserPrompts([
    {
      role: 'user',
      text: '  Add permission prompt support to chat  ',
      timestamp: '2026-01-01T00:00:00Z',
    },
    { role: 'assistant', text: 'Sure, working on it.' },
    { role: 'user', text: '/boss-verify 4624' },
    { role: 'user', text: '   ' },
    { role: 'user', text: 'Also disable free-text reply when a permission prompt is docked.' },
  ])
  assert.deepEqual(prompts, [
    {
      role: 'user',
      text: 'Add permission prompt support to chat',
      timestamp: '2026-01-01T00:00:00Z',
    },
    { role: 'user', text: 'Also disable free-text reply when a permission prompt is docked.' },
  ])
})

test('formatChatTranscript caps long text and marks truncated', () => {
  const out = formatChatTranscript(
    [{ chatId: 'c1', title: 'Main', messages: [{ role: 'user', text: 'a'.repeat(100) }] }],
    40,
  )
  assert.equal(out.truncated, true)
  assert.equal(out.text.length, 40)
})

test('readSessionChatTranscript reads user prompts from the PR session via daemon RPCs', async () => {
  const prUrl = 'https://github.com/org/repo/pull/42'
  const read = (cmd, args) => {
    assert.equal(cmd, 'boss')
    assert.deepEqual(args, ['ls', '--json'])
    return { ok: true, payload: { sessions: [{ id: 'sess-42', pr_url: prUrl }] } }
  }
  const calls = []
  const callDaemon = async (_socket, method, msg) => {
    calls.push({ method, msg })
    if (method === 'ListChats') {
      return {
        chats: [
          {
            agentSessionId: 'chat-verify',
            role: 'CHAT_ROLE_VERIFY',
            title: 'Verify',
            createdAt: '2026-01-01T00:00:00Z',
          },
          {
            agentSessionId: 'chat-main',
            role: 'CHAT_ROLE_OPERATOR',
            title: 'Permission prompts',
            createdAt: '2026-01-01T00:01:00Z',
          },
        ],
      }
    }
    if (method === 'GetChatTranscript') {
      return {
        exists: true,
        messages: [
          { role: 'user', text: 'Allow answering permission prompts from the chat UI.' },
          { role: 'assistant', text: 'Done.' },
          { role: 'user', text: '/boss-verify 42' },
        ],
      }
    }
    throw new Error(`unexpected method ${method}`)
  }

  const res = await readSessionChatTranscript({ prUrl, read, callDaemon })
  assert.equal(res.read, true)
  assert.equal(res.sessionId, 'sess-42')
  assert.equal(res.chats.length, 1)
  assert.equal(res.chats[0].chatId, 'chat-main')
  assert.match(res.text, /Permission prompts/)
  assert.match(res.text, /Allow answering permission prompts from the chat UI\./)
  assert.doesNotMatch(res.text, /\/boss-verify/)
  assert.deepEqual(
    calls.map((c) => c.method),
    ['ListChats', 'GetChatTranscript'],
  )
})

test('readSessionChatTranscript returns read:false cleanly when no session or daemon is unavailable', async () => {
  const noSession = await readSessionChatTranscript({
    prUrl: 'https://github.com/org/repo/pull/99',
    read: () => ({ ok: true, payload: { sessions: [] } }),
  })
  assert.deepEqual(noSession, { read: false, why: 'no-session' })

  const daemonDown = await readSessionChatTranscript({
    prUrl: 'https://github.com/org/repo/pull/99',
    read: () => ({
      ok: true,
      payload: { sessions: [{ id: 'sess-99', pr_url: 'https://github.com/org/repo/pull/99' }] },
    }),
    callDaemon: async () => {
      throw new Error('ENOENT')
    },
  })
  assert.deepEqual(daemonDown, { read: false, why: 'daemon-unreadable' })
})
