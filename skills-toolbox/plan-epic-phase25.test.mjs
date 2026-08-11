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
} from './tracker/adapter-core.mjs'
import { buildLinearOperationMap } from './tracker/linear.mjs'

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
  const opMap = buildLinearOperationMap('t')
  // The keys each op's summary declares, e.g. '{issue, assetUrl, title} -> …'.
  const declaredKeys = (op) =>
    new Set(
      (opMap[op].summary.match(/^\{([^}]*)\}/)?.[1] ?? '')
        .split(',')
        .map((part) => part.trim().split(/[=:(]/)[0].trim())
        .filter(Boolean),
    )

  // EMPTY, and it must stay that way. The first version of this check exempted
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
  const DELIBERATELY_OMITTED = { moveState: new Set(['state']) }

  const adapterBacked = phase25Plan().filter((entry) => !NON_ADAPTER_OPS.includes(entry.op))
  assert.ok(adapterBacked.length >= 4, 'expected the adapter-backed ops to be exercised')

  for (const entry of adapterBacked) {
    const declared = declaredKeys(entry.op)
    assert.ok(declared.size > 0, `${entry.op}: could not read declared args from its summary`)

    // (a) Every key emitted is one the adapter actually accepts.
    for (const key of Object.keys(entry.args)) {
      assert.ok(
        declared.has(key) || BEYOND_SUMMARY[entry.op]?.has(key),
        `${entry.op}: emits "${key}", which its adapter summary does not declare (${[...declared].join(', ')})`,
      )
    }
    // (b) Every runtimeArg is a real argument too — not an invented placeholder.
    for (const key of entry.runtimeArgs) {
      assert.ok(declared.has(key), `${entry.op}: runtimeArg "${key}" is not a declared argument`)
    }
    // (c) Together they COVER the op — an argument that is neither emitted nor
    //     declared runtime is one an executor would silently omit.
    const covered = new Set([...Object.keys(entry.args), ...entry.runtimeArgs])
    for (const key of declared) {
      if (DELIBERATELY_OMITTED[entry.op]?.has(key)) {
        assert.ok(
          !(key in entry.args),
          `${entry.op}: "${key}" is pinned as deliberately omitted but is emitted`,
        )
        continue
      }
      assert.ok(
        covered.has(key),
        `${entry.op}: declared argument "${key}" is neither emitted nor listed in runtimeArgs`,
      )
    }
    assert.ok(Array.isArray(entry.runtimeArgs), `${entry.op}: runtimeArgs must always be present`)
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
