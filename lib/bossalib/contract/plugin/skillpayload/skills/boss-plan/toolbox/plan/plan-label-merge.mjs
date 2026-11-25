// Deterministic planned-label merge for boss-plan Phase 4 writes.
// Preserves the tracker's existing label spellings via shared label-identity
// matching while applying configured spellings only for labels the issue lacks.

import { readFileSync } from 'node:fs'
import { parseArgs } from 'node:util'
import { labelKey, resolveLabelIdentity } from '../label-identity.mjs'
import { isMainModule } from '../main-module.mjs'
import { canonicalContentLabel, labelName, loadSkillConfig } from '../skill-config.mjs'
import { readLabels } from './plan-epic-phase25.mjs'

const USAGE =
  'usage: plan-label-merge.mjs --metadata <draft-metadata.json> --current <current-labels.json> [--interactive] [--add-agent-question] [--siblings-verified]'

/**
 * @param {unknown} value
 * @returns {value is Record<string, unknown>}
 */
function isPlainObject(value) {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}

/**
 * @param {unknown} error
 * @returns {string}
 */
function errorMessage(error) {
  const raw = error instanceof Error ? error.message : String(error)
  return raw.replace(/\s+/g, ' ').trim()
}

/**
 * @typedef {object} MergePlannedLabelsInput
 * @property {object} config
 * @property {unknown} current
 * @property {{
 *   labels?: unknown,
 *   agentBuild?: unknown,
 *   openQuestions?: unknown,
 *   siblings?: unknown,
 * }} metadata
 * @property {boolean} [interactive]
 * @property {boolean} [addAgentQuestion]
 * @property {boolean} [siblingsVerified]
 */

/**
 * Compute the final whole-set labels for a Phase 4 write without rewriting any
 * existing tracker label's spelling.
 *
 * @param {MergePlannedLabelsInput} input
 * @returns {{ labels: string[], added: string[], removed: string[] }}
 */
export function mergePlannedLabels({
  config,
  current,
  metadata,
  interactive = false,
  addAgentQuestion = false,
  siblingsVerified = false,
}) {
  const existing = readLabels(Array.isArray(current) ? { labels: current } : current)

  const agentBuild = labelName(config, 'agentBuild')
  const needsHuman = labelName(config, 'needsHuman')
  const agentPlan = labelName(config, 'agentPlan')
  const agentQuestion = labelName(config, 'agentQuestion')

  const hasUnverifiedSiblings =
    Array.isArray(metadata?.siblings) && metadata.siblings.length > 0 && !siblingsVerified
  const exposeNeedsHuman = hasUnverifiedSiblings || metadata?.agentBuild === false
  const exposureLabel = exposeNeedsHuman ? needsHuman : agentBuild
  const unchosenExposureLabel = exposeNeedsHuman ? agentBuild : needsHuman

  const stripNames = [agentPlan, unchosenExposureLabel]
  if (interactive) stripNames.push(agentQuestion)
  const stripKeys = new Set(stripNames.map(labelKey))

  /** @type {string[]} */
  const kept = []
  /** @type {string[]} */
  const removed = []
  for (const label of existing) {
    if (stripKeys.has(labelKey(label))) {
      removed.push(label)
    } else {
      kept.push(label)
    }
  }

  /** @type {string[]} */
  const additions = []
  if (Array.isArray(metadata?.labels)) {
    for (const label of metadata.labels) {
      const canonical = canonicalContentLabel(config, label)
      if (canonical !== null) additions.push(canonical)
    }
  }
  additions.push(exposureLabel)
  const hasOpenQuestions =
    Array.isArray(metadata?.openQuestions) && metadata.openQuestions.length > 0
  if (!interactive && (hasOpenQuestions || addAgentQuestion)) {
    additions.push(agentQuestion)
  }

  /** @type {string[]} */
  const appended = []
  /** @type {Set<string>} */
  const appendedKeys = new Set()
  for (const addition of additions) {
    const key = labelKey(addition)
    if (key === '') continue
    if (resolveLabelIdentity(addition, kept) !== null) continue
    if (appendedKeys.has(key)) continue
    appendedKeys.add(key)
    appended.push(addition)
  }

  return {
    labels: [...kept, ...appended],
    added: appended,
    removed,
  }
}

/**
 * @param {string} filePath
 * @param {string} label
 * @returns {unknown}
 */
function readJsonFile(filePath, label) {
  let raw
  try {
    raw = readFileSync(filePath, 'utf8')
  } catch (error) {
    throw new Error(`cannot read ${label} file ${filePath}: ${errorMessage(error)}`)
  }
  try {
    return JSON.parse(raw)
  } catch (error) {
    throw new Error(`${label} file ${filePath} is not valid JSON: ${errorMessage(error)}`)
  }
}

/**
 * @param {unknown} value
 */
function assertCurrentShape(value) {
  if (Array.isArray(value)) return
  if (!isPlainObject(value)) {
    throw new Error('current labels input must be a JSON array or issue object')
  }
  if (Array.isArray(value.labels)) return
  if (isPlainObject(value.labels) && Array.isArray(value.labels.nodes)) return
  throw new Error('current.labels must be an array or an object with a nodes array')
}

/**
 * @param {string[]} argv
 * @param {{
 *   cwd?: string,
 *   stdout?: (text: string) => unknown,
 *   stderr?: (text: string) => unknown,
 * }} [io]
 * @returns {number}
 */
export function main(
  argv,
  {
    cwd = process.cwd(),
    stdout = (text) => process.stdout.write(text),
    stderr = (text) => process.stderr.write(text),
  } = {},
) {
  let values
  try {
    ;({ values } = parseArgs({
      args: argv,
      strict: true,
      allowPositionals: false,
      options: {
        metadata: { type: 'string' },
        current: { type: 'string' },
        interactive: { type: 'boolean', default: false },
        'add-agent-question': { type: 'boolean', default: false },
        'siblings-verified': { type: 'boolean', default: false },
      },
    }))
  } catch {
    stderr(`${USAGE}\n`)
    return 2
  }

  if (!values.metadata || !values.current) {
    stderr(`${USAGE}\n`)
    return 2
  }

  try {
    const metadata = readJsonFile(values.metadata, 'metadata')
    if (!isPlainObject(metadata)) {
      throw new Error('metadata input must be a JSON object')
    }
    const current = readJsonFile(values.current, 'current')
    assertCurrentShape(current)
    const config = loadSkillConfig({ cwd })
    const result = mergePlannedLabels({
      config,
      current,
      metadata,
      interactive: Boolean(values.interactive),
      addAgentQuestion: Boolean(values['add-agent-question']),
      siblingsVerified: Boolean(values['siblings-verified']),
    })
    stdout(`${JSON.stringify(result)}\n`)
    return 0
  } catch (error) {
    stderr(`plan-label-merge: input-unreadable: ${errorMessage(error)}\n`)
    return 1
  }
}

if (isMainModule(import.meta.url)) {
  process.exitCode = main(process.argv.slice(2))
}
