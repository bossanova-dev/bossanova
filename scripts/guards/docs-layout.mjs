#!/usr/bin/env node
// guard-id: docs-layout
import path from 'node:path'
import { parseArgs } from 'node:util'
import { isMainModule } from '../../skills-toolbox/main-module.mjs'
import { trackedFiles, textLines } from '../dev/tree-scan-lib.mjs'
import { readMoves, staleReferences } from '../layout/move-ledger.mjs'
import { compareBaseline, readBaseline, splitBaselineEntry } from './baseline.mjs'
import { parseExceptions, isExcepted } from './exceptions.mjs'
import record from './registry/docs-layout.mjs'
const ROOT = path.resolve(import.meta.dirname, '../..')
const ALLOWLIST = 'scripts/layout/docs-allowlist.json'
const LEDGER = 'scripts/layout/docs-moves.json'
/** @param {{root?: string}} [options] */
export function check({ root = ROOT } = {}) {
  const tracked = trackedFiles(root)
  const allowlist = readBaseline(root, ALLOWLIST)
  const baseline = readBaseline(root, record.baseline)
  const ledger = readMoves(root, LEDGER)
  const allowed = new Set(allowlist.entries.map((entry) => splitBaselineEntry(entry).key))
  const top = new Set(
    tracked
      .filter((file) => file.startsWith('docs/'))
      .map((file) => file.split('/').slice(0, 2).join('/')),
  )
  const loose = [...top].filter((file) => !allowed.has(file) || tracked.includes(file))
  const failures = [
    ...allowlist.errors,
    ...baseline.errors,
    ...ledger.errors,
    ...compareBaseline({
      baselinePath: record.baseline,
      entries: baseline.entries.map((entry) => splitBaselineEntry(entry).key),
      offenders: loose,
      remedy: `move this entry into a directory allowed by ${ALLOWLIST}`,
    }).failures,
  ]
  for (const file of tracked) {
    if (file === LEDGER) continue
    const lines = textLines(file, root)
    if (!lines) continue
    const exceptions = parseExceptions(lines.join('\n'), { guardId: record.id })
    failures.push(...exceptions.errors.map((error) => `${file}: ${error}`))
    const filtered = lines.map((line, index) => (isExcepted(exceptions, index + 1) ? '' : line))
    failures.push(...staleReferences(file, filtered, ledger.pairs))
  }
  return { failures }
}
if (isMainModule(import.meta.url)) {
  const { values } = parseArgs({
    strict: true,
    options: { root: { type: 'string', default: ROOT } },
  })
  const { failures } = check({ root: values.root })
  for (const failure of failures) console.error(failure)
  if (!failures.length) console.log('docs-layout OK')
  process.exitCode = failures.length ? 1 : 0
}
