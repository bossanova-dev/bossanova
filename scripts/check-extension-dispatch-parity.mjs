#!/usr/bin/env node

import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { AUDIT_PATH } from './check-dispatch-await-citations.mjs'
import { isMainModule } from '../skills-toolbox/main-module.mjs'

const REPO_ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')

const CORE_FILES = new Set([
  'services/boss/internal/skillinstall/skills/boss-build/SKILL.md',
  'services/boss/internal/skillinstall/skills/boss-plan/SKILL.md',
  'services/boss/internal/skillinstall/skills/boss-review/SKILL.md',
  'services/boss/internal/skillinstall/skills/boss-repair/SKILL.md',
  'services/boss/internal/skillinstall/skills/boss-epic/SKILL.md',
])

const CLAUDE_TERMS = [/\bTask\b/, /\bsubagent_type\b/, /\bgeneral-purpose\b/]
const CODEX_TERMS = [/\bspawn_agent\b/, /\bwait_agent\b/]

function splitRow(line) {
  return line
    .trim()
    .slice(1, -1)
    .split('|')
    .map((cell) => cell.trim())
}

export function extensionDispatchRows(markdown) {
  const rows = []
  let header = null
  for (const line of markdown.split(/\r?\n/)) {
    if (!line.startsWith('|')) continue
    const cells = splitRow(line)
    if (cells.every((cell) => /^-+$/.test(cell))) continue
    if (!header) {
      if (!cells.includes('id')) continue
      header = new Map(cells.map((cell, index) => [cell, index]))
      continue
    }
    const id = cells[header.get('id')]
    if (!id || id.startsWith('---')) continue
    const file = cells[header.get('file')]?.replace(/^`|`$/g, '') ?? ''
    if (cells[header.get('await citation')] !== 'required' || !CORE_FILES.has(file)) continue
    rows.push({
      id,
      file,
      phase: cells[header.get('phase')],
      dispatchShape: cells[header.get('dispatch shape')],
    })
  }
  return rows
}

function hasAny(text, patterns) {
  return patterns.some((pattern) => pattern.test(text))
}

export function checkExtensionDispatchParity({ root = REPO_ROOT } = {}) {
  const audit = fs.readFileSync(path.join(root, AUDIT_PATH), 'utf8')
  const rows = extensionDispatchRows(audit)
  const failures = []
  const checkedFiles = new Set()
  for (const row of rows) {
    if (checkedFiles.has(row.file)) continue
    checkedFiles.add(row.file)
    const fullPath = path.join(root, row.file)
    let prose
    try {
      prose = fs.readFileSync(fullPath, 'utf8')
    } catch {
      failures.push(`${row.id}: missing file ${row.file}`)
      continue
    }
    const namesClaude = hasAny(prose, CLAUDE_TERMS)
    const namesCodex = hasAny(prose, CODEX_TERMS)
    if (namesClaude !== namesCodex) {
      failures.push(
        `${row.file}: extension dispatch prose names ${namesClaude ? 'Claude' : 'Codex'} mechanism without the other`,
      )
    }
  }
  if (rows.length === 0) failures.push(`${AUDIT_PATH}: no extension dispatch rows parsed`)
  return { ok: failures.length === 0, failures, rows }
}

if (isMainModule(import.meta.url)) {
  const result = checkExtensionDispatchParity()
  if (!result.ok) {
    process.stderr.write(result.failures.join('\n') + '\n')
    process.exit(1)
  }
  process.stdout.write(`Verified ${result.rows.length} extension dispatch parity rows\n`)
}
