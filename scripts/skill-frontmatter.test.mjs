#!/usr/bin/env node

import assert from 'node:assert/strict'
import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { test } from 'node:test'

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')

// The three skill trees this repo owns: the project skills, the published cores,
// and the byte-identical plugin mirror of those cores.
const SKILL_ROOTS = [
  '.claude/skills',
  'services/boss/internal/skillinstall/skills',
  'plugins/bossd-plugin-claude/skilldata/skills',
]

// The cron/manual sweeps that must never be model-invocable. Deliberately an
// explicit list rather than the discovered `bs-sweep-*` directory set: an
// eleventh sweep has to be a considered edit here, not an accident.
const SWEEP_SKILLS = [
  'bs-sweep-debt',
  'bs-sweep-mutation',
  'bs-sweep-notes',
  'bs-sweep-plan',
  'bs-sweep-prettify',
  'bs-sweep-review',
  'bs-sweep-security',
  'bs-sweep-sentry',
  'bs-sweep-tests',
  'bs-sweep-tidy-linear',
]

// Mirrors scripts/sync-codex-skills.mjs: frontmatter is the leading `---` block,
// and a key is a line-anchored `name: value` pair inside it. Parsing the key out
// of the block (rather than grepping the file) is what makes a key accidentally
// swallowed into a `description: |` scalar fail rather than pass.
function readFrontmatter(filePath) {
  const markdown = fs.readFileSync(filePath, 'utf8')
  const match = /^---\r?\n([\s\S]*?)\r?\n---/.exec(markdown)

  assert.ok(match, `${path.relative(repoRoot, filePath)}: missing frontmatter`)

  const keys = new Map()

  for (const line of match[1].split(/\r?\n/)) {
    const keyValue = /^([A-Za-z0-9_-]+):\s*(.*)$/.exec(line)

    if (keyValue) {
      keys.set(keyValue[1], keyValue[2].trim())
    }
  }

  return keys
}

function skillFiles() {
  const files = []

  for (const root of SKILL_ROOTS) {
    const absoluteRoot = path.join(repoRoot, root)

    if (!fs.existsSync(absoluteRoot)) {
      continue
    }

    for (const entry of fs.readdirSync(absoluteRoot, { withFileTypes: true })) {
      const skillPath = path.join(absoluteRoot, entry.name, 'SKILL.md')

      if (entry.isDirectory() && fs.existsSync(skillPath)) {
        files.push({ name: entry.name, path: skillPath, root })
      }
    }
  }

  return files
}

test('every bs-sweep-* skill disables model invocation', () => {
  const sweeps = skillFiles().filter(
    (skill) => skill.root === '.claude/skills' && skill.name.startsWith('bs-sweep-'),
  )

  // Discovered-vs-expected first: an eleventh sweep fails here whether or not it
  // carries the flag, so a new sweep cannot silently leak its description back
  // into the model listing by simply omitting the key.
  const discovered = sweeps.map((skill) => skill.name).sort()

  assert.deepEqual(discovered, [...SWEEP_SKILLS].sort())

  const flagged = sweeps
    .filter((skill) => readFrontmatter(skill.path).get('disable-model-invocation') === 'true')
    .map((skill) => skill.name)
    .sort()

  assert.deepEqual(flagged, discovered)
})

test('no boss skill extension disables model invocation', () => {
  // Extensions are dispatched into fresh subagents by their core, and a failed
  // dispatch is only ever a non-fatal skip — so this regression would be silent.
  const offenders = skillFiles()
    .filter((skill) => {
      const frontmatter = readFrontmatter(skill.path)

      return frontmatter.has('x-boss-extension') && frontmatter.has('disable-model-invocation')
    })
    .map((skill) => `${skill.root}/${skill.name}`)

  assert.deepEqual(offenders, [])
})

test('no skill hardcodes a model identifier in a Co-Authored-By trailer', () => {
  // A negative invariant on purpose: the real trailer is harness-injected and
  // session-specific, so there is no expected literal that stays correct.
  const offenders = []

  for (const skill of skillFiles()) {
    for (const line of fs.readFileSync(skill.path, 'utf8').split(/\r?\n/)) {
      // Case-insensitive so the hyphenated API form (`claude-opus-5`) is caught
      // alongside the display form (`Claude Opus 5`) — `-` is a word boundary.
      if (/Co-Authored-By:/.test(line) && /\b(opus|sonnet|haiku)\b/i.test(line)) {
        offenders.push(`${skill.root}/${skill.name}: ${line.trim()}`)
      }
    }
  }

  assert.deepEqual(offenders, [])
})
