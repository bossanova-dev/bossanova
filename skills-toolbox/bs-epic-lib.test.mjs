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

test('normalizeTicket: planUrl falls back to a links array when attachments is absent', () => {
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
})

// --- classifyTickets --------------------------------------------------------

test('classifyTickets: planned-state + agent-friendly + plan is eligible', () => {
  const { eligible } = classifyTickets([t('BOS-1')], 'Todo')
  assert.equal(eligible.length, 1)
})
test('classifyTickets: needs-human, missing plan, not-planned, In Progress all skip with reasons', () => {
  const { skipped } = classifyTickets(
    [
      t('BOS-1', { labels: ['needs-human'] }),
      t('BOS-2', { planUrl: null }),
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
