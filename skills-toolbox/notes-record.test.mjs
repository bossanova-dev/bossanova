import assert from 'node:assert/strict'
import { test } from 'node:test'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { parseNotes, parseRunId } from './bs-record-notes.mjs'
import { mustFixCategories } from './bs-review-triage.mjs'
import { TRIGGERS, addTrigger, flushTriggers, main } from './notes-record.mjs'

function harness(now = '2026-10-07T12:00:00Z') {
  const files = new Map()
  const writes = []
  const output = []
  const errors = []
  const absent = () => Object.assign(new Error('not found'), { code: 'ENOENT' })
  const deps = {
    now: () => new Date(now),
    env: {},
    gitDir: () => '/git',
    stdout: (text) => output.push(text),
    stderr: (text) => errors.push(text),
    addNote: (note) => {
      writes.push(note)
      return `note-${writes.length}`
    },
    fs: {
      appendFileSync: (path, text) => files.set(path, (files.get(path) || '') + text),
      renameSync: (from, to) => {
        if (!files.has(from)) throw absent()
        files.set(to, files.get(from))
        files.delete(from)
      },
      readFileSync: (path) => {
        if (!files.has(path)) throw absent()
        return files.get(path)
      },
      unlinkSync: (path) => {
        if (!files.delete(path)) throw absent()
      },
    },
  }
  const add = (trigger = 'gate-unknown', extra = {}) =>
    addTrigger({ core: 'boss-build', trigger, where: 'CI', ...extra }, deps)
  const flush = (extra = {}) =>
    flushTriggers({ core: 'boss-build', outcome: 'BLOCKED', mode: 'headless', ...extra }, deps)
  return { files, writes, output, errors, deps, add, flush }
}

const triggers = [
  'evidence-unknown',
  'extension-failed',
  'repair-exhausted',
  'human-push-on-agent-pr',
  'stale-claim-taken-over',
  'verify-failed-after-clean-review',
  'dispatch-failed',
  'gate-unknown',
  'review-must-fix',
]
test('vocabulary has all frozen entries', () => {
  assert.deepEqual(Object.keys(TRIGGERS).sort(), [...triggers].sort())
  assert.ok(Object.isFrozen(TRIGGERS))
  for (const entry of Object.values(TRIGGERS)) assert.ok(Object.isFrozen(entry))
})
for (const trigger of triggers)
  test(`records ${trigger} in the existing five-line shape`, () => {
    const h = harness()
    assert.equal(h.add(trigger).ok, true)
    assert.deepEqual(h.flush().recorded, [{ trigger, where: 'CI', noteId: 'note-1' }])
    const { body, tag, idempotencyKey } = h.writes[0]
    assert.equal(tag, 'improvement')
    assert.match(idempotencyKey, /^notes-record:[a-f0-9]{32}$/)
    assert.equal(body.split('\n').length, 5)
    assert.equal(body.split('\n')[0], `boss-build: ${TRIGGERS[trigger].statement}`)
    assert.match(
      body.split('\n')[4],
      new RegExp(
        `^Run: boss-build / BLOCKED / headless / trigger:${trigger} / run:adhoc-[a-f0-9]{8}$`,
      ),
    )
    const parsed = parseNotes(body.split('\n').slice(0, 4).join('\n'), 'run')
    assert.equal(parsed.bodies.length, 1)
    assert.deepEqual(parsed.rejected, [])
  })
test('unknown add is rejected without buffering and main exits zero', () => {
  const h = harness()
  assert.equal(
    main(['add', '--core', 'boss-build', '--trigger', 'pending', '--where', 'CI'], h.deps),
    0,
  )
  assert.equal(JSON.parse(h.output[0]).error, 'unknown trigger: pending')
  assert.equal(h.files.size, 0)
})
test('unknown buffered trigger and invalid records are malformed', () => {
  const h = harness()
  h.add()
  const path = '/git/boss-notes-record.jsonl'
  h.files.set(
    path,
    h.files.get(path) +
      JSON.stringify({
        v: 1,
        at: '2026-10-07',
        core: 'boss-build',
        trigger: 'pending',
        where: 'CI',
        detail: '',
      }) +
      '\nnull\n',
  )
  const result = h.flush()
  assert.equal(result.recorded.length, 1)
  assert.deepEqual(
    result.dropped.map((item) => item.reason),
    ['malformed', 'malformed'],
  )
})
test('cap records three first-seen distinct notes after deduplication', () => {
  const h = harness()
  h.add(triggers[0])
  h.add(triggers[0])
  for (const trigger of triggers.slice(1, 5)) h.add(trigger)
  const result = h.flush()
  assert.deepEqual(
    result.recorded.map((item) => item.trigger),
    triggers.slice(0, 3),
  )
  assert.deepEqual(
    result.dropped.filter((item) => item.reason === 'over-cap').map((item) => item.trigger),
    triggers.slice(3, 5),
  )
  assert.equal(h.writes.length, 3)
})
test('duplicate tuple keeps first detail while different core and where survive', () => {
  const h = harness()
  h.add('gate-unknown', { detail: 'first' })
  h.add('gate-unknown', { detail: 'second' })
  h.add('gate-unknown', { core: 'boss-repair' })
  h.add('gate-unknown', { where: 'other' })
  assert.equal(h.flush().recorded.length, 3)
  assert.match(h.writes[0].body, /Detail: first/)
  assert.doesNotMatch(h.writes[0].body, /second/)
})
test('per-run key excludes detail and records separate occurrences across flushes', () => {
  const h = harness()
  for (const runId of ['a', 'b', 'a']) {
    h.add('gate-unknown', { runId, detail: runId })
    h.flush()
  }
  assert.notEqual(h.writes[0].idempotencyKey, h.writes[1].idempotencyKey)
  assert.equal(h.writes[0].idempotencyKey, h.writes[2].idempotencyKey)
  assert.equal(parseRunId(h.writes[0].body), 'a')
})
test('run identity captured at add survives different flush environment', () => {
  const h = harness()
  h.deps.env.BOSS_AGENT_SESSION_ID = 'original'
  h.add()
  h.deps.env.BOSS_AGENT_SESSION_ID = 'flusher'
  h.flush()
  assert.equal(parseRunId(h.writes[0].body), 'original')
})
test('no-env adds reuse an adhoc buffer identity across processes', () => {
  const h = harness()
  h.files.set(
    '/git/boss-notes-record.jsonl',
    JSON.stringify({
      v: 1,
      at: '2026-10-07',
      core: 'boss-build',
      trigger: 'gate-unknown',
      where: 'CI',
      detail: '',
      runId: 'adhoc-deadbeef',
    }) + '\n',
  )
  h.add()
  const lines = h.files.get('/git/boss-notes-record.jsonl').trim().split('\n').map(JSON.parse)
  assert.equal(lines[1].runId, 'adhoc-deadbeef')
  assert.equal(h.flush().recorded.length, 1)
})
test('within-run collapse preserves distinct runs in one buffer', () => {
  const h = harness()
  h.add('gate-unknown', { runId: 'a' })
  h.add('gate-unknown', { runId: 'a' })
  h.add('gate-unknown', { runId: 'b' })
  assert.equal(h.flush().recorded.length, 2)
})
test('legacy buffered lines resolve their run at flush time', () => {
  const h = harness()
  h.add()
  const path = '/git/boss-notes-record.jsonl'
  const line = JSON.parse(h.files.get(path))
  delete line.runId
  h.files.set(path, JSON.stringify(line) + '\n')
  h.deps.env.BOSS_SESSION_ID = 'legacy'
  h.flush()
  assert.equal(parseRunId(h.writes[0].body), 'legacy')
})
test('secret body is dropped', () => {
  const h = harness()
  h.add('gate-unknown', { detail: `ghp_${'a'.repeat(30)}` })
  assert.deepEqual(h.flush().dropped, [{ trigger: 'gate-unknown', reason: 'secret-shape' }])
  assert.equal(h.writes.length, 0)
})
test('where and detail are single lines capped to 200 characters', () => {
  const h = harness()
  h.add('gate-unknown', { where: 'a\r\nb' + 'c'.repeat(210), detail: 'd\ne' + 'f'.repeat(210) })
  h.flush()
  assert.equal(h.writes[0].body.split('\n').length, 5)
  assert.equal(h.writes[0].body.split('\n')[1].slice(7).length, 200)
  assert.equal(h.writes[0].body.split('\n')[2].split(' Detail: ')[1].length, 200)
})
test('throwing note writer is reported and main exits zero', () => {
  const h = harness()
  h.add()
  h.deps.addNote = () => {
    throw new Error('offline')
  }
  assert.equal(main(['flush', '--core', 'boss-build', '--outcome', 'BLOCKED'], h.deps), 0)
  assert.match(JSON.parse(h.output[0]).failures[0], /offline/)
  assert.match(h.errors.join(''), /offline/)
})
test('absent buffer is an empty successful no-op', () => {
  const h = harness()
  assert.equal(main(['flush', '--core', 'boss-build', '--outcome', 'BLOCKED'], h.deps), 0)
  assert.deepEqual(JSON.parse(h.output[0]), { recorded: [], dropped: [], failures: [] })
})
test('malformed JSONL reports a failure and does not lose valid lines', () => {
  const h = harness()
  h.add()
  h.files.set(
    '/git/boss-notes-record.jsonl',
    'invalid\n' + h.files.get('/git/boss-notes-record.jsonl'),
  )
  assert.equal(main(['flush', '--core', 'boss-build', '--outcome', 'BLOCKED'], h.deps), 0)
  const result = JSON.parse(h.output[0])
  assert.equal(result.recorded.length, 1)
  assert.equal(result.failures.length, 1)
})
test('unresolvable git directory is non-fatal for both verbs', () => {
  for (const argv of [
    ['add', '--core', 'boss-build', '--trigger', 'gate-unknown', '--where', 'CI'],
    ['flush', '--core', 'boss-build', '--outcome', 'BLOCKED'],
  ]) {
    const h = harness()
    h.deps.gitDir = () => {
      throw new Error('no git directory')
    }
    assert.equal(main(argv, h.deps), 0)
    assert.match(JSON.parse(h.output[0]).failures[0], /no git directory/)
  }
})
test('unwritable add buffer is non-fatal', () => {
  const h = harness()
  h.deps.fs.appendFileSync = () => {
    throw new Error('permission denied')
  }
  assert.equal(
    main(['add', '--core', 'boss-build', '--trigger', 'gate-unknown', '--where', 'CI'], h.deps),
    0,
  )
  assert.match(JSON.parse(h.output[0]).failures[0], /permission denied/)
})
test('suppressed nested flush leaves its additions buffered', () => {
  const h = harness()
  h.deps.env.BOSS_NOTES_SUPPRESSED = '1'
  h.add()
  assert.deepEqual(h.flush(), { recorded: [], dropped: [], failures: [] })
  assert.equal(h.files.size, 1)
  h.deps.env = {}
  assert.equal(h.flush().recorded.length, 1)
})
test('atomic claim removes the buffer and second flush records nothing', () => {
  const h = harness()
  h.add()
  assert.equal(h.flush().recorded.length, 1)
  assert.equal(h.files.size, 0)
  assert.equal(h.flush().recorded.length, 0)
})
test('stale entries older than seven days are dropped', () => {
  const h = harness('2026-10-01T00:00:00Z')
  h.add()
  h.deps.now = () => new Date('2026-10-08T00:00:01Z')
  assert.deepEqual(h.flush().dropped, [{ trigger: 'gate-unknown', reason: 'stale' }])
})
test('unreadable claimed buffer is reported and removed', () => {
  const h = harness()
  h.add()
  h.deps.fs.readFileSync = () => {
    throw new Error('unreadable')
  }
  assert.match(h.flush().failures[0], /unreadable/)
  assert.equal(h.files.size, 0)
})
test('explicit buffer bypasses git directory resolution', () => {
  const h = harness()
  h.deps.gitDir = () => {
    throw new Error('must not run')
  }
  h.add('gate-unknown', { buffer: '/custom' })
  assert.equal(h.flush({ buffer: '/custom' }).recorded.length, 1)
})
test('invalid CLI arguments are non-fatal and reported', () => {
  const h = harness()
  assert.equal(main(['unexpected'], h.deps), 0)
  assert.equal(JSON.parse(h.output[0]).ok, false)
  assert.equal(h.errors.length, 1)
})
test('unresolvable boss executable is non-fatal without spawning', () => {
  const h = harness()
  h.add()
  delete h.deps.addNote
  h.deps.fs.statSync = () => {
    throw Object.assign(new Error('not found'), { code: 'ENOENT' })
  }
  assert.equal(main(['flush', '--core', 'boss-build', '--outcome', 'BLOCKED'], h.deps), 0)
  assert.match(JSON.parse(h.output[0]).failures[0], /no usable boss executable/)
})
test('invalid note writer result is reported without recording', () => {
  const h = harness()
  h.add()
  h.deps.addNote = () => undefined
  const result = h.flush()
  assert.equal(result.recorded.length, 0)
  assert.match(result.failures[0], /returned no id/)
})
test('a failed claim does not unlink an existing claimed file', () => {
  const h = harness()
  const claimed = `/git/boss-notes-record.jsonl.flushing-${process.pid}`
  h.files.set(claimed, 'peer data')
  assert.deepEqual(h.flush(), { recorded: [], dropped: [], failures: [] })
  assert.equal(h.files.get(claimed), 'peer data')
})

function fakeBoss(t, script) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'notes-record-test-'))
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }))
  const binary = path.join(dir, 'boss')
  fs.writeFileSync(binary, `#!${process.execPath}\n${script}\n`, { mode: 0o755 })
  const h = harness()
  delete h.deps.addNote
  h.deps.fs = fs
  h.deps.env = { BOSS_BIN: binary, PATH: '' }
  const buffer = path.join(dir, 'buffer')
  h.add('gate-unknown', { buffer })
  return { h, buffer, dir }
}

test('resolved boss receives exact argv without repo override and its JSON id is recorded', (t) => {
  const { h, buffer, dir } = fakeBoss(
    t,
    `require('node:fs').writeFileSync(__dirname + '/argv.json', JSON.stringify(process.argv.slice(2))); console.log(JSON.stringify({id:'fake-note'}))`,
  )
  const result = h.flush({ buffer })
  assert.equal(result.recorded[0].noteId, 'fake-note')
  const argv = JSON.parse(fs.readFileSync(path.join(dir, 'argv.json'), 'utf8'))
  assert.deepEqual(argv.slice(0, 6), [
    'notes',
    'add',
    '--tag',
    'improvement',
    '--json',
    '--idempotency-key',
  ])
  assert.match(argv[6], /^notes-record:[a-f0-9]{32}$/)
  assert.equal(argv[7], '--')
  assert.match(argv[8], /^boss-build:/)
  assert.equal(argv.length, 9)
})

test('nonzero boss note command reports failure and main returns zero', (t) => {
  const { h, buffer } = fakeBoss(t, `console.error('store unavailable'); process.exit(9)`)
  assert.equal(
    main(['flush', '--core', 'boss-build', '--outcome', 'BLOCKED', '--buffer', buffer], h.deps),
    0,
  )
  assert.match(JSON.parse(h.output[0]).failures[0], /exited 9: store unavailable/)
  assert.match(h.errors.join(''), /exited 9/)
})

test('unparseable boss JSON reports failure and main returns zero', (t) => {
  const { h, buffer } = fakeBoss(t, `console.log('invalid json')`)
  assert.equal(
    main(['flush', '--core', 'boss-build', '--outcome', 'BLOCKED', '--buffer', buffer], h.deps),
    0,
  )
  assert.equal(JSON.parse(h.output[0]).failures.length, 1)
  assert.equal(h.errors.length, 1)
})

test('malformed timestamp is reported while a valid record still writes', () => {
  const h = harness()
  h.add()
  const buffer = '/git/boss-notes-record.jsonl'
  const line = JSON.parse(h.files.get(buffer))
  h.files.set(buffer, JSON.stringify({ ...line, at: 'not a date' }) + '\n' + h.files.get(buffer))
  const result = h.flush()
  assert.equal(result.recorded.length, 1)
  assert.deepEqual(result.dropped, [{ trigger: 'gate-unknown', reason: 'malformed' }])
  assert.deepEqual(result.failures, ['malformed buffered trigger record'])
})

test('batch category add normalizes pointers and reports skipped blanks', () => {
  const h = harness()
  h.files.set(
    '/input',
    JSON.stringify([
      { where: 'one/api', detail: 'first' },
      { where: '   ' },
      { where: 'two\ncategory' + 'x'.repeat(210) },
    ]),
  )
  assert.equal(
    main(
      ['add', '--core', 'boss-review', '--trigger', 'review-must-fix', '--from', '/input'],
      h.deps,
    ),
    0,
  )
  const result = JSON.parse(h.output[0])
  assert.equal(result.ok, true)
  assert.equal(result.buffered, 2)
  assert.equal(result.skipped.length, 1)
  h.flush()
  assert.equal(h.writes.length, 2)
  assert.match(h.writes[0].body, /^boss-review:/)
  assert.equal(h.writes[1].body.split('\n')[1].slice(7).length, 200)
})
test('invalid batch inputs are nonfatal and buffer nothing', () => {
  for (const content of [undefined, '{}', 'invalid']) {
    const h = harness()
    if (content !== undefined) h.files.set('/input', content)
    assert.equal(
      main(
        ['add', '--core', 'boss-review', '--trigger', 'review-must-fix', '--from', '/input'],
        h.deps,
      ),
      0,
    )
    assert.equal(JSON.parse(h.output[0]).ok, false)
    assert.ok(!h.files.has('/git/boss-notes-record.jsonl'))
  }
  const h = harness()
  h.files.set('/input', '[]')
  main(
    [
      'add',
      '--core',
      'boss-review',
      '--trigger',
      'review-must-fix',
      '--from',
      '/input',
      '--where',
      'one/api',
    ],
    h.deps,
  )
  assert.equal(JSON.parse(h.output[0]).ok, false)
  assert.ok(!h.files.has('/git/boss-notes-record.jsonl'))
})
test('fixture report categories have a separate three-note cap from malfunction triggers', () => {
  const h = harness()
  for (const trigger of triggers.slice(0, 3)) h.add(trigger)
  const items = Array.from({ length: 5 }, (_, i) => ({
    file: `file${i}`,
    line: 1,
    title: `defect${i}`,
    category: `category${i}`,
    severity: 'Warning',
    lenses: ['reviewer'],
  }))
  const categories = mustFixCategories([{ mustFix: [], pool: items }], {
    report: { mustfix: { items } },
  })
  h.files.set('/input', JSON.stringify(categories))
  main(['add', '--core', 'boss-review', '--trigger', 'review-must-fix', '--from', '/input'], h.deps)
  const result = h.flush()
  assert.equal(result.recorded.length, 6)
  assert.deepEqual(
    result.recorded.slice(3).map((x) => x.where),
    categories.slice(0, 3).map((x) => x.where),
  )
  assert.deepEqual(
    result.dropped,
    categories
      .slice(3)
      .map((x) => ({ trigger: 'review-must-fix', where: x.where, reason: 'over-category-cap' })),
  )
})
test('category idempotency collapses passes within runs and preserves cross-run occurrences', () => {
  const h = harness()
  for (const runId of ['first', 'second']) {
    h.add('review-must-fix', { core: 'boss-review', where: 'reviewer/api', runId })
    h.add('review-must-fix', { core: 'boss-review', where: 'reviewer/api', runId })
    assert.equal(h.flush().recorded.length, 1)
  }
  assert.equal(h.writes.length, 2)
  assert.notEqual(h.writes[0].idempotencyKey, h.writes[1].idempotencyKey)
  assert.equal(parseRunId(h.writes[0].body), 'first')
  assert.match(h.writes[0].body, /^boss-review:/)
})
test('category write failures remain nonfatal in main', () => {
  const h = harness()
  h.add('review-must-fix', { core: 'boss-review', where: 'reviewer/api' })
  h.deps.addNote = () => {
    throw new Error('offline')
  }
  assert.equal(main(['flush', '--core', 'boss-build', '--outcome', 'REVIEW_READY'], h.deps), 0)
  assert.match(JSON.parse(h.output[0]).failures[0], /offline/)
})
