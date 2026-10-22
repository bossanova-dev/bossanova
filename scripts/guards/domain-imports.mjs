#!/usr/bin/env node
// guard-id: domain-imports
import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { parseArgs } from 'node:util'
import { isMainModule } from '../../skills-toolbox/main-module.mjs'
import { imports, checkDepguardRule } from './depguard-rule.mjs'
import { scannedFiles } from './registry.mjs'
import record from './registry/domain-imports.mjs'

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..')
const DENIED = ['github.com/recurser/bossd/', 'github.com/recurser/bosso/']
const ALLOWED = DENIED.flatMap((prefix) => [
  prefix + 'internal/domain$',
  prefix + 'internal/domain/',
])
const FILES = [
  '**/services/bossd/internal/domain/**',
  '**/services/bosso/internal/domain/**',
  '!$test',
]
const REMEDY =
  'internal/domain holds entity types and pure decisions and imports only other domain packages within services; move the dependency to the caller'

export function checkDomainImports({ repoRoot = ROOT } = {}) {
  const failures = checkDepguardRule({
    repoRoot,
    id: 'domain-imports-nothing-internal',
    denied: DENIED,
    files: FILES,
    allowed: ALLOWED,
    listMode: 'lax',
  })
  const offenders = []
  for (const file of scannedFiles(repoRoot, record.scans)) {
    if (
      !/^services\/(?:bossd|bosso)\/internal\/domain\/.+\.go$/.test(file) ||
      file.endsWith('_test.go')
    )
      continue
    const denied = imports(fs.readFileSync(path.join(repoRoot, file), 'utf8')).filter(
      (pkg) =>
        DENIED.some((prefix) => pkg.startsWith(prefix)) &&
        !ALLOWED.some((allowed) =>
          allowed.endsWith('$') ? pkg === allowed.slice(0, -1) : pkg.startsWith(allowed),
        ),
    )
    if (denied.length) {
      offenders.push(file)
      for (const pkg of denied) failures.push(`${file}: imports ${pkg}; ${REMEDY}`)
    }
  }
  return { failures, offenders }
}

if (isMainModule(import.meta.url)) {
  parseArgs()
  const result = checkDomainImports()
  for (const failure of result.failures) console.error(failure)
  if (!result.failures.length) console.log('Domain imports OK')
  process.exitCode = result.failures.length ? 1 : 0
}
