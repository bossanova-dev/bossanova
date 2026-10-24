#!/usr/bin/env node
// guard-id: script-layout

import path from 'node:path'
import { parseArgs } from 'node:util'
import { isMainModule } from '../../skills-toolbox/main-module.mjs'
import { isFixturePath, resolveFileSets } from '../layout/file-sets.mjs'
import { textLines, trackedFiles } from '../dev/tree-scan-lib.mjs'
import { compareBaseline, readBaseline } from './baseline.mjs'
import { scannedFiles } from './registry.mjs'
import record from './registry/script-layout.mjs'
import { readMoves, staleReferences } from '../layout/move-ledger.mjs'

const ROOT = path.resolve(import.meta.dirname, '../..')
const LEDGER = 'scripts/layout/moves.json'
const ALLOWLIST = 'scripts/layout/allowlist.json'

export function checkScriptLayout({ repoRoot = ROOT, fileSets = resolveFileSets(repoRoot) } = {}) {
  const tracked = trackedFiles(repoRoot)
  const baseline = readBaseline(repoRoot, record.baseline)
  const allowlist = readBaseline(repoRoot, ALLOWLIST)
  const ledger = readMoves(repoRoot, LEDGER)
  const failures = [...baseline.errors, ...allowlist.errors, ...ledger.errors]
  const allowed = new Set(allowlist.entries)
  const loose = tracked.filter((file) => /^scripts\/[^/]+$/.test(file) && !allowed.has(file))
  failures.push(
    ...compareBaseline({
      baselinePath: record.baseline,
      entries: baseline.entries,
      offenders: loose,
      remedy: 'move this file into an area folder; new loose scripts are not allowed',
    }).failures,
  )
  const selected = new Set(fileSets.test)
  for (const file of tracked) {
    if (
      /^(scripts|skills-toolbox)\/.+\.test\.mjs$/.test(file) &&
      !isFixturePath(file) &&
      !selected.has(file)
    ) {
      failures.push(`${file}: tracked test is missing from the test file-set`)
    }
  }
  const files = scannedFiles(repoRoot, record.scans, { tracked })
  for (const file of files) {
    if (
      file === LEDGER ||
      isFixturePath(file) ||
      (file.startsWith('lib/bossalib/contract/plugin/skillpayload/skills/') &&
        file.includes('/toolbox/'))
    )
      continue
    const lines = textLines(file, repoRoot)
    if (lines) failures.push(...staleReferences(file, lines, ledger.pairs))
  }
  return { failures, loose, files }
}

if (isMainModule(import.meta.url)) {
  const { values } = parseArgs({ options: { root: { type: 'string', default: ROOT } } })
  const result = checkScriptLayout({ repoRoot: path.resolve(values.root) })
  for (const failure of result.failures) console.error(failure)
  if (!result.failures.length)
    console.log(
      `Script layout OK (${result.loose.length} baseline entries, ${result.files.length} scanned files)`,
    )
  process.exitCode = result.failures.length ? 1 : 0
}
