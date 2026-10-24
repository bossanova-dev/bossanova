#!/usr/bin/env node
// guard-id: ts-cognitive-complexity

import fs from 'node:fs'
import path from 'node:path'
import { parseArgs } from 'node:util'
import { isMainModule } from '../../skills-toolbox/main-module.mjs'
import { readBudgets } from './budgets.mjs'
import { scannedFiles } from './registry.mjs'
import record from './registry/ts-cognitive-complexity.mjs'

const RULE = 'noExcessiveCognitiveComplexity'
const SUPPRESSIONS = 'ts-cognitive-complexity-suppressions'
const EXTENSIONS = new Set(['.ts', '.tsx', '.js', '.jsx', '.mjs', '.cjs', '.astro', '.vue'])
const SKIP_DIRS = new Set(['node_modules', 'gen', 'dist', '.astro'])

/** Count rule-specific Biome directives, excluding ignore-end and other rules.
 * @param {string} text
 * @returns {number}
 */
export function countSuppressions(text) {
  const comments = text.match(/\/\/[^\n]*|\/\*[\s\S]*?\*\//g) ?? []
  let count = 0
  for (const comment of comments) {
    count += [
      ...comment.matchAll(
        /\bbiome-ignore(?:-all|-start)?\s+[^:\r\n]*?\blint\/complexity\/noExcessiveCognitiveComplexity\b/g,
      ),
    ].length
  }
  return count
}

/** @param {{repoRoot?: string}} [options] */
export function checkTSComplexity({ repoRoot = path.resolve(import.meta.dirname, '../..') } = {}) {
  const { budgets, errors } = readBudgets(repoRoot)
  const failures = [...errors]
  if (errors.length > 0) return { failures }
  const threshold = budgets[record.id]?.default
  if (threshold === undefined) failures.push(`${record.id}: missing default complexity budget`)
  const suppressionBudget = budgets[SUPPRESSIONS]
  if (!suppressionBudget || suppressionBudget.default !== undefined) {
    failures.push(`${SUPPRESSIONS}: requires a subjects-only budget (implicit default 0)`)
  }
  const subjects = suppressionBudget?.subjects ?? {}
  const measured = new Map()
  const files = scannedFiles(repoRoot, record.scans)
  const configs = record.scans.filter((file) => file.endsWith('/biome.json'))
  for (const file of configs) {
    try {
      const config = JSON.parse(fs.readFileSync(path.join(repoRoot, file), 'utf8'))
      const baseRule = config.linter?.rules?.complexity?.[RULE]
      const rules = baseRule === undefined ? [] : [baseRule]
      let globalRule = baseRule !== undefined
      for (const override of config.overrides ?? []) {
        const rule = override.linter?.rules?.complexity?.[RULE]
        if (rule !== undefined) {
          rules.push(rule)
          if (override.includes?.length === 1 && override.includes[0] === '**') globalRule = true
        }
      }
      if (
        !globalRule ||
        rules.some(
          (rule) => rule?.level !== 'error' || rule?.options?.maxAllowedComplexity !== threshold,
        )
      ) {
        failures.push(`${file}: ${RULE} must state error with maxAllowedComplexity ${threshold}`)
      }
    } catch (error) {
      failures.push(`${file}: cannot read Biome config: ${error.message}`)
    }
  }
  for (const file of files) {
    if (!EXTENSIONS.has(path.extname(file)) || file.split('/').some((part) => SKIP_DIRS.has(part)))
      continue
    try {
      measured.set(file, countSuppressions(fs.readFileSync(path.join(repoRoot, file), 'utf8')))
    } catch (error) {
      failures.push(`${file}: source vanished or cannot be read: ${error.message}`)
    }
  }
  for (const [file, count] of measured) {
    const allowed = subjects[file] ?? 0
    if (count > allowed)
      failures.push(`${file}: ${count} suppressions exceeds its budget of ${allowed}`)
    if (count < allowed)
      failures.push(`${file}: ${count} suppressions; remove or lower its budget of ${allowed}`)
  }
  for (const file of Object.keys(subjects)) {
    if (!measured.has(file))
      failures.push(
        `${file}: budgeted source vanished or is no longer scanned; remove its suppression budget`,
      )
  }
  return { failures }
}

if (isMainModule(import.meta.url)) {
  parseArgs({ options: {}, strict: true, allowPositionals: false })
  const { failures } = checkTSComplexity()
  if (failures.length > 0) {
    console.error(failures.join('\n'))
    process.exitCode = 1
  } else {
    console.log('TypeScript complexity threshold and suppression budgets OK')
  }
}
