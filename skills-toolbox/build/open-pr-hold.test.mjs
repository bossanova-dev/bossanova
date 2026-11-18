import { test } from 'node:test'
import assert from 'node:assert/strict'
import { mkdtempSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { decideOpenPrHold, holdComment, runCli } from './open-pr-hold.mjs'
import {
  DEFAULT_CONFIG,
  DEFAULT_TRACKER_STATES,
  mergeConfig,
  stageSelectionQuery,
} from '../skill-config.mjs'
import { decideNoChangeDisposition } from './no-change-disposition.mjs'
import { createGithubAdapter } from '../tracker/github.mjs'

const config = mergeConfig(DEFAULT_CONFIG, {
  adapters: { ...DEFAULT_CONFIG.adapters, tracker: 'demo' },
  trackerConfig: {
    demo: { labels: { agentBuild: 'build', agentPlan: 'plan' }, states: DEFAULT_TRACKER_STATES },
  },
})
const path = 'services/mobile/src/Choice.tsx'
const issue = { id: 'uuid', identifier: 'ABC-1', description: `## Key changes\n- \`${path}\`` }
const pr = {
  number: 42,
  url: 'https://example.test/pull/42',
  title: 'Choice',
  headRefName: 'choice',
  isDraft: true,
  files: [path],
  filesTruncated: false,
  ticketIds: [],
}
const snapshot = { listTruncated: false, pullRequests: [pr] }
const decide = (overrides = {}) =>
  decideOpenPrHold({
    config,
    issue,
    planText: '',
    snapshot,
    rootEntries: { dirs: ['services'], files: [], knownFiles: [path] },
    ...overrides,
  })

test('unplanned exact file holds and sibling files do not', () => {
  const d = decide()
  assert.equal(d.route, 'hold')
  assert.equal(d.overlaps[0].level, 'file')
  assert.equal(d.overlaps[0].planned, false)
  assert.equal(
    decide({
      snapshot: {
        ...snapshot,
        pullRequests: [{ ...pr, files: ['services/mobile/src/Other.tsx'] }],
      },
    }).route,
    'clear',
  )
})

test('only mentions above Original notes count, with whole PR and ticket tokens', () => {
  for (const mention of ['#42', pr.url, 'ABC-2']) {
    const data = { ...snapshot, pullRequests: [{ ...pr, ticketIds: ['ABC-2'] }] }
    assert.equal(decide({ snapshot: data, planText: `## Risks\n${mention}` }).route, 'clear')
    assert.equal(
      decide({
        snapshot: data,
        issue: {
          ...issue,
          description: `${issue.description}\nRelated (non-blocking, file overlap): ${mention}`,
        },
      }).route,
      'clear',
    )
    assert.equal(
      decide({ snapshot: data, planText: `## Original notes\n${mention}` }).route,
      'hold',
    )
    assert.equal(
      decide({
        snapshot: data,
        issue: { ...issue, description: `${issue.description}\n## Original notes\n${mention}` },
      }).route,
      'hold',
    )
  }
  assert.equal(decide({ planText: '#4' }).route, 'hold')
  assert.equal(decide({ planText: '#420' }).route, 'hold')
})

test('PR URL references match complete URLs, including markdown and prose', () => {
  for (const mention of [`${pr.url}0`, `${pr.url}/files`, `${pr.url}?view=files`])
    assert.equal(decide({ planText: mention }).route, 'hold')
  for (const mention of [pr.url, `[prior PR](${pr.url})`, `<${pr.url}>`, `${pr.url}.`])
    assert.equal(decide({ planText: mention }).route, 'clear')
})

test('incidental inferred IDs cannot substitute for tracker references', () => {
  const data = { ...snapshot, pullRequests: [{ ...pr, ticketIds: ['ABC-2', 'UTF-8'] }] }
  assert.equal(decide({ snapshot: data, planText: 'Fix UTF-8 handling' }).route, 'hold')
  assert.equal(decide({ snapshot: data, planText: 'After ABC-2' }).route, 'clear')
})

test('uses plan Key changes, excludes own PRs, and reports missing areas and truncation', () => {
  assert.equal(
    decide({ issue: { ...issue, description: '' }, planText: issue.description }).route,
    'hold',
  )
  assert.equal(decide({ ownPr: 42 }).route, 'clear')
  assert.equal(
    decide({ snapshot: { ...snapshot, pullRequests: [{ ...pr, ticketIds: ['ABC-1'] }] } }).route,
    'clear',
  )
  assert.equal(decide({ issue: { ...issue, description: '' } }).reason, 'no-key-changes')
  assert.equal(
    decide({ issue: { ...issue, description: `## Risks\n${path}` } }).reason,
    'no-key-changes',
  )
  const d = decide({
    snapshot: {
      listTruncated: true,
      pullRequests: [{ ...pr, files: ['other/file.js'], filesTruncated: true }],
    },
  })
  assert.equal(d.route, 'clear')
  assert.equal(d.warnings.length, 2)
})

async function run(t, flags = [], deps = {}, values = {}) {
  const dir = mkdtempSync(join(tmpdir(), 'open-pr-hold-'))
  t.after(() => rmSync(dir, { recursive: true, force: true, maxRetries: 3 }))
  for (const [file, value] of Object.entries({
    issue: JSON.stringify(issue),
    plan: '',
    snapshot: JSON.stringify(snapshot),
    ...values,
  }))
    writeFileSync(join(dir, file), value)
  return runCli(
    [
      'check',
      '--issue-json',
      join(dir, 'issue'),
      '--plan',
      join(dir, 'plan'),
      '--open-prs',
      join(dir, 'snapshot'),
      ...flags,
    ],
    { config, cwd: process.cwd(), ...deps },
  )
}

test('CLI holds without writes and clear --apply does not write', async (t) => {
  assert.equal((await run(t)).code, 3)
  const r = await run(
    t,
    ['--apply'],
    {
      adapter: {
        applyIssueWrites() {
          assert.fail('clear must not write')
        },
      },
    },
    { plan: '#42' },
  )
  assert.equal(r.code, 0)
})

test('a PR with no ticket sends the issue back to planning, with bounded evidence and a stable marker', async (t) => {
  const calls = []
  const r = await run(t, ['--apply'], {
    adapter: {
      async applyIssueWrites(writes) {
        calls.push(writes)
        return { outcome: 'ok' }
      },
    },
  })
  assert.equal(r.code, 3)
  assert.equal(r.out.outcome, 'ok')
  assert.equal(calls.length, 2)
  assert.deepEqual(calls[0].addLabels, ['plan'])
  assert.deepEqual(calls[1].removeLabels, ['build'])
  assert.equal(calls[0].addBlockedBy, undefined)
  assert.equal(calls[0].stateName, DEFAULT_TRACKER_STATES.unplanned)
  assert.match(calls[0].comment, /#42 has no open ticket/)
  assert.match(calls[0].marker, /#42/)
  for (const text of ['#42', pr.url, path])
    assert.ok(calls[0].comment.toLowerCase().includes(text.toLowerCase()))
  const many = {
    ...decide(),
    overlaps: Array.from({ length: 7 }, (_, i) => ({
      ...decide().overlaps[0],
      number: i + 1,
      shared: Array.from({ length: 12 }, (_, n) => `file${n}`),
    })),
  }
  assert.match(holdComment(many), /\+2 more/)
  assert.ok(!holdComment(many).includes('file10'))
})

test('changed overlap sets publish fresh comments while identical sets deduplicate', async (t) => {
  const comments = []
  const adapter = {
    async applyIssueWrites(writes) {
      if (writes.comment && !comments.some((body) => body.includes(writes.marker)))
        comments.push(`${writes.comment}\n${writes.marker}`)
      return { outcome: 'ok' }
    },
  }
  for (const numbers of [[12, 13], [12], [1], [12]]) {
    const data = {
      ...snapshot,
      pullRequests: numbers.map((number) => ({
        ...pr,
        number,
        url: `https://example.test/pull/${number}`,
      })),
    }
    const result = await run(t, ['--apply'], { adapter }, { snapshot: JSON.stringify(data) })
    assert.equal(result.code, 3)
  }
  assert.equal(comments.length, 3)
  for (const [index, number] of [12, 12, 1].entries())
    assert.ok(comments[index].includes(`- #${number} https://example.test/pull/${number}:`))
})

test('failed and indeterminate writes are never retried; missing capabilities fail closed', async (t) => {
  for (const outcome of ['failed', 'indeterminate']) {
    let calls = 0
    const r = await run(t, ['--apply'], {
      adapter: {
        async applyIssueWrites() {
          calls++
          return { outcome }
        },
      },
    })
    assert.equal(r.code, 4)
    assert.equal(calls, 1)
    assert.ok(r.out.reason)
  }
  assert.equal((await run(t, ['--apply'], { adapter: {} })).code, 4)
  assert.equal(
    (
      await run(t, ['--apply'], {
        config: mergeConfig(config, { trackerConfig: { demo: { labels: { agentPlan: null } } } }),
        adapter: { applyIssueWrites: () => assert.fail('an unresolved label must not write') },
      })
    ).code,
    4,
  )
})

test('a PR whose ticket is open becomes a blocker and keeps the build label', async (t) => {
  const data = (ticketIds) =>
    JSON.stringify({
      ...snapshot,
      pullRequests: [
        { ...pr, ticketIds },
        { ...pr, number: 43, url: 'https://example.test/pull/43', ticketIds: ['ABC-9'] },
      ],
    })
  const states = {
    'ABC-7': { type: 'started' },
    'ABC-8': { type: 'unstarted' },
    'ABC-9': { type: 'started' },
  }
  const calls = []
  const adapter = {
    readIssueState: async (id) => ({ identifier: id, state: states[id] }),
    async applyIssueWrites(writes) {
      calls.push(writes)
      return { outcome: 'ok' }
    },
  }
  const r = await run(t, ['--apply'], { adapter }, { snapshot: data(['ABC-8', 'ABC-7']) })
  assert.equal(r.code, 3)
  assert.deepEqual(calls[0].addBlockedBy, ['ABC-7', 'ABC-8', 'ABC-9'])
  assert.equal(calls[0].addLabels, undefined)
  assert.equal(calls[0].removeLabels, undefined)
  assert.equal(calls[0].stateName, undefined)
  assert.match(calls[0].comment, /Blocked by ABC-7, ABC-8, ABC-9/)
  assert.match(calls[0].marker, /#42, #43/)
})

test('a held PR whose ticket already blocks this issue sends it back to planning', async (t) => {
  // Selection let the ticket through despite that edge (an In Review soft-clear or a named run),
  // so keeping the build label would claim and hold it again on every run.
  const calls = []
  const adapter = {
    readIssueState: async (id) => ({ identifier: id, state: { type: 'started' } }),
    readBlockers: async (ids) => ids.map((id) => ({ id, blockers: [{ id: 'abc-7' }] })),
    async applyIssueWrites(writes) {
      calls.push(writes)
      return { outcome: 'ok' }
    },
  }
  const data = (ids) =>
    JSON.stringify({
      ...snapshot,
      pullRequests: [
        { ...pr, ticketIds: ids },
        { ...pr, number: 43, url: 'https://example.test/pull/43', ticketIds: ['ABC-9'] },
      ],
    })
  assert.equal((await run(t, ['--apply'], { adapter }, { snapshot: data(['ABC-7']) })).code, 3)
  assert.deepEqual(calls[0].addBlockedBy, ['ABC-9'])
  assert.deepEqual(calls[0].addLabels, ['plan'])
  assert.equal(calls[0].stateName, DEFAULT_TRACKER_STATES.unplanned)
  assert.deepEqual(calls[1].removeLabels, ['build'])
  calls.length = 0
  // A PR with a further open ticket still gains a new edge, which can hold the build.
  assert.equal(
    (await run(t, ['--apply'], { adapter }, { snapshot: data(['ABC-7', 'ABC-8']) })).code,
    3,
  )
  assert.deepEqual(calls[0].addBlockedBy, ['ABC-7', 'ABC-8', 'ABC-9'])
  assert.equal(calls[0].addLabels, undefined)
  assert.equal(calls.length, 1)
})

test('a done, unreadable or unreadable-capability ticket counts as no ticket', async (t) => {
  const adapters = [
    { readIssueState: async () => ({ state: { type: 'completed' } }) },
    { readIssueState: async () => ({ state: { type: 'canceled' } }) },
    { readIssueState: async () => Promise.reject(new Error('not found')) },
    {},
  ]
  for (const extra of adapters) {
    const calls = []
    const adapter = {
      ...extra,
      async applyIssueWrites(writes) {
        calls.push(writes)
        return { outcome: 'ok' }
      },
    }
    const data = JSON.stringify({ ...snapshot, pullRequests: [{ ...pr, ticketIds: ['ABC-7'] }] })
    assert.equal((await run(t, ['--apply'], { adapter }, { snapshot: data })).code, 3)
    assert.equal(calls[0].addBlockedBy, undefined)
    assert.deepEqual(calls[0].addLabels, ['plan'])
  }
})

test('a hold with no open ticket lands where plan selection picks it up after Step 12', async (t) => {
  const own = mergeConfig(DEFAULT_CONFIG, {
    adapters: { ...DEFAULT_CONFIG.adapters, tracker: 'demo' },
    trackerConfig: {
      demo: {
        labels: { agentBuild: 'build', agentPlan: 'plan' },
        states: { ...DEFAULT_TRACKER_STATES, unplanned: 'Ideas', inProgress: 'Doing' },
      },
    },
  })
  const calls = []
  const adapter = {
    async applyIssueWrites(writes) {
      calls.push(writes)
      return { outcome: 'ok' }
    },
  }
  assert.equal((await run(t, ['--apply'], { config: own, adapter })).code, 3)
  // The claimed ticket entered at Todo and sits In Progress while the hold writes land.
  const tracker = { state: calls[0].stateName ?? 'Doing', labels: new Set(['build']) }
  for (const call of calls) {
    call.addLabels?.forEach((label) => tracker.labels.add(label))
    call.removeLabels?.forEach((label) => tracker.labels.delete(label))
  }
  // Step 12 reads the state the hold left; a restore moves the ticket back to its entry state.
  const step12 = decideNoChangeDisposition(
    {
      claimVerdict: 'WON',
      currentState: tracker.state,
      head: null,
      tree: null,
      baseTree: null,
      worktreeClean: true,
    },
    own,
  )
  if (step12.disposition === 'restore') tracker.state = 'Todo'
  const plan = stageSelectionQuery(own, 'plan')
  assert.equal(tracker.state, plan.state)
  assert.ok(plan.requireLabels.every((label) => tracker.labels.has(label)))
  assert.ok(!tracker.labels.has('build'))
})

test('a replan hold that fails partway still leaves the ticket in a queue after Step 12', async (t) => {
  // Each label, comment and state change is its own sequential tracker write; any one can fail.
  for (let failAt = 0; failAt <= 4; failAt++) {
    const ticket = { state: DEFAULT_TRACKER_STATES.inProgress, labels: new Set(['build']) }
    let writes = 0
    const lands = () => writes++ !== failAt
    const adapter = {
      async applyIssueWrites(w) {
        for (const label of w.addLabels ?? []) {
          if (!lands()) return { outcome: 'failed' }
          ticket.labels.add(label)
        }
        for (const label of w.removeLabels ?? []) {
          if (!lands()) return { outcome: 'failed' }
          ticket.labels.delete(label)
        }
        if (w.comment && !lands()) return { outcome: 'failed' }
        if (w.stateName) {
          if (!lands()) return { outcome: 'failed' }
          ticket.state = w.stateName
        }
        return { outcome: 'ok' }
      },
    }
    const r = await run(t, ['--apply'], { adapter })
    assert.equal(r.code, failAt === 4 ? 3 : 4)
    const step12 = decideNoChangeDisposition(
      {
        claimVerdict: 'WON',
        currentState: ticket.state,
        head: null,
        tree: null,
        baseTree: null,
        worktreeClean: true,
      },
      config,
    )
    if (step12.disposition === 'restore') ticket.state = DEFAULT_TRACKER_STATES.planned
    const queued = ['build', 'plan']
      .map((stage) => stageSelectionQuery(config, stage))
      .some((q) => q.state === ticket.state && q.requireLabels.every((l) => ticket.labels.has(l)))
    assert.ok(queued, `failAt ${failAt}: ${ticket.state} ${[...ticket.labels]}`)
  }
})

test('mixed PRs write blockers and send the issue back to planning together', async (t) => {
  const calls = []
  const adapter = {
    readIssueState: async () => ({ state: { type: 'started' } }),
    async applyIssueWrites(writes) {
      calls.push(writes)
      return { outcome: 'ok' }
    },
  }
  const data = JSON.stringify({
    ...snapshot,
    pullRequests: [
      { ...pr, ticketIds: ['ABC-7'] },
      { ...pr, number: 50, url: 'https://example.test/pull/50' },
    ],
  })
  assert.equal((await run(t, ['--apply'], { adapter }, { snapshot: data })).code, 3)
  assert.deepEqual(calls[0].addBlockedBy, ['ABC-7'])
  assert.deepEqual(calls[0].addLabels, ['plan'])
  assert.match(calls[0].comment, /#50 has no open ticket/)
})

test('unreadable and invalid inputs fail open without adapter writes', async (t) => {
  const adapter = {
    applyIssueWrites() {
      assert.fail('must not write')
    },
  }
  for (const values of [{ snapshot: '{' }, { snapshot: '{}' }, { issue: '{' }]) {
    const r = await run(t, ['--apply'], { adapter }, values)
    assert.equal(r.code, 69)
    assert.equal(r.out.route, 'not-evaluated')
    assert.ok(r.out.reason)
  }
  assert.equal((await run(t, ['--open-prs', '/missing/snapshot'], { adapter })).code, 69)
  assert.equal((await run(t, ['--plan', '/missing/plan'], { adapter })).code, 69)
  const d = mkdtempSync(join(tmpdir(), 'open-pr-fetch-'))
  t.after(() => rmSync(d, { recursive: true, force: true, maxRetries: 3 }))
  writeFileSync(join(d, 'issue'), JSON.stringify(issue))
  writeFileSync(join(d, 'plan'), '')
  const r = await runCli(['check', '--issue-json', join(d, 'issue'), '--plan', join(d, 'plan')], {
    config,
    adapter,
    fetch() {
      throw new Error('offline')
    },
  })
  assert.equal(r.code, 69)
  assert.match(r.out.reason, /offline/)
})

test('strict arguments require plan and issue', async () => {
  for (const argv of [
    [],
    ['check'],
    ['check', '--plan', 'x'],
    ['check', '--issue-json', 'x'],
    ['check', '--wat'],
  ])
    assert.equal((await runCli(argv)).code, 64)
})

test('on the GitHub tracker a PR whose ticket is open becomes a blocker', async (t) => {
  const views = {
    7: { number: 7, state: 'OPEN', labels: [{ name: 'boss:in-progress' }] },
    8: { number: 8, state: 'CLOSED', stateReason: 'COMPLETED', labels: [] },
  }
  const github = createGithubAdapter({
    config: { trackerConfig: { github: { repository: 'acme/tasks' } } },
    spawn: (_cmd, args) => ({ status: 0, stdout: JSON.stringify(views[args[2]]) }),
  })
  const calls = []
  const adapter = {
    ...github,
    async applyIssueWrites(writes) {
      calls.push(writes)
      return { outcome: 'ok' }
    },
  }
  const data = JSON.stringify({
    ...snapshot,
    pullRequests: [
      { ...pr, ticketIds: ['GH-7'] },
      { ...pr, number: 43, url: 'https://example.test/pull/43', ticketIds: ['GH-8'] },
    ],
  })
  const subject = JSON.stringify({ ...issue, id: 'GH-1', identifier: 'GH-1' })
  assert.equal((await run(t, ['--apply'], { adapter }, { snapshot: data, issue: subject })).code, 3)
  assert.deepEqual(calls[0].addBlockedBy, ['GH-7'])
  assert.match(calls[0].comment, /#43 has no open ticket/)
})
