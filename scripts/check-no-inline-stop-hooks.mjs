#!/usr/bin/env node

import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

// The inline Stop-hook filter must live ONLY in
// scripts/remove-bossd-stop-hooks.mjs, never copied into a SKILL.md. This
// signature matches the executable filter line, not prose that merely mentions
// the hooks or names the shared script.
const INLINE_SIGNATURE = /startsWith\s*\(\s*(['"])bossd-agent-run-\1\s*\)/
const SKILL_ROOTS = [
  '.claude/skills',
  '.codex/skills',
  'plugins/bossd-plugin-claude/skilldata/skills',
  'services/boss/internal/skillinstall/skills',
]

export function fileHasInlineStopHook(contents) {
  return INLINE_SIGNATURE.test(contents)
}

export function findSkillFiles(root, deps = {}) {
  const fsImpl = deps.fs || fs
  const results = []
  const walk = (dir) => {
    for (const entry of fsImpl.readdirSync(dir, { withFileTypes: true })) {
      const full = path.join(dir, entry.name)
      if (entry.isDirectory()) walk(full)
      else if (entry.name === 'SKILL.md') results.push(full)
    }
  }
  walk(root)
  return results
}

export function findInlineStopHookCopies(skillRoot, deps = {}) {
  const fsImpl = deps.fs || fs
  return findSkillFiles(skillRoot, deps).filter((file) =>
    fileHasInlineStopHook(fsImpl.readFileSync(file, 'utf8')),
  )
}

export function findInlineStopHookCopiesInRepo(repoRoot, deps = {}) {
  const fsImpl = deps.fs || fs
  return SKILL_ROOTS.map((d) => path.join(repoRoot, d))
    .filter((d) => fsImpl.existsSync(d))
    .flatMap((root) => findInlineStopHookCopies(root, deps))
}

function main() {
  const repoRoot = path.join(path.dirname(fileURLToPath(import.meta.url)), '..')
  const offenders = findInlineStopHookCopiesInRepo(repoRoot)
  if (offenders.length > 0) {
    console.error(
      'Inline bossd Stop-hook filter found — call scripts/remove-bossd-stop-hooks.mjs instead:',
    )
    for (const file of offenders) console.error(`  - ${path.relative(repoRoot, file)}`)
    process.exit(1)
  }
  console.log('No inline bossd Stop-hook copies (single source of truth OK).')
}

export function isInvokedDirectly(argvPath, moduleUrl, deps = {}) {
  const fsImpl = deps.fs || fs
  if (!argvPath || !fsImpl.existsSync(argvPath)) return false
  return fsImpl.realpathSync(argvPath) === fsImpl.realpathSync(fileURLToPath(moduleUrl))
}

const invokedDirectly = isInvokedDirectly(process.argv[1], import.meta.url)

if (invokedDirectly) main()
