// dag-scheduler.mjs
// Pure DAG scheduler for epic orchestration: dependency-graph construction,
// ready-set computation, transitive cascade-skip, and merge-serialization
// ordering. Operates on abstract nodes {id, blockedBy, priority, createdAt}
// with ZERO tracker/repo knowledge — no Linear client, no state-type strings,
// no label/plan-URL shapes, no I/O. node builtins only (mirrors the
// dependency-free cron worktree). The tracker-coupled surface (normalize/
// classify/parse and the state-type mapping) lives in bs-epic-lib.mjs, which
// re-exports these functions so existing importers are unchanged.

// Priority ordering, most- to least-urgent: 1 > 2 > 3 > 4 > 0. A generic total
// order over the integer `priority` each node already carries; callers whose
// tracker encodes priority differently map onto this integer space. Internal —
// not part of the node contract's I/O.
const PRIORITY_ORDER = [1, 2, 3, 4, 0]

function priorityRank(priority) {
  const idx = PRIORITY_ORDER.indexOf(priority)
  return idx === -1 ? PRIORITY_ORDER.length : idx
}

// Sort comparator: most-urgent priority first, ties broken by oldest
// createdAt first (serialized-merge / ready-queue order).
function byPriorityThenOldest(a, b) {
  const rankDiff = priorityRank(a.priority) - priorityRank(b.priority)
  if (rankDiff !== 0) return rankDiff
  const ageDiff = new Date(a.createdAt).getTime() - new Date(b.createdAt).getTime()
  // An absent or unparseable createdAt makes this NaN. The sort spec coerces a NaN
  // comparator result to +0, so a caller that checks `!== 0` before falling through
  // to its own next tie-break would return NaN and skip that level entirely — which
  // is exactly when a stable fallback matters most. Report an unusable timestamp as
  // a genuine tie so the caller's next level actually runs.
  return Number.isNaN(ageDiff) ? 0 : ageDiff
}

/**
 * Builds the dependency graph from abstract nodes, partitioning each node's
 * `blockedBy` into edges inside the node set (must be merged before the node
 * can run) vs. outside it (must be externally cleared).
 * Returns `{nodes: Map<id, node>, inEpicBlockers: Map<id, [ids]>, externalBlockers: Map<id, [ids]>}`.
 */
export function buildGraph(nodes) {
  const nodeMap = new Map(nodes.map((node) => [node.id, node]))
  const inEpicBlockers = new Map()
  const externalBlockers = new Map()
  for (const node of nodes) {
    const blockedBy = node.blockedBy ?? []
    inEpicBlockers.set(
      node.id,
      blockedBy.filter((id) => nodeMap.has(id)),
    )
    externalBlockers.set(
      node.id,
      blockedBy.filter((id) => !nodeMap.has(id)),
    )
  }
  return { nodes: nodeMap, inEpicBlockers, externalBlockers }
}

// Reverse adjacency of the in-set edges: blocker id -> the ids it directly
// blocks. Built the same way for both transitive walks below, so the cascade
// and the unlock count can never disagree about what "depends on" means.
function dependentsIndex(graph) {
  const dependents = new Map()
  for (const [id, blockers] of graph.inEpicBlockers) {
    for (const blockerId of blockers) {
      if (!dependents.has(blockerId)) dependents.set(blockerId, [])
      dependents.get(blockerId).push(id)
    }
  }
  return dependents
}

// Ids reachable downstream of `seedIds` over the in-set edges. Finite even over
// a cyclic graph: each id is added to the result at most once, and only an
// unseen id is enqueued.
function reachableDependents(dependents, seedIds) {
  const result = new Set()
  const queue = [...seedIds]
  while (queue.length > 0) {
    const current = queue.shift()
    for (const dependentId of dependents.get(current) ?? []) {
      if (!result.has(dependentId)) {
        result.add(dependentId)
        queue.push(dependentId)
      }
    }
  }
  return result
}

/**
 * Set of node ids downstream (transitively, in-set only) of any id in
 * `failedIds` — these must be skipped, with the failed ancestor named in the
 * skip reason by the caller. Finite even over a cyclic graph: each id is
 * enqueued at most once.
 *
 * NOT the same question as `transitiveDependentCounts` below, which asks how
 * much a node UNLOCKS. This one asks what a failure POISONS. Two different
 * verdicts over the same edges; keeping them as separate exported names is
 * deliberate.
 */
export function transitiveDependents(graph, failedIds) {
  return reachableDependents(dependentsIndex(graph), failedIds)
}

/**
 * `Map<id, number>` of how many OTHER in-set nodes each node transitively
 * unlocks — the launch-ranking signal: scheduling a high-unlock node first is
 * what keeps the combined graph's critical path moving instead of stalling
 * behind an urgent-but-terminal leaf.
 *
 * Transitive, not direct: a node at the head of a three-deep chain unlocks
 * three, not one. A diamond counts each reachable node once. Scoped strictly to
 * the graph — an external blocker is not a node and contributes nothing, and
 * every graph node gets an entry (0 when it unlocks nothing). Cycle-safe and
 * finite; a node in a cycle can reach itself, and is never counted as its own
 * dependent. Pure: reads nothing but `graph`.
 */
export function transitiveDependentCounts(graph) {
  const dependents = dependentsIndex(graph)
  const counts = new Map()
  for (const id of graph.nodes.keys()) {
    const reachable = reachableDependents(dependents, [id])
    reachable.delete(id)
    counts.set(id, reachable.size)
  }
  return counts
}

// Locale-independent total order over ids, so the final tie-break is stable on
// every machine. `localeCompare` is deliberately avoided: it varies with ICU
// data and locale, which is the opposite of a stable identifier fallback.
function byId(a, b) {
  const left = String(a.id)
  const right = String(b.id)
  if (left < right) return -1
  return left > right ? 1 : 0
}

/**
 * Array of nodes that are ready to start, in LAUNCH order: every in-set blocker
 * is in `merged`, every external blocker is in `externallyCleared`, and the node
 * itself is not merged/failed/in-flight nor a transitive dependent of a failed
 * node (cascade-skip is computed here — the single authority other callers
 * should defer to).
 *
 * Ordering, most- to least-preferred, each level breaking the one above:
 *   1. DESCENDING transitive unlock count (`transitiveDependentCounts`) — start
 *      the work that frees the most other requested work first;
 *   2. tracker priority (the pre-existing `byPriorityThenOldest` order);
 *   3. oldest `createdAt` (same);
 *   4. the stable identifier, so an otherwise-exact tie is deterministic rather
 *      than dependent on node insertion order.
 *
 * Ranking is ordering ONLY. It never admits a node: every exclusion above is
 * applied before the sort, so no unlock score can launch past an uncleared
 * blocker, a cascade-skip, or an in-flight slot.
 */
export function readyTickets(graph, { merged, failed, inFlight, externallyCleared }) {
  const cascadeSkipped = transitiveDependents(graph, failed)
  const ready = []
  for (const [id, node] of graph.nodes) {
    if (merged.has(id) || failed.has(id) || inFlight.has(id) || cascadeSkipped.has(id)) continue
    const inEpicClear = (graph.inEpicBlockers.get(id) ?? []).every((b) => merged.has(b))
    const externalClear = (graph.externalBlockers.get(id) ?? []).every((b) =>
      externallyCleared.has(b),
    )
    if (inEpicClear && externalClear) ready.push(node)
  }
  const unlocks = transitiveDependentCounts(graph)
  return ready.sort((a, b) => {
    const unlockDiff = (unlocks.get(b.id) ?? 0) - (unlocks.get(a.id) ?? 0)
    if (unlockDiff !== 0) return unlockDiff
    const ordered = byPriorityThenOldest(a, b)
    if (ordered !== 0) return ordered
    return byId(a, b)
  })
}

/**
 * Of the given green (passed-review, not-yet-merged) items, returns the single
 * node id that is mergeable right now — every in-set blocker already in
 * `merged` — tie-broken by priority then oldest createdAt (serialized-merge
 * order). Returns null when none are mergeable.
 *
 * Deliberately NOT unlock-ranked, and deliberately without the identifier
 * tie-break `readyTickets` gained: merge order is a documented contract, and a
 * green is already built, so reordering the queue buys no parallelism while
 * changing which PR lands first. Launch order is where unlocking pays.
 */
export function nextToMerge(greens, graph, merged) {
  const mergeable = greens
    .map((green) => graph.nodes.get(green.id) ?? green)
    .filter((node) => (graph.inEpicBlockers.get(node.id) ?? []).every((b) => merged.has(b)))
  if (mergeable.length === 0) return null
  return [...mergeable].sort(byPriorityThenOldest)[0].id
}

/**
 * Merge-time external-blocker re-check, tracker-agnostic. `readyTickets` clears
 * external blockers for LAUNCH via `externallyCleared`, but the serialized merge
 * step must NOT merge past a node's own still-open external gate unless the
 * caller explicitly cleared it FOR MERGE. Given the node about to merge, the
 * graph, the merge-clearance set (`clearedForMerge`, an operator override), and
 * the set of external blocker ids the caller has already resolved
 * (`clearedBlockers`), returns the external blocker ids STILL OPEN — neither
 * cleared-for-merge nor resolved. Empty → safe to merge; non-empty → the merge
 * step skips-with-note. A blocker the caller did not mark cleared is treated as
 * still open (fail-closed). No state-type knowledge: the mapping "which state
 * types count as cleared" lives in the tracker wrapper, which passes a resolved
 * `clearedBlockers` set.
 */
export function mergeBlockedExternalBlockers(
  node,
  graph,
  { clearedForMerge = new Set(), clearedBlockers = new Set() } = {},
) {
  const externals = graph.externalBlockers.get(node.id) ?? []
  return externals.filter((id) => !clearedForMerge.has(id) && !clearedBlockers.has(id))
}
