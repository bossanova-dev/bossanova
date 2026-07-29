// plan-epic-lib — pure, tracker-agnostic epic-decomposition primitive for boss-plan.
//
// When boss-plan judges a ticket too big for one PR it decomposes it into an
// epic: a parent + N fully-planned children wired by an intra-epic `blockedBy`
// DAG. This module owns the DETERMINISTIC core of that decision — validation,
// cycle safety, stable creation order, and the tracker-write ("wiring") plan —
// so the SKILL prose never re-derives it inline. node builtins only (the cron
// worktree is dependency-free); no Linear client, no I/O, no tracker strings.
// Modelled on this repo's dependency-graph and DAG-scheduling helpers.
//
// The decomposition spec shape (produced by the SKILL, validated here):
//
//   {
//     parent:   { title, goal, keyChanges[] },              // the epic overview
//     children: [
//       { key, title, goal, keyChanges[], blockedByKeys[], estimate, priority, layer?, agentFriendly?, openQuestions? },
//       ...
//     ]
//
// - `agentFriendly` is the child plan's agent-friendliness call (optional;
//   default true). It is NOT structurally validated, but it IS persisted in the
//   spec marker so a fresh-worktree resume can re-derive each ALREADY-created
//   child's deferred-exposure label (`agent-friendly` vs `needs-human`) without
//   the `.linear-plans/` scratch.
// - `openQuestions` is the child plan's list of genuinely controversial open
//   questions (optional). A non-empty list drives the `agent-question` label
//   (the Phase 4 contract). The marker persists it as a derived boolean
//   `agentQuestion` so a fresh-worktree resume re-applies the label without the
//   `.linear-plans/` scratch.
// - `layer` is the child's architectural seam in a vertical/pipeline feature
//   (optional; one of contract|persistence|producer|read|ui). It drives the
//   producer-before-consumer soft check (`validateLayering`): a `read`/`ui` child
//   must be gated by a `producer` sibling (or an external upstream — see R5a
//   recon). Advisory only — it is persisted in the spec marker but never blocks
//   decomposition (`validateLayering` returns warnings, not `errors`).
//   }
//
// - `key` is a STABLE, title-derived slug (see `stableChildKey`) used to express
//   intra-epic `blockedByKeys` edges AND embedded as a `boss-plan-epic-child`
//   resume marker, so a headless retry in a fresh worktree re-derives the same
//   keys and adopts the existing children instead of duplicating them.
// - `blockedByKeys` declares the intended implementation order WITHIN the epic
//   (child B blocked by child A) — the DAG boss-epic later schedules against.
// - The `boss-plan-epic-spec` parent-description marker persists the FULL spec
//   (parent overview + every child's full metadata), so a headless retry
//   completes the ORIGINAL decomposition — recreating each MISSING child from
//   its persisted metadata and wiring the persisted DAG — rather than
//   re-decomposing from scratch (which could build a DIFFERENT partial epic).
//
// API SHAPES (chosen + pinned in plan-epic-lib.test.mjs):
//   validateDecomposition(spec)  -> { ok: boolean, errors: string[] }   (structured, never throws)
//   assertAcyclic(spec)          -> void; THROWS Error naming the cycle path on a cycle
//   topoOrderChildren(spec)      -> child[] in stable topological creation order (THROWS on cycle)
//   epicWiringPlan(spec, createdIdByKey) -> wiring[]  (THROWS on a missing id)
//     where createdIdByKey maps every child `key` -> created tracker id, plus a
//     reserved `parent` entry -> the epic parent's tracker id, and each wiring
//     entry is { key, childId, parentId, blockedBy: [resolvedId, ...] } in topo order.
//   stableChildKey(child, seen?) -> deterministic title-derived slug, unique within
//     `seen` (a colliding slug gets a `-2`, `-3`, … suffix); the STABLE `key` a
//     headless retry re-derives identically so resume markers keep matching.
//   epicSpecMarker(spec)         -> the hidden `<!-- boss-plan-epic-spec:{parent,children:[…]} -->`
//     marker persisted FIRST in the parent description (the durable FULL spec:
//     parent overview + every child's full metadata).
//   parseEpicSpecMarker(description) -> the persisted FULL spec object
//     `{parent, children:[…]}`, or null when the marker is absent or garbled
//     (recovers the ORIGINAL spec on a fresh worktree so resume finishes it).

// Guard constants. MIN is the "fewer ⇒ not an epic, plan as one ticket" floor.
// MAX is the child-count cap: forcing every child to CHILD_MAX_ESTIMATE points
// (below) means a big feature decomposes into MORE small children, so the cap is
// set above the old 8 to give honest ≥5 features room to land as ≤3-pt children
// before the escape valve trips. Over the cap ⇒ `needs-human` ("too large to
// auto-plan; split by hand") — NEVER a single oversized ticket, which is the
// exact monolith this decomposition exists to avoid.
export const EPIC_MIN_CHILDREN = 2
export const EPIC_MAX_CHILDREN = 12
// Keep the durable parent-description marker comfortably below tracker payload
// limits. Child plan bodies are optional retry accelerators: when their
// aggregate would overflow this cap, omit bodies deterministically and let a
// fresh resume redraft those children before attaching them.
export const EPIC_SPEC_MARKER_MAX_BYTES = 256 * 1024
// A single buildable child must land in one reviewable PR, so its estimate is
// hard-capped here: an honest ≥5 unit is not a child, it is decomposed further.
// This is the forcing function — a `5`/`8` child is REJECTED by
// validateDecomposition, so a would-be monolith cannot survive as one epic child.
export const CHILD_MAX_ESTIMATE = 3
// Stable tracker-label role. The skill resolves its display name through
// skill-config.mjs's labelName(config, EPIC_LABEL) accessor.
export const EPIC_LABEL = 'epic'

// `createdIdByKey` reserves this entry for the epic parent's tracker id, so a
// child may not claim it — otherwise its childId would silently resolve to the
// parent's id in epicWiringPlan. validateDecomposition rejects it up front.
const RESERVED_PARENT_KEY = 'parent'

const isNonEmptyString = (v) => typeof v === 'string' && v.trim().length > 0
const asArray = (v) => (Array.isArray(v) ? v : [])

// A planned child's `estimate` must be a Fibonacci point value and its
// `priority` a real planned-state priority (1=Urgent … 4=Low; 0=None is not a planned
// priority) — matching the SKILL.md Phase 4 estimate + priority guidance.
const FIB_ESTIMATES = new Set([0, 1, 2, 3, 5, 8])
const TODO_PRIORITIES = new Set([1, 2, 3, 4])
// The architectural seams a vertical/pipeline feature decomposes along, ordered
// producer-before-consumer: contract → persistence → producer → read → ui. A
// child's `layer` is optional, but when present must be one of these — it drives
// the producer-before-consumer soft check (validateLayering). `read`/`ui` are the
// consumer layers gated on a `producer`.
const CHILD_LAYERS = new Set(['contract', 'persistence', 'producer', 'read', 'ui'])
const CONSUMER_LAYERS = new Set(['read', 'ui'])

/** Return the exact total complexity of an epic's children. */
export function epicParentEstimate(spec) {
  return asArray(spec?.children).reduce((total, child) => total + child?.estimate, 0)
}

/**
 * Structural validation of a decomposition spec. Returns `{ ok, errors }`:
 * `ok` is true iff `errors` is empty (matches the repo's structured-result
 * style; never throws so a caller can surface every problem at once). Rejects:
 *   - fewer than EPIC_MIN_CHILDREN or more than EPIC_MAX_CHILDREN children
 *   - a missing parent overview (parent.title / parent.goal empty) or priority
 *   - duplicate child `key`s
 *   - empty child title/goal (or empty/missing `key`)
 *   - an empty/missing `keyChanges` (must be a non-empty array of non-empty strings)
 *   - an `estimate` outside the Fibonacci set {0,1,2,3,5,8}
 *   - an `estimate` above CHILD_MAX_ESTIMATE (a child must land in one PR; an
 *     honest 5/8 is decomposed further, never carried as one child)
 *   - a `priority` outside {1,2,3,4} (0=None is not a planned-state priority)
 *   - a `layer`, when present, outside {contract,persistence,producer,read,ui}
 *   - a `blockedByKeys` entry referencing an unknown child key (dangling ref)
 * Cycle detection is NOT done here — that is `assertAcyclic`'s job (a dangling
 * ref is structural; a cycle is graph-shaped).
 */
export function validateDecomposition(spec) {
  const errors = []
  if (!spec || typeof spec !== 'object') {
    return { ok: false, errors: ['decomposition spec must be an object'] }
  }

  const parent = spec.parent
  if (!parent || typeof parent !== 'object') {
    errors.push('missing parent overview')
  } else {
    if (!isNonEmptyString(parent.title)) errors.push('parent overview is missing a title')
    if (!isNonEmptyString(parent.goal)) errors.push('parent overview is missing a goal')
    if (!TODO_PRIORITIES.has(parent.priority)) {
      errors.push('parent overview has an invalid priority (must be 1, 2, 3, or 4)')
    }
  }

  const children = spec.children
  if (!Array.isArray(children)) {
    errors.push('children must be an array')
    return { ok: false, errors }
  }

  if (children.length < EPIC_MIN_CHILDREN) {
    errors.push(
      `epic needs at least ${EPIC_MIN_CHILDREN} children (got ${children.length}); plan as a single ticket instead`,
    )
  }
  if (children.length > EPIC_MAX_CHILDREN) {
    errors.push(
      `epic exceeds the ${EPIC_MAX_CHILDREN}-child cap (got ${children.length}); decomposition is pathological`,
    )
  }

  const seen = new Set()
  const keys = new Set()
  for (const child of children) {
    if (child && isNonEmptyString(child.key)) keys.add(child.key)
  }

  children.forEach((child, i) => {
    const where = child && isNonEmptyString(child.key) ? `child "${child.key}"` : `child #${i + 1}`
    if (!child || typeof child !== 'object') {
      errors.push(`${where} must be an object`)
      return
    }
    if (!isNonEmptyString(child.key)) {
      errors.push(`${where} is missing a key`)
    } else {
      if (child.key === RESERVED_PARENT_KEY) {
        errors.push(`${where} uses the reserved key "${RESERVED_PARENT_KEY}"`)
      }
      if (seen.has(child.key)) errors.push(`duplicate child key "${child.key}"`)
      seen.add(child.key)
    }
    if (!isNonEmptyString(child.title)) errors.push(`${where} is missing a title`)
    if (!isNonEmptyString(child.goal)) errors.push(`${where} is missing a goal`)
    // Planning metadata the epic-child spec requires (Phase 2.5 creates planned
    // children from it): a non-empty keyChanges, a Fibonacci estimate, a real
    // planned-state priority. Missing/out-of-range values are rejected BEFORE any write
    // so the epic falls back to a single-ticket plan rather than creating a
    // child with missing metadata or failing mid-writes.
    if (
      !Array.isArray(child.keyChanges) ||
      child.keyChanges.length === 0 ||
      !child.keyChanges.every(isNonEmptyString)
    ) {
      errors.push(`${where} needs a non-empty keyChanges array of non-empty strings`)
    }
    if (!FIB_ESTIMATES.has(child.estimate)) {
      errors.push(
        `${where} has an invalid estimate (must be a Fibonacci value: 0, 1, 2, 3, 5, or 8)`,
      )
    } else if (child.estimate > CHILD_MAX_ESTIMATE) {
      // The forcing function: a child must land in one reviewable PR. An honest
      // 5/8 is a monolith-in-disguise — reject it so the planner decomposes it
      // further into ≤CHILD_MAX_ESTIMATE-pt children rather than smuggling the
      // oversized unit through as a single epic child.
      errors.push(
        `${where} has estimate ${child.estimate}, above the single-PR ceiling of ${CHILD_MAX_ESTIMATE}; decompose it further`,
      )
    }
    if (!TODO_PRIORITIES.has(child.priority)) {
      errors.push(`${where} has an invalid priority (must be 1, 2, 3, or 4)`)
    }
    // `layer` is optional (a non-pipeline child may omit it), but when present it
    // must name a real architectural seam so the producer-before-consumer soft
    // check (validateLayering) can reason about it. An unknown layer is a spec
    // bug — reject it rather than silently ignore a mislabelled seam.
    if (child.layer != null && !CHILD_LAYERS.has(child.layer)) {
      errors.push(
        `${where} has an unknown layer "${child.layer}" (must be one of contract, persistence, producer, read, ui, or omitted)`,
      )
    }
    // `agentFriendly` is optional (omitted ⇒ agent-friendly default), but when
    // present it must be a real boolean. epicSpecMarker persists it as
    // `agentFriendly !== false`, so a truthy non-boolean — e.g. the string
    // "false" from a drafted JSON spec — would be coerced to `true` and a crash
    // before deferred exposure would let resume stamp an otherwise needs-human
    // child `agent-friendly`, making it boss-build-eligible. Reject it here
    // (validate-before-write) so the epic falls back to a single-ticket plan.
    if (child.agentFriendly != null && typeof child.agentFriendly !== 'boolean') {
      errors.push(`${where} has a non-boolean agentFriendly (must be true or false, or omitted)`)
    }
    // A non-array `blockedByKeys` (e.g. the bare string "c1") must be rejected,
    // not silently coerced to []: coercion would drop a real dependency edge and
    // let a malformed epic pass validation with a missing blocker.
    if (child.blockedByKeys != null && !Array.isArray(child.blockedByKeys)) {
      errors.push(`${where} has a non-array blockedByKeys (must be an array of keys)`)
    }
    for (const dep of asArray(child.blockedByKeys)) {
      if (!keys.has(dep)) {
        errors.push(`${where} is blocked by unknown key "${dep}" (dangling blockedByKeys ref)`)
      }
    }
  })

  return { ok: errors.length === 0, errors }
}

/**
 * Producer-before-consumer SOFT check over a decomposition's `layer` tags.
 * Returns `{ warnings }` — never `errors` and never throws — so a layering
 * heuristic can NEVER block decomposition or trip validate-before-write. Two
 * warnings, both advisory:
 *   - a `read`/`ui` child that has a `producer` sibling it does NOT list in
 *     `blockedByKeys` (the consumer would run before its rows are written); and
 *   - a `read`/`ui` child with NO `producer` sibling at all — which MAY be legit
 *     when the producer already exists in the merged tree (R5a recon confirms the
 *     upstream contract), so it is surfaced for the planner to confirm, not blocked.
 * Children without a `layer` tag are ignored (non-pipeline features opt out).
 * @param {object} spec
 * @returns {{ warnings: string[] }}
 */
export function validateLayering(spec) {
  const warnings = []
  const children = asArray(spec?.children).filter((c) => c && typeof c === 'object')
  const producerKeys = children
    .filter((c) => c.layer === 'producer' && isNonEmptyString(c.key))
    .map((c) => c.key)
  const hasProducer = producerKeys.length > 0
  for (const child of children) {
    if (!CONSUMER_LAYERS.has(child.layer)) continue
    const where = isNonEmptyString(child.key) ? `child "${child.key}"` : 'a read/ui child'
    if (!hasProducer) {
      warnings.push(
        `${where} is a ${child.layer} layer with no producer sibling; confirm its rows are written by an already-merged upstream producer (verify the upstream contract exists) or add a producer child`,
      )
      continue
    }
    const gatedByProducer = asArray(child.blockedByKeys).some((dep) => producerKeys.includes(dep))
    if (!gatedByProducer) {
      warnings.push(
        `${where} is a ${child.layer} layer not blockedBy any producer sibling (${producerKeys.join(', ')}); a read/ui child must be gated by the producer that writes its rows`,
      )
    }
  }
  return { warnings }
}

// Adjacency: child key -> the keys it is blocked by (intra-epic edges only).
// Unknown/dangling refs are dropped here so a cycle walk is robust even on a
// spec that has not been structurally validated (validateDecomposition rejects
// the dangling ref separately). Cycle-safe: never recurses past known keys.
function buildAdjacency(spec) {
  const children = asArray(spec?.children)
  const keys = new Set(children.filter((c) => isNonEmptyString(c?.key)).map((c) => c.key))
  const blockedBy = new Map()
  for (const child of children) {
    if (!isNonEmptyString(child?.key)) continue
    blockedBy.set(
      child.key,
      asArray(child.blockedByKeys).filter((dep) => keys.has(dep)),
    )
  }
  return { keys, blockedBy }
}

/**
 * Detect a cycle in the `blockedByKeys` DAG and THROW naming the cycle path
 * (e.g. "c1 -> c2 -> c1"); returns undefined when acyclic. Self-loops count.
 * Iterative DFS with a recursion stack — finite even on a pathological graph.
 */
export function assertAcyclic(spec) {
  const { blockedBy } = buildAdjacency(spec)
  const WHITE = 0
  const GRAY = 1
  const BLACK = 2
  const color = new Map()
  for (const key of blockedBy.keys()) color.set(key, WHITE)

  // DFS carrying an explicit path so a detected back-edge can be reported.
  const visit = (start) => {
    const stack = [{ key: start, deps: blockedBy.get(start) ?? [], i: 0 }]
    color.set(start, GRAY)
    const path = [start]
    while (stack.length > 0) {
      const frame = stack[stack.length - 1]
      if (frame.i < frame.deps.length) {
        const next = frame.deps[frame.i++]
        const c = color.get(next) ?? WHITE
        if (c === GRAY) {
          const from = path.indexOf(next)
          const cycle = [...path.slice(from), next].join(' -> ')
          throw new Error(`epic decomposition has a blockedByKeys cycle: ${cycle}`)
        }
        if (c === WHITE) {
          color.set(next, GRAY)
          path.push(next)
          stack.push({ key: next, deps: blockedBy.get(next) ?? [], i: 0 })
        }
      } else {
        color.set(frame.key, BLACK)
        stack.pop()
        path.pop()
      }
    }
  }

  for (const key of blockedBy.keys()) {
    if ((color.get(key) ?? WHITE) === WHITE) visit(key)
  }
}

/**
 * Stable topological creation order over the `blockedByKeys` DAG: a child is
 * emitted only after every sibling it is blocked by. THROWS (via assertAcyclic)
 * on a cycle. Determinism: Kahn's algorithm draining ready nodes in the spec's
 * original child order, so the same spec always yields the same order. Returns
 * the child OBJECTS (not just keys) so the orchestrator can create them in place.
 */
export function topoOrderChildren(spec) {
  assertAcyclic(spec)
  const children = asArray(spec?.children).filter((c) => isNonEmptyString(c?.key))
  const { blockedBy } = buildAdjacency(spec)
  const order = children.map((c) => c.key) // stable tie-break: original spec order
  const byKey = new Map(children.map((c) => [c.key, c]))

  const remaining = new Map(order.map((key) => [key, [...(blockedBy.get(key) ?? [])]]))
  const done = new Set()
  const result = []
  while (done.size < order.length) {
    // Ready = every blocker already emitted; pick them in original spec order.
    const ready = order.filter(
      (key) => !done.has(key) && remaining.get(key).every((d) => done.has(d)),
    )
    if (ready.length === 0) {
      // Unreachable after assertAcyclic, but fail loud rather than loop forever.
      throw new Error('epic decomposition has an unresolvable blockedByKeys ordering')
    }
    for (const key of ready) {
      done.add(key)
      result.push(byKey.get(key))
    }
  }
  return result
}

/**
 * Emit the deterministic tracker-write plan. `createdIdByKey` maps every child
 * `key` -> its created tracker id, plus a reserved `parent` entry -> the epic
 * parent's tracker id. Returns one entry per child, in topoOrderChildren order:
 *
 *   { key, childId, parentId, blockedBy: [resolvedSiblingId, ...] }
 *
 * `blockedBy` resolves each of the child's `blockedByKeys` through
 * `createdIdByKey`. THROWS if the parent id, a child id, or any referenced
 * blocker id is missing from the map (wiring runs only once every child exists).
 */
export function epicWiringPlan(spec, createdIdByKey) {
  const map = createdIdByKey && typeof createdIdByKey === 'object' ? createdIdByKey : {}
  const parentId = map[RESERVED_PARENT_KEY]
  if (!isNonEmptyString(parentId)) {
    throw new Error('epicWiringPlan: createdIdByKey.parent (the epic parent id) is missing')
  }
  const ordered = topoOrderChildren(spec)
  return ordered.map((child) => {
    const childId = map[child.key]
    if (!isNonEmptyString(childId)) {
      throw new Error(`epicWiringPlan: no created id for child "${child.key}"`)
    }
    const blockedBy = asArray(child.blockedByKeys).map((dep) => {
      const depId = map[dep]
      if (!isNonEmptyString(depId)) {
        throw new Error(
          `epicWiringPlan: child "${child.key}" blocked by "${dep}" which has no created id`,
        )
      }
      return depId
    })
    return { key: child.key, childId, parentId, blockedBy }
  })
}

// Slugify a title into a tracker-safe, deterministic handle: lowercase, every
// run of non-alphanumerics collapsed to one `-`, trimmed of edge dashes.
const slugify = (s) =>
  String(s ?? '')
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, '-')
    .replace(/^-+|-+$/g, '')

/**
 * A STABLE child `key` derived from the child's title — deterministic, so a
 * headless retry in a FRESH cron worktree (the `.linear-plans/` scratch gone)
 * re-derives the identical key and its `boss-plan-epic-child:<key>` marker
 * still matches the already-created child (no duplicate). `seen` accumulates
 * assigned keys across a decomposition; a colliding slug is disambiguated
 * deterministically with a `-2`, `-3`, … suffix (in spec child order). An empty
 * or symbol-only title falls back to `child`. Never returns the reserved
 * `parent` key (that collides with epicWiringPlan's parent slot) — it is
 * suffixed like any other collision.
 * @param {{title?: string}} child
 * @param {Set<string>} [seen]  keys already assigned in this decomposition
 * @returns {string}
 */
export function stableChildKey(child, seen = new Set()) {
  const base = slugify(child?.title) || 'child'
  let key = base === RESERVED_PARENT_KEY ? `${base}-2` : base
  let n = 2
  while (seen.has(key)) {
    key = `${base}-${n++}`
  }
  seen.add(key)
  return key
}

// The hidden marker persisted in the epic PARENT's description carrying the
// FULL decomposition spec (parent overview + every child's full metadata).
// Written as the FIRST tracker write (a description-only mutation that does NOT
// move the parent out of the unplanned state, so the parent-repurpose-LAST atomicity guard
// still holds), it survives a fresh worktree so a headless retry recovers the
// ORIGINAL spec and completes it — recreating each MISSING child from its
// persisted metadata and wiring the persisted DAG — instead of re-decomposing
// (an LLM re-decomposition could build a DIFFERENT partial epic while believing
// it is resuming idempotently). ≤ EPIC_MAX_CHILDREN children of small metadata
// is a few KB — fine inside a Linear description.
// The spec payload is base64-encoded inside the marker (below). base64's alphabet
// (`A-Za-z0-9+/=`) cannot contain `-->` or `}`, so a generated title/goal/keyChange
// text that includes an HTML-comment terminator (e.g. an epic ABOUT comment parsing)
// can never truncate the marker — the failure mode a raw-JSON payload had.
const EPIC_SPEC_MARKER_RE = /<!--\s*boss-plan-epic-spec:([A-Za-z0-9+/=]+)\s*-->/
// Legacy raw-JSON marker (pre-base64). Still parsed on resume so a marker written
// by an earlier build is recovered rather than lost. Disjoint from the base64 form
// (a JSON payload starts with `{`, which is not in the base64 alphabet).
const EPIC_SPEC_MARKER_LEGACY_RE = /<!--\s*boss-plan-epic-spec:(\{[\s\S]*?\})\s*-->/

/**
 * Serialize the FULL decomposition spec into the durable `boss-plan-epic-spec`
 * parent-description marker: the `parent` overview (title, goal, keyChanges) AND
 * every child's full metadata (key, title, goal, keyChanges, blockedByKeys,
 * estimate, priority, layer, agentFriendly, planMarkdown) — everything needed to finish the original
 * epic WITHOUT re-decomposing, INCLUDING each child's deferred-exposure
 * agent-friendliness call so a resume re-stamps an already-created child
 * correctly. Compact JSON inside the same marker.
 * @param {object} spec
 * @returns {string}
 */
export function epicSpecMarker(spec) {
  const parentSrc = spec?.parent && typeof spec.parent === 'object' ? spec.parent : {}
  const parent = {
    title: parentSrc.title,
    goal: parentSrc.goal,
    keyChanges: asArray(parentSrc.keyChanges),
    priority: parentSrc.priority,
  }
  const children = asArray(spec?.children).map((c) => ({
    key: c?.key,
    title: c?.title,
    goal: c?.goal,
    keyChanges: asArray(c?.keyChanges),
    blockedByKeys: asArray(c?.blockedByKeys),
    estimate: c?.estimate,
    priority: c?.priority,
    // Persist the architectural seam so a fresh-worktree resume re-derives the
    // producer-before-consumer wiring for a MISSING child from the recovered
    // spec. Only a known layer is persisted; an absent/unknown one is dropped
    // (JSON omits undefined) and normalizes back to undefined on parse.
    layer: CHILD_LAYERS.has(c?.layer) ? c.layer : undefined,
    // Persist the per-child agent-friendliness decision so a fresh-worktree
    // resume (whose `.linear-plans/` child plans are gone) exposes each
    // ALREADY-created-but-unexposed child correctly — a child whose plan
    // concluded it needs a human (`agentFriendly:false`) is re-stamped
    // `needs-human`, never `agent-friendly`. Without this the marker carried
    // only `priority`, so a crash in the create→expose window left resume
    // unable to recover the call. Default true (agent-friendly) per the
    // plan-contract convention when the spec omits it; only an explicit
    // `false` is a needs-human child.
    agentFriendly: c?.agentFriendly !== false,
    // Persist whether the child plan recorded genuinely controversial open
    // questions, so a fresh-worktree resume re-applies the `agent-question` label
    // (the Phase 4 contract: `openQuestions` non-empty ⇒ `agent-question`) even
    // though the `.linear-plans/` child plan with the full `## Open Questions` is
    // gone. Derived from the spec's `openQuestions` array on the first pass;
    // carried as a boolean on a re-serialized recovered spec. Default false.
    agentQuestion:
      (Array.isArray(c?.openQuestions) && c.openQuestions.length > 0) || c?.agentQuestion === true,
    // The shell-first flow can crash after a child exists but before its native
    // attachment finalizes. Persist the validated plan body with the rest of the
    // resume spec so a fresh worktree can attach that exact plan to an adopted
    // shell rather than exposing a child with no canonical artifact.
    planMarkdown: typeof c?.planMarkdown === 'string' ? c.planMarkdown : undefined,
  }))
  // Base64-encode the compact JSON so no generated text (a title/goal/keyChange
  // containing `} -->`) can inject an HTML-comment terminator and truncate the
  // marker. Full child plans are persisted only while the resulting marker fits
  // safely in a tracker description; omitted bodies trigger the existing resume
  // redraft path rather than risking a failed first save_issue.
  const encode = () => Buffer.from(JSON.stringify({ parent, children }), 'utf8').toString('base64')
  let payload = encode()
  for (
    let index = children.length - 1;
    Buffer.byteLength(payload, 'utf8') > EPIC_SPEC_MARKER_MAX_BYTES && index >= 0;
    index -= 1
  ) {
    delete children[index].planMarkdown
    payload = encode()
  }
  if (Buffer.byteLength(payload, 'utf8') > EPIC_SPEC_MARKER_MAX_BYTES) {
    throw new Error(
      `epic specification exceeds ${EPIC_SPEC_MARKER_MAX_BYTES}-byte tracker marker limit`,
    )
  }
  return `<!-- boss-plan-epic-spec:${payload} -->`
}

/**
 * Recover the persisted FULL spec from a parent description. Returns the parsed
 * `{ parent, children:[…] }` object, or `null` when the marker is absent or
 * garbled (malformed JSON, or a `children` field that is not an array) — the
 * caller then falls back to enumerating children by marker. Resume drafts each
 * missing child from its recovered metadata and wires the recovered DAG, so it
 * completes the ORIGINAL spec rather than a fresh re-decomposition.
 * @param {string} description
 * @returns {{parent: object, children: object[]}|null}
 */
export function parseEpicSpecMarker(description) {
  if (typeof description !== 'string') return null
  // Prefer the base64 marker; fall back to a legacy raw-JSON marker so a spec
  // written by an earlier build still resumes. The two forms are disjoint, so at
  // most one matches.
  let json = null
  const m = description.match(EPIC_SPEC_MARKER_RE)
  if (m) {
    const decoded = Buffer.from(m[1], 'base64').toString('utf8')
    // Guard against a stray base64-looking match that does not round-trip to the
    // same bytes (e.g. non-canonical padding) — treat it as no base64 marker and
    // let the legacy fallback try.
    if (Buffer.from(decoded, 'utf8').toString('base64') === m[1]) json = decoded
  }
  if (json == null) {
    const legacy = description.match(EPIC_SPEC_MARKER_LEGACY_RE)
    if (legacy) json = legacy[1]
  }
  if (json == null) return null
  try {
    const parsed = JSON.parse(json)
    if (!parsed || typeof parsed !== 'object' || !Array.isArray(parsed.children)) return null
    // Normalize the per-child agent-friendliness decision to a definite boolean
    // so the resume/exposure path never has to guess. An OLD marker (written
    // before this field was persisted) simply lacks it and degrades to true
    // (agent-friendly) — the plan-contract default; only an explicit `false`
    // recovers a needs-human child. Non-object entries are left untouched
    // (callers already tolerate a garbled child list).
    for (const child of parsed.children) {
      if (child && typeof child === 'object') {
        child.agentFriendly = child.agentFriendly !== false
        // Normalize the open-questions decision to a definite boolean too. An OLD
        // marker lacks it and degrades to false (no `agent-question`); only an
        // explicit `true` re-applies the label on resume.
        child.agentQuestion = child.agentQuestion === true
        // Old markers legitimately lack the body. Callers must redraft before
        // finalizing an adopted shell in that case; never expose it planless.
        if (typeof child.planMarkdown !== 'string') delete child.planMarkdown
        // Normalize the architectural seam: keep a known layer, drop anything
        // else. An OLD marker (or a garbled value) simply carries no `layer` key,
        // so the child opts out of the producer-before-consumer soft check on
        // resume (delete, not set-undefined, so a recovered child stays minimal).
        if (!CHILD_LAYERS.has(child.layer)) delete child.layer
      }
    }
    // Older markers may carry no parent priority. Choose the
    // defined Medium default so an older in-progress epic can resume safely.
    if (!TODO_PRIORITIES.has(parsed.parent?.priority)) parsed.parent.priority = 3
    return parsed
  } catch {
    return null
  }
}
