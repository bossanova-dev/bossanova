import assert from 'node:assert/strict'
import { test } from 'node:test'
import { createGithubAdapter } from '../tracker/github.mjs'
import { runCli } from '../tracker/cli.mjs'
import { withTrackerDefaults } from '../skill-config.mjs'
import { gateFail } from '../linear-gate-lib.mjs'
import { evaluateBossPlanGate } from './boss-plan.mjs'
import { evaluateBossBuildGate } from './boss-build.mjs'

const labels = ['boss:unplanned', 'boss:todo', 'agent-plan', 'agent-build', 'needs-human', 'area']
const configFor = (selection) =>
  withTrackerDefaults({
    adapters: { tracker: 'github' },
    trackerConfig: { github: { repository: 'acme/tasks', ...(selection ? { selection } : {}) } },
  })
const row = (number, stage, extra = [], state = 'OPEN') => ({
  number,
  title: `Issue ${number}`,
  body: 'Implement this',
  state,
  labels: [stage === 'plan' ? 'boss:unplanned' : 'boss:todo', `agent-${stage}`, ...extra].map(
    (name) => ({ name }),
  ),
  assignees: [],
  author: { login: 'alice' },
  createdAt: '2026-01-01',
  url: `https://github.com/acme/tasks/issues/${number}`,
})
function fixture({ rows = [], repositoryLabels = labels, blockers = {}, failure } = {}) {
  const calls = []
  const spawn = (_command, args) => {
    calls.push(args)
    const failed = failure?.(args)
    if (failed) return failed
    let output
    if (args[0] === 'label') output = repositoryLabels.map((name) => ({ name }))
    else if (args[0] === 'issue') {
      const required = args.filter((_, index) => args[index - 1] === '--label')
      output = rows.filter(
        (item) =>
          item.state === 'OPEN' &&
          required.every((name) => item.labels.some((label) => label.name === name)),
      )
    } else if (args[0] === 'api') {
      const number = args[1].split('/')[4]
      const edges = blockers[number] ?? []
      if (args[1].endsWith('/parent')) return { status: 1, stderr: 'HTTP 404' }
      output =
        args[1].endsWith('/blocking') || args[1].endsWith('/comments')
          ? [[]]
          : args[1].endsWith('/blocked_by')
            ? [
                edges.map((state, index) => ({
                  number: 100 + index,
                  state,
                  state_reason: 'completed',
                  labels: [],
                  html_url: `https://github.com/acme/tasks/issues/${100 + index}`,
                })),
              ]
            : {
                number: Number(number),
                state: 'open',
                issue_dependencies_summary: { total_blocked_by: edges.length, total_blocking: 0 },
              }
    } else throw new Error(`unexpected gh command ${args.join(' ')}`)
    return { status: 0, stdout: JSON.stringify(output) }
  }
  return { calls, tracker: (config) => createGithubAdapter({ config, spawn }) }
}
async function worker(stage, tracker, config, team = 'acme/tasks') {
  let stdout = '',
    stderr = ''
  const status = await runCli(
    [
      stage === 'plan' ? 'list-unplanned' : 'list-planned',
      ...(team === null ? [] : ['--team', team]),
    ],
    {
      resolveAdapter: () => tracker,
      loadConfig: () => config,
      write: (text) => {
        stdout += text
      },
      errWrite: (text) => {
        stderr += text
      },
    },
  )
  return { status, stderr, rows: stdout ? JSON.parse(stdout) : [] }
}
const evaluate = (stage, tracker, config) =>
  (stage === 'plan' ? evaluateBossPlanGate : evaluateBossBuildGate)({ tracker, config })
const ids = (rows) => rows.map((item) => item.id)
const listArgs = (calls) => calls.filter((args) => args[0] === 'issue' && args[1] === 'list')

for (const [name, options, selection, expected] of [
  ['eligible', { rows: [row(1, 'plan')] }, null, true],
  ['needs human', { rows: [row(1, 'plan', ['needs-human'])] }, null, false],
  ['outside selection', { rows: [row(1, 'plan')] }, { labels: { include: ['area'] } }, false],
  ['closed', { rows: [row(1, 'plan', [], 'CLOSED')] }, null, false],
  ['two roles', { rows: [row(1, 'plan', ['boss:todo'])] }, null, false],
  ['empty', {}, null, false],
])
  test(`GitHub plan gate and worker agree: ${name}`, async () => {
    const config = configFor(selection)
    const gate = fixture(options),
      scan = fixture(options)
    const decision = await evaluate('plan', gate.tracker(config), config)
    const result = await worker('plan', scan.tracker(config), config)
    assert.equal(result.status, 0, result.stderr)
    assert.equal(decision.hasWork, expected)
    assert.equal(decision.hasWork, result.rows.length > 0)
    assert.deepEqual(listArgs(gate.calls), listArgs(scan.calls))
  })

for (const [name, options, expected] of [
  ['unblocked', { rows: [row(1, 'build')] }, true],
  ['open blocker', { rows: [row(1, 'build')], blockers: { 1: ['open'] } }, false],
  ['closed blocker', { rows: [row(1, 'build')], blockers: { 1: ['closed'] } }, true],
  ['needs human', { rows: [row(1, 'build', ['needs-human'])] }, false],
])
  test(`GitHub build gate and worker agree: ${name}`, async () => {
    const config = configFor()
    const gate = fixture(options),
      scan = fixture(options)
    const decision = await evaluate('build', gate.tracker(config), config)
    const tracker = scan.tracker(config)
    const result = await worker('build', tracker, config)
    assert.equal(result.status, 0, result.stderr)
    const unblocked = []
    for (const candidate of result.rows) {
      const issue = await tracker.runOperation('get-issue', { id: candidate.id })
      if (tracker.isUnblocked(issue)) unblocked.push(candidate)
    }
    assert.equal(decision.hasWork, expected)
    assert.equal(decision.hasWork, unblocked.length > 0)
    assert.deepEqual(listArgs(gate.calls), listArgs(scan.calls))
  })

for (const stage of ['plan', 'build'])
  test(`GitHub ${stage} operation agrees with unnarrowed worker`, async () => {
    const config = configFor()
    const options = { rows: [row(1, stage), row(2, stage, ['needs-human'])] }
    const tracker = fixture(options).tracker(config)
    const result = await worker(stage, tracker, config)
    const operation = tracker.operationMap.selectPlanned.command
    const rows = await tracker.runOperation(operation.verb, {
      state: stage === 'plan' ? 'boss:unplanned' : 'boss:todo',
      labels: [`agent-${stage}`],
      limit: 250,
    })
    assert.equal(result.status, 0, result.stderr)
    assert.deepEqual(
      ids(rows.filter((item) => !item.labels.includes('needs-human'))),
      ids(result.rows),
    )
  })

for (const stage of ['plan', 'build']) {
  for (const [name, options, cause] of [
    ['missing gh', { failure: () => ({ error: { code: 'ENOENT' } }) }, /gh-missing/],
    ['auth', { failure: () => ({ status: 1, stderr: 'HTTP 401' }) }, /401.*gh auth login/],
    [
      'rate limit',
      { failure: () => ({ status: 1, stderr: 'API rate limit exceeded' }) },
      /429.*rate limit/,
    ],
    [
      'state label',
      {
        repositoryLabels: labels.filter(
          (name) => name !== (stage === 'plan' ? 'boss:unplanned' : 'boss:todo'),
        ),
      },
      /missing label boss:/,
    ],
    [
      'stage label',
      { repositoryLabels: labels.filter((name) => name !== `agent-${stage}`) },
      new RegExp(`missing label agent-${stage}`),
    ],
    [
      'truncated issues',
      { rows: Array.from({ length: 1000 }, (_, i) => row(i + 1, stage)) },
      /1000 cap/,
    ],
    ...(stage === 'build'
      ? [
          [
            'dependency',
            {
              rows: [row(1, stage)],
              failure: (args) =>
                args[0] === 'api' ? { status: 1, stderr: 'dependency read failed' } : null,
            },
            /dependency read failed/,
          ],
        ]
      : []),
  ])
    test(`GitHub ${stage} gate cannot silently skip on ${name}`, async () => {
      const config = configFor()
      let exit,
        stderr = '',
        decision
      try {
        decision = await evaluate(stage, fixture(options).tracker(config), config)
      } catch (error) {
        gateFail(`boss-${stage} gate: ${error.message}`, {
          exit: (code) => {
            exit = code
          },
          stderr: {
            write: (text) => {
              stderr += text
            },
          },
        })
      }
      assert.equal(decision, undefined)
      assert.equal(exit, 69)
      assert.match(stderr, cause)
      assert.equal(stderr.trim().split('\n').length, 1)
    })
}

test('GitHub worker accepts repository spellings and rejects a foreign team with exit 2', async () => {
  const config = configFor()
  const tracker = fixture({
    rows: [row(1, 'plan')],
    repositoryLabels: labels.filter((name) => name !== 'needs-human'),
  }).tracker(config)
  const baseline = await worker('plan', tracker, config, null)
  assert.deepEqual(ids(baseline.rows), ['GH-1'])
  for (const team of ['acme/tasks', 'ACME/TASKS', 'github.com/ACME/TASKS'])
    assert.deepEqual(await worker('plan', tracker, config, team), baseline)
  const rejected = await worker('plan', tracker, config, 'other/tasks')
  assert.equal(rejected.status, 2)
  assert.match(
    rejected.stderr,
    /--team other\/tasks does not name the configured repository acme\/tasks/,
  )
  assert.deepEqual(rejected.rows, [])
})
