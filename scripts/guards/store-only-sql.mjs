#!/usr/bin/env node
// guard-id: store-only-sql
import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { parseArgs } from 'node:util'
import { imports, checkDepguardRule } from './depguard-rule.mjs'
import { isMainModule } from '../../skills-toolbox/main-module.mjs'
import { scannedFiles } from './registry.mjs'
import { compareBaseline, readBaseline, formatBaseline } from './baseline.mjs'
import record from './registry/store-only-sql.mjs'

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..')
const DENIED = [
  'database/sql',
  'modernc.org/sqlite',
  'github.com/jackc/pgx/v5',
  'github.com/pressly/goose/v3',
  'github.com/lib/pq',
]
const FILES = [
  '**/services/bossd/**/*.go',
  '**/services/bosso/**/*.go',
  '!$test',
  '!**/internal/db/*.go',
  '!**/internal/db/sqlcgen/*.go',
  '!**/internal/dbtest/**',
  '!**/internal/testharness/**',
]
const REMEDY = 'call the store; use errors.Is(err, domain.ErrNotFound) instead of sql.ErrNoRows'
export function checkStoreOnlySql({
  repoRoot = ROOT,
  baselinePath = record.baseline,
  write = false,
} = {}) {
  const failures = [],
    offenders = []
  for (const file of scannedFiles(repoRoot, record.scans)) {
    if (!/^services\/(?:bossd|bosso)\/.+\.go$/.test(file) || file.endsWith('_test.go')) continue
    if (
      /\/internal\/(?:db\/[^/]+\.go|db\/sqlcgen\/[^/]+\.go|dbtest\/.*|testharness\/.*)$/.test(file)
    )
      continue
    if (
      imports(fs.readFileSync(path.join(repoRoot, file), 'utf8')).some((pkg) =>
        DENIED.some((prefix) => pkg.startsWith(prefix)),
      )
    )
      offenders.push(file)
  }
  if (write) fs.writeFileSync(path.join(repoRoot, baselinePath), formatBaseline(offenders))
  const baseline = readBaseline(repoRoot, baselinePath)
  failures.push(
    ...baseline.errors,
    ...compareBaseline({ baselinePath, entries: baseline.entries, offenders, remedy: REMEDY })
      .failures,
  )
  failures.push(
    ...checkDepguardRule({
      repoRoot,
      id: record.id,
      denied: DENIED,
      files: FILES,
      entries: baseline.entries,
      write: write && !baseline.errors.length,
    }),
  )
  return { failures, offenders }
}
if (isMainModule(import.meta.url)) {
  const { values } = parseArgs({
    options: {
      check: { type: 'boolean', default: false },
      write: { type: 'boolean', default: false },
    },
  })
  const result = checkStoreOnlySql({ write: values.write })
  for (const failure of result.failures) console.error(failure)
  if (!result.failures.length)
    console.log(`Store SQL imports OK (${result.offenders.length} baseline entries)`)
  process.exitCode = result.failures.length ? 1 : 0
}
