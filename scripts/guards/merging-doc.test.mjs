import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import test from 'node:test'
import * as prettier from 'prettier'
import { loadSources, mergingDoc, renderRegion } from './merging-doc.mjs'

const repoRoot = path.resolve(import.meta.dirname, '../..')
const cli = path.join(import.meta.dirname, 'merging-doc.mjs')
const original =
  '---\nsummary: Fixture.\n---\n\n# Fixture\n\n<!-- BEGIN GENERATED merge-reasons -->\nstale\n<!-- END GENERATED merge-reasons -->\n'
function fixture(t) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'merging-doc-'))
  t.after(() => fs.rmSync(dir, { recursive: true, force: true, maxRetries: 5 }))
  const docFile = path.join(dir, 'merging.md')
  fs.writeFileSync(docFile, original)
  return docFile
}
const run = (docFile, mode) =>
  spawnSync(process.execPath, [cli, mode, '--doc-file', docFile], { encoding: 'utf8' })

test('--check fails when a merge-eligibility reason is added without regenerating', async (t) => {
  const docFile = fixture(t)
  assert.equal(run(docFile, '--write').status, 0)
  const sources = await loadSources(repoRoot)
  const result = await mergingDoc({
    repoRoot,
    docFile,
    mode: 'check',
    sources: { ...sources, shared: [...sources.shared, 'seeded-reason'] },
  })
  assert.equal(result.ok, false)
  assert.match(result.messages.join('\n'), /merging\.md/)
  assert.match(result.messages.join('\n'), /seeded-reason/)
})
test('--check fails on a hand-edited line inside the region', (t) => {
  const docFile = fixture(t)
  assert.equal(run(docFile, '--write').status, 0)
  fs.writeFileSync(
    docFile,
    fs.readFileSync(docFile, 'utf8').replace('`ci-not-settled`', '`edited-reason`'),
  )
  const result = run(docFile, '--check')
  assert.equal(result.status, 1)
  assert.match(result.stderr, /edited-reason/)
  assert.match(result.stderr, /Run: make merging-doc-update/)
})
test('a second --write changes no bytes', (t) => {
  const docFile = fixture(t)
  assert.equal(run(docFile, '--write').status, 0)
  const before = fs.readFileSync(docFile, 'utf8')
  assert.equal(run(docFile, '--write').status, 0)
  assert.equal(fs.readFileSync(docFile, 'utf8'), before)
  assert.equal(run(docFile, '--check').status, 0)
})
test('the generated output is byte-identical after prettier', async (t) => {
  const docFile = fixture(t)
  assert.equal(run(docFile, '--write').status, 0)
  const text = fs.readFileSync(docFile, 'utf8')
  assert.equal(await prettier.format(text, { parser: 'markdown' }), text)
})
test('source lists retain first-seen order and omit shared session reasons', async () => {
  const sources = await loadSources(repoRoot)
  const text = renderRegion({
    ...sources,
    shared: ['shared'],
    session: ['shared', 'extra', 'extra'],
    human: [],
  })
  assert.equal(text.match(/- `extra`/g)?.length, 1)
  assert.equal(text.match(/- `shared`/g)?.length, 1)
})
test('missing, duplicate and misordered region markers fail without writing', (t) => {
  const docFile = fixture(t)
  for (const content of [
    '',
    original + '<!-- END GENERATED merge-reasons -->\n',
    '<!-- END GENERATED merge-reasons -->\n<!-- BEGIN GENERATED merge-reasons -->\n',
  ]) {
    fs.writeFileSync(docFile, content)
    assert.equal(run(docFile, '--write').status, 1)
    assert.equal(fs.readFileSync(docFile, 'utf8'), content)
  }
})
test('bad usage exits 2', () => {
  for (const args of [[], ['--write', '--check'], ['--bogus']]) {
    assert.equal(spawnSync(process.execPath, [cli, ...args], { encoding: 'utf8' }).status, 2)
  }
})

test('review paths render named areas, park codes, omissions and legacy deprecations', async () => {
  const sources = await loadSources(repoRoot)
  const text = renderRegion({
    ...sources,
    review: {
      alwaysReview: [
        {
          name: 'API schema',
          globs: ['entity/**'],
          parkCode: 'always-human-path',
          label: 'API schema',
        },
      ],
      neverReview: ['dbt/**'],
      deprecations: [],
    },
  })
  assert.match(text, /## Review paths/)
  assert.match(
    text,
    /### alwaysReview[\s\S]*#### API schema[\s\S]*`always-human-path`[\s\S]*`entity\/\*\*`/,
  )
  assert.match(text, /### neverReview[\s\S]*`dbt\/\*\*`/)
  assert.match(text, /legacy keys.*deprecated/i)
})
