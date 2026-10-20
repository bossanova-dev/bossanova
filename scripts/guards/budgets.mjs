// The single budgets file for registered guards: scripts/guards/budgets.json.
//
// Shape: { "<guard-id>": { "default"?: <int>, "subjects"?: { "<subject>": <int> } } }, every key
// sorted and every value a positive integer. A budget is a limit, not a ratchet: a subject under
// its budget passes. Raising a budget is a plain edit to the file in the PR.

import fs from 'node:fs'
import path from 'node:path'

export const BUDGETS_PATH = 'scripts/guards/budgets.json'

const GUARD_KEYS = new Set(['default', 'subjects'])

const isPlainObject = (value) =>
  value !== null && typeof value === 'object' && !Array.isArray(value)
const isPositiveInteger = (value) => Number.isInteger(value) && value > 0
const isSorted = (keys) => keys.every((key, index) => index === 0 || keys[index - 1] < key)

/**
 * Structural errors in a parsed budgets value.
 *
 * @param {unknown} budgets
 * @param {string} [label] Name used in error messages.
 * @returns {string[]}
 */
export function validateBudgets(budgets, label = BUDGETS_PATH) {
  if (!isPlainObject(budgets)) return [`${label} must be a JSON object`]
  const errors = []
  const guardIds = Object.keys(budgets)
  if (!isSorted(guardIds)) errors.push(`${label}: top-level keys must be sorted`)
  for (const guardId of guardIds) {
    const entry = budgets[guardId]
    if (!isPlainObject(entry)) {
      errors.push(`${label}: ${guardId} must be an object`)
      continue
    }
    const keys = Object.keys(entry)
    for (const key of keys) {
      if (!GUARD_KEYS.has(key)) errors.push(`${label}: ${guardId} has unknown key ${key}`)
    }
    if (!isSorted(keys)) errors.push(`${label}: ${guardId} keys must be sorted`)
    if ('default' in entry && !isPositiveInteger(entry.default)) {
      errors.push(`${label}: ${guardId}.default must be a positive integer`)
    }
    if ('subjects' in entry) {
      if (!isPlainObject(entry.subjects)) {
        errors.push(`${label}: ${guardId}.subjects must be an object`)
        continue
      }
      const subjects = Object.keys(entry.subjects)
      if (!isSorted(subjects)) errors.push(`${label}: ${guardId}.subjects keys must be sorted`)
      for (const subject of subjects) {
        if (!isPositiveInteger(entry.subjects[subject])) {
          errors.push(`${label}: ${guardId}.subjects.${subject} must be a positive integer`)
        }
      }
    }
  }
  return errors
}

/**
 * Read and validate the budgets file.
 *
 * @param {string} repoRoot Absolute repository root.
 * @param {string} [relPath]
 * @returns {{budgets: Record<string, {default?: number, subjects?: Record<string, number>}>,
 *   errors: string[]}}
 */
export function readBudgets(repoRoot, relPath = BUDGETS_PATH) {
  let parsed
  try {
    parsed = JSON.parse(fs.readFileSync(path.join(repoRoot, relPath), 'utf8'))
  } catch (error) {
    return { budgets: {}, errors: [`cannot read ${relPath}: ${error.message}`] }
  }
  const errors = validateBudgets(parsed, relPath)
  return { budgets: errors.length === 0 ? parsed : {}, errors }
}

/**
 * Check one guard's measurements against its budget.
 *
 * @param {{budgets: object, guardId: string,
 *   measured: Map<string, number> | Record<string, number>}} input
 * @returns {{failures: string[]}}
 */
export function checkBudget({ budgets, guardId, measured }) {
  const entry = budgets?.[guardId] ?? {}
  const counts = measured instanceof Map ? measured : new Map(Object.entries(measured ?? {}))
  const subjects = entry.subjects ?? {}
  const failures = []
  for (const [subject, count] of [...counts].sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0))) {
    if (Object.hasOwn(subjects, subject)) {
      if (count > subjects[subject]) {
        failures.push(
          `${guardId}: ${subject}: ${count} is over its budget of ${subjects[subject]} (${BUDGETS_PATH})`,
        )
      }
    } else if (entry.default !== undefined && count > entry.default) {
      failures.push(
        `${guardId}: ${subject}: ${count} is over the default budget of ${entry.default} (${BUDGETS_PATH})`,
      )
    }
  }
  for (const subject of Object.keys(subjects)) {
    if (!counts.has(subject)) {
      failures.push(
        `${guardId}: ${subject} has a budget but no measurement — remove it from ${BUDGETS_PATH}`,
      )
    }
  }
  return { failures }
}
