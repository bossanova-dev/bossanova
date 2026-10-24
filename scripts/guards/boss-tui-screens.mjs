import fs from 'node:fs'
import path from 'node:path'
import { scannedFiles } from './registry.mjs'
import { compareBaseline, readBaseline, formatBaseline, splitBaselineEntry } from './baseline.mjs'
import { imports, checkDepguardRule } from './depguard-rule.mjs'

const FILES = ['**/services/boss/api/tui/*/**', '!**/services/boss/api/tui/ui/**', '!$test']

/**
 * @param {{repoRoot: string, record: {id: string, scans: string[], baseline: string},
 * denied: string[], allowed?: string[], listMode?: string, write?: boolean, baselinePath?: string}} options
 */
export function checkBossTuiScreens({
  repoRoot,
  record,
  denied,
  allowed = null,
  listMode = null,
  write = false,
  baselinePath = record.baseline,
}) {
  const matches = (pkg, pattern) =>
    pattern.endsWith('$') ? pkg === pattern.slice(0, -1) : pkg.startsWith(pattern)
  const offenders = scannedFiles(repoRoot, record.scans).filter((file) => {
    if (
      !/^services\/boss\/api\/tui\/(?!ui\/)[^/]+\/.+\.go$/.test(file) ||
      file.endsWith('_test.go')
    )
      return false
    return imports(fs.readFileSync(path.join(repoRoot, file), 'utf8')).some(
      (pkg) =>
        denied.some((pattern) => matches(pkg, pattern)) &&
        !(allowed ?? []).some((pattern) => matches(pkg, pattern)),
    )
  })
  if (write) fs.writeFileSync(path.join(repoRoot, baselinePath), formatBaseline(offenders))
  const baseline = readBaseline(repoRoot, baselinePath)
  const entries = baseline.entries.map((entry) => splitBaselineEntry(entry).key)
  const failures = [
    ...baseline.errors,
    ...compareBaseline({
      baselinePath,
      entries,
      offenders,
      remedy: 'declare a consumer interface and communicate through api/tui/ui messages',
    }).failures,
    ...checkDepguardRule({
      repoRoot,
      id: record.id,
      denied,
      allowed,
      listMode,
      files: FILES,
      entries,
      write: write && !baseline.errors.length,
    }),
  ]
  return { failures, offenders }
}
