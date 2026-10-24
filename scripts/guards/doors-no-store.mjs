#!/usr/bin/env node
// guard-id: doors-no-store
import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { parseArgs } from 'node:util'
import { isMainModule } from '../../skills-toolbox/main-module.mjs'
import { scannedFiles } from './registry.mjs'
import { compareBaseline, readBaseline, formatBaseline } from './baseline.mjs'
import { imports, checkDepguardRule } from './depguard-rule.mjs'
import record from './registry/doors-no-store.mjs'

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..')
const FILES = [
  '**/services/bossd/api/daemon/**',
  ...['plugin', 'upstream'].map((pkg) => `**/services/bossd/internal/${pkg}/**`),
  '**/services/bossd/api/**',
  '**/services/bosso/api/**',
  '!$test',
]
const DENIED = ['bosso', 'bossd'].flatMap((svc) => [
  `github.com/recurser/${svc}/internal/db$`,
  `github.com/recurser/${svc}/internal/db/`,
])
DENIED.push(...['bosso', 'bossd'].map((svc) => `github.com/recurser/${svc}/internal/adapter/`))
const REMEDY =
  'doors must not import stores or adapters; declare a consumer interface in this package and inject it from cmd through the constructor'
export function checkDoorsNoStore({
  repoRoot = ROOT,
  baselinePath = record.baseline,
  write = false,
} = {}) {
  const offenders = []
  for (const file of scannedFiles(repoRoot, record.scans)) {
    if (
      !/^services\/(?:bossd\/(?:api\/|internal\/(?:server|plugin|upstream)\/)|bosso\/api\/)/.test(
        file,
      )
    )
      continue
    if (!file.endsWith('.go') || file.endsWith('_test.go')) continue
    if (
      imports(fs.readFileSync(path.join(repoRoot, file), 'utf8')).some((pkg) =>
        DENIED.some((denied) =>
          denied.endsWith('$') ? pkg === denied.slice(0, -1) : pkg.startsWith(denied),
        ),
      )
    )
      offenders.push(file)
  }
  if (write) fs.writeFileSync(path.join(repoRoot, baselinePath), formatBaseline(offenders))
  const baseline = readBaseline(repoRoot, baselinePath)
  const failures = [
    ...baseline.errors,
    ...compareBaseline({ baselinePath, entries: baseline.entries, offenders, remedy: REMEDY })
      .failures,
    ...checkDepguardRule({
      repoRoot,
      id: record.id,
      denied: DENIED,
      files: FILES,
      entries: baseline.entries,
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
  const result = checkDoorsNoStore({ write: values.write })
  for (const failure of result.failures) console.error(failure)
  if (!result.failures.length)
    console.log(`Door store imports OK (${result.offenders.length} baseline files)`)
  process.exitCode = result.failures.length ? 1 : 0
}
