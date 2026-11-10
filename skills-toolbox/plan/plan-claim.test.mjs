import { test } from 'node:test'
import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { readFileSync, writeFileSync, rmSync } from 'node:fs'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'
import { tempDir } from '../../scripts/test-infra/test-tmpdir.mjs'
import { createLinearAdapter } from '../tracker/linear.mjs'

const helper = fileURLToPath(new URL('./plan-claim.mjs', import.meta.url))
function fixture(t) {
  const dir = tempDir('plan-claim-')
  t.after(() => rmSync(dir, { recursive: true, force: true, maxRetries: 3 }))
  const receipt = '.linear-plans/run-test/ABC-1.claim-receipt.json'
  const run = (...args) =>
    spawnSync(process.execPath, [helper, ...args], { cwd: dir, encoding: 'utf8' })
  const read = () => JSON.parse(readFileSync(join(dir, receipt), 'utf8'))
  return { dir, receipt, run, read }
}
test('receipt persists ownership across separate processes and arbitrates all outcomes', (t) => {
  const { dir, receipt, run, read } = fixture(t)
  assert.equal(run('start', '--receipt', receipt, '--issue', 'ABC-1').status, 0)
  const token = read().token
  assert.match(token, /^[a-f0-9]{32}$/)
  const comment = run('comment', '--receipt', receipt, '--session-id', 'ours')
  assert.equal(comment.status, 0, comment.stderr)
  assert.equal(
    comment.stdout.trim(),
    createLinearAdapter({}).formatClaimComment(token, 'ours', { scope: 'plan' }),
  )
  assert.equal(run('record-comment', '--receipt', receipt, '--id', 'comment-1').status, 0)
  assert.equal(read().commentId, 'comment-1')
  assert.equal(run('record-comment', '--receipt', receipt, '--id', 'comment-1').status, 0)
  assert.equal(run('record-comment', '--receipt', receipt, '--id', 'comment-2').status, 2)
  for (const [claimToken, status] of [
    [token, 0],
    ['b'.repeat(32), 3],
    [null, 4],
  ]) {
    const comments = claimToken
      ? [
          {
            body: createLinearAdapter({}).formatClaimComment(claimToken, null, { scope: 'plan' }),
            createdAt: new Date().toISOString(),
          },
        ]
      : []
    writeFileSync(join(dir, 'comments.json'), JSON.stringify({ comments }))
    const result = run('verdict', '--receipt', receipt, '--comments', 'comments.json')
    assert.equal(result.status, status, result.stderr)
    if (status === 4) assert.equal(result.stdout.trim(), 'NO_WINNER')
  }
})
test('start validates declared paths and issue identity and requires release before reminting', (t) => {
  const { receipt, run, read } = fixture(t)
  assert.equal(run('start', '--receipt', 'invented.json', '--issue', 'ABC-1').status, 2)
  assert.equal(run('start', '--receipt', receipt, '--issue', 'ABC-2').status, 2)
  assert.equal(run('start', '--receipt', receipt, '--issue', 'ABC-1').status, 0)
  const token = read().token
  assert.equal(run('start', '--receipt', receipt, '--issue', 'ABC-1').status, 2)
  assert.equal(run('release', '--receipt', receipt).status, 0)
  assert.equal(read().released, true)
  assert.equal(run('start', '--receipt', receipt, '--issue', 'ABC-1').status, 0)
  assert.notEqual(read().token, token)
})
test('lease operations consume receipt ownership and release prints the recorded comment id', async (t) => {
  const { dir, receipt, run, read } = fixture(t)
  assert.equal(run('start', '--receipt', receipt, '--issue', 'ABC-1').status, 0)
  const { main } = await import('./plan-claim.mjs')
  const absolute = join(dir, receipt)
  let output = '',
    sent
  const deps = {
    loadConfig: () => ({ adapters: { tracker: 'linear' } }),
    resolveBinary: () => ({ ok: true, path: '/boss' }),
    run: (_binary, args) => {
      sent = args
      return {
        status: 0,
        stdout: JSON.stringify({ is_acquired: true, scope: 'daemon', expires_at: 'later' }),
      }
    },
    write: (s) => {
      output += s
    },
    errWrite: () => {},
  }
  assert.equal(main(['acquire', '--receipt', absolute], deps), 0)
  assert.ok(sent.includes('linear:ABC-1'))
  assert.ok(sent.includes(read().token))
  assert.ok(sent.includes('plan'))
  for (const [response, status] of [
    [{ status: 3, stdout: '{"expires_at":"later"}' }, 3],
    [{ status: 1, stderr: 'offline' }, 4],
  ]) {
    output = ''
    assert.equal(main(['acquire', '--receipt', absolute], { ...deps, run: () => response }), status)
    assert.equal(JSON.parse(output).verdict, status === 3 ? 'held' : 'unavailable')
  }
  assert.equal(run('record-comment', '--receipt', receipt, '--id', 'ours').status, 0)
  output = ''
  assert.equal(
    main(['release', '--receipt', absolute], {
      ...deps,
      resolveBinary: () => ({ ok: false, reason: 'offline' }),
    }),
    0,
  )
  assert.equal(JSON.parse(output).commentId, 'ours')
  assert.equal(read().released, true)
  output = ''
  assert.equal(
    main(
      ['release', '--receipt', join(dir, '.linear-plans/run-other/ABC-1.claim-receipt.json')],
      deps,
    ),
    0,
  )
  assert.deepEqual(JSON.parse(output), { lease: 'none', commentId: null })
  output = ''
  assert.equal(
    main(['comment', '--receipt', absolute], {
      ...deps,
      resolveAdapter: () => ({ claimScopes: ['build'] }),
    }),
    2,
  )
  assert.equal(output, '')
})

test('plan verdict expires old claims and consumes terminal owner liveness', (t) => {
  const { receipt, run, read } = fixture(t)
  assert.equal(run('start', '--receipt', receipt, '--issue', 'ABC-1').status, 0)
  const token = read().token
  const adapter = createLinearAdapter({})
  const claim = (value, date, owner) => ({
    body: adapter.formatClaimComment(value, owner, { scope: 'plan' }),
    createdAt: date,
  })
  const mine = claim(token, new Date().toISOString(), 'ours')
  const peer = claim('a'.repeat(32), new Date().toISOString(), 'peer')
  const buildClaim = {
    body: adapter.formatClaimComment('a'.repeat(32), 'builder', { scope: 'build' }),
    createdAt: new Date().toISOString(),
  }
  const verdict = (comments, extra = []) =>
    run('verdict', '--receipt', receipt, '--comments', JSON.stringify(comments), ...extra)
  assert.equal(verdict([claim('a'.repeat(32), '2000-01-01T00:00:00Z', 'old'), mine]).status, 0)
  assert.equal(
    verdict(
      [peer, mine],
      [
        '--liveness',
        JSON.stringify({ sessions: { peer: { terminal: true, reason: 'archived' } } }),
      ],
    ).status,
    0,
  )
  assert.equal(
    verdict([peer, mine], ['--liveness', '{"sessions":{"peer":{"terminal":"yes"}}}']).status,
    2,
  )
})
