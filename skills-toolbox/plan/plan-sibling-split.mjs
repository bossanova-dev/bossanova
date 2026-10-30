#!/usr/bin/env node
// plan-sibling-split — the deterministic core of boss-plan's sibling split.
//
// A ticket that already has a parent is never decomposed (that would mint grandchildren boss-epic
// never schedules). When its honest size is ≥ 5 and it is not atomic, the drafter plans it as the
// first part and returns the other parts as `siblings`; Phase 4 files each one as an unplanned
// shell under the SAME parent. This module validates that list, renders each shell body, decides
// which shells already exist (by marker), computes the blocking edges and verifies the read-back.
// node builtins only, no tracker I/O.
//
//   siblings: [{ key, title, estimate, summary, keyChanges[], blockedBy[], blocks[] }]
//
// - `blockedBy` names `source` (the ticket being planned) or another sibling's key.
// - `blocks` names existing issue ids that depend on this part; their edge on the source stays.

import { mkdirSync, readFileSync, writeFileSync } from 'node:fs'
import { join } from 'node:path'
import { parseArgs } from 'node:util'

import { isMainModule } from '../main-module.mjs'
import { descriptionAppearsTruncated } from '../skill-config.mjs'
import { CHILD_MAX_ESTIMATE, assertAcyclic, topoOrderChildren } from './plan-epic-lib.mjs'
import { planScratchFamily } from './plan-scratch-paths.mjs'

/** More parts than this means the source was epic-sized: fall back to `- Oversized-child:`. */
export const SIBLING_MAX = 5

/** The reserved `blockedBy` name for the ticket being planned. */
export const SOURCE_KEY = 'source'

const KEY_RE = /^[a-z0-9-]{1,40}$/
const MARKER_RE = /<!--\s*boss-plan-sibling-of:(\S+)\s+key:([a-z0-9-]{1,40})\s*-->/

const EX_USAGE = 64

/**
 * @typedef {object} Sibling
 * @property {string} key
 * @property {string} title
 * @property {number} estimate
 * @property {string} summary
 * @property {string[]} keyChanges
 * @property {string[]} blockedBy
 * @property {string[]} blocks
 */

const isNonEmptyString = (value) => typeof value === 'string' && value.trim() !== ''
const unique = (values) => [...new Set(values)]

// The DAG shape plan-epic-lib's cycle check and topological order read. `source` already exists,
// so it is never a node.
const asSpec = (siblings) => ({
  children: siblings.map((sibling) => ({
    key: sibling.key,
    blockedByKeys: sibling.blockedBy.filter((dep) => dep !== SOURCE_KEY),
  })),
})

function stringList(value, where, field, errors) {
  if (value === undefined) return []
  if (!Array.isArray(value) || !value.every(isNonEmptyString)) {
    errors.push(`${where}: ${field} must be an array of non-empty strings`)
    return []
  }
  return unique(value.map((entry) => entry.trim()))
}

/**
 * Validate the drafter's `siblings` list. Never throws: a structural problem is an `errors` entry,
 * and `siblings` is the normalized list only when `errors` is empty.
 * @param {unknown} raw
 * @returns {{siblings: Sibling[], errors: string[]}}
 */
export function normalizeSiblings(raw) {
  const errors = []
  if (!Array.isArray(raw)) return { siblings: [], errors: ['siblings must be an array'] }
  if (raw.length === 0 || raw.length > SIBLING_MAX) {
    return {
      siblings: [],
      errors: [
        `siblings must hold 1..${SIBLING_MAX} parts, got ${raw.length}` +
          (raw.length > SIBLING_MAX ? ' (an epic-sized source is an Oversized-child)' : ''),
      ],
    }
  }
  /** @type {Sibling[]} */
  const siblings = []
  raw.forEach((entry, index) => {
    const where = `siblings[${index}]`
    if (entry === null || typeof entry !== 'object' || Array.isArray(entry)) {
      errors.push(`${where}: must be an object`)
      return
    }
    const key = typeof entry.key === 'string' ? entry.key : ''
    if (!KEY_RE.test(key) || key === SOURCE_KEY) {
      errors.push(
        `${where}: key ${JSON.stringify(entry.key)} must match [a-z0-9-]{1,40} and not be "${SOURCE_KEY}"`,
      )
    }
    if (!isNonEmptyString(entry.title)) errors.push(`${where}: title must be a non-empty string`)
    if (!isNonEmptyString(entry.summary))
      errors.push(`${where}: summary must be a non-empty string`)
    if (
      !Number.isInteger(entry.estimate) ||
      entry.estimate < 1 ||
      entry.estimate > CHILD_MAX_ESTIMATE
    ) {
      errors.push(
        `${where}: estimate ${JSON.stringify(entry.estimate)} must be 1..${CHILD_MAX_ESTIMATE}`,
      )
    }
    let keyChanges = []
    if (Array.isArray(entry.keyChanges) && entry.keyChanges.length > 0) {
      keyChanges = stringList(entry.keyChanges, where, 'keyChanges', errors)
    } else {
      errors.push(`${where}: keyChanges must name at least one change`)
    }
    siblings.push({
      key,
      title: String(entry.title ?? '').trim(),
      estimate: entry.estimate,
      summary: String(entry.summary ?? '').trim(),
      keyChanges,
      blockedBy: stringList(entry.blockedBy, where, 'blockedBy', errors),
      blocks: stringList(entry.blocks, where, 'blocks', errors),
    })
  })
  if (errors.length > 0) return { siblings: [], errors }

  const keys = siblings.map((sibling) => sibling.key)
  const duplicates = unique(keys.filter((key, index) => keys.indexOf(key) !== index))
  if (duplicates.length > 0) errors.push(`duplicate sibling keys: ${duplicates.join(', ')}`)
  const known = new Set([SOURCE_KEY, ...keys])
  for (const sibling of siblings) {
    const unknown = sibling.blockedBy.filter((dep) => !known.has(dep))
    if (unknown.length > 0) {
      errors.push(`sibling "${sibling.key}" is blockedBy unknown key(s): ${unknown.join(', ')}`)
    }
  }
  if (errors.length === 0) {
    try {
      assertAcyclic(asSpec(siblings))
    } catch (error) {
      errors.push(`siblings: ${error.message}`)
    }
  }
  return errors.length > 0 ? { siblings: [], errors } : { siblings, errors }
}

function requireSiblings(raw) {
  const { siblings, errors } = normalizeSiblings(raw)
  if (errors.length > 0) throw new Error(`plan-sibling-split: ${errors.join('; ')}`)
  return siblings
}

/**
 * The marker each sibling shell carries; re-runs find an existing shell by it.
 * @param {string} sourceId
 * @param {string} key
 * @returns {string}
 */
export function siblingMarker(sourceId, key) {
  return `<!-- boss-plan-sibling-of:${sourceId} key:${key} -->`
}

/**
 * @param {unknown} description
 * @returns {{sourceId: string, key: string} | null}
 */
export function parseSiblingMarker(description) {
  if (typeof description !== 'string') return null
  const match = description.match(MARKER_RE)
  return match ? { sourceId: match[1], key: match[2] } : null
}

/**
 * The unplanned shell body. A later planner treats it as the reporter's notes, so it carries the
 * scope, its provenance and the marker, and nothing that looks like a finished plan.
 * @param {{sourceId: string, sourceTitle: string, sibling: Sibling}} input
 * @returns {string}
 */
export function siblingBody({ sourceId, sourceTitle, sibling }) {
  return [
    '## Summary',
    '',
    sibling.summary,
    '',
    '## Key changes',
    '',
    ...sibling.keyChanges.map((change) => `- ${change}`),
    '',
    `Split out of ${sourceId} (${sourceTitle}) by boss-plan: this is part \`${sibling.key}\` of a ` +
      `sibling split the planner made because ${sourceId} was too large for one PR. ${sourceId} ` +
      'keeps the first part; plan this part on its own.',
    '',
    siblingMarker(sourceId, sibling.key),
    '',
  ].join('\n')
}

const issueRef = (issue) =>
  isNonEmptyString(issue?.identifier)
    ? issue.identifier
    : isNonEmptyString(issue?.id)
      ? issue.id
      : null

/**
 * Decide which shells to create and which already exist. A live child of the parent carrying the
 * marker for (source, key) is reused, never duplicated. THROWS when a child's description is
 * truncated (it may hide a marker; re-read it with get_issue) or two children claim one key.
 * @param {{sourceId: string, sourceTitle: string, parentId: string, siblings: unknown,
 *          existingChildren?: unknown}} input
 * @returns {{creates: {key: string, title: string, estimate: number, body: string}[],
 *            reused: Record<string, string>}}
 */
export function siblingSplitPlan({ sourceId, sourceTitle, parentId, siblings, existingChildren }) {
  if (
    !isNonEmptyString(sourceId) ||
    !isNonEmptyString(sourceTitle) ||
    !isNonEmptyString(parentId)
  ) {
    throw new Error('plan-sibling-split: sourceId, sourceTitle and parentId are required')
  }
  const list = requireSiblings(siblings)
  const byKey = new Map(list.map((sibling) => [sibling.key, sibling]))
  /** @type {Map<string, string>} */
  const existing = new Map()
  for (const child of Array.isArray(existingChildren) ? existingChildren : []) {
    const ref = issueRef(child)
    if (descriptionAppearsTruncated(child?.description)) {
      throw new Error(
        `plan-sibling-split: ${ref ?? 'a child'} has a truncated description; re-read it with get_issue`,
      )
    }
    const marker = parseSiblingMarker(child?.description)
    if (!marker || marker.sourceId !== sourceId || ref === null) continue
    const prior = existing.get(marker.key)
    if (prior !== undefined && prior !== ref) {
      throw new Error(
        `plan-sibling-split: ${prior} and ${ref} both carry the marker for ${sourceId} key ${marker.key}; a human keeps one`,
      )
    }
    existing.set(marker.key, ref)
  }
  const creates = []
  /** @type {Record<string, string>} */
  const reused = {}
  for (const { key } of topoOrderChildren(asSpec(list))) {
    const sibling = /** @type {Sibling} */ (byKey.get(key))
    const id = existing.get(key)
    if (id !== undefined) {
      reused[key] = id
      continue
    }
    creates.push({
      key,
      title: sibling.title,
      estimate: sibling.estimate,
      body: siblingBody({ sourceId, sourceTitle, sibling }),
    })
  }
  return { creates, reused }
}

/**
 * The blocking edges to append, deduplicated: `{issue, blockedBy}` means `issue` is blocked by
 * `blockedBy`. Additive only; nothing here removes an edge. THROWS when a sibling has no id yet or
 * `blocks` names the source or a sibling (either would loop back through the split).
 * @param {{sourceId: string, siblings: unknown, idByKey: Record<string, string>}} input
 * @returns {{issue: string, blockedBy: string}[]}
 */
export function siblingEdges({ sourceId, siblings, idByKey }) {
  const list = requireSiblings(siblings)
  const ids = idByKey && typeof idByKey === 'object' ? idByKey : {}
  const idOf = (key) => {
    if (key === SOURCE_KEY) return sourceId
    if (!isNonEmptyString(ids[key]))
      throw new Error(`plan-sibling-split: no issue id for sibling "${key}"`)
    return ids[key]
  }
  const siblingIds = new Set(list.map((sibling) => idOf(sibling.key)))
  const edges = new Map()
  const add = (issue, blockedBy) => edges.set(`${issue}\u0000${blockedBy}`, { issue, blockedBy })
  for (const sibling of list) {
    const id = idOf(sibling.key)
    for (const dep of sibling.blockedBy) add(id, idOf(dep))
    for (const dependent of sibling.blocks) {
      if (dependent === sourceId || siblingIds.has(dependent)) {
        throw new Error(
          `plan-sibling-split: sibling "${sibling.key}" blocks ${dependent}, which is part of this split; use blockedBy`,
        )
      }
      add(dependent, id)
    }
  }
  return [...edges.values()]
}

const parentRefs = (issue) =>
  [
    issue?.parentId,
    issue?.parent?.id,
    issue?.parent?.identifier,
    typeof issue?.parent === 'string' ? issue.parent : null,
  ].filter(isNonEmptyString)

/**
 * Verify each sibling read back from the tracker: it exists, sits under the parent and carries its
 * marker. `readBack` maps each sibling key to the issue read back for it (`null` when the read
 * found nothing).
 * @param {{sourceId: string, parentId: string, siblings: unknown,
 *          readBack: Record<string, any>}} input
 * @returns {{ok: boolean, problems: string[]}}
 */
export function verifySiblings({ sourceId, parentId, siblings, readBack }) {
  const problems = []
  const issues = readBack && typeof readBack === 'object' ? readBack : {}
  for (const { key } of requireSiblings(siblings)) {
    const issue = issues[key]
    if (issue === null || issue === undefined || typeof issue !== 'object') {
      problems.push(`${key}: missing`)
      continue
    }
    const ref = issueRef(issue) ?? key
    const parents = parentRefs(issue)
    if (!parents.includes(parentId)) {
      problems.push(`${key}: ${ref} parent is ${parents.join(' / ') || 'absent'}, want ${parentId}`)
    }
    if (descriptionAppearsTruncated(issue.description)) {
      problems.push(`${key}: ${ref} description is truncated; re-read it with get_issue`)
      continue
    }
    const marker = parseSiblingMarker(issue.description)
    if (!marker || marker.sourceId !== sourceId || marker.key !== key) {
      problems.push(`${key}: ${ref} lacks the marker ${siblingMarker(sourceId, key)}`)
    }
  }
  return { ok: problems.length === 0, problems }
}

// --- CLI ---------------------------------------------------------------------------------------

class UsageError extends Error {}

const USAGE =
  'usage: plan-sibling-split.mjs plan --input <json> --out-dir <dir> | edges --input <json> --created <json> | verify --input <json> --read-back <json>'

const FLAGS = {
  plan: ['input', 'out-dir'],
  edges: ['input', 'created'],
  verify: ['input', 'read-back'],
}

function parseFlags(command, rest) {
  const required = FLAGS[command]
  if (!required) throw new UsageError(USAGE)
  let values
  try {
    values = parseArgs({
      args: rest,
      strict: true,
      allowPositionals: false,
      options: Object.fromEntries(required.map((name) => [name, { type: 'string' }])),
    }).values
  } catch (error) {
    throw new UsageError(`plan-sibling-split: ${command}: ${error.message}`)
  }
  const missing = required.filter((name) => !isNonEmptyString(values[name]))
  if (missing.length > 0) {
    throw new UsageError(`plan-sibling-split: ${command} needs --${missing.join(', --')}`)
  }
  return values
}

const readJSON = (file) => JSON.parse(readFileSync(file, 'utf8'))

function writeBodies(creates, sourceId, outDir) {
  mkdirSync(outDir, { recursive: true })
  const family = /** @type {NonNullable<ReturnType<typeof planScratchFamily>>} */ (
    planScratchFamily('sibling-body')
  )
  return creates.map(({ key, title, estimate, body }) => {
    const bodyFile = join(outDir, family.basename({ issueId: sourceId, key }))
    writeFileSync(bodyFile, body)
    return { key, title, estimate, bodyFile }
  })
}

/**
 * @param {string[]} argv
 * @param {{write?: (s: string) => void, errWrite?: (s: string) => void}} [io]
 * @returns {number}
 */
export function main(
  argv,
  { write = (s) => process.stdout.write(s), errWrite = (s) => process.stderr.write(s) } = {},
) {
  const [command, ...rest] = argv
  let values
  try {
    values = parseFlags(command, rest)
  } catch (error) {
    errWrite(`${error.message}\n`)
    return EX_USAGE
  }
  try {
    const input = readJSON(values.input)
    if (command === 'plan') {
      const { creates, reused } = siblingSplitPlan(input)
      const written = writeBodies(creates, input.sourceId, values['out-dir'])
      write(`${JSON.stringify({ creates: written, reused })}\n`)
      return 0
    }
    if (command === 'edges') {
      write(`${JSON.stringify(siblingEdges({ ...input, idByKey: readJSON(values.created) }))}\n`)
      return 0
    }
    if (command === 'verify') {
      const verdict = verifySiblings({ ...input, readBack: readJSON(values['read-back']) })
      write(`${JSON.stringify(verdict)}\n`)
      for (const problem of verdict.problems) errWrite(`sibling-problem: ${problem}\n`)
      return verdict.ok ? 0 : 1
    }
    throw new UsageError(USAGE)
  } catch (error) {
    errWrite(`plan-sibling-split: ${command}: ${error?.message ?? error}\n`)
    return error instanceof UsageError ? EX_USAGE : 1
  }
}

if (isMainModule(import.meta.url)) process.exitCode = main(process.argv.slice(2))
