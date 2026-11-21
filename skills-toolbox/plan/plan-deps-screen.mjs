import {
  screenBlockingWrites,
  validateBlockerGraph,
  BLOCKER_SCREEN_REASONS,
} from './plan-blocker-graph.mjs'

/** Apply the graph screen to classification output before any write escapes. */
export function screenDependencyResult(result, { graph, subjectId, subjectAgentBuild }) {
  const writes = result.edges
    .filter((edge) => edge.write && edge.edge !== 'relatedTo')
    .map((edge) => edge.write)
  const refuse = (code, message) => ({ status: 'refused', refusals: [{ code, message }] })
  if (graph !== undefined) {
    const defect = validateBlockerGraph(graph)
    if (defect)
      return refuse(
        'blocker-graph-not-fetch-blocker-graph',
        `${defect}; write the graph with tracker/cli.mjs fetch-blocker-graph`,
      )
  }
  if (writes.length && graph === undefined) {
    const seeds = [
      ...new Set([subjectId, ...writes.flatMap((write) => [write.id, ...write.blockedBy])]),
    ].sort()
    return refuse(
      'blocker-graph-required',
      `blocking writes require tracker/cli.mjs fetch-blocker-graph; seed ids: ${seeds.join(', ')}`,
    )
  }
  if (graph === undefined) return null
  const screen = screenBlockingWrites({ graph, writes, subjectId, subjectAgentBuild })
  result.screen = {
    accepted: screen.accepted.length,
    cycleSkips: screen.cycleSkips.length,
    held: screen.held.length,
  }
  for (const item of [...screen.cycleSkips, ...screen.held]) {
    for (const edge of result.edges) {
      if (edge.write?.id === item.id && edge.write.blockedBy?.includes(item.blockedBy[0])) {
        edge.write = null
        edge.screen = item.reason
      }
    }
    const cycle = item.reason === BLOCKER_SCREEN_REASONS.cycle
    const text = cycle
      ? `Blocking write skipped: ${item.blockedBy[0]} → ${item.id} would close a cycle via ${item.via.join(' → ')}.`
      : `Blocking write held: ${item.reason} at ${item.via.join(' → ')}; re-run fetch-blocker-graph with --id ${item.via[item.via.length - 1]} or a higher --max-nodes.`
    result.notes.push({
      severity: 'warning',
      destination: 'risks',
      id: item.id,
      reason: item.reason,
      text,
    })
    if (!cycle) result.questions.push({ id: item.id, reason: item.reason, text })
  }
  for (const warning of screen.transitiveWarnings) {
    const text =
      warning.direction === 'upstream' ? `Transitive-block warning: ${warning.text}` : warning.text
    result.notes.push({
      ...warning,
      destination: warning.direction === 'upstream' ? 'risks' : 'planning',
      text,
    })
    if (warning.severity === 'escalated') result.questions.push({ ...warning, text })
  }
  return null
}
