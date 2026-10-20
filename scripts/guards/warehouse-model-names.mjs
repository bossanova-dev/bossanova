#!/usr/bin/env node
// guard-id: warehouse-model-names
import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { parseArgs } from 'node:util'
import { isMainModule } from '../../skills-toolbox/main-module.mjs'
import { scannedFiles } from './registry.mjs'

import { readBaseline, compareBaseline } from './baseline.mjs'
import { parseExceptions, isExcepted } from './exceptions.mjs'
import record from './registry/warehouse-model-names.mjs'

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..')

/** @param {string} file */
function validName(file) {
  const [layer, ...parts] = file.slice('services/warehouse/models/'.length).split('/')
  const stem = parts.pop().slice(0, -4)
  const snake = /^[a-z][a-z0-9]*(?:_[a-z0-9]+)*$/
  if (!stem.split('__').every((part) => snake.test(part))) return false
  if (layer === 'growth') return !/^(stg|int|mart)_/.test(stem)
  if (layer === 'staging') {
    const match = /^stg_(.+)__(.+)$/.exec(stem)
    return Boolean(match && parts.length && match[1] === parts[0])
  }
  if (layer === 'intermediate') {
    const match = /^int_(.+)__(.+)$/.exec(stem)
    return Boolean(match && (!parts.length || match[1] === parts[0]))
  }
  if (layer === 'marts') {
    if (!stem.startsWith('mart_')) return false
    return !stem.includes('__') || stem.slice(5).split('__')[0] === parts[0]
  }
  return false
}
/** @param {{repoRoot?: string}} [options] */
export function checkWarehouseModelNames({ repoRoot = ROOT } = {}) {
  const failures = [],
    offenders = []
  for (const file of scannedFiles(repoRoot, record.scans)) {
    const exceptions = parseExceptions(fs.readFileSync(path.join(repoRoot, file), 'utf8'), {
      guardId: record.id,
    })
    failures.push(...exceptions.errors.map((error) => `${file}: ${error}`))
    if (!validName(file) && !isExcepted(exceptions, 1)) offenders.push(file)
  }
  const baseline = readBaseline(repoRoot, record.baseline)
  failures.push(
    ...baseline.errors,
    ...compareBaseline({
      baselinePath: record.baseline,
      entries: baseline.entries,
      offenders,
      remedy: 'use the dbt layer naming convention',
    }).failures,
  )
  return { failures, offenders }
}
if (isMainModule(import.meta.url)) {
  parseArgs({ options: {}, allowPositionals: false })
  const { failures, offenders } = checkWarehouseModelNames()
  for (const failure of failures) console.error(failure)
  if (!failures.length)
    console.log(`Warehouse model names OK (${offenders.length} baseline entries)`)
  process.exitCode = failures.length ? 1 : 0
}
