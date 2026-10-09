import { test } from 'node:test'
import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

import {
  APPLY_ISSUE_WRITES_LABELS_QUERY,
  APPLY_ISSUE_WRITES_READ_QUERY,
  buildLinearOperationMap,
  createLinearAdapter,
  linearApplyIssueWrites,
  linearCreatorMention,
  LIST_PLANNED_QUERY,
  READ_DESCRIPTION_QUERY,
  linearReadDescription,
  linearSelectPlanned,
  linearSelectCandidates,
  LIST_CANDIDATES_QUERY,
} from './linear.mjs'
import { assertEffectiveSelection, renderLinearIssueFilter } from '../selection.mjs'
import { assertConforms, REQUIRED_TRACKER_OPERATIONS, TRACKER_STATE_ROLES } from './adapter.mjs'
import { TRACKER_CREDENTIALS_MISSING } from './adapter-core.mjs'
import { formatClaimComment } from '../linear-claim.mjs'
import { loadSkillConfig, stageSelectionQuery, trackerConfigFor } from '../skill-config.mjs'

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

const candidateNode = {
  id: 'uuid-1',
  identifier: 'APP-1',
  title: 'candidate',
  priority: 2,
  createdAt: '2026-01-01',
  description: 'full',
  state: { name: 'Todo', type: 'unstarted' },
  parent: { identifier: 'APP-9' },
  labels: { nodes: [{ name: 'label' }] },
}

test('selectCandidates fetches full bodies, team AND states, paginates and hydrates Done ids without filters', async () => {
  const calls = []
  const fetchImpl = async (_, init) => {
    const body = JSON.parse(init.body)
    calls.push(body)
    return {
      ok: true,
      json: async () => ({
        data: body.variables.id
          ? {
              issue: { ...candidateNode, state: { name: 'Done', type: 'completed' }, parent: null },
            }
          : {
              issues: {
                nodes: [candidateNode],
                pageInfo: { hasNextPage: calls.length === 1, endCursor: 'next' },
              },
            },
      }),
    }
  }
  const rows = await linearSelectCandidates({
    apiKey: 'k',
    fetchImpl,
    team: 'team',
    states: ['Todo'],
    ids: ['APP-1'],
    limit: 1,
  })
  assert.deepEqual(calls[0].variables.filter, {
    team: { name: { eq: 'team' } },
    state: { name: { in: ['Todo'] } },
  })
  assert.equal(calls[1].variables.after, 'next')
  assert.deepEqual(calls[2].variables, { id: 'APP-1' })
  assert.equal(rows.length, 1)
  assert.equal(rows[0].source, 'id')
  assert.equal(rows[0].parentId, null)
  assert.equal(rows[0].stateType, 'completed')
  assert.equal(Object.hasOwn(rows[0], 'state'), false)
  assert.match(LIST_CANDIDATES_QUERY, /description/)
  assert.match(LIST_CANDIDATES_QUERY, /parent\s*\{\s*identifier\s*\}/)
  const control = await linearSelectCandidates({
    apiKey: 'k',
    fetchImpl: fakeFetch([candidateNode]).impl,
    team: 'team',
    states: ['Todo'],
  })
  assert.equal(control[0].parentId, 'APP-9')
  assert.deepEqual(control[0].labels, ['label'])
  const empty = await linearSelectCandidates({
    apiKey: 'k',
    fetchImpl: fakeFetch([{ ...candidateNode, parent: null, description: null }]).impl,
    team: 'team',
    states: ['Todo'],
  })
  assert.equal(empty[0].parentId, null)
  assert.equal(empty[0].description, '')
})

test('selectCandidates adapter forwards selectors and validates label connections', async () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'tracker-candidates-'))
  try {
    fs.writeFileSync(
      path.join(dir, '.boss-skills.json'),
      JSON.stringify({
        adapters: { tracker: 'linear' },
        trackerConfig: { linear: { mcpServer: 'stub', team: 'Acme' } },
      }),
    )
    const { impl, calls } = fakeFetch([candidateNode])
    const fetchImpl = async (url, init) => {
      const response = await impl(url, init)
      return JSON.parse(init.body).variables.id
        ? { ok: true, json: async () => ({ data: { issue: candidateNode } }) }
        : response
    }
    const adapter = createLinearAdapter({
      apiKey: 'secret',
      fetchImpl,
      endpoint: 'https://tracker.example/graphql',
      cwd: dir,
    })
    const rows = await adapter.selectCandidates({ states: ['Todo'], ids: ['APP-1'], limit: 7 })
    assert.equal(calls[0].url, 'https://tracker.example/graphql')
    assert.equal(calls[0].headers.Authorization, 'secret')
    assert.equal(calls[0].body.variables.first, 7)
    assert.deepEqual(calls[0].body.variables.filter, {
      team: { name: { eq: 'Acme' } },
      state: { name: { in: ['Todo'] } },
    })
    assert.deepEqual(rows[0].labels, ['label'])
    assert.deepEqual(calls[1].body.variables, { id: 'APP-1' })
    assert.equal(rows[0].source, 'id')
    assert.throws(
      () => adapter.selectCandidates({ states: ['Todo'], label: 'lost' }),
      /unknown selection/,
    )
    assert.equal(calls.length, 2)
    for (const labels of [
      undefined,
      null,
      {},
      { nodes: null },
      { nodes: [null] },
      { nodes: [{ name: '' }] },
      { nodes: ['epic'] },
    ]) {
      const malformed = createLinearAdapter({
        apiKey: 'k',
        fetchImpl: fakeFetch([{ ...candidateNode, labels }]).impl,
        cwd: dir,
      })
      await assert.rejects(malformed.selectCandidates({ states: ['Todo'] }), /unreadable candidate/)
    }
    const empty = createLinearAdapter({
      apiKey: 'k',
      fetchImpl: fakeFetch([{ ...candidateNode, labels: { nodes: [] } }]).impl,
      cwd: dir,
    })
    assert.deepEqual((await empty.selectCandidates({ states: ['Todo'] }))[0].labels, [])
  } finally {
    fs.rmSync(dir, { recursive: true, force: true })
  }
})

test('selectCandidates input guards throw before network and malformed/incomplete pages never answer empty', async () => {
  for (const patch of [
    { states: [] },
    { states: [''] },
    { team: '' },
    { limit: 0 },
    { limit: 251 },
    { ids: [''] },
    { apiKey: '' },
  ]) {
    let called = false
    await assert.rejects(
      linearSelectCandidates({
        apiKey: 'k',
        team: 'team',
        states: ['Todo'],
        ...patch,
        fetchImpl: async () => {
          called = true
        },
      }),
    )
    assert.equal(called, false)
  }
  await assert.rejects(linearSelectCandidates({ apiKey: '', team: 'team', states: ['Todo'] }), {
    code: TRACKER_CREDENTIALS_MISSING,
  })
  for (const issues of [
    { nodes: null, pageInfo: { hasNextPage: false } },
    { nodes: [], pageInfo: {} },
    { nodes: [], pageInfo: { hasNextPage: true, endCursor: null } },
    { nodes: [], pageInfo: { hasNextPage: true, endCursor: 'repeated' } },
    { nodes: [{}], pageInfo: { hasNextPage: false } },
  ]) {
    await assert.rejects(
      linearSelectCandidates({
        apiKey: 'k',
        team: 'team',
        states: ['Todo'],
        fetchImpl: async () => ({ ok: true, json: async () => ({ data: { issues } }) }),
      }),
    )
  }
  assert.deepEqual(
    await linearSelectCandidates({
      apiKey: 'k',
      team: 'team',
      states: ['Todo'],
      fetchImpl: fakeFetch([]).impl,
    }),
    [],
  )
})

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
  assert.equal(await adapter.hasUnblockedWork({ state: 'Todo', label: 'agent-build' }), true)
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
    label: 'agent-build',
    assignee: 'usr_a',
    creator: 'usr_c',
    assigneeOrCreator: 'me',
  })
  assert.deepEqual(impl.bodies[impl.bodies.length - 1].variables.filter, {
    state: { name: { eq: 'Todo' } },
    labels: { name: { eq: 'agent-build' } },
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
  await adapter.hasWork({ state: 'Unplanned', label: 'agent-build', assigneeOrCreator: 'me' })
  assert.deepEqual(impl.bodies[impl.bodies.length - 1].variables.filter, {
    state: { name: { eq: 'Unplanned' } },
    labels: { name: { eq: 'agent-build' } },
    or: [{ assignee: { id: { eq: 'usr_owner' } } }, { creator: { id: { eq: 'usr_owner' } } }],
  })
})

// The adapter half of the inertness pin: the shipped cron gate calls hasUnblockedWork with
// state and label only, and must keep emitting exactly what it emitted before this ticket.
test('the adapter gates emit the pre-BOS-1292 filter when given no selector', async () => {
  const impl = selectorFetch()
  const adapter = createLinearAdapter({ apiKey: 'k', fetchImpl: impl })
  await adapter.hasUnblockedWork({ state: 'Todo', label: 'agent-build' })
  assert.equal(impl.bodies.length, 1, 'no stray viewer lookup through the adapter either')
  assert.deepEqual(impl.bodies[0].variables.filter, {
    state: { name: { eq: 'Todo' } },
    labels: { name: { eq: 'agent-build' } },
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
    labels: { nodes: [{ name: 'agent-build' }] },
    inverseRelations: { nodes: [] },
  })
  assert.equal(ticket.id, 'BOS-1')
  assert.equal(ticket.stateName, 'Todo')
  assert.deepEqual(ticket.labels, ['agent-build'])
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
  assert.equal(operationMap.listTeams.tool, 'mcp__acme-tracker__list_teams')
  assert.match(operationMap.listTeams.summary, /resolveTrackerTeam/)
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
  // BOS-1333: save_issue takes the description inline, so executing the descriptor re-emits the
  // bytes. The summary must not promise a retype-free write that the tool cannot give.
  assert.doesNotMatch(operationMap.writeDescription.summary, /never retyped/)
  assert.match(operationMap.writeDescription.summary, /read back/)
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

test('createLinearAdapter defaults mcpServer to the adapter name, zero-config included, and fails fast with no linear block', () => {
  const dirs = [
    fs.mkdtempSync(path.join(os.tmpdir(), 'tracker-linear-no-config-')),
    fs.mkdtempSync(path.join(os.tmpdir(), 'tracker-linear-no-mcp-server-')),
    fs.mkdtempSync(path.join(os.tmpdir(), 'tracker-linear-other-tracker-')),
  ]
  try {
    fs.writeFileSync(
      path.join(dirs[1], '.boss-skills.json'),
      JSON.stringify({ trackerConfig: { linear: { team: 'Acme' } } }),
    )
    fs.writeFileSync(
      path.join(dirs[2], '.boss-skills.json'),
      JSON.stringify({ adapters: { tracker: 'jira' } }),
    )
    // Zero-config (BOS-1393): no .boss-skills.json synthesizes the default linear block.
    const zero = createLinearAdapter({ apiKey: 'k', fetchImpl: async () => {}, cwd: dirs[0] })
    assert.equal(zero.operationMap.listTeams.tool, 'mcp__linear__list_teams')
    const adapter = createLinearAdapter({ apiKey: 'k', fetchImpl: async () => {}, cwd: dirs[1] })
    assert.ok(adapter, 'a team-only config is enough: mcpServer defaults to "linear"')
    // Another tracker selected: nothing is synthesized for linear, so the adapter still refuses.
    assert.throws(
      () => createLinearAdapter({ apiKey: 'k', fetchImpl: async () => {}, cwd: dirs[2] }),
      /mcpServer/,
    )
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

test('states() answers the stock names in a repo with no .boss-skills.json (zero-config)', () => {
  const adapter = createLinearAdapter({ apiKey: 'k', fetchImpl: async () => {} })
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'tracker-linear-states-zero-'))
  try {
    const states = adapter.states({ cwd: dir })
    assert.equal(states.planned, 'Todo')
    assert.equal(states.inProgress, 'In Progress')
    assert.equal(states.inReview, 'In Review')
  } finally {
    fs.rmSync(dir, { recursive: true, force: true })
  }
})

test('states() returns all-null instead of throwing when no config can be loaded', () => {
  // An unloadable config is the exact case the adapter-first resolution exists to survive:
  // the caller needs a fallback signal, not an exception.
  const adapter = createLinearAdapter({ apiKey: 'k', fetchImpl: async () => {} })
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'tracker-linear-states-'))
  try {
    fs.writeFileSync(path.join(dir, '.boss-skills.json'), '{')
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

// --- selectPlanned: the executable, filtered candidate read (BOS-1294, BOS-1378) --------------
// The worker's `list-planned` / `list-unplanned` route. Every assertion reads the captured REQUEST
// BODY, because the defect this closes is a filter that silently never reached the wire.

const UUID_ME = '11111111-1111-4111-8111-111111111111'
const UUID_DEV = '22222222-2222-4222-8222-222222222222'
const UUID_PRJ = '33333333-3333-4333-8333-333333333333'

// The tracker's side of the one batched selection lookup.
const REFS_DATA = {
  viewer: { id: UUID_ME },
  users: { nodes: [{ id: UUID_DEV, email: 'dev@example.com' }] },
  issueLabels: { nodes: [{ name: 'infra' }, { name: 'Infra' }, { name: 'backend' }] },
  projects: { nodes: [{ id: UUID_PRJ, name: 'Platform' }] },
}

/** Run `fn(adapter, impl, dir)` against a Linear adapter built from a throwaway config directory. */
async function withPlannedAdapter(linearBlock, fn, { refsData = REFS_DATA, nodes } = {}) {
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
          body.query.includes('SelectionRefs')
            ? { data: refsData }
            : { data: { issues: { nodes: nodes ?? [], pageInfo: { hasNextPage: false } } } },
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
const EMPTY = assertEffectiveSelection(undefined)
const BUILD_RULES = { requireLabels: ['agent-build'], excludeLabels: ['needs-human'] }

test('the Linear adapter declares callable selectPlanned and resolveSelection and still conforms', () => {
  const adapter = createLinearAdapter({ apiKey: 'k', fetchImpl: async () => {} })
  assert.equal(typeof adapter.selectPlanned, 'function')
  assert.equal(typeof adapter.resolveSelection, 'function')
  assert.doesNotThrow(() => assertConforms(adapter))
  assert.ok(adapter.operationMap.selectPlanned, 'the descriptor fallback is still declared')
})

test('selectPlanned with no selection makes ONE request: state AND team AND stage labels', async () => {
  await withPlannedAdapter(ACME, async (adapter, impl) => {
    await adapter.selectPlanned({ state: 'Planned', selection: EMPTY, ...BUILD_RULES, limit: 10 })
    assert.equal(impl.bodies.length, 1, 'nothing to resolve means no lookup request')
    assert.equal(impl.bodies[0].query, LIST_PLANNED_QUERY)
    assert.deepEqual(impl.bodies[0].variables, {
      first: 10,
      filter: {
        state: { name: { eq: 'Planned' } },
        team: { name: { eq: 'Acme' } },
        and: [
          { labels: { some: { name: { eq: 'agent-build' } } } },
          { labels: { every: { name: { nin: ['needs-human'] } } } },
        ],
      },
    })
  })
})

test('selectPlanned resolves every ref in ONE batched lookup, then renders through the shared renderer', async () => {
  const selection = assertEffectiveSelection({
    labels: { include: ['backend'], exclude: ['INFRA'] },
    assignees: { exclude: ['me'] },
    creators: { include: ['dev@example.com'] },
    projects: { exclude: ['platform'] },
  })
  await withPlannedAdapter(ACME, async (adapter, impl) => {
    await adapter.selectPlanned({ state: 'Planned', selection, ...BUILD_RULES })
    assert.equal(impl.bodies.length, 2, 'one lookup, one list query')
    assert.match(impl.bodies[0].query, /SelectionRefs/)
    assert.equal(impl.bodies[1].query, LIST_PLANNED_QUERY)
    const resolved = {
      labels: { include: ['backend'], exclude: ['infra', 'Infra'] },
      assignees: { include: [], exclude: [UUID_ME] },
      creators: { include: [UUID_DEV], exclude: [] },
      projects: { include: [], exclude: [UUID_PRJ] },
    }
    assert.deepEqual(
      impl.bodies[1].variables.filter,
      renderLinearIssueFilter(resolved, { state: 'Planned', team: 'Acme', ...BUILD_RULES }),
    )
    // Spelled out too, so the shared renderer cannot drift into a shape this test would follow.
    assert.deepEqual(impl.bodies[1].variables.filter.and.at(2), {
      labels: { every: { name: { nin: ['needs-human', 'infra', 'Infra'] } } },
    })
    assert.equal(
      impl.bodies[1].variables.first,
      250,
      'the default window matches the descriptor path',
    )
  })
})

test('selectPlanned fails closed on an unresolvable ref: no list query is sent', async () => {
  const selection = assertEffectiveSelection({
    labels: { exclude: ['nope'] },
    creators: { exclude: ['ghost@example.com'] },
  })
  await withPlannedAdapter(ACME, async (adapter, impl) => {
    await assert.rejects(
      adapter.selectPlanned({ state: 'Planned', selection, ...BUILD_RULES }),
      /could not resolve user "ghost@example.com", label "nope"/,
    )
    assert.equal(impl.bodies.length, 1, 'only the lookup ran')
  })
})

test('selectPlanned rejects legacy and unknown query keys before any request', async () => {
  await withPlannedAdapter(ACME, async (adapter, impl) => {
    for (const [query, named] of [
      [{ state: 'Planned', label: 'agent-build' }, /"label"/],
      [{ state: 'Planned', assigneeOrCreator: 'me' }, /"assigneeOrCreator"/],
      [{ state: 'Planned', project: 'Other' }, /"project"/],
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

// Forward-compat pin: every clause `stageSelectionQuery` derives must be accepted by the wrapper
// AND reach the wire. A key added to the derivation without teaching the wrapper fails here.
test('selectPlanned accepts and applies every clause stageSelectionQuery derives', async () => {
  const block = {
    ...ACME,
    states: { planned: 'Planned' },
    labels: { agentBuild: 'agent-build', needsHuman: 'needs-human' },
    selection: {
      labels: { include: ['backend'] },
      stages: { build: { labels: { exclude: ['infra'] }, projects: { include: ['Platform'] } } },
    },
  }
  await withPlannedAdapter(block, async (adapter, impl, dir) => {
    const query = stageSelectionQuery(loadSkillConfig({ cwd: dir }), 'build', {
      creators: { exclude: ['me'] },
    })
    await adapter.selectPlanned({ ...query, limit: 250 })
    const { filter } = impl.bodies.at(-1).variables
    assert.deepEqual(filter.state, { name: { eq: 'Planned' } })
    assert.deepEqual(filter.team, { name: { eq: 'Acme' } })
    assert.deepEqual(filter.and, [
      { labels: { some: { name: { eq: 'agent-build' } } } },
      { labels: { some: { name: { in: ['backend'] } } } },
      { labels: { every: { name: { nin: ['needs-human', 'infra', 'Infra'] } } } },
      { or: [{ creator: { null: true } }, { creator: { id: { nin: [UUID_ME] } } }] },
      { project: { id: { in: [UUID_PRJ] } } },
    ])
  })
})

test('hasWork and hasUnblockedWork render the selection query through the same renderer', async () => {
  const selection = assertEffectiveSelection({ labels: { exclude: ['infra'] } })
  for (const method of ['hasWork', 'hasUnblockedWork']) {
    await withPlannedAdapter(ACME, async (adapter, impl) => {
      await adapter[method]({ state: 'Planned', selection, ...BUILD_RULES })
      assert.equal(impl.bodies.length, 2, method)
      assert.deepEqual(
        impl.bodies[1].variables.filter,
        renderLinearIssueFilter(
          { ...EMPTY, labels: { include: [], exclude: ['infra', 'Infra'] } },
          { state: 'Planned', ...BUILD_RULES },
        ),
        `${method}: the gate is not team-scoped`,
      )
    })
  }
})

test('the gates refuse a selection query mixed with legacy keys, or with no state', async () => {
  await withPlannedAdapter(ACME, async (adapter, impl) => {
    for (const method of ['hasWork', 'hasUnblockedWork']) {
      await assert.rejects(
        adapter[method]({ state: 'Planned', selection: EMPTY, label: 'agent-build' }),
        /legacy key\(s\) label cannot be combined/,
      )
      await assert.rejects(adapter[method]({ selection: EMPTY }), /a non-empty state is required/)
      await assert.rejects(
        adapter[method]({ state: 'Planned', selection: { lables: {} } }),
        /unknown selection dimension "lables"/,
      )
    }
    assert.equal(impl.bodies.length, 0)
  })
})

test('resolveSelection returns the resolved selection with one lookup, or none when empty', async () => {
  await withPlannedAdapter(ACME, async (adapter, impl) => {
    assert.deepEqual(await adapter.resolveSelection({ selection: EMPTY }), EMPTY)
    assert.equal(impl.bodies.length, 0, 'an empty selection costs no request')
    const resolved = await adapter.resolveSelection({
      selection: assertEffectiveSelection({ assignees: { include: ['me'] } }),
    })
    assert.deepEqual(resolved.assignees, { include: [UUID_ME], exclude: [] })
    assert.equal(impl.bodies.length, 1)
  })
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
      labels: { nodes: [{ name: 'agent-build' }, { name: 'bug' }] },
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
      const result = await adapter.selectPlanned({ state: 'Planned', ...BUILD_RULES })
      assert.deepEqual(result[0].labels, ['agent-build', 'bug'])
      assert.ok(Array.isArray(result[0].attachments))
      assert.deepEqual(result[0].attachments, nodes[0].attachments.nodes)
      // The shape the shared normalizer consumes: the canonical plan attachment resolves from it.
      const ticket = adapter.normalizeTicket(result[0])
      assert.equal(ticket.planAttachment?.id, 'att_1')
      assert.deepEqual(ticket.labels, ['agent-build', 'bug'])
    },
    { nodes },
  )
})

test('selectPlanned fails closed — zero requests — on a missing state or team', async () => {
  await withPlannedAdapter(ACME, async (adapter, impl) => {
    for (const state of [undefined, null, '', '   ']) {
      await assert.rejects(
        adapter.selectPlanned({ state, ...BUILD_RULES }),
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
        ...BUILD_RULES,
      }),
      /trackerConfig\.linear\.team is required/,
    )
  }
  assert.equal(calls.length, 0, 'a missing team must never reach the network')
})

test('selectPlanned uses query.team only when the config names no team (BOS-1393)', async () => {
  // Zero-config shape: a linear block with no team. The run's resolved team scopes the read.
  await withPlannedAdapter({ mcpServer: 'acme-tracker' }, async (adapter, impl) => {
    await adapter.selectPlanned({ state: 'Planned', ...BUILD_RULES, team: 'Resolved' })
    assert.deepEqual(impl.bodies.at(-1).variables.filter.team, { name: { eq: 'Resolved' } })
    // With neither a configured nor a passed team the guard refuses before any request.
    const before = impl.bodies.length
    await assert.rejects(
      adapter.selectPlanned({ state: 'Planned', ...BUILD_RULES }),
      /trackerConfig\.linear\.team is required/,
    )
    assert.equal(impl.bodies.length, before)
  })
  // A configured team always beats query.team.
  await withPlannedAdapter(ACME, async (adapter, impl) => {
    await adapter.selectPlanned({ state: 'Planned', ...BUILD_RULES, team: 'Other' })
    assert.deepEqual(impl.bodies.at(-1).variables.filter.team, { name: { eq: 'Acme' } })
  })
})

test('selectCandidates uses query.team only when the config names no team (BOS-1393)', async () => {
  for (const [block, passed, expected] of [
    [{ mcpServer: 'stub' }, 'Resolved', 'Resolved'],
    [{ mcpServer: 'stub', team: 'Acme' }, 'Other', 'Acme'],
    [{ mcpServer: 'stub', team: 'Acme' }, undefined, 'Acme'],
  ]) {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'tracker-candidates-team-'))
    try {
      fs.writeFileSync(
        path.join(dir, '.boss-skills.json'),
        JSON.stringify({ adapters: { tracker: 'linear' }, trackerConfig: { linear: block } }),
      )
      const { impl, calls } = fakeFetch([candidateNode])
      const adapter = createLinearAdapter({ apiKey: 'k', fetchImpl: impl, cwd: dir })
      await adapter.selectCandidates({
        states: ['Todo'],
        ...(passed === undefined ? {} : { team: passed }),
      })
      assert.deepEqual(calls[0].body.variables.filter.team, { name: { eq: expected } })
      if (!block.team) {
        await assert.rejects(
          Promise.resolve().then(() => adapter.selectCandidates({ states: ['Todo'] })),
          /a team is required/,
        )
        assert.equal(calls.length, 1, 'no team must never reach the network')
      }
    } finally {
      fs.rmSync(dir, { recursive: true, force: true })
    }
  }
})

test('selectPlanned rejects inputs that would silently widen the scan', async () => {
  await withPlannedAdapter(ACME, async (adapter, impl) => {
    const cases = [
      [{ requireLabels: [''] }, /requireLabels/],
      [{ requireLabels: 'agent-build' }, /requireLabels/],
      [{ excludeLabels: [7] }, /excludeLabels/],
      [{ selection: { labels: { include: [''] } } }, /selection\.labels\.include/],
      [{ selection: { label: {} } }, /unknown selection dimension/],
      [{ selection: [] }, /selection must be an object/],
      [{ limit: 0 }, /limit/],
      [{ limit: 251 }, /limit/],
      [{ limit: 2.5 }, /limit/],
      [{ limit: '10' }, /limit/],
    ]
    for (const [extra, pattern] of cases) {
      await assert.rejects(
        adapter.selectPlanned({ state: 'Planned', ...BUILD_RULES, ...extra }),
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
        adapter.selectPlanned({ state: 'Planned', ...BUILD_RULES }),
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

// BOS-1392: the executable tracker write behind verify-gate's `post` and `merge`. Every test drives
// it through an injected fetchImpl, so the mutations are asserted as built, never sent.
const writeIssue = {
  id: 'issue-uuid',
  identifier: 'BOS-1',
  team: {
    id: 'team-1',
    states: {
      nodes: [
        { id: 'state-todo', name: 'Todo' },
        { id: 'state-done', name: 'Done' },
      ],
    },
  },
  labels: {
    nodes: [
      { id: 'label-agent', name: 'agent-build' },
      { id: 'label-nh-current', name: 'needs-human' },
    ],
  },
  creator: {
    id: 'u1',
    name: 'dave',
    displayName: 'Dave',
    url: 'https://linear.app/acme/profiles/dave',
  },
  comments: { nodes: [{ body: 'an older comment' }] },
}

// A scripted transport: each POST is recorded and answered by the first handler whose `match`
// returns true for its query text. A handler may throw to simulate a transport failure.
function writeFetch(handlers) {
  const calls = []
  const impl = async (url, init) => {
    const body = JSON.parse(init.body)
    calls.push(body)
    const handler = handlers.find((h) => h.match(body.query, body))
    if (!handler) throw new Error(`unexpected query: ${body.query.slice(0, 60)}`)
    const answer = await handler.answer(body)
    return { ok: true, json: async () => answer }
  }
  return { impl, calls }
}

const readHandler = (issue = writeIssue) => ({
  match: (q) => q === APPLY_ISSUE_WRITES_READ_QUERY,
  answer: () => ({ data: { issue } }),
})
const labelsHandler = (nodes) => ({
  match: (q) => q === APPLY_ISSUE_WRITES_LABELS_QUERY,
  answer: () => ({ data: { issueLabels: { nodes } } }),
})
const mutationHandler = (field, payload = { success: true }) => ({
  match: (q) => q.includes(`${field}(`) && q.startsWith('mutation'),
  answer: () => ({ data: { [field]: payload } }),
})
const mutations = (calls) => calls.filter((c) => c.query.startsWith('mutation'))

test('applyIssueWrites builds each mutation with resolved ids, in label/comment/state order', async () => {
  const { impl, calls } = writeFetch([
    readHandler({ ...writeIssue, labels: { nodes: [{ id: 'label-agent', name: 'agent-build' }] } }),
    labelsHandler([
      { id: 'nh-other-team', name: 'needs-human', team: { id: 'team-2' } },
      { id: 'nh-team', name: 'needs-human', team: { id: 'team-1' } },
      { id: 'nh-workspace', name: 'needs-human', team: null },
    ]),
    mutationHandler('issueAddLabel'),
    mutationHandler('issueRemoveLabel'),
    mutationHandler('commentCreate', {
      success: true,
      comment: { id: 'c1', url: 'https://linear.app/c1' },
    }),
    mutationHandler('issueUpdate'),
  ])
  const result = await linearApplyIssueWrites({
    apiKey: 'k',
    fetchImpl: impl,
    issueId: 'BOS-1',
    addLabels: ['needs-human'],
    removeLabels: ['agent-build'],
    comment: 'needs a human',
    mentionCreator: true,
    marker: 'boss-verify head abc',
    stateName: 'Done',
  })
  assert.equal(result.ok, true)
  assert.equal(result.outcome, 'ok')
  assert.deepEqual(result.comment, { id: 'c1', url: 'https://linear.app/c1' })
  const sent = mutations(calls)
  assert.deepEqual(
    sent.map((c) => c.variables),
    [
      { id: 'issue-uuid', labelId: 'nh-team' },
      { id: 'issue-uuid', labelId: 'label-agent' },
      {
        input: {
          issueId: 'issue-uuid',
          body: 'https://linear.app/acme/profiles/dave\n\nneeds a human\n\nboss-verify head abc',
        },
      },
      { id: 'issue-uuid', input: { stateId: 'state-done' } },
    ],
  )
  assert.match(sent[0].query, /issueAddLabel/)
  assert.match(sent[1].query, /issueRemoveLabel/)
  assert.match(sent[2].query, /commentCreate/)
  assert.match(sent[3].query, /issueUpdate/)
  assert.deepEqual(
    result.applied.map((a) => a.kind),
    ['addLabel', 'removeLabel', 'comment', 'state'],
  )
})

test('applyIssueWrites skips labels already in the wanted state and a comment whose marker exists', async () => {
  const { impl, calls } = writeFetch([
    readHandler({
      ...writeIssue,
      comments: { nodes: [{ body: 'x\n\nboss-verify head abc' }] },
    }),
  ])
  const result = await linearApplyIssueWrites({
    apiKey: 'k',
    fetchImpl: impl,
    issueId: 'BOS-1',
    addLabels: ['needs-human'],
    removeLabels: ['not-present'],
    comment: 'again',
    marker: 'boss-verify head abc',
  })
  assert.equal(result.outcome, 'ok')
  assert.equal(mutations(calls).length, 0, 'nothing to change means nothing is written')
  assert.deepEqual(result.applied, [{ kind: 'comment', existing: true }])
})

test('applyIssueWrites resolves every name before writing anything', async () => {
  for (const [writes, reason] of [
    [{ addLabels: ['missing'] }, /label-not-found: missing/],
    [{ addLabels: ['team2-only'] }, /label-not-found: team2-only/],
    [{ stateName: 'Nope' }, /state-not-found: Nope/],
  ]) {
    const { impl, calls } = writeFetch([
      readHandler(),
      labelsHandler([{ id: 't2', name: 'team2-only', team: { id: 'team-2' } }]),
    ])
    const result = await linearApplyIssueWrites({
      apiKey: 'k',
      fetchImpl: impl,
      issueId: 'BOS-1',
      comment: 'c',
      ...writes,
    })
    assert.equal(result.outcome, 'failed')
    assert.match(result.reason, reason)
    assert.equal(mutations(calls).length, 0, JSON.stringify(writes))
  }
})

test('applyIssueWrites classifies a dropped socket on a mutation indeterminate and never re-sends it', async () => {
  let commentSends = 0
  const { impl, calls } = writeFetch([
    readHandler(),
    {
      match: (q) => q.includes('commentCreate('),
      answer: () => {
        commentSends += 1
        throw new Error('socket hang up')
      },
    },
    {
      match: (q) => q.includes('ApplyIssueWritesComments'),
      answer: () => ({ data: { issue: { comments: { nodes: [{ body: 'unrelated' }] } } } }),
    },
  ])
  const result = await linearApplyIssueWrites({
    apiKey: 'k',
    fetchImpl: impl,
    issueId: 'BOS-1',
    comment: 'needs a human',
    marker: 'boss-verify head abc',
    stateName: 'Done',
  })
  assert.equal(result.outcome, 'indeterminate')
  assert.equal(result.ok, false)
  assert.equal(commentSends, 1, 'an indeterminate mutation is sent exactly once')
  assert.equal(
    calls.filter((c) => c.query.includes('issueUpdate(')).length,
    0,
    'later writes stop after an indeterminate one',
  )
})

test('applyIssueWrites settles an indeterminate comment by the marker read-back', async () => {
  let sends = 0
  const { impl } = writeFetch([
    readHandler(),
    {
      match: (q) => q.includes('commentCreate('),
      answer: () => {
        sends += 1
        throw new Error('fetch failed')
      },
    },
    {
      match: (q) => q.includes('ApplyIssueWritesComments'),
      answer: () => ({
        data: { issue: { comments: { nodes: [{ body: 'hi\n\nboss-verify head abc' }] } } },
      }),
    },
    mutationHandler('issueUpdate'),
  ])
  const result = await linearApplyIssueWrites({
    apiKey: 'k',
    fetchImpl: impl,
    issueId: 'BOS-1',
    comment: 'hi',
    marker: 'boss-verify head abc',
    stateName: 'Done',
  })
  assert.equal(sends, 1)
  assert.equal(result.outcome, 'ok')
  assert.deepEqual(
    result.applied.map((a) => a.kind),
    ['comment', 'state'],
  )
  assert.equal(result.applied[0].verifiedByMarker, true)
})

test('applyIssueWrites reports a rejected or GraphQL-failed mutation as failed, not indeterminate', async () => {
  const rejected = writeFetch([readHandler(), mutationHandler('issueUpdate', { success: false })])
  const r1 = await linearApplyIssueWrites({
    apiKey: 'k',
    fetchImpl: rejected.impl,
    issueId: 'BOS-1',
    stateName: 'Done',
  })
  assert.equal(r1.outcome, 'failed')
  assert.match(r1.reason, /state-rejected/)
  const graphql = writeFetch([
    readHandler(),
    {
      match: (q) => q.includes('issueUpdate('),
      answer: () => ({ errors: [{ message: 'Entity not found' }] }),
    },
  ])
  const r2 = await linearApplyIssueWrites({
    apiKey: 'k',
    fetchImpl: graphql.impl,
    issueId: 'BOS-1',
    stateName: 'Done',
  })
  assert.equal(r2.outcome, 'failed')
})

test('applyIssueWrites throws before any request on a missing key or malformed input', async () => {
  const calls = []
  const fetchImpl = async (...args) => {
    calls.push(args)
    return { ok: true, json: async () => ({ data: {} }) }
  }
  const adapter = createLinearAdapter({ apiKey: undefined, fetchImpl })
  await assert.rejects(adapter.applyIssueWrites({ issueId: 'BOS-1', stateName: 'Done' }), (err) => {
    assert.match(err.message, /LINEAR_API_KEY is not set/)
    assert.equal(err.code, TRACKER_CREDENTIALS_MISSING)
    return true
  })
  for (const writes of [
    { issueId: '' },
    { issueId: 'BOS-1', addLabels: 'needs-human' },
    { issueId: 'BOS-1', removeLabels: [''] },
    { issueId: 'BOS-1', comment: '  ' },
    { issueId: 'BOS-1', stateName: '' },
    { issueId: 'BOS-1', addLabels: ['x'], removeLabels: ['x'] },
  ]) {
    await assert.rejects(
      linearApplyIssueWrites({ apiKey: 'k', fetchImpl, ...writes }),
      /applyIssueWrites/,
      JSON.stringify(writes),
    )
  }
  assert.throws(
    () => createLinearAdapter({ apiKey: 'k', fetchImpl }).applyIssueWrites({ issueId: 'B', x: 1 }),
    /unknown key/,
  )
  assert.equal(calls.length, 0, 'neither a missing key nor bad input may reach the network')
})

test('the Linear adapter declares a callable applyIssueWrites and still conforms (BOS-1392)', () => {
  const adapter = createLinearAdapter({ apiKey: 'k', fetchImpl: async () => {} })
  assert.equal(typeof adapter.applyIssueWrites, 'function')
  assert.doesNotThrow(() => assertConforms(adapter))
  assert.equal(adapter.operationMap.applyIssueWrites, undefined)
})

test('linearCreatorMention prefers the profile URL Linear turns into a mention', () => {
  assert.equal(
    linearCreatorMention({ url: 'https://linear.app/a/profiles/d', displayName: 'D' }),
    'https://linear.app/a/profiles/d',
  )
  assert.equal(linearCreatorMention({ displayName: 'Dave', name: 'dave' }), 'Dave')
  assert.equal(linearCreatorMention({ name: 'dave' }), 'dave')
  assert.equal(linearCreatorMention(null), '')
})

test('applyIssueWrites with no writes is a read of the issue labels and sends no mutation', async () => {
  const { impl, calls } = writeFetch([readHandler()])
  const result = await linearApplyIssueWrites({ apiKey: 'k', fetchImpl: impl, issueId: 'BOS-1' })
  assert.equal(result.outcome, 'ok')
  assert.deepEqual(result.applied, [])
  assert.deepEqual(result.issue, {
    id: 'issue-uuid',
    identifier: 'BOS-1',
    labels: ['agent-build', 'needs-human'],
  })
  assert.equal(calls.length, 1)
  assert.equal(mutations(calls).length, 0)
})

test('selectMarked paginates full marker bodies and preserves delta filtering', async () => {
  const { linearSelectMarked, MARKED_ISSUES_QUERY } = await import('./linear.mjs')
  const calls = []
  const fetchImpl = async (_, init) => {
    const body = JSON.parse(init.body)
    calls.push(body)
    return {
      ok: true,
      json: async () => ({
        data: {
          issues: {
            nodes: [
              {
                identifier: `APP-${calls.length}`,
                title: 'Theme',
                description: 'Notes: key',
                createdAt: '2026-01-01',
                completedAt: null,
                canceledAt: null,
                state: { type: 'started' },
              },
            ],
            pageInfo: { hasNextPage: calls.length === 1, endCursor: 'cursor' },
          },
        },
      }),
    }
  }
  const result = await linearSelectMarked({
    apiKey: 'key',
    fetchImpl,
    markerPrefix: 'Notes: ',
    updatedAfter: '2026-01-01',
  })
  assert.deepEqual(
    result.map((i) => i.identifier),
    ['APP-1', 'APP-2'],
  )
  assert.equal(calls[0].query, MARKED_ISSUES_QUERY)
  assert.deepEqual(calls[0].variables.filter, {
    description: { contains: 'Notes: ' },
    updatedAt: { gt: '2026-01-01' },
  })
  assert.equal(calls[1].variables.after, 'cursor')
  assert.match(MARKED_ISSUES_QUERY, /includeArchived: true/)
})

test('selectMarked refuses incomplete pages, invalid filters and absent credentials', async () => {
  const { linearSelectMarked } = await import('./linear.mjs')
  const good = {
    nodes: [
      {
        identifier: 'APP-1',
        title: 'Theme',
        description: 'Notes: key',
        createdAt: '2026-01-01',
        completedAt: null,
        canceledAt: null,
        state: { type: 'started' },
      },
    ],
    pageInfo: { hasNextPage: false },
  }
  for (const [issues, pattern] of [
    [undefined, /no issues connection/],
    [{ ...good, nodes: {} }, /malformed issue nodes/],
    [{ ...good, nodes: null }, /malformed issue nodes/],
    [{ ...good, nodes: [null] }, /malformed issue entry/],
    [{ ...good, nodes: [{ identifier: '', description: 'Notes: key' }] }, /malformed issue entry/],
    [{ ...good, nodes: [{ identifier: 'APP-1', description: null }] }, /malformed issue entry/],
    [{ ...good, pageInfo: {} }, /malformed page info/],
    [{ ...good, pageInfo: null }, /malformed page info/],
    [{ ...good, pageInfo: { hasNextPage: 'false' } }, /malformed page info/],
    [{ ...good, pageInfo: { hasNextPage: true } }, /no continuation cursor/],
    [{ ...good, pageInfo: { hasNextPage: true, endCursor: 'cursor' } }, /exceeded 1 pages/],
  ]) {
    await assert.rejects(
      linearSelectMarked({
        apiKey: 'key',
        markerPrefix: 'Notes: ',
        maxPages: 1,
        fetchImpl: async () => ({ ok: true, json: async () => ({ data: { issues } }) }),
      }),
      pattern,
    )
  }
  await assert.rejects(linearSelectMarked({ markerPrefix: 'Notes: ' }), {
    code: TRACKER_CREDENTIALS_MISSING,
  })
  await assert.rejects(linearSelectMarked({ apiKey: 'key', markerPrefix: ' ' }), /markerPrefix/)
  await assert.rejects(
    linearSelectMarked({ apiKey: 'key', markerPrefix: 'Notes: ', updatedAfter: 'yesterday' }),
    /timestamp/,
  )
  const adapter = createLinearAdapter({ apiKey: 'key', cwd: repoRoot })
  assert.throws(() => adapter.selectMarked({ unexpected: true }), /unknown/)
  assert.equal(buildLinearOperationMap('test').createIssue.tool, 'mcp__test__save_issue')
})

test('selectMarked exposes tracker-neutral resolution and validates state and dates', async () => {
  const { linearSelectMarked } = await import('./linear.mjs')
  const rows = ['completed', 'canceled', 'started'].map((type, i) => ({
    identifier: `APP-${i}`,
    title: 'Theme',
    description: 'Notes: key',
    createdAt: '2026-01-01',
    completedAt: type === 'completed' ? '2026-02-01' : null,
    canceledAt: type === 'canceled' ? '2026-03-01' : null,
    state: { type },
  }))
  const read = (nodes) =>
    linearSelectMarked({
      apiKey: 'key',
      markerPrefix: 'Notes: ',
      fetchImpl: async () => ({
        ok: true,
        json: async () => ({ data: { issues: { nodes, pageInfo: { hasNextPage: false } } } }),
      }),
    })
  assert.deepEqual(
    await read(rows),
    rows.map((r) => ({
      identifier: r.identifier,
      title: r.title,
      description: r.description,
      createdAt: r.createdAt,
      resolution:
        r.state.type === 'completed' ? 'done' : r.state.type === 'canceled' ? 'canceled' : null,
      resolvedAt: r.completedAt ?? r.canceledAt ?? null,
    })),
  )
  for (const patch of [
    { state: null },
    { state: {} },
    { state: { type: '' } },
    { title: null },
    { completedAt: 1 },
  ]) {
    await assert.rejects(read([{ ...rows[0], ...patch }]), /malformed issue entry/)
  }
})
