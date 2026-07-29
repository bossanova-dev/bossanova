// scripts/tracker/cli.test.mjs
import { test } from 'node:test'
import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { runCli, generateClaimToken } from './cli.mjs'
import { createLinearAdapter } from './linear.mjs'

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
