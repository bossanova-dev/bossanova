import assert from 'node:assert/strict'
import test from 'node:test'
import { classifyChecks } from '../pr-check-state.mjs'
import {
  rollupFromRest,
  runsFromRest,
  runsFromCheckSuites,
  viewFromRestPull,
  restCheckSnapshot,
  graphqlRunsArgs,
} from './rest-check-state.mjs'

const head = 'a'.repeat(40)
const run = {
  name: 'CI',
  workflow_id: 9,
  check_suite_id: 12,
  head_sha: head,
  status: 'completed',
  conclusion: 'success',
  event: 'push',
}
const check = { name: 'build', check_suite: { id: 12 }, status: 'completed', conclusion: 'success' }
const pull = {
  number: 7,
  state: 'open',
  draft: false,
  head: { sha: head, ref: 'feature' },
  base: { ref: 'main' },
  mergeable: null,
  mergeable_state: 'unknown',
}

test('REST rollups classify like equivalent GraphQL checks, including unknown conclusions', () => {
  for (const [status, conclusion] of [
    ['completed', 'success'],
    ['completed', 'neutral'],
    ['in_progress', null],
    ['completed', null],
  ]) {
    const input = {
      checkRuns: [
        { ...check, status, conclusion },
        { ...check, name: 'third party', check_suite: { id: 99 } },
      ],
      statuses: [
        { context: 'deploy', state: 'success', description: 'deployed', created_at: '2026-01-01' },
      ],
      workflowRuns: [run],
    }
    const rest = rollupFromRest(input)
    const graph = [
      {
        __typename: 'CheckRun',
        name: 'build',
        status: status.toUpperCase(),
        conclusion: conclusion?.toUpperCase() ?? '',
        workflowName: 'CI',
        workflowId: 9,
      },
      { __typename: 'CheckRun', name: 'third party', status: 'COMPLETED', conclusion: 'SUCCESS' },
      {
        __typename: 'StatusContext',
        context: 'deploy',
        state: 'SUCCESS',
        description: 'deployed',
        startedAt: '2026-01-01',
      },
    ]
    assert.deepEqual(classifyChecks({ rollup: rest }), classifyChecks({ rollup: graph }))
    assert.equal(rest[0].workflowName, 'CI')
    assert.equal(rest[0].workflowId, 9)
    assert.equal('workflowName' in rest[1], false)
  }
  assert.equal(
    rollupFromRest({
      checkRuns: [{ ...check, status: 'in_progress', conclusion: 'success' }],
      statuses: [],
      workflowRuns: [],
    })[0].conclusion,
    '',
  )
})

test('REST pull and run mappings preserve unknown review/mergeability and workflow identity', () => {
  const view = viewFromRestPull(pull)
  assert.equal(view.headRefOid, head)
  assert.equal(view.mergeable, 'UNKNOWN')
  assert.equal(view.reviewDecision, null)
  for (const [value, expected] of [
    [true, 'MERGEABLE'],
    [false, 'CONFLICTING'],
  ])
    assert.equal(viewFromRestPull({ ...pull, mergeable: value }).mergeable, expected)
  assert.equal(viewFromRestPull({ ...pull, merged_at: '2026-01-01' }).state, 'MERGED')
  assert.equal(viewFromRestPull({ ...pull, state: 'closed' }).state, 'CLOSED')
  const rows = runsFromRest([{ total_count: 1, workflow_runs: [run] }])
  assert.deepEqual(rows, [
    {
      name: 'CI',
      workflowName: 'CI',
      status: 'completed',
      conclusion: 'success',
      headSha: head,
      event: 'push',
      workflowDatabaseId: 9,
    },
  ])
  assert.deepEqual(
    runsFromCheckSuites(
      {
        data: {
          repository: {
            object: {
              checkSuites: {
                totalCount: 2,
                nodes: [
                  {
                    status: 'COMPLETED',
                    conclusion: 'SUCCESS',
                    workflowRun: { event: 'push', workflow: { name: 'CI', databaseId: 9 } },
                  },
                  { workflowRun: null },
                ],
              },
            },
          },
        },
      },
      head,
    ),
    [{ ...rows[0], status: 'COMPLETED', conclusion: 'SUCCESS' }],
  )
  assert.ok(graphqlRunsArgs({ owner: 'acme', name: 'widgets', headSha: head }).includes('graphql'))
})

function snapshot(overrides = {}) {
  const calls = []
  const gh = (args) => {
    calls.push(args)
    const endpoint = args[1]
    let json = endpoint.includes('/pulls/')
      ? pull
      : endpoint.includes('/check-runs?')
        ? [{ total_count: 1, check_runs: [check] }]
        : endpoint.includes('/actions/runs?')
          ? [{ total_count: 1, workflow_runs: [run] }]
          : [{ total_count: 0, statuses: [] }]
    for (const [key, value] of Object.entries(overrides))
      if (endpoint.includes(key)) json = typeof value === 'function' ? value(args) : value
    return { ok: true, json }
  }
  return { result: restCheckSnapshot({ pr: 7, repo: 'ghe.example.test/acme/widgets', gh }), calls }
}

test('REST snapshot paginates checks and runs, pins the host, and fails closed on truncation', () => {
  const { result, calls } = snapshot()
  assert.equal(result.ok, true)
  assert.equal(result.value.view.statusCheckRollup[0].workflowName, 'CI')
  assert.equal(result.value.runs.length, 1)
  assert.equal(calls.length, 4)
  assert.ok(calls.every((args) => args.includes('ghe.example.test')))
  for (const key of ['/check-runs?', '/actions/runs?', '/status?']) {
    const value = key.includes('check')
      ? { total_count: 2, check_runs: [check] }
      : key.includes('actions')
        ? { total_count: 2, workflow_runs: [run] }
        : { total_count: 2, statuses: [{ context: 'external', state: 'success' }] }
    const broken = snapshot({ [key]: [value] }).result
    assert.equal(broken.ok, false)
    assert.equal(broken.error.code, 'UNREADABLE')
    assert.match(broken.error.message, /truncated/)
  }
  assert.throws(
    () =>
      runsFromCheckSuites(
        { data: { repository: { object: { checkSuites: { totalCount: 101, nodes: [] } } } } },
        head,
      ),
    /truncated/,
  )
})

test('REST snapshot includes every paginated status context beyond the default page', () => {
  const statuses = Array.from({ length: 131 }, (_, index) => ({
    context: `external-${index}`,
    state: index === 130 ? 'failure' : 'success',
  }))
  const { result } = snapshot({
    '/status': (args) =>
      args.includes('--paginate') && args.includes('--slurp')
        ? [
            // gate-region-ok: split API fixture rows into pages, not source text.
            { total_count: statuses.length, statuses: statuses.slice(0, 100) },
            { total_count: statuses.length, statuses: statuses.slice(100) },
          ]
        : { total_count: statuses.length, statuses: statuses.slice(0, 30) },
  })
  assert.equal(result.ok, true)
  const rollup = result.value.view.statusCheckRollup
  assert.equal(rollup.filter((node) => node.__typename === 'StatusContext').length, 131)
  assert.equal(classifyChecks({ rollup }).state, 'failing')
})

test('REST snapshot propagates refused and unreadable subreads without partial state', () => {
  for (const refused of [true, false]) {
    const result = restCheckSnapshot({
      pr: 7,
      repo: 'acme/widgets',
      gh: () => ({ ok: false, refused, error: 'refused' }),
    })
    assert.equal(result.ok, false)
    assert.equal(result.error.code, refused ? 'RATE_LIMITED' : 'UNREADABLE')
    assert.equal('value' in result, false)
  }
})
