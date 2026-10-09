#!/usr/bin/env node

// Early copy of TestPublishedCoresAreProjectAgnostic, applied where the leak is written. The Go
// test scans the shipped payloads, but it is bazel-`manual`, so `make test` never selects it; an
// agent editing skills-toolbox/*.mjs only learns that a `BOS-123` comment leaks into a published
// core once CI or review reports it, then has to reword and re-vendor (#2649, #2788). This gate
// reads the rule list out of skills_manifest_test.go — one source of truth, no copy to drift —
// and applies it to the canonical toolbox sources VENDOR_MAP ships into a published core, plus
// the published core trees themselves.

import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

import { isMainModule } from '../skills-toolbox/main-module.mjs'
import { PUBLISHED_SKILLS, VENDOR_MAP } from './vendor-toolbox.mjs'

const REPO_ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
const RULES_FILE = path.join(
  'services',
  'boss',
  'internal',
  'skillinstall',
  'skills_manifest_test.go',
)
const PAYLOAD_ROOT = path.join('services', 'boss', 'internal', 'skillinstall', 'skills')
const TOOLBOX_ROOT = 'skills-toolbox'

// Reads the `forbiddenIdentity` regexps from the Go test. Go's RE2 syntax used there (\b, classes,
// `?`) means the same thing in JavaScript.
export function readIdentityRules(goSource) {
  const start = goSource.indexOf('var forbiddenIdentity = []identityRule{')
  const end = start === -1 ? -1 : goSource.indexOf('\n}\n', start)
  if (end === -1) throw new Error(`${RULES_FILE}: forbiddenIdentity block not found`)
  const block = goSource.slice(start, end)
  const rules = []
  for (const match of block.matchAll(/regexp\.MustCompile\((`[^`]*`|"(?:[^"\\]|\\.)*")\)/g)) {
    const literal = match[1]
    const pattern = literal.startsWith('`') ? literal.slice(1, -1) : JSON.parse(literal)
    rules.push(new RegExp(pattern, 'g'))
  }
  if (rules.length === 0) throw new Error(`${RULES_FILE}: forbiddenIdentity yielded no rules`)
  return rules
}

function walk(dir) {
  return fs.readdirSync(dir, { withFileTypes: true }).flatMap((entry) => {
    const file = path.join(dir, entry.name)
    return entry.isDirectory() ? walk(file) : [file]
  })
}

// Maps each scanned repo-relative file to the published cores it ships into.
export function publishedSources(repoRoot = REPO_ROOT) {
  const sources = new Map()
  const add = (file, core) => sources.set(file, [...(sources.get(file) ?? []), core])
  for (const [core, files] of Object.entries(VENDOR_MAP)) {
    if (!PUBLISHED_SKILLS.has(core)) continue
    for (const file of files) add(path.join(TOOLBOX_ROOT, file), core)
  }
  for (const core of PUBLISHED_SKILLS) {
    const dir = path.join(repoRoot, PAYLOAD_ROOT, core)
    if (!fs.existsSync(dir)) continue
    for (const file of walk(dir)) add(path.relative(repoRoot, file), core)
  }
  return sources
}

export function checkPublishedCoreIdentity({
  repoRoot = REPO_ROOT,
  rules = readIdentityRules(fs.readFileSync(path.join(repoRoot, RULES_FILE), 'utf8')),
  sources = publishedSources(repoRoot),
} = {}) {
  const failures = []
  for (const [file, cores] of [...sources].sort(([a], [b]) => a.localeCompare(b))) {
    const content = fs.readFileSync(path.join(repoRoot, file), 'utf8')
    for (const rule of rules) {
      for (const match of content.matchAll(rule)) {
        const line = content.slice(0, match.index).split('\n').length
        const shipsIn = [...new Set(cores)].join(', ')
        failures.push(`${file}:${line}: "${match[0]}" ships in published ${shipsIn}`)
      }
    }
  }
  return { failures, scanned: sources.size }
}

if (isMainModule(import.meta.url)) {
  const { failures, scanned } = checkPublishedCoreIdentity()
  if (scanned === 0) {
    console.error('Published core identity check scanned no files; it would pass vacuously')
    process.exit(1)
  }
  if (failures.length > 0) {
    console.error('Published core identity check failed:')
    for (const failure of failures) console.error(`  ${failure}`)
    console.error(
      'Remedy: drop the Bossanova identifier (cite the behaviour, not the ticket) or route it ' +
        `through the .boss-skills.json adapter. Rules: forbiddenIdentity in ${RULES_FILE}.`,
    )
    process.exit(1)
  }
  console.log(`Published core identity OK (${scanned} files)`)
}
