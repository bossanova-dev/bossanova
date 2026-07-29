// Pure decision logic for the bs-sweep-notes skill. Linear access remains
// injected; the only local import is the repository's mandatory CLI guard.
import { readFileSync } from 'node:fs'

import { isMainModule } from '../skills-toolbox/main-module.mjs'

const FIELD_PREFIXES = {
  where: 'Where:',
  whyItMatters: 'Why it matters:',
  suggestedFix: 'Suggested fix:',
  run: 'Run:',
}

function normalize(value) {
  return String(value ?? '')
    .trim()
    .toLowerCase()
    .replace(/\s+/g, ' ')
}

function normalizePresentation(value) {
  return String(value ?? '')
    .trim()
    .replace(/\s+/g, ' ')
}

function slug(value) {
  return normalize(value)
    .replace(/[^a-z0-9]+/g, '-')
    .replace(/^-+|-+$/g, '')
}

function noteOrder(a, b) {
  const aCreated = String(a?.created_at ?? '')
  const bCreated = String(b?.created_at ?? '')
  if (aCreated !== bCreated) return aCreated < bCreated ? -1 : 1
  return String(a?.id ?? '').localeCompare(String(b?.id ?? ''))
}

/**
 * Extract bs-record-notes fields without requiring a structured body. Unknown
 * or malformed notes become a free-form statement and null optional fields.
 */
export function parseNote(note = {}) {
  const body = typeof note?.body === 'string' ? note.body : ''
  const fields = Object.fromEntries(Object.keys(FIELD_PREFIXES).map((field) => [field, null]))
  const prose = []
  for (const rawLine of body.split(/\r?\n/)) {
    const line = rawLine.trim()
    let matched = false
    for (const [field, prefix] of Object.entries(FIELD_PREFIXES)) {
      if (line.startsWith(prefix)) {
        fields[field] = line.slice(prefix.length).trim() || null
        matched = true
        break
      }
    }
    if (!matched && line) prose.push(line)
  }
  return {
    id: note?.id,
    body,
    created_at: note?.created_at,
    statement: prose.join(' ').trim(),
    ...fields,
  }
}

/**
 * Group notes by normalized statement plus `Where:` target. Clusters and their
 * member notes are sorted deterministically, independent of input ordering.
 */
export function clusterNotes(notes) {
  const groups = new Map()
  for (const note of Array.isArray(notes) ? notes : []) {
    const parsed = parseNote(note)
    const statement = normalize(parsed.statement)
    const where = normalize(parsed.where)
    const identity = `${statement}\u0000${where}`
    if (!groups.has(identity)) {
      const keyParts = [slug(statement) || 'note']
      if (where) keyParts.push(slug(where) || 'where')
      groups.set(identity, {
        identity,
        key: keyParts.join('--'),
        statement: parsed.statement,
        where: parsed.where,
        notes: [],
      })
    }
    groups.get(identity).notes.push(parsed)
  }
  const clusters = [...groups.values()].map((cluster) => {
    const sortedNotes = cluster.notes.sort(noteOrder)
    return {
      ...cluster,
      statement: normalizePresentation(sortedNotes[0]?.statement),
      where: sortedNotes[0]?.where ? normalizePresentation(sortedNotes[0].where) : null,
      notes: sortedNotes,
    }
  })
  return clusters
    .map(({ identity, ...cluster }) => ({
      ...cluster,
      key: `${cluster.key}--${Buffer.from(identity).toString('base64url')}`,
    }))
    .sort((a, b) => a.key.localeCompare(b.key))
}

/**
 * Apply an agent-proposed partition of mechanical cluster keys. The semantic
 * decision is only the grouping; this function validates and deterministically
 * re-derives every merged cluster.
 */
export function mergeClusters(clusters, groups) {
  if (!Array.isArray(clusters) || !Array.isArray(groups)) {
    throw new Error('mergeClusters requires cluster and group arrays')
  }
  const byKey = new Map()
  for (const cluster of clusters) {
    if (!cluster || typeof cluster.key !== 'string' || byKey.has(cluster.key)) {
      throw new Error('mergeClusters received invalid or duplicate cluster keys')
    }
    byKey.set(cluster.key, cluster)
  }
  const seen = new Set()
  const merged = groups.map((group) => {
    if (!Array.isArray(group) || group.length === 0) {
      throw new Error('mergeClusters groups must be non-empty arrays')
    }
    const members = [...group]
      .sort((a, b) => String(a).localeCompare(String(b)))
      .map((key) => {
        if (typeof key !== 'string' || !byKey.has(key) || seen.has(key)) {
          throw new Error(`mergeClusters group has unknown or duplicate key: ${key}`)
        }
        seen.add(key)
        return byKey.get(key)
      })
    const where = normalize(members[0].where)
    if (members.some((cluster) => normalize(cluster.where) !== where)) {
      throw new Error('mergeClusters cannot merge different Where targets')
    }
    return {
      ...members[0],
      sourceKeys: members.map((cluster) => cluster.key),
      notes: members.flatMap((cluster) => cluster.notes).sort(noteOrder),
    }
  })
  if (seen.size !== byKey.size) {
    throw new Error('mergeClusters groups must account for every cluster')
  }
  return merged.sort((a, b) => a.key.localeCompare(b.key))
}

function escapeRegExp(value) {
  return String(value).replace(/[.*+?^${}()|[\]\\]/g, '\\$&')
}

function clusterMarkerKeys(cluster) {
  const canonical = typeof cluster?.key === 'string' ? cluster.key : ''
  if (!canonical) throw new Error('cluster marker requires a non-empty canonical key')
  const sourceKeys = Array.isArray(cluster?.sourceKeys) ? cluster.sourceKeys : []
  return [
    ...new Set(sourceKeys.filter((key) => typeof key === 'string' && key && key !== canonical)),
    canonical,
  ]
}

/** Render every mechanical identity marker, with the canonical marker last. */
export function renderClusterMarkers(cluster) {
  return clusterMarkerKeys(cluster)
    .map((key) => `Notes: ${key}`)
    .join('\n')
}

function carriesMarker(cluster, markedIssues) {
  const markers = clusterMarkerKeys(cluster).map(
    (key) => new RegExp(`^Notes: ${escapeRegExp(key)}[ \\t]*$`, 'm'),
  )
  return (Array.isArray(markedIssues) ? markedIssues : []).some((issue) => {
    const description = String(issue?.description ?? '')
    return markers.some((marker) => marker.test(description))
  })
}

/**
 * Account for every cluster exactly once. Marker-carriers are dropped; up to
 * `cap` remaining clusters are selected, and the rest are deferred.
 */
export function selectClusters(clusters, markedIssues = [], { cap = 5 } = {}) {
  const selected = []
  const deferred = []
  const dropped = []
  const limit = Number.isFinite(Number(cap)) ? Math.max(0, Math.floor(Number(cap))) : 5
  for (const cluster of Array.isArray(clusters) ? clusters : []) {
    if (!cluster || typeof cluster.key !== 'string') continue
    if (carriesMarker(cluster, markedIssues)) {
      dropped.push({ cluster, reason: 'already-tracked' })
    } else if (selected.length < limit) {
      selected.push({ cluster, reason: 'selected' })
    } else {
      deferred.push({ cluster, reason: 'over-cap' })
    }
  }
  return { selected, deferred, dropped }
}

export const MARKED_ISSUES_QUERY = `query Marked($filter: IssueFilter!, $after: String) {
  issues(first: 250, filter: $filter, after: $after, includeArchived: true) {
    nodes { identifier description }
    pageInfo { hasNextPage endCursor }
  }
}`

/** Fetch the complete marker snapshot; refuse partial pagination results. */
export async function fetchMarkedLinearIssues({
  apiKey,
  linearRequest,
  maxPages = 20,
  markerPrefix = 'Notes: ',
} = {}) {
  if (typeof linearRequest !== 'function') {
    throw new Error('fetchMarkedLinearIssues requires a linearRequest implementation')
  }
  const nodes = []
  let after = null
  for (let page = 0; page < maxPages; page++) {
    const data = await linearRequest({
      apiKey,
      query: MARKED_ISSUES_QUERY,
      variables: { filter: { description: { contains: markerPrefix } }, after },
    })
    const connection = data?.issues
    if (!connection) throw new Error('Linear marker scan returned no issues connection')
    if (!Array.isArray(connection.nodes)) {
      throw new Error('Linear marker scan returned malformed issue nodes')
    }
    if (
      !connection.nodes.every(
        (node) =>
          node !== null &&
          typeof node === 'object' &&
          !Array.isArray(node) &&
          typeof node.identifier === 'string' &&
          node.identifier.length > 0 &&
          typeof node.description === 'string',
      )
    ) {
      throw new Error('Linear marker scan returned malformed issue entry')
    }
    if (typeof connection.pageInfo?.hasNextPage !== 'boolean') {
      throw new Error('Linear marker scan returned malformed page info')
    }
    nodes.push(...connection.nodes)
    if (!connection.pageInfo.hasNextPage) return nodes
    if (
      typeof connection.pageInfo.endCursor !== 'string' ||
      connection.pageInfo.endCursor.length === 0
    ) {
      throw new Error('Linear marker scan returned no continuation cursor')
    }
    after = connection.pageInfo.endCursor
  }
  throw new Error(`Linear marker scan exceeded ${maxPages} pages — dedupe snapshot incomplete`)
}

/** Small CLI adapter, injection-friendly for node:test. */
export function runCli(argv, { readFile = (file) => readFileSync(file, 'utf8') } = {}) {
  const [command, ...args] = Array.isArray(argv) ? argv : []
  const readJson = (file) => JSON.parse(readFile(file))
  switch (command) {
    case 'parse':
      return JSON.stringify(
        (Array.isArray(readJson(args[0])) ? readJson(args[0]) : []).map(parseNote),
      )
    case 'cluster':
      return JSON.stringify(clusterNotes(readJson(args[0])))
    case 'merge':
      return JSON.stringify(mergeClusters(readJson(args[0]), readJson(args[1])))
    case 'select':
      return JSON.stringify(
        selectClusters(readJson(args[0]), readJson(args[1]), {
          cap: args[2] === undefined ? undefined : Number(args[2]),
        }),
      )
    default:
      throw new Error(`unknown subcommand: ${command}`)
  }
}

if (isMainModule(import.meta.url)) {
  try {
    process.stdout.write(`${runCli(process.argv.slice(2))}\n`)
  } catch (error) {
    process.stderr.write(`sweep-notes-gate: ${error.message}\n`)
    process.exitCode = 1
  }
}
