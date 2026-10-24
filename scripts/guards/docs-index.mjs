#!/usr/bin/env node
// guard-id: docs-index-fresh
import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { parseArgs } from 'node:util'
import { isMainModule } from '../../skills-toolbox/main-module.mjs'
import { scannedFiles } from './registry.mjs'
import { spliceRegion, renderDrift } from './generated-region.mjs'

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..')
const UPDATE_COMMAND = 'make docs-index-update'

/** @param {string} repoRoot @param {string} file */
export function summary(repoRoot, file) {
  const text = fs.readFileSync(path.join(repoRoot, file), 'utf8')
  const frontmatter = text.match(/^---\r?\n([\s\S]*?)\r?\n---(?:\r?\n|$)/)?.[1]
  const lines = frontmatter?.split(/\r?\n/) ?? []
  const matches = lines.flatMap((line, index) => (/^summary:/.test(line) ? [index] : []))
  const at = matches[0]
  const value = at === undefined ? '' : lines[at].slice('summary:'.length).trim()
  if (
    matches.length !== 1 ||
    !value ||
    /^[|>]/.test(value) ||
    /^\s+\S/.test(lines[at + 1] ?? '') ||
    /^[\[{]/.test(value)
  ) {
    throw new Error(`${file}: summary must be one non-empty single-line frontmatter scalar`)
  }
  const unquoted = value.replace(/^(['"])(.*)\1$/, '$2')
  if (!unquoted.trim()) throw new Error(`${file}: summary must not be empty`)
  return unquoted
}

/** Generate the tracked docs index or check its freshness.
 * @param {{repoRoot?: string, mode: 'write' | 'check'}} options
 */
export function docsIndex({ repoRoot = ROOT, mode }) {
  try {
    const files = scannedFiles(repoRoot, ['docs/**/*.md']).sort()
    const directories = files
      .filter((file) => file.endsWith('/README.md') && !file.startsWith('docs/solutions/'))
      .map((file) => path.posix.dirname(file) + '/')
    const listed = files.filter(
      (file) =>
        !file.startsWith('docs/solutions/') &&
        !directories.some((dir) => file.startsWith(dir) && file !== `${dir}README.md`),
    )
    const entries = listed.map((file) => ({ file, summary: summary(repoRoot, file) }))
    if (files.some((file) => file.startsWith('docs/solutions/')))
      entries.push({
        file: 'docs/solutions/',
        summary:
          'Documented solutions to past problems, organized by category with module and tag metadata.',
      })
    entries.sort((a, b) => a.file.localeCompare(b.file, 'en'))
    const body = [
      '<!-- prettier-ignore-start -->',
      ...entries.map(({ file, summary }) => `- [${file}](${file}) — ${summary}`),
      '<!-- prettier-ignore-end -->',
      '',
    ].join('\n')
    const file = path.join(repoRoot, 'AGENTS.md')
    const actual = fs.readFileSync(file, 'utf8')
    const expected = spliceRegion(actual, body, 'docs-index', UPDATE_COMMAND)
    if (mode === 'write') {
      if (actual !== expected) fs.writeFileSync(file, expected)
      return { ok: true, messages: [] }
    }
    return {
      ok: actual === expected,
      messages:
        actual === expected
          ? []
          : [renderDrift({ actual, expected, label: 'AGENTS.md' }, UPDATE_COMMAND)],
    }
  } catch (error) {
    return { ok: false, messages: [error.message] }
  }
}

if (isMainModule(import.meta.url)) {
  const { values } = parseArgs({
    options: {
      write: { type: 'boolean', default: false },
      check: { type: 'boolean', default: false },
    },
    allowPositionals: false,
  })
  if (values.write === values.check) {
    console.error('usage: node scripts/guards/docs-index.mjs (--write | --check)')
    process.exit(2)
  }
  const { ok, messages } = docsIndex({ mode: values.write ? 'write' : 'check' })
  for (const message of messages) console.error(message)
  if (ok && values.check) console.log('Docs index OK')
  process.exitCode = ok ? 0 : 1
}
