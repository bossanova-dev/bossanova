#!/usr/bin/env node
// Two-sided ratchet for intra-module Go token clones reported by the debt-dupl tool.
import { spawnSync } from 'node:child_process'
import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { parseArgs } from 'node:util'
import { discoverModules } from './lint-affected.mjs'
import { isMainModule } from '../skills-toolbox/main-module.mjs'

const BASELINE = 'scripts/go-dupl-baseline.json'
const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
const sorted = (object) =>
  Object.fromEntries(Object.entries(object).sort(([a], [b]) => a.localeCompare(b, 'en')))

export function duplPinOf(text) {
  return /^DUPL_PKG\s*:=\s*(\S+)\s*$/m.exec(text)?.[1] ?? null
}
export function readDuplPin(repoRoot) {
  const text = fs.readFileSync(path.join(repoRoot, 'Makefile'), 'utf8')
  const pin = duplPinOf(text)
  if (!pin || !/^github\.com\/mibk\/dupl@v\d+\.\d+\.\d+(?:-[\w.-]+)?$/.test(pin))
    throw new Error('DUPL_PKG must be pinned to an explicit version in Makefile')
  return pin
}

export function parseDuplReport(text) {
  const groups = []
  let active = null,
    expected = 0,
    trailerCount = null
  const fail = () => {
    throw new Error('dupl produced no parseable report (unrecognised or incomplete reporter shape)')
  }
  const finish = () => {
    if (active && active.length !== expected) fail()
    active = null
  }
  for (const raw of String(text).split('\n')) {
    const line = raw.trim()
    if (!line) continue
    let match
    if ((match = /^found (\d+) clones:$/.exec(line))) {
      if (trailerCount !== null) fail()
      finish()
      expected = Number(match[1])
      if (expected < 2) fail()
      active = []
      groups.push(active)
    } else if ((match = /^(.+\.go):(\d+),(\d+)$/.exec(line))) {
      if (!active || trailerCount !== null) fail()
      const file = path.posix.normalize(match[1].replaceAll('\\', '/'))
      if (path.posix.isAbsolute(file) || file === '..' || file.startsWith('../')) fail()
      const start = Number(match[2]),
        end = Number(match[3])
      if (start < 1 || end < start) fail()
      active.push({ file, start, end })
    } else if ((match = /^Found total (\d+) clone groups\.$/.exec(line))) {
      if (trailerCount !== null) fail()
      finish()
      trailerCount = Number(match[1])
    } else fail()
  }
  finish()
  if (trailerCount === null || trailerCount !== groups.length) fail()
  return { groups, trailerCount }
}

/**
 * @param {{file: string, start: number, end: number}[][]} groups
 * @param {{isGenerated?: (file: string) => boolean}} [options]
 */
function pairMembers(groups, { isGenerated = () => false } = {}) {
  const pairs = new Map()
  for (const group of groups) {
    const members = group.filter((m) => !m.file.split('/').includes('gen') && !isGenerated(m.file))
    for (let i = 0; i < members.length; i++)
      for (let j = i + 1; j < members.length; j++) {
        const a = members[i],
          b = members[j]
        if (a.file.endsWith('_test.go') && b.file.endsWith('_test.go')) continue
        const key = [a.file, b.file].sort().join('|')
        if (!pairs.has(key)) pairs.set(key, [])
        pairs.get(key).push([a, b])
      }
  }
  return pairs
}
export function pairCounts(groups, options) {
  return sorted(
    Object.fromEntries(
      [...pairMembers(groups, options)].map(([key, pairs]) => [key, pairs.length]),
    ),
  )
}
export function compareToBaseline(current, baseline) {
  const newPairs = [],
    stalePairs = []
  for (const pair of [...new Set([...Object.keys(current), ...Object.keys(baseline)])].sort()) {
    const measured = current[pair] ?? 0,
      baselined = baseline[pair] ?? 0
    if (measured > baselined) newPairs.push({ pair, measured, baselined })
    if (measured < baselined) stalePairs.push({ pair, measured, baselined })
  }
  return { newPairs, stalePairs }
}

export function readBaseline(repoRoot) {
  const baseline = JSON.parse(fs.readFileSync(path.join(repoRoot, BASELINE), 'utf8'))
  if (
    !Number.isSafeInteger(baseline.threshold) ||
    baseline.threshold < 1 ||
    !baseline.modules ||
    typeof baseline.modules !== 'object' ||
    Array.isArray(baseline.modules)
  )
    throw new Error(
      'invalid Go dupl baseline: positive integer threshold and modules object required',
    )
  for (const counts of Object.values(baseline.modules)) {
    if (
      !counts ||
      typeof counts !== 'object' ||
      Array.isArray(counts) ||
      Object.entries(counts).some(
        ([pair, count]) =>
          !/^.+\.go\|.+\.go$/.test(pair) || !Number.isSafeInteger(count) || count < 1,
      )
    )
      throw new Error('invalid Go dupl baseline: pair counts must be positive integers')
  }
  return baseline
}
function defaultRunDupl({ cwd, pin, threshold }) {
  return spawnSync('go', ['run', pin, '-t', String(threshold), '.'], {
    cwd,
    encoding: 'utf8',
    maxBuffer: 64 * 1024 * 1024,
  })
}

/**
 * @param {{repoRoot?: string, moduleDir: string, runDupl?: typeof defaultRunDupl,
 *   baseline?: ReturnType<typeof readBaseline>, compare?: boolean}} options
 */
export function checkModule({
  repoRoot = ROOT,
  moduleDir,
  runDupl = defaultRunDupl,
  baseline,
  compare = true,
}) {
  try {
    baseline ??= readBaseline(repoRoot)
    const pin = readDuplPin(repoRoot)
    const result = runDupl({
      cwd: path.join(repoRoot, moduleDir),
      pin,
      threshold: baseline.threshold,
    })
    if (result.error || result.status !== 0)
      throw new Error(
        `dupl failed (exit ${result.status ?? 'unknown'}): ${result.stderr ?? ''} ${result.stdout ?? ''} ${result.error?.message ?? ''}`.trim(),
      )
    const { groups } = parseDuplReport(result.stdout)
    const generated = new Map()
    const options = {
      isGenerated: (file) => {
        if (!generated.has(file)) {
          // Go's generated marker belongs before the first non-comment/non-blank line.
          const source = fs.readFileSync(path.join(repoRoot, moduleDir, file), 'utf8')
          const header = source.split(/^package\s/m)[0]
          generated.set(file, /^\/\/ Code generated .* DO NOT EDIT\.$/m.test(header))
        }
        return generated.get(file)
      },
    }
    // Missing reporter member files are a broken detector result, never a clean scan.
    const counts = pairCounts(groups, options)
    const { newPairs, stalePairs } = compareToBaseline(
      counts,
      compare ? (baseline.modules[moduleDir] ?? {}) : counts,
    )
    const remedy = `node scripts/check-go-dupl.mjs --module ${moduleDir} --write-baseline`
    const locations = pairMembers(groups, options)
    const lines = [...newPairs, ...stalePairs].map(({ pair, measured, baselined }) => {
      const ranges = (locations.get(pair) ?? [])
        .flat()
        .map((m) => `${m.file}:${m.start},${m.end}`)
        .join(' ')
      return `Go dupl ${moduleDir}: ${pair}: measured ${measured} ${measured > baselined ? '>' : '<'} baselined ${baselined}${ranges ? `; ${ranges}` : ''}; regenerate: ${remedy} (renames appear as stale plus new)`
    })
    return { ok: lines.length === 0, counts, lines }
  } catch (error) {
    return { ok: false, counts: null, lines: [`Go dupl ${moduleDir}: ${error.message}`] }
  }
}

export function main(
  argv = process.argv.slice(2),
  { repoRoot = ROOT, runDupl = defaultRunDupl, log = console.log } = {},
) {
  try {
    const { values } = parseArgs({
      args: argv,
      options: { module: { type: 'string' }, 'write-baseline': { type: 'boolean' } },
      strict: true,
    })
    const modules = discoverModules(repoRoot)
    if (values.module && !modules.includes(values.module))
      throw new Error(`unknown Go module: ${values.module}`)
    const selected = values.module ? [values.module] : modules
    let baseline
    try {
      baseline = readBaseline(repoRoot)
    } catch (error) {
      if (values['write-baseline'] && error.code === 'ENOENT')
        baseline = { threshold: 100, modules: {} }
      else throw error
    }
    for (const moduleDir of selected) {
      const result = checkModule({
        repoRoot,
        moduleDir,
        baseline,
        runDupl,
        compare: !values['write-baseline'],
      })
      for (const line of result.lines) log(line)
      if (!result.ok) return 1
      if (values['write-baseline']) baseline.modules[moduleDir] = result.counts
      log(`Go dupl ${moduleDir}: passed (${Object.keys(result.counts).length} pairs)`)
    }
    if (values['write-baseline']) {
      baseline.modules = sorted(
        Object.fromEntries(
          Object.entries(baseline.modules).map(([mod, counts]) => [mod, sorted(counts)]),
        ),
      )
      fs.writeFileSync(path.join(repoRoot, BASELINE), JSON.stringify(baseline, null, 2) + '\n')
    }
    return 0
  } catch (error) {
    log(error.message)
    return 1
  }
}
if (isMainModule(import.meta.url)) process.exitCode = main()
