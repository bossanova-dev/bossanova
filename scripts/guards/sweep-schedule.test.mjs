import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import test from 'node:test'
import { fileURLToPath } from 'node:url'

const cli = fileURLToPath(new URL('./sweep-schedule.mjs', import.meta.url))
function fixture(
  t,
  crons = [{ name: 'sample', schedule: '@daily', prompt: '/sample', agent: 'codex' }],
) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'sweep-schedule-'))
  t.after(() => fs.rmSync(dir, { recursive: true, force: true, maxRetries: 5 }))
  const config = path.join(dir, 'config.json')
  const out = path.join(dir, 'schedule.md')
  fs.writeFileSync(config, JSON.stringify({ crons }))
  const run = (mode) =>
    spawnSync(process.execPath, [cli, mode, '--config', config, '--out', out], { encoding: 'utf8' })
  return { config, out, run }
}
test('--check passes after --write and writes are idempotent', (t) => {
  const { out, run } = fixture(t)
  assert.equal(run('--write').status, 0)
  const first = fs.readFileSync(out)
  assert.equal(run('--check').status, 0)
  assert.equal(run('--write').status, 0)
  assert.deepEqual(fs.readFileSync(out), first)
})
test('--check fails after an output edit', (t) => {
  const { out, run } = fixture(t)
  assert.equal(run('--write').status, 0)
  fs.appendFileSync(out, '\nAn edit.\n')
  const result = run('--check')
  assert.equal(result.status, 1)
  assert.match(result.stderr, /^-/m)
})
test('--check fails after adding a cron without regenerating', (t) => {
  const { config, run } = fixture(t)
  assert.equal(run('--write').status, 0)
  const data = JSON.parse(fs.readFileSync(config, 'utf8'))
  data.crons.push({ name: 'another', schedule: '@hourly' })
  fs.writeFileSync(config, JSON.stringify(data))
  assert.equal(run('--check').status, 1)
  assert.equal(run('--write').status, 0)
  assert.equal(run('--check').status, 0)
})
test('factory stages and unknown schedules render without a schedule parser error', (t) => {
  const { out, run } = fixture(t, [
    { name: 'factory', factoryStage: 'verify' },
    { name: 'unknown', schedule: 'unparseable' },
  ])
  assert.equal(run('--write').status, 0)
  const output = fs.readFileSync(out, 'utf8')
  assert.match(output, /factory stage verify/)
  assert.match(output, /custom/)
})
test('renders derived cadence and explicit configuration fields in name order', (t) => {
  const { out, run } = fixture(t, [
    { name: 'f-monthly', schedule: '0 0 1 * *', enabled: false },
    { name: 'e-weekly', schedule: '@weekly' },
    { name: 'd-daily', schedule: '@daily' },
    { name: 'c-hourly', schedule: '@hourly' },
    {
      name: 'b-minutes',
      schedule: '*/15 * * * *',
      timezone: 'UTC',
      gateCommand: 'check',
      zeroOutput: true,
      enabled: true,
    },
    { name: 'a-custom', schedule: '0 0 1,15 * *' },
  ])
  assert.equal(run('--write').status, 0)
  const rows = fs
    .readFileSync(out, 'utf8')
    .split('\n')
    .filter((line) => /^\| [a-f]-/.test(line))
    .map((line) =>
      line
        .split('|')
        .slice(1, -1)
        .map((value) => value.trim()),
    )
  assert.deepEqual(
    rows.map((row) => row.slice(0, 2)),
    [
      ['a-custom', 'custom'],
      ['b-minutes', 'every 15 minutes'],
      ['c-hourly', 'hourly'],
      ['d-daily', 'daily'],
      ['e-weekly', 'weekly'],
      ['f-monthly', 'monthly'],
    ],
  )
  assert.deepEqual(rows[1].slice(3), ['UTC', '—', '—', 'yes', 'yes', 'yes'])
  assert.deepEqual(rows[5].slice(3), ['daemon-local', '—', '—', 'no', 'no', 'no'])
})
