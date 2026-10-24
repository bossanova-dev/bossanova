#!/usr/bin/env node
import { tempDir } from '../test-infra/test-tmpdir.mjs'

import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { test } from 'node:test'

import { filterEnvSource, parseKeyList } from './write-worktree-env.mjs'

const scriptPath = path.join(path.dirname(fileURLToPath(import.meta.url)), 'write-worktree-env.mjs')

// A value that must never leave the source file: not into the output, not into stdout/stderr.
const SENTINEL = 'sentinel-secret-7f3a9c'

function run(args) {
  return spawnSync(process.execPath, [scriptPath, ...args], { encoding: 'utf8' })
}

const FIXTURE = [
  '# a comment',
  '',
  'LINEAR_API_KEY=lin_value',
  `FAKE_SECRET=${SENTINEL}`,
  'export CLOUDFLARE_ACCOUNT_ID="double quoted"',
  `BOSS_SETTINGS_PATH=/dev/${SENTINEL}`,
  "CC='single quoted'",
  'not an assignment line',
  '1BAD=starts with a digit',
  '  # indented comment',
  'LINEAR_API_KEY=second_value',
  'CC = spaced',
  '',
].join('\n')

const KEYS = 'LINEAR_API_KEY,CLOUDFLARE_ACCOUNT_ID,CC'

const EXPECTED = [
  'LINEAR_API_KEY=lin_value\n',
  'export CLOUDFLARE_ACCOUNT_ID="double quoted"\n',
  "CC='single quoted'\n",
  'LINEAR_API_KEY=second_value\n',
  'CC = spaced\n',
].join('')

test('filterEnvSource keeps allowlisted lines byte-for-byte, in order, duplicates included', () => {
  const result = filterEnvSource(FIXTURE, new Set(KEYS.split(',')))
  assert.equal(result.output, EXPECTED)
  assert.equal(result.kept, 5)
  // FAKE_SECRET, BOSS_SETTINGS_PATH, the unparseable line and the digit-led key.
  assert.equal(result.dropped, 4)
})

test('filterEnvSource preserves CRLF endings and a final line without a newline', () => {
  const result = filterEnvSource('CC=a\r\nOTHER=b\r\nCC=c', new Set(['CC']))
  assert.equal(result.output, 'CC=a\r\nCC=c')
})

test('filterEnvSource keeps nothing when no line is allowlisted', () => {
  const result = filterEnvSource(`OTHER=${SENTINEL}\n# c\n`, new Set(['CC']))
  assert.equal(result.output, '')
  assert.equal(result.kept, 0)
  assert.equal(result.dropped, 1)
})

test('parseKeyList splits names, skips empties, and reports invalid names', () => {
  assert.deepEqual(parseKeyList('A_1, _b ,,1BAD,BAD-KEY,A_1'), {
    keys: new Set(['A_1', '_b']),
    invalid: ['1BAD', 'BAD-KEY'],
  })
})

test('CLI writes only allowlisted lines with mode 0600 and names no values', () => {
  const dir = tempDir('write-worktree-env-')
  const from = path.join(dir, 'source.env')
  const to = path.join(dir, 'worktree', '.env')
  fs.mkdirSync(path.dirname(to))
  fs.writeFileSync(from, FIXTURE)

  const result = run(['--from', from, '--to', to, '--keys', KEYS])
  assert.equal(result.status, 0, result.stderr)
  assert.equal(fs.readFileSync(to, 'utf8'), EXPECTED)
  assert.equal(fs.statSync(to).mode & 0o777, 0o600)
  assert.match(result.stdout, /Wrote 5 allowlisted \.env keys into .* \(dropped 4\)/)
  assert.ok(!fs.readFileSync(to, 'utf8').includes(SENTINEL), 'sentinel secret written')
  for (const stream of [result.stdout, result.stderr]) {
    assert.ok(!stream.includes(SENTINEL), 'sentinel secret printed')
    assert.ok(!stream.includes('lin_value'), 'allowlisted value printed')
  }
  // No temp file is left behind in the destination directory.
  assert.deepEqual(fs.readdirSync(path.dirname(to)), ['.env'])
})

test('CLI replaces an existing destination and tightens its mode', () => {
  const dir = tempDir('write-worktree-env-')
  const from = path.join(dir, 'source.env')
  const to = path.join(dir, 'dest.env')
  fs.writeFileSync(from, 'CC=new\n')
  fs.writeFileSync(to, `FAKE_SECRET=${SENTINEL}\n`, { mode: 0o644 })
  fs.chmodSync(to, 0o644)

  const result = run(['--from', from, '--to', to, '--keys', 'CC'])
  assert.equal(result.status, 0, result.stderr)
  assert.equal(fs.readFileSync(to, 'utf8'), 'CC=new\n')
  assert.equal(fs.statSync(to).mode & 0o777, 0o600)
})

test('CLI filters a file in place when --from and --to are the same path', () => {
  const dir = tempDir('write-worktree-env-')
  const file = path.join(dir, '.env')
  fs.writeFileSync(file, FIXTURE)

  const result = run(['--from', file, '--to', file, '--keys', KEYS])
  assert.equal(result.status, 0, result.stderr)
  assert.equal(fs.readFileSync(file, 'utf8'), EXPECTED)
  assert.equal(fs.statSync(file).mode & 0o777, 0o600)
})

test('CLI skips a missing source, writes nothing, and exits 0', () => {
  const dir = tempDir('write-worktree-env-')
  const to = path.join(dir, '.env')
  const result = run(['--from', path.join(dir, 'absent.env'), '--to', to, '--keys', KEYS])
  assert.equal(result.status, 0, result.stderr)
  assert.match(result.stdout, /No \.env in .* — skipping/)
  assert.equal(fs.existsSync(to), false)
})

test('CLI warns about an invalid allowlist name and still writes the valid keys', () => {
  const dir = tempDir('write-worktree-env-')
  const from = path.join(dir, 'source.env')
  const to = path.join(dir, 'dest.env')
  fs.writeFileSync(from, `CC=kept\nBAD-KEY=${SENTINEL}\n`)

  const result = run(['--from', from, '--to', to, '--keys', 'CC,BAD-KEY'])
  assert.equal(result.status, 0, result.stderr)
  assert.match(result.stderr, /invalid allowlist key name "BAD-KEY"/)
  assert.equal(fs.readFileSync(to, 'utf8'), 'CC=kept\n')
  assert.ok(!result.stderr.includes(SENTINEL) && !result.stdout.includes(SENTINEL))
})

test('CLI rejects an unknown flag without writing', () => {
  const dir = tempDir('write-worktree-env-')
  const from = path.join(dir, 'source.env')
  const to = path.join(dir, 'dest.env')
  fs.writeFileSync(from, 'CC=x\n')

  const result = run(['--from', from, '--to', to, '--keys', 'CC', '--force'])
  assert.notEqual(result.status, 0)
  assert.match(result.stderr, /--force/)
  assert.equal(fs.existsSync(to), false)
})

test('CLI rejects a missing required flag without writing', () => {
  const dir = tempDir('write-worktree-env-')
  const from = path.join(dir, 'source.env')
  const to = path.join(dir, 'dest.env')
  fs.writeFileSync(from, 'CC=x\n')

  for (const args of [
    ['--to', to, '--keys', 'CC'],
    ['--from', from, '--keys', 'CC'],
    ['--from', from, '--to', to],
  ]) {
    const result = run(args)
    assert.notEqual(result.status, 0, `accepted ${args.join(' ')}`)
    assert.match(result.stderr, /required/)
    assert.equal(fs.existsSync(to), false)
  }
})
