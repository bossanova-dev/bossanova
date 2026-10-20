#!/usr/bin/env node
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

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..')

/** @param {{repoRoot?: string}} [options] */
export function checkFileSizes({ repoRoot = ROOT } = {}) {
  const { budgets, errors } = readBudgets(repoRoot)
  const failures = [...errors]
  for (const record of [production, tests]) {
    const measured = new Map(
      scannedFiles(repoRoot, record.scans).map((file) => [
        file,
        (fs.readFileSync(path.join(repoRoot, file), 'utf8').match(/\n/g) ?? []).length,
      ]),
    )
    failures.push(...checkBudget({ budgets, guardId: record.id, measured }).failures)
    const budget = budgets[record.id]
    for (const subject of Object.keys(budget?.subjects ?? {})) {
      if (measured.has(subject) && measured.get(subject) <= budget.default)
        failures.push(`${record.id}: ${subject} no longer needs its own budget — remove it`)
    }
  }
  return { failures }
}
if (isMainModule(import.meta.url)) {
  parseArgs({ options: {}, allowPositionals: false })
  const { failures } = checkFileSizes()
  for (const failure of failures) console.error(failure)
  if (!failures.length) console.log('Warehouse file sizes OK')
  process.exitCode = failures.length ? 1 : 0
}
