#!/usr/bin/env node

// Deterministic guards for boss-plan's run boundaries. These checks sit between
// untrusted drafting output and tracker writeback, so they return structured
// violations and keep the CLI shape small enough for skill bash blocks.

import { readFileSync } from 'node:fs'

import { checkPlanContract } from './plan-contract-guard.mjs'
import { selectImplementationPlanAttachment } from './plan-attachment.mjs'
import { isMainModule } from './main-module.mjs'
import {
  DEFAULT_CONFIG,
  loadSkillConfig,
  stateName,
  validatePlanDescription,
} from './skill-config.mjs'

export const PREMISE_LIMIT = 10

const ALLOWED_METADATA_KEYS = new Set([
  'planPath',
  'labels',
  'agentFriendly',
  'estimate',
  'priority',
  'openQuestions',
  'descriptionSummary',
])

const VALID_ESTIMATES = new Set([0, 1, 2, 3, 5])

function entry(code, field, message, extra = {}) {
  return { code, field, message: `plan-run-guards: ${message}`, ...extra }
}

function hasAtomic5Justification(descriptionSummary) {
  const sections = String(descriptionSummary ?? '').split(/\n(?=##\s)/)
  const planning = sections.find((section) => section.trimStart().startsWith('## Planning'))
  return /^[-*]\s*Atomic-5:\s*\S/m.test(planning ?? '')
}

export function validateDraftMetadata(metadata, { config = DEFAULT_CONFIG } = {}) {
  const missing = []
  const invalid = []
  const violations = []

  const object =
    metadata && typeof metadata === 'object' && !Array.isArray(metadata) ? metadata : null
  if (!object) {
    violations.push(entry('metadata-not-object', 'metadata', 'metadata must be a JSON object'))
    return { ok: false, missing, invalid, violations }
  }

  for (const key of ALLOWED_METADATA_KEYS) {
    if (!Object.hasOwn(object, key)) missing.push(key)
  }
  for (const key of Object.keys(object)) {
    if (!ALLOWED_METADATA_KEYS.has(key)) {
      violations.push(entry('unknown-key', key, `metadata carries unknown top-level key "${key}"`))
    }
  }

  if (Object.hasOwn(object, 'planPath')) {
    if (typeof object.planPath !== 'string' || object.planPath.trim() === '')
      invalid.push('planPath')
  }
  if (Object.hasOwn(object, 'labels')) {
    if (!Array.isArray(object.labels) || object.labels.some((label) => typeof label !== 'string')) {
      invalid.push('labels')
    }
  }
  if (Object.hasOwn(object, 'agentFriendly')) {
    if (typeof object.agentFriendly !== 'boolean') invalid.push('agentFriendly')
  }
  if (Object.hasOwn(object, 'estimate')) {
    if (!Number.isInteger(object.estimate) || !VALID_ESTIMATES.has(object.estimate)) {
      invalid.push('estimate')
    } else if (object.estimate === 5 && !hasAtomic5Justification(object.descriptionSummary)) {
      invalid.push('estimate')
      violations.push(
        entry(
          'missing-atomic-5',
          'estimate',
          'estimate 5 requires an "- Atomic-5:" justification under ## Planning',
        ),
      )
    }
  }
  if (Object.hasOwn(object, 'priority')) {
    if (!Number.isInteger(object.priority) || object.priority < 1 || object.priority > 4) {
      invalid.push('priority')
    }
  }
  if (Object.hasOwn(object, 'openQuestions')) {
    if (
      !Array.isArray(object.openQuestions) ||
      object.openQuestions.some((question) => typeof question !== 'string')
    ) {
      invalid.push('openQuestions')
    }
  }
  if (Object.hasOwn(object, 'descriptionSummary')) {
    if (typeof object.descriptionSummary !== 'string' || object.descriptionSummary.trim() === '') {
      invalid.push('descriptionSummary')
    } else {
      const contract = checkPlanContract({ description: object.descriptionSummary, config })
      for (const violation of contract.violations) {
        violations.push(
          entry(
            `description-summary-${violation.code}`,
            'descriptionSummary',
            violation.message.replace(/^plan-contract-guard:\s*/, ''),
            { source: violation },
          ),
        )
      }
    }
  }

  return {
    ok: missing.length === 0 && invalid.length === 0 && violations.length === 0,
    missing,
    invalid: [...new Set(invalid)],
    violations,
  }
}

export function planIdempotencePrecheck({ issue, config = DEFAULT_CONFIG } = {}) {
  const reasons = []
  const attachments = Array.isArray(issue?.attachments) ? issue.attachments : []

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

  for (const premise of list) {
    const id = typeof premise?.id === 'string' ? premise.id : ''
    if (!id) continue
    if (!Object.hasOwn(states, id)) {
      unresolved.push(id)
      continue
    }
    const plannedState = premise.state
    const currentState = states[id]
    if (plannedState !== currentState) drifted.push({ id, plannedState, currentState })
  }

  return {
    ok: drifted.length === 0 && unresolved.length === 0,
    drifted,
    unresolved,
  }
}

function readJSON(file) {
  return JSON.parse(readFileSync(file, 'utf8'))
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
}

export function runCli(argv) {
  const [command, first, second] = argv
  try {
    if (command === 'metadata' && first) {
      const result = validateDraftMetadata(readJSON(first), { config: loadSkillConfig() })
      printViolations(result)
      return result.ok ? 0 : 1
    }
    if (command === 'idempotence' && first) {
      const result = planIdempotencePrecheck({ issue: readJSON(first), config: loadSkillConfig() })
      process.stdout.write(`${JSON.stringify(result)}\n`)
      return 0
    }
    if (command === 'premises' && first && second) {
      const premises = readJSON(first)
      const liveStates = readJSON(second)
      const result = premiseDrift(premises, liveStates)
      const overLimit = Array.isArray(premises) && premises.length > PREMISE_LIMIT
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
      return overLimit || result.unresolved.length > 0 ? 1 : 0
    }
  } catch (error) {
    process.stderr.write(`unreadable-input: plan-run-guards: ${error?.message ?? error}\n`)
    return 1
  }
  process.stderr.write(
    'usage: plan-run-guards.mjs metadata <metadata.json> | idempotence <issue.json> | premises <premises.json> <live-states.json>\n',
  )
  return 2
}

if (isMainModule(import.meta.url)) {
  process.exitCode = runCli(process.argv.slice(2))
}
