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
        labels: { agentFriendly: 'agent-friendly' },
        ...(selection === undefined ? {} : { selection }),
      },
    },
  })
  // Validated, not hand-rolled: a synthetic config that the real loader would have rejected would
  // make every assertion below a statement about an input no operator can actually produce.
  validateConfig(config, 'test')
  return config
}

// THE INERTNESS PIN. The seam ships with no switch, so a repo carrying no `selection` block must
// hand the tracker the argument object the merged tree handed it before this ticket. Asserted as
// an EXACT object — an extra key of any name, including one present and undefined, fails here.
test('the gate passes exactly {state, label} when no selection is configured', async () => {
  const tracker = recordingTracker(true)
  const { hasWork, reason } = await evaluateBossBuildGate({
    config: configWith(undefined),
    tracker,
  })
  assert.equal(tracker.calls.length, 1)
  assert.deepEqual(tracker.calls[0], { state: 'Planned', label: 'agent-friendly' })
  // A present-but-undefined key serialises differently downstream, so read key PRESENCE and not
  // just the value. `deepEqual` above already rejects it; this says so in the failure message.
  assert.equal('assigneeOrCreator' in tracker.calls[0], false)
  assert.equal(hasWork, true)
  assert.equal(reason, null)
})

test('the skip reason for an un-narrowed scan is the pre-seam string', async () => {
  const { hasWork, reason } = await evaluateBossBuildGate({
    config: configWith(undefined),
    tracker: recordingTracker(false),
  })
  assert.equal(hasWork, false)
  assert.equal(reason, 'boss-build gate: no unblocked Planned agent-friendly issues')
})

test('a configured label set SUPERSEDES the single agentFriendly label', async () => {
  const tracker = recordingTracker(false)
  await evaluateBossBuildGate({
    config: configWith({ labels: ['label-a', 'label-b'] }),
    tracker,
  })
  assert.deepEqual(tracker.calls[0], {
    state: 'Planned',
    label: ['label-a', 'label-b'],
  })
  // Supersede, not union: `agent-friendly` is still the configured role and is still resolvable,
  // and it must NOT appear in the selector. A union would widen the scan, which is the opposite
  // of what this seam is for, and a merge bug would look identical without this assertion.
  assert.equal(tracker.calls[0].label.includes('agent-friendly'), false)
})

test('a configured assigneeOrCreator is forwarded alongside the label selector', async () => {
  const tracker = recordingTracker(false)
  await evaluateBossBuildGate({
    config: configWith({ assigneeOrCreator: 'me' }),
    tracker,
  })
  // Identity narrowing alone leaves the label selector at the configured single role.
  assert.deepEqual(tracker.calls[0], {
    state: 'Planned',
    label: 'agent-friendly',
    assigneeOrCreator: 'me',
  })
})

test('both selectors are forwarded together when both are configured', async () => {
  const tracker = recordingTracker(false)
  await evaluateBossBuildGate({
    config: configWith({ assigneeOrCreator: 'usr_1', labels: ['label-a', 'label-b'] }),
    tracker,
  })
  assert.deepEqual(tracker.calls[0], {
    state: 'Planned',
    label: ['label-a', 'label-b'],
    assigneeOrCreator: 'usr_1',
  })
})

// The operator-facing half of the wiring. A narrowed scan that found nothing and an empty backlog
// are different situations calling for opposite responses, and the gate-output log is the only
// place either is visible — so the reason has to name what was actually filtered on.
test('the skip reason names the effective filter, distinguishing a narrowed scan', async () => {
  const narrowed = await evaluateBossBuildGate({
    config: configWith({ assigneeOrCreator: 'me', labels: ['label-a', 'label-b'] }),
    tracker: recordingTracker(false),
  })
  assert.equal(
    narrowed.reason,
    'boss-build gate: no unblocked Planned [label-a|label-b] issues assigned to or created by me',
  )
  const unnarrowed = await evaluateBossBuildGate({
    config: configWith(undefined),
    tracker: recordingTracker(false),
  })
  // The discriminating assertion, not merely two spellings: an operator must be able to tell the
  // two logs apart, and a renderer that ignored the selection block would make them identical.
  assert.notEqual(narrowed.reason, unnarrowed.reason)
})

test('a gate that found work reports no reason, narrowed or not', async () => {
  for (const selection of [undefined, { assigneeOrCreator: 'me', labels: ['label-a'] }]) {
    const { hasWork, reason } = await evaluateBossBuildGate({
      config: configWith(selection),
      tracker: recordingTracker(true),
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
