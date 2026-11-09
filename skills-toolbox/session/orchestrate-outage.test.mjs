#!/usr/bin/env node
import assert from 'node:assert/strict'
import fs from 'node:fs'
import test from 'node:test'

import { tempDir } from '../../scripts/test-infra/test-tmpdir.mjs'
import {
  holdRows,
  outageFile,
  parseRefs,
  pause,
  readOutage,
  resume,
  run,
  status,
} from './orchestrate-outage.mjs'

// One daemon per host, as `boss [--host h] cron|chats|chat` sees it. `calls` records every write.
function fakeFactory() {
  const daemons = {
    '': {
      crons: [
        { id: 'c-build', name: 'nightly-build', prompt: '/boss-build', enabled: true },
        { id: 'c-plan', name: 'plan-sweep', prompt: '/boss-plan', enabled: true },
        { id: 'c-verify', name: 'verify-sweep', prompt: '/boss-verify', enabled: true },
        { id: 'c-off', name: 'old-build', prompt: '/boss-build', enabled: false },
      ],
      chats: {
        s1: [
          { agent_session_id: 'old', status: 'IDLE', created_at: '2026-11-08T08:00:00Z' },
          {
            agent_session_id: 'live',
            status: 'WORKING',
            spinner_present: true,
            created_at: '2026-11-08T09:00:00Z',
          },
        ],
      },
    },
    remote: {
      crons: [{ id: 'r-build', name: 'build-queue', prompt: '/boss-build', enabled: true }],
      chats: {
        s2: [{ agent_session_id: 'idle', status: 'IDLE', created_at: '2026-11-08T07:00Z' }],
      },
    },
  }
  const calls = []
  const runBoss = (args) => {
    let host = ''
    let rest = args
    if (args[0] === '--host') [host, rest] = [args[1], args.slice(2)]
    const daemon = daemons[host]
    const key = rest.slice(0, 2).join(' ')
    if (key === 'cron ls') return JSON.stringify(daemon.crons)
    if (rest[0] === 'chats') return JSON.stringify({ chats: daemon.chats[rest[2]] ?? [] })
    calls.push([host, ...rest])
    if (key === 'cron disable' || key === 'cron enable') {
      const cron = daemon.crons.find((c) => c.id === rest[2])
      cron.enabled = rest[1] === 'enable'
    }
    return '{}'
  }
  return { daemons, calls, runBoss }
}

const up = (url) => ({ url, status: 200, ok: true })
const down = (url) => ({ url, status: 504, ok: false })

test('pause turns off only the enabled build and plan crons and holds the live chats', (t) => {
  const dir = tempDir('outage-', t)
  const factory = fakeFactory()
  const refs = parseRefs({ sessions: ['s1', 's2@remote'], runs: ['acme/app#77'] })
  const result = pause({
    dir,
    reason: 'GitHub release downloads return 504',
    hosts: ['', 'remote'],
    probes: ['https://example.test/asset'],
    runBoss: factory.runBoss,
    ...refs,
  })
  assert.deepEqual(result.disabled.sort(), [
    'local/nightly-build',
    'local/plan-sweep',
    'remote/build-queue',
  ])
  const sends = factory.calls.filter((call) => call[1] === 'chat')
  // Only the live chat is told to hold; idle chats are not woken into the outage.
  assert.deepEqual(
    sends.map((call) => [call[0], call[4]]),
    [['', 'live']],
  )
  assert.match(sends[0][5], /Hold: do not repair, review or rerun/)
  const record = readOutage(dir)
  assert.deepEqual(record.crons.map((c) => `${c.host}/${c.id}`).sort(), [
    '/c-build',
    '/c-plan',
    'remote/r-build',
  ])
  assert.deepEqual(record.runs, [{ repo: 'acme/app', run: '77' }])
  assert.equal(factory.daemons[''].crons.find((c) => c.id === 'c-verify').enabled, true)
})

test('pausing again adds nothing twice and re-disables a recorded cron that came back on', (t) => {
  const dir = tempDir('outage-', t)
  const factory = fakeFactory()
  const once = {
    dir,
    reason: 'CI outage',
    sessions: [{ session: 's1', host: '' }],
    runBoss: factory.runBoss,
  }
  pause(once)
  factory.daemons[''].crons.find((c) => c.id === 'c-build').enabled = true
  factory.calls.length = 0
  pause({ ...once, reason: 'another reason' })
  assert.deepEqual(factory.calls, [['', 'cron', 'disable', 'c-build']])
  const record = readOutage(dir)
  assert.equal(record.reason, 'CI outage')
  assert.equal(record.crons.length, 2)
  assert.equal(record.sessions.length, 1)
})

test('resume waits for recovery, then undoes exactly what pause did and clears the record', (t) => {
  const dir = tempDir('outage-', t)
  const factory = fakeFactory()
  pause({
    dir,
    reason: 'CI outage',
    hosts: ['', 'remote'],
    probes: ['https://example.test/asset'],
    runBoss: factory.runBoss,
    ...parseRefs({ sessions: ['s1', 's2@remote'], runs: ['acme/app#77'] }),
  })
  const reruns = []
  const runGh = (args) => {
    reruns.push(args)
    return ''
  }
  const early = resume({ dir, runBoss: factory.runBoss, runGh, probe: down })
  assert.equal(early.resumed, false)
  assert.match(early.reason, /still down: https:\/\/example.test\/asset \(504\)/)
  assert.deepEqual(reruns, [])
  assert.equal(status({ dir, probe: down }).recovered, false)

  factory.calls.length = 0
  const done = resume({ dir, runBoss: factory.runBoss, runGh, probe: up })
  assert.equal(done.resumed, true)
  const crons = factory.daemons
  assert.equal(crons[''].crons.find((c) => c.id === 'c-build').enabled, true)
  assert.equal(crons[''].crons.find((c) => c.id === 'c-plan').enabled, true)
  assert.equal(crons.remote.crons[0].enabled, true)
  // A cron that was already off before the outage stays off.
  assert.equal(crons[''].crons.find((c) => c.id === 'c-off').enabled, false)
  assert.deepEqual(reruns, [['run', 'rerun', '77', '--failed', '-R', 'acme/app']])
  // Every held session's newest chat is told to continue, idle or not.
  const told = factory.calls
    .filter((call) => call[1] === 'chat')
    .map((call) => `${call[0]}/${call[4]}`)
  assert.deepEqual(told, ['/live', 'remote/idle'])
  assert.equal(fs.existsSync(outageFile(dir)), false)
})

test('a resume that fails partway keeps only what is left, so running it again finishes', (t) => {
  const dir = tempDir('outage-', t)
  const factory = fakeFactory()
  pause({
    dir,
    reason: 'CI outage',
    runBoss: factory.runBoss,
    ...parseRefs({ runs: ['acme/app#1', 'acme/app#2'] }),
  })
  const failing = (args) => {
    if (args[2] === '2') throw new Error('HTTP 502 from api.github.com')
    return ''
  }
  const partial = resume({ dir, runBoss: factory.runBoss, runGh: failing, probe: up })
  assert.equal(partial.resumed, false)
  assert.equal(partial.errors.length, 1)
  assert.deepEqual(readOutage(dir).runs, [{ repo: 'acme/app', run: '2' }])
  assert.deepEqual(readOutage(dir).crons, [])
  // A run an earlier resume already restarted counts as rerun.
  const again = resume({
    dir,
    runBoss: factory.runBoss,
    runGh: () => {
      throw new Error('run 2 cannot be rerun; This workflow is already running')
    },
    probe: up,
  })
  assert.equal(again.resumed, true)
  assert.equal(readOutage(dir), null)
})

test('held sessions read as held in the snapshot until the outage ends', () => {
  const rows = [
    { host: 'local', session: 's1', action: 'repair', why: '2 failing check(s)', next: 'x' },
    { host: 'remote', session: 's2', action: 'done', why: 'verified', next: '' },
    { host: 'remote', session: 's3', action: 'stalled-chat', why: 'idle', next: 'y' },
  ]
  const record = {
    reason: 'CI outage',
    sessions: [
      { host: '', session: 's1' },
      { host: 'remote', session: 's2' },
    ],
  }
  holdRows(rows, record)
  assert.deepEqual(
    rows.map((row) => [row.session, row.action, row.next]),
    [
      ['s1', 'held', ''],
      ['s2', 'done', ''],
      ['s3', 'stalled-chat', 'y'],
    ],
  )
})

test('the CLI pauses, reports and resumes through one record', (t) => {
  const dir = tempDir('outage-', t)
  const factory = fakeFactory()
  const out = []
  const io = {
    write: (text) => out.push(text),
    runBoss: factory.runBoss,
    runGh: () => '',
    probe: up,
  }
  assert.equal(run(['pause', '--state-dir', dir], io), 1)
  assert.equal(
    run(['pause', '--state-dir', dir, '--reason', 'CI outage', '--run', 'acme/app#5'], io),
    0,
  )
  assert.equal(run(['status', '--state-dir', dir], io), 0)
  assert.equal(JSON.parse(out.at(-1)).open, true)
  assert.equal(run(['resume', '--state-dir', dir], io), 0)
  assert.equal(run(['resume', '--state-dir', dir], io), 3)
  assert.equal(JSON.parse(out.at(-1)).reason, 'no outage is recorded')
})
