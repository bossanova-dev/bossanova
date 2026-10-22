#!/usr/bin/env node
// guard-id: proto-at-the-edge
import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { parseArgs } from 'node:util'
import { isMainModule } from '../../skills-toolbox/main-module.mjs'
import { scannedFiles } from './registry.mjs'
import { compareBaseline, readBaseline, formatBaseline, splitBaselineEntry } from './baseline.mjs'
import { imports, checkDepguardRule } from './depguard-rule.mjs'
import record from './registry/proto-at-the-edge.mjs'

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..')
const FILES = [
  ...['bosso', 'bossd'].flatMap((svc) => [
    `**/services/${svc}/**`,
    `!**/services/${svc}/api/**`,
    `!**/services/${svc}/cmd/**`,
  ]),
  '!$test',
]
const DENIED = [
  'github.com/recurser/bossalib/gen/bossanova/v1$',
  'github.com/recurser/bossalib/gen/bossanova/v1/',
]
const REMEDY = 'protobuf belongs at the API edge; use an owned domain type or consumer interface'
function reason(file) {
  if (file.startsWith('services/bossd/'))
    return 'bossd protobuf dependency pending 5-3 owned type extraction'
  if (file.startsWith('services/bosso/internal/server/'))
    return 'server transport pending 5-2 extraction'
  if (file.startsWith('services/bosso/internal/stream/'))
    return 'stream protobuf projection pending extraction'
  if (file.startsWith('services/bosso/internal/domain/trigger/'))
    return 'trigger descriptor reads pending domain type extraction'
  if (file.startsWith('services/bosso/internal/domain/authzlevel/'))
    return 'authzlevel descriptor reads pending extraction'
  return 'feature protobuf dependency pending owned type extraction'
}
export function checkProtoAtTheEdge({
  repoRoot = ROOT,
  baselinePath = record.baseline,
  write = false,
} = {}) {
  const offenders = []
  for (const file of scannedFiles(repoRoot, record.scans)) {
    if (!file.endsWith('.go') || file.endsWith('_test.go')) continue
    if (/^services\/(?:bosso|bossd)\/(?:api|cmd)\//.test(file)) continue
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
      formatBaseline(offenders.map((file) => `${file} — ${reason(file)}`)),
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
  const result = checkProtoAtTheEdge({ write: values.write })
  for (const failure of result.failures) console.error(failure)
  if (!result.failures.length)
    console.log(`Protobuf edge imports OK (${result.offenders.length} baseline files)`)
  process.exitCode = result.failures.length ? 1 : 0
}
