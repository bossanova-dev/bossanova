#!/usr/bin/env node
import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { execFileSync } from 'node:child_process'
import { parseArgs } from 'node:util'

import { isMainModule } from '../../skills-toolbox/main-module.mjs'
import { checkBudget, readBudgets } from './budgets.mjs'
import { scannedFiles } from './registry.mjs'
import record from './registry/part-guides.mjs'

const REPO_ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..')

/** @param {{repoRoot?: string}} [input] */
export function checkPartGuides({ repoRoot = REPO_ROOT } = {}) {
  const files = scannedFiles(repoRoot, record.scans).filter(
    (file) => file.includes('/') && /\/(AGENTS|CLAUDE)\.md$/.test(file),
  )
  const modes = new Map(
    execFileSync('git', ['ls-files', '--stage', '-z'], { cwd: repoRoot, encoding: 'utf8' })
      .split('\0')
      .filter(Boolean)
      .map((entry) => {
        const tab = entry.indexOf('\t')
        return [entry.slice(tab + 1), entry.slice(0, 6)]
      }),
  )
  const { budgets, errors } = readBudgets(repoRoot)
  const failures = [...errors]
  const measured = new Map()
  /** @param {string} file */
  const stat = (file) => {
    try {
      return fs.lstatSync(path.join(repoRoot, file))
    } catch (error) {
      if (error.code !== 'ENOENT') throw error
      return null
    }
  }
  for (const file of files) {
    if (path.basename(file) === 'CLAUDE.md') {
      const guide = `${path.posix.dirname(file)}/AGENTS.md`
      if (!modes.has(guide) || !stat(guide)) {
        failures.push(`${file}: no sibling AGENTS.md tracked and present`)
      }
      continue
    }
    const guideStat = stat(file)
    if (!guideStat) failures.push(`${file}: missing regular file`)
    else if (!guideStat.isFile() || !/^100[0-7]{3}$/.test(modes.get(file) ?? '')) {
      failures.push(`${file}: must be a regular file in the worktree and git index`)
    } else measured.set(file, guideStat.size)
    const sibling = `${path.posix.dirname(file)}/CLAUDE.md`
    const siblingStat = stat(sibling)
    if (!siblingStat) failures.push(`${sibling}: missing sibling symlink`)
    else if (!siblingStat.isSymbolicLink()) failures.push(`${sibling}: must be a symlink`)
    else if (fs.readlinkSync(path.join(repoRoot, sibling)) !== 'AGENTS.md') {
      failures.push(`${sibling}: link target must be exactly AGENTS.md`)
    }
    if (modes.get(sibling) !== '120000') {
      failures.push(`${sibling}: tracked mode must be 120000`)
    }
  }
  failures.push(...checkBudget({ budgets, guardId: record.id, measured }).failures)
  return { failures, filesScanned: files.length }
}

if (isMainModule(import.meta.url)) {
  const { values } = parseArgs({
    options: { root: { type: 'string' } },
    allowPositionals: false,
    strict: true,
  })
  const { failures, filesScanned } = checkPartGuides({ repoRoot: values.root })
  console.log(`Part guides: ${filesScanned} files scanned`)
  for (const failure of failures) console.error(failure)
  if (failures.length > 0) process.exitCode = 1
}
