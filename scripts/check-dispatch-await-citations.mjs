#!/usr/bin/env node

import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { isMainModule } from '../skills-toolbox/main-module.mjs'

const REPO_ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
export const AUDIT_PATH = 'docs/skills/dispatch-graph-audit.md'
export const REQUIRED_CITATION = 'toolbox/bs-dispatch-await.mjs'

function splitRow(line) {
  return line
    .trim()
    .slice(1, -1)
    .split('|')
    .map((cell) => cell.trim())
}

export function auditRows(markdown) {
  const rows = []
  for (const line of markdown.split(/\r?\n/)) {
    if (!line.startsWith('|')) continue
    const cells = splitRow(line)
    if (cells.length !== 7) continue
    if (cells[0] === 'id' || cells[0].startsWith('---')) continue
    rows.push({
      id: cells[0],
      file: cells[1].replace(/^`|`$/g, ''),
      citation: cells[5],
    })
  }
  return rows
}

export function checkDispatchAwaitCitations({ root = REPO_ROOT } = {}) {
  const audit = fs.readFileSync(path.join(root, AUDIT_PATH), 'utf8')
  const rows = auditRows(audit)
  const failures = []
  for (const row of rows) {
    if (row.citation !== 'required') continue
    const fullPath = path.join(root, row.file)
    let prose
    try {
      prose = fs.readFileSync(fullPath, 'utf8')
    } catch {
      failures.push(`${row.id}: missing file ${row.file}`)
      continue
    }
    if (!prose.includes(REQUIRED_CITATION)) {
      failures.push(`${row.id}: ${row.file} does not cite ${REQUIRED_CITATION}`)
    }
  }
  if (rows.length === 0) failures.push(`${AUDIT_PATH}: no audit rows parsed`)
  return { ok: failures.length === 0, failures, rows }
}

if (isMainModule(import.meta.url)) {
  const result = checkDispatchAwaitCitations()
  if (!result.ok) {
    process.stderr.write(result.failures.join('\n') + '\n')
    process.exit(1)
  }
  process.stdout.write(`Verified ${result.rows.length} dispatch await audit rows\n`)
}
