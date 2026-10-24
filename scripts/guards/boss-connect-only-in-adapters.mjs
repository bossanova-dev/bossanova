#!/usr/bin/env node
// guard-id: boss-connect-only-in-adapters
import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { parseArgs } from 'node:util'
import { isMainModule } from '../../skills-toolbox/main-module.mjs'
import { scannedFiles } from './registry.mjs'
import { compareBaseline, readBaseline, formatBaseline, splitBaselineEntry } from './baseline.mjs'
import { imports, checkDepguardRule } from './depguard-rule.mjs'
import record from './registry/boss-connect-only-in-adapters.mjs'

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..')
const FILES = ['**/services/boss/**', '!**/services/boss/internal/adapter/**', '!$test']
const DENIED = [
  'connectrpc.com/connect$',
  'github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect$',
]
const REMEDY = 'use rpcerr and keep Connect transport inside internal/adapter'
function reason() {
  return 'TUI mock daemon transport pending BOS-1751'
}
export function checkBossConnectOnlyInAdapters({
  repoRoot = ROOT,
  baselinePath = record.baseline,
  write = false,
} = {}) {
  const offenders = []
  for (const file of scannedFiles(repoRoot, record.scans)) {
    if (!file.endsWith('.go') || file.endsWith('_test.go')) continue
    if (file.startsWith('services/boss/internal/adapter/')) continue
    if (
      imports(fs.readFileSync(path.join(repoRoot, file), 'utf8')).some((pkg) =>
        DENIED.some((denied) =>
          denied.endsWith('$') ? pkg === denied.slice(0, -1) : pkg.startsWith(denied),
        ),
      )
    )
      offenders.push(file)
  }
  if (write)
    fs.writeFileSync(
      path.join(repoRoot, baselinePath),
      formatBaseline(offenders.map((file) => `${file} — ${reason()}`)),
    )
  const baseline = readBaseline(repoRoot, baselinePath)
  const keys = baseline.entries.map((entry) => splitBaselineEntry(entry).key)
  const failures = [
    ...baseline.errors,
    ...compareBaseline({ baselinePath, entries: keys, offenders, remedy: REMEDY }).failures,
    ...checkDepguardRule({
      repoRoot,
      id: record.id,
      denied: DENIED,
      files: FILES,
      entries: keys,
      write: write && !baseline.errors.length,
    }),
  ]
  return { failures, offenders }
}
if (isMainModule(import.meta.url)) {
  const { values } = parseArgs({
    options: {
      check: { type: 'boolean', default: false },
      write: { type: 'boolean', default: false },
    },
  })
  const result = checkBossConnectOnlyInAdapters({ write: values.write })
  for (const failure of result.failures) console.error(failure)
  if (!result.failures.length)
    console.log(`Boss Connect boundary OK (${result.offenders.length} baseline files)`)
  process.exitCode = result.failures.length ? 1 : 0
}
