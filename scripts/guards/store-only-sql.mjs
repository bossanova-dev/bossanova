#!/usr/bin/env node
// guard-id: store-only-sql
import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { parseArgs } from 'node:util'
import { parse } from 'yaml'
import { isMainModule } from '../../skills-toolbox/main-module.mjs'
import { scannedFiles } from './registry.mjs'
import { compareBaseline, readBaseline, formatBaseline } from './baseline.mjs'
import record from './registry/store-only-sql.mjs'

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..')
const DENIED = [
  'database/sql',
  'modernc.org/sqlite',
  'github.com/jackc/pgx/v5',
  'github.com/pressly/goose/v3',
  'github.com/lib/pq',
]
const FILES = [
  '**/services/bossd/**/*.go',
  '**/services/bosso/**/*.go',
  '!$test',
  '!**/internal/db/*.go',
  '!**/internal/db/sqlcgen/*.go',
  '!**/internal/dbtest/**',
  '!**/internal/testharness/**',
]
const BEGIN =
  '# BEGIN generated: store-only-sql baseline (node scripts/guards/store-only-sql.mjs --write)'
const END = '# END generated'
const REMEDY = 'call the store; use errors.Is(err, domain.ErrNotFound) instead of sql.ErrNoRows'
function generatedBlock(entries) {
  return `${BEGIN}\n${entries.map((file) => `            - "!**/${file}"\n`).join('')}            ${END}`
}
function imports(source) {
  const tokens = [
    ...source.matchAll(
      /\/\/[^\n]*|\/\*[\s\S]*?\*\/|`[^`]*`|"(?:\\.|[^"\\])*"|'(?:\\.|[^'\\])*'|[A-Za-z_][A-Za-z0-9_]*|[^\s]/g,
    ),
  ]
    .map((match) => match[0])
    .filter((token) => !token.startsWith('//') && !token.startsWith('/*'))
  const packages = []
  for (let i = 0; i < tokens.length; i++) {
    if (tokens[i] !== 'import') continue
    const grouped = tokens[i + 1] === '('
    let j = i + 1
    for (; j < tokens.length; j++) {
      const token = tokens[j]
      if (grouped && token === ')') break
      if (token.startsWith('"') || token.startsWith('`')) {
        packages.push(token.slice(1, -1))
        if (!grouped) break
      }
    }
    i = j
  }
  return packages
}
export function checkStoreOnlySql({
  repoRoot = ROOT,
  baselinePath = record.baseline,
  write = false,
} = {}) {
  const failures = [],
    offenders = []
  for (const file of scannedFiles(repoRoot, record.scans)) {
    if (!/^services\/(?:bossd|bosso)\/.+\.go$/.test(file) || file.endsWith('_test.go')) continue
    if (
      /\/internal\/(?:db\/[^/]+\.go|db\/sqlcgen\/[^/]+\.go|dbtest\/.*|testharness\/.*)$/.test(file)
    )
      continue
    if (
      imports(fs.readFileSync(path.join(repoRoot, file), 'utf8')).some((pkg) =>
        DENIED.some((prefix) => pkg.startsWith(prefix)),
      )
    )
      offenders.push(file)
  }
  if (write) fs.writeFileSync(path.join(repoRoot, baselinePath), formatBaseline(offenders))
  const baseline = readBaseline(repoRoot, baselinePath)
  failures.push(
    ...baseline.errors,
    ...compareBaseline({ baselinePath, entries: baseline.entries, offenders, remedy: REMEDY })
      .failures,
  )
  const configPath = path.join(repoRoot, '.golangci.yml')
  try {
    let config = fs.readFileSync(configPath, 'utf8')
    const start = config.indexOf(BEGIN),
      end = config.indexOf(END, start)
    if (start < 0 || end < 0)
      failures.push('.golangci.yml: missing store-only-sql generated baseline block')
    else {
      const expected = generatedBlock(baseline.entries)
      if (write && !baseline.errors.length) {
        config = config.slice(0, start) + expected + config.slice(end + END.length)
        fs.writeFileSync(configPath, config)
      } else if (config.slice(start, end + END.length) !== expected)
        failures.push(
          '.golangci.yml: generated store-only-sql baseline block is stale; run make store-only-sql-update',
        )
    }
    const rule = parse(config)?.linters?.settings?.depguard?.rules?.['store-only-sql']
    if (!rule) failures.push('.golangci.yml: missing store-only-sql depguard rule')
    for (const pkg of DENIED)
      if (!rule?.deny?.some((entry) => entry.pkg === pkg))
        failures.push(`.golangci.yml: store-only-sql missing deny ${pkg}`)
    const expectedFiles = [...FILES, ...baseline.entries.map((file) => `!**/${file}`)]
    if (JSON.stringify(rule?.files) !== JSON.stringify(expectedFiles))
      failures.push(
        '.golangci.yml: store-only-sql files must match service scopes, store/test exemptions and baseline',
      )
  } catch (error) {
    failures.push(`.golangci.yml: ${error.message}`)
  }
  return { failures, offenders }
}
if (isMainModule(import.meta.url)) {
  const { values } = parseArgs({
    options: {
      check: { type: 'boolean', default: false },
      write: { type: 'boolean', default: false },
    },
  })
  const result = checkStoreOnlySql({ write: values.write })
  for (const failure of result.failures) console.error(failure)
  if (!result.failures.length)
    console.log(`Store SQL imports OK (${result.offenders.length} baseline entries)`)
  process.exitCode = result.failures.length ? 1 : 0
}
