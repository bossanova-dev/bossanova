import test from 'node:test'
import assert from 'node:assert/strict'
import { mkdtempSync, writeFileSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import path from 'node:path'
import { spawnSync } from 'node:child_process'
import { main } from './issue-lease.mjs'

const token = 'a'.repeat(32)
const argv = (action = 'acquire') => [
  action,
  '--issue',
  'ENG-1',
  '--scope',
  'plan',
  '--token',
  token,
]
function harness(response, tracker = 'github') {
  const calls = []
  return {
    calls,
    deps: {
      loadConfig: () => ({ adapters: { tracker } }),
      resolveBinary: () => ({ ok: true, path: '/fake/boss', reason: '' }),
      run: (...args) => {
        calls.push(args)
        return response
      },
    },
  }
}

test('acquire forwards configured tracker, holder, purpose and bounded TTL to boss', () => {
  for (const tracker of ['github', 'linear']) {
    for (const scope of ['organization', 'daemon']) {
      const h = harness(
        { status: 0, stdout: JSON.stringify({ is_acquired: true, scope, expires_at: 'later' }) },
        tracker,
      )
      assert.deepEqual(main(argv(), h.deps), {
        status: 0,
        result: { verdict: 'acquired', scope, expiresAt: 'later' },
      })
      assert.deepEqual(h.calls[0][1], [
        'lease',
        'acquire',
        '--issue',
        `${tracker}:ENG-1`,
        '--purpose',
        'plan',
        '--holder',
        token,
        '--ttl',
        '7200s',
        '--json',
      ])
      assert.equal(h.calls[0][2].timeout, 15000)
      main([...argv(), '--ttl-seconds', '30'], h.deps)
      assert.ok(h.calls[1][1].includes('30s'))
    }
  }
})
test('peer contention preserves expiry', () => {
  assert.equal(main(argv(), harness({ status: 3 }).deps).result.verdict, 'held')
  const h = harness({ status: 3, stdout: '{"is_acquired":false,"expires_at":"later"}' })
  assert.deepEqual(main(argv(), h.deps), {
    status: 3,
    result: { verdict: 'held', expiresAt: 'later' },
  })
})
test('errors, old CLI, daemon outage, timeout and malformed responses degrade', () => {
  for (const response of [
    { status: 2, stderr: 'daemon unavailable' },
    { status: 1, stderr: 'unknown command lease' },
    { status: null, error: Object.assign(new Error('timed out'), { code: 'ETIMEDOUT' }) },
    { status: 0, stdout: 'garbage' },
    { status: 0, stdout: '{}' },
  ]) {
    const out = main(argv(), harness(response).deps)
    assert.equal(out.status, 4)
    assert.equal(out.result.verdict, 'unavailable')
    assert.ok(out.result.reason)
  }
  const h = harness({})
  h.deps.resolveBinary = () => ({ ok: false, path: null, reason: 'missing boss' })
  assert.equal(main(argv(), h.deps).status, 4)
  assert.equal(h.calls.length, 0)
})
test('release is best effort for every outcome', () => {
  for (const response of [
    { status: 0, stdout: '{"is_released":true}' },
    { status: 0, stdout: '{"is_released":false}' },
    { status: 2 },
    { error: new Error('timeout') },
  ]) {
    const h = harness(response)
    const out = main(argv('release'), h.deps)
    assert.equal(out.status, 0)
    assert.ok(['released', 'not-held', 'unavailable'].includes(out.result.verdict))
    assert.ok(!h.calls[0][1].includes('--ttl'))
  }
})
test('usage failures print no stdout through the real entry point', () => {
  for (const args of [
    [],
    ['acquire'],
    [...argv(), '--scope', 'wrong'],
    [...argv(), '--token', 'xyz'],
    [...argv(), '--bogus'],
    [...argv(), '--ttl-seconds', '0'],
    [...argv('release'), '--ttl-seconds', '30'],
  ]) {
    const result = spawnSync(process.execPath, ['skills-toolbox/issue-lease.mjs', ...args], {
      encoding: 'utf8',
    })
    assert.equal(result.status, 2)
    assert.equal(result.stdout, '')
  }
})
test('real resolver without boss degrades with a single JSON output line', () => {
  const cwd = mkdtempSync(path.join(tmpdir(), 'issue-lease-'))
  try {
    writeFileSync(
      path.join(cwd, '.boss-skills.json'),
      JSON.stringify({ adapters: { tracker: 'github' } }),
    )
    const result = spawnSync(
      process.execPath,
      [path.resolve('skills-toolbox/issue-lease.mjs'), ...argv()],
      { cwd, env: { ...process.env, PATH: '', BOSS_BIN: '/absent/boss' }, encoding: 'utf8' },
    )
    assert.equal(result.status, 4)
    assert.equal(JSON.parse(result.stdout).verdict, 'unavailable')
    assert.equal(result.stdout.trim().split('\n').length, 1)
  } finally {
    rmSync(cwd, { recursive: true, force: true, maxRetries: 3 })
  }
})
