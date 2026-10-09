import { test } from 'node:test'
import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { fileURLToPath } from 'node:url'

import { evaluateBossBuildGate } from './boss-build.mjs'
import { DEFAULT_CONFIG, mergeConfig, validateConfig } from '../skill-config.mjs'

// A tracker stand-in that records the exact argument object it was handed. The RECORDED object is
// what every assertion below reads: the point of this suite is the gate's outgoing argument shape,
// and a stub that only answered true/false would let that shape drift silently — which is exactly
// what it did before this entry point was reachable from a test at all.
function recordingTracker(hasWork = false) {
  const tracker = {
    calls: [],
    hasUnblockedWork(query) {
      tracker.calls.push(query)
      return Promise.resolve(hasWork)
    },
  }
  return tracker
}

/** A validated config whose tracker adapter carries the given `selection` value (or none). */
function configWith(selection) {
  const config = mergeConfig(DEFAULT_CONFIG, {
    adapters: { ...DEFAULT_CONFIG.adapters, tracker: 'demo' },
    trackerConfig: {
      demo: {
        mcpServer: 'demo-tracker',
        team: 'Demo',
        states: { planned: 'Planned' },
        labels: { agentBuild: 'agent-build', needsHuman: 'needs-human' },
        ...(selection === undefined ? {} : { selection }),
      },
    },
  })
  // Validated, not hand-rolled: a synthetic config that the real loader would have rejected would
  // make every assertion below a statement about an input no operator can actually produce.
  validateConfig(config, 'test')
  return config
}

// The three precedence cases the gate and the worker must agree on: config only, flags only, and
// a flag overriding its one config slot. `cli.test.mjs` drives the SAME cases through
// `list-planned` and asserts the identical query.
const GATE_SELECTION_CASES = [
  [
    'config only',
    { labels: { include: ['backend'] }, stages: { build: { labels: { exclude: ['infra'] } } } },
    [],
  ],
  ['flags only', undefined, ['--exclude-label', 'infra', '--exclude-creator', 'bot@example.com']],
  [
    'flag over config',
    {
      labels: { include: ['backend'], exclude: ['shared-out'] },
      stages: { build: { labels: { exclude: ['infra'] } } },
    },
    ['--exclude-label', 'flag-out'],
  ],
]

test('with no selection the gate scans planned AND agent-build AND NOT needs-human, unnarrowed', async () => {
  const tracker = recordingTracker(true)
  const { hasWork, reason } = await evaluateBossBuildGate({
    config: configWith(undefined),
    tracker,
  })
  assert.equal(tracker.calls.length, 1)
  assert.deepEqual(tracker.calls[0], {
    state: 'Planned',
    selection: {
      labels: { include: [], exclude: [] },
      assignees: { include: [], exclude: [] },
      creators: { include: [], exclude: [] },
      projects: { include: [], exclude: [] },
    },
    requireLabels: ['agent-build'],
    excludeLabels: ['needs-human'],
  })
  assert.equal(hasWork, true)
  assert.equal(reason, null)
})

test('config only: the shared block and stages.build reach the tracker per slot', async () => {
  const tracker = recordingTracker(false)
  await evaluateBossBuildGate({ config: configWith(GATE_SELECTION_CASES[0][1]), tracker })
  assert.deepEqual(tracker.calls[0].selection.labels, { include: ['backend'], exclude: ['infra'] })
  assert.deepEqual(tracker.calls[0].requireLabels, ['agent-build'])
})

test('flags only: the gate argv narrows exactly like a config block', async () => {
  const tracker = recordingTracker(false)
  await evaluateBossBuildGate({
    config: configWith(undefined),
    tracker,
    argv: GATE_SELECTION_CASES[1][2],
  })
  assert.deepEqual(tracker.calls[0].selection.labels, { include: [], exclude: ['infra'] })
  assert.deepEqual(tracker.calls[0].selection.creators, {
    include: [],
    exclude: ['bot@example.com'],
  })
})

test('flag over config: a flag replaces its one slot and keeps the other polarity', async () => {
  const tracker = recordingTracker(false)
  await evaluateBossBuildGate({
    config: configWith(GATE_SELECTION_CASES[2][1]),
    tracker,
    argv: GATE_SELECTION_CASES[2][2],
  })
  assert.deepEqual(tracker.calls[0].selection.labels, {
    include: ['backend'],
    exclude: ['flag-out'],
  })
})

test('the gate refuses a positional or unknown argument before any tracker call', async () => {
  for (const argv of [['BOS-1'], ['--labels', 'x'], ['--label']]) {
    const tracker = recordingTracker(true)
    await assert.rejects(
      evaluateBossBuildGate({ config: configWith(undefined), tracker, argv }),
      /unexpected argument|requires a non-empty value/,
    )
    assert.equal(tracker.calls.length, 0, JSON.stringify(argv))
  }
})

test('the gate refuses a legacy selection block (validation throws at load)', () => {
  assert.throws(() => configWith({ assigneeOrCreator: 'me' }), /assigneeOrCreator was removed/)
})

// The operator-facing half of the wiring: a narrowed scan that found nothing and an empty backlog
// call for opposite responses, and the gate-output log is the only place either is visible.
test('the skip reason names the effective filter, distinguishing a narrowed scan', async () => {
  const unnarrowed = await evaluateBossBuildGate({
    config: configWith(undefined),
    tracker: recordingTracker(false),
  })
  assert.equal(
    unnarrowed.reason,
    'boss-build gate: no unblocked Planned issues labelled agent-build without needs-human',
  )
  const narrowed = await evaluateBossBuildGate({
    config: configWith({ stages: { build: { labels: { exclude: ['infra'] } } } }),
    tracker: recordingTracker(false),
    argv: ['--exclude-creator', 'bot@example.com'],
  })
  assert.equal(
    narrowed.reason,
    'boss-build gate: no unblocked Planned issues labelled agent-build without needs-human matching selection labels.exclude=[infra] creators.exclude=[bot@example.com]',
  )
  assert.notEqual(narrowed.reason, unnarrowed.reason)
})

test('a gate that found work reports no reason, narrowed or not', async () => {
  for (const argv of [[], ['--exclude-label', 'infra']]) {
    const { hasWork, reason } = await evaluateBossBuildGate({
      config: configWith(undefined),
      tracker: recordingTracker(true),
      argv,
    })
    assert.equal(hasWork, true)
    assert.equal(reason, null, 'a gate with work must not render a skip reason')
  }
})

// ---------------------------------------------------------------------------------------------
// THE PROCESS-LEVEL PINS.
//
// Everything above tests `evaluateBossBuildGate` — the decision. None of it can see whether the
// decision is REACHED, and that is the gap a fail-closed cron gate cannot afford: a registered
// entry point that runs nothing exits 0, which the scheduler reads as "there is work", turning
// the gate into an unconditional always-run. That is not hypothetical — it is exactly what the
// `isMainModule` guard did to the `scripts/` forwarding shim, which kept importing the canonical
// module for side effects that the guard had just stopped producing. Every decision test above
// stayed green through it.
//
// So both registered paths are spawned as real child processes here, with the credential removed,
// and asserted on their EXIT CODE. A gate that cannot prove work exists must not exit 0.
const REPO_ROOT = fileURLToPath(new URL('../../', import.meta.url))

// Both are registered entry points. `scripts/` is the path persisted in existing cron jobs'
// GateCommand, so it is as load-bearing as the canonical one and is tested identically.
const ENTRY_POINTS = [
  ['canonical', 'skills-toolbox/cron-gates/boss-build.mjs'],
  ['scripts shim', 'scripts/cron-gates/boss-build.mjs'],
]

/** Run an entry point as a child process with the tracker credential removed from its env. */
function runGateWithoutCredential(relativePath) {
  // The key is deleted rather than blanked: a developer machine or CI runner that exports a real
  // one would otherwise send this test to the network and make its verdict depend on a backlog.
  const env = { ...process.env }
  delete env.LINEAR_API_KEY
  const result = spawnSync(process.execPath, [relativePath], {
    cwd: REPO_ROOT,
    env,
    encoding: 'utf8',
  })
  return { status: result.status, stderr: result.stderr ?? '' }
}

for (const [label, relativePath] of ENTRY_POINTS) {
  test(`${label}: the entry point exits NON-ZERO when it cannot prove work exists`, () => {
    const { status, stderr } = runGateWithoutCredential(relativePath)
    // The assertion that would have caught the shim regression: an entry point whose body never
    // runs exits 0, and 0 means "run the implementer".
    assert.notEqual(
      status,
      0,
      `${relativePath} exited 0 without a credential; a fail-closed gate must not. stderr: ${JSON.stringify(stderr)}`,
    )
    assert.equal(status, 1, `${relativePath} must exit 1, the gateExit skip code`)
    // Exit 1 alone is satisfiable by a crash, which is a different failure with a different fix.
    // The reason line is what proves the gate's own fail-closed path ran and reported.
    assert.match(
      stderr,
      /boss-build gate: LINEAR_API_KEY is not set/,
      `${relativePath} must name its fail-closed reason on stderr for the scheduler's gate log`,
    )
  })
}
