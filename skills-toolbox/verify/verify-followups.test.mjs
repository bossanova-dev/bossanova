import { test } from 'node:test'
import assert from 'node:assert/strict'
import { followUp, main } from './verify-followups.mjs'
import { buildNotes, notesComment, parseNotesTrailer, followUpIssue } from './verify-notes.mjs'
import { UsageError } from './verify-gate.mjs'

const HEAD = 'a'.repeat(40)
const URL = 'https://github.com/o/r/pull/7'
const notes = buildNotes([
  { severity: 'P3', title: 'One', evidence: 'First detail', file: 'a.mjs', line: 1 },
  { severity: 'style', title: 'Two', evidence: 'Second detail' },
])
const ticket = (n = 1) => ({ identifier: `TASK-${n}`, url: `https://tracker.example/${n}` })
const comment = (id = 1, login = 'verify', values = notes, tickets = {}) => ({
  id,
  user: { login },
  created_at: '2026-10-01T00:00:00Z',
  body: notesComment({ head: HEAD, notes: values, tickets }).body,
})
function world({ comments = [comment()], newer, create, fail = '' } = {}) {
  const calls = [],
    creates = [],
    patches = []
  let reads = 0
  const ok = (payload) => ({ ok: true, payload })
  const read = (command, args) => {
    calls.push({ command, args })
    if (args[0] === 'pr') return fail === 'pr' ? { ok: false } : ok({ url: URL, state: 'MERGED' })
    if (args[1] === 'user') return fail === 'viewer' ? { ok: false } : ok({ login: 'verify' })
    if (args[1].includes('/comments?')) {
      reads++
      return fail === 'comments' ? { ok: false } : ok([reads > 1 && newer ? newer : comments])
    }
    if (args[1] === '--method') {
      assert.equal(args[2], 'PATCH')
      patches.push({ path: args[3], body: args.at(-1).slice(5) })
      return fail === 'patch' ? { ok: false } : ok({ html_url: URL + '#issuecomment-1' })
    }
    throw new Error(`unexpected read ${args.join(' ')}`)
  }
  return {
    calls,
    creates,
    patches,
    deps: {
      read,
      loadConfig: () => ({
        adapters: { tracker: 'linear' },
        trackerConfig: {
          linear: {
            states: { unplanned: 'Backlog' },
            labels: { agentPlan: 'plan-me' },
          },
        },
      }),
      adapter: {
        applyIssueCreate: async (input) => {
          creates.push(input)
          return create
            ? create(input, creates.length)
            : { outcome: 'ok', issue: ticket(creates.length) }
        },
      },
    },
  }
}
const options = { pr: 7, repo: 'o/r', all: true }

test('files all notes in trailer order and patches once with canonical ticket links', async () => {
  const w = world()
  const result = await followUp(options, w.deps)
  assert.equal(result.ok, true)
  assert.equal(result.comment, 'updated')
  assert.deepEqual(
    result.results.map((r) => r.outcome),
    ['created', 'created'],
  )
  assert.deepEqual(
    w.creates,
    notes.map((note) => ({
      ...followUpIssue({ repo: 'o/r', pr: 7, prUrl: URL, note }),
      stateName: 'Backlog',
      labels: ['plan-me'],
    })),
  )
  assert.equal(w.patches.length, 1)
  assert.deepEqual(
    parseNotesTrailer(w.patches[0].body).tickets,
    Object.fromEntries(notes.map((note, i) => [note.id, ticket(i + 1)])),
  )
  assert.equal(w.calls.filter(({ args }) => args.includes('--method')).length, 1)
  assert.ok(
    w.calls.every(
      ({ command, args }) =>
        command === 'gh' &&
        (args[0] === 'pr' ||
          args[1] === 'user' ||
          args[1].includes('/comments?') ||
          (args[1] === '--method' &&
            args[2] === 'PATCH' &&
            args[3] === 'repos/o/r/issues/comments/1')),
    ),
  )
})
test('selects individual notes and refuses the whole selection for unknown ids', async () => {
  const w = world()
  assert.equal((await followUp({ ...options, all: false, note: [notes[1].id] }, w.deps)).ok, true)
  assert.equal(w.creates.length, 1)
  assert.equal(
    w.creates[0].marker,
    followUpIssue({ repo: 'o/r', pr: 7, prUrl: URL, note: notes[1] }).marker,
  )
  const unknown = world()
  const result = await followUp(
    { ...options, all: false, note: [notes[0].id, 'note:000000000000'] },
    unknown.deps,
  )
  assert.equal(result.reason, 'unknown-note')
  assert.deepEqual(result.ids, ['note:000000000000'])
  assert.equal(unknown.creates.length, 0)
})
test('existing issue is relinked and a fully ticketed comment is current without writes', async () => {
  const w = world({ create: () => ({ outcome: 'ok', existing: true, issue: ticket() }) })
  assert.ok((await followUp(options, w.deps)).results.every((r) => r.outcome === 'already-filed'))
  assert.equal(Object.keys(parseNotesTrailer(w.patches[0].body).tickets).length, 2)
  const linked = world({
    comments: [comment(1, 'verify', notes, Object.fromEntries(notes.map((n) => [n.id, ticket()])))],
  })
  const result = await followUp(options, linked.deps)
  assert.equal(result.ok, true)
  assert.equal(result.comment, 'current')
  assert.equal(linked.creates.length, 0)
  assert.equal(linked.patches.length, 0)
})
test('indeterminate, failed, throwing and missing-url creates preserve independent successes', async () => {
  for (const [create, outcome, reason] of [
    [() => ({ outcome: 'indeterminate' }), 'indeterminate', undefined],
    [() => ({ outcome: 'failed', reason: 'marker-ambiguous' }), 'failed', 'marker-ambiguous'],
    [
      () => {
        throw new Error('network setup')
      },
      'failed',
      'tracker-error',
    ],
    [() => ({ outcome: 'ok', issue: { identifier: 'TASK-1' } }), 'failed', 'ticket-url-missing'],
  ]) {
    const w = world({
      create: (input, n) => (n === 1 ? create() : { outcome: 'ok', issue: ticket(2) }),
    })
    const result = await followUp(options, w.deps)
    assert.equal(result.ok, false)
    assert.equal(result.results[0].outcome, outcome)
    assert.equal(result.results[0].reason, reason)
    assert.equal(result.results[1].outcome, 'created')
    assert.equal(w.creates.length, 2)
    assert.deepEqual(Object.keys(parseNotesTrailer(w.patches[0].body).tickets), [notes[1].id])
  }
})
test('missing creation capability and missing tracker roles refuse without writes', async () => {
  for (const adapter of [null, {}]) {
    const w = world()
    assert.equal((await followUp(options, { ...w.deps, adapter })).reason, 'tracker-cannot-create')
    assert.equal(w.patches.length, 0)
  }
  for (const config of [
    {},
    {
      adapters: { tracker: 'linear' },
      trackerConfig: { linear: { states: { unplanned: 'Backlog' } } },
    },
  ]) {
    const w = world()
    assert.equal(
      (await followUp(options, { ...w.deps, loadConfig: () => config })).reason,
      'tracker-unconfigured',
    )
    assert.equal(w.creates.length, 0)
  }
})
test('foreign notes are ignored and newest own comment wins by date and id', async () => {
  const foreign = world({ comments: [comment(9, 'someone')] })
  assert.equal((await followUp(options, foreign.deps)).reason, 'notes-absent')
  assert.equal(foreign.patches.length, 0)
  const w = world({ comments: [comment(1), comment(3), comment(9, 'someone')] })
  assert.equal((await followUp(options, w.deps)).ok, true)
  assert.equal(w.patches[0].path, 'repos/o/r/issues/comments/3')
})
test('re-read targets new head comment and reports filed notes that disappeared', async () => {
  const target = {
    ...comment(4, 'verify', [notes[1]]),
    body: notesComment({ head: 'b'.repeat(40), notes: [notes[1]] }).body,
  }
  const w = world({ newer: [target] })
  const result = await followUp(options, w.deps)
  assert.equal(result.ok, true)
  assert.equal(result.results[0].linked, false)
  assert.equal(result.results[0].reason, 'note-gone')
  assert.equal(w.patches[0].path, 'repos/o/r/issues/comments/4')
  assert.equal(parseNotesTrailer(w.patches[0].body).head, 'b'.repeat(40))
  assert.deepEqual(Object.keys(parseNotesTrailer(w.patches[0].body).tickets), [notes[1].id])
})
test('dry run lists creates and comment replacement without writes', async () => {
  const w = world()
  const result = await followUp({ ...options, dryRun: true }, w.deps)
  assert.equal(result.ok, true)
  assert.equal(result.dryRun, true)
  assert.deepEqual(
    result.writes.map((r) => r.kind),
    ['tracker-create', 'tracker-create', 'pr-comment'],
  )
  assert.equal(result.writes.at(-1).replaces, 1)
  assert.equal(w.creates.length, 0)
  assert.equal(w.patches.length, 0)
})
test('unreadable data and patch failure are explicit failures', async () => {
  for (const [fail, reason] of [
    ['pr', 'pr-unreadable'],
    ['viewer', 'viewer-unreadable'],
    ['comments', 'comments-unreadable'],
    ['patch', undefined],
  ]) {
    const w = world({ fail })
    const result = await followUp(options, w.deps)
    assert.equal(result.ok, false)
    assert.equal(result.reason, reason)
    if (fail === 'patch') assert.equal(result.comment, 'failed')
  }
})
test('main accepts strict CLI selection and throws usage errors with exit code 2', async () => {
  for (const args of [
    ['--all', '--note', notes[0].id],
    [],
    ['--unknown'],
    ['--note', 'invalid'],
    ['--all', 'positional'],
    ['--all', '--pr', '0'],
    ['--all', '--pr', '1.5'],
  ])
    await assert.rejects(main(['follow-up', '--pr', '7', ...args], world().deps), UsageError)
  const result = await main(
    ['follow-up', '--pr', '7', '--repo', 'o/r', '--all', '--json'],
    world().deps,
  )
  assert.equal(result.ok, true)
})

test('preserves a newer recorded ticket for notes already filed at the first read', async () => {
  const id = notes[0].id
  const w = world({
    comments: [comment(1, 'verify', notes, { [id]: ticket(1) })],
    newer: [comment(2, 'verify', notes, { [id]: ticket(9) })],
  })
  const result = await followUp({ ...options, all: false, note: [id] }, w.deps)
  assert.equal(result.ok, true)
  assert.equal(result.comment, 'current')
  assert.equal(w.creates.length, 0)
  assert.equal(w.patches.length, 0)
  const gone = world({
    comments: [comment(1, 'verify', notes, { [id]: ticket(1) })],
    newer: [comment(2, 'verify', [])],
  })
  const missing = await followUp({ ...options, all: false, note: [id] }, gone.deps)
  assert.equal(missing.ok, true)
  assert.equal(missing.results[0].linked, false)
  assert.equal(missing.results[0].reason, 'note-gone')
})
