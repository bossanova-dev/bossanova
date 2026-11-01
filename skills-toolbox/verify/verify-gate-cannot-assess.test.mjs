import { test } from 'node:test'
import assert from 'node:assert/strict'
import {
  post,
  EVIDENCE_UNKNOWN_REASONS,
  operatorMarkers,
  readOperatorMarkers,
} from './verify-gate.mjs'
import {
  CANNOT_ASSESS_AFTER_RUNS,
  unreadableRunCount,
  cannotAssessDescription,
  cannotAssessNote,
  cannotAssessMarker,
} from './verify-cannot-assess.mjs'
import { DESCRIPTION_LIMIT } from '../commit-status.mjs'

const HEAD = 'a'.repeat(40)
const URL = 'https://github.com/o/r/pull/7#issuecomment-99'
const NOW = Date.UTC(2026, 9, 2)
const node = (description, state = 'pending', extra = {}) => ({
  context: 'boss/verify',
  state,
  description,
  ...extra,
})
const claim = node('verifying… mine', 'pending', { updated_at: new Date(NOW).toISOString() })
const wait = (reason = 'pr-unreadable') => node(`waiting: ${reason}`)
const options = {
  pr: 7,
  head: HEAD,
  repo: 'o/r',
  token: 'mine',
  verdict: 'wait',
  reason: 'pr-unreadable',
}
const comment = (body, extra = {}) => ({
  body,
  id: 99,
  html_url: URL,
  created_at: '2026-10-01T00:00:00Z',
  user: { login: 'verify' },
  author_association: 'MEMBER',
  ...extra,
})

function world({
  history = [],
  comments = [],
  failNote = false,
  failComments = false,
  permission = 'write',
  mine = true,
  unreadableHistory = false,
} = {}) {
  const writes = []
  let statusReads = 0
  const ok = (payload) => ({ ok: true, payload, output: JSON.stringify(payload), exit: 0 })
  const read = (command, args) => {
    if (command !== 'gh') return ok({})
    if (args[0] === 'pr' && args[1] === 'view')
      return ok({ headRefOid: HEAD, state: 'OPEN', body: '', url: 'https://github.com/o/r/pull/7' })
    const path = args[1]
    if (path.includes('/statuses?')) {
      statusReads++
      if (unreadableHistory && statusReads > 1) return { ok: false, exit: 1 }
      return ok([[...(mine ? [claim] : []), ...history]])
    }
    if (path.includes('/comments?')) return failComments ? { ok: false, exit: 1 } : ok([comments])
    if (path.includes('/permission')) return ok({ permission })
    if (path === 'user') return ok({ login: 'verify' })
    if (path === '--method') {
      const fields = Object.fromEntries(
        args
          .filter((a) => /^(body|state|description|target_url)=/.test(a))
          .map((a) => {
            const at = a.indexOf('=')
            return [a.slice(0, at), a.slice(at + 1)]
          }),
      )
      writes.push({ method: args[2], path: args[3], ...fields })
      if (fields.body && failNote) return { ok: false, exit: 1 }
      return ok({ html_url: URL })
    }
    throw new Error(`Unexpected read: ${command} ${args.join(' ')}`)
  }
  // Exercise the defensive unreadable-history branch even with a readable initial claim.
  if (unreadableHistory) read.writeAware = true
  return { writes, deps: { read, now: () => NOW, exists: () => false, loadConfig: () => ({}) } }
}

test('unreadable run count skips claims and other contexts, adds mixed reasons and cannot-assess, and resets on all other outcomes', () => {
  assert.equal(CANNOT_ASSESS_AFTER_RUNS, 3)
  const reasons = EVIDENCE_UNKNOWN_REASONS
  const counted = [
    claim,
    wait(),
    node('external', 'failure', { context: 'ci' }),
    wait('receipt-unknown'),
    claim,
    node('cannot assess: base-ci-unknown', 'error'),
  ]
  assert.equal(unreadableRunCount(counted, reasons), 3)
  assert.equal(unreadableRunCount([], reasons), 0)
  for (const reset of [
    wait('ci-not-settled'),
    node('verified', 'success'),
    node('defect: tests', 'failure'),
    node('blocked: review', 'failure'),
    node('needs human: extension-failed', 'pending'),
  ])
    assert.equal(unreadableRunCount([wait(), reset, ...counted], reasons), 1)
})

test('cannot-assess description is capped by Unicode code points and note digest excludes runs', () => {
  const description = cannotAssessDescription('😀'.repeat(200))
  assert.equal(Array.from(description).length, DESCRIPTION_LIMIT)
  assert.ok(description.endsWith('…'))
  const note = cannotAssessNote({ headSha: HEAD, reason: 'pr-unreadable', runs: 3 })
  assert.equal(
    note.digest,
    cannotAssessNote({ headSha: HEAD, reason: 'pr-unreadable', runs: 5 }).digest,
  )
  assert.notEqual(
    note.digest,
    cannotAssessNote({ headSha: HEAD, reason: 'receipt-unknown', runs: 3 }).digest,
  )
  assert.notEqual(
    note.digest,
    cannotAssessNote({ headSha: 'b'.repeat(40), reason: 'pr-unreadable', runs: 3 }).digest,
  )
  assert.ok(note.body.endsWith(cannotAssessMarker(HEAD, note.digest)))
})

test('@sessions.AC218201 first two unreadable runs wait, the third posts error linked to a note', async () => {
  for (const earlier of [0, 1, 2]) {
    const w = world({ history: Array.from({ length: earlier }, () => wait()) })
    const result = await post(options, w.deps)
    const status = w.writes.find((write) => write.state)
    assert.equal(result.posted, true)
    assert.equal(status.state, earlier === 2 ? 'error' : 'pending')
    assert.equal(
      status.description,
      earlier === 2 ? 'cannot assess: pr-unreadable' : 'waiting: pr-unreadable',
    )
    assert.equal(result.writes.filter((write) => write.trigger === 'evidence-unknown').length, 1)
    if (earlier === 2) {
      assert.equal(status.target_url, URL)
      assert.deepEqual(result.cannotAssess, { runs: 3, note: 'posted' })
      const body = w.writes.find((write) => write.body).body
      assert.match(body, /pr-unreadable/)
      assert.match(body, /3 consecutive runs/)
      assert.match(body, /<!-- boss-cannot-assess head=/)
    } else assert.equal(w.writes.filter((write) => write.body).length, 0)
  }
})

test('@sessions.AC218201 cannot-assess reruns reuse a matching note and update a changed reason in place', async () => {
  const note = cannotAssessNote({ headSha: HEAD, reason: options.reason, runs: 3 })
  for (const reason of ['pr-unreadable', 'receipt-unknown']) {
    const w = world({
      history: [node('cannot assess: pr-unreadable', 'error'), wait(), wait()],
      comments: [comment(note.body)],
    })
    const result = await post({ ...options, reason }, w.deps)
    assert.equal(result.cannotAssess.runs, 4)
    assert.equal(result.cannotAssess.note, reason === options.reason ? 'current' : 'updated')
    assert.equal(w.writes.find((write) => write.state).target_url, URL)
    const comments = w.writes.filter((write) => write.body)
    assert.equal(comments.length, reason === options.reason ? 0 : 1)
    if (comments.length) assert.equal(comments[0].method, 'PATCH')
  }
})

test('unreadable or failed cosmetic notes never prevent error posting', async () => {
  for (const fault of [{ failNote: true }, { failComments: true }]) {
    const w = world({ history: [wait(), wait()], ...fault })
    const result = await post(options, w.deps)
    assert.deepEqual(result.cannotAssess, { runs: 3, note: fault.failNote ? 'failed' : 'unknown' })
    const status = w.writes.find((write) => write.state)
    assert.equal(status.state, 'error')
    assert.equal(status.target_url, undefined)
  }
})

test('@sessions.AC218201 unreadable history and ordinary waits do not escalate, and an absent claim writes nothing', async () => {
  for (const setup of [
    { unreadableHistory: true },
    { history: [wait(), wait()] },
    { history: [wait(), wait()], mine: false },
  ]) {
    const w = world(setup)
    const reason =
      setup.unreadableHistory || setup.mine === false ? options.reason : 'ci-not-settled'
    const result = await post({ ...options, reason }, w.deps)
    const status = w.writes.find((write) => write.state)
    assert.equal(result.cannotAssess, undefined)
    assert.equal(status?.state, setup.mine === false ? undefined : 'pending')
  }
})

test('@sessions.AC218201 a readable run re-judges a cannot-assess head as verified', async () => {
  const w = world({ history: [node('cannot assess: pr-unreadable', 'error'), wait(), wait()] })
  const result = await post({ ...options, verdict: 'pass', reason: '' }, w.deps)
  assert.equal(result.posted, true)
  assert.equal(w.writes.find((write) => write.state).description, 'verified')
})

test('cannot-assess markers round trip through trusted write-access filtering', () => {
  const note = cannotAssessNote({ headSha: HEAD, reason: options.reason, runs: 3 })
  const markers = operatorMarkers([comment(note.body)])
  assert.equal(markers[0].kind, 'cannot-assess')
  assert.equal(markers[0].head, HEAD)
  assert.equal(markers[0].digest, note.digest)
  assert.equal(markers[0].url, URL)
  assert.deepEqual(operatorMarkers([comment(note.body, { author_association: 'NONE' })]), [])
  const w = world({ comments: [comment(note.body)], permission: 'read' })
  assert.deepEqual(readOperatorMarkers(w.deps.read, 'o/r', 7), { readable: true, markers: [] })
})

test('dry run lists the cannot-assess note and error status without writing', async () => {
  const w = world({ history: [wait(), wait()] })
  const result = await post({ ...options, dryRun: true }, w.deps)
  assert.equal(w.writes.length, 0)
  assert.equal(result.cannotAssess.note, 'planned')
  assert.ok(result.writes.some((write) => write.kind === 'pr-comment'))
  assert.ok(result.writes.some((write) => write.kind === 'status' && write.state === 'error'))
})
