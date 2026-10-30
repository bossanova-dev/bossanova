import assert from 'node:assert/strict'
import { test } from 'node:test'
import { runGh, GhError } from './github-gh.mjs'
import { classifyTrackerOutcome } from './outcome.mjs'

test('gh inherits identity and parses complete JSON without a shell', () => {
  const env = { PATH: '/bin', CUSTOM: 'preserved' }
  assert.deepEqual(
    runGh(['api', 'user', '--hostname', 'git.example'], {
      env,
      spawn: (cmd, args, options) => {
        assert.equal(cmd, 'gh')
        assert.equal(options.env, env)
        assert.equal(options.shell, false)
        assert.ok(args.includes('git.example'))
        return { status: 0, stdout: '{"login":"me"}' }
      },
    }),
    { login: 'me' },
  )
  assert.throws(
    () =>
      runGh(['api', 'x', '--paginate', '--slurp'], {
        spawn: () => ({ status: 0, stdout: '[]\n[' }),
      }),
    /JSON/,
  )
})

test('gh failures carry shared classifier semantics', () => {
  const cases = [
    [{ error: { code: 'ENOENT' } }, 'permanent', 'read'],
    [{ status: 1, stderr: 'please run gh auth login' }, 'permanent', 'read'],
    [{ status: 1, stderr: 'HTTP 401: Bad credentials' }, 'permanent', 'read'],
    [{ status: 1, stderr: 'HTTP 403: API rate limit exceeded' }, 'retryable', 'read'],
    [{ status: 1, stderr: 'HTTP 403: secondary rate limit' }, 'retryable', 'read'],
    [{ error: { code: 'ETIMEDOUT' } }, 'indeterminate', 'write'],
    [{ status: null, signal: 'SIGTERM' }, 'indeterminate', 'write'],
    [{ status: 1, stderr: 'HTTP 422: invalid' }, 'permanent', 'read'],
  ]
  for (const [result, verdict, operation] of cases) {
    let error
    try {
      runGh(['api', 'x'], { spawn: () => result })
    } catch (e) {
      error = e
    }
    assert.ok(error instanceof GhError)
    assert.equal(classifyTrackerOutcome(error, { operation }).verdict, verdict)
  }
})

test('transport failures carry truthful read timeout semantics', () => {
  for (const result of [{ error: { code: 'ETIMEDOUT' } }, { status: null, signal: 'SIGTERM' }])
    assert.throws(
      () => runGh(['api', 'x'], { spawn: () => result }),
      (error) => {
        assert.equal(error.code, 'transport')
        assert.doesNotMatch(error.message, /HTTP 504/)
        assert.deepEqual(classifyTrackerOutcome(error, { operation: 'read' }), {
          verdict: 'retryable',
          reason: 'request-timeout',
          retry: true,
        })
        return true
      },
    )
})
