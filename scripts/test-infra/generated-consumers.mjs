// BOS-2298: route generated bossalib inputs to the Go test packages that link them.
//
// Every Go module imports the one generated package, so module-level narrowing cannot shrink a
// proto change's selection. This resolver narrows at package level instead: it reads the
// workspace's test import graph once and keeps the test packages whose test build depends on a
// changed generated package. Any doubt -- a failed `go list`, an unreadable listing, a
// non-empty generated set that maps nothing -- selects every Go module whole.

import { execFileSync } from 'node:child_process'
import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const REPO_ROOT = path.dirname(fileURLToPath(new URL('../../Makefile', import.meta.url)))
const GEN_ROOT = 'lib/bossalib/gen/'
const GEN_IMPORT_ROOT = 'github.com/recurser/bossalib/gen'
// Go's own "this package and everything below it" pattern.
const ALL_GENERATED = `${GEN_IMPORT_ROOT}/...`

/**
 * @typedef {{ root: string, target: string }} ModuleRule
 * @typedef {{ dir: string, forTest?: string, deps: string[] }} TestPackage
 * @typedef {(roots: string[]) => TestPackage[]} PackageLister
 * @typedef {(packages: string[], options: { modules: ModuleRule[] }) => Map<string, string[]> | null} ConsumerResolver
 */

/**
 * Maps changed repo-relative paths to the generated Go import paths they can alter. A file in a
 * generated package directory maps to that package; a `.proto` maps to every generated package,
 * since one proto feeds several of them and its `go_package` is not parsed.
 *
 * @param {string[]} files
 * @returns {string[]}
 */
export function changedGeneratedPackages(files) {
  const packages = new Set()
  for (const file of files) {
    if (file.startsWith('proto/') && file.endsWith('.proto')) {
      packages.add(ALL_GENERATED)
    } else if (file.startsWith(GEN_ROOT)) {
      packages.add(`${GEN_IMPORT_ROOT}/${path.posix.dirname(file.slice(GEN_ROOT.length))}`)
    }
  }
  return [...packages].map((pkg) => pkg.replace(/\/\.$/, '')).sort()
}

/**
 * Returns, per module root, the sorted module-relative test package dirs (`.` for the root)
 * whose test build depends on any of `packages`, or null when the graph cannot be read.
 *
 * @param {string[]} packages
 * @param {{ modules: ModuleRule[], listPackages?: PackageLister }} options
 * @returns {Map<string, string[]> | null}
 */
export function resolveGeneratedConsumers(packages, { modules, listPackages = listTestPackages }) {
  try {
    const roots = modules.map(({ root }) => root)
    const listed = listPackages(roots)
    if (!Array.isArray(listed)) return null
    /** @type {Map<string, Set<string>>} */
    const consumers = new Map()
    for (const { dir, forTest, deps } of listed) {
      const linked = [forTest ?? '', ...deps].some((dep) => matchesAny(dep, packages))
      const root = roots.find((candidate) => `${dir}/`.startsWith(candidate))
      if (!linked || root === undefined) continue
      const relative = `${dir}/`.slice(root.length).replace(/\/$/, '')
      const dirs = consumers.get(root) ?? new Set()
      dirs.add(relative === '' ? '.' : `./${relative}`)
      consumers.set(root, dirs)
    }
    return new Map([...consumers].map(([root, dirs]) => [root, [...dirs].sort()]))
  } catch {
    return null
  }
}

/**
 * Narrows the generated inputs in `files` to make selections: package-scoped consumers, or
 * every module whole when the resolver fails or maps nothing.
 *
 * @param {string[]} files normalized repo-relative paths
 * @param {ModuleRule[]} modules
 * @param {ConsumerResolver} [resolve]
 * @returns {{ whole: string[], packages: Array<[string, string]> }}
 */
export function generatedConsumerTargets(files, modules, resolve = resolveGeneratedConsumers) {
  const changed = changedGeneratedPackages(files)
  if (changed.length === 0) return { whole: [], packages: [] }
  let consumers = null
  try {
    consumers = resolve(changed, { modules })
  } catch {
    consumers = null
  }
  /** @type {Array<[string, string]>} */
  const packages = []
  for (const { root, target } of modules) {
    for (const dir of consumers?.get(root) ?? []) packages.push([target, dir])
  }
  if (packages.length === 0) return { whole: modules.map(({ target }) => target), packages: [] }
  return { whole: [], packages }
}

/** @type {Map<string, TestPackage[]>} */
const listingCache = new Map()

/**
 * Lists every test variant under `roots` with its test build's transitive deps, through one
 * workspace-wide `go list -test` from `repoRoot`. Memoised per process. Throws on failure;
 * `-e` is deliberately absent so a broken package fails the listing instead of hiding deps.
 *
 * @param {string[]} roots repo-relative module roots, each ending in `/`
 * @param {{ repoRoot?: string }} [options]
 * @returns {TestPackage[]}
 */
export function listTestPackages(roots, { repoRoot = REPO_ROOT } = {}) {
  const patterns = roots.map((root) => `./${root.replace(/\/+$/, '')}/...`)
  const key = `${repoRoot}\0${patterns.join(' ')}`
  const cached = listingCache.get(key)
  if (cached) return cached
  const output = execFileSync(
    'go',
    [
      'list',
      '-test',
      '-f',
      '{{if .ForTest}}{{.ForTest}}\t{{.Dir}}\t{{join .Deps " "}}{{end}}',
      ...patterns,
    ],
    {
      cwd: repoRoot,
      encoding: 'utf8',
      maxBuffer: 512 * 1024 * 1024,
      timeout: 600_000,
      stdio: ['ignore', 'pipe', 'pipe'],
    },
  )
  // go list reports symlink-resolved dirs (macOS /var is /private/var).
  const realRoot = fs.realpathSync(repoRoot)
  const listed = output
    .split(/\r?\n/)
    .filter(Boolean)
    .map((line) => {
      const [forTest, dir, deps = ''] = line.split('\t')
      return {
        forTest,
        dir: path.relative(realRoot, dir).split(path.sep).join('/'),
        deps: deps.split(' ').filter(Boolean),
      }
    })
  listingCache.set(key, listed)
  return listed
}

/**
 * @param {string} dep a `go list` import path, possibly carrying a ` [pkg.test]` variant suffix
 * @param {string[]} packages import paths or `<path>/...` patterns
 */
function matchesAny(dep, packages) {
  const importPath = dep.replace(/ \[.*\]$/, '')
  return packages.some((pkg) =>
    pkg.endsWith('/...')
      ? importPath === pkg.slice(0, -4) || importPath.startsWith(pkg.slice(0, -3))
      : importPath === pkg,
  )
}
