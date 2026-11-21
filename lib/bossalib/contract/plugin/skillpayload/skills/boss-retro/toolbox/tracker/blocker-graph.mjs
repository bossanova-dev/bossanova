import { BLOCKER_CLEARED_STATE_TYPES } from './blocker-clearance.mjs'

/** @typedef {{id: string, identifier: string, stateType: string}} BlockerGraphRef
 * @typedef {BlockerGraphRef & {parentId: string|null, blockedBy: BlockerGraphRef[], blocks: BlockerGraphRef[], relationsComplete: boolean}} BlockerGraphNode
 * @typedef {{seeds: string[], maxNodes: number, complete: boolean, nodes: BlockerGraphNode[]}} BlockerGraph
 */
const nonempty = (value) => typeof value === 'string' && value.trim().length > 0
const refValid = (value) =>
  value && nonempty(value.id) && nonempty(value.identifier) && nonempty(value.stateType)

/** Validate the persisted reader contract, including ambiguous aliases. @param {unknown} value @returns {string|null} */
export function blockerGraphDefect(value) {
  const graph = /** @type {BlockerGraph} */ (value)
  if (
    !graph ||
    !Array.isArray(graph.seeds) ||
    !graph.seeds.length ||
    !graph.seeds.every(nonempty) ||
    !Number.isInteger(graph.maxNodes) ||
    graph.maxNodes < 1 ||
    graph.maxNodes > 500 ||
    typeof graph.complete !== 'boolean' ||
    !Array.isArray(graph.nodes) ||
    graph.nodes.length > graph.maxNodes
  )
    return 'invalid graph envelope'
  const aliases = new Map()
  for (const node of graph.nodes) {
    if (
      !refValid(node) ||
      !(node.parentId === null || nonempty(node.parentId)) ||
      typeof node.relationsComplete !== 'boolean' ||
      !['blockedBy', 'blocks'].every((key) => Array.isArray(node[key]) && node[key].every(refValid))
    )
      return 'invalid graph node'
    for (const alias of new Set([node.id, node.identifier])) {
      if (aliases.has(alias)) return 'duplicate graph node alias'
      aliases.set(alias, node)
    }
  }
  if (graph.complete && graph.nodes.some((node) => !node.relationsComplete))
    return 'complete graph has incomplete node'
  return null
}

/** Bounded downstream walk. Aliases resolve to the single recorded node.
 * @param {{seeds: string[], readNode: (id: string) => Promise<BlockerGraphNode>, maxNodes?: number}} options
 * @returns {Promise<BlockerGraph>} */
export async function walkBlockerGraph({ seeds, readNode, maxNodes = 100 }) {
  const graph = { seeds: [...new Set(seeds)], maxNodes, complete: true, nodes: [] }
  const defect = blockerGraphDefect(graph)
  if (defect) throw new Error(defect)
  const queue = [...graph.seeds],
    seen = new Set()
  while (queue.length) {
    const id = queue.shift()
    if (seen.has(id)) continue
    if (graph.nodes.length === maxNodes) {
      graph.complete = false
      break
    }
    const node = await readNode(id)
    const invalid = blockerGraphDefect({ seeds: [id], maxNodes: 1, complete: false, nodes: [node] })
    if (invalid || ![node.id, node.identifier].includes(id))
      throw new Error('unreadable graph node')
    if (seen.has(node.id) || seen.has(node.identifier))
      throw new Error('inconsistent graph node alias')
    seen.add(node.id)
    seen.add(node.identifier)
    graph.nodes.push(node)
    if (!node.relationsComplete) graph.complete = false
    if (BLOCKER_CLEARED_STATE_TYPES.has(node.stateType)) continue
    queue.push(...node.blocks.map((ref) => ref.identifier))
    if (node.parentId) queue.push(node.parentId)
  }
  return graph
}
