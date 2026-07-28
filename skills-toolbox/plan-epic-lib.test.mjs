// skills-toolbox/plan-epic-lib.test.mjs
// Table-driven unit tests for the epic-decomposition primitive (BOS-442).
// node builtins only (cron worktrees are dependency-free). Modelled on
// scripts/linear-deps-lib.test.mjs.

import assert from 'node:assert/strict'
import { test } from 'node:test'

import {
  EPIC_MIN_CHILDREN,
  EPIC_MAX_CHILDREN,
  CHILD_MAX_ESTIMATE,
  EPIC_LABEL,
  epicParentEstimate,
  validateDecomposition,
  validateLayering,
  assertAcyclic,
  topoOrderChildren,
  epicWiringPlan,
  stableChildKey,
  epicSpecMarker,
  parseEpicSpecMarker,
} from './plan-epic-lib.mjs'

// A minimal well-formed child.
const child = (key, over = {}) => ({
  key,
  title: `title ${key}`,
  goal: `goal ${key}`,
  keyChanges: [`services/x: ${key}`],
  blockedByKeys: [],
  estimate: 3,
  priority: 2,
  ...over,
})

// A well-formed spec with `n` linearly-ordered children (c2 blocked by c1, ...).
const linearSpec = (n) => ({
  parent: {
    title: 'Epic parent',
    goal: 'Ship the big thing',
    keyChanges: ['services/x'],
    priority: 2,
  },
  children: Array.from({ length: n }, (_, i) =>
    child(`c${i + 1}`, { blockedByKeys: i === 0 ? [] : [`c${i}`] }),
  ),
})

// ---------------------------------------------------------------------------
// Guard constants
// ---------------------------------------------------------------------------

test('guard constants are 2, 12, and a single-PR child ceiling of 3', () => {
  assert.equal(EPIC_MIN_CHILDREN, 2)
  assert.equal(EPIC_MAX_CHILDREN, 12)
  assert.equal(CHILD_MAX_ESTIMATE, 3)
})

test('EPIC_LABEL identifies the epic label role', () => {
  assert.equal(EPIC_LABEL, 'epic')
})

test('epicParentEstimate sums every child estimate', () => {
  assert.equal(epicParentEstimate(linearSpec(2)), 6)
  assert.equal(
    epicParentEstimate({
      children: [child('small', { estimate: 1 }), child('large', { estimate: 8 })],
    }),
    9,
  )
})

// ---------------------------------------------------------------------------
// validateDecomposition — structured { ok, errors } result
// ---------------------------------------------------------------------------

test('validateDecomposition: a well-formed 2-child epic is ok with no errors', () => {
  const res = validateDecomposition(linearSpec(2))
  assert.deepEqual(res, { ok: true, errors: [] })
})

test('validateDecomposition: a parent priority must be a planned priority', () => {
  const missing = linearSpec(2)
  delete missing.parent.priority
  const invalid = linearSpec(2)
  invalid.parent.priority = 0
  assert.match(
    validateDecomposition(missing).errors.join('\n'),
    /parent overview has an invalid priority/,
  )
  assert.match(
    validateDecomposition(invalid).errors.join('\n'),
    /parent overview has an invalid priority/,
  )
})

test('validateDecomposition: the max-size epic is accepted', () => {
  const res = validateDecomposition(linearSpec(EPIC_MAX_CHILDREN))
  assert.equal(res.ok, true)
})

test('validateDecomposition: fewer than MIN children is rejected', () => {
  const res = validateDecomposition({
    parent: { title: 't', goal: 'g', priority: 2 },
    children: [child('c1')],
  })
  assert.equal(res.ok, false)
  assert.match(res.errors.join('\n'), /at least 2 children/)
})

test('validateDecomposition: more than MAX children is rejected', () => {
  const res = validateDecomposition(linearSpec(EPIC_MAX_CHILDREN + 1))
  assert.equal(res.ok, false)
  assert.match(res.errors.join('\n'), /exceeds the 12-child cap/)
})

test('validateDecomposition: duplicate keys are rejected', () => {
  const res = validateDecomposition({
    parent: { title: 't', goal: 'g' },
    children: [child('c1'), child('c1')],
  })
  assert.equal(res.ok, false)
  assert.match(res.errors.join('\n'), /duplicate child key "c1"/)
})

test('validateDecomposition: a dangling blockedByKeys ref is rejected', () => {
  const res = validateDecomposition({
    parent: { title: 't', goal: 'g' },
    children: [child('c1'), child('c2', { blockedByKeys: ['nope'] })],
  })
  assert.equal(res.ok, false)
  assert.match(res.errors.join('\n'), /unknown key "nope"/)
})

test('validateDecomposition: a non-array blockedByKeys is rejected (not silently coerced)', () => {
  const res = validateDecomposition({
    parent: { title: 't', goal: 'g' },
    children: [child('c1'), child('c2', { blockedByKeys: 'c1' })],
  })
  assert.equal(res.ok, false)
  assert.match(res.errors.join('\n'), /non-array blockedByKeys/)
})

test('validateDecomposition: a child using the reserved key "parent" is rejected', () => {
  const res = validateDecomposition({
    parent: { title: 't', goal: 'g' },
    children: [child('parent'), child('c2')],
  })
  assert.equal(res.ok, false)
  assert.match(res.errors.join('\n'), /reserved key "parent"/)
})

test('validateDecomposition: empty title or goal is rejected', () => {
  const res = validateDecomposition({
    parent: { title: 't', goal: 'g' },
    children: [child('c1', { title: '  ' }), child('c2', { goal: '' })],
  })
  assert.equal(res.ok, false)
  assert.match(res.errors.join('\n'), /child "c1" is missing a title/)
  assert.match(res.errors.join('\n'), /child "c2" is missing a goal/)
})

test('validateDecomposition: missing or empty keyChanges is rejected', () => {
  const res = validateDecomposition({
    parent: { title: 't', goal: 'g', keyChanges: ['x'], priority: 2 },
    children: [child('c1', { keyChanges: [] }), child('c2', { keyChanges: ['  '] })],
  })
  assert.equal(res.ok, false)
  assert.match(res.errors.join('\n'), /child "c1" needs a non-empty keyChanges array/)
  assert.match(res.errors.join('\n'), /child "c2" needs a non-empty keyChanges array/)
})

test('validateDecomposition: an out-of-range estimate is rejected', () => {
  const res = validateDecomposition({
    parent: { title: 't', goal: 'g' },
    children: [child('c1', { estimate: 4 }), child('c2', { estimate: 7 })],
  })
  assert.equal(res.ok, false)
  assert.match(res.errors.join('\n'), /child "c1" has an invalid estimate/)
  assert.match(res.errors.join('\n'), /child "c2" has an invalid estimate/)
})

test('validateDecomposition: a child estimate above the single-PR ceiling (5 or 8) is rejected', () => {
  // The forcing function: a Fibonacci-valid but oversized child (5/8) must be
  // decomposed further, never carried as one epic child — this is what prevents
  // the monolith-as-one-child failure mode.
  const res = validateDecomposition({
    parent: { title: 't', goal: 'g', priority: 2 },
    children: [child('c1', { estimate: 5 }), child('c2', { estimate: 8 })],
  })
  assert.equal(res.ok, false)
  assert.match(res.errors.join('\n'), /child "c1" has estimate 5, above the single-PR ceiling of 3/)
  assert.match(res.errors.join('\n'), /child "c2" has estimate 8, above the single-PR ceiling of 3/)
})

test('validateDecomposition: children at or below the ceiling (0..3) are accepted', () => {
  const res = validateDecomposition({
    parent: { title: 't', goal: 'g', keyChanges: ['x'], priority: 2 },
    children: [
      child('c1', { estimate: 0 }),
      child('c2', { estimate: 1 }),
      child('c3', { estimate: 2 }),
      child('c4', { estimate: 3 }),
    ],
  })
  assert.deepEqual(res, { ok: true, errors: [] })
})

test('validateDecomposition: an unknown layer is rejected; a known layer (or omitted) is accepted', () => {
  const bad = validateDecomposition({
    parent: { title: 't', goal: 'g', priority: 2 },
    children: [child('c1', { layer: 'frontend' }), child('c2')],
  })
  assert.equal(bad.ok, false)
  assert.match(bad.errors.join('\n'), /child "c1" has an unknown layer "frontend"/)

  const good = validateDecomposition({
    parent: { title: 't', goal: 'g', keyChanges: ['x'], priority: 2 },
    children: [
      child('c1', { layer: 'producer' }),
      child('c2', { layer: 'read', blockedByKeys: ['c1'] }),
    ],
  })
  assert.deepEqual(good, { ok: true, errors: [] })
})

test('validateDecomposition: an out-of-range priority is rejected', () => {
  const res = validateDecomposition({
    parent: { title: 't', goal: 'g' },
    children: [child('c1', { priority: 0 }), child('c2', { priority: 5 })],
  })
  assert.equal(res.ok, false)
  assert.match(res.errors.join('\n'), /child "c1" has an invalid priority/)
  assert.match(res.errors.join('\n'), /child "c2" has an invalid priority/)
})

test('validateDecomposition: a non-boolean agentFriendly is rejected (not coerced true)', () => {
  // A drafted spec can carry the string "false"; epicSpecMarker persists
  // `agentFriendly !== false`, so "false" would be coerced to agent-friendly and
  // let resume stamp an otherwise needs-human child boss-build-eligible. Reject it.
  const res = validateDecomposition({
    parent: { title: 't', goal: 'g' },
    children: [child('c1', { agentFriendly: 'false' }), child('c2', { agentFriendly: 1 })],
  })
  assert.equal(res.ok, false)
  assert.match(res.errors.join('\n'), /child "c1" has a non-boolean agentFriendly/)
  assert.match(res.errors.join('\n'), /child "c2" has a non-boolean agentFriendly/)
})

test('validateDecomposition: a real boolean agentFriendly (or omitted) is accepted', () => {
  const res = validateDecomposition({
    parent: { title: 't', goal: 'g', priority: 2 },
    children: [child('c1', { agentFriendly: false }), child('c2', { agentFriendly: true })],
  })
  assert.deepEqual(res, { ok: true, errors: [] })
  // Omitted entirely (the linearSpec children carry no agentFriendly) also passes.
  assert.equal(validateDecomposition(linearSpec(2)).ok, true)
})

test('validateDecomposition: a fully-valid spec with metadata is ok', () => {
  const res = validateDecomposition({
    parent: { title: 't', goal: 'g', keyChanges: ['x'], priority: 2 },
    children: [
      child('c1', { keyChanges: ['services/a'], estimate: 0, priority: 4 }),
      child('c2', { keyChanges: ['services/b'], estimate: 3, priority: 1 }),
    ],
  })
  assert.deepEqual(res, { ok: true, errors: [] })
})

test('validateDecomposition: a missing parent overview is rejected', () => {
  const res = validateDecomposition({ children: [child('c1'), child('c2')] })
  assert.equal(res.ok, false)
  assert.match(res.errors.join('\n'), /missing parent overview/)
})

test('validateDecomposition: a parent missing title/goal is rejected', () => {
  const res = validateDecomposition({
    parent: { title: 'only title' },
    children: [child('c1'), child('c2')],
  })
  assert.equal(res.ok, false)
  assert.match(res.errors.join('\n'), /parent overview is missing a goal/)
})

test('validateDecomposition: a non-object spec is rejected without throwing', () => {
  assert.equal(validateDecomposition(null).ok, false)
  assert.equal(validateDecomposition(undefined).ok, false)
  assert.equal(validateDecomposition('x').ok, false)
})

// ---------------------------------------------------------------------------
// validateLayering — producer-before-consumer SOFT check (warnings, not errors)
// ---------------------------------------------------------------------------

test('validateLayering: a read/ui child gated on its producer sibling yields no warnings', () => {
  const spec = {
    parent: { title: 't', goal: 'g', priority: 2 },
    children: [
      child('write', { layer: 'producer' }),
      child('api', { layer: 'read', blockedByKeys: ['write'] }),
      child('ui', { layer: 'ui', blockedByKeys: ['write'] }),
    ],
  }
  assert.deepEqual(validateLayering(spec), { warnings: [] })
})

test('validateLayering: a read/ui child NOT blockedBy the producer warns (but never errors)', () => {
  const spec = {
    parent: { title: 't', goal: 'g', priority: 2 },
    children: [child('write', { layer: 'producer' }), child('api', { layer: 'read' })],
  }
  const { warnings } = validateLayering(spec)
  assert.equal(warnings.length, 1)
  assert.match(warnings[0], /child "api" is a read layer not blockedBy any producer sibling/)
  // It is advisory only — decomposition itself still validates.
  assert.equal(validateDecomposition(spec).ok, true)
})

test('validateLayering: a read/ui child with NO producer sibling warns to confirm the upstream', () => {
  const spec = {
    parent: { title: 't', goal: 'g', priority: 2 },
    children: [
      child('api', { layer: 'read' }),
      child('ui', { layer: 'ui', blockedByKeys: ['api'] }),
    ],
  }
  const { warnings } = validateLayering(spec)
  assert.equal(warnings.length, 2)
  assert.match(warnings.join('\n'), /child "api" is a read layer with no producer sibling/)
  assert.match(warnings.join('\n'), /child "ui" is a ui layer with no producer sibling/)
})

test('validateLayering: children without a layer tag are ignored (non-pipeline features opt out)', () => {
  assert.deepEqual(validateLayering(linearSpec(3)), { warnings: [] })
  assert.deepEqual(validateLayering(null), { warnings: [] })
})

// ---------------------------------------------------------------------------
// assertAcyclic — throws naming the cycle path
// ---------------------------------------------------------------------------

test('assertAcyclic: an acyclic spec does not throw', () => {
  assert.doesNotThrow(() => assertAcyclic(linearSpec(4)))
})

test('assertAcyclic: a 2-cycle is rejected', () => {
  const spec = {
    parent: { title: 't', goal: 'g' },
    children: [child('c1', { blockedByKeys: ['c2'] }), child('c2', { blockedByKeys: ['c1'] })],
  }
  assert.throws(() => assertAcyclic(spec), /cycle: /)
})

test('assertAcyclic: a self-loop is rejected', () => {
  const spec = {
    parent: { title: 't', goal: 'g' },
    children: [child('c1', { blockedByKeys: ['c1'] }), child('c2')],
  }
  assert.throws(() => assertAcyclic(spec), /cycle: c1 -> c1/)
})

test('assertAcyclic: a longer cycle names the path', () => {
  const spec = {
    parent: { title: 't', goal: 'g' },
    children: [
      child('c1', { blockedByKeys: ['c3'] }),
      child('c2', { blockedByKeys: ['c1'] }),
      child('c3', { blockedByKeys: ['c2'] }),
    ],
  }
  assert.throws(
    () => assertAcyclic(spec),
    /c1 -> c3 -> c2 -> c1|c2 -> c1 -> c3 -> c2|c3 -> c2 -> c1 -> c3/,
  )
})

// ---------------------------------------------------------------------------
// topoOrderChildren — stable topological order
// ---------------------------------------------------------------------------

test('topoOrderChildren: a linear chain emits in dependency order', () => {
  const order = topoOrderChildren(linearSpec(4)).map((c) => c.key)
  assert.deepEqual(order, ['c1', 'c2', 'c3', 'c4'])
})

test('topoOrderChildren: blockers always precede the blocked child', () => {
  // c3 depends on c1; c2 independent. Declared order c3,c2,c1 — but c1 must precede c3.
  const spec = {
    parent: { title: 't', goal: 'g' },
    children: [child('c3', { blockedByKeys: ['c1'] }), child('c2'), child('c1')],
  }
  const order = topoOrderChildren(spec).map((c) => c.key)
  assert.ok(order.indexOf('c1') < order.indexOf('c3'), 'c1 must precede c3')
  // Deterministic + stable: same spec, same order every call.
  assert.deepEqual(
    topoOrderChildren(spec).map((c) => c.key),
    order,
  )
})

test('topoOrderChildren: throws on a cycle', () => {
  const spec = {
    parent: { title: 't', goal: 'g' },
    children: [child('c1', { blockedByKeys: ['c2'] }), child('c2', { blockedByKeys: ['c1'] })],
  }
  assert.throws(() => topoOrderChildren(spec), /cycle/)
})

test('topoOrderChildren: returns full child objects, not just keys', () => {
  const first = topoOrderChildren(linearSpec(2))[0]
  assert.equal(first.key, 'c1')
  assert.equal(first.title, 'title c1')
  assert.equal(first.estimate, 3)
})

// ---------------------------------------------------------------------------
// epicWiringPlan — deterministic tracker-write list
// ---------------------------------------------------------------------------

test('epicWiringPlan: emits parentId + resolved blockedBy per child, in topo order', () => {
  const spec = linearSpec(3) // c1 <- c2 <- c3
  const createdIdByKey = { parent: 'BOS-42', c1: 'BOS-101', c2: 'BOS-102', c3: 'BOS-103' }
  const plan = epicWiringPlan(spec, createdIdByKey)
  assert.deepEqual(plan, [
    { key: 'c1', childId: 'BOS-101', parentId: 'BOS-42', blockedBy: [] },
    { key: 'c2', childId: 'BOS-102', parentId: 'BOS-42', blockedBy: ['BOS-101'] },
    { key: 'c3', childId: 'BOS-103', parentId: 'BOS-42', blockedBy: ['BOS-102'] },
  ])
})

test('topoOrderChildren + epicWiringPlan: a diamond resolves the shared blocker first and fans out multi-id blockedBy', () => {
  // Diamond: c1 <- {c2, c3} <- c4 (c4 blocked by BOTH c2 and c3).
  const spec = {
    parent: { title: 't', goal: 'g' },
    children: [
      child('c1'),
      child('c2', { blockedByKeys: ['c1'] }),
      child('c3', { blockedByKeys: ['c1'] }),
      child('c4', { blockedByKeys: ['c2', 'c3'] }),
    ],
  }
  const order = topoOrderChildren(spec).map((c) => c.key)
  // Shared blocker precedes both dependents; the fan-in child comes last.
  assert.ok(order.indexOf('c1') < order.indexOf('c2'), 'c1 precedes c2')
  assert.ok(order.indexOf('c1') < order.indexOf('c3'), 'c1 precedes c3')
  assert.ok(order.indexOf('c2') < order.indexOf('c4'), 'c2 precedes c4')
  assert.ok(order.indexOf('c3') < order.indexOf('c4'), 'c3 precedes c4')
  // Deterministic across calls.
  assert.deepEqual(
    topoOrderChildren(spec).map((c) => c.key),
    order,
  )
  // epicWiringPlan resolves the fan-in child's TWO blockers to both ids.
  const ids = { parent: 'BOS-1', c1: 'BOS-10', c2: 'BOS-20', c3: 'BOS-30', c4: 'BOS-40' }
  const plan = epicWiringPlan(spec, ids)
  const c4 = plan.find((e) => e.key === 'c4')
  assert.deepEqual(c4.blockedBy, ['BOS-20', 'BOS-30'])
})

test('epicWiringPlan: throws when the parent id is missing', () => {
  const spec = linearSpec(2)
  assert.throws(() => epicWiringPlan(spec, { c1: 'BOS-1', c2: 'BOS-2' }), /createdIdByKey\.parent/)
})

test('epicWiringPlan: throws when a child id is missing', () => {
  const spec = linearSpec(2)
  assert.throws(
    () => epicWiringPlan(spec, { parent: 'BOS-42', c1: 'BOS-1' }),
    /no created id for child "c2"/,
  )
})

test('epicWiringPlan: throws when a blocker id is missing from the map', () => {
  // c2 blocked by c1, but c1 has no id (pathological — wiring runs post-creation).
  const spec = {
    parent: { title: 't', goal: 'g' },
    children: [child('c1'), child('c2', { blockedByKeys: ['c1'] })],
  }
  // Give c2 an id but not c1: topo puts c1 first so it trips on the missing child id.
  assert.throws(
    () => epicWiringPlan(spec, { parent: 'BOS-42', c2: 'BOS-2' }),
    /no created id for child "c1"/,
  )
})

// ---------------------------------------------------------------------------
// stableChildKey — deterministic title-derived key with collision dedupe
// ---------------------------------------------------------------------------

test('stableChildKey: derives a deterministic slug from the title', () => {
  assert.equal(stableChildKey({ title: 'Add the API surface' }), 'add-the-api-surface')
  // Same title always yields the same key (survives a fresh worktree).
  assert.equal(stableChildKey({ title: 'Add the API surface' }), 'add-the-api-surface')
})

test('stableChildKey: strips punctuation and collapses separators', () => {
  assert.equal(
    stableChildKey({ title: '  Wire up bossd/plugin (v2)!  ' }),
    'wire-up-bossd-plugin-v2',
  )
})

test('stableChildKey: dedupes collisions deterministically with -2/-3 suffixes', () => {
  const seen = new Set()
  assert.equal(stableChildKey({ title: 'Same title' }, seen), 'same-title')
  assert.equal(stableChildKey({ title: 'Same title' }, seen), 'same-title-2')
  assert.equal(stableChildKey({ title: 'Same title' }, seen), 'same-title-3')
})

test('stableChildKey: empty/symbol-only titles fall back to "child"', () => {
  const seen = new Set()
  assert.equal(stableChildKey({ title: '   ' }, seen), 'child')
  assert.equal(stableChildKey({ title: '!!!' }, seen), 'child-2')
  assert.equal(stableChildKey({}, seen), 'child-3')
})

test('stableChildKey: never returns the reserved "parent" key', () => {
  assert.equal(stableChildKey({ title: 'Parent' }), 'parent-2')
})

// ---------------------------------------------------------------------------
// epicSpecMarker / parseEpicSpecMarker — durable FULL-spec round-trip
// ---------------------------------------------------------------------------

test('epicSpecMarker + parseEpicSpecMarker: round-trips the full parent + children spec', () => {
  const spec = {
    parent: { title: 'Epic parent', goal: 'Ship it', keyChanges: ['services/x'], priority: 3 },
    children: [
      child('add-api'),
      child('wire-dag', { blockedByKeys: ['add-api'] }),
      child('ship-ui', { blockedByKeys: ['wire-dag'], estimate: 5, priority: 1 }),
    ],
  }
  const parsed = parseEpicSpecMarker(epicSpecMarker(spec))
  // The parent overview survives verbatim…
  assert.deepEqual(parsed.parent, {
    title: 'Epic parent',
    goal: 'Ship it',
    keyChanges: ['services/x'],
    priority: 3,
  })
  // …and so does every child's FULL metadata (so resume finishes the ORIGINAL
  // epic without re-decomposing).
  assert.deepEqual(
    parsed.children.map((c) => c.key),
    ['add-api', 'wire-dag', 'ship-ui'],
  )
  assert.deepEqual(parsed.children[2], {
    key: 'ship-ui',
    title: 'title ship-ui',
    goal: 'goal ship-ui',
    keyChanges: ['services/x: ship-ui'],
    blockedByKeys: ['wire-dag'],
    estimate: 5,
    priority: 1,
    // A child with no explicit agent-friendliness call defaults to true
    // (agent-friendly), matching the plan-contract convention.
    agentFriendly: true,
    // No recorded open questions ⇒ no agent-question.
    agentQuestion: false,
  })
})

test('epicSpecMarker + parseEpicSpecMarker: persists a needs-human child (agentFriendly:false) so resume re-stamps it correctly', () => {
  const spec = {
    parent: { title: 'Epic parent', goal: 'Ship it', keyChanges: ['services/x'] },
    children: [
      child('agent-child', { agentFriendly: true }),
      // A child whose plan concluded it needs a human — deferred exposure must
      // stamp `needs-human`, NEVER `agent-friendly`. The decision has to survive
      // the marker so a fresh-worktree resume (scratch gone) recovers it.
      child('human-child', { agentFriendly: false }),
    ],
  }
  const parsed = parseEpicSpecMarker(epicSpecMarker(spec))
  assert.equal(parsed.children[0].agentFriendly, true)
  assert.equal(parsed.children[1].agentFriendly, false)
})

test('epicSpecMarker + parseEpicSpecMarker: persists agentQuestion from a child plan openQuestions', () => {
  const spec = {
    parent: { title: 'Epic parent', goal: 'Ship it', keyChanges: ['services/x'] },
    children: [
      // A child whose plan recorded controversial open questions ⇒ agent-question.
      child('has-questions', { openQuestions: ['Which store backend?', 'Sync or async?'] }),
      // Empty openQuestions ⇒ no agent-question.
      child('empty-questions', { openQuestions: [] }),
      // Omitted entirely ⇒ no agent-question.
      child('no-questions'),
    ],
  }
  const parsed = parseEpicSpecMarker(epicSpecMarker(spec))
  assert.equal(parsed.children[0].agentQuestion, true)
  assert.equal(parsed.children[1].agentQuestion, false)
  assert.equal(parsed.children[2].agentQuestion, false)
  // A re-serialized recovered spec (agentQuestion boolean, no openQuestions array)
  // must round-trip the decision, so an idempotent resume re-persists it.
  const reparsed = parseEpicSpecMarker(epicSpecMarker(parsed))
  assert.equal(reparsed.children[0].agentQuestion, true)
  assert.equal(reparsed.children[1].agentQuestion, false)
})

test('parseEpicSpecMarker: an OLD marker without agentQuestion degrades to false (no agent-question)', () => {
  const legacy =
    '<!-- boss-plan-epic-spec:{"parent":{"title":"t","goal":"g","keyChanges":[]},' +
    '"children":[{"key":"c1","title":"t1","goal":"g1","keyChanges":["x"],' +
    '"blockedByKeys":[],"estimate":3,"priority":2}]} -->'
  const parsed = parseEpicSpecMarker(legacy)
  assert.ok(parsed, 'a legacy marker must still parse')
  assert.equal(parsed.children[0].agentQuestion, false)
})

test('parseEpicSpecMarker: an OLD marker without parent priority degrades to a defined priority', () => {
  const legacy =
    '<!-- boss-plan-epic-spec:{"parent":{"title":"t","goal":"g","keyChanges":[]},' +
    '"children":[{"key":"c1","title":"t1","goal":"g1","keyChanges":["x"],"blockedByKeys":[],"estimate":3,"priority":2}]} -->'
  assert.equal(parseEpicSpecMarker(legacy).parent.priority, 3)
})

test('parseEpicSpecMarker: an OLD marker without agentFriendly degrades to agent-friendly (backward-compat)', () => {
  // A marker written before the field was persisted: no agentFriendly key. It
  // must still parse and degrade to the plan-contract default (true), never null.
  const legacy =
    '<!-- boss-plan-epic-spec:{"parent":{"title":"t","goal":"g","keyChanges":[]},' +
    '"children":[{"key":"c1","title":"t1","goal":"g1","keyChanges":["x"],' +
    '"blockedByKeys":[],"estimate":3,"priority":2}]} -->'
  const parsed = parseEpicSpecMarker(legacy)
  assert.ok(parsed, 'a legacy marker must still parse')
  assert.equal(parsed.children[0].agentFriendly, true)
})

test('epicSpecMarker + parseEpicSpecMarker: round-trips a child architectural layer', () => {
  const spec = {
    parent: { title: 'Epic parent', goal: 'Ship it', keyChanges: ['x'], priority: 2 },
    children: [
      child('write', { layer: 'producer' }),
      child('api', { layer: 'read', blockedByKeys: ['write'] }),
    ],
  }
  const parsed = parseEpicSpecMarker(epicSpecMarker(spec))
  assert.equal(parsed.children[0].layer, 'producer')
  assert.equal(parsed.children[1].layer, 'read')
})

test('parseEpicSpecMarker: an OLD marker without layer parses and carries no layer key', () => {
  const legacy =
    '<!-- boss-plan-epic-spec:{"parent":{"title":"t","goal":"g","keyChanges":[],"priority":2},' +
    '"children":[{"key":"c1","title":"t1","goal":"g1","keyChanges":["x"],' +
    '"blockedByKeys":[],"estimate":3,"priority":2}]} -->'
  const parsed = parseEpicSpecMarker(legacy)
  assert.ok(parsed, 'a legacy marker must still parse')
  assert.equal('layer' in parsed.children[0], false)
})

test('epicSpecMarker + parseEpicSpecMarker: round-trips text containing an HTML-comment terminator', () => {
  // A generated title/goal/keyChange can legitimately contain `} -->` (e.g. an
  // epic about HTML-comment parsing). A raw-JSON payload would let that truncate
  // the marker so parse returns null and the durable spec is lost on resume; the
  // base64 payload must round-trip it intact.
  const spec = {
    parent: { title: 'Parse `} -->` in comments', goal: 'g', keyChanges: ['x'] },
    children: [
      child('c1', { title: 'handle a } --> sequence', goal: 'end the <!-- comment --> safely' }),
      child('c2', { blockedByKeys: ['c1'] }),
    ],
  }
  const marker = epicSpecMarker(spec)
  // The marker payload is base64 (no raw brace/terminator leaks into the comment).
  assert.match(marker, /<!-- boss-plan-epic-spec:[A-Za-z0-9+/=]+ -->/)
  const parsed = parseEpicSpecMarker(marker)
  assert.ok(parsed, 'a marker whose text contains `} -->` must still parse')
  assert.equal(parsed.parent.title, 'Parse `} -->` in comments')
  assert.equal(parsed.children[0].title, 'handle a } --> sequence')
  assert.equal(parsed.children[0].goal, 'end the <!-- comment --> safely')
  assert.deepEqual(
    parsed.children.map((c) => c.key),
    ['c1', 'c2'],
  )
})

test('parseEpicSpecMarker: finds the marker embedded in surrounding description prose', () => {
  const desc = `## Epic overview\n\nGoal etc.\n\n${epicSpecMarker({
    parent: { title: 't', goal: 'g' },
    children: [child('c1'), child('c2')],
  })}\n\n## Original notes\n`
  const parsed = parseEpicSpecMarker(desc)
  assert.deepEqual(
    parsed.children.map((c) => c.key),
    ['c1', 'c2'],
  )
})

test('parseEpicSpecMarker: returns null when the marker is absent', () => {
  assert.equal(parseEpicSpecMarker('## Just a normal description'), null)
  assert.equal(parseEpicSpecMarker(''), null)
  assert.equal(parseEpicSpecMarker(null), null)
})

test('parseEpicSpecMarker: returns null on a garbled marker (bad JSON or non-array children)', () => {
  assert.equal(parseEpicSpecMarker('<!-- boss-plan-epic-spec:{not json} -->'), null)
  assert.equal(parseEpicSpecMarker('<!-- boss-plan-epic-spec:{"children":"nope"} -->'), null)
})
