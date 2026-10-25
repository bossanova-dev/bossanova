#!/usr/bin/env node

// Report registry coverage for retro classes; uncovered classes are data, not a gate failure.
import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { parseArgs } from 'node:util'

import { isMainModule } from '../../skills-toolbox/main-module.mjs'
import { loadRegistry } from './registry.mjs'

/**
 * @typedef {{classKey: string, aliases?: string[], existingCheck?: string | null}} RetroClass
 * @typedef {{id: string, origin?: {classKeys: string[]}}} CoverageRecord
 */

/**
 * Match exact class keys/aliases or an existingCheck guard id, never prose.
 * The lowest guard id wins if several records cover the class.
 * @param {RetroClass[]} classes
 * @param {CoverageRecord[]} records
 * @returns {{classKey: string, coverage: string}[]}
 */
export function registryCoverage(classes, records) {
  const sorted = [...records].sort((a, b) => (a.id < b.id ? -1 : a.id > b.id ? 1 : 0))
  return classes.map((entry) => {
    const keys = new Set([entry.classKey, ...(entry.aliases ?? [])])
    const guard = sorted.find(
      (record) =>
        record.id === entry.existingCheck || record.origin?.classKeys.some((key) => keys.has(key)),
    )
    return { classKey: entry.classKey, coverage: guard ? `covered:${guard.id}` : 'uncovered' }
  })
}

if (isMainModule(import.meta.url)) {
  const { values } = parseArgs({
    options: { classes: { type: 'string' } },
    allowPositionals: false,
  })
  if (!values.classes) throw new Error('--classes <file> is required')
  const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..')
  const { records, errors } = await loadRegistry(repoRoot)
  if (errors.length > 0) throw new Error(errors.join('\n'))
  const classes = JSON.parse(fs.readFileSync(values.classes, 'utf8'))
  console.log(JSON.stringify(registryCoverage(classes, records)))
}
