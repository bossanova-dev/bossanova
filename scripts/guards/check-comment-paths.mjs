import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { parseArgs } from 'node:util'
import { isMainModule } from '../../skills-toolbox/main-module.mjs'
import { compareBaseline, formatBaseline, readBaseline, splitBaselineEntry } from './baseline.mjs'
import { parseExceptions, isExcepted } from './exceptions.mjs'
import { commentsOf, commentFiles } from './source-comments.mjs'
import { execFileSync, spawnSync } from 'node:child_process'
import record from './registry/comment-paths.mjs'

const REPO_ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..')

export function check({ root = REPO_ROOT, writeBaseline = false } = {}) {
  const offenders = new Set()
  const errors = []
  const tracked = execFileSync('git', ['ls-files', '-z'], { cwd: root })
    .toString()
    .split('\0')
    .filter(Boolean)
  const existing = new Set(tracked)
  for (const file of tracked) {
    let dir = path.posix.dirname(file)
    while (dir !== '.') {
      existing.add(dir)
      dir = path.posix.dirname(dir)
    }
  }
  const roots = execFileSync('git', ['ls-tree', '-d', '--name-only', 'HEAD'], { cwd: root })
    .toString()
    .trim()
    .split('\n')
    .filter(Boolean)
  const matcher = new RegExp(
    '(?:^|[\\s`\'"(\\[])((' +
      roots.map((root) => root.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')).join('|') +
      ')/[^\\s`\'"()]+)',
    'g',
  )
  const resolves = (citation, file) => {
    const candidates = [citation]
    let dir = path.posix.dirname(file)
    while (dir !== '.') {
      candidates.push(path.posix.join(dir, citation))
      dir = path.posix.dirname(dir)
    }
    return candidates.some(
      (candidate) =>
        existing.has(candidate) || existing.has(candidate.replace(/\.[A-Z]\w*(?:\.\w+)*$/, '')),
    )
  }
  for (const file of commentFiles(root, record.scans)) {
    const text = fs.readFileSync(path.join(root, file), 'utf8')
    if (!roots.some((dir) => text.includes(`${dir}/`))) continue
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
      for (const match of comment.text.matchAll(matcher)) {
        const citationLine =
          comment.line + comment.text.slice(0, match.index).split('\n').length - 1
        if (isExcepted(exceptions, citationLine)) continue
        const citation = match[1]
          .replace(/[.,:;)}]+$/, '')
          .replace(/#.*$/, '')
          .replace(/:\d+(?:-\d+)?$/, '')
          .replace(/\/+$/, '')
        if (/[*?{}<>$\[\]]|\.\.\.|…/.test(citation) || citation.endsWith('-')) continue
        if (!path.posix.extname(citation) && citation.split('/').length < 3) continue
        if (resolves(citation, file)) continue
        const ignored = spawnSync('git', ['check-ignore', '--quiet', '--', citation], { cwd: root })
        if (ignored.status === 0) continue
        if (ignored.status !== 1) throw new Error(`git check-ignore failed for ${citation}`)
        offenders.add(`${file}: ${citation}`)
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
    remedy: 'update the citation to a tracked path or remove the stale reference',
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
