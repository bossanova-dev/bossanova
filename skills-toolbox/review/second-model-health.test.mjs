import assert from 'node:assert/strict'
import { spawn, spawnSync } from 'node:child_process'
import fs from 'node:fs'
import path from 'node:path'
import { test } from 'node:test'
import { fileURLToPath } from 'node:url'

import { tempDir } from '../../scripts/test-infra/test-tmpdir.mjs'

const SCRIPT = fileURLToPath(new URL('./second-model-health.mjs', import.meta.url))
const BASE = 'a'.repeat(40)
const NOT_AUTHED =
  'claude-review: not_authed: no Claude credential is visible to this process; credentials: present=none'

// A sandbox: a fake `boss` that logs each `notes add` argv as one JSON line, a private state dir,
// and a cwd with no skill config. Nothing touches the real HOME or notes store.
function sandbox(t) {
  const dir = tempDir('second-model-health-', t)
  const notesLog = path.join(dir, 'notes.jsonl')
  const boss = path.join(dir, 'boss')
  fs.writeFileSync(
    boss,
    `#!/usr/bin/env node
require('node:fs').appendFileSync(${JSON.stringify(notesLog)}, JSON.stringify(process.argv.slice(2)) + '\\n')
process.stdout.write('{"id":"note-1"}\\n')
`,
  )
  fs.chmodSync(boss, 0o755)
  const cwd = path.join(dir, 'repo')
  fs.mkdirSync(cwd)
  const env = {
    PATH: process.env.PATH,
    HOME: path.join(dir, 'home'),
    BOSS_BIN: boss,
    BOSS_SECOND_MODEL_HEALTH_DIR: path.join(dir, 'state'),
  }
  let n = 0
  const verdictFile = (secondModel) => {
    n += 1
    const file = path.join(dir, `verdict-${n}.json`)
    const head = n.toString(16).padStart(40, '0')
    fs.writeFileSync(
      file,
      JSON.stringify({ version: 1, status: 'pass', base: BASE, head, findings: [], secondModel }),
    )
    return file
  }
  const notes = () =>
    fs.existsSync(notesLog)
      ? fs
          .readFileSync(notesLog, 'utf8')
          .trim()
          .split('\n')
          .map((line) => JSON.parse(line))
      : []
  const record = (secondModel, extraEnv = {}) => {
    const res = spawnSync(process.execPath, [SCRIPT, 'record', verdictFile(secondModel)], {
      cwd,
      env: { ...env, ...extraEnv },
      encoding: 'utf8',
    })
    assert.equal(res.status, 0, res.stderr)
    return JSON.parse(res.stdout)
  }
  const recordAsync = (secondModel) =>
    new Promise((resolve, reject) => {
      const child = spawn(process.execPath, [SCRIPT, 'record', verdictFile(secondModel)], {
        cwd,
        env,
      })
      let out = ''
      child.stdout.on('data', (chunk) => (out += chunk))
      child.on('error', reject)
      child.on('close', (code) => (code === 0 ? resolve(JSON.parse(out)) : reject(new Error(out))))
    })
  return { cwd, notes, record, recordAsync }
}

const tagsOf = (argv) => argv.flatMap((arg, i) => (argv[i - 1] === '--tag' ? [arg] : []))
const bodyOf = (argv) => argv[argv.indexOf('--') + 1]
const failed = { ran: false, reason: NOT_AUTHED }

test('three failures record one health fact; a fourth records none; a success resets and recovers', (t) => {
  const box = sandbox(t)
  assert.equal(box.record(failed).count, 1)
  assert.equal(box.record(failed).count, 2)
  assert.deepEqual(box.notes(), [])
  const third = box.record(failed)
  assert.equal(third.count, 3)
  assert.equal(third.noted, true)
  assert.equal(box.notes().length, 1)
  const [fact] = box.notes()
  assert.deepEqual(tagsOf(fact), ['health', 'second-model'])
  assert.match(bodyOf(fact), /\bclaude not_authed since \d{4}-\d\d-\d\dT[\d:.]+Z\b/)
  assert.ok(fact.includes('--idempotency-key'))

  assert.equal(box.record(failed).noted, false)
  assert.equal(box.notes().length, 1)

  const reset = box.record({ ran: true, agent: 'claude' })
  assert.equal(reset.action, 'reset')
  assert.equal(reset.noted, true)
  const recovered = box.notes()[1]
  assert.deepEqual(tagsOf(recovered), ['health', 'second-model', 'recovered'])
  assert.match(bodyOf(recovered), /claude ran again/)

  // The streak starts over: two more failures stay below the threshold.
  assert.equal(box.record(failed).count, 1)
  assert.equal(box.record(failed).count, 2)
  assert.equal(box.notes().length, 2)
})

test('a success with no recorded fact resets silently', (t) => {
  const box = sandbox(t)
  box.record(failed)
  assert.deepEqual(box.record({ ran: true, agent: 'claude' }), {
    action: 'reset',
    noted: false,
    agent: 'claude',
    count: 0,
  })
  assert.equal(box.record(failed).count, 1)
  assert.deepEqual(box.notes(), [])
})

test('configured-off and BOSS_BS_REVIEW=0 runs neither advance nor reset the streak', (t) => {
  const box = sandbox(t)
  box.record(failed)
  box.record(failed)
  assert.equal(
    box.record({ ran: true, agent: 'claude' }, { BOSS_BS_REVIEW: '0' }).action,
    'ignored',
  )
  assert.equal(box.record(failed, { BOSS_BS_REVIEW: '0' }).action, 'ignored')
  fs.writeFileSync(
    path.join(box.cwd, '.boss-skills.json'),
    JSON.stringify({ review: { secondModel: false } }),
  )
  assert.equal(box.record({ ran: true, agent: 'claude' }).reason, 'second model configured off')
  assert.equal(box.record(failed).reason, 'second model configured off')
  fs.rmSync(path.join(box.cwd, '.boss-skills.json'))
  // Still at two: the next failure is the third and records the fact.
  assert.equal(box.record(failed).count, 3)
  assert.equal(box.notes().length, 1)
})

test('BOSS_NOTES_SUPPRESSED=1 records nothing', (t) => {
  const box = sandbox(t)
  for (let i = 0; i < 4; i += 1) box.record(failed, { BOSS_NOTES_SUPPRESSED: '1' })
  box.record({ ran: true, agent: 'claude' }, { BOSS_NOTES_SUPPRESSED: '1' })
  assert.deepEqual(box.notes(), [])
})

test('concurrent records on one host lose no increment and record the fact once', async (t) => {
  const box = sandbox(t)
  box.record(failed)
  const pair = await Promise.all([box.recordAsync(failed), box.recordAsync(failed)])
  assert.deepEqual(pair.map((r) => r.count).sort(), [2, 3])
  const more = await Promise.all(Array.from({ length: 5 }, () => box.recordAsync(failed)))
  assert.deepEqual(
    more.map((r) => r.count).sort((a, b) => a - b),
    [4, 5, 6, 7, 8],
  )
  assert.equal(box.notes().length, 1)
})

test('streaks are per agent, and transient or unattributed skips leave them alone', (t) => {
  const box = sandbox(t)
  box.record(failed)
  box.record(failed)
  // A Claude session's codex second model running fine does not reset the claude streak.
  assert.equal(box.record({ ran: true, agent: 'codex' }).agent, 'codex')
  for (const reason of ['soft budget spent', 'timed out', 'not recorded', 'not_authed: no prefix'])
    assert.equal(box.record({ ran: false, reason }).action, 'ignored', reason)
  assert.equal(
    box.record({ ran: false, reason: 'codex-review: not_installed: install codex' }).count,
    1,
  )
  assert.equal(box.record(failed).count, 3)
  assert.equal(box.notes().length, 1)
  assert.match(bodyOf(box.notes()[0]), /\bclaude not_authed\b/)
})

test('a failed note write is never fatal and is retried on the next failure', (t) => {
  const box = sandbox(t)
  for (let i = 0; i < 2; i += 1) box.record(failed)
  const broken = box.record(failed, { BOSS_BIN: '/nonexistent/boss', PATH: '' })
  assert.equal(broken.count, 3)
  assert.equal(broken.noted, false)
  assert.ok(broken.noteError)
  assert.equal(box.record(failed).noted, true)
  assert.equal(box.notes().length, 1)
})

test('unreadable verdicts and bad arguments never throw', (t) => {
  const box = sandbox(t)
  const res = spawnSync(process.execPath, [SCRIPT, 'record', path.join(box.cwd, 'missing.json')], {
    encoding: 'utf8',
    cwd: box.cwd,
  })
  assert.equal(res.status, 0)
  assert.equal(JSON.parse(res.stdout).action, 'error')
  for (const argv of [
    [],
    ['record'],
    ['bogus', 'x'],
    ['record', 'a', 'b'],
    ['record', '--x', 'a'],
  ]) {
    const bad = spawnSync(process.execPath, [SCRIPT, ...argv], { encoding: 'utf8', cwd: box.cwd })
    assert.equal(bad.status, 2, argv.join(' '))
    assert.match(bad.stderr, /usage:/)
  }
})
