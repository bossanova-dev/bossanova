#!/usr/bin/env node

// Fenced commands only: prose, inline code and toolbox sources remain outside this guard.
import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { isMainModule } from '../../skills-toolbox/main-module.mjs'
import { PUBLISHED_SKILLS } from '../skills/vendor-toolbox.mjs'
import { extractFencedBlocks, SHELL_INFO_STRINGS } from './check-skill-shell.mjs'

export const LOCATOR_PATTERNS = [
  { name: 'tilde home', pattern: /~\/(?:\.(?:claude|codex)\/skills|\.gemini\/config\/skills)\b/ },
  {
    name: 'variable home',
    pattern:
      /(?:\$HOME|\$\{HOME\}|"\$HOME")\/(?:\.(?:claude|codex)\/skills|\.gemini\/config\/skills)\b/,
  },
  { name: 'shared skills home', pattern: /(?<![A-Za-z0-9_])BOSS_SKILLS_HOME(?![A-Za-z0-9_])/ },
  { name: 'unpinned skill directory', pattern: /<skill-dir>\// },
]

/** @typedef {{file: string, line: number, message: string}} Violation */
/** @typedef {{violations: Violation[], acceptedMarkers: number}} Result */
/** @typedef {{fs?: typeof fs}} Dependencies */

/** @param {string} body */
export function findLocatorLines(body) {
  return body.split(/\r?\n/).flatMap((line, offset) => {
    if (line.trimStart().startsWith('#')) return []
    const match = LOCATOR_PATTERNS.find(({ pattern }) => pattern.test(line))
    return match ? [{ line: offset + 1, form: match.name }] : []
  })
}

/** @param {string} relPath @param {string} text @returns {Result} */
export function checkMarkdown(relPath, text) {
  const lines = text.split(/\r?\n/)
  const blocks = extractFencedBlocks(text).filter(
    (block) => block.info === '' || SHELL_INFO_STRINGS.has(block.info),
  )
  const byLine = new Map(blocks.map((block) => [block.startLine, block]))
  /** @type {Map<number, {line: number, reason: string}>} */
  const markers = new Map()
  /** @type {Violation[]} */
  const violations = []
  const report = (/** @type {number} */ line, /** @type {string} */ message) =>
    violations.push({ file: relPath, line, message })

  for (let index = 0; index < lines.length; index++) {
    const marker = lines[index].replace(/^[\s>]+/, '').match(/^<!--\s*pre-pin:\s*(.*?)\s*-->$/)
    if (!marker) continue
    let next = index + 1
    while (next < lines.length && lines[next].trim() === '') next++
    if (!marker[1]) report(index + 1, 'empty pre-pin marker reason')
    if (!byLine.has(next + 1)) report(index + 1, 'orphan pre-pin marker: expected a shell fence')
    else markers.set(next + 1, { line: index + 1, reason: marker[1] })
  }

  let acceptedMarkers = 0
  for (const block of blocks) {
    const locators = findLocatorLines(block.body)
    const marker = markers.get(block.startLine)
    if (marker) {
      if (locators.length === 0) report(marker.line, 'stale pre-pin marker: no shared-tree locator')
      else if (marker.reason) acceptedMarkers++
    } else if (locators.length > 0) {
      const first = locators[0]
      report(
        block.startLine + first.line,
        `shared-tree locator (${first.form}) outside a pre-pin block`,
      )
    }
  }
  return { violations, acceptedMarkers }
}

/** @param {string} repoRoot @param {Dependencies} [deps] @returns {string[]} */
export function findPublishedCoreMarkdown(repoRoot, deps = {}) {
  const fsImpl = deps.fs || fs
  /** @type {string[]} */
  const files = []
  const walk = (/** @type {string} */ dir) => {
    for (const entry of fsImpl.readdirSync(dir, { withFileTypes: true })) {
      const file = path.join(dir, entry.name)
      if (entry.isDirectory() && entry.name !== 'toolbox') walk(file)
      else if (entry.isFile() && entry.name.endsWith('.md')) files.push(file)
    }
  }
  for (const core of PUBLISHED_SKILLS) {
    const dir = path.join(repoRoot, 'lib/bossalib/contract/plugin/skillpayload/skills', core)
    if (fsImpl.existsSync(dir)) walk(dir)
  }
  return files.sort()
}

/** @param {string} repoRoot @param {Dependencies} [deps] @returns {Result} */
export function checkSkillPinLocatorsInRepo(repoRoot, deps = {}) {
  const fsImpl = deps.fs || fs
  const results = findPublishedCoreMarkdown(repoRoot, deps).map((file) =>
    checkMarkdown(path.relative(repoRoot, file), fsImpl.readFileSync(file, 'utf8')),
  )
  return {
    violations: results.flatMap((result) => result.violations),
    acceptedMarkers: results.reduce((count, result) => count + result.acceptedMarkers, 0),
  }
}

function main() {
  const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..')
  const result = checkSkillPinLocatorsInRepo(root)
  if (result.violations.length > 0) {
    for (const v of result.violations) console.error(`${v.file}:${v.line}: ${v.message}`)
    console.error(
      "After a core's pre-pin block, resolve the toolbox from the run's pin (boss-skill-pins/<core>, " +
        'or <PIN-DIR> for boss-plan). If this block really runs before the pin exists (preflight, ' +
        'or a scheduler-time cron gate), put <!-- pre-pin: <reason> --> on the line before its fence.',
    )
    process.exitCode = 1
  } else console.log(`Skill pin locators OK (${result.acceptedMarkers} pre-pin blocks accepted).`)
}

if (isMainModule(import.meta.url)) main()
