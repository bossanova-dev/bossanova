#!/usr/bin/env node

// Deterministic guards for boss-plan's run boundaries. These checks sit between
// untrusted drafting output and tracker writeback, so they return structured
// violations and keep the CLI shape small enough for skill bash blocks.

import {
  accessSync,
  chmodSync,
  constants,
  copyFileSync,
  existsSync,
  readFileSync,
  realpathSync,
  mkdtempSync,
  renameSync,
  rmSync,
  statSync,
  writeFileSync,
} from 'node:fs'
import { execFileSync } from 'node:child_process'
import { basename, dirname, isAbsolute, relative, resolve, sep } from 'node:path'

import { selectImplementationPlanAttachment } from './plan-attachment.mjs'
import {
  dependencyScanVerdict,
  extractKeyChangeAreas,
  planDependencyEdges,
  validateDependencyScanInput,
  withScanDefaults,
} from './plan-deps-lib.mjs'
import { parseEpicSpec } from './plan-epic-lib.mjs'
import { EPIC_REVERIFY_CLASS, epicReverifyVerdict } from './plan-epic-phase25.mjs'
import { RUN_SCRATCH_DIR_PREFIX, planScratchPath, planScratchToken } from './plan-scratch-paths.mjs'
import { createGateRecorder } from './gate-outcome.mjs'
import { isMainModule } from './main-module.mjs'
import {
  DEFAULT_CONFIG,
  canonicalContentLabel,
  labelName,
  loadSkillConfig,
  optionalLabelName,
  stateName,
  validatePlanDescription,
} from './skill-config.mjs'

export const PREMISE_LIMIT = 10

const plainObject = (value) => value !== null && typeof value === 'object' && !Array.isArray(value)

const ALLOWED_METADATA_KEYS = new Set([
  'planPath',
  'labels',
  'agentBuild',
  'estimate',
  'priority',
  'openQuestions',
  'descriptionSummary',
])

const VALID_ESTIMATES = new Set([0, 1, 2, 3, 5])

function entry(code, field, message, extra = {}) {
  return { code, field, message: `plan-run-guards: ${message}`, ...extra }
}

function planningSection(descriptionSummary) {
  const sections = String(descriptionSummary ?? '').split(/\n(?=##\s)/)
  return sections.find((section) => section.trimStart().startsWith('## Planning')) ?? ''
}

// A parented ticket whose honest size is 5 or more and which is NOT atomic cannot be decomposed
// under its existing epic parent, so it is planned as one ticket carrying `- Oversized-child:`
// (why it is not atomic, and the suggested sibling split) and handed to a human to split. That
// bullet is the estimate-5 justification for this shape; `- Atomic-5:` keeps meaning "atomic".
function hasOversizedChildBullet(descriptionSummary) {
  return /^[-*]\s*Oversized-child:\s*\S/m.test(planningSection(descriptionSummary))
}

function hasAtomic5Justification(descriptionSummary) {
  return (
    /^[-*]\s*Atomic-5:\s*\S/m.test(planningSection(descriptionSummary)) ||
    hasOversizedChildBullet(descriptionSummary)
  )
}

// `descriptionSummary` is either the description text inline, or `{ path }` naming this run's
// declared `description` scratch artifact (the dispatch return channel is not byte-preserving, so
// the drafter may hand back a reference instead). A reference is resolved through the caller's
// `resolveDescription`. An absolute or `./` path under the working tree is normalized to its
// repo-relative spelling; both the logical and the physical spelling of the tree are tried, so a
// symlinked prefix (macOS `/var` -> `/private/var`) still counts.
function repoRelativeScratchPath(candidate, cwd) {
  if (!isAbsolute(candidate) && !candidate.startsWith('./')) return candidate
  const bases = [resolve(cwd)]
  try {
    bases.push(realpathSync(cwd))
  } catch {
    // An unreadable cwd leaves only the logical base; the token check still refuses on no match.
  }
  for (const base of bases) {
    const rel = relative(base, resolve(base, candidate))
    if (rel !== '' && !rel.startsWith('..') && !isAbsolute(rel)) return rel.split(sep).join('/')
  }
  return candidate
}

function readDescriptionSummary(value, resolveDescription, cwd = process.cwd()) {
  if (typeof value === 'string') {
    return value.trim() === '' ? { kind: 'invalid' } : { kind: 'text', text: value }
  }
  // Arrays and `null` are `typeof 'object'` but are not attempts at the reference form; they are
  // the same plain `invalid` a number is, with no reference reason worth printing.
  if (!value || typeof value !== 'object' || Array.isArray(value)) return { kind: 'invalid' }

  const badReference = (reason) => ({ kind: 'bad-reference', reason })
  const keys = Object.keys(value)
  if (keys.length !== 1 || keys[0] !== 'path' || typeof value.path !== 'string') {
    return badReference(
      'a by-reference descriptionSummary must be exactly {"path": "<this run\'s description artifact>"}',
    )
  }
  const token = planScratchToken(repoRelativeScratchPath(value.path, cwd))
  if (!token.ok) return badReference(token.reason)
  const families = token.families ?? []
  if (token.kind !== 'artifact' || families.length !== 1 || families[0] !== 'description') {
    return badReference(
      `${value.path} is not a declared description artifact — it resolves to ` +
        `${families.length > 0 ? families.join(', ') : token.kind}, and only the \`description\` ` +
        'scratch family may carry a by-reference descriptionSummary',
    )
  }
  if (typeof resolveDescription !== 'function') return { kind: 'unresolved', path: value.path }
  let text
  try {
    text = resolveDescription(value.path)
  } catch (error) {
    return { kind: 'unreadable', path: value.path, reason: error?.message ?? String(error) }
  }
  if (typeof text !== 'string' || text.trim() === '') {
    return { kind: 'unreadable', path: value.path, reason: 'resolved to no description bytes' }
  }
  return { kind: 'text', text }
}

export function validateDraftMetadata(
  metadata,
  { config = DEFAULT_CONFIG, resolveDescription, cwd = process.cwd() } = {},
) {
  const missing = []
  const invalid = []
  const violations = []
  const warnings = []
  const warn = (field, message) => warnings.push(entry('normalized', field, message))

  const object =
    metadata && typeof metadata === 'object' && !Array.isArray(metadata) ? metadata : null
  if (!object) {
    violations.push(entry('metadata-not-object', 'metadata', 'metadata must be a JSON object'))
    return { ok: false, missing, invalid, violations, warnings, normalized: metadata }
  }

  // Only the plan path and the description are load-bearing: without them there is nothing to
  // write. Every other field is normalized to something usable and reported as a warning, because a
  // finished plan is worth far more than any one cosmetic field the drafter got slightly wrong.
  const normalized = {}
  for (const key of Object.keys(object)) {
    if (!ALLOWED_METADATA_KEYS.has(key)) warn(key, `dropped unknown top-level key "${key}"`)
  }

  if (typeof object.planPath === 'string' && object.planPath.trim() !== '') {
    normalized.planPath = object.planPath
  } else if (Object.hasOwn(object, 'planPath')) {
    invalid.push('planPath')
  } else {
    missing.push('planPath')
  }

  const rawLabels = Array.isArray(object.labels)
    ? object.labels
    : typeof object.labels === 'string'
      ? [object.labels]
      : []
  if (Object.hasOwn(object, 'labels') && !Array.isArray(object.labels)) {
    warn('labels', 'labels was not an array; coerced')
  }
  const labels = []
  for (const label of rawLabels) {
    const name = canonicalContentLabel(config, label)
    if (name === null) {
      warn('labels', `dropped label ${JSON.stringify(label)}: not a configured content label`)
    } else if (!labels.includes(name)) {
      labels.push(name)
    }
  }
  normalized.labels = labels

  if (typeof object.agentBuild === 'boolean') {
    normalized.agentBuild = object.agentBuild
  } else if (object.agentBuild === 'true' || object.agentBuild === 'false') {
    normalized.agentBuild = object.agentBuild === 'true'
  } else {
    normalized.agentBuild = true
    if (Object.hasOwn(object, 'agentBuild')) {
      warn('agentBuild', 'agentBuild was not a boolean; defaulted to true')
    }
  }

  const summary = Object.hasOwn(object, 'descriptionSummary')
    ? readDescriptionSummary(object.descriptionSummary, resolveDescription, cwd)
    : null
  const descriptionText = summary?.kind === 'text' ? summary.text : null

  const estimate = Number(object.estimate)
  if (Object.hasOwn(object, 'estimate') && Number.isFinite(estimate)) {
    const valid = [...VALID_ESTIMATES].sort((a, b) => a - b)
    const snapped = valid.find((value) => value >= estimate) ?? valid.at(-1)
    if (snapped !== object.estimate) {
      warn('estimate', `estimate ${JSON.stringify(object.estimate)} normalized to ${snapped}`)
    }
    normalized.estimate = snapped
    if (snapped === 5 && descriptionText !== null && !hasAtomic5Justification(descriptionText)) {
      warn(
        'estimate',
        'estimate 5 without an "- Atomic-5:" or "- Oversized-child:" justification under ## Planning',
      )
    }
  } else {
    if (Object.hasOwn(object, 'estimate')) warn('estimate', 'estimate was not a number; omitted')
  }

  const priority = Math.round(Number(object.priority))
  if (Object.hasOwn(object, 'priority') && Number.isFinite(priority)) {
    const clamped = priority < 1 ? 3 : Math.min(priority, 4)
    if (clamped !== object.priority) {
      warn('priority', `priority ${JSON.stringify(object.priority)} normalized to ${clamped}`)
    }
    normalized.priority = clamped
  } else {
    normalized.priority = 3
    if (Object.hasOwn(object, 'priority')) warn('priority', 'priority was not a number; set to 3')
  }

  const rawQuestions = Array.isArray(object.openQuestions)
    ? object.openQuestions
    : typeof object.openQuestions === 'string'
      ? [object.openQuestions]
      : []
  normalized.openQuestions = rawQuestions.filter(
    (question) => typeof question === 'string' && question.trim() !== '',
  )

  if (!summary) {
    missing.push('descriptionSummary')
  } else if (summary.kind === 'invalid') {
    invalid.push('descriptionSummary')
  } else if (summary.kind === 'bad-reference') {
    invalid.push('descriptionSummary')
    violations.push(
      entry('description-summary-bad-reference', 'descriptionSummary', summary.reason),
    )
  } else if (summary.kind === 'unresolved') {
    violations.push(
      entry(
        'description-summary-unresolved',
        'descriptionSummary',
        `descriptionSummary names ${summary.path} but this caller supplied no resolveDescription`,
      ),
    )
  } else if (summary.kind === 'unreadable') {
    violations.push(
      entry(
        'description-summary-unreadable',
        'descriptionSummary',
        `descriptionSummary names ${summary.path}, which could not be read: ${summary.reason}`,
      ),
    )
  }
  // An oversized child is a split request for a human, so it can never be build-ready (`agentBuild`). The
  // helper enforces that even when the drafter's prose forgot to.
  if (
    descriptionText !== null &&
    normalized.agentBuild === true &&
    hasOversizedChildBullet(descriptionText)
  ) {
    normalized.agentBuild = false
    warn(
      'agentBuild',
      'agentBuild true with an "- Oversized-child:" bullet under ## Planning; coerced to false (a human splits it into siblings)',
    )
  }

  // The description's content is checked once, by the Phase 4 plan-contract gate over the bytes
  // actually written; checking it here too only discarded plans that gate would have kept.
  if (Object.hasOwn(object, 'descriptionSummary')) {
    normalized.descriptionSummary = object.descriptionSummary
  }

  return {
    ok: missing.length === 0 && invalid.length === 0 && violations.length === 0,
    missing,
    invalid: [...new Set(invalid)],
    violations,
    warnings,
    normalized,
  }
}

/**
 * Is the fetched payload the issue the run actually selected?
 *
 * The precheck's other three conjuncts all interrogate the payload it was handed; none of them asks
 * whether that payload is the right ticket. A mispicked id therefore plans a *different* ticket
 * while every guard reports a clean run. `selectedID` closes that: it is compared against both the
 * UUID (`issue.id`) and the human identifier (`issue.identifier`), because callers legitimately
 * hold either one.
 *
 * Case-insensitive by design. A UUID is lower-case hex and an identifier is upper-case (`ABC-123`),
 * so folding case can never make two genuinely different ids compare equal — it only removes a
 * spurious mismatch from a caller that normalised the selector differently.
 *
 * Absent (or blank) `selectedID` returns false and the conjunct never fires, so every pre-existing
 * caller keeps a byte-identical result.
 */
function fetchedIssueIdentityMismatch(issue, selectedID) {
  const selected = typeof selectedID === 'string' ? selectedID.trim().toLowerCase() : ''
  if (!selected) return false
  const candidates = [issue?.id, issue?.identifier]
    .filter((value) => typeof value === 'string' && value.trim() !== '')
    .map((value) => value.trim().toLowerCase())
  // No usable id on the payload at all is not a *match* either: a fetch that returned nothing
  // identifiable is exactly the case this conjunct exists to refuse to call clean.
  return !candidates.includes(selected)
}

export function planIdempotencePrecheck({
  issue,
  selectedID = null,
  config = DEFAULT_CONFIG,
} = {}) {
  const reasons = []
  const attachments = Array.isArray(issue?.attachments) ? issue.attachments : []

  // Pushed FIRST because it subsumes the rest: if this is the wrong ticket, every verdict the other
  // three conjuncts reach is about a ticket nobody asked to plan. It can only ever add a reason, so
  // it can only ever push the run toward `plan` — never toward `noop`.
  if (fetchedIssueIdentityMismatch(issue, selectedID)) reasons.push('fetched-issue-id-mismatch')

  let planned = null
  try {
    planned = stateName(config, 'planned')
  } catch {
    planned = null
  }
  const currentState =
    issue?.status ??
    issue?.stateName ??
    (typeof issue?.state === 'string' ? issue.state : issue?.state?.name)
  if (!planned || currentState !== planned) reasons.push('state-not-planned')

  let descriptionValid = false
  try {
    descriptionValid = validatePlanDescription(config, issue?.description ?? '').ok
  } catch {
    descriptionValid = false
  }
  if (!descriptionValid) reasons.push('description-invalid')

  const issueID = issue?.id || issue?.identifier
  if (!issueID || !selectImplementationPlanAttachment(attachments, issueID)) {
    reasons.push('plan-attachment-missing')
  }

  return {
    action: reasons.length === 0 ? 'noop' : 'plan',
    reasons,
  }
}

export function premiseDrift(premises, liveStates) {
  const list = Array.isArray(premises) ? premises : []
  const states =
    liveStates && typeof liveStates === 'object' && !Array.isArray(liveStates) ? liveStates : {}
  const drifted = []
  const unresolved = []
  let verified = 0

  for (const premise of list) {
    const id = typeof premise?.id === 'string' ? premise.id : ''
    if (!id) continue
    if (!Object.hasOwn(states, id)) {
      unresolved.push(id)
      continue
    }
    verified += 1
    const plannedState = premise.state
    const currentState = states[id]
    if (plannedState !== currentState) drifted.push({ id, plannedState, currentState })
  }

  return {
    ok: drifted.length === 0 && unresolved.length === 0,
    drifted,
    unresolved,
    verified,
    declared: list.length,
  }
}

// Keep line terminators, whitespace and the reporter-owned tail intact. Reconciliation
// starts from unannotated bytes each time, so both a changed state and resolved drift
// replace the previous verdict without any state carried between passes.
function reconcilePremises(text, { premises, drifted }) {
  const ids = [...new Set((premises ?? []).map((p) => p?.id).filter(Boolean))]
  const escape = (value) => value.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')
  const token = (id) => new RegExp(`(?<![\\w-])${escape(id)}(?![\\w-])`)
  const mentions = new Map(ids.map((id) => [id, token(id)]))
  const annotations = (drifted ?? [])
    .filter((item) => mentions.has(item.id))
    .map((item) => ({
      ...item,
      inline: new Set(),
      checkbox: new Set(),
    }))
  const driftLine = ids.length
    ? new RegExp(`^[-*] Premise drift: (${ids.map(escape).join('|')})(?![\\w-])`)
    : null
  const marker = ids.length
    ? new RegExp(` \\(premise drift: (?:${ids.map(escape).join('|')})(?![\\w-])`, 'g')
    : null
  const stripMarkers = (line) => {
    if (!marker) return line
    marker.lastIndex = 0
    let output = '',
      cursor = 0,
      match
    while ((match = marker.exec(line))) {
      let depth = 0
      for (let i = match.index + 1; i < line.length; i += 1) {
        if (line[i] === '\\') {
          i += 1
          continue
        }
        if (line[i] === '(') depth += 1
        else if (line[i] === ')') {
          depth -= 1
          if (depth === 0) {
            output += line.slice(cursor, match.index)
            cursor = i + 1
            marker.lastIndex = cursor
            break
          }
        }
      }
    }
    return output + line.slice(cursor)
  }
  const escapeState = (state) => String(state).replace(/[\\()]/g, '\\$&')
  const lines = text.match(/[^\r\n]*(?:\r\n|\n|$)/g).filter((line) => line !== '')
  const tableRows = new Set()
  for (let i = 1; i < lines.length; i += 1) {
    if (!/^\s*\|?\s*:?-{3,}:?\s*(?:\|\s*:?-{3,}:?\s*)+\|?\s*$/.test(lines[i])) continue
    if (lines[i - 1].includes('|')) tableRows.add(i - 1)
    tableRows.add(i)
    for (let j = i + 1; j < lines.length && lines[j].includes('|'); j += 1) tableRows.add(j)
  }
  const newline = text.includes('\r\n') ? '\r\n' : '\n'
  const output = []
  let section = '',
    fence = null,
    original = false,
    originalIndex = null,
    planningEnd = null,
    lastBullet = null
  for (const [index, raw] of lines.entries()) {
    let line = raw.replace(/\r?\n$/, '')
    const eol = raw.slice(line.length)
    if (original) {
      output.push(raw)
      continue
    }
    if (fence) {
      output.push(raw)
      if (new RegExp(`^ {0,3}${fence.char}{${fence.length},}\\s*$`).test(line)) fence = null
      continue
    }
    const openFence = /^ {0,3}(`{3,}|~{3,})/.exec(line)
    if (openFence) {
      fence = { char: openFence[1][0], length: openFence[1].length }
      output.push(raw)
      continue
    }
    const heading = /^##\s+(.+?)\s*#*\s*$/.exec(line)
    if (heading) {
      if (section === 'Planning') planningEnd = output.length
      section = heading[1]
      if (section === 'Planning') {
        lastBullet = null
        planningEnd = output.length + 1
      }
      if (section === 'Original notes') {
        original = true
        originalIndex = output.length
      }
      output.push(raw)
      continue
    }
    if (driftLine?.test(line)) continue
    // Balanced legacy markers and escaped new markers end at their own closing
    // parenthesis, preserving any prose that follows a relocated annotation.
    line = stripMarkers(line)
    if (section === 'Planning' && /^\s*[-*+]\s+/.test(line)) lastBullet = output.length
    if (!tableRows.has(index) && !/^\s*(?:#|\|)/.test(line)) {
      const matched = annotations.filter((item) => mentions.get(item.id).test(line))
      for (const item of matched) {
        if (section === 'Premises' || section === 'Acceptance criteria') item.checkbox.add(section)
        else if (section !== 'Planning') item.inline.add(section || 'preamble')
      }
      // Decide all mentions before appending anything: generated markers are never mentions.
      for (const item of matched) {
        if (section !== 'Planning' && section !== 'Premises' && section !== 'Acceptance criteria') {
          line += ` (premise drift: ${item.id} is now ${escapeState(item.currentState)})`
        }
      }
    }
    output.push(line + eol)
    if (section === 'Planning') planningEnd = output.length
  }
  if (annotations.length) {
    const reports = annotations.map((item) => {
      let report = `- Premise drift: ${item.id} was ${item.plannedState}, is now ${item.currentState}`
      if (item.inline.size) report += `; flagged inline in ${[...item.inline].join(', ')}`
      if (item.checkbox.size) report += `; also stated in ${[...item.checkbox].join(', ')}`
      return report + newline
    })
    if (planningEnd === null) {
      const index = originalIndex === null ? output.length : originalIndex
      if (index > 0 && !output[index - 1].endsWith('\n')) output[index - 1] += newline
      output.splice(index, 0, `## Planning${newline}${newline}`, ...reports)
    } else {
      const index = lastBullet === null ? planningEnd : lastBullet + 1
      if (index > 0 && !output[index - 1].endsWith('\n')) output[index - 1] += newline
      output.splice(index, 0, ...reports)
    }
  }
  return { text: output.join(''), annotations }
}

function writePremiseUpdates(changed) {
  // Check permissions before staging: rename itself can replace a read-only file.
  for (const update of changed) accessSync(update.file, constants.W_OK)
  const staged = []
  const committed = []
  try {
    for (const update of changed) {
      const file = realpathSync(update.file)
      const mode = statSync(file).mode & 0o7777
      const dir = mkdtempSync(resolve(dirname(file), '.premise-annotations-'))
      const item = { file, dir, next: resolve(dir, 'next'), backup: resolve(dir, 'original') }
      staged.push(item)
      copyFileSync(file, item.backup)
      chmodSync(item.backup, mode)
      writeFileSync(item.next, update.text)
      chmodSync(item.next, mode)
    }
    for (const item of staged) {
      renameSync(item.next, item.file)
      committed.push(item)
    }
  } catch (error) {
    // All original bytes are retained until every replacement has succeeded.
    for (const item of committed.reverse()) {
      try {
        renameSync(item.backup, item.file)
      } catch (rollbackError) {
        // Keep the backup if restoration itself fails so recovery remains possible.
        item.keepBackup = true
        error = new Error(
          `${error.message}; rollback failed for ${item.file}: ${rollbackError.message}; original retained at ${item.backup}`,
        )
      }
    }
    throw error
  } finally {
    for (const item of staged) {
      if (!item.keepBackup) rmSync(item.dir, { recursive: true, force: true })
    }
  }
}

export function reconcilePremiseAnnotations(text, options) {
  return reconcilePremises(text, options).text
}

function readJSON(file) {
  return JSON.parse(readFileSync(file, 'utf8'))
}

/**
 * The repo's root-level entries from `git ls-tree -z HEAD`: `dirs` (trees and submodules) are module
 * roots, `files` (blobs) are root files. Split because a root FILE needs no extension to be an area
 * and an unmarked mention of one is reported rather than dropped (`extractKeyChangeAreas`
 * `rootFiles`), while a directory keeps the module-root rules.
 */
export function listRepoRootEntries(cwd = process.cwd()) {
  const out = execFileSync('git', ['ls-tree', '-z', 'HEAD'], { cwd, encoding: 'utf8' })
  const dirs = []
  const files = []
  for (const record of out.split('\0')) {
    const tab = record.indexOf('\t')
    if (tab === -1) continue
    const type = record.slice(0, tab).split(' ')[1]
    const name = record.slice(tab + 1)
    if (name === '') continue
    if (type === 'blob') files.push(name)
    else dirs.push(name)
  }
  return { dirs, files }
}

const FETCH_CANDIDATES_REMEDY =
  'write the candidates file with `tracker/cli.mjs fetch-candidates --out-file <file> --id <ISSUE-ID>`'

function candidateRowDefect(row) {
  if (!plainObject(row)) return 'a row is not an object'
  if (!['state', 'id'].includes(row.source)) return 'a row carries no `source` of state or id'
  if (typeof row.stateType !== 'string' || row.stateType.trim() === '') {
    return 'a row carries no stateType'
  }
  if (!Object.hasOwn(row, 'parentId')) return 'a row carries no own parentId'
  return null
}

/**
 * The `deps` boundary: classify one subject against a `fetch-candidates` file. Both issue sides come
 * from that file — the subject is resolved from it by id or identifier — so nothing in the scan
 * input is retyped. The set is complete (`candidateSetComplete`) unless a row carries
 * `stateScope: 'override'` — fetch-candidates marks every row so when `--state` narrowed the scan.
 *
 * @param {{payload: unknown, rows: unknown, subjectId: string, config: object,
 *   rootEntries: {dirs: string[], files: string[]}}} input
 * @returns {{status: 'refused', refusals: {code: string, message: string}[]} |
 *   {status: 'defects', defects: {code: string, id: string, remedy: string}[]} |
 *   {status: 'ok', result: object, verdict: object, subject: object}}
 *   `subject` is the subject's `extractKeyChangeAreas` result.
 */
export function runDependencyScan({ payload, rows, subjectId, config, rootEntries }) {
  const refusals = []
  const refuse = (code, message) => refusals.push({ code, message })
  if (!plainObject(payload)) {
    refuse('payload-not-object', 'the scan input must be a JSON object')
  } else {
    for (const key of ['candidates', 'subject']) {
      if (Object.hasOwn(payload, key)) {
        refuse(
          `payload-carries-${key}`,
          `the scan input carries \`${key}\`; both issue sides come from the candidates file — remove it`,
        )
      }
    }
  }
  if (!Array.isArray(rows)) {
    refuse(
      'candidates-not-fetch-candidates',
      `the candidates file is not an array; ${FETCH_CANDIDATES_REMEDY}`,
    )
  } else {
    const defect = rows.map(candidateRowDefect).find(Boolean)
    if (defect) {
      refuse(
        'candidates-not-fetch-candidates',
        `the candidates file is not fetch-candidates output (${defect}); ${FETCH_CANDIDATES_REMEDY}`,
      )
    }
  }
  if (refusals.length > 0) return { status: 'refused', refusals }

  const wanted = String(subjectId ?? '')
    .trim()
    .toLowerCase()
  const subject = rows.find((row) =>
    [row.id, row.identifier].some(
      (value) => typeof value === 'string' && value.trim().toLowerCase() === wanted,
    ),
  )
  if (!subject) {
    return {
      status: 'refused',
      refusals: [
        {
          code: 'subject-not-in-candidates',
          message: `${subjectId} is not in the candidates file; re-run fetch-candidates with --id ${subjectId}`,
        },
      ],
    }
  }

  const input = withScanDefaults(config, { ...payload, subject, candidates: rows })
  input.moduleRoots = [...new Set([...input.moduleRoots, ...rootEntries.dirs])]
  const validated = validateDependencyScanInput(input)
  if (!validated.ok) return { status: 'defects', defects: validated.defects }

  const areasOf = (issue) =>
    extractKeyChangeAreas(config, issue.description, {
      moduleRoots: input.moduleRoots,
      rootFiles: rootEntries.files,
    })
  const subjectScan = areasOf(subject)
  input.subjectAreas = subjectScan.areas
  input.subjectUnresolvedAreas = subjectScan.unresolved
  input.candidates = rows.map((row) => ({ ...row, areas: areasOf(row).areas }))
  // Complete only when the fetch scanned the default states: a `--state` override is marked on
  // every row, and a narrowed set must reach could-not-evaluate rather than a clean verdict.
  input.candidateSetComplete = !rows.some((row) => row.stateScope === 'override')
  const result = planDependencyEdges(input)
  return { status: 'ok', result, verdict: dependencyScanVerdict(result), subject: subjectScan }
}

function parseSubjectFlag(argv) {
  const index = argv.indexOf('--subject')
  if (index === -1) return null
  const value = String(argv[index + 1] ?? '').trim()
  return value === '' || value.startsWith('--') ? null : value
}

/** Key-sorted JSON, so two objects compare on content rather than on write order. */
function canonical(value) {
  if (Array.isArray(value)) return `[${value.map(canonical).join(',')}]`
  if (value === null || typeof value !== 'object') return JSON.stringify(value) ?? 'null'
  const keys = Object.keys(value).sort()
  return `{${keys.map((k) => `${JSON.stringify(k)}:${canonical(value[k])}`).join(',')}}`
}

/**
 * Decide which metadata to validate: always the object the dispatch RETURNED, never a same-named
 * file the worker may have written. A diverged file is replaced and reported as a warning.
 *
 * @param {{returned: unknown, onDisk?: unknown}} input
 * @returns {{ok: boolean, adopted: unknown, state: 'absent'|'identical'|'diverged', violations: object[], warnings?: object[]}}
 */
export function adoptReturnedMetadata({ returned, onDisk } = {}) {
  if (returned === undefined || returned === null || typeof returned !== 'object') {
    return {
      ok: false,
      adopted: returned,
      state: 'absent',
      violations: [
        entry(
          'metadata-not-returned',
          'returned',
          'the dispatch returned no metadata object — artifact readiness is not message ' +
            'readiness, so there is nothing here to validate',
        ),
      ],
    }
  }
  if (onDisk === undefined) return { ok: true, adopted: returned, state: 'absent', violations: [] }
  if (canonical(onDisk) === canonical(returned)) {
    return { ok: true, adopted: returned, state: 'identical', violations: [] }
  }
  // The worker wrote the declared basename itself. The returned message is still what gets
  // validated and written; the divergence is reported, never a reason to discard the plan.
  return {
    ok: true,
    adopted: returned,
    state: 'diverged',
    violations: [],
    warnings: [
      entry(
        'metadata-file-overwritten',
        'draft-metadata',
        'the draft-metadata file held bytes that were not the returned object; replaced with ' +
          'the returned object',
      ),
    ],
  }
}

// The `idempotence` verb reads its file as the BARE issue object. A `{issue:{…}}` wrapper — the
// natural mistake, because the function it feeds takes `{issue}` — leaves every field it reads
// undefined, so all three reasons fire at once and the verb prints `action:"plan"` with a full reason
// list. That output is byte-identical to the verdict a genuinely unplanned ticket produces, so the
// guard reads as working while having evaluated nothing at all.
//
// Scoped to an object whose ONLY own key is `issue`: a real issue payload that happens to carry an
// `issue` field alongside its own is not this mistake and is passed through untouched.
function assertBareIssuePayload(value, file) {
  if (value === null || typeof value !== 'object' || Array.isArray(value)) return
  const keys = Object.keys(value)
  if (keys.length === 1 && keys[0] === 'issue') {
    throw new Error(
      // No module prefix here: this throw is caught by the `unreadable-input` handler below, which
      // supplies `plan-run-guards: ` itself. Prefixing again printed
      // `unreadable-input: plan-run-guards: plan-run-guards: …` — a stutter no sibling diagnostic in
      // this file has.
      `idempotence <issue.json> (the BARE issue object) — ${file} contains a {issue:{…}} wrapper; every field would read as undefined and the verdict would be action:"plan" with every reason fired, which is exactly what an unplanned ticket looks like. Pass the issue object itself.`,
    )
  }
}

// A role the config cannot resolve is handed to the verdict as null, which reports it as
// `unresolved-role` for the roles it requires — a named blocker instead of an opaque throw.
function resolveRole(resolve, config, role) {
  try {
    return resolve(config, role)
  } catch {
    return null
  }
}

const readIfPresent = (file) => (existsSync(file) ? readFileSync(file, 'utf8') : null)
const childKey = (issue) =>
  typeof issue?.identifier === 'string' && issue.identifier !== '' ? issue.identifier : issue?.id

/**
 * The `epic-reverify` boundary. The bundle carries tracker payloads only — ids, states, labels,
 * attachments, links. Every other input is a file whose path is DERIVED here, from the bundle's own
 * run directory and ids through `planScratchPath`, so the thing being checked can never choose what
 * it is compared against; and every description the verdict judges is replaced by the stored
 * read-back file, so a retyped or abbreviated description in the bundle is never read.
 */
function runEpicReverify(bundlePath, { config = loadSkillConfig() } = {}) {
  const bundle = JSON.parse(readFileSync(bundlePath, 'utf8'))
  if (!plainObject(bundle)) throw new Error(`${bundlePath} is not a JSON object`)
  const { parentId, childIds } = bundle
  if (typeof parentId !== 'string' || parentId.trim() === '') {
    throw new Error(`${bundlePath} carries no parentId`)
  }
  const runDir = dirname(bundlePath)
  const runDirName = basename(runDir)
  if (!runDirName.startsWith(RUN_SCRATCH_DIR_PREFIX)) {
    throw new Error(`${bundlePath} does not sit in a ${RUN_SCRATCH_DIR_PREFIX}<RUN-ID> directory`)
  }
  const runId = runDirName.slice(RUN_SCRATCH_DIR_PREFIX.length)
  const root = dirname(runDir)
  const at = (family, parts) =>
    planScratchPath(runId, family, { issueId: parentId, ...parts }, root)
  if (basename(bundlePath) !== basename(at('epic-reverify'))) {
    throw new Error(`${bundlePath} is not this parent's declared ${basename(at('epic-reverify'))}`)
  }

  const fileBlockers = []
  const need = (path, code, what) => {
    const text = readIfPresent(path)
    if (text === null) fileBlockers.push({ code, message: `${what} is missing: ${path}` })
    return text
  }

  const specText = need(at('epic-spec'), 'intended-missing', 'the epic spec scratch')
  const intendedOverview = need(at('epic-overview'), 'intended-missing', 'the intended overview')
  const storedOverview = need(at('image-guard-stored'), 'stored-missing', 'the stored overview')

  // Copies with the bundle's own descriptions dropped: only stored read-back files are judged.
  const parent = plainObject(bundle.parent) ? { ...bundle.parent } : bundle.parent
  if (plainObject(parent)) {
    delete parent.description
    if (storedOverview !== null) parent.description = storedOverview
  }

  const children = Array.isArray(bundle.children)
    ? bundle.children.map((child) => {
        if (!plainObject(child)) return child
        const fields = { ...child }
        delete fields.description
        const key = childKey(child)
        const stored =
          typeof key === 'string' && key !== ''
            ? need(
                at('child-image-guard-stored', { childId: key }),
                'stored-missing',
                `${key}'s stored description`,
              )
            : null
        return stored === null ? fields : { ...fields, description: stored }
      })
    : bundle.children

  const childBodies = {}
  for (const childId of Array.isArray(childIds) ? childIds : []) {
    if (typeof childId !== 'string' || childId === '') continue
    const intended = need(
      at('child-image-guard-new', { childId }),
      'intended-missing',
      `${childId}'s intended description`,
    )
    const live = Array.isArray(children)
      ? children.find((c) => plainObject(c) && (c.identifier === childId || c.id === childId))
      : undefined
    childBodies[childId] = { intended: intended ?? undefined, stored: live?.description }
  }

  const verdict = epicReverifyVerdict({
    parentId,
    childIds,
    parent,
    children,
    spec: specText === null ? null : parseEpicSpec(specText),
    roles: {
      planned: resolveRole(stateName, config, 'planned'),
      unplanned: resolveRole(stateName, config, 'unplanned'),
      inProgress: resolveRole(stateName, config, 'inProgress'),
      inReview: resolveRole(stateName, config, 'inReview'),
      epic: resolveRole(labelName, config, 'epic'),
      agentBuild: resolveRole(labelName, config, 'agentBuild'),
      needsHuman: resolveRole(labelName, config, 'needsHuman'),
      agentQuestion: resolveRole(optionalLabelName, config, 'agentQuestion'),
      agentPlan: resolveRole(optionalLabelName, config, 'agentPlan'),
    },
    config,
    parentOverview: {
      intended: intendedOverview ?? undefined,
      stored: storedOverview ?? undefined,
    },
    childBodies,
  })
  if (fileBlockers.length === 0) return verdict
  // A missing file always surfaces in the verdict too (a missing body or spec is itself a
  // blocker), so the class is already decided; the fallback only guards that invariant.
  const decidedClass = verdict.ok ? EPIC_REVERIFY_CLASS.needsHuman : verdict.class
  return {
    ...verdict,
    ok: false,
    class: decidedClass,
    blockers: [...fileBlockers, ...verdict.blockers],
  }
}

// An unreadable input cannot prove the parent is still resumable, so it takes the retain class.
function unreadableEpicReverify(error) {
  return {
    ok: false,
    class: EPIC_REVERIFY_CLASS.needsHuman,
    notices: [],
    blockers: [
      { code: 'unreadable-input', message: `plan-run-guards: ${error?.message ?? error}` },
    ],
  }
}

function printViolations(result) {
  for (const field of result.missing ?? []) {
    process.stderr.write(`plan-run-guards: missing ${field}\n`)
  }
  for (const field of result.invalid ?? []) {
    process.stderr.write(`plan-run-guards: invalid ${field}\n`)
  }
  for (const violation of result.violations ?? []) {
    process.stderr.write(`${violation.code}: ${violation.message}\n`)
  }
  for (const warning of result.warnings ?? []) {
    process.stderr.write(`warning: ${warning.message}\n`)
  }
}

/** Run one verb and report the exit code alongside the gate id and reason to record for it. */
// The dependency scan's repo module roots, as a comma-separated `--module-roots` value. Optional
// and positional-agnostic: every verb here takes positional inputs, so the flag is read out of the
// whole argv rather than from a fixed slot. Absent, the contract check keeps its old empty list.
// `--selected-id <id>` is OPTIONAL by contract: omitting it leaves the idempotence verdict exactly
// as it was before the flag existed. It is not defaulted to anything — a guessed selector would
// manufacture the very mismatch the conjunct exists to detect.
function parseSelectedIDFlag(argv) {
  const index = argv.indexOf('--selected-id')
  if (index === -1) return null
  const value = String(argv[index + 1] ?? '').trim()
  return value === '' ? null : value
}

function parseModuleRootsFlag(argv) {
  const index = argv.indexOf('--module-roots')
  if (index === -1) return []
  return String(argv[index + 1] ?? '')
    .split(',')
    .map((root) => root.trim())
    .filter((root) => root !== '')
}

function runGuardVerb(argv) {
  const [command, first, second] = argv
  // The verbs are gates in their own right: each is invoked at a different point in a planning run
  // and refuses for different reasons, so the retirement decision this record feeds needs their
  // firing rates apart rather than summed — which is why the recorded gate id carries the verb
  // (`plan-run-guards.premises`). Every dispatch branch below claims its verb as its FIRST
  // statement, so the verb is already correct if that branch later throws; `usage` is the standing
  // answer for an argv that matched no branch at all. Deriving it from the branch rather than from
  // a second list of verb names is what stops a verb added to only one of the two from recording
  // its outcomes under the wrong gate id.
  let verb = 'usage'
  try {
    if (command === 'metadata' && first) {
      verb = 'metadata'
      // This verb IS the orchestrator's boundary, so it is where the by-reference
      // `descriptionSummary` gets hydrated: the guard itself stays pure and refuses an
      // unhydrated reference, and the disk read lives here where the run's scratch is.
      const result = validateDraftMetadata(readJSON(first), {
        config: loadSkillConfig(),
        resolveDescription: (file) => readFileSync(file, 'utf8'),
      })
      printViolations(result)
      if (result.ok) writeFileSync(first, `${JSON.stringify(result.normalized)}\n`)
      return {
        verb,
        code: result.ok ? 0 : 1,
        reason: result.ok ? 'ok' : 'invalid-metadata',
      }
    }
    if (command === 'adopt-metadata' && first && second !== undefined) {
      verb = 'adopt-metadata'
      // `second` is the RETURNED object, handed over as argv text because that is the only form
      // the orchestrator has it in — it arrived as a message, not as a file. Writing it to disk
      // first and then pointing this verb at the file would reintroduce the very gap being
      // closed: the thing validated has to be the thing received.
      const decided = adoptReturnedMetadata({
        returned: JSON.parse(second),
        onDisk: existsSync(first) ? readJSON(first) : undefined,
      })
      printViolations(decided)
      if (!decided.ok) {
        return { verb, code: 1, reason: decided.violations[0]?.code ?? 'not-adopted' }
      }
      const result = validateDraftMetadata(decided.adopted, {
        config: loadSkillConfig(),
        resolveDescription: (file) => readFileSync(file, 'utf8'),
      })
      printViolations(result)
      writeFileSync(first, `${JSON.stringify(result.ok ? result.normalized : decided.adopted)}\n`)
      return { verb, code: result.ok ? 0 : 1, reason: result.ok ? 'ok' : 'invalid-metadata' }
    }
    if (command === 'idempotence' && first) {
      verb = 'idempotence'
      const payload = readJSON(first)
      // Raised through the existing `unreadable-input` catch below, so the CLI exits non-zero with a
      // named reason rather than printing a plausible verdict it never computed.
      assertBareIssuePayload(payload, first)
      const result = planIdempotencePrecheck({
        issue: payload,
        selectedID: parseSelectedIDFlag(argv),
        config: loadSkillConfig(),
      })
      process.stdout.write(`${JSON.stringify(result)}\n`)
      // This verb never refuses — it reports whether planning is still needed. Both answers are a
      // `pass`; the reason carries which one, so the record stays informative without inventing a
      // fire that the caller never saw.
      return { verb, code: 0, reason: result.action === 'noop' ? 'noop' : 'plan' }
    }
    if (command === 'deps' && first && second && parseSubjectFlag(argv)) {
      verb = 'deps'
      const subjectId = parseSubjectFlag(argv)
      const scan = runDependencyScan({
        payload: readJSON(first),
        rows: readJSON(second),
        subjectId,
        config: loadSkillConfig({ cwd: process.cwd() }),
        rootEntries: listRepoRootEntries(),
      })
      if (scan.status === 'refused') {
        for (const r of scan.refusals)
          process.stderr.write(`${r.code}: plan-run-guards: ${r.message}\n`)
        return { verb, code: 1, reason: scan.refusals[0].code }
      }
      if (scan.status === 'defects') {
        for (const d of scan.defects) process.stderr.write(`${d.code} ${d.id} ${d.remedy}\n`)
        return { verb, code: 1, reason: 'scan-input-defect' }
      }
      const { result, verdict, subject } = scan
      process.stderr.write(
        `subjectAreas ${JSON.stringify(subject.areas)} unresolved ${JSON.stringify(subject.unresolved)}` +
          ` referenced ${JSON.stringify(subject.referenced)} candidatesWithoutAreas ` +
          `${result.candidatesWithoutAreas} ${verdict.verdict} compared=${verdict.compared}` +
          ` edges=${verdict.edges} ${verdict.reasons}\n`,
      )
      process.stdout.write(`${JSON.stringify({ ...result, verdict })}\n`)
      return { verb, code: 0, reason: verdict.verdict }
    }
    if (command === 'epic-reverify' && first) {
      verb = 'epic-reverify'
      // This branch owns its read/parse/config errors: the shared catch below exits 1, and exit 1 is
      // the ONE code whose skill branch deletes the run scratch. An unreadable input proves nothing
      // about the parent, so it must retain that evidence (exit 3), never take the resumable exit.
      let decided
      try {
        decided = runEpicReverify(first)
      } catch (error) {
        decided = unreadableEpicReverify(error)
      }
      for (const b of decided.blockers) process.stderr.write(`${b.code}: ${b.message}\n`)
      process.stdout.write(`${JSON.stringify(decided)}\n`)
      if (decided.ok) return { verb, code: 0, reason: 'ok' }
      return {
        verb,
        code: decided.class === EPIC_REVERIFY_CLASS.resumable ? 1 : 3,
        reason: decided.blockers[0]?.code ?? 'blocked',
      }
    }
    if (command === 'premises' && first && second) {
      verb = 'premises'
      const premises = readJSON(first)
      const liveStates = readJSON(second)
      const result = premiseDrift(premises, liveStates)
      const overLimit = Array.isArray(premises) && premises.length > PREMISE_LIMIT
      process.stderr.write(`premises: verified ${result.verified} of ${result.declared}\n`)
      if (overLimit) {
        process.stderr.write(
          `premise-limit: plan-run-guards: premises length ${premises.length} exceeds ${PREMISE_LIMIT}\n`,
        )
      }
      for (const item of result.drifted) {
        process.stderr.write(
          `premise-drift: plan-run-guards: ${item.id} was ${item.plannedState}, is now ${item.currentState}\n`,
        )
      }
      for (const id of result.unresolved) {
        process.stderr.write(`premise-unresolved: plan-run-guards: ${id} could not be read\n`)
      }
      // Reasons reuse the stderr code vocabulary above rather than inventing a second set.
      let reason = 'ok'
      if (overLimit) reason = 'premise-limit'
      else if (result.unresolved.length > 0) reason = 'premise-unresolved'
      else if (result.drifted.length > 0) reason = 'premise-drift'
      if (!overLimit && result.unresolved.length === 0) {
        const files = []
        for (let i = 3; i < argv.length; i += 2) {
          if (argv[i] !== '--annotate' || !argv[i + 1] || argv[i + 1].startsWith('--')) {
            throw new Error('premises expects repeatable --annotate <file>')
          }
          files.push(argv[i + 1])
        }
        // Read every target before writing any: an unreadable second target must not
        // leave the first annotated on a SAFE-branch abort.
        const updates = [...new Set(files)].map((file) => {
          const before = readFileSync(file, 'utf8')
          return {
            file,
            before,
            ...reconcilePremises(before, { premises, drifted: result.drifted }),
          }
        })
        const changed = updates.filter((update) => update.before !== update.text)
        writePremiseUpdates(changed)
        for (const item of result.drifted) {
          const sections = new Set(
            updates.flatMap((update) =>
              update.annotations
                .filter((annotation) => annotation.id === item.id)
                .flatMap((annotation) => [...annotation.inline, ...annotation.checkbox]),
            ),
          )
          if (updates.length)
            process.stderr.write(
              `premise-annotated: plan-run-guards: ${item.id} flagged in ${[...sections].join(', ') || 'Planning'}\n`,
            )
        }
      }
      return {
        verb,
        code: overLimit || result.unresolved.length > 0 ? 1 : 0,
        reason,
      }
    }
  } catch (error) {
    process.stderr.write(`unreadable-input: plan-run-guards: ${error?.message ?? error}\n`)
    return { verb, code: 1, reason: 'unreadable-input' }
  }
  process.stderr.write(
    'usage: plan-run-guards.mjs metadata <metadata.json> [--module-roots <a,b>] | adopt-metadata <metadata.json> <returnedJson> [--module-roots <a,b>] | idempotence <issue.json> [--selected-id <id>] | premises <premises.json> <live-states.json> [--annotate <file>]... | epic-reverify <bundle.json> | deps <deps-in.json> <candidates.json> --subject <ISSUE-ID>\n',
  )
  return { verb, code: 2, reason: 'unknown-verb' }
}

export function runCli(argv) {
  const { verb, code, reason } = runGuardVerb(argv)
  // One line per invocation, recorded from the single place every verb returns through,
  // so a new verb cannot be added without an outcome.
  createGateRecorder(`plan-run-guards.${verb}`).record(code === 0 ? 'pass' : 'fire', reason)
  return code
}

if (isMainModule(import.meta.url)) {
  process.exitCode = runCli(process.argv.slice(2))
}
