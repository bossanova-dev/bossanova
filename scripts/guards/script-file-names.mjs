#!/usr/bin/env node
// guard-id: script-file-names
import path from 'node:path'
import { parseArgs } from 'node:util'
import { isMainModule } from '../../skills-toolbox/main-module.mjs'
import { trackedFiles } from '../dev/tree-scan-lib.mjs'
import { loadRegistry, scannedFiles } from './registry.mjs'
import record from './registry/script-file-names.mjs'

const ROOT = path.resolve(import.meta.dirname, '../..')
const STANDARD_FILES = new Set([
  'scripts/Makefile',
  'scripts/AGENTS.md',
  'scripts/CLAUDE.md',
  'skills-toolbox/AGENTS.md',
  'skills-toolbox/CLAUDE.md',
])
const NAME = /^[a-z0-9]+(?:-[a-z0-9]+)*(?:\.[a-z0-9]+(?:-[a-z0-9]+)*)*$/

export async function checkScriptFileNames({ repoRoot = ROOT } = {}) {
  const { records, errors } = await loadRegistry(repoRoot)
  const failures = [...errors]
  const owned = new Set(records.map((record) => record.baseline))
  const files = scannedFiles(repoRoot, record.scans, { tracked: trackedFiles(repoRoot) })
  const checked = new Set()
  for (const file of files) {
    // Fixture names are test inputs, including names intentionally rejected by guards.
    if (file.split('/').some((part) => ['fixtures', 'testdata'].includes(part))) continue
    const parts = file.split('/')
    for (let i = 1; i < parts.length; i++) {
      const subject = parts.slice(0, i + 1).join('/')
      if (checked.has(subject)) continue
      checked.add(subject)
      if (!STANDARD_FILES.has(subject) && !NAME.test(parts[i]))
        failures.push(`${subject}: use a kebab-case name with optional dotted suffixes`)
    }
    if (file.endsWith('-baseline.json') && !owned.has(file))
      failures.push(`${file}: baseline must belong to a registered guard's baseline field`)
  }
  return { failures }
}

if (isMainModule(import.meta.url)) {
  const { values } = parseArgs({
    options: { root: { type: 'string', default: ROOT } },
    allowPositionals: false,
  })
  const { failures } = await checkScriptFileNames({ repoRoot: path.resolve(values.root) })
  for (const failure of failures) console.error(failure)
  if (!failures.length) console.log('Script file names OK')
  process.exitCode = failures.length ? 1 : 0
}
