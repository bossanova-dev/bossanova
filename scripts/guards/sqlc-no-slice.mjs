#!/usr/bin/env node
// guard-id: sqlc-no-slice
import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { parseArgs } from 'node:util'
import { isMainModule } from '../../skills-toolbox/main-module.mjs'
import { scannedFiles } from './registry.mjs'
import record from './registry/sqlc-no-slice.mjs'

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..')
export function checkSqlcNoSlice({ repoRoot = ROOT } = {}) {
  const failures = []
  for (const file of scannedFiles(repoRoot, record.scans)) {
    const source = fs.readFileSync(path.join(repoRoot, file), 'utf8')
    // Keep offsets while masking literals and comments; a quoted '--' is not a comment.
    const masked = source.replace(/'(?:''|[^'])*'|"(?:""|[^"])*"|--[^\n]*|\/\*[\s\S]*?\*\//g, (s) =>
      s.replace(/[^\n]/g, ' '),
    )
    for (const m of masked.matchAll(/\bsqlc\.slice\s*\(/gi)) {
      const line = masked.slice(0, m.index).split('\n').length
      failures.push(
        `${file}:${line}: sqlc.slice is invalid for Postgres; use = ANY($1::text[]) and pass the slice as an array parameter`,
      )
    }
  }
  return { failures }
}
if (isMainModule(import.meta.url)) {
  parseArgs({ options: {} })
  const result = checkSqlcNoSlice()
  for (const failure of result.failures) console.error(failure)
  if (!result.failures.length) console.log('Postgres sqlc queries OK (no sqlc.slice)')
  process.exitCode = result.failures.length ? 1 : 0
}
