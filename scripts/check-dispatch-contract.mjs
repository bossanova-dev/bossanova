#!/usr/bin/env node

import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

import { isMainModule } from '../skills-toolbox/main-module.mjs'
import { SKILL_ROOTS } from './check-skill-shell.mjs'

const REPO_ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')

export const EXTRA_SCAN_ROOTS = ['CLAUDE.md', 'services/bossd/internal/session/tmux_chat.go']
export const EXCLUDED_PREFIXES = [
  'plugins/bossd-plugin-claude/skilldata/',
  '.codex/',
  'docs/plans/',
]

const RUN_IN_BACKGROUND = /run_in_background/
const DISPATCH_CONTEXT = /\b(?:subagent|Agent|Task|dispatch)\b/i
const AFFIRMATIVE_PARAMETER_CLAIM =
  /\b(?:pass|set|provide|suppl(?:y|ies)|accepts?|exposes?|supports?|signature|parameter|argument|field)\b/i
const PROHIBITION_OR_NEGATION =
  /\b(?:never|forbid(?:s|den)?|must\s+not|does\s+not|do\s+not|no\s+such|no\s+matching|absen(?:t|ce)|without)\b/i

export function resolveScanRoots(repoRoot = REPO_ROOT) {
  return [...SKILL_ROOTS, ...EXTRA_SCAN_ROOTS].map((root) => path.resolve(repoRoot, root))
}

function toRepoRelative(repoRoot, file) {
  return path.relative(repoRoot, file).split(path.sep).join('/')
}

function isExcluded(repoRoot, file) {
  const relative = toRepoRelative(repoRoot, file)
  return EXCLUDED_PREFIXES.some(
    (prefix) => relative === prefix.slice(0, -1) || relative.startsWith(prefix),
  )
}

function* walk(root, repoRoot) {
  if (!fs.existsSync(root) || isExcluded(repoRoot, root)) return
  const stat = fs.statSync(root)
  if (stat.isFile()) {
    yield root
    return
  }
  if (!stat.isDirectory()) return
  for (const entry of fs.readdirSync(root, { withFileTypes: true })) {
    const child = path.join(root, entry.name)
    if (isExcluded(repoRoot, child)) continue
    if (entry.isDirectory()) {
      yield* walk(child, repoRoot)
    } else if (entry.isFile()) {
      yield child
    }
  }
}

export function discoverScanFiles(repoRoot = REPO_ROOT) {
  return resolveScanRoots(repoRoot)
    .flatMap((root) => [...walk(root, repoRoot)])
    .filter((file, index, files) => files.indexOf(file) === index)
}

function lineIsAffirmativeClaim(line, previous = '', next = '') {
  if (!RUN_IN_BACKGROUND.test(line)) return false
  const window = `${previous} ${line} ${next}`
  if (/\bBash\b/.test(window)) return false
  if (!DISPATCH_CONTEXT.test(window)) return false
  if (PROHIBITION_OR_NEGATION.test(line)) return false
  return AFFIRMATIVE_PARAMETER_CLAIM.test(window)
}

export function scanDispatchContractText(source) {
  const lines = source.split(/\r?\n/)
  const findings = []
  for (let index = 0; index < lines.length; index += 1) {
    if (lineIsAffirmativeClaim(lines[index], lines[index - 1] ?? '', lines[index + 1] ?? '')) {
      findings.push({ line: index + 1, text: lines[index].trim() })
    }
  }
  return findings
}

export function checkDispatchContract({
  repoRoot = REPO_ROOT,
  files = discoverScanFiles(repoRoot),
} = {}) {
  const findings = []
  for (const file of files) {
    const source = fs.readFileSync(file, 'utf8')
    for (const finding of scanDispatchContractText(source)) {
      findings.push({ file: toRepoRelative(repoRoot, file), ...finding })
    }
  }
  return findings
}

export function main() {
  const findings = checkDispatchContract()
  if (findings.length === 0) return 0

  console.error(
    'check-dispatch-contract: subagent dispatch must not claim a run_in_background parameter',
  )
  for (const finding of findings) {
    console.error(`${finding.file}:${finding.line}: ${finding.text}`)
  }
  return 1
}

if (isMainModule(import.meta.url)) {
  process.exitCode = main()
}
