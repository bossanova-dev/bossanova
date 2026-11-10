import assert from 'node:assert/strict'
import test from 'node:test'
import { mkdtempSync, mkdirSync, readFileSync, existsSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { formatClaimComment, resolveClaimVerdict } from '../linear-claim.mjs'
import { main } from './claim-owner-status.mjs'

const me = 'f'.repeat(32)
const now = '2026-11-09T12:00:00Z'
const comment = (token, owner) => ({
  body: formatClaimComment(token.repeat(32), owner, { scope: 'plan' }),
  createdAt: '2026-11-09T11:30:00Z',
})
function fixture(t) {
  const dir = mkdtempSync(join(tmpdir(), 'claim-owner-'))
  t.after(() => rmSync(dir, { recursive: true, force: true, maxRetries: 3 }))
  const out = join(dir, 'liveness.json')
  const receipt = join(dir, '.linear-plans/run-owner/APP-1.claim-receipt.json')
  mkdirSync(join(dir, '.linear-plans/run-owner'), { recursive: true })
  writeFileSync(
    receipt,
    JSON.stringify({
      version: 1,
      scope: 'plan',
      issueId: 'APP-1',
      token: me,
      commentId: null,
      released: false,
    }),
  )
  return { dir, out, args: ['--claim-receipt', receipt, '--out', out] }
}
const binary = () => ({ ok: true, path: '/boss' })

test('terminal evidence deduplicates lookups, names every peer and releases only terminal peer leases', (t) => {
  const { out, args } = fixture(t)
  const calls = [],
    releases = []
  const comments = [
    comment('a', 'archived'),
    comment('b', 'closed'),
    comment('c', 'archived'),
    comment('d', '-flag'),
    comment('e', null),
    comment('f', 'ours'),
  ]
  const result = main(
    [
      ...args,
      '--comments',
      JSON.stringify({ comments }),
      '--issue',
      'APP-1',
      '--release-forfeited-leases',
    ],
    {
      now: () => new Date(now),
      resolveBinary: binary,
      run: (path, argv, options) => {
        calls.push(argv)
        assert.equal(path, '/boss')
        assert.equal(options.shell, false)
        assert.equal(options.timeout, 15000)
        return {
          status: 0,
          stdout: JSON.stringify(
            argv[1] === 'archived' ? { archived_at: now, state: 'MERGED' } : { state: 'CLOSED' },
          ),
        }
      },
      releaseLease: (argv) => {
        releases.push(argv)
        return {
          status: 0,
          result: {
            verdict: argv.token.startsWith('a') ? 'released' : 'unavailable',
            reason: 'offline',
          },
        }
      },
    },
  )
  assert.equal(result.status, 0)
  const evidence = JSON.parse(readFileSync(out, 'utf8'))
  assert.deepEqual(evidence.sessions, {
    archived: { terminal: true, reason: 'archived' },
    closed: { terminal: true, reason: 'closed' },
  })
  assert.deepEqual(calls, [
    ['show', 'archived', '--json'],
    ['show', 'closed', '--json'],
  ])
  assert.equal(evidence.peers.length, 5)
  assert.equal(evidence.peers[0].ageMinutes, 30)
  assert.equal(evidence.peers[0].lease.verdict, 'released')
  assert.equal(evidence.peers[1].lease.verdict, 'unavailable')
  assert.equal(evidence.peers[3].owner, 'unknown')
  assert.equal(evidence.peers[4].owner, 'none')
  assert.deepEqual(
    releases.map((argv) => argv.token),
    ['a', 'b', 'c'].map((v) => v.repeat(32)),
  )
  assert.ok(
    releases.every(
      (argv) => argv.command === 'release' && argv.scope === 'plan' && argv.issue === 'APP-1',
    ),
  )
  assert.equal(resolveClaimVerdict(comments, me, { scope: 'plan', now, ...evidence }), false)
})

test('unreadable or non-terminal owners supply no evidence and their young claims still win', (t) => {
  for (const response of [
    { status: 1, stderr: 'NOT_FOUND' },
    { status: 1 },
    { status: 0, stdout: '{' },
    { status: null, error: new Error('ETIMEDOUT') },
    ...['ACTIVE', 'MERGED', 'ORPHANED'].map((state) => ({
      status: 0,
      stdout: JSON.stringify({ state }),
    })),
  ]) {
    const { out, args } = fixture(t)
    const comments = [comment('a', 'peer'), comment('f', 'ours')]
    const result = main([...args, '--comments', JSON.stringify(comments)], {
      now: () => new Date(now),
      resolveBinary: binary,
      run: () => response,
      releaseLease: () => assert.fail('unknown owners must not release'),
    })
    assert.equal(result.status, 0)
    const evidence = JSON.parse(readFileSync(out, 'utf8'))
    assert.deepEqual(evidence.sessions, {})
    assert.equal(evidence.peers[0].owner, 'unknown')
    assert.equal(resolveClaimVerdict(comments, me, { scope: 'plan', now, ...evidence }), false)
  }
})

test('missing binary and failed releases are best effort; comments files are accepted', (t) => {
  const { dir, out, args } = fixture(t)
  const comments = join(dir, 'comments.json')
  writeFileSync(comments, JSON.stringify([comment('a', 'peer')]))
  assert.equal(
    main([...args, '--comments', comments], {
      resolveBinary: () => ({ ok: false, reason: 'missing' }),
    }).status,
    0,
  )
  assert.deepEqual(JSON.parse(readFileSync(out, 'utf8')).sessions, {})
  assert.equal(
    main([...args, '--comments', comments, '--release-forfeited-leases', '--issue', 'APP-1'], {
      resolveBinary: binary,
      run: () => ({ status: 0, stdout: '{"state":"CLOSED"}' }),
      releaseLease: () => {
        throw new Error('release failed')
      },
    }).status,
    0,
  )
  assert.equal(JSON.parse(readFileSync(out, 'utf8')).peers[0].lease.verdict, 'unavailable')
})

test('usage and unreadable comments exit 2 without creating evidence', (t) => {
  for (const extra of [
    ['--comments', 'missing.json'],
    ['--comments', 'null'],
    ['--comments', '[]', '--release-forfeited-leases'],
    ['--comments', '[]', '--surprise'],
  ]) {
    const { out, args } = fixture(t)
    assert.equal(main([...args, ...extra]).status, 2)
    assert.equal(existsSync(out), false)
  }
})
