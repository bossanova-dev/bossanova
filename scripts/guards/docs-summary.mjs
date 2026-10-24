#!/usr/bin/env node
// guard-id: docs-summary
import fs from 'node:fs'
import path from 'node:path'
import { parseArgs } from 'node:util'
import { isMainModule } from '../../skills-toolbox/main-module.mjs'
import { scannedFiles } from './registry.mjs'
import { summary } from './docs-index.mjs'
const ROOT = path.resolve(import.meta.dirname, '../..')
/** @param {{root?: string}} [options] */
export function check({ root = ROOT } = {}) {
  const failures = []
  for (const file of scannedFiles(root, ['docs/**/*.md'])) {
    if (/^docs\/(solutions|skills)\//.test(file) || /^docs\/email-course\/day-.*\.md$/.test(file))
      continue
    if (!fs.lstatSync(path.join(root, file)).isFile()) continue
    try {
      summary(root, file)
    } catch (error) {
      failures.push(error.message)
    }
  }
  return { failures }
}
if (isMainModule(import.meta.url)) {
  const { values } = parseArgs({
    strict: true,
    options: { root: { type: 'string', default: ROOT } },
  })
  const { failures } = check({ root: values.root })
  for (const failure of failures) console.error(failure)
  if (!failures.length) console.log('docs-summary OK')
  process.exitCode = failures.length ? 1 : 0
}
