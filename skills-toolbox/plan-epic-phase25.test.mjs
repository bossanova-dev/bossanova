// skills-toolbox/plan-epic-phase25.test.mjs
// Contract tests for the Phase 2.5 epic-parent primitives (BOS-652).
// node builtins only (cron worktrees are dependency-free). Modelled on
// plan-epic-lib.test.mjs (table-driven, section banners) and on
// plan-epic-lib.demo.mjs's `makeFakeTracker` shape.

import assert from 'node:assert/strict'
import { test } from 'node:test'

import {
  detectEpicParent,
  epicSpecRecoveryGate,
  stalePlanAttachmentSweep,
  epicPhase25WritePlan,
  epicReverifyVerdict,
} from './plan-epic-phase25.mjs'
import {
  serializeEpicSpec,
  parseEpicSpec,
  specAttachmentTitle,
  topoOrderChildren,
  epicChildMarker,
} from './plan-epic-lib.mjs'
import {
  REQUIRED_TRACKER_OPERATIONS,
  OPTIONAL_TRACKER_OPERATIONS,
  assertWritePlanEntryExecutable,
} from './tracker/adapter.mjs'
import { buildLinearOperationMap } from './tracker/linear.mjs'
import {
  DEFAULT_CONFIG,
  DESCRIPTION_NORMALIZATION_TRANSFORMS,
  mergeConfig,
} from './skill-config.mjs'

const PARENT_ID = 'BOS-999'

// A minimal well-formed child (mirrors plan-epic-lib.test.mjs's `child`).
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
const linearSpec = (n, over = {}) => ({
  parentId: PARENT_ID,
  parent: {
    title: 'Epic parent',
    goal: 'Ship the big thing',
    keyChanges: ['services/x'],
    priority: 2,
  },
  children: Array.from({ length: n }, (_, i) =>
    child(`c${i + 1}`, { blockedByKeys: i === 0 ? [] : [`c${i}`] }),
  ),
  ...over,
})

// A legacy inline description marker that really round-trips through
// parseEpicSpec (base64 of a serialized spec), so the positive inline case is
// not testing a string the parser happens to reject.
const legacyInlineDescription = (spec) =>
  `Some reporter prose.\n\n<!-- boss-plan-epic-spec:${Buffer.from(
    serializeEpicSpec(spec),
    'utf8',
  ).toString('base64')} -->\n`

// Push a spec through the REAL serializer and parser, so the write-plan
// scenarios run against the exact child shape production data has — not a
// hand-built object carrying fields `serializeEpicSpec` silently drops. A
// hand-built fixture is the same vacuity hazard as a replayed script: it lets an
// emitter "handle" a field that never survives a round trip.
//
// Deliberately the RAW-JSON grammar, not `legacyInlineDescription`'s base64
// marker: `epicPhase25WritePlan` consumes an ATTACHMENT-sourced spec, and the
// inline marker store is frozen, read-only and slated for removal. Routing these
// fixtures through it would break them, when it goes, for a reason that has
// nothing to do with what they assert. (Both routes parse to the same object
// today; the marker route stays where it is actually under test, in S1.)
const roundTrip = (spec) => parseEpicSpec(serializeEpicSpec(spec))

// ---------------------------------------------------------------------------
// The fake tracker — test-local, never exported to production code.
// ---------------------------------------------------------------------------

/**
 * Replay an emitted write plan against in-memory state, stopping at the first
 * rejection. `ops` is the ordered log of EXECUTED op names: the observable the
 * scenarios assert on, so an op that never ran cannot look like one that did.
 */
function makeFakeTracker({ failOn, attachments = [] } = {}) {
  const ops = []
  const children = []
  const state = { attachments: [...attachments] }
  let prepared = null
  return {
    ops,
    children,
    get attachments() {
      return state.attachments
    },
    run(plan) {
      const list = Array.isArray(plan) ? plan : []
      for (const entry of list) {
        if (entry.op === failOn) {
          return {
            ok: false,
            executed: [...ops],
            error: new Error(`injected failure: ${entry.op}`),
          }
        }
        switch (entry.op) {
          case 'moveState':
            break
          case 'preparePlanAttachment':
            // The signed URL only exists once the prepare has run — which is
            // why `putPlanAttachment` declares it a runtimeArg rather than the
            // emitter pretending to know it.
            prepared = { uploadURL: 'https://signed.invalid/upload', assetUrl: 'asset://spec' }
            break
          case 'putPlanAttachment':
            if (!prepared) {
              return { ok: false, executed: [...ops], error: new Error('put before prepare') }
            }
            break
          case 'finalizePlanAttachment':
            if (!prepared) {
              return { ok: false, executed: [...ops], error: new Error('finalize before prepare') }
            }
            state.attachments.push({
              id: `att-${state.attachments.length + 1}`,
              title: entry.args.title,
            })
            break
          case 'deletePlanAttachment':
            state.attachments = state.attachments.filter((a) => a.id !== entry.args.id)
            break
          case 'createChild':
            children.push({ ...entry.args })
            break
          default:
            return { ok: false, executed: [...ops], error: new Error(`unknown op ${entry.op}`) }
        }
        ops.push(entry.op)
      }
      return { ok: true, executed: [...ops], error: null }
    },
  }
}

// ---------------------------------------------------------------------------
// S1 — legacy detection, and the store-specific presence rule
// ---------------------------------------------------------------------------

test('S1: a valid legacy inline marker with no attachments is an inline-sourced epic parent', () => {
  const res = detectEpicParent({
    id: PARENT_ID,
    description: legacyInlineDescription(linearSpec(3)),
    attachments: [],
  })
  assert.equal(res.isEpicParent, true)
  assert.equal(res.source, 'inline')
  assert.equal(res.specAttachmentId, null)
  assert.equal(res.ambiguous, false)
  assert.ok(res.reasons.length >= 1)
})

test('S1: ATTACHMENT store — presence decides, an unreadable body is still an epic parent', () => {
  // Nothing but Phase 2.5 ever creates an `Epic spec (…)` attachment, so its
  // mere presence is proof. parseEpicSpec returning null on the body must NOT
  // flip detection — that is the whole point of the attachment store's rule.
  const res = detectEpicParent({
    id: PARENT_ID,
    description: 'ordinary reporter prose, no marker',
    attachments: [{ id: 'att-1', title: specAttachmentTitle(PARENT_ID), body: '{ truncated' }],
  })
  assert.equal(res.isEpicParent, true)
  assert.equal(res.source, 'attachment')
  assert.equal(res.specAttachmentId, 'att-1')
  assert.equal(res.ambiguous, false)
})

test('S1: DESCRIPTION store diverges — a QUOTED but unparseable marker is NOT an epic parent', () => {
  // The merged boss-plan SKILL.md (Phase 2.5) states the divergence: the
  // description is reporter-writable prose and a reporter can QUOTE the marker
  // string (SKILL.md itself does). Treating the quote as presence would
  // classify a brand-new ticket as an unreadable epic parent and abort it
  // loudly on every sweep, leaving it permanently unplannable. So the
  // description counts only when parseEpicSpec actually returns a spec.
  for (const description of [
    'Reporter prose quoting the marker: <!-- boss-plan-epic-spec: -->',
    '<!-- boss-plan-epic-spec:eyJzY2hlbWFW -->', // truncated base64
    '<!-- boss-plan-epic-spec:not base64 at all!! -->',
  ]) {
    const res = detectEpicParent({ id: PARENT_ID, description, attachments: [] })
    assert.equal(res.isEpicParent, false, description)
    assert.equal(res.source, null, description)
    assert.match(res.reasons.join('\n'), /quote|unparseable|could not (be )?read|not evidence/i)
  }
})

test('S1: the attachment store wins when both stores are present', () => {
  const res = detectEpicParent({
    id: PARENT_ID,
    description: legacyInlineDescription(linearSpec(2)),
    attachments: [{ id: 'att-7', title: specAttachmentTitle(PARENT_ID) }],
  })
  assert.equal(res.source, 'attachment')
  assert.equal(res.specAttachmentId, 'att-7')
})

test('S1: malformed issue payloads return a well-formed result and never throw', () => {
  const cases = [
    null,
    undefined,
    42,
    'BOS-999',
    [],
    {},
    { id: PARENT_ID, attachments: 'nope' },
    { id: PARENT_ID, description: null, attachments: null },
    { description: legacyInlineDescription(linearSpec(2)) }, // no id
    { id: PARENT_ID, attachments: [null, 3, { title: 42 }] },
  ]
  for (const input of cases) {
    const res = detectEpicParent(input)
    assert.equal(typeof res.isEpicParent, 'boolean', JSON.stringify(input))
    assert.ok(res.source === null || res.source === 'attachment' || res.source === 'inline')
    assert.equal(typeof res.ambiguous, 'boolean')
    assert.ok(Array.isArray(res.reasons) && res.reasons.length >= 1, JSON.stringify(input))
  }
})

// ---------------------------------------------------------------------------
// S2 — a failed spec upload must strand nothing destructive
// ---------------------------------------------------------------------------

// The two ops that make up the spec upload, used to locate the stage boundary
// in the executed-op log.
const SPEC_UPLOAD_OPS = ['preparePlanAttachment', 'putPlanAttachment', 'finalizePlanAttachment']

const phase25Plan = () =>
  epicPhase25WritePlan({
    parentId: PARENT_ID,
    spec: roundTrip(linearSpec(3)),
    unplannedState: 'Backlog',
    staleAttachmentIds: ['att-stale'],
    labelsToStrip: ['agent-friendly', 'needs-human'],
  })

test('S2: a failed spec upload deletes nothing and creates no children', () => {
  const fake = makeFakeTracker({
    failOn: 'finalizePlanAttachment',
    attachments: [{ id: 'att-stale', title: `Implementation plan (${PARENT_ID})` }],
  })
  const result = fake.run(phase25Plan())

  // Load-bearing assertion first: the destructive op never ran.
  assert.equal(
    fake.ops.filter((op) => op === 'deletePlanAttachment').length,
    0,
    'a failed spec upload must not delete the parent’s only plan artifact',
  )
  assert.equal(fake.ops.filter((op) => op === 'createChild').length, 0)
  assert.equal(fake.children.length, 0)
  assert.equal(result.ok, false)
  assert.equal(fake.attachments.length, 1) // the stale one survives, untouched
})

test('S2 positive control: with no injected failure every stage runs, uploads before destruction', () => {
  // Without this control S2 passes against a plan that emits nothing at all.
  const fake = makeFakeTracker({
    attachments: [{ id: 'att-stale', title: `Implementation plan (${PARENT_ID})` }],
  })
  const result = fake.run(phase25Plan())
  assert.equal(result.ok, true, result.error?.message)

  for (const op of ['moveState', ...SPEC_UPLOAD_OPS, 'deletePlanAttachment', 'createChild']) {
    assert.ok(fake.ops.includes(op), `expected ${op} to have executed`)
  }
  const lastUpload = Math.max(...SPEC_UPLOAD_OPS.map((op) => fake.ops.lastIndexOf(op)))
  assert.ok(lastUpload < fake.ops.indexOf('deletePlanAttachment'))
  assert.ok(lastUpload < fake.ops.indexOf('createChild'))
  // The stale attachment really was swept, and the spec attachment remains.
  assert.deepEqual(
    fake.attachments.map((a) => a.title),
    [specAttachmentTitle(PARENT_ID)],
  )
})

test('S2: children are created in topo order carrying the spec-persisted fields', () => {
  const spec = roundTrip(
    linearSpec(3, {
      children: [
        child('c3', { blockedByKeys: ['c2'], estimate: 5, priority: 1 }),
        child('c1', { estimate: 1, priority: 3 }),
        child('c2', { blockedByKeys: ['c1'], estimate: 2, priority: 4 }),
      ],
    }),
  )
  const plan = epicPhase25WritePlan({
    parentId: PARENT_ID,
    spec,
    unplannedState: 'Backlog',
    labelsToStrip: ['agent-friendly', 'needs-human'],
  })
  const creates = plan.filter((e) => e.op === 'createChild')

  assert.deepEqual(
    creates.map((e) => e.args.key),
    topoOrderChildren(spec).map((c) => c.key),
  )
  const byKey = Object.fromEntries(spec.children.map((c) => [c.key, c]))
  for (const entry of creates) {
    assert.equal(entry.args.marker, epicChildMarker(entry.args.key))
    assert.equal(entry.args.state, 'Backlog')
    assert.equal(entry.args.parentId, PARENT_ID)
    // `boss-epic` orders ready/merge work by these two, so a dropped or
    // defaulted value silently reschedules the epic.
    assert.equal(entry.args.estimate, byKey[entry.args.key].estimate)
    assert.equal(entry.args.priority, byKey[entry.args.key].priority)
  }

  // The emitter must NOT pretend to own child labels. `serializeEpicSpec`
  // persists no `labels` array, so any subtraction here would run against a
  // field round-tripped data never carries — and a caller trusting an emitted
  // `labels: []` as complete would create every child with NO content labels
  // and no `agent-question`, the signal Phase 4 mandates at creation.
  for (const entry of creates) {
    assert.ok(!('labels' in entry.args), `createChild must not emit a labels field`)
  }
  assert.ok(
    spec.children.every((c) => !('labels' in c)),
    'the round-tripped spec must carry no labels field — the premise of the assertion above',
  )
})

test('S2: the label strip is a pure label write — it carries no state field', () => {
  const [first] = phase25Plan()
  assert.equal(first.stage, 'label-strip')
  assert.equal(first.op, 'moveState')
  // `args` carries ONLY real `save_issue` arguments. The labels to strip are an
  // instruction to the executor, not a call argument: `save_issue` has no
  // "remove these" parameter — its `labels` replaces the whole set — so the
  // value to send is `currentLabels − stripLabels`, which this pure module is
  // never given. Emitting it inside `args` would name a parameter the tool does
  // not have, and an executor that spreads `args` would silently send `{id}`
  // alone, leaving the parent exposed for the whole create→wire→expose window.
  assert.deepEqual(first.args, { id: PARENT_ID })
  assert.ok(!('state' in first.args))
  assert.ok(!('removeLabels' in first.args), 'save_issue has no removeLabels argument')
  assert.deepEqual(first.stripLabels, ['agent-friendly', 'needs-human'])
  // Emitted even when there is nothing to strip.
  const empty = epicPhase25WritePlan({ parentId: PARENT_ID, spec: linearSpec(2) })
  assert.equal(empty[0].stage, 'label-strip')
  assert.deepEqual(empty[0].stripLabels, [])
})

test('S2: a malformed spec degrades to zero create-children ops rather than throwing', () => {
  const cyclic = {
    parent: { title: 't', goal: 'g', priority: 2 },
    children: [child('a', { blockedByKeys: ['b'] }), child('b', { blockedByKeys: ['a'] })],
  }
  for (const spec of [undefined, null, 'nope', [], {}, cyclic]) {
    // `unplannedState` IS resolved here, so a zero-child result can only be the
    // spec degrading — not the fail-closed state guard below firing instead.
    const plan = epicPhase25WritePlan({ parentId: PARENT_ID, spec, unplannedState: 'Backlog' })
    assert.equal(plan.filter((e) => e.op === 'createChild').length, 0, JSON.stringify(spec))
    // The non-child stages are unaffected — the caller still strips + uploads.
    assert.deepEqual(
      plan.map((e) => e.op),
      ['moveState', ...SPEC_UPLOAD_OPS],
    )
  }
})

test('S2: an unresolved parentId fails CLOSED — the whole plan is empty', () => {
  // Every emitted op addresses the parent, so there is no safe partial. Before
  // this guard the emitter happily produced `moveState {issueId: ""}` and three
  // uploads titled `Epic spec ()` — writes aimed at nothing, on the phase whose
  // sibling gate (epicSpecRecoveryGate) already fails closed on an unresolved
  // role. Emitting nothing is the only honest degradation.
  for (const input of [undefined, null, 'nope', 42, [], {}, { spec: linearSpec(2) }]) {
    assert.deepEqual(epicPhase25WritePlan(input), [], JSON.stringify(input))
  }
  for (const parentId of ['', '   ', 7, null, undefined]) {
    assert.deepEqual(
      epicPhase25WritePlan({ parentId, spec: roundTrip(linearSpec(2)), unplannedState: 'Backlog' }),
      [],
      JSON.stringify(parentId),
    )
  }
})

test('S2: an unresolved unplannedState fails CLOSED — stages 1-3 stand, zero children', () => {
  // A child created with no state lands in the tracker's DEFAULT state, which
  // may be the planned one — exposing an unwired shell to boss-build. Stages
  // 1-3 need no state, so they still stand.
  const spec = roundTrip(linearSpec(3))
  for (const unplannedState of [undefined, null, '', '  ', 7]) {
    const plan = epicPhase25WritePlan({ parentId: PARENT_ID, spec, unplannedState })
    assert.deepEqual(
      plan.map((e) => e.op),
      ['moveState', ...SPEC_UPLOAD_OPS],
      JSON.stringify(unplannedState),
    )
  }
  // Positive control: the ONLY difference is a resolved state.
  assert.equal(
    epicPhase25WritePlan({ parentId: PARENT_ID, spec, unplannedState: 'Backlog' }).filter(
      (e) => e.op === 'createChild',
    ).length,
    3,
  )
})

// The two emitted ops that are NOT tracker-adapter operations, pinned as a
// literal here so a drift in either direction is loud:
//   * putPlanAttachment — a real exported function in plan-attachment.mjs: the
//     raw signed-URL HTTP PUT between prepare and finalize. Not an MCP tool.
//   * createChild — no adapter operation exists at all; children are created
//     through the `save_issue` MCP tool. The name is existing fake-tracker
//     vocabulary (plan-epic-lib.demo.mjs).
const NON_ADAPTER_OPS = ['createChild', 'putPlanAttachment']

test('S2: every emitted op is either an adapter operation or a pinned non-adapter op', () => {
  const adapterOps = new Set([...REQUIRED_TRACKER_OPERATIONS, ...OPTIONAL_TRACKER_OPERATIONS])
  const emitted = phase25Plan().map((e) => e.op)

  // 1. Nothing is emitted outside the union.
  for (const op of emitted) {
    assert.ok(adapterOps.has(op) || NON_ADAPTER_OPS.includes(op), `unclassified op: ${op}`)
  }
  // 2. Everything not pinned non-adapter really is an adapter operation — this
  //    is what makes an adapter-op rename break this plan.
  for (const op of emitted) {
    if (NON_ADAPTER_OPS.includes(op)) continue
    assert.ok(adapterOps.has(op), `${op} is not a tracker adapter operation`)
  }
  // 3. No dead allowlist entries.
  for (const op of NON_ADAPTER_OPS) {
    assert.ok(emitted.includes(op), `${op} is allowlisted but never emitted`)
  }
  // 4. Disjoint: if the adapter ever grows an operation by one of these names,
  //    fail loudly and force a re-partition rather than silently absorbing it.
  for (const op of NON_ADAPTER_OPS) {
    assert.ok(!adapterOps.has(op), `${op} is now an adapter operation — re-partition`)
  }
})

test('S2: every adapter-backed op emits the adapter’s OWN arg keys, and names the rest', () => {
  // Ordering alone is not executability. Before this check the emitter gave all
  // three spec-upload ops one identical `{issueId, filename, mimeType, title}`
  // blob and keyed the delete by `{issueId, attachmentId}` — while the adapter
  // declares `{issue, filename, contentType, size}`, `{issue, assetUrl, title}`
  // and `{id}`. Nothing caught it: the fake tracker reads two arg fields, so a
  // plan of plausible-looking WRONG keys replayed perfectly green. Cross-check
  // against the adapter's own summaries instead of a restated literal, so a
  // rename or a new required argument breaks here.
  const adapter = { operationMap: buildLinearOperationMap('t') }

  // No BEYOND_SUMMARY option exists, and it must stay that way. The first version of this check exempted
  // `moveState: {removeLabels}` — and that single exception was exactly the size
  // of a real bug: `save_issue` has no `removeLabels` argument (its `labels`
  // REPLACES the set, which is why SKILL.md tells the executor to read and
  // merge), so the one emitted key that was not a real argument was waved
  // through by the one check that would have caught it. Anything that needs an
  // entry here is a claim the adapter cannot back — carry it OUTSIDE `args`,
  // the way stage 1 now carries `stripLabels`.
  const BEYOND_SUMMARY = {}

  // The mirror exception: an argument the emitter deliberately OMITS. Stage 1
  // is a pure label write, so it must not carry `state` — no stage of this
  // sequence moves the parent out of unplanned (parent-repurpose-last). Pinned
  // here rather than blanket-exempting, so any OTHER uncovered argument — one
  // an executor would silently omit — still fails.
  const DELIBERATELY_OMITTED = { moveState: ['state'] }

  const adapterBacked = phase25Plan().filter((entry) => !NON_ADAPTER_OPS.includes(entry.op))
  assert.ok(adapterBacked.length >= 4, 'expected the adapter-backed ops to be exercised')

  for (const entry of adapterBacked) {
    assert.doesNotThrow(() =>
      assertWritePlanEntryExecutable(entry, adapter, {
        nonAdapterOps: NON_ADAPTER_OPS,
        deliberatelyOmitted: DELIBERATELY_OMITTED,
      }),
    )
  }

  // The raw PUT is not an adapter op, but it is the one whose arguments are
  // ENTIRELY runtime-derived — pin that it claims none statically.
  const put = phase25Plan().find((entry) => entry.op === 'putPlanAttachment')
  assert.deepEqual(put.args, {})
  assert.deepEqual(put.runtimeArgs, ['file', 'uploadURL', 'headers'])
})

// ---------------------------------------------------------------------------
// S3 — duplicate spec attachments
// ---------------------------------------------------------------------------

test('S3: two attachments with the exact spec title are ambiguous and yield no id', () => {
  const res = detectEpicParent({
    id: PARENT_ID,
    attachments: [
      { id: 'att-1', title: specAttachmentTitle(PARENT_ID) },
      { id: 'att-2', title: specAttachmentTitle(PARENT_ID) },
    ],
  })
  assert.equal(res.ambiguous, true)
  assert.equal(res.isEpicParent, true)
  assert.equal(res.source, 'attachment')
  assert.equal(res.specAttachmentId, null) // never guess which one is current
})

test('S3: exactly one match is unambiguous and reports that attachment id', () => {
  const res = detectEpicParent({
    id: PARENT_ID,
    attachments: [
      { id: 'other', title: `Implementation plan (${PARENT_ID})` },
      { id: 'att-42', title: specAttachmentTitle(PARENT_ID) },
    ],
  })
  assert.equal(res.ambiguous, false)
  assert.equal(res.specAttachmentId, 'att-42')
})

test('S3: title matching is id-scoped — another epic’s spec title does not count', () => {
  const res = detectEpicParent({
    id: PARENT_ID,
    description: 'no marker here',
    attachments: [
      { id: 'att-1', title: specAttachmentTitle('BOS-000') },
      { id: 'att-2', title: specAttachmentTitle('BOS-000') },
    ],
  })
  assert.equal(res.isEpicParent, false)
  assert.equal(res.ambiguous, false)
  assert.equal(res.specAttachmentId, null)
})

// ---------------------------------------------------------------------------
// S4 — the unreadable-spec recovery gate (noop | abort, never fall through)
// ---------------------------------------------------------------------------

const PLANNED = 'Todo'
const EPIC = 'epic'

const goodChild = (id, over = {}) => ({
  id,
  state: PLANNED,
  attachments: [{ id: `${id}-plan`, title: `Implementation plan (${id})` }],
  links: [],
  ...over,
})

const goodParent = (over = {}) => ({ id: PARENT_ID, state: PLANNED, labels: [EPIC], ...over })

// A live child the epic path never minted: a real, non-empty description that
// demonstrably carries no epic-child marker.
const handAddedChild = (id, over = {}) => ({
  id,
  state: 'Backlog',
  description: 'Somebody filed this by hand under the epic parent.',
  attachments: [],
  links: [],
  ...over,
})

// ---------------------------------------------------------------------------
// S4b — the epic-child membership test at the recovery gate (BOS-1255). Same
// rule as reconcileEpicChildren's: the marker is the membership test, and a
// child that carries none is excluded from the epic-child conjuncts rather
// than aborting the gate — but only where its absence is PROVEN.
// ---------------------------------------------------------------------------

test('S4b: a hand-added sub-issue is excluded from the child conjuncts and named, not aborted', () => {
  const res = epicSpecRecoveryGate({
    plannedState: PLANNED,
    epicLabel: EPIC,
    parent: goodParent(),
    // The hand-added child fails BOTH per-child conjuncts (unplanned, no plan
    // artifact), so before the membership test it contributed two abort
    // reasons and wedged the gate.
    children: [
      goodChild('BOS-1', { description: epicChildMarker('c1') }),
      handAddedChild('BOS-HAND'),
    ],
  })

  assert.equal(res.action, 'noop', res.reasons.join(' | '))
  assert.ok(
    res.reasons.some((r) => r.includes('BOS-HAND') && r.includes('no epic-child marker')),
    'the excluded child must still be named on the noop path',
  )
  assert.ok(
    !res.reasons.some((r) => r.includes('BOS-HAND') && r.includes('Backlog')),
    'the excluded child must not be scored against the planned-state conjunct',
  )
  assert.ok(
    !res.reasons.some((r) => r.includes('BOS-HAND') && r.includes('no plan artifact')),
    'the excluded child must not be scored against the plan-artifact conjunct',
  )
})

test('S4b: a marker-carrying child that fails a conjunct still aborts', () => {
  const res = epicSpecRecoveryGate({
    plannedState: PLANNED,
    epicLabel: EPIC,
    parent: goodParent(),
    children: [
      goodChild('BOS-1', { description: epicChildMarker('c1'), state: 'Backlog' }),
      handAddedChild('BOS-HAND'),
    ],
  })

  assert.equal(res.action, 'abort')
  assert.ok(res.reasons.some((r) => r.includes('BOS-1') && r.includes('Backlog')))
  assert.ok(
    res.reasons.some((r) => r.includes('BOS-HAND')),
    'the excluded child is named on the abort path too',
  )
})

test('S4b: absence of the marker must be PROVEN — truncated, empty and missing descriptions still judge', () => {
  // Each of these is a description that proves nothing about the marker, so
  // the child keeps being judged and the gate stays fail-closed. All three
  // children are unplanned with no plan artifact, so a wrongly-excluded child
  // would flip the verdict to noop.
  const unprovable = {
    'list-truncated description': 'summary … (truncated, use get_issue for full description)',
    'empty description': '',
    'whitespace-only description': '   ',
    'absent description': undefined,
  }
  for (const [name, description] of Object.entries(unprovable)) {
    const res = epicSpecRecoveryGate({
      plannedState: PLANNED,
      epicLabel: EPIC,
      parent: goodParent(),
      children: [
        goodChild('BOS-1', { description: epicChildMarker('c1') }),
        handAddedChild('BOS-UNPROVEN', { description }),
      ],
    })
    assert.equal(res.action, 'abort', `${name}: ${res.reasons.join(' | ')}`)
    assert.ok(
      res.reasons.some((r) => r.includes('BOS-UNPROVEN') && r.includes('Backlog')),
      `${name}: the child must still be scored against the planned-state conjunct`,
    )
  }
})

test('S4b: children that are ALL unmarked abort — a never-decomposed epic is not complete', () => {
  // The sibling of the zero-enumerated-children conjunct. A parent whose only
  // live children were added by hand proves exactly as little about
  // decomposition as a parent with none, so noop-ing here would declare a
  // never-decomposed epic complete.
  const res = epicSpecRecoveryGate({
    plannedState: PLANNED,
    epicLabel: EPIC,
    parent: goodParent(),
    children: [
      handAddedChild('BOS-H1', { state: PLANNED }),
      handAddedChild('BOS-H2', { state: PLANNED }),
    ],
  })

  assert.equal(res.action, 'abort')
  assert.ok(
    res.reasons.some((r) => r.includes('none carries an epic-child marker')),
    res.reasons.join(' | '),
  )
  assert.ok(
    res.reasons.some((r) => r.includes('BOS-H1')) && res.reasons.some((r) => r.includes('BOS-H2')),
    'every excluded child is still named',
  )
})

test('S4: the recovery gate noops only when every conjunct holds', () => {
  const cases = [
    {
      name: 'all conjuncts satisfied',
      input: { parent: goodParent(), children: [goodChild('BOS-1'), goodChild('BOS-2')] },
      action: 'noop',
    },
    {
      name: 'plan artifact is a LINK rather than an attachment',
      input: {
        parent: goodParent(),
        children: [
          goodChild('BOS-1', {
            attachments: [],
            links: [{ title: 'Implementation plan (BOS-1)', url: 'https://x.invalid/p' }],
          }),
        ],
      },
      action: 'noop',
    },
    {
      name: 'parent is still unplanned',
      input: { parent: goodParent({ state: 'Backlog' }), children: [goodChild('BOS-1')] },
      action: 'abort',
      reason: /parent/i,
    },
    {
      name: 'parent is missing the epic label',
      input: { parent: goodParent({ labels: ['backend'] }), children: [goodChild('BOS-1')] },
      action: 'abort',
      reason: /label/i,
    },
    {
      name: 'zero children',
      input: { parent: goodParent(), children: [] },
      action: 'abort',
      reason: /child/i,
    },
    {
      name: 'one child is unplanned',
      input: {
        parent: goodParent(),
        children: [goodChild('BOS-1'), goodChild('BOS-2', { state: 'In Progress' })],
      },
      action: 'abort',
      reason: /BOS-2/,
    },
    {
      name: 'one child has no plan artifact',
      input: {
        parent: goodParent(),
        children: [goodChild('BOS-1'), goodChild('BOS-2', { attachments: [], links: [] })],
      },
      action: 'abort',
      reason: /BOS-2/,
    },
    {
      name: 'a near-miss artifact title does not count',
      input: {
        parent: goodParent(),
        children: [
          goodChild('BOS-1', {
            attachments: [{ id: 'a', title: `Epic spec (BOS-1)` }],
            links: [],
          }),
        ],
      },
      action: 'abort',
      reason: /BOS-1/,
    },
    {
      name: 'malformed children collection',
      input: { parent: goodParent(), children: 'nope' },
      action: 'abort',
    },
    {
      name: 'malformed parent',
      input: { parent: null, children: [goodChild('BOS-1')] },
      action: 'abort',
      reason: /parent/i,
    },
  ]

  for (const testCase of cases) {
    const res = epicSpecRecoveryGate({
      plannedState: PLANNED,
      epicLabel: EPIC,
      ...testCase.input,
    })
    assert.equal(res.action, testCase.action, testCase.name)
    assert.ok(Array.isArray(res.reasons) && res.reasons.length >= 1, testCase.name)
    if (testCase.reason) assert.match(res.reasons.join('\n'), testCase.reason, testCase.name)
    // The mechanical form of "never falls through to the single-ticket path".
    assert.ok(['noop', 'abort'].includes(res.action), testCase.name)
  }
})

test('S4: a conforming epic reaches noop in EVERY issue shape the tracker returns', () => {
  // The gate's whole purpose is the noop/abort discrimination, and the prose
  // tells the caller to feed it the `list_issues` enumeration it already has —
  // no normalization step is named anywhere. So reading one shape would make
  // `'noop'` unreachable on real data: a fully conforming epic aborted while
  // naming a FALSE cause for every conjunct, which actively misleads the human
  // doing the remediation. Every shape below is the SAME conforming epic.
  const artifacts = (id) => ({
    attachments: [{ id: `${id}-plan`, title: `Implementation plan (${id})` }],
    links: [],
  })
  const shapes = {
    'MCP (status + label objects)': {
      parent: { id: PARENT_ID, status: PLANNED, labels: [{ name: EPIC }] },
      children: [{ id: 'BOS-1', status: PLANNED, ...artifacts('BOS-1') }],
    },
    'raw GraphQL (state.name + labels.nodes + attachments.nodes)': {
      parent: { id: PARENT_ID, state: { name: PLANNED }, labels: { nodes: [{ name: EPIC }] } },
      children: [
        {
          id: 'BOS-1',
          state: { name: PLANNED },
          labels: { nodes: [] },
          attachments: { nodes: [{ id: 'a', title: 'Implementation plan (BOS-1)' }] },
          links: { nodes: [] },
        },
      ],
    },
    'already normalized (stateName + bare strings)': {
      parent: { id: PARENT_ID, stateName: PLANNED, labels: [EPIC] },
      children: [{ id: 'BOS-1', stateName: PLANNED, ...artifacts('BOS-1') }],
    },
    'bare state string': {
      parent: goodParent(),
      children: [goodChild('BOS-1')],
    },
  }
  for (const [name, input] of Object.entries(shapes)) {
    const res = epicSpecRecoveryGate({ plannedState: PLANNED, epicLabel: EPIC, ...input })
    assert.equal(res.action, 'noop', `${name}: ${res.reasons.join(' | ')}`)
  }

  // Negative control: the shape tolerance must not turn into "anything passes" —
  // an unplanned parent still aborts in the MCP shape.
  const unplanned = epicSpecRecoveryGate({
    plannedState: PLANNED,
    epicLabel: EPIC,
    parent: { id: PARENT_ID, status: 'Backlog', labels: [{ name: EPIC }] },
    children: [{ id: 'BOS-1', status: PLANNED, ...artifacts('BOS-1') }],
  })
  assert.equal(unplanned.action, 'abort')
  assert.match(unplanned.reasons.join('\n'), /"Backlog"/)
})

test('S4: multiple failed conjuncts are each named separately in reasons', () => {
  const res = epicSpecRecoveryGate({
    plannedState: PLANNED,
    epicLabel: EPIC,
    parent: goodParent({ state: 'Backlog', labels: [] }),
    children: [],
  })
  assert.equal(res.action, 'abort')
  assert.ok(res.reasons.length >= 3, res.reasons.join('\n'))
})

test('S4: an unresolved plannedState or epicLabel fails CLOSED', () => {
  const good = { parent: goodParent(), children: [goodChild('BOS-1')] }
  for (const roles of [
    {},
    { plannedState: PLANNED },
    { epicLabel: EPIC },
    { plannedState: '', epicLabel: EPIC },
    { plannedState: PLANNED, epicLabel: null },
    { plannedState: 7, epicLabel: EPIC },
  ]) {
    const res = epicSpecRecoveryGate({ ...good, ...roles })
    assert.equal(res.action, 'abort', JSON.stringify(roles))
    assert.match(res.reasons.join('\n'), /plannedState|epicLabel/)
  }
})

test('S4: the gate never throws, including on undefined input', () => {
  for (const input of [undefined, null, 'nope', 42, []]) {
    const res = epicSpecRecoveryGate(input)
    assert.equal(res.action, 'abort')
    assert.ok(res.reasons.length >= 1)
  }
})

// ---------------------------------------------------------------------------
// S5 — the stale plan-attachment sweep
// ---------------------------------------------------------------------------

test('S5: the sweep returns stale plan attachments only, keeping the current one', () => {
  const attachments = [
    { id: '#stale', title: `Implementation plan (${PARENT_ID})` },
    { id: '#keep', title: `Implementation plan (${PARENT_ID})` },
    { id: '#spec', title: specAttachmentTitle(PARENT_ID) },
  ]
  const swept = stalePlanAttachmentSweep(attachments, { keepAttachmentId: '#keep' })
  assert.ok(swept.includes('#stale'))
  assert.ok(!swept.includes('#keep'))
  assert.ok(!swept.includes('#spec'))
  assert.deepEqual(swept, ['#stale'])
})

test('S5: an Epic spec attachment declared text/markdown is STILL never swept', () => {
  // The near-miss being guarded: selectImplementationPlanAttachment (in
  // plan-attachment.mjs) has a Markdown fallback that matches any attachment
  // whose title merely INCLUDES the issue id — and `Epic spec (BOS-999)`
  // includes it. That helper is saved today only by the incidental fact that a
  // spec attachment is application/json. This predicate is prefix-scoped and
  // consults no content type, so flipping the MIME changes nothing.
  const swept = stalePlanAttachmentSweep([
    { id: '#spec-md', title: specAttachmentTitle(PARENT_ID), contentType: 'text/markdown' },
    { id: '#spec-md2', title: specAttachmentTitle(PARENT_ID), mimeType: 'text/markdown' },
    { id: '#stale', title: `Implementation plan (${PARENT_ID})`, contentType: 'text/markdown' },
  ])
  assert.deepEqual(swept, ['#stale'])
})

test('S5: input order is preserved and unusable entries are skipped', () => {
  const swept = stalePlanAttachmentSweep([
    { id: '#a', title: 'Implementation plan (BOS-1)' },
    null,
    { title: 'Implementation plan (BOS-2)' }, // no id
    { id: '', title: 'Implementation plan (BOS-3)' }, // empty id
    { id: 42, title: 'Implementation plan (BOS-4)' }, // non-string id
    { id: '#b', title: 'Implementation plan' }, // bare prefix still counts
    { id: '#c', title: 'implementation plan (BOS-5)' }, // case-sensitive: no
    { id: '#d' }, // no title
  ])
  assert.deepEqual(swept, ['#a', '#b'])
})

test('S5: a non-array or absent attachments collection never throws', () => {
  for (const input of [undefined, null, 'nope', 42, {}]) {
    assert.deepEqual(stalePlanAttachmentSweep(input), [])
  }
  assert.deepEqual(stalePlanAttachmentSweep([{ id: '#a', title: 'Implementation plan' }], null), [
    '#a',
  ])
})

// ---------------------------------------------------------------------------
// S6 — epicReverifyVerdict: the epic-outcome acceptance gate (BOS-1335)
// ---------------------------------------------------------------------------

const RV_PARENT = 'BOS-900'
const RV_ROLES = {
  planned: 'Todo',
  unplanned: 'Unplanned',
  inProgress: 'In Progress',
  inReview: 'In Review',
  epic: 'epic',
  agentFriendly: 'agent-friendly',
  needsHuman: 'needs-human',
  agentQuestion: 'agent-question',
  agentPlan: 'agent-plan',
}
const rvConfig = (tolerated = DESCRIPTION_NORMALIZATION_TRANSFORMS) =>
  mergeConfig(DEFAULT_CONFIG, {
    adapters: { ...DEFAULT_CONFIG.adapters, tracker: 'demo' },
    trackerConfig: {
      demo: {
        mcpServer: 'demo-tracker',
        team: 'Demo',
        descriptionNormalization: { tolerated: [...tolerated] },
      },
    },
  })

const RV_NOTES = 'Reporter context.\n\n- first observation\n- second observation\n'

// A description satisfying the DEFAULT_CONFIG child-plan contract, carrying the epic-child marker
// immediately before `## Original notes` (where the skill places it).
const rvChildBody = (key, { drop = null, notes = RV_NOTES } = {}) =>
  [
    `## Summary\n\nChild ${key}.`,
    '## Approach\n\n- do the thing',
    '## Key changes\n\n- `skills-toolbox/x.mjs`',
    '## Testing\n\n- unit coverage',
    '## Risks / unknowns\n\n- none',
    '## Acceptance criteria\n\n- [ ] it works',
    '## Required proof\n\n- [ ] (backend-only) no screenshot applicable',
    '## Planning\n\n- Contract: v1',
    `${epicChildMarker(key)}\n\n## Original notes\n\n${notes}`,
  ]
    .filter((section) => drop == null || !section.startsWith(drop))
    .join('\n\n')

const rvOverview = (extra = '') =>
  [
    `## Summary\n\nDecompose the epic into shippable children.${extra}`,
    '## Child tickets\n\n- BOS-901\n- BOS-902',
    '## Planning\n\n- Contract: v1',
    `## Original notes\n\n${RV_NOTES}`,
  ].join('\n\n')

const rvSpec = () =>
  parseEpicSpec(
    serializeEpicSpec({
      parentId: RV_PARENT,
      parent: { title: 'Epic', goal: 'g', keyChanges: ['x'], priority: 2 },
      children: [
        child('c1', { agentFriendly: true }),
        child('c2', { agentFriendly: false, openQuestions: ['why?'], blockedByKeys: ['c1'] }),
      ],
    }),
  )

const rvChild = (n, key, labels, over = {}) => ({
  id: `uuid-${n}`,
  identifier: `BOS-${n}`,
  title: `title ${key}`,
  state: { name: 'Todo' },
  labels: labels.map((name) => ({ name })),
  attachments: [{ id: `plan-${n}`, title: `Implementation plan (BOS-${n})` }],
  links: [],
  description: rvChildBody(key),
  ...over,
})

// The conforming epic: one agent-friendly child, one needs-human + agent-question child, both
// byte-identical to their intended bodies, an attachment-sourced spec and a single overview artifact.
function rvInput(mutate = () => {}) {
  const children = [
    rvChild(901, 'c1', ['agent-friendly']),
    rvChild(902, 'c2', ['needs-human', 'agent-question']),
  ]
  const input = {
    parentId: RV_PARENT,
    childIds: ['BOS-901', 'BOS-902'],
    parent: {
      id: 'uuid-900',
      identifier: RV_PARENT,
      state: { name: 'Todo' },
      labels: [{ name: 'epic' }],
      attachments: [
        { id: 'att-spec', title: specAttachmentTitle(RV_PARENT) },
        { id: 'att-plan', title: `Implementation plan (${RV_PARENT})` },
      ],
      links: [],
    },
    children,
    spec: rvSpec(),
    roles: { ...RV_ROLES },
    config: rvConfig(),
    parentOverview: { intended: rvOverview(), stored: rvOverview() },
    childBodies: Object.fromEntries(
      children.map((c) => [c.identifier, { intended: c.description, stored: c.description }]),
    ),
  }
  mutate(input)
  return input
}

const codes = (verdict) => verdict.blockers.map((b) => b.code)
const noticeCodes = (verdict) => verdict.notices.map((n) => n.code)
const childOf = (input, identifier) => input.children.find((c) => c.identifier === identifier)
// Change a child's stored description everywhere the verb would have filled it.
const setStored = (input, identifier, text) => {
  childOf(input, identifier).description = text
  input.childBodies[identifier].stored = text
}

test('S6: a conforming epic passes with no blockers', () => {
  const verdict = epicReverifyVerdict(rvInput())
  assert.deepEqual(codes(verdict), [])
  assert.equal(verdict.ok, true)
  assert.equal(verdict.class, 'pass')
  // The passing overview is an epic-parent shape: no `## Acceptance criteria`, so a child-plan
  // comparison would have failed it — the mode is threaded through.
  assert.doesNotMatch(rvOverview(), /## Acceptance criteria/)
  assert.match(rvOverview(), /## Child tickets/)
})

test('S6: missing or empty childIds is its own code, reported ALONGSIDE a reconcile code', () => {
  for (const childIds of [undefined, []]) {
    const verdict = epicReverifyVerdict(
      rvInput((input) => {
        input.childIds = childIds
        input.children = input.children.slice(0, 1) // c2 never created ⇒ reconcile-missing
      }),
    )
    assert.ok(codes(verdict).includes('childids-missing'), JSON.stringify(codes(verdict)))
    assert.ok(codes(verdict).includes('reconcile-missing'), JSON.stringify(codes(verdict)))
    assert.equal(verdict.ok, false)
  }
})

test('S6: the marked child set must equal childIds; unmarked children are notices', () => {
  const extra = epicReverifyVerdict(rvInput((input) => (input.childIds = ['BOS-901'])))
  assert.ok(codes(extra).includes('child-set-mismatch'))

  const ghost = epicReverifyVerdict(rvInput((input) => input.childIds.push('BOS-999')))
  assert.ok(codes(ghost).includes('child-set-mismatch'))

  const handAdded = epicReverifyVerdict(
    rvInput((input) =>
      input.children.push({
        id: 'uuid-950',
        identifier: 'BOS-950',
        state: { name: 'Unplanned' },
        labels: [],
        description: 'A sub-issue a human added by hand.',
      }),
    ),
  )
  assert.deepEqual(codes(handAdded), [])
  assert.ok(noticeCodes(handAdded).includes('child-unmarked'))

  const truncated = epicReverifyVerdict(
    rvInput((input) => {
      childOf(input, 'BOS-902').description =
        '## Summary\n\n…(truncated, use get_issue for full description)'
    }),
  )
  assert.ok(codes(truncated).includes('child-description-truncated'))
})

test('S6: childIds match on identifier OR id — UUID sentinels and identifier sentinels both work', () => {
  const byUuid = epicReverifyVerdict(
    rvInput((input) => {
      input.childIds = ['uuid-901', 'uuid-902']
      input.childBodies = {
        'uuid-901': input.childBodies['BOS-901'],
        'uuid-902': input.childBodies['BOS-902'],
      }
    }),
  )
  assert.deepEqual(codes(byUuid), [])
})

test('S6: the parent state decides the failure class', () => {
  const unplanned = epicReverifyVerdict(
    rvInput((input) => (input.parent.state = { name: 'Unplanned' })),
  )
  assert.ok(codes(unplanned).includes('parent-unplanned'))
  assert.equal(unplanned.class, 'resumable')

  const rolledUp = epicReverifyVerdict(
    rvInput((input) => (input.parent.state = { name: 'In Progress' })),
  )
  assert.deepEqual(codes(rolledUp), [])
  assert.ok(noticeCodes(rolledUp).includes('parent-state'))

  const noEpic = epicReverifyVerdict(rvInput((input) => (input.parent.labels = [])))
  assert.ok(codes(noEpic).includes('parent-epic-label-missing'))
  assert.equal(noEpic.class, 'needs-human')

  const noState = epicReverifyVerdict(rvInput((input) => delete input.parent.state))
  assert.ok(codes(noState).includes('parent-state-unreadable'))
  assert.equal(noState.class, 'needs-human')
})

test('S6: every forbidden parent label is named; an unmapped agentPlan forbids nothing', () => {
  const all = epicReverifyVerdict(
    rvInput((input) =>
      input.parent.labels.push(
        { name: 'agent-friendly' },
        { name: 'needs-human' },
        { name: 'agent-plan' },
      ),
    ),
  )
  assert.equal(codes(all).filter((c) => c === 'parent-forbidden-label').length, 3)

  const unmapped = epicReverifyVerdict(
    rvInput((input) => {
      input.roles.agentPlan = null
      input.parent.labels.push({ name: 'agent-plan' })
    }),
  )
  assert.deepEqual(codes(unmapped), [])
})

test('S6: the parent holds exactly one spec store and exactly one plan artifact', () => {
  const twoSpecs = epicReverifyVerdict(
    rvInput((input) =>
      input.parent.attachments.push({ id: 'att-spec-2', title: specAttachmentTitle(RV_PARENT) }),
    ),
  )
  assert.ok(codes(twoSpecs).includes('parent-spec-ambiguous'))

  const stalePlan = epicReverifyVerdict(
    rvInput((input) =>
      input.parent.attachments.push({ id: 'att-old', title: `Implementation plan (${RV_PARENT})` }),
    ),
  )
  assert.ok(codes(stalePlan).includes('parent-plan-artifact-count'))

  const staleLink = epicReverifyVerdict(
    rvInput((input) =>
      input.parent.links.push({ id: 'lnk', title: `Implementation plan (${RV_PARENT})`, url: 'x' }),
    ),
  )
  assert.ok(codes(staleLink).includes('parent-plan-artifact-count'))

  const foreignSpec = epicReverifyVerdict(rvInput((input) => (input.spec.parentId = 'BOS-1')))
  assert.ok(codes(foreignSpec).includes('spec-identity'))
})

test('S6: a legacy inline-spec parent passes only while its stored overview kept the marker', () => {
  const legacy = (keep) =>
    rvInput((input) => {
      input.parent.attachments = input.parent.attachments.filter((a) => a.id !== 'att-spec')
      // A legacy spec may predate parentId binding: identity is not checked on this store.
      delete input.spec.parentId
      const marker = keep ? `\n\n${legacyInlineDescription(input.spec).trim()}` : ''
      const text = rvOverview(marker)
      input.parentOverview = { intended: text, stored: text }
    })
  assert.deepEqual(codes(epicReverifyVerdict(legacy(true))), [])
  assert.ok(codes(epicReverifyVerdict(legacy(false))).includes('parent-spec-store-missing'))
})

test('S6: reconcile refusals and unapplied repairs are named blockers', () => {
  const duplicate = epicReverifyVerdict(
    rvInput((input) => setStored(input, 'BOS-902', rvChildBody('c1'))),
  )
  assert.ok(codes(duplicate).includes('reconcile-refused'))

  const renamed = epicReverifyVerdict(
    rvInput((input) => setStored(input, 'BOS-902', rvChildBody('c2-old'))),
  )
  assert.ok(codes(renamed).includes('reconcile-unrepaired'), JSON.stringify(codes(renamed)))
})

test('S6: per-child state, artifact and label conjuncts', () => {
  const blockerOf = (mutate) => codes(epicReverifyVerdict(rvInput(mutate)))
  assert.ok(
    blockerOf((i) => (childOf(i, 'BOS-901').state = { name: 'Unplanned' })).includes(
      'child-unplanned',
    ),
  )
  for (const state of ['In Progress', 'Done', 'Canceled']) {
    const verdict = epicReverifyVerdict(
      rvInput((i) => (childOf(i, 'BOS-901').state = { name: state })),
    )
    assert.deepEqual(codes(verdict), [], state)
    assert.ok(noticeCodes(verdict).includes('child-state'), state)
  }
  const extraQuestion = epicReverifyVerdict(
    rvInput((i) => childOf(i, 'BOS-901').labels.push({ name: 'agent-question' })),
  )
  assert.deepEqual(codes(extraQuestion), [])
  assert.ok(noticeCodes(extraQuestion).includes('child-agent-question-extra'))

  assert.ok(
    blockerOf((i) => (childOf(i, 'BOS-901').attachments = [])).includes(
      'child-plan-artifact-missing',
    ),
  )
  assert.ok(
    blockerOf((i) => childOf(i, 'BOS-901').labels.push({ name: 'needs-human' })).includes(
      'child-exposure-label',
    ),
  )
  assert.ok(blockerOf((i) => (childOf(i, 'BOS-901').labels = [])).includes('child-exposure-label'))
  assert.ok(
    blockerOf(
      (i) =>
        (childOf(i, 'BOS-902').labels = [{ name: 'agent-friendly' }, { name: 'agent-question' }]),
    ).includes('child-exposure-mismatch'),
  )
  assert.ok(
    blockerOf((i) => (childOf(i, 'BOS-902').labels = [{ name: 'needs-human' }])).includes(
      'child-agent-question-missing',
    ),
  )
  assert.ok(
    blockerOf((i) => childOf(i, 'BOS-901').labels.push({ name: 'agent-plan' })).includes(
      'child-forbidden-label',
    ),
  )
})

test('S6: child bodies are contract-checked and write-back verified against the intended bytes', () => {
  const drifted = epicReverifyVerdict(
    rvInput((i) => setStored(i, 'BOS-901', rvChildBody('c1', { notes: 'A different note.\n' }))),
  )
  assert.ok(codes(drifted).includes('child-body-drift'), JSON.stringify(codes(drifted)))

  const empty = epicReverifyVerdict(rvInput((i) => (i.childBodies['BOS-901'].stored = '  ')))
  assert.ok(codes(empty).includes('child-body-unverified'))

  // A difference that is only a declared transform (bullet-marker substitution) passes.
  const tolerated = epicReverifyVerdict(
    rvInput((i) => setStored(i, 'BOS-901', rvChildBody('c1').replace(/^- /gm, '* '))),
  )
  assert.deepEqual(codes(tolerated), [])

  // Byte-identical, so the write-back comparison is tier 1 — only the contract check catches it.
  const malformed = epicReverifyVerdict(
    rvInput((i) => {
      const body = rvChildBody('c1', { drop: '## Summary' })
      setStored(i, 'BOS-901', body)
      i.childBodies['BOS-901'].intended = body
    }),
  )
  assert.deepEqual(codes(malformed), ['child-body-contract'])
})

test('S6: the parent overview is write-back verified in epic-parent mode', () => {
  const drifted = epicReverifyVerdict(
    rvInput((i) => (i.parentOverview.stored = rvOverview().replace(/## Child tickets[^#]*/, ''))),
  )
  assert.ok(codes(drifted).includes('parent-overview-drift'), JSON.stringify(codes(drifted)))
})

test('S6: a malformed overview that round-trips byte-identically still fails its contract', () => {
  const malformed = rvOverview().replace(/## Child tickets[^#]*/, '')
  const verdict = epicReverifyVerdict(
    rvInput((i) => (i.parentOverview = { intended: malformed, stored: malformed })),
  )
  assert.deepEqual(codes(verdict), ['parent-overview-contract'])
  assert.equal(verdict.class, 'needs-human')
})

test('S6: malformed input yields blockers, never a throw', () => {
  for (const input of [undefined, null, 'x', 42, []]) {
    const verdict = epicReverifyVerdict(input)
    assert.equal(verdict.ok, false)
    assert.equal(verdict.class, 'needs-human')
  }
  const nullChildren = epicReverifyVerdict(rvInput((i) => (i.children = null)))
  assert.ok(codes(nullChildren).includes('children-unreadable'))
  const noConfig = epicReverifyVerdict(rvInput((i) => delete i.config))
  assert.ok(codes(noConfig).includes('config-missing'))
  const noRole = epicReverifyVerdict(rvInput((i) => delete i.roles.planned))
  assert.ok(codes(noRole).includes('unresolved-role'))
})
