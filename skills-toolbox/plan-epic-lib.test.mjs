// skills-toolbox/plan-epic-lib.test.mjs
// Table-driven unit tests for the epic-decomposition primitive (BOS-442).
// node builtins only (cron worktrees are dependency-free). Modelled on
// scripts/linear-deps-lib.test.mjs.

import assert from 'node:assert/strict'
import { test } from 'node:test'

import {
  EPIC_MIN_CHILDREN,
  EPIC_MAX_CHILDREN,
  SPEC_SCHEMA_VERSION,
  SPEC_ATTACHMENT_MIME,
  CHILD_MAX_ESTIMATE,
  EPIC_LABEL,
  epicParentEstimate,
  validateDecomposition,
  validateLayering,
  assertAcyclic,
  topoOrderChildren,
  epicWiringPlan,
  stableChildKey,
  serializeEpicSpec,
  parseEpicSpec,
  specAttachmentFilename,
  specAttachmentTitle,
  validateSpecIdentity,
  epicChildMarker,
  parseEpicChildMarker,
  reconcileEpicChildren,
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
  // A drafted spec can carry the string "false"; serializeEpicSpec persists
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

test('stableChildKey: rejects non-object child inputs with a named error', () => {
  for (const value of ['title', null, 3]) {
    assert.throws(() => stableChildKey(value), /stableChildKey: child must be an object/)
  }
})

test('stableChildKey: never returns the reserved "parent" key', () => {
  assert.equal(stableChildKey({ title: 'Parent' }), 'parent-2')
})

// ---------------------------------------------------------------------------
// serializeEpicSpec / parseEpicSpec — durable FULL-spec round-trip
// ---------------------------------------------------------------------------

test('serializeEpicSpec + parseEpicSpec: round-trips the full parent + children spec', () => {
  const spec = {
    parent: { title: 'Epic parent', goal: 'Ship it', keyChanges: ['services/x'], priority: 3 },
    children: [
      child('add-api'),
      child('wire-dag', { blockedByKeys: ['add-api'] }),
      child('ship-ui', { blockedByKeys: ['wire-dag'], estimate: 5, priority: 1 }),
    ],
  }
  const parsed = parseEpicSpec(serializeEpicSpec(spec))
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

test('serializeEpicSpec + parseEpicSpec: persists a needs-human child (agentFriendly:false) so resume re-stamps it correctly', () => {
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
  const parsed = parseEpicSpec(serializeEpicSpec(spec))
  assert.equal(parsed.children[0].agentFriendly, true)
  assert.equal(parsed.children[1].agentFriendly, false)
})

test('G2 REGRESSION serializeEpicSpec: a child planMarkdown is never persisted', () => {
  // planMarkdown was the ONLY reason the spec ever approached a size bound. It is
  // no longer part of the spec shape: a caller that still hands one in must have
  // it dropped, not smuggled into the attachment.
  const planMarkdown = '# Implementation plan\n\n- restore the canonical native attachment\n'
  const spec = {
    parentId: 'BOS-651',
    parent: { title: 'Epic parent', goal: 'Ship it', keyChanges: ['services/x'], priority: 2 },
    children: [child('attachment-retry', { planMarkdown }), child('plain')],
  }
  const serialized = serializeEpicSpec(spec)
  assert.equal(serialized.includes('planMarkdown'), false, 'no planMarkdown key in the payload')
  assert.equal(serialized.includes('restore the canonical'), false, 'no plan body in the payload')
  const parsed = parseEpicSpec(serialized)
  assert.equal('planMarkdown' in parsed.children[0], false)
  assert.equal('planMarkdown' in parsed.children[1], false)
})

test('G2 REGRESSION parseEpicSpec: a LEGACY payload carrying planMarkdown is scrubbed on read', () => {
  // The serialize side cannot cover this: every REAL legacy marker predates the
  // deletion and DOES carry plan bodies — that is what the old byte cap existed
  // for. If the read-side scrub regressed, a legacy resume would hand callers a
  // stale plan body and an agent could attach it instead of taking the
  // unconditional `allowEpic:false` redraft this ticket makes the single path.
  // Every other legacy fixture is missing fields rather than carrying extra ones,
  // so without this case `delete child.planMarkdown` (and the sibling `layer`
  // scrub) are unreachable by any assertion.
  const legacy = {
    parent: { title: 'Legacy epic', goal: 'g', keyChanges: ['x'], priority: 2 },
    children: [
      {
        key: 'c1',
        title: 't1',
        goal: 'g1',
        keyChanges: ['x'],
        blockedByKeys: [],
        estimate: 2,
        priority: 3,
        layer: 'not-a-real-layer',
        planMarkdown: '# stale body\n\n- do not resurrect this\n',
      },
    ],
  }
  const encoded = Buffer.from(JSON.stringify(legacy), 'utf8').toString('base64')
  for (const [form, description] of [
    ['base64', `## Notes\n\n<!-- boss-plan-epic-spec:${encoded} -->\n`],
    ['raw JSON', `## Notes\n\n<!-- boss-plan-epic-spec:${JSON.stringify(legacy)} -->\n`],
  ]) {
    const parsed = parseEpicSpec(description)
    assert.ok(parsed, `${form} legacy marker must still parse`)
    assert.equal('planMarkdown' in parsed.children[0], false, `${form}: planMarkdown scrubbed`)
    assert.equal('layer' in parsed.children[0], false, `${form}: unknown layer dropped`)
    assert.equal(parsed.children[0].key, 'c1', `${form}: the rest of the child survives`)
  }
})

test('G2 REGRESSION serializeEpicSpec: no size-triggered truncation branch survives', () => {
  // The old emitter walked children backwards deleting plan bodies until the
  // base64 payload fit a byte cap, then threw if it still did not. Both are gone:
  // two ≥256 KiB bodies must neither throw nor cost a child, because the bodies
  // simply never enter the payload.
  const huge = 256 * 1024
  const spec = {
    parentId: 'BOS-651',
    parent: { title: 'Epic parent', goal: 'Ship it', keyChanges: ['services/x'], priority: 2 },
    children: [
      child('first', { planMarkdown: '# First\n\n' + 'a'.repeat(huge) }),
      child('second', { planMarkdown: '# Second\n\n' + 'b'.repeat(huge) }),
    ],
  }
  let serialized
  assert.doesNotThrow(() => {
    serialized = serializeEpicSpec(spec)
  })
  // Small: the two 256 KiB bodies contributed nothing at all.
  assert.ok(
    Buffer.byteLength(serialized, 'utf8') < 4096,
    `serialized spec must stay small, got ${Buffer.byteLength(serialized, 'utf8')} bytes`,
  )
  const parsed = parseEpicSpec(serialized)
  // No child was sacrificed to a size bound.
  assert.deepEqual(
    parsed.children.map((c) => c.key),
    ['first', 'second'],
  )
})

test('parseEpicSpec: an OLD marker without planMarkdown leaves the body absent for a safe redraft', () => {
  const legacy =
    '<!-- boss-plan-epic-spec:{"parent":{"title":"t","goal":"g","keyChanges":[]},' +
    '"children":[{"key":"c1","title":"t1","goal":"g1","keyChanges":["x"],' +
    '"blockedByKeys":[],"estimate":3,"priority":2}]} -->'
  const parsed = parseEpicSpec(legacy)
  assert.ok(parsed, 'a legacy marker must still parse')
  assert.equal('planMarkdown' in parsed.children[0], false)
})

test('serializeEpicSpec + parseEpicSpec: persists agentQuestion from a child plan openQuestions', () => {
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
  const parsed = parseEpicSpec(serializeEpicSpec(spec))
  assert.equal(parsed.children[0].agentQuestion, true)
  assert.equal(parsed.children[1].agentQuestion, false)
  assert.equal(parsed.children[2].agentQuestion, false)
  // A re-serialized recovered spec (agentQuestion boolean, no openQuestions array)
  // must round-trip the decision, so an idempotent resume re-persists it.
  const reparsed = parseEpicSpec(serializeEpicSpec(parsed))
  assert.equal(reparsed.children[0].agentQuestion, true)
  assert.equal(reparsed.children[1].agentQuestion, false)
})

test('parseEpicSpec: an OLD marker without agentQuestion degrades to false (no agent-question)', () => {
  const legacy =
    '<!-- boss-plan-epic-spec:{"parent":{"title":"t","goal":"g","keyChanges":[]},' +
    '"children":[{"key":"c1","title":"t1","goal":"g1","keyChanges":["x"],' +
    '"blockedByKeys":[],"estimate":3,"priority":2}]} -->'
  const parsed = parseEpicSpec(legacy)
  assert.ok(parsed, 'a legacy marker must still parse')
  assert.equal(parsed.children[0].agentQuestion, false)
})

test('parseEpicSpec: an OLD marker without parent priority degrades to a defined priority', () => {
  const legacy =
    '<!-- boss-plan-epic-spec:{"parent":{"title":"t","goal":"g","keyChanges":[]},' +
    '"children":[{"key":"c1","title":"t1","goal":"g1","keyChanges":["x"],"blockedByKeys":[],"estimate":3,"priority":2}]} -->'
  assert.equal(parseEpicSpec(legacy).parent.priority, 3)
})

test('parseEpicSpec: an OLD marker without agentFriendly degrades to agent-friendly (backward-compat)', () => {
  // A marker written before the field was persisted: no agentFriendly key. It
  // must still parse and degrade to the plan-contract default (true), never null.
  const legacy =
    '<!-- boss-plan-epic-spec:{"parent":{"title":"t","goal":"g","keyChanges":[]},' +
    '"children":[{"key":"c1","title":"t1","goal":"g1","keyChanges":["x"],' +
    '"blockedByKeys":[],"estimate":3,"priority":2}]} -->'
  const parsed = parseEpicSpec(legacy)
  assert.ok(parsed, 'a legacy marker must still parse')
  assert.equal(parsed.children[0].agentFriendly, true)
})

test('serializeEpicSpec + parseEpicSpec: round-trips a child architectural layer', () => {
  const spec = {
    parent: { title: 'Epic parent', goal: 'Ship it', keyChanges: ['x'], priority: 2 },
    children: [
      child('write', { layer: 'producer' }),
      child('api', { layer: 'read', blockedByKeys: ['write'] }),
    ],
  }
  const parsed = parseEpicSpec(serializeEpicSpec(spec))
  assert.equal(parsed.children[0].layer, 'producer')
  assert.equal(parsed.children[1].layer, 'read')
})

test('parseEpicSpec: an OLD marker without layer parses and carries no layer key', () => {
  const legacy =
    '<!-- boss-plan-epic-spec:{"parent":{"title":"t","goal":"g","keyChanges":[],"priority":2},' +
    '"children":[{"key":"c1","title":"t1","goal":"g1","keyChanges":["x"],' +
    '"blockedByKeys":[],"estimate":3,"priority":2}]} -->'
  const parsed = parseEpicSpec(legacy)
  assert.ok(parsed, 'a legacy marker must still parse')
  assert.equal('layer' in parsed.children[0], false)
})

test('serializeEpicSpec + parseEpicSpec: round-trips text containing an HTML-comment terminator', () => {
  // A generated title/goal/keyChange can legitimately contain `} -->` (e.g. an
  // epic about HTML-comment parsing). That text was the whole reason the inline
  // marker had to be base64: it could truncate the surrounding HTML comment.
  // Standing free in its own attachment there is no comment to terminate, and
  // JSON string escaping carries the bytes verbatim — but the round-trip is
  // still worth pinning, because it is the case a naive re-introduction of an
  // inline marker would break.
  const spec = {
    parent: { title: 'Parse `} -->` in comments', goal: 'g', keyChanges: ['x'], priority: 2 },
    children: [
      child('c1', { title: 'handle a } --> sequence', goal: 'end the <!-- comment --> safely' }),
      child('c2', { blockedByKeys: ['c1'] }),
    ],
  }
  const serialized = serializeEpicSpec(spec)
  const parsed = parseEpicSpec(serialized)
  assert.ok(parsed, 'a spec whose text contains `} -->` must still parse')
  assert.equal(parsed.parent.title, 'Parse `} -->` in comments')
  assert.equal(parsed.children[0].title, 'handle a } --> sequence')
  assert.equal(parsed.children[0].goal, 'end the <!-- comment --> safely')
  assert.deepEqual(
    parsed.children.map((c) => c.key),
    ['c1', 'c2'],
  )
})

test('parseEpicSpec: the plain-JSON form is the WHOLE content, not a marker hidden in prose', () => {
  // The new store is an attachment whose body IS the spec, so the JSON branch
  // parses the whole content. Prose-wrapping it is not a supported input (only
  // the two legacy INLINE markers are found embedded — see G4/G5), and must fail
  // closed with null rather than half-parse.
  const serialized = serializeEpicSpec({
    parentId: 'BOS-651',
    parent: { title: 't', goal: 'g', keyChanges: ['x'], priority: 2 },
    children: [child('c1'), child('c2')],
  })
  assert.deepEqual(
    parseEpicSpec(serialized).children.map((c) => c.key),
    ['c1', 'c2'],
  )
  assert.equal(parseEpicSpec(`## Epic overview\n\n${serialized}\n\n## Original notes\n`), null)
})

test('parseEpicSpec: returns null when the marker is absent', () => {
  assert.equal(parseEpicSpec('## Just a normal description'), null)
  assert.equal(parseEpicSpec(''), null)
  assert.equal(parseEpicSpec(null), null)
})

test('parseEpicSpec: returns null on a garbled marker (bad JSON or non-array children)', () => {
  assert.equal(parseEpicSpec('<!-- boss-plan-epic-spec:{not json} -->'), null)
  assert.equal(parseEpicSpec('<!-- boss-plan-epic-spec:{"children":"nope"} -->'), null)
})

// A full-size decomposition with every optional field exercised, used by G1.
const twelveChildSpec = () => ({
  parentId: 'BOS-651',
  parent: {
    title: 'Move the epic spec to an attachment',
    goal: 'Stop hiding a base64 blob in the parent description',
    keyChanges: ['skills-toolbox/plan-epic-lib.mjs', 'boss-plan/SKILL.md'],
    priority: 2,
  },
  children: Array.from({ length: EPIC_MAX_CHILDREN }, (_, i) =>
    child(`c${i + 1}`, {
      // A real chain: every child after the first is blocked by its predecessor.
      blockedByKeys: i === 0 ? [] : [`c${i}`],
      estimate: i % 4,
      priority: (i % 4) + 1,
      // Mixed layers, including the two consumer layers and an omitted layer.
      layer: ['contract', 'persistence', 'producer', 'read', 'ui', undefined][i % 6],
      // Mixed agent-friendliness: every third child needs a human.
      agentFriendly: i % 3 !== 0,
      // Mixed open questions: every other child recorded one.
      openQuestions: i % 2 === 0 ? [`question ${i}`] : [],
    }),
  ),
})

test('G1 serializeEpicSpec + parseEpicSpec: a full 12-child spec round-trips every field', () => {
  const spec = twelveChildSpec()
  const parsed = parseEpicSpec(serializeEpicSpec(spec))
  assert.ok(parsed, 'a serialized spec must parse back')
  assert.equal(parsed.schemaVersion, SPEC_SCHEMA_VERSION)
  assert.equal(parsed.parentId, 'BOS-651')
  assert.deepEqual(parsed.parent, spec.parent)
  // Deep-equal the FULL projection, not just the keys: a resume recreates each
  // missing child from exactly these fields, so a silently dropped one would
  // produce a differently-shaped child than the original decomposition.
  const expected = spec.children.map((c) => ({
    key: c.key,
    title: c.title,
    goal: c.goal,
    keyChanges: c.keyChanges,
    blockedByKeys: c.blockedByKeys,
    estimate: c.estimate,
    priority: c.priority,
    // An omitted/unknown layer is dropped entirely rather than carried as null.
    ...(c.layer === undefined ? {} : { layer: c.layer }),
    agentFriendly: c.agentFriendly,
    agentQuestion: c.openQuestions.length > 0,
  }))
  assert.deepEqual(parsed.children, expected)
  // The DAG survives, so topological scheduling still works on the recovered spec.
  assert.deepEqual(
    topoOrderChildren(parsed).map((c) => c.key),
    spec.children.map((c) => c.key),
  )
  // And a re-serialized recovered spec is byte-identical: resume is idempotent.
  assert.equal(serializeEpicSpec(parsed), serializeEpicSpec(spec))
})

test('G3 serializeEpicSpec: emits schemaVersion + parentId as readable JSON, with no marker or base64', () => {
  const serialized = serializeEpicSpec({
    parentId: 'BOS-651',
    parent: { title: 'Epic parent', goal: 'Ship it', keyChanges: ['services/x'], priority: 2 },
    children: [child('c1'), child('c2', { blockedByKeys: ['c1'] })],
  })
  // Assert the RAW string: the whole point of the move is that a human opening
  // the attachment can read it, so the wire bytes are the contract, not just the
  // object they parse into.
  assert.equal(serialized.includes('<!-- boss-plan-epic-spec:'), false, 'no marker wrapper')
  assert.equal(serialized.includes('<!--'), false, 'no HTML comment at all')
  assert.match(serialized, /^\{\n {2}"schemaVersion": 1,\n {2}"parentId": "BOS-651",\n/)
  assert.match(serialized, /\n {2}"children": \[\n {4}\{\n {6}"key": "c1",/)
  const parsed = JSON.parse(serialized)
  assert.equal(parsed.schemaVersion, 1)
  assert.equal(parsed.schemaVersion, SPEC_SCHEMA_VERSION)
  assert.equal(parsed.parentId, 'BOS-651')
})

test('G3 serializeEpicSpec: omits parentId entirely when the spec carries none', () => {
  // Never fabricate an id: an absent/blank parentId must leave the key out so
  // validateSpecIdentity refuses rather than trusting an invented match.
  for (const parentId of [undefined, null, '', '   ', 42]) {
    const serialized = serializeEpicSpec({
      parentId,
      parent: { title: 't', goal: 'g', keyChanges: ['x'], priority: 2 },
      children: [child('c1'), child('c2')],
    })
    assert.equal(serialized.includes('parentId'), false, `parentId omitted for ${String(parentId)}`)
    assert.equal('parentId' in JSON.parse(serialized), false)
  }
})

test('G6 parseEpicSpec: malformed, empty or truncated content returns null and never throws', () => {
  const cases = [
    ['a non-string (undefined)', undefined],
    ['a non-string (null)', null],
    ['a non-string (number)', 42],
    ['a non-string (object)', { parent: {}, children: [] }],
    ['an empty string', ''],
    ['plain prose', '## Just a normal description'],
    ['truncated JSON', '{'],
    ['a JSON array', '[{"key":"c1"}]'],
    ['a JSON scalar', '"just a string"'],
    ['JSON null', 'null'],
    ['a non-array children', '{"parent":{"title":"t"},"children":"nope"}'],
    // The TypeError trap: children IS an array, so the assignment
    // `parsed.parent.priority = 3` is reached on an absent parent. The old
    // parser wrapped its whole body in one try/catch and swallowed the throw;
    // the refactor narrowed that try to `JSON.parse` alone, so the explicit
    // parent guard — not a catch — is what keeps this null instead of throwing.
    ['an array children with NO parent', '{"schemaVersion":1,"children":[{"key":"c1"}]}'],
    ['a non-object parent', '{"schemaVersion":1,"parent":"nope","children":[]}'],
    ['an ARRAY parent (typeof "object")', '{"schemaVersion":1,"parent":[],"children":[]}'],
    ['a garbled base64 marker', '<!-- boss-plan-epic-spec:bm90IGpzb24= -->'],
    ['a garbled legacy marker', '<!-- boss-plan-epic-spec:{not json} -->'],
  ]
  for (const [label, input] of cases) {
    assert.doesNotThrow(() => parseEpicSpec(input), `${label} must not throw`)
    assert.equal(parseEpicSpec(input), null, `${label} must return null`)
  }
})

test('G7 parseEpicSpec: a non-canonical base64 marker is rejected and the legacy marker wins', () => {
  // The round-trip guard: a base64-LOOKING match that does not re-encode to the
  // same bytes is not a base64 marker at all. Without the guard it would be
  // accepted (or would swallow the match) and the real legacy payload sitting in
  // the same description would never be reached.
  const canonical = Buffer.from('{"parent":{"title":"decoy"},"children":[]}', 'utf8').toString(
    'base64',
  )
  // Extra padding: still matches [A-Za-z0-9+/=]+, but re-encoding drops it.
  const nonCanonical = `${canonical}==`
  assert.notEqual(nonCanonical, Buffer.from(nonCanonical, 'base64').toString('base64'))
  const description =
    `<!-- boss-plan-epic-spec:${nonCanonical} -->\n\n` +
    '<!-- boss-plan-epic-spec:{"parent":{"title":"real legacy","goal":"g","keyChanges":[],' +
    '"priority":1},"children":[{"key":"c1","title":"t1","goal":"g1","keyChanges":["x"],' +
    '"blockedByKeys":[],"estimate":3,"priority":2}]} -->'
  const parsed = parseEpicSpec(description)
  assert.ok(parsed, 'the legacy marker must be recovered')
  assert.equal(parsed.parent.title, 'real legacy')
  assert.notEqual(parsed.parent.title, 'decoy')
})

// The exact base64 wire format an EARLIER build wrote into the epic parent's
// description, pinned here as a LITERAL. It is deliberately NOT produced by
// calling into plan-epic-lib: the emitter is gone, and reconstructing the bytes
// through a helper the production code also owns would make this regression
// vacuous (the fallback could be deleted and the test would still agree with
// itself). Decoded payload:
//   {"parent":{"title":"Legacy epic","goal":"Recover the base64 marker",
//     "keyChanges":["services/x"],"priority":2},
//    "children":[{"key":"c1",…},{"key":"c2",…,"layer":"read",
//     "agentFriendly":false,"agentQuestion":true}]}
const LEGACY_BASE64_PAYLOAD =
  'eyJwYXJlbnQiOnsidGl0bGUiOiJMZWdhY3kgZXBpYyIsImdvYWwiOiJSZWNvdmVyIHRoZSBiYXNlNjQgbWFya2VyIiwia2' +
  'V5Q2hhbmdlcyI6WyJzZXJ2aWNlcy94Il0sInByaW9yaXR5IjoyfSwiY2hpbGRyZW4iOlt7ImtleSI6ImMxIiwidGl0bGUi' +
  'OiJ0MSIsImdvYWwiOiJnMSIsImtleUNoYW5nZXMiOlsieCJdLCJibG9ja2VkQnlLZXlzIjpbXSwiZXN0aW1hdGUiOjMsIn' +
  'ByaW9yaXR5IjoyfSx7ImtleSI6ImMyIiwidGl0bGUiOiJ0MiIsImdvYWwiOiJnMiIsImtleUNoYW5nZXMiOlsieSJdLCJi' +
  'bG9ja2VkQnlLZXlzIjpbImMxIl0sImVzdGltYXRlIjoyLCJwcmlvcml0eSI6NCwibGF5ZXIiOiJyZWFkIiwiYWdlbnRGcm' +
  'llbmRseSI6ZmFsc2UsImFnZW50UXVlc3Rpb24iOnRydWV9XX0='
const LEGACY_BASE64_MARKER = `<!-- boss-plan-epic-spec:${LEGACY_BASE64_PAYLOAD} -->`

test('G4 REGRESSION parseEpicSpec: a legacy base64 inline marker still parses', () => {
  // In-flight epics planned by an earlier build carry this marker in the parent
  // DESCRIPTION and no attachment at all. Dropping the base64 branch would make
  // every one of them unresumable — and would do so silently, because nothing
  // writes this format any more.
  const parsed = parseEpicSpec(LEGACY_BASE64_MARKER)
  assert.ok(parsed, 'a legacy base64 marker must still parse')
  assert.equal(parsed.parent.title, 'Legacy epic')
  assert.equal(parsed.parent.priority, 2)
  assert.deepEqual(
    parsed.children.map((c) => c.key),
    ['c1', 'c2'],
  )
  // The recovered metadata drives resume, so pin more than the keys.
  assert.equal(parsed.children[1].layer, 'read')
  assert.equal(parsed.children[1].agentFriendly, false)
  assert.equal(parsed.children[1].agentQuestion, true)
  assert.deepEqual(parsed.children[1].blockedByKeys, ['c1'])
  // Found embedded in surrounding parent-description prose too, which is how it
  // actually appears in the wild.
  const embedded = `## Epic overview\n\nGoal etc.\n\n${LEGACY_BASE64_MARKER}\n\n## Notes\n`
  assert.equal(parseEpicSpec(embedded).parent.title, 'Legacy epic')
})

test('G5 REGRESSION parseEpicSpec: a legacy raw-JSON inline marker still parses', () => {
  // The oldest wire format (pre-base64). Same silent-unresumability hazard as
  // G4: nothing writes it, so only this test holds the branch in place.
  const legacy =
    '<!-- boss-plan-epic-spec:{"parent":{"title":"Raw JSON epic","goal":"g","keyChanges":["x"],' +
    '"priority":4},"children":[{"key":"c1","title":"t1","goal":"g1","keyChanges":["x"],' +
    '"blockedByKeys":[],"estimate":3,"priority":2},{"key":"c2","title":"t2","goal":"g2",' +
    '"keyChanges":["y"],"blockedByKeys":["c1"],"estimate":1,"priority":3}]} -->'
  const parsed = parseEpicSpec(legacy)
  assert.ok(parsed, 'a legacy raw-JSON marker must still parse')
  assert.equal(parsed.parent.title, 'Raw JSON epic')
  assert.equal(parsed.parent.priority, 4)
  assert.deepEqual(
    parsed.children.map((c) => c.key),
    ['c1', 'c2'],
  )
  assert.deepEqual(parsed.children[1].blockedByKeys, ['c1'])
  const embedded = `## Epic overview\n\n${legacy}\n\n## Notes\n`
  assert.equal(parseEpicSpec(embedded).parent.title, 'Raw JSON epic')
})

// ---------------------------------------------------------------------------
// The spec-attachment helpers — filename, MIME, title, identity validation
// ---------------------------------------------------------------------------

test('G9 specAttachmentTitle/Filename/MIME pin the attachment identity', () => {
  assert.equal(specAttachmentFilename(), 'epic-spec.json')
  assert.equal(SPEC_ATTACHMENT_MIME, 'application/json')
  assert.equal(specAttachmentTitle('BOS-651'), 'Epic spec (BOS-651)')
  assert.equal(specAttachmentTitle('ENG-7'), 'Epic spec (ENG-7)')
})

test('G9 REGRESSION specAttachmentTitle does not start with "Implementation plan"', () => {
  // bs-epic-lib's normalizeTicket recognizes a ticket's PLAN attachment by
  // exactly that title prefix. A spec attachment whose title collided would be
  // mistaken for the ticket's implementation plan — so the prefix is a hard
  // negative, not a style preference.
  for (const id of ['BOS-651', 'Implementation', 'plan-1']) {
    assert.equal(
      specAttachmentTitle(id).startsWith('Implementation plan'),
      false,
      `title for ${id} must not collide with the plan-attachment prefix`,
    )
  }
})

test('G8 validateSpecIdentity: accepts only a current-schema spec bound to this ticket', () => {
  const bound = (over = {}) => ({
    schemaVersion: SPEC_SCHEMA_VERSION,
    parentId: 'BOS-651',
    parent: { title: 't', goal: 'g', keyChanges: ['x'], priority: 2 },
    children: [],
    ...over,
  })
  assert.deepEqual(validateSpecIdentity(bound(), 'BOS-651'), { ok: true, errors: [] })
  // A spec recovered from the WRONG ticket's attachment — the failure the
  // validator exists for, since an attachment title is a weak sentinel a human
  // can set to anything.
  const wrongTicket = validateSpecIdentity(bound(), 'BOS-999')
  assert.equal(wrongTicket.ok, false)
  assert.match(wrongTicket.errors.join('\n'), /BOS-651/)
  for (const [label, spec] of [
    ['a wrong schemaVersion', bound({ schemaVersion: 2 })],
    ['a missing schemaVersion', bound({ schemaVersion: undefined })],
    ['a stringified schemaVersion', bound({ schemaVersion: '1' })],
    ['a missing parentId', bound({ parentId: undefined })],
    ['a blank parentId', bound({ parentId: '   ' })],
    ['a non-string parentId', bound({ parentId: 651 })],
  ]) {
    const res = validateSpecIdentity(spec, 'BOS-651')
    assert.equal(res.ok, false, `${label} must be rejected`)
    assert.ok(res.errors.length > 0, `${label} must explain itself`)
  }
  // A non-object (e.g. parseEpicSpec returned null) is rejected structurally.
  for (const notASpec of [null, undefined, 'BOS-651', 42, []]) {
    let res
    assert.doesNotThrow(
      () => {
        res = validateSpecIdentity(notASpec, 'BOS-651')
      },
      `validateSpecIdentity(${String(notASpec)}) must not throw`,
    )
    assert.equal(res.ok, false)
    assert.ok(res.errors.length > 0)
  }
})

test('G8 validateSpecIdentity: a round-tripped spec validates against its own ticket', () => {
  // End to end: what serializeEpicSpec writes, parseEpicSpec recovers, and the
  // identity validator accepts — but only for the ticket it was written for.
  const recovered = parseEpicSpec(
    serializeEpicSpec({
      parentId: 'BOS-651',
      parent: { title: 't', goal: 'g', keyChanges: ['x'], priority: 2 },
      children: [child('c1'), child('c2')],
    }),
  )
  assert.equal(validateSpecIdentity(recovered, 'BOS-651').ok, true)
  assert.equal(validateSpecIdentity(recovered, 'BOS-650').ok, false)
})

// ---------------------------------------------------------------------------
// epicChildMarker / parseEpicChildMarker — the per-child resume marker
// ---------------------------------------------------------------------------

test('parseEpicChildMarker: round-trips epicChildMarker, finds it in surrounding prose, and returns null when absent', () => {
  assert.equal(parseEpicChildMarker(epicChildMarker('reclaim-the-budget')), 'reclaim-the-budget')
  const embedded = `## Plan\n\nSome description text.\n\n${epicChildMarker('ship-the-thing')}\n\nMore notes.`
  assert.equal(parseEpicChildMarker(embedded), 'ship-the-thing')
  assert.equal(parseEpicChildMarker('## No marker here'), null)
  assert.equal(parseEpicChildMarker(''), null)
  assert.equal(parseEpicChildMarker(null), null)
  assert.equal(parseEpicChildMarker(undefined), null)
  assert.equal(parseEpicChildMarker(42), null)
})

// ---------------------------------------------------------------------------
// reconcileEpicChildren — join spec.children[].key against each live child's
// epic-child marker
// ---------------------------------------------------------------------------

// A live child record shaped like a tracker's `list_issues parentId=<parentId>`
// result, carrying an epic-child marker in its description.
const liveChild = (id, key, title = `title ${key}`) => ({
  id,
  title,
  description: `${epicChildMarker(key)}\n\nBody text.`,
})

test('reconcileEpicChildren: an aligned epic adopts every live child and reports nothing missing or orphaned', () => {
  const spec = { children: [child('alpha'), child('beta')] }
  const live = [liveChild('id-1', 'alpha'), liveChild('id-2', 'beta')]
  const res = reconcileEpicChildren(spec, live)
  assert.equal(res.ok, true)
  assert.deepEqual(res.missing, [])
  assert.deepEqual(res.orphans, [])
  assert.deepEqual(res.repairs, [])
  assert.deepEqual(res.errors, [])
  assert.deepEqual(res.adopted, [
    { key: 'alpha', id: 'id-1' },
    { key: 'beta', id: 'id-2' },
  ])
})

test('reconcileEpicChildren: a partially-built epic reports the honest create-list, not an error', () => {
  const spec = { children: [child('alpha'), child('beta')] }
  const live = [liveChild('id-1', 'alpha')]
  const res = reconcileEpicChildren(spec, live)
  assert.equal(res.ok, true)
  assert.deepEqual(res.missing, ['beta'])
  assert.deepEqual(res.orphans, [])
  assert.deepEqual(res.adopted, [{ key: 'alpha', id: 'id-1' }])
})

test('reconcileEpicChildren: a retitled parent self-heals the unambiguous 1:1 rename', () => {
  const spec = { children: [child('reclaim-the-budget-with-x-and-y')] }
  const live = [liveChild('id-1', 'reclaim-the-budget-with-x')]
  const res = reconcileEpicChildren(spec, live)
  assert.equal(res.ok, true)
  assert.deepEqual(res.repairs, [
    {
      specKey: 'reclaim-the-budget-with-x-and-y',
      liveKey: 'reclaim-the-budget-with-x',
      id: 'id-1',
    },
  ])
  assert.deepEqual(res.missing, [])
  assert.deepEqual(res.orphans, [])
  assert.deepEqual(res.adopted, [{ key: 'reclaim-the-budget-with-x-and-y', id: 'id-1' }])
})

test('reconcileEpicChildren: the 1:1 repair lands at the orphan position in liveChildren order', () => {
  const spec = { children: [child('key-a'), child('key-b')] }
  const live = [liveChild('child0', 'key-a-old'), liveChild('child1', 'key-b')]
  const res = reconcileEpicChildren(spec, live)
  assert.equal(res.ok, true)
  assert.deepEqual(res.adopted, [
    { key: 'key-a', id: 'child0' },
    { key: 'key-b', id: 'child1' },
  ])
  assert.deepEqual(res.missing, [])
  assert.deepEqual(res.orphans, [])
  assert.deepEqual(res.repairs, [{ specKey: 'key-a', liveKey: 'key-a-old', id: 'child0' }])
})

test('reconcileEpicChildren: ambiguous drift (2 missing, 2 orphans) refuses and names both orphan keys', () => {
  const spec = { children: [child('alpha'), child('beta')] }
  const live = [liveChild('id-1', 'alpha-old'), liveChild('id-2', 'beta-old')]
  const res = reconcileEpicChildren(spec, live)
  assert.equal(res.ok, false)
  assert.deepEqual(res.repairs, [])
  assert.ok(res.errors.some((e) => e.includes('alpha-old') && e.includes('beta-old')))
})

test('reconcileEpicChildren: an unmarked live child refuses', () => {
  const spec = { children: [child('alpha')] }
  const live = [{ id: 'id-1', title: 'No marker child', description: 'plain body, no marker' }]
  const res = reconcileEpicChildren(spec, live)
  // Same ordering rationale as the fail-closed liveChildren/spec tests above:
  // `missing`/`repairs` are the whole-epic-duplication guard, so they are
  // asserted BEFORE `ok`. A refusal that quietly reports the spec keys as
  // `missing` (or fabricates a `repairs` entry) would still duplicate the
  // epic on resume even though `ok` is correctly false either way.
  assert.deepEqual(res.missing, [])
  assert.deepEqual(res.repairs, [])
  assert.equal(res.ok, false)
  assert.deepEqual(res.unmarked, [{ id: 'id-1', title: 'No marker child' }])
  assert.ok(
    res.errors.some((e) => e.includes('id-1')),
    'errors must name the unmarked child id',
  )
})

test('reconcileEpicChildren: truncated live child description names hydration instead of a missing marker', () => {
  const spec = { children: [child('alpha')] }
  const live = [
    {
      id: 'id-1',
      title: 'Truncated child',
      description: 'summary only … (truncated, use get_issue for full description)',
    },
  ]
  const res = reconcileEpicChildren(spec, live)

  assert.deepEqual(res.missing, [])
  assert.deepEqual(res.repairs, [])
  assert.equal(res.ok, false)
  assert.deepEqual(res.unmarked, [{ id: 'id-1', title: 'Truncated child' }])
  assert.ok(
    res.errors.some(
      (e) =>
        e.includes('description appears truncated') &&
        e.includes('get_issue') &&
        e.includes('not list_issues'),
    ),
    'errors must direct the caller to hydrate children with get_issue',
  )
  assert.ok(
    !res.errors.some((e) => e.includes('carries no epic-child marker')),
    'truncated descriptions must not use the genuinely unmarked diagnostic',
  )
})

test('reconcileEpicChildren: hydrated live children with full markers adopt cleanly', () => {
  const spec = { children: [child('alpha'), child('beta')] }
  const live = [liveChild('id-1', 'alpha'), liveChild('id-2', 'beta')]
  const res = reconcileEpicChildren(spec, live)

  assert.equal(res.ok, true)
  assert.deepEqual(res.adopted, [
    { key: 'alpha', id: 'id-1' },
    { key: 'beta', id: 'id-2' },
  ])
  assert.deepEqual(res.errors, [])
})

test('reconcileEpicChildren: genuinely unmarked short descriptions keep the original diagnostic', () => {
  const spec = { children: [child('alpha')] }
  const live = [{ id: 'id-1', title: 'No marker child', description: 'plain body, no marker' }]
  const res = reconcileEpicChildren(spec, live)

  assert.equal(res.ok, false)
  assert.ok(
    res.errors.some((e) => e === 'live child "id-1" carries no epic-child marker'),
    'short unmarked descriptions must keep the original diagnostic',
  )
  assert.ok(
    !res.errors.some((e) => e.includes('description appears truncated')),
    'short unmarked descriptions must not be classified as truncated',
  )
})

test('reconcileEpicChildren: duplicate live marker keys refuse', () => {
  const spec = { children: [child('alpha')] }
  const live = [liveChild('id-1', 'alpha'), liveChild('id-2', 'alpha')]
  const res = reconcileEpicChildren(spec, live)
  // Same ordering rationale as above: `missing`/`repairs` before `ok`.
  assert.deepEqual(res.missing, [])
  assert.deepEqual(res.repairs, [])
  assert.equal(res.ok, false)
  assert.ok(
    res.errors.some((e) => e.includes('alpha')),
    'errors must name the duplicated marker key',
  )
})

test('reconcileEpicChildren: fails closed on a non-array liveChildren without reporting the full spec as missing', () => {
  const spec = { children: [child('alpha'), child('beta')] }
  const resUndefined = reconcileEpicChildren(spec, undefined)
  // Assert `missing` BEFORE `ok`: `missing` is the load-bearing
  // whole-epic-duplication guard (a bug that refuses but still reports every
  // spec key as missing would still duplicate the epic on resume). node:test
  // aborts a test at its first failing assertion, so if `ok` were asserted
  // first, a mutation that neuters only the fail-closed guard on `missing`
  // would never surface here — the test would still go red, but on the wrong
  // assertion, making the mutation-revert proof illegible.
  assert.deepEqual(resUndefined.missing, [])
  assert.equal(resUndefined.ok, false)
  const resEnvelope = reconcileEpicChildren(spec, { nodes: [] })
  assert.deepEqual(resEnvelope.missing, [])
  assert.equal(resEnvelope.ok, false)
})

test('reconcileEpicChildren: fails closed on a spec with no children array', () => {
  const live = [liveChild('id-1', 'alpha')]
  const resNonObject = reconcileEpicChildren(null, live)
  // Same ordering rationale as the liveChildren fail-closed test above:
  // `missing` is the guarantee that matters, so it is asserted first.
  assert.deepEqual(resNonObject.missing, [])
  assert.equal(resNonObject.ok, false)
  const resNoChildren = reconcileEpicChildren({ children: 'nope' }, live)
  assert.deepEqual(resNoChildren.missing, [])
  assert.equal(resNoChildren.ok, false)
})
