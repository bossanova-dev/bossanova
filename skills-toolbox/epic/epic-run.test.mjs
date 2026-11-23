import assert from 'node:assert/strict'
import test from 'node:test'
import { createEpicState, normalizeEpicState, recordLaunch } from './epic-driver.mjs'
import {
  checkVerdictFromDisplayStatus,
  createEpicIo,
  epicTriggerConditions,
  normalizeCallbackRow,
  pullFromRest,
} from './epic-io.mjs'
import {
  main,
  readStateFile,
  reconcileProviderOutcomes,
  runInit,
  runReconcile,
  writeStateFile,
} from './epic-run.mjs'
import { mkdtempSync, rmSync } from 'node:fs'
import os from 'node:os'
import path from 'node:path'

const NOW = '2026-01-01T12:00:00.000Z'
const ticket = (id, blockedBy = []) => ({
  id,
  title: `Work ${id}`,
  url: `https://tracker.example/${id}`,
  priority: 2,
  createdAt: '2026-01-01T00:00:00Z',
  stateName: 'Planned',
  labels: ['agent-build'],
  blockedBy,
})

const mcpAdapter = {
  operationMap: Object.fromEntries(
    ['readComments', 'writeComment', 'updateComment', 'moveState'].map((key) => [
      key,
      { transport: 'mcp', tool: `mcp__tracker__${key}` },
    ]),
  ),
}

/** A fake `boss`/`gh` that answers from a script and records every call. */
function fakeExec(handlers) {
  const calls = []
  const exec = (cmd, args, options = {}) => {
    calls.push({ cmd, args, input: options.input })
    const key = cmd === 'gh' ? `gh ${args[0]} ${args[1]}` : `${args[0]} ${args[1] ?? ''}`.trim()
    const handler = handlers[key] ?? handlers[`${cmd === 'gh' ? 'gh ' : ''}${args[0]}`]
    if (!handler) return { status: 1, stdout: '', stderr: `unhandled ${cmd} ${args.join(' ')}` }
    const out = handler(args, options)
    return typeof out === 'string' ? { status: 0, stdout: out, stderr: '' } : out
  }
  return { exec, calls }
}

const baseState = (tickets, extra = {}) =>
  normalizeEpicState({
    ...createEpicState({
      runId: 'run-1',
      epicId: 'ROOT',
      repoId: 'repo-1',
      agent: 'claude',
      parallel: 2,
      plannedState: 'Planned',
      reviewState: 'In Review',
      childWallClockMinutes: 360,
      startedAt: NOW,
      tickets,
    }),
    phase: 'polling',
    ...extra,
  })

function ioFor(state, handlers, extra = {}) {
  const fake = fakeExec(handlers)
  const built = createEpicIo({
    state,
    repoId: 'repo-1',
    prRepo: 'owner/repo',
    driverChatId: 'driver-chat',
    trackerAdapter: mcpAdapter,
    upsertProgressImpl: async () => ({ results: [] }),
    exec: fake.exec,
    now: NOW,
    sleep: () => {},
    ...extra,
  })
  return { ...built, calls: fake.calls }
}

test('createSession launches the implement command unattended and waits for a late chat id', () => {
  const state = baseState([ticket('A')])
  let chatReads = 0
  const { io, calls } = ioFor(
    state,
    {
      new: () => JSON.stringify({ session: { id: 'sess-A' } }),
      chats: () => {
        chatReads += 1
        return JSON.stringify({
          chats: chatReads < 2 ? [] : [{ agent_session_id: 'chat-A', created_at: NOW }],
        })
      },
    },
    {
      model: 'opus[1m]',
      promptSuffix: ' --team Core',
      trackerSource: 'linear',
      ticketUrls: { A: 'https://tracker.example/A' },
    },
  )
  assert.deepEqual(io.createSession({ ticket: state.tickets[0] }), {
    sessionId: 'sess-A',
    chatId: 'chat-A',
  })
  const args = calls[0].args
  const flag = (name) => args[args.indexOf(name) + 1]
  assert.equal(flag('--prompt'), '/boss-build A --team Core')
  assert.equal(flag('--title'), '[A] Work A')
  assert.equal(flag('--model'), 'opus[1m]')
  assert.equal(flag('--tracker-id'), 'A')
  assert.equal(flag('--tracker-source'), 'linear')
  assert.equal(flag('--tracker-url'), 'https://tracker.example/A')
  assert.ok(args.includes('--tmux-unattended'))
  assert.equal(args.includes('--base-branch'), false)
})

test('a delivered callback reads as absent so the cycle re-arms it; leased stays coverage', () => {
  assert.equal(normalizeCallbackRow({ state: 'delivered' }).state, 'fired')
  assert.equal(normalizeCallbackRow({ state: 'leased' }).state, 'active')
  const { io } = ioFor(baseState([ticket('A')]), {
    callback: () =>
      JSON.stringify([
        { id: 'c1', trigger: 'merged', state: 'active' },
        { id: 'c2', trigger: 'closed', state: 'delivered' },
      ]),
  })
  assert.deepEqual(
    io.listWatches({ prNumber: 7 }).map((row) => row.trigger),
    ['merged'],
  )
})

test('arming before the child has a PR is a failure the cycle must cover with a fallback', () => {
  const { io, calls } = ioFor(baseState([ticket('A')]), {})
  assert.deepEqual(io.armWatches({ prNumber: null, triggers: ['merged'], message: 'm' }), {
    callbacks: [],
    satisfied: [],
    error: 'child has no PR yet',
  })
  assert.equal(calls.length, 0)
  assert.equal(io.scheduleFallbackWake({ ticketId: 'A', reason: 'x' }), null)
})

test('only a passing display status admits green checks', () => {
  assert.equal(checkVerdictFromDisplayStatus('passing').state, 'passing')
  assert.equal(checkVerdictFromDisplayStatus('Failing').state, 'failing')
  assert.equal(checkVerdictFromDisplayStatus('conflict').state, 'failing')
  assert.equal(checkVerdictFromDisplayStatus('draft').state, 'pending')
  assert.equal(checkVerdictFromDisplayStatus(undefined).state, 'pending')
})

test('the snapshot reads the tracked chat, the PR text and the wall clock', () => {
  const launched = recordLaunch(baseState([ticket('A')]), {
    ticketId: 'A',
    sessionId: 'sess-A',
    chatId: 'chat-A',
    at: '2026-01-01T00:00:00.000Z',
  })
  const { io } = ioFor(launched, {
    show: () =>
      JSON.stringify({
        session: { state: 'READY_FOR_REVIEW', display_status: 'passing', pr_number: 7 },
      }),
    chats: () =>
      JSON.stringify({
        chats: [
          { agent_session_id: 'other', status: 'WORKING', spinner_present: true },
          { agent_session_id: 'chat-A', status: 'IDLE', spinner_present: false },
        ],
      }),
    'gh api': (args) => {
      assert.equal(args[1], 'repos/owner/repo/pulls/7')
      return JSON.stringify({
        number: 7,
        html_url: 'u',
        draft: false,
        title: 't',
        body: 'b',
        state: 'open',
      })
    },
  })
  const snapshot = io.readSessionSnapshot({ ticketId: 'A', sessionId: 'sess-A', chatId: 'chat-A' })
  assert.equal(snapshot.chatStatus, 'IDLE')
  assert.equal(snapshot.chatStatusReadable, true)
  assert.equal(snapshot.spinnerPresent, false)
  assert.equal(snapshot.checkVerdict.state, 'passing')
  assert.equal(snapshot.prView.isDraft, false)
  assert.equal(snapshot.prView.repo, 'owner/repo')
  assert.equal(snapshot.wallClockExceeded, true)
})

test('an MCP-transport tracker write is deferred to the host, never dropped', () => {
  const { io, deferred } = ioFor(baseState([ticket('A')]), {})
  assert.equal(io.moveTicketDone({ ticketId: 'A' }), true)
  assert.deepEqual(deferred, [
    {
      operation: 'moveState',
      tool: 'mcp__tracker__moveState',
      args: { id: 'A', state: 'completed' },
      outcome: 'descriptor-emitted',
    },
  ])
})

test('a PR merged by another actor is folded in; one closed unmerged fail-isolates its cascade', () => {
  let s = baseState([ticket('A'), ticket('B'), ticket('C', ['B'])])
  s = recordLaunch(s, { ticketId: 'A', sessionId: 's-A', chatId: 'c-A', prNumber: 1 })
  s = recordLaunch(s, { ticketId: 'B', sessionId: 's-B', chatId: 'c-B', prNumber: 2 })
  const moved = []
  const { state, actions } = reconcileProviderOutcomes(s, {
    prState: (number) => (number === 1 ? 'MERGED' : 'CLOSED'),
    moveTicketDone: ({ ticketId }) => moved.push(ticketId) > 0,
  })
  assert.deepEqual(state.merged, ['A'])
  assert.deepEqual(state.failed, ['B'])
  assert.deepEqual(state.cascadeSkipped, ['C'])
  assert.deepEqual(state.inFlight, [])
  assert.deepEqual(moved, ['A'])
  assert.deepEqual(actions, ['external-merge:A', 'external-close:B'])
})

test('reconcile refuses to run a cycle without every external blocker state', async () => {
  const state = baseState([ticket('A', ['OUTSIDE'])])
  const result = await runReconcile(
    { 'state-file': 'f', 'repo-id': 'r', 'pr-repo': 'o/r', 'driver-chat': 'c' },
    { readState: () => state, config: {}, env: {}, adapter: mcpAdapter },
  )
  assert.equal(result.outcome, 'blocker-states-required')
  assert.deepEqual(result.required, ['OUTSIDE'])
})

test('one reconcile cycle launches ready work, subscribes to it, and stays RUNNING on a fallback', async () => {
  const state = baseState([ticket('A'), ticket('B', ['A'])])
  const writes = []
  const subscriptions = []
  const { exec, calls } = fakeExec({
    ls: () => JSON.stringify({ sessions: [] }),
    new: () => JSON.stringify({ session: { id: 'sess-A', chat_id: 'chat-A' } }),
    show: () =>
      JSON.stringify({ session: { state: 'IMPLEMENTING_PLAN', display_status: 'draft' } }),
    chats: () =>
      JSON.stringify({
        chats: [{ agent_session_id: 'chat-A', status: 'WORKING', spinner_present: true }],
      }),
    'broadcast subscribe': (_args, { input }) => {
      const row = {
        id: 'sub-1',
        owner_session_id: 'sess-A',
        origin_chat_id: 'driver-chat',
        trigger_event: 'settled',
        state: 'active',
      }
      subscriptions.push({ ...row, message: input })
      return JSON.stringify(row)
    },
    'broadcast subscriptions': () => JSON.stringify(subscriptions),
  })
  const result = await runReconcile(
    {
      'state-file': 'state.json',
      'repo-id': 'repo-1',
      'pr-repo': 'owner/repo',
      'driver-chat': 'driver-chat',
      wake: 'initial',
      'fallback-mechanism': 'host-session-wake',
      'fallback-wake-at': '2026-01-01T12:15:00.000Z',
      now: NOW,
    },
    {
      readState: () => state,
      writeState: (_file, next) => {
        writes.push(next)
        return normalizeEpicState(next)
      },
      config: {},
      env: {},
      adapter: mcpAdapter,
      exec,
      sleep: () => {},
      upsertProgress: async () => ({
        results: [
          { issueId: 'ROOT', outcome: 'comments-required', tool: 'mcp__tracker__readComments' },
        ],
      }),
    },
  )
  assert.equal(result.status, 'RUNNING')
  assert.deepEqual(result.inFlight, ['A'])
  assert.ok(result.actions.includes('launch:A'))
  assert.ok(result.actions.includes('subscribe:A:verified'))
  assert.ok(result.actions.includes('fallback:A:host-session-wake'))
  assert.equal(calls.filter((call) => call.args[0] === 'new').length, 1)
  assert.match(subscriptions[0].message, /ROOT/)
  assert.deepEqual(
    result.deferred.map((row) => row.operation),
    ['progress'],
  )
  const saved = writes.at(-1)
  assert.equal(saved.sessions.A.sessionId, 'sess-A')
  assert.equal(saved.watches.A.subscription.id, 'sub-1')
})

test('a second cycle adopts the live child instead of launching a duplicate', async () => {
  const state = baseState([ticket('A')])
  const { exec, calls } = fakeExec({
    ls: () =>
      JSON.stringify({
        sessions: [
          { id: 'sess-A', title: '[A] Work A', tracker_id: 'A', state: 'IMPLEMENTING_PLAN' },
        ],
      }),
    chats: () => JSON.stringify({ chats: [{ agent_session_id: 'chat-A', status: 'WORKING' }] }),
    show: () =>
      JSON.stringify({ session: { state: 'IMPLEMENTING_PLAN', display_status: 'draft' } }),
    'broadcast subscribe': () =>
      JSON.stringify({ id: 'sub-1', owner_session_id: 'sess-A', state: 'active' }),
    'broadcast subscriptions': () =>
      JSON.stringify([{ id: 'sub-1', owner_session_id: 'sess-A', state: 'active' }]),
  })
  const result = await runReconcile(
    {
      'state-file': 'f',
      'repo-id': 'repo-1',
      'pr-repo': 'owner/repo',
      'driver-chat': 'driver-chat',
      now: NOW,
    },
    {
      readState: () => state,
      writeState: (_file, next) => normalizeEpicState(next),
      config: {},
      env: {},
      adapter: mcpAdapter,
      exec,
      upsertProgress: async () => ({ results: [] }),
    },
  )
  assert.ok(result.actions.includes('adopt:A'))
  assert.equal(calls.filter((call) => call.args[0] === 'new').length, 0)
  assert.deepEqual(result.inFlight, ['A'])
})

test('a REST pull reads MERGED only when it merged; a closed unmerged one reads CLOSED', () => {
  assert.equal(pullFromRest({ state: 'closed', merged: true }).state, 'MERGED')
  assert.equal(pullFromRest({ state: 'closed', merged_at: '2026-01-01T00:00:00Z' }).state, 'MERGED')
  assert.equal(pullFromRest({ state: 'closed', merged: false }).state, 'CLOSED')
  assert.equal(pullFromRest({ state: 'open', draft: true }).isDraft, true)
  assert.equal(pullFromRest({ state: 'open', mergeable: false }).mergeable, 'CONFLICTING')
})

test('a BLOCKED child whose run concluded with a reason routes to repair, not investigate', async () => {
  const { classifyChildLiveness } = await import('./bs-epic-lib.mjs')
  const launched = recordLaunch(baseState([ticket('A')]), {
    ticketId: 'A',
    sessionId: 'sess-A',
    chatId: 'chat-A',
    at: NOW,
  })
  const read = (status, reason) =>
    ioFor(launched, {
      show: () => JSON.stringify({ session: { state: 'BLOCKED', blocked_reason: reason } }),
      chats: () => JSON.stringify({ chats: [{ agent_session_id: 'chat-A', status }] }),
    }).io.readSessionSnapshot({ ticketId: 'A', sessionId: 'sess-A', chatId: 'chat-A' })
  assert.equal(classifyChildLiveness(read('IDLE', 'run blocked: review capped')).action, 'repair')
  assert.equal(classifyChildLiveness(read('IDLE', '')).action, 'investigate')
  assert.equal(classifyChildLiveness(read('WORKING', 'run blocked')).action, 'hold')
})

test('epicTriggerConditions classifies satisfied, unknown, and known-false triggers across PR and check-run states', () => {
  const green = { readable: true, failing: false, pending: false }
  const red = { readable: true, failing: true, pending: false }
  const unread = { readable: false, failing: false, pending: false }
  const cases = [
    {
      name: 'null PR marks every epic trigger unknown',
      input: { pr: null, runs: unread },
      want: {
        satisfied: [],
        unknown: ['checks_passed_ready', 'ready_for_review', 'checks_failed', 'merged', 'closed'],
      },
    },
    {
      name: 'ready PR with green runs satisfies ready_for_review and checks_passed_ready',
      input: { pr: { state: 'OPEN', isDraft: false }, runs: green },
      want: { satisfied: ['checks_passed_ready', 'ready_for_review'], unknown: [] },
    },
    {
      name: 'ready PR with red runs satisfies ready_for_review and checks_failed',
      input: { pr: { state: 'OPEN', isDraft: false }, runs: red },
      want: { satisfied: ['ready_for_review', 'checks_failed'], unknown: [] },
    },
    {
      name: 'draft PR with unreadable runs keeps checks_passed_ready known false and checks_failed unknown',
      input: { pr: { state: 'OPEN', isDraft: true }, runs: unread },
      want: { satisfied: [], unknown: ['checks_failed'] },
    },
    {
      name: 'ready PR with unreadable runs marks both check triggers unknown',
      input: { pr: { state: 'OPEN', isDraft: false }, runs: unread },
      want: {
        satisfied: ['ready_for_review'],
        unknown: ['checks_passed_ready', 'checks_failed'],
      },
    },
    {
      name: 'merged PR satisfies only merged',
      input: { pr: { state: 'MERGED', isDraft: false }, runs: green },
      want: { satisfied: ['merged'], unknown: [] },
    },
    {
      name: 'merged PR with unreadable runs has nothing unknown',
      input: { pr: { state: 'MERGED', isDraft: false }, runs: unread },
      want: { satisfied: ['merged'], unknown: [] },
    },
  ]
  for (const tc of cases) {
    assert.deepEqual(epicTriggerConditions(tc.input), tc.want, tc.name)
  }
})

test('a trigger whose condition already holds is returned as satisfied without error, never armed', () => {
  const launched = recordLaunch(baseState([ticket('A')]), {
    ticketId: 'A',
    sessionId: 'sess-A',
    chatId: 'chat-A',
    prNumber: 7,
  })
  const { io, calls } = ioFor(launched, {
    'gh api': (args) =>
      args[1].includes('/check-runs')
        ? JSON.stringify({ check_runs: [{ status: 'completed', conclusion: 'success' }] })
        : JSON.stringify({ number: 7, draft: false, state: 'open', head: { sha: 'abc' } }),
    callback: (args) => JSON.stringify({ id: `cb-${args[3]}`, trigger: args[3], state: 'active' }),
  })
  const armed = io.armWatches({
    ticketId: 'A',
    prNumber: 7,
    triggers: ['checks_passed_ready', 'ready_for_review', 'checks_failed', 'merged', 'closed'],
    message: 'm',
  })
  const added = calls.filter((call) => call.args[0] === 'callback').map((call) => call.args[3])
  assert.deepEqual(added, ['checks_failed', 'merged', 'closed'])
  assert.deepEqual(armed.satisfied, ['checks_passed_ready', 'ready_for_review'])
  assert.equal(armed.error, undefined)
})

test('a pending required status does not stop checks_passed_ready from reporting satisfied and staying unarmed', () => {
  const launched = recordLaunch(baseState([ticket('A')]), {
    ticketId: 'A',
    sessionId: 'sess-A',
    chatId: 'chat-A',
    prNumber: 7,
  })
  const { io, calls } = ioFor(
    launched,
    {
      'gh api': (args) => {
        if (args[1].endsWith('/status')) {
          return JSON.stringify({ statuses: [{ context: 'boss/verify', state: 'pending' }] })
        }
        if (args[1].includes('/check-runs')) {
          return JSON.stringify({ check_runs: [{ status: 'completed', conclusion: 'success' }] })
        }
        return JSON.stringify({ number: 7, draft: false, state: 'open', head: { sha: 'abc' } })
      },
      callback: (args) =>
        JSON.stringify({ id: `cb-${args[3]}`, trigger: args[3], state: 'active' }),
    },
    { requiredStatus: 'boss/verify' },
  )
  const armed = io.armWatches({
    ticketId: 'A',
    prNumber: 7,
    triggers: ['checks_passed_ready', 'ready_for_review', 'checks_failed', 'merged', 'closed'],
    message: 'm',
  })
  const added = calls.filter((call) => call.args[0] === 'callback').map((call) => call.args[3])
  assert.deepEqual(added, ['checks_failed', 'merged', 'closed'])
  assert.deepEqual(armed.satisfied, ['checks_passed_ready', 'ready_for_review'])
  assert.equal(armed.error, undefined)
})

test('armWatches with an unreadable PR arms no callbacks and reports an error naming the PR', () => {
  const launched = recordLaunch(baseState([ticket('A')]), {
    ticketId: 'A',
    sessionId: 'sess-A',
    chatId: 'chat-A',
    prNumber: 7,
  })
  const { io, calls } = ioFor(launched, {
    'gh api': () => ({ status: 1, stdout: '', stderr: 'HTTP 502 Bad Gateway' }),
    callback: (args) => JSON.stringify({ id: `cb-${args[3]}`, trigger: args[3], state: 'active' }),
  })
  const armed = io.armWatches({
    ticketId: 'A',
    prNumber: 7,
    triggers: ['checks_passed_ready', 'ready_for_review', 'checks_failed', 'merged', 'closed'],
    message: 'm',
  })
  assert.equal(calls.filter((call) => call.args[0] === 'callback').length, 0)
  assert.deepEqual(armed.callbacks, [])
  assert.deepEqual(armed.satisfied, [])
  assert.match(armed.error, /PR #7.*unreadable/i)
})

test('armWatches with unreadable check runs on a ready PR arms only merged and closed and errors on check triggers', () => {
  const launched = recordLaunch(baseState([ticket('A')]), {
    ticketId: 'A',
    sessionId: 'sess-A',
    chatId: 'chat-A',
    prNumber: 7,
  })
  const { io, calls } = ioFor(launched, {
    'gh api': (args) =>
      args[1].includes('/check-runs')
        ? { status: 1, stdout: '', stderr: 'HTTP 500 Internal Server Error' }
        : JSON.stringify({ number: 7, draft: false, state: 'open', head: { sha: 'abc' } }),
    callback: (args) => JSON.stringify({ id: `cb-${args[3]}`, trigger: args[3], state: 'active' }),
  })
  const armed = io.armWatches({
    ticketId: 'A',
    prNumber: 7,
    triggers: ['checks_passed_ready', 'ready_for_review', 'checks_failed', 'merged', 'closed'],
    message: 'm',
  })
  const added = calls.filter((call) => call.args[0] === 'callback').map((call) => call.args[3])
  assert.deepEqual(added, ['merged', 'closed'])
  assert.deepEqual(armed.satisfied, ['ready_for_review'])
  assert.match(armed.error, /checks_passed_ready/)
  assert.match(armed.error, /checks_failed/)
})

test('with a required status, passing checks wait for it to succeed on the head', () => {
  const launched = recordLaunch(baseState([ticket('A')]), {
    ticketId: 'A',
    sessionId: 'sess-A',
    chatId: 'chat-A',
  })
  const snapshotWith = (verify) =>
    ioFor(
      launched,
      {
        show: () => JSON.stringify({ session: { display_status: 'passing', pr_number: 7 } }),
        chats: () => JSON.stringify({ chats: [{ agent_session_id: 'chat-A', status: 'IDLE' }] }),
        'gh api': (args) =>
          args[1].endsWith('/status')
            ? JSON.stringify({
                statuses: verify ? [{ context: 'boss/verify', state: verify }] : [],
              })
            : JSON.stringify({ number: 7, draft: false, state: 'open', head: { sha: 'abc1234' } }),
      },
      { requiredStatus: 'boss/verify' },
    ).io.readSessionSnapshot({ ticketId: 'A', sessionId: 'sess-A', chatId: 'chat-A' }).checkVerdict
  assert.deepEqual(snapshotWith('pending'), { state: 'pending', reason: 'boss/verify:pending' })
  assert.deepEqual(snapshotWith(null), { state: 'pending', reason: 'boss/verify:missing' })
  assert.equal(snapshotWith('success').state, 'passing')
})

test('check triggers follow the head check runs, not the daemon display status', () => {
  const launched = recordLaunch(baseState([ticket('A')]), {
    ticketId: 'A',
    sessionId: 'sess-A',
    chatId: 'chat-A',
    prNumber: 7,
  })
  const armedFor = (runs) => {
    const { io, calls } = ioFor(launched, {
      'gh api': (args) =>
        args[1].includes('/check-runs')
          ? JSON.stringify({ check_runs: runs })
          : JSON.stringify({ number: 7, draft: false, state: 'open', head: { sha: 'abc' } }),
      // A conflicted PR reads `conflict` here; GitHub's check triggers never look at it.
      show: () => JSON.stringify({ session: { display_status: 'conflict' } }),
      callback: (args) =>
        JSON.stringify({ id: `cb-${args[3]}`, trigger: args[3], state: 'active' }),
    })
    io.armWatches({ prNumber: 7, triggers: ['checks_passed_ready', 'checks_failed'], message: 'm' })
    return calls.filter((call) => call.args[0] === 'callback').map((call) => call.args[3])
  }
  assert.deepEqual(armedFor([{ status: 'completed', conclusion: 'success' }]), ['checks_failed'])
  assert.deepEqual(armedFor([{ status: 'completed', conclusion: 'failure' }]), [
    'checks_passed_ready',
  ])
  assert.deepEqual(armedFor([{ status: 'in_progress', conclusion: null }]), [
    'checks_passed_ready',
    'checks_failed',
  ])
})

test('init persists a verified origin base and refuses missing or unreadable branches without state writes', async () => {
  for (const status of [0, 2, 128]) {
    const writes = []
    const calls = []
    const values = {
      'tickets-file': 'tickets',
      'planned-state': 'Planned',
      epic: 'ROOT',
      'repo-id': 'repo-1',
      'state-dir': '/state',
      'base-branch': 'integration',
      now: NOW,
    }
    const result = runInit(values, {
      config: { adapters: { tracker: 'linear' }, trackerConfig: {} },
      readJson: () => [ticket('A')],
      exec: (cmd, args) => {
        calls.push([cmd, args])
        return { status, stdout: 'abc123\trefs/heads/integration\n', stderr: '' }
      },
      writeState: (file, state) => writes.push({ file, state }),
    })
    if (status === 0) {
      assert.equal((await result).outcome, 'initialized')
      assert.equal(writes[0].state.baseBranch, 'integration')
    } else {
      await assert.rejects(
        result,
        status === 2 ? /does not exist on origin/ : /could not verify base branch/,
      )
      assert.equal(writes.length, 0)
    }
    assert.deepEqual(calls, [
      ['git', ['ls-remote', '--exit-code', '--heads', 'origin', 'refs/heads/integration']],
    ])
  }
})

test('a resumed reconcile launches every ready child against the persisted base', async (t) => {
  const dir = mkdtempSync(path.join(os.tmpdir(), 'epic-base-'))
  t.after(() => rmSync(dir, { recursive: true, force: true, maxRetries: 3 }))
  const file = path.join(dir, 'state.json')
  writeStateFile(file, baseState([ticket('A'), ticket('B')], { baseBranch: 'integration' }))
  const { exec, calls } = fakeExec({
    ls: () => JSON.stringify({ sessions: [] }),
    new: (args) =>
      JSON.stringify({
        session: { id: `sess-${args[args.indexOf('--tracker-id') + 1]}`, chat_id: 'chat' },
      }),
    show: () =>
      JSON.stringify({ session: { state: 'IMPLEMENTING_PLAN', display_status: 'draft' } }),
    chats: () => JSON.stringify({ chats: [{ agent_session_id: 'chat', status: 'WORKING' }] }),
    'broadcast subscribe': () => JSON.stringify({ id: 'sub', state: 'active' }),
    'broadcast subscriptions': () => JSON.stringify([]),
  })
  const result = await runReconcile(
    {
      'state-file': file,
      'repo-id': 'repo-1',
      'pr-repo': 'owner/repo',
      'driver-chat': 'driver-chat',
      now: NOW,
    },
    {
      config: {},
      env: {},
      adapter: mcpAdapter,
      exec,
      sleep: () => {},
      upsertProgress: async () => ({ results: [] }),
    },
  )
  assert.deepEqual(result.inFlight, ['A', 'B'])
  const launches = calls.filter((call) => call.args[0] === 'new')
  assert.equal(launches.length, 2)
  for (const { args } of launches) {
    const baseFlag = args.indexOf('--base-branch')
    assert.notEqual(baseFlag, -1)
    assert.equal(args[baseFlag + 1], 'integration')
  }
  assert.equal(readStateFile(file).baseBranch, 'integration')
})

test('reconcile refuses to switch the base, including a run originally started without one', async () => {
  for (const baseBranch of ['', 'integration']) {
    const state = baseState([ticket('A')], { baseBranch })
    const result = await main(
      [
        'reconcile',
        '--state-file',
        'f',
        '--repo-id',
        'r',
        '--pr-repo',
        'o/r',
        '--driver-chat',
        'c',
        '--base-branch',
        'other',
      ],
      {
        readState: () => state,
        exec: () => {
          throw new Error('a rejected cycle must not execute commands')
        },
        writeState: () => {
          throw new Error('a rejected cycle must not write state')
        },
      },
    )
    assert.notEqual(result.exitCode, 0)
    assert.equal(JSON.parse(result.stdout).outcome, 'base-branch-mismatch')
  }
})

test('init requires an exact origin ref even when ls-remote matches a glob', async () => {
  await assert.rejects(
    runInit(
      { 'base-branch': 'integrat*' },
      {
        exec: () => ({ status: 0, stdout: 'abc123\trefs/heads/integration\n', stderr: '' }),
        writeState: () => {
          throw new Error('an absent branch must not write state')
        },
      },
    ),
    /does not exist on origin/,
  )
})
