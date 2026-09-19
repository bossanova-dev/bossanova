import assert from 'node:assert/strict'
import test from 'node:test'

import {
  CONTINUATION_DIRECTIVES,
  EPIC_CHILD_PR_TRIGGERS,
  EPIC_DRIVER_IO,
  EPIC_RUN_STATUSES,
  EPIC_STATE_VERSION,
  FORBIDDEN_EPIC_CHILD_PR_TRIGGERS,
  NON_TERMINAL_STATUSES,
  assertEpicCanTerminate,
  assertEpicIo,
  beginCycle,
  buildContinuationPrompt,
  buildEpicProgressState,
  canEpicTerminate,
  createEpicState,
  epicDoneBlockers,
  epicStatePath,
  epicTerminalBlockers,
  greenAdmissionBlockers,
  loadEpicState,
  main,
  needsRearm,
  normalizeEpicState,
  recordAdoption,
  recordFailure,
  recordGreen,
  recordLaunch,
  recordMerge,
  recordProgressUpsert,
  recordReconciliation,
  recordWake,
  recordWatchCleanup,
  recordWatchCoverage,
  recordWatchFailure,
  reconcileEpic,
  renderRunStatusLine,
  saveEpicState,
  transitionToDone,
  validateContinuationPrompt,
  validateEpicState,
  verifySubscriptionRow,
} from './epic-driver.mjs'
import { parseProgressRunMetadata, validateProgressState } from './progress-comment.mjs'

const NOW = '2026-01-01T00:00:00Z'
const EPIC = 'EPIC-1'

function ticket(id, overrides = {}) {
  return {
    id,
    title: `work ${id}`,
    priority: 2,
    createdAt: '2026-01-01T00:00:00Z',
    blockedBy: [],
    ...overrides,
  }
}

function baseState(overrides = {}) {
  return createEpicState({
    runId: 'run-1',
    epicId: EPIC,
    repoId: 'repo-1',
    agent: 'claude',
    parallel: 2,
    plannedState: 'Todo',
    reviewState: 'In Review',
    childWallClockMinutes: 360,
    startedAt: NOW,
    tickets: [ticket('T-1'), ticket('T-2', { blockedBy: ['T-1'] })],
    ...overrides,
  })
}

// A snapshot of a child that is genuinely merge-eligible: passing, non-draft, in the review state,
// no partial marker, settled chat. Every "held" test flips exactly one of these.
function greenSnapshot(overrides = {}) {
  return {
    chatStatus: 'IDLE',
    chatStatusReadable: true,
    sessionState: 'READY_FOR_REVIEW',
    chatSettled: true,
    checkVerdict: { state: 'passing' },
    prView: { number: 10, repo: 'acme/app', url: 'https://example.test/pr/10', isDraft: false },
    reviewStateMatches: true,
    partialMarker: false,
    ...overrides,
  }
}

function subscriptionRow(overrides = {}) {
  return {
    id: 'sub-1',
    owner_session_id: 'sess-1',
    origin_chat_id: 'orch-chat',
    trigger_event: 'settled',
    selector: 'chat:orch-chat',
    state: 'active',
    fired_broadcast_id: '',
    fired_at: '',
    expires_at: '2026-01-02T00:00:00Z',
    created_at: NOW,
    updated_at: NOW,
    ...overrides,
  }
}

// A fully recording fake io. Every call is logged, so a test can assert what was NOT called —
// which is the only way to prove adoption did not relaunch.
function makeIo(overrides = {}) {
  const calls = []
  const record =
    (name, impl) =>
    async (...args) => {
      calls.push({ name, args })
      return typeof impl === 'function' ? impl(...args) : impl
    }
  const io = {
    calls,
    listSessions: record('listSessions', () => []),
    readSessionSnapshot: record('readSessionSnapshot', () => greenSnapshot()),
    readExternalBlockers: record('readExternalBlockers', () => ({})),
    createSession: record('createSession', ({ ticket: t }) => ({
      sessionId: `sess-${t.id}`,
      chatId: `chat-${t.id}`,
    })),
    armWatches: record('armWatches', () => ({ callbacks: [] })),
    listWatches: record('listWatches', () =>
      EPIC_CHILD_PR_TRIGGERS.map((trigger, index) => ({
        id: `cb-${index}`,
        trigger,
        state: 'active',
      })),
    ),
    subscribeSessionOutcome: record('subscribeSessionOutcome', () => ({
      subscription: subscriptionRow(),
    })),
    listSessionSubscriptions: record('listSessionSubscriptions', ({ sessionId }) => [
      subscriptionRow({ owner_session_id: sessionId }),
    ]),
    removeWatches: record('removeWatches', () => undefined),
    scheduleFallbackWake: record('scheduleFallbackWake', () => null),
    mergeChild: record('mergeChild', () => ({ merged: true })),
    verifyMerged: record('verifyMerged', () => true),
    moveTicketDone: record('moveTicketDone', () => true),
    upsertProgress: record('upsertProgress', () => ({ commentId: 'comment-1' })),
  }
  for (const [name, impl] of Object.entries(overrides)) io[name] = record(name, impl)
  io.names = () => calls.map((call) => call.name)
  io.count = (name) => calls.filter((call) => call.name === name).length
  return io
}

// A tiny in-memory fs standing in for node:fs, so the persistence round trip is exercised without
// touching the disk and a "fresh process" is just a second load from the same store.
function makeFs() {
  const files = new Map()
  return {
    files,
    mkdirSync: () => undefined,
    writeFileSync: (p, data) => files.set(p, data),
    renameSync: (from, to) => {
      files.set(to, files.get(from))
      files.delete(from)
    },
    existsSync: (p) => files.has(p),
    readFileSync: (p) => {
      if (!files.has(p)) throw Object.assign(new Error('ENOENT'), { code: 'ENOENT' })
      return files.get(p)
    },
    unlinkSync: (p) => files.delete(p),
  }
}

// --- state model -----------------------------------------------------------

test('createEpicState produces a valid, JSON-safe, normalized state', () => {
  const state = baseState()
  assert.equal(state.version, EPIC_STATE_VERSION)
  assert.deepEqual(validateEpicState(state), { ok: true, errors: [] })
  assert.deepEqual(JSON.parse(JSON.stringify(state)), state)
})

test('normalizeEpicState is idempotent', () => {
  const once = normalizeEpicState(baseState())
  assert.deepEqual(normalizeEpicState(once), once)
})

test('validateEpicState rejects Map and Set anywhere in the tree', () => {
  const withSet = { ...baseState(), inFlight: new Set(['T-1']) }
  const setVerdict = validateEpicState(withSet)
  assert.equal(setVerdict.ok, false)
  assert.ok(
    setVerdict.errors.some((e) => e.includes('inFlight')),
    setVerdict.errors.join('; '),
  )

  const nested = {
    ...baseState(),
    sessions: { 'T-1': { sessionId: 's', chatId: 'c', extra: new Map() } },
  }
  const mapVerdict = validateEpicState(nested)
  assert.equal(mapVerdict.ok, false)
  assert.ok(
    mapVerdict.errors.some((e) => e.includes('Map/Set is not JSON-safe')),
    mapVerdict.errors.join('; '),
  )
})

test('validateEpicState rejects a blank ownership identifier', () => {
  const state = { ...baseState(), sessions: { 'T-1': { sessionId: '', chatId: '  ' } } }
  const verdict = validateEpicState(state)
  assert.equal(verdict.ok, false)
  assert.ok(verdict.errors.some((e) => e.includes('sessions.T-1.sessionId')))
  assert.ok(verdict.errors.some((e) => e.includes('sessions.T-1.chatId')))
})

test('needs-human tickets never enter ready or inFlight, and cannot be launched', () => {
  const state = normalizeEpicState({
    ...baseState({ tickets: [ticket('T-1', { needsHuman: true }), ticket('T-2')] }),
    ready: ['T-1', 'T-2'],
    inFlight: ['T-1'],
  })
  assert.deepEqual(state.ready, ['T-2'])
  assert.deepEqual(state.inFlight, [])
  assert.deepEqual(state.needsHuman, ['T-1'])
  assert.throws(
    () => recordLaunch(state, { ticketId: 'T-1', sessionId: 's', chatId: 'c' }),
    /needs-human/,
  )
})

// --- persistence -----------------------------------------------------------

test('epicStatePath is deterministic from epic + run and refuses a missing key', () => {
  const first = epicStatePath({ epicId: EPIC, runId: 'run-1', dir: '/tmp/x' })
  assert.equal(first, epicStatePath({ epicId: EPIC, runId: 'run-1', dir: '/tmp/x' }))
  assert.notEqual(first, epicStatePath({ epicId: EPIC, runId: 'run-2', dir: '/tmp/x' }))
  assert.throws(() => epicStatePath({ runId: 'r', dir: '/tmp/x' }), /epicId is required/)
})

test('saveEpicState writes atomically via a temp file and refuses invalid state', () => {
  const fs = makeFs()
  const state = recordLaunch(baseState(), {
    ticketId: 'T-1',
    sessionId: 'sess-1',
    chatId: 'chat-1',
    at: NOW,
  })
  const { path: target } = saveEpicState(state, { dir: '/store', fs })
  assert.ok(fs.files.has(target))
  // The temp file is renamed away, never left behind.
  assert.equal(fs.files.has(`${target}.tmp`), false)
  assert.throws(
    () =>
      saveEpicState(
        { ...state, sessions: { 'T-1': { sessionId: '', chatId: '' } } },
        { dir: '/store', fs },
      ),
    /refusing to persist invalid state/,
  )
})

test('loadEpicState returns null for a fresh run and throws on a corrupt file', () => {
  const fs = makeFs()
  assert.equal(loadEpicState({ epicId: EPIC, runId: 'run-1', dir: '/store', fs }), null)
  const target = epicStatePath({ epicId: EPIC, runId: 'run-1', dir: '/store' })
  fs.files.set(target, '{not json')
  assert.throws(
    () => loadEpicState({ epicId: EPIC, runId: 'run-1', dir: '/store', fs }),
    /would relaunch live children/,
  )
})

// --- the terminal guard ----------------------------------------------------

test('launch is non-terminal: assertEpicCanTerminate rejects an in-flight child', () => {
  const launched = recordLaunch(baseState(), {
    ticketId: 'T-1',
    sessionId: 'sess-1',
    chatId: 'chat-1',
    at: NOW,
  })
  assert.equal(canEpicTerminate(launched), false)
  assert.throws(() => assertEpicCanTerminate(launched), /non-terminal; continuation required/)
  assert.throws(() => assertEpicCanTerminate(launched), /inFlight:T-1/)
  assert.notEqual(launched.status, EPIC_RUN_STATUSES.DONE)
  assert.ok(NON_TERMINAL_STATUSES.includes(launched.status))
})

test('adoption is non-terminal until reconciled, and separately as in-flight work', () => {
  const adopted = recordAdoption(baseState(), {
    ticketId: 'T-1',
    sessionId: 'sess-1',
    chatId: 'chat-1',
    at: NOW,
  })
  assert.equal(adopted.sessions['T-1'].reconciled, false)
  const blockers = epicTerminalBlockers(adopted)
  assert.ok(
    blockers.some((b) => b.startsWith('inFlight:')),
    blockers.join('; '),
  )
  assert.ok(
    blockers.some((b) => b.startsWith('unreconciled:')),
    blockers.join('; '),
  )
})

test('each terminal-invariant condition independently rejects DONE', () => {
  const settled = () =>
    recordReconciliation(baseState(), { at: NOW, externalBlockersEvaluated: true })
  assert.deepEqual(epicTerminalBlockers(settled()), [])

  const cases = {
    ready: { ready: ['T-1'] },
    inFlight: { inFlight: ['T-1'] },
    greens: { greens: ['T-1'] },
    cascade: { pendingCascade: ['T-2'] },
  }
  for (const [label, patch] of Object.entries(cases)) {
    const state = normalizeEpicState({ ...settled(), ...patch })
    const blockers = epicTerminalBlockers(state)
    assert.ok(
      blockers.some((b) => b.startsWith(`${label}:`)),
      `${label} did not block: ${blockers.join('; ')}`,
    )
    assert.throws(() => transitionToDone(state), /cannot be reported DONE/)
  }

  // An unreconciled wake.
  const woken = recordWake(settled(), { kind: 'callback', at: NOW })
  assert.ok(epicTerminalBlockers(woken).some((b) => b.startsWith('pendingWake:')))

  // A stale external-blocker evaluation is not an evaluation of THIS cycle.
  const staleCycle = beginCycle(settled())
  assert.ok(
    epicTerminalBlockers(staleCycle).includes('externalBlockers:not-evaluated-this-cycle'),
    epicTerminalBlockers(staleCycle).join('; '),
  )
})

test('transitionToDone is the sole DONE path and demands full terminal bookkeeping', () => {
  let state = normalizeEpicState({
    ...baseState(),
    merged: ['T-1', 'T-2'],
    externallyCleared: ['T-1', 'T-2'],
  })
  state = recordReconciliation(state, { at: NOW, externalBlockersEvaluated: true })
  // Invariant is false, but the bookkeeping is not done yet.
  assert.deepEqual(epicTerminalBlockers(state), [])
  let blockers = epicDoneBlockers(state)
  assert.ok(blockers.includes('finalProgress:not-written'), blockers.join('; '))
  assert.ok(blockers.includes('watchCleanup:not-done'), blockers.join('; '))
  assert.throws(() => transitionToDone(state), /cannot be reported DONE/)

  state = recordWatchCleanup(recordProgressUpsert(state, { commentId: 'c', finalAt: NOW }), {
    at: NOW,
  })
  assert.deepEqual(epicDoneBlockers(state), [])
  assert.equal(transitionToDone(state).status, EPIC_RUN_STATUSES.DONE)
})

test('a not-yet-terminal ticket blocks DONE even with empty work collections', () => {
  const state = recordReconciliation(
    recordWatchCleanup(recordProgressUpsert(baseState(), { commentId: 'c', finalAt: NOW }), {
      at: NOW,
    }),
    { at: NOW, externalBlockersEvaluated: true },
  )
  const blockers = epicDoneBlockers(state)
  assert.ok(
    blockers.some((b) => b.startsWith('notTerminal:T-1')),
    blockers.join('; '),
  )
})

// --- transitions -----------------------------------------------------------

test('recordMerge refuses an unverified merge and folds a merge into externallyCleared', () => {
  const launched = recordLaunch(baseState(), {
    ticketId: 'T-1',
    sessionId: 'sess-1',
    chatId: 'chat-1',
    at: NOW,
  })
  const green = recordGreen(launched, { ticketId: 'T-1' })
  assert.throws(() => recordMerge(green, { ticketId: 'T-1' }), /unverified merge/)
  assert.throws(() => recordMerge(green, { ticketId: 'T-1', verified: false }), /unverified merge/)
  const merged = recordMerge(green, { ticketId: 'T-1', verified: true })
  assert.deepEqual(merged.merged, ['T-1'])
  assert.deepEqual(merged.greens, [])
  assert.deepEqual(merged.inFlight, [])
  assert.ok(merged.externallyCleared.includes('T-1'))
})

test('recordFailure records every transitive dependent as cascadeSkipped', () => {
  const state = baseState({
    tickets: [
      ticket('T-1'),
      ticket('T-2', { blockedBy: ['T-1'] }),
      ticket('T-3', { blockedBy: ['T-2'] }),
      ticket('T-9'),
    ],
  })
  const launched = recordLaunch(state, {
    ticketId: 'T-1',
    sessionId: 'sess-1',
    chatId: 'chat-1',
    at: NOW,
  })
  const failed = recordFailure(launched, { ticketId: 'T-1', reason: 'wall clock' })
  assert.deepEqual(failed.failed, ['T-1'])
  assert.deepEqual(failed.cascadeSkipped.sort(), ['T-2', 'T-3'])
  assert.deepEqual(failed.pendingCascade, [])
  assert.ok(!failed.cascadeSkipped.includes('T-9'))
})

test('recordWatchFailure with a fallback stays RUNNING; without one it is never DONE', () => {
  const launched = recordLaunch(baseState(), {
    ticketId: 'T-1',
    sessionId: 'sess-1',
    chatId: 'chat-1',
    at: NOW,
  })
  const withFallback = recordWatchFailure(launched, {
    ticketId: 'T-1',
    reason: 'arm failed',
    fallback: { mechanism: 'in-session-wake', nextWakeAt: '2026-01-01T00:05:00Z' },
    at: NOW,
  })
  assert.equal(withFallback.status, EPIC_RUN_STATUSES.RUNNING)
  const fallback = withFallback.watches['T-1'].fallback
  assert.equal(fallback.mechanism, 'in-session-wake')
  assert.equal(fallback.nextWakeAt, '2026-01-01T00:05:00Z')
  assert.equal(fallback.reason, 'arm failed')
  assert.equal(fallback.retryCount, 1)

  const unwatched = recordWatchFailure(launched, {
    ticketId: 'T-1',
    reason: 'no transport',
    at: NOW,
  })
  assert.equal(unwatched.status, EPIC_RUN_STATUSES.RUNNING_BUT_UNWATCHED)
  const blocked = recordWatchFailure(launched, {
    ticketId: 'T-1',
    reason: 'no transport',
    at: NOW,
    blocked: true,
  })
  assert.equal(blocked.status, EPIC_RUN_STATUSES.BLOCKED)
  for (const state of [withFallback, unwatched, blocked]) {
    assert.notEqual(state.status, EPIC_RUN_STATUSES.DONE)
    assert.throws(() => assertEpicCanTerminate(state), /non-terminal/)
  }
})

// --- watch policy ----------------------------------------------------------

test('the epic child trigger set is draft-aware and never bare checks_passed', () => {
  assert.ok(EPIC_CHILD_PR_TRIGGERS.includes('checks_passed_ready'))
  assert.ok(EPIC_CHILD_PR_TRIGGERS.includes('ready_for_review'))
  assert.ok(EPIC_CHILD_PR_TRIGGERS.includes('checks_failed'))
  assert.ok(EPIC_CHILD_PR_TRIGGERS.includes('merged'))
  assert.deepEqual(FORBIDDEN_EPIC_CHILD_PR_TRIGGERS, ['checks_passed'])
  for (const forbidden of FORBIDDEN_EPIC_CHILD_PR_TRIGGERS) {
    assert.ok(
      !EPIC_CHILD_PR_TRIGGERS.includes(forbidden),
      `${forbidden} must never be armed as an epic child merge-ready signal`,
    )
  }
})

test('needsRearm re-arms a consumed or expired registration while the child is in flight', () => {
  assert.equal(needsRearm({ registration: { state: 'active' }, inFlight: true }), false)
  // A settled subscription can fire MID-FLIGHT and burn the wake, so `fired` is a hole.
  assert.equal(needsRearm({ registration: { state: 'fired' }, inFlight: true }), true)
  assert.equal(needsRearm({ registration: { state: 'expired' }, inFlight: true }), true)
  assert.equal(needsRearm({ registration: { state: 'canceled' }, inFlight: true }), true)
  assert.equal(needsRearm({ registration: null, inFlight: true }), true)
  // Nothing is re-armed for a child that is no longer in flight.
  assert.equal(needsRearm({ registration: null, inFlight: false }), false)
})

test('verifySubscriptionRow checks ownership and liveness, not just presence', () => {
  assert.equal(verifySubscriptionRow(subscriptionRow(), { ownerSessionId: 'sess-1' }).ok, true)
  assert.match(
    verifySubscriptionRow(subscriptionRow(), { ownerSessionId: 'other' }).reason,
    /owner_session_id/,
  )
  assert.match(
    verifySubscriptionRow(subscriptionRow({ state: 'fired', fired_at: NOW }), {
      ownerSessionId: 'sess-1',
    }).reason,
    /state is fired/,
  )
  assert.match(verifySubscriptionRow({ id: '' }).reason, /no id/)
  assert.match(
    verifySubscriptionRow(subscriptionRow(), { originChatId: 'someone-else' }).reason,
    /origin_chat_id/,
  )
})

// --- green admission -------------------------------------------------------

test('green admission holds a draft, an unsettled chat, a partial marker and a wrong state', () => {
  assert.deepEqual(greenAdmissionBlockers(greenSnapshot()), [])
  assert.deepEqual(greenAdmissionBlockers(greenSnapshot({ prView: { isDraft: true } })), [
    'pr:draft-or-unknown',
  ])
  assert.deepEqual(greenAdmissionBlockers(greenSnapshot({ chatSettled: false })), [
    'chat:unsettled',
  ])
  assert.deepEqual(greenAdmissionBlockers(greenSnapshot({ partialMarker: true })), [
    'pr:partial-marker',
  ])
  assert.deepEqual(greenAdmissionBlockers(greenSnapshot({ reviewStateMatches: false })), [
    'ticket:not-in-review-state',
  ])
  assert.deepEqual(greenAdmissionBlockers(greenSnapshot({ checkVerdict: { state: 'pending' } })), [
    'checks:pending',
  ])
  // Missing evidence is never admission.
  assert.ok(greenAdmissionBlockers({}).length >= 4)
})

// --- continuation prompts --------------------------------------------------

test('continuation prompts carry every directive and interpolate the epic id', () => {
  for (const kind of ['callback', 'subscription', 'fallback']) {
    const message = buildContinuationPrompt({ epicId: EPIC, kind, runId: 'run-1' })
    assert.deepEqual(validateContinuationPrompt(message, { epicId: EPIC }), {
      ok: true,
      errors: [],
    })
    for (const directive of Object.values(CONTINUATION_DIRECTIVES)) {
      assert.ok(message.includes(directive), `${kind} payload lost: ${directive}`)
    }
    assert.ok(message.includes(EPIC))
    assert.ok(message.includes(kind))
  }
  assert.throws(() => buildContinuationPrompt({ epicId: '' }), /epicId is required/)
  assert.throws(() => buildContinuationPrompt({ epicId: EPIC, kind: 'nope' }), /unknown wake kind/)
})

test('validateContinuationPrompt refuses an informational-only notification', () => {
  const verdict = validateContinuationPrompt('Your PR is green.', { epicId: EPIC })
  assert.equal(verdict.ok, false)
  assert.ok(verdict.errors.some((e) => e.includes('must name the epic')))
  for (const name of Object.keys(CONTINUATION_DIRECTIVES)) {
    assert.ok(verdict.errors.some((e) => e.includes(`missing ${name} directive`)))
  }
})

test('the driver hard-codes no example epic id', async () => {
  const { readFileSync } = await import('node:fs')
  const source = readFileSync(new URL('./epic-driver.mjs', import.meta.url), 'utf8')
  // A published core must not carry an example ticket from any backlog: the prompt interpolates the
  // epic id. `TICKET-123` is the shape; a handful of technical tokens share it (UTF-8, SHA-256,
  // HTTP-2) and are stripped first, because a gate that reds on those gets switched off and then
  // checks nothing at all.
  const scrubbed = source.replace(/\b(?:UTF|SHA|HTTP|ISO|RFC|SHA1|MD)-\d+\b/g, '')
  assert.doesNotMatch(scrubbed, /\b[A-Z]{2,}-\d+\b/)
})

test('renderRunStatusLine names the status and never says success', () => {
  const launched = recordLaunch(baseState(), {
    ticketId: 'T-1',
    sessionId: 'sess-1',
    chatId: 'chat-1',
    at: NOW,
  })
  const line = renderRunStatusLine(launched)
  assert.match(line, /^RUNNING EPIC-1 /)
  assert.match(line, /inFlight=1/)
  assert.doesNotMatch(line, /success/i)
})

// --- progress metadata -----------------------------------------------------

test('buildEpicProgressState is a valid ProgressState carrying resume metadata', () => {
  let state = recordLaunch(baseState(), {
    ticketId: 'T-1',
    sessionId: 'sess-1',
    chatId: 'chat-1',
    at: NOW,
    prNumber: 10,
    prUrl: 'https://example.test/pr/10',
  })
  state = recordWatchCoverage(state, { ticketId: 'T-1', callbacks: [], verifiedAt: NOW })
  state = recordReconciliation(state, { at: NOW, externalBlockersEvaluated: true })
  const progress = buildEpicProgressState(state, { updatedAt: NOW, nextWake: 'callback' })
  assert.deepEqual(validateProgressState(progress), { ok: true, errors: [] })
  assert.equal(progress.run.runId, 'run-1')
  assert.equal(progress.run.status, EPIC_RUN_STATUSES.RUNNING)
  assert.equal(progress.run.lastReconciledAt, NOW)
  assert.equal(progress.run.nextWake, 'callback')
  const row = progress.tickets.find((t) => t.id === 'T-1')
  assert.equal(row.status, 'building')
  assert.equal(row.pr, 'https://example.test/pr/10')
  assert.match(row.note, /session sess-1/)
  assert.match(row.note, /chat chat-1/)
  assert.match(row.note, /watch verified/)
})

test('an unwatched or fallback child says so in the progress note', () => {
  let state = recordLaunch(baseState(), {
    ticketId: 'T-1',
    sessionId: 'sess-1',
    chatId: 'chat-1',
    at: NOW,
  })
  state = recordWatchFailure(state, { ticketId: 'T-1', reason: 'arm refused', at: NOW })
  const unwatched = buildEpicProgressState(state, { updatedAt: NOW })
  assert.match(unwatched.tickets[0].note, /unwatched: arm refused/)

  state = recordWatchFailure(state, {
    ticketId: 'T-1',
    reason: 'arm refused',
    fallback: { mechanism: 'in-session-wake', nextWakeAt: NOW },
    at: NOW,
  })
  assert.match(
    buildEpicProgressState(state, { updatedAt: NOW }).tickets[0].note,
    /fallback in-session-wake/,
  )
})

// --- io contract -----------------------------------------------------------

test('assertEpicIo names every missing capability', () => {
  assert.throws(() => assertEpicIo({}), /io is missing/)
  const { calls, names, count, ...io } = makeIo()
  assert.equal(assertEpicIo(io), io)
  delete io.mergeChild
  assert.throws(() => assertEpicIo(io), /mergeChild/)
  assert.ok(EPIC_DRIVER_IO.includes('subscribeSessionOutcome'))
})

// --- reconciliation --------------------------------------------------------

test('a launch cycle arms verified coverage, persists, and returns RUNNING not DONE', async () => {
  const io = makeIo()
  const saves = []
  const result = await reconcileEpic({
    state: baseState({ parallel: 1 }),
    wake: 'initial',
    io,
    now: NOW,
    save: (next) => saves.push(next),
  })
  assert.equal(result.status, EPIC_RUN_STATUSES.RUNNING)
  assert.notEqual(result.status, EPIC_RUN_STATUSES.DONE)
  assert.ok(result.blockers.length > 0, result.blockers.join('; '))
  assert.ok(result.actions.includes('launch:T-1'))
  // Registration is list-VERIFIED before yielding.
  assert.ok(io.count('listWatches') >= 1)
  assert.ok(io.count('listSessionSubscriptions') >= 1)
  assert.ok(result.state.watches['T-1'].verifiedAt, 'coverage was not recorded as verified')
  assert.equal(result.state.watches['T-1'].subscription.id, 'sub-1')
  // Persisted after transitions, not only at the end.
  assert.ok(saves.length > 1, `expected multiple persists, got ${saves.length}`)
  assert.throws(() => assertEpicCanTerminate(result.state), /non-terminal/)
})

test('the continuation message armed on a watch and a subscription is a resume command', async () => {
  const armed = []
  const subscribed = []
  const io = makeIo({
    armWatches: ({ message }) => {
      armed.push(message)
      return { callbacks: [] }
    },
    subscribeSessionOutcome: ({ message }) => {
      subscribed.push(message)
      return { subscription: subscriptionRow() }
    },
    listWatches: () => [],
  })
  await reconcileEpic({ state: baseState({ parallel: 1 }), io, now: NOW })
  assert.ok(armed.length > 0 && subscribed.length > 0)
  for (const message of [...armed, ...subscribed]) {
    assert.deepEqual(validateContinuationPrompt(message, { epicId: EPIC }), {
      ok: true,
      errors: [],
    })
  }
})

test('adoption never creates a second session for the same ticket', async () => {
  const io = makeIo({
    listSessions: () => [
      {
        id: 'live-sess',
        title: '[T-1] work T-1',
        tracker_id: 'T-1',
        agent_session_id: 'live-chat',
      },
    ],
    readSessionSnapshot: () => greenSnapshot({ chatSettled: false }),
  })
  const result = await reconcileEpic({ state: baseState({ parallel: 1 }), io, now: NOW })
  assert.ok(result.actions.includes('adopt:T-1'))
  assert.equal(result.state.sessions['T-1'].sessionId, 'live-sess')
  assert.equal(result.state.sessions['T-1'].chatId, 'live-chat')
  assert.equal(io.count('createSession'), 0, 'an adopted ticket must never be relaunched')
  assert.notEqual(result.status, EPIC_RUN_STATUSES.DONE)
})

test('adoption matches on the [TICKET] title convention when tracker_id is absent', async () => {
  const io = makeIo({
    listSessions: () => [
      { id: 'live-sess', title: '[T-1] work T-1', agent_session_id: 'live-chat' },
    ],
    readSessionSnapshot: () => greenSnapshot({ chatSettled: false }),
  })
  const result = await reconcileEpic({ state: baseState({ parallel: 1 }), io, now: NOW })
  assert.equal(result.state.sessions['T-1'].sessionId, 'live-sess')
  assert.equal(io.count('createSession'), 0)
})

test('every wake kind enters the same cycle and re-reads authoritative state', async () => {
  for (const wake of ['callback', 'subscription', 'fallback', 'retry', 'manual-resume']) {
    const io = makeIo({ readSessionSnapshot: () => greenSnapshot({ chatSettled: false }) })
    const launched = recordLaunch(baseState({ parallel: 1 }), {
      ticketId: 'T-1',
      sessionId: 'sess-1',
      chatId: 'chat-1',
      at: NOW,
    })
    const result = await reconcileEpic({ state: launched, wake, io, now: NOW })
    assert.ok(result.actions.includes(`wake:${wake}`))
    // The wake did not select a shortcut: the child's own state was re-read.
    assert.equal(io.count('readSessionSnapshot'), 1, `${wake} skipped the authoritative re-read`)
    assert.ok(io.count('readExternalBlockers') >= 0)
    // And the wake no longer holds the run open once reconciled.
    assert.ok(!epicTerminalBlockers(result.state).some((b) => b.startsWith('pendingWake:')))
  }
})

test('a draft green and an unsettled chat are held out of greens on a wake', async () => {
  for (const snapshot of [
    greenSnapshot({ prView: { number: 10, repo: 'acme/app', isDraft: true } }),
    greenSnapshot({ chatSettled: false }),
  ]) {
    const io = makeIo({ readSessionSnapshot: () => snapshot })
    const launched = recordLaunch(baseState({ parallel: 1 }), {
      ticketId: 'T-1',
      sessionId: 'sess-1',
      chatId: 'chat-1',
      at: NOW,
    })
    const result = await reconcileEpic({ state: launched, wake: 'callback', io, now: NOW })
    assert.deepEqual(result.state.greens, [])
    assert.equal(io.count('mergeChild'), 0, 'a held child must never be merged')
    assert.ok(result.actions.some((a) => a.startsWith('hold:T-1:')))
  }
})

test('two greens issue exactly one merge, and the verified merge unblocks its dependent', async () => {
  const io = makeIo()
  let state = baseState({
    parallel: 4,
    tickets: [ticket('T-1'), ticket('T-2'), ticket('T-3', { blockedBy: ['T-1'] })],
  })
  state = recordLaunch(state, { ticketId: 'T-1', sessionId: 'sess-1', chatId: 'chat-1', at: NOW })
  state = recordLaunch(state, { ticketId: 'T-2', sessionId: 'sess-2', chatId: 'chat-2', at: NOW })
  const result = await reconcileEpic({ state, wake: 'callback', io, now: NOW })
  assert.equal(io.count('mergeChild'), 1, 'merges must be serialized: one per cycle')
  assert.equal(io.count('moveTicketDone'), 1)
  assert.equal(result.state.merged.length, 1)
  // The other green keeps its slot and its place in the queue.
  assert.equal(result.state.greens.length, 1)
  // T-3 was blocked by T-1. If T-1 merged, the same cycle must both unpark T-3 AND take it into
  // flight — concurrency is 4, so the slot exists. Leaving it merely `ready` would end the cycle
  // non-terminal with nothing armed to wake it. If the other green merged, T-3 stays parked.
  const mergedId = result.state.merged[0]
  if (mergedId === 'T-1') {
    assert.ok(
      result.state.inFlight.includes('T-3'),
      'a verified merge must unblock its dependent AND schedule it in the same cycle',
    )
    assert.ok(!result.state.ready.includes('T-3'), 'a launched dependent is no longer ready')
  } else {
    assert.ok(!result.state.ready.includes('T-3'))
    assert.ok(!result.state.inFlight.includes('T-3'))
  }
  assert.notEqual(result.status, EPIC_RUN_STATUSES.DONE)
})

test('an unverified merge is demoted rather than recorded, and writes no Done', async () => {
  const io = makeIo({
    mergeChild: () => ({ merged: false, error: 'PR is not passing' }),
    verifyMerged: () => false,
  })
  let state = baseState({ parallel: 2, tickets: [ticket('T-1')] })
  state = recordLaunch(state, { ticketId: 'T-1', sessionId: 'sess-1', chatId: 'chat-1', at: NOW })
  const result = await reconcileEpic({ state, wake: 'callback', io, now: NOW })
  assert.equal(io.count('moveTicketDone'), 0, 'only a verified merge may write Done')
  assert.deepEqual(result.state.merged, [])
  assert.deepEqual(result.state.greens, [])
  assert.ok(result.actions.includes('merge-unverified:T-1'))
  assert.match(result.state.sessions['T-1'].note, /merge not verified/)
})

test('a merge is skipped while the target has a still-open external blocker', async () => {
  const io = makeIo({ readExternalBlockers: () => ({ 'EXT-9': 'open' }) })
  let state = baseState({ parallel: 2, tickets: [ticket('T-1', { blockedBy: ['EXT-9'] })] })
  state = recordLaunch(state, { ticketId: 'T-1', sessionId: 'sess-1', chatId: 'chat-1', at: NOW })
  const result = await reconcileEpic({ state, wake: 'callback', io, now: NOW })
  assert.equal(io.count('mergeChild'), 0)
  assert.ok(result.actions.some((a) => a.startsWith('merge-skip:T-1:external:EXT-9')))
})

test('watch registration failure yields a fallback RUNNING, or unwatched — never DONE', async () => {
  const withFallback = makeIo({
    listWatches: () => [],
    armWatches: () => ({ error: 'callback transport unavailable' }),
    scheduleFallbackWake: () => ({
      mechanism: 'in-session-wake',
      nextWakeAt: '2026-01-01T00:05:00Z',
    }),
    readSessionSnapshot: () => greenSnapshot({ chatSettled: false }),
  })
  let state = recordLaunch(baseState({ parallel: 1 }), {
    ticketId: 'T-1',
    sessionId: 'sess-1',
    chatId: 'chat-1',
    at: NOW,
  })
  let result = await reconcileEpic({ state, wake: 'callback', io: withFallback, now: NOW })
  assert.equal(result.status, EPIC_RUN_STATUSES.RUNNING)
  const fallback = result.state.watches['T-1'].fallback
  assert.equal(fallback.mechanism, 'in-session-wake')
  assert.equal(fallback.nextWakeAt, '2026-01-01T00:05:00Z')
  assert.match(fallback.reason, /callback transport unavailable/)
  assert.equal(fallback.retryCount, 1)
  assert.ok(result.actions.some((a) => a.startsWith('fallback:T-1:in-session-wake')))

  const noFallback = makeIo({
    listWatches: () => [],
    armWatches: () => ({ error: 'callback transport unavailable' }),
    subscribeSessionOutcome: () => ({ error: 'no durable transport' }),
    listSessionSubscriptions: () => [],
    scheduleFallbackWake: () => null,
    readSessionSnapshot: () => greenSnapshot({ chatSettled: false }),
  })
  result = await reconcileEpic({ state, wake: 'callback', io: noFallback, now: NOW })
  assert.equal(result.status, EPIC_RUN_STATUSES.RUNNING_BUT_UNWATCHED)
  assert.notEqual(result.status, EPIC_RUN_STATUSES.DONE)
  assert.equal(result.state.watches['T-1'].fallback, null)
  assert.match(
    result.state.watches['T-1'].unwatchedReason,
    /no durable transport|callback transport/,
  )
})

test('an arm that returns clean but does not appear in the list read is not coverage', async () => {
  // The exact failure the list-read verification exists for: no error, and nothing armed.
  const io = makeIo({
    listWatches: () => [],
    armWatches: () => ({ callbacks: [] }),
    scheduleFallbackWake: () => ({ mechanism: 'in-session-wake', nextWakeAt: NOW }),
    readSessionSnapshot: () => greenSnapshot({ chatSettled: false }),
  })
  const state = recordLaunch(baseState({ parallel: 1 }), {
    ticketId: 'T-1',
    sessionId: 'sess-1',
    chatId: 'chat-1',
    at: NOW,
  })
  const result = await reconcileEpic({ state, wake: 'callback', io, now: NOW })
  assert.equal(result.state.watches['T-1'].verifiedAt, '')
  assert.match(result.state.watches['T-1'].unwatchedReason, /list verification found no watch/)
  assert.notEqual(result.status, EPIC_RUN_STATUSES.DONE)
})

test('a fired subscription is re-armed while the child stays in flight', async () => {
  let listed = 0
  const io = makeIo({
    listSessionSubscriptions: () => {
      listed += 1
      return [subscriptionRow({ id: 'sub-2', owner_session_id: 'sess-1' })]
    },
    subscribeSessionOutcome: () => ({ subscription: subscriptionRow({ id: 'sub-2' }) }),
    readSessionSnapshot: () => greenSnapshot({ chatSettled: false }),
  })
  let state = recordLaunch(baseState({ parallel: 1 }), {
    ticketId: 'T-1',
    sessionId: 'sess-1',
    chatId: 'chat-1',
    at: NOW,
  })
  // A settled subscription that already fired mid-flight: the one-shot wake is burned.
  state = recordWatchCoverage(state, {
    ticketId: 'T-1',
    callbacks: [],
    subscription: { id: 'sub-1', ownerSessionId: 'sess-1', state: 'fired', firedAt: NOW },
    verifiedAt: NOW,
  })
  const result = await reconcileEpic({ state, wake: 'subscription', io, now: NOW })
  assert.equal(io.count('subscribeSessionOutcome'), 1, 'a burned wake must be re-armed')
  assert.equal(result.state.watches['T-1'].subscription.id, 'sub-2')
  assert.equal(result.state.watches['T-1'].subscription.state, 'active')
  assert.ok(listed > 0)
})

test('a fail-isolated child retains its evidence watch through terminal cleanup', async () => {
  const io = makeIo({
    readSessionSnapshot: () => greenSnapshot({ wallClockExceeded: true, chatSettled: false }),
  })
  let state = baseState({
    parallel: 1,
    tickets: [ticket('T-1'), ticket('T-2', { blockedBy: ['T-1'] })],
  })
  state = recordLaunch(state, { ticketId: 'T-1', sessionId: 'sess-1', chatId: 'chat-1', at: NOW })
  state = recordWatchCoverage(state, {
    ticketId: 'T-1',
    callbacks: [{ id: 'cb-1', trigger: 'merged', state: 'active' }],
    verifiedAt: NOW,
  })
  const result = await reconcileEpic({ state, wake: 'callback', io, now: NOW })
  assert.ok(result.actions.includes('fail-isolate:T-1'))
  assert.deepEqual(result.state.failed, ['T-1'])
  assert.deepEqual(result.state.cascadeSkipped, ['T-2'])
  assert.ok(result.state.retainedEvidenceWatches.includes('T-1'))
  assert.ok(result.actions.includes('retain-evidence-watch:T-1'))
  assert.equal(io.count('removeWatches'), 0, 'a live fail-isolated child keeps its evidence watch')
  // The run may terminate: every ticket is failed or cascade-skipped.
  assert.equal(result.status, EPIC_RUN_STATUSES.DONE)
})

test('a full run reaches DONE only after cleanup and the final progress upsert', async () => {
  const io = makeIo()
  let state = baseState({ parallel: 1, tickets: [ticket('T-1')] })
  state = recordLaunch(state, { ticketId: 'T-1', sessionId: 'sess-1', chatId: 'chat-1', at: NOW })
  const result = await reconcileEpic({ state, wake: 'callback', io, now: NOW })
  assert.equal(result.status, EPIC_RUN_STATUSES.DONE)
  assert.deepEqual(result.blockers, [])
  assert.equal(result.state.finalProgressWrittenAt, NOW)
  assert.equal(result.state.watchCleanupDoneAt, NOW)
  assert.ok(io.count('removeWatches') >= 1)
  assert.ok(io.count('upsertProgress') >= 2, 'the final summary is its own upsert')
  assert.ok(result.actions.includes('terminal:done'))
  assert.equal(assertEpicCanTerminate(result.state), true)
})

test('a needs-human ticket is never launched by a reconciliation cycle', async () => {
  const io = makeIo()
  const state = baseState({
    parallel: 4,
    tickets: [ticket('T-1', { needsHuman: true }), ticket('T-2', { needsHuman: true })],
  })
  const result = await reconcileEpic({ state, wake: 'initial', io, now: NOW })
  assert.equal(io.count('createSession'), 0)
  assert.deepEqual(result.state.ready, [])
  assert.deepEqual(result.state.inFlight, [])
})

test('reconcileEpic refuses an unknown wake kind and an unwired io', async () => {
  await assert.rejects(
    () => reconcileEpic({ state: baseState(), wake: 'whenever', io: makeIo(), now: NOW }),
    /unknown wake/,
  )
  await assert.rejects(
    () => reconcileEpic({ state: baseState(), io: {}, now: NOW }),
    /io is missing/,
  )
})

// --- fresh-process resume --------------------------------------------------

test('a fresh process reconstructs the same in-flight record and does not relaunch', async () => {
  const fs = makeFs()
  const dir = '/store'
  const firstIo = makeIo({ readSessionSnapshot: () => greenSnapshot({ chatSettled: false }) })
  const first = await reconcileEpic({
    state: baseState({ parallel: 1 }),
    wake: 'initial',
    io: firstIo,
    now: NOW,
    save: (next) => saveEpicState(next, { dir, fs }),
  })
  assert.equal(firstIo.count('createSession'), 1)
  const launchedSession = first.state.sessions['T-1'].sessionId

  // --- simulated driver death: nothing survives but the state file and the live session.
  const rehydrated = loadEpicState({ epicId: EPIC, runId: 'run-1', dir, fs })
  assert.deepEqual(rehydrated.inFlight, ['T-1'])
  assert.equal(rehydrated.sessions['T-1'].sessionId, launchedSession)
  assert.deepEqual(validateEpicState(rehydrated), { ok: true, errors: [] })

  const secondIo = makeIo({
    listSessions: () => [
      {
        id: launchedSession,
        title: '[T-1] work T-1',
        tracker_id: 'T-1',
        agent_session_id: 'chat-T-1',
      },
    ],
    readSessionSnapshot: () => greenSnapshot({ chatSettled: false }),
  })
  const second = await reconcileEpic({
    state: rehydrated,
    wake: 'manual-resume',
    io: secondIo,
    now: '2026-01-01T00:10:00Z',
    save: (next) => saveEpicState(next, { dir, fs }),
  })
  assert.equal(secondIo.count('createSession'), 0, 'resume must adopt, never relaunch')
  assert.deepEqual(second.state.inFlight, ['T-1'])
  assert.equal(second.state.sessions['T-1'].sessionId, launchedSession)
  assert.equal(second.state.runId, 'run-1')
  assert.notEqual(second.status, EPIC_RUN_STATUSES.DONE)
})

test('the progress comment carries the run join a fresh driver resumes from', async () => {
  const { renderProgressComment } = await import('./progress-comment.mjs')
  let state = recordLaunch(baseState(), {
    ticketId: 'T-1',
    sessionId: 'sess-1',
    chatId: 'chat-1',
    at: NOW,
  })
  state = recordReconciliation(state, { at: NOW, externalBlockersEvaluated: true })
  const body = renderProgressComment(
    buildEpicProgressState(state, { updatedAt: NOW, nextWake: 'callback' }),
  )
  const parsed = parseProgressRunMetadata(body)
  assert.equal(parsed.runId, 'run-1')
  assert.equal(parsed.status, EPIC_RUN_STATUSES.RUNNING)
  assert.equal(parsed.lastReconciledAt, NOW)
  assert.equal(parsed.nextWake, 'callback')
  // The join is what makes the state file findable from the comment alone.
  assert.equal(
    epicStatePath({ epicId: EPIC, runId: parsed.runId, dir: '/store' }),
    epicStatePath({ epicId: EPIC, runId: 'run-1', dir: '/store' }),
  )
})

// --- duplicate / late wakes -----------------------------------------------

test('a duplicate late wake is idempotent: no relaunch, no second merge', async () => {
  const fs = makeFs()
  const dir = '/store'
  let state = baseState({ parallel: 1, tickets: [ticket('T-1')] })
  state = recordLaunch(state, { ticketId: 'T-1', sessionId: 'sess-1', chatId: 'chat-1', at: NOW })
  saveEpicState(state, { dir, fs })

  const io = makeIo({
    listSessions: () => [
      { id: 'sess-1', title: '[T-1] work T-1', tracker_id: 'T-1', agent_session_id: 'chat-1' },
    ],
  })
  const first = await reconcileEpic({
    state: loadEpicState({ epicId: EPIC, runId: 'run-1', dir, fs }),
    wake: 'callback',
    io,
    now: NOW,
    save: (next) => saveEpicState(next, { dir, fs }),
  })
  assert.equal(io.count('mergeChild'), 1)
  assert.equal(first.status, EPIC_RUN_STATUSES.DONE)

  // The same callback delivered twice (at-least-once delivery, or a stale-SHA repeat).
  const second = await reconcileEpic({
    state: loadEpicState({ epicId: EPIC, runId: 'run-1', dir, fs }),
    wake: 'callback',
    io,
    now: NOW,
    save: (next) => saveEpicState(next, { dir, fs }),
  })
  assert.equal(io.count('mergeChild'), 1, 'a repeat delivery must not merge again')
  assert.equal(io.count('createSession'), 0)
  assert.deepEqual(second.state.merged, ['T-1'])
})

// --- CLI -------------------------------------------------------------------

test('the CLI exposes validate / blockers / status / prompt and refuses anything else', () => {
  const fs = makeFs()
  const launched = recordLaunch(baseState(), {
    ticketId: 'T-1',
    sessionId: 'sess-1',
    chatId: 'chat-1',
    at: NOW,
  })
  fs.files.set('/state.json', JSON.stringify(launched))

  assert.deepEqual(main(['validate', '--state', '/state.json'], { fs }), { ok: true, errors: [] })
  const blockers = main(['blockers', '--state', '/state.json'], { fs })
  assert.equal(blockers.canTerminate, false)
  assert.ok(blockers.blockers.some((b) => b.startsWith('inFlight:')))
  assert.equal(main(['status', '--state', '/state.json'], { fs }).status, EPIC_RUN_STATUSES.RUNNING)
  const prompt = main(['prompt', '--epic', EPIC, '--kind', 'subscription'], { fs })
  assert.deepEqual(validateContinuationPrompt(prompt.message, { epicId: EPIC }), {
    ok: true,
    errors: [],
  })
  assert.throws(() => main(['nope'], { fs }), /unknown command/)
})

// --- the chained dry-run ---------------------------------------------------
//
// The plan's named proof, as ONE scenario rather than the sum of the single-leg tests above:
// launch a root, verify its durable coverage, wake, reconcile, merge exactly once, launch the
// dependent the merge unblocked, and only then reach terminal cleanup — asserting at EVERY
// intermediate step that the response is RUNNING and the terminal guard still throws. A run that
// returns DONE at any of those points is the incident this ticket exists to make impossible.

test('dry run: launch -> wake -> one merge -> dependent launch -> terminal, RUNNING throughout', async () => {
  const fs = makeFs()
  const dir = '/store'
  const save = (next) => saveEpicState(next, { dir, fs })
  const load = () => loadEpicState({ epicId: EPIC, runId: 'run-1', dir, fs })

  // Two children, T-2 blocked by T-1, concurrency 1 so the dependent cannot start early.
  let state = baseState({ parallel: 1 })
  const live = new Map()
  // The child is still working on its first two cycles, so it cannot go green yet.
  let settled = false
  const io = makeIo({
    listSessions: () => [...live.values()],
    readSessionSnapshot: () => greenSnapshot({ chatSettled: settled }),
    createSession: ({ ticket: t }) => {
      const record = {
        id: `sess-${t.id}`,
        title: `[${t.id}] work ${t.id}`,
        tracker_id: t.id,
        agent_session_id: `chat-${t.id}`,
      }
      live.set(t.id, record)
      return { sessionId: record.id, chatId: record.agent_session_id }
    },
  })

  // --- cycle 1: initial invocation launches the root only.
  let result = await reconcileEpic({ state, wake: 'initial', io, now: NOW, save })
  assert.equal(result.status, EPIC_RUN_STATUSES.RUNNING)
  assert.deepEqual(result.state.inFlight, ['T-1'])
  assert.equal(
    io.count('createSession'),
    1,
    'the dependent must not launch behind an unmerged blocker',
  )
  assert.ok(
    result.state.watches['T-1'].verifiedAt,
    'coverage must be list-verified before yielding',
  )
  assert.equal(result.state.watches['T-1'].subscription.state, 'active')
  assert.throws(() => assertEpicCanTerminate(result.state), /non-terminal/)

  // --- cycle 2: a callback wake arrives while the child is still working. Still RUNNING.
  result = await reconcileEpic({ state: load(), wake: 'callback', io, now: NOW, save })
  assert.equal(result.status, EPIC_RUN_STATUSES.RUNNING)
  assert.deepEqual(result.state.greens, [], 'an unsettled chat is held out of the merge queue')
  assert.equal(io.count('mergeChild'), 0)
  assert.throws(() => assertEpicCanTerminate(result.state), /non-terminal/)

  // --- cycle 3: the settled subscription fires. Now the root is admissible and merges once.
  settled = true
  result = await reconcileEpic({ state: load(), wake: 'subscription', io, now: NOW, save })
  assert.equal(io.count('mergeChild'), 1, 'exactly one merge per cycle')
  assert.equal(io.count('moveTicketDone'), 1, 'only a verified merge writes Done')
  assert.deepEqual(result.state.merged, ['T-1'])
  // The merge unblocked the dependent in the SAME cycle, and the concurrency slot freed up, so
  // T-2 launched here. Either way the run is RUNNING, never DONE.
  assert.equal(result.status, EPIC_RUN_STATUSES.RUNNING)
  assert.deepEqual(result.state.inFlight, ['T-2'], 'the unblocked dependent takes the free slot')
  assert.equal(io.count('createSession'), 2)
  assert.throws(() => assertEpicCanTerminate(result.state), /non-terminal/)

  // --- cycle 4: the dependent settles and merges; nothing remains, so this is terminal.
  result = await reconcileEpic({ state: load(), wake: 'callback', io, now: NOW, save })
  assert.equal(io.count('mergeChild'), 2, 'one merge per cycle, never two')
  assert.deepEqual(result.state.merged.sort(), ['T-1', 'T-2'])
  assert.equal(result.status, EPIC_RUN_STATUSES.DONE)
  assert.deepEqual(result.blockers, [])
  assert.equal(result.state.finalProgressWrittenAt, NOW)
  assert.equal(result.state.watchCleanupDoneAt, NOW)
  assert.equal(assertEpicCanTerminate(result.state), true)

  // The persisted state agrees with the returned state: a fresh process reading the file sees a
  // finished run, not a run it should restart.
  const reloaded = load()
  assert.equal(reloaded.status, EPIC_RUN_STATUSES.DONE)
  assert.deepEqual(epicTerminalBlockers(reloaded), [])
})
