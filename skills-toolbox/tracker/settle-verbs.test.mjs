import { test } from 'node:test'
import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { runCli } from './cli.mjs'
import { createLinearAdapter } from './linear.mjs'
import { formatClaimComment } from '../linear-claim.mjs'
const token = 'a'.repeat(32)
const own = { id: 'own', body: formatClaimComment(token), createdAt: '2026-01-01' }
async function fixture(fn) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'settle-verbs-'))
  try {
    await fn(path.join(dir, 'comments.json'), dir)
  } finally {
    fs.rmSync(dir, { recursive: true, force: true, maxRetries: 3 })
  }
}
async function run(
  verb,
  flags,
  reads,
  capability = verb === 'await-state' ? 'readIssueState' : 'readClaimComments',
) {
  let clock = 0,
    calls = 0,
    sleeps = 0,
    out = '',
    err = ''
  const code = await runCli([verb, ...flags], {
    resolveAdapter: () => ({
      [capability]: async () => {
        const v = reads[Math.min(calls++, reads.length - 1)]
        if (v instanceof Error) throw v
        return v
      },
    }),
    write: (s) => {
      out += s
    },
    errWrite: (s) => {
      err += s
    },
    now: () => clock,
    sleep: async (ms) => {
      clock += ms
      sleeps++
    },
  })
  return { code, out, err, calls, sleeps, clock, receipt: out ? JSON.parse(out) : null }
}
const comments = (rows) => ({ id: 'uuid', identifier: 'APP-1', comments: rows })
const state = (name) => ({ id: 'uuid', identifier: 'APP-1', state: { name, type: 'started' } })
test('own claim settles immediately or on third read and writes matching read', () =>
  fixture(async (file) => {
    for (const lag of [0, 2]) {
      const r = await run(
        'read-claim-comments',
        ['--id', 'uuid', '--out-file', file, '--until-claim', token],
        [...Array(lag).fill(comments([])), comments([own])],
      )
      assert.equal(r.code, 0)
      assert.equal(r.receipt.attempts, lag + 1)
      assert.equal(r.sleeps, lag)
      assert.equal(r.receipt.ownClaimId, 'own')
      assert.equal(r.receipt.settled, true)
      assert.deepEqual(JSON.parse(fs.readFileSync(file)), [own])
    }
  }))
test('absence combines with token and polls until both conditions hold', () =>
  fixture(async (file) => {
    const stale = { ...own, id: 'deleted' }
    const r = await run(
      'read-claim-comments',
      ['--id', 'uuid', '--out-file', file, '--until-claim', token, '--until-absent', 'deleted'],
      [comments([stale]), comments([]), comments([own])],
    )
    assert.equal(r.receipt.attempts, 3)
    assert.equal(r.receipt.settled, true)
    const absent = await run(
      'read-claim-comments',
      ['--id', 'uuid', '--out-file', file, '--until-absent', 'deleted'],
      [comments([stale]), comments([])],
    )
    assert.equal(absent.receipt.attempts, 2)
  }))
test('unsettled reads write final result; no condition reads exactly once', () =>
  fixture(async (file) => {
    const rows = [{ ...own, body: `merely ${token}` }]
    const r = await run(
      'read-claim-comments',
      ['--id', 'uuid', '--out-file', file, '--until-claim', token, '--settle-ms', '6000'],
      [comments(rows)],
    )
    assert.equal(r.code, 0)
    assert.equal(r.receipt.settled, false)
    assert.equal(r.clock, 6000)
    assert.deepEqual(JSON.parse(fs.readFileSync(file)), rows)
    const once = await run(
      'read-claim-comments',
      ['--id', 'uuid', '--out-file', file],
      [comments([])],
    )
    assert.equal(once.calls, 1)
  }))
test('invalid flags fail usage before calling adapter', () =>
  fixture(async (file) => {
    const base = ['--id', 'uuid', '--out-file', file]
    for (const flags of [
      [],
      ['--id', 'uuid'],
      [...base, '--settle-ms', '3'],
      [...base, '--scope', 'bad'],
      [...base, '--id', 'again'],
      [...base, '--unknown', 'x'],
      [...base, '--until-claim', token, '--settle-ms', '0'],
      [...base, '--until-claim', token, '--settle-ms', '1.5'],
      [...base, '--until-claim', token, '--settle-ms', '300001'],
      ['--id', 'uuid', '--out-file', path.join(file, 'absent')],
    ]) {
      const r = await run('read-claim-comments', flags, [])
      assert.equal(r.code, 64)
      assert.equal(r.calls, 0)
      assert.equal(r.out, '')
    }
  }))
test('missing capability and read failure leave no output or temp sibling', () =>
  fixture(async (file, dir) => {
    const flags = ['--id', 'uuid', '--out-file', file, '--until-claim', token]
    const missing = await run('read-claim-comments', flags, [], 'other')
    assert.equal(missing.code, 2)
    assert.equal(missing.out, '')
    assert.equal(fs.existsSync(file), false)
    fs.writeFileSync(file, 'original')
    const failure = await run('read-claim-comments', flags, [comments([]), new Error('failed')])
    assert.equal(failure.code, 2)
    assert.equal(failure.out, '')
    assert.equal(fs.readFileSync(file, 'utf8'), 'original')
    assert.deepEqual(fs.readdirSync(dir), ['comments.json'])
    assert.match(failure.err, /readComments/)
  }))
test('await-state reports first, delayed and never reached target', async () => {
  for (const lag of [0, 2]) {
    const r = await run(
      'await-state',
      ['--id', 'uuid', '--state', 'Review'],
      [...Array(lag).fill(state('Doing')), state('Review')],
    )
    assert.equal(r.code, 0)
    assert.equal(r.receipt.attempts, lag + 1)
    assert.equal(r.receipt.settled, true)
  }
  const r = await run(
    'await-state',
    ['--id', 'uuid', '--state', 'Review', '--settle-ms', '10000'],
    [state('Doing')],
  )
  assert.equal(r.code, 0)
  assert.equal(r.receipt.settled, false)
  assert.equal(r.receipt.state, 'Doing')
  assert.equal(
    (await run('await-state', ['--id', 'uuid', '--state', 'Review'], [], 'other')).code,
    2,
  )
  for (const args of [
    ['--id', 'uuid'],
    ['--id', 'uuid', '--state', 'Review', '--settle-ms', '300001'],
  ])
    assert.equal((await run('await-state', args, [])).code, 64)
})

test('written comment file arbitrates identically to inline comments', () =>
  fixture(async (file) => {
    const result = await run(
      'read-claim-comments',
      ['--id', 'uuid', '--out-file', file, '--until-claim', token],
      [comments([own])],
    )
    assert.equal(result.code, 0)
    const verdict = async (input) => {
      let output = ''
      const code = await runCli(['claim-verdict', '--me', token, '--comments', input], {
        write: (s) => {
          output += s
        },
        errWrite: () => {},
        resolveAdapter: () =>
          createLinearAdapter({ apiKey: 'key', trackerConfig: { mcpServer: 'linear' } }),
      })
      return { code, output }
    }
    assert.deepEqual(await verdict(file), await verdict(JSON.stringify([own])))
  }))
