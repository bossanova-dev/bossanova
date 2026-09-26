// scripts/tracker/cli.test.mjs
import { test } from 'node:test'
import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { spawnSync } from 'node:child_process'
import { fileURLToPath } from 'node:url'
import { runCli, generateClaimToken, TRACKER_USAGE } from './cli.mjs'
import { buildLinearOperationMap, createLinearAdapter } from './linear.mjs'
import { TRACKER_CREDENTIALS_MISSING } from './adapter-core.mjs'
import { DEFAULT_CONFIG, mergeConfig, validateConfig } from '../skill-config.mjs'

const won = '11111111111111111111111111111111'
const lost = '22222222222222222222222222222222'
const early = '2026-01-01T00:00:00.000Z'
const late = '2026-01-02T00:00:00.000Z'
const marker = (t) => `🔒 bs-implement-claim:${t} (bs-implement run claiming this ticket)`

test('generateClaimToken returns a 32-char lowercase hex token', () => {
  const t = generateClaimToken()
  assert.match(t, /^[0-9a-f]{32}$/)
})

test('claim-token prints a fresh token and exits 0', () => {
  let out = ''
  const code = runCli(['claim-token'], { write: (s) => (out += s) })
  assert.equal(code, 0)
  assert.match(out.trim(), /^[0-9a-f]{32}$/)
})

test('claim-comment prints a legacy claim body when no session id is available', () => {
  let out = ''
  const code = runCli(['claim-comment', '--token', won], {
    write: (s) => (out += s),
    env: {},
  })
  assert.equal(code, 0)
  assert.equal(out.trim(), marker(won))
})

test('claim-comment defaults the owner suffix from BOSS_SESSION_ID', () => {
  let out = ''
  const code = runCli(['claim-comment', '--token', won], {
    write: (s) => (out += s),
    env: { BOSS_SESSION_ID: 'session-123' },
  })
  assert.equal(code, 0)
  assert.equal(out.trim(), `${marker(won)} owner:session-123`)
})

test('claim-comment exits 2 when the session id cannot be recovered by the parser', () => {
  let err = ''
  const code = runCli(['claim-comment', '--token', won], {
    errWrite: (s) => (err += s),
    env: { BOSS_SESSION_ID: 'session/123' },
  })
  assert.equal(code, 2)
  assert.match(err, /invalid claim session id/)
})

test('claim-verdict exits 0 when my token is the first writer (WON)', () => {
  const comments = JSON.stringify([
    { body: marker(won), createdAt: early },
    { body: marker(lost), createdAt: late },
  ])
  const code = runCli(['claim-verdict', '--me', won, '--comments', comments], {})
  assert.equal(code, 0)
})

test('claim-verdict exits 3 when another token wins first-writer (LOST)', () => {
  const comments = JSON.stringify([
    { body: marker(won), createdAt: early },
    { body: marker(lost), createdAt: late },
  ])
  const code = runCli(['claim-verdict', '--me', lost, '--comments', comments], {})
  assert.equal(code, 3)
})

test('claim-verdict threads liveness evidence through resolveClaim', () => {
  const comments = JSON.stringify([
    { body: `${marker(won)} owner:dead-session`, createdAt: early },
    { body: marker(lost), createdAt: late },
  ])
  const liveness = JSON.stringify({
    now: '2026-01-02T00:10:00.000Z',
    inactiveAfterMs: 60_000,
    sessions: {
      'dead-session': { lastActivityAt: '2026-01-01T00:00:00.000Z' },
    },
  })
  const code = runCli(
    ['claim-verdict', '--me', lost, '--comments', comments, '--liveness', liveness],
    {},
  )
  assert.equal(code, 0)
})

test('claim-verdict exits 4 when forfeiture leaves no winner', () => {
  const comments = JSON.stringify([{ body: `${marker(won)} owner:dead-session`, createdAt: early }])
  const liveness = JSON.stringify({
    now: '2026-01-02T00:10:00.000Z',
    inactiveAfterMs: 60_000,
    sessions: {
      'dead-session': { lastActivityAt: '2026-01-01T00:00:00.000Z' },
    },
  })
  let out = ''
  const code = runCli(
    ['claim-verdict', '--me', won, '--comments', comments, '--liveness', liveness],
    { write: (s) => (out += s) },
  )
  assert.equal(code, 4)
  assert.equal(out, 'NO_WINNER\n')
})

test('claim-verdict exits 2 for malformed liveness evidence', () => {
  const comments = JSON.stringify([{ body: marker(won), createdAt: early }])
  let err = ''
  const code = runCli(['claim-verdict', '--me', won, '--comments', comments, '--liveness', '{'], {
    errWrite: (s) => (err += s),
  })
  assert.equal(code, 2)
  assert.match(err, /--liveness: malformed JSON/)
})

test('claim-verdict exits 2 for primitive liveness evidence', () => {
  const comments = JSON.stringify([{ body: marker(won), createdAt: early }])
  let err = ''
  const code = runCli(
    ['claim-verdict', '--me', won, '--comments', comments, '--liveness', 'true'],
    {
      errWrite: (s) => (err += s),
    },
  )
  assert.equal(code, 2)
  assert.match(err, /claim arbitration failed: claim liveness options must be an object/)
})

test('claim-verdict exits 2 for malformed comments evidence', () => {
  let err = ''
  const code = runCli(['claim-verdict', '--me', won, '--comments', '{'], {
    errWrite: (s) => (err += s),
  })
  assert.equal(code, 2)
  assert.match(err, /--comments: malformed JSON/)
})

test('claim-verdict exits 2 for invalid liveness evidence', () => {
  const comments = JSON.stringify([{ body: `${marker(won)} owner:session-a`, createdAt: early }])
  const liveness = JSON.stringify({
    now: 'not-a-date',
    inactiveAfterMs: 60_000,
    sessions: {
      'session-a': { lastActivityAt: early },
    },
  })
  let err = ''
  const code = runCli(
    ['claim-verdict', '--me', won, '--comments', comments, '--liveness', liveness],
    { errWrite: (s) => (err += s) },
  )
  assert.equal(code, 2)
  assert.match(err, /claim arbitration failed: invalid claim liveness now/)
})

test('claim-verdict without --comments exits 2 (parity with the required arg)', () => {
  let err = ''
  const code = runCli(['claim-verdict', '--me', won], { errWrite: (s) => (err += s) })
  assert.equal(code, 2)
  assert.match(err, /--comments <json-array> is required/)
})

test('an unknown capability exits 2', () => {
  let err = ''
  const code = runCli(['bogus'], { errWrite: (s) => (err += s) })
  assert.equal(code, 2)
  assert.match(err, /unknown tracker capability: bogus/)
})

// Writes `body` to a fresh temp file and returns its path; caller cleans up.
function writeTempBody(body) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'tracker-cli-test-'))
  const file = path.join(dir, 'body.txt')
  fs.writeFileSync(file, body)
  return file
}

test('update-comment writes the JSON descriptor with the tool name and {id, body} args, exits 0', () => {
  const bodyFile = writeTempBody('progress update')
  try {
    let out = ''
    const code = runCli(['update-comment', '--id', 'comment-1', '--body-file', bodyFile], {
      write: (s) => (out += s),
      env: { LINEAR_API_KEY: 'k' },
    })
    assert.equal(code, 0)
    assert.equal(out.endsWith('\n'), true)
    const descriptor = JSON.parse(out)
    assert.equal(descriptor.tool, 'mcp__bossanova-linear__save_comment')
    assert.deepEqual(descriptor.args, { id: 'comment-1', body: 'progress update' })
  } finally {
    fs.rmSync(path.dirname(bodyFile), { recursive: true, force: true })
  }
})

test('update-comment passes the body verbatim (trailing newline and marker line preserved)', () => {
  const body = 'line one\n<!-- marker -->\n'
  const bodyFile = writeTempBody(body)
  try {
    let out = ''
    const code = runCli(['update-comment', '--id', 'comment-1', '--body-file', bodyFile], {
      write: (s) => (out += s),
      env: { LINEAR_API_KEY: 'k' },
    })
    assert.equal(code, 0)
    const descriptor = JSON.parse(out)
    assert.equal(descriptor.args.body, body)
  } finally {
    fs.rmSync(path.dirname(bodyFile), { recursive: true, force: true })
  }
})

test('update-comment without --id exits 2 with a message', () => {
  const bodyFile = writeTempBody('x')
  try {
    let err = ''
    const code = runCli(['update-comment', '--body-file', bodyFile], {
      errWrite: (s) => (err += s),
      env: { LINEAR_API_KEY: 'k' },
    })
    assert.equal(code, 2)
    assert.match(err, /--id/)
  } finally {
    fs.rmSync(path.dirname(bodyFile), { recursive: true, force: true })
  }
})

test('update-comment without --body-file exits 2 with a message', () => {
  let err = ''
  const code = runCli(['update-comment', '--id', 'comment-1'], {
    errWrite: (s) => (err += s),
    env: { LINEAR_API_KEY: 'k' },
  })
  assert.equal(code, 2)
  assert.match(err, /--body-file/)
})

test('update-comment with an empty or whitespace-only body file exits 2 rather than emitting a blanking descriptor', () => {
  // A blank update erases the target comment along with its marker anchor, so
  // the next run finds nothing and posts a duplicate — the exact failure the
  // single-comment protocol exists to prevent, and the same input the
  // progress-comment toolbox's own upsert planner throws on.
  for (const blank of ['', '   \n  ']) {
    const bodyFile = writeTempBody(blank)
    try {
      let err = ''
      let out = ''
      const code = runCli(['update-comment', '--id', 'comment-1', '--body-file', bodyFile], {
        write: (s) => (out += s),
        errWrite: (s) => (err += s),
        env: { LINEAR_API_KEY: 'k' },
      })
      assert.equal(code, 2, `expected exit 2 for body ${JSON.stringify(blank)}`)
      assert.equal(out, '', 'must not emit a descriptor for a blank body')
      assert.match(err, /empty/)
    } finally {
      fs.rmSync(path.dirname(bodyFile), { recursive: true, force: true })
    }
  }
})

test('update-comment with an unreadable body file exits 2, naming the path', () => {
  const missingPath = path.join(os.tmpdir(), 'tracker-cli-test-does-not-exist', 'body.txt')
  let err = ''
  const code = runCli(['update-comment', '--id', 'comment-1', '--body-file', missingPath], {
    errWrite: (s) => (err += s),
    env: { LINEAR_API_KEY: 'k' },
  })
  assert.equal(code, 2)
  assert.match(err, new RegExp(missingPath.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')))
})

test('update-comment against an updateComment entry with no usable tool exits 2 instead of emitting a toolless descriptor', () => {
  // An entry that exists but names no MCP tool is as unusable as an absent one;
  // emitting `{"tool":""}` and exiting 0 would defer the failure to whatever
  // tried to execute the descriptor.
  for (const op of [{}, { tool: '' }, { tool: '   ' }, { tool: 42 }]) {
    const bodyFile = writeTempBody('progress update')
    try {
      let err = ''
      let out = ''
      const code = runCli(['update-comment', '--id', 'comment-1', '--body-file', bodyFile], {
        write: (s) => (out += s),
        errWrite: (s) => (err += s),
        env: {},
        resolveAdapter: () => ({ operationMap: { updateComment: op } }),
      })
      assert.equal(code, 2, `expected exit 2 for op ${JSON.stringify(op)}`)
      assert.equal(out, '', 'must not emit a descriptor without a usable tool')
      assert.match(err, /has no tool/)
    } finally {
      fs.rmSync(path.dirname(bodyFile), { recursive: true, force: true })
    }
  }
})

test('update-comment against an adapter whose operationMap has no updateComment entry exits 2, naming the gap', () => {
  const bodyFile = writeTempBody('progress update')
  try {
    let err = ''
    const stubAdapter = { operationMap: {} }
    const code = runCli(['update-comment', '--id', 'comment-1', '--body-file', bodyFile], {
      errWrite: (s) => (err += s),
      env: {},
      resolveAdapter: () => stubAdapter,
    })
    assert.equal(code, 2)
    assert.match(err, /update-comment: resolved tracker adapter has no updateComment operation/)
  } finally {
    fs.rmSync(path.dirname(bodyFile), { recursive: true, force: true })
  }
})

// --- the `write-description` subcommand (BOS-1198) ---------------------------

test('write-description emits the descriptor with the tool name, {id, description} args and the on-disk byte count', () => {
  const body = '## Summary\n\nplan body\n'
  const bodyFile = writeTempBody(body)
  try {
    let out = ''
    const code = runCli(['write-description', '--id', 'ISSUE-1', '--body-file', bodyFile], {
      write: (s) => (out += s),
      env: { LINEAR_API_KEY: 'k' },
    })
    assert.equal(code, 0)
    assert.equal(out.endsWith('\n'), true)
    const descriptor = JSON.parse(out)
    assert.equal(descriptor.tool, 'mcp__bossanova-linear__save_issue')
    assert.deepEqual(descriptor.args, { id: 'ISSUE-1', description: body })
    assert.equal(descriptor.bytes, fs.statSync(bodyFile).size)
    // The explicit outcome the caller branches on. Without it a caller has only exit 0,
    // which a write that changed nothing shares with a write that landed.
    assert.equal(descriptor.outcome, 'descriptor-emitted')
  } finally {
    fs.rmSync(path.dirname(bodyFile), { recursive: true, force: true })
  }
})

test('write-description reports the BYTE count on disk, not the character count, for a multi-byte body', () => {
  // A character count silently disagrees with the file the caller gated: these
  // bodies are deliberately shorter in code points than in bytes, so a length-based
  // measurement would pass every other assertion in this file and still be wrong.
  for (const body of ['— em dash ✅ 🎉\n', 'Ünïcödé ハロー\n']) {
    const bodyFile = writeTempBody(body)
    try {
      let out = ''
      const code = runCli(['write-description', '--id', 'ISSUE-1', '--body-file', bodyFile], {
        write: (s) => (out += s),
        env: { LINEAR_API_KEY: 'k' },
      })
      assert.equal(code, 0)
      const descriptor = JSON.parse(out)
      assert.equal(descriptor.bytes, Buffer.byteLength(body, 'utf8'))
      assert.notEqual(
        descriptor.bytes,
        body.length,
        `fixture must have byteLength != length, got ${JSON.stringify(body)}`,
      )
    } finally {
      fs.rmSync(path.dirname(bodyFile), { recursive: true, force: true })
    }
  }
})

test('write-description passes the body verbatim, byte for byte, including its trailing newline', () => {
  // The whole point of the verb: the bytes the local gates validated are the bytes
  // that reach the tracker. A body carrying the verbatim block's own markers must
  // survive untouched — no trimming, no re-wrapping, no added terminal byte.
  const body = '## Original notes\n\n- a bullet\n\n<!-- marker -->'
  const bodyFile = writeTempBody(body)
  try {
    let out = ''
    const code = runCli(['write-description', '--id', 'ISSUE-1', '--body-file', bodyFile], {
      write: (s) => (out += s),
      env: { LINEAR_API_KEY: 'k' },
    })
    assert.equal(code, 0)
    assert.equal(JSON.parse(out).args.description, body)
  } finally {
    fs.rmSync(path.dirname(bodyFile), { recursive: true, force: true })
  }
})

test('write-description without --id exits 2 with a message and writes nothing to stdout', () => {
  const bodyFile = writeTempBody('x')
  try {
    let err = ''
    let out = ''
    const code = runCli(['write-description', '--body-file', bodyFile], {
      write: (s) => (out += s),
      errWrite: (s) => (err += s),
      env: { LINEAR_API_KEY: 'k' },
    })
    assert.equal(code, 2)
    assert.equal(out, '')
    assert.match(err, /--id/)
  } finally {
    fs.rmSync(path.dirname(bodyFile), { recursive: true, force: true })
  }
})

test('write-description without --body-file exits 2 with a message and writes nothing to stdout', () => {
  let err = ''
  let out = ''
  const code = runCli(['write-description', '--id', 'ISSUE-1'], {
    write: (s) => (out += s),
    errWrite: (s) => (err += s),
    env: { LINEAR_API_KEY: 'k' },
  })
  assert.equal(code, 2)
  assert.equal(out, '')
  assert.match(err, /--body-file/)
})

test('write-description with an empty or whitespace-only body file refuses to blank the description', () => {
  // The most destructive input this verb can receive, and the reason the guard is
  // load-bearing rather than defensive: the tracker keeps no description history, so a
  // blanked description destroys the only surviving copy of the reporter's original
  // notes. Nothing may reach stdout, or a caller piping the descriptor would execute it.
  for (const blank of ['', '   \n  ', '\t\n']) {
    const bodyFile = writeTempBody(blank)
    try {
      let err = ''
      let out = ''
      const code = runCli(['write-description', '--id', 'ISSUE-1', '--body-file', bodyFile], {
        write: (s) => (out += s),
        errWrite: (s) => (err += s),
        env: { LINEAR_API_KEY: 'k' },
      })
      assert.equal(code, 2, `expected exit 2 for body ${JSON.stringify(blank)}`)
      assert.equal(out, '', 'must not emit a descriptor for a blank body')
      assert.match(err, /empty; refusing to blank the description/)
    } finally {
      fs.rmSync(path.dirname(bodyFile), { recursive: true, force: true })
    }
  }
})

test('write-description with a missing or unreadable body file exits 2, naming the path, stdout empty', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'tracker-cli-test-'))
  const missingPath = path.join(dir, 'does-not-exist', 'body.md')
  // A DIRECTORY is readable-as-a-path but not as a body: readFileSync throws EISDIR,
  // which is the unreadable-but-present case a missing-file test alone would not reach.
  const unreadable = [missingPath, dir]
  try {
    for (const candidate of unreadable) {
      let err = ''
      let out = ''
      const code = runCli(['write-description', '--id', 'ISSUE-1', '--body-file', candidate], {
        write: (s) => (out += s),
        errWrite: (s) => (err += s),
        env: { LINEAR_API_KEY: 'k' },
      })
      assert.equal(code, 2, `expected exit 2 for ${candidate}`)
      assert.equal(out, '', 'must not emit a descriptor for an unreadable body file')
      assert.match(err, new RegExp(candidate.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')))
    }
  } finally {
    fs.rmSync(dir, { recursive: true, force: true })
  }
})

test('write-description with a body file that is not valid UTF-8 exits 2 rather than emitting a corrupted description', () => {
  // The silent sibling of the blank-body case. readFileSync(..., 'utf8') does NOT throw on
  // malformed input — it substitutes U+FFFD — so without the guard the descriptor would
  // carry corrupted text while `bytes` (from stat) still attested to the intact on-disk
  // size and `outcome` still read `descriptor-emitted`. The write replaces the whole
  // description and the tracker keeps no history, so that corruption is unrecoverable.
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'tracker-cli-test-'))
  const bodyFile = path.join(dir, 'body.md')
  // 0xC3 begins a 2-byte sequence that 0x28 cannot continue, so this is genuinely
  // malformed rather than merely non-ASCII.
  const raw = Buffer.from([0x41, 0xc3, 0x28, 0x0a])
  fs.writeFileSync(bodyFile, raw)
  try {
    // Prove the fixture actually round-trips lossily, or the guard below is vacuous.
    assert.notEqual(
      Buffer.byteLength(fs.readFileSync(bodyFile, 'utf8'), 'utf8'),
      raw.length,
      'fixture must decode lossily, otherwise this test asserts nothing',
    )
    let err = ''
    let out = ''
    const code = runCli(['write-description', '--id', 'ISSUE-1', '--body-file', bodyFile], {
      write: (s) => (out += s),
      errWrite: (s) => (err += s),
      env: { LINEAR_API_KEY: 'k' },
    })
    assert.equal(code, 2)
    assert.equal(out, '', 'must not emit a descriptor for a body that did not decode cleanly')
    assert.match(err, /is not valid UTF-8/)
    assert.match(err, /refusing to write a corrupted description/)
  } finally {
    fs.rmSync(dir, { recursive: true, force: true })
  }
})

test('write-description against an adapter that does not declare writeDescription exits 2, naming the gap', () => {
  // The optional-capability path: the caller must be told the capability is absent so it
  // can fall back to an inline save, never handed a descriptor for a tool that does not exist.
  const bodyFile = writeTempBody('plan body')
  try {
    let err = ''
    let out = ''
    const code = runCli(['write-description', '--id', 'ISSUE-1', '--body-file', bodyFile], {
      write: (s) => (out += s),
      errWrite: (s) => (err += s),
      env: {},
      resolveAdapter: () => ({ operationMap: {} }),
    })
    assert.equal(code, 2)
    assert.equal(out, '')
    assert.match(err, /has no writeDescription operation/)
  } finally {
    fs.rmSync(path.dirname(bodyFile), { recursive: true, force: true })
  }
})

test('write-description against a writeDescription entry with no usable tool exits 2 instead of emitting a toolless descriptor', () => {
  for (const op of [{}, { tool: '' }, { tool: '   ' }, { tool: 42 }]) {
    const bodyFile = writeTempBody('plan body')
    try {
      let err = ''
      let out = ''
      const code = runCli(['write-description', '--id', 'ISSUE-1', '--body-file', bodyFile], {
        write: (s) => (out += s),
        errWrite: (s) => (err += s),
        env: {},
        resolveAdapter: () => ({ operationMap: { writeDescription: op } }),
      })
      assert.equal(code, 2, `expected exit 2 for op ${JSON.stringify(op)}`)
      assert.equal(out, '', 'must not emit a descriptor without a usable tool')
      assert.match(err, /has no tool/)
    } finally {
      fs.rmSync(path.dirname(bodyFile), { recursive: true, force: true })
    }
  }
})

// --- the `states` subcommand (BOS-524) --------------------------------------

test('states prints the adapter JSON map plus a newline and exits 0', () => {
  let out = ''
  const map = { planned: 'Ready', inProgress: 'Doing', inReview: 'Reviewing' }
  const code = runCli(['states'], {
    write: (s) => (out += s),
    env: {},
    resolveAdapter: () => ({ states: () => map }),
  })
  assert.equal(code, 0)
  assert.equal(out.endsWith('\n'), true)
  assert.deepEqual(JSON.parse(out), map)
})

test('states passes a null role through so the caller can fall back per role', () => {
  let out = ''
  const code = runCli(['states'], {
    write: (s) => (out += s),
    env: {},
    resolveAdapter: () => ({ states: () => ({ planned: null, inProgress: 'Doing' }) }),
  })
  assert.equal(code, 0)
  assert.deepEqual(JSON.parse(out), { planned: null, inProgress: 'Doing' })
})

test('states against an adapter with no states capability exits 2 with an EMPTY stdout', () => {
  // Callers invoke this as `... states 2>/dev/null || true` and fall back to their own
  // config read, so an absent capability must be a clean no-output exit 2 — never a
  // partial/empty map on stdout, which the caller would parse as an answer.
  for (const adapter of [{}, { states: undefined }, { states: null }, { states: 'nope' }]) {
    let out = ''
    let err = ''
    const code = runCli(['states'], {
      write: (s) => (out += s),
      errWrite: (s) => (err += s),
      env: {},
      resolveAdapter: () => adapter,
    })
    assert.equal(code, 2, `expected exit 2 for adapter ${JSON.stringify(adapter)}`)
    assert.equal(out, '', 'must print nothing on stdout without the capability')
    assert.match(err, /no states capability/)
  }
})

test('a states capability that VIOLATES its never-throw contract still exits 2, not a crash', () => {
  // The contract says states() never throws, but this CLI cannot assume a vendored
  // adapter honors it: a throw must degrade to the caller's config fallback exactly
  // like an absent capability, never take down a caller that has a good fallback.
  let out = ''
  let err = ''
  const code = runCli(['states'], {
    write: (s) => (out += s),
    errWrite: (s) => (err += s),
    env: {},
    resolveAdapter: () => ({
      states: () => {
        throw new Error('tracker unreachable')
      },
    }),
  })
  assert.equal(code, 2)
  assert.equal(out, '', 'a throwing capability must print nothing on stdout')
  assert.match(err, /threw: tracker unreachable/)
})

test('a states capability returning a non-object exits 2 rather than printing `undefined`', () => {
  // JSON.stringify(undefined) is the JS value undefined, which `write` would emit as
  // the literal text "undefined" with exit 0 — neither valid JSON nor an empty stdout.
  for (const bad of [undefined, null, 'planned', 42]) {
    let out = ''
    let err = ''
    const code = runCli(['states'], {
      write: (s) => (out += s),
      errWrite: (s) => (err += s),
      env: {},
      resolveAdapter: () => ({ states: () => bad }),
    })
    assert.equal(code, 2, `expected exit 2 for states() => ${JSON.stringify(bad)}`)
    assert.equal(out, '', 'must never print a non-object map')
    assert.match(err, /non-object/)
  }
})

test('the Linear reference states() tolerates an explicit null opts (never-throw contract)', () => {
  // `states: (opts) => linearStates(opts)` passes its argument straight through, and a
  // destructuring default fires only on undefined — an explicit null must not TypeError.
  const adapter = createLinearAdapter({ apiKey: 'k' })
  assert.doesNotThrow(() => adapter.states(null))
  assert.doesNotThrow(() => adapter.states())
})

test('states through the real Linear adapter emits a parseable map (end-to-end)', () => {
  let out = ''
  const code = runCli(['states'], { write: (s) => (out += s), env: { LINEAR_API_KEY: 'k' } })
  assert.equal(code, 0)
  const map = JSON.parse(out)
  assert.equal(map !== null && typeof map === 'object', true)
  assert.ok('planned' in map, 'the real adapter must answer for the planned role')
})

// ---------------------------------------------------------------------------
// Shape acceptance (BOS-1244 row 12)
// ---------------------------------------------------------------------------

test('claim-verdict accepts both a bare array and the {comments:[…]} envelope, identically', () => {
  // `list_comments` returns the envelope. It used to reach `for (const c of comments)`, throw
  // `comments is not iterable`, and be reported as `claim arbitration failed` — a diagnostic naming
  // malformed EVIDENCE when the fault was an argument shape.
  const list = [
    { body: marker(won), createdAt: early },
    { body: marker(lost), createdAt: late },
  ]
  for (const [label, me, expected] of [
    ['WON', won, 0],
    ['LOST', lost, 3],
  ]) {
    const bare = runCli(['claim-verdict', '--me', me, '--comments', JSON.stringify(list)], {})
    const enveloped = runCli(
      ['claim-verdict', '--me', me, '--comments', JSON.stringify({ comments: list })],
      {},
    )
    assert.equal(bare, expected, `${label}: bare array`)
    assert.equal(enveloped, expected, `${label}: envelope must produce the same verdict`)
  }

  // The envelope's other fields do not matter, only that it carries a `comments` array.
  const withExtras = runCli(
    ['claim-verdict', '--me', won, '--comments', JSON.stringify({ comments: list, cursor: 'abc' })],
    {},
  )
  assert.equal(withExtras, 0)
})

test('claim-verdict refuses any other --comments shape by name, not as a claim failure', () => {
  for (const bad of ['"a string"', '42', 'null', '{"nodes":[]}', '{}']) {
    let err = ''
    const code = runCli(['claim-verdict', '--me', won, '--comments', bad], {
      errWrite: (s) => (err += s),
    })
    assert.equal(code, 2, `${bad} must exit 2`)
    assert.match(err, /--comments \(/, `${bad}: the message must name the flag`)
    assert.match(err, /envelope/, `${bad}: and the two accepted shapes`)
    assert.doesNotMatch(
      err,
      /claim arbitration failed/,
      `${bad}: a wrong shape must not be reported as malformed evidence`,
    )
    assert.doesNotMatch(err, / a object/, `${bad}: the descriptor must read as English`)
  }

  // BOS-1244 review round 1 (boss-review-thermonuclear + boss-review-ce). The descriptor names the
  // KEYS an object carried, matching `normalizeClaimComments` in the library this flag fronts — a
  // bare `a object` discarded exactly the detail that identifies the likeliest remaining misuse.
  let nodesErr = ''
  runCli(['claim-verdict', '--me', won, '--comments', '{"nodes":[]}'], {
    errWrite: (s) => (nodesErr += s),
  })
  assert.match(nodesErr, /an object with keys nodes/, 'the offending key is named back')
  let emptyErr = ''
  runCli(['claim-verdict', '--me', won, '--comments', '{}'], {
    errWrite: (s) => (emptyErr += s),
  })
  assert.match(emptyErr, /an object with keys /, 'and an empty object still reads as an object')
})

// --- list-planned (BOS-1294) ----------------------------------------------------
// The worker's narrowed candidate read. Defaults come from the repo config through the SAME helper
// the cron gate reads; explicit flags override. Config and adapter are both injected, so no test
// here reads this repo's real config or reaches a network.

/** A validated config whose tracker block carries the given `selection` (or none). */
function plannedCliConfig(selection) {
  const config = mergeConfig(DEFAULT_CONFIG, {
    adapters: { ...DEFAULT_CONFIG.adapters, tracker: 'demo' },
    trackerConfig: {
      demo: {
        mcpServer: 'demo-tracker',
        team: 'Demo',
        states: { planned: 'Planned' },
        labels: { agentFriendly: 'agent-friendly' },
        ...(selection === undefined ? {} : { selection }),
      },
    },
  })
  validateConfig(config, 'test')
  return config
}

/** Run list-planned against a recording stub adapter; resolves to what it printed and was asked. */
async function listPlanned(args, { result = [], config = plannedCliConfig(), adapter } = {}) {
  const calls = []
  let out = ''
  let err = ''
  let configLoads = 0
  const stub = adapter ?? {
    selectPlanned: async (query) => {
      calls.push(query)
      return typeof result === 'function' ? result() : result
    },
  }
  const code = await runCli(['list-planned', ...args], {
    write: (s) => (out += s),
    errWrite: (s) => (err += s),
    env: {},
    resolveAdapter: () => stub,
    loadConfig: () => {
      configLoads += 1
      if (config instanceof Error) throw config
      return config
    },
  })
  return { code, out, err, calls, configLoads }
}

test('list-planned with no flags queries the config-derived selection and prints a JSON array', async () => {
  const nodes = [{ identifier: 'DEMO-1', labels: ['agent-friendly'], attachments: [] }]
  const { code, out, err, calls } = await listPlanned([], { result: nodes })
  assert.equal(code, 0)
  assert.equal(err, '')
  assert.ok(out.endsWith('\n'))
  assert.deepEqual(JSON.parse(out), nodes)
  // Exactly the gate's un-narrowed query, plus the Step 2 window — no identity key at all.
  assert.deepEqual(calls, [{ state: 'Planned', label: 'agent-friendly', limit: 250 }])
})

test('list-planned defaults pick up a configured selection block, identically to the gate', async () => {
  const { code, calls } = await listPlanned([], {
    config: plannedCliConfig({ assigneeOrCreator: 'me', labels: ['label-a', 'label-b'] }),
  })
  assert.equal(code, 0)
  assert.deepEqual(calls, [
    { state: 'Planned', label: ['label-a', 'label-b'], assigneeOrCreator: 'me', limit: 250 },
  ])
})

test('list-planned parses all four flags; explicit flags override the config', async () => {
  const { code, calls } = await listPlanned([
    '--state',
    'Ready',
    '--label',
    'label-a,label-b',
    '--label',
    'label-c',
    '--assignee-or-creator',
    'usr_9',
    '--limit',
    '25',
  ])
  assert.equal(code, 0)
  assert.deepEqual(calls, [
    {
      state: 'Ready',
      label: ['label-a', 'label-b', 'label-c'],
      assigneeOrCreator: 'usr_9',
      limit: 25,
    },
  ])
  assert.equal(typeof calls[0].limit, 'number', '--limit is parsed as an integer')
})

test('list-planned refuses selection override flags when a selection block is configured', async () => {
  const config = plannedCliConfig({ assigneeOrCreator: 'me', labels: ['label-a'] })
  for (const [args, flag] of [
    [['--label', 'agent-friendly'], '--label'],
    [['--state', 'Other'], '--state'],
    [['--assignee-or-creator', 'usr_other'], '--assignee-or-creator'],
  ]) {
    const { code, out, err, calls } = await listPlanned(args, { config })
    assert.equal(code, 2, JSON.stringify(args))
    assert.equal(out, '', `${JSON.stringify(args)} prints nothing on stdout`)
    assert.match(err, /^list-planned: /, JSON.stringify(args))
    assert.ok(err.includes(flag), `${JSON.stringify(args)} names the refused flag`)
    assert.equal(err.trim().split('\n').length, 1, 'a one-line diagnostic')
    assert.equal(calls.length, 0, `${JSON.stringify(args)} must never reach the adapter`)
  }
})

test('list-planned still accepts --limit under a configured selection, keeping the narrowing', async () => {
  const { code, calls } = await listPlanned(['--limit', '10'], {
    config: plannedCliConfig({ assigneeOrCreator: 'me', labels: ['label-a'] }),
  })
  assert.equal(code, 0)
  assert.deepEqual(calls, [
    { state: 'Planned', label: ['label-a'], assigneeOrCreator: 'me', limit: 10 },
  ])
})

test('list-planned keeps a single --label a single name, not a one-element set', async () => {
  const { calls } = await listPlanned(['--label', 'label-a'])
  assert.equal(calls[0].label, 'label-a')
})

test('list-planned exits 2 with EMPTY stdout when the adapter lacks the selectPlanned capability', async () => {
  for (const adapter of [
    {},
    { selectPlanned: undefined },
    { selectPlanned: null },
    { selectPlanned: 'nope' },
  ]) {
    const { code, out, err, configLoads } = await listPlanned([], { adapter })
    assert.equal(code, 2, JSON.stringify(adapter))
    assert.equal(out, '', 'nothing on stdout without the capability')
    assert.match(err, /no selectPlanned capability/)
    assert.equal(err.trim().split('\n').length, 1, 'a one-line diagnostic')
    assert.equal(configLoads, 0, 'the capability is checked before any config work')
  }
})

test('list-planned rejects an invalid --limit, a bad --label, an unknown or valueless flag', async () => {
  for (const args of [
    ['--limit', '0'],
    ['--limit', '251'],
    ['--limit', 'abc'],
    ['--limit', '2.5'],
    ['--limit', '10abc'],
    ['--label', 'label-a,,label-b'],
    ['--label', ''],
    ['--state', ''],
    ['--assignee-or-creator', ''],
    ['--state'],
    ['--team', 'Other'],
  ]) {
    const { code, out, err, calls } = await listPlanned(args)
    assert.equal(code, 2, JSON.stringify(args))
    assert.equal(out, '', JSON.stringify(args))
    assert.match(err, /^list-planned: /, JSON.stringify(args))
    assert.equal(calls.length, 0, `${JSON.stringify(args)} must never reach the adapter`)
  }
})

test('list-planned fails closed — exit 2, empty stdout — on config, adapter, or result failure', async () => {
  const cases = [
    [{ config: new Error('skill-config: broken') }, /skill-config: broken/],
    [{ result: () => Promise.reject(new Error('Linear API HTTP 500\nsecond line')) }, /HTTP 500/],
    [{ result: { nodes: [] } }, /non-array/],
    [{ result: null }, /non-array/],
  ]
  for (const [options, pattern] of cases) {
    const { code, out, err } = await listPlanned([], options)
    assert.equal(code, 2)
    assert.equal(out, '')
    assert.match(err, pattern)
    assert.equal(err.trim().split('\n').length, 1, 'the diagnostic stays on one line')
  }
  // A config without the planned state cannot derive a default, so it fails rather than widening.
  const noState = plannedCliConfig()
  delete noState.trackerConfig.demo.states.planned
  const { code, out, err } = await listPlanned([], { config: noState })
  assert.equal(code, 2)
  assert.equal(out, '')
  assert.match(err, /states\.planned/)
})

// --- read-description (BOS-1303) -----------------------------------------------
// The code-written read every stored-description gate compares against. The bytes on disk must be
// exactly what the tracker stored, a failed read must never leave or alter a file at --out-file,
// and every failure must be a non-zero exit with empty stdout — a caller that sees exit 0 trusts
// the file.

/** A scratch dir for one test, removed afterwards. */
function withScratch(fn) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'tracker-read-description-'))
  return Promise.resolve(fn(dir)).finally(() => fs.rmSync(dir, { recursive: true, force: true }))
}

/** Run read-description against a stub adapter; resolves to what it printed and was asked. */
async function readDescription(args, { adapter, resolveAdapter } = {}) {
  const calls = []
  let out = ''
  let err = ''
  const stub = adapter ?? {
    readDescription: async (id) => {
      calls.push(id)
      return { id: 'u-1', identifier: 'BOS-1', description: 'a\nb' }
    },
  }
  const pending = runCli(['read-description', ...args], {
    write: (s) => (out += s),
    errWrite: (s) => (err += s),
    env: {},
    resolveAdapter: resolveAdapter ?? (() => stub),
  })
  const code = await pending
  return { pending, code, out, err, calls }
}

/** Every non-dotfile and dotfile entry left in `dir`, so a surviving temp sibling is visible. */
const entries = (dir) => fs.readdirSync(dir).sort()

test('read-description writes the stored bytes verbatim and prints one receipt line', async () => {
  await withScratch(async (dir) => {
    const outFile = path.join(dir, 'BOS-1.image-guard-orig.md')
    const { pending, code, out, err, calls } = await readDescription([
      '--id',
      'u-1',
      '--out-file',
      outFile,
    ])
    assert.ok(pending instanceof Promise, 'read-description settles asynchronously')
    assert.equal(code, 0)
    assert.equal(err, '')
    assert.deepEqual(calls, ['u-1'])
    // Exactly the three stored bytes: no trailing newline appended.
    assert.deepEqual(fs.readFileSync(outFile), Buffer.from('a\nb'))
    assert.ok(out.endsWith('\n') && out.trim().split('\n').length === 1, 'one JSON line')
    const receipt = JSON.parse(out)
    assert.deepEqual(receipt, {
      bytes: 3,
      outcome: 'stored-description-written',
      id: 'u-1',
      identifier: 'BOS-1',
    })
    assert.equal(receipt.bytes, fs.statSync(outFile).size, 'bytes is the stat(2) size')
    assert.deepEqual(entries(dir), ['BOS-1.image-guard-orig.md'], 'no temp sibling survives')
  })
})

test('read-description reports the on-disk BYTE count for a multi-byte description', async () => {
  await withScratch(async (dir) => {
    const description = 'naïve — 日本語\n'
    const outFile = path.join(dir, 'multi.md')
    const { code, out } = await readDescription(['--id', 'u-1', '--out-file', outFile], {
      adapter: { readDescription: async () => ({ id: 'u-1', identifier: 'BOS-1', description }) },
    })
    assert.equal(code, 0)
    assert.equal(fs.readFileSync(outFile, 'utf8'), description)
    assert.equal(JSON.parse(out).bytes, Buffer.byteLength(description, 'utf8'))
  })
})

test('read-description writes a zero-byte file with the distinct empty outcome', async () => {
  await withScratch(async (dir) => {
    const outFile = path.join(dir, 'empty.md')
    const { code, out } = await readDescription(['--id', 'u-1', '--out-file', outFile], {
      adapter: {
        readDescription: async () => ({ id: 'u-1', identifier: 'BOS-1', description: '' }),
      },
    })
    assert.equal(code, 0)
    assert.equal(fs.statSync(outFile).size, 0)
    assert.deepEqual(JSON.parse(out), {
      bytes: 0,
      outcome: 'stored-description-empty',
      id: 'u-1',
      identifier: 'BOS-1',
    })
  })
})

test('read-description replaces a pre-existing out-file atomically on success', async () => {
  await withScratch(async (dir) => {
    const outFile = path.join(dir, 'stored.md')
    fs.writeFileSync(outFile, 'an older, longer snapshot\n')
    const { code } = await readDescription(['--id', 'u-1', '--out-file', outFile])
    assert.equal(code, 0)
    assert.equal(fs.readFileSync(outFile, 'utf8'), 'a\nb')
    assert.deepEqual(entries(dir), ['stored.md'])
  })
})

test('read-description exits 2 naming the getIssue fallback when the adapter lacks the capability', async () => {
  await withScratch(async (dir) => {
    const outFile = path.join(dir, 'x.md')
    for (const adapter of [
      {},
      { readDescription: undefined },
      { readDescription: null },
      { readDescription: 'nope' },
    ]) {
      const { code, out, err } = await readDescription(['--id', 'u-1', '--out-file', outFile], {
        adapter,
      })
      assert.equal(code, 2, JSON.stringify(adapter))
      assert.equal(out, '')
      assert.match(err, /^read-description: .*no readDescription capability/)
      assert.match(err, /getIssue/, 'the diagnostic names the fallback')
      assert.equal(err.trim().split('\n').length, 1, 'a one-line diagnostic')
    }
    assert.deepEqual(entries(dir), [], 'nothing is written without the capability')
  })
})

test('read-description turns a missing tracker credential into exit 2, never 0, naming the fallback', async () => {
  await withScratch(async (dir) => {
    const outFile = path.join(dir, 'x.md')
    // Keyed on the tracker-neutral code, not the message: any tracker's credential text qualifies.
    const { code, out, err } = await readDescription(['--id', 'u-1', '--out-file', outFile], {
      adapter: {
        readDescription: async () => {
          throw Object.assign(new Error('ACME_TOKEN is not set'), {
            code: TRACKER_CREDENTIALS_MISSING,
          })
        },
      },
    })
    assert.equal(code, 2)
    assert.equal(out, '')
    assert.match(err, /^read-description: tracker credential missing \(ACME_TOKEN is not set\)/)
    assert.match(err, /getIssue/)
    assert.equal(err.trim().split('\n').length, 1)
    assert.doesNotMatch(err, /\n\s+at /, 'no stack frames reach stderr')
    assert.deepEqual(entries(dir), [])
  })
})

test('read-description fails closed — exit 2, empty stdout, one line — on any other read failure', async () => {
  const cases = [
    [
      {
        resolveAdapter: () => {
          throw new Error('tracker adapter: trackerConfig.linear.mcpServer is required')
        },
      },
      /could not resolve the tracker adapter/,
    ],
    [
      {
        adapter: {
          readDescription: async () => {
            throw new Error('Linear GraphQL error: Entity not found\nsecond line')
          },
        },
      },
      /Entity not found second line/,
    ],
    [
      { adapter: { readDescription: () => Promise.reject(new Error('Linear API HTTP 500')) } },
      /HTTP 500/,
    ],
    [{ adapter: { readDescription: async () => null } }, /unreadable/],
    [
      {
        adapter: {
          readDescription: async () => ({ id: 'u-1', identifier: 'B-1', description: 7 }),
        },
      },
      /unreadable/,
    ],
    [
      { adapter: { readDescription: async () => ({ identifier: 'B-1', description: 'x' }) } },
      /unreadable/,
    ],
    [
      {
        adapter: {
          readDescription: async () => ({ id: 'u-1', identifier: 'B-1', description: 'a\uD800b' }),
        },
      },
      /well-formed/,
    ],
  ]
  for (const [options, pattern] of cases) {
    await withScratch(async (dir) => {
      const outFile = path.join(dir, 'x.md')
      const { code, out, err } = await readDescription(
        ['--id', 'u-1', '--out-file', outFile],
        options,
      )
      assert.equal(code, 2, String(pattern))
      assert.equal(out, '')
      assert.match(err, pattern)
      assert.equal(err.trim().split('\n').length, 1, 'the diagnostic stays on one line')
      assert.deepEqual(entries(dir), [], `${pattern}: nothing written, no temp sibling`)
    })
  }
})

test('read-description leaves a pre-existing out-file byte-unchanged when the read fails', async () => {
  await withScratch(async (dir) => {
    const outFile = path.join(dir, 'BOS-1.image-guard-stored.md')
    const prior = Buffer.from('prior snapshot — keep me\n')
    fs.writeFileSync(outFile, prior)
    const { code, out } = await readDescription(['--id', 'u-1', '--out-file', outFile], {
      adapter: {
        readDescription: async () => {
          throw new Error('Linear API HTTP 503')
        },
      },
    })
    assert.equal(code, 2)
    assert.equal(out, '')
    assert.deepEqual(fs.readFileSync(outFile), prior)
    assert.deepEqual(entries(dir), ['BOS-1.image-guard-stored.md'])
  })
})

test('read-description exits 2 and creates nothing when the out-file parent does not exist', async () => {
  await withScratch(async (dir) => {
    const outFile = path.join(dir, 'missing', 'x.md')
    const { code, out, err, calls } = await readDescription(['--id', 'u-1', '--out-file', outFile])
    assert.equal(code, 2)
    assert.equal(out, '')
    assert.match(err, /parent directory/)
    assert.equal(calls.length, 0, 'a doomed write never spends a tracker read')
    assert.deepEqual(entries(dir), [])
  })
})

test('read-description exits 2 and leaves no temp sibling when the rename cannot land', async () => {
  await withScratch(async (dir) => {
    // A non-empty directory at --out-file: the temp write succeeds and the rename fails.
    const outFile = path.join(dir, 'occupied')
    fs.mkdirSync(outFile)
    fs.writeFileSync(path.join(outFile, 'keep'), 'x')
    const { code, out, err } = await readDescription(['--id', 'u-1', '--out-file', outFile])
    assert.equal(code, 2)
    assert.equal(out, '')
    assert.match(err, /could not write --out-file/)
    assert.deepEqual(entries(dir), ['occupied'], 'the temp sibling was removed')
    assert.deepEqual(entries(outFile), ['keep'])
  })
})

test('read-description usage errors exit 64 with empty stdout and never reach the adapter', async () => {
  for (const args of [
    [],
    ['--out-file', 'x.md'],
    ['--id', 'u-1'],
    ['--id', 'u-1', '--out-file'],
    ['--id', 'u-1', '--out-file', ''],
    ['--id', '   ', '--out-file', 'x.md'],
    ['--id', '--out-file', 'x.md'],
    ['--id', 'u-1', '--out-file', 'x.md', '--body-file', 'y.md'],
    ['--id', 'u-1', '--id', 'u-2', '--out-file', 'x.md'],
    ['u-1', 'x.md'],
  ]) {
    let resolved = 0
    const { code, out, err, calls } = await readDescription(args, {
      resolveAdapter: () => {
        resolved += 1
        return { readDescription: async () => ({}) }
      },
    })
    assert.equal(code, 64, JSON.stringify(args))
    assert.equal(out, '', JSON.stringify(args))
    assert.match(err, /^read-description: usage: /, JSON.stringify(args))
    assert.equal(err.trim().split('\n').length, 1, JSON.stringify(args))
    assert.equal(resolved + calls.length, 0, `${JSON.stringify(args)} must never reach the adapter`)
  }
})

test('read-description through a real child process exits 2 when LINEAR_API_KEY is unset', async () => {
  await withScratch(async (dir) => {
    const cli = fileURLToPath(new URL('./cli.mjs', import.meta.url))
    // A self-contained config, so the child resolves the real Linear adapter without depending on
    // whichever repo config (if any) sits above the test's working directory.
    fs.writeFileSync(
      path.join(dir, '.boss-skills.json'),
      JSON.stringify({
        adapters: { tracker: 'linear' },
        trackerConfig: { linear: { mcpServer: 'acme-tracker', team: 'Acme' } },
      }),
    )
    const outDir = path.join(dir, 'out')
    fs.mkdirSync(outDir)
    const env = { ...process.env }
    delete env.LINEAR_API_KEY
    delete env.TRACKER
    const child = spawnSync(
      process.execPath,
      [cli, 'read-description', '--id', 'BOS-1', '--out-file', path.join(outDir, 'x.md')],
      { cwd: dir, env, encoding: 'utf8' },
    )
    assert.equal(child.status, 2, `process exit code, stderr: ${child.stderr}`)
    assert.equal(child.stdout, '')
    assert.match(child.stderr, /LINEAR_API_KEY is not set/)
    assert.match(child.stderr, /getIssue/)
    assert.equal(child.stderr.trim().split('\n').length, 1)
    assert.deepEqual(entries(outDir), [])
  })
})

test('every verb other than list-planned and read-description still returns synchronously', () => {
  for (const argv of [['claim-token'], ['--help'], ['bogus']]) {
    const code = runCli(argv, { write: () => {}, errWrite: () => {} })
    assert.equal(typeof code, 'number', JSON.stringify(argv))
  }
})

// --- help surface -------------------------------------------------------------

// The capability names are derived from this module's OWN dispatch literals, so a new
// `cmd === '...'` branch cannot be added without appearing in help.
function dispatchedTrackerCommands() {
  const source = fs.readFileSync(new URL('./cli.mjs', import.meta.url), 'utf8')
  return [...source.matchAll(/cmd === '([^']+)'/g)]
    .map((m) => m[1])
    .filter((c) => !c.startsWith('-') && c !== 'help')
}

for (const flag of ['--help', '-h', 'help']) {
  test(`${flag} exits 0 and names every dispatched tracker capability`, () => {
    let out = ''
    let err = ''
    const code = runCli([flag], { write: (s) => (out += s), errWrite: (s) => (err += s) })
    assert.equal(code, 0)
    assert.equal(err, '')
    const dispatched = dispatchedTrackerCommands()
    assert.ok(dispatched.includes('claim-token'), 'the dispatch scan found no capabilities')
    for (const cmd of dispatched) {
      assert.ok(out.includes(cmd), `help output is missing capability ${cmd}`)
    }
  })
}

test('an unknown tracker capability rejection carries the capability list', () => {
  let err = ''
  const code = runCli(['bogus'], { errWrite: (s) => (err += s), write: () => {} })
  assert.equal(code, 2)
  assert.match(err, /unknown tracker capability: bogus/)
  for (const cmd of dispatchedTrackerCommands()) {
    assert.ok(err.includes(cmd), `rejection is missing capability ${cmd}`)
  }
  assert.ok(err.includes(TRACKER_USAGE))
})

// --- classify-outcome ---------------------------------------------------------
// BOS-1282: the agent-driven MCP sites execute the tool themselves, so no code wrapper can
// intercept their outcome. This verb is how they reach the SAME classifier the executable paths
// run on — the mechanism that stops a second outcome vocabulary growing in skill prose.

function classify(args) {
  let out = ''
  let err = ''
  const code = runCli(['classify-outcome', ...args], {
    write: (s) => (out += s),
    errWrite: (s) => (err += s),
  })
  return { code, out, err, machine: out.split('\n')[0] }
}

test('classify-outcome prints a machine-readable verdict line for each of the five verdicts', () => {
  const cases = [
    [['--observed', 'fetch failed'], 'retryable', 'transport-failed', 'yes'],
    [['--observed', 'Linear API HTTP 401'], 'permanent', 'unauthorized', 'no'],
    [
      ['--observed', 'The operation timed out', '--operation', 'write'],
      'indeterminate',
      'write-may-have-applied',
      'no',
    ],
    [['--result', '{"issues":{"nodes":[]}}'], 'ok', 'success', 'no'],
    [['--result', 'null'], 'false-empty', 'unreadable-payload', 'no'],
  ]
  const seen = new Set()
  for (const [args, verdict, reason, retry] of cases) {
    const { code, machine, out, err } = classify(args)
    assert.equal(code, 0, `${args.join(' ')} must exit 0`)
    assert.equal(err, '')
    const operation = args.includes('write') ? 'write' : 'read'
    assert.equal(
      machine,
      `tracker-outcome verdict=${verdict} reason=${reason} retry=${retry} operation=${operation}`,
    )
    // One machine line, then one human line naming the action — never more.
    assert.equal(out.trimEnd().split('\n').length, 2, `${verdict} must print exactly two lines`)
    seen.add(verdict)
  }
  assert.equal(seen.size, 5, 'all five verdicts must be reachable through the verb')
})

test('classify-outcome names the action each verdict requires, not just the verdict', () => {
  const indeterminate = classify(['--observed', 'The operation timed out', '--operation', 'write'])
  // The forbidden pair is what makes the verdict actionable at an agent-driven site.
  assert.match(indeterminate.out, /READ THE TARGET BACK before any second attempt/)
  assert.match(indeterminate.out, /blind retry and a silent abandon are both forbidden/)

  const falseEmpty = classify(['--result', 'null'])
  assert.match(falseEmpty.out, /never "no work"/)
})

test('classify-outcome reads an explicit --status when the text carries none', () => {
  assert.equal(
    classify(['--observed', 'upstream said no', '--status', '429']).machine,
    'tracker-outcome verdict=retryable reason=rate-limited retry=yes operation=read',
  )
})

test('classify-outcome exits non-zero with a diagnostic and EMPTY stdout when the observation is missing', () => {
  const { code, out, err } = classify([])
  assert.equal(code, 2)
  assert.equal(out, '', 'a refusal must write nothing to stdout')
  assert.match(err, /one of --observed <text> or --result <json> is required/)
})

test('classify-outcome refuses both sources at once, and a bad operation or status', () => {
  const both = classify(['--observed', 'x', '--result', '{}'])
  assert.equal(both.code, 2)
  assert.equal(both.out, '')
  assert.match(both.err, /mutually exclusive/)

  const badOp = classify(['--observed', 'x', '--operation', 'delete'])
  assert.equal(badOp.code, 2)
  assert.equal(badOp.out, '')
  assert.match(badOp.err, /--operation must be one of read, write/)

  const badStatus = classify(['--observed', 'x', '--status', 'nope'])
  assert.equal(badStatus.code, 2)
  assert.equal(badStatus.out, '')
  assert.match(badStatus.err, /--status must be an integer HTTP status/)

  const badJson = classify(['--result', '{oops'])
  assert.equal(badJson.code, 2)
  assert.equal(badJson.out, '')
  assert.match(badJson.err, /malformed JSON/)
})

test('classify-outcome is a CLI capability and NOT a tracker operation', () => {
  // The verb is DISPATCHED here...
  assert.ok(dispatchedTrackerCommands().includes('classify-outcome'))
  // ...and absent from the declarative operation map, which is the invariant that keeps every
  // vendored adapter conforming: classification is not something a tracker performs.
  const operationMap = buildLinearOperationMap('bossanova-linear')
  assert.equal('classifyOutcome' in operationMap, false)
  for (const key of Object.keys(operationMap)) assert.doesNotMatch(key, /classif/i)
})
