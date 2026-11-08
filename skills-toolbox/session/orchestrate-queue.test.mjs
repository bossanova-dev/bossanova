#!/usr/bin/env node
import assert from 'node:assert/strict'
import test from 'node:test'

import { gateVerdict, notesClient, planQueue, syncQueue } from './orchestrate-queue.mjs'

// An in-memory bossd notes store behind the `boss notes` argv the queue sends.
function fakeBoss() {
  const notes = []
  let next = 1
  const flag = (args, name) => {
    const i = args.indexOf(name)
    return i === -1 ? undefined : args[i + 1]
  }
  const flags = (args, name) => args.flatMap((arg, i) => (arg === name ? [args[i + 1]] : []))
  const run = (args) => {
    assert.equal(args[0], 'notes')
    const [, verb] = args
    if (verb === 'ls') {
      const tags = flags(args, '--tag')
      return JSON.stringify(notes.filter((n) => n.tags.some((tag) => tags.includes(tag))))
    }
    if (verb === 'add') {
      const key = flag(args, '--idempotency-key')
      const existing = key && notes.find((n) => n.key === key)
      if (existing) return JSON.stringify(existing)
      const body = args[args.indexOf('--') + 1]
      const note = { id: `n${next++}`, body, tags: flags(args, '--tag'), key }
      notes.push(note)
      return JSON.stringify(note)
    }
    if (verb === 'edit') {
      const note = notes.find((n) => n.id === args[2])
      assert.ok(note, `edit of unknown note ${args[2]}`)
      const body = flag(args, '--body')
      if (body !== undefined) note.body = body
      if (args.includes('--tag')) note.tags = flags(args, '--tag')
      return JSON.stringify(note)
    }
    if (verb === 'rm') {
      const at = notes.findIndex((n) => n.id === args[2])
      assert.notEqual(at, -1, `rm of unknown note ${args[2]}`)
      notes.splice(at, 1)
      return ''
    }
    throw new Error(`unexpected boss ${args.join(' ')}`)
  }
  return { notes, client: notesClient({ run }) }
}

const parkRow = (over = {}) => ({
  host: 'build-host',
  session: 's1',
  pr: 12,
  repo: 'acme/app',
  title: 'Add the export button',
  action: 'awaiting-operator',
  why: 'human-review:ui · moderate: UI',
  ...over,
})
const snapshotOf = (rows, over = {}) => ({
  rows,
  operatorItems: [],
  roundExtensions: [],
  unread: [],
  errors: [],
  ...over,
})

test('an operator decision in the snapshot is queued once, however often the round reruns', () => {
  const { notes, client } = fakeBoss()
  const snapshot = snapshotOf([parkRow(), parkRow({ session: 's2', action: 'repair', pr: 13 })])
  const first = syncQueue({ snapshot, client })
  assert.equal(first.added.length, 1)
  assert.equal(notes.length, 1)
  assert.match(notes[0].body, /^key: host=build-host session=s1 pr=12 action=awaiting-operator/)
  for (let i = 0; i < 3; i++) syncQueue({ snapshot, client })
  assert.equal(notes.length, 1)
  assert.equal(client.list().length, 1)
})

test('an item whose condition cleared is resolved', () => {
  const { notes, client } = fakeBoss()
  syncQueue({ snapshot: snapshotOf([parkRow()]), client })
  // The operator approved; verify now runs on the session, so the park is gone.
  const result = syncQueue({ snapshot: snapshotOf([parkRow({ action: 'working' })]), client })
  assert.equal(result.removed.length, 1)
  assert.equal(notes.length, 0)
})

test('a park re-coded under the same session becomes a new question', () => {
  const { notes, client } = fakeBoss()
  syncQueue({ snapshot: snapshotOf([parkRow()]), client })
  syncQueue({
    snapshot: snapshotOf([parkRow({ why: 'always-human-path · moderate: UI' })]),
    client,
  })
  assert.equal(notes.length, 1)
  assert.match(notes[0].body, /reason=always-human-path/)
})

test('items for a host or session the round could not read are kept', () => {
  const { notes, client } = fakeBoss()
  syncQueue({
    snapshot: snapshotOf([parkRow(), parkRow({ host: 'local', session: 's9' })]),
    client,
  })
  assert.equal(notes.length, 2)
  // build-host was unreachable and local's s9 failed to read: neither condition is known to have cleared.
  const result = syncQueue({
    snapshot: snapshotOf([parkRow({ host: 'local', session: 's9', unread: true, action: 'ok' })], {
      unread: ['build-host'],
    }),
    client,
  })
  assert.equal(result.removed.length, 0)
  assert.equal(notes.length, 2)
})

test('an extra decision the worker raised holds while its row keeps that action', () => {
  const { notes, client } = fakeBoss()
  const blocked = parkRow({ action: 'unblock', why: 'needs a product call on the copy' })
  const extra = {
    host: 'build-host',
    session: 's1',
    pr: 12,
    action: 'unblock',
    title: 'acme/app#12 blocked on a product call',
    detail: 'The session asks which copy to ship.',
  }
  syncQueue({ snapshot: snapshotOf([blocked]), client, extras: [extra] })
  assert.equal(notes.length, 1)
  // A later run that does not repeat the extra keeps it while the session is still blocked …
  assert.equal(syncQueue({ snapshot: snapshotOf([blocked]), client }).removed.length, 0)
  assert.equal(notes.length, 1)
  // … and resolves it once the block is gone.
  syncQueue({ snapshot: snapshotOf([parkRow({ action: 'working' })]), client })
  assert.equal(notes.length, 0)
})

test('an extra that names a row in another action is rejected, not queued', () => {
  const { notes, client } = fakeBoss()
  const result = syncQueue({
    snapshot: snapshotOf([parkRow({ action: 'working' })]),
    client,
    extras: [{ host: 'build-host', session: 's1', action: 'unblock', title: 't', detail: 'd' }],
  })
  assert.equal(result.rejected.length, 1)
  assert.equal(notes.length, 0)
})

test('an answered item is not asked again while its condition holds, and still resolves', () => {
  const { notes, client } = fakeBoss()
  syncQueue({ snapshot: snapshotOf([parkRow()]), client })
  const [pending] = client.list()
  client.answer(pending.id, 'B: leave it parked until Monday')
  assert.deepEqual(
    client.list().filter((item) => !item.answered),
    [],
  )
  // A changed detail does not overwrite the recorded answer.
  syncQueue({ snapshot: snapshotOf([parkRow({ title: 'Add the export button (v2)' })]), client })
  assert.match(notes[0].body, /answer: B: leave it parked until Monday/)
  syncQueue({ snapshot: snapshotOf([]), client })
  assert.equal(notes.length, 0)
})

test('an unanswered item whose detail changed is edited in place', () => {
  const { notes, client } = fakeBoss()
  syncQueue({ snapshot: snapshotOf([parkRow()]), client })
  const result = syncQueue({
    snapshot: snapshotOf([parkRow({ title: 'Add the export button (v2)' })]),
    client,
  })
  assert.equal(result.updated.length, 1)
  assert.equal(notes.length, 1)
  assert.match(notes[0].body, /\(v2\)/)
})

test('duplicate notes for one key collapse to the oldest', () => {
  const { notes, client } = fakeBoss()
  syncQueue({ snapshot: snapshotOf([parkRow()]), client })
  notes.push({ ...notes[0], id: 'dup', key: undefined })
  syncQueue({ snapshot: snapshotOf([parkRow()]), client })
  assert.deepEqual(
    notes.map((n) => n.id),
    ['n1'],
  )
})

test('extension items resolve only when their extension ran and stopped reporting them', () => {
  const { notes, client } = fakeBoss()
  const item = { extension: 'bs-orchestrate-x', title: 'Release freeze ends today', detail: 'd' }
  syncQueue({
    snapshot: snapshotOf([], { operatorItems: [item], roundExtensions: ['bs-orchestrate-x'] }),
    client,
  })
  assert.equal(notes.length, 1)
  // The extension was skipped this round: its item is kept.
  syncQueue({ snapshot: snapshotOf([], { roundExtensions: [] }), client })
  assert.equal(notes.length, 1)
  syncQueue({ snapshot: snapshotOf([], { roundExtensions: ['bs-orchestrate-x'] }), client })
  assert.equal(notes.length, 0)
})

test('a topic item stays until it is resolved by id', () => {
  const { notes, client } = fakeBoss()
  const extra = { topic: 'helper-fix-grace', title: 'Approve a helper fix', detail: 'd' }
  syncQueue({ snapshot: snapshotOf([]), client, extras: [extra] })
  syncQueue({ snapshot: snapshotOf([]), client })
  assert.equal(notes.length, 1)
  client.resolve(notes[0].id)
  assert.equal(notes.length, 0)
})

test('notes under the tag that are not queue items are left alone', () => {
  const { notes, client } = fakeBoss()
  notes.push({ id: 'foreign', body: 'a hand-written reminder', tags: ['orchestrate-question'] })
  syncQueue({ snapshot: snapshotOf([]), client })
  assert.deepEqual(
    notes.map((n) => n.id),
    ['foreign'],
  )
})

test('a dry plan reports the changes and writes nothing', () => {
  const { notes, client } = fakeBoss()
  const plan = planQueue({ snapshot: snapshotOf([parkRow()]), items: client.list() })
  assert.equal(plan.add.length, 1)
  assert.equal(notes.length, 0)
})

test('the gate wakes the worker for an agent-movable row', () => {
  const verdict = gateVerdict({
    snapshot: snapshotOf([parkRow({ action: 'repair', why: 'merge conflict' })]),
    items: [],
  })
  assert.equal(verdict.exit, 0)
  assert.match(verdict.reason, /repair/)
})

test('the gate stays shut when nothing moves and the queue is in sync', () => {
  const { client } = fakeBoss()
  const snapshot = snapshotOf([
    parkRow(),
    parkRow({ session: 's2', action: 'grace' }),
    parkRow({ session: 's3', action: 'working' }),
  ])
  syncQueue({ snapshot, client })
  const verdict = gateVerdict({ snapshot, items: client.list() })
  assert.equal(verdict.exit, 1)
})

test('the gate wakes the worker when the operator queue needs syncing', () => {
  assert.equal(gateVerdict({ snapshot: snapshotOf([parkRow()]), items: [] }).exit, 0)
})

test('a movable row whose decision is already queued does not wake the worker', () => {
  const { client } = fakeBoss()
  const stalled = parkRow({ action: 'stalled', pr: null, why: 'no PR, no live chat' })
  syncQueue({
    snapshot: snapshotOf([stalled]),
    client,
    extras: [{ host: 'build-host', session: 's1', action: 'stalled', title: 't', detail: 'd' }],
  })
  assert.equal(gateVerdict({ snapshot: snapshotOf([stalled]), items: client.list() }).exit, 1)
})

test('the gate cannot evaluate when a host could not be read', () => {
  const verdict = gateVerdict({
    snapshot: snapshotOf([], { unread: ['build-host'], errors: ['build-host: ssh refused'] }),
    items: [],
  })
  assert.equal(verdict.exit, 69)
  assert.match(verdict.reason, /build-host/)
})
