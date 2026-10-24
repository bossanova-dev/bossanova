#!/usr/bin/env node
import fs from 'node:fs'
import path from 'node:path'
import { parseArgs } from 'node:util'
import { isMainModule } from '../../skills-toolbox/main-module.mjs'
import { scanGoTests, REPO_ROOT } from './go-test-scan.mjs'
import record from './registry/shared-test-fakes.mjs'
/** @param {string} [root] */
export function scanFakes(root = REPO_ROOT) {
  const declarations = scanGoTests(root, record).fakes
  /** @type {Map<string, Set<string>>} */
  const dirs = new Map()
  for (const item of declarations) {
    if (!dirs.has(item.name)) dirs.set(item.name, new Set())
    dirs.get(item.name).add(item.dir)
  }
  const counts = Object.fromEntries(
    [...dirs]
      .filter(([, dirs]) => dirs.size > 1)
      .sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0))
      .map(([name, dirs]) => [name, dirs.size]),
  )
  return { counts, declarations }
}

/** @param {string} root @param {string} dir */
function moduleFor(root, dir) {
  let current = dir
  while (true) {
    if (fs.existsSync(path.join(root, current, 'go.mod'))) return current
    const parent = path.posix.dirname(current)
    if (parent === current) return null
    current = parent
  }
}

/** @param {string} root @param {string} name @param {string[]} dirs */
export function suggestedPackage(root, name, dirs) {
  const modules = dirs.map((dir) => moduleFor(root, dir))
  if (modules[0] !== null && modules.every((module) => module === modules[0])) {
    let common = dirs[0].split('/')
    for (const dir of dirs.slice(1)) {
      const parts = dir.split('/')
      const mismatch = common.findIndex((part, i) => part !== parts[i])
      if (mismatch >= 0) common = common.slice(0, mismatch)
    }
    const dir = common.join('/') || '.'
    return path.posix.join(dir, `${path.posix.basename(dir === '.' ? root : dir)}test`)
  }
  return `lib/bossalib/${name.replace(/^(fake|stub|mock)/, '').toLowerCase()}test`
}
/** @param {Record<string, number>} counts @param {unknown} baseline */
export function compareBaseline(counts, baseline) {
  const failures = []
  if (!baseline || Array.isArray(baseline) || typeof baseline !== 'object') {
    return [{ kind: 'invalid-baseline', message: `${record.baseline} must be a count map` }]
  }
  const entries = Object.entries(baseline)
  const keys = entries.map(([key]) => key)
  if (keys.some((key, i) => key !== [...keys].sort()[i])) {
    failures.push({ kind: 'invalid-baseline', message: `${record.baseline} must stay sorted` })
  }
  for (const [key, count] of entries) {
    if (!Number.isSafeInteger(count) || count <= 1) {
      failures.push({ kind: 'invalid-baseline', message: `${key}: invalid baseline count` })
    } else if ((counts[key] ?? 0) < count) {
      failures.push({
        kind: 'stale-entry',
        message: `${key}: ${counts[key] ?? 0} remaining, baseline ${count}; lower the entry (remove it at one)`,
      })
    }
  }
  for (const [key, count] of Object.entries(counts)) {
    if (count > (baseline[key] ?? 0)) {
      failures.push({
        kind: 'new-instance',
        message: `${key}: ${count} exceeds baseline ${baseline[key] ?? 0}`,
        key,
      })
    }
  }
  return failures
}

if (isMainModule(import.meta.url)) {
  const { values } = parseArgs({
    options: { 'write-baseline': { type: 'boolean', default: false } },
    strict: true,
  })
  try {
    const result = scanFakes()
    const baselinePath = path.join(REPO_ROOT, record.baseline)
    if (values['write-baseline'])
      fs.writeFileSync(baselinePath, `${JSON.stringify(result.counts, null, 2)}\n`)
    const failures = compareBaseline(
      result.counts,
      JSON.parse(fs.readFileSync(baselinePath, 'utf8')),
    )
    for (const failure of failures) {
      console.error(failure.message)
      if (failure.kind === 'new-instance') {
        const declarations = result.declarations.filter((item) => item.name === failure.key)
        console.error(declarations.map((item) => `${item.file}:${item.line}`).join('\n'))
        console.error(
          `Share in ${suggestedPackage(
            REPO_ROOT,
            failure.key,
            declarations.map((item) => item.dir),
          )}`,
        )
      }
    }
    if (failures.length) process.exitCode = 1
    else console.log('Shared test fakes OK')
  } catch (error) {
    console.error(error.message)
    process.exitCode = 1
  }
}
