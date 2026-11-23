#!/usr/bin/env node
import assert from 'node:assert/strict'
import { spawn } from 'node:child_process'
import fs from 'node:fs'
import path from 'node:path'
import test from 'node:test'
import { setTimeout as delay } from 'node:timers/promises'
import { fileURLToPath } from 'node:url'

import { tempDir } from '../../scripts/test-infra/test-tmpdir.mjs'
import { collect } from './orchestrate-snapshot.mjs'

const SNAPSHOT = fileURLToPath(new URL('./orchestrate-snapshot.mjs', import.meta.url))
const NOW = Date.parse('2026-11-07T09:00:00Z')
// The collection budget handed to the code under test: long enough for the fast calls to land on
// a loaded host, while the hung `boss chats` call can only end when the budget does.
const DUMP_DEADLINE_MS = 5000
// node:test hang guards. Without a fix the remote dump outlives its caller by a whole per-call
// timeout (90 s) and then keeps issuing calls.
const HANG_GUARD = { timeout: 60_000 }

// A boss whose `chats` call never returns, like a daemon at 300% CPU. Every invocation appends
// `{cmd, pid, ppid}` to `log`; the dump process is the fake's parent.
const slowBoss = (log) => `#!/usr/bin/env node
const fs = require('node:fs')
const [cmd, , id] = process.argv.slice(2)
fs.appendFileSync(${JSON.stringify(log)}, JSON.stringify({ cmd, pid: process.pid, ppid: process.ppid }) + '\\n')
if (cmd === 'chats') setInterval(() => {}, 1 << 30)
else {
  const out = {
    cron: [],
    ls: { sessions: [{ id: 's1', state: 'IMPLEMENTING_PLAN' }, { id: 's2', state: 'IMPLEMENTING_PLAN' }] },
    show: { session: { id, state: 'IMPLEMENTING_PLAN' } },
    notes: [],
  }[cmd]
  process.stdout.write(JSON.stringify(out))
}
`

function fakeBoss(t) {
  const dir = tempDir('orchestrate-dump-', t)
  const log = path.join(dir, 'calls.jsonl')
  const boss = path.join(dir, 'boss')
  fs.writeFileSync(boss, slowBoss(log), { mode: 0o755 })
  const calls = () =>
    fs.existsSync(log)
      ? fs
          .readFileSync(log, 'utf8')
          .split('\n')
          .filter(Boolean)
          .map((line) => JSON.parse(line))
      : []
  // A failing run must not leave its dump or hung fakes behind.
  t.after(() => {
    for (const call of calls())
      for (const pid of [call.pid, call.ppid])
        if (pid !== process.pid && alive(pid)) process.kill(pid, 'SIGKILL')
  })
  return { dir, boss, calls }
}

const alive = (pid) => {
  try {
    process.kill(pid, 0)
    return true
  } catch {
    return false
  }
}

test(
  'a collection past its deadline stops calling boss and returns what it read',
  HANG_GUARD,
  (t) => {
    const { dir, boss, calls } = fakeBoss(t)
    const result = collect({
      hosts: [],
      local: true,
      bossBin: boss,
      now: NOW,
      root: dir,
      deadlineMs: DUMP_DEADLINE_MS,
    })
    const made = calls()
    // Both sessions reached their hung `chats` call; nothing was issued after the deadline ended it.
    assert.deepEqual(made.map((call) => call.cmd).sort(), ['chats', 'chats', 'cron', 'ls'])
    assert.deepEqual(
      made.filter((call) => alive(call.pid)),
      [],
    )
    assert.deepEqual(result.rows.map((row) => [row.session, row.unread === true]).sort(), [
      ['s1', true],
      ['s2', true],
    ])
    // A cut-short host keeps its grace clocks, like an unreachable one.
    assert.deepEqual(result.unread, ['local'])
    assert.ok(result.errors.length > 0)
  },
)

test('a collection whose caller dies stops, and stops its boss calls', HANG_GUARD, async (t) => {
  const { dir, boss, calls } = fakeBoss(t)
  const caller = spawn(
    process.execPath,
    [SNAPSHOT, 'collect', '--json', '--boss', boss, '--root', dir, '--state', path.join(dir, 's')],
    { stdio: 'ignore' },
  )
  const exited = new Promise((resolve) => caller.once('exit', resolve))
  t.after(() => caller.kill('SIGKILL'))
  let hung = []
  while (hung.length < 2) {
    await delay(50)
    hung = calls().filter((call) => call.cmd === 'chats')
  }
  const dump = hung[0].ppid
  assert.notEqual(dump, caller.pid)
  // The caller goes away the way `timeout` ends it; the dump it spawned is left behind.
  caller.kill('SIGTERM')
  await exited
  while (alive(dump) || hung.some((call) => alive(call.pid))) await delay(50)
  assert.deepEqual(
    calls().filter((call) => call.cmd === 'show' || call.cmd === 'notes'),
    [],
  )
})
