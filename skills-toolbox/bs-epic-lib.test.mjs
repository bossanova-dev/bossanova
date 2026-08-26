// scripts/bs-epic-lib.test.mjs — node:test, mirroring linear-deps-lib.test.mjs style.
// Unit tests for the /bs-epic pure scheduling library. node builtins only.

import assert from 'node:assert/strict'
import { test } from 'node:test'

import {
  normalizeTicket,
  classifyTickets,
  buildGraph,
  readyTickets,
  transitiveDependents,
  nextToMerge,
  parseEpicArgs,
  parseTicketRef,
  mergeBlockedExternalBlockers,
  classifyChildLiveness,
  CHILD_LIVENESS_VERDICTS,
  CHILD_LIVENESS_ACTIONS,
  classifyRepairLease,
  resolveStateRole,
  resolvePlannedState,
  REPAIR_STALL_WINDOW_MS,
} from './bs-epic-lib.mjs'

const t = (id, over = {}) => ({
  id,
  title: id,
  priority: 3,
  createdAt: '2026-01-01T00:00:00Z',
  stateName: 'Todo',
  stateType: 'unstarted',
  labels: ['agent-friendly'],
  planUrl: `https://proof.bossanova.dev/${id}`,
  planAttachment: { title: `Implementation plan (${id})`, url: `https://uploads.example/${id}.md` },
  blockedBy: [],
  ...over,
})

// --- normalizeTicket ------------------------------------------------------

test('normalizeTicket: raw GraphQL shape uses extractBlockers + state.{name,type}', () => {
  const issue = {
    id: 'issue-uuid-5',
    identifier: 'BOS-5',
    title: 'Do the thing',
    priority: 2,
    createdAt: '2026-01-01T00:00:00Z',
    state: { name: 'Todo', type: 'unstarted' },
    labels: ['agent-friendly'],
    attachments: [
      { title: 'Implementation plan (BOS-5)', url: 'https://proof.bossanova.dev/BOS-5' },
    ],
    inverseRelations: {
      nodes: [
        {
          type: 'blocks',
          issue: {
            id: 'blocker-uuid-1',
            identifier: 'BOS-1',
            state: { name: 'Todo', type: 'unstarted' },
          },
        },
      ],
    },
  }
  const ticket = normalizeTicket(issue)
  assert.equal(ticket.id, 'BOS-5')
  assert.equal(ticket.title, 'Do the thing')
  assert.equal(ticket.priority, 2)
  assert.equal(ticket.stateName, 'Todo')
  assert.equal(ticket.stateType, 'unstarted')
  assert.deepEqual(ticket.labels, ['agent-friendly'])
  assert.equal(ticket.planUrl, 'https://proof.bossanova.dev/BOS-5')
  assert.deepEqual(ticket.planAttachment, issue.attachments[0])
  assert.deepEqual(ticket.blockedBy, ['BOS-1'])
})

test('normalizeTicket: MCP get_issue includeRelations shape uses relations.blockedBy + status/statusType + priority.value', () => {
  const issue = {
    id: 'BOS-6',
    title: 'MCP ticket',
    priority: { value: 1, name: 'Urgent' },
    createdAt: '2026-02-01T00:00:00Z',
    status: 'Todo',
    statusType: 'unstarted',
    labels: ['agent-friendly'],
    attachments: [
      { title: 'Implementation plan (BOS-6)', url: 'https://proof.bossanova.dev/BOS-6' },
    ],
    relations: {
      blockedBy: [{ identifier: 'BOS-2' }, 'BOS-3'],
    },
  }
  const ticket = normalizeTicket(issue)
  assert.equal(ticket.id, 'BOS-6')
  assert.equal(ticket.priority, 1)
  assert.equal(ticket.stateName, 'Todo')
  assert.equal(ticket.stateType, 'unstarted')
  assert.equal(ticket.planUrl, 'https://proof.bossanova.dev/BOS-6')
  assert.deepEqual(ticket.planAttachment, issue.attachments[0])
  assert.deepEqual(ticket.blockedBy, ['BOS-2', 'BOS-3'])
})

test('normalizeTicket: falls back to an already-flat blockedBy array when no relation shape is present', () => {
  const ticket = normalizeTicket({
    id: 'BOS-7',
    title: 'Flat',
    priority: 3,
    createdAt: '2026-01-01T00:00:00Z',
    stateName: 'Todo',
    stateType: 'unstarted',
    labels: [],
    blockedBy: ['BOS-1', 'BOS-2'],
  })
  assert.deepEqual(ticket.blockedBy, ['BOS-1', 'BOS-2'])
  assert.equal(ticket.stateName, 'Todo')
  assert.equal(ticket.stateType, 'unstarted')
})

test('normalizeTicket: link-only plans retain display metadata but have no native attachment', () => {
  const ticket = normalizeTicket({
    id: 'BOS-8',
    title: 'Links fallback',
    priority: 3,
    createdAt: '2026-01-01T00:00:00Z',
    stateName: 'Todo',
    stateType: 'unstarted',
    labels: [],
    links: [{ title: 'Implementation plan (BOS-8)', url: 'https://proof.bossanova.dev/BOS-8' }],
    blockedBy: [],
  })
  assert.equal(ticket.planUrl, 'https://proof.bossanova.dev/BOS-8')
  assert.equal(ticket.planAttachment, null)
})

test('normalizeTicket: a canonical attachment takes precedence over a stale plan link', () => {
  const ticket = normalizeTicket({
    id: 'BOS-999',
    title: 'Attachment precedence',
    priority: 3,
    createdAt: '2026-01-01T00:00:00Z',
    stateName: 'Todo',
    stateType: 'unstarted',
    labels: [],
    links: [{ title: 'Implementation plan (BOS-999)', url: 'https://example.test/stale.md' }],
    attachments: [
      {
        id: 'attachment-id',
        title: 'Implementation plan (BOS-999)',
        url: 'https://uploads.example/current.md',
        createdAt: '2026-02-01T00:00:00Z',
      },
    ],
    blockedBy: [],
  })
  assert.equal(ticket.planUrl, 'https://uploads.example/current.md')
  assert.equal(ticket.planAttachment?.id, 'attachment-id')
})

test('normalizeTicket: does not accept an untitled attachment as a plan', () => {
  const ticket = normalizeTicket({
    id: 'BOS-999',
    title: 'Untitled attachment',
    priority: 3,
    createdAt: '2026-01-01T00:00:00Z',
    stateName: 'Todo',
    stateType: 'unstarted',
    labels: [],
    attachments: [{ id: 'attachment-id', url: 'https://uploads.example/current.md' }],
    blockedBy: [],
  })
  assert.equal(ticket.planUrl, null)
  assert.equal(ticket.planAttachment, null)
})

test('normalizeTicket: planUrl is null when no matching attachment/link exists', () => {
  const ticket = normalizeTicket({
    id: 'BOS-9',
    title: 'No plan',
    priority: 3,
    createdAt: '2026-01-01T00:00:00Z',
    stateName: 'Todo',
    stateType: 'unstarted',
    labels: [],
    blockedBy: [],
  })
  assert.equal(ticket.planUrl, null)
  assert.equal(ticket.planAttachment, null)
})

// --- classifyTickets --------------------------------------------------------

test('classifyTickets: planned-state + agent-friendly + plan is eligible', () => {
  const { eligible } = classifyTickets([t('BOS-1')], 'Todo')
  assert.equal(eligible.length, 1)
})
test('classifyTickets: a legacy link-only plan is skipped for migration/replanning', () => {
  const { eligible, skipped } = classifyTickets([t('BOS-2', { planAttachment: null })], 'Todo')
  assert.equal(eligible.length, 0)
  assert.match(skipped[0].reason, /native Implementation plan attachment.*migration\/replanning/)
})
test('classifyTickets: needs-human, missing plan, not-planned, In Progress all skip with reasons', () => {
  const { skipped } = classifyTickets(
    [
      t('BOS-1', { labels: ['needs-human'] }),
      t('BOS-2', { planAttachment: null }),
      t('BOS-3', { stateName: 'Unplanned' }),
      t('BOS-4', { stateName: 'In Progress' }),
    ],
    'Todo',
  )
  assert.equal(skipped.length, 4)
  for (const s of skipped) assert.ok(s.reason.length > 0)
})
test('classifyTickets: Done child counts as done, not skipped', () => {
  const { done } = classifyTickets(
    [t('BOS-1', { stateName: 'Done', stateType: 'completed' })],
    'Todo',
  )
  assert.equal(done.length, 1)
})
test('classifyTickets: planned state is config-driven, not hard-coded', () => {
  // A repo whose configured planned state is a different word: the ticket in that
  // state is eligible, and one named "Todo" is skipped as not-yet-planned. Proves
  // the eligibility gate reads the passed planned-state name, never a baked literal.
  const tickets = [t('BOS-1', { stateName: 'Ready' }), t('BOS-2', { stateName: 'Todo' })]
  const { eligible, skipped } = classifyTickets(tickets, 'Ready')
  assert.deepEqual(
    eligible.map((tk) => tk.id),
    ['BOS-1'],
  )
  assert.deepEqual(
    skipped.map((s) => s.ticket.id),
    ['BOS-2'],
  )
  assert.match(skipped[0].reason, /expected Ready/)
})
test('classifyTickets: a missing planned-state name fails closed', () => {
  // Rather than silently marking every ticket eligible (or none), an unresolved
  // planned-state config surfaces loudly so a mis-configured repo cannot spawn
  // sessions for unplanned work.
  for (const bad of [undefined, null, '']) {
    assert.throws(() => classifyTickets([t('BOS-1')], bad), /plannedState/)
  }
})

// --- readyTickets / buildGraph / transitiveDependents -----------------------

test('readyTickets: blocked ticket not ready until blocker merged', () => {
  const g = buildGraph([t('BOS-1'), t('BOS-2', { blockedBy: ['BOS-1'] })])
  assert.deepEqual(
    readyTickets(g, {
      merged: new Set(),
      failed: new Set(),
      inFlight: new Set(),
      externallyCleared: new Set(),
    }).map((x) => x.id),
    ['BOS-1'],
  )
  assert.deepEqual(
    readyTickets(g, {
      merged: new Set(['BOS-1']),
      failed: new Set(),
      inFlight: new Set(),
      externallyCleared: new Set(),
    }).map((x) => x.id),
    ['BOS-2'],
  )
})
test('readyTickets: in-flight tickets are excluded, and a green-but-unmerged blocker still gates', () => {
  // Loop-termination invariant: greens stay in inFlight until merged, so
  // readyTickets must never re-list an in-flight ticket (double-spawn guard),
  // and a dependent stays blocked until the blocker is actually in `merged`.
  const g = buildGraph([t('BOS-1'), t('BOS-2', { blockedBy: ['BOS-1'] })])
  assert.deepEqual(
    readyTickets(g, {
      merged: new Set(),
      failed: new Set(),
      inFlight: new Set(['BOS-1']), // BOS-1 launched (possibly green, unmerged)
      externallyCleared: new Set(),
    }).map((x) => x.id),
    [], // BOS-1 not re-listed; BOS-2 still gated on the unmerged blocker
  )
})

test('readyTickets: a blocker outside the node set is gated by externallyCleared, never by merged', () => {
  // The wiring contract the skill prose describes: a done sibling is not a
  // graph node, so its edge is external — putting its id in `merged` must NOT
  // unblock the dependent; only `externallyCleared` does.
  const g = buildGraph([t('BOS-2', { blockedBy: ['BOS-1'] })]) // BOS-1 not a node
  assert.equal(
    readyTickets(g, {
      merged: new Set(['BOS-1']),
      failed: new Set(),
      inFlight: new Set(),
      externallyCleared: new Set(),
    }).length,
    0,
  )
  assert.equal(
    readyTickets(g, {
      merged: new Set(),
      failed: new Set(),
      inFlight: new Set(),
      externallyCleared: new Set(['BOS-1']),
    }).length,
    1,
  )
})

test('readyTickets: external blocker gates until externallyCleared', () => {
  const g = buildGraph([t('BOS-2', { blockedBy: ['BOS-999'] })])
  assert.equal(
    readyTickets(g, {
      merged: new Set(),
      failed: new Set(),
      inFlight: new Set(),
      externallyCleared: new Set(),
    }).length,
    0,
  )
  assert.equal(
    readyTickets(g, {
      merged: new Set(),
      failed: new Set(),
      inFlight: new Set(),
      externallyCleared: new Set(['BOS-999']),
    }).length,
    1,
  )
})
test('readyTickets: priority order then oldest createdAt', () => {
  const g = buildGraph([
    t('BOS-1', { priority: 3 }),
    t('BOS-2', { priority: 1 }),
    t('BOS-3', { priority: 1, createdAt: '2025-01-01T00:00:00Z' }),
  ])
  assert.deepEqual(
    readyTickets(g, {
      merged: new Set(),
      failed: new Set(),
      inFlight: new Set(),
      externallyCleared: new Set(),
    }).map((x) => x.id),
    ['BOS-3', 'BOS-2', 'BOS-1'],
  )
})
test('transitiveDependents: failure cascades through chain', () => {
  const g = buildGraph([
    t('BOS-1'),
    t('BOS-2', { blockedBy: ['BOS-1'] }),
    t('BOS-3', { blockedBy: ['BOS-2'] }),
  ])
  assert.deepEqual([...transitiveDependents(g, new Set(['BOS-1']))].sort(), ['BOS-2', 'BOS-3'])
})
test('readyTickets: failed ticket cascade-skips its dependents (single authority)', () => {
  const g = buildGraph([
    t('BOS-1'),
    t('BOS-2', { blockedBy: ['BOS-1'] }),
    t('BOS-3', { blockedBy: ['BOS-2'] }),
  ])
  const ready = readyTickets(g, {
    merged: new Set(),
    failed: new Set(['BOS-1']),
    inFlight: new Set(),
    externallyCleared: new Set(),
  })
  assert.deepEqual(
    ready.map((x) => x.id),
    [],
  )
})
test('readyTickets: cycle does not run forever — cyclic tickets are never ready', () => {
  const g = buildGraph([t('BOS-1', { blockedBy: ['BOS-2'] }), t('BOS-2', { blockedBy: ['BOS-1'] })])
  assert.equal(
    readyTickets(g, {
      merged: new Set(),
      failed: new Set(),
      inFlight: new Set(),
      externallyCleared: new Set(),
    }).length,
    0,
  )
})

// --- nextToMerge --------------------------------------------------------------

test('nextToMerge: prefers green whose blockers are all merged', () => {
  const g = buildGraph([t('BOS-1'), t('BOS-2', { blockedBy: ['BOS-1'] })])
  assert.equal(nextToMerge([{ id: 'BOS-2' }, { id: 'BOS-1' }], g, new Set()), 'BOS-1')
})
test('nextToMerge: returns null when no green is mergeable', () => {
  const g = buildGraph([t('BOS-0'), t('BOS-1', { blockedBy: ['BOS-0'] })])
  assert.equal(nextToMerge([{ id: 'BOS-1' }], g, new Set()), null)
})
test('nextToMerge: ties break on priority then oldest createdAt', () => {
  const g = buildGraph([
    t('BOS-1', { priority: 3 }),
    t('BOS-2', { priority: 1, createdAt: '2026-02-01T00:00:00Z' }),
    t('BOS-3', { priority: 1, createdAt: '2025-01-01T00:00:00Z' }),
  ])
  assert.equal(
    nextToMerge([{ id: 'BOS-1' }, { id: 'BOS-2' }, { id: 'BOS-3' }], g, new Set()),
    'BOS-3',
  )
})

// --- parseEpicArgs --------------------------------------------------------------

test('parseEpicArgs: parent, list, parallel override, agent override, bounds', () => {
  assert.deepEqual(parseEpicArgs(['BOS-100']), {
    parentId: 'BOS-100',
    ids: [],
    parallel: 4,
    agent: 'claude',
    assumeCleared: [],
    assumeClearedAndMerge: [],
  })
  assert.deepEqual(parseEpicArgs(['BOS-1', 'BOS-2']).ids, ['BOS-1', 'BOS-2'])
  assert.equal(parseEpicArgs(['BOS-1', 'BOS-2']).parentId, null)
  assert.equal(parseEpicArgs(['BOS-100', '--parallel', '2']).parallel, 2)
  assert.equal(parseEpicArgs(['BOS-100', '--agent', 'codex']).agent, 'codex')
  assert.throws(() => parseEpicArgs(['--parallel', '9', 'BOS-1']))
  assert.throws(() => parseEpicArgs([]))
})
test('parseEpicArgs: rejects out-of-bounds and non-integer --parallel', () => {
  assert.throws(() => parseEpicArgs(['BOS-1', '--parallel', '0']))
  assert.throws(() => parseEpicArgs(['BOS-1', '--parallel', '2.5']))
  assert.throws(() => parseEpicArgs(['BOS-1', '--parallel', 'abc']))
})
test('parseEpicArgs: rejects a positional that does not look like a ticket id (typo guard)', () => {
  assert.throws(() => parseEpicArgs(['BOS-1', 'prallel']))
})
test('parseEpicArgs: rejects --agent with a missing or flag-like value', () => {
  assert.throws(() => parseEpicArgs(['BOS-1', '--agent']))
  assert.throws(() => parseEpicArgs(['BOS-1', '--agent', '--parallel']))
})

// --- parseTicketRef / Linear URL parsing ----------------------------------------

test('parseTicketRef: accepts a bare id or a pasted Linear issue URL', () => {
  assert.equal(parseTicketRef('BOS-179'), 'BOS-179')
  assert.equal(parseTicketRef('  BOS-179  '), 'BOS-179')
  assert.equal(
    parseTicketRef('https://linear.app/bossanova-dev/issue/BOS-179/make-bs-epic-work-headlessly'),
    'BOS-179',
  )
  assert.equal(parseTicketRef('https://linear.app/acme/issue/ACME-42'), 'ACME-42')
  assert.equal(parseTicketRef('https://linear.app/acme/issue/ACME-42?foo=bar'), 'ACME-42')
  assert.equal(parseTicketRef('not-a-ticket'), null)
  assert.equal(parseTicketRef('https://example.com/issue/BOS-1'), null)
  assert.equal(parseTicketRef(undefined), null)
})

test('parseEpicArgs: accepts a pasted Linear issue URL as the parent', () => {
  const parsed = parseEpicArgs(['https://linear.app/bossanova-dev/issue/BOS-157/some-epic-slug'])
  assert.equal(parsed.parentId, 'BOS-157')
  assert.equal(parsed.ids.length, 0)
})

test('parseEpicArgs: accepts Linear URLs mixed with ids in an explicit list', () => {
  const parsed = parseEpicArgs(['BOS-1', 'https://linear.app/bossanova-dev/issue/BOS-2/slug'])
  assert.deepEqual(parsed.ids, ['BOS-1', 'BOS-2'])
  assert.equal(parsed.parentId, null)
})

// --- --assume-cleared vs --assume-cleared-and-merge (split override) --------------

test('parseEpicArgs: --assume-cleared and --assume-cleared-and-merge are distinct, repeatable', () => {
  const parsed = parseEpicArgs([
    'BOS-100',
    '--assume-cleared',
    'BOS-9',
    '--assume-cleared-and-merge',
    'BOS-10',
    '--assume-cleared',
    'https://linear.app/bossanova-dev/issue/BOS-11/slug',
  ])
  assert.deepEqual(parsed.assumeCleared, ['BOS-9', 'BOS-11'])
  assert.deepEqual(parsed.assumeClearedAndMerge, ['BOS-10'])
})

test('parseEpicArgs: --assume-cleared* validate their value', () => {
  assert.throws(() => parseEpicArgs(['BOS-1', '--assume-cleared']))
  assert.throws(() => parseEpicArgs(['BOS-1', '--assume-cleared', '--parallel']))
  assert.throws(() => parseEpicArgs(['BOS-1', '--assume-cleared', 'notaticket']))
  assert.throws(() => parseEpicArgs(['BOS-1', '--assume-cleared-and-merge']))
  assert.throws(() => parseEpicArgs(['BOS-1', '--assume-cleared-and-merge', 'notaticket']))
})

// --- mergeBlockedExternalBlockers (merge-time external re-check) ------------------

test('mergeBlockedExternalBlockers: an open external blocker gates the merge', () => {
  const g = buildGraph([t('BOS-2', { blockedBy: ['BOS-999'] })]) // BOS-999 is external
  // BOS-999 is still In Progress at merge time → merge is blocked.
  const open = mergeBlockedExternalBlockers(g.nodes.get('BOS-2'), g, {
    blockerStateTypes: new Map([['BOS-999', 'started']]),
  })
  assert.deepEqual(open, ['BOS-999'])
})

test('mergeBlockedExternalBlockers: a now-resolved external blocker no longer gates', () => {
  const g = buildGraph([t('BOS-2', { blockedBy: ['BOS-999'] })])
  const open = mergeBlockedExternalBlockers(g.nodes.get('BOS-2'), g, {
    blockerStateTypes: new Map([['BOS-999', 'completed']]),
  })
  assert.deepEqual(open, [])
})

test('mergeBlockedExternalBlockers: --assume-cleared-and-merge overrides an open gate', () => {
  const g = buildGraph([t('BOS-2', { blockedBy: ['BOS-999'] })])
  const open = mergeBlockedExternalBlockers(g.nodes.get('BOS-2'), g, {
    clearedForMerge: new Set(['BOS-999']),
    blockerStateTypes: new Map([['BOS-999', 'started']]), // still open, but cleared for merge
  })
  assert.deepEqual(open, [])
})

test('mergeBlockedExternalBlockers: an unknown-state blocker is fail-closed (still open)', () => {
  const g = buildGraph([t('BOS-2', { blockedBy: ['BOS-999'] })])
  const open = mergeBlockedExternalBlockers(g.nodes.get('BOS-2'), g, {
    blockerStateTypes: new Map(), // no fetched state
  })
  assert.deepEqual(open, ['BOS-999'])
})

test('mergeBlockedExternalBlockers: in-epic blockers are never treated as external gates', () => {
  const g = buildGraph([t('BOS-1'), t('BOS-2', { blockedBy: ['BOS-1'] })]) // BOS-1 is in-epic
  const open = mergeBlockedExternalBlockers(g.nodes.get('BOS-2'), g, {
    blockerStateTypes: new Map([['BOS-1', 'started']]),
  })
  assert.deepEqual(open, [])
})

// --- classifyChildLiveness (BOS-997: child liveness routing) ---------------
//
// Synthetic driver payloads. The driver reads the tracked chat status and
// classifies the last message; this pure helper only routes plain facts.
// Unmoved head SHA, stale transcript/activity, and agent-stalled attention are
// corroborating reasons only. Missing evidence fails to unknown/investigate.

const childParked = {
  headShaMoved: false,
  activityStale: true,
  attentionReasons: ['ATTENTION_REASON_AGENT_STALLED'],
}

const childLivenessCases = [
  {
    name: 'WAITING parked child is alive even with unmoved head and stale activity',
    input: { ...childParked, chatStatus: 'WAITING' },
    verdict: 'alive',
    action: 'hold',
    reasons: ['head-sha-unmoved', 'activity-stale', 'attention-agent-stalled'],
  },
  {
    name: 'proto CHAT_STATUS_WAITING parked child is alive',
    input: { ...childParked, chatStatus: 'CHAT_STATUS_WAITING' },
    verdict: 'alive',
    action: 'hold',
  },
  {
    name: 'numeric CHAT_STATUS_WAITING parked child is alive',
    input: { ...childParked, chatStatus: 6 },
    verdict: 'alive',
    action: 'hold',
  },
  {
    name: 'WORKING parked child is alive even with unmoved head and stale activity',
    input: { ...childParked, chatStatus: 'WORKING' },
    verdict: 'alive',
    action: 'hold',
  },
  {
    name: 'unmoved head plus stale activity alone is unknown',
    input: { headShaMoved: false, activityStale: true },
    verdict: 'unknown',
    action: 'investigate',
  },
  {
    name: 'LIMITED chat status is a usage-limit resume lane',
    input: { chatStatus: 'LIMITED' },
    verdict: 'environmental-death',
    action: 'resume',
  },
  {
    name: 'proto CHAT_STATUS_LIMITED is a usage-limit resume lane',
    input: { chatStatus: 'CHAT_STATUS_LIMITED' },
    verdict: 'environmental-death',
    action: 'resume',
  },
  {
    name: 'numeric CHAT_STATUS_LIMITED is a usage-limit resume lane',
    input: { chatStatus: 5 },
    verdict: 'environmental-death',
    action: 'resume',
  },
  {
    name: 'usage-cap last message is a usage-limit resume lane',
    input: { lastMessageKind: 'usage-limit' },
    verdict: 'environmental-death',
    action: 'resume',
  },
  {
    name: 'transient API last message resumes instead of repairing',
    input: { lastMessageKind: 'transient-api-error' },
    verdict: 'environmental-death',
    action: 'resume',
  },
  {
    name: 'BLOCKED with agent conclusion repairs',
    input: { sessionState: 'BLOCKED', lastMessageKind: 'agent-conclusion' },
    verdict: 'agent-blocked',
    action: 'repair',
  },
  {
    name: 'proto SESSION_STATE_BLOCKED with agent conclusion repairs',
    input: { sessionState: 'SESSION_STATE_BLOCKED', lastMessageKind: 'agent-conclusion' },
    verdict: 'agent-blocked',
    action: 'repair',
  },
  {
    name: 'numeric SESSION_STATE_BLOCKED with agent conclusion repairs',
    input: { sessionState: 10, lastMessageKind: 'agent-conclusion' },
    verdict: 'agent-blocked',
    action: 'repair',
  },
  {
    name: 'BLOCKED without agent conclusion is unknown',
    input: { sessionState: 'BLOCKED' },
    verdict: 'unknown',
    action: 'investigate',
  },
  {
    name: 'agent-stalled attention alone is unknown and never repair',
    input: { attentionReasons: ['agent-stalled'] },
    verdict: 'unknown',
    action: 'investigate',
    reasons: ['attention-agent-stalled'],
  },
  {
    name: 'unreadable chat status is unknown',
    input: { chatStatusReadable: false },
    verdict: 'unknown',
    action: 'investigate',
    reasons: ['chat-status-unreadable'],
  },
  {
    name: 'UNSPECIFIED chat status is unknown',
    input: { chatStatus: 'UNSPECIFIED' },
    verdict: 'unknown',
    action: 'investigate',
  },
  {
    name: 'proto CHAT_STATUS_UNSPECIFIED is unknown',
    input: { chatStatus: 'CHAT_STATUS_UNSPECIFIED' },
    verdict: 'unknown',
    action: 'investigate',
  },
  {
    name: 'numeric CHAT_STATUS_UNSPECIFIED is unknown',
    input: { chatStatus: 0 },
    verdict: 'unknown',
    action: 'investigate',
  },
  {
    name: 'wall-clock expiry fail-isolates and never repairs',
    input: { wallClockExceeded: true, chatStatus: 'IDLE' },
    verdict: 'wall-clock-expired',
    action: 'fail-isolate',
  },
]

for (const { name, input, verdict, action, reasons = [] } of childLivenessCases) {
  test(`classifyChildLiveness: ${name}`, () => {
    const result = classifyChildLiveness(input)
    assert.equal(result.verdict, verdict)
    assert.equal(result.action, action)
    assert.ok(CHILD_LIVENESS_VERDICTS.has(result.verdict), `unknown verdict: ${result.verdict}`)
    assert.ok(CHILD_LIVENESS_ACTIONS.has(result.action), `unknown action: ${result.action}`)
    for (const reason of reasons) {
      assert.ok(result.reasons.includes(reason), `missing reason: ${reason}`)
    }
  })
}

test('classifyChildLiveness: unknown verdict never pairs with repair action', () => {
  for (const { name, input } of childLivenessCases) {
    const result = classifyChildLiveness(input)
    if (result.verdict === 'unknown') {
      assert.notEqual(result.action, 'repair', name)
    }
  }
  assert.notEqual(classifyChildLiveness().action, 'repair', 'missing evidence')
})

test('classifyChildLiveness: is pure and does not mutate the payload', () => {
  const payload = {
    chatStatus: 'WAITING',
    headShaMoved: false,
    activityStale: true,
    attentionReasons: ['agent-stalled'],
  }
  const snapshot = JSON.stringify(payload)
  assert.deepEqual(classifyChildLiveness(payload), classifyChildLiveness(payload))
  assert.equal(JSON.stringify(payload), snapshot, 'must not mutate its argument')
})

// --- classifyRepairLease (BOS-520: frozen-repair-lease escape) -------------
//
// Synthetic `get_session` payloads. The driver reads `repair_active`,
// `repair_stalled_at`, `last_repair_head_sha` off the session and pairs them
// with its own previous-poll snapshot + the tracked repair chat's activity
// timestamp. `'stalled'` is what makes Phase 3c terminate, so every branch is
// pinned — especially the fail-toward-'active' cases, where a false 'stalled'
// would burn a repair round or fail-isolate a healthy ticket.

const NOW = 1_770_000_000_000
// A live lease making progress: head advanced since the last poll, chat is chatty.
const lease = (over = {}) => ({
  repairActive: true,
  lastRepairHeadSha: 'bbbb222',
  prevLastRepairHeadSha: 'aaaa111',
  repairChatLastOutputAtMs: NOW - 30_000,
  nowMs: NOW,
  ...over,
})

test('classifyRepairLease: no lease held and no daemon stall field → none', () => {
  assert.equal(classifyRepairLease(lease({ repairActive: false })), 'none')
  assert.equal(classifyRepairLease({}), 'none')
  assert.equal(classifyRepairLease(), 'none')
})

test('classifyRepairLease: active and progressing → active', () => {
  assert.equal(classifyRepairLease(lease()), 'active')
})

test('classifyRepairLease: head advanced but chat silent → active (still working)', () => {
  // Only one leg of the driver evidence: the repairer committed, so the lease
  // is alive even though the chat has not spoken for a while.
  assert.equal(
    classifyRepairLease(lease({ repairChatLastOutputAtMs: NOW - 60 * 60_000 })),
    'active',
  )
})

test('classifyRepairLease: chatting but head frozen → active (thinking, not dead)', () => {
  // The other single leg: no commit yet, but the chat is producing output.
  assert.equal(classifyRepairLease(lease({ prevLastRepairHeadSha: 'bbbb222' })), 'active')
})

test('classifyRepairLease: repair_stalled_at set → stalled (the daemon decided)', () => {
  // Takes precedence over every other signal, including a chatty, advancing lease.
  assert.equal(classifyRepairLease(lease({ repairStalledAt: '2026-07-25T12:00:00Z' })), 'stalled')
  // …and even once the daemon has already dropped repair_active.
  assert.equal(
    classifyRepairLease(lease({ repairActive: false, repairStalledAt: '2026-07-25T12:00:00Z' })),
    'stalled',
  )
})

test('classifyRepairLease: frozen head + stale chat → stalled from driver evidence alone', () => {
  // The MAD-652 shape on a daemon with no repair_stalled_at field at all.
  assert.equal(
    classifyRepairLease(
      lease({
        prevLastRepairHeadSha: 'bbbb222',
        repairChatLastOutputAtMs: NOW - 21 * 60_000,
      }),
    ),
    'stalled',
  )
})

test('classifyRepairLease: stall window boundary is inclusive', () => {
  const frozen = { prevLastRepairHeadSha: 'bbbb222' }
  // Exactly at the window → stalled.
  assert.equal(
    classifyRepairLease(
      lease({ ...frozen, repairChatLastOutputAtMs: NOW - REPAIR_STALL_WINDOW_MS }),
    ),
    'stalled',
  )
  // One millisecond short → still active.
  assert.equal(
    classifyRepairLease(
      lease({ ...frozen, repairChatLastOutputAtMs: NOW - REPAIR_STALL_WINDOW_MS + 1 }),
    ),
    'active',
  )
  // An explicit window overrides the default in both directions.
  assert.equal(
    classifyRepairLease(
      lease({ ...frozen, repairChatLastOutputAtMs: NOW - 60_000, stallWindowMs: 30_000 }),
    ),
    'stalled',
  )
})

test('classifyRepairLease: missing evidence fails toward active, never stalled', () => {
  const frozenAndSilent = {
    prevLastRepairHeadSha: 'bbbb222',
    repairChatLastOutputAtMs: NOW - 21 * 60_000,
  }
  for (const missing of [
    { lastRepairHeadSha: undefined, prevLastRepairHeadSha: undefined },
    { lastRepairHeadSha: '', prevLastRepairHeadSha: '' }, // no SHA reported yet
    { repairChatLastOutputAtMs: undefined }, // chat never tracked / no timestamp
    { repairChatLastOutputAtMs: null },
    { nowMs: undefined }, // caller forgot the clock
  ]) {
    assert.equal(
      classifyRepairLease(lease({ ...frozenAndSilent, ...missing })),
      'active',
      `absent evidence must not read as a dead lease: ${JSON.stringify(missing)}`,
    )
  }
})

test('classifyRepairLease: is pure — the same payload classifies identically twice', () => {
  const payload = lease({
    prevLastRepairHeadSha: 'bbbb222',
    repairChatLastOutputAtMs: NOW - 21 * 60_000,
  })
  const snapshot = JSON.stringify(payload)
  assert.equal(classifyRepairLease(payload), classifyRepairLease(payload))
  assert.equal(JSON.stringify(payload), snapshot, 'must not mutate its argument')
})

// --- resolveStateRole / resolvePlannedState (BOS-524) ----------------------
// A repo can be fully functional through a vendored tracker adapter that already
// knows its own state names and carries no trackerConfig.<tracker>.states block;
// resolving from configuration alone self-BLOCKs such a repo for a value the adapter
// was holding. Pin the adapter-first order, the config fallback, and the fail-closed
// null that makes the caller BLOCK naming both probed sources.

test('resolveStateRole: the adapter alone resolves the role (no trackerConfig states needed)', () => {
  assert.equal(
    resolveStateRole({ role: 'planned', adapterStates: { planned: 'Scheduled' } }),
    'Scheduled',
  )
  // Explicitly absent config, not merely omitted — the no-trackerConfig repo.
  assert.equal(
    resolveStateRole({
      role: 'inReview',
      adapterStates: { inReview: 'Under Review' },
      trackerConfigStates: null,
    }),
    'Under Review',
  )
})

test('resolveStateRole: falls back to trackerConfig when the adapter has no states', () => {
  for (const adapterStates of [undefined, null, {}]) {
    assert.equal(
      resolveStateRole({
        role: 'planned',
        adapterStates,
        trackerConfigStates: { planned: 'Ready' },
      }),
      'Ready',
      `config must answer when adapterStates = ${JSON.stringify(adapterStates)}`,
    )
  }
})

test('resolveStateRole: the adapter WINS when both sources carry the role', () => {
  assert.equal(
    resolveStateRole({
      role: 'inProgress',
      adapterStates: { inProgress: 'From Adapter' },
      trackerConfigStates: { inProgress: 'From Config' },
    }),
    'From Adapter',
  )
})

test('resolveStateRole: neither source ⇒ null (the caller fails closed)', () => {
  assert.equal(resolveStateRole({ role: 'planned' }), null)
  assert.equal(
    resolveStateRole({ role: 'planned', adapterStates: {}, trackerConfigStates: {} }),
    null,
  )
  // A role the adapter answers null for, exactly as the states() contract requires.
  assert.equal(
    resolveStateRole({
      role: 'planned',
      adapterStates: { planned: null },
      trackerConfigStates: {},
    }),
    null,
  )
  assert.equal(resolveStateRole(), null, 'a bare call must not throw')
})

test('resolveStateRole: a blank adapter value falls THROUGH to config, never wins', () => {
  // A whitespace-only state name is exactly as unusable as an absent one; letting it
  // win would BLOCK a repo whose config held a perfectly good name.
  for (const blank of ['', '   ', '\n', '\t ']) {
    assert.equal(
      resolveStateRole({
        role: 'planned',
        adapterStates: { planned: blank },
        trackerConfigStates: { planned: 'Ready' },
      }),
      'Ready',
      `adapter value ${JSON.stringify(blank)} must fall through`,
    )
    // ...and with no config behind it, a blank resolves null rather than ''.
    assert.equal(
      resolveStateRole({ role: 'planned', adapterStates: { planned: blank } }),
      null,
      `adapter value ${JSON.stringify(blank)} must resolve null, not itself`,
    )
  }
})

test('resolveStateRole: malformed sources resolve null instead of throwing', () => {
  // The adapter probe is a CLI read the caller may hand over unparsed/garbled; a throw
  // here would defeat the very fallback this helper exists to perform.
  for (const bad of ['nope', 42, [], true, () => {}]) {
    assert.equal(
      resolveStateRole({ role: 'planned', adapterStates: bad, trackerConfigStates: bad }),
      null,
      `malformed source ${JSON.stringify(bad)} must resolve null`,
    )
    // A malformed adapter source must still let a good config answer.
    assert.equal(
      resolveStateRole({
        role: 'planned',
        adapterStates: bad,
        trackerConfigStates: { planned: 'Ready' },
      }),
      'Ready',
    )
  }
  // A non-string state value is not a name.
  assert.equal(resolveStateRole({ role: 'planned', adapterStates: { planned: 7 } }), null)
})

test('resolvePlannedState delegates to resolveStateRole for the planned role', () => {
  const adapterStates = { planned: 'Scheduled', inReview: 'Under Review' }
  const trackerConfigStates = { planned: 'Ready', inReview: 'Reviewing' }
  assert.equal(resolvePlannedState({ adapterStates }), 'Scheduled')
  assert.equal(resolvePlannedState({ trackerConfigStates }), 'Ready')
  assert.equal(resolvePlannedState({ adapterStates, trackerConfigStates }), 'Scheduled')
  assert.equal(resolvePlannedState({}), null)
  assert.equal(resolvePlannedState(), null)
  // It must read the `planned` role specifically, not the first/any entry.
  assert.equal(resolvePlannedState({ adapterStates: { inReview: 'Under Review' } }), null)
  assert.equal(
    resolvePlannedState({
      adapterStates,
      trackerConfigStates,
    }),
    resolveStateRole({ role: 'planned', adapterStates, trackerConfigStates }),
  )
})

test('a resolved planned state feeds classifyTickets, whose empty-state throw stays the backstop', () => {
  // The helper is the resolution; classifyTickets keeps its own throw as the library
  // backstop, so a caller that skips the BLOCK still cannot schedule unplanned work.
  const planned = resolvePlannedState({ adapterStates: { planned: 'Todo' } })
  assert.equal(classifyTickets([t('BOS-1')], planned).eligible.length, 1)
  assert.throws(
    () => classifyTickets([t('BOS-1')], resolvePlannedState({})),
    /plannedState .* is required/,
  )
})
