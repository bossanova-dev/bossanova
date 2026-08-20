// skills-toolbox/plan-deps-lib.mjs
//
// The deterministic core of boss-plan's dependency linking — the decision that
// used to live entirely in Phase 4 step 5's prose. Everything here is PURE:
// each function inspects caller-supplied payloads and returns a decision
// object; nothing reads a file, opens a socket, loads config, or throws (the
// one exception is the argument-order guard, which fails loudly on a swapped
// call rather than silently parsing a config object as a description). The
// caller performs every fetch and every write.
//
// PROJECT-AGNOSTIC BY CONSTRUCTION. This module ships inside the published
// `boss-plan` core, which is installed into every user's global skill directory
// across thousands of unrelated repos. So it hard-codes no tracker name, no
// label literal and no repository path: `epicLabel`, `stateRoles`,
// `moduleRoots`, `repoWideTokens` and `areaAliases` are all caller-supplied,
// resolved once by the caller through the tracker adapter / skill config. The
// example paths in these comments are invented (`app/api`, `app/web`), not
// paths in any real repository.
//
// ---------------------------------------------------------------------------
// Why the decision lives here at all
// ---------------------------------------------------------------------------
// Prose an agent can skip is a rail with no gate behind it. Six defects lived
// in one paragraph: a skippable overlap test that let priority orientation
// serialize file-disjoint tickets, a blocking edge written onto already-started
// work, a cleared-state exclusion that dropped a real prerequisite the moment
// it merged, declared relations never considered, epic parents left in the
// comparison set, and fuzzy full-text search used as the overlap oracle. Each
// one is now a named rung with a named `reason` code, so a rejection is
// explainable and a regression is a failing test rather than a re-read.
//
// ---------------------------------------------------------------------------
// The two inlined constants
// ---------------------------------------------------------------------------
// `DEFAULT_CLEARED_STATE_TYPES` + `DEFAULT_CANCELED_STATE_TYPES` and
// `DEFAULT_PRIORITY_ORDER` are deliberate COPIES of values that also live in
// non-vendored toolbox modules. They are copied rather than imported because
// importing either would drag a tracker-named, network-capable module into this
// project-agnostic payload's import closure (this module may import
// `skill-config.mjs` and nothing else). A silent second copy is exactly the
// "prose and gate diverge" failure those modules exist to prevent, so the test
// file — which is never vendored and may import anything — imports the
// originals and asserts the copies still agree with them.
//
// ---------------------------------------------------------------------------
// The result shape, and the three things a naive shape gets wrong
// ---------------------------------------------------------------------------
//   1. `basis` and `source` are SEPARATE fields. `basis` is the justification
//      (`'overlap' | 'logical' | null`); `source` is the provenance
//      (`'backlog-scan' | 'declared-related'`). Conflating them makes a
//      declared-but-non-conflicting candidate look like an edge to write, and
//      makes a blocking edge derived from a declared relation unexplainable.
//   2. A result carries a WRITE INTENT, never a bare direction string. Every
//      write goes through the adapter's `appendDependency` — `{id, blockedBy:
//      [ids]}` — and there is no `blocks` verb, so `write` names the issue that
//      receives the save. Returning `'blocks'` and leaving the caller to
//      re-derive the receiving side makes an inversion a silent corruption.
//   3. Ambiguous orientation is its OWN outcome, not an arbitrary edge. It
//      returns `edge: 'none'` with a `question`, and questions route to a
//      different destination than notes.
//
// `semantic judgment stays with the model`: "this ticket needs the other's
// feature" is not a function of two strings, so the caller supplies it as a
// per-candidate `logicalDependencies` verdict. The agent makes the judgment;
// this module makes the decision.

import { planDescriptionSections, planSections, scanFences } from './skill-config.mjs'

// ---------------------------------------------------------------------------
// Exported constants
// ---------------------------------------------------------------------------

/**
 * The CLOSED enum of `reason` codes. Every `reason` returned by
 * `classifyDependencyEdge` or carried by a `planDependencyEdges` skip entry is
 * a member of this list, so a caller can branch on the code instead of
 * string-matching free prose, and a test can assert the whole table stays
 * inside the enum.
 */
export const DEPENDENCY_REASONS = Object.freeze([
  // Rung 0 — subject guards.
  'subject-is-epic',
  // Rung 1 — identity.
  'self',
  'excluded',
  // Rung 2 — the candidate is not a valid blocker.
  'epic-parent',
  'candidate-not-schedulable',
  // Rung 3 — no basis was established.
  'no-areas',
  'no-overlap',
  'declared-related-no-conflict',
  // Rung 4 — cleared state.
  'candidate-cleared',
  'prerequisite-satisfied',
  'prerequisite-canceled',
  // Rung 5 — orientation.
  'oriented-by-logical',
  'oriented-by-priority',
  'oriented-by-age',
  'ambiguous-orientation',
  // Rung 6 — started-side downgrade.
  'downgraded-subject-started',
  'downgraded-candidate-started',
  'downgraded-unknown-state',
  // Write guard — an established edge whose write intent cannot name both sides.
  'unidentifiable-issue',
  // Set-level outcomes.
  'declared-related-unresolved',
  'no-candidates-compared',
  'no-subject-areas',
])

/**
 * State types that mean a candidate's work LANDED. Split from the canceled set
 * on purpose: rung 4 treats "the prerequisite is satisfied" and "the
 * prerequisite was dropped" as different outcomes, and a single cleared set
 * cannot express that difference.
 */
export const DEFAULT_CLEARED_STATE_TYPES = Object.freeze(['completed'])

/** State types that mean a candidate's work was DROPPED, not delivered. */
export const DEFAULT_CANCELED_STATE_TYPES = Object.freeze(['canceled'])

/**
 * Priority ordering, most- to least-urgent: 1 > 2 > 3 > 4 > 0. `0` means "no
 * priority set" and is therefore the WEAKEST, which is why a plain
 * `a.priority - b.priority` inverts every comparison that involves it.
 */
export const DEFAULT_PRIORITY_ORDER = Object.freeze([1, 2, 3, 4, 0])

// ---------------------------------------------------------------------------
// Module-private tuning defaults
// ---------------------------------------------------------------------------

// Area names too broad to mean "these two tickets touch the same thing". Kept
// deliberately small and generic: a repo closes its own gaps through the
// `repoWideTokens` parameter rather than by growing this list with names that
// only mean something in one checkout.
const DEFAULT_REPO_WIDE_TOKENS = Object.freeze([
  'src',
  'lib',
  'test',
  'tests',
  'spec',
  'docs',
  'doc',
  'build',
  'dist',
  'vendor',
  'node_modules',
])

// The state roles a candidate must occupy to be a valid blocker. Cleared
// candidates are admitted separately (rung 4 has something to say about them);
// everything else — backlog, triage, duplicate, a tracker's own inbox state —
// never produces a pull request, so a block on one never clears.
const SCHEDULABLE_ROLES = Object.freeze(['planned', 'inProgress', 'inReview'])

// Roles that mean an agent may already be mid-task on the ticket.
const STARTED_ROLES = Object.freeze(['inProgress', 'inReview'])

// How deep `planDependencyEdges` will expand epic parents into their children.
// Depth 0 is the caller-supplied candidate set. The cap bounds ONE call, and that
// is all it can bound: a caller that re-runs with freshly fetched children starts
// a new call with a fresh budget, so termination across the documented re-run loop
// is the CALLER's to hold — bound the number of re-runs and pass every
// already-expanded parent id in `excludeIds`, which rung 1 rejects before any
// expansion. Stated here because the opposite claim ("a malformed parent/child
// graph terminates here rather than looping") was true of this module and false of
// the loop that drives it.
const DEFAULT_MAX_EXPANSION_DEPTH = 2

// The `## Key changes` section is resolved from the plan contract by TITLE, so
// the heading's exact spelling (and its `##` prefix) comes from config rather
// than from a literal baked in here.
const KEY_CHANGES_TITLE = 'key changes'

const LIST_MARKER_RE = /^\s*(?:[-*+]|\d+[.)])\s+/
const HEADING_RE = /^\s*#{1,6}\s/
const BACKTICK_SPAN_RE = /`([^`]+)`/g
// A colon or a dash separates an area from its description in the drafting
// template; real plan bodies use an em dash far more often than a colon.
const AREA_DESCRIPTION_SPLIT_RE = /[:—–]/

// ---------------------------------------------------------------------------
// Small shared helpers
// ---------------------------------------------------------------------------

// The argument-order guard `skill-config.mjs` applies to every config-first
// export. Duplicated rather than imported because it is module-private there;
// the alternative is a swapped call parsing a config object as a description
// and reporting every ticket as arealess.
function assertConfigFirst(config, fn) {
  if (typeof config === 'string' || !config || typeof config !== 'object' || !config.planContract) {
    throw new Error(
      `plan-deps-lib: ${fn}(config, description) — arguments look swapped; pass the config first`,
    )
  }
}

function text(value) {
  return typeof value === 'string' ? value : ''
}

/** The id an adapter write must name: the tracker id, or the identifier when that is all we have. */
function issueKey(issue) {
  const id = text(issue?.id).trim()
  if (id !== '') return id
  return text(issue?.identifier).trim() || null
}

/** Every string that identifies this issue, lower-cased — uuid AND identifier. */
function issueAliases(issue) {
  const aliases = []
  for (const raw of [issue?.id, issue?.identifier]) {
    const value = text(raw).trim().toLowerCase()
    if (value !== '') aliases.push(value)
  }
  return aliases
}

/**
 * True when `a` and `b` are the same issue. Compares BOTH fields: a planned-state
 * scan returns uuids while a declared relation returns identifiers, so a self-skip
 * that compared one field would let a ticket block itself.
 */
function sameIssue(a, b) {
  const left = new Set(issueAliases(a))
  return issueAliases(b).some((alias) => left.has(alias))
}

function idSet(ids) {
  const out = new Set()
  for (const raw of Array.isArray(ids) ? ids : []) {
    const value = text(raw).trim().toLowerCase()
    if (value !== '') out.add(value)
  }
  return out
}

function matchesIdSet(issue, set) {
  return issueAliases(issue).some((alias) => set.has(alias))
}

/** Label names off any of the shapes an adapter hands back. */
function labelNames(issue) {
  const raw = Array.isArray(issue?.labels) ? issue.labels : issue?.labels?.nodes
  if (!Array.isArray(raw)) return []
  return raw
    .map((entry) => (typeof entry === 'string' ? entry : text(entry?.name)))
    .map((name) => name.trim().toLowerCase())
    .filter((name) => name !== '')
}

/** An `epicLabel` of undefined/empty means NO candidate is epic — never "every candidate is". */
function carriesLabel(issue, label) {
  const wanted = text(label).trim().toLowerCase()
  if (wanted === '') return false
  return labelNames(issue).includes(wanted)
}

function stateType(issue) {
  return text(issue?.stateType ?? issue?.state?.type)
    .trim()
    .toLowerCase()
}

function stateName(issue) {
  return text(issue?.stateName ?? issue?.state?.name).trim()
}

/**
 * The caller-resolved role for this issue's state, or `null` when the state name
 * is not one the caller mapped. `null` is UNKNOWN, never "fine" — rung 6 degrades
 * on it rather than betting a blocking edge on a state it cannot classify.
 */
function stateRole(issue, stateRoles) {
  const direct = text(issue?.stateRole).trim()
  if (direct !== '') return direct
  const name = stateName(issue)
  if (name === '') return null
  const map = stateRoles && typeof stateRoles === 'object' ? stateRoles : {}
  const hit = map[name] ?? map[name.toLowerCase()]
  const role = text(hit).trim()
  return role === '' ? null : role
}

function priorityValue(priority) {
  if (typeof priority === 'number' && Number.isFinite(priority)) return priority
  // The object form `{value, name}` some adapters return must rank identically
  // to the bare number, or an adapter swap silently reorders every edge.
  if (priority && typeof priority === 'object' && typeof priority.value === 'number') {
    return Number.isFinite(priority.value) ? priority.value : null
  }
  return null
}

/** Rank through the priority ORDER, never by subtraction. Unknown/null ranks last. */
function priorityRank(priority, order) {
  const value = priorityValue(priority)
  const index = value === null ? -1 : order.indexOf(value)
  return index === -1 ? order.length : index
}

function createdAtMillis(issue) {
  const raw = text(issue?.createdAt).trim()
  if (raw === '') return null
  const millis = new Date(raw).getTime()
  return Number.isFinite(millis) ? millis : null
}

// A note carries its own `reason` so a caller reading `notes` alone can still tell
// WHICH rung produced the line without re-deriving it from the prose.
function note(severity, destination, candidate, reason, body) {
  return { severity, destination, candidate, reason, text: body }
}

function question(candidate, body) {
  return { destination: 'open-questions', candidate, text: body }
}

/** The label a note or question uses for an issue: identifier when present, else id. */
function issueLabel(issue) {
  return text(issue?.identifier).trim() || text(issue?.id).trim() || 'the candidate'
}

// ---------------------------------------------------------------------------
// `## Key changes` area extraction
// ---------------------------------------------------------------------------

function normalizeHeadingTitle(heading) {
  return text(heading)
    .replace(/^#+\s*/, '')
    .trim()
    .toLowerCase()
}

function resolveKeyChangesHeading(config, override) {
  const explicit = text(override).trim()
  if (explicit !== '') return explicit
  const sections = planSections(config)
  const match = (Array.isArray(sections) ? sections : []).find(
    (section) => normalizeHeadingTitle(section?.heading) === KEY_CHANGES_TITLE,
  )
  return match ? match.heading : null
}

/**
 * Join wrapped continuation lines into one entry each. A real `## Key changes`
 * bullet routinely wraps a backticked path across two lines; scanning
 * line-by-line splits the span and finds no path at all.
 */
function joinWrappedLines(lines) {
  const entries = []
  let open = false
  for (const raw of lines) {
    const line = text(raw).replace(/\r$/, '')
    if (line.trim() === '') {
      open = false
      continue
    }
    const isHeading = HEADING_RE.test(line)
    if (!open || isHeading || LIST_MARKER_RE.test(line)) {
      entries.push(line.trim())
      open = !isHeading
      continue
    }
    entries[entries.length - 1] += ` ${line.trim()}`
  }
  return entries
}

/**
 * The candidate tokens in one entry. Backtick-delimited spans win outright when
 * present: real plan bodies write paths in backticks and prose around them, so
 * splitting on the description separator first would take a sentence fragment.
 */
function entryTokens(entry) {
  const spans = [...entry.matchAll(BACKTICK_SPAN_RE)].map((match) => match[1])
  if (spans.length > 0) return spans
  return [entry.replace(LIST_MARKER_RE, '').split(AREA_DESCRIPTION_SPLIT_RE)[0]]
}

/**
 * A token normalized to an area, or `null` when it is not path-shaped. Drops
 * commands (anything with whitespace), URLs, flags, and bare symbol names that
 * are not one of the caller's module roots.
 */
function normalizeArea(token, moduleRoots) {
  let value = text(token).replace(/\r/g, '').replace(/\*\*/g, '').trim()
  value = value.replace(/^[`'"(<[]+/, '').replace(/[`'")>\],;.]+$/, '')
  value = value.replace(/:\d+(?:[-–]\d+)?$/, '') // a `path:12-20` line anchor is still that path
  value = value.trim()
  if (value === '') return null
  if (/\s/.test(value)) return null // a command or a phrase, never an area
  if (value.includes('://')) return null // a URL
  if (value.startsWith('-')) return null // a flag
  value = value.replace(/^\.\//, '').replace(/^~\//, '')
  value = value.replace(/\/?\*+$/, '') // a glob suffix names the directory
  value = value.replace(/\/+$/, '')
  value = value.replace(/[.,;:]+$/, '')
  value = value.toLowerCase()
  if (value === '') return null
  if (value.includes('/')) return value
  return moduleRoots.has(value) ? value : null
}

function areasFromLines(lines, moduleRoots) {
  const seen = new Set()
  const areas = []
  for (const entry of joinWrappedLines(lines)) {
    for (const token of entryTokens(entry)) {
      const area = normalizeArea(token, moduleRoots)
      if (area === null || seen.has(area)) continue
      seen.add(area)
      areas.push(area)
    }
  }
  return areas
}

/**
 * Extract the areas a plan description says it touches.
 *
 * Composes `planDescriptionSections` with `scanFences` exactly as the contract's
 * own parsers do — a hand-rolled multiline section regex is the documented way
 * to capture the empty string silently, which would make every candidate look
 * file-disjoint: the very defect this module exists to fix, recreated in the fix.
 *
 * @param {object} config resolved skill config (config-first, like every plan parser)
 * @param {string} description the ticket/plan description
 * @param {{moduleRoots?: string[], keyChangesHeading?: string}} [options]
 *   `moduleRoots` admits bare, slash-free tokens (a repo's top-level module names)
 *   as areas; `keyChangesHeading` overrides the heading resolved from the contract.
 * @returns {{areas: string[], source: 'key-changes'|'fallback-text'|'none'}}
 *   `source` distinguishes THREE outcomes that must never be conflated: the
 *   section was parsed (`key-changes`), the section was absent so the whole
 *   description was scanned (`fallback-text`), or there was no body to read at
 *   all (`none`). A parsed-but-arealess section is `key-changes` with an empty
 *   `areas` — different from `none`, because "we looked and found nothing" and
 *   "there was nothing to look at" carry different weight downstream.
 */
export function extractKeyChangeAreas(config, description, options = {}) {
  assertConfigFirst(config, 'extractKeyChangeAreas')
  const moduleRoots = new Set(
    (Array.isArray(options.moduleRoots) ? options.moduleRoots : [])
      .map((root) => text(root).trim().toLowerCase())
      .filter((root) => root !== ''),
  )
  const body = text(description)
  const heading = resolveKeyChangesHeading(config, options.keyChangesHeading)
  const section = heading
    ? planDescriptionSections(config, body).find((entry) => entry.heading === heading)
    : null

  if (!section) {
    const lines = scanFences(body).lines.map((entry) => entry.line)
    return { areas: areasFromLines(lines, moduleRoots), source: 'fallback-text' }
  }

  const lines = scanFences(section.bodyLines.join('\n')).lines.map((entry) => entry.line)
  if (!lines.some((line) => line.trim() !== '')) return { areas: [], source: 'none' }
  return { areas: areasFromLines(lines, moduleRoots), source: 'key-changes' }
}

// ---------------------------------------------------------------------------
// Area overlap
// ---------------------------------------------------------------------------

function normalizeAreaValue(value) {
  return text(value).trim().replace(/\/+$/, '').toLowerCase()
}

function expandAreas(areas, areaAliases) {
  const aliases = areaAliases && typeof areaAliases === 'object' ? areaAliases : {}
  const out = new Set()
  for (const raw of Array.isArray(areas) ? areas : []) {
    const area = normalizeAreaValue(raw)
    if (area === '') continue
    out.add(area)
    const extra = aliases[area]
    for (const alias of Array.isArray(extra) ? extra : extra === undefined ? [] : [extra]) {
      const value = normalizeAreaValue(alias)
      if (value !== '') out.add(value)
    }
  }
  return [...out]
}

/**
 * The shared region of two areas, or `null`. SEGMENT arrays, never `startsWith`:
 * a prefix test reports `app/apidocs` as inside `app/api`, and in a monorepo two
 * such names are routinely different modules that share nothing.
 */
function sharedRegion(a, b) {
  const left = a.split('/')
  const right = b.split('/')
  const shorter = left.length <= right.length ? left : right
  const longer = shorter === left ? right : left
  for (let index = 0; index < shorter.length; index += 1) {
    if (shorter[index] !== longer[index]) return null
  }
  // The intersection is the DEEPER path: it is the part both tickets touch.
  return longer.join('/')
}

/**
 * Do two area sets touch the same thing?
 *
 * Granularity, stated so it can be argued with rather than discovered: EXACT
 * path or ancestor/descendant containment only. Areas are NOT truncated to
 * module roots — doing so makes every file under one module "the same area",
 * which over-links a monorepo exactly as badly as skipping the overlap test
 * under-links it. It is one predicate and it is pinned by a named test, so it
 * can be flipped with evidence.
 *
 * @param {string[]} a subject areas
 * @param {string[]} b candidate areas
 * @param {{repoWideTokens?: string[], areaAliases?: Record<string,string|string[]>}} [options]
 *   `repoWideTokens` names areas too broad to count as a conflict; they are
 *   excluded from `shared`, not merely from the boolean, so a caller reading
 *   `shared` never sees a token the boolean already ignored, and the exclusion
 *   applies to EITHER side of a containment, not only to the shared region. `areaAliases` maps
 *   an area onto the other areas it stands for — the seam a repo uses to close
 *   a known false negative (generated mirrors of one logical file) WITHOUT
 *   making this module know anything about that repo.
 * @returns {{overlap: boolean, shared: string[]}} `shared` is sorted, so a
 *   re-plan of unchanged tickets produces an unchanged dependency line.
 */
export function areasOverlap(a, b, options = {}) {
  const wide = new Set(
    (Array.isArray(options.repoWideTokens) ? options.repoWideTokens : DEFAULT_REPO_WIDE_TOKENS).map(
      (token) => normalizeAreaValue(token),
    ),
  )
  const left = expandAreas(a, options.areaAliases)
  const right = expandAreas(b, options.areaAliases)
  const shared = new Set()
  for (const x of left) {
    for (const y of right) {
      const region = sharedRegion(x, y)
      // Test BOTH sides, not only the region. `sharedRegion` returns the DEEPER
      // path, so a region test alone never sees the broad token that produced the
      // containment: `['docs']` against `['docs/plans/x.md']` yields the deep path,
      // passes the denylist, and lets a subject whose only area is a repo-wide token
      // phantom-overlap everything beneath it — which rung 5 then orients into a real
      // blocking write.
      if (region === null || wide.has(region) || wide.has(x) || wide.has(y)) continue
      shared.add(region)
    }
  }
  const list = [...shared].sort()
  return { overlap: list.length > 0, shared: list }
}

// ---------------------------------------------------------------------------
// The decision ladder
// ---------------------------------------------------------------------------

function result(fields) {
  return {
    edge: 'none',
    basis: null,
    source: 'backlog-scan',
    reason: null,
    note: null,
    question: null,
    write: null,
    ...fields,
  }
}

/**
 * The caller's semantic verdict, normalized to `{note, direction}`. A falsy verdict
 * means NO logical basis.
 *
 * DIRECTION IS PART OF THE VERDICT, never something orientation can re-derive.
 * "This ticket needs the other's feature" already names which side must land
 * first, and priority and creation order know nothing about it: orienting a
 * logical pair by priority writes the prerequisite as blocked by the ticket that
 * depends on it whenever the prerequisite happens to carry the weaker priority —
 * the wrong-edge class this module exists to remove, produced by the module.
 *
 * `true` and a bare note string both mean the DEFAULT direction: the CANDIDATE is
 * the prerequisite, so the subject is `blockedBy` it. That is the direction rung
 * 4's own messages have always asserted. The reverse — the subject is the
 * prerequisite the candidate needs — must be said out loud as
 * `{direction: 'blocks'}`. Anything else, a misspelled direction included, falls
 * back to the default rather than throwing.
 */
function normalizeLogical(verdict) {
  if (!verdict) return null
  if (verdict === true) return { note: '', direction: 'blockedBy' }
  if (typeof verdict === 'string') return { note: verdict, direction: 'blockedBy' }
  if (typeof verdict === 'object') {
    return {
      note: text(verdict.note),
      direction: text(verdict.direction).trim() === 'blocks' ? 'blocks' : 'blockedBy',
    }
  }
  return null
}

/**
 * Classify ONE subject/candidate pair against the seven-rung ladder.
 *
 * The rungs run in this order and each one either returns or falls through —
 * there is no implicit fallthrough branch, and orientation is unreachable
 * without a basis:
 *
 *   0. subject guards          — an epic subject produces no edges at all
 *   1. identity                — self and caller-excluded ids
 *   2. valid-blocker check     — epic parents and unschedulable states, BEFORE
 *                                the overlap test (an epic parent's description
 *                                is the union of its children's, so it overlaps
 *                                almost everything and reports phantom overlaps)
 *   3. establish a basis       — overlap or the caller's logical verdict
 *   4. cleared state           — a cleared candidate ends an OVERLAP basis quietly,
 *                                and ends a logical basis the SUBJECT is the
 *                                prerequisite for; a logical prerequisite the
 *                                subject needs survives being completed, and a
 *                                CANCELED one is a warning, not a satisfaction
 *   5. orientation             — a LOGICAL basis is already oriented by its own
 *                                verdict; an overlap orients by priority ORDER,
 *                                then older createdAt, else ambiguous-orientation
 *                                with a question
 *   6. started-side downgrade  — symmetric: whichever side would RECEIVE the
 *                                blocking write, if it has already started (or
 *                                sits in a state we cannot classify), gets a
 *                                non-blocking relation instead
 *
 * @param {object} input
 * @param {object} input.subject the ticket being planned
 * @param {object} input.candidate the ticket it is being compared against
 * @param {string[]} [input.subjectAreas] areas from `extractKeyChangeAreas`
 * @param {string[]} [input.candidateAreas]
 * @param {'backlog-scan'|'declared-related'} [input.source]
 * @param {boolean|string|{note?: string, direction?: 'blockedBy'|'blocks'}} [input.logicalDependency]
 *   the caller's semantic verdict for this pair, INCLUDING its direction: `true` or
 *   a bare note means the candidate is the prerequisite (subject `blockedBy` it),
 *   and `{direction: 'blocks'}` says the subject is the one the candidate needs.
 *   Without a verdict `basis: 'logical'` is unreachable and the
 *   cleared-prerequisite rung is dead code.
 * @param {string} [input.epicLabel] undefined means NO candidate is epic
 * @param {string[]} [input.excludeIds] ids that must never become edges
 * @param {Record<string,string>} [input.stateRoles] state name -> role
 * @returns {{edge: 'none'|'blockedBy'|'blocks'|'relatedTo', basis: 'overlap'|'logical'|null,
 *   source: string, reason: string, note: object|null, question: object|null,
 *   write: {id: string, blockedBy: string[]}|null}}
 */
export function classifyDependencyEdge(input = {}) {
  const {
    subject = {},
    candidate = {},
    subjectAreas = [],
    candidateAreas = [],
    source = 'backlog-scan',
    logicalDependency = null,
    epicLabel,
    excludeIds = [],
    stateRoles = {},
    clearedStateTypes = DEFAULT_CLEARED_STATE_TYPES,
    canceledStateTypes = DEFAULT_CANCELED_STATE_TYPES,
    priorityOrder = DEFAULT_PRIORITY_ORDER,
    repoWideTokens,
    areaAliases,
  } = input
  const base = { source }
  const candidateName = issueLabel(candidate)
  const subjectName = issueLabel(subject)

  // Rung 0 — subject guards. A fully built epic parent's description is the
  // union of every child's key changes, so classifying it against the backlog
  // produces a burst of edges onto a container that never merges.
  if (carriesLabel(subject, epicLabel)) {
    return result({
      ...base,
      reason: 'subject-is-epic',
      note: note(
        'info',
        'planning',
        candidateName,
        'subject-is-epic',
        `${subjectName} carries the epic role label, so it takes no dependency edges of its own; its children carry them.`,
      ),
    })
  }

  // Rung 1 — identity. Compares BOTH id fields in both directions.
  if (sameIssue(subject, candidate)) return result({ ...base, reason: 'self' })
  if (matchesIdSet(candidate, idSet(excludeIds))) return result({ ...base, reason: 'excluded' })

  // Rung 2 — is the candidate a valid blocker at all? This precedes the overlap
  // test deliberately; running overlap first reports phantom overlaps for the
  // very candidates that are about to be rejected anyway.
  if (carriesLabel(candidate, epicLabel)) {
    return result({
      ...base,
      reason: 'epic-parent',
      note: note(
        'info',
        'planning',
        candidateName,
        'epic-parent',
        `${candidateName} is an epic parent and never produces a pull request of its own; compare against its active children instead.`,
      ),
    })
  }

  // Caller-supplied tuning is spread defensively: this module's header promises
  // nothing throws, and `new Set(undefined-ish)`/`[...notAnArray]` breaks that.
  const cleared = new Set(
    Array.isArray(clearedStateTypes) ? clearedStateTypes : DEFAULT_CLEARED_STATE_TYPES,
  )
  const canceled = new Set(
    Array.isArray(canceledStateTypes) ? canceledStateTypes : DEFAULT_CANCELED_STATE_TYPES,
  )
  const candidateType = stateType(candidate)
  const candidateCompleted = cleared.has(candidateType)
  const candidateCanceled = canceled.has(candidateType)
  const candidateRole = stateRole(candidate, stateRoles)
  if (
    candidateRole !== null &&
    !SCHEDULABLE_ROLES.includes(candidateRole) &&
    !candidateCompleted &&
    !candidateCanceled
  ) {
    return result({ ...base, reason: 'candidate-not-schedulable' })
  }

  // Rung 3 — establish a basis. No basis means STOP: orientation is unreachable
  // from here, which is what stops priority from serializing disjoint tickets.
  const overlap = areasOverlap(subjectAreas, candidateAreas, { repoWideTokens, areaAliases })
  const logical = normalizeLogical(logicalDependency)
  // Logical outranks overlap: rung 4 keeps a cleared logical prerequisite and
  // drops a cleared overlap, so a pair holding both must take the logical path.
  const basis = logical ? 'logical' : overlap.overlap ? 'overlap' : null
  if (basis === null) {
    if (source === 'declared-related') {
      // The relation already exists in the tracker. Re-writing it is noise, and
      // silently dropping it hides that it WAS considered.
      return result({ ...base, reason: 'declared-related-no-conflict' })
    }
    const arealess =
      expandAreas(subjectAreas, areaAliases).length === 0 ||
      expandAreas(candidateAreas, areaAliases).length === 0
    return result({ ...base, reason: arealess ? 'no-areas' : 'no-overlap' })
  }

  // Rung 4 — cleared state, applied to an OVERLAP basis only.
  if (candidateCompleted || candidateCanceled) {
    // A cleared candidate on an OVERLAP basis is dropped quietly — and so is one the
    // SUBJECT is the prerequisite for: there the dependent side is the one that
    // landed, so no ordering is left to enforce, and reporting it as a satisfied or
    // canceled prerequisite would name the wrong side as the prerequisite.
    if (basis === 'overlap' || logical.direction === 'blocks') {
      return result({ ...base, basis, reason: 'candidate-cleared' })
    }
    if (candidateCompleted) {
      return result({
        ...base,
        basis,
        reason: 'prerequisite-satisfied',
        note: note(
          'info',
          'planning',
          candidateName,
          'prerequisite-satisfied',
          `${candidateName} is the logical prerequisite for ${subjectName} and has already landed; the prerequisite is satisfied, no edge is needed.`,
        ),
      })
    }
    return result({
      ...base,
      basis,
      reason: 'prerequisite-canceled',
      note: note(
        'warning',
        'risks',
        candidateName,
        'prerequisite-canceled',
        `${candidateName} was the logical prerequisite for ${subjectName} and was CANCELED — the prerequisite was dropped, not delivered. Confirm ${subjectName} can still be built before starting it.`,
      ),
    })
  }

  // Rung 5 — orientation. A LOGICAL basis arrives ALREADY oriented: the caller's
  // verdict named which side is the prerequisite, and neither priority nor age may
  // overrule it. Only an overlap — where no one has said which way the dependency
  // runs — is oriented by priority ORDER, then by older createdAt.
  const order = Array.isArray(priorityOrder) ? [...priorityOrder] : [...DEFAULT_PRIORITY_ORDER]
  const subjectRank = priorityRank(subject.priority, order)
  const candidateRank = priorityRank(candidate.priority, order)
  let edge = null
  let reason = null
  if (basis === 'logical') {
    edge = logical.direction
    reason = 'oriented-by-logical'
  } else if (subjectRank !== candidateRank) {
    // The more urgent ticket lands first, so the less urgent one is blocked by it.
    edge = candidateRank < subjectRank ? 'blockedBy' : 'blocks'
    reason = 'oriented-by-priority'
  } else {
    const subjectMillis = createdAtMillis(subject)
    const candidateMillis = createdAtMillis(candidate)
    if (subjectMillis === null || candidateMillis === null || subjectMillis === candidateMillis) {
      // Epic children are created in one batch and routinely share a timestamp.
      // An arbitrary edge here is a coin flip written into the tracker.
      return result({
        ...base,
        basis,
        reason: 'ambiguous-orientation',
        question: question(
          candidateName,
          `${subjectName} and ${candidateName} conflict but their link direction is genuinely balanced (equal priority, no usable creation order). Which must land first?`,
        ),
      })
    }
    edge = candidateMillis < subjectMillis ? 'blockedBy' : 'blocks'
    reason = 'oriented-by-age'
  }

  // Which side RECEIVES the blocking write. Both the write guard below and rung 6
  // need it: the receiving side is the one that must be nameable, and the one that
  // gets stranded if it has already started.
  const blocked = edge === 'blockedBy' ? subject : candidate
  const blocker = edge === 'blockedBy' ? candidate : subject

  // The write guard runs BEFORE rung 6, not after it. A downgrade's `relatedTo` is
  // an instruction to the caller to save the relation too, so it needs both ids
  // exactly as much as a blocking write does; running the guard afterwards returns
  // `{edge: 'relatedTo', write: null}` with both id fields null — an unwritable
  // relation dressed as a decided edge, the one outcome this module promises not to
  // produce. `issueKey` is null when an issue carries neither `id` nor `identifier`.
  const blockedKey = issueKey(blocked)
  const blockerKey = issueKey(blocker)
  if (blockedKey === null || blockerKey === null) {
    return result({
      ...base,
      basis,
      reason: 'unidentifiable-issue',
      note: note(
        'warning',
        'risks',
        candidateName,
        'unidentifiable-issue',
        `A ${basis} dependency between ${subjectName} and ${candidateName} was established, but one side carries neither an id nor an identifier, so no write can name it. Re-fetch that issue with its id before treating the dependency line as complete.`,
      ),
    })
  }

  // Rung 6 — started-side downgrade, applied SYMMETRICALLY. The ticket that
  // RECEIVES the blocking write is the one that gets stranded mid-task, and that
  // is the subject on an inbound edge just as much as the candidate on an
  // outbound one.
  const blockedRole = stateRole(blocked, stateRoles)
  const blockerRole = stateRole(blocker, stateRoles)
  const blockedIsSubject = blocked === subject
  // `null` is UNKNOWN on EITHER side, not only the blocked one. Rung 2 rejects an
  // unschedulable candidate only when its role RESOLVED, so an unmapped state name
  // arrives here as a live prerequisite that may in truth be un-startable — and a
  // blocking edge whose BLOCKER sits in a state this run cannot classify is exactly
  // as unsafe as one whose blocked side does. Testing only the blocked side left
  // this module's own "degrades rather than betting a blocking edge on a state it
  // cannot classify" comment untrue for every pair where the unknown side outranks
  // the other, which is half of them.
  const unclassified = blockedRole === null ? blocked : blockerRole === null ? blocker : null
  const unknownState = unclassified !== null
  if (unknownState || STARTED_ROLES.includes(blockedRole)) {
    const downgradeReason = unknownState
      ? 'downgraded-unknown-state'
      : blockedIsSubject
        ? 'downgraded-subject-started'
        : 'downgraded-candidate-started'
    // Degrading an overlap costs rebase churn; degrading a LOGICAL prerequisite
    // drops a real ordering constraint, so it is not the same note.
    const severity = basis === 'logical' ? 'warning' : 'info'
    const destination = basis === 'logical' ? 'risks' : 'planning'
    // Name the side the note is ABOUT: the unclassified one when that is why we
    // degraded, the stranded one when it started.
    const named = unknownState ? unclassified : blocked
    const why = unknownState
      ? `sits in a state this run could not classify (${stateName(unclassified) || 'no state name'})`
      : 'has already started'
    const body =
      basis === 'logical'
        ? `${issueLabel(named)} ${why}, so the LOGICAL prerequisite between ${issueLabel(subject)} and ${issueLabel(candidate)} is recorded as a non-blocking relation instead of a blocking edge. The ordering constraint is real and is no longer enforced by the tracker — sequence it by hand.`
        : `${issueLabel(named)} ${why}, so the overlap between ${issueLabel(subject)} and ${issueLabel(candidate)} is recorded as a non-blocking relation instead of a blocking edge. Expect rebase churn between the two.`
    return result({
      ...base,
      edge: 'relatedTo',
      basis,
      reason: downgradeReason,
      note: note(severity, destination, candidateName, downgradeReason, body),
    })
  }

  return result({
    ...base,
    edge,
    basis,
    reason,
    write: { id: blockedKey, blockedBy: [blockerKey] },
  })
}

// ---------------------------------------------------------------------------
// The whole candidate set
// ---------------------------------------------------------------------------

function sortKey(issue) {
  return (text(issue?.identifier).trim() || text(issue?.id).trim()).toLowerCase()
}

function compareByKey(a, b) {
  if (a.key < b.key) return -1
  if (a.key > b.key) return 1
  return 0
}

/**
 * Classify a whole candidate set against one subject.
 *
 * Declared relations are seeded as candidates BEFORE any text-overlap test —
 * a relation a human already asserted is evidence, and a text scan that never
 * sees it cannot report on it. A declared id that is absent from the fetched
 * candidate set is reported in `skipped`, never silently dropped.
 *
 * @param {object} input
 * @param {object} input.subject
 * @param {string[]} [input.subjectAreas] defaults to `subject.areas`
 * @param {object[]} [input.candidates] fetched candidates; each MAY carry its own
 *   `areas` (from `extractKeyChangeAreas`) — otherwise it is compared as arealess
 * @param {string[]} [input.declaredRelatedIds] ids of existing declared relations
 * @param {Record<string, boolean|string|{note?: string}>} [input.logicalDependencies]
 *   the caller's per-candidate semantic verdicts, keyed by id OR identifier
 * @param {Record<string, object[]>} [input.childrenByParentId] children of an epic
 *   parent, when the caller has already fetched them: supplying them expands the
 *   parent in place; omitting them reports the parent for expansion instead
 * @returns {{edges: object[], skipped: object[], notes: object[], questions: object[], compared: number}}
 *   `compared` is a LOWER BOUND on the candidates actually evaluated — rung-1
 *   rejections (the subject itself, a caller-excluded id) are not counted — so a
 *   set holding nothing else reports "could not evaluate" (compared 0, plus a
 *   warning note) rather than the indistinguishable "evaluated, found no
 *   dependencies". Each epic-parent entry in `skipped` also carries `expansion`
 *   (`'pending' | 'supplied' | 'depth-capped'`), which separates the two ways
 *   `expandChildren: false` is reached.
 */
export function planDependencyEdges(input = {}) {
  const {
    subject = {},
    candidates = [],
    declaredRelatedIds = [],
    logicalDependencies = {},
    childrenByParentId = {},
    maxExpansionDepth = DEFAULT_MAX_EXPANSION_DEPTH,
    epicLabel,
  } = input
  const subjectAreas = Array.isArray(input.subjectAreas)
    ? input.subjectAreas
    : Array.isArray(subject.areas)
      ? subject.areas
      : []
  const declared = idSet(declaredRelatedIds)
  const verdicts =
    logicalDependencies && typeof logicalDependencies === 'object' ? logicalDependencies : {}
  const children =
    childrenByParentId && typeof childrenByParentId === 'object' ? childrenByParentId : {}

  // Dedupe by unordered pair, normalizing uuid-vs-identifier: a candidate that
  // is BOTH declared and text-overlapping must yield exactly one edge.
  const byAlias = new Map()
  const queue = []
  const enqueue = (issue, depth) => {
    const aliases = issueAliases(issue)
    if (aliases.length === 0) return
    if (aliases.some((alias) => byAlias.has(alias))) return
    const entry = { issue, depth }
    for (const alias of aliases) byAlias.set(alias, entry)
    queue.push(entry)
  }
  for (const issue of Array.isArray(candidates) ? candidates : []) enqueue(issue, 0)

  const skipped = []
  // A declared id we never fetched is a hole in the comparison set, not a clean pass.
  // Reconciliation runs AFTER the queue is drained, never before it: expansion
  // enqueues an epic parent's children mid-loop, so a declared id that lives under a
  // parent is unknown at the start and resolved by the end. Reconciling first
  // reported that id as both a real edge and an unresolved relation, sending the
  // caller back to fetch a ticket this very run had already evaluated.
  const reconcileDeclared = () => {
    for (const id of declared) {
      if (byAlias.has(id)) continue
      skipped.push({
        id,
        identifier: id,
        key: id,
        edge: 'none',
        basis: null,
        source: 'declared-related',
        write: null,
        question: null,
        expandChildren: false,
        reason: 'declared-related-unresolved',
        note: note(
          'warning',
          'risks',
          id,
          'declared-related-unresolved',
          `${id} is a declared relation on ${issueLabel(subject)} but was not in the fetched candidate set, so it was never evaluated. Fetch it by id — a declared relation can sit in any state.`,
        ),
      })
    }
  }

  const edges = []
  let compared = 0
  for (let index = 0; index < queue.length; index += 1) {
    const { issue, depth } = queue[index]
    const aliases = issueAliases(issue)
    // Verdicts may be keyed by either id form, in either case: the agent writes them
    // against whatever the candidate listing showed it.
    const verdictKey = Object.keys(verdicts).find((key) =>
      aliases.includes(key.trim().toLowerCase()),
    )
    const logicalDependency = verdictKey === undefined ? null : verdicts[verdictKey]
    const classified = classifyDependencyEdge({
      ...input,
      subject,
      candidate: issue,
      subjectAreas,
      candidateAreas: Array.isArray(issue?.areas) ? issue.areas : [],
      source: aliases.some((alias) => declared.has(alias)) ? 'declared-related' : 'backlog-scan',
      logicalDependency,
    })
    // Rung-1 rejections are NOT comparisons. The subject's own row and a
    // caller-excluded id were never evaluated against anything, so counting them
    // makes a degenerate set — "here is the ticket itself" — report `compared: 1`,
    // indistinguishable from a clean evaluation that genuinely found nothing, and
    // the `no-candidates-compared` warning that exists for exactly that case never
    // fires.
    if (classified.reason !== 'self' && classified.reason !== 'excluded') compared += 1
    const record = {
      id: text(issue?.id).trim() || null,
      identifier: text(issue?.identifier).trim() || null,
      key: sortKey(issue),
      ...classified,
    }
    if (classified.reason === 'subject-is-epic') {
      // Rung 0 short-circuits the WHOLE set, not just this pair — but it must not
      // discard what the set already knows. Declared ids still get reconciled, and
      // every accumulated note still ships, so an epic subject reports the same
      // holes in its comparison set that any other run would.
      skipped.push({ ...record, expandChildren: false })
      reconcileDeclared()
      skipped.sort(compareByKey)
      return {
        edges: [],
        skipped,
        notes: skipped.map((entry) => entry.note).filter(Boolean),
        questions: [],
        compared,
      }
    }
    if (classified.reason === 'epic-parent') {
      const parentKey = text(issue?.id).trim()
      const fetched = children[parentKey] ?? children[text(issue?.identifier).trim()] ?? null
      const supplied = Array.isArray(fetched)
      const withinDepth = depth < maxExpansionDepth
      if (withinDepth && supplied) {
        // Children re-enter at rung 1 — they are ordinary candidates from here.
        for (const child of fetched) enqueue(child, depth + 1)
      }
      // `expandChildren` is an INSTRUCTION to the caller — "fetch this parent's
      // children and re-run" — not a property of the parent. Reporting it true for
      // a parent whose children were already supplied sends the caller to fetch
      // what it just handed us: a duplicate re-run on a non-empty list, and on a
      // deliberately empty one a caller that can never settle "no active children".
      //
      // `expansion` says WHY the instruction reads the way it does, because the two
      // ways of reaching `false` are not the same event: `supplied` means this run
      // already expanded the parent, `depth-capped` means the cap stopped it and a
      // branch of the graph went unexamined. Collapsed onto one boolean, a capped
      // parent is silently indistinguishable from a fully handled one.
      const expansion = supplied ? 'supplied' : withinDepth ? 'pending' : 'depth-capped'
      skipped.push({ ...record, expandChildren: withinDepth && !supplied, expansion, depth })
      continue
    }
    if (classified.edge === 'none') {
      skipped.push({ ...record, expandChildren: false })
      continue
    }
    edges.push(record)
  }

  reconcileDeclared()
  edges.sort(compareByKey)
  skipped.sort(compareByKey)
  const notes = [...edges, ...skipped].map((entry) => entry.note).filter(Boolean)
  const questions = [...edges, ...skipped].map((entry) => entry.question).filter(Boolean)
  if (compared === 0) {
    notes.push(
      note(
        'warning',
        'risks',
        issueLabel(subject),
        'no-candidates-compared',
        `No candidates were evaluated for ${issueLabel(subject)}, so this run found no dependencies because it compared nothing — not because none exist. Re-run the candidate fetch before treating the dependency line as complete.`,
      ),
    )
  } else if (
    // A DIFFERENT silence from comparing nothing, and the more likely one: the
    // subject contributed no comparable areas, so `compared` is non-zero, every
    // candidate stops at `no-areas` — a note-free rung — and the caller receives
    // a clean, question-free "no dependencies" over a comparison that could never
    // have found one. That is the missed-prerequisite failure this module exists
    // to remove, one rung further in, so it gets its own warning rather than
    // riding on `compared === 0`. Only a logical basis can establish an edge
    // without areas, so the warning stands down when one did.
    expandAreas(subjectAreas, input.areaAliases).length === 0 &&
    !edges.some((entry) => entry.basis === 'logical')
  ) {
    notes.push(
      note(
        'warning',
        'risks',
        issueLabel(subject),
        'no-subject-areas',
        `${issueLabel(subject)} contributed no comparable change areas, so overlap was never testable against any of the ${compared} candidates compared. This run found no dependencies because it had nothing to compare them on — not because none exist. Give the subject a \`## Key changes\` section naming concrete paths, or judge each candidate logically, before treating the dependency line as complete.`,
      ),
    )
  }
  return { edges, skipped, notes, questions, compared }
}
