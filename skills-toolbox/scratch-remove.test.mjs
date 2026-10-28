import assert from 'node:assert/strict'
import {
  existsSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  rmSync,
  symlinkSync,
  writeFileSync,
} from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { spawnSync } from 'node:child_process'
import test from 'node:test'
import { fileURLToPath } from 'node:url'
import { classifyScratchTarget, main } from './scratch-remove.mjs'

function fixture(t) {
  const base = mkdtempSync(path.join(os.tmpdir(), 'scratch-remove-test-'))
  t.after(() => rmSync(base, { recursive: true, force: true, maxRetries: 3 }))
  const root = path.join(base, 'temp')
  const outside = path.join(base, 'outside')
  mkdirSync(root)
  mkdirSync(outside)
  return { base, root, outside }
}

function run(records, root) {
  const lines = []
  const status = main(
    records.flatMap((record) => ['--record', record]),
    {
      tmpRoots: [root],
      write: (line) => lines.push(JSON.parse(line)),
    },
  )
  return { status, lines }
}

test('pure classifier confines absolute paths strictly beneath roots', () => {
  const tmpRoots = ['/fixture/temp']
  for (const target of [
    '',
    'relative',
    '/',
    '/fixture/temp',
    '/fixture/temporary/x',
    '/fixture/temp/../outside',
  ]) {
    assert.equal(classifyScratchTarget(target, { tmpRoots }).allowed, false, target)
  }
  assert.equal(classifyScratchTarget('/fixture/temp/work', { tmpRoots }).allowed, true)
})

test('removes recorded directory using trimmed first line and removes record', (t) => {
  const { base, root } = fixture(t)
  const target = path.join(root, 'work')
  mkdirSync(target)
  writeFileSync(path.join(target, 'data'), 'fixture')
  const record = path.join(base, 'record')
  writeFileSync(record, `  ${target}  \nignored second line`)
  assert.deepEqual(run([record], root), {
    status: 0,
    lines: [{ record, path: target, outcome: 'removed' }],
  })
  assert.equal(existsSync(target), false)
  assert.equal(existsSync(record), false)
})

test('missing records and missing targets are idempotent successes', (t) => {
  const { base, root } = fixture(t)
  const missingRecord = path.join(base, 'missing-record')
  const record = path.join(base, 'record')
  const target = path.join(root, 'gone', 'work')
  writeFileSync(record, target)
  assert.deepEqual(run([missingRecord, record], root), {
    status: 0,
    lines: [
      { record: missingRecord, path: null, outcome: 'absent' },
      { record, path: target, outcome: 'absent' },
    ],
  })
  assert.equal(existsSync(record), false)
})

test('refuses unsafe fixture targets and still processes subsequent records', (t) => {
  const { base, root, outside } = fixture(t)
  const escape = path.join(root, 'escape')
  symlinkSync(outside, escape)
  const targets = [root, outside, 'relative', '', escape, path.join(escape, 'missing')]
  const records = targets.map((target, i) => {
    const record = path.join(base, `record-${i}`)
    writeFileSync(record, target)
    return record
  })
  const safe = path.join(root, 'safe')
  mkdirSync(safe)
  const safeRecord = path.join(base, 'safe-record')
  writeFileSync(safeRecord, safe)
  const result = run([...records, safeRecord], root)
  assert.equal(result.status, 2)
  assert.deepEqual(
    result.lines.map((line) => line.outcome),
    [...targets.map(() => 'refused'), 'removed'],
  )
  for (const [i, record] of records.entries())
    assert.equal(readFileSync(record, 'utf8'), targets[i])
  assert.equal(existsSync(outside), true)
  assert.equal(existsSync(escape), true)
  assert.equal(existsSync(safe), false)
})

test('CLI rejects missing, unknown and positional arguments with usage exit code', () => {
  const cli = fileURLToPath(new URL('./scratch-remove.mjs', import.meta.url))
  for (const args of [[], ['--record'], ['--unknown'], ['positional']]) {
    const result = spawnSync(process.execPath, [cli, ...args], { encoding: 'utf8' })
    assert.equal(result.status, 64, result.stderr)
  }
})

test('CLI removes a disposable recorded target with default temp roots', (t) => {
  const { base, root } = fixture(t)
  const target = path.join(root, 'cli-work')
  mkdirSync(target)
  const record = path.join(base, 'cli-record')
  writeFileSync(record, target)
  const cli = fileURLToPath(new URL('./scratch-remove.mjs', import.meta.url))
  const result = spawnSync(process.execPath, [cli, '--record', record], { encoding: 'utf8' })
  assert.equal(result.status, 0, result.stderr)
  assert.deepEqual(JSON.parse(result.stdout), { record, path: target, outcome: 'removed' })
  assert.equal(existsSync(target), false)
  assert.equal(existsSync(record), false)
})

test('record inside its removed directory is already cleaned up', (t) => {
  const { root } = fixture(t)
  const target = path.join(root, 'work')
  mkdirSync(target)
  const record = path.join(target, 'record')
  writeFileSync(record, target)
  assert.deepEqual(run([record], root), {
    status: 0,
    lines: [{ record, path: target, outcome: 'removed' }],
  })
  assert.equal(existsSync(target), false)
})
