import fs, { mkdtempSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import path from 'node:path'

import { afterEach, describe, expect, it } from 'vitest'

import { checkDensity, measureMarkdown } from './prose-density.mjs'

const temporaryDirectories = []

function fixture(files, baseline) {
  const root = mkdtempSync(path.join(tmpdir(), 'prose-density-'))
  temporaryDirectories.push(root)
  const docsDir = path.join(root, 'docs')
  fs.mkdirSync(docsDir, { recursive: true })
  for (const [name, content] of Object.entries(files)) {
    const destination = path.join(docsDir, name)
    fs.mkdirSync(path.dirname(destination), { recursive: true })
    writeFileSync(destination, content)
  }
  writeFileSync(path.join(root, 'prose-baseline.json'), JSON.stringify(baseline))
  return root
}

afterEach(() => {
  for (const directory of temporaryDirectories.splice(0)) {
    fs.rmSync(directory, { recursive: true, force: true })
  }
})

describe('measureMarkdown', () => {
  it('excludes fences, table rows, and term-definition labels', () => {
    const markdown = [
      'One two three — four.',
      '',
      '**Term** — definition',
      '- **List term** — definition — aside',
      '  - **Nested term** — definition',
      '',
      '| Column — value | Other |',
      '| --- | --- |',
      '',
      '````text',
      '```',
      'code — code',
      '````',
    ].join('\n')

    expect(measureMarkdown(markdown)).toEqual({ dashes: 2, words: 13, perThousand: 153.846154 })
  })
})

describe('checkDensity', () => {
  it('passes a file at its baseline', () => {
    const root = fixture({ 'guide.md': 'one two — three four\n' }, { 'guide.md': 250 })
    expect(checkDensity(root)).toMatchObject({ ok: true, checked: 1 })
  })

  it('passes a file below its baseline', () => {
    const root = fixture({ 'guide.md': 'one two three four\n' }, { 'guide.md': 250 })
    expect(checkDensity(root)).toMatchObject({ ok: true, checked: 1 })
  })

  it('fails a file above its baseline', () => {
    const root = fixture({ 'guide.md': 'one — two\n' }, { 'guide.md': 499 })
    const result = checkDensity(root)
    expect(result.ok).toBe(false)
    expect(result.failures[0]).toMatchObject({ file: 'guide.md', actual: 500, baseline: 499 })
  })

  it('fails when a document has no baseline entry', () => {
    const root = fixture({ 'new.md': 'new document\n' }, {})
    const result = checkDensity(root)
    expect(result.ok).toBe(false)
    expect(result.failures[0]).toMatchObject({ file: 'new.md', reason: 'missing baseline' })
  })
})
