import test from 'node:test'
import assert from 'node:assert/strict'
import fs from 'node:fs'
import path from 'node:path'
import { tempDir } from '../../scripts/test-infra/test-tmpdir.mjs'
import {
  MAX_PUBLISH_OUTPUT_BYTES,
  PUBLISH_HOOK_TIMEOUT_MS,
  parsePublishMarker,
  publishContentType,
  publishPrefix,
  refineProofPublish,
  runProofPublish,
} from './verify-publish-hook.mjs'

const SHA = '0123456789abcdef0123456789abcdef01234567'
const SECRET = 'cf-hook-SECRET-value-must-not-surface'
const valid = () => ({
  ok: true,
  extension: 'boss-verify-host',
  role: 'proof-publish',
  baseUrl: 'https://proof.example',
})

function hookDir(t) {
  const dir = tempDir('publish-hook-', t)
  fs.writeFileSync(path.join(dir, 'publish.mjs'), '// stub, the runner is injected\n')
  return { name: 'boss-verify-host', dir, publish: 'publish.mjs' }
}

const envelope = {
  repo: 'acme/widgets',
  pr: 7,
  headSha: SHA,
  prefix: `acme/widgets/7/${SHA}/`,
  evidenceDir: '/work/.verify-evidence/run',
  objects: [{ name: 'manifest.json', path: 'manifest.json', contentType: 'application/json' }],
}

test('the publish marker takes an extension-relative script path only', () => {
  assert.deepEqual(parsePublishMarker({ publish: './bin\\publish.mjs' }), {
    publish: 'bin/publish.mjs',
  })
  for (const publish of [
    undefined,
    '',
    ' ',
    '/abs/publish.mjs',
    'C:/publish.mjs',
    '../x.mjs',
    'a/../../x.mjs',
    3,
  ])
    assert.equal(parsePublishMarker({ publish }), null, String(publish))
})

test('the result validator accepts only a closed https result', () => {
  assert.deepEqual(refineProofPublish(valid()), [])
  assert.deepEqual(refineProofPublish({ ...valid(), baseUrl: 'https://proof.example/base/' }), [])
  for (const patch of [
    { baseUrl: 'http://proof.example' },
    { baseUrl: 'https://user:pw@proof.example' },
    { baseUrl: 'https://proof.example/?q=1' },
    { baseUrl: 'https://proof.example/#x' },
    { baseUrl: 'proof.example' },
    { baseUrl: 42 },
    { pageUrl: 'https://proof.example/page' },
    { ok: false },
    { role: 'proof-describe' },
    { extension: '' },
  ])
    assert.ok(refineProofPublish({ ...valid(), ...patch }).length, JSON.stringify(patch))
  const { baseUrl: _omit, ...missing } = valid()
  assert.ok(refineProofPublish(missing).length)
  assert.ok(refineProofPublish([]).length)
})

test('the deterministic prefix and content types match the daemon contract', () => {
  assert.equal(publishPrefix({ repo: 'acme/widgets', pr: 7, headSha: SHA }), envelope.prefix)
  for (const bad of [
    { repo: 'acme', pr: 7, headSha: SHA },
    { repo: 'a/b/c', pr: 7, headSha: SHA },
    { repo: '../widgets', pr: 7, headSha: SHA },
    { repo: 'acme/widgets', pr: 0, headSha: SHA },
    { repo: 'acme/widgets', pr: 1.5, headSha: SHA },
    { repo: 'acme/widgets', pr: 7, headSha: SHA.toUpperCase() },
    { repo: 'acme/widgets', pr: 7, headSha: 'abc' },
  ])
    assert.equal(publishPrefix(bad), null, JSON.stringify(bad))
  assert.equal(publishContentType('index.html'), 'text/html; charset=utf-8')
  assert.equal(publishContentType('screenshots/a.webp'), 'image/webp')
  assert.equal(publishContentType('credentials.txt'), null)
})

test('the runner passes the envelope and returns the validated base URL', (t) => {
  const extension = hookDir(t)
  let seen
  const outcome = runProofPublish({
    extension,
    envelope,
    cwd: '/checkout',
    env: { HOOK_TOKEN: SECRET },
    run: (cmd, args, opts) => {
      seen = { cmd, args, opts, input: JSON.parse(opts.input) }
      return {
        status: 0,
        stdout: JSON.stringify({ ...valid(), baseUrl: 'https://proof.example/' }),
      }
    },
  })
  assert.deepEqual(outcome, { ok: true, baseUrl: 'https://proof.example' })
  assert.equal(seen.cmd, process.execPath)
  assert.equal(seen.args[0], fs.realpathSync(path.join(extension.dir, 'publish.mjs')))
  assert.equal(seen.opts.cwd, '/checkout')
  assert.equal(seen.opts.env.HOOK_TOKEN, SECRET)
  assert.equal(seen.opts.timeout, PUBLISH_HOOK_TIMEOUT_MS)
  assert.equal(seen.opts.maxBuffer, MAX_PUBLISH_OUTPUT_BYTES)
  assert.equal(seen.input.prefix, `acme/widgets/7/${SHA}/`)
  assert.equal(seen.input.role, 'proof-publish')
  assert.equal(seen.input.extension, 'boss-verify-host')
  assert.deepEqual(seen.input.objects, envelope.objects)
})

test('every runner failure is a reason that never carries hook output', (t) => {
  const extension = hookDir(t)
  const leak = `${SECRET}\n`
  const cases = [
    ['exit:3', () => ({ status: 3, stdout: leak, stderr: leak })],
    ['signal', () => ({ status: null, signal: 'SIGKILL', stdout: leak, stderr: leak })],
    [
      'timeout',
      () => ({ error: Object.assign(new Error(leak), { code: 'ETIMEDOUT' }), stderr: leak }),
    ],
    ['oversize', () => ({ error: Object.assign(new Error(leak), { code: 'ENOBUFS' }) })],
    ['oversize', () => ({ status: 0, stdout: 'x'.repeat(MAX_PUBLISH_OUTPUT_BYTES + 1) })],
    ['not-json', () => ({ status: 0, stdout: leak, stderr: leak })],
    [
      'invalid:',
      () => ({
        status: 0,
        stdout: JSON.stringify({ ...valid(), baseUrl: `http://${SECRET}.example` }),
      }),
    ],
    [
      'invalid:extension mismatch',
      () => ({ status: 0, stdout: JSON.stringify({ ...valid(), extension: 'other' }) }),
    ],
    [
      'exit:1',
      () => {
        throw new Error(leak)
      },
    ],
  ]
  for (const [reason, run] of cases) {
    const outcome = runProofPublish({ extension, envelope, cwd: '/checkout', run })
    assert.equal(outcome.ok, false, reason)
    assert.ok(outcome.reason.startsWith(reason), `${reason}: ${outcome.reason}`)
    assert.ok(!JSON.stringify(outcome).includes(SECRET), `${reason}: hook output leaked`)
  }
  const missing = runProofPublish({
    extension: { ...extension, publish: 'absent.mjs' },
    envelope,
    cwd: '/checkout',
    run: () => assert.fail('a missing script must not run'),
  })
  assert.deepEqual(missing, { ok: false, reason: 'script-missing' })
})

test('a hook that logs more than the stdout bound to stderr still publishes', (t) => {
  const extension = hookDir(t)
  fs.writeFileSync(
    path.join(extension.dir, 'publish.mjs'),
    `process.stderr.write('x'.repeat(${MAX_PUBLISH_OUTPUT_BYTES * 4}))\n` +
      `process.stdout.write(${JSON.stringify(JSON.stringify(valid()))})\n`,
  )
  const outcome = runProofPublish({ extension, envelope, cwd: extension.dir })
  assert.deepEqual(outcome, { ok: true, baseUrl: 'https://proof.example' })
})

test('an unknown result key is refused without echoing its name', (t) => {
  const extension = hookDir(t)
  const run = () => ({ status: 0, stdout: JSON.stringify({ ...valid(), [SECRET]: true }) })
  const outcome = runProofPublish({ extension, envelope, cwd: '/checkout', run })
  assert.equal(outcome.ok, false)
  assert.ok(outcome.reason.startsWith('invalid:'), outcome.reason)
  assert.ok(!JSON.stringify(outcome).includes(SECRET), 'the hook key name leaked')
})

test('the runner enforces the timeout against a real hook process', (t) => {
  const extension = hookDir(t)
  fs.writeFileSync(path.join(extension.dir, 'publish.mjs'), 'setTimeout(() => {}, 60_000)\n')
  const outcome = runProofPublish({ extension, envelope, cwd: extension.dir, timeoutMs: 200 })
  assert.equal(outcome.ok, false)
  assert.ok(['timeout', 'signal'].includes(outcome.reason), outcome.reason)
})

test('a hook that traps SIGTERM is killed at its timeout', (t) => {
  const extension = hookDir(t)
  const trapped = path.join(extension.dir, 'trapped-sigterm')
  fs.writeFileSync(
    path.join(extension.dir, 'publish.mjs'),
    `import fs from 'node:fs'
process.on('SIGTERM', () => fs.writeFileSync(${JSON.stringify(trapped)}, ''))
setTimeout(() => {}, 2_000)
`,
  )
  const outcome = runProofPublish({ extension, envelope, cwd: extension.dir, timeoutMs: 1_000 })
  assert.deepEqual(outcome, { ok: false, reason: 'timeout' })
  assert.equal(fs.existsSync(trapped), false, 'the hook saw a catchable signal')
})
