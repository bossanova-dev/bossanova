// Pure decision logic for the bs-sweep-notes skill. Linear access, the signed
// attachment PUT, and the filesystem/git probes the staleness signals and the
// pointer resolver need, remain injected — the concrete implementations live
// only as `runCli` defaults (or in the skill's `node -e` blocks), alongside the
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
 * Longest `statement` (and `where`) kept on one `digest` line. The digest exists
 * because the full clusters file measured 921KB at 439 clusters — too large for
 * the theming subagent to read — so every field on a digest line is bounded.
 */
export const DIGEST_STATEMENT_MAX = 160

/**
 * Evidence bullets rendered into a filed child description before the rest are
 * summarised by a remainder line. The verbatim bodies live in the attachment.
 */
export const MAX_EVIDENCE_BULLETS = 20

/**
 * Where a published `boss`/`boss-*` core's source lives in this checkout. A
 * pointer at `.claude/skills/<core>/…` names a directory that does not exist
 * here (published cores install globally), so the resolver probes this root.
 */
export const PUBLISHED_CORE_PAYLOAD_ROOT = 'services/boss/internal/skillinstall/skills'

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

/**
 * Markdown image syntax, inline (`![alt](src)`) and reference (`![alt][ref]`) alike.
 *
 * Deliberately anchored on the `!`, so an ordinary link `[text](url)` is NOT matched: a note body
 * that cites a URL is evidence a reader still wants, and defanging it would be a silent loss.
 */
const EVIDENCE_IMAGE = /!\[([^\]]*)\](?:\(([^)]*)\)|\[([^\]]*)\])/g

/**
 * Make one note field safe to interpolate into a filed issue description.
 *
 * TWO defects, one function, because both are properties of the same untrusted text and a prose
 * rule that names only the first is what let the second ship:
 *
 *   1. CR/LF is stripped, so note text cannot forge a `Notes: <key>` marker line.
 *   2. Image markdown is DEFANGED. The tracker re-hosts an image it finds in a description behind
 *      a short-lived signed URL, so a filed ticket that carries one ships permanently broken
 *      images -- and the clipped evidence block is not where images were ever meant to survive.
 *      The verbatim attachment is.
 *
 * Non-image markdown is left intact: a link, a code span, emphasis and a bullet are all evidence a
 * reader still needs.
 */
export function sanitizeEvidenceText(value) {
  return String(value ?? '')
    .replace(/[\r\n]+/g, ' ')
    .replace(EVIDENCE_IMAGE, (_match, alt, _inline, reference) => {
      const label = String(alt ?? reference ?? '')
        .replace(/\s+/g, ' ')
        .trim()
      return `(image omitted${label ? `: ${label}` : ''} - see the attached source notes)`
    })
    .trim()
}

/** The exact attachment title a filed child must carry before its notes may be deleted. */
export function sourceNotesTitle(issueID) {
  return `Source notes (${issueID})`
}

/**
 * Per child id, is the exact `Source notes (<issue-id>)` attachment present?
 *
 * This is the command the deletion precondition names. It existed only as a prose instruction to
 * "re-read each child's attachments and confirm the exact title is present", and a precondition
 * that gates a destructive action while supplying no command is a precondition each run improvises
 * -- which is how an ad-hoc unsigned attachment query came to decide whether evidence was deleted.
 *
 * Fails CLOSED and LOUD on a malformed entry rather than reporting absence: "no attachment list
 * for this id" and "this id has no attachment" must not be the same answer when the answer decides
 * a delete.
 *
 * @param {Record<string, Array<{title?: string}|string>>} byId child id -> its attachment list
 * @returns {{present: string[], missing: string[], perId: Array<{id: string, present: boolean}>}}
 */
export function attachmentPresence(byId) {
  if (!byId || typeof byId !== 'object' || Array.isArray(byId)) {
    throw new Error('attachmentPresence requires an object mapping child id to its attachments')
  }
  const perId = []
  for (const [id, attachments] of Object.entries(byId)) {
    if (!Array.isArray(attachments)) {
      throw new Error(
        `attachment list for ${id} is not an array — re-read that child's attachments`,
      )
    }
    const wanted = sourceNotesTitle(id)
    const present = attachments.some((attachment) => {
      const title = typeof attachment === 'string' ? attachment : attachment?.title
      return title === wanted
    })
    perId.push({ id, title: wanted, present })
  }
  return {
    present: perId.filter((entry) => entry.present).map((entry) => entry.id),
    missing: perId.filter((entry) => !entry.present).map((entry) => entry.id),
    perId,
  }
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

function staleWindowMs(staleDays) {
  return Math.max(0, Math.floor(Number(staleDays))) * 24 * 60 * 60 * 1000
}

function isExpired(cluster, { staleDays, now }) {
  const newest = newestNoteEpoch(cluster)
  if (newest === null) return false
  const current = Number(now)
  if (!Number.isFinite(current)) return false
  return newest < current - staleWindowMs(staleDays)
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

const INSTALLED_CORE_POINTER = /^\.claude\/skills\/([^/]+)\/(.+)$/

/**
 * Resolve one extracted path token against the tree. A token that exists is
 * kept; a missing `.claude/skills/<core>/<rest>` whose payload source
 * `PUBLISHED_CORE_PAYLOAD_ROOT/<core>/<rest>` exists is rewritten onto it; any
 * other missing token is reported as missing.
 *
 * @returns {{token: string, path: string, status: 'exists'|'rewritten'|'missing'}}
 */
function resolvePathToken(token, pathExists) {
  if (pathExists(token)) return { token, path: token, status: 'exists' }
  const installed = INSTALLED_CORE_POINTER.exec(token)
  if (installed) {
    const payload = `${PUBLISHED_CORE_PAYLOAD_ROOT}/${installed[1]}/${installed[2]}`
    if (pathExists(payload)) return { token, path: payload, status: 'rewritten' }
  }
  return { token, path: token, status: 'missing' }
}

/** Replace a path token only where it stands as a whole token, never mid-path. */
function replacePathToken(text, token, replacement) {
  const pattern = new RegExp(
    `(?<![A-Za-z0-9_.@\\-/~])${escapeRegExp(token)}(?![A-Za-z0-9_@\\-/]|\\.[A-Za-z0-9])`,
    'g',
  )
  return text.replace(pattern, () => replacement)
}

/**
 * Resolve every path token in one `Where:` entry with the same token rule the
 * staleness signals use. Returns the entry with rewritable published-core
 * pointers moved onto their payload source path, plus the tokens that exist
 * nowhere in this checkout.
 *
 * @param {string} where one `Where:` entry
 * @param {{pathExists: (path: string) => boolean}} probes
 * @returns {{text: string, rewritten: Array<{from: string, to: string}>, unresolved: string[]}}
 */
export function resolveWherePointer(where, { pathExists } = {}) {
  if (typeof pathExists !== 'function') {
    throw new Error('resolveWherePointer requires a pathExists probe')
  }
  let text = normalizePresentation(where)
  const rewritten = []
  const unresolved = []
  for (const token of extractPaths([text])) {
    const resolved = resolvePathToken(token, pathExists)
    if (resolved.status === 'rewritten') {
      text = replacePathToken(text, token, resolved.path)
      rewritten.push({ from: token, to: resolved.path })
    } else if (resolved.status === 'missing') {
      unresolved.push(token)
    }
  }
  return { text, rewritten, unresolved }
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
    const notes = Array.isArray(cluster?.notes) ? cluster.notes : []
    const newest = notes.reduce((latest, entry) => {
      const at = toEpoch(entry?.created_at)
      return at !== null && (latest === null || at > latest) ? at : latest
    }, null)
    const missing = []
    const changedSince = []
    // A published-core pointer is probed at its payload source path, so a live
    // file is never handed to the currency pass as `missing`.
    const resolved = [
      ...new Set(extractPaths(targets).map((token) => resolvePathToken(token, pathExists).path)),
    ].sort((a, b) => a.localeCompare(b))
    for (const path of resolved) {
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
      paths: resolved,
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

/** One note is old when its own parseable timestamp is outside the stale window. */
function isNoteOld(note, { staleDays, now }) {
  const at = toEpoch(note?.created_at)
  const current = Number(now)
  if (at === null || !Number.isFinite(current) || !Number.isFinite(Number(staleDays))) return false
  return at < current - staleWindowMs(staleDays)
}

/**
 * The complete, deduplicated set of note ids this run may retire: every note in
 * a wholly `fixed` theme, every individually named `fixedNotes` id on any other
 * verdict, every note of an `expired` theme, and — when `{staleDays, now}` is
 * supplied — every OLD member note of a `deferred` theme or an `unverifiable`
 * theme. Computing it here keeps the union out of skill prose, where an omitted
 * bucket would silently under- or over-retire.
 *
 * Per-note expiry exists because theme-level expiry is near-inert at realistic
 * grouping density: a theme is as young as its newest member, so one recent
 * recurrence holds every old sibling in the backlog forever. Selected and
 * already-tracked themes contribute nothing through expiry — their notes are
 * deleted once filed — and `unverifiable` themes never reach `select`, so
 * without this they could never expire at all.
 */
export function retiredNoteIds(buckets, selection = {}, { staleDays, now } = {}) {
  const ids = new Set()
  const addAll = (cluster, keep = () => true) => {
    for (const note of Array.isArray(cluster?.notes) ? cluster.notes : []) {
      if (typeof note?.id === 'string' && note.id && keep(note)) ids.add(note.id)
    }
  }
  for (const verdict of VERDICTS) {
    for (const entry of Array.isArray(buckets?.[verdict]) ? buckets[verdict] : []) {
      if (verdict === 'fixed') addAll(entry?.cluster)
      for (const id of Array.isArray(entry?.fixedNoteIds) ? entry.fixedNoteIds : []) ids.add(id)
    }
  }
  for (const entry of [
    ...(Array.isArray(buckets?.expired) ? buckets.expired : []),
    ...(Array.isArray(selection?.expired) ? selection.expired : []),
  ]) {
    addAll(entry?.cluster)
  }
  if (staleDays !== undefined && now !== undefined) {
    const old = (note) => isNoteOld(note, { staleDays, now })
    for (const entry of [
      ...(Array.isArray(selection?.deferred) ? selection.deferred : []),
      ...(Array.isArray(buckets?.unverifiable) ? buckets.unverifiable : []),
    ]) {
      addAll(entry?.cluster, old)
    }
  }
  return [...ids].sort((a, b) => a.localeCompare(b))
}

function validateDeleteIds(deleteIds) {
  if (!Array.isArray(deleteIds) || !deleteIds.every((id) => typeof id === 'string' && id)) {
    throw new Error('retirePlan requires the delete set as an array of note ids')
  }
  return [...new Set(deleteIds)].sort((a, b) => a.localeCompare(b))
}

/**
 * Phase 4's two loops and Phase 5's drain, from one computation. `retag` is the
 * retirement set minus `delete` — deletion wins, because a filed theme's
 * evidence already lives on its ticket — so no id is counted twice and
 * `counts.drain` is exactly `counts.delete + counts.retag`.
 *
 * @returns {{delete: string[], retag: string[],
 *   counts: {delete: number, retag: number, drain: number}}}
 */
export function retirePlan(buckets, selection, deleteIds, { staleDays, now } = {}) {
  const deletions = validateDeleteIds(deleteIds)
  const deleting = new Set(deletions)
  const retag = retiredNoteIds(buckets, selection, { staleDays, now }).filter(
    (id) => !deleting.has(id),
  )
  return {
    delete: deletions,
    retag,
    counts: {
      delete: deletions.length,
      retag: retag.length,
      drain: deletions.length + retag.length,
    },
  }
}

/**
 * Account for every cluster exactly once. Marker-carriers are dropped; themes
 * whose newest note is older than the stale window expire (the special case of
 * per-note expiry where every member is old — see `retiredNoteIds`); up to `cap`
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

function truncateText(value, max) {
  const text = normalizePresentation(value)
  return text.length > max ? `${text.slice(0, max - 1)}…` : text
}

/**
 * One bounded line per mechanical cluster, for the theming subagent. The full
 * clusters file carries every member note's body and grows with the backlog; a
 * digest line carries only what a grouping decision needs, so the file stays
 * readable with an offset/limit read at any backlog size.
 *
 * @returns {Array<{key: string, statement: string, where: string|null, notes: number}>}
 */
export function digestClusters(clusters) {
  if (!Array.isArray(clusters)) throw new Error('digestClusters requires a cluster array')
  return clusters.map((cluster) => {
    if (!cluster || typeof cluster.key !== 'string' || !cluster.key) {
      throw new Error('digestClusters received a cluster without a key')
    }
    return {
      key: cluster.key,
      statement: truncateText(cluster.statement, DIGEST_STATEMENT_MAX),
      where: cluster.where ? truncateText(cluster.where, DIGEST_STATEMENT_MAX) : null,
      notes: Array.isArray(cluster.notes) ? cluster.notes.length : 0,
    }
  })
}

/**
 * Wrap text in a code span whose fence is one backtick longer than the longest
 * backtick run inside it, so the span cannot close early and the tracker's
 * autolinker leaves `SKILL.md:689`-shaped tokens as literal text.
 */
function codeSpan(text) {
  const longest = Math.max(0, ...(String(text).match(/`+/g) ?? []).map((run) => run.length))
  const fence = '`'.repeat(longest + 1)
  const pad = /^`|`$/.test(text) ? ' ' : ''
  return `${fence}${pad}${text}${pad}${fence}`
}

function renderWhereBullet(where, pathExists) {
  const { text, unresolved } = resolveWherePointer(where, { pathExists })
  const annotation = unresolved.length
    ? ` (not found at filing: ${unresolved.map(codeSpan).join(', ')})`
    : ''
  return `- ${codeSpan(text)}${annotation}`
}

/**
 * Render a filed child's whole description: `## Problem`, `## Where`,
 * `## Evidence`, then the marker block as the final lines.
 *
 * Every `## Where` entry is one code span, published-core pointers are moved
 * onto their payload source path, and a token that exists nowhere is annotated
 * outside the span. The Evidence line names the attachment by its bare kind:
 * the issue id does not exist until the create returns, so it cannot be cited
 * here. Every interpolated note field passes through `sanitizeEvidenceText`.
 */
export function renderChildDescription(cluster, { pathExists } = {}) {
  if (typeof pathExists !== 'function') {
    throw new Error('renderChildDescription requires a pathExists probe')
  }
  const markers = renderClusterMarkers(cluster)
  const notes = Array.isArray(cluster?.notes) ? cluster.notes : []
  const wheres =
    Array.isArray(cluster?.wheres) && cluster.wheres.length
      ? cluster.wheres
      : [cluster?.where].filter(Boolean)
  const title = sanitizeEvidenceText(cluster?.title ?? cluster?.statement)
  const shown = notes.slice(0, MAX_EVIDENCE_BULLETS)
  const noun = notes.length === 1 ? 'source note' : 'source notes'
  const lines = [
    '## Problem',
    '',
    title,
    '',
    '## Where',
    '',
    ...(wheres.length
      ? wheres.map((where) => renderWhereBullet(where, pathExists))
      : ['- (no Where: pointer recorded)']),
    '',
    '## Evidence',
    '',
    `_${notes.length} ${noun}; the verbatim bodies are in this issue's Source notes attachment._`,
    '',
    ...shown.map((entry) => {
      const statement = sanitizeEvidenceText(entry?.statement)
      const run = sanitizeEvidenceText(entry?.run) || 'unknown'
      const at = sanitizeEvidenceText(entry?.created_at) || 'unknown'
      return `- ${statement} — recorded by ${run}, ${at}`
    }),
    ...(notes.length > shown.length
      ? [`- …and ${notes.length - shown.length} more in the attachment`]
      : []),
    '',
    markers,
  ]
  return lines.join('\n')
}

/**
 * The notes whose parsed `Where:` line contains `pointer`, case-insensitively.
 * The record-side duplicate search used to match whole bodies, so a note that
 * merely mentioned a skill name drowned out the real duplicates at that pointer.
 * The raw note objects are returned unchanged.
 */
export function whereMatch(notes, pointer) {
  if (!Array.isArray(notes)) throw new Error('whereMatch requires a note array')
  const needle = normalize(pointer)
  if (!needle) throw new Error('whereMatch requires a non-empty pointer')
  return notes.filter((entry) => normalize(parseNote(entry).where).includes(needle))
}

export const MARKED_ISSUES_QUERY = `query Marked($filter: IssueFilter!, $after: String) {
  issues(first: 250, filter: $filter, after: $after, includeArchived: true) {
    nodes { identifier description }
    pageInfo { hasNextPage endCursor }
  }
}`

/**
 * Fetch the complete marker snapshot, or with `updatedAfter` only the issues
 * updated since then; refuse partial pagination results either way.
 */
export async function fetchMarkedLinearIssues({
  apiKey,
  linearRequest,
  maxPages = 20,
  markerPrefix = 'Notes: ',
  updatedAfter,
} = {}) {
  if (typeof linearRequest !== 'function') {
    throw new Error('fetchMarkedLinearIssues requires a linearRequest implementation')
  }
  if (updatedAfter !== undefined && updatedAfter !== null && toEpoch(updatedAfter) === null) {
    throw new Error(`fetchMarkedLinearIssues updatedAfter is not a timestamp: ${updatedAfter}`)
  }
  // With `updatedAfter`, only issues changed since the baseline snapshot are
  // fetched; the caller scans the union of both, so the delta cannot lose one.
  const filter = {
    description: { contains: markerPrefix },
    ...(updatedAfter ? { updatedAt: { gt: updatedAfter } } : {}),
  }
  const nodes = []
  let after = null
  for (let page = 0; page < maxPages; page++) {
    const data = await linearRequest({
      apiKey,
      query: MARKED_ISSUES_QUERY,
      variables: { filter, after },
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

export const ISSUE_ATTACHMENTS_QUERY = `query IssueAttachments($id: String!) {
  issue(id: $id) {
    id
    identifier
    attachments(first: 250) { nodes { id title } }
  }
}`

export const FILE_UPLOAD_MUTATION = `mutation FileUpload($contentType: String!, $filename: String!, $size: Int!) {
  fileUpload(contentType: $contentType, filename: $filename, size: $size) {
    success
    uploadFile { uploadUrl assetUrl headers { key value } }
  }
}`

export const ATTACHMENT_CREATE_MUTATION = `mutation AttachmentCreate($input: AttachmentCreateInput!) {
  attachmentCreate(input: $input) { success attachment { id title } }
}`

async function readIssueAttachments({ apiKey, linearRequest, issueId }) {
  const data = await linearRequest({
    apiKey,
    query: ISSUE_ATTACHMENTS_QUERY,
    variables: { id: issueId },
  })
  const issue = data?.issue
  const nodes = issue?.attachments?.nodes
  if (!issue || typeof issue.id !== 'string' || !issue.id || !Array.isArray(nodes)) {
    throw new Error(`attachment read for ${issueId} returned no readable attachment list`)
  }
  return { uuid: issue.id, titles: nodes.map((node) => node?.title) }
}

/**
 * Upload one child's verbatim source notes and attach them under the exact
 * `sourceNotesTitle(issueId)` title — the whole prepare → PUT → finalize leg in
 * one call, so no run hand-transcribes a signed URL or its headers.
 *
 * - An issue already carrying the exact title is `skipped`, so a resumed run
 *   cannot duplicate it (`attachmentCreate` mints a new row on every call).
 * - The declared size is the BYTE length of what `readFile` returns for the
 *   exact file handed to `put`.
 * - Every header `fileUpload` returns is passed to `put`.
 * - The create is sent once, as a write. If it throws or is not confirmed, the
 *   outcome is settled by re-reading the attachments for the exact title — never
 *   by re-sending — and a create the read-back cannot find re-throws.
 *
 * @returns {Promise<{status: 'skipped'|'created', issueId: string, title: string,
 *   settledBy?: 'response'|'read-back'}>}
 */
export async function attachSourceNotes({
  apiKey,
  linearRequest,
  put,
  readFile,
  issueId,
  file,
  contentType = 'text/markdown',
  filename,
} = {}) {
  for (const [name, value] of Object.entries({ linearRequest, put, readFile })) {
    if (typeof value !== 'function') throw new Error(`attachSourceNotes requires ${name}`)
  }
  if (typeof issueId !== 'string' || !issueId) throw new Error('attachSourceNotes requires issueId')
  if (typeof file !== 'string' || !file) throw new Error('attachSourceNotes requires file')
  const title = sourceNotesTitle(issueId)
  const before = await readIssueAttachments({ apiKey, linearRequest, issueId })
  if (before.titles.includes(title)) return { status: 'skipped', issueId, title }

  const content = readFile(file)
  const size = Buffer.isBuffer(content)
    ? content.length
    : Buffer.byteLength(String(content ?? ''), 'utf8')
  if (size === 0) throw new Error(`attachSourceNotes refuses an empty source notes file: ${file}`)

  const upload = await linearRequest({
    apiKey,
    query: FILE_UPLOAD_MUTATION,
    variables: { contentType, filename: filename || `${issueId}-source-notes.md`, size },
  })
  const uploadFile = upload?.fileUpload?.uploadFile
  if (
    upload?.fileUpload?.success !== true ||
    typeof uploadFile?.uploadUrl !== 'string' ||
    typeof uploadFile?.assetUrl !== 'string' ||
    !Array.isArray(uploadFile?.headers)
  ) {
    throw new Error(`fileUpload for ${issueId} returned no usable signed upload`)
  }
  const headers = { 'Content-Type': contentType, 'Cache-Control': 'public, max-age=31536000' }
  for (const header of uploadFile.headers) {
    if (typeof header?.key !== 'string' || typeof header?.value !== 'string') {
      throw new Error(`fileUpload for ${issueId} returned a malformed header`)
    }
    headers[header.key] = header.value
  }
  await put({ file, uploadURL: uploadFile.uploadUrl, headers })

  let createError = null
  try {
    const created = await linearRequest({
      apiKey,
      query: ATTACHMENT_CREATE_MUTATION,
      variables: { input: { issueId: before.uuid, title, url: uploadFile.assetUrl } },
      operation: 'write',
    })
    const result = created?.attachmentCreate
    if (result?.success === true && result?.attachment?.title === title) {
      return { status: 'created', issueId, title, settledBy: 'response' }
    }
  } catch (error) {
    createError = error
  }
  const after = await readIssueAttachments({ apiKey, linearRequest, issueId })
  if (after.titles.includes(title)) {
    return { status: 'created', issueId, title, settledBy: 'read-back' }
  }
  throw new Error(
    `attachmentCreate for ${issueId} did not land ${JSON.stringify(title)}` +
      (createError ? `: ${createError.message}` : ' (unconfirmed response, absent on read-back)'),
  )
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
    now = Date.now,
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
    case 'attachments':
      return JSON.stringify(attachmentPresence(readJson(args[0])))
    case 'retired':
      return JSON.stringify(
        retiredNoteIds(readJson(args[0]), args[1] ? readJson(args[1]) : {}, {
          staleDays: resolveStaleDays(args[2], env),
          now: now(),
        }),
      )
    case 'retire-plan':
      return JSON.stringify(
        retirePlan(readJson(args[0]), readJson(args[1]), readJson(args[2]), {
          staleDays: resolveStaleDays(args[3], env),
          now: now(),
        }),
      )
    case 'digest':
      return digestClusters(readJson(args[0]))
        .map((line) => JSON.stringify(line))
        .join('\n')
    case 'describe': {
      const selection = readJson(args[0])
      const entry = (Array.isArray(selection?.selected) ? selection.selected : []).find(
        (candidate) => candidate?.cluster?.key === args[1],
      )
      if (!entry) throw new Error(`describe: no selected theme with key ${args[1]}`)
      return renderChildDescription(entry.cluster, { pathExists })
    }
    case 'where-match':
      return JSON.stringify(whereMatch(readJson(args[0]), args[1]))
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
    const output = runCli(process.argv.slice(2))
    process.stdout.write(output ? `${output}\n` : '')
  } catch (error) {
    process.stderr.write(`sweep-notes-gate: ${error.message}\n`)
    process.exitCode = 1
  }
}
