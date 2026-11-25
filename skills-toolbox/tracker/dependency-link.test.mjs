import { test } from 'node:test'
import assert from 'node:assert/strict'
import { execFileSync } from 'node:child_process'
import { writeFileSync } from 'node:fs'
import { join } from 'node:path'
import { tempDir } from '../../scripts/test-infra/test-tmpdir.mjs'
import { TRACKER_CREDENTIALS_MISSING } from './adapter-core.mjs'
import { OperationFailure } from './operation-runner.mjs'
import { transitiveBlockWarnings } from '../plan/plan-deps-lib.mjs'
import {
  LINK_DEPENDENCY_USAGE,
  decidePreWrite,
  evaluateLinkState,
  normalizeIssueRelations,
  runLinkDependency,
} from './dependency-link.mjs'

function makeClock() {
  let time = 0
  const sleeps = []
  return {
    now: () => time,
    sleep: async (ms) => {
      sleeps.push(ms)
      time += ms
    },
    sleeps,
  }
}

function makeIssue(id, { blockedBy = [], blocks = [], stateType = 'unstarted', uuid } = {}) {
  return {
    id,
    identifier: id,
    ...(uuid ? { uuid } : {}),
    ...(stateType !== undefined ? { state: { name: stateType ?? 'Open', type: stateType } } : {}),
    relations: { blockedBy, blocks },
  }
}

function commandOp(verb) {
  return { command: { helper: 'tracker/fake.mjs', verb }, summary: '{id, blockedBy}' }
}

function mcpOp(tool) {
  return { tool, summary: '{id, blockedBy}' }
}

function forbiddingAppendOpMap(extra = {}) {
  const map = { ...extra }
  Object.defineProperty(map, 'appendDependency', {
    enumerable: true,
    get() {
      throw new Error('appendDependency must not be resolved')
    },
  })
  return map
}

async function run(args, opts = {}) {
  const clock = opts.clock ?? makeClock()
  let stdout = ''
  let stderr = ''
  const code = await runLinkDependency(args, {
    write: (chunk) => {
      stdout += chunk
    },
    errWrite: (chunk) => {
      stderr += chunk
    },
    env: opts.env ?? {},
    resolveAdapter: opts.resolveAdapter ?? (() => opts.adapter),
    sleep: clock.sleep,
    now: clock.now,
    ...(opts.runOperation ? { runOperation: opts.runOperation } : {}),
  })
  const lines = stdout === '' ? [] : stdout.trimEnd().split('\n')
  const json = lines.length === 1 ? JSON.parse(lines[0]) : null
  return { code, stdout, stderr, lines, json, clock }
}

function writeJsonPair(t, issuePayload, blockerPayload) {
  const dir = tempDir('dependency-link-', t)
  const issueFile = join(dir, 'issue.json')
  const blockerFile = join(dir, 'blocker.json')
  writeFileSync(issueFile, JSON.stringify(issuePayload))
  writeFileSync(blockerFile, JSON.stringify(blockerPayload))
  return { dir, issueFile, blockerFile }
}

test('LINK_DEPENDENCY_USAGE names the verb and core flags', () => {
  assert.match(LINK_DEPENDENCY_USAGE, /link-dependency/)
  assert.match(LINK_DEPENDENCY_USAGE, /--issue/)
  assert.match(LINK_DEPENDENCY_USAGE, /--blocked-by/)
  assert.match(LINK_DEPENDENCY_USAGE, /--verify/)
})

test('1. self-edge (--issue A --blocked-by a) => refused-self-edge, exit 3, appendDependency never resolved', async () => {
  let adapterResolved = false
  const r = await run(['--issue', 'APP-1', '--blocked-by', 'app-1'], {
    resolveAdapter: () => {
      adapterResolved = true
      return { operationMap: forbiddingAppendOpMap() }
    },
  })
  assert.equal(r.code, 3)
  assert.equal(r.lines.length, 1)
  assert.equal(r.json.outcome, 'refused-self-edge')
  assert.equal(r.json.issue, 'APP-1')
  assert.equal(r.json.blockedBy, 'app-1')
  assert.equal(adapterResolved, false)
})

test('1b. self-edge via distinct alias and uuid => refused-self-edge, exit 3, appendDependency never resolved', async () => {
  const same = makeIssue('APP-1', { uuid: 'uuid-1' })
  const r = await run(['--issue', 'uuid-1', '--blocked-by', 'APP-1'], {
    adapter: {
      readIssueSnapshot: async () => same,
      operationMap: forbiddingAppendOpMap(),
    },
  })
  assert.equal(r.code, 3)
  assert.equal(r.json.outcome, 'refused-self-edge')
})

test('2. direct cycle seen from issue side, and from blocker side only => refused-cycle, exit 3', async () => {
  const fromIssue = await run(['--issue', 'APP-1', '--blocked-by', 'APP-2'], {
    adapter: {
      readIssueSnapshot: async (id) =>
        id === 'APP-1'
          ? makeIssue('APP-1', { blocks: ['APP-2'] })
          : makeIssue('APP-2', { blockedBy: [] }),
      operationMap: forbiddingAppendOpMap(),
    },
  })
  assert.equal(fromIssue.code, 3)
  assert.equal(fromIssue.json.outcome, 'refused-cycle')
  assert.deepEqual(fromIssue.json.cycle, ['APP-1', 'APP-2'])

  const fromBlockerOnly = await run(['--issue', 'APP-1', '--blocked-by', 'APP-2'], {
    adapter: {
      readIssueSnapshot: async (id) =>
        id === 'APP-1'
          ? makeIssue('APP-1', { blocks: [] })
          : makeIssue('APP-2', {
              blockedBy: [{ identifier: 'APP-1', state: { type: 'started' } }],
            }),
      operationMap: forbiddingAppendOpMap(),
    },
  })
  assert.equal(fromBlockerOnly.code, 3)
  assert.equal(fromBlockerOnly.json.outcome, 'refused-cycle')
  assert.deepEqual(fromBlockerOnly.json.cycle, ['APP-1', 'APP-2'])
})

test('3. two-hop cycle (blocker.blockedBy = [X], issue.blocks = [X]) => refused-cycle with cycle [issue, X, blocker], exit 3', async () => {
  const r = await run(['--issue', 'APP-1', '--blocked-by', 'APP-2'], {
    adapter: {
      readIssueSnapshot: async (id) =>
        id === 'APP-1'
          ? makeIssue('APP-1', { blocks: [{ identifier: 'APP-3', uuid: 'u-3' }] })
          : makeIssue('APP-2', { blockedBy: [{ id: 'app-3' }] }),
      operationMap: forbiddingAppendOpMap(),
    },
  })
  assert.equal(r.code, 3)
  assert.equal(r.json.outcome, 'refused-cycle')
  assert.deepEqual(r.json.cycle, ['APP-1', 'APP-3', 'APP-2'])
})

test('4. already on both sides => already-linked, exit 0, no write', async () => {
  const r = await run(['--issue', 'APP-1', '--blocked-by', 'APP-2'], {
    adapter: {
      readIssueSnapshot: async (id) =>
        id === 'APP-1'
          ? makeIssue('APP-1', { blockedBy: ['APP-2'] })
          : makeIssue('APP-2', { blocks: ['APP-1'] }),
      operationMap: forbiddingAppendOpMap(),
    },
  })
  assert.equal(r.code, 0)
  assert.equal(r.json.outcome, 'already-linked')
  assert.deepEqual(r.json.before, { issueHasBlockedBy: true, blockerHasBlocks: true })
})

test('5. only in issue.blockedBy => one-sided, exit 4, no write', async () => {
  const r = await run(['--issue', 'APP-1', '--blocked-by', 'APP-2'], {
    adapter: {
      readIssueSnapshot: async (id) =>
        id === 'APP-1'
          ? makeIssue('APP-1', { blockedBy: ['APP-2'] })
          : makeIssue('APP-2', { blocks: [] }),
      operationMap: forbiddingAppendOpMap(),
    },
  })
  assert.equal(r.code, 4)
  assert.equal(r.json.outcome, 'one-sided')
  assert.deepEqual(r.json.before, { issueHasBlockedBy: true, blockerHasBlocks: false })
  assert.ok(r.json.problems.some((p) => p.includes('APP-2')))
})

test('6. command transport, write succeeds, re-read shows both => linked, exit 0, one call with {id, blockedBy: [blocker]} and warnings passthrough', async () => {
  let linked = false
  const writeCalls = []
  const r = await run(['--issue', 'APP-1', '--blocked-by', 'APP-2'], {
    adapter: {
      readIssueSnapshot: async (id) => {
        if (id === 'APP-1') return makeIssue('APP-1', { blockedBy: linked ? ['APP-2'] : [] })
        return makeIssue('APP-2', { blocks: linked ? ['APP-1'] : [] })
      },
      operationMap: { appendDependency: commandOp('append-dependency') },
    },
    runOperation: (op, args) => {
      writeCalls.push({ op, args })
      linked = true
      return {
        transport: 'command',
        outcome: 'operation-executed',
        result: { id: 'APP-1', blockedBy: ['APP-2'], warnings: ['summary count lagging'] },
      }
    },
  })
  assert.equal(r.code, 0)
  assert.equal(r.json.outcome, 'linked')
  assert.equal(r.json.attempts, 1)
  assert.deepEqual(r.json.before, { issueHasBlockedBy: false, blockerHasBlocks: false })
  assert.deepEqual(r.json.after, { issueHasBlockedBy: true, blockerHasBlocks: true })
  assert.deepEqual(r.json.warnings, ['summary count lagging'])
  assert.equal(writeCalls.length, 1)
  assert.deepEqual(writeCalls[0].args, { id: 'APP-1', blockedBy: ['APP-2'] })
})

test('7. command transport, re-read shows neither through whole settle budget => missing, exit 4, one write call, attempts > 1', async () => {
  const writeCalls = []
  const r = await run(['--issue', 'APP-1', '--blocked-by', 'APP-2', '--settle-ms', '3000'], {
    adapter: {
      readIssueSnapshot: async (id) => makeIssue(id),
      operationMap: { appendDependency: commandOp('append-dependency') },
    },
    runOperation: (_op, args) => {
      writeCalls.push(args)
      return { transport: 'command', outcome: 'operation-executed', result: { ok: true } }
    },
  })
  assert.equal(r.code, 4)
  assert.equal(r.json.outcome, 'missing')
  assert.equal(writeCalls.length, 1)
  assert.ok(r.json.attempts > 1, `expected attempts > 1, got ${r.json.attempts}`)
  assert.deepEqual(r.json.after, { issueHasBlockedBy: false, blockerHasBlocks: false })
})

test('8. command transport, first re-read lags then shows both => linked, exit 0, one write call', async () => {
  let postReads = 0
  let wrote = false
  const writeCalls = []
  const r = await run(['--issue', 'APP-1', '--blocked-by', 'APP-2'], {
    adapter: {
      readIssueSnapshot: async (id) => {
        if (!wrote) return makeIssue(id)
        if (id === 'APP-1') postReads += 1
        const ready = postReads >= 2
        if (id === 'APP-1') return makeIssue('APP-1', { blockedBy: ready ? ['APP-2'] : [] })
        return makeIssue('APP-2', { blocks: ready ? ['APP-1'] : [] })
      },
      operationMap: { appendDependency: commandOp('append-dependency') },
    },
    runOperation: (_op, args) => {
      writeCalls.push(args)
      wrote = true
      return { transport: 'command', outcome: 'operation-executed', result: { ok: true } }
    },
  })
  assert.equal(r.code, 0)
  assert.equal(r.json.outcome, 'linked')
  assert.equal(r.json.attempts, 2)
  assert.equal(writeCalls.length, 1)
})

test('9. command transport, re-read shows only forward side => one-sided naming blocker side, exit 4', async () => {
  let wrote = false
  const r = await run(['--issue', 'APP-1', '--blocked-by', 'APP-2', '--settle-ms', '0'], {
    adapter: {
      readIssueSnapshot: async (id) => {
        if (id === 'APP-1') return makeIssue('APP-1', { blockedBy: wrote ? ['APP-2'] : [] })
        return makeIssue('APP-2', { blocks: [] })
      },
      operationMap: { appendDependency: commandOp('append-dependency') },
    },
    runOperation: () => {
      wrote = true
      return { transport: 'command', outcome: 'operation-executed', result: { ok: true } }
    },
  })
  assert.equal(r.code, 4)
  assert.equal(r.json.outcome, 'one-sided')
  assert.deepEqual(r.json.after, { issueHasBlockedBy: true, blockerHasBlocks: false })
  assert.ok(
    r.json.problems.some((p) => p.includes('APP-2') && p.includes('blocks')),
    `expected problem naming blocker side, got ${JSON.stringify(r.json.problems)}`,
  )
})

test('10. write throws definitive failure => exit 2, no read-back success claim', async () => {
  let reads = 0
  const r = await run(['--issue', 'APP-1', '--blocked-by', 'APP-2'], {
    adapter: {
      readIssueSnapshot: async (id) => {
        reads += 1
        return makeIssue(id)
      },
      operationMap: { appendDependency: commandOp('append-dependency') },
    },
    runOperation: () => {
      throw new OperationFailure(1, 'HTTP 422 validation failed: invalid issue id')
    },
  })
  assert.equal(r.code, 2)
  assert.equal(r.stdout, '')
  assert.match(r.stderr, /HTTP 422|validation/)
  assert.equal(reads, 2, 'must not run post-write read-back after definitive failure')
})

test('11. write throws tracker-outcome=indeterminate and re-read shows both => linked with writeError kept', async () => {
  let applied = false
  let writes = 0
  const r = await run(['--issue', 'APP-1', '--blocked-by', 'APP-2'], {
    adapter: {
      readIssueSnapshot: async (id) => {
        if (id === 'APP-1') return makeIssue('APP-1', { blockedBy: applied ? ['APP-2'] : [] })
        return makeIssue('APP-2', { blocks: applied ? ['APP-1'] : [] })
      },
      operationMap: { appendDependency: commandOp('append-dependency') },
    },
    runOperation: () => {
      writes += 1
      applied = true
      throw new OperationFailure(1, 'tracker-outcome=indeterminate; socket closed after POST')
    },
  })
  assert.equal(r.code, 0)
  assert.equal(r.json.outcome, 'linked')
  assert.equal(writes, 1)
  assert.match(r.json.writeError, /tracker-outcome=indeterminate/)
})

test('12. write times out and re-read shows neither => indeterminate, exit 4, one write call', async () => {
  let writes = 0
  const r = await run(['--issue', 'APP-1', '--blocked-by', 'APP-2', '--settle-ms', '2000'], {
    adapter: {
      readIssueSnapshot: async (id) => makeIssue(id),
      operationMap: { appendDependency: commandOp('append-dependency') },
    },
    runOperation: () => {
      writes += 1
      throw new OperationFailure('ETIMEDOUT', 'request timed out')
    },
  })
  assert.equal(r.code, 4)
  assert.equal(r.json.outcome, 'indeterminate')
  assert.equal(writes, 1)
  assert.match(r.json.writeError, /ETIMEDOUT|timed out/)
})

test('12b. write throws unrecognized error and re-read shows neither => indeterminate, exit 4, one write call', async () => {
  let writes = 0
  const r = await run(['--issue', 'APP-1', '--blocked-by', 'APP-2', '--settle-ms', '0'], {
    adapter: {
      readIssueSnapshot: async (id) => makeIssue(id),
      operationMap: { appendDependency: commandOp('append-dependency') },
    },
    runOperation: () => {
      writes += 1
      throw new OperationFailure(1, 'unexpected crash with no status')
    },
  })
  assert.equal(r.code, 4)
  assert.equal(r.json.outcome, 'indeterminate')
  assert.equal(writes, 1)
  assert.match(r.json.writeError, /unexpected crash/)
})

test('13. MCP transport => descriptor-emitted with tool, args, and next naming --verify, exit 0, nothing executed', async () => {
  let executed = 0
  const r = await run(['--issue', 'APP-1', '--blocked-by', 'APP-2'], {
    adapter: {
      readIssueSnapshot: async (id) => makeIssue(id),
      operationMap: { appendDependency: mcpOp('save_issue') },
    },
    runOperation: () => {
      executed += 1
      return { transport: 'mcp' }
    },
  })
  assert.equal(r.code, 0)
  assert.equal(r.json.outcome, 'descriptor-emitted')
  assert.equal(r.json.tool, 'save_issue')
  assert.deepEqual(r.json.args, { id: 'APP-1', blockedBy: ['APP-2'] })
  assert.match(r.json.next, /link-dependency.*--issue APP-1.*--blocked-by APP-2.*--verify/)
  assert.equal(executed, 0)
})

test('14. --verify with both / neither / one side => linked 0 / missing 4 / one-sided 4, appendDependency never resolved', async () => {
  const both = await run(['--issue', 'APP-1', '--blocked-by', 'APP-2', '--verify'], {
    adapter: {
      readIssueSnapshot: async (id) =>
        id === 'APP-1'
          ? makeIssue('APP-1', { blockedBy: ['APP-2'] })
          : makeIssue('APP-2', { blocks: ['APP-1'] }),
      operationMap: forbiddingAppendOpMap(),
    },
  })
  assert.equal(both.code, 0)
  assert.equal(both.json.outcome, 'linked')
  assert.equal(both.json.attempts, 1)

  const neither = await run(
    ['--issue', 'APP-1', '--blocked-by', 'APP-2', '--verify', '--settle-ms', '0'],
    {
      adapter: {
        readIssueSnapshot: async (id) => makeIssue(id),
        operationMap: forbiddingAppendOpMap(),
      },
    },
  )
  assert.equal(neither.code, 4)
  assert.equal(neither.json.outcome, 'missing')
  assert.equal(neither.json.attempts, 1)

  const oneSide = await run(
    ['--issue', 'APP-1', '--blocked-by', 'APP-2', '--verify', '--settle-ms', '0'],
    {
      adapter: {
        readIssueSnapshot: async (id) =>
          id === 'APP-1'
            ? makeIssue('APP-1', { blockedBy: [] })
            : makeIssue('APP-2', { blocks: ['APP-1'] }),
        operationMap: forbiddingAppendOpMap(),
      },
    },
  )
  assert.equal(oneSide.code, 4)
  assert.equal(oneSide.json.outcome, 'one-sided')
  assert.ok(oneSide.json.problems.some((p) => p.includes('APP-1') && p.includes('blockedBy')))
})

test('15. --verify parses as a boolean (not 64); repeated --verify => 64', async () => {
  const ok = await run(['--issue', 'APP-1', '--blocked-by', 'APP-2', '--verify'], {
    adapter: {
      readIssueSnapshot: async (id) =>
        id === 'APP-1'
          ? makeIssue('APP-1', { blockedBy: ['APP-2'] })
          : makeIssue('APP-2', { blocks: ['APP-1'] }),
      operationMap: forbiddingAppendOpMap(),
    },
  })
  assert.equal(ok.code, 0)

  const repeated = await run(['--issue', 'APP-1', '--blocked-by', 'APP-2', '--verify', '--verify'])
  assert.equal(repeated.code, 64)
  assert.equal(repeated.stdout, '')
  assert.match(repeated.stderr, /--verify/)
})

test('16. file pair in Linear MCP shape and in snapshot shape both normalize with stateType completed or null', async (t) => {
  const mcpFiles = writeJsonPair(
    t,
    {
      id: 'uuid-1',
      identifier: 'APP-1',
      statusType: 'completed',
      relations: {
        blockedBy: [{ id: 'APP-2', title: 'Blocker title' }],
        blocks: [],
      },
    },
    {
      id: 'APP-2',
      relations: {
        blockedBy: [],
        blocks: [{ id: 'APP-1', title: 'Dependent title' }],
      },
    },
  )
  const mcpRes = await run([
    '--issue',
    'APP-1',
    '--blocked-by',
    'APP-2',
    '--verify',
    '--issue-file',
    mcpFiles.issueFile,
    '--blocker-file',
    mcpFiles.blockerFile,
  ])
  assert.equal(mcpRes.code, 0)
  assert.equal(mcpRes.json.outcome, 'linked')
  assert.equal(mcpRes.json.source, 'files')
  assert.equal(mcpRes.json.relationsById['APP-1'].stateType, 'completed')
  assert.equal(mcpRes.json.relationsById['APP-2'].stateType, null)

  const snapFiles = writeJsonPair(
    t,
    {
      id: 'APP-1',
      identifier: 'APP-1',
      uuid: 'u-1',
      state: { name: 'Done', type: 'completed' },
      relations: {
        blockedBy: [
          {
            id: 'APP-2',
            identifier: 'APP-2',
            uuid: 'u-2',
            state: { name: 'Todo', type: 'unstarted' },
          },
        ],
        blocks: [],
      },
    },
    {
      id: 'APP-2',
      identifier: 'APP-2',
      uuid: 'u-2',
      relations: {
        blockedBy: [],
        blocks: [
          {
            id: 'APP-1',
            identifier: 'APP-1',
            uuid: 'u-1',
            state: { name: 'Done', type: 'completed' },
          },
        ],
      },
    },
  )
  const snapRes = await run([
    '--issue',
    'APP-1',
    '--blocked-by',
    'APP-2',
    '--verify',
    '--issue-file',
    snapFiles.issueFile,
    '--blocker-file',
    snapFiles.blockerFile,
  ])
  assert.equal(snapRes.code, 0)
  assert.equal(snapRes.json.outcome, 'linked')
  assert.equal(snapRes.json.relationsById['APP-1'].stateType, 'completed')
  assert.equal(snapRes.json.relationsById['APP-2'].stateType, null)
})

test('17. payload without relations.blockedBy => exit 2, diagnostic names the file', async (t) => {
  const files = writeJsonPair(
    t,
    { id: 'APP-1', identifier: 'APP-1', relations: { blocks: [] } },
    makeIssue('APP-2'),
  )
  const r = await run([
    '--issue',
    'APP-1',
    '--blocked-by',
    'APP-2',
    '--issue-file',
    files.issueFile,
    '--blocker-file',
    files.blockerFile,
  ])
  assert.equal(r.code, 2)
  assert.equal(r.stdout, '')
  assert.ok(
    r.stderr.includes(files.issueFile),
    `expected stderr to name ${files.issueFile}, got: ${r.stderr}`,
  )
})

test('18. --issue-file without --blocker-file (and other usage errors) => exit 64', async (t) => {
  const files = writeJsonPair(t, makeIssue('APP-1'), makeIssue('APP-2'))
  for (const args of [
    ['--issue', 'APP-1', '--blocked-by', 'APP-2', '--issue-file', files.issueFile],
    ['--issue', 'APP-1', '--blocked-by', 'APP-2', '--blocker-file', files.blockerFile],
    ['--issue', 'APP-1', '--blocked-by', 'APP-2', '--settle-ms', '-5'],
    ['--issue', 'APP-1', '--blocked-by', 'APP-2', '--settle-ms', 'abc'],
    ['--issue', 'APP-1', '--blocked-by', 'APP-2', '--unknown'],
    ['--issue', 'APP-1', '--blocked-by', 'APP-2', 'extra'],
    ['--issue', 'APP-1'],
  ]) {
    const r = await run(args)
    assert.equal(r.code, 64, `expected 64 for ${JSON.stringify(args)}, got ${r.code}`)
    assert.equal(r.stdout, '')
  }
})

test('19. readIssueSnapshot throws TRACKER_CREDENTIALS_MISSING and getIssue is MCP => exit 2 naming MCP getIssue file fallback', async () => {
  const r = await run(['--issue', 'APP-1', '--blocked-by', 'APP-2'], {
    adapter: {
      readIssueSnapshot: async () => {
        throw Object.assign(new Error('API key is not set'), {
          code: TRACKER_CREDENTIALS_MISSING,
        })
      },
      operationMap: { getIssue: mcpOp('get_issue'), appendDependency: mcpOp('save_issue') },
    },
  })
  assert.equal(r.code, 2)
  assert.equal(r.stdout, '')
  assert.match(r.stderr, /getIssue/)
  assert.match(r.stderr, /--issue-file/)
  assert.match(r.stderr, /--blocker-file/)
  assert.equal(r.stderr.trim().split('\n').length, 1)
})

test('20. snapshot without relations and command getIssue => read through runOperation', async () => {
  const getIssueCalls = []
  const r = await run(['--issue', 'GH-1', '--blocked-by', 'GH-2', '--verify'], {
    adapter: {
      readIssueSnapshot: async (id) => ({ id, identifier: id, title: 'No relations' }),
      operationMap: {
        getIssue: commandOp('get-issue'),
        appendDependency: commandOp('append-dependency'),
      },
    },
    runOperation: (op, args) => {
      getIssueCalls.push({ verb: op.command.verb, args })
      if (args.id === 'GH-1') {
        return {
          transport: 'command',
          outcome: 'operation-executed',
          result: makeIssue('GH-1', { blockedBy: ['GH-2'] }),
        }
      }
      return {
        transport: 'command',
        outcome: 'operation-executed',
        result: makeIssue('GH-2', { blocks: ['GH-1'] }),
      }
    },
  })
  assert.equal(r.code, 0)
  assert.equal(r.json.outcome, 'linked')
  assert.equal(r.json.source, 'getIssue-command')
  assert.deepEqual(getIssueCalls, [
    { verb: 'get-issue', args: { id: 'GH-1' } },
    { verb: 'get-issue', args: { id: 'GH-2' } },
  ])
})

test('21. relationsById keys both issues by identifier with stateType, blockedBy, blocks and feeds transitiveBlockWarnings', async () => {
  const r = await run(['--issue', 'APP-1', '--blocked-by', 'APP-2'], {
    adapter: {
      readIssueSnapshot: async (id) =>
        id === 'APP-1'
          ? makeIssue('APP-1', { stateType: 'unstarted', blockedBy: [], blocks: [] })
          : makeIssue('APP-2', {
              stateType: 'started',
              blockedBy: [
                { id: 'APP-3', identifier: 'APP-3', state: { name: 'Todo', type: 'unstarted' } },
                { id: 'APP-4', identifier: 'APP-4', state: { name: 'Done', type: 'completed' } },
              ],
              blocks: [],
            }),
      operationMap: { appendDependency: mcpOp('save_issue') },
    },
  })
  assert.equal(r.code, 0)
  assert.equal(r.json.outcome, 'descriptor-emitted')
  assert.deepEqual(Object.keys(r.json.relationsById).sort(), ['APP-1', 'APP-2'])
  assert.deepEqual(r.json.relationsById['APP-1'], {
    stateType: 'unstarted',
    blockedBy: [],
    blocks: [],
  })
  assert.deepEqual(r.json.relationsById['APP-2'], {
    stateType: 'started',
    blockedBy: [
      { id: 'APP-3', stateType: 'unstarted' },
      { id: 'APP-4', stateType: 'completed' },
    ],
    blocks: [],
  })
  const warnings = transitiveBlockWarnings({
    subjectId: 'APP-1',
    writes: [{ id: 'APP-1', blockedBy: ['APP-2'] }],
    relationsById: r.json.relationsById,
  })
  assert.equal(warnings.length, 1)
  assert.equal(warnings[0].direction, 'upstream')
  assert.deepEqual(warnings[0].via, ['APP-3'])
})

test('pure helpers: normalizeIssueRelations, evaluateLinkState, decidePreWrite', () => {
  const issue = normalizeIssueRelations(
    makeIssue('APP-1', { blockedBy: ['APP-2'] }),
    'APP-1',
    'APP-1',
  )
  const blocker = normalizeIssueRelations(
    makeIssue('APP-2', { blocks: ['APP-1'] }),
    'APP-2',
    'APP-2',
  )
  const link = evaluateLinkState(issue, blocker)
  assert.equal(link.outcome, 'linked')
  assert.deepEqual(link.state, { issueHasBlockedBy: true, blockerHasBlocks: true })
  const decision = decidePreWrite(issue, blocker)
  assert.equal(decision.outcome, 'already-linked')
  assert.equal(decision.exitCode, 0)
})

test('22. MCP descriptor next command POSIX single-quotes metacharacters so shells never expand them', async () => {
  const nastyIssue = "APP-$(echo PWNED)'1"
  const nastyBlocker = 'APP-`echo PWNED` $HOME'
  const r = await run(['--issue', nastyIssue, '--blocked-by', nastyBlocker], {
    adapter: {
      readIssueSnapshot: async (id) => makeIssue(id),
      operationMap: { appendDependency: mcpOp('save_issue') },
    },
  })
  assert.equal(r.code, 0)
  assert.equal(r.json.outcome, 'descriptor-emitted')
  assert.doesNotMatch(r.json.next, /"/)
  const evaluated = execFileSync(
    'sh',
    ['-c', `set -- ${r.json.next}; printf '%s\\0' "$@"`],
    { encoding: 'utf8' },
  )
    .split('\0')
    .slice(0, -1)
  const issueIdx = evaluated.indexOf('--issue')
  const blockerIdx = evaluated.indexOf('--blocked-by')
  assert.ok(issueIdx >= 0 && blockerIdx >= 0)
  assert.equal(evaluated[issueIdx + 1], nastyIssue)
  assert.equal(evaluated[blockerIdx + 1], nastyBlocker)
})

test('23. command write with --issue-file/--blocker-file verifies through live getIssue instead of stale files', async (t) => {
  const files = writeJsonPair(t, makeIssue('GH-1'), makeIssue('GH-2'))
  const ops = []
  let getIssueReads = 0
  const r = await run(
    [
      '--issue',
      'GH-1',
      '--blocked-by',
      'GH-2',
      '--issue-file',
      files.issueFile,
      '--blocker-file',
      files.blockerFile,
    ],
    {
      adapter: {
        operationMap: {
          getIssue: commandOp('get-issue'),
          appendDependency: commandOp('append-dependency'),
        },
      },
      runOperation: (op, args) => {
        ops.push({ verb: op.command.verb, args })
        if (op.command.verb === 'append-dependency') {
          return { transport: 'command', outcome: 'operation-executed', result: { ok: true } }
        }
        if (args.id === 'GH-1') getIssueReads += 1
        const ready = getIssueReads >= 2
        if (args.id === 'GH-1') {
          return {
            transport: 'command',
            outcome: 'operation-executed',
            result: makeIssue('GH-1', { blockedBy: ready ? ['GH-2'] : [] }),
          }
        }
        return {
          transport: 'command',
          outcome: 'operation-executed',
          result: makeIssue('GH-2', { blocks: ready ? ['GH-1'] : [] }),
        }
      },
    },
  )
  assert.equal(r.code, 0)
  assert.equal(r.json.outcome, 'linked')
  assert.equal(r.json.source, 'getIssue-command')
  assert.equal(r.json.attempts, 2)
  assert.deepEqual(r.json.after, { issueHasBlockedBy: true, blockerHasBlocks: true })
  assert.equal(
    ops.filter((o) => o.verb === 'append-dependency').length,
    1,
    'must execute appendDependency once',
  )
})

test('24. command write with --issue-file/--blocker-file and no live reader emits next verify command instead of failing on stale files', async (t) => {
  const files = writeJsonPair(t, makeIssue('APP-1'), makeIssue('APP-2'))
  let writes = 0
  const ok = await run(
    [
      '--issue',
      'APP-1',
      '--blocked-by',
      'APP-2',
      '--issue-file',
      files.issueFile,
      '--blocker-file',
      files.blockerFile,
    ],
    {
      adapter: {
        operationMap: {
          getIssue: mcpOp('get_issue'),
          appendDependency: commandOp('append-dependency'),
        },
      },
      runOperation: (_op, args) => {
        writes += 1
        assert.deepEqual(args, { id: 'APP-1', blockedBy: ['APP-2'] })
        return {
          transport: 'command',
          outcome: 'operation-executed',
          result: { ok: true, warnings: ['check inverse'] },
        }
      },
    },
  )
  assert.equal(ok.code, 0)
  assert.equal(ok.json.outcome, 'verification-required')
  assert.equal(ok.json.source, 'files')
  assert.deepEqual(ok.json.warnings, ['check inverse'])
  assert.match(
    ok.json.next,
    /link-dependency --issue APP-1 --blocked-by APP-2 --verify --issue-file <fresh-issue-file> --blocker-file <fresh-blocker-file>/,
  )
  assert.equal(writes, 1)

  const timedOut = await run(
    [
      '--issue',
      'APP-1',
      '--blocked-by',
      'APP-2',
      '--issue-file',
      files.issueFile,
      '--blocker-file',
      files.blockerFile,
    ],
    {
      adapter: {
        operationMap: {
          getIssue: mcpOp('get_issue'),
          appendDependency: commandOp('append-dependency'),
        },
      },
      runOperation: () => {
        throw new OperationFailure('ETIMEDOUT', 'request timed out')
      },
    },
  )
  assert.equal(timedOut.code, 4)
  assert.equal(timedOut.json.outcome, 'indeterminate')
  assert.match(timedOut.json.writeError, /ETIMEDOUT|timed out/)
  assert.match(timedOut.json.next, /--verify --issue-file <fresh-issue-file> --blocker-file <fresh-blocker-file>/)
})
