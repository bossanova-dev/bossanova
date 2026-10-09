import { test } from 'node:test'
import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { fileURLToPath } from 'node:url'

import { evaluateBossPlanGate } from './boss-plan.mjs'
import { DEFAULT_CONFIG, mergeConfig, validateConfig } from '../skill-config.mjs'

// Records the exact argument object the gate hands the tracker: the point of this suite is that
// outgoing query shape, which a true/false stub could not see drift.
function recordingTracker(hasWork = false) {
  const tracker = {
    calls: [],
    hasWork(query) {
      tracker.calls.push(query)
      return Promise.resolve(hasWork)
    },
  }
  return tracker
}

function configWith(selection) {
  const config = mergeConfig(DEFAULT_CONFIG, {
    adapters: { ...DEFAULT_CONFIG.adapters, tracker: 'demo' },
    trackerConfig: {
      demo: {
        mcpServer: 'demo-tracker',
        team: 'Demo',
        states: { planned: 'Planned', unplanned: 'Unplanned' },
        labels: { agentBuild: 'agent-build', agentPlan: 'agent-plan', needsHuman: 'needs-human' },
        ...(selection === undefined ? {} : { selection }),
      },
    },
  })
  validateConfig(config, 'test')
  return config
}

const EMPTY = {
  labels: { include: [], exclude: [] },
  assignees: { include: [], exclude: [] },
  creators: { include: [], exclude: [] },
  projects: { include: [], exclude: [] },
}

test('with no selection the plan gate scans unplanned AND agent-plan AND NOT needs-human', async () => {
  const tracker = recordingTracker(true)
  const { hasWork, reason } = await evaluateBossPlanGate({ config: configWith(undefined), tracker })
  assert.deepEqual(tracker.calls, [
    {
      state: 'Unplanned',
      selection: EMPTY,
      requireLabels: ['agent-plan'],
      excludeLabels: ['needs-human'],
    },
  ])
  assert.equal(hasWork, true)
  assert.equal(reason, null)
})

test('config only: stages.plan applies to the plan gate and stages.build does not', async () => {
  const tracker = recordingTracker(false)
  await evaluateBossPlanGate({
    config: configWith({
      assignees: { include: ['me'] },
      stages: { plan: { labels: { exclude: ['infra'] } }, build: { labels: { include: ['x'] } } },
    }),
    tracker,
  })
  assert.deepEqual(tracker.calls[0].selection.labels, { include: [], exclude: ['infra'] })
  assert.deepEqual(tracker.calls[0].selection.assignees, { include: ['me'], exclude: [] })
})

test('flags only and flag over config narrow per slot', async () => {
  const flagsOnly = recordingTracker(false)
  await evaluateBossPlanGate({
    config: configWith(undefined),
    tracker: flagsOnly,
    argv: ['--project', 'Platform', '--exclude-creator=bot@example.com'],
  })
  assert.deepEqual(flagsOnly.calls[0].selection.projects, { include: ['Platform'], exclude: [] })
  assert.deepEqual(flagsOnly.calls[0].selection.creators, {
    include: [],
    exclude: ['bot@example.com'],
  })

  const over = recordingTracker(false)
  await evaluateBossPlanGate({
    config: configWith({ labels: { include: ['backend'], exclude: ['a'] } }),
    tracker: over,
    argv: ['--exclude-label', 'b'],
  })
  assert.deepEqual(over.calls[0].selection.labels, { include: ['backend'], exclude: ['b'] })
})

test('the plan gate refuses a positional or malformed argument before any tracker call', async () => {
  for (const argv of [['BOS-1'], ['--headless'], ['--exclude-label']]) {
    const tracker = recordingTracker(true)
    await assert.rejects(evaluateBossPlanGate({ config: configWith(undefined), tracker, argv }))
    assert.equal(tracker.calls.length, 0, JSON.stringify(argv))
  }
})

test('the skip reason distinguishes a narrowed scan from an empty backlog', async () => {
  const plain = await evaluateBossPlanGate({
    config: configWith(undefined),
    tracker: recordingTracker(false),
  })
  assert.equal(
    plain.reason,
    'boss-plan gate: no Unplanned issues labelled agent-plan without needs-human',
  )
  const narrowed = await evaluateBossPlanGate({
    config: configWith(undefined),
    tracker: recordingTracker(false),
    argv: ['--exclude-label', 'infra'],
  })
  assert.equal(
    narrowed.reason,
    'boss-plan gate: no Unplanned issues labelled agent-plan without needs-human matching selection labels.exclude=[infra]',
  )
})

// THE PROCESS-LEVEL PINS: both registered entry points, spawned without the credential, must exit
// 1 with the gate's own reason. An entry point whose body never runs exits 0 — "run the planner".
const REPO_ROOT = fileURLToPath(new URL('../../', import.meta.url))
const ENTRY_POINTS = [
  ['canonical', 'skills-toolbox/cron-gates/boss-plan.mjs'],
  ['scripts shim', 'scripts/cron-gates/boss-plan.mjs'],
]

for (const [label, relativePath] of ENTRY_POINTS) {
  test(`${label}: the entry point exits 1 naming LINEAR_API_KEY when the credential is absent`, () => {
    const env = { ...process.env }
    delete env.LINEAR_API_KEY
    const result = spawnSync(process.execPath, [relativePath], {
      cwd: REPO_ROOT,
      env,
      encoding: 'utf8',
    })
    assert.equal(result.status, 1, `${relativePath} stderr: ${JSON.stringify(result.stderr)}`)
    assert.match(result.stderr ?? '', /boss-plan gate: LINEAR_API_KEY is not set/)
  })
}
