import { test } from 'node:test'
import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

import {
  buildLinearOperationMap,
  createLinearAdapter,
  LIST_PLANNED_QUERY,
  READ_DESCRIPTION_QUERY,
  linearReadDescription,
  linearSelectPlanned,
} from './linear.mjs'
import { buildIssueCountFilter } from '../linear-gate-lib.mjs'
import { assertConforms, REQUIRED_TRACKER_OPERATIONS, TRACKER_STATE_ROLES } from './adapter.mjs'
import { TRACKER_CREDENTIALS_MISSING } from './adapter-core.mjs'
import { formatClaimComment } from '../linear-claim.mjs'
import { loadSkillConfig, plannedSelectionQuery, trackerConfigFor } from '../skill-config.mjs'

// This repo's own root, so the states() tests read a real config regardless of the
// cwd the test runner happens to be launched from.
const repoRoot = path.dirname(path.dirname(path.dirname(fileURLToPath(import.meta.url))))

// A fetchImpl that records the POST and returns a canned GraphQL payload.
function fakeFetch(nodes) {
  const calls = []
  const impl = async (url, init) => {
    calls.push({ url, body: JSON.parse(init.body), headers: init.headers })
    return {
      ok: true,
      json: async () => ({ data: { issues: { nodes, pageInfo: { hasNextPage: false } } } }),
    }
  }
  return { impl, calls }
}

test('the Linear adapter conforms to the interface', () => {
  const adapter = createLinearAdapter({ apiKey: 'k', fetchImpl: async () => {} })
  assert.equal(adapter.tracker, 'linear')
  assert.doesNotThrow(() => assertConforms(adapter))
})

test('hasWork delegates to runLinearGate and preserves the auth header', async () => {
  const { impl, calls } = fakeFetch([{ id: 'iss_1' }])
  const adapter = createLinearAdapter({ apiKey: 'secret-key', fetchImpl: impl })
  const has = await adapter.hasWork({ state: 'Unplanned' })
  assert.equal(has, true)
  // Auth header carries the key directly, no "Bearer" prefix.
  assert.equal(calls[0].headers.Authorization, 'secret-key')
})

test('a custom endpoint threads through to the fetchImpl URL', async () => {
  const { impl, calls } = fakeFetch([{ id: 'iss_1' }])
  const adapter = createLinearAdapter({
    apiKey: 'k',
    fetchImpl: impl,
    endpoint: 'https://linear.example/graphql',
  })
  await adapter.hasWork({ state: 'Unplanned' })
  assert.equal(calls[0].url, 'https://linear.example/graphql')
})

test('hasWork returns false when no issue matches', async () => {
  const { impl } = fakeFetch([])
  const adapter = createLinearAdapter({ apiKey: 'k', fetchImpl: impl })
  assert.equal(await adapter.hasWork({ state: 'Unplanned' }), false)
})

test('hasUnblockedWork delegates to runUnblockedGate', async () => {
  const { impl } = fakeFetch([
    { id: 'iss_1', identifier: 'BOS-1', inverseRelations: { nodes: [] } },
  ])
  const adapter = createLinearAdapter({ apiKey: 'k', fetchImpl: impl })
  assert.equal(await adapter.hasUnblockedWork({ state: 'Todo', label: 'agent-friendly' }), true)
})

// BOS-1292: the adapter is how the shipped cron gate reaches the gate libraries at all, so a
// selector it drops is a selector no caller can use without bypassing the seam. Each test reads
// the emitted GraphQL filter, not the adapter's arguments, so a forward that stops at the
// boundary still fails.

// A fetch that answers the viewer lookup separately from the gate query and records every body.
function selectorFetch(viewerId = 'usr_me') {
  const bodies = []
  const impl = async (url, init) => {
    const body = JSON.parse(init.body)
    bodies.push(body)
    return {
      ok: true,
      json: async () =>
        body.query.includes('viewer')
          ? { data: { viewer: { id: viewerId } } }
          : {
              data: {
                issues: {
                  nodes: [{ id: 'iss_1', identifier: 'BOS-1', inverseRelations: { nodes: [] } }],
                  pageInfo: { hasNextPage: false },
                },
              },
            },
    }
  }
  impl.bodies = bodies
  return impl
}

test('hasWork forwards assignee and creator into the emitted filter', async () => {
  const impl = selectorFetch('usr_owner')
  const adapter = createLinearAdapter({ apiKey: 'k', fetchImpl: impl })
  await adapter.hasWork({ state: 'Unplanned', assignee: 'me', creator: 'usr_c' })
  assert.deepEqual(impl.bodies[impl.bodies.length - 1].variables.filter, {
    state: { name: { eq: 'Unplanned' } },
    assignee: { id: { eq: 'usr_owner' } },
    creator: { id: { eq: 'usr_c' } },
  })
})

test('hasUnblockedWork forwards all three selectors into the emitted filter', async () => {
  const impl = selectorFetch('usr_owner')
  const adapter = createLinearAdapter({ apiKey: 'k', fetchImpl: impl })
  await adapter.hasUnblockedWork({
    state: 'Todo',
    label: 'agent-friendly',
    assignee: 'usr_a',
    creator: 'usr_c',
    assigneeOrCreator: 'me',
  })
  assert.deepEqual(impl.bodies[impl.bodies.length - 1].variables.filter, {
    state: { name: { eq: 'Todo' } },
    labels: { name: { eq: 'agent-friendly' } },
    assignee: { id: { eq: 'usr_a' } },
    creator: { id: { eq: 'usr_c' } },
    or: [{ assignee: { id: { eq: 'usr_owner' } } }, { creator: { id: { eq: 'usr_owner' } } }],
  })
})

// BOS-1292 must-fix: `hasWork` forwarded only {assignee, creator}, so an `assigneeOrCreator`
// handed to the adapter vanished and the gate widened to the whole board. Read from the emitted
// filter so the forward is proven all the way to the wire.
test('hasWork forwards assigneeOrCreator into the emitted filter', async () => {
  const impl = selectorFetch('usr_owner')
  const adapter = createLinearAdapter({ apiKey: 'k', fetchImpl: impl })
  await adapter.hasWork({ state: 'Unplanned', label: 'agent-friendly', assigneeOrCreator: 'me' })
  assert.deepEqual(impl.bodies[impl.bodies.length - 1].variables.filter, {
    state: { name: { eq: 'Unplanned' } },
    labels: { name: { eq: 'agent-friendly' } },
    or: [{ assignee: { id: { eq: 'usr_owner' } } }, { creator: { id: { eq: 'usr_owner' } } }],
  })
})

// The adapter half of the inertness pin: the shipped cron gate calls hasUnblockedWork with
// state and label only, and must keep emitting exactly what it emitted before this ticket.
test('the adapter gates emit the pre-BOS-1292 filter when given no selector', async () => {
  const impl = selectorFetch()
  const adapter = createLinearAdapter({ apiKey: 'k', fetchImpl: impl })
  await adapter.hasUnblockedWork({ state: 'Todo', label: 'agent-friendly' })
  assert.equal(impl.bodies.length, 1, 'no stray viewer lookup through the adapter either')
  assert.deepEqual(impl.bodies[0].variables.filter, {
    state: { name: { eq: 'Todo' } },
    labels: { name: { eq: 'agent-friendly' } },
  })
})

test('resolveClaim reproduces first-writer-wins', () => {
  const adapter = createLinearAdapter({ apiKey: 'k', fetchImpl: async () => {} })
  const mine = 'a'.repeat(32)
  const theirs = 'b'.repeat(32)
  const comments = [
    { body: formatClaimComment(theirs), createdAt: '2026-01-01T00:00:02Z' },
    { body: formatClaimComment(mine), createdAt: '2026-01-01T00:00:01Z' },
  ]
  assert.equal(adapter.resolveClaim(comments, mine), true)
  assert.equal(adapter.resolveClaim(comments, theirs), false)
})

test('resolveClaim forwards liveness evidence to claim arbitration', () => {
  const adapter = createLinearAdapter({ apiKey: 'k', fetchImpl: async () => {} })
  const mine = 'a'.repeat(32)
  const theirs = 'b'.repeat(32)
  const comments = [
    { body: formatClaimComment(theirs, 'dead-session'), createdAt: '2026-01-01T00:00:01Z' },
    { body: formatClaimComment(mine), createdAt: '2026-01-01T00:00:02Z' },
  ]
  assert.equal(
    adapter.resolveClaim(comments, mine, {
      now: '2026-01-01T00:10:00Z',
      inactiveAfterMs: 60_000,
      sessions: {
        'dead-session': { lastActivityAt: '2026-01-01T00:00:00Z' },
      },
    }),
    true,
  )
})

test('resolveClaim returns null when liveness forfeits every claim', () => {
  const adapter = createLinearAdapter({ apiKey: 'k', fetchImpl: async () => {} })
  const mine = 'a'.repeat(32)
  const comments = [
    { body: formatClaimComment(mine, 'dead-session'), createdAt: '2026-01-01T00:00:01Z' },
  ]
  assert.equal(
    adapter.resolveClaim(comments, mine, {
      now: '2026-01-01T00:10:00Z',
      inactiveAfterMs: 60_000,
      sessions: {
        'dead-session': { lastActivityAt: '2026-01-01T00:00:00Z' },
      },
    }),
    null,
  )
})

test('normalizeTicket flattens a raw GraphQL issue', () => {
  const adapter = createLinearAdapter({ apiKey: 'k', fetchImpl: async () => {} })
  const ticket = adapter.normalizeTicket({
    identifier: 'BOS-1',
    title: 'x',
    priority: 2,
    createdAt: '2026-01-01T00:00:00Z',
    state: { name: 'Todo', type: 'unstarted' },
    labels: { nodes: [{ name: 'agent-friendly' }] },
    inverseRelations: { nodes: [] },
  })
  assert.equal(ticket.id, 'BOS-1')
  assert.equal(ticket.stateName, 'Todo')
  assert.deepEqual(ticket.labels, ['agent-friendly'])
})

test('buildLinearOperationMap derives agent-driven MCP tools from the configured server', () => {
  const operationMap = buildLinearOperationMap('acme-tracker')
  assert.equal(operationMap.selectPlanned.tool, 'mcp__acme-tracker__list_issues')
  assert.equal(operationMap.moveState.tool, 'mcp__acme-tracker__save_issue')
  assert.equal(operationMap.readComments.tool, 'mcp__acme-tracker__list_comments')
  assert.match(
    operationMap.readComments.summary,
    /\{id, body, createdAt\}/,
    'readComments must document that it surfaces comment ids — updateComment is unreachable without them',
  )
  assert.equal(operationMap.writeComment.tool, 'mcp__acme-tracker__save_comment')
  assert.equal(operationMap.updateComment.tool, 'mcp__acme-tracker__save_comment')
  assert.equal(operationMap.readLabels.tool, 'mcp__acme-tracker__get_issue')
  assert.equal(operationMap.extractImages.tool, 'mcp__acme-tracker__extract_images')
  assert.equal(operationMap.createLabel.tool, 'mcp__acme-tracker__create_issue_label')
  assert.equal(operationMap.setPriorityEstimate.tool, 'mcp__acme-tracker__save_issue')
  assert.equal(operationMap.appendDependency.tool, 'mcp__acme-tracker__save_issue')
  assert.equal(operationMap.appendRelatedTo.tool, 'mcp__acme-tracker__save_issue')
  assert.match(
    operationMap.appendRelatedTo.summary,
    /relatedTo/,
    'appendRelatedTo must document the relatedTo payload — it shares save_issue with appendDependency, so the summary is the only thing distinguishing a non-blocking edge from a blocking one',
  )
  assert.equal(
    operationMap.preparePlanAttachment.tool,
    'mcp__acme-tracker__prepare_attachment_upload',
  )
  assert.equal(
    operationMap.finalizePlanAttachment.tool,
    'mcp__acme-tracker__create_attachment_from_upload',
  )
  assert.equal(operationMap.readPlanAttachment.tool, 'mcp__acme-tracker__get_attachment')
  assert.equal(operationMap.deletePlanAttachment.tool, 'mcp__acme-tracker__delete_attachment')
  assert.equal(operationMap.writeDescription.tool, 'mcp__acme-tracker__save_issue')
  for (const key of REQUIRED_TRACKER_OPERATIONS) {
    assert.match(operationMap[key].tool, /^mcp__acme-tracker__/)
  }
})

test('buildLinearOperationMap updateComment summary mentions updating in place and the id argument', () => {
  const operationMap = buildLinearOperationMap('acme-tracker')
  assert.match(operationMap.updateComment.summary, /\bid\b/)
  assert.match(operationMap.updateComment.summary, /updates.*in place/i)
})

test('buildLinearOperationMap writeDescription names the description argument key (BOS-1198)', () => {
  // TrackerOperation carries no argument-shape field, so the summary is the ONLY place
  // the descriptor emitter's argument key is stated. writeDescription shares save_issue
  // with four other ops, so the tool name alone distinguishes nothing: a summary that
  // named some other key would emit a save the tracker accepts and that leaves the
  // description untouched.
  const operationMap = buildLinearOperationMap('acme-tracker')
  assert.match(operationMap.writeDescription.summary, /^\{id, description\}/)
  assert.match(
    operationMap.writeDescription.summary,
    /from a file/,
    'the summary must state that the bytes come from a file — that is the capability, not a detail',
  )
})

test('buildLinearOperationMap contains the single-comment progress protocol trio', () => {
  const operationMap = buildLinearOperationMap('acme-tracker')
  for (const key of ['readComments', 'writeComment', 'updateComment']) {
    assert.equal(typeof operationMap[key].tool, 'string')
    assert.notEqual(operationMap[key].tool, '')
    assert.equal(typeof operationMap[key].summary, 'string')
    assert.notEqual(operationMap[key].summary, '')
  }
})

test('createLinearAdapter builds its operation map from trackerConfig.mcpServer', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'tracker-linear-mcp-server-'))
  try {
    fs.writeFileSync(
      path.join(dir, '.boss-skills.json'),
      JSON.stringify({
        adapters: { tracker: 'linear' },
        trackerConfig: { linear: { mcpServer: 'acme-tracker', team: 'Acme' } },
      }),
    )
    const adapter = createLinearAdapter({ apiKey: 'k', fetchImpl: async () => {}, cwd: dir })
    assert.equal(adapter.operationMap.selectPlanned.tool, 'mcp__acme-tracker__list_issues')
    assert.doesNotThrow(() => assertConforms(adapter))
  } finally {
    fs.rmSync(dir, { recursive: true, force: true })
  }
})

test('createLinearAdapter reads the linear config block when another tracker is selected', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'tracker-linear-explicit-config-'))
  try {
    fs.writeFileSync(
      path.join(dir, '.boss-skills.json'),
      JSON.stringify({
        adapters: { tracker: 'jira' },
        trackerConfig: {
          jira: { mcpServer: 'jira-tools', team: 'Jira' },
          linear: { mcpServer: 'linear-tools', team: 'Linear' },
        },
      }),
    )
    const adapter = createLinearAdapter({ apiKey: 'k', fetchImpl: async () => {}, cwd: dir })
    assert.equal(adapter.operationMap.selectPlanned.tool, 'mcp__linear-tools__list_issues')
  } finally {
    fs.rmSync(dir, { recursive: true, force: true })
  }
})

test('createLinearAdapter fails fast when trackerConfig.mcpServer is unavailable', () => {
  const dirs = [
    fs.mkdtempSync(path.join(os.tmpdir(), 'tracker-linear-no-config-')),
    fs.mkdtempSync(path.join(os.tmpdir(), 'tracker-linear-no-mcp-server-')),
  ]
  try {
    fs.writeFileSync(
      path.join(dirs[1], '.boss-skills.json'),
      JSON.stringify({
        adapters: { tracker: 'linear' },
        trackerConfig: { linear: { team: 'Acme' } },
      }),
    )
    for (const cwd of dirs) {
      assert.throws(
        () => createLinearAdapter({ apiKey: 'k', fetchImpl: async () => {}, cwd }),
        /mcpServer/,
      )
    }
  } finally {
    for (const dir of dirs) fs.rmSync(dir, { recursive: true, force: true })
  }
})

test('createLinearAdapter preserves invalid config diagnostics', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'tracker-linear-invalid-config-'))
  try {
    fs.writeFileSync(path.join(dir, '.boss-skills.json'), '{')
    assert.throws(
      () => createLinearAdapter({ apiKey: 'k', fetchImpl: async () => {}, cwd: dir }),
      /is not valid JSON/,
    )
  } finally {
    fs.rmSync(dir, { recursive: true, force: true })
  }
})

// --- the optional `states` capability (BOS-524) -----------------------------

test('the Linear adapter exposes states() answering every canonical role', () => {
  const adapter = createLinearAdapter({ apiKey: 'k', fetchImpl: async () => {} })
  assert.equal(typeof adapter.states, 'function')
  const states = adapter.states()
  // The contract: a plain object with an entry for EVERY canonical role (a string
  // name or null), so a caller can resolve any role without probing for presence.
  assert.equal(states !== null && typeof states === 'object', true)
  assert.equal(Array.isArray(states), false)
  for (const role of TRACKER_STATE_ROLES) {
    assert.ok(role in states, `states() must answer for the ${role} role`)
    const name = states[role]
    assert.ok(
      name === null || (typeof name === 'string' && name.length > 0),
      `states().${role} must be a non-empty string or null, got ${JSON.stringify(name)}`,
    )
  }
})

test('states() resolution is UNCHANGED from the config it derives: it equals trackerConfig states', () => {
  // The capability adds an AUTHORITY, not a new answer — this reference path must
  // still end at exactly the values the trackerConfig read always produced.
  const adapter = createLinearAdapter({ apiKey: 'k', fetchImpl: async () => {} })
  const configured = trackerConfigFor(loadSkillConfig({ cwd: repoRoot }))?.states ?? {}
  const states = adapter.states({ cwd: repoRoot })
  for (const role of TRACKER_STATE_ROLES) {
    assert.equal(states[role], configured[role] ?? null, `states().${role} must match the config`)
  }
})

test('states() returns all-null instead of throwing when no config can be loaded', () => {
  // A repo with no .boss-skills.json is the exact case the adapter-first resolution
  // exists to survive: the caller needs a fallback signal, not an exception.
  const adapter = createLinearAdapter({ apiKey: 'k', fetchImpl: async () => {} })
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'tracker-linear-states-'))
  try {
    let states
    assert.doesNotThrow(() => {
      states = adapter.states({ cwd: dir })
    })
    for (const role of TRACKER_STATE_ROLES) {
      assert.equal(states[role], null, `${role} must be null with no resolvable config`)
    }
  } finally {
    fs.rmSync(dir, { recursive: true, force: true })
  }
})

test('states() maps a blank configured state name to null, never an empty string', () => {
  // An empty name is as unusable as an absent one; surfacing '' would let it win the
  // adapter-first resolution and BLOCK a repo whose fallback held a good name.
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'tracker-linear-states-'))
  try {
    fs.writeFileSync(
      path.join(dir, '.boss-skills.json'),
      JSON.stringify({
        adapters: { tracker: 'linear' },
        trackerConfig: {
          linear: {
            mcpServer: 'stub-tracker',
            team: 'Stub',
            states: { planned: '   ', inProgress: 'Doing', inReview: 'Reviewing' },
          },
        },
      }),
    )
    const adapter = createLinearAdapter({ apiKey: 'k', fetchImpl: async () => {} })
    const states = adapter.states({ cwd: dir })
    assert.equal(states.planned, null)
    assert.equal(states.inProgress, 'Doing')
    assert.equal(states.inReview, 'Reviewing')
  } finally {
    fs.rmSync(dir, { recursive: true, force: true })
  }
})

// ---------------------------------------------------------------------------
// Shape guard (BOS-1244 row 9)
// ---------------------------------------------------------------------------

test('buildLinearOperationMap raises for a non-string, so no mcp__[object Object]__* map exists', () => {
  // The recorded misuse: every other helper in this tree takes an options object, so
  // `buildLinearOperationMap({mcpServer: 'bossanova-linear'})` is the natural call. It used to return
  // a well-formed map whose every tool name was `mcp__[object Object]__*`, failing only much later at
  // invocation against the live tracker.
  for (const bad of [
    { mcpServer: 'bossanova-linear' },
    undefined,
    null,
    42,
    '',
    '   ',
    ['bossanova-linear'],
  ]) {
    assert.throws(
      () => buildLinearOperationMap(bad),
      (error) => {
        assert.match(error.message, /buildLinearOperationMap\(mcpServer\)/, 'names the function')
        assert.match(error.message, /non-empty string/, 'and the expected shape')
        return true
      },
      `${JSON.stringify(bad)} must raise rather than produce a map`,
    )
  }

  // The correct call is untouched, and no reachable value can mint the broken namespace.
  const operationMap = buildLinearOperationMap('bossanova-linear')
  for (const [name, op] of Object.entries(operationMap)) {
    assert.doesNotMatch(op.tool, /\[object Object\]/, `${name} must carry a real tool name`)
    assert.match(op.tool, /^mcp__bossanova-linear__/, `${name} must use the supplied server name`)
  }
})

// --- selectPlanned: the executable, filtered candidate read (BOS-1294) -------------------------
// The worker's `list-planned` route. Every assertion reads the captured REQUEST BODY, because the
// defect this closes is a filter that silently never reached the wire — a forward that stops at the
// adapter boundary must still fail here.

/** Run `fn(adapter, impl, dir)` against a Linear adapter built from a throwaway config directory. */
async function withPlannedAdapter(linearBlock, fn, { viewerId = 'usr_me', nodes } = {}) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'tracker-linear-select-planned-'))
  try {
    fs.writeFileSync(
      path.join(dir, '.boss-skills.json'),
      JSON.stringify({ adapters: { tracker: 'linear' }, trackerConfig: { linear: linearBlock } }),
    )
    const bodies = []
    const impl = async (url, init) => {
      const body = JSON.parse(init.body)
      bodies.push(body)
      return {
        ok: true,
        json: async () =>
          body.query.includes('viewer')
            ? { data: { viewer: { id: viewerId } } }
            : { data: { issues: { nodes: nodes ?? [] } } },
      }
    }
    impl.bodies = bodies
    const adapter = createLinearAdapter({ apiKey: 'k', fetchImpl: impl, cwd: dir })
    await fn(adapter, impl, dir)
  } finally {
    fs.rmSync(dir, { recursive: true, force: true })
  }
}

const ACME = { mcpServer: 'acme-tracker', team: 'Acme' }

test('the Linear adapter declares a callable selectPlanned and still conforms', () => {
  const adapter = createLinearAdapter({ apiKey: 'k', fetchImpl: async () => {} })
  assert.equal(typeof adapter.selectPlanned, 'function')
  assert.doesNotThrow(() => assertConforms(adapter))
  assert.ok(adapter.operationMap.selectPlanned, 'the descriptor fallback is still declared')
})

test('selectPlanned composes its filter through buildIssueCountFilter and ANDs the team', async () => {
  await withPlannedAdapter(ACME, async (adapter, impl) => {
    await adapter.selectPlanned({
      state: 'Planned',
      label: ['label-a', 'label-b'],
      assigneeOrCreator: 'usr_owner',
    })
    assert.equal(impl.bodies.length, 1, 'a concrete id costs no viewer lookup')
    const { query, variables } = impl.bodies[0]
    assert.equal(query, LIST_PLANNED_QUERY)
    assert.deepEqual(variables.filter, {
      ...buildIssueCountFilter({
        state: 'Planned',
        label: ['label-a', 'label-b'],
        assigneeOrCreatorId: 'usr_owner',
      }),
      team: { name: { eq: 'Acme' } },
    })
    // Spelled out as well, so the shared builder cannot drift into a shape this test would follow.
    assert.deepEqual(variables.filter.labels, { name: { in: ['label-a', 'label-b'] } })
    assert.deepEqual(variables.filter.or, [
      { assignee: { id: { eq: 'usr_owner' } } },
      { creator: { id: { eq: 'usr_owner' } } },
    ])
    assert.equal(variables.first, 250, 'the default window matches the descriptor path')
  })
})

test('selectPlanned with no identity emits no or-clause and a single-label eq', async () => {
  await withPlannedAdapter(ACME, async (adapter, impl) => {
    await adapter.selectPlanned({ state: 'Planned', label: 'agent-friendly', limit: 10 })
    assert.deepEqual(impl.bodies[0].variables, {
      first: 10,
      filter: {
        state: { name: { eq: 'Planned' } },
        labels: { name: { eq: 'agent-friendly' } },
        team: { name: { eq: 'Acme' } },
      },
    })
  })
})

test('selectPlanned rejects an unknown query key before any request rather than dropping it', async () => {
  await withPlannedAdapter(ACME, async (adapter, impl) => {
    for (const [query, named] of [
      [{ state: 'Planned', label: 'agent-friendly', team: 'Other' }, /"team"/],
      [{ state: 'Planned', label: 'agent-friendly', creator: 'usr_x' }, /"creator"/],
    ]) {
      await assert.rejects(
        () => adapter.selectPlanned(query),
        (error) => {
          assert.match(error.message, /unknown selection key/)
          assert.match(error.message, named, 'the offending key is named')
          return true
        },
        JSON.stringify(query),
      )
    }
    assert.equal(impl.bodies.length, 0, 'an unknown key never reaches the wire')
  })
})

// Forward-compat pin: every clause `plannedSelectionQuery` derives from a fully-populated selection
// must be accepted by the wrapper AND reach the wire. A key added to the derivation without teaching
// the wrapper fails here instead of silently widening the worker's read.
test('selectPlanned accepts and applies every clause plannedSelectionQuery derives', async () => {
  const block = {
    ...ACME,
    states: { planned: 'Planned' },
    labels: { agentFriendly: 'agent-friendly' },
    selection: { assigneeOrCreator: 'usr_owner', labels: ['label-a', 'label-b'] },
  }
  await withPlannedAdapter(block, async (adapter, impl, dir) => {
    const query = plannedSelectionQuery(loadSkillConfig({ cwd: dir }))
    await adapter.selectPlanned({ ...query, limit: 250 })
    assert.equal(impl.bodies.length, 1)
    const { first, filter } = impl.bodies[0].variables
    assert.equal(first, 250)
    assert.deepEqual(filter, {
      ...buildIssueCountFilter({
        state: query.state,
        label: query.label,
        assigneeOrCreatorId: query.assigneeOrCreator,
      }),
      team: { name: { eq: 'Acme' } },
    })
    assert.deepEqual(filter.state, { name: { eq: 'Planned' } })
    assert.deepEqual(filter.labels, { name: { in: ['label-a', 'label-b'] } })
    assert.deepEqual(filter.or, [
      { assignee: { id: { eq: 'usr_owner' } } },
      { creator: { id: { eq: 'usr_owner' } } },
    ])
  })
})

test("selectPlanned resolves 'me' with exactly one viewer lookup before the list query", async () => {
  await withPlannedAdapter(
    ACME,
    async (adapter, impl) => {
      await adapter.selectPlanned({
        state: 'Planned',
        label: 'agent-friendly',
        assigneeOrCreator: 'me',
      })
      assert.equal(impl.bodies.length, 2)
      assert.match(impl.bodies[0].query, /viewer/)
      assert.equal(impl.bodies[1].query, LIST_PLANNED_QUERY)
      assert.deepEqual(impl.bodies[1].variables.filter.or, [
        { assignee: { id: { eq: 'usr_viewer' } } },
        { creator: { id: { eq: 'usr_viewer' } } },
      ])
    },
    { viewerId: 'usr_viewer' },
  )
})

test('the list query selects every field the Step 2 eligibility walk reads', () => {
  for (const field of [
    'identifier',
    'title',
    'priority',
    'estimate',
    'createdAt',
    /state\s*\{\s*name\s+type\s*\}/,
    /labels\s*\{\s*nodes\s*\{\s*name\s*\}\s*\}/,
    /attachments\s*\{\s*nodes\s*\{\s*id\s+title\s+url\s+createdAt\s*\}\s*\}/,
  ]) {
    assert.match(LIST_PLANNED_QUERY, field instanceof RegExp ? field : new RegExp(`\\b${field}\\b`))
  }
  assert.match(LIST_PLANNED_QUERY, /issues\(first:\s*\$first,\s*filter:\s*\$filter\)/)
})

test('selectPlanned flattens labels to names and attachments to a plain array', async () => {
  const nodes = [
    {
      identifier: 'ACME-7',
      title: 'Do the thing',
      priority: 2,
      estimate: 3,
      createdAt: '2026-01-01T00:00:00.000Z',
      state: { name: 'Planned', type: 'unstarted' },
      labels: { nodes: [{ name: 'agent-friendly' }, { name: 'bug' }] },
      attachments: {
        nodes: [
          {
            id: 'att_1',
            title: 'Implementation plan (ACME-7)',
            url: 'https://uploads.example/plan.md',
            createdAt: '2026-01-02T00:00:00.000Z',
          },
        ],
      },
    },
  ]
  await withPlannedAdapter(
    ACME,
    async (adapter) => {
      const result = await adapter.selectPlanned({ state: 'Planned', label: 'agent-friendly' })
      assert.deepEqual(result[0].labels, ['agent-friendly', 'bug'])
      assert.ok(Array.isArray(result[0].attachments))
      assert.deepEqual(result[0].attachments, nodes[0].attachments.nodes)
      // The shape the shared normalizer consumes: the canonical plan attachment resolves from it.
      const ticket = adapter.normalizeTicket(result[0])
      assert.equal(ticket.planAttachment?.id, 'att_1')
      assert.deepEqual(ticket.labels, ['agent-friendly', 'bug'])
    },
    { nodes },
  )
})

test('selectPlanned fails closed — zero requests — on a missing state or team', async () => {
  await withPlannedAdapter(ACME, async (adapter, impl) => {
    for (const state of [undefined, null, '', '   ']) {
      await assert.rejects(
        adapter.selectPlanned({ state, label: 'agent-friendly' }),
        /selectPlanned: a non-empty state is required/,
      )
    }
    assert.equal(impl.bodies.length, 0, 'a missing state must never reach the network')
  })
  // No team: unreachable through the loader (validateConfig requires one), so pinned directly.
  const calls = []
  for (const team of [undefined, null, '', '  ']) {
    await assert.rejects(
      linearSelectPlanned({
        apiKey: 'k',
        fetchImpl: async (...args) => calls.push(args),
        team,
        state: 'Planned',
        label: 'agent-friendly',
      }),
      /trackerConfig\.linear\.team is required/,
    )
  }
  assert.equal(calls.length, 0, 'a missing team must never reach the network')
})

test('selectPlanned rejects inputs that would silently widen the scan', async () => {
  await withPlannedAdapter(ACME, async (adapter, impl) => {
    const cases = [
      [{ label: [] }, /label/],
      [{ label: ['label-a', ''] }, /label/],
      [{ label: 7 }, /label/],
      [{ assigneeOrCreator: '' }, /assigneeOrCreator/],
      [{ limit: 0 }, /limit/],
      [{ limit: 251 }, /limit/],
      [{ limit: 2.5 }, /limit/],
      [{ limit: '10' }, /limit/],
    ]
    for (const [extra, pattern] of cases) {
      await assert.rejects(
        adapter.selectPlanned({ state: 'Planned', label: 'agent-friendly', ...extra }),
        pattern,
        JSON.stringify(extra),
      )
    }
    assert.equal(impl.bodies.length, 0)
  })
})

test('selectPlanned throws on an unreadable payload rather than answering empty', async () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'tracker-linear-select-planned-bad-'))
  try {
    fs.writeFileSync(
      path.join(dir, '.boss-skills.json'),
      JSON.stringify({ adapters: { tracker: 'linear' }, trackerConfig: { linear: ACME } }),
    )
    for (const data of [{}, { issues: null }, { issues: { nodes: null } }, { issues: {} }]) {
      const adapter = createLinearAdapter({
        apiKey: 'k',
        cwd: dir,
        fetchImpl: async () => ({ ok: true, json: async () => ({ data }) }),
      })
      await assert.rejects(
        adapter.selectPlanned({ state: 'Planned', label: 'agent-friendly' }),
        /cannot be evaluated/,
        JSON.stringify(data),
      )
    }
  } finally {
    fs.rmSync(dir, { recursive: true, force: true })
  }
})

// BOS-1303: the executable stored-description read behind `tracker/cli.mjs read-description`. The
// bytes it returns become the file every stored-description gate compares, so it must hand back
// exactly what the tracker stored — no newline added or stripped — and throw rather than answer
// with a description it did not read.
function describeFetch(issue) {
  const bodies = []
  const impl = async (url, init) => {
    bodies.push({ url, body: JSON.parse(init.body), headers: init.headers })
    return { ok: true, json: async () => ({ data: { issue } }) }
  }
  impl.bodies = bodies
  return impl
}

test('the Linear adapter declares a callable readDescription and still conforms (BOS-1303)', () => {
  const adapter = createLinearAdapter({ apiKey: 'k', fetchImpl: async () => {} })
  assert.equal(typeof adapter.readDescription, 'function')
  assert.doesNotThrow(() => assertConforms(adapter))
  assert.equal(
    adapter.operationMap.readDescription,
    undefined,
    'an executable read adds nothing to the MCP approval surface',
  )
})

test('readDescription sends issue(id:) with the id as given, trimmed, for a UUID and an identifier (BOS-1303)', async () => {
  for (const [given, sent] of [
    ['6f1c2e9a-0b8d-4c55-9a1e-3f2b7c4d5e60', '6f1c2e9a-0b8d-4c55-9a1e-3f2b7c4d5e60'],
    ['BOS-123', 'BOS-123'],
    ['  BOS-123\n', 'BOS-123'],
  ]) {
    const impl = describeFetch({ id: 'u-1', identifier: 'BOS-123', description: 'x' })
    const adapter = createLinearAdapter({ apiKey: 'raw-key', fetchImpl: impl })
    await adapter.readDescription(given)
    assert.equal(impl.bodies.length, 1)
    assert.equal(impl.bodies[0].body.query, READ_DESCRIPTION_QUERY)
    assert.deepEqual(impl.bodies[0].body.variables, { id: sent })
    // Raw key, never a Bearer form — the same header every other linearRequest caller sends.
    assert.equal(impl.bodies[0].headers.Authorization, 'raw-key')
  }
  assert.match(READ_DESCRIPTION_QUERY, /issue\(id: \$id\)/)
  for (const field of ['id', 'identifier', 'description']) {
    assert.match(READ_DESCRIPTION_QUERY, new RegExp(`\\b${field}\\b`))
  }
})

test('readDescription returns the stored description byte-verbatim (BOS-1303)', async () => {
  for (const description of ['a\nb', 'a\nb\n', 'naïve — 日本語 ✓\n\n', '']) {
    const impl = describeFetch({ id: 'u-1', identifier: 'BOS-1', description })
    const adapter = createLinearAdapter({ apiKey: 'k', fetchImpl: impl })
    const got = await adapter.readDescription('BOS-1')
    assert.deepEqual(got, { id: 'u-1', identifier: 'BOS-1', description })
    assert.equal(Buffer.byteLength(got.description), Buffer.byteLength(description))
  }
})

test('readDescription maps a null description to an empty string (BOS-1303)', async () => {
  const impl = describeFetch({ id: 'u-1', identifier: 'BOS-1', description: null })
  const got = await linearReadDescription({ apiKey: 'k', fetchImpl: impl, issueId: 'BOS-1' })
  assert.deepEqual(got, { id: 'u-1', identifier: 'BOS-1', description: '' })
})

test('readDescription fails closed on a missing issue or an unreadable payload (BOS-1303)', async () => {
  await assert.rejects(
    linearReadDescription({ apiKey: 'k', fetchImpl: describeFetch(null), issueId: 'BOS-404' }),
    /BOS-404/,
  )
  for (const issue of [
    { id: 'u-1', identifier: 'BOS-1', description: 42 },
    { id: 'u-1', identifier: 'BOS-1', description: { text: 'x' } },
    { id: 'u-1', identifier: 'BOS-1' },
    { identifier: 'BOS-1', description: 'x' },
    { id: 'u-1', description: 'x' },
  ]) {
    await assert.rejects(
      linearReadDescription({ apiKey: 'k', fetchImpl: describeFetch(issue), issueId: 'BOS-1' }),
      /readDescription/,
      JSON.stringify(issue),
    )
  }
})

test('readDescription throws before any request on a blank id or an unset key (BOS-1303)', async () => {
  const calls = []
  const fetchImpl = async (...args) => {
    calls.push(args)
    return { ok: true, json: async () => ({ data: { issue: null } }) }
  }
  for (const issueId of [undefined, null, '', '   ', 42]) {
    await assert.rejects(
      linearReadDescription({ apiKey: 'k', fetchImpl, issueId }),
      /non-empty issue id/,
      JSON.stringify(issueId),
    )
  }
  const adapter = createLinearAdapter({ apiKey: undefined, fetchImpl })
  await assert.rejects(adapter.readDescription('BOS-1'), (err) => {
    assert.match(err.message, /LINEAR_API_KEY is not set/)
    assert.equal(err.code, TRACKER_CREDENTIALS_MISSING, 'the CLI keys its fallback on this code')
    return true
  })
  assert.equal(calls.length, 0, 'neither a blank id nor a missing key may reach the network')
})

test('readDescription surfaces a GraphQL error instead of answering (BOS-1303)', async () => {
  const fetchImpl = async () => ({
    ok: true,
    json: async () => ({ errors: [{ message: 'Entity not found' }] }),
  })
  await assert.rejects(
    linearReadDescription({ apiKey: 'k', fetchImpl, issueId: 'BOS-1' }),
    /Linear GraphQL error: Entity not found/,
  )
})
