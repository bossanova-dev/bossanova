#!/usr/bin/env node
import { execFileSync } from 'node:child_process'
import fs from 'node:fs'
import path from 'node:path'
import { parseArgs } from 'node:util'
import { isMainModule } from '../../skills-toolbox/main-module.mjs'
import { trackedFiles, textLines } from '../dev/tree-scan-lib.mjs'
import { readBaseline, splitBaselineEntry } from '../guards/baseline.mjs'
import { readMoves, replacePathReferences } from './move-ledger.mjs'

const ROOT = path.resolve(import.meta.dirname, '../..')
const LEDGER = 'scripts/layout/docs-moves.json'

/** @param {string} file */
function generatorFor(file) {
  if (file.startsWith('.codex/skills/')) return 'make codex-skills'
  if (/^(lib\/bossalib\/gen|services\/web\/src\/gen|services\/docs\/openapi)\//.test(file))
    return 'make generate'
  return null
}

/** Preserve the resolved target when a Markdown file or its target moves.
 * @param {string} text
 * @param {string} before
 * @param {string} after
 * @param {Map<string, string>} mappings
 */
function rewriteLinks(text, before, after, mappings) {
  return text.replace(
    /(!?\[[^\]\n]*\]\()([^\s)]+)([^)\n]*\))/g,
    (match, prefix, target, suffix) => {
      if (/^(?:[a-z][a-z\d+.-]*:|#|\/)/i.test(target) || /[<>*$?{}]/.test(target)) return match
      const [, name, anchor = ''] = target.match(/^([^#]*)(#.*)?$/)
      const resolved = path.posix.normalize(path.posix.join(path.posix.dirname(before), name))
      const destination = mappings.get(resolved) ?? resolved
      if (before === after && destination === resolved) return match
      let relative = path.posix.relative(path.posix.dirname(after), destination)
      if (name.endsWith('/')) relative += '/'
      return `${prefix}${relative}${anchor}${suffix}`
    },
  )
}

/** Move tracked documentation and repair its references.
 * @param {{root?: string, moves: string[][], dryRun?: boolean}} options
 */
export function moveDocs({ root = ROOT, moves, dryRun = false }) {
  const tracked = trackedFiles(root)
  const existing = new Set(tracked)
  const allowlist = readBaseline(root, 'scripts/layout/docs-allowlist.json')
  const ledger = readMoves(root, LEDGER)
  if (allowlist.errors.length || ledger.errors.length)
    throw new Error([...allowlist.errors, ...ledger.errors].join('\n'))
  const allowed = new Set(allowlist.entries.map((entry) => splitBaselineEntry(entry).key))
  const pending = []
  const sources = new Set()
  const targets = new Set()
  for (const [from, to] of moves) {
    if (
      !existing.has(from) &&
      existing.has(to) &&
      ledger.pairs.some(([a, b]) => a === from && b === to)
    )
      continue
    if (
      !/^docs\/.+/.test(from) ||
      !existing.has(from) ||
      !fs.lstatSync(path.join(root, from)).isFile()
    )
      throw new Error(`${from}: source must be a tracked docs file`)
    if (
      !/^docs\/[^/]+\/.+/.test(to) ||
      path.posix.normalize(to) !== to ||
      !allowed.has(to.split('/').slice(0, 2).join('/'))
    )
      throw new Error(`${to}: destination directory must be in scripts/layout/docs-allowlist.json`)
    if (
      existing.has(to) ||
      fs.existsSync(path.join(root, to)) ||
      sources.has(from) ||
      targets.has(to)
    )
      throw new Error(`${from} -> ${to}: duplicate source or taken destination`)
    sources.add(from)
    targets.add(to)
    pending.push([from, to])
  }
  const mappings = new Map(pending.map(([from, to]) => [from, to]))
  const edits = new Map()
  const generators = new Set()
  for (const file of tracked) {
    if (file === LEDGER || !fs.lstatSync(path.join(root, file)).isFile()) continue
    const lines = textLines(file, root)
    if (!lines) continue
    const original = lines.join('\n')
    const destination = mappings.get(file) ?? file
    let updated = original
    if (/\.mdx?$/.test(file)) updated = rewriteLinks(updated, file, destination, mappings)
    for (const [from, to] of pending) updated = replacePathReferences(updated, from, to)
    if (updated === original) continue
    const generator = generatorFor(file)
    if (generator) generators.add(generator)
    else edits.set(destination, updated)
  }
  if (!dryRun && pending.length) {
    for (const [from, to] of pending) {
      fs.mkdirSync(path.dirname(path.join(root, to)), { recursive: true })
      execFileSync('git', ['mv', '--', from, to], { cwd: root })
    }
    for (const [file, text] of edits) fs.writeFileSync(path.join(root, file), text)
    fs.writeFileSync(
      path.join(root, LEDGER),
      JSON.stringify([...ledger.pairs, ...pending], null, 2) + '\n',
    )
  }
  return { moves: pending, files: [...edits.keys()], generators: [...generators].sort() }
}

if (isMainModule(import.meta.url)) {
  const { tokens, values } = parseArgs({
    strict: true,
    tokens: true,
    options: {
      root: { type: 'string', default: ROOT },
      from: { type: 'string', multiple: true },
      to: { type: 'string', multiple: true },
      'dry-run': { type: 'boolean', default: false },
    },
  })
  const moves = []
  let from
  for (const token of tokens) {
    if (token.kind !== 'option') continue
    if (token.name === 'from') {
      if (from !== undefined) throw new Error('Each --from must be followed by --to')
      from = token.value
    } else if (token.name === 'to') {
      if (from === undefined) throw new Error('--to needs a preceding --from')
      moves.push([from, token.value])
      from = undefined
    }
  }
  if (from !== undefined || !moves.length)
    throw new Error('Provide repeated --from <path> --to <path> pairs')
  const result = moveDocs({ root: values.root, moves, dryRun: values['dry-run'] })
  console.log(JSON.stringify(result, null, 2))
  for (const generator of result.generators) console.log(`Regenerate: ${generator}`)
}
