// scripts/linear-tidy-lib.mjs
// Pure tidy logic + injectable I/O for the bs-sweep-tidy-linear worker gate.
// NO imports beyond node builtins (+ the shared linear-gate-lib) — the cron
// worktree is dependency-free. All decision logic is pure and unit-tested with
// fixtures; the only side effects live in the injectable I/O wrappers so the
// logic runs with zero network under `node --test`.
//
// Two behaviours (see .claude/skills/bs-sweep-tidy-linear/SKILL.md):
//   1. Auto-close: a non-terminal ticket whose matched `[BOS-NNN]` PRs have ALL
//      merged is moved to Done. Any non-merged matched PR (or none) → untouched.
//      Conflicting PR states → escalate, never guess.
//   2. Parent rollup: a parent is moved to reflect its children — started
//      when any child has started (target the least-advanced STARTED open child),
//      else the least-advanced OPEN child. A started, non-terminal parent may move
//      backward only to a started target; terminal and not-started floors are kept.

import { linearRequest } from '../skills-toolbox/linear-gate-lib.mjs'
import { labelName, loadSkillConfig, stateName } from '../skills-toolbox/skill-config.mjs'

// --- State model -----------------------------------------------------------

// Linear's API exposes no position for a workflow status, so the ordering lives
// here, keyed by stable state *role* — the `state.type` cannot distinguish the
// two unstarted planning states, the same lesson as linear-gate-lib.mjs.
// Display names resolve from the repo-local config.
const CONFIG = loadSkillConfig()
export const STATE_RANK = Object.freeze({
  backlog: 0,
  unplanned: 1,
  planned: 2,
  inProgress: 3,
  inReview: 4,
  done: 5,
})
const STATE_NAMES = Object.freeze(
  Object.fromEntries(Object.keys(STATE_RANK).map((role) => [role, stateName(CONFIG, role)])),
)
const LABEL_NAMES = Object.freeze({
  agentPlan: labelName(CONFIG, 'agentPlan'),
  agentFriendly: labelName(CONFIG, 'agentFriendly'),
  needsHuman: labelName(CONFIG, 'needsHuman'),
})

// States that mean "no further tidy applies": a ticket here is never auto-closed
// and, as a child, is ignored when computing a parent rollup target.
const TERMINAL_STATE_NAMES = new Set(
  ['done', 'canceled', 'duplicate'].map((role) => stateName(CONFIG, role)),
)

export function rankOf(stateName) {
  const role = Object.keys(STATE_NAMES).find((key) => STATE_NAMES[key] === stateName)
  return role == null ? null : STATE_RANK[role]
}

export function isTerminalStateName(stateName) {
  return TERMINAL_STATE_NAMES.has(stateName)
}

// The rank at/above which a workflow state means "work has started" — matches
// Linear's `started` state type (In Progress, In Review). Below this (Backlog,
// Unplanned, Todo) the item has not started.
export const STARTED_RANK = STATE_RANK.inProgress

// True for a started, non-terminal state (In Progress / In Review). Used by the
// rollup: a not-started child must not drag a parent below a started sibling.
export function isStartedStateName(stateName) {
  const r = rankOf(stateName)
  return r !== null && r >= STARTED_RANK && !isTerminalStateName(stateName)
}

// --- PR tag parsing --------------------------------------------------------

// Match the Linear id embedded in a PR title as `[BOS-<n>]`, case-insensitively,
// returning every distinct normalized identifier (a title can tag several
// tickets). Brackets are required so a stray `BOS-12` in prose does not match,
// and the `BOS-` prefix means a PR-number tag like `[#123]` never matches.
const TAG_RE = /\[BOS-(\d+)\]/gi

export function parseLinearTag(title) {
  const ids = []
  const seen = new Set()
  for (const m of String(title ?? '').matchAll(TAG_RE)) {
    const id = `BOS-${m[1]}`
    if (!seen.has(id)) {
      seen.add(id)
      ids.push(id)
    }
  }
  return ids
}

// Group PRs (from `gh pr list --json number,title,state,url`) by the Linear
// identifier(s) tagged in each title. A PR tagging several tickets appears under
// each; an untagged PR is dropped. Returns Map<identifier, PR[]>.
export function groupPrsByTicket(prs) {
  const byTicket = new Map()
  for (const pr of Array.isArray(prs) ? prs : []) {
    for (const id of parseLinearTag(pr?.title)) {
      if (!byTicket.has(id)) byTicket.set(id, [])
      byTicket.get(id).push(pr)
    }
  }
  return byTicket
}

const prState = (pr) => String(pr?.state ?? '').toUpperCase()
const prNumbers = (prs) => prs.map((p) => p?.number).filter((n) => n != null)

// --- Behaviour 1: auto-close -----------------------------------------------

// Decide which eligible (non-terminal) tickets to close and which to escalate.
//   close  iff  ≥1 matched PR AND every matched PR is MERGED.
//   skip   when no matched PR, no merged PR, or merged+open (not all merged yet).
//   escalate when matched PRs conflict (some MERGED, some CLOSED-unmerged).
// When `knownIdentifiers` (all identifiers Linear knows, any state) is supplied,
// a merged PR whose tag maps to none of them is escalated as an unknown-ticket
// reference. It is OPT-IN because the gate cannot cheaply enumerate Done tickets,
// so omitting it keeps the default fail-safe (an orphan merged tag is skipped,
// never treated as a wrong-close).
// `blockedIdentifiers` names issues that must NOT be mechanically closed even on
// an all-merged match — an epic parent with ≥1 open child. Such a parent is not
// "done" while its children are open; its state is governed by the rollup rule,
// not its own PR. Skipping it here prevents a wrong-close (parent → Done with open
// children) and a double-write (close then rollup clobbering the same id).
export function computeCloseable(
  tickets,
  prsByTicket,
  { knownIdentifiers, blockedIdentifiers } = {},
) {
  const close = []
  const escalate = []
  const map = prsByTicket instanceof Map ? prsByTicket : new Map()
  const blocked =
    blockedIdentifiers instanceof Set ? blockedIdentifiers : new Set(blockedIdentifiers ?? [])

  for (const ticket of Array.isArray(tickets) ? tickets : []) {
    const name = ticket?.state?.name
    if (isTerminalStateName(name)) continue // defensive: caller passes non-terminal
    if (blocked.has(ticket.identifier)) continue // parent mid-epic → rollup governs it, never auto-close
    const prs = map.get(ticket.identifier) ?? []
    if (prs.length === 0) continue // no matched PR → never close

    const states = prs.map(prState)
    const mergedCount = states.filter((s) => s === 'MERGED').length
    if (mergedCount === 0) continue // nothing merged → no positive close signal

    if (states.every((s) => s === 'MERGED')) {
      close.push({
        id: ticket.id,
        identifier: ticket.identifier,
        from: name,
        prs: prNumbers(prs),
      })
    } else if (states.some((s) => s === 'CLOSED')) {
      escalate.push({
        kind: 'close-conflict',
        identifier: ticket.identifier,
        reason: 'matched PRs conflict: some MERGED, some CLOSED without merge',
        prs: prNumbers(prs),
      })
    }
    // else: merged + open → not all merged yet; skip (a later run closes it).
  }

  if (knownIdentifiers) {
    const known = knownIdentifiers instanceof Set ? knownIdentifiers : new Set(knownIdentifiers)
    for (const [identifier, prs] of map) {
      if (known.has(identifier)) continue
      if (prs.some((p) => prState(p) === 'MERGED')) {
        escalate.push({
          kind: 'unknown-ticket',
          identifier,
          reason: 'merged PR tags a ticket not found in Linear',
          prs: prNumbers(prs),
        })
      }
    }
  }

  return { close, escalate }
}

// --- Behaviour 2: parent rollup --------------------------------------------

// For each parent, roll it to reflect its non-terminal children (children
// in a terminal state are ignored). Started when any child has started: if any open
// child is In Progress/In Review, target the least-advanced STARTED open child so a
// not-started sibling never drags it down; otherwise the least-advanced open child.
// Regress only within started, non-terminal states: both the parent and target must
// satisfy isStartedStateName, so terminal parents stay closed and not-started
// children never drag a started parent backward.
//   move     when the parent trails its target, or both started-state guards permit regression.
//   skip     when the parent equals its target, a regression guard refuses, or no child is open.
//   escalate when an open child (or the parent) is in an unrankable state.
export function computeRollups(parents) {
  const move = []
  const escalate = []

  for (const parent of Array.isArray(parents) ? parents : []) {
    // If the child page was truncated (>100 children), the least-advanced open
    // child may be unread — computing a target from the visible subset could roll
    // the parent PAST its true least-advanced child (a wrong forward move), or
    // produce a wrong backward move. Refuse to guess on a partial view.
    if (parent?.childrenTruncated) {
      escalate.push({
        kind: 'rollup-children-truncated',
        identifier: parent.identifier,
        reason:
          'parent has more children than one read page (>100); cannot determine least-advanced open child safely',
      })
      continue
    }
    const children = Array.isArray(parent?.children) ? parent.children : []
    const open = children.filter((c) => !isTerminalStateName(c?.state?.name))
    if (open.length === 0) continue // no open children → nothing to roll up to

    const unrankable = open.find((c) => rankOf(c?.state?.name) === null)
    if (unrankable) {
      escalate.push({
        kind: 'rollup-unrankable-child',
        identifier: parent.identifier,
        reason: `open child ${unrankable.identifier} in unrankable state "${unrankable?.state?.name}"`,
      })
      continue
    }

    const parentRank = rankOf(parent?.state?.name)
    if (parentRank === null) {
      escalate.push({
        kind: 'rollup-unrankable-parent',
        identifier: parent.identifier,
        reason: `parent in unrankable state "${parent?.state?.name}"`,
      })
      continue
    }

    // "Started when any child is started; not finished until all are finished."
    // A not-started child (Backlog/Unplanned/Todo) must NOT pull the parent below a
    // started sibling, so when any open child has started we target the
    // least-advanced STARTED open child; otherwise the least-advanced open child.
    // (All open children are already rankable here — unrankable ones escalated above.)
    const started = open.filter((c) => isStartedStateName(c.state.name))
    const pool = started.length > 0 ? started : open
    let target = pool[0]
    for (const c of pool) {
      if (rankOf(c.state.name) < rankOf(target.state.name)) target = c
    }
    const targetRank = rankOf(target.state.name)

    if (parentRank < targetRank) {
      move.push({
        id: parent.id,
        identifier: parent.identifier,
        from: parent.state.name,
        to: target.state.name,
        toStateId: target.state.id,
      })
    } else if (
      parentRank > targetRank &&
      isStartedStateName(parent.state.name) &&
      isStartedStateName(target.state.name)
    ) {
      move.push({
        id: parent.id,
        identifier: parent.identifier,
        from: parent.state.name,
        to: target.state.name,
        toStateId: target.state.id,
      })
    }
    // else: equal rank, terminal parent, or not-started regression target → skip.
  }

  return { move, escalate }
}

// --- Behaviour 3: planning-queue reconcile ---------------------------------

// True when an issue carries a native boss-plan implementation-plan attachment.
// Keep this legacy URL rejection only for migration/replanning: a link-only historical
// artifact is queued for agent-plan instead of being promoted to Todo, where boss-build
// would reject it for lacking a native attachment.
export function hasImplementationPlan(identifier, attachments) {
  const expectedTitle = `Implementation plan (${String(identifier ?? '')})`
  return (Array.isArray(attachments) ? attachments : []).some((a) => {
    const title = String(a?.title ?? '')
    const url = String(a?.url ?? '')
    if (url.includes('proof.bossanova.dev/plans/')) return false
    return title === expectedTitle
  })
}

const hasLabel = (ticket, name) =>
  (Array.isArray(ticket?.labels) ? ticket.labels : []).some((l) => l?.name === name)

// Decide the planning-queue reconcile for each Unplanned + `agent-friendly` ticket:
//   has an implementation plan  → move Unplanned → Todo (anomaly recovery: boss-plan
//                                 normally moves atomically, so this only fires on a
//                                 partial-failure or a manual reopen).
//   no plan                     → drop `agent-friendly`, add `agent-plan` so the
//                                 bs-sweep-plan queue picks it up. `newLabelIds` is
//                                 the full replacement set (current − agent-friendly
//                                 + agent-plan).
// Excludes `needs-human` (must never auto-plan), an already-`agent-plan` ticket
// (no-op), and any identifier in `blockedIdentifiers` (an epic parent with children —
// don't auto-plan an epic). Config gaps are escalated, never guessed: a no-plan case
// with no resolvable `agentPlanId`, or a has-plan case with no `todoStateId`.
export function computePlanningReconcile(
  tickets,
  { agentPlanId, agentFriendlyId, todoStateId, blockedIdentifiers } = {},
) {
  const toTodo = []
  const toAgentPlan = []
  const escalate = []
  const blocked =
    blockedIdentifiers instanceof Set ? blockedIdentifiers : new Set(blockedIdentifiers ?? [])

  for (const ticket of Array.isArray(tickets) ? tickets : []) {
    if (ticket?.state?.name !== STATE_NAMES.unplanned) continue
    if (!hasLabel(ticket, LABEL_NAMES.agentFriendly)) continue
    if (hasLabel(ticket, LABEL_NAMES.needsHuman)) continue // never auto-plan a needs-human ticket
    if (hasLabel(ticket, LABEL_NAMES.agentPlan)) continue // already queued → no-op
    if (blocked.has(ticket.identifier)) continue // epic parent → don't auto-plan an epic

    if (hasImplementationPlan(ticket.identifier, ticket.attachments)) {
      if (!todoStateId) {
        escalate.push({
          kind: 'no-todo-state',
          identifier: ticket.identifier,
          reason: `planned ${STATE_NAMES.unplanned} ticket but workspace exposes no ${STATE_NAMES.planned} workflow state`,
        })
        continue
      }
      toTodo.push({ id: ticket.id, identifier: ticket.identifier, from: ticket.state.name })
      continue
    }

    if (!agentPlanId) {
      escalate.push({
        kind: 'no-agent-plan-label',
        identifier: ticket.identifier,
        reason: `${LABEL_NAMES.agentFriendly} ${STATE_NAMES.unplanned} ticket to queue but the ${LABEL_NAMES.agentPlan} label does not exist`,
      })
      continue
    }
    // Full replacement set: keep every current label except agent-friendly, add
    // agent-plan (dedup in case it somehow co-exists). Drop null/undefined ids.
    const currentIds = (Array.isArray(ticket.labels) ? ticket.labels : [])
      .map((l) => l?.id)
      .filter((id) => id != null)
    const kept = currentIds.filter((id) => id !== agentFriendlyId && id !== agentPlanId)
    toAgentPlan.push({
      id: ticket.id,
      identifier: ticket.identifier,
      from: ticket.state.name,
      newLabelIds: [...kept, agentPlanId],
    })
  }

  return { toTodo, toAgentPlan, escalate }
}

// --- GraphQL read shape ----------------------------------------------------

// One combined read covers all three behaviours (verified live against the workspace):
//   - workflowStates → name→id map (the Done target for auto-close, the Todo target
//     for the planning-queue reconcile)
//   - non-terminal issues + their (bounded) children → close tickets AND rollup
//     parents in a single query. children(first: 100) bounds the page explicitly
//     (the linear-deps-lib.mjs lesson: an unstated size can paginate real nodes
//     out and silently under-report).
//   - each issue's labels + attachments → the planning-queue reconcile (behaviour 3)
//     needs the label set (agent-friendly / needs-human / agent-plan) and whether an
//     implementation-plan attachment is present. Bounded pages for the same reason.
//   - issueLabels(agent-plan, agent-friendly) → the label ids the relabel mutation
//     needs, resolved once per read rather than per issue.
export const TIDY_READ_QUERY = `
  query TidyReads($first: Int!) {
    workflowStates(first: 100) {
      nodes { id name type }
    }
    issueLabels(first: 50, filter: { name: { in: ${JSON.stringify([LABEL_NAMES.agentPlan, LABEL_NAMES.agentFriendly])} } }) {
      nodes { id name }
    }
    issues(first: $first, filter: { state: { type: { in: ["backlog", "unstarted", "started"] } } }) {
      nodes {
        id
        identifier
        state { id name type }
        labels(first: 20) { nodes { id name } }
        attachments(first: 50) { nodes { title url } }
        children(first: 100) {
          nodes { id identifier state { id name type } }
          pageInfo { hasNextPage }
        }
      }
      pageInfo { hasNextPage }
    }
  }
`

const MOVE_MUTATION = `
  mutation TidyMove($id: String!, $stateId: String!) {
    issueUpdate(id: $id, input: { stateId: $stateId }) { success }
  }
`

// Full-set label replace for the planning-queue reconcile. Linear's issueUpdate
// `labelIds` replaces the issue's entire label set, so the caller computes the new
// set (drop agent-friendly, add agent-plan) and sends it whole — the same semantics
// as the Linear MCP `save_issue labels`.
const LABELS_MUTATION = `
  mutation TidyRelabel($id: String!, $labelIds: [String!]!) {
    issueUpdate(id: $id, input: { labelIds: $labelIds }) { success }
  }
`

// Parse the combined read into the shapes the pure functions consume. Tickets
// are every non-terminal issue; parents are the subset carrying children.
//
// Single-team assumption: Linear workflow states are team-scoped, so `doneStateId`
// (and a rollup's `toStateId`) belong to whatever team owns that state. This
// workspace is single-team (every id is `BOS-NNN`), like the sibling gate libs, so
// one Done state applies to all issues. If a second team is ever added, a cross-team
// `issueUpdate` is rejected by Linear → runTidy throws → fail-closed (no wrong write);
// the read would then need a per-team state map or a team-scoped filter.
export function parseTidyData(data) {
  const stateNodes = data?.workflowStates?.nodes ?? []
  const statesByName = new Map(stateNodes.map((s) => [s.name, s]))
  const doneState =
    statesByName.get(STATE_NAMES.done) ?? stateNodes.find((s) => s?.type === 'completed')

  const labelNodes = data?.issueLabels?.nodes ?? []
  const labelIdsByName = new Map(labelNodes.map((l) => [l.name, l.id]))

  const issueNodes = data?.issues?.nodes ?? []
  // Tickets carry their labels + attachments so behaviour 3 (planning-queue
  // reconcile) can read the label set and detect an implementation-plan attachment
  // without a second read. Behaviours 1 & 2 ignore the extra fields.
  const tickets = issueNodes.map((n) => ({
    id: n.id,
    identifier: n.identifier,
    state: n.state,
    labels: (n?.labels?.nodes ?? []).map((l) => ({ id: l.id, name: l.name })),
    attachments: (n?.attachments?.nodes ?? []).map((a) => ({ title: a.title, url: a.url })),
  }))
  const parents = issueNodes
    .filter((n) => (n?.children?.nodes?.length ?? 0) > 0)
    .map((n) => ({
      id: n.id,
      identifier: n.identifier,
      state: n.state,
      // True when this parent has more children than the single child page read.
      childrenTruncated: Boolean(n?.children?.pageInfo?.hasNextPage),
      children: n.children.nodes.map((c) => ({
        id: c.id,
        identifier: c.identifier,
        state: c.state,
      })),
    }))

  return {
    tickets,
    parents,
    statesByName,
    doneStateId: doneState?.id ?? null,
    todoStateId: statesByName.get(STATE_NAMES.planned)?.id ?? null,
    labelIdsByName,
    hasNextPage: Boolean(data?.issues?.pageInfo?.hasNextPage),
  }
}

// --- Injectable I/O wrappers ------------------------------------------------

// Default `gh` PR read: one repo-scoped title search over ALL states. Injectable
// so tests never shell out. Returns the parsed PR array.
export function defaultReadPullRequests({ execFileSync, limit = 800 } = {}) {
  const out = execFileSync(
    'gh',
    [
      'pr',
      'list',
      '--search',
      '[BOS- in:title',
      '--state',
      'all',
      '--limit',
      String(limit),
      '--json',
      'number,title,state,url',
    ],
    { encoding: 'utf8', maxBuffer: 64 * 1024 * 1024 },
  )
  return JSON.parse(out)
}

// --- Orchestrator ----------------------------------------------------------

// Run the whole tidy: ONE gh read + ONE Linear read, compute deltas, then apply
// (unless dryRun) and report. I/O is injected so the budget (read call counts)
// and every branch are asserted with fixtures and zero network.
//
// Contract honoured by the gate:
//   - Reads happen BEFORE any write. A read/credential failure therefore mutates
//     nothing (fail-closed with no partial mutation).
//   - Returns { closed, rolledUp, reclassified:{toTodo,toAgentPlan}, escalations,
//     counts:{prReads, linearReads, writes} }.
export async function runTidy({
  apiKey,
  dryRun = false,
  first = 250,
  readPullRequests,
  linearReadImpl,
  linearWriteImpl,
} = {}) {
  if (!apiKey) throw new Error('LINEAR_API_KEY is not set')
  if (typeof readPullRequests !== 'function') throw new Error('readPullRequests impl required')
  if (typeof linearReadImpl !== 'function') throw new Error('linearReadImpl impl required')

  const counts = { prReads: 0, linearReads: 0, writes: 0 }

  // --- Reads (before any write) ---
  counts.prReads += 1
  const prs = await readPullRequests()

  counts.linearReads += 1
  const data = await linearReadImpl({ query: TIDY_READ_QUERY, variables: { first } })
  const { tickets, parents, doneStateId, todoStateId, labelIdsByName, hasNextPage } =
    parseTidyData(data)

  // --- Compute deltas ---
  const prsByTicket = groupPrsByTicket(prs)
  // A parent with ≥1 open child (or a truncated child page we can't fully see) is
  // not mechanically closeable: its state is owned by the rollup rule, not its own
  // PR. Block it from auto-close so we never close an epic over open children or
  // double-write the same id in the apply phase.
  const blockedFromClose = new Set(
    parents
      .filter(
        (p) =>
          p.childrenTruncated ||
          (Array.isArray(p.children) ? p.children : []).some(
            (c) => !isTerminalStateName(c?.state?.name),
          ),
      )
      .map((p) => p.identifier),
  )
  const { close, escalate: closeEscalate } = computeCloseable(tickets, prsByTicket, {
    blockedIdentifiers: blockedFromClose,
  })
  const { move, escalate: rollupEscalate } = computeRollups(parents)

  // Behaviour 3: reclassify Unplanned + agent-friendly tickets into the planning
  // queue. An epic parent (already blocked from auto-close) must not be auto-planned
  // either, so reuse blockedFromClose as the block set.
  const {
    toTodo,
    toAgentPlan,
    escalate: reconcileEscalate,
  } = computePlanningReconcile(tickets, {
    agentPlanId: labelIdsByName.get(LABEL_NAMES.agentPlan),
    agentFriendlyId: labelIdsByName.get(LABEL_NAMES.agentFriendly),
    todoStateId,
    blockedIdentifiers: blockedFromClose,
  })

  const escalations = [...closeEscalate, ...rollupEscalate, ...reconcileEscalate]

  // The issue read is one bounded page. If the non-terminal issue set exceeds it,
  // the unread tail would silently under-tidy (tickets never close, parents never
  // roll up). Surface it as a residual so the agent wakes rather than a clean run
  // masking the gap — the same "never silently under-report" bias as the query's
  // explicit page bounds.
  if (hasNextPage) {
    escalations.push({
      kind: 'board-truncated',
      identifier: '(board)',
      reason: `non-terminal issue set exceeds one read page (first=${first}); the remainder was not read this run`,
    })
  }

  // A closeable ticket with no resolvable Done state is a config problem, not a
  // safe mechanical close → escalate rather than guess.
  const closeable = []
  for (const c of close) {
    if (doneStateId) closeable.push(c)
    else
      escalations.push({
        kind: 'no-done-state',
        identifier: c.identifier,
        reason: `workspace exposes no ${STATE_NAMES.done} workflow state`,
      })
  }

  const closed = []
  const rolledUp = []
  const movedToTodo = []
  const queuedForPlan = []

  // --- Apply (unless dry-run) ---
  if (!dryRun) {
    if (typeof linearWriteImpl !== 'function')
      throw new Error('linearWriteImpl impl required to apply')
    for (const c of closeable) {
      counts.writes += 1
      const res = await linearWriteImpl({
        query: MOVE_MUTATION,
        variables: { id: c.id, stateId: doneStateId },
      })
      if (res?.issueUpdate?.success === false) throw new Error(`failed to close ${c.identifier}`)
      closed.push({ ...c, to: STATE_NAMES.done })
    }
    for (const m of move) {
      counts.writes += 1
      const res = await linearWriteImpl({
        query: MOVE_MUTATION,
        variables: { id: m.id, stateId: m.toStateId },
      })
      if (res?.issueUpdate?.success === false) throw new Error(`failed to roll up ${m.identifier}`)
      rolledUp.push(m)
    }
    // Behaviour 3 writes: planned-but-Unplanned → Todo; unplanned no-plan → agent-plan.
    for (const t of toTodo) {
      counts.writes += 1
      const res = await linearWriteImpl({
        query: MOVE_MUTATION,
        variables: { id: t.id, stateId: todoStateId },
      })
      if (res?.issueUpdate?.success === false)
        throw new Error(`failed to move ${t.identifier} to Todo`)
      movedToTodo.push({ ...t, to: STATE_NAMES.planned })
    }
    for (const t of toAgentPlan) {
      counts.writes += 1
      const res = await linearWriteImpl({
        query: LABELS_MUTATION,
        variables: { id: t.id, labelIds: t.newLabelIds },
      })
      if (res?.issueUpdate?.success === false)
        throw new Error(`failed to queue ${t.identifier} for planning`)
      queuedForPlan.push({ ...t, to: LABEL_NAMES.agentPlan })
    }
  } else {
    closed.push(...closeable.map((c) => ({ ...c, to: STATE_NAMES.done })))
    rolledUp.push(...move)
    movedToTodo.push(...toTodo.map((t) => ({ ...t, to: STATE_NAMES.planned })))
    queuedForPlan.push(...toAgentPlan.map((t) => ({ ...t, to: LABEL_NAMES.agentPlan })))
  }

  return {
    closed,
    rolledUp,
    reclassified: { toTodo: movedToTodo, toAgentPlan: queuedForPlan },
    escalations,
    counts,
  }
}

// Real-network I/O defaults for the gate (thin wrappers over linearRequest).
export function makeLinearRead(apiKey, fetchImpl = fetch) {
  return ({ query, variables }) => linearRequest({ apiKey, query, variables, fetchImpl })
}
export function makeLinearWrite(apiKey, fetchImpl = fetch) {
  return ({ query, variables }) => linearRequest({ apiKey, query, variables, fetchImpl })
}
