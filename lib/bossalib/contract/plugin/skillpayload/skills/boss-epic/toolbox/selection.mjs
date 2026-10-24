#!/usr/bin/env node
// skills-toolbox/selection.mjs
// The one candidate-selection vocabulary every pipeline stage reads: labels, assignees, creators
// and projects, each with an `include` and an `exclude` slot, a shared block, per-stage overrides
// and eight shared CLI flags. node builtins only (this module is vendored into dependency-free
// installed toolboxes).
//
// A LEAF module on purpose. It imports nothing from skill-config.mjs (which imports it for
// validation), so the two cannot form an ESM cycle, and it does no I/O: tracker lookups are the
// adapter's job, handed back here as a plain resolution map. That keeps every rule below testable
// without a network. The one tracker-specific piece is the explicitly named Linear renderer.
//
// Fail-closed throughout: an unknown or legacy key is rejected (an ignored filter WIDENS the
// scan), a ref that did not resolve throws naming itself rather than dropping its slot, and an
// issue that lacks a field a configured slot needs never matches.

import { isMainModule } from './main-module.mjs'

export const SELECTION_DIMENSIONS = Object.freeze(['labels', 'assignees', 'creators', 'projects'])
export const SELECTION_POLARITIES = Object.freeze(['include', 'exclude'])
export const SELECTION_STAGES = Object.freeze([
  'plan',
  'build',
  'epic',
  'verify',
  'retro',
  'release',
])

/** The user-valued dimensions: each value is `me`, a tracker user id, or an email. */
const USER_DIMENSIONS = new Set(['assignees', 'creators'])

/** The eight shared flags, each mapped to the one `(dimension, polarity)` slot it sets. */
export const SELECTION_FLAGS = Object.freeze({
  '--label': Object.freeze(['labels', 'include']),
  '--exclude-label': Object.freeze(['labels', 'exclude']),
  '--assignee': Object.freeze(['assignees', 'include']),
  '--exclude-assignee': Object.freeze(['assignees', 'exclude']),
  '--creator': Object.freeze(['creators', 'include']),
  '--exclude-creator': Object.freeze(['creators', 'exclude']),
  '--project': Object.freeze(['projects', 'include']),
  '--exclude-project': Object.freeze(['projects', 'exclude']),
})

/**
 * The label ROLES each stage's core ANDs into every scan (resolved by the caller through
 * `labelName`). `needsHuman` is not here: it is excluded by every stage, by the core, and a
 * user's `labels.exclude` replaces their own list without ever re-admitting it.
 */
const STAGE_LABEL_ROLES = Object.freeze({
  plan: Object.freeze(['agentPlan']),
  build: Object.freeze(['agentBuild']),
  epic: Object.freeze([]),
  verify: Object.freeze([]),
  retro: Object.freeze([]),
  release: Object.freeze([]),
})

// Linear user ids are UUIDs; the API rejects any other string in an `id` comparator.
const UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i
const EMAIL_RE = /^[^\s@]+@[^\s@]+\.[^\s@]+$/

const isPlainObject = (value) =>
  value !== null && typeof value === 'object' && !Array.isArray(value)

/** Whether a string is shaped like a tracker id (a UUID). */
export function isTrackerId(value) {
  return typeof value === 'string' && UUID_RE.test(value)
}

/** Whether a user selector is one of the accepted forms: `me`, a user id, or an email. */
export function isUserSelector(value) {
  return value === 'me' || isTrackerId(value) || EMAIL_RE.test(value)
}

/** The stage-label roles the core requires for `stage`. Throws on an unknown stage. */
export function stageLabelRoles(stage) {
  if (!Object.hasOwn(STAGE_LABEL_ROLES, stage)) {
    throw new Error(
      `selection: unknown stage ${JSON.stringify(stage)}; expected one of ${SELECTION_STAGES.join(', ')}`,
    )
  }
  return [...STAGE_LABEL_ROLES[stage]]
}

function validateDimension(value, path, dimension) {
  if (dimension === 'labels' && Array.isArray(value)) {
    return [`${path} as an array was removed; use ${path}: {include: [...]}`]
  }
  if (!isPlainObject(value)) {
    return [`${path} must be an object with optional include and exclude arrays`]
  }
  const errors = []
  for (const [key, list] of Object.entries(value)) {
    if (!SELECTION_POLARITIES.includes(key)) {
      errors.push(`${path} has unknown key ${JSON.stringify(key)}; allowed keys: include, exclude`)
      continue
    }
    if (!Array.isArray(list)) {
      errors.push(`${path}.${key} must be an array of non-empty strings`)
      continue
    }
    for (const entry of list) {
      if (typeof entry !== 'string' || entry.trim() === '') {
        errors.push(
          `${path}.${key} entries must be non-empty strings; got ${JSON.stringify(entry)}`,
        )
      } else if (USER_DIMENSIONS.has(dimension) && !isUserSelector(entry)) {
        errors.push(
          `${path}.${key} entry ${JSON.stringify(entry)} is not a user selector; use me, a user id (UUID), or an email`,
        )
      }
    }
  }
  return errors
}

function validateDimensions(block, path, { allowStages }) {
  const errors = []
  for (const [key, value] of Object.entries(block)) {
    if (SELECTION_DIMENSIONS.includes(key)) {
      errors.push(...validateDimension(value, `${path}.${key}`, key))
    } else if (key === 'stages' && allowStages) {
      errors.push(...validateStages(value, `${path}.stages`))
    } else if (key === 'assigneeOrCreator') {
      errors.push(
        `${path}.assigneeOrCreator was removed; use ${path}.assignees.include and/or ${path}.creators.include`,
      )
    } else {
      const allowed = allowStages ? [...SELECTION_DIMENSIONS, 'stages'] : SELECTION_DIMENSIONS
      errors.push(
        `${path} has unknown key ${JSON.stringify(key)}; allowed keys: ${allowed.join(', ')}`,
      )
    }
  }
  return errors
}

function validateStages(stages, path) {
  if (!isPlainObject(stages)) return [`${path} must be an object keyed by stage`]
  const errors = []
  for (const [stage, block] of Object.entries(stages)) {
    if (!SELECTION_STAGES.includes(stage)) {
      errors.push(
        `${path} has unknown stage ${JSON.stringify(stage)}; allowed stages: ${SELECTION_STAGES.join(', ')}`,
      )
      continue
    }
    if (!isPlainObject(block)) {
      errors.push(`${path}.${stage} must be an object of dimension blocks`)
      continue
    }
    errors.push(...validateDimensions(block, `${path}.${stage}`, { allowStages: false }))
  }
  return errors
}

/**
 * Validate a raw `selection` block. Returns every error (empty when valid) rather than throwing,
 * so the config validator can turn each into its own `fail(...)` with its own prefix.
 * @param {unknown} block
 * @param {string} path the config path to name in each message
 * @returns {string[]}
 */
export function validateSelectionBlock(block, path = 'selection') {
  if (!isPlainObject(block)) return [`${path} must be an object when present`]
  return validateDimensions(block, path, { allowStages: true })
}

/**
 * Parse the eight shared selection flags out of an argv. Each is repeatable and comma-separated,
 * in both `--flag value` and `--flag=value` forms. Every token that is not a selection flag (or its
 * value) is returned in `positionals`, in order, for the caller to interpret.
 * @param {string[]} argv
 * @returns {{flags: object, positionals: string[]} | {error: string}}
 */
export function parseSelectionFlags(argv = []) {
  const flags = {}
  const positionals = []
  for (let i = 0; i < argv.length; i += 1) {
    const token = argv[i]
    const eq = typeof token === 'string' && token.startsWith('--') ? token.indexOf('=') : -1
    const name = eq === -1 ? token : token.slice(0, eq)
    const slot = typeof name === 'string' ? SELECTION_FLAGS[name] : undefined
    if (!slot || !Object.hasOwn(SELECTION_FLAGS, name)) {
      positionals.push(token)
      continue
    }
    let value
    if (eq !== -1) {
      value = token.slice(eq + 1)
    } else {
      value = argv[i + 1]
      if (typeof value === 'string' && value.startsWith('--')) value = undefined
      else i += 1
    }
    if (typeof value !== 'string' || value.trim() === '') {
      return { error: `${name} requires a non-empty value` }
    }
    const entries = value.split(',').map((entry) => entry.trim())
    if (entries.some((entry) => entry === '')) {
      return { error: `${name} ${JSON.stringify(value)} carries an empty entry` }
    }
    const [dimension, polarity] = slot
    flags[dimension] ??= {}
    flags[dimension][polarity] ??= []
    for (const entry of entries) {
      if (!flags[dimension][polarity].includes(entry)) flags[dimension][polarity].push(entry)
    }
  }
  return { flags, positionals }
}

/** Whether a parsed flag set names any selection slot at all. */
export function hasSelectionFlags(flags) {
  return SELECTION_DIMENSIONS.some((d) => isPlainObject(flags?.[d]) && Object.keys(flags[d]).length)
}

/**
 * The effective per-slot selection for one stage. Precedence is PER SLOT (dimension x polarity):
 * a CLI flag, then `stages.<stage>`, then the shared block. A higher source replaces that one slot
 * and never merges into it or touches the other polarity, so `--exclude-label infra` keeps a
 * configured `labels.include`. Pure; never throws on a validated block.
 * @returns {Record<string, {include: string[], exclude: string[]}>}
 */
export function effectiveSelection(block, stage, flags = {}) {
  stageLabelRoles(stage) // validates the stage name
  const stageBlock = block?.stages?.[stage]
  const out = {}
  for (const dimension of SELECTION_DIMENSIONS) {
    out[dimension] = {}
    for (const polarity of SELECTION_POLARITIES) {
      const value =
        flags?.[dimension]?.[polarity] ??
        stageBlock?.[dimension]?.[polarity] ??
        block?.[dimension]?.[polarity] ??
        []
      out[dimension][polarity] = [...value]
    }
  }
  return out
}

/**
 * Check that a value handed across a seam is an effective (or resolved) selection — only the four
 * dimensions, each with only `include`/`exclude` arrays of non-empty strings — and return it in
 * full shape. Throws naming the first fault: a misspelt dimension silently ignored would WIDEN.
 * `undefined`/`null` is the empty selection.
 */
export function assertEffectiveSelection(selection, caller = 'selection') {
  if (selection === undefined || selection === null) return effectiveSelection(null, 'epic')
  if (!isPlainObject(selection)) throw new Error(`${caller}: selection must be an object`)
  for (const [dimension, slots] of Object.entries(selection)) {
    if (!SELECTION_DIMENSIONS.includes(dimension)) {
      throw new Error(
        `${caller}: unknown selection dimension ${JSON.stringify(dimension)}; expected ${SELECTION_DIMENSIONS.join(', ')}`,
      )
    }
    if (!isPlainObject(slots))
      throw new Error(`${caller}: selection.${dimension} must be an object`)
    for (const [polarity, values] of Object.entries(slots)) {
      if (!SELECTION_POLARITIES.includes(polarity)) {
        throw new Error(
          `${caller}: unknown selection slot ${dimension}.${JSON.stringify(polarity)}`,
        )
      }
      if (
        !Array.isArray(values) ||
        !values.every((v) => typeof v === 'string' && v.trim() !== '')
      ) {
        throw new Error(
          `${caller}: selection.${dimension}.${polarity} must be an array of non-empty strings`,
        )
      }
    }
  }
  return effectiveSelection({ ...selection }, 'epic')
}

/** The non-empty slots of an effective (or resolved) selection, in the canonical order. */
export function selectionSlots(selection) {
  const slots = []
  for (const dimension of SELECTION_DIMENSIONS) {
    for (const polarity of SELECTION_POLARITIES) {
      const values = selection?.[dimension]?.[polarity] ?? []
      if (values.length > 0) slots.push({ dimension, polarity, values: [...values] })
    }
  }
  return slots
}

/** Whether a selection narrows anything at all. */
export function isNarrowed(selection) {
  return selectionSlots(selection).length > 0
}

/**
 * The users, labels and projects an effective selection needs resolved. All three lists are empty
 * when nothing is configured, so the zero-config path makes zero extra tracker requests.
 * @returns {{users: string[], labels: string[], projects: string[]}}
 */
export function selectionResolutionRefs(effective) {
  const collect = (dimensions) => {
    const seen = []
    for (const dimension of dimensions) {
      for (const polarity of SELECTION_POLARITIES) {
        for (const value of effective?.[dimension]?.[polarity] ?? []) {
          if (!seen.includes(value)) seen.push(value)
        }
      }
    }
    return seen
  }
  return {
    users: collect(['assignees', 'creators']),
    labels: collect(['labels']),
    projects: collect(['projects']),
  }
}

/** Whether a ref set needs any tracker lookup. */
export function hasResolutionRefs(refs) {
  return ['users', 'labels', 'projects'].some((kind) => (refs?.[kind] ?? []).length > 0)
}

/**
 * Turn an effective selection into a resolved one: users and projects become ids, labels become
 * every canonical label name the tracker returned for them (a case-insensitive match keeps every
 * case variant). Any ref missing from `resolved` throws naming it — a slot is never dropped.
 * @param {object} effective
 * @param {{users?: Record<string,string|string[]>, labels?: Record<string,string|string[]>, projects?: Record<string,string|string[]>}} resolved each value maps to one id/name or several (a same-named project in two teams, every case variant of a label)
 */
export function applyResolution(effective, resolved = {}) {
  const missing = []
  const lookup = (kind, map, value) => {
    const hit = Object.hasOwn(map ?? {}, value) ? map[value] : undefined
    const ok =
      (typeof hit === 'string' && hit !== '') ||
      (Array.isArray(hit) && hit.length > 0 && hit.every((v) => typeof v === 'string' && v !== ''))
    if (!ok) missing.push(`${kind} ${JSON.stringify(value)}`)
    return ok ? hit : null
  }
  const out = {}
  for (const dimension of SELECTION_DIMENSIONS) {
    out[dimension] = {}
    for (const polarity of SELECTION_POLARITIES) {
      const values = []
      for (const value of effective?.[dimension]?.[polarity] ?? []) {
        let mapped
        if (dimension === 'labels') mapped = lookup('label', resolved.labels, value)
        else if (dimension === 'projects') mapped = lookup('project', resolved.projects, value)
        else mapped = lookup('user', resolved.users, value)
        for (const entry of mapped === null ? [] : [mapped].flat()) {
          if (!values.includes(entry)) values.push(entry)
        }
      }
      out[dimension][polarity] = values
    }
  }
  if (missing.length > 0) {
    throw new Error(
      `selection: could not resolve ${missing.join(', ')}; refusing to drop the filter and widen the scan`,
    )
  }
  return out
}

// The relation field each id-valued dimension filters on, in the tracker's issue model.
const ID_DIMENSION_FIELDS = Object.freeze([
  ['assignees', 'assignee'],
  ['creators', 'creator'],
  ['projects', 'project'],
])

const uniq = (values) => [...new Set(values)]

/**
 * Render a resolved selection plus the core-owned stage rules as a Linear `IssueFilter`. Uses only
 * shapes probed against the live API: `labels.some`/`labels.every` with `eq`/`in`/`nin`, a
 * top-level `and` array, `<relation>.id.in/nin`, and `<relation>.null`. An exclude slot keeps an
 * issue with no value for that relation (unassigned, creator-less, project-less).
 */
export function renderLinearIssueFilter(
  selection,
  { state, team, requireLabels = [], excludeLabels = [] } = {},
) {
  const filter = {}
  if (state) filter.state = { name: { eq: state } }
  if (team) filter.team = { name: { eq: team } }
  const and = []
  for (const label of requireLabels) and.push({ labels: { some: { name: { eq: label } } } })
  const include = selection?.labels?.include ?? []
  if (include.length > 0) and.push({ labels: { some: { name: { in: [...include] } } } })
  const exclude = uniq([...excludeLabels, ...(selection?.labels?.exclude ?? [])])
  if (exclude.length > 0) and.push({ labels: { every: { name: { nin: exclude } } } })
  for (const [dimension, field] of ID_DIMENSION_FIELDS) {
    const ids = selection?.[dimension] ?? {}
    if ((ids.include ?? []).length > 0) and.push({ [field]: { id: { in: [...ids.include] } } })
    if ((ids.exclude ?? []).length > 0) {
      and.push({
        or: [{ [field]: { null: true } }, { [field]: { id: { nin: [...ids.exclude] } } }],
      })
    }
  }
  if (and.length > 0) filter.and = and
  return filter
}

// Read one id-valued relation off any of the three issue shapes. `undefined` means the fetched
// issue does not carry the field at all; `null` means it carries it and it is empty.
function relationId(issue, flatKeys, objectKey) {
  for (const key of flatKeys) {
    if (issue[key] !== undefined) return issue[key] ?? null
  }
  const value = issue[objectKey]
  if (value === null) return null
  if (isPlainObject(value)) return typeof value.id === 'string' ? value.id : undefined
  return undefined
}

function issueLabels(issue) {
  const raw = issue.labels
  const list = Array.isArray(raw) ? raw : Array.isArray(raw?.nodes) ? raw.nodes : undefined
  if (list === undefined) return undefined
  return list
    .map((entry) => (typeof entry === 'string' ? entry : entry?.name))
    .filter((name) => typeof name === 'string')
}

const RELATIONS = Object.freeze({
  assignees: { field: 'assignee', read: (i) => relationId(i, ['assigneeId'], 'assignee') },
  creators: {
    field: 'creator',
    read: (i) => relationId(i, ['creatorId', 'createdById'], 'creator'),
  },
  projects: { field: 'project', read: (i) => relationId(i, ['projectId'], 'project') },
})

const fmtList = (values) => `[${values.join(', ')}]`

/**
 * Evaluate a resolved selection plus the core stage rules against ONE already-fetched issue, in
 * memory — the same predicates `renderLinearIssueFilter` puts on the wire, in the same order.
 * Accepts a raw GraphQL issue, an MCP `get_issue` payload, or a `normalizeTicket` row. A field the
 * issue does not carry never passes a slot that needs it.
 * @returns {{matches: boolean, reason: string|null}}
 */
export function matchIssue(issue, selection, { requireLabels = [], excludeLabels = [] } = {}) {
  const fail = (reason) => ({ matches: false, reason })
  if (!isPlainObject(issue)) return fail('issue: not an object')
  const include = selection?.labels?.include ?? []
  const userExclude = selection?.labels?.exclude ?? []
  const needsLabels =
    requireLabels.length + include.length + excludeLabels.length + userExclude.length
  const labels = needsLabels > 0 ? issueLabels(issue) : []
  if (labels === undefined) return fail('labels: field absent on the fetched issue')
  for (const label of requireLabels) {
    if (!labels.includes(label)) return fail(`requireLabels: missing ${JSON.stringify(label)}`)
  }
  if (include.length > 0 && !include.some((label) => labels.includes(label))) {
    return fail(`labels.include: carries none of ${fmtList(include)}`)
  }
  for (const label of excludeLabels) {
    if (labels.includes(label)) return fail(`excludeLabels: carries ${JSON.stringify(label)}`)
  }
  for (const label of userExclude) {
    if (labels.includes(label)) return fail(`labels.exclude: carries ${JSON.stringify(label)}`)
  }
  for (const [dimension, { field, read }] of Object.entries(RELATIONS)) {
    const ids = selection?.[dimension] ?? {}
    const inc = ids.include ?? []
    const exc = ids.exclude ?? []
    if (inc.length === 0 && exc.length === 0) continue
    const id = read(issue)
    if (id === undefined) return fail(`${dimension}: field absent on the fetched issue`)
    if (inc.length > 0 && !inc.includes(id)) {
      return fail(`${dimension}.include: ${field} ${id ?? 'none'} not in ${fmtList(inc)}`)
    }
    if (exc.length > 0 && id !== null && exc.includes(id)) {
      return fail(`${dimension}.exclude: ${field} ${id} is excluded`)
    }
  }
  return { matches: true, reason: null }
}

/**
 * The one-line skip reason a stage gate prints when its scan found nothing. Names the state, the
 * core stage rules and every non-empty selection slot, so a narrowed skip never reads like an
 * empty backlog.
 */
export function describeEmptyScan(
  gate,
  { state, selection, requireLabels = [], excludeLabels = [] },
  { unblocked = false } = {},
) {
  let line = `${gate} gate: no ${unblocked ? 'unblocked ' : ''}${state} issues`
  if (requireLabels.length > 0) line += ` labelled ${requireLabels.join(' + ')}`
  if (excludeLabels.length > 0) line += ` without ${excludeLabels.join(', ')}`
  const slots = selectionSlots(selection)
  if (slots.length > 0) {
    line += ` matching selection ${slots
      .map(({ dimension, polarity, values }) => `${dimension}.${polarity}=${fmtList(values)}`)
      .join(' ')}`
  }
  return line
}

/**
 * Parse a stage gate's argv into selection flags, refusing everything else. Shared by the stage
 * gates so each one fails closed identically on a token it does not understand.
 * @returns {object} the parsed selection flags
 */
export function gateSelectionFlags(gate, argv = []) {
  const parsed = parseSelectionFlags(argv)
  if (parsed.error) throw new Error(parsed.error)
  if (parsed.positionals.length > 0) {
    throw new Error(
      `unexpected argument(s) ${parsed.positionals.map((token) => JSON.stringify(token)).join(' ')}; the ${gate} gate accepts only the shared selection flags`,
    )
  }
  return parsed.flags
}

// The same two ticket-ref spellings boss-epic's `parseTicketRef` accepts: a bare `KEY-123` id or a
// pasted tracker issue URL (`https://<host>/<workspace>/issue/KEY-123/<slug>`).
const TICKET_REF_RE = /^[A-Za-z]+-\d+$/
const TICKET_URL_RE = /^https?:\/\/[^/\s]+\/[^/\s]+\/issue\/[A-Za-z]+-\d+(?:[/?#]|$)/

/**
 * Split a skill's raw argument list into ticket refs, selection-flag tokens (verbatim, ready to
 * forward to `list-planned` / `list-unplanned`) and everything else.
 * @returns {{tickets: string[], selectionArgs: string[], other: string[], error?: string}}
 */
export function splitArgs(argv = []) {
  const parsed = parseSelectionFlags(argv)
  if (parsed.error) return { tickets: [], selectionArgs: [], other: [], error: parsed.error }
  const positional = new Set()
  const selectionArgs = []
  // Re-walk the argv to keep the selection tokens verbatim and in order.
  for (let i = 0; i < argv.length; i += 1) {
    const token = argv[i]
    const name = typeof token === 'string' && token.includes('=') ? token.split('=')[0] : token
    if (Object.hasOwn(SELECTION_FLAGS, name)) {
      selectionArgs.push(token)
      if (!token.includes('=')) selectionArgs.push(argv[(i += 1)])
    } else {
      positional.add(i)
    }
  }
  const tickets = []
  const other = []
  for (const i of positional) {
    const token = argv[i]
    if (TICKET_REF_RE.test(token) || TICKET_URL_RE.test(token)) {
      tickets.push(token)
    } else {
      other.push(token)
    }
  }
  return { tickets, selectionArgs, other }
}

if (isMainModule(import.meta.url)) {
  const [cmd, ...rest] = process.argv.slice(2)
  if (cmd === 'split-args') {
    const args = rest[0] === '--' ? rest.slice(1) : rest
    const result = splitArgs(args)
    process.stdout.write(JSON.stringify(result) + '\n')
    process.exit(result.error ? 2 : 0)
  }
  process.stderr.write('usage: node selection.mjs split-args -- <args...>\n')
  process.exit(64)
}

/** Filing policy for the retrospective writing stage; never used to narrow dedupe. */
export function retroFilingSelection(block, flags = {}) {
  const selection = effectiveSelection(block, 'retro', flags),
    ignored = []
  for (const [dimension, slots] of Object.entries(selection))
    for (const [polarity, values] of Object.entries(slots)) {
      if (polarity === 'include' && ['labels', 'projects'].includes(dimension)) continue
      if (!values.length) continue
      if (
        flags[dimension]?.[polarity] !== undefined ||
        block?.stages?.retro?.[dimension]?.[polarity] !== undefined
      )
        throw new Error(`unsupported retro selection slot ${dimension}.${polarity}`)
      ignored.push(`${dimension}.${polarity}`)
      selection[dimension][polarity] = []
    }
  if (selection.projects.include.length > 1)
    throw new Error('retro supports at most one projects.include')
  return { selection, ignored }
}
