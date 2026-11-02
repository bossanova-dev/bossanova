// Parity: the same head, read once through raw `gh` and once through the daemon's `boss pr state`
// envelope, reaches the same verdict state and reason — at ci-wait's reading and at the classifier
// the `pr-check-state.mjs classify` CLI runs. Each scenario states the GitHub-side payloads and the
// envelope the daemon builds from that same GitHub state, including the daemon's own
// `check_verdict`, which differs from the agent-side verdict in the coordinator cases and must
// never be what either path keys on.

import assert from 'node:assert/strict'
import test from 'node:test'

import { takeReading } from '../ci-wait.mjs'
import { CHECK_REASONS, CHECK_STATES, classifyChecks } from '../pr-check-state.mjs'
import { daemonFirstReader, ghCheckReader, readPRView } from '../pr-state.mjs'
import { classifyInputsFromPrState } from './pr-state-envelope.mjs'
import { requiredChecksFor } from './required-checks.mjs'

const HEAD = 'a'.repeat(40)
const T0 = 1_800_000_000_000
const FRESH = new Date(T0).toISOString()

const ghCheck = (name, conclusion, status = 'COMPLETED') => ({
  __typename: 'CheckRun',
  name,
  status,
  conclusion: status === 'COMPLETED' ? conclusion : '',
  workflowName: 'CI',
})
const ghStatus = (context, state) => ({ __typename: 'StatusContext', context, state })
const ghRun = (name, status, conclusion = '') => ({
  name,
  workflowName: name,
  status,
  conclusion,
  headSha: HEAD,
  event: 'pull_request',
})
const daemonCheck = (name, status, conclusion = '', extra = {}) => ({
  name,
  status,
  conclusion,
  description: '',
  is_unclassified: false,
  ...extra,
})
const daemonRun = (name, status, conclusion = '') => ({
  name,
  status,
  conclusion,
  event: 'pull_request',
  head_sha: HEAD,
})
const verdictOf = (state, reason) => ({ state, reason })

function envelope({ checks, runs, verdict, settled = null, runsRead = true, phase = 'none' }) {
  return {
    repo: 'acme/widgets',
    pr_number: 7,
    state: 'open',
    head_sha: HEAD,
    base_branch: 'main',
    checks,
    check_verdict: verdict,
    verify_phase: { kind: phase, reason: '', label: '' },
    has_settled_workflow_runs: settled,
    held_workflow_runs: [],
    workflow_runs: runsRead ? runs : [],
    is_workflow_runs_read: runsRead,
  }
}

const SCENARIOS = [
  {
    name: 'green',
    want: verdictOf(CHECK_STATES.GREEN, CHECK_REASONS.OK),
    gh: {
      rollup: [ghCheck('build', 'SUCCESS'), ghCheck('lint', 'SUCCESS')],
      runs: [ghRun('CI', 'completed', 'success')],
    },
    daemon: envelope({
      checks: [
        daemonCheck('build', 'completed', 'success'),
        daemonCheck('lint', 'completed', 'success'),
      ],
      runs: [daemonRun('CI', 'completed', 'success')],
      verdict: verdictOf('green', 'ok'),
      settled: true,
    }),
  },
  {
    name: 'failing',
    want: verdictOf(CHECK_STATES.FAILING, CHECK_REASONS.FAILED),
    gh: {
      rollup: [ghCheck('build', 'FAILURE'), ghCheck('lint', 'SUCCESS')],
      runs: [ghRun('CI', 'completed', 'failure')],
    },
    daemon: envelope({
      checks: [
        daemonCheck('build', 'completed', 'failure'),
        daemonCheck('lint', 'completed', 'success'),
      ],
      runs: [daemonRun('CI', 'completed', 'failure')],
      verdict: verdictOf('failing', 'failed'),
    }),
  },
  {
    name: 'failing with the runs not read',
    want: verdictOf(CHECK_STATES.FAILING, CHECK_REASONS.FAILED),
    gh: { rollup: [ghCheck('build', 'FAILURE')], runs: [ghRun('CI', 'completed', 'failure')] },
    daemon: envelope({
      checks: [daemonCheck('build', 'completed', 'failure')],
      runsRead: false,
      verdict: verdictOf('failing', 'failed'),
    }),
  },
  {
    name: 'pending head run',
    want: verdictOf(CHECK_STATES.PENDING, CHECK_REASONS.PENDING),
    gh: {
      rollup: [ghCheck('build', 'SUCCESS'), ghCheck('e2e', '', 'IN_PROGRESS')],
      runs: [ghRun('CI', 'in_progress')],
    },
    daemon: envelope({
      checks: [daemonCheck('build', 'completed', 'success'), daemonCheck('e2e', 'in_progress')],
      runs: [daemonRun('CI', 'in_progress')],
      verdict: verdictOf('pending', 'pending'),
    }),
  },
  {
    name: 'prior-head absent gate',
    prior: ['build', 'e2e'],
    want: verdictOf(CHECK_STATES.PENDING, CHECK_REASONS.ABSENT_GATE),
    gh: { rollup: [ghCheck('build', 'SUCCESS')], runs: [ghRun('CI', 'completed', 'success')] },
    daemon: envelope({
      checks: [daemonCheck('build', 'completed', 'success')],
      runs: [daemonRun('CI', 'completed', 'success')],
      verdict: verdictOf('green', 'ok'),
      settled: true,
    }),
  },
  {
    name: 'not-requested under ci.required',
    required: ['e2e'],
    want: verdictOf(CHECK_STATES.UNKNOWN, CHECK_REASONS.NOT_REQUESTED),
    gh: { rollup: [ghCheck('build', 'SUCCESS')], runs: [ghRun('CI', 'completed', 'success')] },
    daemon: envelope({
      checks: [daemonCheck('build', 'completed', 'success')],
      runs: [daemonRun('CI', 'completed', 'success')],
      verdict: verdictOf('green', 'ok'),
      settled: true,
    }),
  },
  {
    name: 'live verify claim',
    want: verdictOf(CHECK_STATES.GREEN, CHECK_REASONS.OK),
    gh: {
      rollup: [ghCheck('build', 'SUCCESS'), ghStatus('boss/verify', 'PENDING')],
      runs: [ghRun('CI', 'completed', 'success')],
      statuses: [
        [
          {
            context: 'boss/verify',
            state: 'pending',
            description: 'verifying… tok1',
            updated_at: FRESH,
          },
        ],
      ],
    },
    daemon: envelope({
      checks: [
        daemonCheck('build', 'completed', 'success'),
        daemonCheck('boss/verify', 'queued', '', { description: 'verifying… tok1' }),
      ],
      runs: [daemonRun('CI', 'completed', 'success')],
      verdict: verdictOf('green', 'ok'),
      settled: true,
      phase: 'verifying',
    }),
  },
  {
    name: 'unclassified node',
    want: verdictOf(CHECK_STATES.UNKNOWN, CHECK_REASONS.UNCLASSIFIED),
    gh: {
      rollup: [
        ghCheck('build', 'SUCCESS'),
        { name: 'mystery', status: 'COMPLETED', conclusion: null },
      ],
      runs: [ghRun('CI', 'completed', 'success')],
      buckets: [{ name: 'mystery', state: 'MYSTERY', bucket: '' }],
    },
    daemon: envelope({
      checks: [
        daemonCheck('build', 'completed', 'success'),
        daemonCheck('mystery', 'completed', '', { is_unclassified: true }),
      ],
      runs: [daemonRun('CI', 'completed', 'success')],
      verdict: verdictOf('unknown', 'unclassified'),
    }),
  },
  {
    name: 'all skipped or neutral (daemon: green no-gate-ran)',
    want: verdictOf(CHECK_STATES.UNKNOWN, CHECK_REASONS.NO_GATE_RAN),
    gh: {
      rollup: [ghCheck('build', 'SKIPPED'), ghCheck('lint', 'NEUTRAL')],
      runs: [ghRun('CI', 'completed', 'skipped')],
    },
    daemon: envelope({
      checks: [
        daemonCheck('build', 'completed', 'skipped'),
        daemonCheck('lint', 'completed', 'neutral'),
      ],
      runs: [daemonRun('CI', 'completed', 'skipped')],
      verdict: verdictOf('green', 'no-gate-ran'),
      settled: true,
    }),
  },
  {
    name: 'workflow run still in flight (daemon: green, not settled)',
    want: verdictOf(CHECK_STATES.PENDING, CHECK_REASONS.PENDING),
    gh: {
      rollup: [ghCheck('build', 'SUCCESS')],
      runs: [ghRun('CI', 'completed', 'success'), ghRun('Deploy', 'in_progress')],
    },
    daemon: envelope({
      checks: [daemonCheck('build', 'completed', 'success')],
      runs: [daemonRun('CI', 'completed', 'success'), daemonRun('Deploy', 'in_progress')],
      verdict: verdictOf('green', 'ok'),
      settled: false,
    }),
  },
  {
    name: 'required check missing before any run (daemon: unknown not-requested)',
    required: ['e2e'],
    want: verdictOf(CHECK_STATES.PENDING, CHECK_REASONS.PENDING),
    gh: { rollup: [ghStatus('external/scan', 'SUCCESS')], runs: [] },
    daemon: envelope({
      checks: [daemonCheck('external/scan', 'completed', 'success')],
      runs: [],
      verdict: verdictOf('unknown', 'not-requested'),
    }),
  },
]

// The gh side answers the reads ghCheckReader spawns, keyed by the gh verb.
function ghSpawn(scenario) {
  return (cmd, args) => {
    assert.equal(cmd, 'gh')
    const verb = args.slice(0, 2).join(' ')
    const out = (value) => ({ status: 0, stdout: JSON.stringify(value) })
    if (verb === 'pr view')
      return out({ headRefOid: HEAD, baseRefName: 'main', statusCheckRollup: scenario.gh.rollup })
    if (verb === 'run list') return out(scenario.gh.runs)
    if (args[0] === 'api') return out(scenario.gh.statuses ?? [[]])
    if (verb === 'pr checks')
      return scenario.gh.buckets ? out(scenario.gh.buckets) : { status: 1, stdout: '' }
    throw new Error(`unexpected gh ${args.join(' ')}`)
  }
}

// The daemon side answers `boss pr state` with the envelope and refuses every gh spawn.
function daemonDeps(scenario) {
  return {
    resolveBoss: () => ({ ok: true, path: '/fake/boss' }),
    spawn: (cmd, args) => {
      assert.equal(cmd, '/fake/boss', `the daemon path spawned ${cmd} ${args.join(' ')}`)
      assert.deepEqual(args.slice(0, 2), ['pr', 'state'])
      return { status: 0, stdout: JSON.stringify(scenario.daemon) }
    },
  }
}

function reading(read, scenario) {
  return takeReading({
    read,
    pr: 7,
    readTimeoutMs: 60_000,
    deadline: T0 + 480_000,
    priorContexts: scenario.prior ?? null,
    ciRequired: scenario.required ? { checks: scenario.required } : null,
    now: () => T0,
  })
}

for (const scenario of SCENARIOS) {
  test(`parity — ${scenario.name}: ci-wait reads the same verdict through gh and the daemon`, async () => {
    const gh = await reading(ghCheckReader({ deps: { spawn: ghSpawn(scenario) } }), scenario)
    const daemonRead = daemonFirstReader({ deps: daemonDeps(scenario) })
    const daemon = await reading(daemonRead, scenario)
    assert.equal(daemonRead.source(), 'daemon')
    assert.deepEqual(verdictOf(gh.verdict.state, gh.verdict.reason), scenario.want, 'gh path')
    assert.deepEqual(
      verdictOf(daemon.verdict.state, daemon.verdict.reason),
      scenario.want,
      'daemon path',
    )
    assert.equal(daemon.verifyHold?.kind ?? null, gh.verifyHold?.kind ?? null)
  })

  test(`parity — ${scenario.name}: classify reads the same verdict from gh payloads and the envelope`, () => {
    const requiredChecks = requiredChecksFor(
      scenario.required ? { checks: scenario.required } : null,
      'main',
    )
    const fromGh = classifyChecks({
      headSHA: HEAD,
      observedSHA: HEAD,
      rollup: { statusCheckRollup: scenario.gh.rollup },
      priorContexts: scenario.prior ?? null,
      workflowRuns: scenario.gh.runs,
      requiredChecks,
    })
    const fromDaemon = classifyChecks({
      ...classifyInputsFromPrState({ source: 'daemon', state: scenario.daemon }),
      priorContexts: scenario.prior ?? null,
      requiredChecks,
    })
    assert.deepEqual(
      verdictOf(fromDaemon.state, fromDaemon.reason),
      verdictOf(fromGh.state, fromGh.reason),
    )
  })

  test(`parity — ${scenario.name}: the view verb's rollup classifies like gh's`, () => {
    const requiredChecks = requiredChecksFor(
      scenario.required ? { checks: scenario.required } : null,
      'main',
    )
    const view = readPRView({ pr: 7, timeoutMs: 60_000, deps: daemonDeps(scenario) })
    assert.equal(view.out.source, 'daemon')
    const classify = (rollup, prior) =>
      classifyChecks({
        headSHA: HEAD,
        observedSHA: view.out.headRefOid,
        rollup,
        priorContexts: prior,
        workflowRuns: scenario.gh.runs,
        requiredChecks,
      })
    const fromGh = classify({ statusCheckRollup: scenario.gh.rollup }, scenario.prior ?? null)
    const fromView = classify(view.out, scenario.prior ?? null)
    assert.deepEqual(
      verdictOf(fromView.state, fromView.reason),
      verdictOf(fromGh.state, fromGh.reason),
    )
    // A prior head's view is a `--prior` payload: the same set as prior reads as no absent gate.
    const againstItself = classify(view.out, view.out)
    assert.equal(againstItself.priorKnown, true)
    assert.deepEqual(againstItself.absent, [])
  })
}

// A path-filtered push drops a whole workflow's checks. The prior head's view, saved as `--prior`,
// names each check's workflow on the daemon path as gh's rollup does, so a check whose workflow did
// not run on the head is `notTriggered` either way; a prior without workflow names stays absent.
test('parity — a prior head saved through view keeps workflow identity for notTriggered', () => {
  const priorDaemon = envelope({
    checks: [
      daemonCheck('build', 'completed', 'success', { workflow_name: 'CI' }),
      daemonCheck('docs-lint', 'completed', 'success', { workflow_name: 'Docs' }),
    ],
    runs: [daemonRun('CI', 'completed', 'success'), daemonRun('Docs', 'completed', 'success')],
    verdict: verdictOf('green', 'ok'),
    settled: true,
  })
  const priorGh = [
    ghCheck('build', 'SUCCESS'),
    { ...ghCheck('docs-lint', 'SUCCESS'), workflowName: 'Docs' },
  ]
  const deps = {
    resolveBoss: () => ({ ok: true, path: '/fake/boss' }),
    spawn: (cmd, args) => {
      assert.equal(cmd, '/fake/boss', `the daemon path spawned ${cmd} ${args.join(' ')}`)
      return { status: 0, stdout: JSON.stringify(priorDaemon) }
    },
  }
  const view = readPRView({ pr: 7, timeoutMs: 60_000, deps })
  assert.equal(view.out.source, 'daemon')
  const classify = (prior) =>
    classifyChecks({
      headSHA: HEAD,
      observedSHA: HEAD,
      rollup: { statusCheckRollup: [ghCheck('build', 'SUCCESS')] },
      priorContexts: prior,
      workflowRuns: [ghRun('CI', 'completed', 'success')],
    })
  const fromView = classify(view.out)
  const fromGh = classify({ statusCheckRollup: priorGh })
  assert.deepEqual(fromView.notTriggered, ['docs-lint'])
  assert.deepEqual(
    verdictOf(fromView.state, fromView.reason),
    verdictOf(fromGh.state, fromGh.reason),
  )
  assert.equal(fromView.state, CHECK_STATES.GREEN)

  const unplaced = classify({
    statusCheckRollup: view.out.statusCheckRollup.map(({ workflowName, ...node }) => node),
  })
  assert.deepEqual(
    verdictOf(unplaced.state, unplaced.reason),
    verdictOf(CHECK_STATES.PENDING, CHECK_REASONS.ABSENT_GATE),
  )
})
