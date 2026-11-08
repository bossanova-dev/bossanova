#!/usr/bin/env node
import assert from 'node:assert/strict'
import test from 'node:test'

import { tempDir } from '../../scripts/test-infra/test-tmpdir.mjs'
import { AUTOMATION_GRACE, applyGrace, collect, evaluateGate } from './orchestrate-snapshot.mjs'

const NOW = Date.parse('2026-11-07T09:00:00Z')
const stoppedChat = { status: 'UNSPECIFIED', spinner_present: false, waiting_reason: '' }
const session = (id, over = {}) => ({
  id,
  agent: 'claude',
  state: 'READY_FOR_REVIEW',
  last_agent_activity_at: '2026-11-07T08:55:00Z',
  ...over,
})
const prState = (number, over = {}) => ({
  pr_number: number,
  state: 'open',
  is_draft: false,
  is_mergeable: true,
  merge_state_status: 'clean',
  has_build_receipt: true,
  check_verdict: { state: 'green', failed_count: 0, pending_count: 0 },
  verify_phase: { kind: 'none', reason: '', label: '' },
  checks: [{ name: 'boss/verify', conclusion: 'success' }],
  ...over,
})
const policyPark = { verify_phase: { kind: 'needs_human', reason: 'human-review:ui', label: '' } }
const conflict = { merge_state_status: 'dirty', is_mergeable: false }

// A daemon whose sessions are `facts` ({ session, pr, chats }). It answers the full, PR-only and
// detail reads the way the on-host dump does, and records which sessions each read covered.
function fakeDaemon(facts) {
  const reads = []
  const dump = (_host, _where, { mode = 'full', ids = [] } = {}) => {
    const covered = facts.filter((fact) => mode !== 'detail' || ids.includes(fact.session.id))
    reads.push({ mode, ids: covered.map((fact) => fact.session.id) })
    return covered.map((fact) =>
      mode === 'pr'
        ? { session: fact.session, chats: [], pr: fact.pr, repo: 'acme/app', prOnly: true }
        : {
            session: fact.session,
            chats: fact.chats ?? [stoppedChat],
            pr: fact.pr,
            repo: 'acme/app',
          },
    )
  }
  return { dump, reads }
}

const emptyQueue = { list: () => [] }
const gate = (t, dump, over = {}) =>
  evaluateGate({
    hosts: [],
    local: true,
    bossBin: 'boss',
    now: NOW,
    root: tempDir('orchestrate-gate-', t),
    dump,
    crons: () => [],
    seen: {},
    client: emptyQueue,
    ...over,
  })

test('the quick read reads chats only for sessions the PR state does not settle', (t) => {
  const daemon = fakeDaemon([
    { session: session('parked'), pr: prState(1, policyPark) },
    { session: session('verified'), pr: prState(2) },
    { session: session('conflicted'), pr: prState(3, conflict) },
    // Blocked before its PR was handed off: only the full read can tell a live chat from a stop.
    { session: session('blocked', { state: 'BLOCKED', blocked_reason: 'x' }), pr: null },
  ])
  const snapshot = collect({
    hosts: [],
    local: true,
    bossBin: 'boss',
    now: NOW,
    root: tempDir('orchestrate-gate-', t),
    dump: daemon.dump,
    crons: () => [],
    quick: true,
  })
  assert.deepEqual(daemon.reads, [
    { mode: 'pr', ids: ['parked', 'verified', 'conflicted', 'blocked'] },
    { mode: 'detail', ids: ['conflicted', 'blocked'] },
  ])
  // The quick read classifies every row the way the full read does.
  const full = collect({
    hosts: [],
    local: true,
    bossBin: 'boss',
    now: NOW,
    root: tempDir('orchestrate-gate-', t),
    dump: daemon.dump,
    crons: () => [],
  })
  assert.deepEqual(
    snapshot.rows.map((row) => [row.session, row.action]),
    full.rows.map((row) => [row.session, row.action]),
  )
})

test('a fresh conflict keeps the gate shut until the grace window passes', (t) => {
  const { dump } = fakeDaemon([{ session: session('c'), pr: prState(3, conflict) }])
  const seen = {}
  assert.equal(gate(t, dump, { seen }).exit, 1)
  const later = gate(t, dump, { seen, now: NOW + AUTOMATION_GRACE })
  assert.equal(later.exit, 0)
  assert.match(later.reason, /repair/)
})

test('a policy park with no queued question wakes the worker to queue it', (t) => {
  const { dump } = fakeDaemon([{ session: session('p'), pr: prState(1, policyPark) }])
  const verdict = gate(t, dump)
  assert.equal(verdict.exit, 0)
  assert.match(verdict.reason, /queue/)
})

test('an idle factory keeps the gate shut', (t) => {
  const { dump } = fakeDaemon([{ session: session('v'), pr: prState(2) }])
  assert.equal(gate(t, dump).exit, 1)
})

test('an unreachable host or an unreadable queue means the gate could not evaluate', (t) => {
  const down = () => {
    throw new Error('ssh: connect to host build-host: Connection refused')
  }
  const unreachable = gate(t, down, { hosts: ['build-host'], local: false })
  assert.equal(unreachable.exit, 69)
  assert.match(unreachable.reason, /build-host/)
  const { dump } = fakeDaemon([])
  const broken = {
    list: () => {
      throw new Error('boss notes: daemon not running')
    },
  }
  assert.equal(gate(t, dump, { client: broken }).exit, 69)
})

test('a host that could not be read keeps its grace clocks', () => {
  const seen = { 'build-host/s1/repair': NOW - 10 * 60 * 1000 }
  applyGrace([], seen, NOW, ['build-host'])
  assert.deepEqual(Object.keys(seen), ['build-host/s1/repair'])
  applyGrace([], seen, NOW)
  assert.deepEqual(Object.keys(seen), [])
})

// With `respectOwners`, one orchestrator per daemon: a daemon whose own cron list runs this skill
// `--unattended` is that worker's, and an attended collect leaves it alone. Without it, the one
// attended orchestrator covers every daemon.
const worker = { name: 'orchestrate-worker', prompt: '/bs-orchestrate --unattended', enabled: true }
const local = [{ session: session('mine'), pr: prState(4, conflict) }]
const remote = [{ session: session('theirs'), pr: prState(5, conflict) }]

// Daemons keyed by SSH host ('' is this machine). A cron list given as an Error cannot be read.
function daemons(facts, cronLists) {
  const read = []
  const dump = (host, where, scope) => {
    read.push(host)
    return fakeDaemon(facts[host]).dump(host, where, scope)
  }
  const crons = (host) => {
    const jobs = cronLists[host] ?? []
    if (jobs instanceof Error) throw jobs
    return jobs
  }
  return { dump, crons, read }
}

const attended = (t, fleet, over = {}) =>
  collect({
    hosts: ['build-host'],
    local: true,
    bossBin: 'boss',
    now: NOW,
    root: tempDir('orchestrate-owner-', t),
    dump: fleet.dump,
    crons: fleet.crons,
    respectOwners: true,
    ...over,
  })
const rowHosts = (snapshot) => snapshot.rows.map((row) => [row.host, row.session])

test('by default an attended collect covers every daemon, worker cron or not', (t) => {
  const down = new Error('ssh: connect to host build-host: Connection timed out')
  for (const cronList of [[worker], down]) {
    const fleet = daemons(
      { '': local, 'build-host': remote },
      { '': [worker], 'build-host': cronList },
    )
    const snapshot = attended(t, fleet, { respectOwners: undefined })
    assert.deepEqual(rowHosts(snapshot), [
      ['local', 'mine'],
      ['build-host', 'theirs'],
    ])
    assert.deepEqual(snapshot.owners, [])
    assert.deepEqual(snapshot.errors, [])
    assert.deepEqual(fleet.read, ['', 'build-host'])
  }
})

test('an attended collect leaves a daemon with a worker cron to that worker', (t) => {
  const fleet = daemons({ '': local, 'build-host': remote }, { 'build-host': [worker] })
  const seen = { 'build-host/theirs/repair': NOW - 60 * 60 * 1000 }
  const snapshot = attended(t, fleet, { seen })
  assert.deepEqual(rowHosts(snapshot), [['local', 'mine']])
  assert.deepEqual(snapshot.owners, [{ host: 'build-host', cron: 'orchestrate-worker' }])
  assert.deepEqual(fleet.read, [''])
  assert.deepEqual(snapshot.errors, [])
  // The worker's grace clocks survive an attended round that shares its state file.
  assert.ok('build-host/theirs/repair' in snapshot.seen)
})

test('a daemon whose cron list runs no unattended worker is the attended chat’s', (t) => {
  const notWorkers = [
    { ...worker, enabled: false },
    { ...worker, prompt: '/bs-orchestrate --host build-host' },
    { ...worker, prompt: '/bs-orchestrate-extra --unattended' },
    { ...worker, prompt: '/boss-verify --unattended' },
  ]
  const fleet = daemons({ '': local, 'build-host': remote }, { 'build-host': notWorkers })
  const snapshot = attended(t, fleet)
  assert.deepEqual(rowHosts(snapshot), [
    ['local', 'mine'],
    ['build-host', 'theirs'],
  ])
  assert.deepEqual(snapshot.owners, [])
})

test('the worker classifies its own daemon', (t) => {
  const codexWorker = { ...worker, prompt: '$bs-orchestrate --unattended' }
  const fleet = daemons({ '': local }, { '': [codexWorker] })
  assert.deepEqual(rowHosts(attended(t, fleet, { hosts: [] })), [])
  const own = attended(t, fleet, { hosts: [], unattended: true })
  assert.deepEqual(rowHosts(own), [['local', 'mine']])
  assert.deepEqual(own.owners, [])
  // The worker's own gate opens on its own daemon's work.
  const opened = gate(t, fleet.dump, {
    crons: fleet.crons,
    respectOwners: true,
    now: NOW + AUTOMATION_GRACE,
    seen: { 'local/mine/repair': NOW },
  })
  assert.equal(opened.exit, 0)
})

test('a daemon whose cron list cannot be read is left alone and reported', (t) => {
  const down = new Error('ssh: connect to host build-host: Connection timed out')
  const fleet = daemons({ '': local, 'build-host': remote }, { 'build-host': down })
  const snapshot = attended(t, fleet)
  assert.deepEqual(rowHosts(snapshot), [['local', 'mine']])
  assert.deepEqual(snapshot.owners, [])
  assert.deepEqual(snapshot.unread, ['build-host'])
  assert.equal(snapshot.errors.length, 1)
  assert.match(snapshot.errors[0], /build-host.*cron list.*timed out/)
  assert.deepEqual(fleet.read, [''])
})
