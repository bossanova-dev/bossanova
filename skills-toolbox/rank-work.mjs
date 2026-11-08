// skills-toolbox/rank-work.mjs
// Build order from tracker facts only. `rankWork` sorts planned candidates by what the team's tracker
// says matters (priority up the epic chain and through blocks, deadlines, schedule, manual order) and
// never by how easy a ticket looks: estimate, predicted park, confidence and size are not keys.
// A leaf module with node builtins only, vendored into dependency-free installed toolboxes.
//
// Every adapter hands nodes in one shape (Linear's): `priority` (1 Urgent .. 4 Low, 0 None),
// `dueDate`, `prioritySortOrder`, `createdAt`, `parent` (the same fields, nested up the chain),
// `project` ({ id, name, priority, prioritySortOrder, startDate, targetDate, status: { type },
// relations: { nodes: [{ type: 'dependency', relatedAnchorType, relatedProject }] } }), and
// `relations.nodes` ({ type: 'blocks', relatedIssue }) for the tickets this one blocks. A
// `relatedIssue` carries its own facts one level deep; a dependent that is itself a candidate is
// walked further through its own node. A missing fact is neutral: it sorts after known values.

// Walk limit for parent chains and blocks closures; cycles are cut by identity.
export const RANK_WALK_LIMIT = 10

const CLOSED_STATE_TYPES = new Set(['completed', 'canceled', 'duplicate'])
const NONE = 5

const urgency = (priority) =>
  Number.isInteger(priority) && priority >= 1 && priority <= 4 ? priority : NONE
const isDate = (value) => typeof value === 'string' && /^\d{4}-\d{2}-\d{2}/.test(value)
const day = (value) => value.slice(0, 10)
const isOpen = (issue) => !CLOSED_STATE_TYPES.has(issue?.state?.type)
const isOpenProject = (project) => !CLOSED_STATE_TYPES.has(project?.status?.type)

// Earliest first; a missing value sorts after every present one.
const ascending = (a, b) =>
  a == null ? (b == null ? 0 : 1) : b == null ? -1 : a < b ? -1 : a > b ? 1 : 0

/** The more urgent of two tier facts, keeping the first on a tie so the closest source names it. */
const moreUrgent = (best, next) => (urgency(next.tier) < urgency(best.tier) ? next : best)
/** The earlier of two deadline facts; an undated fact never wins. */
const earlier = (best, next) =>
  next.deadline != null && (best.deadline == null || next.deadline < best.deadline) ? next : best

function ancestors(issue) {
  const chain = []
  const seen = new Set([issue?.identifier])
  for (let at = issue?.parent; at && chain.length < RANK_WALK_LIMIT; at = at.parent) {
    if (at.identifier != null && seen.has(at.identifier)) break
    seen.add(at.identifier)
    chain.push(at)
  }
  return chain
}

/**
 * The projects `project` must finish before, through the dependency relations of the projects the
 * candidates carry. A dependent's deadline is the date its anchor names: its start for an end→start
 * dependency (Linear's default), its target for end→end.
 */
function dependentProjects(project, projectsById) {
  const out = []
  const seen = new Set([project?.id])
  const queue = [{ at: project, depth: 0 }]
  while (queue.length > 0) {
    const { at, depth } = queue.shift()
    if (depth >= RANK_WALK_LIMIT) continue
    for (const relation of at?.relations?.nodes ?? []) {
      if (relation?.type !== 'dependency') continue
      const ref = relation.relatedProject
      if (ref?.id == null || seen.has(ref.id) || !isOpenProject(ref)) continue
      seen.add(ref.id)
      const full = projectsById.get(ref.id) ?? ref
      const anchor = relation.relatedAnchorType === 'end' ? full.targetDate : full.startDate
      out.push({ project: full, deadline: isDate(anchor) ? day(anchor) : null })
      queue.push({ at: full, depth: depth + 1 })
    }
  }
  return out
}

/** The tier and deadline an issue carries on its own: itself, its epic chain, its project. */
function ownFacts(issue, projectsById) {
  const label = (project) => project?.name ?? project?.id ?? 'project'
  let tier = { tier: issue?.priority ?? 0, from: 'own' }
  let deadline = { deadline: isDate(issue?.dueDate) ? day(issue.dueDate) : null, from: 'own' }
  for (const epic of ancestors(issue)) {
    tier = moreUrgent(tier, { tier: epic.priority, from: `epic ${epic.identifier}` })
    if (isDate(epic.dueDate))
      deadline = earlier(deadline, { deadline: day(epic.dueDate), from: `epic ${epic.identifier}` })
  }
  const project = issue?.project ? (projectsById.get(issue.project.id) ?? issue.project) : null
  if (project) {
    tier = moreUrgent(tier, { tier: project.priority, from: 'project' })
    if (isDate(project.targetDate))
      deadline = earlier(deadline, { deadline: day(project.targetDate), from: 'project' })
    for (const dependent of dependentProjects(project, projectsById)) {
      const from = `blocks project ${label(dependent.project)}`
      tier = moreUrgent(tier, { tier: dependent.project.priority, from })
      deadline = earlier(deadline, { deadline: dependent.deadline, from })
    }
  }
  return { tier, deadline }
}

/** Open tickets `node` blocks, transitively through dependents that are candidates themselves. */
function blockedClosure(node, byId) {
  const reached = new Map()
  const queue = [{ issue: node, depth: 0 }]
  while (queue.length > 0) {
    const { issue, depth } = queue.shift()
    if (depth >= RANK_WALK_LIMIT) continue
    for (const relation of issue?.relations?.nodes ?? []) {
      if (relation?.type !== 'blocks' || !relation.relatedIssue) continue
      const ref = relation.relatedIssue
      const id = ref.identifier
      if (id == null || id === node.identifier || reached.has(id) || !isOpen(ref)) continue
      const full = byId.get(id) ?? ref
      reached.set(id, full)
      if (byId.has(id)) queue.push({ issue: full, depth: depth + 1 })
    }
  }
  return reached
}

function scheduled(project, today) {
  if (!project) return true
  if (project.status?.type === 'started') return true
  return isDate(project.startDate) && day(project.startDate) <= today
}

/** The rank facts of every candidate: why it sits where it does. */
function rankOf(node, { byId, projectsById, today }) {
  let { tier, deadline } = ownFacts(node, projectsById)
  const closure = blockedClosure(node, byId)
  for (const [id, dependent] of closure) {
    const facts = ownFacts(dependent, projectsById)
    tier = moreUrgent(tier, { ...facts.tier, from: `blocks ${id}` })
    deadline = earlier(deadline, { ...facts.deadline, from: `blocks ${id}` })
  }
  const project = node?.project ? (projectsById.get(node.project.id) ?? node.project) : null
  return {
    tier: urgency(tier.tier) === NONE ? 0 : tier.tier,
    tierFrom: tier.from,
    deadline: deadline.deadline,
    deadlineFrom: deadline.deadline == null ? null : deadline.from,
    scheduled: scheduled(project, today),
    leverage: closure.size,
  }
}

/** Compare shared tracker rank facts, leaving node-level tie breakers to the caller. */
export function compareRank(a, b) {
  return (
    urgency(a.tier) - urgency(b.tier) ||
    ascending(a.deadline, b.deadline) ||
    Number(b.scheduled) - Number(a.scheduled) ||
    b.leverage - a.leverage
  )
}

const todayUtc = () => new Date().toISOString().slice(0, 10)

/**
 * Candidates in build order, each with its `rank` facts. Keys, in order: tier (most urgent of its
 * own priority, every ancestor epic's, its project's, and every open ticket or project it
 * transitively blocks; None is least), deadline (earliest of the same sources' due and target dates,
 * plus a blocked project's start; overdue first, undated last), scheduled (its project has started),
 * leverage (open tickets it transitively unblocks, more first), manual order (project, then issue
 * `prioritySortOrder`), then age (oldest first).
 */
export function rankWork(nodes, { today = todayUtc() } = {}) {
  const list = Array.isArray(nodes) ? nodes : []
  const byId = new Map(list.filter((n) => n?.identifier != null).map((n) => [n.identifier, n]))
  const projectsById = new Map()
  for (const node of list) {
    const project = node?.project
    if (project?.id != null && (!projectsById.has(project.id) || project.relations))
      projectsById.set(project.id, project)
  }
  const ranked = list.map((node) => ({
    ...node,
    rank: rankOf(node, { byId, projectsById, today }),
  }))
  return ranked.sort(
    (a, b) =>
      compareRank(a.rank, b.rank) ||
      ascending(a.project?.prioritySortOrder, b.project?.prioritySortOrder) ||
      ascending(a.prioritySortOrder, b.prioritySortOrder) ||
      ascending(a.createdAt, b.createdAt),
  )
}
