import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { test } from 'node:test'
import {
  formatRunLine,
  parseRunId,
  sanitizeRunId,
  resolveRunId,
  resetAdhocRunId,
  idempotencyKey,
  main,
  MAX_NOTES,
  MAX_NOTES_BYTES,
  isSecretBearing,
  parseNotes,
  recordNotes,
} from './bs-record-notes.mjs'
import { clusterNotes } from './retro-notes.mjs'
import { validateResult } from './skill-extensions.mjs'

const ENVELOPE = {
  role: 'notes',
  core: 'boss-build',
  context: { mode: 'headless', core: 'boss-build', outcome: 'REVIEW_READY', repoId: 'r1' },
}

function note(problem, where = 'scripts/x.mjs') {
  return [problem, `Where: ${where}`, 'Why it matters: costs time.', 'Suggested fix: unknown'].join(
    '\n',
  )
}

function writeNotes(text) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'bs-record-notes-'))
  const file = path.join(dir, 'notes.md')
  fs.writeFileSync(file, text)
  return file
}

test('parseNotes shapes each block into five lines and writes the Run line itself', () => {
  const { bodies, rejected } = parseNotes(
    `${note('Gate retried twice.')}\nRun: made-up\n\n${note('Flaky test.')}\n`,
    'boss-build / REVIEW_READY / headless',
  )
  assert.deepEqual(rejected, [])
  assert.equal(bodies.length, 2)
  assert.equal(
    bodies[0],
    [
      'Gate retried twice.',
      'Where: scripts/x.mjs',
      'Why it matters: costs time.',
      'Suggested fix: unknown',
      'Run: boss-build / REVIEW_READY / headless',
    ].join('\n'),
  )
})

test('parseNotes rejects a block that is not in the labelled shape', () => {
  const { bodies, rejected } = parseNotes('just a free-form observation\n\n- a bullet', 'r')
  assert.deepEqual(bodies, [])
  assert.deepEqual(rejected, ['just a free-form observation', '- a bullet'])
})

test('isSecretBearing catches credential shapes and passes plain prose', () => {
  assert.equal(isSecretBearing('token: ghp_abcdefghijklmnopqrstuvwxyz0123'), true)
  assert.equal(isSecretBearing('-----BEGIN OPENSSH PRIVATE KEY-----'), true)
  assert.equal(isSecretBearing('the lint gate retried twice before passing'), false)
})

test('recordNotes writes one note per block and returns a valid notes envelope', () => {
  const calls = []
  const result = recordNotes(ENVELOPE, {
    notesPath: writeNotes(`${note('slow gate')}\n\n${note('flaky test')}\n`),
    extension: 'boss-build-notes',
    addNote: ({ body, repoId }) => {
      calls.push({ body, repoId })
      return `n${calls.length}`
    },
  })
  assert.equal(result.ok, true)
  assert.equal(result.extension, 'boss-build-notes')
  assert.deepEqual(
    result.items.map((item) => item.noteId),
    ['n1', 'n2'],
  )
  assert.match(calls[0].body, /^slow gate\n/)
  assert.equal(calls[0].repoId, 'r1')
  assert.equal(validateResult(result, 'notes').ok, true)
})

test('recordNotes caps the count and drops secret-bearing notes', () => {
  const blocks = [note('leak', 'api_key=supersecretvalue')]
  for (let i = 0; i < MAX_NOTES + 2; i += 1) blocks.push(note(`problem ${i}`))
  let n = 0
  const result = recordNotes(ENVELOPE, {
    notesPath: writeNotes(blocks.join('\n\n')),
    addNote: () => `n${(n += 1)}`,
  })
  assert.equal(result.items.length, MAX_NOTES)
  assert.ok(result.items.every((item) => !item.body.includes('supersecret')))
  assert.match(result.notes, /secret shape/)
  assert.match(result.notes, /over the cap/)
})

test('recordNotes fails its envelope, never throws, on missing or oversized input', () => {
  assert.equal(recordNotes(ENVELOPE, {}).ok, false)
  assert.equal(recordNotes(ENVELOPE, { notesPath: '/nonexistent/notes.md' }).ok, false)
  const big = writeNotes('x'.repeat(MAX_NOTES_BYTES + 1))
  const result = recordNotes(ENVELOPE, { notesPath: big, addNote: () => 'n' })
  assert.equal(result.ok, false)
  assert.match(result.error, /exceeds/)
})

test('recordNotes reports ok:false only when every write failed', () => {
  const notesPath = writeNotes(`${note('a')}\n\n${note('b')}`)
  const allFail = recordNotes(ENVELOPE, {
    notesPath,
    addNote: () => {
      throw new Error('daemon down')
    },
  })
  assert.equal(allFail.ok, false)
  assert.match(allFail.error, /daemon down/)
  let first = true
  const partial = recordNotes(ENVELOPE, {
    notesPath,
    addNote: () => {
      if (first) {
        first = false
        throw new Error('blip')
      }
      return 'n2'
    },
  })
  assert.equal(partial.ok, true)
  assert.equal(partial.items.length, 1)
  assert.match(partial.notes, /1 write\(s\) failed/)
})

test('run-id sanitization and precedence use explicit, chat, session then memoized adhoc', () => {
  assert.equal(sanitizeRunId(' a/b!:#@-._ '), 'ab:#@-._')
  assert.equal(sanitizeRunId('! /'), null)
  assert.equal(sanitizeRunId('a'.repeat(100)).length, 80)
  const env = { BOSS_AGENT_SESSION_ID: 'chat', BOSS_SESSION_ID: 'session' }
  assert.equal(resolveRunId({ runId: 'explicit', env }), 'explicit')
  assert.equal(resolveRunId({ runId: '!', env }), 'chat')
  assert.equal(resolveRunId({ env: { BOSS_SESSION_ID: 'session' } }), 'session')
  resetAdhocRunId()
  const id = resolveRunId({ env: {} })
  assert.match(id, /^adhoc-[a-f0-9]{8}$/)
  assert.equal(resolveRunId({ env: {} }), id)
})
test('Run line round-trip accepts separators, last line and legacy notes', () => {
  assert.equal(parseRunId(formatRunLine(['core', 'done', 'headless'], 'pr:123')), 'pr:123')
  assert.equal(parseRunId('Run: core · done · run:a'), 'a')
  assert.equal(parseRunId('Run: core / run:a\nRun: core / run:b'), 'b')
  assert.equal(parseRunId('Run: core / run:a\nRun: legacy'), null)
  assert.equal(parseRunId('Run: legacy'), null)
  const { bodies } = parseNotes(`${note('problem')}\nRun: fake / run:evil`, 'core / run:trusted')
  assert.equal(parseRunId(bodies[0]), 'trusted')
})
test('two runs with the same body write different keys into one cluster', () => {
  const notesPath = writeNotes(note('repeated problem'))
  const writes = []
  for (const id of ['a', 'b'])
    recordNotes(ENVELOPE, {
      notesPath,
      env: { BOSS_AGENT_SESSION_ID: id },
      addNote: ({ body }) => {
        writes.push(body)
        return id
      },
    })
  assert.notEqual(idempotencyKey(writes[0]), idempotencyKey(writes[1]))
  const clusters = clusterNotes(writes.map((body, i) => ({ id: String(i), body })))
  assert.equal(clusters.length, 1)
  assert.equal(clusters[0].notes.length, 2)
})
test('same-run collapse precedes cap and repeated calls preserve body and key', () => {
  const notesPath = writeNotes([note('problem'), note('problem'), note('other')].join('\n\n'))
  const writes = []
  const deps = {
    notesPath,
    env: { BOSS_AGENT_SESSION_ID: 'a' },
    addNote: ({ body }) => {
      writes.push(body)
      return 'n'
    },
  }
  const result = recordNotes(ENVELOPE, deps)
  assert.equal(writes.length, 2)
  assert.match(result.notes, /1 note\(s\) repeated within this run/)
  recordNotes(ENVELOPE, deps)
  assert.equal(writes[0], writes[2])
  assert.equal(idempotencyKey(writes[0]), idempotencyKey(writes[2]))
})
test('no-envelope CLI records a token with defaults and missing files never fail', () => {
  const writes = []
  const output = []
  const deps = {
    env: { BOSS_AGENT_SESSION_ID: 'cli' },
    addNote: ({ body }) => {
      writes.push(body)
      return 'n'
    },
    stdout: (t) => output.push(JSON.parse(t)),
  }
  assert.equal(
    main(
      ['--notes', writeNotes(note('cli problem')), '--core', 'bs-sweep-x', '--outcome', 'none'],
      deps,
    ),
    0,
  )
  assert.match(writes[0], /Run: bs-sweep-x \/ none \/ headless \/ run:cli$/)
  assert.equal(main(['--notes', '/nonexistent/notes', '--core', 'x'], deps), 0)
  assert.equal(output[1].ok, false)
})
