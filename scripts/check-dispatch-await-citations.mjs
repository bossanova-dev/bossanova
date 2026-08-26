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
    if (cells.length !== header.size) continue
    const id = cells[header.get('id')]
    if (!id || id.startsWith('---')) continue
    rows.push({
      id,
      file: cells[header.get('file')]?.replace(/^`|`$/g, '') ?? '',
      verdict: cells[header.get('verdict')] ?? '',
      citation: cells[header.get('await citation')] ?? '',
      parallelOutPath: cells[header.get('parallel out-path')] ?? '',
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
