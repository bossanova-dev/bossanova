#!/usr/bin/env node
import fs from 'node:fs'
import path from 'node:path'
import { execFileSync } from 'node:child_process'
import { parseArgs } from 'node:util'
import { isMainModule } from '../../skills-toolbox/main-module.mjs'

const escape = (text) => text.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')
const git = (root, args) => execFileSync('git', args, { cwd: root, encoding: 'utf8' })

function keptAttributes(source) {
  const lines = source.split('\n')
  const kept = []
  let pending = false
  for (let i = 0; i < lines.length; i++) {
    if (/^\s*#/.test(lines[i])) {
      pending ||= /# keep\b/.test(lines[i])
      continue
    }
    if (!/^\s+\w+\s*=/.test(lines[i])) {
      if (lines[i].trim()) pending = false
      continue
    }
    let value = '',
      depth = 0,
      keep = pending
    pending = false
    do {
      const line = lines[i]
      keep ||= /# keep\b/.test(line)
      const tokens = line.match(/"(?:\\.|[^"\\])*"|'(?:\\.|[^'\\])*'|#[^\n]*|[^\s]/g) ?? []
      for (const token of tokens) {
        if (token.startsWith('#')) break
        value += token
        if (['(', '[', '{'].includes(token)) depth++
        if ([')', ']', '}'].includes(token)) depth--
      }
      if (depth <= 0) break
      i++
    } while (i < lines.length)
    if (keep) kept.push(value)
  }
  return kept
}

/** @param {{root?: string, from?: string, to?: string, run?: (command: string, args: string[]) => unknown}} options */
export function moveGoPackage({ root = process.cwd(), from, to, run } = {}) {
  root = fs.realpathSync(root)
  for (const value of [from, to]) {
    if (
      !value ||
      path.isAbsolute(value) ||
      value.split('/').some((part) => !part || part === '..' || part === '.')
    )
      throw new Error('from and to must be repository-relative directory paths')
    let parent = path.dirname(path.join(root, value))
    while (!fs.existsSync(parent)) parent = path.dirname(parent)
    const real = fs.realpathSync(parent)
    if (real !== root && !real.startsWith(root + path.sep))
      throw new Error('package paths must stay inside the repository')
  }
  if (to.startsWith(from + '/') || fs.existsSync(path.join(root, to)))
    throw new Error('destination must be new and outside the source')
  let moduleDir = path.join(root, from)
  while (!fs.existsSync(path.join(moduleDir, 'go.mod'))) {
    if (moduleDir === root) throw new Error('source must belong to a Go module')
    moduleDir = path.dirname(moduleDir)
  }
  const module = fs
    .readFileSync(path.join(moduleDir, 'go.mod'), 'utf8')
    .match(/^module\s+(\S+)/m)?.[1]
  if (!module || !path.join(root, to).startsWith(moduleDir + path.sep))
    throw new Error('move must stay within its Go module')
  const oldImport =
    module + '/' + path.relative(moduleDir, path.join(root, from)).split(path.sep).join('/')
  const newImport =
    module + '/' + path.relative(moduleDir, path.join(root, to)).split(path.sep).join('/')
  const files = git(root, ['ls-files', '-z']).split('\0').filter(Boolean)
  const builds = files
    .filter((file) => file.startsWith(from + '/') && /(?:^|\/)BUILD(?:\.bazel)?$/.test(file))
    .map((file) => ({
      file: to + file.slice(from.length),
      kept: keptAttributes(fs.readFileSync(path.join(root, file), 'utf8')),
    }))
  fs.mkdirSync(path.dirname(path.join(root, to)), { recursive: true })
  git(root, ['mv', '--', from, to])
  const touchedGo = []
  const citation = new RegExp(escape(from) + '(?=/|$|[^A-Za-z0-9_*?/-])', 'g')
  const importPath = new RegExp(escape(oldImport) + '(?=[/"`])', 'g')
  for (const oldFile of files) {
    const file = oldFile.startsWith(from + '/') ? to + oldFile.slice(from.length) : oldFile
    if (file.startsWith('.codex/') || !fs.lstatSync(path.join(root, file)).isFile()) continue
    const bytes = fs.readFileSync(path.join(root, file))
    if (bytes.includes(0)) continue
    const before = bytes.toString('utf8')
    if (!Buffer.from(before).equals(bytes)) continue
    const after = file.endsWith('.go')
      ? before.replace(importPath, newImport)
      : before.replace(citation, to)
    if (after !== before) fs.writeFileSync(path.join(root, file), after)
    if (file.endsWith('.go') && (after !== before || file.startsWith(to + '/')))
      touchedGo.push(file)
  }
  const execute = run ?? ((cmd, args) => execFileSync(cmd, args, { cwd: root, stdio: 'inherit' }))
  if (touchedGo.length) {
    execute('gofmt', ['-w', ...touchedGo])
    execute('goimports', ['-w', ...touchedGo])
  }
  execute('bazel', ['run', '//:gazelle'])
  for (const { file, kept } of builds) {
    const actual = keptAttributes(fs.readFileSync(path.join(root, file), 'utf8'))
    for (const value of kept) {
      if (!actual.includes(value))
        throw new Error(`${file}: kept BUILD attribute changed: ${value}`)
    }
  }
  return { from, to, oldImport, newImport, touchedGo }
}

if (isMainModule(import.meta.url)) {
  const { values } = parseArgs({ options: { from: { type: 'string' }, to: { type: 'string' } } })
  console.log(JSON.stringify(moveGoPackage(values)))
}
