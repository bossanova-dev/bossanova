import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { test } from 'node:test'
import { runSentryGate } from '../../.claude/skills/bs-sweep-sentry/gate/gate.mjs'
import { fetchSentryIssues } from './sweep-sentry-gate.mjs'

const env = { SENTRY_API_KEY: 'test-token', SENTRY_ORG: 'test-org', LINEAR_API_KEY: 'test-key' }

async function runGate(overrides = {}) {
  const codes = []
  let output = ''
  await runSentryGate({
    env,
    fetchSentryIssues: async () => [],
    fetchMarkedLinearIssues: async () => [],
    exit: (code) => codes.push(code),
    stderr: {
      write: (text) => {
        output += text
      },
    },
    ...overrides,
  })
  return { codes, output }
}

test('actionable Sentry issue runs the sweep', async () => {
  const result = await runGate({
    fetchSentryIssues: async () => [{ shortId: 'TEST-1', level: 'fatal' }],
  })
  assert.deepEqual(result.codes, [0])
})

test('empty Sentry results skip the sweep', async () => {
  assert.deepEqual((await runGate()).codes, [1])
})

test('missing Linear credentials fail evaluation', async () => {
  const result = await runGate({ env: { ...env, LINEAR_API_KEY: '' } })
  assert.deepEqual(result.codes, [69])
  assert.ok(result.output.trim())
})

test('Sentry HTTP failure fails evaluation', async () => {
  const result = await runGate({
    fetchSentryIssues: (options) =>
      fetchSentryIssues({
        ...options,
        fetchImpl: async () => ({ ok: false, status: 503 }),
      }),
  })
  assert.deepEqual(result.codes, [69])
  assert.ok(result.output.trim())
})

test('Linear marker scan failure fails evaluation', async () => {
  const result = await runGate({
    fetchMarkedLinearIssues: async () => {
      throw new Error('incomplete marker scan')
    },
  })
  assert.deepEqual(result.codes, [69])
  assert.ok(result.output.trim())
})

test('importing the gate performs no network or exit', () => {
  const gateUrl = new URL('../../.claude/skills/bs-sweep-sentry/gate/gate.mjs', import.meta.url)
  const result = spawnSync(
    process.execPath,
    [
      '--input-type=module',
      '-e',
      `
    process.exit = () => { throw new Error('unexpected exit') }
    globalThis.fetch = () => { throw new Error('unexpected network request') }
    await import(${JSON.stringify(gateUrl.href)})
  `,
    ],
    { env, encoding: 'utf8' },
  )
  assert.equal(result.status, 0, result.stderr)
  assert.equal(result.stderr, '')
})
