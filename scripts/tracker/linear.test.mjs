import { test } from 'node:test'
import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

import { createLinearAdapter, linearOperationMap } from './linear.mjs'
import { assertConforms, TRACKER_STATE_ROLES } from './adapter.mjs'
import { formatClaimComment } from '../linear-claim.mjs'
import { loadSkillConfig, trackerConfigFor } from '../../skills-toolbox/skill-config.mjs'

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

test('linearOperationMap names the agent-driven MCP tools', () => {
  assert.equal(linearOperationMap.selectPlanned.tool, 'mcp__bossanova-linear__list_issues')
  assert.equal(linearOperationMap.moveState.tool, 'mcp__bossanova-linear__save_issue')
  assert.equal(linearOperationMap.readComments.tool, 'mcp__bossanova-linear__list_comments')
  assert.match(
    linearOperationMap.readComments.summary,
    /\{id, body, createdAt\}/,
    'readComments must document that it surfaces comment ids — updateComment is unreachable without them',
  )
  assert.equal(linearOperationMap.writeComment.tool, 'mcp__bossanova-linear__save_comment')
  assert.equal(linearOperationMap.updateComment.tool, 'mcp__bossanova-linear__save_comment')
  assert.equal(linearOperationMap.readLabels.tool, 'mcp__bossanova-linear__get_issue')
  assert.equal(linearOperationMap.extractImages.tool, 'mcp__bossanova-linear__extract_images')
  assert.equal(linearOperationMap.createLabel.tool, 'mcp__bossanova-linear__create_issue_label')
  assert.equal(linearOperationMap.setPriorityEstimate.tool, 'mcp__bossanova-linear__save_issue')
  assert.equal(linearOperationMap.appendDependency.tool, 'mcp__bossanova-linear__save_issue')
  assert.equal(
    linearOperationMap.preparePlanAttachment.tool,
    'mcp__bossanova-linear__prepare_attachment_upload',
  )
  assert.equal(
    linearOperationMap.finalizePlanAttachment.tool,
    'mcp__bossanova-linear__create_attachment_from_upload',
  )
  assert.equal(linearOperationMap.readPlanAttachment.tool, 'mcp__bossanova-linear__get_attachment')
  assert.equal(
    linearOperationMap.deletePlanAttachment.tool,
    'mcp__bossanova-linear__delete_attachment',
  )
})

test('linearOperationMap.updateComment summary mentions updating in place and the id argument', () => {
  assert.match(linearOperationMap.updateComment.summary, /\bid\b/)
  assert.match(linearOperationMap.updateComment.summary, /updates.*in place/i)
})

test('readComments, writeComment, and updateComment are all present (the single-comment progress protocol trio)', () => {
  for (const key of ['readComments', 'writeComment', 'updateComment']) {
    assert.equal(typeof linearOperationMap[key].tool, 'string')
    assert.notEqual(linearOperationMap[key].tool, '')
    assert.equal(typeof linearOperationMap[key].summary, 'string')
    assert.notEqual(linearOperationMap[key].summary, '')
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
