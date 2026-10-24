#!/usr/bin/env node

import fs from 'node:fs'
import path from 'node:path'
import { execFileSync } from 'node:child_process'
import { parseArgs } from 'node:util'
import { fileURLToPath } from 'node:url'
import { isMainModule } from '../../skills-toolbox/main-module.mjs'
import { stripCommentsAndStrings } from './check-timing-bounds.mjs'

const REPO_ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..')
const BASELINE = 'scripts/guards/fixture-rm-retries-baseline.json'
const REMEDY = 'use { recursive: true, force: true, maxRetries: 5, retryDelay: 100 }'
const OPEN = { '(': ')', '[': ']', '{': '}' }

// Split only at the current depth: paths, options and callbacks can all contain nested calls.
function argumentsAt(source, start, closer) {
  const parts = []
  const stack = []
  let from = start
  for (let i = start; i < source.length; i++) {
    const ch = source[i]
    if (!stack.length && (ch === ',' || ch === closer)) {
      parts.push(source.slice(from, i).trim())
      if (ch === closer) return { parts, end: i }
      from = i + 1
    } else if (OPEN[ch]) stack.push(OPEN[ch])
    else if (ch === stack.at(-1)) stack.pop()
  }
  return null
}

export function analyzeSource(contents) {
  // Preserve supported quoted property names without exposing string contents as code.
  const normalized = contents.replace(
    /(['"])(recursive|maxRetries)\1(?=\s*:)/g,
    (_, quote, key) => ` ${key} `,
  )
  const source = stripCommentsAndStrings(normalized)
  const calls =
    /(?<![\w$.])(?:(?:fs(?:\s*\.\s*promises)?|fsp|promises)\s*\.\s*)?(?:rmSync|rm|rmdirSync)\s*\(/g
  const violations = []
  for (const match of source.matchAll(calls)) {
    const args = argumentsAt(source, match.index + match[0].length, ')')
    const options = args?.parts[1]
    if (!options) continue
    const object = options.startsWith('{') ? argumentsAt(options, 1, '}') : null
    const properties = object?.end === options.length - 1 ? object.parts : null
    // Identifiers and spreads hide whether recursion/retries are enabled, so fail closed.
    const undecidable = !properties || properties.some((property) => property.startsWith('...'))
    if (
      undecidable ||
      (properties.some((property) => /^recursive\s*:\s*true\s*$/.test(property)) &&
        !properties.some((property) => /^maxRetries\s*:/.test(property)))
    ) {
      violations.push({ line: source.slice(0, match.index).split('\n').length })
    }
  }
  return violations
}

export function compareBaseline(counts, baseline) {
  const failures = []
  if (!baseline || Array.isArray(baseline) || typeof baseline !== 'object') {
    return [{ kind: 'invalid-baseline', message: `${BASELINE} must be a path-to-count object` }]
  }
  const keys = Object.keys(baseline)
  const sorted = [...keys].sort()
  if (keys.some((key, index) => key !== sorted[index])) {
    failures.push({ kind: 'invalid-baseline', message: `${BASELINE} must stay sorted` })
  }
  for (const [file, count] of Object.entries(baseline)) {
    if (!Number.isSafeInteger(count) || count <= 0) {
      failures.push({
        kind: 'invalid-baseline',
        file,
        message: `${file}: baseline count must be a positive integer`,
      })
    } else if ((counts[file] ?? 0) < count) {
      failures.push({
        kind: 'stale-entry',
        file,
        message: `${file}: ${counts[file] ?? 0} remaining, baseline ${count}; lower the entry (remove it at zero)`,
      })
    }
  }
  for (const [file, count] of Object.entries(counts)) {
    if (count > (baseline[file] ?? 0)) {
      failures.push({
        kind: 'new-instance',
        file,
        message: `${file}: ${count} un-retried deletes exceed baseline ${baseline[file] ?? 0}; ${REMEDY}`,
      })
    }
  }
  return failures
}

export function scanFixtures(repoRoot = REPO_ROOT) {
  const files = execFileSync('git', ['ls-files', '-z', '--', '*.test.mjs'], {
    cwd: repoRoot,
    encoding: 'utf8',
  })
    .split('\0')
    .filter((file) => file && !file.startsWith('.codex/'))
    .sort()
  const counts = {}
  for (const file of files) {
    const count = analyzeSource(fs.readFileSync(path.join(repoRoot, file), 'utf8')).length
    if (count) counts[file] = count
  }
  return counts
}

if (isMainModule(import.meta.url)) {
  const { values } = parseArgs({
    options: { 'write-baseline': { type: 'boolean', default: false } },
    strict: true,
  })
  const counts = scanFixtures()
  const baselinePath = path.join(REPO_ROOT, BASELINE)
  if (values['write-baseline'])
    fs.writeFileSync(baselinePath, `${JSON.stringify(counts, null, 2)}\n`)
  const failures = compareBaseline(counts, JSON.parse(fs.readFileSync(baselinePath, 'utf8')))
  if (failures.length) {
    for (const failure of failures) console.error(failure.message)
    process.exitCode = 1
  } else {
    console.log(
      `Fixture rm retries OK (${Object.values(counts).reduce((sum, count) => sum + count, 0)} baselined instances in ${Object.keys(counts).length} files)`,
    )
  }
}
