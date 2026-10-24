#!/usr/bin/env node
import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { parseArgs } from 'node:util'
import { isMainModule } from '../../skills-toolbox/main-module.mjs'
import { compareBaseline, readBaseline, splitBaselineEntry } from './baseline.mjs'
import { scannedFiles } from './registry.mjs'
import record from './registry/web-api-casts.mjs'

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..')
const baselinePath = record.baseline

/** @param {string} root */
export function checkWebApiCasts(root = repoRoot) {
  const { entries, errors } = readBaseline(root, baselinePath)
  if (errors.length) return { failures: errors }
  const offenders = scannedFiles(root, record.scans).filter((file) =>
    /\bas\s+(unknown\s+as\s+)?(Partial<\s*)?[A-Za-z]*Api\b/.test(
      fs.readFileSync(path.join(root, file), 'utf8'),
    ),
  )
  return compareBaseline({
    baselinePath,
    entries: entries.map((entry) => splitBaselineEntry(entry).key),
    offenders,
    remedy: 'Use fakeApi from services/web/tests/mocks/api.ts instead of an Api cast.',
  })
}

if (isMainModule(import.meta.url)) {
  const { values } = parseArgs({ options: { root: { type: 'string' } }, strict: true })
  const { failures } = checkWebApiCasts(values.root ?? repoRoot)
  if (failures.length) {
    console.error(failures.join('\n'))
    process.exitCode = 1
  } else {
    console.log('OK: web API cast baseline is current; no new casts')
  }
}
