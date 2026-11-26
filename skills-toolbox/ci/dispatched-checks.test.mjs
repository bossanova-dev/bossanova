import assert from 'node:assert/strict'
import test from 'node:test'
import {
  DISPATCH_EVENT,
  checkRunsFromSuites,
  dispatchedCheckRuns,
  graphqlCheckSuitesArgs,
} from './dispatched-checks.mjs'

const HEAD = 'a'.repeat(40)
const OTHER = 'b'.repeat(40)

const suite = (event, runs, oid = HEAD) => ({
  commit: { oid },
  workflowRun: { event, workflow: { name: 'bazel (test + lint)', databaseId: 311920081 } },
  checkRuns: { totalCount: runs.length, nodes: runs },
})
const goTest = (conclusion) => ({
  name: 'go-test',
  status: 'COMPLETED',
  conclusion,
  startedAt: '2026-11-25T23:18:56Z',
  completedAt: '2026-11-25T23:49:02Z',
  detailsUrl: 'https://github.com/o/r/actions/runs/45720345201/job/139576816705',
})
const read = (nodes, totalCount = nodes.length) => ({
  data: { repository: { object: { checkSuites: { totalCount, nodes } } } },
})

test('checkRunsFromSuites: every workflow suite run carries its event and commit', () => {
  const rows = checkRunsFromSuites(
    read([
      suite('pull_request', [goTest('SKIPPED')]),
      suite(DISPATCH_EVENT, [goTest('SUCCESS')]),
      { commit: { oid: HEAD }, workflowRun: null, checkRuns: { totalCount: 0, nodes: [] } },
    ]),
  )
  assert.deepEqual(
    rows.map((row) => [row.name, row.conclusion, row.event, row.headSha, row.workflowId]),
    [
      ['go-test', 'SKIPPED', 'pull_request', HEAD, 311920081],
      ['go-test', 'SUCCESS', DISPATCH_EVENT, HEAD, 311920081],
    ],
  )
  assert.equal(rows[1].workflowName, 'bazel (test + lint)')
})

test('checkRunsFromSuites: a malformed or truncated read throws rather than hide a run', () => {
  assert.throws(() => checkRunsFromSuites({ errors: [{ message: 'x' }] }), /malformed/)
  assert.throws(() => checkRunsFromSuites(read([], 1)), /truncated check suites/)
  const cut = suite(DISPATCH_EVENT, [goTest('FAILURE')])
  cut.checkRuns.totalCount = 2
  assert.throws(() => checkRunsFromSuites(read([cut])), /truncated check runs/)
})

test('dispatchedCheckRuns: only workflow_dispatch rows on exactly the head', () => {
  const rows = checkRunsFromSuites(
    read([
      suite('pull_request', [goTest('SKIPPED')]),
      suite(DISPATCH_EVENT, [goTest('SUCCESS')]),
      suite(DISPATCH_EVENT, [goTest('FAILURE')], OTHER),
    ]),
  )
  assert.deepEqual(
    dispatchedCheckRuns(rows, HEAD).map((row) => row.conclusion),
    ['SUCCESS'],
  )
  assert.deepEqual(dispatchedCheckRuns(rows, ''), [])
  assert.deepEqual(dispatchedCheckRuns(null, HEAD), [])
})

test('graphqlCheckSuitesArgs: one GraphQL read keyed to the head commit', () => {
  const args = graphqlCheckSuitesArgs({ owner: 'o', name: 'r', headSha: HEAD })
  assert.deepEqual(args.slice(0, 2), ['api', 'graphql'])
  assert.ok(args.includes(`head=${HEAD}`))
})
