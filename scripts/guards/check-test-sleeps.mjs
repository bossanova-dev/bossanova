#!/usr/bin/env node
import fs from 'node:fs'
import path from 'node:path'
import { parseArgs } from 'node:util'
import { isMainModule } from '../../skills-toolbox/main-module.mjs'
import { scanGoTests, REPO_ROOT } from './go-test-scan.mjs'
import record from './registry/test-sleeps.mjs'
/** @param {string} [root] */
export function scanSleeps(root = REPO_ROOT) {
  const declarations = scanGoTests(root, record).sleeps.filter((item) => !item.inSynctest)
  /** @type {Record<string, number>} */
  const counts = {}
  for (const item of declarations) counts[item.file] = (counts[item.file] ?? 0) + 1
  return {
    counts: Object.fromEntries(
      Object.entries(counts).sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0)),
    ),
    declarations,
  }
}
/** @param {Record<string, number>} counts @param {unknown} baseline */
export function compareBaseline(counts, baseline) {
  const failures = []
  if (!baseline || Array.isArray(baseline) || typeof baseline !== 'object') {
    return [{ kind: 'invalid-baseline', message: `${record.baseline} must be a count map` }]
  }
  const entries = Object.entries(baseline)
  const keys = entries.map(([key]) => key)
  if (keys.some((key, i) => key !== [...keys].sort()[i])) {
    failures.push({ kind: 'invalid-baseline', message: `${record.baseline} must stay sorted` })
  }
  for (const [key, count] of entries) {
    if (!Number.isSafeInteger(count) || count <= 0) {
      failures.push({ kind: 'invalid-baseline', message: `${key}: invalid baseline count` })
    } else if ((counts[key] ?? 0) < count) {
      failures.push({
        kind: 'stale-entry',
        message: `${key}: ${counts[key] ?? 0} remaining, baseline ${count}; lower the entry (remove it at zero)`,
      })
    }
  }
  for (const [key, count] of Object.entries(counts)) {
    if (count > (baseline[key] ?? 0)) {
      failures.push({
        kind: 'new-instance',
        message: `${key}: ${count} exceeds baseline ${baseline[key] ?? 0}`,
        key,
      })
    }
  }
  return failures
}

if (isMainModule(import.meta.url)) {
  const { values } = parseArgs({
    options: { 'write-baseline': { type: 'boolean', default: false } },
    strict: true,
  })
  try {
    const result = scanSleeps()
    const baselinePath = path.join(REPO_ROOT, record.baseline)
    if (values['write-baseline'])
      fs.writeFileSync(baselinePath, `${JSON.stringify(result.counts, null, 2)}\n`)
    const failures = compareBaseline(
      result.counts,
      JSON.parse(fs.readFileSync(baselinePath, 'utf8')),
    )
    for (const failure of failures) {
      console.error(failure.message)
      if (failure.kind === 'new-instance') {
        console.error(
          result.declarations
            .filter((item) => item.file === failure.key)
            .map((item) => `${item.file}:${item.line}`)
            .join('\n'),
        )
        console.error(
          'Use testing/synctest (synctest.Test or synctest.Wait) or lib/bossalib/clocktest',
        )
      }
    }
    if (failures.length) process.exitCode = 1
    else console.log('Test sleeps OK')
  } catch (error) {
    console.error(error.message)
    process.exitCode = 1
  }
}
