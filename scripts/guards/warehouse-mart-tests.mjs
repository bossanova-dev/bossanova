#!/usr/bin/env node
// guard-id: warehouse-mart-tests
import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { parseArgs } from 'node:util'
import { isMainModule } from '../../skills-toolbox/main-module.mjs'
import { scannedFiles } from './registry.mjs'

import { parse } from 'yaml'
import record from './registry/warehouse-mart-tests.mjs'

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..')

/** @param {{repoRoot?: string}} [options] */
export function checkWarehouseMartTests({ repoRoot = ROOT } = {}) {
  const files = scannedFiles(repoRoot, record.scans),
    tested = new Set(),
    failures = []
  const hasTests = (node) =>
    [node?.tests, node?.data_tests].some((tests) => Array.isArray(tests) && tests.length > 0)
  for (const file of files) {
    if (file.endsWith('.yml')) {
      try {
        const doc = parse(fs.readFileSync(path.join(repoRoot, file), 'utf8'))
        for (const model of doc?.models ?? []) {
          if (hasTests(model) || (model.columns ?? []).some(hasTests)) tested.add(model.name)
        }
        for (const unit of doc?.unit_tests ?? []) tested.add(unit.model)
      } catch (error) {
        failures.push(`${file}: ${error.message}`)
      }
    } else if (/^services\/warehouse\/data-tests\/[^/]+\.sql$/.test(file)) {
      const sql = fs
        .readFileSync(path.join(repoRoot, file), 'utf8')
        .replace(/--[^\n]*|\/\*[\s\S]*?\*\//g, '')
      for (const match of sql.matchAll(/\bref\(\s*['"]([^'"]+)['"]\s*\)/g)) tested.add(match[1])
    }
  }
  const models = files.filter((file) =>
    /^services\/warehouse\/models\/(marts|growth)\/.*\.sql$/.test(file),
  )
  for (const file of models) {
    if (!tested.has(path.basename(file, '.sql')))
      failures.push(`${file}: no dbt test covers this mart model`)
  }
  return { failures, models: models.length, gaps: failures.length }
}
if (isMainModule(import.meta.url)) {
  parseArgs({ options: {}, allowPositionals: false })
  const { failures, models } = checkWarehouseMartTests()
  for (const failure of failures) console.error(failure)
  if (!failures.length) console.log(`Warehouse mart tests OK (${models} models, zero gaps)`)
  process.exitCode = failures.length ? 1 : 0
}
