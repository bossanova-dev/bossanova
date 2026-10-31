#!/usr/bin/env node

// guard-id: feature-ownership
// guard-id: feature-guides
// guard-id: feature-index-fresh
// guard-id: legacy-criteria-ceiling

import { spawnSync } from 'node:child_process'
import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { parseArgs } from 'node:util'
import * as prettier from 'prettier'
import { isMainModule } from '../../skills-toolbox/main-module.mjs'
import { globToRegExp } from '../../skills-toolbox/skill-config.mjs'
import {
  FATAL_MAP_WARNINGS,
  LEGACY_ID_LIMIT,
  acNumber,
  loadFeatureMap,
  parseFrontmatter,
} from '../../.claude/skills/verify/lib/features.mjs'
import { compareBaseline, formatBaseline, readBaseline } from './baseline.mjs'

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..')
const MAP = 'docs/features'
export const BASELINE = 'scripts/guards/feature-map-baseline.json'
export const LEGACY_CEILING = 'scripts/guards/legacy-criteria-ceiling-baseline.json'
export const BEGIN_MARKER = '<!-- BEGIN GENERATED feature-index -->'
export const END_MARKER = '<!-- END GENERATED feature-index -->'
export const INFRASTRUCTURE_FOLDERS = new Set([
  'db',
  'dbtest',
  'domain',
  'adapter',
  'testharness',
  'fixtures',
  'tuidriver',
  'realstack',
])
const GUIDE_ROOT = 'services/docs/docs/guides/'

function trackedFiles(root) {
  const result = spawnSync('git', ['ls-files', '-z'], {
    cwd: root,
    encoding: 'utf8',
    maxBuffer: 64 * 1024 * 1024,
  })
  if (result.error || result.status !== 0) {
    throw new Error(`git ls-files failed: ${result.error?.message ?? result.stderr}`)
  }
  return result.stdout.split('\0').filter(Boolean)
}

const CEILING_ENTRY = /^([^.\s]+)\.AC([1-9][0-9]*)$/

/**
 * Failures that freeze each feature file's legacy ids (below LEGACY_ID_LIMIT) at its ceiling, the
 * `<feature>.AC<n>` entry in `entries`; a feature with no entry has ceiling 0. An id above the
 * ceiling is `legacy-criterion-added`, and a ticket id in the feature file is
 * `ticket-criterion-in-feature-file`. An entry above the feature's highest legacy id, naming no
 * feature or repeating one is `legacy-ceiling-stale`, so a ceiling follows deletions down.
 */
export function legacyCeilingFailures(map, entries) {
  const failures = []
  const stale = (entry, why) =>
    failures.push(`${LEGACY_CEILING}: legacy-ceiling-stale: ${entry} ${why}`)
  const ceilings = new Map()
  for (const entry of entries) {
    const match = CEILING_ENTRY.exec(entry)
    if (!match || BigInt(match[2]) >= LEGACY_ID_LIMIT)
      stale(entry, 'is not <feature>.AC<n> below AC1000')
    else if (ceilings.has(match[1])) stale(entry, `repeats the ${match[1]} ceiling`)
    else ceilings.set(match[1], { entry, n: BigInt(match[2]) })
  }
  for (const feature of map.features) {
    const entry = ceilings.get(feature.id)
    const ceiling = entry?.n ?? 0n
    let highest = 0n
    for (const criterion of feature.criteria) {
      const n = acNumber(criterion.id)
      if (criterion.file !== feature.file) continue
      if (n >= LEGACY_ID_LIMIT) {
        failures.push(
          `${MAP}/${feature.file}: ticket-criterion-in-feature-file: ${criterion.id} is a ticket id; move it to criteria/${feature.id}/<KEY>-${n / 100n}.md`,
        )
        continue
      }
      if (n > highest) highest = n
      if (n > ceiling)
        failures.push(
          `${MAP}/${feature.file}: legacy-criterion-added: ${criterion.id} is above the ${feature.id} legacy ceiling AC${ceiling} in ${LEGACY_CEILING}; put new criteria in criteria/${feature.id}/<TICKET>.md; run \`control-boss features next-id ${feature.id} --ticket <KEY>-<n>\``,
        )
    }
    ceilings.delete(feature.id)
    if (entry && entry.n > highest)
      stale(
        entry.entry,
        highest === 0n
          ? 'names a feature with no legacy criterion; remove it'
          : `is above the highest ${feature.id} legacy id; lower it to ${feature.id}.AC${highest}`,
      )
  }
  for (const { entry } of ceilings.values()) stale(entry, 'names no feature; remove it')
  return failures
}

/**
 * The feature map and the failures every guard reading it shares: the loader's fatal warnings,
 * then the legacy ceiling (legacyCeilingFailures) once the map loads cleanly.
 */
export function loadCheckedMap(root) {
  const map = loadFeatureMap(path.join(root, MAP))
  const failures = map.warnings
    .filter((w) => FATAL_MAP_WARNINGS.includes(w.code))
    .map((w) => `${MAP}/${w.file}: ${w.code}: ${w.detail}`)
  if (failures.length) return { map, failures }
  const baseline = readBaseline(root, LEGACY_CEILING)
  return { map, failures: [...baseline.errors, ...legacyCeilingFailures(map, baseline.entries)] }
}

const matcher = (globs) => {
  const patterns = globs.map(globToRegExp)
  return (file) => patterns.some((pattern) => pattern.test(file))
}

/** Direct feature folders are inferred from tracked paths, including files outside user-facing roots. */
function featureFolder(file) {
  const match =
    /^(services\/boss\/api\/(?:tui|cli)\/|services\/web\/src\/features\/|services\/[^/]+\/internal\/)([^/]+)\//.exec(
      file,
    )
  if (!match || INFRASTRUCTURE_FOLDERS.has(match[2]) || match[2].endsWith('test')) return null
  return `${match[1]}${match[2]}/`
}

export function ownershipOffenders(map, tracked) {
  const ignored = matcher(map.roots.ignore)
  const userFacing = matcher(map.roots.userFacingRoots)
  const owned = matcher(map.features.flatMap((feature) => feature.owns))
  const folders = new Map()
  const files = []
  for (const file of tracked) {
    if (ignored(file)) continue
    const isOwned = owned(file)
    if (userFacing(file) && !isOwned) files.push(file)
    const folder = featureFolder(file)
    if (folder) folders.set(folder, (folders.get(folder) ?? false) || isOwned)
  }
  return [...files, ...[...folders].filter(([, owned]) => !owned).map(([folder]) => folder)].sort()
}

function ownershipConflicts(map, tracked) {
  const ignored = matcher(map.roots.ignore)
  const owners = map.features.map((feature) => ({ id: feature.id, matches: matcher(feature.owns) }))
  return tracked
    .filter((file) => !ignored(file))
    .flatMap((file) => {
      const ids = owners.filter((owner) => owner.matches(file)).map((owner) => owner.id)
      return ids.length > 1
        ? [`${file} is owned by ${ids.join(' and ')}: keep it in one feature's owns:`]
        : []
    })
}

export function ownership({ root = ROOT, update = false } = {}) {
  const { map, failures } = loadCheckedMap(root)
  if (failures.length) return { ok: false, messages: failures }
  const tracked = trackedFiles(root)
  const conflicts = ownershipConflicts(map, tracked)
  if (conflicts.length) return { ok: false, messages: conflicts }
  const offenders = ownershipOffenders(map, tracked)
  if (update) {
    const file = path.join(root, BASELINE)
    fs.mkdirSync(path.dirname(file), { recursive: true })
    fs.writeFileSync(file, formatBaseline(offenders))
    return { ok: true, messages: [], offenders }
  }
  const baseline = readBaseline(root, BASELINE)
  const comparison = compareBaseline({
    baselinePath: BASELINE,
    entries: baseline.entries,
    offenders,
    remedy:
      'is user-facing and no feature owns it: add a glob to the owns: field of a feature file in docs/features/ (or a new feature file)',
  })
  const messages = [...baseline.errors, ...comparison.failures]
  return { ok: messages.length === 0, messages, offenders }
}

const list = (value) => (value === undefined ? [] : Array.isArray(value) ? value : [value])

function metadata(map, feature) {
  return parseFrontmatter(fs.readFileSync(path.join(map.dir, feature.file), 'utf8')).data
}

export function guides({ root = ROOT } = {}) {
  const { map, failures } = loadCheckedMap(root)
  const tracked = new Set(trackedFiles(root))
  for (const feature of map.features) {
    for (const guide of list(metadata(map, feature).guide)) {
      if (
        typeof guide === 'string' &&
        guide.startsWith(GUIDE_ROOT) &&
        path.posix.normalize(guide) === guide &&
        tracked.has(guide) &&
        fs.existsSync(path.join(root, guide)) &&
        fs.statSync(path.join(root, guide)).isFile()
      )
        continue
      failures.push(
        `${MAP}/${feature.file}: guide: ${guide} does not resolve to a tracked file under ${GUIDE_ROOT}`,
      )
    }
  }
  return { ok: failures.length === 0, messages: failures }
}

export function renderIndex(map) {
  return (
    [...map.features]
      .sort((a, b) => (a.id < b.id ? -1 : a.id > b.id ? 1 : 0))
      .map((feature) => {
        const data = metadata(map, feature)
        const summary = typeof data.summary === 'string' && data.summary ? ` — ${data.summary}` : ''
        const guideLinks = list(data.guide)
          .map((guide) => `[guide](${path.posix.relative(MAP, guide)})`)
          .join(', ')
        return `- [${feature.title}](${feature.file}) (${feature.surfaces.join(', ')})${summary}${guideLinks ? `; ${guideLinks}` : ''}`
      })
      .join('\n') + '\n'
  )
}

export function replaceIndexRegion(text, body) {
  const lines = text.split(/\r?\n/)
  const begins = lines.flatMap((line, i) => (line === BEGIN_MARKER ? [i] : []))
  const ends = lines.flatMap((line, i) => (line === END_MARKER ? [i] : []))
  if (begins.length !== 1 || ends.length !== 1 || begins[0] >= ends[0]) {
    throw new Error(`${MAP}/README.md: generated feature-index markers must occur once, in order`)
  }
  return [...lines.slice(0, begins[0] + 1), '', body.trimEnd(), '', ...lines.slice(ends[0])].join(
    '\n',
  )
}

export async function index({ root = ROOT, write = false } = {}) {
  const { map, failures } = loadCheckedMap(root)
  if (failures.length) return { ok: false, messages: failures }
  const file = path.join(map.dir, 'README.md')
  const actual = fs.readFileSync(file, 'utf8')
  const expected = await prettier.format(replaceIndexRegion(actual, renderIndex(map)), {
    ...(await prettier.resolveConfig(file)),
    parser: 'markdown',
  })
  if (write) {
    if (actual !== expected) fs.writeFileSync(file, expected)
    return { ok: true, messages: [] }
  }
  return actual === expected
    ? { ok: true, messages: [] }
    : {
        ok: false,
        messages: [
          `${MAP}/README.md: generated feature index is stale; run make feature-index-update`,
        ],
      }
}

export async function main(args = process.argv.slice(2)) {
  const { values, positionals } = parseArgs({
    args,
    strict: true,
    allowPositionals: true,
    options: {
      root: { type: 'string', default: ROOT },
      check: { type: 'boolean', default: false },
      update: { type: 'boolean', default: false },
      write: { type: 'boolean', default: false },
    },
  })
  const [verb] = positionals
  const modes = Number(values.check) + Number(values.update) + Number(values.write)
  if (
    positionals.length !== 1 ||
    modes !== 1 ||
    !['ownership', 'guides', 'index'].includes(verb) ||
    (values.update && verb !== 'ownership') ||
    (values.write && verb !== 'index')
  ) {
    throw new Error(
      'usage: feature-map.mjs ownership (--check|--update) | guides --check | index (--check|--write) [--root <path>]',
    )
  }
  const options = { root: path.resolve(values.root), update: values.update, write: values.write }
  const result =
    verb === 'ownership'
      ? ownership(options)
      : verb === 'guides'
        ? guides(options)
        : await index(options)
  for (const message of result.messages) console.error(message)
  return result.ok ? 0 : 1
}

if (isMainModule(import.meta.url)) {
  try {
    process.exitCode = await main()
  } catch (error) {
    console.error(error.message)
    process.exitCode = 1
  }
}
