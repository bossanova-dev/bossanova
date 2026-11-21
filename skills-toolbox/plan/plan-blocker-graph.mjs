import { readFileSync } from 'node:fs'
import { parseArgs } from 'node:util'
import { isMainModule } from '../main-module.mjs'
import { blockerGraphDefect } from '../tracker/blocker-graph.mjs'
import {
  DEFAULT_CLEARED_STATE_TYPES,
  DEFAULT_CANCELED_STATE_TYPES,
  transitiveBlockWarnings,
} from './plan-deps-lib.mjs'

export const BLOCKER_SCREEN_REASONS = Object.freeze({
  cycle: 'would-close-cycle',
  incomplete: 'blocker-graph-incomplete',
  missing: 'blocker-graph-missing-node',
  upstream: 'transitive-block-upstream',
  downstream: 'transitive-block-downstream',
})
export const validateBlockerGraph = blockerGraphDefect
const key = (id) => id.toLowerCase()
/** @typedef {{id:string, blockedBy:string[]}} BlockingWrite
 * @typedef {BlockingWrite & {reason:string, via:string[]}} ScreenedWrite */

/** @param {{graph: import('../tracker/blocker-graph.mjs').BlockerGraph, writes: ({id?:string,issue?:string,blockedBy:string[]|string})[], subjectId: string, subjectAgentBuild?: boolean}} input */
export function screenBlockingWrites({ graph, writes, subjectId, subjectAgentBuild }) {
  const defect = validateBlockerGraph(graph)
  if (defect) throw new Error(defect)
  if (!Array.isArray(writes)) throw new Error('writes must be an array')
  const nodes = new Map()
  /** @type {Record<string, import('../tracker/blocker-graph.mjs').BlockerGraphNode>} */
  const relationsById = {}
  for (const node of graph.nodes)
    for (const alias of [node.id, node.identifier]) {
      nodes.set(key(alias), node)
      relationsById[alias] = node
    }
  const canonical = (id) => nodes.get(key(id))?.identifier ?? id
  const settled = new Set([...DEFAULT_CLEARED_STATE_TYPES, ...DEFAULT_CANCELED_STATE_TYPES])
  const pending = new Map()
  const pairs = []
  for (const write of writes) {
    const id = write?.id ?? write?.issue
    const blockers = typeof write.blockedBy === 'string' ? [write.blockedBy] : write.blockedBy
    if (
      typeof id !== 'string' ||
      !id.trim() ||
      !Array.isArray(blockers) ||
      !blockers.length ||
      blockers.some((b) => typeof b !== 'string' || !b.trim())
    )
      throw new Error('invalid blocking write')
    for (const b of blockers) pairs.push({ id: id.trim(), blockedBy: [b.trim()] })
  }
  pairs.sort(
    (a, b) =>
      canonical(a.id).localeCompare(canonical(b.id)) ||
      canonical(a.blockedBy[0]).localeCompare(canonical(b.blockedBy[0])) ||
      a.id.localeCompare(b.id) ||
      a.blockedBy[0].localeCompare(b.blockedBy[0]),
  )
  /** @type {BlockingWrite[]} */ const accepted = []
  /** @type {ScreenedWrite[]} */ const cycleSkips = [],
    held = []
  const seenWrites = new Set()
  for (const write of pairs) {
    const start = canonical(write.id),
      target = canonical(write.blockedBy[0])
    const identity = JSON.stringify([key(start), key(target)])
    if (seenWrites.has(identity)) continue
    seenWrites.add(identity)
    const queue = [[start]],
      visited = new Set()
    let cycle = null,
      gap = null
    while (queue.length) {
      const via = queue.shift(),
        id = via[via.length - 1],
        normalized = key(id)
      if (normalized === key(target)) {
        cycle = via
        break
      }
      if (visited.has(normalized)) continue
      visited.add(normalized)
      const node = nodes.get(normalized)
      if (!node) {
        gap = { reason: BLOCKER_SCREEN_REASONS.missing, via }
        continue
      }
      if (settled.has(node.stateType)) continue
      if (!node.relationsComplete) gap ??= { reason: BLOCKER_SCREEN_REASONS.incomplete, via }
      const downstream = [
        ...node.blocks.map((ref) => ref.identifier),
        ...(node.parentId ? [node.parentId] : []),
        ...(pending.get(key(node.identifier)) ?? []),
      ]
      for (const next of downstream) queue.push([...via, canonical(next)])
    }
    if (cycle) {
      cycleSkips.push({ ...write, reason: BLOCKER_SCREEN_REASONS.cycle, via: cycle })
      continue
    }
    if (!nodes.has(key(target))) gap ??= { reason: BLOCKER_SCREEN_REASONS.missing, via: [target] }
    if (gap) {
      held.push({ ...write, ...gap })
      continue
    }
    accepted.push(write)
    const from = key(target)
    pending.set(from, [...(pending.get(from) ?? []), start])
  }
  const subject = nodes.get(key(subjectId))
  const warningInput = {
    subjectId: subject ? [subject.id, subject.identifier] : subjectId,
    writes: accepted,
    relationsById,
    subjectAgentBuild,
  }
  const warnings = transitiveBlockWarnings(warningInput)
  return {
    accepted,
    cycleSkips,
    held,
    transitiveWarnings: warnings.map((w) => ({
      ...w,
      reason:
        w.direction === 'upstream'
          ? BLOCKER_SCREEN_REASONS.upstream
          : BLOCKER_SCREEN_REASONS.downstream,
    })),
  }
}

/** @param {string[]} argv @param {{stdout?: (s:string)=>void,stderr?: (s:string)=>void}} [io] */
export function main(
  argv,
  { stdout = (s) => process.stdout.write(s), stderr = (s) => process.stderr.write(s) } = {},
) {
  try {
    const cmd = argv[0]
    if (cmd !== 'screen') throw new Error('expected screen verb')
    const { values } = parseArgs({
      args: argv.slice(1),
      strict: true,
      options: {
        writes: { type: 'string' },
        graph: { type: 'string' },
        subject: { type: 'string' },
      },
    })
    if (!values.writes || !values.graph || !values.subject)
      throw new Error('screen requires --writes, --graph and --subject')
    stdout(
      JSON.stringify(
        screenBlockingWrites({
          graph: JSON.parse(readFileSync(values.graph, 'utf8')),
          writes: JSON.parse(readFileSync(values.writes, 'utf8')),
          subjectId: values.subject,
        }),
      ) + '\n',
    )
    return 0
  } catch (error) {
    stderr(`blocker screen: ${error.message}\n`)
    return 1
  }
}
if (isMainModule(import.meta.url)) process.exitCode = main(process.argv.slice(2))
