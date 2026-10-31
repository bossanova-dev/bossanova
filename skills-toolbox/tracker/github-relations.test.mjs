import assert from 'node:assert/strict'
import { test } from 'node:test'
import { readFileSync } from 'node:fs'
import { createGithubAdapter } from './github.mjs'
import { DEPENDENCY_READBACK_RETRY } from './github-relations.mjs'
import { normalizeTicket, classifyTickets, buildGraph, readyTickets } from '../epic/bs-epic-lib.mjs'
import { initFanoutLedger, parentFlipDecision } from '../plan/plan-child-fanout.mjs'
import { DEFAULT_CONFIG, requiredPlanSections, withTrackerDefaults } from '../skill-config.mjs'
import { planCommentBody } from './github-plan.mjs'
import { classifyTrackerOutcome } from './outcome.mjs'

const expectedDelays = (count) =>
  Array.from({ length: count }, (_, index) =>
    Math.min(
      DEPENDENCY_READBACK_RETRY.baseDelayMs * 2 ** index,
      DEPENDENCY_READBACK_RETRY.maxDelayMs,
    ),
  )

const config = { trackerConfig: { github: { repository: 'acme/tasks' } } }
const raw = (number, extra = {}) => ({
  number,
  id: number + 100,
  title: `Issue ${number}`,
  state: 'open',
  state_reason: null,
  labels: [{ name: 'boss:todo' }],
  html_url: `https://github.com/acme/tasks/issues/${number}`,
  repository_url: 'https://api.github.com/repos/acme/tasks',
  issue_dependencies_summary: { total_blocked_by: 0, total_blocking: 0 },
  sub_issues_summary: { total: 0 },
  ...extra,
})
function harness({
  rows = [raw(1)],
  blockers = {},
  blocking = {},
  children = {},
  parent = {},
  comments = {},
  fail,
  oneSided = false,
  lag = {},
} = {}) {
  const calls = [],
    posts = [],
    sleeps = [],
    delayed = []
  const spawn = (_cmd, args) => {
    calls.push(args)
    const path = args[1]
    const failure = fail?.(args)
    if (failure)
      return typeof failure === 'object' ? failure : { status: 1, stderr: 'HTTP 500: read failed' }
    let result
    if (args[0] === 'label')
      result = ['boss:todo', 'needs-human', 'agent-build'].map((name) => ({ name }))
    else if (args[0] === 'issue')
      result = rows.map((row) => ({ ...row, state: row.state.toUpperCase(), url: row.html_url }))
    else {
      const number = Number(/\/issues\/(\d+)/.exec(path)?.[1])
      const row = rows.find((row) => row.number === number)
      if (args.includes('--input')) {
        const body = JSON.parse(readFileSync(args[args.indexOf('--input') + 1], 'utf8'))
        posts.push({ path, body })
        const source = rows.find((row) => row.id === body.issue_id)
        blockers[number] = [...(blockers[number] ?? []), source]
        row.issue_dependencies_summary.total_blocked_by++
        if (!oneSided) {
          blocking[source.number] = [...(blocking[source.number] ?? []), row]
          source.issue_dependencies_summary.total_blocking++
        }
        delayed.push({
          target: number,
          source: source.number,
          forward: lag.forward ?? 0,
          mirror: lag.mirror ?? 0,
          forwardSummary: lag.summary ?? 0,
          mirrorSummary: lag.summary ?? 0,
        })
        result = source
      } else if (path.endsWith('/dependencies/blocked_by')) {
        result = (blockers[number] ?? [])
          .filter((source) => {
            const edge = delayed.find(
              (edge) => edge.target === number && edge.source === source.number,
            )
            if (!edge || edge.forward <= 0) return true
            edge.forward--
            return false
          })
          .map((row) => [row])
      } else if (path.endsWith('/dependencies/blocking')) {
        result = [
          (blocking[number] ?? []).filter((target) => {
            const edge = delayed.find(
              (edge) => edge.source === number && edge.target === target.number,
            )
            if (!edge || edge.mirror <= 0) return true
            edge.mirror--
            return false
          }),
        ]
      } else if (path.endsWith('/sub_issues')) result = (children[number] ?? []).map((row) => [row])
      else if (path.endsWith('/parent')) {
        if (!parent[number]) return { status: 1, stderr: 'HTTP 404: Not Found' }
        result = parent[number]
      } else if (path.endsWith('/comments')) result = [comments[number] ?? []]
      else {
        result = { ...row, issue_dependencies_summary: { ...row?.issue_dependencies_summary } }
        for (const edge of delayed) {
          if (edge.target === number && edge.forwardSummary > 0) {
            result.issue_dependencies_summary.total_blocked_by--
            edge.forwardSummary--
          }
          if (!oneSided && edge.source === number && edge.mirrorSummary > 0) {
            result.issue_dependencies_summary.total_blocking--
            edge.mirrorSummary--
          }
        }
      }
    }
    return { status: 0, stdout: JSON.stringify(result) }
  }
  return {
    adapter: createGithubAdapter({
      config,
      spawn,
      sleep: async (ms) => {
        sleeps.push(ms)
      },
    }),
    calls,
    posts,
    sleeps,
  }
}
test('dependencies require hydrated relations and clear only closed readable blockers', () => {
  const { adapter } = harness()
  assert.throws(() => adapter.readDependencies({}), /relations/)
  assert.deepEqual(adapter.readDependencies({ relations: { blockedBy: [] } }), [])
  for (const type of ['completed', 'canceled'])
    assert.equal(adapter.isUnblocked({ relations: { blockedBy: [{ state: { type } }] } }), true)
  for (const blocker of [{ state: { type: 'started' } }, {}])
    assert.equal(adapter.isUnblocked({ relations: { blockedBy: [blocker] } }), false)
})
test('hydration joins pages, checks both counts, maps closed reasons and parent', async () => {
  const rows = [
    raw(1, { issue_dependencies_summary: { total_blocked_by: 3, total_blocking: 0 } }),
    raw(2, { state: 'closed' }),
    raw(3, { state: 'closed', state_reason: 'not_planned' }),
    raw(4),
  ]
  const h = harness({ rows, blockers: { 1: rows.slice(1) }, parent: { 1: raw(9) } })
  const issue = await h.adapter.runOperation('get-issue', { id: 'GH-1' })
  assert.deepEqual(
    issue.relations.blockedBy.map((row) => row.state.type),
    ['completed', 'canceled', 'unstarted'],
  )
  assert.equal(issue.parentId, 'GH-9')
  assert.deepEqual(issue.blockedBy, ['GH-2', 'GH-3', 'GH-4'])
  assert.ok(h.calls.some((args) => args.includes('--paginate') && args.includes('--slurp')))
  rows[0].issue_dependencies_summary.total_blocked_by = 4
  await assert.rejects(h.adapter.runOperation('get-issue', { id: 1 }), /disagrees.*summary count/)
})
test('get-issue honours foreign blockers and foreign reads while writes reject them', async () => {
  const foreign = raw(2, {
    html_url: 'https://github.com/other/tasks/issues/2',
    repository_url: 'https://api.github.com/repos/other/tasks',
    state: 'closed',
    state_reason: 'duplicate',
  })
  const first = raw(1, {
    issue_dependencies_summary: { total_blocked_by: 1, total_blocking: 0 },
    parent_issue_url: 'https://api.github.com/repos/acme/tasks/issues/8',
  })
  const h = harness({ rows: [first, foreign], blockers: { 1: [foreign] } })
  const issue = await h.adapter.runOperation('get-issue', { id: 1 })
  assert.equal(issue.parentId, 'GH-8')
  assert.equal(issue.relations.blockedBy[0].identifier, foreign.html_url)
  assert.equal(issue.relations.blockedBy[0].repository, 'other/tasks')
  assert.equal(issue.relations.blockedBy[0].state.type, 'canceled')
  assert.equal(
    (await h.adapter.runOperation('get-issue', { id: foreign.html_url })).id,
    foreign.html_url,
  )
  await assert.rejects(
    h.adapter.runOperation('move-state', { id: foreign.html_url, state: 'boss:todo' }),
    /foreign|repository/,
  )
})
test('unreadable dependency state fails closed', async () => {
  const row = raw(1, { issue_dependencies_summary: { total_blocked_by: 1, total_blocking: 0 } })
  const h = harness({ rows: [row], blockers: { 1: [raw(2, { state: undefined })] } })
  await assert.rejects(h.adapter.runOperation('get-issue', { id: 1 }), /state/)
})
test('child listing hydrates pages, reconciles one-sided edges and rejects incomplete or foreign sets', async () => {
  const root = raw(1, { sub_issues_summary: { total: 2 } })
  const a = raw(2, { issue_dependencies_summary: { total_blocked_by: 0, total_blocking: 1 } }),
    b = raw(3)
  const h = harness({ rows: [root, a, b], children: { 1: [a, b] }, blocking: { 2: [b] } })
  const result = await h.adapter.runOperation('list-children', { parentId: 1 })
  assert.deepEqual(
    result.children[1].relations.blockedBy.map((row) => row.identifier),
    ['GH-2'],
  )
  assert.deepEqual(result.relationWarnings, ['GH-2 blocks GH-3 was read only from GH-2'])
  assert.equal(result.children[0].comments, undefined)
  root.sub_issues_summary.total = 3
  await assert.rejects(h.adapter.runOperation('list-children', { parentId: 1 }), /summary count/)
  root.sub_issues_summary.total = 1
  const foreign = raw(4, { html_url: 'https://github.com/other/tasks/issues/4' })
  const foreignHarness = harness({ rows: [root], children: { 1: [foreign] } })
  await assert.rejects(
    foreignHarness.adapter.runOperation('list-children', { parentId: 1 }),
    /other\/tasks/,
  )
})
test('consistent sibling edges need no warning', async () => {
  const a = raw(2, { issue_dependencies_summary: { total_blocked_by: 0, total_blocking: 1 } })
  const b = raw(3, { issue_dependencies_summary: { total_blocked_by: 1, total_blocking: 0 } })
  const h = harness({
    rows: [raw(1, { sub_issues_summary: { total: 2 } }), a, b],
    children: { 1: [a, b] },
    blockers: { 3: [a] },
    blocking: { 2: [b] },
  })
  assert.deepEqual(
    (await h.adapter.runOperation('list-children', { parentId: 1 })).relationWarnings,
    [],
  )
})
test('append validates before writing, posts REST ids via input and verifies both directions', async () => {
  const h = harness({ rows: [raw(1), raw(2)] })
  const result = await h.adapter.runOperation('append-dependency', { id: 1, blockedBy: ['GH-2'] })
  assert.deepEqual(result, { id: 'GH-1', blockedBy: ['GH-2'] })
  assert.deepEqual(h.posts[0].body, { issue_id: 102 })
  await h.adapter.runOperation('append-dependency', { id: 1, blockedBy: [2] })
  assert.equal(h.posts.length, 1)
  for (const blockedBy of [[1], ['https://github.com/other/tasks/issues/2'], [2, 99]]) {
    const h = harness({ rows: [raw(1), raw(2)] })
    await assert.rejects(h.adapter.runOperation('append-dependency', { id: 1, blockedBy }))
    assert.equal(h.posts.length, 0)
  }
})
test('lagging mirror and summary converge without reposting', async () => {
  const h = harness({ rows: [raw(1), raw(2)], lag: { mirror: 2, summary: 2 } })
  assert.deepEqual(await h.adapter.runOperation('append-dependency', { id: 1, blockedBy: [2] }), {
    id: 'GH-1',
    blockedBy: ['GH-2'],
  })
  assert.equal(h.posts.length, 1)
  assert.deepEqual(h.sleeps, expectedDelays(2))
})
test('all blockers are written before lagging readback', async () => {
  const h = harness({ rows: [raw(1), raw(2), raw(3)], lag: { forward: 1 } })
  assert.deepEqual(
    await h.adapter.runOperation('append-dependency', { id: 1, blockedBy: [2, 3] }),
    {
      id: 'GH-1',
      blockedBy: ['GH-2', 'GH-3'],
    },
  )
  assert.deepEqual(
    h.posts.map((post) => post.body.issue_id),
    [102, 103],
  )
  const firstPost = h.calls.findIndex((args) => args.includes('--input'))
  assert.ok(h.calls[firstPost + 1].includes('--input'))
})
test('missing forward edge exhausts bounded readback with an honest indeterminate error', async () => {
  const h = harness({ rows: [raw(1), raw(2)], lag: { forward: Infinity } })
  await assert.rejects(
    h.adapter.runOperation('append-dependency', { id: 1, blockedBy: [2] }),
    (error) => {
      assert.match(error.message, /may have applied.*forward blocked-by.*GH-2/)
      assert.doesNotMatch(error.message, /HTTP 504/)
      assert.equal(
        classifyTrackerOutcome(error.message, { operation: 'write' }).verdict,
        'indeterminate',
      )
      return true
    },
  )
  assert.equal(h.sleeps.length, DEPENDENCY_READBACK_RETRY.maxAttempts - 1)
  assert.deepEqual(h.sleeps, expectedDelays(DEPENDENCY_READBACK_RETRY.maxAttempts - 1))
  assert.equal(
    h.calls.filter(
      (args) => args[1].endsWith('/dependencies/blocked_by') && !args.includes('--input'),
    ).length,
    DEPENDENCY_READBACK_RETRY.maxAttempts + 1,
  )
})
test('persistent reverse lag succeeds with a blocking-side warning', async () => {
  const h = harness({ rows: [raw(1), raw(2)], oneSided: true })
  const result = await h.adapter.runOperation('append-dependency', { id: 1, blockedBy: [2] })
  assert.deepEqual(result.blockedBy, ['GH-2'])
  assert.ok(result.warnings.some((warning) => /GH-2.*blocking side/.test(warning)))
  assert.equal(h.sleeps.length, DEPENDENCY_READBACK_RETRY.maxAttempts - 1)
})
test('persistent summary lag succeeds with count warnings after the budget', async () => {
  const h = harness({ rows: [raw(1), raw(2)], lag: { summary: Infinity } })
  const result = await h.adapter.runOperation('append-dependency', { id: 1, blockedBy: [2] })
  assert.deepEqual(result.blockedBy, ['GH-2'])
  assert.deepEqual(result.warnings, [
    'blocked-by summary for GH-1 counts 0 but lists 1',
    'blocking summary for GH-2 counts 0 but lists 1',
  ])
  assert.deepEqual(h.sleeps, expectedDelays(DEPENDENCY_READBACK_RETRY.maxAttempts - 1))
})
test('persistent reverse read error stays indeterminate without a fabricated status', async () => {
  const h = harness({
    rows: [raw(1), raw(2)],
    fail: (args) => args[1].endsWith('/dependencies/blocking'),
  })
  await assert.rejects(
    h.adapter.runOperation('append-dependency', { id: 1, blockedBy: [2] }),
    (error) => {
      assert.match(error.message, /may have applied.*HTTP 500/)
      assert.doesNotMatch(error.message, /HTTP 504/)
      assert.equal(
        classifyTrackerOutcome(error.message, { operation: 'write' }).verdict,
        'indeterminate',
      )
      return true
    },
  )
  assert.equal(h.sleeps.length, DEPENDENCY_READBACK_RETRY.maxAttempts - 1)
})
test('pre-write summary disagreement exhausts the budget before any post', async () => {
  const h = harness({
    rows: [
      raw(1, { issue_dependencies_summary: { total_blocked_by: 1, total_blocking: 0 } }),
      raw(2),
    ],
  })
  await assert.rejects(
    h.adapter.runOperation('append-dependency', { id: 1, blockedBy: [2] }),
    /disagrees.*summary count/,
  )
  assert.equal(h.posts.length, 0)
  assert.equal(h.sleeps.length, DEPENDENCY_READBACK_RETRY.maxAttempts - 1)
})
test('unblocked gate stops at first free candidate, returns false only for complete reads', async () => {
  const a = raw(1, { issue_dependencies_summary: { total_blocked_by: 1, total_blocking: 0 } }),
    b = raw(2),
    c = raw(3)
  const h = harness({ rows: [a, b, c], blockers: { 1: [c] } })
  const query = { state: 'boss:todo', selection: {} }
  assert.equal(await h.adapter.hasUnblockedWork(query), true)
  assert.equal(
    h.calls.some((args) => args[1] === 'repos/acme/tasks/issues/3'),
    false,
  )
  const blocked = harness({ rows: [a], blockers: { 1: [c] } })
  assert.equal(await blocked.adapter.hasUnblockedWork(query), false)
  await assert.rejects(h.adapter.hasUnblockedWork({ state: 'boss:todo' }), /legacy/)
  const failed = harness({ rows: [a], fail: (args) => args[0] === 'api' })
  await assert.rejects(failed.adapter.hasUnblockedWork(query), /failed/)
})

test('hydrated three-child epic schedules in order and a closed child clears its dependent', async () => {
  const a = raw(2, {
    state: 'closed',
    labels: [{ name: 'agent-build' }],
    issue_dependencies_summary: { total_blocked_by: 0, total_blocking: 1 },
  })
  const b = raw(3, {
    labels: [{ name: 'boss:todo' }, { name: 'agent-build' }],
    issue_dependencies_summary: { total_blocked_by: 1, total_blocking: 1 },
  })
  const c = raw(4, {
    labels: [{ name: 'boss:todo' }, { name: 'agent-build' }],
    issue_dependencies_summary: { total_blocked_by: 1, total_blocking: 0 },
  })
  const h = harness({
    rows: [raw(1, { sub_issues_summary: { total: 3 } }), a, b, c],
    children: { 1: [a, b, c] },
    blockers: { 3: [a], 4: [b] },
    blocking: { 2: [b], 3: [c] },
  })
  const { children } = await h.adapter.runOperation('list-children', { parentId: 1 })
  const classified = classifyTickets(children.map(normalizeTicket), 'boss:todo')
  assert.deepEqual(
    classified.done.map((row) => row.id),
    ['GH-2'],
  )
  const graph = buildGraph([...classified.eligible, ...classified.done])
  const state = {
    merged: new Set(classified.done.map((row) => row.id)),
    failed: new Set(),
    inFlight: new Set(),
    externallyCleared: new Set(),
  }
  assert.deepEqual(
    readyTickets(graph, state).map((row) => row.id),
    ['GH-3'],
  )
  state.merged.add('GH-3')
  assert.deepEqual(
    readyTickets(graph, state).map((row) => row.id),
    ['GH-4'],
  )
})

test('child plans survive enumeration so a completed fan-out can resume its parent write', async () => {
  const planningConfig = withTrackerDefaults({
    ...DEFAULT_CONFIG,
    adapters: { tracker: 'github' },
    ...config,
  })
  const description = requiredPlanSections(DEFAULT_CONFIG)
    .map((heading) => `${heading}\n\nPlanned work.`)
    .join('\n\n')
  const child = raw(2, { body: description })
  const h = harness({
    rows: [
      raw(1, {
        labels: [{ name: 'boss:unplanned' }, { name: 'agent-plan' }],
        sub_issues_summary: { total: 1 },
      }),
      child,
    ],
    children: { 1: [child] },
    comments: {
      2: [
        {
          id: 27,
          body: planCommentBody('Implementation details'),
          author_association: 'MEMBER',
          html_url: 'https://github.com/acme/tasks/issues/2#issuecomment-27',
        },
      ],
    },
  })
  const result = await h.adapter.runOperation('list-children', { parentId: 1 })
  const fetched = await h.adapter.runOperation('get-issue', { id: 2 })
  assert.equal(result.children[0].planAttachment?.id, '27')
  assert.deepEqual(result.children[0].attachments, fetched.attachments)
  assert.equal(result.children[0].comments, undefined)
  const ledger = initFanoutLedger({ ...result, config: planningConfig })
  assert.deepEqual(ledger.order, [])
  assert.deepEqual(
    ledger.skipped.map(({ id, reason }) => ({ id, reason })),
    [{ id: 'GH-2', reason: 'already-planned' }],
  )
  assert.equal(
    parentFlipDecision({ ledger, parent: result.parent, config: planningConfig }).flip,
    true,
  )
})

test('dependency batches preserve effects when a later POST is rejected', async () => {
  for (const status of [422, 429]) {
    let posts = 0
    const h = harness({
      rows: [raw(1), raw(2), raw(3)],
      fail: (args) =>
        args.includes('--input') && ++posts === 2
          ? { status: 1, stderr: `HTTP ${status}: rejected` }
          : false,
    })
    await assert.rejects(
      h.adapter.runOperation('append-dependency', { id: 1, blockedBy: [2, 3] }),
      (error) => {
        assert.equal(
          classifyTrackerOutcome(error.message, { operation: 'write' }).verdict,
          'indeterminate',
        )
        assert.ok(error.message.includes('GH-2'))
        assert.doesNotMatch(error.message, /HTTP 504/)
        return true
      },
    )
    assert.deepEqual(
      h.posts.map((post) => post.body.issue_id),
      [102],
    )
  }
})

test('post-write dependency reads retain effects despite definitive read rejection', async () => {
  for (const status of [404, 403]) {
    const h = harness({
      rows: [raw(1), raw(2)],
      fail: (args) =>
        args[1].endsWith('/dependencies/blocking')
          ? { status: 1, stderr: `HTTP ${status}: rejected` }
          : false,
    })
    await assert.rejects(
      h.adapter.runOperation('append-dependency', { id: 1, blockedBy: [2] }),
      (error) => {
        assert.equal(
          classifyTrackerOutcome(error.message, { operation: 'write' }).verdict,
          'indeterminate',
        )
        assert.doesNotMatch(error.message, /HTTP 504/)
        return true
      },
    )
    assert.equal(h.posts.length, 1)
  }
})

test('a first dependency POST distinguishes definitive rejection from possible effect', async () => {
  for (const [failure, verdict] of [
    [{ status: 1, stderr: 'HTTP 422: rejected' }, 'permanent'],
    [{ error: { code: 'ETIMEDOUT' } }, 'indeterminate'],
  ]) {
    const h = harness({
      rows: [raw(1), raw(2)],
      fail: (args) => (args.includes('--input') ? failure : false),
    })
    await assert.rejects(
      h.adapter.runOperation('append-dependency', { id: 1, blockedBy: [2] }),
      (error) => {
        assert.equal(classifyTrackerOutcome(error.message, { operation: 'write' }).verdict, verdict)
        if (verdict === 'permanent') assert.doesNotMatch(error.message, /may have applied/)
        assert.doesNotMatch(error.message, /HTTP 504/)
        return true
      },
    )
    assert.equal(h.posts.length, 0)
  }
})

test('readBlockers capability retains GitHub state-only behavior', async () => {
  const { adapter } = harness({
    rows: [
      raw(1, { issue_dependencies_summary: { total_blocked_by: 1, total_blocking: 0 } }),
      raw(2, { state: 'closed', state_reason: 'completed' }),
    ],
    blockers: { 1: [raw(2, { state: 'closed', state_reason: 'completed' })] },
  })
  const rows = await adapter.readBlockers(['GH-1'])
  assert.equal(rows[0].id, 'GH-1')
  assert.equal(rows[0].blockers[0].state.type, 'completed')
})

test('GitHub selection hydrates candidate description and blocker PR links while strict scheduling stays blocked', async () => {
  const description = '## Key changes\n`src/dependent.mjs`'
  const blocker = raw(2, {
    labels: [{ name: 'boss:in-review' }],
    body: 'Work: https://github.com/acme/code/pull/9',
  })
  const { adapter } = harness({
    rows: [
      raw(1, {
        body: description,
        issue_dependencies_summary: { total_blocked_by: 1, total_blocking: 0 },
      }),
      blocker,
    ],
    blockers: { 1: [blocker] },
  })
  const [candidate] = await adapter.readBlockers(['GH-1'], { mode: 'selection' })
  assert.equal(candidate.description, description)
  assert.deepEqual(candidate.blockers[0].attachments.nodes, [
    { url: 'https://github.com/acme/code/pull/9' },
  ])
  assert.equal(adapter.isUnblocked({ relations: { blockedBy: candidate.blockers } }), false)
  assert.equal((await adapter.readBlockers(['GH-1']))[0].description, undefined)
})

test('GitHub gate treats green review blockers as possible selection work without file evidence', async () => {
  const blocker = raw(2, {
    labels: [{ name: 'boss:in-review' }],
    body: 'https://github.com/acme/code/pull/9',
  })
  for (const isDraft of [false, true]) {
    const { adapter } = harness({
      rows: [
        raw(1, {
          labels: [{ name: 'boss:todo' }, { name: 'agent-build' }],
          issue_dependencies_summary: { total_blocked_by: 1, total_blocking: 0 },
        }),
        blocker,
      ],
      blockers: { 1: [blocker] },
    })
    const result = await adapter.hasUnblockedWork(
      { state: 'boss:todo', requireLabels: ['agent-build'], selection: {} },
      {
        inReviewState: 'boss:in-review',
        prLookup: async () => ({ state: 'OPEN', isDraft, green: true }),
      },
    )
    assert.equal(result, !isDraft)
  }
})
