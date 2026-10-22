import { mkdtempSync, readdirSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { dirname, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { parseArgs } from 'node:util'
import { spawnSync } from 'node:child_process'
import { isMainModule } from '../skills-toolbox/main-module.mjs'
import { TEMP_NAMES } from './recover-disk-space.mjs'
import { compareBaseline, readBaseline, splitBaselineEntry } from './guards/baseline.mjs'

/** @param {string} name */
export function prefixKey(name) {
  if (TEMP_NAMES[0].test(name)) return name.slice(0, -6)
  if (TEMP_NAMES[1].test(name)) return name.replace(/\d+$/, '')
  return name
}

/** @param {string[]} entries */
export function classifyLeftovers(entries) {
  /** @type {Map<string, number>} */
  const counts = new Map()
  for (const entry of entries) {
    const key = prefixKey(entry)
    counts.set(key, (counts.get(key) ?? 0) + 1)
  }
  return counts
}

/**
 * @param {string[]} command
 * @param {{repoRoot?: string, baselinePath?: string, write?: (line: string) => void}} [options]
 */
export function run(
  command,
  {
    repoRoot = resolve(dirname(fileURLToPath(import.meta.url)), '..'),
    baselinePath = 'scripts/test-tmp-leaks-baseline.json',
    write = (line) => process.stderr.write(`${line}\n`),
  } = {},
) {
  if (command.length === 0) throw new Error('usage: check-test-tmp-leaks.mjs -- <command…>')
  const root = mkdtempSync(join(tmpdir(), 'tl-'))
  try {
    const child = spawnSync(command[0], command.slice(1), {
      stdio: 'inherit',
      env: { ...process.env, TMPDIR: root, TMP: root, TEMP: root, NODE_DISABLE_COMPILE_CACHE: '1' },
    })
    const counts = classifyLeftovers(readdirSync(root))
    const baseline = readBaseline(repoRoot, baselinePath)
    const comparison = compareBaseline({
      baselinePath,
      entries: baseline.entries.map((entry) => splitBaselineEntry(entry).key),
      offenders: counts.keys(),
      remedy: (prefix) =>
        `${counts.get(prefix)} leftover(s); use scripts/test-tmpdir.mjs tempDir(prefix, t) or clean production scratch in finally`,
    })
    for (const error of [...baseline.errors, ...comparison.failures]) {
      write(`test-tmp-leaks: ${error}`)
    }
    if (child.error) write(`test-tmp-leaks: ${child.error.message}`)
    if (child.signal) write(`test-tmp-leaks: command killed by ${child.signal}`)
    if (baseline.errors.length === 0 && comparison.failures.length === 0) {
      write(
        `test-tmp-leaks: passed (${counts.size} baselined prefixes, ${readdirSync(root).length} leftovers)`,
      )
    }
    return child.status !== 0
      ? (child.status ?? 1)
      : baseline.errors.length || comparison.failures.length
        ? 1
        : 0
  } finally {
    rmSync(root, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 })
  }
}

if (isMainModule(import.meta.url)) {
  const { positionals } = parseArgs({ strict: true, allowPositionals: true })
  process.exitCode = run(positionals)
}
