// Pure decision logic for the bs-sweep-notes skill. Linear access, and the
// filesystem/git probes the staleness signals need, remain injected — the
// concrete implementations live only as `runCli` defaults, alongside the
// existing `readFile` default. The only local import is the repository's
// mandatory CLI guard.
import { spawnSync } from 'node:child_process'
import { createHash } from 'node:crypto'
import { existsSync, readFileSync } from 'node:fs'

import { isMainModule } from '../skills-toolbox/main-module.mjs'

/**
 * Longest slug segment kept in a marker key. The segments are there to make a
 * key readable; the digest below is what carries identity, so truncating a
 * segment cannot merge two distinct notes.
 */
export const MAX_SLUG_SEGMENT = 80

/**
 * Hex digits of the identity digest appended to every marker key. Sixteen hex
 * is 64 bits — at backlog scale (hundreds of notes) the collision probability
 * is vanishingly small, and the key stays short enough to query and to embed
 * dozens of times in one issue description.
 */
export const KEY_DIGEST_LENGTH = 16

/** Longest agent-supplied theme title accepted by `mergeClusters`. */
export const MAX_TITLE_LENGTH = 200

/** The complete verdict vocabulary `applyVerdicts` accepts. */
export const VERDICTS = ['live', 'fixed', 'unverifiable']

/**
 * Themes selected per run when neither an explicit argument nor
 * `BS_SWEEP_NOTES_MAX_ISSUES` says otherwise. The library default and the
 * skill's documented default are the same number on purpose: when they
 * disagreed, the skill passed its own value on every call and the library
 * default was dead code that still read as authoritative.
 */
export const DEFAULT_CAP = 15

/** Live themes older than this many days expire out of the improvement backlog. */
export const DEFAULT_STALE_DAYS = 30

/**
 * Resolve the cap from an explicit argument, then the environment, then the
 * default. The environment leg exists because the documented way to raise the
 * cap is a `BS_SWEEP_NOTES_MAX_ISSUES=50 node …` prefix, and a shell expands
 * `"${BS_SWEEP_NOTES_MAX_ISSUES:-15}"` in the PARENT before that prefix reaches
 * the child — so passing the expansion as an argument silently ignores the
 * prefix and yields the default. Reading the variable inside the process makes
 * both the prefix and an exported variable work.
 */
export function resolveCap(argument, env = {}) {
  for (const candidate of [argument, env.BS_SWEEP_NOTES_MAX_ISSUES]) {
    if (candidate === undefined || candidate === null || String(candidate).trim() === '') continue
    const parsed = Number(candidate)
    if (!Number.isFinite(parsed)) continue
    return Math.max(0, Math.floor(parsed))
  }
  return DEFAULT_CAP
}

/**
 * Resolve the age-expiry window with the same argument-then-environment shape
 * as `resolveCap`. Reading the environment inside the process is the important
 * part: shell expansions happen before a VAR=value prefix reaches this child.
 */
export function resolveStaleDays(argument, env = {}) {
  for (const candidate of [argument, env.BS_SWEEP_NOTES_STALE_DAYS]) {
    if (candidate === undefined || candidate === null || String(candidate).trim() === '') continue
    const parsed = Number(candidate)
    if (!Number.isFinite(parsed) || parsed < 0) continue
    return Math.floor(parsed)
  }
  return DEFAULT_STALE_DAYS
}

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

/** Keep a slug readable and bounded, without leaving a trailing separator. */
function slugSegment(value, fallback) {
  return slug(value).slice(0, MAX_SLUG_SEGMENT).replace(/-+$/, '') || fallback
}

/**
 * Stable short digest of a cluster identity. This replaced a base64 encoding of
 * the whole identity: `parseNote` folds every non-field line into `statement`,
 * so a note carrying recurrence appendices produced a multi-kilobyte key. On a
 * 613-note backlog that reached 9,085 characters, which made the marker block
 * enormous and the pre-create Linear query unusable.
 */
function identityDigest(identity) {
  return createHash('sha256').update(identity, 'utf8').digest('hex').slice(0, KEY_DIGEST_LENGTH)
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
      const keyParts = [slugSegment(statement, 'note')]
      if (where) keyParts.push(slugSegment(where, 'where'))
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
      key: `${cluster.key}--${identityDigest(identity)}`,
    }))
    .sort((a, b) => a.key.localeCompare(b.key))
}

/**
 * Accept either the legacy key-array group or a titled `{ title, keys }` group.
 * A theme's name is the one thing no member note contains, so it is the only
 * field an agent may supply here.
 */
function normalizeGroup(group) {
  if (Array.isArray(group)) return { title: null, keys: group }
  if (group && typeof group === 'object') {
    return { title: group.title === undefined ? null : group.title, keys: group.keys }
  }
  throw new Error('mergeClusters groups must be key arrays or { title, keys } objects')
}

function validateTitle(title) {
  if (title === null) return null
  if (typeof title !== 'string') throw new Error('mergeClusters group title must be a string')
  if (/[\r\n]/.test(title)) throw new Error('mergeClusters group title must be a single line')
  const trimmed = normalizePresentation(title)
  if (!trimmed) throw new Error('mergeClusters group title must be non-empty')
  if (trimmed.length > MAX_TITLE_LENGTH) {
    throw new Error(`mergeClusters group title exceeds ${MAX_TITLE_LENGTH} characters`)
  }
  return trimmed
}

/**
 * Apply an agent-proposed partition of mechanical cluster keys. The semantic
 * decisions are the grouping and the optional theme title; this function
 * validates both and deterministically re-derives every other field.
 *
 * Members may span different `Where:` targets. A theme is defined by a shared
 * problem, not a shared file — refusing a cross-target merge is what made
 * thematic grouping impossible, so the members' targets are collected into
 * `wheres` instead of constraining the merge.
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
    const { title, keys } = normalizeGroup(group)
    if (!Array.isArray(keys) || keys.length === 0) {
      throw new Error('mergeClusters groups must be non-empty arrays')
    }
    const themeTitle = validateTitle(title)
    const members = [...keys]
      .sort((a, b) => String(a).localeCompare(String(b)))
      .map((key) => {
        if (typeof key !== 'string' || !byKey.has(key) || seen.has(key)) {
          throw new Error(`mergeClusters group has unknown or duplicate key: ${key}`)
        }
        seen.add(key)
        return byKey.get(key)
      })
    const wheres = [
      ...new Set(members.map((cluster) => normalizePresentation(cluster.where)).filter(Boolean)),
    ].sort((a, b) => a.localeCompare(b))
    return {
      ...members[0],
      title: themeTitle ?? normalizePresentation(members[0].statement),
      wheres,
      sourceKeys: members.map((cluster) => cluster.key),
      notes: members.flatMap((cluster) => cluster.notes).sort(noteOrder),
    }
  })
  if (seen.size !== byKey.size) {
    throw new Error('mergeClusters groups must account for every cluster')
  }
  return merged.sort((a, b) => a.key.localeCompare(b.key))
}

/**
 * Order themes by corroboration, then by age, then by key. Selection used to be
 * a single pass over a key-sorted array, which made it alphabetical by problem
 * statement — so the same head was filed every run and the backlog never moved.
 */
export function rankClusters(clusters) {
  return [...(Array.isArray(clusters) ? clusters : [])].sort((a, b) => {
    const aCount = Array.isArray(a?.notes) ? a.notes.length : 0
    const bCount = Array.isArray(b?.notes) ? b.notes.length : 0
    if (aCount !== bCount) return bCount - aCount
    const aFirst = String(a?.notes?.[0]?.created_at ?? '')
    const bFirst = String(b?.notes?.[0]?.created_at ?? '')
    if (aFirst !== bFirst) return aFirst < bFirst ? -1 : 1
    return String(a?.key ?? '').localeCompare(String(b?.key ?? ''))
  })
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

// The leading lookbehind is load-bearing: without it the match can start mid-path,
// so `/Users/dave/x/y.go` yields `Users/dave/x/y.go` and `~/.claude/a/b.md` yields
// `.claude/a/b.md` — both of which then read as repo-relative and probe as missing.
const PATH_TOKEN = /(?<![A-Za-z0-9_.@\-/~])[A-Za-z0-9_.@-]+(?:\/[A-Za-z0-9_.@-]+)+/g

function toEpoch(value) {
  const parsed = Date.parse(String(value ?? ''))
  return Number.isFinite(parsed) ? parsed : null
}

function newestNoteEpoch(cluster) {
  return (Array.isArray(cluster?.notes) ? cluster.notes : []).reduce((latest, note) => {
    const at = toEpoch(note?.created_at)
    return at !== null && (latest === null || at > latest) ? at : latest
  }, null)
}

function isExpired(cluster, { staleDays, now }) {
  const newest = newestNoteEpoch(cluster)
  if (newest === null) return false
  const current = Number(now)
  if (!Number.isFinite(current)) return false
  const windowMs = Math.max(0, Math.floor(Number(staleDays))) * 24 * 60 * 60 * 1000
  return newest < current - windowMs
}

/**
 * Pull repo-relative file paths out of free-text `Where:` prose. A token must
 * carry a directory separator and a file extension: absolute and home-relative
 * paths name an installed copy rather than this checkout, and a bare filename
 * (`SKILL.md`, `Makefile`) is too ambiguous to probe, so all are skipped rather
 * than reported as missing.
 */
function extractPaths(values) {
  const found = new Set()
  for (const value of Array.isArray(values) ? values : []) {
    for (const raw of String(value ?? '').match(PATH_TOKEN) ?? []) {
      const token = raw.replace(/[.,;:)\]]+$/, '')
      if (!token || token.startsWith('~') || token.startsWith('/')) continue
      if (token.includes('://') || !/\.[A-Za-z0-9]+$/.test(token)) continue
      found.add(token)
    }
  }
  return [...found].sort((a, b) => a.localeCompare(b))
}

/**
 * Report what the tree says about each theme's cited paths. These are SIGNALS,
 * never a verdict: a surviving path proves nothing, and a changed one only
 * means the finding is worth re-reading. Probes are injected so the core stays
 * pure and testable.
 */
export function stalenessSignals(clusters, { pathExists, lastChangeAt } = {}) {
  if (typeof pathExists !== 'function' || typeof lastChangeAt !== 'function') {
    throw new Error('stalenessSignals requires pathExists and lastChangeAt probes')
  }
  return (Array.isArray(clusters) ? clusters : []).map((cluster) => {
    const targets =
      Array.isArray(cluster?.wheres) && cluster.wheres.length ? cluster.wheres : [cluster?.where]
    const paths = extractPaths(targets)
    const notes = Array.isArray(cluster?.notes) ? cluster.notes : []
    const newest = notes.reduce((latest, entry) => {
      const at = toEpoch(entry?.created_at)
      return at !== null && (latest === null || at > latest) ? at : latest
    }, null)
    const missing = []
    const changedSince = []
    for (const path of paths) {
      if (!pathExists(path)) {
        missing.push(path)
        continue
      }
      const changed = toEpoch(lastChangeAt(path))
      if (changed !== null && newest !== null && changed > newest) changedSince.push(path)
    }
    return {
      key: cluster?.key,
      newestNoteAt: newest === null ? null : new Date(newest).toISOString(),
      paths,
      missing,
      changedSince,
    }
  })
}

/**
 * Validate the optional per-note retirement list on a verdict entry. A theme is
 * only `fixed` when the whole problem is gone, which a broad theme almost never
 * is — one live sibling holds the entire theme open. Without a per-note escape
 * a demonstrably fixed member can never retire, and the drain never fires.
 * Members are named individually here instead; the ids must belong to this
 * theme, so a verdict cannot reach across themes.
 */
function resolveFixedNoteIds(entry, cluster, evidence) {
  const raw = entry?.fixedNotes
  if (raw === undefined || raw === null) return []
  if (!Array.isArray(raw)) {
    throw new Error(`applyVerdicts fixedNotes must be an array: ${cluster.key}`)
  }
  const owned = new Set((Array.isArray(cluster.notes) ? cluster.notes : []).map((note) => note?.id))
  const seen = new Set()
  for (const id of raw) {
    if (typeof id !== 'string' || !id) {
      throw new Error(`applyVerdicts fixedNotes must be note ids: ${cluster.key}`)
    }
    if (!owned.has(id)) {
      throw new Error(`applyVerdicts fixedNotes names a note outside ${cluster.key}: ${id}`)
    }
    if (seen.has(id)) {
      throw new Error(`applyVerdicts fixedNotes repeats a note id: ${id}`)
    }
    seen.add(id)
  }
  if (seen.size && !evidence) {
    throw new Error(`applyVerdicts requires evidence to retire notes: ${cluster.key}`)
  }
  return [...seen].sort((a, b) => a.localeCompare(b))
}

/**
 * Partition themes by an agent-supplied currency verdict, accounting for every
 * theme exactly once. Anything that retires notes — a `fixed` verdict, or a
 * `fixedNotes` list on any verdict — must cite evidence.
 */
export function applyVerdicts(clusters, verdicts) {
  if (!Array.isArray(clusters) || !Array.isArray(verdicts)) {
    throw new Error('applyVerdicts requires cluster and verdict arrays')
  }
  const byKey = new Map()
  for (const cluster of clusters) {
    if (!cluster || typeof cluster.key !== 'string' || byKey.has(cluster.key)) {
      throw new Error('applyVerdicts received invalid or duplicate cluster keys')
    }
    byKey.set(cluster.key, cluster)
  }
  const buckets = Object.fromEntries(VERDICTS.map((verdict) => [verdict, []]))
  const seen = new Set()
  for (const entry of verdicts) {
    const key = entry?.key
    if (typeof key !== 'string' || !byKey.has(key) || seen.has(key)) {
      throw new Error(`applyVerdicts has an unknown or duplicate key: ${key}`)
    }
    if (!VERDICTS.includes(entry?.verdict)) {
      throw new Error(`applyVerdicts got an unknown verdict for ${key}: ${entry?.verdict}`)
    }
    const cluster = byKey.get(key)
    const evidence = typeof entry?.evidence === 'string' ? entry.evidence.trim() : ''
    if (entry.verdict === 'fixed' && !evidence) {
      throw new Error(`applyVerdicts requires evidence for a fixed verdict: ${key}`)
    }
    const fixedNoteIds = resolveFixedNoteIds(entry, cluster, evidence)
    seen.add(key)
    buckets[entry.verdict].push({ cluster, evidence: evidence || null, fixedNoteIds })
  }
  if (seen.size !== byKey.size) {
    throw new Error('applyVerdicts must account for every cluster')
  }
  for (const bucket of Object.values(buckets)) {
    bucket.sort((a, b) => a.cluster.key.localeCompare(b.cluster.key))
  }
  return buckets
}

/**
 * The complete, deduplicated set of note ids this run may retire: every note in
 * a wholly `fixed` theme, plus every individually named `fixedNotes` id on any
 * other verdict. Computing it here keeps the union out of skill prose, where an
 * omitted bucket would silently under- or over-retire.
 */
export function retiredNoteIds(buckets, selection = {}) {
  const ids = new Set()
  for (const verdict of VERDICTS) {
    for (const entry of Array.isArray(buckets?.[verdict]) ? buckets[verdict] : []) {
      if (verdict === 'fixed') {
        for (const note of Array.isArray(entry?.cluster?.notes) ? entry.cluster.notes : []) {
          if (typeof note?.id === 'string' && note.id) ids.add(note.id)
        }
      }
      for (const id of Array.isArray(entry?.fixedNoteIds) ? entry.fixedNoteIds : []) ids.add(id)
    }
  }
  for (const entry of [
    ...(Array.isArray(buckets?.expired) ? buckets.expired : []),
    ...(Array.isArray(selection?.expired) ? selection.expired : []),
  ]) {
    for (const note of Array.isArray(entry?.cluster?.notes) ? entry.cluster.notes : []) {
      if (typeof note?.id === 'string' && note.id) ids.add(note.id)
    }
  }
  return [...ids].sort((a, b) => a.localeCompare(b))
}

/**
 * Account for every cluster exactly once. Marker-carriers are dropped; themes
 * whose newest note is older than the stale window expire; up to `cap`
 * remaining clusters are selected, and the rest are deferred. Ranking is
 * applied here rather than by the caller so it cannot be skipped.
 */
export function selectClusters(
  clusters,
  markedIssues = [],
  { cap = DEFAULT_CAP, staleDays = DEFAULT_STALE_DAYS, now = Date.now() } = {},
) {
  const selected = []
  const deferred = []
  const dropped = []
  const expired = []
  const limit = Number.isFinite(Number(cap)) ? Math.max(0, Math.floor(Number(cap))) : DEFAULT_CAP
  const staleWindow = Number.isFinite(Number(staleDays))
    ? Math.max(0, Math.floor(Number(staleDays)))
    : DEFAULT_STALE_DAYS
  for (const cluster of rankClusters(clusters)) {
    if (!cluster || typeof cluster.key !== 'string') continue
    if (carriesMarker(cluster, markedIssues)) {
      dropped.push({ cluster, reason: 'already-tracked' })
    } else if (isExpired(cluster, { staleDays: staleWindow, now })) {
      expired.push({ cluster, reason: 'expired' })
    } else if (selected.length < limit) {
      selected.push({ cluster, reason: 'selected' })
    } else {
      deferred.push({ cluster, reason: 'over-cap' })
    }
  }
  return { selected, deferred, dropped, expired }
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

/** Last commit time for a path, or null when git cannot answer. */
function gitLastChangeAt(path) {
  const result = spawnSync('git', ['log', '-1', '--format=%cI', '--', path], { encoding: 'utf8' })
  if (result.error || result.status !== 0) return null
  return String(result.stdout ?? '').trim() || null
}

/** Small CLI adapter, injection-friendly for node:test. */
export function runCli(
  argv,
  {
    readFile = (file) => readFileSync(file, 'utf8'),
    pathExists = (path) => existsSync(path),
    lastChangeAt = gitLastChangeAt,
    env = process.env,
  } = {},
) {
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
    case 'rank':
      return JSON.stringify(rankClusters(readJson(args[0])))
    case 'stale':
      return JSON.stringify(stalenessSignals(readJson(args[0]), { pathExists, lastChangeAt }))
    case 'verdicts':
      return JSON.stringify(applyVerdicts(readJson(args[0]), readJson(args[1])))
    case 'retired':
      return JSON.stringify(retiredNoteIds(readJson(args[0]), args[1] ? readJson(args[1]) : {}))
    case 'select':
      return JSON.stringify(
        selectClusters(readJson(args[0]), readJson(args[1]), {
          cap: resolveCap(args[2], env),
          staleDays: resolveStaleDays(args[3], env),
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
