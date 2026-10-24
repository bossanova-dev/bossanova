import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { parseArgs } from 'node:util'
import { isMainModule } from '../../skills-toolbox/main-module.mjs'
import { compareBaseline, formatBaseline, readBaseline, splitBaselineEntry } from './baseline.mjs'
import { parseExceptions, isExcepted } from './exceptions.mjs'
import { commentsOf, commentFiles } from './source-comments.mjs'
import record from './registry/todo-tickets.mjs'

const REPO_ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..')

export function check({ root = REPO_ROOT, writeBaseline = false } = {}) {
  const offenders = new Set()
  const errors = []

  for (const file of commentFiles(root, record.scans)) {
    const text = fs.readFileSync(path.join(root, file), 'utf8')
    if (!text.includes('TODO')) continue
    const comments = commentsOf(text, path.extname(file))
    const commentLines = Array(text.split('\n').length).fill('')
    for (const comment of comments) {
      comment.text.split('\n').forEach((line, offset) => {
        commentLines[comment.line - 1 + offset] += line
      })
    }
    const exceptions = parseExceptions(commentLines.join('\n'), { guardId: record.id })
    errors.push(...exceptions.errors.map((error) => `${file}: ${error}`))
    for (const comment of comments) {
      if (isExcepted(exceptions, comment.line)) continue
      const normalized = comment.text
        .replace(/^\/\*+|\*\/$/g, '')
        .split('\n')
        .map((line) => line.replace(/^\s*(?:\/\/+|\*)?\s*/, ''))
        .join('\n')
        .trim()
      for (const section of normalized.split(/\n\s*\n/)) {
        if (/^TODO\b/.test(section) && !/\bBOS-\d+\b/.test(section)) {
          offenders.add(`${file}: ${section.split('\n')[0].replace(/\s+/g, ' ').trim()}`)
        }
      }
    }
  }
  if (writeBaseline) {
    fs.writeFileSync(path.join(root, record.baseline), formatBaseline(offenders))
    return { failures: errors, offenders: [...offenders].sort(), baselined: offenders.size }
  }
  const baseline = readBaseline(root, record.baseline)
  const compared = compareBaseline({
    baselinePath: record.baseline,
    entries: baseline.entries.map((entry) => splitBaselineEntry(entry).key),
    offenders,
    remedy:
      'file a ticket and write TODO(BOS-n), or delete the TODO; in skills-toolbox move the TODO to the tracker',
  })
  return {
    failures: [...errors, ...baseline.errors, ...compared.failures],
    offenders: [...offenders].sort(),
    baselined: baseline.entries.length,
  }
}

if (isMainModule(import.meta.url)) {
  const { values } = parseArgs({
    strict: true,
    options: {
      root: { type: 'string', default: REPO_ROOT },
      'write-baseline': { type: 'boolean', default: false },
    },
  })
  const result = check({ root: values.root, writeBaseline: values['write-baseline'] })
  if (result.failures.length) {
    console.error(result.failures.join('\n'))
    process.exitCode = 1
  } else
    console.log(
      `${record.id} OK (${result.offenders.length} existing, ${result.baselined} baselined)`,
    )
}
