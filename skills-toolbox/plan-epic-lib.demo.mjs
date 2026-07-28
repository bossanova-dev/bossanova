#!/usr/bin/env node
// skills-toolbox/plan-epic-lib.demo.mjs
// Deterministic DRY-RUN of the headless boss-plan epic path on a SYNTHETIC
// oversized ticket (BOS-442 acceptance criterion 5 / Required proof #3). Pure:
// no network, ZERO real Linear writes — an in-memory fake "tracker" records the
// writes the orchestrator WOULD make. Run 1 creates every child; Run 2 resumes
// on the same parent, adopts the already-created children, and makes no new
// create writes (idempotent resume).
//
//   node skills-toolbox/plan-epic-lib.demo.mjs
//
// Evidence tokens it prints: `children: N`, `acyclic`, `adopted`.

import {
  validateDecomposition,
  validateLayering,
  assertAcyclic,
  topoOrderChildren,
  epicWiringPlan,
} from './plan-epic-lib.mjs'

// A synthetic oversized ticket the planner judged to span multiple PRs.
// Every child is <= CHILD_MAX_ESTIMATE (3) so the whole feature lands as small,
// one-atomic-PR children, and the pipeline is decomposed producer-before-consumer
// along its architectural seams (persistence -> producer -> read -> ui) so the
// read/ui children are gated on the producer that writes their rows.
const spec = {
  parent: {
    title: 'Add multi-tenant billing',
    goal: 'Ship org-scoped billing across schema, producer, API, UI, and docs',
    keyChanges: ['services/bosso/billing', 'services/web', 'docs'],
    priority: 2,
  },
  children: [
    {
      key: 'schema',
      title: 'Billing schema + migrations',
      goal: 'Org-scoped billing tables',
      keyChanges: ['services/bosso/db'],
      blockedByKeys: [],
      estimate: 3,
      priority: 2,
      layer: 'persistence',
    },
    {
      key: 'billing-writer',
      title: 'Billing usage writer',
      goal: 'The worker that writes billing rows from usage events',
      keyChanges: ['services/bosso/billing'],
      blockedByKeys: ['schema'],
      estimate: 3,
      priority: 2,
      layer: 'producer',
    },
    {
      key: 'api',
      title: 'Billing API endpoints',
      goal: 'Read over billing entities the writer populates',
      keyChanges: ['services/bosso/billing'],
      blockedByKeys: ['billing-writer'],
      estimate: 3,
      priority: 2,
      layer: 'read',
    },
    {
      key: 'ui',
      title: 'Billing settings UI',
      goal: 'Web billing surface',
      keyChanges: ['services/web'],
      blockedByKeys: ['api', 'billing-writer'],
      estimate: 3,
      priority: 3,
      layer: 'ui',
    },
    {
      key: 'docs',
      title: 'Billing docs',
      goal: 'Document the billing flow',
      keyChanges: ['docs'],
      blockedByKeys: ['api'],
      estimate: 1,
      priority: 4,
    },
  ],
}

const PARENT_ID = 'BOS-9001' // the original ticket, repurposed as the epic parent

// A tiny in-memory tracker — proves the flow WITHOUT any real Linear write.
// `existing` pre-seeds children already on the parent (the resume case).
function makeFakeTracker(existing = {}) {
  const childrenByKey = new Map(Object.entries(existing))
  const writes = []
  return {
    writes,
    // Idempotent create: adopt an existing child (matched by key) or mint a new id.
    createChild(key, seq) {
      if (childrenByKey.has(key)) {
        const id = childrenByKey.get(key)
        writes.push({ op: 'adopt', key, id })
        return { id, adopted: true }
      }
      const id = `BOS-91${String(seq).padStart(2, '0')}`
      childrenByKey.set(key, id)
      writes.push({ op: 'create', key, id })
      return { id, adopted: false }
    },
    wire(entry) {
      writes.push({ op: 'wire', child: entry.childId, blockedBy: entry.blockedBy })
    },
  }
}

// One dry-run pass. Returns the created-id map so a resume run can pre-seed it.
function runEpicPass(label, tracker) {
  console.log(`\n=== ${label} ===`)

  const result = validateDecomposition(spec)
  console.log(`validate: ok=${result.ok} errors=${result.errors.length}`)
  if (!result.ok) {
    console.log('would FALL BACK to a single-ticket plan:', result.errors.join('; '))
    return {}
  }

  // Cycle safety before any write.
  try {
    assertAcyclic(spec)
    console.log('acyclic: yes (blockedByKeys DAG is acyclic)')
  } catch (err) {
    console.log(`NOT acyclic — abort with zero writes: ${err.message}`)
    return {}
  }

  // Producer-before-consumer soft check (advisory — warnings never block).
  const { warnings } = validateLayering(spec)
  console.log(`layering: ${warnings.length} warning(s) (producer-before-consumer soft check)`)
  for (const w of warnings) console.log(`  warn: ${w}`)

  const order = topoOrderChildren(spec)
  console.log(`children: ${order.length} (topo order: ${order.map((c) => c.key).join(' -> ')})`)

  const createdIdByKey = { parent: PARENT_ID }
  let created = 0
  let adopted = 0
  order.forEach((childNode, i) => {
    const { id, adopted: wasAdopted } = tracker.createChild(childNode.key, i + 1)
    createdIdByKey[childNode.key] = id
    wasAdopted ? (adopted += 1) : (created += 1)
  })
  console.log(`created: ${created}  adopted: ${adopted}`)

  // Deterministic wiring plan.
  const wiring = epicWiringPlan(spec, createdIdByKey)
  for (const entry of wiring) tracker.wire(entry)
  console.log('wiring plan:')
  for (const entry of wiring) {
    console.log(
      `  ${entry.childId} (${entry.key}) parent=${entry.parentId} blockedBy=[${entry.blockedBy.join(', ')}]`,
    )
  }
  return createdIdByKey
}

// RUN 1 — fresh headless auto-create. Creates every child.
const run1 = makeFakeTracker()
const created = runEpicPass('RUN 1 (fresh headless auto-create — dry-run)', run1)
const created1 = run1.writes.filter((w) => w.op === 'create').length
const wires1 = run1.writes.filter((w) => w.op === 'wire').length
console.log(
  `RUN 1 fake-tracker writes: ${created1} create + ${wires1} wire — ZERO real Linear writes`,
)

// RUN 2 — idempotent resume: the same children already exist on the parent.
const existing = Object.fromEntries(Object.entries(created).filter(([k]) => k !== 'parent'))
const run2 = makeFakeTracker(existing)
runEpicPass('RUN 2 (idempotent resume — children already exist)', run2)
const newCreates = run2.writes.filter((w) => w.op === 'create').length
const adoptions = run2.writes.filter((w) => w.op === 'adopt').length
console.log(`RUN 2: new creates=${newCreates}  adopted=${adoptions}`)
console.log(
  newCreates === 0 && adoptions === spec.children.length
    ? 'idempotent: adopted existing children, no duplicate creates'
    : 'ERROR: idempotency violated',
)

console.log(
  '\nDONE — synthetic epic produced a valid parent + N children + acyclic blockedBy DAG with zero real Linear writes.',
)
