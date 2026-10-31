// Built-in proof-describe script for the dependencies area: bump levels, added and removed direct
// dependencies, new install or lifecycle scripts and new transitive packages. A major bump or a
// new install script marks the area breaking; new transitive packages are reported, not breaking.

import { spawnSync } from 'node:child_process'
import path from 'node:path'

import { isMainModule } from '../main-module.mjs'
import { readChangedFiles, runTool } from './verify-diff.mjs'
import { describerMain } from './verify-describe.mjs'
import { INSTALL_SCRIPTS, bumpLevel, dependencyChanges } from './verify-areas.mjs'

/**
 * @typedef {import('./verify-diff.mjs').Run} Run
 * @typedef {{name: string, from: string, to: string, level: string, path: string}} LevelledChange
 * @typedef {{packages: Set<string>, installs: Set<string>}} LockfileFacts
 */

const DEPENDENCY_SECTIONS = [
  'dependencies',
  'devDependencies',
  'peerDependencies',
  'optionalDependencies',
]
const MAX_NAMES = 10
const plural = (/** @type {number} */ n, /** @type {string} */ one) =>
  `${n} ${one}${n === 1 ? '' : 's'}`
const LEVELS = ['major', 'minor', 'patch', 'unknown']

/** @param {string} key */
const unquote = (key) =>
  key
    .trim()
    .replace(/^['"]|['"]:?$/g, '')
    .replace(/:$/, '')

/** The package name of a `name@version` spec, keeping a leading scope `@`. @param {string} spec */
function specName(spec) {
  const bare = spec.replace(/^\//, '').replace(/\(.*$/, '')
  const at = bare.indexOf('@', 1)
  return at > 0 ? bare.slice(0, at) : bare
}

/** Lockfile parsers: package names, packages flagged as running an install script, and whether
 * the format records install scripts at all. */
const LOCKFILES = {
  /** @param {string} text */
  'package-lock.json'(text) {
    const facts = { packages: new Set(), installs: new Set() }
    const packages = JSON.parse(text).packages ?? {}
    for (const [key, entry] of Object.entries(packages)) {
      if (!key) continue
      const name =
        entry?.name ?? key.slice(key.lastIndexOf('node_modules/') + 'node_modules/'.length)
      facts.packages.add(name)
      if (entry?.hasInstallScript === true) facts.installs.add(name)
    }
    return facts
  },
  /** @param {string} text */
  'pnpm-lock.yaml'(text) {
    const version = /^lockfileVersion:\s*['"]?(\d+)/m.exec(text)
    // pnpm 9 dropped `requiresBuild`, so its lockfile cannot show install scripts.
    const facts = {
      packages: new Set(),
      installs: new Set(),
      unrecorded: Boolean(version && Number(version[1]) >= 9),
    }
    let section = ''
    let current = ''
    for (const line of text.split('\n')) {
      if (/^\S/.test(line)) section = line.replace(/:.*$/, '')
      if (section !== 'packages') continue
      const key = /^ {2}(\S.*):\s*$/.exec(line)
      if (key) {
        const spec = unquote(key[1])
        // pnpm 5 keys are `/name/version`; later ones are `/name@version` or `name@version`.
        current = /^\/(?:@[^/]+\/)?[^/@]+\/[^/]+$/.test(spec)
          ? spec.slice(1, spec.lastIndexOf('/'))
          : specName(spec)
        facts.packages.add(current)
      } else if (current && /^ {4}requiresBuild:\s*true\s*$/.test(line)) facts.installs.add(current)
    }
    return facts
  },
  /** @param {string} text */
  'yarn.lock'(text) {
    const facts = { packages: new Set(), installs: new Set() }
    for (const line of text.split('\n')) {
      if (!/^[^\s#].*:\s*$/.test(line) || line.startsWith('__metadata')) continue
      facts.packages.add(specName(unquote(line.split(',')[0])))
    }
    return facts
  },
  /** @param {string} text */
  'go.sum'(text) {
    const packages = new Set(
      text
        .split('\n')
        .map((line) => line.split(/\s+/)[0])
        .filter(Boolean),
    )
    return { packages, installs: new Set() }
  },
  /** @param {string} text */
  'Cargo.lock'(text) {
    const packages = new Set()
    let inPackage = false
    for (const line of text.split('\n')) {
      if (/^\[/.test(line)) inPackage = line.trim() === '[[package]]'
      const name = inPackage && /^name\s*=\s*"([^"]+)"/.exec(line)
      if (name) packages.add(name[1])
    }
    return { packages, installs: new Set() }
  },
}
LOCKFILES['go.work.sum'] = LOCKFILES['go.sum']
LOCKFILES['poetry.lock'] = LOCKFILES['Cargo.lock']
LOCKFILES['uv.lock'] = LOCKFILES['Cargo.lock']
const OTHER_LOCKFILES = /^(?:bun\.lockb|Gemfile\.lock|composer\.lock)$/

/** @param {string[]} names */
function nameList(names) {
  const shown = names.slice(0, MAX_NAMES).join(', ')
  return names.length > MAX_NAMES ? `${shown} (+${names.length - MAX_NAMES} more)` : shown
}

/**
 * @param {{area: {paths: string[]}, baseSha: string, headSha: string}} envelope
 * @param {{cwd?: string, run?: Run}} [options]
 */
export function describeDependencies(envelope, { cwd, run = spawnSync } = {}) {
  const { baseSha, headSha } = envelope
  const files = new Map(
    readChangedFiles({ baseSha, headSha, cwd, run }).map((file) => [file.path, file]),
  )
  /** @param {string} sha @param {string} file @returns {string | null} */
  const show = (sha, file) => {
    const out = runTool(run, 'git', ['show', `${sha}:${file}`], { cwd })
    return out.ok ? out.stdout : null
  }
  const mergeBase = runTool(run, 'git', ['merge-base', baseSha, headSha], { cwd })
  const before = mergeBase.ok ? mergeBase.stdout.trim() : baseSha
  /** @type {LevelledChange[]} */
  const changes = []
  const added = new Set()
  const removed = new Set()
  const scripts = []
  const newInstalls = new Set()
  const newPackages = new Set()
  const unrecordedInstalls = []
  const notInspected = []
  for (const file of [...new Set(envelope.area.paths)].sort()) {
    if (!files.has(file)) continue
    const name = path.posix.basename(file)
    if (name === 'package.json') {
      const read = (/** @type {string} */ sha) => {
        const text = show(sha, file)
        if (text === null) return {}
        try {
          return JSON.parse(text)
        } catch {
          return null
        }
      }
      const [old, next] = [read(before), read(headSha)]
      if (!old || !next) {
        notInspected.push(`Manifest ${file} not inspected`)
        continue
      }
      for (const section of DEPENDENCY_SECTIONS) {
        const a = old[section] ?? {}
        const b = next[section] ?? {}
        for (const [dep, to] of Object.entries(b)) {
          if (!(dep in a)) added.add(dep)
          else if (a[dep] !== to)
            changes.push({ name: dep, from: a[dep], to, level: bumpLevel(a[dep], to), path: file })
        }
        for (const dep of Object.keys(a)) if (!(dep in b)) removed.add(dep)
      }
      for (const script of INSTALL_SCRIPTS) {
        const to = next.scripts?.[script]
        if (typeof to === 'string' && to !== old.scripts?.[script])
          scripts.push(
            `${old.scripts?.[script] === undefined ? 'New' : 'Changed'} ${script} script in ${file}`,
          )
      }
      continue
    }
    const parse = LOCKFILES[/** @type {'go.sum'} */ (name)]
    if (parse) {
      const facts = (/** @type {string} */ sha) => {
        const text = show(sha, file)
        return text === null ? { packages: new Set(), installs: new Set() } : parse(text)
      }
      const [old, next] = [facts(before), facts(headSha)]
      const fresh = [...next.packages].filter((pkg) => !old.packages.has(pkg))
      for (const pkg of fresh) newPackages.add(pkg)
      for (const pkg of next.installs) if (!old.installs.has(pkg)) newInstalls.add(pkg)
      if ('unrecorded' in next && next.unrecorded && fresh.length > 0)
        unrecordedInstalls.push(
          `Install scripts of new packages unknown: ${file} does not record them`,
        )
      continue
    }
    if (OTHER_LOCKFILES.test(name)) {
      notInspected.push(`Lockfile ${file} not inspected`)
      continue
    }
    const manifest = dependencyChanges(file, files.get(file)?.patch)
    if (!manifest.readable) notInspected.push(`Manifest ${file} not inspected`)
    for (const change of manifest.changes)
      changes.push({ ...change, level: bumpLevel(change.from, change.to), path: file })
    for (const dep of manifest.added) added.add(dep)
    for (const dep of manifest.removed) removed.add(dep)
  }
  const counts = LEVELS.map((level) => [level, changes.filter((c) => c.level === level).length])
    .filter(([, n]) => n)
    .map(([level, n]) => `${n} ${level}`)
  const order = (/** @type {LevelledChange} */ c) => LEVELS.indexOf(c.level)
  const items = [
    ...(counts.length ? [counts.join(', ')] : []),
    ...scripts,
    ...(newInstalls.size
      ? [
          `${plural(newInstalls.size, 'package')} newly running install scripts: ${nameList([...newInstalls].sort())}`,
        ]
      : []),
    ...changes
      .sort((a, b) => order(a) - order(b) || a.name.localeCompare(b.name))
      .filter((c) => c.level !== 'patch')
      .map((c) => `${c.name} ${c.from} → ${c.to} (${c.level})`),
    ...(added.size ? [`Added: ${nameList([...added].sort())}`] : []),
    ...(removed.size ? [`Removed: ${nameList([...removed].sort())}`] : []),
    ...(newPackages.size
      ? [
          `${plural(newPackages.size, 'new transitive package')}: ${nameList([...newPackages].sort())}`,
        ]
      : []),
    ...unrecordedInstalls,
    ...notInspected,
  ]
  return {
    items,
    breaking:
      changes.some((c) => c.level === 'major') || scripts.length > 0 || newInstalls.size > 0,
  }
}

export const main = () => describerMain(describeDependencies)

if (isMainModule(import.meta.url)) await main()
