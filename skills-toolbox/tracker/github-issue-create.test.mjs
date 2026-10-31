import assert from 'node:assert/strict'
import { test } from 'node:test'
import { readFileSync } from 'node:fs'
import { classifyTrackerOutcome } from './outcome.mjs'
import { createGithubAdapter } from './github.mjs'

function fixture({
  missing = false,
  linkFail = false,
  createFail,
  absent = false,
  missingRanking,
  expectedLabels = ['agent-plan'],
} = {}) {
  const calls = []
  const raw = { id: 9001, number: 7, title: 'Child', body: 'body', state: 'OPEN', labels: [] }
  const adapter = createGithubAdapter({
    config: { trackerConfig: { github: { repository: 'acme/tasks' } } },
    spawn: (_cmd, args) => {
      calls.push(args)
      let out = ''
      if (args[0] === 'label')
        out = (
          missing
            ? []
            : [
                'agent-plan',
                'boss:unplanned',
                'boss:todo',
                ...[1, 2, 3, 4].map((n) => `boss:priority-${n}`),
                ...[0, 1, 2, 3, 5, 8].map((n) => `boss:estimate-${n}`),
              ]
        )
          .filter((name) => name !== missingRanking)
          .map((name) => ({ name }))
      else if (args[0] === 'issue') {
        if (args[1] === 'view') out = raw
        else if (args[1] === 'edit') raw.labels.push(args[args.indexOf('--add-label') + 1])
      } else if (args[1].endsWith('/sub_issues')) {
        if (args.includes('--input')) {
          assert.deepEqual(JSON.parse(readFileSync(args[args.indexOf('--input') + 1], 'utf8')), {
            sub_issue_id: 9001,
          })
          if (linkFail) return { status: 1, stderr: 'HTTP 422: rejected' }
          out = raw
        } else out = absent ? [[]] : [[], [{ ...raw }]]
      } else if (args[1] === 'repos/acme/tasks/issues') {
        if (createFail) return { status: 1, stderr: 'HTTP 422: rejected' }
        const body = JSON.parse(readFileSync(args[args.indexOf('--input') + 1], 'utf8'))
        assert.deepEqual(body.labels, expectedLabels)
        assert.equal(body.body, 'body')
        raw.labels = [...body.labels]
        out = { ...raw, html_url: 'https://github.com/acme/tasks/issues/7' }
      } else throw new Error(`unexpected ${args}`)
      return { status: 0, stdout: typeof out === 'string' ? out : JSON.stringify(out) }
    },
  })
  return { adapter, calls, raw }
}
const args = {
  title: 'Child',
  description: 'body',
  labels: ['agent-plan'],
  state: 'boss:unplanned',
  parentId: 'GH-1',
}
test('create links by REST id, verifies paginated membership, then exposes state', async () => {
  const h = fixture()
  assert.equal(h.adapter.operationMap.createIssue.command.verb, 'create-issue')
  const result = await h.adapter.runOperation('create-issue', args)
  assert.equal(result.id, 'GH-7')
  assert.equal(result.parentId, 'GH-1')
  const create = h.calls.findIndex((a) => a[1] === 'repos/acme/tasks/issues')
  const link = h.calls.findIndex((a) => a[1]?.endsWith('/sub_issues') && a.includes('--input'))
  const verify = h.calls.findIndex((a) => a[1]?.endsWith('/sub_issues') && a.includes('--paginate'))
  const expose = h.calls.findIndex((a) => a[0] === 'issue' && a[1] === 'edit')
  assert.ok(create < link && link < verify && verify < expose)
})
test('invalid create inputs and missing labels perform no write', async () => {
  for (const input of [
    { ...args, project: 'x' },
    { ...args, description: 'x'.repeat(65537) },
    { ...args, labels: ['boss:todo'] },
  ]) {
    const h = fixture()
    await assert.rejects(h.adapter.runOperation('create-issue', input), /project|65536|workflow/)
    assert.equal(
      h.calls.some((a) => a.includes('--input')),
      false,
    )
  }
  const h = fixture({ missing: true })
  await assert.rejects(h.adapter.runOperation('create-issue', args), /missing label/)
  assert.equal(
    h.calls.some((a) => a.includes('--input')),
    false,
  )
})
test('failed or unverified link leaves inert orphan and names it', async () => {
  for (const options of [{ linkFail: true }, { absent: true }]) {
    const h = fixture(options)
    await assert.rejects(h.adapter.runOperation('create-issue', args), /GH-7.*may have applied/)
    assert.deepEqual(h.raw.labels, ['agent-plan'])
    assert.equal(h.adapter.normalizeTicket(h.raw).stateName, null)
    assert.equal(
      h.calls.some((a) => a[1] === 'edit'),
      false,
    )
  }
})
test('standalone create applies state without sub-issue calls', async () => {
  const h = fixture()
  const result = await h.adapter.runOperation('create-issue', { ...args, parentId: undefined })
  assert.equal(result.parentId, null)
  assert.equal(
    h.calls.some((a) => a[1]?.endsWith('/sub_issues')),
    false,
  )
  assert.ok(h.raw.labels.includes('boss:unplanned'))
})

test('create applies ranking on the POST and preserves it when exposing state', async () => {
  for (const priority of [1, 2, 3, 4]) {
    const h = fixture({
      expectedLabels: ['agent-plan', `boss:priority-${priority}`, 'boss:estimate-5'],
    })
    await h.adapter.runOperation('create-issue', { ...args, priority, estimate: 5 })
    const ticket = h.adapter.normalizeTicket(h.raw)
    assert.equal(ticket.priority, priority)
    assert.equal(ticket.estimate, 5)
    assert.equal(ticket.stateName, 'boss:unplanned')
  }
})
test('create clears supplied ranking case-insensitively and deduplicates labels', async () => {
  const h = fixture({ expectedLabels: ['agent-plan', 'boss:priority-2', 'boss:estimate-5'] })
  await h.adapter.runOperation('create-issue', {
    ...args,
    labels: ['agent-plan', 'AGENT-PLAN', 'BOSS:PRIORITY-1', 'BOSS:ESTIMATE-8'],
    priority: 2,
    estimate: 5,
  })
  assert.equal(h.adapter.normalizeTicket(h.raw).priority, 2)
  assert.equal(h.adapter.normalizeTicket(h.raw).estimate, 5)
})
test('create priority zero removes supplied priority and adds the estimate', async () => {
  const h = fixture({ expectedLabels: ['agent-plan', 'boss:estimate-3'] })
  await h.adapter.runOperation('create-issue', {
    ...args,
    labels: ['agent-plan', 'boss:priority-4'],
    priority: 0,
    estimate: 3,
  })
  assert.equal(
    h.raw.labels.some((name) => /^boss:priority-/i.test(name)),
    false,
  )
  assert.equal(h.adapter.normalizeTicket(h.raw).estimate, 3)
})
test('missing ranking labels and invalid values reject before any write', async () => {
  for (const [options, ranking, error] of [
    [{ missingRanking: 'boss:estimate-5' }, { priority: 2, estimate: 5 }, /missing label/],
    [{}, { priority: 7 }, /priority must be/],
    [{}, { estimate: -1 }, /estimate must be/],
  ]) {
    const h = fixture(options)
    await assert.rejects(h.adapter.runOperation('create-issue', { ...args, ...ranking }), error)
    assert.equal(
      h.calls.some((a) => a.includes('--input') || a[1] === 'edit'),
      false,
    )
  }
})

test('create failures distinguish an orphan from a rejected first write', async () => {
  for (const [options, verdict] of [
    [{ linkFail: true }, 'indeterminate'],
    [{ createFail: true }, 'permanent'],
  ]) {
    const h = fixture(options)
    await assert.rejects(h.adapter.runOperation('create-issue', args), (error) => {
      assert.equal(classifyTrackerOutcome(error.message, { operation: 'write' }).verdict, verdict)
      assert.doesNotMatch(error.message, /HTTP 504/)
      if (options.linkFail) assert.ok(error.message.includes('GH-7'))
      else assert.doesNotMatch(error.message, /may have applied/)
      return true
    })
  }
})

const createInput = {
  title: 'Follow-up',
  description: 'Finding details',
  stateName: 'boss:unplanned',
  labels: ['agent-plan'],
}
const marker = '<!-- follow-up:key -->'
function createHarness({ snapshots = [[]], createError, stateError, missingLabel = false } = {}) {
  const calls = [],
    posts = []
  let reads = 0
  const marked = (number, extra = {}) => ({
    id: number + 9000,
    number,
    title: 'Existing',
    body: marker,
    state: 'open',
    created_at: '2026-01-01',
    closed_at: null,
    author_association: 'MEMBER',
    html_url: `https://github.com/acme/tasks/issues/${number}`,
    ...extra,
  })
  const raw = { number: 7, title: 'Follow-up', body: '', state: 'OPEN', labels: [] }
  const adapter = createGithubAdapter({
    config: { trackerConfig: { github: { repository: 'acme/tasks' } } },
    spawn: (_cmd, argv) => {
      calls.push(argv)
      if (argv[0] === 'label')
        return {
          status: 0,
          stdout: JSON.stringify(
            (missingLabel ? ['boss:unplanned'] : ['boss:unplanned', 'agent-plan']).map((name) => ({
              name,
            })),
          ),
        }
      if (argv[0] === 'api') {
        if (!argv.includes('--input')) {
          const snapshot = snapshots[Math.min(reads++, snapshots.length - 1)]
          if (snapshot === 'error') return { status: 1, stderr: 'network unavailable' }
          return {
            status: 0,
            stdout: JSON.stringify(
              snapshot.map((row) => marked(row.number ?? row, row.extra ?? {})),
            ),
          }
        }
        const body = JSON.parse(readFileSync(argv[argv.indexOf('--input') + 1], 'utf8'))
        posts.push(body)
        if (createError) return { status: 1, stderr: createError }
        raw.labels = body.labels
        return {
          status: 0,
          stdout: JSON.stringify({
            id: 9007,
            ...raw,
            html_url: 'https://github.com/acme/tasks/issues/7',
          }),
        }
      }
      if (argv[1] === 'view') return { status: 0, stdout: JSON.stringify(raw) }
      if (argv[1] === 'edit') {
        if (stateError) return { status: 1, stderr: 'state unavailable' }
        raw.labels.push(argv[argv.indexOf('--add-label') + 1])
        return { status: 0, stdout: '' }
      }
      throw new Error(`unexpected ${argv}`)
    },
  })
  return { adapter, calls, posts }
}

test('applyIssueCreate delegates creation and workflow transition', async () => {
  const h = createHarness()
  assert.deepEqual(await h.adapter.applyIssueCreate(createInput), {
    ok: true,
    outcome: 'ok',
    issue: { id: 'GH-7', identifier: 'GH-7', url: 'https://github.com/acme/tasks/issues/7' },
  })
  assert.equal(h.posts.length, 1)
  assert.ok(h.calls.some((call) => call[1] === 'edit'))
})
test('applyIssueCreate validates every input before requesting', async () => {
  for (const input of [
    null,
    [],
    { ...createInput, extra: true },
    { ...createInput, title: '' },
    { ...createInput, title: 'a'.repeat(257) },
    { ...createInput, description: null },
    { ...createInput, description: 'a'.repeat(65537) },
    { ...createInput, stateName: '' },
    { ...createInput, labels: [null] },
    { ...createInput, labels: 'agent-plan' },
    { ...createInput, marker: '' },
    { ...createInput, marker: 'a\nb' },
    { ...createInput, marker: 'a\rb' },
    { ...createInput, description: 'x'.repeat(65530), marker },
  ]) {
    const h = createHarness()
    await assert.rejects(h.adapter.applyIssueCreate(input))
    assert.equal(h.calls.length, 0)
  }
})
test('applyIssueCreate fails unknown states and labels permanently before writes', async () => {
  for (const [input, options] of [
    [{ ...createInput, stateName: 'missing' }, {}],
    [createInput, { missingLabel: true }],
    [{ ...createInput, labels: ['boss:unplanned'] }, {}],
  ]) {
    const h = createHarness(options)
    const result = await h.adapter.applyIssueCreate(input)
    assert.equal(result.outcome, 'failed')
    assert.equal(result.permanent, true)
    assert.equal(h.posts.length, 0)
    assert.ok(!h.calls.some((call) => call[1] === 'edit'))
  }
})
test('applyIssueCreate reuses only trusted exact marker matches, refusing ambiguous or failed reads', async () => {
  for (const [snapshots, outcome, existing] of [
    [[[1]], 'ok', true],
    [[[1, 2]], 'failed', undefined],
    [['error'], 'failed', undefined],
  ]) {
    const h = createHarness({ snapshots })
    const result = await h.adapter.applyIssueCreate({ ...createInput, marker })
    assert.equal(result.outcome, outcome)
    assert.equal(result.existing, existing)
    if (existing) assert.equal(result.issue.url, 'https://github.com/acme/tasks/issues/1')
    if (Array.isArray(snapshots[0]) && snapshots[0].length === 2)
      assert.equal(result.reason, 'marker-ambiguous')
    assert.equal(h.posts.length, 0)
  }
  const h = createHarness({
    snapshots: [
      [
        { number: 1, extra: { author_association: 'NONE' } },
        { number: 2, extra: { body: 'ordinary' } },
      ],
    ],
  })
  assert.equal((await h.adapter.applyIssueCreate({ ...createInput, marker })).outcome, 'ok')
  assert.equal(h.posts[0].body, `${createInput.description}\n\n${marker}`)
})
test('applyIssueCreate preserves an already present marker', async () => {
  const h = createHarness()
  const description = `${marker}\nFinding details`
  assert.equal(
    (await h.adapter.applyIssueCreate({ ...createInput, description, marker })).outcome,
    'ok',
  )
  assert.equal(h.posts[0].body, description)
})
test('applyIssueCreate settles indeterminate creates by lookup without resending', async () => {
  for (const options of [{ createError: 'network unavailable' }, { stateError: true }]) {
    for (const [snapshot, outcome] of [
      [[7], 'ok'],
      [[], 'indeterminate'],
      ['error', 'indeterminate'],
      [[7, 8], 'failed'],
    ]) {
      const h = createHarness({ ...options, snapshots: [[], snapshot] })
      assert.equal((await h.adapter.applyIssueCreate({ ...createInput, marker })).outcome, outcome)
      assert.equal(h.posts.length, 1)
    }
  }
  const h = createHarness({ stateError: true })
  assert.equal((await h.adapter.applyIssueCreate(createInput)).outcome, 'indeterminate')
  assert.equal(h.posts.length, 1)
  const rejected = createHarness({ createError: 'HTTP 422: rejected' })
  assert.equal((await rejected.adapter.applyIssueCreate(createInput)).outcome, 'failed')
  assert.equal(rejected.posts.length, 1)
})
