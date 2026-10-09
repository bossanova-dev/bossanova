import { test } from 'node:test'
import assert from 'node:assert/strict'
import { execFileSync } from 'node:child_process'
import { existsSync, mkdtempSync, readFileSync, readdirSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'

import {
  MAX_DISPATCH_ATTEMPTS,
  PARALLEL_DEFAULT,
  fanoutSummary,
  initFanoutLedger,
  nextFanoutStep,
  parentFlipDecision,
  parsePlanArgs,
  recordChildOutcome,
  routeSelectedIssue,
  writeLedgerFile,
} from './plan-child-fanout.mjs'
import { requiredPlanSections, withTrackerDefaults, DEFAULT_CONFIG } from './skill-config.mjs'

// The CLI never records a gate outcome, but it imports a module that can; pin the destination so
// a spawned child under a live session never appends to the real firing-rate file.
process.env.BOSS_GATE_OUTCOME_FILE = join(
  mkdtempSync(join(tmpdir(), 'gate-outcome-fanout-')),
  'outcomes.tsv',
)

const CLI = fileURLToPath(new URL('./plan-child-fanout.mjs', import.meta.url))

const CONFIG = withTrackerDefaults({ ...DEFAULT_CONFIG, trackerConfig: { linear: {} } })
// Default role names: agent-plan, epic, needs-human, agent-build; states Backlog → Todo.

const plannedDescription = (id) =>
  `${requiredPlanSections(DEFAULT_CONFIG)
    .map((heading) => `${heading}\n\nBody for ${heading}.`)
    .join('\n\n')
    .replace('## Planning\n\nBody for ## Planning.', `## Planning\n\n- Contract: v1`)
    .replace(
      '## Key changes\n\nBody for ## Key changes.',
      `## Key changes\n\n- \`skills-toolbox/plan-child-fanout.mjs\`: ${id}.`,
    )}\n`

const issue = (id, overrides = {}) => ({
  id,
  uuid: `uuid-${id}`,
  title: `Title ${id}`,
  status: 'Backlog',
  statusType: 'unstarted',
  labels: [],
  priority: { value: 3, name: 'Medium' },
  createdAt: '2026-01-01T00:00:00Z',
  description: `notes for ${id}`,
  attachments: [],
  relations: { blockedBy: [] },
  ...overrides,
})

const planned = (id, overrides = {}) =>
  issue(id, {
    status: 'Todo',
    description: plannedDescription(id),
    attachments: [{ id: `att-${id}`, title: `Implementation plan (${id})` }],
    ...overrides,
  })

const PARENT = issue('ABC-1', { labels: [{ name: 'agent-plan' }] })

// --- parsePlanArgs ------------------------------------------------------------------------------

test('parsePlanArgs: no args is the sweep at the default width', () => {
  assert.deepEqual(parsePlanArgs([]), {
    mode: 'sweep',
    ticketId: null,
    parallel: PARALLEL_DEFAULT,
  })
  assert.equal(PARALLEL_DEFAULT, 4)
})

test('parsePlanArgs: a named ticket with --parallel 6', () => {
  assert.deepEqual(parsePlanArgs(['ABC-1', '--parallel', '6']), {
    mode: 'named',
    ticketId: 'ABC-1',
    parallel: 6,
  })
  assert.equal(parsePlanArgs(['--parallel=2', 'ABC-1']).parallel, 2)
})

test('parsePlanArgs: a pasted Linear URL resolves to its id', () => {
  const parsed = parsePlanArgs(['https://linear.app/acme/issue/ABC-42/some-slug'])
  assert.equal(parsed.ticketId, 'ABC-42')
  assert.equal(parsed.mode, 'named')
})

test('parsePlanArgs: --team and selection flags pass through rather than being rejected', () => {
  const parsed = parsePlanArgs(['--team', 'Core', '--label', 'infra'])
  assert.equal(parsed.team, 'Core')
  assert.equal(parsed.mode, 'sweep')
  assert.ok(parsed.selectionFlags, 'selection flags are returned for resolve-selection')
})

test('parsePlanArgs: --parallel out of range or non-integer throws', () => {
  for (const bad of ['0', '9', '2.5', 'x', '']) {
    assert.throws(
      () => parsePlanArgs(['--parallel', bad]),
      /--parallel must be an integer between 1 and 8/,
      `--parallel ${JSON.stringify(bad)} must throw`,
    )
  }
  assert.throws(() => parsePlanArgs(['--parallel']), /--parallel must be an integer/)
})

test('parsePlanArgs: an unknown flag, a stray word or two tickets throw', () => {
  assert.throws(() => parsePlanArgs(['--paralel', '3']), /unknown flag --paralel/)
  assert.throws(() => parsePlanArgs(['not-a-ticket']), /not a ticket id or Linear URL/)
  assert.throws(() => parsePlanArgs(['ABC-1', 'ABC-2']), /at most one ticket/)
})

// --- routeSelectedIssue -------------------------------------------------------------------------

test('routeSelectedIssue: a spec-attachment parent resumes even when children carry agent-plan', () => {
  const parent = issue('ABC-1', {
    attachments: [{ id: 'spec-1', title: 'Epic spec (ABC-1)' }],
  })
  const children = [issue('ABC-2', { labels: [{ name: 'agent-plan' }] })]
  const route = routeSelectedIssue({ parent, children, config: CONFIG })
  assert.equal(route.verdict, 'epic-resume')
})

test('routeSelectedIssue: two spec attachments abort', () => {
  const parent = issue('ABC-1', {
    attachments: [
      { id: 'spec-1', title: 'Epic spec (ABC-1)' },
      { id: 'spec-2', title: 'Epic spec (ABC-1)' },
    ],
  })
  assert.equal(routeSelectedIssue({ parent, children: [], config: CONFIG }).verdict, 'abort')
})

test('routeSelectedIssue: one Agent-Plan-spelled child fans out', () => {
  const children = [issue('ABC-2', { labels: ['Agent-Plan'] }), issue('ABC-3')]
  const route = routeSelectedIssue({ parent: PARENT, children, config: CONFIG })
  assert.equal(route.verdict, 'fan-out')
  assert.equal(route.labelledCount, 1)
  assert.equal(route.childCount, 2)
})

test('routeSelectedIssue: an archived labelled child does not fan out', () => {
  const children = [issue('ABC-2', { labels: ['agent-plan'], archivedAt: '2026-01-02T00:00:00Z' })]
  assert.equal(routeSelectedIssue({ parent: PARENT, children, config: CONFIG }).verdict, 'single')
})

test('routeSelectedIssue: an epic-labelled parent with only unlabelled children is a noop', () => {
  const parent = issue('ABC-1', { labels: [{ name: 'Epic' }] })
  const route = routeSelectedIssue({ parent, children: [issue('ABC-2')], config: CONFIG })
  assert.equal(route.verdict, 'epic-noop')
  assert.match(route.reasons[0], /nothing to plan; label children "agent-plan"/)
})

test('routeSelectedIssue: no children is single', () => {
  assert.equal(
    routeSelectedIssue({ parent: PARENT, children: [], config: CONFIG }).verdict,
    'single',
  )
})

test('routeSelectedIssue: unlabelled children and no epic label is single', () => {
  const route = routeSelectedIssue({ parent: PARENT, children: [issue('ABC-2')], config: CONFIG })
  assert.equal(route.verdict, 'single')
})

test('routeSelectedIssue: a config that renames agentPlan is honoured', () => {
  const renamed = withTrackerDefaults({
    ...DEFAULT_CONFIG,
    trackerConfig: { linear: { labels: { agentPlan: 'needs-plan' } } },
  })
  const children = [issue('ABC-2', { labels: ['agent-plan'] })]
  assert.equal(routeSelectedIssue({ parent: PARENT, children, config: renamed }).verdict, 'single')
  const relabelled = [issue('ABC-2', { labels: ['Needs Plan'] })]
  assert.equal(
    routeSelectedIssue({ parent: PARENT, children: relabelled, config: renamed }).verdict,
    'fan-out',
  )
})

test('routeSelectedIssue: a malformed bundle throws instead of guessing', () => {
  assert.throws(
    () => routeSelectedIssue({ parent: PARENT, config: CONFIG }),
    /children must be an array/,
  )
  assert.throws(
    () => routeSelectedIssue({ parent: null, children: [], config: CONFIG }),
    /no parent issue object/,
  )
})

// --- initFanoutLedger ---------------------------------------------------------------------------

const label = ['agent-plan']

test('initFanoutLedger: unlabelled, closed and already-planned children are skipped with reasons', () => {
  const children = [
    issue('ABC-2', { labels: label }),
    issue('ABC-3'),
    issue('ABC-4', { labels: label, status: 'Done', statusType: 'completed' }),
    issue('ABC-5', { labels: label, status: 'Canceled', statusType: 'canceled' }),
    planned('ABC-6', { labels: label }),
    issue('ABC-7', { labels: ['agent-plan', 'needs-human'] }),
  ]
  const ledger = initFanoutLedger({ parent: PARENT, children, config: CONFIG })
  assert.deepEqual(ledger.order, ['ABC-2'])
  assert.deepEqual(
    ledger.skipped.map(({ id, reason }) => [id, reason]),
    [
      ['ABC-3', 'no-agent-plan-label'],
      ['ABC-4', 'done'],
      ['ABC-5', 'done'],
      ['ABC-6', 'already-planned'],
      ['ABC-7', 'needs-human'],
    ],
  )
  assert.equal(ledger.children['ABC-2'].uuid, 'uuid-ABC-2')
  assert.equal(ledger.children['ABC-2'].status, 'queued')
  assert.deepEqual(ledger.siblingIds, ['ABC-2', 'ABC-3', 'ABC-4', 'ABC-5', 'ABC-6', 'ABC-7'])
  assert.equal(ledger.parallel, 4)
})

test('initFanoutLedger: a blocker is ordered before its dependent even at lower priority', () => {
  const children = [
    issue('ABC-2', {
      labels: label,
      priority: { value: 1, name: 'Urgent' },
      relations: { blockedBy: [{ id: 'uuid-ABC-3', identifier: 'ABC-3' }] },
    }),
    issue('ABC-3', { labels: label, priority: { value: 4, name: 'Low' } }),
  ]
  const ledger = initFanoutLedger({ parent: PARENT, children, config: CONFIG })
  assert.deepEqual(ledger.order, ['ABC-3', 'ABC-2'])
  assert.deepEqual(ledger.warnings, [])
})

test('initFanoutLedger: priority, then oldest, then id; None sorts last', () => {
  const children = [
    issue('ABC-2', { labels: label, priority: { value: 0, name: 'No priority' } }),
    issue('ABC-3', { labels: label, priority: { value: 4, name: 'Low' } }),
    issue('ABC-4', { labels: label, priority: { value: 1, name: 'Urgent' } }),
    issue('ABC-5', { labels: label, priority: 3, createdAt: '2025-06-01T00:00:00Z' }),
    issue('ABC-6', { labels: label, priority: 3 }),
    issue('ABC-7', { labels: label, priority: 3 }),
  ]
  const ledger = initFanoutLedger({ parent: PARENT, children, config: CONFIG })
  assert.deepEqual(ledger.order, ['ABC-4', 'ABC-5', 'ABC-6', 'ABC-7', 'ABC-3', 'ABC-2'])
})

test('initFanoutLedger: earlierSiblings is exactly the prefix of the order', () => {
  const children = ['ABC-2', 'ABC-3', 'ABC-4'].map((id) => issue(id, { labels: label }))
  const ledger = initFanoutLedger({ parent: PARENT, children, config: CONFIG })
  ledger.order.forEach((id, index) => {
    assert.deepEqual(ledger.children[id].earlierSiblings, ledger.order.slice(0, index))
  })
})

test('initFanoutLedger: a blocker outside the eligible set never stalls the order', () => {
  const children = [
    issue('ABC-2', { labels: label, relations: { blockedBy: ['ABC-3', 'XYZ-9'] } }),
    issue('ABC-3', { status: 'Done', statusType: 'completed' }),
  ]
  const ledger = initFanoutLedger({ parent: PARENT, children, config: CONFIG })
  assert.deepEqual(ledger.order, ['ABC-2'])
})

test('initFanoutLedger: an existing cycle yields priority order plus a warning, not a throw', () => {
  const children = [
    issue('ABC-2', { labels: label, priority: 4, relations: { blockedBy: ['ABC-3'] } }),
    issue('ABC-3', { labels: label, priority: 2, relations: { blockedBy: ['ABC-2'] } }),
    issue('ABC-4', { labels: label, priority: 1 }),
  ]
  const ledger = initFanoutLedger({ parent: PARENT, children, config: CONFIG })
  assert.deepEqual(ledger.order, ['ABC-4', 'ABC-3', 'ABC-2'])
  assert.equal(ledger.warnings.length, 1)
  assert.match(ledger.warnings[0], /cycle .*ABC-2, ABC-3.*priority order/)
})

test('initFanoutLedger: an out-of-range width throws', () => {
  assert.throws(
    () => initFanoutLedger({ parent: PARENT, children: [], config: CONFIG, parallel: 9 }),
    /parallel must be an integer between 1 and 8/,
  )
})

// --- nextFanoutStep -----------------------------------------------------------------------------

const ledgerOf = (count, parallel = 4) =>
  initFanoutLedger({
    parent: PARENT,
    children: Array.from({ length: count }, (_, i) => issue(`ABC-${i + 2}`, { labels: label })),
    config: CONFIG,
    parallel,
  })

const dispatchAll = (ledger, ids, now) =>
  ids.reduce(
    (current, id, index) =>
      recordChildOutcome(current, id, 'dispatched', { scratch: `s-${id}`, now: now + index })
        .ledger,
    ledger,
  )

test('nextFanoutStep: six queued at width 4 launches four', () => {
  const { step } = nextFanoutStep(ledgerOf(6), { now: 1 })
  assert.deepEqual(step.launch, ['ABC-2', 'ABC-3', 'ABC-4', 'ABC-5'])
  assert.equal(step.poll, null)
  assert.equal(step.complete, false)
})

test('nextFanoutStep: four in flight launches nothing and polls the oldest-polled, round-robin', () => {
  let ledger = ledgerOf(6)
  ledger = dispatchAll(ledger, ['ABC-2', 'ABC-3', 'ABC-4', 'ABC-5'], 100)
  const seen = []
  for (let i = 0; i < 5; i += 1) {
    const result = nextFanoutStep(ledger, { now: 1000 + i })
    assert.deepEqual(result.step.launch, [])
    seen.push(result.step.poll)
    ledger = result.ledger
  }
  assert.deepEqual(seen, ['ABC-2', 'ABC-3', 'ABC-4', 'ABC-5', 'ABC-2'])
})

test('nextFanoutStep: one planned frees exactly one slot', () => {
  let ledger = dispatchAll(ledgerOf(6), ['ABC-2', 'ABC-3', 'ABC-4', 'ABC-5'], 100)
  ledger = recordChildOutcome(ledger, 'ABC-3', 'planned').ledger
  const { step } = nextFanoutStep(ledger, { now: 2000 })
  assert.deepEqual(step.launch, ['ABC-6'])
})

test('nextFanoutStep: complete only when nothing is queued or in flight', () => {
  let ledger = ledgerOf(2)
  assert.equal(nextFanoutStep(ledger).step.complete, false)
  ledger = dispatchAll(ledger, ['ABC-2', 'ABC-3'], 1)
  assert.equal(nextFanoutStep(ledger).step.complete, false)
  ledger = recordChildOutcome(ledger, 'ABC-2', 'planned').ledger
  assert.equal(nextFanoutStep(ledger).step.complete, false)
  ledger = recordChildOutcome(ledger, 'ABC-3', 'failed', { reason: 'not-planned' }).ledger
  const { step } = nextFanoutStep(ledger)
  assert.equal(step.complete, true)
  assert.equal(step.poll, null)
  assert.deepEqual(step.launch, [])
})

test('nextFanoutStep: an empty fan-out is complete at once', () => {
  assert.equal(nextFanoutStep(ledgerOf(0)).step.complete, true)
})

test('nextFanoutStep: parallel 1 serialises', () => {
  let ledger = ledgerOf(3, 1)
  assert.deepEqual(nextFanoutStep(ledger).step.launch, ['ABC-2'])
  ledger = dispatchAll(ledger, ['ABC-2'], 1)
  assert.deepEqual(nextFanoutStep(ledger).step.launch, [])
  ledger = recordChildOutcome(ledger, 'ABC-2', 'planned').ledger
  assert.deepEqual(nextFanoutStep(ledger).step.launch, ['ABC-3'])
})

test('nextFanoutStep: does not mutate its input', () => {
  const ledger = dispatchAll(ledgerOf(2), ['ABC-2'], 1)
  const before = JSON.stringify(ledger)
  nextFanoutStep(ledger, { now: 5 })
  assert.equal(JSON.stringify(ledger), before)
})

// --- recordChildOutcome -------------------------------------------------------------------------

test('recordChildOutcome: illegal transitions throw', () => {
  let ledger = ledgerOf(2)
  assert.throws(
    () => recordChildOutcome(ledger, 'ABC-2', 'planned'),
    /illegal transition.*queued → planned/,
  )
  ledger = dispatchAll(ledger, ['ABC-2'], 1)
  assert.throws(() => recordChildOutcome(ledger, 'ABC-2', 'dispatched'), /dispatched → dispatched/)
  ledger = recordChildOutcome(ledger, 'ABC-2', 'planned').ledger
  assert.throws(() => recordChildOutcome(ledger, 'ABC-2', 'dispatched'), /planned → dispatched/)
  assert.throws(() => recordChildOutcome(ledger, 'ABC-2', 'failed'), /planned → failed/)
  assert.throws(() => recordChildOutcome(ledger, 'ABC-9', 'dispatched'), /not in this ledger/)
  assert.throws(() => recordChildOutcome(ledger, 'ABC-3', 'bogus'), /unknown outcome/)
})

test('recordChildOutcome: a skipped child cannot be recorded', () => {
  const ledger = initFanoutLedger({ parent: PARENT, children: [issue('ABC-2')], config: CONFIG })
  assert.throws(
    () => recordChildOutcome(ledger, 'ABC-2', 'dispatched'),
    /was skipped \(no-agent-plan-label\)/,
  )
})

test('recordChildOutcome: a first transport death re-queues; the second fails with attempts 2', () => {
  let ledger = dispatchAll(ledgerOf(1), ['ABC-2'], 1)
  let result = recordChildOutcome(ledger, 'ABC-2', 'failed', { reason: 'transport' })
  assert.equal(result.child.status, 'queued')
  assert.equal(result.child.attempts, 1)
  assert.equal(result.child.scratch, 's-ABC-2', 'the retry keeps the same scratch')
  ledger = result.ledger
  assert.deepEqual(nextFanoutStep(ledger).step.launch, ['ABC-2'])
  ledger = recordChildOutcome(ledger, 'ABC-2', 'dispatched', { now: 9 }).ledger
  assert.equal(ledger.children['ABC-2'].scratch, 's-ABC-2')
  result = recordChildOutcome(ledger, 'ABC-2', 'failed', { reason: 'transport' })
  assert.equal(result.child.status, 'failed')
  assert.equal(result.child.attempts, MAX_DISPATCH_ATTEMPTS)
  assert.equal(result.child.attempts, 2)
  assert.equal(result.child.reason, 'transport')
})

test('recordChildOutcome: a non-transport failure fails at once', () => {
  const ledger = dispatchAll(ledgerOf(1), ['ABC-2'], 1)
  const { child } = recordChildOutcome(ledger, 'ABC-2', 'failed', {
    reason: 'plan-attachment-missing',
  })
  assert.equal(child.status, 'failed')
  assert.equal(child.attempts, 1)
})

test('writeLedgerFile: a thrown serialisation leaves the existing file untouched and no temp', () => {
  const dir = mkdtempSync(join(tmpdir(), 'fanout-atomic-'))
  const file = join(dir, 'ABC-1.fanout-ledger.json')
  writeLedgerFile(file, ledgerOf(1))
  const before = readFileSync(file, 'utf8')
  const circular = ledgerOf(1)
  circular.self = circular
  assert.throws(() => writeLedgerFile(file, circular), /circular/i)
  assert.throws(() => writeLedgerFile(file, { ...ledgerOf(1), big: 1n }), /BigInt/)
  assert.equal(readFileSync(file, 'utf8'), before)
  assert.deepEqual(readdirSync(dir), ['ABC-1.fanout-ledger.json'])
})

// --- summary / parent flip ----------------------------------------------------------------------

test('fanoutSummary: rows cover planned, failed and skipped children', () => {
  let ledger = initFanoutLedger({
    parent: PARENT,
    children: [
      issue('ABC-2', { labels: label }),
      issue('ABC-3', { labels: label }),
      issue('ABC-4'),
    ],
    config: CONFIG,
  })
  ledger = dispatchAll(ledger, ['ABC-2', 'ABC-3'], 1)
  ledger = recordChildOutcome(ledger, 'ABC-2', 'planned').ledger
  ledger = recordChildOutcome(ledger, 'ABC-3', 'failed', { reason: 'description-invalid' }).ledger
  const summary = fanoutSummary(ledger)
  assert.equal(summary.complete, true)
  assert.equal(summary.ok, false)
  assert.deepEqual(
    summary.rows.map(({ id, outcome }) => [id, outcome]),
    [
      ['ABC-2', 'planned'],
      ['ABC-3', 'failed: description-invalid'],
      ['ABC-4', 'skipped: no-agent-plan-label'],
    ],
  )
})

const finishedLedger = (outcomes) => {
  let ledger = ledgerOf(outcomes.length)
  ledger = dispatchAll(ledger, ledger.order, 1)
  ledger.order.forEach((id, index) => {
    ledger = recordChildOutcome(ledger, id, outcomes[index], { reason: 'x' }).ledger
  })
  return ledger
}

test('parentFlipDecision: any failed child means no parent write', () => {
  const decision = parentFlipDecision({
    ledger: finishedLedger(['planned', 'failed']),
    parent: PARENT,
    config: CONFIG,
  })
  assert.equal(decision.flip, false)
  assert.match(decision.reasons[0], /1 child\(ren\) failed \(ABC-3\)/)
})

test('parentFlipDecision: an unfinished fan-out means no parent write', () => {
  const decision = parentFlipDecision({ ledger: ledgerOf(2), parent: PARENT, config: CONFIG })
  assert.equal(decision.flip, false)
  assert.match(decision.reasons[0], /still queued or in flight/)
})

test('parentFlipDecision: all planned adds epic, strips queue labels and moves unplanned → planned', () => {
  const parent = issue('ABC-1', {
    labels: [
      { name: 'feature' },
      { name: 'Agent-Plan' },
      { name: 'agent-friendly' },
      { name: 'needs-human' },
      { name: 'agent-build' },
    ],
  })
  const decision = parentFlipDecision({
    ledger: finishedLedger(['planned', 'planned']),
    parent,
    config: CONFIG,
  })
  assert.equal(decision.flip, true)
  assert.equal(decision.write, true)
  assert.equal(decision.id, 'ABC-1')
  assert.deepEqual(decision.labels, ['feature', 'epic'])
  assert.deepEqual(decision.added, ['epic'])
  assert.deepEqual(decision.removed, ['Agent-Plan', 'agent-friendly', 'needs-human', 'agent-build'])
  assert.equal(decision.state, 'Todo')
})

test('parentFlipDecision: label and state names come from config', () => {
  const renamed = withTrackerDefaults({
    ...DEFAULT_CONFIG,
    trackerConfig: {
      linear: {
        labels: { epic: 'Initiative', agentPlan: 'needs-plan' },
        states: { unplanned: 'Inbox', planned: 'Ready' },
      },
    },
  })
  const parent = issue('ABC-1', { status: 'Inbox', labels: ['needs-plan', 'agent-plan'] })
  const decision = parentFlipDecision({
    ledger: finishedLedger(['planned']),
    parent,
    config: renamed,
  })
  assert.deepEqual(decision.labels, ['agent-plan', 'Initiative'])
  assert.equal(decision.state, 'Ready')
})

test('parentFlipDecision: a parent already in another state keeps it', () => {
  const parent = issue('ABC-1', { status: 'In Progress', labels: ['epic'] })
  const decision = parentFlipDecision({
    ledger: finishedLedger(['planned']),
    parent,
    config: CONFIG,
  })
  assert.equal(decision.flip, true)
  assert.equal(decision.state, null)
  assert.deepEqual(decision.labels, ['epic'])
  assert.equal(decision.write, false, 'nothing to change means no write')
})

test('parentFlipDecision: a payload for a different issue throws', () => {
  assert.throws(
    () =>
      parentFlipDecision({
        ledger: finishedLedger(['planned']),
        parent: issue('ABC-99'),
        config: CONFIG,
      }),
    /not the ledger parent ABC-1/,
  )
})

// --- CLI ----------------------------------------------------------------------------------------

const runCliIn = (cwd, args) => {
  const stdout = execFileSync(process.execPath, [CLI, ...args], { cwd, encoding: 'utf8' })
  const lines = stdout.trim().split('\n')
  assert.equal(lines.length, 1, `one JSON object on stdout, got: ${stdout}`)
  return JSON.parse(lines[0])
}

test('CLI: every verb round-trips through files and prints one JSON object', () => {
  const dir = mkdtempSync(join(tmpdir(), 'fanout-cli-'))
  const bundle = join(dir, 'ABC-1.fanout-route.json')
  const ledger = join(dir, 'ABC-1.fanout-ledger.json')
  const parentFile = join(dir, 'parent.json')
  writeFileSync(
    bundle,
    JSON.stringify({
      parent: PARENT,
      children: [
        issue('ABC-2', { labels: label, relations: { blockedBy: ['ABC-3'] } }),
        issue('ABC-3', { labels: label }),
        issue('ABC-4'),
      ],
    }),
  )
  writeFileSync(parentFile, JSON.stringify(PARENT))

  assert.equal(runCliIn(dir, ['args', '--', 'ABC-1', '--parallel', '3']).parallel, 3)
  assert.equal(runCliIn(dir, ['route', bundle]).verdict, 'fan-out')
  const init = runCliIn(dir, ['init', bundle, ledger, '--parallel', '1'])
  assert.deepEqual(init.order, ['ABC-3', 'ABC-2'])
  assert.deepEqual(
    init.skipped.map((row) => row.reason),
    ['no-agent-plan-label'],
  )
  assert.ok(existsSync(ledger))

  assert.deepEqual(runCliIn(dir, ['next', ledger]).launch, ['ABC-3'])
  assert.equal(
    runCliIn(dir, ['record', ledger, 'ABC-3', 'dispatched', '--scratch', 's3']).status,
    'dispatched',
  )
  assert.equal(runCliIn(dir, ['next', ledger]).poll, 'ABC-3')
  assert.equal(runCliIn(dir, ['record', ledger, 'ABC-3', 'planned']).status, 'planned')
  assert.deepEqual(runCliIn(dir, ['next', ledger]).launch, ['ABC-2'])
  runCliIn(dir, ['record', ledger, 'ABC-2', 'dispatched'])
  runCliIn(dir, ['record', ledger, 'ABC-2', 'planned'])
  assert.equal(runCliIn(dir, ['next', ledger]).complete, true)
  assert.equal(runCliIn(dir, ['summary', ledger]).ok, true)
  const flip = runCliIn(dir, ['parent-flip', ledger, parentFile])
  assert.equal(flip.flip, true)
  assert.deepEqual(flip.labels, ['epic'])
  assert.equal(flip.state, 'Todo')
})

test('CLI: bad input exits 1 naming it; a bad verb exits 2', () => {
  const dir = mkdtempSync(join(tmpdir(), 'fanout-cli-bad-'))
  const ledger = join(dir, 'ledger.json')
  writeFileSync(ledger, JSON.stringify(ledgerOf(1)))
  const run = (args) => {
    try {
      execFileSync(process.execPath, [CLI, ...args], { cwd: dir, encoding: 'utf8', stdio: 'pipe' })
      return { status: 0, stderr: '' }
    } catch (error) {
      return { status: error.status, stderr: String(error.stderr) }
    }
  }
  const illegal = run(['record', ledger, 'ABC-2', 'planned'])
  assert.equal(illegal.status, 1)
  assert.match(illegal.stderr, /illegal transition/)
  assert.equal(run(['record', ledger, 'ABC-2', 'planned']).status, 1)
  assert.equal(run(['route', join(dir, 'missing.json')]).status, 1)
  assert.equal(run(['args', '--parallel', '9']).status, 1)
  assert.equal(run(['frobnicate']).status, 2)
  assert.equal(run([]).status, 2)
})

test('parentFlipDecision: nothing planned now or earlier leaves the parent untouched', () => {
  const children = [issue('ABC-2', { labels: ['agent-plan', 'needs-human'] })]
  const ledger = initFanoutLedger({ parent: PARENT, children, config: CONFIG })
  assert.deepEqual(ledger.order, [])
  const decision = parentFlipDecision({ ledger, parent: PARENT, config: CONFIG })
  assert.equal(decision.flip, false)
  assert.match(decision.reasons[0], /no child was planned/)
})

test('initFanoutLedger: already-planned siblings are permitted edge targets', () => {
  const children = [issue('ABC-2', { labels: label }), planned('ABC-3', { labels: label })]
  const ledger = initFanoutLedger({ parent: PARENT, children, config: CONFIG })
  assert.deepEqual(ledger.children['ABC-2'].plannedSiblings, ['ABC-3'])
})

test('parentFlipDecision: a resume whose children were all planned earlier still flips', () => {
  const children = [planned('ABC-2', { labels: label })]
  const ledger = initFanoutLedger({ parent: PARENT, children, config: CONFIG })
  assert.deepEqual(ledger.order, [])
  assert.equal(parentFlipDecision({ ledger, parent: PARENT, config: CONFIG }).flip, true)
})
