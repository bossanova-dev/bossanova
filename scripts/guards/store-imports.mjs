#!/usr/bin/env node
// guard-id: store-imports
import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { imports } from './depguard-rule.mjs'
import { isMainModule } from '../../skills-toolbox/main-module.mjs'
import { scannedFiles } from './registry.mjs'
import { compareBaseline, readBaseline } from './baseline.mjs'
import record from './registry/store-imports.mjs'

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..')
const REMEDY =
  'the store imports only entity types; move the shared shape into internal/domain, or define it in the store so the feature imports the store'

export function checkStoreImports({ repoRoot = ROOT, baselinePath = record.baseline } = {}) {
  const edges = new Set()
  for (const file of scannedFiles(repoRoot, record.scans)) {
    if (file.endsWith('_test.go')) continue
    const service = file.split('/')[1]
    const servicePrefix = `github.com/recurser/${service}/`
    const domain = `${servicePrefix}internal/domain`
    const store = `${servicePrefix}internal/db/`
    for (const pkg of imports(fs.readFileSync(path.join(repoRoot, file), 'utf8'))) {
      if (
        pkg.startsWith('github.com/recurser/bossalib/gen/') ||
        (pkg.startsWith(servicePrefix) &&
          pkg !== domain &&
          !pkg.startsWith(`${domain}/`) &&
          !pkg.startsWith(store))
      )
        edges.add(`services/${service}/internal/db -> ${pkg}`)
    }
  }
  const offenders = [...edges].sort()
  const baseline = readBaseline(repoRoot, baselinePath)
  const failures = [
    ...baseline.errors,
    ...compareBaseline({ baselinePath, entries: baseline.entries, offenders, remedy: REMEDY })
      .failures,
  ]
  return { failures, offenders }
}

if (isMainModule(import.meta.url)) {
  const result = checkStoreImports()
  for (const failure of result.failures) console.error(failure)
  if (!result.failures.length)
    console.log(`Store imports OK (${result.offenders.length} baseline entries)`)
  process.exitCode = result.failures.length ? 1 : 0
}
