import { test } from 'node:test'
import assert from 'node:assert/strict'

import { trackerMcpPreflight } from './preflight.mjs'

const operationMap = {
  getIssue: { tool: 'mcp__acme-linear__get_issue' },
  saveIssue: { tool: 'mcp__acme-linear__save_issue' },
}
const base = { operationMap, mcpServer: 'acme-linear', agent: 'claude' }

test('a successful probe passes regardless of tool naming', () => {
  const r = trackerMcpPreflight({ ...base, availableTools: [], probeOk: true })
  assert.equal(r.ok, true)
  assert.equal(r.status, 'ok')
  assert.equal(r.message, '')
  assert.deepEqual(r.missing, [])
})

test('probe failure with no matching tools reports absent and names server + harness', () => {
  const r = trackerMcpPreflight({ ...base, availableTools: ['Bash', 'Read'], probeOk: false })
  assert.equal(r.ok, false)
  assert.equal(r.status, 'absent')
  assert.match(r.message, /acme-linear/)
  assert.match(r.message, /claude/)
  assert.match(r.message, /not present in this session/)
  assert.deepEqual(r.missing, ['mcp__acme-linear__get_issue', 'mcp__acme-linear__save_issue'])
})

test('probe failure with the tools present reports unreachable, not absent', () => {
  const r = trackerMcpPreflight({
    ...base,
    availableTools: ['mcp__acme-linear__get_issue', 'mcp__acme-linear__save_issue'],
    probeOk: false,
  })
  assert.equal(r.ok, false)
  assert.equal(r.status, 'unreachable')
  assert.match(r.message, /present in this session but unreachable/)
  assert.doesNotMatch(r.message, /not present in this session/)
  assert.deepEqual(r.missing, [])
})

test('a partially present tool set is unreachable and lists only what is missing', () => {
  const r = trackerMcpPreflight({
    ...base,
    availableTools: ['mcp__acme-linear__get_issue'],
    probeOk: false,
  })
  assert.equal(r.status, 'unreachable')
  assert.deepEqual(r.missing, ['mcp__acme-linear__save_issue'])
})

test('a harness that namespaces tools without the mcp__ prefix is not called absent', () => {
  const r = trackerMcpPreflight({
    ...base,
    agent: 'codex',
    availableTools: ['acme-linear__get_issue', 'acme-linear__save_issue'],
    probeOk: false,
  })
  assert.equal(r.status, 'unreachable')
  assert.match(r.message, /codex/)
})

test('an unnamed harness still produces a message naming the server', () => {
  const r = trackerMcpPreflight({ ...base, agent: '', availableTools: [], probeOk: false })
  assert.equal(r.status, 'absent')
  assert.match(r.message, /acme-linear/)
  assert.match(r.message, /unknown harness/)
})

test('the absent message tells the reader to fix the repo, not the credentials', () => {
  const r = trackerMcpPreflight({ ...base, availableTools: [], probeOk: false })
  assert.match(r.message, /the repo must declare/)
  assert.match(r.message, /Expected tools: /)
})

test('the unreachable message tells the reader to fix credentials, not the declaration', () => {
  const r = trackerMcpPreflight({
    ...base,
    availableTools: ['mcp__acme-linear__get_issue', 'mcp__acme-linear__save_issue'],
    probeOk: false,
  })
  assert.match(r.message, /credential environment and network/)
})

test('an operation map with no tools is absent rather than vacuously reachable', () => {
  const r = trackerMcpPreflight({
    operationMap: { viaCli: { command: 'linear-cli' } },
    mcpServer: 'acme-linear',
    agent: 'claude',
    availableTools: [],
    probeOk: false,
  })
  assert.equal(r.ok, false)
  assert.equal(r.status, 'absent')
  assert.deepEqual(r.missing, [])
  assert.match(r.message, /\(none declared\)/)
})

test('duplicate tool names across operations are reported once', () => {
  const r = trackerMcpPreflight({
    operationMap: {
      setState: { tool: 'mcp__acme-linear__save_issue' },
      setLabels: { tool: 'mcp__acme-linear__save_issue' },
    },
    mcpServer: 'acme-linear',
    agent: 'claude',
    availableTools: [],
    probeOk: false,
  })
  assert.deepEqual(r.missing, ['mcp__acme-linear__save_issue'])
})

test('called with no arguments it fails closed rather than throwing', () => {
  const r = trackerMcpPreflight()
  assert.equal(r.ok, false)
  assert.equal(r.status, 'absent')
})
