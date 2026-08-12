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

// --- the optional declaration report -------------------------------------------------
//
// A server that fails at CONNECT publishes no tools, so the tool list alone cannot tell it apart
// from one the repo never declared — and the two have opposite fixes. These cases cover the second,
// optional evidence channel that separates them, and (just as importantly) pin that a caller which
// supplies nothing keeps today's behaviour to the byte.

// The literal message a no-signal caller sees today. Spelled out rather than computed so that a
// future reword of the absent branch fails HERE, where the degradation contract is stated, instead
// of silently changing what an already-installed caller reads.
const ABSENT_MESSAGE_TODAY = `tracker MCP server "acme-linear" is not present in this session (harness: claude). MCP servers are not configured by the session runner — the repo must declare "acme-linear" through claude's own mechanism, and where that harness has a separate approval step it must be enabled there too. Expected tools: mcp__acme-linear__get_issue, mcp__acme-linear__save_issue.`

test('a declared server that published no usable tools is unreachable, not absent', () => {
  const r = trackerMcpPreflight({
    ...base,
    availableTools: [],
    probeOk: false,
    declaredServers: [{ name: 'acme-linear', toolCount: 0, authStatus: 'unauthenticated' }],
  })
  assert.equal(r.ok, false)
  assert.equal(r.status, 'unreachable')
  assert.equal(r.declared, true)
  // The whole point: the operator must NOT be sent to fix a declaration that is already correct.
  assert.doesNotMatch(r.message, /not present in this session/)
  assert.doesNotMatch(r.message, /the repo must declare/)
  assert.match(r.message, /credential environment and network/)
  assert.match(r.message, /acme-linear/)
})

test('the declared-but-empty message reports the tool count and the auth status it was given', () => {
  const r = trackerMcpPreflight({
    ...base,
    availableTools: [],
    probeOk: false,
    declaredServers: [{ name: 'acme-linear', toolCount: 0, authStatus: 'unauthenticated' }],
  })
  assert.match(r.message, /0 tools/)
  assert.match(r.message, /unauthenticated/)
})

test('a report naming only other servers leaves the verdict absent', () => {
  const r = trackerMcpPreflight({
    ...base,
    availableTools: [],
    probeOk: false,
    declaredServers: [{ name: 'acme-other', toolCount: 7 }],
  })
  assert.equal(r.status, 'absent')
  assert.equal(r.declared, false)
  // Still the repo's problem, and the message must still say so.
  assert.match(r.message, /the repo must declare/)
})

test('an omitted declaration report degrades to today absent message exactly', () => {
  const r = trackerMcpPreflight({ ...base, availableTools: [], probeOk: false })
  assert.equal(r.status, 'absent')
  assert.equal(r.message, ABSENT_MESSAGE_TODAY)
  assert.equal(r.declared, null)
})

test('null, undefined, empty and non-array reports all degrade like an omitted one', () => {
  // Every already-installed caller passes no report. If any of these shapes changed the verdict,
  // upgrading the module would silently change what those callers see.
  const omitted = trackerMcpPreflight({ ...base, availableTools: [], probeOk: false })
  for (const declaredServers of [null, undefined, [], {}, 'acme-linear', 42]) {
    const r = trackerMcpPreflight({ ...base, availableTools: [], probeOk: false, declaredServers })
    assert.equal(r.status, omitted.status, `status for ${JSON.stringify(declaredServers)}`)
    assert.equal(r.message, ABSENT_MESSAGE_TODAY, `message for ${JSON.stringify(declaredServers)}`)
    assert.equal(r.declared, null, `declared for ${JSON.stringify(declaredServers)}`)
  }
})

test('every pre-existing case is byte-identical across all no-signal report shapes', () => {
  const preExisting = [
    { ...base, availableTools: [], probeOk: true },
    { ...base, availableTools: ['Bash', 'Read'], probeOk: false },
    {
      ...base,
      availableTools: ['mcp__acme-linear__get_issue', 'mcp__acme-linear__save_issue'],
      probeOk: false,
    },
    { ...base, availableTools: ['mcp__acme-linear__get_issue'], probeOk: false },
    {
      ...base,
      agent: 'codex',
      availableTools: ['acme-linear__get_issue', 'acme-linear__save_issue'],
      probeOk: false,
    },
    { ...base, agent: '', availableTools: [], probeOk: false },
    {
      operationMap: { viaCli: { command: 'linear-cli' } },
      mcpServer: 'acme-linear',
      agent: 'claude',
      availableTools: [],
      probeOk: false,
    },
  ]
  for (const input of preExisting) {
    const baseline = trackerMcpPreflight(input)
    for (const declaredServers of [null, undefined, [], {}, 'nope', 0]) {
      const r = trackerMcpPreflight({ ...input, declaredServers })
      assert.equal(r.status, baseline.status, `status ${JSON.stringify(input)}`)
      assert.equal(r.message, baseline.message, `message ${JSON.stringify(input)}`)
      assert.deepEqual(r.missing, baseline.missing, `missing ${JSON.stringify(input)}`)
      assert.equal(r.ok, baseline.ok, `ok ${JSON.stringify(input)}`)
    }
  }
})

test('probeOk still short-circuits past a report that omits the server', () => {
  const r = trackerMcpPreflight({
    ...base,
    availableTools: [],
    probeOk: true,
    declaredServers: [{ name: 'acme-other' }],
  })
  assert.equal(r.ok, true)
  assert.equal(r.status, 'ok')
  assert.equal(r.message, '')
  assert.deepEqual(r.missing, [])
})

test('visible expected tools still win over the declaration report', () => {
  // Proves the new branch was inserted AFTER the tool-visibility test, not before it.
  const r = trackerMcpPreflight({
    ...base,
    availableTools: ['mcp__acme-linear__get_issue', 'mcp__acme-linear__save_issue'],
    probeOk: false,
    declaredServers: [{ name: 'acme-other' }],
  })
  assert.equal(r.status, 'unreachable')
  assert.match(r.message, /present in this session but unreachable/)
  assert.deepEqual(r.missing, [])
})

test('partial tool visibility is unchanged whether or not a report is supplied', () => {
  const withReport = trackerMcpPreflight({
    ...base,
    availableTools: ['mcp__acme-linear__get_issue'],
    probeOk: false,
    declaredServers: [{ name: 'acme-linear', toolCount: 1 }],
  })
  const without = trackerMcpPreflight({
    ...base,
    availableTools: ['mcp__acme-linear__get_issue'],
    probeOk: false,
  })
  assert.equal(withReport.status, 'unreachable')
  assert.equal(withReport.message, without.message)
  assert.deepEqual(withReport.missing, ['mcp__acme-linear__save_issue'])
})

test('declaration-report name matching is exact', () => {
  // A near-miss must not silently flip absent to unreachable: case and surrounding whitespace are
  // exactly how a mis-keyed declaration presents, and calling that "declared" hides the real bug.
  for (const name of ['ACME-Linear', ' acme-linear', 'acme-linear ', 'acme-linear\n']) {
    const r = trackerMcpPreflight({
      ...base,
      availableTools: [],
      probeOk: false,
      declaredServers: [{ name }],
    })
    assert.equal(r.status, 'absent', `name ${JSON.stringify(name)}`)
    assert.equal(r.declared, false, `declared ${JSON.stringify(name)}`)
  }
})

test('duplicate records naming the same server classify once and do not throw', () => {
  const r = trackerMcpPreflight({
    ...base,
    availableTools: [],
    probeOk: false,
    declaredServers: [
      { name: 'acme-linear', toolCount: 0 },
      { name: 'acme-linear', toolCount: 0 },
    ],
  })
  assert.equal(r.status, 'unreachable')
  assert.equal(r.declared, true)
})

test('malformed records inside a report are skipped rather than thrown on', () => {
  const r = trackerMcpPreflight({
    ...base,
    availableTools: [],
    probeOk: false,
    declaredServers: [null, 'acme-linear', { noName: true }, { name: 42 }, { name: 'acme-linear' }],
  })
  assert.equal(r.status, 'unreachable')
  assert.equal(r.declared, true)
})

test('an unconfigured server name never matches a record, even an empty-named one', () => {
  // '' is the mcpServer default. Matching it against a malformed empty-named record would report a
  // server that has no name as declared, and send the reader to check credentials for nothing.
  const r = trackerMcpPreflight({
    operationMap,
    mcpServer: '',
    agent: 'claude',
    availableTools: [],
    probeOk: false,
    declaredServers: [{ name: '', toolCount: 0 }],
  })
  assert.equal(r.status, 'absent')
  assert.equal(r.declared, false)
})

test('a report carrying the snake_case field names the CLI emits is understood', () => {
  // The report maps 1:1 onto the host's own JSON, so the caller does no interpretation.
  const r = trackerMcpPreflight({
    ...base,
    availableTools: [],
    probeOk: false,
    declaredServers: [{ name: 'acme-linear', tool_count: 0, auth_status: 'unauthenticated' }],
  })
  assert.equal(r.status, 'unreachable')
  assert.match(r.message, /0 tools/)
  assert.match(r.message, /unauthenticated/)
})
