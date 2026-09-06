#!/usr/bin/env node

import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

import { isMainModule } from '../../../skills-toolbox/main-module.mjs'

const DOC_EXTENSIONS = new Set(['.md', '.mdx'])
const TERM_DEFINITION_RE = /^(\s*(?:(?:[-+*]|\d+[.)])\s+)?\*\*[^*\n]+\*\*)\s+—\s+/

function collectDocumentFiles(directory, root, files) {
  for (const entry of fs.readdirSync(directory, { withFileTypes: true })) {
    const fullPath = path.join(directory, entry.name)
    if (entry.isDirectory()) collectDocumentFiles(fullPath, root, files)
    else if (DOC_EXTENSIONS.has(path.extname(entry.name))) {
      files.push(path.relative(root, fullPath).split(path.sep).join('/'))
    }
  }
}

function documentFiles(directory) {
  const files = []
  collectDocumentFiles(directory, directory, files)
  return files.sort()
}

export function proseLines(markdown) {
  const kept = []
  let fence = null

  for (const line of markdown.split(/\r?\n/)) {
    if (fence !== null) {
      const closing = line.match(/^ {0,3}([`~]+)[ \t]*$/)?.[1]
      if (closing?.[0] === fence[0] && closing.length >= fence.length) fence = null
      continue
    }
    const opening = line.match(/^ {0,3}(`{3,}|~{3,})/)?.[1]
    if (opening) {
      fence = opening
      continue
    }
    if (/^\s*\|.*\|\s*$/.test(line)) continue
    kept.push(line.replace(TERM_DEFINITION_RE, '$1 '))
  }

  return kept.join('\n')
}

export function measureMarkdown(markdown) {
  const prose = proseLines(markdown)
  const dashes = prose.match(/—/g)?.length ?? 0
  const words = prose.match(/[\p{L}\p{N}]+(?:['’][\p{L}\p{N}]+)*/gu)?.length ?? 0
  const perThousand = words === 0 ? 0 : Number(((dashes * 1000) / words).toFixed(6))
  return { dashes, words, perThousand }
}

const DEFAULT_ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')

function createBaseline(root = DEFAULT_ROOT) {
  const docsDirectory = path.join(root, 'docs')
  return Object.fromEntries(
    documentFiles(docsDirectory).map((file) => {
      const markdown = fs.readFileSync(path.join(docsDirectory, file), 'utf8')
      return [file, measureMarkdown(markdown).perThousand]
    }),
  )
}

export function checkDensity(root = DEFAULT_ROOT) {
  const docsDirectory = path.join(root, 'docs')
  const baselinePath = path.join(root, 'prose-baseline.json')
  let baseline
  let files
  try {
    baseline = JSON.parse(fs.readFileSync(baselinePath, 'utf8'))
  } catch (error) {
    if (error.code === 'ENOENT') {
      return { ok: false, checked: 0, failures: [{ reason: 'baseline missing' }] }
    }
    throw error
  }
  try {
    files = documentFiles(docsDirectory)
  } catch (error) {
    if (error.code === 'ENOENT') {
      return { ok: false, checked: 0, failures: [{ reason: 'docs directory missing' }] }
    }
    throw error
  }
  const failures = []
  const fileSet = new Set(files)

  for (const file of files) {
    if (!Object.hasOwn(baseline, file)) {
      failures.push({ file, reason: 'missing baseline' })
      continue
    }
    const actual = measureMarkdown(
      fs.readFileSync(path.join(docsDirectory, file), 'utf8'),
    ).perThousand
    if (actual > baseline[file]) failures.push({ file, actual, baseline: baseline[file] })
  }

  for (const file of Object.keys(baseline)) {
    if (!fileSet.has(file)) failures.push({ file, reason: 'baseline entry has no document' })
  }

  return { ok: failures.length === 0, checked: files.length, failures }
}

function run() {
  if (process.argv.includes('--write-baseline')) {
    fs.writeFileSync(
      path.join(DEFAULT_ROOT, 'prose-baseline.json'),
      `${JSON.stringify(createBaseline(DEFAULT_ROOT), null, 2)}\n`,
    )
    console.log('Wrote prose-baseline.json')
    return
  }

  const result = checkDensity(DEFAULT_ROOT)
  if (!result.ok) {
    console.error('Prose density failed:')
    for (const failure of result.failures) {
      if (failure.reason) console.error(`- ${failure.file ?? 'docs'}: ${failure.reason}`)
      else
        console.error(
          `- ${failure.file}: ${failure.actual} exceeds ${failure.baseline} em dashes/1,000 words`,
        )
    }
    process.exitCode = 1
    return
  }
  console.log(`Prose density OK (${result.checked} files checked)`)
}

if (isMainModule(import.meta.url)) run()
