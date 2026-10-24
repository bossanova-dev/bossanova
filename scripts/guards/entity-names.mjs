#!/usr/bin/env node

// guard-id: entity-names

import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { parseArgs } from 'node:util'
import ts from 'typescript'
import { isMainModule } from '../../skills-toolbox/main-module.mjs'
import { globToRegExp } from '../../skills-toolbox/skill-config.mjs'
import { stripLineComments } from './check-proto-field-names.mjs'
import { compareBaseline, readBaseline, formatBaseline } from './baseline.mjs'
import { loadEntities } from './entities.mjs'
import { parseExceptions, isExcepted } from './exceptions.mjs'
import { scannedFiles } from './registry.mjs'
import record from './registry/entity-names.mjs'

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..')
const words = (name) =>
  name
    .replace(/([A-Z]+)([A-Z][a-z])/g, '$1 $2')
    .replace(/([a-z0-9])([A-Z])/g, '$1 $2')
    .split(/[^A-Za-z0-9]+/)
    .filter(Boolean)
    .map((w) => w.toLowerCase())
const plural = (a, b) => a === b || a === `${b}s` || a === `${b}es`

export function matchesBan(name, ban, verbs, file) {
  if (ban.paths && !ban.paths.some((p) => globToRegExp(p).test(file))) return false
  if (ban.exclude_paths?.some((p) => globToRegExp(p).test(file))) return false
  if (ban.kind === 'text') {
    let at = name.indexOf(ban.match)
    while (at !== -1) {
      if (!/[a-z]/.test(name[at + ban.match.length] ?? '')) return true
      at = name.indexOf(ban.match, at + 1)
    }
    return false
  }
  const input = words(name),
    match = words(ban.match)
  for (let i = 0; i <= input.length - match.length; i++) {
    if (
      !match.every((w, j) =>
        j === match.length - 1 ? plural(input[i + j], w) : input[i + j] === w,
      )
    )
      continue
    if (ban.position === 'whole' && (i !== 0 || input.length !== match.length)) continue
    if (ban.except_after?.some((w) => w.toLowerCase() === input[i - 1])) continue
    if (ban.position === 'after-verb' && !verbs.some((w) => w.toLowerCase() === input[i - 1]))
      continue
    return true
  }
  return false
}

// Tokens preserve source offsets while comments and quoted/raw literals cannot manufacture declarations.
function goTokens(source) {
  const pattern =
    /\/\/[^\n]*|\/\*[\s\S]*?\*\/|`[^`]*`|"(?:\\.|[^"\\])*"|'(?:\\.|[^'\\])*'|[A-Za-z_][A-Za-z0-9_]*|[^\s]/g
  const tokens = []
  let line = 1,
    previous = 0
  for (const m of source.matchAll(pattern)) {
    line += source.slice(previous, m.index).split('\n').length - 1
    previous = m.index
    if (/^(?:\/\/|\/\*|`|"|')/.test(m[0])) continue
    tokens.push({ text: m[0], at: m.index, line })
  }
  return tokens
}
export function goDeclarations(source) {
  const tokens = goTokens(source),
    found = []
  let braces = 0,
    parens = 0,
    group = null
  const add = (token, name = token?.text) => {
    if (token && /^[A-Z]/.test(name.split('.').at(-1))) found.push({ name, line: token.line })
  }
  for (let i = 0; i < tokens.length; i++) {
    const t = tokens[i],
      v = t.text
    if (braces === 0 && parens === 0 && ['func', 'type', 'const', 'var'].includes(v)) {
      if (v === 'func') {
        let next = i + 1,
          recv = ''
        if (tokens[next]?.text === '(') {
          let depth = 1
          const start = ++next
          while (next < tokens.length && depth) {
            if (tokens[next].text === '(') depth++
            if (tokens[next].text === ')') depth--
            next++
          }
          const ids = tokens.slice(start, next - 1).filter((t) => /^[A-Za-z_]/.test(t.text))
          recv = ids[1]?.text ?? ids[0]?.text ?? ''
        }
        add(tokens[next], recv ? `${recv}.${tokens[next]?.text}` : tokens[next]?.text)
      } else if (tokens[i + 1]?.text === '(') group = v
      else {
        add(tokens[i + 1])
        if (v === 'var' || v === 'const')
          for (let j = i + 2; tokens[j]?.text === ','; j += 2) add(tokens[j + 1])
      }
    } else if (
      group &&
      braces === 0 &&
      parens === 1 &&
      /^[A-Za-z_]/.test(v) &&
      tokens[i - 1]?.line < t.line
    ) {
      add(t)
      if (group === 'var' || group === 'const')
        for (let j = i + 1; tokens[j]?.text === ','; j += 2) add(tokens[j + 1])
    }
    if (v === '{') braces++
    if (v === '}') braces--
    if (v === '(') parens++
    if (v === ')') {
      parens--
      if (parens === 0) group = null
    }
  }
  return found
}
export function tsDeclarations(source) {
  const ast = ts.createSourceFile(
      'source.tsx',
      source,
      ts.ScriptTarget.Latest,
      true,
      ts.ScriptKind.TSX,
    ),
    found = []
  const add = (node) => {
    if (node && ts.isIdentifier(node))
      found.push({
        name: node.text,
        line: ast.getLineAndCharacterOfPosition(node.getStart(ast)).line + 1,
      })
  }
  for (const node of ast.statements) {
    if (ts.isExportDeclaration(node) && node.exportClause && ts.isNamedExports(node.exportClause))
      for (const item of node.exportClause.elements) add(item.name)
    else if (
      ts.canHaveModifiers(node) &&
      ts.getModifiers(node)?.some((m) => m.kind === ts.SyntaxKind.ExportKeyword)
    ) {
      if (ts.isVariableStatement(node))
        for (const d of node.declarationList.declarations) add(d.name)
      else if ('name' in node) add(node.name)
    }
  }
  return found
}
function protoDeclarations(source) {
  let block = false
  return source.split('\n').flatMap((line, index) => {
    const result = stripLineComments(line, block)
    block = result.inBlock
    return [...result.line.matchAll(/\b(message|enum|service|rpc)\s+(\w+)/g)].map((m) => ({
      name: m[2],
      kind: m[1],
      line: index + 1,
    }))
  })
}
export function replayTables(sources) {
  const tables = new Set()
  for (const source of sources) {
    const up = source.split(/--\s*\+goose\s+Down\b/)[0].replace(/--[^\n]*|\/\*[\s\S]*?\*\//g, '')
    const ident = '["`]?([A-Za-z_][A-Za-z0-9_]*)["`]?'
    const pattern = new RegExp(
      `CREATE\\s+TABLE\\s+(?:IF\\s+NOT\\s+EXISTS\\s+)?${ident}|ALTER\\s+TABLE\\s+${ident}\\s+RENAME\\s+TO\\s+${ident}|DROP\\s+TABLE\\s+(?:IF\\s+EXISTS\\s+)?${ident}`,
      'gi',
    )
    for (const m of up.matchAll(pattern)) {
      if (m[1]) tables.add(m[1])
      else if (m[2]) {
        tables.delete(m[2])
        tables.add(m[3])
      } else tables.delete(m[4])
    }
  }
  return tables
}
export function checkEntityNames({
  repoRoot = ROOT,
  entitiesFile = 'docs/structure/entities.yaml',
  baselinePath = record.baseline,
  writeBaseline = false,
} = {}) {
  const config = loadEntities(path.resolve(repoRoot, entitiesFile)),
    failures = [...config.errors],
    offenders = new Map()
  if (failures.length) return { failures, warnings: config.warnings, offenders: [] }
  const files = scannedFiles(repoRoot, record.scans)
  const inspect = (layer, file, decl, exceptions = null) => {
    if (exceptions && isExcepted(exceptions, decl.line)) return
    for (const entity of config.entities)
      for (const ban of entity.bans) {
        if (!ban.layers.includes(layer) || !matchesBan(decl.name, ban, config.verbs, file)) continue
        const id =
          layer === 'table'
            ? `table:${decl.service}:${decl.modelName ?? decl.name}`
            : `${layer}:${file}:${decl.kind ? `${decl.kind} ` : ''}${decl.name}`
        offenders.set(
          id,
          `"${ban.match}" is a banned synonym for ${entity.code}. Use ${ban.use ?? (layer === 'table' ? entity.table : entity.code)} (docs/structure/entities.yaml)`,
        )
      }
  }
  for (const file of files) {
    if (file.startsWith('services/warehouse/models/') && file.endsWith('.sql')) {
      const modelName = path.basename(file, '.sql')
      const name = file.startsWith('services/warehouse/models/growth/')
        ? modelName
        : modelName.includes('__')
          ? modelName.slice(modelName.lastIndexOf('__') + 2)
          : modelName.replace(/^mart_/, '')
      inspect('table', file, { name, modelName, service: 'warehouse' })
      continue
    }
    let layer
    if (file.endsWith('.go') && !file.endsWith('_test.go') && !file.split('/').includes('gen'))
      layer = 'go'
    if (file.startsWith('proto/') && file.endsWith('.proto')) layer = 'proto'
    if (/^services\/web\/src\/.+\.tsx?$/.test(file) && !file.startsWith('services/web/src/gen/'))
      layer = 'ts'
    if (!layer) continue
    const source = fs.readFileSync(path.join(repoRoot, file), 'utf8')
    if (layer === 'go' && /^\/\/ Code generated .*DO NOT EDIT\./m.test(source)) continue
    const exceptions = parseExceptions(source, { guardId: record.id })
    failures.push(...exceptions.errors.map((e) => `${file}: ${e}`))
    const declarations =
      layer === 'go'
        ? goDeclarations(source)
        : layer === 'ts'
          ? tsDeclarations(source)
          : protoDeclarations(source)
    for (const decl of declarations) inspect(layer, file, decl, exceptions)
  }
  for (const service of ['bossd', 'bosso']) {
    const dir = `services/${service}/${service === 'bossd' ? 'migrations' : 'migrations_postgres'}/`
    for (const name of replayTables(
      files
        .filter((f) => f.startsWith(dir) && f.endsWith('.sql'))
        .map((f) => fs.readFileSync(path.join(repoRoot, f), 'utf8')),
    ))
      inspect('table', dir, { name, service })
  }
  if (writeBaseline && !failures.length)
    fs.writeFileSync(path.resolve(repoRoot, baselinePath), formatBaseline(offenders.keys()))
  const baseline = readBaseline(repoRoot, baselinePath)
  failures.push(
    ...baseline.errors,
    ...compareBaseline({
      baselinePath,
      entries: baseline.entries,
      offenders: offenders.keys(),
      remedy: (id) => offenders.get(id),
    }).failures,
  )
  return { failures, warnings: config.warnings, offenders: [...offenders.keys()].sort() }
}
if (isMainModule(import.meta.url)) {
  const { values } = parseArgs({
    options: { 'write-baseline': { type: 'boolean', default: false } },
  })
  const result = checkEntityNames({ writeBaseline: values['write-baseline'] })
  for (const warning of result.warnings) console.error(`warning: ${warning}`)
  for (const failure of result.failures) console.error(failure)
  if (!result.failures.length)
    console.log(`Entity names OK (${result.offenders.length} baseline entries)`)
  process.exitCode = result.failures.length ? 1 : 0
}
