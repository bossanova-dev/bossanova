#!/usr/bin/env node
// guard-id: bossalib-consumers
import fs from 'node:fs'
import path from 'node:path'
import { spawnSync } from 'node:child_process'
import { fileURLToPath } from 'node:url'
import { parseArgs } from 'node:util'
import { isMainModule } from '../../skills-toolbox/main-module.mjs'
import { readBaseline, splitBaselineEntry, compareBaseline } from './baseline.mjs'

const SHARED = 'github.com/recurser/bossalib'
const BASELINE = 'scripts/guards/bossalib-consumers-baseline.json'
const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..')
/** @typedef {{Path: string, Dir: string}} GoModule */
/** @typedef {{ImportPath: string, Dir?: string, Imports?: string[], TestImports?: string[], XTestImports?: string[], Error?: {Err: string}, DepsErrors?: {Err: string}[]}} GoPackage */

/**
 * Count external workspace modules reaching each shared package through production imports.
 * Test imports supply consumers only for packages with no production importers anywhere.
 * @param {GoPackage[]} listing
 * @param {GoModule[]} modules
 * @param {string[]} baseline
 */
export function consumersByPackage(listing, modules, baseline = []) {
  const library = modules.find((module) => module.Path === SHARED)
  if (!library) throw new Error(`workspace has no ${SHARED} module`)
  const root = path.resolve(library.Dir, '../..')
  const graph = new Map(listing.map((pkg) => [pkg.ImportPath, pkg.Imports ?? []]))
  const imported = new Set(listing.flatMap((pkg) => pkg.Imports ?? []))
  /** @type {Map<string, Set<string>>} */
  const production = new Map()
  /** @type {Map<string, Set<string>>} */
  const tests = new Map()
  const isShared = (/** @type {string} */ name) => name === SHARED || name.startsWith(`${SHARED}/`)
  /** @param {string[]} imports @param {string} consumer @param {Map<string, Set<string>>} counts */
  function visit(imports, consumer, counts) {
    const seen = new Set()
    const pending = [...imports]
    while (pending.length) {
      const name = pending.pop()
      if (!isShared(name) || seen.has(name)) continue
      seen.add(name)
      if (!counts.has(name)) counts.set(name, new Set())
      counts.get(name).add(consumer)
      pending.push(...(graph.get(name) ?? []))
    }
  }
  for (const pkg of listing) {
    const module = modules
      .filter((item) => pkg.ImportPath === item.Path || pkg.ImportPath.startsWith(`${item.Path}/`))
      .sort((a, b) => b.Path.length - a.Path.length)[0]
    if (!module || module.Path === SHARED) continue
    const consumer = path.relative(root, module.Dir).split(path.sep).join('/')
    visit(pkg.Imports ?? [], consumer, production)
    visit([...(pkg.TestImports ?? []), ...(pkg.XTestImports ?? [])], consumer, tests)
  }
  const packages = listing
    .filter(
      (pkg) =>
        isShared(pkg.ImportPath) &&
        pkg.ImportPath !== `${SHARED}/gen` &&
        !pkg.ImportPath.startsWith(`${SHARED}/gen/`),
    )
    .map((pkg) => {
      const testSupport = !imported.has(pkg.ImportPath)
      return {
        package: pkg.ImportPath.replace(SHARED, 'lib/bossalib'),
        consumers: [...((testSupport ? tests : production).get(pkg.ImportPath) ?? [])].sort(),
        testSupport,
      }
    })
    .sort((a, b) => a.package.localeCompare(b.package))
  const offenders = packages.filter((pkg) => pkg.consumers.length < 2)
  const result = compareBaseline({
    baselinePath: BASELINE,
    entries: baseline.map((entry) => splitBaselineEntry(entry).key),
    offenders: offenders.map((pkg) => pkg.package),
    remedy: (name) => {
      const pkg = offenders.find((entry) => entry.package === name)
      if (!pkg.consumers.length) return 'imported by no module; delete it'
      const module = pkg.consumers[0]
      return `imported by 1 module (${module}); move it into ${module}/internal/${name.slice('lib/bossalib/'.length)}`
    },
  })
  return { packages, violations: result.failures }
}

/** Decode the concatenated JSON objects emitted by go list without altering quoted strings.
 * @param {string} text
 * @returns {GoPackage[]}
 */
export function parseListing(text) {
  const listing = []
  let start = 0,
    depth = 0,
    quoted = false,
    escaped = false
  for (let i = 0; i < text.length; i++) {
    const char = text[i]
    if (quoted) {
      if (escaped) escaped = false
      else if (char === '\\') escaped = true
      else if (char === '"') quoted = false
    } else if (char === '"') quoted = true
    else if (char === '{') {
      if (depth++ === 0) start = i
    } else if (char === '}' && --depth === 0) listing.push(JSON.parse(text.slice(start, i + 1)))
    else if (depth === 0 && !/\s/.test(char)) throw new Error('invalid go list output')
  }
  if (depth || quoted || !listing.length) throw new Error('incomplete or empty go list output')
  for (const pkg of listing) {
    if (!pkg.ImportPath) throw new Error('go list package has no import path')
    if (pkg.Error || pkg.DepsErrors?.length)
      throw new Error(pkg.Error?.Err ?? pkg.DepsErrors[0].Err)
  }
  return listing
}

const DEFAULT_FIELDS = 'ImportPath,Imports,TestImports,XTestImports,Module,Dir,Error,DepsErrors'

/** List packages in every module named by the root's Go workspace.
 * @param {string} root
 * @param {string} [fields]
 * @returns {{listing: GoPackage[], modules: GoModule[]}}
 */
export function listWorkspacePackages(root, fields = DEFAULT_FIELDS) {
  /** @param {string[]} args */
  const go = (args) => {
    const result = spawnSync('go', args, {
      cwd: root,
      encoding: 'utf8',
      maxBuffer: 64 * 1024 * 1024,
    })
    if (result.error || result.status !== 0)
      throw new Error(result.error?.message ?? result.stderr ?? 'go failed')
    return result.stdout
  }
  const workspace = JSON.parse(go(['work', 'edit', '-json']))
  if (!workspace.Use?.length) throw new Error('go workspace contains no modules')
  const workspaceFile =
    process.env.GOWORK && process.env.GOWORK !== 'auto' && process.env.GOWORK !== 'off'
      ? process.env.GOWORK
      : path.join(root, 'go.work')
  /** @type {GoModule[]} */
  const modules = workspace.Use.map((/** @type {{DiskPath: string}} */ item) => {
    const dir = path.resolve(path.dirname(workspaceFile), item.DiskPath)
    const content = fs.readFileSync(path.join(dir, 'go.mod'), 'utf8')
    const match = /^module\s+(?:"([^"]+)"|(\S+))/m.exec(content)
    if (!match) throw new Error(`cannot read module path in ${dir}/go.mod`)
    return { Path: match[1] ?? match[2], Dir: dir }
  })
  const args = ['list', '-e', `-json=${fields}`, ...modules.map((module) => `${module.Dir}/...`)]
  return { listing: parseListing(go(args)), modules }
}

/** @param {string} root */
export function checkBossalibConsumers(root) {
  const { listing, modules } = listWorkspacePackages(root)
  console.log(
    `bossalib-consumers: go list -e -json=${DEFAULT_FIELDS} ${modules.map((module) => `${module.Dir}/...`).join(' ')}`,
  )
  const baseline = readBaseline(root, BASELINE)
  const result = consumersByPackage(listing, modules, baseline.entries)
  return { ...result, violations: [...baseline.errors, ...result.violations] }
}

if (isMainModule(import.meta.url)) {
  try {
    const { values } = parseArgs({
      options: { root: { type: 'string', default: ROOT } },
      strict: true,
    })
    const result = checkBossalibConsumers(path.resolve(values.root))
    for (const violation of result.violations) console.error(violation)
    if (!result.violations.length)
      console.log(`Bossalib consumers OK (${result.packages.length} packages)`)
    process.exitCode = result.violations.length ? 1 : 0
  } catch (error) {
    console.error(`bossalib-consumers: ${error.message}`)
    process.exitCode = 2
  }
}
