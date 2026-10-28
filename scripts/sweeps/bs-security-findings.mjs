#!/usr/bin/env node
// scripts/sweeps/bs-security-findings.mjs
// Findings core for the bs-security sweep: parse reviewer reports into one record
// schema, fingerprint them, check their locations against the tree, merge exact
// duplicates, and classify them against the Linear ledger of filed findings.
//
// Everything above the CLI section is pure (I/O arrives injected), so the record
// schema, the run-dir layout and the marker format are defined and tested once
// here and consumed by the Codex runner, the secret scan, the epic helpers and the
// skill. This module files nothing and dispatches nothing.

import { createHash } from 'node:crypto'
import fs from 'node:fs'
import path from 'node:path'
import { parseArgs } from 'node:util'

import { linearRequest } from '../../skills-toolbox/linear-gate-lib.mjs'
import { isMainModule } from '../../skills-toolbox/main-module.mjs'
import { fetchMarkedLinearIssues } from './sweep-sentry-gate.mjs'
import {
  confidenceRank,
  normaliseConfidence,
  normaliseOutcome,
  normaliseSeverity,
  severityRank,
} from './bs-security-severity.mjs'

export { normaliseConfidence, normaliseSeverity } from './bs-security-severity.mjs'

// ---------------------------------------------------------------------------
// Contract: record schema and run-dir layout.

export const RECORD_SCHEMA_VERSION = 1

/**
 * One finding, whatever produced it.
 *
 * @typedef {object} FindingLocation
 * @property {string} path         repo-relative, or absolute when outside the root
 * @property {number|null|unknown} line  integer, or null when absent; a JSON value that
 *                                  is neither (nor a decimal-digit string) is kept
 *                                  verbatim and fails verification as line-out-of-range
 * @property {string} note         free text after the path; backticked tokens are checked
 * @property {string} raw          the location text as written
 * @property {true} [outsideRoot]  set when an absolute path lies outside the root
 * @property {boolean} [verified]  set by verifyLocations
 * @property {string|null} [reason] set by verifyLocations: outside-root | missing-file |
 *                                  line-out-of-range | token-not-found
 *
 * @typedef {object} FindingRecord
 * @property {'claude'|'codex'|'history-scan'|'tenant-scan'} source
 * @property {string} lens
 * @property {string} lensFamily
 * @property {string} title
 * @property {'info'|'low'|'medium'|'high'|'critical'|null} severity
 * @property {'finding'|'needs_validation'} outcome
 * @property {string} unresolved
 * @property {string} gate
 * @property {string} severityRaw
 * @property {true} [severityUnparsed]
 * @property {FindingLocation[]} locations
 * @property {string|null} primaryPath
 * @property {string} defect
 * @property {string} impact
 * @property {string} fix
 * @property {string} confidence
 * @property {string} confidenceRaw
 * @property {string} fingerprint
 * @property {'location-verified'|'location-unverified'} [locationStatus]
 * @property {unknown} [evidence]
 */

/** Paths inside a run dir. Sibling runners write to these; collect reads them. */
export const RUN_LAYOUT = Object.freeze({
  findingsDir: 'findings', // one <lens-id>.md per Claude lens
  codexFindings: 'codex/findings.json',
  secretScanFindings: 'secret-scan/findings.json',
  tenantScanFindings: 'tenant-scan/findings.json',
  merged: 'merged.json',
})

const SOURCES = new Set(['claude', 'codex', 'history-scan', 'tenant-scan'])
const LOCATION_STATUSES = new Set(['location-verified', 'location-unverified'])
const DEFAULT_LENS = {
  codex: 'codex',
  'history-scan': 'secret-scan',
  'tenant-scan': 'tenant-access',
}
const DEFAULT_LENS_FAMILY = {
  codex: 'cross-model',
  'history-scan': 'secret-history',
  'tenant-scan': 'tenant-access',
}

// ---------------------------------------------------------------------------
// Fingerprint.

// First match wins, so the more specific classes come first. This table and
// STOPWORDS feed the fingerprint, which filed issues carry as their marker: an edit
// that moves any fingerprint re-files those findings. The golden fingerprint test
// pins this; a deliberate change needs a ledger migration and a
// RECORD_SCHEMA_VERSION bump.
const DEFECT_CLASSES = [
  [
    'path-traversal',
    /path traversal|directory traversal|\.\.\/|zip ?slip|symlink|escapes? the .*director/,
  ],
  ['ssrf', /\bssrf\b|server-side request|metadata endpoint|internal network/],
  ['xss', /\bxss\b|cross-site scripting|innerhtml|unescaped html/],
  ['injection', /injection|\binject|command exec|shell meta|\bsql\b|\beval\b/],
  [
    'secret-exposure',
    /secret|credential|api key|password|private key|\btoken\b.*\b(?:leak|log|expos)|\b(?:leak|log|expos)\w*\b.*\btoken/,
  ],
  ['authn', /authenticat|\bauthn\b|unauthenticated|session fixation|signature verif|\blogin\b/],
  [
    'authz',
    /authori[sz]|\bauthz\b|permission|access control|privilege|\bidor\b|\bowner|tenant|cross-org/,
  ],
  [
    'crypto',
    /crypto|cipher|\bhmac\b|\bnonce\b|\btls\b|certificate|timing attack|weak hash|insecure random/,
  ],
  [
    'dos',
    /denial of service|\bdos\b|unbounded|exhaust|rate limit|\boom\b|out of memory|amplification/,
  ],
  ['supply-chain', /supply chain|dependency|unpinned|typosquat|third-party action|curl .*\| *sh/],
]

/** Closed-set defect class from a keyword table; `other` when nothing matches. */
export function classifyDefect(title, defect) {
  const text = `${title ?? ''} ${defect ?? ''}`.toLowerCase()
  for (const [cls, re] of DEFECT_CLASSES) if (re.test(text)) return cls
  return 'other'
}

const STOPWORDS = new Set(
  'a an the of in on at to for from by with and or is are was be been can could may via when that this these its it as into than then which who'.split(
    ' ',
  ),
)

// Lowercased alphanumeric tokens minus stopwords (keeping `no`/`not`) and pure
// numbers, so a renumbered heading keeps its stem; first eight tokens.
export function titleStem(title) {
  return (
    String(title ?? '')
      .toLowerCase()
      .match(/[a-z0-9]+/g) ?? []
  )
    .filter((t) => !STOPWORDS.has(t) && !/^\d+$/.test(t))
    .slice(0, 8)
    .join(' ')
}

/**
 * `sf-` + 12 hex of sha256(lensFamily | primaryPath | defectClass | titleStem).
 * Line numbers are excluded so drift does not mint a new fingerprint.
 */
export function fingerprint(record) {
  const key = [
    record.lensFamily ?? '',
    record.primaryPath ?? '',
    classifyDefect(record.title, record.defect),
    titleStem(record.title),
  ].join('|')
  return `sf-${createHash('sha256').update(key).digest('hex').slice(0, 12)}`
}

// ---------------------------------------------------------------------------
// Markdown parsing.

const FIELD_ALIASES = {
  severity: 'severity',
  outcome: 'outcome',
  unresolved: 'unresolved',
  gate: 'gate',
  location: 'location',
  locations: 'location',
  defect: 'defect',
  issue: 'defect',
  problem: 'defect',
  impact: 'impact',
  exploit: 'impact',
  risk: 'impact',
  fix: 'fix',
  remediation: 'fix',
  recommendation: 'fix',
  confidence: 'confidence',
}

const HEADING_RE = /^(#{1,6})\s+(.*?)\s*#*\s*$/
// A top-level (unindented) field line: `- Label: value`, `- **Label:** value`,
// `**Label**: value`, or `Label: value`.
const FIELD_RE =
  /^(?:[-*+]\s+)?(?:\*\*|__)?([A-Za-z][A-Za-z ]{0,30}?)(?:\*\*|__)?\s*:\s*(?:\*\*|__)?\s*(.*)$/
const MD_LINK_RE = /\[([^\]]*)\]\(([^)\s]*)\)/g
const PATH_CHARS_RE = /^[A-Za-z0-9_.\-/@+~]+$/

const collapse = (s) => s.replace(/\s+/g, ' ').trim()

// Drop a leading enumerator ("1.", "F2:", "H-3 —") from a heading; "S3 bucket" stays.
function cleanTitle(heading) {
  return stripMarkdownEmphasis(heading)
    .replace(/^(?:\d+[.)]|[A-Za-z]{1,2}-?\d+\s*[.:)—–-]|#\d+)\s*(?=\S)/, '')
    .trim()
}

const stripMarkdownEmphasis = (s) => s.replace(/\*\*|__/g, '').trim()

// Split a Location value on `;`, `,` and newlines, but never inside (), [] or backticks.
function splitLocations(value) {
  const pieces = []
  let depth = 0
  let inTick = false
  let current = ''
  for (const ch of value) {
    if (ch === '`') inTick = !inTick
    else if (!inTick && (ch === '(' || ch === '[')) depth++
    else if (!inTick && (ch === ')' || ch === ']')) depth = Math.max(0, depth - 1)
    if (!inTick && depth === 0 && (ch === ';' || ch === ',' || ch === '\n')) {
      pieces.push(current)
      current = ''
    } else {
      current += ch
    }
  }
  pieces.push(current)
  return pieces
    .map((p) =>
      p
        .trim()
        .replace(/^[-*+]\s+/, '')
        .trim(),
    )
    .filter(Boolean)
}

function defaultTopLevelFiles(root) {
  if (!root) return () => false
  let names = null
  return (name) => {
    if (names === null) {
      try {
        names = new Set(
          fs
            .readdirSync(root, { withFileTypes: true })
            .filter((d) => d.isFile())
            .map((d) => d.name),
        )
      } catch {
        names = new Set()
      }
    }
    return names.has(name)
  }
}

// `path`, `path:12`, `path:12-14`, `path:12:5`, `path#L12` → {path, line}; null when
// the token is not a path (a path contains `/` or names a top-level file).
function parsePathToken(token, { root, isTopLevelFile }) {
  const cleaned = token.replace(/`/g, '').replace(/[.,;:]+$/, '')
  const m = cleaned.match(/^(.*?)(?:(?::|#L)(\d+)(?:(?:[-–:]|-L)\d+)?)?$/)
  let p = m?.[1] ?? ''
  const line = m?.[2] ? Number(m[2]) : null
  if (!p || /^[a-z]+:\/\//i.test(p) || !PATH_CHARS_RE.test(p)) return null
  if (!p.includes('/') && !isTopLevelFile(p)) return null
  return { ...repoRelativePath(p, root), line }
}

// Repo-relative and normalised; an absolute path outside `root` stays absolute and
// is flagged. Markdown and JSON records both go through here so a path's spelling
// (`./x`, `x`, `<root>/x`) never changes its fingerprint. The root's physical
// spelling counts too, since a reviewer may print a symlinked root's realpath (on
// macOS /var and /tmp are links into /private); it is resolved only when the lexical
// prefix misses, and a root that does not resolve has the lexical prefix alone.
function repoRelativePath(p, root) {
  if (!p.startsWith('/')) return { path: path.posix.normalize(p) }
  if (root) {
    const absRoot = path.resolve(root)
    const under = (prefix) => prefix !== null && p.startsWith(`${prefix}/`)
    const prefix = under(absRoot) ? absRoot : realOrNull(fs.realpathSync, absRoot)
    if (under(prefix)) return { path: path.posix.normalize(p.slice(prefix.length + 1)) }
  }
  return { path: p, outsideRoot: true }
}

function parseLocation(piece, opts) {
  const raw = piece
  const links = [...piece.matchAll(MD_LINK_RE)]
  const text = piece.replace(MD_LINK_RE, '$1').trim()
  const [first = '', ...rest] = text.split(/\s+/)
  let parsed = parsePathToken(first, opts)
  let note = rest.join(' ')
  // A link whose text is not a path ("here") may still point at one.
  if (!parsed && links.length > 0) {
    parsed = parsePathToken(links[0][2], opts)
    note = text
  }
  if (!parsed) return null
  note = note
    .trim()
    .replace(/^[-–—:]\s*/, '')
    .replace(/^\(([\s\S]*)\)$/, '$1')
    .trim()
  const loc = { path: parsed.path, line: parsed.line, note, raw }
  if (parsed.outsideRoot) loc.outsideRoot = true
  return loc
}

// Only closed samples without report severity fields suppress structural parsing.
// Unclosed fences remain transparent so a stray opener cannot hide later findings.
function fencedLineMask(lines) {
  const masked = new Set()
  for (let i = 0; i < lines.length; i += 1) {
    const opener = lines[i].match(/^\s*(`{3,}|~{3,})(.*)$/)
    if (!opener || (opener[1][0] === '`' && opener[2].includes('`'))) continue
    let end = i + 1
    for (; end < lines.length; end += 1) {
      const closer = lines[end].match(/^\s*(`{3,}|~{3,})\s*$/)
      if (closer && closer[1][0] === opener[1][0] && closer[1].length >= opener[1].length) break
    }
    if (end === lines.length) continue
    const hasSeverity = lines.slice(i + 1, end).some((line) => {
      const field = !/^\s/.test(line) && line.match(FIELD_RE)
      return field && FIELD_ALIASES[field[1].trim().toLowerCase()] === 'severity'
    })
    if (!hasSeverity) {
      for (let j = i; j <= end; j += 1) masked.add(j)
    }
    i = end
  }
  return masked
}

/**
 * Parse one reviewer report. A block starts at any `##`–`####` heading and is a
 * finding when it carries Severity and Location, or a lead with Outcome, Location
 * and Unresolved; every other block is
 * returned in `skipped` with its heading. Tolerant by design: labels are matched
 * case-insensitively with aliases, bold or not, and continuation lines extend the
 * current field.
 *
 * @returns {{records: FindingRecord[], skipped: {heading: string, reason: string}[]}}
 */
export function parseFindingsMarkdown(text, opts = {}) {
  const { source = 'claude', root } = opts
  const lens = opts.lens ?? DEFAULT_LENS[source] ?? source
  const lensFamily = opts.lensFamily ?? DEFAULT_LENS_FAMILY[source] ?? lens
  const pathOpts = { root, isTopLevelFile: opts.isTopLevelFile ?? defaultTopLevelFiles(root) }

  const blocks = []
  let block = null
  let field = null
  const lines = String(text ?? '').split(/\r?\n/)
  const fenced = fencedLineMask(lines)
  for (const [i, line] of lines.entries()) {
    const heading = !fenced.has(i) && line.match(HEADING_RE)
    if (heading) {
      const level = heading[1].length
      block = level >= 2 && level <= 4 ? { heading: heading[2], fields: {} } : null
      if (block) blocks.push(block)
      field = null
      continue
    }
    if (!block) continue
    if (line.trim() === '') {
      field = null
      continue
    }
    const unindented = !/^\s/.test(line)
    const fm = !fenced.has(i) && unindented ? line.match(FIELD_RE) : null
    if (fm) {
      const key = FIELD_ALIASES[fm[1].trim().toLowerCase()]
      // `- Dockerfile:12` inside a Location field is a location, not a label.
      const locationContinuation =
        !key && field === 'location' && parseLocation(line.replace(/^[-*+]\s+/, ''), pathOpts)
      if (!locationContinuation && (key || /^[-*+]\s/.test(line))) {
        // An unknown bulleted label ends the current field without being captured.
        field = key && !(key in block.fields) ? key : null
        if (field) block.fields[field] = [fm[2]]
        continue
      }
    }
    if (field) block.fields[field].push(line.trim())
  }

  const records = []
  const skipped = []
  for (const b of blocks) {
    const title = cleanTitle(b.heading)
    const get = (k) => collapse((b.fields[k] ?? []).join(' '))
    const severityRaw = get('severity')
    const outcome = normaliseOutcome(get('outcome'))
    const unresolved = get('unresolved')
    const locationRaw = (b.fields.location ?? []).join('\n').trim()
    if (!severityRaw && outcome !== 'needs_validation') {
      skipped.push({ heading: title, reason: 'no-severity' })
      continue
    }
    if (!locationRaw) {
      skipped.push({ heading: title, reason: 'no-location' })
      continue
    }
    if (outcome === 'needs_validation' && !unresolved) {
      skipped.push({ heading: title, reason: 'no-unresolved-fact' })
      continue
    }
    const locations = splitLocations(locationRaw)
      .map((piece) => parseLocation(piece, pathOpts))
      .filter(Boolean)
    // One builder owns record construction, so the two input shapes cannot drift.
    records.push(
      normaliseRecord(
        {
          title,
          severityRaw,
          outcome,
          unresolved,
          gate: get('gate'),
          locations,
          defect: get('defect'),
          impact: get('impact'),
          fix: get('fix'),
          confidenceRaw: get('confidence'),
        },
        { source, lens, lensFamily, root },
      ),
    )
  }
  return { records, skipped }
}

// Caller-owned provenance: a caller that names the source decides lens and family
// too, because lensFamily feeds the fingerprint and a record that picked its own
// would move into another lens's ledger namespace. Input provenance is read only
// when the caller supplied none.
function provenance(input, defaults) {
  const claim = defaults.source ? {} : input
  const source = defaults.source ?? claim.source ?? 'codex'
  const lens = defaults.lens ?? claim.lens ?? DEFAULT_LENS[source] ?? source
  const lensFamily = defaults.lensFamily ?? claim.lensFamily ?? DEFAULT_LENS_FAMILY[source] ?? lens
  return { source, lens, lensFamily }
}

// An integer line stays and a decimal-digit string becomes its number; an absent
// line is null. Anything else is kept verbatim so verification reports it as
// line-out-of-range instead of reading it as "no line" and checking the whole file.
function normaliseLine(line) {
  if (line === undefined || line === null || line === '') return null
  if (typeof line === 'string' && /^\d+$/.test(line)) return Number(line)
  return line
}

// Only the fields a location is made of; verification verdicts and outsideRoot are
// derived downstream and from the path, never taken from input.
function normaliseLocation(l, root) {
  const { path: p, outsideRoot } = repoRelativePath(l.path, root)
  const line = normaliseLine(l.line)
  const loc = {
    path: p,
    line,
    note: String(l.note ?? ''),
    raw: String(l.raw ?? `${l.path}${line !== null ? `:${line}` : ''}`),
  }
  if (outsideRoot) loc.outsideRoot = true
  return loc
}

/**
 * Build a schema record from raw fields — the single constructor for markdown blocks
 * and JSON records (the Codex runner, the history scan). The record is built from an
 * allowlist: provenance comes from `defaults` (see provenance()), severity and
 * confidence are re-normalised from their raw values, the primary path is the first
 * location's, and the fingerprint is always recomputed here so it has exactly one
 * definition. `defaults.root` makes location paths repo-relative. A preset
 * locationStatus is kept only when the caller passes `trustLocationStatus` (collect
 * does so for the history scan's file alone, which reads the blob directly).
 */
export function normaliseRecord(input, defaults = {}) {
  const { source, lens, lensFamily } = provenance(input, defaults)
  const severityRaw = String(input.severityRaw ?? input.severity ?? '')
  const outcome = normaliseOutcome(input.outcome)
  const { severity, severityUnparsed } =
    outcome === 'needs_validation'
      ? { severity: null, severityUnparsed: false }
      : normaliseSeverity(severityRaw)
  const confidenceRaw = String(input.confidenceRaw ?? input.confidence ?? '')
  const locations = (Array.isArray(input.locations) ? input.locations : [])
    .filter((l) => l && typeof l.path === 'string' && l.path)
    .map((l) => normaliseLocation(l, defaults.root))
  const rec = {
    source,
    lens,
    lensFamily,
    title: String(input.title ?? ''),
    severity,
    severityRaw,
    outcome,
    unresolved: String(input.unresolved ?? ''),
    gate: String(input.gate ?? ''),
    ...(severityUnparsed ? { severityUnparsed: true } : {}),
    locations,
    primaryPath: locations[0]?.path ?? null,
    defect: String(input.defect ?? ''),
    impact: String(input.impact ?? ''),
    fix: String(input.fix ?? ''),
    confidence: normaliseConfidence(confidenceRaw),
    confidenceRaw,
  }
  rec.fingerprint = fingerprint(rec)
  if (defaults.trustLocationStatus === true && LOCATION_STATUSES.has(input.locationStatus)) {
    rec.locationStatus = input.locationStatus
  }
  if (input.evidence !== undefined) rec.evidence = input.evidence
  return rec
}

// ---------------------------------------------------------------------------
// Location verification.

const TOKEN_WINDOW = 20

function noteTokens(note) {
  return [...String(note ?? '').matchAll(/`([^`]+)`/g)]
    .map((m) => m[1].replace(/\(.*\)$/s, '').trim())
    .filter((t) => t.length >= 3)
}

const escapesRoot = (rel) => rel === '' || rel.startsWith('..') || path.isAbsolute(rel)

function realOrNull(realpath, p) {
  try {
    return realpath(p)
  } catch {
    return null
  }
}

function checkLocation(loc, { root, readFile, realpath }) {
  const absRoot = path.resolve(root)
  const abs = path.resolve(absRoot, loc.path)
  if (loc.outsideRoot || escapesRoot(path.relative(absRoot, abs))) return 'outside-root'
  // readFile follows symlinks, so containment is judged on the resolved target too.
  // Never read an unresolved target: readFile could follow an unchecked link.
  const realAbs = realOrNull(realpath, abs)
  if (realAbs === null) return 'missing-file'
  const realRoot = realOrNull(realpath, absRoot) ?? absRoot
  if (escapesRoot(path.relative(realRoot, realAbs))) return 'outside-root'
  // A directory component could still change before the read; closing that race
  // requires a portable openat/O_NOFOLLOW walk, outside this verifier's scope.
  let text
  try {
    text = String(readFile(realAbs))
  } catch {
    return 'missing-file'
  }
  const lines = text.split('\n')
  if (lines.length > 1 && lines.at(-1) === '') lines.pop()
  if (loc.line !== null && loc.line !== undefined) {
    if (!Number.isInteger(loc.line) || loc.line < 1 || loc.line > lines.length) {
      return 'line-out-of-range'
    }
  }
  const tokens = noteTokens(loc.note)
  if (tokens.length === 0) return null
  const window =
    loc.line === null || loc.line === undefined
      ? lines
      : lines.slice(Math.max(0, loc.line - 1 - TOKEN_WINDOW), loc.line + TOKEN_WINDOW)
  const haystack = window.join('\n')
  return tokens.some((t) => haystack.includes(t)) ? null : 'token-not-found'
}

/**
 * Mark each location verified only when its file resolves inside `root` and that
 * exact resolved path can be read (symlinks followed), the line is within the file, and (when its note has backticked tokens
 * of three or more characters) one token appears within ±20 lines. The record's locationStatus
 * follows its primary location. A record arriving with locationStatus keeps it.
 * Returns new records; nothing is dropped and inputs are not mutated.
 */
export function verifyLocations(
  records,
  { root, readFile = (p) => fs.readFileSync(p, 'utf8'), realpath = fs.realpathSync } = {},
) {
  return records.map((rec) => {
    if (rec.locationStatus) return rec
    const locations = (rec.locations ?? []).map((loc) => {
      const reason = checkLocation(loc, { root: root ?? process.cwd(), readFile, realpath })
      return { ...loc, verified: reason === null, reason }
    })
    const primary = locations[0]
    return {
      ...rec,
      locations,
      locationStatus: primary?.verified ? 'location-verified' : 'location-unverified',
    }
  })
}

// ---------------------------------------------------------------------------
// Merge.

const reporterKey = (r) => `${r.source}\u0000${r.lens}`

// Highest severity, then highest confidence, then source/lens/title — a total order
// so the representative member does not depend on input order.
function compareMembers(a, b) {
  return (
    severityRank(b.severity) - severityRank(a.severity) ||
    confidenceRank(b.confidence) - confidenceRank(a.confidence) ||
    reporterKey(a).localeCompare(reporterKey(b)) ||
    String(a.title).localeCompare(String(b.title)) ||
    JSON.stringify(a.locations).localeCompare(JSON.stringify(b.locations))
  )
}

/**
 * Group by fingerprint. Each merged record keeps every {source, lens} as
 * `reporters`, the maximum severity and confidence, the union of locations,
 * location-verified when any member verified, and the text fields of the
 * highest-severity member. Sorted by severity then fingerprint, so reruns are
 * byte-stable.
 */
export function mergeFindings(records) {
  const groups = new Map()
  for (const rec of records) {
    const list = groups.get(rec.fingerprint) ?? []
    list.push(rec)
    groups.set(rec.fingerprint, list)
  }
  const merged = []
  for (const members of groups.values()) {
    members.sort(compareMembers)
    const [base] = members
    const reporters = [
      ...new Map(members.map((m) => [reporterKey(m), { source: m.source, lens: m.lens }])).values(),
    ].sort((a, b) => reporterKey(a).localeCompare(reporterKey(b)))
    const seen = new Set()
    const locations = []
    for (const loc of members.flatMap((m) => m.locations ?? [])) {
      const key = `${loc.path}\u0000${loc.line ?? ''}`
      if (seen.has(key)) continue
      seen.add(key)
      locations.push(loc)
    }
    const confidence = members.reduce(
      (best, m) => (confidenceRank(m.confidence) > confidenceRank(best) ? m.confidence : best),
      base.confidence,
    )
    const out = {
      ...base,
      outcome: members.some((m) => normaliseOutcome(m.outcome) === 'finding')
        ? 'finding'
        : 'needs_validation',
      reporters,
      confidence,
      locations,
      primaryPath: base.primaryPath ?? locations[0]?.path ?? null,
    }
    if (members.some((m) => m.severityUnparsed)) out.severityUnparsed = true
    if (members.some((m) => m.locationStatus === 'location-verified')) {
      out.locationStatus = 'location-verified'
    } else if (members.some((m) => m.locationStatus)) {
      out.locationStatus = 'location-unverified'
    }
    merged.push(out)
  }
  return merged.sort(
    (a, b) =>
      severityRank(b.severity) - severityRank(a.severity) ||
      (a.fingerprint < b.fingerprint ? -1 : a.fingerprint > b.fingerprint ? 1 : 0),
  )
}

/**
 * Records sharing a primary path and defect class but not a fingerprint — most often
 * Codex and Claude reporting one defect. Surfaced for the orchestrator to judge;
 * never merged automatically, because merging two distinct defects in one file
 * would suppress the second as already filed.
 */
export function nearDuplicateCandidates(records) {
  const byKey = new Map()
  for (const rec of records) {
    if (!rec.primaryPath) continue
    const defectClass = classifyDefect(rec.title, rec.defect)
    const key = `${rec.primaryPath}\u0000${defectClass}`
    const entry = byKey.get(key) ?? { primaryPath: rec.primaryPath, defectClass, fps: new Set() }
    entry.fps.add(rec.fingerprint)
    byKey.set(key, entry)
  }
  const pairs = []
  for (const { primaryPath, defectClass, fps } of byKey.values()) {
    const sorted = [...fps].sort()
    for (let i = 0; i < sorted.length; i++) {
      for (let j = i + 1; j < sorted.length; j++) {
        pairs.push({ fingerprints: [sorted[i], sorted[j]], primaryPath, defectClass })
      }
    }
  }
  return pairs.sort(
    (a, b) =>
      a.primaryPath.localeCompare(b.primaryPath) ||
      a.defectClass.localeCompare(b.defectClass) ||
      a.fingerprints.join().localeCompare(b.fingerprints.join()),
  )
}

// ---------------------------------------------------------------------------
// Ledger.

export const SECURITY_MARKER_PREFIX = 'SecurityFinding: '

// Same shape as sweep-sentry-gate's MARKED_ISSUES_QUERY (all states, archived
// included), plus the title and state the ledger policy needs.
export const SECURITY_LEDGER_QUERY = `query SecurityLedger($filter: IssueFilter!, $after: String) {
  issues(first: 250, filter: $filter, after: $after, includeArchived: true) {
    nodes { identifier title description state { type name } }
    pageInfo { hasNextPage endCursor }
  }
}`

const FINGERPRINT_RE = /^sf-[0-9a-f]{12}$/
// Line-anchored like sweep-maingate-gate's MAINGATE_MARKER_RE: the content class
// excludes all four ECMAScript line terminators.
const SECURITY_MARKER_RE = /^SecurityFinding: ([^\r\n\u2028\u2029]+?)[ \t]*$/gm

export function markerLine(fp) {
  if (!FINGERPRINT_RE.test(String(fp))) throw new Error(`not a security fingerprint: ${fp}`)
  return `${SECURITY_MARKER_PREFIX}${fp}`
}

/** Unique marker values, in order of appearance, from a Linear description. */
export function parseSecurityMarkers(description) {
  const out = []
  for (const m of String(description ?? '').matchAll(SECURITY_MARKER_RE)) {
    if (!out.includes(m[1])) out.push(m[1])
  }
  return out
}

// A ledger issue without a state type means the query drifted; a partial ledger
// must never be classified (fail-closed, like the truncated-scan error).
function stateType(issue) {
  const type = issue?.state?.type
  if (typeof type !== 'string' || !type) {
    throw new Error(
      `ledger issue ${issue?.identifier ?? '<unknown>'} has no state — ledger read is incomplete`,
    )
  }
  return type
}

/**
 * Attach `ledger: {status, issues, reason}` to each record:
 * no carrier → new; any non-completed carrier (open, in progress, triage, canceled,
 * Duplicate) → known; only completed carriers → regression-candidate.
 */
export function classifyAgainstLedger(records, ledgerIssues) {
  const carriers = new Map()
  for (const issue of ledgerIssues ?? []) {
    const type = stateType(issue)
    for (const fp of parseSecurityMarkers(issue.description)) {
      const list = carriers.get(fp) ?? []
      list.push({ identifier: issue.identifier, type, name: issue.state.name })
      carriers.set(fp, list)
    }
  }
  return records.map((rec) => {
    const found = carriers.get(rec.fingerprint) ?? []
    const open = found.filter((c) => c.type !== 'completed')
    let ledger
    if (found.length === 0) {
      ledger = { status: 'new', issues: [], reason: 'no ledger issue carries this fingerprint' }
    } else if (open.length > 0) {
      ledger = {
        status: 'known',
        issues: found.map((c) => c.identifier),
        reason: `carried by a non-completed issue (${open.map((c) => `${c.identifier}: ${c.name ?? c.type}`).join(', ')})`,
      }
    } else {
      ledger = {
        status: 'regression-candidate',
        issues: found.map((c) => c.identifier),
        reason: 'carried only by completed issues — the defect may have regressed',
      }
    }
    return { ...rec, ledger }
  })
}

// First repo-relative path (with an extension, optional :line) in a description,
// never one inside a URL.
const DESCRIPTION_PATH_RE =
  /(?<![\w./:@~-])((?:[A-Za-z0-9_.@+~-]+\/)+[A-Za-z0-9_@+~-]*\.[A-Za-z0-9]+)(?::(\d+))?/

function firstPath(description) {
  const m = String(description ?? '').match(DESCRIPTION_PATH_RE)
  return m ? `${m[1]}${m[2] ? `:${m[2]}` : ''}` : null
}

const naturalKey = (id) => {
  const m = String(id).match(/^(.*?)(\d+)$/)
  return m ? [m[1], Number(m[2])] : [String(id), -1]
}
function compareIdentifiers(a, b) {
  const [pa, na] = naturalKey(a)
  const [pb, nb] = naturalKey(b)
  return pa.localeCompare(pb) || na - nb
}

/**
 * Markdown list of everything reviewers should not re-report: every non-completed
 * ledger issue (canceled ones tagged `[accepted risk]`) plus `extraIssues` (open
 * issues carrying no marker, e.g. children of security epics). No cap: a truncated
 * list makes reviewers re-report filed items.
 */
export function renderKnownList(ledgerIssues, extraIssues = []) {
  const entries = new Map()
  for (const issue of ledgerIssues ?? []) {
    const type = stateType(issue)
    if (type === 'completed') continue
    entries.set(issue.identifier, { issue, acceptedRisk: type === 'canceled' })
  }
  for (const issue of extraIssues ?? []) {
    if (entries.has(issue.identifier)) continue
    if (issue.state?.type === 'completed') continue
    entries.set(issue.identifier, { issue, acceptedRisk: issue.state?.type === 'canceled' })
  }
  return [...entries.values()]
    .sort((a, b) => compareIdentifiers(a.issue.identifier, b.issue.identifier))
    .map(({ issue, acceptedRisk }) => {
      const where = firstPath(issue.description)
      return `- ${issue.identifier}: ${collapse(String(issue.title ?? ''))}${where ? ` (${where})` : ''}${acceptedRisk ? ' [accepted risk]' : ''}`
    })
    .join('\n')
}

/** Fetch every SecurityFinding-marked issue (all states, archived included; fail-closed on truncation). */
export async function fetchSecurityLedger({
  apiKey,
  linearRequest: request = linearRequest,
  maxPages = 20,
} = {}) {
  const issues = await fetchMarkedLinearIssues({
    apiKey,
    linearRequest: request,
    maxPages,
    markerPrefix: SECURITY_MARKER_PREFIX,
    query: SECURITY_LEDGER_QUERY,
  })
  for (const issue of issues) stateType(issue)
  return issues
}

// ---------------------------------------------------------------------------
// Collect: one run dir → merged.json.

const issuesOf = (doc) =>
  Array.isArray(doc) ? doc : Array.isArray(doc?.issues) ? doc.issues : null

// Lenses file: {"<id>": "<family>"}, {"<id>": {"family": "<family>"}} or [{id, family}].
function lensFamilies(doc) {
  const map = new Map()
  const entries = Array.isArray(doc)
    ? doc.map((l) => [l?.id, l?.family ?? l?.lensFamily])
    : Object.entries(doc ?? {}).map(([id, v]) => [
        id,
        typeof v === 'string' ? v : (v?.family ?? v?.lensFamily),
      ])
  for (const [id, family] of entries) if (id && family) map.set(String(id), String(family))
  return map
}

// A JSON source holds records (an array, or {records}) or a markdown report
// ({markdown} or a bare string); both are accepted.
function recordsFromJsonSource(doc, defaults, root, trustLocationStatus) {
  if (typeof doc === 'string' || typeof doc?.markdown === 'string') {
    return parseFindingsMarkdown(typeof doc === 'string' ? doc : doc.markdown, {
      ...defaults,
      root,
    })
  }
  const list = Array.isArray(doc) ? doc : Array.isArray(doc?.records) ? doc.records : null
  if (!list) throw new Error(`unrecognised findings JSON for source ${defaults.source}`)
  return {
    records: list.map((r) => normaliseRecord(r, { ...defaults, root, trustLocationStatus })),
    skipped: [],
  }
}

/**
 * Read every source in RUN_LAYOUT that exists under `runDir`, parse, verify, merge
 * and classify; returns {doc, counts, warnings}. I/O is injected.
 */
export function collectRun({ runDir, root, lenses, ledger, readFile, readDir, exists }) {
  const families = lensFamilies(lenses)
  const ledgerIssues = issuesOf(ledger)
  if (!ledgerIssues) throw new Error('ledger must be an array of issues (or {issues: [...]})')
  const warnings = []
  const parsed = []
  const skipped = []

  const findingsDir = path.join(runDir, RUN_LAYOUT.findingsDir)
  const lensFiles = exists(findingsDir)
    ? readDir(findingsDir)
        .filter((f) => f.endsWith('.md'))
        .sort()
    : []
  for (const file of lensFiles) {
    const lens = file.slice(0, -'.md'.length)
    let lensFamily = families.get(lens)
    if (!lensFamily) {
      lensFamily = lens
      warnings.push(`lens ${lens} is not in the lenses file; using its id as its family`)
    }
    const out = parseFindingsMarkdown(readFile(path.join(findingsDir, file)), {
      source: 'claude',
      lens,
      lensFamily,
      root,
    })
    parsed.push(...out.records)
    skipped.push(...out.skipped.map((s) => ({ lens, ...s })))
  }

  // Only the history scan's file may preset locationStatus: it reads the blob
  // directly. Trust is decided here by the file, never by a record's own claim.
  for (const [rel, source, trustLocationStatus] of [
    [RUN_LAYOUT.codexFindings, 'codex', false],
    [RUN_LAYOUT.secretScanFindings, 'history-scan', true],
    [RUN_LAYOUT.tenantScanFindings, 'tenant-scan', false],
  ]) {
    const file = path.join(runDir, rel)
    if (!exists(file)) continue
    const defaults = { source, lens: DEFAULT_LENS[source], lensFamily: DEFAULT_LENS_FAMILY[source] }
    const out = recordsFromJsonSource(
      JSON.parse(readFile(file)),
      defaults,
      root,
      trustLocationStatus,
    )
    parsed.push(...out.records)
    skipped.push(...out.skipped.map((s) => ({ lens: defaults.lens, ...s })))
  }

  const verified = verifyLocations(parsed, { root, readFile })
  const records = classifyAgainstLedger(mergeFindings(verified), ledgerIssues)
  const nearDuplicates = nearDuplicateCandidates(records)
  const count = (status) => records.filter((r) => r.ledger.status === status).length
  const counts = {
    parsed: parsed.length,
    skippedBlocks: skipped.length,
    merged: records.length,
    new: count('new'),
    known: count('known'),
    regressionCandidates: count('regression-candidate'),
    locationUnverified: records.filter((r) => r.locationStatus !== 'location-verified').length,
    nearDuplicatePairs: nearDuplicates.length,
  }
  const doc = {
    schemaVersion: RECORD_SCHEMA_VERSION,
    counts,
    records,
    skipped,
    nearDuplicates,
    warnings,
  }
  return { doc, counts, warnings }
}

// ---------------------------------------------------------------------------
// CLI. Exit 0 on success, 2 on usage or input errors (including a missing key or a
// truncated ledger scan).

const USAGE = `usage:
  bs-security-findings.mjs parse <file> --source <claude|codex|history-scan|tenant-scan> --lens <id> [--lens-family <f>] [--root <dir>]
  bs-security-findings.mjs collect --run-dir <dir> --lenses <file> --ledger <file> [--root <dir>]
  bs-security-findings.mjs ledger --out <file>
  bs-security-findings.mjs known-list --ledger <file> [--extra <file>]`

class UsageError extends Error {}

const COMMANDS = {
  parse: {
    options: {
      source: { type: 'string' },
      lens: { type: 'string' },
      'lens-family': { type: 'string' },
      root: { type: 'string' },
    },
    required: ['source', 'lens'],
    positionals: 1,
  },
  collect: {
    options: {
      'run-dir': { type: 'string' },
      lenses: { type: 'string' },
      ledger: { type: 'string' },
      root: { type: 'string' },
    },
    required: ['run-dir', 'lenses', 'ledger'],
    positionals: 0,
  },
  ledger: { options: { out: { type: 'string' } }, required: ['out'], positionals: 0 },
  'known-list': {
    options: { ledger: { type: 'string' }, extra: { type: 'string' } },
    required: ['ledger'],
    positionals: 0,
  },
}

function parseCommand(argv) {
  const [cmd, ...rest] = argv
  const spec = COMMANDS[cmd]
  if (!spec) throw new UsageError(cmd ? `unknown subcommand: ${cmd}` : 'missing subcommand')
  let parsed
  try {
    parsed = parseArgs({
      args: rest,
      options: spec.options,
      allowPositionals: spec.positionals > 0,
      strict: true,
    })
  } catch (err) {
    throw new UsageError(err.message)
  }
  for (const name of spec.required) {
    if (!parsed.values[name]) throw new UsageError(`${cmd}: --${name} is required`)
  }
  if (parsed.positionals.length !== spec.positionals) {
    throw new UsageError(`${cmd}: expected ${spec.positionals} positional argument(s)`)
  }
  return { cmd, values: parsed.values, positionals: parsed.positionals }
}

const readJson = (readFile, file) => JSON.parse(readFile(file))

/** Run one CLI invocation; returns {stdout, stderr}. Throws on any failure. */
export async function runCli(
  argv,
  {
    readFile = (f) => fs.readFileSync(f, 'utf8'),
    writeFile = (f, s) => fs.writeFileSync(f, s),
    readDir = (d) => fs.readdirSync(d),
    exists = (p) => fs.existsSync(p),
    env = process.env,
    request = linearRequest,
    cwd = process.cwd(),
  } = {},
) {
  const { cmd, values, positionals } = parseCommand(argv)
  const root = path.resolve(cwd, values.root ?? '.')
  switch (cmd) {
    case 'parse': {
      if (!SOURCES.has(values.source)) {
        throw new UsageError(`parse: --source must be one of ${[...SOURCES].join(', ')}`)
      }
      const out = parseFindingsMarkdown(readFile(positionals[0]), {
        source: values.source,
        lens: values.lens,
        lensFamily: values['lens-family'],
        root,
      })
      return { stdout: JSON.stringify(out, null, 2), stderr: '' }
    }
    case 'collect': {
      const runDir = path.resolve(cwd, values['run-dir'])
      const { doc, counts, warnings } = collectRun({
        runDir,
        root,
        lenses: readJson(readFile, values.lenses),
        ledger: readJson(readFile, values.ledger),
        readFile,
        readDir,
        exists,
      })
      writeFile(path.join(runDir, RUN_LAYOUT.merged), `${JSON.stringify(doc, null, 2)}\n`)
      return {
        stdout: JSON.stringify(counts),
        stderr: warnings.map((w) => `warning: ${w}`).join('\n'),
      }
    }
    case 'ledger': {
      const apiKey = env.LINEAR_API_KEY
      if (!apiKey) throw new UsageError('ledger: LINEAR_API_KEY is not set')
      const issues = await fetchSecurityLedger({ apiKey, linearRequest: request })
      writeFile(values.out, `${JSON.stringify(issues, null, 2)}\n`)
      return { stdout: JSON.stringify({ issues: issues.length }), stderr: '' }
    }
    case 'known-list': {
      const ledger = issuesOf(readJson(readFile, values.ledger))
      if (!ledger) throw new UsageError('known-list: --ledger must hold an array of issues')
      const extra = values.extra ? issuesOf(readJson(readFile, values.extra)) : []
      if (!extra) throw new UsageError('known-list: --extra must hold an array of issues')
      return { stdout: renderKnownList(ledger, extra), stderr: '' }
    }
  }
  throw new UsageError(`unknown subcommand: ${cmd}`)
}

if (isMainModule(import.meta.url)) {
  try {
    const { stdout, stderr } = await runCli(process.argv.slice(2))
    if (stderr) process.stderr.write(`${stderr}\n`)
    process.stdout.write(`${stdout}\n`)
  } catch (err) {
    process.stderr.write(`bs-security-findings: ${err.message}\n`)
    if (err instanceof UsageError) process.stderr.write(`${USAGE}\n`)
    process.exit(2)
  }
}
