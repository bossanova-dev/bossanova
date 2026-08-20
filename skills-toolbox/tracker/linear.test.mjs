import { test } from 'node:test'
import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

import { buildLinearOperationMap, createLinearAdapter } from './linear.mjs'
import { assertConforms, REQUIRED_TRACKER_OPERATIONS, TRACKER_STATE_ROLES } from './adapter.mjs'
import { formatClaimComment } from '../linear-claim.mjs'
import { loadSkillConfig, trackerConfigFor } from '../skill-config.mjs'

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
  for (const key of REQUIRED_TRACKER_OPERATIONS) {
    assert.match(operationMap[key].tool, /^mcp__acme-tracker__/)
  }
})

test('buildLinearOperationMap updateComment summary mentions updating in place and the id argument', () => {
  const operationMap = buildLinearOperationMap('acme-tracker')
  assert.match(operationMap.updateComment.summary, /\bid\b/)
  assert.match(operationMap.updateComment.summary, /updates.*in place/i)
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
