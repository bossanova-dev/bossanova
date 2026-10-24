#!/usr/bin/env node
// guard-id: e2e-harness-imports
import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { parseArgs } from 'node:util'
import { isMainModule } from '../../skills-toolbox/main-module.mjs'
import { scannedFiles } from './registry.mjs'
import { compareBaseline, readBaseline, formatBaseline, splitBaselineEntry } from './baseline.mjs'
import { imports, checkDepguardRule } from './depguard-rule.mjs'
import record from './registry/e2e-harness-imports.mjs'

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..')
const EXEMPT = [
  'services/boss/e2e/',
  'services/boss/internal/tuidriver/',
  'services/boss/internal/tuitest/',
  'services/boss/internal/clitest/',
  'services/bossd/testharness/',
]
const DENIED = [
  'github.com/recurser/boss/internal/tuidriver',
  'github.com/recurser/boss/internal/tuitest',
  'github.com/recurser/boss/internal/clitest',
  'github.com/recurser/bossd/testharness',
]
export function checkE2eHarnessImports({ repoRoot = ROOT, write = false } = {}) {
  const offenders = scannedFiles(repoRoot, record.scans).filter(
    (file) =>
      file.endsWith('_test.go') &&
      !EXEMPT.some((dir) => file.startsWith(dir)) &&
      imports(fs.readFileSync(path.join(repoRoot, file), 'utf8')).some((pkg) =>
        DENIED.some((prefix) => pkg.startsWith(prefix)),
      ),
  )
  if (write) {
    const existing = readBaseline(repoRoot, record.baseline)
    const reasons = new Map(
      existing.entries.map((entry) => {
        const { key, reason } = splitBaselineEntry(entry)
        return [key, reason]
      }),
    )
    fs.writeFileSync(
      path.join(repoRoot, record.baseline),
      formatBaseline(
        offenders.map(
          (file) =>
            `${file} — ${reasons.get(file) || 'Existing integration test uses the shared harness; migrate before removing this exception.'}`,
        ),
      ),
    )
  }
  const baseline = readBaseline(repoRoot, record.baseline)
  const entries = baseline.entries.map((entry) => splitBaselineEntry(entry).key)
  const failures = [
    ...baseline.errors,
    ...compareBaseline({
      baselinePath: record.baseline,
      entries,
      offenders,
      remedy:
        'import e2e harnesses only from e2e/ or their own directories; see docs/testing/policy.md',
    }).failures,
    ...checkDepguardRule({
      repoRoot,
      id: record.id,
      denied: DENIED,
      files: ['$test', ...EXEMPT.map((dir) => `!**/${dir}**`)],
      entries,
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
  const result = checkE2eHarnessImports({ write: values.write })
  for (const failure of result.failures) console.error(failure)
  if (!result.failures.length)
    console.log(`e2e-harness-imports OK (${result.offenders.length} baseline files)`)
  process.exitCode = result.failures.length ? 1 : 0
}
