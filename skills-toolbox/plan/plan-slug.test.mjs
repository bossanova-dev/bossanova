// skills-toolbox/plan/plan-slug.test.mjs
import assert from 'node:assert/strict'
import test from 'node:test'
import { spawnSync } from 'node:child_process'
import { mkdtempSync, writeFileSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'
import { planScratchToken } from './plan-scratch-paths.mjs'

import { issueSlug } from './plan-slug.mjs'

test('issueSlug lowercases, hyphenates, and upcases the id', () => {
  assert.equal(
    issueSlug('bos-5', 'Add an unsubscribe mechanism!'),
    'BOS-5-add-an-unsubscribe-mechanism',
  )
})

test('issueSlug collapses runs of non-alphanumerics and trims edges', () => {
  assert.equal(issueSlug('BOS-12', '  Review   the   DB schema  '), 'BOS-12-review-the-db-schema')
})

test('issueSlug leaves an already-uppercase id unchanged', () => {
  assert.equal(issueSlug('XYZ-9', 'Ship the thing!'), 'XYZ-9-ship-the-thing')
})

test('issueSlug rejects missing, wrong-typed, and blank arguments', () => {
  for (const args of [
    [{ id: 'BOS-1', title: 'x' }],
    ['Some title'],
    [],
    [1, 'Valid title'],
    ['BOS-1', 1],
    ['', 'Valid title'],
    ['   ', 'Valid title'],
    ['BOS-1', ''],
    ['BOS-1', '   '],
  ]) {
    assert.throws(() => issueSlug(...args), TypeError)
  }
})

test('issueSlug preserves a non-blank title that slugs to nothing', () => {
  assert.equal(issueSlug('BOS-1', '!!!'), 'BOS-1-')
})

const cli = fileURLToPath(new URL('./plan-slug.mjs', import.meta.url))
const invoke = (...args) => spawnSync(process.execPath, [cli, ...args], { encoding: 'utf8' })

test('titleSlug validates titles and preserves existing slug normalization', async () => {
  const { titleSlug } = await import('./plan-slug.mjs')
  assert.equal(titleSlug('  Fix the THING!  '), 'fix-the-thing')
  assert.equal(titleSlug('!!!'), '')
  for (const title of [undefined, null, {}, 1, '', '  ']) {
    assert.throws(() => titleSlug(title), TypeError)
  }
})

test('plan-path prints one declared plan path with a single issue prefix', () => {
  const result = invoke('plan-path', '--run-id', 'r1', '--id', 'bos-7', '--title', 'Fix the thing!')
  assert.equal(result.status, 0, result.stderr)
  assert.equal(result.stdout, '.linear-plans/run-r1/BOS-7-fix-the-thing.md\n')
  assert.deepEqual(planScratchToken(result.stdout.trim()).families, ['plan'])
  const punctuation = invoke('plan-path', '--run-id', 'r1', '--id', 'bos-7', '--title', '!!!')
  assert.equal(punctuation.status, 0, punctuation.stderr)
  assert.equal(punctuation.stdout, '.linear-plans/run-r1/BOS-7-.md\n')
})

test('plan-path reads bare payloads and prefers a non-blank identifier', () => {
  const dir = mkdtempSync(join(tmpdir(), 'plan-slug-'))
  try {
    const file = join(dir, 'issue.json')
    for (const payload of [
      { id: 'bos-7', title: 'Fix the thing!' },
      { id: 'uuid', identifier: 'bos-7', title: 'Fix the thing!' },
      { id: 'bos-7', identifier: '  ', title: 'Fix the thing!' },
    ]) {
      writeFileSync(file, JSON.stringify(payload))
      const result = invoke('plan-path', '--run-id', 'r1', '--issue-json', file)
      assert.equal(result.status, 0, result.stderr)
      assert.equal(result.stdout, '.linear-plans/run-r1/BOS-7-fix-the-thing.md\n')
    }
    for (const payload of [{ id: 'bos-7' }, { id: 7, title: 'Fix' }, null]) {
      writeFileSync(file, JSON.stringify(payload))
      const result = invoke('plan-path', '--run-id', 'r1', '--issue-json', file)
      assert.equal(result.status, 1)
      assert.equal(result.stdout, '')
      assert.equal(result.stderr.trim().split('\n').length, 1)
    }
    writeFileSync(file, '{')
    assert.equal(invoke('plan-path', '--run-id', 'r1', '--issue-json', file).status, 1)
    assert.equal(
      invoke('plan-path', '--run-id', 'r1', '--issue-json', join(dir, 'missing')).status,
      1,
    )
  } finally {
    rmSync(dir, { recursive: true, force: true, maxRetries: 3 })
  }
})

test('plan-path accepts issue titles containing undefined', () => {
  for (const title of ['Handle undefined values', 'undefined']) {
    const result = invoke('plan-path', '--run-id', 'r1', '--id', 'bos-7', '--title', title)
    assert.equal(result.status, 0, result.stderr)
    assert.equal(
      result.stdout,
      `.linear-plans/run-r1/BOS-7-${title.toLowerCase().replaceAll(' ', '-')}.md\n`,
    )
  }
})

test('plan-path rejects invalid usage without printing a path', () => {
  const valid = ['plan-path', '--run-id', 'r1', '--id', 'bos-7', '--title', 'Fix']
  for (const args of [
    [],
    ['unknown'],
    [...valid, '--unknown'],
    [...valid, '--issue-json', 'issue.json'],
    ['plan-path', '--run-id', 'r1'],
    ['plan-path', '--run-id', 'r1', '--id', 'bos-7'],
    ['plan-path', '--run-id', 'r1', '--title', 'Fix'],
    ['plan-path', '--id', 'bos-7', '--title', 'Fix'],
    [...valid, 'unexpected'],
  ]) {
    const result = invoke(...args)
    assert.equal(result.status, 64, result.stderr)
    assert.equal(result.stdout, '')
    assert.equal(result.stderr.trim().split('\n').length, 1)
  }
  for (const args of [
    ['plan-path', '--run-id', 'bad/run', '--id', 'bos-7', '--title', 'Fix'],
    ['plan-path', '--run-id', 'r1', '--id', '', '--title', 'Fix'],
    ['plan-path', '--run-id', 'r1', '--id', 'bos-7', '--title', ' '],
  ]) {
    const result = invoke(...args)
    assert.equal(result.status, 1, result.stderr)
    assert.equal(result.stdout, '')
    assert.equal(result.stderr.trim().split('\n').length, 1)
  }
})
