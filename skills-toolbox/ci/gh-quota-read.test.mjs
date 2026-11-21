import assert from 'node:assert/strict'
import test from 'node:test'
import { isRateLimitRefusal, quotaOf, readWithQuotaFallback } from './gh-quota-read.mjs'

test('quota identification and refusal detection distinguish permission failures', () => {
  for (const args of [
    ['api', 'repos/o/r'],
    ['run', 'list'],
  ])
    assert.equal(quotaOf(args), 'core')
  for (const args of [
    ['api', 'graphql'],
    ['pr', 'view'],
  ])
    assert.equal(quotaOf(args), 'graphql')
  for (const message of [
    'API rate limit exceeded',
    'API rate limit already exceeded',
    'You have exceeded a secondary rate limit',
    'ABUSE DETECTION',
  ])
    assert.equal(isRateLimitRefusal(message), true)
  for (const message of ['HTTP 404', 'authentication failed', 'HTTP 403', 'HTTP 429'])
    assert.equal(isRateLimitRefusal(message), false)
})

function read(answers) {
  const calls = []
  const result = readWithQuotaFallback({
    primary: { args: ['pr', 'list'], normalize: (value) => value },
    alternate: { args: ['api', 'repos/o/r/pulls'], normalize: (value) => value },
    label: 'test:quota',
    timeoutMs: 1234,
    deps: {
      env: { HOME: '/home/test' },
      spawn: (cmd, args, opts) => {
        calls.push({ cmd, args, opts })
        return answers.shift()
      },
    },
  })
  for (const call of calls) {
    assert.equal(call.opts.env.BOSS_GH_USAGE_LABEL, 'test:quota')
    assert.equal(call.opts.env.HOME, '/home/test')
    assert.equal(call.opts.timeout, 1234)
  }
  return { result, calls }
}
const ok = { status: 0, stdout: '[7]' }
const refused = { status: 1, stderr: 'API rate limit already exceeded' }

test('healthy primary reads once; rate refusal alone tries the alternate', () => {
  const healthy = read([ok])
  assert.equal(healthy.calls.length, 1)
  assert.deepEqual(healthy.result, { ok: true, value: [7], transport: 'graphql', degraded: null })
  const fallback = read([refused, ok])
  assert.equal(fallback.calls.length, 2)
  assert.deepEqual(fallback.result, {
    ok: true,
    value: [7],
    transport: 'core',
    degraded: { transport: 'core', reason: 'graphql-rate-limited' },
  })
})

test('ordinary failure, timeout and malformed JSON never spend the alternate quota', () => {
  for (const [answer, message] of [
    [{ status: 1, stderr: 'HTTP 403' }, 'HTTP 403'],
    [{ status: 1, stderr: '' }, 'gh exited 1'],
    [{ status: 2, stderr: ' \n\t' }, 'gh exited 2'],
    [{ error: { code: 'ETIMEDOUT' }, stderr: '' }, 'ETIMEDOUT'],
    [{ error: { message: 'spawn failed' }, stderr: '' }, 'spawn failed'],
    [{ status: 0, stdout: 'invalid' }, 'unparseable gh output'],
  ]) {
    const { result, calls } = read([answer])
    assert.equal(calls.length, 1)
    assert.equal(result.error.code, 'UNREADABLE')
    assert.equal(result.error.message, message)
  }
})

test('both refused reports both quotas; error excerpts are bounded and redact tokens', () => {
  const { result, calls } = read([refused, refused])
  assert.equal(calls.length, 2)
  assert.equal(result.error.code, 'RATE_LIMITED')
  assert.deepEqual(result.error.quotas, ['graphql', 'core'])
  const failed = read([{ status: 1, stderr: 'ghp_secret0123456789 ' + 'x'.repeat(1000) }]).result
  assert.ok(failed.error.message.length < 400)
  assert.equal(failed.error.message.includes('ghp_secret'), false)
})
