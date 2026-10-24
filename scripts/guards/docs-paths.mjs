#!/usr/bin/env node
// guard-id: docs-paths
import fs from 'node:fs'
import path from 'node:path'
import { spawnSync } from 'node:child_process'
import { parseArgs } from 'node:util'
import { isMainModule } from '../../skills-toolbox/main-module.mjs'
import { trackedFiles } from '../dev/tree-scan-lib.mjs'
import { scannedFiles } from './registry.mjs'
import { compareBaseline, formatBaseline, readBaseline, splitBaselineEntry } from './baseline.mjs'
import { parseExceptions, isExcepted } from './exceptions.mjs'
import record from './registry/docs-paths.mjs'
const ROOT = path.resolve(import.meta.dirname, '../..')
/** Extract inline citations while preserving source line numbers.
 * @param {string} text
 * @returns {{value: string, line: number, link: boolean}[]}
 */
export function citations(text) {
  const found = []
  let fence = ''
  let width = 0
  for (const [index, line] of text.split('\n').entries()) {
    const marker = /^ {0,3}(`{3,}|~{3,})/.exec(line)?.[1]
    if (marker) {
      if (!fence) {
        fence = marker[0]
        width = marker.length
      } else if (marker[0] === fence && marker.length >= width && /^ {0,3}[`~]+\s*$/.test(line))
        fence = ''
      continue
    }
    if (fence) continue
    // Remove code spans before scanning links, so link-like examples remain code.
    const withoutCode = line.replace(/(`+)([^`]*?)\1/g, (_match, _ticks, value) => {
      found.push({ value, line: index + 1, link: false })
      return ' '
    })
    for (const match of withoutCode.matchAll(
      /!?\[[^\]]*\]\(\s*(<[^>]*>|[^\s)]+)(?:\s+["'][^)]*)?\s*\)/g,
    )) {
      found.push({ value: match[1], line: index + 1, link: true })
    }
  }
  return found
}
/** @param {{root?: string, writeBaseline?: boolean}} [options] */
export function check({ root = ROOT, writeBaseline = false } = {}) {
  const tracked = trackedFiles(root)
  const existing = new Set(tracked)
  const roots = new Set(tracked.map((file) => file.split('/')[0]))
  for (const file of tracked) {
    let dir = path.posix.dirname(file)
    while (dir !== '.') {
      existing.add(dir)
      dir = path.posix.dirname(dir)
    }
  }
  const offenders = new Set()
  const errors = []
  for (const file of scannedFiles(root, record.scans, { tracked })) {
    if (!fs.lstatSync(path.join(root, file)).isFile()) continue
    const text = fs.readFileSync(path.join(root, file), 'utf8')
    const exceptions = parseExceptions(text, { guardId: record.id })
    errors.push(...exceptions.errors.map((error) => `${file}: ${error}`))
    for (const citation of citations(text)) {
      if (isExcepted(exceptions, citation.line)) continue
      const raw = citation.value
      if (
        /^[a-z][a-z\d+.-]*:/i.test(raw) ||
        raw.startsWith('#') ||
        /[<>*?{}\[\]$]|\.\.\.|…/.test(raw)
      )
        continue
      const value = raw
        .replace(/#.*$/, '')
        .replace(/[.,;)}]+$/, '')
        .replace(/:\d+(?:-\d+)?$/, '')
        .replace(/\/+$/, '')
      if (!value) continue
      if (!citation.link && (!value.includes('/') || !roots.has(value.split('/')[0]))) continue
      const target = citation.link
        ? path.posix.normalize(
            value.startsWith('/')
              ? value.slice(1)
              : path.posix.join(path.posix.dirname(file), value),
          )
        : value
      if (existing.has(target)) continue
      const ignored = spawnSync('git', ['check-ignore', '--quiet', '--', target], { cwd: root })
      if (ignored.status === 0) continue
      const ignoredDirectory = spawnSync('git', ['check-ignore', '--quiet', '--', `${target}/`], {
        cwd: root,
      })
      if (ignoredDirectory.status === 0) continue
      if (ignored.status !== 1) {
        let ancestor = path.posix.dirname(target)
        let covered = false
        while (ancestor !== '.' && ancestor !== '/') {
          if (
            spawnSync('git', ['check-ignore', '--quiet', '--', ancestor], { cwd: root }).status ===
            0
          ) {
            covered = true
            break
          }
          ancestor = path.posix.dirname(ancestor)
        }
        if (covered) continue
        throw new Error(`git check-ignore failed for ${target}`)
      }
      offenders.add(`${file}: ${value}`)
    }
  }
  if (writeBaseline) {
    fs.writeFileSync(path.join(root, record.baseline), formatBaseline(offenders))
    return { failures: errors, offenders: [...offenders].sort() }
  }
  const baseline = readBaseline(root, record.baseline)
  return {
    failures: [
      ...errors,
      ...baseline.errors,
      ...compareBaseline({
        baselinePath: record.baseline,
        entries: baseline.entries.map((entry) => splitBaselineEntry(entry).key),
        offenders,
        remedy: 'update the citation to a tracked or git-ignored path',
      }).failures,
    ],
    offenders: [...offenders].sort(),
  }
}
if (isMainModule(import.meta.url)) {
  const { values } = parseArgs({
    strict: true,
    options: {
      root: { type: 'string', default: ROOT },
      'write-baseline': { type: 'boolean', default: false },
    },
  })
  const result = check({ root: values.root, writeBaseline: values['write-baseline'] })
  for (const failure of result.failures) console.error(failure)
  if (!result.failures.length)
    console.log(`docs-paths OK (${result.offenders.length} existing citations)`)
  process.exitCode = result.failures.length ? 1 : 0
}
