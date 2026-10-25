#!/usr/bin/env node
// guard-id: agents-index-size
// guard-id: go-file-size
// guard-id: tsx-file-size
// guard-id: test-file-size
// guard-id: script-file-size
// guard-id: warehouse-file-size
// guard-id: warehouse-test-file-size
import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { parseArgs } from 'node:util'
import { isMainModule } from '../../skills-toolbox/main-module.mjs'
import { scannedFiles } from './registry.mjs'

import { readBudgets, checkBudget } from './budgets.mjs'
import production from './registry/warehouse-file-size.mjs'
import tests from './registry/warehouse-test-file-size.mjs'
import agents from './registry/agents-index-size.mjs'
import scripts from './registry/script-file-size.mjs'
import go from './registry/go-file-size.mjs'
import tsx from './registry/tsx-file-size.mjs'
import sourceTests from './registry/test-file-size.mjs'
import { isFixturePath } from '../layout/file-sets.mjs'

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..')

/** Measure physical newline characters in repo-relative files, matching existing size budgets.
 * @param {string} repoRoot
 * @param {string[]} files
 * @returns {Map<string, number>}
 */
export function measureFileLines(repoRoot, files) {
  return new Map(
    files.map((file) => [
      file,
      (fs.readFileSync(path.join(repoRoot, file), 'utf8').match(/\n/g) ?? []).length,
    ]),
  )
}

/** @param {string} repoRoot
 * @param {string[]} files
 * @returns {Map<string, number>}
 */
export function measureFileBytes(repoRoot, files) {
  return new Map(files.map((file) => [file, fs.statSync(path.join(repoRoot, file)).size]))
}

/** Measure each file-size guard with the filters used by enforcement.
 * @param {string} repoRoot
 * @returns {Map<string, Map<string, number>>}
 */
export function measureBudgetedGuards(repoRoot) {
  const measurements = new Map()
  for (const record of [agents, production, tests, scripts, go, tsx, sourceTests]) {
    const files = scannedFiles(repoRoot, record.scans).filter((file) => {
      if (record === scripts)
        return !isFixturePath(file) && !/\.(?:test|generated)\.mjs$/.test(file)
      if (record === go) {
        if (/(?:^|\/)(?:gen|testdata)\//.test(file) || /(?:_test|\.pb)\.go$/.test(file))
          return false
        const source = fs.readFileSync(path.join(repoRoot, file), 'utf8')
        const packageStart = source.search(/^package\s/m)
        const header = packageStart < 0 ? source : source.slice(0, packageStart)
        return !/^\/\/ Code generated .*DO NOT EDIT\.\r?$/m.test(header)
      }
      if (record === tsx) return !/(?:^|\/)gen\//.test(file) && !/\.(?:test|spec)\.tsx$/.test(file)
      if (record === sourceTests && file.endsWith('.go')) return !/(?:^|\/)testdata\//.test(file)
      return true
    })
    const measured =
      record === agents ? measureFileBytes(repoRoot, files) : measureFileLines(repoRoot, files)
    measurements.set(record.id, measured)
  }
  return measurements
}

/** @param {{repoRoot?: string}} [options] */
export function checkFileSizes({ repoRoot = ROOT } = {}) {
  const { budgets, errors } = readBudgets(repoRoot)
  const failures = [...errors]
  for (const [guardId, measured] of measureBudgetedGuards(repoRoot)) {
    failures.push(...checkBudget({ budgets, guardId, measured }).failures)
    const budget = budgets[guardId]
    for (const subject of Object.keys(budget?.subjects ?? {})) {
      if (measured.has(subject) && measured.get(subject) <= budget.default)
        failures.push(`${guardId}: ${subject} no longer needs its own budget — remove it`)
    }
  }
  return { failures }
}
if (isMainModule(import.meta.url)) {
  parseArgs({ options: {}, allowPositionals: false })
  const { failures } = checkFileSizes()
  for (const failure of failures) console.error(failure)
  if (!failures.length) console.log('File sizes OK')
  process.exitCode = failures.length ? 1 : 0
}
