import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import test from 'node:test'
import { fileURLToPath } from 'node:url'

import { compareBaseline, formatBaseline, readBaseline, splitBaselineEntry } from './baseline.mjs'

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..')

function fixture(text) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'guard-baseline-'))
  fs.writeFileSync(path.join(root, 'baseline.json'), text)
  return root
}

function cleanup(root) {
  fs.rmSync(root, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 })
}

test('readBaseline accepts a sorted array of unique strings', () => {
  const root = fixture('[\n  "a.mjs",\n  "b.mjs"\n]\n')
  try {
    assert.deepEqual(readBaseline(root, 'baseline.json'), {
      entries: ['a.mjs', 'b.mjs'],
      errors: [],
    })
  } finally {
    cleanup(root)
  }
})

test('readBaseline rejects an unsorted file', () => {
  const root = fixture(JSON.stringify(['b.mjs', 'a.mjs']))
  try {
    const { errors } = readBaseline(root, 'baseline.json')
    assert.equal(errors.length, 1)
    assert.match(errors[0], /sorted/)
  } finally {
    cleanup(root)
  }
})

test('readBaseline rejects a file with a duplicate', () => {
  const root = fixture(JSON.stringify(['a.mjs', 'a.mjs']))
  try {
    const { errors } = readBaseline(root, 'baseline.json')
    assert.equal(errors.length, 1)
    assert.match(errors[0], /duplicate/)
  } finally {
    cleanup(root)
  }
})

test('readBaseline rejects a non-string entry', () => {
  const root = fixture(JSON.stringify(['a.mjs', 7]))
  try {
    const { entries, errors } = readBaseline(root, 'baseline.json')
    assert.deepEqual(entries, [])
    assert.match(errors[0], /array of strings/)
  } finally {
    cleanup(root)
  }
})

test('readBaseline rejects a non-array value and an unreadable file', () => {
  const root = fixture('{"a": 1}')
  try {
    assert.match(readBaseline(root, 'baseline.json').errors[0], /array of strings/)
    assert.match(readBaseline(root, 'missing.json').errors[0], /cannot read missing\.json/)
  } finally {
    cleanup(root)
  }
})

test('compareBaseline reports a stale entry as a failure', () => {
  const result = compareBaseline({
    baselinePath: 'scripts/x-baseline.json',
    entries: ['gone.mjs', 'kept.mjs'],
    offenders: ['kept.mjs'],
    remedy: 'fix it',
  })
  assert.deepEqual(result.staleEntries, ['gone.mjs'])
  assert.deepEqual(result.newViolations, [])
  assert.deepEqual(result.failures, [
    'scripts/x-baseline.json: gone.mjs no longer occurs — remove it',
  ])
})

test('compareBaseline reports an offender missing from the baseline with the remedy', () => {
  const result = compareBaseline({
    baselinePath: 'scripts/x-baseline.json',
    entries: ['kept.mjs'],
    offenders: ['kept.mjs', 'new.mjs'],
    remedy: 'fix it',
  })
  assert.deepEqual(result.newViolations, ['new.mjs'])
  assert.deepEqual(result.staleEntries, [])
  assert.deepEqual(result.failures, ['new.mjs: fix it'])
})

test('formatBaseline sorts, de-duplicates and serialises', () => {
  assert.equal(formatBaseline(['b', 'a', 'b']), '[\n  "a",\n  "b"\n]\n')
  assert.equal(formatBaseline([]), '[]\n')
})

test('formatBaseline reproduces the CLI flag-parser baseline byte for byte', () => {
  const file = path.join(repoRoot, 'scripts', 'cli-flag-parsers-baseline.json')
  const bytes = fs.readFileSync(file, 'utf8')
  const { entries, errors } = readBaseline(repoRoot, 'scripts/cli-flag-parsers-baseline.json')
  assert.deepEqual(errors, [])
  assert.ok(entries.length > 0, 'the real baseline must not be empty')
  assert.equal(formatBaseline(entries), bytes)
})

test('compareBaseline supports a remedy per offender', () => {
  assert.deepEqual(
    compareBaseline({
      baselinePath: 'x',
      entries: [],
      offenders: ['a', 'b'],
      remedy: (id) => `replace ${id}`,
    }).failures,
    ['a: replace a', 'b: replace b'],
  )
})

test('splitBaselineEntry separates the compared key from an inline reason', () => {
  assert.deepEqual(splitBaselineEntry('a.mjs — kept on purpose'), {
    key: 'a.mjs',
    reason: 'kept on purpose',
  })
  assert.deepEqual(splitBaselineEntry('a.mjs: x — y — z'), { key: 'a.mjs: x', reason: 'y — z' })
  assert.deepEqual(splitBaselineEntry('a.mjs'), { key: 'a.mjs', reason: '' })
  assert.deepEqual(splitBaselineEntry('a.mjs —  '), { key: 'a.mjs', reason: '' })
})

test('every key-list baseline a guard reads is in the shared format, byte for byte', () => {
  for (const relPath of [
    'scripts/action-pins-baseline.json',
    'scripts/cli-flag-parsers-baseline.json',
    'scripts/ellipsis-consistency-baseline.json',
    'scripts/exec-waitdelay-baseline.json',
    'scripts/price-parity-baseline.json',
    'scripts/proto-field-names-baseline.json',
  ]) {
    const { entries, errors } = readBaseline(repoRoot, relPath)
    assert.deepEqual(errors, [], relPath)
    assert.equal(
      formatBaseline(entries),
      fs.readFileSync(path.join(repoRoot, relPath), 'utf8'),
      relPath,
    )
  }
})
