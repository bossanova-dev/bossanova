#!/usr/bin/env node
// guard-id: store-verbs
import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { parseArgs } from 'node:util'
import { isMainModule } from '../../skills-toolbox/main-module.mjs'
import { scannedFiles } from './registry.mjs'
import { compareBaseline, readBaseline, formatBaseline } from './baseline.mjs'
import record from './registry/store-verbs.mjs'

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..')
const VERBS = [
  'Get',
  'List',
  'Count',
  'Create',
  'Upsert',
  'Update',
  'Set',
  'Mark',
  'Record',
  'Delete',
]
const REPLACEMENTS = {
  Insert: 'Create',
  Remove: 'Delete',
  Find: 'Get/List',
  Fetch: 'Get/List',
  Load: 'Get/List',
  Lookup: 'Get/List',
  Save: 'Upsert/Update',
  Recent: 'List/Get',
  Latest: 'List/Get',
}
const words = (name) =>
  name
    .replace(/([A-Z]+)([A-Z][a-z])/g, '$1 $2')
    .replace(/([a-z0-9])([A-Z])/g, '$1 $2')
    .split(' ')
function violation(name, receiver = '') {
  name = name.replace(/Unscoped$/, '')
  const verb = VERBS.find(
    (v) => name.startsWith(v) && (/^[A-Z]/.test(name.slice(v.length)) || name === v),
  )
  if (!verb) {
    const banned = Object.keys(REPLACEMENTS).find((v) => name.startsWith(v))
    return `allowed verbs: ${VERBS.join(', ')}${banned ? `; replace ${banned} with ${REPLACEMENTS[banned]}` : ''}`
  }
  const entity = words(
    receiver
      .replace(/^(?:SQLite|SQL)/, '')
      .replace(/^(?:Notifying|Recomputing)/, '')
      .replace(/Store$/, ''),
  )
  const rest = words(name.slice(verb.length))
  if (receiver && entity[0] && rest.some((_, i) => entity.every((word, j) => rest[i + j] === word)))
    return `remove repeated entity ${entity.join('')}; allowed verbs: ${VERBS.join(', ')}`
  return null
}
export function checkStoreVerbs({
  repoRoot = ROOT,
  baselinePath = record.baseline,
  write = false,
} = {}) {
  const offenders = new Map()
  for (const file of scannedFiles(repoRoot, record.scans)) {
    if (file.endsWith('_test.go') || file.includes('/sqlcgen/')) continue
    const source = fs.readFileSync(path.join(repoRoot, file), 'utf8')
    const declarations = []
    if (file.endsWith('.go')) {
      const masked = source.replace(
        /\/\/[^\n]*|\/\*[\s\S]*?\*\/|`[^`]*`|"(?:\\.|[^"\\])*"|'(?:\\.|[^'\\])*'/g,
        (s) => s.replace(/[^\n]/g, ' '),
      )
      for (const m of masked.matchAll(/^func \(\w+ \*?([A-Z]\w*Store|Store)\) ([A-Z]\w*)\(/gm))
        declarations.push({ receiver: m[1], name: m[2] })
    } else if (file.endsWith('.sql')) {
      for (const m of source.matchAll(/^\s*--\s*name:\s*(\w+)\s+:\w+/gm))
        declarations.push({ name: m[1] })
    }
    for (const { receiver, name } of declarations) {
      const problem = violation(name, receiver)
      if (problem)
        offenders.set(
          `${path.posix.dirname(file)}:${receiver ? `${receiver}.` : ''}${name}`,
          problem,
        )
    }
  }
  if (write) fs.writeFileSync(path.join(repoRoot, baselinePath), formatBaseline(offenders.keys()))
  const baseline = readBaseline(repoRoot, baselinePath)
  return {
    offenders: [...offenders.keys()].sort(),
    failures: [
      ...baseline.errors,
      ...compareBaseline({
        baselinePath,
        entries: baseline.entries,
        offenders: offenders.keys(),
        remedy: (key) => offenders.get(key),
      }).failures,
    ],
  }
}
if (isMainModule(import.meta.url)) {
  const { values } = parseArgs({ options: { write: { type: 'boolean', default: false } } })
  const result = checkStoreVerbs({ write: values.write })
  for (const failure of result.failures) console.error(failure)
  if (!result.failures.length)
    console.log(`Store verbs OK (${result.offenders.length} baseline entries)`)
  process.exitCode = result.failures.length ? 1 : 0
}
